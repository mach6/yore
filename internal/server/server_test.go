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

	"yore/internal/reqsign"
	"yore/internal/wire"
)

// ---- harness ----

// testClient talks to the server with a bearer token and, when it carries a
// signing identity (devID + priv), signs mutating requests exactly the way the
// real client (internal/syncer) must: reqsign.Sign over the request's
// RequestURI and the exact body bytes.
type testClient struct {
	t     *testing.T
	base  string
	token string // "" => send no Authorization header
	devID string
	priv  ed25519.PrivateKey // nil => send no signature headers
}

// withKey returns a copy of the client that signs as (devID, priv).
func (c *testClient) withKey(devID string, priv ed25519.PrivateKey) *testClient {
	cp := *c
	cp.devID = devID
	cp.priv = priv
	return &cp
}

func (c *testClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	return c.send(method, path, marshal(c.t, body), true)
}

// doRaw sends raw bytes as the body (for malformed-JSON cases).
func (c *testClient) doRaw(method, path string, raw []byte) (int, []byte) {
	c.t.Helper()
	return c.send(method, path, raw, true)
}

// send issues the request, attaching the bearer token and — when sign is true
// and the client has a signing identity — the reqsign headers over the exact
// body bytes and the request's RequestURI.
func (c *testClient) send(method, path string, body []byte, sign bool) (int, []byte) {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	if sign && c.priv != nil {
		for k, v := range c.signHeaders(req, method, body, time.Now()) {
			req.Header.Set(k, v)
		}
	}
	return c.roundtrip(req)
}

// sendSignedAt signs the request at a specific clock time (used to forge a
// stale-timestamp request).
func (c *testClient) sendSignedAt(method, path string, body []byte, now time.Time) (int, []byte) {
	c.t.Helper()
	req := c.newRequest(method, path, body)
	for k, v := range c.signHeaders(req, method, body, now) {
		req.Header.Set(k, v)
	}
	return c.roundtrip(req)
}

// sendWithHeaders replays a captured signature verbatim (same nonce).
func (c *testClient) sendWithHeaders(method, path string, body []byte, headers map[string]string) (int, []byte) {
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
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req
}

func (c *testClient) signHeaders(req *http.Request, method string, body []byte, now time.Time) map[string]string {
	c.t.Helper()
	hdrs, err := reqsign.Sign(c.devID, func(b []byte) []byte { return ed25519.Sign(c.priv, b) },
		method, req.URL.RequestURI(), body, now)
	if err != nil {
		c.t.Fatalf("sign: %v", err)
	}
	return hdrs
}

func (c *testClient) roundtrip(req *http.Request) (int, []byte) {
	c.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func marshal(t *testing.T, body any) []byte {
	t.Helper()
	if body == nil {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

func setup(t *testing.T) (*Server, *testClient) {
	t.Helper()
	s, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		s.Close()
	})
	return s, &testClient{t: t, base: srv.URL, token: "tok"}
}

func mustJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %T from %s: %v", v, data, err)
	}
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

// registerDevice generates a fresh Ed25519 keypair, self-signs a registration
// for id, and returns a client that signs later requests as that device.
func registerDevice(t *testing.T, base *testClient, id string) *testClient {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	dc := base.withKey(id, priv)
	status, body := dc.do("POST", "/v1/devices", wire.RegisterReq{ID: id, Name: id, PubKey: pubKey(), SignKey: pub})
	if status != http.StatusOK {
		t.Fatalf("register %s: status %d body %s", id, status, body)
	}
	return dc
}

// activateDevice activates id, signed by `signer` — an already-active device,
// or (for the very first device) the pending device itself at bootstrap.
func activateDevice(t *testing.T, signer *testClient, id string, version int) {
	t.Helper()
	req := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: id, HKVersion: version, Blob: []byte("hk-" + id)}}
	status, body := signer.do("POST", "/v1/devices/"+id+"/activate", req)
	if status != http.StatusOK {
		t.Fatalf("activate %s: status %d body %s", id, status, body)
	}
}

// bootstrapActive registers id and activates it as the first (bootstrap) device,
// returning its signing client. Use once per server.
func bootstrapActive(t *testing.T, base *testClient, id string) *testClient {
	t.Helper()
	dc := registerDevice(t, base, id)
	activateDevice(t, dc, id, 1) // bootstrap: the pending device self-activates
	return dc
}

// ---- New ----

