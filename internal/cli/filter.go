package cli

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"yore/internal/config"
	"yore/internal/redact"
)

// newFilterCmd builds `yore filter`: the redaction gate for the shell's own
// native history. It is called synchronously from the shell's add-history hook
// (zsh's zshaddhistory) with the command text on stdin. It exits 0 if yore
// WOULD record the command (clean → keep it in the shell's history) or 1 if it
// should be dropped (secret, ignored directory, or space-prefixed). It must be
// fast and print nothing on any path.
func newFilterCmd() *cobra.Command {
	var cwd string
	cmd := &cobra.Command{
		Use:   "filter",
		Short: "Redaction gate for the shell's native history (reads command on stdin)",
		Long: "filter reads a command on stdin and exits 0 if yore would record it\n" +
			"(clean → keep it in the shell's own history) or 1 if it should be\n" +
			"dropped (secret, ignored directory, or space-prefixed). It prints\n" +
			"nothing and is called synchronously from the shell's add-history hook.",
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return code(runFilter(cwd))
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "the shell's working directory ($PWD)")
	return cmd
}

// runFilter reads the command text on stdin and returns the process exit code:
// 0 = keep (yore would record it), 1 = drop. It mirrors runRecord's gate.
func runFilter(cwd string) int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20)) // sanity cap: 1MiB
	if err != nil {
		return 0
	}
	cmd := strings.TrimRight(string(raw), "\n")
	if strings.TrimSpace(cmd) == "" {
		// An empty command never actually reaches here; nothing to drop.
		return 0
	}
	dir := stateDir()
	cfg, _ := config.Load(dir)
	if filterDecision(dir, cfg, cmd, cwd) {
		return 0
	}
	return 1
}

// filterDecision reports whether the SHELL should keep the command in its own
// history (true = keep, false = drop). dir is the state directory whose
// redact.yml supplies the rules (built-ins on fallback). Callers must have
// already handled empty input. It is strictly harsher than yore's own gate,
// and has to be: yore keeps a secret-bearing command with the credential
// masked, but this hook can only accept or reject; zsh gives it no way to
// rewrite the line it is about to write. So a command yore would redact is
// dropped from the shell's history entirely; yore's copy, redacted, is still
// there to search.
func filterDecision(dir string, cfg config.Config, cmd, cwd string) bool {
	// histignorespace: a leading space/tab opts a command out of history unless
	// the user has explicitly turned that off. Checked on the raw text.
	if !cfg.RecordSpacePrefixed && cmd != "" && (cmd[0] == ' ' || cmd[0] == '\t') {
		return false
	}
	filter, _ := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs)
	if filter.SkipDir(cwd) || filter.Sensitive(cmd) {
		return false
	}
	return true
}
