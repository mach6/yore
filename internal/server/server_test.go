package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"yore/internal/wire"
)

// ---- harness ----

type testClient struct {
	t     *testing.T
	base  string
	token string // "" => send no Authorization header
}

func (c *testClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	return c.doRaw(method, path, r)
}

func (c *testClient) doRaw(method, path string, r io.Reader) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
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

func registerDevice(t *testing.T, c *testClient, id string) {
	t.Helper()
	status, body := c.do("POST", "/v1/devices", wire.RegisterReq{ID: id, Name: id, PubKey: pubKey()})
	if status != http.StatusOK {
		t.Fatalf("register %s: status %d body %s", id, status, body)
	}
}

func activateDevice(t *testing.T, c *testClient, id string, version int) {
	t.Helper()
	req := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: id, HKVersion: version, Blob: []byte("hk-" + id)}}
	status, body := c.do("POST", "/v1/devices/"+id+"/activate", req)
	if status != http.StatusOK {
		t.Fatalf("activate %s: status %d body %s", id, status, body)
	}
}

// ---- New ----

func TestNewRefusesEmptyToken(t *testing.T) {
	_, err := New(Options{DBPath: filepath.Join(t.TempDir(), "x.db"), Token: ""})
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

// ---- auth ----

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

// ---- push ----

func TestPushHappyAndIdempotent(t *testing.T) {
	_, c := setup(t)

	status, body := c.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	if status != http.StatusOK {
		t.Fatalf("push: status %d body %s", status, body)
	}
	resp := mustJSON[wire.PushResp](t, body)
	if resp.Stored != 3 || resp.MaxSeq != 3 {
		t.Fatalf("push: got stored=%d maxSeq=%d want 3/3", resp.Stored, resp.MaxSeq)
	}

	// Re-push the same batch: nothing stored, MaxSeq stable.
	status, body = c.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
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

	// Missing host_id.
	if status, _ := c.do("POST", "/v1/records", wire.PushReq{Records: mkRecords(1, 1)}); status != http.StatusBadRequest {
		t.Errorf("empty host_id: got %d want 400", status)
	}

	// Descending seqs.
	desc := []wire.PushRecord{{Seq: 3}, {Seq: 2}, {Seq: 1}}
	if status, _ := c.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: desc}); status != http.StatusBadRequest {
		t.Errorf("descending seqs: got %d want 400", status)
	}

	// More than 1000 records.
	if status, _ := c.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 1001)}); status != http.StatusBadRequest {
		t.Errorf(">1000 records: got %d want 400", status)
	}

	// Malformed JSON.
	if status, _ := c.doRaw("POST", "/v1/records", bytes.NewReader([]byte("{not json"))); status != http.StatusBadRequest {
		t.Errorf("malformed JSON: got %d want 400", status)
	}
}

// ---- pull ----

func TestPullPaging(t *testing.T) {
	_, c := setup(t)

	// Load 2500 records over 3 pushes (push caps at 1000).
	for _, batch := range [][2]uint64{{1, 1000}, {1001, 1000}, {2001, 500}} {
		status, body := c.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(batch[0], batch[1])})
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
	c.do("POST", "/v1/records", wire.PushReq{HostID: "h", Records: mkRecords(1, 3)})
	status, body = c.do("GET", "/v1/records?host_id=h&after=100", nil)
	if status != http.StatusOK {
		t.Fatalf("beyond end: status %d body %s", status, body)
	}
	resp = mustJSON[wire.PullResp](t, body)
	if len(resp.Records) != 0 || resp.NextAfter != nil {
		t.Fatalf("beyond end: got %d records nextAfter=%v", len(resp.Records), resp.NextAfter)
	}
}

// ---- hosts ----

