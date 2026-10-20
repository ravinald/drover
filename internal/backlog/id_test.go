package backlog

import (
	"sort"
	"strings"
	"testing"
)

var prefixes = []string{"B", "E", "F"}

func TestParseIDRoundTrip(t *testing.T) {
	cases := []struct {
		bare string
		stem string
	}{
		{"1", "01"},
		{"2b", "02b"},
		{"11c", "11c"},
		{"25e", "25e"},
		{"B1", "B01"},
		{"B13", "B13"},
		{"E17", "E17"},
		{"F5", "F05"},
		{"F20", "F20"},
	}
	for _, c := range cases {
		id, err := ParseID(c.bare, prefixes)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", c.bare, err)
		}
		if got := id.Bare(); got != c.bare {
			t.Errorf("ParseID(%q).Bare() = %q", c.bare, got)
		}
		if got := id.Stem(); got != c.stem {
			t.Errorf("ParseID(%q).Stem() = %q, want %q", c.bare, got, c.stem)
		}
		back, err := ParseStem(c.stem, prefixes)
		if err != nil {
			t.Fatalf("ParseStem(%q): %v", c.stem, err)
		}
		if back != id {
			t.Errorf("ParseStem(%q) = %+v, want %+v", c.stem, back, id)
		}
	}
}

func TestParseIDRejects(t *testing.T) {
	// "08" is the case that took prompt.sh down under set -e: bash read it as
	// invalid octal. Here it is rejected for a different reason worth keeping —
	// a padded id and its bare twin would be two distinct ledger rows.
	for _, s := range []string{"", "0", "08", "2bb", "X1", "b1", "-1", "2.5", "F", "1B"} {
		if id, err := ParseID(s, prefixes); err == nil {
			t.Errorf("ParseID(%q) = %+v, want error", s, id)
		}
	}
}

func TestLessGroupsPlannedFirstAndSortsNumerically(t *testing.T) {
	raw := []string{"B10", "2b", "F5", "B9", "1", "E1", "2a", "25e", "B1", "10"}
	ids := make([]ID, 0, len(raw))
	for _, s := range raw {
		id, err := ParseID(s, prefixes)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", s, err)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Less(ids[j]) })

	want := []string{"1", "2a", "2b", "10", "25e", "B1", "B9", "B10", "E1", "F5"}
	for i, w := range want {
		if got := ids[i].Bare(); got != w {
			t.Fatalf("position %d = %q, want %q (full: %v)", i, got, w, bares(ids))
		}
	}
}

func TestLongestPrefixWins(t *testing.T) {
	// A two-character namespace must not be split by the one-character scan.
	id, err := ParseID("BUG3", []string{"B", "BUG"})
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if id.Prefix != "BUG" || id.Num != 3 {
		t.Errorf("got %+v, want prefix BUG num 3", id)
	}
}

func bares(ids []ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.Bare()
	}
	return out
}

// The suggestion in a zero-padded rejection is the id a caller will retype, so
// it has to name the same item. Built from the number alone it would name a
// different one: "B01" would answer "(1)", and 1 is a planned item in any
// project with both namespaces.
func TestZeroPaddedSuggestsTheSameItem(t *testing.T) {
	for _, c := range []struct{ padded, want string }{
		{"B01", "(B1)"},
		{"B01b", "(B1b)"},
		{"08", "(8)"},
		{"F012", "(F12)"},
	} {
		_, err := ParseID(c.padded, prefixes)
		if err == nil {
			t.Fatalf("ParseID(%q): want error", c.padded)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseID(%q) = %v, want the suggestion %s", c.padded, err, c.want)
		}
		// The suggestion has to be something the caller can retype. Stem is only
		// asserted where the input was a real filename stem: "F012" carries a
		// zero no stem ever renders, so it has no padded form to round-trip to.
		bare := strings.Trim(c.want, "()")
		id, err := ParseID(bare, prefixes)
		if err != nil {
			t.Errorf("the suggestion %q does not parse: %v", bare, err)
			continue
		}
		if got := id.Bare(); got != bare {
			t.Errorf("ParseID(%q).Bare() = %q", bare, got)
		}
		if len(id.Stem()) == len(c.padded) && id.Stem() != c.padded {
			t.Errorf("ParseID(%q).Stem() = %q, want the %q the caller typed", bare, id.Stem(), c.padded)
		}
	}
}

// A padded id whose number is all zeros has no bare form to suggest, so it gets
// the range error rather than an empty parenthetical.
func TestAllZeroIDIsARangeError(t *testing.T) {
	_, err := ParseID("B00", []string{"B"})
	if err == nil {
		t.Fatal("ParseID(\"B00\"): want error")
	}
	if !strings.Contains(err.Error(), "1 or greater") {
		t.Errorf("ParseID(\"B00\") = %v, want a range error", err)
	}
}
