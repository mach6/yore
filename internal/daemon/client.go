package daemon

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"yore/internal/config"
	"yore/internal/proto"
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
}

// Dial connects to the daemon's socket under dir (100ms timeout).
func Dial(dir string) (*Client, error) {
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), dialTimeout)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
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
	return c.ok(proto.Request{Op: proto.OpPing}, opDeadline)
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
	return c.ok(proto.Request{Op: proto.OpDelete, DeleteID: id}, opDeadline)
}

// Devices lists enrolled devices (via the daemon's syncer).
func (c *Client) Devices() (proto.DevicesInfo, error) {
	resp, err := c.roundtrip(proto.Request{Op: proto.OpDevices}, syncDeadline)
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
	return c.ok(proto.Request{Op: proto.OpApprove, DeviceID: id}, syncDeadline)
}

// Revoke revokes a device and rotates keys.
func (c *Client) Revoke(id string) error {
	return c.ok(proto.Request{Op: proto.OpRevoke, DeviceID: id}, syncDeadline)
}

// Sync forces a synchronous push/pull cycle (no-op if sync isn't configured).
func (c *Client) Sync() error {
	return c.ok(proto.Request{Op: proto.OpSync}, syncDeadline)
}

// Shutdown asks the daemon to exit gracefully.
func (c *Client) Shutdown() error {
	return c.ok(proto.Request{Op: proto.OpShutdown}, opDeadline)
}

// Close closes the connection (it does not stop the daemon).
func (c *Client) Close() error { return c.conn.Close() }

// ok performs a roundtrip and checks Response.OK.
func (c *Client) ok(req proto.Request, deadline time.Duration) error {
	resp, err := c.roundtrip(req, deadline)
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
func (c *Client) roundtrip(req proto.Request, deadline time.Duration) (proto.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func respErr(resp proto.Response) error {
	if resp.Err != "" {
		return errors.New("daemon: " + resp.Err)
	}
	return errors.New("daemon: request failed")
}

// spawnDaemon starts `yore daemon` fully detached (mirrors cli/record.go). The
// daemon handles the already-running race itself.
func spawnDaemon() {
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
