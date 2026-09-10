package browse

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/proto"
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
	require.NotNil(t, m.bulk, "the batch is in flight until its result comes back")

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
	require.Nil(t, m.bulk)
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
	require.NotNil(t, m.bulk)

	m, cmd := step(t, m, press("S"))
	require.Nil(t, cmd, "S must not re-query while the tags are still going out")
	require.Zero(t, f.syncCount())

	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	require.Nil(t, m.bulk, "the tag batch releases the keys on its own result")
}

// --- stopping a batch, and watching one ----------------------------------
// A confirmed batch is unbounded: buildReq asks for every matching row, so
// ctrl+a can commit to as many round trips as the archive has records. That
// makes two things load-bearing rather than nice to have: a way out, and some
// sign of how far it has got.

// TestEscStopsABatchWhereItIs: esc during a run stops it before the next call.
// What already went to the daemon stands (a tombstone is not the browser's to
// take back), the rows it never reached are untouched and still checked, and
// the flash gives both numbers rather than claiming the whole selection.
func TestEscStopsABatchWhereItIs(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c", "d", "e"))})
	m, _ = step(t, m, press("ctrl+a"))

	// esc goes through the real key path, from inside the third call: a batch is
	// stopped between round trips, so that call still lands and the two after it
	// are never sent. Which ids those are is not fixed (the batch walks the
	// checked set, which is a map), so the count is the assertion and the
	// survivors are read back from what the fake was actually asked to delete.
	calls := 0
	f.onDelete = func(string) {
		calls++
		if calls == 3 {
			m, _ = step(t, m, press("esc"))
			require.Contains(t, strip(m.View()), "stopping…", "esc has to say something at once")
		}
	}

	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))
	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}

	require.Len(t, f.deleted, 3, "the calls after the stop must never be sent")
	require.Contains(t, strip(m.View()), "stopped: deleted 3 of 5",
		"a stopped run must report what it did and what it was asked for")
	require.Nil(t, m.bulk, "a stopped run still ends, and releases the keys")

	gone := map[string]bool{}
	for _, id := range f.deleted {
		gone[id] = true
	}
	require.Len(t, m.rows, 2, "the rows the batch never reached must be untouched")
	for _, r := range m.rows {
		require.False(t, gone[r.ID], "a deleted row is still in the table")
		require.Truef(t, m.isChecked(r.ID),
			"row %s was never reached, so it stays checked and d picks up where it left off", r.ID)
	}
}

// esc is a key held down as often as any other; asking to stop twice must not
// take the browser down with it.
func TestEscTwiceDuringABatchIsHarmless(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))

	m, _ = step(t, m, press("esc"))
	m, _ = step(t, m, press("esc"))
	require.Contains(t, strip(m.View()), "stopping…")

	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	require.Nil(t, m.bulk)
	require.Empty(t, f.deleted, "a stop before the first call must delete nothing at all")
	require.Contains(t, strip(m.View()), "stopped: deleted 0 of 2")
}

// TestABatchShowsItsProgress: the worker cannot reach the event loop, so the
// loop reads its counter on a tick. The count has to move, and the ticking has
// to stop with the run rather than outliving it for the session.
func TestABatchShowsItsProgress(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))
	require.Contains(t, strip(m.View()), "deleting 0/3… esc to stop", "a batch starts by saying what it is doing")

	m.bulk.done.Store(2) // two round trips have come back
	m, tick := step(t, m, bulkTickMsg{})
	require.Contains(t, strip(m.View()), "deleting 2/3…", "the tick must redraw the count the worker is keeping")
	require.NotNil(t, tick, "the tick chain has to reschedule itself while the run lasts")

	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	m, tick = step(t, m, bulkTickMsg{})
	require.Nil(t, tick, "a tick arriving after the run must not keep the chain alive")
	require.Contains(t, strip(m.View()), "✓ deleted 3 records", "the result has the last word on the flash")
}

// The footer says so too: while a batch runs those are the only two keys the
// browser answers to, and a stop nobody can find is not a stop.
func TestFooterOffersTheStopWhileABatchRuns(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})
	m, _ = step(t, m, press("ctrl+a"))
	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))

	foot := footerText(m)
	require.Contains(t, foot, "esc stop")
	require.Contains(t, foot, "quit")
	require.NotContains(t, foot, "delete", "the keys that act on rows are not live and must not be offered")

	for _, msg := range collect(batch) {
		m, _ = step(t, m, msg)
	}
	require.NotContains(t, footerText(m), "esc stop", "the footer goes back to the table's own keys")
}

// --- the same hazard in the other sequence -------------------------------
// The stats sample is a second query, numbered in its own sequence, and it
// feeds both the stats screen and every list in the agent explorer. One of
// those in flight when a delete lands carries the deleted commands into the
// aggregates, where nothing else would take them out again until the view is
// next re-entered.

