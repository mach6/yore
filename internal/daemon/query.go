package daemon

import (
	"sort"

	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
)

// runQuery answers one search over the RAM corpus: match, then scope, then
// dedupe, then sort newest-first, then window.
//
// The Filter is per-connection and must always see the WHOLE commands slice
// (its incremental optimization assumes an append-only corpus), so scope is
// applied to the matched indices afterwards rather than by feeding it a
// shrunken slice.
func (s *server) runQuery(f *match.Filter, q proto.QueryReq) proto.QueryResp {
	scope := q.Scope
	if scope == "" {
		scope = proto.ScopeLocal
	}

	// Snapshot the slice headers under RLock. The corpus is append-only and
	// its [0,len) prefix is never mutated in place, so the snapshot is a stable
	// view and we can scan it after releasing the lock. corpus and cmds are
	// appended together under the same lock, so their lengths agree.
	s.mu.RLock()
	corpus := s.corpus
	cmds := s.cmds
	s.mu.RUnlock()

	matched := f.Apply(q.Q, cmds)

	// Scope predicate over matched indices (Apply returned a private copy we
	// may filter in place).
	n := 0
	for _, idx := range matched {
		if scopeMatch(scope, corpus[idx], q) {
			matched[n] = idx
			n++
		}
	}
	matched = matched[:n]

	rows := make([]rec.Record, len(matched))
	for i, idx := range matched {
		rows[i] = corpus[idx]
	}

	// Newest-first: descending StartMs, ties broken by descending Seq. (Seq
	// order alone is not StartMs order — imported rows arrive out of time.)
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].StartMs != rows[b].StartMs {
			return rows[a].StartMs > rows[b].StartMs
		}
		return rows[a].Seq > rows[b].Seq
	})

	if q.Dedupe {
		seen := make(map[string]struct{}, len(rows))
		out := rows[:0]
		for _, r := range rows {
			if _, ok := seen[r.Cmd]; ok {
				continue
			}
			seen[r.Cmd] = struct{}{}
			out = append(out, r)
		}
		rows = out
	}

	total := len(rows) // matches after scope+dedupe, before windowing

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	lo := q.Offset
	if lo < 0 {
		lo = 0
	}
	if lo > total {
		lo = total
	}
	hi := lo + limit
	if hi > total {
		hi = total
	}
	windowed := rows[lo:hi]
	if windowed == nil {
		windowed = []rec.Record{}
	}

	return proto.QueryResp{
		Rows:   windowed,
		Total:  total,
		Scope:  scope,
		Remote: s.remote.info(),
	}
}

// scopeMatch reports whether r belongs to the requested scope. "all" and
// "host" behave like "local" for now (the sync layer is a later milestone);
// unknown scopes also fall through to local.
func scopeMatch(scope string, r rec.Record, q proto.QueryReq) bool {
	switch scope {
	case proto.ScopeSession:
		return r.Session == q.Session
	case proto.ScopeCwd:
		return r.Cwd == q.Cwd
	default:
		return true
	}
}
