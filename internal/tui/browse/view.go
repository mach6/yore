package browse

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/risk"
	"yore/internal/tui/hl"
	"yore/internal/tui/keyhelp"
	"yore/internal/tui/theme"
)

// renderHelp draws the "?" panel: every binding that does something in the view
// underneath it, grouped, in as many columns as the terminal affords. It is
// framed like a pane so it reads as something laid over the view rather than as
// the view having changed.
func (m Model) renderHelp(w, h int) string {
	bw, bh := maxInt(1, w-2), maxInt(1, h-2)
	body, total := keyhelp.Panel(m.th, m.helpGroups(), bw, bh, m.helpTop)
	suffix := m.helpViewName()
	if total > bh {
		suffix += "  ↑↓ for more"
	}
	return m.titledBox(true, bw, bh, "KEYS", suffix, body)
}

// helpViewName names the view the panel is describing — the panel covers it, so
// without this the list has no subject.
func (m Model) helpViewName() string {
	switch m.view {
	case viewStats:
		return "stats"
	case viewAgents:
		return "agents"
	case viewDevices:
		return "devices"
	default:
		return "browse"
	}
}

// View renders the whole alt-screen.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w := m.width
	if w < 1 {
		w = 80
	}

	var top, mid string
	switch m.view {
	case viewStats:
		top = m.statsTitle(w)
		mid = m.renderStats(w, m.midHeight)
	case viewDevices:
		top = m.devicesTitle(w)
		mid = m.renderDevices(w, m.midHeight)
	case viewAgents:
		top = m.agentsTitle(w)
		mid = m.renderAgentsView(w, m.midHeight)
	default:
		top = m.searchLine(w)
		mid = m.renderPanes(w)
	}
	// The key panel takes over the middle, keeping the header and status bar so
	// you can still see which view you asked about.
	if m.showCols {
		mid = m.renderColumns(w, m.midHeight)
	}
	// The key panel outranks the columns pane: it is what you press ? to see, and
	// what it lists while the columns pane is up are the columns pane's own keys.
	if m.showHelp {
		mid = m.renderHelp(w, m.midHeight)
	}
	footer := clipW(keyhelp.Line(m.th, m.footerRows(), w), w)
	return strings.Join([]string{top, mid, m.statusBar(w), footer}, "\n")
}

// searchLine draws the "❯ query" input row, with the shared period tabs pushed
// to the right so the browse view advertises the same 1..5 filter the stats and
// agent screens carry in their headers. It is never wider than w: the input
// viewport is clamped in applyLayout (which reserves the tab strip) and the
// prompt is two columns.
func (m Model) searchLine(w int) string {
	if m.tagging {
		// tagInput already carries a "tag: " prompt.
		return clipW(m.th.Prompt.Render("❯ ")+m.tagInput.View(), w)
	}
	if m.axis != axisNone {
		// The prompt names the axis: this box and the search field share the line,
		// and a bare ❯ would not say which of the three was taking the keystrokes.
		return clipW(m.th.Prompt.Render(filterEntryPrompt(m.axis))+m.filterInput.View(), w)
	}
	prompt := m.th.Dim.Render("❯ ")
	if m.searching {
		prompt = m.th.Prompt.Render("❯ ")
	}
	return m.titleWithTabs(prompt+m.ti.View(), w)
}

// showPeriodTabs reports whether there is room for the tab strip beside a view's
// own header content. On a narrow terminal the header wins and the keys still
// work — the status bar names the active window.
func (m Model) showPeriodTabs() bool { return m.width >= periodTabsWidth()+24 }

// clipW truncates a (possibly styled) line to at most w columns, ANSI-aware.
func clipW(s string, w int) string {
	if w < 0 {
		w = 0
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// titledBox draws a pane: its name seated IN the top border rule, its body
// filling everything inside.
//
//	╭─ PROMPTS  3/47 ──────╮
//	│ 2h  s.4f2  claude  8 │
//	╰──────────────────────╯
//
// The name goes in the rule rather than on a line of its own inside the box
// because a pane only has so many rows: a title line costs one, and in the
// four-pane explorer that was four rows of data spent on chrome. It also stops
// the title competing with the first data row for the eye — a border is read as
// frame, a line inside the box is read as content.
//
// inner is expected to be w columns wide already, but every line is padded and
// clipped to w regardless: a viewport that renders short would otherwise leave
// the right-hand border ragged.
func (m Model) titledBox(focused bool, w, h int, name, suffix, inner string) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	ink := m.inkBlur
	if focused {
		ink = m.inkFocus
	}

	var b strings.Builder
	b.WriteString(m.topRule(ink, focused, w, name, suffix))
	b.WriteByte('\n')

	side := ink.Render("│")
	lines := strings.Split(inner, "\n")
	for i := 0; i < h; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		b.WriteString(side + padTo(line, w) + side)
		b.WriteByte('\n')
	}
	b.WriteString(ink.Render("╰" + strings.Repeat("─", w) + "╯"))
	return b.String()
}

