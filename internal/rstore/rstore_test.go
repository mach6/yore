package rstore

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/wire"
)

// blobs builds n sealed records for a host, seq 1..n, with recognizable bodies.
func blobs(n int) []wire.PullRecord {
	out := make([]wire.PullRecord, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, wire.PullRecord{
			Seq:   uint64(i),
			ID:    string(rune('a'+i%26)) + "-id",
			KeyID: "k1",
			Blob:  []byte{byte(i)},
		})
	}
	return out
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestAppendAndReadBack is the core promise: what went in comes back out in seq
// order, with the cursor persisted alongside it.
func TestAppendAndReadBack(t *testing.T) {
	s := open(t)

	trimmed, err := s.Append("h1", blobs(3), 3, 0)
	require.NoError(t, err)
	assert.Zero(t, trimmed, "no trimming with keep=0")

	got, err := s.Records("h1")
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, r := range got {
		assert.Equal(t, uint64(i+1), r.Seq, "records come back in ascending seq")
		assert.Equal(t, []byte{byte(i + 1)}, r.Blob, "the sealed blob round-trips byte for byte")
	}

	cursors, err := s.Cursors()
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{"h1": 3}, cursors)

	hosts, err := s.Hosts()
	require.NoError(t, err)
	assert.Equal(t, []string{"h1"}, hosts)
}

// TestCursorSurvivesReopen is the whole point of the package: a new process
// resumes from where the last one stopped instead of re-pulling from seq 0.
func TestCursorSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	_, err = s.Append("h1", blobs(5), 5, 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	cursors, err := reopened.Cursors()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), cursors["h1"], "the pull position outlives the process")

	got, err := reopened.Records("h1")
	require.NoError(t, err)
	assert.Len(t, got, 5, "the ciphertext outlives the process too")
}

// TestTrimKeepsNewest bounds retention: the oldest records go, the newest stay,
// and the cursor does NOT rewind (trimmed records must never be re-fetched).
func TestTrimKeepsNewest(t *testing.T) {
	s := open(t)

	trimmed, err := s.Append("h1", blobs(10), 10, 4)
	require.NoError(t, err)
	assert.Equal(t, 6, trimmed, "10 appended, 4 kept")

	got, err := s.Records("h1")
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, uint64(7), got[0].Seq, "the newest 4 are the ones retained")
	assert.Equal(t, uint64(10), got[3].Seq)

	cursors, err := s.Cursors()
	require.NoError(t, err)
	assert.Equal(t, uint64(10), cursors["h1"], "trimming must not rewind the cursor")
}

// TestTrimAcrossAppends checks the bound holds when a host's stream grows over
// many cycles, not just in one oversized batch.
func TestTrimAcrossAppends(t *testing.T) {
	s := open(t)
	for i := range 5 {
		batch := []wire.PullRecord{{Seq: uint64(i + 1), ID: "x", KeyID: "k", Blob: []byte{byte(i)}}}
		_, err := s.Append("h1", batch, uint64(i+1), 3)
		require.NoError(t, err)
	}
	got, err := s.Records("h1")
	require.NoError(t, err)
	require.Len(t, got, 3, "retention holds across cycles, not just within one")
	assert.Equal(t, uint64(3), got[0].Seq)
	assert.Equal(t, uint64(5), got[2].Seq)
}

// TestPerHostIsolation: hosts are independent streams with independent cursors.
func TestPerHostIsolation(t *testing.T) {
	s := open(t)
	_, err := s.Append("h1", blobs(2), 2, 0)
	require.NoError(t, err)
	_, err = s.Append("h2", blobs(4), 4, 0)
	require.NoError(t, err)

	h1, err := s.Records("h1")
	require.NoError(t, err)
	h2, err := s.Records("h2")
	require.NoError(t, err)
	assert.Len(t, h1, 2)
	assert.Len(t, h2, 4)

	cursors, err := s.Cursors()
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{"h1": 2, "h2": 4}, cursors)
}

// TestReset wipes everything: switching sync servers must not leave the old
// group's ciphertext behind for the new one's keys to fail on.
func TestReset(t *testing.T) {
	s := open(t)
	_, err := s.Append("h1", blobs(3), 3, 0)
	require.NoError(t, err)
	require.NoError(t, s.Reset())

	hosts, err := s.Hosts()
	require.NoError(t, err)
	assert.Empty(t, hosts, "no streams survive a reset")
	cursors, err := s.Cursors()
	require.NoError(t, err)
	assert.Empty(t, cursors, "no cursors survive a reset")

	// Still usable afterwards.
	_, err = s.Append("h1", blobs(1), 1, 0)
	require.NoError(t, err)
}

// TestAppendIsIdempotent: re-appending an already-cached seq is harmless, which
// is what makes an interrupted sync cycle safe to repeat.
func TestAppendIsIdempotent(t *testing.T) {
	s := open(t)
	_, err := s.Append("h1", blobs(3), 3, 0)
	require.NoError(t, err)
	_, err = s.Append("h1", blobs(3), 3, 0)
	require.NoError(t, err)

	got, err := s.Records("h1")
	require.NoError(t, err)
	assert.Len(t, got, 3, "a repeated batch must not duplicate records")
}

func TestUnknownHostIsEmpty(t *testing.T) {
	s := open(t)
	got, err := s.Records("nobody")
	require.NoError(t, err)
	assert.Empty(t, got, "an unknown host reads as empty, not an error")
}

func TestAppendRejectsEmptyHost(t *testing.T) {
	s := open(t)
	_, err := s.Append("", blobs(1), 1, 0)
	require.Error(t, err, "an empty host id would collide every stream into one bucket")
}

func TestOpenIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })

	_, err = Open(dir)
	require.ErrorIs(t, err, ErrLocked, "a second opener must be reported, not blocked forever")
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	require.NoError(t, Remove(dir))
	require.NoError(t, Remove(dir), "removing an absent cache is not an error")
	assert.Equal(t, filepath.Join(dir, "remote.db"), Path(dir))
}
