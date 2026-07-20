package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/tui/browse"
)

// runBrowse opens the full-screen history browser (the `h` alias target).
func runBrowse() int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore browse: daemon unavailable:", err)
		return 1
	}
	defer c.Close()

	cwd, _ := os.Getwd()
	cfg, _ := config.Load(stateDir())
	if err := browse.Run(c, browse.Options{
		Version: Version,
		Session: os.Getenv("YORE_SESSION"),
		Cwd:     cwd,
		Keymap:  cfg.Keymap,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "yore browse:", err)
		return 1
	}
	return 0
}