// topRule composes the pane's top border with its name inside it. The rule is
// exactly w+2 columns. A pane too narrow to seat a name gets a plain rule — the
// status bar and the focus ring still say where you are.
func (m Model) topRule(ink lipgloss.Style, focused bool, w int, name, suffix string) string {
	plain := ink.Render("╭" + strings.Repeat("─", w) + "╮")
	if w < 4 {
		return plain
	}
	style := m.th.Title
	if !focused {
		style = m.th.Dim
	}
	head := style.Render(name)
	if suffix != "" {
		head += m.th.Dim.Render("  " + suffix)
	}
	if focused && m.zoom {
		head += m.th.Accent.Render("  ⛶")
	}
	// "╭─ " + head + " " + fill + "╮" spans w+2 columns.
	fill := w - 3 - lipgloss.Width(head)
	if fill < 0 {
		return plain
	}
	return ink.Render("╭─ ") + head + ink.Render(" "+strings.Repeat("─", fill)+"╮")
}

// padTo pads a possibly-styled line with spaces to exactly w columns, clipping
// if it overruns.
func padTo(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return clipW(s, w)
}

// renderPanes lays out the three browse panes: host sidebar on the left, the
// results table over the detail pane on the right. Zoomed, the focused pane
// alone fills the frame.
func (m Model) renderPanes(w int) string {
	if m.zoom {
		r := m.geo.p[m.focus]
		return m.browsePaneBox(m.focus, true, r.w-2, r.h-2)
	}
	lw := m.leftW
	rightOuter := w - lw

	left := m.browsePaneBox(focusHosts, m.focus == focusHosts, lw-2, m.midHeight-2)
	tbl := m.browsePaneBox(focusTable, m.focus == focusTable, rightOuter-2, m.tableOuterH-2)
	det := m.browsePaneBox(focusDetail, m.focus == focusDetail, rightOuter-2, m.detailOuterH-2)

	right := lipgloss.JoinVertical(lipgloss.Left, tbl, det)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// browsePaneBox renders one browse pane's title line and content inside its
// border. Sizes come from the caller so the tiled and zoomed paths share one
// renderer; the titles match the agent explorer's, so both views read the same.
func (m Model) browsePaneBox(f focus, focused bool, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	name, suffix := m.browsePaneHeading(f)
	var inner string
	switch f {
	case focusHosts:
		inner = m.leftInner(w, h)
	case focusTable:
		inner = m.tableInner(w, h)
	default:
		inner = m.detail.View()
	}
	return m.titledBox(focused, w, h, name, suffix, inner)
}

// browsePaneHeading names a browse pane and the count/position that goes beside
// it — the host count, the cursor's place in the result set, the selected
// command's host.
func (m Model) browsePaneHeading(f focus) (name, suffix string) {
	switch f {
	case focusHosts:
		// The aggregate row is not a host, so it does not count as one.
		return "HOSTS", strconv.Itoa(maxInt(0, len(m.hosts)-1))
	case focusTable:
		if m.total == 0 {
			return "COMMANDS", "0"
		}
		return "COMMANDS", fmt.Sprintf("%d/%d", m.sel+1, m.total)
	default:
		r, ok := m.selected()
		if !ok {
			return "DETAILS", ""
		}
		return "DETAILS", r.Hostname
	}
}

// --- host sidebar -------------------------------------------------------

// leftInner renders the host list. The pane's own "HOSTS" title is drawn by
// browsePaneBox, so this is purely the rows.
func (m Model) leftInner(w, h int) string {
	lines := make([]string, 0, h)

	avail := h
	if avail < 1 {
		avail = 1
	}
	start := 0
	if m.hostSel >= avail {
		start = m.hostSel - avail + 1
	}
	for i := start; i < len(m.hosts) && i < start+avail; i++ {
		lines = append(lines, m.hostLine(i, w))
	}
	return padLines(lines, w, h)
}

func (m Model) hostLine(i, w int) string {
	it := m.hosts[i]
	count := strconv.Itoa(it.count)
	labelMax := w - runewidth.StringWidth(count) - 1
	if labelMax < 1 {
		labelMax = 1
	}
	label := truncCols(it.label, labelMax)
	gap := w - runewidth.StringWidth(label) - runewidth.StringWidth(count)
	if gap < 1 {
		gap = 1
	}

	if i == m.hostSel {
		plain := label + strings.Repeat(" ", gap) + count
		return m.th.Sel.Render(fitPlain(plain, w))
	}

	labelStyle := m.th.Norm
	switch {
	case i == 0:
		labelStyle = m.th.Accent // "All hosts"
	case it.scope == proto.ScopeHost || it.scope == proto.ScopeLocal:
		labelStyle = m.th.Host(it.label)
	}
	return labelStyle.Render(label) + strings.Repeat(" ", gap) + m.th.Dim.Render(count)
}

// --- results table ------------------------------------------------------

// Executor and tags are two columns, never one. They answer different questions
// — which agent ran this, versus what did I label it — and merging them meant a
// user tag competed for cells with "claude-code" on every agent row. The set, the
// widths, the shedding order and the sort keys all live in columns.go.

func (m Model) tableInner(w, h int) string {
	l := m.colLayout()
	lines := make([]string, 0, h)
	lines = append(lines, m.tableHeader(l, w))

	bodyRows := h - 1
	if bodyRows < 1 {
		bodyRows = 1
	}

	switch {
	case len(m.rows) == 0 && !m.gotResult:
		lines = append(lines, centered(m.th.Dim.Render("loading…"), w, bodyRows)...)
	case len(m.rows) == 0:
		// An empty screen is a place to say what to do next, not just that there is
		// nothing. A search that found nothing is the user's own doing and needs no
		// instruction; an empty history means yore has nothing to show yet. But
		// when the only matches are behind the agent filter, "no matches" is a lie
		// about the user's own history — so that case names the way through.
		msg := "no history yet — run a command, or import with: yore import auto"
		switch note := m.hiddenAgentsNote(); {
		case note != "":
			msg = note + " — A shows them"
		case m.ti.Value() != "":
			msg = "no matches"
		}
		lines = append(lines, centered(m.th.Dim.Render(truncCols(msg, w)), w, bodyRows)...)
	default:
		now := m.now()
		q := match.Parse(m.ti.Value())
		end := m.top + bodyRows
		if end > len(m.rows) {
			end = len(m.rows)
		}
		for i := m.top; i < end; i++ {
			lines = append(lines, m.renderRow(m.rows[i], l, q, i == m.sel, w, now))
		}
	}
	return padLines(lines, w, h)
}

// tableHeader names each visible column, with the sort arrow on the one the table
// is ordered by — where the cell is wide enough to hold it. Narrow columns leave
// it to the status bar rather than truncating the name to make room for a glyph.
func (m Model) tableHeader(l colLayout, w int) string {
	var segs []styledSeg
	for c := range tableColCount {
		width := l.w[c]
		if width <= 0 {
			continue
		}
		spec := tableSpecs[c]
		title := spec.title
		if m.sortCol == c && runewidth.StringWidth(title)+1 <= width {
			title += sortArrow(m.sortDesc)
		}
		segs = append(segs, styledSeg{text: alignCell(title, width, spec.right), style: m.th.Dim})
		if c != colCmd {
			segs = append(segs, styledSeg{text: " ", raw: true})
		}
	}
	return composeSegs(segs, false, w, m.th)
}

// alignCell pads text into a cell of exactly width columns, right-aligned for the
// quantities and left-aligned (truncating) for the names.
func alignCell(text string, width int, right bool) string {
	if right {
		return padLeft(text, width)
	}
	return padRight(truncCols(text, width), width)
}

func (m Model) renderRow(r rec.Record, l colLayout, q match.Query, selected bool, w int, now int64) string {
	th := m.th
	var segs []styledSeg

	for c := range colCmd {
		width := l.w[c]
		if width <= 0 {
			continue
		}
		spec := tableSpecs[c]
		text, style := spec.cell(th, r, now)
		segs = append(segs,
			styledSeg{text: alignCell(text, width, spec.right), style: style},
			styledSeg{text: " ", raw: true},
		)
	}
	if l.w[colCmd] > 0 {
		if selected && m.focus == focusTable && m.hscroll > 0 {
			// The selected row scrolls horizontally to reveal a truncated command
			// (it renders as a flat selection bar anyway, so syntax color is moot).
			segs = append(segs, styledSeg{text: hOffset(oneLine(r.Cmd), m.hscroll, l.w[colCmd]), raw: true})
		} else {
			cmdSegs, used := commandSegments(th, r.Cmd, q, l.w[colCmd])
			segs = append(segs, cmdSegs...)
			if pad := l.w[colCmd] - used; pad > 0 {
				segs = append(segs, styledSeg{text: strings.Repeat(" ", pad), raw: true})
			}
		}
	}
	return composeSegs(segs, selected, w, th)
}

// exitMarker is a command's outcome as a glyph plus a style. Each of the three
// states gets its OWN glyph: success and unknown used to share "·" and differ
// only in color, which put a real distinction out of reach for anyone who cannot
// separate dim grey from green — and put it out of reach of a screenshot.
func exitMarker(th *theme.Theme, r rec.Record) (string, lipgloss.Style) {
	switch {
	case r.Exit == nil:
		return "·", th.Dim
	case *r.Exit == 0:
		return "✓", th.ExitOK
	default:
		return "✗" + strconv.Itoa(*r.Exit), th.ExitErr
	}
}

// exitWord is a command's outcome spelled out, for the details panes where there
// is room for a word. It leads with the same glyph exitMarker uses in the table,
// so the two never disagree about what a "·" means.
func exitWord(r rec.Record) string {
	switch {
	case r.Exit == nil:
		return "· unknown"
	case *r.Exit == 0:
		return "✓ 0"
	default:
		return "✗ " + strconv.Itoa(*r.Exit)
	}
}

// exitSegs is exitWord with the table's outcome colors on the same glyphs —
// the text is identical, only the ink differs.
func exitSegs(th *theme.Theme, r rec.Record) []styledSeg {
	switch {
	case r.Exit == nil:
		return []styledSeg{{text: "· unknown", style: th.Dim}}
	case *r.Exit == 0:
		return []styledSeg{{text: "✓", style: th.ExitOK}, {text: " 0", style: th.Norm}}
	default:
		return []styledSeg{{text: "✗ " + strconv.Itoa(*r.Exit), style: th.ExitErr}}
	}
}

// pathSegs dims a path's directory and keeps its leaf normal, so the eye lands
// on the name rather than the boilerplate prefix. Values without a slash
// (including the "—" placeholder) render plain.
func pathSegs(th *theme.Theme, path string, maxCols int) []styledSeg {
	p := truncCols(path, maxCols)
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return []styledSeg{{text: p, style: th.Norm}}
	}
	segs := []styledSeg{{text: p[:i+1], style: th.Dim}}
	if leaf := p[i+1:]; leaf != "" {
		segs = append(segs, styledSeg{text: leaf, style: th.Norm})
	}
	return segs
}

