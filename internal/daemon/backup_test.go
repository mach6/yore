package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/store"
)

func TestBackupsToPrune(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		keep  int
		want  []string
	}{
		{
			name:  "keep newest 2, delete the rest (oldest first)",
			names: []string{"data-100.db", "data-300.db", "data-200.db"},
			keep:  2,
			want:  []string{"data-100.db"},
		},
		{
			name:  "keep exceeds count is a no-op",
			names: []string{"data-1.db", "data-2.db"},
			keep:  5,
			want:  nil,
		},
		{
			name:  "keep equals count is a no-op",
			names: []string{"data-1.db", "data-2.db"},
			keep:  2,
			want:  nil,
		},
		{
			name:  "non-matching names are ignored",
			names: []string{"data-2.db", "data-1.db", ".tmp-9.db", "corpus.snap", "data-bad.db"},
			keep:  1,
			want:  []string{"data-1.db"},
		},
		{
			name:  "keep zero deletes all matching",
			names: []string{"data-5.db", "data-9.db"},
			keep:  0,
			want:  []string{"data-5.db", "data-9.db"},
		},
		{
			name:  "empty input",
			names: nil,
			keep:  3,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, backupsToPrune(tt.names, tt.keep))
		})
	}
}

// TestDoBackup exercises the full write+rename+prune path without a running
// daemon: it constructs a minimal server over a real store and calls doBackup
// directly (as backupLoop would).
func TestDoBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	require.NoError(t, err, "store.Open")
	defer func() { _ = st.Close() }()
	_, err = st.Append(rec.Record{ID: "x", Cmd: "echo hi"})
	require.NoError(t, err, "seed Append")

	s := &server{dir: dir, store: st, cfg: config.Config{BackupKeep: 2}}

	// Three backups; only the newest 2 must survive, and no temp files linger.
	for i := 0; i < 3; i++ {
		s.doBackup()
		time.Sleep(2 * time.Millisecond) // distinct UnixMilli filenames
	}

	backups, err := filepath.Glob(filepath.Join(config.BackupDir(dir), "data-*.db"))
	require.NoError(t, err, "glob backups")
	assert.Len(t, backups, 2, "backups after prune to keep=2")

	tmps, err := filepath.Glob(filepath.Join(config.BackupDir(dir), ".tmp-*.db"))
	require.NoError(t, err, "glob temps")
	assert.Empty(t, tmps, "temp files left behind")

	// Each surviving backup is a valid, openable bbolt db with the record.
	newDir := t.TempDir()
	require.NoError(t, copyFile(backups[0], config.DBPath(newDir)), "copy backup")
	restored, err := store.Open(newDir)
	require.NoError(t, err, "open restored backup")
	defer func() { _ = restored.Close() }()
	all, err := restored.All()
	require.NoError(t, err, "restored All")
	require.Len(t, all, 1, "restored record count")
	assert.Equal(t, "echo hi", all[0].Cmd, "restored cmd")
}

// TestBackupLoopRuns covers the loop goroutine: a short interval makes the
// ticker fire well before the 2s first-timer, producing a backup, and closing
// done stops the loop.
func TestBackupLoopRuns(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	require.NoError(t, err, "store.Open")
	defer func() { _ = st.Close() }()
	_, err = st.Append(rec.Record{ID: "x", Cmd: "echo hi"})
	require.NoError(t, err, "seed Append")

	s := &server{
		dir:   dir,
		store: st,
		cfg:   config.Config{BackupInterval: "20ms", BackupKeep: 3},
		done:  make(chan struct{}),
	}
	s.wg.Add(1)
	go s.backupLoop()

	deadline := time.Now().Add(2 * time.Second)
	var backups []string
	for time.Now().Before(deadline) {
		backups, _ = filepath.Glob(filepath.Join(config.BackupDir(dir), "data-*.db"))
		if len(backups) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.NotEmpty(t, backups, "backupLoop produced no backup")

	close(s.done)
	s.wg.Wait() // loop must observe done and return
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
