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
	assert.True(t, d.LogSilent, "LogSilent default true")
	assert.False(t, d.EnterExecutes, "EnterExecutes default false: Enter reviews, it does not run")

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

	// A file that mentions only auto_deepen:false must keep the other defaults
	// on and flip just that one — the classic case an omitempty *bool got wrong.
	dir := t.TempDir()
	require.NoError(t, Save(dir, Config{AutoDeepen: false, LogSilent: true, ServerURL: "https://s"}))
	got, err := Load(dir)
	require.NoError(t, err)
	assert.False(t, got.AutoDeepen, "explicit false survives Save/Load")
	assert.True(t, got.LogSilent, "unrelated default stays true")
	assert.Equal(t, "https://s", got.ServerURL)

	// An absent key keeps its default in both directions: a default-true bool
	// must not fall to the zero value, and a default-false one must not rise.
	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(ConfigPath(dir2), []byte("server_url = \"https://x\"\n"), 0o600))
	got2, err := Load(dir2)
	require.NoError(t, err)
	assert.True(t, got2.LogSilent, "absent key keeps default true")
	assert.False(t, got2.EnterExecutes, "absent key keeps default false")
	assert.Equal(t, "https://x", got2.ServerURL)

	// An explicit true on a default-false bool survives too.
	dir3 := t.TempDir()
	require.NoError(t, Save(dir3, Config{EnterExecutes: true}))
	got3, err := Load(dir3)
	require.NoError(t, err)
	assert.True(t, got3.EnterExecutes, "explicit true survives Save/Load")
}

// TestGetSet covers the config accessor pair the shell integration and
// get-config/set-config rely on: values round-trip through config.toml by their
// TOML key, defaults are reported for absent keys, and type/key errors are
// reported rather than silently written.
func TestGetSet(t *testing.T) {
	dir := t.TempDir()

	// A default-false bool reads back "false" before anything is written; the
	// shell integration asks exactly this question on every Ctrl-R.
	v, err := Get(dir, "enter_executes")
	require.NoError(t, err)
	assert.Equal(t, "false", v, "default reported for an absent key")

	// Set flips it and persists; Get reflects the new value.
	require.NoError(t, Set(dir, "enter_executes", "true"))
	v, err = Get(dir, "enter_executes")
	require.NoError(t, err)
	assert.Equal(t, "true", v, "set value round-trips")

	// A string key and an int key round-trip too.
	require.NoError(t, Set(dir, "server_url", "https://sync.example"))
	require.NoError(t, Set(dir, "backup_keep", "7"))
	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "https://sync.example", got.ServerURL)
	assert.Equal(t, 7, got.BackupKeep)
	assert.True(t, got.EnterExecutes, "earlier set is preserved across further sets")

	// A bad type and an unknown key are errors, and neither writes anything.
	require.Error(t, Set(dir, "backup_keep", "not-a-number"), "non-integer rejected")
	require.Error(t, Set(dir, "no_such_key", "x"), "unknown key rejected")
	_, err = Get(dir, "no_such_key")
	require.Error(t, err, "unknown key on Get is an error")

	after, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 7, after.BackupKeep, "a rejected Set left the file unchanged")
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

// TestUIStateRoundTrip proves the browser's remembered layout persists to its
// OWN file, leaving the user's hand-edited config.toml untouched.
func TestUIStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Nothing saved yet: the zero state, and no error.
	got, err := LoadUI(dir)
	require.NoError(t, err, "a missing ui.toml is not an error")
	require.Equal(t, UIState{}, got)

	want := UIState{BrowseLeftSplit: 333, BrowseTopSplit: 700, AgentLeftSplit: 260, AgentTopSplit: 550}
	require.NoError(t, SaveUI(dir, want))

	got, err = LoadUI(dir)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// It is a separate file, and toml was never created by saving it.
	require.NotEqual(t, ConfigPath(dir), UIStatePath(dir))
	_, statErr := os.Stat(ConfigPath(dir))
	require.True(t, os.IsNotExist(statErr), "saving UI state must not write toml")

	fi, err := os.Stat(UIStatePath(dir))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestUIStateColumnsRoundTrip: a table's column choices persist beside the
// splits, keyed by table name and naming columns rather than numbering them.
func TestUIStateColumnsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := UIState{
		BrowseLeftSplit: 333,
		Columns: map[string]ColumnPrefs{
			"browse":  {Hidden: []string{"host", "tags"}, Sort: "dur", SortDesc: true},
			"prompts": {Sort: "cmds"},
		},
	}
	require.NoError(t, SaveUI(dir, want))

	got, err := LoadUI(dir)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// Hand-editable: the names are in the file as written, not as indexes.
	b, err := os.ReadFile(UIStatePath(dir))
	require.NoError(t, err)
	require.Contains(t, string(b), "[columns.browse]")
	require.Contains(t, string(b), "'host'")
	require.Contains(t, string(b), "sort = 'dur'")

	// A state with no column choices writes no columns section at all.
	require.NoError(t, SaveUI(dir, UIState{BrowseLeftSplit: 1}))
	b, err = os.ReadFile(UIStatePath(dir))
	require.NoError(t, err)
	require.NotContains(t, string(b), "columns")
}

// TestLoadUIMalformed proves a corrupt ui.toml degrades to defaults rather than
// blocking the browser.
func TestLoadUIMalformed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, EnsureDir(dir))
	require.NoError(t, os.WriteFile(UIStatePath(dir), []byte("not = = toml"), 0o600))

	got, err := LoadUI(dir)
	require.Error(t, err, "the parse failure is reported…")
	require.Equal(t, UIState{}, got, "…but callers that ignore it still get a usable state")
}
