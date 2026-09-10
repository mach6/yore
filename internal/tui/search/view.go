package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/mach6/yore/internal/match"
	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/tui/hl"
	"github.com/mach6/yore/internal/tui/keyhelp"
	"github.com/mach6/yore/internal/tui/theme"
)

// Fixed column widths for the row prefix: reltime, hostname, exit marker.
const (
	relW  = 8
	hostW = 10
	exitW = 4
	// A space separates each prefix cell and precedes the command, so the
	// command starts at a fixed column. The host cell is only present for
	// --scope all (the only scope that spans hosts); see prefixWidth.
	prefixNoHost = relW + 1 + exitW + 1
	prefixW      = relW + 1 + hostW + 1 + exitW + 1
)

// showHost reports whether the host column is shown: only --scope all can
// return rows from other hosts, so only there is a host column meaningful.
func (m Model) showHost() bool { return m.scope == proto.ScopeAll }

// prefixWidth is the row prefix width for the current scope.
func (m Model) prefixWidth() int {
	if m.showHost() {
		return prefixW
	}
	return prefixNoHost
}

// command-cell run kinds. Normal runs carry an hl.Kind directly (values 0..N),
// so kindMatch/kindMarker are offset well above the hl.Kind range.
const (
	kindNorm   = int(hl.Normal) // 0
	kindMatch  = 100
	kindMarker = 101
)

// styledSeg is one run of text with an associated style. raw runs (padding,
// gaps) are emitted verbatim.
type styledSeg struct {
	text  string
	style lipgloss.Style
	raw   bool
}

// View renders the whole panel. It returns the empty string once the program
// is done, so Bubble Tea's inline renderer clears the panel on exit.
func (m Model) View() string {
	if m.done {
		return ""
	}
	w := m.width
	if w < 1 {
		w = 80
	}

	var b strings.Builder
	b.WriteString(m.promptLine())
	b.WriteByte('\n')

	q := match.Parse(m.ti.Value())
	nowMs := time.Now().UnixMilli()

	switch {
	case m.showHelp:
		// The key list takes the rows' place and is bounded by the same rowsVisible
		// they are: the panel is drawn inline above the shell prompt, so a list
		// that grew past a full result set would shove the terminal's scrollback
		// around every time someone asked what a key does.
		body, _ := keyhelp.Panel(m.th, m.helpGroups(), m.helpWidth(), m.rowsVisible, m.helpTop)
		for _, line := range strings.Split(body, "\n") {
			if line != "" {
				b.WriteString("  " + line) // the gutter the result rows use
			}
			b.WriteByte('\n')
		}
	case len(m.rows) == 0 && !m.gotResult:
		b.WriteString(m.th.Dim.Render("  loading…"))
		b.WriteByte('\n')
	case len(m.rows) == 0 && m.lastErr == nil:
		// "no matches" is a lie when the matches are sitting behind the agent
		// filter, and this panel's whole job is recall, so the one thing it must
		// never do is tell you a command you ran does not exist.
		msg := "no matches"
		if note := m.hiddenAgentsNote(); note != "" {
			msg = note + "; alt+a shows them"
		}
		b.WriteString(m.th.Dim.Render("  " + msg))
		b.WriteByte('\n')
	default:
		end := m.top + m.rowsVisible
		if end > len(m.rows) {
			end = len(m.rows)
		}
		for i := m.top; i < end; i++ {
			b.WriteString(m.renderRow(m.rows[i], q, i == m.sel, w, nowMs))
			b.WriteByte('\n')
		}
	}

	b.WriteString(m.statusLine(w))
	return b.String()
}

// promptLine is the scope-aware prompt plus the text input.
func (m Model) promptLine() string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.Prompt.Render("yore"))
	if l := scopeLabel(m.scope); l != "" {
		b.WriteByte(' ')
		b.WriteString(th.Dim.Render(l))
	}
	b.WriteString(th.Accent.Render(" ❯ "))
	b.WriteString(m.ti.View())
	return b.String()
}

