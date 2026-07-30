package browse

import (
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/risk"
	"yore/internal/tui/keyhelp"
	"yore/internal/tui/theme"
)

// Backend is how the browser talks to the daemon. *daemon.Client satisfies it;
// tests substitute a fake. The browser never imports daemon.
type Backend interface {
	Query(proto.QueryReq) (proto.QueryResp, error)
	Hosts() (proto.HostsInfo, error)
	Delete(id string) error
	SubmitRecord(rec.Record) error
	Devices() (proto.DevicesInfo, error)
	Approve(id string) error
	Revoke(id string) error
	Tokens() (proto.TokensInfo, error)
	Token() (proto.TokenInfo, error)
	RevokeToken(id string) error
	Sync() error
}

// Options configures a browse session.
type Options struct {
	Version string
	Session string // current shell session id ($YORE_SESSION)
	Cwd     string // current directory
	Now     int64  // injectable clock in unix ms; 0 => time.Now
	Keymap  string // "vim" enables vi-style navigation; "" / "emacs" = default

	// HideAgents starts the browse table with agent-run commands filtered out
	// (config hide_agent_commands). A is the session toggle; this is only the
	// state it opens in.
	HideAgents bool

	// Splits restores the pane layout the user last dragged to; the zero value
	// starts each view at its default proportions. SaveSplits, when set, is
	// called once a drag finishes so the choice sticks across runs — it runs in
	// a tea.Cmd, off the render path, and its error is ignored (a layout that
	// fails to persist must never interrupt browsing).
	Splits     Splits
	SaveSplits func(Splits) error

	// Start is the screen to open on — how `yore stats` and `yore agents` land
	// straight where they mean to. An unknown value opens the browse table, so a
	// bad string costs nothing. Esc still drops through to browse from either.
	Start StartView

	// Risk is the ruleset behind the detail panes' Risk row — risk.Load's
	// result, so the user's risk.toml applies here exactly as it does to the
	// MCP assess_risk tool. Nil falls back to the built-in rules.
	Risk *risk.Ruleset
}

// StartView names the screen Options.Start opens on.
type StartView string

const (
	StartBrowse  StartView = ""        // the command table (the default)
	StartStats   StartView = "stats"   // the full-screen stats screen
	StartAgents  StartView = "agents"  // the agent explorer
	StartDevices StartView = "devices" // the enrolled-device pane
)

// Tunables.
//
// Neither the table nor the aggregation caps how much history it asks for: this
// is the screen you open to look through your archive, and a row budget makes
// the oldest of it unreachable by scrolling. The daemon sorts every match before
// windowing regardless, so asking for all of them buys the whole timeline for
// the cost of serializing it — and a non-empty query is narrowed server-side, so
// the full corpus only crosses the socket when you asked to see the full corpus.
const (
	leftWidth = 24   // host-sidebar outer width (incl. border)
	flashMs   = 1500 // how long the copied/deleted flash lingers
)

// focus identifies which of the three browse panes owns navigation keys.
type focus int

const (
	focusHosts focus = iota
	focusTable
	focusDetail
)

// viewMode switches between the browse panes and the full-screen views.
type viewMode int

