package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"yore/internal/proto"
	"yore/internal/shell"
	"yore/internal/tui/browse"
)

// exitErr carries a precise process exit code out through cobra's RunE. Each
// subcommand's logic returns an int; the RunE wrapper turns a nonzero code into
// an exitErr (Error is empty so cobra, with errors silenced, prints nothing),
// and Run maps it back to the process exit status. This preserves the exact
// per-command exit codes the shell hooks and scripts depend on.
type exitErr int

func (e exitErr) Error() string { return "" }

// code wraps a run* function's int result for RunE: 0 means success (nil),
// anything else propagates as the process exit code.
func code(n int) error {
	if n == 0 {
		return nil
	}
	return exitErr(n)
}

// Run builds the cobra command tree, executes args, and returns the process
// exit code. Command logic lives in run* functions returning an int; RunE
// wraps those via code(). Cobra's own errors (unknown command, bad flags) are
// printed here (errors/usage are silenced on the tree) and mapped to exit 2,
// staying close to the previous stdlib-flag behavior.
func Run(args []string) int {
	root := newRootCmd()
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		// Bare `yore` prints help via cobra but must still exit non-zero (2),
		// matching the previous dispatcher.
		if len(args) == 0 {
			return 2
		}
		return 0
	}

	var ee exitErr
	if errors.As(err, &ee) {
		return int(ee)
	}
	// A cobra-level usage error (unknown command/subcommand, bad flag/args).
	fmt.Fprintln(os.Stderr, "Error:", err)
	fmt.Fprintln(os.Stderr, "Run 'yore --help' for usage.")
	return 2
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "yore",
		Short: "Your shell history, everywhere, encrypted",
		Long: "yore records your shell history, syncs it end-to-end encrypted across your\n" +
			"machines, and serves fast search from a local background daemon.",
		Version:       Version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	// `yore version` and `yore --version` print the same "yore <ver>" line.
	root.SetVersionTemplate("yore {{.Version}}\n")

	root.AddCommand(
		newRecordCmd(),
		newFilterCmd(),
		newExportCmd(),
		newSearchCmd(),
		newBrowseCmd(),
		newStatsCmd(),
		newAgentsCmd(),
		newDaemonCmd(),
		newImportCmd(),
		newInitCmd(),
		newUninitCmd(),
		newHookCmd(),
		newMcpServeCmd(),
		newTagCmd(),
		newSetupCmd(),
		newDevicesCmd(),
		newRecoverCmd(),
		newServerCmd(),
		newHealthcheckCmd(),
		newSyncCmd(),
		newGenIDCmd(),
		newStatusCmd(),
		newStopCmd(),
		newDoctorCmd(),
		newGetConfigCmd(),
		newSetConfigCmd(),
		newVersionCmd(),
	)
	strictSubcommands(root)
	return root
}

// strictSubcommands makes every command group reject an unknown subcommand the
// way the root does.
//
// Cobra's default argument check (legacyArgs) only looks for unknown
// subcommands on the root, so every group below it silently swallowed the
// argument instead: `yore tag refactor` fell through to the help text and
// exited 0, and `yore daemon bogus` ignored the word and started the daemon.
// One rule for the whole tree is the only version of this a user can predict.
//
// Groups that declare their own Args are left alone — they have already said
// what they accept — as are commands that take positional arguments.
func strictSubcommands(c *cobra.Command) {
	for _, sub := range c.Commands() {
		strictSubcommands(sub)
	}
	if !c.HasSubCommands() || !c.HasParent() || c.Args != nil {
		return
	}
	c.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return fmt.Errorf("unknown command %q for %q%s",
			args[0], cmd.CommandPath(), suggestionsFor(cmd, args[0]))
	}
	if !c.Runnable() {
		// Cobra bails out to the help text as soon as it sees a group with
		// nothing to run — before it ever validates the arguments. A bare group
		// like `yore tag` still has to print its help, so it gets a Run that does
		// exactly that, and the check above finally gets to see the argument.
		c.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	}
}

// suggestionsFor renders cobra's "did you mean" block for an unknown
// subcommand, matching the wording the root already produces.
func suggestionsFor(c *cobra.Command, typedName string) string {
	names := c.SuggestionsFor(typedName)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nDid you mean this?\n")
	for _, n := range names {
		fmt.Fprintf(&b, "\t%v\n", n)
	}
	return b.String()
}

// --- fast path -------------------------------------------------------------

