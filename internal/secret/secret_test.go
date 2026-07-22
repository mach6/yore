package secret

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileStore returns a Store pinned to the file backend. CI has no session bus,
// and a developer machine must not have its real keyring written to by tests.
func fileStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("YORE_SECRET_BACKEND", string(BackendFile))
	return Open(t.TempDir())
}

func TestBackendSelection(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want Backend
	}{
		{name: "file forced", env: "file", want: BackendFile},
		{name: "keyring forced", env: "keyring", want: BackendKeyring},
		{name: "case insensitive", env: "  FILE ", want: BackendFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("YORE_SECRET_BACKEND", tc.env)
			assert.Equal(t, tc.want, Open(t.TempDir()).Backend())
		})
	}
}

func TestRoundTrip(t *testing.T) {
	s := fileStore(t)

	_, err := s.Get(DeviceKey)
	require.ErrorIs(t, err, ErrNotFound, "unset secret")

	require.NoError(t, s.Set(DeviceKey, "tok-value"), "Set")
	got, err := s.Get(DeviceKey)
	require.NoError(t, err, "Get")
	assert.Equal(t, "tok-value", got)

	require.NoError(t, s.Set(DeviceKey, "rotated"), "re-Set")
	got, err = s.Get(DeviceKey)
	require.NoError(t, err, "Get after rotate")
	assert.Equal(t, "rotated", got, "Set must replace, not append")

	require.NoError(t, s.Delete(DeviceKey), "Delete")
	_, err = s.Get(DeviceKey)
	assert.ErrorIs(t, err, ErrNotFound, "deleted secret")

	assert.NoError(t, s.Delete(DeviceKey), "Delete of a missing secret is not an error")
}

// TestFileIsPrivate pins the on-disk contract: secrets are written 0600, and a
// pre-existing loose mode is tightened rather than trusted.
func TestFileIsPrivate(t *testing.T) {
	s := fileStore(t)
	path := filepath.Join(s.dir, DeviceKey)

	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o644), "seed a world-readable file")
	require.NoError(t, s.Set(DeviceKey, "value"), "Set")

	fi, err := os.Stat(path)
	require.NoError(t, err, "stat")
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "secret file must be 0600")
}

// TestRefusesExposedFile is the fail-closed case: a secret another user could
// read is refused outright rather than silently used.
func TestRefusesExposedFile(t *testing.T) {
	s := fileStore(t)
	path := filepath.Join(s.dir, DeviceKey)
	require.NoError(t, os.WriteFile(path, []byte("value"), 0o600), "write")
	require.NoError(t, os.Chmod(path, 0o644), "loosen")

	_, err := s.Get(DeviceKey)
	require.Error(t, err, "a group/other-readable secret must be refused")
	assert.NotErrorIs(t, err, ErrNotFound, "the error must name the permission problem, not look absent")
}

// TestTrailingNewlineStripped keeps hand-written secret files usable: an editor
// that adds a trailing newline must not change the token.
func TestTrailingNewlineStripped(t *testing.T) {
	s := fileStore(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, DeviceKey), []byte("value\n"), 0o600), "write")

	got, err := s.Get(DeviceKey)
	require.NoError(t, err, "Get")
	assert.Equal(t, "value", got)
}

// TestDeviceKeyStore covers the device identity round-tripping through the
// store: EnsureDeviceKey mints once and is stable, and a stored key parses back
// to the same identity.
func TestDeviceKeyStore(t *testing.T) {
	s := fileStore(t)

	_, err := s.LoadDeviceKey()
	require.ErrorIs(t, err, ErrNotFound, "no device key before enrollment")

	k1, err := s.EnsureDeviceKey()
	require.NoError(t, err, "EnsureDeviceKey")

	k2, err := s.EnsureDeviceKey()
	require.NoError(t, err, "EnsureDeviceKey is idempotent")
	require.Equal(t, k1.Public(), k2.Public(), "EnsureDeviceKey must not mint a second identity")

	loaded, err := s.LoadDeviceKey()
	require.NoError(t, err, "LoadDeviceKey after ensure")
	require.Equal(t, k1.Public(), loaded.Public(), "loaded identity must match")
	require.Equal(t, k1.SignPublic(), loaded.SignPublic(), "loaded signing key must match")
}
