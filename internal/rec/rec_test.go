package rec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewIDUniqueAndWellFormed(t *testing.T) {
	const n = 500
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		require.Len(t, id, 26, "a ULID is 26 chars")
		_, dup := seen[id]
		require.Falsef(t, dup, "NewID returned a duplicate: %s", id)
		seen[id] = struct{}{}
	}
	// Note: NewID uses crypto/rand entropy (not monotonic), so IDs minted within
	// the same millisecond are NOT mutually ordered — the store uses its assigned
	// seq for stream position, never the ULID. Ordering across distinct
	// timestamps IS guaranteed, which is what NewIDAcrossTimeSorts covers.
}

// TestNewIDAcrossTimeSorts confirms the one ordering guarantee that does hold:
// the millisecond-precision timestamp prefix means a later ULID sorts after an
// earlier one when the mint times differ.
func TestNewIDAcrossTimeSorts(t *testing.T) {
	first := NewID()
	var second string
	// Spin until the clock advances at least 1ms so the timestamp prefixes differ.
	for i := 0; i < 100_000; i++ {
		second = NewID()
		if second[:10] != first[:10] { // first 10 chars encode the 48-bit ms timestamp
			break
		}
	}
	require.NotEqual(t, first[:10], second[:10], "clock did not advance a millisecond")
	require.Less(t, first, second, "a later ULID must sort after an earlier one")
}

func TestImportIDDeterministic(t *testing.T) {
	a := ImportID("host-1", 1000, "git status")
	require.Len(t, a, 32, "import id is 32 hex chars")
	require.Equal(t, a, ImportID("host-1", 1000, "git status"), "same inputs must yield the same id (re-import is a no-op)")

	// Any component changing must change the id.
	assert.NotEqual(t, a, ImportID("host-2", 1000, "git status"), "host must matter")
	assert.NotEqual(t, a, ImportID("host-1", 1001, "git status"), "start time must matter")
	assert.NotEqual(t, a, ImportID("host-1", 1000, "git diff"), "command must matter")
}

func TestDeleted(t *testing.T) {
	assert.False(t, Record{}.Deleted(), "a fresh record is not deleted")
	assert.True(t, Record{DeletedMs: 1}.Deleted(), "a tombstoned record is deleted")
}

func TestPtrHelpers(t *testing.T) {
	require.Equal(t, 7, *IntPtr(7))
	require.Equal(t, int64(9), *Int64Ptr(9))
}

// TestRecordJSONPointerFidelity pins the contract the sync payload depends on:
// Exit/DurMs are pointers so "unknown" (nil, e.g. an imported row) survives a
// round trip distinctly from a real zero value.
func TestRecordJSONPointerFidelity(t *testing.T) {
	tests := []struct {
		name string
		rec  Record
	}{
		{name: "known zero exit", rec: Record{ID: "a", Cmd: "true", Exit: IntPtr(0), DurMs: Int64Ptr(0)}},
		{name: "nonzero exit", rec: Record{ID: "b", Cmd: "false", Exit: IntPtr(1), DurMs: Int64Ptr(42)}},
		{name: "unknown exit/dur", rec: Record{ID: "c", Cmd: "imported"}},
		{name: "tombstone", rec: Record{ID: "d", Type: TypeDelete, TargetID: "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.rec)
			require.NoError(t, err, "marshal")
			var got Record
			require.NoError(t, json.Unmarshal(b, &got), "unmarshal")
			require.Equal(t, tc.rec, got, "record must survive a JSON round trip byte-for-byte")
		})
	}
}

// TestRecordUnknownVsZeroExitEncoding proves nil and 0 serialize differently, so
// a decoder can tell "no exit recorded" from "exited 0".
func TestRecordUnknownVsZeroExitEncoding(t *testing.T) {
	unknown, err := json.Marshal(Record{ID: "x"})
	require.NoError(t, err)
	assert.NotContains(t, string(unknown), "\"exit\"", "unknown exit must be omitted")

	zero, err := json.Marshal(Record{ID: "x", Exit: IntPtr(0)})
	require.NoError(t, err)
	assert.Contains(t, string(zero), "\"exit\":0", "a known zero exit must be present")
}

// FuzzImportID guards the deterministic id helper against any panic on
// arbitrary input, and pins its output width.
func FuzzImportID(f *testing.F) {
	f.Add("host", int64(0), "cmd")
	f.Add("", int64(-1), "")
	f.Fuzz(func(t *testing.T, host string, start int64, cmd string) {
		id := ImportID(host, start, cmd)
		if len(id) != 32 {
			t.Fatalf("ImportID width = %d, want 32", len(id))
		}
	})
}

// FuzzRecordUnmarshal ensures decoding arbitrary bytes into a Record never
// panics — the spool and the decrypted sync payload both feed it untrusted-ish
// bytes.
func FuzzRecordUnmarshal(f *testing.F) {
	f.Add([]byte(`{"id":"a","cmd":"ls","exit":0}`))
	f.Add([]byte(`{"type":"delete","target_id":"x"}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		var r Record
		_ = json.Unmarshal(data, &r)
	})
}

// TestExecutorHasItsOwnKey pins the format apart from the user-tag fields. The
// executor once rode on "tag", the same key user tags are named by, and every
// consumer downstream inherited the ambiguity. The keys are the contract, so
// they are asserted here rather than left to whatever the struct tags happen to
// say.
func TestExecutorHasItsOwnKey(t *testing.T) {
	b, err := json.Marshal(Record{ID: "x", Cmd: "go test", Executor: "claude-code"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"executor":"claude-code"`)
	assert.NotContains(t, string(b), `"tag"`, "the executor must not be written under a tag key")

	// A user-tag record uses the tag_* keys and carries no executor.
	b, err = json.Marshal(Record{ID: "y", Type: TypeTag, TagName: "refactor", TargetID: "x"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"tag_name":"refactor"`)
	assert.NotContains(t, string(b), `"executor"`)

	// And the two survive together on one decode.
	var got Record
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"z","cmd":"make","executor":"cursor","tag_name":"wip","tag_op":"remove"}`), &got))
	assert.Equal(t, "cursor", got.Executor)
	assert.Equal(t, "wip", got.TagName)
	assert.Equal(t, TagOpRemove, got.TagOp)
}
