package syncer

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"yore/internal/cryptobox"
	"yore/internal/rec"
	"yore/internal/store"
	"yore/internal/wire"
)

const testEpoch = time.Hour

// newDevice builds a fully independent machine: its own local store (hence its
// own hostID) and its own device key, talking to the shared server at url.
func newDevice(t *testing.T, url string) (*Syncer, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })
	key, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err, "GenerateDeviceKey")
	return New(st, NewHTTPClient(url, ""), key, testEpoch), st
}

// requireBootstrap forms the group from s: register with the server's own token
// (allowed only while no device is active), then bootstrap with a fresh
// recovery key.
func requireBootstrap(t *testing.T, s *Syncer, name string) {
	t.Helper()
	ctx := context.Background()
	_, _, err := s.Enroll(ctx, name, testToken)
	require.NoError(t, err, "%s enroll", name)
	salt, err := cryptobox.NewRecoverySalt()
	require.NoError(t, err, "NewRecoverySalt")
	phrase, err := cryptobox.NewRecoveryPhrase()
	require.NoError(t, err, "NewRecoveryPhrase")
	rk, err := cryptobox.DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "DeriveRecoveryKey")
	require.NoError(t, s.Bootstrap(ctx, rk, salt), "%s bootstrap", name)
}

// enrollPair returns two enrolled machines sharing one server: A has
// bootstrapped the group and approved B, so both hold the same HK.
func enrollPair(t *testing.T, url string) (a *Syncer, aStore *store.Store, b *Syncer, bStore *store.Store) {
	t.Helper()
	ctx := context.Background()
	a, aStore = newDevice(t, url)
	b, bStore = newDevice(t, url)

	requireBootstrap(t, a, "machine-A")
	token, err := a.MintToken(ctx)
	require.NoError(t, err, "A.MintToken")
	_, _, err = b.Enroll(ctx, "machine-B", token.Token)
	require.NoError(t, err, "B.Enroll")
	pending, err := a.PendingDevices(ctx)
	require.NoError(t, err, "A.PendingDevices")
	found := false
	for _, d := range pending {
		if d.ID == b.DeviceID() {
			found = true
		}
	}
	require.Truef(t, found, "A did not see B pending; pending=%+v", pending)
	require.NoError(t, a.Approve(ctx, b.DeviceID()), "A.Approve(B)")
	return a, aStore, b, bStore
}

// makeRecords builds n records whose StartMs spans about three epoch widths, so
// they land in at least two DEK epochs. Exit and DurMs are set on some records
// and nil on others to exercise pointer-fidelity across the round trip.
func makeRecords(n int) []rec.Record {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	span := int64(3 * testEpoch / time.Millisecond)
	step := span / int64(n)
	if step < 1 {
		step = 1
	}
	out := make([]rec.Record, n)
	for i := range out {
		r := rec.Record{
			ID:      rec.NewID(),
			Session: fmt.Sprintf("sess-%d", i%4),
			Cmd:     fmt.Sprintf("echo command number %d", i),
			Cwd:     fmt.Sprintf("/home/u/proj/%d", i%9),
			StartMs: base + int64(i)*step,
		}
		if i%2 == 0 {
			r.Exit = rec.IntPtr(i % 3)
		}
		if i%3 == 0 {
			r.DurMs = rec.Int64Ptr(int64(i) * 10)
		}
		out[i] = r
	}
	return out
}

// canonicalByID reads a store's full raw stream and indexes it by record ID.
func canonicalByID(t *testing.T, st *store.Store) map[string]rec.Record {
	t.Helper()
	all, err := st.Since(0, 0)
	require.NoError(t, err, "store.Since")
	m := make(map[string]rec.Record, len(all))
	for _, r := range all {
		m[r.ID] = r
	}
	return m
}

