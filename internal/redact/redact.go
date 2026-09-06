// Package redact is yore's recording security gate. It finds the secrets in
// captured text: shell commands, and the agent prompts yore records alongside
// them, which is why the rule table covers both assignment syntax and the prose
// an English sentence would use.
//
// A match REDACTS rather than rejects: Redact replaces the credential with a
// Mark naming the rule that caught it and keeps everything else. The command
// stays in your history, searchable and readable, minus the one span that must
// never be persisted or synced. Dropping the whole record was the older,
// blunter behaviour, and it was wrong in practice: the commands most worth
// remembering are often exactly the ones with a token somewhere in them, and an
// entry that silently vanished is indistinguishable from one you never ran.
//
// Two things still drop a record outright, because both are the user asking for
// it rather than the gate guessing: a cwd under an ignore_dirs prefix (SkipDir)
// and a match against one of the user's own ignore_patterns (Ignored).
//
// Two failure modes matter and pull in opposite directions:
//
//   - A false negative leaks a secret: the credential is recorded and later
//     synced to a server (encrypted, but still a persisted copy of a live one).
//   - A false positive corrupts history the user wanted, blanking a span that
//     was never a secret.
//
// So every built-in rule anchors on the *shape* of a secret value or on an
// unambiguous assignment/flag context, never on a bare keyword. "password"
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
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"yore/internal/config"
)