func newRecordCmd() *cobra.Command {
	var (
		exit                   int
		durMs, startMs         int64
		session, cwd, executor string
	)
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Capture one command from the shell hook (reads command text on stdin)",
		Long: "record is the shell-hook fast path: it reads the command text on stdin,\n" +
			"spools it, nudges the daemon, and always exits 0 so the shell is never blocked.",
		// Keep it the fast path: no completion/help machinery, and bad flags are
		// swallowed to exit 0 (see the flag-error func below) rather than erroring.
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			runRecord(exit, durMs, startMs, session, cwd, executor)
			return nil
		},
	}
	cmd.Flags().IntVar(&exit, "exit", -1, "exit status of the command (-1 = unknown)")
	cmd.Flags().Int64Var(&durMs, "duration-ms", -1, "wall time in milliseconds (-1 = unknown)")
	cmd.Flags().StringVar(&session, "session", "", "shell session id")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory the command ran in")
	cmd.Flags().Int64Var(&startMs, "start-ms", 0, "start time unix millis (0 = derive from now-duration)")
	cmd.Flags().StringVar(&executor, "executor", "", "agent that ran it (default: auto-detect, else interactive)")
	// Never break the shell on a malformed flag: swallow it and exit 0.
	cmd.SetFlagErrorFunc(func(*cobra.Command, error) error { return exitErr(0) })
	return cmd
}

// --- history ---------------------------------------------------------------

func newSearchCmd() *cobra.Command {
	var (
		query, scope, executor, tag, sortMode string
		headless, fuzzy, noHost               bool
		limit                                 int
	)
	cmd := &cobra.Command{
		Use:   "search [query...]",
		Short: "Interactive history search (Ctrl-R); --headless for scripts",
		Long: "search opens the inline Ctrl-R search TUI by default (the accepted command\n" +
			"is printed to stdout). --headless prints matching commands as plain lines\n" +
			"for scripts and `yore search --headless foo | grep`-style piping.",
		Example: "  yore search --headless docker\n  yore search --scope all --sort frecency kubectl",
		RunE: func(_ *cobra.Command, args []string) error {
			q := query
			if q == "" {
				q = strings.Join(args, " ")
			}
			if headless {
				return code(headlessSearch(q, scope, executor, tag, sortMode, fuzzy, limit, headlessShowHost(scope, noHost)))
			}
			return code(interactiveSearch(q, scope, executor, tag))
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "initial query")
	cmd.Flags().BoolVar(&headless, "headless", false, "print matches to stdout instead of the TUI")
	cmd.Flags().IntVar(&limit, "limit", 0, "headless: max results (default 200)")
	cmd.Flags().BoolVar(&noHost, "no-host", false, "headless: hide the host column (shown only for --scope all)")
	cmd.Flags().StringVar(&scope, "scope", proto.ScopeLocal, "search scope: local|all|host|session|cwd|workspace")
	cmd.Flags().StringVar(&executor, "executor", "", "filter by executor (e.g. claude-code)")
	cmd.Flags().StringVar(&tag, "tag", "", "filter by a user tag (a label you applied; for agents use --executor)")
	cmd.Flags().StringVar(&sortMode, "sort", "", "sort: recency (default) or frecency")
	cmd.Flags().BoolVar(&fuzzy, "fuzzy", false, "subsequence (fzf-style) matching")

	_ = cmd.RegisterFlagCompletionFunc("scope", fixedComp(
		proto.ScopeLocal, proto.ScopeAll, proto.ScopeHost,
		proto.ScopeSession, proto.ScopeCwd, proto.ScopeWorkspace))
	_ = cmd.RegisterFlagCompletionFunc("sort", fixedComp("recency", "frecency"))
	return cmd
}

func newBrowseCmd() *cobra.Command {
	var acceptFile string
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Full-screen history browser",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runBrowse(acceptFile, browse.StartBrowse))
		},
	}
	cmd.Flags().StringVar(&acceptFile, "accept-file", "",
		"write the accepted command to this file instead of stdout (used by the `h` shell function)")
	return cmd
}

