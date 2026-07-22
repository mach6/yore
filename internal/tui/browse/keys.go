package browse

import (
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
)

// keyMap is the browser's full binding set. Behavior is dispatched from
// handleKey via msg.String() (so tests can synthesize keys directly); these
// bindings exist to drive the bubbles/help bar and to document the map in one
// place.
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Page    key.Binding
	Half    key.Binding // vim only: ctrl+u / ctrl+d half-page scroll
	Jump    key.Binding
	Focus   key.Binding
	Search  key.Binding
	Accept  key.Binding
	Copy    key.Binding
	Delete  key.Binding
	Tag     key.Binding
	Stats   key.Binding
	Sync    key.Binding
	Help    key.Binding
	Quit    key.Binding
	Confirm key.Binding
	vim     bool
}

// defaultKeyMap builds the binding set. In vim mode the pane-switch hint also
// advertises h/l and a half-page (ctrl+u/ctrl+d) binding is surfaced; in emacs
// mode the map is exactly as it has always been.
func defaultKeyMap(vim bool) keyMap {
	focus := key.NewBinding(key.WithKeys("tab", "shift+tab"), key.WithHelp("tab", "switch pane"))
	if vim {
		focus = key.NewBinding(key.WithKeys("tab", "shift+tab", "h", "l"), key.WithHelp("tab/h/l", "switch pane"))
	}
	return keyMap{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Page:    key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/pgdn", "page")),
		Half:    key.NewBinding(key.WithKeys("ctrl+u", "ctrl+d"), key.WithHelp("^u/^d", "half-page")),
		Jump:    key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom")),
		Focus:   focus,
		Search:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Accept:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "insert")),
		Copy:    key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy")),
		Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Tag:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tag filter")),
		Stats:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stats")),
		Sync:    key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "sync now")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Confirm: key.NewBinding(key.WithKeys("y", "n"), key.WithHelp("y/n", "confirm")),
		vim:     vim,
	}
}

// ShortHelp implements help.KeyMap: the compact one-line hint bar.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Focus, k.Search, k.Accept, k.Copy, k.Delete, k.Tag, k.Stats, k.Sync, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap: the expanded ? menu.
func (k keyMap) FullHelp() [][]key.Binding {
	nav := []key.Binding{k.Up, k.Down, k.Page, k.Jump}
	if k.vim {
		nav = []key.Binding{k.Up, k.Down, k.Page, k.Half, k.Jump}
	}
	return [][]key.Binding{
		nav,
		{k.Focus, k.Search, k.Accept, k.Copy, k.Delete},
		{k.Tag, k.Stats, k.Sync, k.Help, k.Quit},
	}
}

// helpKeys returns the footer hint set for the active view, so the bottom line
// advertises only the keys that actually do something here (handled in handleKey
// and handleDevicesKey). The browse view keeps the full keyMap.
func (m Model) helpKeys() help.KeyMap {
	switch m.view {
	case viewStats:
		return statsKeys{}
	case viewDevices:
		return devicesKeys{}
	case viewAgents:
		return agentsKeys{}
	default:
		return m.keys
	}
}

// statsKeys is the footer hint set for the stats view: s or esc returns to
// browse, q quits (see handleKey's global keys and its viewStats branch).
type statsKeys struct{}

func (statsKeys) ShortHelp() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("1", "2", "3", "4", "5"), key.WithHelp("1-5", "period")),
		key.NewBinding(key.WithKeys("s", "esc"), key.WithHelp("s/esc", "back")),
		key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
	}
}

func (k statsKeys) FullHelp() [][]key.Binding { return [][]key.Binding{k.ShortHelp()} }

// agentsKeys is the footer hint set for the agent-monitor view: period tabs,
// a/esc returns to browse, q quits.
type agentsKeys struct{}

func (agentsKeys) ShortHelp() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("1", "2", "3", "4", "5"), key.WithHelp("1-5", "period")),
		key.NewBinding(key.WithKeys("a", "esc"), key.WithHelp("a/esc", "back")),
		key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
	}
}

func (k agentsKeys) FullHelp() [][]key.Binding { return [][]key.Binding{k.ShortHelp()} }

// devicesKeys is the footer hint set for the devices view (see handleDevicesKey:
// a/x act, r refetches, j/k move; esc/q/D return to browse, ctrl+c quits — q
// does NOT quit here, so the hint must not claim it does).
type devicesKeys struct{}

func (devicesKeys) ShortHelp() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "revoke")),
		key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc/q", "back")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^c", "quit")),
	}
}

func (k devicesKeys) FullHelp() [][]key.Binding { return [][]key.Binding{k.ShortHelp()} }
