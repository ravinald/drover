package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// reviewedThenMoved is item B1's branch with a passing review recorded on its
// first commit, then whatever after does to the branch, merged into main the
// way a pull request merges. It returns the reviewed head and the merged one.
func reviewedThenMoved(t *testing.T, conf string, after func(dir, branch string)) (dir, reviewed, merged string) {
	t.Helper()
	dir = registerProject(t, conf)
	write(t, filepath.Join(dir, "work", "B01-merged.md"), body("Merged owner", ""))
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "the work")
	reviewed = git(t, dir, "rev-parse", "HEAD")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "B1", Pass: passPassed, Head: reviewed, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	after(dir, branch)
	merged = git(t, dir, "rev-parse", "HEAD")
	recordedHead(t, dir, "B1", branch)
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
	return dir, reviewed, merged
}

func handFix(t *testing.T) func(dir, branch string) {
	return func(dir, _ string) {
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix the retry bound by hand")
	}
}

// What a drive does to a branch after its review on purpose: the trunk merged
// in, and the reviewer's own correction.
func trunkMergeAndReviewCommit(t *testing.T) func(dir, branch string) {
	return func(dir, branch string) {
		git(t, dir, "switch", "-q", "main")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "the trunk moves")
		git(t, dir, "switch", "-q", branch)
		git(t, dir, "merge", "-q", "--no-ff", "-m", "merge main into "+branch, "main")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "review: a typo")
	}
}

func finishB1(t *testing.T, dir string) string {
	t.Helper()
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)
	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	return out
}

func completionRow(t *testing.T, dir string) ledger.Row {
	t.Helper()
	for _, r := range openIn(t, dir).ledger.Rows {
		if r.ID == "B1" && r.Merged != nil && *r.Merged {
			return r
		}
	}
	t.Fatal("finish wrote no completion row")
	return ledger.Row{}
}

func unreviewedFindings(t *testing.T, dir string) []Finding {
	t.Helper()
	var out []Finding
	for _, f := range openIn(t, dir).checkReport().Findings {
		if f.Kind == "UNREVIEWED MERGE" {
			out = append(out, f)
		}
	}
	return out
}

// A hand fix after the last passing review merged, and every reader that owes
// the operator a word about it says it: finish on stdout and in the completion
// row, check as a finding, unaudited as a row. An audit naming the merged head
// is what clears it, and a headless audit is not.
func TestAHandCommitAfterThePassIsReportedUntilAnAuditNamesTheMergedHead(t *testing.T) {
	dir, reviewed, merged := reviewedThenMoved(t, fileProject, handFix(t))
	short := git(t, dir, "rev-parse", "--short", merged)

	out := finishB1(t, dir)
	for _, want := range []string{short + " fix the retry bound by hand", "UNREVIEWED MERGE", "drover audit B1 reviewed --head " + merged} {
		if !strings.Contains(out, want) {
			t.Errorf("finish wrote:\n%s\nwant it to name %q", out, want)
		}
	}
	row := completionRow(t, dir)
	if row.MergedHead != merged || row.ReviewedHead != reviewed {
		t.Errorf("completion row records merged %q reviewed %q, want %q and %q", row.MergedHead, row.ReviewedHead, merged, reviewed)
	}
	if len(row.Unreviewed) != 1 || row.Unreviewed[0] != short+" fix the retry bound by hand" {
		t.Errorf("completion row records unreviewed %q, want the hand fix alone", row.Unreviewed)
	}

	fs := unreviewedFindings(t, dir)
	if len(fs) != 1 || !strings.Contains(fs[0].Message, short+" fix the retry bound by hand") ||
		!strings.Contains(fs[0].Detail, "drover review B1") || !strings.Contains(fs[0].Detail, "--head "+merged) {
		t.Fatalf("check reported %+v, want one UNREVIEWED MERGE naming the hand fix and the fix for it", fs)
	}

	// An audit that names no head is somebody's level, not a reading of what merged.
	if err := execute(t, "audit", "B1", "reviewed", "--code-by", "agent", "--reviewer", "sam"); err != nil {
		t.Fatal(err)
	}
	rows, _ := openIn(t, dir).unauditedRows()
	if len(rows) != 1 || rows[0].ID != "B1" || len(rows[0].Unreviewed) != 1 {
		t.Errorf("unaudited lists %+v, want B1 with its unreviewed commit despite the audit", rows)
	}
	if len(unreviewedFindings(t, dir)) != 1 {
		t.Error("a headless audit cleared UNREVIEWED MERGE")
	}

	if err := execute(t, "audit", "B1", "reviewed", "--code-by", "agent", "--reviewer", "sam", "--head", short); err != nil {
		t.Fatal(err)
	}
	if fs := unreviewedFindings(t, dir); len(fs) != 0 {
		t.Errorf("an audit naming the merged head left %+v", fs)
	}
	if rows, _ := openIn(t, dir).unauditedRows(); len(rows) != 0 {
		t.Errorf("unaudited still lists %+v after the audit naming the merged head", rows)
	}
}

