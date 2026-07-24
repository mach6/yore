package browse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// The two long-arc graphs — the contribution heatmap and the daily trend — are
// aggregated over the widest window they could ever draw and then sliced to fit
// the terminal, so a wide window shows more history rather than more whitespace.
// Both deliberately ignore the selected period: they exist to show the shape of
// activity AROUND the window the other panels summarize, not inside it.
const (
	maxHeatWeeks = 105 // two years of columns — enough to fill a wide terminal
	maxSparkDays = 732 // and of daily bars

	heatLabelW = 4 // the "Sun " gutter; the daily trend indents to match
	heatCellW  = 2 // columns per day cell — one is too thin to read as a square
	minHeatWks = 8 // below this the heatmap is not worth the rows it costs
)

// sparkBlocks are the eight ascending block glyphs used by the histograms
// (level 1..8).
var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// heatShades are the intensity glyphs for the contribution heatmap: index 0 is
// "no activity", 1..4 are increasing shading.
var heatShades = []rune("·░▒▓█")

// statPeriods are the selectable time windows (keys 1..5), shared by every view
// — the browse table, the agent explorer, and the stats screen all filter on the
// same one. days == 0 means all history.
var statPeriods = []struct {
	label string
	days  int
}{
	{"Today", 1},
	{"7d", 7},
	{"30d", 30},
	{"90d", 90},
	{"All", 0},
}

// allPeriod is the index of the "All" tab — the default, so nothing is hidden
// until the user asks for a window.
var allPeriod = len(statPeriods) - 1

// periodCutoff is the instant a period starts, in unix ms (0 = all history).
//
// "Today" is the CALENDAR day in local time, not a rolling 24 hours: the tab
// says today, and a rolling window would fold yesterday evening into this
// morning's hour-of-day buckets — the same clock hour appearing twice, from two
// different days.
func periodCutoff(nowMs int64, days int) int64 {
	switch {
	case days <= 0:
		return 0
	case days == 1:
		n := time.UnixMilli(nowMs)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).UnixMilli()
	default:
		return nowMs - int64(days)*86_400_000
	}
}

// periodDays returns the selected window's length in days (0 = all).
func (m Model) periodDays() int { return statPeriods[m.period].days }

// periodTabs renders the shared "1 Today 2 7d …" strip, the active tab
// highlighted. Every view draws the identical widget, so the keys mean the same
// thing wherever you are.
func (m Model) periodTabs() string {
	th := m.th
	var b strings.Builder
	for i, p := range statPeriods {
		if i > 0 {
			b.WriteByte(' ')
		}
		key := strconv.Itoa(i + 1)
		if i == m.period {
			b.WriteString(th.Accent.Bold(true).Render(key + " " + p.label))
		} else {
			b.WriteString(th.Dim.Render(key + " " + p.label))
		}
	}
	return b.String()
}

// periodTabsWidth is the strip's fixed display width, so callers can reserve
// room for it without rendering it first.
func periodTabsWidth() int {
	w := 0
	for i, p := range statPeriods {
		if i > 0 {
			w++
		}
		w += len(strconv.Itoa(i+1)) + 1 + runewidth.StringWidth(p.label)
	}
	return w
}

// titleWithTabs lays out a view's header: its own text on the left, the shared
// period tabs pinned to the right. Every period-aware view uses this, so the
// filter is always in the same place on screen. On a terminal too narrow for
// both, the title wins — the keys still work and the status bar names the
// active window.
func (m Model) titleWithTabs(left string, w int) string {
	if !m.showPeriodTabs() {
		return clipW(left, w)
	}
	tabs := m.periodTabs()
	pad := w - lipgloss.Width(left) - lipgloss.Width(tabs)
	if pad < 1 {
		return clipW(left, w)
	}
	return left + strings.Repeat(" ", pad) + tabs
}

// cmdCount is a name paired with a tally, used for every ranked list.
type cmdCount struct {
	name string
	n    int
}

