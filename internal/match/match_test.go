package match

import (
	"math/rand"
	"reflect"
	"testing"
)

func TestMatchSemantics(t *testing.T) {
	tests := []struct {
		name  string
		query string
		text  string
		want  bool
	}{
		{"empty query matches", "", "anything at all", true},
		{"whitespace-only query matches", "   \t ", "anything", true},
		{"single term present", "git", "git status", true},
		{"single term absent", "git", "docker ps", false},

		{"multi-term AND both present", "git commit", "git commit -m x", true},
		{"multi-term AND order irrelevant", "commit git", "git commit -m x", true},
		{"multi-term AND one missing", "git push", "git commit -m x", false},

		{"lowercase is case-insensitive (lower haystack)", "readme", "cat README.md", true},
		{"lowercase is case-insensitive (mixed haystack)", "readme", "cat ReAdMe", true},
		{"uppercase term is case-sensitive (match)", "README", "cat README.md", true},
		{"uppercase term is case-sensitive (no match)", "README", "cat readme.md", false},
		{"mixed-case term is case-sensitive", "GitHub", "clone from GitHub", true},
		{"mixed-case term is case-sensitive (no match)", "GitHub", "clone from github", false},

		{"per-term smart-case mix", "git README", "git show README", true},
		{"per-term smart-case mix fails on sensitive", "git README", "git show readme", false},

		{"substring not word", "omm", "git commit", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.query).Match(tc.text); got != tc.want {
				t.Fatalf("Parse(%q).Match(%q) = %v, want %v", tc.query, tc.text, got, tc.want)
			}
		})
	}
}

func TestEmpty(t *testing.T) {
	if !Parse("").Empty() {
		t.Fatal(`Parse("").Empty() = false, want true`)
	}
	if !Parse("   ").Empty() {
		t.Fatal(`Parse("   ").Empty() = false, want true`)
	}
	if Parse("x").Empty() {
		t.Fatal(`Parse("x").Empty() = true, want false`)
	}
}

func TestMatchUTF8(t *testing.T) {
	// Case-insensitive non-ASCII term against a UTF-8 haystack.
	tests := []struct {
		query, text string
		want        bool
	}{
		{"éclair", "commande café ÉCLAIR maison", true},
		{"éclair", "commande café eclair maison", false}, // missing accent
		{"straße", "die STRASSE", false},                 // ß != ss under simple fold
		{"café", "un CAFÉ noir", true},
		{"日本", "テスト 日本 語", true},
		{"ελλάδα", "ΕΛΛΆΔΑ και άλλα", true},
	}
	for _, tc := range tests {
		if got := Parse(tc.query).Match(tc.text); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.query, tc.text, got, tc.want)
		}
	}
}

func TestRanges(t *testing.T) {
	tests := []struct {
		name  string
		query string
		text  string
		want  [][2]int
	}{
		{"empty query", "", "git commit", nil},
		{"no occurrence", "zzz", "git commit", nil},
		{"single occurrence", "git", "git status", [][2]int{{0, 3}}},
		{
			"multiple occurrences of one term",
			"a", "banana",
			[][2]int{{1, 2}, {3, 4}, {5, 6}},
		},
		{
			"overlapping occurrences of one term coalesced",
			"aa", "aaaa",
			[][2]int{{0, 4}},
		},
		{
			"two terms, disjoint spans sorted",
			"git commit", "git commit",
			[][2]int{{0, 3}, {4, 10}},
		},
		{
			"two terms overlapping spans merged",
			"commit ommi", "git commit",
			// "commit" -> [4,10), "ommi" -> [5,9); merged -> [4,10)
			[][2]int{{4, 10}},
		},
		{
			"two terms touching spans coalesced",
			"ab cd", "abcd",
			[][2]int{{0, 4}},
		},
		{
			"case-insensitive offsets exact",
			"readme", "cat README here",
			[][2]int{{4, 10}},
		},
		{
			"case-sensitive term only matches exact case",
			"README", "readme README",
			[][2]int{{7, 13}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.query).Ranges(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Ranges(%q, %q) = %v, want %v", tc.query, tc.text, got, tc.want)
			}
		})
	}
}

