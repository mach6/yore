package spool

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

func glob(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	return m
}

func TestAppendDrainRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	want := []rec.Record{
		{ID: "a", Cmd: "ls -la", Cwd: "/tmp"},
		{ID: "b", Cmd: "echo hi", Exit: rec.IntPtr(0)},
		{Type: rec.TypeDelete, TargetID: "a"},
	}
	for _, r := range want {
		require.NoError(t, Append(dir, r), "Append")
	}

	var got []rec.Record
	n, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err, "Drain")
	require.Equal(t, len(want), n, "drained count")
	require.Len(t, got, len(want), "drained records")
	for i := range want {
		assert.Equalf(t, want[i].ID, got[i].ID, "record %d ID", i)
		assert.Equalf(t, want[i].Cmd, got[i].Cmd, "record %d Cmd", i)
		assert.Equalf(t, want[i].TargetID, got[i].TargetID, "record %d TargetID", i)
	}
	assert.Empty(t, glob(t, dir), "spool files remain after drain")

	// A second drain finds nothing.
	n2, err := Drain(dir, func(rec.Record) error { return nil })
	assert.NoError(t, err, "second Drain")
	assert.Equal(t, 0, n2, "second Drain count")
}

func TestDrainTornFinalLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, Append(dir, rec.Record{ID: "ok", Cmd: "good"}))

	// Simulate a crash mid-write: a valid line already fsynced, then a
	// partial JSON fragment with no trailing newline.
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"id":"torn","cmd":"half`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	var got []rec.Record
	n, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err, "Drain")
	require.Equal(t, 1, n, "Drain count")
	require.Len(t, got, 1, "Drain records")
	require.Equal(t, "ok", got[0].ID, "Drain record id")
	assert.Empty(t, glob(t, dir), "file not deleted after tolerating torn line")
}

func TestDrainFnErrorKeepsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	for _, c := range []string{"one", "two", "three"} {
		require.NoError(t, Append(dir, rec.Record{ID: c, Cmd: c}))
	}

	boom := errors.New("boom")
	calls := 0
	n, err := Drain(dir, func(rec.Record) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	require.ErrorIs(t, err, boom, "Drain err")
	require.Equal(t, 1, n, "Drain count (only the first was handed off)")
	require.Len(t, glob(t, dir), 1, "file removed despite fn error")

	// Retrying with a passing fn re-reads the whole file (nothing on disk was
	// consumed), so all three come through; downstream dedupes by ID.
	var got []rec.Record
	n2, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, n2, "retry drained count")
	assert.Empty(t, glob(t, dir), "files remain after successful retry")
}

func TestDrainMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	n, err := Drain(missing, func(rec.Record) error {
		require.Fail(t, "fn must not be called for a missing dir")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 0, n)
}
