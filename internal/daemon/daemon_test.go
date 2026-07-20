package daemon

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/spool"
	"yore/internal/store"
)

// daemonHandle observes a Run goroutine's completion. done is closed once Run
// returns; err is then safe to read (the close provides happens-before).
type daemonHandle struct {
	done chan struct{}
	err  error
}

// startDaemon launches Run in a goroutine and returns a connected client once
// the socket is up. A t.Cleanup shuts the daemon down if it is still running.
func startDaemon(t *testing.T, dir string, idle time.Duration) (*Client, *daemonHandle) {
	t.Helper()
	h := &daemonHandle{done: make(chan struct{})}
	go func() {
		// No test assertions in this goroutine: the outcome is stashed in h.err
		// and only read on the main goroutine after <-h.done (which provides the
		// happens-before). testify's FailNow is illegal off the main goroutine.
		h.err = Run(dir, Options{IdleTimeout: idle, Version: "test-ver"})
		close(h.done)
	}()

	c := dialRetry(t, dir, 2*time.Second)
	t.Cleanup(func() {
		select {
		case <-h.done:
			c.Close()
			return
		default:
		}
		if sc, err := Dial(dir); err == nil {
			_ = sc.Shutdown()
			_ = sc.Close()
		}
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			assert.Fail(t, "daemon did not shut down in cleanup")
		}
		c.Close()
	})
	return c, h
}

func dialRetry(t *testing.T, dir string, within time.Duration) *Client {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := Dial(dir); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNowf(t, "could not dial daemon", "within %s", within)
	return nil
}

// rawRequest sends one request on a fresh connection and returns the response.
// Used to exercise ops the Client type does not expose (e.g. OpRecord).
func rawRequest(t *testing.T, dir string, req proto.Request) proto.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 500*time.Millisecond)
	require.NoError(t, err, "dial")
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, proto.WriteMsg(conn, req), "write")
	var resp proto.Response
	require.NoError(t, proto.ReadMsg(bufio.NewReader(conn), &resp), "read")
	return resp
}

// queryUntil polls Query(q) on c until it returns >= want rows or timeout. It
// runs entirely on the caller's (main) goroutine, so require is safe here.
func queryUntil(t *testing.T, c *Client, q proto.QueryReq, want int, within time.Duration) proto.QueryResp {
	t.Helper()
	deadline := time.Now().Add(within)
	var last proto.QueryResp
	for time.Now().Before(deadline) {
		resp, err := c.Query(q)
		require.NoError(t, err, "query")
		last = resp
		if len(resp.Rows) >= want {
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(config.SpoolDir(dir), "*.jsonl"))
	require.NoError(t, err, "glob")
	return m
}

// seed opens the store, appends rows, and closes it — so the daemon can then
// take ownership and load them into the corpus.
func seed(t *testing.T, dir string, rows []rec.Record) {
	t.Helper()
	s, err := store.Open(dir)
	require.NoError(t, err, "store.Open")
	_, err = s.AppendBatch(rows)
	require.NoError(t, err, "AppendBatch")
	require.NoError(t, s.Close(), "store.Close")
}

func TestEnsureRunningDialSuccess(t *testing.T) {
	dir := t.TempDir()
	_, h := startDaemon(t, dir, 30*time.Second)

	// The daemon is already up, so EnsureRunning takes the dial-success path
	// and never spawns (which, under test, would re-exec the test binary).
	c, err := EnsureRunning(dir)
	require.NoError(t, err, "EnsureRunning")
	defer c.Close()
	require.NoError(t, c.Ping(), "Ping")
	_ = h
}

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []rec.Record{
		{ID: "a", Cmd: "echo one", StartMs: 1},
		{ID: "b", Cmd: "echo two", StartMs: 2},
	})
	c, _ := startDaemon(t, dir, 30*time.Second)

	st, err := c.Status()
	require.NoError(t, err, "Status")
	assert.Positive(t, st.PID, "PID")
	assert.Equal(t, 2, st.LocalRows, "LocalRows")
	assert.Equal(t, "test-ver", st.Version, "Version")
	assert.Equal(t, proto.RemoteOff, st.Remote.State, "Remote.State")
}

