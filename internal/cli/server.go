package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"yore/internal/config"
	"yore/internal/server"
)

// runServer runs the sync server in the foreground:
//
//	yore server [--db P] [--listen A] [--pidfile F]   run (foreground)
//	yore server stop [--pidfile F | --db P]           stop a running server
//
// The server also shuts down gracefully on SIGINT/SIGTERM (Ctrl-C, docker
// stop), so `stop` is a convenience for a backgrounded local server.
//
// It hosts either ONE tenant (its token from --token, $YORE_TOKEN, or
// $YORE_TOKEN_FILE, its db at --db) or a set of NAMED tenants from
// $YORE_TOKENS_FILE (a JSON object {"name":"token", …}), each its own db at
// <dir(--db)>/tenants/<name>.db. The two are mutually exclusive: configuring
// both is an error, not a merge, so there is exactly one answer to which db a
// token's history lives in.
//
// Rolling per-tenant backups are controlled by $YORE_BACKUP_INTERVAL (a
// duration, default 1h; "0" disables) and $YORE_BACKUP_KEEP (int, default 3).
func runServer(db, listen, token, pidfile string) int {
	tok, err := resolveServerToken(token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
	tenants, err := loadTenants(os.Getenv("YORE_TOKENS_FILE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
	switch {
	case tok == "" && len(tenants) == 0:
		fmt.Fprintln(os.Stderr, "yore server: no token set (one tenant: --token, $YORE_TOKEN, or $YORE_TOKEN_FILE; named tenants: $YORE_TOKENS_FILE)")
		return 1
	case tok != "" && len(tenants) != 0:
		fmt.Fprintln(os.Stderr, "yore server: $YORE_TOKENS_FILE cannot be combined with --token/$YORE_TOKEN/$YORE_TOKEN_FILE; set one or the other")
		return 1
	}
	backupInterval, err := parseBackupInterval(os.Getenv("YORE_BACKUP_INTERVAL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
	backupKeep, err := parseBackupKeep(os.Getenv("YORE_BACKUP_KEEP"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}

	// Record the PID so `yore server stop` can find us; clean it up on exit.
	pf := serverPidFile(pidfile, db)
	if err := os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "yore server: warning: cannot write pidfile:", err)
	} else {
		defer func() { _ = os.Remove(pf) }()
	}

	if len(tenants) == 0 {
		fmt.Fprintf(os.Stderr, "yore server listening on %s (db %s)\n", listen, db)
	} else {
		fmt.Fprintf(os.Stderr, "yore server listening on %s (%d tenants under %s)\n",
			listen, len(tenants), filepath.Join(filepath.Dir(db), "tenants"))
	}
	opts := server.Options{
		DBPath:         db,
		Token:          tok,
		Tenants:        tenants,
		BackupInterval: backupInterval,
		BackupKeep:     backupKeep,
	}
	if err := server.Run(opts, listen); err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
	return 0
}

// resolveServerToken returns the single tenant's bearer token from the --token
// flag, else $YORE_TOKEN, else the contents of $YORE_TOKEN_FILE. An empty result
// means none was configured: which is an error only if no tokens file was given
// either; the caller decides.
func resolveServerToken(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if tok := os.Getenv("YORE_TOKEN"); tok != "" {
		return tok, nil
	}
	tf := os.Getenv("YORE_TOKEN_FILE")
	if tf == "" {
		return "", nil
	}
	b, err := os.ReadFile(tf)
	if err != nil {
		return "", fmt.Errorf("reading token file: %w", err)
	}
	return string(trimNL(b)), nil
}

// loadTenants reads named tenants from a JSON object {"name":"token", …} at
// path. An empty path means no named tenants: the single-token server. An
// unreadable file, invalid JSON, or an object with no tenants in it is a hard
// error: the server must never silently fall back to some other mode when
// multi-tenancy was intended.
func loadTenants(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading tokens file: %w", err)
	}
	var tenants map[string]string
	if err := json.Unmarshal(b, &tenants); err != nil {
		return nil, fmt.Errorf("parsing tokens file %s: %w", path, err)
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("tokens file %s defines no tenants", path)
	}
	return tenants, nil
}

// parseBackupInterval resolves the server backup interval. Empty/unset uses the
// 1h default; an explicit "0" disables backups; anything else must parse as a Go
// duration and be non-negative.
func parseBackupInterval(s string) (time.Duration, error) {
	if s == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid YORE_BACKUP_INTERVAL %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid YORE_BACKUP_INTERVAL %q: must not be negative", s)
	}
	return d, nil
}

// parseBackupKeep resolves how many backups to retain per tenant. Empty/unset
// uses the default of 3; anything else must parse as an integer >= 1.
func parseBackupKeep(s string) (int, error) {
	if s == "" {
		return 3, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid YORE_BACKUP_KEEP %q: %w", s, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("invalid YORE_BACKUP_KEEP %q: must be >= 1", s)
	}
	return n, nil
}

// serverPidFile resolves the pidfile path: explicit flag, else <db>.pid.
func serverPidFile(pidfile, db string) string {
	if pidfile != "" {
		return pidfile
	}
	return db + ".pid"
}

// runServerStop reads the server pidfile and sends SIGTERM for a graceful stop.
func runServerStop(db, pidfile string) int {
	pf := serverPidFile(pidfile, db)
	b, err := os.ReadFile(pf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yore server stop: no pidfile at %s (is the server running? in a container use `docker stop`)\n", pf)
		return 1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore server stop: bad pidfile:", err)
		return 1
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "yore server stop: signaling pid %d: %v\n", pid, err)
		return 1
	}
	fmt.Printf("sent graceful stop to server (pid %d)\n", pid)
	return 0
}

// trimNL trims one trailing newline (and any surrounding whitespace) from a
// token file's contents.
func trimNL(b []byte) []byte {
	for len(b) > 0 {
		c := b[len(b)-1]
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// runHealthcheck probes url's health endpoint. It backs the container HEALTHCHECK
// (distroless has no curl) but also runs interactively, so it prints a one-line
// result ("healthy: …" to stdout, "unhealthy: …" to stderr) and returns the exit
// code the container probe reads (0 = 2xx). With no url it defaults to the
// configured server (like `yore doctor`), so a bare `yore healthcheck` probes the
// server you actually sync with; it falls back to the local server address only
// when no server is configured (the container self-check). Pass --url only to
// probe somewhere else.
func runHealthcheck(url string) int {
	if url == "" {
		if cfg, err := config.Load(stateDir()); err == nil && cfg.ServerURL != "" {
			url = strings.TrimRight(cfg.ServerURL, "/") + "/v1/health"
		} else {
			url = "http://localhost:8080/v1/health"
		}
	}
	code, detail := server.HealthCheck(url)
	if code == 0 {
		fmt.Printf("healthy: %s (%s)\n", url, detail)
	} else {
		fmt.Fprintf(os.Stderr, "unhealthy: %s: %s\n", url, detail)
	}
	return code
}
