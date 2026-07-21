package daemon

import (
	"fmt"
	"os"
	"sync"
)

// rotatingWriter is a size-capped io.WriteCloser over a single log file. It
// keeps daemon.log bounded: once a write would push the file past maxSize it
// rotates — daemon.log.(keep-1) shifts up to .keep (the oldest is dropped),
// daemon.log becomes daemon.log.1, and a fresh daemon.log is opened. The size
// check is done at write time (the daemon's log volume is low, so a per-write
// stat-free size accumulator is plenty) and the writer is mutex-guarded so the
// logger's concurrent Printf calls stay consistent.
type rotatingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	f       *os.File
	size    int64
}

// newRotatingWriter opens path for append and seeds the current size from its
// stat, so a restart continues an existing segment rather than resetting the
// cap. maxSize must be > 0 and keep >= 1.
func newRotatingWriter(path string, maxSize int64, keep int) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	return &rotatingWriter{path: path, maxSize: maxSize, keep: keep, f: f, size: size}, nil
}

// Write appends p, rotating first if it would overflow the cap. A single write
// larger than maxSize still lands in a freshly rotated file (it is never split).
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file. Safe to call more than once.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate closes the current file, shifts the numbered segments up (dropping the
// oldest), moves daemon.log to daemon.log.1, and reopens a fresh daemon.log.
// Caller holds w.mu.
func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	// Drop the oldest, then shift each segment up one: .(k-1) -> .k, ... .1 -> .2.
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Rename(w.path, w.path+".1")

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	return nil
}