// A trunk merge and a review: commit are the trunk's code and the reviewer's
// own, so a branch carrying only those after its pass merged reviewed.
func TestATrunkMergeAndAReviewCommitAfterThePassAreNotUnreviewed(t *testing.T) {
	dir, reviewed, merged := reviewedThenMoved(t, fileProject, trunkMergeAndReviewCommit(t))

	out := finishB1(t, dir)
	if strings.Contains(out, "no review read") {
		t.Errorf("finish reported unreviewed commits:\n%s", out)
	}
	row := completionRow(t, dir)
	if row.MergedHead != merged || row.ReviewedHead != reviewed || len(row.Unreviewed) != 0 {
		t.Errorf("completion row: merged %q reviewed %q unreviewed %q; want %q, %q and none",
			row.MergedHead, row.ReviewedHead, row.Unreviewed, merged, reviewed)
	}
	if fs := unreviewedFindings(t, dir); len(fs) != 0 {
		t.Errorf("check reported %+v", fs)
	}
}

// A completion row from before finish recorded heads carries none, and a pass
// row from before pass rows named one covers no particular head: neither is
// read as a merge somebody failed to review.
func TestRowsWrittenBeforeHeadsReportNothing(t *testing.T) {
	l := &ledger.Ledger{Rows: []ledger.Row{
		{ID: "1", Pass: passPassed, Date: "2026-01-01"},
		{ID: "1", Merged: new(true), Date: "2026-01-02"},
	}}
	if head, ok := reviewedHead(l, "1"); !ok || head != "" {
		t.Errorf("reviewedHead = %q, %v; want a passing review covering no particular head", head, ok)
	}
	if head, commits := l.UnreviewedMerge("1"); head != "" || commits != nil {
		t.Errorf("UnreviewedMerge = %q, %q; want nothing", head, commits)
	}
	s := &session{ledger: l}
	if _, commits, err := s.unreviewedSince("1", "0123456789abcdef"); err != nil || commits != nil {
		t.Errorf("unreviewedSince compared against a headless pass: %q, %v", commits, err)
	}
}

// open-pr is the last point before the merge where the gap can still be
// closed by reviewing, so it refuses the head, naming the commit and the
// review that would cover it; --force opens it and says what it waved past.
func TestOpenPRRefusesAHeadPastTheReviewedOneAndForceOpensIt(t *testing.T) {
	dir, branch, ghLog := openPRReady(t)
	reviewed := git(t, dir, "rev-parse", "HEAD")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "1", Pass: passPassed, Head: reviewed, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix by hand after the review")
	short := git(t, dir, "rev-parse", "--short", "HEAD")

	err := execute(t, "open-pr", "1")
	if Code(err) != ExitRefused {
		t.Fatalf("open-pr exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{branch, short + " fix by hand after the review", "drover review 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if n := prCreates(t, ghLog); len(n) != 0 {
		t.Fatalf("a refused head opened %d pull requests", len(n))
	}

	out := captureErr(t, func() { err = execute(t, "open-pr", "1", "--force") })
	if err != nil {
		t.Fatalf("open-pr --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--force") || !strings.Contains(out, short+" fix by hand after the review") {
		t.Errorf("--force did not say what it opened past:\n%s", out)
	}
	if n := prCreates(t, ghLog); len(n) != 1 {
		t.Errorf("--force opened %d pull requests, want 1", len(n))
	}
}

// The head the review read has to reach the pass row on disk, or nothing
// downstream has a head to compare against. It is read after the review session
// exits, so a review: commit the reviewer made lies inside it.
func TestADrivePassRowNamesTheHeadTheReviewRead(t *testing.T) {
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	driveAll(t)

	var pushed, read string
	for _, r := range openIn(t, dir).ledger.Rows {
		switch {
		case r.ID != "1":
		case r.Branch != "" && r.Head != "" && read == "":
			pushed = r.Head
		case r.Pass == passPassed:
			read = r.Head
		}
	}
	if read == "" {
		t.Fatal("the drive's passing review row names no head")
	}
	// The stub reviewer commits a review: fix, and the pass covers it: the head
	// is read once the session exits, so what lies between is the reviewer's own.
	git(t, dir, "merge-base", "--is-ancestor", pushed, read)
	between := strings.Split(git(t, dir, "log", "--format=%s", pushed+".."+read), "\n")
	if len(between) == 0 || between[0] == "" {
		t.Fatalf("the pass row names %s, the pushed head itself; the stub reviewer's commit is not covered", shortSHA(read))
	}
	for _, subject := range between {
		if !strings.HasPrefix(subject, "review:") {
			t.Errorf("the pass row names %s, past %q, which the review did not write", shortSHA(read), subject)
		}
	}
}

// merge_drive_pr lets a supervisor finish what drive's review read. A head
// holding a hand fix past that is more than the policy covers, so the
// supervisor is refused where the operator's finish records and reports.
func TestASupervisorFinishRefusesAnUnreviewedMerge(t *testing.T) {
	dir, reviewed, _ := reviewedThenMoved(t, noProject+permissiveTable, handFix(t))
	path := filepath.Join(dir, "ledger.jsonl")
	branch := ""
	for _, r := range openIn(t, dir).ledger.Rows {
		if r.Branch != "" {
			branch = r.Branch
		}
	}
	for _, r := range []ledger.Row{
		{ID: "B1", Branch: branch, PR: 7, Drive: "D1", Date: ledger.Today()},
		{ID: "B1", Pass: passPassed, Head: reviewed, Drive: "D1", Date: ledger.Today()},
	} {
		if err := ledger.Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	stubPATH(t)
	t.Chdir(dir)
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)

	err := refuse(t, "finish", "B1")
	if Code(err) != ExitRefused {
		t.Fatalf("finish exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"fix the retry bound by hand", "drover review B1", "merge_drive_pr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if openIn(t, dir).ledger.State("B1").Merged {
		t.Error("a refusal wrote the completion row")
	}
}
