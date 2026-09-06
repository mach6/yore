package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/rstore"
	"yore/internal/secret"
	"yore/internal/store"
	"yore/internal/syncer"
	"yore/internal/wire"
)

// seedCache writes one host's ciphertext into the on-disk cache so a test can
// prove it is gone afterwards.
func seedCache(t *testing.T, dir string) {
	t.Helper()
	rs, err := rstore.Open(dir)
	require.NoError(t, err)
	_, err = rs.Append("hostA", []wire.PullRecord{
		{ID: "r1", Seq: 1, KeyID: "k1", Blob: []byte("ciphertext")},
	}, 1, 0)
	require.NoError(t, err)
	require.NoError(t, rs.Close())
	require.FileExists(t, rstore.Path(dir), "cache should exist before the revocation")
}

// TestRevokedPurgesCiphertextNow is the security guarantee: the moment the
// server says this device is revoked, the group's ciphertext leaves the disk.
// The already-decrypted history in RAM is deliberately left alone: the user is
// looking at it, and it is gone at the next start either way.
func TestRevokedPurgesCiphertextNow(t *testing.T) {
	dir := t.TempDir()
	seedCache(t, dir)

	st := openStore(t, dir)
	rs, err := rstore.Open(dir)
	require.NoError(t, err)
	rc := &remoteCache{
		state:   proto.RemoteOK,
		cursors: map[string]uint64{"hostA": 1},
		records: []rec.Record{{ID: "r1", Cmd: "echo hi", Hostname: "other"}},
		rs:      rs,
		dir:     dir,
		st:      st,
		tags:    newTagIndex(),
		prompts: newPromptIndex(),
	}

	rc.onRevoked()

	require.Equal(t, proto.RemoteRevoked, rc.info().State)
	require.True(t, rc.revoked())
	require.NoFileExists(t, rstore.Path(dir), "the cached ciphertext must be deleted at once")
	require.True(t, wasRevoked(st), "the revocation must outlive this process")
	require.True(t, rc.latched(), "this process must stop retrying")
	require.Nil(t, rc.rs, "the cache must be detached so nothing writes it back")
	require.Empty(t, rc.cursors, "cursors describe streams this device may no longer read")
	require.Len(t, rc.records, 1, "history already decrypted into RAM is left for this session")
}

// TestRevokedRecordPurgesOnStart is the backstop for a revocation that could not
// finish: the process died between recording and deleting, or the cache came back
// from a backup. A start reads the record and deletes the cache without needing
// the server: which matters most on a machine that comes back up offline.
func TestRevokedRecordPurgesOnStart(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, config.EnsureDir(dir))
	seedCache(t, dir)

	st := openStore(t, dir)
	setRevokedMeta(st, true)
	key, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)
	require.NoError(t, secret.Open(dir).SaveDeviceKey(key))
	sc := syncConf{url: "https://example.invalid", epoch: time.Hour}

	rc := newRemote(dir, st, sc, newTagIndex(), newPromptIndex())
	t.Cleanup(rc.close)

	require.Equal(t, proto.RemoteRevoked, rc.info().State, "a recorded revocation must start revoked")
	require.False(t, rc.latched(),
		"a revoked start must still be allowed one attempt, or a re-enrolled device could never come back")

	// The cache file is reopened (empty) so a device that turns out to be
	// re-enrolled has somewhere to cache its re-pull, but the ciphertext the
	// previous run left behind is gone.
	require.NotNil(t, rc.rs)
	hosts, err := rc.rs.Hosts()
	require.NoError(t, err)
	require.Empty(t, hosts, "the leftover ciphertext must be deleted at startup")
	cursors, err := rc.rs.Cursors()
	require.NoError(t, err)
	require.Empty(t, cursors, "the leftover cursors must go with it")
}

// TestSyncOnceRefusesWhenRevoked proves the terminal state stops the cycle
// without touching the network: retrying a revocation can never succeed.
func TestSyncOnceRefusesWhenRevoked(t *testing.T) {
	dir := t.TempDir()
	rc := &remoteCache{
		state:        proto.RemoteRevoked,
		revokedLatch: true,
		cursors:      map[string]uint64{},
		dir:          dir,
		st:           openStore(t, dir),
		sy:           newTestSyncer(t),
		tags:         newTagIndex(),
		prompts:      newPromptIndex(),
	}
	err := rc.syncOnce(t.Context(), 0)
	require.ErrorIs(t, err, errRevoked)
}

// TestAttachForgetsRevocation covers a device that legitimately moves to another
// server: the old group's revocation says nothing about the new one, and left in
// place it would delete the new cache on every start.
func TestAttachForgetsRevocation(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	setRevokedMeta(st, true)
	rc := &remoteCache{
		state: proto.RemoteRevoked, revokedLatch: true,
		cursors: map[string]uint64{}, dir: dir, st: st,
	}

	rc.attach(newTestSyncer(t), 0, false)

	require.Equal(t, proto.RemoteUnavailable, rc.info().State)
	require.False(t, rc.revoked())
	require.False(t, rc.latched())
	require.False(t, wasRevoked(st), "pointing at a new server must clear the old revocation")
	require.NotNil(t, rc.rs, "the new server's cache should be opened")
	t.Cleanup(rc.close)
}

