package browse

import "github.com/charmbracelet/bubbles/key"

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
	Copy    key.Binding
	Delete  key.Binding
	Stats   key.Binding
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
		Copy:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "copy")),
		Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Stats:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stats")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Confirm: key.NewBinding(key.WithKeys("y", "n"), key.WithHelp("y/n", "confirm")),
		vim:     vim,
	}
}

// ShortHelp implements help.KeyMap: the compact one-line hint bar.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Focus, k.Search, k.Copy, k.Delete, k.Stats, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap: the expanded ? menu.
func (k keyMap) FullHelp() [][]key.Binding {
	nav := []key.Binding{k.Up, k.Down, k.Page, k.Jump}
	if k.vim {
		nav = []key.Binding{k.Up, k.Down, k.Page, k.Half, k.Jump}
	}
	return [][]key.Binding{
		nav,
		{k.Focus, k.Search, k.Copy, k.Delete},
		{k.Stats, k.Help, k.Quit},
	}
}
