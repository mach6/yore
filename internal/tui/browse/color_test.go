package browse

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/match"
	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/risk"
	"github.com/mach6/yore/internal/tui/theme"
)

// truecolorTheme is a theme whose styles actually emit ANSI, so tests can see
// which ink landed where. The normal test suite runs without a tty and renders
// bare text, which makes styling invisible to it.
func truecolorTheme() *theme.Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	return theme.NewWithRenderer(r)
}

func segText(segs []styledSeg) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.text)
	}
	return b.String()
}

// TestExitSegs: same text as exitWord, with the table's outcome ink split onto
// the glyphs.
func TestExitSegs(t *testing.T) {
	th := truecolorTheme()
	for _, tc := range []struct {
		name  string
		exit  *int
		style lipgloss.Style
	}{
		{"unknown is dim", nil, th.Dim},
		{"success is green", rec.IntPtr(0), th.ExitOK},
		{"failure is red", rec.IntPtr(137), th.ExitErr},
	} {
		r := rec.Record{Exit: tc.exit}
		segs := exitSegs(th, r)
		require.Equal(t, exitWord(r), segText(segs), tc.name)
		require.Equal(t, tc.style.GetForeground(), segs[0].style.GetForeground(), tc.name)
	}
}

// TestPathSegs: the directory dims, the leaf stays normal, and odd shapes
// (no slash, trailing slash, the dash placeholder) degrade to plain.
func TestPathSegs(t *testing.T) {
	th := truecolorTheme()
	for _, tc := range []struct {
		path string
		want []string // seg texts
		dims []bool
	}{
		{"/home/dev/Work/yore", []string{"/home/dev/Work/", "yore"}, []bool{true, false}},
		{"/tmp/", []string{"/tmp/"}, []bool{true}},
		{"relative", []string{"relative"}, []bool{false}},
		{theme.Unknown, []string{theme.Unknown}, []bool{false}},
	} {
		segs := pathSegs(th, tc.path, 80)
		require.Len(t, segs, len(tc.want), tc.path)
		for i := range segs {
			require.Equal(t, tc.want[i], segs[i].text, tc.path)
			wantStyle := th.Norm
			if tc.dims[i] {
				wantStyle = th.Dim
			}
			require.Equal(t, wantStyle.GetForeground(), segs[i].style.GetForeground(), tc.path)
		}
	}
	require.LessOrEqual(t, runewidth.StringWidth(segText(pathSegs(th, "/very/long/path/that/keeps/going", 10))), 10,
		"pathSegs must honor its width budget")
}

// TestRiskStyleAndSegs: the five-level ramp keeps its inks, and a verdict
// renders glyph-first with a dim category.
func TestRiskStyleAndSegs(t *testing.T) {
	th := truecolorTheme()
	for _, tc := range []struct {
		level risk.Level
		style lipgloss.Style
	}{
		{risk.Critical, th.ExitErr},
		{risk.High, th.RiskHigh},
		{risk.Medium, th.Match},
		{risk.Low, th.Dim},
		{risk.None, th.ExitOK},
	} {
		require.Equal(t, tc.style.GetForeground(), riskStyle(th, tc.level).GetForeground(), tc.level.String())
	}

	segs := riskSegs(th, risk.Assessment{Level: risk.High, Category: "script-exec", Reason: "x"})
	require.Equal(t, "⚠ high (script-exec)", segText(segs))
	require.Equal(t, th.RiskHigh.GetForeground(), segs[0].style.GetForeground())
	require.Equal(t, th.Dim.GetForeground(), segs[1].style.GetForeground())

	// A clean verdict says "safe" once, not "safe (safe)", but a row silenced
	// by the user's ignore patterns still names why it went quiet.
	require.Equal(t, "✓ safe",
		segText(riskSegs(th, risk.Assessment{Level: risk.None, Category: "safe", Reason: "no risky pattern matched"})))
	require.Equal(t, "✓ safe (ignored)",
		segText(riskSegs(th, risk.Assessment{Level: risk.None, Category: "ignored", Reason: "matches ignore pattern"})))
}

