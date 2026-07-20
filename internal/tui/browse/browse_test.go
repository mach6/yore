package browse

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"yore/internal/proto"
	"yore/internal/rec"
)

// --- test doubles & helpers ---------------------------------------------

type fakeBackend struct {
	mu      sync.Mutex
	reqs    []proto.QueryReq
	resp    proto.QueryResp
	hosts   proto.HostsInfo
	deleted []string
	delErr  error
}

func (f *fakeBackend) Query(req proto.QueryReq) (proto.QueryResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.resp, nil
}

func (f *fakeBackend) Hosts() (proto.HostsInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts, nil
}

func (f *fakeBackend) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeBackend) lastReq() proto.QueryReq {
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
		t.Fatalf("Update returned %T, want browse.Model", tm)
	}
	return nm, cmd
}

func press(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func strip(s string) string { return ansiRE.ReplaceAllString(s, "") }

const now = int64(1_700_000_000_000)

func mkRows(cmds ...string) []rec.Record {
	rows := make([]rec.Record, len(cmds))
	for i, c := range cmds {
		rows[i] = rec.Record{
			ID:       strconv.Itoa(i),
			Cmd:      c,
			Cwd:      "/work/" + strconv.Itoa(i),
			Hostname: "host",
			Session:  "sess-" + strconv.Itoa(i),
			StartMs:  now - int64(i)*1000, // already newest-first
			Exit:     rec.IntPtr(0),
		}
	}
	return rows
}

func mkResp(rows []rec.Record) proto.QueryResp {
	return proto.QueryResp{Rows: rows, Total: len(rows), Remote: proto.RemoteInfo{State: proto.RemoteOK}}
}

func ready(t *testing.T, f *fakeBackend, w, h int) Model {
	t.Helper()
	m := NewModel(f, Options{Version: "v1", Now: now})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	if len(f.hosts.Hosts) > 0 {
		m, _ = step(t, m, hostsResultMsg{info: f.hosts})
	}
	return m
}

// --- tests ---------------------------------------------------------------

func TestHostSelectionChangesScope(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{
			{Hostname: "local-box", Count: 10},
			{Hostname: "beta", Count: 5},
		}},
	}
	m := ready(t, f, 120, 30)

	// Focus the host sidebar (shift+tab from the table lands on hosts).
	m, _ = step(t, m, press("shift+tab"))
	if m.focus != focusHosts {
		t.Fatalf("focus = %v, want focusHosts", m.focus)
	}

	// Down -> local host (index 1) => ScopeLocal, no Host filter.
	m, cmd := step(t, m, press("down"))
	if cmd != nil {
		cmd()
	}
	if got := f.lastReq(); got.Scope != proto.ScopeLocal || got.Host != "" {
		t.Errorf("local host req = %+v, want Scope=local Host=empty", got)
	}

	// Down again -> "beta" (index 2) => ScopeHost with that hostname.
	m, cmd = step(t, m, press("down"))
	if cmd != nil {
		cmd()
	}
	if got := f.lastReq(); got.Scope != proto.ScopeHost || got.Host != "beta" {
		t.Errorf("beta host req = %+v, want Scope=host Host=beta", got)
	}

	// Back up to "All hosts" => ScopeAll.
	m, cmd = step(t, m, press("up"))
	m, cmd = step(t, m, press("up"))
	if cmd != nil {
		cmd()
	}
	if got := f.lastReq(); got.Scope != proto.ScopeAll {
		t.Errorf("all-hosts req scope = %q, want all", got.Scope)
	}
}

