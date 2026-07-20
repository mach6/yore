// Package spool is the crash-safe handoff between the ultra-fast
// `yore record` process and the store. Each record is one JSON line
// (rec.Record's JSON encoding) in a per-process file under the spool dir;
// the store drains those files and deletes them.
package spool

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"yore/internal/rec"
)

// Append writes r as one JSON line to this process's spool file, fsyncs it,
// and closes it. The file is named "<pid>.jsonl": all records from one
// process share a handle's worth of files, and Drain removes them promptly,
// so PID reuse across boots is harmless (a stale file is drained and gone
// before the number is recycled; a live collision just appends, which the
// line format tolerates).
func Append(spoolDir string, r rec.Record) error {
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	path := filepath.Join(spoolDir, strconv.Itoa(os.Getpid())+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Drain hands every spooled record to fn in file order and deletes each file
// once its records are consumed, returning the number of records delivered.
//
// A torn final line — a crash mid-write that leaves the last line without a
// terminating newline or as invalid JSON — is not fatal: the valid prefix is
// delivered and the file is still removed, because the incomplete record was
// never durable and downstream dedupes by ID, so any rare reprocessing is a
// no-op. If fn returns an error Drain stops immediately and returns it
// WITHOUT deleting the file it was working on, so the records survive to be
// retried. A missing spool dir yields (0, nil).
func Drain(spoolDir string, fn func(rec.Record) error) (int, error) {
	matches, err := filepath.Glob(filepath.Join(spoolDir, "*.jsonl"))
	if err != nil {
		return 0, err // only ErrBadPattern; a missing dir yields no matches
	}
	sort.Strings(matches) // deterministic, oldest-PID-first-ish order

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
	return total, nil
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
