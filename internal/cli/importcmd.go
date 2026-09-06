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
	filter, ferrs := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs)
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

		warnUntimed(src, recs)

		// Mask credentials before they ever reach the store; years of shell
		// history is full of them, and the first sync would otherwise ship the
		// lot. The commands themselves are kept: that history is the whole point
		// of importing. Entries the user asked to ignore are dropped outright.
		kept := recs[:0]
		var redacted int
		for _, r := range recs {
			if filter.Ignored(r.Cmd) {
				continue
			}
			cmd, hits := filter.Redact(r.Cmd)
			if len(hits) > 0 {
				r.Cmd = cmd
				redacted++
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
		fmt.Printf("%-40s %6d entries, %6d new, %5d secrets redacted\n", src.Path, len(recs), added, redacted)
	}
	return 0
}

// histTimeFormatHint is the line a user can paste to make bash start writing
// "#<epoch>" markers. It is a var rather than a const so vet does not constant-
// fold it into the Fprintln below and read strftime's %F/%T as Printf verbs.
var histTimeFormatHint = `echo "export HISTTIMEFORMAT='%F %T '" >> ~/.bashrc`

// warnUntimed reports a bash history file that carries no timestamps at all.
// Bash writes a "#<epoch>" line before each command only when HISTTIMEFORMAT was
// set in the shell that flushed the file; by default it stores bare command
// lines, so every entry imports with no time. The information is not recoverable
// after the fact, and a silent import leaves a screen of untimed rows that reads
// like yore lost the dates, so say it once, at the moment it becomes true, with
// the fix for future entries. A partly-timestamped file (HISTTIMEFORMAT set at
// some point) is normal and says nothing.
func warnUntimed(src importer.Source, recs []rec.Record) {
	if src.Format != "bash" || len(recs) == 0 {
		return
	}
	for _, r := range recs {
		if r.StartMs != 0 {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "yore: %s: no timestamps. Bash records them only when HISTTIMEFORMAT is set,\n", src.Path)
	fmt.Fprintln(os.Stderr, "      so these entries import with an unknown time (existing entries cannot be dated).")
	fmt.Fprintln(os.Stderr, "      To timestamp future entries: "+histTimeFormatHint)
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
