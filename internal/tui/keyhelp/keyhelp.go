// Package keyhelp renders a view's key bindings two ways from one description:
// the terse line that sits in the footer all the time, and the full grouped
// panel behind "?".
//
// Both come from the same [Group] / [Row] values so the two can't drift: a
// binding that is real enough to appear in the panel is described in exactly one
// place, and the footer is a hand-picked subset of it rather than a second list
// someone has to remember to update.
package keyhelp

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/tui/theme"
)

// Row is one binding: the keys as the user should press them, and what they do.
// Keys is display text ("↑/k", "^t", "1-5"), not a key name the program matches
// against — dispatch happens on the raw key strings in Keyed.
type Row struct {
	Keys string
	Desc string
	// Keyed lists the raw key strings this row stands for (what a KeyMsg's
	// String() returns). It documents the mapping from what is shown to what is
	// handled, so a test can hold the help to the dispatcher.
	Keyed []string
}

// Group is a titled block of rows in the full panel.
type Group struct {
	Title string
	Rows  []Row
}

// gutter is the gap between columns in the full panel.
const gutter = 3

// Line renders the terse footer: "key desc · key desc · …", clipped to w by
// dropping whole pieces from the end. The first piece is always kept — a footer
// that shows one binding is still a footer; a truncated one is noise.
func Line(th *theme.Theme, rows []Row, w int) string {
	if w < 1 || len(rows) == 0 {
		return ""
	}
	const sep = " · "

	var b strings.Builder
	used := 0
	for i, r := range rows {
		piece := r.Keys + " " + r.Desc
		add := runewidth.StringWidth(piece)
		if i > 0 {
			add += len(sep)
		}
		if i > 0 && used+add > w {
			break
		}
		if i > 0 {
			b.WriteString(th.Dim.Render(sep))
		}
		b.WriteString(th.Norm.Render(r.Keys))
		b.WriteByte(' ')
		b.WriteString(th.Dim.Render(r.Desc))
		used += add
	}
	// The first piece is kept even when it does not fit, so that a very narrow
	// terminal still says something; clip it rather than let it wrap.
	if used > w {
		return lipgloss.NewStyle().MaxWidth(w).Render(b.String())
	}
	return b.String()
}

// Panel renders the full grouped key list into a w×h block, windowed at row
// `top`. It reports the total number of rows the content wants, so the caller
// can clamp its scroll offset and say whether there is more below.
//
// Groups are packed into as many columns as w affords and are filled in reading
// order — down the first column, then the second — so the list reads the way it
// is written however wide the terminal is.
func Panel(th *theme.Theme, groups []Group, w, h, top int) (body string, total int) {
	if w < 1 || h < 1 {
		return "", 0
	}

	blocks := make([][]styledLine, 0, len(groups))
	blockW := 0
	for _, g := range groups {
		blk := renderGroup(th, g)
		if len(blk) == 0 {
			continue
		}
		blocks = append(blocks, blk)
		for _, l := range blk {
			blockW = maxInt(blockW, l.width)
		}
	}
	if len(blocks) == 0 {
		return strings.Repeat("\n", h-1), 0
	}

	cols := (w + gutter) / (blockW + gutter)
	cols = clampInt(cols, 1, len(blocks))

	// A blank line separates blocks within a column, so it counts toward the
	// height a column needs.
	lines := 0
	for _, blk := range blocks {
		lines += len(blk) + 1
	}
	target := (lines + cols - 1) / cols

	columns := make([][]styledLine, cols)
	c := 0
	for _, blk := range blocks {
		// Move on once this column has met its share, but never leave a column
		// empty and never spill past the last one.
		if c < cols-1 && len(columns[c]) > 0 && len(columns[c])+len(blk) > target {
			c++
		}
		if len(columns[c]) > 0 {
			columns[c] = append(columns[c], styledLine{})
		}
		columns[c] = append(columns[c], blk...)
	}

	total = 0
	for _, col := range columns {
		total = maxInt(total, len(col))
	}
	top = clampInt(top, 0, maxInt(0, total-h))

	out := make([]string, h)
	for i := 0; i < h; i++ {
		row := top + i
		var b strings.Builder
		width := 0
		for ci, col := range columns {
			cell := styledLine{}
			if row < len(col) {
				cell = col[row]
			}
			if ci > 0 {
				b.WriteString(strings.Repeat(" ", gutter))
				width += gutter
			}
			b.WriteString(cell.text)
			width += cell.width
			// Pad every cell but the last: trailing space on the final column
			// would push the pane's right border out.
			if ci < len(columns)-1 {
				pad := maxInt(0, blockW-cell.width)
				b.WriteString(strings.Repeat(" ", pad))
				width += pad
			}
		}
		line := strings.TrimRight(b.String(), " ")
		if width > w {
			// One binding described at more length than the terminal is wide.
			// Clipping is the honest failure here: the panel is drawn inside a
			// border that was sized before this ran, and a line that overruns it
			// takes the frame apart.
			line = lipgloss.NewStyle().MaxWidth(w).Render(line)
		}
		out[i] = line
	}
	return strings.Join(out, "\n"), total
}

// styledLine is a rendered line plus the display width it would have without
// its escape sequences — measuring the styled string is unreliable, and every
// caller needs the width to pad columns.
type styledLine struct {
	text  string
	width int
}

// renderGroup lays one group out as a title over its rows, with the key column
// aligned within the group. Alignment is per-group rather than global so a
// single wide binding ("pgup/pgdn") doesn't indent every description on screen.
func renderGroup(th *theme.Theme, g Group) []styledLine {
	if len(g.Rows) == 0 {
		return nil
	}
	keyW := 0
	for _, r := range g.Rows {
		keyW = maxInt(keyW, runewidth.StringWidth(r.Keys))
	}

	out := make([]styledLine, 0, len(g.Rows)+1)
	if g.Title != "" {
		out = append(out, styledLine{
			text:  th.Section.Render(g.Title),
			width: runewidth.StringWidth(g.Title),
		})
	}
	for _, r := range g.Rows {
		pad := strings.Repeat(" ", keyW-runewidth.StringWidth(r.Keys))
		out = append(out, styledLine{
			text:  th.Norm.Render(r.Keys) + pad + "  " + th.Dim.Render(r.Desc),
			width: keyW + 2 + runewidth.StringWidth(r.Desc),
		})
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
