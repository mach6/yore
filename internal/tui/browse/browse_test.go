package browse

import (
	"bytes"
	"encoding/base64"
	"errors"
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

	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
)

// --- test doubles & helpers ---------------------------------------------

type fakeBackend struct {
	mu           sync.Mutex
	reqs         []proto.QueryReq
	resp         proto.QueryResp
	hosts        proto.HostsInfo
	hostsCalls   int // Hosts() invocations; the sidebar may re-fetch repeatedly
	deleted      []string
	delErr       error
	onDelete     func(id string) // runs before each Delete is served; see fakeBackend.Delete
	failIDs      map[string]bool // Delete fails for exactly these ids, regardless of delErr
	submitted    []rec.Record
	submitErr    error
	submitFailID map[string]bool // SubmitRecord fails for exactly these TargetIDs, regardless of submitErr
	devices      proto.DevicesInfo
	approved     []string
	revoked      []string
	tokens       proto.TokensInfo
	tokRevoked   []string
	minted       proto.TokenInfo
	mintCalls    int
	synced       int
	syncErr      error
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
	// The hook runs outside the lock, and before the call is served, so a test
	// can act on the browser (press a key, say) part way through a batch.
	if f.onDelete != nil {
		f.onDelete(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	if f.failIDs[id] {
		return errors.New("delete failed")
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeBackend) SubmitRecord(r rec.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitFailID[r.TargetID] {
		return errors.New("submit failed")
	}
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, r)
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

func (f *fakeBackend) Tokens() (proto.TokensInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens, nil
}

func (f *fakeBackend) Token() (proto.TokenInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintCalls++
	return f.minted, nil
}

func (f *fakeBackend) RevokeToken(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokRevoked = append(f.tokRevoked, id)
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
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
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

// --- bulk selection --------------------------------------------------------

func TestCheckToggleAndCount(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})

	require.Zero(t, m.checkedCount())
	require.NotContains(t, strip(m.View()), "selected")

	m, _ = step(t, m, press(" "))
	require.Equal(t, 1, m.checkedCount())
	require.True(t, m.isChecked(m.rows[0].ID), "the row under the cursor should be the one marked")
	require.Contains(t, strip(m.View()), "1 selected", "the count must be visible on screen")

	// Space toggles: pressing it again on the same row unmarks it.
	m, _ = step(t, m, press(" "))
	require.Zero(t, m.checkedCount())
	require.NotContains(t, strip(m.View()), "selected")
}

func TestCheckAll(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})

	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount())
	for _, r := range m.rows {
		require.Truef(t, m.isChecked(r.ID), "row %s should be checked by select-all", r.ID)
	}
	require.Contains(t, strip(m.View()), "3 selected")

	// With nothing on screen, select-all has nothing to mark.
	m, _ = step(t, m, queryResultMsg{seq: 2, resp: mkResp(nil)})
	m, _ = step(t, m, press("ctrl+a"))
	require.Zero(t, m.checkedCount())
}

// TestCheckAllTogglesFromPartial: ctrl+a is a master-checkbox tri-state, not
// a one-way "select everything"; a partial selection promotes to all, a full
// selection clears, and a third press checks all again, exactly the way a
// table header's own select-all checkbox behaves.
func TestCheckAllTogglesFromPartial(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})

	// Only row 0 checked: a partial selection.
	m, _ = step(t, m, press(" "))
	require.Equal(t, 1, m.checkedCount())

	// ctrl+a on a partial selection promotes it to everything shown.
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount(), "ctrl+a on a partial selection should check every row shown")

	// Pressing it again, now that everything is checked, clears it outright.
	m, _ = step(t, m, press("ctrl+a"))
	require.Zero(t, m.checkedCount(), "ctrl+a on a full selection should clear it")

	// A third press checks all again: the toggle keeps working, not a one-shot.
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount(), "ctrl+a should check all again after clearing")
}

// TestCheckAllClearsWhenAllCheckedBySpace: the "all checked" test is about
// the state of m.checked, not about which key put it there; checking every
// row one at a time with space must clear on the next ctrl+a exactly like
// checking them with ctrl+a itself would.
func TestCheckAllClearsWhenAllCheckedBySpace(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})

	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press(" "))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, press("ctrl+a"))
	require.Zero(t, m.checkedCount(), "ctrl+a should clear a selection that reached 'all' via space alone")
}

func TestCheckClearOnEsc(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, press("esc"))
	require.Zero(t, m.checkedCount(), "esc should clear an active selection")
}

// TestCheckSurvivesUnzoomEsc: esc backs out one visible thing at a time (the
// zoom first, then the selection) the same "innermost first" rule the agent
// explorer's esc already follows.
func TestCheckSurvivesUnzoomEsc(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)

	m, _ = step(t, m, press("esc"))
	require.False(t, m.zoom, "the first esc should unzoom")
	require.Equal(t, 2, m.checkedCount(), "the first esc must not also drop the selection")

	m, _ = step(t, m, press("esc"))
	require.Zero(t, m.checkedCount(), "the second esc should clear the now-visible selection")
}

func TestCheckedRowShowsAMarker(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha"))})

	before := strip(m.View())
	m, _ = step(t, m, press(" "))
	after := strip(m.View())
	require.NotEqual(t, before, after, "checking a row should visibly change the table")
	require.Contains(t, after, "✓", "a checked row should carry a visible marker")
}

func TestDeleteConfirmShowsCheckedCount(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press(" ")) // check row 0
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press(" ")) // check row 1 too

	m, _ = step(t, m, press("d"))
	require.True(t, m.confirmDelete)
	out := strip(m.View())
	require.Contains(t, out, "delete 2 records?",
		"a bulk delete must confirm with the count, not the generic single-record prompt")
	require.NotContains(t, out, "delete this command?")
}

// TestDeleteConfirmSingleCheckedUsesCountPrompt: even one row EXPLICITLY
// checked goes through the bulk (count) prompt rather than the cursor-based
// one; the user went through the selection mechanism on purpose, and the two
// prompts confirm different things (a set of ids vs. "whatever the cursor is
// on right now").
func TestDeleteConfirmSingleCheckedUsesCountPrompt(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("only-one"))})
	m, _ = step(t, m, press(" "))

	m, _ = step(t, m, press("d"))
	out := strip(m.View())
	require.Contains(t, out, "delete 1 record?")
	require.NotContains(t, out, "delete this command?")
}

// runBulkDelete confirms a bulk delete and delivers its result. The deletes run
// in a command rather than inline in Update (the table holds every matching row
// (proto.LimitAll), so ctrl+a can check an archive and doing the round trips on
// the event loop would freeze the UI) so a test has to drain the command the way
// bubbletea would, or it asserts on a delete that has not happened yet.
func runBulkDelete(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = step(t, m, press("d"))
	m, cmd := step(t, m, press("y"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	return m
}

// TestBulkDeleteDoesNotBlockUpdate proves the round trips happen in a command
// and not on the event loop. buildReq asks for proto.LimitAll, so the table
// holds every matching row and ctrl+a can check an entire archive; deleting
// inline would hold Update for the whole run, freezing the UI with no redraw
// and nothing to tell it from a hang. Confirming must therefore return with
// nothing deleted yet, and the in-progress flash already on screen.
func TestBulkDeleteDoesNotBlockUpdate(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("d"))
	m, cmd := step(t, m, press("y"))

	require.Empty(t, f.deleted, "confirming must not delete inline: the work belongs in the command")
	require.Len(t, m.rows, 3, "no row may leave the table before the deletes have run")
	require.Contains(t, strip(m.View()), "deleting 0/3… esc to stop",
		"the in-progress flash must be showing while the command runs, counting and naming the key that stops it")
	require.NotNil(t, cmd, "confirming must hand back the command that does the work")

	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Len(t, f.deleted, 3, "draining the command must then do every delete")
	require.Empty(t, m.rows)
}

func TestBulkDeleteRemovesExactlySelected(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("keep-a", "drop-b", "keep-c", "drop-d"))})

	m, _ = step(t, m, press("down")) // row 1: drop-b
	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("down")) // row 3: drop-d
	m, _ = step(t, m, press(" "))

	m = runBulkDelete(t, m)

	require.ElementsMatch(t, []string{"1", "3"}, f.deleted, "exactly the checked records must be deleted")
	require.Len(t, m.rows, 2)
	require.Len(t, m.allRows, 2, "allRows must also drop deleted records, or a period switch would resurrect them")
	for _, r := range m.rows {
		require.NotEqualf(t, "drop-b", r.Cmd, "deleted row still present: %+v", m.rows)
		require.NotEqualf(t, "drop-d", r.Cmd, "deleted row still present: %+v", m.rows)
	}
	require.Zero(t, m.checkedCount(), "the selection should be cleared after a successful delete")
}

// TestBulkDeletePartialFailure: one delete call in the batch fails. The others
// must still go through, the failed row must stay put (and stay checked, so
// it can be retried), and the flash must report both counts rather than
// picking one of "asked for 3" / "deleted 2" to report.
func TestBulkDeletePartialFailure(t *testing.T) {
	f := &fakeBackend{failIDs: map[string]bool{"1": true}}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press("ctrl+a"))

	m = runBulkDelete(t, m)

	require.ElementsMatch(t, []string{"0", "2"}, f.deleted, "the failing id must not be reported as deleted")
	require.Len(t, m.rows, 1, "only the row whose delete failed should remain")
	require.Equal(t, "1", m.rows[0].ID)
	require.Contains(t, strip(m.View()), "1 failed", "a partial failure must be reported, not silently swallowed")
	require.True(t, m.isChecked("1"), "a row whose delete failed should stay checked so it can be retried")
}

