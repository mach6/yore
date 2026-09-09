package browse

import (
	"yore/internal/tui/keyhelp"
)

// The browser's bindings, described once. Behavior is dispatched from handleKey
// on the raw key string (so tests can synthesize keys directly); these tables are
// what the footer and the "?" panel show, and each row carries the raw keys it
// stands for so a test can hold the two to each other: a key that does something
// but is described nowhere is a bug, and so is a key described but not handled.
// The tables are contextual: a view advertises the keys that do something *in
// that view*, in the state it is in. The devices pane, for one, cannot reach the
// global keys at all (handleDevicesKey runs before them), so it must not claim
// them; the stats screen has nothing to zoom, so it does not offer z.

// row is the terse constructor for a binding: how it is shown, what it does, and
// the raw keys behind it.
func row(keys, desc string, keyed ...string) keyhelp.Row {
	return keyhelp.Row{Keys: keys, Desc: desc, Keyed: keyed}
}

// agentToggleDesc says what A will do next, not what it is for: a control that
// reads "show agents" when they are hidden tells the user both the current state
// and the outcome of pressing it, which one static label cannot.
func (m Model) agentToggleDesc() string {
	if m.hideAgents {
		return "show agent commands"
	}
	return "hide agent commands"
}

// mouseRow describes a pointer gesture. It carries no keys, so the coverage test
// skips it, but it belongs in the panel: the panes are resizable by drag and
// nothing else on screen says so.
func mouseRow(gesture, desc string) keyhelp.Row {
	return keyhelp.Row{Keys: gesture, Desc: desc}
}

// --- the full "?" panel --------------------------------------------------

// helpGroups is every binding that does something in the current view.
func (m Model) helpGroups() []keyhelp.Group {
	if m.showCols {
		return columnsGroups()
	}
	switch m.view {
	case viewStats:
		return m.statsGroups()
	case viewAgents:
		return m.agentsGroups()
	case viewDevices:
		return devicesGroups()
	default:
		return m.browseGroups()
	}
}

func (m Model) browseGroups() []keyhelp.Group {
	move := []keyhelp.Row{
		row("↑/k", "up", "up", "k"),
		row("↓/j", "down", "down", "j"),
		row("pgup/pgdn", "page", "pgup", "pgdown"),
	}
	if m.vim {
		move = append(move, row("^u/^d", "half page", "ctrl+u", "ctrl+d"))
	}
	move = append(move,
		row("g/G", "first/last", "g", "G", "home", "end"),
		row("←/→", "scroll the long command", "left", "right"),
	)

	panes := []keyhelp.Row{row("tab/shift+tab", "switch pane", "tab", "shift+tab")}
	if m.vim {
		panes = append(panes, row("h/l", "switch pane", "h", "l"))
	}
	panes = append(panes,
		row("z", "zoom the pane", "z"),
		row("Z", "zoom, keeping the detail pane beside it", "Z"),
		// esc backs out one visible thing at a time, same as the agent
		// explorer: the zoom first, then a bulk selection still on screen.
		row("esc", "unzoom, then clear the selection", "esc"),
		mouseRow("click", "focus a pane"),
		mouseRow("drag", "resize panes"),
		mouseRow("wheel", "scroll under the pointer"),
	)

	act := []keyhelp.Row{
		row("enter", "put it on the prompt", "enter"),
		row("y", "copy", "y"),
		row("^t", "tag (the selection, if any)", "ctrl+t"),
		row("^x", "untag (the selection, if any)", "ctrl+x"),
	}
	if m.vim {
		act = append(act, row("d", "delete (the selection, if any)", "d"))
	} else {
		act = append(act, row("d/^d", "delete (the selection, if any)", "d", "ctrl+d"))
	}

	return []keyhelp.Group{
		{Title: "MOVE", Rows: move},
		{Title: "PANES", Rows: panes},
		{Title: "FIND", Rows: []keyhelp.Row{
			row("/", "search", "/"),
			row("A", m.agentToggleDesc(), "A"),
			row("t/e", "filter by this row's tag/executor", "t", "e"),
			row("T/E", "filter by any tag/executor", "T", "E"),
			row("H", "cycle the host scope", "H"),
			row("1-5", "time window", "1", "2", "3", "4", "5"),
		}},
		{Title: "COLUMNS", Rows: []keyhelp.Row{
			row("c", "show, hide, and sort by column", "c"),
		}},
		{Title: "SELECT", Rows: []keyhelp.Row{
			row("space", "check this row for a bulk action", " "),
			row("^a", "check all shown, or clear if all are checked", "ctrl+a"),
		}},
		{Title: "ACT", Rows: act},
		{Title: "GO", Rows: globalRows("")},
	}
}