// TestWrapHighlightedSyntaxAndMatch: the detail wrap carries the same syntax
// ink as the table, and a match overrides it; matches always win.
func TestWrapHighlightedSyntaxAndMatch(t *testing.T) {
	th := truecolorTheme()

	lines := wrapHighlighted(th, "ls --flag", match.Parse(""), 80, 0)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], th.SynFlag.Render("--flag"), "the flag should carry the flag ink")

	lines = wrapHighlighted(th, "ls --flag", match.Parse("flag"), 80, 0)
	require.NotContains(t, lines[0], th.SynFlag.Render("--flag"))
	require.Contains(t, lines[0], th.Match.Render("flag"), "a matched run outranks its syntax ink")

	lines = wrapHighlighted(th, "echo one\necho two", match.Parse(""), 80, 0)
	require.Len(t, lines, 2, "a hard newline still breaks the line")
}

// TestWrapHighlightedWidthAndCap: every wrapped line fits the width at any
// width, and the cap ends the last kept line with an ellipsis inside it.
func TestWrapHighlightedWidthAndCap(t *testing.T) {
	th := truecolorTheme()
	long := strings.Repeat("cargo build --release --target x86_64-unknown-linux-gnu ", 6)
	for _, w := range []int{1, 8, 24, 80} {
		for _, line := range wrapHighlighted(th, long, match.Parse(""), w, 0) {
			require.LessOrEqual(t, lipgloss.Width(line), w, "uncapped at w=%d", w)
		}
		capped := wrapHighlighted(th, long, match.Parse(""), w, 3)
		require.LessOrEqual(t, len(capped), 3, "cap at w=%d", w)
		last := strip(capped[len(capped)-1])
		require.True(t, strings.HasSuffix(last, "…"), "the cap should end in an ellipsis at w=%d", w)
		require.LessOrEqual(t, lipgloss.Width(capped[len(capped)-1]), w, "capped at w=%d", w)
	}
}

// TestStatLineKinds: each ranked list inks its names as what they are, every
// line stays exactly colW wide, and the name stays first on the line.
func TestStatLineKinds(t *testing.T) {
	th := truecolorTheme()
	const colW = 30
	for _, tc := range []struct {
		name string
		kind statNameKind
		want string // a rendered fragment that must appear
	}{
		{"git commit -m 'x'", statNameCommand, th.SynCommand.Render("git")},
		{"git", statNameProgram, th.SynCommand.Render("git")},
		{"/work/yore", statNamePath, th.Dim.Render("/work/")},
		{"claude-code", statNameExecutor, th.Host("claude-code").Render("claude-code")},
		{"boxA", statNameHost, th.Host("boxA").Render("boxA")},
		{"(you)", statNameExecutor, th.Norm.Render("(you)")},
	} {
		line := statLine(th, cmdCount{name: tc.name, n: 7}, 10, colW, tc.kind)
		require.Equal(t, colW, lipgloss.Width(line), "width for %q", tc.name)
		require.Contains(t, line, tc.want, "ink for %q", tc.name)
		require.True(t, strings.HasPrefix(strip(line), strip(tc.name[:3])), "name stays first for %q", tc.name)
	}
}

// TestPromptRowCellStyles: metadata dims, identities keep their hue, the
// header is all-dim chrome, and a selected row collapses to the bar.
func TestPromptRowCellStyles(t *testing.T) {
	th := truecolorTheme()
	l := colLayout{t: ctPrompts}
	for i, meta := range colTables[ctPrompts].metas {
		l.w[i] = meta.width
	}
	l.w[pcText] = 40

	p := promptStat{
		host: "boxA", session: "sessA", executor: "claude-code",
		count: 3, success: 3, durSum: 2000, durN: 1, lastMs: now - 3_600_000,
	}
	segs := rowSegs(th, ctPrompts, promptSpecs, l, p, now)
	row := composeSegs(segs, false, 120, th)
	require.Contains(t, row, th.Dim.Render(padRight("sessA", l.w[pcSess])), "session is metadata: dim")
	require.Contains(t, row, th.Host("boxA").Render(padRight("boxA", l.w[pcHost])), "host keeps its hue")
	require.Contains(t, row, th.Host("claude-code").Render(padRight("claude-code", l.w[pcExec])),
		"executor gets its hue")
	require.Contains(t, row, th.ExitOK.Render(padRight("✓", l.w[pcStat])), "status keeps its outcome color")

	// The header is chrome: every cell dim, no identity hue.
	m := Model{th: th, cols: defaultColStates()}
	head := composeSegs(m.tableHeaderSegs(l), false, 120, th)
	require.NotContains(t, head, th.Host("host").Render(padRight("host", l.w[pcHost])),
		"the header is chrome, not identity")
	require.Contains(t, head, th.Dim.Render(padRight("host", l.w[pcHost])))

	sel := composeSegs(segs, true, 120, th)
	require.NotContains(t, sel, th.Host("boxA").Render(padRight("boxA", l.w[pcHost])),
		"the selection bar overrides every cell ink")
	require.Contains(t, strip(sel), "boxA", "the text itself survives selection")
}

