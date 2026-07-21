// Package config resolves yore's single-footprint directory and settings.
// ALL client-side state lives under Dir() — nothing is ever written
// anywhere else in the user's home.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Dir returns the state directory: $YORE_DIR, else ~/.config/yore.
func Dir() string {
	if d := os.Getenv("YORE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".yore" // last resort: relative; practically unreachable
	}
	return filepath.Join(home, ".config", "yore")
}

func DBPath(dir string) string     { return filepath.Join(dir, "data.db") }
func KeyPath(dir string) string    { return filepath.Join(dir, "device.key") }
func SpoolDir(dir string) string   { return filepath.Join(dir, "spool") }
func SocketPath(dir string) string { return filepath.Join(dir, "daemon.sock") }
func ConfigPath(dir string) string { return filepath.Join(dir, "config.json") }
func BackupDir(dir string) string  { return filepath.Join(dir, "backups") }

// Config is ~/.config/yore/config.json. Zero values mean "use default";
// accessor methods apply defaults so callers never branch.
type Config struct {
	ServerURL string `json:"server_url,omitempty"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	// ServerPin, when set, pins the server's TLS certificate: the base64 SHA-256
	// of its SubjectPublicKeyInfo. The syncer refuses to connect unless the
	// leaf cert matches — defeating TLS-inspecting proxies (fail-closed) but
	// also preventing sync through one. Captured at `yore setup --pin`.
	ServerPin    string `json:"server_pin,omitempty"`
	KeyEpoch     string `json:"key_epoch,omitempty"`     // default 24h
	DaemonIdle   string `json:"daemon_idle,omitempty"`   // default 30m
	SyncInterval string `json:"sync_interval,omitempty"` // default 5m
	AutoDeepen   *bool  `json:"auto_deepen,omitempty"`   // default true

	// EnterExecutes controls the Ctrl-R search widget: when true/unset, accepting
	// a result with Enter runs it immediately (Atuin parity); set false to insert
	// it into the prompt for review instead. Read directly by the emitted shell
	// integration; see internal/shell/assets.
	EnterExecutes *bool `json:"enter_executes,omitempty"` // default true

	// BindUpArrow, when true, also binds the Up arrow to the search TUI (in
	// addition to Ctrl-R), Atuin-style. Off by default because it changes a
	// very muscle-memoried key. Read by the emitted shell integration.
	BindUpArrow *bool `json:"bind_up_arrow,omitempty"` // default false

	// Keymap selects the interactive key style for the TUIs (Atuin keymap_mode
	// parity): "emacs" (default, also the empty value) or "vim". Vim mode adds
	// vi-style navigation to `yore browse` and an insert/normal sub-mode to the
	// Ctrl-R search widget. The CLI passes this through to each TUI's Options.
	Keymap string `json:"keymap,omitempty"` // default "emacs"

	// Integration picks how deeply the emitted shell hooks take over history:
	//   "takeover" (default) — yore is the single source of truth: the shell's
	//     persistent history is disabled, its in-memory list is seeded from yore
	//     and gated by yore's redaction (so !N / up-arrow work against yore-
	//     consistent, secret-free history), and Ctrl-R / up-arrow / h / hs are
	//     yore.
	//   "coexist"  — record alongside the shell's own history (untouched); rebind
	//     Ctrl-R and add h/hs. Native !N works against native history.
	//   "capture"  — only record; no keybinding or alias changes.
	Integration string `json:"integration,omitempty"` // default "takeover"

	// Recording filters (see internal/redact). Commands matching a built-in
	// secret pattern or any of these user regexes are never recorded; commands
	// run under an ignored directory are never recorded; a leading space skips
	// recording (histignorespace convention) unless RecordSpacePrefixed=true.
	IgnorePatterns      []string `json:"ignore_patterns,omitempty"`
	IgnoreDirs          []string `json:"ignore_dirs,omitempty"`
	RecordSpacePrefixed *bool    `json:"record_space_prefixed,omitempty"` // default false

	// Rolling local-db backup. The daemon writes a consistent snapshot of
	// data.db into BackupDir on BackupInterval, keeping the newest BackupKeep.
	BackupInterval string `json:"backup_interval,omitempty"` // default 1h; "0" disables
	BackupKeep     int    `json:"backup_keep,omitempty"`     // default 3

	// daemon.log size cap. When LogMaxSize > 0 the log rotates once it would
	// exceed that many bytes, keeping LogKeep old segments; "0" disables
	// rotation (plain unbounded append). LogSilent suppresses the log entirely
	// (no file is created and logging is a no-op).
	LogMaxSize string `json:"log_max_size,omitempty"` // default "5MB"; "0" disables rotation
	LogKeep    int    `json:"log_keep,omitempty"`     // default 1
	LogSilent  *bool  `json:"log_silent,omitempty"`   // default false
}

// RecordSpacePrefixedOn reports whether space-prefixed commands are recorded.
func (c Config) RecordSpacePrefixedOn() bool {
	return c.RecordSpacePrefixed != nil && *c.RecordSpacePrefixed
}

// EnterExecutesOn reports whether accepting a Ctrl-R result runs it immediately
// (Atuin parity) rather than inserting it into the prompt for review.
func (c Config) EnterExecutesOn() bool {
	return c.EnterExecutes == nil || *c.EnterExecutes
}

// KeymapVim reports whether the vim keymap is selected (Atuin keymap_mode=vim
// parity). The empty value and "emacs" both keep the default emacs bindings.
func (c Config) KeymapVim() bool { return c.Keymap == "vim" }

// IntegrationMode returns the shell-integration mode, defaulting to "takeover".
func (c Config) IntegrationMode() string {
	switch c.Integration {
	case "coexist", "capture":
		return c.Integration
	default:
		return "takeover"
	}
}

func (c Config) KeyEpochD() time.Duration     { return durOr(c.KeyEpoch, 24*time.Hour) }
func (c Config) DaemonIdleD() time.Duration   { return durOr(c.DaemonIdle, 30*time.Minute) }
func (c Config) SyncIntervalD() time.Duration { return durOr(c.SyncInterval, 5*time.Minute) }
func (c Config) AutoDeepenOn() bool           { return c.AutoDeepen == nil || *c.AutoDeepen }
func (c Config) BindUpArrowOn() bool          { return c.BindUpArrow != nil && *c.BindUpArrow }

// BackupIntervalD is how often the daemon writes a db backup; an explicit "0"
// disables backups. Default 1h. (durOr treats "0" as "use default", so the
// disable case is handled here rather than in the shared helper.)
func (c Config) BackupIntervalD() time.Duration {
	if strings.TrimSpace(c.BackupInterval) == "0" {
		return 0
	}
	return durOr(c.BackupInterval, time.Hour)
}

// BackupKeepN is how many backup files to retain, newest first. Default 3.
func (c Config) BackupKeepN() int {
	if c.BackupKeep > 0 {
		return c.BackupKeep
	}
	return 3
}

// LogMaxBytes is the daemon.log rotation threshold in bytes: an empty value
// means the 5MB default, "0" disables rotation, and any humanized size
// ("5MB", "512KB", "1GB", case-insensitive) or plain byte count is honored.
func (c Config) LogMaxBytes() int64 { return parseSize(c.LogMaxSize, 5*1024*1024) }

// LogKeepN is how many rotated daemon.log segments to keep. Default 1.
func (c Config) LogKeepN() int {
	if c.LogKeep > 0 {
		return c.LogKeep
	}
	return 1
}

// LogSilentOn reports whether daemon logging is suppressed entirely.
func (c Config) LogSilentOn() bool { return c.LogSilent != nil && *c.LogSilent }

func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// parseSize parses a humanized byte size: a bare integer is bytes, a "KB"/"MB"/
// "GB" suffix (case-insensitive, binary multiples) scales it. An empty string
// yields def; an explicit "0" yields 0 (used to disable). Anything unparseable
// also yields def, so a typo never silently removes the size cap.
func parseSize(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	up := strings.ToUpper(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(up, "KB"):
		mult, up = 1024, strings.TrimSpace(strings.TrimSuffix(up, "KB"))
	case strings.HasSuffix(up, "MB"):
		mult, up = 1024*1024, strings.TrimSpace(strings.TrimSuffix(up, "MB"))
	case strings.HasSuffix(up, "GB"):
		mult, up = 1024*1024*1024, strings.TrimSpace(strings.TrimSuffix(up, "GB"))
	case strings.HasSuffix(up, "B"):
		up = strings.TrimSpace(strings.TrimSuffix(up, "B"))
	}
	n, err := strconv.ParseInt(up, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n * mult
}

// Load reads config.json from dir; a missing file yields the zero Config.
func Load(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

// Save writes config.json (0600).
func Save(dir string, c Config) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(dir), append(b, '\n'), 0o600)
}

// EnsureDir creates the state directory tree with private permissions.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(SpoolDir(dir), 0o700); err != nil {
		return err
	}
	// MkdirAll leaves pre-existing dirs' modes alone; enforce ours.
	return os.Chmod(dir, 0o700)
}
