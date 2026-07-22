package browse

import (
	"io"
	"os"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// Backend is how the browser talks to the daemon. *daemon.Client satisfies it;
// tests substitute a fake. The browser never imports daemon.
type Backend interface {
	Query(proto.QueryReq) (proto.QueryResp, error)
	Hosts() (proto.HostsInfo, error)
	Delete(id string) error
	Devices() (proto.DevicesInfo, error)
	Approve(id string) error
	Revoke(id string) error
	Sync() error
}

// Options configures a browse session.
type Options struct {
	Version string
	Session string // current shell session id ($YORE_SESSION)
	Cwd     string // current directory
	Now     int64  // injectable clock in unix ms; 0 => time.Now
	Keymap  string // "vim" enables vi-style navigation; "" / "emacs" = default
}

// Tunables.
const (
	queryLimit = 1000 // rows requested for the browse table
	statsLimit = 5000 // rows requested for the stats aggregation
	leftWidth  = 24   // host-sidebar outer width (incl. border)
	flashMs    = 1500 // how long the copied/deleted flash lingers
)

// focus identifies which of the three browse panes owns navigation keys.
type focus int

const (
	focusHosts focus = iota
	focusTable
	focusDetail
)

// viewMode switches between the browse panes and the stats screen.
type viewMode int

const (
	viewBrowse viewMode = iota
	viewStats
	viewDevices
	viewAgents
	viewPrompts
)

// hostItem is one row of the host sidebar. The first real host reported by the
// daemon is the local one (proto guarantees local-first ordering), so it maps
// to ScopeLocal; the aggregate row maps to ScopeAll and every other host to
// ScopeHost.
type hostItem struct {
	label string
	count int
	scope string
	host  string // hostname for ScopeHost; empty otherwise
}

// --- messages -----------------------------------------------------------

type initMsg struct{}

type queryResultMsg struct {
	seq  uint64
	resp proto.QueryResp
	err  error
}

type hostsResultMsg struct {
	info proto.HostsInfo
	err  error
}

type statsResultMsg struct {
	seq  uint64
	resp proto.QueryResp
	err  error
}

type flashExpireMsg struct{ id int }

// hostsTickMsg fires the bounded init-time warm loop (see onHostsTick).
type hostsTickMsg struct{}

// syncDoneMsg carries the result of an on-demand "sync now" (the S key).
type syncDoneMsg struct{ err error }

// Model is the Bubble Tea model backing the browser. Exported so tests can
// drive Update directly.
type Model struct {
	b    Backend
	opts Options
	th   *theme.Theme
	keys keyMap

	// child components
	ti     textinput.Model
	detail viewport.Model
	help   help.Model

	// pre-built (once) box styles for focused / blurred panes
	borderFocus lipgloss.Style
	borderBlur  lipgloss.Style

	// data
	hosts     []hostItem
	hostSel   int
	rows      []rec.Record
	total     int
	remote    proto.RemoteInfo
	lastErr   error
	gotResult bool
	hasTags   bool // any current row carries a Tag (gates the tag column)

	// table window
	sel int
	top int

	// stats
	stats        *statsData
	agents       *agentsData  // agent-monitor aggregation, from the same sample
	prompts      *promptsData // prompt-explorer aggregation, from the same sample
	promptSel    int          // selected row in the prompt-explorer table
	promptDrill  bool         // drilled into the selected prompt's command list
	drillSel     int          // selected row within the drilled command list
	statsRows    []rec.Record // the full sample; re-aggregated when the period changes
	statsPeriod  int          // index into statPeriods
	statsErr     error
	gotStats     bool
	statsSeq     uint64
	appliedStats uint64

	// interaction state
	view           viewMode
	focus          focus
	vim            bool // vi-style navigation (from Options.Keymap == "vim")
	searching      bool
	confirmDelete  bool
	showHelp       bool
	executorFilter string // active executor-tag filter (the t key); "" = no filter
	flash          string
	flashID        int
	quitting       bool
	accepted       string // command the user chose with enter; read by Run on exit

	// devices pane
	devices    []proto.DeviceInfo
	devSel     int
	devErr     error
	gotDevices bool
	devConfirm string // pending "revoke <id>" awaiting y/n; "" = none

	// query sequencing
	seq        uint64
	appliedSeq uint64

	// remote-cache convergence: hostsRemote is the remote host count the sidebar
	// currently reflects; a query result reporting a different count triggers a
	// host refetch. hostsTicks/ticking drive a BOUNDED init-time warm loop that
	// re-fetches while the cache is still warming, so a cold daemon converges
	// without a keystroke. Both are capped so they can never spin forever.
	hostsRemote int
	hostsTicks  int
	ticking     bool

	// geometry
	width, height int
	leftW         int // host sidebar outer width (responsive)
	tableRows     int // visible data rows in the table pane
	tableWidth    int // right-pane content width
	midHeight     int
	tableOuterH   int
	detailOuterH  int

	// out is where OSC 52 copy sequences are written (the tty). Run sets it;
	// it stays nil under test so copying is a silent no-op.
	out io.Writer
}

// NewModel builds the browser Model, constructing the Theme exactly once.
func NewModel(b Backend, opts Options) Model {
	th := theme.New()
	vim := opts.Keymap == "vim"

	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "type to filter…"
	ti.TextStyle = th.Input
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Cursor.Style = th.Accent

	h := help.New()
	h.Styles.ShortKey = th.Accent
	h.Styles.ShortDesc = th.Dim
	h.Styles.ShortSeparator = th.Dim
	h.Styles.FullKey = th.Accent
	h.Styles.FullDesc = th.Dim
	h.Styles.FullSeparator = th.Dim
	h.Styles.Ellipsis = th.Dim

	vp := viewport.New(0, 0)

	m := Model{
		b:      b,
		opts:   opts,
		th:     th,
		keys:   defaultKeyMap(vim),
		vim:    vim,
		ti:     ti,
		detail: vp,
		help:   h,
		hosts:  []hostItem{{label: "All hosts", scope: proto.ScopeAll}},
		// Stats open on the widest window; keys 1..4 narrow it.
		statsPeriod: len(statPeriods) - 1,
		focus:       focusTable,
		width:       80,
		height:      24,
		borderFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Accent.GetForeground()),
		borderBlur: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Border.GetBorderTopForeground()),
	}
	m.applyLayout()
	return m
}

