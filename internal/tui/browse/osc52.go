package browse

import "encoding/base64"

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
