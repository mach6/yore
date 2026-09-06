package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadTenants(t *testing.T) {
	dir := t.TempDir()

	// Empty path => no tenants, no error (single-tenant, back-compat).
	tenants, err := loadTenants("")
	require.NoError(t, err, "empty path")
	require.Nil(t, tenants, "empty path tenants")

	// Valid JSON object of name -> token.
	good := filepath.Join(dir, "tokens.json")
	require.NoError(t, os.WriteFile(good, []byte(`{"alice":"a-tok","bob":"b-tok"}`), 0o600), "write good")
	tenants, err = loadTenants(good)
	require.NoError(t, err, "good file")
	require.Equal(t, map[string]string{"alice": "a-tok", "bob": "b-tok"}, tenants, "parsed tenants")

	// Missing file => hard error.
	_, err = loadTenants(filepath.Join(dir, "nope.json"))
	require.Error(t, err, "missing file")

	// Invalid JSON => hard error.
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0o600), "write bad")
	_, err = loadTenants(bad)
	require.Error(t, err, "invalid json")

	// A well-formed but empty object => hard error, never a silent fallback to
	// the single-token mode.
	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte(`{}`), 0o600), "write empty")
	_, err = loadTenants(empty)
	require.Error(t, err, "empty tenants object")
}

func TestResolveServerToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(file, []byte("from-file\n"), 0o600), "write token file")

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "from-env")
		t.Setenv("YORE_TOKEN_FILE", file)
		tok, err := resolveServerToken("from-flag")
		require.NoError(t, err)
		require.Equal(t, "from-flag", tok)
	})

	t.Run("env beats file", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "from-env")
		t.Setenv("YORE_TOKEN_FILE", file)
		tok, err := resolveServerToken("")
		require.NoError(t, err)
		require.Equal(t, "from-env", tok)
	})

	t.Run("file trailing newline trimmed", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "")
		t.Setenv("YORE_TOKEN_FILE", file)
		tok, err := resolveServerToken("")
		require.NoError(t, err)
		require.Equal(t, "from-file", tok)
	})

	t.Run("unreadable file is an error", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "")
		t.Setenv("YORE_TOKEN_FILE", filepath.Join(dir, "nope"))
		_, err := resolveServerToken("")
		require.Error(t, err)
	})

	t.Run("nothing set is empty, not an error", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "")
		t.Setenv("YORE_TOKEN_FILE", "")
		tok, err := resolveServerToken("")
		require.NoError(t, err)
		require.Empty(t, tok, "the caller decides whether an empty token is fatal")
	})
}

// TestRunServerTokenModes pins the either/or at the CLI boundary: neither a
// token nor a tokens file fails, and both together fails, without ever
// reaching the point of opening a database.
func TestRunServerTokenModes(t *testing.T) {
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens.json")
	require.NoError(t, os.WriteFile(tokens, []byte(`{"alice":"a-tok"}`), 0o600), "write tokens")

	t.Run("neither", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "")
		t.Setenv("YORE_TOKEN_FILE", "")
		t.Setenv("YORE_TOKENS_FILE", "")
		require.Equal(t, 1, runServer(filepath.Join(dir, "a.db"), "127.0.0.1:0", "", ""), "no credential must fail")
	})

	t.Run("both", func(t *testing.T) {
		t.Setenv("YORE_TOKEN", "tok")
		t.Setenv("YORE_TOKEN_FILE", "")
		t.Setenv("YORE_TOKENS_FILE", tokens)
		require.Equal(t, 1, runServer(filepath.Join(dir, "b.db"), "127.0.0.1:0", "", ""), "both modes must fail")
	})

	// Neither case may leave a database behind.
	for _, name := range []string{"a.db", "b.db"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.Truef(t, os.IsNotExist(err), "%s must not be created: %v", name, err)
	}
}

func TestParseBackupInterval(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"unset defaults to 1h", "", time.Hour, false},
		{"explicit zero disables", "0", 0, false},
		{"duration", "30m", 30 * time.Minute, false},
		{"negative rejected", "-5m", 0, true},
		{"garbage rejected", "abc", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBackupInterval(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseBackupKeep(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"unset defaults to 3", "", 3, false},
		{"positive", "5", 5, false},
		{"zero rejected", "0", 0, true},
		{"negative rejected", "-1", 0, true},
		{"garbage rejected", "x", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBackupKeep(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
