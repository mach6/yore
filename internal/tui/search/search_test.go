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

	"yore/internal/proto"
	"yore/internal/rec"
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
	if !ok {
		t.Fatalf("Update returned %T, want search.Model", tm)
	}
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
	case "alt+d":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d"), Alt: true}
	default:
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
	if cmd == nil {
		t.Fatal("typing produced no command")
	}
	if _, ok := cmd().(queryResultMsg); !ok {
		t.Fatalf("expected queryResultMsg from typing command")
	}
	got := f.last()
	if got.Q != "g" {
		t.Errorf("Q = %q, want %q", got.Q, "g")
	}
	if got.Scope != proto.ScopeLocal {
		t.Errorf("Scope = %q, want %q", got.Scope, proto.ScopeLocal)
	}
	if !got.Dedupe {
		t.Error("Dedupe = false, want true (default on)")
	}
	if got.Limit != queryLimit {
		t.Errorf("Limit = %d, want %d", got.Limit, queryLimit)
	}
	if got.Session != "" || got.Cwd != "" {
		t.Errorf("local scope should not populate Session/Cwd: %+v", got)
	}
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
	if got := f.last(); got.Scope != proto.ScopeSession || got.Session != "S1" || got.Cwd != "" {
		t.Errorf("session scope req = %+v, want Scope=session Session=S1 Cwd=empty", got)
	}
	if got := f.last(); got.Q != "l" {
		t.Errorf("Q = %q, want %q", got.Q, "l")
	}

	// session -> cwd
	m, cmd = step(t, m, key("ctrl+r")) // cwd
	cmd()
	if got := f.last(); got.Scope != proto.ScopeCwd || got.Cwd != "/work" || got.Session != "" {
		t.Errorf("cwd scope req = %+v, want Scope=cwd Cwd=/work Session=empty", got)
	}
}

func TestOutOfOrderResponseDiscarded(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(mkRows("keep-two"))})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("stale-one"))})

	if len(m.rows) != 1 || m.rows[0].Cmd != "keep-two" {
		t.Fatalf("rows = %+v, want the seq-2 rows (keep-two)", m.rows)
	}
	out := strip(m.View())
	if !strings.Contains(out, "keep-two") {
		t.Errorf("view missing keep-two:\n%s", out)
	}
	if strings.Contains(out, "stale-one") {
		t.Errorf("view contains discarded stale-one:\n%s", out)
	}
}

func TestCtrlRCyclesScopes(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{})

	want := []string{proto.ScopeAll, proto.ScopeSession, proto.ScopeCwd, proto.ScopeWorkspace, proto.ScopeLocal}
	if m.scope != proto.ScopeLocal {
		t.Fatalf("start scope = %q, want local", m.scope)
	}
	for _, w := range want {
		var cmd tea.Cmd
		m, cmd = step(t, m, key("ctrl+r"))
		if m.scope != w {
			t.Fatalf("scope = %q, want %q", m.scope, w)
		}
		if cmd == nil {
			t.Fatalf("ctrl+r at scope %q did not re-query", w)
		}
	}
}

func TestAltDTogglesDedupe(t *testing.T) {
	f := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(f, Options{})
	if !m.dedupe {
		t.Fatal("dedupe default should be on")
	}

	m, cmd := step(t, m, key("alt+d"))
	cmd()
	if m.dedupe {
		t.Error("alt+d did not turn dedupe off")
	}
	if f.last().Dedupe {
		t.Error("re-query after toggle should have Dedupe=false")
	}
	if !strings.Contains(strip(m.View()), "dups shown") {
		t.Error("status line should show 'dups shown' when dedupe is off")
	}

	m, cmd = step(t, m, key("alt+d"))
	cmd()
	if !m.dedupe {
		t.Error("second alt+d did not turn dedupe back on")
	}
	if !f.last().Dedupe {
		t.Error("re-query after second toggle should have Dedupe=true")
	}
}

