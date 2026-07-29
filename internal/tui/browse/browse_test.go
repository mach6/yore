package browse

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
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
	submitted  []rec.Record
	submitErr  error
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

func (f *fakeBackend) SubmitRecord(r rec.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, r)
	return f.submitErr
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
	require.NotEmptyf(t, m.stats.topPrograms, "top command aggregate = %+v, want git=3 first", m.stats)
	require.Equalf(t, "git", m.stats.topPrograms[0].name, "top command aggregate = %+v, want git=3 first", m.stats)
	require.Equalf(t, 3, m.stats.topPrograms[0].n, "top command aggregate = %+v, want git=3 first", m.stats)

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
					m.stats = computeStats(f.resp.Rows, f.resp.Total, now, 0)
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

// TestFrameFillsExactHeight pins the whole frame to the terminal height in every
// view. The panes compose their own borders (titledBox) to seat each pane name in
// its top rule, so a one-row arithmetic slip would not look like a bug — it would
// scroll the alt-screen by a line.
func TestFrameFillsExactHeight(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
		resp:  mkResp(mkRows("go build", "go test", "ls -la")),
	}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	m.stats = computeStats(f.resp.Rows, f.resp.Total, now, 0)

	for _, wh := range [][2]int{{200, 50}, {120, 40}, {80, 24}, {60, 20}, {40, 15}} {
		m, _ = step(t, m, tea.WindowSizeMsg{Width: wh[0], Height: wh[1]})
		for _, mode := range []viewMode{viewBrowse, viewStats, viewAgents} {
			// Each view lays its panes out differently, so the geometry has to be
			// resolved for the view being measured — exactly as switching to it does.
			m.view = mode
			m.applyLayout()
			got := lipgloss.Height(m.View())
			require.Equalf(t, wh[1], got, "%dx%d mode=%d: frame is %d rows, want %d",
				wh[0], wh[1], mode, got, wh[1])
		}
		m.view = viewBrowse
		m.applyLayout()
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

func TestRefreshPreservesCursor(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("a", "b", "c", "d", "e", "f")
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// Move the cursor down onto the 4th record.
	for i := 0; i < 3; i++ {
		m, _ = step(t, m, press("down"))
	}
	require.Equal(t, 3, m.sel)
	selID := m.rows[m.sel].ID

	// A background warm-loop refresh delivers the SAME result set: the cursor must
	// stay on its command rather than snapping back to row 0.
	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(rows)})
	require.Equalf(t, selID, m.rows[m.sel].ID, "refresh with same rows must keep the cursor on its record (sel=%d)", m.sel)
	require.Equal(t, 3, m.sel, "refresh should preserve the row index for an unchanged result set")

	// A genuinely new result set that no longer contains the selected record
	// resets the cursor to the top.
	m, _ = step(t, m, queryResultMsg{seq: 3, resp: mkResp(mkRows("x", "y", "z"))})
	require.Equal(t, 0, m.sel, "a result missing the selected record resets to row 0")
}

func TestTagFilter(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("agent-run", "ls", "vim")
	rows[0].Tag = "claude-code"
	rows[0].Tags = []string{"claude-code"} // effective tags, as the daemon resolves them
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// The tags column appears once a row carries tags; the header advertises it.
	require.True(t, m.hasTags, "a tagged row should set hasTags")
	require.True(t, m.colLayout().showTag, "the tag column should show when the result set has tags")
	out := strip(m.View())
	require.Containsf(t, out, "tag", "table header should show the tags column:\n%s", out)
	require.Containsf(t, out, "claude-code", "the tag cell should render the row's tags:\n%s", out)

	// t on the tagged row (row 0 selected) adopts its tag; buildReq carries it.
	m, cmd := step(t, m, press("t"))
	require.Equal(t, "claude-code", m.executorFilter, "t should adopt the selected row's tag")
	require.NotNil(t, cmd, "toggling the tag filter should re-issue the query")
	require.Equal(t, "claude-code", m.buildReq().Executor, "buildReq should carry the active tag filter")
	require.Containsf(t, strip(m.View()), "executor: claude-code", "status bar should show the active executor filter")

	// t again clears the filter.
	m, _ = step(t, m, press("t"))
	require.Equal(t, "", m.executorFilter, "second t should clear the tag filter")
	require.Equal(t, "", m.buildReq().Executor, "a cleared filter sends an empty Tag")
}

func TestTagRowPicker(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("git status", "ls", "vim")
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// ctrl+t on the selected row opens the tag input.
	m, _ = step(t, m, press("ctrl+t"))
	require.True(t, m.tagging, "ctrl+t enters tagging mode")

	// Type a name and submit.
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("refactor")})
	m, _ = step(t, m, press("enter"))
	require.False(t, m.tagging, "enter leaves tagging mode")

	// A TypeTag record was submitted for the selected command, and the tag shows
	// optimistically on the row.
	require.Len(t, f.submitted, 1)
	assert.Equal(t, "refactor", f.submitted[0].TagName)
	assert.Equal(t, rows[0].ID, f.submitted[0].TargetID)
	assert.Equal(t, "tag", f.submitted[0].Type)
	assert.Contains(t, m.rows[0].Tags, "refactor", "row shows the new tag at once")
	require.Contains(t, strip(m.View()), "tagged: refactor")
}

func TestTagFilterNoTag(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("ls", "vim"))})
	require.False(t, m.hasTags, "no rows carry a tag")
	require.False(t, m.colLayout().showTag, "the tag column stays hidden without tags")

	// t on an untagged row flashes "no tag" and leaves the filter empty.
	m, cmd := step(t, m, press("t"))
	require.Equal(t, "", m.executorFilter, "t on an untagged row must not set a filter")
	require.NotNil(t, cmd, "the no-tag flash should still schedule its expiry tick")
	require.Containsf(t, strip(m.View()), "no tag", "t on an untagged row should flash 'no tag'")
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

// --- agent explorer -------------------------------------------------------

// mouseAt builds a mouse event at a screen cell.
func mouseAt(x, y int, action tea.MouseAction, btn tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: action, Button: btn}
}

func click(x, y int) tea.MouseMsg {
	return mouseAt(x, y, tea.MouseActionPress, tea.MouseButtonLeft)
}

func dragTo(x, y int) tea.MouseMsg {
	return mouseAt(x, y, tea.MouseActionMotion, tea.MouseButtonLeft)
}

