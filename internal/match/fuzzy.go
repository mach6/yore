package match

import (
	"unicode"
	"unicode/utf8"
)

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

// FuzzyRanges returns the byte-offset [start,end) spans of the runes a fuzzy
// match of pattern consumes in s, with a contiguous run coalesced into one
// span, or nil when pattern does not match. It is Ranges' counterpart for the
// subsequence matcher: substring highlighting has nothing to mark when the
// match is scattered, which left the one mode where "why is this row here?"
// is not obvious as the one that answered it least.
//
// It walks s exactly as Fuzzy does and takes the leftmost rune for each
// pattern rune, so the marked characters are the ones the match was actually
// made of, and the highlight cannot disagree with the score.
func FuzzyRanges(pattern, s string) [][2]int {
	if pattern == "" {
		return nil
	}
	fold := !hasUpper(pattern)
	pr := []rune(pattern)

	var out [][2]int
	pi := 0
	for i := 0; i < len(s) && pi < len(pr); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			size = 1
		}
		if runeEq(r, pr[pi], fold) {
			if n := len(out); n > 0 && out[n-1][1] == i {
				out[n-1][1] = i + size // extends the run before it
			} else {
				out = append(out, [2]int{i, i + size})
			}
			pi++
		}
		i += size
	}
	if pi < len(pr) {
		return nil
	}
	return out
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
