package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/rec"
	"yore/internal/tui/theme"
)

const sparkDays = 30

// sparkBlocks are the eight ascending block glyphs used by the histograms
// (level 1..8).
var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// statPeriods are the selectable stats windows (keys 1..5). days == 0 means all
// history.
var statPeriods = []struct {
	label string
	days  int
}{
	{"Today", 1},
	{"7d", 7},
	{"30d", 30},
	{"90d", 90},
	{"All", 0},
}

// cmdCount is a name paired with a tally, used for every ranked list.
type cmdCount struct {
	name string
	n    int
}

// statsData is the pre-computed aggregation behind the stats screen. Every
// field respects the selected period (see computeStats).
type statsData struct {
	// KPI header.
	total     int
	unique    int     // distinct programs (first token)
	successPS float64 // success %, over rows with a known exit; <0 = n/a
	knownExit int     // rows contributing to successPS
	avgDurMs  float64 // over rows with a known duration; <0 = n/a
	agentPS   float64 // % of rows run by an agent (tagged)

	topPrograms []cmdCount // by first token
	topCommands []cmdCount // by full command
	topDirs     []cmdCount // by cwd
	byExecutor  []cmdCount // by tag ("" -> "(you)")

	spark    []int // per-day counts, oldest (left) .. newest (right)
	sparkMax int
	hourly   [24]int // counts by hour-of-day (local)
	hourMax  int

	periodLabel string
	days        int // sparkline window
}

// computeStats aggregates a broad row sample into the stats screen's data,
// restricted to the last periodDays days (0 = all history). It reads only
// fields every record already carries — no schema dependency.
func computeStats(rows []rec.Record, now int64, periodDays int) *statsData {
	s := &statsData{days: sparkDays, spark: make([]int, sparkDays)}
	s.periodLabel = periodLabel(periodDays)

	cutoff := int64(0)
	if periodDays > 0 {
		cutoff = now - int64(periodDays)*86_400_000
	}

	programs := map[string]int{}
	commands := map[string]int{}
	dirs := map[string]int{}
	execs := map[string]int{}
	var durSum float64
	var durN int
	var success int

	for _, r := range rows {
		if r.Deleted() || r.StartMs < cutoff {
			continue
		}
		s.total++
		if tok := firstToken(r.Cmd); tok != "" {
			programs[tok]++
		}
		if c := strings.TrimSpace(r.Cmd); c != "" {
			commands[c]++
		}
		if r.Cwd != "" {
			dirs[r.Cwd]++
		}
		execs[executorLabel(r.Tag)]++
		if r.Tag != "" {
			s.agentPS++
		}
		if r.Exit != nil {
			s.knownExit++
			if *r.Exit == 0 {
				success++
			}
		}
		if r.DurMs != nil {
			durSum += float64(*r.DurMs)
			durN++
		}
		if day := int((now - r.StartMs) / 86_400_000); day >= 0 && day < sparkDays {
			s.spark[sparkDays-1-day]++
		}
		s.hourly[hourOfDay(r.StartMs)]++
	}

	s.unique = len(programs)
	if s.knownExit > 0 {
		s.successPS = 100 * float64(success) / float64(s.knownExit)
	} else {
		s.successPS = -1
	}
	if durN > 0 {
		s.avgDurMs = durSum / float64(durN)
	} else {
		s.avgDurMs = -1
	}
	if s.total > 0 {
		s.agentPS = 100 * s.agentPS / float64(s.total)
	}

	s.topPrograms = topN(programs)
	s.topCommands = topN(commands)
	s.topDirs = topN(dirs)
	s.byExecutor = topN(execs)

	for _, c := range s.spark {
		if c > s.sparkMax {
			s.sparkMax = c
		}
	}
	for _, c := range s.hourly {
		if c > s.hourMax {
			s.hourMax = c
		}
	}
	return s
}

