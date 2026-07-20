package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"yore/internal/server"
)

// runServer runs the sync server in the foreground:
//
//	yore server [--db P] [--listen A] [--pidfile F]   run (foreground)
//	yore server stop [--pidfile F | --db P]           stop a running server
//
// The server also shuts down gracefully on SIGINT/SIGTERM (Ctrl-C, docker
// stop), so `stop` is a convenience for a backgrounded local server. The
// bearer token comes from --token, $YORE_TOKEN, or $YORE_TOKEN_FILE.
func runServer(db, listen, token, pidfile string) int {
	tok := token
	if tok == "" {
		tok = os.Getenv("YORE_TOKEN")
	}
	if tok == "" {
		if tf := os.Getenv("YORE_TOKEN_FILE"); tf != "" {
			b, err := os.ReadFile(tf)
			if err != nil {
				fmt.Fprintln(os.Stderr, "yore server: reading token file:", err)
				return 1
			}
			tok = string(trimNL(b))
		}
	}
	if tok == "" {
		fmt.Fprintln(os.Stderr, "yore server: no token set (use --token, $YORE_TOKEN, or $YORE_TOKEN_FILE)")
		return 1
	}

	// Record the PID so `yore server stop` can find us; clean it up on exit.
	pf := serverPidFile(pidfile, db)
	if err := os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "yore server: warning: cannot write pidfile:", err)
	} else {
		defer os.Remove(pf)
	}

	fmt.Fprintf(os.Stderr, "yore server listening on %s (db %s)\n", listen, db)
	if err := server.Run(server.Options{DBPath: db, Token: tok}, listen); err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
	return 0
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

// runHealthcheck is used by the container HEALTHCHECK (distroless has no curl).
func runHealthcheck(url string) int {
	return server.HealthCheck(url)
}