func TestNewRefusesEmptyToken(t *testing.T) {
	_, err := New(Options{DBPath: filepath.Join(t.TempDir(), "x.db"), Token: ""})
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

// ---- auth (bearer token; runs before signatures) ----

func TestAuth(t *testing.T) {
	_, c := setup(t)

	// Health is open.
	noAuth := &testClient{t: t, base: c.base, token: ""}
	if status, _ := noAuth.do("GET", "/v1/health", nil); status != http.StatusOK {
		t.Fatalf("health no-auth: got %d", status)
	}

	wrong := &testClient{t: t, base: c.base, token: "nope"}
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
		if status, _ := noAuth.do(r.method, r.path, nil); status != http.StatusUnauthorized {
			t.Errorf("no header %s %s: got %d want 401", r.method, r.path, status)
		}
		if status, _ := wrong.do(r.method, r.path, nil); status != http.StatusUnauthorized {
			t.Errorf("wrong token %s %s: got %d want 401", r.method, r.path, status)
		}
	}
}

// ---- register signature ----

func TestRegisterSignature(t *testing.T) {
	s, c := setup(t)

	// Happy path: self-signed register stores the sign key and is pending.
	pub, priv, _ := ed25519.GenerateKey(nil)
	dc := c.withKey("A", priv)
	status, body := dc.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pub})
	if status != http.StatusOK {
		t.Fatalf("register: status %d body %s", status, body)
	}
	if d := mustJSON[wire.Device](t, body); d.Status != wire.DevicePending || !bytes.Equal(d.SignKey, pub) {
		t.Fatalf("register: status=%q signKeyStored=%v", d.Status, bytes.Equal(d.SignKey, pub))
	}
	_ = s

	// No signature (valid token) => 401.
	if status, _ := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "B", Name: "B", PubKey: pubKey(), SignKey: pub}); status != http.StatusUnauthorized {
		t.Fatalf("unsigned register: got %d want 401", status)
	}

	// sign_key not 32 bytes => 400.
	if status, _ := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "C", PubKey: pubKey(), SignKey: []byte("short")}); status != http.StatusBadRequest {
		t.Fatalf("short sign_key: got %d want 400", status)
	}

	// Header device id != body id => rejected. dc2 signs as "A" but claims id "D".
	dc2 := c.withKey("A", priv)
	if status, _ := dc2.do("POST", "/v1/devices", wire.RegisterReq{ID: "D", PubKey: pubKey(), SignKey: pub}); status != http.StatusUnauthorized {
		t.Fatalf("device-id mismatch register: got %d want 401", status)
	}
}

// ---- push signature ----

func TestPushSignature(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")
	pushBody := marshal(t, wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})

	// Signed push from an active device succeeds.
	hdrs := dev.signOnly("POST", "/v1/records", pushBody)
	if status, body := dev.sendWithHeaders("POST", "/v1/records", pushBody, hdrs); status != http.StatusOK {
		t.Fatalf("signed push: status %d body %s", status, body)
	}
	// Same request replayed (same nonce) => 401.
	if status, _ := dev.sendWithHeaders("POST", "/v1/records", pushBody, hdrs); status != http.StatusUnauthorized {
		t.Fatalf("replayed push: got %d want 401", status)
	}

	// Unsigned push (valid token, no signature) => 401.
	if status, _ := c.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(10, 1)}); status != http.StatusUnauthorized {
		t.Fatalf("unsigned push: got %d want 401", status)
	}

	// Push signed by a different key than registered => 401.
	_, wrongPriv, _ := ed25519.GenerateKey(nil)
	bad := c.withKey("pusher", wrongPriv)
	if status, _ := bad.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(20, 1)}); status != http.StatusUnauthorized {
		t.Fatalf("wrong-key push: got %d want 401", status)
	}

	// Push with a stale timestamp => 401.
	stale := marshal(t, wire.PushReq{HostID: "hostA", Records: mkRecords(30, 1)})
	if status, _ := dev.sendSignedAt("POST", "/v1/records", stale, time.Now().Add(-2*reqsign.Skew)); status != http.StatusUnauthorized {
		t.Fatalf("stale-timestamp push: got %d want 401", status)
	}
}

