package search

import (
	"yore/internal/tui/keyhelp"
)

// The Ctrl-R panel's bindings, described once and shown two ways: a terse tail
// on the status line, and the full list behind alt+/ (and "?" in vim's normal
// sub-mode, where "?" is not a character being typed).
//
// The panel is a filter box first, so it cannot spend "?" on help the way the
// browser does: there, "?" means the literal character, and a history search for
// "?" has to work. alt+/ is the nearest thing to "?" that a filter box can
// afford, and it sits with the panel's other alt-modified toggles.
//
// Keys are named the way the keyboard in front of the user names them, so the
// alt keys read "alt+z" and not the Mac "⌥z": yore's platforms are Linux,
// FreeBSD and WSL as much as macOS, and on all but one of them that glyph is
// on no key at all. "^r" stays, being the terminal's own notation for control
// rather than any one vendor's.

func row(keys, desc string, keyed ...string) keyhelp.Row {
	return keyhelp.Row{Keys: keys, Desc: desc, Keyed: keyed}
}

// agentToggleDesc says what alt+a will do next, not what it is for, so the key list
// carries both the filter's current state and the outcome of pressing it.
func (m Model) agentToggleDesc() string {
	if m.hideAgents {
		return "show agent commands"
	}
	return "hide agent commands"
}

// helpGroups is every binding that does something in the panel's current mode.
func (m Model) helpGroups() []keyhelp.Group {
	move := []keyhelp.Row{
		row("↑/^p", "up", "up", "ctrl+p"),
		row("↓/^n", "down", "down", "ctrl+n"),
	}
	filter := []keyhelp.Row{
		row("alt+a", m.agentToggleDesc(), "alt+a"),
		row("alt+d", "show duplicates", "alt+d"),
		row("alt+z", "fuzzy matching", "alt+z"),
		row("alt+f", "rank by frequency, not time", "alt+f"),
		row("^r", "cycle scope", "ctrl+r"),
	}
	run := []keyhelp.Row{
		row("enter", "put it on the prompt", "enter"),
	}

	if m.vim && m.normal {
		move = append([]keyhelp.Row{
			row("j/k", "down/up", "j", "k"),
			row("g/G", "first/last", "g", "G"),
		}, move...)
		run = append(run,
			row("i/a//", "back to filtering", "i", "a", "/"),
			row("esc", "cancel", "esc", "ctrl+c", "ctrl+g"),
			row("?", "these keys", "?"),
		)
		// Normal mode reaches the scope cycle and the agent toggle; the other alt
		// toggles are insert-mode only.
		filter = []keyhelp.Row{
			row("^r", "cycle scope", "ctrl+r"),
			row("alt+a", m.agentToggleDesc(), "alt+a"),
		}
	} else {
		run = append(run, row("esc", "cancel", "esc", "ctrl+c", "ctrl+g"))
		if m.vim {
			// In vim mode the first Esc is a mode change, not a cancel; say so,
			// because everywhere else in yore Esc backs out.
			run[len(run)-1] = row("esc", "normal mode (again cancels)", "esc")
			run = append(run, row("^c", "cancel", "ctrl+c", "ctrl+g"))
		}
		filter = append([]keyhelp.Row{{Keys: "type", Desc: "to filter"}}, filter...)
		run = append(run, row("alt+/", "these keys", "alt+/"))
	}

	return []keyhelp.Group{
		{Title: "MOVE", Rows: move},
		{Title: "FILTER", Rows: filter},
		{Title: "RUN", Rows: run},
	}
}

// hintRows is the terse tail of the status line: the few keys worth naming
// where the eye already is, ending in the pointer to the rest. They are the last
// pieces on the line, so a narrow terminal drops them before it drops a warning.
func (m Model) hintRows() []keyhelp.Row {
	if m.showHelp {
		return []keyhelp.Row{row("esc", "close", "esc")}
	}
	if m.vim && m.normal {
		return []keyhelp.Row{
			row("j/k", "move", "j", "k"),
			row("enter", "run", "enter"),
			row("i", "filter", "i"),
			row("?", "keys", "?"),
		}
	}
	rows := make([]keyhelp.Row, 0, 4)
	rows = append(rows,
		row("↑↓", "move", "up", "down"),
		row("enter", "run", "enter"),
		row("^r", "scope", "ctrl+r"),
	)
	// When results are being withheld, the key that brings them back takes the
	// third slot rather than being appended: the hints are dropped from the end on
	// a narrow terminal, and this is the one a user staring at a short result list
	// actually needs.
	if m.hiddenAgentsNote() != "" {
		rows[2] = row("alt+a", "show agents", "alt+a")
	}
	return append(rows, row("alt+/", "keys", "alt+/"))
}
