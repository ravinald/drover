package cmd

import (
	"strings"
	"testing"
)

func TestSummarizeStripsTheMarkupFindingsAreWrittenIn(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"- **The mode dropped to 0644.** publish.go:88", "The mode dropped to 0644. publish.go:88"},
		{"1. **Persist by key.** store.go:41", "Persist by key. store.go:41"},
		{"* `drover check` answers 0 findings", "drover check answers 0 findings"},
		{"Publication is not atomic.", "Publication is not atomic."},
	} {
		if got := summarize(c.in); got != c.want {
			t.Errorf("summarize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A headline is bounded because the brief lists these: one paragraph among five
// one-liners reads as the round that matters, and it is only the round somebody
// wrote at length.
func TestSummarizeBoundsTheHeadline(t *testing.T) {
	long := "- " + strings.Repeat("the metadata is read before the open, ", 8)
	got := summarize(long)
	if n := len([]rune(got)); n > 97 {
		t.Errorf("headline is %d runes: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated headline does not say so: %q", got)
	}
	// Cut on runes, not bytes. A byte cut lands mid-rune on the first headline
	// quoting a path with an accent in it and the brief carries U+FFFD.
	accented := "- " + strings.Repeat("café ", 40)
	for _, r := range summarize(accented) {
		if r == '�' {
			t.Fatalf("cut mid-rune: %q", summarize(accented))
		}
	}
}

// The header carries the date and the evidence starts below it, so a brief can
// say when a round happened without a second record to keep in step.
func TestReadRoundAnswersTheDateAndTheFirstFinding(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/B01-x.changes-2.md"
	write(t, path, "# B1 request for changes 2 · 2026-09-16\n\n1. **Persist by key.** store.go:41\n\n   Detail nobody reads from a list.\n")
	date, headline := readRound(path)
	if date != "2026-09-16" {
		t.Errorf("date = %q, want 2026-09-16", date)
	}
	if headline != "Persist by key. store.go:41" {
		t.Errorf("headline = %q", headline)
	}
}

// A round drover cannot read renders as one it could not summarize. Answering
// empty rather than failing is deliberate: the brief's job is the current
// findings, which are in the body either way.
func TestReadRoundIsQuietAboutAFileThatIsNotThere(t *testing.T) {
	date, headline := readRound(t.TempDir() + "/absent.md")
	if date != "" || headline != "" {
		t.Errorf("readRound invented %q / %q", date, headline)
	}
}