func mouseUp(x int) tea.MouseMsg {
	return mouseAt(x, 5, tea.MouseActionRelease, tea.MouseButtonLeft)
}

func wheelAt(x, y int, btn tea.MouseButton) tea.MouseMsg {
	return mouseAt(x, y, tea.MouseActionPress, btn)
}

// agentRows builds a sample with two executors: claude-code ran two commands
// under prompt p1, devin one (failing) under p2.
func agentSample() []rec.Record {
	rows := mkRows("cargo add tower", "cargo build", "cargo test")
	rows[0].Tag, rows[0].PromptID, rows[0].Prompt, rows[0].Session = "claude-code", "p1", "add rate limiting", "sessionAAAA1111"
	rows[1].Tag, rows[1].PromptID, rows[1].Prompt, rows[1].Session = "claude-code", "p1", "add rate limiting", "sessionAAAA1111"
	rows[2].Tag, rows[2].PromptID, rows[2].Prompt, rows[2].Session = "devin", "p2", "fix the N+1 query", "sessionBBBB2222"
	rows[2].Exit = rec.IntPtr(1)
	return rows
}

// openAgents opens the agent explorer and delivers the shared stats sample so
// every aggregation is populated.
func openAgents(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := step(t, m, press("a"))
	require.Equal(t, viewAgents, m.view, "`a` did not switch to the agent explorer")
	require.NotNil(t, cmd, "opening the explorer issued no aggregation query")
	sr, ok := cmd().(statsResultMsg)
	require.True(t, ok, "the explorer's command must yield a statsResultMsg (shared sample)")
	m, _ = step(t, m, sr)
	return m
}

func agentModel(t *testing.T, w, h int) Model {
	t.Helper()
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
		resp:  mkResp(agentSample()),
	}
	return openAgents(t, ready(t, f, w, h))
}

// TestAgentsFourPanes proves the explorer shows all four panes at once: the
// executor sidebar, the prompt list, the highlighted prompt's commands, and the
// details of whatever is selected.
func TestAgentsFourPanes(t *testing.T) {
	m := agentModel(t, 140, 40)
	out := strip(m.View())

	for _, want := range []string{"AGENTS", "PROMPTS", "COMMANDS", "DETAILS"} {
		require.Containsf(t, out, want, "pane title %q missing:\n%s", want, out)
	}
	// The sidebar lists both executors plus the "all agents" row.
	require.Len(t, m.agents.agents, 2, "two executors in the sample")
	require.Equal(t, 3, m.agentRows(), "sidebar = all-agents row + one per executor")
	require.Containsf(t, out, "claude-code", "sidebar missing an executor:\n%s", out)
	require.Containsf(t, out, "devin", "sidebar missing an executor:\n%s", out)

	// The prompt pane holds focus on open, on the newest prompt; its commands
	// (and only its commands) fill the command pane.
	require.Equal(t, apPrompts, m.apane, "the prompt pane should hold focus on open")
	require.Equal(t, 0, m.promptSel)
	require.Containsf(t, out, "add rate limiting", "prompt text missing:\n%s", out)
	require.Containsf(t, out, "cargo add tower", "command pane omitted a prompt command:\n%s", out)
	require.NotContainsf(t, out, "cargo test", "command pane leaked another prompt's command:\n%s", out)

	// The details pane describes the selected prompt.
	require.Containsf(t, out, "Executor", "details pane missing its labels:\n%s", out)
	require.Containsf(t, out, "Session", "details pane missing its labels:\n%s", out)

	// `a` again returns to browse.
	m, _ = step(t, m, press("a"))
	require.Equal(t, viewBrowse, m.view, "second `a` did not return to browse")
}

// TestAgentsPaneFocusCycles walks tab (and shift+tab) around all four panes and
// checks each pane's cursor keys act on the pane that holds focus.
func TestAgentsPaneFocusCycles(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, apPrompts, m.apane)

	for _, want := range []agentPane{apCommands, apInfo, apAgents, apPrompts} {
		m, _ = step(t, m, press("tab"))
		require.Equal(t, want, m.apane, "tab landed on the wrong pane")
	}
	m, _ = step(t, m, press("shift+tab"))
	require.Equal(t, apAgents, m.apane, "shift+tab did not go back")

	// On the sidebar, j moves the executor cursor (not the prompt cursor).
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.agentSel)
	require.Equal(t, 0, m.promptSel, "sidebar navigation must not move the prompt cursor")

	// On the command pane, j moves the command cursor.
	m.agentSel, m.agentFilter = 0, ""
	m.recomputeStats()
	m, _ = step(t, m, press("tab")) // -> prompts
	m, _ = step(t, m, press("tab")) // -> commands
	require.Equal(t, apCommands, m.apane)
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.drillSel)
}

// TestAgentsSidebarFilters proves picking an executor narrows the prompt,
// command, and details panes to that agent's work — and that the "all agents"
// row restores everything.
func TestAgentsSidebarFilters(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Len(t, m.prompts.prompts, 2, "both prompts visible with no filter")

	// Focus the sidebar and select the first executor (claude-code: 2 commands,
	// so it sorts ahead of devin).
	m, _ = step(t, m, press("shift+tab")) // prompts -> agents
	require.Equal(t, apAgents, m.apane)
	m, _ = step(t, m, press("j"))
	require.Equal(t, "claude-code", m.agentFilter)
	require.Len(t, m.prompts.prompts, 1, "only claude-code's prompt survives the filter")
	require.Equal(t, "p1", m.prompts.prompts[0].id)

	out := strip(m.View())
	require.Containsf(t, out, "add rate limiting", "the filtered prompt should still show:\n%s", out)
	require.NotContainsf(t, out, "fix the N+1 query", "the other agent's prompt leaked through:\n%s", out)

	// The next row filters to devin.
	m, _ = step(t, m, press("j"))
	require.Equal(t, "devin", m.agentFilter)
	require.Len(t, m.prompts.prompts, 1)
	require.Equal(t, "p2", m.prompts.prompts[0].id)

	// Back to the "all agents" row.
	m, _ = step(t, m, press("g"))
	require.Equal(t, 0, m.agentSel)
	require.Empty(t, m.agentFilter, "the all-agents row clears the filter")
	require.Len(t, m.prompts.prompts, 2)
}