func TestRangesUTF8Offsets(t *testing.T) {
	// "café ÉCLAIR" — the case-insensitive term "éclair" must report the
	// byte offsets of "ÉCLAIR" in the ORIGINAL string, not in a lowercased
	// copy (é and É are 2 bytes each in UTF-8).
	text := "café ÉCLAIR" // bytes: c a f é(2) sp É(2) C L A I R
	got := Parse("éclair").Ranges(text)
	// "café " = c(1)a(1)f(1)é(2)space(1) = 6 bytes; ÉCLAIR spans [6, 6+7)=[6,13)
	// É is 2 bytes, CLAIR is 5 bytes -> 7 bytes total.
	want := [][2]int{{6, 13}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Ranges = %v, want %v", got, want)
	}
	// Sanity: the reported span, sliced out of the original text, is ÉCLAIR.
	if s := text[got[0][0]:got[0][1]]; s != "ÉCLAIR" {
		t.Fatalf("sliced span = %q, want %q", s, "ÉCLAIR")
	}
}

// naiveScan is the oracle: a full linear scan using Query.Match.
func naiveScan(q Query, corpus []string) []int {
	var out []int
	for i, c := range corpus {
		if q.Match(c) {
			out = append(out, i)
		}
	}
	return out
}

// randQueryChar returns a single-byte character biased toward letters (both
// cases, to exercise smart-case) plus spaces and a few others.
func randQueryChar(rng *rand.Rand) byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP  ./-0123"
	return alphabet[rng.Intn(len(alphabet))]
}

func editQuery(rng *rand.Rand, q string) string {
	if q == "" {
		return string(randQueryChar(rng))
	}
	switch rng.Intn(3) {
	case 0: // append
		return q + string(randQueryChar(rng))
	case 1: // delete last byte (all chars are single-byte ASCII)
		return q[:len(q)-1]
	default: // replace last byte
		return q[:len(q)-1] + string(randQueryChar(rng))
	}
}

// TestFilterMatchesNaive drives 500 random query-edit sequences over a fixed
// 2k-row corpus and asserts Filter.Apply equals a naive full scan at every
// step (exercising the prefix-incremental path via appends and the full
// rescan via deletes/replaces).
func TestFilterMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	corpus := synthCommands(2000)
	f := NewFilter()
	q := ""
	for step := 0; step < 500; step++ {
		q = editQuery(rng, q)
		got := f.Apply(q, corpus)
		want := naiveScan(Parse(q), corpus)
		if want == nil {
			want = []int{}
		}
		if got == nil {
			got = []int{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d query=%q:\n got=%v\nwant=%v", step, q, got, want)
		}
	}
}

// TestFilterAppends exercises the "rows appended since the last call" branch:
// the corpus grows between Apply calls under a forward-typed query.
func TestFilterAppends(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	full := synthCommands(3000)
	f := NewFilter()

	q := ""
	n := 0
	for step := 0; step < 200; step++ {
		// Grow the corpus (append-only) and edit the query by appending.
		if n < len(full) {
			n += rng.Intn(40)
			if n > len(full) {
				n = len(full)
			}
		}
		// Bias toward appends so the query keeps its prefix and the corpus
		// keeps growing under the incremental path.
		if rng.Intn(4) == 0 && q != "" {
			q = q[:len(q)-1] // occasional delete forces a full rescan
		} else {
			q += string(randQueryChar(rng))
		}
		corpus := full[:n]
		got := f.Apply(q, corpus)
		want := naiveScan(Parse(q), corpus)
		if want == nil {
			want = []int{}
		}
		if got == nil {
			got = []int{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d n=%d query=%q:\n got=%v\nwant=%v", step, n, q, got, want)
		}
	}
}

func TestFilterReset(t *testing.T) {
	corpus := []string{"git status", "docker ps", "git commit"}
	f := NewFilter()
	f.Apply("git", corpus)
	f.Reset()
	// After Reset the next Apply must full-scan correctly.
	got := f.Apply("docker", corpus)
	want := []int{1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after Reset Apply = %v, want %v", got, want)
	}
}

// TestFilterReturnedSliceIndependent verifies the returned slice does not
// alias internal state a later Apply could disturb.
func TestFilterReturnedSliceIndependent(t *testing.T) {
	corpus := []string{"git a", "git b", "git c"}
	f := NewFilter()
	first := f.Apply("git", corpus)
	firstCopy := append([]int(nil), first...)
	// Mutate the returned slice and run another Apply; the first result must
	// be unchanged and internal bookkeeping must remain correct.
	for i := range first {
		first[i] = -1
	}
	second := f.Apply("git ", corpus)
	if !reflect.DeepEqual(second, firstCopy) {
		t.Fatalf("second Apply = %v, want %v (caller mutation leaked into Filter)", second, firstCopy)
	}
}
