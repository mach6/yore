package syncer

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := cryptobox.GenerateDeviceKey()
	if err != nil {
		t.Fatalf("GenerateDeviceKey: %v", err)
	}
	return New(st, NewHTTPClient(url, testToken), key, testEpoch), st
}

// enrollPair returns two enrolled machines sharing one server: A has
// bootstrapped the group and approved B, so both hold the same HK.
func enrollPair(t *testing.T, url string) (a *Syncer, aStore *store.Store, b *Syncer, bStore *store.Store) {
	t.Helper()
	ctx := context.Background()
	a, aStore = newDevice(t, url)
	b, bStore = newDevice(t, url)

	if err := a.Bootstrap(ctx, "machine-A"); err != nil {
		t.Fatalf("A.Bootstrap: %v", err)
	}
	if _, err := b.Register(ctx, "machine-B"); err != nil {
		t.Fatalf("B.Register: %v", err)
	}
	pending, err := a.PendingDevices(ctx)
	if err != nil {
		t.Fatalf("A.PendingDevices: %v", err)
	}
	found := false
	for _, d := range pending {
		if d.ID == b.DeviceID() {
			found = true
		}
	}
	if !found {
		t.Fatalf("A did not see B pending; pending=%+v", pending)
	}
	if err := a.Approve(ctx, b.DeviceID()); err != nil {
		t.Fatalf("A.Approve(B): %v", err)
	}
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
	if err != nil {
		t.Fatalf("store.Since: %v", err)
	}
	m := make(map[string]rec.Record, len(all))
	for _, r := range all {
		m[r.ID] = r
	}
	return m
}

// requireSameRecord asserts every synced field matches, treating Exit and DurMs
// pointers by their nil-ness and value. KeyID (transit-only) and DeletedMs
// (local-only) are intentionally not compared.
func requireSameRecord(t *testing.T, want, got rec.Record) {
	t.Helper()
	if want.ID != got.ID || want.Type != got.Type || want.TargetID != got.TargetID {
		t.Fatalf("identity mismatch: want %+v got %+v", want, got)
	}
	if want.HostID != got.HostID || want.Hostname != got.Hostname || want.Seq != got.Seq {
		t.Fatalf("stream fields mismatch for %s: want host=%s/%s seq=%d got host=%s/%s seq=%d",
			want.ID, want.HostID, want.Hostname, want.Seq, got.HostID, got.Hostname, got.Seq)
	}
	if want.Session != got.Session || want.Cmd != got.Cmd || want.Cwd != got.Cwd || want.StartMs != got.StartMs {
		t.Fatalf("content mismatch for %s: want %+v got %+v", want.ID, want, got)
	}
	if !samePtrInt(want.Exit, got.Exit) {
		t.Fatalf("exit mismatch for %s: want %v got %v", want.ID, deref(want.Exit), deref(got.Exit))
	}
	if !samePtrInt64(want.DurMs, got.DurMs) {
		t.Fatalf("dur_ms mismatch for %s: want %v got %v", want.ID, deref64(want.DurMs), deref64(got.DurMs))
	}
}