// TestAgentsFilterSurvivesPeriodChange proves the filter is held by executor
// name: an agent that ages out of the period releases the filter instead of
// silently handing it to whichever agent inherits its row.
func TestAgentsFilterSurvivesPeriodChange(t *testing.T) {
	m := agentModel(t, 140, 40)
	m, _ = step(t, m, press("shift+tab"))
	m, _ = step(t, m, press("j"))
	m, _ = step(t, m, press("j"))
	require.Equal(t, "devin", m.agentFilter)

	// A wider period keeps devin, and the selection stays on it.
	m, _ = step(t, m, press("5"))
	require.Equal(t, "devin", m.agentFilter, "devin is still present in the widest period")

	// Age every devin row out; the filter falls back to all agents.
	rows := agentSample()
	rows[2].StartMs = now - 400*86_400_000
	m.statsRows = rows
	m, _ = step(t, m, press("1")) // Today
	require.Empty(t, m.agentFilter, "a vanished executor must release the filter")
	require.Equal(t, 0, m.agentSel)
}

// TestAgentsDetailsFollowFocus proves the details pane describes the prompt while
// the prompt pane is focused and the command once the command pane is.
func TestAgentsDetailsFollowFocus(t *testing.T) {
	m := agentModel(t, 140, 40)
	out := strip(m.View())
	require.Containsf(t, out, "Prompt", "details should head with the prompt:\n%s", out)
	require.Containsf(t, out, "Commands", "prompt details should carry the command count:\n%s", out)

	m, _ = step(t, m, press("tab")) // -> commands
	require.Equal(t, apCommands, m.apane)
	out = strip(m.View())
	require.Containsf(t, out, "Command", "details should head with the command:\n%s", out)
	require.Containsf(t, out, "Exit", "command details should carry the exit status:\n%s", out)
	// The command pane runs oldest-first (the agent's working order), so the
	// cursor starts on `cargo build` — the details pane must track that, not the
	// newest row.
	require.Containsf(t, out, "cargo build", "command details should describe the selected command:\n%s", out)
	require.Containsf(t, out, "/work/1", "command details should carry the selected command's cwd:\n%s", out)
}

// TestAgentsZoom expands the focused pane to the whole frame and restores it.
func TestAgentsZoom(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Containsf(t, strip(m.View()), "AGENTS", "the sidebar should be visible while tiled")

	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)
	out := strip(m.View())
	require.Containsf(t, out, "PROMPTS", "the zoomed pane must still render:\n%s", out)
	require.NotContainsf(t, out, "COMMANDS", "a zoomed pane must be the only one drawn:\n%s", out)
	require.NotContainsf(t, out, "DETAILS", "a zoomed pane must be the only one drawn:\n%s", out)
	require.Containsf(t, out, "zoomed", "the status bar should say the view is zoomed:\n%s", out)

	// The zoomed pane fills the middle region.
	require.Equal(t, 0, m.geo.p[apPrompts].x)
	require.Equal(t, m.width, m.geo.p[apPrompts].w)
	require.Equal(t, m.midHeight, m.geo.p[apPrompts].h)

	// Tab moves the zoom with focus rather than dropping back to the tiles.
	m, _ = step(t, m, press("tab"))
	require.True(t, m.zoom, "tab should keep the zoom")
	out = strip(m.View())
	require.Containsf(t, out, "COMMANDS", "zoom did not follow focus:\n%s", out)
	require.NotContainsf(t, out, "PROMPTS", "zoom did not follow focus:\n%s", out)

	// Esc unzooms first; only a second esc leaves the view.
	m, _ = step(t, m, press("esc"))
	require.False(t, m.zoom, "esc should unzoom")
	require.Equal(t, viewAgents, m.view, "the first esc must not also leave the view")
	m, _ = step(t, m, press("esc"))
	require.Equal(t, viewBrowse, m.view)
}

// TestBrowseZoom proves the same z key expands a browse pane.
func TestBrowseZoom(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	require.Containsf(t, strip(m.View()), "HOSTS", "the host sidebar should be visible while tiled")

	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)
	out := strip(m.View())
	require.NotContainsf(t, out, "HOSTS", "zooming the table must hide the sidebar:\n%s", out)
	require.Containsf(t, out, "cargo build", "the zoomed table must still render its rows:\n%s", out)
	require.Equal(t, m.width, m.geo.p[focusTable].w)

	m, _ = step(t, m, press("esc"))
	require.False(t, m.zoom, "esc should unzoom the browse view")
	require.Containsf(t, strip(m.View()), "HOSTS", "unzooming should bring the sidebar back")
}

// TestDragResizesPanes drags the vertical seam and proves the sidebar follows
// the pointer, that the ratio survives a terminal resize, and that the drag
// clamps rather than collapsing a pane.
func TestDragResizesPanes(t *testing.T) {
	m := agentModel(t, 140, 40)
	seam := m.geo.vDiv
	require.Positive(t, seam, "the explorer should have a vertical seam")

	// Grab the seam and drag it right.
	m, _ = step(t, m, click(seam, 5))
	require.Equal(t, dragVert, m.drag, "clicking the seam did not start a drag")
	m, _ = step(t, m, dragTo(60, 5))
	require.Equal(t, 60, m.geo.vDiv, "the seam did not follow the pointer")
	m, _ = step(t, m, mouseUp(60))
	require.Equal(t, dragNone, m.drag, "releasing did not end the drag")

	// The split is held as a ratio, so a resize keeps the proportion.
	require.Equal(t, ratioOf(60, 140), m.splits.AgentLeft)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 280, Height: 40})
	require.Equal(t, 120, m.geo.vDiv, "the split ratio did not survive a resize")

	// Dragging past the edge clamps instead of collapsing the right-hand panes.
	m, _ = step(t, m, click(m.geo.vDiv, 5))
	m, _ = step(t, m, dragTo(279, 5))
	require.LessOrEqual(t, m.splits.AgentLeft, maxColRatio, "the seam must clamp")
	require.Less(t, m.geo.vDiv, m.width-minPaneCols, "the right-hand panes must stay usable")

	// The horizontal seam is draggable too.
	m, _ = step(t, m, mouseUp(279))
	hs := m.geo.hDiv
	m, _ = step(t, m, click(m.geo.vDiv+20, hs))
	require.Equal(t, dragHoriz, m.drag, "clicking the horizontal seam did not start a drag")
	m, _ = step(t, m, dragTo(m.geo.vDiv+20, 30))
	require.Equal(t, 30, m.geo.hDiv, "the horizontal seam did not follow the pointer")
}

