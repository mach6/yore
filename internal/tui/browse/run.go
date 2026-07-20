package browse

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run drives the full-screen browser on the controlling terminal. Unlike the
// inline Ctrl-R search, `yore browse` is invoked directly (not through command
// substitution), so os.Stdin/os.Stdout are the real tty; it uses the alternate
// screen. OSC 52 clipboard writes go to os.Stdout, which reaches the terminal
// even over SSH. It returns any program error.
func Run(b Backend, opts Options) error {
	m := NewModel(b, opts)
	m.out = os.Stdout
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
