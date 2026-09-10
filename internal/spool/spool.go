// Package spool is the crash-safe handoff between the ultra-fast `yore
// record` process and the store. Each record is one JSON line (rec.Record's
// JSON encoding) in a file under the spool dir; the store drains those files
// and deletes them. A record is published ATOMICALLY: it is written and
// fsynced under a ".tmp" name the drainer does not look at, then renamed into
// place. Only complete files ever carry the ".jsonl" name, so a drain running
// concurrently with a write can neither read a half-written record nor delete
// a file whose writer has not finished with it: a record that exists is a
// record that survives.
package spool

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/mach6/yore/internal/rec"
)

// tmpTTL bounds how long an abandoned ".tmp" file (a process that died between
// creating it and renaming it into place) is left on disk. It only has to be
// far longer than a write takes; Drain sweeps anything older.
const tmpTTL = time.Hour

// seq disambiguates two writes from one process that land in the same
// nanosecond, so a name is never reused within a run.
var seq atomic.Uint64

// Append writes r as one JSON line to a fresh spool file and publishes it
// atomically: the line goes to a ".tmp" file, is fsynced, and only then is
// renamed to "<nanos>-<pid>-<n>.jsonl"; the name Drain collects. The rename is
// what makes concurrent draining safe. The obvious alternative, appending to a
// per-process file, gives the drainer a file it can read (empty) and delete in
// the window between the writer creating it and writing to it; the write then
// lands on an unlinked inode and the record is gone, with no error anywhere.
// Publishing under a name the drainer ignores closes that window: until the
// rename there is nothing to collect, and after it there is nothing left to
// write. Names sort chronologically (fixed-width nanoseconds first), so Drain
// hands records to the store in roughly the order they were recorded.
func Append(spoolDir string, r rec.Record) error {
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	base := fmt.Sprintf("%019d-%d-%d", time.Now().UnixNano(), os.Getpid(), seq.Add(1))
	tmp := filepath.Join(spoolDir, base+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(spoolDir, base+".jsonl")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Drain hands every spooled record to fn in file order and deletes each file
// once its records are consumed, returning the number of records delivered. It
// also sweeps ".tmp" files older than tmpTTL: the residue of a process that
// died mid-write, whose record was never durable. A torn line (invalid JSON or
// a missing terminating newline) is not fatal: the valid prefix is delivered
// and the file is still removed. Append's rename means a file it wrote cannot
// be torn, but a file left by an older version of yore, or a truncated disk,
// still has to be got past rather than retried forever. If fn returns an error
// Drain stops immediately and returns it WITHOUT deleting the file it was
// working on, so those records survive to be retried. A missing spool dir
// yields (0, nil).
func Drain(spoolDir string, fn func(rec.Record) error) (int, error) {
	matches, err := filepath.Glob(filepath.Join(spoolDir, "*.jsonl"))
	if err != nil {
		return 0, err // only ErrBadPattern; a missing dir yields no matches
	}
	sort.Strings(matches) // chronological: names lead with fixed-width nanos

	total := 0
	for _, path := range matches {
		n, err := drainFile(path, fn)
		total += n
		if err != nil {
			return total, err // fn failed: leave this file in place
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return total, err
		}
	}
	sweepTmp(spoolDir)
	return total, nil
}

// sweepTmp removes abandoned ".tmp" files. Anything younger than tmpTTL may
// belong to a write in flight and is left strictly alone: a live writer holds
// the only reference to that record.
func sweepTmp(spoolDir string) {
	tmps, err := filepath.Glob(filepath.Join(spoolDir, "*.tmp"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tmpTTL)
	for _, path := range tmps {
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}

// drainFile delivers one file's records to fn. It returns the count handed
// off and, only if fn itself errors, that error (leaving the file intact).
// Parse failures are treated as a torn tail: the valid prefix is kept and
// the caller proceeds to delete the file.
func drainFile(path string, fn func(rec.Record) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // raced with another drainer
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()

	n := 0
	br := bufio.NewReader(f)
	for {
		line, rerr := br.ReadBytes('\n')
		// A line without a trailing newline (rerr != nil, typically io.EOF)
		// is the torn tail of a crashed write; stop and drop it.
		if rerr != nil {
			return n, nil
		}
		var r rec.Record
		if err := json.Unmarshal(line, &r); err != nil {
			// Complete lines are always valid JSON (Append fsyncs whole
			// lines); a parse error here means corruption at the tail.
			return n, nil
		}
		if err := fn(r); err != nil {
			return n, err
		}
		n++
	}
}
