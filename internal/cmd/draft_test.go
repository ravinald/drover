package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
)

// draftProject builds a tree holding both states draft distinguishes: a
// scaffold still carrying the marker, and an item whose requirements a human
// already accepted. The fixture above cannot serve, because its frontmatter
// schema is closed and declares no issue key.
func draftProject(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n")
	write(t, filepath.Join(dir, config.FileName), schemaLine+`project = "example"

[paths]
backlog      = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "go.mod"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["issue", "agent"]

[agents]
principles = "no raw SQL, permissions per object"

[intake]
stop_marker = "STOP: requirements not yet written"

[verify]
default = "make test"

[github]
repo = "owner/example"
`+extra)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\nRequirements:\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "B01-empty-feed.md"),
		"---\ntitle: Empty feed\ndeps:\nissue: 12\n---\n\n"+
			"STOP: requirements not yet written\n\nRequirements:\n\n1.\n2.\n")
	t.Chdir(dir)
	return dir
}

// stubIssueGH answers the two calls the drafting brief makes: the availability
// probe and the issue read. Anything else fails here rather than reaching the
// network.
func stubIssueGH(t *testing.T, dir string) {
	t.Helper()
	body := `{"number":12,"title":"feed renders empty after a purge",` +
		`"body":"Reproduced twice on staging. The purge leaves the index empty.",` +
		`"state":"OPEN","labels":[]}`
	t.Setenv("DROVER_TEST_GH_ISSUE", body)
	stub(t, filepath.Join(dir, "gh"), ghIssueView)
}

// Everything an author needs and an implement prompt does not. Each string below is one
// of the things a body written without it comes back missing: requirements
// nobody can falsify, a defect absorbed from a sibling, or a marker deleted by
// the agent that drafted the very requirements the marker holds inert.
func TestDraftBriefCarriesWhatAnAuthorNeeds(t *testing.T) {
	t.Cleanup(func() { draftRunner, draftHeadless = "", false })
	dir := draftProject(t, "")
	stubIssueGH(t, stubPATH(t))

	var err error
	out := capture(t, func() { err = execute(t, "draft", "B1") })
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	for _, want := range []string{
		"work/B01-empty-feed.md",              // the file to write
		"feed renders empty after a purge",    // the issue title, read live
		"The purge leaves the index empty.",   // the issue body
		"evidence, not requirements",          // and what it is not
		"no raw SQL, permissions per object",  // [agents] principles
		"number, a period and a space",        // the numbered-list contract
		"1  First (work/01-first.md)",         // the sibling, for routing
		"STOP: requirements not yet written",  // the marker, left in place
		"Deleting it is the act of accepting", // and why the agent must not
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the prompt omits %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "could not be read") {
		t.Errorf("the issue read cleanly; the prompt should not report it unavailable:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ledger.jsonl")); statErr == nil {
		t.Error("draft wrote a ledger row; assembling a prompt records nothing")
	}
}

// A body past intake is somebody's accepted contract. Redrafting it would
// overwrite requirements a human read and signed off, which is the one edit in
// this tree nobody would notice until the run executed the new version.
func TestDraftRefusesABodyPastIntake(t *testing.T) {
	draftProject(t, "")
	err := execute(t, "draft", "1")
	if err == nil {
		t.Fatal("draft accepted an item whose stop marker is gone")
	}
	if got := Code(err); got != ExitRefused {
		t.Errorf("exit %d, want %d", got, ExitRefused)
	}
	if !strings.Contains(err.Error(), "work/01-first.md") {
		t.Errorf("the refusal must name the body path, got: %v", err)
	}
}

// The division run and review keep: drover assembles and launches, the agent
// writes. A draft that left a ledger row or touched the body itself would be
// claiming work it did not do.
func TestDraftLaunchesAndWritesNothingItself(t *testing.T) {
	t.Cleanup(func() { draftRunner, draftHeadless = "", false })
	logs := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", logs)
	dir := draftProject(t, `
[run]
draft_runner = "drafter"

[runners.drafter]
interactive = ["stub-agent"]
headless    = ["stub-agent"]
`)
	bin := stubPATH(t)
	stubIssueGH(t, bin)
	stubRunner(t, bin, "stub-agent", "")

	body := filepath.Join(dir, "work", "B01-empty-feed.md")
	before, err := os.ReadFile(body) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "draft", "B1", "--headless"); err != nil {
		t.Fatalf("draft: %v", err)
	}

	after, err := os.ReadFile(body) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("draft rewrote the body; the agent writes it, drover does not")
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.jsonl")); err == nil {
		t.Error("draft appended to the ledger; drafting is not recorded work")
	}

	logDir := filepath.Join(logs, "drover", "example")
	found, err := runlog.Find(logDir, runlog.DraftBase("B01", "empty-feed"))
	if err != nil || len(found) != 1 {
		t.Fatalf("draft log: %d found, err %v", len(found), err)
	}
	// A draft is a session about the item, not a run of it. Sharing the run's
	// base would make `report` read the drafter's report as the run's.
	if runs, _ := runlog.Find(logDir, runlog.Base("B01", "empty-feed")); len(runs) != 0 {
		t.Errorf("the draft log is visible to the run base: %v", runs)
	}
}

// The item's frontmatter names who implements it. An item with no requirements
// yet has nothing to say about who should write them, so the drafting runner
// is a property of the project and of the flag, never of the body.
func TestDraftRunnerNeverInheritsTheItemAgent(t *testing.T) {
	t.Cleanup(func() { draftRunner, runRunner = "", "" })
	it := backlog.Item{Meta: map[string]string{"agent": "codex"}}
	s := &session{cfg: &config.Config{Run: config.Run{RunnerKey: "agent", DefaultRunner: "claude"}}}

	if got := s.runnerName(it); got != "codex" {
		t.Fatalf("run resolves the item's agent to %q, want codex — the fixture proves nothing otherwise", got)
	}
	if got := s.draftRunnerName(); got != "" {
		t.Errorf("draft resolved runner %q from the item; an empty name is what makes it print", got)
	}

	s.cfg.Run.DraftRunner = "gemini"
	if got := s.draftRunnerName(); got != "gemini" {
		t.Errorf("[run] draft_runner = %q, want gemini", got)
	}
	draftRunner = "claude"
	if got := s.draftRunnerName(); got != "claude" {
		t.Errorf("--runner = %q, want it to override the project default", got)
	}
}

// "Nobody could ask" and "the issue said nothing" are different answers, and a
// draft written from the second when the first is true is written from a gap
// the author never sees.
func TestDraftBriefNamesAnUnreadableIssue(t *testing.T) {
	out, err := prompts.Render("draft", prompts.DraftTemplate(), prompts.Data{
		ID: "B1", Title: "Empty feed", Path: "work/B01-empty-feed.md",
		Body:   "STOP: requirements not yet written",
		Origin: prompts.Origin{Number: 12, Unavailable: "gh is not on PATH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(#12) could not be read", "gh is not on PATH", "quoted copy"} {
		if !strings.Contains(out, want) {
			t.Errorf("the prompt omits %q:\n%s", want, out)
		}
	}
}