// requireSameRecord asserts every synced field matches, treating Exit and DurMs
// pointers by their nil-ness and value. KeyID (transit-only) and DeletedMs
// (local-only) are intentionally not compared. require.Equal compares the two
// pointers via deep equality, so it matches on both nil-ness and pointee value.
func requireSameRecord(t *testing.T, want, got rec.Record) {
	t.Helper()
	require.Equal(t, want.ID, got.ID, "id mismatch")
	require.Equal(t, want.Type, got.Type, "type mismatch")
	require.Equal(t, want.TargetID, got.TargetID, "target_id mismatch")
	require.Equal(t, want.HostID, got.HostID, "host_id mismatch")
	require.Equal(t, want.Hostname, got.Hostname, "hostname mismatch")
	require.Equal(t, want.Seq, got.Seq, "seq mismatch")
	require.Equal(t, want.Session, got.Session, "session mismatch")
	require.Equal(t, want.Cmd, got.Cmd, "cmd mismatch")
	require.Equal(t, want.Cwd, got.Cwd, "cwd mismatch")
	require.Equal(t, want.StartMs, got.StartMs, "start_ms mismatch")
	require.Equal(t, want.Exit, got.Exit, "exit mismatch")
	require.Equal(t, want.DurMs, got.DurMs, "dur_ms mismatch")
}

// TestIntegration is the milestone gate: two machines with distinct keys and
// hostIDs converge through the real server.
func TestIntegration(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)

	// --- Steps 1-2: enrollment. ---
	a, aStore, b, _ := enrollPair(t, url)

	require.NotEqual(t, a.DeviceID(), b.DeviceID(), "devices must have distinct IDs")

	// Step 1: A is active and its HK resolves.
	devs, err := a.http.ListDevices(ctx)
	require.NoError(t, err, "ListDevices")
	for _, d := range devs {
		if d.ID == a.DeviceID() {
			require.Equal(t, wire.DeviceActive, d.Status, "A should be active")
		}
	}
	hkA, verA, err := a.resolveHK(ctx)
	require.NoError(t, err, "A.resolveHK")

	// Step 2: B resolves the SAME HK after approval.
	hkB, verB, err := b.resolveHK(ctx)
	require.NoError(t, err, "B.resolveHK")
	require.Equal(t, hkA, hkB, "B's HK does not equal A's HK")
	require.Equal(t, verB, verA, "A and B HK versions differ")
	require.Equal(t, bootstrapHKVersion, verA, "HK version")

	// --- Step 3: A stores ~2500 records across >1 epoch and pushes. ---
	const n = 2500
	_, err = aStore.AppendBatch(makeRecords(n))
	require.NoError(t, err, "AppendBatch")
	pushed, err := a.Push(ctx)
	require.NoError(t, err, "A.Push")
	require.Equal(t, n, pushed, "A.Push uploaded")

	// At least two DEK epochs were created.
	dekList, err := a.http.ListDEKWraps(ctx, "", 1000)
	require.NoError(t, err, "ListDEKWraps")
	require.GreaterOrEqual(t, len(dekList.Wraps), 2, "want >= 2 DEK epochs")

	// A re-push uploads nothing (watermark persisted).
	again, err := a.Push(ctx)
	require.NoError(t, err, "A.Push (repeat)")
	require.Zero(t, again, "A.Push (repeat) should upload nothing")

	// --- Step 4: B pulls all of A's records, decrypted, fields intact. ---
	want := canonicalByID(t, aStore)
	recs, cursors, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err, "B.PullOthers")
	require.Len(t, recs, n, "B pulled records")
	seen := make(map[string]bool, n)
	for _, got := range recs {
		w, ok := want[got.ID]
		require.Truef(t, ok, "B pulled unknown record %s", got.ID)
		requireSameRecord(t, w, got)
		seen[got.ID] = true
	}
	require.Len(t, seen, n, "B pulled distinct records")
	require.NotZero(t, cursors[a.DeviceID()], "cursor for A did not advance")

	// Second pull with advanced cursors returns nothing new.
	recs2, cursors2, err := b.PullOthers(ctx, cursors)
	require.NoError(t, err, "B.PullOthers (2nd)")
	require.Empty(t, recs2, "2nd pull returned records")
	require.Equal(t, cursors[a.DeviceID()], cursors2[a.DeviceID()], "cursor moved on empty pull")

	// --- Step 5: tombstone replays to B. ---
	targetID := recs[0].ID
	_, err = aStore.Append(rec.Record{
		Type:     rec.TypeDelete,
		TargetID: targetID,
		StartMs:  time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC).UnixMilli(),
	})
	require.NoError(t, err, "append tombstone")
	pushed, err = a.Push(ctx)
	require.NoError(t, err, "A.Push tombstone")
	require.Equal(t, 1, pushed, "A.Push tombstone")
	recs3, _, err := b.PullOthers(ctx, cursors2)
	require.NoError(t, err, "B.PullOthers (tombstone)")
	require.Len(t, recs3, 1, "tombstone pull")
	tomb := recs3[0]
	require.Equalf(t, rec.TypeDelete, tomb.Type, "B did not receive a usable delete tombstone: %+v", tomb)
	require.Equalf(t, targetID, tomb.TargetID, "B did not receive a usable delete tombstone: %+v", tomb)
}

