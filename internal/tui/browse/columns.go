package browse

// The TUI's three row tables (the browse view's results, the explorer's prompt
// list, and that prompt's command list) described once each. What a column is
// called, how wide it is, how it draws a cell and how it orders two rows all live
// in one spec, and the layout, the header, the row renderers and the columns pane
// are loops over those specs. Every one of them is the same shape underneath:
// some fixed metadata columns, then one flexible column at the end that takes
// what is left and carries the text (a command, or a prompt). That shape is what
// colTableDef captures, so the width arithmetic and the shedding rules are
// written once rather than three times in three slightly different ways.

import (
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/tui/theme"
)

// colTable names a table whose columns the user can reshape. Each keeps its own
// choices: hiding the host in the browse table says nothing about whether the
// prompt list should show one.
type colTable int

const (
	ctBrowse colTable = iota
	ctPrompts
	ctCommands
	ctDevices
	ctTokens
	colTableCount
)

// maxTableCols bounds the per-table column state so it can live in an array. The
// Model is copied on every update, and a map would be shared between the copies.
const maxTableCols = 8

// The browse table's columns, in render order.
const (
	colTime = iota
	colHost
	colExit
	colDur
	colExec
	colTags
	colCmd
)

// The explorer's prompt list.
const (
	pcWhen = iota
	pcHost
	pcSess
	pcExec
	pcCmds
	pcStat
	pcDur
	pcText
)

// The explorer's command list.
const (
	dcWhen = iota
	dcStat
	dcDur
	dcCmd
)

// The devices view's machine list.
const (
	mcStatus = iota
	mcID
	mcCode
	mcName
)

// The devices view's token list.
const (
	tcState = iota
	tcMinted
	tcExpires
	tcEnded
	tcClaimedBy
	tcID
)

// colSpec describes one column of a table whose rows are T.
type colSpec[T any] struct {
	title string
	width int  // 0 for the flexible column, which takes what is left
	right bool // right-aligned: the cells that read as quantities
	fixed bool // cannot be hidden
	flex  bool // takes the remaining width; exactly one per table

	// descFirst opens this column descending when it is first sorted on. It is
	// the recency columns: "newest first" is what a reader means by sorting on
	// when something happened. A deadline is the exception: the soonest to run
	// out is the interesting end of that one.
	descFirst bool

	// cell renders this column's value for one row. The flexible column has none:
	// its text carries match highlighting and horizontal scroll, which a single
	// string and style cannot express, so each table draws it itself.
	cell func(th *theme.Theme, row T, now int64) (string, lipgloss.Style)

	// less orders two rows by this column, ascending. nil means it cannot sort.
	less func(a, b T) bool

	// gate reports whether the rows have anything to put in this column. nil is
	// always. why explains a closed gate in the columns pane, where a column that
	// the data switched off would otherwise look like one the user switched off.
	gate func(Model) bool
	why  string
}

// colMeta is the part of a spec that does not depend on the row type: all the
// layout, the header and the columns pane ever need.
type colMeta struct {
	title     string
	width     int
	right     bool
	fixed     bool
	flex      bool
	descFirst bool
	sortable  bool
	gate      func(Model) bool
	why       string
}

func metasOf[T any](specs []colSpec[T]) []colMeta {
	out := make([]colMeta, len(specs))
	for i, s := range specs {
		out[i] = colMeta{
			title: s.title, width: s.width, right: s.right, fixed: s.fixed,
			flex: s.flex, descFirst: s.descFirst, sortable: s.less != nil,
			gate: s.gate, why: s.why,
		}
	}
	return out
}

// --- the browse table ----------------------------------------------------

// Executor and tags are two columns, never one. They answer different questions
// (which agent ran this, versus what did I label it) and merging them meant a user
// tag competed for cells with "claude-code" on every agent row.
var browseSpecs = []colSpec[rec.Record]{
	{
		title: "time", width: 8, right: true, descFirst: true,
		cell: func(th *theme.Theme, r rec.Record, now int64) (string, lipgloss.Style) {
			return theme.RelTime(now, r.StartMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return a.StartMs < b.StartMs },
	},
	{
		title: "host", width: 14,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return r.Hostname, th.Host(r.Hostname)
		},
		less: func(a, b rec.Record) bool { return a.Hostname < b.Hostname },
		// Scope-all only: with one host selected every cell would repeat its name.
		gate: func(m Model) bool { return m.hosts[m.hostSel].scope == proto.ScopeAll },
		why:  "one host in scope",
	},
	{
		title: "exit", width: 4,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) { return exitMarker(th, r) },
		less: func(a, b rec.Record) bool { return exitRank(a) < exitRank(b) },
	},
	{
		title: "dur", width: 7, right: true,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			if r.DurMs == nil {
				return "", th.Dim
			}
			return theme.Duration(*r.DurMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return durRank(a) < durRank(b) },
	},
	{
		title: "exec", width: 12,
		// Metadata, so Dim: the same weight as the host and duration cells, and
		// deliberately not the accent the user's own tags get. Blank for a command
		// the user typed.
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) { return r.Executor, th.Dim },
		less: func(a, b rec.Record) bool { return a.Executor < b.Executor },
		gate: func(m Model) bool { return m.hasExec },
		why:  "no agent commands here",
	},
	{
		title: "tags", width: 12,
		// The user's own labels, in the accent: they are the one thing in the row
		// that is there because somebody put it there.
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			return strings.Join(r.Tags, ","), th.Accent
		},
		less: func(a, b rec.Record) bool { return strings.Join(a.Tags, ",") < strings.Join(b.Tags, ",") },
		gate: func(m Model) bool { return m.hasTags },
		why:  "nothing tagged here",
	},
	{
		title: "command", flex: true, fixed: true,
		less: func(a, b rec.Record) bool { return a.Cmd < b.Cmd },
	},
}

