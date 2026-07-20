// Package match implements yore's search matcher, shared by the daemon's
// server-side filtering and by headless search.
//
// Semantics: a query is split on whitespace into terms; a row matches iff
// EVERY term appears as a substring of the command text. Matching is
// per-term smart-case: a term containing any uppercase letter matches
// case-sensitively, an all-lowercase term matches case-insensitively. The
// empty query (no terms) matches everything.
package match

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// term is one whitespace-delimited query token.
type term struct {
	text  string // original token text (used for case-sensitive matching)
	lower string // strings.ToLower(text); only meaningful when fold is true
	fold  bool   // case-insensitive (the token had no uppercase letter)
	ascii bool   // text is pure ASCII (enables the byte-scan fast path)
}

// Query is a parsed, ready-to-match search query.
type Query struct {
	terms []term
}

// Parse splits q into terms and classifies each for smart-case matching.
func Parse(q string) Query {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return Query{}
	}
	terms := make([]term, len(fields))
	for i, f := range fields {
		t := term{text: f, ascii: isASCII(f)}
		if hasUpper(f) {
			t.fold = false
		} else {
			t.fold = true
			t.lower = strings.ToLower(f)
		}
		terms[i] = t
	}
	return Query{terms: terms}
}

// Empty reports whether the query has no terms (and so matches everything).
func (q Query) Empty() bool { return len(q.terms) == 0 }

// Match reports whether every term of q occurs in s.
func (q Query) Match(s string) bool {
	for i := range q.terms {
		if !q.terms[i].match(s) {
			return false
		}
	}
	return true
}

// match reports whether the single term occurs in s.
func (t term) match(s string) bool {
	switch {
	case !t.fold:
		return strings.Contains(s, t.text)
	case t.ascii:
		return asciiFoldIndex(s, t.lower) >= 0
	default:
		// Rare non-ASCII case-insensitive path. Folding both sides is the
		// standard idiom and correct as a boolean (offsets are irrelevant
		// here; Ranges handles those on the rune-accurate path).
		return strings.Contains(strings.ToLower(s), t.lower)
	}
}

// Ranges returns the byte-offset [start,end) spans of every occurrence of
// every term in s, sorted and coalesced (overlapping or touching spans are
// merged). It is used to highlight matches. Case-insensitive terms are
// matched case-insensitively; offsets always index into the original s and
// never split a UTF-8 rune.
func (q Query) Ranges(s string) [][2]int {
	if len(q.terms) == 0 {
		return nil
	}
	var acc [][2]int
	for i := range q.terms {
		acc = q.terms[i].appendRanges(s, acc)
	}
	if len(acc) == 0 {
		return nil
	}
	sort.Slice(acc, func(i, j int) bool {
		if acc[i][0] != acc[j][0] {
			return acc[i][0] < acc[j][0]
		}
		return acc[i][1] < acc[j][1]
	})
	merged := acc[:1]
	for _, r := range acc[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1] { // overlapping or touching
			if r[1] > last[1] {
				last[1] = r[1]
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// appendRanges appends every occurrence of the term in s (including
// overlapping occurrences, which the caller coalesces) to acc.
func (t term) appendRanges(s string, acc [][2]int) [][2]int {
	if t.text == "" {
		return acc
	}
	switch {
	case !t.fold:
		// Exact byte substring; step by one to catch overlaps.
		for from := 0; ; {
			i := strings.Index(s[from:], t.text)
			if i < 0 {
				break
			}
			abs := from + i
			acc = append(acc, [2]int{abs, abs + len(t.text)})
			from = abs + 1
		}
	case t.ascii:
		// ASCII case-insensitive: byte scan. An ASCII needle can only align
		// on ASCII bytes of the haystack (UTF-8 lead/continuation bytes are
		// >= 0x80), so a match never spans into a multi-byte rune and the
		// offsets are exact for any UTF-8 haystack.
		n := len(t.lower)
		last := len(s) - n
		for i := 0; i <= last; i++ {
			j := 0
			for ; j < n; j++ {
				c := s[i+j]
				if 'A' <= c && c <= 'Z' {
					c += 'a' - 'A'
				}
				if c != t.lower[j] {
					break
				}
			}
			if j == n {
				acc = append(acc, [2]int{i, i + n})
			}
		}
	default:
		// Non-ASCII case-insensitive: rune-accurate scan so offsets index
		// into the original bytes (strings.ToLower can change byte lengths).
		nr := []rune(t.lower)
		for i := 0; i < len(s); {
			if m := matchFoldAt(s, i, nr); m >= 0 {
				acc = append(acc, [2]int{i, i + m})
			}
			_, size := utf8.DecodeRuneInString(s[i:])
			if size == 0 {
				size = 1
			}
			i += size
		}
	}
	return acc
}

// matchFoldAt reports the number of bytes of s consumed by a
// case-insensitive rune-by-rune match of the (already lowercased) needle
// runes nr starting at byte offset i, or -1 if there is no match.
func matchFoldAt(s string, i int, nr []rune) int {
	p := i
	for _, want := range nr {
		if p >= len(s) {
			return -1
		}
		r, size := utf8.DecodeRuneInString(s[p:])
		if unicode.ToLower(r) != want {
			return -1
		}
		p += size
	}
	return p - i
}

// asciiFoldIndex returns the first index at which the lowercase ASCII needle
// occurs in s under ASCII case folding, or -1. It allocates nothing.
func asciiFoldIndex(s, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	last := len(s) - n
	for i := 0; i <= last; i++ {
		j := 0
		for ; j < n; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
