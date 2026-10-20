// Package backlog reads the backlog: one work item per markdown file, with its
// metadata in a leading frontmatter fence and its text below.
package backlog

import (
	"fmt"
	"strconv"
	"strings"
)

// ID identifies one work item.
//
// Two forms exist and they are not interchangeable. Bare is what a human
// types, what the ledger records, and what a routing label carries. Stem is
// the zero-padded form every filename is built from, so that a plain directory
// listing sorts in backlog order. Deriving one by hand instead of asking for it
// is how a saved report ends up orphaned from the ledger row that names it.
type ID struct {
	// Prefix is empty for a planned item, else a namespace key such as "B".
	Prefix string
	Num    int
	// Sub is an optional trailing lowercase letter splitting one item into
	// several: 2b, 25e.
	Sub string
}

// Bare renders the form a human types: no padding. "2b", "B1", "25e".
func (id ID) Bare() string {
	return id.Prefix + strconv.Itoa(id.Num) + id.Sub
}

// Stem renders the zero-padded filename component. "02b", "B01", "25e".
func (id ID) Stem() string {
	return fmt.Sprintf("%s%02d%s", id.Prefix, id.Num, id.Sub)
}

// Planned reports whether this is a planned-backlog item rather than one that
// arrived after the plan closed.
func (id ID) Planned() bool { return id.Prefix == "" }

// Less orders ids the way a reader expects to scan them: planned items first
// in numeric order, then each intake namespace grouped and numbered. Comparing
// Stem strings would put B10 before B9.
func (id ID) Less(other ID) bool {
	if id.Prefix != other.Prefix {
		// Planned items lead; the rest fall in namespace order.
		if id.Planned() != other.Planned() {
			return id.Planned()
		}
		return id.Prefix < other.Prefix
	}
	if id.Num != other.Num {
		return id.Num < other.Num
	}
	return id.Sub < other.Sub
}

// ParseID reads an id in its bare form. prefixes are the configured intake
// namespace keys; an id carrying any other leading letter is rejected rather
// than silently read as a planned item, since that would route its labels and
// filenames into the wrong namespace.
func ParseID(s string, prefixes []string) (ID, error) {
	var id ID
	if s == "" {
		return id, fmt.Errorf("empty id")
	}

	rest := s
	// Longest match first: a one-character scan would mis-split a two-character
	// namespace key the moment one is configured.
	for _, p := range longestFirst(prefixes) {
		if strings.HasPrefix(rest, p) {
			id.Prefix, rest = p, rest[len(p):]
			break
		}
	}

	// ASCII-only on purpose: every id becomes a filename component, a branch
	// name and a label, and none of those want a Unicode lowercase letter.
	if n := len(rest); n > 0 && rest[n-1] >= 'a' && rest[n-1] <= 'z' {
		id.Sub, rest = rest[n-1:], rest[:n-1]
	}

	if rest == "" {
		return ID{}, fmt.Errorf("id %q has no number", s)
	}
	// Reject a leading zero rather than accept it: "08" and "8" would name the
	// same item while rendering as two distinct ledger rows.
	//
	// The suggestion carries the prefix and the sub-letter back. Built from the
	// number alone it would point at a different item: "B01" would answer "ids
	// are bare (1)", and 1 is a planned item in every project that has both
	// namespaces.
	if len(rest) > 1 && rest[0] == '0' {
		if bare := strings.TrimLeft(rest, "0"); bare != "" {
			return ID{}, fmt.Errorf("id %q is zero-padded; ids are bare (%s)", s, id.Prefix+bare+id.Sub)
		}
		return ID{}, fmt.Errorf("id %q: number must be 1 or greater", s)
	}
	num, err := strconv.Atoi(rest)
	if err != nil {
		return ID{}, fmt.Errorf("id %q: %q is not a number", s, rest)
	}
	if num < 1 {
		return ID{}, fmt.Errorf("id %q: number must be 1 or greater", s)
	}
	id.Num = num
	return id, nil
}

// ParseStem reads the zero-padded form found at the head of a filename.
func ParseStem(s string, prefixes []string) (ID, error) {
	var id ID
	rest := s
	for _, p := range longestFirst(prefixes) {
		if strings.HasPrefix(rest, p) {
			id.Prefix, rest = p, rest[len(p):]
			break
		}
	}
	if n := len(rest); n > 0 && rest[n-1] >= 'a' && rest[n-1] <= 'z' {
		id.Sub, rest = rest[n-1:], rest[:n-1]
	}
	num, err := strconv.Atoi(rest)
	if err != nil {
		return ID{}, fmt.Errorf("filename stem %q: %q is not a number", s, rest)
	}
	if num < 1 {
		return ID{}, fmt.Errorf("filename stem %q: number must be 1 or greater", s)
	}
	id.Num = num
	return id, nil
}

func longestFirst(prefixes []string) []string {
	out := make([]string, len(prefixes))
	copy(out, prefixes)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
