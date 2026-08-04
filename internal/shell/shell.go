// Package shell renders yore's shell-integration scripts. These scripts are
// the product's integration contract: they are sourced into every one of the
// user's interactive shells (typically via `eval "$(yore init zsh)"`), so they
// must be fast, side-effect-free during normal operation, and must never break
// the user's shell.
//
// The scripts live under assets/ and are embedded at build time. Init renders
// them through text/template, substituting the invoked binary name/path and,
// optionally, the convenience-alias block.
package shell

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed assets/*
var assets embed.FS

// DefaultBin is the command used when Options.Bin is empty.
const DefaultBin = "yore"

// Options configures the emitted shell-integration script.
type Options struct {
	// Aliases controls whether the h/hs convenience aliases are emitted.
	// The caller passes this explicitly; the CLI's default is true.
	Aliases bool
	// Bin is the command name or path used to invoke yore from the hooks
	// (e.g. "yore" or "/usr/local/bin/yore"). Empty means DefaultBin.
	Bin string
	// Mode is the integration depth: "takeover" (default), "coexist", or
	// "capture". Empty means "takeover".
	Mode string
}

// tmplData is the payload handed to the embedded templates.
type tmplData struct {
	Bin      string // resolved binary name/path
	Aliases  bool   // emit the alias block (and only when not capture-only)
	Preexec  string // vendored bash-preexec source (bash only)
	Mode     string // "takeover" | "coexist" | "capture"
	Takeover bool   // Mode == takeover: yore owns history
	Bindings bool   // Mode != capture: rebind Ctrl-R (+ up-arrow), emit aliases
}

// Init returns the full shell-integration script for the given shell, which
// must be "zsh", "bash", or "fish". It returns an error for any other shell.
func Init(sh string, o Options) (string, error) {
	bin := o.Bin
	if bin == "" {
		bin = DefaultBin
	}
	mode := o.Mode
	switch mode {
	case "coexist", "capture":
	default:
		mode = "takeover"
	}
	data := tmplData{
		Bin:      bin,
		Mode:     mode,
		Takeover: mode == "takeover",
		Bindings: mode != "capture",
		Aliases:  o.Aliases && mode != "capture",
	}

	var asset string
	switch sh {
	case "zsh":
		asset = "assets/init.zsh"
	case "bash":
		asset = "assets/init.bash"
		pe, err := assets.ReadFile("assets/bash-preexec.sh")
		if err != nil {
			return "", fmt.Errorf("shell: reading vendored bash-preexec: %w", err)
		}
		data.Preexec = string(pe)
	case "fish":
		asset = "assets/init.fish"
	default:
		return "", fmt.Errorf("shell: unknown shell %q (want \"zsh\", \"bash\", or \"fish\")", sh)
	}

	raw, err := assets.ReadFile(asset)
	if err != nil {
		return "", fmt.Errorf("shell: reading %s: %w", asset, err)
	}
	// Use a delimiter-safe template: shell code never contains "{{"/"}}", so
	// the default delimiters are unambiguous. Missing keys are an error so a
	// malformed template fails loudly at test time rather than silently.
	t, err := template.New(sh).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("shell: parsing %s: %w", asset, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("shell: rendering %s: %w", asset, err)
	}
	return b.String(), nil
}