// TestBulkDeleteAllFail: every call in the batch fails. Nothing local should
// change, and the selection survives for a retry.
func TestBulkDeleteAllFail(t *testing.T) {
	f := &fakeBackend{delErr: errors.New("boom")}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))

	m = runBulkDelete(t, m)

	require.Empty(t, f.deleted)
	require.Len(t, m.rows, 2, "nothing should be removed locally if every delete failed")
	require.Contains(t, strip(m.View()), "delete failed")
	require.Equal(t, 2, m.checkedCount(), "a fully failed bulk delete should leave the selection intact for retry")
}

// --- selection goes stale ---------------------------------------------------
// Every path that can change which records the table shows must drop a bulk
// selection made under the old rows: a mark that survived would be a mark on
// data the user never looked at, and the one consumer today is delete.

func TestCheckClearedOnNewSearch(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, press("/"))
	m, _ = step(t, m, press("x")) // typing narrows the query and re-issues it
	require.Zero(t, m.checkedCount(), "a new search must drop a selection made over the old results")
}

func TestCheckClearedOnHostChange(t *testing.T) {
	f := &fakeBackend{hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 2}}}}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, press("H")) // cycles the host scope: a new query
	require.Zero(t, m.checkedCount(), "changing the host scope must drop the old selection")
}

func TestCheckClearedOnPeriodChange(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, press("2")) // narrows the period filter client-side
	require.Zero(t, m.checkedCount(), "changing the period must drop the old selection")
}

func TestCheckClearedOnViewSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"stats", "s"},
		{"agents", "a"},
		{"devices", "D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{}
			m := ready(t, f, 120, 30)
			m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
			m, _ = step(t, m, press("ctrl+a"))
			require.Equal(t, 2, m.checkedCount())

			m, _ = step(t, m, press(tc.key))
			require.Zero(t, m.checkedCount(), "leaving the browse table for "+tc.name+" must drop the selection")
		})
	}
}

// A BACKGROUND refresh is the other half of that rule: nobody asked for
// different rows, so a selection has to survive it. The init-time warm loop
// re-queries once a second for the first several seconds of every session, so
// clearing there made space/ctrl+a in a freshly-opened browser look like it
// undid itself a beat later, and then start working once the loop stopped.

func TestCheckSurvivesWarmLoopRefresh(t *testing.T) {
	rows := mkRows("a", "b", "c")
	f := &fakeBackend{resp: func() proto.QueryResp {
		r := mkResp(rows)
		r.Remote = proto.RemoteInfo{State: proto.RemoteSyncing} // keeps the loop ticking
		return r
	}()}
	m := NewModel(f, Options{Now: now})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = step(t, m, initMsg{})
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: f.resp})

	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount())

	// A warm-loop tick re-issues the same query...
	m, _ = step(t, m, hostsTickMsg{})
	require.True(t, m.ticking, "the warm loop should still be running for this test to mean anything")
	require.Equal(t, 3, m.checkedCount(), "issuing a background refresh must not drop the selection")

	// ...and its answer (the same rows) arrives and keeps every mark.
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: f.resp})
	require.Equal(t, 3, m.checkedCount(), "a background refresh returning the same rows must keep the selection")

	// The same holds for a single row marked with space.
	m, _ = step(t, m, press("esc"))
	m, _ = step(t, m, press(" "))
	require.Equal(t, 1, m.checkedCount())
	m, _ = step(t, m, hostsTickMsg{})
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: f.resp})
	require.Equal(t, 1, m.checkedCount(), "space in a freshly-opened browser must not be undone by the warm loop")
}

func TestCheckSurvivesSyncRefresh(t *testing.T) {
	rows := mkRows("a", "b")
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 10, resp: f.resp})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 2, m.checkedCount())

	m, _ = step(t, m, syncDoneMsg{})
	m, _ = step(t, m, queryResultMsg{seq: 11, resp: f.resp})
	require.Equal(t, 2, m.checkedCount(), "a refresh after sync must keep a selection over rows that are still there")
}

// A background refresh keeps marks by pruning, not by trusting: rows the
// refresh no longer returns lose theirs, so a mark can never end up on a
// different row's data, and rows the refresh newly brings in are never marked.
func TestRefreshPrunesChecksForVanishedRows(t *testing.T) {
	rows := mkRows("a", "b", "c")
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 10, resp: f.resp})
	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount())

	// The refresh comes back without the middle row, plus one the user has
	// never seen.
	fresh := []rec.Record{{ID: "new", Cmd: "z", StartMs: now + 1000}, rows[0], rows[2]}
	m, _ = step(t, m, queryResultMsg{seq: 11, resp: mkResp(fresh)})
	require.Equal(t, 2, m.checkedCount(), "a vanished row's mark must be dropped, and a new row must not be marked")
	require.True(t, m.isChecked("0"))
	require.True(t, m.isChecked("2"))
	require.False(t, m.isChecked("new"), "a row the refresh newly brought in must not arrive checked")

	// Once nothing is left checked the set is back to empty.
	m, _ = step(t, m, queryResultMsg{seq: 12, resp: mkResp([]rec.Record{{ID: "other", Cmd: "q", StartMs: now}})})
	require.Zero(t, m.checkedCount(), "a refresh sharing no rows with the selection clears it")
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
// its top rule, so a one-row arithmetic slip would not look like a bug: it would
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
			// resolved for the view being measured; exactly as switching to it does.
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

	// A deep query result reports the remote cache just warmed with 2 hosts (a
	// count the sidebar hasn't seen) so the model must refetch the host list.
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

// TestExecutorAndTagColumns holds the two axes apart in the table: the executor
// is an attribute of the row, the tags are what somebody put on it, and one
// merged cell used to render them as a single indistinguishable list.
func TestExecutorAndTagColumns(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("agent-run", "ls", "vim")
	rows[0].Executor = "claude-code"
	rows[1].Tags = []string{"refactor"} // resolved by the daemon; no executor in it

	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	require.True(t, m.hasExec, "a row an agent ran should set hasExec")
	require.True(t, m.hasTags, "a labelled row should set hasTags")
	l := m.colLayout()
	require.True(t, l.shows(colExec), "the exec column shows when a row carries an executor")
	require.True(t, l.shows(colTags), "the tags column shows when a row carries a tag")

	out := strip(m.View())
	require.Containsf(t, out, "exec", "table header should name the exec column:\n%s", out)
	require.Containsf(t, out, "tags", "table header should name the tags column:\n%s", out)
	require.Containsf(t, out, "claude-code", "the exec cell should render the executor:\n%s", out)
	require.Containsf(t, out, "refactor", "the tags cell should render the user tag:\n%s", out)

	// Neither column claims the other's value.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "agent-run") {
			require.NotContains(t, line, "refactor", "the agent row carries no user tag")
		}
		if strings.Contains(line, "ls") && strings.Contains(line, "refactor") {
			require.NotContains(t, line, "claude-code", "the tagged row was not run by an agent")
		}
	}
}

// TestExecutorFilter covers e: adopt the selected row's executor, send it as
// Executor, clear on a second press.
func TestExecutorFilter(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("agent-run", "ls", "vim")
	rows[0].Executor = "claude-code"
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, cmd := step(t, m, press("e"))
	require.Equal(t, "claude-code", m.executorFilter, "e should adopt the selected row's executor")
	require.NotNil(t, cmd, "toggling the executor filter should re-issue the query")
	require.Equal(t, "claude-code", m.buildReq().Executor, "buildReq should carry the executor filter")
	require.Empty(t, m.buildReq().Tag, "the executor filter must not travel as a tag")
	require.Containsf(t, strip(m.View()), "executor: claude-code", "status bar should name the executor filter")

	m, _ = step(t, m, press("e"))
	require.Equal(t, "", m.executorFilter, "second e should clear the filter")
	require.Equal(t, "", m.buildReq().Executor)
}

// TestTagFilter covers t: the mirror of e on the other axis. An agent-run row
// with no user tag has nothing for it to adopt: which is the whole point of
// keeping the two apart.
func TestTagFilter(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("agent-run", "ls", "vim")
	rows[0].Executor = "claude-code"
	rows[1].Tags = []string{"refactor"}
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// Row 0 is the agent row: an executor, no tags. t finds nothing to adopt.
	m, cmd := step(t, m, press("t"))
	require.Empty(t, m.tagFilter, "an executor is not a tag t can adopt")
	require.NotNil(t, cmd, "the flash still needs a tick")
	require.Contains(t, strip(m.View()), "no tags on this row")

	// Move to the tagged row and adopt it.
	m, _ = step(t, m, press("down"))
	m, cmd = step(t, m, press("t"))
	require.Equal(t, "refactor", m.tagFilter, "t should adopt the selected row's tag")
	require.NotNil(t, cmd, "toggling the tag filter should re-issue the query")
	require.Equal(t, "refactor", m.buildReq().Tag, "buildReq should carry the tag filter")
	require.Empty(t, m.buildReq().Executor, "the tag filter must not travel as an executor")
	require.Containsf(t, strip(m.View()), "tag: refactor", "status bar should name the tag filter")

	m, _ = step(t, m, press("t"))
	require.Equal(t, "", m.tagFilter, "second t should clear the filter")
	require.Equal(t, "", m.buildReq().Tag)
}

