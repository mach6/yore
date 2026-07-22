package cli

import (
	"fmt"
	"os"

	"yore/internal/daemon"
	"yore/internal/mcp"
)

// runMcpServe runs the Model Context Protocol server on stdio: it ensures the
// daemon is up, then serves read-only history queries to an AI agent (Claude
// Code, Cursor, …) until stdin closes. Wired into agent configs by
// `yore init claude-code` / `yore init cursor`; not typed by users.
func runMcpServe() int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore mcp-serve: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	host, _ := os.Hostname()
	srv := mcp.New(c, mcp.Options{Version: Version, LocalHost: host})
	if err := srv.Serve(os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "yore mcp-serve:", err)
		return 1
	}
	return 0
}