func TestSearchSendsTaggedQueries(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	m := ready(t, f, 100, 24)

	m, _ = step(t, m, press("/"))
	if !m.searching {
		t.Fatal("`/` did not focus search")
	}
	var lastCmd tea.Cmd
	for _, r := range "grep" {
		var c tea.Cmd
		m, c = step(t, m, press(string(r)))
		if c != nil {
			lastCmd = c
		}
	}
	if lastCmd == nil {
		t.Fatal("typing produced no query command")
	}
	if _, ok := lastCmd().(queryResultMsg); !ok {
		t.Fatal("typing command did not yield a queryResultMsg")
	}
	if got := f.lastReq(); got.Q != "grep" {
		t.Errorf("query Q = %q, want grep", got.Q)
	}
	if m.seq < 2 {
		t.Errorf("seq = %d, want it to advance per keystroke", m.seq)
	}

	// Esc leaves search focus but keeps the filter text.
	m, _ = step(t, m, press("esc"))
	if m.searching {
		t.Error("esc did not leave search focus")
	}
	if m.ti.Value() != "grep" {
		t.Errorf("filter lost after esc: %q", m.ti.Value())
	}
}

func TestStaleResponseDropped(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 100, 24)

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(mkRows("keep-two"))})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("stale-one"))})

	if len(m.rows) != 1 || m.rows[0].Cmd != "keep-two" {
		t.Fatalf("rows = %+v, want keep-two (seq 2)", m.rows)
	}
	out := strip(m.View())
	if !strings.Contains(out, "keep-two") {
		t.Errorf("view missing keep-two:\n%s", out)
	}
	if strings.Contains(out, "stale-one") {
		t.Errorf("view shows dropped stale-one:\n%s", out)
	}
}

func TestTableNavUpdatesDetail(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha", "bravo", "charlie"))})

	// Row 0 selected first.
	out := strip(m.View())
	if !strings.Contains(out, "/work/0") || !strings.Contains(out, "sess-0") {
		t.Errorf("detail should show row 0 cwd/session:\n%s", out)
	}

	// Move down twice: detail must reflect row 2.
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("j"))
	if m.sel != 2 {
		t.Fatalf("sel = %d, want 2", m.sel)
	}
	out = strip(m.View())
	if !strings.Contains(out, "/work/2") || !strings.Contains(out, "sess-2") {
		t.Errorf("detail should show row 2 cwd/session:\n%s", out)
	}
	if strings.Contains(out, "sess-0") {
		t.Errorf("detail still shows row 0 after navigation:\n%s", out)
	}
}

func TestDeleteConfirmYes(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("keep-a", "drop-b", "keep-c"))})
	m, _ = step(t, m, press("down")) // select drop-b (id "1")

	m, _ = step(t, m, press("d"))
	if !m.confirmDelete {
		t.Fatal("`d` did not arm the delete confirmation")
	}
	if !strings.Contains(strip(m.View()), "delete this command?") {
		t.Error("confirmation prompt not shown in status bar")
	}

	m, _ = step(t, m, press("y"))
	if len(f.deleted) != 1 || f.deleted[0] != "1" {
		t.Fatalf("Delete calls = %v, want [\"1\"]", f.deleted)
	}
	if len(m.rows) != 2 {
		t.Fatalf("rows after delete = %d, want 2", len(m.rows))
	}
	for _, r := range m.rows {
		if r.Cmd == "drop-b" {
			t.Errorf("deleted row still present: %+v", m.rows)
		}
	}
}

func TestDeleteConfirmNo(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})

	m, _ = step(t, m, press("d"))
	m, _ = step(t, m, press("n"))
	if m.confirmDelete {
		t.Error("`n` did not dismiss the confirmation")
	}
	if len(f.deleted) != 0 {
		t.Errorf("Delete should not be called on `n`: %v", f.deleted)
	}
	if len(m.rows) != 2 {
		t.Errorf("rows changed after cancel: %d", len(m.rows))
	}
}

func TestCopyFlash(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("echo hi"))})

	m, cmd := step(t, m, press("enter"))
	if !strings.Contains(strip(m.View()), "copied") {
		t.Errorf("status bar should flash 'copied':\n%s", strip(m.View()))
	}
	if cmd != nil {
		cmd() // out is nil under test: the OSC 52 write is a silent no-op
	}
}

