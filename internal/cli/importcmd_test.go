package cli

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/importer"
	"yore/internal/rec"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what it
// wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

func TestWarnUntimed(t *testing.T) {
	timed := []rec.Record{{Cmd: "ls", StartMs: 1_700_000_000_000}}
	untimed := []rec.Record{{Cmd: "ls"}, {Cmd: "cd /tmp"}}
	mixed := []rec.Record{{Cmd: "ls"}, {Cmd: "cd /tmp", StartMs: 1_700_000_000_000}}

	tests := []struct {
		name string
		src  importer.Source
		recs []rec.Record
		warn bool
	}{
		{"bash with no timestamps warns", importer.Source{Path: "/h/.bash_history", Format: "bash"}, untimed, true},
		{"bash with timestamps is quiet", importer.Source{Path: "/h/.bash_history", Format: "bash"}, timed, false},
		{"partly timestamped bash is quiet", importer.Source{Path: "/h/.bash_history", Format: "bash"}, mixed, false},
		{"empty bash file is quiet", importer.Source{Path: "/h/.bash_history", Format: "bash"}, nil, false},
		// Plain (non-extended) zsh lines are untimed too, but zsh has no
		// HISTTIMEFORMAT and the advice would be wrong, so it stays quiet.
		{"untimed zsh is quiet", importer.Source{Path: "/h/.zsh_history", Format: "zsh"}, untimed, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() { warnUntimed(tc.src, tc.recs) })
			if !tc.warn {
				require.Empty(t, out)
				return
			}
			require.Contains(t, out, tc.src.Path)
			require.Contains(t, out, "HISTTIMEFORMAT")
			require.Contains(t, out, "unknown time")
		})
	}
}
