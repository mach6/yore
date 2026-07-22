package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"yore/internal/match"
	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// promptStat is one user prompt and the aggregate of the agent commands it
// triggered. cmds holds those commands (oldest first) so the explorer can drill
// from a prompt into the exact sequence the agent ran.
type promptStat struct {
	id       string
	text     string
	executor string
	session  string
	count    int
	success  int
	failures int
	durSum   int64
	durN     int
	firstMs  int64
	lastMs   int64
	cmds     []rec.Record
}

// promptsData is the aggregation behind the prompt-explorer view.
type promptsData struct {
	prompts     []promptStat
	total       int // total prompts in the period
	periodLabel string
}

// computePrompts groups agent commands by their triggering prompt over the last
// periodDays days (0 = all). Commands with no prompt id (human commands, or
// agent commands captured before prompt tracing) are excluded.
func computePrompts(rows []rec.Record, now int64, periodDays int) *promptsData {
	cutoff := int64(0)
	if periodDays > 0 {
		cutoff = now - int64(periodDays)*86_400_000
	}
	byID := map[string]*promptStat{}
	for _, r := range rows {
		if r.Deleted() || r.PromptID == "" || r.StartMs < cutoff {
			continue
		}
		p := byID[r.PromptID]
		if p == nil {
			p = &promptStat{
				id: r.PromptID, text: r.Prompt, executor: r.Tag, session: r.Session,
				firstMs: r.StartMs, lastMs: r.StartMs,
			}
			byID[r.PromptID] = p
		}
		if p.text == "" {
			p.text = r.Prompt
		}
		if p.session == "" {
			p.session = r.Session
		}
		p.count++
		if r.StartMs > p.lastMs {
			p.lastMs = r.StartMs
		}
		if r.StartMs < p.firstMs {
			p.firstMs = r.StartMs
		}
		if r.Exit != nil {
			if *r.Exit == 0 {
				p.success++
			} else {
				p.failures++
			}
		}
		if r.DurMs != nil {
			p.durSum += *r.DurMs
			p.durN++
		}
		p.cmds = append(p.cmds, r)
	}

	out := &promptsData{periodLabel: periodLabel(periodDays)}
	for _, p := range byID {
		// Chronological within a prompt: this is the agent's actual working order.
		sort.SliceStable(p.cmds, func(i, j int) bool { return p.cmds[i].StartMs < p.cmds[j].StartMs })
		out.prompts = append(out.prompts, *p)
	}
	out.total = len(out.prompts)
	sort.Slice(out.prompts, func(i, j int) bool {
		return out.prompts[i].lastMs > out.prompts[j].lastMs // most recent first
	})
	return out
}

// promptDur is the total execution time across a prompt's commands, or "—" when
// no command carried a known duration.
func promptDur(p promptStat) string {
	if p.durN == 0 {
		return "—"
	}
	return theme.Duration(p.durSum)
}

