package browse

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

// --- test doubles & helpers ---------------------------------------------

type fakeBackend struct {
	mu         sync.Mutex
	reqs       []proto.QueryReq
	resp       proto.QueryResp
	hosts      proto.HostsInfo
	hostsCalls int // Hosts() invocations; the sidebar may re-fetch repeatedly
	deleted    []string
	delErr     error
	devices    proto.DevicesInfo
	approved   []string
	revoked    []string
	synced     int
	syncErr    error
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
	f.hostsCalls++
	return f.hosts, nil
}

func (f *fakeBackend) hostsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostsCalls
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

func (f *fakeBackend) Devices() (proto.DevicesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devices, nil
}

func (f *fakeBackend) Approve(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id)
	return nil
}

func (f *fakeBackend) Revoke(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeBackend) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced++
	return f.syncErr
}

func (f *fakeBackend) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced
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
	require.Truef(t, ok, "Update returned %T, want browse.Model", tm)
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

func TestSyncNow(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	m := ready(t, f, 120, 30)

	// S flashes progress and issues a sync command.
	m, cmd := step(t, m, press("S"))
	require.Contains(t, strip(m.View()), "syncing", "S should flash syncing…")
	require.NotNil(t, cmd, "S should issue a sync command")

	// The command yields a syncDoneMsg once the (fake) cycle returns.
	sd, ok := cmd().(syncDoneMsg)
	require.True(t, ok, "S command did not yield a syncDoneMsg")
	require.NoError(t, sd.err)
	require.Equal(t, 1, f.syncCount(), "Sync should have been called exactly once")

	// Applying it flashes success and schedules a refresh (query + host list).
	m, cmd2 := step(t, m, sd)
	require.Contains(t, strip(m.View()), "synced", "after sync the view should flash ✓ synced")
	require.NotNil(t, cmd2, "sync should schedule a refresh (query + host list)")
}

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
	require.Equal(t, focusHosts, m.focus)

	// Down -> local host (index 1) => ScopeLocal, no Host filter.
	m, cmd := step(t, m, press("down"))
	if cmd != nil {
		cmd()
	}
	got := f.lastReq()
	require.Equalf(t, proto.ScopeLocal, got.Scope, "local host req = %+v, want Scope=local Host=empty", got)
	require.Emptyf(t, got.Host, "local host req = %+v, want Scope=local Host=empty", got)

	// Down again -> "beta" (index 2) => ScopeHost with that hostname.
	m, cmd = step(t, m, press("down"))
	if cmd != nil {
		cmd()
	}
	got = f.lastReq()
	require.Equalf(t, proto.ScopeHost, got.Scope, "beta host req = %+v, want Scope=host Host=beta", got)
	require.Equalf(t, "beta", got.Host, "beta host req = %+v, want Scope=host Host=beta", got)

	// Back up to "All hosts" => ScopeAll.
	m, _ = step(t, m, press("up"))
	m, cmd = step(t, m, press("up"))
	if cmd != nil {
		cmd()
	}
	require.Equal(t, proto.ScopeAll, f.lastReq().Scope)
}

func TestSearchSendsTaggedQueries(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	m := ready(t, f, 100, 24)

	m, _ = step(t, m, press("/"))
	require.True(t, m.searching, "`/` did not focus search")

	var lastCmd tea.Cmd
	for _, r := range "grep" {
		var c tea.Cmd
		m, c = step(t, m, press(string(r)))
		if c != nil {
			lastCmd = c
		}
	}
	require.NotNil(t, lastCmd, "typing produced no query command")
	_, ok := lastCmd().(queryResultMsg)
	require.True(t, ok, "typing command did not yield a queryResultMsg")
	require.Equal(t, "grep", f.lastReq().Q)
	require.GreaterOrEqualf(t, m.seq, uint64(2), "seq = %d, want it to advance per keystroke", m.seq)

	// Esc leaves search focus but keeps the filter text.
	m, _ = step(t, m, press("esc"))
	require.False(t, m.searching, "esc did not leave search focus")
	require.Equal(t, "grep", m.ti.Value())
}

