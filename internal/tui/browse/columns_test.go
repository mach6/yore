package browse

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
// starts in has to be the order the table always had, and the status bar says
// nothing about it, because there is nothing yet to explain.
func TestTableOpensNewestFirst(t *testing.T) {
	m := sortedTable(t)
	require.Equal(t, colTime, m.cols[ctBrowse].sortCol)
	require.True(t, m.cols[ctBrowse].sortDesc)
	require.True(t, m.sortedByDefault(ctBrowse))
	require.Equal(t, []string{"zebra build", "apple test", "mango lint"}, cmds(m),
		"the rows arrive newest-first and stay that way")
	require.NotContains(t, strip(m.View()), "sort:", "the default order needs no chip")
}

// TestSortByColumnReorders: sorting walks each column's own key, ascending first
// for everything but time, which reads newest-first.
func TestSortByColumnReorders(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  int
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
			m, ok := m.sortBy(ctBrowse, tc.col)
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
	m, _ = m.sortBy(ctBrowse, colHost)
	require.False(t, m.cols[ctBrowse].sortDesc, "everything but time starts ascending")
	require.Equal(t, "apple test", m.rows[0].Cmd)

	m, _ = m.sortBy(ctBrowse, colHost)
	require.True(t, m.cols[ctBrowse].sortDesc)
	require.Equal(t, "zebra build", m.rows[0].Cmd, "the same column again reverses it")

	// Time is the exception: it reads newest-first when chosen.
	m, _ = m.sortBy(ctBrowse, colTime)
	require.True(t, m.cols[ctBrowse].sortDesc)
	m, _ = m.sortBy(ctBrowse, colTime)
	require.False(t, m.cols[ctBrowse].sortDesc)
	require.Equal(t, "mango lint", m.rows[0].Cmd, "oldest first")
}

// TestSortKeepsTheCursorOnItsRecord: re-sorting moves a row's index but not the
// user's place in their own history.
func TestSortKeepsTheCursorOnItsRecord(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("down")) // onto "apple test"
	require.Equal(t, "apple test", m.rows[m.sel].Cmd)

	m, _ = m.sortBy(ctBrowse, colCmd)
	require.Equal(t, "apple test", m.rows[m.sel].Cmd, "the cursor followed its record")
	require.Zero(t, m.sel, "which is now the first row")
}

// TestSortSurvivesARefresh: a background refetch delivers the same rows again, so
// the order the user chose has to be reapplied rather than reverting to recency.
func TestSortSurvivesARefresh(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(ctBrowse, colCmd)
	require.Equal(t, "apple test", m.rows[0].Cmd)

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(sortFixture())})
	require.Equal(t, "apple test", m.rows[0].Cmd, "the chosen order outlives a refetch")

	// And a period change, which re-derives m.rows from the held sample.
	m, _ = step(t, m, press("2"))
	require.Equal(t, "apple test", m.rows[0].Cmd)
}

// TestSortIsNamedOnScreen: the arrow rides the header where the cell is wide
// enough, and the status bar names the sort in words either way; a four-column
// header has no room for a glyph, and color alone is not a distinction.
func TestSortIsNamedOnScreen(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(ctBrowse, colHost)
	require.Contains(t, tableHeaderLine(m), "host▲", "the wide header carries the arrow")
	require.Contains(t, strip(m.View()), "sort: host ▲", "and the status bar says it in words")

	m, _ = m.sortBy(ctBrowse, colExit)
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
	for m.colSel != colHost {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press(" "))
	require.True(t, m.cols[ctBrowse].hidden[colHost])
	require.Contains(t, strip(m.View()), "[ ] ", "and off reads as off")

	m, _ = step(t, m, press("esc"))
	require.False(t, m.showCols)
	require.NotContains(t, tableHeaderLine(m), "host", "the hidden column is off the table")
	require.Greater(t, m.colLayout().w[colCmd], cmdW, "its width went to the command")
	require.Contains(t, strip(m.View()), "1 column hidden",
		"a column off by choice looks like one off for width unless it says so")

	// And back on again: the pane reopens where it was left, so the same key
	// undoes it without navigating there a second time.
	m, _ = step(t, m, press("c"))
	require.Equal(t, colHost, m.colSel, "the cursor stayed where it was left")
	m, _ = step(t, m, press(" "))
	require.False(t, m.cols[ctBrowse].hidden[colHost])
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
	require.Equal(t, colCmd, m.colSel, "the command column is last")

	m, _ = step(t, m, press(" "))
	require.False(t, m.cols[ctBrowse].hidden[colCmd])
	require.Contains(t, strip(m.View()), "cannot be hidden")
	require.Positive(t, m.colLayout().w[colCmd])
}

