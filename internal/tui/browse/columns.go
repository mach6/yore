package browse

// The results table's columns, described once. What a column is called, how wide
// it is, how it draws a cell and how it orders two rows all live in one entry of
// tableSpecs — the layout, the header, the row renderer and the columns pane are
// loops over it. They used to be three hand-kept lists that had to agree about
// the set and the order, which is the kind of agreement that lasts until someone
// adds a column.

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// tableCol identifies one column. The constants are in render order and index
// tableSpecs, so a column's identity is a small integer the Model can hold by
// value — which matters, because the Model is copied on every update.
type tableCol int

const (
	colTime tableCol = iota
	colHost
	colExit
	colDur
	colExec
	colTags
	colCmd
	tableColCount
)

// colSpec is everything the table knows about one column.
type colSpec struct {
	title string // the header cell, and the name in the columns pane
	width int    // fixed; the command column is 0 and takes what is left
	right bool   // right-aligned: the time and duration cells read as quantities
	fixed bool   // cannot be hidden — the table without it is not a history

	// cell renders the column's value for one record.
	cell func(th *theme.Theme, r rec.Record, now int64) (string, lipgloss.Style)

	// less orders two records by this column, ascending.
	less func(a, b rec.Record) bool
}

var tableSpecs = [tableColCount]colSpec{
	colTime: {
		title: "time", width: 8, right: true,
		cell: func(th *theme.Theme, r rec.Record, now int64) (string, lipgloss.Style) {
			return theme.RelTime(now, r.StartMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return a.StartMs < b.StartMs },
	},
	colHost: {
		title: "host", width: 14,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return r.Hostname, th.Host(r.Hostname)
		},
		less: func(a, b rec.Record) bool { return a.Hostname < b.Hostname },
	},
	colExit: {
		title: "exit", width: 4,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return exitMarker(th, r)
		},
		// Unknown sorts below every known status: a row still running has no
		// outcome, and pretending it is a 0 would file it with the successes.
		less: func(a, b rec.Record) bool { return exitRank(a) < exitRank(b) },
	},
	colDur: {
		title: "dur", width: 7, right: true,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			if r.DurMs == nil {
				return "", th.Dim
			}
			return theme.Duration(*r.DurMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return durOr(a, -1) < durOr(b, -1) },
	},
	colExec: {
		title: "exec", width: 12,
		// Metadata, so Dim — the same weight as the host and duration cells, and
		// deliberately not the accent the user's own tags get. Blank for a command
		// the user typed.
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return r.Executor, th.Dim
		},
		less: func(a, b rec.Record) bool { return a.Executor < b.Executor },
	},
	colTags: {
		title: "tags", width: 12,
		// The user's own labels, in the accent: they are the one thing in the row
		// that is there because somebody put it there.
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return strings.Join(r.Tags, ","), th.Accent
		},
		less: func(a, b rec.Record) bool { return strings.Join(a.Tags, ",") < strings.Join(b.Tags, ",") },
	},
	colCmd: {
		title: "command", fixed: true,
		less: func(a, b rec.Record) bool { return a.Cmd < b.Cmd },
	},
}

// exitRank orders exit status: unknown below everything, then 0, then failures by
// code. Sorting on the raw code would file a still-running command with the
// successes, which is the one thing this column exists to keep apart.
func exitRank(r rec.Record) int {
	if r.Exit == nil {
		return -1
	}
	return *r.Exit
}

func durOr(r rec.Record, fallback int64) int64 {
	if r.DurMs == nil {
		return fallback
	}
	return *r.DurMs
}

// colShedOrder is the order columns give up their width when the terminal is too
// narrow for all of them, least-missed first. Executor goes before tags because
// it also appears in the details pane, the agent explorer and stats, whereas a
// user tag is shown nowhere else in the table. keep is the room the command
// column must still have for the drop to be unnecessary.
var colShedOrder = []struct {
	id   tableCol
	keep int
}{
	{colExec, 5}, {colTags, 5}, {colDur, 5}, {colHost, 5}, {colExit, 5}, {colTime, 3},
}

// colLayout is every column's resolved width for this frame. 0 means the column
// is not on screen — switched off, empty for these rows, or shed to keep the
// command readable.
type colLayout struct{ w [tableColCount]int }

func (l colLayout) shows(c tableCol) bool { return l.w[c] > 0 }