func TestStaleResponseDropped(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 100, 24)

	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(mkRows("keep-two"))})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("stale-one"))})

	require.Lenf(t, m.rows, 1, "rows = %+v, want keep-two (seq 2)", m.rows)
	require.Equalf(t, "keep-two", m.rows[0].Cmd, "rows = %+v, want keep-two (seq 2)", m.rows)
	out := strip(m.View())
	require.Containsf(t, out, "keep-two", "view missing keep-two:\n%s", out)
	require.NotContainsf(t, out, "stale-one", "view shows dropped stale-one:\n%s", out)
}

func TestTableNavUpdatesDetail(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha", "bravo", "charlie"))})

	// Row 0 selected first.
	out := strip(m.View())
	require.Containsf(t, out, "/work/0", "detail should show row 0 cwd/session:\n%s", out)
	require.Containsf(t, out, "sess-0", "detail should show row 0 cwd/session:\n%s", out)

	// Move down twice: detail must reflect row 2.
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("j"))
	require.Equal(t, 2, m.sel)
	out = strip(m.View())
	require.Containsf(t, out, "/work/2", "detail should show row 2 cwd/session:\n%s", out)
	require.Containsf(t, out, "sess-2", "detail should show row 2 cwd/session:\n%s", out)
	require.NotContainsf(t, out, "sess-0", "detail still shows row 0 after navigation:\n%s", out)
}

func TestDeleteConfirmYes(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("keep-a", "drop-b", "keep-c"))})
	m, _ = step(t, m, press("down")) // select drop-b (id "1")

	m, _ = step(t, m, press("d"))
	require.True(t, m.confirmDelete, "`d` did not arm the delete confirmation")
	require.Contains(t, strip(m.View()), "delete this command?")

	m, _ = step(t, m, press("y"))
	require.Equal(t, []string{"1"}, f.deleted)
	require.Len(t, m.rows, 2)
	for _, r := range m.rows {
		require.NotEqualf(t, "drop-b", r.Cmd, "deleted row still present: %+v", m.rows)
	}
}

func TestDeleteConfirmNo(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})

	m, _ = step(t, m, press("d"))
	m, _ = step(t, m, press("n"))
	require.False(t, m.confirmDelete, "`n` did not dismiss the confirmation")
	require.Empty(t, f.deleted, "Delete should not be called on `n`")
	require.Len(t, m.rows, 2)
}

func TestAcceptOnEnter(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("echo hi"))})

	m, cmd := step(t, m, press("enter"))
	require.Equal(t, "echo hi", m.accepted, "enter should record the selected command for recall")
	require.True(t, m.quitting, "enter should quit so Run can return the pick")
	require.NotNil(t, cmd, "enter should issue a command")
	_, ok := cmd().(tea.QuitMsg)
	require.True(t, ok, "enter should return tea.Quit")
}

