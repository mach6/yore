package browse

import (
	"strconv"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

// mkTaggedRows is mkRows with a tag list per row, for the untag paths: every
// one of them turns on what the row already carries.
func mkTaggedRows(tags ...[]string) []rec.Record {
	cmds := make([]string, len(tags))
	for i := range tags {
		cmds[i] = "cmd-" + strconv.Itoa(i)
	}
	rows := mkRows(cmds...)
	for i := range rows {
		rows[i].Tags = tags[i]
	}
	return rows
}

// submitUntag opens ctrl+x, types name and submits, handing back whatever
// command that produced. It deliberately does not run it: the single-row path
// hands back the flash's own expiry tick, and draining that would sleep out the
// flash the test is about to read.
func submitUntag(t *testing.T, m Model, name string) (Model, tea.Cmd) {
	t.Helper()
	m, _ = step(t, m, press("ctrl+x"))
	for _, r := range name {
		m, _ = step(t, m, press(string(r)))
	}
	return step(t, m, press("enter"))
}

// runBulkUntag submits and then drains the batch command the way bubbletea
// would: the bulk path's round trips run off the event loop, so nothing has
// happened until it does.
func runBulkUntag(t *testing.T, m Model, name string) Model {
	t.Helper()
	m, cmd := submitUntag(t, m, name)
	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	return m
}

// TestUntagRow is the single-row ctrl+x path: it submits a tag record carrying
// the remove op (not an add) and the row loses the tag at once.
func TestUntagRow(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkTaggedRows([]string{"wip"}, []string{"wip"})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, _ = submitUntag(t, m, "wip")

	require.Len(t, f.submitted, 1)
	require.Equal(t, rec.TypeTag, f.submitted[0].Type)
	require.Equal(t, "wip", f.submitted[0].TagName)
	require.Equal(t, rows[0].ID, f.submitted[0].TargetID)
	require.Equal(t, rec.TagOpRemove, f.submitted[0].TagOp,
		"ctrl+x must submit the remove op: an add here would silently re-tag the row")

	require.Empty(t, m.rows[0].Tags, "the row should lose the tag at once")
	require.Equal(t, []string{"wip"}, m.rows[1].Tags, "the row under no cursor must keep its tag")
	require.Contains(t, strip(m.View()), "untagged: wip")
}

// TestUntagRowWithoutTheTag: removing a tag the row does not carry is a record
// the daemon folds into nothing, so it is not sent, and the flash says what
// actually happened rather than reporting a removal that never occurred.
func TestUntagRowWithoutTheTag(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"review"}))})

	m, _ = submitUntag(t, m, "wip")

	require.Empty(t, f.submitted, "a tag the row does not have is not a round trip worth making")
	require.Equal(t, []string{"review"}, m.rows[0].Tags, "the row's real tags must survive")
	require.Contains(t, strip(m.View()), "not tagged: wip")
}

// TestUntagPromptShowsScopeAndResets mirrors the tag box's own contract: the
// prompt names the op and the count, and closing it puts both back; a
// cancelled ctrl+x must not leave the next ctrl+t reading "untag".
func TestUntagPromptShowsScopeAndResets(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b", "c"))})

	m, _ = step(t, m, press("ctrl+x"))
	require.Equal(t, "untag: ", m.tagInput.Prompt)
	require.True(t, m.tagRemove)
	require.Contains(t, footerText(m), "remove the tag", "the footer must say which op enter commits")

	m, _ = step(t, m, press("esc"))
	require.Equal(t, "tag: ", m.tagInput.Prompt, "closing must drop the stale verb")
	require.False(t, m.tagRemove)

	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press(" "))
	m, _ = step(t, m, press("ctrl+x"))
	require.Equal(t, "untag 2 records: ", m.tagInput.Prompt,
		"the prompt should name how many records it will untag")

	// And the add box is unaffected by any of it.
	m, _ = step(t, m, press("esc"))
	m, _ = step(t, m, press("ctrl+t"))
	require.Equal(t, "tag 2 records: ", m.tagInput.Prompt)
	require.False(t, m.tagRemove)
}

