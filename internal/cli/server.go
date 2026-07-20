package cli

import (
	"flag"
	"fmt"
	"os"

	"yore/internal/server"
)

// cmdServer runs the sync server (normally in a container). The bearer token
// comes from --token, $YORE_TOKEN, or a file named by $YORE_TOKEN_FILE
// (Swarm secrets are file-mounted); an empty token is refused.
func cmdServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	db := fs.String("db", "yore-server.db", "path to the server database")
	listen := fs.String("listen", ":8080", "listen address")
	token := fs.String("token", "", "bearer token (else $YORE_TOKEN / $YORE_TOKEN_FILE)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok := *token
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

	fmt.Fprintf(os.Stderr, "yore server listening on %s (db %s)\n", *listen, *db)
	if err := server.Run(server.Options{DBPath: *db, Token: tok}, *listen); err != nil {
		fmt.Fprintln(os.Stderr, "yore server:", err)
		return 1
	}
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

// cmdHealthcheck is used by the container HEALTHCHECK (distroless has no curl).
func cmdHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fs.String("url", "http://localhost:8080/v1/health", "health endpoint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return server.HealthCheck(*url)
}
