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
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
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
var builtins = compile([]spec{
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
})

// spec is the pre-compilation form of a built-in rule.
type spec struct {
	name    string
	pattern string
	hints   []string
	fold    bool
}

// compile turns specs into rules; a bad built-in pattern is a programmer error
// and panics at init (built-ins are constant and covered by tests).
func compile(specs []spec) []rule {
	rules := make([]rule, len(specs))
	for i, s := range specs {
		rules[i] = rule{name: s.name, re: regexp.MustCompile(s.pattern), hints: s.hints, fold: s.fold}
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

// New compiles a Filter. userPatterns are user-supplied regexes from config:
// an invalid one is skipped and returned in errs (never fatal), so one typo
// can't disable recording. ignoreDirs are absolute path prefixes; a command
// whose cwd is inside one is never recorded. Either slice may be nil/empty.
func New(userPatterns, ignoreDirs []string) (f *Filter, errs []error) {
	rules := make([]rule, len(builtins), len(builtins)+len(userPatterns))
	copy(rules, builtins)
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

// Sensitive reports whether cmd matches any built-in or user rule. It is the
// hot path and does not allocate for a command that trips no rule's hints.
func (f *Filter) Sensitive(cmd string) bool { return f.match(cmd) >= 0 }

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