// topN returns the highest-count entries from a tally map, most first, ties
// broken by name for stable output.
func topN(m map[string]int) []cmdCount {
	const n = 12
	out := make([]cmdCount, 0, len(m))
	for k, v := range m {
		out = append(out, cmdCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func firstToken(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// executorLabel maps a record's tag to a display label; the empty tag (a
// command the user typed) reads as "(you)".
func executorLabel(tag string) string {
	if tag == "" {
		return "(you)"
	}
	return tag
}

func hourOfDay(ms int64) int { return time.UnixMilli(ms).Hour() }

func periodLabel(days int) string {
	for _, p := range statPeriods {
		if p.days == days {
			return p.label
		}
	}
	return "All"
}

// statsTitle is the top line shown in place of the search bar in stats mode: a
// title plus the period tabs (keys 1..5).
func (m Model) statsTitle(w int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.Title.Render("STATS"))
	b.WriteString("  ")
	for i, p := range statPeriods {
		key := strconv.Itoa(i + 1)
		if i == m.statsPeriod {
			b.WriteString(th.Accent.Bold(true).Render(key+" "+p.label) + " ")
		} else {
			b.WriteString(th.Dim.Render(key+" "+p.label) + " ")
		}
	}
	b.WriteString(th.Dim.Render(" · s to return"))
	return clipW(b.String(), w)
}

// renderStats draws the full stats screen in exactly h lines, no wider than w:
// a KPI header, up to four ranked columns, then the daily + hourly histograms.
func (m Model) renderStats(w, h int) string {
	th := m.th
	if m.stats == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}
	s := m.stats

	out := make([]string, 0, h)
	out = append(out, m.kpiHeader(s, w), "")

	// Histograms occupy the bottom; columns fill what remains above them.
	hist := m.histograms(s, w)
	bodyH := h - len(out) - len(hist)
	if bodyH < 3 {
		bodyH = 3
	}

	out = append(out, m.statColumns(s, w, bodyH)...)
	out = append(out, hist...)
	return padLines(out, w, h)
}

// kpiHeader is the single summary line: totals, success rate, avg duration,
// agent share.
func (m Model) kpiHeader(s *statsData, w int) string {
	th := m.th
	success := "n/a"
	if s.successPS >= 0 {
		success = fmt.Sprintf("%.0f%%", s.successPS)
	}
	dur := "n/a"
	if s.avgDurMs >= 0 {
		dur = theme.Duration(int64(s.avgDurMs))
	}
	seg := func(label, val string) string {
		return th.Dim.Render(label+" ") + th.Norm.Render(val)
	}
	parts := []string{
		seg("Total", strconv.Itoa(s.total)),
		seg("Unique", strconv.Itoa(s.unique)),
		seg("Success", success),
		seg("Avg", dur),
		seg("Agent", fmt.Sprintf("%.0f%%", s.agentPS)),
	}
	return clipW("  "+strings.Join(parts, th.Dim.Render("  ·  ")), w)
}

// statColumns lays out the ranked lists side by side, dropping columns (from the
// right) until they fit the width. Programs and commands are the priority.
func (m Model) statColumns(s *statsData, w, bodyH int) []string {
	type col struct {
		title string
		items []cmdCount
	}
	all := []col{
		{"Top programs", s.topPrograms},
		{"Top commands", s.topCommands},
		{"Top directories", s.topDirs},
		{"By executor", s.byExecutor},
	}

	const gap, minCol = 2, 16
	n := len(all)
	for n > 1 && (w-(n-1)*gap)/n < minCol {
		n--
	}
	colW := (w - (n-1)*gap) / n
	if colW < 8 {
		colW = 8
	}
	rows := bodyH - 1
	if rows < 1 {
		rows = 1
	}

	rendered := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines := statColumn(m.th, all[i].title, all[i].items, colW, rows)
		rendered = append(rendered, padLines(lines, colW, bodyH))
	}
	joined := make([]string, 0, 2*n-1)
	for i, r := range rendered {
		if i > 0 {
			joined = append(joined, strings.Repeat(" ", gap))
		}
		joined = append(joined, r)
	}
	return strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, joined...), "\n")
}

// statColumn renders a titled list column of name/count pairs, each line padded
// to exactly colW columns.
func statColumn(th *theme.Theme, title string, items []cmdCount, colW, rows int) []string {
	lines := []string{th.Title.Render(fitPlain(title, colW))}
	if len(items) == 0 {
		lines = append(lines, th.Dim.Render(fitPlain("  (none)", colW)))
		return lines
	}
	maxN := 0
	for _, it := range items {
		if it.n > maxN {
			maxN = it.n
		}
	}
	for i := 0; i < len(items) && i < rows; i++ {
		lines = append(lines, statLine(th, items[i], maxN, colW))
	}
	return lines
}

// statLine is "name  ▬▬▬▬   count", padded to exactly colW columns. The bar is a
// proportional gauge; it is dropped when the column is too narrow.
func statLine(th *theme.Theme, it cmdCount, maxN, colW int) string {
	count := strconv.Itoa(it.n)
	cntW := runewidth.StringWidth(count)

	barW := colW / 4
	if colW < 24 {
		barW = 0
	}

	nameW := colW - cntW - 1 - barW
	if barW > 0 {
		nameW-- // gap before the bar
	}
	if nameW < 1 {
		nameW = 1
	}
	name := truncCols(it.name, nameW)

	var b strings.Builder
	b.WriteString(th.Norm.Render(name))
	used := runewidth.StringWidth(name)

	if barW > 0 {
		filled := 0
		if maxN > 0 {
			filled = it.n * barW / maxN
		}
		bar := strings.Repeat("▬", filled) + strings.Repeat(" ", barW-filled)
		b.WriteString(" " + th.Accent.Render(bar))
		used += 1 + barW
	}

	gap := colW - used - cntW
	if gap < 1 {
		gap = 1
	}
	b.WriteString(strings.Repeat(" ", gap) + th.Dim.Render(count))
	return b.String()
}

// histograms renders the per-day sparkline and the hourly distribution.
func (m Model) histograms(s *statsData, w int) []string {
	th := m.th
	title := th.Title.Render(fitPlain("Commands per day (last 30d)", w))
	summary := th.Dim.Render(fitPlain(
		fmt.Sprintf("%s · %d commands · peak %d/day", s.periodLabel, s.total, s.sparkMax), w))
	hourTitle := th.Title.Render(fitPlain("By hour of day (0–23)", w))
	return []string{
		"",
		title,
		renderBars(th, s.spark, s.sparkMax, w),
		summary,
		"",
		hourTitle,
		renderBars(th, s.hourly[:], s.hourMax, w),
	}
}

// renderBars draws a block-glyph histogram of vals, one glyph per value, capped
// at w glyphs. A zero value is a dim dot; others scale into the 8 block levels.
func renderBars(th *theme.Theme, vals []int, maxN, w int) string {
	if maxN <= 0 {
		return th.Dim.Render(strings.Repeat("·", minInt(len(vals), w)))
	}
	var b strings.Builder
	for i, cnt := range vals {
		if i >= w {
			break
		}
		if cnt <= 0 {
			b.WriteString(th.Dim.Render("·"))
			continue
		}
		lvl := (cnt*8 + maxN - 1) / maxN // ceil into 1..8
		if lvl < 1 {
			lvl = 1
		}
		if lvl > 8 {
			lvl = 8
		}
		b.WriteString(th.Accent.Render(string(sparkBlocks[lvl-1])))
	}
	return b.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
