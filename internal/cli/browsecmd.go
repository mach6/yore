package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/risk"
	"yore/internal/tui/browse"
)

// riskRules loads risk.toml for the browser, dropping warnings on purpose: the
// alt-screen eats stderr, and `yore doctor` is the venue that reports a broken
// file. Load fails safe, so the built-ins are on either way.
func riskRules(dir string) *risk.Ruleset {
	rs, _ := risk.Load(dir)
	return rs
}

// prefsFromUI and uiFromPrefs translate between ui.toml's shape and the
// browser's. They are a pair on purpose: ui.toml is rewritten whole on every
// save, so anything one of them drops the other silently deletes from the file.
func prefsFromUI(ui config.UIState) browse.Prefs {
	p := browse.Prefs{
		Splits: browse.Splits{
			BrowseLeft: ui.BrowseLeftSplit,
			BrowseTop:  ui.BrowseTopSplit,
			AgentLeft:  ui.AgentLeftSplit,
			AgentTop:   ui.AgentTopSplit,
			AgentHosts: ui.AgentHostsSplit,
			DevicesTop: ui.DevicesTopSplit,
		},
	}
	if len(ui.Columns) > 0 {
		p.Columns = make(map[string]browse.ColumnPrefs, len(ui.Columns))
		for table, c := range ui.Columns {
			p.Columns[table] = browse.ColumnPrefs{
				Hidden: c.Hidden, Sort: c.Sort, SortDesc: c.SortDesc,
			}
		}
	}
	return p
}

func uiFromPrefs(p browse.Prefs) config.UIState {
	ui := config.UIState{
		BrowseLeftSplit: p.Splits.BrowseLeft,
		BrowseTopSplit:  p.Splits.BrowseTop,
		AgentLeftSplit:  p.Splits.AgentLeft,
		AgentTopSplit:   p.Splits.AgentTop,
		AgentHostsSplit: p.Splits.AgentHosts,
		DevicesTopSplit: p.Splits.DevicesTop,
	}
	if len(p.Columns) > 0 {
		ui.Columns = make(map[string]config.ColumnPrefs, len(p.Columns))
		for table, c := range p.Columns {
			ui.Columns[table] = config.ColumnPrefs{
				Hidden: c.Hidden, Sort: c.Sort, SortDesc: c.SortDesc,
			}
		}
	}
	return ui
}

// runBrowse opens the full-screen history browser (`yore browse`, which the
// `hb` shell function wraps) on the given start view; `yore stats` and
// `yore agents` are the same program landed on a different screen. The
// command the user accepts with Enter is delivered
// out-of-band: to acceptFile when the `hb` shell function passes --accept-file
// (so it can drop it on the next prompt), otherwise printed to stdout so a bare
// `yore browse` still surfaces the pick. Nothing is emitted when the user quits
// without accepting.
func runBrowse(acceptFile string, start browse.StartView) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore browse: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	cwd, _ := os.Getwd()
	dir := stateDir()
	cfg, _ := config.Load(dir)
	// A layout that can't be read (or written) is a convenience lost, not a reason
	// to refuse to open the browser: both errors are deliberately dropped.
	ui, _ := config.LoadUI(dir)
	accepted, err := browse.Run(c, browse.Options{
		Version: Version,
		Session: os.Getenv("YORE_SESSION"),
		Cwd:     cwd,
		Keymap:  cfg.Keymap,
		Start:   start,
		// The browse table opens without agent-run commands (A toggles); the agent
		// explorer is where that work is shown, grouped by the prompt behind it.
		HideAgents: cfg.HideAgentCommands,
		Risk:       riskRules(dir),
		Prefs:      prefsFromUI(ui),
		SavePrefs:  func(p browse.Prefs) error { return config.SaveUI(dir, uiFromPrefs(p)) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore browse:", err)
		return 1
	}
	if accepted == "" {
		return 0
	}
	if acceptFile != "" {
		if err := os.WriteFile(acceptFile, []byte(accepted), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "yore browse:", err)
			return 1
		}
		return 0
	}
	fmt.Println(accepted)
	return 0
}
