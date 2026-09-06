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
	host     string
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
	total       int  // total prompts in the period
	hasDur      bool // any prompt carries a known duration (gates the DUR column)
	periodLabel string
}

// computePrompts groups agent commands by their triggering prompt over the last
// periodDays days (0 = all). Commands with no prompt id (human commands, or agent
// commands captured before prompt tracing) are excluded, as are commands from
// another executor when `executor` names one (the sidebar's filter). prompts are
// the prompt records themselves. They seed the aggregate before any command is
// counted, so a prompt that triggered no commands at all still shows up (with a
// count of zero) instead of vanishing because nothing referenced it.
func computePrompts(rows, prompts []rec.Record, now int64, periodDays int, executor string) *promptsData {
	cutoff := periodCutoff(now, periodDays)
	byID := map[string]*promptStat{}
	for _, r := range prompts {
		if r.Deleted() || r.ID == "" || r.StartMs < cutoff {
			continue
		}
		if executor != "" && r.Executor != executor {
			continue
		}
		byID[r.ID] = &promptStat{
			id: r.ID, text: r.Prompt, executor: r.Executor, session: r.Session,
			host: r.Hostname, firstMs: r.StartMs, lastMs: r.StartMs,
		}
	}
	for _, r := range rows {
		if r.Deleted() || r.PromptID == "" || r.StartMs < cutoff {
			continue
		}
		if executor != "" && r.Executor != executor {
			continue
		}
		p := byID[r.PromptID]
		if p == nil {
			p = &promptStat{
				id: r.PromptID, text: r.Prompt, executor: r.Executor, session: r.Session,
				host: r.Hostname, firstMs: r.StartMs, lastMs: r.StartMs,
			}
			byID[r.PromptID] = p
		}
		if p.text == "" {
			p.text = r.Prompt
		}
		if p.executor == "" {
			p.executor = r.Executor
		}
		if p.session == "" {
			p.session = r.Session
		}
		if p.host == "" {
			p.host = r.Hostname
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
		if p.durN > 0 {
			out.hasDur = true // at least one agent reports timing (e.g. Cursor)
		}
		out.prompts = append(out.prompts, *p)
	}
	out.total = len(out.prompts)
	sort.Slice(out.prompts, func(i, j int) bool {
		return out.prompts[i].lastMs > out.prompts[j].lastMs // most recent first
	})
	return out
}

// promptDur is the total execution time across a prompt's commands, or "; "
// when no command carried a known duration.
func promptDur(p promptStat) string {
	if p.durN == 0 {
		return theme.Unknown
	}
	return theme.Duration(p.durSum)
}

// shortSession trims an opaque session id to a stable, readable prefix; enough
// to tell one agent conversation from another without eating the row.
func shortSession(s string) string {
	if s == "" {
		return theme.Unknown
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// drilledPrompt is the prompt currently under the prompt-pane cursor (the one
// whose commands the command pane shows) and whether one exists.
func (m Model) drilledPrompt() (promptStat, bool) {
	if m.promptSel < 0 || m.promptSel >= len(m.filteredPrompts) {
		return promptStat{}, false
	}
	return m.filteredPrompts[m.promptSel], true
}

// noPromptMsg says why the command and details panes have nothing to show. The
// two reasons read very differently to a reader: a window with no agent prompts
// in it at all is not the same as prompts being present with none under the
// cursor, and reporting the first as "no prompt selected" blames the user for
// something they did not do.
func (m Model) noPromptMsg() string {
	if m.promptLen() == 0 {
		return "no agent prompts in this window; 1..5 widens it"
	}
	return "no prompt selected"
}

// drilledCmd is the command under the command-pane cursor, and whether one
// exists.
func (m Model) drilledCmd() (rec.Record, bool) {
	cmds := m.visibleCmds()
	if m.drillSel < 0 || m.drillSel >= len(cmds) {
		return rec.Record{}, false
	}
	return cmds[m.drillSel], true
}

// promptListInner renders the prompt pane: the one-row-per-prompt table,
// windowed so the selected prompt stays on screen.
func (m Model) promptListInner(w, h int) string {
	th := m.th
	if m.prompts == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}
	if len(m.prompts.prompts) == 0 {
		// Keep the pane's frame and header; only the body reports the emptiness.
		return padLines([]string{
			"",
			"  " + th.Norm.Render("No agent prompts"+periodSuffix(m.prompts.periodLabel)+"."),
			"",
			"  " + th.Dim.Render("Prompt tracing needs an agent's hooks:  yore init claude-code"),
		}, w, h)
	}

	l := m.tableLayout(ctPrompts, w)
	// Lowercase, like every other column header in the UI: uppercase is reserved
	// for pane names, which now sit in the border above this.
	lines := []string{composeSegs(m.tableHeaderSegs(l), false, w, th)}

	rows := m.filteredPrompts
	if len(rows) == 0 {
		// The prompts exist; this query just does not reach them. Say which: the
		// pane is otherwise indistinguishable from a period with no agent work.
		lines = append(lines, "", "  "+th.Dim.Render(
			fmt.Sprintf("no prompt matches %q; esc clears the filter", m.promptQ)))
		return padLines(lines, w, h)
	}

	visible := h - 1
	if visible < 1 {
		visible = 1
	}
	textW := l.flexW()
	q := match.Parse(m.promptQ)
	sel := clampIndex(m.promptSel, len(rows))
	top := windowStart(sel, visible, len(rows))
	now := m.now()
	for i := top; i < len(rows) && i < top+visible; i++ {
		p := rows[i]
		segs := rowSegs(th, ctPrompts, promptSpecs, l, p, now)
		text := oneLine(p.text)
		// The selected row scrolls horizontally to reveal a long prompt, but only
		// when this pane holds focus; else the command pane owns ←/→.
		if i == sel && m.apane == apPrompts && m.hscroll > 0 {
			segs = append(segs, plainSegs(th.Norm, hOffset(text, m.hscroll, textW), textW)...)
		} else {
			tsegs, used := matchSegments(th, text, q, textW)
			segs = append(segs, tsegs...)
			segs = append(segs, padSeg(textW-used))
		}
		lines = append(lines, composeSegs(segs, i == sel, w, th))
	}
	return padLines(lines, w, h)
}

// promptCmdInner renders the command pane: the highlighted prompt's command
// sequence, oldest first (the agent's actual working order), windowed on the
// command cursor. It always tracks the prompt under the prompt-pane cursor; the
// prompt's own text and metadata live in the details pane.
func (m Model) promptCmdInner(w, h int) string {
	th := m.th
	p, ok := m.drilledPrompt()
	if !ok {
		return padLines([]string{"", "  " + th.Dim.Render(m.noPromptMsg())}, w, h)
	}
	l := m.tableLayout(ctCommands, w)
	lines := []string{composeSegs(m.tableHeaderSegs(l), false, w, th)}
	if len(p.cmds) == 0 {
		lines = append(lines, "  "+th.Dim.Render("no commands recorded for this prompt"))
		return padLines(lines, w, h)
	}

	cmds := m.visibleCmds()
	if len(cmds) == 0 {
		// This prompt ran commands; none of them match. Naming the query keeps the
		// pane from reading as "this prompt did nothing".
		lines = append(lines, "", "  "+th.Dim.Render(
			fmt.Sprintf("none of this prompt's %d commands match %q; esc clears the filter",
				len(p.cmds), m.cmdQ)))
		return padLines(lines, w, h)
	}

	visible := h - len(lines)
	if visible < 1 {
		visible = 1
	}
	// Only the focused pane scrolls; the prompt pane owns the offset otherwise.
	hs := 0
	if m.apane == apCommands {
		hs = m.hscroll
	}
	now := m.now()
	sel := clampIndex(m.drillSel, len(cmds))
	top := windowStart(sel, visible, len(cmds))
	q := match.Parse(m.cmdQ)
	cmdW := l.flexW()
	for i := top; i < len(cmds) && i < top+visible; i++ {
		r := cmds[i]
		segs := rowSegs(th, ctCommands, drillSpecs, l, r, now)
		if i == sel && hs > 0 {
			segs = append(segs, styledSeg{text: hOffset(oneLine(r.Cmd), hs, cmdW), raw: true})
		} else {
			cmdSegs, used := commandSegments(th, r.Cmd, q, cmdW)
			segs = append(segs, cmdSegs...)
			if pad := cmdW - used; pad > 0 {
				segs = append(segs, styledSeg{text: strings.Repeat(" ", pad), raw: true})
			}
		}
		lines = append(lines, composeSegs(segs, i == sel, w, th))
	}
	return padLines(lines, w, h)
}

// modalCwd returns the most common non-empty cwd across a prompt's commands.
func modalCwd(cmds []rec.Record) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for i := range cmds {
		cwd := cmds[i].Cwd
		if cwd == "" {
			continue
		}
		counts[cwd]++
		if counts[cwd] > bestN {
			best, bestN = cwd, counts[cwd]
		}
	}
	if best == "" {
		return theme.Unknown
	}
	return best
}

// promptStatus summarizes a prompt's command outcomes as a compact glyph/label
// and the style for its status cell. The CMDS column already carries the total,
// so a clean run is just a green ✓ (not "✓N", which read as an exit code and
// duplicated CMDS); any failures surface as a red ✗N: the count that matters.
func promptStatus(th *theme.Theme, p promptStat) (string, lipgloss.Style) {
	switch {
	case p.failures > 0:
		return "✗" + strconv.Itoa(p.failures), th.ExitErr
	case p.success > 0:
		return "✓", th.ExitOK
	default:
		return "·", th.Dim
	}
}

// plainSegs is one unhighlighted cell, truncated and padded to width.
func plainSegs(style lipgloss.Style, s string, w int) []styledSeg {
	return []styledSeg{{text: padRight(truncCols(s, w), w), style: style}}
}

// padSeg is n columns of filler, or nothing when the cell already fills.
func padSeg(n int) styledSeg {
	if n < 1 {
		return styledSeg{raw: true}
	}
	return styledSeg{text: strings.Repeat(" ", n), raw: true}
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
