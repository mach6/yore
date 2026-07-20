package cli

import (
	"flag"
	"fmt"
	"os"

	"yore/internal/shell"
)

// cmdInit prints the shell integration script for eval'ing in rc files:
//
//	eval "$(yore init zsh)"    # ~/.zshrc
//	eval "$(yore init bash)"   # ~/.bashrc
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	noAliases := fs.Bool("no-aliases", false, "omit the h/hs convenience aliases")
	bin := fs.String("bin", shell.DefaultBin, "binary name or path the hooks should invoke")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: yore init [--no-aliases] [--bin PATH] zsh|bash")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	script, err := shell.Init(fs.Arg(0), shell.Options{Aliases: !*noAliases, Bin: *bin})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 2
	}
	fmt.Print(script)
	return 0
}