// hueSeg is one identity-colored value cell: a hostname or executor in its
// stable hue, or a plain dash when absent.
func hueSeg(th *theme.Theme, name string) []styledSeg {
	if name == "" {
		return []styledSeg{{text: "—", style: th.Norm}}
	}
	return []styledSeg{{text: name, style: th.Host(name)}}
}

// riskStyle maps a risk level to its ink: critical borrows the exit red,
// medium the match amber, and high is the ramp's own orange between them.
func riskStyle(th *theme.Theme, l risk.Level) lipgloss.Style {
	switch l {
	case risk.Critical:
		return th.ExitErr
	case risk.High:
		return th.RiskHigh
	case risk.Medium:
		return th.Match
	default:
		return th.Dim
	}
}

// riskSegs renders a verdict as "⚠ high (script-exec)": glyph and level in the
// tier's ink, the category dim. Glyph-first, so the tier survives without color.
func riskSegs(th *theme.Theme, a risk.Assessment) []styledSeg {
	segs := []styledSeg{{text: a.Level.Glyph() + " " + a.Level.String(), style: riskStyle(th, a.Level)}}
	if a.Category != "" {
		segs = append(segs, styledSeg{text: " (" + a.Category + ")", style: th.Dim})
	}
	return segs
}

// --- detail pane --------------------------------------------------------

