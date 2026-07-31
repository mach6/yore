package risk

import "strings"

// A command is judged by what it would *run*, not by what it contains. The
// difference is the whole game: `grep -rn "rm -rf" docs/` and `sh -c 'rm -rf /'`
// carry the same eight characters, and only one of them deletes anything.
//
// So before any rule runs, the line is lexed into words and cut into command
// segments wherever a shell would start a new command — a pipe, a `;`, a `&&`,
// a command substitution, a `find -exec`. Each segment names the command word
// it would actually execute, with `sudo`-style wrappers stripped, quoted text
// kept as inert data, and its own flags kept to itself. Rules then ask "is
// `kill` the command here?" instead of "does this string contain kill".
//
// This is not a shell parser and does not try to be. It resolves exactly the
// cases that separate a command from a mention of one; anything subtler — an
// alias, a Makefile target, a variable holding a command name — stays invisible,
// deliberately, because a classifier that is wrong in an inspectable direction
// beats one that is wrong subtly.

// tok is one lexed word or operator. quoted marks text that came from inside
// quotes: a shell hands it over as data, so no rule may read it as a command.
type tok struct {
	val    string
	quoted bool
	op     bool
}

// subst is the marker the lexer emits wherever a command substitution, process
// substitution, or subshell opens or closes — every one of them a place a new
// command begins, including inside double quotes, where `"$(curl …)"` still runs.
const subst = "\x00("

// lex splits a command line into words and operators. Escapes, both quote
// styles, and nested substitutions are honored; everything else is a word.
func lex(s string) []tok {
	var (
		out      []tok
		cur      strings.Builder
		quoted   bool
		has      bool
		inDouble bool
		stack    []bool // the double-quote state each open substitution suspended
	)
	flush := func() {
		if has {
			out = append(out, tok{val: cur.String(), quoted: quoted})
			cur.Reset()
			quoted, has = false, false
		}
	}
	emit := func(v string) {
		flush()
		out = append(out, tok{val: v, op: true})
	}
	word := func(c byte, q bool) {
		cur.WriteByte(c)
		has = true
		if q {
			quoted = true
		}
	}

	for i := 0; i < len(s); {
		c := s[i]

		// Substitutions open a command position even inside double quotes.
		switch {
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			emit(subst)
			stack = append(stack, inDouble)
			inDouble = false
			i += 2
			continue
		case c == '`':
			emit(subst)
			i++
			continue
		case c == ')' && len(stack) > 0:
			emit(subst)
			inDouble = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			i++
			continue
		}

		if inDouble {
			if c == '"' {
				inDouble = false
			} else {
				word(c, true)
			}
			i++
			continue
		}

		switch {
		case c == '\\' && i+1 < len(s):
			word(s[i+1], false)
			i += 2
		case c == '"':
			inDouble, has, quoted = true, true, true
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				has, quoted = true, true
				i = len(s)
				break
			}
			cur.WriteString(s[i+1 : i+1+j])
			has, quoted = true, true
			i += j + 2
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '|' || c == '&' || c == ';':
			n := 1
			if i+1 < len(s) && s[i+1] == c {
				n = 2
			}
			emit(s[i : i+n])
			i += n
		case c == '<' && i+1 < len(s) && s[i+1] == '(':
			emit(subst)
			stack = append(stack, inDouble)
			i += 2
		case c == '(' || c == ')':
			emit(subst)
			i++
		case c == '>':
			n := 1
			if i+1 < len(s) && s[i+1] == '>' {
				n = 2
			}
			emit(s[i : i+n])
			i += n
		case c == '<':
			emit("<")
			i++
		default:
			word(c, false)
			i++
		}
	}
	flush()
	return out
}

