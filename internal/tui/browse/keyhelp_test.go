package browse

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/tui/keyhelp"
)

// handlerFuncs are the functions that decide what a key does. Every one of them
// dispatches on the key string, so their case clauses are the whole truth about
// which keys the browser answers to.
var handlerFuncs = map[string]bool{
	"handleKey":        true,
	"handleAgentsKey":  true,
	"handleDevicesKey": true,
	"handleHelpKey":    true,
}

// handledKeys reads the package's own source and returns every key string its
// handlers dispatch on. Parsing the source rather than listing the keys by hand
// is the point: a list would be one more thing to forget to update, which is
// exactly the drift this test exists to catch.
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
		if file.Name.Name != "browse" {
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
					lit, ok := e.(*ast.BasicLit)
					if ok && lit.Kind == token.STRING {
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

// describedKeys is every key the browser can put on screen, gathered from all
// the states it advertises: the four views' panels (in both keymaps, since vim
// mode adds bindings) and each modal footer.
func describedKeys(t *testing.T) map[string]bool {
	t.Helper()

	var rows []keyhelp.Row
	base := ready(t, &fakeBackend{}, 100, 30)
	for _, keymap := range []string{"", "vim"} {
		for _, v := range []viewMode{viewBrowse, viewStats, viewAgents, viewDevices} {
			m := ready(t, &fakeBackend{}, 100, 30)
			m.vim, m.view = keymap == "vim", v
			for _, g := range m.helpGroups() {
				rows = append(rows, g.Rows...)
			}
			for _, f := range []focus{focusHosts, focusTable, focusDetail} {
				m.focus = f
				rows = append(rows, m.footerRows()...)
			}
			m.zoom = true
			rows = append(rows, m.footerRows()...)
		}
	}
	rows = append(rows, confirmDeleteRows()...)
	rows = append(rows, confirmDeviceRows(false)...)
	rows = append(rows, confirmDeviceRows(true)...)
	rows = append(rows, searchingRows()...)
	rows = append(rows, taggingRows()...)
	rows = append(rows, base.helpOpenRows()...)

	keys := map[string]bool{}
	for _, r := range rows {
		for _, k := range r.Keyed {
			keys[k] = true
		}
	}
	return keys
}

// TestEveryHandledKeyIsDescribed is the contract behind "?": a key that does
// something is a key the user can find. It runs both ways — a binding that is
// documented but no longer handled is just as much a lie as one that is missing.
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

// TestHelpPanelOpensInEveryView: ? reaches the key list from every screen,
// including devices, which handles its keys before the global ones.
func TestHelpPanelOpensInEveryView(t *testing.T) {
	for _, tc := range []struct {
		name  string
		enter string
	}{
		{"browse", ""},
		{"stats", "s"},
		{"agents", "a"},
		{"devices", "D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{resp: mkResp(mkRows("ls"))}
			m := ready(t, f, 100, 30)
			if tc.enter != "" {
				m, _ = step(t, m, press(tc.enter))
			}

			m, _ = step(t, m, press("?"))
			out := strip(m.View())
			require.Containsf(t, out, "KEYS", "? should raise the key panel:\n%s", out)
			require.Containsf(t, out, tc.name, "the panel should name the view it describes:\n%s", out)

			m, _ = step(t, m, press("esc"))
			require.NotContains(t, strip(m.View()), "KEYS", "esc should close the key panel")
		})
	}
}

// TestHelpPanelSwallowsKeys: the panel covers the view underneath, so keys that
// would act on rows the user cannot see must not fire through it.
func TestHelpPanelSwallowsKeys(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("first", "second", "third"))}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
	m, _ = step(t, m, press("?"))

	m, _ = step(t, m, press("down"))
	require.Zero(t, m.sel, "the selection must not move while the panel is up")

	m, _ = step(t, m, press("D"))
	require.Equal(t, viewBrowse, m.view, "a view key must not fire through the panel")
}

