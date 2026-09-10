package search

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/match"
	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
)

// --- test doubles & helpers ---------------------------------------------

type fakeQuerier struct {
	mu    sync.Mutex
	reqs  []proto.QueryReq
	resp  proto.QueryResp
	err   error
	calls int
}

func (f *fakeQuerier) Query(req proto.QueryReq) (proto.QueryResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.calls++
	return f.resp, f.err
}

func (f *fakeQuerier) last() proto.QueryReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return proto.QueryReq{}
	}
	return f.reqs[len(f.reqs)-1]
}

func step(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	tm, cmd := m.Update(msg)
	nm, ok := tm.(Model)
	require.Truef(t, ok, "Update returned %T, want search.Model", tm)
	return nm, cmd
}

func key(s string) tea.KeyMsg {
	switch s {
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	case "ctrl+n":
		return tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+g":
		return tea.KeyMsg{Type: tea.KeyCtrlG}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		if r, ok := strings.CutPrefix(s, "alt+"); ok {
			return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r), Alt: true}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func typeStr(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = step(t, m, key(string(r)))
	}
	return m
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func strip(s string) string { return ansiRE.ReplaceAllString(s, "") }

func mkRows(cmds ...string) []rec.Record {
	now := time.Now().UnixMilli()
	rows := make([]rec.Record, len(cmds))
	for i, c := range cmds {
		rows[i] = rec.Record{
			ID:       strconv.Itoa(i),
			Cmd:      c,
			Hostname: "host",
			StartMs:  now,
			Exit:     rec.IntPtr(0),
		}
	}
	return rows
}

func mkResp(rows []rec.Record) proto.QueryResp {
	return proto.QueryResp{
		Rows:   rows,
		Total:  len(rows),
		Scope:  proto.ScopeLocal,
		Remote: proto.RemoteInfo{State: proto.RemoteOK},
	}
}

// --- tests ---------------------------------------------------------------

func TestTypingSendsQuery(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{Version: "v1"})

	m, cmd := step(t, m, key("g"))
	require.NotNil(t, cmd, "typing produced no command")
	_, ok := cmd().(queryResultMsg)
	require.True(t, ok, "expected queryResultMsg from typing command")

	got := f.last()
	require.Equal(t, "g", got.Q)
	require.Equal(t, proto.ScopeLocal, got.Scope)
	require.True(t, got.Dedupe, "Dedupe = false, want true (default on)")
	require.Equal(t, queryLimit, got.Limit)
	require.Emptyf(t, got.Session, "local scope should not populate Session/Cwd: %+v", got)
	require.Emptyf(t, got.Cwd, "local scope should not populate Session/Cwd: %+v", got)
	_ = m
}

func TestScopeFieldsPopulated(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{Session: "S1", Cwd: "/work"})
	m = typeStr(t, m, "l")

	// local -> all -> session
	m, _ = step(t, m, key("ctrl+r")) // all
	var cmd tea.Cmd
	m, cmd = step(t, m, key("ctrl+r")) // session
	cmd()
	got := f.last()
	require.Equalf(t, proto.ScopeSession, got.Scope, "session scope req = %+v", got)
	require.Equalf(t, "S1", got.Session, "session scope req = %+v", got)
	require.Emptyf(t, got.Cwd, "session scope req = %+v", got)
	require.Equal(t, "l", f.last().Q)

	// session -> cwd
	_, cmd = step(t, m, key("ctrl+r")) // cwd
	cmd()
	got = f.last()
	require.Equalf(t, proto.ScopeCwd, got.Scope, "cwd scope req = %+v", got)
	require.Equalf(t, "/work", got.Cwd, "cwd scope req = %+v", got)
	require.Emptyf(t, got.Session, "cwd scope req = %+v", got)
}

func TestOutOfOrderResponseDiscarded(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(mkRows("keep-two"))})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("stale-one"))})

	require.Lenf(t, m.rows, 1, "rows = %+v, want the seq-2 rows (keep-two)", m.rows)
	require.Equalf(t, "keep-two", m.rows[0].Cmd, "rows = %+v, want the seq-2 rows (keep-two)", m.rows)
	out := strip(m.View())
	require.Containsf(t, out, "keep-two", "view missing keep-two:\n%s", out)
	require.NotContainsf(t, out, "stale-one", "view contains discarded stale-one:\n%s", out)
}