// TestBulkUntagOnlyTheRowsThatCarryIt: ctrl+x over a selection removes the tag
// from the checked rows that actually have it, and asks nothing of the ones
// that do not; the count in the flash has to be the truth, which means knowing
// the scope before submitting rather than after.
func TestBulkUntagOnlyTheRowsThatCarryIt(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkTaggedRows([]string{"wip"}, []string{"review"}, []string{"wip", "review"})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, _ = step(t, m, press("ctrl+a"))
	require.Equal(t, 3, m.checkedCount())

	m = runBulkUntag(t, m, "wip")

	require.Len(t, f.submitted, 2, "only the two rows carrying the tag should be submitted")
	got := map[string]bool{}
	for _, r := range f.submitted {
		require.Equal(t, rec.TagOpRemove, r.TagOp)
		require.Equal(t, "wip", r.TagName)
		got[r.TargetID] = true
	}
	require.True(t, got["0"] && got["2"], "the tagged ids are the ones in scope")

	byID := map[string][]string{}
	for _, r := range m.rows {
		byID[r.ID] = r.Tags
	}
	require.Empty(t, byID["0"])
	require.Equal(t, []string{"review"}, byID["1"], "an untouched row keeps its own tag")
	require.Equal(t, []string{"review"}, byID["2"], "only the named tag comes off a multi-tag row")
	require.Contains(t, strip(m.View()), "✓ untagged 2 records")

	// Neither op destroys the rows, so the selection survives both.
	require.Equal(t, 3, m.checkedCount(), "a bulk untag should not clear the selection")
}

// TestBulkUntagNothingInScope: no checked row carries the tag, so there is
// nothing to send. The flash says so instead of reporting a removal of zero.
func TestBulkUntagNothingInScope(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"review"}, []string{"review"}))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = submitUntag(t, m, "wip")

	require.Empty(t, f.submitted, "nothing in scope must cost no round trips at all")
	require.Contains(t, strip(m.View()), "no checked record has that tag")
	require.Equal(t, 2, m.checkedCount())
}

// TestBulkUntagDoesNotBlockUpdate is the freeze guard, the same one bulk tag
// and bulk delete carry: ctrl+a can check an entire archive, so the N round
// trips must happen in a command rather than on the event loop.
func TestBulkUntagDoesNotBlockUpdate(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"wip"}, []string{"wip"}, []string{"wip"}))})
	m, _ = step(t, m, press("ctrl+a"))

	m, _ = step(t, m, press("ctrl+x"))
	for _, r := range "wip" {
		m, _ = step(t, m, press(string(r)))
	}
	m, cmd := step(t, m, press("enter"))

	require.Empty(t, f.submitted, "confirming must not submit inline: the work belongs in the command")
	for _, r := range m.rows {
		require.Equalf(t, []string{"wip"}, r.Tags, "no row may lose the tag before the command has run: %+v", r)
	}
	require.Contains(t, strip(m.View()), "untagging 3 records…")
	require.NotNil(t, cmd, "confirming must hand back the command that does the work")

	for _, msg := range collect(cmd) {
		m, _ = step(t, m, msg)
	}
	require.Len(t, f.submitted, 3)
	for _, r := range m.rows {
		require.Empty(t, r.Tags)
	}
}

// TestBulkUntagPartialFailure: one call in the batch fails. The rest still go
// through, the failed id keeps its tag, and the flash reports both counts.
func TestBulkUntagPartialFailure(t *testing.T) {
	f := &fakeBackend{submitFailID: map[string]bool{"1": true}}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"wip"}, []string{"wip"}, []string{"wip"}))})
	m, _ = step(t, m, press("ctrl+a"))

	m = runBulkUntag(t, m, "wip")

	byID := map[string][]string{}
	for _, r := range m.rows {
		byID[r.ID] = r.Tags
	}
	require.Empty(t, byID["0"])
	require.Equal(t, []string{"wip"}, byID["1"], "the failing id must keep the tag it still has")
	require.Empty(t, byID["2"])
	require.Contains(t, strip(m.View()), "untagged 2, 1 failed")
	require.True(t, m.isChecked("1"), "the failed id stays checked so it can be retried")
}

