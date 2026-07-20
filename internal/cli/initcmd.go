package cli

import (
	"fmt"
	"os"

	"yore/internal/shell"
)

// runInit prints the shell integration script for eval'ing in rc files:
//
//	eval "$(yore init zsh)"    # ~/.zshrc
//	eval "$(yore init bash)"   # ~/.bashrc
func runInit(sh, bin string, noAliases bool) int {
	script, err := shell.Init(sh, shell.Options{Aliases: !noAliases, Bin: bin})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 2
	}
	fmt.Print(script)
	return 0
}
