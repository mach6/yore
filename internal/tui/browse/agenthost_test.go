package browse

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

// hostFixture is the explorer's sample spread over two machines: boxA ran two
// claude-code commands under one prompt, boxB ran one codex command under
// another. boxBMinsAgo lets a test push boxB's work out of a time window.
func hostFixture(boxBMinsAgo int64) (rows, prompts []rec.Record) {
	specs := []struct {
		id, prompt, cmd, executor, host, session string
		minsAgo                                  int64
	}{
		{"a1", "pa", "go build ./...", "claude-code", "boxA", "sessA", 12},
		{"a2", "pa", "go test ./...", "claude-code", "boxA", "sessA", 11},
		{"b1", "pb", "npm run lint", "codex", "boxB", "sessB", boxBMinsAgo},
	}
	for _, s := range specs {
		rows = append(rows, rec.Record{
			ID: s.id, Cmd: s.cmd, PromptID: s.prompt,
			Executor: s.executor, Session: s.session, Hostname: s.host, Cwd: "/work",
			StartMs: now - s.minsAgo*60_000, Exit: rec.IntPtr(0),
		})
	}
	for _, p := range []struct {
		id, text, executor, host, session string
		minsAgo                           int64
	}{
		{"pa", "refactor the auth layer", "claude-code", "boxA", "sessA", 13},
		{"pb", "write the release notes", "codex", "boxB", "sessB", boxBMinsAgo + 1},
	} {
		prompts = append(prompts, rec.Record{
			ID: p.id, Type: rec.TypePrompt, Prompt: p.text, Hostname: p.host,
			Executor: p.executor, Session: p.session, StartMs: now - p.minsAgo*60_000,
		})
	}
	return rows, prompts
}

// explorerHosts opens the agent explorer on a two-host sample, wide enough
// that the prompt pane keeps every column.
func explorerHosts(t *testing.T, rows, prompts []rec.Record) Model {
	t.Helper()
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, 180, 40)
	m, _ = step(t, m, press("a"))
	m, _ = step(t, m, statsResultMsg{seq: 1, resp: proto.QueryResp{
		Rows: rows, Total: len(rows), Prompts: prompts,
	}})
	return m
}

// focusSidebar tabs round to the executor sidebar, whichever pane focus starts on.
func focusSidebar(t *testing.T, m Model) Model {
	t.Helper()
	for range agentPaneCount {
		if m.apane == apAgents {
			return m
		}
		m, _ = step(t, m, press("tab"))
	}
	require.Equal(t, apAgents, m.apane, "could not focus the sidebar")
	return m
}

// TestHostCycleWalksHostsAndReturnsToAll: H walks the ring all → each host in
// name order → all, and every aggregate the view shows walks with it.
func TestHostCycleWalksHostsAndReturnsToAll(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)
	require.Equal(t, []cmdCount{{name: "boxA", n: 2}, {name: "boxB", n: 1}}, m.agentHosts)

	for _, want := range []struct {
		host             string
		commands, prompt int
	}{
		{"boxA", 2, 1},
		{"boxB", 1, 1},
		{"", 3, 2},
	} {
		m, _ = step(t, m, press("H"))
		require.Equal(t, want.host, m.agentHostFilter)
		require.Equal(t, want.commands, m.agents.total, "command total on %q", want.host)
		require.Equal(t, want.prompt, m.prompts.total, "prompt total on %q", want.host)
		require.Zero(t, m.promptSel, "the cursor is on a different list now")
		require.Zero(t, m.drillSel)

		out := strip(m.View())
		if want.host == "" {
			require.NotContains(t, out, " on box", "the header names no host when unfiltered")
		} else {
			require.Contains(t, out, "on "+want.host, "the header should name the host")
		}
	}
}

