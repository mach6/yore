package daemon

import (
	"encoding/gob"
	"os"
	"path/filepath"
	"time"

	"yore/internal/rec"
)

// snapshotFile names the warm-start corpus cache inside the state dir.
const snapshotFile = "corpus.snap"

// snapshot is the on-disk warm-start payload: the live search corpus plus the
// raw stream position it was built at. It is DERIVED data — always rebuildable
// from data.db — so any read/decode error just falls back to a full load.
type snapshot struct {
	LastSeq uint64
	Records []rec.Record
}

func snapshotPath(dir string) string { return filepath.Join(dir, snapshotFile) }

// writeSnapshot atomically persists the corpus so the next start is instant.
// It writes a temp file, fsyncs, and renames — a crash never leaves a torn
// snapshot in place (the rename is atomic; a stray .tmp is ignored on read).
func writeSnapshot(dir string, records []rec.Record, lastSeq uint64) error {
	path := snapshotPath(dir)
	tmp, err := os.CreateTemp(dir, snapshotFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if err := gob.NewEncoder(tmp).Encode(snapshot{LastSeq: lastSeq, Records: records}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// readSnapshot loads the warm-start corpus. ok is false (with no error) when
// there is simply no usable snapshot — a missing or unreadable/corrupt file is
// not an error, just a cold start.
func readSnapshot(dir string) (snap snapshot, ok bool) {
	f, err := os.Open(snapshotPath(dir))
	if err != nil {
		return snapshot{}, false
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&snap); err != nil {
		return snapshot{}, false // corrupt/partial: fall back to full load
	}
	return snap, true
}

// loadCorpus populates the corpus for a fresh daemon. It prefers the warm
// snapshot: decoding a gob slice in one sequential read is far cheaper than
// re-scanning and JSON-decoding every row from bbolt. Whatever the snapshot's
// position, the store's rows above it are folded in (via Since) so the corpus
// is always current — records only append while the daemon is down, and local
// deletions only happen through a running daemon, so a shutdown snapshot
// already reflects every deletion. A snapshot that is somehow ahead of the
// store (should never happen) is discarded for a full load.
func (s *server) loadCorpus() error {
	start := time.Now()

	if snap, ok := readSnapshot(s.dir); ok {
		storeSeq, err := s.store.LastSeq()
		if err != nil {
			return err
		}
		if snap.LastSeq <= storeSeq {
			s.corpus = snap.Records
			s.cmds = make([]string, len(snap.Records))
			for i := range snap.Records {
				s.cmds[i] = snap.Records[i].Cmd
			}
			s.lastSeq = snap.LastSeq
			tail, err := s.store.Since(snap.LastSeq, 0)
			if err != nil {
				return err
			}
			s.foldRows(tail)
			s.logf("warm start: snapshot=%d +tail=%d corpus=%d load_ms=%d",
				len(snap.Records), len(tail), len(s.corpus), time.Since(start).Milliseconds())
			return nil
		}
		s.logf("snapshot ahead of store (snap=%d store=%d); full reload", snap.LastSeq, storeSeq)
	}

	all, err := s.store.All()
	if err != nil {
		return err
	}
	s.corpus = all
	s.cmds = make([]string, len(all))
	for i := range all {
		s.cmds[i] = all[i].Cmd
	}
	if s.lastSeq, err = s.store.LastSeq(); err != nil {
		return err
	}
	s.logf("cold start: corpus=%d load_ms=%d", len(all), time.Since(start).Milliseconds())
	return nil
}

// snapshotNow persists the current corpus. Callers hold no lock; it takes a
// brief read lock to copy the slice header (safe: the corpus is append-only,
// so the captured prefix is immutable even as the writer appends).
func (s *server) snapshotNow() {
	s.mu.RLock()
	records := s.corpus
	lastSeq := s.lastSeq
	s.mu.RUnlock()
	if err := writeSnapshot(s.dir, records, lastSeq); err != nil {
		s.logf("snapshot write error: %v", err)
	}
}
