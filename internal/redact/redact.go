// Package redact is yore's recording security gate. It decides whether a
// captured shell command carries a secret (and so must NEVER be spooled,
// stored, or synced) or was run under a directory the user asked to ignore.
//
// Two failure modes matter and pull in opposite directions:
//
//   - A false negative leaks a secret: the command is recorded and later
//     synced to a server (encrypted, but still a persisted copy of a live
//     credential).
//   - A false positive silently drops history the user wanted to keep.
//
// So every built-in rule anchors on the *shape* of a secret value or on an
// unambiguous assignment/flag context — never on a bare keyword. "password"
// in a commit message, "token" in a path, or a git SHA must all pass through
// untouched, while `export DB_PASSWORD=hunter2` or an AKIA key must not.
//
// Sensitive is the hot path (every recorded command runs through it) and is
// allocation-free for the overwhelmingly common case of a clean command: a
// cheap literal pre-scan (see rule.hints) rejects a command before any regex
// engine is touched. Reason exposes the matching rule's name for a future
// `yore doctor`/debug mode and is not on the hot path.
package redact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"yore/internal/config"
)

// rule is one built-in (or user) detector. re is the authority; hints is a
// set of cheap literal substrings of which at least one MUST be present for
// re to have any chance of matching. When hints is empty the rule always runs
// its regex (used for user patterns, whose shape we can't predict). fold makes
// the hint scan ASCII-case-insensitive (hints must then be lowercase).
type rule struct {
	name  string
	re    *regexp.Regexp
	hints []string
	fold  bool
}

// builtins is the ordered built-in rule table, compiled once at init and
// shared by every Filter. Order is significant: Reason returns the FIRST
// matching rule, so shape-specific and tool-specific rules precede the broad
// assignment catch-alls (generic-token-assign, env-secret-export).
//
// name -> intent:
//
//	pem-block            a pasted PEM private-key header
//	jwt                  a JSON Web Token (two base64url JSON segments)
//	aws-access-key       an AWS access key id (AKIA / ASIA temp)
//	aws-secret           an AWS secret access key value in assignment context
//	github-token         a GitHub PAT / OAuth / app token (ghp_ … / github_pat_)
//	slack-token          a Slack API token (xoxb-/xoxa-/xoxp-/xoxr-/xoxs-)
//	url-userinfo         a password embedded in a URL (scheme://user:pass@host)
//	openssl-passin       an inline passphrase to openssl (-passin pass:…)
//	gpg-passphrase       an inline passphrase to gpg (--passphrase …)
//	sshpass-password     sshpass -p <password> (its whole purpose is inline pw)
//	mysql-password       mysql/mariadb -p<pw> (attached only; see notes below)
//	mongo-password       mongosh/mongo -p <pw> / --password <pw>
//	smbclient-password   smbclient -U user%pass / --password
//	curl-userpass        curl -u user:pass / --user user:pass
//	wget-password        wget --password=<pw> (and --http-/--ftp-/--proxy-)
//	generic-token-assign token/secret/password/api-key/auth = <value> anywhere
//	env-secret-export    a leading VAR=<value> whose name embeds a secret word
var builtins = compile(defaultSpecs)

