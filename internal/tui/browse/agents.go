package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// The agent explorer (the `a` key) is a four-pane view over everything the
// agents on this machine have done: which executors ran (the sidebar), the
// prompts they were given, the commands each prompt triggered, and the details
// of whatever is under the cursor. Selecting an executor filters the other three.

// agentPane identifies one of the explorer's panes. The values are also the
// index into layout.p, so a focused pane maps straight to its rectangle.
type agentPane int

const (
	apAgents agentPane = iota
	apPrompts
	apCommands
	apInfo
)

// agentPaneCount is the explorer's pane count (used to cycle focus).
const agentPaneCount = 4

// agentStat is one executor's aggregate activity over the selected period.
type agentStat struct {
	name      string
	count     int
	success   int // rows with a known exit == 0
	failures  int // rows with a known exit != 0
	knownExit int
	durSum    float64
	durN      int
	lastMs    int64
}

func (a agentStat) successPS() float64 {
	if a.knownExit == 0 {
		return -1
	}
	return 100 * float64(a.success) / float64(a.knownExit)
}

func (a agentStat) avgDurMs() float64 {
	if a.durN == 0 {
		return -1
	}
	return a.durSum / float64(a.durN)
}

// agentsData is the sidebar's aggregation: one row per executor tag, plus totals.
type agentsData struct {
	agents      []agentStat
	total       int // total agent commands in the period
	periodLabel string
}

// computeAgents groups tagged (agent) commands by executor over the last
// periodDays days (0 = all). Untagged commands the user typed are excluded —
// this view is specifically "what agents ran".
func computeAgents(rows []rec.Record, now int64, periodDays int) *agentsData {
	cutoff := periodCutoff(now, periodDays)
	byTag := map[string]*agentStat{}
	total := 0
	for _, r := range rows {
		if r.Deleted() || r.Tag == "" || r.StartMs < cutoff {
			continue
		}
		total++
		a := byTag[r.Tag]
		if a == nil {
			a = &agentStat{name: r.Tag}
			byTag[r.Tag] = a
		}
		a.count++
		if r.StartMs > a.lastMs {
			a.lastMs = r.StartMs
		}
		if r.Exit != nil {
			a.knownExit++
			if *r.Exit == 0 {
				a.success++
			} else {
				a.failures++
			}
		}
		if r.DurMs != nil {
			a.durSum += float64(*r.DurMs)
			a.durN++
		}
	}

	out := &agentsData{total: total, periodLabel: periodLabel(periodDays)}
	for _, a := range byTag {
		out.agents = append(out.agents, *a)
	}
	sort.Slice(out.agents, func(i, j int) bool {
		if out.agents[i].count != out.agents[j].count {
			return out.agents[i].count > out.agents[j].count
		}
		return out.agents[i].name < out.agents[j].name
	})
	return out
}

// --- selection helpers ---------------------------------------------------

// agentRows is the sidebar's row count: every executor plus the leading
// "All agents" row.
func (m Model) agentRows() int {
	if m.agents == nil {
		return 1
	}
	return len(m.agents.agents) + 1
}

// agentAt returns the executor on sidebar row i, or false for the "All agents"
// row (and for any row past the end).
func (m Model) agentAt(i int) (agentStat, bool) {
	if m.agents == nil || i <= 0 || i > len(m.agents.agents) {
		return agentStat{}, false
	}
	return m.agents.agents[i-1], true
}

// --- title ---------------------------------------------------------------

