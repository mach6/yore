package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

const testHost = "01ABCDEFGHIJKLMNOPQRSTUV00"

// want is a compact description of an expected record.
type want struct {
	startMs int64
	dur     *int64 // nil = no duration
	cmd     string
}

// check compares got against wants field by field. It uses assert (rather
// than require) for the per-record checks so that a mismatch on one field or
// one record doesn't hide mismatches on the others in the same run — but the
// length check stays a require since indexing got[i] below depends on it.
func check(t *testing.T, got []rec.Record, wants []want) {
	t.Helper()
	require.Len(t, got, len(wants), "got %d records, want %d: %+v", len(got), len(wants), got)
	for i, w := range wants {
		r := got[i]
		assert.Equal(t, w.cmd, r.Cmd, "record %d cmd", i)
		assert.Equal(t, w.startMs, r.StartMs, "record %d startMs", i)
		assert.Equal(t, w.dur, r.DurMs, "record %d dur", i)
		assert.Equal(t, "import", r.Session, "record %d session", i)
		assert.Nil(t, r.Exit, "record %d exit", i)
		assert.Empty(t, r.Cwd, "record %d cwd", i)
		assert.Empty(t, r.HostID, "record %d hostID", i)
		assert.Empty(t, r.Hostname, "record %d hostname", i)
		assert.Equal(t, rec.ImportID(testHost, w.startMs, w.cmd), r.ID, "record %d id not deterministic ImportID", i)
	}
}

func TestZsh(t *testing.T) {
	b, err := os.ReadFile("testdata/zsh_history")
	require.NoError(t, err)
	got, err := Zsh(strings.NewReader(string(b)), testHost)
	require.NoError(t, err)
	check(t, got, []want{
		{startMs: 1600000000000, dur: rec.Int64Ptr(5000), cmd: "echo hello"},
		{startMs: 1600000100000, dur: rec.Int64Ptr(0), cmd: "git status"},
		{startMs: 0, dur: nil, cmd: "plain command without timestamp"},
		{startMs: 0, dur: nil, cmd: ": > /tmp/truncate"},
		{startMs: 1600000200000, dur: rec.Int64Ptr(12000), cmd: "echo line1\nline2\nline3"},
		{startMs: 1600000300000, dur: rec.Int64Ptr(0), cmd: "ls -la"},
	})
}

// TestZshUnit exercises the line parser and continuation joiner directly.
func TestZshUnit(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		wants []want
	}{
		{
			name:  "extended elapsed positive",
			in:    ": 1700000000:42;make build\n",
			wants: []want{{startMs: 1700000000000, dur: rec.Int64Ptr(42000), cmd: "make build"}},
		},
		{
			name:  "extended elapsed zero keeps pointer",
			in:    ": 1700000000:0;true\n",
			wants: []want{{startMs: 1700000000000, dur: rec.Int64Ptr(0), cmd: "true"}},
		},
		{
			name:  "plain line no timestamp",
			in:    "echo plain\n",
			wants: []want{{startMs: 0, dur: nil, cmd: "echo plain"}},
		},
		{
			name:  "colon command not misparsed as extended",
			in:    ": > file\n",
			wants: []want{{startMs: 0, dur: nil, cmd: ": > file"}},
		},
		{
			name:  "empty and whitespace lines skipped",
			in:    "\n   \n: 1700000000:0;echo ok\n\n",
			wants: []want{{startMs: 1700000000000, dur: rec.Int64Ptr(0), cmd: "echo ok"}},
		},
		{
			name:  "no trailing newline",
			in:    ": 1700000000:1;echo last",
			wants: []want{{startMs: 1700000000000, dur: rec.Int64Ptr(1000), cmd: "echo last"}},
		},
		{
			name:  "multiline continuation reconstructs newlines",
			in:    ": 1700000000:0;for i in 1 2\\\ndo echo $i\\\ndone\n",
			wants: []want{{startMs: 1700000000000, dur: rec.Int64Ptr(0), cmd: "for i in 1 2\ndo echo $i\ndone"}},
		},
		{
			name:  "negative elapsed yields nil duration",
			in:    ": 1700000000:-3;weird\n",
			wants: []want{{startMs: 1700000000000, dur: nil, cmd: "weird"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Zsh(strings.NewReader(tc.in), testHost)
			require.NoError(t, err)
			check(t, got, tc.wants)
		})
	}
}