// segment is one command a shell would execute: its resolved command word, the
// arguments that belong to it alone, and where it redirects.
type segment struct {
	head    string   // command word, basename'd, wrappers stripped ("" if none)
	headRaw string   // as written, so `./deploy.sh` keeps its path
	all     []tok    // every word of the segment, head included
	args    []tok    // everything after the head
	words   []string // args that are neither flags nor quoted data
	redirs  []string // redirection targets
	sudo    bool     // reached through sudo/doas/pkexec
	inert   bool     // --help/--version/--dry-run: announces, does not act
	piped   bool     // stdin comes from the previous segment
}

// splitters end a command segment. `subst` is included so the inside of a
// substitution is judged as the separate command it is.
var splitters = map[string]bool{
	"|": true, "||": true, "&&": true, ";": true, ";;": true, "&": true, subst: true,
}

// execArgs introduce a command inside another command's argument list, which is
// how `find … -exec rm -rf {} \;` hides a deletion from a naive scan.
var execArgs = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true,
}

// wrappers run another command and are transparent to what the risk actually
// is: `sudo rm -rf /` is an rm, not a sudo. `su` and `pkexec` are deliberately
// absent — they are interesting in their own right.
var wrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true, "builtin": true,
	"exec": true, "nohup": true, "time": true, "nice": true, "ionice": true,
	"stdbuf": true, "setsid": true, "timeout": true, "xargs": true, "watch": true,
}

// privWrappers are the wrappers that also mean "as root".
var privWrappers = map[string]bool{"sudo": true, "doas": true}

// wrapperValueFlags are wrapper flags that consume the next word, so the word
// after them is not mistaken for the wrapped command.
var wrapperValueFlags = map[string]bool{
	"-u": true, "-g": true, "-p": true, "-C": true, "--user": true, "--group": true,
}

// inertFlags turn a command into an announcement. A rule must not rate
// `npm install --dry-run` the same as the install it declines to perform.
var inertFlags = map[string]bool{
	"--help": true, "--version": true, "--dry-run": true, "--dryrun": true, "--usage": true,
}

// cmdline is a parsed command: the raw text (still needed by rules that read
// content rather than structure, and by the user's regexps) and its segments.
type cmdline struct {
	raw   string
	segs  []*segment
	depth int // nesting inside interpreter payloads; bounds the recursion
}

func parse(raw string, depth int) *cmdline {
	c := &cmdline{raw: raw, depth: depth}
	var cur []tok
	piped := false
	nextPiped := false

	flush := func() {
		if s := resolve(cur, piped); s != nil {
			c.segs = append(c.segs, s)
		}
		cur = nil
		piped = nextPiped
		nextPiped = false
	}
	for _, t := range lex(raw) {
		switch {
		case t.op && splitters[t.val]:
			nextPiped = t.val == "|"
			flush()
		case t.op:
			cur = append(cur, t) // redirection, resolved below
		case !t.quoted && execArgs[t.val]:
			flush()
		default:
			cur = append(cur, t)
		}
	}
	flush()
	return c
}

// resolve turns one segment's tokens into a command: assignments and wrappers
// are stepped over, redirection targets are pulled out, and what remains is the
// command word and its own arguments.
func resolve(toks []tok, piped bool) *segment {
	s := &segment{piped: piped}

	// Redirections first, so `> /dev/sda` is a target and not an argument.
	var plain []tok
	for i := 0; i < len(toks); i++ {
		if t := toks[i]; t.op {
			if i+1 < len(toks) && !toks[i+1].op {
				s.redirs = append(s.redirs, toks[i+1].val)
				i++
			}
			continue
		}
		plain = append(plain, toks[i])
	}
	if len(plain) == 0 && len(s.redirs) == 0 {
		return nil
	}
	s.all = plain

	i := 0
	for i < len(plain) {
		t := plain[i]
		if t.quoted {
			break // quoted data never names the command
		}
		if isAssignment(t.val) {
			i++
			continue
		}
		base := basename(t.val)
		if !wrappers[base] {
			s.head, s.headRaw = base, t.val
			s.args = plain[i+1:]
			break
		}
		if privWrappers[base] {
			s.sudo = true
		}
		i++
		for i < len(plain) {
			v := plain[i].val
			if plain[i].quoted {
				break
			}
			if wrapperValueFlags[v] {
				i += 2
				continue
			}
			if strings.HasPrefix(v, "-") || isNumber(v) {
				i++
				continue
			}
			break
		}
	}

	for _, a := range s.args {
		if a.quoted {
			continue
		}
		if inertFlags[strings.SplitN(a.val, "=", 2)[0]] {
			s.inert = true
		}
		if !strings.HasPrefix(a.val, "-") {
			s.words = append(s.words, a.val)
		}
	}
	return s
}