// statsData is the pre-computed aggregation behind the stats screen. Every
// field respects the selected period (see computeStats).
type statsData struct {
	// KPI header.
	total     int
	unique    int     // distinct programs (first token)
	successPS float64 // success %, over rows with a known exit; <0 = n/a
	knownExit int     // rows contributing to successPS
	avgDurMs  float64 // over rows with a known duration; <0 = n/a
	agentPS   float64 // % of rows run by an agent (tagged)

	topPrograms []cmdCount // by first token
	topCommands []cmdCount // by full command
	topDirs     []cmdCount // by cwd
	byExecutor  []cmdCount // by tag ("" -> "(you)")
	byHost      []cmdCount // by hostname (cross-machine corpus)

	// Daily trend: per-day counts, oldest (left) .. newest (right), spanning
	// maxSparkDays. Period-independent (see the constants above); the renderer
	// takes the newest slice that fits and scales it to its own peak.
	spark   []int
	hourly  [24]int // counts by hour-of-day (local), within the period
	hourMax int

	// Contribution heatmap: [weekday 0=Sun][week column] counts, newest week on
	// the right. Period-independent, spanning maxHeatWeeks.
	heat [7][maxHeatWeeks]int

	// The aggregation runs over the newest statsLimit rows, not the whole
	// archive. capped records that the sample hit that ceiling and sampleFrom is
	// the oldest row in it — without both, a window wider than the sample's reach
	// looks identical to every other wide window and the tabs read as broken.
	capped     bool
	sampleFrom int64

	periodLabel string
}

// foldHeat places a command's local date into the contribution grid: row =
// weekday (0=Sun), column = weeks ago (0 = oldest held .. maxHeatWeeks-1 =
// current week). Out-of-window dates are ignored. Pure, for unit testing.
func foldHeat(heat *[7][maxHeatWeeks]int, nowMs, startMs int64) {
	now := time.UnixMilli(nowMs)
	d := time.UnixMilli(startMs)
	// Sunday that starts each date's week (local).
	weekStart := func(t time.Time) time.Time {
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		return t.AddDate(0, 0, -int(t.Weekday()))
	}
	weeksAgo := int(weekStart(now).Sub(weekStart(d)).Hours()/24) / 7
	col := (maxHeatWeeks - 1) - weeksAgo
	if col < 0 || col >= maxHeatWeeks {
		return
	}
	heat[int(d.Weekday())][col]++
}

// computeStats aggregates a broad row sample into the stats screen's data,
// restricted to the last periodDays days (0 = all history). It reads only
// fields every record already carries — no schema dependency.
func computeStats(rows []rec.Record, now int64, periodDays int) *statsData {
	s := &statsData{spark: make([]int, maxSparkDays), capped: len(rows) >= statsLimit}
	s.periodLabel = periodLabel(periodDays)
	cutoff := periodCutoff(now, periodDays)

	programs := map[string]int{}
	commands := map[string]int{}
	dirs := map[string]int{}
	execs := map[string]int{}
	hosts := map[string]int{}
	var durSum float64
	var durN int
	var success int

	for _, r := range rows {
		if r.Deleted() {
			continue
		}
		if s.sampleFrom == 0 || r.StartMs < s.sampleFrom {
			s.sampleFrom = r.StartMs
		}
		// The long-arc graphs are folded BEFORE the period cutoff: their job is
		// to show the shape of activity around the selected window, so narrowing
		// the period must not blank them out.
		foldHeat(&s.heat, now, r.StartMs)
		if day := int((now - r.StartMs) / 86_400_000); day >= 0 && day < maxSparkDays {
			s.spark[maxSparkDays-1-day]++
		}
		if r.StartMs < cutoff {
			continue
		}
		s.total++
		if tok := firstToken(r.Cmd); tok != "" {
			programs[tok]++
		}
		if c := strings.TrimSpace(r.Cmd); c != "" {
			commands[c]++
		}
		if r.Cwd != "" {
			dirs[r.Cwd]++
		}
		execs[executorLabel(r.Tag)]++
		if r.Hostname != "" {
			hosts[r.Hostname]++
		}
		if r.Tag != "" {
			s.agentPS++
		}
		if r.Exit != nil {
			s.knownExit++
			if *r.Exit == 0 {
				success++
			}
		}
		if r.DurMs != nil {
			durSum += float64(*r.DurMs)
			durN++
		}
		s.hourly[hourOfDay(r.StartMs)]++
	}

	s.unique = len(programs)
	if s.knownExit > 0 {
		s.successPS = 100 * float64(success) / float64(s.knownExit)
	} else {
		s.successPS = -1
	}
	if durN > 0 {
		s.avgDurMs = durSum / float64(durN)
	} else {
		s.avgDurMs = -1
	}
	if s.total > 0 {
		s.agentPS = 100 * s.agentPS / float64(s.total)
	}

	s.topPrograms = topN(programs)
	s.topCommands = topN(commands)
	s.topDirs = topN(dirs)
	s.byExecutor = topN(execs)
	s.byHost = topN(hosts)

	for _, c := range s.hourly {
		if c > s.hourMax {
			s.hourMax = c
		}
	}
	return s
}

