package cryptobox

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// NewHistoryKey generates a fresh 32-byte History Key from crypto/rand. There
// is exactly one live HK per group; it is created once, on group bootstrap.
func NewHistoryKey() ([32]byte, error) {
	var hk [32]byte
	if _, err := rand.Read(hk[:]); err != nil {
		return [32]byte{}, err
	}
	return hk, nil
}

// WrapHK seals the History Key to recipientPub using an anonymous sealed-box
// construction: a throwaway X25519 keypair is generated, ECDH'd with the
// recipient's public key, and the shared secret is run through HKDF-SHA256
// (bound to both public keys) to derive the wrapping key. Only the holder of
// recipientPub's private key can unwrap. The blob is
//
//	ephemeralPub(32) ‖ nonce(24) ‖ ciphertext
//
// and reveals nothing about the sender.
func WrapHK(hk [32]byte, recipientPub [32]byte) ([]byte, error) {
	var ephPriv [32]byte
	if _, err := rand.Read(ephPriv[:]); err != nil {
		return nil, err
	}
	defer zero(ephPriv[:])

	ephPub, err := curve25519.X25519(ephPriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(ephPriv[:], recipientPub[:])
	if err != nil {
		return nil, err // X25519 rejects low-order points itself
	}
	defer zero(shared)
	if isAllZero(shared) { // explicit belt-and-braces low-order guard
		return nil, ErrLowOrder
	}

	wrapKey, err := hkWrapKey(shared, ephPub, recipientPub[:])
	if err != nil {
		return nil, err
	}
	defer zero(wrapKey[:])

	aead, err := chacha20poly1305.NewX(wrapKey[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	blob := make([]byte, 0, keySize+nonceSize+len(hk)+chacha20poly1305.Overhead)
	blob = append(blob, ephPub...)
	blob = append(blob, nonce...)
	blob = aead.Seal(blob, nonce, hk[:], []byte(hkWrapDomain))
	return blob, nil
}

// UnwrapHK reverses WrapHK using k's private key. It succeeds only for the
// device the blob was sealed to; any other device (or a tampered blob) fails.
func UnwrapHK(blob []byte, k DeviceKey) ([32]byte, error) {
	if len(blob) < keySize+nonceSize {
		return [32]byte{}, ErrTruncated
	}
	ephPub := blob[:keySize]
	nonce := blob[keySize : keySize+nonceSize]
	ct := blob[keySize+nonceSize:]

	shared, err := curve25519.X25519(k.priv[:], ephPub)
	if err != nil {
		return [32]byte{}, err
	}
	defer zero(shared)
	if isAllZero(shared) {
		return [32]byte{}, ErrLowOrder
	}

	// The recipient's own public key is bound into the HKDF info, matching
	// what WrapHK used as recipientPub.
	wrapKey, err := hkWrapKey(shared, ephPub, k.pub[:])
	if err != nil {
		return [32]byte{}, err
	}
	defer zero(wrapKey[:])

	aead, err := chacha20poly1305.NewX(wrapKey[:])
	if err != nil {
		return [32]byte{}, err
	}
	pt, err := aead.Open(nil, nonce, ct, []byte(hkWrapDomain))
	if err != nil {
		return [32]byte{}, err
	}
	defer zero(pt)
	if len(pt) != keySize {
		return [32]byte{}, ErrKeyLength
	}
	var hk [32]byte
	copy(hk[:], pt)
	return hk, nil
}

// hkWrapKey derives the 32-byte HK wrapping key via HKDF-SHA256 over the ECDH
// shared secret, binding both public keys into the info string so a blob is
// cryptographically tied to the exact (ephemeral, recipient) pair.
func hkWrapKey(shared, ephPub, recipientPub []byte) ([32]byte, error) {
	info := hkWrapDomain + "|" + b64(ephPub) + "|" + b64(recipientPub)
	out, err := hkdf.Key(sha256.New, shared, nil, info, keySize)
	if err != nil {
		return [32]byte{}, err
	}
	defer zero(out)
	var wk [32]byte
	copy(wk[:], out)
	return wk, nil
}

// b64 is the internal, unpadded encoding used only inside HKDF info strings.
// It is never parsed back, so the exact alphabet is immaterial as long as
// wrap and unwrap agree — which they do, both routing through this function.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