// Init kicks off the first host and query fetches without blocking the first
// paint.
func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return initMsg{} }
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case initMsg:
		mm, qcmd := m.issueQuery()
		mm.ticking = true // start the single bounded warm-loop chain
		return mm, tea.Batch(qcmd, mm.hostsCmd(), hostsTick())

	case hostsTickMsg:
		return m.onHostsTick()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.applyLayout()
		m.syncDetail()
		return m, nil

	case queryResultMsg:
		return m.applyResult(msg)

	case hostsResultMsg:
		return m.applyHosts(msg)

	case statsResultMsg:
		return m.applyStats(msg)

	case syncDoneMsg:
		return m.applySync(msg)

	case flashExpireMsg:
		if msg.id == m.flashID {
			m.flash = ""
		}
		return m, nil

	case devicesResultMsg:
		m.gotDevices = true
		m.devErr = msg.err
		m.devices = msg.info.Devices
		if m.devSel >= len(m.devices) {
			m.devSel = 0
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// --- result application -------------------------------------------------

func (m Model) applyResult(msg queryResultMsg) (tea.Model, tea.Cmd) {
	if msg.seq <= m.appliedSeq {
		return m, nil // stale / out-of-order
	}
	m.appliedSeq = msg.seq
	m.gotResult = true
	if msg.err != nil {
		m.lastErr = msg.err // keep the last good rows visible
		return m, nil
	}
	m.lastErr = nil
	// Preserve the cursor across a refresh by record identity: a background
	// warm-loop / remote-cache refetch delivers the SAME result set, so keep the
	// cursor on the command it was on rather than yanking it to row 0. A genuine
	// new search/filter (the selected record is gone) still resets to the top.
	var selID string
	if m.sel >= 0 && m.sel < len(m.rows) {
		selID = m.rows[m.sel].ID
	}
	rows := append([]rec.Record(nil), msg.resp.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartMs > rows[j].StartMs })
	m.rows = rows
	m.total = msg.resp.Total
	m.remote = msg.resp.Remote
	m.hasTags = false
	for _, r := range m.rows {
		if len(r.Tags) > 0 {
			m.hasTags = true
			break
		}
	}
	m.sel = 0
	if selID != "" {
		for i, r := range m.rows {
			if r.ID == selID {
				m.sel = i
				break
			}
		}
	}
	m.top = 0
	m.clampWindow()
	m.syncDetail()
	// If this response reports a remote host count the sidebar hasn't caught up
	// to (e.g. a deep pull just warmed the cache), refetch the host list. This
	// converges: applyHosts sets hostsRemote to match, so once the sidebar
	// reflects the current count no further refetch fires.
	if msg.resp.Remote.Hosts != m.hostsRemote {
		return m, m.hostsCmd()
	}
	return m, nil
}

