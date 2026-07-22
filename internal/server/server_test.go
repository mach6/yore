package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/reqsign"
	"yore/internal/wire"
)

// ---- harness ----

// testClient talks to the server the way the real client (internal/syncer)
// must: every authenticated request is signed with reqsign over the request's
// RequestURI and the exact body bytes. There is no bearer token — a client
// without a signing identity can reach only the open endpoints and enrollment,
// the latter authorized by ticket.
type testClient struct {
	t      *testing.T
	base   string
	ticket string // "" => send no X-Yore-Ticket header
	devID  string
	priv   ed25519.PrivateKey // nil => send no signature headers
}

// anon returns a copy with no signing identity: an unenrolled caller.
func (c *testClient) anon() *testClient {
	cp := *c
	cp.devID, cp.priv = "", nil
	return &cp
}

// withTicket returns a copy presenting a specific enrollment ticket.
func (c *testClient) withTicket(ticket string) *testClient {
	cp := *c
	cp.ticket = ticket
	return &cp
}

// withKey returns a copy of the client that signs as (devID, priv).
func (c *testClient) withKey(devID string, priv ed25519.PrivateKey) *testClient {
	cp := *c
	cp.devID = devID
	cp.priv = priv
	return &cp
}

func (c *testClient) do(method, path string, body any) (status int, resp []byte) {
	c.t.Helper()
	return c.send(method, path, marshal(c.t, body), true)
}

// doRaw sends raw bytes as the body (for malformed-JSON cases).
func (c *testClient) doRaw(method, path string, raw []byte) (status int, resp []byte) {
	c.t.Helper()
	return c.send(method, path, raw, true)
}

// send issues the request, attaching the bearer token and — when sign is true
// and the client has a signing identity — the reqsign headers over the exact
// body bytes and the request's RequestURI.
func (c *testClient) send(method, path string, body []byte, sign bool) (status int, resp []byte) {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	if sign && c.priv != nil {
		for k, v := range c.signHeaders(req, method, body, time.Now()) {
			req.Header.Set(k, v)
		}
	}
	return c.roundtrip(req)
}

// sendConcurrent performs a signed request and returns the outcome purely as
// values (it never touches c.t), so it is safe to call from a spawned
// goroutine: testify's FailNow is illegal off the main test goroutine, so the
// caller ships the result back and asserts on the main goroutine. It mirrors
// do -> send(sign=true).
func (c *testClient) sendConcurrent(method, path string, body any) (status int, respBody []byte, err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal: %w", err)
	}
	var r io.Reader
	if raw != nil {
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return 0, nil, fmt.Errorf("new request: %w", err)
	}
	if c.ticket != "" {
		req.Header.Set(hdrTicket, c.ticket)
	}
	if c.priv != nil {
		hdrs, err := reqsign.Sign(c.devID, func(b []byte) []byte { return ed25519.Sign(c.priv, b) },
			method, req.URL.RequestURI(), raw, time.Now())
		if err != nil {
			return 0, nil, fmt.Errorf("sign: %w", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// sendSignedAt signs the request at a specific clock time (used to forge a
// stale-timestamp request).
func (c *testClient) sendSignedAt(method, path string, body []byte, now time.Time) (status int, resp []byte) {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	for k, v := range c.signHeaders(req, method, body, now) {
		req.Header.Set(k, v)
	}
	return c.roundtrip(req)
}

// sendWithHeaders replays a captured signature verbatim (same nonce).
func (c *testClient) sendWithHeaders(method, path string, body []byte, headers map[string]string) (status int, resp []byte) {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.roundtrip(req)
}

// signOnly returns the signature headers for (method, path, body) without
// sending, so a caller can replay the exact same signed request.
func (c *testClient) signOnly(method, path string, body []byte) map[string]string {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	return c.signHeaders(req, method, body, time.Now())
}

func (c *testClient) newRequest(method, path string, body []byte) *http.Request {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	require.NoError(c.t, err, "new request")
	if c.ticket != "" {
		req.Header.Set(hdrTicket, c.ticket)
	}
	return req
}

func (c *testClient) signHeaders(req *http.Request, method string, body []byte, now time.Time) map[string]string {
	c.t.Helper()
	hdrs, err := reqsign.Sign(c.devID, func(b []byte) []byte { return ed25519.Sign(c.priv, b) },
		method, req.URL.RequestURI(), body, now)
	require.NoError(c.t, err, "sign")
	return hdrs
}

func (c *testClient) roundtrip(req *http.Request) (status int, body []byte) {
	c.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err, "do request")
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func marshal(t *testing.T, body any) []byte {
	t.Helper()
	if body == nil {
		return nil
	}
	b, err := json.Marshal(body)
	require.NoError(t, err, "marshal body")
	return b
}

func setup(t *testing.T) *testClient {
	t.Helper()
	s, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: "tok"})
	require.NoError(t, err, "New")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	// The base client holds only the server's bootstrap token: it can form a
	// group, but it is not a device and so cannot read anything. Tests obtain a
	// usable identity with bootstrapActive / registerDevice.
	return &testClient{t: t, base: srv.URL, ticket: "tok"}
}

// mintTicket asks the server for a single-use enrollment ticket, signed by c
// (which must be an active device).
func mintTicket(t *testing.T, c *testClient) string {
	t.Helper()
	status, body := c.do("POST", "/v1/tickets", struct{}{})
	require.Equalf(t, http.StatusOK, status, "mint ticket: body %s", body)
	return mustJSON[wire.TicketResp](t, body).Ticket
}

func mustJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	require.NoErrorf(t, json.Unmarshal(data, &v), "unmarshal %T from %s", v, data)
	return v
}