func TestPushHappyAndIdempotent(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	status, body := dev.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	if status != http.StatusOK {
		t.Fatalf("push: status %d body %s", status, body)
	}
	resp := mustJSON[wire.PushResp](t, body)
	if resp.Stored != 3 || resp.MaxSeq != 3 {
		t.Fatalf("push: got stored=%d maxSeq=%d want 3/3", resp.Stored, resp.MaxSeq)
	}

	// Re-push the same batch (fresh signature/nonce): nothing stored, MaxSeq stable.
	status, body = dev.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	if status != http.StatusOK {
		t.Fatalf("re-push: status %d body %s", status, body)
	}
	resp = mustJSON[wire.PushResp](t, body)
	if resp.Stored != 0 || resp.MaxSeq != 3 {
		t.Fatalf("re-push: got stored=%d maxSeq=%d want 0/3", resp.Stored, resp.MaxSeq)
	}
}

func TestPushValidation(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	// Missing host_id.
	if status, _ := dev.do("POST", "/v1/records", wire.PushReq{Records: mkRecords(1, 1)}); status != http.StatusBadRequest {
		t.Errorf("empty host_id: got %d want 400", status)
	}

	// Descending seqs.
	desc := []wire.PushRecord{{Seq: 3}, {Seq: 2}, {Seq: 1}}
	if status, _ := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: desc}); status != http.StatusBadRequest {
		t.Errorf("descending seqs: got %d want 400", status)
	}

	// More than 1000 records.
	if status, _ := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 1001)}); status != http.StatusBadRequest {
		t.Errorf(">1000 records: got %d want 400", status)
	}

	// Malformed JSON (validly signed over the raw bytes).
	if status, _ := dev.doRaw("POST", "/v1/records", []byte("{not json")); status != http.StatusBadRequest {
		t.Errorf("malformed JSON: got %d want 400", status)
	}
}

// ---- pull ----

func TestPullPaging(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	// Load 2500 records over 3 pushes (push caps at 1000).
	for _, batch := range [][2]uint64{{1, 1000}, {1001, 1000}, {2001, 500}} {
		status, body := dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(batch[0], batch[1])})
		if status != http.StatusOK {
			t.Fatalf("load push: status %d body %s", status, body)
		}
	}

	after := uint64(0)
	total := 0
	pages := 0
	for {
		status, body := c.do("GET", fmt.Sprintf("/v1/records?host_id=h&after=%d&limit=1000", after), nil)
		if status != http.StatusOK {
			t.Fatalf("pull: status %d body %s", status, body)
		}
		resp := mustJSON[wire.PullResp](t, body)
		total += len(resp.Records)
		pages++
		// Verify ascending & contiguous.
		for i, r := range resp.Records {
			want := after + uint64(i) + 1
			if r.Seq != want {
				t.Fatalf("page %d record %d: seq %d want %d", pages, i, r.Seq, want)
			}
		}
		if resp.NextAfter == nil {
			break
		}
		after = *resp.NextAfter
		if pages > 10 {
			t.Fatal("too many pages")
		}
	}
	if total != 2500 {
		t.Fatalf("pulled %d records want 2500", total)
	}
	if pages != 3 {
		t.Fatalf("used %d pages want 3", pages)
	}
}

func TestPullUnknownAndBeyond(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	// Unknown host => empty, no NextAfter, not 404.
	status, body := c.do("GET", "/v1/records?host_id=nope", nil)
	if status != http.StatusOK {
		t.Fatalf("unknown host: status %d body %s", status, body)
	}
	resp := mustJSON[wire.PullResp](t, body)
	if len(resp.Records) != 0 || resp.NextAfter != nil {
		t.Fatalf("unknown host: got %d records nextAfter=%v", len(resp.Records), resp.NextAfter)
	}

	// after beyond end => empty, no NextAfter.
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 3)})
	status, body = c.do("GET", "/v1/records?host_id=h&after=100", nil)
	if status != http.StatusOK {
		t.Fatalf("beyond end: status %d body %s", status, body)
	}
	resp = mustJSON[wire.PullResp](t, body)
	if len(resp.Records) != 0 || resp.NextAfter != nil {
		t.Fatalf("beyond end: got %d records nextAfter=%v", len(resp.Records), resp.NextAfter)
	}
}

// ---- reads stay token-only ----

func TestReadsTokenOnly(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "reader")
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 3)})

	// GET endpoints work with the token alone — no signature attached.
	reads := []string{"/v1/health", "/v1/hosts", "/v1/records?host_id=h", "/v1/devices", "/v1/keys/dek"}
	for _, path := range reads {
		if status, body := c.do("GET", path, nil); status != http.StatusOK {
			t.Errorf("read %s: got %d want 200 body %s", path, status, body)
		}
	}
	// Health is open even without the token.
	noAuth := &testClient{t: t, base: c.base, token: ""}
	if status, _ := noAuth.do("GET", "/v1/health", nil); status != http.StatusOK {
		t.Errorf("health no-auth: got %d want 200", status)
	}
}

