package daemon

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
)

// listenSocket binds a unix listener at dir's socket path (closed on cleanup)
// and returns the FileInfo of the file it created.
func listenSocket(t *testing.T, dir string) os.FileInfo {
	t.Helper()
	ln, err := net.Listen("unix", config.SocketPath(dir))
	require.NoError(t, err, "listen")
	t.Cleanup(func() { _ = ln.Close() })
	fi, err := os.Stat(config.SocketPath(dir))
	require.NoError(t, err, "stat socket")
	return fi
}

func TestSocketLive(t *testing.T) {
	tests := []struct {
		name string
		// mutate runs after the socket is bound; it returns the sockInfo the
		// server should carry into the check.
		mutate func(t *testing.T, dir string, bound os.FileInfo) os.FileInfo
		want   bool
	}{
		{
			name:   "unknown identity never shuts down",
			mutate: func(*testing.T, string, os.FileInfo) os.FileInfo { return nil },
			want:   true,
		},
		{
			name:   "socket still present",
			mutate: func(_ *testing.T, _ string, bound os.FileInfo) os.FileInfo { return bound },
			want:   true,
		},
		{
			name: "socket unlinked",
			mutate: func(t *testing.T, dir string, bound os.FileInfo) os.FileInfo {
				require.NoError(t, os.Remove(config.SocketPath(dir)), "remove socket")
				return bound
			},
			want: false,
		},
		{
			name: "socket replaced by another file",
			mutate: func(t *testing.T, dir string, bound os.FileInfo) os.FileInfo {
				require.NoError(t, os.Remove(config.SocketPath(dir)), "remove socket")
				require.NoError(t, os.WriteFile(config.SocketPath(dir), nil, 0o600), "replace socket")
				return bound
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bound := listenSocket(t, dir)
			s := &server{dir: dir, sockInfo: tc.mutate(t, dir, bound)}
			assert.Equal(t, tc.want, s.socketLive())
		})
	}
}

// TestSocketWatchTriggersShutdown is the regression guard: a daemon whose
// socket file is unlinked out from under it must stop, releasing the store
// lock, instead of serving forever on an unreachable listener.
func TestSocketWatchTriggersShutdown(t *testing.T) {
	dir := t.TempDir()
	bound := listenSocket(t, dir)

	s := &server{
		dir:         dir,
		sockInfo:    bound,
		sockCheck:   10 * time.Millisecond,
		done:        make(chan struct{}),
		shutdownReq: make(chan struct{}, 1),
	}
	s.wg.Add(1)
	go s.socketWatchLoop()
	defer close(s.done)

	// While the socket is intact the watchdog must stay quiet.
	select {
	case <-s.shutdownReq:
		require.FailNow(t, "watchdog asked to shut down while the socket was healthy")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, os.Remove(config.SocketPath(dir)), "remove socket")

	select {
	case <-s.shutdownReq:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog did not request shutdown after the socket vanished")
	}
	s.wg.Wait()
}