// TestUntagSurvivesPeriodChange is the allRows regression, the mirror of
// TestOptimisticTagSurvivesPeriodChange: a removal that reached only m.rows is
// undone the next time applyPeriodFilter rebuilds them from allRows. It also
// pins the aliasing hazard in dropTag. Narrowing the period copies each record
// into m.rows, so m.rows[i].Tags and m.allRows[j].Tags are separate headers
// over ONE array; compacting that array in place would rewrite it under the
// second header, which still carries the old length, and the row would come
// back showing "review" twice.
func TestUntagSurvivesPeriodChange(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkTaggedRows([]string{"wip", "review"}, []string{"wip"})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	// Narrow to "Today" first: on "All", rows and allRows are the same slice, so
	// neither the missing-allRows write nor the shared array would show up here.
	m, _ = step(t, m, press("1"))
	require.NotEmpty(t, m.rows)

	m, _ = submitUntag(t, m, "wip")
	require.Equal(t, []string{"review"}, m.rows[0].Tags,
		"the remaining tag must survive exactly once")

	// Back to "All": rows becomes the allRows slice outright. A removal that
	// never reached allRows vanishes here.
	m, _ = step(t, m, press("5"))
	found := false
	for _, r := range m.rows {
		if r.ID == rows[0].ID {
			found = true
			require.Equal(t, []string{"review"}, r.Tags,
				"the removal must survive a period-filter round trip, and not duplicate what is left")
		}
	}
	require.True(t, found)
}

// TestUntagOnEmptyTableDoesNothing: with no rows and nothing checked there is
// no target, so the box must not open at all; the same guard ctrl+t carries.
func TestUntagOnEmptyTableDoesNothing(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(nil)})

	m, _ = step(t, m, press("ctrl+x"))
	require.False(t, m.tagging, "there is nothing to untag")
}

// TestUntagEmptyNameIsANoop: enter on an empty box closes it without asking the
// daemon to remove a tag with no name.
func TestUntagEmptyNameIsANoop(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"wip"}))})

	m, _ = step(t, m, press("ctrl+x"))
	m, _ = step(t, m, press("enter"))

	require.False(t, m.tagging)
	require.Empty(t, f.submitted)
	require.Equal(t, []string{"wip"}, m.rows[0].Tags)
}

// TestUntagNameIsNormalized: tag names are lowercased identity everywhere else,
// so a removal typed in caps has to reach the same tag the add created.
func TestUntagNameIsNormalized(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkTaggedRows([]string{"wip"}))})

	m, _ = submitUntag(t, m, "WIP")

	require.Len(t, f.submitted, 1, "the typed case must not decide whether the tag is found")
	require.Equal(t, "wip", f.submitted[0].TagName)
	require.Empty(t, m.rows[0].Tags)
}

// TestUntagChecksTagsOutsideTheVisiblePage: the scope pass reads m.allRows, so
// a checked id the period tab is hiding is still found and still untagged. The
// state is built by hand because no key can reach it today: every path that
// changes the visible row set (search, host, period, leaving the table) clears
// the selection first, so checked ⊆ rows holds. That is what makes this worth
// pinning rather than dropping: the lookup does not lean on that invariant, and
// a bulk untag that silently skipped rows would be a quiet wrong answer if it
// ever stopped holding.
func TestUntagChecksTagsOutsideTheVisiblePage(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	rows := mkTaggedRows([]string{"wip"}, []string{"wip"})
	rows[1].StartMs = now - 30*86_400_000 // a month back: outside "Today"
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(rows)})

	m, _ = step(t, m, press("1")) // narrow to Today: row 1 leaves the page
	require.Len(t, m.rows, 1)
	require.Len(t, m.allRows, 2)

	m.checked = map[string]struct{}{rows[0].ID: {}, rows[1].ID: {}}

	runBulkUntag(t, m, "wip")
	require.Len(t, f.submitted, 2, "a checked id off the visible page is still in scope")
}

// TestTagStillAddsAfterUntagExists guards the shared box: ctrl+t must keep
// submitting an add, with no remove op riding along from the new path.
func TestTagStillAddsAfterUntagExists(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("a", "b"))})

	m, _ = step(t, m, press("ctrl+t"))
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("wip")})
	m, _ = step(t, m, press("enter"))

	require.Len(t, f.submitted, 1)
	require.Equal(t, rec.TagOpAdd, f.submitted[0].TagOp, "ctrl+t must still be an add")
	require.Contains(t, m.rows[0].Tags, "wip")
}
