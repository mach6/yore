package cli

import (
	"os"
	"testing"
)

// TestMain pins secret storage to the file backend for the whole package.
// Tests here run `yore setup`, which stores an auth token, without this they
// would write test values into the developer's real OS keyring, where a stray
// entry would then outrank their actual token.
func TestMain(m *testing.M) {
	if err := os.Setenv("YORE_SECRET_BACKEND", "file"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
