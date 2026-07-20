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
	// Deep scopes (all hosts, or a specific remote host) also draw on the RAM
	// remote cache and, if it's cold or stale, kick a background sync so the
	// next query is richer — the request itself never blocks on the network.
	deep := scope == proto.ScopeAll || scope == proto.ScopeHost
	if deep && s.remote.enabled() {
		nudge(s.syncWake)
	}

	// Local corpus contributes unless the scope targets a specific remote host.
	local := s.store.Hostname()
	wantLocal := !(scope == proto.ScopeHost && q.Host != "" && q.Host != local)

	var rows []rec.Record
	if wantLocal {
		s.mu.RLock()
		corpus := s.corpus
		cmds := s.cmds
		s.mu.RUnlock()

		matched := f.Apply(q.Q, cmds)
		n := 0
		for _, idx := range matched {
			if scopeMatch(scope, corpus[idx], q) {
				matched[n] = idx
				n++
			}
		}
		matched = matched[:n]
		rows = make([]rec.Record, 0, len(matched))
		for _, idx := range matched {
			rows = append(rows, corpus[idx])
		}
	}

	// Merge in remote records for deep scopes.
	if deep && s.remote.enabled() {
		host := "" // ScopeAll = every remote host
		if scope == proto.ScopeHost {
			host = q.Host
		}
		rows = append(rows, s.remote.search(q.Q, host)...)
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

// scopeMatch reports whether a LOCAL-corpus record belongs to the requested
// scope. Remote records are filtered separately in remoteCache.search.
func scopeMatch(scope string, r rec.Record, q proto.QueryReq) bool {
	switch scope {
	case proto.ScopeSession:
		return r.Session == q.Session
	case proto.ScopeCwd:
		return r.Cwd == q.Cwd
	case proto.ScopeHost:
		// Local corpus is all one host; include it only when the filter names
		// this host (empty host = no restriction).
		return q.Host == "" || q.Host == r.Hostname
	default:
		return true
	}
}