func (m *Model) syncDetail() {
	w := m.detail.Width
	if w < 1 {
		w = 1
	}
	r, ok := m.selected()
	if !ok {
		m.detail.SetContent(m.th.Dim.Render("no selection"))
		m.detail.SetYOffset(0)
		return
	}
	q := match.Parse(m.ti.Value())

	var b strings.Builder
	for _, line := range wrapHighlighted(m.th, r.Cmd, q, w, 0) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	// Same field names, same order, same label column as the agent explorer's
	// details pane (see cmdInfoLines): the two panes are one keystroke apart and
	// describe the same record, so calling a cwd "cwd" here and "Path" there just
	// makes the reader re-learn the record.
	meta := func(label, val string) {
		b.WriteString(m.infoRow(label, val, w))
		b.WriteByte('\n')
	}
	metaSegs := func(label string, segs []styledSeg) {
		b.WriteString(m.infoRowSegs(label, segs, w))
		b.WriteByte('\n')
	}
	vw := maxInt(1, w-infoLabelW)
	metaSegs("Path", pathSegs(m.th, dashIfEmpty(r.Cwd), vw))
	metaSegs("Host", hueSeg(m.th, r.Hostname))
	meta("Session", dashIfEmpty(r.Session))
	if r.Executor != "" {
		metaSegs("Executor", hueSeg(m.th, r.Executor))
	}
	if len(r.Tags) > 0 {
		metaSegs("Tags", []styledSeg{{text: strings.Join(r.Tags, ", "), style: m.th.Accent}})
	}
	meta("Time", theme.AbsTime(r.StartMs))
	dur := "—"
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	meta("Duration", dur)
	metaSegs("Exit", exitSegs(m.th, r))
	if a := m.riskRS.Assess(r.Cmd); a.Level > risk.None {
		metaSegs("Risk", riskSegs(m.th, a))
	}

	m.detail.SetContent(strings.TrimRight(b.String(), "\n"))
	m.detail.SetYOffset(0)
}

