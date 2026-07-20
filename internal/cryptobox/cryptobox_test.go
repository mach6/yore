package cryptobox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- fixtures -------------------------------------------------------------

func mustDevice(t *testing.T) DeviceKey {
	t.Helper()
	k, err := GenerateDeviceKey()
	if err != nil {
		t.Fatalf("GenerateDeviceKey: %v", err)
	}
	return k
}

// openFn re-runs an Open/Unwrap over a (possibly mangled) blob and reports
// only whether it authenticated. It lets the tamper/truncation loops treat
// every blob type uniformly.
type openFn func(blob []byte) error

// assertRejectsTampering flips every single bit-carrying byte of blob and
// requires each mutation to fail to open. A pristine blob must still open.
func assertRejectsTampering(t *testing.T, blob []byte, open openFn) {
	t.Helper()
	if err := open(blob); err != nil {
		t.Fatalf("pristine blob failed to open: %v", err)
	}
	for i := range blob {
		mangled := bytes.Clone(blob)
		mangled[i] ^= 0x01
		if err := open(mangled); err == nil {
			t.Fatalf("flipping byte %d did not fail to open", i)
		}
	}
}

// assertRejectsTruncation requires every proper prefix of blob to fail to
// open without panicking.
func assertRejectsTruncation(t *testing.T, blob []byte, open openFn) {
	t.Helper()
	for n := 0; n < len(blob); n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("open panicked on truncation to %d bytes: %v", n, r)
				}
			}()
			if err := open(blob[:n:n]); err == nil {
				t.Fatalf("truncation to %d bytes did not fail to open", n)
			}
		}()
	}
}

// --- device key ----------------------------------------------------------

func TestDeviceKeySaveLoadRoundTrip(t *testing.T) {
	k := mustDevice(t)
	path := filepath.Join(t.TempDir(), "device.key")
	if err := k.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", got)
	}

	got, err := LoadDeviceKey(path)
	if err != nil {
		t.Fatalf("LoadDeviceKey: %v", err)
	}
	if got.priv != k.priv || got.pub != k.pub {
		t.Fatal("loaded key does not match saved key")
	}
	if got.Public() != k.Public() {
		t.Fatal("Public() mismatch after round trip")
	}
}

func TestLoadDeviceKeyRejectsLoosePermissions(t *testing.T) {
	k := mustDevice(t)
	path := filepath.Join(t.TempDir(), "device.key")
	if err := k.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, err := LoadDeviceKey(path)
	if !errors.Is(err, ErrKeyPerms) {
		t.Fatalf("LoadDeviceKey err = %v, want ErrKeyPerms", err)
	}
}

func TestLoadDeviceKeyRejectsCorruption(t *testing.T) {
	k := mustDevice(t)
	path := filepath.Join(t.TempDir(), "device.key")
	if err := k.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Flip a byte inside the base64 payload (after the prefix) so the stored
	// public key no longer matches the private key.
	corrupt := bytes.Clone(raw)
	corrupt[len(devicePrefix)+5] ^= 0x01
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadDeviceKey(path); err == nil {
		t.Fatal("LoadDeviceKey accepted a corrupted key")
	}
}

func TestPublicFromBytes(t *testing.T) {
	k := mustDevice(t)
	pubSlice := k.Public()
	got, err := PublicFromBytes(pubSlice[:])
	if err != nil {
		t.Fatalf("PublicFromBytes: %v", err)
	}
	if got != k.Public() {
		t.Fatal("PublicFromBytes changed the key")
	}
	if _, err := PublicFromBytes(pubSlice[:31]); !errors.Is(err, ErrKeyLength) {
		t.Fatalf("short key err = %v, want ErrKeyLength", err)
	}
}

// --- History Key ---------------------------------------------------------

func TestHKWrapUnwrapAcrossDevices(t *testing.T) {
	a, b, c := mustDevice(t), mustDevice(t), mustDevice(t)
	hk, err := NewHistoryKey()
	if err != nil {
		t.Fatalf("NewHistoryKey: %v", err)
	}

	blob, err := WrapHK(hk, b.Public()) // sealed to B
	if err != nil {
		t.Fatalf("WrapHK: %v", err)
	}

	got, err := UnwrapHK(blob, b)
	if err != nil {
		t.Fatalf("B UnwrapHK: %v", err)
	}
	if got != hk {
		t.Fatal("B unwrapped the wrong HK")
	}

	if _, err := UnwrapHK(blob, a); err == nil {
		t.Fatal("A unwrapped a blob sealed to B")
	}
	if _, err := UnwrapHK(blob, c); err == nil {
		t.Fatal("third device C unwrapped a blob sealed to B")
	}
}

