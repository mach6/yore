package cli

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/rec"
)

// fakeDaemon listens on the daemon socket and signals on pokeCh the first time
// the capture path dials it, so a test can assert whether a poke happened. It is
// torn down when the test finishes.
func fakeDaemon(t *testing.T, dir string) <-chan struct{} {
	t.Helper()
	ln, err := net.Listen("unix", config.SocketPath(dir))
	require.NoError(t, err, "listen on daemon socket")
	t.Cleanup(func() { _ = ln.Close() })
	poked := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case poked <- struct{}{}:
			default:
			}
			_ = conn.Close()
		}
	}()
	return poked
}

// TestSpoolOnlyNoPoke is the core of the opt-in capture_spool_only mode: a
// capture must land in the spool but NEVER touch the daemon.
func TestSpoolOnlyNoPoke(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	require.NoError(t, config.Save(dir, config.Config{CaptureSpoolOnly: true}), "save config")

	poked := fakeDaemon(t, dir)
	spoolRecord(dir, rec.Record{Cmd: "echo spool-only"})

	select {
	case <-poked:
		assert.Fail(t, "spool-only mode must not poke the daemon")
	case <-time.After(200 * time.Millisecond):
	}

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "the record must still be spooled")
	assert.Equal(t, "echo spool-only", rows[0].Cmd)
}

// TestCaptureDoesPoke is the default: a capture nudges a running daemon so the
// record is ingested promptly.
func TestCaptureDoesPoke(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir)) // default config: CaptureSpoolOnly=false

	poked := fakeDaemon(t, dir)
	spoolRecord(dir, rec.Record{Cmd: "echo poke"})

	select {
	case <-poked:
	case <-time.After(2 * time.Second):
		assert.Fail(t, "default mode must poke a running daemon")
	}
}