// TestHelpPanelScrollsWhenItDoesNotFit: on a short terminal the list is taller
// than the pane, so it scrolls and says so — the alternative is a silently
// truncated list, which is the same as a wrong one.
func TestHelpPanelScrollsWhenItDoesNotFit(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	m := ready(t, f, 80, 14)
	m, _ = step(t, m, press("?"))
	require.Positive(t, m.helpMaxTop(), "80x14 should not fit the browse key list")

	out := strip(m.View())
	require.Contains(t, out, "↑↓ for more", "an overflowing panel should say it scrolls")
	require.Contains(t, out, "MOVE", "the panel starts at the top")

	m, _ = step(t, m, press("pgdown"))
	require.Positive(t, m.helpTop, "pgdown should scroll the panel")
	require.NotContains(t, strip(m.View()), "MOVE", "scrolling should move the list")

	// It cannot be scrolled past its end, however hard you push.
	for range 40 {
		m, _ = step(t, m, press("down"))
	}
	require.Equal(t, m.helpMaxTop(), m.helpTop, "the scroll offset should clamp to the end")
}

// TestFooterIsOneLineInEveryState guards the layout arithmetic: applyLayout
// budgets exactly one row for the footer, so a state whose footer wrapped would
// push the panes off the bottom of the frame.
func TestFooterIsOneLineInEveryState(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("go build ./...")), hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}, {Hostname: "boxB", Count: 1}}}}

	for _, tc := range []struct {
		name string
		keys []string
	}{
		{"browse", nil},
		{"searching", []string{"/"}},
		{"tagging", []string{"ctrl+t"}},
		{"confirm delete", []string{"d"}},
		{"zoomed", []string{"z"}},
		{"stats", []string{"s"}},
		{"agents", []string{"a"}},
		{"devices", []string{"D"}},
		{"help", []string{"?"}},
		{"help over devices", []string{"D", "?"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{40, 80, 120} {
				m := ready(t, f, w, 24)
				m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})
				for _, k := range tc.keys {
					m, _ = step(t, m, press(k))
				}
				lines := strings.Split(m.View(), "\n")
				footer := lines[len(lines)-1]
				require.LessOrEqualf(t, lipgloss.Width(footer), w,
					"w=%d: footer overflows:\n%s", w, strip(footer))
				require.NotEmptyf(t, strip(footer), "w=%d: the footer should never be blank", w)
			}
		})
	}
}

// TestFooterFollowsFocus: the same arrow keys mean three different things across
// the browse panes, so the hint that names them has to follow focus.
func TestFooterFollowsFocus(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls")), hosts: proto.HostsInfo{Hosts: []proto.HostCount{{Hostname: "boxA", Count: 3}}}}
	m := ready(t, f, 100, 30)

	require.Contains(t, footerText(m), "↑↓ move", "the table pane moves a selection")

	m.focus = focusDetail
	require.Contains(t, footerText(m), "↑↓ scroll", "the detail pane scrolls")

	m.focus = focusHosts
	require.Contains(t, footerText(m), "↑↓ host", "the sidebar picks a host")
}

// TestModalFootersReplaceTheView: while a modal is up, the footer describes the
// modal — the keys underneath it are not reachable and must not be offered.
func TestModalFootersReplaceTheView(t *testing.T) {
	f := &fakeBackend{resp: mkResp(mkRows("ls"))}
	m := ready(t, f, 100, 30)
	m, _ = step(t, m, queryResultMsg{seq: 1, resp: f.resp})

	m, _ = step(t, m, press("d"))
	require.Contains(t, footerText(m), "y delete")
	require.NotContains(t, footerText(m), "search", "the browse keys are unreachable here")

	m, _ = step(t, m, press("n"))
	m, _ = step(t, m, press("/"))
	require.Contains(t, footerText(m), "to filter")
	require.NotContains(t, footerText(m), "? keys", "? types a character while filtering")
}

func footerText(m Model) string {
	lines := strings.Split(strip(m.View()), "\n")
	return lines[len(lines)-1]
}
