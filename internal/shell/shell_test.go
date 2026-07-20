package shell_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
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
	var cs []renderCase
	for _, sh := range []string{"zsh", "bash"} {
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
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			got, err := shell.Init(sh, shell.Options{Aliases: true, Bin: bin})
			require.NoError(t, err)
			for _, w := range []string{
				"command " + bin + " gen-id",
				"command " + bin + " record",
				"command " + bin + ` search --query "`,
				"alias h='" + bin + " browse'",
				"command " + bin + ` search --headless "$*"`,
			} {
				require.Contains(t, got, w, "missing invocation")
			}
			require.NotContains(t, got, "command yore ", "leaked bare default binary")
			require.NotContains(t, got, "alias h='yore ", "leaked bare default binary")
		})
	}
}

func TestAliasToggle(t *testing.T) {
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			on, err := shell.Init(sh, shell.Options{Aliases: true, Bin: "yore"})
			require.NoError(t, err)
			off, err := shell.Init(sh, shell.Options{Aliases: false, Bin: "yore"})
			require.NoError(t, err)
			require.Contains(t, on, "alias h=", "aliases:true should emit the alias block")
			require.NotContains(t, off, "alias h=", "aliases:false must not emit the alias block")
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
	_, err := shell.Init("fish", shell.Options{Bin: "yore"})
	require.Error(t, err, "expected an error for an unknown shell")
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
