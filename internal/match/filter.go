package match

import "strings"

// Filter incrementally filters an append-only corpus of commands for
// search-as-you-type. It exploits the fact that typing forward (the new
// query has the previous query as a string prefix) can only narrow the set
// of matching rows, so only the previously-matched rows need re-testing;
// rows appended since the last call are always tested. Any other change to
// the query string forces a full rescan.
//
// A Filter is not safe for concurrent use.
type Filter struct {
	prevQuery string
	query     Query
	prevLen   int   // len(cmds) at the last Apply
	matched   []int // authoritative current match set (internal, never handed out)
	scratch   []int // build buffer, swapped with matched each call
}

// NewFilter returns a ready-to-use Filter.
func NewFilter() *Filter { return &Filter{} }

// Apply returns the ascending indices of rows in cmds that match q.
//
// The returned slice is a fresh copy and never aliases the Filter's internal
// state, so the caller may retain or mutate it freely.
func (f *Filter) Apply(q string, cmds []string) []int {
	query := Parse(q)

	// Typing forward: the previous query is a prefix of the new one. The
	// corpus is append-only, so a shrunk length can only mean misuse; treat
	// it as a full rescan to stay correct.
	incremental := strings.HasPrefix(q, f.prevQuery) && len(cmds) >= f.prevLen

	dst := f.scratch[:0]
	if incremental {
		// Re-test only the rows that matched the (less restrictive) previous
		// query; a row that failed it cannot pass a narrower query.
		for _, idx := range f.matched {
			if query.Match(cmds[idx]) {
				dst = append(dst, idx)
			}
		}
		// Rows appended since the last call are new and must be tested.
		for i := f.prevLen; i < len(cmds); i++ {
			if query.Match(cmds[i]) {
				dst = append(dst, i)
			}
		}
	} else {
		for i := 0; i < len(cmds); i++ {
			if query.Match(cmds[i]) {
				dst = append(dst, i)
			}
		}
	}

	// Retain dst internally; reuse the old matched buffer as next scratch.
	f.scratch = f.matched
	f.matched = dst
	f.prevQuery = q
	f.query = query
	f.prevLen = len(cmds)

	out := make([]int, len(dst))
	copy(out, dst)
	return out
}

// Reset clears the Filter's memory so the next Apply performs a full scan.
func (f *Filter) Reset() {
	f.prevQuery = ""
	f.query = Query{}
	f.prevLen = 0
	f.matched = f.matched[:0]
}