// newStatsCmd and newAgentsCmd open the browser directly on one of its
// full-screen views, so the two screens people actually go looking for are one
// command away instead of `yore browse` plus a keystroke. Both are the same
// program: Esc drops through to the browse table, and every key works as usual.
func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Open the browser on the full-screen stats view",
		Long: "stats opens the history browser directly on its stats screen: KPIs,\n" +
			"top programs / commands / directories, per-host and per-executor\n" +
			"breakdowns, and the activity, daily-trend and hour-of-day graphs.\n\n" +
			"Keys 1-5 pick the time window; Esc drops through to the browse table.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runBrowse("", browse.StartStats))
		},
	}
}

func newAgentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "Open the browser on the agent explorer",
		Long: "agents opens the history browser directly on its agent explorer: the\n" +
			"executors that have run commands, the prompts they were given, the exact\n" +
			"commands each prompt triggered, and the details of whatever is selected.\n\n" +
			"Picking an executor in the sidebar filters the other panes; keys 1-5 pick\n" +
			"the time window; Esc drops through to the browse table.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runBrowse("", browse.StartAgents))
		},
	}
}

// --- daemon ----------------------------------------------------------------

func newDaemonCmd() *cobra.Command {
	// runFn binds a fresh --idle value for both `daemon` and `daemon run`.
	runFn := func(idle *time.Duration) func(*cobra.Command, []string) error {
		return func(*cobra.Command, []string) error { return code(runDaemon(*idle)) }
	}

	var idle time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the background daemon (normally auto-spawned)",
		Long: "daemon runs the background daemon in the foreground. It is normally\n" +
			"auto-spawned by the record hook; run it directly to debug or to control\n" +
			"the idle timeout.",
		RunE: runFn(&idle),
	}
	cmd.Flags().DurationVar(&idle, "idle", 0, "idle timeout before exiting (default from config, 30m)")

	var runIdle time.Duration
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (alias of `yore daemon`)",
		RunE:  runFn(&runIdle),
	}
	runCmd.Flags().DurationVar(&runIdle, "idle", 0, "idle timeout before exiting (default from config, 30m)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Ask a running daemon to exit gracefully",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(daemonStop()) },
	}
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status (alias of `yore status`)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(runStatus()) },
	}
	cmd.AddCommand(runCmd, stopCmd, statusCmd)
	return cmd
}

// --- import ----------------------------------------------------------------

func newImportCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "import [auto | FILE...]",
		Short: "Import existing shell history files",
		Long: "import ingests existing shell history. `yore import auto` finds and imports\n" +
			"the usual files (~/.zsh_history, ~/.bash_history, …); `yore import --format\n" +
			"zsh|bash FILE...` imports explicit files. Re-runs are no-ops.",
		Example: "  yore import auto\n  yore import --format zsh ~/.zsh_history",
		RunE: func(_ *cobra.Command, args []string) error {
			return code(runImport(format, args))
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			// First positional: offer `auto` plus normal file completion.
			if len(args) == 0 {
				return []cobra.Completion{cobra.CompletionWithDesc("auto", "auto-detect and import the usual history files")},
					cobra.ShellCompDirectiveDefault
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "history format for explicit files: zsh|bash")
	_ = cmd.RegisterFlagCompletionFunc("format", fixedComp("zsh", "bash"))
	return cmd
}

// --- init ------------------------------------------------------------------