// maxOf returns the largest value in a slice (0 when empty). The width-adaptive
// graphs scale to the peak of what they actually DRAW, not of everything held,
// so a spike outside the visible window can't flatten the bars on screen.
func maxOf(vals []int) int {
	m := 0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// topN returns the highest-count entries from a tally map, most first, ties
// broken by name for stable output.
func topN(m map[string]int) []cmdCount {
	const n = 12
	out := make([]cmdCount, 0, len(m))
	for k, v := range m {
		out = append(out, cmdCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func firstToken(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// executorLabel maps a record's tag to a display label; the empty tag (a
// command the user typed) reads as "(you)".
func executorLabel(tag string) string {
	if tag == "" {
		return "(you)"
	}
	return tag
}

func hourOfDay(ms int64) int { return time.UnixMilli(ms).Hour() }

func periodLabel(days int) string {
	for _, p := range statPeriods {
		if p.days == days {
			return p.label
		}
	}
	return "All"
}

// statsTitle is the top line shown in place of the search bar in stats mode: a
// title and how far the sample reaches, with the period tabs at the right.
func (m Model) statsTitle(w int) string {
	th := m.th
	left := th.Title.Render("STATS")
	if m.stats != nil {
		left += th.Dim.Render("  " + m.sampleNote(m.stats))
	}
	return m.titleWithTabs(left, w)
}

// sampleNote says what the aggregation actually covers. When the newest
// statsLimit rows do not reach back as far as the selected window, every wider
// tab shows the same numbers — so say so rather than let the tabs look inert.
func (m Model) sampleNote(s *statsData) string {
	if !s.capped || s.sampleFrom == 0 {
		return "all history"
	}
	reach := theme.RelTime(m.now(), s.sampleFrom)
	if d := m.periodDays(); d == 0 || int64(d)*86_400_000 > m.now()-s.sampleFrom {
		return fmt.Sprintf("newest %d commands only — reaches back %s", statsLimit, reach)
	}
	return fmt.Sprintf("newest %d commands · reaches back %s", statsLimit, reach)
}

// renderStats draws the full stats screen in exactly h lines, no wider than w:
// a KPI header, up to four ranked columns, then the daily + hourly histograms.
func (m Model) renderStats(w, h int) string {
	th := m.th
	if m.stats == nil {
		return padLines([]string{"", "  " + th.Dim.Render("computing…")}, w, h)
	}
	s := m.stats

	out := make([]string, 0, h)
	out = append(out, m.kpiHeader(s, w), "")

	// The heatmap + histograms occupy the bottom; columns fill what remains above
	// them. The heatmap (9 lines) is shown only when the pane is tall enough to
	// still leave room for the ranked columns.
	bottom := m.histograms(s, w)
	if h >= 26 {
		bottom = append(m.renderHeatmap(s, w), bottom...)
	}
	bodyH := h - len(out) - len(bottom)
	if bodyH < 3 {
		bodyH = 3
	}

	out = append(out, m.statColumns(s, w, bodyH)...)
	out = append(out, bottom...)
	return padLines(out, w, h)
}

// kpiHeader is the single summary line: totals, success rate, avg duration,
// agent share.
func (m Model) kpiHeader(s *statsData, w int) string {
	th := m.th
	dur := "n/a"
	if s.avgDurMs >= 0 {
		dur = theme.Duration(int64(s.avgDurMs))
	}
	seg := func(label, val string) string {
		return th.Dim.Render(label+" ") + th.Norm.Render(val)
	}
	// Success rate is color-coded: green ≥90%, amber ≥70%, else red.
	successSeg := th.Dim.Render("Success ") + th.Norm.Render("n/a")
	if s.successPS >= 0 {
		style := th.ExitErr
		switch {
		case s.successPS >= 90:
			style = th.ExitOK
		case s.successPS >= 70:
			style = th.Match
		}
		successSeg = th.Dim.Render("Success ") + style.Render(fmt.Sprintf("%.0f%%", s.successPS))
	}
	parts := []string{
		seg("Total", strconv.Itoa(s.total)),
		seg("Unique", strconv.Itoa(s.unique)),
		successSeg,
		seg("Avg", dur),
		seg("Agent", fmt.Sprintf("%.0f%%", s.agentPS)),
	}
	return clipW("  "+strings.Join(parts, th.Dim.Render("  ·  ")), w)
}

// statColumns lays out the ranked lists side by side, dropping columns (from the
// right) until they fit the width. Programs and commands are the priority.
func (m Model) statColumns(s *statsData, w, bodyH int) []string {
	type col struct {
		title string
		items []cmdCount
	}
	all := []col{
		{"Top programs", s.topPrograms},
		{"Top commands", s.topCommands},
		{"Top directories", s.topDirs},
		{"By executor", s.byExecutor},
		{"By host", s.byHost},
	}

	const gap, minCol = 2, 16
	n := len(all)
	for n > 1 && (w-(n-1)*gap)/n < minCol {
		n--
	}
	colW := (w - (n-1)*gap) / n
	if colW < 8 {
		colW = 8
	}
	rows := bodyH - 1
	if rows < 1 {
		rows = 1
	}

	rendered := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines := statColumn(m.th, all[i].title, all[i].items, colW, rows)
		rendered = append(rendered, padLines(lines, colW, bodyH))
	}
	joined := make([]string, 0, 2*n-1)
	for i, r := range rendered {
		if i > 0 {
			joined = append(joined, strings.Repeat(" ", gap))
		}
		joined = append(joined, r)
	}
	return strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, joined...), "\n")
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

// statLine is "name  ▬▬▬▬   count", padded to exactly colW columns. The bar is a
// proportional gauge; it is dropped when the column is too narrow.
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

// histograms renders the daily trend and the hourly distribution. Both span the
// full width: the daily trend by showing as many days as there are columns, the
// hourly by widening each of its fixed 24 buckets to fill them.
func (m Model) histograms(s *statsData, w int) []string {
	th := m.th
	pad := strings.Repeat(" ", heatLabelW) // align both charts under the heatmap
	inner := w - heatLabelW
	if inner < 8 {
		pad, inner = "", w
	}

	// Daily trend: one column per day, newest at the right, as far back as the
	// terminal is wide.
	days := inner
	if days > maxSparkDays {
		days = maxSparkDays
	}
	if days < 1 {
		days = 1
	}
	spark := s.spark[len(s.spark)-days:]
	sparkMax := maxOf(spark)
	total := 0
	for _, c := range spark {
		total += c
	}

	return []string{
		"",
		th.Title.Render(fitPlain(fmt.Sprintf("Commands per day (last %d days)", days), w)),
		pad + renderBars(th, spark, sparkMax, inner),
		th.Dim.Render(fitPlain(
			fmt.Sprintf("%s%d commands over the window · peak %d/day", pad, total, sparkMax), w)),
		"",
		th.Title.Render(fitPlain(
			"By hour of day · "+plural(s.total, "command")+" in "+s.periodLabel, w)),
		pad + hourBars(th, s.hourly, s.hourMax, inner, m.hoursElapsed()),
		pad + th.Dim.Render(hourAxis(inner)),
	}
}

// hoursElapsed is how many of the day's 24 hours the selected period can have
// commands in. Only "Today" is partial — the day is still running, so the hours
// that have not happened yet are drawn blank rather than as zero. Every wider
// window covers whole days, so all 24 are live.
func (m Model) hoursElapsed() int {
	if m.periodDays() != 1 {
		return 24
	}
	return hourOfDay(m.now()) + 1
}

// hourBars draws the hour-of-day histogram. Hours past `live` render blank: on a
// partial day that is the difference between "nothing ran at 11pm" and "it isn't
// 11pm yet", which a row of zero-dots cannot say.
func hourBars(th *theme.Theme, hourly [24]int, maxN, w, live int) string {
	var b strings.Builder
	for h := 0; h < 24; h++ {
		cw := bucketStart(w, 24, h+1) - bucketStart(w, 24, h)
		if cw < 1 {
			break
		}
		switch {
		case h >= live:
			b.WriteString(strings.Repeat(" ", cw))
		case maxN <= 0 || hourly[h] <= 0:
			b.WriteString(th.Dim.Render(strings.Repeat("·", cw)))
		default:
			b.WriteString(th.Accent.Render(strings.Repeat(string(sparkBlocks[sparkLevel(hourly[h], maxN)]), cw)))
		}
	}
	return b.String()
}

// hourAxis labels the hourly histogram, each label sitting at the left edge of
// its own bucket. The labelling step widens until a label fits, and drops out
// entirely on a terminal too narrow for even one.
func hourAxis(w int) string {
	if w < 2 {
		return strings.Repeat(" ", maxInt(0, w))
	}
	// A label is two columns and needs a blank after it, so a step is only usable
	// when that many buckets span at least three columns.
	step := 3
	for step < 24 && bucketStart(w, 24, step) < 3 {
		step *= 2
	}
	if bucketStart(w, 24, step) < 3 {
		return strings.Repeat(" ", w)
	}
	var b strings.Builder
	for h := 0; h < 24; h += step {
		want := bucketStart(w, 24, h)
		if pad := want - b.Len(); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		fmt.Fprintf(&b, "%02d", h)
	}
	return fitPlain(b.String(), w)
}

// heatLevel buckets a count into 0..4 by its fraction of the period's peak day.
func heatLevel(cnt, peak int) int {
	if cnt <= 0 || peak <= 0 {
		return 0
	}
	switch {
	case cnt*4 <= peak:
		return 1
	case cnt*2 <= peak:
		return 2
	case cnt*4 <= peak*3:
		return 3
	default:
		return 4
	}
}

// heatWeeks is how many week-columns the heatmap can draw at width w: as much
// history as fits, capped at the year the aggregation holds.
func heatWeeks(w int) int {
	n := (w - heatLabelW) / heatCellW
	if n > maxHeatWeeks {
		n = maxHeatWeeks
	}
	return n
}

// renderHeatmap draws the contribution calendar: 7 weekday rows × as many week
// columns as the terminal affords, each cell shaded by activity intensity (a
// GitHub-style graph) and heatCellW columns wide so it reads as a square. A
// month ruler runs underneath — without it a year of unlabelled columns says
// nothing about when.
func (m Model) renderHeatmap(s *statsData, w int) []string {
	th := m.th
	weeks := heatWeeks(w)
	if weeks < minHeatWks {
		return nil
	}
	first := maxHeatWeeks - weeks // leftmost column drawn, into s.heat

	peak := 0
	for row := range s.heat {
		if v := maxOf(s.heat[row][first:]); v > peak {
			peak = v
		}
	}

	labels := [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	lines := []string{
		"",
		th.Title.Render(fitPlain(
			fmt.Sprintf("Activity (last %d weeks) · peak %d/day", weeks, peak), w)),
	}
	for row := 0; row < 7; row++ {
		var b strings.Builder
		b.WriteString(th.Dim.Render(padRight(labels[row], heatLabelW)))
		for col := first; col < maxHeatWeeks; col++ {
			lvl := heatLevel(s.heat[row][col], peak)
			cell := strings.Repeat(string(heatShades[lvl]), heatCellW)
			if lvl == 0 {
				b.WriteString(th.Dim.Render(cell))
			} else {
				b.WriteString(th.Accent.Render(cell))
			}
		}
		lines = append(lines, clipW(b.String(), w))
	}
	return append(lines, th.Dim.Render(monthRuler(m.now(), weeks, w)))
}

// monthRuler labels the heatmap's columns with a month abbreviation wherever the
// month changes, leaving the rest blank — the same trick a calendar heatmap uses
// to stay legible without a label per column.
func monthRuler(nowMs int64, weeks, w int) string {
	// The Sunday that starts the current week; column c sits (weeks-1-c) weeks
	// before it.
	n := time.UnixMilli(nowMs)
	cur := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).
		AddDate(0, 0, -int(n.Weekday()))

	b := strings.Builder{}
	b.WriteString(strings.Repeat(" ", heatLabelW))
	last := time.Month(0)
	for c := 0; c < weeks; c++ {
		want := heatLabelW + c*heatCellW
		mon := cur.AddDate(0, 0, -7*(weeks-1-c)).Month()
		if mon == last || b.Len() > want {
			continue // same month, or a prior label still running past this column
		}
		last = mon
		if pad := want - b.Len(); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(mon.String()[:3])
	}
	return fitPlain(b.String(), w)
}

// bucketStart is the first column of bucket i when w columns are spread across
// n buckets — the leftmost w%n buckets take one extra column each, so the row
// ends flush with the right edge instead of leaving a ragged remainder. The
// histogram and its axis both position from here, so labels cannot drift off
// their bars.
func bucketStart(w, n, i int) int {
	if n < 1 {
		return 0
	}
	return i*(w/n) + minInt(i, w%n)
}

// renderBars draws a block-glyph histogram of vals across exactly w columns. A
// zero value is a dim dot; others scale into the 8 block levels relative to
// maxN. With more buckets than columns the tail is dropped rather than squeezed.
func renderBars(th *theme.Theme, vals []int, maxN, w int) string {
	n := len(vals)
	if n < 1 || w < 1 {
		return ""
	}
	var b strings.Builder
	for i, cnt := range vals {
		cw := bucketStart(w, n, i+1) - bucketStart(w, n, i)
		if cw < 1 {
			break
		}
		if maxN <= 0 || cnt <= 0 {
			b.WriteString(th.Dim.Render(strings.Repeat("·", cw)))
			continue
		}
		b.WriteString(th.Accent.Render(strings.Repeat(string(sparkBlocks[sparkLevel(cnt, maxN)]), cw)))
	}
	return b.String()
}

// sparkLevel buckets a count into an index into sparkBlocks (0..7), relative to
// the peak of what is being drawn.
func sparkLevel(cnt, maxN int) int {
	if maxN <= 0 {
		return 0
	}
	lvl := (cnt*8 + maxN - 1) / maxN // ceil into 1..8
	if lvl < 1 {
		lvl = 1
	}
	if lvl > 8 {
		lvl = 8
	}
	return lvl - 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
