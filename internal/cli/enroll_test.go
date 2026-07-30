package cli

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/server"
)

// setupTestToken is the bearer token the in-process test server accepts.
const setupTestToken = "setup-test-token"

// newSetupServer stands up a real in-process sync server (empty store, known
// token) and returns its base URL. It is torn down when the test finishes.
func newSetupServer(t *testing.T) string {
	t.Helper()
	s, err := server.New(server.Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: setupTestToken})
	require.NoError(t, err, "server.New")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	return srv.URL
}

// TestRunSetup exercises the validate-before-persist contract: a setup that the
// server rejects (or a flag conflict caught before contacting it) must leave
// config.toml untouched, while a setup the server accepts bootstraps the group
// and writes the correct server + token.
func TestRunSetup(t *testing.T) {
	tests := []struct {
		name        string
		token       string // token passed to runSetup
		pin         bool
		clearPin    bool
		preseedGood bool // seed a valid config.toml before running
		wantCode    int
		// assertConfig runs after runSetup; before is config.toml's bytes prior to
		// the run (nil when it was absent), base the server URL.
		assertConfig func(t *testing.T, dir, base string, before []byte)
	}{
		{
			name:     "wrong token leaves no config behind",
			token:    "wrong-token",
			wantCode: 1,
			assertConfig: func(t *testing.T, dir, _ string, _ []byte) {
				_, err := os.Stat(config.ConfigPath(dir))
				require.True(t, os.IsNotExist(err), "config.toml must not be created by a failed setup")
			},
		},
		{
			name:        "wrong token preserves an existing config byte-for-byte",
			token:       "wrong-token",
			preseedGood: true,
			wantCode:    1,
			assertConfig: func(t *testing.T, dir, _ string, before []byte) {
				after, err := os.ReadFile(config.ConfigPath(dir))
				require.NoError(t, err, "read config.toml")
				require.True(t, bytes.Equal(before, after), "failed setup must not modify config.toml")
				require.NotContains(t, string(after), "wrong-token", "bad token must not reach config.toml")
			},
		},
		{
			name:     "correct token bootstraps the first device and persists",
			token:    setupTestToken,
			wantCode: 0,
			assertConfig: func(t *testing.T, dir, base string, _ []byte) {
				cfg, err := config.Load(dir)
				require.NoError(t, err, "load config.toml")
				require.Equal(t, base, cfg.ServerURL, "saved server URL")
				require.Equal(t, "takeover", cfg.Integration, "saved integration")

				// The token belongs in the secret store, never in config.toml.
				raw, err := os.ReadFile(config.ConfigPath(dir))
				require.NoError(t, err, "read config.toml")
				require.NotContains(t, string(raw), setupTestToken, "token must not appear in config.toml")

			},
		},
		{
			name:     "pin and clear-pin together error without saving",
			token:    setupTestToken,
			pin:      true,
			clearPin: true,
			wantCode: 1,
			assertConfig: func(t *testing.T, dir, _ string, _ []byte) {
				_, err := os.Stat(config.ConfigPath(dir))
				require.True(t, os.IsNotExist(err), "config.toml must not be created on a flag conflict")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("YORE_DIR", dir)
			base := newSetupServer(t)

			if tc.preseedGood {
				require.NoError(t, config.Save(dir, config.Config{
					ServerURL:   base,
					Integration: "takeover",
				}), "preseed config")
			}
			var before []byte
			if b, err := os.ReadFile(config.ConfigPath(dir)); err == nil {
				before = b
			}

			got := runSetup(base, tc.token, "test-device", "takeover", tc.pin, tc.clearPin)
			require.Equal(t, tc.wantCode, got, "runSetup exit code")
			tc.assertConfig(t, dir, base, before)
		})
	}
}

// TestRunSetupOnEnrolledMachineSavesPinChange is the regression test for a setup
// run that had nothing to enroll silently discarding its flags: `--clear-pin` (or
// `--pin`) reported success and left config.toml alone, because the
// already-enrolled path returned before config.Save.
//
// The already-enrolled machine is the ONLY one you can run these flags on — a
// pin is changed long after enrollment — so this was the whole feature.
//
// It also pins the token handling: clearing a pin needs no enrollment token, so
// the run must succeed with an empty one rather than prompting for a credential
// it will not spend.
func TestRunSetupOnEnrolledMachineSavesPinChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	base := newSetupServer(t)

	require.Equal(t, 0, runSetup(base, setupTestToken, "test-device", "takeover", false, false),
		"first setup should bootstrap the group")

	// Stand in for a pin whose certificate has since been replaced. Written by
	// hand because capturing one needs a TLS server, while clearing one does not.
	cfg, err := config.Load(dir)
	require.NoError(t, err, "load config.toml")
	cfg.ServerPin = "c3RhbGUtcGluLXRoYXQtbm8tbG9uZ2VyLW1hdGNoZXM="
	require.NoError(t, config.Save(dir, cfg), "seed a stale pin")

	require.Equal(t, 0, runSetup(base, "", "test-device", "takeover", false, true),
		"clearing a pin on an enrolled machine should succeed without a token")

	cfg, err = config.Load(dir)
	require.NoError(t, err, "reload config.toml")
	require.Empty(t, cfg.ServerPin, "--clear-pin must persist the cleared pin")
	require.Equal(t, base, cfg.ServerURL, "clearing a pin must not disturb the server URL")
}
