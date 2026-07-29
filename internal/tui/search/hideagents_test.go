package search

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

// agentQuerier answers like the daemon does: it applies HumanOnly itself and
// reports what that dropped, so the panel is exercised against the real contract
// rather than a fixture pretending the filter is client-side.
type agentQuerier struct {
	lastReq proto.QueryReq
}

func (a *agentQuerier) Query(req proto.QueryReq) (proto.QueryResp, error) {
	a.lastReq = req

	var rows []rec.Record
	hidden := 0
	for i, s := range []struct{ cmd, tag string }{
		{"git status", ""},
		{"cargo build", "claude-code"},
		{"cargo test", "claude-code"},
		{"make lint", ""},
	} {
		if req.Q != "" && !strings.Contains(s.cmd, req.Q) {
			continue
		}
		if req.HumanOnly && s.tag != "" {
			hidden++
			continue
		}
		rows = append(rows, rec.Record{
			ID: strconv.Itoa(i), Cmd: s.cmd, Tag: s.tag, Hostname: "host",
			StartMs: 1000 - int64(i), Exit: rec.IntPtr(0),
		})
	}
	return proto.QueryResp{
		Rows: rows, Total: len(rows), HiddenAgents: hidden,
		Remote: proto.RemoteInfo{State: proto.RemoteOK},
	}, nil
}

// panelWith opens the panel and delivers one round of results.
func panelWith(t *testing.T, q *agentQuerier, opts Options) Model {
	t.Helper()
	opts.Version = "v1"
	m := NewModel(q, opts)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	return refresh(t, tm.(Model), q, 1)
}

// refresh runs the model's current query through the querier and applies it.
func refresh(t *testing.T, m Model, q *agentQuerier, seq uint64) Model {
	t.Helper()
	resp, err := q.Query(m.buildReq())
	require.NoError(t, err)
	tm, _ := m.Update(queryResultMsg{seq: seq, resp: resp})
	return tm.(Model)
}

// altA is the agent toggle as the terminal delivers it.
func altA() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a"), Alt: true}
}

// TestPanelOpensWithoutAgentCommands: Ctrl-R is for recall, and a prompt's forty
// tool invocations crowd out the handful of commands the user actually typed.
func TestPanelOpensWithoutAgentCommands(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true})

	require.True(t, q.lastReq.HumanOnly, "the filter must be applied server-side")
	require.Len(t, m.rows, 2)
	for _, r := range m.rows {
		require.Emptyf(t, r.Tag, "%q is an agent command", r.Cmd)
	}
}

// TestAltAToggles: ⌥a sits with the panel's other alt-modified toggles, because
// in a filter box a plain letter is text.
func TestAltAToggles(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true})

	tm, cmd := m.Update(altA())
	m = tm.(Model)
	require.False(t, m.hideAgents)
	require.NotNil(t, cmd, "the filter is the daemon's, so it re-queries")

	m = refresh(t, m, q, 2)
	require.False(t, q.lastReq.HumanOnly)
	require.Len(t, m.rows, 4, "every command is back")

	tm, _ = m.Update(altA())
	m = refresh(t, tm.(Model), q, 3)
	require.True(t, q.lastReq.HumanOnly, "⌥a toggles both ways")
	require.Len(t, m.rows, 2)
}

// TestAltAWorksInVimNormalMode: the panel's other alt toggles are insert-only,
// but a filter you cannot turn off from normal mode is a filter you are stuck
// with after one Esc.
func TestAltAWorksInVimNormalMode(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true, Keymap: "vim"})

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // insert -> normal
	m = tm.(Model)
	require.True(t, m.normal)

	tm, _ = m.Update(altA())
	require.False(t, tm.(Model).hideAgents, "⌥a should reach normal mode too")
}

// TestPanelSaysWhatItIsHolding: the panel has one status line and no footer, so
// that line carries the disclosure.
func TestPanelSaysWhatItIsHolding(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true})

	out := strip(m.View())
	require.Contains(t, out, "2 agent commands hidden")
	require.Contains(t, out, "⌥a show agents", "and names the key that brings them back")

	tm, _ := m.Update(altA())
	m = refresh(t, tm.(Model), q, 2)
	require.NotContains(t, strip(m.View()), "hidden")
}

// TestNoMatchesNeverLies is the failure this exists to prevent. Ctrl-R's whole
// job is recall: telling someone a command they ran does not exist is the one
// answer it must never give.
func TestNoMatchesNeverLies(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true})

	m.ti.SetValue("cargo")
	m = refresh(t, m, q, 2)
	require.Empty(t, m.rows, "every cargo command here was an agent's")

	out := strip(m.View())
	require.NotContains(t, out, "no matches", "the matches exist; they are filtered")
	require.Contains(t, out, "2 agent commands hidden")
	require.Contains(t, out, "⌥a shows them")
}

// TestExplicitExecutorOverridesTheFilter: `yore search --executor claude-code`
// is a request for agent commands, so the panel must not open with them hidden —
// nor let ⌥a hide the very rows that were asked for.
func TestExplicitExecutorOverridesTheFilter(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true, Executor: "claude-code"})

	require.False(t, m.hideAgents)
	require.False(t, q.lastReq.HumanOnly)
	require.Equal(t, "claude-code", q.lastReq.Executor)

	tm, _ := m.Update(altA())
	require.False(t, tm.(Model).hideAgents, "⌥a must not fight an explicit --executor")
}

// TestHiddenCountOutranksTheHints: a filter withholding results the user is
// actively searching for matters more than a key hint, so it survives a narrow
// terminal that drops the hints.
func TestHiddenCountOutranksTheHints(t *testing.T) {
	q := &agentQuerier{}
	m := panelWith(t, q, Options{HideAgents: true})

	narrow := strip(m.statusLine(34))
	require.Contains(t, narrow, "agent commands hidden")
	require.NotContains(t, narrow, "⌥/ keys", "the hints shed first")
}
