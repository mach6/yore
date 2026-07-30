package browse

import (
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
)

// tableHeaderLine is the results table's header row on its own. Asserting on the
// whole view cannot tell a column header from the same word appearing in a
// command, a status chip, or the columns pane.
func tableHeaderLine(m Model) string {
	return strip(m.tableHeader(m.colLayout(), m.tableWidth))
}

// sortFixture is three rows that disagree on every sortable column, so an
// ordering assertion can only pass for the right reason.
func sortFixture() []rec.Record {
	rows := mkRows("zebra build", "apple test", "mango lint")
	rows[0].Hostname, rows[0].Executor = "boxC", "devin"
	rows[1].Hostname, rows[1].Executor = "boxA", "claude-code"
	rows[2].Hostname, rows[2].Executor = "boxB", ""
	rows[0].DurMs, rows[1].DurMs, rows[2].DurMs = i64(300), i64(100), i64(200)
	rows[0].Exit, rows[1].Exit, rows[2].Exit = rec.IntPtr(0), rec.IntPtr(2), nil
	rows[1].Tags = []string{"refactor"}
	return rows
}

func i64(v int64) *int64 { return &v }

// sortedTable opens a browse view on sortFixture in scope-all, so every column
// including host is on screen.
func sortedTable(t *testing.T) Model {
	t.Helper()
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}, {Hostname: "boxB", Count: 1}}},
		resp:  mkResp(sortFixture()),
	}
	m := ready(t, f, 140, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	require.Equal(t, proto.ScopeAll, m.hosts[m.hostSel].scope, "the fixture wants every column on screen")
	return m
}

// cmds is the table's commands in the order they are on screen.
func cmds(m Model) []string {
	out := make([]string, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r.Cmd)
	}
	return out
}

// TestTableOpensNewestFirst: the sort is a thing you can change, so the order it
// starts in has to be the order the table always had — and the status bar says
// nothing about it, because there is nothing yet to explain.
func TestTableOpensNewestFirst(t *testing.T) {
	m := sortedTable(t)
	require.Equal(t, colTime, m.sortCol)
	require.True(t, m.sortDesc)
	require.True(t, m.sortedByDefault())
	require.Equal(t, []string{"zebra build", "apple test", "mango lint"}, cmds(m),
		"the rows arrive newest-first and stay that way")
	require.NotContains(t, strip(m.View()), "sort:", "the default order needs no chip")
}