// --- status bar ---------------------------------------------------------

func (m Model) statusBar(w int) string {
	th := m.th

	if m.confirmDelete {
		if r, ok := m.selected(); ok {
			prompt := th.ExitErr.Render("delete this command? ") + th.Accent.Render("(y/n)")
			hint := th.Dim.Render("  " + truncCols(firstLine(r.Cmd), maxInt(0, w-40)))
			return clipW(prompt+hint, w)
		}
	}

	var pieces []string
	if m.flash != "" {
		pieces = append(pieces, th.Accent.Render(m.flash))
	}
	if m.lastErr != nil {
		pieces = append(pieces, th.ExitErr.Render("daemon unreachable"))
	}

	// Row position and scope are browse-table concepts; in the aggregate Stats
	// view and the Devices view there is no table row, so surface a summary that
	// actually fits the view instead of a meaningless "row N/M".
	if m.zoom {
		pieces = append(pieces, th.Accent.Render("zoomed")+th.Dim.Render(" — z restores the panes"))
	}

	switch m.view {
	case viewStats, viewAgents:
		// These aggregate views are self-describing (the panels/panes show their
		// own totals), so the status bar stays minimal — just any flash/error.
	case viewDevices:
		unit := "devices"
		if len(m.devices) == 1 {
			unit = "device"
		}
		pieces = append(pieces, th.Dim.Render(fmt.Sprintf("%d %s", len(m.devices), unit)))
	default:
		pos := 0
		if len(m.rows) > 0 {
			pos = m.sel + 1
		}
		pieces = append(pieces, th.Dim.Render(fmt.Sprintf("row %d/%d", pos, m.total)))
		pieces = append(pieces, th.Dim.Render(scopeWord(m.hosts[m.hostSel])))
		// Name the window whenever it hides anything — and always when the tab
		// strip did not fit, so the active period is never invisible.
		if m.period != allPeriod {
			label := statPeriods[m.period].label
			if hidden := len(m.allRows) - len(m.rows); hidden > 0 {
				label += fmt.Sprintf(" (%d older hidden)", hidden)
			}
			pieces = append(pieces, th.Accent.Render(label))
		}
		// Two filters, named separately: they narrow on different axes and can
		// both be up at once.
		if m.executorFilter != "" {
			pieces = append(pieces, th.Accent.Render("executor: "+m.executorFilter))
		}
		if m.tagFilter != "" {
			pieces = append(pieces, th.Accent.Render("tag: "+m.tagFilter))
		}
		// The sort, named only when it is not the order the table opens in — and
		// named in words, since a narrow column's header has no room for the arrow.
		if !m.sortedByDefault() {
			pieces = append(pieces, th.Accent.Render(
				"sort: "+tableSpecs[m.sortCol].title+" "+sortArrow(m.sortDesc)))
		}
		if n := m.hiddenColCount(); n > 0 {
			pieces = append(pieces, th.Dim.Render(plural(n, "column")+" hidden"))
		}
		// A filter that drops a whole category of history has to say so. Without
		// this the view is indistinguishable from one where no agent ever ran, and
		// the archive looks like it lost the work.
		if note := m.hiddenAgentsNote(); note != "" {
			pieces = append(pieces, th.Dim.Render(note))
		}

		if m.remote.State == proto.RemoteOff &&
			(m.hosts[m.hostSel].scope == proto.ScopeAll || m.hosts[m.hostSel].scope == proto.ScopeHost) {
			pieces = append(pieces, th.Dim.Render("remote sync not configured — showing local only"))
		}
	}

	left := strings.Join(pieces, th.Dim.Render("  •  "))
	if m.opts.Version == "" {
		return clipW(left, w)
	}
	right := th.Dim.Render(m.opts.Version)
	pad := w - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		return clipW(left, w) // drop the version rather than overflow
	}
	return left + strings.Repeat(" ", pad) + right
}

