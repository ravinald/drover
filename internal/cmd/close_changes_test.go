package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

// blockStanding is one accepted item whose body carries what a review sent back,
// which is the only state clear acts on. `request-changes` writes it, so the fixture is
// the two commands in the order a review round runs them rather than a body
// hand-written into the shape the command wants.
func blockStanding(t *testing.T) (dir, body, evidence string) {
	t.Helper()
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit, clearFile = "", false, "" })
	dir, evidence = capProject(t, 0)
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatalf("request for changes: %v", err)
	}
	return dir, filepath.Join(dir, "work", "01-first.md"), evidence
}

func ledgerRows(t *testing.T, dir string) int {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Rows)
}

func kept(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "reports"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// The whole of the command: the section says the block is closed, and the
// ledger gains the body's digest and no changes row. A changes row here would make
// clearing a discharged block cost an `open-pr` refusal and a round against the
// cap.
func TestCloseChangesWritesTheSectionAndNoChangesRow(t *testing.T) {
	dir, body, _ := blockStanding(t)
	rows, rounds := ledgerRows(t, dir), kept(t, dir)

	clearFile = ""
	if err := execute(t, "close-changes", "1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := read(t, body)
	if strings.Contains(got, "over-excludes") {
		t.Errorf("the closed finding survived the clear:\n%s", got)
	}
	if !strings.Contains(got, "Nothing is outstanding.") {
		t.Errorf("the block does not say it is closed:\n%s", got)
	}
	if n := strings.Count(got, prompts.ChangesHeading); n != 1 {
		t.Errorf("%d copies of the heading, want 1:\n%s", n, got)
	}
	if n := len(backlog.Item{Body: got}.Requirements()); n != 1 {
		t.Errorf("the contract moved: %d requirements, want 1\n%s", n, got)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(l.Rows); n != rows+1 {
		t.Fatalf("%d ledger rows after the clear, want %d", n, rows+1)
	}
	if last := l.Rows[rows]; last.Changes != "" || last.Body != bodySum([]byte(got)) {
		t.Errorf("the clear's row is not the body's digest alone: %+v", last)
	}
	if n := kept(t, dir); n != rounds {
		t.Errorf("%d kept rounds after the clear, want %d: a clear is not a round", n, rounds)
	}
}

// A first pass has nothing to discharge, and writing the statement anyway would
// link a review read findings nobody wrote.
func TestClearRefusesWhenNothingHasBeenSentBack(t *testing.T) {
	t.Cleanup(func() { clearFile = "" })
	dir, _ := capProject(t, 0)
	body := filepath.Join(dir, "work", "01-first.md")
	before := read(t, body)

	clearFile = ""
	err := execute(t, "close-changes", "1")
	if err == nil {
		t.Fatal("cleared a block nobody wrote")
	}
	if code := Code(err); code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if got := read(t, body); got != before {
		t.Error("the refusal rewrote the body")
	}
	for _, want := range []string{prompts.ChangesHeading, "drover audit 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// One disposition per pass. A block this review wrote minutes ago is current
// whatever a later command says about it, and the ledger's changes row inside
// the window is the only thing that can tell that block from an earlier one.
func TestCloseChangesRefusesWhileThisPassesOwnChangesRowStands(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, clearFile = "", "" })
	dir, evidence := capProject(t, 0)
	body := filepath.Join(dir, "work", "01-first.md")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-21"}); err != nil {
		t.Fatal(err)
	}
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatalf("request for changes: %v", err)
	}
	before := read(t, body)

	clearFile = ""
	err := execute(t, "close-changes", "1")
	if err == nil {
		t.Fatal("a review cleared the block it had just written")
	}
	if code := Code(err); code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if got := read(t, body); got != before {
		t.Error("the refusal rewrote the body")
	}
	if !strings.Contains(err.Error(), "sent work back") {
		t.Errorf("the refusal does not name what stands against it:\n%v", err)
	}
}

// The gap this command closes, driven end to end. A review whose findings the
// branch closed has to clear the block, and saying so with request-changes refuses the
// branch it just cleared: open-pr reads any changes row inside the window after
// the latest review stage row as work still outstanding.
func TestAClearedBlockLeavesTheBranchProposable(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, clearFile, openPRForce = "", "", false })
	dir, _, evidence := blockStanding(t)
	path := filepath.Join(dir, "ledger.jsonl")
	for _, row := range []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-20"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-21"},
		{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-21"},
	} {
		if err := ledger.Append(path, row); err != nil {
			t.Fatal(err)
		}
	}

	clearFile = ""
	if err := execute(t, "close-changes", "1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	captureErr(t, func() {
		if err := s.reviewPassed(it); err != nil {
			t.Fatalf("a cleared block refused the branch it cleared: %v", err)
		}
	})

	// The counterfactual, and the reason the command exists: the same sentence
	// written with `request-changes` parks the branch.
	write(t, evidence, "Nothing came back: every finding above is closed.\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatalf("request for changes: %v", err)
	}
	s = openIn(t, dir)
	if it, err = s.item("1"); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewPassed(it); err == nil {
		t.Error("a request for changes inside the review window opened a pull request")
	}
}

