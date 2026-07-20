package cli

import (
	"fmt"
	"os"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/tui/search"
)

// interactiveSearch runs the inline TUI. Contract with the shell widgets:
// the accepted command is the ONLY thing printed to stdout (exit 0); cancel
// prints nothing and exits 1. Falls back to headless when no TTY exists.
func interactiveSearch(initialQuery, scope string) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	cwd, _ := os.Getwd()
	cfg, _ := config.Load(stateDir())
	cmd, ok, err := search.Run(c, search.Options{
		InitialQuery: initialQuery,
		Scope:        scope,
		Session:      os.Getenv("YORE_SESSION"),
		Cwd:          cwd,
		Version:      Version,
		Keymap:       cfg.Keymap,
	})
	if err != nil {
		// No /dev/tty (or the TUI failed): behave like headless so pipes
		// and odd environments still get results. showHost=false: this feeds
		// the Ctrl-R `$(yore search …)` capture, whose contract is that ONLY
		// the bare command is printed — a host prefix would corrupt the buffer.
		return headlessSearch(initialQuery, proto.ScopeLocal, "", "", false, 0, false)
	}
	if !ok {
		return 1
	}
	fmt.Println(cmd)
	return 0
}

func headlessSearch(q, scope, tag, sortMode string, fuzzy bool, limit int, showHost bool) int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

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
	for _, line := range formatHeadless(resp.Rows, showHost) {
		fmt.Println(line)
	}
	return 0
}

// formatHeadless renders query rows as headless output lines. With showHost the
// hostname leads each line, left-padded to the widest hostname in rows and
// followed by two spaces, so the command column aligns; without it each line is
// the bare command (the Ctrl-R capture contract). It's a pure helper so the
// column math stays unit-testable. Embedded newlines in a command are passed
// through unchanged, matching the previous fmt.Println behavior.
func formatHeadless(rows []rec.Record, showHost bool) []string {
	if !showHost {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Cmd
		}
		return out
	}
	maxw := 0
	for _, r := range rows {
		if len(r.Hostname) > maxw {
			maxw = len(r.Hostname)
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		// Hostnames are ASCII, so byte width == column width.
		out[i] = fmt.Sprintf("%-*s  %s", maxw, r.Hostname, r.Cmd)
	}
	return out
}