func scopeWord(it hostItem) string {
	switch it.scope {
	case proto.ScopeAll:
		return "all hosts"
	case proto.ScopeLocal:
		return "this host"
	case proto.ScopeHost:
		return it.host
	default:
		return it.scope
	}
}

// --- shared row composition (ported to stay package-independent) --------

type styledSeg struct {
	text  string
	style lipgloss.Style
	raw   bool
}

// composeSegs joins segments into a single line no wider than w. A selected row
// becomes a solid full-width selection bar; otherwise each segment keeps its
// own style.
func composeSegs(segs []styledSeg, selected bool, w int, th *theme.Theme) string {
	if selected {
		var plain strings.Builder
		for _, s := range segs {
			plain.WriteString(s.text)
		}
		return th.Sel.Render(fitPlain(plain.String(), w))
	}
	var b strings.Builder
	width := 0
	for _, s := range segs {
		sw := runewidth.StringWidth(s.text)
		if width+sw > w {
			s.text = truncCols(s.text, w-width)
			sw = runewidth.StringWidth(s.text)
		}
		if s.raw {
			b.WriteString(s.text)
		} else {
			b.WriteString(s.style.Render(s.text))
		}
		width += sw
		if width >= w {
			break
		}
	}
	return b.String()
}

// Normal runs carry an hl.Kind directly (0..N); kindMatch/kindMarker sit above.
const (
	kindNorm   = int(hl.Normal) // 0
	kindMatch  = 100
	kindMarker = 101
)

// rk is one display rune and the kind that colors it (an hl.Kind for normal
// runs, or kindMatch/kindMarker).
type rk struct {
	r rune
	k int
}

// commandSegments turns a command into highlighted, single-line, width-limited
// runs, collapsing embedded newlines to a dim ⏎ marker. Returns the runs and
// their total display width.
func commandSegments(th *theme.Theme, cmd string, q match.Query, maxCols int) (segs []styledSeg, width int) {
	if maxCols <= 0 || cmd == "" {
		return nil, 0
	}
	ranges := q.Ranges(cmd)
	syn := hl.Classify(cmd)

	disp := make([]rk, 0, len(cmd))
	ri := 0
	inRange := func(pos int) bool {
		for ri < len(ranges) && ranges[ri][1] <= pos {
			ri++
		}
		return ri < len(ranges) && ranges[ri][0] <= pos
	}
	skipWS := false
	for i := 0; i < len(cmd); {
		r, size := utf8.DecodeRuneInString(cmd[i:])
		if size == 0 {
			size = 1
		}
		switch {
		case r == '\n' || r == '\r':
			if !skipWS {
				disp = append(disp, rk{' ', kindNorm}, rk{'⏎', kindMarker}, rk{' ', kindNorm})
				skipWS = true
			}
		case skipWS && (r == ' ' || r == '\t'):
		default:
			skipWS = false
			k := int(syn[i]) // hl.Kind for this byte (syntax coloring)
			if inRange(i) {
				k = kindMatch
			}
			disp = append(disp, rk{r, k})
		}
		i += size
	}
	return emitRuns(th, disp, maxCols)
}

