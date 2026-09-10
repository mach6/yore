package risk

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"github.com/mach6/yore/internal/config"
)

// Ruleset is a compiled set of risk rules: the built-ins plus whatever the
// user added in risk.toml. It is immutable after construction, so one instance
// can serve every caller concurrently, and the TUI and the MCP server agree
// because they load the same file.
type Ruleset struct {
	rules   []rule
	ignores []*regexp.Regexp
}

// Spec is one user rule as written in risk.toml.
type Spec struct {
	Pattern  string `toml:"pattern"`
	Level    string `toml:"level"`
	Category string `toml:"category,omitempty"`
	Reason   string `toml:"reason,omitempty"`
}

// specFile is risk.toml's shape: an ignore list plus any number of [[rule]]s.
type specFile struct {
	Ignore []string `toml:"ignore"`
	Rules  []Spec   `toml:"rule"`
}

var defaultRuleset = sync.OnceValue(func() *Ruleset {
	return &Ruleset{rules: rules}
})

// DefaultRuleset is the compiled-in rule table with no user additions.
func DefaultRuleset() *Ruleset { return defaultRuleset() }

// Compile builds a Ruleset from the built-ins plus user specs and ignore
// patterns. It is fail-safe: a blank pattern is dropped silently, an invalid
// regexp or unknown level drops that one entry with a warning in errs, and the
// built-ins always remain; a typo can never switch risk assessment off. User
// rules sit after the built-ins, so escalating a command to a higher level
// always wins, while a same-level duplicate keeps the built-in's category and
// reason.
func Compile(specs []Spec, ignore []string) (*Ruleset, []error) {
	rs := &Ruleset{rules: make([]rule, len(rules), len(rules)+len(specs))}
	copy(rs.rules, rules)
	var errs []error
	for _, s := range specs {
		if strings.TrimSpace(s.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("risk: skipping invalid rule %q: %w", s.Pattern, err))
			continue
		}
		level := ParseLevel(s.Level)
		if level == None {
			errs = append(errs, fmt.Errorf("risk: skipping rule %q: unknown level %q", s.Pattern, s.Level))
			continue
		}
		category := s.Category
		if category == "" {
			category = "user"
		}
		reason := s.Reason
		if reason == "" {
			reason = "matches " + s.Pattern
		}
		// A user rule is a regexp over the command as written: risk.toml
		// predates the parser and must keep meaning exactly what it says.
		rs.rules = append(rs.rules, rule{level, category, reason,
			func(c *cmdline) bool { return re.MatchString(c.raw) }})
	}
	for _, pat := range ignore {
		if strings.TrimSpace(pat) == "" {
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			errs = append(errs, fmt.Errorf("risk: skipping invalid ignore pattern %q: %w", pat, err))
			continue
		}
		rs.ignores = append(rs.ignores, re)
	}
	return rs, errs
}

// Load compiles a Ruleset from <dir>/risk.toml. The file is optional: absent
// means the built-ins alone, with no warning. An unreadable or unparseable
// file falls back to the built-ins with one warning; bad entries inside an
// otherwise-valid file are skipped individually, exactly as Compile does.
func Load(dir string) (*Ruleset, []error) {
	path := config.RiskPath(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultRuleset(), nil
		}
		return DefaultRuleset(), []error{fmt.Errorf("risk: %s unreadable, using built-in rules: %w", path, err)}
	}
	var f specFile
	if err := toml.Unmarshal(b, &f); err != nil {
		return DefaultRuleset(), []error{fmt.Errorf("risk: %s unparseable, using built-in rules: %w", path, err)}
	}
	return Compile(f.Rules, f.Ignore)
}

// Assess classifies a command against this ruleset. Semantics match the
// package-level Assess: non-executing commands are None, an ignore match is
// None with the pattern named, otherwise the highest-severity rule wins.
func (rs *Ruleset) Assess(cmd string) Assessment {
	c := strings.TrimSpace(cmd)
	if isNonExecuting(c) {
		return Assessment{Level: None, Category: "safe", Reason: "no side effects"}
	}
	for _, re := range rs.ignores {
		if re.MatchString(c) {
			return Assessment{Level: None, Category: "ignored", Reason: "matches ignore pattern " + re.String()}
		}
	}
	return rs.assess(parse(c, 0))
}

// maxPayloadDepth bounds how far assessment follows code into code; `sh -c` of
// a `python -c` of a string. Three is past anything a person writes by hand and
// keeps a hostile line from costing unbounded work.
const maxPayloadDepth = 3

// assess scans every rule and keeps the most severe match, then does the same
// for whatever this command hands to an interpreter, so `sh -c 'rm -rf /'` is
// rated as the deletion it is rather than as a shell invocation.
func (rs *Ruleset) assess(c *cmdline) Assessment {
	best := Assessment{Level: None, Category: "safe", Reason: "no risky pattern matched"}
	for _, r := range rs.rules {
		if r.level > best.Level && r.match(c) {
			best = Assessment{Level: r.level, Category: r.category, Reason: r.reason}
			if best.Level == Critical {
				return best // nothing outranks critical
			}
		}
	}
	if c.depth >= maxPayloadDepth {
		return best
	}
	for _, p := range c.payloads() {
		if a := rs.assess(parse(p, c.depth+1)); a.Level > best.Level {
			best = a
			if best.Level == Critical {
				return best
			}
		}
	}
	return best
}
