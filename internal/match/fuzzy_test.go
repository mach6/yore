package match

import (
	"testing"

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