// matchSegments renders plain text with the query's matches highlighted, clipped
// to maxCols. Unlike commandSegments it applies no syntax coloring: a prompt is
// prose, and shell highlighting over prose paints arbitrary words as flags and
// paths.
func matchSegments(th *theme.Theme, s string, q match.Query, maxCols int) (segs []styledSeg, width int) {
	if maxCols <= 0 || s == "" {
		return nil, 0
	}
	ranges := q.Ranges(s)
	disp := make([]rk, 0, len(s))
	ri := 0
	for i, r := range s {
		for ri < len(ranges) && ranges[ri][1] <= i {
			ri++
		}
		k := kindNorm
		if ri < len(ranges) && ranges[ri][0] <= i {
			k = kindMatch
		}
		disp = append(disp, rk{r, k})
	}
	return emitRuns(th, disp, maxCols)
}

// emitRuns clips a display sequence to maxCols (appending an ellipsis when it
// had to cut) and groups adjacent same-kind runes into styled runs.
func emitRuns(th *theme.Theme, disp []rk, maxCols int) (segs []styledSeg, width int) {
	total := 0
	for _, d := range disp {
		total += runewidth.RuneWidth(d.r)
	}
	budget := maxCols
	cut := total > maxCols
	if cut {
		budget = maxCols - 1
		if budget < 0 {
			budget = 0
		}
	}
	used := 0
	kept := disp[:0:0]
	for _, d := range disp {
		rw := runewidth.RuneWidth(d.r)
		if used+rw > budget {
			cut = true
			break
		}
		kept = append(kept, d)
		used += rw
	}
	if cut {
		kept = append(kept, rk{'…', kindNorm})
		used++
	}
	if len(kept) == 0 {
		return nil, 0
	}

	var run strings.Builder
	curK := kept[0].k
	flush := func() {
		if run.Len() > 0 {
			segs = append(segs, styledSeg{text: run.String(), style: styleForKind(th, curK)})
			run.Reset()
		}
	}
	for _, d := range kept {
		if d.k != curK {
			flush()
			curK = d.k
		}
		run.WriteRune(d.r)
	}
	flush()
	return segs, used
}

func styleForKind(th *theme.Theme, k int) lipgloss.Style {
	switch k {
	case kindMatch:
		return th.Match
	case kindMarker:
		return th.Dim
	default:
		return th.Syntax(hl.Kind(k)) // k is an hl.Kind for normal runs
	}
}

// --- highlight-aware wrapping (detail panes) ------------------------------

// wrapRune is one display rune plus the kind that colors it (an hl.Kind, or
// kindMatch/kindMarker) and whether it is a hard line break.
type wrapRune struct {
	r   rune
	k   int
	brk bool
}

// wrapHighlighted word-wraps a command to width w with syntax highlighting
// layered under match highlighting (matches win) — the same coloring the table
// gives the same text, kept when the command grows to multiple lines. maxLines
// > 0 caps the output the way wrapPlain does, ending the last kept line with a
// dim ellipsis; 0 leaves it uncapped.
func wrapHighlighted(th *theme.Theme, s string, q match.Query, w, maxLines int) []string {
	if w < 1 {
		w = 1
	}
	if s == "" {
		return []string{""}
	}
	ranges := q.Ranges(s)
	syn := hl.Classify(s)
	ri := 0
	inRange := func(pos int) bool {
		for ri < len(ranges) && ranges[ri][1] <= pos {
			ri++
		}
		return ri < len(ranges) && ranges[ri][0] <= pos
	}

	var seq []wrapRune
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			size = 1
		}
		switch r {
		case '\n':
			seq = append(seq, wrapRune{brk: true})
		case '\r':
		default:
			k := int(syn[i])
			if inRange(i) {
				k = kindMatch
			}
			seq = append(seq, wrapRune{r: r, k: k})
		}
		i += size
	}

	var wrapped [][]wrapRune
	var cur []wrapRune
	curW := 0
	lastSpace := -1
	for _, t := range seq {
		if t.brk {
			wrapped = append(wrapped, cur)
			cur, curW, lastSpace = nil, 0, -1
			continue
		}
		rw := runewidth.RuneWidth(t.r)
		if curW+rw > w && len(cur) > 0 {
			if lastSpace > 0 {
				head := cur[:lastSpace]
				tail := append([]wrapRune(nil), cur[lastSpace+1:]...)
				wrapped = append(wrapped, head)
				cur = tail
				curW = wrapRuneWidth(tail)
			} else {
				wrapped = append(wrapped, cur)
				cur, curW = nil, 0
			}
			lastSpace = -1
		}
		if t.r == ' ' {
			lastSpace = len(cur)
		}
		cur = append(cur, t)
		curW += rw
	}
	if len(cur) > 0 || len(wrapped) == 0 {
		wrapped = append(wrapped, cur)
	}

	if maxLines > 0 && len(wrapped) > maxLines {
		last := wrapped[maxLines-1]
		for wrapRuneWidth(last) > w-1 && len(last) > 0 {
			last = last[:len(last)-1]
		}
		wrapped = wrapped[:maxLines]
		wrapped[maxLines-1] = append(last, wrapRune{r: '…', k: kindMarker})
	}

	lines := make([]string, 0, len(wrapped))
	for _, ln := range wrapped {
		lines = append(lines, renderWrapped(th, ln))
	}
	return lines
}

