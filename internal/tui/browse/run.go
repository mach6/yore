package browse

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run drives the full-screen browser on the controlling terminal. Unlike the
// inline Ctrl-R search, `yore browse` is invoked directly (not through command
// substitution), so os.Stdin/os.Stdout are the real tty; it uses the alternate
// screen. OSC 52 clipboard writes (the `y` key) go to os.Stdout, which reaches
// the terminal even over SSH.
//
// It returns the command the user accepted with Enter ("" if they quit without
// accepting) and any program error. The caller delivers that command back to the
// shell (recall-to-prompt), mirroring the Ctrl-R search.
//
// Mouse reporting is on in cell-motion mode: clicking focuses a pane, the wheel
// scrolls the pane under the pointer, and dragging a border resizes the panes
// either side of it (motion events only arrive while a button is held). The
// trade-off is that the terminal's own text selection needs the usual Shift
// modifier while the browser is open.
func Run(b Backend, opts Options) (string, error) {
	m := NewModel(b, opts)
	m.out = os.Stdout
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	res, ok := final.(Model)
	if !ok {
		return "", nil
	}
	return res.accepted, nil
}
