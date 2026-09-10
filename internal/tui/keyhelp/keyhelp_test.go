package keyhelp

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/tui/theme"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func strip(s string) string { return ansiRE.ReplaceAllString(s, "") }

func testRows() []Row {
	return []Row{
		{Keys: "↑↓", Desc: "move"},
		{Keys: "enter", Desc: "put it on the prompt"},
		{Keys: "y", Desc: "copy"},
		{Keys: "?", Desc: "keys"},
	}
}

func testGroups() []Group {
	return []Group{
		{Title: "MOVE", Rows: []Row{
			{Keys: "↑/k", Desc: "up"},
			{Keys: "↓/j", Desc: "down"},
			{Keys: "pgup/pgdn", Desc: "page"},
		}},
		{Title: "ACT", Rows: []Row{
			{Keys: "enter", Desc: "put it on the prompt"},
			{Keys: "y", Desc: "copy"},
		}},
		{Title: "GO", Rows: []Row{
			{Keys: "s", Desc: "stats"},
			{Keys: "q", Desc: "quit"},
		}},
	}
}

// TestLineNeverExceedsWidth: the footer shares its row with nothing, so an
// overrun wraps and steals a line from the panes above it.
func TestLineNeverExceedsWidth(t *testing.T) {
	th := theme.New()
	for _, w := range []int{1, 5, 12, 24, 40, 80, 200} {
		got := strip(Line(th, testRows(), w))
		require.LessOrEqualf(t, runewidth.StringWidth(got), w, "w=%d: %q is too wide", w, got)
		require.NotContains(t, got, "\n", "the line must stay one line")
	}
}

// TestLineDropsFromTheEnd: hints are ordered by how much they matter, so what
// falls off a narrow terminal is the least important thing, not the first thing.
func TestLineDropsFromTheEnd(t *testing.T) {
	th := theme.New()
	wide := strip(Line(th, testRows(), 200))
	require.Contains(t, wide, "? keys", "a wide line shows every hint")

	narrow := strip(Line(th, testRows(), 20))
	require.Contains(t, narrow, "↑↓ move", "the first hint is always kept")
	require.NotContains(t, narrow, "? keys", "the last hint sheds first")
}

// TestLineKeepsTheFirstHint: even at an absurd width the line says something,
// because a footer that renders empty reads as "this view has no keys".
func TestLineKeepsTheFirstHint(t *testing.T) {
	require.NotEmpty(t, strip(Line(theme.New(), testRows(), 3)))
	require.Empty(t, Line(theme.New(), nil, 80), "no rows is genuinely nothing to say")
}

// TestPanelFillsExactlyItsBox: the panel is drawn inside a border that was sized
// before it rendered, so it must return exactly h lines of at most w columns;
// one line over and the frame no longer fits the terminal.
func TestPanelFillsExactlyItsBox(t *testing.T) {
	th := theme.New()
	for _, w := range []int{20, 40, 78, 118, 200} {
		for _, h := range []int{1, 4, 12, 30} {
			body, _ := Panel(th, testGroups(), w, h, 0)
			lines := strings.Split(body, "\n")
			require.Lenf(t, lines, h, "%dx%d: wrong line count", w, h)
			for i, l := range lines {
				require.LessOrEqualf(t, runewidth.StringWidth(strip(l)), w,
					"%dx%d: line %d overflows: %q", w, h, i, strip(l))
			}
		}
	}
}

// TestPanelReportsWhatItCouldNotShow: the caller offers a scroll hint only when
// there is something below the fold, so the total has to be the real one.
func TestPanelReportsWhatItCouldNotShow(t *testing.T) {
	th := theme.New()

	_, total := Panel(th, testGroups(), 30, 4, 0)
	require.Greater(t, total, 4, "a narrow, short panel cannot show everything")

	body, _ := Panel(th, testGroups(), 30, 4, 0)
	require.Contains(t, strip(body), "MOVE", "top of the list at offset 0")

	scrolled, _ := Panel(th, testGroups(), 30, 4, 3)
	require.NotEqual(t, body, scrolled, "the offset should move the window")

	// Past the end, the window clamps rather than rendering blank.
	clamped, _ := Panel(th, testGroups(), 30, 4, 999)
	require.NotEmpty(t, strings.TrimSpace(strip(clamped)), "an over-scrolled panel still shows the tail")
}

// TestPanelUsesTheWidthItHas: given room for more columns it uses them, which is
// the whole reason the list fits on a stock terminal at all.
func TestPanelUsesTheWidthItHas(t *testing.T) {
	th := theme.New()
	_, narrow := Panel(th, testGroups(), 30, 40, 0)
	_, wide := Panel(th, testGroups(), 120, 40, 0)
	require.Less(t, wide, narrow, "a wider panel should need fewer rows")
}

// TestPanelWithoutGroupsIsBlank rather than a crash: a view with nothing to say
// is a design mistake, not a runtime one.
func TestPanelWithoutGroupsIsBlank(t *testing.T) {
	body, total := Panel(theme.New(), nil, 40, 5, 0)
	require.Zero(t, total)
	require.Len(t, strings.Split(body, "\n"), 5)
	require.Empty(t, strings.TrimSpace(body))
}