// TestDragResizesBrowsePanes proves the browse view's sidebar is draggable and
// that, until it is dragged, the layout is exactly the one it has always had.
func TestDragResizesBrowsePanes(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	m := ready(t, f, 120, 30)
	require.Equal(t, leftWidth, m.leftW, "an undragged sidebar keeps the default width")
	require.Zero(t, m.splits.BrowseLeft, "no drag means no stored ratio")

	m, _ = step(t, m, click(m.geo.vDiv, 5))
	m, _ = step(t, m, dragTo(40, 5))
	require.Equal(t, 40, m.leftW, "the browse sidebar did not follow the pointer")
	require.Equal(t, 40, m.geo.vDiv)
	require.Equal(t, m.width-40-2, m.tableWidth, "the table content width must track the sidebar")
}

// TestMouseClickAndWheel proves a click focuses the pane under the pointer and
// the wheel scrolls a pane without stealing focus from another.
func TestMouseClickAndWheel(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, apPrompts, m.apane)

	// Click well inside the sidebar (away from the seam).
	m, _ = step(t, m, click(2, 3))
	require.Equal(t, apAgents, m.apane, "clicking the sidebar did not focus it")

	// Click inside the command pane.
	cmds := m.geo.p[apCommands]
	m, _ = step(t, m, click(cmds.x+10, cmds.y+3))
	require.Equal(t, apCommands, m.apane, "clicking the command pane did not focus it")

	// The wheel scrolls the pane under the pointer, leaving focus alone.
	prompts := m.geo.p[apPrompts]
	m, _ = step(t, m, wheelAt(prompts.x+10, prompts.y+3, tea.MouseButtonWheelDown))
	require.Equal(t, 1, m.promptSel, "the wheel did not scroll the prompt pane")
	require.Equal(t, apCommands, m.apane, "the wheel must not move focus")
	m, _ = step(t, m, wheelAt(prompts.x+10, prompts.y+3, tea.MouseButtonWheelUp))
	require.Equal(t, 0, m.promptSel, "the wheel did not scroll back up")
}

// TestZoomedPaneKeepsMouse proves the wheel still scrolls a zoomed pane. A
// zoomed layout parks its one full-frame rect at the zoomed pane's OWN index, so
// a hit test bounded by a pane count matched nothing unless that index happened
// to be 0 — and neither view zooms to pane 0 by default.
func TestZoomedPaneKeepsMouse(t *testing.T) {
	// Browse: zoom the results table (index 1) and scroll it.
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 4}}},
		resp:  mkResp(mkRows("cargo add tower", "cargo build", "cargo test", "cargo fmt")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)
	require.NotEqual(t, focus(0), m.focus, "this test is only meaningful off index 0")
	require.Equal(t, 0, m.sel)

	m, _ = step(t, m, wheelAt(10, 5, tea.MouseButtonWheelDown))
	require.Positivef(t, m.sel, "the wheel must still scroll a zoomed browse pane")
	down := m.sel
	m, _ = step(t, m, wheelAt(10, 5, tea.MouseButtonWheelUp))
	require.Lessf(t, m.sel, down, "the wheel must still scroll a zoomed browse pane back up")

	// A click inside the zoomed pane is hit-tested too, and keeps focus on it.
	m, _ = step(t, m, click(10, 5))
	require.Equal(t, focusTable, m.focus)
	require.True(t, m.zoom, "clicking inside a zoomed pane must not drop the zoom")

	// Agents: the same, on the zoomed prompt pane (also not index 0).
	am := agentModel(t, 140, 40)
	am, _ = step(t, am, press("z"))
	require.True(t, am.zoom)
	require.NotEqual(t, agentPane(0), am.apane, "this test is only meaningful off index 0")
	require.Equal(t, 0, am.promptSel)
	am, _ = step(t, am, wheelAt(10, 5, tea.MouseButtonWheelDown))
	require.Positivef(t, am.promptSel, "the wheel must still scroll a zoomed agent pane")
}

// TestAgentsDurColumnAdapts keeps the adaptive DUR column honest across the
// prompt and command panes.
func TestAgentsDurColumnAdapts(t *testing.T) {
	// No command carries a duration (the shape Claude Code's hook payload has, on
	// its own), so the DUR column is dropped from both panes.
	rows := mkRows("go build", "go test")
	for i := range rows {
		rows[i].Tag, rows[i].PromptID, rows[i].Prompt = "claude-code", "p1", "ship it"
	}
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 2}}},
		resp:  mkResp(rows),
	}
	m := openAgents(t, ready(t, f, 140, 40))
	require.False(t, m.prompts.hasDur, "no command has a duration")
	require.NotContainsf(t, strip(m.View()), " dur ", "the dur column should be hidden when nothing is timed")

	// A duration on any one command brings the column back (e.g. a Cursor
	// command mixed into the same view).
	rows[1].DurMs = rec.Int64Ptr(1200)
	m.statsRows = rows
	// Round-trip the period to force a re-aggregation of the held sample; the
	// widest window is already selected, so asking for it again is a no-op.
	m, _ = step(t, m, press("1"))
	m, _ = step(t, m, press("5"))
	require.True(t, m.prompts.hasDur, "a timed command should light the dur column")
	require.Containsf(t, strip(m.View()), " dur ", "the dur column should return once a command is timed")
}

// TestAgentsHorizontalScroll proves ←/→ reveal a prompt truncated with an
// ellipsis, and that the offset resets on any other move.
func TestAgentsHorizontalScroll(t *testing.T) {
	long := "add rate limiting to the authentication middleware so that every request " +
		"path is throttled per client without dropping legitimate bursts ZZZEND"
	rows := mkRows("go build", "go test")
	for i := range rows {
		rows[i].Tag, rows[i].PromptID, rows[i].Prompt = "claude-code", "p1", long
	}
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 2}}},
		resp:  mkResp(rows),
	}
	// A narrow window guarantees the prompt text is clipped in its pane.
	m := openAgents(t, ready(t, f, 70, 30))
	require.Equal(t, 0, m.hscroll)
	require.NotContainsf(t, strip(m.View()), "ZZZEND", "the far end should start off-screen")

	for i := 0; i < 30 && m.hscroll < m.maxHScroll(); i++ {
		m, _ = step(t, m, press("right"))
	}
	require.Positive(t, m.hscroll, "→ should advance the horizontal offset")
	require.Containsf(t, strip(m.View()), "ZZZEND", "→ did not reveal the truncated tail:\n%s", strip(m.View()))

	// Any vertical move resets the offset back to the line start.
	m, _ = step(t, m, press("k"))
	require.Equal(t, 0, m.hscroll, "moving the cursor should reset horizontal scroll")

	// ← never drives the offset negative.
	m, _ = step(t, m, press("left"))
	require.Equal(t, 0, m.hscroll)
}