// shortSession trims an opaque session id to a stable, readable prefix — enough
// to tell one agent conversation from another without eating the row.
func shortSession(s string) string {
	if s == "" {
		return "—"
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// drilledPrompt is the prompt the explorer is currently drilled into (the one
// under the cursor when Enter was pressed), and whether one exists.
func (m Model) drilledPrompt() (promptStat, bool) {
	if m.prompts == nil || m.promptSel < 0 || m.promptSel >= len(m.prompts.prompts) {
		return promptStat{}, false
	}
	return m.prompts.prompts[m.promptSel], true
}

// promptsTitle is the header shown in place of the search bar in prompt-explorer
// mode: the period tabs at the top level, or the drilled prompt's text.
func (m Model) promptsTitle(w int) string {
	th := m.th
	if m.promptDrill {
		if p, ok := m.drilledPrompt(); ok {
			head := th.Title.Render("PROMPT") + "  " +
				th.Norm.Render(oneLine(p.text)) +
				th.Dim.Render("   · esc back")
			return clipW(head, w)
		}
	}
	var b strings.Builder
	b.WriteString(th.Title.Render("PROMPTS"))
	b.WriteString("  ")
	for i, p := range statPeriods {
		key := strconv.Itoa(i + 1)
		style := th.Dim
		if i == m.statsPeriod {
			style = th.Accent.Bold(true)
		}
		b.WriteString(style.Render(key+" "+p.label) + " ")
	}
	if m.prompts != nil {
		b.WriteString(th.Dim.Render(fmt.Sprintf(" · %d prompts", m.prompts.total)))
	}
	return clipW(b.String(), w)
}

// renderPrompts draws the prompt-explorer: the drilled command list when a
// prompt is open, otherwise the selectable one-row-per-prompt table.
func (m Model) renderPrompts(w, h int) string {
	th := m.th
	if m.prompts == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}
	if m.promptDrill {
		return m.renderPromptDrill(w, h)
	}
	if len(m.prompts.prompts) == 0 {
		body := []string{
			"",
			"  " + th.Norm.Render("No agent prompts"+periodSuffix(m.prompts.periodLabel)+"."),
			"",
			"  " + th.Dim.Render("Prompt tracing needs the Claude Code hooks:  yore init claude-code"),
		}
		return padLines(body, w, h)
	}

	c := promptLayout(w)
	lines := []string{promptRow(th.Dim, th.Dim, false, w, th,
		"WHEN", "SESSION", "EXECUTOR", "CMDS", "STATUS", "DUR", "PROMPT", c)}

	visible := h - 1
	if visible < 1 {
		visible = 1
	}
	top := windowStart(m.promptSel, visible, len(m.prompts.prompts))
	now := m.now()
	for i := top; i < len(m.prompts.prompts) && i < top+visible; i++ {
		p := m.prompts.prompts[i]
		status, statusStyle := promptStatus(th, p)
		lines = append(lines, promptRow(th.Norm, statusStyle, i == m.promptSel, w, th,
			theme.RelTime(now, p.lastMs), shortSession(p.session), p.executor,
			strconv.Itoa(p.count), status, promptDur(p), oneLine(p.text), c))
	}
	return padLines(lines, w, h)
}

// renderPromptDrill draws the command list for the drilled prompt: the exact
// sequence of commands the agent ran, oldest first, each selectable.
func (m Model) renderPromptDrill(w, h int) string {
	th := m.th
	p, ok := m.drilledPrompt()
	if !ok {
		return padLines([]string{"", "  " + th.Dim.Render("no prompt selected")}, w, h)
	}

	dc := drillCols{whenW: 7, statW: 6, durW: 7}
	dc.cmdW = w - (dc.whenW + dc.statW + dc.durW + 6) // three two-space gaps
	if dc.cmdW < 12 {
		dc.cmdW = 12
	}

	lines := []string{drillHeader(th, dc, w)}
	if len(p.cmds) == 0 {
		lines = append(lines, "  "+th.Dim.Render("no commands recorded for this prompt"))
		return padLines(lines, w, h)
	}

	visible := h - 1
	if visible < 1 {
		visible = 1
	}
	top := windowStart(m.drillSel, visible, len(p.cmds))
	now := m.now()
	q := match.Query{}
	for i := top; i < len(p.cmds) && i < top+visible; i++ {
		lines = append(lines, drillRow(th, p.cmds[i], i == m.drillSel, now, q, dc, w))
	}
	return padLines(lines, w, h)
}

// drillCols is the column layout for the drilled command list.
type drillCols struct{ whenW, statW, durW, cmdW int }

// drillHeader draws the dim column header for the drilled command list.
func drillHeader(th *theme.Theme, dc drillCols, w int) string {
	segs := []styledSeg{
		{text: padLeft("WHEN", dc.whenW), style: th.Dim}, {text: "  ", raw: true},
		{text: padRight("ST", dc.statW), style: th.Dim}, {text: "  ", raw: true},
		{text: padLeft("DUR", dc.durW), style: th.Dim}, {text: "  ", raw: true},
		{text: padRight("COMMAND", dc.cmdW), style: th.Dim},
	}
	return composeSegs(segs, false, w, th)
}

// drillRow formats one command row in the drilled prompt view, syntax-
// highlighting the command and coloring the exit marker (or a full selection bar
// when selected).
func drillRow(th *theme.Theme, r rec.Record, sel bool, now int64, q match.Query, dc drillCols, w int) string {
	var segs []styledSeg
	sep := func() { segs = append(segs, styledSeg{text: "  ", raw: true}) }

	segs = append(segs, styledSeg{text: padLeft(theme.RelTime(now, r.StartMs), dc.whenW), style: th.Dim})
	sep()
	mark, mStyle := exitMarker(th, r)
	segs = append(segs, styledSeg{text: padRight(truncCols(mark, dc.statW), dc.statW), style: mStyle})
	sep()
	dur := "—"
	if r.DurMs != nil {
		dur = theme.Duration(*r.DurMs)
	}
	segs = append(segs, styledSeg{text: padLeft(dur, dc.durW), style: th.Dim})
	sep()
	cmdSegs, used := commandSegments(th, r.Cmd, q, dc.cmdW)
	segs = append(segs, cmdSegs...)
	if pad := dc.cmdW - used; pad > 0 {
		segs = append(segs, styledSeg{text: strings.Repeat(" ", pad), raw: true})
	}
	return composeSegs(segs, sel, w, th)
}

// promptStatus summarizes a prompt's command outcomes as a compact glyph/label
// and the style for its status cell.
func promptStatus(th *theme.Theme, p promptStat) (string, lipgloss.Style) {
	switch {
	case p.failures > 0:
		return fmt.Sprintf("✗%d", p.failures), th.ExitErr
	case p.success > 0:
		return fmt.Sprintf("✓%d", p.success), th.ExitOK
	default:
		return "·", th.Dim
	}
}

// promptCols is the resolved column layout for the prompt table; optional
// columns shed (session, then dur, then executor) on a narrow terminal so the
// prompt text always keeps room.
type promptCols struct {
	whenW, sessW, execW, cmdsW, statW, durW, textW int
	showSess, showExec, showDur                    bool
}

func promptLayout(w int) promptCols {
	c := promptCols{
		whenW: 7, sessW: 8, execW: 12, cmdsW: 4, statW: 6, durW: 7,
		showSess: true, showExec: true, showDur: true,
	}
	prefix := func() int {
		p := c.whenW + 2 + c.cmdsW + 2 + c.statW + 2
		if c.showSess {
			p += c.sessW + 2
		}
		if c.showExec {
			p += c.execW + 2
		}
		if c.showDur {
			p += c.durW + 2
		}
		return p
	}
	if prefix()+12 > w {
		c.showSess = false
	}
	if prefix()+12 > w {
		c.showDur = false
	}
	if prefix()+12 > w {
		c.showExec = false
	}
	c.textW = w - prefix()
	if c.textW < 8 {
		c.textW = 8
	}
	return c
}

// promptRow formats one fixed-width prompt table row (header or data). base
// styles every cell except status, which takes its own color; a selected row
// renders as a solid selection bar.
func promptRow(base, statusStyle lipgloss.Style, sel bool, w int, th *theme.Theme,
	when, sess, exec, cmds, status, dur, text string, c promptCols) string {
	var segs []styledSeg
	sep := func() { segs = append(segs, styledSeg{text: "  ", raw: true}) }
	cell := func(s string, wdt int, style lipgloss.Style, right bool) {
		t := truncCols(s, wdt)
		if right {
			t = padLeft(t, wdt)
		} else {
			t = padRight(t, wdt)
		}
		segs = append(segs, styledSeg{text: t, style: style})
	}

	cell(when, c.whenW, base, false)
	sep()
	if c.showSess {
		cell(sess, c.sessW, base, false)
		sep()
	}
	if c.showExec {
		cell(exec, c.execW, base, false)
		sep()
	}
	cell(cmds, c.cmdsW, base, true)
	sep()
	cell(status, c.statW, statusStyle, false)
	sep()
	if c.showDur {
		cell(dur, c.durW, base, true)
		sep()
	}
	cell(text, c.textW, base, false)
	return composeSegs(segs, sel, w, th)
}

// windowStart returns the first visible index so that sel stays on screen within
// a window of `visible` rows over `n` items (mirrors the host-sidebar scroll).
func windowStart(sel, visible, n int) int {
	if visible < 1 {
		visible = 1
	}
	top := 0
	if sel >= visible {
		top = sel - visible + 1
	}
	if maxTop := n - visible; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// oneLine collapses a possibly multi-line prompt into a single spaced line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
