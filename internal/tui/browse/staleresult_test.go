package browse

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
)

// A delete has to survive a query that was already in flight when it ran.
// appliedSeq orders deliveries, not the daemon state each query observed: an
// answer computed before the tombstones lands after them, carries a higher seq
// than anything applied so far, and is accepted on that basis. What the user
// sees is the rows they just deleted back in the table, unchecked (pruneChecked
// only removes), with the flash still saying they went. The records really are
// gone from the daemon, so the table is simply lying until something re-queries.
//
// The window is a real one: a bulk delete runs its round trips off the event
// loop precisely so the browser keeps redrawing, and the init-time warm loop
// re-queries once a second for the first several seconds of every session.

// midFlight opens a browser whose warm loop is still running, so a test can put
// a query in flight the way that loop does. The fake answers every query with
// the same rows, which is the case that matters here: the daemon has not yet
// seen the delete, so it hands back exactly what it did before.
func midFlight(t *testing.T, f *fakeBackend) Model {
	t.Helper()
	f.resp.Remote = proto.RemoteInfo{State: proto.RemoteSyncing} // keeps the loop ticking
	m := NewModel(f, Options{Now: now})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = step(t, m, initMsg{})
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: f.resp})
	return m
}

func TestBulkDeleteIgnoresAnAnswerThatPredatesIt(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("alpha", "beta", "gamma"))}
	m := midFlight(t, f)

	// A warm-loop tick issues a query and leaves it unanswered.
	m, _ = step(t, m, hostsTickMsg{})
	require.True(t, m.ticking, "the warm loop has to still be running for this test to mean anything")
	inFlight := m.seq

	// The whole table is deleted while that query is out.
	m, _ = step(t, m, press("ctrl+a"))
	m = runBulkDelete(t, m)
	require.Len(t, f.deleted, 3)
	require.Empty(t, m.rows)

	// Its answer arrives last, still carrying every deleted row.
	m, _ = step(t, m, queryResultMsg{seq: inFlight, resp: f.resp})

	require.Empty(t, m.rows, "an answer computed before the delete must not put the rows back")
	require.Empty(t, m.allRows, "allRows too, or the next period switch resurrects them")
	require.Zero(t, m.srvTotal, "the count must not report rows the daemon has tombstoned")
	require.NotContains(t, strip(m.View()), "alpha", "a deleted command is back on screen")
}

// The single-row path takes its delete inline on the event loop, which protects
// it from nothing: the hazard is a query issued BEFORE the keypress, and one of
// those is in flight for the first several seconds of every session. Only the
// deleted row is filtered; the rest of the answer applies normally.
func TestSingleDeleteIgnoresAnAnswerThatPredatesIt(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("alpha", "beta", "gamma"))}
	m := midFlight(t, f)

	m, _ = step(t, m, hostsTickMsg{})
	inFlight := m.seq

	m, _ = step(t, m, press("d"))
	m, _ = step(t, m, press("y")) // the cursor is on alpha
	require.Equal(t, []string{"0"}, f.deleted)

	m, _ = step(t, m, queryResultMsg{seq: inFlight, resp: f.resp})

	require.Len(t, m.rows, 2, "the deleted row must stay gone and the other two must still apply")
	require.Equal(t, 2, m.srvTotal)
	for _, r := range m.rows {
		require.NotEqual(t, "alpha", r.Cmd, "the deleted command is back in the table")
	}
}

// The filter lasts exactly as long as the query it guards against. Once an
// answer arrives from a query issued after the delete, that answer reflects the
// tombstones (Delete had returned before it was asked) and the ids are dropped:
// otherwise the browser carries a set that only ever grows, filtering every
// result for the rest of the session against records the daemon stopped
// returning long ago.
func TestALaterAnswerReleasesTheFilter(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("alpha", "beta", "gamma"))}
	m := midFlight(t, f)

	m, _ = step(t, m, hostsTickMsg{})
	stale := m.seq
	m, _ = step(t, m, press("ctrl+a"))
	m = runBulkDelete(t, m)
	require.NotEmpty(t, m.gone, "the deleted ids are held while the stale query is still out")

	// The stale answer is filtered, and the filter stays up: it was issued
	// before the delete, so nothing about it says the deletes have landed.
	m, _ = step(t, m, queryResultMsg{seq: stale, resp: f.resp})
	require.NotEmpty(t, m.gone)

	// A query issued after the delete settles it either way.
	m, _ = step(t, m, hostsTickMsg{})
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: mkResp(nil)})
	require.Empty(t, m.gone, "an answer newer than the delete must release the ids, not accumulate them")
}

// A batch runs off the event loop, so every key stays live over a table it is
// halfway through changing. They are swallowed for the duration: a second d/y
// would start an overlapping batch, S would re-query mid-run, and enter would
// leave the browser with the tail of the deletes unmade.
func TestKeysAreSwallowedWhileABatchRuns(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha", "beta", "gamma"))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))
	require.True(t, m.bulkBusy, "the batch is in flight until its result comes back")

	m, cmd := step(t, m, press("S"))
	require.Nil(t, cmd, "S must not re-query over a table the batch is still changing")
	require.Zero(t, f.syncCount())

	m, _ = step(t, m, press("d"))
	require.False(t, m.confirmDelete, "a second delete prompt must not open over a running batch")

	m, _ = step(t, m, press("enter"))
	require.False(t, m.quitting, "enter must not leave the browser mid-batch")
	require.Empty(t, m.accepted)

	// The result releases them again.
	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	require.False(t, m.bulkBusy)
	m, cmd = step(t, m, press("S"))
	require.NotNil(t, cmd, "keys have to work again once the batch is done")
	require.Contains(t, strip(m.View()), "syncing")
}

// The interrupt is the exception. A run over an archive can be long, and
// refusing to let the user out of it is worse than leaving the rest unmade.
func TestInterruptStillQuitsDuringABatch(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha", "beta"))})
	m, _ = step(t, m, press("ctrl+a"))
	m, _ = step(t, m, press("d"))
	m, _ = step(t, m, press("y"))

	m, cmd := step(t, m, press("ctrl+c"))
	require.True(t, m.quitting, "ctrl+c must still work while a batch runs")
	require.NotNil(t, cmd)
}

// Bulk tag holds the same lock, for the same reason, and releases it on its own
// result rather than on delete's.
func TestKeysAreSwallowedWhileABulkTagRuns(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("alpha", "beta"))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("ctrl+t"))
	m = typeIn(t, m, "wip")
	m, batch := step(t, m, press("enter"))
	require.True(t, m.bulkBusy)

	m, cmd := step(t, m, press("S"))
	require.Nil(t, cmd, "S must not re-query while the tags are still going out")
	require.Zero(t, f.syncCount())

	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	require.False(t, m.bulkBusy, "the tag batch releases the keys on its own result")
}
