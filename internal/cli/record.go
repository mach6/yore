package cli

import (
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"yore/internal/config"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/redact"
	"yore/internal/spool"
)

// stateDir resolves the single-footprint state directory once per process.
func stateDir() string { return config.Dir() }

// cmdRecord is the shell-hook fast path. Contract: NEVER block the shell,
// NEVER print, ALWAYS exit 0. The command text arrives on stdin; metadata
// via flags. All it does is one fsync'd spool append plus a best-effort
// daemon poke.
func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	exit := fs.Int("exit", -1, "exit status of the command (-1 = unknown)")
	durMs := fs.Int64("duration-ms", -1, "wall time in milliseconds (-1 = unknown)")
	session := fs.String("session", "", "shell session id")
	cwd := fs.String("cwd", "", "working directory the command ran in")
	startMs := fs.Int64("start-ms", 0, "start time unix millis (0 = derive from now-duration)")
	if fs.Parse(args) != nil {
		return 0
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20)) // sanity cap: 1MiB of command text
	if err != nil {
		return 0
	}
	cmd := strings.TrimRight(string(raw), "\n")
	if strings.TrimSpace(cmd) == "" {
		return 0
	}

	// Recording gate. Any rejection returns silently (exit 0) — never break the
	// shell, never explain why (that would itself leak that a secret was typed).
	dir := stateDir()
	cfg, _ := config.Load(dir)
	// histignorespace: a leading space opts a command out of history unless the
	// user has explicitly turned that off. Checked on the raw text.
	if !cfg.RecordSpacePrefixedOn() && len(cmd) > 0 && (cmd[0] == ' ' || cmd[0] == '\t') {
		return 0
	}
	filter, _ := redact.New(cfg.IgnorePatterns, cfg.IgnoreDirs)
	if filter.SkipDir(*cwd) || filter.Sensitive(cmd) {
		return 0
	}

	start := *startMs
	if start == 0 {
		start = time.Now().UnixMilli()
		if *durMs > 0 {
			start -= *durMs
		}
	}
	r := rec.Record{
		ID:      rec.NewID(),
		Session: *session,
		Cmd:     cmd,
		Cwd:     *cwd,
		StartMs: start,
	}
	if *exit >= 0 {
		r.Exit = rec.IntPtr(*exit)
	}
	if *durMs >= 0 {
		r.DurMs = rec.Int64Ptr(*durMs)
	}

	if spool.Append(config.SpoolDir(dir), r) == nil {
		pokeDaemon(dir)
	}
	return 0
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
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	_ = proto.WriteMsg(conn, proto.Request{Op: proto.OpPing})
	// The response is irrelevant; the write itself resets the idle timer and
	// triggers ingest. Read just to avoid RST-before-processing races.
	buf := make([]byte, 64)
	_, _ = conn.Read(buf)
}

// spawnDaemon starts `yore daemon` fully detached. The daemon itself
// handles the already-running race (store lock loser exits silently).
func spawnDaemon() {
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
