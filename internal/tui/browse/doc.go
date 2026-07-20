// Package browse implements yore's full-screen history browser: the
// deliberate "go look at my history" experience bound to the `h` alias
// (yore browse), as opposed to the inline Ctrl-R search panel.
//
// It is a full alt-screen Bubble Tea program with three browse panes — a
// host sidebar, a virtualized results list, and a detail pane — plus a
// single-screen stats view. Like the search TUI it never touches the daemon
// directly: it talks through the Backend interface so tests can drive it with
// a fake, and every keystroke fires an asynchronous, sequence-tagged query so
// stale, out-of-order responses are discarded. All styling comes from a
// theme.Theme built once in NewModel.
//
// The results list is rendered by hand rather than with bubbles/table: the
// table widget truncates every cell with runewidth.Truncate before styling,
// which counts ANSI escape bytes as display width and so corrupts any per-cell
// color (host hues, red/green exit markers, amber match highlights). A custom
// windowed renderer — the same approach the search panel uses — is required to
// meet the styling bar and keeps the two TUIs visually identical.
package browse