func TestCopyOnY(t *testing.T) {
	// Stub the local clipboard tool: without this the batched copy would spawn
	// wl-copy/xclip and overwrite the developer's real clipboard on every run.
	prev := localClipboardCopy
	localClipboardCopy = func(string) {}
	t.Cleanup(func() { localClipboardCopy = prev })

	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("echo hi"))})

	var buf bytes.Buffer
	m.out = &buf
	m, cmd := step(t, m, press("y"))
	require.Contains(t, strip(m.View()), "copied", "y should flash the copied indicator")
	require.Empty(t, m.accepted, "y copies; it must not accept the command")
	require.NotNil(t, cmd, "y should issue the copy command")

	// copySelected batches the clipboard write with the flash tick; run each
	// batched command so the OSC 52 write actually reaches out.
	batch, ok := cmd().(tea.BatchMsg)
	require.Truef(t, ok, "copy command yielded %T, want tea.BatchMsg", cmd())
	for _, c := range batch {
		if c != nil {
			c()
		}
	}
	require.Contains(t, buf.String(), osc52("echo hi"), "y should write the OSC 52 sequence to out")
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
	require.Equal(t, viewStats, m.view, "`s` did not switch to stats view")
	require.NotNil(t, cmd, "toggling stats issued no aggregation query")
	msg := cmd()
	sr, ok := msg.(statsResultMsg)
	require.Truef(t, ok, "stats command yielded %T, want statsResultMsg", msg)
	m, _ = step(t, m, sr)

	out := strip(m.View())
	require.Containsf(t, out, "Top commands", "stats missing top-commands section:\n%s", out)
	require.Containsf(t, out, "git", "stats missing top command 'git':\n%s", out)
	require.Containsf(t, out, "Commands per day", "stats missing sparkline section:\n%s", out)
	require.Containsf(t, out, "█", "stats sparkline missing a full block glyph:\n%s", out) // all rows share one day -> that bucket peaks

	require.NotNilf(t, m.stats, "top command aggregate = %+v, want git=3 first", m.stats)
	require.NotEmptyf(t, m.stats.topCmds, "top command aggregate = %+v, want git=3 first", m.stats)
	require.Equalf(t, "git", m.stats.topCmds[0].name, "top command aggregate = %+v, want git=3 first", m.stats)
	require.Equalf(t, 3, m.stats.topCmds[0].n, "top command aggregate = %+v, want git=3 first", m.stats)

	// `s` again returns to the browse view.
	m, _ = step(t, m, press("s"))
	require.Equal(t, viewBrowse, m.view, "second `s` did not return to browse")
}

func TestResizeStaysWithinWidth(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "aaaaaaaaaaaaaaaaaaaa", Count: 3}}},
		resp:  mkResp(mkRows(strings.Repeat("x", 400), "short", "another command here")),
	}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	sizes := [][2]int{{200, 50}, {120, 40}, {80, 24}, {60, 20}, {40, 15}, {24, 10}}
	for _, wh := range sizes {
		t.Run(strconv.Itoa(wh[0])+"x"+strconv.Itoa(wh[1]), func(t *testing.T) {
			m, _ = step(t, m, tea.WindowSizeMsg{Width: wh[0], Height: wh[1]})
			for _, mode := range []viewMode{viewBrowse, viewStats} {
				m.view = mode
				if mode == viewStats {
					m.stats = computeStats(f.resp.Rows, m.hosts, now)
				}
				for _, line := range strings.Split(m.View(), "\n") {
					got := lipgloss.Width(line)
					require.LessOrEqualf(t, got, wh[0], "w=%d h=%d mode=%d: line width %d > %d: %q",
						wh[0], wh[1], mode, got, wh[0], strip(line))
				}
			}
			m.view = viewBrowse
		})
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
	require.Contains(t, strip(m.View()), "remote sync not configured")

	// Tab cycles hosts -> table -> detail.
	order := []focus{focusDetail, focusHosts, focusTable}
	for _, want := range order {
		m, _ = step(t, m, press("tab"))
		require.Equal(t, want, m.focus)
	}
}

func TestQuit(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 80, 24)
	_, cmd := step(t, m, press("q"))
	require.NotNil(t, cmd, "q produced no command")
	_, ok := cmd().(tea.QuitMsg)
	require.True(t, ok, "q should return tea.Quit")
}

func TestOSC52RoundTrip(t *testing.T) {
	payload := "git commit -m 'héllo wörld' && echo done"
	seq := osc52(payload)
	require.Truef(t, strings.HasPrefix(seq, "\x1b]52;c;"), "osc52 framing wrong: %q", seq)
	require.Truef(t, strings.HasSuffix(seq, "\x07"), "osc52 framing wrong: %q", seq)
	b64 := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	got, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	require.Equal(t, payload, string(got))
}

