package browse

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/tui/hl"
	"yore/internal/tui/theme"
)

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
		mid = m.renderAgents(w, m.midHeight)
	case viewPrompts:
		top = m.promptsTitle(w)
		mid = m.renderPrompts(w, m.midHeight)
	default:
		top = m.searchLine(w)
		mid = m.renderPanes(w)
	}
	// bubbles/help can overrun its Width when even an ellipsis would not fit;
	// clip as a final guarantee against horizontal overflow. The keymap is
	// contextual: browse, stats, and devices each advertise their own keys.
	helpv := clipW(m.help.View(m.helpKeys()), w)
	return strings.Join([]string{top, mid, m.statusBar(w), helpv}, "\n")
}

// searchLine draws the "❯ query" input row. It is never wider than w: the
// input viewport is clamped in applyLayout and the prompt is two columns.
func (m Model) searchLine(w int) string {
	prompt := m.th.Dim.Render("❯ ")
	if m.searching {
		prompt = m.th.Prompt.Render("❯ ")
	}
	return clipW(prompt+m.ti.View(), w)
}

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

// box wraps inner content (already sized to contentW × contentH) in a rounded
// border, accent when focused and dim otherwise.
func (m Model) box(focused bool, contentW, contentH int, inner string) string {
	st := m.borderBlur
	if focused {
		st = m.borderFocus
	}
	if contentW < 1 {
		contentW = 1
	}
	if contentH < 1 {
		contentH = 1
	}
	return st.Width(contentW).Height(contentH).
		MaxWidth(contentW + 2).MaxHeight(contentH + 2).
		Render(inner)
}

