package main

import (
	"os"

	"github.com/mach6/yore/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
