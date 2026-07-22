package daemon

import (
	"os"
	"testing"
)

// TestMain pins secret storage to the file backend for the whole package. The
// daemon resolves its auth token through internal/secret, so without this the
// tests would probe (and could write to) the developer's real OS keyring.
func TestMain(m *testing.M) {
	if err := os.Setenv("YORE_SECRET_BACKEND", "file"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