func (m Model) statsGroups() []keyhelp.Group {
	return []keyhelp.Group{
		{Title: "WINDOW", Rows: []keyhelp.Row{
			row("1-5", "time window", "1", "2", "3", "4", "5"),
		}},
		{Title: "GO", Rows: append(
			[]keyhelp.Row{row("s/esc", "back to browsing", "esc")},
			globalRows("s")...,
		)},
	}
}

func (m Model) agentsGroups() []keyhelp.Group {
	return []keyhelp.Group{
		{Title: "MOVE", Rows: []keyhelp.Row{
			row("↑/k", "up", "up", "k"),
			row("↓/j", "down", "down", "j"),
			row("pgup/pgdn", "page", "pgup", "pgdown"),
			row("g/G", "first/last", "g", "G", "home", "end"),
			row("←/→", "scroll the long command", "left", "right"),
		}},
		{Title: "PANES", Rows: []keyhelp.Row{
			row("tab/shift+tab", "switch pane", "tab", "shift+tab"),
			row("z", "zoom the pane", "z"),
			row("Z", "zoom, keeping the detail pane beside it", "Z"),
			mouseRow("click", "focus a pane"),
			mouseRow("drag", "resize panes"),
			mouseRow("wheel", "scroll under the pointer"),
		}},
		{Title: "FIND", Rows: []keyhelp.Row{
			row("/", "filter this pane's list", "/"),
			row("H", "cycle the host filter", "H"),
			row("A", "cycle the agent filter", "A"),
			row("1-5", "time window", "1", "2", "3", "4", "5"),
		}},
		// c aims at the focused list, so it is described where focus can be.
		{Title: "COLUMNS", Rows: []keyhelp.Row{
			row("c", "show, hide, and sort this list's columns", "c"),
		}},
		// Esc backs out one visible thing at a time. Spelling the order out is
		// the only way the key is predictable in a view that can be zoomed and
		// filtered at once.
		{Title: "GO", Rows: append(
			[]keyhelp.Row{row("esc", "unzoom, then clear the filter, then back", "esc")},
			globalRows("a")...,
		)},
	}
}

// devicesGroups is the devices view, which owns its keys outright: the global
// switch never runs here, so nothing from it may be advertised, and q goes back
// rather than quitting, which is the one place the browser's q does not quit.
func devicesGroups() []keyhelp.Group {
	return []keyhelp.Group{
		{Title: "MOVE", Rows: []keyhelp.Row{
			row("↑/k", "up", "up", "k"),
			row("↓/j", "down", "down", "j"),
			row("g/G", "first/last", "g", "G"),
			row("tab", "switch pane", "tab", "shift+tab"),
			row("z", "zoom the pane", "z"),
			mouseRow("click", "focus a pane"),
			mouseRow("drag", "resize the panes"),
			mouseRow("wheel", "scroll under the pointer"),
		}},
		{Title: "MACHINES", Rows: []keyhelp.Row{
			row("a", "approve a pending machine (asks first)", "a"),
			row("x", "revoke it and rotate keys (asks first)", "x"),
		}},
		// x is one key doing the pane's version of "revoke", so it is described
		// once per pane rather than once with a caveat.
		{Title: "TOKENS", Rows: []keyhelp.Row{
			row("n", "mint an enrollment token", "n"),
			row("y", "copy the one just minted", "y"),
			row("x", "cancel an unused token (asks first)", "x"),
		}},
		// c aims at the focused list, so it is described where focus can be.
		{Title: "COLUMNS", Rows: []keyhelp.Row{
			row("c", "show, hide, and sort this list's columns", "c"),
		}},
		{Title: "GO", Rows: []keyhelp.Row{
			row("S", "refetch both lists from the server", "S"),
			row("esc/q/D", "back to browsing", "esc", "q", "D"),
			row("?", "these keys", "?"),
			row("^c", "quit", "ctrl+c"),
		}},
	}
}

// columnsGroups is the columns pane, which owns its keys while it is up: it
// covers the table it reshapes, so nothing from the view beneath may be claimed.
func columnsGroups() []keyhelp.Group {
	return []keyhelp.Group{
		{Title: "MOVE", Rows: []keyhelp.Row{
			row("↑/k", "up", "up", "k"),
			row("↓/j", "down", "down", "j"),
			row("g/G", "first/last", "g", "G", "home", "end"),
		}},
		{Title: "COLUMN", Rows: []keyhelp.Row{
			row("space/x", "show or hide it", " ", "x"),
			row("s/enter", "sort by it (again reverses)", "s", "enter"),
		}},
		{Title: "GO", Rows: []keyhelp.Row{
			row("esc/c/q", "back to the table", "esc", "c", "q"),
			row("?", "these keys", "?"),
			row("^c", "quit", "ctrl+c"),
		}},
	}
}

