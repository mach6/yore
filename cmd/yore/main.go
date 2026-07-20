package main

import (
	"os"

	"yore/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