// TestColumnsPaneSortsFromTheCursor: the pane is where both column questions get
// answered, so it sorts as well as hides.
func TestColumnsPaneSortsFromTheCursor(t *testing.T) {
	m := sortedTable(t)
	m, _ = step(t, m, press("c"))
	for m.colSel != colCmd {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	require.Equal(t, colCmd, m.cols[ctBrowse].sortCol)
	require.Equal(t, "apple test", m.rows[0].Cmd, "the table reordered behind the pane")
	require.Contains(t, strip(m.View()), "▲ ", "the pane marks the sorted column")

	m, _ = step(t, m, press("s"))
	require.True(t, m.cols[ctBrowse].sortDesc, "s again reverses, as it does from the table")
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
	for m.colSel != colTags {
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
// a filter; the tag filter still works with the tags column off.
func TestHiddenColumnDoesNotStopFiltering(t *testing.T) {
	m := sortedTable(t)
	require.Contains(t, tableHeaderLine(m), "tags")

	m, _ = step(t, m, press("c"))
	for m.colSel != colTags {
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

// --- the explorer's two lists --------------------------------------------

// promptHeaderOf is the PROMPTS pane's header row on its own.
func promptHeaderOf(m Model) string {
	return strip(composeSegs(m.tableHeaderSegs(
		m.tableLayout(ctPrompts, maxInt(1, m.geo.p[apPrompts].w-2))), false, 200, m.th))
}

// cmdHeaderOf is the COMMANDS pane's header row on its own.
func cmdHeaderOf(m Model) string {
	return strip(composeSegs(m.tableHeaderSegs(
		m.tableLayout(ctCommands, maxInt(1, m.geo.p[apCommands].w-2))), false, 200, m.th))
}

// TestColumnsPaneFollowsTheFocusedList: c reshapes the list you are looking at.
// The sidebar and the details pane have no columns of their own, so they aim at
// the prompt list the view hangs off: the same rule / already uses.
func TestColumnsPaneFollowsTheFocusedList(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, apPrompts, m.apane)

	m, _ = step(t, m, press("c"))
	require.Equal(t, ctPrompts, m.colTable)
	require.Contains(t, strip(m.View()), "prompt list", "the pane names the table it is aimed at")
	m, _ = step(t, m, press("esc"))

	m, _ = step(t, m, press("tab")) // -> commands
	m, _ = step(t, m, press("c"))
	require.Equal(t, ctCommands, m.colTable)
	require.Contains(t, strip(m.View()), "command list")
	m, _ = step(t, m, press("esc"))

	// The sidebar has no columns, so it borrows the prompt list's.
	for m.apane != apAgents {
		m, _ = step(t, m, press("tab"))
	}
	m, _ = step(t, m, press("c"))
	require.Equal(t, ctPrompts, m.colTable)
	m, _ = step(t, m, press("esc"))

	// And the browse view still aims at its own table.
	m, _ = step(t, m, press("esc")) // leave the explorer
	require.Equal(t, viewBrowse, m.view)
	m, _ = step(t, m, press("c"))
	require.Equal(t, ctBrowse, m.colTable)
	require.Contains(t, strip(m.View()), "browse table")
}

// TestPromptListHidesAndSorts: the prompt list reshapes like the browse table,
// and keeps its own choices; hiding a column here says nothing about there.
func TestPromptListHidesAndSorts(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Contains(t, promptHeaderOf(m), "session")

	m, _ = step(t, m, press("c"))
	for m.colSel != pcSess {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("esc"))
	require.NotContains(t, promptHeaderOf(m), "session", "the hidden column is off the list")
	require.Contains(t, strip(m.View()), "1 col hidden", "the pane title admits it")
	require.False(t, m.cols[ctBrowse].hidden[colHost], "the browse table is untouched")

	// Sorting by cmds reorders the list: p1 ran two commands, p2 one.
	require.Equal(t, "add rate limiting", m.filteredPrompts[0].text, "most recent first by default")
	m, _ = step(t, m, press("c"))
	for m.colSel != pcCmds {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	require.Equal(t, pcCmds, m.cols[ctPrompts].sortCol)
	require.False(t, m.cols[ctPrompts].sortDesc, "ascending first")
	require.Equal(t, 1, m.filteredPrompts[0].count, "fewest commands first")
	m, _ = step(t, m, press("s"))
	require.Equal(t, 2, m.filteredPrompts[0].count, "s again reverses")
	require.Contains(t, strip(m.View()), "sort: cmds")
}

// TestPromptSortDoesNotReorderTheAggregate: the unfiltered list IS
// prompts.prompts, so sorting it in place would reorder the aggregate every other
// pane reads from.
func TestPromptSortDoesNotReorderTheAggregate(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Empty(t, m.promptQ, "the unfiltered path is the aliased one")
	before := make([]string, len(m.prompts.prompts))
	for i, p := range m.prompts.prompts {
		before[i] = p.id
	}

	// cmds ascending puts the one-command prompt first, which recency did not.
	m, _ = m.sortBy(ctPrompts, pcCmds)
	after := make([]string, len(m.prompts.prompts))
	for i, p := range m.prompts.prompts {
		after[i] = p.id
	}
	require.Equal(t, before, after, "the aggregate kept its own order")
	require.NotEqual(t, before[0], m.filteredPrompts[0].id, "only the view reordered")
}

// TestCommandListSortsAndKeepsItsDefault: the command list opens oldest-first
// because that is the agent's working order, and sorting it copies rather than
// reordering the prompt's own command slice.
func TestCommandListSortsAndKeepsItsDefault(t *testing.T) {
	m := agentModel(t, 140, 40)
	m, _ = step(t, m, press("tab")) // -> commands
	require.Equal(t, dcWhen, m.cols[ctCommands].sortCol)
	require.False(t, m.cols[ctCommands].sortDesc, "oldest first")
	require.Equal(t, "cargo build", m.visibleCmds()[0].Cmd)

	p, ok := m.drilledPrompt()
	require.True(t, ok)
	held := append([]rec.Record(nil), p.cmds...)

	m, _ = m.sortBy(ctCommands, dcCmd)
	require.Equal(t, "cargo add tower", m.visibleCmds()[0].Cmd, "alphabetical now")

	p2, _ := m.drilledPrompt()
	require.Equal(t, held[0].Cmd, p2.cmds[0].Cmd, "the prompt's own command order is untouched")

	// The pane title says the order is no longer the natural one.
	require.Contains(t, strip(m.View()), "sort: command")
}

// TestCommandListDurGate: the dur column is gated on some agent reporting timing,
// and the pane says so rather than looking switched off.
func TestCommandListDurGate(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.False(t, m.prompts.hasDur, "the sample carries no durations")
	require.NotContains(t, cmdHeaderOf(m), "dur")

	m, _ = step(t, m, press("tab")) // -> commands
	m, _ = step(t, m, press("c"))
	for m.colSel != dcDur {
		m, _ = step(t, m, press("down"))
	}
	require.Contains(t, strip(m.View()), "no agent reports timing")
}

// TestEachTableKeepsItsOwnSort: three tables, three independent orders; sorting
// the prompt list must not reorder the browse table underneath it.
func TestEachTableKeepsItsOwnSort(t *testing.T) {
	m := sortedTable(t)
	m, _ = m.sortBy(ctBrowse, colCmd)
	require.Equal(t, "apple test", m.rows[0].Cmd)

	m, _ = m.sortBy(ctPrompts, pcCmds)
	require.Equal(t, colCmd, m.cols[ctBrowse].sortCol, "the browse table's order stands")
	require.Equal(t, "apple test", m.rows[0].Cmd)
	require.Equal(t, pcCmds, m.cols[ctPrompts].sortCol)
	require.Equal(t, dcWhen, m.cols[ctCommands].sortCol, "and the command list's default stands")
}

// --- persistence ---------------------------------------------------------

// TestColumnChoicesPersistAndRestore: hiding a column and choosing a sort are
// written to ui.toml as they happen, by name, and come back on the next run.
func TestColumnChoicesPersistAndRestore(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}, {Hostname: "boxB", Count: 1}}},
		resp:  mkResp(sortFixture()),
	}
	var saved []Prefs
	m := NewModel(f, Options{
		Version:   "v1",
		Now:       now,
		SavePrefs: func(p Prefs) error { saved = append(saved, p); return nil },
	})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})
	m, _ = step(t, m, hostsResultMsg{info: f.hosts})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	// Hide the host column.
	m, _ = step(t, m, press("c"))
	for m.colSel != colHost {
		m, _ = step(t, m, press("down"))
	}
	m, cmd := step(t, m, press(" "))
	require.NotNil(t, cmd, "hiding a column should persist it")
	runBatch(cmd)
	require.NotEmpty(t, saved)
	require.Equal(t, []string{"host"}, saved[len(saved)-1].Columns["browse"].Hidden,
		"columns are remembered by name")

	// Sort by dur, descending.
	for m.colSel != colDur {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	m, cmd = step(t, m, press("s"))
	runBatch(cmd)
	last := saved[len(saved)-1]
	require.Equal(t, "dur", last.Columns["browse"].Sort)
	require.True(t, last.Columns["browse"].SortDesc)

	// A fresh model restores both.
	m2 := NewModel(f, Options{Version: "v1", Now: now, Prefs: last})
	m2, _ = step(t, m2, tea.WindowSizeMsg{Width: 140, Height: 30})
	m2, _ = step(t, m2, hostsResultMsg{info: f.hosts})
	m2, _ = step(t, m2, queryResultMsg{seq: 1, resp: f.resp})
	require.True(t, m2.cols[ctBrowse].hidden[colHost], "the hidden column came back hidden")
	require.Equal(t, colDur, m2.cols[ctBrowse].sortCol)
	require.True(t, m2.cols[ctBrowse].sortDesc)
	require.NotContains(t, tableHeaderLine(m2), "host")
	require.Equal(t, "zebra build", m2.rows[0].Cmd, "and the rows arrive in that order")
}