func (m Model) applyHosts(msg hostsResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, nil // keep the aggregate-only sidebar
	}
	items := []hostItem{{label: "All hosts", scope: proto.ScopeAll}}
	total := 0
	for i, h := range msg.info.Hosts {
		it := hostItem{label: h.Hostname, count: h.Count, scope: proto.ScopeHost, host: h.Hostname}
		if i == 0 { // proto guarantees the local host is listed first
			it.scope = proto.ScopeLocal
			it.host = ""
		}
		items = append(items, it)
		total += h.Count
	}
	items[0].count = total
	m.hosts = items
	if m.hostSel >= len(m.hosts) {
		m.hostSel = len(m.hosts) - 1
	}
	m.hostsRemote = msg.info.Remote.Hosts
	// Stats' per-host list and totals derive from the sidebar, so a changed host
	// list needs a fresh aggregation to match.
	if m.view == viewStats {
		m.statsSeq++
		return m, m.statsCmd(m.statsSeq)
	}
	return m, nil
}

func (m Model) applyStats(msg statsResultMsg) (tea.Model, tea.Cmd) {
	if msg.seq <= m.appliedStats {
		return m, nil
	}
	m.appliedStats = msg.seq
	m.gotStats = true
	if msg.err != nil {
		m.statsErr = msg.err
		return m, nil
	}
	m.statsErr = nil
	m.statsRows = msg.resp.Rows
	m.recomputeStats()
	return m, nil
}

// --- commands -----------------------------------------------------------

func (m Model) issueQuery() (Model, tea.Cmd) {
	m.seq++
	return m, m.queryCmd(m.seq, m.buildReq())
}

func (m Model) queryCmd(seq uint64, req proto.QueryReq) tea.Cmd {
	b := m.b
	return func() tea.Msg {
		resp, err := b.Query(req)
		return queryResultMsg{seq: seq, resp: resp, err: err}
	}
}

func (m Model) hostsCmd() tea.Cmd {
	b := m.b
	return func() tea.Msg {
		info, err := b.Hosts()
		return hostsResultMsg{info: info, err: err}
	}
}

// syncNow forces an immediate push/pull (the S key) and flashes progress; the
// result arrives as a syncDoneMsg. b.Sync() blocks until the daemon's cycle
// finishes, so "syncing…" shows for the real duration.
func (m Model) syncNow() (tea.Model, tea.Cmd) {
	m.flash = "syncing…" // no expiry tick: replaced by applySync when the cycle ends
	m.flashID++
	return m, m.syncCmd()
}

func (m Model) syncCmd() tea.Cmd {
	b := m.b
	return func() tea.Msg { return syncDoneMsg{err: b.Sync()} }
}

// applySync flashes the outcome and, on success, re-fetches the table, host
// sidebar, and (in stats view) the aggregation so just-synced data shows at once
// instead of waiting for the periodic cycle.
func (m Model) applySync(msg syncDoneMsg) (tea.Model, tea.Cmd) {
	m.flashID++
	if msg.err != nil {
		m.flash = "sync failed"
		return m, flashTick(m.flashID)
	}
	m.flash = "✓ synced"
	mm, qcmd := m.issueQuery()
	cmds := []tea.Cmd{flashTick(mm.flashID), qcmd, mm.hostsCmd()}
	if mm.view == viewStats {
		mm.statsSeq++
		cmds = append(cmds, mm.statsCmd(mm.statsSeq))
	}
	return mm, tea.Batch(cmds...)
}

func (m Model) statsCmd(seq uint64) tea.Cmd {
	b := m.b
	return func() tea.Msg {
		resp, err := b.Query(proto.QueryReq{Scope: proto.ScopeAll, Limit: statsLimit})
		return statsResultMsg{seq: seq, resp: resp, err: err}
	}
}

func (m Model) buildReq() proto.QueryReq {
	it := m.hosts[m.hostSel]
	req := proto.QueryReq{
		Q:        m.ti.Value(),
		Scope:    it.scope,
		Host:     it.host,
		Executor: m.executorFilter,
		Limit:    queryLimit,
		Dedupe:   false, // browse shows the real timeline, newest first
	}
	return req
}

