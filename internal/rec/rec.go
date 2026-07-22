// Package rec defines the record type shared by every yore component.
// It is a leaf package: it must not import any other yore package.
package rec

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Type values for Record.Type.
const (
	TypeCmd    = ""       // a captured shell command (the zero value)
	TypeDelete = "delete" // a tombstone; TargetID names the record it deletes
	TypeTag    = "tag"    // a user-tag op: add/remove a freeform label on a command or session
)

// TagOp values for Record.TagOp (on TypeTag records).
const (
	TagOpAdd    = ""       // add the tag to the target (the zero value)
	TagOpRemove = "remove" // remove the tag from the target
)

// Record is one captured shell command (or tombstone) as spooled, stored,
// and synced. JSON field names are the wire format for both the spool and
// the encrypted sync payload — do not rename them.
type Record struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	TargetID string `json:"target_id,omitempty"`

	// Stream position; assigned by the local store at ingest, never by
	// the producer. HostID is the opaque ULID of the recording machine.
	HostID   string `json:"host_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`

	Session string `json:"session,omitempty"`
	Cmd     string `json:"cmd,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Exit    *int   `json:"exit,omitempty"`   // nil = unknown (e.g. imported)
	DurMs   *int64 `json:"dur_ms,omitempty"` // nil = unknown
	StartMs int64  `json:"start_ms,omitempty"`
	Tag     string `json:"tag,omitempty"` // executor: agent/tool that ran it (e.g. "claude-code"), "" = interactive

	// Prompt tracing (agent commands only). PromptID groups every command an
	// agent ran in service of one user prompt; Prompt is that prompt's text,
	// carried on each member so the grouping survives sync without a join.
	PromptID string `json:"prompt_id,omitempty"`
	Prompt   string `json:"prompt,omitempty"`

	// User tags (Type == TypeTag record carries these; TargetID names a command
	// or Session names a session it applies to — neither = a bare definition).
	// TagName is the freeform label, TagDesc an optional description, TagOp the
	// add/remove op. Names are the identity, so tags remap across machines for
	// free on sync.
	TagName string `json:"tag_name,omitempty"`
	TagDesc string `json:"tag_desc,omitempty"`
	TagOp   string `json:"tag_op,omitempty"`

	// Tags is the record's resolved freeform tags, filled by the daemon at query
	// time (executor auto-tag ∪ command tags ∪ session tags). Never stored or
	// synced — it is derived on read.
	Tags []string `json:"tags,omitempty"`

	DeletedMs int64  `json:"deleted_ms,omitempty"` // tombstone applied locally
	KeyID     string `json:"key_id,omitempty"`     // DEK that sealed this record in transit
}

// Deleted reports whether the record has been tombstoned locally.
func (r Record) Deleted() bool { return r.DeletedMs != 0 }

// NewID returns a fresh ULID (sortable, crypto-random).
func NewID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// ImportID returns the deterministic id used for records ingested from
// existing history files, so re-importing is a no-op.
func ImportID(hostID string, startMs int64, cmd string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "import|%s|%d|%s", hostID, startMs, cmd))
	return hex.EncodeToString(sum[:])[:32]
}

func IntPtr(v int) *int       { return &v }
func Int64Ptr(v int64) *int64 { return &v }
