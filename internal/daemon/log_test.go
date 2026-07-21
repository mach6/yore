package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	// max 20 bytes, keep 2 old segments.
	w, err := newRotatingWriter(path, 20, 2)
	require.NoError(t, err, "newRotatingWriter")

	// Each line is 10 bytes; six writes force multiple rotations.
	lines := []string{"aaaaaaaaa\n", "bbbbbbbbb\n", "ccccccccc\n", "ddddddddd\n", "eeeeeeeee\n", "fffffffff\n"}
	for _, ln := range lines {
		n, werr := w.Write([]byte(ln))
		require.NoError(t, werr, "Write")
		require.Equal(t, len(ln), n, "short write")
	}
	require.NoError(t, w.Close(), "Close")
	assert.NoError(t, w.Close(), "second Close is a no-op")

	// keep=2 means daemon.log plus at most .1 and .2 — never .3.
	_, err = os.Stat(path)
	require.NoError(t, err, "live log must exist")
	_, err = os.Stat(path + ".1")
	require.NoError(t, err, "daemon.log.1 must exist after rotation")
	_, err = os.Stat(path + ".2")
	require.NoError(t, err, "daemon.log.2 must exist after rotation")
	_, err = os.Stat(path + ".3")
	assert.True(t, os.IsNotExist(err), "daemon.log.3 must not exist (keep=2)")

	// No byte is dropped or split: the newest content and the retained segments
	// together must contain every line, and the live log holds the last write.
	live, err := os.ReadFile(path)
	require.NoError(t, err, "read live log")
	assert.Contains(t, string(live), "fffffffff", "live log holds the newest write")

	total := ""
	for _, p := range []string{path + ".2", path + ".1", path} {
		b, rerr := os.ReadFile(p)
		require.NoError(t, rerr, "read segment %s", p)
		total += string(b)
	}
	for _, ln := range lines {
		assert.Contains(t, total, ln, "line missing across segments")
	}
}

func TestRotatingWriterSeedsSizeFromStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o600), "seed existing log")

	// max 12: the existing 10 bytes are seeded, so a 5-byte write overflows and
	// rotates rather than appending onto a file already near the cap.
	w, err := newRotatingWriter(path, 12, 1)
	require.NoError(t, err, "newRotatingWriter")
	_, err = w.Write([]byte("hello"))
	require.NoError(t, err, "Write")
	require.NoError(t, w.Close(), "Close")

	rotated, err := os.ReadFile(path + ".1")
	require.NoError(t, err, "rotated segment must exist")
	assert.Equal(t, "0123456789", string(rotated), "rotated segment holds seeded content")
	live, err := os.ReadFile(path)
	require.NoError(t, err, "live log")
	assert.Equal(t, "hello", string(live), "live log holds the new write")
}
