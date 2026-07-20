package daemon

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			t.Error("daemon did not shut down in cleanup")
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
	t.Fatalf("could not dial daemon in %s", within)
	return nil
}

// rawRequest sends one request on a fresh connection and returns the response.
// Used to exercise ops the Client type does not expose (e.g. OpRecord).
func rawRequest(t *testing.T, dir string, req proto.Request) proto.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.WriteMsg(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp proto.Response
	if err := proto.ReadMsg(bufio.NewReader(conn), &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp
}

// queryUntil polls Query(q) on c until it returns >= want rows or timeout.
func queryUntil(t *testing.T, c *Client, q proto.QueryReq, want int, within time.Duration) proto.QueryResp {
	t.Helper()
	deadline := time.Now().Add(within)
	var last proto.QueryResp
	for time.Now().Before(deadline) {
		resp, err := c.Query(q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
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
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return m
}

// seed opens the store, appends rows, and closes it — so the daemon can then
// take ownership and load them into the corpus.
func seed(t *testing.T, dir string, rows []rec.Record) {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := s.AppendBatch(rows); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
}

func TestEnsureRunningDialSuccess(t *testing.T) {
	dir := t.TempDir()
	_, h := startDaemon(t, dir, 30*time.Second)

	// The daemon is already up, so EnsureRunning takes the dial-success path
	// and never spawns (which, under test, would re-exec the test binary).
	c, err := EnsureRunning(dir)
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	defer c.Close()
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PID <= 0 {
		t.Errorf("PID = %d, want > 0", st.PID)
	}
	if st.LocalRows != 2 {
		t.Errorf("LocalRows = %d, want 2", st.LocalRows)
	}
	if st.Version != "test-ver" {
		t.Errorf("Version = %q, want test-ver", st.Version)
	}
	if st.Remote.State != proto.RemoteOff {
		t.Errorf("Remote.State = %q, want %q", st.Remote.State, proto.RemoteOff)
	}
}

func TestRecordDurableAndIngested(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)

	resp := rawRequest(t, dir, proto.Request{
		Op:     proto.OpRecord,
		Record: &rec.Record{ID: "r1", Cmd: "echo durable", StartMs: 1000},
	})
	if !resp.OK {
		t.Fatalf("OpRecord resp not ok: %+v", resp)
	}

	got := queryUntil(t, c, proto.QueryReq{Q: "durable"}, 1, time.Second)
	if len(got.Rows) != 1 || got.Rows[0].Cmd != "echo durable" {
		t.Fatalf("record not queryable after debounce: %+v", got.Rows)
	}

	// After ingest the spool must be drained (records live durably in the store).
	if f := spoolFiles(t, dir); len(f) != 0 {
		t.Errorf("spool not drained: %v", f)
	}
}

func TestPokeIngest(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)

	// Simulate the CLI: fsync a record into the spool directly, then poke.
	if err := spoolAppendDirect(dir, rec.Record{ID: "p1", Cmd: "poked command", StartMs: 5}); err != nil {
		t.Fatalf("spool append: %v", err)
	}
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	got := queryUntil(t, c, proto.QueryReq{Q: "poked"}, 1, time.Second)
	if len(got.Rows) != 1 || got.Rows[0].Cmd != "poked command" {
		t.Fatalf("poked record not queryable: %+v", got.Rows)
	}
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

	// Match + newest-first ordering (local scope).
	resp, err := c.Query(proto.QueryReq{Q: "git"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 {
		t.Errorf("Total = %d, want 4", resp.Total)
	}
	if resp.Scope != proto.ScopeLocal {
		t.Errorf("Scope = %q, want local", resp.Scope)
	}
	gotIDs := ids(resp.Rows)
	if want := []string{"r5", "r3", "r2", "r1"}; !eq(gotIDs, want) {
		t.Errorf("order = %v, want %v", gotIDs, want)
	}

	// Dedupe collapses "git status" (r1/r3) to the newest (r3).
	resp, _ = c.Query(proto.QueryReq{Q: "git", Dedupe: true})
	if resp.Total != 3 {
		t.Errorf("dedupe Total = %d, want 3", resp.Total)
	}
	if want := []string{"r5", "r3", "r2"}; !eq(ids(resp.Rows), want) {
		t.Errorf("dedupe order = %v, want %v", ids(resp.Rows), want)
	}

	// Session scope keeps only s1.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeSession, Session: "s1"})
	if want := []string{"r5", "r2", "r1"}; !eq(ids(resp.Rows), want) {
		t.Errorf("session scope = %v, want %v", ids(resp.Rows), want)
	}

	// Cwd scope keeps only /a.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeCwd, Cwd: "/a"})
	if want := []string{"r5", "r3", "r1"}; !eq(ids(resp.Rows), want) {
		t.Errorf("cwd scope = %v, want %v", ids(resp.Rows), want)
	}

	// Offset/limit window applies after dedupe; Total is unwindowed.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Limit: 2, Offset: 1})
	if resp.Total != 4 {
		t.Errorf("windowed Total = %d, want 4", resp.Total)
	}
	if want := []string{"r3", "r2"}; !eq(ids(resp.Rows), want) {
		t.Errorf("window = %v, want %v", ids(resp.Rows), want)
	}

	// "all"/"host" behave like local but report Remote off.
	resp, _ = c.Query(proto.QueryReq{Q: "git", Scope: proto.ScopeAll})
	if resp.Total != 4 || resp.Remote.State != proto.RemoteOff {
		t.Errorf("scope all: Total=%d Remote=%q, want 4/off", resp.Total, resp.Remote.State)
	}
	if resp.Scope != proto.ScopeAll {
		t.Errorf("scope all: effective scope = %q, want all", resp.Scope)
	}
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
		if _, err := c.Query(proto.QueryReq{Q: q}); err != nil {
			t.Fatalf("incremental query %q: %v", q, err)
		}
	}
	inc, _ := c.Query(proto.QueryReq{Q: "git"})

	// Compare against a fresh connection's full-scan result.
	fresh, _ := startDaemonClient(t, dir)
	full, _ := fresh.Query(proto.QueryReq{Q: "git"})
	fresh.Close()

	if !eq(ids(inc.Rows), ids(full.Rows)) {
		t.Errorf("incremental %v != full %v", ids(inc.Rows), ids(full.Rows))
	}
	if want := []string{"r4", "r2", "r1"}; !eq(ids(inc.Rows), want) {
		t.Errorf("git rows = %v, want %v", ids(inc.Rows), want)
	}
}

func TestIdleExit(t *testing.T) {
	dir := t.TempDir()
	c, h := startDaemon(t, dir, 300*time.Millisecond)

	if err := c.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	start := time.Now()

	select {
	case <-h.done:
		if h.err != nil {
			t.Fatalf("Run returned error: %v", h.err)
		}
		if d := time.Since(start); d > 1500*time.Millisecond {
			t.Errorf("idle exit took %s, want < 1.5s", d)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Run did not return after idle timeout")
	}

	if _, err := os.Stat(config.SocketPath(dir)); !os.IsNotExist(err) {
		t.Errorf("socket still present after idle exit: %v", err)
	}
}

func TestSecondRunLockedLosesQuietly(t *testing.T) {
	dir := t.TempDir()
	_, _ = startDaemon(t, dir, 30*time.Second) // first daemon owns the store

	start := time.Now()
	err := Run(dir, Options{IdleTimeout: 30 * time.Second, Version: "loser"})
	if err != nil {
		t.Fatalf("second Run = %v, want nil (ErrLocked loser)", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("second Run took %s, want fast", d)
	}

	// The first daemon must still be serving (the loser did not disturb it).
	c := dialRetry(t, dir, time.Second)
	defer c.Close()
	if err := c.Ping(); err != nil {
		t.Errorf("first daemon not serving after loser: %v", err)
	}
}

func TestShutdownOp(t *testing.T) {
	dir := t.TempDir()
	c, h := startDaemon(t, dir, 30*time.Second)

	if err := c.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-h.done:
		if h.err != nil {
			t.Fatalf("Run returned error: %v", h.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after OpShutdown")
	}
	if _, err := os.Stat(config.SocketPath(dir)); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

func TestSyncNoOp(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
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

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