// TestAgentsNavigationAndPeriod covers prompt-cursor clamping and the period
// tabs re-aggregating the held sample.
func TestAgentsNavigationAndPeriod(t *testing.T) {
	rows := mkRows("a", "b")
	for i := range rows {
		rows[i].Tag = "claude-code"
	}
	rows[0].PromptID, rows[0].Prompt = "p1", "newer prompt"
	rows[1].PromptID, rows[1].Prompt = "p2", "older prompt"
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 2}}},
		resp:  mkResp(rows),
	}
	m := openAgents(t, ready(t, f, 140, 40))
	require.Len(t, m.prompts.prompts, 2)

	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.promptSel)
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.promptSel, "selection must clamp at the last prompt")
	m, _ = step(t, m, press("k"))
	require.Equal(t, 0, m.promptSel)
	m, _ = step(t, m, press("G"))
	require.Equal(t, 1, m.promptSel, "G jumps to the last prompt")

	// A narrow period that excludes everything empties the list without leaving a
	// dangling selection.
	rows[0].StartMs = now - 10*86_400_000
	rows[1].StartMs = now - 10*86_400_000
	m.statsRows = rows
	m, _ = step(t, m, press("1")) // Today
	require.Empty(t, m.prompts.prompts, "the period should exclude the aged prompts")
	require.Equal(t, 0, m.promptSel, "selection must clamp when the list empties")
	require.Containsf(t, strip(m.View()), "No agent prompts", "the empty pane should say so")
}

// TestAgentsNarrowTerminalStaysWithinWidth proves the four-pane grid still tiles
// on a small terminal: every rendered line fits the frame, no pane collapses
// below its minimum, and the panes still sum to the full width and height.
func TestAgentsNarrowTerminalStaysWithinWidth(t *testing.T) {
	m := agentModel(t, 60, 18)
	for _, line := range strings.Split(m.View(), "\n") {
		require.LessOrEqualf(t, lipgloss.Width(line), 60, "line overflows the terminal: %q", strip(line))
	}
	g := m.geo
	require.GreaterOrEqual(t, g.p[apAgents].w, minPaneCols, "the sidebar must stay usable")
	require.GreaterOrEqual(t, g.p[apPrompts].w, minPaneCols, "the prompt pane must stay usable")
	require.GreaterOrEqual(t, g.p[apCommands].h, minPaneRows, "the command pane must stay usable")
	require.Equal(t, 60, g.p[apAgents].w+g.p[apPrompts].w, "the columns must sum to the width")
	require.Equal(t, m.midHeight, g.p[apPrompts].h+g.p[apCommands].h, "the rows must sum to the height")
}

// --- pane headers, persistence, and the width-adaptive stats graphs ---------

// TestBrowsePaneHeaders proves each browse pane carries the same kind of title
// the agent explorer's do, with its count/position.
func TestBrowsePaneHeaders(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 2}}},
		resp:  mkResp(mkRows("cargo build", "cargo test")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	out := strip(m.View())
	require.Containsf(t, out, "HOSTS  1", "host pane title should carry the real host count:\n%s", out)
	require.Containsf(t, out, "COMMANDS  1/2", "table pane title should carry the cursor position:\n%s", out)
	require.Containsf(t, out, "DETAILS", "detail pane title missing:\n%s", out)

	// The position tracks the cursor.
	m, _ = step(t, m, press("j"))
	require.Containsf(t, strip(m.View()), "COMMANDS  2/2", "the title should follow the cursor")
}

// TestAgentsKeyIsOnlyA proves `p` no longer opens the explorer — `a` owns it.
func TestAgentsKeyIsOnlyA(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
		resp:  mkResp(agentSample()),
	}
	m := ready(t, f, 120, 30)
	m, cmd := step(t, m, press("p"))
	require.Equal(t, viewBrowse, m.view, "`p` must no longer switch views")
	require.Nil(t, cmd, "`p` must not issue a query")
}

// TestSplitsPersistAndRestore proves a dragged layout is handed to SaveSplits
// once the drag settles, and that Options.Splits restores it on the next run.
func TestSplitsPersistAndRestore(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	var saved []Splits
	m := NewModel(f, Options{
		Version:    "v1",
		Now:        now,
		SaveSplits: func(s Splits) error { saved = append(saved, s); return nil },
	})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})

	// Mid-drag nothing is written; the file should only ever hold a layout the
	// user stopped on.
	m, _ = step(t, m, click(m.geo.vDiv, 5))
	m, cmd := step(t, m, dragTo(40, 5))
	require.Nil(t, cmd, "a motion event must not persist")
	require.Empty(t, saved)

	m, cmd = step(t, m, mouseUp(40))
	require.NotNil(t, cmd, "releasing a drag should persist the layout")
	cmd()
	require.Len(t, saved, 1)
	require.Equal(t, ratioOf(40, 120), saved[0].BrowseLeft)

	// A release that ends no drag writes nothing.
	_, cmd = step(t, m, mouseUp(40))
	require.Nil(t, cmd, "a stray release must not persist")

	// A fresh model restores that layout.
	m2 := NewModel(f, Options{Version: "v1", Now: now, Splits: saved[0]})
	m2, _ = step(t, m2, tea.WindowSizeMsg{Width: 120, Height: 30})
	require.Equal(t, 40, m2.leftW, "the remembered sidebar width was not restored")

	// The zero value leaves the browse view on its long-standing default and
	// still gives the agent explorer sensible proportions.
	m3 := NewModel(f, Options{Version: "v1", Now: now})
	m3, _ = step(t, m3, tea.WindowSizeMsg{Width: 120, Height: 30})
	require.Equal(t, leftWidth, m3.leftW, "an unremembered browse layout keeps its default")
	require.Equal(t, defaultAgentLeftRatio, m3.splits.AgentLeft)
}

