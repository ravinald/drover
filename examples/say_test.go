package main

import (
	"strings"
	"testing"
)

func TestSayDrawsTheCowUnderTheBubble(t *testing.T) {
	got, err := Say("moo", "cow")
	if err != nil {
		t.Fatal(err)
	}
	want := " _____\n< moo >\n -----\n" + cow + "\n"
	if got != want {
		t.Errorf("Say(moo) =\n%s\nwant\n%s", got, want)
	}
}

func TestBubbleCollapsesWhitespace(t *testing.T) {
	if got, want := bubble("  moo\n\tmoo  "), bubble("moo moo"); got != want {
		t.Errorf("bubble with runs of whitespace =\n%s\nwant\n%s", got, want)
	}
}

func TestSayRefusesAnUnknownAnimal(t *testing.T) {
	_, err := Say("moo", "yak")
	if err == nil || !strings.Contains(err.Error(), `"yak"`) || !strings.Contains(err.Error(), "cow") {
		t.Errorf("Say with an unknown animal: err = %v, want one naming yak and the known animals", err)
	}
}
