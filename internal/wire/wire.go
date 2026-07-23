// Package wire defines the JSON types of the sync server's HTTP API.
// It is a leaf contract package shared by internal/server and internal/syncer;
// like internal/proto it must not import any non-leaf yore package.
//
// []byte fields marshal as base64 (encoding/json's default) — that is the
// wire encoding for all ciphertext blobs. The server never sees plaintext:
// record blobs, HK wraps, and DEK wraps are sealed client-side.
package wire

// Device lifecycle states.
const (
	DevicePending = "pending" // registered, awaiting approval from an existing device
	DeviceActive  = "active"
	DeviceRevoked = "revoked"
)

// Device is a machine enrolled (or enrolling) in the history group.
type Device struct {
	ID        string `json:"id"`       // client-generated ULID
	Name      string `json:"name"`     // human label, e.g. hostname (user-chosen; NOT secret)
	PubKey    []byte `json:"pub_key"`  // X25519 public key (HK wrapping)
	SignKey   []byte `json:"sign_key"` // Ed25519 public key (request-signature verification)
	Status    string `json:"status"`
	CreatedMs int64  `json:"created_ms"`
}

// RegisterReq enrolls a new pending device. The enrollment token authorizing
// it travels in the X-Yore-Token header, not the body, so it is never stored
// alongside the device record.
type RegisterReq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	PubKey  []byte `json:"pub_key"`
	SignKey []byte `json:"sign_key"`
}

// RegisterResp answers a registration. GroupFormed tells the newcomer which
// path it is on without needing a read it is not yet authorized for: false
// means it is the first device and should form the group, true means an active
// device already exists and this one waits for approval.
type RegisterResp struct {
	Device      Device `json:"device"`
	GroupFormed bool   `json:"group_formed"`
}

// TokenResp is a freshly minted enrollment token. The plaintext is returned
// exactly once, at mint time: the server keeps only its hash.
type TokenResp struct {
	Token     string `json:"token"`
	ExpiresMs int64  `json:"expires_ms"`
}

// RecoveryInit publishes the recovery keypair derived from the recovery
// passphrase, plus the History Key sealed to it. Uploaded once at bootstrap.
type RecoveryInit struct {
	Salt    []byte `json:"salt"`     // Argon2id salt (not secret)
	PubKey  []byte `json:"pub_key"`  // X25519 public key derived from the passphrase
	SignKey []byte `json:"sign_key"` // Ed25519 public key derived from the passphrase
	Wrap    HKWrap `json:"wrap"`     // HK sealed to PubKey
}

// RecoverySalt is the unauthenticated half of recovery: the Argon2id parameters
// needed to derive the recovery keypair from the passphrase before the holder
// can prove possession of it.
type RecoverySalt struct {
	Salt []byte `json:"salt"`
}

// HKWrap is the History Key sealed to one device's public key.
type HKWrap struct {
	DeviceID  string `json:"device_id"`
	Blob      []byte `json:"blob"`
	HKVersion int    `json:"hk_version"`
}

// ActivateReq approves a pending device: the approver uploads the HK wrapped
// for the new device's public key.
type ActivateReq struct {
	Wrap HKWrap `json:"wrap"`
}

// DEKWrap is one epoch data key sealed under the History Key.
type DEKWrap struct {
	KeyID     string `json:"key_id"` // ULID, referenced by record envelopes
	DeviceID  string `json:"device_id"`
	Epoch     int64  `json:"epoch"` // unix millis of the epoch start (informational)
	Blob      []byte `json:"blob"`
	HKVersion int    `json:"hk_version"`
}

// DEKListResp pages DEK wraps; cursor is the last KeyID seen ("" from start).
type DEKListResp struct {
	Wraps      []DEKWrap `json:"wraps"`
	NextCursor string    `json:"next_cursor,omitempty"` // "" = caught up
}

// RotateReq atomically replaces the entire wrap set after a revocation:
// a full new HK wrap set (surviving devices only) and every DEK re-wrapped
// under the new HK. The server applies all-or-nothing.
type RotateReq struct {
	HKVersion int       `json:"hk_version"` // must be current+1
	HKWraps   []HKWrap  `json:"hk_wraps"`
	DEKWraps  []DEKWrap `json:"dek_wraps"`
}

// PushRecord is one sealed history record in a host's append-only stream.
type PushRecord struct {
	Seq   uint64 `json:"seq"`
	ID    string `json:"id"`
	KeyID string `json:"key_id"`
	Blob  []byte `json:"blob"`
}

// PushReq uploads a batch (ascending seq, ≤1000) for one host stream.
type PushReq struct {
	HostID  string       `json:"host_id"`
	Records []PushRecord `json:"records"`
}

// PushResp acknowledges a push. Duplicate (host_id, seq) rows are skipped
// silently — pushes are idempotent.
type PushResp struct {
	Stored int    `json:"stored"`
	MaxSeq uint64 `json:"max_seq"`
}

// PullRecord is one sealed record streamed back during sync.
type PullRecord struct {
	Seq       uint64 `json:"seq"`
	ID        string `json:"id"`
	KeyID     string `json:"key_id"`
	Blob      []byte `json:"blob"`
	CreatedMs int64  `json:"created_ms"` // server receive time, informational
}

// PullResp pages one host's stream after a cursor.
type PullResp struct {
	Records   []PullRecord `json:"records"`
	NextAfter *uint64      `json:"next_after,omitempty"` // nil = caught up
}

// HostInfo summarizes one host stream for cursor planning.
type HostInfo struct {
	HostID string `json:"host_id"`
	MaxSeq uint64 `json:"max_seq"`
}

// HostsResp lists all host streams the server knows.
type HostsResp struct {
	Hosts []HostInfo `json:"hosts"`
}

// ErrorResp is the body of any non-2xx response.
type ErrorResp struct {
	Error string `json:"error"`
}