// --- key handling -------------------------------------------------------

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()

	// Delete confirmation swallows all other input.
	if m.confirmDelete {
		switch s {
		case "y", "Y":
			return m.doDelete()
		case "n", "N", "esc", "ctrl+c":
			m.confirmDelete = false
		}
		return m, nil
	}

	// Search input focused: only esc/enter/ctrl+c are special; the rest edits.
	if m.searching {
		switch s {
		case "esc", "enter":
			m.searching = false
			m.ti.Blur()
			if m.focus == focusHosts {
				m.focus = focusTable
			}
			return m, nil
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
		prev := m.ti.Value()
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		if m.ti.Value() != prev {
			var q tea.Cmd
			m, q = m.issueQuery()
			return m, tea.Batch(cmd, q)
		}
		return m, cmd
	}

	// Devices view swallows its own keys.
	if m.view == viewDevices {
		return m.handleDevicesKey(s)
	}

	// Global keys (both views).
	switch s {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
		m.help.ShowAll = m.showHelp
		m.applyLayout()
		m.syncDetail()
		return m, nil
	case "s":
		return m.toggleStats()
	case "a":
		return m.toggleAgents()
	case "p":
		return m.togglePrompts()
	case "S":
		return m.syncNow()
	case "D":
		m.view = viewDevices
		m.devConfirm = ""
		return m, m.devicesCmd()
	}

	// The prompt explorer is interactive (selection + drill-down), so it owns its
	// keys rather than sharing the simple stats/agents handler.
	if m.view == viewPrompts {
		return m.handlePromptsKey(s)
	}

	if m.view == viewStats || m.view == viewAgents {
		switch s {
		case "esc":
			m.view = viewBrowse
		case "1", "2", "3", "4", "5":
			// Period tabs: re-aggregate the held sample without a new query.
			if p := int(s[0] - '1'); p < len(statPeriods) {
				m.statsPeriod = p
				m.recomputeStats()
			}
		}
		return m, nil
	}

	// Browse-view keys.
	switch s {
	case "/":
		m.searching = true
		m.ti.Focus()
		return m, nil
	case "t":
		return m.toggleExecutorFilter()
	case "tab":
		m.cycleFocus(1)
		return m, nil
	case "shift+tab":
		m.cycleFocus(-1)
		return m, nil
	}

	// Vim: h/l (and left/right) move focus between panes.
	if m.vim {
		switch s {
		case "h", "left":
			m.cycleFocus(-1)
			return m, nil
		case "l", "right":
			m.cycleFocus(1)
			return m, nil
		}
	}

	switch m.focus {
	case focusHosts:
		switch s {
		case "up", "k":
			return m.moveHost(-1)
		case "down", "j":
			return m.moveHost(1)
		}
	case focusTable:
		switch s {
		case "up", "k":
			m.moveSel(-1)
			m.syncDetail()
		case "down", "j":
			m.moveSel(1)
			m.syncDetail()
		case "pgup":
			m.moveSel(-m.tableRows)
			m.syncDetail()
		case "pgdown":
			m.moveSel(m.tableRows)
			m.syncDetail()
		case "ctrl+u":
			// vim half-page up; a no-op in emacs mode (ctrl+u is unbound there).
			if m.vim {
				m.moveSel(-m.halfPage())
				m.syncDetail()
			}
		case "ctrl+d":
			// vim: half-page down. emacs: delete alias (unchanged).
			if m.vim {
				m.moveSel(m.halfPage())
				m.syncDetail()
			} else if len(m.rows) > 0 {
				m.confirmDelete = true
			}
		case "g", "home":
			m.sel, m.top = 0, 0
			m.syncDetail()
		case "G", "end":
			if len(m.rows) > 0 {
				m.sel = len(m.rows) - 1
			}
			m.clampWindow()
			m.syncDetail()
		case "enter":
			// Hand the picked command back to the shell (recall-to-prompt);
			// Run returns it and the `h` function drops it on the next prompt.
			return m.acceptSelected()
		case "y":
			return m.copySelected()
		case "d":
			// Delete stays a single-key action (with y/n confirm) in both
			// keymaps; vim's "dd" is intentionally NOT implemented.
			if len(m.rows) > 0 {
				m.confirmDelete = true
			}
		}
	case focusDetail:
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
	return m, nil
}

// toggleExecutorFilter flips the executor-tag filter (the t key). With a filter
// active it clears it; otherwise it adopts the selected row's Tag if it has one
// (flashing "no tag" and doing nothing when it doesn't). Either change re-issues
// the query and flashes the new state.
func (m Model) toggleExecutorFilter() (tea.Model, tea.Cmd) {
	if m.executorFilter != "" {
		m.executorFilter = ""
		m.flash = "executor filter cleared"
	} else {
		r, ok := m.selected()
		if !ok || r.Tag == "" {
			m.flash = "no tag"
			m.flashID++
			return m, flashTick(m.flashID)
		}
		m.executorFilter = r.Tag
		m.flash = "executor: " + r.Tag
	}
	m.flashID++
	mm, qcmd := m.issueQuery()
	return mm, tea.Batch(qcmd, flashTick(mm.flashID))
}

// recomputeStats re-derives the stats and agent aggregates from the held sample
// for the current period. Cheap; called on load and on a period-tab change.
func (m *Model) recomputeStats() {
	if m.statsRows == nil {
		return
	}
	days := statPeriods[m.statsPeriod].days
	m.stats = computeStats(m.statsRows, m.now(), days)
	m.agents = computeAgents(m.statsRows, m.now(), days)
	m.prompts = computePrompts(m.statsRows, m.now(), days)
	m.clampPrompts()
}

// toggleAgents opens the agent-monitor view (or returns to browse), reusing the
// stats sample query so the aggregation has data.
func (m Model) toggleAgents() (tea.Model, tea.Cmd) {
	if m.view == viewAgents {
		m.view = viewBrowse
		return m, nil
	}
	m.view = viewAgents
	if m.statsRows != nil {
		m.recomputeStats()
		return m, nil
	}
	m.statsSeq++
	return m, m.statsCmd(m.statsSeq)
}

// togglePrompts opens the prompt-explorer view (or returns to browse), reusing
// the stats sample query. Opening always starts at the top of the prompt list,
// not drilled into a stale prompt.
func (m Model) togglePrompts() (tea.Model, tea.Cmd) {
	if m.view == viewPrompts {
		m.view = viewBrowse
		return m, nil
	}
	m.view = viewPrompts
	m.promptDrill = false
	m.promptSel = 0
	m.drillSel = 0
	if m.statsRows != nil {
		m.recomputeStats()
		return m, nil
	}
	m.statsSeq++
	return m, m.statsCmd(m.statsSeq)
}

// handlePromptsKey services the prompt-explorer view: selection, drill-in
// (Enter) and drill-out (Esc), plus the shared period tabs. The global keys
// (p/s/a/q/S/D/?) are handled before this in handleKey, so they still work here.
func (m Model) handlePromptsKey(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc":
		if m.promptDrill {
			m.promptDrill = false // drill-out: back to the prompt list
			return m, nil
		}
		m.view = viewBrowse
		return m, nil
	case "1", "2", "3", "4", "5":
		if p := int(s[0] - '1'); p < len(statPeriods) {
			m.statsPeriod = p
			m.promptDrill = false // the prompt set changes; leave the drill
			m.recomputeStats()
		}
		return m, nil
	case "up", "k":
		if m.promptDrill {
			m.drillSel = clampIndex(m.drillSel-1, m.drillLen())
		} else {
			m.promptSel = clampIndex(m.promptSel-1, m.promptLen())
		}
		return m, nil
	case "down", "j":
		if m.promptDrill {
			m.drillSel = clampIndex(m.drillSel+1, m.drillLen())
		} else {
			m.promptSel = clampIndex(m.promptSel+1, m.promptLen())
		}
		return m, nil
	case "g", "home":
		if m.promptDrill {
			m.drillSel = 0
		} else {
			m.promptSel = 0
		}
		return m, nil
	case "G", "end":
		if m.promptDrill {
			m.drillSel = clampIndex(m.drillLen()-1, m.drillLen())
		} else {
			m.promptSel = clampIndex(m.promptLen()-1, m.promptLen())
		}
		return m, nil
	case "enter":
		if !m.promptDrill && m.promptLen() > 0 {
			m.promptDrill = true
			m.drillSel = 0
		}
		return m, nil
	}
	return m, nil
}

