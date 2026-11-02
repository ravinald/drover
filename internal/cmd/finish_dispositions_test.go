package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
)

// stubIssueWriteGH answers the availability probe and records every issue write, so
// a test can assert what reached the tracker rather than that a function
// returned. exit non-zero makes gh fail the way an unreachable one does.
func stubIssueWriteGH(t *testing.T, dir, log string, exit int) {
	t.Helper()
	// One line per call, whatever the argv holds: a comment body carries blank
	// lines, and logging it raw turned a single close into three entries and a
	// count assertion that was measuring newlines.
	t.Setenv("DROVER_TEST_GH_LOG", log)
	t.Setenv("DROVER_TEST_GH_EXIT", stubExit(exit))
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n" +
		"printf '%s' \"$*\" | tr '\\n' ' ' >> \"$DROVER_TEST_GH_LOG\"\n" +
		"printf '\\n' >> \"$DROVER_TEST_GH_LOG\"\n" +
		"exit \"$DROVER_TEST_GH_EXIT\"\n"
	stub(t, filepath.Join(dir, "gh"), script)
}

// ghPATH puts the stub directory ahead of the caller's PATH rather than
// replacing it. applyDispositions runs no git, so the only thing this has to
// decide is which gh answers, and shadowing beats truncating: a truncated PATH
// leaves a package-manager git with no interpreter to find, which is a failure
// in a different command than the one under test.
func ghPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func stubExit(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

func ghCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log) // #nosec G304 -- written by the stub above
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// applyDispositions is the only place drover closes anything, and each of the
// three answers it can give has to be visible on the tracker: satisfied closes,
// partial comments, and an issue nobody judged is named and left alone.
//
// The undisposed case is the whole reason this path exists. Eleven issues named
// in a pull request body and one of them closed is the failure it was built for,
// so a run that silently acted on ten and guessed the eleventh would rebuild it.
func TestApplyDispositionsWritesWhatWasRecorded(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 0)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	for _, row := range []ledger.Row{
		{ID: "B1", Issue: 47, Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true,
			Note: "traced to the handler", Date: ledger.Today()},
		{ID: "B1", Issue: 100, Disposition: ledger.DispositionPartial, CodeBy: ledger.CodeAgent, Independent: true,
			Note: "the migration is B2", Date: ledger.Today()},
	} {
		if err := ledger.Append(path, row); err != nil {
			t.Fatal(err)
		}
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	s.applyDispositions(it, "PR #203")

	calls := ghCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("gh calls: %v, want one close and one comment", calls)
	}
	if !strings.HasPrefix(calls[0], "issue close 47 ") {
		t.Errorf("#47 was not closed: %q", calls[0])
	}
	if !strings.Contains(calls[0], "PR #203") || !strings.Contains(calls[0], "traced to the handler") {
		t.Errorf("the close comment carries neither the merge nor the note: %q", calls[0])
	}
	if !strings.HasPrefix(calls[1], "issue comment 100 ") {
		t.Errorf("#100 was not commented: %q", calls[1])
	}
	// #103 has no disposition. Nothing may reach it, and nothing may invent one.
	for _, call := range calls {
		if strings.Contains(call, " 103 ") {
			t.Errorf("an issue nobody judged reached the tracker: %q", call)
		}
	}

	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Resolution("B1", 47); r.Applied != ledger.AppliedClosed {
		t.Errorf("#47 applied %q, want %q", r.Applied, ledger.AppliedClosed)
	}
	if r, _ := l.Resolution("B1", 100); r.Applied != ledger.AppliedCommented {
		t.Errorf("#100 applied %q, want %q", r.Applied, ledger.AppliedCommented)
	}
	if r, _ := l.Resolution("B1", 103); r.Applied != "" {
		t.Errorf("#103 recorded %q against an issue nothing was written to", r.Applied)
	}
}

// Finishing twice must not comment twice. The applied row is what makes it
// idempotent, and an operator re-running finish after fixing something else is
// ordinary rather than exceptional.
func TestApplyDispositionsSkipsWhatItAlreadyWrote(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 0)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "B1", Issue: 47,
		Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		s, err := open()
		if err != nil {
			t.Fatal(err)
		}
		it, err := s.item("B1")
		if err != nil {
			t.Fatal(err)
		}
		s.applyDispositions(it, "PR #203")
		if got := len(ghCalls(t, log)); got != 1 {
			t.Fatalf("pass %d: %d gh calls, want 1", pass+1, got)
		}
	}
}