// renderRow lays out one history row on a single line no wider than w columns.
func (m Model) renderRow(r rec.Record, q match.Query, selected bool, w int, nowMs int64) string {
	th := m.th
	var segs []styledSeg

	rel := theme.RelTime(nowMs, r.StartMs)
	segs = append(segs,
		styledSeg{text: padLeft(rel, relW), style: th.Dim},
		styledSeg{text: " ", raw: true},
	)

	if m.showHost() {
		host := padRight(truncCols(r.Hostname, hostW), hostW)
		segs = append(segs,
			styledSeg{text: host, style: th.Host(r.Hostname)},
			styledSeg{text: " ", raw: true},
		)
	}

	mark, markStyle := m.exitMarker(r)
	segs = append(segs,
		styledSeg{text: padRight(truncCols(mark, exitW), exitW), style: markStyle},
		styledSeg{text: " ", raw: true},
	)

	avail := w - m.prefixWidth() - 2 // 2 cols reserved for the selection marker gutter
	if avail < 1 {
		avail = 1
	}

	dur := ""
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	durWidth := runewidth.StringWidth(dur)
	showDur := dur != "" && avail >= durWidth+2

	cmdMax := avail
	if showDur {
		cmdMax = avail - durWidth - 1
	}
	cmdSegs, cmdWidth := m.commandSegments(r.Cmd, q, cmdMax)
	segs = append(segs, cmdSegs...)

	if showDur {
		gap := avail - cmdWidth - durWidth
		if gap < 1 {
			gap = 1
		}
		segs = append(segs,
			styledSeg{text: strings.Repeat(" ", gap), raw: true},
			styledSeg{text: dur, style: th.Dim},
		)
	}

	return m.composeLine(segs, selected, w)
}

// exitMarker is a command's outcome as a glyph plus a style. Each of the three
// states gets its own glyph: success and unknown used to share "·" and differ
// only in color, which is unreadable for anyone who cannot separate dim grey
// from green.
func (m Model) exitMarker(r rec.Record) (string, lipgloss.Style) {
	switch {
	case r.Exit == nil:
		return "·", m.th.Dim
	case *r.Exit == 0:
		return "✓", m.th.ExitOK
	default:
		return "✗" + strconv.Itoa(*r.Exit), m.th.ExitErr
	}
}

// composeLine joins the segments behind a 2-column selection gutter. The
// selected row shows a bold accent "❯ " marker (always visible, independent of
// background rendering) followed by a reverse-video bar; unselected rows get a
// blank gutter so columns stay aligned.
func (m Model) composeLine(segs []styledSeg, selected bool, w int) string {
	body := w - 2
	if body < 0 {
		body = 0
	}
	if selected {
		var plain strings.Builder
		for _, s := range segs {
			plain.WriteString(s.text)
		}
		line := plain.String()
		if width := runewidth.StringWidth(line); width < body {
			line += strings.Repeat(" ", body-width)
		} else if width > body {
			line = truncCols(line, body)
		}
		return m.th.Prompt.Render("❯ ") + m.th.Sel.Render(line)
	}
	var b strings.Builder
	b.WriteString("  ") // gutter, keeps unselected rows aligned with the marker
	for _, s := range segs {
		if s.raw {
			b.WriteString(s.text)
		} else {
			b.WriteString(s.style.Render(s.text))
		}
	}
	return b.String()
}