func mkRecords(start, n uint64) []wire.PushRecord {
	rs := make([]wire.PushRecord, 0, n)
	for i := uint64(0); i < n; i++ {
		seq := start + i
		rs = append(rs, wire.PushRecord{
			Seq:   seq,
			ID:    fmt.Sprintf("id-%d", seq),
			KeyID: "k1",
			Blob:  []byte(fmt.Sprintf("blob-%d", seq)),
		})
	}
	return rs
}

func pubKey() []byte { return make([]byte, 32) }

// registerDevice enrolls a new device into an existing group: it mints a ticket
// with base (an active device) and registers id against it.
func registerDevice(t *testing.T, base *testClient, id string) *testClient {
	t.Helper()
	return registerWithTicket(t, base, id, mintTicket(t, base))
}

// registerWithTicket generates a fresh Ed25519 keypair, self-signs a
// registration for id authorized by ticket, and returns a client that signs
// later requests as that device.
func registerWithTicket(t *testing.T, base *testClient, id, ticket string) *testClient {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "genkey")
	dc := base.withKey(id, priv).withTicket(ticket)
	status, body := dc.do("POST", "/v1/devices", wire.RegisterReq{ID: id, Name: id, PubKey: pubKey(), SignKey: pub})
	require.Equalf(t, http.StatusOK, status, "register %s: body %s", id, body)
	return dc
}

// activateDevice activates id, signed by `signer` — an already-active device,
// or (for the very first device) the pending device itself at bootstrap.
func activateDevice(t *testing.T, signer *testClient, id string) {
	t.Helper()
	req := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: id, HKVersion: 1, Blob: []byte("hk-" + id)}}
	status, body := signer.do("POST", "/v1/devices/"+id+"/activate", req)
	require.Equalf(t, http.StatusOK, status, "activate %s: body %s", id, body)
}

// bootstrapActive registers id and activates it as the first (bootstrap) device,
// returning its signing client. Use once per server.
func bootstrapActive(t *testing.T, base *testClient, id string) *testClient {
	t.Helper()
	// The very first device enrolls on the server's own token, which is accepted
	// only while no device is active.
	dc := registerWithTicket(t, base, id, base.ticket)
	activateDevice(t, dc, id) // bootstrap: the pending device self-activates
	return dc
}

