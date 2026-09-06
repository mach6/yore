// Package risk is a deterministic, rule-based classifier for how dangerous a
// shell command is: no model, no network. It answers "is this safe to run?"
// for the MCP assess_risk tool, the browse views, and `yore agent report
// --fail-on`. Highest-severity match wins; a command that only reads or prints
// is never flagged. It is intentionally conservative and explainable: every
// verdict names a category and a short reason, so an agent (or a human) can
// see WHY. Rules are written against a parsed command (see parse.go), not
// against the raw string, so they can ask what a line *runs* rather than what
// it contains.
package risk

import (
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

// rule is one classifier: a matcher plus the verdict it yields. The matcher
// sees the parsed command, so it can distinguish a command from a mention of
// one; a user rule from risk.toml is a matcher that reads the raw text.
type rule struct {
	level    Level
	category string
	reason   string
	match    func(c *cmdline) bool
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
