package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSize(t *testing.T) {
	const def = int64(5 * 1024 * 1024)
	tests := []struct {
		name string
		in   string
		def  int64
		want int64
	}{
		{"empty uses default", "", def, def},
		{"plain bytes", "1024", def, 1024},
		{"bytes suffix", "512B", def, 512},
		{"kilobytes", "512KB", def, 512 * 1024},
		{"megabytes", "5MB", def, 5 * 1024 * 1024},
		{"gigabytes", "1GB", def, 1024 * 1024 * 1024},
		{"case insensitive", "2mb", def, 2 * 1024 * 1024},
		{"spaces tolerated", " 3 MB ", def, 3 * 1024 * 1024},
		{"explicit zero disables", "0", def, 0},
		{"garbage uses default", "not-a-size", def, def},
		{"negative uses default", "-4MB", def, def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseSize(tt.in, tt.def))
		})
	}
}

// TestDefaults pins the default-true booleans that Defaults() must seed (they
// cannot come from the zero value) and confirms a zero Config leaves them false —
// i.e. the defaults live in Defaults(), not in any accessor.
func TestDefaults(t *testing.T) {
	d := Defaults()
	assert.True(t, d.AutoDeepen, "AutoDeepen default true")
	assert.True(t, d.EnterExecutes, "EnterExecutes default true")
	assert.True(t, d.LogSilent, "LogSilent default true")

	var z Config
	assert.False(t, z.AutoDeepen, "zero Config: AutoDeepen false (no magic accessor)")
	assert.False(t, z.EnterExecutes, "zero Config: EnterExecutes false")
	assert.False(t, z.LogSilent, "zero Config: LogSilent false")
}

// TestLoadSeedsDefaultsAndOverrides is the crux of the design: Load seeds
// Defaults() then unmarshals the file over it, so an absent key keeps its default
// while an explicit value — including false — overrides. This is the round-trip
// that a *bool "was it set?" scheme was standing in for.
func TestLoadSeedsDefaultsAndOverrides(t *testing.T) {
	// Missing file -> Defaults().
	missing, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, Defaults(), missing, "missing config yields Defaults()")

	// A file that mentions only enter_executes:false must keep the other defaults
	// on and flip just that one — the classic case an omitempty *bool got wrong.
	dir := t.TempDir()
	require.NoError(t, Save(dir, Config{EnterExecutes: false, AutoDeepen: true, LogSilent: true, ServerURL: "https://s"}))
	got, err := Load(dir)
	require.NoError(t, err)
	assert.False(t, got.EnterExecutes, "explicit false survives Save/Load")
	assert.True(t, got.AutoDeepen, "unrelated default stays true")
	assert.True(t, got.LogSilent, "unrelated default stays true")
	assert.Equal(t, "https://s", got.ServerURL)

	// An absent enter_executes must remain the default (true), not fall to the
	// bool zero value.
	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(ConfigPath(dir2), []byte(`{"server_url":"https://x"}`), 0o600))
	got2, err := Load(dir2)
	require.NoError(t, err)
	assert.True(t, got2.EnterExecutes, "absent key keeps default")
	assert.True(t, got2.LogSilent, "absent key keeps default")
}

// TestTypedAccessorDefaults covers the string-backed settings, which default
// from their zero value via typed accessors (not Defaults()).
func TestTypedAccessorDefaults(t *testing.T) {
	var c Config // zero value: every typed accessor supplies its default

	assert.Equal(t, 24*time.Hour, c.KeyEpochD(), "KeyEpochD default")
	assert.Equal(t, 30*time.Minute, c.DaemonIdleD(), "DaemonIdleD default")
	assert.Equal(t, 5*time.Minute, c.SyncIntervalD(), "SyncIntervalD default")
	assert.Equal(t, time.Hour, c.BackupIntervalD(), "BackupIntervalD default")
	assert.Equal(t, 3, c.BackupKeepN(), "BackupKeepN default")
	assert.EqualValues(t, 5*1024*1024, c.LogMaxBytes(), "LogMaxBytes default")
	assert.Equal(t, 1, c.LogKeepN(), "LogKeepN default")

	// Explicit "0" interval disables backups (durOr alone would give the default).
	assert.EqualValues(t, 0, Config{BackupInterval: "0"}.BackupIntervalD(), "backup_interval 0 disables")
	assert.Equal(t, 30*time.Minute, Config{BackupInterval: "30m"}.BackupIntervalD(), "backup_interval honored")

	// Overrides.
	assert.Equal(t, 7, Config{BackupKeep: 7}.BackupKeepN(), "BackupKeep override")
	assert.Equal(t, 4, Config{LogKeep: 4}.LogKeepN(), "LogKeep override")
	assert.EqualValues(t, 0, Config{LogMaxSize: "0"}.LogMaxBytes(), "log_max_size 0 disables rotation")

	// push_debounce is EXPERIMENTAL and OFF by default; a valid duration enables
	// it, while "0"/invalid stay disabled.
	assert.EqualValues(t, 0, c.PushDebounceD(), "PushDebounceD default off")
	assert.EqualValues(t, 0, Config{PushDebounce: "0"}.PushDebounceD(), "push_debounce 0 disabled")
	assert.EqualValues(t, 0, Config{PushDebounce: "nope"}.PushDebounceD(), "push_debounce invalid disabled")
	assert.Equal(t, 2*time.Second, Config{PushDebounce: "2s"}.PushDebounceD(), "push_debounce honored")
}

func TestBackupDir(t *testing.T) {
	assert.Equal(t, "/state/backups", BackupDir("/state"))
}
