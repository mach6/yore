package importer

import (
	"io"
	"strconv"
	"strings"

	"yore/internal/rec"
)

// metaChar is zsh's Meta escape byte. In the history file a byte X that zsh
// needs to escape is written as the pair (metaChar, X^metaShift).
const (
	metaChar  = 0x83
	metaShift = 0x20
)

// unmetafy reverses zsh's metafication: every (0x83, X) byte pair becomes the
// single byte X^0x20. A trailing lone 0x83 (no following byte) is passed
// through unchanged. This must run before any structural parsing so that
// non-ASCII (metafied) command bytes are restored first.
func unmetafy(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == metaChar && i+1 < len(b) {
			out = append(out, b[i+1]^metaShift)
			i++
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// Zsh parses a zsh history stream (extended or plain) into records. hostID is
// used only to compute deterministic ids; it is not stored on the record.
func Zsh(r io.Reader, hostID string) ([]rec.Record, error) {
	raw, err := readAll(r)
	if err != nil {
		return nil, err
	}

	// Unmetafy each physical line, then reconstruct logical entries by joining
	// backslash-continued physical lines. Newline bytes are never produced by
	// unmetafication, so line splitting before unmetafying is safe.
	phys := physicalLines(raw)
	lines := make([]string, len(phys))
	for i, p := range phys {
		lines[i] = string(unmetafy(p))
	}

	var out []rec.Record
	for i := 0; i < len(lines); {
		logical := lines[i]
		i++
		// A physical line ending in a single backslash escapes the newline:
		// strip the backslash and join with a real newline. Repeat while the
		// growing entry still ends in a continuation backslash.
		for endsWithBackslash(logical) && i < len(lines) {
			logical = logical[:len(logical)-1] + "\n" + lines[i]
			i++
		}

		startMs, dur, cmd := parseZshLine(logical)
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		out = append(out, rec.Record{
			ID:      rec.ImportID(hostID, startMs, cmd),
			Session: "import",
			Cmd:     cmd,
			StartMs: startMs,
			DurMs:   dur,
		})
	}
	return out, nil
}

func endsWithBackslash(s string) bool {
	return s != "" && s[len(s)-1] == '\\'
}

// parseZshLine parses one logical history line. Extended lines have the form
// ": <start>:<elapsed>;<command>" where start/elapsed are epoch/whole seconds;
// anything else is treated as a plain command with no timestamp. dur is non-nil
// only for extended lines with elapsed >= 0 (0 is a valid duration).
func parseZshLine(line string) (startMs int64, dur *int64, cmd string) {
	if start, elapsed, command, ok := parseExtended(line); ok {
		var d *int64
		if elapsed >= 0 {
			d = rec.Int64Ptr(elapsed * 1000)
		}
		return start * 1000, d, command
	}
	return 0, nil, line
}

// parseExtended recognises the strict ": <digits>:<[-]digits>;" prefix so that
// ordinary commands beginning with ':' (e.g. ": > file") are not misparsed.
func parseExtended(line string) (start, elapsed int64, cmd string, ok bool) {
	if line == "" || line[0] != ':' {
		return 0, 0, "", false
	}
	i := 1
	// zsh always writes a single space after the colon.
	if i >= len(line) || line[i] != ' ' {
		return 0, 0, "", false
	}
	for i < len(line) && line[i] == ' ' {
		i++
	}
	// start: one or more digits, terminated by ':'.
	j := i
	for j < len(line) && isDigit(line[j]) {
		j++
	}
	if j == i || j >= len(line) || line[j] != ':' {
		return 0, 0, "", false
	}
	startStr := line[i:j]
	j++ // consume ':'
	// elapsed: optional '-' then one or more digits, terminated by ';'.
	e := j
	if e < len(line) && line[e] == '-' {
		e++
	}
	d := e
	for d < len(line) && isDigit(line[d]) {
		d++
	}
	if d == e || d >= len(line) || line[d] != ';' {
		return 0, 0, "", false
	}
	elapsedStr := line[j:d]

	start, err1 := strconv.ParseInt(startStr, 10, 64)
	elapsed, err2 := strconv.ParseInt(elapsedStr, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	return start, elapsed, line[d+1:], true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
