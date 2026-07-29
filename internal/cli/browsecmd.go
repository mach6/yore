package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/tui/browse"
)

// runBrowse opens the full-screen history browser (the `h` alias target) on the
// given start view — `yore stats` and `yore agents` are the same program landed
// on a different screen. The command the user accepts with Enter is delivered
// out-of-band: to acceptFile when the `h` shell function passes --accept-file
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
	// A layout that can't be read (or written) is a convenience lost, not a
	// reason to refuse to open the browser — both errors are deliberately dropped.
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
		Splits: browse.Splits{
			BrowseLeft: ui.BrowseLeftSplit,
			BrowseTop:  ui.BrowseTopSplit,
			AgentLeft:  ui.AgentLeftSplit,
			AgentTop:   ui.AgentTopSplit,
		},
		SaveSplits: func(s browse.Splits) error {
			return config.SaveUI(dir, config.UIState{
				BrowseLeftSplit: s.BrowseLeft,
				BrowseTopSplit:  s.BrowseTop,
				AgentLeftSplit:  s.AgentLeft,
				AgentTopSplit:   s.AgentTop,
			})
		},
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