// TestOSC52Seq covers the copy sequence framing: a bare OSC 52 when not in tmux,
// and tmux's passthrough wrapping (every inner ESC doubled, wrapped in
// \ePtmux;…\e\\) when it is.
func TestOSC52Seq(t *testing.T) {
	const payload = "echo hi"
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	tests := []struct {
		name string
		tmux bool
		want string
	}{
		{
			name: "plain OSC 52 without tmux",
			tmux: false,
			want: "\x1b]52;c;" + b64 + "\x07",
		},
		{
			name: "tmux passthrough doubles ESCs and wraps",
			tmux: true,
			want: "\x1bPtmux;\x1b" + "\x1b\x1b]52;c;" + b64 + "\x07" + "\x1b\\",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, osc52Seq(payload, tc.tmux))
		})
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
	require.True(t, m.vim, "vim mode should be enabled from Options.Keymap")

	// j/k navigate the table (these also work in emacs, but must in vim).
	m, _ = step(t, m, press("j"))
	m, _ = step(t, m, press("j"))
	require.Equal(t, 2, m.sel)
	m, _ = step(t, m, press("k"))
	require.Equal(t, 1, m.sel)

	// g/G jump to top/bottom.
	m, _ = step(t, m, press("G"))
	require.Equal(t, len(cmds)-1, m.sel)
	m, _ = step(t, m, press("g"))
	require.Equal(t, 0, m.sel)

	// h/l move focus between panes (default focus is the table).
	require.Equal(t, focusTable, m.focus, "default focus")
	m, _ = step(t, m, press("l"))
	require.Equal(t, focusDetail, m.focus)
	m, _ = step(t, m, press("h"))
	m, _ = step(t, m, press("h"))
	require.Equal(t, focusHosts, m.focus, "h twice from table")

	// ctrl+d is a half-page scroll in vim, NOT delete.
	m.focus = focusTable
	m.sel = 0
	m, _ = step(t, m, press("ctrl+d"))
	require.False(t, m.confirmDelete, "ctrl+d in vim mode wrongly armed delete confirmation")
	require.NotEqual(t, 0, m.sel, "ctrl+d in vim mode did not scroll the selection down")
	half := m.halfPage()
	require.Equal(t, half, m.sel, "ctrl+d: want half-page")
	m, _ = step(t, m, press("ctrl+u"))
	require.Equal(t, 0, m.sel, "ctrl+u: want back to 0")

	// `d` still deletes (single-key, with confirm) in vim mode.
	m, _ = step(t, m, press("d"))
	require.True(t, m.confirmDelete, "`d` should still arm delete in vim mode")
}

func TestEmacsCtrlDStillDeletes(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30) // default (emacs) keymap
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	require.False(t, m.vim, "default keymap should not be vim")
	m, _ = step(t, m, press("ctrl+d"))
	require.True(t, m.confirmDelete, "ctrl+d in emacs mode should still arm delete")
}

func TestRemoteWarmTriggersHostsRefresh(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "local-box", Count: 3}}},
		resp:  mkResp(mkRows("ls")),
	}
	m := ready(t, f, 120, 30) // ready() feeds a synthetic hosts result, no backend call yet
	require.Equal(t, 0, m.hostsRemote)
	require.Equal(t, 0, f.hostsCallCount())

	// A deep query result reports the remote cache just warmed with 2 hosts —
	// a count the sidebar hasn't seen — so the model must refetch the host list.
	resp := mkResp(mkRows("ls"))
	resp.Remote = proto.RemoteInfo{State: proto.RemoteOK, Hosts: 2}
	m, cmd := step(t, m, queryResultMsg{seq: 10, resp: resp})
	require.NotNil(t, cmd, "a newly-known remote host count should refresh the sidebar")
	msg := cmd()
	_, ok := msg.(hostsResultMsg)
	require.Truef(t, ok, "refresh command yielded %T, want hostsResultMsg", msg)
	require.Equal(t, 1, f.hostsCallCount(), "the refresh should have called Hosts()")

	// Applying a hosts result carrying that same remote count converges the model.
	f.hosts.Remote = proto.RemoteInfo{State: proto.RemoteOK, Hosts: 2}
	m, _ = step(t, m, hostsResultMsg{info: f.hosts})
	require.Equal(t, 2, m.hostsRemote)

	// A subsequent query result with the SAME remote count triggers no refetch.
	m, cmd = step(t, m, queryResultMsg{seq: 11, resp: resp})
	require.Nil(t, cmd, "matching remote count should not refetch the sidebar")
}