// ---- New ----

func TestNewRefusesEmptyToken(t *testing.T) {
	_, err := New(Options{DBPath: filepath.Join(t.TempDir(), "x.db"), Token: ""})
	require.Error(t, err, "expected error for empty token")
}

// ---- auth (bearer token; runs before signatures) ----

func TestAuth(t *testing.T) {
	c := setup(t)

	// Health is open.
	noAuth := &testClient{t: t, base: c.base}
	status, _ := noAuth.do("GET", "/v1/health", nil)
	require.Equal(t, http.StatusOK, status, "health no-auth")

	// An unenrolled caller: no signing identity, and a ticket the server never
	// minted. Nothing beyond /v1/health is reachable.
	wrong := &testClient{t: t, base: c.base, ticket: "nope"}
	routes := []struct{ method, path string }{
		{"GET", "/v1/hosts"},
		{"POST", "/v1/records"},
		{"GET", "/v1/records?host_id=h"},
		{"POST", "/v1/devices"},
		{"GET", "/v1/devices"},
		{"POST", "/v1/devices/x/activate"},
		{"POST", "/v1/devices/x/revoke"},
		{"GET", "/v1/keys/hk?device_id=x"},
		{"GET", "/v1/keys/dek"},
		{"POST", "/v1/keys/dek"},
		{"POST", "/v1/keys/rotate"},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			// Both the no-header and wrong-token cases are independent checks, so
			// accumulate them with assert rather than fail-fast.
			status, _ := noAuth.do(r.method, r.path, nil)
			assert.Equal(t, http.StatusUnauthorized, status, "no header")
			status, _ = wrong.do(r.method, r.path, nil)
			assert.Equal(t, http.StatusUnauthorized, status, "wrong token")
		})
	}
}

// ---- register signature ----

func TestRegisterSignature(t *testing.T) {
	c := setup(t)

	// Happy path: self-signed register stores the sign key and is pending.
	pub, priv, _ := ed25519.GenerateKey(nil)
	dc := c.withKey("A", priv)
	status, body := dc.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pub})
	require.Equalf(t, http.StatusOK, status, "register: body %s", body)
	d := mustJSON[wire.RegisterResp](t, body).Device
	require.Equal(t, wire.DevicePending, d.Status, "register status")
	require.True(t, bytes.Equal(d.SignKey, pub), "register: sign key stored")

	// No signature (valid token) => 401.
	status, _ = c.do("POST", "/v1/devices", wire.RegisterReq{ID: "B", Name: "B", PubKey: pubKey(), SignKey: pub})
	require.Equal(t, http.StatusUnauthorized, status, "unsigned register")

	// sign_key not 32 bytes => 400.
	status, _ = c.do("POST", "/v1/devices", wire.RegisterReq{ID: "C", PubKey: pubKey(), SignKey: []byte("short")})
	require.Equal(t, http.StatusBadRequest, status, "short sign_key")

	// Header device id != body id => rejected. dc2 signs as "A" but claims id "D".
	dc2 := c.withKey("A", priv)
	status, _ = dc2.do("POST", "/v1/devices", wire.RegisterReq{ID: "D", PubKey: pubKey(), SignKey: pub})
	require.Equal(t, http.StatusUnauthorized, status, "device-id mismatch register")
}

// ---- push signature ----

