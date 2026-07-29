package search

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"yore/internal/tui/keyhelp"
)

// The panel's key handlers. As in the browser, their case clauses are the whole
// truth about which keys do something.
var handlerFuncs = map[string]bool{
	"handleKey":       true,
	"handleNormalKey": true,
	"handleHelpKey":   true,
}

func handledKeys(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "reading the package directory")

	keys := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		require.NoError(t, err, entry.Name())
		if file.Name.Name != "search" {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !handlerFuncs[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						keys[s] = true
					}
				}
				return true
			})
		}
	}
	require.NotEmpty(t, keys, "found no key cases — the handler names must have drifted")
	return keys
}

// describedKeys gathers every key the panel can show, across both keymaps and
// both of vim's sub-modes.
func describedKeys(t *testing.T) map[string]bool {
	t.Helper()

	var rows []keyhelp.Row
	for _, keymap := range []string{"", "vim"} {
		for _, normal := range []bool{false, true} {
			for _, help := range []bool{false, true} {
				m := NewModel(&fakeQuerier{}, Options{Keymap: keymap})
				m.normal, m.showHelp = normal, help
				for _, g := range m.helpGroups() {
					rows = append(rows, g.Rows...)
				}
				rows = append(rows, m.hintRows()...)
			}
		}
	}

	keys := map[string]bool{}
	for _, r := range rows {
		for _, k := range r.Keyed {
			keys[k] = true
		}
	}
	return keys
}

// TestEveryHandledKeyIsDescribed: the Ctrl-R panel used to advertise nothing at
// all, so alt+d, alt+f, alt+z and ctrl+r were findable only by reading the
// source. This holds the two lists to each other, both ways.
func TestEveryHandledKeyIsDescribed(t *testing.T) {
	handled, described := handledKeys(t), describedKeys(t)

	for k := range handled {
		require.Truef(t, described[k],
			"key %q is handled but appears in no help text — add it to keys.go", k)
	}
	for k := range described {
		require.Truef(t, handled[k],
			"key %q is advertised but no handler dispatches on it — drop it from keys.go", k)
	}
}

func readyPanel(t *testing.T, w int) Model {
	t.Helper()
	q := &fakeQuerier{resp: mkResp(mkRows("git status", "make test"))}
	m := NewModel(q, Options{Version: "v1"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 16})
	m = tm.(Model)
	tm, _ = m.Update(queryResultMsg{seq: 1, resp: q.resp})
	return tm.(Model)
}

func altSlash() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/"), Alt: true}
}

// TestHelpFitsThePanel: the panel is drawn inline above the shell prompt, so the
// key list has to live inside the rows the panel already claims — a list that
// grew past a full result set would shove the terminal's scrollback around every
// time someone asked what a key does.
func TestHelpFitsThePanel(t *testing.T) {
	m := readyPanel(t, 100)

	tm, _ := m.Update(altSlash())
	hm := tm.(Model)
	require.True(t, hm.showHelp, "alt+/ should raise the key list")
	require.LessOrEqual(t, len(strings.Split(hm.View(), "\n")), hm.rowsVisible+2,
		"the list must fit the panel: the prompt line, rowsVisible rows, the status line")
	require.Contains(t, strip(hm.View()), "FILTER", "the list should be on screen")

	tm, _ = hm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.False(t, tm.(Model).showHelp, "esc should close the list")
	require.False(t, tm.(Model).cancel, "esc should close the list, not cancel the search")
}

// TestHelpSwallowsNavigation: the list covers the rows, so keys that would move
// a selection the user cannot see must not fire through it.
func TestHelpSwallowsNavigation(t *testing.T) {
	m := readyPanel(t, 100)
	tm, _ := m.Update(altSlash())
	m = tm.(Model)

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Zero(t, tm.(Model).sel, "the selection must not move while the list is up")

	// Ctrl-C is the exception: it always means "get me out of here".
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	require.True(t, tm.(Model).cancel, "ctrl+c should still cancel")
}

// TestNormalModeUsesQuestionMark: in vim's normal sub-mode nothing is being
// typed, so "?" can mean what it means everywhere else in yore.
func TestNormalModeUsesQuestionMark(t *testing.T) {
	q := &fakeQuerier{resp: mkResp(mkRows("ls"))}
	m := NewModel(q, Options{Keymap: "vim"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	m = tm.(Model)

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // insert -> normal
	m = tm.(Model)
	require.True(t, m.normal)
	require.Contains(t, strip(m.View()), "? keys", "normal mode should advertise ?")

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	require.True(t, tm.(Model).showHelp, "? should raise the key list in normal mode")
}

// TestQuestionMarkStillFiltersWhileTyping: the panel is a filter box first. A
// history search for "?" has to work, which is why the key list is on alt+/.
func TestQuestionMarkStillFiltersWhileTyping(t *testing.T) {
	m := readyPanel(t, 100)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	m = tm.(Model)
	require.False(t, m.showHelp, "? must not open help while filtering")
	require.Equal(t, "?", m.ti.Value(), "? should reach the filter")
}

// TestStatusHintsYieldToWarnings: the hints are the least important thing on the
// status line, so a narrow terminal drops them before it drops "daemon
// unreachable" — and never overflows.
func TestStatusHintsYieldToWarnings(t *testing.T) {
	for _, w := range []int{20, 40, 60, 100, 160} {
		m := readyPanel(t, w)
		line := m.statusLine(w)
		require.LessOrEqualf(t, lipgloss.Width(line), w, "w=%d: status line overflows: %q", w, strip(line))
	}

	wide := readyPanel(t, 120)
	require.Contains(t, strip(wide.statusLine(120)), "⌥/ keys", "a wide line should point at the key list")

	// With an error to report, the warning wins the room.
	errd := readyPanel(t, 60)
	tm, _ := errd.Update(queryResultMsg{seq: 2, err: errors.New("dial: connection refused")})
	got := strip(tm.(Model).statusLine(60))
	require.Contains(t, got, "daemon unreachable", "the warning must survive")
	require.LessOrEqual(t, lipgloss.Width(got), 60)
}
