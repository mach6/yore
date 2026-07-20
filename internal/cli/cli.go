// Package cli is yore's command-line surface. The command tree is built with
// cobra (see root.go) so the binary ships shell completions; each subcommand's
// logic lives in its own file as a runXxx function returning a process exit
// code. Run wires cobra's error handling back to those exit codes.
package cli

// Version is stamped via -ldflags at release; the default marks dev builds.
var Version = "0.1.0-dev"