// statsModel renders the stats screen over a year-plus of daily activity, on a
// terminal tall enough for every chart.
func statsModel(t *testing.T, w int) Model {
	t.Helper()
	return statsModelWH(t, w, 40)
}

// statsModelWH is statsModel at an explicit terminal size, for the fitting rules.
func statsModelWH(t *testing.T, w, h int) Model {
	t.Helper()
	var rows []rec.Record
	for i := 0; i < 800; i++ {
		rows = append(rows, rec.Record{
			ID: strconv.Itoa(i), Cmd: "go build", Cwd: "/work", Hostname: "boxA",
			StartMs: now - int64(i)*6*3_600_000, Exit: rec.IntPtr(0),
		})
	}
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: len(rows)}}},
		resp:  mkResp(rows),
	}
	m := ready(t, f, w, h)
	m, cmd := step(t, m, press("s"))
	require.NotNil(t, cmd)
	sr, ok := cmd().(statsResultMsg)
	require.True(t, ok)
	m, _ = step(t, m, sr)
	return m
}

// TestStatsHeatmapSurvivesShortTerminal: the activity graph is the one view that
// shows years at a glance, and it used to be gated on a pane 26 rows tall — so on
// a stock 80×24 terminal it never appeared at all. It must compress instead.
func TestStatsHeatmapSurvivesShortTerminal(t *testing.T) {
	full := strip(statsModelWH(t, 100, 40).View())
	require.Contains(t, full, "peak", "a tall terminal gets a heatmap")
	require.Contains(t, full, "Sun", "and the full form carries weekday rows")

	short := strip(statsModelWH(t, 100, 24).View())
	require.Contains(t, short, "Activity (last", "24 rows must still show the activity arc")
	require.NotContains(t, short, "Sun", "at 24 rows it is the compact, folded form")
}

// TestStatsChartsFitWholeOrNotAtAll: a chart that cannot fit yields its rows
// instead of being drawn and then sliced, which used to lop the axis off the
// hourly histogram on a short terminal.
func TestStatsChartsFitWholeOrNotAtAll(t *testing.T) {
	for h := 10; h <= 40; h++ {
		out := strip(statsModelWH(t, 100, h).View())
		if strings.Contains(out, "By hour of day") {
			require.Containsf(t, out, "00 ", "hourly chart drawn without its axis at h=%d:\n%s", h, out)
		}
		if strings.Contains(out, "Commands per day") {
			require.Containsf(t, out, "peak", "daily chart drawn without its caption at h=%d", h)
		}
		// Whatever was dropped, the ranked columns keep their heading and rows.
		require.Containsf(t, out, "Top programs", "the ranked columns were starved at h=%d", h)
	}
}

// statLineWidth returns the display width of the first rendered line whose
// stripped text starts with prefix.
func statLineWidth(t *testing.T, m Model, prefix string) int {
	t.Helper()
	for _, line := range strings.Split(m.View(), "\n") {
		if strings.HasPrefix(strip(line), prefix) {
			return lipgloss.Width(strings.TrimRight(strip(line), " "))
		}
	}
	require.FailNowf(t, "line not found", "no rendered line starts with %q:\n%s", prefix, strip(m.View()))
	return 0
}

// TestStatsGraphsFillWidth proves the three graphs span the terminal and grow
// with it, rather than sitting at a fixed narrow size.
func TestStatsGraphsFillWidth(t *testing.T) {
	narrow := statsModel(t, 100)
	wide := statsModel(t, 180)

	for _, tc := range []struct{ name, prefix string }{
		{"heatmap", "Sun "},
		{"daily trend", "    █"},
	} {
		nw := statLineWidth(t, narrow, tc.prefix)
		ww := statLineWidth(t, wide, tc.prefix)
		require.Greaterf(t, nw, 90, "%s should fill a 100-col terminal, got %d", tc.name, nw)
		require.Greaterf(t, ww, nw, "%s should grow with the terminal (%d -> %d)", tc.name, nw, ww)
		require.LessOrEqualf(t, ww, 180, "%s overflowed the terminal at %d", tc.name, ww)
	}

	// The heatmap shows more weeks of history on the wider terminal.
	require.Equal(t, (100-heatLabelW)/heatCellW, heatWeeks(100))
	require.Greater(t, heatWeeks(180), heatWeeks(100))
	require.Containsf(t, strip(wide.View()), "By hour of day", "the hourly chart should still render")

	// The hourly histogram widens its 24 fixed buckets instead of staying 24
	// cells wide.
	hourW := statLineWidth(t, wide, "    "+string(sparkBlocks[7]))
	if hourW == 0 {
		hourW = statLineWidth(t, wide, "    ·")
	}
	require.Greaterf(t, hourW, 24, "the hourly buckets should be widened, got %d", hourW)
}

// TestStatsGraphsIgnorePeriod proves the long-arc graphs keep their shape when
// the period narrows — they exist to show activity AROUND the window.
func TestStatsGraphsIgnorePeriod(t *testing.T) {
	m := statsModel(t, 140)
	wide := strip(m.View())
	require.Contains(t, wide, "Activity (last")

	m, _ = step(t, m, press("1")) // Today
	today := strip(m.View())
	require.Contains(t, today, "Activity (last", "the heatmap must survive a narrow period")
	require.Containsf(t, today, string(heatFill), "the heatmap must still show history:\n%s", today)
	// The KPI line, by contrast, does narrow to the period.
	require.NotEqual(t, wide, today, "the period must still change something")
}

// TestHourAxisAlignsToBuckets pins the hour ruler to its buckets and proves it
// never overflows.
func TestHourAxisAlignsToBuckets(t *testing.T) {
	for _, w := range []int{24, 48, 96, 144, 200} {
		axis := hourAxis(w)
		require.Equalf(t, w, lipgloss.Width(axis), "the axis must be exactly w columns at w=%d", w)
		require.Truef(t, strings.HasPrefix(axis, "00"), "the axis should start at hour 00 (w=%d): %q", w, axis)
		// Hour 12's label starts exactly at hour 12's bucket.
		at := bucketStart(w, 24, 12)
		if strings.Contains(axis, "12") {
			require.Equalf(t, "12", axis[at:at+2],
				"hour 12's label is off its bucket at w=%d: %q", w, axis)
		}
	}
	// A cramped axis still labels what it can, at the same bucket positions the
	// bars use…
	require.Equal(t, "00 03 06 09 ", hourAxis(12))
	// …and blanks out entirely rather than emitting a truncated label when even
	// one will not fit.
	require.Equal(t, "  ", hourAxis(2))
	require.Empty(t, hourAxis(0))
}

