// Package hl is a lightweight, best-effort syntax classifier for shell command
// lines. It is not a shell parser: it exists only to color command rows in the
// TUIs, so it favors being fast and visually helpful over being exhaustively
// correct. Classify returns one Kind per byte of the input, which the caller
// layers UNDER its own match highlighting (matches always win).
package hl

// Kind is the syntactic role of a byte in a command line.
type Kind uint8

const (
	Normal   Kind = iota // plain argument text
	Command              // the command word (first token of a pipeline stage)
	Flag                 // -x / --long options
	String               // quoted text (contents and quotes)
	Path                 // a token containing a path separator
	Operator             // | & ; < > ( ) and && || etc.
	Variable             // $name / ${...}
)

// Classify returns a Kind for every byte of s.
func Classify(s string) []Kind {
	out := make([]Kind, len(s))
	n := len(s)
	i := 0
	expectCommand := true // the next word starts a new pipeline stage

	for i < n {
		c := s[i]

		// Whitespace: skip.
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}

		// Operators separate pipeline stages; the next word is a command again.
		if isOperator(c) {
			for i < n && isOperator(s[i]) {
				out[i] = Operator
				i++
			}
			expectCommand = true
			continue
		}

		// A token runs until whitespace or an operator (quotes may contain them).
		start := i
		hasSlash := false
		var quote byte
		for i < n {
			ch := s[i]
			if quote != 0 {
				out[i] = String
				if ch == quote {
					quote = 0
				}
				i++
				continue
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				out[i] = String
				i++
				continue
			}
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || isOperator(ch) {
				break
			}
			if ch == '/' {
				hasSlash = true
			}
			i++
		}

		tok := s[start:i]
		kind := Normal
		switch {
		case tok != "" && tok[0] == '-':
			kind = Flag
		case isAssignment(tok):
			kind = Normal // VAR=value before the command; leave the value plain
		case expectCommand:
			kind = Command
			expectCommand = false
		case hasSlash:
			kind = Path
		}
		// Assignments don't consume the command slot; a real word does.
		if kind != Normal || !isAssignment(tok) {
			if !isAssignment(tok) {
				expectCommand = false
			}
		}

		// Paint the token's non-string bytes with its kind.
		for j := start; j < i; j++ {
			if out[j] != String {
				out[j] = kind
			}
		}
		// Variables override everything (even inside double quotes).
		markVariables(s, start, i, out)
	}
	return out
}

func isOperator(c byte) bool {
	switch c {
	case '|', '&', ';', '<', '>', '(', ')':
		return true
	}
	return false
}

// isAssignment reports whether tok looks like NAME=value (a leading env
// assignment), so it isn't mistaken for the command word.
func isAssignment(tok string) bool {
	eq := -1
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c == '=' {
			eq = i
			break
		}
		isName := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')
		if !isName {
			return false
		}
	}
	return eq > 0
}

// markVariables paints $name and ${...} spans within [start,end) as Variable,
// including inside double-quoted strings (where shells still expand them).
func markVariables(s string, start, end int, out []Kind) {
	for i := start; i < end; i++ {
		if s[i] != '$' {
			continue
		}
		// Skip single-quoted regions (no expansion there): the byte is String
		// and its opening quote was '\''; approximate by not marking $ that the
		// classifier already left as String with no following name/brace.
		j := i + 1
		if j < end && s[j] == '{' {
			out[i] = Variable
			for j < end {
				out[j] = Variable
				if s[j] == '}' {
					break
				}
				j++
			}
			i = j
			continue
		}
		k := j
		for k < end && (s[k] == '_' || (s[k] >= 'A' && s[k] <= 'Z') || (s[k] >= 'a' && s[k] <= 'z') || (s[k] >= '0' && s[k] <= '9')) {
			k++
		}
		if k > j { // $ followed by a name
			out[i] = Variable
			for p := j; p < k; p++ {
				out[p] = Variable
			}
			i = k - 1
		}
	}
}
