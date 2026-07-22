// Package secret stores yore's sensitive material — the device private key —
// outside of config.json, which stays plain, diffable, and safe to share.
//
// It prefers the OS keyring (Secret Service on Linux, Keychain on macOS) and
// falls back to a 0600 file under the state dir whenever no keyring is usable:
// headless servers, containers, and SSH sessions without a session bus all land
// there. The backend is probed once per process and reported by Backend(), so
// `yore doctor` can tell the user which one is in effect rather than leaving it
// a mystery.
package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// service is the keyring service name every yore entry is filed under.
const service = "yore"

// probeTimeout bounds every keyring call. A locked or wedged Secret Service can
// otherwise block indefinitely — unacceptable in the daemon, which must never
// hang waiting on a desktop unlock prompt.
const probeTimeout = 3 * time.Second

// Secret names. The file fallback uses these verbatim as filenames under the
// state dir, so DeviceKey resolves to ~/.config/yore/device.key.
const DeviceKey = "device.key"

// Backend names where secrets are actually kept.
type Backend string

const (
	BackendKeyring Backend = "keyring"
	BackendFile    Backend = "file"
)

// ErrNotFound is returned by Get when no secret of that name is stored.
var ErrNotFound = errors.New("secret: not found")

// errTimeout marks a keyring call that exceeded probeTimeout.
var errTimeout = errors.New("secret: keyring timed out")

// Store reads and writes secrets for one state directory.
type Store struct {
	dir     string
	backend Backend
}

// Open returns a Store for dir, choosing the backend once.
//
// $YORE_SECRET_BACKEND forces the choice ("keyring" or "file"); anything else
// (including unset) auto-detects by round-tripping a probe entry through the
// keyring. Detection is deliberately a real write/read/delete: a keyring that
// accepts a connection but cannot store anything is worse than none.
func Open(dir string) *Store {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("YORE_SECRET_BACKEND"))) {
	case string(BackendFile):
		return &Store{dir: dir, backend: BackendFile}
	case string(BackendKeyring):
		return &Store{dir: dir, backend: BackendKeyring}
	}
	if keyringAvailable() {
		return &Store{dir: dir, backend: BackendKeyring}
	}
	return &Store{dir: dir, backend: BackendFile}
}

// probe caches auto-detection for the process. Detection costs a full keyring
// round-trip (three IPC calls), and Open sits on paths that run repeatedly —
// the daemon re-resolves its configuration every few seconds — so probing once
// is the difference between a negligible cost and constant IPC chatter.
var probe struct {
	once sync.Once
	ok   bool
}

func keyringAvailable() bool {
	probe.once.Do(func() { probe.ok = keyringUsable() })
	return probe.ok
}

// Backend reports where this Store keeps secrets.
func (s *Store) Backend() Backend { return s.backend }

// Get returns the named secret.
//
// The file is consulted whenever the keyring has no entry, so a machine that
// later loses keyring access (session bus gone, keyring locked) still finds
// secrets written to disk. A file that is group- or other-readable is refused
// rather than trusted.
func (s *Store) Get(name string) (string, error) {
	if s.backend == BackendKeyring {
		v, err := keyringGet(name)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, keyring.ErrNotFound) && !errors.Is(err, errTimeout) {
			return "", err
		}
	}
	return s.readFile(name)
}

// Set stores the named secret, replacing any previous value.
//
// On a successful keyring write any stale file copy is removed: leaving the
// secret in two places would mean rotating it in one and still authenticating
// with the other.
func (s *Store) Set(name, value string) error {
	if s.backend == BackendKeyring {
		if err := keyringSet(name, value); err != nil {
			return err
		}
		if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("secret: removing superseded file copy of %s: %w", name, err)
		}
		return nil
	}
	return s.writeFile(name, value)
}

// Delete removes the named secret from both backends, so no copy survives.
func (s *Store) Delete(name string) error {
	var firstErr error
	if s.backend == BackendKeyring {
		if err := keyringDelete(name); err != nil && !errors.Is(err, keyring.ErrNotFound) && !errors.Is(err, errTimeout) {
			firstErr = err
		}
	}
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// path is the file-fallback location of a secret.
func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// readFile loads a secret from the file fallback, enforcing private permissions.
func (s *Store) readFile(name string) (string, error) {
	path := s.path(name)
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret: %s is mode %04o; want no group/other access", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// writeFile stores a secret in the file fallback at mode 0600, enforced even if
// the file already existed with looser bits.
func (s *Store) writeFile(name, value string) error {
	path := s.path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ---- keyring calls, each bounded by probeTimeout ----

func keyringGet(name string) (string, error) {
	var out string
	err := withTimeout(func() error {
		v, err := keyring.Get(service, name)
		out = v
		return err
	})
	return out, err
}

func keyringSet(name, value string) error {
	return withTimeout(func() error { return keyring.Set(service, name, value) })
}

func keyringDelete(name string) error {
	return withTimeout(func() error { return keyring.Delete(service, name) })
}

// keyringUsable reports whether the keyring can actually round-trip a value.
func keyringUsable() bool {
	const probe = "probe"
	if err := keyringSet(probe, "1"); err != nil {
		return false
	}
	v, err := keyringGet(probe)
	_ = keyringDelete(probe)
	return err == nil && v == "1"
}

// withTimeout runs fn on its own goroutine and gives up after probeTimeout.
//
// A timed-out goroutine is deliberately left running: it is blocked inside the
// keyring library and cannot be cancelled, but it holds nothing of ours and the
// path is rare (a locked or wedged Secret Service). Abandoning it is strictly
// better than blocking the daemon behind it.
func withTimeout(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(probeTimeout):
		return errTimeout
	}
}
