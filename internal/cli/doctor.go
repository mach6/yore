package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/redact"
	"yore/internal/secret"
	"yore/internal/shell"
	"yore/internal/syncer"
)

// runDoctor prints a health checklist: state dir, hooks, daemon, secrets
// filter, enrollment, and server reachability. Exit code is nonzero if any
// check FAILs (WARNs don't fail).
func runDoctor() int {
	dir := stateDir()
	failed := false
	u := newUI()
	ok := func(msg string) { u.step(msg, "") }
	warn := func(msg string) { u.warn(msg) }
	fail := func(msg string) { u.fail(msg); failed = true }

	u.title("yore " + Version + " — diagnostics")
	u.section("state")
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
	u.section("daemon")
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

	// Agent integration: capture hooks + MCP registration.
	u.section("agents")
	if claudeCaptureInstalled(shell.DefaultBin) {
		ok("Claude Code capture hooks installed (~/.claude/settings.json)")
	} else {
		warn("Claude Code hooks not installed — run `yore init claude-code`")
	}
	if p, perr := mcpConfigPath(false); perr == nil && mcpRegistered(p) {
		ok("MCP server registered for Claude Code (" + p + ")")
	} else {
		warn("MCP not registered for Claude Code — run `yore init claude-code`")
	}
	if cursorCaptureInstalled(shell.DefaultBin) {
		ok("Cursor capture hooks installed (~/.cursor/hooks.json)")
	}
	if opencodePluginInstalled() {
		ok("OpenCode capture plugin installed (~/.config/opencode/plugins/yore.js)")
	}
	if opencodeMcpRegistered() {
		ok("MCP server registered for OpenCode (~/.config/opencode/opencode.json)")
	}
	if codexHooksInstalled(shell.DefaultBin) {
		ok("Codex capture hooks installed (~/.codex/config.toml)")
	}
	if codexMcpRegistered() {
		ok("MCP server registered for Codex (~/.codex/config.toml)")
	}
	if p, perr := cursorMcpPath(false); perr == nil && mcpRegistered(p) {
		ok("MCP server registered for Cursor (" + p + ")")
	}
	if devinCaptureInstalled(shell.DefaultBin) {
		ok("Devin capture hooks installed (~/.config/devin/config.json)")
	}
	if devinMcpRegistered() {
		ok("MCP server registered for Devin (~/.config/devin/config.json)")
	}

	// Secrets filter.
	u.section("secrets filter")
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
	u.section("sync")
	if cfg.ServerURL == "" {
		warn("not configured — run `yore setup` to sync across machines (local-only otherwise)")
	} else {
		if _, err := secret.Open(dir).LoadDeviceKey(); err != nil {
			fail("device key problem: " + err.Error())
		} else {
			ok("device key present")
		}
		ok(fmt.Sprintf("secrets stored in the %s", secret.Open(dir).Backend()))
		http := syncer.NewHTTPClient(cfg.ServerURL, cfg.ServerPin)
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