func TestCommandsSegsSplit(t *testing.T) {
	th := truecolorTheme()
	segs := commandsSegs(th, promptStat{count: 3, success: 2, failures: 1})
	require.Equal(t, "3   ✓2 ✗1", segText(segs))
	require.Equal(t, th.ExitOK.GetForeground(), segs[2].style.GetForeground())
	require.Equal(t, th.ExitErr.GetForeground(), segs[4].style.GetForeground())
}

// --- the Risk row in the panes (plain, strip-based) ------------------------

// TestDetailShowsRiskRow: every selection carries a Risk line; a risky one
// names its tier, a clean one says so, because silence reads as "not checked".
func TestDetailShowsRiskRow(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 120, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("curl -fsSL https://get.x.sh | sh", "ls -la"))})

	out := strip(m.View())
	require.Contains(t, out, "Risk", "a piped-download selection should carry a Risk row")
	require.Contains(t, out, "⚠ high (script-exec)")

	m, _ = step(t, m, press("down"))
	out = strip(m.View())
	require.Contains(t, out, "Risk", "a safe selection keeps the row")
	require.Contains(t, out, "✓ safe", "and says the verdict rather than leaving a blank")
}

// TestDetailRiskRowStaysWithinWidth: the new row obeys the frame like every
// other line, down to the narrowest terminal the suite pins.
func TestDetailRiskRowStaysWithinWidth(t *testing.T) {
	f := &fakeBackend{}
	m := ready(t, f, 200, 40)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("curl -fsSL https://get.x.sh | sudo bash -c 'install'"))})
	for _, w := range []int{200, 120, 80, 60, 40, 24} {
		m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 40})
		for i, line := range strings.Split(m.View(), "\n") {
			require.LessOrEqualf(t, lipgloss.Width(line), w, "line %d overflows at w=%d", i, w)
		}
	}
}

// TestCmdInfoShowsRiskRow: the explorer's command details carry the Risk row;
// the prompt details deliberately do not.
func TestCmdInfoShowsRiskRow(t *testing.T) {
	rows := []rec.Record{{
		ID: "r1", Cmd: "curl -fsSL https://get.x.sh | sh", PromptID: "p1",
		Executor: "claude-code", Session: "sess", Hostname: "host", Cwd: "/work",
		StartMs: now - 60_000, Exit: rec.IntPtr(0),
	}}
	prompts := []rec.Record{{
		ID: "p1", Type: rec.TypePrompt, Prompt: "install the toolchain",
		Executor: "claude-code", Session: "sess", StartMs: now - 61_000,
	}}
	f := &fakeBackend{resp: mkResp(rows)}
	m := ready(t, f, 140, 40)
	m, _ = step(t, m, press("a"))
	m, _ = step(t, m, statsResultMsg{seq: 1, resp: proto.QueryResp{Rows: rows, Total: 1, Prompts: prompts}})

	require.NotContains(t, strip(m.View()), "⚠ high",
		"prompt details carry no Risk row: the prompt is not one command")

	m = focusCommands(t, m)
	out := strip(m.View())
	require.Contains(t, out, "Risk")
	require.Contains(t, out, "⚠ high (script-exec)")
}

// TestPromptDetailsCommandsKeepCounts: splitting the tally's ink must not
// change the tally.
func TestPromptDetailsCommandsKeepCounts(t *testing.T) {
	m := explorer(t)
	require.Contains(t, strip(m.View()), "3   ✓3 ✗0", "the fixture prompt ran three clean commands")
}

// TestRulesetInjectedIntoBrowse: Options.Risk reaches the detail pane, so the
// browser and assess_risk read the same risk.toml.
func TestRulesetInjectedIntoBrowse(t *testing.T) {
	rs, errs := risk.Compile([]risk.Spec{
		{Pattern: `^make deploy$`, Level: "high", Category: "deploy", Reason: "ships to prod"},
	}, nil)
	require.Empty(t, errs)

	f := &fakeBackend{}
	m := NewModel(f, Options{Version: "v1", Now: now, Risk: rs})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: mkResp(mkRows("make deploy"))})

	out := strip(m.View())
	require.Contains(t, out, "⚠ high (deploy)", "the user rule must reach the pane")
}