// TestHostColumnOnlyForScopeAll is the fix: only --scope all can return other
// hosts' rows, so only there does the row prefix carry a host column. Every
// other scope (local/session/cwd/workspace, and the Ctrl-R default) hides it.
func TestHostColumnOnlyForScopeAll(t *testing.T) {
	rows := []rec.Record{{
		ID: "0", Cmd: "git status", Hostname: "zbox42",
		StartMs: time.Now().UnixMilli(), Exit: rec.IntPtr(0),
	}}

	nonAll := []string{proto.ScopeLocal, proto.ScopeSession, proto.ScopeCwd, proto.ScopeWorkspace}
	for _, scope := range nonAll {
		m := NewModel(&fakeQuerier{}, Options{Scope: scope, Session: "S", Cwd: "/w"})
		m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})
		out := strip(m.View())
		require.Containsf(t, out, "git status", "%s: row missing:\n%s", scope, out)
		require.NotContainsf(t, out, "zbox42", "%s must NOT show the host column:\n%s", scope, out)
	}

	// --scope all shows the host column.
	m := NewModel(&fakeQuerier{}, Options{Scope: proto.ScopeAll})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})
	out := strip(m.View())
	require.Containsf(t, out, "zbox42", "all scope must show the host column:\n%s", out)
}

func TestNewModelInitialScope(t *testing.T) {
	tests := []struct {
		name string
		opt  string
		want string
	}{
		{"empty defaults to local", "", proto.ScopeLocal},
		{"all", proto.ScopeAll, proto.ScopeAll},
		{"session", proto.ScopeSession, proto.ScopeSession},
		{"cwd", proto.ScopeCwd, proto.ScopeCwd},
		{"workspace", proto.ScopeWorkspace, proto.ScopeWorkspace},
		{"local", proto.ScopeLocal, proto.ScopeLocal},
		{"host is not a search scope", proto.ScopeHost, proto.ScopeLocal},
		{"bogus falls back to local", "bogus", proto.ScopeLocal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel(&fakeQuerier{}, Options{Scope: tc.opt})
			require.Equal(t, tc.want, m.scope)
		})
	}
}

func TestCtrlRCyclesScopes(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{})

	want := []string{proto.ScopeAll, proto.ScopeSession, proto.ScopeCwd, proto.ScopeWorkspace, proto.ScopeLocal}
	require.Equalf(t, proto.ScopeLocal, m.scope, "start scope = %q, want local", m.scope)
	for _, w := range want {
		var cmd tea.Cmd
		m, cmd = step(t, m, key("ctrl+r"))
		require.Equal(t, w, m.scope)
		require.NotNilf(t, cmd, "ctrl+r at scope %q did not re-query", w)
	}
}

func TestAltDTogglesDedupe(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{})
	require.True(t, m.dedupe, "dedupe default should be on")

	m, cmd := step(t, m, key("alt+d"))
	cmd()
	require.False(t, m.dedupe, "alt+d did not turn dedupe off")
	require.False(t, f.last().Dedupe, "re-query after toggle should have Dedupe=false")
	require.Contains(t, strip(m.View()), "duplicates shown")

	m, cmd = step(t, m, key("alt+d"))
	cmd()
	require.True(t, m.dedupe, "second alt+d did not turn dedupe back on")
	require.True(t, f.last().Dedupe, "re-query after second toggle should have Dedupe=true")
}

func TestEnterReturnsSelected(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a", "cmd-b", "cmd-c"))})

	// First row on plain Enter.
	tm, _ := m.Update(key("enter"))
	res := tm.(Model)
	require.Equalf(t, "cmd-a", res.accepted, "enter on first row = %q accept=%v, want cmd-a true", res.accepted, res.accept)
	require.Truef(t, res.accept, "enter on first row = %q accept=%v, want cmd-a true", res.accepted, res.accept)

	// Move down twice, then Enter picks the non-first selection.
	m, _ = step(t, m, key("down"))
	m, _ = step(t, m, key("ctrl+n"))
	tm, _ = m.Update(key("enter"))
	res = tm.(Model)
	require.Truef(t, res.accept, "enter after two downs = %q accept=%v, want cmd-c true", res.accepted, res.accept)
	require.Equalf(t, "cmd-c", res.accepted, "enter after two downs = %q accept=%v, want cmd-c true", res.accepted, res.accept)
	require.True(t, res.done, "accepting should mark the model done")
}

