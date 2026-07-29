// Package config resolves yore's single-footprint directory and settings.
// ALL client-side state lives under Dir() — nothing is ever written
// anywhere else in the user's home.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
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
func SpoolDir(dir string) string   { return filepath.Join(dir, "spool") }
func SocketPath(dir string) string { return filepath.Join(dir, "daemon.sock") }
func ConfigPath(dir string) string { return filepath.Join(dir, "config.toml") }
func RedactPath(dir string) string { return filepath.Join(dir, "redact.yml") }
func BackupDir(dir string) string  { return filepath.Join(dir, "backups") }

// Config is ~/.config/yore/config.toml (TOML — no JSON quoting or trailing-comma
// quirks to trip over in a hand-edited file).
//
// Defaults come from Defaults(), which Load() seeds *before* decoding the file
// over it — so an absent key keeps its default and an explicit value (including
// false / 0) overrides it, using go-toml's decode-into semantics rather than any
// *bool "was it set?" bookkeeping. Booleans therefore carry their effective
// value directly (no accessor indirection). Duration and size settings are
// stored as human strings ("24h", "5MB") and resolved by the typed accessors
// below (KeyEpochD, LogMaxBytes, …), which also fall back to the default if the
// stored value is malformed. No consumer parses this file by hand — the shell
// integration and other callers read values through Get / `yore get-config`.
type Config struct {
	ServerURL string `toml:"server_url,omitempty"`
	// TokenFile optionally points at a file holding the auth token, for setups
	// that manage it externally. The token itself is NEVER stored here — it
	// lives in the OS keyring, or a 0600 file when no keyring is available (see
	// internal/secret) — so config.toml holds no secrets and stays safe to
	// read, diff, and share.
	TokenFile string `toml:"token_file,omitempty"`
	// ServerPin, when set, pins the server's TLS certificate: the base64 SHA-256
	// of its SubjectPublicKeyInfo. The syncer refuses to connect unless the
	// leaf cert matches — defeating TLS-inspecting proxies (fail-closed) but
	// also preventing sync through one. Captured at `yore setup --pin`.
	ServerPin    string `toml:"server_pin,omitempty"`
	KeyEpoch     string `toml:"key_epoch,omitempty"`     // default 24h
	DaemonIdle   string `toml:"daemon_idle,omitempty"`   // default 30m
	SyncInterval string `toml:"sync_interval,omitempty"` // default 5m
	// PushDebounce is EXPERIMENTAL and OFF by default. Set it (e.g. "2s") to have
	// the daemon push shortly after a command is recorded — coalescing a burst
	// into one delta push — so cross-host propagation is seconds instead of up to
	// SyncInterval. Empty/"0"/invalid leaves it disabled; only the periodic tick
	// pushes. (Note: an agent that fires commands in bursts will push at up to one
	// cycle per PushDebounce for the whole run — that's why it's opt-in.)
	PushDebounce string `toml:"push_debounce,omitempty"` // EXPERIMENTAL; default off

	// SyncPrompts controls whether agent prompt records leave this machine. The
	// server only ever holds ciphertext either way, so this is not about trusting
	// the server — it is about blast radius: a prompt is far more likely than a
	// command to carry pasted secrets, customer data, or context you would rather
	// not have decryptable by every device in the group. Off keeps prompts
	// readable on the machine that recorded them and nowhere else; the commands
	// they caused still sync, and still show as agent commands, they just have no
	// prompt text on other hosts. Defaults true — so no omitempty: an explicit
	// false must survive a Save/Load round-trip. Not retroactive: prompts already
	// pushed stay on the server.
	SyncPrompts bool `toml:"sync_prompts"` // default true

	// RemoteKeep caps how many of each OTHER host's records this machine caches
	// and holds in RAM — the newest RemoteKeep per host. Remote history is
	// unbounded over years, and all of it decrypted in a background daemon is
	// not; this is the bound. Searching a remote host reaches back this far.
	// 0 means unlimited (the old behaviour; a deliberate choice, not a default).
	RemoteKeep int `toml:"remote_keep,omitempty"` // default 50000

	// AutoDeepen lets a shallow (local) search that finds little transparently
	// extend to all hosts when the server is reachable. Defaults true — so it has
	// no omitempty: an explicit false must survive a Save/Load round-trip.
	AutoDeepen bool `toml:"auto_deepen"` // default true

	// EnterExecutes controls the Ctrl-R search widget: when true, accepting a
	// result with Enter runs it immediately (Atuin parity); false inserts it into
	// the prompt for review instead. The emitted shell integration reads this at
	// runtime via `yore get-config enter_executes` (never by parsing the file),
	// so an explicit false must round-trip — hence no omitempty.
	EnterExecutes bool `toml:"enter_executes"` // default true

	// BindUpArrow also binds the Up arrow to the search TUI (in addition to
	// Ctrl-R), Atuin-style. Off by default because it changes a very
	// muscle-memoried key. The shell integration reads it via `yore get-config`.
	BindUpArrow bool `toml:"bind_up_arrow,omitempty"` // default false

	// HideAgentCommands keeps agent-run commands out of the interactive search
	// UIs, where one prompt's forty tool invocations otherwise bury a morning of
	// the user's own work. The agent explorer (`a`) is where that history
	// belongs — grouped under the prompt that caused it rather than interleaved.
	// Default true; `A` in the browser and `⌥a` in the Ctrl-R panel toggle it for
	// the session, and both always say how many rows the filter is holding back.
	//
	// It governs only the interactive UIs. `--headless` never hides anything: it
	// feeds scripts, which want the whole archive and have no status line to be
	// told what was withheld. An explicit false must round-trip, hence no
	// omitempty.
	HideAgentCommands bool `toml:"hide_agent_commands"` // default true

	// Keymap selects the interactive key style for the TUIs (Atuin keymap_mode
	// parity): "emacs" (default, also the empty value) or "vim". Vim mode adds
	// vi-style navigation to `yore browse` and an insert/normal sub-mode to the
	// Ctrl-R search widget. The CLI passes this through to each TUI's Options.
	Keymap string `toml:"keymap,omitempty"` // default "emacs"

	// Integration picks how deeply the emitted shell hooks take over history:
	//   "takeover" (default) — yore is the single source of truth: the shell's
	//     persistent history is disabled, its in-memory list is seeded from yore
	//     and gated by yore's redaction (so !N / up-arrow work against yore-
	//     consistent, secret-free history), and Ctrl-R / up-arrow / h / hs are
	//     yore.
	//   "coexist"  — record alongside the shell's own history (untouched); rebind
	//     Ctrl-R and add h/hs. Native !N works against native history.
	//   "capture"  — only record; no keybinding or alias changes.
	Integration string `toml:"integration,omitempty"` // default "takeover"

	// Recording filters (see internal/redact). Commands matching a built-in
	// secret pattern or any of these user regexes are never recorded; commands
	// run under an ignored directory are never recorded; a leading space skips
	// recording (histignorespace convention) unless RecordSpacePrefixed=true.
	IgnorePatterns      []string `toml:"ignore_patterns,omitempty"`
	IgnoreDirs          []string `toml:"ignore_dirs,omitempty"`
	RecordSpacePrefixed bool     `toml:"record_space_prefixed,omitempty"` // default false

	// AutoTags maps a directory prefix to a tag name: commands run in (or under)
	// a matching directory carry that freeform tag, resolved at query time by the
	// daemon (so there is no cost on the record path). Longest prefix wins.
	// Set via `yore set-config auto_tags "/work/proj=refactor,/personal=home"`.
	AutoTags map[string]string `toml:"auto_tags,omitempty"`

	// CaptureSpoolOnly makes the capture path (the shell hook and the agent
	// hooks) write the record to the spool and stop there: it does NOT poke or
	// spawn the daemon. Off by default — normally a capture nudges the daemon so
	// the record is ingested and searchable within milliseconds. With this on,
	// nothing touches the daemon on the capture path; the spool is drained the
	// next time a daemon runs (your next `hs`/`hb` spawns one, which ingests the
	// whole spool at startup). The trade is slightly staler search for a machine
	// where capture never starts a background process — useful with no sync
	// server configured. Records are never lost either way; only ingest timing
	// changes.
	CaptureSpoolOnly bool `toml:"capture_spool_only,omitempty"` // default false

	// Rolling local-db backup. The daemon writes a consistent snapshot of
	// data.db into BackupDir on BackupInterval, keeping the newest BackupKeep.
	BackupInterval string `toml:"backup_interval,omitempty"` // default 1h; "0" disables
	BackupKeep     int    `toml:"backup_keep,omitempty"`     // default 3

	// daemon.log size cap. When LogMaxSize > 0 the log rotates once it would
	// exceed that many bytes, keeping LogKeep old segments; "0" disables
	// rotation (plain unbounded append). LogSilent suppresses the log entirely
	// (no file is created and logging is a no-op) — the default; set it false to
	// write a rotating daemon.log for debugging. Defaults true, so no omitempty:
	// an explicit false must round-trip.
	LogMaxSize string `toml:"log_max_size,omitempty"` // default "5MB"; "0" disables rotation
	LogKeep    int    `toml:"log_keep,omitempty"`     // default 1
	LogSilent  bool   `toml:"log_silent"`             // default true (no daemon.log)
}

