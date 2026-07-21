package store

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/spool"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAppendMonotonicAndHostFields(t *testing.T) {
	s := openTemp(t)

	hn, _ := os.Hostname()
	require.NotEmpty(t, s.HostID(), "HostID empty after Open")
	if hn != "" {
		assert.Equal(t, hn, s.Hostname())
	}

	var last uint64
	for i := 0; i < 5; i++ {
		r, err := s.Append(rec.Record{Cmd: fmt.Sprintf("cmd-%d", i)})
		require.NoError(t, err)
		assert.Equal(t, last+1, r.Seq, "seq")
		last = r.Seq
		assert.NotEmpty(t, r.ID, "ID not generated")
		assert.Equal(t, s.HostID(), r.HostID, "record HostID not filled")
		assert.Equal(t, s.Hostname(), r.Hostname, "record Hostname not filled")
	}

	ls, _ := s.LastSeq()
	assert.EqualValues(t, 5, ls, "LastSeq")
	c, _ := s.Count()
	assert.Equal(t, 5, c, "Count")
}

func TestIdempotentReAppend(t *testing.T) {
	s := openTemp(t)

	first, err := s.Append(rec.Record{ID: "fixed", Cmd: "echo once"})
	require.NoError(t, err)

	added, err := s.AppendBatch([]rec.Record{
		{ID: "fixed", Cmd: "echo once"},
		{ID: "fixed", Cmd: "same id different cmd"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, added, "AppendBatch added, want 0 (all duplicate IDs)")

	// A duplicate single Append returns the originally stored record.
	got, err := s.Append(rec.Record{ID: "fixed", Cmd: "yet another"})
	require.NoError(t, err)
	assert.Equal(t, first.Seq, got.Seq, "dup Append returned wrong Seq")
	assert.Equal(t, first.Cmd, got.Cmd, "dup Append returned wrong Cmd")

	c, _ := s.Count()
	assert.Equal(t, 1, c, "Count")
}

func TestSecondOpenLocked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	_, err = Open(dir)
	require.ErrorIs(t, err, ErrLocked, "second Open")
}

func TestTombstoneMarksTargetAndAllExcludes(t *testing.T) {
	s := openTemp(t)

	_, err := s.Append(rec.Record{ID: "victim", Cmd: "secret command"})
	require.NoError(t, err)
	_, err = s.Append(rec.Record{ID: "keep", Cmd: "harmless"})
	require.NoError(t, err)
	del, err := s.Append(rec.Record{Type: rec.TypeDelete, TargetID: "victim", StartMs: 12345})
	require.NoError(t, err)
	require.Equal(t, rec.TypeDelete, del.Type, "tombstone not appended to stream")
	require.NotZero(t, del.Seq, "tombstone not appended to stream")

	// All excludes both the tombstone and the now-deleted target.
	all, err := s.All()
	require.NoError(t, err)
	require.Len(t, all, 1, "All, want only [keep]")
	assert.Equal(t, "keep", all[0].ID, "All, want only [keep]")

	// Since is the raw stream: target (deleted), keep, and the tombstone.
	raw, err := s.Since(0, 0)
	require.NoError(t, err)
	require.Len(t, raw, 3, "Since(0)")
	var sawTarget, sawTomb bool
	for _, r := range raw {
		if r.ID == "victim" {
			sawTarget = true
			assert.Equal(t, int64(12345), r.DeletedMs, "victim DeletedMs (from tombstone StartMs)")
		}
		if r.Type == rec.TypeDelete {
			sawTomb = true
		}
	}
	assert.True(t, sawTarget, "Since must include deleted target")
	assert.True(t, sawTomb, "Since must include tombstone")
}

func TestMarkDeleted(t *testing.T) {
	s := openTemp(t)

	_, err := s.Append(rec.Record{ID: "x", Cmd: "rm -rf /"})
	require.NoError(t, err)
	require.NoError(t, s.MarkDeleted("x", 999))

	all, _ := s.All()
	assert.Empty(t, all, "All after MarkDeleted")

	raw, _ := s.Since(0, 0)
	if assert.Len(t, raw, 1, "raw stream after MarkDeleted") {
		assert.Equal(t, int64(999), raw[0].DeletedMs, "raw stream DeletedMs")
	}

	assert.Error(t, s.MarkDeleted("does-not-exist", 0), "MarkDeleted on missing ID should error")
}

func TestSinceLimitAndStrictlyGreater(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 10; i++ {
		_, err := s.Append(rec.Record{Cmd: fmt.Sprintf("c%d", i)})
		require.NoError(t, err)
	}
	rows, err := s.Since(3, 4)
	require.NoError(t, err)
	require.Len(t, rows, 4, "Since(3,4)")
	assert.EqualValues(t, 4, rows[0].Seq, "first seq should be strictly greater than 3")
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTemp(t)
	v, _ := s.Meta("absent")
	assert.Empty(t, v, "Meta(absent)")

	require.NoError(t, s.SetMeta("k", "v"))

	v2, _ := s.Meta("k")
	assert.Equal(t, "v", v2, "Meta(k)")
}

func TestIngestSpoolEndToEnd(t *testing.T) {
	s := openTemp(t)
	sd := config.SpoolDir(s.Dir())

	const n = 20
	for i := 0; i < n; i++ {
		r := rec.Record{ID: fmt.Sprintf("s-%d", i), Cmd: fmt.Sprintf("cmd %d", i)}
		require.NoError(t, spool.Append(sd, r))
	}

	added, err := s.IngestSpool()
	require.NoError(t, err)
	require.Equal(t, n, added, "IngestSpool added")

	all, err := s.All()
	require.NoError(t, err)
	require.Len(t, all, n, "All")
	for _, r := range all {
		assert.Equal(t, s.HostID(), r.HostID, "ingested record missing host fields")
		assert.Equal(t, s.Hostname(), r.Hostname, "ingested record missing host fields")
	}

	// Spool is now empty; a second ingest is a no-op.
	added2, err := s.IngestSpool()
	assert.NoError(t, err, "second IngestSpool")
	assert.Equal(t, 0, added2, "second IngestSpool")
}

func TestBackupTo(t *testing.T) {
	s := openTemp(t)
	want := []rec.Record{
		{ID: "b1", Cmd: "echo one", StartMs: 1},
		{ID: "b2", Cmd: "echo two", StartMs: 2},
	}
	_, err := s.AppendBatch(want)
	require.NoError(t, err, "seed AppendBatch")

	// Snapshot to a standalone bbolt file in a fresh dir, at the path a Store
	// there would use, so Open can adopt it directly.
	backupDir := t.TempDir()
	f, err := os.Create(config.DBPath(backupDir))
	require.NoError(t, err, "create backup file")
	n, err := s.BackupTo(f)
	require.NoError(t, err, "BackupTo")
	require.NoError(t, f.Close(), "close backup file")
	assert.Positive(t, n, "BackupTo byte count")

	// The backup is a complete bbolt file: opening a Store on that dir must see
	// every record.
	restored, err := Open(backupDir)
	require.NoError(t, err, "Open restored")
	defer func() { _ = restored.Close() }()

	all, err := restored.All()
	require.NoError(t, err, "restored All")
	require.Len(t, all, len(want), "restored record count")
	got := map[string]string{}
	for _, r := range all {
		got[r.ID] = r.Cmd
	}
	assert.Equal(t, "echo one", got["b1"], "restored b1")
	assert.Equal(t, "echo two", got["b2"], "restored b2")
}

func BenchmarkAppend(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Append(rec.Record{Cmd: "benchmark command --flag value"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAll200k(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	const total = 200_000
	const chunk = 5_000
	batch := make([]rec.Record, 0, chunk)
	for i := 0; i < total; i += chunk {
		batch = batch[:0]
		for j := 0; j < chunk; j++ {
			batch = append(batch, rec.Record{
				ID:  fmt.Sprintf("r-%d", i+j),
				Cmd: "some representative shell command --with flags",
				Cwd: "/home/user/project",
			})
		}
		if _, err := s.AppendBatch(batch); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.All()
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != total {
			b.Fatalf("All returned %d rows, want %d", len(rows), total)
		}
	}
}
