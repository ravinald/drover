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
	if head, ok := reviewedHead(l, "1", nil); !ok || head != "" {
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

func TestB113ReviewSideBranchMerge(t *testing.T) {
	var side string
	dir, _, _ := reviewedThenMoved(t, fileProject, func(dir, branch string) {
		git(t, dir, "switch", "-q", "-c", "side-fix")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "unreviewed side fix")
		side = git(t, dir, "rev-parse", "--short", "HEAD")
		git(t, dir, "switch", "-q", branch)
		git(t, dir, "merge", "-q", "--no-ff", "-m", "merge side fix", "side-fix")
	})
	out := finishB1(t, dir)
	if !strings.Contains(out, side+" unreviewed side fix") || len(unreviewedFindings(t, dir)) != 1 {
		t.Fatalf("non-trunk merge disappeared: findings=%v output=%s", unreviewedFindings(t, dir), out)
	}
}

func TestB113ReviewRejectedAuditIsNotPass(t *testing.T) {
	dir, reviewed, merged := reviewedThenMoved(t, fileProject, handFix(t))
	path := filepath.Join(dir, "ledger.jsonl")
	for _, row := range []ledger.Row{
		{ID: "B1", Stage: ledger.StageReview, Date: ledger.Today()},
		{ID: "B1", Changes: "still broken", Date: ledger.Today()},
		{ID: "B1", Level: ledger.LevelVerified, Head: merged, Independent: true, Date: ledger.Today()},
		{ID: "B1", Pass: passChangesRequested, Head: merged, Date: ledger.Today()},
	} {
		if err := ledger.Append(path, row); err != nil {
			t.Fatal(err)
		}
	}
	finishB1(t, dir)
	row := completionRow(t, dir)
	if row.ReviewedHead != reviewed || len(row.Unreviewed) != 1 {
		t.Fatalf("rejected review became passing baseline: reviewed=%s want=%s unreviewed=%v", row.ReviewedHead, reviewed, row.Unreviewed)
	}
}

func TestB113ReviewFinishAfterMarkMerged(t *testing.T) {
	dir, reviewed, merged := reviewedThenMoved(t, fileProject, handFix(t))
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Row{ID: "B1", Merged: new(true), Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	finishB1(t, dir)
	l := openIn(t, dir).ledger
	var found bool
	for _, row := range l.Rows {
		if row.ID == "B1" && row.MergedHead == merged && row.ReviewedHead == reviewed {
			found = true
		}
	}
	if !found || len(unreviewedFindings(t, dir)) != 1 {
		t.Fatalf("finish left prior completion without evidence: heads recorded=%v findings=%v", found, unreviewedFindings(t, dir))
	}
}

func TestB113ReviewSupervisorPreflight(t *testing.T) {
	dir, reviewed, _ := reviewedThenMoved(t, noProject+permissiveTable, handFix(t))
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	// Restore the pre-merge trunk; the branch still holds the hand fix.
	git(t, dir, "reset", "--hard", "HEAD^")
	for _, row := range []ledger.Row{
		{ID: "B1", Branch: branch, PR: 7, Drive: "D1", Date: ledger.Today()},
		{ID: "B1", Pass: passPassed, Head: reviewed, Drive: "D1", Date: ledger.Today()},
	} {
		if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), row); err != nil {
			t.Fatal(err)
		}
	}
	s = openIn(t, dir)
	err = s.supervisorMayFinish("B1", branch)
	if err == nil || !strings.Contains(err.Error(), "fix the retry bound by hand") || !strings.Contains(err.Error(), "drover review B1") {
		t.Fatalf("supervisor eligibility before merge = %v; want refusal naming hand fix and drover review B1", err)
	}
}

// supervisorBeforeMerge is B1 reviewed at its first commit, after moved the
// branch, and the trunk put back to before the pull request merged, with the
// ledger rows drive writes for a pull request it opened and a pass it read.
// gh answers for pull request 7 from the branch head, merges it with git when
// asked, and logs every call to the file it returns.
func supervisorBeforeMerge(t *testing.T, after func(dir, branch string)) (dir, reviewed, log string) {
	t.Helper()
	dir, reviewed, head := reviewedThenMoved(t, noProject+permissiveTable, after)
	git(t, dir, "reset", "-q", "--hard", "HEAD^")
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
		if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), r); err != nil {
			t.Fatal(err)
		}
	}
	bin := stubPATH(t)
	log = filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("DROVER_TEST_GH_LOG", log)
	t.Setenv("DROVER_TEST_GH_HEAD", head)
	t.Setenv("DROVER_TEST_GH_MERGED", filepath.Join(t.TempDir(), "merged"))
	stub(t, filepath.Join(bin, "gh"), `#!/bin/sh
echo "$*" >> "$DROVER_TEST_GH_LOG"
case "$1 $2" in
'auth status') exit 0 ;;
'pr list')
	case "$*" in
	*open*) [ -f "$DROVER_TEST_GH_MERGED" ] && printf '[]' || printf '[{"number":7,"headRefOid":"%s"}]' "$DROVER_TEST_GH_HEAD" ;;
	*) [ -f "$DROVER_TEST_GH_MERGED" ] && printf '[{"number":7,"mergedAt":"2026-10-30T00:00:00Z","headRefOid":"%s"}]' "$DROVER_TEST_GH_HEAD" || printf '[]' ;;
	esac ;;
'pr merge') for a; do last=$a; done; git merge -q --no-ff -m "merge $last" "$last" && : > "$DROVER_TEST_GH_MERGED" ;;
*) echo "stub gh: unexpected $*" >&2; exit 1 ;;
esac
`)
	t.Chdir(dir)
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	return dir, reviewed, log
}

