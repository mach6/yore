package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// tempPrefix names an in-progress snapshot. It is deliberately not the
// data-<millis>.db shape a finished backup has, so a half-written file is never
// mistaken for one, by prune, or by whoever restores.
const tempPrefix = ".tmp-"

// backupDir returns the directory holding one tenant's rolling snapshots:
// <dir(DBPath)>/backups/<tenant>/. A single-token server's one tenant uses
// "default".
func (s *Server) backupDir(tenant string) string {
	return filepath.Join(filepath.Dir(s.dbPath), "backups", tenant)
}

// backupLoop snapshots every tenant db on a ticker until done is closed, with a
// short first timer so the first backup does not wait a whole interval. A single
// goroutine, so backups never overlap. Started only when BackupInterval > 0.
func (s *Server) backupLoop(done <-chan struct{}) {
	s.backupLoopEvery(done, 2*time.Second, s.backupInterval)
}

// backupLoopEvery is backupLoop with explicit timings, so tests can drive it
// quickly without waiting on production intervals.
func (s *Server) backupLoopEvery(done <-chan struct{}, firstDelay, interval time.Duration) {
	first := time.NewTimer(firstDelay)
	tick := time.NewTicker(interval)
	defer first.Stop()
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-first.C:
			s.backupAll()
		case <-tick.C:
			s.backupAll()
		}
	}
}

// backupAll snapshots every tenant, logging per-tenant results. A failed backup
// is logged, never fatal: it must not take the server down.
func (s *Server) backupAll() {
	for _, t := range s.tenants {
		if err := s.backupTenant(t); err != nil {
			log.Printf("backup error: tenant=%q: %v", t.name, err)
		}
	}
}

// backupTenant writes a consistent snapshot of one tenant db to
// backups/<tenant>/data-<unixMilli>.db (temp file then atomic rename) and prunes
// to the newest keep.
func (s *Server) backupTenant(t *tenant) error {
	dir := s.backupDir(t.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Write to a temp file first so a crash mid-snapshot never leaves a partial
	// data-*.db that prune (or a restore) would treat as a real backup.
	tmp := filepath.Join(dir, fmt.Sprintf("%s%d.db", tempPrefix, time.Now().UnixNano()))
	n, err := snapshotDB(t.db, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("data-%d.db", time.Now().UnixMilli()))
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	log.Printf("backup ok: tenant=%q %s bytes=%d", t.name, path, n)

	keep := s.backupKeep
	if keep <= 0 {
		keep = defaultBackupKeep
	}
	return pruneBackups(dir, keep)
}

// snapshotDB writes a consistent online snapshot of db to path and syncs it, so
// the file is durable before it is renamed into place.
func snapshotDB(db *bbolt.DB, path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := db.View(func(tx *bbolt.Tx) error {
		var werr error
		n, werr = tx.WriteTo(f)
		return werr
	}); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, err
	}
	return n, f.Close()
}

// pruneBackups removes all but the newest keep data-*.db files in dir, plus
// every leftover snapshot temp file. A remove error is returned but does not
// stop the remaining removals.
func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	var rmErr error
	for _, name := range append(backupsToPrune(names, keep), tempsToPrune(names)...) {
		if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil && rmErr == nil {
			rmErr = rerr
		}
	}
	return rmErr
}

// tempsToPrune returns the abandoned snapshot temp files in names. backupTenant
// removes its own temp file when the snapshot fails, but a process killed
// mid-snapshot cannot: the file is left behind, it is a full-size copy of the
// db, and (not matching data-<millis>.db) backupsToPrune never counted it. A
// server that restarts repeatedly therefore writes a permanent copy of its own
// database on every start until the volume fills, which is the failure a backup
// is supposed to protect against. Collecting them here is safe: backups run on a
// single goroutine and prune only after the rename, and one bbolt file has one
// owning process, so no temp file in this directory is being written now.
func tempsToPrune(names []string) []string {
	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, ".db") {
			out = append(out, name)
		}
	}
	return out
}

// backupsToPrune returns the backup filenames to delete so only the newest keep
// remain. Only names matching data-<unixMillis>.db are considered; they are
// ordered by that numeric timestamp and everything older than the newest keep is
// returned (oldest first). keep >= the number of backups is a no-op. Pure: no
// I/O, so it is unit-testable.
func backupsToPrune(names []string, keep int) []string {
	type backup struct {
		name string
		ts   int64
	}
	backups := make([]backup, 0, len(names))
	for _, name := range names {
		ts, ok := backupTimestamp(name)
		if !ok {
			continue
		}
		backups = append(backups, backup{name: name, ts: ts})
	}
	if keep < 0 {
		keep = 0
	}
	if len(backups) <= keep {
		return nil
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].ts < backups[j].ts })
	old := backups[:len(backups)-keep]
	out := make([]string, len(old))
	for i := range old {
		out[i] = old[i].name
	}
	return out
}

// backupTimestamp parses the unix-millis timestamp from a data-<millis>.db name,
// reporting ok=false for any name that does not match the pattern.
func backupTimestamp(name string) (int64, bool) {
	if !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".db") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "data-"), ".db")
	ts, err := strconv.ParseInt(mid, 10, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}
