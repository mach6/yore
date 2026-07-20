package search

import (
	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/tui/theme"
)

// Querier is how the TUI talks to the daemon. It is an interface so tests can
// substitute a fake without a running daemon or socket.
type Querier interface {
	Query(proto.QueryReq) (proto.QueryResp, error)
}

// Options configures a search session.
type Options struct {
	InitialQuery string
	Session      string // current shell session id ($YORE_SESSION), for scope cycling
	Cwd          string // current directory, for scope cycling
	Version      string
	Keymap       string // "vim" enables an insert/normal sub-mode; "" / "emacs" = default

	// Renderer is bound to the output tty so color detection ignores a piped
	// os.Stdout; nil = default renderer.
	Renderer *lipgloss.Renderer
}

// Panel geometry.
const (
	minRows    = 5
	maxRows    = 12
	queryLimit = 200 // rows requested from the daemon; more than we ever show, so we can scroll
)

// initMsg is delivered once at startup to kick off the first query without
// blocking the first paint.
type initMsg struct{}

// queryResultMsg carries one daemon response back to the model. seq identifies
// the request it answers so out-of-order responses can be discarded.
type queryResultMsg struct {
	seq  uint64
	resp proto.QueryResp
	err  error
}

// Model is the Bubble Tea model backing the search panel. It is exported so
// tests can drive Update directly with tea messages.
type Model struct {
	q    Querier
	opts Options
	th   *theme.Theme
	ti   textinput.Model

	scope    string // one of proto.Scope*
	dedupe   bool
	frecency bool // alt+f: rank by frequency×recency instead of recency
	fuzzy    bool // alt+z: subsequence matching instead of substring

	vim    bool // vim keymap: Esc toggles an insert/normal sub-mode
	normal bool // vim only: true while in the normal (navigation) sub-mode

	rows      []rec.Record
	total     int
	remote    proto.RemoteInfo
	lastErr   error
	gotResult bool // a response (ok or error) has arrived at least once

	sel int // selected index into rows
	top int // first visible index (windowing)

	seq        uint64 // last issued request sequence
	appliedSeq uint64 // highest sequence whose response has been applied

	rowsVisible int
	width       int
	height      int

	accepted string
	accept   bool
	cancel   bool
	done     bool
}

// NewModel builds the search Model, constructing the Theme once.
func NewModel(q Querier, opts Options) Model {
	th := theme.New()
	if opts.Renderer != nil {
		th = theme.NewWithRenderer(opts.Renderer)
	}

	ti := textinput.New()
	ti.Prompt = "" // the panel draws its own scope-aware prompt
	ti.TextStyle = th.Input
	ti.Cursor.SetMode(cursor.CursorStatic) // steady cursor: no blink timer, instant + deterministic
	ti.Cursor.Style = th.Accent
	ti.SetValue(opts.InitialQuery)
	ti.CursorEnd()
	ti.Focus()

	m := Model{
		q:           q,
		opts:        opts,
		th:          th,
		ti:          ti,
		scope:       proto.ScopeLocal,
		dedupe:      true,
		vim:         opts.Keymap == "vim",
		width:       80,
		rowsVisible: maxRows,
	}
	m.applyLayout()
	return m
}

// Init kicks off the first query. It returns immediately; the first paint
// shows a placeholder and results arrive via queryResultMsg.
func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return initMsg{} }
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case initMsg:
		return m.issueQuery()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.applyLayout()
		m.clampWindow()
		return m, nil

	case queryResultMsg:
		return m.applyResult(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Anything else (e.g. cursor internals) goes to the text input.
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	return m, cmd
}

func (m Model) applyResult(msg queryResultMsg) (tea.Model, tea.Cmd) {
	if msg.seq <= m.appliedSeq {
		return m, nil // stale / out-of-order: discard
	}
	m.appliedSeq = msg.seq
	m.gotResult = true
	if msg.err != nil {
		m.lastErr = msg.err // keep the last good rows on screen
		return m, nil
	}
	m.lastErr = nil
	m.rows = msg.resp.Rows
	m.total = msg.resp.Total
	m.remote = msg.resp.Remote
	m.sel = 0
	m.top = 0
	m.clampWindow()
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Vim normal sub-mode swallows input with its own tiny binding set.
	if m.vim && m.normal {
		return m.handleNormalKey(msg)
	}

	switch msg.String() {
	case "esc":
		// Vim: the first Esc leaves insert/filter for the normal sub-mode; a
		// second Esc (handled in handleNormalKey) cancels. Emacs: cancel now.
		if m.vim {
			m.normal = true
			m.ti.Blur()
			return m, nil
		}
		m.cancel = true
		m.done = true
		return m, tea.Quit

	case "ctrl+c", "ctrl+g":
		m.cancel = true
		m.done = true
		return m, tea.Quit

	case "enter":
		if len(m.rows) > 0 {
			m.accepted = m.rows[m.sel].Cmd
		} else {
			m.accepted = m.ti.Value()
		}
		m.accept = true
		m.done = true
		return m, tea.Quit

	case "ctrl+r":
		m.scope = nextScope(m.scope)
		m.applyLayout()
		return m.issueQuery()

	case "alt+d":
		m.dedupe = !m.dedupe
		return m.issueQuery()

	case "alt+f":
		m.frecency = !m.frecency
		m.applyLayout()
		return m.issueQuery()

	case "alt+z":
		m.fuzzy = !m.fuzzy
		m.applyLayout()
		return m.issueQuery()

	case "up", "ctrl+p":
		m.moveSel(-1)
		return m, nil

	case "down", "ctrl+n":
		m.moveSel(1)
		return m, nil
	}

	// Typing / editing: let the text input handle it, then re-query if the
	// query text actually changed.
	prev := m.ti.Value()
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	if m.ti.Value() != prev {
		var qcmd tea.Cmd
		m, qcmd = m.issueQuery()
		return m, tea.Batch(cmd, qcmd)
	}
	return m, cmd
}