// prefix is the width everything left of the command column occupies, each cell
// plus its one-column separator.
func (l colLayout) prefix() int {
	p := 0
	for c := range tableColCount {
		if c != colCmd && l.w[c] > 0 {
			p += l.w[c] + 1
		}
	}
	return p
}

// colVisible reports whether a column has something to say for the rows on
// screen and has not been switched off in the columns pane. Width shedding is
// applied after this, in colLayout.
//
// The data gates are one-way: they can hide a column that would be blank on every
// row, but the pane cannot force one back on. A column of empty cells costs width
// and tells you nothing, and "no row here has an executor" is already the answer.
func (m Model) colVisible(c tableCol) bool {
	if m.colHidden[c] {
		return false
	}
	switch c {
	case colHost:
		// Scope-all only: with one host selected every cell would repeat its name.
		return m.hosts[m.hostSel].scope == proto.ScopeAll
	case colExec:
		return m.hasExec
	case colTags:
		return m.hasTags
	}
	return true
}

// colLayout distributes the table's content width across the visible columns,
// shedding the least-missed ones until the command fits, so a row never overflows
// and the command always keeps room.
func (m Model) colLayout() colLayout {
	var l colLayout
	for c := range tableColCount {
		if c != colCmd && m.colVisible(c) {
			l.w[c] = tableSpecs[c].width
		}
	}
	for _, s := range colShedOrder {
		if l.prefix()+s.keep <= m.tableWidth {
			break
		}
		l.w[s.id] = 0
	}
	l.w[colCmd] = maxInt(0, m.tableWidth-l.prefix())
	return l
}

// --- sorting -------------------------------------------------------------

// sortArrow marks the sorted column's header and names the direction in the
// status bar. A glyph, not a color: the table's other distinctions are all
// legible without seeing hue, and this one has no reason to be the exception.
func sortArrow(desc bool) string {
	if desc {
		return "▼"
	}
	return "▲"
}

// defaultSort is the order the table opens in: newest first, what it has always
// done.
var defaultSort = struct {
	col  tableCol
	desc bool
}{colTime, true}

// sortedByDefault reports whether the table is in its opening order, which is the
// one order the status bar does not bother naming.
func (m Model) sortedByDefault() bool {
	return m.sortCol == defaultSort.col && m.sortDesc == defaultSort.desc
}

// hiddenColCount is how many columns the user has switched off. The status bar
// says so: a column missing because it was turned off looks exactly like one
// missing because the terminal is narrow, and only one of those is worth
// explaining.
func (m Model) hiddenColCount() int {
	n := 0
	for c := range tableColCount {
		if m.colHidden[c] {
			n++
		}
	}
	return n
}

// sortRows orders the visible rows by the chosen column. The slice arrives
// newest-first and the sort is stable, so recency stays the tiebreak within equal
// durations or equal exit statuses — the order the table has always had, kept
// underneath whatever the user asked for on top. On the default sort that makes
// this a no-op over already-ordered rows.
func (m *Model) sortRows() {
	less := tableSpecs[m.sortCol].less
	if less == nil {
		return
	}
	sort.SliceStable(m.rows, func(i, j int) bool {
		if m.sortDesc {
			return less(m.rows[j], m.rows[i])
		}
		return less(m.rows[i], m.rows[j])
	})
}

// sortBy points the table at a column, or reverses the direction when it is
// already the sorted one — so the key that chooses a column is also the key that
// flips it, and there is no second binding to remember.
func (m Model) sortBy(c tableCol) (Model, bool) {
	if tableSpecs[c].less == nil {
		return m, false
	}
	if m.sortCol == c {
		m.sortDesc = !m.sortDesc
	} else {
		// Time reads newest-first and everything else smallest-first; those are the
		// directions each column is usually wanted in.
		m.sortCol, m.sortDesc = c, c == colTime
	}
	m.resortKeepingCursor()
	return m, true
}

// resortKeepingCursor re-orders the rows and keeps the cursor on the record it
// was on. Re-sorting moves a row's index but not the user's place in the history.
func (m *Model) resortKeepingCursor() {
	var selID string
	if m.sel >= 0 && m.sel < len(m.rows) {
		selID = m.rows[m.sel].ID
	}
	m.sortRows()
	if selID != "" {
		for i, r := range m.rows {
			if r.ID == selID {
				m.sel = i
				break
			}
		}
	}
	m.clampWindow()
	m.syncDetail()
}