// TestTypedFilterReachesValuesNoRowCarries: adopting from the cursor can only
// find values already in front of you. T and E take a typed one, which is the
// only way to filter on a tag or executor the visible rows do not mention.
func TestTypedFilterReachesValuesNoRowCarries(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("ls", "vim")
	rows[0].Tags = []string{"refactor"}
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, _ = step(t, m, press("T"))
	require.Equal(t, axisTag, m.axis, "T opens the typed tag box")
	require.Contains(t, strip(m.View()), "filter tag", "the prompt names the axis")
	require.Equal(t, "refactor", m.filterInput.Value(),
		"it opens on the row's own value, so enter alone does what t does")

	// Type a tag no row carries.
	m = setFilterEntry(t, m, "deploy")
	m, cmd := step(t, m, press("enter"))
	require.Equal(t, axisNone, m.axis, "enter closes the box")
	require.Equal(t, "deploy", m.tagFilter)
	require.Equal(t, "deploy", m.buildReq().Tag, "the typed tag travels as the tag filter")
	require.NotNil(t, cmd, "applying a filter should re-issue the query")
	require.Contains(t, strip(m.View()), "tag: deploy")

	// E is the same on the executor axis, and the two do not cross.
	m, _ = step(t, m, press("E"))
	require.Equal(t, axisExec, m.axis)
	m = setFilterEntry(t, m, "codex")
	m, _ = step(t, m, press("enter"))
	require.Equal(t, "codex", m.executorFilter)
	require.Equal(t, "codex", m.buildReq().Executor)
	require.Equal(t, "deploy", m.buildReq().Tag, "the tag filter is untouched")
}

// TestTypedFilterEmptyClears: the box is also how a filter comes off without
// hunting the table for a row that happens to carry it.
func TestTypedFilterEmptyClears(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("ls"))})
	m.executorFilter = "claude-code"

	m, _ = step(t, m, press("E"))
	require.Equal(t, "claude-code", m.filterInput.Value(),
		"the box opens on the filter in force, so it can be edited not just replaced")

	m = setFilterEntry(t, m, "")
	m, cmd := step(t, m, press("enter"))
	require.Empty(t, m.executorFilter)
	require.NotNil(t, cmd)
	require.Contains(t, strip(m.View()), "executor filter cleared")
}

// TestTypedFilterEscAbandons: esc leaves the filter exactly as it was; unlike
// the search field, where the text typed so far IS the filter.
func TestTypedFilterEscAbandons(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("ls"))})
	m.tagFilter = "refactor"

	m, _ = step(t, m, press("T"))
	m = setFilterEntry(t, m, "deploy")
	m, cmd := step(t, m, press("esc"))
	require.Equal(t, axisNone, m.axis)
	require.Equal(t, "refactor", m.tagFilter, "esc must not apply what was typed")
	require.Nil(t, cmd, "and must not spend a query")

	// An unchanged value spends no query either.
	m, _ = step(t, m, press("T"))
	m, cmd = step(t, m, press("enter"))
	require.Equal(t, "refactor", m.tagFilter)
	require.Nil(t, cmd, "re-applying the same value is not a change")
}

// TestTypedFilterBoxSwallowsViewKeys: while the box has focus its keystrokes are
// text, so the letters that would switch view or delete a row must type instead.
func TestTypedFilterBoxSwallowsViewKeys(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("ls"))})

	m, _ = step(t, m, press("T"))
	m = setFilterEntry(t, m, "")
	for _, k := range []string{"s", "a", "D", "d", "q", "?"} {
		m, _ = step(t, m, press(k))
	}
	require.Equal(t, viewBrowse, m.view, "no view key should have fired")
	require.False(t, m.confirmDelete, "d must not arm a delete")
	require.False(t, m.showHelp)
	require.False(t, m.quitting)
	require.Equal(t, "saDdq?", m.filterInput.Value(), "they should all be text")
}

// setFilterEntry replaces the typed box's contents, one keystroke at a time so
// the real Update path does the editing.
func setFilterEntry(t *testing.T, m Model, s string) Model {
	t.Helper()
	for range len(m.filterInput.Value()) {
		m, _ = step(t, m, press("backspace"))
	}
	require.Empty(t, m.filterInput.Value())
	for _, r := range s {
		m, _ = step(t, m, press(string(r)))
	}
	return m
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

// TestTagPromptShowsScopeAndResets: ctrl+t with a selection names the count
// it is about to tag, the same precedent the delete confirm sets; closing it
// (by cancel or submit) must put the prompt back to the single-row default so
// the next ctrl+t with nothing checked does not still show a stale count.
func TestTagPromptShowsScopeAndResets(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})

	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press(" "))

	m, _ = step(t, m, press("ctrl+t"))
	require.Equal(t, "tag 2 records: ", m.tagInput.Prompt, "the prompt should name how many records it will tag")

	m, _ = step(t, m, press("esc"))
	require.Equal(t, "tag: ", m.tagInput.Prompt, "closing the box must drop the stale count")

	// Reopening with nothing checked shows the plain single-row prompt.
	m, _ = step(t, m, press("esc")) // clear the selection
	m, _ = step(t, m, press("ctrl+t"))
	require.Equal(t, "tag: ", m.tagInput.Prompt)
}

// runBulkTag opens ctrl+t, types name, submits, and drains the resulting
// command the way bubbletea would: the async mirror of runBulkDelete, since
// a bulk tag's SubmitRecord round trips run in a tea.Cmd, not on Update.
func runBulkTag(t *testing.T, m Model, name string) Model {
	t.Helper()
	m, _ = step(t, m, press("ctrl+t"))
	for _, r := range name {
		m, _ = step(t, m, press(string(r)))
	}
	m, cmd := step(t, m, press("enter"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	return m
}

// TestBulkTagTagsEveryChecked: with a selection, ctrl+t tags the whole
// checked set instead of the cursor row, and every tagged row shows the tag
// optimistically. The unchecked row must not be touched.
func TestBulkTagTagsEveryChecked(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("a", "b", "c")
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, _ = step(t, m, press(" ")) // check row 0
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press(" ")) // check row 2

	m = runBulkTag(t, m, "wip")

	require.Len(t, f.submitted, 2, "exactly the checked rows should be tagged")
	got := map[string]bool{}
	for _, r := range f.submitted {
		require.Equal(t, "wip", r.TagName)
		require.Equal(t, "tag", r.Type)
		got[r.TargetID] = true
	}
	require.True(t, got["0"] && got["2"], "the two checked ids should have been submitted")

	byID := map[string][]string{}
	for _, r := range m.rows {
		byID[r.ID] = r.Tags
	}
	require.Contains(t, byID["0"], "wip")
	require.Contains(t, byID["2"], "wip")
	require.Empty(t, byID["1"], "the row never checked must not gain the tag")
	require.Contains(t, strip(m.View()), "✓ tagged 2 records")

	// Tagging is additive, not destructive like delete: the selection survives
	// so a second tag over the same set does not require reselecting.
	require.Equal(t, 2, m.checkedCount(), "a successful bulk tag should not clear the selection")
}

// TestBulkTagSingleRowStillWorksUnchecked proves the dispatch in submitTag:
// with nothing checked, ctrl+t still acts on the single row under the
// cursor, exactly as before bulk tag existed.
func TestBulkTagSingleRowStillWorksUnchecked(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("a", "b")
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})
	require.Zero(t, m.checkedCount())

	m, _ = step(t, m, press("ctrl+t"))
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("solo")})
	m, _ = step(t, m, press("enter"))

	require.Len(t, f.submitted, 1, "the single-row path submits synchronously in Update")
	require.Equal(t, rows[0].ID, f.submitted[0].TargetID)
	require.Contains(t, m.rows[0].Tags, "solo")
}

// TestBulkTagDoesNotBlockUpdate proves the round trips happen in a command,
// not on the event loop: the same freeze bug fixed for bulk delete. This is
// the important assertion: it must fail if the loop moves back inline.
func TestBulkTagDoesNotBlockUpdate(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("ctrl+t"))
	for _, r := range "wip" {
		m, _ = step(t, m, press(string(r)))
	}
	m, cmd := step(t, m, press("enter"))

	require.Empty(t, f.submitted, "confirming must not submit inline: the work belongs in the command")
	for _, r := range m.rows {
		require.Emptyf(t, r.Tags, "no row may show the tag before the command has run: %+v", r)
	}
	require.Contains(t, strip(m.View()), "tagging 0/3… esc to stop",
		"the in-progress flash must be showing while the command runs, counting and naming the key that stops it")
	require.NotNil(t, cmd, "confirming must hand back the command that does the work")

	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Len(t, f.submitted, 3, "draining the command must then do every submit")
	for _, r := range m.rows {
		require.Contains(t, r.Tags, "wip")
	}
}

// TestBulkTagPartialFailure: one SubmitRecord call in the batch fails. The
// others must still go through, the failed id must stay checked so it can be
// retried, and the flash must report both counts. Note the whole selection
// survives here, not just the failure: a bulk tag never unchecks anything,
// which is what makes the failed id retry-able without any special case.
func TestBulkTagPartialFailure(t *testing.T) {
	f := &fakeBackend{submitFailID: map[string]bool{"1": true}}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press("ctrl+a"))

	m = runBulkTag(t, m, "wip")

	byID := map[string][]string{}
	for _, r := range m.rows {
		byID[r.ID] = r.Tags
	}
	require.Contains(t, byID["0"], "wip")
	require.Contains(t, byID["2"], "wip")
	require.Empty(t, byID["1"], "the failing id must not show the tag")
	require.Contains(t, strip(m.View()), "tagged 2, 1 failed")

	// Tagging never drops anything from the checked set (rows survive it, so
	// the selection is worth keeping for a follow-up tag) but the one id that
	// actually failed is exactly as retry-able as if nothing else had
	// succeeded: it is (still) checked.
	require.True(t, m.isChecked("0"))
	require.True(t, m.isChecked("1"), "the failed id should stay checked so it can be retried")
	require.True(t, m.isChecked("2"))
}