// Findings a reviewer confirmed closed are written the way findings get
// written, and a top-level "N." anywhere in the body is read as a requirement.
func TestClearedEvidenceCannotEnterTheContract(t *testing.T) {
	dir, body, _ := blockStanding(t)
	confirmed := filepath.Join(dir, "confirmed.md")
	write(t, confirmed, "1. A closed at walk.go:120.\n2. B closed at walk.go:200.\n")

	clearFile = ""
	if err := execute(t, "close-changes", "1", "-f", confirmed); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got := read(t, body)
	if n := len(backlog.Item{Body: got}.Requirements()); n != 1 {
		t.Errorf("the contract grew to %d requirements:\n%s", n, got)
	}
	for _, want := range []string{"Nothing is outstanding.", "- A closed at walk.go:120.", "- B closed at walk.go:200."} {
		if !strings.Contains(got, want) {
			t.Errorf("the cleared block lost %q:\n%s", want, got)
		}
	}
}

// A command nothing routes an agent to is one no review runs. The brief carries
// it only where a block stands: a first pass told to clear one would go looking
// for a section nobody wrote.
func TestTheReviewBriefNamesTheCommandThatClearsABlock(t *testing.T) {
	standing := prompts.Data{
		Project: "example", ID: "1", Title: "First", VerifyCommand: "make test",
		Path: "work/01-first.md",
		Body: "1. Do it.\n\n" + prompts.ChangesHeading + "\n\nThe walk over-excludes on a nullable column.",
	}
	out := renderBrief(t, "review", prompts.ReviewTemplate(), standing)
	for _, want := range []string{"drover close-changes 1", "writes no changes row"} {
		if !strings.Contains(out, want) {
			t.Errorf("the prompt does not say %q", want)
		}
	}

	none := standing
	none.Body = "1. Do it."
	if out := renderBrief(t, "review", prompts.ReviewTemplate(), none); strings.Contains(out, "drover close-changes") {
		t.Error("the prompt tells a first pass to clear a block nobody wrote")
	}
}

// Both briefs read the block, so both have to tell one a review discharged from
// findings a run owes something about. Rendering a cleared block as current
// findings sends the next run to re-verify finished work, which is the state
// clearing exists to end — and tells the next reviewer to clear it again.
func TestTheBriefsTellAClearedBlockFromFindings(t *testing.T) {
	d := prompts.Data{
		Project: "example", ID: "1", Title: "First", VerifyCommand: "make test",
		Path: "work/01-first.md",
		Body: "1. Do it.\n\n" + prompts.ChangesHeading + "\n\n" + prompts.ClearedMark +
			" The review of 2026-09-21 read the block this replaces\nand found every finding in it closed against the tree.",
	}
	for _, c := range []struct{ name, text string }{
		{"run", prompts.DefaultTemplate()},
		{"review", prompts.ReviewTemplate()},
	} {
		out := renderBrief(t, c.name, c.text, d)
		if !strings.Contains(out, "cleared") {
			t.Errorf("%s prompt does not say the block was cleared:\n%s", c.name, out)
		}
		for _, wrong := range []string{"A review sent this back", "What the last pass sent back", "drover close-changes 1"} {
			if strings.Contains(out, wrong) {
				t.Errorf("%s prompt reads a cleared block as current findings: %q", c.name, wrong)
			}
		}
	}
}

// The round a clear discharged stays visible. `close-changes` replaces that round's
// evidence with the statement that nothing is outstanding, so the kept file is
// the only place the prose survives: a brief that still drops the newest round
// as duplicated hands the next pass a cleared body and no record of what the
// clear claims somebody read against the tree. Both briefs, because both list
// the trail and either one alone leaves the other reader guessing.
func TestBothBriefsKeepTheRoundAClearDischarged(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, clearFile = "", "" })
	dir, _, evidence := blockStanding(t)
	const (
		first  = "The walk over-excludes on a nullable column."
		second = "The handle closes before the rename."
	)
	write(t, evidence, second+"\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatalf("second request for changes: %v", err)
	}
	newest := filepath.Join("reports", "01-first.changes-2.md")

	briefs := []struct{ name, text string }{
		{"run", prompts.DefaultTemplate()},
		{"review", prompts.ReviewTemplate()},
	}
	data := func() prompts.Data {
		t.Helper()
		s := openIn(t, dir)
		it, err := s.item("1")
		if err != nil {
			t.Fatal(err)
		}
		return s.localData(it)
	}

	// Standing, the newest round is the block itself, and listing it beside the
	// block hands the current findings over twice.
	d := data()
	if len(d.Trail) != 2 {
		t.Fatalf("%d kept rounds, want 2: %+v", len(d.Trail), d.Trail)
	}
	for _, c := range briefs {
		out := renderBrief(t, c.name, c.text, d)
		if strings.Contains(out, newest) {
			t.Errorf("%s prompt lists the standing round as history:\n%s", c.name, out)
		}
		if !strings.Contains(out, first) {
			t.Errorf("%s prompt lost the earlier round:\n%s", c.name, out)
		}
	}

	clearFile = ""
	if err := execute(t, "close-changes", "1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	d = data()
	if strings.Contains(d.Body, second) {
		t.Fatalf("the clear left the discharged findings in the body:\n%s", d.Body)
	}
	if n := len(d.Earlier()); n != len(d.Trail) {
		t.Errorf("cleared prompt exposes %d prior rounds, want %d; trail has %d",
			n, len(d.Trail), len(d.Trail))
	}
	for _, c := range briefs {
		out := renderBrief(t, c.name, c.text, d)
		for _, want := range []string{first, second, newest} {
			if !strings.Contains(out, want) {
				t.Errorf("%s prompt lost %q after the clear:\n%s", c.name, want, out)
			}
		}
	}
}
