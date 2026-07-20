package spool

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"yore/internal/rec"
)

func glob(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
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
		if err := Append(dir, r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var got []rec.Record
	n, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != len(want) || len(got) != len(want) {
		t.Fatalf("drained n=%d got=%d, want %d", n, len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Cmd != want[i].Cmd || got[i].TargetID != want[i].TargetID {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if left := glob(t, dir); len(left) != 0 {
		t.Errorf("spool files remain after drain: %v", left)
	}

	// A second drain finds nothing.
	n2, err := Drain(dir, func(rec.Record) error { return nil })
	if err != nil || n2 != 0 {
		t.Errorf("second Drain = (%d,%v), want (0,nil)", n2, err)
	}
}

func TestDrainTornFinalLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := Append(dir, rec.Record{ID: "ok", Cmd: "good"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-write: a valid line already fsynced, then a
	// partial JSON fragment with no trailing newline.
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"torn","cmd":"half`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var got []rec.Record
	n, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 1 || len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("Drain got n=%d %+v, want 1 record id=ok", n, got)
	}
	if left := glob(t, dir); len(left) != 0 {
		t.Errorf("file not deleted after tolerating torn line: %v", left)
	}
}

func TestDrainFnErrorKeepsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	for _, c := range []string{"one", "two", "three"} {
		if err := Append(dir, rec.Record{ID: c, Cmd: c}); err != nil {
			t.Fatal(err)
		}
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
	if !errors.Is(err, boom) {
		t.Fatalf("Drain err = %v, want boom", err)
	}
	if n != 1 {
		t.Fatalf("Drain count = %d, want 1 (only the first was handed off)", n)
	}
	if left := glob(t, dir); len(left) != 1 {
		t.Fatalf("file removed despite fn error: %v", left)
	}

	// Retrying with a passing fn re-reads the whole file (nothing on disk was
	// consumed), so all three come through; downstream dedupes by ID.
	var got []rec.Record
	n2, err := Drain(dir, func(r rec.Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 3 {
		t.Fatalf("retry drained %d, want 3", n2)
	}
	if left := glob(t, dir); len(left) != 0 {
		t.Errorf("files remain after successful retry: %v", left)
	}
}

func TestDrainMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	n, err := Drain(missing, func(rec.Record) error {
		t.Fatal("fn must not be called for a missing dir")
		return nil
	})
	if n != 0 || err != nil {
		t.Fatalf("Drain(missing) = (%d,%v), want (0,nil)", n, err)
	}
}