// TestOptimisticTagSurvivesPeriodChange is the allRows regression test: the
// single-row tag path used to mutate only m.rows, so applyPeriodFilter (which
// rebuilds rows from allRows on every 1..5 press) silently dropped the tag the
// next time the period changed. It must fail against the old m.rows-only
// submitTag.
func TestOptimisticTagSurvivesPeriodChange(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkRows("a", "b")
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// Narrow to "Today" first: on the default "All" period, rows and allRows
	// are literally the same slice, so the bug would not reproduce there;
	// applyPeriodFilter has to actually rebuild rows from allRows for the
	// missing-allRows write to matter.
	m, _ = step(t, m, press("1"))
	require.NotEmpty(t, m.rows, "the rows should still be within today's window")

	m, _ = step(t, m, press("ctrl+t"))
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("refactor")})
	m, _ = step(t, m, press("enter"))

	found := false
	for _, r := range m.rows {
		if r.ID == rows[0].ID {
			found = true
			require.Contains(t, r.Tags, "refactor")
		}
	}
	require.True(t, found)

	// Back to "All": applyPeriodFilter now hands rows the allRows slice
	// directly. If the tag never reached allRows, it vanishes here.
	m, _ = step(t, m, press("5"))
	found = false
	for _, r := range m.rows {
		if r.ID == rows[0].ID {
			found = true
			require.Contains(t, r.Tags, "refactor", "the tag must survive a period-filter round trip")
		}
	}
	require.True(t, found)
}

func TestTagFilterNoTag(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("ls", "vim"))})
	require.False(t, m.hasTags, "no rows carry a tag")
	require.False(t, m.colLayout().shows(colTags), "the tag column stays hidden without tags")

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

	// Enter the devices view; it fetches both lists asynchronously.
	m, cmd := step(t, m, press("D"))
	require.Equal(t, viewDevices, m.view)
	require.NotNil(t, cmd, "entering devices view did not fetch")
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Len(t, m.devices, 2)
	out := strip(m.View())
	require.Containsf(t, out, "laptop", "devices view missing device/code:\n%s", out)
	require.Containsf(t, out, "AB12-CD34", "devices view missing device/code:\n%s", out)

	// The pending machine sorts to the top of the list: it is the only row that
	// is waiting for the user to do something.
	require.Equal(t, "pending", m.devices[0].Status, "a pending machine leads the list")

	// Approve it. Approving asks first, and the question quotes the verification
	// code: admitting a machine to the group is only safe if the user checked
	// that code against the one it is showing.
	m, _ = step(t, m, press("a"))
	require.NotEmpty(t, m.devConfirm, "a did not arm an approve confirmation")
	require.True(t, m.devApproving, "the armed action should be an approval")
	out = strip(m.View())
	require.Containsf(t, out, "AB12-CD34", "the approve prompt must show the code:\n%s", out)
	require.Containsf(t, out, "server", "the approve prompt must name the machine:\n%s", out)
	require.Empty(t, f.approved, "nothing is approved until the question is answered")

	m, acmd := step(t, m, press("y"))
	require.NotNil(t, acmd, "y did not trigger approve")
	acmd()
	require.Equal(t, []string{"01BBBBBBBBBBBBBBBBBBBBBBBB"}, f.approved)

	// Anything else cancels.
	m, _ = step(t, m, press("a"))
	require.NotEmpty(t, m.devConfirm)
	m, _ = step(t, m, press("n"))
	require.Empty(t, m.devConfirm, "n cleared the confirmation")
	require.Len(t, f.approved, 1, "a cancelled approval approves nothing")

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
	rows[0].Executor, rows[0].PromptID, rows[0].Prompt, rows[0].Session = "claude-code", "p1", "add rate limiting", "sessionAAAA1111"
	rows[1].Executor, rows[1].PromptID, rows[1].Prompt, rows[1].Session = "claude-code", "p1", "add rate limiting", "sessionAAAA1111"
	rows[2].Executor, rows[2].PromptID, rows[2].Prompt, rows[2].Session = "devin", "p2", "fix the N+1 query", "sessionBBBB2222"
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

// TestAgentsPanes proves the explorer shows all five panes at once: the
// executor sidebar, the host list, the prompt list, the highlighted prompt's
// commands, and the details of whatever is selected.
func TestAgentsPanes(t *testing.T) {
	m := agentModel(t, 140, 40)
	out := strip(m.View())

	for _, want := range []string{"AGENTS", "HOSTS", "PROMPTS", "COMMANDS", "DETAILS"} {
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

// TestAgentsPaneFocusCycles walks tab (and shift+tab) around all five panes and
// checks each pane's cursor keys act on the pane that holds focus.
func TestAgentsPaneFocusCycles(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, apPrompts, m.apane)

	for _, want := range []agentPane{apCommands, apInfo, apAgents, apHosts, apPrompts} {
		m, _ = step(t, m, press("tab"))
		require.Equal(t, want, m.apane, "tab landed on the wrong pane")
	}
	m, _ = step(t, m, press("shift+tab"))
	require.Equal(t, apHosts, m.apane, "shift+tab did not go back")

	// On the host pane, j moves the host cursor (not the prompt cursor).
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.agentHostSel)
	require.Equal(t, 0, m.promptSel, "host navigation must not move the prompt cursor")
	m, _ = step(t, m, press("k"))
	require.Equal(t, 0, m.agentHostSel)

	// On the sidebar, j moves the executor cursor.
	m, _ = step(t, m, press("shift+tab"))
	require.Equal(t, apAgents, m.apane)
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.agentSel)
	require.Equal(t, 0, m.promptSel, "sidebar navigation must not move the prompt cursor")

	// On the command pane, j moves the command cursor.
	m.agentSel, m.agentFilter = 0, ""
	m.recomputeStats()
	m, _ = step(t, m, press("tab")) // -> hosts
	m, _ = step(t, m, press("tab")) // -> prompts
	m, _ = step(t, m, press("tab")) // -> commands
	require.Equal(t, apCommands, m.apane)
	m, _ = step(t, m, press("j"))
	require.Equal(t, 1, m.drillSel)
}

// TestAgentsSidebarFilters proves picking an executor narrows the prompt,
// command, and details panes to that agent's work, and that the "all agents"
// row restores everything.
func TestAgentsSidebarFilters(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Len(t, m.prompts.prompts, 2, "both prompts visible with no filter")

	// Focus the sidebar and select the first executor (claude-code: 2 commands,
	// so it sorts ahead of devin).
	m, _ = step(t, m, press("shift+tab")) // prompts -> hosts
	m, _ = step(t, m, press("shift+tab")) // hosts -> agents
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
	m, _ = step(t, m, press("shift+tab")) // prompts -> hosts
	m, _ = step(t, m, press("shift+tab")) // hosts -> agents
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
	// cursor starts on `cargo build`: the details pane must track that, not the
	// newest row.
	require.Containsf(t, out, "cargo build", "command details should describe the selected command:\n%s", out)
	require.Containsf(t, out, "/work/1", "command details should carry the selected command's cwd:\n%s", out)
}

// TestAgentsDetailsKeepsItsSubjectOnFocus: tabbing onto the details pane must not
// change what it is describing. It used to read the focused pane, so the one
// route to a command's full record (focus the pane, then z) swapped the prompt's
// in on the way, and the arrows still drove the command cursor while the prompt
// was on screen.
func TestAgentsDetailsKeepsItsSubjectOnFocus(t *testing.T) {
	m := agentModel(t, 140, 40)

	m, _ = step(t, m, press("tab")) // prompts -> commands
	require.Equal(t, apCommands, m.apane)
	require.True(t, m.infoCmd)

	m, _ = step(t, m, press("tab")) // commands -> details
	require.Equal(t, apInfo, m.apane)
	body := detailsBody(m)
	require.Containsf(t, body, "cargo build", "details must still describe the command:\n%s", body)
	require.Containsf(t, body, "Exit", "and still be the command's record:\n%s", body)
	require.NotContainsf(t, body, "add rate limiting",
		"the prompt's text must not have replaced it:\n%s", body)
	require.Contains(t, strip(m.View()), "DETAILS  command",
		"the title says which record it is holding")

	// Zooming it (the reason to focus it at all) keeps the command too.
	m, _ = step(t, m, press("z"))
	out := strip(m.View())
	require.Containsf(t, out, "cargo build", "zoomed details must hold the command:\n%s", out)
	require.NotContains(t, out, "PROMPTS", "z fills the frame with the focused pane")
}

// detailsBody is the explorer details pane's own text, without the rest of the
// frame: the prompt list is on screen too, so a view-wide assertion cannot tell
// which pane a prompt's text came from.
func detailsBody(m Model) string {
	return strip(strings.Join(m.agentInfoLines(maxInt(1, m.geo.p[apInfo].w-2)), "\n"))
}

