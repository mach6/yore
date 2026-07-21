package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/daemon"
	"yore/internal/redact"
	"yore/internal/syncer"
)

// runDoctor prints a health checklist: state dir, hooks, daemon, secrets
// filter, enrollment, and server reachability. Exit code is nonzero if any
// check FAILs (WARNs don't fail).
func runDoctor() int {
	dir := stateDir()
	failed := false
	ok := func(msg string) { fmt.Printf("  \033[32mok\033[0m   %s\n", msg) }
	warn := func(msg string) { fmt.Printf("  \033[33mwarn\033[0m %s\n", msg) }
	fail := func(msg string) { fmt.Printf("  \033[31mFAIL\033[0m %s\n", msg); failed = true }

	fmt.Printf("yore %s — diagnostics\n\nstate\n", Version)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		if fi.Mode().Perm()&0o077 != 0 {
			warn(fmt.Sprintf("%s is %04o (want 0700)", dir, fi.Mode().Perm()))
		} else {
			ok(dir)
		}
	} else {
		warn(dir + " does not exist yet (nothing recorded)")
	}

	// Daemon + local history.
	fmt.Println("\ndaemon")
	if c, err := daemon.EnsureRunning(dir); err != nil {
		fail("daemon not reachable: " + err.Error())
	} else {
		st, serr := c.Status()
		_ = c.Close()
		if serr != nil {
			fail("daemon status failed: " + serr.Error())
		} else {
			ok(fmt.Sprintf("running (pid %d), %d local entries", st.PID, st.LocalRows))
			if st.LocalRows == 0 {
				warn("no history yet — add `eval \"$(yore init zsh)\"` to your rc, or `yore import auto`")
			}
		}
	}

	// Secrets filter.
	fmt.Println("\nsecrets filter")
	cfg, _ := config.Load(dir)
	filter, errs := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs)
	// Load's warnings already say when it fell back to the built-ins (missing,
	// unreadable, unparseable, empty, or an invalid pattern in redact.yml).
	for _, e := range errs {
		warn(e.Error())
	}
	if _, serr := os.Stat(config.RedactPath(dir)); serr == nil {
		ok("rules file: " + config.RedactPath(dir))
	}
	ok(fmt.Sprintf("active (%d rules loaded, %d user patterns, %d ignored dirs)",
		filter.NumRules(), len(cfg.IgnorePatterns), len(cfg.IgnoreDirs)))

	// Enrollment + server.
	fmt.Println("\nsync")
	if cfg.ServerURL == "" {
		warn("not configured — run `yore setup` to sync across machines (local-only otherwise)")
	} else {
		if _, err := cryptobox.LoadDeviceKey(config.KeyPath(dir)); err != nil {
			fail("device key problem: " + err.Error())
		} else {
			ok("device key present")
		}
		token := firstNonEmpty(cfg.Token, os.Getenv("YORE_TOKEN"))
		http := syncer.NewHTTPClient(cfg.ServerURL, token, cfg.ServerPin)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := http.Health(ctx); err != nil {
			fail(fmt.Sprintf("server %s unreachable: %v", cfg.ServerURL, err))
		} else {
			ok("server reachable: " + cfg.ServerURL)
			if devices, err := http.ListDevices(ctx); err == nil {
				active := 0
				for _, d := range devices {
					if d.Status == "active" {
						active++
					}
				}
				ok(fmt.Sprintf("%d device(s) enrolled, %d active", len(devices), active))
			}
		}
	}

	fmt.Println()
	if failed {
		return 1
	}
	return 0
}
