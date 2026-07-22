package cryptobox

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/curve25519"
)

// Recovery is the escape hatch from the per-device key model: without it,
// losing every enrolled device means the History Key can never be unwrapped
// again and the entire archive is cryptographically gone.
//
// A recovery passphrase is generated at bootstrap and stretched with Argon2id
// into two keypairs whose private halves exist only while the user is typing
// the phrase:
//
//   - X25519 — a recipient for one extra History Key wrap, so HK can be
//     recovered.
//   - Ed25519 — proof of possession, so the holder can authorize enrolling a
//     replacement device when no existing device survives to approve it.
//
// The server stores only the two public keys, the salt, and the wrapped HK, so
// it still holds nothing that can decrypt history. Brute-forcing the wrap means
// brute-forcing the passphrase through Argon2id, which the phrase entropy and
// the parameters below are chosen to make hopeless.

// Argon2id parameters. Stretching is a second line of defence only: the phrase
// this package generates carries 160 bits of entropy, which is already far
// beyond brute force at any cost factor. So these are tuned to be
// comfortably strong while staying safe on a small VM or container — a memory
// cost high enough to OOM the machine doing the recovery would be a worse
// failure than the attack it prevents.
const (
	argonTime    = 3
	argonMemory  = 128 * 1024 // KiB (128 MiB)
	argonThreads = 4
	saltSize     = 16
)

// Recovery phrase shape: groups of Crockford base32 characters over a 32-symbol
// alphabet, so 8*4*5 = 160 bits of entropy — well beyond offline attack.
const (
	recoveryGroups   = 8
	recoveryPerGroup = 4
)

// base32Alphabet is Crockford-style (no I, L, O, U) so a phrase read off a
// screen or a printout cannot be transcribed ambiguously.
const base32Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RecoveryKey is the keypair set derived from a recovery passphrase.
type RecoveryKey struct {
	pub      [32]byte
	priv     [32]byte
	signPriv ed25519.PrivateKey
	signPub  ed25519.PublicKey
}

// NewRecoveryPhrase generates a fresh recovery passphrase. It is shown to the
// user once and never stored: only keys derived from it reach the server.
func NewRecoveryPhrase() (string, error) {
	var b [recoveryGroups * recoveryPerGroup]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	var sb strings.Builder
	for g := 0; g < recoveryGroups; g++ {
		if g > 0 {
			sb.WriteByte('-')
		}
		for i := 0; i < recoveryPerGroup; i++ {
			sb.WriteByte(base32Alphabet[b[g*recoveryPerGroup+i]%32])
		}
	}
	return sb.String(), nil
}

// NewRecoverySalt returns a fresh Argon2id salt.
func NewRecoverySalt() ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// DeriveRecoveryKey stretches a recovery phrase into its keypairs. Derivation is
// deliberately slow (see the Argon2id parameters); callers should tell the user
// it is working.
//
// The phrase is normalised first — upper-cased and stripped of spaces and
// dashes — so it round-trips however the user retypes it.
func DeriveRecoveryKey(phrase string, salt []byte) (RecoveryKey, error) {
	norm := NormalizeRecoveryPhrase(phrase)
	if norm == "" {
		return RecoveryKey{}, fmt.Errorf("%w: empty recovery phrase", ErrKeyFormat)
	}
	if len(salt) != saltSize {
		return RecoveryKey{}, fmt.Errorf("%w: salt is %d bytes, want %d", ErrKeyLength, len(salt), saltSize)
	}

	// One derivation, split into two independent 32-byte halves, so the X25519
	// scalar and the Ed25519 seed never share bytes.
	out := argon2.IDKey([]byte(norm), salt, argonTime, argonMemory, argonThreads, 2*keySize)
	defer zero(out)

	var k RecoveryKey
	copy(k.priv[:], out[:keySize])
	pub, err := curve25519.X25519(k.priv[:], curve25519.Basepoint)
	if err != nil {
		return RecoveryKey{}, err
	}
	copy(k.pub[:], pub)

	k.signPriv = ed25519.NewKeyFromSeed(out[keySize:])
	k.signPub = k.signPriv.Public().(ed25519.PublicKey)
	return k, nil
}

// NormalizeRecoveryPhrase makes user-typed phrases comparable: upper-case, with
// spaces and dashes removed.
func NormalizeRecoveryPhrase(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Public returns the X25519 public key HK is wrapped to.
func (k RecoveryKey) Public() [32]byte { return k.pub }

// SignPublic returns the Ed25519 public key the server verifies recovery
// requests against.
func (k RecoveryKey) SignPublic() []byte { return append([]byte(nil), k.signPub...) }

// Sign signs msg with the recovery Ed25519 key, proving possession of the
// passphrase.
func (k RecoveryKey) Sign(msg []byte) []byte { return ed25519.Sign(k.signPriv, msg) }

// DeviceKeyFor adapts the recovery key to the DeviceKey shape UnwrapHK needs,
// so recovery reuses the exact same wrap/unwrap construction as a device.
func (k RecoveryKey) DeviceKeyFor() DeviceKey {
	return DeviceKey{priv: k.priv, pub: k.pub, signPriv: k.signPriv, signPub: k.signPub}
}
