package cryptobox

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// DeviceKey is a machine's long-lived X25519 identity. The private half never
// leaves the machine; the public half is uploaded to the server so other
// devices can seal the History Key to it. The zero value is unusable — obtain
// one via GenerateDeviceKey or LoadDeviceKey.
type DeviceKey struct {
	priv [32]byte // raw random scalar; X25519 clamps on use
	pub  [32]byte // curve25519.X25519(priv, basepoint)
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
	return k, nil
}

// Public returns a copy of the device's public key.
func (k DeviceKey) Public() [32]byte { return k.pub }

// PublicFromBytes validates and copies a 32-byte public key, e.g. one received
// from the server as wire.Device.PubKey.
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
//	yore-device1.<base64url(priv‖pub)>\n
//
// with mode 0600, enforced even if path already existed with looser bits.
func (k DeviceKey) Save(path string) error {
	var raw [2 * keySize]byte
	copy(raw[:keySize], k.priv[:])
	copy(raw[keySize:], k.pub[:])
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
// or if the stored public key does not match the private key (corruption).
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
		return DeviceKey{}, fmt.Errorf("%w: missing %q prefix", ErrKeyFormat, devicePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(line, devicePrefix))
	if err != nil {
		return DeviceKey{}, fmt.Errorf("%w: %v", ErrKeyFormat, err)
	}
	if len(raw) != 2*keySize {
		return DeviceKey{}, fmt.Errorf("%w: payload is %d bytes, want %d", ErrKeyFormat, len(raw), 2*keySize)
	}

	var k DeviceKey
	copy(k.priv[:], raw[:keySize])
	copy(k.pub[:], raw[keySize:])
	zero(raw)

	// Corruption check: the stored public key must be the one the private
	// key derives, so a silent bit-flip in either half is rejected here
	// rather than producing undecryptable ciphertext later.
	derived, err := curve25519.X25519(k.priv[:], curve25519.Basepoint)
	if err != nil {
		return DeviceKey{}, err
	}
	if subtle.ConstantTimeCompare(derived, k.pub[:]) != 1 {
		return DeviceKey{}, fmt.Errorf("%w: public key does not match private key", ErrKeyFormat)
	}
	return k, nil
}
