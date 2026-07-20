// Package importer parses existing shell-history files (zsh and bash) into
// rec.Record values so they can be ingested into yore's store.
//
// Records are produced with deterministic ids (see rec.ImportID), so importing
// the same file twice yields identical ids and re-import is a downstream no-op.
// The producer leaves HostID, Hostname, and Seq empty; the local store fills
// those at ingest. Session is set to "import" and Exit/Cwd are left unset.
//
// Ordering: the importer preserves the original file order and does NOT sort.
// Callers that want a stable chronological order should note that imported
// records with StartMs == 0 (plain zsh lines and untimestamped bash entries)
// carry no timestamp and should sort before/oldest relative to timestamped
// records.
package importer

import (
	"io"
	"os"
	"path/filepath"
)

// Source is a discovered history file and the format it should be parsed with.
// Format is "zsh" or "bash".
type Source struct {
	Path   string
	Format string
}

// Auto returns the history files that exist under home, in a stable order.
// It checks the common locations: ~/.zsh_history, ~/.config/zsh/.zsh_history,
// ~/.zhistory (all zsh), and ~/.bash_history (bash). Only files that actually
// exist as regular files are returned. Resolving $HISTFILE is out of scope.
func Auto(home string) []Source {
	candidates := []Source{
		{Path: filepath.Join(home, ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".config", "zsh", ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".zhistory"), Format: "zsh"},
		{Path: filepath.Join(home, ".bash_history"), Format: "bash"},
	}
	var out []Source
	for _, c := range candidates {
		if fi, err := os.Stat(c.Path); err == nil && fi.Mode().IsRegular() {
			out = append(out, c)
		}
	}
	return out
}

// physicalLines splits raw file bytes into physical lines on '\n', dropping a
// trailing '\r' from each (so CRLF files parse cleanly). A trailing newline at
// end of file does not produce a spurious empty final line.
func physicalLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			lines = append(lines, dropCR(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, dropCR(b[start:]))
	}
	return lines
}

func dropCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