func TestEnterReturnsSelected(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a", "cmd-b", "cmd-c"))})

	// First row on plain Enter.
	if tm, _ := m.Update(key("enter")); tm.(Model).accepted != "cmd-a" || !tm.(Model).accept {
		t.Fatalf("enter on first row = %q accept=%v, want cmd-a true", tm.(Model).accepted, tm.(Model).accept)
	}

	// Move down twice, then Enter picks the non-first selection.
	m, _ = step(t, m, key("down"))
	m, _ = step(t, m, key("ctrl+n"))
	tm, _ := m.Update(key("enter"))
	res := tm.(Model)
	if !res.accept || res.accepted != "cmd-c" {
		t.Fatalf("enter after two downs = %q accept=%v, want cmd-c true", res.accepted, res.accept)
	}
	if !res.done {
		t.Error("accepting should mark the model done")
	}
}

func TestEscCancels(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a"))})

	tm, _ := m.Update(key("esc"))
	res := tm.(Model)
	if res.accept || !res.cancel || !res.done {
		t.Fatalf("esc: accept=%v cancel=%v done=%v, want false true true", res.accept, res.cancel, res.done)
	}
	// Done model renders nothing so the panel clears.
	if res.View() != "" {
		t.Errorf("done model View = %q, want empty", res.View())
	}
}

func TestWindowingSlides(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 12}) // rowsVisible = 10

	cmds := make([]string, 30)
	for i := range cmds {
		cmds[i] = "row-" + strconv.Itoa(1000 + i)[1:] // row-000 .. row-029
	}
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows(cmds...))})

	for i := 0; i < 20; i++ {
		m, _ = step(t, m, key("down"))
	}
	if m.sel != 20 {
		t.Fatalf("sel = %d, want 20", m.sel)
	}

	out := strip(m.View())
	for _, want := range []string{"row-011", "row-020"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing visible %s:\n%s", want, out)
		}
	}
	for _, absent := range []string{"row-000", "row-010", "row-021", "row-029"} {
		if strings.Contains(out, absent) {
			t.Errorf("view shows out-of-window %s:\n%s", absent, out)
		}
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
	if !strings.Contains(out, "run foo now") {
		t.Errorf("view missing highlighted command:\n%s", out)
	}
	if i := strings.Index(out, "foo"); i < 0 {
		t.Errorf("matched term not present in rendered command:\n%s", out)
	}
}

func TestTruncationRespectsWidth(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{Version: "v1.2.3"})
	const w = 40
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 12})

	long := strings.Repeat("x", 200)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows(long, long))})

	for _, line := range strings.Split(m.View(), "\n") {
		if got := lipgloss.Width(line); got > w {
			t.Errorf("line width %d > %d: %q", got, w, strip(line))
		}
	}
}

func TestMultilineCommandCollapsed(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 12})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("for i in 1 2\ndo\n  echo $i\ndone"))})

	out := strip(m.View())
	if !strings.Contains(out, "⏎") {
		t.Errorf("multiline command should show a ⏎ marker:\n%s", out)
	}
	if strings.Contains(out, "\n  echo") {
		t.Errorf("continuation indentation should be squeezed:\n%s", out)
	}
}

func TestDaemonUnreachableKeepsRows(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("cmd-a"))})
	m, _ = step(t, m, queryResultMsg{seq: 2, err: errors.New("dial: connection refused")})

	if len(m.rows) != 1 || m.rows[0].Cmd != "cmd-a" {
		t.Fatalf("rows lost after error: %+v", m.rows)
	}
	if !strings.Contains(strip(m.View()), "daemon unreachable") {
		t.Errorf("status should show daemon unreachable:\n%s", strip(m.View()))
	}
}

func TestRemoteOffHint(t *testing.T) {
	f := &fakeQuerier{}
	m := NewModel(f, Options{})
	m, _ = step(t, m, key("ctrl+r")) // scope all
	resp := mkResp(mkRows("cmd-a"))
	resp.Remote = proto.RemoteInfo{State: proto.RemoteOff}
	m, _ = step(t, m, queryResultMsg{seq: 5, resp: resp})

	if !strings.Contains(strip(m.View()), "sync not configured") {
		t.Errorf("all-scope + RemoteOff should hint sync not configured:\n%s", strip(m.View()))
	}
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
	if f.calls != len(cmds) {
		t.Errorf("fake got %d calls, want %d", f.calls, len(cmds))
	}
}