func TestPushSignature(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")
	pushBody := marshal(t, wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})

	// Signed push from an active device succeeds.
	hdrs := dev.signOnly("POST", "/v1/records", pushBody)
	status, body := dev.sendWithHeaders("POST", "/v1/records", pushBody, hdrs)
	require.Equalf(t, http.StatusOK, status, "signed push: body %s", body)
	// Same request replayed (same nonce) => 401.
	status, _ = dev.sendWithHeaders("POST", "/v1/records", pushBody, hdrs)
	require.Equal(t, http.StatusUnauthorized, status, "replayed push")

	// Unsigned push (valid token, no signature) => 401.
	status, _ = c.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(10, 1)})
	require.Equal(t, http.StatusUnauthorized, status, "unsigned push")

	// Push signed by a different key than registered => 401.
	_, wrongPriv, _ := ed25519.GenerateKey(nil)
	bad := c.withKey("pusher", wrongPriv)
	status, _ = bad.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(20, 1)})
	require.Equal(t, http.StatusUnauthorized, status, "wrong-key push")

	// Push with a stale timestamp => 401.
	stale := marshal(t, wire.PushReq{HostID: "hostA", Records: mkRecords(30, 1)})
	status, _ = dev.sendSignedAt("POST", "/v1/records", stale, time.Now().Add(-2*reqsign.Skew))
	require.Equal(t, http.StatusUnauthorized, status, "stale-timestamp push")
}

func TestPushHappyAndIdempotent(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	status, body := dev.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	require.Equalf(t, http.StatusOK, status, "push: body %s", body)
	resp := mustJSON[wire.PushResp](t, body)
	require.Equal(t, 3, resp.Stored, "push stored")
	require.Equal(t, uint64(3), resp.MaxSeq, "push maxSeq")

	// Re-push the same batch (fresh signature/nonce): nothing stored, MaxSeq stable.
	status, body = dev.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	require.Equalf(t, http.StatusOK, status, "re-push: body %s", body)
	resp = mustJSON[wire.PushResp](t, body)
	require.Equal(t, 0, resp.Stored, "re-push stored")
	require.Equal(t, uint64(3), resp.MaxSeq, "re-push maxSeq")
}

func TestPushValidation(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	tests := []struct {
		name string
		send func() int // returns the response status for a 400-expected request
	}{
		{"empty host_id", func() int {
			status, _ := dev.do("POST", "/v1/records", wire.PushReq{Records: mkRecords(1, 1)})
			return status
		}},
		{"descending seqs", func() int {
			desc := []wire.PushRecord{{Seq: 3}, {Seq: 2}, {Seq: 1}}
			status, _ := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: desc})
			return status
		}},
		{">1000 records", func() int {
			status, _ := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 1001)})
			return status
		}},
		{"malformed JSON", func() int {
			// Validly signed over the raw bytes, but not valid JSON.
			status, _ := dev.doRaw("POST", "/v1/records", []byte("{not json"))
			return status
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, tc.send())
		})
	}
}

// ---- pull ----

func TestPullPaging(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	// Load 2500 records over 3 pushes (push caps at 1000).
	for _, batch := range [][2]uint64{{1, 1000}, {1001, 1000}, {2001, 500}} {
		status, body := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(batch[0], batch[1])})
		require.Equalf(t, http.StatusOK, status, "load push: body %s", body)
	}

	after := uint64(0)
	total := 0
	pages := 0
	for {
		status, body := dev.do("GET", fmt.Sprintf("/v1/records?host_id=h&after=%d&limit=1000", after), nil)
		require.Equalf(t, http.StatusOK, status, "pull: body %s", body)
		resp := mustJSON[wire.PullResp](t, body)
		total += len(resp.Records)
		pages++
		// Verify ascending & contiguous.
		for i, r := range resp.Records {
			want := after + uint64(i) + 1
			require.Equalf(t, want, r.Seq, "page %d record %d seq", pages, i)
		}
		if resp.NextAfter == nil {
			break
		}
		after = *resp.NextAfter
		require.LessOrEqual(t, pages, 10, "too many pages")
	}
	require.Equal(t, 2500, total, "pulled records")
	require.Equal(t, 3, pages, "pages")
}

func TestPullUnknownAndBeyond(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	// Unknown host => empty, no NextAfter, not 404.
	status, body := dev.do("GET", "/v1/records?host_id=nope", nil)
	require.Equalf(t, http.StatusOK, status, "unknown host: body %s", body)
	resp := mustJSON[wire.PullResp](t, body)
	require.Empty(t, resp.Records, "unknown host records")
	require.Nil(t, resp.NextAfter, "unknown host nextAfter")

	// after beyond end => empty, no NextAfter.
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 3)})
	status, body = dev.do("GET", "/v1/records?host_id=h&after=100", nil)
	require.Equalf(t, http.StatusOK, status, "beyond end: body %s", body)
	resp = mustJSON[wire.PullResp](t, body)
	require.Empty(t, resp.Records, "beyond end records")
	require.Nil(t, resp.NextAfter, "beyond end nextAfter")
}