// Defaults returns the configuration used when config.toml is absent. Load()
// seeds this before decoding the file over it, so every setting the user does
// not mention keeps the value here. Booleans whose default is true are the
// reason this exists — a decoder cannot distinguish "absent" from "false" once
// seeded, which is exactly the behavior we want: absent → default, present →
// override. String/int settings left at their zero value here are defaulted by
// their typed accessors instead (single source of truth for those).
func Defaults() Config {
	return Config{
		AutoDeepen:        true,
		EnterExecutes:     true,
		LogSilent:         true,
		SyncPrompts:       true,
		HideAgentCommands: true,
	}
}

// RemoteKeepN is how many of each remote host's records to cache and hold in
// RAM, newest first. Default 50000; an explicit negative value means unlimited.
func (c Config) RemoteKeepN() int {
	switch {
	case c.RemoteKeep > 0:
		return c.RemoteKeep
	case c.RemoteKeep < 0:
		return 0 // unlimited
	default:
		return 50000
	}
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

// PushDebounceD is EXPERIMENTAL and OFF by default (0). A valid duration enables
// push-on-record — the daemon pushes that long after new records are ingested,
// coalescing bursts. Empty/"0"/invalid all resolve to 0 (disabled), so only the
// periodic SyncInterval tick pushes.
func (c Config) PushDebounceD() time.Duration { return durOr(c.PushDebounce, 0) }

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

// Load reads config.toml from dir over a Defaults() base: a missing file yields
// Defaults(); a present file overrides only the keys it names. A read or parse
// error returns Defaults() (plus the error) so callers that ignore it still get
// a usable configuration rather than an empty one.
func Load(dir string) (Config, error) {
	c := Defaults()
	b, err := os.ReadFile(ConfigPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Defaults(), err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return Defaults(), err
	}
	return c, nil
}

// Save writes config.toml (0600).
func Save(dir string, c Config) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
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

// Get returns the effective value of a config key (its TOML field name) as a
// string: "true"/"false" for booleans, the raw text for strings, a decimal for
// integers, comma-separated items for string lists. An empty string means the
// key is at its default. This is the one authoritative reader — the shell
// integration and `yore get-config` use it, so nothing parses config.toml by
// hand. An unknown key is an error.
func Get(dir, key string) (string, error) {
	c, err := Load(dir)
	if err != nil {
		return "", err
	}
	f, ok := fieldByKey(reflect.ValueOf(c), key)
	if !ok {
		return "", fmt.Errorf("unknown config key %q", key)
	}
	return formatField(f), nil
}

// Set assigns a config key from a string value and persists config.toml
// (creating it if absent). The value is parsed into the field's type — bool,
// string, integer, or comma-separated list; a value that does not fit the type
// is an error and nothing is written. An unknown key is an error.
func Set(dir, key, value string) error {
	c, err := Load(dir)
	if err != nil {
		return err
	}
	f, ok := fieldByKey(reflect.ValueOf(&c).Elem(), key)
	if !ok {
		return fmt.Errorf("unknown config key %q", key)
	}
	if err := assignField(f, value); err != nil {
		return fmt.Errorf("config %s: %w", key, err)
	}
	return Save(dir, c)
}

// fieldByKey finds the Config field whose TOML tag name equals key.
func fieldByKey(v reflect.Value, key string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
		if name == key {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// formatField renders a field value as the string get-config prints.
func formatField(f reflect.Value) string {
	switch f.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(f.Bool())
	case reflect.String:
		return f.String()
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(f.Int(), 10)
	case reflect.Slice:
		parts := make([]string, f.Len())
		for i := range parts {
			parts[i] = f.Index(i).String()
		}
		return strings.Join(parts, ",")
	case reflect.Map:
		keys := make([]string, 0, f.Len())
		for _, k := range f.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + f.MapIndex(reflect.ValueOf(k)).String()
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

// assignField parses value into f according to f's kind.
func assignField(f reflect.Value, value string) error {
	switch f.Kind() {
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("want a boolean (true/false), got %q", value)
		}
		f.SetBool(b)
	case reflect.String:
		f.SetString(value)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("want an integer, got %q", value)
		}
		f.SetInt(n)
	case reflect.Slice:
		parts := []string{}
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		f.Set(reflect.ValueOf(parts))
	case reflect.Map:
		m := map[string]string{}
		for _, pair := range strings.Split(value, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				return fmt.Errorf("want key=value pairs, got %q", pair)
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		f.Set(reflect.ValueOf(m))
	default:
		return fmt.Errorf("unsupported field kind %s", f.Kind())
	}
	return nil
}

// --- UI state ---------------------------------------------------------------

// UIStatePath is the browser's remembered-layout file. It sits beside
// config.toml but is deliberately SEPARATE: config.toml is the user's
// hand-edited settings file, and a TUI that rewrote (and so reformatted) it
// every time a pane was dragged would be a poor neighbour.
func UIStatePath(dir string) string { return filepath.Join(dir, "ui.toml") }

// UIState is layout the browser remembers between runs. Every field is a
// divider position in per-mille of the axis it cuts; zero means "never dragged",
// so the view falls back to its own default proportions.
type UIState struct {
	BrowseLeftSplit int `toml:"browse_left_split,omitempty"`
	BrowseTopSplit  int `toml:"browse_top_split,omitempty"`
	AgentLeftSplit  int `toml:"agent_left_split,omitempty"`
	AgentTopSplit   int `toml:"agent_top_split,omitempty"`
}

// LoadUI reads ui.toml. A missing or unreadable file yields the zero state:
// remembered layout is a convenience, never a reason to fail to open the
// browser, so callers can ignore the error and still get something usable.
func LoadUI(dir string) (UIState, error) {
	var s UIState
	b, err := os.ReadFile(UIStatePath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return UIState{}, err
	}
	if err := toml.Unmarshal(b, &s); err != nil {
		return UIState{}, err
	}
	return s, nil
}

// SaveUI writes ui.toml (0600).
func SaveUI(dir string, s UIState) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	b, err := toml.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(UIStatePath(dir), append(b, '\n'), 0o600)
}
