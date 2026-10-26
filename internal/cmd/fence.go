package cmd

import "strings"

// fence follows CommonMark fenced code blocks through a body one line at a
// time. A fence opens on a run of three or more backticks or tildes, indented
// at most three spaces, and closes only on a run of the same character at least
// as long with nothing but whitespace after it. A plain toggle on any
// three-character run closes a four-backtick fence on the three-backtick fence
// it quotes, and closes a tilde fence on a backtick line.
type fence struct {
	char byte
	run  int // zero while no fence is open
}

// skip advances past line and reports whether it is fence syntax or sits
// inside a fenced block. A fence never closed skips the rest of the body.
func (f *fence) skip(line string) bool {
	char, run, rest := fenceRun(line)
	if f.run == 0 {
		if run == 0 || char == '`' && strings.ContainsRune(rest, '`') {
			return false
		}
		f.char, f.run = char, run
		return true
	}
	if char == f.char && run >= f.run && strings.TrimSpace(rest) == "" {
		f.run = 0
	}
	return true
}

// fenceRun splits a line into the fence character, the length of its run and
// what follows the run. run is zero when the line cannot open or close a fence.
func fenceRun(line string) (char byte, run int, rest string) {
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if indent > 3 {
		return 0, 0, ""
	}
	s := line[indent:]
	if s == "" || s[0] != '`' && s[0] != '~' {
		return 0, 0, ""
	}
	n := len(s) - len(strings.TrimLeft(s, s[:1]))
	if n < 3 {
		return 0, 0, ""
	}
	return s[0], n, s[n:]
}
