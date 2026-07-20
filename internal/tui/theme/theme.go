// Package theme holds yore's TUI styling: one Theme of pre-built lipgloss
// styles constructed once and passed into the models. Styles are never
// constructed per frame. Every color is a lipgloss.AdaptiveColor so the UI
// reads correctly on both light and dark terminals.
package theme

import (
	"hash/fnv"

	"github.com/charmbracelet/lipgloss"

	"yore/internal/tui/hl"
)

// Theme is the immutable set of styles shared across the TUI. Build it once
// with New and pass it by pointer into every model.
type Theme struct {
	Accent  lipgloss.Style // the single accent color, for emphasis
	Prompt  lipgloss.Style // the search prompt sigil
	Input   lipgloss.Style // the text the user is typing
	Norm    lipgloss.Style // normal, unselected row text
	Sel     lipgloss.Style // the selected row
	Dim     lipgloss.Style // metadata: times, durations, cwd
	Match   lipgloss.Style // highlighted matched substrings
	ExitOK  lipgloss.Style // exit status 0 (green)
	ExitErr lipgloss.Style // non-zero exit status (red)
	Status  lipgloss.Style // the bottom status bar
	Border  lipgloss.Style // pane borders
	Title   lipgloss.Style // pane / section titles
	Help    lipgloss.Style // the key-hints help line

	// Syntax styles for command rows (layered under Match). Restrained so the
	// match highlight and exit/host colors still read clearly.
	SynCommand  lipgloss.Style
	SynFlag     lipgloss.Style
	SynString   lipgloss.Style
	SynPath     lipgloss.Style
	SynOperator lipgloss.Style
	SynVariable lipgloss.Style

	// hostStyles are the pre-built per-host hue styles selected by Host.
	hostStyles []lipgloss.Style
}

// Palette. AdaptiveColor picks Light on light terminals, Dark on dark ones.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#0066cc", Dark: "#5fafff"} // blue
	cNorm   = lipgloss.AdaptiveColor{Light: "#1c1c1c", Dark: "#dadada"}
	cDim    = lipgloss.AdaptiveColor{Light: "#8a8a8a", Dark: "#6c6c6c"}
	cSelBg  = lipgloss.AdaptiveColor{Light: "#d7e6ff", Dark: "#25384f"}
	cSelFg  = lipgloss.AdaptiveColor{Light: "#003a75", Dark: "#eaf2ff"}
	cMatch  = lipgloss.AdaptiveColor{Light: "#b35a00", Dark: "#ffb454"} // amber, not red/green
	cOK     = lipgloss.AdaptiveColor{Light: "#207520", Dark: "#5fd75f"} // green
	cErr    = lipgloss.AdaptiveColor{Light: "#c02020", Dark: "#ff6b6b"} // red
	cBorder = lipgloss.AdaptiveColor{Light: "#c6c6c6", Dark: "#3a3a3a"}
	cStatBg = lipgloss.AdaptiveColor{Light: "#eeeeee", Dark: "#1c1c1c"}

	// Syntax hues — muted so they read as texture, not decoration, and never
	// out-shout the amber match highlight. Red/green avoided (exit-status).
	cSynCmd = lipgloss.AdaptiveColor{Light: "#0055aa", Dark: "#87afd7"} // command: soft blue
	cSynFlg = lipgloss.AdaptiveColor{Light: "#8a6d00", Dark: "#c5b070"} // flags: muted gold
	cSynStr = lipgloss.AdaptiveColor{Light: "#0a7a5a", Dark: "#8fcfaf"} // strings: soft teal-green
	cSynPth = lipgloss.AdaptiveColor{Light: "#7a3fb0", Dark: "#b79fd7"} // paths: soft purple
	cSynOp  = lipgloss.AdaptiveColor{Light: "#a01e78", Dark: "#d79fc7"} // operators: soft magenta
	cSynVar = lipgloss.AdaptiveColor{Light: "#0a6ea0", Dark: "#7fc7df"} // variables: soft cyan
)

// hostPalette is 8 distinguishable hues (on both light and dark) for
// per-hostname coloring. Pure red and green are deliberately excluded so
// host colors never collide with the exit-status semantics.
var hostPalette = [8]lipgloss.AdaptiveColor{
	{Light: "#0066cc", Dark: "#5fafff"}, // blue
	{Light: "#7a3fb0", Dark: "#c58fff"}, // purple
	{Light: "#0a7a7a", Dark: "#5fd7d7"}, // teal
	{Light: "#b35a00", Dark: "#ffb454"}, // amber
	{Light: "#a01e78", Dark: "#ff87d7"}, // magenta
	{Light: "#3060c0", Dark: "#87afff"}, // indigo
	{Light: "#8a6d00", Dark: "#d7c15f"}, // olive/gold
	{Light: "#0a6ea0", Dark: "#5fc7ff"}, // cyan
}

// New builds the Theme. Call it once at startup.
func New() *Theme {
	base := lipgloss.NewStyle()
	t := &Theme{
		Accent:  base.Foreground(cAccent),
		Prompt:  base.Foreground(cAccent).Bold(true),
		Input:   base.Foreground(cNorm),
		Norm:    base.Foreground(cNorm),
		Sel:     base.Foreground(cSelFg).Background(cSelBg).Bold(true),
		Dim:     base.Foreground(cDim),
		Match:   base.Foreground(cMatch).Bold(true),
		ExitOK:  base.Foreground(cOK),
		ExitErr: base.Foreground(cErr).Bold(true),
		Status:  base.Foreground(cDim).Background(cStatBg),
		Border:  base.Foreground(cBorder).BorderForeground(cBorder).Border(lipgloss.RoundedBorder()),
		Title:   base.Foreground(cAccent).Bold(true),
		Help:    base.Foreground(cDim),

		SynCommand:  base.Foreground(cSynCmd),
		SynFlag:     base.Foreground(cSynFlg),
		SynString:   base.Foreground(cSynStr),
		SynPath:     base.Foreground(cSynPth),
		SynOperator: base.Foreground(cSynOp),
		SynVariable: base.Foreground(cSynVar),
	}
	t.hostStyles = make([]lipgloss.Style, len(hostPalette))
	for i, c := range hostPalette {
		t.hostStyles[i] = base.Foreground(c)
	}
	return t
}

// Syntax returns the style for a syntax kind, or Norm for plain text.
func (t *Theme) Syntax(k hl.Kind) lipgloss.Style {
	switch k {
	case hl.Command:
		return t.SynCommand
	case hl.Flag:
		return t.SynFlag
	case hl.String:
		return t.SynString
	case hl.Path:
		return t.SynPath
	case hl.Operator:
		return t.SynOperator
	case hl.Variable:
		return t.SynVariable
	default:
		return t.Norm
	}
}

// Host returns a deterministic style for a hostname. The name is hashed with
// FNV-1a and reduced modulo the 8-hue palette, so the same name always maps
// to the same color across processes and sessions.
func (t *Theme) Host(name string) lipgloss.Style {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return t.hostStyles[h.Sum32()%uint32(len(t.hostStyles))]
}
