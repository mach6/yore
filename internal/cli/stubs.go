package cli

import (
	"fmt"

	"github.com/mach6/yore/internal/rec"
)

// runGenID prints one fresh ULID; the shell hook uses it for session ids.
func runGenID() int {
	fmt.Println(rec.NewID())
	return 0
}