func TestTheStatsSampleIgnoresAnAnswerThatPredatesADelete(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("alpha", "beta", "gamma"))}
	m := midFlight(t, f)

	// Opening the agent explorer puts a sample in flight; come back to the table
	// without answering it.
	m, _ = step(t, m, press("a"))
	inFlight := m.statsSeq
	require.NotZero(t, inFlight, "opening the explorer has to have asked for a sample")
	m, _ = step(t, m, press("a"))

	m, _ = step(t, m, press("d"))
	m, _ = step(t, m, press("y"))
	require.Len(t, f.deleted, 1)

	// The sample answers with what the daemon held before the delete.
	m, _ = step(t, m, statsResultMsg{seq: inFlight, resp: f.resp})

	require.Len(t, m.statsRows, 2, "the deleted command must not reach the stats screen or the explorer")
	require.Equal(t, 2, m.statsTotal, "nor be counted in the total")
	for _, r := range m.statsRows {
		require.NotEqual(t, f.deleted[0], r.ID, "a deleted command is in the sample")
	}
}

// A delete leaves a mark in both sequences and the ids live until both have
// answered past their own: a fresh answer to the table says nothing about what
// a sample still in flight is carrying.
func TestTheIdsLiveUntilBothSequencesHaveAnswered(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("alpha", "beta", "gamma"))}
	m := midFlight(t, f)

	m, _ = step(t, m, press("a")) // a sample goes out
	staleStats := m.statsSeq
	m, _ = step(t, m, press("a"))
	m, _ = step(t, m, hostsTickMsg{}) // and a table query
	m, _ = step(t, m, press("ctrl+a"))
	m = runBulkDelete(t, m)
	require.NotEmpty(t, m.gone)

	// The table settles first. The sample is still out, so the ids stay.
	m, _ = step(t, m, hostsTickMsg{})
	m, _ = step(t, m, queryResultMsg{seq: m.seq, resp: mkResp(nil)})
	require.NotEmpty(t, m.gone, "a fresh table answer must not release ids the sample still needs")

	// The stale sample is filtered on the strength of that second mark...
	m, _ = step(t, m, statsResultMsg{seq: staleStats, resp: f.resp})
	require.Empty(t, m.statsRows, "every command in the stale sample had been deleted")

	// ...and a sample newer than the delete settles the last of them.
	m, _ = step(t, m, statsResultMsg{seq: staleStats + 1, resp: mkResp(nil)})
	require.Empty(t, m.gone, "with both sequences answered, the ids are no longer worth holding")
}

// TestABatchAndTheLoopCrossGoroutinesCleanly runs the worker where bubbletea
// runs it. Everywhere else a command is drained by calling it on the test
// goroutine, which is convenient and proves nothing about the handoff: the
// counter and the stop are the whole contract between the two, and only running
// them apart puts either under the race detector.
func TestABatchAndTheLoopCrossGoroutinesCleanly(t *testing.T) {
	release := make(chan struct{})
	f := &fakeBackend{}
	f.onDelete = func(string) { <-release } // hold the worker inside each call

	m := ready(t, f, 120, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c", "d"))})
	m, _ = step(t, m, press("ctrl+a"))
	m, _ = step(t, m, press("d"))
	m, batch := step(t, m, press("y"))
	run := m.bulk
	require.NotNil(t, run)

	// Every cmd in the batch runs on its own goroutine, and the worker is picked
	// out by what it answers rather than by its place in the batch: the order
	// bubbletea keeps them in is not this test's to depend on.
	cmds, ok := batch().(tea.BatchMsg)
	require.True(t, ok)
	out := make(chan tea.Msg, len(cmds))
	for _, c := range cmds {
		go func() { out <- c() }()
	}
	done := func() tea.Msg {
		for msg := range out {
			if d, isDone := msg.(bulkDeleteDoneMsg); isDone {
				return d
			}
		}
		return nil
	}

	// One call through, and the loop watches the count move while the worker is
	// still going.
	release <- struct{}{}
	require.Eventually(t, func() bool { return run.done.Load() == 1 }, 2*time.Second, time.Millisecond,
		"the loop must see the worker's progress while the run is in flight")
	m, _ = step(t, m, bulkTickMsg{})
	require.Contains(t, strip(m.View()), "deleting 1/4…")

	// Stopped from the loop while the worker sits inside a call.
	m, _ = step(t, m, press("esc"))
	close(release) // that call finishes; the two after it are never sent

	m, _ = step(t, m, done())
	require.Nil(t, m.bulk)
	require.Len(t, f.deleted, 2, "the call in flight lands, and the run stops there")
	require.Contains(t, strip(m.View()), "stopped: deleted 2 of 4 records")
}