func TestRecordDurableAndIngested(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)

	resp := rawRequest(t, dir, proto.Request{
		Op:     proto.OpRecord,
		Record: &rec.Record{ID: "r1", Cmd: "echo durable", StartMs: 1000},
	})
	require.Truef(t, resp.OK, "OpRecord resp not ok: %+v", resp)

	got := queryUntil(t, c, proto.QueryReq{Q: "durable"}, 1, time.Second)
	require.Lenf(t, got.Rows, 1, "record not queryable after debounce: %+v", got.Rows)
	require.Equal(t, "echo durable", got.Rows[0].Cmd)

	// After ingest the spool must be drained (records live durably in the store).
	assert.Empty(t, spoolFiles(t, dir), "spool not drained")
}

func TestPokeIngest(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)

	// Simulate the CLI: fsync a record into the spool directly, then poke.
	require.NoError(t, spoolAppendDirect(dir, rec.Record{ID: "p1", Cmd: "poked command", StartMs: 5}), "spool append")
	require.NoError(t, c.Ping(), "Ping")

	got := queryUntil(t, c, proto.QueryReq{Q: "poked"}, 1, time.Second)
	require.Lenf(t, got.Rows, 1, "poked record not queryable: %+v", got.Rows)
	require.Equal(t, "poked command", got.Rows[0].Cmd)
}

func TestQuerySemantics(t *testing.T) {
	dir := t.TempDir()
	// r1/r3 share the cmd "git status" (dedupe target). StartMs is deliberately
	// not seq-monotonic to prove ordering sorts by StartMs, not insertion.
	seed(t, dir, []rec.Record{
		{ID: "r1", Cmd: "git status", Session: "s1", Cwd: "/a", StartMs: 1000},
		{ID: "r2", Cmd: "git commit", Session: "s1", Cwd: "/b", StartMs: 2000},
		{ID: "r3", Cmd: "git status", Session: "s2", Cwd: "/a", StartMs: 3000},
		{ID: "r4", Cmd: "ls -la", Session: "s2", Cwd: "/a", StartMs: 4000},
		{ID: "r5", Cmd: "git push", Session: "s1", Cwd: "/a", StartMs: 5000},
	})
	c, _ := startDaemon(t, dir, 30*time.Second)

	// Match + newest-first ordering (local scope). Independent field checks are
	// accumulated with assert.
	resp, err := c.Query(proto.QueryReq{Q: "git"})
	require.NoError(t, err)
	assert.Equal(t, 4, resp.Total, "Total")
	assert.Equal(t, proto.ScopeLocal, resp.Scope, "Scope")
	assert.Equal(t, []string{"r5", "r3", "r2", "r1"}, ids(resp.Rows), "order")

	// Dedupe collapses "git status" (r1/r3) to the newest (r3).
	resp, _ = c.Query(proto.QueryReq{Q: "git", Dedupe: true})
	assert.Equal(t, 3, resp.Total, "dedupe Total")
	assert.Equal(t, []string{"r5", "r3", "r2"}, ids(resp.Rows), "dedupe order")

	// Session scope keeps only s1.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeSession, Session: "s1"})
	assert.Equal(t, []string{"r5", "r2", "r1"}, ids(resp.Rows), "session scope")

	// Cwd scope keeps only /a.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeCwd, Cwd: "/a"})
	assert.Equal(t, []string{"r5", "r3", "r1"}, ids(resp.Rows), "cwd scope")

	// Offset/limit window applies after dedupe; Total is unwindowed.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Limit: 2, Offset: 1})
	assert.Equal(t, 4, resp.Total, "windowed Total")
	assert.Equal(t, []string{"r3", "r2"}, ids(resp.Rows), "window")

	// "all"/"host" behave like local but report Remote off.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeAll})
	assert.Equal(t, 4, resp.Total, "scope all Total")
	assert.Equal(t, proto.RemoteOff, resp.Remote.State, "scope all Remote")
	assert.Equal(t, proto.ScopeAll, resp.Scope, "scope all effective scope")
}

