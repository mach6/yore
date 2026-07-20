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