// TestEveryTablesChoicesPersist: all three tables are remembered, each under its
// own key, so reshaping the explorer's lists outlives the session too.
func TestEveryTablesChoicesPersist(t *testing.T) {
	states := defaultColStates()
	states[ctBrowse].hidden[colTags] = true
	states[ctBrowse].sortCol, states[ctBrowse].sortDesc = colExec, false
	states[ctPrompts].hidden[pcSess] = true
	states[ctPrompts].sortCol, states[ctPrompts].sortDesc = pcCmds, true
	states[ctCommands].sortCol, states[ctCommands].sortDesc = dcDur, true

	prefs := colPrefsOf(states)
	require.Equal(t, []string{"tags"}, prefs["browse"].Hidden)
	require.Equal(t, "exec", prefs["browse"].Sort)
	require.Equal(t, []string{"session"}, prefs["prompts"].Hidden)
	require.Equal(t, "cmds", prefs["prompts"].Sort)
	require.Equal(t, "dur", prefs["commands"].Sort)

	require.Equal(t, states, colStatesFrom(prefs), "the round trip is lossless")
}

// TestUnchangedTablesAreNotWrittenDown: a table left alone contributes nothing, so
// ui.toml stays quiet about it and inherits whatever this build's default becomes.
func TestUnchangedTablesAreNotWrittenDown(t *testing.T) {
	require.Empty(t, colPrefsOf(defaultColStates()), "defaults are not choices")

	states := defaultColStates()
	states[ctPrompts].sortCol = pcCmds
	prefs := colPrefsOf(states)
	require.Len(t, prefs, 1, "only the table that changed is written")
	require.Contains(t, prefs, "prompts")
}

