package cryptobox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// DeviceKey is a machine's long-lived identity. It holds two keypairs whose
// private halves never leave the machine:
//
//   - an X25519 keypair, used to seal/unwrap the History Key (the crypto
//     hierarchy); its public half is uploaded so other devices can wrap HK to it.
//   - an Ed25519 keypair, used to SIGN sync requests (see internal/reqsign) so a
//     captured bearer token can't be used to push or revoke; its public half is
//     registered so the server can verify signatures.
//
// The zero value is unusable — obtain one via GenerateDeviceKey or LoadDeviceKey.
type DeviceKey struct {
	priv     [32]byte           // X25519 scalar; clamped on use
	pub      [32]byte           // curve25519.X25519(priv, basepoint)
	signSeed [32]byte           // Ed25519 seed
	signPriv ed25519.PrivateKey // 64B, derived from signSeed
	signPub  ed25519.PublicKey  // 32B
}

// GenerateDeviceKey creates a fresh device identity from crypto/rand.
func GenerateDeviceKey() (DeviceKey, error) {
	var k DeviceKey
	if _, err := rand.Read(k.priv[:]); err != nil {
		return DeviceKey{}, err
	}
	pub, err := curve25519.X25519(k.priv[:], curve25519.Basepoint)
	if err != nil {
		return DeviceKey{}, err
	}
	copy(k.pub[:], pub)

	if _, err := rand.Read(k.signSeed[:]); err != nil {
		return DeviceKey{}, err
	}
	k.deriveSign()
	return k, nil
}

func (k *DeviceKey) deriveSign() {
	k.signPriv = ed25519.NewKeyFromSeed(k.signSeed[:])
	k.signPub = k.signPriv.Public().(ed25519.PublicKey)
}

// Public returns a copy of the device's X25519 public key (for HK wrapping).
func (k DeviceKey) Public() [32]byte { return k.pub }

// SignPublic returns the device's Ed25519 public key (for request-signature
// verification); it is registered with the server.
func (k DeviceKey) SignPublic() []byte {
	return append([]byte(nil), k.signPub...)
}

// Sign signs msg with the device's Ed25519 private key.
func (k DeviceKey) Sign(msg []byte) []byte {
	return ed25519.Sign(k.signPriv, msg)
}

// Verify checks an Ed25519 signature against a 32-byte public key. Malformed
// inputs return false rather than panicking.
func Verify(pub, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}

// PublicFromBytes validates and copies a 32-byte X25519 public key, e.g. one
// received from the server as wire.Device.PubKey.
func PublicFromBytes(b []byte) ([32]byte, error) {
	var pub [32]byte
	if len(b) != keySize {
		return pub, fmt.Errorf("%w: public key is %d bytes, want %d", ErrKeyLength, len(b), keySize)
	}
	copy(pub[:], b)
	return pub, nil
}

// Save writes the key to path as a single line
//
//	yore-device2.<base64url(x25519priv‖x25519pub‖ed25519seed)>\n
//
// with mode 0600, enforced even if path already existed with looser bits.
func (k DeviceKey) Save(path string) error {
	var raw [3 * keySize]byte
	copy(raw[0:], k.priv[:])
	copy(raw[keySize:], k.pub[:])
	copy(raw[2*keySize:], k.signSeed[:])
	line := devicePrefix + base64.RawURLEncoding.EncodeToString(raw[:]) + "\n"
	zero(raw[:])

	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		return err
	}
	// WriteFile leaves a pre-existing file's mode untouched; enforce 0600.
	return os.Chmod(path, 0o600)
}

// LoadDeviceKey reads and validates a device.key written by Save. It errors if
// the file mode grants any group or other permission, if the format is wrong,
// or if the stored X25519 public key does not match its private key (corruption).
func LoadDeviceKey(path string) (DeviceKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return DeviceKey{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return DeviceKey{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return DeviceKey{}, fmt.Errorf("%w: mode is %04o", ErrKeyPerms, fi.Mode().Perm())
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return DeviceKey{}, err
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, devicePrefix) {
		// A pre-signing device1 key can't be upgraded in place (no signing seed);
		// point the user at re-enrolling rather than loading a partial identity.
		if strings.HasPrefix(line, "yore-device1.") {
			return DeviceKey{}, fmt.Errorf("%w: device key predates request signing — run `yore setup` to re-enroll", ErrKeyFormat)
		}
		return DeviceKey{}, fmt.Errorf("%w: missing %q prefix", ErrKeyFormat, devicePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(line, devicePrefix))
	if err != nil {
		return DeviceKey{}, fmt.Errorf("%w: %v", ErrKeyFormat, err)
	}
	if len(raw) != 3*keySize {
		return DeviceKey{}, fmt.Errorf("%w: payload is %d bytes, want %d", ErrKeyFormat, len(raw), 3*keySize)
	}

	var k DeviceKey
	copy(k.priv[:], raw[0:keySize])
	copy(k.pub[:], raw[keySize:2*keySize])
	copy(k.signSeed[:], raw[2*keySize:])
	zero(raw)
	k.deriveSign()

	// Corruption check: the stored X25519 public key must be the one the private
	// key derives, so a silent bit-flip is rejected here rather than producing
	// undecryptable ciphertext later.
	derived, err := curve25519.X25519(k.priv[:], curve25519.Basepoint)
	if err != nil {
		return DeviceKey{}, err
	}
	if subtle.ConstantTimeCompare(derived, k.pub[:]) != 1 {
		return DeviceKey{}, fmt.Errorf("%w: public key does not match private key", ErrKeyFormat)
	}
	return k, nil
}