// --- the explorer's prompt list ------------------------------------------

var promptSpecs = []colSpec[promptStat]{
	{
		title: "when", width: 7,
		cell: func(th *theme.Theme, p promptStat, now int64) (string, lipgloss.Style) {
			return theme.RelTime(now, p.lastMs), th.Dim
		},
		less: func(a, b promptStat) bool { return a.lastMs < b.lastMs },
	},
	{
		title: "host", width: 10,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) {
			return p.host, th.Host(p.host)
		},
		less: func(a, b promptStat) bool { return a.host < b.host },
		gate: func(m Model) bool { return m.showPromptHost() },
		why:  "one host in this view",
	},
	{
		title: "session", width: 8,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) {
			return shortSession(p.session), th.Dim
		},
		less: func(a, b promptStat) bool { return a.session < b.session },
	},
	{
		title: "executor", width: 12,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) {
			if p.executor == "" {
				return "", th.Dim
			}
			return p.executor, th.Host(p.executor)
		},
		less: func(a, b promptStat) bool { return a.executor < b.executor },
	},
	{
		title: "cmds", width: 4, right: true,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) {
			return strconv.Itoa(p.count), th.Norm
		},
		less: func(a, b promptStat) bool { return a.count < b.count },
	},
	{
		title: "status", width: 6,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) { return promptStatus(th, p) },
		// Worst outcome first when descending: the reason to sort on this column is
		// to find what went wrong.
		less: func(a, b promptStat) bool { return a.failures < b.failures },
	},
	{
		title: "dur", width: 7, right: true,
		cell: func(th *theme.Theme, p promptStat, _ int64) (string, lipgloss.Style) {
			return promptDur(p), th.Dim
		},
		less: func(a, b promptStat) bool { return a.durSum < b.durSum },
		gate: func(m Model) bool { return m.prompts != nil && m.prompts.hasDur },
		why:  "no agent reports timing",
	},
	{
		title: "prompt", flex: true, fixed: true,
		less: func(a, b promptStat) bool { return a.text < b.text },
	},
}

// --- the explorer's command list -----------------------------------------

var drillSpecs = []colSpec[rec.Record]{
	{
		title: "when", width: 7, right: true,
		cell: func(th *theme.Theme, r rec.Record, now int64) (string, lipgloss.Style) {
			return theme.RelTime(now, r.StartMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return a.StartMs < b.StartMs },
	},
	{
		// 4 wide, matching the browse table's exit column ("✗255" is the widest
		// marker), so the same value is the same width in both tables.
		title: "st", width: 4,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) { return exitMarker(th, r) },
		less: func(a, b rec.Record) bool { return exitRank(a) < exitRank(b) },
	},
	{
		title: "dur", width: 7, right: true,
		cell: func(th *theme.Theme, r rec.Record, _ int64) (string, lipgloss.Style) {
			if r.DurMs == nil {
				return theme.Unknown, th.Dim // this agent didn't report timing, though a sibling did
			}
			return theme.Duration(*r.DurMs), th.Dim
		},
		less: func(a, b rec.Record) bool { return durRank(a) < durRank(b) },
		gate: func(m Model) bool { return m.prompts != nil && m.prompts.hasDur },
		why:  "no agent reports timing",
	},
	{
		title: "command", flex: true, fixed: true,
		less: func(a, b rec.Record) bool { return a.Cmd < b.Cmd },
	},
}

// --- the devices view's machine list -------------------------------------

// deviceRank orders machines by what they need from you: a machine waiting to be
// approved first, then the working ones, then the ones that were thrown out. The
// raw status strings would order them "active, pending, revoked" alphabetically,
// which buries the only row that is asking for anything.
func deviceRank(d proto.DeviceInfo) int {
	switch d.Status {
	case "pending":
		return 0
	case "active":
		return 1
	default:
		return 2
	}
}

