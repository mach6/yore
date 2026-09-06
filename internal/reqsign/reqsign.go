// Package reqsign signs and verifies yore's mutating sync requests with a
// device's Ed25519 key, so that authorization no longer rests on the bearer
// token alone. A TLS-inspecting proxy (or anyone who captures the token) can
// see the header but cannot forge a signature (the device private key is never
// on the wire) so it can neither push garbage records nor revoke a device. It
// can at most replay a verbatim captured request; the server's nonce cache plus
// the timestamp window (Skew) block that too. This package is the SINGLE source
// of truth for the canonical byte string, so the client (internal/syncer) and
// server (internal/server) can never disagree.
package reqsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// Signature header names.
const (
	HdrDevice    = "X-Yore-Device"    // device id (the signer)
	HdrTimestamp = "X-Yore-Timestamp" // unix seconds
	HdrNonce     = "X-Yore-Nonce"     // random, single-use within Skew
	HdrSignature = "X-Yore-Signature" // base64url Ed25519 signature
)

// Skew bounds how far a request timestamp may be from the server clock; it also
// bounds how long the server must remember nonces for replay rejection.
const Skew = 5 * time.Minute

// Canonical is the exact byte string that gets signed. `target` is the request
// path plus raw query (i.e. net/http's Request.URL.RequestURI(); scheme and
// host excluded), which is identical on both ends of a normal reverse proxy.
func Canonical(method, target, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	s := method + "\n" + target + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	return []byte(s)
}

// Headers is the read side of an http.Header (which satisfies it).
type Headers interface{ Get(string) string }

// Sign produces the four signature headers for a request. sign is the device's
// Ed25519 signer (cryptobox.DeviceKey.Sign). The timestamp is the current time
// and the nonce is fresh random, so each call yields a distinct signature.
func Sign(deviceID string, sign func([]byte) []byte, method, target string, body []byte, now time.Time) (map[string]string, error) {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nb[:])
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(Canonical(method, target, ts, nonce, body))
	return map[string]string{
		HdrDevice:    deviceID,
		HdrTimestamp: ts,
		HdrNonce:     nonce,
		HdrSignature: base64.RawURLEncoding.EncodeToString(sig),
	}, nil
}

// Verify errors are distinguished so the server can log without leaking which
// check failed to a client.
var (
	ErrMissing   = errors.New("reqsign: missing signature headers")
	ErrTimestamp = errors.New("reqsign: timestamp outside window")
	ErrSignature = errors.New("reqsign: signature verification failed")
)

// Verify checks a request's signature headers against pub (the signing device's
// Ed25519 public key) and the timestamp window. It does NOT check nonce replay:
// the caller owns the nonce cache and must reject a repeated (device, nonce);
// use Device and Nonce to key it. On success the request is authentic and
// within the window; the caller should then record the nonce.
func Verify(h Headers, method, target string, body, pub []byte, now time.Time) error {
	ts := h.Get(HdrTimestamp)
	nonce := h.Get(HdrNonce)
	sigB := h.Get(HdrSignature)
	if ts == "" || nonce == "" || sigB == "" {
		return ErrMissing
	}
	tsec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrTimestamp
	}
	if d := now.Sub(time.Unix(tsec, 0)); d > Skew || d < -Skew {
		return ErrTimestamp
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB)
	if err != nil {
		return ErrSignature
	}
	if len(pub) != ed25519.PublicKeySize ||
		!ed25519.Verify(ed25519.PublicKey(pub), Canonical(method, target, ts, nonce, body), sig) {
		return ErrSignature
	}
	return nil
}

// Device returns the claimed signer's device id (before verification).
func Device(h Headers) string { return h.Get(HdrDevice) }

// Nonce returns the request nonce (used by the server's replay cache).
func Nonce(h Headers) string { return h.Get(HdrNonce) }
