package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/proto"
	"yore/internal/tui/search"
)

// interactiveSearch runs the inline TUI. Contract with the shell widgets:
// the accepted command is the ONLY thing printed to stdout (exit 0); cancel
// prints nothing and exits 1. Falls back to headless when no TTY exists.
func interactiveSearch(initialQuery string) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer c.Close()

	cwd, _ := os.Getwd()
	cfg, _ := config.Load(stateDir())
	cmd, ok, err := search.Run(c, search.Options{
		InitialQuery: initialQuery,
		Session:      os.Getenv("YORE_SESSION"),
		Cwd:          cwd,
		Version:      Version,
		Keymap:       cfg.Keymap,
	})
	if err != nil {
		// No /dev/tty (or the TUI failed): behave like headless so pipes
		// and odd environments still get results.
		return headlessSearch(initialQuery, proto.ScopeLocal, "", "", false, 0)
	}
	if !ok {
		return 1
	}
	fmt.Println(cmd)
	return 0
}

func headlessSearch(q, scope, tag, sortMode string, fuzzy bool, limit int) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer c.Close()

	req := proto.QueryReq{Q: q, Scope: scope, Tag: tag, Sort: sortMode, Fuzzy: fuzzy, Limit: limit, Dedupe: true}
	switch scope {
	case proto.ScopeSession:
		req.Session = os.Getenv("YORE_SESSION")
	case proto.ScopeCwd, proto.ScopeWorkspace:
		if wd, err := os.Getwd(); err == nil {
			req.Cwd = wd
		}
	}
	resp, err := c.Query(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 1
	}
	for _, r := range resp.Rows {
		fmt.Println(r.Cmd)
	}
	return 0
}
