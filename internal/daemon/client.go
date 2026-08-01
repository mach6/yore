package daemon

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"yore/internal/config"
	"yore/internal/proto"
	"yore/internal/rec"
)

const (
	dialTimeout   = 100 * time.Millisecond
	opDeadline    = 2 * time.Second // ping/status/shutdown
	queryDeadline = 2 * time.Second
	syncDeadline  = 35 * time.Second // explicit sync runs a full push/pull cycle

	spawnBudget = 2500 * time.Millisecond // total wait for a spawned daemon
)

// Client is a connection to a running daemon. Each call is one request/response
// over a single persistent connection. Calls are serialized by an internal
// mutex, so it is safe to share one Client across goroutines — notably the
// Bubble Tea TUIs, which fan out Query/Hosts/stats commands concurrently.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	dir  string // state dir, so slow ops can open their own connection
}

// Dial connects to the daemon's socket under dir (100ms timeout).
func Dial(dir string) (*Client, error) {
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), dialTimeout)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, r: bufio.NewReader(conn), dir: dir}, nil
}

// solo runs one request on its OWN connection instead of the shared one. The
// network-backed ops (sync, device management) can take tens of seconds against
// an unreachable server, and roundtrip holds the client mutex for the whole
// exchange — so on the shared connection they would stall every query queued
// behind them and freeze the TUI. The daemon serves each connection on its own
// goroutine, so a second connection runs genuinely in parallel.
//
// A client with no dir (constructed directly in tests) falls back to the shared
// connection rather than failing. Every op routed here reaches the sync server,
// so they all wait the same syncDeadline.
func (c *Client) solo(req proto.Request) (proto.Response, error) {
	if c.dir == "" {
		return c.roundtrip(req, syncDeadline)
	}
	// EnsureRunning rather than Dial: these ops are also what a TUI reaches for
	// after sitting open long enough for the daemon to idle out, and a screen
	// that cannot sync until you quit and come back is a screen that is broken.
	sc, err := EnsureRunning(c.dir)
	if err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = sc.Close() }()
	return sc.roundtrip(req, syncDeadline)
}

