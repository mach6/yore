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

	// EnterExecutes controls the Ctrl-R search widget: when true, accepting a
	// result with Enter runs it immediately (Atuin parity); when false/unset it
	// is inserted into the prompt for review. Read directly by the emitted shell
	// integration; see internal/shell/assets.
	EnterExecutes *bool `json:"enter_executes,omitempty"` // default false

	// BindUpArrow, when true, also binds the Up arrow to the search TUI (in
	// addition to Ctrl-R), Atuin-style. Off by default because it changes a
	// very muscle-memoried key. Read by the emitted shell integration.
	BindUpArrow *bool `json:"bind_up_arrow,omitempty"` // default false

	// Keymap selects the interactive key style for the TUIs (Atuin keymap_mode
	// parity): "emacs" (default, also the empty value) or "vim". Vim mode adds
	// vi-style navigation to `yore browse` and an insert/normal sub-mode to the
	// Ctrl-R search widget. The CLI passes this through to each TUI's Options.
	Keymap string `json:"keymap,omitempty"` // default "emacs"

	// Recording filters (see internal/redact). Commands matching a built-in
	// secret pattern or any of these user regexes are never recorded; commands
	// run under an ignored directory are never recorded; a leading space skips
	// recording (histignorespace convention) unless RecordSpacePrefixed=true.
	IgnorePatterns      []string `json:"ignore_patterns,omitempty"`
	IgnoreDirs          []string `json:"ignore_dirs,omitempty"`
	RecordSpacePrefixed *bool    `json:"record_space_prefixed,omitempty"` // default false
}

// RecordSpacePrefixedOn reports whether space-prefixed commands are recorded.
func (c Config) RecordSpacePrefixedOn() bool {
	return c.RecordSpacePrefixed != nil && *c.RecordSpacePrefixed
}

// EnterExecutesOn reports whether accepting a Ctrl-R result runs it immediately
// (Atuin parity) rather than inserting it into the prompt for review.
func (c Config) EnterExecutesOn() bool {
	return c.EnterExecutes != nil && *c.EnterExecutes
}

// KeymapVim reports whether the vim keymap is selected (Atuin keymap_mode=vim
// parity). The empty value and "emacs" both keep the default emacs bindings.
func (c Config) KeymapVim() bool { return c.Keymap == "vim" }

func (c Config) KeyEpochD() time.Duration     { return durOr(c.KeyEpoch, 24*time.Hour) }
func (c Config) DaemonIdleD() time.Duration   { return durOr(c.DaemonIdle, 30*time.Minute) }
func (c Config) SyncIntervalD() time.Duration { return durOr(c.SyncInterval, 5*time.Minute) }
func (c Config) AutoDeepenOn() bool           { return c.AutoDeepen == nil || *c.AutoDeepen }
func (c Config) BindUpArrowOn() bool          { return c.BindUpArrow != nil && *c.BindUpArrow }

func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
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
	return c, json.Unmarshal(b, &c)
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