func ghMerges(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var merges []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "pr merge") {
			merges = append(merges, line)
		}
	}
	return merges
}

// A supervisor's finish is what merges under merge_drive_pr, so a pull request
// holding a hand fix past the reviewed head is refused before gh is asked to
// merge it, and one holding only what the review read is merged bound to that
// head and recorded.
func TestASupervisorFinishRefusesAnUnreviewedHeadBeforeMerging(t *testing.T) {
	dir, _, log := supervisorBeforeMerge(t, handFix(t))
	err := refuse(t, "finish", "B1")
	if Code(err) != ExitRefused {
		t.Fatalf("finish exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"fix the retry bound by hand", "drover review B1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if m := ghMerges(t, log); len(m) != 0 {
		t.Errorf("a refused head was merged: %q", m)
	}
	if openIn(t, dir).ledger.State("B1").Merged {
		t.Error("a refusal wrote the completion row")
	}
}

func TestASupervisorFinishMergesAReviewedHeadBoundToIt(t *testing.T) {
	dir, reviewed, log := supervisorBeforeMerge(t, func(string, string) {})
	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	m := ghMerges(t, log)
	if len(m) != 1 || !strings.Contains(m[0], "--match-head-commit "+reviewed) {
		t.Errorf("gh merges %q, want one bound to %s", m, reviewed)
	}
	if !openIn(t, dir).ledger.State("B1").Merged {
		t.Errorf("finish recorded no completion:\n%s", out)
	}
}

// The finding and its remedy are documented where an operator meets them: the
// check help that names the finding, and the lifecycle section that explains
// it. A kind list that merely names UNREVIEWED MERGE would pass with neither.
func TestUnreviewedMergeIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "lifecycle.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "### Commits merged after the last review")
	if !ok {
		t.Fatal("docs/lifecycle.md has no section on commits merged after the last review")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	var entry []string
	for _, line := range strings.Split(checkCmd.Long, "\n") {
		if strings.HasPrefix(line, "  UNREVIEWED MERGE") || len(entry) > 0 && strings.HasPrefix(line, "    ") {
			entry = append(entry, line)
		} else if len(entry) > 0 {
			break
		}
	}
	help := strings.Join(strings.Fields(strings.Join(entry, " ")), " ")
	for surface, text := range map[string]string{"docs/lifecycle.md": section, "drover check --help": help} {
		for _, want := range []string{"UNREVIEWED MERGE", "drover review", "--head", "review:"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not describe UNREVIEWED MERGE with %q", surface, want)
			}
		}
	}
}

func TestB113ManualOpenFindingsDoNotAdvanceReview(t *testing.T) {
	dir, reviewed, merged := reviewedThenMoved(t, fileProject, handFix(t))
	path := filepath.Join(dir, "ledger.jsonl")
	write(t, filepath.Join(dir, "work", "B01-merged.md"), body("Merged owner", "")+"\n\n## What the review sent back\n\nR1 remains broken.\n")
	for _, row := range []ledger.Row{
		{ID: "B1", Changes: "R1 remains broken", Date: ledger.Today()},
		{ID: "B1", Stage: ledger.StageReview, Date: ledger.Today()},
		{ID: "B1", Level: ledger.LevelVerified, Head: merged, Independent: true, Date: ledger.Today()},
	} {
		if err := ledger.Append(path, row); err != nil {
			t.Fatal(err)
		}
	}
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.blockStands(it) {
		t.Fatal("fixture has no standing findings")
	}
	if err := s.reviewPassed(it); err == nil {
		t.Fatal("manual review unexpectedly passed")
	}
	finishB1(t, dir)
	row := completionRow(t, dir)
	if row.ReviewedHead != reviewed || len(row.Unreviewed) != 1 {
		t.Fatalf("non-passing manual review erased gap: reviewed=%s want=%s unreviewed=%v", row.ReviewedHead, reviewed, row.Unreviewed)
	}
}

func TestB113NonTrunkMergeOwnChangesAreReported(t *testing.T) {
	var merge string
	dir, _, _ := reviewedThenMoved(t, fileProject, func(dir, branch string) {
		git(t, dir, "switch", "-q", "-c", "side-review")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "review: side typo")
		git(t, dir, "switch", "-q", branch)
		git(t, dir, "merge", "-q", "--no-ff", "--no-commit", "side-review")
		write(t, filepath.Join(dir, "unreviewed.txt"), "new behavior introduced during the merge\n")
		git(t, dir, "add", "unreviewed.txt")
		git(t, dir, "commit", "-q", "-m", "unreviewed merge fix")
		merge = git(t, dir, "rev-parse", "--short", "HEAD")
	})
	out := finishB1(t, dir)
	if !strings.Contains(out, merge+" unreviewed merge fix") || len(unreviewedFindings(t, dir)) != 1 {
		t.Fatalf("non-trunk merge's own change disappeared: findings=%v output=%s", unreviewedFindings(t, dir), out)
	}
}
