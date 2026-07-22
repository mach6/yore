package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"yore/internal/rec"
	"yore/internal/risk"
)

// --- record formatting -------------------------------------------------------

// formatCommandList renders rows as a titled list: time, exit glyph, command,
// executor, directory, and (cross-machine) host.
func formatCommandList(title string, rows []rec.Record, localHost string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", title)
	if len(rows) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s  %s", relTime(r.StartMs), exitGlyph(r), oneLine(r.Cmd, 120))
		var tail []string
		if r.Tag != "" {
			tail = append(tail, r.Tag)
		}
		if r.Cwd != "" {
			tail = append(tail, r.Cwd)
		}
		if r.Hostname != "" && r.Hostname != localHost {
			tail = append(tail, "@"+r.Hostname)
		}
		if len(tail) > 0 {
			fmt.Fprintf(&b, "  [%s]", strings.Join(tail, " · "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// formatChronological renders rows oldest-first with interleaved prompts.
func formatChronological(title string, rows []rec.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d command(s)\n\n", title, len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s  %s", relTime(r.StartMs), exitGlyph(r), oneLine(r.Cmd, 120))
		if r.Cwd != "" {
			fmt.Fprintf(&b, "  (%s)", r.Cwd)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// exitGlyph renders a record's outcome: ✓ ok, ✗N failed, · unknown.
func exitGlyph(r rec.Record) string {
	if r.Exit == nil {
		return "·"
	}
	if *r.Exit == 0 {
		return "✓"
	}
	return fmt.Sprintf("✗%d", *r.Exit)
}

// tallyExits counts ok/fail/unknown outcomes across rows.
func tallyExits(rows []rec.Record) (ok, fail, unknown int) {
	for _, r := range rows {
		switch {
		case r.Exit == nil:
			unknown++
		case *r.Exit == 0:
			ok++
		default:
			fail++
		}
	}
	return
}

// oneLine flattens newlines and clamps to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func shortID(id string) string {
	// Strip common agent prefixes, then clamp.
	for _, p := range []string{"claude-", "cursor-", "opencode-", "codex-"} {
		id = strings.TrimPrefix(id, p)
	}
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "—"
	}
	return id
}

func execLabel(tag string) string {
	if tag == "" {
		return "(you)"
	}
	return tag
}

// relTime renders a millisecond timestamp as a compact age ("5m", "3h", "2d").
func relTime(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return time.UnixMilli(ms).Format("Jan 2")
	}
}

// --- session aggregation -----------------------------------------------------

type sessionAgg struct {
	id          string
	executor    string
	host        string
	count       int
	ok, fail    int
	firstMs     int64
	lastMs      int64
	firstPrompt string
	dirs        map[string]bool
}

// groupSessions aggregates rows into per-session summaries, newest-active first.
// executor != "" restricts to that executor.
func groupSessions(rows []rec.Record, executor string) []*sessionAgg {
	by := map[string]*sessionAgg{}
	order := []string{}
	for i := range rows {
		r := rows[i]
		if r.Session == "" || r.Deleted() {
			continue
		}
		if executor != "" && r.Tag != executor {
			continue
		}
		a := by[r.Session]
		if a == nil {
			a = &sessionAgg{id: r.Session, executor: r.Tag, host: r.Hostname, firstMs: r.StartMs, lastMs: r.StartMs, dirs: map[string]bool{}}
			by[r.Session] = a
			order = append(order, r.Session)
		}
		a.count++
		if r.StartMs < a.firstMs {
			a.firstMs = r.StartMs
		}
		if r.StartMs > a.lastMs {
			a.lastMs = r.StartMs
		}
		if r.Cwd != "" {
			a.dirs[r.Cwd] = true
		}
		if r.Prompt != "" && a.firstPrompt == "" {
			a.firstPrompt = r.Prompt
		}
		switch {
		case r.Exit == nil:
		case *r.Exit == 0:
			a.ok++
		default:
			a.fail++
		}
	}
	out := make([]*sessionAgg, 0, len(order))
	for _, id := range order {
		out = append(out, by[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastMs > out[j].lastMs })
	return out
}

// --- prompt aggregation ------------------------------------------------------

type promptAgg struct {
	id       string
	text     string
	executor string
	session  string
	count    int
	ok, fail int
	firstMs  int64
	lastMs   int64
	cmds     []rec.Record
}

// groupPrompts aggregates agent commands by PromptID, newest-active first. Rows
// with no PromptID are skipped. executor/session/dir, when non-empty, filter.
func groupPrompts(rows []rec.Record, executor, session, dir string) []*promptAgg {
	by := map[string]*promptAgg{}
	order := []string{}
	for i := range rows {
		r := rows[i]
		if r.PromptID == "" || r.Deleted() {
			continue
		}
		if executor != "" && r.Tag != executor {
			continue
		}
		if session != "" && r.Session != session {
			continue
		}
		if dir != "" && r.Cwd != dir {
			continue
		}
		a := by[r.PromptID]
		if a == nil {
			a = &promptAgg{id: r.PromptID, text: r.Prompt, executor: r.Tag, session: r.Session, firstMs: r.StartMs, lastMs: r.StartMs}
			by[r.PromptID] = a
			order = append(order, r.PromptID)
		}
		if a.text == "" {
			a.text = r.Prompt
		}
		a.count++
		a.cmds = append(a.cmds, r)
		if r.StartMs < a.firstMs {
			a.firstMs = r.StartMs
		}
		if r.StartMs > a.lastMs {
			a.lastMs = r.StartMs
		}
		switch {
		case r.Exit == nil:
		case *r.Exit == 0:
			a.ok++
		default:
			a.fail++
		}
	}
	out := make([]*promptAgg, 0, len(order))
	for _, id := range order {
		g := by[id]
		sort.SliceStable(g.cmds, func(i, j int) bool { return g.cmds[i].StartMs < g.cmds[j].StartMs })
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastMs > out[j].lastMs })
	return out
}

// groupFailures collects failed commands (known nonzero exit) within the cutoff
// and optional directory, grouped by their triggering prompt when one exists,
// else by session. Groups are newest-active first; each group's cmds are its
// failures, oldest first. count/fail both track the failure tally.
func groupFailures(rows []rec.Record, dir string, cutoff int64) []*promptAgg {
	by := map[string]*promptAgg{}
	order := []string{}
	for i := range rows {
		r := rows[i]
		if r.Deleted() || r.Exit == nil || *r.Exit == 0 || r.StartMs < cutoff {
			continue
		}
		if dir != "" && r.Cwd != dir && !strings.HasPrefix(r.Cwd, strings.TrimRight(dir, "/")+"/") {
			continue
		}
		key := r.PromptID
		if key == "" {
			key = "session:" + r.Session
		}
		a := by[key]
		if a == nil {
			a = &promptAgg{id: key, text: r.Prompt, session: r.Session, executor: r.Tag, firstMs: r.StartMs, lastMs: r.StartMs}
			by[key] = a
			order = append(order, key)
		}
		if a.text == "" {
			a.text = r.Prompt
		}
		a.count++
		a.fail++
		a.cmds = append(a.cmds, r)
		if r.StartMs > a.lastMs {
			a.lastMs = r.StartMs
		}
	}
	out := make([]*promptAgg, 0, len(order))
	for _, k := range order {
		g := by[k]
		sort.SliceStable(g.cmds, func(i, j int) bool { return g.cmds[i].StartMs < g.cmds[j].StartMs })
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastMs > out[j].lastMs })
	return out
}

// --- stats -------------------------------------------------------------------

// formatStats renders an aggregate briefing over rows within the last `days`,
// optionally restricted to a directory subtree, excluding any excluded dir.
func formatStats(rows []rec.Record, dir string, days int, excluded func(string) bool) string {
	cutoff := cutoffMs(days)
	var total, ok, known int
	programs := map[string]int{}
	dirs := map[string]int{}
	execs := map[string]int{}
	hosts := map[string]int{}
	for i := range rows {
		r := rows[i]
		if r.Deleted() || r.StartMs < cutoff {
			continue
		}
		if dir != "" && r.Cwd != dir && !strings.HasPrefix(r.Cwd, strings.TrimRight(dir, "/")+"/") {
			continue
		}
		if excluded != nil && excluded(r.Cwd) {
			continue
		}
		total++
		if tok := firstToken(r.Cmd); tok != "" {
			programs[tok]++
		}
		if r.Cwd != "" {
			dirs[r.Cwd]++
		}
		execs[execLabel(r.Tag)]++
		if r.Hostname != "" {
			hosts[r.Hostname]++
		}
		if r.Exit != nil {
			known++
			if *r.Exit == 0 {
				ok++
			}
		}
	}
	var b strings.Builder
	window := "all history"
	if days > 0 {
		window = fmt.Sprintf("last %dd", days)
	}
	fmt.Fprintf(&b, "Stats (%s)\n\n", window)
	success := "n/a"
	if known > 0 {
		success = fmt.Sprintf("%.0f%%", 100*float64(ok)/float64(known))
	}
	fmt.Fprintf(&b, "Total %d · Success %s · Hosts %d\n\n", total, success, len(hosts))
	writeTop(&b, "Top programs", programs)
	writeTop(&b, "Top directories", dirs)
	writeTop(&b, "By executor", execs)
	if len(hosts) > 1 {
		writeTop(&b, "By host", hosts)
	}
	return b.String()
}

// topRows is how many entries each ranked list in a stats report shows.
const topRows = 8

func writeTop(b *strings.Builder, title string, m map[string]int) {
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(m))
	for k, v := range m {
		items = append(items, kv{k, v})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	fmt.Fprintf(b, "%s:\n", title)
	for i := 0; i < len(items) && i < topRows; i++ {
		fmt.Fprintf(b, "  %5d  %s\n", items[i].v, items[i].k)
	}
	b.WriteByte('\n')
}

// riskGlyph is a compact severity marker for risk output.
func riskGlyph(l risk.Level) string {
	switch l {
	case risk.Critical:
		return "⛔"
	case risk.High:
		return "⚠"
	case risk.Medium:
		return "▲"
	case risk.Low:
		return "•"
	default:
		return "✓"
	}
}

func firstToken(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