// handleNormalKey services the vim normal sub-mode. It is deliberately minimal:
// j/k (and ctrl+n/ctrl+p) navigate results, g/G jump, i/a// return to filtering,
// Enter accepts, and Esc/ctrl+c cancels. Unmapped keys are ignored so the
// filter text is never edited while in normal mode.
func (m Model) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "ctrl+g":
		m.cancel = true
		m.done = true
		return m, tea.Quit

	case "enter":
		if len(m.rows) > 0 {
			m.accepted = m.rows[m.sel].Cmd
		} else {
			m.accepted = m.ti.Value()
		}
		m.accept = true
		m.done = true
		return m, tea.Quit

	case "i", "a", "/":
		m.normal = false
		m.ti.Focus()
		return m, nil

	case "j", "down", "ctrl+n":
		m.moveSel(1)
		return m, nil

	case "k", "up", "ctrl+p":
		m.moveSel(-1)
		return m, nil

	case "g":
		m.sel, m.top = 0, 0
		m.clampWindow()
		return m, nil

	case "G":
		if len(m.rows) > 0 {
			m.sel = len(m.rows) - 1
		}
		m.clampWindow()
		return m, nil

	case "ctrl+r":
		m.scope = nextScope(m.scope)
		m.applyLayout()
		return m.issueQuery()
	}
	return m, nil
}

// issueQuery bumps the sequence counter and returns a command that runs the
// query asynchronously.
func (m Model) issueQuery() (Model, tea.Cmd) {
	m.seq++
	return m, m.queryCmd(m.seq, m.buildReq())
}

func (m Model) queryCmd(seq uint64, req proto.QueryReq) tea.Cmd {
	q := m.q
	return func() tea.Msg {
		resp, err := q.Query(req)
		return queryResultMsg{seq: seq, resp: resp, err: err}
	}
}

func (m Model) buildReq() proto.QueryReq {
	req := proto.QueryReq{
		Q:      m.ti.Value(),
		Scope:  m.scope,
		Limit:  queryLimit,
		Dedupe: m.dedupe,
	}
	if m.frecency {
		req.Sort = proto.SortFrecency
	}
	req.Fuzzy = m.fuzzy
	switch m.scope {
	case proto.ScopeSession:
		req.Session = m.opts.Session
	case proto.ScopeCwd, proto.ScopeWorkspace:
		req.Cwd = m.opts.Cwd
	}
	return req
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

// clampWindow keeps the selection inside the visible window.
func (m *Model) clampWindow() {
	rv := m.rowsVisible
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

// applyLayout recomputes the visible-row count and the input viewport width
// from the current terminal size and scope.
func (m *Model) applyLayout() {
	rv := maxRows
	if m.height > 0 {
		rv = m.height - 2 // input line + status line
		if rv < minRows {
			rv = minRows
		}
		if rv > maxRows {
			rv = maxRows
		}
	}
	m.rowsVisible = rv

	w := m.width
	if w < 1 {
		w = 80
	}
	// textinput's viewport always renders one extra column for the trailing
	// cursor cell, so reserve it to keep the whole input line within width.
	iw := w - m.promptWidth() - 1
	if iw < 4 {
		iw = 4
	}
	m.ti.Width = iw
}

func (m Model) promptWidth() int {
	w := runewidth.StringWidth("yore") + runewidth.StringWidth(" ❯ ")
	if l := scopeLabel(m.scope); l != "" {
		w += 1 + runewidth.StringWidth(l)
	}
	return w
}

// scopeLabel is the compact scope word shown in the prompt (empty for local).
func scopeLabel(s string) string {
	switch s {
	case proto.ScopeAll:
		return "all"
	case proto.ScopeSession:
		return "sess"
	case proto.ScopeCwd:
		return "cwd"
	case proto.ScopeWorkspace:
		return "repo"
	default:
		return ""
	}
}

// nextScope cycles local -> all -> session -> cwd -> workspace -> local.
func nextScope(s string) string {
	switch s {
	case proto.ScopeLocal:
		return proto.ScopeAll
	case proto.ScopeAll:
		return proto.ScopeSession
	case proto.ScopeSession:
		return proto.ScopeCwd
	case proto.ScopeCwd:
		return proto.ScopeWorkspace
	default:
		return proto.ScopeLocal
	}
}
