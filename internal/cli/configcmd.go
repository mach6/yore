package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
)

// runGetConfig prints the effective value of a config key. It reads config.toml
// directly (no daemon, no network), so the shell integration can ask for a
// setting cheaply at call time instead of parsing the file itself.
func runGetConfig(key string) int {
	v, err := config.Get(stateDir(), key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore get-config:", err)
		return 1
	}
	fmt.Println(v)
	return 0
}

// runSetConfig assigns a config key and persists config.toml.
func runSetConfig(key, value string) int {
	if err := config.Set(stateDir(), key, value); err != nil {
		fmt.Fprintln(os.Stderr, "yore set-config:", err)
		return 1
	}
	fmt.Printf("%s = %s\n", key, value)
	return 0
}
