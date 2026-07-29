package browse

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

// The explorer's sample: three prompts, each with its own commands. Ages are
// explicit (minutes ago) because the prompt list is newest-first, so which
// prompt the cursor opens on is a property the tests depend on: p1, the one
// with three commands.
func filterFixture() (rows, prompts []rec.Record) {
	specs := []struct {
		prompt, cmd string
		minsAgo     int64
	}{
		{"p1", "cargo add tower", 12},
		{"p1", "cargo build", 11},
		{"p1", "cargo test --all", 10},
		{"p2", "go test ./...", 30},
		{"p2", "go build ./...", 29},
		{"p3", "npm install", 50},
	}
	for i, s := range specs {
		rows = append(rows, rec.Record{
			ID: strconv.Itoa(i), Cmd: s.cmd, PromptID: s.prompt,
			Executor: "claude-code", Session: "sess", Hostname: "host", Cwd: "/work",
			StartMs: now - s.minsAgo*60_000, Exit: rec.IntPtr(0),
		})
	}
	for _, p := range []struct {
		id, text string
		minsAgo  int64
	}{
		{"p1", "add a tower middleware layer", 13},
		{"p2", "fix the failing build", 31},
		{"p3", "set up the frontend toolchain", 51},
	} {
		prompts = append(prompts, rec.Record{
			ID: p.id, Type: rec.TypePrompt, Prompt: p.text,
			Executor: "claude-code", Session: "sess", StartMs: now - p.minsAgo*60_000,
		})
	}
	return rows, prompts
}

// explorer opens the agent explorer on the fixture, prompt pane focused.
func explorer(t *testing.T) Model {
	t.Helper()
	rows, prompts := filterFixture()
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, 110, 30)
	m, _ = step(t, m, press("a"))
	m, _ = step(t, m, statsResultMsg{seq: 1, resp: proto.QueryResp{
		Rows: rows, Total: len(rows), Prompts: prompts,
	}})
	return m
}

// typeIn drives the filter box a rune at a time, the way a user does — so the
// live re-filtering on every keystroke is what is under test, not a SetValue.
func typeIn(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = step(t, m, press(string(r)))
	}
	return m
}

// focusCommands tabs round to the command pane, whichever pane focus starts on.
func focusCommands(t *testing.T, m Model) Model {
	t.Helper()
	for range agentPaneCount {
		if m.apane == apCommands {
			return m
		}
		m, _ = step(t, m, press("tab"))
	}
	require.Equal(t, apCommands, m.apane, "could not focus the command pane")
	return m
}

// TestFilterPromptsNarrowsTheList: / over the prompt pane filters prompts by
// their text, like / in the browse view filters commands.
func TestFilterPromptsNarrowsTheList(t *testing.T) {
	m := explorer(t)
	require.Equal(t, 3, m.promptLen(), "all three prompts to start")

	m, _ = step(t, m, press("/"))
	require.True(t, m.afiltering, "/ should open the filter box")
	require.Equal(t, apPrompts, m.afilterPane)

	m = typeIn(t, m, "tower")
	require.Equal(t, 1, m.promptLen(), "only the tower prompt matches")

	p, ok := m.drilledPrompt()
	require.True(t, ok)
	require.Equal(t, "add a tower middleware layer", p.text)

	out := strip(m.View())
	require.Contains(t, out, "add a tower middleware layer")
	require.NotContains(t, out, "fix the failing build", "a non-matching prompt should be gone")
}

// TestFilterCommandsNarrowsOnlyThatPane: with the command pane focused, / aims
// at the commands — the prompt list must not move under the user.
func TestFilterCommandsNarrowsOnlyThatPane(t *testing.T) {
	m := explorer(t)
	m = focusCommands(t, m)
	require.Equal(t, 3, m.drillLen(), "the newest prompt's three commands")

	m, _ = step(t, m, press("/"))
	require.Equal(t, apCommands, m.afilterPane, "/ should aim at the focused list")
	m = typeIn(t, m, "test")

	require.Equal(t, 1, m.drillLen(), "one command matches")
	require.Equal(t, 3, m.promptLen(), "the prompt list is untouched")

	r, ok := m.drilledCmd()
	require.True(t, ok)
	require.Contains(t, r.Cmd, "test")
}

// TestFiltersAreIndependent: each list keeps its own query, so tabbing between
// panes never re-points one pane's filter at another pane's rows.
func TestFiltersAreIndependent(t *testing.T) {
	m := explorer(t)

	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "tower")
	m, _ = step(t, m, press("esc"))

	m = focusCommands(t, m)
	m, _ = step(t, m, press("/"))
	require.Empty(t, m.afilter.Value(), "the command box should start empty, not hold the prompt query")
	m = typeIn(t, m, "build")
	m, _ = step(t, m, press("enter"))

	require.Equal(t, "tower", m.promptQ)
	require.Equal(t, "build", m.cmdQ)
	require.Equal(t, 1, m.promptLen(), "the prompt filter still holds")
	require.Equal(t, 1, m.drillLen(), "cargo build is the one command matching")

	// Re-opening a filter offers its own text to edit rather than making the user
	// retype it.
	m, _ = step(t, m, press("/"))
	require.Equal(t, "build", m.afilter.Value())
}