// TestHostFilterNarrowsTheWorkPanes: one host's work vanishes from the
// executor sidebar, the prompt list, the command list, and the details pane
// together — the panes must not disagree about what the view is showing.
func TestHostFilterNarrowsTheWorkPanes(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)
	m, _ = step(t, m, press("H")) // boxA

	out := strip(m.View())
	require.Contains(t, out, "claude-code", "boxA's executor stays in the sidebar")
	require.NotContains(t, out, "codex", "boxB's executor is gone from every pane")
	require.Contains(t, out, "go build ./...", "boxA's commands stay")
	require.NotContains(t, out, "npm run lint", "boxB's commands are gone")
	require.Contains(t, out, "refactor the auth layer")
	require.NotContains(t, out, "write the release notes")
}

// TestHostFilterComposesWithExecutorAndTextFilters: the host filter, the
// sidebar's executor filter and the / text filter are independent narrowings;
// esc clears the typed filter and only the typed filter.
func TestHostFilterComposesWithExecutorAndTextFilters(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)

	m = focusSidebar(t, m)
	m, _ = step(t, m, press("down")) // claude-code, the busiest executor
	require.Equal(t, "claude-code", m.agentFilter)

	m, _ = step(t, m, press("H"))
	require.Equal(t, "boxA", m.agentHostFilter)
	require.Equal(t, "claude-code", m.agentFilter, "the executor exists on boxA — kept")

	m, _ = step(t, m, press("/"))
	m = typeIn(t, m, "auth")
	require.Equal(t, 1, m.promptLen())

	m, _ = step(t, m, press("esc")) // close the box, keep the query
	m, _ = step(t, m, press("esc")) // clear the query
	require.Empty(t, m.promptQ)
	require.Equal(t, "boxA", m.agentHostFilter, "esc clears typed filters, not the host")
	require.Equal(t, "claude-code", m.agentFilter, "nor the executor")
}

// TestHostFilterSurvivesAPeriodChange: like the executor filter, the host
// filter and the period are independent narrowings.
func TestHostFilterSurvivesAPeriodChange(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)
	m, _ = step(t, m, press("H"))
	m, _ = step(t, m, press("2")) // 7d — both hosts still inside it
	require.Equal(t, "boxA", m.agentHostFilter)
	require.Equal(t, 2, m.agents.total)
}

// TestHostFilterReleasesWhenPeriodDropsTheHost: a host with no agent activity
// in the window releases the filter rather than pinning the view to nothing —
// the same rule the executor filter follows.
func TestHostFilterReleasesWhenPeriodDropsTheHost(t *testing.T) {
	rows, prompts := hostFixture(40 * 24 * 60) // boxB worked 40 days ago
	m := explorerHosts(t, rows, prompts)
	m, _ = step(t, m, press("H")) // boxA
	m, _ = step(t, m, press("H")) // boxB
	require.Equal(t, "boxB", m.agentHostFilter)

	m, _ = step(t, m, press("2")) // 7d — boxB has nothing inside it
	require.Empty(t, m.agentHostFilter, "the filtered host left the window")
	require.Equal(t, []cmdCount{{name: "boxA", n: 2}}, m.agentHosts)
	require.Equal(t, 2, m.agents.total, "the view falls back to everything in the window")
}

// TestHostCycleReleasesExecutorAbsentOnHost: cycling to a host where the
// filtered executor never ran releases the executor filter by the existing
// vanished-from-the-period rule, instead of showing an empty view.
func TestHostCycleReleasesExecutorAbsentOnHost(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)

	m = focusSidebar(t, m)
	m, _ = step(t, m, press("down"))
	m, _ = step(t, m, press("down")) // codex, which only boxB ran
	require.Equal(t, "codex", m.agentFilter)

	m, _ = step(t, m, press("H")) // boxA
	require.Equal(t, "boxA", m.agentHostFilter)
	require.Empty(t, m.agentFilter, "codex has no commands on boxA")
	require.Equal(t, 2, m.agents.total)
}