// ---- reads require a device signature ----

// TestReadsRequireSignature pins the auth model: with no bearer token, a read is
// exactly as privileged as a write. An active device may read; an unenrolled
// caller may not read anything but /v1/health.
func TestReadsRequireSignature(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "reader")
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 3)})

	reads := []string{"/v1/hosts", "/v1/records?host_id=h", "/v1/devices", "/v1/keys/dek"}
	for _, path := range reads {
		t.Run(path, func(t *testing.T) {
			status, body := dev.do("GET", path, nil)
			require.Equalf(t, http.StatusOK, status, "signed read %s: body %s", path, body)

			status, _ = dev.anon().do("GET", path, nil)
			require.Equalf(t, http.StatusUnauthorized, status, "unsigned read %s must be refused", path)
		})
	}
	// Health is open to an unauthenticated caller.
	noAuth := &testClient{t: t, base: c.base}
	status, _ := noAuth.do("GET", "/v1/health", nil)
	require.Equal(t, http.StatusOK, status, "health no-auth")
}

// ---- hosts ----

func TestHosts(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "alpha", Records: mkRecords(1, 5)})
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "beta", Records: mkRecords(1, 9)})

	status, body := dev.do("GET", "/v1/hosts", nil)
	require.Equalf(t, http.StatusOK, status, "hosts: body %s", body)
	resp := mustJSON[wire.HostsResp](t, body)
	got := map[string]uint64{}
	for _, h := range resp.Hosts {
		got[h.HostID] = h.MaxSeq
	}
	require.Equal(t, uint64(5), got["alpha"], "alpha max seq")
	require.Equal(t, uint64(9), got["beta"], "beta max seq")
}

// ---- device lifecycle ----

func TestDeviceLifecycle(t *testing.T) {
	c := setup(t)

	// Register A (self-signed) => pending.
	pubA, privA, _ := ed25519.GenerateKey(nil)
	dcA := c.withKey("A", privA)
	status, body := dcA.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pubA})
	require.Equalf(t, http.StatusOK, status, "register A: body %s", body)
	require.Equal(t, wire.DevicePending, mustJSON[wire.RegisterResp](t, body).Device.Status, "register A status")

	// Duplicate (still self-signed, fresh nonce) => 409.
	status, _ = dcA.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pubA})
	require.Equal(t, http.StatusConflict, status, "duplicate register")

	// Bad pubkey => 400 (shape check precedes signature).
	status, _ = c.do("POST", "/v1/devices", wire.RegisterReq{ID: "Z", PubKey: []byte("short"), SignKey: pubA})
	require.Equal(t, http.StatusBadRequest, status, "short pubkey")

	// Activate with wrong wrap.DeviceID => 400 (A self-signs at bootstrap).
	badWrap := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "other", HKVersion: 1}}
	status, _ = dcA.do("POST", "/v1/devices/A/activate", badWrap)
	require.Equal(t, http.StatusBadRequest, status, "wrong wrap.device_id")

	// Bootstrap first activate sets version=1.
	activateDevice(t, dcA, "A")

	// hk_version is now 1: activating B at wrong version fails, at 1 succeeds.
	// B is approved by the active device A (signer need not be the resource).
	dcB := registerDevice(t, dcA, "B")
	_ = dcB
	wrongVer := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "B", HKVersion: 2}}
	status, _ = dcA.do("POST", "/v1/devices/B/activate", wrongVer)
	require.Equal(t, http.StatusBadRequest, status, "activate B wrong version")
	activateDevice(t, dcA, "B")

	// A's HK wrap is retrievable by an active device.
	status, _ = dcA.do("GET", "/v1/keys/hk?device_id=A", nil)
	require.Equal(t, http.StatusOK, status, "get hk A")

	// Revoke A, signed by active device B => deletes A's wrap.
	status, _ = dcB.do("POST", "/v1/devices/A/revoke", nil)
	require.Equal(t, http.StatusOK, status, "revoke A")
	status, _ = dcB.do("GET", "/v1/keys/hk?device_id=A", nil)
	require.Equal(t, http.StatusNotFound, status, "get hk A after revoke")

	// Activate revoked A (signed by active B) => 409.
	reactivate := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "A", HKVersion: 1}}
	status, _ = dcB.do("POST", "/v1/devices/A/activate", reactivate)
	require.Equal(t, http.StatusConflict, status, "activate revoked A")

	// Revoke unknown (signed by active B) => 404.
	status, _ = dcB.do("POST", "/v1/devices/ghost/revoke", nil)
	require.Equal(t, http.StatusNotFound, status, "revoke unknown")

	// List returns all devices regardless of status.
	status, body = dcB.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusOK, status, "list devices")
	require.Len(t, mustJSON[[]wire.Device](t, body), 2, "list devices count")

	// hk get for a device that never had a wrap => 404.
	status, _ = dcB.do("GET", "/v1/keys/hk?device_id=ghost", nil)
	require.Equal(t, http.StatusNotFound, status, "get hk ghost")
}

