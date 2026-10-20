// Package sev ranks the severity an item declares and names the colours a
// terminal renders it in.
//
// It deliberately has no error path. Severity drives display and ordering, not
// correctness, so an unrecognised value ranks last and renders plain rather
// than refusing to load the backlog that carries it. A backlog that will not
// open because someone wrote "Sev1" is a worse outcome than an uncoloured cell.
package sev

import "strings"

// Rank orders severity. Unranked is the zero value so an unrecognised, empty or
// absent severity can never outrank a declared one.
type Rank int

// The ladder, ascending. Unranked is the zero value.
const (
	Unranked Rank = iota
	Low
	Medium
	High
	Critical
)

// aliases maps every accepted spelling to its rank. Letter and word are the
// same rank because the two arrive from different places: a hand-written item
// tends to carry the letter, an audit tool writes the word, and neither author
// should have to know which one this project settled on.
var aliases = map[string]Rank{
	"c": Critical, "critical": Critical,
	"h": High, "high": High,
	"m": Medium, "medium": Medium,
	"l": Low, "low": Low,
}

// Of ranks a raw frontmatter value. Unknown, empty and absent all yield
// Unranked, which is the whole graceful-degradation contract in one line.
func Of(s string) Rank { return aliases[strings.ToLower(strings.TrimSpace(s))] }

// Letter is the one-character form, and the key a project writes in its
// [severity] ranks table. Unranked has none.
func (r Rank) Letter() string {
	switch r {
	case Critical:
		return "C"
	case High:
		return "H"
	case Medium:
		return "M"
	case Low:
		return "L"
	default:
		return ""
	}
}

// Word is the spelled-out form, which a tracker's severity label carries so the
// label needs no legend. Unranked has none.
func (r Rank) Word() string {
	switch r {
	case Critical:
		return "critical"
	case High:
		return "high"
	case Medium:
		return "medium"
	case Low:
		return "low"
	default:
		return ""
	}
}

// Ranks returns the declared ranks worst first, for enumerating in help and
// error text so a rejected value is told what it could have been.
func Ranks() []Rank { return []Rank{Critical, High, Medium, Low} }

// Letters is Ranks in its written form, for that same error text.
func Letters() []string {
	out := make([]string, 0, 4)
	for _, r := range Ranks() {
		out = append(out, r.Letter())
	}
	return out
}

// palette is 256-colour rather than the 4-bit set: orange has no 4-bit code at
// all, and 4-bit blue is close to unreadable on the dark background most
// terminals ship with.
//
// White is deliberately empty. Emitting \x1b[37m would paint text white on a
// light-background terminal, so "no colour" means no escape sequence, which
// leaves whatever foreground the reader chose.
var palette = map[string]string{
	"red":    "38;5;196",
	"orange": "38;5;208",
	"yellow": "38;5;220",
	"blue":   "38;5;39",
	"green":  "38;5;76",
	"grey":   "38;5;245",
	"white":  "",
}

// Colour resolves a palette name to SGR parameters. The second return
// distinguishes a name that is not in the palette from white, which resolves to
// no escape at all.
func Colour(name string) (string, bool) {
	code, ok := palette[strings.ToLower(strings.TrimSpace(name))]
	return code, ok
}

// ColourNames lists the palette in a stable order, for the error a typo'd
// colour earns.
func ColourNames() []string {
	return []string{"red", "orange", "yellow", "blue", "green", "grey", "white"}
}
