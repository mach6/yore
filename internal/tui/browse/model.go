package browse

import (
	"io"
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

	// table window
	sel int
	top int

	// stats
	stats        *statsData
	statsErr     error
	gotStats     bool
	statsSeq     uint64
	appliedStats uint64

	// interaction state
	view          viewMode
	focus         focus
	vim           bool // vi-style navigation (from Options.Keymap == "vim")
	searching     bool
	confirmDelete bool
	showHelp      bool
	flash         string
	flashID       int
	quitting      bool

	// query sequencing
	seq        uint64
	appliedSeq uint64

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
		focus:  focusTable,
		width:  80,
		height: 24,
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
		return mm, tea.Batch(qcmd, mm.hostsCmd())

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

	case flashExpireMsg:
		if msg.id == m.flashID {
			m.flash = ""
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
	rows := append([]rec.Record(nil), msg.resp.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartMs > rows[j].StartMs })
	m.rows = rows
	m.total = msg.resp.Total
	m.remote = msg.resp.Remote
	m.sel = 0
	m.top = 0
	m.clampWindow()
	m.syncDetail()
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
	m.stats = computeStats(msg.resp.Rows, m.hosts, m.now())
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
		Q:      m.ti.Value(),
		Scope:  it.scope,
		Host:   it.host,
		Limit:  queryLimit,
		Dedupe: false, // browse shows the real timeline, newest first
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
	}

	if m.view == viewStats {
		if s == "esc" {
			m.view = viewBrowse
		}
		return m, nil
	}

	// Browse-view keys.
	switch s {
	case "/":
		m.searching = true
		m.ti.Focus()
		return m, nil
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

func (m Model) copySelected() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	cmd := m.rows[m.sel].Cmd
	m.flash = "✓ copied"
	m.flashID++
	id := m.flashID
	out := m.out
	return m, tea.Batch(
		func() tea.Msg {
			if out != nil {
				_, _ = io.WriteString(out, osc52(cmd))
			}
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
	helpH := lipgloss.Height(m.help.View(m.keys))

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
