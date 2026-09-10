package importer

import (
	"io"
	"strconv"
	"strings"

	"github.com/mach6/yore/internal/rec"
)

// Bash parses a bash history stream into records. When HISTTIMEFORMAT was set,
// bash writes a "#<epoch>" line immediately before each command; such a line
// (a '#' followed only by digits) is a timestamp for the NEXT command. Any
// other '#'-prefixed line is a literal command (users do type comments). If
// several timestamp lines appear in a row, the last one wins. Commands with no
// preceding timestamp get StartMs == 0. Bash records no duration.
//
// Each physical line is treated as one command; bash's multiline history
// storage (cmdhist/lithist) is not reconstructed (documented limitation).
func Bash(r io.Reader, hostID string) ([]rec.Record, error) {
	raw, err := readAll(r)
	if err != nil {
		return nil, err
	}

	var out []rec.Record
	var pending int64 // StartMs for the next command, 0 if none
	for _, p := range physicalLines(raw) {
		line := string(p)
		if ms, ok := bashTimestamp(line); ok {
			pending = ms // last timestamp before a command wins
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		startMs := pending
		pending = 0
		out = append(out, rec.Record{
			ID:      rec.ImportID(hostID, startMs, line),
			Session: "import",
			Cmd:     line,
			StartMs: startMs,
		})
	}
	return out, nil
}

// bashTimestamp reports whether line is a "#<epoch>" timestamp comment and, if
// so, returns the epoch in milliseconds. The line must be '#' followed by one
// or more digits and nothing else.
func bashTimestamp(line string) (int64, bool) {
	if len(line) < 2 || line[0] != '#' {
		return 0, false
	}
	digits := line[1:]
	for i := 0; i < len(digits); i++ {
		if !isDigit(digits[i]) {
			return 0, false
		}
	}
	sec, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false // overflow: treat as a literal command
	}
	return sec * 1000, true
}
