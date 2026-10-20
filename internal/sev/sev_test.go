package sev

import "testing"

// Both spellings of a rank arrive from different authors — a hand-written item
// tends to carry the letter, an audit tool writes the word — and neither should
// have to know which one a project settled on.
func TestOfAcceptsLetterAndWord(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Rank
	}{
		{"C", Critical}, {"c", Critical}, {"Critical", Critical},
		{"CRITICAL", Critical}, {"critical", Critical}, {" C ", Critical},
		{"H", High}, {"high", High}, {"High", High},
		{"M", Medium}, {"medium", Medium},
		{"L", Low}, {"low", Low},
	} {
		if got := Of(c.in); got != c.want {
			t.Errorf("Of(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The whole graceful-degradation contract: nothing here is an error, and
// nothing here outranks a declared severity.
func TestUnrecognisedRanksLast(t *testing.T) {
	for _, in := range []string{"", " ", "P0", "Sev1", "crit", "1", "-", "none"} {
		if got := Of(in); got != Unranked {
			t.Errorf("Of(%q) = %v, want Unranked", in, got)
		}
	}
	if Unranked != 0 {
		t.Fatal("Unranked must be the zero value, or an absent severity outranks a declared one")
	}
	for _, r := range Ranks() {
		if r <= Unranked {
			t.Errorf("%s does not outrank Unranked", r.Letter())
		}
	}
}

func TestRankOrder(t *testing.T) {
	ladder := []Rank{Critical, High, Medium, Low, Unranked}
	for i := 1; i < len(ladder); i++ {
		if ladder[i-1] <= ladder[i] {
			t.Fatalf("rank %d does not outrank rank %d", ladder[i-1], ladder[i])
		}
	}
}

func TestLetters(t *testing.T) {
	want := []string{"C", "H", "M", "L"}
	got := Letters()
	if len(got) != len(want) {
		t.Fatalf("Letters() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Letters()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if Unranked.Letter() != "" {
		t.Errorf("Unranked.Letter() = %q, want empty", Unranked.Letter())
	}
}

// White resolves to no escape rather than \x1b[37m, which would paint text
// white on a light-background terminal. The bool is what tells it apart from a
// name that is not in the palette at all.
func TestColourKnowsWhiteFromUnknown(t *testing.T) {
	for _, name := range ColourNames() {
		if _, ok := Colour(name); !ok {
			t.Errorf("Colour(%q) not in the palette it was listed from", name)
		}
	}
	if code, ok := Colour("white"); !ok || code != "" {
		t.Errorf("Colour(white) = %q, %v; want empty code and ok", code, ok)
	}
	if code, ok := Colour("RED"); !ok || code == "" {
		t.Errorf("Colour(RED) = %q, %v; want a code — palette lookup is case-insensitive", code, ok)
	}
	if _, ok := Colour("puce"); ok {
		t.Error("Colour(puce) reported known")
	}
}

// A severity label spells its rank out, and reads back through Of, so the word
// a label carries and the letter frontmatter carries rank the same.
func TestWordRoundTrips(t *testing.T) {
	for _, r := range Ranks() {
		if Of(r.Word()) != r {
			t.Errorf("Of(%q) = %v, want %v", r.Word(), Of(r.Word()), r)
		}
		if Of(r.Letter()).Word() != r.Word() {
			t.Errorf("letter %q spells %q, want %q", r.Letter(), Of(r.Letter()).Word(), r.Word())
		}
	}
	if Unranked.Word() != "" {
		t.Errorf("Unranked.Word() = %q, want empty", Unranked.Word())
	}
}