func deviceStatusStyle(th *theme.Theme, d proto.DeviceInfo) lipgloss.Style {
	switch d.Status {
	case "active":
		return th.ExitOK
	case "pending":
		return th.Match
	default:
		return th.Dim
	}
}

var deviceSpecs = []colSpec[proto.DeviceInfo]{
	{
		title: "status", width: 8,
		cell: func(th *theme.Theme, d proto.DeviceInfo, _ int64) (string, lipgloss.Style) {
			return d.Status, deviceStatusStyle(th, d)
		},
		less: func(a, b proto.DeviceInfo) bool { return deviceRank(a) < deviceRank(b) },
	},
	{
		title: "id", width: 12,
		cell: func(th *theme.Theme, d proto.DeviceInfo, _ int64) (string, lipgloss.Style) {
			return shortID(d.ID), th.Dim
		},
		less: func(a, b proto.DeviceInfo) bool { return a.ID < b.ID },
	},
	{
		// 29 wide: the whole verification code, six groups of four (see
		// syncer.VerificationCode). A truncated one is worse than none: it is here
		// to be compared character by character against the code the other machine
		// is showing.
		title: "code", width: 29,
		// The verification code, which only a pending machine has. It is shown here
		// as well as in the approval prompt so the codes can be compared before the
		// prompt is even raised.
		cell: func(th *theme.Theme, d proto.DeviceInfo, _ int64) (string, lipgloss.Style) {
			return d.Code, th.Match
		},
		less: func(a, b proto.DeviceInfo) bool { return a.Code < b.Code },
		gate: func(m Model) bool {
			for _, d := range m.devices {
				if d.Code != "" {
					return true
				}
			}
			return false
		},
		why: "no machine is waiting to be approved",
	},
	{
		// The flexible column: a hostname is the one field here with no bound on its
		// length, and it carries the "(this machine)" marker, which the renderer
		// appends (see deviceRow).
		title: "machine", flex: true, fixed: true,
		less: func(a, b proto.DeviceInfo) bool { return a.Name < b.Name },
	},
}

// --- the devices view's token list ---------------------------------------

// tokenRank orders tokens by whether they can still admit a machine: the open
// ones first, since those are the ones an action (hand it over, or cancel it)
// can still be taken on.
func tokenRank(t proto.EnrollToken) int {
	switch t.State {
	case proto.TokenOpen:
		return 0
	case proto.TokenClaimed:
		return 1
	case proto.TokenRevoked:
		return 2
	default:
		return 3
	}
}

func tokenStateStyle(th *theme.Theme, t proto.EnrollToken) lipgloss.Style {
	switch t.State {
	case proto.TokenOpen:
		return th.Match // still live: the one state that is a standing invitation
	case proto.TokenClaimed:
		return th.ExitOK
	case proto.TokenRevoked:
		return th.ExitErr
	default:
		return th.Dim
	}
}

// tokenEndedMs is when a token stopped being open; claimed or cancelled. Zero
// while it is still open, or if it simply ran out (that time is its expiry).
func tokenEndedMs(t proto.EnrollToken) int64 {
	if t.ClaimedMs > 0 {
		return t.ClaimedMs
	}
	return t.RevokedMs
}

var tokenSpecs = []colSpec[proto.EnrollToken]{
	{
		title: "state", width: 8,
		cell: func(th *theme.Theme, t proto.EnrollToken, _ int64) (string, lipgloss.Style) {
			return t.State, tokenStateStyle(th, t)
		},
		less: func(a, b proto.EnrollToken) bool { return tokenRank(a) < tokenRank(b) },
	},
	{
		title: "minted", width: 7, right: true, descFirst: true,
		cell: func(th *theme.Theme, t proto.EnrollToken, now int64) (string, lipgloss.Style) {
			return theme.RelTime(now, t.CreatedMs), th.Dim
		},
		less: func(a, b proto.EnrollToken) bool { return a.CreatedMs < b.CreatedMs },
	},
	{
		// How long this token has LEFT: the one thing anyone wants to know about an
		// open one. Forward-looking, so it counts down (see theme.TimeLeft); a token
		// that is no longer open has nothing left to count, and says so.
		title: "expires", width: 7, right: true,
		cell: func(th *theme.Theme, t proto.EnrollToken, now int64) (string, lipgloss.Style) {
			if t.State != proto.TokenOpen {
				return theme.Unknown, th.Dim
			}
			return theme.TimeLeft(now, t.ExpiresMs), th.Dim
		},
		less: func(a, b proto.EnrollToken) bool { return a.ExpiresMs < b.ExpiresMs },
	},
	{
		title: "ended", width: 7, right: true, descFirst: true,
		cell: func(th *theme.Theme, t proto.EnrollToken, now int64) (string, lipgloss.Style) {
			ms := tokenEndedMs(t)
			if ms == 0 {
				return theme.Unknown, th.Dim
			}
			return theme.RelTime(now, ms), th.Dim
		},
		less: func(a, b proto.EnrollToken) bool { return tokenEndedMs(a) < tokenEndedMs(b) },
		gate: func(m Model) bool {
			for _, t := range m.tokens {
				if tokenEndedMs(t) > 0 {
					return true
				}
			}
			return false
		},
		why: "no token has been used or cancelled",
	},
	{
		title: "claimed by", width: 12,
		cell: func(th *theme.Theme, t proto.EnrollToken, _ int64) (string, lipgloss.Style) {
			if t.ClaimedBy == "" {
				return theme.Unknown, th.Dim
			}
			return shortID(t.ClaimedBy), th.Dim
		},
		less: func(a, b proto.EnrollToken) bool { return a.ClaimedBy < b.ClaimedBy },
		gate: func(m Model) bool {
			for _, t := range m.tokens {
				if t.ClaimedBy != "" {
					return true
				}
			}
			return false
		},
		why: "no token has admitted a machine",
	},
	{
		// The flexible column, so a wide pane shows more of the id. It is the hash of
		// the token, never the token: the server keeps no plaintext, so the only
		// moment the token itself exists is the banner above this list.
		title: "token", flex: true, fixed: true,
		less: func(a, b proto.EnrollToken) bool { return a.ID < b.ID },
	},
}