// globalRows are the keys handled for every view that reaches the global switch;
// browse, stats and the agent explorer. `except` drops the one that would name
// the view you are already in, since there it reads as "back", listed separately.
func globalRows(except string) []keyhelp.Row {
	all := []keyhelp.Row{
		row("s", "stats", "s"),
		row("a", "agents", "a"),
		row("D", "devices", "D"),
		row("S", "sync now", "S"),
		row("?", "these keys", "?"),
		row("q", "quit", "q", "ctrl+c"),
	}
	out := make([]keyhelp.Row, 0, len(all))
	for _, r := range all {
		if r.Keys == except {
			continue
		}
		out = append(out, r)
	}
	return out
}

// --- the terse footer ----------------------------------------------------

// footerRows is the one-line hint bar: a handful of keys chosen for the state
// the browser is actually in, ending in the pointer to the rest. Anything longer
// is a help panel pretending to be a footer.
func (m Model) footerRows() []keyhelp.Row {
	switch {
	case m.bulk != nil:
		return bulkRows()
	case m.confirmDelete:
		return confirmDeleteRows()
	case m.searching:
		return searchingRows()
	case m.axis != axisNone:
		return filterEntryRows(m.axis)
	case m.afiltering:
		return agentFilterRows(m.afilterPane)
	case m.tagging:
		return taggingRows(m.tagRemove)
	case m.showHelp:
		return m.helpOpenRows()
	case m.showCols:
		return []keyhelp.Row{
			row("↑↓", "column", "up", "down"),
			row("space", "show/hide", " "),
			row("s", "sort by it", "s"),
			row("esc", "back", "esc"),
		}
	case m.view == viewDevices && m.devConfirm != "":
		return confirmDeviceRows(m.devApproving)
	}

	switch m.view {
	case viewStats:
		return []keyhelp.Row{
			row("1-5", "window", "1", "2", "3", "4", "5"),
			row("a", "agents", "a"),
			row("s/esc", "back", "esc"),
			row("?", "keys", "?"),
			row("q", "quit", "q"),
		}
	case viewAgents:
		// ↑↓ move a cursor in the four list panes and scroll the body in the
		// details pane, so the hint follows focus rather than naming one of them.
		move := "move"
		if m.apane == apInfo {
			move = "scroll"
		}
		rows := []keyhelp.Row{
			row("↑↓", move, "up", "down"),
			row("tab", "pane", "tab"),
			row("/", "filter "+filterNoun(m.filterTarget()), "/"),
			row("z", "zoom", "z"),
		}
		// Z only does something different from z on the panes that have a
		// details companion worth keeping, so it is offered exactly there.
		if m.zoomHasDetail() {
			rows = append(rows, row("Z", "zoom with details", "Z"))
		}
		rows = append(rows,
			row("1-5", "window", "1", "2", "3", "4", "5"),
			row("H", "host", "H"),
			row("A", "agent", "A"),
		)
		// With a filter up, esc means "drop it" before it means "leave"; say the
		// one that will actually happen next.
		if m.filterFor(m.filterTarget()) != "" {
			rows = append(rows, row("esc", "clear filter", "esc"))
		} else {
			rows = append(rows, row("a/esc", "back", "esc"))
		}
		return append(rows, row("?", "keys", "?"), row("q", "quit", "q"))
	case viewDevices:
		// The two panes offer different actions, so the hint follows focus rather
		// than listing both and leaving the user to work out which applies here.
		rows := []keyhelp.Row{row("↑↓", "move", "up", "down"), row("tab", "pane", "tab")}
		if m.dpane == dpTokens {
			rows = append(rows, row("n", "mint", "n"), row("x", "cancel", "x"))
		} else {
			rows = append(rows, row("a", "approve", "a"), row("x", "revoke", "x"))
		}
		// A minted token is on screen for one moment and never again, so the key
		// that saves it leads while it is up, from either pane, since n mints
		// from either.
		if m.minted != "" {
			rows = append([]keyhelp.Row{row("y", "copy the token", "y")}, rows...)
		}
		rows = append(rows, row("c", "columns", "c"), row("S", "refresh", "S"))
		if m.zoom {
			rows = append(rows, row("z", "unzoom", "z"))
		}
		return append(rows,
			row("esc", "back", "esc"),
			row("?", "keys", "?"),
			row("^c", "quit", "ctrl+c"),
		)
	}

	// Browse: the first hints follow focus, because the same arrow keys mean
	// three different things across the three panes.
	var rows []keyhelp.Row
	switch m.focus {
	case focusHosts:
		rows = []keyhelp.Row{
			row("↑↓", "host", "up", "down"),
			row("tab", "pane", "tab"),
			row("/", "search", "/"),
		}
	case focusDetail:
		rows = []keyhelp.Row{
			row("↑↓", "scroll", "up", "down"),
			row("tab", "pane", "tab"),
			row("/", "search", "/"),
		}
	default:
		rows = []keyhelp.Row{
			row("↑↓", "move", "up", "down"),
			row("enter", "prompt", "enter"),
			row("y", "copy", "y"),
			row("/", "search", "/"),
			row("tab", "pane", "tab"),
		}
	}
	// Advertise the agent filter exactly when it is holding something back: which
	// is exactly when someone might be wondering where a command they remember
	// running went. With nothing hidden there is nothing to explain, and "?"
	// still carries the key.
	if m.hiddenAgentsNote() != "" {
		rows = append(rows, row("A", "show agents", "A"))
	}
	if m.zoom {
		// Says what pressing it will do next, the way agentToggleDesc reads
		// hideAgents: the same key means "add the side pane" until there is one,
		// then "drop it" once there is.
		if m.zoomHasDetail() {
			if m.zoomDetail {
				rows = append(rows, row("Z", "hide detail", "Z"))
			} else {
				rows = append(rows, row("Z", "show detail", "Z"))
			}
		}
		rows = append(rows, row("z/esc", "unzoom", "z", "esc"))
	}
	return append(rows, row("?", "keys", "?"), row("q", "quit", "q"))
}

