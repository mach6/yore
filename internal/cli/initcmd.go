package cli

import (
	"fmt"
	"os"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/shell"
)

// runInit prints the shell integration script for eval'ing (zsh/bash) or
// sourcing (fish) in rc files:
//
//	eval "$(yore init zsh)"    # ~/.zshrc
//	eval "$(yore init bash)"   # ~/.bashrc
//	yore init fish | source    # ~/.config/fish/config.fish
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
