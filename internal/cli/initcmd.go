package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/shell"
)

// runInit prints the shell integration script for eval'ing in rc files:
//
//	eval "$(yore init zsh)"    # ~/.zshrc
//	eval "$(yore init bash)"   # ~/.bashrc
//
// The integration mode is the --mode flag if given, else config.integration
// (default "takeover").
func runInit(sh, bin, mode string, noAliases bool) int {
	if mode == "" {
		cfg, _ := config.Load(stateDir())
		mode = cfg.IntegrationMode()
	}
	script, err := shell.Init(sh, shell.Options{Aliases: !noAliases, Bin: bin, Mode: mode})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 2
	}
	fmt.Print(script)
	return 0
}
