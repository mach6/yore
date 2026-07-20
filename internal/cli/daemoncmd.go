package cli

import (
	"flag"
	"fmt"
	"os"

	"yore/internal/daemon"
)

// cmdDaemon runs the background daemon in the foreground of this process.
// It is normally auto-spawned (see record.go / daemon.EnsureRunning); running
// it by hand is useful for debugging with --idle.
func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	idle := fs.Duration("idle", 0, "idle timeout before exiting (default from config, 30m)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := daemon.Run(stateDir(), daemon.Options{
		IdleTimeout: *idle,
		Version:     Version,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore daemon:", err)
		return 1
	}
	return 0
}