// ---- hosts ----

func TestHosts(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "alpha", Records: mkRecords(1, 5)})
	dev.do("POST", "/v1/records", wire.PushReq{HostID: "beta", Records: mkRecords(1, 9)})

	status, body := c.do("GET", "/v1/hosts", nil)
	if status != http.StatusOK {
		t.Fatalf("hosts: status %d body %s", status, body)
	}
	resp := mustJSON[wire.HostsResp](t, body)
	got := map[string]uint64{}
	for _, h := range resp.Hosts {
		got[h.HostID] = h.MaxSeq
	}
	if got["alpha"] != 5 || got["beta"] != 9 {
		t.Fatalf("hosts max seqs: got %+v want alpha=5 beta=9", got)
	}
}

// ---- device lifecycle ----

func TestDeviceLifecycle(t *testing.T) {
	_, c := setup(t)

	// Register A (self-signed) => pending.
	pubA, privA, _ := ed25519.GenerateKey(nil)
	dcA := c.withKey("A", privA)
	status, body := dcA.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pubA})
	if status != http.StatusOK {
		t.Fatalf("register A: status %d body %s", status, body)
	}
	if d := mustJSON[wire.Device](t, body); d.Status != wire.DevicePending {
		t.Fatalf("register A: status %q want pending", d.Status)
	}

	// Duplicate (still self-signed, fresh nonce) => 409.
	if status, _ := dcA.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pubA}); status != http.StatusConflict {
		t.Fatalf("duplicate register: got %d want 409", status)
	}

	// Bad pubkey => 400 (shape check precedes signature).
	if status, _ := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "Z", PubKey: []byte("short"), SignKey: pubA}); status != http.StatusBadRequest {
		t.Fatalf("short pubkey: got %d want 400", status)
	}

	// Activate with wrong wrap.DeviceID => 400 (A self-signs at bootstrap).
	badWrap := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "other", HKVersion: 1}}
	if status, _ := dcA.do("POST", "/v1/devices/A/activate", badWrap); status != http.StatusBadRequest {
		t.Fatalf("wrong wrap.device_id: got %d want 400", status)
	}

	// Bootstrap first activate sets version=1.
	activateDevice(t, dcA, "A", 1)

	// hk_version is now 1: activating B at wrong version fails, at 1 succeeds.
	// B is approved by the active device A (signer need not be the resource).
	dcB := registerDevice(t, c, "B")
	_ = dcB
	wrongVer := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "B", HKVersion: 2}}
	if status, _ := dcA.do("POST", "/v1/devices/B/activate", wrongVer); status != http.StatusBadRequest {
		t.Fatalf("activate B wrong version: got %d want 400", status)
	}
	activateDevice(t, dcA, "B", 1)

	// A's HK wrap is retrievable (token-only GET).
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=A", nil); status != http.StatusOK {
		t.Fatalf("get hk A: got %d want 200", status)
	}

	// Revoke A, signed by active device B => deletes A's wrap.
	if status, _ := dcB.do("POST", "/v1/devices/A/revoke", nil); status != http.StatusOK {
		t.Fatalf("revoke A: got %d want 200", status)
	}
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=A", nil); status != http.StatusNotFound {
		t.Fatalf("get hk A after revoke: got %d want 404", status)
	}

	// Activate revoked A (signed by active B) => 409.
	reactivate := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "A", HKVersion: 1}}
	if status, _ := dcB.do("POST", "/v1/devices/A/activate", reactivate); status != http.StatusConflict {
		t.Fatalf("activate revoked A: got %d want 409", status)
	}

	// Revoke unknown (signed by active B) => 404.
	if status, _ := dcB.do("POST", "/v1/devices/ghost/revoke", nil); status != http.StatusNotFound {
		t.Fatalf("revoke unknown: got %d want 404", status)
	}

	// List returns all devices regardless of status.
	status, body = c.do("GET", "/v1/devices", nil)
	if status != http.StatusOK {
		t.Fatalf("list devices: status %d", status)
	}
	if devs := mustJSON[[]wire.Device](t, body); len(devs) != 2 {
		t.Fatalf("list devices: got %d want 2", len(devs))
	}

	// hk get for a device that never had a wrap => 404.
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=ghost", nil); status != http.StatusNotFound {
		t.Fatalf("get hk ghost: got %d want 404", status)
	}
}