// TestPayloadRoundTrip focuses on field fidelity through seal->push->pull->open,
// especially nil-vs-set pointers and the encrypted hostname.
func TestPayloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	inputs := []rec.Record{
		{ID: rec.NewID(), Cmd: "ls -la", Cwd: "/tmp", Session: "s1", StartMs: 1_700_000_000_000, Exit: rec.IntPtr(0), DurMs: rec.Int64Ptr(1234)},
		{ID: rec.NewID(), Cmd: "vim", Cwd: "/home", Session: "s2", StartMs: 1_700_000_100_000},                               // Exit & DurMs nil
		{ID: rec.NewID(), Cmd: "grep x", Cwd: "/var", Session: "s3", StartMs: 1_700_000_200_000, Exit: rec.IntPtr(2)},        // DurMs nil
		{ID: rec.NewID(), Cmd: "sleep 1", Cwd: "/etc", Session: "s4", StartMs: 1_700_000_300_000, DurMs: rec.Int64Ptr(1000)}, // Exit nil
	}
	_, err := aStore.AppendBatch(inputs)
	require.NoError(t, err, "AppendBatch")
	_, err = a.Push(ctx)
	require.NoError(t, err, "A.Push")

	want := canonicalByID(t, aStore)
	recs, _, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err, "B.PullOthers")
	require.Len(t, recs, len(inputs), "pulled count")
	for _, got := range recs {
		requireSameRecord(t, want[got.ID], got)
		require.Equal(t, aStore.Hostname(), got.Hostname, "hostname not preserved")
	}
}

