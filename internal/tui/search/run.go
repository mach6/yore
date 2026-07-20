package search

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run drives the interactive search. It opens /dev/tty for both input and
// output (via tea.WithInput/WithOutput), so the caller's stdout stays clean
// and the selected command can be captured through command substitution. It
// does not use the alternate screen: the panel renders inline under the shell
// prompt and is cleared on exit.
//
// It returns the accepted command and true, or ("", false) on cancel. If
// /dev/tty cannot be opened (the process is not attached to a terminal) it
// returns an error immediately so the caller can fall back to headless search.
func Run(q Querier, opts Options) (string, bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false, fmt.Errorf("search: open /dev/tty: %w", err)
	}
	defer tty.Close()

	p := tea.NewProgram(
		NewModel(q, opts),
		tea.WithInput(tty),
		tea.WithOutput(tty),
	)

	final, err := p.Run()
	if err != nil {
		return "", false, err
	}

	res, ok := final.(Model)
	if !ok || !res.accept || res.cancel {
		return "", false, nil
	}
	return res.accepted, true, nil
}
