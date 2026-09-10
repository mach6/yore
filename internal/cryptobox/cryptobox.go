// Package cryptobox is yore's end-to-end encryption core. It is the security
// guarantee of the whole system: the sync server stores only the outputs of
// this package and never sees a plaintext record or any key.
//
// # Key hierarchy
//
//	device X25519 keypair   never leaves the machine (device.key, 0600)
//	  │  seals ──▶ History Key (HK)   32B symmetric, one per group,
//	  │                               stored only as per-device wrapped blobs
//	  │  wraps  ──▶ epoch Data Encryption Keys (DEKs)   32B, one per device
//	  │                               per epoch, each wrapped once under HK
//	  └  seals  ──▶ history records    each sealed with its epoch's DEK
//
// Every operation is O(1) in the age of the history: unwrapping one DEK opens
// an entire epoch of records, and adding a device re-wraps only HK and the
// DEKs, never the records.
//
// # Construction
//
// Every confidentiality boundary is XChaCha20-Poly1305 (chacha20poly1305.NewX)
// with a fresh 24-byte random nonce. XChaCha's 192-bit nonces make random
// nonces collision-safe at any realistic volume, so no counter state is kept.
// The domain-separation strings below are the version seams: each is bound
// into its AEAD as additional data (and, for HK, into the HKDF info), so a
// blob produced under one construction can never be opened under another.
//
// Key material is zeroed after use on a best-effort basis. Go's garbage
// collector may copy or retain values, so these wipes reduce, but cannot
// eliminate, the window in which a key sits in memory.
package cryptobox

import (
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// Version-seam domain separators. To migrate a construction, introduce a new
// vN constant rather than editing one of these: existing blobs stay openable
// only under the exact string they were sealed with.
const (
	hkWrapDomain  = "github.com/mach6/yore/hk-wrap/v1"  // History Key sealed to a device
	dekWrapDomain = "github.com/mach6/yore/dek-wrap/v1" // DEK wrapped under HK
	recSealDomain = "github.com/mach6/yore/rec/v1"      // history record sealed under a DEK

	devicePrefix = "yore-device2." // device.key line prefix (format v2: adds Ed25519 signing seed)
)

// Byte sizes shared across the package.
const (
	keySize   = 32                          // X25519 scalar / symmetric key
	nonceSize = chacha20poly1305.NonceSizeX // 24, XChaCha20-Poly1305
)

// Sentinel errors. AEAD authentication failures (tampering, wrong key, wrong
// AAD) surface as the underlying cipher error; callers should treat any
// non-nil error from an Open/Unwrap as "this blob is not authentic".
var (
	// ErrTruncated means a blob is shorter than its fixed framing requires.
	ErrTruncated = errors.New("cryptobox: blob truncated")
	// ErrLowOrder means an X25519 exchange produced an all-zero secret.
	ErrLowOrder = errors.New("cryptobox: X25519 shared secret is low-order")
	// ErrKeyFormat means a device key string is not a well-formed v2 key line.
	ErrKeyFormat = errors.New("cryptobox: malformed device key")
	// ErrKeyLength means a key or plaintext had an unexpected length.
	ErrKeyLength = errors.New("cryptobox: unexpected key length")
)

// sealAEAD encrypts plaintext under key with a fresh nonce, returning
// nonce ‖ ciphertext. aad is authenticated but not encrypted.
func sealAEAD(key [32]byte, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Seal appends the ciphertext to nonce, yielding nonce ‖ ciphertext.
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

// openAEAD reverses sealAEAD. It returns a clean error (never a panic) for
// blobs too short to contain a nonce, and the cipher's authentication error
// for anything that fails to verify.
func openAEAD(key [32]byte, blob, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	if len(blob) < nonceSize {
		return nil, ErrTruncated
	}
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	return aead.Open(nil, nonce, ct, aad)
}

// zero wipes b (best effort; see package doc).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// isAllZero reports whether every byte of b is zero, in constant time.
func isAllZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}
