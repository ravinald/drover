package backlog

import "testing"

// A shape drover cannot read checks nothing while reading as though it does,
// which is the gap the table was added to close. The error has to name the
// forms, since the author is looking at the spec they just typed.
func TestParseShapeRefusesWhatItCannotCheck(t *testing.T) {
	for _, spec := range []string{"intt", "enum", "enum:", "enum:C,,L", "", "ids"} {
		if _, err := ParseShape(spec); err == nil {
			t.Errorf("ParseShape(%q) accepted a spec nothing can check", spec)
		}
	}
	for _, spec := range []string{"int", "idlist", "intlist", "enum:C,H,M,L", "enum:yes"} {
		if _, err := ParseShape(spec); err != nil {
			t.Errorf("ParseShape(%q): %v", spec, err)
		}
	}
}

// Each shape against a value that fits and one that does not, plus the empty
// value every shape passes.
func TestShapeChecksTheValueAsWritten(t *testing.T) {
	prefixes := []string{"B", "F"}
	for _, c := range []struct {
		spec, value string
		ok          bool
	}{
		{"int", "46", true},
		{"int", "-3", true},
		{"int", "banana", false},
		{"int", "46 47", false},
		{"enum:C,H,M,L", "M", true},
		{"enum:C,H,M,L", "Z", false},
		{"enum:C,H,M,L", "m", false},
		{"idlist", "B1 F2 3b", true},
		{"idlist", "B0004", false},
		{"idlist", "nonsense", false},
		{"intlist", "47 100 103", true},
		{"intlist", "47", true},
		// finish turns each of these into a close or a comment on somebody's
		// report, so a token that is not an issue number is caught here rather
		// than at the tracker.
		{"intlist", "47 banana", false},
		{"intlist", "#47", false},
		{"intlist", "0", false},
		{"intlist", "-3", false},
	} {
		shape, err := ParseShape(c.spec)
		if err != nil {
			t.Fatalf("ParseShape(%q): %v", c.spec, err)
		}
		if got := shape.Check(c.value, prefixes); (got == nil) != c.ok {
			t.Errorf("%s against %q: %v, want ok=%v", c.spec, c.value, got, c.ok)
		}
		// Presence is [frontmatter] required's question and stays there. A shape
		// that also demanded a value would move that line without saying so.
		if err := shape.Check("   ", prefixes); err != nil {
			t.Errorf("%s rejected an empty value: %v", c.spec, err)
		}
	}
}

// Ints reads past a bad token rather than refusing the list. Check is where a
// typo is reported; a reader that gave up on the first one would hide every
// number beside it, which is how ten issues go unnoticed behind one.
func TestIntsSkipsWhatIsNotANumber(t *testing.T) {
	got := Ints("47 banana 100 -3 0 103")
	want := []int{47, 100, 103}
	if len(got) != len(want) {
		t.Fatalf("Ints: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Ints: %v, want %v", got, want)
		}
	}
	if n := Ints(""); n != nil {
		t.Errorf("Ints(\"\"): %v, want nil", n)
	}
}
