package cmd

import (
	"errors"
	"fmt"
	"testing"
)

// A wrapper reads the code, not the message. "drover could not answer",
// "the project failed its own validation" and "drover refused" call for three
// different responses, and collapsing them into 1 is what forced stderr parsing.
func TestCodeSeparatesRefusalFromFailure(t *testing.T) {
	base := errors.New("boom")
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"clean", nil, 0},
		{"plain error", base, ExitError},
		{"findings", Findings{base}, ExitFindings},
		{"refusal", Refusal{base}, ExitRefused},
		// Cobra and the command layer wrap errors on the way out, so the code has
		// to survive a wrap or it is only correct at the return statement.
		{"wrapped findings", fmt.Errorf("check: %w", Findings{base}), ExitFindings},
		{"wrapped refusal", fmt.Errorf("finish: %w", Refusal{base}), ExitRefused},
	} {
		if got := Code(c.err); got != c.want {
			t.Errorf("%s: Code = %d, want %d", c.name, got, c.want)
		}
	}
}

// The message a person reads must survive the wrapper that carries the code.
func TestRefusalKeepsItsMessage(t *testing.T) {
	r := Refusal{errors.New("2 does not look merged")}
	if r.Error() != "2 does not look merged" {
		t.Errorf("Error() = %q", r.Error())
	}
	if !errors.Is(fmt.Errorf("x: %w", r), r.Err) {
		t.Error("the wrapped cause is not reachable through errors.Is")
	}
}