// TestAgentsDetailsSubjectFollowsTheListPanes: the subject is chosen by the list
// pane focus last landed on, so arriving at DETAILS from the prompt side shows
// the prompt and from the command side shows the command.
func TestAgentsDetailsSubjectFollowsTheListPanes(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.False(t, m.infoCmd, "the explorer opens on the prompt list")

	// shift+tab from prompts reaches DETAILS the long way round, via the panes
	// that select a prompt, so it is a prompt that is being described.
	for range 3 {
		m, _ = step(t, m, press("shift+tab"))
	}
	require.Equal(t, apInfo, m.apane)
	require.False(t, m.infoCmd)
	body := detailsBody(m)
	require.Containsf(t, body, "add rate limiting", "details should hold the prompt:\n%s", body)
	require.Contains(t, strip(m.View()), "DETAILS  prompt")

	// Landing on the command pane repoints it, and it stays repointed.
	m, _ = step(t, m, press("tab")) // -> agents
	require.False(t, m.infoCmd, "the sidebar selects a prompt, not a command")
	for m.apane != apCommands {
		m, _ = step(t, m, press("tab"))
	}
	require.True(t, m.infoCmd)
}

// TestAgentsDetailsScrolls: the pane holds a whole record, which on a tiled pane
// is taller than the pane. Focused, ↑/↓ move its body rather than a list it is
// not showing, it says when there is more below, and it clamps at both ends.
func TestAgentsDetailsScrolls(t *testing.T) {
	m := agentModel(t, 140, 14) // short enough that a record overflows
	for m.apane != apInfo {
		m, _ = step(t, m, press("tab"))
	}
	require.Positive(t, m.infoMaxTop(), "140x14 should not fit a whole record")
	require.Contains(t, strip(m.View()), "↓ more", "an overflowing pane should say so")
	require.Contains(t, footerText(m), "↑↓ scroll", "the hint follows focus into the pane")

	before := m.drillSel
	m, _ = step(t, m, press("down"))
	require.Equal(t, 1, m.infoTop, "↓ scrolls the body")
	require.Equal(t, before, m.drillSel, "and no longer drags the command cursor with it")

	// It clamps at the bottom, and G/g jump to each end.
	for range 40 {
		m, _ = step(t, m, press("down"))
	}
	require.Equal(t, m.infoMaxTop(), m.infoTop, "scrolling clamps to the last line")
	require.NotContains(t, strip(m.View()), "↓ more", "at the end there is nothing more")

	m, _ = step(t, m, press("g"))
	require.Zero(t, m.infoTop)
	m, _ = step(t, m, press("G"))
	require.Equal(t, m.infoMaxTop(), m.infoTop)
}

// TestAgentsDetailsScrollResetsOnANewSubject: an offset belongs to the record it
// was scrolled into, so moving to another command or prompt starts at the top
// rather than part-way down a different record.
func TestAgentsDetailsScrollResetsOnANewSubject(t *testing.T) {
	m := agentModel(t, 140, 14)
	for m.apane != apInfo {
		m, _ = step(t, m, press("tab"))
	}
	m, _ = step(t, m, press("down"))
	require.Positive(t, m.infoTop)

	// Back to the command pane and onto the next command.
	m, _ = step(t, m, press("shift+tab"))
	require.Equal(t, apCommands, m.apane)
	require.Zero(t, m.infoTop, "leaving the pane for a list repoints and rewinds it")

	m, _ = step(t, m, press("tab")) // -> details
	m, _ = step(t, m, press("down"))
	require.Positive(t, m.infoTop)
	m, _ = step(t, m, press("shift+tab")) // -> commands
	m, _ = step(t, m, press("down"))      // a different command
	require.Zero(t, m.infoTop)
}

// TestAgentsDetailsLiftsTheBodyCapWhenFocused: the wrapped command is capped
// while the pane is glanced at, so a long one cannot push the metadata rows out.
// Focused, the cap would hide the very text the user tabbed over to read.
func TestAgentsDetailsLiftsTheBodyCapWhenFocused(t *testing.T) {
	long := strings.Repeat("cargo build --features one,two,three ", 40)
	rows := mkRows(long)
	rows[0].Executor, rows[0].PromptID, rows[0].Prompt = "claude-code", "p1", "add rate limiting"
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(rows),
	}
	m := openAgents(t, ready(t, f, 100, 40))
	for m.apane != apCommands {
		m, _ = step(t, m, press("tab"))
	}
	require.Len(t, wrapPlain(long, 60, infoBodyCap), infoBodyCap, "the glanced-at body is capped")
	capped := len(m.agentInfoLines(60))

	m, _ = step(t, m, press("tab")) // -> details
	require.Equal(t, apInfo, m.apane)
	lines := m.agentInfoLines(60)
	require.Greater(t, len(lines), capped,
		"focusing the pane should uncap the body so scrolling can reach all of it")
	require.NotContains(t, strip(strings.Join(lines, "\n")), "…",
		"the cap's truncation marker would hide what the pane was focused to show")
	require.Positive(t, m.infoMaxTop(), "and the rest is reachable by scrolling")
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
// to be 0, and neither view zooms to pane 0 by default.
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

// TestZoomDetailToggle proves z and Z are a mirrored pair: z always reaches
// the plain single-pane zoom this view had before Z existed, Z keeps the
// detail companion beside the zoomed pane instead of hiding it, and either
// key exits once its own flavor is already showing.
func TestZoomDetailToggle(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	require.Equal(t, focusTable, m.focus)

	// Plain zoom: no regression. Only the table's own slot is populated.
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)
	require.False(t, m.zoomDetail)
	require.Zero(t, m.geo.p[focusDetail], "plain zoom must not show the detail companion")
	m, _ = step(t, m, press("z"))
	require.False(t, m.zoom, "z exits the flavor it already showed")

	// Z: the detail companion joins the zoomed pane.
	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom)
	require.True(t, m.zoomDetail)
	require.NotZero(t, m.geo.p[focusDetail], "Z must keep the detail companion visible")
	require.NotZero(t, m.geo.p[focusTable])

	// Switching flavor mid-zoom stays zoomed rather than exiting.
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom, "z should downgrade the flavor, not leave zoom")
	require.False(t, m.zoomDetail)
	require.Zero(t, m.geo.p[focusDetail])

	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom, "Z should upgrade the flavor, not leave zoom")
	require.True(t, m.zoomDetail)

	// Z exits once its own flavor is already showing.
	m, _ = step(t, m, press("Z"))
	require.False(t, m.zoom)
	require.False(t, m.zoomDetail)
}

// TestBrowseZoomDetail proves Z keeps the results table's detail pane visible
// as a side pane, sized from the same per-mille split mechanism as the rest of
// the layout, instead of losing it the way plain zoom does.
func TestBrowseZoomDetail(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom)
	require.True(t, m.zoomDetail)
	out := strip(m.View())
	require.NotContainsf(t, out, "HOSTS", "the sidebar has no detail companion and stays hidden:\n%s", out)
	require.Containsf(t, out, "COMMANDS", "the zoomed table must still render:\n%s", out)
	require.Containsf(t, out, "cargo build", "and still show its rows:\n%s", out)
	require.Containsf(t, out, "DETAILS", "the detail companion must still be on screen:\n%s", out)

	tbl, det := m.geo.p[focusTable], m.geo.p[focusDetail]
	require.Positive(t, tbl.w)
	require.Positive(t, det.w)
	require.Equal(t, m.width, tbl.w+det.w, "the pair must still fill the whole frame")
	require.Equal(t, tbl.w, m.geo.vDiv, "the seam between them must be draggable")

	// A pane with no detail companion (the host sidebar) falls back to plain
	// zoom, per the same rule the agent explorer's sidebar and host lists get.
	m, _ = step(t, m, press("esc")) // drop the companion
	m, _ = step(t, m, press("esc")) // unzoom entirely
	m, _ = step(t, m, press("shift+tab"))
	require.Equal(t, focusHosts, m.focus)
	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom)
	require.False(t, m.zoomDetail, "the sidebar has nothing to pair with")
	out = strip(m.View())
	require.Containsf(t, out, "HOSTS", "the zoomed sidebar must still render:\n%s", out)
	require.NotContainsf(t, out, "DETAILS", "there is nothing to keep beside it:\n%s", out)
}

// TestAgentsZoomDetail is TestBrowseZoomDetail's counterpart for the explorer's
// prompt pane: Z keeps DETAILS beside it, and the pairing follows whichever of
// the prompt or command list last pointed DETAILS at itself.
func TestAgentsZoomDetail(t *testing.T) {
	m := agentModel(t, 140, 40)
	require.Equal(t, apPrompts, m.apane)

	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom)
	require.True(t, m.zoomDetail)
	out := strip(m.View())
	require.Containsf(t, out, "PROMPTS", "the zoomed prompt list must still render:\n%s", out)
	require.Containsf(t, out, "DETAILS", "its detail companion must still be on screen:\n%s", out)
	require.NotContainsf(t, out, "COMMANDS", "the command pane has no place in this pair:\n%s", out)
	// "AGENTS" is not a safe marker here: it is also the explorer's permanent
	// title-line label, on screen in every state. The sidebar's own visibility
	// is a geometry fact instead.
	require.Zero(t, m.geo.p[apAgents], "the executor sidebar has no rect while this pair is zoomed")

	prompts, info := m.geo.p[apPrompts], m.geo.p[apInfo]
	require.Positive(t, prompts.w)
	require.Positive(t, info.w)
	require.Equal(t, m.width, prompts.w+info.w)
	require.Equal(t, prompts.w, m.geo.vDiv)

	// Tabbing onto DETAILS keeps the same pair on screen; focus moves between
	// the two visible panes rather than the layout collapsing to one.
	m, _ = step(t, m, press("tab")) // prompts -> commands
	m, _ = step(t, m, press("tab")) // commands -> details
	require.Equal(t, apInfo, m.apane)
	require.True(t, m.infoCmd, "landing on details via the command pane describes the command")
	out = strip(m.View())
	require.Contains(t, out, "COMMANDS", "the pairing followed the command pane, not the prompt pane")
	require.NotContains(t, out, "PROMPTS")

	// A pane with no detail companion (the executor sidebar) falls back to
	// plain zoom.
	m, _ = step(t, m, press("esc")) // drop the companion
	m, _ = step(t, m, press("esc")) // unzoom
	for m.apane != apAgents {
		m, _ = step(t, m, press("shift+tab"))
	}
	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoom)
	require.False(t, m.zoomDetail)
	require.Equal(t, m.width, m.geo.p[apAgents].w, "the sidebar alone fills the frame")
	require.Zero(t, m.geo.p[apInfo], "there is nothing to keep beside it")
}

