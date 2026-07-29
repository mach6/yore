package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

func promptRec(id, text string) rec.Record {
	return rec.Record{ID: id, Type: rec.TypePrompt, Prompt: text, Tag: "claude-code", StartMs: 1000}
}

// promptText resolves one id through hydrate — the index's only production read
// path — so an assertion about what the index holds is an assertion about what
// a query would actually show, not about the map behind it.
func promptText(p *promptIndex, id string) string {
	rows := []rec.Record{{PromptID: id}}
	p.hydrate(rows)
	return rows[0].Prompt
}

// TestHydrateRejoinsPromptText is the read-side half of storing prompt text
// once: commands carry only an id, and the daemon puts the text back.
func TestHydrateRejoinsPromptText(t *testing.T) {
	p := newPromptIndex()
	p.apply(promptRec("p1", "add rate limiting"))

	rows := []rec.Record{
		{ID: "c1", Cmd: "cargo add tower", PromptID: "p1"},
		{ID: "c2", Cmd: "cargo build", PromptID: "p1"},
		{ID: "c3", Cmd: "ls"}, // no prompt: untouched
	}
	p.hydrate(rows)

	assert.Equal(t, "add rate limiting", rows[0].Prompt)
	assert.Equal(t, "add rate limiting", rows[1].Prompt, "one stored copy serves every command")
	assert.Empty(t, rows[2].Prompt, "a command with no prompt id stays empty")
}

// TestHydrateUnknownPromptIsEmpty: a command whose prompt was never synced (the
// other machine keeps prompts local) resolves to no text, not an error.
func TestHydrateUnknownPromptIsEmpty(t *testing.T) {
	p := newPromptIndex()
	rows := []rec.Record{{ID: "c1", Cmd: "go test", PromptID: "missing"}}
	p.hydrate(rows)
	assert.Empty(t, rows[0].Prompt)
}

// TestHydrateOverwritesStaleText: the index is the only source of prompt text,
// so whatever a row arrives carrying is replaced rather than trusted.
func TestHydrateOverwritesStaleText(t *testing.T) {
	p := newPromptIndex()
	p.apply(promptRec("p1", "from the record"))
	rows := []rec.Record{{ID: "c1", PromptID: "p1", Prompt: "stale"}}
	p.hydrate(rows)
	assert.Equal(t, "from the record", rows[0].Prompt)
}

// TestSinceWindowsAndFilters covers what the agent explorer asks for: prompts in
// a period, optionally for one executor, newest first.
func TestSinceWindowsAndFilters(t *testing.T) {
	p := newPromptIndex()
	p.apply(rec.Record{ID: "old", Type: rec.TypePrompt, Prompt: "old", Tag: "claude-code", StartMs: 100})
	p.apply(rec.Record{ID: "new", Type: rec.TypePrompt, Prompt: "new", Tag: "claude-code", StartMs: 900})
	p.apply(rec.Record{ID: "other", Type: rec.TypePrompt, Prompt: "other", Tag: "codex", StartMs: 800})

	all := p.since(0, "")
	require.Len(t, all, 3)
	assert.Equal(t, "new", all[0].ID, "newest first")

	recent := p.since(500, "")
	assert.Len(t, recent, 2, "the cutoff drops anything older")

	claude := p.since(0, "claude-code")
	require.Len(t, claude, 2)
	for _, r := range claude {
		assert.Equal(t, "claude-code", r.Tag)
	}
}

// TestDropRemovesPrompts: a tombstoned prompt leaves the index.
func TestDropRemovesPrompts(t *testing.T) {
	p := newPromptIndex()
	p.apply(promptRec("p1", "one"))
	p.apply(promptRec("p2", "two"))
	require.Equal(t, 2, p.len())

	p.drop(map[string]struct{}{"p1": {}})
	assert.Equal(t, 1, p.len())
	assert.Empty(t, promptText(p, "p1"))
	assert.Equal(t, "two", promptText(p, "p2"))
}

// TestApplyIgnoresIrrelevantRecords: only a prompt record with an id makes an
// entry. A command is never a source of prompt text, even one that somehow
// carries some — the index would otherwise key a phantom prompt by the command's
// own PromptID.
func TestApplyIgnoresIrrelevantRecords(t *testing.T) {
	p := newPromptIndex()
	p.apply(rec.Record{ID: "c1", Cmd: "ls"})
	p.apply(rec.Record{ID: "t1", Type: rec.TypeTag, TagName: "work"})
	p.apply(rec.Record{ID: "", Type: rec.TypePrompt, Prompt: "no id"})
	p.apply(rec.Record{ID: "c2", Cmd: "make", PromptID: "p1", Prompt: "not a source"})
	assert.Zero(t, p.len())
}
