package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/rec"
)

func tagAdd(name, cmdID, session string) rec.Record {
	return rec.Record{Type: rec.TypeTag, TagName: name, TargetID: cmdID, Session: session}
}

func TestTagIndexApplyAndResolve(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("Refactor", "cmd1", ""))                                          // command tag, mixed case
	idx.apply(tagAdd("work", "", "sess1"))                                             // session tag
	idx.apply(rec.Record{Type: rec.TypeTag, TagName: "orphan", TagDesc: "just a def"}) // bare definition

	cmd := rec.Record{ID: "cmd1", Session: "sess1", Executor: "claude-code"}

	// Effective tags = command tag ∪ session tag, normalized. The executor is an
	// attribute of the record, not a label on it, and must not appear.
	eff := idx.effective(cmd)
	assert.ElementsMatch(t, []string{"refactor", "work"}, eff)

	// has() matches any effective tag, case-insensitively.
	assert.True(t, idx.has(cmd, "REFACTOR"))
	assert.True(t, idx.has(cmd, "work"))
	assert.False(t, idx.has(cmd, "nope"))

	// A different command in the same session inherits only the session tag.
	other := rec.Record{ID: "cmd2", Session: "sess1"}
	assert.ElementsMatch(t, []string{"work"}, idx.effective(other))
	assert.False(t, idx.has(other, "refactor"))
}

// TestTagIndexIgnoresExecutor pins the split: `--tag claude-code` is not a way
// to ask for agent commands, `--executor claude-code` is. Folding the two meant
// a row could not say which of its labels somebody had actually chosen.
func TestTagIndexIgnoresExecutor(t *testing.T) {
	idx := newTagIndex()
	agentRun := rec.Record{ID: "c1", Executor: "claude-code"}

	assert.Nil(t, idx.effective(agentRun), "an executor is not a tag")
	assert.False(t, idx.has(agentRun, "claude-code"))
	assert.Empty(t, idx.list([]rec.Record{agentRun}), "executors are never listed as tags")

	// Tagging the same row leaves the tag alone in the set.
	idx.apply(tagAdd("urgent", "c1", ""))
	assert.ElementsMatch(t, []string{"urgent"}, idx.effective(agentRun))
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

// TestTagIndexListCountsCommands is the number the listing is supposed to give:
// how much history carries the label. One session tag over three commands is
// three, not the one association that put it there.
func TestTagIndexListCountsCommands(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("work", "", "sess1"))
	idx.apply(tagAdd("urgent", "c2", ""))
	idx.setAutoTags(map[string]string{"/repo": "yore", "/never": "unused"})
	idx.apply(rec.Record{Type: rec.TypeTag, TagName: "docs", TagDesc: "documentation"})

	rows := []rec.Record{
		{ID: "c1", Session: "sess1"},
		{ID: "c2", Session: "sess1", Cwd: "/repo/internal"},
		{ID: "c3", Session: "sess1"},
		{ID: "c4", Cwd: "/repo"},
		{ID: "c5", Executor: "claude-code"}, // no tags at all
	}

	got := map[string]int{}
	desc := map[string]string{}
	list := idx.list(rows)
	for _, tc := range list {
		got[tc.Name] = tc.Count
		desc[tc.Name] = tc.Desc
	}

	assert.Equal(t, 3, got["work"], "the session tag counts every command in the session")
	assert.Equal(t, 1, got["urgent"])
	assert.Equal(t, 2, got["yore"], "auto_tags rules count the rows they match")
	assert.Equal(t, 0, got["unused"], "a rule that matches nothing still lists")
	assert.Equal(t, 0, got["docs"], "a bare definition still lists")
	assert.Equal(t, "documentation", desc["docs"])
	assert.NotContains(t, got, "claude-code", "an executor is not a tag")

	// Sorted by name.
	require.NotEmpty(t, list)
	assert.Equal(t, "docs", list[0].Name)
}

// TestTagIndexListEmptyCorpus covers the daemon that has tags but no matching
// history in scope: every name still lists, at zero.
func TestTagIndexListEmptyCorpus(t *testing.T) {
	idx := newTagIndex()
	idx.apply(tagAdd("work", "", "sess1"))

	list := idx.list(nil)
	require.Len(t, list, 1)
	assert.Equal(t, "work", list[0].Name)
	assert.Equal(t, 0, list[0].Count)
}

func TestTagIndexAutoTags(t *testing.T) {
	idx := newTagIndex()
	idx.setAutoTags(map[string]string{"/work/proj": "refactor", "/personal": "home"})

	// A command under /work/proj/src carries the auto-tag, no record needed.
	inProj := rec.Record{ID: "c1", Cwd: "/work/proj/src"}
	assert.ElementsMatch(t, []string{"refactor"}, idx.effective(inProj))
	assert.True(t, idx.has(inProj, "refactor"))

	// Boundary-aware: /work/project is NOT under /work/proj.
	sibling := rec.Record{ID: "c2", Cwd: "/work/project"}
	assert.Nil(t, idx.effective(sibling))
	assert.False(t, idx.has(sibling, "refactor"))

	// Auto-tags compose with explicit user tags, and with nothing else.
	idx.apply(tagAdd("urgent", "c1", ""))
	withExec := rec.Record{ID: "c1", Cwd: "/work/proj", Executor: "claude-code"}
	assert.ElementsMatch(t, []string{"refactor", "urgent"}, idx.effective(withExec))
}

func TestTagIndexIgnoresNonTag(t *testing.T) {
	idx := newTagIndex()
	cmd := rec.Record{ID: "x", Cmd: "ls", Executor: "claude-code"}
	idx.apply(cmd) // a command, not a tag record
	assert.Empty(t, idx.list([]rec.Record{cmd}), "non-tag records do not register tags")
}