// TestHeatmapDropsWhenTooNarrow proves the heatmap yields its rows rather than
// rendering a stub on a very narrow terminal.
func TestHeatmapDropsWhenTooNarrow(t *testing.T) {
	require.Less(t, heatWeeks(minHeatWks*heatCellW+heatLabelW-1), minHeatWks)
	m := statsModel(t, 100)
	for _, compact := range []bool{false, true} {
		require.Nilf(t, m.heatmapChart(m.stats, 16, compact),
			"a 16-column heatmap is not worth its rows (compact=%v)", compact)
	}
	require.NotEmpty(t, m.heatmapChart(m.stats, 100, false))
	require.Less(t, len(m.heatmapChart(m.stats, 100, true)),
		len(m.heatmapChart(m.stats, 100, false)), "the compact form must be shorter")
}

// --- the shared period filter ---------------------------------------------

// TestPeriodFiltersBrowseTable proves the browse view now HAS a time filter and
// that it narrows the table, which it never used to.
func TestPeriodFiltersBrowseTable(t *testing.T) {
	rows := mkRows("today-cmd", "week-old", "year-old")
	rows[1].StartMs = now - 3*86_400_000   // 3 days ago
	rows[2].StartMs = now - 300*86_400_000 // ~10 months ago
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
		resp:  mkResp(rows),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	// The default window hides nothing.
	require.Equal(t, allPeriod, m.period, "the browser opens on All")
	require.Len(t, m.rows, 3)
	require.Containsf(t, strip(m.View()), "year-old", "All must show everything")

	// 7d drops the year-old row but keeps the 3-day-old one.
	m, _ = step(t, m, press("2"))
	require.Len(t, m.rows, 2, "7d should keep today's and the 3-day-old command")
	out := strip(m.View())
	require.Contains(t, out, "week-old")
	require.NotContainsf(t, out, "year-old", "7d must hide the year-old command:\n%s", out)
	require.Containsf(t, out, "1 older hidden", "the status bar should say what the window hides:\n%s", out)

	// Today keeps only today's.
	m, _ = step(t, m, press("1"))
	require.Len(t, m.rows, 1)
	require.Equal(t, 1, m.total, "the row count must reflect the filter, not the server total")

	// Back to All restores everything, including the daemon's own total.
	m, _ = step(t, m, press("5"))
	require.Len(t, m.rows, 3)
	require.Equal(t, m.srvTotal, m.total, "All hands the daemon's total back")
}

// TestPeriodIsSharedAcrossViews proves one window drives every view, so the
// 1..5 keys mean the same thing wherever they are pressed.
func TestPeriodIsSharedAcrossViews(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, allPeriod, m.period)

	// Set it in the agent explorer…
	m, _ = step(t, m, press("2"))
	require.Equal(t, 1, m.period)

	// …it holds in the stats screen…
	m, _ = step(t, m, press("s"))
	require.Equal(t, viewStats, m.view)
	require.Equal(t, 1, m.period)
	require.Containsf(t, strip(m.View()), "2 7d", "the stats header carries the same tabs")

	// …and setting it there is visible back in browse.
	m, _ = step(t, m, press("3"))
	m, _ = step(t, m, press("esc"))
	require.Equal(t, viewBrowse, m.view)
	require.Equal(t, 2, m.period)
	require.Containsf(t, strip(m.View()), "3 30d", "the browse view advertises the same tabs")
}

// TestTodayIsCalendarDay pins "Today" to the local calendar day rather than a
// rolling 24 hours — otherwise the same clock hour appears twice in the
// hour-of-day histogram, from two different days.
func TestTodayIsCalendarDay(t *testing.T) {
	midnight := periodCutoff(now, 1)
	require.Equal(t, 0, time.UnixMilli(midnight).Hour(), "Today starts at local midnight")
	require.LessOrEqual(t, midnight, now)
	require.Greater(t, midnight, now-86_400_000, "…which is later than a rolling 24h window")

	// The wider windows stay rolling, and All is unbounded.
	require.Equal(t, now-7*86_400_000, periodCutoff(now, 7))
	require.Zero(t, periodCutoff(now, 0))
}

// TestHourOfDayOnPartialDay is the heart of it: on Today the chart must show the
// hours that HAVE happened, and leave the rest blank rather than drawing them as
// zero — "it isn't 11pm yet" is not "nothing ran at 11pm".
func TestHourOfDayOnPartialDay(t *testing.T) {
	// Commands every 20 minutes through the morning, up to `now` (14:13 local).
	var rows []rec.Record
	midnight := periodCutoff(now, 1)
	for ms := midnight; ms <= now; ms += 20 * 60_000 {
		rows = append(rows, rec.Record{
			ID: strconv.FormatInt(ms, 10), Cmd: "go build", Cwd: "/w", Hostname: "boxA",
			StartMs: ms, Exit: rec.IntPtr(0),
		})
	}
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: len(rows)}}},
		resp:  mkResp(rows),
	}
	m := ready(t, f, 120, 40)
	m, cmd := step(t, m, press("s"))
	m, _ = step(t, m, cmd().(statsResultMsg))
	m, _ = step(t, m, press("1")) // Today

	require.Positive(t, m.stats.total, "today's commands must survive the Today window")
	require.Positive(t, m.stats.hourMax, "the hour buckets must be populated")

	elapsed := m.hoursElapsed()
	require.Equal(t, hourOfDay(now)+1, elapsed, "every hour up to the current one is live")
	for h := 0; h < elapsed; h++ {
		require.Positivef(t, m.stats.hourly[h], "hour %02d ran commands but is empty", h)
	}

	// The bar row draws the elapsed hours and leaves the rest blank.
	// Every glyph here is one column wide, so a rune index is a column index.
	bars := []rune(strip(hourBars(m.th, m.stats.hourly, m.stats.hourMax, 96, elapsed)))
	require.Len(t, bars, 96, "the row must be exactly as wide as it was given")
	cut := bucketStart(96, 24, elapsed)
	require.NotContainsf(t, string(bars[:cut]), " ",
		"elapsed hours must be drawn, not blank: %q", string(bars[:cut]))
	require.Equal(t, strings.Repeat(" ", 96-cut), string(bars[cut:]),
		"hours that have not happened yet must be blank, not zero-dots")

	// A wider window covers whole days, so all 24 hours are live.
	m, _ = step(t, m, press("5"))
	require.Equal(t, 24, m.hoursElapsed())

	// The title says how many commands it is drawing, so an empty chart explains
	// itself instead of just looking broken.
	require.Containsf(t, strip(m.View()), "By hour of day · ", "the hourly title should carry its count")
}