func TestEscCancels(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a"))})

	tm, _ := m.Update(key("esc"))
	res := tm.(Model)
	require.Falsef(t, res.accept, "esc: accept=%v cancel=%v done=%v, want false true true", res.accept, res.cancel, res.done)
	require.Truef(t, res.cancel, "esc: accept=%v cancel=%v done=%v, want false true true", res.accept, res.cancel, res.done)
	require.Truef(t, res.done, "esc: accept=%v cancel=%v done=%v, want false true true", res.accept, res.cancel, res.done)
	// Done model renders nothing so the panel clears.
	require.Empty(t, res.View())
}

func TestWindowingSlides(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 12}) // height 12 gives 10 visible rows

	cmds := make([]string, 30)
	for i := range cmds {
		cmds[i] = "row-" + strconv.Itoa(1000 + i)[1:] // row-000 .. row-029
	}
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows(cmds...))})

	for i := 0; i < 20; i++ {
		m, _ = step(t, m, key("down"))
	}
	require.Equal(t, 20, m.sel)

	out := strip(m.View())
	for _, want := range []string{"row-011", "row-020"} {
		require.Containsf(t, out, want, "view missing visible %s:\n%s", want, out)
	}
	for _, absent := range []string{"row-000", "row-010", "row-021", "row-029"} {
		require.NotContainsf(t, out, absent, "view shows out-of-window %s:\n%s", absent, out)
	}
}

func TestHighlightRendered(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m = typeStr(t, m, "foo")
	m, _ = step(t, m, queryResultMsg{seq: 99, resp: mkResp(mkRows("run foo now"))})

	// Style assertions are lenient (the test environment has no color
	// profile, so lipgloss renders plain): assert the command and the matched
	// term survive rendering, in order.
	out := strip(m.View())
	require.Containsf(t, out, "run foo now", "view missing highlighted command:\n%s", out)
	require.GreaterOrEqualf(t, strings.Index(out, "foo"), 0, "matched term not present in rendered command:\n%s", out)
}

func TestTruncationRespectsWidth(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{Version: "v1.2.3"})
	const w = 40
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 12})

	long := strings.Repeat("x", 200)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows(long, long))})

	for _, line := range strings.Split(m.View(), "\n") {
		require.LessOrEqualf(t, lipgloss.Width(line), w, "line width %d > %d: %q", lipgloss.Width(line), w, strip(line))
	}
}

func TestMultilineCommandCollapsed(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 12})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("for i in 1 2\ndo\n  echo $i\ndone"))})

	out := strip(m.View())
	require.Containsf(t, out, "⏎", "multiline command should show a ⏎ marker:\n%s", out)
	require.NotContainsf(t, out, "\n  echo", "continuation indentation should be squeezed:\n%s", out)
}

func TestDaemonUnreachableKeepsRows(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a"))})
	m, _ = step(t, m, queryResultMsg{seq: 2, err: errors.New("dial: connection refused")})

	require.Lenf(t, m.rows, 1, "rows lost after error: %+v", m.rows)
	require.Equalf(t, "cmd-a", m.rows[0].Cmd, "rows lost after error: %+v", m.rows)
	require.Containsf(t, strip(m.View()), "daemon unreachable", "status should show daemon unreachable:\n%s", strip(m.View()))
}

func TestRemoteOffHint(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, key("ctrl+r")) // scope all
	resp := mkResp(mkRows("cmd-a"))
	resp.Remote = proto.RemoteInfo{State: proto.RemoteOff}
	m, _ = step(t, m, queryResultMsg{seq: 5, resp: resp})

	require.Containsf(t, strip(m.View()), "sync not configured", "all-scope + RemoteOff should hint sync not configured:\n%s", strip(m.View()))
}