// --- the tables ----------------------------------------------------------

// colTableDef is a table's shared geometry and defaults: its columns, the gap
// between cells, the room its flexible column must keep, the order columns give
// up width in when the pane is too narrow, and the order it opens in.
type colTableDef struct {
	key       string // its name in ui.toml; stable, and not the display label
	name      string // what the columns pane calls it
	metas     []colMeta
	sep       int // columns of gap between cells
	flexFloor int // the flexible column never renders narrower than this
	shed      []shedStep
	sortCol   int
	sortDesc  bool
}

// shedStep is one column's turn to give up its width, and the room the flexible
// column must already have for its turn to be skipped.
type shedStep struct {
	c    int
	keep int
}

// colTables is every reshapeable table. The shed orders are each table's own: the
// browse table gives up the executor before the tag because the executor also
// appears in the details pane, the explorer, and stats, whereas a user tag is
// shown nowhere else; the prompt list gives up the session first because it is the
// column a reader can most easily do without.
var colTables = [colTableCount]colTableDef{
	ctBrowse: {
		key: "browse", name: "browse table", metas: metasOf(browseSpecs), sep: 1, flexFloor: 0,
		shed: []shedStep{
			{colExec, 5}, {colTags, 5}, {colDur, 5}, {colHost, 5}, {colExit, 5}, {colTime, 3},
		},
		sortCol: colTime, sortDesc: true,
	},
	ctPrompts: {
		key: "prompts", name: "prompt list", metas: metasOf(promptSpecs), sep: 2, flexFloor: 8,
		shed:    []shedStep{{pcSess, 12}, {pcHost, 12}, {pcDur, 12}, {pcExec, 12}},
		sortCol: pcWhen, sortDesc: true,
	},
	ctCommands: {
		// Oldest first, and it stays that way by default: this is the agent's
		// actual working order, which is the reason to look at the list at all.
		key: "commands", name: "command list", metas: metasOf(drillSpecs), sep: 2, flexFloor: 12,
		shed:    []shedStep{{dcDur, 12}},
		sortCol: dcWhen, sortDesc: false,
	},
	ctDevices: {
		// By status ascending, which deviceRank defines as "whatever needs you
		// first": the machine waiting for approval opens at the top of the list.
		key: "machines", name: "machine list", metas: metasOf(deviceSpecs), sep: 2, flexFloor: 12,
		shed:    []shedStep{{mcID, 16}, {mcCode, 16}},
		sortCol: mcStatus, sortDesc: false,
	},
	ctTokens: {
		// Newest first: the token you just minted is the one you are looking at the
		// list to find.
		key: "tokens", name: "token list", metas: metasOf(tokenSpecs), sep: 2, flexFloor: 12,
		shed:    []shedStep{{tcEnded, 16}, {tcClaimedBy, 16}, {tcExpires, 16}, {tcMinted, 16}},
		sortCol: tcMinted, sortDesc: true,
	},
}

// colState is one table's column choices.
type colState struct {
	hidden   [maxTableCols]bool
	sortCol  int
	sortDesc bool
}

// defaultColStates is what every table opens on.
func defaultColStates() [colTableCount]colState {
	var out [colTableCount]colState
	for t := range colTableCount {
		out[t] = colState{sortCol: colTables[t].sortCol, sortDesc: colTables[t].sortDesc}
	}
	return out
}

