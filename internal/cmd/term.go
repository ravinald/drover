package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

// Column budget: the glyph, id and column cells, plus the widest status cell
// and its gap. The status cell is what people scan, so it stays flush and the
// title yields.
const (
	fixedCells = 18
	statusCell = 22
	minTitle   = 24
	// maxColumn bounds the project-defined column so a long value cannot crowd
	// out the title entirely.
	maxColumn = 12
	// maxNote bounds how much an unusually long status cell, an unreadable
	// body's parse error, can take from every title in the table.
	maxNote = 40
	// maxPR bounds the PR cell. It fits "#12345 unknown (not authenticated)", so the
	// reason survives beside a recorded number.
	maxPR        = 34
	maxTitle     = 71
	fallbackTerm = 110
)

// titleWidth sizes the title column beside the fixed status cell the list
// commands share.
func titleWidth() int { return titleWidthBeside(statusCell) }

// titleWidthBeside sizes the title column to the terminal once noteCell columns
// are set aside for the cell after it, gap included.
//
// COLUMNS wins when set, then the terminal stdout is attached to, then a fixed
// fallback. The terminal has to be asked: zsh sets COLUMNS without exporting
// it, so reading the variable alone sized every interactive table to the
// fallback. maxTitle bounds only that fallback, since a measured width fits the
// screen by construction and a pipe has no screen to fit.
func titleWidthBeside(noteCell int) int {
	w, measured := fallbackTerm, false
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			w, measured = n, true
		}
	}
	if !measured {
		if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
			if n, _, err := term.GetSize(fd); err == nil && n > 0 {
				w, measured = n, true
			}
		}
	}
	w -= fixedCells + noteCell
	if w < minTitle {
		return minTitle
	}
	if !measured && w > maxTitle {
		return maxTitle
	}
	return w
}

// colour is opt-out: a pipe has nobody to read the escapes, and NO_COLOR is the
// one convention a tool emitting them is expected to honour.
func colour() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// links is colour()'s rule for OSC 8 hyperlinks, asked of the stream the line
// is written to, plus TERM=dumb: a terminal that declares it understands no
// escapes prints this one as garbage around every number.
func links(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return onTerminal(f)
}

// onTerminal is a variable so a test can drive the terminal-only paths, which
// a captured stream never reaches.
var onTerminal = func(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// hyperlink wraps text in an OSC 8 hyperlink to url. The visible text is the
// same either way, so a terminal that ignores the sequence, a pipe and a
// screen reader all read the number they always did. An empty url is no link.
func hyperlink(text, url string, on bool) string {
	if !on || url == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// issueRefTo is #n for an issue on the project's forge, linked when f takes
// links. A register kept in a file has no web page behind its numbers, so
// those stay plain through registerRefTo.
func (s *session) issueRefTo(f *os.File, n int) string {
	return s.forgeRef(f, n, "issues", "")
}

// prRefTo is #n for a pull request. url is the one the forge reported, when
// the caller has it, so no second read is spent building it.
func (s *session) prRefTo(f *os.File, n int, url string) string {
	return s.forgeRef(f, n, "pull", url)
}

func (s *session) registerRefTo(f *os.File, n int) string {
	if s == nil || s.cfg.RegisterBackend() != config.BackendGitHub {
		return fmt.Sprintf("#%d", n)
	}
	return s.issueRefTo(f, n)
}

func (s *session) forgeRef(f *os.File, n int, kind, url string) string {
	text := fmt.Sprintf("#%d", n)
	if s == nil || !links(f) {
		return text
	}
	if url == "" && !s.offline {
		if base := s.repoURL(); base != "" {
			url = fmt.Sprintf("%s/%s/%d", base, kind, n)
		}
	}
	return hyperlink(text, url, true)
}

// repoURL asks gh once per process which repository it resolves here, and
// only when a link is about to be written. A failure is no link, never an
// error: the number reads the same without one.
func (s *session) repoURL() string {
	if s.forgeURL == nil {
		u, err := gh.RepoURL(s.labels())
		if err != nil {
			u = ""
		}
		s.forgeURL = &u
	}
	return *s.forgeURL
}

// sgr wraps s in SGR parameters. An empty code is the way to say "no colour":
// it returns the string untouched rather than emitting a reset, so a palette
// entry meaning "leave the reader's foreground alone" costs nothing.
//
// Callers pad before they colour. fmt counts escape bytes as width, so a cell
// wrapped first and padded second shears the table by the length of the escape.
func sgr(s, code string, on bool) string {
	if !on || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// paint marks a line as the one worth looking at. Colour is never the only
// signal — the caller also carries a marker glyph and a footer note, because a
// reader who cannot see green still has to find their place.
func paint(s string, on bool) string { return sgr(s, "1;32", on) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// trunc shortens to n display columns, counting runes so a multi-byte title is
// not cut mid-character.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

// padRef pads a linked #n to the six columns "#%-5d" gives the listings. The
// link's escape bytes carry no width on screen and all of it to fmt, so the
// padding is counted from the number rather than from the string.
func padRef(ref string, n int) string {
	if pad := 5 - len(strconv.Itoa(n)); pad > 0 {
		return ref + strings.Repeat(" ", pad)
	}
	return ref
}

// registerRefsTo and issueRefsTo are renderInts with each number linked.
func (s *session) registerRefsTo(f *os.File, xs []int) string {
	return refs(xs, func(n int) string { return s.registerRefTo(f, n) })
}

func (s *session) issueRefsTo(f *os.File, xs []int) string {
	return refs(xs, func(n int) string { return s.issueRefTo(f, n) })
}

func refs(xs []int, ref func(int) string) string {
	if len(xs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, ref(x))
	}
	return strings.Join(parts, ", ")
}