// ---- revoke signature ----

func TestRevokeSignature(t *testing.T) {
	_, c := setup(t)
	dcA := bootstrapActive(t, c, "A")
	dcB := registerDevice(t, c, "B")
	activateDevice(t, dcA, "B", 1)

	// Token-only revoke (no signature) => 401.
	if status, _ := c.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusUnauthorized {
		t.Fatalf("token-only revoke: got %d want 401", status)
	}

	// Properly signed revoke by an active device => 200. (Any active device may
	// revoke any device; A revokes B.)
	if status, _ := dcA.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusOK {
		t.Fatalf("signed revoke: got %d want 200", status)
	}
	_ = dcB
}

// ---- DEK ----

func TestDEK(t *testing.T) {
	_, c := setup(t)
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
	if status != http.StatusOK {
		t.Fatalf("upload dek: status %d body %s", status, body)
	}
	if got := mustJSON[map[string]int](t, body)["stored"]; got != 5 {
		t.Fatalf("upload dek: stored %d want 5", got)
	}

	// Idempotent re-upload => stored 0.
	status, body = dcA.do("POST", "/v1/keys/dek", mkDEKs(ids, 1))
	if got := mustJSON[map[string]int](t, body)["stored"]; got != 0 {
		t.Fatalf("re-upload dek: stored %d want 0", got)
	}

	// Wrong HKVersion => 400.
	if status, _ := dcA.do("POST", "/v1/keys/dek", mkDEKs([]string{"key-999"}, 2)); status != http.StatusBadRequest {
		t.Fatalf("wrong hk_version dek: got %d want 400", status)
	}

	// Paging cursor walk (token-only GET), limit 2 over 5 keys => pages 2,2,1.
	cursor := ""
	var walked []string
	pages := 0
	for {
		path := "/v1/keys/dek?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, body = c.do("GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("dek list: status %d body %s", status, body)
		}
		resp := mustJSON[wire.DEKListResp](t, body)
		for _, w := range resp.Wraps {
			walked = append(walked, w.KeyID)
		}
		pages++
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
		if pages > 10 {
			t.Fatal("too many dek pages")
		}
	}
	if pages != 3 {
		t.Fatalf("dek paging: used %d pages want 3", pages)
	}
	if fmt.Sprint(walked) != fmt.Sprint(ids) {
		t.Fatalf("dek paging: walked %v want %v", walked, ids)
	}
}

// ---- rotate ----

// rotateSetup bootstraps A, approves B, and uploads 3 DEKs at v1; it returns A's
// signing client (the surviving active device) and the DEK key ids.
func rotateSetup(t *testing.T, c *testClient) (*testClient, []string) {
	dcA := bootstrapActive(t, c, "A")
	dcB := registerDevice(t, c, "B")
	activateDevice(t, dcA, "B", 1)
	_ = dcB

	ids := []string{"key-001", "key-002", "key-003"}
	deks := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		deks = append(deks, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 1, Blob: []byte("v1-" + id)})
	}
	if status, body := dcA.do("POST", "/v1/keys/dek", deks); status != http.StatusOK {
		t.Fatalf("rotate setup dek: status %d body %s", status, body)
	}
	return dcA, ids
}

func TestRotateHappy(t *testing.T) {
	_, c := setup(t)
	dcA, ids := rotateSetup(t, c)

	// Revoke B (signed by active A); only A survives.
	if status, _ := dcA.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusOK {
		t.Fatal("revoke B failed")
	}

	newDEKs := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		newDEKs = append(newDEKs, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 2, Blob: []byte("v2-" + id)})
	}
	req := wire.RotateReq{
		HKVersion: 2,
		HKWraps:   []wire.HKWrap{{DeviceID: "A", HKVersion: 2, Blob: []byte("hk2-A")}},
		DEKWraps:  newDEKs,
	}
	if status, body := dcA.do("POST", "/v1/keys/rotate", req); status != http.StatusOK {
		t.Fatalf("rotate: status %d body %s", status, body)
	}

	// Version bumped: A's hk wrap now v2.
	status, body := c.do("GET", "/v1/keys/hk?device_id=A", nil)
	if status != http.StatusOK {
		t.Fatalf("get hk A: status %d", status)
	}
	if w := mustJSON[wire.HKWrap](t, body); w.HKVersion != 2 {
		t.Fatalf("A hk after rotate: version %d want 2", w.HKVersion)
	}

	// B's wrap is gone (was revoked before rotate).
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=B", nil); status != http.StatusNotFound {
		t.Fatalf("get hk B after rotate: got %d want 404", status)
	}

	// All DEKs replaced with v2.
	status, body = c.do("GET", "/v1/keys/dek?limit=1000", nil)
	resp := mustJSON[wire.DEKListResp](t, body)
	if len(resp.Wraps) != 3 {
		t.Fatalf("dek count after rotate: %d want 3", len(resp.Wraps))
	}
	for _, w := range resp.Wraps {
		if w.HKVersion != 2 {
			t.Fatalf("dek %s version %d want 2", w.KeyID, w.HKVersion)
		}
	}

	// New DEKs at the new version are now accepted (confirms hk_version=2).
	if status, _ := dcA.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-100", DeviceID: "A", HKVersion: 2, Blob: []byte("x")}}); status != http.StatusOK {
		t.Fatalf("post dek at v2 after rotate: got %d want 200", status)
	}
}

