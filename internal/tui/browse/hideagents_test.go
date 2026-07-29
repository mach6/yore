package browse

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

// agentBackend answers like the daemon does: it applies HumanOnly itself and
// reports how many rows that dropped, so the browser is exercised against the
// real contract rather than a fixture that pretends the filter is client-side.
type agentBackend struct {
	fakeBackend
	lastHumanOnly bool
}

func (a *agentBackend) Query(req proto.QueryReq) (proto.QueryResp, error) {
	a.lastHumanOnly = req.HumanOnly

	var rows []rec.Record
	hidden := 0
	for i, s := range []struct{ cmd, executor string }{
		{"git status", ""},
		{"cargo build", "claude-code"},
		{"cargo test", "claude-code"},
		{"make lint", ""},
		{"cargo fmt", "claude-code"},
	} {
		if req.Q != "" && !strings.Contains(s.cmd, req.Q) {
			continue
		}
		if req.HumanOnly && s.executor != "" {
			hidden++
			continue
		}
		rows = append(rows, rec.Record{
			ID: strconv.Itoa(i), Cmd: s.cmd, Executor: s.executor, Hostname: "host", Cwd: "/w",
			StartMs: now - int64(i)*60_000, Exit: rec.IntPtr(0),
		})
	}
	return proto.QueryResp{
		Rows: rows, Total: len(rows), HiddenAgents: hidden,
		Remote: proto.RemoteInfo{State: proto.RemoteOK},
	}, nil
}

// browseWith opens the browse table and delivers one round of results.
func browseWith(t *testing.T, f *agentBackend, hide bool) Model {
	t.Helper()
	m := NewModel(f, Options{Version: "v1", Now: now, HideAgents: hide})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	return deliver(t, m, f, 1)
}

// deliver runs the model's current query through the backend and applies it,
// the way the real command would.
func deliver(t *testing.T, m Model, f *agentBackend, seq uint64) Model {
	t.Helper()
	resp, err := f.Query(m.buildReq())
	require.NoError(t, err)
	m, _ = step(t, m, queryResultMsg{seq: seq, resp: resp})
	return m
}

// TestBrowseOpensWithoutAgentCommands: one prompt's forty tool invocations bury
// a morning of the user's own work, and the explorer already shows that history
// grouped by the prompt behind it.
func TestBrowseOpensWithoutAgentCommands(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)

	require.True(t, f.lastHumanOnly, "the filter must be applied server-side")
	require.Len(t, m.rows, 2, "only the commands the user typed")
	for _, r := range m.rows {
		require.Emptyf(t, r.Executor, "%q is an agent command", r.Cmd)
	}
}

// TestShowingAgentsIsOneKey: A flips the filter and re-queries, because the
// rows it wants were never sent.
func TestShowingAgentsIsOneKey(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)

	m, cmd := step(t, m, press("A"))
	require.False(t, m.hideAgents)
	require.NotNil(t, cmd, "A should re-issue the query")

	m = deliver(t, m, f, 2)
	require.False(t, f.lastHumanOnly)
	require.Len(t, m.rows, 5, "every command is back")

	m, _ = step(t, m, press("A"))
	m = deliver(t, m, f, 3)
	require.True(t, f.lastHumanOnly, "A toggles both ways")
	require.Len(t, m.rows, 2)
}

// TestHiddenCountIsOnScreen: yore does not drop a category of history quietly.
// Without this the view is indistinguishable from one where no agent ever ran.
func TestHiddenCountIsOnScreen(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)

	out := strip(m.View())
	require.Contains(t, out, "3 agent commands hidden", "the status bar should say what is held back")

	m, _ = step(t, m, press("A"))
	m = deliver(t, m, f, 2)
	require.NotContains(t, strip(m.View()), "hidden", "nothing is hidden once they are shown")
}

// TestEmptySearchNamesTheFilter is the failure this design exists to prevent: a
// search whose only matches are an agent's must not answer "no matches", which
// is a lie about the user's own history.
func TestEmptySearchNamesTheFilter(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)

	m, _ = step(t, m, press("/"))
	for _, r := range "cargo" {
		m, _ = step(t, m, press(string(r)))
	}
	m = deliver(t, m, f, 2)
	require.Empty(t, m.rows, "every cargo command here was run by an agent")

	out := strip(m.View())
	require.NotContains(t, out, "no matches", "the matches exist; they are filtered")
	require.Contains(t, out, "3 agent commands hidden")
	require.Contains(t, out, "A shows them", "an empty screen should be a way forward")
}

// TestFooterOffersTheKeyWhenItMatters: the hint appears exactly when someone
// might be wondering where a command they remember running went.
func TestFooterOffersTheKeyWhenItMatters(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)
	require.Contains(t, footerText(m), "A show agents")

	m, _ = step(t, m, press("A"))
	m = deliver(t, m, f, 2)
	require.NotContains(t, footerText(m), "A show agents",
		"with nothing hidden there is nothing to explain")

	// The full key list carries it either way, and says what pressing it does now.
	m, _ = step(t, m, press("?"))
	require.Contains(t, strip(m.View()), "hide agent commands")
}

// TestExecutorFilterWinsOverTheAgentFilter: asking for one executor is asking
// for agent commands, so the two filters cannot both apply.
func TestExecutorFilterWinsOverTheAgentFilter(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)
	require.True(t, m.buildReq().HumanOnly)

	m.executorFilter = "claude-code"
	require.False(t, m.buildReq().HumanOnly, "t must not be cancelled by A")
	require.Equal(t, "claude-code", m.buildReq().Executor)
}

// TestNarrowedPeriodDropsTheCount: the daemon counts across everything the query
// matched, and the period tabs narrow further over rows it already dropped — so
// with a period selected the number cannot be attributed to the window on
// screen, and quoting it would describe rows that are not there.
func TestNarrowedPeriodDropsTheCount(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, true)
	require.Contains(t, m.hiddenAgentsNote(), "3", "the All window can be exact")

	m, _ = step(t, m, press("2")) // 7d
	m = deliver(t, m, f, 2)
	note := m.hiddenAgentsNote()
	require.Equal(t, "agent commands hidden", note, "no number it cannot stand behind")
	require.Contains(t, strip(m.View()), note, "but still says the filter is on")
}

// TestShowingAgentsIsNotASetting: A is a session toggle. config's
// hide_agent_commands decides what the browser opens with, and a keystroke that
// rewrote it would make an experiment permanent.
func TestShowingAgentsIsNotASetting(t *testing.T) {
	f := &agentBackend{}
	m := browseWith(t, f, false)
	require.False(t, f.lastHumanOnly, "opening with the setting off shows everything")
	require.Len(t, m.rows, 5)
	require.Empty(t, m.hiddenAgentsNote(), "and reports nothing hidden")
}