// rule is one built-in (or user) detector. re is the authority; hints is a
// set of cheap literal substrings of which at least one MUST be present for
// re to have any chance of matching. When hints is empty the rule always runs
// its regex (used for user patterns, whose shape we can't predict). fold makes
// the hint scan ASCII-case-insensitive (hints must then be lowercase).
//
// secret is the index of the pattern's `(?P<secret>…)` capture group, or 0 when
// it has none. It is what lets Redact mask the credential and keep the rest of
// the line: most rules match a context far wider than the secret itself
// (`mysql -u root -p<pw>` matches from the program name), so replacing the
// whole match would throw away the command along with the password.
type rule struct {
	name   string
	re     *regexp.Regexp
	hints  []string
	fold   bool
	secret int
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
//	google-api-key       a Google / Firebase API key (AIza…)
//	stripe-key           a Stripe secret or restricted key (sk_live_/rk_test_…)
//	llm-api-key          an OpenAI / Anthropic style key (sk-…, sk-ant-…)
//	huggingface-token    a Hugging Face access token (hf_…)
//	npm-token            an npm automation/access token (npm_…)
//	pypi-token           a PyPI API token (pypi-…)
//	sendgrid-key         a SendGrid API key (SG.<id>.<secret>)
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
//	secret-prose-assign  the same stated in prose ("the api key is <value>")
//	env-secret-export    a leading VAR=<value> whose name embeds a secret word
var builtins = compile(defaultSpecs)

// defaultSpecs is the ordered built-in rule table in its editable, pre-compiled
// form. It both compiles into builtins (above) and seeds ~/.config/yore/redact.yml
// via Seed/DefaultSpecs, so the two can never drift.
var defaultSpecs = []Spec{
	{"pem-block", `-----BEGIN [A-Z ]*PRIVATE KEY-----`, []string{"-----BEGIN"}, false},
	{"jwt", `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`, []string{"eyJ"}, false},

	{"aws-access-key", `(AKIA|ASIA)[0-9A-Z]{16}`, []string{"AKIA", "ASIA"}, false},
	{"aws-secret", `(?i)aws_secret_access_key["']?\s*[=: ]+\s*["']?(?P<secret>[A-Za-z0-9/+]{40})`, []string{"aws_secret"}, true},

	{"github-token", `gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`,
		[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, false},
	{"slack-token", `xox[baprs]-[A-Za-z0-9-]{10,}`, []string{"xox"}, false},

	{"google-api-key", `AIza[0-9A-Za-z_\-]{35}`, []string{"AIza"}, false},
	{"stripe-key", `[sr]k_(live|test)_[0-9A-Za-z]{16,}`,
		[]string{"sk_live", "sk_test", "rk_live", "rk_test"}, false},
	// OpenAI / Anthropic style. The length floor is what keeps a branch or
	// package named "sk-something-or-other" out of it.
	{"llm-api-key", `sk-[A-Za-z0-9_\-]{32,}`, []string{"sk-"}, false},
	{"huggingface-token", `hf_[A-Za-z0-9]{30,}`, []string{"hf_"}, false},
	{"npm-token", `npm_[A-Za-z0-9]{30,}`, []string{"npm_"}, false},
	{"pypi-token", `pypi-[A-Za-z0-9_\-]{32,}`, []string{"pypi-"}, false},
	{"sendgrid-key", `SG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}`, []string{"SG."}, false},

	{"url-userinfo", `[a-z][a-z0-9+.-]*://[^/@:\s]*:(?P<secret>[^/@\s]+)@`, []string{"://"}, false},

	{"openssl-passin", `(?i:openssl)\b.*\s-(?:passin|passout|pass)[ =]pass:(?P<secret>\S+)`, []string{"openssl"}, true},
	{"gpg-passphrase", `(?i:gpg)\b.*--passphrase[ =](?P<secret>\S+)`, []string{"gpg"}, true},

	{"sshpass-password", `(?i:sshpass)\b.*\s-p\s*(?P<secret>\S+)`, []string{"sshpass"}, true},
	{"mysql-password", `^\s*(?i:mysql|mariadb|mysqldump)\b.*\s(?:-p|--password=)(?P<secret>\S+)`, []string{"mysql", "mariadb", "mysqldump"}, true},
	{"mongo-password", `^\s*(?i:mongosh|mongo)\b.*(?:\s-p\s*|--password[ =])(?P<secret>\S+)`, []string{"mongo"}, true},
	{"smbclient-password", `^\s*(?i:smbclient)\b.*(?:-U\s*[^\s%]+%|--password[ =])(?P<secret>\S+)`, []string{"smbclient"}, true},
	{"curl-userpass", `(?i:curl)\b.*\s(?:-u\s*|--user[ =])[^\s:]+:(?P<secret>\S+)`, []string{"curl"}, true},
	{"wget-password", `(?i:wget)\b.*--(?:http-|ftp-|proxy-)?passw(?:or)?d[= ](?P<secret>\S+)`, []string{"wget"}, true},

	{"generic-token-assign", `(?i)(?:token|secret|passw(?:or)?d|api[_-]?key|auth)[=:](?P<secret>\S{6,})`,
		[]string{"token", "secret", "passw", "apikey", "api_key", "api-key", "auth"}, true},
	// The prose form of the same thing: "the api key is <value>", "password: <value>".
	// It exists because this gate now guards agent PROMPTS as well as commands, and a
	// prompt states a secret in a sentence where a command would assign it. The
	// separator must be followed by whitespace (the no-space form is already covered
	// above) and the value must look like a key (16+ characters of key alphabet) so
	// "password is required" and "see the token: ticket" pass.
	{"secret-prose-assign",
		`(?i)\b(?:api[ _-]?key|secret|token|password|passphrase|credential)s?\b\s*(?:is|are|=|:)\s+["']?(?P<secret>[A-Za-z0-9_\-+/=]{16,})`,
		[]string{"key", "secret", "token", "password", "passphrase", "credential"}, true},
	{"env-secret-export", `(?i)^\s*(?:export\s+)?[a-z0-9_]*(?:secret|token|password|passwd|apikey|api_key|credentials)[a-z0-9_]*=(?P<secret>\S+)`,
		[]string{"secret", "token", "password", "passwd", "apikey", "api_key", "credential"}, true},
}

// Spec is one redaction rule in its editable (pre-compilation) form: the YAML
// shape of ~/.config/yore/redact.yml and the seed form of the built-in table.
// Name identifies the rule (surfaced by Reason); Pattern is a Go regexp that is
// the authority for a match. Hints is an optional set of cheap literal
// substrings, at least one of which must be present for the regexp to run: a
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
		rules[i] = newRule(s.Name, regexp.MustCompile(s.Pattern), s.Hints, s.Fold)
	}
	return rules
}

// secretGroup is the capture-group name a rule uses to mark the credential
// inside a wider match. See rule.secret.
const secretGroup = "secret"