// defaultSpecs is the ordered built-in rule table in its editable, pre-compiled
// form. It both compiles into builtins (above) and seeds ~/.config/yore/redact.yml
// via Seed/DefaultSpecs, so the two can never drift.
var defaultSpecs = []Spec{
	{"pem-block", `-----BEGIN [A-Z ]*PRIVATE KEY-----`, []string{"-----BEGIN"}, false},
	{"jwt", `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`, []string{"eyJ"}, false},

	{"aws-access-key", `(AKIA|ASIA)[0-9A-Z]{16}`, []string{"AKIA", "ASIA"}, false},
	{"aws-secret", `(?i)aws_secret_access_key["']?\s*[=: ]+\s*["']?[A-Za-z0-9/+]{40}`, []string{"aws_secret"}, true},

	{"github-token", `gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`,
		[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, false},
	{"slack-token", `xox[baprs]-[A-Za-z0-9-]{10,}`, []string{"xox"}, false},

	{"url-userinfo", `[a-z][a-z0-9+.-]*://[^/@:\s]*:[^/@\s]+@`, []string{"://"}, false},

	{"openssl-passin", `(?i:openssl)\b.*\s-(passin|passout|pass)[ =]pass:\S`, []string{"openssl"}, true},
	{"gpg-passphrase", `(?i:gpg)\b.*--passphrase[ =]\S`, []string{"gpg"}, true},

	{"sshpass-password", `(?i:sshpass)\b.*\s-p\s*\S`, []string{"sshpass"}, true},
	{"mysql-password", `^\s*(?i:mysql|mariadb|mysqldump)\b.*\s(-p\S|--password=\S)`, []string{"mysql", "mariadb", "mysqldump"}, true},
	{"mongo-password", `^\s*(?i:mongosh|mongo)\b.*(\s-p\s*\S|--password[ =]\S)`, []string{"mongo"}, true},
	{"smbclient-password", `^\s*(?i:smbclient)\b.*(-U\s*\S+%\S|--password[ =]\S)`, []string{"smbclient"}, true},
	{"curl-userpass", `(?i:curl)\b.*\s(-u\s*|--user[ =])[^\s:]+:\S`, []string{"curl"}, true},
	{"wget-password", `(?i:wget)\b.*--(http-|ftp-|proxy-)?passw(or)?d[= ]\S`, []string{"wget"}, true},

	{"generic-token-assign", `(?i)(token|secret|passw(or)?d|api[_-]?key|auth)[=:]\S{6,}`,
		[]string{"token", "secret", "passw", "apikey", "api_key", "api-key", "auth"}, true},
	{"env-secret-export", `(?i)^\s*(export\s+)?[a-z0-9_]*(secret|token|password|passwd|apikey|api_key|credentials)[a-z0-9_]*=\S+`,
		[]string{"secret", "token", "password", "passwd", "apikey", "api_key", "credential"}, true},
}

// Spec is one redaction rule in its editable (pre-compilation) form: the YAML
// shape of ~/.config/yore/redact.yml and the seed form of the built-in table.
// Name identifies the rule (surfaced by Reason); Pattern is a Go regexp that is
// the authority for a match. Hints is an optional set of cheap literal
// substrings, at least one of which must be present for the regexp to run — a
// hot-path pre-filter; omit it to always run the regexp. Fold makes the hint
// scan ASCII-case-insensitive (hints must then be lowercase).
type Spec struct {
	Name    string   `yaml:"name"`
	Pattern string   `yaml:"pattern"`
	Hints   []string `yaml:"hints,omitempty"`
	Fold    bool     `yaml:"fold,omitempty"`
}

// DefaultSpecs returns a copy of the built-in rule table. It is the source both
// for the always-available in-memory fallback and for seeding redact.yml, so a
// user can see and edit exactly the rules that ship.
func DefaultSpecs() []Spec {
	out := make([]Spec, len(defaultSpecs))
	copy(out, defaultSpecs)
	return out
}

// compile turns specs into rules; a bad built-in pattern is a programmer error
// and panics at init (built-ins are constant and covered by tests). Rules loaded
// from redact.yml are compiled non-fatally via compileSpecs instead.
func compile(specs []Spec) []rule {
	rules := make([]rule, len(specs))
	for i, s := range specs {
		rules[i] = rule{name: s.Name, re: regexp.MustCompile(s.Pattern), hints: s.Hints, fold: s.Fold}
	}
	return rules
}

// Filter is a compiled set of built-in and user rules plus a set of ignored
// directory prefixes. It is safe for concurrent use (all fields are read-only
// after New) and its Sensitive method allocates nothing on the clean path.
type Filter struct {
	rules []rule   // built-ins followed by user patterns
	dirs  []string // filepath.Clean'd ignore-dir prefixes
}

// New compiles a Filter from the in-memory built-in table plus userPatterns.
// userPatterns are user-supplied regexes from config: an invalid one is skipped
// and returned in errs (never fatal), so one typo can't disable recording.
// ignoreDirs are absolute path prefixes; a command whose cwd is inside one is
// never recorded. Either slice may be nil/empty.
//
// New always uses the compiled-in built-ins and never touches redact.yml; use
// Load to honor an edited rules file (with New's built-ins as the fail-safe
// fallback).
func New(userPatterns, ignoreDirs []string) (f *Filter, errs []error) {
	base := make([]rule, len(builtins))
	copy(base, builtins)
	return assemble(base, userPatterns, ignoreDirs, nil)
}

// Load compiles a Filter from the editable rules file <dir>/redact.yml, then
// appends userPatterns and ignoreDirs exactly as New does. It is fail-SAFE: the
// ruleset is never empty, so a typo can never silently switch redaction off.
//
//   - Missing or unparseable redact.yml, or a file whose patterns: list is
//     empty, falls back to the compiled-in built-ins (DefaultSpecs) and appends
//     a non-fatal warning to errs — redaction stays fully on.
//   - A single invalid regexp inside an otherwise-valid file is skipped with a
//     warning (like an invalid user pattern); the remaining rules still apply.
//
// A user CAN intentionally drop individual rules by editing the file; only a
// broken/empty file triggers the whole-table fallback.
func Load(dir string, userPatterns, ignoreDirs []string) (*Filter, []error) {
	base, errs := loadBaseRules(dir)
	return assemble(base, userPatterns, ignoreDirs, errs)
}

// loadBaseRules reads redact.yml and compiles its patterns into the base rule
// set, applying the fail-safe fallback to the built-ins. Warnings (never fatal)
// are returned alongside the rules.
func loadBaseRules(dir string) (rules []rule, errs []error) {
	path := config.RedactPath(dir)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return builtinRules(), []error{fmt.Errorf("redact: %s not found; using built-in rules (run `yore setup` to seed an editable copy)", path)}
	case err != nil:
		return builtinRules(), []error{fmt.Errorf("redact: cannot read %s: %w; using built-in rules", path, err)}
	}

	var file struct {
		Patterns []Spec `yaml:"patterns"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		return builtinRules(), []error{fmt.Errorf("redact: cannot parse %s: %w; using built-in rules", path, err)}
	}
	if len(file.Patterns) == 0 {
		// An empty/typo'd file must never mean "redact nothing".
		return builtinRules(), []error{fmt.Errorf("redact: %s has no patterns; using built-in rules", path)}
	}

	rules, errs = compileSpecs(file.Patterns)
	if len(rules) == 0 {
		// Every rule in the file was invalid; fall back rather than run open.
		errs = append(errs, fmt.Errorf("redact: %s has no valid patterns; using built-in rules", path))
		return builtinRules(), errs
	}
	return rules, errs
}

// builtinRules returns a fresh copy of the compiled built-in rule table so the
// caller can append to it without touching the shared slice.
func builtinRules() []rule {
	base := make([]rule, len(builtins))
	copy(base, builtins)
	return base
}

// compileSpecs compiles editable specs into rules non-fatally: an invalid
// regexp is skipped with a warning (mirroring user-pattern handling) rather
// than panicking, so one bad line in redact.yml can't disable the rest.
func compileSpecs(specs []Spec) (rules []rule, errs []error) {
	rules = make([]rule, 0, len(specs))
	for _, s := range specs {
		if strings.TrimSpace(s.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("redact: skipping invalid pattern %q (%s): %w", s.Name, s.Pattern, err))
			continue
		}
		name := s.Name
		if name == "" {
			name = "unnamed"
		}
		rules = append(rules, rule{name: name, re: re, hints: s.Hints, fold: s.Fold})
	}
	return rules, errs
}

// assemble appends userPatterns to the base rules and attaches the ignore dirs,
// returning the finished Filter. It is the shared core of New and Load; errs
// accumulates any prior (base-load) warnings plus per-user-pattern ones.
func assemble(base []rule, userPatterns, ignoreDirs []string, errs []error) (*Filter, []error) {
	rules := base
	for _, p := range userPatterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("redact: skipping invalid user pattern %q: %w", p, err))
			continue
		}
		// No hints: a user pattern's shape is unknown, so it always runs.
		rules = append(rules, rule{name: "user:" + p, re: re})
	}

	dirs := make([]string, 0, len(ignoreDirs))
	for _, d := range ignoreDirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(d))
	}

	return &Filter{rules: rules, dirs: dirs}, errs
}

// Seed writes the built-in rules to <dir>/redact.yml so the user can see and
// edit them, but ONLY when the file does not already exist — it never clobbers
// edits, so it is safe to call on every setup. The file is written 0600 and
// atomically (temp file + rename), and the state dir is created if needed.
func Seed(dir string) error {
	path := config.RedactPath(dir)
	switch _, err := os.Stat(path); {
	case err == nil:
		return nil // already present: never overwrite the user's edits
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := config.EnsureDir(dir); err != nil {
		return err
	}

	body, err := marshalSpecs(DefaultSpecs())
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".redact-*.yml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// redactHeader documents the file for whoever opens it in an editor.
const redactHeader = `# yore secret-redaction rules — seeded from the built-ins; edit freely. A command
# matching any rule below is never recorded (and so never synced). Each rule has a
# name, a Go regexp pattern, optional literal hints (a fast pre-filter — at least
# one must be present for the regexp to run; omit to always run it), and an
# optional fold flag (case-insensitive hint match; hints must then be lowercase).
#
# Fail-safe: if this file is removed, unreadable, unparseable, or left with no
# patterns, yore falls back to the built-in rules — redaction never silently
# turns off. You can still delete individual rules you don't want.
`

// marshalSpecs renders specs as the redact.yml document (header + patterns).
func marshalSpecs(specs []Spec) ([]byte, error) {
	doc := struct {
		Patterns []Spec `yaml:"patterns"`
	}{Patterns: specs}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte(redactHeader), body...), nil
}

// Sensitive reports whether cmd matches any built-in or user rule. It is the
// hot path and does not allocate for a command that trips no rule's hints.
func (f *Filter) Sensitive(cmd string) bool { return f.match(cmd) >= 0 }

// NumRules is the count of compiled rules (loaded/built-in plus valid user
// patterns) backing this Filter. Intended for `yore doctor` reporting.
func (f *Filter) NumRules() int { return len(f.rules) }

// Reason returns the name of the first rule cmd matches, or "" if none.
// Intended for `yore doctor`/debug output, not the hot path.
func (f *Filter) Reason(cmd string) string {
	if i := f.match(cmd); i >= 0 {
		return f.rules[i].name
	}
	return ""
}

// match returns the index of the first matching rule, or -1. The literal-hint
// pre-scan means a clean command never reaches a regex engine.
func (f *Filter) match(cmd string) int {
	for i := range f.rules {
		r := &f.rules[i]
		if !hintsPresent(cmd, r.hints, r.fold) {
			continue
		}
		if r.re.MatchString(cmd) {
			return i
		}
	}
	return -1
}

// hintsPresent reports whether any hint occurs in cmd (or hints is empty, in
// which case the rule always runs). fold does an ASCII-case-insensitive scan.
func hintsPresent(cmd string, hints []string, fold bool) bool {
	if len(hints) == 0 {
		return true
	}
	for _, h := range hints {
		if fold {
			if containsFold(cmd, h) {
				return true
			}
		} else if strings.Contains(cmd, h) {
			return true
		}
	}
	return false
}

// containsFold reports whether sub occurs in s under ASCII case folding. sub
// must already be lowercase. It allocates nothing (unlike strings.ToLower).
func containsFold(s, sub string) bool {
	n, m := len(s), len(sub)
	if m == 0 {
		return true
	}
	if m > n {
		return false
	}
	for i := 0; i+m <= n; i++ {
		j := 0
		for ; j < m; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != sub[j] {
				break
			}
		}
		if j == m {
			return true
		}
	}
	return false
}

// SkipDir reports whether cwd falls under an ignored directory. The match is
// path-segment aware: an ignore dir of /home/x/secrets matches /home/x/secrets
// and /home/x/secrets/sub but NOT /home/x/secretsandmore. Trailing slashes and
// non-canonical paths are normalized. An empty cwd never matches.
func (f *Filter) SkipDir(cwd string) bool {
	if cwd == "" {
		return false
	}
	c := filepath.Clean(cwd)
	for _, d := range f.dirs {
		if c == d {
			return true
		}
		// c is strictly under d iff d is a prefix ending on a segment
		// boundary. "/" is its own boundary (Clean never leaves a trailing
		// slash except on root), so guard the index explicitly.
		if len(c) > len(d) && c[:len(d)] == d && (d == "/" || c[len(d)] == '/') {
			return true
		}
	}
	return false
}
