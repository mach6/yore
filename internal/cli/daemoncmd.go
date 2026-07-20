package cli

import (
	"flag"
	"fmt"
	"os"

	"yore/internal/daemon"
)

// cmdDaemon runs or controls the background daemon:
//
//	yore daemon          run in the foreground (normally auto-spawned)
//	yore daemon stop     ask a running daemon to exit gracefully
//	yore daemon status   show daemon status (alias of `yore status`)
func cmdDaemon(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			return daemonStop()
		case "status":
			return cmdStatus(nil)
		case "run":
			args = args[1:] // explicit run verb; fall through to foreground
		}
	}

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
