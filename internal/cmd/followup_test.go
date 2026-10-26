package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// finishedResult is the terminal event of a headless session that finished,
// so a pass reads from the rows it wrote rather than as one that died.
const finishedResult = `{"event":"result","result":{"status":"SUCCESS","response":"reviewed it","num_turns":9}}`

// reviewProject is an item with a reviewer whose session writes the given
// ledger rows, prints result as its stream and exits with the given status,
// the way a real session's own `drover request-changes` and `drover audit` land rows in
// a child process. The script uses shell builtins alone because stubPATH
// narrows PATH to git.
func reviewProject(t *testing.T, exit int, result string, rows ...string) (s *session) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s = fixture(t, "\n[runners.rev]\ninteractive = [\"stub-rev\"]\nheadless    = [\"stub-rev\"]\n")
	git(t, s.cfg.Root, "init", "-q", "-b", "main")
	git(t, s.cfg.Root, "commit", "-q", "--allow-empty", "-m", "root")
	t.Setenv("DROVER_TEST_LEDGER", s.cfg.Abs(s.cfg.Paths.Ledger))
	script := "#!/bin/sh\n"
	if len(rows) > 0 {
		t.Setenv("DROVER_TEST_REV_ROWS", strings.Join(rows, "\n"))
		script += "printf '%s\\n' \"$DROVER_TEST_REV_ROWS\" >> \"$DROVER_TEST_LEDGER\"\n"
	}
	if result != "" {
		t.Setenv("DROVER_TEST_REV_RESULT", result)
		script += "printf '%s\\n' \"$DROVER_TEST_REV_RESULT\"\n"
	}
	t.Setenv("DROVER_TEST_STUB_EXIT", strconv.Itoa(exit))
	script += "exit \"$DROVER_TEST_STUB_EXIT\"\n"
	stub(t, filepath.Join(stubPATH(t), "stub-rev"), script)
	t.Chdir(s.cfg.Root)
	return s
}

// The request this exists for: a review ends and the operator reads what
// happened and the one command to run next, without knowing the pass
// vocabulary. Each case is the real command, a real child writing rows, and
// the lines it printed.
func TestReviewEndsOnItsOutcomeAndTheNextStep(t *testing.T) {
	const sentBack = `{"id":"1","changes":"abc123","date":"2026-09-22"}`
	const audit = `{"id":"1","level":"verified","code_by":"agent","independent":true,"date":"2026-09-22"}`
	for _, c := range []struct {
		name          string
		exit          int
		rows          []string
		outcome, next string
		wantErr       bool
	}{
		{"it sent work back", 0, []string{audit, sentBack},
			"the review sent work back", "Next steps: drover run 1, then drover review 1 --runner rev", false},
		{"it recorded an audit and nothing else", 0, []string{audit},
			"recorded an audit and sent nothing back", "Next steps: drover open-pr 1\n", false},
		{"it recorded nothing", 0, nil,
			"recorded nothing: no audit and no request for changes", "Next steps: drover review 1 --runner rev --headless, or record", false},
		{"it exited non-zero and recorded nothing", 1, nil,
			"recorded nothing: no audit and no request for changes", "Next steps: drover review 1 --runner rev --headless", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			reviewProject(t, c.exit, finishedResult, c.rows...)
			var err error
			out := captureErr(t, func() { err = execute(t, "review", "1", "--runner", "rev", "--headless") })
			if (err != nil) != c.wantErr {
				t.Fatalf("review returned %v, want an error: %v", err, c.wantErr)
			}
			if !strings.Contains(out, "Outcome:    ") || !strings.Contains(out, c.outcome) {
				t.Errorf("no outcome naming %q:\n%s", c.outcome, out)
			}
			if !strings.Contains(out, c.next) {
				t.Errorf("no next step %q:\n%s", c.next, out)
			}
		})
	}
}

