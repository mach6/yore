package match

import "testing"

func TestFuzzy(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"", "anything", true},
		{"gco", "git commit -m fix", true},
		{"dcu", "docker compose up", true},
		{"gco", "grep -r foo", false}, // no 'c' after 'g...o' subsequence
		{"xyz", "git status", false},
		{"GIT", "git status", false},   // smart-case: uppercase pattern is case-sensitive
		{"git", "GIT STATUS", true},    // lowercase pattern folds
		{"café", "run café now", true}, // unicode
		{"abc", "aXbXc", true},         // scattered subsequence
		{"cba", "aXbXc", false},        // wrong order
	}
	for _, c := range cases {
		got, _ := Fuzzy(c.pattern, c.s)
		if got != c.want {
			t.Errorf("Fuzzy(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestFuzzyScoreOrders(t *testing.T) {
	// A contiguous match should score lower (tighter) than a scattered one.
	_, tight := Fuzzy("abc", "abcdef")
	_, loose := Fuzzy("abc", "aXbXcX")
	if tight >= loose {
		t.Errorf("contiguous score %d should be < scattered score %d", tight, loose)
	}
}