// TestSampleNoteExplainsFlatTabs proves a short aggregation says so. The
// explorer asks for every row, so this should never fire in practice — but if a
// sample ever does arrive windowed, every period wider than its reach shows
// identical numbers and the tabs read as broken unless the header owns up to it.
func TestSampleNoteExplainsFlatTabs(t *testing.T) {
	// A sample the daemon windowed: 300 rows out of a matched 9000, reaching
	// back only a couple of hours.
	const sample = 300
	rows := make([]rec.Record, sample)
	for i := range rows {
		rows[i] = rec.Record{
			ID: strconv.Itoa(i), Cmd: "ls", Cwd: "/w", Hostname: "boxA",
			StartMs: now - int64(i)*30_000, Exit: rec.IntPtr(0),
		}
	}
	resp := mkResp(rows)
	resp.Total = 9000 // matched far more than it returned
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: resp.Total}}},
		resp:  resp,
	}
	m := ready(t, f, 160, 40)
	m, cmd := step(t, m, press("s"))
	m, _ = step(t, m, cmd().(statsResultMsg))

	require.True(t, m.stats.capped, "a windowed sample must know it is short")
	require.Equal(t, sample, m.stats.sampleN)
	require.Positive(t, m.stats.sampleFrom)
	out := strip(m.View())
	require.Containsf(t, out, "newest 300 commands", "the header must own up to the window:\n%s", out)
	require.Containsf(t, out, "reaches back", "…and say how far back it actually goes:\n%s", out)

	// A complete sample makes no such claim.
	small := statsModel(t, 160)
	require.False(t, small.stats.capped)
	require.Contains(t, strip(small.View()), "all history")
}

// TestBrowseAsksForEveryRow pins the contract that makes the explorer an
// explorer: both the table and the aggregation ask the daemon for all matches,
// not a page of them. A row budget here is invisible in the UI and silently puts
// the oldest history out of scrolling reach.
func TestBrowseAsksForEveryRow(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
		resp:  mkResp(mkRows("go build", "go test", "ls -la")),
	}
	m := ready(t, f, 120, 40)

	require.Equal(t, proto.LimitAll, m.buildReq().Limit, "the browse table must not window its query")

	// The stats/agents sample comes from statsCmd; run it and read what it asked.
	_ = m.statsCmd(1)()
	require.Equal(t, proto.LimitAll, f.lastReq().Limit, "the aggregation must not window its query")
	require.True(t, f.lastReq().WantPrompts)
}

// TestStartViewOpensDirectly proves Options.Start lands on a full-screen view
// and fetches the aggregation sample the `s`/`a` keys would have fetched — this
// is what `yore stats` and `yore agents` ride on.
func TestStartViewOpensDirectly(t *testing.T) {
	for _, tc := range []struct {
		start StartView
		view  viewMode
		want  string
	}{
		{StartStats, viewStats, "STATS"},
		{StartAgents, viewAgents, "AGENTS"},
	} {
		f := &fakeBackend{
			hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}},
			resp:  mkResp(agentSample()),
		}
		m := NewModel(f, Options{Version: "v1", Now: now, Start: tc.start})
		require.Equalf(t, tc.view, m.view, "Start=%q did not open its view", tc.start)

		m, _ = step(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
		// Init must carry the stats query, or the view opens permanently blank.
		var gotSample bool
		mm, cmd := step(t, m, initMsg{})
		require.NotNil(t, cmd)
		for _, msg := range collect(cmd) {
			if sr, ok := msg.(statsResultMsg); ok {
				gotSample = true
				mm, _ = step(t, mm, sr)
			}
		}
		require.Truef(t, gotSample, "Start=%q issued no aggregation query", tc.start)
		require.Containsf(t, strip(mm.View()), tc.want, "Start=%q rendered the wrong view", tc.start)

		// Esc still drops through to the browse table.
		mm, _ = step(t, mm, press("esc"))
		require.Equal(t, viewBrowse, mm.view, "esc should reach the browse table")
	}

	// An unrecognised value costs nothing: the browse table, as always.
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	require.Equal(t, viewBrowse, NewModel(f, Options{Start: "nope"}).view)
}

// collect flattens a tea.Cmd into the messages it produces, following one level
// of tea.Batch (which is how Init returns its several commands).
func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			if c != nil {
				out = append(out, c())
			}
		}
		return out
	}
	return []tea.Msg{msg}
}

// TestPeriodTabsPinnedRight proves every period-aware view puts the tab strip in
// the same place — hard against the right edge — so the filter never moves.
func TestPeriodTabsPinnedRight(t *testing.T) {
	const w = 150
	m := agentModel(t, w, 40)

	headerOf := func(m Model) string {
		return strings.TrimRight(strip(strings.SplitN(m.View(), "\n", 2)[0]), " ")
	}
	for _, tc := range []struct {
		name string
		key  string
		view viewMode
	}{
		{"agents", "", viewAgents},
		{"stats", "s", viewStats},
		{"browse", "esc", viewBrowse},
	} {
		if tc.key != "" {
			m, _ = step(t, m, press(tc.key))
		}
		require.Equal(t, tc.view, m.view, tc.name)
		head := headerOf(m)
		require.Equalf(t, w, lipgloss.Width(head),
			"%s header should reach the right edge: %q", tc.name, head)
		require.Truef(t, strings.HasSuffix(head, "5 All"),
			"%s header should end with the period tabs: %q", tc.name, head)
	}

	// Too narrow for both: the header keeps its own content and the tabs go.
	narrow := agentModel(t, 40, 30)
	require.False(t, narrow.showPeriodTabs())
	require.NotContains(t, strip(narrow.View()), "5 All",
		"a cramped header should drop the tabs rather than overflow")
}