// colIndex finds a table's column by name, reporting whether it exists. Names are
// what ui.toml stores, so this is also the tolerance for a file written by another
// version: a column that no longer exists is simply not found.
func colIndex(t colTable, name string) (int, bool) {
	for i, meta := range colTables[t].metas {
		if meta.title == name {
			return i, true
		}
	}
	return 0, false
}

// colStatesFrom restores remembered column choices, starting from the defaults so
// anything the file does not mention keeps them. It is fail-safe by construction:
// a column name this build does not have is ignored, a sort naming a column that
// cannot sort is dropped, and a hidden entry for a column that must always show is
// refused, so a stale or hand-mangled ui.toml can never leave a table unusable,
// only unremembered.
func colStatesFrom(prefs map[string]ColumnPrefs) [colTableCount]colState {
	out := defaultColStates()
	for t := range colTableCount {
		p, ok := prefs[colTables[t].key]
		if !ok {
			continue
		}
		for _, name := range p.Hidden {
			if i, found := colIndex(t, name); found && !colTables[t].metas[i].fixed {
				out[t].hidden[i] = true
			}
		}
		if i, found := colIndex(t, p.Sort); found && colTables[t].metas[i].sortable {
			out[t].sortCol, out[t].sortDesc = i, p.SortDesc
		}
	}
	return out
}

// colPrefsOf is the inverse: every table's choices, by name, ready to write. A
// table left at its defaults contributes an empty entry that omitempty drops, so
// an untouched ui.toml grows no columns section at all.
func colPrefsOf(states [colTableCount]colState) map[string]ColumnPrefs {
	out := make(map[string]ColumnPrefs, colTableCount)
	for t := range colTableCount {
		var p ColumnPrefs
		for i, meta := range colTables[t].metas {
			if states[t].hidden[i] {
				p.Hidden = append(p.Hidden, meta.title)
			}
		}
		// Only a non-default order is worth writing down; the default is whatever
		// this build says it is, which is the more useful thing to inherit.
		if states[t].sortCol != colTables[t].sortCol || states[t].sortDesc != colTables[t].sortDesc {
			p.Sort = colTables[t].metas[states[t].sortCol].title
			p.SortDesc = states[t].sortDesc
		}
		if len(p.Hidden) == 0 && p.Sort == "" {
			continue
		}
		out[colTables[t].key] = p
	}
	return out
}

// sortedByDefault reports whether a table is in its opening order, which is the
// one order the UI does not bother naming.
func (m Model) sortedByDefault(t colTable) bool {
	return m.cols[t].sortCol == colTables[t].sortCol &&
		m.cols[t].sortDesc == colTables[t].sortDesc
}

// hiddenColCount is how many of a table's columns the user has switched off. The
// UI says so: a column missing because it was turned off looks exactly like one
// missing because the pane is narrow, and only one of those is worth explaining.
func (m Model) hiddenColCount(t colTable) int {
	n := 0
	for i := range colTables[t].metas {
		if m.cols[t].hidden[i] {
			n++
		}
	}
	return n
}

// exitRank orders exit status: unknown below everything, then 0, then failures by
// code. Sorting on the raw code would file a still-running command with the
// successes, which is the one thing that column exists to keep apart.
func exitRank(r rec.Record) int {
	if r.Exit == nil {
		return -1
	}
	return *r.Exit
}

// durRank sorts an unknown duration below every known one, the same way exitRank
// treats an unknown outcome: a command whose timing was never reported is not a
// fast command.
func durRank(r rec.Record) int64 {
	if r.DurMs == nil {
		return -1
	}
	return *r.DurMs
}

// --- layout --------------------------------------------------------------

// colLayout is every column's resolved width for this frame. 0 means the column
// is not on screen; switched off, empty for these rows, or shed to keep the
// flexible column readable.
type colLayout struct {
	t colTable
	w [maxTableCols]int
}

func (l colLayout) shows(c int) bool { return l.w[c] > 0 }

// flexW is the width the flexible column ended up with.
func (l colLayout) flexW() int {
	for i, meta := range colTables[l.t].metas {
		if meta.flex {
			return l.w[i]
		}
	}
	return 0
}

// prefix is the width everything before the flexible column occupies, each cell
// plus its separator.
func (l colLayout) prefix() int {
	def := colTables[l.t]
	p := 0
	for i, meta := range def.metas {
		if !meta.flex && l.w[i] > 0 {
			p += l.w[i] + def.sep
		}
	}
	return p
}

// colVisible reports whether a column has something to say for the rows on screen
// and has not been switched off in the columns pane. Width shedding is applied
// after this, in tableLayout.
//
// The data gates are one-way: they can hide a column that would be blank on every
// row, but the pane cannot force one back on. A column of empty cells costs width
// and tells you nothing, and "no row here has an executor" is already the answer.
func (m Model) colVisible(t colTable, c int) bool {
	if m.cols[t].hidden[c] {
		return false
	}
	if g := colTables[t].metas[c].gate; g != nil {
		return g(m)
	}
	return true
}