// newRule assembles a rule, resolving the index of its `secret` capture group.
func newRule(name string, re *regexp.Regexp, hints []string, fold bool) rule {
	idx := 0
	for i, n := range re.SubexpNames() {
		if n == secretGroup {
			idx = i
			break
		}
	}
	return rule{name: name, re: re, hints: hints, fold: fold, secret: idx}
}

// Filter is a compiled set of built-in and user rules plus a set of ignored
// directory prefixes. It is safe for concurrent use (all fields are read-only
// after New) and its Sensitive method allocates nothing on the clean path.
type Filter struct {
	rules []rule   // built-ins followed by user patterns
	dirs  []string // filepath.Clean'd ignore-dir prefixes
	// nuser is how many trailing entries of rules came from the user's
	// ignore_patterns. Those mean "do not record this at all"; the built-in
	// secret rules mean "record it, without the secret". See Redact / Ignored.
	nuser int
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
//     a non-fatal warning to errs; redaction stays fully on.
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

// MissingBuiltins returns the names of built-in rules that <dir>/redact.yml
// does not define. Seeding never overwrites an existing file, so a user who
// seeded before a rule shipped keeps a table without it; silently, since a
// missing detector looks exactly like a clean history. `yore doctor` reports
// this; nothing acts on it automatically, because a rule may be absent because
// the user deliberately deleted it. It returns nil when the file is absent or
// unusable: those already fall back to the full built-in table, so nothing is
// missing.
func MissingBuiltins(dir string) []string {
	b, err := os.ReadFile(config.RedactPath(dir))
	if err != nil {
		return nil
	}
	var file struct {
		Patterns []Spec `yaml:"patterns"`
	}
	if yaml.Unmarshal(b, &file) != nil || len(file.Patterns) == 0 {
		return nil
	}
	have := make(map[string]bool, len(file.Patterns))
	for _, s := range file.Patterns {
		have[s.Name] = true
	}
	var missing []string
	for _, s := range defaultSpecs {
		if !have[s.Name] {
			missing = append(missing, s.Name)
		}
	}
	return missing
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
		rules = append(rules, newRule(name, re, s.Hints, s.Fold))
	}
	return rules, errs
}

// assemble appends userPatterns to the base rules and attaches the ignore dirs,
// returning the finished Filter. It is the shared core of New and Load; errs
// accumulates any prior (base-load) warnings plus per-user-pattern ones.
func assemble(base []rule, userPatterns, ignoreDirs []string, errs []error) (*Filter, []error) {
	rules := base
	nuser := 0
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
		rules = append(rules, newRule("user:"+p, re, nil, false))
		nuser++
	}

	dirs := make([]string, 0, len(ignoreDirs))
	for _, d := range ignoreDirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(d))
	}

	return &Filter{rules: rules, dirs: dirs, nuser: nuser}, errs
}