// A converged item: an audit with no changes row over a body drover last wrote is a
// pass, so the next step is the pull request.
func TestAReviewThatPassesPointsAtOpenPR(t *testing.T) {
	s := reviewProject(t, 0, finishedResult, `{"id":"1","level":"verified","code_by":"agent","independent":true,"date":"2026-09-22"}`)
	if err := os.WriteFile(s.cfg.Abs(s.cfg.Paths.Ledger),
		[]byte(`{"id":"1","changes":"earlier","date":"2026-09-21"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureErr(t, func() {
		if err := execute(t, "review", "1", "--runner", "rev", "--headless"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Next steps: drover open-pr 1\n") {
		t.Errorf("a passing review does not point at propose:\n%s", out)
	}
}

// A review that writes into the body itself, rather than through request-changes, leaves
// the same rows as a clean pass and a body that no longer matches its launch
// digest. The outcome names both digests.
func TestAReviewThatEditsTheBodyByHandReadsEdited(t *testing.T) {
	s := reviewProject(t, 0, finishedResult)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := bodyDigest(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := exec.LookPath("stub-rev")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_ITEM", it.Path)
	stub(t, rev, "#!/bin/sh\n"+
		"printf '%s\\n' '{\"id\":\"1\",\"level\":\"verified\",\"code_by\":\"agent\",\"independent\":true,\"date\":\"2026-10-01\"}' >> \"$DROVER_TEST_LEDGER\"\n"+
		"printf 'R2 unmet: fix it.\\n' >> \"$DROVER_TEST_ITEM\"\n"+
		"printf '%s\\n' '"+finishedResult+"'\n")
	out := captureErr(t, func() {
		if err := execute(t, "review", "1", "--runner", "rev", "--headless"); err != nil {
			t.Fatal(err)
		}
	})
	now, err := bodyDigest(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"changed outside drover", launch + " then", now + " now", "drover request-changes 1 -f <file>", "drover open-pr 1 --force"} {
		if !strings.Contains(out, want) {
			t.Errorf("the edited outcome does not say %q:\n%s", want, out)
		}
	}
}

// reviewWrites is a reviewer that records an audit and runs edit, shell
// builtins only, against the item file before it finishes.
func reviewWrites(t *testing.T, edit string) string {
	t.Helper()
	rev, err := exec.LookPath("stub-rev")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, rev, "#!/bin/sh\n"+
		"printf '%s\\n' '{\"id\":\"1\",\"level\":\"verified\",\"code_by\":\"agent\",\"independent\":true,\"date\":\"2026-10-01\"}' >> \"$DROVER_TEST_LEDGER\"\n"+
		edit+"\n"+
		"printf '%s\\n' '"+finishedResult+"'\n")
	return captureErr(t, func() {
		if err := execute(t, "review", "1", "--runner", "rev", "--headless"); err != nil {
			t.Fatal(err)
		}
	})
}

// A snapshot hook can append a newline to the body mid-review, and a pass that
// found nothing must not park as edited.
func TestANewlineAppendedToTheBodyIsNotAnEdit(t *testing.T) {
	s := reviewProject(t, 0, finishedResult)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_ITEM", it.Path)
	out := reviewWrites(t, "printf '\\n' >> \"$DROVER_TEST_ITEM\"")
	if !strings.Contains(out, "recorded an audit and sent nothing back") || !strings.Contains(out, "Next steps: drover open-pr 1\n") {
		t.Errorf("an appended newline did not read as a pass:\n%s", out)
	}
}

// One word changed is an edit, and the outcome points at clear, which is how a
// closed block is discharged without moving the digest.
func TestAWordChangedInTheBodyReadsEditedAndNamesClear(t *testing.T) {
	s := reviewProject(t, 0, finishedResult)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(string(raw))
	last := words[len(words)-1]
	changed := strings.Replace(string(raw), last, last+"x", 1)
	if changed == string(raw) {
		t.Fatalf("the fixture body has no word to change: %q", raw)
	}
	src := filepath.Join(t.TempDir(), "changed.md")
	if err := os.WriteFile(src, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_ITEM", it.Path)
	t.Setenv("DROVER_TEST_SRC", src)
	out := reviewWrites(t, "while IFS= read -r l; do printf '%s\\n' \"$l\"; done < \"$DROVER_TEST_SRC\" > \"$DROVER_TEST_ITEM\"")
	for _, want := range []string{"changed outside drover", "a block deleted by hand reads the same", "drover close-changes <id>"} {
		if !strings.Contains(out, want) {
			t.Errorf("the edited outcome does not say %q:\n%s", want, out)
		}
	}
}

// A headless session that left no terminal event died, and the next step
// repeats the headless flag so the copied command is the pass it follows.
func TestAHeadlessReviewThatDiedSaysSo(t *testing.T) {
	reviewProject(t, 0, "")
	out := captureErr(t, func() { _ = execute(t, "review", "1", "--runner", "rev", "--headless") })
	if !strings.Contains(out, "ended without finishing") {
		t.Errorf("a session with no terminal event is not reported as died:\n%s", out)
	}
	if !strings.Contains(out, "drover review 1 --runner rev --headless") {
		t.Errorf("the next step drops --headless:\n%s", out)
	}
}

// Every pass token names a next step of its own, and none falls through to the
// generic line: that line exists for a token added later, not for these.
func TestEveryPassTokenHasItsOwnNextStep(t *testing.T) {
	for _, token := range []string{passDied, passFaulted, passUnreadable, passSilent, passCapped,
		passRepeating, passChangesRequested, passChangesUnrecorded, passEdited, passPassed} {
		f := passFollowUp(token, pass{cap: 4}, "7", "work/07-x.md", "rev", "", "drover review 7 --runner rev")
		if f.Outcome == "" || f.Next == "" {
			t.Errorf("%s: empty follow-up %+v", token, f)
		}
		if strings.HasPrefix(f.Outcome, "the review ended as ") {
			t.Errorf("%s falls through to the generic outcome", token)
		}
		if !strings.Contains(f.Next, "7") {
			t.Errorf("%s: the next step does not name the item: %q", token, f.Next)
		}
	}
}

func TestDriveEndsEachItemOnItsNextStep(t *testing.T) {
	for _, c := range []struct {
		name    string
		d       DriveItem
		want    string
		drafted bool
		deny    config.Policy
		absent  string
	}{
		{"merged", DriveItem{ID: "3", Outcome: outcomeReviewed, PR: 12, Merged: true}, "drover finish 3 once the forge has merged it", false, "", ""},
		{"held by --no-merge", DriveItem{ID: "3", Outcome: outcomeReviewed, PR: 12}, "merge PR #12, then drover finish 3", false, "", ""},
		{"no merge gate", DriveItem{ID: "3", Outcome: outcomeReviewed}, "drover open-pr 3 --force", false, "", ""},
		{"scaffold", DriveItem{ID: "3", Outcome: outcomeSkipped, Park: stateScaffold}, "write them in work/03-x.md, then drover accept 3", false, "", ""},
		{"drafted scaffold", DriveItem{ID: "3", Outcome: outcomeSkipped, Park: stateScaffold}, "read them in work/03-x.md, then drover accept 3", true, "", ""},
		{"ran, unnamed", DriveItem{ID: "3", Outcome: outcomeSkipped, Park: stateRan}, "drover drive 3 to resume it by name", false, "", ""},
		{"at limit", DriveItem{ID: "3", Outcome: outcomeParked, Park: passCapped}, "drover split 3", false, "", ""},
		{"no_record", DriveItem{ID: "3", Outcome: outcomeParked, Park: passSilent}, "drover drive 3, or record", false, "", ""},
		{"denied paths", DriveItem{ID: "3", Outcome: outcomeParked, Park: "protected_paths", PR: 12}, "merge PR #12 by hand, then drover finish 3", false, "", ""},
		{"denied paths, supervisor", DriveItem{ID: "3", Outcome: outcomeParked, Park: "protected_paths", PR: 12}, "read the diff of each denied file, merge PR #12, then drover finish 3", false, polSupervisor, "by hand"},
		{"blocked on deps", DriveItem{ID: "3", Outcome: outcomeParked, Park: "blocked_on_deps", PR: 12}, "then merge PR #12 and drover finish 3", false, "", ""},
		{"merge gate refused", DriveItem{ID: "3", Outcome: outcomeParked, Park: "gate_refused"}, "drover drive 3, or drover run 3", false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := driveFollowUp(c.d, "work/03-x.md", c.drafted, 4, c.deny).Next
			if !strings.Contains(got, c.want) {
				t.Errorf("next step %q, want it to contain %q", got, c.want)
			}
			if c.absent != "" && strings.Contains(got, c.absent) {
				t.Errorf("next step %q contains %q", got, c.absent)
			}
		})
	}
}

// A park cause added to drive.go later still ends on a next step that names the
// item and a command. This reads the causes out of the source so a new one is
// covered the day it is written.
func TestEveryParkCauseInDriveNamesTheItem(t *testing.T) {
	src, err := os.ReadFile("drive.go")
	if err != nil {
		t.Fatal(err)
	}
	causes := regexp.MustCompile(`(?:park\(|Park = |outcomeParked, )"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(causes) < 10 {
		t.Fatalf("found %d park causes in drive.go; the pattern no longer matches how they are written", len(causes))
	}
	for _, m := range causes {
		f := driveFollowUp(DriveItem{ID: "9", Outcome: outcomeParked, Park: m[1], Reason: "why"}, "work/09-x.md", false, 4, polOperator)
		if !strings.Contains(f.Next, "9") || !strings.Contains(f.Next, "drover ") {
			t.Errorf("%s: next step %q names neither the item nor a command", m[1], f.Next)
		}
	}
}
