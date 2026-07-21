package config

import (
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

func TestNewOpsAccessorDefaults(t *testing.T) {
	var c Config // zero value: every accessor supplies its default

	assert.Equal(t, time.Hour, c.BackupIntervalD(), "BackupIntervalD default")
	assert.Equal(t, 3, c.BackupKeepN(), "BackupKeepN default")
	assert.EqualValues(t, 5*1024*1024, c.LogMaxBytes(), "LogMaxBytes default")
	assert.Equal(t, 1, c.LogKeepN(), "LogKeepN default")
	assert.False(t, c.LogSilentOn(), "LogSilentOn default")

	// Explicit "0" interval disables backups (durOr alone would give the default).
	assert.EqualValues(t, 0, Config{BackupInterval: "0"}.BackupIntervalD(), "backup_interval 0 disables")
	// A set interval is honored.
	assert.Equal(t, 30*time.Minute, Config{BackupInterval: "30m"}.BackupIntervalD(), "backup_interval honored")

	// Overrides.
	assert.Equal(t, 7, Config{BackupKeep: 7}.BackupKeepN(), "BackupKeep override")
	assert.Equal(t, 4, Config{LogKeep: 4}.LogKeepN(), "LogKeep override")
	assert.EqualValues(t, 0, Config{LogMaxSize: "0"}.LogMaxBytes(), "log_max_size 0 disables rotation")

	tru := true
	require.True(t, Config{LogSilent: &tru}.LogSilentOn(), "LogSilent true")
}

func TestBackupDir(t *testing.T) {
	assert.Equal(t, "/state/backups", BackupDir("/state"))
}
