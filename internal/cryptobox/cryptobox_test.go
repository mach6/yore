package cryptobox

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- fixtures -------------------------------------------------------------

func mustDevice(t *testing.T) DeviceKey {
	t.Helper()
	k, err := GenerateDeviceKey()
	require.NoError(t, err)
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
	require.NoError(t, open(blob), "pristine blob failed to open")
	for i := range blob {
		mangled := bytes.Clone(blob)
		mangled[i] ^= 0x01
		require.Error(t, open(mangled), "flipping byte %d did not fail to open", i)
	}
}

// assertRejectsTruncation requires every proper prefix of blob to fail to
// open without panicking.
func assertRejectsTruncation(t *testing.T, blob []byte, open openFn) {
	t.Helper()
	for n := 0; n < len(blob); n++ {
		var err error
		require.NotPanics(t, func() {
			err = open(blob[:n:n])
		}, "open panicked on truncation to %d bytes", n)
		require.Error(t, err, "truncation to %d bytes did not fail to open", n)
	}
}

// --- device key ----------------------------------------------------------

func TestDeviceKeyMarshalParseRoundTrip(t *testing.T) {
	k := mustDevice(t)
	got, err := ParseDeviceKey(k.Marshal())
	require.NoError(t, err)
	require.Equal(t, k.priv, got.priv, "parsed key does not match marshaled key")
	require.Equal(t, k.pub, got.pub, "parsed key does not match marshaled key")
	require.Equal(t, k.Public(), got.Public(), "Public() mismatch after round trip")

	// Surrounding whitespace/newlines must not matter (hand-edited fallback file).
	got2, err := ParseDeviceKey("  " + k.Marshal() + "\n")
	require.NoError(t, err)
	require.Equal(t, k.Public(), got2.Public(), "whitespace changed the parse")
}

func TestParseDeviceKeyRejectsBadFormat(t *testing.T) {
	_, err := ParseDeviceKey("not-a-yore-key")
	require.ErrorIs(t, err, ErrKeyFormat, "missing prefix must be rejected")

	_, err = ParseDeviceKey(devicePrefix + "!!!not-base64!!!")
	require.ErrorIs(t, err, ErrKeyFormat, "bad base64 must be rejected")
}

func TestParseDeviceKeyRejectsCorruption(t *testing.T) {
	k := mustDevice(t)
	s := k.Marshal()
	// Flip a byte inside the base64 payload (after the prefix) so the stored
	// public key no longer matches the private key.
	b := []byte(s)
	b[len(devicePrefix)+5] ^= 0x01
	if string(b) == s { // extremely unlikely; keep the test deterministic
		b[len(devicePrefix)+6] ^= 0x01
	}
	_, err := ParseDeviceKey(string(b))
	require.Error(t, err, "ParseDeviceKey accepted a corrupted key")
}

func TestPublicFromBytes(t *testing.T) {
	k := mustDevice(t)
	pubSlice := k.Public()
	got, err := PublicFromBytes(pubSlice[:])
	require.NoError(t, err)
	require.Equal(t, k.Public(), got, "PublicFromBytes changed the key")

	_, err = PublicFromBytes(pubSlice[:31])
	require.ErrorIs(t, err, ErrKeyLength)
}

// --- History Key ---------------------------------------------------------

func TestHKWrapUnwrapAcrossDevices(t *testing.T) {
	a, b, c := mustDevice(t), mustDevice(t), mustDevice(t)
	hk, err := NewHistoryKey()
	require.NoError(t, err)

	blob, err := WrapHK(hk, b.Public()) // sealed to B
	require.NoError(t, err)

	got, err := UnwrapHK(blob, b)
	require.NoError(t, err)
	require.Equal(t, hk, got, "B unwrapped the wrong HK")

	_, err = UnwrapHK(blob, a)
	require.Error(t, err, "A unwrapped a blob sealed to B")

	_, err = UnwrapHK(blob, c)
	require.Error(t, err, "third device C unwrapped a blob sealed to B")
}

func TestHKBlobTamperAndTruncation(t *testing.T) {
	b := mustDevice(t)
	hk, _ := NewHistoryKey()
	blob, err := WrapHK(hk, b.Public())
	require.NoError(t, err)
	open := func(blob []byte) error { _, err := UnwrapHK(blob, b); return err }
	assertRejectsTampering(t, blob, open)
	assertRejectsTruncation(t, blob, open)
}

// --- DEK -----------------------------------------------------------------

func TestDEKWrapUnwrapRoundTrip(t *testing.T) {
	hk, _ := NewHistoryKey()
	keyID, dek, err := NewDEK()
	require.NoError(t, err)
	blob, err := WrapDEK(dek, hk, keyID, "dev-1", 1700000000000, 3)
	require.NoError(t, err)
	got, err := UnwrapDEK(blob, hk, keyID, "dev-1", 1700000000000, 3)
	require.NoError(t, err)
	require.Equal(t, dek, got, "unwrapped DEK differs from original")
}

func TestDEKWrongHKFails(t *testing.T) {
	hk, _ := NewHistoryKey()
	other, _ := NewHistoryKey()
	keyID, dek, _ := NewDEK()
	blob, _ := WrapDEK(dek, hk, keyID, "dev-1", 42, 1)
	_, err := UnwrapDEK(blob, other, keyID, "dev-1", 42, 1)
	require.Error(t, err, "UnwrapDEK succeeded under the wrong HK")
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
			_, err := UnwrapDEK(blob, hk, tc.keyID, tc.deviceID, tc.epoch, tc.hkVersion)
			require.Error(t, err, "changing %s did not fail authentication", tc.name)
		})
	}
	// Sanity: the exact parameters still open.
	_, err := UnwrapDEK(blob, hk, keyID, devID, epoch, hkVer)
	require.NoError(t, err, "baseline UnwrapDEK failed")
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
	require.NoError(t, err)
	got, err := OpenRecord(blob, dek, aad)
	require.NoError(t, err)
	require.Equal(t, pt, got)
}

func TestRecordWrongDEKFails(t *testing.T) {
	_, dek, _ := NewDEK()
	_, wrong, _ := NewDEK()
	blob, _ := SealRecord([]byte("secret"), dek, sampleAAD())
	_, err := OpenRecord(blob, wrong, sampleAAD())
	require.Error(t, err, "OpenRecord succeeded under the wrong DEK")
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
			_, err := OpenRecord(blob, dek, aad)
			require.Error(t, err, "changing %s did not fail authentication", name)
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
	require.NotEqual(t, b1, b2, "two seals of the same plaintext produced identical blobs (nonce reuse)")

	// Both must still open to the same plaintext.
	for _, b := range [][]byte{b1, b2} {
		got, err := OpenRecord(b, dek, aad)
		require.NoError(t, err)
		require.Equal(t, pt, got)
	}
}

// --- epoch ---------------------------------------------------------------

func TestEpochStart(t *testing.T) {
	const day = 24 * time.Hour
	const sixH = 6 * time.Hour
	mustParse := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err, "parse %s", s)
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
			require.Equal(t, tc.want, EpochStart(tc.t, tc.width))
		})
	}

	// A record's epoch start is stable across the whole window and identical
	// for two clocks reading different instants within it.
	require.Equal(t, EpochStart(mustParse("2026-07-20T00:00:01Z"), day), EpochStart(mustParse("2026-07-20T23:00:00Z"), day),
		"EpochStart not stable across a 24h window")
}
