package cli

import (
	"fmt"

	"yore/internal/rec"
)

// cmdGenID prints one fresh ULID; the shell hook uses it for session ids.
func cmdGenID([]string) int {
	fmt.Println(rec.NewID())
	return 0
}

// The commands below are wired up as later build waves land.

func cmdBrowse(args []string) int { return notImplemented("browse") }
