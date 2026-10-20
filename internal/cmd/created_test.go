package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// trackedProject is a project whose bodies are committed, which is the case a
// test asserting only the populated answer would have covered. An untracked
// backlog is the other one, and it is where a wrong zero would print.
func trackedProject(t *testing.T) string {
	t.Helper()
	isolate(t)
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug" }

[verify]
default = "true"
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-tracked.md"),
		[]byte(body("Tracked", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "the body")
	return dir
}

// The populated direction: a body git tracks has a first commit, and that date
// is the answer.
func TestCreatedReadsTheCommitThatAddedTheBody(t *testing.T) {
	dir := trackedProject(t)
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}

	date, why := s.created(it.Path)
	if why != "" {
		t.Fatalf("created is unknown in a tracked tree: %s", why)
	}
	if len(date) != len("2006-01-02") || strings.Count(date, "-") != 2 {
		t.Errorf("created = %q, want a YYYY-MM-DD date", date)
	}
}

// The degraded direction, which is drover's own backlog: a gitignored bodies
// directory has no commit to read, and the answer says which question could not
// be asked rather than printing a zero date.
func TestCreatedSaysWhyItCannotAnswerForAnUntrackedBody(t *testing.T) {
	dir := trackedProject(t)
	untracked := filepath.Join(dir, "work", "B02-untracked.md")
	if err := os.WriteFile(untracked, []byte(body("Untracked", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openIn(t, dir)

	date, why := s.created(untracked)
	if date != "" {
		t.Errorf("created = %q for a body git does not track", date)
	}
	if !strings.Contains(why, "not tracked") || !strings.Contains(why, "B02-untracked.md") {
		t.Errorf("the reason does not name the path or the question: %q", why)
	}
}

// A project that declares no work tree must not shell out at all. Reading it
// through On rather than off Enabled is what makes an unset pointer safe.
func TestCreatedDoesNotAskGitWhenGitIsOff(t *testing.T) {
	dir := trackedProject(t)
	s := openIn(t, dir)
	off := false
	s.cfg.Git.Enabled = &off

	date, why := s.created(filepath.Join(dir, "work", "B01-tracked.md"))
	if date != "" {
		t.Errorf("created = %q with [git] enabled false", date)
	}
	if !strings.Contains(why, "[git] enabled is false") {
		t.Errorf("the reason does not name the key: %q", why)
	}
}

// modified needs nothing but the file, so it is the one line show can always
// print. An empty one would mean nobody stat'd the body.
func TestShowCarriesModified(t *testing.T) {
	out := capture(t, func() {
		Show{ID: "B1", Path: "work/B01-one.md", Modified: "2026-08-27"}.print(&config.Config{})
	})
	if !strings.Contains(out, "modified   2026-08-27") {
		t.Errorf("show does not carry the mtime:\n%s", out)
	}
}

// An absent created renders as the unanswered question it is. Printing nothing
// there would read as a body with no history.
func TestShowRendersAnUnknownCreated(t *testing.T) {
	out := capture(t, func() {
		Show{ID: "B1", Modified: "2026-08-27", CreatedUnknown: ".drover/x.md is not tracked in the work tree"}.
			print(&config.Config{})
	})
	if !strings.Contains(out, "created    unknown (.drover/x.md is not tracked") {
		t.Errorf("show hides the reason created is absent:\n%s", out)
	}
}