// TestStaleColumnPrefsAreIgnored: ui.toml outlives releases, so a name this build
// does not have, an unsortable column, and a hidden entry for a column that must
// always show are all dropped rather than breaking a table.
func TestStaleColumnPrefsAreIgnored(t *testing.T) {
	got := colStatesFrom(map[string]ColumnPrefs{
		"browse":      {Hidden: []string{"gone", "command"}, Sort: "vanished"},
		"nosuchtable": {Sort: "time"},
		"prompts":     {Sort: "prompt", SortDesc: true},
	})
	require.Equal(t, defaultColStates()[ctBrowse], got[ctBrowse],
		"unknown names and an unhidable column leave the table at its defaults")
	require.Equal(t, pcText, got[ctPrompts].sortCol, "a real column still applies")
}

// TestSavingAfterADragKeepsTheColumns: ui.toml is rewritten whole, so the drag
// path and the column path must write the same struct; otherwise moving a seam
// would silently erase every column choice.
func TestSavingAfterADragKeepsTheColumns(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	var saved []Prefs
	m := NewModel(f, Options{
		Version:   "v1",
		Now:       now,
		SavePrefs: func(p Prefs) error { saved = append(saved, p); return nil },
	})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	m, _ = step(t, m, press("c"))
	for m.colSel != colExit {
		m, _ = step(t, m, press("down"))
	}
	m, cmd := step(t, m, press(" "))
	runBatch(cmd)
	m, _ = step(t, m, press("esc"))
	require.Equal(t, []string{"exit"}, saved[len(saved)-1].Columns["browse"].Hidden)

	// Now drag a seam. The write that follows must still carry the column.
	m, _ = step(t, m, click(m.geo.vDiv, 5))
	m, _ = step(t, m, dragTo(40, 5))
	_, cmd = step(t, m, mouseUp(40))
	require.NotNil(t, cmd)
	cmd()
	last := saved[len(saved)-1]
	require.Equal(t, ratioOf(40, 120), last.Splits.BrowseLeft, "the drag was written")
	require.Equal(t, []string{"exit"}, last.Columns["browse"].Hidden,
		"and it did not erase the column choice")
}