// TestPullDecryptionFatal proves a record that fails authentication makes
// PullOthers return an error rather than silently skipping it. We upload a
// legitimate DEK wrap, then a record that claims that keyID but was sealed with
// a different key.
func TestPullDecryptionFatal(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	// A pushes one honest record so a real DEK wrap exists on the server.
	_, err := aStore.Append(rec.Record{Cmd: "echo hi", StartMs: 1_700_000_000_000})
	require.NoError(t, err, "append")
	_, err = a.Push(ctx)
	require.NoError(t, err, "A.Push")
	dekList, err := a.http.ListDEKWraps(ctx, "", 10)
	require.NoError(t, err, "ListDEKWraps")
	require.NotEmpty(t, dekList.Wraps, "ListDEKWraps returned no wraps")
	keyID := dekList.Wraps[0].KeyID

	// Craft a tampered record: correct keyID and AAD, but sealed with a random
	// key the DEK wrap does not correspond to.
	var wrongDEK [32]byte
	_, err = rand.Read(wrongDEK[:])
	require.NoError(t, err, "rand")
	tamperSeq := uint64(1_000_000)
	tamperID := rec.NewID()
	pt, err := marshalPayload(rec.Record{ID: tamperID, HostID: aStore.HostID(), Cmd: "evil", StartMs: 1_700_000_000_000})
	require.NoError(t, err, "marshalPayload")
	aad := cryptobox.RecordAAD{RecordID: tamperID, HostID: aStore.HostID(), Seq: tamperSeq, KeyID: keyID}
	blob, err := cryptobox.SealRecord(pt, wrongDEK, aad)
	require.NoError(t, err, "SealRecord")
	_, err = a.http.PushRecords(ctx, wire.PushReq{
		HostID:  aStore.HostID(),
		Records: []wire.PushRecord{{Seq: tamperSeq, ID: tamperID, KeyID: keyID, Blob: blob}},
	})
	require.NoError(t, err, "push tampered")

	// B must FAIL the pull, not skip.
	_, _, err = b.PullOthers(ctx, map[string]uint64{})
	require.Error(t, err, "PullOthers: want fatal decryption error, got nil")
}

// TestRevokeRotation proves O(1) revocation: after A revokes B and rotates the
// group key, a brand-new device C (enrolled under the NEW HK) can still decrypt
// A's records written BEFORE the rotation. Records are never re-encrypted; only
// keys rotate.
func TestRevokeRotation(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	// A writes and pushes records under HK version 1.
	const n = 1200 // spans >1 push batch and >1 epoch
	_, err := aStore.AppendBatch(makeRecords(n))
	require.NoError(t, err, "AppendBatch")
	pushed, err := a.Push(ctx)
	require.NoError(t, err, "A.Push")
	require.Equal(t, n, pushed, "A.Push")
	want := canonicalByID(t, aStore)

	// A revokes B and rotates to HK version 2.
	require.NoError(t, a.Revoke(ctx, b.DeviceID()), "A.Revoke(B)")

	// B's device is revoked and has no HK wrap anymore.
	devs, err := a.http.ListDevices(ctx)
	require.NoError(t, err, "ListDevices")
	for _, d := range devs {
		if d.ID == b.DeviceID() {
			require.Equal(t, wire.DeviceRevoked, d.Status, "B should be revoked")
		}
	}
	// A revoked device cannot authenticate at all any more: its key is the only
	// credential and the server no longer honours it. (It also has no HK wrap,
	// but it can no longer get far enough to learn that.)
	_, _, err = b.http.GetHKWrap(ctx, b.DeviceID())
	var revokedErr *APIError
	require.ErrorAs(t, err, &revokedErr, "B GetHKWrap after revoke: want APIError")
	require.Equal(t, http.StatusUnauthorized, revokedErr.Status, "a revoked device must be refused")

	// A is now on HK version 2.
	_, verA, err := a.resolveHK(ctx)
	require.NoError(t, err, "A.resolveHK after rotate")
	require.Equal(t, bootstrapHKVersion+1, verA, "A HK version")

	// Enroll a fresh device C AFTER the rotation. It receives HK2.
	c, _ := newDevice(t, url)
	tokenC, err := a.MintToken(ctx)
	require.NoError(t, err, "A.MintToken for C")
	_, _, err = c.Enroll(ctx, "machine-C", tokenC.Token)
	require.NoError(t, err, "C.Enroll")
	require.NoError(t, a.Approve(ctx, c.DeviceID()), "A.Approve(C)")
	hkC, verC, err := c.resolveHK(ctx)
	require.NoError(t, err, "C.resolveHK")
	require.Equal(t, bootstrapHKVersion+1, verC, "C HK version")
	hkA2, _, _ := a.resolveHK(ctx)
	require.Equal(t, hkA2, hkC, "C's HK does not equal A's rotated HK")

	// C decrypts every pre-rotation record — proving old records stayed
	// decryptable through the rotation without being re-encrypted.
	recs, _, err := c.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err, "C.PullOthers")
	require.Len(t, recs, n, "C pulled records")
	for _, got := range recs {
		requireSameRecord(t, want[got.ID], got)
	}
}

