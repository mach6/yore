package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// agentStat is one executor's aggregate activity over the selected period.
type agentStat struct {
	name      string
	count     int
	success   int // rows with a known exit == 0
	failures  int // rows with a known exit != 0
	knownExit int
	durSum    float64
	durN      int
	lastMs    int64
}

func (a agentStat) successPS() float64 {
	if a.knownExit == 0 {
		return -1
	}
	return 100 * float64(a.success) / float64(a.knownExit)
}

func (a agentStat) avgDurMs() float64 {
	if a.durN == 0 {
		return -1
	}
	return a.durSum / float64(a.durN)
}

// agentsData is the aggregation behind the agent-monitor view: one row per
// executor tag (agent), plus totals.
type agentsData struct {
	agents      []agentStat
	total       int // total agent commands in the period
	periodLabel string
}

// computeAgents groups tagged (agent) commands by executor over the last
// periodDays days (0 = all). Untagged commands the user typed are excluded —
// this view is specifically "what agents ran".
func computeAgents(rows []rec.Record, now int64, periodDays int) *agentsData {
	cutoff := int64(0)
	if periodDays > 0 {
		cutoff = now - int64(periodDays)*86_400_000
	}
	byTag := map[string]*agentStat{}
	total := 0
	for _, r := range rows {
		if r.Deleted() || r.Tag == "" || r.StartMs < cutoff {
			continue
		}
		total++
		a := byTag[r.Tag]
		if a == nil {
			a = &agentStat{name: r.Tag}
			byTag[r.Tag] = a
		}
		a.count++
		if r.StartMs > a.lastMs {
			a.lastMs = r.StartMs
		}
		if r.Exit != nil {
			a.knownExit++
			if *r.Exit == 0 {
				a.success++
			} else {
				a.failures++
			}
		}
		if r.DurMs != nil {
			a.durSum += float64(*r.DurMs)
			a.durN++
		}
	}

	out := &agentsData{total: total, periodLabel: periodLabel(periodDays)}
	for _, a := range byTag {
		out.agents = append(out.agents, *a)
	}
	sort.Slice(out.agents, func(i, j int) bool {
		if out.agents[i].count != out.agents[j].count {
			return out.agents[i].count > out.agents[j].count
		}
		return out.agents[i].name < out.agents[j].name
	})
	return out
}

// agentsTitle is the header shown in place of the search bar in agent-monitor
// mode: a title, the period tabs, and a total.
func (m Model) agentsTitle(w int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.Title.Render("AGENTS"))
	b.WriteString("  ")
	for i, p := range statPeriods {
		key := strconv.Itoa(i + 1)
		style := th.Dim
		if i == m.statsPeriod {
			style = th.Accent.Bold(true)
		}
		b.WriteString(style.Render(key+" "+p.label) + " ")
	}
	if m.agents != nil {
		b.WriteString(th.Dim.Render(fmt.Sprintf(" · %d agent commands", m.agents.total)))
	}
	return clipW(b.String(), w)
}

// renderAgents draws the agent-monitor table: one row per executor with counts,
// success rate, failures, average duration, and last-seen.
func (m Model) renderAgents(w, h int) string {
	th := m.th
	if m.agents == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}
	if len(m.agents.agents) == 0 {
		body := []string{
			"",
			"  " + th.Norm.Render("No agent commands"+periodSuffix(m.agents.periodLabel)+"."),
			"",
			"  " + th.Dim.Render("Agents are auto-detected in your interactive shell, or install"),
			"  " + th.Dim.Render("the Claude Code hook with:  yore init claude-code"),
		}
		return padLines(body, w, h)
	}

	// Column layout: Executor | Commands | Success | Fails | Avg | Last seen.
	nameW := 20
	numW := 9
	lastW := 12
	if w < nameW+numW*4+lastW+6 {
		nameW = maxInt(10, w-numW*4-lastW-6)
	}

	header := agentRow(th.Dim, "EXECUTOR", "COMMANDS", "SUCCESS", "FAILS", "AVG", "LAST", nameW, numW, lastW)
	lines := []string{header}
	now := m.now()
	for _, a := range m.agents.agents {
		success := "n/a"
		if a.successPS() >= 0 {
			success = fmt.Sprintf("%.0f%%", a.successPS())
		}
		avg := "n/a"
		if a.avgDurMs() >= 0 {
			avg = theme.Duration(int64(a.avgDurMs()))
		}
		fails := strconv.Itoa(a.failures)
		if a.failures == 0 {
			fails = "·"
		}
		lines = append(lines, agentRow(th.Norm,
			a.name, strconv.Itoa(a.count), success, fails, avg,
			theme.RelTime(now, a.lastMs), nameW, numW, lastW))
	}
	return padLines(lines, w, h)
}

// agentRow formats one fixed-width agent table row.
func agentRow(style lipgloss.Style, name, cmds, success, fails, avg, last string, nameW, numW, lastW int) string {
	cell := func(sty lipgloss.Style, s string, wdt int, right bool) string {
		s = truncCols(s, wdt)
		pad := wdt - runewidth.StringWidth(s)
		if pad < 0 {
			pad = 0
		}
		if right {
			return strings.Repeat(" ", pad) + sty.Render(s)
		}
		return sty.Render(s) + strings.Repeat(" ", pad)
	}
	return cell(style, name, nameW, false) + "  " +
		cell(style, cmds, numW, true) + "  " +
		cell(style, success, numW, true) + "  " +
		cell(style, fails, numW, true) + "  " +
		cell(style, avg, numW, true) + "  " +
		cell(style, last, lastW, true)
}

func periodSuffix(label string) string {
	if label == "All" {
		return ""
	}
	return " in the last " + strings.ToLower(label)
}