// TestHostColumnOnlyOnMultiHostSamples: the prompt pane spends a column on the
// host only when the sample spans more than one — and keeps it while filtered
// to one, so the one key being pressed does not reshape the table under it.
func TestHostColumnOnlyOnMultiHostSamples(t *testing.T) {
	single := explorer(t) // the one-host fixture from the filter tests
	require.NotContains(t, promptHeaderLine(t, single), "host")

	single, _ = step(t, single, press("H"))
	require.Empty(t, single.agentHostFilter)
	require.Contains(t, strip(single.View()), "one host in this sample",
		"H on a one-host sample says why nothing happened")

	rows, prompts := hostFixture(30)
	multi := explorerHosts(t, rows, prompts)
	require.Contains(t, promptHeaderLine(t, multi), "host")
	out := strip(multi.View())
	require.Contains(t, out, "boxA")
	require.Contains(t, out, "boxB")

	multi, _ = step(t, multi, press("H"))
	require.Equal(t, "boxA", multi.agentHostFilter)
	require.Contains(t, promptHeaderLine(t, multi), "host",
		"the column must not vanish while H walks the ring")
}

// TestHostPaneShowsTheRing: the HOSTS pane is the visible state of the ring H
// walks — every host with its command count, the bullet on the active stop —
// and its rows never narrow, because it is the map of where the filter can go.
func TestHostPaneShowsTheRing(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)

	out := strip(m.View())
	require.Contains(t, out, "HOSTS  2", "the pane title carries the host count")
	require.Contains(t, out, "● all hosts", "unfiltered, the bullet sits on all hosts")
	require.Regexp(t, `boxA\s+2`, out, "each host row carries its command count")
	require.Regexp(t, `boxB\s+1`, out)
	require.Regexp(t, `all hosts\s+3`, out)

	m, _ = step(t, m, press("H"))
	out = strip(m.View())
	require.Contains(t, out, "● boxA", "the bullet follows the filter")
	require.NotContains(t, out, "● all hosts")
	require.Regexp(t, `boxB\s+1`, out, "the pane keeps every host while one is filtered")
}

// TestHostPaneCursorSelects: the host pane is a list like the executor sidebar
// — moving its cursor filters the other panes, and the H key and the cursor
// drive the same selection.
func TestHostPaneCursorSelects(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)

	m, _ = step(t, m, press("shift+tab")) // prompts -> hosts
	require.Equal(t, apHosts, m.apane)
	m, _ = step(t, m, press("down"))
	require.Equal(t, "boxA", m.agentHostFilter)
	require.Equal(t, 1, m.prompts.total)
	m, _ = step(t, m, press("down"))
	require.Equal(t, "boxB", m.agentHostFilter)

	// H continues from where the cursor put the selection.
	m, _ = step(t, m, press("H"))
	require.Empty(t, m.agentHostFilter, "after the last host, H wraps to all hosts")
	require.Zero(t, m.agentHostSel)

	m, _ = step(t, m, press("G"))
	require.Equal(t, "boxB", m.agentHostFilter, "G jumps to the last host")
	m, _ = step(t, m, press("g"))
	require.Empty(t, m.agentHostFilter, "g jumps back to all hosts")
}