// agentsTitle is the header shown in place of the search bar: the period tabs
// and what the current filter is showing.
func (m Model) agentsTitle(w int) string {
	th := m.th
	if m.afiltering {
		// While the box has focus it takes the header line, exactly as the browse
		// view's search does — the header is where this UI puts a query.
		return m.titleWithTabs(
			th.Dim.Render("filter "+filterNoun(m.afilterPane)+" ")+
				th.Prompt.Render("❯ ")+m.afilter.View(), w)
	}
	var b strings.Builder
	b.WriteString(th.Title.Render("AGENTS"))
	scope := "all agents"
	if m.agentFilter != "" {
		scope = m.agentFilter
	}
	b.WriteString(th.Accent.Render("  " + scope))
	if m.prompts != nil && m.agents != nil {
		b.WriteString(th.Dim.Render(" · " +
			plural(m.prompts.total, "prompt") + " · " + plural(m.agents.total, "command")))
	}
	if m.stats != nil {
		// The explorer reads the same capped sample as the stats screen, so it
		// owes the same caveat: without it, two windows wider than the sample's
		// reach show identical numbers and the tabs look broken.
		b.WriteString(th.Dim.Render(" · " + m.sampleNote(m.stats)))
	}
	return m.titleWithTabs(b.String(), w)
}

// --- the four-pane grid --------------------------------------------------