// ---- revoke signature ----

func TestRevokeSignature(t *testing.T) {
	c := setup(t)
	dcA := bootstrapActive(t, c, "A")
	dcB := registerDevice(t, dcA, "B")
	activateDevice(t, dcA, "B")

	// Unsigned revoke => 401: there is no credential but the device key.
	status, _ := c.anon().do("POST", "/v1/devices/B/revoke", nil)
	require.Equal(t, http.StatusUnauthorized, status, "unsigned revoke")

	// Properly signed revoke by an active device => 200. (Any active device may
	// revoke any device; A revokes B.)
	status, _ = dcA.do("POST", "/v1/devices/B/revoke", nil)
	require.Equal(t, http.StatusOK, status, "signed revoke")
	_ = dcB
}

// ---- DEK ----

func TestDEK(t *testing.T) {
	c := setup(t)
	dcA := bootstrapActive(t, c, "A") // current hk_version = 1

	mkDEKs := func(ids []string, version int) []wire.DEKWrap {
		out := make([]wire.DEKWrap, 0, len(ids))
		for _, id := range ids {
			out = append(out, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: version, Blob: []byte("dek-" + id)})
		}
		return out
	}

	ids := []string{"key-001", "key-002", "key-003", "key-004", "key-005"}

	// Upload all 5 (signed by active A).
	status, body := dcA.do("POST", "/v1/keys/dek", mkDEKs(ids, 1))
	require.Equalf(t, http.StatusOK, status, "upload dek: body %s", body)
	require.Equal(t, 5, mustJSON[map[string]int](t, body)["stored"], "upload dek stored")

	// Idempotent re-upload => stored 0.
	_, body = dcA.do("POST", "/v1/keys/dek", mkDEKs(ids, 1))
	require.Equal(t, 0, mustJSON[map[string]int](t, body)["stored"], "re-upload dek stored")

	// Wrong HKVersion => 400.
	status, _ = dcA.do("POST", "/v1/keys/dek", mkDEKs([]string{"key-999"}, 2))
	require.Equal(t, http.StatusBadRequest, status, "wrong hk_version dek")

	// Paging cursor walk (token-only GET), limit 2 over 5 keys => pages 2,2,1.
	cursor := ""
	var walked []string
	pages := 0
	for {
		path := "/v1/keys/dek?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, body = dcA.do("GET", path, nil)
		require.Equalf(t, http.StatusOK, status, "dek list: body %s", body)
		resp := mustJSON[wire.DEKListResp](t, body)
		for _, w := range resp.Wraps {
			walked = append(walked, w.KeyID)
		}
		pages++
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
		require.LessOrEqual(t, pages, 10, "too many dek pages")
	}
	require.Equal(t, 3, pages, "dek paging pages")
	require.Equal(t, ids, walked, "dek paging walked")
}

