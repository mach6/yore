package store

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/spool"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendMonotonicAndHostFields(t *testing.T) {
	s := openTemp(t)

	hn, _ := os.Hostname()
	if s.HostID() == "" {
		t.Fatal("HostID empty after Open")
	}
	if hn != "" && s.Hostname() != hn {
		t.Errorf("Hostname = %q, want %q", s.Hostname(), hn)
	}

	var last uint64
	for i := 0; i < 5; i++ {
		r, err := s.Append(rec.Record{Cmd: fmt.Sprintf("cmd-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if r.Seq != last+1 {
			t.Errorf("seq = %d, want %d", r.Seq, last+1)
		}
		last = r.Seq
		if r.ID == "" {
			t.Error("ID not generated")
		}
		if r.HostID != s.HostID() || r.Hostname != s.Hostname() {
			t.Errorf("host fields not filled: %+v", r)
		}
	}

	if ls, _ := s.LastSeq(); ls != 5 {
		t.Errorf("LastSeq = %d, want 5", ls)
	}
	if c, _ := s.Count(); c != 5 {
		t.Errorf("Count = %d, want 5", c)
	}
}

func TestIdempotentReAppend(t *testing.T) {
	s := openTemp(t)

	first, err := s.Append(rec.Record{ID: "fixed", Cmd: "echo once"})
	if err != nil {
		t.Fatal(err)
	}

	added, err := s.AppendBatch([]rec.Record{
		{ID: "fixed", Cmd: "echo once"},
		{ID: "fixed", Cmd: "same id different cmd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("AppendBatch added = %d, want 0 (all duplicate IDs)", added)
	}

	// A duplicate single Append returns the originally stored record.
	got, err := s.Append(rec.Record{ID: "fixed", Cmd: "yet another"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != first.Seq || got.Cmd != first.Cmd {
		t.Errorf("dup Append returned %+v, want stored %+v", got, first)
	}
	if c, _ := s.Count(); c != 1 {
		t.Errorf("Count = %d, want 1", c)
	}
}

func TestSecondOpenLocked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v, want ErrLocked", err)
	}
}

func TestTombstoneMarksTargetAndAllExcludes(t *testing.T) {
	s := openTemp(t)

	if _, err := s.Append(rec.Record{ID: "victim", Cmd: "secret command"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(rec.Record{ID: "keep", Cmd: "harmless"}); err != nil {
		t.Fatal(err)
	}
	del, err := s.Append(rec.Record{Type: rec.TypeDelete, TargetID: "victim", StartMs: 12345})
	if err != nil {
		t.Fatal(err)
	}
	if del.Type != rec.TypeDelete || del.Seq == 0 {
		t.Fatalf("tombstone not appended to stream: %+v", del)
	}

	// All excludes both the tombstone and the now-deleted target.
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "keep" {
		t.Fatalf("All = %+v, want only [keep]", all)
	}

	// Since is the raw stream: target (deleted), keep, and the tombstone.
	raw, err := s.Since(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("Since(0) len = %d, want 3", len(raw))
	}
	var sawTarget, sawTomb bool
	for _, r := range raw {
		if r.ID == "victim" {
			sawTarget = true
			if r.DeletedMs != 12345 {
				t.Errorf("victim DeletedMs = %d, want 12345 (from tombstone StartMs)", r.DeletedMs)
			}
		}
		if r.Type == rec.TypeDelete {
			sawTomb = true
		}
	}
	if !sawTarget || !sawTomb {
		t.Errorf("Since must include deleted target and tombstone; target=%v tomb=%v", sawTarget, sawTomb)
	}
}

func TestMarkDeleted(t *testing.T) {
	s := openTemp(t)

	if _, err := s.Append(rec.Record{ID: "x", Cmd: "rm -rf /"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeleted("x", 999); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.All(); len(all) != 0 {
		t.Errorf("All after MarkDeleted = %+v, want empty", all)
	}
	raw, _ := s.Since(0, 0)
	if len(raw) != 1 || raw[0].DeletedMs != 999 {
		t.Errorf("raw stream = %+v, want x with DeletedMs=999", raw)
	}

	if err := s.MarkDeleted("does-not-exist", 0); err == nil {
		t.Error("MarkDeleted on missing ID should error")
	}
}

func TestSinceLimitAndStrictlyGreater(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 10; i++ {
		if _, err := s.Append(rec.Record{Cmd: fmt.Sprintf("c%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Since(3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("Since(3,4) len = %d, want 4", len(rows))
	}
	if rows[0].Seq != 4 {
		t.Errorf("first seq = %d, want 4 (strictly greater than 3)", rows[0].Seq)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTemp(t)
	if v, _ := s.Meta("absent"); v != "" {
		t.Errorf("Meta(absent) = %q, want empty", v)
	}
	if err := s.SetMeta("k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Meta("k"); v != "v" {
		t.Errorf("Meta(k) = %q, want v", v)
	}
}

func TestIngestSpoolEndToEnd(t *testing.T) {
	s := openTemp(t)
	sd := config.SpoolDir(s.Dir())

	const n = 20
	for i := 0; i < n; i++ {
		r := rec.Record{ID: fmt.Sprintf("s-%d", i), Cmd: fmt.Sprintf("cmd %d", i)}
		if err := spool.Append(sd, r); err != nil {
			t.Fatal(err)
		}
	}

	added, err := s.IngestSpool()
	if err != nil {
		t.Fatal(err)
	}
	if added != n {
		t.Fatalf("IngestSpool added %d, want %d", added, n)
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("All len = %d, want %d", len(all), n)
	}
	for _, r := range all {
		if r.HostID != s.HostID() || r.Hostname != s.Hostname() {
			t.Errorf("ingested record missing host fields: %+v", r)
		}
	}

	// Spool is now empty; a second ingest is a no-op.
	if added2, err := s.IngestSpool(); err != nil || added2 != 0 {
		t.Errorf("second IngestSpool = (%d,%v), want (0,nil)", added2, err)
	}
}

func BenchmarkAppend(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Append(rec.Record{Cmd: "benchmark command --flag value"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAll200k(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	const total = 200_000
	const chunk = 5_000
	batch := make([]rec.Record, 0, chunk)
	for i := 0; i < total; i += chunk {
		batch = batch[:0]
		for j := 0; j < chunk; j++ {
			batch = append(batch, rec.Record{
				ID:  fmt.Sprintf("r-%d", i+j),
				Cmd: "some representative shell command --with flags",
				Cwd: "/home/user/project",
			})
		}
		if _, err := s.AppendBatch(batch); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.All()
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != total {
			b.Fatalf("All returned %d rows, want %d", len(rows), total)
		}
	}
}
