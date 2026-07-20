package match

import "unicode"

// Fuzzy reports whether every rune of pattern appears in s in order (a
// subsequence match, fzf-style), and returns a score where LOWER is a tighter
// match. Matching is smart-case: an all-lowercase pattern matches
// case-insensitively; any uppercase rune makes the whole pattern case-
// sensitive. An empty pattern matches everything with score 0.
//
// The score rewards contiguous runs and an early first match, so "gco" ranks
// "git commit" (g…co) sensibly against longer, more scattered hits. It is used
// only for relative ordering, not as an absolute quantity.
func Fuzzy(pattern, s string) (ok bool, score int) {
	if pattern == "" {
		return true, 0
	}
	fold := !hasUpper(pattern)

	pr := []rune(pattern)
	pi := 0
	prev := -2 // index (in matched-rune count) of the previous match, for gap cost
	firstAt := -1
	gaps := 0
	idx := 0
	for _, r := range s {
		if pi < len(pr) && runeEq(r, pr[pi], fold) {
			if firstAt < 0 {
				firstAt = idx
			}
			if idx != prev+1 {
				gaps++ // non-contiguous with the previous matched rune
			}
			prev = idx
			pi++
		}
		idx++
	}
	if pi < len(pr) {
		return false, 0
	}
	// Lower is better: penalize scattered matches and a late start.
	return true, gaps*4 + firstAt
}

func runeEq(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	if fold {
		return unicode.ToLower(a) == unicode.ToLower(b)
	}
	return false
}