func newInitCmd() *cobra.Command {
	var (
		noAliases     bool
		bin           string
		mode          string
		project, prnt bool
	)
	cmd := &cobra.Command{
		Use:   "init zsh|bash|claude-code",
		Short: "Set up shell or agent integration",
		Long: "init wires yore into a shell or an agent.\n\n" +
			"Shells print a script to eval in your rc file:\n" +
			"  eval \"$(yore init zsh)\"   # ~/.zshrc\n" +
			"  eval \"$(yore init bash)\"  # ~/.bashrc\n" +
			"Integration mode comes from --mode, else config.integration (default\n" +
			"takeover). Modes: takeover (single source of truth), coexist, capture.\n\n" +
			"claude-code installs Claude Code hooks that record every Bash command\n" +
			"the agent runs (tagged claude-code, with exit status, traced to its\n" +
			"prompt) — an agent's non-interactive shell never loads the rc hooks —\n" +
			"and registers yore's MCP server so the agent can query history back:\n" +
			"  yore init claude-code             # ~/.claude/settings.json + ~/.claude.json\n" +
			"  yore init claude-code --project   # ./.claude/settings.json + ./.mcp.json\n" +
			"  yore init claude-code --print     # print the JSON, install by hand\n\n" +
			"cursor installs Cursor capture hooks (records the commands its agent\n" +
			"runs, tagged cursor, traced to prompts) and registers the MCP server:\n" +
			"  yore init cursor                  # ~/.cursor/hooks.json + ~/.cursor/mcp.json\n\n" +
			"opencode installs a capture plugin (records its agent's commands,\n" +
			"tagged opencode, with exit + prompt tracing) and registers the MCP server:\n" +
			"  yore init opencode                # ~/.config/opencode/{plugins/yore.js,opencode.json}\n\n" +
			"codex installs PostToolUse + UserPromptSubmit hooks (tagged codex, traced\n" +
			"to prompts) and registers the MCP server:\n" +
			"  yore init codex                   # ~/.codex/config.toml\n\n" +
			"devin installs PreToolUse + PostToolUse + UserPromptSubmit hooks for the\n" +
			"Devin CLI's exec tool (tagged devin, with exit + duration, traced to\n" +
			"prompts) and registers yore's MCP server, in Devin's config.json:\n" +
			"  yore init devin                   # ~/.config/devin/config.json\n" +
			"  yore init devin --project         # ./.devin/config.json\n" +
			"  yore init devin --print           # print the JSON, install by hand",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []cobra.Completion{"zsh", "bash", "claude-code", "cursor", "opencode", "codex", "devin"},
		RunE: func(_ *cobra.Command, args []string) error {
			switch args[0] {
			case "claude-code":
				return code(runInitClaudeCode(bin, project, prnt))
			case "cursor":
				return code(runInitCursor(bin, project))
			case "opencode":
				return code(runInitOpenCode(bin, project))
			case "codex":
				return code(runInitCodex(bin, project))
			case "devin":
				return code(runInitDevin(bin, project, prnt))
			}
			return code(runInit(args[0], bin, mode, noAliases))
		},
	}
	cmd.Flags().BoolVar(&noAliases, "no-aliases", false, "omit the h/hs convenience aliases (shells)")
	cmd.Flags().StringVar(&bin, "bin", shell.DefaultBin, "binary name or path the hooks should invoke")
	cmd.Flags().StringVar(&mode, "mode", "", "integration mode: takeover|coexist|capture (shells; default: config)")
	cmd.Flags().BoolVar(&project, "project", false, "claude-code/devin: write the project-scoped config instead of the user file")
	cmd.Flags().BoolVar(&prnt, "print", false, "claude-code/devin: print the config JSON instead of writing it")
	_ = cmd.RegisterFlagCompletionFunc("mode", fixedComp("takeover", "coexist", "capture"))
	return cmd
}

// newTagCmd groups the user-tag commands: freeform labels on commands and
// sessions that sync end-to-end. Which agent ran a command is a separate axis
// (--executor); these are the labels you apply yourself.
func newTagCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Manage freeform tags on commands and sessions",
		Long: "tag applies freeform labels to commands and sessions; a record can carry\n" +
			"several. With no --command or --session, `tag add` labels the shell you\n" +
			"are in — which covers everything it has run and everything it runs next.\n\n" +
			"Filter with `yore search --tag <name>`, or t in the browser. Tags sync\n" +
			"end-to-end like everything else.\n\n" +
			"Tags are not executors: which agent ran a command is recorded separately\n" +
			"and filtered with `--executor claude-code`, never `--tag`.",
	}
	var desc, listScope string
	list := &cobra.Command{
		Use: "list", Short: "List known tags with how many commands carry each",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return code(runTagList(listScope)) },
	}
	list.Flags().StringVar(&listScope, "scope", "local", "count over: local|all")
	_ = list.RegisterFlagCompletionFunc("scope", fixedComp("local", "all"))
	create := &cobra.Command{
		Use: "create <name>", Short: "Create a tag (name + optional description)",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return code(runTagCreate(args[0], desc)) },
	}
	create.Flags().StringVarP(&desc, "description", "d", "", "optional tag description")

	var command, session string
	add := &cobra.Command{
		Use: "add <name>", Short: "Add a tag to a command/session (default: current session)",
		Aliases: []string{"associate"}, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return code(runTagAssociate(args[0], command, session, false))
		},
	}
	rm := &cobra.Command{
		Use: "rm <name>", Short: "Remove a tag from a command/session (default: current session)",
		Aliases: []string{"remove"}, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return code(runTagAssociate(args[0], command, session, true))
		},
	}
	for _, c := range []*cobra.Command{add, rm} {
		c.Flags().StringVar(&command, "command", "", "target command id (default: current session)")
		c.Flags().StringVar(&session, "session", "", "target session id (default: current session)")
	}
	cmd.AddCommand(list, create, add, rm)
	return cmd
}

