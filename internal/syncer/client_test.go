package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/cryptobox"
	"yore/internal/rec"
	"yore/internal/server"
	"yore/internal/wire"
)

const testToken = "test-token"

// enroll registers and self-activates a fresh ACTIVE device on the server (the
// server's first-device bootstrap path), returning a request-signing client for
// it. Mutating endpoints now require an active device's signature, so transport
// tests that push/etc. need this.
func enroll(t *testing.T, url string) *HTTPClient {
	t.Helper()
	ctx := context.Background()
	dk, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)
	id := rec.NewID()
	c := NewHTTPClient(url, "")
	c.SetSigner(id, dk.Sign)
	pub := dk.Public()
	_, err = c.RegisterDevice(ctx, wire.RegisterReq{ID: id, Name: "test", PubKey: pub[:], SignKey: dk.SignPublic()}, testToken)
	require.NoError(t, err, "enroll register")
	hk, err := cryptobox.NewHistoryKey()
	require.NoError(t, err)
	blob, err := cryptobox.WrapHK(hk, pub)
	require.NoError(t, err)
	err = c.ActivateDevice(ctx, id, wire.ActivateReq{Wrap: wire.HKWrap{DeviceID: id, Blob: blob, HKVersion: 1}})
	require.NoError(t, err, "enroll activate")
	return c
}

// newServer spins up a real sync server behind httptest and returns its base
// URL. The server and its temp DB are torn down at test end.
func newServer(t *testing.T) string {
	t.Helper()
	srv, err := server.New(server.Options{
		DBPath: filepath.Join(t.TempDir(), "sync.db"),
		Token:  testToken,
	})
	require.NoError(t, err, "server.New")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Close() })
	return ts.URL
}

func TestHTTPClientHealth(t *testing.T) {
	c := NewHTTPClient(newServer(t), "")
	require.NoError(t, c.Health(context.Background()), "Health")
}

// TestHTTPClientUnknownDeviceIsAPIError pins the new auth model: a client whose
// device the server does not know cannot read anything, and says so as a typed
// 401. There is no token to get wrong — the device key IS the credential.
func TestHTTPClientUnknownDeviceIsAPIError(t *testing.T) {
	ctx := context.Background()
	c := NewHTTPClient(newServer(t), "")
	dk, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)
	c.SetSigner(rec.NewID(), dk.Sign)

	// Health is open: it still succeeds.
	require.NoError(t, c.Health(ctx), "Health needs no credential")

	// An authenticated call must surface a typed 401.
	_, err = c.Hosts(ctx)
	require.Error(t, err, "Hosts as an unknown device: want error, got nil")
	var ae *APIError
	require.ErrorAs(t, err, &ae, "want *APIError")
	require.Equal(t, http.StatusUnauthorized, ae.Status, "want status 401")
}

func TestHTTPClientDeviceLifecycle(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)

	// A self-signed registration (device stays pending until approved).
	dk, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)
	id := rec.NewID()
	c := NewHTTPClient(url, "")
	c.SetSigner(id, dk.Sign)
	pub := dk.Public()
	req := wire.RegisterReq{ID: id, Name: "laptop", PubKey: pub[:], SignKey: dk.SignPublic()}

	resp, err := c.RegisterDevice(ctx, req, testToken)
	require.NoError(t, err, "RegisterDevice")
	require.Equal(t, wire.DevicePending, resp.Device.Status)
	require.False(t, resp.GroupFormed, "an empty group is not yet formed")

	// Duplicate register is a typed 409. It needs its own token: the first
	// enrollment consumed the bootstrap allowance only once a device is active,
	// so reuse of the server token is what is being exercised here.
	_, err = c.RegisterDevice(ctx, req, testToken)
	var ae *APIError
	require.ErrorAs(t, err, &ae, "duplicate register: want APIError")
	require.Equal(t, http.StatusConflict, ae.Status, "duplicate register: want 409")

	// A PENDING device cannot read: with the token gone, authentication requires
	// an ACTIVE device record, and this one is still awaiting approval.
	_, err = c.ListDevices(ctx)
	require.ErrorAs(t, err, &ae, "pending ListDevices: want APIError")
	require.Equal(t, http.StatusUnauthorized, ae.Status, "a pending device must not be able to read")

	// From an active device the registration is visible, and the pending device
	// has no HK wrap yet: found=false, no error.
	active := enroll(t, url)
	devs, err := active.ListDevices(ctx)
	require.NoError(t, err, "ListDevices as an active device")
	ids := make([]string, 0, len(devs))
	for _, d := range devs {
		ids = append(ids, d.ID)
	}
	require.Contains(t, ids, id, "the pending registration should be listed")

	_, found, err := active.GetHKWrap(ctx, id)
	require.NoError(t, err, "GetHKWrap")
	require.False(t, found, "GetHKWrap: want found=false before activation")
}

func TestHTTPClientPushPull(t *testing.T) {
	ctx := context.Background()
	c := enroll(t, newServer(t)) // push requires an active, signing device

	push, err := c.PushRecords(ctx, wire.PushReq{
		HostID: "host-A",
		Records: []wire.PushRecord{
			{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("blob-1")},
			{Seq: 2, ID: "r2", KeyID: "k1", Blob: []byte("blob-2")},
		},
	})
	require.NoError(t, err, "PushRecords")
	require.Equal(t, 2, push.Stored)
	require.Equal(t, uint64(2), push.MaxSeq)

	// Idempotent re-push stores nothing.
	push2, err := c.PushRecords(ctx, wire.PushReq{
		HostID:  "host-A",
		Records: []wire.PushRecord{{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("blob-1")}},
	})
	require.NoError(t, err, "PushRecords (re)")
	require.Equal(t, 0, push2.Stored, "re-push should store 0")

	pull, err := c.PullRecords(ctx, "host-A", 0, 100)
	require.NoError(t, err, "PullRecords")
	require.Len(t, pull.Records, 2)
	require.Equal(t, "blob-1", string(pull.Records[0].Blob))

	// Hosts sees the stream.
	hosts, err := c.Hosts(ctx)
	require.NoError(t, err, "Hosts")
	require.Len(t, hosts, 1)
	require.Equal(t, "host-A", hosts[0].HostID)
	require.Equal(t, uint64(2), hosts[0].MaxSeq)
}

func TestVerificationCodeStableAndDistinct(t *testing.T) {
	var a, b [32]byte
	a[0] = 1
	b[0] = 2
	require.Equal(t, VerificationCode(a), VerificationCode(a), "VerificationCode not stable for the same key")
	require.NotEqual(t, VerificationCode(a), VerificationCode(b), "VerificationCode collided for distinct keys")
	// Format: 6 groups of 4 => 6*4 + 5 separators = 29 chars.
	require.Len(t, VerificationCode(a), 29, "code length")
}
