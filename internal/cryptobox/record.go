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
func (a RecordAAD) bytes() []byte {
	return []byte(recSealDomain + "|" + a.RecordID + "|" + a.HostID + "|" +
		strconv.FormatUint(a.Seq, 10) + "|" + a.KeyID)
}

// SealRecord encrypts a record's plaintext (typically its JSON encoding) under
// the epoch DEK, binding aad. The blob is nonce(24) ‖ ciphertext.
func SealRecord(plaintext []byte, dek [32]byte, aad RecordAAD) ([]byte, error) {
	return sealAEAD(dek, plaintext, aad.bytes())
}

// OpenRecord reverses SealRecord. aad must match the seal-time value exactly.
func OpenRecord(blob []byte, dek [32]byte, aad RecordAAD) ([]byte, error) {
	return openAEAD(dek, blob, aad.bytes())
}
