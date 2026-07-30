// Package risk is a deterministic, rule-based classifier for how dangerous a
// shell command is — no model, no network. It answers "is this safe to run?"
// for the MCP assess_risk tool, the browse views, and `yore agent report
// --fail-on`. Highest-severity match wins; a command that only reads or prints
// is never flagged.
//
// It is intentionally conservative and explainable: every verdict names a
// category and a short reason, so an agent (or a human) can see WHY.
package risk

import (
	"regexp"
	"strings"
)

// Level is a command's risk severity, ordered None < Low < Medium < High <
// Critical.
type Level int

const (
	None Level = iota
	Low
	Medium
	High
	Critical
)

// String is the lowercase label used in reports and MCP output.
func (l Level) String() string {
	switch l {
	case Low:
		return "low"
	case Medium:
		return "medium"
	case High:
		return "high"
	case Critical:
		return "critical"
	default:
		return "safe"
	}
}

// Glyph is the one-column severity marker, shared by MCP output and the TUI so
// the same level reads the same everywhere.
func (l Level) Glyph() string {
	switch l {
	case Critical:
		return "⛔"
	case High:
		return "⚠"
	case Medium:
		return "▲"
	case Low:
		return "•"
	default:
		return "✓"
	}
}

// ParseLevel maps a label back to a Level (for --fail-on). Unknown => None.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return Low
	case "medium":
		return Medium
	case "high":
		return High
	case "critical":
		return Critical
	default:
		return None
	}
}

// Assessment is the verdict for one command.
type Assessment struct {
	Level    Level
	Category string // e.g. "destructive", "package-install", "privilege"
	Reason   string // short human explanation
}

// rule is one classifier: a matcher plus the verdict it yields.
type rule struct {
	level    Level
	category string
	reason   string
	match    func(cmd string) bool
}

func re(pattern string) func(string) bool {
	r := regexp.MustCompile(pattern)
	return func(cmd string) bool { return r.MatchString(cmd) }
}

// rules are scanned in full; the highest-severity match wins (ties keep the
// first, which is why the table is ordered most-severe first within a level).
var rules = []rule{
	// --- Critical: irreversible / destructive ---
	{Critical, "destructive", "recursive force delete", matchRmRF},
	{Critical, "destructive", "force-push rewrites remote history", matchGitPushForce},
	{Critical, "destructive", "hard reset discards work", re(`(?i)\bgit\s+reset\s+--hard\b`)},
	{Critical, "destructive", "drops a database object", re(`(?i)\bdrop\s+(table|database|schema)\b`)},
	{Critical, "destructive", "overwrites a raw disk device", re(`(?i)>\s*/dev/(sd|nvme|disk)`)},
	{Critical, "destructive", "formats a filesystem", re(`(?i)\bmkfs\.`)},
	{Critical, "destructive", "raw write to a disk device", re(`(?i)\bdd\b.*\bof=/dev/`)},

	// --- High: installs, permissions, remote code execution ---
	{High, "package-install", "installs packages", matchPackageInstall},
	{High, "permission", "world-writable permissions", re(`(?i)\bchmod\s+([0-7]*[2367][0-7]*)\b`)},
	{High, "permission", "recursive ownership change", re(`(?i)\bchown\s+-R\b`)},
	{High, "script-exec", "pipes a download straight into a shell", re(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`)},
	{High, "script-exec", "executes a local script", re(`(?i)(^|\s)(\./|(sudo\s+)?(bash|sh|zsh)\s+)\S+\.sh\b`)},
	{High, "destructive", "force-cleans untracked files", re(`(?i)\bgit\s+clean\s+-[a-z]*f`)},

	// --- Medium: privilege, containers, process control ---
	{Medium, "privilege", "runs as root", re(`(?i)(^|\s)sudo\s`)},
	{Medium, "container", "removes or kills containers", re(`(?i)\bdocker\s+(rm|kill|stop|prune)\b`)},
	{Medium, "process", "kills processes", re(`(?i)\b(kill|killall|pkill)\b`)},
	{Medium, "destructive", "discards git state", re(`(?i)\bgit\s+(reset\b|checkout\s+--\s|stash\s+drop|branch\s+-[dD]\b)`)},

	// --- Low: network / remote reach ---
	{Low, "network", "reaches the network", re(`(?i)\b(curl|wget|ssh|scp|rsync)\b`)},
	{Low, "vcs", "pushes to a remote", re(`(?i)\bgit\s+push\b`)},
}

// Assess classifies a command against the built-in rules alone. Callers that
// honor the user's risk.toml hold a Ruleset from Load and call its Assess.
func Assess(cmd string) Assessment {
	return DefaultRuleset().Assess(cmd)
}

// isNonExecuting reports commands that cannot cause harm: comments, a bare
// echo/printf (no command substitution or chaining), and alias definitions.
func isNonExecuting(cmd string) bool {
	if cmd == "" || strings.HasPrefix(cmd, "#") {
		return true
	}
	if strings.HasPrefix(cmd, "alias ") {
		return true
	}
	if (strings.HasPrefix(cmd, "echo ") || strings.HasPrefix(cmd, "printf ") || cmd == "echo") &&
		!strings.ContainsAny(cmd, "|&;><`") && !strings.Contains(cmd, "$(") {
		return true
	}
	return false
}

var rmRe = regexp.MustCompile(`(?i)\brm\b`)

// matchRmRF flags rm invocations that are both recursive and forced, in either
// flag order and whether combined (-rf) or split (-r -f), including long forms.
func matchRmRF(cmd string) bool {
	if !rmRe.MatchString(cmd) {
		return false
	}
	recursive := containsFlagLetter(cmd, 'r') || containsFlagLetter(cmd, 'R') || strings.Contains(cmd, "--recursive")
	force := containsFlagLetter(cmd, 'f') || strings.Contains(cmd, "--force")
	return recursive && force
}

// containsFlagLetter reports whether any short-flag cluster (a token starting
// with a single '-') contains the given letter — so "-rf", "-fr", "-r" all match
// 'r', without matching a bare word or a long --flag.
func containsFlagLetter(cmd string, letter byte) bool {
	for _, tok := range strings.Fields(cmd) {
		if len(tok) >= 2 && tok[0] == '-' && tok[1] != '-' {
			if strings.IndexByte(tok[1:], letter) >= 0 {
				return true
			}
		}
	}
	return false
}

// matchGitPushForce flags a force push, but not the safer --force-with-lease.
// (Go's RE2 has no negative lookahead, so this is expressed in code.)
func matchGitPushForce(cmd string) bool {
	l := strings.ToLower(cmd)
	if !strings.Contains(l, "git push") {
		return false
	}
	if strings.Contains(l, "--force-with-lease") {
		return false
	}
	return strings.Contains(l, "--force") || regexpPushShortForce.MatchString(l)
}

var regexpPushShortForce = regexp.MustCompile(`\bgit\s+push\b[^|;&]*\s-\w*f`)

var pkgRe = regexp.MustCompile(`(?i)\b(` +
	`npm\s+(i|install|add)|yarn\s+add|pnpm\s+(add|install)|` +
	`pip3?\s+install|` +
	`cargo\s+(add|install)|` +
	`brew\s+install|gem\s+install|` +
	`go\s+(install|get)|` +
	`apt(-get)?\s+install|dnf\s+install|yum\s+install|pacman\s+-S\b|apk\s+add` +
	`)\b`)

func matchPackageInstall(cmd string) bool { return pkgRe.MatchString(cmd) }
