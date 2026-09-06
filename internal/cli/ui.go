package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"yore/internal/tui/theme"
)

// ui renders the styled output of the interactive commands (setup, recover,
// devices) using the same theme as the TUIs, so the CLI and the full-screen
// views look like one product. Everything here is decoration and goes to
// STDERR; any value a script would want (a token, a device id) is printed bare
// on stdout by the caller, so `$(yore devices token)` yields exactly the token
// and nothing else. Colour degrades on its own: lipgloss detects the output's
// capabilities, so a piped or NO_COLOR run gets plain text without any
// special-casing here.
type ui struct {
	t         *theme.Theme
	w         io.Writer
	sectioned bool // a section heading has already been printed
}

// Layout constants. Setup output is a short transcript, not a full-screen view,
// so it does not reflow with the terminal: a stable width keeps a scrollback of
// several runs aligned, and a fixed label column lines the details up.
const (
	panelWidth = 58
	labelWidth = 34
)

func newUI() *ui {
	return &ui{
		// Bind the renderer to stderr, where this output goes: colour detection
		// must follow the stream actually being written to.
		t: theme.NewWithRenderer(lipgloss.NewRenderer(os.Stderr)),
		w: os.Stderr,
	}
}

// title prints a command banner.
func (u *ui) title(s string) {
	u.printf("\n  %s\n\n", u.t.Title.Render(s))
}

// section prints a group heading in a multi-part report (yore doctor). Headings
// are separated by a blank line, but the first one follows the title directly:
// the title already provides the gap.
func (u *ui) section(name string) {
	if u.sectioned {
		u.blank()
	}
	u.sectioned = true
	u.printf("  %s\n", u.t.Title.Render(name))
}

// step reports a completed stage. detail is optional trailing context, aligned
// into a column so a run reads as a table rather than ragged prose.
func (u *ui) step(msg, detail string) {
	if detail == "" {
		u.printf("  %s %s\n", u.t.ExitOK.Render("✓"), u.t.Norm.Render(msg))
		return
	}
	pad := labelWidth - lipgloss.Width(msg)
	if pad < 1 {
		pad = 1
	}
	u.printf("  %s %s%s%s\n",
		u.t.ExitOK.Render("✓"), u.t.Norm.Render(msg),
		strings.Repeat(" ", pad), u.t.Dim.Render(detail))
}

// working reports a stage that is about to take a noticeable moment.
func (u *ui) working(msg string) {
	u.printf("  %s %s\n", u.t.Accent.Render("…"), u.t.Dim.Render(msg))
}

// warn prints a caution that does not stop the command.
func (u *ui) warn(msg string) {
	u.printf("  %s %s\n", u.t.Match.Render("!"), u.t.Norm.Render(msg))
}

// fail prints an error. Callers still return a nonzero exit code themselves.
func (u *ui) fail(msg string) {
	u.printf("  %s %s\n", u.t.ExitErr.Render("✗"), u.t.Norm.Render(msg))
}

// note prints an indented explanatory line under a step or failure.
func (u *ui) note(msg string) {
	u.printf("    %s\n", u.t.Dim.Render(msg))
}

// panel draws a titled box around a highlighted value plus optional body lines.
// The value is rendered large and accented because these are the things a user
// must read carefully off the screen: a recovery phrase, a verification code.
func (u *ui) panel(title, value string, body ...string) {
	var b strings.Builder
	b.WriteString(u.t.Accent.Bold(true).Render(value))
	for _, line := range body {
		b.WriteString("\n")
		b.WriteString(u.t.Dim.Render(line))
	}
	box := u.t.Border.
		Width(panelWidth).
		Padding(1, 2).
		Render(b.String())

	u.blank()
	u.printf("%s\n", "  "+u.t.Title.Render(title))
	u.printf("%s\n", indent(box, "  "))
	u.blank()
}

// next prints a "what to do now" block: a heading and the commands to run.
func (u *ui) next(heading string, cmds ...string) {
	u.printf("  %s %s\n", u.t.Title.Render("Next"), u.t.Norm.Render(heading))
	for _, c := range cmds {
		u.printf("       %s\n", u.t.Accent.Render(c))
	}
	u.blank()
}

// printf writes one formatted line of decoration. The write error is
// deliberately discarded: this is human-facing output on stderr, and if
// stderr is broken there is nowhere left to report that fact; failing a
// working enrollment over it would be strictly worse.
func (u *ui) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(u.w, format, args...)
}

// blank writes one empty line.
func (u *ui) blank() { _, _ = fmt.Fprintln(u.w) }

// indent prefixes every line of s with pad.
func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