func TestHKBlobTamperAndTruncation(t *testing.T) {
	b := mustDevice(t)
	hk, _ := NewHistoryKey()
	blob, err := WrapHK(hk, b.Public())
	if err != nil {
		t.Fatalf("WrapHK: %v", err)
	}
	open := func(blob []byte) error { _, err := UnwrapHK(blob, b); return err }
	assertRejectsTampering(t, blob, open)
	assertRejectsTruncation(t, blob, open)
}

// --- DEK -----------------------------------------------------------------

func TestDEKWrapUnwrapRoundTrip(t *testing.T) {
	hk, _ := NewHistoryKey()
	keyID, dek, err := NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	blob, err := WrapDEK(dek, hk, keyID, "dev-1", 1700000000000, 3)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	got, err := UnwrapDEK(blob, hk, keyID, "dev-1", 1700000000000, 3)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if got != dek {
		t.Fatal("unwrapped DEK differs from original")
	}
}

func TestDEKWrongHKFails(t *testing.T) {
	hk, _ := NewHistoryKey()
	other, _ := NewHistoryKey()
	keyID, dek, _ := NewDEK()
	blob, _ := WrapDEK(dek, hk, keyID, "dev-1", 42, 1)
	if _, err := UnwrapDEK(blob, other, keyID, "dev-1", 42, 1); err == nil {
		t.Fatal("UnwrapDEK succeeded under the wrong HK")
	}
}

func TestDEKAADBinding(t *testing.T) {
	hk, _ := NewHistoryKey()
	keyID, dek, _ := NewDEK()
	const (
		devID = "dev-1"
		epoch = int64(1700000000000)
		hkVer = 2
	)
	blob, _ := WrapDEK(dek, hk, keyID, devID, epoch, hkVer)

	cases := []struct {
		name            string
		keyID, deviceID string
		epoch           int64
		hkVersion       int
	}{
		{"keyID", "different-key", devID, epoch, hkVer},
		{"deviceID", keyID, "dev-2", epoch, hkVer},
		{"epoch", keyID, devID, epoch + 1, hkVer},
		{"hkVersion", keyID, devID, epoch, hkVer + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := UnwrapDEK(blob, hk, tc.keyID, tc.deviceID, tc.epoch, tc.hkVersion); err == nil {
				t.Fatalf("changing %s did not fail authentication", tc.name)
			}
		})
	}
	// Sanity: the exact parameters still open.
	if _, err := UnwrapDEK(blob, hk, keyID, devID, epoch, hkVer); err != nil {
		t.Fatalf("baseline UnwrapDEK failed: %v", err)
	}
}

func TestDEKBlobTamperAndTruncation(t *testing.T) {
	hk, _ := NewHistoryKey()
	keyID, dek, _ := NewDEK()
	blob, _ := WrapDEK(dek, hk, keyID, "dev-1", 7, 1)
	open := func(blob []byte) error {
		_, err := UnwrapDEK(blob, hk, keyID, "dev-1", 7, 1)
		return err
	}
	assertRejectsTampering(t, blob, open)
	assertRejectsTruncation(t, blob, open)
}

// --- records -------------------------------------------------------------

func sampleAAD() RecordAAD {
	return RecordAAD{RecordID: "01H...REC", HostID: "01H...HOST", Seq: 99, KeyID: "01H...KEY"}
}

