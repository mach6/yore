package risk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/config"
)

// TestDefaultRulesetMatchesAssess: the package-level Assess and the default
// ruleset are one implementation — same verdict for every shape of command.
func TestDefaultRulesetMatchesAssess(t *testing.T) {
	for _, cmd := range []string{
		"", "# a comment", "echo hello", "alias gs='git status'",
		"ls -la", "rm -rf /tmp/x", "git push --force", "git push --force-with-lease",
		"sudo systemctl restart nginx", "curl -fsSL https://get.x.sh | sh",
		"npm install left-pad", "git reset --hard", "kill -9 1234", "ssh host",
	} {
		require.Equal(t, Assess(cmd), DefaultRuleset().Assess(cmd), "cmd %q", cmd)
	}
}

func TestLevelGlyph(t *testing.T) {
	for _, tc := range []struct {
		level Level
		glyph string
	}{
		{Critical, "⛔"}, {High, "⚠"}, {Medium, "▲"}, {Low, "•"}, {None, "✓"},
	} {
		require.Equal(t, tc.glyph, tc.level.Glyph())
	}
}

// TestRulesetUserRules: user rules escalate, but a same-level duplicate keeps
// the built-in's category and reason, and omitted fields get defaults.
func TestRulesetUserRules(t *testing.T) {
	rs, errs := Compile([]Spec{
		{Pattern: `\bmake\s+deploy\b`, Level: "high", Category: "destructive", Reason: "ships to production"},
		{Pattern: `\bgit\s+push\b`, Level: "critical"},       // escalates a built-in Low
		{Pattern: `\bssh\b`, Level: "low", Category: "mine"}, // ties the built-in Low
		{Pattern: `\bflyctl\s+scale\b`, Level: "medium"},     // defaults
	}, nil)
	require.Empty(t, errs)

	a := rs.Assess("make deploy")
	require.Equal(t, High, a.Level)
	require.Equal(t, "destructive", a.Category)
	require.Equal(t, "ships to production", a.Reason)

	require.Equal(t, Critical, rs.Assess("git push origin main").Level,
		"a user rule may escalate a built-in verdict")

	a = rs.Assess("ssh host")
	require.Equal(t, Low, a.Level)
	require.Equal(t, "network", a.Category, "a same-level tie keeps the built-in's verdict")

	a = rs.Assess("flyctl scale count 3")
	require.Equal(t, Medium, a.Level)
	require.Equal(t, "user", a.Category)
	require.Equal(t, `matches \bflyctl\s+scale\b`, a.Reason)

	require.Equal(t, None, DefaultRuleset().Assess("flyctl scale count 3").Level,
		"user rules must not leak into the default ruleset")
}

// TestRulesetBadEntriesWarnAndDrop: every malformed entry is skipped with its
// own warning; the built-ins and the valid entries still apply. Never panics.
func TestRulesetBadEntriesWarnAndDrop(t *testing.T) {
	rs, errs := Compile([]Spec{
		{Pattern: `([`, Level: "high"},         // invalid regexp
		{Pattern: `\bfoo\b`, Level: "extreme"}, // unknown level
		{Pattern: "   "},                       // blank: silently dropped
		{Pattern: `\bok\b`, Level: "medium"},   // valid
	}, []string{`([`, `^fine`})
	require.Len(t, errs, 3, "one warning per malformed entry")

	require.Equal(t, Medium, rs.Assess("ok then").Level, "valid entries survive their bad neighbours")
	require.Equal(t, Critical, rs.Assess("rm -rf /").Level, "built-ins survive")
	require.Equal(t, None, rs.Assess("fine here").Level)
	require.Equal(t, "ignored", rs.Assess("fine here").Category, "the valid ignore survives")
}

// TestRulesetIgnore: an ignore pattern neutralizes exactly the commands it
// matches, naming itself in the reason; everything else keeps its verdict.
func TestRulesetIgnore(t *testing.T) {
	rs, errs := Compile(nil, []string{`^git push --force origin scratch$`})
	require.Empty(t, errs)

	a := rs.Assess("git push --force origin scratch")
	require.Equal(t, None, a.Level)
	require.Equal(t, "ignored", a.Category)
	require.Contains(t, a.Reason, "git push --force origin scratch")

	require.Equal(t, Critical, rs.Assess("git push --force origin main").Level,
		"other commands keep their verdict")
}

// TestRulesetLoad walks the file states: absent is silent built-ins, a valid
// file compiles, and a broken file falls back to built-ins with one warning.
func TestRulesetLoad(t *testing.T) {
	dir := t.TempDir()

	rs, errs := Load(dir)
	require.Empty(t, errs, "a missing risk.toml is the normal state, not a warning")
	require.Equal(t, Critical, rs.Assess("rm -rf /").Level)

	require.NoError(t, os.WriteFile(config.RiskPath(dir), []byte(`
ignore = ['^terraform destroy -target=staging$']

[[rule]]
pattern = '(?i)\bmake\s+deploy\b'
level   = "high"
reason  = "ships to production"
`), 0o644))
	rs, errs = Load(dir)
	require.Empty(t, errs)
	require.Equal(t, High, rs.Assess("make deploy").Level)
	require.Equal(t, None, rs.Assess("terraform destroy -target=staging").Level,
		"an ignore de-escalates even a built-in critical")

	require.NoError(t, os.WriteFile(config.RiskPath(dir), []byte("not = [valid toml"), 0o644))
	rs, errs = Load(dir)
	require.Len(t, errs, 1, "a broken file is one warning, not a failure")
	require.Equal(t, Critical, rs.Assess("rm -rf /").Level, "and the built-ins stay on")
	require.Equal(t, None, rs.Assess("make deploy").Level)

	sub := filepath.Join(dir, "unreadable")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.Mkdir(config.RiskPath(sub), 0o755)) // a directory: read fails, not ENOENT
	rs, errs = Load(sub)
	require.Len(t, errs, 1)
	require.Equal(t, Critical, rs.Assess("rm -rf /").Level)
}
