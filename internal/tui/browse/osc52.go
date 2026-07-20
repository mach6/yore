package browse

import (
	"encoding/base64"
	"os/exec"
	"strings"
)

// osc52 returns the OSC 52 escape sequence that sets the terminal clipboard to
// s. It is the terminal-native "copy" primitive: emitting these bytes to the
// controlling tty copies without any external dependency and works across SSH.
//
// The form is ESC ] 52 ; c ; <base64(payload)> BEL, targeting the "c"
// (clipboard) selection.
func osc52(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	return "\x1b]52;c;" + enc + "\x07"
}

// osc52Seq builds the clipboard-set escape sequence for s. Without tmux it is a
// bare OSC 52. Inside tmux the sequence must be wrapped in tmux's passthrough
// form — ESC P tmux ; ESC <inner, every ESC doubled> ESC \ — or tmux swallows it
// instead of forwarding it to the outer terminal. It is a pure function so the
// framing (plain vs tmux-wrapped) is unit-testable without a terminal.
func osc52Seq(s string, tmux bool) string {
	seq := osc52(s)
	if !tmux {
		return seq
	}
	return "\x1bPtmux;\x1b" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// clipboardTools are the local system clipboard commands tried in order; the
// first one found on PATH receives the copy. OSC 52 (above) covers SSH and
// terminals that honor it; this covers terminals that silently ignore OSC 52 but
// have a local clipboard tool. CGO-free (os/exec only).
var clipboardTools = []struct {
	name string
	args []string
}{
	{"wl-copy", nil},
	{"xclip", []string{"-selection", "clipboard"}},
	{"xsel", []string{"--clipboard", "--input"}},
	{"pbcopy", nil},
	{"clip.exe", nil},
}

// localClipboardCopy is the local-tool copy indirected through a var so tests
// can stub it: invoking a real clipboard tool from a test would overwrite the
// developer's system clipboard. Production always uses clipboardCopy.
var localClipboardCopy = clipboardCopy

// clipboardCopy best-effort pipes s to the first available local clipboard tool.
// It is fire-and-forget: any error (no tool on PATH, a failing tool) is ignored.
func clipboardCopy(s string) {
	for _, t := range clipboardTools {
		path, err := exec.LookPath(t.name)
		if err != nil {
			continue
		}
		c := exec.Command(path, t.args...)
		c.Stdin = strings.NewReader(s)
		_ = c.Run()
		return
	}
}
