package cryptobox

import (
	"crypto/rand"
	"strconv"

	"github.com/mach6/yore/internal/rec"
)

// NewDEK generates a fresh epoch Data Encryption Key and its keyID (a ULID, so
// keyIDs sort by creation time). Records reference the DEK by this keyID.
func NewDEK() (keyID string, dek [32]byte, err error) {
	if _, err = rand.Read(dek[:]); err != nil {
		return "", [32]byte{}, err
	}
	return rec.NewID(), dek, nil
}

// WrapDEK seals dek under the History Key. The keyID, deviceID, epoch, and HK
// version are all bound as additional data, so a wrap cannot be silently
// replayed against a different key slot, device, epoch, or HK generation. The
// blob is nonce(24) ‖ ciphertext.
func WrapDEK(dek, hk [32]byte, keyID, deviceID string, epoch int64, hkVersion int) ([]byte, error) {
	return sealAEAD(hk, dek[:], dekAAD(keyID, deviceID, epoch, hkVersion))
}

// UnwrapDEK reverses WrapDEK. The four binding parameters must match exactly
// what WrapDEK was given, or authentication fails.
func UnwrapDEK(blob []byte, hk [32]byte, keyID, deviceID string, epoch int64, hkVersion int) ([32]byte, error) {
	pt, err := openAEAD(hk, blob, dekAAD(keyID, deviceID, epoch, hkVersion))
	if err != nil {
		return [32]byte{}, err
	}
	defer zero(pt)
	if len(pt) != keySize {
		return [32]byte{}, ErrKeyLength
	}
	var dek [32]byte
	copy(dek[:], pt)
	return dek, nil
}

// dekAAD builds the DEK-wrap additional-data string. Field order and
// separators are part of the wire contract; do not reorder.
func dekAAD(keyID, deviceID string, epoch int64, hkVersion int) []byte {
	return []byte(dekWrapDomain + "|" + keyID + "|" + deviceID + "|" +
		strconv.FormatInt(epoch, 10) + "|" + strconv.Itoa(hkVersion))
}