// TestFilterResetsTheCursor: after a query change the row under the cursor is a
// different row, and a cursor left at index 3 of a two-row list reads as the
// filter having misfired.
func TestFilterResetsTheCursor(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("down"))
	require.Equal(t, 2, m.promptSel)

	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "e") // matches every prompt; only the cursor should move
	require.Equal(t, 3, m.promptLen())
	require.Zero(t, m.promptSel, "the cursor should go back to the top")
}

// TestEscBacksOutOneLevelAtATime: zoom, then the filter, then the view. Each
// level is visible on screen, so each gets its own Esc.
func TestEscBacksOutOneLevelAtATime(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "tower")

	m, _ = step(t, m, press("esc"))
	require.False(t, m.afiltering, "the box closes with the filter kept")
	require.Equal(t, "tower", m.promptQ)

	// z only zooms once the box is closed — inside it, every letter is text.
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)

	m, _ = step(t, m, press("esc"))
	require.False(t, m.zoom, "the next esc unzooms")
	require.Equal(t, "tower", m.promptQ, "unzooming must not drop the filter")

	m, _ = step(t, m, press("esc"))
	require.Empty(t, m.promptQ, "the next esc clears the filter")
	require.Equal(t, 3, m.promptLen())
	require.Equal(t, viewAgents, m.view, "clearing a filter is not leaving the view")

	m, _ = step(t, m, press("esc"))
	require.Equal(t, viewBrowse, m.view, "the last esc leaves")
}

// TestFilterWithNoMatchesSaysWhy: a narrowed-to-nothing pane is otherwise
// indistinguishable from a period in which the agents did nothing.
func TestFilterWithNoMatchesSaysWhy(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "zzzz")
	require.Zero(t, m.promptLen())

	out := strip(m.View())
	require.Contains(t, out, "no prompt matches", "the pane should name the query")
	require.Contains(t, out, "zzzz")

	// And the same for the command pane.
	m, _ = step(t, m, press("esc"))
	m, _ = step(t, m, press("esc")) // clear
	m = focusCommands(t, m)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "zzzz")

	out = strip(m.View())
	require.Contains(t, out, "commands match", "the command pane should name the query too")
	require.NotContains(t, out, "no commands recorded",
		"a filtered-out list is not a prompt that ran nothing")
}

// TestCommandFilterDoesNotRewriteThePromptsAggregate: the details pane describes
// the prompt, not the search. If filtering commands changed those counts, the
// details pane and the prompt list would disagree about the same prompt.
func TestCommandFilterDoesNotRewriteThePromptsAggregate(t *testing.T) {
	m := explorer(t)
	m = focusCommands(t, m)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "test")
	m, _ = step(t, m, press("enter"))
	require.Equal(t, 1, m.drillLen(), "the list is narrowed")

	p, ok := m.drilledPrompt()
	require.True(t, ok)
	require.Equal(t, 3, p.count, "the prompt still ran three commands")
	require.Len(t, p.cmds, 3, "the aggregate keeps every command")
}

// TestFilterSurvivesAPeriodChange: the period and the filter are independent
// narrowings, and re-aggregating for a new window must not silently drop one.
func TestFilterSurvivesAPeriodChange(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "tower")
	m, _ = step(t, m, press("esc"))

	m, _ = step(t, m, press("2")) // 7d
	require.Equal(t, "tower", m.promptQ, "the filter should outlive the period change")
	require.Equal(t, 1, m.promptLen(), "and still be applied")
}

// TestFilterMarkerIsOnThePane: the count in a pane's border is a filtered count,
// and the marker is the only thing on screen that explains the missing rows.
func TestFilterMarkerIsOnThePane(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "tower")
	m, _ = step(t, m, press("esc"))

	out := strip(m.View())
	require.Contains(t, out, "PROMPTS  1/1  /tower", "the prompt pane should show its filter")

	m = focusCommands(t, m)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "cargo")
	m, _ = step(t, m, press("esc"))
	require.Contains(t, strip(m.View()), "/cargo", "the command pane should show its filter")
}

// TestFilterBoxOwnsTheHeaderLine: the query goes where this UI already puts a
// query — and the footer names the list being narrowed, since two panes on
// screen can both be filtered.
func TestFilterBoxOwnsTheHeaderLine(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	lines := strings.Split(strip(m.View()), "\n")
	require.Contains(t, lines[0], "filter prompts ❯", "the header becomes the filter box")
	require.Contains(t, lines[len(lines)-1], "to filter prompts")

	m, _ = step(t, m, press("esc"))
	m = focusCommands(t, m)
	m, _ = step(t, m, press("/"))
	lines = strings.Split(strip(m.View()), "\n")
	require.Contains(t, lines[0], "filter commands ❯")
}

// TestFilterBoxSwallowsViewKeys: while typing, letters are text. "a" must not
// leave the explorer and "?" must not raise the key panel.
func TestFilterBoxSwallowsViewKeys(t *testing.T) {
	m := explorer(t)
	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "a?s")

	require.Equal(t, viewAgents, m.view, "letters are text, not view keys")
	require.False(t, m.showHelp)
	require.Equal(t, "a?s", m.afilter.Value())
}
