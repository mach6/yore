package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/tui/browse"
)

// runBrowse opens the full-screen history browser (the `h` alias target). The
// command the user accepts with Enter is delivered out-of-band: to acceptFile
// when the `h` shell function passes --accept-file (so it can drop it on the
// next prompt), otherwise printed to stdout so a bare `yore browse` still
// surfaces the pick. Nothing is emitted when the user quits without accepting.
func runBrowse(acceptFile string) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore browse: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	cwd, _ := os.Getwd()
	cfg, _ := config.Load(stateDir())
	accepted, err := browse.Run(c, browse.Options{
		Version: Version,
		Session: os.Getenv("YORE_SESSION"),
		Cwd:     cwd,
		Keymap:  cfg.Keymap,
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