// ---- rotate ----

// rotateSetup bootstraps A, approves B, and uploads 3 DEKs at v1; it returns A's
// signing client (the surviving active device) and the DEK key ids.
func rotateSetup(t *testing.T, c *testClient) (signer *testClient, keyIDs []string) {
	dcA := bootstrapActive(t, c, "A")
	dcB := registerDevice(t, dcA, "B")
	activateDevice(t, dcA, "B")
	_ = dcB

	ids := []string{"key-001", "key-002", "key-003"}
	deks := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		deks = append(deks, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 1, Blob: []byte("v1-" + id)})
	}
	status, body := dcA.do("POST", "/v1/keys/dek", deks)
	require.Equalf(t, http.StatusOK, status, "rotate setup dek: body %s", body)
	return dcA, ids
}

func TestRotateHappy(t *testing.T) {
	c := setup(t)
	dcA, ids := rotateSetup(t, c)

	// Revoke B (signed by active A); only A survives.
	status, _ := dcA.do("POST", "/v1/devices/B/revoke", nil)
	require.Equal(t, http.StatusOK, status, "revoke B failed")

	newDEKs := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		newDEKs = append(newDEKs, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 2, Blob: []byte("v2-" + id)})
	}
	req := wire.RotateReq{
		HKVersion: 2,
		HKWraps:   []wire.HKWrap{{DeviceID: "A", HKVersion: 2, Blob: []byte("hk2-A")}},
		DEKWraps:  newDEKs,
	}
	status, body := dcA.do("POST", "/v1/keys/rotate", req)
	require.Equalf(t, http.StatusOK, status, "rotate: body %s", body)

	// Version bumped: A's hk wrap now v2.
	status, body = dcA.do("GET", "/v1/keys/hk?device_id=A", nil)
	require.Equal(t, http.StatusOK, status, "get hk A")
	require.Equal(t, 2, mustJSON[wire.HKWrap](t, body).HKVersion, "A hk after rotate")

	// B's wrap is gone (was revoked before rotate).
	status, _ = dcA.do("GET", "/v1/keys/hk?device_id=B", nil)
	require.Equal(t, http.StatusNotFound, status, "get hk B after rotate")

	// All DEKs replaced with v2.
	_, body = dcA.do("GET", "/v1/keys/dek?limit=1000", nil)
	resp := mustJSON[wire.DEKListResp](t, body)
	require.Len(t, resp.Wraps, 3, "dek count after rotate")
	for _, w := range resp.Wraps {
		require.Equalf(t, 2, w.HKVersion, "dek %s version", w.KeyID)
	}

	// New DEKs at the new version are now accepted (confirms hk_version=2).
	status, _ = dcA.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-100", DeviceID: "A", HKVersion: 2, Blob: []byte("x")}})
	require.Equal(t, http.StatusOK, status, "post dek at v2 after rotate")
}