// soloOK is solo plus the Response.OK check.
func (c *Client) soloOK(req proto.Request) error {
	resp, err := c.solo(req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	return nil
}

// EnsureRunning returns a client to a running daemon, spawning one if needed.
// It mirrors cli/record.go spawnDaemon: a fully detached `yore daemon` whose
// store-lock loser (if we raced another spawner) exits silently.
func EnsureRunning(dir string) (*Client, error) {
	if c, err := Dial(dir); err == nil {
		return c, nil
	}
	spawnDaemon()

	deadline := time.Now().Add(spawnBudget)
	backoff := 25 * time.Millisecond
	for {
		time.Sleep(backoff)
		if c, err := Dial(dir); err == nil {
			return c, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("daemon: not reachable after spawn")
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// Ping resets the daemon's idle timer and nudges spool ingest.
func (c *Client) Ping() error {
	return c.ok(proto.Request{Op: proto.OpPing})
}

// Query runs a search.
func (c *Client) Query(q proto.QueryReq) (proto.QueryResp, error) {
	resp, err := c.roundtrip(proto.Request{Op: proto.OpQuery, Query: &q}, queryDeadline)
	if err != nil {
		return proto.QueryResp{}, err
	}
	if !resp.OK {
		return proto.QueryResp{}, respErr(resp)
	}
	if resp.Query == nil {
		return proto.QueryResp{}, errors.New("daemon: query response missing body")
	}
	return *resp.Query, nil
}

// SubmitRecord delivers one record to the daemon for immediate ingest. Used for
// non-command records — user-tag ops — that the shell fast path never produces.
func (c *Client) SubmitRecord(r rec.Record) error {
	return c.ok(proto.Request{Op: proto.OpRecord, Record: &r})
}

// Tags lists the known user tags with how many commands carry each. scope is a
// proto.Scope* value bounding what is counted; "" counts this host.
func (c *Client) Tags(scope string) (proto.TagsInfo, error) {
	req := proto.Request{Op: proto.OpTags, Tags: &proto.TagsReq{Scope: scope}}
	resp, err := c.roundtrip(req, opDeadline)
	if err != nil {
		return proto.TagsInfo{}, err
	}
	if !resp.OK {
		return proto.TagsInfo{}, respErr(resp)
	}
	if resp.Tags == nil {
		return proto.TagsInfo{}, errors.New("daemon: tags response missing body")
	}
	return *resp.Tags, nil
}

// Status reports daemon status.
func (c *Client) Status() (proto.StatusResp, error) {
	resp, err := c.roundtrip(proto.Request{Op: proto.OpStatus}, opDeadline)
	if err != nil {
		return proto.StatusResp{}, err
	}
	if !resp.OK {
		return proto.StatusResp{}, respErr(resp)
	}
	if resp.Status == nil {
		return proto.StatusResp{}, errors.New("daemon: status response missing body")
	}
	return *resp.Status, nil
}

// Hosts returns per-host record counts for the browse sidebar.
func (c *Client) Hosts() (proto.HostsInfo, error) {
	resp, err := c.roundtrip(proto.Request{Op: proto.OpHosts}, opDeadline)
	if err != nil {
		return proto.HostsInfo{}, err
	}
	if !resp.OK {
		return proto.HostsInfo{}, respErr(resp)
	}
	if resp.Hosts == nil {
		return proto.HostsInfo{}, errors.New("daemon: hosts response missing body")
	}
	return *resp.Hosts, nil
}

// Delete tombstones one record by id.
func (c *Client) Delete(id string) error {
	return c.ok(proto.Request{Op: proto.OpDelete, DeleteID: id})
}

// Devices lists enrolled devices (via the daemon's syncer).
func (c *Client) Devices() (proto.DevicesInfo, error) {
	resp, err := c.solo(proto.Request{Op: proto.OpDevices})
	if err != nil {
		return proto.DevicesInfo{}, err
	}
	if !resp.OK {
		return proto.DevicesInfo{}, respErr(resp)
	}
	if resp.Devices == nil {
		return proto.DevicesInfo{}, errors.New("daemon: devices response missing body")
	}
	return *resp.Devices, nil
}

// Approve admits a pending device (wraps the History Key for it).
func (c *Client) Approve(id string) error {
	return c.soloOK(proto.Request{Op: proto.OpApprove, DeviceID: id})
}

// Revoke revokes a device and rotates keys.
func (c *Client) Revoke(id string) error {
	return c.soloOK(proto.Request{Op: proto.OpRevoke, DeviceID: id})
}

// Token mints a single-use enrollment token for adding another machine.
func (c *Client) Token() (proto.TokenInfo, error) {
	resp, err := c.solo(proto.Request{Op: proto.OpToken})
	if err != nil {
		return proto.TokenInfo{}, err
	}
	if !resp.OK {
		return proto.TokenInfo{}, respErr(resp)
	}
	if resp.Token == nil {
		return proto.TokenInfo{}, errors.New("daemon: token response missing body")
	}
	return *resp.Token, nil
}

// Tokens lists the enrollment tokens the server records, and what became of
// each. It never carries a token's plaintext — that existed only at mint time.
func (c *Client) Tokens() (proto.TokensInfo, error) {
	resp, err := c.solo(proto.Request{Op: proto.OpTokens})
	if err != nil {
		return proto.TokensInfo{}, err
	}
	if !resp.OK {
		return proto.TokensInfo{}, respErr(resp)
	}
	if resp.Tokens == nil {
		return proto.TokensInfo{}, errors.New("daemon: tokens response missing body")
	}
	return *resp.Tokens, nil
}

// RevokeToken cancels an unclaimed enrollment token so it can admit no one.
func (c *Client) RevokeToken(id string) error {
	return c.soloOK(proto.Request{Op: proto.OpRevokeTk, TokenID: id})
}

// Sync forces a synchronous push/pull cycle (no-op if sync isn't configured).
func (c *Client) Sync() error {
	return c.soloOK(proto.Request{Op: proto.OpSync})
}

// Shutdown asks the daemon to exit gracefully.
func (c *Client) Shutdown() error {
	return c.ok(proto.Request{Op: proto.OpShutdown})
}

// Close closes the connection (it does not stop the daemon).
func (c *Client) Close() error { return c.conn.Close() }

// ok performs a roundtrip (under the short op deadline) and checks Response.OK.
func (c *Client) ok(req proto.Request) error {
	resp, err := c.roundtrip(req, opDeadline)
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	return nil
}

// roundtrip writes one request and reads one response under a deadline. It
// holds the client mutex for the whole exchange so concurrent callers queue
// rather than interleaving on the single connection.
//
// If the connection has gone — the usual reason being that the daemon idled out
// under a TUI that was open but quiet, taking its connections with it — the
// request is retried once on a fresh connection, spawning a daemon if none is
// listening. A long-lived screen recovers by itself instead of turning into an
// error message per keystroke.
func (c *Client) roundtrip(req proto.Request, deadline time.Duration) (proto.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.exchange(req, deadline)
	if err == nil || !lostConn(err) || !resumable(req.Op) {
		return resp, err
	}
	if rerr := c.reconnect(); rerr != nil {
		return proto.Response{}, err // report the original failure, not the redial
	}
	return c.exchange(req, deadline)
}

// exchange is one write/read on the current connection.
func (c *Client) exchange(req proto.Request, deadline time.Duration) (proto.Response, error) {
	if err := c.conn.SetDeadline(time.Now().Add(deadline)); err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	if err := proto.WriteMsg(c.conn, req); err != nil {
		return proto.Response{}, err
	}
	var resp proto.Response
	if err := proto.ReadMsg(c.r, &resp); err != nil {
		return proto.Response{}, err
	}
	return resp, nil
}

// reconnect replaces the client's connection with a live one, spawning a daemon
// if the socket is dead. The caller holds the mutex.
func (c *Client) reconnect() error {
	if c.dir == "" {
		return errors.New("daemon: no state dir to reconnect with")
	}
	_ = c.conn.Close()
	fresh, err := EnsureRunning(c.dir)
	if err != nil {
		return err
	}
	c.conn, c.r = fresh.conn, fresh.r
	return nil
}

// lostConn reports whether err means the connection went away rather than the
// daemon answering slowly. A deadline is not a lost connection: the daemon may
// be mid-query, and re-asking would only pile on.
func lostConn(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// resumable reports whether re-sending an op after a lost connection is safe.
// Reads are, and so are the mutations that converge (a record dedupes by id, a
// tombstone and an approval are the same applied twice). Minting an enrollment
// token is NOT: a second attempt would leave a second live invitation standing
// on the server, which is precisely what nobody can see to revoke. Shutdown is
// excluded because retrying it would spawn a daemon in order to stop it.
func resumable(op string) bool {
	switch op {
	case proto.OpToken, proto.OpShutdown:
		return false
	}
	return true
}

func respErr(resp proto.Response) error {
	if resp.Err != "" {
		return errors.New("daemon: " + resp.Err)
	}
	return errors.New("daemon: request failed")
}

// spawnDaemon starts `yore daemon` fully detached (mirrors cli/record.go). The
// daemon handles the already-running race itself.
func spawnDaemon() {
	// Never re-exec under `go test`: os.Executable() is the test binary, so
	// `<testbin> daemon` re-runs the whole suite and re-spawns recursively — a
	// detached fork bomb. Production binaries return false here.
	if testing.Testing() {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
