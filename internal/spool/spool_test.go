package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	// A file left by an older yore (or a truncated disk): one valid line
	// already fsynced, then a partial JSON fragment with no trailing newline.
	// Append itself can no longer produce this (it renames a complete file
	// into place) but Drain still has to get past it rather than retry it
	// forever.
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("{\"id\":\"ok\",\"cmd\":\"good\"}\n" + `{"id":"torn","cmd":"half`)
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
	require.Len(t, glob(t, dir), 2, "files consumed despite fn error")

	// Retrying with a passing fn delivers exactly what was left: the record fn
	// rejected and the one after it. The record already handed off is gone.
	var got []rec.Record
	n2, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, n2, "retry drained count")
	require.Len(t, got, 2, "retry records")
	assert.Equal(t, []string{"two", "three"}, []string{got[0].ID, got[1].ID}, "retry order")
	assert.Empty(t, glob(t, dir), "files remain after successful retry")
}

// TestAppendPublishesAtomically pins the property the whole package rests on:
// a record becomes visible to Drain only once it is complete on disk. A write
// in flight is a ".tmp" file, which a drain must neither read nor remove: the
// bug this replaced let a drain delete a file the writer had created but not
// yet written to, and the record vanished with no error anywhere.
func TestAppendPublishesAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, Append(dir, rec.Record{ID: "done", Cmd: "finished"}))

	require.Len(t, glob(t, dir), 1, "one published file")
	tmps, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	require.NoError(t, err)
	require.Empty(t, tmps, "a completed Append leaves no temp file")

	// A write in flight, mid-Append: present, named .tmp, not yet published.
	inflight := filepath.Join(dir, "99999999999999999999-1-1.tmp")
	require.NoError(t, os.WriteFile(inflight, []byte("{\"id\":\"inflight\"}\n"), 0o600))

	var got []rec.Record
	n, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err, "Drain")
	require.Equal(t, 1, n, "Drain must not consume a write in flight")
	require.Equal(t, "done", got[0].ID, "drained record")
	require.FileExists(t, inflight, "Drain must not remove a write in flight")
}

// TestDrainSweepsAbandonedTemps: a process that dies between creating its temp
// file and renaming it leaves residue whose record was never durable. It is
// swept once it is old enough that no writer can still be holding it.
func TestDrainSweepsAbandonedTemps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	fresh := filepath.Join(dir, "00000000000000000001-1-1.tmp")
	stale := filepath.Join(dir, "00000000000000000002-1-1.tmp")
	require.NoError(t, os.WriteFile(fresh, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(stale, []byte("{}\n"), 0o600))
	old := time.Now().Add(-2 * tmpTTL)
	require.NoError(t, os.Chtimes(stale, old, old))

	_, err := Drain(dir, func(rec.Record) error { return nil })
	require.NoError(t, err, "Drain")

	require.FileExists(t, fresh, "a recent temp may be a write in flight")
	require.NoFileExists(t, stale, "an abandoned temp is swept")
}

// TestConcurrentDrainLosesNothing is the regression test for the lost records:
// writers and a drainer running flat out against the same directory, where
// every record must end up either delivered or still on disk; never neither.
func TestConcurrentDrainLosesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	const writers, perWriter = 8, 40

	var delivered atomic.Int64
	stop := make(chan struct{})
	var drainers sync.WaitGroup
	drainers.Go(func() {
		for {
			n, err := Drain(dir, func(rec.Record) error { return nil })
			if err == nil {
				delivered.Add(int64(n))
			}
			select {
			case <-stop:
				// One last pass, so anything written after the previous drain
				// is accounted for rather than left to the final count.
				n, err := Drain(dir, func(rec.Record) error { return nil })
				if err == nil {
					delivered.Add(int64(n))
				}
				return
			default:
			}
		}
	})

	var ws sync.WaitGroup
	for w := range writers {
		ws.Go(func() {
			for i := range perWriter {
				require.NoError(t, Append(dir, rec.Record{ID: fmt.Sprintf("w%d-%d", w, i), Cmd: "echo hi"}))
			}
		})
	}
	ws.Wait()
	close(stop)
	drainers.Wait()

	left, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.Equal(t, int64(writers*perWriter), delivered.Load()+int64(len(left)),
		"every record must be delivered or still spooled; none lost")
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