func samePtrInt(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func samePtrInt64(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
func deref64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestIntegration is the milestone gate: two machines with distinct keys and
// hostIDs converge through the real server.
func TestIntegration(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)

	// --- Steps 1-2: enrollment. ---
	a, aStore, b, _ := enrollPair(t, url)

	if a.DeviceID() == b.DeviceID() {
		t.Fatal("devices must have distinct IDs")
	}

	// Step 1: A is active and its HK resolves.
	devs, err := a.http.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	for _, d := range devs {
		if d.ID == a.DeviceID() && d.Status != wire.DeviceActive {
			t.Fatalf("A should be active, got %q", d.Status)
		}
	}
	hkA, verA, err := a.resolveHK(ctx)
	if err != nil {
		t.Fatalf("A.resolveHK: %v", err)
	}

	// Step 2: B resolves the SAME HK after approval.
	hkB, verB, err := b.resolveHK(ctx)
	if err != nil {
		t.Fatalf("B.resolveHK: %v", err)
	}
	if hkA != hkB {
		t.Fatal("B's HK does not equal A's HK")
	}
	if verA != verB || verA != bootstrapHKVersion {
		t.Fatalf("HK versions: A=%d B=%d want %d", verA, verB, bootstrapHKVersion)
	}

	// --- Step 3: A stores ~2500 records across >1 epoch and pushes. ---
	const n = 2500
	if _, err := aStore.AppendBatch(makeRecords(n)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	pushed, err := a.Push(ctx)
	if err != nil {
		t.Fatalf("A.Push: %v", err)
	}
	if pushed != n {
		t.Fatalf("A.Push uploaded %d, want %d", pushed, n)
	}

	// At least two DEK epochs were created.
	dekList, err := a.http.ListDEKWraps(ctx, "", 1000)
	if err != nil {
		t.Fatalf("ListDEKWraps: %v", err)
	}
	if len(dekList.Wraps) < 2 {
		t.Fatalf("want >= 2 DEK epochs, got %d", len(dekList.Wraps))
	}

	// A re-push uploads nothing (watermark persisted).
	if again, err := a.Push(ctx); err != nil || again != 0 {
		t.Fatalf("A.Push (repeat): pushed=%d err=%v; want 0,nil", again, err)
	}

	// --- Step 4: B pulls all of A's records, decrypted, fields intact. ---
	want := canonicalByID(t, aStore)
	recs, cursors, err := b.PullOthers(ctx, map[string]uint64{})
	if err != nil {
		t.Fatalf("B.PullOthers: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("B pulled %d records, want %d", len(recs), n)
	}
	seen := make(map[string]bool, n)
	for _, got := range recs {
		w, ok := want[got.ID]
		if !ok {
			t.Fatalf("B pulled unknown record %s", got.ID)
		}
		requireSameRecord(t, w, got)
		seen[got.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("B pulled %d distinct records, want %d", len(seen), n)
	}
	if cursors[a.DeviceID()] == 0 {
		t.Fatal("cursor for A did not advance")
	}

	// Second pull with advanced cursors returns nothing new.
	recs2, cursors2, err := b.PullOthers(ctx, cursors)
	if err != nil {
		t.Fatalf("B.PullOthers (2nd): %v", err)
	}
	if len(recs2) != 0 {
		t.Fatalf("2nd pull returned %d records, want 0", len(recs2))
	}
	if cursors2[a.DeviceID()] != cursors[a.DeviceID()] {
		t.Fatalf("cursor moved on empty pull: %d -> %d", cursors[a.DeviceID()], cursors2[a.DeviceID()])
	}

	// --- Step 5: tombstone replays to B. ---
	targetID := recs[0].ID
	if _, err := aStore.Append(rec.Record{
		Type:     rec.TypeDelete,
		TargetID: targetID,
		StartMs:  time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC).UnixMilli(),
	}); err != nil {
		t.Fatalf("append tombstone: %v", err)
	}
	if pushed, err := a.Push(ctx); err != nil || pushed != 1 {
		t.Fatalf("A.Push tombstone: pushed=%d err=%v; want 1,nil", pushed, err)
	}
	recs3, _, err := b.PullOthers(ctx, cursors2)
	if err != nil {
		t.Fatalf("B.PullOthers (tombstone): %v", err)
	}
	if len(recs3) != 1 {
		t.Fatalf("tombstone pull returned %d records, want 1", len(recs3))
	}
	tomb := recs3[0]
	if tomb.Type != rec.TypeDelete || tomb.TargetID != targetID {
		t.Fatalf("B did not receive a usable delete tombstone: %+v", tomb)
	}
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
	if _, err := aStore.AppendBatch(inputs); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := a.Push(ctx); err != nil {
		t.Fatalf("A.Push: %v", err)
	}

	want := canonicalByID(t, aStore)
	recs, _, err := b.PullOthers(ctx, map[string]uint64{})
	if err != nil {
		t.Fatalf("B.PullOthers: %v", err)
	}
	if len(recs) != len(inputs) {
		t.Fatalf("pulled %d, want %d", len(recs), len(inputs))
	}
	for _, got := range recs {
		requireSameRecord(t, want[got.ID], got)
		if got.Hostname != aStore.Hostname() {
			t.Fatalf("hostname not preserved: got %q want %q", got.Hostname, aStore.Hostname())
		}
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
	if _, err := aStore.Append(rec.Record{Cmd: "echo hi", StartMs: 1_700_000_000_000}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := a.Push(ctx); err != nil {
		t.Fatalf("A.Push: %v", err)
	}
	dekList, err := a.http.ListDEKWraps(ctx, "", 10)
	if err != nil || len(dekList.Wraps) == 0 {
		t.Fatalf("ListDEKWraps: %v (%d)", err, len(dekList.Wraps))
	}
	keyID := dekList.Wraps[0].KeyID

	// Craft a tampered record: correct keyID and AAD, but sealed with a random
	// key the DEK wrap does not correspond to.
	var wrongDEK [32]byte
	if _, err := rand.Read(wrongDEK[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	tamperSeq := uint64(1_000_000)
	tamperID := rec.NewID()
	pt, err := marshalPayload(rec.Record{ID: tamperID, HostID: aStore.HostID(), Cmd: "evil", StartMs: 1_700_000_000_000})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	aad := cryptobox.RecordAAD{RecordID: tamperID, HostID: aStore.HostID(), Seq: tamperSeq, KeyID: keyID}
	blob, err := cryptobox.SealRecord(pt, wrongDEK, aad)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	if _, err := a.http.PushRecords(ctx, wire.PushReq{
		HostID:  aStore.HostID(),
		Records: []wire.PushRecord{{Seq: tamperSeq, ID: tamperID, KeyID: keyID, Blob: blob}},
	}); err != nil {
		t.Fatalf("push tampered: %v", err)
	}

	// B must FAIL the pull, not skip.
	_, _, err = b.PullOthers(ctx, map[string]uint64{})
	if err == nil {
		t.Fatal("PullOthers: want fatal decryption error, got nil")
	}
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
	if _, err := aStore.AppendBatch(makeRecords(n)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if pushed, err := a.Push(ctx); err != nil || pushed != n {
		t.Fatalf("A.Push: pushed=%d err=%v want %d", pushed, err, n)
	}
	want := canonicalByID(t, aStore)

	// A revokes B and rotates to HK version 2.
	if err := a.Revoke(ctx, b.DeviceID()); err != nil {
		t.Fatalf("A.Revoke(B): %v", err)
	}

	// B's device is revoked and has no HK wrap anymore.
	devs, err := a.http.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	for _, d := range devs {
		if d.ID == b.DeviceID() && d.Status != wire.DeviceRevoked {
			t.Fatalf("B should be revoked, got %q", d.Status)
		}
	}
	if _, found, err := b.http.GetHKWrap(ctx, b.DeviceID()); err != nil || found {
		t.Fatalf("B GetHKWrap after revoke: found=%v err=%v; want found=false", found, err)
	}

	// A is now on HK version 2.
	_, verA, err := a.resolveHK(ctx)
	if err != nil {
		t.Fatalf("A.resolveHK after rotate: %v", err)
	}
	if verA != bootstrapHKVersion+1 {
		t.Fatalf("A HK version = %d, want %d", verA, bootstrapHKVersion+1)
	}

	// Enroll a fresh device C AFTER the rotation. It receives HK2.
	c, _ := newDevice(t, url)
	if _, err := c.Register(ctx, "machine-C"); err != nil {
		t.Fatalf("C.Register: %v", err)
	}
	if err := a.Approve(ctx, c.DeviceID()); err != nil {
		t.Fatalf("A.Approve(C): %v", err)
	}
	hkC, verC, err := c.resolveHK(ctx)
	if err != nil {
		t.Fatalf("C.resolveHK: %v", err)
	}
	if verC != bootstrapHKVersion+1 {
		t.Fatalf("C HK version = %d, want %d", verC, bootstrapHKVersion+1)
	}
	hkA2, _, _ := a.resolveHK(ctx)
	if hkC != hkA2 {
		t.Fatal("C's HK does not equal A's rotated HK")
	}

	// C decrypts every pre-rotation record — proving old records stayed
	// decryptable through the rotation without being re-encrypted.
	recs, _, err := c.PullOthers(ctx, map[string]uint64{})
	if err != nil {
		t.Fatalf("C.PullOthers: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("C pulled %d records, want %d", len(recs), n)
	}
	for _, got := range recs {
		requireSameRecord(t, want[got.ID], got)
	}
}
