package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

func tagAdd(name, cmdID, session string) rec.Record {
	return rec.Record{Type: rec.TypeTag, TagName: name, TargetID: cmdID, Session: session}
}

func TestTagIndexApplyAndResolve(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("Refactor", "cmd1", ""))                                          // command tag, mixed case
	idx.apply(tagAdd("work", "", "sess1"))                                             // session tag
	idx.apply(rec.Record{Type: rec.TypeTag, TagName: "orphan", TagDesc: "just a def"}) // bare definition

	cmd := rec.Record{ID: "cmd1", Session: "sess1", Tag: "claude-code"}

	// Effective tags = executor auto-tag ∪ command tag ∪ session tag, normalized.
	eff := idx.effective(cmd)
	assert.ElementsMatch(t, []string{"claude-code", "refactor", "work"}, eff)

	// has() matches any effective tag, case-insensitively.
	assert.True(t, idx.has(cmd, "REFACTOR"))
	assert.True(t, idx.has(cmd, "work"))
	assert.True(t, idx.has(cmd, "claude-code"))
	assert.False(t, idx.has(cmd, "nope"))

	// A different command in the same session inherits only the session tag.
	other := rec.Record{ID: "cmd2", Session: "sess1"}
	assert.ElementsMatch(t, []string{"work"}, idx.effective(other))
	assert.False(t, idx.has(other, "refactor"))
}

func TestTagIndexRemove(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("work", "cmd1", ""))
	cmd := rec.Record{ID: "cmd1"}
	require.True(t, idx.has(cmd, "work"))

	idx.apply(rec.Record{Type: rec.TypeTag, TagName: "work", TargetID: "cmd1", TagOp: rec.TagOpRemove})
	assert.False(t, idx.has(cmd, "work"), "removed tag no longer applies")
	assert.Nil(t, idx.effective(cmd))
}

func TestTagIndexList(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("work", "cmd1", ""))
	idx.apply(tagAdd("work", "cmd2", ""))
	idx.apply(tagAdd("play", "", "sess1"))
	idx.apply(rec.Record{Type: rec.TypeTag, TagName: "docs", TagDesc: "documentation"})

	list := idx.list()
	got := map[string]int{}
	desc := map[string]string{}
	for _, tc := range list {
		got[tc.Name] = tc.Count
		desc[tc.Name] = tc.Desc
	}
	assert.Equal(t, 2, got["work"], "work is on two commands")
	assert.Equal(t, 1, got["play"])
	assert.Equal(t, 0, got["docs"], "a bare definition has no associations")
	assert.Equal(t, "documentation", desc["docs"])
	// Sorted by name.
	require.NotEmpty(t, list)
	assert.Equal(t, "docs", list[0].Name)
}

func TestTagIndexIgnoresNonTag(t *testing.T) {
	idx := newTagIndex()
	idx.apply(rec.Record{ID: "x", Cmd: "ls", Tag: "claude-code"}) // a command, not a tag record
	assert.Empty(t, idx.list(), "non-tag records do not register tags")
}
