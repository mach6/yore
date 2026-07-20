package importer

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"yore/internal/rec"
)

const testHost = "01ABCDEFGHIJKLMNOPQRSTUV00"

// want is a compact description of an expected record.
type want struct {
	startMs int64
	dur     *int64 // nil = no duration
	cmd     string
}

func check(t *testing.T, got []rec.Record, host string, wants []want) {
	t.Helper()
	if len(got) != len(wants) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(wants), got)
	}
	for i, w := range wants {
		r := got[i]
		if r.Cmd != w.cmd {
			t.Errorf("record %d cmd = %q, want %q", i, r.Cmd, w.cmd)
		}
		if r.StartMs != w.startMs {
			t.Errorf("record %d startMs = %d, want %d", i, r.StartMs, w.startMs)
		}
		if !reflect.DeepEqual(r.DurMs, w.dur) {
			t.Errorf("record %d dur = %v, want %v", i, ptr(r.DurMs), ptr(w.dur))
		}
		if r.Session != "import" {
			t.Errorf("record %d session = %q, want %q", i, r.Session, "import")
		}
		if r.Exit != nil {
			t.Errorf("record %d exit = %v, want nil", i, *r.Exit)
		}
		if r.Cwd != "" || r.HostID != "" || r.Hostname != "" {
			t.Errorf("record %d expected empty cwd/host fields, got %+v", i, r)
		}
		if r.ID != rec.ImportID(host, w.startMs, w.cmd) {
			t.Errorf("record %d id = %q, not deterministic ImportID", i, r.ID)
		}
	}
}

func ptr(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func TestZsh(t *testing.T) {
	b, err := os.ReadFile("testdata/zsh_history")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Zsh(strings.NewReader(string(b)), testHost)
	if err != nil {
		t.Fatal(err)
	}
	check(t, got, testHost, []want{
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
			if err != nil {
				t.Fatal(err)
			}
			check(t, got, testHost, tc.wants)
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
	if err != nil {
		t.Fatal(err)
	}
	check(t, got, testHost, []want{
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
	if err != nil {
		t.Fatal(err)
	}
	got, err := Bash(strings.NewReader(string(b)), testHost)
	if err != nil {
		t.Fatal(err)
	}
	check(t, got, testHost, []want{
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
			if err != nil {
				t.Fatal(err)
			}
			check(t, got, testHost, tc.wants)
		})
	}
}

// TestDeterminism verifies re-import is stable and host-scoped.
func TestDeterminism(t *testing.T) {
	b, err := os.ReadFile("testdata/zsh_history")
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := Zsh(strings.NewReader(string(b)), testHost)
	a2, _ := Zsh(strings.NewReader(string(b)), testHost)
	if len(a1) != len(a2) || len(a1) == 0 {
		t.Fatalf("unexpected lengths %d, %d", len(a1), len(a2))
	}
	other := "01ZZZZZZZZZZZZZZZZZZZZZZZZZ"
	a3, _ := Zsh(strings.NewReader(string(b)), other)
	for i := range a1 {
		if a1[i].ID != a2[i].ID {
			t.Errorf("record %d: same host produced different ids %q vs %q", i, a1[i].ID, a2[i].ID)
		}
		if a1[i].ID == a3[i].ID {
			t.Errorf("record %d: different hosts produced same id %q", i, a1[i].ID)
		}
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
	if err := os.Mkdir(filepath.Join(home, ".zhistory"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Auto(home)
	want := []Source{
		{Path: filepath.Join(home, ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".config", "zsh", ".zsh_history"), Format: "zsh"},
		{Path: filepath.Join(home, ".bash_history"), Format: "bash"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Auto() = %+v, want %+v", got, want)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