func TestRotatePartialIsAllOrNothing(t *testing.T) {
	_, c := setup(t)
	dcA, ids := rotateSetup(t, c)
	if status, _ := dcA.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusOK {
		t.Fatal("revoke B failed")
	}

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
	if status != http.StatusBadRequest {
		t.Fatalf("partial rotate: got %d want 400 body %s", status, body)
	}

	// Assert NOTHING changed: A's wrap still v1.
	_, body = c.do("GET", "/v1/keys/hk?device_id=A", nil)
	if w := mustJSON[wire.HKWrap](t, body); w.HKVersion != 1 {
		t.Fatalf("A hk after failed rotate: version %d want 1", w.HKVersion)
	}
	// All 3 DEKs still present at v1.
	_, body = c.do("GET", "/v1/keys/dek?limit=1000", nil)
	resp := mustJSON[wire.DEKListResp](t, body)
	if len(resp.Wraps) != 3 {
		t.Fatalf("dek count after failed rotate: %d want 3", len(resp.Wraps))
	}
	for _, w := range resp.Wraps {
		if w.HKVersion != 1 {
			t.Fatalf("dek %s version %d want 1 (unchanged)", w.KeyID, w.HKVersion)
		}
	}
	// hk_version unchanged: a fresh v1 DEK still validates (== current).
	if status, _ := dcA.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-777", DeviceID: "A", HKVersion: 1, Blob: []byte("x")}}); status != http.StatusOK {
		t.Fatalf("post dek at v1 after failed rotate: got %d want 200 (version unchanged)", status)
	}
}

func TestRotateWrongVersion(t *testing.T) {
	_, c := setup(t)
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
	if status, _ := dcA.do("POST", "/v1/keys/rotate", req); status != http.StatusBadRequest {
		t.Fatalf("rotate wrong version: got %d want 400", status)
	}
}

// ---- concurrency ----

func TestConcurrentPushDistinctHosts(t *testing.T) {
	_, c := setup(t)
	dev := bootstrapActive(t, c, "pusher")

	const hosts = 10
	const perHost = 100

	var wg sync.WaitGroup
	errs := make([]error, hosts)
	for h := 0; h < hosts; h++ {
		wg.Add(1)
		go func(h int) {
			defer wg.Done()
			// Each goroutine signs as the same active device with fresh nonces;
			// ed25519.Sign is safe for concurrent use and the nonce cache is
			// mutex-guarded.
			cc := *dev
			hostID := fmt.Sprintf("host-%02d", h)
			status, body := (&cc).do("POST", "/v1/records", wire.PushReq{HostID: hostID, Records: mkRecords(1, perHost)})
			if status != http.StatusOK {
				errs[h] = fmt.Errorf("host %s push status %d body %s", hostID, status, body)
				return
			}
			resp := mustJSON[wire.PushResp](t, body)
			if resp.Stored != perHost || resp.MaxSeq != perHost {
				errs[h] = fmt.Errorf("host %s stored=%d maxSeq=%d", hostID, resp.Stored, resp.MaxSeq)
			}
		}(h)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Every host present with the right max seq.
	_, body := c.do("GET", "/v1/hosts", nil)
	resp := mustJSON[wire.HostsResp](t, body)
	if len(resp.Hosts) != hosts {
		t.Fatalf("hosts: got %d want %d", len(resp.Hosts), hosts)
	}
	for _, h := range resp.Hosts {
		if h.MaxSeq != perHost {
			t.Fatalf("host %s maxSeq=%d want %d", h.HostID, h.MaxSeq, perHost)
		}
	}
}