const (
	viewBrowse viewMode = iota
	viewStats
	viewDevices
	viewAgents
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
	b      Backend
	opts   Options
	th     *theme.Theme
	riskRS *risk.Ruleset // never nil; the detail panes' Risk row asks it

	// child components
	ti     textinput.Model
	detail viewport.Model

	// pre-built (once) box styles for focused / blurred panes
	borderFocus lipgloss.Style
	borderBlur  lipgloss.Style
	inkFocus    lipgloss.Style
	inkBlur     lipgloss.Style

	// data. allRows is what the daemon returned; rows is that narrowed to the
	// selected period (see applyPeriodFilter) and is what the table renders.
	hosts     []hostItem
	hostSel   int
	allRows   []rec.Record
	rows      []rec.Record
	srvTotal  int // matches the daemon reported before its own limit
	total     int // what the table is actually showing, after the period filter
	remote    proto.RemoteInfo
	lastErr   error
	gotResult bool
	hasExec   bool // any current row was run by an executor (gates the exec column)
	hasTags   bool // any current row carries a user tag (gates the tags column)

	// hideAgents keeps agent-run commands out of the table (the A key). hidden
	// is how many the daemon dropped for the current query — the number the
	// status bar and the empty state quote, so the filter never costs the user
	// history without telling them.
	hideAgents bool
	hidden     int

	// table window
	sel int
	top int

	// stats
	stats           *statsData
	agents          *agentsData  // per-executor aggregation, from the same sample
	prompts         *promptsData // prompt aggregation, from the same sample
	promptSel       int          // selected prompt in the explorer's prompt pane
	drillSel        int          // selected row within that prompt's command pane
	hscroll         int          // horizontal column offset for the focused list's selected row
	agentSel        int          // selected row in the executor sidebar (0 = all agents)
	agentFilter     string       // executor the sidebar is filtering to; "" = all
	agentHostFilter string       // hostname the explorer is filtering to; "" = all hosts
	agentHosts      []cmdCount   // the host pane's rows: hosts with agent activity in the period, name order
	agentHostSel    int          // selected row in the explorer's host pane (0 = all hosts)
	apane           agentPane    // which of the explorer's five panes holds focus
	infoCmd         bool         // the details pane is describing the selected command, not its prompt
	infoTop         int          // the details pane's scroll offset, in body lines
	statsRows       []rec.Record // the full sample; re-aggregated when the period changes
	statsTotal      int          // matches the daemon reported for that sample (see statsData.capped)
	promptRows      []rec.Record // prompt records covering the sample, incl. ones that ran nothing

	// The explorer's per-list text filters (the / key). Each list keeps its own
	// query: tabbing between panes must not silently re-point one pane's filter
	// at another pane's rows. filteredPrompts is prompts.prompts narrowed by
	// promptQ, held rather than recomputed because every render and every cursor
	// move asks for it.
	afilter         textinput.Model
	afiltering      bool      // the filter input has focus
	afilterPane     agentPane // which list that input is editing
	promptQ         string
	cmdQ            string
	filteredPrompts []promptStat

	period       int // index into statPeriods
	statsErr     error
	gotStats     bool
	statsSeq     uint64
	appliedStats uint64

	// interaction state
	view           viewMode
	focus          focus
	vim            bool // vi-style navigation (from Options.Keymap == "vim")
	searching      bool
	tagging        bool // ctrl+t: entering a freeform tag for the selected row
	tagInput       textinput.Model
	confirmDelete  bool
	showHelp       bool   // ?: the key panel, over whichever view is beneath it
	helpTop        int    // first visible row of that panel, when it overflows
	executorFilter string // active executor filter (the e key); "" = no filter
	tagFilter      string // active user-tag filter (the t key); "" = no filter
	flash          string
	flashID        int
	quitting       bool
	accepted       string // command the user chose with enter; read by Run on exit

	// devices view: the enrolled machines over the enrollment tokens.
	devices    []proto.DeviceInfo
	devSel     int
	devErr     error
	gotDevices bool
	dpane      devPane // which of the two panes has focus
	tokens     []proto.EnrollToken
	tokSel     int
	gotTokens  bool
	// minted is a token this session just created, held only so it can be read
	// off the screen. The server keeps a hash, so this is the one moment the
	// plaintext exists — and it goes nowhere but here: not to ui.toml, not to
	// the log, not to disk.
	minted     string
	mintedTill int64 // expiry of that token, unix ms
	// devConfirm is the id of an action awaiting y/n ("" = none); devApproving
	// and devConfirmKind say which action, on which kind of thing. Every action
	// here asks first: revoking a device rotates the group's keys, revoking a
	// token cannot be undone, and approving admits a machine to everything the
	// group can read — which is only safe if the verification code on screen is
	// checked against the one that machine is showing.
	devConfirm     string
	devApproving   bool
	devConfirmKind devPane // whether devConfirm names a device or a token

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

	// pane geometry: where the active view's panes and their draggable seams
	// landed (geo), where the user has dragged those seams to (splits), which
	// seam a mouse drag is currently moving (drag), and whether the focused pane
	// is expanded to fill the frame (zoom).
	geo    layout
	splits Splits
	drag   dragKind
	zoom   bool

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

	afilter := textinput.New()
	afilter.Prompt = "" // the explorer draws its own "filter <list> ❯" prompt
	afilter.TextStyle = th.Input
	afilter.Cursor.SetMode(cursor.CursorStatic)
	afilter.Cursor.Style = th.Accent

	tagInput := textinput.New()
	tagInput.Prompt = "tag: "
	tagInput.Placeholder = "name"
	tagInput.TextStyle = th.Input
	tagInput.Cursor.SetMode(cursor.CursorStatic)
	tagInput.Cursor.Style = th.Accent

	vp := viewport.New(0, 0)

	riskRS := opts.Risk
	if riskRS == nil {
		riskRS = risk.DefaultRuleset()
	}

	m := Model{
		b:          b,
		opts:       opts,
		th:         th,
		vim:        vim,
		riskRS:     riskRS,
		hideAgents: opts.HideAgents,
		ti:         ti,
		tagInput:   tagInput,
		afilter:    afilter,
		detail:     vp,
		hosts:      []hostItem{{label: "All hosts", scope: proto.ScopeAll}},
		period:     allPeriod, // open on the widest window; 1..5 narrow it
		focus:      focusTable,
		apane:      apPrompts,
		splits:     opts.Splits.withDefaults(),
		width:      80,
		height:     24,
		borderFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Accent.GetForeground()),
		borderBlur: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Border.GetBorderTopForeground()),
		// The same two inks as plain foregrounds: titledBox composes its own rule
		// glyphs (to seat the pane name in the top border) rather than letting
		// lipgloss draw the frame, so it needs the color without the border.
		inkFocus: lipgloss.NewStyle().Foreground(th.Accent.GetForeground()),
		inkBlur:  lipgloss.NewStyle().Foreground(th.Border.GetBorderTopForeground()),
	}
	switch opts.Start {
	case StartStats:
		m.view = viewStats
	case StartAgents:
		m.view = viewAgents
	case StartDevices:
		m.view = viewDevices
	}
	m.applyLayout()
	return m
}

