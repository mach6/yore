package cryptobox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRecoveryPhraseShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		p, err := NewRecoveryPhrase()
		require.NoError(t, err, "NewRecoveryPhrase")
		assert.Len(t, p, recoveryGroups*recoveryPerGroup+recoveryGroups-1, "phrase length")
		for _, r := range NormalizeRecoveryPhrase(p) {
			assert.Contains(t, base32Alphabet, string(r), "phrase must use the unambiguous alphabet")
		}
		assert.False(t, seen[p], "phrases must not repeat")
		seen[p] = true
	}
}

func TestNormalizeRecoveryPhrase(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "already normal", in: "ABCD1234", want: "ABCD1234"},
		{name: "lowercase", in: "abcd1234", want: "ABCD1234"},
		{name: "dashed", in: "ABCD-1234", want: "ABCD1234"},
		{name: "spaced and padded", in: "  abcd 1234  ", want: "ABCD1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeRecoveryPhrase(tc.in))
		})
	}
}

// TestDeriveRecoveryKeyDeterministic is the property recovery depends on: the
// same phrase and salt must reproduce the exact keypair months later, on
// another machine, or the wrapped History Key is unrecoverable.
func TestDeriveRecoveryKeyDeterministic(t *testing.T) {
	salt, err := NewRecoverySalt()
	require.NoError(t, err, "NewRecoverySalt")
	phrase, err := NewRecoveryPhrase()
	require.NoError(t, err, "NewRecoveryPhrase")

	a, err := DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "derive a")
	b, err := DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "derive b")
	assert.Equal(t, a.Public(), b.Public(), "same phrase+salt must derive the same X25519 key")
	assert.Equal(t, a.SignPublic(), b.SignPublic(), "same phrase+salt must derive the same Ed25519 key")

	// Formatting must not matter: the user retypes this by hand.
	c, err := DeriveRecoveryKey(NormalizeRecoveryPhrase(phrase), salt)
	require.NoError(t, err, "derive from normalized")
	assert.Equal(t, a.Public(), c.Public(), "normalization must not change the key")
}

func TestDeriveRecoveryKeySeparation(t *testing.T) {
	salt, err := NewRecoverySalt()
	require.NoError(t, err, "salt")
	other, err := NewRecoverySalt()
	require.NoError(t, err, "other salt")
	phrase, err := NewRecoveryPhrase()
	require.NoError(t, err, "phrase")
	wrong, err := NewRecoveryPhrase()
	require.NoError(t, err, "wrong phrase")

	base, err := DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "derive")

	bySalt, err := DeriveRecoveryKey(phrase, other)
	require.NoError(t, err, "derive with other salt")
	assert.NotEqual(t, base.Public(), bySalt.Public(), "a different salt must derive a different key")

	byPhrase, err := DeriveRecoveryKey(wrong, salt)
	require.NoError(t, err, "derive with wrong phrase")
	assert.NotEqual(t, base.Public(), byPhrase.Public(), "a different phrase must derive a different key")

	// The two halves must be independent: the X25519 key must never equal the
	// Ed25519 key material.
	basePub := base.Public()
	assert.NotEqual(t, basePub[:], base.SignPublic(), "keypairs must not share bytes")
}

func TestDeriveRecoveryKeyRejectsBadInput(t *testing.T) {
	good, err := NewRecoverySalt()
	require.NoError(t, err, "salt")

	_, err = DeriveRecoveryKey("", good)
	assert.ErrorIs(t, err, ErrKeyFormat, "empty phrase")

	_, err = DeriveRecoveryKey("ABCD", []byte("short"))
	assert.ErrorIs(t, err, ErrKeyLength, "wrong salt size")
}

// TestRecoveryUnwrapsHistoryKey is the end-to-end guarantee: HK sealed to the
// recovery key at bootstrap opens later with nothing but the phrase and salt.
func TestRecoveryUnwrapsHistoryKey(t *testing.T) {
	salt, err := NewRecoverySalt()
	require.NoError(t, err, "salt")
	phrase, err := NewRecoveryPhrase()
	require.NoError(t, err, "phrase")

	rk, err := DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "derive")

	hk, err := NewHistoryKey()
	require.NoError(t, err, "NewHistoryKey")
	blob, err := WrapHK(hk, rk.Public())
	require.NoError(t, err, "WrapHK to recovery key")

	// Everything the device held is gone; only the phrase and salt remain.
	again, err := DeriveRecoveryKey(phrase, salt)
	require.NoError(t, err, "re-derive")
	got, err := UnwrapHK(blob, again.DeviceKeyFor())
	require.NoError(t, err, "UnwrapHK with the recovery key")
	assert.Equal(t, hk, got, "recovered History Key must match")

	// A wrong phrase must not open it.
	wrongPhrase, err := NewRecoveryPhrase()
	require.NoError(t, err, "wrong phrase")
	wrong, err := DeriveRecoveryKey(wrongPhrase, salt)
	require.NoError(t, err, "derive wrong")
	_, err = UnwrapHK(blob, wrong.DeviceKeyFor())
	assert.Error(t, err, "a wrong phrase must not unwrap the History Key")
}
