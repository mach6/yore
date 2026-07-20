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

const sparkDays = 30

// sparkBlocks are the eight ascending block glyphs used by the per-day
// sparkline (level 1..8).
var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// cmdCount is a name paired with a tally, used for both the top-command and
// per-host lists.
type cmdCount struct {
	name string
	n    int
}

// statsData is the pre-computed aggregation behind the stats screen.
type statsData struct {
	topCmds   []cmdCount
	perHost   []cmdCount
	spark     []int // per-day command counts, oldest (left) .. newest (right)
	sparkMax  int
	totalRows int
	days      int
}

// computeStats aggregates a broad row sample into the stats screen's data:
// the top commands by first token, per-host counts (from the sidebar), and a
// commands-per-day histogram over the last sparkDays days.
func computeStats(rows []rec.Record, hosts []hostItem, now int64) *statsData {
	s := &statsData{days: sparkDays, spark: make([]int, sparkDays)}
	counts := map[string]int{}
	for _, r := range rows {
		if r.Deleted() {
			continue
		}
		s.totalRows++
		if tok := firstToken(r.Cmd); tok != "" {
			counts[tok]++
		}
		day := int((now - r.StartMs) / 86_400_000)
		if day >= 0 && day < sparkDays {
			s.spark[sparkDays-1-day]++
		}
	}

	s.topCmds = make([]cmdCount, 0, len(counts))
	for k, v := range counts {
		s.topCmds = append(s.topCmds, cmdCount{k, v})
	}
	sort.Slice(s.topCmds, func(i, j int) bool {
		if s.topCmds[i].n != s.topCmds[j].n {
			return s.topCmds[i].n > s.topCmds[j].n
		}
		return s.topCmds[i].name < s.topCmds[j].name
	})
	if len(s.topCmds) > 20 {
		s.topCmds = s.topCmds[:20]
	}

	for i := 1; i < len(hosts); i++ { // skip the "All hosts" aggregate row
		s.perHost = append(s.perHost, cmdCount{hosts[i].label, hosts[i].count})
	}

	for _, c := range s.spark {
		if c > s.sparkMax {
			s.sparkMax = c
		}
	}
	return s
}

func firstToken(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// statsTitle is the top line shown in place of the search bar in stats mode.
func (m Model) statsTitle(w int) string {
	return clipW(m.th.Title.Render("STATS")+m.th.Dim.Render("  press s to return"), w)
}

// renderStats draws the full stats screen as exactly h lines wide no more than
// w columns: two columns (top commands, per host) above the per-day sparkline.
func (m Model) renderStats(w, h int) string {
	th := m.th
	if m.stats == nil {
		body := []string{"", "  " + th.Dim.Render("computing…")}
		return padLines(body, w, h)
	}
	s := m.stats

	spark := m.sparkBlock(s, w)
	bodyH := h - len(spark)
	if bodyH < 2 {
		bodyH = 2
	}

	colW := (w - 3) / 2
	if colW < 8 {
		colW = 8
	}
	rows := bodyH - 1
	if rows < 1 {
		rows = 1
	}

	top := statColumn(th, "Top commands", s.topCmds, colW, rows)
	hostsCol := statColumn(th, "Per host", s.perHost, colW, rows)
	cols := lipgloss.JoinHorizontal(lipgloss.Top,
		padLines(top, colW, bodyH),
		strings.Repeat(" ", 3),
		padLines(hostsCol, colW, bodyH),
	)

	return padLines(append(strings.Split(cols, "\n"), spark...), w, h)
}

// statColumn renders a titled list column of name/count pairs, each line padded
// to exactly colW columns.
func statColumn(th *theme.Theme, title string, items []cmdCount, colW, rows int) []string {
	lines := []string{th.Title.Render(fitPlain(title, colW))}
	if len(items) == 0 {
		lines = append(lines, th.Dim.Render(fitPlain("  (none)", colW)))
		return lines
	}
	maxN := 0
	for _, it := range items {
		if it.n > maxN {
			maxN = it.n
		}
	}
	for i := 0; i < len(items) && i < rows; i++ {
		lines = append(lines, statLine(th, items[i], maxN, colW))
	}
	return lines
}

// statLine is "name  ▆▆▆▆   count", padded to exactly colW columns. The bar is
// a proportional block gauge; it is dropped when the column is too narrow.
func statLine(th *theme.Theme, it cmdCount, maxN, colW int) string {
	count := strconv.Itoa(it.n)
	cntW := runewidth.StringWidth(count)

	barW := colW / 4
	if colW < 24 {
		barW = 0
	}

	nameW := colW - cntW - 1 - barW
	if barW > 0 {
		nameW-- // gap before the bar
	}
	if nameW < 1 {
		nameW = 1
	}
	name := truncCols(it.name, nameW)

	var b strings.Builder
	b.WriteString(th.Norm.Render(name))
	used := runewidth.StringWidth(name)

	if barW > 0 {
		filled := 0
		if maxN > 0 {
			filled = it.n * barW / maxN
		}
		bar := strings.Repeat("▬", filled) + strings.Repeat(" ", barW-filled)
		b.WriteString(" " + th.Accent.Render(bar))
		used += 1 + barW
	}

	gap := colW - used - cntW
	if gap < 1 {
		gap = 1
	}
	b.WriteString(strings.Repeat(" ", gap) + th.Dim.Render(count))
	return b.String()
}

// sparkBlock renders the per-day histogram section: a title, the block-glyph
// sparkline, and a summary line.
func (m Model) sparkBlock(s *statsData, w int) []string {
	th := m.th
	title := th.Title.Render(fitPlain("Commands per day (last 30d)", w))
	summary := th.Dim.Render(fitPlain(
		fmt.Sprintf("%d commands over %d days · peak %d/day", s.totalRows, s.days, s.sparkMax), w))
	return []string{"", title, renderSparkline(th, s.spark, s.sparkMax, w), summary}
}

func renderSparkline(th *theme.Theme, spark []int, maxN, w int) string {
	if maxN <= 0 {
		return th.Dim.Render(strings.Repeat("·", minInt(len(spark), w)))
	}
	var b strings.Builder
	for i, c := range spark {
		if i >= w {
			break
		}
		if c <= 0 {
			b.WriteString(th.Dim.Render("·"))
			continue
		}
		lvl := (c*8 + maxN - 1) / maxN // ceil into 1..8
		if lvl < 1 {
			lvl = 1
		}
		if lvl > 8 {
			lvl = 8
		}
		b.WriteString(th.Accent.Render(string(sparkBlocks[lvl-1])))
	}
	return b.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