// renderPanes lays out the three browse panes: host sidebar on the left, the
// results table over the detail pane on the right.
func (m Model) renderPanes(w int) string {
	lw := m.leftW
	rightOuter := w - lw

	left := m.box(m.focus == focusHosts, lw-2, m.midHeight-2, m.leftInner(lw-2, m.midHeight-2))
	tbl := m.box(m.focus == focusTable, rightOuter-2, m.tableOuterH-2, m.tableInner(rightOuter-2, m.tableOuterH-2))
	det := m.box(m.focus == focusDetail, rightOuter-2, m.detailOuterH-2, m.detail.View())

	right := lipgloss.JoinVertical(lipgloss.Left, tbl, det)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// --- host sidebar -------------------------------------------------------

func (m Model) leftInner(w, h int) string {
	lines := make([]string, 0, h)
	lines = append(lines, m.th.Title.Render(fitPlain("HOSTS", w)))

	avail := h - 1
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

type colLayout struct {
	relW, hostW, exitW, durW, tagW, cmdW int
	showHost, showTag                    bool
}

// colLayout distributes the table content width across fixed columns, dropping
// optional columns (tag, duration, host, exit) when the terminal is too narrow
// so the command always keeps room and the row never overflows. The tag column
// only appears when the result set actually carries tags, and it sheds first so
// it never crowds out host/exit on a narrow terminal.
func (m Model) colLayout() colLayout {
	w := m.tableWidth
	showHost := m.hosts[m.hostSel].scope == proto.ScopeAll
	showTag := m.hasTags
	relW, exitW, durW, hostW, tagW := 8, 4, 7, 0, 0
	if showHost {
		hostW = 14
	}
	if showTag {
		tagW = 12
	}
	prefix := func() int {
		p := 0
		if relW > 0 {
			p += relW + 1
		}
		if hostW > 0 {
			p += hostW + 1
		}
		if exitW > 0 {
			p += exitW + 1
		}
		if durW > 0 {
			p += durW + 1
		}
		if tagW > 0 {
			p += tagW + 1
		}
		return p
	}
	if prefix()+5 > w {
		tagW, showTag = 0, false
	}
	if prefix()+5 > w {
		durW = 0
	}
	if prefix()+5 > w {
		hostW, showHost = 0, false
	}
	if prefix()+5 > w {
		exitW = 0
	}
	if prefix()+3 > w {
		relW = 0
	}
	cmdW := w - prefix()
	if cmdW < 0 {
		cmdW = 0
	}
	return colLayout{relW: relW, hostW: hostW, exitW: exitW, durW: durW, tagW: tagW, cmdW: cmdW, showHost: showHost, showTag: showTag}
}

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
		msg := "no history yet"
		if m.ti.Value() != "" {
			msg = "no matches"
		}
		lines = append(lines, centered(m.th.Dim.Render(msg), w, bodyRows)...)
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

func (m Model) tableHeader(l colLayout, w int) string {
	var segs []styledSeg
	add := func(text string, width int, left bool) {
		if width <= 0 {
			return
		}
		cell := padLeft(text, width)
		if left {
			cell = padRight(truncCols(text, width), width)
		}
		segs = append(segs,
			styledSeg{text: cell, style: m.th.Dim},
			styledSeg{text: " ", raw: true},
		)
	}
	add("time", l.relW, false)
	if l.showHost {
		add("host", l.hostW, true)
	}
	add("exit", l.exitW, true)
	add("dur", l.durW, false)
	if l.showTag {
		add("tag", l.tagW, true)
	}
	if l.cmdW > 0 {
		segs = append(segs, styledSeg{text: padRight("command", l.cmdW), style: m.th.Dim})
	}
	return composeSegs(segs, false, w, m.th)
}

func (m Model) renderRow(r rec.Record, l colLayout, q match.Query, selected bool, w int, now int64) string {
	th := m.th
	var segs []styledSeg
	sep := func() { segs = append(segs, styledSeg{text: " ", raw: true}) }

	if l.relW > 0 {
		segs = append(segs, styledSeg{text: padLeft(theme.RelTime(now, r.StartMs), l.relW), style: th.Dim})
		sep()
	}
	if l.showHost {
		segs = append(segs, styledSeg{text: padRight(truncCols(r.Hostname, l.hostW), l.hostW), style: th.Host(r.Hostname)})
		sep()
	}
	if l.exitW > 0 {
		mark, style := exitMarker(th, r)
		segs = append(segs, styledSeg{text: padRight(truncCols(mark, l.exitW), l.exitW), style: style})
		sep()
	}
	if l.durW > 0 {
		dur := ""
		if r.DurMs != nil {
			dur = theme.Duration(*r.DurMs)
		}
		segs = append(segs, styledSeg{text: padLeft(dur, l.durW), style: th.Dim})
		sep()
	}
	if l.showTag {
		// Tagged cells stand out (accent); interactive rows render blank.
		segs = append(segs, styledSeg{text: padRight(truncCols(r.Tag, l.tagW), l.tagW), style: th.Accent})
		sep()
	}
	if l.cmdW > 0 {
		cmdSegs, used := commandSegments(th, r.Cmd, q, l.cmdW)
		segs = append(segs, cmdSegs...)
		if pad := l.cmdW - used; pad > 0 {
			segs = append(segs, styledSeg{text: strings.Repeat(" ", pad), raw: true})
		}
	}
	return composeSegs(segs, selected, w, th)
}

func exitMarker(th *theme.Theme, r rec.Record) (string, lipgloss.Style) {
	if r.Exit == nil {
		return "·", th.Dim
	}
	if *r.Exit == 0 {
		return "·", th.ExitOK
	}
	return "✗" + strconv.Itoa(*r.Exit), th.ExitErr
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
	for _, line := range wrapHighlighted(m.th, r.Cmd, q, w) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	meta := func(label, val string) {
		lab := m.th.Dim.Render(padRight(label, 8))
		vw := w - 9
		if vw < 1 {
			vw = 1
		}
		b.WriteString(lab + " " + m.th.Norm.Render(truncCols(val, vw)))
		b.WriteByte('\n')
	}
	dash := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	meta("cwd", dash(r.Cwd))
	meta("host", dash(r.Hostname))
	meta("session", dash(r.Session))
	if r.Tag != "" {
		meta("ran by", r.Tag)
	}
	meta("time", time.UnixMilli(r.StartMs).Local().Format("2006-01-02 15:04:05"))
	dur := "—"
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	meta("dur", dur)
	exit := "—"
	if r.Exit != nil {
		exit = strconv.Itoa(*r.Exit)
	}
	meta("exit", exit)

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
	switch m.view {
	case viewStats, viewAgents, viewPrompts:
		// These aggregate views are self-describing (the panels/table show their
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
		if m.executorFilter != "" {
			pieces = append(pieces, th.Accent.Render("executor: "+m.executorFilter))
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

// commandSegments turns a command into highlighted, single-line, width-limited
// runs, collapsing embedded newlines to a dim ⏎ marker. Returns the runs and
// their total display width.
func commandSegments(th *theme.Theme, cmd string, q match.Query, maxCols int) (segs []styledSeg, width int) {
	if maxCols <= 0 || cmd == "" {
		return nil, 0
	}
	ranges := q.Ranges(cmd)
	syn := hl.Classify(cmd)

	type rk struct {
		r rune
		k int
	}
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

// --- highlight-aware wrapping (detail pane) ------------------------------

type hlRune struct {
	r     rune
	match bool
	brk   bool // a hard line break
}

func wrapHighlighted(th *theme.Theme, s string, q match.Query, w int) []string {
	if w < 1 {
		w = 1
	}
	if s == "" {
		return []string{""}
	}
	ranges := q.Ranges(s)
	ri := 0
	inRange := func(pos int) bool {
		for ri < len(ranges) && ranges[ri][1] <= pos {
			ri++
		}
		return ri < len(ranges) && ranges[ri][0] <= pos
	}

	var seq []hlRune
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			size = 1
		}
		switch r {
		case '\n':
			seq = append(seq, hlRune{brk: true})
		case '\r':
		default:
			seq = append(seq, hlRune{r: r, match: inRange(i)})
		}
		i += size
	}

	var lines []string
	var cur []hlRune
	curW := 0
	lastSpace := -1
	for _, t := range seq {
		if t.brk {
			lines = append(lines, renderHL(th, cur))
			cur, curW, lastSpace = nil, 0, -1
			continue
		}
		rw := runewidth.RuneWidth(t.r)
		if curW+rw > w && len(cur) > 0 {
			if lastSpace > 0 {
				head := cur[:lastSpace]
				tail := append([]hlRune(nil), cur[lastSpace+1:]...)
				lines = append(lines, renderHL(th, head))
				cur = tail
				curW = hlWidth(tail)
			} else {
				lines = append(lines, renderHL(th, cur))
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
	if len(cur) > 0 || len(lines) == 0 {
		lines = append(lines, renderHL(th, cur))
	}
	return lines
}

func hlWidth(ts []hlRune) int {
	w := 0
	for _, t := range ts {
		w += runewidth.RuneWidth(t.r)
	}
	return w
}

func renderHL(th *theme.Theme, ts []hlRune) string {
	if len(ts) == 0 {
		return ""
	}
	var b, run strings.Builder
	curMatch := ts[0].match
	flush := func() {
		if run.Len() > 0 {
			st := th.Norm
			if curMatch {
				st = th.Match
			}
			b.WriteString(st.Render(run.String()))
			run.Reset()
		}
	}
	for _, t := range ts {
		if t.match != curMatch {
			flush()
			curMatch = t.match
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
