package browse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

// countOf is the tally for one name in a ranked list, or 0 when it is absent.
func countOf(items []cmdCount, name string) int {
	for _, c := range items {
		if c.name == name {
			return c.n
		}
	}
	return 0
}

// TestComputeStatsByTag: a row lands in one bucket per tag it carries, so a
// command tagged twice is counted under both. That is what makes this list
// unlike every other one on the screen: the counts do not partition the total.
func TestComputeStatsByTag(t *testing.T) {
	rows := mkTaggedRows(
		[]string{"wip", "review"},
		[]string{"wip"},
		nil,
	)
	s := computeStats(rows, len(rows), now, 0)

	require.Equal(t, 2, countOf(s.byTag, "wip"))
	require.Equal(t, 1, countOf(s.byTag, "review"))
	require.Len(t, s.byTag, 2, "only tags actually carried should appear")
	require.Equal(t, 3, s.total, "the fan-out must not inflate the row total")
	require.Equal(t, "wip", s.byTag[0].name, "the list is ranked, most-used first")
}

// TestComputeStatsByTagSkipsDeletedAndOutOfPeriod holds the tag tally to the
// same rules the other ranked lists follow: tombstones never count, and the
// period tabs narrow it like everything else on the screen.
func TestComputeStatsByTagSkipsDeletedAndOutOfPeriod(t *testing.T) {
	rows := mkTaggedRows([]string{"wip"}, []string{"wip"}, []string{"wip"})
	rows[1].DeletedMs = now
	rows[2].StartMs = now - 30*86_400_000 // a month back

	all := computeStats(rows, len(rows), now, 0)
	require.Equal(t, 2, countOf(all.byTag, "wip"), "the tombstone must not be counted")

	today := computeStats(rows, len(rows), now, 1)
	require.Equal(t, 1, countOf(today.byTag, "wip"), "the month-old row is outside Today")
}

// TestComputeStatsByTagEmpty: an archive nobody has tagged produces no list at
// all, which is what the renderer gates the column on.
func TestComputeStatsByTagEmpty(t *testing.T) {
	s := computeStats(mkRows("ls", "vim"), 2, now, 0)
	require.Empty(t, s.byTag)
}

// statsView drives the model to the stats screen over the given rows and
// returns what it drew.
func statsView(t *testing.T, rows []rec.Record, w int) string {
	t.Helper()
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, w, 40)
	m, _ = step(t, m, press("s"))
	m, _ = step(t, m, statsResultMsg{seq: 1, resp: mkResp(rows)})
	return strip(m.View())
}

// TestStatsTagColumnAppearsOnlyWhenTagged is the layout contract: the ranked
// columns share a fixed width, so a permanent sixth one would cost the other
// five their space on an archive that has never been tagged. It shows up
// exactly when it has something to say.
func TestStatsTagColumnAppearsOnlyWhenTagged(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []rec.Record
		want bool
	}{
		{"tagged", mkTaggedRows([]string{"wip"}, []string{"review"}), true},
		{"untagged", mkRows("ls", "vim"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := statsView(t, tc.rows, 200)
			if tc.want {
				require.Contains(t, out, "By tag")
				require.Contains(t, out, "wip", "the column should list the tags themselves")
			} else {
				require.NotContains(t, out, "By tag")
			}
			// The columns it sits beside are there either way.
			require.Contains(t, out, "Top programs")
			require.Contains(t, out, "By host")
		})
	}
}

// TestStatsTagColumnDropsFirstOnANarrowTerminal: statColumns drops from the
// right, and tags are last, so the five lists a user who never tags still reads
// are the ones that survive the squeeze.
func TestStatsTagColumnDropsFirstOnANarrowTerminal(t *testing.T) {
	rows := mkTaggedRows([]string{"wip"}, []string{"review"})

	require.Contains(t, statsView(t, rows, 200), "By tag", "a wide terminal fits every column")

	narrow := statsView(t, rows, 100)
	require.NotContains(t, narrow, "By tag", "the tag column yields its width first")
	require.Contains(t, narrow, "Top programs", "the priority columns keep theirs")
}