func TestIncrementalQueriesConsistent(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []rec.Record{
		{ID: "r1", Cmd: "git status", StartMs: 1000},
		{ID: "r2", Cmd: "git commit -m x", StartMs: 2000},
		{ID: "r3", Cmd: "grep foo", StartMs: 3000},
		{ID: "r4", Cmd: "git push origin", StartMs: 4000},
	})
	c, _ := startDaemon(t, dir, 30*time.Second)

	// Type "g" -> "gi" -> "git" down one connection (incremental Filter path).
	for _, q := range []string{"g", "gi", "git"} {
		_, err := c.Query(proto.QueryReq{Q: q})
		require.NoErrorf(t, err, "incremental query %q", q)
	}
	inc, _ := c.Query(proto.QueryReq{Q: "git"})

	// Compare against a fresh connection's full-scan result.
	fresh, _ := startDaemonClient(t, dir)
	full, _ := fresh.Query(proto.QueryReq{Q: "git"})
	fresh.Close()

	assert.Equal(t, ids(full.Rows), ids(inc.Rows), "incremental != full")
	assert.Equal(t, []string{"r4", "r2", "r1"}, ids(inc.Rows), "git rows")
}

func TestIdleExit(t *testing.T) {
	dir := t.TempDir()
	c, h := startDaemon(t, dir, 300*time.Millisecond)

	require.NoError(t, c.Ping(), "Ping")
	start := time.Now()

	select {
	case <-h.done:
		require.NoError(t, h.err, "Run returned error")
		assert.LessOrEqual(t, time.Since(start), 1500*time.Millisecond, "idle exit took too long, want < 1.5s")
	case <-time.After(1500 * time.Millisecond):
		require.FailNow(t, "Run did not return after idle timeout")
	}

	_, err := os.Stat(config.SocketPath(dir))
	assert.Truef(t, os.IsNotExist(err), "socket still present after idle exit: %v", err)
}

func TestSecondRunLockedLosesQuietly(t *testing.T) {
	dir := t.TempDir()
	_, _ = startDaemon(t, dir, 30*time.Second) // first daemon owns the store

	start := time.Now()
	err := Run(dir, Options{IdleTimeout: 30 * time.Second, Version: "loser"})
	require.NoError(t, err, "second Run should be nil (ErrLocked loser)")
	assert.LessOrEqual(t, time.Since(start), time.Second, "second Run took too long, want fast")

	// The first daemon must still be serving (the loser did not disturb it).
	c := dialRetry(t, dir, time.Second)
	defer c.Close()
	assert.NoError(t, c.Ping(), "first daemon not serving after loser")
}

func TestShutdownOp(t *testing.T) {
	dir := t.TempDir()
	c, h := startDaemon(t, dir, 30*time.Second)

	require.NoError(t, c.Shutdown(), "Shutdown")
	select {
	case <-h.done:
		require.NoError(t, h.err, "Run returned error")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Run did not return after OpShutdown")
	}
	_, err := os.Stat(config.SocketPath(dir))
	assert.Truef(t, os.IsNotExist(err), "socket still present after shutdown: %v", err)
}

func TestSyncNoOp(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)
	require.NoError(t, c.Sync(), "Sync")
}

// --- helpers ---

// startDaemonClient dials an already-running daemon for a second connection.
func startDaemonClient(t *testing.T, dir string) (*Client, error) {
	t.Helper()
	return Dial(dir)
}

// spoolAppendDirect mirrors what the CLI does before poking the daemon.
func spoolAppendDirect(dir string, r rec.Record) error {
	return spool.Append(config.SpoolDir(dir), r)
}

func ids(rows []rec.Record) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
