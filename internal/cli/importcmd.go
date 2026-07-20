package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"yore/internal/config"
	"yore/internal/importer"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/redact"
	"yore/internal/store"
)

// runImport ingests existing shell history files. Usage:
//
//	yore import auto            # find and import ~/.zsh_history, ~/.bash_history, …
//	yore import --format zsh FILE...
//
// Deterministic import ids make re-runs no-ops. rest holds the positional args
// (either the single word "auto" or one-or-more file paths).
func runImport(format string, rest []string) int {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: yore import auto | yore import --format zsh|bash FILE...")
	}

	var sources []importer.Source
	switch {
	case len(rest) == 1 && rest[0] == "auto":
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "yore: cannot resolve home:", err)
			return 1
		}
		sources = importer.Auto(home)
		if len(sources) == 0 {
			fmt.Println("no history files found")
			return 0
		}
	case len(rest) > 0 && (format == "zsh" || format == "bash"):
		for _, p := range rest {
			sources = append(sources, importer.Source{Path: p, Format: format})
		}
	default:
		usage()
		return 2
	}

	dir := stateDir()
	cfg, _ := config.Load(dir)
	// Historical history files are full of secrets typed over the years. They
	// must pass the SAME recording gate as live commands, or the first sync
	// would ship years of credentials to the server. SkipDir does not apply
	// (imported records carry no cwd).
	filter, ferrs := redact.New(cfg.IgnorePatterns, cfg.IgnoreDirs)
	for _, e := range ferrs {
		fmt.Fprintln(os.Stderr, "yore:", e)
	}

	s, err := openStoreExclusive(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 1
	}
	defer func() { _ = s.Close() }()

	for _, src := range sources {
		f, err := os.Open(src.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "yore: %s: %v (skipped)\n", src.Path, err)
			continue
		}
		var recs []rec.Record
		switch src.Format {
		case "zsh":
			recs, err = importer.Zsh(f, s.HostID())
		case "bash":
			recs, err = importer.Bash(f, s.HostID())
		}
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "yore: %s: %v (skipped)\n", src.Path, err)
			continue
		}

		// Drop secret-bearing entries before they ever reach the store.
		kept := recs[:0]
		var skipped int
		for _, r := range recs {
			if filter.Sensitive(r.Cmd) {
				skipped++
				continue
			}
			kept = append(kept, r)
		}

		added := 0
		for i := 0; i < len(kept); i += 5000 {
			end := min(i+5000, len(kept))
			n, err := s.AppendBatch(kept[i:end])
			if err != nil {
				fmt.Fprintf(os.Stderr, "yore: %s: %v\n", src.Path, err)
				return 1
			}
			added += n
		}
		fmt.Printf("%-40s %6d entries, %6d new, %5d secrets skipped\n", src.Path, len(recs), added, skipped)
	}
	return 0
}

// openStoreExclusive opens the store, asking a running daemon to step aside
// first if it holds the lock. The daemon respawns on the next shell command.
func openStoreExclusive(dir string) (*store.Store, error) {
	s, err := store.Open(dir)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, store.ErrLocked) {
		return nil, err
	}
	requestDaemonShutdown(dir)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if s, err = store.Open(dir); err == nil {
			return s, nil
		} else if !errors.Is(err, store.ErrLocked) {
			return nil, err
		}
	}
	return nil, errors.New("store is locked and the daemon did not release it")
}

// requestDaemonShutdown sends a best-effort OpShutdown over the socket.
func requestDaemonShutdown(dir string) {
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 100*time.Millisecond)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_ = proto.WriteMsg(conn, proto.Request{Op: proto.OpShutdown})
	buf := make([]byte, 256)
	_, _ = conn.Read(buf)
}
