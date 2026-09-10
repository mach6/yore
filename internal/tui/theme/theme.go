// Package theme holds yore's TUI styling: one Theme of pre-built lipgloss
// styles constructed once and passed into the models. Styles are never
// constructed per frame. Every color is a lipgloss.AdaptiveColor so the UI
// reads correctly on both light and dark terminals.
package theme

import (
	"hash/fnv"

	"github.com/charmbracelet/lipgloss"

	"github.com/mach6/yore/internal/tui/hl"
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
	// RiskHigh is the risk ramp's one ink of its own: critical borrows ExitErr
	// and medium borrows Match, so high sits between them; an orange that is
	// neither the amber of a match nor the red reserved for failure.
	RiskHigh lipgloss.Style
	Status   lipgloss.Style // the bottom status bar
	Border   lipgloss.Style // pane borders
	Help     lipgloss.Style // the key-hints help line

	// Three ranks of heading, and only three. A terminal has very few levers for
	// hierarchy (color, weight, case, indent) so each rank gets its own and no
	// rank shares. Title is the frame (a pane's name, in its border); Section is
	// a heading inside a pane or screen; Dim is the chrome below both (column
	// headers, field labels).
	Title   lipgloss.Style // accent + bold: pane names, the outermost rank
	Section lipgloss.Style // bold, normal color: a heading within a pane

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
	// dataStyles are the pre-built chart-ink styles selected by Data.
	dataStyles []lipgloss.Style
}

// Palette. AdaptiveColor picks Light on light terminals, Dark on dark ones.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#0066cc", Dark: "#5fafff"} // blue
	cNorm   = lipgloss.AdaptiveColor{Light: "#1c1c1c", Dark: "#dadada"}
	cDim    = lipgloss.AdaptiveColor{Light: "#8a8a8a", Dark: "#6c6c6c"}
	cMatch  = lipgloss.AdaptiveColor{Light: "#b35a00", Dark: "#ffb454"} // amber, not red/green
	cOK     = lipgloss.AdaptiveColor{Light: "#207520", Dark: "#5fd75f"} // green
	cErr    = lipgloss.AdaptiveColor{Light: "#c02020", Dark: "#ff6b6b"} // red
	cRisk   = lipgloss.AdaptiveColor{Light: "#c2410c", Dark: "#ff9e4a"} // orange, between cMatch and cErr
	cBorder = lipgloss.AdaptiveColor{Light: "#c6c6c6", Dark: "#3a3a3a"}
	cStatBg = lipgloss.AdaptiveColor{Light: "#eeeeee", Dark: "#1c1c1c"}

	// Syntax hues; muted so they read as texture, not decoration, and never
	// out-shout the amber match highlight. Red/green avoided (exit-status).
	cSynCmd = lipgloss.AdaptiveColor{Light: "#0055aa", Dark: "#87afd7"} // command: soft blue
	cSynFlg = lipgloss.AdaptiveColor{Light: "#8a6d00", Dark: "#c5b070"} // flags: muted gold
	cSynStr = lipgloss.AdaptiveColor{Light: "#0a7a5a", Dark: "#8fcfaf"} // strings: soft teal-green
	cSynPth = lipgloss.AdaptiveColor{Light: "#7a3fb0", Dark: "#b79fd7"} // paths: soft purple
	cSynOp  = lipgloss.AdaptiveColor{Light: "#a01e78", Dark: "#d79fc7"} // operators: soft magenta
	cSynVar = lipgloss.AdaptiveColor{Light: "#0a6ea0", Dark: "#7fc7df"} // variables: soft cyan
)

// dataRamp is the chart ink: ONE hue at four intensity steps, ascending. It is
// deliberately not the UI accent. Every bar, gauge and heat cell used to render
// in accent blue, which meant that on the stats screen everything with ink was
// the same color as everything selectable: the accent distinguished nothing,
// and nothing could be emphasized within the data. Violet is clear of the blue
// accent, of red/green (exit status) and of amber (match highlight). Intensity
// rides on lightness, not only on glyph height or density, so a chart survives
// a terminal or a font that renders shade glyphs poorly.
var dataRamp = [4]lipgloss.AdaptiveColor{
	// On light terminals intensity darkens; on dark ones it brightens.
	{Light: "#b7a9dc", Dark: "#4e4176"},
	{Light: "#9080c6", Dark: "#6d5aa4"},
	{Light: "#6b52ac", Dark: "#9583ce"},
	{Light: "#472f86", Dark: "#c3b4f2"},
}

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

// New builds the Theme with the default renderer. Call it once at startup.
func New() *Theme {
	return NewWithRenderer(lipgloss.DefaultRenderer())
}

// NewWithRenderer builds the Theme with styles bound to renderer r, so color
// and background detection follow r's output rather than the default
// renderer's os.Stdout. The inline search TUI passes a renderer bound to
// /dev/tty for this reason; New uses the default renderer.
func NewWithRenderer(r *lipgloss.Renderer) *Theme {
	base := r.NewStyle()
	t := &Theme{
		Accent: base.Foreground(cAccent),
		Prompt: base.Foreground(cAccent).Bold(true),
		Input:  base.Foreground(cNorm),
		Norm:   base.Foreground(cNorm),
		// Reverse video (terminal-native) so the selected row is unmistakable
		// even when adaptive light/dark detection is wrong or the terminal does
		// not render truecolor backgrounds. Paired with a ❯ marker in the rows.
		Sel:      base.Reverse(true).Bold(true),
		Dim:      base.Foreground(cDim),
		Match:    base.Foreground(cMatch).Bold(true),
		ExitOK:   base.Foreground(cOK),
		ExitErr:  base.Foreground(cErr).Bold(true),
		RiskHigh: base.Foreground(cRisk).Bold(true),
		Status:   base.Foreground(cDim).Background(cStatBg),
		Border:   base.Foreground(cBorder).BorderForeground(cBorder).Border(lipgloss.RoundedBorder()),
		Title:    base.Foreground(cAccent).Bold(true),
		Section:  base.Foreground(cNorm).Bold(true),
		Help:     base.Foreground(cDim),

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
	t.dataStyles = make([]lipgloss.Style, len(dataRamp))
	for i, c := range dataRamp {
		t.dataStyles[i] = base.Foreground(c)
	}
	return t
}

// DataLevels is how many intensity steps Data accepts (0 .. DataLevels-1).
const DataLevels = len(dataRamp)

// Data returns the chart-ink style for an intensity level, clamped into the
// ramp. Level 0 is the faintest step that still counts as activity; callers
// render true zero with Dim, so "none" and "a little" never look alike.
func (t *Theme) Data(level int) lipgloss.Style {
	if level < 0 {
		level = 0
	}
	if level >= len(t.dataStyles) {
		level = len(t.dataStyles) - 1
	}
	return t.dataStyles[level]
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
