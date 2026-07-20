// Package cli dispatches yore's subcommands. Each subcommand lives in its
// own file and returns a process exit code.
package cli

import (
	"fmt"
	"os"
)

// Version is stamped via -ldflags at release; the default marks dev builds.
var Version = "0.1.0-dev"

type command struct {
	name    string
	summary string
	run     func(args []string) int
}

func commands() []command {
	return []command{
		{"record", "capture one command from the shell hook (reads command text on stdin)", cmdRecord},
		{"search", "interactive history search (Ctrl-R); --headless for scripts", cmdSearch},
		{"browse", "full-screen history browser", cmdBrowse},
		{"daemon", "run the background daemon (normally auto-spawned)", cmdDaemon},
		{"import", "import existing shell history files", cmdImport},
		{"init", "print shell integration script (eval \"$(yore init zsh)\")", cmdInit},
		{"setup", "enroll this machine with a sync server", cmdSetup},
		{"devices", "list, approve, or revoke enrolled machines", cmdDevices},
		{"server", "run the sync server (normally in a container)", cmdServer},
		{"healthcheck", "probe a server's /v1/health (container HEALTHCHECK)", cmdHealthcheck},
		{"sync", "force an immediate push/pull with the server", cmdSync},
		{"gen-id", "print a fresh ULID (used for session ids)", cmdGenID},
		{"status", "show daemon and store status", cmdStatus},
		{"doctor", "run environment diagnostics", cmdDoctor},
		{"version", "print version", func([]string) int { fmt.Println("yore " + Version); return 0 }},
	}
}

// Run executes the subcommand named in args and returns its exit code.
func Run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	name := args[0]
	for _, c := range commands() {
		if c.name == name {
			return c.run(args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "yore: unknown command %q\n\n", name)
	usage()
	return 2
}

func usage() {
	fmt.Fprintf(os.Stderr, "yore %s — your shell history, everywhere, encrypted\n\nusage: yore <command> [flags]\n\n", Version)
	for _, c := range commands() {
		fmt.Fprintf(os.Stderr, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(os.Stderr, "\nstate dir: %s (override with YORE_DIR)\n", stateDir())
}

func notImplemented(name string) int {
	fmt.Fprintf(os.Stderr, "yore: %s is not wired up yet in this build\n", name)
	return 2
}