// Seed writes the built-in rules to <dir>/redact.yml so the user can see and
// edit them, but ONLY when the file does not already exist: it never clobbers
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
const redactHeader = `# yore secret-redaction rules, seeded from the built-ins; edit freely. A command
# matching any rule below is never recorded (and so never synced). Each rule has a
# name, a Go regexp pattern, optional literal hints (a fast pre-filter: at least
# one must be present for the regexp to run; omit to always run it), and an
# optional fold flag (case-insensitive hint match; hints must then be lowercase).
#
# Fail-safe: if this file is removed, unreadable, unparseable, or left with no
# patterns, yore falls back to the built-in rules, so redaction never silently
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

// Mark is the placeholder Redact leaves where a credential was. It names the
// rule that fired, so the history says WHY a span is missing instead of just
// showing a hole, and it is deliberately not valid shell, so a redacted
// command recalled into the prompt fails loudly rather than running wrong.
func Mark(rule string) string { return markPrefix + rule + markSuffix }

// The delimiters of a Mark. They are deliberately outside the character set any
// shell or credential uses, so isMark below cannot be fooled by real text.
const (
	markPrefix = "⟪redacted:"
	markSuffix = "⟫"
)

// isMark reports whether s is already a redaction marker. Skipping those makes
// Redact idempotent: `PASSWORD=⟪redacted:…⟫` still matches the assignment rules
// that produced it, and without this a second pass (on import, or on a record
// that crossed the gate twice) would redact the marker and report a fresh hit
// for text that has no secret left in it.
func isMark(s string) bool {
	return strings.HasPrefix(s, markPrefix) && strings.HasSuffix(s, markSuffix)
}

// Redact returns text with every secret-rule match replaced by Mark, along with
// the names of the rules that fired (nil, and text unchanged, when clean). This
// is what the recording gate does with a secret now: the command is KEPT, minus
// the credential. Dropping the whole record was the safe default but a bad one
// in practice: the commands most worth remembering are often the ones with a
// token in them, and a silently missing entry is indistinguishable from one
// that was never run. Only the credential goes. Rules mark it with a `secret`
// capture group, so `mysql -u root -pHUNTER2 app` keeps everything but HUNTER2;
// a rule with no such group (a whole-value shape like an AWS key, or a user
// pattern) has its entire match replaced. User ignore_patterns are NOT applied
// here: those mean "don't record this", which is Ignored's job.
func (f *Filter) Redact(text string) (redacted string, rules []string) {
	if text == "" {
		return text, nil
	}
	spans := f.spans(text)
	if len(spans) == 0 {
		return text, nil
	}

	var fired []string
	seen := map[string]bool{}
	out := text
	// Splice right-to-left so each replacement leaves the offsets of the spans
	// still to come untouched.
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		out = out[:s.start] + Mark(s.rule) + out[s.end:]
	}
	for _, s := range spans {
		if !seen[s.rule] {
			seen[s.rule] = true
			fired = append(fired, s.rule)
		}
	}
	return out, fired
}

// span is one range of text to blank out, and the rule that claimed it.
type span struct {
	start, end int
	rule       string
}

// spans finds every range to redact, as non-overlapping ranges in ascending
// order. Every rule is matched against the ORIGINAL text, never against the
// partially redacted result. Rules genuinely overlap (`export
// DB_PASSWORD=hunter2` is caught by both generic-token-assign and
// env-secret-export) and redacting them one after another would let the second
// rule match the marker the first just inserted, nesting markers and
// misattributing the span. Overlapping ranges are merged instead, keeping the
// name of the first rule in table order that claimed the range (the table is
// ordered specific-before-general, so that is the more informative name).
func (f *Filter) spans(text string) []span {
	var found []span
	secrets := f.secretRules()
	for i := range secrets {
		r := &secrets[i]
		if !hintsPresent(text, r.hints, r.fold) {
			continue
		}
		for _, loc := range r.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := r.span(loc)
			if start < 0 || end <= start {
				continue // an optional group that did not participate
			}
			if isMark(text[start:end]) {
				continue // already redacted; see isMark
			}
			found = append(found, span{start: start, end: end, rule: r.name})
		}
	}
	if len(found) == 0 {
		return nil
	}
	// Ascending by start; on a tie the earlier rule (already in table order) wins,
	// so a stable sort preserves attribution.
	sort.SliceStable(found, func(i, j int) bool { return found[i].start < found[j].start })

	merged := found[:1]
	for _, s := range found[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// span picks the byte range to blank out for one match: the `secret` capture
// group when the rule defines one, else the whole match.
func (r *rule) span(loc []int) (start, end int) {
	if r.secret > 0 && 2*r.secret+1 < len(loc) {
		return loc[2*r.secret], loc[2*r.secret+1]
	}
	return loc[0], loc[1]
}

// secretRules is the built-in (and redact.yml) portion of the rule set: every
// rule except the user's ignore_patterns, which live at the tail.
func (f *Filter) secretRules() []rule { return f.rules[:len(f.rules)-f.nuser] }

// Ignored reports whether text matches one of the user's ignore_patterns. Those
// are an instruction to not record the command at all, unlike a secret rule,
// which only costs the command its credential.
func (f *Filter) Ignored(text string) bool {
	for i := len(f.rules) - f.nuser; i < len(f.rules); i++ {
		if f.rules[i].re.MatchString(text) {
			return true
		}
	}
	return false
}

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
