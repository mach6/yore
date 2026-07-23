package redact

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"yore/internal/config"
)

// writeRedact writes raw bytes as dir's redact.yml.
func writeRedact(t *testing.T, dir string, body []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(config.RedactPath(dir), body, 0o600))
}

// specsExcept returns the built-in specs with the named rule removed.
func specsExcept(name string) []Spec {
	out := make([]Spec, 0, len(DefaultSpecs()))
	for _, s := range DefaultSpecs() {
		if s.Name == name {
			continue
		}
		out = append(out, s)
	}
	return out
}

// TestSeedWritesDefault: Seed writes a 0600 redact.yml that round-trips back to
// the built-in rule set, and a second Seed never clobbers an edited file.
func TestSeedWritesDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Seed(dir))

	path := config.RedactPath(dir)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "redact.yml must be 0600")

	// The file re-marshals (parses back) to exactly the built-in specs.
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var file struct {
		Patterns []Spec `yaml:"patterns"`
	}
	require.NoError(t, yaml.Unmarshal(b, &file))
	require.Equal(t, DefaultSpecs(), file.Patterns, "seeded file must equal the built-ins")

	// Editing then re-seeding must leave the edit untouched (idempotent).
	edited := []byte("patterns:\n  - name: mine\n    pattern: keepme\n")
	writeRedact(t, dir, edited)
	require.NoError(t, Seed(dir), "second Seed must be a no-op")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, edited, got, "Seed must never overwrite an existing file")
}

// TestLoadFromFile: a file that drops the jwt rule loses JWT detection while its
// remaining rules (pem-block) keep working — the file, not the built-ins, wins.
func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	body, err := marshalSpecs(specsExcept("jwt"))
	require.NoError(t, err)
	writeRedact(t, dir, body)

	f, errs := Load(dir, nil, nil)
	require.Empty(t, errs, "a valid file must load without warnings")

	const jwt = `echo eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEF`
	require.False(t, f.Sensitive(jwt), "removed jwt rule must no longer flag a JWT")
	require.True(t, f.Sensitive(`echo "-----BEGIN OPENSSH PRIVATE KEY-----"`), "kept pem-block rule must still flag")
}

// TestLoadMissingFallsBackToBuiltins: with no redact.yml, Load uses the built-in
// rules (a known secret is still flagged) and warns that it fell back.
func TestLoadMissingFallsBackToBuiltins(t *testing.T) {
	dir := t.TempDir()
	f, errs := Load(dir, nil, nil)
	require.True(t, f.Sensitive(`export DB_PASSWORD=hunter2`), "built-in rules must apply when the file is absent")
	require.False(t, f.Sensitive(`git status`), "clean command must stay clean")
	require.NotEmpty(t, errs, "a missing file must warn (non-fatal)")
	require.Contains(t, errs[0].Error(), "not found")
}

// TestLoadBrokenFileFailsSafe: a malformed or empty rules file must fall back to
// the built-ins (redaction stays ON) and warn — never a filter that redacts
// nothing.
func TestLoadBrokenFileFailsSafe(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"malformed yaml", []byte("patterns:\n  - name: x\n   pattern: [oops\n")},
		{"empty patterns list", []byte("patterns: []\n")},
		{"no patterns key", []byte("# just a comment, nothing else\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRedact(t, dir, tc.body)

			f, errs := Load(dir, nil, nil)
			require.NotEmpty(t, errs, "a broken/empty file must warn")
			require.True(t, f.Sensitive(`export DB_PASSWORD=hunter2`), "must fall back to built-ins, not redact nothing")
			require.GreaterOrEqual(t, f.NumRules(), len(builtins), "fallback must load the full built-in table")
			require.False(t, f.Sensitive(`git status`), "clean command must stay clean")
		})
	}
}

// TestLoadSkipsBadPattern: one invalid regexp in an otherwise-valid file is
// skipped with a warning while the remaining rules still apply (the file's
// rules, not the built-ins, are in force).
func TestLoadSkipsBadPattern(t *testing.T) {
	dir := t.TempDir()
	body, err := marshalSpecs([]Spec{
		{Name: "custom", Pattern: `mysecret-[0-9]+`},
		{Name: "broken", Pattern: `(unclosed`},
		{Name: "pem-block", Pattern: `-----BEGIN [A-Z ]*PRIVATE KEY-----`, Hints: []string{"-----BEGIN"}},
	})
	require.NoError(t, err)
	writeRedact(t, dir, body)

	f, errs := Load(dir, nil, nil)
	require.Len(t, errs, 1, "exactly the one invalid pattern is reported")
	require.Contains(t, errs[0].Error(), "invalid pattern")

	require.True(t, f.Sensitive(`run mysecret-12345`), "valid custom rule must fire")
	require.True(t, f.Sensitive(`echo "-----BEGIN RSA PRIVATE KEY-----"`), "valid pem rule must fire")
	require.False(t, f.Sensitive(`export DB_PASSWORD=hunter2`), "built-ins are NOT used when the file has valid rules")
}

// TestLoadWithUserPatterns: Load layers config user patterns and ignore dirs on
// top of the file's rules, exactly as New does for the built-ins.
func TestLoadWithUserPatterns(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Seed(dir))

	f, errs := Load(dir, []string{`INTERNAL-[0-9]{6}`}, []string{"/home/x/secrets"})
	require.Empty(t, errs)
	require.True(t, f.Sensitive(`deploy --token INTERNAL-004217`), "user pattern must apply")
	require.True(t, f.Sensitive(`export DB_PASSWORD=hunter2`), "seeded built-in must still apply")
	require.True(t, f.SkipDir("/home/x/secrets/sub"), "ignore dir must apply")
}