// newMcpServeCmd runs the Model Context Protocol server on stdio. Hidden: it is
// launched by an agent's MCP config (see `yore init claude-code`), not by users.
func newMcpServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp-serve",
		Short:  "Serve shell history to AI agents over MCP (stdio JSON-RPC)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE:   func(*cobra.Command, []string) error { return code(runMcpServe()) },
	}
}

// newHookCmd is the agent-hook capture entrypoint: hidden because it is invoked
// by an agent's hook config (see `yore init claude-code`), not typed by users.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Ingest an agent hook payload (invoked by agent hook config)",
		Hidden: true,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "claude-code-pre",
		Short: "Stamp a Bash command's start time from a Claude Code PreToolUse hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookClaudeCodePre(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "claude-code",
		Short: "Record a Bash command from a Claude Code PostToolUse hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookClaudeCode(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "claude-code-failure",
		Short: "Record a failed Bash command from a Claude Code PostToolUseFailure hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookClaudeCodeFailure(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "claude-prompt",
		Short: "Record a prompt from a Claude Code UserPromptSubmit hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookClaudePrompt(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "cursor",
		Short: "Record a command from a Cursor afterShellExecution hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookCursor(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "cursor-prompt",
		Short: "Record a prompt from a Cursor beforeSubmitPrompt hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookCursorPrompt(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "opencode",
		Short: "Record a command from the OpenCode capture plugin (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookOpenCode(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "opencode-prompt",
		Short: "Record a prompt from the OpenCode capture plugin (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookOpenCodePrompt(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "codex",
		Short: "Record a Bash command from a Codex PostToolUse hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookCodex(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "codex-prompt",
		Short: "Record a prompt from a Codex UserPromptSubmit hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookCodexPrompt(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "devin-pre",
		Short: "Stamp an exec command's start time from a Devin PreToolUse hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookDevinPre(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "devin",
		Short: "Record an exec command from a Devin PostToolUse hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookDevin(); return nil },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "devin-prompt",
		Short: "Record a prompt from a Devin UserPromptSubmit hook (reads JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { runHookDevinPrompt(); return nil },
	})
	return cmd
}

// --- enrollment ------------------------------------------------------------

func newSetupCmd() *cobra.Command {
	var server, token, name, integration string
	var pin, clearPin bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Enroll this machine with a sync server",
		Long: "setup enrolls this machine using a single-use enrollment token. The first\n" +
			"machine forms the history group and is shown a recovery phrase; every later\n" +
			"machine registers as pending and must be approved from one already enrolled.\n" +
			"Get a token with `yore devices token` on an enrolled machine; the first\n" +
			"machine uses the server's own token. Nothing is saved if enrollment fails.\n\n" +
			"--pin captures and pins the server's TLS certificate (do it on a trusted\n" +
			"network): thereafter the client refuses any other cert, defeating a\n" +
			"TLS-inspecting proxy — but it also won't sync through one. --clear-pin removes\n" +
			"a previously pinned certificate.",
		RunE: func(*cobra.Command, []string) error {
			return code(runSetup(server, token, name, integration, pin, clearPin))
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "server URL")
	cmd.Flags().StringVar(&token, "token", "", "single-use enrollment token (else $YORE_TOKEN, else prompt)")
	cmd.Flags().StringVar(&name, "name", "", "device name (default: hostname)")
	cmd.Flags().StringVar(&integration, "integration", "", "shell integration: takeover|coexist|capture (default: prompt/takeover)")
	cmd.Flags().BoolVar(&pin, "pin", false, "pin the server's TLS certificate (capture it now)")
	cmd.Flags().BoolVar(&clearPin, "clear-pin", false, "remove a previously pinned server certificate")
	_ = cmd.RegisterFlagCompletionFunc("integration", fixedComp("takeover", "coexist", "capture"))
	return cmd
}