// TestRecoverAfterLosingEveryDevice is the guarantee recovery exists for: with
// every enrolled machine gone, the recovery phrase alone must bring the history
// back. It is the difference between "lost a laptop" and "lost the archive".
func TestRecoverAfterLosingEveryDevice(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)

	// Machine A forms the group with a known recovery phrase and pushes history.
	a, aStore := newDevice(t, url)
	_, _, err := a.Enroll(ctx, "machine-A", testToken)
	require.NoError(t, err, "A.Enroll")
	salt, err := cryptobox.NewRecoverySalt()
	require.NoError(t, err, "NewRecoverySalt")
	phrase, err := cryptobox.NewRecoveryPhrase()
	require.NoError(t, err, "NewRecoveryPhrase")
	rk, err := cryptobox.DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "DeriveRecoveryKey")
	require.NoError(t, a.Bootstrap(ctx, rk, salt), "A.Bootstrap")

	for _, r := range makeRecords(12) {
		_, aerr := aStore.Append(r)
		require.NoError(t, aerr, "A append")
	}
	_, err = a.Push(ctx)
	require.NoError(t, err, "A.Push")
	want := canonicalByID(t, aStore)

	// A is gone: a brand-new machine holds nothing but the phrase. Note that A is
	// still ACTIVE server-side — that is exactly the real situation — so the
	// bootstrap allowance does not apply and nothing can vouch for B.
	b, _ := newDevice(t, url)
	rc := NewHTTPClient(url, "")
	hk, hkVer, err := RecoverHK(ctx, rc, phrase)
	require.NoError(t, err, "RecoverHK")

	// The recovery key authorizes B's enrollment token, then B admits itself
	// with the recovered History Key.
	tkt, err := rc.RecoveryToken(ctx)
	require.NoError(t, err, "RecoveryToken")
	_, _, err = b.Enroll(ctx, "machine-B", tkt.Token)
	require.NoError(t, err, "B.Enroll")
	bPub := b.dev.Public()
	blob, err := cryptobox.WrapHK(hk, bPub)
	require.NoError(t, err, "wrap HK for B")
	require.NoError(t, rc.RecoveryActivate(ctx, b.DeviceID(), wire.ActivateReq{Wrap: wire.HKWrap{
		DeviceID: b.DeviceID(), Blob: blob, HKVersion: hkVer,
	}}), "recovery activate B")

	// B now reads everything A ever wrote.
	got, _, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err, "B.PullOthers")
	require.Len(t, got, len(want), "recovered record count")
	for _, g := range got {
		requireSameRecord(t, want[g.ID], g)
	}
}

// TestRecoverWrongPhraseFails pins that recovery is not a bypass: the wrap only
// opens for the exact phrase, and a wrong one cannot reach the History Key.
func TestRecoverWrongPhraseFails(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)

	a, _ := newDevice(t, url)
	_, _, err := a.Enroll(ctx, "machine-A", testToken)
	require.NoError(t, err, "A.Enroll")
	salt, err := cryptobox.NewRecoverySalt()
	require.NoError(t, err, "salt")
	phrase, err := cryptobox.NewRecoveryPhrase()
	require.NoError(t, err, "phrase")
	rk, err := cryptobox.DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "derive")
	require.NoError(t, a.Bootstrap(ctx, rk, salt), "A.Bootstrap")

	wrong, err := cryptobox.NewRecoveryPhrase()
	require.NoError(t, err, "wrong phrase")
	_, _, err = RecoverHK(ctx, NewHTTPClient(url, ""), wrong)
	require.Error(t, err, "a wrong recovery phrase must not recover the History Key")
}