// commandSegments turns a command into highlighted, single-line, width-limited
// runs. Newlines collapse to a dim ⏎ marker with following whitespace
// squeezed out; match spans (byte offsets over the original command) are
// colored with theme.Match. It returns the runs and their total display width.
func (m Model) commandSegments(cmd string, q match.Query, maxCols int) (segs []styledSeg, width int) {
	if maxCols <= 0 || cmd == "" {
		return nil, 0
	}
	ranges := m.matchRanges(cmd, q)
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
				disp = append(disp,
					rk{' ', kindNorm}, rk{'⏎', kindMarker}, rk{' ', kindNorm})
				skipWS = true
			}
		case skipWS && (r == ' ' || r == '\t'):
			// squeeze indentation on continuation lines
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

	// Truncate to maxCols display columns, reserving a column for the ellipsis
	// when we cut.
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

	// Group consecutive same-kind runes into segments.
	var run strings.Builder
	curK := kept[0].k
	flush := func() {
		if run.Len() > 0 {
			segs = append(segs, styledSeg{text: run.String(), style: m.styleForKind(curK)})
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

// matchRanges is the spans to mark in a command, from whichever matcher
// actually selected the row: the substring terms normally, and the subsequence
// runes under alt+z. The two disagree completely (nothing the fuzzy matcher found
// need contain the query as a substring), so highlighting with the wrong one
// marks nothing at all.
func (m Model) matchRanges(cmd string, q match.Query) [][2]int {
	if m.fuzzy {
		// The raw input, not q's parsed terms: the daemon fuzzy-matches on the
		// whole query string, spaces and all, and the highlight has to be made
		// of the same runes the row was chosen for.
		return match.FuzzyRanges(m.ti.Value(), cmd)
	}
	return q.Ranges(cmd)
}

func (m Model) styleForKind(k int) lipgloss.Style {
	switch k {
	case kindMatch:
		return m.th.Match
	case kindMarker:
		return m.th.Dim
	default:
		return m.th.Syntax(hl.Kind(k)) // k is an hl.Kind for normal runs
	}
}

type statusPiece struct {
	text  string
	style lipgloss.Style
}

// statusLine renders the bottom line: position/total, any hints, and the
// version on the far right when it fits.
func (m Model) statusLine(w int) string {
	th := m.th

	pos := 0
	if len(m.rows) > 0 {
		pos = m.sel + 1
	}
	pieces := []statusPiece{{fmt.Sprintf("%d/%d", pos, m.total), th.Dim}}
	if m.lastErr != nil {
		pieces = append(pieces, statusPiece{"daemon unreachable", th.ExitErr})
	}
	if !m.dedupe {
		pieces = append(pieces, statusPiece{"duplicates shown", th.Dim})
	}
	// What the list is made of, when it is not the default. The scope says
	// itself in the prompt; these two had nothing anywhere, so a panel sorted
	// by frequency or matched by subsequence was indistinguishable from one
	// that was neither, and there was no way to tell what you were looking at.
	if m.fuzzy {
		pieces = append(pieces, statusPiece{"fuzzy", th.Dim})
	}
	if m.frecency {
		pieces = append(pieces, statusPiece{"by frequency", th.Dim})
	}
	// Ahead of the sync hint and the key hints: a filter withholding results the
	// user is actively searching for outranks both. With no rows at all the empty
	// state is already saying this, and the panel is too small to say it twice.
	if note := m.hiddenAgentsNote(); note != "" && len(m.rows) > 0 {
		pieces = append(pieces, statusPiece{note, th.Dim})
	}
	if m.remote.State == proto.RemoteOff && (m.scope == proto.ScopeAll || m.scope == proto.ScopeHost) {
		pieces = append(pieces, statusPiece{"remote sync not configured; showing local only", th.Dim})
	}

	right := ""
	rightWidth := 0
	if m.opts.Version != "" {
		right = th.Dim.Render(m.opts.Version)
		rightWidth = runewidth.StringWidth(m.opts.Version)
	}

	const sep = "  "
	budget := w
	if right != "" {
		budget = w - rightWidth - 1
	}

	var rendered []string
	leftWidth := 0
	for i, p := range pieces {
		add := runewidth.StringWidth(p.text)
		if i > 0 {
			add += len(sep)
		}
		if i > 0 && leftWidth+add > budget {
			break // always keep the first piece; drop overflow hints
		}
		rendered = append(rendered, p.style.Render(p.text))
		leftWidth += add
	}
	left := strings.Join(rendered, sep)

	// Key hints go last on the line and are dropped whole when they do not fit:
	// they are the least important thing here, and half a hint is not a hint.
	if hints := keyhelp.Line(th, m.hintRows(), budget); hints != "" {
		if hw := lipgloss.Width(hints); leftWidth+len(sep)+hw <= budget {
			left += th.Dim.Render(sep) + hints
			leftWidth += len(sep) + hw
		}
	}

	if right == "" {
		return left
	}
	pad := w - leftWidth - rightWidth
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// --- small width helpers -------------------------------------------------

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
