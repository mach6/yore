package cli

import (
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"yore/internal/config"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/redact"
	"yore/internal/spool"
)

// stateDir resolves the single-footprint state directory once per process.
func stateDir() string { return config.Dir() }

// runRecord is the shell-hook fast path. Contract: NEVER block the shell,
// NEVER print, ALWAYS exit 0. The command text arrives on stdin; metadata
// via flags. All it does is one fsync'd spool append plus a best-effort
// daemon poke. Bad flags are swallowed by the cobra command (exit 0) before
// this runs, keeping the shell unbreakable.
func runRecord(exit int, durMs, startMs int64, session, cwd, tag string) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20)) // sanity cap: 1MiB of command text
	if err != nil {
		return
	}
	cmd := strings.TrimRight(string(raw), "\n")
	if strings.TrimSpace(cmd) == "" {
		return
	}

	dir := stateDir()
	start := startMs
	if start == 0 {
		start = time.Now().UnixMilli()
		if durMs > 0 {
			start -= durMs
		}
	}
	tagVal := tag
	if tagVal == "" {
		tagVal = executorTag()
	}
	r := rec.Record{
		Session: session,
		Cmd:     cmd,
		Cwd:     cwd,
		StartMs: start,
		Tag:     tagVal,
	}
	if exit >= 0 {
		r.Exit = rec.IntPtr(exit)
	}
	if durMs >= 0 {
		r.DurMs = rec.Int64Ptr(durMs)
	}
	spoolRecord(dir, r)
}

// spoolRecord applies the recording gate and, if the command passes, assigns an
// id, spools the record (fsync'd), and pokes the daemon. It is the shared tail
// of every capture path — the shell fast path (runRecord) and the agent hooks
// (internal/cli/claudecode.go). It NEVER blocks or prints and treats any
// rejection or error as a silent no-op, so a capture can never disrupt the
// shell or an agent.
func spoolRecord(dir string, r rec.Record) {
	cfg, _ := config.Load(dir)
	// histignorespace: a leading space opts a command out of history unless the
	// user has turned that off. Any rejection returns silently — never explain
	// why (that would itself leak that a secret was typed).
	if !cfg.RecordSpacePrefixed && r.Cmd != "" && (r.Cmd[0] == ' ' || r.Cmd[0] == '\t') {
		return
	}
	filter, _ := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs)
	if filter.SkipDir(r.Cwd) || filter.Sensitive(r.Cmd) {
		return
	}
	if r.ID == "" {
		r.ID = rec.NewID()
	}
	if r.StartMs == 0 {
		r.StartMs = time.Now().UnixMilli()
	}
	if spool.Append(config.SpoolDir(dir), r) == nil {
		pokeDaemon(dir)
	}
}

// pokeDaemon nudges a running daemon to ingest the spool, spawning one if
// none answers. Every step is best-effort with tight deadlines: worst case
// (~150ms) is still invisible because record itself runs backgrounded.
func pokeDaemon(dir string) {
	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 50*time.Millisecond)
	if err != nil {
		spawnDaemon()
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	_ = proto.WriteMsg(conn, proto.Request{Op: proto.OpPing})
	// The response is irrelevant; the write itself resets the idle timer and
	// triggers ingest. Read just to avoid RST-before-processing races.
	buf := make([]byte, 64)
	_, _ = conn.Read(buf)
}

// spawnDaemon starts `yore daemon` fully detached. The daemon itself
// handles the already-running race (store lock loser exits silently).
func spawnDaemon() {
	// Never re-exec under `go test`: os.Executable() is the test binary, so
	// `<testbin> daemon` re-runs the whole suite (the arg is not a -run filter),
	// which pokes the daemon again and re-spawns — a detached fork bomb that
	// pegs every core and exhausts RAM. Production binaries return false here.
	if testing.Testing() {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