func TestInitStartsBoundedWarmLoop(t *testing.T) {
	f := &fakeBackend{resp: func() proto.QueryResp {
		r := mkResp(mkRows("ls"))
		r.Remote = proto.RemoteInfo{State: proto.RemoteOff}
		return r
	}()}
	m := NewModel(f, Options{Now: now})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})

	// Init starts a single warm-loop tick chain (plus the query + hosts fetch).
	m, cmd := step(t, m, initMsg{})
	require.True(t, m.ticking, "init should start the warm loop")
	require.NotNil(t, cmd, "init should batch query/hosts/tick commands")

	// The query result reports remote Off (sync not configured).
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	// A tick with remote Off stops the loop and does NOT reschedule.
	m, tcmd := step(t, m, hostsTickMsg{})
	require.False(t, m.ticking, "warm loop must stop once remote is Off")
	require.Nil(t, tcmd, "no reschedule after remote Off")

	// A stale tick from a stopped chain is a no-op.
	_, tcmd = step(t, m, hostsTickMsg{})
	require.Nil(t, tcmd, "a tick after the chain stopped must be dropped")
}

func TestWarmLoopStopsAtHardCap(t *testing.T) {
	// Remote stays "syncing" forever; the loop must still terminate at the cap.
	f := &fakeBackend{resp: func() proto.QueryResp {
		r := mkResp(mkRows("ls"))
		r.Remote = proto.RemoteInfo{State: proto.RemoteSyncing}
		return r
	}()}
	m := NewModel(f, Options{Now: now})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = step(t, m, initMsg{})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	var tcmd tea.Cmd
	for i := 0; i < hostsTickMax; i++ {
		require.Truef(t, m.ticking, "should still be ticking before cap (i=%d)", i)
		m, tcmd = step(t, m, hostsTickMsg{})
	}
	require.False(t, m.ticking, "warm loop must stop at the hard cap")
	require.Nil(t, tcmd, "no reschedule once the cap is hit")
}

// compile-time assurance the interface matches what daemon.Client provides.
var _ Backend = (*fakeBackend)(nil)

func TestDevicesPane(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		devices: proto.DevicesInfo{Devices: []proto.DeviceInfo{
			{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
			{ID: "01BBBBBBBBBBBBBBBBBBBBBBBB", Name: "server", Status: "pending", Code: "AB12-CD34"},
		}},
	}
	m := ready(t, f, 120, 30)

	// Enter the devices view; it fetches asynchronously.
	m, cmd := step(t, m, press("D"))
	require.Equal(t, viewDevices, m.view)
	require.NotNil(t, cmd, "entering devices view did not fetch")
	m, _ = step(t, m, cmd())
	require.Len(t, m.devices, 2)
	out := strip(m.View())
	require.Containsf(t, out, "laptop", "devices view missing device/code:\n%s", out)
	require.Containsf(t, out, "AB12-CD34", "devices view missing device/code:\n%s", out)

	// Move to the pending device and approve it.
	m, _ = step(t, m, press("j"))
	m, acmd := step(t, m, press("a"))
	require.NotNil(t, acmd, "approve produced no command")
	acmd()
	require.Equal(t, []string{"01BBBBBBBBBBBBBBBBBBBBBBBB"}, f.approved)

	// Revoke needs confirmation: x then y.
	m, _ = step(t, m, press("x"))
	require.NotEmpty(t, m.devConfirm, "x did not arm a revoke confirmation")
	m, rcmd := step(t, m, press("y"))
	require.NotNil(t, rcmd, "y did not trigger revoke")
	rcmd()
	require.Len(t, f.revoked, 1)

	// Esc leaves the devices view.
	m, _ = step(t, m, press("esc"))
	require.Equal(t, viewBrowse, m.view, "esc did not leave devices view")
}