// --- the columns pane ----------------------------------------------------

// toggleColumns raises or drops the columns pane (the c key). The cursor stays
// where it was left: switching several columns off is a handful of trips through
// this pane, and resetting it each time would make the second trip longer than
// the first.
func (m Model) toggleColumns() (tea.Model, tea.Cmd) {
	m.showCols = !m.showCols
	return m, nil
}

// handleColumnsKey services the columns pane. It owns its keys outright while it
// is up, like the key panel: it covers the table it is describing, so the table's
// own keys must not fire through it.
func (m Model) handleColumnsKey(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc", "c", "q":
		m.showCols = false
		return m, nil
	case "?":
		// This pane runs before the global keys, so ? has to be handled here too —
		// otherwise the pane with the least guessable keys is the one pane that
		// cannot show you what they are.
		return m.openHelp()
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		m.colSel = clampIndex(m.colSel-1, int(tableColCount))
		return m, nil
	case "down", "j":
		m.colSel = clampIndex(m.colSel+1, int(tableColCount))
		return m, nil
	case "g", "home":
		m.colSel = 0
		return m, nil
	case "G", "end":
		m.colSel = int(tableColCount) - 1
		return m, nil
	case " ", "x":
		return m.toggleColumn(tableCol(m.colSel))
	case "s", "enter":
		c := tableCol(m.colSel)
		mm, ok := m.sortBy(c)
		if !ok {
			return m.flashOnly(tableSpecs[c].title + " cannot be sorted on")
		}
		return mm.flashOnly("sort: " + tableSpecs[c].title + " " + sortArrow(mm.sortDesc))
	}
	return m, nil
}

// toggleColumn switches one column off or back on. The command column stays: a
// table of metadata with no commands in it is not a shell history.
func (m Model) toggleColumn(c tableCol) (tea.Model, tea.Cmd) {
	if tableSpecs[c].fixed {
		return m.flashOnly(tableSpecs[c].title + " cannot be hidden")
	}
	m.colHidden[c] = !m.colHidden[c]
	m.applyLayout() // the command column just gained or lost that width
	m.syncDetail()
	if m.colHidden[c] {
		return m.flashOnly(tableSpecs[c].title + " hidden")
	}
	return m.flashOnly(tableSpecs[c].title + " shown")
}

// colStateNote says why a column is not on screen when the answer is not "you
// switched it off" — a gate the rows themselves closed, or a width the terminal
// does not have. Without it a column can read as switched off when it is not, and
// pressing space on it would appear to do nothing.
func (m Model) colStateNote(c tableCol, l colLayout) string {
	switch {
	case m.colHidden[c]:
		return ""
	case !m.colVisible(c):
		switch c {
		case colHost:
			return "one host in scope"
		case colExec:
			return "no agent commands here"
		case colTags:
			return "nothing tagged here"
		}
		return "nothing to show"
	case !l.shows(c):
		return "no room at this width"
	}
	return ""
}

// renderColumns draws the columns pane: every column, whether it is on, and which
// one the table is ordered by. It takes over the middle of the frame like the key
// panel, keeping the header and status bar so the view it belongs to is still
// named.
func (m Model) renderColumns(w, h int) string {
	th := m.th
	bw, bh := maxInt(1, w-2), maxInt(1, h-2)
	l := m.colLayout()

	lines := make([]string, 0, tableColCount+1)
	for c := range tableColCount {
		spec := tableSpecs[c]
		box := "[x]"
		switch {
		case spec.fixed:
			box = " · " // not a choice, so not drawn as one
		case m.colHidden[c]:
			box = "[ ]"
		}
		sorted := "  "
		if m.sortCol == c {
			sorted = sortArrow(m.sortDesc) + " "
		}
		text := box + " " + sorted + padRight(truncCols(spec.title, 10), 10)
		if note := m.colStateNote(c, l); note != "" {
			text += "  " + note
		}
		cursor, style := "  ", th.Norm
		if int(c) == m.colSel {
			cursor, style = th.Accent.Render("❯ "), th.Sel
		}
		lines = append(lines, padTo(cursor+style.Render(fitPlain(text, maxInt(1, bw-2))), bw))
	}
	return m.titledBox(true, bw, bh, "COLUMNS", "browse table", padLines(lines, bw, bh))
}
