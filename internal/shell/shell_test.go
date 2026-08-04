package shell_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/shell"
)

var update = flag.Bool("update", false, "regenerate golden files under testdata/")

// renderCase is one (shell, mode, aliases) combination we render a golden for.
type renderCase struct {
	name   string
	sh     string
	opts   shell.Options
	golden string
}

// cases covers each shell × integration mode, with the alias toggle where it is
// meaningful (capture mode emits no aliases regardless).
var cases = func() []renderCase {
	cs := make([]renderCase, 0, 15)
	for _, sh := range []string{"zsh", "bash", "fish"} {
		for _, mode := range []string{"takeover", "coexist"} {
			for _, al := range []bool{true, false} {
				suffix := "aliases"
				if !al {
					suffix = "noaliases"
				}
				cs = append(cs, renderCase{
					name:   sh + "_" + mode + "_" + suffix,
					sh:     sh,
					opts:   shell.Options{Aliases: al, Bin: "yore", Mode: mode},
					golden: "init_" + sh + "_" + mode + "_" + suffix + ".golden",
				})
			}
		}
		cs = append(cs, renderCase{
			name:   sh + "_capture",
			sh:     sh,
			opts:   shell.Options{Aliases: true, Bin: "yore", Mode: "capture"},
			golden: "init_" + sh + "_capture.golden",
		})
	}
	return cs
}()

func TestInitGolden(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := shell.Init(tc.sh, tc.opts)
			require.NoError(t, err)
			path := filepath.Join("testdata", tc.golden)
			if *update {
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "reading golden (run `go test -run TestInitGolden -update`)")
			require.Equal(t, string(want), got, "rendered %s does not match its golden", tc.golden)
		})
	}
}

// TestSyntax writes each emitted script to disk and asks the real shell to
// parse it (`-n`), so a syntactically broken hook can never ship.
func TestSyntax(t *testing.T) {
	if *update {
		t.Skip("skipping syntax check during -update")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, err := exec.LookPath(tc.sh)
			if err != nil {
				t.Skipf("%s not installed: %v", tc.sh, err)
			}
			script, err := shell.Init(tc.sh, tc.opts)
			require.NoError(t, err)
			f := filepath.Join(t.TempDir(), "init."+tc.sh)
			require.NoError(t, os.WriteFile(f, []byte(script), 0o644))
			out, err := exec.Command(bin, "-n", f).CombinedOutput()
			require.NoError(t, err, "%s -n rejected the emitted script:\n%s", tc.sh, out)
		})
	}
}

// TestAbsoluteBin ensures an absolute Bin path is substituted everywhere it is
// invoked and the bare default never leaks in.
func TestAbsoluteBin(t *testing.T) {
	const bin = "/usr/local/bin/yore"
	// zsh/bash join the headless query with bash's "$*"; fish has no such
	// operator, so _yore_hs joins $argv itself into $query first.
	headless := map[string]string{
		"zsh":  `command ` + bin + ` search --headless --scope "$scope" "$*"`,
		"bash": `command ` + bin + ` search --headless --scope "$scope" "$*"`,
		"fish": `command ` + bin + ` search --headless --scope "$scope" "$query"`,
	}
	for _, sh := range []string{"zsh", "bash", "fish"} {
		t.Run(sh, func(t *testing.T) {
			got, err := shell.Init(sh, shell.Options{Aliases: true, Bin: bin})
			require.NoError(t, err)
			for _, w := range []string{
				"command " + bin + " gen-id",
				"command " + bin + " record",
				"command " + bin + ` search --query "`,
				"command " + bin + " browse",
				headless[sh],
			} {
				require.Contains(t, got, w, "missing invocation")
			}
			require.NotContains(t, got, "command yore ", "leaked bare default binary")
		})
	}
}

func TestAliasToggle(t *testing.T) {
	for _, sh := range []string{"zsh", "bash", "fish"} {
		t.Run(sh, func(t *testing.T) {
			on, err := shell.Init(sh, shell.Options{Aliases: true, Bin: "yore"})
			require.NoError(t, err)
			off, err := shell.Init(sh, shell.Options{Aliases: false, Bin: "yore"})
			require.NoError(t, err)
			require.Contains(t, on, "command yore browse", "aliases:true should emit the alias block")
			require.NotContains(t, off, "command yore browse", "aliases:false must not emit the alias block")
		})
	}
}

// TestModes checks the mode-specific blocks: takeover disables persistent
// history and gates it; capture emits neither bindings nor takeover.
func TestModes(t *testing.T) {
	tests := []struct {
		sh, mode   string
		mustHave   []string
		mustNotHav []string
	}{
		{"zsh", "takeover", []string{"SAVEHIST=0", "fc -R", "zshaddhistory", "bindkey '^r'"}, nil},
		{"zsh", "coexist", []string{"bindkey '^r'"}, []string{"SAVEHIST=0", "zshaddhistory"}},
		{"zsh", "capture", nil, []string{"SAVEHIST=0", "bindkey '^r'", "alias h="}},
		{"bash", "takeover", []string{"unset HISTFILE", "history -r", "__yore_bash_gate"}, nil},
		{"bash", "coexist", []string{`bind -x '"\eyore"`}, []string{"unset HISTFILE", "__yore_bash_gate"}},
		{"bash", "capture", nil, []string{"unset HISTFILE", `bind -x '"\eyore"`, "alias h="}},
		// fish has no `!N`/`!!` expansion and no separate in-memory history list to
		// seed or gate, so takeover needs only fish_private_mode — nothing else
		// changes between takeover and coexist besides that and the Up binding.
		{"fish", "takeover", []string{"fish_private_mode", `bind \cr`}, []string{"_yore_bind_up_arrow"}},
		{"fish", "coexist", []string{`bind \cr`, "_yore_bind_up_arrow"}, []string{"fish_private_mode"}},
		{"fish", "capture", nil, []string{"fish_private_mode", `bind \cr`, "function hb"}},
	}
	for _, tc := range tests {
		t.Run(tc.sh+"_"+tc.mode, func(t *testing.T) {
			got, err := shell.Init(tc.sh, shell.Options{Aliases: true, Bin: "yore", Mode: tc.mode})
			require.NoError(t, err)
			for _, w := range tc.mustHave {
				require.Contains(t, got, w)
			}
			for _, w := range tc.mustNotHav {
				require.NotContains(t, got, w)
			}
		})
	}
}

