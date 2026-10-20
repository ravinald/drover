package reports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const body = `Act as a Senior Go Backend Developer.

Requirements:

1. **Mirror F5's shape; do not invent a second one.** Add author_alias.
   Continuation lines are part of requirement 1, not a new one.
2. State the disclosure decision in writing.
10. Route what you do not own.

Not a requirement: 1999. was a year, and 2.5 is a number.
`

func TestRequirementsAreOneEntryPerNumberedLine(t *testing.T) {
	got := Requirements(body)
	if len(got) != 3 {
		t.Fatalf("got %d requirements, want 3:\n%s", len(got), strings.Join(got, "\n"))
	}
	for i, want := range []string{"- [ ] R1:", "- [ ] R2:", "- [ ] R10:"} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("entry %d = %q, want prefix %q", i, got[i], want)
		}
	}
	// The number is what makes a requirement auditable, so a continuation line
	// must not become a second checkbox.
	if strings.Contains(got[0], "Continuation") {
		t.Error("a continuation line was captured as its own requirement")
	}
}

func TestRequirementsEmptyWhenNoneNumbered(t *testing.T) {
	if got := Requirements("Just prose, no numbered list.\n"); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestRenderCarriesReportAndChecklist(t *testing.T) {
	out := Render(Data{
		ID: "F11", Title: "A review never says who wrote it", Slug: "review-attribution",
		Meta:    map[string]string{"kind": "feature", "severity": "M", "title": "ignored"},
		LogPath: "/tmp/x.jsonl", Headline: "success · error false · 12 turns · $1.5",
		Report: "shipped it", Body: body,
	})
	for _, want := range []string{
		"# F11: A review never says who wrote it",
		"- kind: feature",
		"- severity: M",
		"## Report (self-reported)",
		"shipped it",
		"- [ ] R1:",
		"**Validated by / date:**",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report is missing %q", want)
		}
	}
	// title is rendered as the heading; repeating it as a metadata row is noise.
	if strings.Contains(out, "- title:") {
		t.Error("title should not repeat as a metadata row")
	}
}

func TestRenderMarksAMissingReportAsAStub(t *testing.T) {
	out := Render(Data{ID: "1", Title: "x", Body: body})
	if !strings.Contains(out, NoResult) {
		t.Error("a run with no result must be marked, not left blank")
	}
}

// An existing result may hold a human's filled-in addendum, so it is never
// overwritten. The single exception is a stub from an interrupted run, which
// would otherwise lock out the result of the successful re-run that follows.
func TestWriteRefusesToClobberButReplacesAStub(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F11-x.md")

	stub := Data{ID: "F11", Title: "x", Body: body}
	if written, err := Write(path, stub); err != nil || !written {
		t.Fatalf("first write: written=%v err=%v", written, err)
	}

	filled := Data{ID: "F11", Title: "x", Body: body, Report: "the real self-report"}
	written, err := Write(path, filled)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("a stub must be replaceable by a real result")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "the real self-report") {
		t.Fatal("the replacement did not finish")
	}

	// Now that it holds a real report, it is somebody's work.
	if written, err := Write(path, Data{ID: "F11", Title: "x", Body: body, Report: "later run"}); err != nil || written {
		t.Errorf("an existing result must not be overwritten: written=%v err=%v", written, err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "later run") {
		t.Error("an existing result was clobbered")
	}
}

func TestWriteIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1-x.md")
	if _, err := Write(path, Data{ID: "1", Title: "x", Body: body, Report: "r"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("result mode = %o, want 600", perm)
	}
	// A temp file left behind means the rename path is wrong.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".result-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// The header is the part that looks authoritative, so it must name who actually
// answered. The item's frontmatter names who was asked for, and the two differ
// exactly when someone overrode the runner — which is when a reader most needs
// them told apart.
func TestHeaderNamesTheRunnerThatRanIt(t *testing.T) {
	out := Render(Data{
		ID: "E6", Title: "Who did which", Slug: "who-did-which",
		Meta:      map[string]string{"agent": "claude", "kind": "feature"},
		RunnerKey: "agent",
		Runner:    "codex",
		LogPath:   "/tmp/e6.jsonl",
		Report:    "done",
	})
	if !strings.Contains(out, "- Runner codex\n") {
		t.Errorf("header does not name the runner that produced the report:\n%s", out)
	}
	if !strings.Contains(out, "- agent (requested): claude\n") {
		t.Errorf("frontmatter agent is not labeled as the request it is:\n%s", out)
	}
	if strings.Contains(out, "- agent: claude\n") {
		t.Errorf("the unlabelled frontmatter line survives and still reads as the answer:\n%s", out)
	}
	if !strings.Contains(out, "- kind: feature\n") {
		t.Errorf("an ordinary metadata key was mangled:\n%s", out)
	}
}

// An item run before the record existed has no runner, and the header says so
// rather than falling back to the frontmatter or to a default.
func TestHeaderReportsAnUnrecordedRunnerAsUnknown(t *testing.T) {
	out := Render(Data{
		ID: "16", Meta: map[string]string{"agent": "claude"}, RunnerKey: "agent",
		LogPath: "/tmp/16.jsonl",
	})
	if !strings.Contains(out, "- Runner unknown") {
		t.Errorf("an unrecorded runner is not reported as unknown:\n%s", out)
	}
	if strings.Contains(out, "- Runner claude") {
		t.Errorf("the frontmatter was promoted into the runner line:\n%s", out)
	}
}
