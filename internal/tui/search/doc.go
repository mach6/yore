// Package search implements yore's inline Ctrl-R history search: a compact,
// non-alt-screen Bubble Tea panel (input line + result rows + status line)
// that renders under the shell prompt and vanishes on exit.
//
// The TUI never touches the daemon directly; it talks through the Querier
// interface so tests can drive it with a fake. Every keystroke fires an
// asynchronous query (tagged with a monotonic sequence number so stale,
// out-of-order responses are discarded); the first paint never blocks on the
// daemon. All styling comes from a theme.Theme built once in NewModel.
package search
