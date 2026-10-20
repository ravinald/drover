package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const cow = `        \   ^__^
         \  (oo)\_______
            (__)\       )\/\
                ||----w |
                ||     ||`

// animals maps each name --animal accepts to its art. The art's first two
// lines carry the tail that joins it to the bubble.
var animals = map[string]string{
	"cow": cow,
}

// Say renders msg in a speech bubble above the named animal.
func Say(msg, animal string) (string, error) {
	art, ok := animals[animal]
	if !ok {
		return "", fmt.Errorf("unknown animal %q (known: %s)", animal, strings.Join(names(), ", "))
	}
	return bubble(msg) + "\n" + art + "\n", nil
}

// bubble draws msg on one line between angle brackets. Runs of whitespace
// collapse to one space, so a message read from a file draws the same as one
// typed on the command line.
func bubble(msg string) string {
	text := strings.Join(strings.Fields(msg), " ")
	width := utf8.RuneCountInString(text)
	top := " " + strings.Repeat("_", width+2)
	bottom := " " + strings.Repeat("-", width+2)
	return top + "\n< " + text + " >\n" + bottom
}

func names() []string {
	out := make([]string, 0, len(animals))
	for n := range animals {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
