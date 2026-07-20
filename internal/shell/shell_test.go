package shell_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"yore/internal/shell"
)

var update = flag.Bool("update", false, "regenerate golden files under testdata/")

// cases covers every (shell, aliases) combination we render goldens for.
var cases = []struct {
	name   string
	sh     string
	opts   shell.Options
	golden string
	syntax string // shell binary used for `-n` syntax validation
	ext    string
}{
	{"zsh_aliases", "zsh", shell.Options{Aliases: true, Bin: "yore"}, "init_zsh_aliases.golden", "zsh", "zsh"},
	{"zsh_noaliases", "zsh", shell.Options{Aliases: false, Bin: "yore"}, "init_zsh_noaliases.golden", "zsh", "zsh"},
	{"bash_aliases", "bash", shell.Options{Aliases: true, Bin: "yore"}, "init_bash_aliases.golden", "bash", "bash"},
	{"bash_noaliases", "bash", shell.Options{Aliases: false, Bin: "yore"}, "init_bash_noaliases.golden", "bash", "bash"},
}

func TestInitGolden(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := shell.Init(tc.sh, tc.opts)
			if err != nil {
				t.Fatalf("Init(%q): %v", tc.sh, err)
			}
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden (run `go test -run TestInitGolden -update`): %v", err)
			}
			if got != string(want) {
				t.Errorf("Init(%q, %+v) does not match %s\n--- got ---\n%s\n--- want ---\n%s",
					tc.sh, tc.opts, tc.golden, got, want)
			}
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
			bin, err := exec.LookPath(tc.syntax)
			if err != nil {
				t.Skipf("%s not installed: %v", tc.syntax, err)
			}
			script, err := shell.Init(tc.sh, tc.opts)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			f := filepath.Join(t.TempDir(), "init."+tc.ext)
			if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "-n", f).CombinedOutput()
			if err != nil {
				t.Fatalf("%s -n rejected the emitted script: %v\n%s", tc.syntax, err, out)
			}
		})
	}
}

// TestAbsoluteBin ensures an absolute Bin path is substituted everywhere it is
// invoked: gen-id, record, the Ctrl-R widget, the browse alias, and both arms
// of the hs helper.
func TestAbsoluteBin(t *testing.T) {
	const bin = "/usr/local/bin/yore"
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			got, err := shell.Init(sh, shell.Options{Aliases: true, Bin: bin})
			if err != nil {
				t.Fatal(err)
			}
			wants := []string{
				"command " + bin + " gen-id",
				"command " + bin + " record",
				"command " + bin + ` search --query "`,
				"alias h='" + bin + " browse'",
				"command " + bin + ` search --query "$*"`,
				"command " + bin + ` search --headless "$*"`,
			}
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Errorf("emitted %s script missing %q", sh, w)
				}
			}
			// The bare default must never leak in when an absolute path is set.
			if strings.Contains(got, "command yore ") || strings.Contains(got, "alias h='yore ") {
				t.Errorf("emitted %s script leaked the bare default binary name", sh)
			}
		})
	}
}

func TestAliasToggle(t *testing.T) {
	for _, sh := range []string{"zsh", "bash"} {
		on, err := shell.Init(sh, shell.Options{Aliases: true, Bin: "yore"})
		if err != nil {
			t.Fatal(err)
		}
		off, err := shell.Init(sh, shell.Options{Aliases: false, Bin: "yore"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(on, "alias h=") || !strings.Contains(on, "hs()") {
			t.Errorf("%s: aliases:true should emit the alias block", sh)
		}
		if strings.Contains(off, "alias h=") || strings.Contains(off, "hs()") {
			t.Errorf("%s: aliases:false must not emit the alias block", sh)
		}
	}
}

func TestDefaultBin(t *testing.T) {
	got, err := shell.Init("zsh", shell.Options{Aliases: true}) // Bin empty
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "command yore gen-id") {
		t.Errorf("empty Bin should default to %q", shell.DefaultBin)
	}
}

func TestUnknownShell(t *testing.T) {
	if _, err := shell.Init("fish", shell.Options{Bin: "yore"}); err == nil {
		t.Fatal("expected an error for an unknown shell")
	}
}

// TestVendoredPreexec verifies the pinned bash-preexec is present, non-empty,
// carries our provenance header, and defines the install entrypoint. It is
// inlined verbatim into the bash script.
func TestVendoredPreexec(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("assets", "bash-preexec.sh"))
	if err != nil {
		t.Fatalf("vendored bash-preexec missing: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("vendored bash-preexec is empty")
	}
	src := string(b)
	for _, want := range []string{"__bp_install", "version: 0.5.0", "MIT"} {
		if !strings.Contains(src, want) {
			t.Errorf("vendored bash-preexec missing %q", want)
		}
	}
	// It must actually be embedded in the emitted bash script.
	bash, err := shell.Init("bash", shell.Options{Aliases: true, Bin: "yore"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bash, "__bp_install") {
		t.Error("emitted bash script does not embed the vendored bash-preexec")
	}
}
