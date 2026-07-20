package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"yore/internal/daemon"
	"yore/internal/proto"
	"yore/internal/tui/search"
)

// cmdSearch searches history. Interactive TUI by default (drawn on /dev/tty,
// selection printed to stdout — the Ctrl-R widget contract); --headless
// prints matching commands as plain lines for scripts and `hs | grep`-style
// piping. The query is --query or the joined positional args.
func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	query := fs.String("query", "", "initial query")
	headless := fs.Bool("headless", false, "print matches to stdout instead of the TUI")
	limit := fs.Int("limit", 0, "headless: max results (default 200)")
	scope := fs.String("scope", proto.ScopeLocal, "headless: local|all|session|cwd")
	tag := fs.String("tag", "", "filter by executor tag (e.g. claude-code)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := *query
	if q == "" {
		q = strings.Join(fs.Args(), " ")
	}

	if *headless {
		return headlessSearch(q, *scope, *tag, *limit)
	}
	return interactiveSearch(q)
}

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
	cmd, ok, err := search.Run(c, search.Options{
		InitialQuery: initialQuery,
		Session:      os.Getenv("YORE_SESSION"),
		Cwd:          cwd,
		Version:      Version,
	})
	if err != nil {
		// No /dev/tty (or the TUI failed): behave like headless so pipes
		// and odd environments still get results.
		return headlessSearch(initialQuery, proto.ScopeLocal, "", 0)
	}
	if !ok {
		return 1
	}
	fmt.Println(cmd)
	return 0
}

func headlessSearch(q, scope, tag string, limit int) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer c.Close()

	req := proto.QueryReq{Q: q, Scope: scope, Tag: tag, Limit: limit, Dedupe: true}
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
