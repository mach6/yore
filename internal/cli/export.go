package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"yore/internal/daemon"
	"yore/internal/proto"
)

// newExportCmd builds `yore export`: it dumps this host's recent history in a
// format the shell can seed its in-memory history from (zsh `fc -R`). Only the
// zsh extended-history format (--shell) is implemented for now.
func newExportCmd() *cobra.Command {
	var (
		shellFmt bool
		format   string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export recent local history for seeding the shell (fc -R / history -r)",
		Long: "export prints this host's recent history for the shell to seed its\n" +
			"in-memory history from (zsh `fc -R`, bash `history -r`). It is best-effort:\n" +
			"if the daemon is unreachable it prints nothing and exits 0 so shell startup\n" +
			"never fails.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return code(runExport(shellFmt, format, limit))
		},
	}
	cmd.Flags().BoolVar(&shellFmt, "shell", false, "emit shell-history lines for seeding (required)")
	cmd.Flags().StringVar(&format, "format", "zsh", "history format: zsh (extended) or bash (plain lines)")
	cmd.Flags().IntVar(&limit, "limit", 5000, "maximum number of records to export")
	_ = cmd.RegisterFlagCompletionFunc("format", fixedComp("zsh", "bash"))
	return cmd
}

// runExport queries the local daemon and prints records oldest-first in zsh
// extended-history format. Seeding is best-effort: a missing/erroring daemon is
// not an error (exit 0, no output) so the shell's startup never fails.
func runExport(shellFmt bool, format string, limit int) int {
	if !shellFmt {
		fmt.Fprintln(os.Stderr, "yore: only --shell is supported")
		return 1
	}
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		return 0 // best-effort: no daemon means nothing to seed
	}
	defer c.Close()

	resp, err := c.Query(proto.QueryReq{Scope: proto.ScopeLocal, Limit: limit, Dedupe: false})
	if err != nil {
		return 0 // best-effort: never break shell startup
	}

	// The daemon returns rows newest-first; both `fc -R` and `history -r` want
	// oldest-first so the most recent command ends up last (highest event number).
	bash := format == "bash"
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for i := len(resp.Rows) - 1; i >= 0; i-- {
		r := resp.Rows[i]
		if bash {
			fmt.Fprintln(w, bashHistoryLine(r.Cmd))
		} else {
			fmt.Fprintln(w, shellHistoryLine(r.StartMs, r.Cmd))
		}
	}
	return 0
}

// shellHistoryLine formats one record as a single zsh extended-history entry:
//
//	: <start_seconds>:0;<command>
//
// start_seconds is startMs/1000 (0 when startMs is 0). A command containing
// newlines is emitted as one logical entry by escaping each embedded newline as
// backslash-newline, which zsh's extended-history reader rejoins on read. A
// single-line command is emitted as-is after the ';'.
func shellHistoryLine(startMs int64, cmd string) string {
	secs := startMs / 1000
	if startMs == 0 {
		secs = 0
	}
	cmd = strings.ReplaceAll(cmd, "\n", "\\\n")
	return ": " + strconv.FormatInt(secs, 10) + ":0;" + cmd
}

// bashHistoryLine formats one record for bash `history -r`, which is line-
// oriented: each entry is one physical line. A multiline command is collapsed
// to a single runnable line (newlines → "; ") so it seeds as one entry rather
// than several bogus ones — a documented coarseness of bash takeover.
func bashHistoryLine(cmd string) string {
	return strings.ReplaceAll(strings.ReplaceAll(cmd, "\r\n", "\n"), "\n", "; ")
}