func TestRotatePartialIsAllOrNothing(t *testing.T) {
	c := setup(t)
	dcA, ids := rotateSetup(t, c)
	status, _ := dcA.do("POST", "/v1/devices/B/revoke", nil)
	require.Equal(t, http.StatusOK, status, "revoke B failed")

	// Provide only 2 of 3 DEKs => 400, nothing changes.
	partial := []wire.DEKWrap{
		{KeyID: ids[0], DeviceID: "A", HKVersion: 2, Blob: []byte("v2")},
		{KeyID: ids[1], DeviceID: "A", HKVersion: 2, Blob: []byte("v2")},
	}
	req := wire.RotateReq{
		HKVersion: 2,
		HKWraps:   []wire.HKWrap{{DeviceID: "A", HKVersion: 2, Blob: []byte("hk2-A")}},
		DEKWraps:  partial,
	}
	status, body := dcA.do("POST", "/v1/keys/rotate", req)
	require.Equalf(t, http.StatusBadRequest, status, "partial rotate: body %s", body)

	// Assert NOTHING changed: A's wrap still v1.
	_, body = dcA.do("GET", "/v1/keys/hk?device_id=A", nil)
	require.Equal(t, 1, mustJSON[wire.HKWrap](t, body).HKVersion, "A hk after failed rotate")
	// All 3 DEKs still present at v1.
	_, body = dcA.do("GET", "/v1/keys/dek?limit=1000", nil)
	resp := mustJSON[wire.DEKListResp](t, body)
	require.Len(t, resp.Wraps, 3, "dek count after failed rotate")
	for _, w := range resp.Wraps {
		require.Equalf(t, 1, w.HKVersion, "dek %s version want 1 (unchanged)", w.KeyID)
	}
	// hk_version unchanged: a fresh v1 DEK still validates (== current).
	status, _ = dcA.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-777", DeviceID: "A", HKVersion: 1, Blob: []byte("x")}})
	require.Equal(t, http.StatusOK, status, "post dek at v1 after failed rotate")
}

func TestRotateWrongVersion(t *testing.T) {
	c := setup(t)
	dcA, ids := rotateSetup(t, c)

	newDEKs := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		newDEKs = append(newDEKs, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 3, Blob: []byte("v3")})
	}
	// current is 1; must be current+1 (=2). Supply 3 => 400.
	req := wire.RotateReq{
		HKVersion: 3,
		HKWraps:   []wire.HKWrap{{DeviceID: "A", HKVersion: 3, Blob: []byte("hk3-A")}},
		DEKWraps:  newDEKs,
	}
	status, _ := dcA.do("POST", "/v1/keys/rotate", req)
	require.Equal(t, http.StatusBadRequest, status, "rotate wrong version")
}

// ---- concurrency ----

func TestConcurrentPushDistinctHosts(t *testing.T) {
	c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	const hosts = 10
	const perHost = 100

	// pushResult carries each goroutine's raw outcome back to the main goroutine,
	// where all assertions happen (testify's FailNow is illegal off the main test
	// goroutine). Each goroutine writes its own slice slot, so no lock is needed
	// and wg.Wait establishes happens-before for the reads below.
	type pushResult struct {
		hostID string
		status int
		body   []byte
		err    error
	}

	var wg sync.WaitGroup
	results := make([]pushResult, hosts)
	for h := 0; h < hosts; h++ {
		wg.Add(1)
		go func(h int) {
			defer wg.Done()
			// Each goroutine signs as the same active device with fresh nonces;
			// ed25519.Sign is safe for concurrent use and the nonce cache is
			// mutex-guarded.
			cc := *dev
			hostID := fmt.Sprintf("host-%02d", h)
			status, body, err := (&cc).sendConcurrent("POST", "/v1/records", wire.PushReq{HostID: hostID, Records: mkRecords(1, perHost)})
			results[h] = pushResult{hostID: hostID, status: status, body: body, err: err}
		}(h)
	}
	wg.Wait()

	for _, r := range results {
		require.NoErrorf(t, r.err, "host %s push", r.hostID)
		require.Equalf(t, http.StatusOK, r.status, "host %s push status; body %s", r.hostID, r.body)
		resp := mustJSON[wire.PushResp](t, r.body)
		require.Equalf(t, perHost, resp.Stored, "host %s stored", r.hostID)
		require.Equalf(t, uint64(perHost), resp.MaxSeq, "host %s maxSeq", r.hostID)
	}

	// Every host present with the right max seq.
	_, body := dev.do("GET", "/v1/hosts", nil)
	resp := mustJSON[wire.HostsResp](t, body)
	require.Len(t, resp.Hosts, hosts, "hosts count")
	for _, h := range resp.Hosts {
		require.Equalf(t, uint64(perHost), h.MaxSeq, "host %s maxSeq", h.HostID)
	}
}