// TestZoomDetailMouse proves the mouse still works in the side-pane layout: the
// wheel scrolls whichever of the pair is under the pointer, and a click on the
// detail companion focuses it without dropping the zoom.
func TestZoomDetailMouse(t *testing.T) {
	long := strings.Repeat("cargo build --features one,two,three ", 20)
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows(long)),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoomDetail)
	require.Positive(t, m.detail.TotalLineCount()-m.detail.Height, "the long command should overflow the companion")

	det := m.geo.p[focusDetail]
	m, _ = step(t, m, wheelAt(det.x+2, det.y+2, tea.MouseButtonWheelDown))
	require.Positive(t, m.detail.YOffset, "the wheel must scroll the detail companion")

	m, _ = step(t, m, click(det.x+2, det.y+2))
	require.Equal(t, focusDetail, m.focus, "clicking the detail companion must focus it")
	require.True(t, m.zoom, "focusing the companion must not drop the zoom")
	require.True(t, m.zoomDetail, "and must not drop the companion")

	// The table half of the pair is still reachable and still scrolls.
	tbl := m.geo.p[focusTable]
	m, _ = step(t, m, click(tbl.x+2, tbl.y+2))
	require.Equal(t, focusTable, m.focus)
}

// TestZoomDetailSplitSurvivesResize proves the seam between the zoomed pane and
// its detail companion is stored as a per-mille ratio, like every other split,
// so the proportion the user dragged to holds through a terminal resize.
func TestZoomDetailSplitSurvivesResize(t *testing.T) {
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 1}}},
		resp:  mkResp(mkRows("cargo build")),
	}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	m, _ = step(t, m, press("Z"))
	require.True(t, m.zoomDetail)
	// This split is a new feature with no legacy layout to preserve, so (like
	// the agent explorer's own splits) it opens at a sensible default rather
	// than at 0 ("auto" for the browse view's pre-existing splits, which have
	// an established layout to leave alone until dragged).
	require.Equal(t, defaultZoomDetailRatio, m.splits.ZoomDetail, "an undragged companion opens at the default ratio")

	seam := m.geo.vDiv
	m, _ = step(t, m, click(seam, 5))
	require.Equal(t, dragVert, m.drag, "clicking the seam did not start a drag")
	m, _ = step(t, m, dragTo(90, 5))
	require.Equal(t, 90, m.geo.vDiv, "the seam did not follow the pointer")
	m, _ = step(t, m, mouseUp(90))
	require.Equal(t, dragNone, m.drag)

	wantRatio := ratioFull - ratioOf(90, 120)
	require.Equal(t, wantRatio, m.splits.ZoomDetail, "the ratio stored is the companion's own share")

	// A resize keeps the ratio, which recomputes a wider companion rather than
	// leaving it pinned at the cell count it was dragged to.
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 240, Height: 30})
	require.Equal(t, wantRatio, m.splits.ZoomDetail, "the stored ratio must not drift on its own")
	require.Equal(t, 60, m.geo.p[focusDetail].w, "the companion's width must scale with the frame")
	require.Equal(t, 180, m.geo.vDiv)
}

// TestAgentsDurColumnAdapts keeps the adaptive DUR column honest across the
// prompt and command panes.
func TestAgentsDurColumnAdapts(t *testing.T) {
	// No command carries a duration (the shape Claude Code's hook payload has, on
	// its own), so the DUR column is dropped from both panes.
	rows := mkRows("go build", "go test")
	for i := range rows {
		rows[i].Executor, rows[i].PromptID, rows[i].Prompt = "claude-code", "p1", "ship it"
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
		rows[i].Executor, rows[i].PromptID, rows[i].Prompt = "claude-code", "p1", long
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
		rows[i].Executor = "claude-code"
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

// TestAgentsKeyIsOnlyA proves `p` no longer opens the explorer: `a` owns it.
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

// TestSplitsPersistAndRestore proves a dragged layout is handed to SavePrefs once
// the drag settles, and that Options.Prefs restores it on the next run.
func TestSplitsPersistAndRestore(t *testing.T) {
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
	require.Equal(t, ratioOf(40, 120), saved[0].Splits.BrowseLeft)

	// A release that ends no drag writes nothing.
	_, cmd = step(t, m, mouseUp(40))
	require.Nil(t, cmd, "a stray release must not persist")

	// A fresh model restores that layout.
	m2 := NewModel(f, Options{Version: "v1", Now: now, Prefs: saved[0]})
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
// shows years at a glance, and it used to be gated on a pane 26 rows tall, so on
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

// manyProgramsRows returns count records, each a distinct program run with a
// distinct full command line ("progN --flag"), so every ranked list
// (programs by first token, commands by full line) ends up with count
// distinct entries instead of collapsing onto one.
func manyProgramsRows(count int) []rec.Record {
	rows := make([]rec.Record, count)
	for i := range rows {
		rows[i] = rec.Record{
			ID:       strconv.Itoa(i),
			Cmd:      "prog" + strconv.Itoa(i) + " --flag",
			Cwd:      "/work",
			Hostname: "boxA",
			StartMs:  now - int64(i)*3_600_000,
			Exit:     rec.IntPtr(0),
		}
	}
	return rows
}

// manyProgramsModel opens the stats view over count distinct programs at an
// explicit terminal size.
func manyProgramsModel(t *testing.T, w, h, count int) Model {
	t.Helper()
	f := &fakeBackend{
		hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: count}}},
		resp:  mkResp(manyProgramsRows(count)),
	}
	m := ready(t, f, w, h)
	m, cmd := step(t, m, press("s"))
	require.NotNil(t, cmd)
	sr, ok := cmd().(statsResultMsg)
	require.True(t, ok)
	m, _ = step(t, m, sr)
	return m
}

// TestStatsTallTerminalRendersMoreThan12Rows pins the fix for the ranked
// columns stopping dead at 12 rows: with more than 12 distinct programs held
// and a terminal tall enough to draw them, the aggregation must retain more
// than the old fixed cap and the renderer must actually draw them.
func TestStatsTallTerminalRendersMoreThan12Rows(t *testing.T) {
	const count = 40
	m := manyProgramsModel(t, 200, 120, count)

	require.Greaterf(t, len(m.stats.topPrograms), 12,
		"the aggregation must retain more than 12 distinct programs, got %d", len(m.stats.topPrograms))
	require.Greaterf(t, len(m.stats.topCommands), 12,
		"the aggregation must retain more than 12 distinct commands, got %d", len(m.stats.topCommands))

	out := strip(m.View())
	shown := strings.Count(out, "--flag")
	require.Greaterf(t, shown, 12,
		"a tall terminal must render more than 12 ranked rows, got %d:\n%s", shown, out)
}

// TestStatsShortTerminalDoesNotRegressWithManyEntries proves raising the
// aggregation's cap did not change short-terminal behavior: with the same
// 40-distinct-program fixture, a short terminal still shows only what fits
// (nowhere near every retained entry) and the ranked columns are not starved
// below what minColumnsH promises.
func TestStatsShortTerminalDoesNotRegressWithManyEntries(t *testing.T) {
	const count = 40
	m := manyProgramsModel(t, 100, 8, count)

	lines := strings.Split(m.View(), "\n")
	require.Len(t, lines, 8, "the view must render exactly the requested height")

	out := strip(m.View())
	require.Contains(t, out, "Top programs", "the ranked columns must survive a short terminal")

	shown := strings.Count(out, "--flag")
	require.Greaterf(t, shown, 0, "some ranked rows must still be visible, got %d:\n%s", shown, out)
	require.Lessf(t, shown, 12,
		"a short terminal must not spill every retained entry onto screen, got %d:\n%s", shown, out)
}