// promptLen / drillLen are the row counts of the two prompt-explorer lists.
func (m Model) promptLen() int {
	if m.prompts == nil {
		return 0
	}
	return len(m.prompts.prompts)
}

func (m Model) drillLen() int {
	p, ok := m.drilledPrompt()
	if !ok {
		return 0
	}
	return len(p.cmds)
}

// clampPrompts keeps the prompt and drill selections in range after the sample
// (and thus the prompt set) is recomputed.
func (m *Model) clampPrompts() {
	m.promptSel = clampIndex(m.promptSel, m.promptLen())
	if m.promptDrill {
		m.drillSel = clampIndex(m.drillSel, m.drillLen())
	}
}

// clampIndex bounds i to [0, n-1], returning 0 when the list is empty.
func clampIndex(i, n int) int {
	if i >= n {
		i = n - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

func (m Model) toggleStats() (tea.Model, tea.Cmd) {
	if m.view == viewStats {
		m.view = viewBrowse
		return m, nil
	}
	m.view = viewStats
	m.statsSeq++
	return m, m.statsCmd(m.statsSeq)
}

func (m *Model) cycleFocus(d int) {
	m.focus = focus((int(m.focus) + d + 3) % 3)
}

func (m Model) moveHost(d int) (tea.Model, tea.Cmd) {
	n := len(m.hosts)
	if n == 0 {
		return m, nil
	}
	m.hostSel += d
	if m.hostSel < 0 {
		m.hostSel = 0
	}
	if m.hostSel >= n {
		m.hostSel = n - 1
	}
	return m.issueQuery()
}

// acceptSelected records the highlighted command and quits so Run can return
// it: this is the recall-to-prompt path (Enter), mirroring the Ctrl-R search.
func (m Model) acceptSelected() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.accepted = r.Cmd
	m.quitting = true
	return m, tea.Quit
}

// copySelected copies the highlighted command to the clipboard (the `y` path).
// It emits OSC 52 to the tty (tmux-wrapped when inside tmux) so the copy works
// over SSH, AND best-effort pipes to a local clipboard tool, since many
// terminals silently ignore OSC 52. Both are fire-and-forget in a tea.Cmd, so
// neither blocks the UI.
func (m Model) copySelected() (tea.Model, tea.Cmd) {
	cmd, ok := m.selected()
	if !ok {
		return m, nil
	}
	text := cmd.Cmd
	m.flash = "✓ copied"
	m.flashID++
	id := m.flashID
	out := m.out
	tmux := os.Getenv("TMUX") != ""
	return m, tea.Batch(
		func() tea.Msg {
			if out != nil {
				_, _ = io.WriteString(out, osc52Seq(text, tmux))
			}
			localClipboardCopy(text)
			return nil
		},
		flashTick(id),
	)
}

func (m Model) doDelete() (tea.Model, tea.Cmd) {
	m.confirmDelete = false
	if len(m.rows) == 0 {
		return m, nil
	}
	id := m.rows[m.sel].ID
	if err := m.b.Delete(id); err != nil {
		m.flash = "delete failed"
		m.lastErr = err
		m.flashID++
		return m, flashTick(m.flashID)
	}
	m.rows = append(m.rows[:m.sel], m.rows[m.sel+1:]...)
	if m.total > 0 {
		m.total--
	}
	if m.sel >= len(m.rows) {
		m.sel = len(m.rows) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	m.clampWindow()
	m.syncDetail()
	m.flash = "✓ deleted"
	m.flashID++
	return m, flashTick(m.flashID)
}

func flashTick(id int) tea.Cmd {
	return tea.Tick(flashMs*time.Millisecond, func(time.Time) tea.Msg {
		return flashExpireMsg{id: id}
	})
}

// hostsTickInterval/hostsTickMax bound the init-time warm loop: at most
// hostsTickMax ticks, one per hostsTickInterval.
const (
	hostsTickInterval = time.Second
	hostsTickMax      = 15
)

func hostsTick() tea.Cmd {
	return tea.Tick(hostsTickInterval, func(time.Time) tea.Msg { return hostsTickMsg{} })
}

// onHostsTick drives the bounded init-time warm loop. While the remote cache is
// enabled but not yet OK, it re-issues the table query and the host fetch so a
// cold daemon converges without a keystroke, then reschedules itself. It STOPS
// — and never reschedules — once the cache is OK (one final fetch), the remote
// is Off (disabled), or a hard tick cap is reached, so it can never spin
// forever. A single in-flight chain is enforced by m.ticking: any tick arriving
// after the chain has stopped is dropped.
func (m Model) onHostsTick() (tea.Model, tea.Cmd) {
	if !m.ticking {
		return m, nil // stale tick from a superseded/stopped chain
	}
	m.hostsTicks++
	switch {
	case m.remote.State == proto.RemoteOK:
		m.ticking = false
		return m, m.hostsCmd() // one final refresh now the cache is warm
	case m.remote.State == proto.RemoteOff:
		m.ticking = false
		return m, nil // remote disabled: nothing to warm
	case m.hostsTicks >= hostsTickMax:
		m.ticking = false
		return m, nil // hard cap: give up rather than poll forever
	}
	// Still syncing / unavailable (or state not yet known): re-issue both and
	// reschedule the single chain.
	mm, qcmd := m.issueQuery()
	return mm, tea.Batch(qcmd, mm.hostsCmd(), hostsTick())
}

// --- selection / window helpers -----------------------------------------

// halfPage is the vim ctrl+d/ctrl+u scroll distance: half the visible table.
func (m Model) halfPage() int {
	h := m.tableRows / 2
	if h < 1 {
		h = 1
	}
	return h
}

func (m *Model) moveSel(d int) {
	if len(m.rows) == 0 {
		return
	}
	m.sel += d
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel >= len(m.rows) {
		m.sel = len(m.rows) - 1
	}
	m.clampWindow()
}

func (m *Model) clampWindow() {
	rv := m.tableRows
	if rv < 1 {
		rv = 1
	}
	if m.sel < m.top {
		m.top = m.sel
	}
	if m.sel >= m.top+rv {
		m.top = m.sel - rv + 1
	}
	if m.top < 0 {
		m.top = 0
	}
	if maxTop := len(m.rows) - rv; m.top > maxTop {
		if maxTop < 0 {
			maxTop = 0
		}
		m.top = maxTop
	}
}

func (m Model) now() int64 {
	if m.opts.Now != 0 {
		return m.opts.Now
	}
	return time.Now().UnixMilli()
}

// selected returns the currently highlighted record and whether one exists.
func (m Model) selected() (rec.Record, bool) {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return rec.Record{}, false
	}
	return m.rows[m.sel], true
}

// --- layout -------------------------------------------------------------

// applyLayout recomputes pane geometry from the current terminal size. It runs
// on every WindowSizeMsg and help toggle; it never builds styles.
func (m *Model) applyLayout() {
	w, h := m.width, m.height
	if w < 1 {
		w = 80
	}
	if h < 1 {
		h = 24
	}

	m.help.Width = w
	helpH := lipgloss.Height(m.help.View(m.helpKeys()))

	// search line (1) + status line (1) + help.
	mid := h - 2 - helpH
	if mid < 3 {
		mid = 3
	}
	m.midHeight = mid

	// Split the right column between the table and the detail pane.
	detail := mid / 3
	if detail < 5 {
		detail = 5
	}
	if detail > 12 {
		detail = 12
	}
	if detail > mid-4 {
		detail = mid - 4
	}
	if detail < 3 {
		detail = 3
	}
	m.detailOuterH = detail
	m.tableOuterH = mid - detail

	// Responsive sidebar width: shrink it on narrow terminals so left + right
	// always sum to exactly w (no horizontal overflow).
	lw := leftWidth
	if lw > w-12 {
		lw = w - 12
	}
	if lw < 10 {
		lw = 10
	}
	if lw > w-1 {
		lw = w - 1
	}
	m.leftW = lw

	// Right column content width (minus left sidebar and the pane border).
	rightOuter := w - lw
	tableContent := rightOuter - 2
	if tableContent < 1 {
		tableContent = 1
	}
	m.tableWidth = tableContent

	// Visible data rows: table content height minus the header line.
	m.tableRows = m.tableOuterH - 2 - 1
	if m.tableRows < 1 {
		m.tableRows = 1
	}

	// Detail viewport fills the detail box interior.
	m.detail.Width = tableContent
	m.detail.Height = m.detailOuterH - 2
	if m.detail.Height < 1 {
		m.detail.Height = 1
	}

	// Text input spans the search line after the "❯ " prompt (reserve the
	// trailing cursor cell textinput always draws).
	iw := w - 3
	if iw < 4 {
		iw = 4
	}
	m.ti.Width = iw

	m.clampWindow()
}
