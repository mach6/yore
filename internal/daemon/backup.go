package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"yore/internal/config"
)

// backupLoop periodically writes a consistent snapshot of the local db into the
// backups dir and prunes to the newest BackupKeepN. Started only when backups
// are enabled (BackupIntervalD > 0). A single goroutine, so a backup never
// overlaps itself. Mirrors snapshotLoop, with an initial run shortly after
// startup (like syncLoop) so the first backup does not wait a whole interval.
func (s *server) backupLoop() {
	defer s.wg.Done()
	first := time.NewTimer(2 * time.Second)
	tick := time.NewTicker(s.cfg.BackupIntervalD())
	defer first.Stop()
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-first.C:
			s.doBackup()
		case <-tick.C:
			s.doBackup()
		}
	}
}

// doBackup writes a consistent online snapshot of data.db into the backups dir
// under a timestamped name, then prunes older backups to the configured keep
// count. Called only from backupLoop (single goroutine), so writes and prunes
// never overlap. Errors are logged, not fatal — a failed backup must never take
// the daemon down.
func (s *server) doBackup() {
	dir := config.BackupDir(s.dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.logf("backup error: %v", err)
		return
	}

	// Write to a temp file first so a crash mid-snapshot never leaves a partial
	// data-*.db that prune (or a restore) would treat as a real backup.
	tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d.db", time.Now().UnixNano()))
	n, err := s.writeBackupFile(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		s.logf("backup error: %v", err)
		return
	}

	path := filepath.Join(dir, fmt.Sprintf("data-%d.db", time.Now().UnixMilli()))
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		s.logf("backup error: %v", err)
		return
	}
	s.logf("backup ok: %s bytes=%d", path, n)

	if err := pruneBackups(dir, s.cfg.BackupKeepN()); err != nil {
		s.logf("backup prune error: %v", err)
	}
}

// writeBackupFile snapshots the store to path and syncs+closes it, so the file
// is durable on disk before doBackup renames it into place.
func (s *server) writeBackupFile(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := s.store.BackupTo(f)
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, err
	}
	return n, f.Close()
}

// pruneBackups removes all but the newest keep data-*.db files in dir. A remove
// error is returned but does not stop the remaining removals.
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
	for _, name := range backupsToPrune(names, keep) {
		if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil && rmErr == nil {
			rmErr = rerr
		}
	}
	return rmErr
}

// backupsToPrune returns the backup filenames to delete so only the newest keep
// remain. Only names matching data-<unixMillis>.db are considered; they are
// ordered by that numeric timestamp and everything older than the newest keep
// is returned (oldest first). keep >= the number of backups is a no-op. Pure:
// no I/O, so it is unit-testable.
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