func wrapRuneWidth(ts []wrapRune) int {
	w := 0
	for _, t := range ts {
		w += runewidth.RuneWidth(t.r)
	}
	return w
}

// renderWrapped is emitRuns' wrapped sibling: adjacent same-kind runes coalesce
// into one styled run, with no clipping — wrapHighlighted already sized the line.
func renderWrapped(th *theme.Theme, ts []wrapRune) string {
	if len(ts) == 0 {
		return ""
	}
	var b, run strings.Builder
	curK := ts[0].k
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(styleForKind(th, curK).Render(run.String()))
			run.Reset()
		}
	}
	for _, t := range ts {
		if t.k != curK {
			flush()
			curK = t.k
		}
		run.WriteRune(t.r)
	}
	flush()
	return b.String()
}

// --- small width helpers ------------------------------------------------

// padLines forces lines to exactly h entries, each rendered plain to width w
// only where it is a bare blank; styled lines are left intact (they were built
// to width).
func padLines(lines []string, w, h int) string {
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

func centered(s string, w, h int) []string {
	sw := lipgloss.Width(s)
	pad := (w - sw) / 2
	if pad < 0 {
		pad = 0
	}
	line := strings.Repeat(" ", pad) + s
	out := make([]string, h)
	mid := h / 2
	for i := range out {
		if i == mid {
			out[i] = line
		} else {
			out[i] = strings.Repeat(" ", w)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func padLeft(s string, w int) string {
	d := w - runewidth.StringWidth(s)
	if d <= 0 {
		return truncCols(s, w)
	}
	return strings.Repeat(" ", d) + s
}

func padRight(s string, w int) string {
	d := w - runewidth.StringWidth(s)
	if d <= 0 {
		return truncCols(s, w)
	}
	return s + strings.Repeat(" ", d)
}

// fitPlain pads or truncates a plain (unstyled) string to exactly w columns.
func fitPlain(s string, w int) string {
	if w <= 0 {
		return ""
	}
	sw := runewidth.StringWidth(s)
	if sw == w {
		return s
	}
	if sw < w {
		return s + strings.Repeat(" ", w-sw)
	}
	return truncCols(s, w)
}

func truncCols(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	used := 0
	var b strings.Builder
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if used+rw > w {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// hOffset returns the display-column window [offset, offset+w) of a single-line
// string, marking a leading "…" when content is hidden to the left and a
// trailing "…" when it continues past the right. Result width is at most w. It
// is how a selected row scrolls horizontally to reveal truncated text.
func hOffset(s string, offset, w int) string {
	if w <= 0 {
		return ""
	}
	runes := []rune(s)
	widths := make([]int, len(runes))
	total := 0
	for i, r := range runes {
		widths[i] = runewidth.RuneWidth(r)
		total += widths[i]
	}
	if total <= w {
		return s // fits whole; nothing to scroll
	}
	// A leading marker costs one column, so the furthest useful offset leaves
	// exactly w-1 content columns visible on the right edge.
	if maxOff := total - (w - 1); offset > maxOff {
		offset = maxOff
	}
	if offset < 0 {
		offset = 0
	}
	// Advance to the first rune at or beyond `offset` display columns.
	start, col := 0, 0
	for start < len(runes) && col < offset {
		col += widths[start]
		start++
	}
	leading := offset > 0
	budget := w
	if leading {
		budget-- // reserve the leading "…"
	}
	end, used := start, 0
	for end < len(runes) && used+widths[end] <= budget {
		used += widths[end]
		end++
	}
	trailing := end < len(runes)
	if trailing {
		for end > start && used+1 > budget { // make room for the trailing "…"
			end--
			used -= widths[end]
		}
	}
	var b strings.Builder
	if leading {
		b.WriteRune('…')
	}
	for i := start; i < end; i++ {
		b.WriteRune(runes[i])
	}
	if trailing {
		b.WriteRune('…')
	}
	return b.String()
}