func TestHosts(t *testing.T) {
	_, c := setup(t)
	c.do("POST", "/v1/records", wire.PushReq{HostID: "alpha", Records: mkRecords(1, 5)})
	c.do("POST", "/v1/records", wire.PushReq{HostID: "beta", Records: mkRecords(1, 9)})

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

	// Register A => pending.
	status, body := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey()})
	if status != http.StatusOK {
		t.Fatalf("register A: status %d body %s", status, body)
	}
	if d := mustJSON[wire.Device](t, body); d.Status != wire.DevicePending {
		t.Fatalf("register A: status %q want pending", d.Status)
	}

	// Duplicate => 409.
	if status, _ := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey()}); status != http.StatusConflict {
		t.Fatalf("duplicate register: got %d want 409", status)
	}

	// Bad pubkey => 400.
	if status, _ := c.do("POST", "/v1/devices", wire.RegisterReq{ID: "Z", PubKey: []byte("short")}); status != http.StatusBadRequest {
		t.Fatalf("short pubkey: got %d want 400", status)
	}

	// Activate with wrong wrap.DeviceID => 400.
	badWrap := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "other", HKVersion: 1}}
	if status, _ := c.do("POST", "/v1/devices/A/activate", badWrap); status != http.StatusBadRequest {
		t.Fatalf("wrong wrap.device_id: got %d want 400", status)
	}

	// Bootstrap first activate sets version=1.
	activateDevice(t, c, "A", 1)

	// hk_version is now 1: activating B at wrong version fails, at 1 succeeds.
	registerDevice(t, c, "B")
	wrongVer := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "B", HKVersion: 2}}
	if status, _ := c.do("POST", "/v1/devices/B/activate", wrongVer); status != http.StatusBadRequest {
		t.Fatalf("activate B wrong version: got %d want 400", status)
	}
	activateDevice(t, c, "B", 1)

	// A's HK wrap is retrievable.
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=A", nil); status != http.StatusOK {
		t.Fatalf("get hk A: got %d want 200", status)
	}

	// Revoke A => deletes its wrap.
	if status, _ := c.do("POST", "/v1/devices/A/revoke", nil); status != http.StatusOK {
		t.Fatalf("revoke A: got %d want 200", status)
	}
	if status, _ := c.do("GET", "/v1/keys/hk?device_id=A", nil); status != http.StatusNotFound {
		t.Fatalf("get hk A after revoke: got %d want 404", status)
	}

	// Activate revoked => 409.
	reactivate := wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: "A", HKVersion: 1}}
	if status, _ := c.do("POST", "/v1/devices/A/activate", reactivate); status != http.StatusConflict {
		t.Fatalf("activate revoked A: got %d want 409", status)
	}

	// Revoke unknown => 404.
	if status, _ := c.do("POST", "/v1/devices/ghost/revoke", nil); status != http.StatusNotFound {
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

// ---- DEK ----

func TestDEK(t *testing.T) {
	_, c := setup(t)
	registerDevice(t, c, "A")
	activateDevice(t, c, "A", 1) // current hk_version = 1

	mkDEKs := func(ids []string, version int) []wire.DEKWrap {
		out := make([]wire.DEKWrap, 0, len(ids))
		for _, id := range ids {
			out = append(out, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: version, Blob: []byte("dek-" + id)})
		}
		return out
	}

	ids := []string{"key-001", "key-002", "key-003", "key-004", "key-005"}

	// Upload all 5.
	status, body := c.do("POST", "/v1/keys/dek", mkDEKs(ids, 1))
	if status != http.StatusOK {
		t.Fatalf("upload dek: status %d body %s", status, body)
	}
	if got := mustJSON[map[string]int](t, body)["stored"]; got != 5 {
		t.Fatalf("upload dek: stored %d want 5", got)
	}

	// Idempotent re-upload => stored 0.
	status, body = c.do("POST", "/v1/keys/dek", mkDEKs(ids, 1))
	if got := mustJSON[map[string]int](t, body)["stored"]; got != 0 {
		t.Fatalf("re-upload dek: stored %d want 0", got)
	}

	// Wrong HKVersion => 400.
	if status, _ := c.do("POST", "/v1/keys/dek", mkDEKs([]string{"key-999"}, 2)); status != http.StatusBadRequest {
		t.Fatalf("wrong hk_version dek: got %d want 400", status)
	}

	// Paging cursor walk, limit 2 over 5 keys => pages 2,2,1.
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

// rotateSetup registers/activates A and B at v1 and uploads 3 DEKs at v1.
func rotateSetup(t *testing.T, c *testClient) []string {
	registerDevice(t, c, "A")
	activateDevice(t, c, "A", 1)
	registerDevice(t, c, "B")
	activateDevice(t, c, "B", 1)

	ids := []string{"key-001", "key-002", "key-003"}
	deks := make([]wire.DEKWrap, 0, len(ids))
	for _, id := range ids {
		deks = append(deks, wire.DEKWrap{KeyID: id, DeviceID: "A", HKVersion: 1, Blob: []byte("v1-" + id)})
	}
	if status, body := c.do("POST", "/v1/keys/dek", deks); status != http.StatusOK {
		t.Fatalf("rotate setup dek: status %d body %s", status, body)
	}
	return ids
}

func TestRotateHappy(t *testing.T) {
	_, c := setup(t)
	ids := rotateSetup(t, c)

	// Revoke B; only A survives.
	if status, _ := c.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusOK {
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
	if status, body := c.do("POST", "/v1/keys/rotate", req); status != http.StatusOK {
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
	if status, _ := c.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-100", DeviceID: "A", HKVersion: 2, Blob: []byte("x")}}); status != http.StatusOK {
		t.Fatalf("post dek at v2 after rotate: got %d want 200", status)
	}
}

func TestRotatePartialIsAllOrNothing(t *testing.T) {
	_, c := setup(t)
	ids := rotateSetup(t, c)
	if status, _ := c.do("POST", "/v1/devices/B/revoke", nil); status != http.StatusOK {
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
	status, body := c.do("POST", "/v1/keys/rotate", req)
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
	if status, _ := c.do("POST", "/v1/keys/dek", []wire.DEKWrap{{KeyID: "key-777", DeviceID: "A", HKVersion: 1, Blob: []byte("x")}}); status != http.StatusOK {
		t.Fatalf("post dek at v1 after failed rotate: got %d want 200 (version unchanged)", status)
	}
}

func TestRotateWrongVersion(t *testing.T) {
	_, c := setup(t)
	ids := rotateSetup(t, c)

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
	if status, _ := c.do("POST", "/v1/keys/rotate", req); status != http.StatusBadRequest {
		t.Fatalf("rotate wrong version: got %d want 400", status)
	}
}

// ---- concurrency ----

func TestConcurrentPushDistinctHosts(t *testing.T) {
	_, c := setup(t)

	const hosts = 10
	const perHost = 100

	var wg sync.WaitGroup
	errs := make([]error, hosts)
	for h := 0; h < hosts; h++ {
		wg.Add(1)
		go func(h int) {
			defer wg.Done()
			// Each goroutine gets its own client (own http transport usage is safe,
			// but this keeps t.Helper accounting clean).
			cc := &testClient{t: t, base: c.base, token: c.token}
			hostID := fmt.Sprintf("host-%02d", h)
			status, body := cc.do("POST", "/v1/records", wire.PushReq{HostID: hostID, Records: mkRecords(1, perHost)})
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