// tableLayout distributes a pane's content width across one table's visible
// columns, shedding the least-missed ones until the flexible column fits, so a row
// never overflows and the text always keeps room.
func (m Model) tableLayout(t colTable, w int) colLayout {
	def := colTables[t]
	l := colLayout{t: t}
	for i, meta := range def.metas {
		if !meta.flex && m.colVisible(t, i) {
			l.w[i] = meta.width
		}
	}
	for _, s := range def.shed {
		if l.prefix()+s.keep <= w {
			break
		}
		l.w[s.c] = 0
	}
	for i, meta := range def.metas {
		if meta.flex {
			l.w[i] = maxInt(def.flexFloor, w-l.prefix())
		}
	}
	return l
}

// colLayout is the browse table's layout at its own pane width, less the
// selection gutter every row leads with (see selGutterW): that glyph column
// is drawn outside this layout entirely, by the browse table's own header and
// row renderers, so the columns here must add up to tableWidth minus it for
// the two to line up under one another.
func (m Model) colLayout() colLayout {
	return m.tableLayout(ctBrowse, maxInt(1, m.tableWidth-selGutterW))
}

// --- header --------------------------------------------------------------

// tableHeaderSegs names each visible column, with the sort arrow on the one the
// table is ordered by, where the cell is wide enough to hold it. A narrow column
// leaves the arrow to the status line rather than truncating its own name to make
// room for a glyph.
func (m Model) tableHeaderSegs(l colLayout) []styledSeg {
	def := colTables[l.t]
	th := m.th
	var segs []styledSeg
	for i, meta := range def.metas {
		width := l.w[i]
		if width <= 0 {
			continue
		}
		title := meta.title
		if m.cols[l.t].sortCol == i && runewidth.StringWidth(title)+1 <= width {
			title += sortArrow(m.cols[l.t].sortDesc)
		}
		segs = append(segs, styledSeg{text: alignCell(title, width, meta.right), style: th.Dim})
		if !meta.flex {
			segs = append(segs, styledSeg{text: strings.Repeat(" ", def.sep), raw: true})
		}
	}
	return segs
}

// alignCell pads text into a cell of exactly width columns, right-aligned for the
// quantities and left-aligned (truncating) for the names.
func alignCell(text string, width int, right bool) string {
	if right {
		return padLeft(text, width)
	}
	return padRight(truncCols(text, width), width)
}

// rowSegs draws every visible fixed cell of one row, leaving the flexible column
// to the caller: its text carries highlighting and horizontal scroll that a
// single string cannot express.
func rowSegs[T any](th *theme.Theme, t colTable, specs []colSpec[T], l colLayout, row T, now int64) []styledSeg {
	def := colTables[t]
	var segs []styledSeg
	for i, s := range specs {
		if s.flex || l.w[i] <= 0 {
			continue
		}
		text, style := s.cell(th, row, now)
		segs = append(segs,
			styledSeg{text: alignCell(text, l.w[i], s.right), style: style},
			styledSeg{text: strings.Repeat(" ", def.sep), raw: true},
		)
	}
	return segs
}

// --- sorting -------------------------------------------------------------

// sortArrow marks the sorted column's header and names the direction in the
// status line. A glyph, not a color: the tables' other distinctions are all
// legible without seeing hue, and this one has no reason to be the exception.
func sortArrow(desc bool) string {
	if desc {
		return "▼"
	}
	return "▲"
}

