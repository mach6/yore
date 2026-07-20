package cli

import (
	"fmt"
	"os"
	"time"

	"yore/internal/daemon"
)

// runDaemon runs the background daemon in the foreground (normally
// auto-spawned via `yore daemon`). idle is the idle timeout before exit; 0
// means take the default from config (30m).
func runDaemon(idle time.Duration) int {
	err := daemon.Run(stateDir(), daemon.Options{
		IdleTimeout: idle,
		Version:     Version,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore daemon:", err)
		return 1
	}
	return 0
}

// daemonStop asks a running daemon to shut down gracefully (it writes a final
// warm snapshot and releases the socket). A daemon that isn't running is not
// an error.
func daemonStop() int {
	c, err := daemon.Dial(stateDir())
	if err != nil {
		fmt.Println("daemon not running")
		return 0
	}
	defer c.Close()
	if err := c.Shutdown(); err != nil {
		fmt.Fprintln(os.Stderr, "yore daemon stop:", err)
		return 1
	}
	fmt.Println("daemon stopped")
	return 0
}
