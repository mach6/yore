package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// commandRef matches a command a message tells the user to run, written the
// way every message writes one: in backticks, starting with `yore`.
var commandRef = regexp.MustCompile("`yore ([a-z][a-z-]*)")

// TestMessagesNameRealCommands fails when any message sends the user to a
// command that does not exist, which is worse than no advice at all.
func TestMessagesNameRealCommands(t *testing.T) {
	known := map[string]bool{"help": true}
	for _, c := range newRootCmd().Commands() {
		known[c.Name()] = true
	}

	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".agents" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range commandRef.FindAllStringSubmatch(string(src), -1) {
			checked++
			require.Truef(t, known[m[1]], "%s tells the user to run `yore %s`, which is not a command", path, m[1])
		}
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, checked, "found no command references at all; the pattern has rotted")
}