// sortRows orders a slice by one table's chosen column. It is stable, and every
// caller hands it a slice already in the table's natural order, so that order
// stays the tiebreak underneath whatever was asked for on top; equal durations
// keep their time order.
func sortRows[T any](specs []colSpec[T], st colState, rows []T) {
	if st.sortCol < 0 || st.sortCol >= len(specs) {
		return
	}
	less := specs[st.sortCol].less
	if less == nil {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if st.sortDesc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
}

// sortBy points a table at a column, or reverses it when it is already the sorted
// one, so the key that chooses a column is also the key that flips it, and there
// is no second binding to remember.
func (m Model) sortBy(t colTable, c int) (Model, bool) {
	if !colTables[t].metas[c].sortable {
		return m, false
	}
	if m.cols[t].sortCol == c {
		m.cols[t].sortDesc = !m.cols[t].sortDesc
	} else {
		// A recency column reads newest-first and everything else smallest-first;
		// those are the directions each kind is usually wanted in.
		m.cols[t].sortCol, m.cols[t].sortDesc = c, colTables[t].metas[c].descFirst
	}
	m.reorder(t)
	return m, true
}

// reorder re-sorts whichever list the table owns and keeps the cursor on the row
// it was on. Re-sorting moves a row's index but not the user's place in their own
// history.
func (m *Model) reorder(t colTable) {
	switch t {
	case ctBrowse:
		var selID string
		if m.sel >= 0 && m.sel < len(m.rows) {
			selID = m.rows[m.sel].ID
		}
		sortRows(browseSpecs, m.cols[ctBrowse], m.rows)
		if selID != "" {
			for i, r := range m.rows {
				if r.ID == selID {
					m.sel = i
					break
				}
			}
		}
		m.clampWindow()
		m.syncDetail()
	case ctPrompts:
		var selID string
		if p, ok := m.drilledPrompt(); ok {
			selID = p.id
		}
		// Through applyPromptFilter, not a sort in place: unfiltered, the list IS
		// prompts.prompts, and applyPromptFilter is the one place that knows to copy
		// before reordering it.
		m.applyPromptFilter()
		for i, p := range m.filteredPrompts {
			if p.id == selID {
				m.promptSel = i
				break
			}
		}
		m.clampPrompts()
	case ctCommands:
		// The command list is derived on demand by visibleCmds, which sorts what it
		// returns, so there is nothing held to re-sort, only a cursor to rescue.
		var selID string
		if r, ok := m.drilledCmd(); ok {
			selID = r.ID
		}
		for i, r := range m.visibleCmds() {
			if r.ID == selID {
				m.drillSel = i
				break
			}
		}
		m.infoTop = 0
	case ctDevices:
		var selID string
		if d, ok := m.selectedDevice(); ok {
			selID = d.ID
		}
		m.sortDevices()
		for i, d := range m.devices {
			if d.ID == selID {
				m.devSel = i
				break
			}
		}
	case ctTokens:
		var selID string
		if t, ok := m.selectedToken(); ok {
			selID = t.ID
		}
		m.sortTokens()
		for i, t := range m.tokens {
			if t.ID == selID {
				m.tokSel = i
				break
			}
		}
	}
}

// sortDevices and sortTokens put the chosen column's order on top of the order
// the daemon listed them in. Both lists are re-sorted wherever they are replaced,
// so a background refresh cannot quietly restore the server's order under a
// cursor the user placed.
func (m *Model) sortDevices() { sortRows(deviceSpecs, m.cols[ctDevices], m.devices) }

func (m *Model) sortTokens() { sortRows(tokenSpecs, m.cols[ctTokens], m.tokens) }

// --- the columns pane ----------------------------------------------------

// colTarget is the table the columns pane reshapes: in the explorer and the
// devices view, whichever list pane has focus (the explorer's sidebar and details
// pane have no columns of their own, so they aim at the prompt list the view
// hangs off; the same rule / uses); everywhere else, the browse table.
func (m Model) colTarget() colTable {
	switch m.view {
	case viewAgents:
		if m.apane == apCommands {
			return ctCommands
		}
		return ctPrompts
	case viewDevices:
		if m.dpane == dpTokens {
			return ctTokens
		}
		return ctDevices
	}
	return ctBrowse
}

// toggleColumns raises or drops the columns pane (the c key). The cursor stays
// where it was left: switching several columns off is a handful of trips through
// this pane, and resetting it each time would make the second trip longer than the
// first.
func (m Model) toggleColumns() (tea.Model, tea.Cmd) {
	m.showCols = !m.showCols
	if m.showCols {
		m.colTable = m.colTarget()
		m.colSel = clampIndex(m.colSel, len(colTables[m.colTable].metas))
	}
	return m, nil
}

// handleColumnsKey services the columns pane. It owns its keys outright while it
// is up, like the key panel: it covers the table it reshapes, so that table's own
// keys must not fire through it.
func (m Model) handleColumnsKey(s string) (tea.Model, tea.Cmd) {
	n := len(colTables[m.colTable].metas)
	switch s {
	case "esc", "c", "q":
		m.showCols = false
		return m, nil
	case "?":
		// This pane runs before the global keys, so ? has to be handled here too;
		// otherwise the pane with the least guessable keys is the one pane that
		// cannot show you what they are.
		return m.openHelp()
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		m.colSel = clampIndex(m.colSel-1, n)
		return m, nil
	case "down", "j":
		m.colSel = clampIndex(m.colSel+1, n)
		return m, nil
	case "g", "home":
		m.colSel = 0
		return m, nil
	case "G", "end":
		m.colSel = n - 1
		return m, nil
	case " ", "x":
		return m.toggleColumn(m.colTable, m.colSel)
	case "s", "enter":
		mm, ok := m.sortBy(m.colTable, m.colSel)
		title := colTables[m.colTable].metas[m.colSel].title
		if !ok {
			return m.flashOnly(title + " cannot be sorted on")
		}
		return mm.remember("sort: " + title + " " + sortArrow(mm.cols[mm.colTable].sortDesc))
	}
	return m, nil
}

// remember flashes what changed and writes the choice to ui.toml. Every column
// change goes through it, so there is one place that cannot forget to persist.
func (m Model) remember(note string) (tea.Model, tea.Cmd) {
	m.flash = note
	m.flashID++
	return m, tea.Batch(flashTick(m.flashID), m.savePrefsCmd())
}

// toggleColumn switches one column off or back on. The flexible column stays: a
// table of metadata with no commands or prompts in it is not a history.
func (m Model) toggleColumn(t colTable, c int) (tea.Model, tea.Cmd) {
	meta := colTables[t].metas[c]
	if meta.fixed {
		return m.flashOnly(meta.title + " cannot be hidden")
	}
	m.cols[t].hidden[c] = !m.cols[t].hidden[c]
	m.applyLayout() // the flexible column just gained or lost that width
	m.syncDetail()
	if m.cols[t].hidden[c] {
		return m.remember(meta.title + " hidden")
	}
	return m.remember(meta.title + " shown")
}

// colStateNote says why a column is not on screen when the answer is not "you
// switched it off": a gate the rows themselves closed, or a width the pane does
// not have. Without it a column can read as switched off when it is not, and
// pressing space on it would appear to do nothing.
func (m Model) colStateNote(t colTable, c int, l colLayout) string {
	switch {
	case m.cols[t].hidden[c]:
		return ""
	case !m.colVisible(t, c):
		if why := colTables[t].metas[c].why; why != "" {
			return why
		}
		return "nothing to show"
	case !l.shows(c):
		return "no room at this width"
	}
	return ""
}

// renderColumns draws the columns pane: every column of the table it is aimed at,
// whether that column is on, and which one the table is ordered by. It takes over
// the middle of the frame like the key panel, keeping the header and status bar so
// the view it belongs to is still named.
func (m Model) renderColumns(w, h int) string {
	th := m.th
	t := m.colTable
	def := colTables[t]
	bw, bh := maxInt(1, w-2), maxInt(1, h-2)
	l := m.paneLayout(t, bw)

	lines := make([]string, 0, len(def.metas))
	for i, meta := range def.metas {
		box := "[x]"
		switch {
		case meta.fixed:
			box = " · " // not a choice, so not drawn as one
		case m.cols[t].hidden[i]:
			box = "[ ]"
		}
		sorted := "  "
		if m.cols[t].sortCol == i {
			sorted = sortArrow(m.cols[t].sortDesc) + " "
		}
		text := box + " " + sorted + padRight(truncCols(meta.title, 10), 10)
		if note := m.colStateNote(t, i, l); note != "" {
			text += "  " + note
		}
		cursor, style := "  ", th.Norm
		if i == m.colSel {
			cursor, style = th.Accent.Render("❯ "), th.Sel
		}
		lines = append(lines, padTo(cursor+style.Render(fitPlain(text, maxInt(1, bw-2))), bw))
	}
	return m.titledBox(true, bw, bh, "COLUMNS", def.name, padLines(lines, bw, bh))
}

// paneLayout is a table's layout at the width of the pane that shows it, so the
// columns pane can report "no room at this width" about the pane the user is
// actually looking at rather than about itself.
func (m Model) paneLayout(t colTable, fallback int) colLayout {
	switch t {
	case ctBrowse:
		return m.colLayout() // shares colLayout's selection-gutter adjustment
	case ctPrompts:
		return m.tableLayout(ctPrompts, maxInt(1, m.geo.p[apPrompts].w-2))
	case ctCommands:
		return m.tableLayout(ctCommands, maxInt(1, m.geo.p[apCommands].w-2))
	case ctDevices:
		return m.tableLayout(ctDevices, maxInt(1, m.geo.p[dpDevices].w-2))
	case ctTokens:
		return m.tableLayout(ctTokens, maxInt(1, m.geo.p[dpTokens].w-2))
	}
	return m.tableLayout(t, fallback)
}

// listSuffix appends a table's reshaping state to its pane title. The explorer's
// panes have no status bar of their own, and a list in an unexpected order or
// missing a column has to say so on the pane that holds it; otherwise "oldest
// first" silently becomes "sorted by duration" and the reader has no way to tell.
func (m Model) listSuffix(t colTable, base string) string {
	parts := make([]string, 0, 3)
	if base != "" {
		parts = append(parts, base)
	}
	if note := m.sortNote(t); note != "" {
		parts = append(parts, note)
	}
	if n := m.hiddenColCount(t); n > 0 {
		parts = append(parts, plural(n, "col")+" hidden")
	}
	return strings.Join(parts, "  ")
}

// sortNote names a table's order for a pane title or the status bar, in words:
// a four-column header has no room for an arrow, and a distinction carried by
// color alone is no distinction. Empty while the table is in its opening order,
// which needs no explaining.
func (m Model) sortNote(t colTable) string {
	if m.sortedByDefault(t) {
		return ""
	}
	return "sort: " + colTables[t].metas[m.cols[t].sortCol].title + " " + sortArrow(m.cols[t].sortDesc)
}