// TestHostPaneSeamDrags: the seam above the HOSTS pane resizes it like every
// other seam — the pane follows the pointer, the proportion survives a resize,
// and the grab only works in the left column, where the seam actually is.
func TestHostPaneSeamDrags(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)
	seam := m.geo.hDiv2
	require.Positive(t, seam, "the explorer should have a hosts seam")
	require.Equal(t, m.geo.p[apHosts].y, seam, "the seam sits on the host pane's top border")

	// Grabbing the seam's row in the right column is not a grab — over there
	// that row is the middle of the prompt pane.
	m, _ = step(t, m, click(m.geo.vDiv+10, seam))
	require.Equal(t, dragNone, m.drag, "the hosts seam must not extend into the right column")

	// Grab it in the left column and drag it up: the host pane grows.
	m, _ = step(t, m, click(3, seam))
	require.Equal(t, dragHosts, m.drag, "clicking the seam did not start a drag")
	m, _ = step(t, m, dragTo(3, seam-4))
	require.Equal(t, seam-4, m.geo.hDiv2, "the seam did not follow the pointer")
	require.Equal(t, seam-4, m.geo.p[apHosts].y)
	m, _ = step(t, m, mouseUp(3))
	require.Equal(t, dragNone, m.drag, "releasing did not end the drag")

	// The split is held as a ratio of the top-left region, so dragging the main
	// horizontal seam keeps the proportion rather than the cell count.
	topH := m.geo.hDiv - 1
	require.Equal(t, ratioOf(seam-4-1, topH), m.splits.AgentHosts)
	grown := m.geo.p[apHosts].h
	m, _ = step(t, m, tea.WindowSizeMsg{Width: m.width, Height: m.height + 20})
	require.Greater(t, m.geo.p[apHosts].h, grown, "the dragged share must scale with the region")

	// A dragged pane stops being content-sized: the ratio wins after recompute.
	m.recomputeStats()
	require.NotZero(t, m.splits.AgentHosts)
	require.Greater(t, m.geo.p[apHosts].h, grown, "recomputing must not snap back to content size")
}

// TestHostPanePresentOnSingleHostSamples: the pane count never depends on the
// data — a one-host sample keeps the pane (like the browse sidebar), and H says
// why it has nothing to do.
func TestHostPanePresentOnSingleHostSamples(t *testing.T) {
	single := explorer(t)
	out := strip(single.View())
	require.Contains(t, out, "HOSTS  1")
	require.Contains(t, out, "● all hosts")

	single, _ = step(t, single, press("H"))
	require.Empty(t, single.agentHostFilter)
	require.Contains(t, strip(single.View()), "one host in this sample",
		"H on a one-host sample says why nothing happened")
}

// promptHeaderLine is the prompt pane's column-header row.
func promptHeaderLine(t *testing.T, m Model) string {
	t.Helper()
	for _, line := range strings.Split(strip(m.View()), "\n") {
		if strings.Contains(line, "when") && strings.Contains(line, "prompt") {
			return line
		}
	}
	require.Fail(t, "no prompt header row on screen")
	return ""
}

// TestPromptDetailsShowHost: the details pane names the machine a prompt ran
// on, and a legacy prompt record with no hostname borrows its commands'.
func TestPromptDetailsShowHost(t *testing.T) {
	rows, prompts := hostFixture(30)
	m := explorerHosts(t, rows, prompts)
	p, ok := m.drilledPrompt()
	require.True(t, ok)
	require.Equal(t, "boxA", p.host)
	require.Contains(t, strip(m.View()), "Host")

	legacy := explorer(t) // its prompt records carry no hostname
	p, ok = legacy.drilledPrompt()
	require.True(t, ok)
	require.Equal(t, "host", p.host, "backfilled from the prompt's commands")
}

// TestPromptLayoutSheds: on a narrowing pane the optional columns go one at a
// time — session, then host, then duration, then executor — so the prompt text
// itself is the last thing squeezed.
func TestPromptLayoutSheds(t *testing.T) {
	for _, tc := range []struct {
		w                                     int
		showSess, showHost, showDur, showExec bool
	}{
		{120, true, true, true, true},
		{79, false, true, true, true},
		{69, false, false, true, true},
		{57, false, false, false, true},
		{48, false, false, false, false},
	} {
		c := promptLayout(tc.w, true, true)
		require.Equal(t, tc.showSess, c.showSess, "session at w=%d", tc.w)
		require.Equal(t, tc.showHost, c.showHost, "host at w=%d", tc.w)
		require.Equal(t, tc.showDur, c.showDur, "dur at w=%d", tc.w)
		require.Equal(t, tc.showExec, c.showExec, "executor at w=%d", tc.w)
	}
	require.Equal(t, 10, promptLayout(120, true, true).hostW)
	require.False(t, promptLayout(120, true, false).showHost,
		"a one-host sample never shows the column")
}