func TestVimSearchEscToNormal(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{Keymap: "vim"})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a", "cmd-b", "cmd-c"))})
	require.True(t, m.vim, "vim mode should be enabled from Options.Keymap")

	// First Esc enters the normal sub-mode instead of cancelling.
	m, _ = step(t, m, key("esc"))
	require.Falsef(t, m.cancel, "first esc cancelled: cancel=%v done=%v, want false/false", m.cancel, m.done)
	require.Falsef(t, m.done, "first esc cancelled: cancel=%v done=%v, want false/false", m.cancel, m.done)
	require.True(t, m.normal, "first esc did not enter the normal sub-mode")

	// j/k navigate results while in normal mode.
	m, _ = step(t, m, key("j"))
	require.Equal(t, 1, m.sel)
	m, _ = step(t, m, key("j"))
	m, _ = step(t, m, key("k"))
	require.Equal(t, 1, m.sel)

	// An unmapped key in normal mode must NOT edit the filter text.
	m, _ = step(t, m, key("z"))
	require.Emptyf(t, m.ti.Value(), "normal-mode 'z' edited the filter: %q", m.ti.Value())

	// `i` returns to insert/filter mode; typing edits again.
	m, _ = step(t, m, key("i"))
	require.False(t, m.normal, "`i` did not return to insert mode")
	m = typeStr(t, m, "x")
	require.Equal(t, "x", m.ti.Value())

	// Esc back to normal, then a second Esc cancels.
	m, _ = step(t, m, key("esc"))
	require.True(t, m.normal, "esc did not re-enter normal mode")
	tm, cmd := m.Update(key("esc"))
	res := tm.(Model)
	require.Truef(t, res.cancel, "second esc: cancel=%v done=%v, want true/true", res.cancel, res.done)
	require.Truef(t, res.done, "second esc: cancel=%v done=%v, want true/true", res.cancel, res.done)
	require.NotNil(t, cmd, "second esc should return tea.Quit")
}

func TestConcurrentQueryCommands(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{})

	var cmds []tea.Cmd
	for _, r := range "abcdef" {
		var c tea.Cmd
		m, c = step(t, m, key(string(r)))
		if c != nil {
			cmds = append(cmds, c)
		}
	}

	var wg sync.WaitGroup
	for _, c := range cmds {
		wg.Add(1)
		go func(c tea.Cmd) {
			defer wg.Done()
			_ = c()
		}(c)
	}
	wg.Wait()
	require.Equal(t, len(cmds), f.calls)
}

// TestFuzzyHighlightsTheRunesItMatched guards the pairing of matcher and
// highlighter. Under alt+z the daemon selects rows by subsequence, and a row
// returns need not contain the query as a substring anywhere: highlighting one
// with the substring spans marks nothing at all, which is how the panel used to
// answer "why is this row here?" in the one mode where the answer is not
// obvious by eye. It asserts the spans rather than the rendered line because
// strip() throws styling away, so a line-level check passes either way.
func TestFuzzyHighlightsTheRunesItMatched(t *testing.T) {
	const cmd = "git push origin main"
	f := &fakeQuerier{resp: mkResp(mkRows(cmd))}
	m := NewModel(f, Options{Version: "v1"})
	m = typeStr(t, m, "gpo")
	q := match.Parse("gpo")

	require.Empty(t, m.matchRanges(cmd, q),
		"substring matching has nothing to mark for a subsequence-only query")

	m, _ = step(t, m, key("alt+z"))
	require.True(t, m.fuzzy, "alt+z should turn fuzzy matching on")
	require.Equal(t, [][2]int{{0, 1}, {4, 5}, {9, 10}}, m.matchRanges(cmd, q),
		"alt+z should mark the g, p and o the match was made of")

	m, _ = step(t, m, key("alt+z"))
	require.False(t, m.fuzzy)
	require.Empty(t, m.matchRanges(cmd, q), "alt+z off goes back to substring spans")
}

// TestStatusNamesTheActiveToggles: alt+z and alt+f change what the list is
// and used to leave no mark anywhere, so a panel matched by subsequence or
// ranked by frequency looked exactly like one that was neither. The scope has
// its word in the prompt; these two need the status line to have one.
func TestStatusNamesTheActiveToggles(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("git push origin main"))}
	m := NewModel(f, Options{Version: "v1"})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 132, Height: 12})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("git push origin main"))})

	require.NotContains(t, strip(m.View()), "fuzzy", "nothing to say when both are off")
	require.NotContains(t, strip(m.View()), "by frequency")

	m, _ = step(t, m, key("alt+z"))
	require.Contains(t, strip(m.View()), "fuzzy", "alt+z should say so on the status line")

	m, _ = step(t, m, key("alt+f"))
	require.Contains(t, strip(m.View()), "by frequency", "alt+f should say so too")

	m, _ = step(t, m, key("alt+z"))
	m, _ = step(t, m, key("alt+f"))
	out := strip(m.View())
	require.NotContains(t, out, "fuzzy", "toggling back off should take the label away")
	require.NotContains(t, out, "by frequency")
}