// TestSortByColumnReorders: sorting walks each column's own key, ascending first
// for everything but time, which reads newest-first.
func TestSortByColumnReorders(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  tableCol
		want []string
	}{
		{"host", colHost, []string{"apple test", "mango lint", "zebra build"}},
		{"dur", colDur, []string{"apple test", "mango lint", "zebra build"}},
		{"command", colCmd, []string{"apple test", "mango lint", "zebra build"}},
		{"exec", colExec, []string{"mango lint", "claude-code", "devin"}},
		// Unknown status sorts below every known one: a row still running has no
		// outcome, and filing it with the successes is what this column prevents.
		{"exit", colExit, []string{"mango lint", "zebra build", "apple test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sortedTable(t)
			m, ok := m.sortBy(tc.col)
			require.True(t, ok, "every column in this table sorts")
			got := cmds(m)
			if tc.col == colExec {
				// Executor order is what is being asserted, not the commands.
				got = []string{m.rows[0].Cmd, m.rows[1].Executor, m.rows[2].Executor}
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// TestSortReversesOnTheSameColumn: the key that chooses a column is the key that
// flips it, so there is no second binding to remember.
func TestSortReversesOnTheSameColumn(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(colHost)
	require.False(t, m.sortDesc, "everything but time starts ascending")
	require.Equal(t, "apple test", m.rows[0].Cmd)

	m, _ = m.sortBy(colHost)
	require.True(t, m.sortDesc)
	require.Equal(t, "zebra build", m.rows[0].Cmd, "the same column again reverses it")

	// Time is the exception: it reads newest-first when chosen.
	m, _ = m.sortBy(colTime)
	require.True(t, m.sortDesc)
	m, _ = m.sortBy(colTime)
	require.False(t, m.sortDesc)
	require.Equal(t, "mango lint", m.rows[0].Cmd, "oldest first")
}

// TestSortKeepsTheCursorOnItsRecord: re-sorting moves a row's index but not the
// user's place in their own history.
func TestSortKeepsTheCursorOnItsRecord(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("down")) // onto "apple test"
	require.Equal(t, "apple test", m.rows[m.sel].Cmd)

	m, _ = m.sortBy(colCmd)
	require.Equal(t, "apple test", m.rows[m.sel].Cmd, "the cursor followed its record")
	require.Zero(t, m.sel, "which is now the first row")
}

// TestSortSurvivesARefresh: a background refetch delivers the same rows again, so
// the order the user chose has to be reapplied rather than reverting to recency.
func TestSortSurvivesARefresh(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(colCmd)
	require.Equal(t, "apple test", m.rows[0].Cmd)

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(sortFixture())})
	require.Equal(t, "apple test", m.rows[0].Cmd, "the chosen order outlives a refetch")

	// And a period change, which re-derives m.rows from the held sample.
	m, _ = step(t, m, press("2"))
	require.Equal(t, "apple test", m.rows[0].Cmd)
}

// TestSortIsNamedOnScreen: the arrow rides the header where the cell is wide
// enough, and the status bar names the sort in words either way — a four-column
// header has no room for a glyph, and color alone is not a distinction.
func TestSortIsNamedOnScreen(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(colHost)
	require.Contains(t, tableHeaderLine(m), "host▲", "the wide header carries the arrow")
	require.Contains(t, strip(m.View()), "sort: host ▲", "and the status bar says it in words")

	m, _ = m.sortBy(colExit)
	require.NotContains(t, tableHeaderLine(m), "exit▲", "a 4-wide cell has no room for it")
	require.Contains(t, tableHeaderLine(m), "exit", "and is not truncated to make room")
	require.Contains(t, strip(m.View()), "sort: exit ▲")
}

// TestColumnsPaneHidesAndShows: hiding a column takes it off the table and gives
// its width to the command, and the status bar admits that a column is missing
// because it was switched off rather than because the terminal is narrow.
func TestColumnsPaneHidesAndShows(t *testing.T) {
	m := sortedTable(t)
	require.Contains(t, tableHeaderLine(m), "host")
	cmdW := m.colLayout().w[colCmd]

	m, _ = step(t, m, press("c"))
	require.True(t, m.showCols)
	out := strip(m.View())
	require.Contains(t, out, "COLUMNS", "the pane is titled")
	require.Contains(t, out, "[x] ", "a column that is on reads as on")

	// Down to host, then hide it.
	for tableCol(m.colSel) != colHost {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press(" "))
	require.True(t, m.colHidden[colHost])
	require.Contains(t, strip(m.View()), "[ ] ", "and off reads as off")

	m, _ = step(t, m, press("esc"))
	require.False(t, m.showCols)
	require.NotContains(t, tableHeaderLine(m), "host", "the hidden column is off the table")
	require.Greater(t, m.colLayout().w[colCmd], cmdW, "its width went to the command")
	require.Contains(t, strip(m.View()), "1 column hidden",
		"a column off by choice looks like one off for width unless it says so")

	// And back on again — the pane reopens where it was left, so the same key
	// undoes it without navigating there a second time.
	m, _ = step(t, m, press("c"))
	require.Equal(t, colHost, tableCol(m.colSel), "the cursor stayed where it was left")
	m, _ = step(t, m, press(" "))
	require.False(t, m.colHidden[colHost])
	m, _ = step(t, m, press("esc"))
	require.Contains(t, tableHeaderLine(m), "host")
	require.NotContains(t, strip(m.View()), "column hidden")
}

// TestCommandColumnCannotBeHidden: a table of metadata with no commands in it is
// not a shell history.
func TestCommandColumnCannotBeHidden(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("c"))
	m, _ = step(t, m, press("G"))
	require.Equal(t, colCmd, tableCol(m.colSel), "the command column is last")

	m, _ = step(t, m, press(" "))
	require.False(t, m.colHidden[colCmd])
	require.Contains(t, strip(m.View()), "cannot be hidden")
	require.Positive(t, m.colLayout().w[colCmd])
}

// TestColumnsPaneSortsFromTheCursor: the pane is where both column questions get
// answered, so it sorts as well as hides.
func TestColumnsPaneSortsFromTheCursor(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("c"))
	for tableCol(m.colSel) != colCmd {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	require.Equal(t, colCmd, m.sortCol)
	require.Equal(t, "apple test", m.rows[0].Cmd, "the table reordered behind the pane")
	require.Contains(t, strip(m.View()), "▲ ", "the pane marks the sorted column")

	m, _ = step(t, m, press("s"))
	require.True(t, m.sortDesc, "s again reverses, as it does from the table")
}