func TestStatsViewRenders(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 7}}},
		resp: mkResp(mkRows(
			"git status", "git commit", "git push",
			"ls -la", "vim main.go",
		)),
	}
	m := ready(t, f, 120, 40)

	m, cmd := step(t, m, press("s"))
	if m.view != viewStats {
		t.Fatal("`s` did not switch to stats view")
	}
	if cmd == nil {
		t.Fatal("toggling stats issued no aggregation query")
	}
	msg := cmd()
	sr, ok := msg.(statsResultMsg)
	if !ok {
		t.Fatalf("stats command yielded %T, want statsResultMsg", msg)
	}
	m, _ = step(t, m, sr)

	out := strip(m.View())
	if !strings.Contains(out, "Top commands") {
		t.Errorf("stats missing top-commands section:\n%s", out)
	}
	if !strings.Contains(out, "git") {
		t.Errorf("stats missing top command 'git':\n%s", out)
	}
	if !strings.Contains(out, "Commands per day") {
		t.Errorf("stats missing sparkline section:\n%s", out)
	}
	if !strings.Contains(out, "█") { // all rows share one day -> that bucket peaks
		t.Errorf("stats sparkline missing a full block glyph:\n%s", out)
	}
	if m.stats == nil || len(m.stats.topCmds) == 0 || m.stats.topCmds[0].name != "git" || m.stats.topCmds[0].n != 3 {
		t.Errorf("top command aggregate = %+v, want git=3 first", m.stats)
	}

	// `s` again returns to the browse view.
	m, _ = step(t, m, press("s"))
	if m.view != viewBrowse {
		t.Error("second `s` did not return to browse")
	}
}

func TestResizeStaysWithinWidth(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "aaaaaaaaaaaaaaaaaaaa", Count: 3}}},
		resp:  mkResp(mkRows(strings.Repeat("x", 400), "short", "another command here")),
	}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	for _, wh := range [][2]int{{200, 50}, {120, 40}, {80, 24}, {60, 20}, {40, 15}, {24, 10}} {
		m, _ = step(t, m, tea.WindowSizeMsg{Width: wh[0], Height: wh[1]})
		for _, mode := range []viewMode{viewBrowse, viewStats} {
			m.view = mode
			if mode == viewStats {
				m.stats = computeStats(f.resp.Rows, m.hosts, now)
			}
			for _, line := range strings.Split(m.View(), "\n") {
				if got := lipgloss.Width(line); got > wh[0] {
					t.Errorf("w=%d h=%d mode=%d: line width %d > %d: %q",
						wh[0], wh[1], mode, got, wh[0], strip(line))
				}
			}
		}
		m.view = viewBrowse
	}
}

func TestFocusCyclesAndRemoteHint(t *testing.T) {
	f := &fakeBackend{resp: func() proto.QueryResp {
		r := mkResp(mkRows("ls"))
		r.Remote = proto.RemoteInfo{State: proto.RemoteOff}
		return r
	}()}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	// Default scope is All hosts; RemoteOff should surface the hint.
	if !strings.Contains(strip(m.View()), "remote sync not configured") {
		t.Errorf("expected remote-off hint in All-hosts scope:\n%s", strip(m.View()))
	}

	// Tab cycles hosts -> table -> detail.
	order := []focus{focusDetail, focusHosts, focusTable}
	for _, want := range order {
		m, _ = step(t, m, press("tab"))
		if m.focus != want {
			t.Fatalf("focus = %v, want %v", m.focus, want)
		}
	}
}

func TestQuit(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 80, 24)
	_, cmd := step(t, m, press("q"))
	if cmd == nil {
		t.Fatal("q produced no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q should return tea.Quit")
	}
}