// A drive's finish and an operator's can apply one item from two processes, each
// having read the ledger before the other wrote. Both sessions are opened before
// either applies to stand for that: the race is a stale in-memory read, so no
// goroutine is needed to reach it. The second case has b reload at recordDone,
// after a's done row and before a's tracker writes.
func TestTwoFinishesApplyEachIssueOnce(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "opened together"
		if reload {
			name = "reloaded at recordDone"
		}
		t.Run(name, func(t *testing.T) {
			dir := closesProject(t)
			bin := ghPATH(t)
			log := filepath.Join(t.TempDir(), "gh.log")
			stubIssueWriteGH(t, bin, log, 0)
			t.Chdir(dir)

			path := filepath.Join(dir, "ledger.jsonl")
			for _, row := range []ledger.Row{
				{ID: "B1", Issue: 100, Disposition: ledger.DispositionPartial, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()},
				{ID: "B1", Issue: 47, Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()},
			} {
				if err := ledger.Append(path, row); err != nil {
					t.Fatal(err)
				}
			}

			a, b := openIn(t, dir), openIn(t, dir)
			if reload {
				yes := true
				done := ledger.Row{ID: "B1", Merged: &yes, Date: ledger.Today()}
				for _, s := range []*session{a, b} {
					if _, err := s.recordDone(done); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, s := range []*session{a, b} {
				it, err := s.item("B1")
				if err != nil {
					t.Fatal(err)
				}
				s.applyDispositions(it, "PR #203")
			}

			if calls := ghCalls(t, log); len(calls) != 2 {
				t.Errorf("%d gh calls, want 2:\n%s", len(calls), strings.Join(calls, "\n"))
			}
			l, err := ledger.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			for issue, n := range appliedRows(l, "B1") {
				if n != 1 {
					t.Errorf("#%d has %d applied rows, want 1", issue, n)
				}
			}
			if got := len(appliedRows(l, "B1")); got != 2 {
				t.Errorf("applied rows for %d issues, want 2", got)
			}
		})
	}
}

// A claim another process holds is a call in flight, and the only safe reading
// of it is that the comment may already be on its way.
func TestApplyDispositionsLeavesALockedIssueAlone(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 0)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	for _, row := range []ledger.Row{
		{ID: "B1", Issue: 100, Disposition: ledger.DispositionPartial, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()},
		{ID: "B1", Issue: 100, Locking: time.Now().UTC().Format(time.RFC3339), Date: ledger.Today()},
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
	out := capture(t, func() { s.applyDispositions(it, "PR #203") })
	if calls := ghCalls(t, log); len(calls) != 0 {
		t.Errorf("wrote to an issue another finish holds: %v", calls)
	}
	if !strings.Contains(out, "#100: already being applied") {
		t.Errorf("no word on the claim it found:\n%s", out)
	}
	// Pending, not applied: --no-tracker still names it.
	if out := capture(t, func() { s.reportDispositions(it) }); !strings.Contains(out, "#100") {
		t.Errorf("--no-tracker dropped a claimed issue nothing has written:\n%s", out)
	}
}

func appliedRows(l *ledger.Ledger, id string) map[int]int {
	out := map[int]int{}
	for _, r := range l.Rows {
		if r.ID == id && r.Applied != "" {
			out[r.Issue]++
		}
	}
	return out
}

// A tracker nobody can reach must not fail the finish. The merge happened and
// the ledger says so; the operator who has to run finish again over a network
// blip is the failure this degrade prevents.
func TestApplyDispositionsSurvivesAnUnreachableTracker(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 1)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "B1", Issue: 47,
		Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	s.applyDispositions(it, "PR #203")

	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// No applied row: the next finish has to try again, and a row written on a
	// failed call would tell it the close is already done.
	if r, _ := l.Resolution("B1", 47); r.Applied != "" {
		t.Errorf("recorded %q for a close the tracker refused", r.Applied)
	}
	// Released, too, or the next finish would find the issue held for the lease.
	if r, _ := l.Resolution("B1", 47); !r.Lockable(time.Now()) {
		t.Error("a refused close left the issue claimed")
	}
}

// --no-tracker is what a drive finishes under. Closing an issue is visible to
// whoever filed it, so it belongs to a command an operator typed for that
// purpose rather than to a step a long-running command takes on the way past.
// Asserted on the gh log, because the reporting path and the writing path share
// every line up to the call and a test on the printed output cannot tell them
// apart.
func TestNoTrackerWritesNothingAndSaysWhatItLeft(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 0)
	t.Chdir(dir)

	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Row{
		ID: "B1", Issue: 47, Disposition: ledger.DispositionSatisfied,
		CodeBy: ledger.CodeAgent, Independent: true, Note: "traced to the handler", Date: ledger.Today(),
	}); err != nil {
		t.Fatal(err)
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() { s.reportDispositions(it) })

	if calls := ghCalls(t, log); len(calls) != 0 {
		t.Errorf("--no-tracker reached the tracker: %v", calls)
	}
	if !strings.Contains(out, "#47") {
		t.Errorf("the issue it left open is not named: %q", out)
	}
	if !strings.Contains(out, "drover finish B1") {
		t.Errorf("nothing says how to close it later: %q", out)
	}
}

// A partial recorded before an amend was judged against requirements that have
// since changed, and it leaves the issue open. finish names it before writing
// anything to the tracker, and goes on to write what was recorded.
func TestFinishWarnsOnAPartialOlderThanTheAmend(t *testing.T) {
	dir := closesProject(t)
	bin := ghPATH(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stubIssueWriteGH(t, bin, log, 0)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	for _, row := range []ledger.Row{
		{ID: "B1", Issue: 47, Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-10-28"},
		{ID: "B1", Issue: 100, Disposition: ledger.DispositionPartial, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-10-28"},
		{ID: "B1", Amend: &ledger.Amendment{From: 1, To: 1}, Date: "2026-10-29"},
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
	out := capture(t, func() { s.applyDispositions(it, "PR #203") })
	warn := strings.Index(out, "#100: partial was recorded before the amend on 2026-10-29")
	if warn < 0 {
		t.Fatalf("no warning for the outdated partial:\n%s", out)
	}
	if !strings.Contains(out, "drover resolve B1 100 <") {
		t.Errorf("warning does not name the resolve command:\n%s", out)
	}
	if strings.Contains(out, "#47: partial") {
		t.Errorf("warned on a satisfied disposition:\n%s", out)
	}
	if first := strings.Index(out, "✓"); first >= 0 && first < warn {
		t.Errorf("warned after writing to the tracker:\n%s", out)
	}
	if calls := ghCalls(t, log); len(calls) != 2 {
		t.Errorf("gh calls: %v, want the close and the comment still written", calls)
	}
}