func TestRecordSealOpenRoundTrip(t *testing.T) {
	_, dek, _ := NewDEK()
	aad := sampleAAD()
	pt := []byte(`{"cmd":"git status","cwd":"/home/dev"}`)
	blob, err := SealRecord(pt, dek, aad)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	got, err := OpenRecord(blob, dek, aad)
	if err != nil {
		t.Fatalf("OpenRecord: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("OpenRecord = %q, want %q", got, pt)
	}
}

func TestRecordWrongDEKFails(t *testing.T) {
	_, dek, _ := NewDEK()
	_, wrong, _ := NewDEK()
	blob, _ := SealRecord([]byte("secret"), dek, sampleAAD())
	if _, err := OpenRecord(blob, wrong, sampleAAD()); err == nil {
		t.Fatal("OpenRecord succeeded under the wrong DEK")
	}
}

func TestRecordAADBinding(t *testing.T) {
	_, dek, _ := NewDEK()
	base := sampleAAD()
	blob, _ := SealRecord([]byte("secret"), dek, base)

	mut := map[string]func(a *RecordAAD){
		"RecordID": func(a *RecordAAD) { a.RecordID = "other" },
		"HostID":   func(a *RecordAAD) { a.HostID = "other" },
		"Seq":      func(a *RecordAAD) { a.Seq++ },
		"KeyID":    func(a *RecordAAD) { a.KeyID = "other" },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			aad := base
			f(&aad)
			if _, err := OpenRecord(blob, dek, aad); err == nil {
				t.Fatalf("changing %s did not fail authentication", name)
			}
		})
	}
}

func TestRecordBlobTamperAndTruncation(t *testing.T) {
	_, dek, _ := NewDEK()
	aad := sampleAAD()
	blob, _ := SealRecord([]byte("a somewhat longer plaintext payload"), dek, aad)
	open := func(blob []byte) error { _, err := OpenRecord(blob, dek, aad); return err }
	assertRejectsTampering(t, blob, open)
	assertRejectsTruncation(t, blob, open)
}

// --- nonce freshness -----------------------------------------------------

func TestSealIsNondeterministic(t *testing.T) {
	_, dek, _ := NewDEK()
	aad := sampleAAD()
	pt := []byte("same plaintext")
	b1, _ := SealRecord(pt, dek, aad)
	b2, _ := SealRecord(pt, dek, aad)
	if bytes.Equal(b1, b2) {
		t.Fatal("two seals of the same plaintext produced identical blobs (nonce reuse)")
	}
	// Both must still open to the same plaintext.
	for _, b := range [][]byte{b1, b2} {
		got, err := OpenRecord(b, dek, aad)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("re-open failed: got %q err %v", got, err)
		}
	}
}

// --- epoch ---------------------------------------------------------------

func TestEpochStart(t *testing.T) {
	const day = 24 * time.Hour
	const sixH = 6 * time.Hour
	mustParse := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return ts
	}
	msOf := func(s string) int64 { return mustParse(s).UnixMilli() }

	cases := []struct {
		name  string
		t     time.Time
		width time.Duration
		want  int64
	}{
		{"day-midnight-exact", mustParse("2026-07-20T00:00:00Z"), day, msOf("2026-07-20T00:00:00Z")},
		{"day-just-after-midnight", mustParse("2026-07-20T00:00:00.001Z"), day, msOf("2026-07-20T00:00:00Z")},
		{"day-midday", mustParse("2026-07-20T12:34:56Z"), day, msOf("2026-07-20T00:00:00Z")},
		{"day-just-before-next", mustParse("2026-07-20T23:59:59.999Z"), day, msOf("2026-07-20T00:00:00Z")},
		{"day-next-boundary", mustParse("2026-07-21T00:00:00Z"), day, msOf("2026-07-21T00:00:00Z")},
		{"6h-first-window", mustParse("2026-07-20T05:59:59Z"), sixH, msOf("2026-07-20T00:00:00Z")},
		{"6h-second-window", mustParse("2026-07-20T06:00:00Z"), sixH, msOf("2026-07-20T06:00:00Z")},
		{"6h-third-window", mustParse("2026-07-20T13:00:00Z"), sixH, msOf("2026-07-20T12:00:00Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EpochStart(tc.t, tc.width); got != tc.want {
				t.Fatalf("EpochStart = %d, want %d", got, tc.want)
			}
		})
	}

	// A record's epoch start is stable across the whole window and identical
	// for two clocks reading different instants within it.
	if EpochStart(mustParse("2026-07-20T00:00:01Z"), day) != EpochStart(mustParse("2026-07-20T23:00:00Z"), day) {
		t.Fatal("EpochStart not stable across a 24h window")
	}
}
