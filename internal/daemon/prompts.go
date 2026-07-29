package daemon

import (
	"sort"
	"sync"

	"yore/internal/rec"
)

// promptIndex holds every known user prompt by id, so a command row that
// carries only a PromptID can be shown with the text behind it. It is the
// read-side join that lets the prompt text be stored exactly once.
//
// It is fed by TypePrompt records from all three places they arrive: the local
// store scan at startup, the local ingest fold, and the remote pull fold.
//
// Safe for concurrent use.
type promptIndex struct {
	mu sync.RWMutex
	by map[string]rec.Record // prompt id -> its TypePrompt record
}

func newPromptIndex() *promptIndex {
	return &promptIndex{by: map[string]rec.Record{}}
}

// apply folds a prompt record into the index. Anything else is ignored: only a
// TypePrompt record carries prompt text, so a command row — which holds nothing
// but the id — has nothing to contribute and must not seed a phantom entry.
func (p *promptIndex) apply(r rec.Record) {
	if r.Type != rec.TypePrompt || r.ID == "" {
		return
	}
	p.mu.Lock()
	p.by[r.ID] = r
	p.mu.Unlock()
}

// hydrate fills in the Prompt text on every command row carrying a PromptID —
// which is the only thing a stored command row carries. Query results are
// copies, so this never touches the corpus; consumers (the agent explorer, the
// MCP prompt tools) just read r.Prompt and never see the join. An id with no
// prompt in the index resolves to no text, which is what a machine that keeps
// its prompts local looks like from here.
func (p *promptIndex) hydrate(rows []rec.Record) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range rows {
		if rows[i].PromptID != "" {
			rows[i].Prompt = p.by[rows[i].PromptID].Prompt
		}
	}
}

// since returns every prompt record started at or after cutoffMs, newest first,
// optionally restricted to one executor. These ride alongside a query's command
// rows so the agent explorer can show a prompt that triggered no commands at
// all — invisible for as long as a prompt existed only as a field on the
// commands it caused.
func (p *promptIndex) since(cutoffMs int64, executor string) []rec.Record {
	p.mu.RLock()
	out := make([]rec.Record, 0, len(p.by))
	for _, r := range p.by {
		if r.StartMs < cutoffMs {
			continue
		}
		if executor != "" && r.Executor != executor {
			continue
		}
		out = append(out, r)
	}
	p.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].StartMs != out[j].StartMs {
			return out[i].StartMs > out[j].StartMs
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// drop removes prompt records for the given ids (a tombstone applied to a
// prompt, or a remote-cache eviction).
func (p *promptIndex) drop(ids map[string]struct{}) {
	if len(ids) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range ids {
		delete(p.by, id)
	}
}

// len reports how many prompts are indexed.
func (p *promptIndex) len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.by)
}