// runBatch runs a tea.Cmd that may be a batch, so the writes inside it happen.
func runBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				c()
			}
		}
	}
}

// TestColumnHeaderMatchesTheRow: the header and the cells are one loop over one
// table now, so a column can no longer be in the header and missing from the
// row, and both still fill the table exactly, at every width the columns shed
// at.
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
		for c := range colTables[ctBrowse].metas {
			// A cell narrower than its own name is truncated, which only the command
			// column ever is: it takes what is left, down to five columns.
			if l.shows(c) && l.w[c] >= len(colTables[ctBrowse].metas[c].title) {
				require.Containsf(t, header, colTables[ctBrowse].metas[c].title,
					"w=%d: %s is laid out but not in the header", w, colTables[ctBrowse].metas[c].title)
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

// --- the devices view's two lists ----------------------------------------

// devPaneLines returns the rendered lines of one devices pane, stripped.
func devPaneLines(m Model, p devPane) []string {
	w, h := 116, 12
	inner := m.deviceListInner(w, h)
	if p == dpTokens {
		inner = m.tokenListInner(w, h)
	}
	return strings.Split(strip(inner), "\n")
}

// TestDevicesListsAreTables: both panes of the devices view carry a column
// header over their rows, and c aims at whichever one has focus.
func TestDevicesListsAreTables(t *testing.T) {
	m, _ := devicesFixture(t)

	machines := devPaneLines(m, dpDevices)
	require.Contains(t, machines[0], "status", "the machine list has a header")
	require.Contains(t, machines[0], "machine")
	tokens := devPaneLines(m, dpTokens)
	require.Contains(t, tokens[0], "state", "the token list has a header")
	require.Contains(t, tokens[0], "minted")
	require.Contains(t, tokens[0], "expires")

	m, _ = step(t, m, press("c"))
	require.Equal(t, ctDevices, m.colTable, "c aims at the focused pane's list")
	require.Contains(t, strip(m.View()), "machine list")
	m, _ = step(t, m, press("esc"))

	m, _ = step(t, m, press("tab"))
	m, _ = step(t, m, press("c"))
	require.Equal(t, ctTokens, m.colTable)
	require.Contains(t, strip(m.View()), "token list")
}

// TestTokenListSortsAndHides: the token list reshapes like every other table,
// and says on its own pane title that it has been reshaped; the devices view
// has no status bar to say it anywhere else.
func TestTokenListSortsAndHides(t *testing.T) {
	m, _ := devicesFixture(t)
	require.Equal(t, "aaaa111122223333", m.tokens[0].ID, "newest first is the opening order")

	m, _ = step(t, m, press("tab")) // focus the tokens pane
	m, _ = step(t, m, press("c"))
	for m.colSel != tcID {
		m, _ = step(t, m, press("down"))
	}
	m, cmd := step(t, m, press("s"))
	require.NotNil(t, cmd, "sorting should flash and persist")
	runBatch(cmd)
	require.Equal(t, "aaaa111122223333", m.tokens[0].ID, "id ascending starts at aaaa…")
	m, _ = step(t, m, press("s"))
	require.Equal(t, "cccc777788889999", m.tokens[0].ID, "s again reverses it")

	// Hiding a column drops it from the header, and the pane title says so.
	for m.colSel != tcMinted {
		m, _ = step(t, m, press("up"))
	}
	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("esc"))
	require.NotContains(t, devPaneLines(m, dpTokens)[0], "minted")
	out := strip(m.View())
	require.Containsf(t, out, "1 col hidden", "the pane title reports the hidden column:\n%s", out)
	require.Containsf(t, out, "sort: token", "and the order it is in:\n%s", out)
}

// TestMachineListSortsByWhatNeedsYou: the machine list opens with the machine
// waiting for approval on top, because that is the only row asking for anything.
func TestMachineListSortsByWhatNeedsYou(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		devices: proto.DevicesInfo{Devices: []proto.DeviceInfo{
			{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
			{ID: "01BBBBBBBBBBBBBBBBBBBBBBBB", Name: "old-box", Status: "revoked"},
			{ID: "01CCCCCCCCCCCCCCCCCCCCCCCC", Name: "server", Status: "pending", Code: "AB12-CD34-EF56-GH78-JK90-MN12"},
		}},
	}
	m := ready(t, f, 140, 30)
	m, cmd := step(t, m, press("D"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Equal(t, []string{"server", "laptop", "old-box"}, deviceNames(m),
		"pending, then working, then thrown out")

	// The code column appears only while a machine is waiting, and it is not
	// truncated: it exists to be compared character by character.
	require.Contains(t, devPaneLines(m, dpDevices)[1], "AB12-CD34-EF56-GH78-JK90-MN12")

	// Sorting by machine name is the user's to choose.
	m, _ = step(t, m, press("c"))
	for m.colSel != mcName {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	require.Equal(t, []string{"laptop", "old-box", "server"}, deviceNames(m))
}

func deviceNames(m Model) []string {
	out := make([]string, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d.Name)
	}
	return out
}

// TestDeviceSortSurvivesARefresh: S refetches both lists, and a refresh that
// silently restored the server's order would move rows out from under the
// cursor; including under an armed approve/revoke question.
func TestDeviceSortSurvivesARefresh(t *testing.T) {
	m, _ := devicesFixture(t)
	m, _ = step(t, m, press("tab"))
	m, _ = step(t, m, press("c"))
	for m.colSel != tcID {
		m, _ = step(t, m, press("down"))
	}
	m, _ = step(t, m, press("s"))
	m, _ = step(t, m, press("s")) // id, descending
	m, _ = step(t, m, press("esc"))
	require.Equal(t, "cccc777788889999", m.tokens[0].ID)

	m, cmd := step(t, m, press("S"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Equal(t, "cccc777788889999", m.tokens[0].ID, "the refresh kept the chosen order")
}

// TestDevicesChoicesPersist: both new tables are remembered under their own
// keys, like the other three.
func TestDevicesChoicesPersist(t *testing.T) {
	states := defaultColStates()
	states[ctDevices].hidden[mcID] = true
	states[ctTokens].sortCol, states[ctTokens].sortDesc = tcExpires, false

	prefs := colPrefsOf(states)
	require.Equal(t, []string{"id"}, prefs["machines"].Hidden)
	require.Equal(t, "expires", prefs["tokens"].Sort)
	require.Equal(t, states, colStatesFrom(prefs), "the round trip is lossless")
}
