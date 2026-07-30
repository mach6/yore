package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"yore/internal/match"
	"yore/internal/rec"
	"yore/internal/risk"
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
	apHosts
	apPrompts
	apCommands
	apInfo
)

// agentPaneCount is the explorer's pane count (used to cycle focus).
const agentPaneCount = 5

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
		if r.Deleted() || r.Executor == "" || r.StartMs < cutoff {
			continue
		}
		total++
		a := byTag[r.Executor]
		if a == nil {
			a = &agentStat{name: r.Executor}
			byTag[r.Executor] = a
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

// agentHostsIn is the explorer's host cycle: every hostname with agent activity
// in the period — a tagged command or a prompt record — with its agent-command
// count, in name order, because the ring must not reshuffle under repeated
// presses of the key that walks it. The counts ignore the explorer's filters:
// the block is the map of where H can go, not a view of where it is.
func agentHostsIn(rows, prompts []rec.Record, now int64, periodDays int) []cmdCount {
	cutoff := periodCutoff(now, periodDays)
	seen := map[string]int{}
	for _, r := range rows {
		if !r.Deleted() && r.Executor != "" && r.StartMs >= cutoff && r.Hostname != "" {
			seen[r.Hostname]++
		}
	}
	for _, r := range prompts {
		if !r.Deleted() && r.ID != "" && r.StartMs >= cutoff && r.Hostname != "" {
			seen[r.Hostname] += 0 // a prompt that ran nothing still puts its host on the map
		}
	}
	out := make([]cmdCount, 0, len(seen))
	for h, n := range seen {
		out = append(out, cmdCount{name: h, n: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// hostOnly narrows records to those captured on one hostname.
func hostOnly(rows []rec.Record, host string) []rec.Record {
	out := make([]rec.Record, 0, len(rows))
	for _, r := range rows {
		if r.Hostname == host {
			out = append(out, r)
		}
	}
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

// hostRows is the host pane's row count: every host plus the leading
// "all hosts" row.
func (m Model) hostRows() int {
	return len(m.agentHosts) + 1
}

// hostAt returns the host on row i, or false for the "all hosts" row (and for
// any row past the end).
func (m Model) hostAt(i int) (cmdCount, bool) {
	if i <= 0 || i > len(m.agentHosts) {
		return cmdCount{}, false
	}
	return m.agentHosts[i-1], true
}

// showPromptHost reports whether the prompt table should spend a column on the
// host. It earns one only when the rows can actually differ: several machines in
// the sample and no host filter up. Filtered to one, every cell would repeat a
// name the pane title, the HOSTS pane's bullet, and the header line all already
// carry, and those ten columns are worth more to the prompt text.
//
// So the table does reshape as H walks the ring, which it deliberately did not
// before. The reshape is the point: the column is there to tell hosts apart, and
// once one is chosen there are none to tell apart.
func (m Model) showPromptHost() bool {
	return m.agentHostFilter == "" && len(m.agentHosts) > 1
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
	if m.agentHostFilter != "" {
		b.WriteString(th.Dim.Render(" on "))
		b.WriteString(th.Host(m.agentHostFilter).Render(m.agentHostFilter))
	}
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

// --- the five-pane grid --------------------------------------------------

// renderAgentsView draws the explorer. Zoomed, the focused pane alone fills the
// frame; otherwise the five panes tile the geometry applyLayout resolved, with
// the focused one carrying the accent border.
func (m Model) renderAgentsView(w, h int) string {
	if m.zoom {
		return m.agentPaneBox(m.apane, true, w-2, h-2)
	}
	g := m.geo
	left := lipgloss.JoinVertical(lipgloss.Left,
		m.agentPaneBox(apAgents, m.apane == apAgents, g.p[apAgents].w-2, g.p[apAgents].h-2),
		m.agentPaneBox(apHosts, m.apane == apHosts, g.p[apHosts].w-2, g.p[apHosts].h-2),
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
	case apHosts:
		inner = m.agentHostsInner(w, h)
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
	case apHosts:
		return "HOSTS", strconv.Itoa(len(m.agentHosts))
	case apPrompts:
		return "PROMPTS", m.listSuffix(ctPrompts, countSuffix(m.promptSel, m.promptLen(), m.promptQ))
	case apCommands:
		return "COMMANDS", m.listSuffix(ctCommands, countSuffix(m.drillSel, m.drillLen(), m.cmdQ))
	default:
		// The title says what the pane is describing, since the pane can outlive
		// the focus that chose it, and "↓ more" when its body runs past the
		// bottom — the pane has no scrollbar, and a record silently cut off at
		// the last visible row reads as a record that ends there.
		suffix := "prompt"
		if m.infoCmd {
			suffix = "command"
		}
		if m.infoTop > 0 {
			suffix += "  ↑"
		}
		if m.infoTop < m.infoMaxTop() {
			suffix += "  ↓ more"
		}
		return "DETAILS", suffix
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

// agentHostsInner draws the host pane: an "all hosts" row followed by one row
// per host with its agent-command count. It is the ring the H key walks, made
// visible and walkable by cursor — picking a host filters the other panes the
// way picking an executor does. The counts ignore the explorer's filters: the
// pane is the map of where the filter can go, not a view of where it is.
func (m Model) agentHostsInner(w, h int) string {
	if m.agents == nil {
		return padLines([]string{"", "  " + m.th.Dim.Render("computing…")}, w, h)
	}
	rows := m.hostRows()
	sel := clampIndex(m.agentHostSel, rows)
	lines := make([]string, 0, h)
	top := windowStart(sel, h, rows)
	for i := top; i < rows && i < top+h; i++ {
		lines = append(lines, m.agentHostLine(i, i == sel, w))
	}
	return padLines(lines, w, h)
}

// agentHostLine formats one host row in agentLine's "● name    count" shape, so
// the two left-column lists read the same way — except the hostname keeps its
// identity color, the one it carries everywhere else in the UI.
func (m Model) agentHostLine(i int, sel bool, w int) string {
	th := m.th
	name, count, active := "all hosts", 0, m.agentHostFilter == ""
	style := th.Norm
	if active {
		style = th.Accent
	}
	for _, hc := range m.agentHosts {
		count += hc.n
	}
	if hc, ok := m.hostAt(i); ok {
		name, count, active = hc.name, hc.n, hc.name == m.agentHostFilter
		style = th.Host(hc.name)
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
	segs := []styledSeg{
		{text: bullet + " " + padRight(truncCols(name, nameW), nameW), style: style},
		{text: " ", raw: true},
		{text: num, style: th.Dim},
	}
	return composeSegs(segs, sel, w, th)
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

// infoBodyCap bounds the details pane's wrapped command or prompt text while the
// pane is only being glanced at, so a long one cannot push the metadata rows out
// of a short pane. Focused, the pane is being read rather than glanced at: the
// cap lifts and scrolling reaches the rest.
const infoBodyCap = 8

// infoCap is the wrapped-body cap for the pane's current state — 0 (uncapped)
// once it has focus, where an ellipsis would hide the very text the user tabbed
// over to read.
func (m Model) infoCap() int {
	if m.apane == apInfo {
		return 0
	}
	return infoBodyCap
}

// agentInfoLines is the details pane's body: the selected command's record when
// that is what the pane is describing, otherwise the selected prompt's.
//
// The subject is m.infoCmd, set when focus lands on a list pane and left alone
// when focus lands on DETAILS itself — so tabbing onto the pane to read or zoom
// it does not change what it is showing. Reading the focused pane instead (as
// this did) made the one route to a command's full record — focus it, then z —
// swap in the prompt's on the way.
func (m Model) agentInfoLines(w int) []string {
	p, ok := m.drilledPrompt()
	if !ok {
		return []string{"", "  " + m.th.Dim.Render(m.noPromptMsg())}
	}
	if m.infoCmd {
		if r, has := m.drilledCmd(); has {
			return m.cmdInfoLines(r, w, m.infoCap())
		}
	}
	return m.promptInfoLines(p, w, m.infoCap())
}

// agentInfoInner draws the details pane, windowed at its scroll offset.
func (m Model) agentInfoInner(w, h int) string {
	lines := m.agentInfoLines(w)
	if top := clampIndex(m.infoTop, len(lines)); top > 0 {
		lines = lines[top:]
	}
	return padLines(lines, w, h)
}

// infoBody is the details pane's body and how many of its lines fit, both at the
// pane's current geometry. The scroll clamp goes through this so it cannot
// disagree with the renderer about how far there is left to scroll.
func (m Model) infoBody() (lines []string, visible int) {
	r := m.geo.p[apInfo]
	return m.agentInfoLines(maxInt(1, r.w-2)), maxInt(1, r.h-2)
}

// infoMaxTop is the furthest the details pane scrolls: far enough to bring its
// last line into view, and no further.
func (m Model) infoMaxTop() int {
	lines, visible := m.infoBody()
	return maxInt(0, len(lines)-visible)
}

// promptInfoLines is the details body for a prompt: its full text wrapped, then
// the aggregate of the commands it triggered.
func (m Model) promptInfoLines(p promptStat, w, bodyCap int) []string {
	th := m.th
	lines := make([]string, 0, 16)
	lines = append(lines, th.Section.Render(fitPlain("Prompt", w)))
	for _, l := range wrapPlain(p.text, w-1, bodyCap) {
		lines = append(lines, " "+th.Norm.Render(l))
	}
	lines = append(lines, strings.Repeat(" ", w))

	now := m.now()
	vw := maxInt(1, w-infoLabelW)
	// Ordered by what a short pane should keep: on a squeezed layout the tail
	// (the timestamps) is what gets clipped, not the identity of the prompt.
	lines = append(lines,
		m.infoRowSegs("Executor", hueSeg(th, p.executor), w),
		m.infoRow("Session", shortSession(p.session), w),
		m.infoRowSegs("Host", hueSeg(th, p.host), w),
		m.infoRowSegs("Commands", commandsSegs(th, p), w),
		m.infoRow("Duration", promptDur(p), w),
		m.infoRowSegs("Path", pathSegs(th, modalCwd(p.cmds), vw), w),
		m.infoRow("First", theme.RelTime(now, p.firstMs), w),
		m.infoRow("Last", theme.RelTime(now, p.lastMs), w),
	)
	return lines
}

// commandsSegs is the prompt's outcome tally with the table's outcome colors
// on the same glyphs: "3   ✓2 ✗1", count normal, successes green, failures red.
func commandsSegs(th *theme.Theme, p promptStat) []styledSeg {
	return []styledSeg{
		{text: strconv.Itoa(p.count), style: th.Norm},
		{text: "   ", raw: true},
		{text: "✓" + strconv.Itoa(p.success), style: th.ExitOK},
		{text: " ", raw: true},
		{text: "✗" + strconv.Itoa(p.failures), style: th.ExitErr},
	}
}

// cmdInfoLines is the details body for one captured command.
func (m Model) cmdInfoLines(r rec.Record, w, bodyCap int) []string {
	th := m.th
	lines := make([]string, 0, 16)
	lines = append(lines, th.Section.Render(fitPlain("Command", w)))
	for _, l := range wrapHighlighted(th, oneLine(r.Cmd), match.Parse(""), w-1, bodyCap) {
		lines = append(lines, " "+l)
	}
	lines = append(lines, strings.Repeat(" ", w))

	dur := "—"
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	vw := maxInt(1, w-infoLabelW)
	lines = append(lines,
		m.infoRowSegs("Path", pathSegs(th, dashIfEmpty(r.Cwd), vw), w),
		m.infoRowSegs("Host", hueSeg(th, r.Hostname), w),
		m.infoRow("Session", shortSession(r.Session), w),
		m.infoRowSegs("Executor", hueSeg(th, r.Executor), w),
		m.infoRow("Time", theme.AbsTime(r.StartMs), w),
		m.infoRow("Duration", dur, w),
		m.infoRowSegs("Exit", exitSegs(th, r), w),
	)
	if a := m.riskRS.Assess(r.Cmd); a.Level > risk.None {
		lines = append(lines, m.infoRowSegs("Risk", riskSegs(th, a), w))
	}
	return lines
}

// infoRowSegs formats one "Label   value" line of the details pane, with the
// value arriving as styled segments so a row can carry outcome or identity ink.
func (m Model) infoRowSegs(label string, segs []styledSeg, w int) string {
	all := append([]styledSeg{{text: padRight(label, infoLabelW), style: m.th.Dim}}, segs...)
	return padTo(composeSegs(all, false, w, m.th), w)
}

// infoRow is infoRowSegs for the plain rows: one normal-ink value.
func (m Model) infoRow(label, value string, w int) string {
	return m.infoRowSegs(label, []styledSeg{{text: value, style: m.th.Norm}}, w)
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

// wrapPlain word-wraps an unstyled single-line string to width w. maxLines > 0
// caps the result so a huge prompt cannot push the metadata off the details
// pane, ending the last kept line with an ellipsis; 0 leaves it uncapped, the
// same contract wrapHighlighted uses.
func wrapPlain(s string, w, maxLines int) []string {
	if w < 1 {
		w = 1
	}
	s = oneLine(s)
	if s == "" {
		return []string{"—"}
	}
	var out []string
	for s != "" && (maxLines <= 0 || len(out) < maxLines) {
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
