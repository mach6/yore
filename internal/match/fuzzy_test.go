package match

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestFuzzy(t *testing.T) {
	tests := []struct {
		name       string
		pattern, s string
		want       bool
	}{
		{"empty pattern matches anything", "", "anything", true},
		{"gco subsequence", "gco", "git commit -m fix", true},
		{"dcu subsequence", "dcu", "docker compose up", true},
		{"no c after g...o subsequence", "gco", "grep -r foo", false},
		{"no subsequence at all", "xyz", "git status", false},
		{"smart-case: uppercase pattern is case-sensitive", "GIT", "git status", false},
		{"lowercase pattern folds", "git", "GIT STATUS", true},
		{"unicode", "café", "run café now", true},
		{"scattered subsequence", "abc", "aXbXc", true},
		{"wrong order", "cba", "aXbXc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Fuzzy(tc.pattern, tc.s)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFuzzyScoreOrders(t *testing.T) {
	// A contiguous match should score lower (tighter) than a scattered one.
	_, tight := Fuzzy("abc", "abcdef")
	_, loose := Fuzzy("abc", "aXbXcX")
	require.Less(t, tight, loose, "contiguous score should be less than scattered score")
}

func TestFuzzyRanges(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		s       string
		want    [][2]int
	}{
		{"scattered runes each get a span", "gpo", "git push origin main",
			[][2]int{{0, 1}, {4, 5}, {9, 10}}},
		{"a contiguous run is one span", "git", "git push", [][2]int{{0, 3}}},
		{"runs coalesce with the span before them", "gitp", "git push",
			[][2]int{{0, 3}, {4, 5}}},
		{"leftmost rune wins, as Fuzzy scores it", "go", "go run ./cmd",
			[][2]int{{0, 2}}},
		{"no match marks nothing", "zz", "git push", nil},
		{"empty pattern marks nothing", "", "git push", nil},
		{"lowercase pattern folds", "gp", "Git Push", [][2]int{{0, 1}, {4, 5}}},
		{"smart-case: an uppercase rune makes it case-sensitive", "GP", "git push", nil},
		// Offsets are bytes into the original string, so a multi-byte rune
		// before or inside the match must not shift them.
		{"multi-byte runes keep byte offsets exact", "né", "café née", [][2]int{{6, 9}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FuzzyRanges(tc.pattern, tc.s)
			require.Equal(t, tc.want, got)
			for _, r := range got {
				require.True(t, utf8.ValidString(tc.s[r[0]:r[1]]),
					"span %v splits a rune of %q", r, tc.s)
			}
		})
	}
}

// TestFuzzyRangesAgreeWithFuzzy is the pairing the highlight depends on: every
// row Fuzzy accepts must have runes to mark, and every row it rejects must have
// none. A highlighter that disagreed with the matcher would mark the wrong
// characters on rows the user is looking at precisely to see why they matched.
func TestFuzzyRangesAgreeWithFuzzy(t *testing.T) {
	cmds := []string{
		"git push origin main", "kubectl get pods -n staging", "go run ./cmd/api",
		"grep -o 'HTTP/[0-9.]*' access.log", "ls -la", "café née", "docker ps",
	}
	for _, pattern := range []string{"gpo", "g", "zz", "café", "GPO", ""} {
		for _, c := range cmds {
			ok, _ := Fuzzy(pattern, c)
			got := FuzzyRanges(pattern, c)
			if !ok || pattern == "" {
				require.Nilf(t, got, "Fuzzy(%q, %q) did not match; ranges must be nil", pattern, c)
				continue
			}
			require.NotEmptyf(t, got, "Fuzzy(%q, %q) matched; ranges must say where", pattern, c)
			// The marked runes, concatenated, are the pattern itself.
			var marked string
			for _, r := range got {
				marked += c[r[0]:r[1]]
			}
			require.Equalf(t, strings.ToLower(pattern), strings.ToLower(marked),
				"marked runes of %q should spell the pattern %q", c, pattern)
		}
	}
}
