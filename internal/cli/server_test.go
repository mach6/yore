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
