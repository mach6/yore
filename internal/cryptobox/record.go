package cryptobox

import "strconv"

// RecordAAD is the additional data bound into a sealed history record. Every
// field is authenticated (not encrypted), so a sealed record cannot be moved
// to a different record ID, host stream, sequence position, or DEK without the
// tamper being detected on Open. These map to rec.Record / wire.PushRecord
// fields; the caller supplies the same values at seal and open time.
type RecordAAD struct {
	RecordID string // rec.Record.ID
	HostID   string // rec.Record.HostID (the stream's ULID)
	Seq      uint64 // rec.Record.Seq (position in the host stream)
	KeyID    string // DEK keyID that sealed this record
}

// bytes renders the AAD. Field order and separators are part of the wire
// contract; do not reorder.
func (a RecordAAD) bytes(domain string) []byte {
	return []byte(domain + "|" + a.RecordID + "|" + a.HostID + "|" +
		strconv.FormatUint(a.Seq, 10) + "|" + a.KeyID)
}

// SealRecord encrypts a record's plaintext (typically its JSON encoding) under
// the epoch DEK, binding aad. The blob is nonce(24) ‖ ciphertext.
func SealRecord(plaintext []byte, dek [32]byte, aad RecordAAD) ([]byte, error) {
	return sealAEAD(dek, plaintext, aad.bytes(recSealDomain))
}

// OpenRecord reverses SealRecord. aad must match the seal-time value exactly.
// A record sealed under the legacy separator opens too.
func OpenRecord(blob []byte, dek [32]byte, aad RecordAAD) ([]byte, error) {
	pt, err := openAEAD(dek, blob, aad.bytes(recSealDomain))
	if err != nil {
		if legacy, lerr := openAEAD(dek, blob, aad.bytes(legacyRecSealDomain)); lerr == nil {
			return legacy, nil
		}
	}
	return pt, err
}