// needsStatsSample reports whether the active view is one of the two that render
// from the shared aggregation sample.
func (m Model) needsStatsSample() bool {
	return m.view == viewStats || m.view == viewAgents
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
		cmds := []tea.Cmd{qcmd, mm.hostsCmd(), hostsTick()}
		// Opening straight onto stats or the agent explorer (yore stats /
		// yore agents) needs the aggregation sample the `s` and `a` keys would
		// otherwise have fetched on the way in.
		if mm.needsStatsSample() {
			mm.statsSeq++
			cmds = append(cmds, mm.statsCmd(mm.statsSeq))
		}
		// Same for `yore devices`: the D key fetches both lists on the way in, so
		// landing on that view directly has to fetch them too or it opens on
		// "loading…" and stays there.
		// Appended individually rather than as one batch: Init's own return is
		// already a tea.Batch, and nesting one inside it buys nothing.
		if mm.view == viewDevices {
			cmds = append(cmds, mm.devicesCmd(), mm.tokensCmd())
		}
		return mm, tea.Batch(cmds...)

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

	case tokensResultMsg:
		m.gotTokens = true
		if msg.err != nil {
			m.devErr = msg.err // one error line for the view; the panes share it
		}
		m.tokens = msg.info.Tokens
		if m.tokSel >= len(m.tokens) {
			m.tokSel = 0
		}
		return m, nil

	case mintedMsg:
		if msg.err != nil {
			m.devErr = msg.err
			return m, nil
		}
		// Held for display only, and only until dismissed. A refetch follows so
		// the new token appears in the list beneath it as "open".
		m.minted, m.mintedTill = msg.info.Token, msg.info.ExpiresMs
		return m, m.tokensCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
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
	m.allRows = rows
	m.srvTotal = msg.resp.Total
	m.hidden = msg.resp.HiddenAgents
	m.remote = msg.resp.Remote
	m.applyPeriodFilter()
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
	m.statsTotal = msg.resp.Total
	m.promptRows = msg.resp.Prompts
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
		// WantPrompts: the agent explorer aggregates prompts from the commands
		// they caused, so a prompt that caused none has no row to be found in.
		// These carry them alongside.
		resp, err := b.Query(proto.QueryReq{Scope: proto.ScopeAll, Limit: proto.LimitAll, WantPrompts: true})
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
		Tag:      m.tagFilter,
		Limit:    proto.LimitAll,
		Dedupe:   false, // browse shows the real timeline, newest first
		// Asking for one executor is asking for agent commands, so the two
		// filters cannot both apply — e wins over A while it is set.
		HumanOnly: m.hideAgents && m.executorFilter == "",
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

	// Tag input focused: esc cancels, enter submits the tag, the rest edits.
	if m.tagging {
		switch s {
		case "esc", "ctrl+c":
			m.tagging = false
			m.tagInput.Blur()
			return m, nil
		case "enter":
			return m.submitTag()
		}
		var cmd tea.Cmd
		m.tagInput, cmd = m.tagInput.Update(msg)
		return m, cmd
	}

	// The explorer's filter box: esc/enter leave it, the rest edits and re-filters
	// as you type.
	if m.afiltering {
		return m.handleAgentFilterKey(msg, s)
	}

	// The key panel is a modal like the others: it lists what works in the view
	// beneath it, so letting those keys fire through it would act on a view the
	// user cannot currently see.
	if m.showHelp {
		return m.handleHelpKey(s)
	}

	// Devices view swallows its own keys.
	if m.view == viewDevices {
		return m.handleDevicesKey(s)
	}

	// Horizontal scroll is anchored to the current selection; any key other than
	// the scroll keys themselves collapses the row back to its start.
	if s != "left" && s != "right" {
		m.hscroll = 0
	}

	// Global keys (both views).
	switch s {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		return m.openHelp()
	case "s":
		return m.toggleStats()
	case "a":
		return m.toggleAgents()
	case "z":
		return m.toggleZoom()
	case "1", "2", "3", "4", "5":
		// The period is global, so the tabs work in every view — including the
		// browse table, which had no time filter at all before.
		return m.setPeriod(int(s[0] - '1'))
	case "S":
		return m.syncNow()
	case "D":
		m.view = viewDevices
		m.devConfirm, m.dpane, m.zoom = "", dpDevices, false
		m.applyLayout()
		return m, m.refreshDevicesCmd()
	}

	// The agent explorer is interactive (four focusable panes), so it owns its
	// keys rather than sharing the simple stats handler.
	if m.view == viewAgents {
		return m.handleAgentsKey(s)
	}

	if m.view == viewStats {
		if s == "esc" {
			m.view = viewBrowse
		}
		return m, nil
	}

	// Browse-view keys.
	switch s {
	case "esc":
		if m.zoom {
			return m.toggleZoom()
		}
		return m, nil
	case "/":
		m.searching = true
		m.ti.Focus()
		return m, nil
	case "t":
		return m.toggleTagFilter()
	case "e":
		return m.toggleExecutorFilter()
	case "H":
		return m.cycleHost()
	case "A":
		return m.toggleHideAgents()
	case "ctrl+t":
		if m.sel >= 0 && m.sel < len(m.rows) {
			m.tagging = true
			m.tagInput.SetValue("")
			m.tagInput.Focus()
		}
		return m, nil
	case "tab":
		m.cycleFocus(1)
		return m, nil
	case "shift+tab":
		m.cycleFocus(-1)
		return m, nil
	}

	// Vim: h/l move focus between panes. (left/right are reserved for horizontal
	// scroll of the table's selected command, handled under focusTable below.)
	if m.vim {
		switch s {
		case "h":
			m.cycleFocus(-1)
			return m, nil
		case "l":
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
		case "right":
			m.scrollRight()
		case "left":
			m.scrollLeft()
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

// --- the key panel ------------------------------------------------------

// openHelp raises the key panel over the current view.
func (m Model) openHelp() (tea.Model, tea.Cmd) {
	m.showHelp = true
	m.helpTop = 0
	return m, nil
}

// handleHelpKey services the key panel: it scrolls when the list is taller than
// the pane, and esc/?/q dismiss it. q closes the panel rather than quitting —
// the panel is transient, and on the devices screen underneath it q means "back",
// so a q that quit from here would be the one place it ended the session.
func (m Model) handleHelpKey(s string) (tea.Model, tea.Cmd) {
	page := maxInt(1, m.helpBodyHeight())
	switch s {
	case "esc", "?", "q":
		m.showHelp = false
		m.helpTop = 0
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		m.helpTop--
	case "down", "j":
		m.helpTop++
	case "pgup":
		m.helpTop -= page
	case "pgdown":
		m.helpTop += page
	case "g", "home":
		m.helpTop = 0
	case "G", "end":
		m.helpTop = m.helpMaxTop()
	}
	if top := m.helpMaxTop(); m.helpTop > top {
		m.helpTop = top
	}
	if m.helpTop < 0 {
		m.helpTop = 0
	}
	return m, nil
}

// The panel is a pane like any other: its body is the frame less the box sides
// and the two border rows.
func (m Model) helpBodyWidth() int  { return maxInt(1, m.width-2) }
func (m Model) helpBodyHeight() int { return maxInt(1, m.midHeight-2) }

// helpMaxTop is how far the panel can scroll — zero unless the list is taller
// than the pane, which on a stock 80×24 terminal it can be.
func (m Model) helpMaxTop() int {
	body := m.helpBodyHeight()
	_, total := keyhelp.Panel(m.th, m.helpGroups(), m.helpBodyWidth(), body, 0)
	return maxInt(0, total-body)
}

// applyPeriodFilter narrows the queried rows to the selected period. The daemon's
// query protocol carries no time window, so the browse table filters the rows it
// got back — every row matching the query, so the period narrows the whole
// timeline rather than a slice of it. The "All" tab is a straight pass-through.
func (m *Model) applyPeriodFilter() {
	cutoff := periodCutoff(m.now(), m.periodDays())
	if cutoff == 0 {
		m.rows, m.total = m.allRows, m.srvTotal
	} else {
		kept := make([]rec.Record, 0, len(m.allRows))
		for _, r := range m.allRows {
			if r.StartMs >= cutoff {
				kept = append(kept, r)
			}
		}
		m.rows, m.total = kept, len(kept)
	}
	m.hasExec, m.hasTags = false, false
	for _, r := range m.rows {
		m.hasExec = m.hasExec || r.Executor != ""
		m.hasTags = m.hasTags || len(r.Tags) > 0
		if m.hasExec && m.hasTags {
			break
		}
	}
	m.clampWindow()
}

// setPeriod switches the shared time window. It applies everywhere at once — the
// browse table, the agent explorer, and the stats screen all read the same
// period, so the 1..5 keys mean one thing wherever they are pressed.
func (m Model) setPeriod(p int) (tea.Model, tea.Cmd) {
	if p < 0 || p >= len(statPeriods) || p == m.period {
		return m, nil
	}
	m.period = p
	m.drillSel, m.infoTop = 0, 0
	m.applyPeriodFilter()
	if m.sel >= len(m.rows) {
		m.sel = maxInt(0, len(m.rows)-1)
	}
	m.clampWindow()
	m.recomputeStats()
	m.syncDetail()
	return m, nil
}

// hiddenAgentsNote describes what the agent filter is holding back, or "" when
// it is holding nothing back worth reporting. The footer always names the
// filter's state; this speaks only when history is actually being withheld.
//
// The count is the daemon's, taken across everything the query matched. The
// period tabs narrow further, client-side, over rows the daemon has already
// dropped — so with a period selected the count cannot be attributed to the
// window on screen, and the note states the filter without quoting a number
// that may be describing last month.
func (m Model) hiddenAgentsNote() string {
	if !m.hideAgents || m.hidden == 0 {
		return ""
	}
	if m.period != allPeriod {
		return "agent commands hidden"
	}
	return plural(m.hidden, "agent command") + " hidden"
}

// toggleHideAgents flips agent-run commands in and out of the browse table (the
// A key), re-issuing the query because the filter is applied server-side.
//
// It is a session toggle, not a setting: config's hide_agent_commands decides
// what the browser opens with, and a keystroke that quietly rewrote that file
// would make an experiment permanent.
func (m Model) toggleHideAgents() (tea.Model, tea.Cmd) {
	m.hideAgents = !m.hideAgents
	if m.hideAgents {
		m.flash = "agent commands hidden"
	} else {
		m.flash = "agent commands shown"
	}
	m.flashID++
	var q tea.Cmd
	m, q = m.issueQuery()
	return m, tea.Batch(q, flashTick(m.flashID))
}

// toggleExecutorFilter flips the executor filter (the e key). With a filter
// active it clears it; otherwise it adopts the selected row's executor if it has
// one (flashing and doing nothing when the user typed the command). Either
// change re-issues the query and flashes the new state.
func (m Model) toggleExecutorFilter() (tea.Model, tea.Cmd) {
	if m.executorFilter != "" {
		m.executorFilter = ""
		return m.afterFilterChange("executor filter cleared")
	}
	r, ok := m.selected()
	if !ok || r.Executor == "" {
		return m.flashOnly("no executor on this row")
	}
	m.executorFilter = r.Executor
	return m.afterFilterChange("executor: " + r.Executor)
}

// toggleTagFilter flips the user-tag filter (the t key), the mirror of e for the
// other axis: with one active it clears it, otherwise it adopts the selected
// row's first tag. Executors are not tags, so a row an agent ran but nobody
// labelled has nothing to adopt here.
func (m Model) toggleTagFilter() (tea.Model, tea.Cmd) {
	if m.tagFilter != "" {
		m.tagFilter = ""
		return m.afterFilterChange("tag filter cleared")
	}
	r, ok := m.selected()
	if !ok || len(r.Tags) == 0 {
		return m.flashOnly("no tags on this row — ^t adds one")
	}
	m.tagFilter = r.Tags[0]
	return m.afterFilterChange("tag: " + r.Tags[0])
}

// afterFilterChange re-issues the query and flashes what changed.
func (m Model) afterFilterChange(msg string) (tea.Model, tea.Cmd) {
	m.flash = msg
	m.flashID++
	mm, qcmd := m.issueQuery()
	return mm, tea.Batch(qcmd, flashTick(mm.flashID))
}

// flashOnly says why nothing happened, without spending a query on it.
func (m Model) flashOnly(msg string) (tea.Model, tea.Cmd) {
	m.flash = msg
	m.flashID++
	return m, flashTick(m.flashID)
}

// recomputeStats re-derives the stats, agent, and prompt aggregates from the
// held sample for the current period and executor filter. Cheap; called on load,
// on a period-tab change, and when the sidebar's filter moves.
//
// The executor filter is held by NAME, not by row index: an executor that drops
// out of the period would otherwise silently hand its row (and the filter) to a
// different agent.
func (m *Model) recomputeStats() {
	if m.statsRows == nil {
		return
	}
	days := m.periodDays()
	// The stats screen reads m.stats from this same call, so it aggregates the
	// unfiltered sample: the explorer's host filter must not bleed into it.
	m.stats = computeStats(m.statsRows, m.statsTotal, m.now(), days)

	hostsBefore := len(m.agentHosts)
	m.agentHosts = agentHostsIn(m.statsRows, m.promptRows, m.now(), days)
	if len(m.agentHosts) != hostsBefore {
		m.applyLayout() // the host pane is content-sized, so its height just changed
	}
	m.agentHostSel = 0
	for i, c := range m.agentHosts {
		if c.name == m.agentHostFilter {
			m.agentHostSel = i + 1
		}
	}
	if m.agentHostSel == 0 {
		m.agentHostFilter = "" // the filtered host has no agent activity in this period
	}
	rows, prompts := m.statsRows, m.promptRows
	if m.agentHostFilter != "" {
		rows = hostOnly(rows, m.agentHostFilter)
		prompts = hostOnly(prompts, m.agentHostFilter)
	}

	m.agents = computeAgents(rows, m.now(), days)
	m.agentSel = 0
	for i, a := range m.agents.agents {
		if a.name == m.agentFilter {
			m.agentSel = i + 1
		}
	}
	if m.agentSel == 0 {
		m.agentFilter = "" // the filtered executor has no commands in this period
	}
	m.prompts = computePrompts(rows, prompts, m.now(), days, m.agentFilter)
	m.applyPromptFilter()
	m.clampPrompts()
}

// toggleAgents opens the agent explorer (or returns to browse), reusing the
// stats sample query so the aggregation has data. Opening always starts on the
// newest prompt with the prompt pane focused.
func (m Model) toggleAgents() (tea.Model, tea.Cmd) {
	if m.view == viewAgents {
		m.view = viewBrowse
		m.zoom = false
		m.applyLayout()
		return m, nil
	}
	m.view = viewAgents
	m.apane = apPrompts
	m.zoom = false
	m.promptSel = 0
	m.drillSel = 0
	m.infoCmd, m.infoTop = false, 0 // opening on the prompt list, details describes its prompt
	m.applyLayout()
	if m.statsRows != nil {
		m.recomputeStats()
		return m, nil
	}
	m.statsSeq++
	return m, m.statsCmd(m.statsSeq)
}

// toggleZoom expands the focused pane to fill the frame (or restores the tiled
// layout). Only the multi-pane views have anything to zoom.
func (m Model) toggleZoom() (tea.Model, tea.Cmd) {
	if m.view != viewBrowse && m.view != viewAgents {
		return m, nil
	}
	m.zoom = !m.zoom
	m.applyLayout()
	m.syncDetail()
	return m, nil
}

// handleAgentsKey services the agent explorer: navigation in whichever pane holds
// focus, Tab to cycle panes, / to filter that pane's list, Esc to back out one
// level at a time, plus the shared period tabs. The global keys (a/s/z/q/S/D/?)
// are handled before this in handleKey, so they still work here.
func (m Model) handleAgentsKey(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc":
		// Back out one visible thing at a time, innermost first: the zoom, then
		// the focused list's filter, then the view.
		if m.zoom {
			return m.toggleZoom()
		}
		if m.clearFocusedFilter() {
			return m, nil
		}
		m.view = viewBrowse
		m.applyLayout()
		return m, nil
	case "/":
		return m.openAgentFilter()
	case "H":
		return m.cycleAgentHost()
	case "A":
		return m.cycleAgent()
	case "tab":
		m.focusAgentPane(1)
		return m, nil
	case "shift+tab":
		m.focusAgentPane(-1)
		return m, nil
	case "up", "k":
		m.moveAgentPane(-1)
		return m, nil
	case "down", "j":
		m.moveAgentPane(1)
		return m, nil
	case "pgup":
		m.moveAgentPane(-m.agentPaneRows())
		return m, nil
	case "pgdown":
		m.moveAgentPane(m.agentPaneRows())
		return m, nil
	case "g", "home":
		m.jumpAgentPane(true)
		return m, nil
	case "G", "end":
		m.jumpAgentPane(false)
		return m, nil
	case "right":
		m.scrollRight()
		return m, nil
	case "left":
		m.scrollLeft()
		return m, nil
	}
	return m, nil
}

// --- the explorer's list filters ----------------------------------------

// filterTarget is the list the / key filters from the current pane. The two list
// panes filter themselves; the sidebar and the details pane have no list of
// their own to narrow, so they aim at the prompts — the list everything else in
// the view hangs off.
func (m Model) filterTarget() agentPane {
	if m.apane == apCommands {
		return apCommands
	}
	return apPrompts
}

// filterFor returns the committed query for a list.
func (m Model) filterFor(p agentPane) string {
	if p == apCommands {
		return m.cmdQ
	}
	return m.promptQ
}

// setFilter stores a list's query and re-narrows whatever it feeds. The cursor
// goes back to the top: after a query change the row under it is a different
// row, and leaving the cursor at index 3 of a list that just became two rows
// long only looks like the filter misfired.
func (m *Model) setFilter(p agentPane, q string) {
	m.hscroll = 0 // the row under the cursor is a different row now
	m.infoTop = 0 // and the details pane is describing a different one
	if p == apCommands {
		m.cmdQ = q
		m.drillSel = 0
		return
	}
	m.promptQ = q
	m.applyPromptFilter()
	m.promptSel = 0
	m.drillSel = 0
}

// openAgentFilter puts the cursor in the filter box for the focused list,
// pre-loaded with that list's current query so a filter can be edited rather
// than retyped.
func (m Model) openAgentFilter() (tea.Model, tea.Cmd) {
	m.afilterPane = m.filterTarget()
	m.afiltering = true
	m.afilter.SetValue(m.filterFor(m.afilterPane))
	m.afilter.CursorEnd()
	m.afilter.Focus()
	return m, nil
}

// handleAgentFilterKey services the filter box. Esc and Enter both leave it with
// the query kept — the filter is the point, and there is nothing to "cancel"
// that closing the box would not also undo — so the way to drop a filter is to
// empty it, or Esc again once the box is closed.
func (m Model) handleAgentFilterKey(msg tea.KeyMsg, s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc", "enter":
		m.afiltering = false
		m.afilter.Blur()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}
	prev := m.afilter.Value()
	var cmd tea.Cmd
	m.afilter, cmd = m.afilter.Update(msg)
	if v := m.afilter.Value(); v != prev {
		m.setFilter(m.afilterPane, v)
	}
	return m, cmd
}

// clearFocusedFilter drops the focused list's filter, reporting whether there
// was one. It is what the first Esc does in a filtered explorer: the filter is
// on screen, so backing out of it before backing out of the view is what the
// key already means everywhere else.
func (m *Model) clearFocusedFilter() bool {
	p := m.filterTarget()
	if m.filterFor(p) == "" {
		return false
	}
	m.setFilter(p, "")
	return true
}

// focusAgentPane moves focus around the explorer's five panes. Zoom follows
// focus, so tabbing while zoomed swaps which pane fills the frame rather than
// dropping back to the tiled layout.
func (m *Model) focusAgentPane(d int) {
	m.setAgentPane(agentPane((int(m.apane) + d + agentPaneCount) % agentPaneCount))
}

// setAgentPane focuses a specific pane — the mouse path, and where tabbing ends
// up. Landing on a list pane points the details pane at what that list selects;
// landing on the details pane itself changes nothing, so tabbing over to read or
// zoom a command's record cannot swap the prompt's in on the way.
func (m *Model) setAgentPane(p agentPane) {
	if m.apane == p {
		return
	}
	m.apane = p
	m.hscroll = 0
	switch p {
	case apCommands:
		m.infoCmd, m.infoTop = true, 0
	case apPrompts, apAgents, apHosts:
		m.infoCmd, m.infoTop = false, 0
	}
	m.applyLayout()
}

// moveAgentPane moves the focused pane's cursor by d rows. The details pane has
// no cursor, but it does have more body than fits, so there it scrolls — the same
// thing ↑/↓ do in the browse view's detail pane. Moving the command cursor from
// here instead, as it once did, meant the keys drove a list the focused pane was
// not even showing.
func (m *Model) moveAgentPane(d int) {
	switch m.apane {
	case apAgents:
		m.selectAgent(m.agentSel + d)
	case apHosts:
		m.selectAgentHost(m.agentHostSel + d)
	case apPrompts:
		m.selectPrompt(m.promptSel + d)
	case apInfo:
		m.scrollInfo(d)
	default:
		m.setDrill(m.drillSel + d)
	}
}

// jumpAgentPane sends the focused pane's cursor to the first or last row, or the
// details pane to the top or bottom of its body.
func (m *Model) jumpAgentPane(first bool) {
	switch m.apane {
	case apAgents:
		if first {
			m.selectAgent(0)
		} else {
			m.selectAgent(m.agentRows() - 1)
		}
	case apHosts:
		if first {
			m.selectAgentHost(0)
		} else {
			m.selectAgentHost(m.hostRows() - 1)
		}
	case apPrompts:
		if first {
			m.selectPrompt(0)
		} else {
			m.selectPrompt(m.promptLen() - 1)
		}
	case apInfo:
		if first {
			m.infoTop = 0
		} else {
			m.infoTop = m.infoMaxTop()
		}
	default:
		if first {
			m.setDrill(0)
		} else {
			m.setDrill(m.drillLen() - 1)
		}
	}
}

// setDrill moves the command cursor. The details pane describes that command
// whenever it is the pane's subject, so its scroll offset belongs to the old row
// and goes back to the top.
func (m *Model) setDrill(i int) {
	next := clampIndex(i, m.drillLen())
	if next == m.drillSel {
		return
	}
	m.drillSel = next
	m.infoTop = 0
}

// scrollInfo moves the details pane's body under its window, clamped so the last
// line stops at the bottom rather than scrolling off into blank space.
func (m *Model) scrollInfo(d int) {
	m.infoTop = clampIndex(m.infoTop+d, m.infoMaxTop()+1)
}

// agentPaneRows is the focused pane's visible row count, for page scrolling.
func (m Model) agentPaneRows() int {
	h := m.geo.p[m.apane].h - 2 // the two border rows; the pane title is in one
	if h < 1 {
		h = 1
	}
	return h
}

// selectAgent moves the sidebar cursor and re-aggregates: picking an executor
// filters the prompt (and therefore command and details) panes to its work.
func (m *Model) selectAgent(i int) {
	next := clampIndex(i, m.agentRows())
	if next == m.agentSel {
		return
	}
	m.agentSel = next
	m.agentFilter = ""
	if a, ok := m.agentAt(next); ok {
		m.agentFilter = a.name
	}
	m.promptSel, m.drillSel, m.infoTop = 0, 0, 0
	m.recomputeStats()
}

// selectAgentHost moves the host pane's cursor and re-aggregates: picking a
// host filters the executor, prompt, command and details panes to that
// machine's work. Like the executor filter, the host filter is held by
// hostname, so recomputeStats releases it when the host drops out of the period.
func (m *Model) selectAgentHost(i int) {
	next := clampIndex(i, m.hostRows())
	if next == m.agentHostSel {
		return
	}
	m.agentHostSel = next
	m.agentHostFilter = ""
	if hc, ok := m.hostAt(next); ok {
		m.agentHostFilter = hc.name
	}
	m.promptSel, m.drillSel, m.infoTop = 0, 0, 0
	m.recomputeStats()
}

// cycleAgentHost is the H key: one stop around the host pane's rows — all
// hosts, each host in name order, back to all — from anywhere in the explorer.
func (m Model) cycleAgentHost() (tea.Model, tea.Cmd) {
	if len(m.agentHosts) < 2 {
		return m.flashOnly("one host in this sample")
	}
	m.selectAgentHost((m.agentHostSel + 1) % m.hostRows())
	return m, nil
}

// cycleAgent is the A key: H's mirror on the other axis, one stop around the
// executor sidebar — all agents, each executor in turn, back to all. With a
// single executor in the sample the "all agents" row and its one child show the
// same work, so there is nothing to cycle between and the key says so instead.
//
// A means this only in the explorer. In the browse table the same key hides and
// shows agent commands, which is that view's one agent-shaped question; here
// every row is agent work already, so the useful question is which agent.
func (m Model) cycleAgent() (tea.Model, tea.Cmd) {
	if m.agents == nil || len(m.agents.agents) < 2 {
		return m.flashOnly("one agent in this sample")
	}
	m.selectAgent((m.agentSel + 1) % m.agentRows())
	return m, nil
}

// hscrollStep is how many display columns one ←/→ press moves the selected row.
const hscrollStep = 8

// scrollRight / scrollLeft nudge the horizontal offset of the focused list's
// selected row, clamped so scrolling stops at the end of the line (and never
// goes negative), keeping ←/→ responsive in both directions.
func (m *Model) scrollRight() {
	m.hscroll += hscrollStep
	if mx := m.maxHScroll(); m.hscroll > mx {
		m.hscroll = mx
	}
}

func (m *Model) scrollLeft() {
	m.hscroll -= hscrollStep
	if m.hscroll < 0 {
		m.hscroll = 0
	}
}

// maxHScroll is the furthest useful horizontal offset for the focused list's
// selected row: enough to bring the end of the text into view, and 0 when it
// already fits or nothing is selected.
func (m Model) maxHScroll() int {
	text, colW, ok := m.hScrollTarget()
	if !ok || colW <= 1 {
		return 0
	}
	total := runewidth.StringWidth(text)
	if total <= colW {
		return 0
	}
	return total - (colW - 1) // mirror hOffset: the leading "…" costs one column
}

// hScrollTarget returns the selected item's single-line primary text and the
// width of the column it renders in, for the currently focused list. ok=false
// when no scrollable list row is focused.
func (m Model) hScrollTarget() (text string, colW int, ok bool) {
	switch m.view {
	case viewAgents:
		if m.prompts == nil {
			return "", 0, false
		}
		iw := m.geo.p[m.apane].w - 2 // the pane renders inside a bordered box
		switch m.apane {
		case apPrompts:
			p, has := m.drilledPrompt()
			if !has {
				return "", 0, false
			}
			return oneLine(p.text), promptLayout(iw, m.prompts.hasDur, m.showPromptHost()).textW, true
		case apCommands:
			r, has := m.drilledCmd()
			if !has {
				return "", 0, false
			}
			return oneLine(r.Cmd), promptCmdCols(iw, m.prompts.hasDur).cmdW, true
		}
		return "", 0, false
	case viewBrowse:
		if m.focus != focusTable {
			return "", 0, false
		}
		r, sel := m.selected()
		if !sel {
			return "", 0, false
		}
		return oneLine(r.Cmd), m.colLayout().cmdW, true
	}
	return "", 0, false
}

// selectPrompt moves the top-pane cursor and, whenever it lands on a different
// prompt, resets the command pane to the top — the bottom pane now reflects a
// different prompt's commands.
func (m *Model) selectPrompt(i int) {
	next := clampIndex(i, m.promptLen())
	if next != m.promptSel {
		m.drillSel, m.infoTop = 0, 0
	}
	m.promptSel = next
}

// applyPromptFilter narrows the aggregated prompts to those matching promptQ.
// It runs whenever the sample, the period, the executor filter or the query text
// changes — everything downstream (the list, the cursor, the command pane, the
// details) reads the narrowed slice, so this is the one place the filter is
// applied.
func (m *Model) applyPromptFilter() {
	if m.prompts == nil {
		m.filteredPrompts = nil
		return
	}
	q := match.Parse(m.promptQ)
	if q.Empty() {
		m.filteredPrompts = m.prompts.prompts
		return
	}
	kept := make([]promptStat, 0, len(m.prompts.prompts))
	for _, p := range m.prompts.prompts {
		if q.Match(p.text) {
			kept = append(kept, p)
		}
	}
	m.filteredPrompts = kept
}

// visibleCmds is the drilled prompt's commands narrowed by cmdQ. The prompt's
// own aggregate (its counts, duration, modal path) is deliberately NOT filtered:
// those describe the prompt, and rewriting them to match a search would make the
// details pane disagree with the prompt list about the same prompt.
func (m Model) visibleCmds() []rec.Record {
	p, ok := m.drilledPrompt()
	if !ok {
		return nil
	}
	q := match.Parse(m.cmdQ)
	if q.Empty() {
		return p.cmds
	}
	kept := make([]rec.Record, 0, len(p.cmds))
	for _, r := range p.cmds {
		if q.Match(r.Cmd) {
			kept = append(kept, r)
		}
	}
	return kept
}

// promptLen / drillLen are the row counts of the two prompt-explorer lists,
// after their filters.
func (m Model) promptLen() int { return len(m.filteredPrompts) }

func (m Model) drillLen() int { return len(m.visibleCmds()) }

// clampPrompts keeps the prompt and drill selections in range after the sample
// (and thus the prompt set) is recomputed.
func (m *Model) clampPrompts() {
	m.promptSel = clampIndex(m.promptSel, m.promptLen())
	m.drillSel = clampIndex(m.drillSel, m.drillLen())
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

// cycleFocus moves focus around the three browse panes. Zoom follows focus, so
// tabbing while zoomed swaps which pane fills the frame.
func (m *Model) cycleFocus(d int) {
	m.focus = focus((int(m.focus) + d + 3) % 3)
	m.hscroll = 0
	m.applyLayout()
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

// cycleHost is the H key: one stop down the sidebar — all hosts, then each
// machine in the order the sidebar lists them, and around again — from anywhere
// in the browse view, the mirror of the explorer's H. It wraps where ↑/↓ clamp:
// a key you press repeatedly to sweep the machines has to come back around,
// while an arrow key that jumped from the last row to the first would be a
// cursor that lost its place. The header's scope word reports where it landed,
// so it needs no flash of its own — but with only the aggregate row there is
// nothing to cycle through, and saying so beats a keypress that looks broken.
func (m Model) cycleHost() (tea.Model, tea.Cmd) {
	if len(m.hosts) < 2 {
		return m.flashOnly("one host in this store")
	}
	m.hostSel = (m.hostSel + 1) % len(m.hosts)
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

// copyText puts text on the clipboard and says so. It emits OSC 52 to the tty
// (tmux-wrapped when inside tmux) so the copy works over SSH, AND best-effort
// pipes to a local clipboard tool, since many terminals silently ignore OSC 52.
// Both are fire-and-forget in a tea.Cmd, so neither blocks the UI.
func (m Model) copyText(text, note string) (tea.Model, tea.Cmd) {
	m.flash = note
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

// copySelected copies the highlighted command to the clipboard (the `y` path).
func (m Model) copySelected() (tea.Model, tea.Cmd) {
	cmd, ok := m.selected()
	if !ok {
		return m, nil
	}
	return m.copyText(cmd.Cmd, "✓ copied")
}

// submitTag sends a user-tag record for the selected row and optimistically
// shows the tag at once (the daemon folds it on ingest; a later query confirms).
func (m Model) submitTag() (tea.Model, tea.Cmd) {
	name := strings.ToLower(strings.TrimSpace(m.tagInput.Value()))
	m.tagging = false
	m.tagInput.Blur()
	if name == "" || m.sel < 0 || m.sel >= len(m.rows) {
		return m, nil
	}
	r := m.rows[m.sel]
	m.flashID++
	if err := m.b.SubmitRecord(rec.Record{ID: rec.NewID(), Type: rec.TypeTag, TagName: name, TargetID: r.ID}); err != nil {
		m.flash = "tag failed"
		m.lastErr = err
		return m, flashTick(m.flashID)
	}
	dup := false
	for _, tg := range m.rows[m.sel].Tags {
		if tg == name {
			dup = true
			break
		}
	}
	if !dup {
		m.rows[m.sel].Tags = append(m.rows[m.sel].Tags, name)
		m.hasTags = true
	}
	m.flash = "tagged: " + name
	return m, flashTick(m.flashID)
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
	case m.remote.State == proto.RemoteRevoked:
		m.ticking = false
		return m, nil // revoked: no amount of waiting brings the remote back
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

	// header (1) + status line (1) + the footer hint line (1). The footer is one
	// line in every state — the expanded key list is a panel over the view, not a
	// footer that grows and reflows the panes under it.
	mid := h - 3
	if mid < 3 {
		mid = 3
	}
	m.midHeight = mid

	// Split the right column between the table and the detail pane. The default
	// keeps the detail pane a compact, bounded slice; once the user drags the
	// seam, splits.BrowseTop takes over and holds that ratio through a resize.
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
	m.tableOuterH = splitAt(m.splits.BrowseTop, mid, minPaneRows, mid-detail)
	m.detailOuterH = mid - m.tableOuterH

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
	m.leftW = splitAt(m.splits.BrowseLeft, w, minPaneCols, lw)
	lw = m.leftW

	// Right column content width (minus left sidebar and the pane border).
	rightOuter := w - lw
	tableContent := rightOuter - 2
	if tableContent < 1 {
		tableContent = 1
	}
	m.tableWidth = tableContent

	// Visible data rows: table content height less the two border rows and the
	// column-header line. The pane title costs nothing — it rides in the border.
	m.tableRows = m.tableOuterH - 2 - 1
	if m.tableRows < 1 {
		m.tableRows = 1
	}

	m.applyGeometry(w, mid)

	// The detail viewport fills its box interior — which, zoomed, is the whole
	// frame, so a long record rewraps to the full width instead of staying
	// wrapped for the tile it came from.
	dr := m.geo.p[focusDetail]
	if m.zoom && m.focus != focusDetail {
		dr = rect{w: tableContent + 2, h: m.detailOuterH}
	}
	m.detail.Width = maxInt(1, dr.w-2)
	m.detail.Height = maxInt(1, dr.h-2) // the two border rows; the title is in one

	// The text input spans the search line after the "❯ " prompt (reserving the
	// trailing cursor cell textinput always draws), less the period tab strip
	// that shares the line.
	iw := w - 3
	if m.showPeriodTabs() {
		iw -= periodTabsWidth() + 2
	}
	if iw < 4 {
		iw = 4
	}
	m.ti.Width = iw
	// The explorer's filter box shares that line with its own "filter <list> ❯"
	// prompt, which is longer than the browse view's bare "❯ ".
	m.afilter.Width = maxInt(4, iw-lipgloss.Width("filter commands "))

	m.clampWindow()
}

// applyGeometry records where the active view's panes and seams landed, so the
// renderers and the mouse agree on one set of rectangles. A zoomed pane owns the
// whole frame and has no seams to grab.
func (m *Model) applyGeometry(w, mid int) {
	if m.zoom {
		g := noDividers()
		full := rect{x: 0, y: 1, w: w, h: mid}
		switch m.view {
		case viewAgents:
			g.p[m.apane] = full
		case viewDevices:
			g.p[m.dpane] = full
		default:
			g.p[m.focus] = full
		}
		m.geo = g
		return
	}
	switch m.view {
	case viewAgents:
		lw := splitAt(m.splits.AgentLeft, w, minPaneCols, w*defaultAgentLeftRatio/ratioFull)
		topH := splitAt(m.splits.AgentTop, mid, minPaneRows, mid*defaultAgentTopRatio/ratioFull)
		// Until its seam is dragged, the host pane is content-sized — you have as
		// many hosts as you have — and the executor list flexes above it. A drag
		// stores AgentHosts and that proportion takes over, like every other seam.
		hostsH := m.hostRows() + 2 // one row per entry plus the two border lines
		if hostsH > topH/2 {
			hostsH = topH / 2
		}
		if hostsH < 3 {
			hostsH = 3
		}
		hostsH = topH - splitAt(m.splits.AgentHosts, topH, 3, topH-hostsH)
		m.geo = agentGeom(w, mid, lw, topH, hostsH)
	case viewDevices:
		topH := splitAt(m.splits.DevicesTop, mid, minPaneRows, mid*defaultDevicesTopRatio/ratioFull)
		m.geo = devicesGeom(w, mid, topH)
	default:
		m.geo = browseGeom(w, mid, m.leftW, m.tableOuterH)
	}
}