// TestAttachKeepsRevocationOnSameServer is the other half: editing the transport
// (a certificate pin) does not move this device to another group, so the
// revocation stands. Forgetting it here would re-open the cache the revocation
// deleted and start pulling the group's ciphertext back onto the disk.
func TestAttachKeepsRevocationOnSameServer(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	setRevokedMeta(st, true)
	rc := &remoteCache{
		state: proto.RemoteRevoked, revokedLatch: true,
		cursors: map[string]uint64{}, dir: dir, st: st,
	}

	rc.attach(newTestSyncer(t), 0, true)

	require.Equal(t, proto.RemoteRevoked, rc.info().State)
	require.True(t, rc.revoked())
	require.True(t, rc.latched())
	require.True(t, wasRevoked(st), "the same server's revocation must stand")
	require.Nil(t, rc.rs, "no cache may be re-opened for a revoked device")
	t.Cleanup(rc.close)
}

// TestSyncOnceReactsToRevokedResponse closes the loop end to end: a server that
// answers with the revoked code drives a real sync cycle into the terminal
// state, marker written and cached ciphertext deleted. Everything between the
// HTTP response and the purge is under test here.
func TestSyncOnceReactsToRevokedResponse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, config.EnsureDir(dir))
	seedCache(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(wire.ErrorResp{
			Error: "device revoked", Code: wire.CodeDeviceRevoked,
		})
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.AppendBatch([]rec.Record{{ID: "r1", Cmd: "echo hi", StartMs: 1}})
	require.NoError(t, err) // something to push, so the cycle reaches the network
	key, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)

	rs, err := rstore.Open(dir)
	require.NoError(t, err)
	rc := &remoteCache{
		state:   proto.RemoteOK,
		cursors: map[string]uint64{"hostA": 1},
		rs:      rs,
		dir:     dir,
		st:      st,
		sy:      syncer.New(st, syncer.NewHTTPClient(srv.URL, ""), key, time.Hour),
		tags:    newTagIndex(),
		prompts: newPromptIndex(),
	}

	err = rc.syncOnce(t.Context(), 0)
	require.ErrorIs(t, err, errRevoked, "a revoked response must end the cycle as revoked")
	require.Equal(t, proto.RemoteRevoked, rc.info().State)
	require.NoFileExists(t, rstore.Path(dir), "the cached ciphertext must be gone")
	require.True(t, wasRevoked(st), "the next start must know")

	// And it stays terminal: no further cycle touches the network.
	require.ErrorIs(t, rc.syncOnce(t.Context(), 0), errRevoked)
}

// TestRevokedClearsWhenServerServesAgain is the way back. A recorded revocation
// is the last thing the server said, not a permanent verdict: enrolling the
// machine again has to retire it, and nothing in the enrollment path can reach
// into data.db (the daemon holds the write lock). So the evidence that clears it
// is the server serving this device once more.
func TestRevokedClearsWhenServerServesAgain(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, config.EnsureDir(dir))

	st := openStore(t, dir)
	setRevokedMeta(st, true) // the previous run was refused
	key, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err)

	// A server that serves this device again: it hands out a History Key wrapped
	// to this device's public key, and has no other host's stream to offer.
	hk, err := cryptobox.NewHistoryKey()
	require.NoError(t, err)
	blob, err := cryptobox.WrapHK(hk, key.Public())
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/keys/hk":
			_ = json.NewEncoder(w).Encode(wire.HKWrap{Blob: blob, HKVersion: 1})
		case "/v1/keys/dek":
			_ = json.NewEncoder(w).Encode(wire.DEKListResp{})
		default: // /v1/hosts and anything else the cycle touches
			_ = json.NewEncoder(w).Encode(wire.HostsResp{})
		}
	}))
	t.Cleanup(srv.Close)

	rc := &remoteCache{
		state:   proto.RemoteRevoked, // last known standing; NOT latched
		cursors: map[string]uint64{},
		dir:     dir,
		st:      st,
		sy:      syncer.New(st, syncer.NewHTTPClient(srv.URL, ""), key, time.Hour),
		tags:    newTagIndex(),
		prompts: newPromptIndex(),
	}

	require.NoError(t, rc.syncOnce(t.Context(), 0), "a re-admitted device must be able to sync")
	require.Equal(t, proto.RemoteOK, rc.info().State)
	require.False(t, wasRevoked(st), "a successful cycle must retire the recorded revocation")
	require.False(t, rc.revoked())
}

// TestRevokedMetaRoundTrip pins where the revocation lives: data.db's meta
// bucket, which the daemon holds under its write lock; not a loose file in the
// state directory that could simply be deleted.
func TestRevokedMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	require.False(t, wasRevoked(st), "a fresh store is not revoked")

	setRevokedMeta(st, true)
	require.True(t, wasRevoked(st))
	require.NoFileExists(t, filepath.Join(dir, "revoked"), "the record must not be a standalone file")

	setRevokedMeta(st, false)
	require.False(t, wasRevoked(st))

	require.False(t, wasRevoked(nil), "a nil store must not panic")
	setRevokedMeta(nil, true)
}

// openStore opens a local store in dir for the duration of the test.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}