func TestOSC52RoundTrip(t *testing.T) {
	payload := "git commit -m 'héllo wörld' && echo done"
	seq := osc52(payload)
	if !strings.HasPrefix(seq, "\x1b]52;c;") || !strings.HasSuffix(seq, "\x07") {
		t.Fatalf("osc52 framing wrong: %q", seq)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	got, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if string(got) != payload {
		t.Errorf("round-trip = %q, want %q", got, payload)
	}
}

// readyVim is ready() but with the vim keymap enabled.
func readyVim(t *testing.T, f *fakeBackend, w, h int) Model {
	t.Helper()
	m := NewModel(f, Options{Version: "v1", Now: now, Keymap: "vim"})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	if len(f.hosts.Hosts) > 0 {
		m, _ = step(t, m, hostsResultMsg{info: f.hosts})
	}
	return m
}

func TestVimBrowseNavigation(t *testing.T) {
	f := &fakeBackend{}
	m := readyVim(t, f, 120, 40)
	// Plenty of rows so a half-page jump is not clamped by the row count.
	cmds := make([]string, 40)
	for i := range cmds {
		cmds[i] = "cmd-" + strconv.Itoa(i)
	}
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows(cmds...))})
	if !m.vim {
		t.Fatal("vim mode should be enabled from Options.Keymap")
	}

	// j/k navigate the table (these also work in emacs, but must in vim).
	m, _ = step(t, m, press("j"))
	m, _ = step(t, m, press("j"))
	if m.sel != 2 {
		t.Fatalf("after two j: sel = %d, want 2", m.sel)
	}
	m, _ = step(t, m, press("k"))
	if m.sel != 1 {
		t.Fatalf("after k: sel = %d, want 1", m.sel)
	}

	// g/G jump to top/bottom.
	m, _ = step(t, m, press("G"))
	if m.sel != len(cmds)-1 {
		t.Fatalf("G: sel = %d, want %d (last row)", m.sel, len(cmds)-1)
	}
	m, _ = step(t, m, press("g"))
	if m.sel != 0 {
		t.Fatalf("g: sel = %d, want 0", m.sel)
	}

	// h/l move focus between panes (default focus is the table).
	if m.focus != focusTable {
		t.Fatalf("default focus = %v, want focusTable", m.focus)
	}
	m, _ = step(t, m, press("l"))
	if m.focus != focusDetail {
		t.Fatalf("l: focus = %v, want focusDetail", m.focus)
	}
	m, _ = step(t, m, press("h"))
	m, _ = step(t, m, press("h"))
	if m.focus != focusHosts {
		t.Fatalf("h twice from table: focus = %v, want focusHosts", m.focus)
	}

	// ctrl+d is a half-page scroll in vim, NOT delete.
	m.focus = focusTable
	m.sel = 0
	m, _ = step(t, m, press("ctrl+d"))
	if m.confirmDelete {
		t.Fatal("ctrl+d in vim mode wrongly armed delete confirmation")
	}
	if m.sel == 0 {
		t.Fatal("ctrl+d in vim mode did not scroll the selection down")
	}
	half := m.halfPage()
	if m.sel != half {
		t.Fatalf("ctrl+d: sel = %d, want %d (half-page)", m.sel, half)
	}
	m, _ = step(t, m, press("ctrl+u"))
	if m.sel != 0 {
		t.Fatalf("ctrl+u: sel = %d, want back to 0", m.sel)
	}

	// `d` still deletes (single-key, with confirm) in vim mode.
	m, _ = step(t, m, press("d"))
	if !m.confirmDelete {
		t.Fatal("`d` should still arm delete in vim mode")
	}
}

func TestEmacsCtrlDStillDeletes(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30) // default (emacs) keymap
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	if m.vim {
		t.Fatal("default keymap should not be vim")
	}
	m, _ = step(t, m, press("ctrl+d"))
	if !m.confirmDelete {
		t.Fatal("ctrl+d in emacs mode should still arm delete")
	}
}

// compile-time assurance the interface matches what daemon.Client provides.
var _ Backend = (*fakeBackend)(nil)