// TestStatsGrowTallerShowsMoreRowsWithoutRecompute pins the design decision
// that the aggregation's cap is height-independent: recomputeStats is not
// wired to tea.WindowSizeMsg (recomputing on every resize would be wasteful,
// and the sample the stats screen aggregates over does not change size), so
// growing the terminal must reveal more of the SAME retained data purely by
// the renderer drawing further down the same slices.
func TestStatsGrowTallerShowsMoreRowsWithoutRecompute(t *testing.T) {
	const count = 40
	m := manyProgramsModel(t, 200, 20, count)
	statsBefore := m.stats
	shortCount := strings.Count(strip(m.View()), "--flag")

	m, _ = step(t, m, tea.WindowSizeMsg{Width: 200, Height: 100})
	require.Same(t, statsBefore, m.stats, "growing the terminal must not recompute stats")

	tallCount := strings.Count(strip(m.View()), "--flag")
	require.Greaterf(t, tallCount, shortCount,
		"a taller terminal must render more ranked rows from the same data (%d -> %d)", shortCount, tallCount)
	require.Greater(t, tallCount, 12, "the taller terminal should exceed the old fixed cap")
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
// the period narrows: they exist to show activity AROUND the window.
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
// rolling 24 hours; otherwise the same clock hour appears twice in the
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
// zero; "it isn't 11pm yet" is not "nothing ran at 11pm".
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
// explorer asks for every row, so this should never fire in practice, but if a
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
// and fetches the aggregation sample the `s`/`a` keys would have fetched: this
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

// TestStartDevicesFetchesTheList is what `yore devices` rides on. It is its own
// test because the devices pane draws from a different fetch than the stats
// sample: without it the pane opens on "loading…" and never leaves.
func TestStartDevicesFetchesTheList(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		devices: proto.DevicesInfo{Devices: []proto.DeviceInfo{
			{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
		}},
	}
	m := NewModel(f, Options{Version: "v1", Now: now, Start: StartDevices})
	require.Equal(t, viewDevices, m.view, "Start=devices did not open the devices pane")

	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	mm, cmd := step(t, m, initMsg{})
	require.NotNil(t, cmd)
	var got bool
	for _, msg := range collect(cmd) {
		if dr, ok := msg.(devicesResultMsg); ok {
			got = true
			mm, _ = step(t, mm, dr)
		}
	}
	require.True(t, got, "opening on devices issued no device fetch")
	require.Containsf(t, strip(mm.View()), "laptop", "the pane rendered without its list")

	// Esc still drops through to the browse table, as from any other view.
	mm, _ = step(t, mm, press("esc"))
	require.Equal(t, viewBrowse, mm.view)
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
// the same place (hard against the right edge) so the filter never moves.
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

// devicesFixture is a devices view with one machine and three tokens in
// different states, already loaded.
func devicesFixture(t *testing.T) (Model, *fakeBackend) {
	t.Helper()
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		devices: proto.DevicesInfo{Devices: []proto.DeviceInfo{
			{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
		}},
		tokens: proto.TokensInfo{Tokens: []proto.EnrollToken{
			{ID: "aaaa111122223333", State: proto.TokenOpen, CreatedMs: now - 60_000, ExpiresMs: now + 600_000},
			{ID: "bbbb444455556666", State: proto.TokenClaimed, CreatedMs: now - 300_000,
				ClaimedMs: now - 240_000, ClaimedBy: "01CCCCCCCCCCCCCCCCCCCCCCCC"},
			{ID: "cccc777788889999", State: proto.TokenRevoked, CreatedMs: now - 900_000, RevokedMs: now - 800_000},
		}},
		minted: proto.TokenInfo{Token: "s3cret-enrollment-token", ExpiresMs: now + 1_800_000},
	}
	m := ready(t, f, 120, 30)
	m, cmd := step(t, m, press("D"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	return m, f
}

// TestTokensPaneShowsWhatBecameOfEach is the point of the pane: an open token
// is a standing invitation to join the group, and until now nothing said one
// existed.
func TestTokensPaneShowsWhatBecameOfEach(t *testing.T) {
	m, _ := devicesFixture(t)
	require.Len(t, m.tokens, 3)

	out := strip(m.View())
	require.Containsf(t, out, "MACHINES", "both panes should be titled:\n%s", out)
	require.Containsf(t, out, "TOKENS", "both panes should be titled:\n%s", out)
	for _, want := range []string{"open", "claimed", "revoked", "01CCCCCCCC"} {
		require.Containsf(t, out, want, "tokens pane missing %q:\n%s", want, out)
	}

	// Two panes, stacked, both full width and neither overlapping the other.
	g := m.geo
	require.Equal(t, 0, g.p[dpDevices].x)
	require.Equal(t, g.p[dpDevices].w, g.p[dpTokens].w, "both panes span the frame")
	require.Equal(t, g.p[dpDevices].y+g.p[dpDevices].h, g.p[dpTokens].y, "tokens sit under machines")
}

// TestTokensPaneRevoke covers x on each pane: the same key, aimed by focus at
// the thing the pane holds.
func TestTokensPaneRevoke(t *testing.T) {
	m, f := devicesFixture(t)

	// On the machines pane, x aims at the device, and this one is self, so it
	// is refused outright.
	m, _ = step(t, m, press("x"))
	require.Empty(t, m.devConfirm, "you cannot revoke the machine you are sitting at")

	// Tab to the tokens pane; x arms a token revoke, naming it as such.
	m, _ = step(t, m, press("tab"))
	require.Equal(t, dpTokens, m.dpane)
	m, _ = step(t, m, press("x"))
	require.Equal(t, "aaaa111122223333", m.devConfirm)
	require.Equal(t, dpTokens, m.devConfirmKind)
	require.Containsf(t, strip(m.View()), "revoke this token", "the prompt must say what it will revoke")
	require.Empty(t, f.tokRevoked, "nothing is revoked until the question is answered")

	m, cmd := step(t, m, press("y"))
	require.NotNil(t, cmd)
	cmd()
	require.Equal(t, []string{"aaaa111122223333"}, f.tokRevoked)
	require.Empty(t, f.revoked, "revoking a token must not revoke a device")

	// A claimed token has nothing to cancel: it already let someone in.
	m, _ = step(t, m, press("j"))
	m, _ = step(t, m, press("x"))
	require.Empty(t, m.devConfirm, "a claimed token cannot be revoked")
}

// TestTokensPaneMint proves n mints and puts the plaintext on screen: the one
// moment it exists, since the server keeps only a hash.
func TestTokensPaneMint(t *testing.T) {
	m, f := devicesFixture(t)
	m, _ = step(t, m, press("tab"))

	m, cmd := step(t, m, press("n"))
	require.NotNil(t, cmd, "n issued no mint")
	m, _ = step(t, m, cmd())
	require.Equal(t, 1, f.mintCalls)

	out := strip(m.View())
	require.Containsf(t, out, "s3cret-enrollment-token", "the minted token must be shown:\n%s", out)
	require.Containsf(t, out, "never shown again", "the pane must say it cannot be recovered:\n%s", out)

	// Esc dismisses it and leaves the view where it was.
	m, _ = step(t, m, press("esc"))
	require.Empty(t, m.minted)
	require.Equal(t, viewDevices, m.view, "dismissing the token is not leaving the view")
	require.NotContains(t, strip(m.View()), "s3cret-enrollment-token")
}

// TestMintedTokenCopies: the banner is the only moment a token's plaintext
// exists, and a narrow pane can clip it out of the mouse's reach, so y has to
// take it from the state rather than the screen.
func TestMintedTokenCopies(t *testing.T) {
	prev := localClipboardCopy
	localClipboardCopy = func(string) {}
	t.Cleanup(func() { localClipboardCopy = prev })

	m, _ := devicesFixture(t)
	// With nothing minted there is no secret here to copy, and y must not be
	// mistaken for an answer to a confirmation that was never asked.
	m, cmd := step(t, m, press("y"))
	require.Nil(t, cmd, "y with no minted token should do nothing")
	require.NotContains(t, strip(m.View()), "copied")

	m, cmd = step(t, m, press("n"))
	m, _ = step(t, m, cmd())
	require.Contains(t, footerText(m), "y copy the token", "the footer leads with it while it is up")

	var buf bytes.Buffer
	m.out = &buf
	m, cmd = step(t, m, press("y"))
	require.Contains(t, strip(m.View()), "token copied")
	require.Equal(t, "s3cret-enrollment-token", m.minted, "copying is not dismissing")

	batch, ok := cmd().(tea.BatchMsg)
	require.Truef(t, ok, "copy command yielded %T, want tea.BatchMsg", cmd())
	for _, c := range batch {
		if c != nil {
			c()
		}
	}
	require.Contains(t, buf.String(), osc52("s3cret-enrollment-token"))
}

// TestMintedTokenCopyYieldsToAConfirmation: y answers an armed confirmation
// before it copies. Both can be on screen at once (mint, then arm a revoke)
// and the destructive question is the one the footer is showing.
func TestMintedTokenCopyYieldsToAConfirmation(t *testing.T) {
	m, f := devicesFixture(t)
	m, cmd := step(t, m, press("n"))
	m, _ = step(t, m, cmd())
	require.NotEmpty(t, m.minted)

	m, _ = step(t, m, press("tab")) // to the tokens pane
	m, _ = step(t, m, press("x"))   // arm the cancel on the open token
	require.NotEmpty(t, m.devConfirm)

	m, cmd = step(t, m, press("y"))
	require.NotNil(t, cmd, "y should answer the confirmation")
	m, _ = step(t, m, cmd())
	require.NotEmpty(t, f.tokRevoked, "y confirmed the revoke rather than copying")
	require.NotContains(t, strip(m.View()), "token copied")
}

// setDevices replaces the backend's device list, standing in for a machine that
// enrolled elsewhere since this view was opened.
func (f *fakeBackend) setDevices(devs ...proto.DeviceInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = proto.DevicesInfo{Devices: devs}
}

// TestDevicesRefreshOnDemand: S refetches both lists, so a machine that enrolled
// elsewhere while you were looking at this screen appears without leaving it,
// and nothing moves under you until you ask for it.
func TestDevicesRefreshOnDemand(t *testing.T) {
	m, f := devicesFixture(t)
	require.Len(t, m.devices, 1)

	f.setDevices(
		proto.DeviceInfo{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
		proto.DeviceInfo{ID: "01DDDDDDDDDDDDDDDDDDDDDDDD", Name: "desktop", Status: "pending", Code: "AB12-CD34"},
	)
	require.Len(t, m.devices, 1, "the view does not refetch on its own")

	m, cmd := step(t, m, press("S"))
	require.NotNil(t, cmd, "S should refetch both lists")
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Len(t, m.devices, 2, "S found the machine that enrolled elsewhere")
	require.Contains(t, strip(m.View()), "desktop")
}

// TestOpenTokenCountsDownToItsExpiry: a token's remaining life runs FORWARD.
// Rendering it with the past-tense formatter clamped every future time to zero,
// so a token with half an hour left announced that it "expires now".
func TestOpenTokenCountsDownToItsExpiry(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		tokens: proto.TokensInfo{Tokens: []proto.EnrollToken{
			{ID: "aaaa111122223333", State: proto.TokenOpen, CreatedMs: now - 300_000, ExpiresMs: now + 1_500_000},
		}},
	}
	m := ready(t, f, 120, 30)
	m, cmd := step(t, m, press("D"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}

	l := m.tableLayout(ctTokens, 100)
	row := strip(m.tokenRow(m.tokens[0], l, false, 100, m.now()))
	require.Containsf(t, row, "25m", "a token minted 5m ago with a 30m life has 25m left:\n%s", row)
	require.NotContainsf(t, row, "now", "it does not expire now:\n%s", row)
}

// TestDetailPaneSurvivesTheDevicesView: the detail pane belongs to the browse
// view, so it is sized from that view's geometry even while another view is on
// screen. Sizing it from whatever was on screen squashed it to one column in the
// devices view, and any refresh landing in that moment re-wrapped its content
// one character per line, which is the state you came back to.
func TestDetailPaneSurvivesTheDevicesView(t *testing.T) {
	const cmd = "go test ./internal/tui/browse"
	f := &fakeBackend{resp: mkResp(mkRows(cmd))}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	want := m.detail.Width
	require.Greater(t, want, 20, "the browse detail pane is wider than a column")

	m, _ = step(t, m, press("D"))
	require.Equal(t, viewDevices, m.view)
	require.Equal(t, want, m.detail.Width, "the detail pane keeps its width while another view is up")

	// A background refresh lands while the devices view is up: the moment that
	// used to bake the one-column wrapping in.
	m, _ = step(t, m, queryResultMsg{seq: 2, resp: f.resp})
	m, _ = step(t, m, press("esc"))

	out := strip(m.View())
	require.Containsf(t, out, cmd, "the command should be on one line, not stacked:\n%s", out)
}

// TestDevicesViewMouse: the devices view answers the mouse the way every other
// multi-pane view does; click to focus, wheel to scroll the pane under the
// pointer without stealing focus, drag the seam to resize. It used to fall
// through to the browse arm of every one of those, so a wheel over the machine
// list moved the *browse* host sidebar and re-ran its query, and dragging the
// seam moved the browse view's seam instead of this one's.
func TestDevicesViewMouse(t *testing.T) {
	f := &fakeBackend{
		resp: mkResp(mkRows("ls")),
		devices: proto.DevicesInfo{Devices: []proto.DeviceInfo{
			{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", Status: "active", Self: true},
			{ID: "01BBBBBBBBBBBBBBBBBBBBBBBB", Name: "server", Status: "active"},
			{ID: "01CCCCCCCCCCCCCCCCCCCCCCCC", Name: "builder", Status: "active"},
		}},
		tokens: proto.TokensInfo{Tokens: []proto.EnrollToken{
			{ID: "aaaa111122223333", State: proto.TokenOpen, CreatedMs: now - 60_000, ExpiresMs: now + 600_000},
			{ID: "bbbb444455556666", State: proto.TokenOpen, CreatedMs: now - 300_000, ExpiresMs: now + 300_000},
		}},
	}
	m := ready(t, f, 120, 30)
	m, cmd := step(t, m, press("D"))
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Equal(t, dpDevices, m.dpane)
	hostSel, browseSel := m.hostSel, m.sel

	// Click into the tokens pane: same effect as tab, so the pane keys can be
	// aimed with the mouse.
	tok := m.geo.p[dpTokens]
	m, _ = step(t, m, click(tok.x+10, tok.y+3))
	require.Equal(t, dpTokens, m.dpane, "clicking the tokens pane did not focus it")

	// The wheel scrolls the pane under the pointer and leaves focus alone.
	dev := m.geo.p[dpDevices]
	m, _ = step(t, m, wheelAt(dev.x+10, dev.y+3, tea.MouseButtonWheelDown))
	require.Positive(t, m.devSel, "the wheel did not scroll the machine list")
	require.Equal(t, dpTokens, m.dpane, "the wheel must not move focus")
	m, _ = step(t, m, wheelAt(dev.x+10, dev.y+3, tea.MouseButtonWheelUp))
	require.Zero(t, m.devSel, "the wheel did not scroll back up")

	m, _ = step(t, m, wheelAt(tok.x+10, tok.y+3, tea.MouseButtonWheelDown))
	require.Positive(t, m.tokSel, "the wheel did not scroll the token list")

	// None of it touched the browse view underneath.
	require.Equal(t, hostSel, m.hostSel, "the wheel must not move the browse sidebar")
	require.Equal(t, browseSel, m.sel, "the wheel must not move the browse table")

	// The seam between the two panes drags, and it is THIS view's seam.
	seam := m.geo.hDiv
	m, _ = step(t, m, click(30, seam))
	require.Equal(t, dragHoriz, m.drag, "clicking the seam did not start a drag")
	m, _ = step(t, m, dragTo(30, 18))
	require.Equal(t, 18, m.geo.hDiv, "the seam did not follow the pointer")
	require.Zero(t, m.splits.BrowseTop, "the browse view's seam must not have moved")
	require.Equal(t, ratioOf(17, m.midHeight), m.splits.DevicesTop)
	m, _ = step(t, m, mouseUp(30))
	require.Equal(t, dragNone, m.drag, "releasing did not end the drag")

	// The ratio survives a resize, like every other seam.
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 60})
	require.Equal(t, ratioOf(17, 27), m.splits.DevicesTop)
	require.Greater(t, m.geo.hDiv, 18, "the split is a proportion, not a row number")

	// And a zoomed pane still takes the wheel: the zoomed rect is parked at that
	// pane's own index, not at 0.
	m, _ = step(t, m, press("z"))
	require.True(t, m.zoom)
	require.Equal(t, dpTokens, m.dpane, "this is only meaningful off index 0")
	before := m.tokSel
	m, _ = step(t, m, wheelAt(10, 5, tea.MouseButtonWheelUp))
	require.Less(t, m.tokSel, before, "the wheel must still scroll a zoomed devices pane")
	m, _ = step(t, m, click(10, 5))
	require.True(t, m.zoom, "clicking inside a zoomed pane must not drop the zoom")
}

// TestMintCannotBeSpammed: every press of n mints a REAL token; a standing
// invitation into everything the group can read. Holding the key used to issue
// one request per repeat, leaving that many live on the server and burying each
// banner's plaintext under the next, which is the only copy there will ever be.
func TestMintCannotBeSpammed(t *testing.T) {
	m, f := devicesFixture(t)
	m, _ = step(t, m, press("tab"))

	// The first press mints; the ones on top of it, while that request is still
	// in flight, do nothing at all.
	m, cmd := step(t, m, press("n"))
	require.NotNil(t, cmd, "the first n should mint")
	for range 5 {
		var again tea.Cmd
		m, again = step(t, m, press("n"))
		require.Nil(t, again, "a mint already in flight must swallow further presses")
	}
	m, _ = step(t, m, cmd())
	require.Equal(t, 1, f.mintCalls, "a held key must mint exactly once")
	require.NotEmpty(t, m.minted)

	// With the token still on screen, n says why it will not mint another rather
	// than replacing a secret that exists nowhere else.
	m, cmd = step(t, m, press("n"))
	require.NotNil(t, cmd, "the refusal should flash")
	require.Equal(t, 1, f.mintCalls, "n must not clobber the banner")
	require.Contains(t, strip(m.View()), "dismiss this token first")

	// Dismissing it makes n live again; minting a second token is a deliberate
	// second act.
	m, _ = step(t, m, press("esc"))
	require.Empty(t, m.minted)
	m, cmd = step(t, m, press("n"))
	require.NotNil(t, cmd)
	m, _ = step(t, m, cmd())
	require.Equal(t, 2, f.mintCalls)
}

// TestFailedMintReleasesTheKey: the guard is cleared by the request landing, not
// by it succeeding; otherwise one failed mint would disable n for the session.
func TestFailedMintReleasesTheKey(t *testing.T) {
	m, _ := devicesFixture(t)
	m, _ = step(t, m, press("n"))
	require.True(t, m.minting)

	m, _ = step(t, m, mintedMsg{err: errors.New("daemon unreachable")})
	require.False(t, m.minting, "a failed mint must not wedge the key")
	m, cmd := step(t, m, press("n"))
	require.NotNil(t, cmd, "n should work again after a failure")
}

// TestDevicesRefreshSaysSo: S reports itself the way the browse view's sync
// does; a "refreshing…" while it runs, a "✓ refreshed" when it lands. A
// refetch that finished silently was indistinguishable from a dead key, which
// is exactly what it looks like when nothing has changed.
func TestDevicesRefreshSaysSo(t *testing.T) {
	m, _ := devicesFixture(t)

	m, cmd := step(t, m, press("S"))
	require.True(t, m.devRefreshing)
	require.Contains(t, strip(m.View()), "refreshing…", "S should say it is working")

	// A second S while the first is in flight is swallowed, not stacked.
	_, again := step(t, m, press("S"))
	require.Nil(t, again, "a refresh already in flight must swallow further presses")

	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.False(t, m.devRefreshing)
	require.Contains(t, strip(m.View()), "✓ refreshed", "S should report that it landed")

	// The view's own first load is not an S, so it announces nothing.
	m2, _ := devicesFixture(t)
	require.NotContains(t, strip(m2.View()), "refreshed", "loading the view is not a refresh")
}
