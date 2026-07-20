package syncer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"yore/internal/server"
	"yore/internal/wire"
)

const testToken = "test-token"

// newServer spins up a real sync server behind httptest and returns its base
// URL. The server and its temp DB are torn down at test end.
func newServer(t *testing.T) string {
	t.Helper()
	srv, err := server.New(server.Options{
		DBPath: filepath.Join(t.TempDir(), "sync.db"),
		Token:  testToken,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { srv.Close() })
	return ts.URL
}

func TestHTTPClientHealth(t *testing.T) {
	c := NewHTTPClient(newServer(t), testToken)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHTTPClientBadTokenIsAPIError(t *testing.T) {
	ctx := context.Background()
	c := NewHTTPClient(newServer(t), "wrong-token")

	// Health needs no token: it still succeeds.
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health with wrong token should still work: %v", err)
	}

	// An authed call must surface a typed 401.
	_, err := c.Hosts(ctx)
	if err == nil {
		t.Fatal("Hosts with wrong token: want error, got nil")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Status != http.StatusUnauthorized {
		t.Fatalf("want status 401, got %d", ae.Status)
	}
}

func TestHTTPClientDeviceLifecycle(t *testing.T) {
	ctx := context.Background()
	c := NewHTTPClient(newServer(t), testToken)

	// Register.
	pub := make([]byte, 32)
	pub[0] = 7
	dev, err := c.RegisterDevice(ctx, wire.RegisterReq{ID: "dev-1", Name: "laptop", PubKey: pub})
	if err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if dev.Status != wire.DevicePending {
		t.Fatalf("want pending, got %q", dev.Status)
	}

	// Duplicate register is a typed 409.
	_, err = c.RegisterDevice(ctx, wire.RegisterReq{ID: "dev-1", Name: "laptop", PubKey: pub})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("duplicate register: want APIError 409, got %v", err)
	}

	// List sees it.
	devs, err := c.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devs) != 1 || devs[0].ID != "dev-1" {
		t.Fatalf("ListDevices: unexpected %+v", devs)
	}

	// No HK wrap yet: found=false, no error.
	_, found, err := c.GetHKWrap(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetHKWrap: %v", err)
	}
	if found {
		t.Fatal("GetHKWrap: want found=false before activation")
	}
}

func TestHTTPClientPushPull(t *testing.T) {
	ctx := context.Background()
	c := NewHTTPClient(newServer(t), testToken)

	push, err := c.PushRecords(ctx, wire.PushReq{
		HostID: "host-A",
		Records: []wire.PushRecord{
			{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("blob-1")},
			{Seq: 2, ID: "r2", KeyID: "k1", Blob: []byte("blob-2")},
		},
	})
	if err != nil {
		t.Fatalf("PushRecords: %v", err)
	}
	if push.Stored != 2 || push.MaxSeq != 2 {
		t.Fatalf("PushResp: %+v", push)
	}

	// Idempotent re-push stores nothing.
	push2, err := c.PushRecords(ctx, wire.PushReq{
		HostID:  "host-A",
		Records: []wire.PushRecord{{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("blob-1")}},
	})
	if err != nil {
		t.Fatalf("PushRecords (re): %v", err)
	}
	if push2.Stored != 0 {
		t.Fatalf("re-push should store 0, got %d", push2.Stored)
	}

	pull, err := c.PullRecords(ctx, "host-A", 0, 100)
	if err != nil {
		t.Fatalf("PullRecords: %v", err)
	}
	if len(pull.Records) != 2 {
		t.Fatalf("want 2 records, got %d", len(pull.Records))
	}
	if string(pull.Records[0].Blob) != "blob-1" {
		t.Fatalf("blob mismatch: %q", pull.Records[0].Blob)
	}

	// Hosts sees the stream.
	hosts, err := c.Hosts(ctx)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].HostID != "host-A" || hosts[0].MaxSeq != 2 {
		t.Fatalf("Hosts: unexpected %+v", hosts)
	}
}

func TestVerificationCodeStableAndDistinct(t *testing.T) {
	var a, b [32]byte
	a[0] = 1
	b[0] = 2
	if VerificationCode(a) != VerificationCode(a) {
		t.Fatal("VerificationCode not stable for the same key")
	}
	if VerificationCode(a) == VerificationCode(b) {
		t.Fatal("VerificationCode collided for distinct keys")
	}
	// Format: 6 groups of 4 => 6*4 + 5 separators = 29 chars.
	if got := len(VerificationCode(a)); got != 29 {
		t.Fatalf("code length = %d, want 29 (%q)", got, VerificationCode(a))
	}
}
