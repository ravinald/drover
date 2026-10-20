package cmd

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSgrLeavesTheStringAloneWhenItHasNothingToSay(t *testing.T) {
	for _, c := range []struct {
		name string
		code string
		on   bool
	}{
		{"off", "38;5;196", false},
		{"no colour", "", true},
		{"off and no colour", "", false},
	} {
		if got := sgr("B01", c.code, c.on); got != "B01" {
			t.Errorf("%s: sgr = %q, want the string untouched", c.name, got)
		}
	}
	if got := sgr("B01", "38;5;196", true); got != "\x1b[38;5;196mB01\x1b[0m" {
		t.Errorf("sgr = %q, want the cell wrapped", got)
	}
}

// The trap this guards: fmt counts escape bytes as width, so a cell colored
// before it is padded shears every column to its right by the length of the
// escape. Pad first, color second.
func TestColourAfterPaddingKeepsTheColumnWidth(t *testing.T) {
	const w = 6
	plain := fmt.Sprintf("%-*s", w, "C")
	tinted := sgr(plain, "38;5;196", true)

	if visible := utf8.RuneCountInString(strip(tinted)); visible != w {
		t.Errorf("visible width = %d, want %d", visible, w)
	}
	// The other order, shown failing, so the test says why it exists.
	wrong := fmt.Sprintf("%-*s", w, sgr("C", "38;5;196", true))
	if utf8.RuneCountInString(strip(wrong)) == w {
		t.Error("colouring before padding produced the right width — the trap is gone and this test is stale")
	}
}

// strip removes SGR sequences so a test can measure what a reader sees.
func strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// The bytes a terminal reads, both ways: on, the OSC 8 pair around the visible
// number; off, the number alone and not one escape byte.
func TestHyperlinkWritesExactBytes(t *testing.T) {
	url := "https://github.com/o/r/pull/236"
	if got, want := hyperlink("#236", url, true), "\x1b]8;;"+url+"\x1b\\#236\x1b]8;;\x1b\\"; got != want {
		t.Errorf("on: hyperlink = %q, want %q", got, want)
	}
	if got := hyperlink("#236", url, false); got != "#236" {
		t.Errorf("off: hyperlink = %q, want %q", got, "#236")
	}
	if got := hyperlink("#236", "", true); got != "#236" {
		t.Errorf("no url: hyperlink = %q, want %q", got, "#236")
	}
}

// links is off on a pipe whatever the environment says, and off on a
// terminal under NO_COLOR or TERM=dumb. A pty is not available here, so the
// terminal half is the env switches over a stream that is not one.
func TestLinksAreOffWithoutATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if links(w) {
		t.Error("links on a pipe")
	}
	for _, env := range [][2]string{{"NO_COLOR", "1"}, {"TERM", "dumb"}} {
		t.Setenv(env[0], env[1])
		if links(w) {
			t.Errorf("links with %s=%s", env[0], env[1])
		}
	}
}

// A padded reference pads by what the reader sees, so a linked number keeps
// the column to its right where an unlinked one would.
func TestPadRefCountsTheVisibleNumber(t *testing.T) {
	linked := hyperlink("#7", "https://x/7", true)
	if got, want := padRef(linked, 7), linked+"    "; got != want {
		t.Errorf("padRef = %q, want %q", got, want)
	}
	if got := padRef("#123456", 123456); got != "#123456" {
		t.Errorf("padRef past the width = %q", got)
	}
}