// newDevicesCmd opens the browser on its devices pane — the one place devices
// are managed. It used to also print the list and carry `approve`/`revoke`
// subcommands, which was a second implementation of the same three actions,
// with its own confirmation rules and its own idea of what a device looks like.
// `token` stays: minting an enrollment credential is a thing you pipe
// (`TOKEN=$(yore devices token)`), not a thing you manage.
func newDevicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage enrolled machines (opens the browser's devices pane)",
		Long: "devices opens the history browser on its devices pane: approve a pending\n" +
			"machine with a, revoke one with x (both ask first), r refetches. It is the\n" +
			"same screen the browser reaches with D.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runBrowse("", browse.StartDevices))
		},
	}
	token := &cobra.Command{
		Use:   "token",
		Short: "Mint a single-use enrollment token for another machine",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runDevicesToken())
		},
	}
	cmd.AddCommand(token)
	return cmd
}

func newRecoverCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Regain access with your recovery phrase",
		Long: "recover is the way back when no enrolled machine survives. It asks for the\n" +
			"recovery phrase shown when the group was created, uses it to unwrap the\n" +
			"History Key, then enrolls this machine and admits it directly (no other\n" +
			"device is left to approve it).\n\n" +
			"The phrase is the only thing you need: it also authorizes the enrollment,\n" +
			"because the machines you lost still count as enrolled on the server.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runRecover(server))
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "server URL")
	return cmd
}

// --- server ----------------------------------------------------------------

func newServerCmd() *cobra.Command {
	var db, listen, token, pidfile string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the sync server (normally in a container)",
		Long: "server runs the sync server in the foreground. It hosts either ONE tenant,\n" +
			"whose token comes from --token, $YORE_TOKEN, or $YORE_TOKEN_FILE and whose db\n" +
			"is --db, or a set of NAMED tenants from $YORE_TOKENS_FILE (a JSON object\n" +
			"{\"name\":\"token\", …}), each with its own db under <dir(--db)>/tenants/.\n" +
			"The two are mutually exclusive — set one or the other, never both.\n\n" +
			"It shuts down gracefully on SIGINT/SIGTERM; `yore server stop` is a\n" +
			"convenience for a local instance.",
		RunE: func(*cobra.Command, []string) error {
			return code(runServer(db, listen, token, pidfile))
		},
	}
	cmd.Flags().StringVar(&db, "db", "yore-server.db", "server database (with $YORE_TOKENS_FILE: roots tenants/ and backups/)")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "listen address")
	cmd.Flags().StringVar(&token, "token", "", "single tenant's bearer token (else $YORE_TOKEN / $YORE_TOKEN_FILE)")
	cmd.Flags().StringVar(&pidfile, "pidfile", "", "write the server PID here (default <db>.pid; enables `yore server stop`)")

	var stopDB, stopPidfile string
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop a running server via its pidfile",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runServerStop(stopDB, stopPidfile))
		},
	}
	stop.Flags().StringVar(&stopDB, "db", "yore-server.db", "server database (to locate <db>.pid)")
	stop.Flags().StringVar(&stopPidfile, "pidfile", "", "pidfile written by `yore server`")
	cmd.AddCommand(stop)
	return cmd
}

func newHealthcheckCmd() *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe a server's /v1/health (container HEALTHCHECK)",
		// Hidden because nobody types it: the container's HEALTHCHECK runs it, and
		// the distroless image ships no curl, so the binary probing itself is the
		// only option. A person asking whether the server is up wants `yore doctor`,
		// which probes it and explains what it found. Listing both only invites the
		// question of which one to use.
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return code(runHealthcheck(url))
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "health endpoint (default: the configured server, else http://localhost:8080/v1/health)")
	return cmd
}

// --- misc ------------------------------------------------------------------

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Force an immediate push/pull with the server",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(runSync()) },
	}
}

func newGenIDCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gen-id",
		Short: "Print a fresh ULID (used for session ids)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(runGenID()) },
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon and store status",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(runStatus()) },
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the local background daemon",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(daemonStop()) },
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run environment diagnostics",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return code(runDoctor()) },
	}
}

func newGetConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-config <key>",
		Short: "Print the effective value of a config key",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return code(runGetConfig(args[0])) },
	}
}

func newSetConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-config <key> <value>",
		Short: "Set a config key and save config.toml",
		Args:  cobra.ExactArgs(2),
		RunE:  func(_ *cobra.Command, args []string) error { return code(runSetConfig(args[0], args[1])) },
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("yore " + Version)
			return nil
		},
	}
}

// --- completion helpers ----------------------------------------------------

// fixedComp returns a flag-completion function offering a fixed set of values
// with no file completion.
func fixedComp(values ...string) cobra.CompletionFunc {
	return func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}
