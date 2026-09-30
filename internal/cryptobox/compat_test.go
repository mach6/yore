package cryptobox

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// These blobs were sealed by released builds, one set under each generation of
// domain separators. Every machine in a group has to keep opening all of them,
// so they are hex rather than strings: a rename that rewrites the separators
// cannot rewrite these along with them, and any change that alters what a
// construction binds fails here instead of at a user's next daemon restart.
const (
	compatDeviceKey = "yore-device2.AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyAHo3y8FCCTyLdV3BsQ6Gy0JjdK0WqoU-0L38CyuG0cfGVmZ2hpamtsbW5vcHFyc3R1dnd4eXp7fH1-f4CBgoOE"
	legacyHKWrap    = "d6129e56d0fbc0a1d4b620eb505a1e4ba2c38245250d8f09b13eda158666e747b4d7d26d0870d7f83528add0c043ce6af342481cf16e421f63b33ee5b206984c4696340869aa6c75bcb3df4772f460419d9acd66487112014db666322b20d0285065bc2198cf5d57"
	legacyDEKWrap   = "812f17735c161d9b15cd81fe5cabae79dcfdba14e89900ef079f5ab9931edd17fa6dfb9aaceadc46586ba6483955c94222f89bff80ebc88f9b648fcf95186f54efef84352244e43f"
	legacyRecord    = "ad5529fd895107c4afb9d11d936bee83f56e7d90331b50dc1e8b04da4f2a1dedc0996425bb4e50fe3e8553cb96d2f0529e96"

	currentHKWrap  = "88538bff2bf296c7f891874cb84e3e4580539fa4745d58f7f2a3be45cb823d2bee932ef30746cada4e4991edad3d624e2caa5f61e618b77600dd517c7a9f2824cba3758df1a65786bc202f24dfee4630bc50c21a3f88489d15f5aa3db7320b65142a7bbfb5d61d06"
	currentDEKWrap = "a8e85e27e5c5b0a8caf2647adb1722c91a083543b4df0716001d8e69b8088d9d5d8a484afc00e4cd3a562685303ff0e222f748fd59f34fdd9342a0f6d758f9574c618d67f82f4862"
	currentRecord  = "4d0ad3c0c880579001e71f7c1634397f5ee42d1525bf5304acbe57273dd079f8325076ce7a3c4126b2c12b176487b83b9dfa"
)

// seq32 is 32 bytes counting up from start, the fixture keys' material.
func seq32(start byte) (a [32]byte) {
	for i := range a {
		a[i] = start + byte(i)
	}
	return a
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestOpensBlobsSealedByEarlierBuilds(t *testing.T) {
	for _, tc := range []struct{ name, hkWrap, dekWrap, record string }{
		{"legacy separators", legacyHKWrap, legacyDEKWrap, legacyRecord},
		{"current separators", currentHKWrap, currentDEKWrap, currentRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseDeviceKey(compatDeviceKey)
			require.NoError(t, err)

			hk, err := UnwrapHK(unhex(t, tc.hkWrap), k)
			require.NoError(t, err, "history key wrap")
			require.Equal(t, seq32(51), hk)

			dek, err := UnwrapDEK(unhex(t, tc.dekWrap), hk, "key-1", "device-1", 1700000000, 1)
			require.NoError(t, err, "DEK wrap")
			require.Equal(t, seq32(151), dek)

			pt, err := OpenRecord(unhex(t, tc.record), dek, compatAAD)
			require.NoError(t, err, "record")
			require.Equal(t, "echo hello", string(pt))
		})
	}
}

var compatAAD = RecordAAD{RecordID: "rec-1", HostID: "host-1", Seq: 7, KeyID: "key-1"}

// New blobs are sealed only under the current separators: the legacy ones are
// accepted on open and never produced.
func TestSealsUnderCurrentSeparatorsOnly(t *testing.T) {
	k, err := ParseDeviceKey(compatDeviceKey)
	require.NoError(t, err)
	hk, dek := seq32(51), seq32(151)

	hkBlob, err := WrapHK(hk, k.Public())
	require.NoError(t, err)
	_, err = unwrapHK(hkWrapDomain, hkBlob, k)
	require.NoError(t, err)
	_, err = unwrapHK(legacyHKWrapDomain, hkBlob, k)
	require.Error(t, err)

	dekBlob, err := WrapDEK(dek, hk, "key-1", "device-1", 1700000000, 1)
	require.NoError(t, err)
	_, err = openAEAD(hk, dekBlob, dekAAD(legacyDEKWrapDomain, "key-1", "device-1", 1700000000, 1))
	require.Error(t, err)

	recBlob, err := SealRecord([]byte("echo hello"), dek, compatAAD)
	require.NoError(t, err)
	_, err = openAEAD(dek, recBlob, compatAAD.bytes(legacyRecSealDomain))
	require.Error(t, err)
}

// A blob that opens under neither separator reports the failure, not a key.
func TestLegacyFallbackStillRejectsTampering(t *testing.T) {
	k, err := ParseDeviceKey(compatDeviceKey)
	require.NoError(t, err)
	for _, wrap := range []string{legacyHKWrap, currentHKWrap} {
		blob := unhex(t, wrap)
		blob[len(blob)-1] ^= 1
		_, err := UnwrapHK(blob, k)
		require.Error(t, err)
	}
}
