package cmd

import (
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// palette names what a piece of terminal output means, and the SGR parameters
// that say it. Only the 16 base colors, bold and dim: the terminal's theme
// picks the shade, so the same code reads on a light background and a dark
// one. Color only reinforces a word or glyph already on the line, so a reader
// who cannot see it, a pipe and NO_COLOR lose nothing.
type palette struct {
	OK      string // merged, passed, done
	Warn    string // paused, held, waiting, skipped, a usage warning
	Fail    string // parked, refused, a red check, an error
	Heading string // the ==> lines, help's section headings
	Detail  string // the · lines
	ID      string // an item or drive id
	Command string // a command the operator is told to run
	Muted   string // timestamps, paths
}

var pal = palette{
	OK:      "32",
	Warn:    "33",
	Fail:    "31",
	Heading: "1",
	Detail:  "2",
	ID:      "36",
	Command: "35",
	Muted:   "2",
}

// state is the role of one state, outcome or verdict word as drover prints
// it, empty for a word that carries none.
func (p palette) state(word string) string {
	switch word {
	case stateDone, outcomeReviewed, outcomeMergedByGate, outcomeMergedAtForge,
		passPassed, "done", "ok", "verified", "fixed", stateReady, "READY":
		return p.OK
	case outcomeParked, "refused", "red", "trunk_red", "failed", "failure", stateBroken,
		"needs-operator", "interrupted", passDied, passFaulted, passUnreadable, passSilent,
		passCapped, passRepeating, passNeedsDecision:
		return p.Fail
	case outcomeSkipped, outcomeHeld, "paused", "waiting", "running, paused",
		evNeedsOperator, evNeedsSupervisor, evUsageWarning, evReexecWaiting, evConfigChanged,
		stateBlocked, stateScaffold, passChangesRequested, passStanding, passChangesUnrecorded,
		passEdited, "UNAUDITED", "unknown":
		return p.Warn
	}
	return ""
}

// row is the role of a status row's state cell: a park outranks the state it
// parked in.
func (p palette) row(r Row) string {
	if r.Park != "" {
		return p.Fail
	}
	if r.State == stateDone && r.Level == "" {
		return p.Warn
	}
	return p.state(r.State)
}

// event is the role of a drive event's name.
func (p palette) event(name string) string {
	switch name {
	case evMerged:
		return p.OK
	case evMergeQueued:
		return p.Warn
	case evParked:
		return p.Fail
	case evCheck, evStarted, evQueued, evDispatched, evStage, evFiled, evResumed, evExit:
		return ""
	}
	return p.state(name)
}

// errOut is stderr as the drive stream and every progress line write it,
// styled line by line when stderr takes color. It resolves os.Stderr at each
// write rather than holding the file, so a test that swaps the stream still
// reads every line, and what reaches a pipe or the detached log is the bytes
// the format string produced.
var errOut io.Writer = lineStyler{stream: func() *os.File { return os.Stderr }}

type lineStyler struct{ stream func() *os.File }

// Write styles each line of p and hands the result to the stream in one write,
// so lines from items running at once interleave no worse than before.
func (l lineStyler) Write(p []byte) (int, error) {
	f := l.stream()
	if !colorFor(f) {
		return f.Write(p)
	}
	lines := strings.Split(string(p), "\n")
	for i, line := range lines {
		lines[i] = styleLine(line)
	}
	if _, err := io.WriteString(f, strings.Join(lines, "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

var (
	failWords = regexp.MustCompile(`\b(parked|parks|refused|refuses|trunk_red|red|failed|exited [1-9][0-9]*)\b`)
	warnWords = regexp.MustCompile(`\b(paused|held|holds|waits|waiting|skipped|needs_operator|usage_warning|usage_admission|interrupted|stopping|stopped)\b`)
	okWords   = regexp.MustCompile(`\b(merged|passed|finished|ready_to_merge|merged_by_drover|merged_immediately)\b`)
	// idWord is an id that cannot be a count: a prefixed one (B84, F14) or a
	// drive's (D3). A bare planned id is a number, so it is an id only where
	// the line's form says so: leading a detail line or a warning, heading a
	// stage's preamble, listed on the drive header, or on a line of its own.
	idWord = regexp.MustCompile(`\b[A-Z][0-9]+[a-z]?\b`)
	// A detail line's id is followed by a colon, because "· 4 merged" opens
	// on a count.
	leadIDs = []*regexp.Regexp{
		regexp.MustCompile(`^(\s*· )([A-Za-z]?[0-9]+[a-z]?)(:)`),
		regexp.MustCompile(`^(\s*! )([A-Za-z]?[0-9]+[a-z]?)([: ])`),
		regexp.MustCompile(`^(\s*==> )([A-Za-z]?[0-9]+[a-z]?)( · )`),
	}
	idList    = regexp.MustCompile(`^(\s*==> drive · [0-9]+ items?: )(.+)$`)
	bareID    = regexp.MustCompile(`^[A-Za-z]?[0-9]+[a-z]?$`)
	zeroCount = regexp.MustCompile(`(^|[^0-9.])0 $`)
	oscLink   = regexp.MustCompile(`\x1b\]8;;[^\x1b]*\x1b\\`)
	cmdOnce   sync.Once
	cmdRegexp *regexp.Regexp
)

// commandPattern matches "drover <command>". The names come from the tree,
// so "drover never launched it" is prose and "drover finish 4" is a command.
func commandPattern() *regexp.Regexp {
	cmdOnce.Do(func() {
		var names []string
		for _, c := range root.Commands() {
			names = append(names, regexp.QuoteMeta(c.Name()))
		}
		// Longest first, so drive is not taken for the start of drives.
		slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })
		cmdRegexp = regexp.MustCompile(`\bdrover (?:` + strings.Join(names, "|") + `)\b`)
	})
	return cmdRegexp
}

var (
	argToken = regexp.MustCompile(`^(?:--?[a-z][a-z0-9-]*(?:=\S+)?|#?[A-Za-z]?[0-9]+[a-z]?|<[^>]+>|"[^"]*"|'[^']*')$`)
	flagWord = regexp.MustCompile(`^[a-z][a-z0-9_./-]*$`)
	// prose is the words that follow a flag in a sentence rather than as its
	// value: "drover drive --resume to continue".
	prose = map[string]bool{"to": true, "and": true, "or": true, "then": true, "the": true, "a": true,
		"once": true, "until": true, "with": true, "when": true, "if": true, "which": true, "so": true,
		"for": true, "before": true, "after": true, "on": true, "in": true, "at": true, "is": true,
		"brings": true, "first": true, "again": true, "instead": true}
)

// commandEnd is where the arguments of the command ending at at stop: the
// flags, ids, numbers, placeholders and quoted strings after it, and a word
// after a flag that reads as its value. Trailing punctuation ends the command
// without belonging to it.
func commandEnd(s string, at int) int {
	afterFlag := false
	for at < len(s) && s[at] == ' ' {
		rest := s[at+1:]
		n := strings.IndexByte(rest, ' ')
		if n < 0 {
			n = len(rest)
		}
		if strings.HasPrefix(rest, `"`) {
			if q := strings.IndexByte(rest[1:], '"'); q >= 0 {
				n = q + 2
			}
		}
		tok := rest[:n]
		bare := strings.TrimRight(tok, ",;:.)`")
		switch {
		case bare == "":
			return at
		case argToken.MatchString(bare):
			afterFlag = strings.HasPrefix(bare, "-") && !strings.Contains(bare, "=")
		case afterFlag && flagWord.MatchString(bare) && !prose[bare]:
			afterFlag = false
		default:
			return at
		}
		if bare != tok {
			return at + 1 + len(bare)
		}
		at += 1 + n
	}
	return at
}

// styleLine colors one line of the stream by what it reports. A line a caller
// already colored is left as the caller wrote it.
func styleLine(line string) string {
	if line == "" || strings.Contains(line, "\x1b[") {
		return line
	}
	trimmed := strings.TrimLeft(line, " ")
	role := ""
	switch {
	case strings.HasPrefix(trimmed, "==> "):
		role = pal.Heading
	case strings.HasPrefix(trimmed, "· "), strings.HasPrefix(trimmed, "! "), strings.HasPrefix(trimmed, "Outcome:"):
		role = lineRole(trimmed)
		if role == "" && strings.HasPrefix(trimmed, "· ") {
			role = pal.Detail
		}
		if role == "" && strings.HasPrefix(trimmed, "! ") {
			role = pal.Warn
		}
	case bareID.MatchString(trimmed):
		return sgr(line, pal.ID, true)
	}
	return sgr(highlight(line), role, true)
}

// lineRole is the most serious state a line reports. A count of zero reports
// nothing: "0 parked" is the routine case, not a park.
func lineRole(s string) string {
	for _, c := range []struct {
		re   *regexp.Regexp
		role string
	}{{failWords, pal.Fail}, {warnWords, pal.Warn}, {okWords, pal.OK}} {
		for _, m := range c.re.FindAllStringIndex(s, -1) {
			if !zeroCount.MatchString(s[:m[0]]) {
				return c.role
			}
		}
	}
	return ""
}

// highlight colors the commands and ids inside a line, outside any OSC 8 link
// so a URL keeps its bytes.
func highlight(line string) string {
	var b strings.Builder
	if p := idList.FindStringSubmatch(line); p != nil {
		ids := strings.Split(p[2], " ")
		for i, id := range ids {
			ids[i] = sgr(id, pal.ID, true)
		}
		return p[1] + strings.Join(ids, " ")
	}
	for _, re := range leadIDs {
		if p := re.FindStringSubmatch(line); p != nil {
			b.WriteString(p[1] + sgr(p[2], pal.ID, true) + p[3])
			line = line[len(p[0]):]
			break
		}
	}
	at := 0
	for _, m := range oscLink.FindAllStringIndex(line, -1) {
		b.WriteString(highlightText(line[at:m[0]]))
		b.WriteString(line[m[0]:m[1]])
		at = m[1]
	}
	b.WriteString(highlightText(line[at:]))
	return b.String()
}

// referents are the words that make what follows a noun phrase, so "the next
// drover drive finishes it" names a drive and tells nobody to run one.
var referents = map[string]bool{"the": true, "a": true, "an": true, "next": true, "this": true,
	"that": true, "each": true, "every": true, "one": true}

// namesACommand reports whether the text before a match turns it into prose
// about a command rather than a command to run.
func namesACommand(before string) bool {
	words := strings.Fields(before)
	return len(words) > 0 && referents[strings.ToLower(words[len(words)-1])]
}

func highlightText(s string) string {
	ids := func(t string) string {
		return idWord.ReplaceAllStringFunc(t, func(id string) string { return sgr(id, pal.ID, true) })
	}
	var b strings.Builder
	at := 0
	for _, m := range commandPattern().FindAllStringIndex(s, -1) {
		if m[0] < at || namesACommand(s[:m[0]]) {
			continue
		}
		end := commandEnd(s, m[1])
		b.WriteString(ids(s[at:m[0]]))
		b.WriteString(sgr(s[m[0]:end], pal.Command, true))
		at = end
	}
	b.WriteString(ids(s[at:]))
	return b.String()
}