// TestColumnsPaneSaysWhyAColumnIsOff: a data gate and a narrow terminal both take
// a column off the table without the user asking, and pressing space on either
// would otherwise look broken.
func TestColumnsPaneSaysWhyAColumnIsOff(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls", "vim"))}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	require.False(t, m.hasTags)

	m, _ = step(t, m, press("c"))
	out := strip(m.View())
	require.Contains(t, out, "nothing tagged here", "the tags gate names itself")
	require.Contains(t, out, "no agent commands here", "and so does the executor gate")

	// The gates are one-way: the pane cannot force a blank column back on.
	for tableCol(m.colSel) != colTags {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press(" "))
	m2, _ := step(t, m, press("esc"))
	require.NotContains(t, tableHeaderLine(m2), "tags",
		"a column with nothing in it stays off however it is toggled")
}

// TestColumnsPaneSwallowsViewKeys: it covers the table it reshapes, so the
// table's own keys must not fire through it.
func TestColumnsPaneSwallowsViewKeys(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("c"))

	m, _ = step(t, m, press("d"))
	require.False(t, m.confirmDelete, "d must not arm a delete on a row you cannot see")
	m, _ = step(t, m, press("D"))
	require.Equal(t, viewBrowse, m.view, "and no view key should fire")
	m, _ = step(t, m, press("/"))
	require.False(t, m.searching)
	require.True(t, m.showCols, "the pane is still up")

	// ? outranks it: the panel is how you find out what these keys are.
	m, _ = step(t, m, press("?"))
	require.True(t, m.showHelp)
	out := strip(m.View())
	require.Contains(t, out, "KEYS")
	require.Contains(t, out, "show or hide it", "the panel describes the pane it is over")
	require.NotContains(t, out, "put it on the prompt", "not the table underneath")
}

// TestHiddenColumnDoesNotStopFiltering: hiding a column is a display choice, not
// a filter — the tag filter still works with the tags column off.
func TestHiddenColumnDoesNotStopFiltering(t *testing.T) {
	m := sortedTable(t)
	require.Contains(t, tableHeaderLine(m), "tags")

	m, _ = step(t, m, press("c"))
	for tableCol(m.colSel) != colTags {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("esc"))
	require.NotContains(t, tableHeaderLine(m), "tags")

	m, _ = step(t, m, press("down")) // onto the tagged row
	require.Equal(t, "apple test", m.rows[m.sel].Cmd)
	m, cmd := step(t, m, press("t"))
	require.Equal(t, "refactor", m.tagFilter, "the tag is still on the record")
	require.NotNil(t, cmd)
	require.Contains(t, strip(m.View()), "tag: refactor")
}

// TestColumnHeaderMatchesTheRow: the header and the cells are one loop over one
// table now, so a column can no longer be in the header and missing from the row
// — and both still fill the table exactly, at every width the columns shed at.
func TestColumnHeaderMatchesTheRow(t *testing.T) {
	for _, w := range []int{140, 100, 76, 60, 44, 30} {
		f := &fakeBackend{
			hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}, {Hostname: "boxB", Count: 1}}},
			resp:  mkResp(sortFixture()),
		}
		m := ready(t, f, w, 30)
		m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

		l := m.colLayout()
		header := tableHeaderLine(m)
		for c := range tableColCount {
			// A cell narrower than its own name is truncated, which only the command
			// column ever is — it takes what is left, down to five columns.
			if l.shows(c) && l.w[c] >= len(tableSpecs[c].title) {
				require.Containsf(t, header, tableSpecs[c].title,
					"w=%d: %s is laid out but not in the header", w, tableSpecs[c].title)
			}
		}
		require.LessOrEqualf(t, runewidth.StringWidth(header), m.tableWidth,
			"w=%d: the header overflows the table", w)
		row := strip(m.renderRow(m.rows[0], l, match.Parse(""), false, m.tableWidth, now))
		require.LessOrEqualf(t, runewidth.StringWidth(row), m.tableWidth,
			"w=%d: a row overflows the table", w)
		require.Positivef(t, l.w[colCmd], "w=%d: the command column must always keep room", w)
	}
}