// The modal states. Each swallows nearly all input, so its footer lists what is
// left rather than the keys the view underneath would have offered.

// bulkRows is the footer while a batch runs. It is the shortest of them all
// because it is the whole truth: those two keys are the only ones the browser
// answers to until the run ends, and the flash beside it is counting.
func bulkRows() []keyhelp.Row {
	return []keyhelp.Row{
		row("esc", "stop", "esc"),
		row("^c", "quit", "ctrl+c"),
	}
}

func confirmDeleteRows() []keyhelp.Row {
	return []keyhelp.Row{
		row("y", "delete", "y", "Y"),
		row("n/esc", "cancel", "n", "N", "esc"),
	}
}

func confirmDeviceRows(approving bool) []keyhelp.Row {
	verb := "revoke"
	if approving {
		verb = "approve"
	}
	return []keyhelp.Row{
		row("y", verb, "y", "Y"),
		// Deliberately not a binding: anything that is not y cancels. Both actions
		// here are ones you cannot take back quietly; approving admits a machine
		// to the group, revoking rotates its keys.
		{Keys: "any other key", Desc: "cancel"},
	}
}

func searchingRows() []keyhelp.Row {
	return []keyhelp.Row{
		{Keys: "type", Desc: "to filter"},
		row("enter/esc", "done", "enter", "esc"),
		row("^c", "quit", "ctrl+c"),
	}
}

// agentFilterRows is the footer while the explorer's filter box has focus. It
// names the list being narrowed, because the box is on the header line and the
// two panes it can aim at are both on screen.
func agentFilterRows(p agentPane) []keyhelp.Row {
	return []keyhelp.Row{
		{Keys: "type", Desc: "to filter " + filterNoun(p)},
		row("enter/esc", "done", "enter", "esc"),
		row("^c", "quit", "ctrl+c"),
	}
}

// taggingRows is the footer while the tag box has focus. One box serves both
// ops, so it says which one enter is about to commit; "save the tag" over a
// box that is going to remove one would be exactly backwards.
func taggingRows(remove bool) []keyhelp.Row {
	desc := "save the tag"
	if remove {
		desc = "remove the tag"
	}
	return []keyhelp.Row{
		row("enter", desc, "enter"),
		row("esc", "cancel", "esc"),
	}
}

// filterEntryPrompt labels the typed value box on the search line.
func filterEntryPrompt(a filterAxis) string { return "filter " + a.noun() + " ❯ " }

// filterEntryRows is the footer while that box has focus. It says an empty box
// clears the filter, because that is the one thing the box does that typing a
// value does not suggest.
func filterEntryRows(a filterAxis) []keyhelp.Row {
	return []keyhelp.Row{
		{Keys: "type", Desc: "a " + a.noun()},
		row("enter", "apply (empty clears)", "enter"),
		row("esc", "cancel", "esc"),
		row("^c", "quit", "ctrl+c"),
	}
}

// helpOpenRows offers a scroll hint only when the list actually overflows the
// pane, on a wide terminal it never does, and a hint for a key that would move
// nothing is worse than no hint.
func (m Model) helpOpenRows() []keyhelp.Row {
	var rows []keyhelp.Row
	if m.helpMaxTop() > 0 {
		rows = append(rows, row("↑↓", "scroll", "up", "down", "k", "j"))
	}
	return append(rows, row("esc/?", "close", "esc", "?", "q"))
}