// TestPromptPathWritesNothingToTheTerminal pins a property of the hooks that is
// invisible in normal use and expensive when it breaks: nothing the prompt path
// runs may inherit the terminal on stdout. A yore process whose stdout is a
// terminal queries it for its background colour and reads the reply, so a hook
// that leaks the terminal into a per-command call costs a round trip on every
// prompt and eats input that is already queued.
func TestPromptPathWritesNothingToTheTerminal(t *testing.T) {
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			got, err := shell.Init(sh, shell.Options{Aliases: true, Bin: "yore", Mode: "takeover"})
			require.NoError(t, err)
			for _, line := range strings.Split(got, "\n") {
				if !strings.Contains(line, "yore filter") {
					continue
				}
				require.Contains(t, line, ">/dev/null",
					"the redaction gate runs for every command and must not inherit the terminal")
				return
			}
			require.Fail(t, "takeover mode must install the redaction gate")
		})
	}
}

func TestDefaultBin(t *testing.T) {
	got, err := shell.Init("zsh", shell.Options{Aliases: true}) // Bin empty
	require.NoError(t, err)
	require.Contains(t, got, "command yore gen-id", "empty Bin should default to DefaultBin")
}

func TestDefaultModeIsTakeover(t *testing.T) {
	got, err := shell.Init("zsh", shell.Options{Aliases: true, Bin: "yore"}) // Mode empty
	require.NoError(t, err)
	require.Contains(t, got, "SAVEHIST=0", "empty Mode should default to takeover")
}

func TestUnknownShell(t *testing.T) {
	_, err := shell.Init("ksh", shell.Options{Bin: "yore"})
	require.Error(t, err, "expected an error for an unknown shell")
}

// TestFishFieldsAndIdioms checks the fish hook captures the same fields as
// zsh/bash (command, exit, duration, cwd, session) using fish's own idioms
// ($status, $CMD_DURATION, $PWD) rather than a manually tracked timestamp, and
// keeps the record call off the prompt path (backgrounded + disowned, no db/
// network/crypto).
func TestFishFieldsAndIdioms(t *testing.T) {
	got, err := shell.Init("fish", shell.Options{Aliases: true, Bin: "yore", Mode: "takeover"})
	require.NoError(t, err)
	for _, w := range []string{
		"--on-event fish_preexec",
		"--on-event fish_postexec",
		"set -l exit $status", // $status captured before anything else can disturb it
		"CMD_DURATION",        // duration: fish's own builtin, no hand-rolled timestamp math
		`--cwd "$PWD"`,
		`--session "$YORE_SESSION"`,
		"printf '%s' \"$__yore_cmd\" | command yore record",
		"&>/dev/null &", // backgrounded
		"disown",        // and disowned, so no job-control notice ever prints
	} {
		require.Contains(t, got, w)
	}
	require.NotContains(t, got, "__yore_start", "fish should not need a hand-tracked start timestamp")
}

// TestFishNoSubstitutionInsideQuotes guards the one way a script ported from
// zsh/bash silently does nothing in fish: `"$(cmd)"` translated to `"(cmd)"`.
// Fish expands `(cmd)` only OUTSIDE quotes, so the quoted form is a literal
// string — every `test "(...)" = true` is then false forever and every
// `--query "(commandline -b)"` searches for that text. Nothing errors, so no
// golden file and no substring assertion catches it. Substitute into a variable
// first and quote the variable instead.
func TestFishNoSubstitutionInsideQuotes(t *testing.T) {
	for _, mode := range []string{"coexist", "takeover", "capture"} {
		t.Run(mode, func(t *testing.T) {
			got, err := shell.Init("fish", shell.Options{Aliases: true, Bin: "yore", Mode: mode})
			require.NoError(t, err)
			for i, line := range strings.Split(got, "\n") {
				code, _, _ := strings.Cut(line, "#")
				require.NotContainsf(t, code, `"(`,
					"line %d substitutes inside double quotes, which fish treats as a literal: %s", i+1, line)
			}
		})
	}
}

// TestVendoredPreexec verifies the pinned bash-preexec is present, carries our
// provenance header, defines the install entrypoint, and is embedded.
func TestVendoredPreexec(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("assets", "bash-preexec.sh"))
	require.NoError(t, err, "vendored bash-preexec missing")
	require.NotEmpty(t, b, "vendored bash-preexec is empty")
	src := string(b)
	for _, want := range []string{"__bp_install", "version: 0.5.0", "MIT"} {
		require.Contains(t, src, want)
	}
	bash, err := shell.Init("bash", shell.Options{Aliases: true, Bin: "yore"})
	require.NoError(t, err)
	require.Contains(t, bash, "__bp_install", "emitted bash script does not embed bash-preexec")
}