// renderAgentsView draws the explorer. Zoomed, the focused pane alone fills the
// frame; otherwise the four panes tile the geometry applyLayout resolved, with
// the focused one carrying the accent border.
func (m Model) renderAgentsView(w, h int) string {
	if m.zoom {
		return m.agentPaneBox(m.apane, true, w-2, h-2)
	}
	g := m.geo
	left := lipgloss.JoinVertical(lipgloss.Left,
		m.agentPaneBox(apAgents, m.apane == apAgents, g.p[apAgents].w-2, g.p[apAgents].h-2),
		m.agentPaneBox(apInfo, m.apane == apInfo, g.p[apInfo].w-2, g.p[apInfo].h-2),
	)
	right := lipgloss.JoinVertical(lipgloss.Left,
		m.agentPaneBox(apPrompts, m.apane == apPrompts, g.p[apPrompts].w-2, g.p[apPrompts].h-2),
		m.agentPaneBox(apCommands, m.apane == apCommands, g.p[apCommands].w-2, g.p[apCommands].h-2),
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// agentPaneBox renders one pane's title line plus its body inside a border.
func (m Model) agentPaneBox(p agentPane, focused bool, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	name, suffix := m.agentPaneHeading(p)
	var inner string
	switch p {
	case apAgents:
		inner = m.agentListInner(w, h)
	case apPrompts:
		inner = m.promptListInner(w, h)
	case apCommands:
		inner = m.promptCmdInner(w, h)
	case apInfo:
		inner = m.agentInfoInner(w, h)
	}
	return m.titledBox(focused, w, h, name, suffix, inner)
}

// agentPaneHeading is a pane's name and the count/position suffix that goes
// beside it, so each title doubles as a scrollbar-free position readout.
func (m Model) agentPaneHeading(p agentPane) (name, suffix string) {
	switch p {
	case apAgents:
		if m.agents == nil {
			return "AGENTS", ""
		}
		return "AGENTS", strconv.Itoa(len(m.agents.agents))
	case apPrompts:
		return "PROMPTS", countSuffix(m.promptSel, m.promptLen(), m.promptQ)
	case apCommands:
		return "COMMANDS", countSuffix(m.drillSel, m.drillLen(), m.cmdQ)
	default:
		return "DETAILS", ""
	}
}

// filterNoun names the list the / key is aiming at, for the prompt on the filter
// box: "filter prompts ❯" says what is about to be narrowed, which a bare ❯
// cannot when two panes can both be filtered.
func filterNoun(p agentPane) string {
	if p == apCommands {
		return "commands"
	}
	return "prompts"
}

// countSuffix is a list pane's "cursor/total" readout, with the active filter
// appended so the total is understood as a filtered one. Without the marker a
// narrowed list is indistinguishable from a quiet period, and the /query is the
// only thing on screen that explains the missing rows.
func countSuffix(sel, n int, query string) string {
	out := "0"
	if n > 0 {
		out = fmt.Sprintf("%d/%d", clampIndex(sel, n)+1, n)
	}
	if query != "" {
		out += "  /" + query
	}
	return out
}

// --- agent sidebar -------------------------------------------------------

// agentListInner draws the executor sidebar: an "All agents" row followed by one
// row per executor with its command count. The active filter carries a ● bullet
// (inactive rows keep the column, so the names stay aligned), and the selected
// executor's summary fills the space below the list.
func (m Model) agentListInner(w, h int) string {
	th := m.th
	if m.agents == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}

	rows := m.agentRows()
	sel := clampIndex(m.agentSel, rows)
	// Reserve the summary block only when there is room for the list as well.
	summary := m.agentSummary(sel, w)
	listH := h
	if len(summary) > 0 && h-len(summary) >= 2 {
		listH = h - len(summary)
	} else {
		summary = nil
	}

	lines := make([]string, 0, h)
	top := windowStart(sel, listH, rows)
	for i := top; i < rows && i < top+listH; i++ {
		lines = append(lines, m.agentLine(i, i == sel, w))
	}
	for len(lines) < listH {
		lines = append(lines, strings.Repeat(" ", w))
	}
	lines = append(lines, summary...)
	return padLines(lines, w, h)
}

// agentLine formats one sidebar row: "● name    count".
func (m Model) agentLine(i int, sel bool, w int) string {
	th := m.th
	name, count, active := "all agents", 0, m.agentFilter == ""
	if m.agents != nil {
		count = m.agents.total
	}
	if a, ok := m.agentAt(i); ok {
		name, count, active = a.name, a.count, a.name == m.agentFilter
	}
	bullet := " "
	if active {
		bullet = "●"
	}
	num := strconv.Itoa(count)
	nameW := w - lipgloss.Width(num) - 3 // bullet + space + gap
	if nameW < 1 {
		nameW = 1
	}
	label := bullet + " " + padRight(truncCols(name, nameW), nameW)
	segs := []styledSeg{
		{text: label, style: th.Accent},
		{text: " ", raw: true},
		{text: num, style: th.Dim},
	}
	if !active {
		segs[0].style = th.Norm
	}
	return composeSegs(segs, sel, w, th)
}

// agentSummary is the compact stat block under the sidebar list for the executor
// on row i — success rate, failures, average duration, last seen. The "All
// agents" row summarizes every executor together.
func (m Model) agentSummary(i, w int) []string {
	th := m.th
	if m.agents == nil || w < 12 {
		return nil
	}
	a, ok := m.agentAt(i)
	if !ok {
		a = agentStat{name: "all agents"}
		for _, x := range m.agents.agents {
			a.count += x.count
			a.success += x.success
			a.failures += x.failures
			a.knownExit += x.knownExit
			a.durSum += x.durSum
			a.durN += x.durN
			if x.lastMs > a.lastMs {
				a.lastMs = x.lastMs
			}
		}
	}
	if a.count == 0 {
		return nil
	}
	ok2 := "n/a"
	if a.successPS() >= 0 {
		ok2 = fmt.Sprintf("%.0f%% ok", a.successPS())
	}
	avg := "no timing"
	if a.avgDurMs() >= 0 {
		avg = "avg " + theme.Duration(int64(a.avgDurMs()))
	}
	fails := "no failures"
	if a.failures > 0 {
		fails = fmt.Sprintf("%d failed", a.failures)
	}
	return []string{
		strings.Repeat(" ", w),
		th.Section.Render(fitPlain(truncCols(a.name, w), w)),
		th.Dim.Render(fitPlain(fmt.Sprintf("%d cmds · %s", a.count, ok2), w)),
		th.Dim.Render(fitPlain(fails+" · "+avg, w)),
		th.Dim.Render(fitPlain("last "+theme.RelTime(m.now(), a.lastMs), w)),
	}
}

// --- details pane --------------------------------------------------------

// infoLabelW is the details pane's label column, wide enough for the longest
// label so every value starts in the same column.
const infoLabelW = 9

// agentInfoInner draws the details pane. It follows focus: with the command pane
// active it describes the selected command, otherwise the selected prompt — so
// the pane always explains whatever the cursor is on.
func (m Model) agentInfoInner(w, h int) string {
	th := m.th
	p, ok := m.drilledPrompt()
	if !ok {
		return padLines([]string{"", "  " + th.Dim.Render(m.noPromptMsg())}, w, h)
	}
	if m.apane == apCommands {
		if r, has := m.drilledCmd(); has {
			return padLines(m.cmdInfoLines(r, w), w, h)
		}
	}
	return padLines(m.promptInfoLines(p, w), w, h)
}

// promptInfoLines is the details body for a prompt: its full text wrapped, then
// the aggregate of the commands it triggered.
func (m Model) promptInfoLines(p promptStat, w int) []string {
	th := m.th
	lines := make([]string, 0, 16)
	lines = append(lines, th.Section.Render(fitPlain("Prompt", w)))
	for _, l := range wrapPlain(p.text, w-1) {
		lines = append(lines, " "+th.Norm.Render(l))
	}
	lines = append(lines, strings.Repeat(" ", w))

	now := m.now()
	status := fmt.Sprintf("%d   ✓%d ✗%d", p.count, p.success, p.failures)
	// Ordered by what a short pane should keep: on a squeezed layout the tail
	// (the timestamps) is what gets clipped, not the identity of the prompt.
	for _, kv := range [][2]string{
		{"Executor", dashIfEmpty(p.executor)},
		{"Session", shortSession(p.session)},
		{"Commands", status},
		{"Duration", promptDur(p)},
		{"Path", modalCwd(p.cmds)},
		{"First", theme.RelTime(now, p.firstMs)},
		{"Last", theme.RelTime(now, p.lastMs)},
	} {
		lines = append(lines, m.infoRow(kv[0], kv[1], w))
	}
	return lines
}

// cmdInfoLines is the details body for one captured command.
func (m Model) cmdInfoLines(r rec.Record, w int) []string {
	th := m.th
	lines := make([]string, 0, 16)
	lines = append(lines, th.Section.Render(fitPlain("Command", w)))
	for _, l := range wrapPlain(oneLine(r.Cmd), w-1) {
		lines = append(lines, " "+th.Norm.Render(l))
	}
	lines = append(lines, strings.Repeat(" ", w))

	dur := "—"
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	for _, kv := range [][2]string{
		{"Path", dashIfEmpty(r.Cwd)},
		{"Host", dashIfEmpty(r.Hostname)},
		{"Session", shortSession(r.Session)},
		{"Executor", dashIfEmpty(r.Tag)},
		{"Time", time.UnixMilli(r.StartMs).Local().Format("2006-01-02 15:04:05")},
		{"Duration", dur},
		{"Exit", exitWord(r)},
	} {
		lines = append(lines, m.infoRow(kv[0], kv[1], w))
	}
	return lines
}

// infoRow formats one "Label   value" line of the details pane.
func (m Model) infoRow(label, value string, w int) string {
	vw := w - infoLabelW
	if vw < 1 {
		vw = 1
	}
	return m.th.Dim.Render(padRight(label, infoLabelW)) +
		m.th.Norm.Render(padRight(truncCols(value, vw), vw))
}

// plural renders "1 prompt" / "3 prompts" for the title's counters.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// wrapPlain word-wraps an unstyled single-line string to width w, capping the
// result so a huge prompt cannot push the metadata off the details pane.
func wrapPlain(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	s = oneLine(s)
	if s == "" {
		return []string{"—"}
	}
	var out []string
	for s != "" && len(out) < 8 {
		if lipgloss.Width(s) <= w {
			out = append(out, s)
			break
		}
		cut := truncCols(s, w)
		// Prefer breaking at the last space so words stay intact.
		if i := strings.LastIndexByte(cut, ' '); i > w/2 {
			cut = cut[:i]
		}
		out = append(out, cut)
		s = strings.TrimLeft(s[len(cut):], " ")
	}
	if s != "" && lipgloss.Width(s) > w {
		out[len(out)-1] = truncCols(out[len(out)-1], w-1) + "…"
	}
	return out
}

func periodSuffix(label string) string {
	if label == "All" {
		return ""
	}
	return " in the last " + strings.ToLower(label)
}