func basename(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}

// isAssignment reports a leading `FOO=bar`, which precedes a command rather
// than being one.
func isAssignment(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (j == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// --- queries the rules are written against ---

// each runs f over every segment that actually acts, skipping the ones that
// only announce themselves (`--help`, `--dry-run`).
func (c *cmdline) each(f func(*segment) bool) bool {
	for _, s := range c.segs {
		if !s.inert && f(s) {
			return true
		}
	}
	return false
}

// cmd reports whether any segment runs one of these commands.
func (c *cmdline) cmd(names ...string) bool {
	return c.each(func(s *segment) bool {
		for _, n := range names {
			if s.head == n {
				return true
			}
		}
		return false
	})
}

// sub matches a command and the leading words of its subcommand, so
// `aws s3 rm …` is reachable without a regexp that would also match prose.
func (c *cmdline) sub(head string, words ...string) bool {
	return c.each(func(s *segment) bool { return s.is(head, words...) })
}

func (s *segment) is(head string, words ...string) bool {
	if s.head != head || len(s.words) < len(words) {
		return false
	}
	for i, w := range words {
		if s.words[i] != w {
			return false
		}
	}
	return true
}

// shortFlag reports a letter in one of this segment's own short-flag clusters —
// this segment's, which is what keeps `grep -rn "rm -rf"` from reading as an rm.
func (s *segment) shortFlag(letter byte) bool {
	for _, a := range s.args {
		if a.quoted || len(a.val) < 2 || a.val[0] != '-' || a.val[1] == '-' {
			continue
		}
		if strings.IndexByte(a.val[1:], letter) >= 0 {
			return true
		}
	}
	return false
}

// longFlag reports `--name` or `--name=value` among this segment's arguments.
func (s *segment) longFlag(name string) bool {
	for _, a := range s.args {
		if !a.quoted && (a.val == name || strings.HasPrefix(a.val, name+"=")) {
			return true
		}
	}
	return false
}

// hasArg reports an exact argument, quoted or not.
func (s *segment) hasArg(v string) bool {
	for _, a := range s.args {
		if a.val == v {
			return true
		}
	}
	return false
}

// argPrefix reports an argument beginning with p — `of=/dev/sda`, `s3://…`.
func (s *segment) argPrefix(p string) bool {
	for _, a := range s.args {
		if strings.HasPrefix(a.val, p) {
			return true
		}
	}
	return false
}

// payloads are the strings this line hands to something that will execute them:
// the quoted arguments of an interpreter (`sh -c '…'`, `python -c '…'`). Once
// inside such a payload every quoted string is suspect in turn, which is how
// `python -c 'os.system("rm -rf /")'` is reached — while `grep "rm -rf"`, whose
// command word executes nothing, is not.
func (c *cmdline) payloads() []string {
	var out []string
	for _, s := range c.segs {
		scan := s.args
		if c.depth > 0 {
			scan = s.all // already inside executable text: every string is code
		} else if !interpreters[s.head] {
			continue
		}
		for _, a := range scan {
			if a.quoted && strings.TrimSpace(a.val) != "" {
				out = append(out, a.val)
			}
		}
	}
	return out
}