// TestZshMetafied constructs metafied bytes from a known UTF-8 string (the same
// way zsh writes non-ASCII commands) and asserts an exact round-trip.
func TestZshMetafied(t *testing.T) {
	const s = "echo café 日本語 🎉 λ"
	line := append([]byte(": 1600000000:0;"), metafy([]byte(s))...)
	line = append(line, '\n')

	got, err := Zsh(strings.NewReader(string(line)), testHost)
	require.NoError(t, err)
	check(t, got, []want{
		{startMs: 1600000000000, dur: rec.Int64Ptr(0), cmd: s},
	})
}

// metafy mirrors zsh's history metafication: every byte with the high bit set
// (which includes all non-ASCII UTF-8 bytes) is written as (0x83, b^0x20).
func metafy(b []byte) []byte {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		if c&0x80 != 0 {
			out = append(out, metaChar, c^metaShift)
			continue
		}
		out = append(out, c)
	}
	return out
}

func TestBash(t *testing.T) {
	b, err := os.ReadFile("testdata/bash_history")
	require.NoError(t, err)
	got, err := Bash(strings.NewReader(string(b)), testHost)
	require.NoError(t, err)
	check(t, got, []want{
		{startMs: 1600000000000, cmd: "echo first"},
		{startMs: 0, cmd: "echo no timestamp"},
		{startMs: 1600000200000, cmd: "echo last wins"}, // consecutive ts, last wins
		{startMs: 0, cmd: "#notatimestamp comment"},     // literal comment command
		{startMs: 1600000300000, cmd: "git status"},
		{startMs: 0, cmd: "echo after blank"},
	})
}

func TestBashUnit(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		wants []want
	}{
		{
			name:  "timestamped and untimestamped mix",
			in:    "#1600000000\nls\necho untimed\n",
			wants: []want{{startMs: 1600000000000, cmd: "ls"}, {startMs: 0, cmd: "echo untimed"}},
		},
		{
			name:  "hash alone is a command",
			in:    "#\n",
			wants: []want{{startMs: 0, cmd: "#"}},
		},
		{
			name:  "hash with trailing text is a command",
			in:    "#1600 not a ts\n",
			wants: []want{{startMs: 0, cmd: "#1600 not a ts"}},
		},
		{
			name:  "consecutive timestamps last wins",
			in:    "#111\n#222\n#333\ncmd\n",
			wants: []want{{startMs: 333000, cmd: "cmd"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bash(strings.NewReader(tc.in), testHost)
			require.NoError(t, err)
			check(t, got, tc.wants)
		})
	}
}

// TestDeterminism verifies re-import is stable and host-scoped.
func TestDeterminism(t *testing.T) {
	b, err := os.ReadFile("testdata/zsh_history")
	require.NoError(t, err)
	a1, _ := Zsh(strings.NewReader(string(b)), testHost)
	a2, _ := Zsh(strings.NewReader(string(b)), testHost)
	require.Equal(t, len(a1), len(a2))
	require.NotEmpty(t, a1)
	other := "01ZZZZZZZZZZZZZZZZZZZZZZZZZ"
	a3, _ := Zsh(strings.NewReader(string(b)), other)
	for i := range a1 {
		assert.Equal(t, a1[i].ID, a2[i].ID, "record %d: same host produced different ids", i)
		assert.NotEqual(t, a1[i].ID, a3[i].ID, "record %d: different hosts produced same id", i)
	}
}

func TestAuto(t *testing.T) {
	home := t.TempDir()
	// Present: ~/.zsh_history, ~/.config/zsh/.zsh_history, ~/.bash_history.
	// Absent: ~/.zhistory (a candidate) and an unrelated file.
	mustWrite(t, filepath.Join(home, ".zsh_history"), "echo a\n")
	mustWrite(t, filepath.Join(home, ".config", "zsh", ".zsh_history"), "echo b\n")
	mustWrite(t, filepath.Join(home, ".bash_history"), "echo c\n")
	mustWrite(t, filepath.Join(home, "unrelated"), "x\n")
	// A directory named like a candidate must be ignored (not a regular file).
	require.NoError(t, os.Mkdir(filepath.Join(home, ".zhistory"), 0o755))

	got := Auto(home)
	want := []Source{
		{Path: filepath.Join(home, ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".config", "zsh", ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".bash_history"), Format: "bash"},
	}
	require.Equal(t, want, got)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
