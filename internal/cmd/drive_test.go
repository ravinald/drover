package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/reports"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
)

func passCounts(audits, changes int, repeated bool) ledger.Counts {
	return ledger.Counts{Audits: audits, ChangesRequested: changes, RepeatedChanges: repeated}
}

var ok = runlog.Result{Subtype: "success"}

// bodyCounts is passCounts over a body whose last recorded digest is the one
// the review launched with.
func bodyCounts(audits, changes int) ledger.Counts {
	c := passCounts(audits, changes, false)
	c.LastBody = launchBody
	return c
}

// rowCounts is the Counts the real reader takes from rows, for the cases whose
// link is about which row a digest came from.
func rowCounts(rows ...ledger.Row) ledger.Counts {
	return (&ledger.Ledger{Rows: rows}).Counts("1")
}

// The digests the table's bodies hash to. Any 12 hex would do; distinct is what
// matters.
const (
	launchBody = "aaaaaaaaaaaa"
	guardBody  = "bbbbbbbbbbbb"
	handBody   = "aaaaaaaaaaab" // launchBody with one byte changed
)

// Item bodies on either side of `drover close-changes`.
const (
	openBody    = "1. Do it.\n\n" + prompts.ChangesHeading + "\n\nR1 unmet: still open.\n"
	clearedBody = "1. Do it.\n\n" + prompts.ChangesHeading + "\n\n" + prompts.ClearedMark + " Closed on 2026-10-01.\n"
)

var (
	reviewLaunch = ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: launchBody, Date: "2026-10-01"}
	reviewAudit  = ledger.Row{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-10-01"}
	reviewClear  = ledger.Row{ID: "1", Body: guardBody, Cleared: true, Date: "2026-10-01"}
)

// The branch that merges unreviewed work if it is wrong, so it is driven
// directly rather than through a drive. Every row of the table, plus the two
// orderings that carry the design.
func TestPassResult(t *testing.T) {
	for _, c := range []struct {
		name string
		p    pass
		want string
	}{
		{"no log at all", pass{before: passCounts(0, 0, false), after: passCounts(1, 0, false)}, passDied},
		{"a log with no terminal event",
			pass{before: passCounts(0, 0, false), after: passCounts(1, 0, false), logged: true}, passDied},
		// Events drover's own schemas claim, with no terminal one among them:
		// the session stopped. The count is not the discriminator.
		{"a stream drover reads, cut off",
			pass{before: passCounts(0, 0, false), after: passCounts(1, 0, false), events: 40, known: 40}, passDied},
		// Events nothing claims: the reviewer executed and drover cannot read
		// it. Reporting that as a session that died sends the next operator
		// looking for a crash that never happened.
		{"a stream drover has no schema for",
			pass{before: passCounts(0, 0, false), after: passCounts(1, 0, false), events: 40}, passUnreadable},
		{"the session errored",
			pass{after: passCounts(1, 0, false), logged: true, res: runlog.Result{Subtype: "error", IsError: true}}, passDied},
		{"recorded nothing",
			pass{before: passCounts(2, 1, false), after: passCounts(2, 1, false), logged: true, res: ok}, passSilent},
		{"sent work back",
			pass{before: passCounts(1, 0, false), after: passCounts(2, 1, false), logged: true, res: ok}, passChangesRequested},
		{"found nothing",
			pass{before: bodyCounts(1, 1), after: bodyCounts(2, 1), logged: true, res: ok,
				body: launchBody}, passPassed},
		{"the same evidence twice",
			pass{before: passCounts(1, 1, false), after: passCounts(2, 2, true), logged: true, res: ok}, passRepeating},
		// The park for a ledger that has nothing to compare against: rows written
		// before launch rows carried a digest.
		{"an audit, and no body digest anywhere in the item's rows",
			pass{before: passCounts(0, 0, false), after: passCounts(1, 0, false), logged: true, res: ok,
				body: launchBody}, passChangesUnrecorded},
		// The cleanest item in a batch needs no human to merge it.
		{"a first pass, an audit, no request for changes, and the body as it launched",
			pass{before: rowCounts(), after: rowCounts(reviewLaunch, reviewAudit), logged: true, res: ok,
				body: launchBody}, passPassed},
		{"the same first pass with one byte of the body changed by hand",
			pass{before: rowCounts(), after: rowCounts(reviewLaunch, reviewAudit), logged: true, res: ok,
				body: handBody}, passEdited},
		// invariant appends a section and link rewrites the closes line, and each
		// records the digest of what it wrote, so the body moving is drover's
		// own write and not a finding.
		{"guard wrote the body during the pass",
			pass{before: rowCounts(), logged: true, res: ok, body: guardBody,
				after: rowCounts(reviewLaunch, ledger.Row{ID: "1", Body: guardBody, Date: "2026-10-01"}, reviewAudit)},
			passPassed},
		{"claim wrote the body during the pass",
			pass{before: rowCounts(), logged: true, res: ok, body: guardBody,
				after: rowCounts(reviewLaunch, reviewAudit, ledger.Row{ID: "1", Body: guardBody, Date: "2026-10-01"})},
			passPassed},
		// A discriminator that counted changes rows would merge this: an earlier
		// request for changes is no proof that this pass wrote its findings down.
		{"a pass after an earlier request for changes, with the body edited by hand",
			pass{before: bodyCounts(1, 1), after: bodyCounts(2, 1), logged: true, res: ok,
				body: handBody}, passEdited},
		// An earlier round's block still open over an unchanged digest.
		{"an audit over a body whose earlier block still stands",
			pass{before: bodyCounts(1, 1), after: bodyCounts(2, 1), logged: true, res: ok,
				body: launchBody, text: openBody}, passStanding},
		{"the same block with no digest to read it against",
			pass{before: passCounts(1, 1, false), after: passCounts(2, 1, false), logged: true, res: ok,
				body: launchBody, text: openBody}, passStanding},
		{"the same block once cleared",
			pass{before: bodyCounts(1, 1), after: bodyCounts(2, 1), logged: true, res: ok,
				body: launchBody, text: clearedBody}, passPassed},
		// The review itself runs `drover close-changes` and records its audit. The
		// clear's digest row is drover's own write, so the moved body is not
		// passEdited, and the block it leaves is cleared, so not passStanding.
		{"a review that clears the block, then audits",
			pass{before: rowCounts(), logged: true, res: ok, body: guardBody, text: clearedBody,
				after: rowCounts(reviewLaunch, reviewClear, reviewAudit)}, passPassed},
		{"a review that audits, then clears the block",
			pass{before: rowCounts(), logged: true, res: ok, body: guardBody, text: clearedBody,
				after: rowCounts(reviewLaunch, reviewAudit, reviewClear)}, passPassed},
		{"a review that audits over the open block and clears nothing",
			pass{before: rowCounts(), logged: true, res: ok, body: launchBody, text: openBody,
				after: rowCounts(reviewLaunch, reviewAudit)}, passStanding},
		// A row recorded before bodySum holds the exact-bytes digest, which the
		// pass carries beside the new one.
		{"a launch digest in the exact-bytes form",
			pass{before: bodyCounts(1, 1), after: bodyCounts(2, 1), logged: true, res: ok,
				body: handBody, exact: launchBody}, passPassed},
		// The cap still outranks all three: a refused request-changes over an untouched
		// body is the shape of a clean pass and is not one.
		{"a refused request for changes over an unchanged body",
			pass{before: rowCounts(), after: rowCounts(reviewLaunch, reviewAudit), logged: true, res: ok,
				body: launchBody, cap: 2, refused: true}, passCapped},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := passResult(c.p); got != c.want {
				t.Errorf("passResult = %q, want %q", got, c.want)
			}
		})
	}
}

// The one that must never be deleted. max_review_rounds is enforced inside `drover
// request-changes`, which exits 3, so a capped review that wanted to send work back
// records its audit row, is refused, and leaves an audit with no request for changes: the
// signature of a clean pass. Testing for a pass before the cap merges an item
// whose review was still finding defects.
func TestACappedReviewDoesNotLookLikeAPass(t *testing.T) {
	capped := pass{
		before: bodyCounts(1, 2),
		after:  bodyCounts(2, 2), // an audit row, and no request for changes beside it
		logged: true, res: ok, cap: 2, refused: true, body: launchBody,
	}
	if got := passResult(capped); got != passCapped {
		t.Fatalf("a capped review read as %q, want %q", got, passCapped)
	}
	// The case a count cannot express. Identical rows, identical cap, and
	// the cap turned nothing away: this review found nothing, and reading it as
	// capped shuts the item out of a merge for good.
	clean := capped
	clean.refused = false
	if got := passResult(clean); got != passPassed {
		t.Errorf("a review at the cap that sent nothing back read as %q, want %q", got, passPassed)
	}
	// Same rows, no cap declared: then it genuinely is a pass.
	capped.cap = 0
	if got := passResult(capped); got != passPassed {
		t.Errorf("with no cap the same rows read as %q, want %q", got, passPassed)
	}
}

// A session that died beats every other reading, including a cap it happens to
// be at: there is nothing to conclude from a pass that did not finish.
func TestADeadSessionOutranksTheCap(t *testing.T) {
	p := pass{before: passCounts(1, 2, false), after: passCounts(1, 2, false), logged: false, cap: 2}
	if got := passResult(p); got != passDied {
		t.Errorf("a dead session at the cap read as %q, want %q", got, passDied)
	}
}

// auditNoteOnlyRows is a ledger where three review passes each left one implement row,
// one review row and one audit row, and no changes row anywhere in the item's
// history: each pass sent work back in its audit note alone. The notes are
// trimmed to the clause in each that says what the pass sent back.
//
// Kept as a constant rather than read from a live ledger. That file is
// append-only, so a test reading it asserts something different every time
// somebody works.
const auditNoteOnlyRows = `{"id":"E24","stage":"implement","runner":"claude","date":"2026-09-10"}
{"id":"E24","stage":"review","runner":"claude","date":"2026-09-10"}
{"id":"E24","reviewer":"claude-opus-5-review","level":"verified","code_by":"agent","independent":true,"date":"2026-09-10","note":"R2 unmet: go test caches the internal/cmd result and git tag output is not a cache input. Remanded with repro and fix. Also sent back: docs/releasing.md:26 hardcodes today's rows as the instruction."}
{"id":"E24","stage":"implement","runner":"claude","date":"2026-09-10"}
{"id":"E24","stage":"review","runner":"claude","date":"2026-09-10"}
{"id":"E24","reviewer":"claude","level":"verified","code_by":"agent","independent":true,"date":"2026-09-10","note":"Found one branch regression, sent back: the test compares with slices.Equal, so a table ahead of the newest tag fails identically to one behind it."}
{"id":"E24","stage":"implement","runner":"claude","date":"2026-09-11"}
{"id":"E24","stage":"review","runner":"claude","date":"2026-09-11"}
{"id":"E24","reviewer":"claude-opus-5 (independent review)","level":"verified","code_by":"agent","independent":true,"date":"2026-09-11","note":"Both sent back findings closed and mutation-tested. Found no unmet requirement and no branch regression."}
{"id":"E24","merged":true,"implementer":"claude","date":"2026-09-11"}
`

// naivePassResult is the predicate without the no-request-for-changes case. The claim
// under test is a regression claim, so the naive reading has to run beside the
// real one: without it the test asserts what drive does and says nothing about
// what a predicate lacking the case would do with the same rows.
func naivePassResult(p pass) string {
	audits := p.after.Audits - p.before.Audits
	changes := p.after.ChangesRequested - p.before.ChangesRequested
	switch {
	case !p.logged || !p.res.Succeeded():
		return passDied
	case audits == 0 && changes == 0:
		return passSilent
	case p.cap > 0 && p.after.ChangesRequested >= p.cap:
		return passCapped
	case changes >= 1 && p.after.RepeatedChanges:
		return passRepeating
	case changes >= 1:
		return passChangesRequested
	case audits >= 1:
		return passPassed
	}
	return passSilent
}

// Real rows, driven through the real parser. Each of the three passes recorded
// an audit, sent work back in the audit note, and wrote no changes row. Under
// the naive predicate the first of them reads as a review that found nothing
// and carries the item toward a merge.
func TestAPassThatRequestedChangesByHandDoesNotReadAsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte(auditNoteOnlyRows), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Malformed) != 0 {
		t.Fatalf("the fixture does not parse: %v", l.Malformed)
	}
	counts := func(n int) ledger.Counts { return (&ledger.Ledger{Rows: l.Rows[:n]}).Counts("E24") }

	// before is the ledger as each review launched, after it once the audit row
	// finished, which is the pair driveItem hands passResult.
	for _, c := range []struct {
		name          string
		before, after int
	}{
		{"the first pass, R2 unmet", 1, 3},
		{"the second, one branch regression", 4, 6},
		{"the third, both findings closed", 7, 9},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := pass{before: counts(c.before), after: counts(c.after), logged: true, res: ok}
			if p.after.Audits-p.before.Audits != 1 || p.after.ChangesRequested != 0 {
				t.Fatalf("the fixture is not the shape under test: audits +%d, requests for changes %d",
					p.after.Audits-p.before.Audits, p.after.ChangesRequested)
			}
			if got := naivePassResult(p); got != passPassed {
				t.Fatalf("before B27 these rows read as %q, want %q; the regression claim is wrong", got, passPassed)
			}
			if got := passResult(p); got == passPassed {
				t.Errorf("passResult = %q: a pass that sent back by hand still carries the item toward a merge", got)
			} else if got != passChangesUnrecorded {
				t.Errorf("passResult = %q, want %q", got, passChangesUnrecorded)
			}
		})
	}
}

// driveProject is a repository with one accepted item and two declared runners.
// driveProject is a project drive will walk: two stub runners it cannot launch,
// so every item parks on a review that recorded nothing, and a gate declared so
// the run is not refused before it starts. Nothing here reaches the gate, which
// is why the binary it names never has to exist.
func driveProject(t *testing.T) *session {
	t.Helper()
	return driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n")
}

func driveProjectWith(t *testing.T, extra string) *session {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// A gh that answers. drive refuses before launching when a gate is declared
	// and the forge cannot be reached, because a green gate opens a pull
	// request; the host's own gh is not usable here either way, since fixture
	// gives the process a home with no credentials in it.
	ghBin := t.TempDir()
	stub(t, filepath.Join(ghBin, "gh"), "#!/bin/sh\n"+
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n"+
		"echo '[]'\n")
	// Runners that resolve, because drive refuses one that does not before it
	// launches anything. A test that launches puts its own ahead of these.
	for _, name := range []string{"stub-impl", "stub-rev"} {
		stub(t, filepath.Join(ghBin, name), "#!/bin/sh\nexit 1\n")
	}
	t.Setenv("PATH", ghBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := fixture(t, `
[runners.impl]
interactive = ["stub-impl"]
headless    = ["stub-impl"]

[runners.rev]
interactive = ["stub-rev"]
headless    = ["stub-rev"]

[run]
default_runner = "impl"
review_runner  = "rev"
`+extra)
	dir := s.cfg.Root
	// Everything committed and drover's own records ignored, because
	// reconciliation refuses to finish over any path git reports, and an
	// untracked config or ledger is one.
	write(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\n")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	// A push target. drive refuses a tree with no origin before it launches
	// anything, because the push is what makes the gate's answer and the merge's
	// commit binding about this code; a fixture with no remote would be testing
	// that refusal and nothing else.
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "remote", "add", "origin", remote)
	git(t, dir, "push", "-q", "-u", "origin", "main")
	t.Chdir(dir)
	return s
}

// The staleness trap, and a silent total failure if it is wrong. open() reads
// the ledger once and session.record appends only the rows this process writes,
// so rows an agent's own `drover audit` wrote land in a child and reach nothing
// here. A drive that skipped the reload would read every delta as zero, take
// every review for a pass, and merge the lot.
func TestDriveReloadsTheLedgerAfterEachLaunch(t *testing.T) {
	s := driveProject(t)
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	before := s.ledger.Counts("1")

	// A child process appends, exactly as a review session's `drover audit` does.
	if err := os.WriteFile(path,
		[]byte(`{"id":"1","level":"reviewed","code_by":"agent","independent":true,"date":"2026-09-13"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.ledger.Counts("1"); got.Audits != before.Audits {
		t.Fatal("the in-memory ledger saw a child's write without a reload; this test has nothing to prove")
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.ledger.Counts("1"); got.Audits != before.Audits+1 {
		t.Errorf("after reload audits = %d, want %d", got.Audits, before.Audits+1)
	}
}

// --dry-run resolves every step and leaves nothing behind: no stage row, no
// branch switch, no log.
func TestDriveDryRunLaunchesNothing(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	head := headBranch(t, dir)

	out := capture(t, func() {
		if err := execute(t, "drive", "--dry-run"); err != nil {
			t.Fatalf("drive --dry-run: %v", err)
		}
	})
	if !strings.Contains(out, "Nothing launched") {
		t.Errorf("dry run does not say it launched nothing:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl")); err == nil && strings.Contains(string(b), "stage") {
		t.Error("dry run wrote a stage row")
	}
	if got := headBranch(t, dir); got != head {
		t.Errorf("dry run moved the tree to %q", got)
	}
}

// gateScript writes a merge gate that exits with the code given and prints a
// line, so the three arms can be driven without a forge.
func gateScript(t *testing.T, dir string, code int) string {
	t.Helper()
	p := filepath.Join(dir, "merge-gate")
	t.Setenv("DROVER_TEST_GATE_EXIT", strconv.Itoa(code))
	stub(t, p, "#!/bin/sh\necho \"merge gate saw $1 against $2\"\nexit \"$DROVER_TEST_GATE_EXIT\"\n")
	return p
}

// The gate answers two questions with three codes, and the two failures want
// opposite responses: a red check the diff caused goes back to the branch, and
// one it did not stops the drive, because every later item builds on a trunk
// that never received this merge.
func TestTheGateSeparatesADiffFromAForge(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		want int
	}{
		{"green", 0, gateGreen},
		{"this diff", 1, gateDiff},
		{"the forge", 2, gateForge},
		{"unclassified stops rather than loops", 9, gateForge},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := driveProject(t)
			g := gateScript(t, t.TempDir(), c.code)
			s.cfg.Loop.MergeGate = []string{g}
			got, out, err := s.runGate(t.Context(), "b1-x", "main")
			if err != nil {
				t.Fatalf("runGate: %v", err)
			}
			if got != c.want {
				t.Errorf("exit %d read as %d, want %d", c.code, got, c.want)
			}
			if !strings.Contains(out, "merge gate saw b1-x against main") {
				t.Errorf("the merge gate did not get the branch and the trunk: %q", out)
			}
		})
	}
}

// A gate that cannot run is a misconfiguration rather than a "no". It will not
// run for the next item either, so parking every item for one reason and
// merging none is the wrong answer.
func TestAGateThatCannotRunIsNotAnAnswer(t *testing.T) {
	s := driveProject(t)
	s.cfg.Loop.MergeGate = []string{"no-such-merge-gate-binary"}
	if _, _, err := s.runGate(t.Context(), "b", "main"); err == nil {
		t.Error("a merge gate absent from PATH reported no error")
	}
}

// An agent that edits the gate, a workflow or the config in its own pull
// request changes what every later item in the drive is measured by, and that
// change would be reviewed by the loop it just altered.
func TestAMergeRefusesTheFilesThatDecideWhatAGateMeans(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	git(t, dir, "switch", "-q", "-c", "work")
	for _, f := range []string{".drover.toml", "web/app.ts"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "touch the config")

	hits, err := s.deniedPaths("work", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0] != ".drover.toml" {
		t.Errorf("denied %v, want [.drover.toml] and not the ordinary file beside it", hits)
	}
}

// drive invokes finish's and request-changes' work in this process, which skips
// the root's PersistentPreRunE. What closes that hole is the classification
// table: drive is operator, so a session that cannot reach finish cannot reach
// it through drive either. Nothing enforces that link but this.
func TestDriveIsNoMoreOpenThanTheStepsItDrives(t *testing.T) {
	install()
	for _, step := range []string{"run", "review", "finish", "request for changes", "propose"} {
		if classified[step].who == operator && classified["drive"].who != operator {
			t.Errorf("%s is the operator's and drive is not, so drive reaches it from a session", step)
		}
	}
	if classified["drive"].who != operator {
		t.Error("drive must stay operator: it launches the sessions it drives and finishes what merged")
	}
}

// The discriminator reconcile turns on. A merged branch says work shipped; it
// does not say a drive produced it. An operator implementing an item in their
// own session leaves the same branch and the same merge, and drive adopting
// that records a completion for work it never drove and never reviewed.
func TestReconcileFinishesOnlyWhatDroverLaunched(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil || branch == "" {
		t.Fatalf("branchFor: %q %v", branch, err)
	}

	// A merged branch, and nothing else: no stage row, as an item implemented
	// outside drover leaves it.
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, "shipped.txt"), "work\n")
	git(t, dir, "add", "shipped.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	// The head row a drive writes before its push, recorded here for the same
	// reason: after the merge this commit is the trunk's too, and nothing could
	// then tell this branch from one that never moved.
	if err := s.recordHead("1", branch); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", branch)

	landed, left, err := s.reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if landed != 0 || left != 1 {
		t.Errorf("with no launch row: finished %d left %d, want 0 and 1", landed, left)
	}
	if s.ledger.State("1").Merged {
		t.Error("reconcile recorded a completion for work drover never launched")
	}

	// The same branch, once drover has launched the item. This is the crash
	// state reconcile exists for: merged, unfinished, reading as ran forever.
	if err := s.record(ledger.Row{ID: "1", Stage: ledger.StageImplement,
		Runner: "impl", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if landed, _, err := s.reconcile(); err != nil || landed != 1 {
		t.Errorf("with a launch row: finished %d (%v), want 1", landed, err)
	}
	if !s.ledger.State("1").Merged {
		t.Error("reconcile left a drover-launched merge unlanded")
	}
}

// A park naming neither the runner nor the log leaves the next operator to
// diagnose "it died" from nothing: the cause lives in [runners] and the
// evidence lives in the log, and a launch that never wrote one is its own
// state.
func TestAParkNamesTheRunnerAndItsLog(t *testing.T) {
	got := whereItRan("agy", "/logs/B28-x.jsonl")
	if !strings.Contains(got, "agy") || !strings.Contains(got, "/logs/B28-x.jsonl") {
		t.Errorf("got %q", got)
	}
	if got := whereItRan("agy", ""); !strings.Contains(got, "no log written") {
		t.Errorf("a launch that wrote no log reported %q", got)
	}
}

// One contract: a park leaves a clean log behind it, the item reads as `ran`,
// and naming the id is how it resumes. A driveItem that asked the state gate a
// second time could not see that the operator had named anything, so it would
// skip every named park with the cause `ran` and return 0. Nothing clears a run
// log, so the resume path would be unreachable rather than awkward, and
// --dry-run does not run the gate, so the preview would say it drives the item
// the drive throws out.
//
// The arms after the first are the reason the gate cannot simply be deleted: a
// name resumes a park, and says nothing about deps that have not merged or an
// item already finished.
func TestDriveResumesAParkedItemOnlyWhenNamed(t *testing.T) {

	// One fixture per arm. A drive reconciles before it reads the queue, and
	// reconciling finishes what drover launched, so arms sharing a project would
	// see the previous arm's item finish and change the state under test.
	t.Run("a named park resumes", func(t *testing.T) {
		s := driveProject(t)
		it := s.backlog.Items[0]
		id := it.ID.Bare()
		ranClean(t, s, it)

		s = openIn(t, s.cfg.Root)
		if state, _ := s.workState(it, s.cfg.LogDir()); state != stateRan {
			t.Fatalf("the item is %q, not %q: this arm has nothing to prove", state, stateRan)
		}
		if q := s.queue(); len(q) != 0 {
			t.Fatalf("the bare queue carries %d items, want 0: a park has to stay out of it", len(q))
		}
		if d := driveOne(t, id); d.Outcome == outcomeSkipped {
			t.Errorf("drive %s skipped a park it was named, cause %q; the resume path its help documents cannot run", id, d.Park)
		}
	})

	// The two arms a name must not override, and the reason the gate cannot
	// simply be deleted from the named path.
	t.Run("a named blocked item still skips", func(t *testing.T) {
		s := driveProject(t)
		write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"),
			"---\ntitle: Second\ndeps: "+s.backlog.Items[0].ID.Bare()+"\n---\n\n1. Do it.\n")
		git(t, s.cfg.Root, "add", "-A")
		git(t, s.cfg.Root, "commit", "-q", "-m", "second item")
		if d := driveOne(t, "2"); d.Outcome != outcomeSkipped || d.Park != stateBlocked {
			t.Errorf("a named blocked item drove as %q/%q, want %q/%q", d.Outcome, d.Park, outcomeSkipped, stateBlocked)
		}
	})

	t.Run("a named merged item still skips", func(t *testing.T) {
		s := driveProject(t)
		id := s.backlog.Items[0].ID.Bare()
		yes := true
		if err := ledger.Append(filepath.Join(s.cfg.Root, "ledger.jsonl"),
			ledger.Row{ID: id, Merged: &yes, Date: ledger.Today()}); err != nil {
			t.Fatal(err)
		}
		if d := driveOne(t, id); d.Outcome != outcomeSkipped || d.Park != stateDone {
			t.Errorf("a named merged item drove as %q/%q, want %q/%q", d.Outcome, d.Park, outcomeSkipped, stateDone)
		}
	})
}

// ranClean writes the log a clean run leaves, which is what workState reads as
// `ran` and what a park leaves behind it.
func ranClean(t *testing.T, s *session, it backlog.Item) {
	t.Helper()
	path := filepath.Join(s.cfg.LogDir(),
		runlog.Name(s.cfg.Paths.LogPrefix+runlog.Base(it.ID.Stem(), it.Slug), time.Now()))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path,
		[]byte(`{"type":"result","subtype":"success","is_error":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// driveOne drives one named id through the real command and returns the row it
// reported, which is the only place the outcome and the skip cause appear.
func driveOne(t *testing.T, id string) DriveItem {
	t.Helper()
	out := capture(t, func() {
		if err := execute(t, "drive", id, "--json"); err != nil {
			t.Fatalf("drive %s: %v", id, err)
		}
	})
	// The rows are picked out of the stream rather than decoded from the top of
	// it: reconcile invokes finish, and finish reports its trunk handling on stdout,
	// so a drive that reconciles anything puts progress lines beside its
	// documents.
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			return d
		}
	}
	t.Fatalf("drive %s wrote no row to parse:\n%s", id, out)
	return DriveItem{}
}

// The preview and the run have to agree. --dry-run is the read an operator has
// before spending a night. A drivePlan that resolved a branch and a runner for
// every item it was handed would list a named blocked item among the "1
// item(s)" it would drive, then skip it with one word when the drive ran.
func TestDriveDryRunAgreesWithTheRun(t *testing.T) {

	t.Run("a named blocked item previews as skipped", func(t *testing.T) {
		s := driveProject(t)
		write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"),
			"---\ntitle: Second\ndeps: "+s.backlog.Items[0].ID.Bare()+"\n---\n\n1. Do it.\n")

		out := dryRun(t, "2")
		row := previewRow(t, out, "2")
		if !strings.Contains(row, outcomeSkipped) {
			t.Errorf("the preview does not say it would skip: %q", row)
		}
		if !strings.Contains(row, "blocked by ") {
			t.Errorf("the preview does not name the dep, so the operator cannot fix it: %q", row)
		}
		if !strings.Contains(out, "0 item(s) to drive") {
			t.Errorf("the preview counts an item it would not drive:\n%s", out)
		}
	})

	t.Run("a named merged item previews as skipped", func(t *testing.T) {
		s := driveProject(t)
		id := s.backlog.Items[0].ID.Bare()
		yes := true
		if err := ledger.Append(filepath.Join(s.cfg.Root, "ledger.jsonl"),
			ledger.Row{ID: id, Merged: &yes, Date: ledger.Today()}); err != nil {
			t.Fatal(err)
		}
		if row := previewRow(t, dryRun(t, id), id); !strings.Contains(row, outcomeSkipped) {
			t.Errorf("a merged item previews as drivable: %q", row)
		}
	})

	// The other direction, and the reason the gate cannot simply be copied into
	// the preview as "ready only": a named park is what the run resumes, so the
	// preview has to resolve its branch and its runners.
	t.Run("a named park previews as drivable", func(t *testing.T) {
		s := driveProject(t)
		it := s.backlog.Items[0]
		ranClean(t, s, it)

		out := dryRun(t, it.ID.Bare())
		row := previewRow(t, out, it.ID.Bare())
		if !strings.Contains(row, "branch ") {
			t.Errorf("the preview skips a park the run resumes: %q", row)
		}
		if !strings.Contains(out, "1 item(s) to drive") {
			t.Errorf("the preview does not count the park it would drive:\n%s", out)
		}
	})

	// A bare drive never resumes a park, so the preview must not offer one. The
	// empty queue is reported on stderr, which is what keeps --json's stdout
	// carrying the document and nothing else.
	t.Run("an unnamed park is not in the previewed queue", func(t *testing.T) {
		s := driveProject(t)
		ranClean(t, s, s.backlog.Items[0])
		var out string
		err := captureErr(t, func() {
			out = capture(t, func() {
				if e := execute(t, "drive", "--dry-run"); e != nil {
					t.Fatalf("drive --dry-run: %v", e)
				}
			})
		})
		if !strings.Contains(err, "queue empty") {
			t.Errorf("the bare preview does not report an empty queue: %q", err)
		}
		if strings.Contains(out, "item(s) to drive") {
			t.Errorf("the bare preview offered a park:\n%s", out)
		}
	})
}

// previewRow is the preview's line for one id, so an assertion about that item
// cannot be satisfied by the summary underneath it: "0 skipped" carries the
// same word a skipped row does.
func previewRow(t *testing.T, out, id string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, id+" ") {
			return line
		}
	}
	t.Fatalf("the preview has no row for %s:\n%s", id, out)
	return ""
}

// dryRun previews a drive and returns what it printed. The ids are optional:
// with none it previews the ready queue.
func dryRun(t *testing.T, ids ...string) string {
	t.Helper()
	args := append([]string{"drive", "--dry-run"}, ids...)
	return capture(t, func() {
		if err := execute(t, args...); err != nil {
			t.Fatalf("drive --dry-run %v: %v", ids, err)
		}
	})
}

// A codex review that traced its requirements, wrote a changes row and an audit,
// wrote its report and ended on turn.completed, must not park as a session that
// died. Built from a real codex stream through ReadResult rather than from
// hand-set fields, because the defect this guards lives in the seam between the
// two: every field passResult reads can be correct for a log drover misreads.
func TestACodexReviewThatFinishedReadsAsChangesRequested(t *testing.T) {
	rd, err := runlog.ReadResult(filepath.Join("..", "runlog", "testdata", "codex-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := pass{
		before: passCounts(1, 0, false),
		after:  passCounts(2, 1, false),
		res:    rd.Result, logged: rd.Found, events: rd.Events, known: rd.Known,
	}
	if got := passResult(p); got != passChangesRequested {
		t.Fatalf("a codex review that finished and sent work back read as %q, want %q", got, passChangesRequested)
	}
}

// The other side of it. A codex turn that failed is a session that died, and
// reading its ledger rows instead would take a request for changes from a review that never
// finished its trace.
func TestACodexTurnThatFailedReadsAsDied(t *testing.T) {
	rd, err := runlog.ReadResult(filepath.Join("..", "runlog", "testdata", "codex-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := pass{
		before: passCounts(1, 0, false),
		after:  passCounts(2, 1, false),
		res:    rd.Result, logged: rd.Found, events: rd.Events, known: rd.Known,
	}
	if got := passResult(p); got != passDied {
		t.Errorf("a codex turn that failed read as %q, want %q", got, passDied)
	}
}

// mergeProject is a queue drive can take all the way to the forge: two ready
// items, a green gate, a gh that records what it was asked, and runner stubs
// that leave the ledger rows a drive reads back.
//
// Item 1's review records an audit and item 2's records nothing, so one run
// carries both arms of the discriminator. A fixture where every item takes the
// same branch proves the drive walked a queue and nothing about what it decided
// per item.
func mergeProject(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	identify(t)
	bin := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh.log")

	// The gate records what the remote holds against what the branch holds,
	// because that equality is the whole claim of the push: a gate reading check
	// runs for a SHA answers about the commit the forge has, and a merge bound
	// with --match-head-commit refuses anything else. It runs in the item's
	// worktree, and DROVER_ROOT names the project, so merge-gate.log lands beside the
	// ledger.
	stub(t, filepath.Join(bin, "merge-gate"), "#!/bin/sh\n"+
		"echo \"merge gate saw $1 against $2\"\n"+
		"printf '%s local=%s remote=%s\\n' \"$1\" \"$(git rev-parse \"$1\")\" "+
		"\"$(git rev-parse \"origin/$1\" 2>/dev/null || echo none)\" >> \"$DROVER_ROOT/merge-gate.log\"\n"+
		"exit 0\n")
	// Every gh drover reaches during a drive answers here. The merge and the
	// pull request are the calls under test; the register reads are answered
	// with an empty page so a degraded read never stands in for a merge that did
	// not happen.
	//
	// pr list answers [] rather than nothing. One `case pr) exit 0` for every pr
	// subcommand returns an empty body to a JSON read, which fails to parse, so
	// the stub would be reporting a broken forge to every caller that looks for
	// an existing pull request before opening one.
	//
	// The check-runs read answers one finished green check, which is what a
	// forge running CI on a push says about the head drive waits on before
	// each gate.
	t.Setenv("DROVER_TEST_GH_LOG", ghLog)
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n"+
		"printf '%s\\n' \"$*\" >> \"$DROVER_TEST_GH_LOG\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pr list')   echo '[]' ;;\n"+
		"  'pr create') echo 'https://example.invalid/o/r/pull/7' ;;\n"+
		"  'pr '*)      : ;;\n"+
		"  'api '*/required_status_checks) echo '{\"contexts\":[\"ci\"],\"checks\":[]}' ;;\n"+
		"  'api '*/check-runs*) echo '{\"total_count\":1,\"check_runs\":[{\"id\":1,\"name\":\"ci\",\"status\":\"completed\",\"conclusion\":\"success\"}]}' ;;\n"+
		"  *)           echo '[]' ;;\n"+
		"esac\n")
	// The review stub writes its audit row for item 1 alone, keyed on a marker
	// only item 1's body carries: the prompt is the last argument, so the
	// runner sees which item it was handed.
	//
	// It also commits, which is what a FIX-IN-REVIEW pass does. Reviews that
	// only read leave the head where the implement stage's push put it, and a
	// fixture made of those cannot tell one push from two.
	stub(t, filepath.Join(bin, "rev"), "#!/bin/sh\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n"+
		"case \"$*\" in *REVIEW-PASSES*) "+
		"printf '%s\\n' '{\"id\":\"1\",\"level\":\"reviewed\",\"code_by\":\"agent\",\"independent\":true,\"date\":\"2026-09-20\"}' "+
		">> \"$DROVER_ROOT/ledger.jsonl\" ; "+
		"if [ -n \"${DROVER_TEST_SPLIT:-}\" ]; then "+
		"sed -i.bak 's/^deps:$/deps: 2/' \"$DROVER_ROOT/work/01-first.md\" ; rm -f \"$DROVER_ROOT/work/01-first.md.bak\" ; "+
		// The body row `drover split` records after rewriting the deps line.
		// Without it this is a hand edit, and the pass parks edited.
		"sum=$( (sha256sum 2>/dev/null || shasum -a 256) < \"$DROVER_ROOT/work/01-first.md\" | cut -c1-12) ; "+
		"printf '{\"id\":\"1\",\"body\":\"%s\",\"date\":\"2026-09-20\"}\\n' \"$sum\" >> \"$DROVER_ROOT/ledger.jsonl\" ; fi ; "+
		"echo fixed > review-fix.txt ; "+
		"git add review-fix.txt ; "+
		"if [ -n \"${DROVER_TEST_ADD:-}\" ]; then "+
		"mkdir -p \"$(dirname \"$DROVER_TEST_ADD\")\" ; echo x > \"$DROVER_TEST_ADD\" ; "+
		"git add -- \"$DROVER_TEST_ADD\" ; fi ; "+
		"git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false "+
		"commit -q -m 'review: a typo' ;; esac\n")
	// The implement stub emits a terminal event unless told to stop short of
	// one, leaving events drover's own schemas claim, none of them a result.
	// Keyed on the environment for DROVER_TEST_SPLIT's reason, so
	// every other test here keeps the fixture it was written against.
	stub(t, filepath.Join(bin, "impl"),
		"#!/bin/sh\n"+
			"if [ -n \"${DROVER_TEST_IMPL_STOPS:-}\" ]; then\n"+
			"  echo '"+strings.ReplaceAll(strings.TrimSpace(hookOnlyEvents), "\n", "'\n  echo '")+"'\n"+
			"  exit 0\n"+
			"fi\n"+
			// DROVER_TEST_IMPL_WAIT holds the run until the file it names
			// exists, so a test can act on a drive while an item is running.
			"if [ -n \"${DROVER_TEST_IMPL_WAIT:-}\" ]; then\n"+
			"  while [ ! -e \"$DROVER_TEST_IMPL_WAIT\" ]; do sleep 0.05; done\n"+
			"fi\n"+
			// It commits, because an implement run that leaves the branch at the
			// trunk is a parked item: there is no diff for the reviewer and
			// `git branch --merged` would list that branch as merged.
			"echo work > implemented.txt\n"+
			"git add implemented.txt\n"+
			"git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false "+
			"commit -q -m 'the run did something'\n"+
			"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	// The item's verify command, which drive runs itself only on a tree a
	// catch-up merged. It logs which verify command ran, where and on what, and fails when
	// every file DROVER_TEST_CHECK_REFUSE names exists: two files that are fine
	// apart and not together are a trunk and a branch that only fail on meeting.
	// DROVER_TEST_CHECK_HANG touches the file it names and never returns.
	stub(t, filepath.Join(bin, "check"), `#!/bin/sh
b=$(git symbolic-ref -q --short HEAD || echo detached)
printf '%s dir=%s head=%s pushed=%s\n' "$1" "$(pwd -P)" "$(git rev-parse HEAD)" \
  "$(git rev-parse -q --verify "origin/$b" || echo none)" >> "$DROVER_ROOT/check.log"
if [ -n "${DROVER_TEST_CHECK_HANG:-}" ]; then touch "$DROVER_TEST_CHECK_HANG"; exec sleep 30; fi
if [ -n "${DROVER_TEST_CHECK_REFUSE:-}" ]; then
  for f in $DROVER_TEST_CHECK_REFUSE; do [ -e "$f" ] || exit 0; done
  echo "check: $DROVER_TEST_CHECK_REFUSE cannot all exist"
  exit 1
fi
exit 0
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	proj := fixture(t, `
[runners.impl]
headless = ["`+filepath.Join(bin, "impl")+`"]

[runners.rev]
headless = ["`+filepath.Join(bin, "rev")+`"]

[run]
default_runner = "impl"
review_runner  = "rev"

[drive]
merge_gate = ["`+filepath.Join(bin, "merge-gate")+`"]
`)
	dir := proj.cfg.Root
	check := filepath.Join(bin, "check")
	cfgPath := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfgPath, strings.NewReplacer(
		`default = "make test"`, `default = "`+check+` default"`,
		`ui_only = "make test-ui"`, `ui_only = "`+check+` ui_only"`,
	).Replace(string(raw)))
	// A changes row the reviews converged on, so the item's history reads as one
	// a review has sent back before.
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "1", Changes: "earlier", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES\n")
	write(t, filepath.Join(dir, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it too.\n")
	// drover's own records and the gate's log, ignored. drive stops on any path
	// git reports, so a fixture tracking the ledger parks every item on the rows
	// the drive just wrote and tests the boundary rather than the merge.
	// The backlog joins them: a review's request-changes and split rewrite the item body,
	// which is the write the project's own records rule exists for.
	write(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\nmerge-gate.log\nmerge-gate.merged\ncheck.log\nwork/\n")

	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	// A push target, because drover pushes what it drives and a fixture with no
	// remote would park every item on the push rather than on the thing under
	// test. Bare and on disk: the tests assert against what arrived there, which
	// is the only evidence that the SHA the gate read is the SHA that merges.
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "remote", "add", "origin", remote)
	git(t, dir, "push", "-q", "-u", "origin", "main")
	t.Chdir(dir)
	return ghLog
}

// driveAll drives the whole queue through the real command and returns every
// row it reported, in order.
func driveAll(t *testing.T, args ...string) []DriveItem {
	t.Helper()
	out := capture(t, func() {
		if err := execute(t, append([]string{"drive", "--json"}, args...)...); err != nil {
			t.Fatalf("drive: %v", err)
		}
	})
	var rows []DriveItem
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			rows = append(rows, d)
		}
	}
	return rows
}

// Running drive is the operator saying the code may merge, so the default
// merges. What it must not do is merge everything it walked: the gate and the
// review are the go/no-go, and an item whose review recorded nothing has said
// nothing that could be read as green.
//
// Both arms run inside one drive, because a queue where every item takes the
// same branch says nothing about the discriminator.
func TestDriveMergesWhatPassedAndParksWhatDidNot(t *testing.T) {
	ghLog := mergeProject(t)

	rows := driveAll(t)
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
	}
	pass, park := rows[0], rows[1]
	if pass.ID != "1" || park.ID != "2" {
		t.Fatalf("rows arrived as %s then %s, want 1 then 2", pass.ID, park.ID)
	}
	if pass.PR == 0 {
		t.Error("the merged item reports no pull request; drover opens one for what it merges")
	}
	if !pass.Merged || pass.Outcome != outcomeReviewed {
		t.Errorf("the reviewed item is %q/merged=%v, want %q/true; drive stopped short of the merge it was run to perform",
			pass.Outcome, pass.Merged, outcomeReviewed)
	}
	if park.Merged || park.Outcome != outcomeParked || park.Park != passSilent {
		t.Errorf("the silent item is %q/%q/merged=%v, want %q/%q/false; a park is never merged",
			park.Outcome, park.Park, park.Merged, outcomeParked, passSilent)
	}

	calls := ghCalls(t, ghLog)
	var merges []string
	for _, c := range calls {
		if strings.HasPrefix(c, "pr merge") {
			merges = append(merges, c)
		}
	}
	if len(merges) != 1 {
		t.Fatalf("gh was asked to merge %d times, want 1: %v", len(merges), merges)
	}
	if !strings.Contains(merges[0], pass.Branch) {
		t.Errorf("gh merged %q, which does not name %s's branch %q", merges[0], pass.ID, pass.Branch)
	}
	// A forge that queues the merge decides it, so drive neither merges nor
	// finishes anything itself: the next drive's reconciliation does.
	if !strings.HasPrefix(merges[0], "pr merge --auto --squash --match-head-commit ") || pass.Finished {
		t.Errorf("gh was asked %q and the row reads finished=%v; want a queued squash merge, not finished here",
			merges[0], pass.Finished)
	}
	if strings.Contains(merges[0], park.Branch) {
		t.Errorf("gh was asked to merge the parked item's branch: %q", merges[0])
	}
}

// --no-merge is the opt-out, and it holds the item the default would have
// merged. Same fixture, same green gate, same clean review: the only difference
// is the flag, which is what makes this a test of the flag.
func TestNoMergeHoldsWhatTheGatePassed(t *testing.T) {
	ghLog := mergeProject(t)

	rows := driveAll(t, "--no-merge")
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
	}
	pass := rows[0]
	if pass.Merged || pass.Outcome != outcomeReviewed {
		t.Errorf("under --no-merge the reviewed item is %q/merged=%v, want %q/false",
			pass.Outcome, pass.Merged, outcomeReviewed)
	}
	// The flag holds the merge, not the pull request. An operator driving a
	// queue overnight and merging in the morning needs something to merge, and
	// the branch passed both the things that decide whether one should exist.
	if pass.PR == 0 {
		t.Error("--no-merge left the green item with no pull request, so the operator has nothing to merge")
	}
	if got := len(prCreates(t, ghLog)); got != 1 {
		t.Errorf("the forge was asked for %d pull requests under --no-merge, want 1", got)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr merge") {
			t.Errorf("--no-merge asked the forge to merge: %q", c)
		}
	}
}

// An absent gate is a config error, and catching it at the start means nothing
// has been paid for yet. A refusal that arrived per item would arrive after a
// night of launches.
func TestDriveRefusesBeforeSpendingAnythingOnAQueueNothingCanPass(t *testing.T) {
	s := driveProjectWith(t, "") // no [drive] merge_gate
	dir := s.cfg.Root
	head := headBranch(t, dir)

	err := refuse(t, "drive")
	if Code(err) != ExitRefused {
		t.Fatalf("drive with no merge gate exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	if !strings.Contains(err.Error(), "--no-merge") {
		t.Errorf("the refusal does not name the way out:\n%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl")); err == nil && strings.Contains(string(b), "stage") {
		t.Error("the refusal arrived after a launch: a stage row is on disk")
	}
	if got := headBranch(t, dir); got != head {
		t.Errorf("the refusal arrived after a branch switch, on %q", got)
	}

	// The same project under --no-merge asked nobody to say green, so nothing
	// there is missing and the drive runs.
	if err := refuse(t, "drive", "--no-merge"); Code(err) == ExitRefused {
		t.Errorf("--no-merge refused a project that declares no merge gate and needs none: %v", err)
	}
}

// gateLog is what the merge gate saw, one line per run.
func gateLog(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "merge-gate.log")) // #nosec G304 -- written by the fixture's own stub
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

// The push is what makes the gate's answer about this code. A gate that reads
// check runs for a SHA, and a merge bound to one, both ask the forge about a
// commit; if the remote head is behind the branch, both are answering about
// something else and neither says so.
func TestAPassedItemIsPushedBeforeTheGateRuns(t *testing.T) {
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	driveAll(t)

	seen := gateLog(t, dir)
	if len(seen) == 0 {
		t.Fatal("the merge gate never ran, so nothing here is evidence about the push")
	}
	for _, line := range seen {
		branch, local, remote := splitGateLine(t, line)
		if remote == "none" {
			t.Errorf("%s: the merge gate ran against a branch the remote has never seen", branch)
			continue
		}
		if local != remote {
			t.Errorf("%s: merge gate saw local %s and remote %s; the merge would be bound to a commit the forge does not have",
				branch, local, remote)
		}
	}
}

func splitGateLine(t *testing.T, line string) (branch, local, remote string) {
	t.Helper()
	f := strings.Fields(line)
	if len(f) != 3 {
		t.Fatalf("merge-gate.log line %q is not <branch> local=<sha> remote=<sha>", line)
	}
	return f[0], strings.TrimPrefix(f[1], "local="), strings.TrimPrefix(f[2], "remote=")
}

// The push is unconditional, so a tree with no push target is the same class of
// config error as an absent gate: it fails identically for every item, and a
// park per item reports it once per paid session.
func TestDriveRefusesAQueueItCannotPush(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	git(t, dir, "remote", "remove", "origin")
	head := headBranch(t, dir)

	for _, args := range [][]string{{"drive"}, {"drive", "--no-merge"}, {"drive", "--dry-run"}} {
		err := refuse(t, args...)
		if Code(err) != ExitRefused {
			t.Errorf("%v exited %d (%v), want %d", args, Code(err), err, ExitRefused)
		}
		if err != nil && !strings.Contains(err.Error(), "origin") {
			t.Errorf("%v: the refusal does not name what is missing:\n%v", args, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl")); err == nil && strings.Contains(string(b), "stage") {
		t.Error("the refusal arrived after a launch: a stage row is on disk")
	}
	if got := headBranch(t, dir); got != head {
		t.Errorf("the refusal arrived after a branch switch, on %q", got)
	}
}

// A gate that can pass opens a pull request, so an unreachable forge is
// refused up front rather than discovered on the first green item. A project
// declaring no gate reaches no forge and is left alone.
func TestDriveRefusesAGateItCannotActOn(t *testing.T) {
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\nexit 1\n") // gh is there and says no
	driveProject(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := refuse(t, "drive")
	if Code(err) != ExitRefused {
		t.Fatalf("drive with an unreachable forge exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("the refusal does not name the next step:\n%v", err)
	}
}

// prCreates is the pull requests the forge was asked to open.
func prCreates(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, c := range ghCalls(t, log) {
		if strings.HasPrefix(c, "pr create") {
			out = append(out, c)
		}
	}
	return out
}

// An open pull request is the claim that a branch may merge, so a park must not
// leave one. The branch is pushed all the same: work that came back is on the
// remote where somebody can look at it, and what it does not have is a document
// saying it is ready.
func TestAParkOpensNoPullRequest(t *testing.T) {
	ghLog := mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	rows := driveAll(t)
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2", len(rows))
	}
	parked := rows[1]
	if parked.Outcome != outcomeParked {
		t.Fatalf("item 2 is %q, want %q: the fixture's discriminator moved", parked.Outcome, outcomeParked)
	}
	if parked.PR != 0 {
		t.Errorf("a parked item reports pull request #%d", parked.PR)
	}
	if got := len(prCreates(t, ghLog)); got != 1 {
		t.Errorf("the forge was asked for %d pull requests, want 1 (the passed item's alone): %v",
			got, prCreates(t, ghLog))
	}
	// The park still pushed, which is what makes the sent-back work readable to a human
	// and the next round's gate answerable.
	if out := git(t, dir, "ls-remote", "--heads", "origin", "second"); !strings.Contains(out, "second") {
		t.Errorf("the parked item's branch is not on the remote: %q", out)
	}
}

// The gate blaming the diff is a finding like a reviewer's, and it goes back the
// same way. Nothing has said the branch may merge, so nothing opens a document
// that says so.
func TestAGateRefusalLeavesABranchAndNoPullRequest(t *testing.T) {
	ghLog := mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Repoint the gate at one that blames the diff.
	red := gateScript(t, t.TempDir(), 1)
	cfg := filepath.Join(dir, config.FileName)
	b, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`merge_gate = \[".*"\]`).ReplaceAllString(string(b), `merge_gate = ["`+red+`"]`)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Committed, because the config is tracked here and drive stops on any path
	// git reports: an edit left in the tree would park the item on the tree
	// rather than on the gate.
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "point the merge gate at a red one")

	rows := driveAll(t, "1")
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1", len(rows))
	}
	if rows[0].Park != "gate_refused" {
		t.Errorf("a red merge gate parked %q/%q, want gate_refused", rows[0].Outcome, rows[0].Park)
	}
	if n := prCreates(t, ghLog); len(n) != 0 {
		t.Errorf("a merge gate that blamed the diff still opened a pull request: %v", n)
	}
	if out := git(t, dir, "ls-remote", "--heads", "origin", "first"); !strings.Contains(out, "first") {
		t.Error("the branch a request for changes sent back is not on the remote")
	}
}

// A SPLIT-OUT the review filed mid-drive rewrites the deps line, so the item is
// blocked on work nobody has accepted. Another round would re-implement against
// the same gap; the branch is green and reviewed, so it keeps its pull request
// and waits for a human to merge it once the dep merges.
func TestASplitMidDriveParksWithThePullRequestOpen(t *testing.T) {
	// --no-merge holds a merge that was going to happen; a blocked item was never
	// going to merge. Reporting it reviewed under the flag hides the split behind
	// something the operator set for an unrelated reason, and the next drive
	// re-implements against a dep nobody accepted.
	for _, c := range []struct {
		name string
		args []string
	}{
		{"merging", []string{"1"}},
		{"--no-merge", []string{"--no-merge", "1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ghLog := mergeProject(t)
			t.Setenv("DROVER_TEST_SPLIT", "1")

			rows := driveAll(t, c.args...)
			if len(rows) != 1 {
				t.Fatalf("drive reported %d rows, want 1: %+v", len(rows), rows)
			}
			d := rows[0]
			if d.Park != "blocked_on_deps" {
				t.Errorf("parked %q/%q, want blocked_on_deps: %s", d.Outcome, d.Park, d.Reason)
			}
			if d.Merged {
				t.Error("an item blocked on a dep nobody has accepted was merged")
			}
			if d.PR == 0 {
				t.Error("the branch is green and reviewed and has no pull request, so nobody can merge it when the dep finishes")
			}
			if !strings.Contains(d.Reason, "2") {
				t.Errorf("the park does not name what it waits for: %q", d.Reason)
			}
			for _, call := range ghCalls(t, ghLog) {
				if strings.HasPrefix(call, "pr merge") {
					t.Errorf("a blocked item was sent to the forge to merge: %q", call)
				}
			}
		})
	}
}

// A rejected push is an answer, not an obstacle. Forcing past it would discard
// whatever put the remote ahead, which can be a human's commits.
func TestAPushRejectionParksAndNeverForces(t *testing.T) {
	ghLog := mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Put an unrelated commit on origin/first, so drover's push of the item's
	// own branch is not a fast-forward.
	remote := git(t, dir, "remote", "get-url", "origin")
	other := t.TempDir()
	git(t, other, "clone", "-q", remote, other)
	git(t, other, "switch", "-q", "-c", "first")
	git(t, other, "commit", "-q", "--allow-empty", "-m", "somebody else was here")
	git(t, other, "push", "-q", "origin", "first")

	rows := driveAll(t, "1")
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1", len(rows))
	}
	if rows[0].Park != "push_failed" {
		t.Errorf("parked %q/%q, want push_failed: %s", rows[0].Outcome, rows[0].Park, rows[0].Reason)
	}
	if n := prCreates(t, ghLog); len(n) != 0 {
		t.Errorf("a branch drover could not push still got a pull request: %v", n)
	}
	// The remote still holds what it held: nothing overwrote the other commit.
	if out := git(t, other, "log", "-1", "--format=%s", "origin/first"); out != "somebody else was here" {
		t.Errorf("origin/first now reads %q; the push overwrote somebody else's commit", out)
	}
}

// [run] branch renders a name from the item and nothing guarantees a ref behind
// it. The check belongs to the push because every path to the forge goes through
// one, and git's own "src refspec does not match any" names neither the template
// nor the item.
func TestAPushNamesABranchNothingCreated(t *testing.T) {
	s := driveProject(t)
	err := s.pushBranch("never-created")
	if err == nil {
		t.Fatal("pushed a branch that does not exist")
	}
	for _, want := range []string{"never-created", "[run] branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// A correction pass returns to the same branch, so finding a pull request already
// open is the ordinary case. Opening a second splits the review history of one
// contract across two places.
func TestOpenPRReusesAPullRequestAlreadyOpen(t *testing.T) {
	s := driveProject(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("DROVER_TEST_GH_LOG", log)
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n"+
		"printf '%s\\n' \"$*\" >> \"$DROVER_TEST_GH_LOG\"\n"+
		"case \"$1 $2\" in 'pr list') echo '[{\"number\":7}]' ;; *) echo '[]' ;; esac\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The branch has to exist. Both callers push it first, and the push is what
	// checks: a rendered name is not a ref.
	git(t, s.cfg.Root, "switch", "-q", "-c", "first")
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.openPR(it, "first", "main", s.factsFor(it))
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 7 {
		t.Errorf("reported pull request #%d, want the open #7", p.Number)
	}
	if p.Created {
		t.Error("reported opening one when #7 was already there")
	}
	for _, c := range ghCalls(t, log) {
		if strings.HasPrefix(c, "pr create") {
			t.Errorf("opened a second pull request over an open one: %q", c)
		}
	}
}

// The preview answers the same question as the run, which is the whole reason it
// exists. A drive that pushes and opens pull requests and a preview that mentions
// neither answer different ones.
func TestTheDryRunNamesThePushAndThePullRequest(t *testing.T) {
	driveProject(t)

	out := capture(t, func() {
		if err := execute(t, "drive", "--dry-run"); err != nil {
			t.Fatalf("drive --dry-run: %v", err)
		}
	})
	for _, want := range []string{"push each branch to origin", "open a pull request"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview does not name %q:\n%s", want, out)
		}
	}
}

// The body is published, so nothing machine-local goes in it. A gate declared by
// absolute path names one filesystem, and naming the key says as much without
// carrying the operator's home directory into a document strangers read.
func TestTheBodyNamesTheGateKeyAndNotItsArgv(t *testing.T) {
	body := openPRBody(prFacts{Gated: true, Level: "reviewed", By: "agent"})
	if !strings.Contains(body, "[drive] merge_gate exit 0") {
		t.Errorf("the body does not say what passed:\n%s", body)
	}
	for _, leak := range []string{"/Users/", "/home/", "/private/tmp", "/var/folders"} {
		if strings.Contains(body, leak) {
			t.Errorf("the body carries a local path (%s):\n%s", leak, body)
		}
	}
	// And no closing keyword: finish closes on a recorded disposition, and GitHub
	// reads a list behind one keyword as a single reference.
	withIssues := openPRBody(prFacts{Gated: true, Level: "reviewed", By: "agent", Closes: []int{12, 14}})
	for _, kw := range []string{"Closes #", "Fixes #", "Resolves #"} {
		if strings.Contains(withIssues, kw) {
			t.Errorf("the body carries %q, which closes an issue no reviewer judged:\n%s", kw, withIssues)
		}
	}
	if !strings.Contains(withIssues, "#12, #14") {
		t.Errorf("the body does not reference the issues the item claims:\n%s", withIssues)
	}
}

// auditNoteOnlyRows: three review passes, each carrying a verified audit by an
// independent reviewer and no changes row, each sending its findings back in the
// audit note alone. A ledger-only reading passes every one of them, and the
// first would merge a stale supported-versions table. The pass row is what
// open-pr reads instead, and it is the row drive computes for exactly this.
func TestOpenPRTrustsThePassRowOverTheLedgerReading(t *testing.T) {
	s := driveProject(t)
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	if err := os.WriteFile(path, []byte(auditNoteOnlyRows+
		`{"id":"1","review_result":"no_digest","date":"2026-09-11"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the fixture history at the fixture's own item, so the rows are what is
	// under test rather than the id.
	b, err := os.ReadFile(path) // #nosec G304 -- the fixture's own ledger
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), `"id":"E24"`, `"id":"1"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openIn(t, s.cfg.Root)

	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	err = s.reviewPassed(it)
	if err == nil {
		t.Fatal("a recorded pass with no request for changes opened a pull request; the ledger reading would have passed it")
	}
	if Code(err) != ExitRefused {
		t.Errorf("refusal exited %d, want %d: %v", Code(err), ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "no_digest") {
		t.Errorf("the refusal does not name what the review read:\n%v", err)
	}
}

// The manual lifecycle records no pass row, and refusing it outright would refuse
// the lifecycle docs/lifecycle.md documents. What it must not do is answer as
// though a drive had spoken: the reading cannot see a session that died, and the
// output says so.
func TestOpenPRWithoutADriveNamesTheAuthorityThatAnswered(t *testing.T) {
	s := driveProject(t)
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	// A converged manual history: a review that sent work back, a re-run, then a
	// review that recorded an audit and nothing else.
	for _, row := range []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-18"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-18"},
		{ID: "1", Changes: "abc123", Date: "2026-09-18"},
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-19"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: bodyAtLaunch, Date: "2026-09-20"},
		{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-20"},
	} {
		if err := ledger.Append(path, stamp(t, s, row)); err != nil {
			t.Fatal(err)
		}
	}
	s = openIn(t, s.cfg.Root)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}

	out := captureErr(t, func() {
		if err := s.reviewPassed(it); err != nil {
			t.Fatalf("an independent review over a recorded request for changes must pass: %v", err)
		}
	})
	if !strings.Contains(out, "the ledger answered") {
		t.Errorf("the fallback does not name which authority spoke:\n%s", out)
	}
	if !strings.Contains(out, "died") {
		t.Errorf("the fallback does not say what it cannot see:\n%s", out)
	}
}

// bodyAtLaunch stands in a fixture row for the digest of the item body as the
// fixture wrote it, which stamp fills in once the file exists.
const bodyAtLaunch = "at-launch"

func stamp(t *testing.T, s *session, row ledger.Row) ledger.Row {
	t.Helper()
	if row.Body != bodyAtLaunch {
		return row
	}
	it, err := s.item(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Body, err = bodyDigest(it.Path); err != nil {
		t.Fatal(err)
	}
	return row
}

// unrequestedPassRows is a ledger holding one drive round that recorded a pass with no request
// pass, two manual rounds that each ended in a changes row, and a manual review
// that recorded a verified audit and nothing after it.
func unrequestedPassRows() []ledger.Row {
	return []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-21"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-21"},
		{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-21"},
		{ID: "1", Pass: passChangesUnrecorded, Date: "2026-09-21"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-21"},
		{ID: "1", Changes: "first", Date: "2026-09-21"},
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-22"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-22"},
		{ID: "1", Changes: "second", Date: "2026-09-22"},
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-22"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: bodyAtLaunch, Date: "2026-09-22"},
		{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-22"},
	}
}

// reviewPassedOver runs reviewPassed against a ledger holding rows, in a
// project whose config carries extra, and returns what it printed alongside the
// answer.
func reviewPassedOver(t *testing.T, extra string, rows []ledger.Row) (stderr string, err error) {
	t.Helper()
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n"+extra)
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	for _, row := range rows {
		if err := ledger.Append(path, stamp(t, s, row)); err != nil {
			t.Fatal(err)
		}
	}
	s = openIn(t, s.cfg.Root)
	it, ierr := s.item("1")
	if ierr != nil {
		t.Fatal(ierr)
	}
	stderr = captureErr(t, func() { err = s.reviewPassed(it) })
	return stderr, err
}

// setAside is what every answer reached past unrequestedPassRows' pass row has to carry: the
// pass drive computed, and that the ledger is what answered instead.
var setAside = []string{"set aside the drive pass", passChangesUnrecorded, "2026-09-21", "the ledger answered"}

// A drive pass followed by manual reviews must not read as a judgment about a
// commit the branch has moved past, which no manual review could clear. The
// later review is what the ledger reads, and the output says which pass it set
// aside so a reader can tell a pass drive computed from one the ledger
// inferred.
func TestALaterManualReviewOutranksAStalePass(t *testing.T) {
	out, err := reviewPassedOver(t, "", unrequestedPassRows())
	if err != nil {
		t.Fatalf("B31's converged manual review must pass: %v", err)
	}
	for _, want := range append(setAside, "audit verified --code-by agent", "died") {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not name %q:\n%s", want, out)
		}
	}
}

// The cap bounds how many rounds a review may spend, not whether a human may
// open a pull request about what shipped. `drover request-changes`'s own refusal
// at the cap tells the reviewer to file the finding and let the work merge, so a
// second gate refusing the pull request on the same count would contradict the
// instruction the reviewer had just followed: findings filed as asked, and their
// items still unable to merge.
func TestAnItemAtTheLimitCanStillOpenAPullRequest(t *testing.T) {
	out, err := reviewPassedOver(t, "\n[review]\nmax_review_rounds = 2\n", unrequestedPassRows())
	if err != nil {
		t.Fatalf("an item at the cap with a converged review must propose: %v", err)
	}
	if strings.Contains(out, "max_review_rounds") {
		t.Errorf("the cap still reaches propose:\n%s", out)
	}
	if !strings.Contains(out, "the ledger answered") {
		t.Errorf("the output does not say which authority decided:\n%s", out)
	}
}

// Setting the pass aside hands the answer to the fallback with every refusal it
// carries, not to a weaker reading, and each of those refusals still names the
// pass it set aside: a reader of one cannot otherwise tell it from a project
// drive never ran in.
func TestAStalePassHandsTheFallbackItsOwnRefusals(t *testing.T) {
	authored := unrequestedPassRows()
	authored[len(authored)-1].Independent = false
	selfGraded := unrequestedPassRows()
	selfGraded[len(selfGraded)-1].Level = ledger.LevelClaimed
	for _, c := range []struct {
		name, extra, want string
		rows              []ledger.Row
	}{{
		name: "the final window holds a request for changes",
		want: "sent work back",
		rows: append(unrequestedPassRows(), ledger.Row{ID: "1", Changes: "third", Date: "2026-09-22"}),
	}, {
		name: "an implement row follows the final review",
		want: "no review has looked at it since",
		rows: append(unrequestedPassRows(), ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-22"}),
	}, {
		name: "the final audit was recorded by the author",
		want: `recorded by whoever wrote the code (--code-by "agent")`,
		rows: authored,
	}, {
		name: "the final audit is weaker than review",
		want: `the latest review's audit row reads "claimed"`,
		rows: selfGraded,
	}, {
		name: "no body digest to read the review against",
		want: "no body digest exists",
		rows: append(unrequestedPassRows()[:4],
			ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-22"},
			ledger.Row{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-22"}),
	}, {
		name: "the body changed outside drover",
		want: "changed outside drover",
		rows: append(unrequestedPassRows()[:4],
			ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: "0123456789ab", Date: "2026-09-22"},
			ledger.Row{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-22"}),
	}} {
		t.Run(c.name, func(t *testing.T) {
			_, err := reviewPassedOver(t, c.extra, c.rows)
			if err == nil {
				t.Fatal("a stale pass opened a pull request no review accepted")
			}
			if Code(err) != ExitRefused {
				t.Errorf("exited %d, want %d: %v", Code(err), ExitRefused, err)
			}
			for _, want := range append(setAside, c.want) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}

// A first review that found nothing, over a body drover recorded at its launch
// and nobody has touched since, is a pass: nothing has to have been sent back
// first.
func TestACleanFirstReviewOpensAPullRequestWithoutForce(t *testing.T) {
	out, err := reviewPassedOver(t, "", []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-10-01"},
		{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: bodyAtLaunch, Date: "2026-10-01"},
		{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-10-01"},
	})
	if err != nil {
		t.Fatalf("a clean first review over an untouched body was refused: %v", err)
	}
	if !strings.Contains(out, "the ledger answered") {
		t.Errorf("the output does not say which authority decided:\n%s", out)
	}
}

// A stale pass with no review after it has no later review to read, so it
// refuses as stale and sets nothing aside.
func TestAStalePassNobodyReviewedStillRefusesAsStale(t *testing.T) {
	_, err := reviewPassedOver(t, "",
		append(unrequestedPassRows()[:4], ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-22"}))
	if Code(err) != ExitRefused {
		t.Fatalf("exited %d, want %d: %v", Code(err), ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "moved past") || strings.Contains(err.Error(), "set aside") {
		t.Errorf("the refusal is not the stale-pass one:\n%v", err)
	}
}

// The window the fallback reasons over is the latest review's own rows, and
// without a review stage row there is no window. Counts cannot substitute: a
// lifetime request for changes total and an audit level both read as acceptance when the last
// review sent the work straight back.
func TestTheFallbackNeedsAReviewItCanDelimit(t *testing.T) {
	for _, c := range []struct {
		name, want string
		rows       []ledger.Row
	}{{
		name: "no review stage row at all",
		want: "no review stage row exists",
		rows: []ledger.Row{
			{ID: "1", Changes: "abc123", Date: "2026-09-18"},
			{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-20"},
		},
	}, {
		name: "the latest review sent back",
		want: "sent work back",
		rows: []ledger.Row{
			{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-20"},
			{ID: "1", Changes: "still-unmet", Date: "2026-09-20"},
			{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-20"},
		},
	}, {
		name: "it ran again after the last review",
		want: "no review has looked at it since",
		rows: []ledger.Row{
			{ID: "1", Changes: "abc123", Date: "2026-09-17"},
			{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-18"},
			{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-18"},
			{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: "2026-09-20"},
		},
	}, {
		// Not a stage drover writes today. A stage added later must refuse here
		// on the strength of being a launch, rather than passing because this
		// window only knew the two names that existed when it was written.
		name: "a stage nobody here has heard of ran after the last review",
		want: "no review has looked at it since",
		rows: []ledger.Row{
			{ID: "1", Changes: "abc123", Date: "2026-09-17"},
			{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-18"},
			{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-18"},
			{ID: "1", Stage: "rehearse", Runner: "impl", Date: "2026-09-20"},
		},
	}, {
		name: "the latest review recorded nothing, an older one audited",
		want: "audit row reads",
		rows: []ledger.Row{
			{ID: "1", Changes: "abc123", Date: "2026-09-17"},
			{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-18"},
			{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-09-18"},
			{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-20"},
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			s := driveProject(t)
			path := filepath.Join(s.cfg.Root, "ledger.jsonl")
			for _, row := range c.rows {
				if err := ledger.Append(path, row); err != nil {
					t.Fatal(err)
				}
			}
			s = openIn(t, s.cfg.Root)
			it, err := s.item("1")
			if err != nil {
				t.Fatal(err)
			}
			err = s.reviewPassed(it)
			if err == nil {
				t.Fatal("the fallback passed work no review accepted")
			}
			if Code(err) != ExitRefused {
				t.Errorf("exited %d, want %d: %v", Code(err), ExitRefused, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why:\n%v", err)
			}
		})
	}
}

// The body names the item. A pull request is read by people holding the branch
// name and nothing else, and the branch name is a slug.
func TestTheBodyNamesTheItemItIsAbout(t *testing.T) {
	s := driveProject(t)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	body := openPRBody(s.factsFor(it))
	for _, want := range []string{"1", it.Meta["title"]} {
		if !strings.Contains(body, want) {
			t.Errorf("the body omits %q:\n%s", want, body)
		}
	}
	// Identity comes from the item, not from a constant: a body assembled without
	// one has to say so rather than invent a name.
	if bare := openPRBody(prFacts{Gated: true}); strings.Contains(bare, it.Meta["title"]) {
		t.Errorf("the body invented an identity nobody passed:\n%s", bare)
	}
}

// Every other test here seeds a pass row by hand or folds rows already in
// memory, so deleting the writer would leave the suite green. This drives the
// real command and reads the file back through a fresh session, which is the
// only reader that proves the row reached disk.
func TestADriveWritesThePassRowToDisk(t *testing.T) {
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	driveAll(t)

	// A fresh reader, not s.ledger: session.record appends to the loaded copy, so
	// asserting against it would pass with no file write at all.
	fresh := openIn(t, dir)
	for _, c := range []struct{ id, want string }{
		{"1", passPassed},
		{"2", passSilent},
	} {
		token, _, stale, ok := fresh.ledger.Pass(c.id)
		if !ok {
			t.Errorf("%s: the drive recorded no pass row on disk", c.id)
			continue
		}
		if token != c.want {
			t.Errorf("%s: disk says %q, want %q", c.id, token, c.want)
		}
		if stale {
			t.Errorf("%s: the row reads stale with nothing appended after it", c.id)
		}
	}

	// And it goes stale when the item runs again, read back the same way.
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	if _, _, stale, _ := openIn(t, dir).ledger.Pass("1"); !stale {
		t.Error("a launch after the pass row leaves the result current")
	}
}

// openPRReady is the manual lifecycle standing where open-pr is run: the item's
// branch checked out with work on it, and the pass row a drive would have left.
func openPRReady(t *testing.T) (dir, branch, ghLog string) {
	t.Helper()
	ghLog = mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if branch, err = s.branchFor(it); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, "shipped.txt"), "the work\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "the work")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "1", Pass: passPassed, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	return dir, branch, ghLog
}

// The gate asks the forge about a commit, and the forge knows only what has been
// pushed. Without the push a manual proposal either strands the branch nobody can
// see, or passes a gate run against a remote head older than the one that was
// reviewed — and the pull request names the older one with nothing saying so.
func TestOpenPRPublishesTheHeadTheGateReadsAbout(t *testing.T) {
	for _, c := range []struct {
		name    string
		advance bool
	}{
		{"the remote has never seen the branch", false},
		// What a FIX-IN-REVIEW commit leaves behind: pushed once, then moved.
		{"the remote is behind the reviewed head", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, branch, ghLog := openPRReady(t)
			if c.advance {
				git(t, dir, "push", "-q", "-u", "origin", branch)
				git(t, dir, "commit", "-q", "--allow-empty", "-m", "review: a typo")
			}

			var err error
			out := captureErr(t, func() { err = execute(t, "open-pr", "1") })
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			seen := gateLog(t, dir)
			if len(seen) != 1 {
				t.Fatalf("the merge gate ran %d times, want once: %v", len(seen), seen)
			}
			b, local, remote := splitGateLine(t, seen[0])
			if b != branch {
				t.Errorf("the merge gate was asked about %q, want %q", b, branch)
			}
			if remote == "none" {
				t.Fatal("the merge gate ran against a branch the forge has never seen")
			}
			if local != remote {
				t.Errorf("the merge gate read %s while the forge holds %s; the pull request would name a commit nobody reviewed",
					local, remote)
			}
			if n := prCreates(t, ghLog); len(n) != 1 {
				t.Errorf("opened %d pull requests, want 1: %v", len(n), n)
			}
			// Which authority answered, for the same reason the fallback says so:
			// a persisted pass and a ledger reading are different evidence.
			if !strings.Contains(out, "recorded drive pass") {
				t.Errorf("the output does not name the authority that answered:\n%s", out)
			}
		})
	}
}

// A rejected push is an answer. Nothing downstream of it may run: a gate against
// a stale remote head reports on code nobody proposed, and a pull request over it
// claims a commit the forge does not have.
func TestAProposalThatCannotPushReachesNeitherGateNorForge(t *testing.T) {
	dir, branch, ghLog := openPRReady(t)
	remote := git(t, dir, "remote", "get-url", "origin")
	other := t.TempDir()
	git(t, other, "clone", "-q", remote, other)
	git(t, other, "switch", "-q", "-c", branch)
	git(t, other, "commit", "-q", "--allow-empty", "-m", "somebody else was here")
	git(t, other, "push", "-q", "origin", branch)

	err := execute(t, "open-pr", "1")
	if err == nil {
		t.Fatal("a branch drover could not push was proposed anyway")
	}
	if !strings.Contains(err.Error(), "would not push") {
		t.Errorf("the error does not name what failed: %v", err)
	}
	if seen := gateLog(t, dir); len(seen) != 0 {
		t.Errorf("the merge gate ran against a commit the forge does not have: %v", seen)
	}
	if n := prCreates(t, ghLog); len(n) != 0 {
		t.Errorf("a branch drover could not push still got a pull request: %v", n)
	}
	if out := git(t, other, "log", "-1", "--format=%s", "origin/"+branch); out != "somebody else was here" {
		t.Errorf("origin/%s now reads %q; the push overwrote somebody else's commit", branch, out)
	}
}

// --force substitutes the operator's judgement for drover's evidence. It cannot
// substitute for a forge, so the refusal sits above it and says which of the two
// it is.
func TestOpenPRRefusesAForgeItCannotReach(t *testing.T) {
	for _, args := range [][]string{{"open-pr", "1"}, {"open-pr", "1", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			openPRReady(t)
			bin := t.TempDir()
			stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\nexit 1\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			err := execute(t, args...)
			if Code(err) != ExitRefused {
				t.Fatalf("exited %d, want %d: %v", Code(err), ExitRefused, err)
			}
			if !strings.Contains(err.Error(), "gh auth login") {
				t.Errorf("the refusal does not name the next step: %v", err)
			}
		})
	}
}

// A drive parks a branch touching [drive] protected_paths because the merge it would
// perform is unattended. Here a person is about to read the diff, and the pull
// request is what they read it in: withholding it takes away the review of the
// one change the loop is not allowed to make for them.
func TestAProposalNamesADeniedPathAndOpensAnyway(t *testing.T) {
	dir, _, ghLog := openPRReady(t)
	// [drive] is the last table the fixture writes, so this lands inside it.
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("protected_paths = [\"deny.txt\"]\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "deny.txt"), "this decides what every later item is measured by\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "touch what a drive may not merge")

	var runErr error
	out := capture(t, func() { runErr = execute(t, "open-pr", "1") })
	if runErr != nil {
		t.Fatalf("propose: %v", runErr)
	}
	for _, want := range []string{"deny.txt", "protected_paths"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not name %q:\n%s", want, out)
		}
	}
	if n := prCreates(t, ghLog); len(n) != 1 {
		t.Errorf("opened %d pull requests, want 1: a human still has to read this diff somewhere", len(n))
	}
}

// overloaded is the terminal event an implementing run leaves on an API
// overload: Claude Code reports an HTTP failure as subtype "success" with
// is_error true, and names the API only in terminal_reason and in the prose.
const overloaded = `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"terminal_reason":"api_error",` +
	`"result":"API Error: 529 Overloaded. This is a server-side issue, usually temporary - try again in a moment."}`

// quotaSpent is what codex leaves on a review that ran out of quota. It names
// the instant it will serve again, which is the one fact a wait needs.
const quotaSpent = `{"type":"turn.failed","error":{"message":"You have hit your usage limit. ` +
	`Visit https://example.invalid/usage to purchase more credits or try again at Sep 22nd, 2026 10:03 AM."}}`

// The same failure with no instant in it. codex names one; Claude Code's own
// quota message does not always, and a wait drover cannot bound is a wait it
// does not take.
const quotaSpentNoInstant = `{"type":"result","subtype":"success","is_error":true,"num_turns":1,` +
	`"terminal_reason":"api_error","result":"API Error: 429 You have hit your usage limit."}`

func faultResult(t *testing.T, line string) runlog.Result {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := runlog.ReadResult(p)
	if err != nil || !rd.Found {
		t.Fatalf("the fixture does not parse: found=%v err=%v", rd.Found, err)
	}
	return rd.Result
}

// A fault is not a session that died, and the difference is the whole park an
// operator reads at nine in the morning: one sends them to the log hunting a
// crash, the other names the service and the instant it comes back.
func TestAFaultedReviewDoesNotReadAsADeadSession(t *testing.T) {
	for _, c := range []struct {
		name, line string
	}{
		{"the service was overloaded", overloaded},
		{"the account is out of budget", quotaSpent},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := pass{before: passCounts(1, 1, false), after: passCounts(1, 1, false),
				logged: true, res: faultResult(t, c.line)}
			if got := passResultBeforeFaults(p); got != passDied {
				t.Fatalf("before this change the rows read as %q, want %q; the regression claim is wrong", got, passDied)
			}
			if got := passResult(p); got != passFaulted {
				t.Errorf("passResult = %q, want %q", got, passFaulted)
			}
		})
	}
}

// A reviewer that ran and failed on its own work is not a fault: the session
// produced a result about the code, and relaunching it would overwrite one.
func TestAReviewThatFailedOnItsOwnWorkStillReadsAsDied(t *testing.T) {
	p := pass{before: passCounts(1, 1, false), after: passCounts(1, 1, false), logged: true,
		res: runlog.Result{Subtype: "error_max_turns", IsError: true, Text: "reached the turn limit"}}
	if got := passResult(p); got != passDied {
		t.Errorf("passResult = %q, want %q", got, passDied)
	}
}

// passResultBeforeFaults is the predicate without passFaulted. The claim under
// test is a regression claim, so the naive reading runs beside the real one,
// for the reason naivePassResult already gives.
func passResultBeforeFaults(p pass) string {
	audits := p.after.Audits - p.before.Audits
	changes := p.after.ChangesRequested - p.before.ChangesRequested
	switch {
	case !p.logged && p.events > 0 && p.known == 0:
		return passUnreadable
	case !p.logged || !p.res.Succeeded():
		return passDied
	case audits == 0 && changes == 0:
		return passSilent
	case p.cap > 0 && p.after.ChangesRequested >= p.cap:
		return passCapped
	case changes >= 1 && p.after.RepeatedChanges:
		return passRepeating
	case changes >= 1:
		return passChangesRequested
	case audits >= 1 && p.after.ChangesRequested == 0:
		return passChangesUnrecorded
	case audits >= 1:
		return passPassed
	}
	return passSilent
}

func TestFaultWait(t *testing.T) {
	now := time.Date(2026, time.September, 21, 18, 0, 0, 0, time.Local)
	transient := runlog.Result{Fault: runlog.FaultTransient}
	quota := func(at time.Time) runlog.Result {
		return runlog.Result{Fault: runlog.FaultQuota, RetryAt: at}
	}
	for _, c := range []struct {
		name     string
		res      runlog.Result
		attempt  int
		deadline time.Time
		want     time.Duration
	}{
		{"the first transient failure backs off",
			transient, 0, now.Add(8 * time.Hour), faultBackoff},
		{"the second backs off twice as far",
			transient, 1, now.Add(8 * time.Hour), 2 * faultBackoff},
		{"the third does not, because two said it already",
			transient, 2, now.Add(8 * time.Hour), 0},
		{"a backoff that would outlast the drive is not spent",
			transient, 0, now.Add(10 * time.Second), 0},
		{"an unbounded drive still retries a transient failure",
			transient, 0, time.Time{}, faultBackoff},
		{"a quota reset inside the bound is waited out",
			quota(now.Add(90 * time.Minute)), 0, now.Add(8 * time.Hour), 90*time.Minute + quotaSlack},
		{"a reset past the bound stops the drive instead",
			quota(now.Add(9 * time.Hour)), 0, now.Add(8 * time.Hour), 0},
		{"an unbounded drive never sleeps on a quota: nobody granted the night",
			quota(now.Add(90 * time.Minute)), 0, time.Time{}, 0},
		{"credentials the service will not accept are never waited on",
			runlog.Result{Fault: runlog.FaultAuth}, 0, now.Add(8 * time.Hour), 0},
		{"a quota naming no instant stops rather than picking a number",
			quota(time.Time{}), 0, now.Add(8 * time.Hour), 0},
		{"a reset already past is not a wait",
			quota(now.Add(-time.Minute)), 0, now.Add(8 * time.Hour), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, why := faultWait(c.res, c.attempt, c.deadline, now)
			if got != c.want {
				t.Errorf("faultWait = %v, want %v", got, c.want)
			}
			if (got > 0) != (why != "") {
				t.Errorf("a wait of %v is explained by %q", got, why)
			}
		})
	}
}

// The park is the whole product of a fault, so it says which side failed, what
// the runner itself said, and where the evidence is.
func TestFaultReasonQuotesTheRunner(t *testing.T) {
	oc := runner.Outcome{Result: faultResult(t, quotaSpent), LogPath: "/logs/1-x.jsonl"}
	got := faultReason("codex", oc.Result, oc.LogPath)
	for _, want := range []string{"codex", "out of budget", "usage limit", "/logs/1-x.jsonl", "2026-09-22T10:03:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("the park does not carry %q:\n%s", want, got)
		}
	}
}

// A drive that walks every item through a 529 spends a branch, a push and two
// dead sessions on each. A fault is true of the next item as well, which is
// the reading runGate already applies to a gate that will not run.
func TestADriveStopsAtTheFirstRunnerFault(t *testing.T) {
	s := driveProject(t)
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it too.\n")
	git(t, s.cfg.Root, "add", "-A")
	git(t, s.cfg.Root, "commit", "-q", "-m", "second item")

	// The quota stream rather than the overloaded one: a transient failure is
	// answered by waiting, and faultWait holds the arithmetic for that. This is
	// the arm with nothing left to wait for, which is where the stop lives.
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\ncat <<'STREAM'\n"+quotaSpentNoInstant+"\nSTREAM\n")
	t.Setenv("DROVER_TEST_REV_STDERR", "the review ran against a run that never happened")
	stub(t, filepath.Join(bin, "stub-rev"), revRefuses)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var err error
	out := capture(t, func() { err = execute(t, "drive", "--json") })
	if !errors.Is(err, errRunnerFault) {
		t.Fatalf("drive returned %v, want %v", err, errRunnerFault)
	}

	var rows []DriveItem
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("drive reported %d items, want 1: the second was never worth launching\n%s", len(rows), out)
	}
	d := rows[0]
	if d.ID != "1" || d.Outcome != outcomeParked || d.Park != "runner_fault" {
		t.Errorf("got %s %s/%s, want 1 parked/runner_fault", d.ID, d.Outcome, d.Park)
	}
	if !strings.Contains(d.Reason, "usage limit") {
		t.Errorf("the park does not carry what the runner said: %q", d.Reason)
	}

	// The review is the expensive half, and a run that never reached the work
	// has nothing for it to read.
	b, readErr := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	ledgerText := string(b)
	if !strings.Contains(ledgerText, `"stage":"implement"`) {
		t.Error("the implementing launch went unrecorded")
	}
	if strings.Contains(ledgerText, `"stage":"review"`) {
		t.Error("a review was launched against a run that faulted before it did anything")
	}
	if strings.Contains(ledgerText, `"id":"2"`) {
		t.Error("the drive walked on to the next item after a fault the next item would meet too")
	}
	// Nothing to push: the branch carries no commit, and the forge would be
	// answering about a head that means nothing.
	if refs := git(t, s.cfg.Root, "ls-remote", "--heads", "origin"); strings.Contains(refs, "first") {
		t.Errorf("a faulted run still pushed its branch:\n%s", refs)
	}
	// The report file is the item's saved self-report, read by every later run.
	if _, statErr := os.Stat(filepath.Join(s.cfg.Root, "reports", "01-first.md")); statErr == nil {
		t.Error("a result was captured from a run that never reached the work")
	}
}

// Claude Code's weekly-limit refusal, as F14's review log recorded it: the
// reset stated once as an epoch on the rate_limit_event and once as prose with
// no year. The epoch is 2026-10-25T20:00:00-07:00.
const (
	weeklyLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792983600,"rateLimitType":"seven_day"}}`
	weeklyLimitHit   = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error",` +
		`"result":"You've hit your weekly limit · resets Oct 25 at 8pm (America/Los_Angeles)"}`
)

// A week-long window resets more than a day out, which is past what --until
// can name, so --for is the bound that has to cover it.
func TestFaultWaitOnClaudesWeeklyReset(t *testing.T) {
	res := faultResult(t, weeklyLimitEvent+"\n"+weeklyLimitHit)
	reset := time.Unix(1792983600, 0)
	if res.Fault != runlog.FaultQuota || !res.RetryAt.Equal(reset) {
		t.Fatalf("fault %q at %v, want %q at %v", res.Fault, res.RetryAt, runlog.FaultQuota, reset)
	}
	now := time.Date(2026, time.October, 24, 17, 0, 0, 0, time.UTC)
	if got, _ := faultWait(res, 0, now.Add(48*time.Hour), now); got != reset.Sub(now)+quotaSlack {
		t.Errorf("under --for 48h faultWait = %v, want %v", got, reset.Sub(now)+quotaSlack)
	}
	if got, _ := faultWait(res, 0, now.Add(8*time.Hour), now); got != 0 {
		t.Errorf("under --for 8h faultWait = %v, want 0: the reset is past the bound", got)
	}
}

// The same refusal end to end, at usage_admission = "off": the park stands as
// it did before the action existed, naming the event's reset.
func TestADriveParksOnAWeeklyResetPastItsBoundWhenAdmissionIsOff(t *testing.T) {
	driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[supervise]\nusage_admission = \"off\"\n")
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"printf '%s\\n' \"{\\\"type\\\":\\\"rate_limit_event\\\",\\\"rate_limit_info\\\":{\\\"status\\\":\\\"rejected\\\",\\\"resetsAt\\\":$(($(date +%s)+172800))}}\"\n"+
		"cat <<'STREAM'\n"+weeklyLimitHit+"\nSTREAM\n")
	stub(t, filepath.Join(bin, "stub-rev"), revRefuses)
	t.Setenv("DROVER_TEST_REV_STDERR", "the review ran against a run that never happened")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var err error
	out := capture(t, func() { err = execute(t, "drive", "--json", "--for", "1h", "--no-merge", "1") })
	if !errors.Is(err, errRunnerFault) {
		t.Fatalf("drive returned %v, want %v\n%s", err, errRunnerFault, out)
	}
	// Two days out is the event's epoch; the prose would name October 25th.
	day := time.Now().Add(48 * time.Hour).Format("2006-01-02")
	if !strings.Contains(out, `"park":"runner_fault"`) || !strings.Contains(out, "It serves again at "+day) {
		t.Errorf("the item did not park runner_fault naming the event's reset on %s:\n%s", day, out)
	}
}

// The other half of a quota: a reset the operator's own bound covers is waited
// out and the item carries on, rather than costing a park and a resume somebody
// has to type in the morning.
func TestADriveWaitsOutAQuotaInsideItsBound(t *testing.T) {
	backoff, slack := faultBackoff, quotaSlack
	t.Cleanup(func() { faultBackoff, quotaSlack = backoff, slack })
	faultBackoff, quotaSlack = 10*time.Millisecond, 0

	s := driveProject(t)
	bin := t.TempDir()
	// Out of budget on the first launch, naming an instant two seconds out;
	// working on the second. The epoch form is Claude Code's own.
	t.Setenv("DROVER_TEST_IMPL_COUNT", filepath.Join(bin, "n"))
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"n=$(cat \"$DROVER_TEST_IMPL_COUNT\" 2>/dev/null || echo 0); echo $((n+1)) > \"$DROVER_TEST_IMPL_COUNT\"\n"+
		"if [ \"$n\" = 0 ]; then\n"+
		"  printf '%s\\n' \"{\\\"type\\\":\\\"result\\\",\\\"subtype\\\":\\\"success\\\",\\\"is_error\\\":true,\\\"num_turns\\\":1,\\\"terminal_reason\\\":\\\"api_error\\\",\\\"result\\\":\\\"Claude AI usage limit reached|$(($(date +%s)+2))\\\"}\"\n"+
		"else\n"+
		// It commits: a run that finishes and leaves the branch at the trunk
		// parks no_commits, which would hide the wait this test is about.
		"  echo waited > waited.txt\n"+
		"  git add waited.txt\n"+
		"  git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false "+
		"commit -q -m 'after the wait'\n"+
		"  printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":9,\"result\":\"done\"}'\n"+
		"fi\n")
	// A review that records nothing, which parks the item on its own terms.
	// What is under test is everything before that.
	t.Setenv("DROVER_TEST_RESULT_LINE", `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"looked"}`)
	stub(t, filepath.Join(bin, "stub-rev"), printsResultLine)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var err error
	out := capture(t, func() { err = execute(t, "drive", "--json", "--for", "1h", "--no-merge", "1") })
	if errors.Is(err, errRunnerFault) {
		t.Fatalf("the drive stopped on a quota its own bound covered:\n%s", out)
	}
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"id":"1"`) || strings.Contains(out, `"park":"runner_fault"`) {
		t.Errorf("the item did not carry on past the wait:\n%s", out)
	}
	b, readErr := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.Count(string(b), `"stage":"implement"`); got != 2 {
		t.Errorf("implement rows = %d, want 2: the wait is followed by a relaunch\n%s", got, b)
	}
	if !strings.Contains(string(b), `"stage":"review"`) {
		t.Error("the review never ran, so the wait did not return the item to the loop")
	}
}

// hookOnlyEvents is a captured implement log, verbatim but for the trimmed hook
// payloads: three events, every one a SessionStart hook row, and no result
// among them. A drive that reads none of it launches a reviewer against a
// branch with no commits past main, and parks on the reviewer's silence.
const hookOnlyEvents = `{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}
{"type":"system","subtype":"hook_progress","hook_name":"SessionStart:startup"}
{"type":"system","subtype":"hook_response","hook_name":"SessionStart:startup"}
`

// The two readings a log states on its own, applied to the stage that wrote it.
// passResult cannot answer for an implement run: every row below these two is
// a ledger delta about a review, and an implementation writes neither an audit
// nor a request for changes.
func TestRunResultReadsTheImplementLogAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "B29.20260921-101413.000000000.jsonl")
	if err := os.WriteFile(p, []byte(hookOnlyEvents), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := runlog.ReadResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Found {
		t.Fatalf("the fixture must carry no terminal event; got %+v", rd.Result)
	}
	if rd.Events != 3 || rd.Known != 3 {
		t.Fatalf("events=%d known=%d, want 3 and 3: claimed events with no terminal one is a session that stopped",
			rd.Events, rd.Known)
	}

	stopped := pass{res: rd.Result, logged: rd.Found, events: rd.Events, known: rd.Known}
	if got := runResult(stopped); got != passDied {
		t.Errorf("runResult = %q, want %q", got, passDied)
	}
	// The park an operator reads has to send them to the implementer's log,
	// not the reviewer's, which never held the cause.
	sentence := passSentence(passDied, ledger.StageImplement, stopped, "the implement run", "claude", p)
	for _, want := range []string{"the implement run", "runner claude", p} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the park sentence does not name %q:\n%s", want, sentence)
		}
	}

	// A clean run is not the implement stage's business to report.
	if got := runResult(pass{logged: true, res: ok}); got != "" {
		t.Errorf("runResult on a run that finished = %q, want empty", got)
	}
	// And the rows passResult adds below these two stay out of it: an
	// implement run records no audit and no request for changes, so reading it through
	// passResult would park every clean one as silent.
	if got := passResult(pass{logged: true, res: ok}); got != passSilent {
		t.Fatalf("passResult on the same run = %q, want %q: the fixture proves nothing otherwise", got, passSilent)
	}
}

// A denial is the cause, never the outcome. Naming it on a token that already
// read badly answers the operator's question; naming it anywhere else would
// fire on most runs this project makes, since a run that loses one command to a
// prompt and then finishes is ordinary.
func TestOnlyADeadOrSilentPassNamesTheDenial(t *testing.T) {
	denied := runlog.Result{Subtype: "success", Denied: []string{"RunCommand"}}
	const cause = "the runner denied RunCommand"

	for _, c := range []struct {
		token string
		p     pass
		named bool
	}{
		{passSilent, pass{logged: true, res: denied}, true},
		// Parsed from a stream rather than hand-built. A hand-built
		// pass{res: denied}, logged false with denials present, is a state
		// runlog.ReadResult cannot produce: a result carrying denials is a result
		// whose terminal event was found, so no run reaches it.
		{passDied, pass{logged: true, res: deniedFromLog(t)}, true},
		{passPassed, pass{logged: true, res: denied}, false},
		{passChangesRequested, pass{logged: true, res: denied}, false},
		{passUnreadable, pass{events: 40, res: denied}, false},
		{passChangesUnrecorded, pass{logged: true, res: denied}, false},
		{passCapped, pass{logged: true, res: denied, cap: 2}, false},
		{passRepeating, pass{logged: true, res: denied}, false},
	} {
		t.Run(c.token, func(t *testing.T) {
			got := passSentence(c.token, ledger.StageReview, c.p, "the review", "agy", "/tmp/x.jsonl")
			if strings.Contains(got, cause) != c.named {
				t.Errorf("names the denial = %v, want %v:\n%s", !c.named, c.named, got)
			}
			if got == "" || strings.HasPrefix(got, "unrecognized") {
				t.Errorf("%s has no sentence: %q", c.token, got)
			}
		})
	}

	// Unchanged where the runner denied nothing, which is every ordinary pass.
	quiet := passSentence(passSilent, ledger.StageReview, pass{logged: true, res: runlog.Result{Subtype: "success"}}, "the review", "agy", "")
	if strings.Contains(quiet, "denied") {
		t.Errorf("a pass with no denial gained a cause clause:\n%s", quiet)
	}
}

// Every token drive and review key on has to be worded somewhere, or an
// operator reads a bare word. A token added later fails here rather than in
// front of somebody at nine in the morning.
func TestEveryPassTokenIsWorded(t *testing.T) {
	for _, token := range []string{
		passDied, passUnreadable, passSilent, passCapped,
		passRepeating, passChangesRequested, passChangesUnrecorded, passPassed,
		passFaulted,
	} {
		got := passSentence(token, ledger.StageReview, pass{}, "the review", "agy", "")
		if got == "" || strings.HasPrefix(got, "unrecognized") {
			t.Errorf("%s: %q", token, got)
		}
	}
}

// An implement run that left no terminal event parks before any reviewer
// launches. Otherwise drive reads none of its log, launches a reviewer against
// a branch with no commits past main, and parks on the reviewer's silence,
// sending the operator to the reviewer's log for a cause that is in the
// implementer's.
func TestDriveParksOnTheImplementRunBeforeSpendingAReviewer(t *testing.T) {
	ghLog := mergeProject(t)
	t.Setenv("DROVER_TEST_IMPL_STOPS", "1")

	rows := driveAll(t)
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
	}
	for _, d := range rows {
		if d.Outcome != outcomeParked || d.Park != parkImplementDied {
			t.Errorf("%s is %q/%q, want %q/%q", d.ID, d.Outcome, d.Park, outcomeParked, parkImplementDied)
		}
		if d.Reviews != 0 {
			t.Errorf("%s launched %d reviews; a dead implement run must not buy one", d.ID, d.Reviews)
		}
		// The sentence has to send the operator to the implementer's log.
		for _, want := range []string{"the implement run", "runner impl", ".jsonl"} {
			if !strings.Contains(d.Reason, want) {
				t.Errorf("%s: the park reason does not name %q:\n%s", d.ID, want, d.Reason)
			}
		}
	}

	// mergeProject chdirs into the project root, so the ledger is beside us.
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if n := l.Counts("1").ByStage[ledger.StageReview]; n != 0 {
		t.Errorf("%d review stage rows for item 1, want 0", n)
	}
	// And nothing reached the forge: no pull request, no merge.
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr create") || strings.HasPrefix(c, "pr merge") {
			t.Errorf("gh was asked %q for an item whose implement run left no result", c)
		}
	}
}

// One function words every pass token, so an outcome reads the same whichever
// command the operator ran. passFaulted is the case where that can drift: drive
// parks through faultReason and carries the fault kind, the runner's own words
// and the retry instant, and a headless `drover review` reaching passSentence
// must carry all three too.
func TestAFaultReadsTheSameFromReviewAsFromADrive(t *testing.T) {
	res := runlog.Result{
		Subtype: "success", IsError: true, Fault: runlog.FaultQuota,
		Text:    "API Error: 429 You have hit your usage limit.",
		RetryAt: time.Date(2026, time.September, 27, 3, 0, 0, 0, time.UTC),
	}
	park := faultReason("agy", res, "/tmp/r.jsonl")
	review := passSentence(passFaulted, ledger.StageReview, pass{res: res}, "the review", "agy", "/tmp/r.jsonl")
	if park != review {
		t.Errorf("the two paths word one outcome differently:\n drive:  %s\n review: %s", park, review)
	}
	for _, want := range []string{"out of budget", "usage limit", "/tmp/r.jsonl", "2026-09-27T03:00:00Z"} {
		if !strings.Contains(review, want) {
			t.Errorf("the review sentence drops %q:\n%s", want, review)
		}
	}
}

// deniedFromLog is a failing terminal event read back through the real parser,
// so a test need not hand-build a pass. A result carrying denials is a result
// whose terminal event was found, and a hand-built pass could claim otherwise,
// asserting about a state no run reaches.
func deniedFromLog(t *testing.T) runlog.Result {
	t.Helper()
	return resultFromLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,`+
		`"permission_denials":[{"tool_name":"RunCommand"}]}`)
}

func resultFromLine(t *testing.T, line string) runlog.Result {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := runlog.ReadResult(p)
	if err != nil || !rd.Found {
		t.Fatalf("the fixture does not parse: found=%v err=%v", rd.Found, err)
	}
	return rd.Result
}

// runResult returns passDied for a log with no terminal event and for a log
// whose terminal event reports a failure. One sentence covering both would
// describe only the first, so a pass carrying a denial clause (which only the
// second can) would contradict its own opening.
func TestTheTwoDeadStatesReadDifferently(t *testing.T) {
	found := pass{logged: true, res: deniedFromLog(t)}
	absent := pass{res: runlog.Result{}}

	foundSentence := passSentence(passDied, ledger.StageReview, found, "the review", "agy", "/tmp/r.jsonl")
	absentSentence := passSentence(passDied, ledger.StageReview, absent, "the review", "agy", "/tmp/r.jsonl")

	if foundSentence == absentSentence {
		t.Fatalf("both dead states word one outcome identically:\n%s", foundSentence)
	}
	if strings.Contains(foundSentence, "left no terminal event") {
		t.Errorf("a found terminal event reads as an absent one:\n%s", foundSentence)
	}
	if !strings.Contains(foundSentence, "error_during_execution") {
		t.Errorf("the sentence does not name what it ended on:\n%s", foundSentence)
	}
	if !strings.Contains(absentSentence, "left no terminal event") {
		t.Errorf("a log with no terminal event should say so:\n%s", absentSentence)
	}
}

// The consequence belongs to the stage. An implement run calls neither `drover
// audit` nor `drover request-changes`, so naming them in its park sends an operator looking
// for a row that stage never writes.
func TestADeniedImplementRunIsNotToldToAudit(t *testing.T) {
	p := pass{logged: true, res: deniedFromLog(t)}
	review := passSentence(passDied, ledger.StageReview, p, "the review", "agy", "/tmp/r.jsonl")
	implement := passSentence(passDied, ledger.StageImplement, p, "the implement run", "claude", "/tmp/i.jsonl")

	if !strings.Contains(review, "`drover audit`") {
		t.Errorf("a review's denial should name the rows it could not write:\n%s", review)
	}
	if strings.Contains(implement, "drover audit") || strings.Contains(implement, "drover request-changes") {
		t.Errorf("an implement run is told to audit:\n%s", implement)
	}
	if !strings.Contains(implement, "could not finish the work") {
		t.Errorf("an implement run's denial does not say what it could not do:\n%s", implement)
	}
	// Both still name the denial itself.
	for _, got := range []string{review, implement} {
		if !strings.Contains(got, "the runner denied RunCommand") {
			t.Errorf("the denial went unnamed:\n%s", got)
		}
	}
}

// awkwardNames are the filenames Git's display form rewrites or a line split
// breaks: each one reaches the matcher wrong if any reader on the way to it
// stops treating a name as bytes.
var awkwardNames = []struct{ kind, name string }{
	{"nonascii", "001_\u00e9.sql"},
	{"newline", "new\nline.sql"},
	{"tab", "tab\there.sql"},
	{"leading", " lead.sql"},
	{"trailing", "trail.sql "},
	{"quote", "quo\"te.sql"},
}

// awkwardTree commits one file per awkward name, each in its own directory,
// on a branch cut from an empty trunk, under Git's default display quoting.
func awkwardTree(t *testing.T) (*session, map[string]string) {
	t.Helper()
	s := driveProject(t)
	dir := s.cfg.Root
	git(t, dir, "config", "core.quotePath", "true")
	git(t, dir, "switch", "-q", "-c", "work")
	files := map[string]string{}
	for _, a := range awkwardNames {
		rel := "held-" + a.kind + "/" + a.name
		if err := os.MkdirAll(filepath.Join(dir, "held-"+a.kind), 0o750); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, filepath.FromSlash(rel)), "x\n")
		git(t, dir, "add", "--", rel)
		files[a.kind] = rel
	}
	git(t, dir, "commit", "-q", "-m", "awkward names")
	return s, files
}

// Every entry form against every awkward name, asserted at the hold the drive
// parks on rather than at the matcher: a matcher that agrees with itself over a
// name the transport already mangled holds nothing.
func TestEveryEntryFormHoldsEveryAwkwardName(t *testing.T) {
	s, files := awkwardTree(t)
	for _, a := range awkwardNames {
		file := files[a.kind]
		forms := map[string]string{
			"directory": "held-" + a.kind + "/",
			"star":      "held-" + a.kind + "*",
			"exact":     file,
			"glob":      "held-" + a.kind + "/*.sql*",
		}
		for form, entry := range forms {
			t.Run(a.kind+"/"+form, func(t *testing.T) {
				s.cfg.Loop.DenyPaths = []string{entry}
				hits, err := s.deniedPaths("work", "main")
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) != 1 || hits[0] != file {
					t.Errorf("entry %q denied %q, want exactly [%q]", entry, hits, file)
				}
				park, reason := s.denyHold("work", "main")
				if park != "protected_paths" {
					t.Fatalf("entry %q parked %q (%s), want protected_paths", entry, park, reason)
				}
				if strings.Contains(reason, "\n") {
					t.Errorf("the reason carries a raw newline, so a name forged a line: %q", reason)
				}
			})
		}
	}
}

// treeProject is driveProject with two items, each launch recorded to a file
// outside the tree so a test can say which items a drive reached.
func treeProject(t *testing.T) (s *session, launches string) {
	t.Helper()
	s = driveProject(t)
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it too.\n")
	git(t, s.cfg.Root, "add", "-A")
	git(t, s.cfg.Root, "commit", "-q", "-m", "second item")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	launches = filepath.Join(bin, "launches")
	t.Setenv("DROVER_TEST_LAUNCHES", launches)
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"git rev-parse --abbrev-ref HEAD >> \"$DROVER_TEST_LAUNCHES\"\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	stub(t, filepath.Join(bin, "stub-rev"),
		"#!/bin/sh\necho '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s, launches
}

// driveErr drives the queue and returns the error it ended on, which for a
// refusal is the point.
func driveErr(t *testing.T) error {
	t.Helper()
	var err error
	capture(t, func() { err = execute(t, "drive", "--json") })
	return err
}

func launched(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- written by the stub
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// mergedLaunched leaves id merged into main from a branch holding a commit of
// its own, with both rows reconciliation requires before it finishes it: the launch row
// that says drover drove it, and the head row that says the branch carried work.
func mergedLaunched(t *testing.T, s *session, id, branch string) {
	t.Helper()
	dir := s.cfg.Root
	git(t, dir, "switch", "-q", "-c", branch)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work "+id)
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.launched(it, ledger.StageImplement, "impl"); err != nil {
		t.Fatal(err)
	}
	// Recorded before the merge, as a drive records it before the push: after the
	// merge the commit is the trunk's too and nothing could tell it from a branch
	// that never moved.
	if err := s.recordHead(id, branch); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "finish "+id, branch)
}

// refusedCleanly asserts a drive refused with exit 3 naming want, and left the
// operator's branch, HEAD and tree as they were, with no completion for 1 and
// no runner launched.
func refusedCleanly(t *testing.T, repo, ledgerPath, launches, before, want string, err error) {
	t.Helper()
	if Code(err) != ExitRefused || err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("drive ended %v (exit %d), want exit %d naming %s", err, Code(err), ExitRefused, want)
	}
	if got := headBranch(t, repo); got != "operator-work" {
		t.Errorf("the checkout moved to %s", got)
	}
	if got := git(t, repo, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved from %s to %s", before, got)
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("the refusal left the tree dirty: %q", st)
	}
	l, lerr := ledger.Load(ledgerPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if l.State("1").Merged {
		t.Error("done1=true: a completion row was written before the refusal")
	}
	if got := launched(t, launches); len(got) != 0 {
		t.Errorf("a runner launched on %v", got)
	}
}

// A clean entry tree does not make a finish safe when finish's own ledger write
// is what git reports: the write lands, and the switch after it carries the row
// off the branch the operator stood on.
func TestReconciliationRefusesARecordFinishWouldDirty(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-f", "ledger.jsonl")
	git(t, dir, "commit", "-q", "-m", "track records")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	before := git(t, dir, "rev-parse", "HEAD")

	err := driveErr(t)
	refusedCleanly(t, dir, filepath.Join(dir, "ledger.jsonl"), launches, before, "ledger.jsonl", err)
}

// A stop inside reconciliation is the drive's stop, whether the queue behind
// it is empty or not, and the count of what did finish before it survives.
func TestAReconciliationStopEndsTheDrive(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	for _, it := range s.backlog.Items {
		branch, err := s.branchFor(it)
		if err != nil {
			t.Fatal(err)
		}
		mergedLaunched(t, s, it.ID.Bare(), branch)
	}
	// The second item's result is tracked and it has a run log, so its finish
	// would write a path git reports; the first item's writes are all ignored.
	second := s.backlog.Items[1]
	if err := os.MkdirAll(s.cfg.LogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.cfg.LogDir(), runlog.Name(runlog.Base(second.ID.Stem(), second.Slug), time.Now())),
		`{"type":"result","subtype":"success","is_error":false}`+"\n")
	vpath := s.reportPath(second)
	if err := os.MkdirAll(filepath.Dir(vpath), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, vpath, reports.NoResult+"\n")
	git(t, dir, "add", "-f", vpath)
	git(t, dir, "commit", "-q", "-m", "track a result")

	var err error
	stderr := captureErr(t, func() { err = driveErr(t) })
	if Code(err) != ExitRefused || err == nil || !strings.Contains(err.Error(), "not finishing 2") {
		t.Errorf("drive ended %v (exit %d), want exit %d refusing to finish 2", err, Code(err), ExitRefused)
	}
	if !strings.Contains(stderr, "1 finished") {
		t.Errorf("the partial progress went unreported:\n%s", stderr)
	}
	l, lerr := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !l.State("1").Merged || l.State("2").Merged {
		t.Errorf("done: 1=%v 2=%v, want true false", l.State("1").Merged, l.State("2").Merged)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the tree was left dirty: %q", st)
	}
	if got := launched(t, launches); len(got) != 0 {
		t.Errorf("a runner launched on %v", got)
	}
}

// reconcile reports a dirty tree as an error, not as a short count.
func TestReconcileReturnsItsStop(t *testing.T) {
	s, _ := treeProject(t)
	mergedLaunched(t, s, "1", "first")
	write(t, filepath.Join(s.cfg.Root, "stray.txt"), "somebody's afternoon\n")
	landed, _, err := s.reconcile()
	if !errors.Is(err, errDirtyTree) || !strings.Contains(err.Error(), "stray.txt") {
		t.Errorf("reconcile returned %v, want errDirtyTree naming stray.txt", err)
	}
	if landed != 0 || s.ledger.State("1").Merged {
		t.Errorf("finished %d over a dirty tree", landed)
	}
}

func nestedProject(t *testing.T) (s *session, repo, launches string) {
	t.Helper()
	s, launches = treeProject(t)
	repo = s.cfg.Root
	project := filepath.Join(repo, "component")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{config.FileName, "go.mod", "work"} {
		if err := os.Rename(filepath.Join(repo, name), filepath.Join(project, name)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(project, config.FileName)
	data, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.ReplaceAll(string(data), `"ledger.jsonl"`, `"../ledger.jsonl"`))
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "nested project")
	t.Chdir(project)
	s = openIn(t, project)
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	return s, repo, launches
}

// A project nested in a larger repository writes a ledger its checkout
// carries, whatever directory .drover.toml sits in. The preflight measures
// against the worktree a switch changes, so it refuses before finish writes.
func TestReconciliationRefusesANestedProjectsParentRecord(t *testing.T) {
	s, repo, launches := nestedProject(t)
	git(t, repo, "add", "-f", "ledger.jsonl")
	git(t, repo, "commit", "-q", "-m", "track records")
	git(t, repo, "switch", "-q", "-c", "operator-work")
	before := git(t, repo, "rev-parse", "HEAD")

	if err := s.cleanForFinish(s.backlog.Items[0]); err == nil || !strings.Contains(err.Error(), "ledger.jsonl") {
		t.Errorf("preflight returned %v, want a refusal naming ledger.jsonl", err)
	}
	err := driveErr(t)
	refusedCleanly(t, repo, filepath.Join(repo, "ledger.jsonl"), launches, before, "ledger.jsonl", err)
}

// The same nesting with the ledger ignored reconciles as before.
func TestReconciliationFinishesANestedProjectsIgnoredRecord(t *testing.T) {
	s, repo, _ := nestedProject(t)
	git(t, repo, "switch", "-q", "-c", "operator-work")
	if err := s.cleanForFinish(s.backlog.Items[0]); err != nil {
		t.Fatalf("preflight refused an ignored record: %v", err)
	}
	if err := driveErr(t); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("the finish left the tree dirty: %q", st)
	}
	l, err := ledger.Load(filepath.Join(repo, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !l.State("1").Merged {
		t.Error("the merged item did not finish")
	}
}

// Reconciliation is the drive finishing what merged outside it, so the done row
// it writes is a row the drive appended and names it. The queue then comes up
// empty, and the drive still writes the start and end rows that list it.
func TestAReconciledDoneRowCarriesTheDrive(t *testing.T) {
	s, repo, _ := nestedProject(t)
	git(t, repo, "switch", "-q", "-c", "operator-work")
	if err := s.cleanForFinish(s.backlog.Items[0]); err != nil {
		t.Fatalf("preflight refused an ignored record: %v", err)
	}
	if err := driveErr(t); err != nil {
		t.Fatalf("drive: %v", err)
	}
	l, err := ledger.Load(filepath.Join(repo, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ds := l.Drives()
	if len(ds) != 1 || ds[0].ID != "D1" || ds[0].Started == "" || ds[0].Exit == nil || *ds[0].Exit != 0 {
		t.Fatalf("drives = %+v, want D1 started and ended with exit 0", ds)
	}
	var done *ledger.Row
	for i, r := range l.Rows {
		if r.ID == "1" && r.Merged != nil && *r.Merged {
			done = &l.Rows[i]
		}
	}
	if done == nil {
		t.Fatal("the merged item did not finish")
	}
	if done.Drive != "D1" {
		t.Errorf("the reconciled done row carries drive %q, want D1", done.Drive)
	}
	if got := l.DriveOf("1"); got != "D1" {
		t.Errorf("DriveOf(1) = %q, want D1", got)
	}
}

// A ledger outside the worktree is exempt: finish's append never touches the
// checkout, so a drive finishes it and leaves the tree clean.
func TestReconciliationFinishesARecordOutsideTheWorktree(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	outside := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.Rename(filepath.Join(dir, "ledger.jsonl"), outside); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.FileName)
	data, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.ReplaceAll(string(data), `"ledger.jsonl"`, fmt.Sprintf("%q", outside)))
	git(t, dir, "commit", "-q", "-am", "ledger outside")
	s = openIn(t, dir)
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", "operator-work")

	if err := s.cleanForFinish(s.backlog.Items[0]); err != nil {
		t.Fatalf("preflight refused a record outside the worktree: %v", err)
	}
	if err := driveErr(t); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the finish left the tree dirty: %q", st)
	}
	l, err := ledger.Load(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !l.State("1").Merged {
		t.Error("the merged item did not finish")
	}
}

// A git status that fails is a stop, not a clean tree.
func TestPreflightStopsOnAStatusError(t *testing.T) {
	s, _ := treeProject(t)
	mergedLaunched(t, s, "1", "first")
	// The repository itself goes away, rather than GIT_DIR pointing somewhere
	// absent: every git command drover runs drops GIT_DIR before exec, so that
	// sabotage never reaches git and the test would pass while asserting
	// nothing. A root that is not a repository is a failure no environment can
	// hide.
	if err := os.Rename(filepath.Join(s.cfg.Root, ".git"), filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	err := s.cleanForFinish(s.backlog.Items[0])
	if err == nil || !strings.Contains(err.Error(), "git status") {
		t.Errorf("preflight returned %v, want the git status failure", err)
	}
}

// Containment is by path component: a basename opening with two dots is a
// file inside the tree, and a record outside it is exempt.
func TestInWorktreeComparesComponents(t *testing.T) {
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		abs  string
		rel  string
		want bool
	}{
		{filepath.Join(top, "..ledger.jsonl"), "..ledger.jsonl", true},
		{filepath.Join(top, "sub", "ledger.jsonl"), "sub/ledger.jsonl", true},
		{filepath.Join(filepath.Dir(top), "ledger.jsonl"), "", false},
		{filepath.Dir(top), "", false},
	} {
		rel, ok := inWorktree(top, c.abs)
		if ok != c.want || rel != c.rel {
			t.Errorf("inWorktree(%s) = %q %v, want %q %v", c.abs, rel, ok, c.rel, c.want)
		}
	}
}

// The merge hold and check read the tree through one transport, so for the
// same tree they hand denyMatch the same bytes, and those bytes are the
// filenames on disk. An entry check counts as covering a file is then an entry
// the hold enforces on it.
func TestHoldAndCheckReadTheSameBytes(t *testing.T) {
	s, files := awkwardTree(t)
	changed, err := s.changedNames("work", "main")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := s.trackedNames()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, f := range files {
		want = append(want, f)
	}
	sort.Strings(want)
	sort.Strings(changed)
	sort.Strings(tracked)
	if !slices.Equal(changed, want) {
		t.Errorf("the hold reads %q, want the names on disk %q", changed, want)
	}
	// Containment, not equality: trackedNames lists the whole tree, and the
	// fixture's own config, module file and item body are tracked too. What this
	// asserts is that both readers hand denyMatch the same bytes for the same
	// file, which is the equivalence a shared matcher over two transports needs.
	for _, name := range want {
		if !slices.Contains(tracked, name) {
			t.Errorf("check does not read %q as the hold does; it read %q", name, tracked)
		}
	}

	s.cfg.Loop.DenyPaths = want
	for _, f := range s.checkReport().Findings {
		if f.Kind == "PROTECTED UNMATCHED" {
			t.Errorf("check calls an entry dead that names a tracked file: %s", f.Message)
		}
	}
	hits, err := s.deniedPaths("work", "main")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(hits)
	if !slices.Equal(hits, want) {
		t.Errorf("check counts every entry as covering and the hold enforces %q of %q", hits, want)
	}
}

// A protected_paths entry that matches no tracked file holds nothing, and check says
// so; one that matches, read the way the merge hold reads it, is silent.
func TestCheckReportsADenyEntryThatMatchesNothing(t *testing.T) {
	s, files := awkwardTree(t)
	s.cfg.Loop.DenyPaths = []string{"missing-migrations/", "held-nonascii/", files["tab"]}
	var hits []string
	for _, f := range s.checkReport().Findings {
		if f.Kind == "PROTECTED UNMATCHED" {
			hits = append(hits, f.Message)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "missing-migrations/") {
		t.Errorf("PROTECTED UNMATCHED findings %q, want one naming missing-migrations/ alone", hits)
	}
}

// End to end: Git's default display quoting, a directory entry, a non-ASCII
// file the branch adds beneath it, and a review and gate that both pass. A
// transport that read the quoted display form would never match the prefix and
// would merge it.
func TestADriveHoldsANonASCIIFileUnderADeniedDirectory(t *testing.T) {
	ghLog := mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.quotePath", "true")
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("protected_paths = [\"backend/migrations/\"]\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-am", "deny migrations")
	git(t, dir, "push", "-q", "origin", "main")
	t.Setenv("DROVER_TEST_ADD", "backend/migrations/001_\u00e9.sql")

	rows := driveAll(t)
	if len(rows) == 0 || rows[0].ID != "1" {
		t.Fatalf("drive reported %+v, want item 1 first", rows)
	}
	if d := rows[0]; d.Outcome != outcomeParked || d.Park != "protected_paths" || d.Merged {
		t.Errorf("item 1 is %q/%q/merged=%v (%s), want %q/protected_paths/false",
			d.Outcome, d.Park, d.Merged, d.Reason, outcomeParked)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr merge") {
			t.Errorf("the forge was asked to merge a branch adding a denied file: %q", c)
		}
	}
}

// symlinkedReport leaves item 1 merged with a successful run log and its
// result tracked as a symlink to an interrupted-run stub outside the
// worktree, with item 2 done so nothing else is drivable, and the operator on
// a branch of their own.
func symlinkedReport(t *testing.T) (s *session, launches, link string) {
	t.Helper()
	s, launches = treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	first := s.backlog.Items[0]
	if err := os.MkdirAll(s.cfg.LogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.cfg.LogDir(), runlog.Name(runlog.Base(first.ID.Stem(), first.Slug), time.Now())),
		`{"type":"result","subtype":"success","is_error":false}`+"\n")
	stubPath := filepath.Join(t.TempDir(), "01-first.md")
	write(t, stubPath, reports.NoResult+"\n")
	link = s.reportPath(first)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stubPath, link); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-f", link)
	git(t, dir, "commit", "-q", "-m", "track a result link")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	return s, launches, link
}

// reports.Write renames over its destination, so a tracked symlink is replaced
// inside the worktree however far outside its referent lives. The preflight
// refuses before any completion row or checkout.
func TestReconciliationRefusesAReportSymlinkFinishWouldReplace(t *testing.T) {
	s, launches, link := symlinkedReport(t)
	dir := s.cfg.Root
	before := git(t, dir, "rev-parse", "HEAD")

	err := driveErr(t)
	refusedCleanly(t, dir, filepath.Join(dir, "ledger.jsonl"), launches, before, "01-first.md", err)
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the result link was replaced: %v %v", fi, err)
	}
}

// A write's mutation decides how far a real symlink resolves: an append lands
// at the referent, a dangling one included, a replacement lands on the link,
// and both follow a symlinked parent directory.
func TestRecordWritesResolveByMutation(t *testing.T) {
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	referent := filepath.Join(outside, "record")
	write(t, referent, "x\n")
	realDir := filepath.Join(top, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(top, "alias")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realDir, "record")
	if err := os.Symlink(referent, link); err != nil {
		t.Fatal(err)
	}
	written := filepath.Join(top, "alias", "record")
	// A relative target is read from the link's own directory, and ".." after
	// a link climbs from where the link led, as the kernel does, through a
	// second link to a file nobody has created yet.
	if err := os.Symlink("real/../real/hop", filepath.Join(outside, "relative")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("created", filepath.Join(realDir, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(top, "real"), filepath.Join(outside, "real")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		w    recordWrite
		want string
	}{
		{replaces(written), link},
		{appendsTo(written), referent},
		{replaces(filepath.Join(top, "alias", "absent")), filepath.Join(realDir, "absent")},
		{appendsTo(filepath.Join(top, "alias", "absent")), filepath.Join(realDir, "absent")},
		{appendsTo(filepath.Join(outside, "relative")), filepath.Join(realDir, "created")},
		{replaces(filepath.Join(outside, "relative")), filepath.Join(outside, "relative")},
	} {
		got, err := c.w.destination()
		if err != nil || got != c.want {
			t.Errorf("%T(%s).destination() = %s %v, want %s", c.w, c.w, got, err, c.want)
		}
	}
	if rel, ok := inWorktree(top, mustDestination(t, replaces(written))); !ok || rel != "real/record" {
		t.Errorf("a replaced link counted as %q %v, want real/record inside the worktree", rel, ok)
	}
	if _, ok := inWorktree(top, mustDestination(t, appendsTo(written))); ok {
		t.Error("an append through the link counted as inside the worktree")
	}

	f, err := os.OpenFile(filepath.Join(outside, "relative"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the test's own link
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := os.Lstat(filepath.Join(realDir, "created")); err != nil {
		t.Errorf("the kernel created the append somewhere other than the modeled destination: %v", err)
	}

	loop := filepath.Join(outside, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if got, err := appendsTo(loop).destination(); err == nil {
		t.Errorf("a symlink cycle resolved to %s, want an error", got)
	}
}

func mustDestination(t *testing.T, w recordWrite) string {
	t.Helper()
	got, err := w.destination()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ledger.Append opens with O_CREATE, which follows a dangling symlink and
// creates its missing referent. A link git ignores inside the worktree, or one
// outside it, exempts nothing when that referent is an unignored path inside
// the worktree: the preflight refuses, and the real writer confirms the file
// it refused over is the one it creates.
func TestPreflightFollowsADanglingAppendLink(t *testing.T) {
	for _, c := range []struct {
		name    string
		outside bool
	}{{"ignored link inside", false}, {"link outside", true}} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := treeProject(t)
			top, err := filepath.EvalSymlinks(s.cfg.Root)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(top, "created-by-append.jsonl")
			link := filepath.Join(s.cfg.Root, "ledger.jsonl")
			if c.outside {
				link = filepath.Join(t.TempDir(), "ledger-link")
			} else if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			s.cfg.Paths.Ledger = link
			if st := git(t, s.cfg.Root, "status", "--porcelain"); st != "" {
				t.Fatalf("fixture dirty: %q", st)
			}

			if got := mustDestination(t, appendsTo(link)); got != target {
				t.Errorf("append destination = %s, want the dangling referent %s", got, target)
			}
			err = s.cleanForFinish(s.backlog.Items[0])
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), "created-by-append.jsonl") {
				t.Errorf("preflight returned %v, want a refusal naming created-by-append.jsonl", err)
			}
			yes := true
			if err := ledger.Append(link, ledger.Row{ID: "1", Merged: &yes}); err != nil {
				t.Fatal(err)
			}
			if st := git(t, s.cfg.Root, "status", "--porcelain"); st != "?? created-by-append.jsonl" {
				t.Errorf("the writer dirtied %q, not the path the preflight modeled", st)
			}
		})
	}
}

// The worktree top keeps every byte of its directory name. A root ending in
// whitespace, trimmed, would place its own records outside the worktree and
// exempt the result link finish replaces.
func TestReconciliationRefusesInAWorktreeNamedWithTrailingWhitespace(t *testing.T) {
	for _, suffix := range []string{" ", "\t", "\n"} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			s, launches, _ := symlinkedReport(t)
			moved := filepath.Join(t.TempDir(), "repo"+suffix)
			if err := os.Rename(s.cfg.Root, moved); err != nil {
				t.Fatal(err)
			}
			t.Chdir(moved)
			s = openIn(t, moved)
			if top, err := s.worktreeRoot(); err != nil || filepath.Base(top) != filepath.Base(moved) {
				t.Errorf("worktree root %q (%v), want a directory named %q", top, err, filepath.Base(moved))
			}
			before := git(t, moved, "rev-parse", "HEAD")

			err := driveErr(t)
			refusedCleanly(t, moved, filepath.Join(moved, "ledger.jsonl"), launches, before, "01-first.md", err)
			if fi, err := os.Lstat(s.reportPath(s.backlog.Items[0])); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the result link was replaced: %v %v", fi, err)
			}
		})
	}
}

// An append changes the file, not the name it was opened by, so every hard
// link to it changes too. A ledger configured at an external name whose inode
// is also tracked inside the worktree dirties the tree through that link.
func TestReconciliationRefusesAnExternalLedgerHardLinkedToATrackedFile(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.Link(filepath.Join(dir, "ledger.jsonl"), outside); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.FileName)
	data, err := os.ReadFile(cfg) // #nosec G304 -- the test's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.ReplaceAll(string(data), `"ledger.jsonl"`, fmt.Sprintf("%q", outside)))
	git(t, dir, "add", "-f", "ledger.jsonl", config.FileName)
	git(t, dir, "commit", "-q", "-m", "track the ledger's inode under an external alias")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	before := git(t, dir, "rev-parse", "HEAD")

	err = driveErr(t)
	refusedCleanly(t, dir, outside, launches, before, "ledger.jsonl", err)
}

// An ignored ledger exempts its own name only: a tracked hard link to the same
// file is what git reports once the append lands.
func TestReconciliationRefusesAnIgnoredLedgerHardLinkedToATrackedFile(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes}); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "ledger.jsonl")
	if err := os.Link(original, filepath.Join(dir, "tracked-ledger.jsonl")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked-ledger.jsonl")
	git(t, dir, "commit", "-q", "-m", "track a ledger alias")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	before := git(t, dir, "rev-parse", "HEAD")

	err := driveErr(t)
	refusedCleanly(t, dir, original, launches, before, "tracked-ledger.jsonl", err)
}

// A rename over an external result unlinks its old inode from that one entry,
// so a tracked hard link to the old file keeps its bytes and the finish is safe.
// The alias rule for appends must not reach a replacement.
func TestReconciliationFinishesAnExternalReportReplacedOverATrackedHardLink(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes}); err != nil {
		t.Fatal(err)
	}
	first := s.backlog.Items[0]
	if err := os.MkdirAll(s.cfg.LogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.cfg.LogDir(), runlog.Name(runlog.Base(first.ID.Stem(), first.Slug), time.Now())),
		`{"type":"result","subtype":"success","is_error":false}`+"\n")
	outside := t.TempDir()
	external := filepath.Join(outside, "01-first.md")
	original := reports.NoResult + "\n"
	write(t, external, original)
	alias := filepath.Join(dir, "tracked-result.md")
	if err := os.Link(external, alias); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.FileName)
	data, err := os.ReadFile(cfg) // #nosec G304 -- the test's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.ReplaceAll(string(data), `"reports"`, fmt.Sprintf("%q", outside)))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "external result with a tracked alias")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	s = openIn(t, dir)
	if got := s.reportPath(first); got != external {
		t.Fatalf("fixture result = %q, want %q", got, external)
	}

	if err := s.cleanForFinish(first); err != nil {
		t.Fatalf("preflight refused a replacement that changes only an external entry: %v", err)
	}
	if err := driveErr(t); err != nil {
		t.Fatal(err)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the finish left the tree dirty: %q", st)
	}
	if got, err := os.ReadFile(alias); err != nil || string(got) != original { // #nosec G304 -- the test's own file
		t.Errorf("the tracked alias changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(external); err != nil || string(got) == original { // #nosec G304 -- the test's own file
		t.Errorf("the external entry was not replaced: %q %v", got, err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil || !l.State("1").Merged {
		t.Fatalf("no completion for 1: %v", err)
	}
}

// submoduleWith adds a populated submodule at module under dir, holding a
// further one at module/inner when nested, and returns the directory of the
// deepest, where a test commits the alias it wants status to report.
func submoduleWith(t *testing.T, dir string, nested bool) string {
	t.Helper()
	repo := func() string {
		src := t.TempDir()
		git(t, src, "init", "-q", "-b", "main")
		write(t, filepath.Join(src, "seed"), "seed\n")
		git(t, src, "add", "seed")
		git(t, src, "commit", "-q", "-m", "seed")
		return src
	}
	outer := repo()
	if nested {
		git(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", "-q", repo(), "inner")
		git(t, outer, "commit", "-q", "-m", "inner")
	}
	git(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", outer, "module")
	git(t, dir, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init", "--recursive")
	if nested {
		return filepath.Join(dir, "module", "inner")
	}
	return filepath.Join(dir, "module")
}

// commitUp commits every change from the deepest submodule out to dir, so
// each gitlink records the commit below it and the whole tree reads clean.
func commitUp(t *testing.T, dir, deepest, msg string) {
	t.Helper()
	for repo := deepest; ; repo = filepath.Dir(repo) {
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-q", "-m", msg)
		if repo == dir {
			return
		}
	}
}

// A submodule's files are one gitlink to the parent's listing, yet status
// reports the submodule modified when any of them changes. An ignored or
// external ledger sharing an inode with a file tracked inside a submodule,
// at any depth, dirties the tree through it.
func TestReconciliationRefusesALedgerHardLinkedIntoASubmodule(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested=%v/external=%v", nested, external), func(t *testing.T) {
				s, launches := treeProject(t)
				dir := s.cfg.Root
				mergedLaunched(t, s, "1", "first")
				yes := true
				if err := s.record(ledger.Row{ID: "2", Merged: &yes}); err != nil {
					t.Fatal(err)
				}
				deepest := submoduleWith(t, dir, nested)
				original := filepath.Join(dir, "ledger.jsonl")
				if err := os.Link(original, filepath.Join(deepest, "tracked-ledger.jsonl")); err != nil {
					t.Fatal(err)
				}
				ledgerPath := original
				if external {
					ledgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
					if err := os.Link(original, ledgerPath); err != nil {
						t.Fatal(err)
					}
					cfg := filepath.Join(dir, config.FileName)
					data, err := os.ReadFile(cfg) // #nosec G304 -- the test's own config
					if err != nil {
						t.Fatal(err)
					}
					write(t, cfg, strings.ReplaceAll(string(data), `"ledger.jsonl"`, fmt.Sprintf("%q", ledgerPath)))
				}
				commitUp(t, dir, deepest, "track the ledger's inode in a submodule")
				git(t, dir, "switch", "-q", "-c", "operator-work")
				before := git(t, dir, "rev-parse", "HEAD")
				if st := git(t, dir, "status", "--porcelain", "--ignore-submodules=none"); st != "" {
					t.Fatalf("dirty fixture: %q", st)
				}

				err := driveErr(t)
				refusedCleanly(t, dir, ledgerPath, launches, before, "tracked-ledger.jsonl", err)
				if st := git(t, deepest, "status", "--porcelain"); st != "" {
					t.Errorf("the submodule was left dirty: %q", st)
				}
			})
		}
	}
}

// A replacement changes only its own entry, so a tracked peer of the old
// inode inside a submodule keeps its bytes and an external result is safe to replace.
func TestReconciliationFinishesAnExternalReportReplacedOverASubmoduleHardLink(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	yes := true
	if err := s.record(ledger.Row{ID: "2", Merged: &yes}); err != nil {
		t.Fatal(err)
	}
	first := s.backlog.Items[0]
	if err := os.MkdirAll(s.cfg.LogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.cfg.LogDir(), runlog.Name(runlog.Base(first.ID.Stem(), first.Slug), time.Now())),
		`{"type":"result","subtype":"success","is_error":false}`+"\n")
	deepest := submoduleWith(t, dir, false)
	outside := t.TempDir()
	external := filepath.Join(outside, "01-first.md")
	original := reports.NoResult + "\n"
	write(t, external, original)
	alias := filepath.Join(deepest, "tracked-result.md")
	if err := os.Link(external, alias); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.FileName)
	data, err := os.ReadFile(cfg) // #nosec G304 -- the test's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.ReplaceAll(string(data), `"reports"`, fmt.Sprintf("%q", outside)))
	commitUp(t, dir, deepest, "external result with a submodule alias")
	git(t, dir, "switch", "-q", "-c", "operator-work")
	s = openIn(t, dir)

	if err := s.cleanForFinish(first); err != nil {
		t.Fatalf("preflight refused a replacement that changes only an external entry: %v", err)
	}
	if err := driveErr(t); err != nil {
		t.Fatal(err)
	}
	if st := git(t, dir, "status", "--porcelain", "--ignore-submodules=none"); st != "" {
		t.Errorf("the finish left the tree dirty: %q", st)
	}
	if got, err := os.ReadFile(alias); err != nil || string(got) != original { // #nosec G304 -- the test's own file
		t.Errorf("the submodule alias changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(external); err != nil || string(got) == original { // #nosec G304 -- the test's own file
		t.Errorf("the external entry was not replaced: %q %v", got, err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil || !l.State("1").Merged {
		t.Fatalf("no completion for 1: %v", err)
	}
}

// A session that ran and left no commit gives the reviewer an empty diff and
// the forge a branch that never moved — and `git branch --merged` lists such a
// branch as merged, because it sits at the trunk it was cut from. Reconciliation
// would then record a completion for work that does not exist.
func TestADriveParksOnABranchHoldingNoCommits(t *testing.T) {
	s := driveProject(t)

	bin := t.TempDir()
	// A run that finishes cleanly and commits nothing.
	t.Setenv("DROVER_TEST_RESULT_LINE", `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"result":"looked around"}`)
	stub(t, filepath.Join(bin, "stub-impl"), printsResultLine)
	t.Setenv("DROVER_TEST_REV_STDERR", "a reviewer was paid for an empty diff")
	stub(t, filepath.Join(bin, "stub-rev"), revRefuses)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	rows := driveAll(t)
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1", len(rows))
	}
	d := rows[0]
	if d.Outcome != outcomeParked || d.Park != parkNoCommits {
		t.Errorf("got %s/%s, want %s/%s", d.Outcome, d.Park, outcomeParked, parkNoCommits)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"stage":"review"`) {
		t.Error("a reviewer was launched against a branch with no commits")
	}
	if refs := git(t, s.cfg.Root, "ls-remote", "--heads", "origin"); strings.Contains(refs, "first") {
		t.Errorf("an empty branch reached the forge:\n%s", refs)
	}
}

// commitsPast is what tells a branch nobody advanced from one whose work merged:
// ancestry reports both as merged, because the first sits at the trunk.
func TestCommitsPastCountsOnlyWhatTheBranchAdded(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root

	git(t, dir, "switch", "-q", "-c", "empty")
	if n, err := commitsPast(dir, "main", "empty"); err != nil || n != 0 {
		t.Errorf("a branch nobody advanced counts %d (%v), want 0", n, err)
	}
	write(t, filepath.Join(dir, "work.txt"), "x\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "one")
	if n, err := commitsPast(dir, "main", "empty"); err != nil || n != 1 {
		t.Errorf("one commit past the trunk counts %d (%v), want 1", n, err)
	}
	git(t, dir, "switch", "-q", "main")
}

// driveRows drives the whole queue and returns every row it reported alongside
// the error it ended on. driveAll fails the test on an error, which is no use to
// a case whose point is the refusal.
func driveRows(t *testing.T) ([]DriveItem, error) {
	t.Helper()
	var err error
	out := capture(t, func() { err = execute(t, "drive", "--json") })
	var rows []DriveItem
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			rows = append(rows, d)
		}
	}
	return rows, err
}

// Reconciliation writes the ledger and a result into the operator's checkout,
// so a checkout holding somebody's uncommitted work skips it. Nothing else does:
// the items run in worktrees of their own, and the checkout stays on whatever
// branch the operator was standing on, with their file where they left it.
func TestADirtyCheckoutSkipsReconciliationAndDrivesTheQueue(t *testing.T) {
	for _, c := range []struct{ name, branch string }{
		{name: "operator branch", branch: "operator-work"},
		{name: "trunk", branch: "main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, launches := treeProject(t)
			dir := s.cfg.Root
			mergedLaunched(t, s, "1", "first")
			if c.branch != "main" {
				git(t, dir, "switch", "-q", "-c", c.branch)
			}
			write(t, filepath.Join(dir, "leftover.txt"), "belongs to "+c.branch+"\n")

			rows, err := driveRows(t)
			if err != nil {
				t.Fatalf("drive ended %v, want the queue driven", err)
			}
			if got := headBranch(t, dir); got != c.branch {
				t.Errorf("the dirty checkout moved from %s to %s", c.branch, got)
			}
			if b, err := os.ReadFile(filepath.Join(dir, "leftover.txt")); err != nil || !strings.Contains(string(b), c.branch) {
				t.Errorf("the dirty file did not stay put: %q %v", b, err)
			}
			l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if l.State("1").Merged {
				t.Error("reconciliation finished item 1 over a dirty checkout")
			}
			if got := launched(t, launches); len(got) == 0 {
				t.Error("nothing launched: the operator's file stopped the queue")
			}
			for _, d := range rows {
				if d.Park == parkUncommitted || strings.Contains(d.Reason, "leftover.txt") {
					t.Errorf("%s parked on the operator's file: %s", d.ID, d.Reason)
				}
			}
		})
	}
}

// With nothing drivable a dirty checkout costs only the reconciliation, and the
// drive says so and exits 0 rather than refusing a night it had no work for.
func TestADirtyCheckoutWithAnEmptyQueueIsLeftAlone(t *testing.T) {
	s, launches := treeProject(t)
	yes := true
	for _, id := range []string{"1", "2"} {
		if err := s.record(ledger.Row{ID: id, Merged: &yes, Date: ledger.Today()}); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(s.cfg.Root, "stray.txt"), "somebody's afternoon\n")

	var err error
	errOut := captureErr(t, func() { _, err = driveRows(t) })
	if err != nil {
		t.Errorf("drive ended %v, want exit 0", err)
	}
	if !strings.Contains(errOut, "stray.txt") || !strings.Contains(errOut, "not reconciling") {
		t.Errorf("the drive did not say why it skipped reconciliation:\n%s", errOut)
	}
	if got := headBranch(t, s.cfg.Root); got != "main" {
		t.Errorf("the drive switched to %q", got)
	}
	if got := launched(t, launches); len(got) != 0 {
		t.Errorf("a runner launched on %v", got)
	}
}

// A shared checkout would make one item's leftovers the next item's problem.
// Somebody's file in the operator's checkout never reaches an item:
// the item runs in its own worktree, which does not carry the file, and the
// checkout is neither switched nor touched.
func TestADirtyCheckoutNeverReachesAnItem(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	before := git(t, dir, "rev-parse", "HEAD")
	write(t, filepath.Join(dir, "somebody-elses.txt"), "not this item's work\n")

	rows, err := driveRows(t)
	if err != nil {
		t.Fatalf("drive ended %v", err)
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("the drive switched the checkout to %q", got)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved from %s to %s", before, got)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "?? somebody-elses.txt" {
		t.Errorf("the checkout's status changed to %q", st)
	}
	if got := launched(t, launches); len(got) == 0 || got[0] != "first" {
		t.Errorf("launches = %v, want item 1 implemented on its own branch", got)
	}
	for _, d := range rows {
		if strings.Contains(d.Reason, "somebody-elses.txt") {
			t.Errorf("%s parked on the operator's file: %s", d.ID, d.Reason)
		}
	}
}

// A session that closes with its work in the tree parks before the review.
// Otherwise drover pushes and reviews the branch anyway, and the review comes
// back "settled in the working tree, still absent from the PR head".
func TestARunThatLeavesWorkUncommittedParksBeforeTheReview(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	// It commits and then leaves more behind, which is the case a commit count
	// alone cannot see: the branch moved, and the head still lacks the work. Only
	// on first, so the second item shows whether one item's leftovers stop the
	// queue.
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"b=$(git rev-parse --abbrev-ref HEAD)\n"+
		"echo \"$b\" >> \"$DROVER_TEST_LAUNCHES\"\n"+
		"echo \"$b\" > \"$b.txt\"\n"+
		"git add \"$b.txt\"\n"+
		"git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false "+
		"commit -q -m 'half the work'\n"+
		"[ \"$b\" = first ] && echo 'the browser suite was still running' > half-finished.txt\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	stub(t, filepath.Join(bin, "stub-rev"), "#!/bin/sh\n"+
		"git rev-parse --abbrev-ref HEAD >> \"$DROVER_TEST_LAUNCHES\"\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	rows, err := driveRows(t)
	if err != nil {
		t.Fatalf("drive ended %v: one item's uncommitted work stopped the queue", err)
	}
	if len(rows) != 2 || rows[0].Park != parkUncommitted {
		t.Fatalf("drive reported %+v, want first parked %s and second driven", rows, parkUncommitted)
	}
	first := rows[0]
	for _, want := range []string{"half-finished.txt", first.Tree, "on first"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("the park does not carry %q: %s", want, first.Reason)
		}
	}
	// The tree stays for the operator, holding the work, on the item's branch.
	if b, err := os.ReadFile(filepath.Join(first.Tree, "half-finished.txt")); err != nil || !strings.Contains(string(b), "browser suite") {
		t.Errorf("the uncommitted work did not stay in the item's worktree: %q %v", b, err)
	}
	if got := headBranch(t, first.Tree); got != "first" {
		t.Errorf("the kept worktree is on %q, want first", got)
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("the operator's checkout moved to %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "half-finished.txt")); !os.IsNotExist(err) {
		t.Errorf("an item's leftover reached the operator's checkout: %v", err)
	}
	if got := launched(t, launches); len(got) < 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("launches were %v, want first's implement run and then second's", got)
	}
	if rows[1].Park == parkUncommitted {
		t.Errorf("second parked on uncommitted work it never wrote: %s", rows[1].Reason)
	}
}

// A branch can ignore a file its own generator writes, and its review can
// regenerate it. In one shared checkout the file is invisible on that branch,
// appears the moment the drive returns to the trunk, and parks the next item
// with the rest of the queue behind it. In worktrees the file stays in the
// first item's tree, which is clean by that branch's own rules and so is
// removed, and the next item runs.
func TestAFileOneBranchIgnoresNeverReachesTheNextItem(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"b=$(git rev-parse --abbrev-ref HEAD)\n"+
		"echo \"$b\" >> \"$DROVER_TEST_LAUNCHES\"\n"+
		"if [ \"$b\" = first ]; then\n"+
		"  echo candidates.json >> .gitignore\n"+
		"  git add .gitignore\n"+
		"  echo '{}' > candidates.json\n"+
		"else\n"+
		"  echo \"$b\" > \"$b.txt\" && git add \"$b.txt\"\n"+
		"fi\n"+
		"git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m work\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	rows, err := driveRows(t)
	if err != nil {
		t.Fatalf("drive ended %v", err)
	}
	if got := launched(t, launches); len(got) != 2 || got[1] != "second" {
		t.Fatalf("launches were %v, want second to run after first", got)
	}
	for _, d := range rows {
		if d.Park == parkUncommitted || strings.Contains(d.Reason, "candidates.json") {
			t.Errorf("%s parked on first's generated file: %s", d.ID, d.Reason)
		}
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the operator's checkout is no longer clean: %q", st)
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("the operator's checkout moved to %q", got)
	}
}

// run switches branch too, and the same carry applies: exit 3, nothing switched,
// nothing stashed on the operator's behalf.
func TestRunRefusesToSwitchOverADirtyTree(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	write(t, filepath.Join(dir, "another-items-work.txt"), "left behind\n")

	err := execute(t, "run", "1", "--headless")
	if Code(err) != ExitRefused || err == nil || !strings.Contains(err.Error(), "another-items-work.txt") {
		t.Fatalf("run ended %v (exit %d), want exit %d naming the path", err, Code(err), ExitRefused)
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("run switched to %q over a dirty tree", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "another-items-work.txt")); err != nil || !strings.Contains(string(b), "left behind") {
		t.Errorf("run did not leave the work alone: %q %v", b, err)
	}
}

// The run committed nothing, other items merged behind it, and the empty branch
// ends up *behind* the trunk rather than level with it. A tip-equality check
// answers no there, `git branch --merged` says merged, and reconciliation would
// record a completion with none of the item's code on main.
func TestReconciliationRefusesAMergedBranchThatNeverHeldACommit(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil || branch == "" {
		t.Fatalf("branchFor: %q %v", branch, err)
	}
	// The branch a killed run leaves: created from the trunk, never committed to.
	git(t, dir, "branch", branch, "main")
	if _, err := s.launched(it, ledger.StageImplement, "impl"); err != nil {
		t.Fatal(err)
	}
	// The trunk moves on, so the branch is behind it rather than level with it and
	// tip equality cannot separate the two.
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "somebody else's work")
	if same, err := sameCommit(dir, branch, "main"); err != nil || same {
		t.Fatalf("the fixture left %s level with main (%v), which is the case a tip check already caught", branch, err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}

	answer, how, err := s.mergedInto(branch, "main")
	if err != nil {
		t.Fatalf("mergedInto: %v", err)
	}
	// mergeUnvouched rather than mergeNo: one --force covering both would make
	// this case quiet, so the answer has to name which no it is.
	if answer != mergeUnvouched {
		t.Errorf("an empty branch behind the trunk answered %v (%s), want mergeUnvouched", answer, how)
	}
	if !strings.Contains(how, "no row shows the branch holding commits") {
		t.Errorf("the source string does not say why: %s", how)
	}
	var landed, left int
	out := captureErr(t, func() {
		var rerr error
		if landed, left, rerr = s.reconcile(); rerr != nil {
			t.Fatalf("reconcile: %v", rerr)
		}
	})
	if s.ledger.State("1").Merged {
		t.Error("reconciliation recorded a completion for a branch that never held a commit")
	}
	// Named, not skipped. finish refuses this on its own, so reaching it would refuse
	// rather than record — but it would print an error per item, and walking past in
	// silence tells the operator nothing about an id whose work may not be on the
	// trunk.
	if landed != 0 || left != 1 {
		t.Errorf("reconcile reported finished=%d left=%d, want 0 and 1", landed, left)
	}
	for _, want := range []string{"1 is merged only because", "drover finish 1 --force"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q:\n%s", want, out)
		}
	}
	if got := launched(t, launches); len(got) != 0 {
		t.Errorf("a runner launched on %v", got)
	}
}

// The row is written before the push and only for a branch off the trunk, which is
// the whole of what it distinguishes. Recorded after a merge it would vouch for
// nothing, because the commit is the trunk's by then.
func TestRecordHeadWritesOnlyForABranchOffTheTrunk(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	git(t, dir, "branch", "untouched", "main")
	if err := s.recordHead("1", "untouched"); err != nil {
		t.Fatalf("recordHead on a branch at the trunk: %v", err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.ledger.Heads("untouched"); len(got) != 0 {
		t.Errorf("a branch sitting at the trunk recorded %v", got)
	}

	git(t, dir, "switch", "-q", "-c", "moved")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	if err := s.recordHead("1", "moved"); err != nil {
		t.Fatalf("recordHead: %v", err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	want := git(t, dir, "rev-parse", "moved")
	if got := s.ledger.Heads("moved"); len(got) != 1 || got[0] != want {
		t.Fatalf("recorded %v, want one row at %s", got, want)
	}
	if !s.carriedWork("moved") {
		t.Error("the branch still holds the recorded head and carriedWork says no")
	}
	// An old row must not vouch for a branch since reset back to the trunk.
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "branch", "-f", "moved", "main")
	if s.carriedWork("moved") {
		t.Error("a stale row vouched for a branch reset to the trunk")
	}
}

// forge rewrites mergeProject's gh and gate as a forge that can actually merge:
// a merge without --auto merges the branch into origin's main and is reported
// merged from then on, as GitHub does once `gh pr merge` returns.
//
// DROVER_TEST_NO_AUTO makes --auto fail the way a private repository on the
// free plan does while checks are running, and DROVER_TEST_AUTO_MERGES makes it
// merge on the spot, as GitHub does when nothing at the forge waits.
// DROVER_TEST_REQUIRED answers the trunk's required-checks read: unset is 200
// with a context, and 404, 403 or 500 fail as GitHub does with that status.
// DROVER_TEST_MERGED_FAIL makes the merged pull request read fail. The check-runs read reports one check running for the first
// DROVER_TEST_PENDING reads and finished after, so a test can see drive wait.
// DROVER_TEST_GATE_REFUSE_FROM refuses every gate run from that zero-based
// count on, which lets the first gate pass and the one at the merge refuse.
// DROVER_TEST_GATE_BEHIND makes that many first gate runs exit 4, a head
// behind the trunk, and DROVER_TEST_GATE_STALE makes that many exit 1 saying
// so, as a gate that never learned exit 4 does. DROVER_TEST_GATE_ANCESTRY makes every gate run exit 4 when
// origin's main is not an ancestor of the head, the check merge-gate.sh makes,
// skipped under DROVER_GATE_MERGED=1 unless the value is "ignore". Each run
// appends the DROVER_GATE_MERGED it saw to merge-gate.merged. DROVER_TEST_OPEN names
// the branches whose pull request #8 reads as open.
// DROVER_TEST_CHECKS names a script that answers every other gh api read and
// gh run in its place, for a test that needs check runs of its own.
func forge(t *testing.T, ghLog string) (dir, remote string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	remote = git(t, dir, "remote", "get-url", "origin")
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	gatePath, err := exec.LookPath("merge-gate")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("DROVER_TEST_GH_LOG", ghLog)
	t.Setenv("DROVER_TEST_MERGED", filepath.Join(state, "merged"))
	t.Setenv("DROVER_TEST_READS", filepath.Join(state, "reads"))
	t.Setenv("DROVER_TEST_REMOTE", remote)
	stub(t, ghPath, `#!/bin/sh
if [ "$1 $2" = 'auth status' ]; then exit 0; fi
printf '%s\n' "$*" >> "$DROVER_TEST_GH_LOG"
case "$1 $2" in
  'pr list')
    head=""; prev=""
    for a; do [ "$prev" = "--head" ] && head=$a; prev=$a; done
    case "$*" in *'--state merged'*)
      if [ -n "${DROVER_TEST_MERGED_FAIL:-}" ]; then echo 'gh: Server Error (HTTP 502)' >&2; exit 1; fi
      if [ -f "$DROVER_TEST_MERGED" ] && grep -qxF -- "$head" "$DROVER_TEST_MERGED"; then
        echo '[{"number":7,"mergedAt":"2026-10-07T00:00:00Z"}]'; exit 0
      fi ;;
    *'--state open'*)
      for b in ${DROVER_TEST_OPEN:-}; do
        if [ "$b" = "$head" ]; then echo '[{"number":8}]'; exit 0; fi
      done ;;
    esac
    echo '[]' ;;
  'pr create') echo 'https://example.invalid/o/r/pull/7' ;;
  'pr merge')
    case "$*" in *--auto*)
      if [ -n "${DROVER_TEST_NO_AUTO:-}" ]; then
        echo 'GraphQL: Auto merge is not allowed for this repository (enablePullRequestAutoMerge)' >&2
        exit 1
      fi
      [ -n "${DROVER_TEST_AUTO_MERGES:-}" ] || exit 0 ;;
    esac
    for b; do :; done
    tmp=$(mktemp -d)
    git clone -q "$DROVER_TEST_REMOTE" "$tmp/c" >/dev/null 2>&1 &&
      git -C "$tmp/c" -c user.name=f -c user.email=f@example.invalid -c commit.gpgsign=false \
        merge -q --no-ff --no-edit "origin/$b" >/dev/null 2>&1 &&
      git -C "$tmp/c" push -q origin HEAD:main >/dev/null 2>&1 || { echo "merge of $b failed" >&2; exit 1; }
    printf '%s\n' "$b" >> "$DROVER_TEST_MERGED" ;;
  'pr '*) : ;;
  'api '*/required_status_checks)
    case "${DROVER_TEST_REQUIRED:-}" in
      '') echo '{"contexts":["ci/test"],"checks":[{"context":"ci/test","app_id":null}]}' ;;
      404) echo 'gh: Branch not protected (HTTP 404)' >&2; exit 1 ;;
      403) echo 'gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)' >&2; exit 1 ;;
      *) echo "gh: Server Error (HTTP $DROVER_TEST_REQUIRED)" >&2; exit 1 ;;
    esac ;;
  'api '*|'run '*)
    if [ -n "${DROVER_TEST_CHECKS:-}" ]; then exec "$DROVER_TEST_CHECKS" "$@"; fi
    n=$(cat "$DROVER_TEST_READS" 2>/dev/null || echo 0); echo $((n + 1)) > "$DROVER_TEST_READS"
    status=completed; [ "$n" -lt "${DROVER_TEST_PENDING:-0}" ] && status=in_progress
    echo '{"total_count":1,"check_runs":[{"id":1,"name":"ci/test","status":"'$status'","conclusion":"success"}]}' ;;
  *) echo '[]' ;;
esac
`)
	stub(t, gatePath, `#!/bin/sh
n=$(cat "$DROVER_ROOT/merge-gate.log" 2>/dev/null | wc -l | tr -d ' ')
printf '%s local=%s remote=%s\n' "$1" "$(git rev-parse "$1")" \
  "$(git rev-parse "origin/$1" 2>/dev/null || echo none)" >> "$DROVER_ROOT/merge-gate.log"
printf '%s\n' "${DROVER_GATE_MERGED:-}" >> "$DROVER_ROOT/merge-gate.merged"
case "${DROVER_TEST_GATE_ANCESTRY:-}:${DROVER_GATE_MERGED:-}" in
  :*|on:1) ;;
  *)
    git fetch -q origin main 2>/dev/null
    if ! git merge-base --is-ancestor origin/main "$1"; then
      echo "merge-gate: $1 is behind origin/main" >&2
      exit 4
    fi ;;
esac
if [ -n "${DROVER_TEST_GATE_STALE:-}" ] && [ "$n" -lt "$DROVER_TEST_GATE_STALE" ]; then
  echo "merge-gate: main has moved 9 commit(s) since $1 was checked. Refusing." >&2
  exit 1
fi
if [ -n "${DROVER_TEST_GATE_BEHIND:-}" ] && [ "$n" -lt "$DROVER_TEST_GATE_BEHIND" ]; then
  echo "merge-gate: $1 is behind origin/main" >&2
  exit 4
fi
if [ -n "${DROVER_TEST_GATE_REFUSE_FROM:-}" ] && [ "$n" -ge "$DROVER_TEST_GATE_REFUSE_FROM" ]; then
  echo 'checking the head'
  echo 'ci/test: failure on linux'
  exit 1
fi
exit 0
`)
	return dir, remote
}

// revRefuses is a reviewer that fails with $DROVER_TEST_REV_STDERR on stderr.
const revRefuses = "#!/bin/sh\necho \"$DROVER_TEST_REV_STDERR\" >&2\nexit 1\n"

// printsResultLine is a runner that prints $DROVER_TEST_RESULT_LINE as its
// whole stream and exits 0.
const printsResultLine = "#!/bin/sh\nprintf '%s\\n' \"$DROVER_TEST_RESULT_LINE\"\n"

// moveTrunk wraps the implement stub so that the first run, after committing,
// lands a commit on origin's main from somewhere else: what a merge earlier in
// the same drive does to every item after it. It writes file with body, so the
// caller picks a clean merge or a conflict with implemented.txt.
func moveTrunk(t *testing.T, remote, file, body string) {
	t.Helper()
	moveTrunkAfter(t, remote, "impl", "*", file, body)
}

// moveTrunkAfter is moveTrunk on the first call of bin whose arguments match
// the shell pattern when.
func moveTrunkAfter(t *testing.T, remote, bin, when, file, body string) {
	t.Helper()
	path, err := exec.LookPath(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_MOVE_MARKER", filepath.Join(t.TempDir(), "moved"))
	t.Setenv("DROVER_TEST_MOVE_REMOTE", remote)
	t.Setenv("DROVER_TEST_MOVE_FILE", file)
	t.Setenv("DROVER_TEST_MOVE_BODY", body)
	t.Setenv("DROVER_TEST_MOVE_WHEN", when)
	stub(t, path, `#!/bin/sh
"$0.orig" "$@"; rc=$?
case "$*" in $DROVER_TEST_MOVE_WHEN) ;; *) exit $rc ;; esac
if [ ! -e "$DROVER_TEST_MOVE_MARKER" ]; then
  touch "$DROVER_TEST_MOVE_MARKER"
  tmp=$(mktemp -d)
  {
    git clone -q "$DROVER_TEST_MOVE_REMOTE" "$tmp/c" &&
    printf '%s\n' "$DROVER_TEST_MOVE_BODY" > "$tmp/c/$DROVER_TEST_MOVE_FILE" &&
    git -C "$tmp/c" add -A &&
    git -C "$tmp/c" -c user.name=o -c user.email=o@example.invalid -c commit.gpgsign=false commit -q -m 'elsewhere' &&
    git -C "$tmp/c" push -q origin HEAD:main
  } >/dev/null 2>&1
fi
exit $rc
`)
}

// mergeCalls are the gh pr merge invocations, in order.
func mergeCalls(t *testing.T, ghLog string) []string {
	t.Helper()
	var out []string
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr merge") {
			out = append(out, c)
		}
	}
	return out
}

// The forge refuses auto-merge, a gate is declared, and the gate passes at the
// head once its checks finish: drive merges without --auto, bound to that head,
// and finishes the item in the same drive, rather than parking it merge_failed
// for a person to merge by hand.
func TestAForgeWithoutAutoMergeMergesOnTheGatesWord(t *testing.T) {
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	t.Setenv("DROVER_TEST_PENDING", "2")
	poll := checksPoll
	checksPoll = time.Millisecond
	t.Cleanup(func() { checksPoll = poll })

	rows := driveAll(t)
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Outcome != outcomeMergedByGate || !got.Merged || !got.Finished || got.PR != 7 {
		t.Fatalf("item 1 is %q merged=%v finished=%v pr=%d (%s), want %q, merged, finished, PR 7",
			got.Outcome, got.Merged, got.Finished, got.PR, got.Reason, outcomeMergedByGate)
	}

	merges := mergeCalls(t, ghLog)
	if len(merges) != 2 || !strings.Contains(merges[0], "--auto") {
		t.Fatalf("merges = %q, want the --auto attempt and then the merge gate's merge", merges)
	}
	head := git(t, dir, "rev-parse", got.Branch)
	if want := "pr merge --squash --match-head-commit " + head + " " + got.Branch; merges[1] != want {
		t.Errorf("the merge gate's merge was %q, want %q", merges[1], want)
	}

	// The checks finish before each gate reads them: two pending reads and a
	// finished one before the first gate, then a finished one before the gate at
	// the merge, all before the merge that follows it.
	calls := ghCalls(t, ghLog)
	var reads int
	for _, c := range calls {
		if strings.HasPrefix(c, "api ") && !strings.HasSuffix(c, "/required_status_checks") {
			if !strings.Contains(c, head) {
				t.Errorf("checks read for another commit: %q", c)
			}
			reads++
		}
		if c == merges[1] {
			break
		}
	}
	if reads != 4 {
		t.Errorf("checks were read %d times before the merge, want 4 (two pending and one finished at the first merge gate, one at the merge)", reads)
	}
	if n := len(gateLog(t, dir)); n != 2 {
		t.Errorf("the merge gate ran %d times, want 2: once before the pull request and once at the merge", n)
	}

	git(t, dir, "fetch", "-q", remote, "main:refs/remotes/origin/main")
	if ok, err := isAncestor(dir, head, "origin/main"); err != nil || !ok {
		t.Errorf("the merged head is not on origin's main (err %v)", err)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ledger.State("1").Merged {
		t.Error("item 1 merged and the ledger does not record it done: it was not finished in this drive")
	}
}

// A gate is declared and the trunk has no required checks, so an --auto request
// would merge on the spot with nothing at the forge waiting. drive never makes
// it: the gate reads the head and the merge is bound to it, whether the lookup
// answered 404, 403 or failed outright. An --auto here would merge unchecked
// whenever CI happens to finish first.
func TestNoRequiredChecksMergesOnTheGateWithoutAuto(t *testing.T) {
	for _, c := range []struct{ name, status, says string }{
		{"no required checks", "404", "main has no required checks, so nothing at the forge would wait"},
		{"plan cannot protect", "403", "main has no required checks, so nothing at the forge would wait"},
		{"lookup fails", "500", "the required checks on main could not be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ghLog := mergeProject(t)
			dir, _ := forge(t, ghLog)
			t.Setenv("DROVER_TEST_REQUIRED", c.status)
			t.Setenv("DROVER_TEST_AUTO_MERGES", "1")

			var rows []DriveItem
			errOut := captureErr(t, func() { rows = driveAll(t) })
			got := rows[0]
			if got.Outcome != outcomeMergedByGate || !got.Merged || !got.Finished {
				t.Fatalf("item 1 is %q merged=%v finished=%v (%s), want %q, merged and finished",
					got.Outcome, got.Merged, got.Finished, got.Reason, outcomeMergedByGate)
			}
			head := git(t, dir, "rev-parse", got.Branch)
			merges := mergeCalls(t, ghLog)
			if want := "pr merge --squash --match-head-commit " + head + " " + got.Branch; len(merges) != 1 || merges[0] != want {
				t.Fatalf("merges = %q, want only %q", merges, want)
			}
			seen := gateLog(t, dir)
			if len(seen) != 2 {
				t.Fatalf("the merge gate ran %d times, want 2: once before the pull request and once at the merge", len(seen))
			}
			if _, local, remote := splitGateLine(t, seen[1]); local != head || remote != head {
				t.Errorf("the merge gate at the merge read local %s remote %s, want %s", local, remote, head)
			}
			if !strings.Contains(errOut, "· 1: "+c.says) || !strings.Contains(errOut, "[drive] merge_gate decides at the head") {
				t.Errorf("stderr does not say why --auto was skipped:\n%s", errOut)
			}
			var lookups int
			for _, call := range ghCalls(t, ghLog) {
				if strings.HasSuffix(call, "/branches/main/protection/required_status_checks") {
					lookups++
				}
			}
			if lookups != 1 {
				t.Errorf("the required checks were read %d times, want 1", lookups)
			}
		})
	}
}

// The trunk has required checks, so --auto is asked for and the forge decides.
// Queued, the item reads reviewed and says so on stderr; merged on the spot, it
// reads merged_immediately, the gate reads the merged head, and it finishes.
func TestRequiredChecksLeaveTheMergeToTheForge(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		ghLog := mergeProject(t)
		forge(t, ghLog)

		var rows []DriveItem
		errOut := captureErr(t, func() { rows = driveAll(t) })
		got := rows[0]
		if got.Outcome != outcomeReviewed || !got.Merged || got.Finished {
			t.Fatalf("item 1 is %q merged=%v finished=%v (%s), want reviewed, merged, not finished",
				got.Outcome, got.Merged, got.Finished, got.Reason)
		}
		if merges := mergeCalls(t, ghLog); len(merges) != 1 || !strings.Contains(merges[0], "--auto") {
			t.Errorf("merges = %q, want the one --auto request", merges)
		}
		if !strings.Contains(errOut, "· 1: merge queued at the forge; the next drive finishes it") {
			t.Errorf("stderr does not report the queued merge:\n%s", errOut)
		}
	})
	t.Run("merged at once", func(t *testing.T) {
		ghLog := mergeProject(t)
		dir, _ := forge(t, ghLog)
		t.Setenv("DROVER_TEST_AUTO_MERGES", "1")

		got := driveAll(t)[0]
		if got.Outcome != outcomeMergedAtForge || !got.Merged || !got.Finished || got.PR != 7 {
			t.Fatalf("item 1 is %q merged=%v finished=%v pr=%d (%s), want %q, merged, finished, PR 7",
				got.Outcome, got.Merged, got.Finished, got.PR, got.Reason, outcomeMergedAtForge)
		}
		head := git(t, dir, "rev-parse", got.Branch)
		if !strings.Contains(got.Reason, shortSHA(head)) || !strings.Contains(got.Reason, "merged") {
			t.Errorf("the reason does not name the merged head %s: %s", shortSHA(head), got.Reason)
		}
		if merges := mergeCalls(t, ghLog); len(merges) != 1 || !strings.Contains(merges[0], "--auto") {
			t.Errorf("merges = %q, want the one --auto request", merges)
		}
		if n := len(gateLog(t, dir)); n != 2 {
			t.Errorf("the merge gate ran %d times, want 2: once before the pull request and once on the merged head", n)
		}
		s, err := open()
		if err != nil {
			t.Fatal(err)
		}
		if !s.ledger.State("1").Merged {
			t.Error("item 1 merged at the forge and the ledger does not record it done")
		}
	})
	t.Run("merged at once and the merge gate refuses", func(t *testing.T) {
		ghLog := mergeProject(t)
		forge(t, ghLog)
		t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
		t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "1")

		rows, err := driveRows(t)
		if !errors.Is(err, errMergedRed) {
			t.Fatalf("drive ended on %v, want %v", err, errMergedRed)
		}
		if len(rows) != 1 {
			t.Fatalf("drive reported %d rows, want 1: the drive stops at the refused merge", len(rows))
		}
		got := rows[0]
		if got.Outcome != outcomeMergedAtForge || !got.Merged || got.Finished {
			t.Fatalf("item 1 is %q merged=%v finished=%v, want %q, merged, not finished",
				got.Outcome, got.Merged, got.Finished, outcomeMergedAtForge)
		}
		if !strings.Contains(got.Reason, "PR #7") || !strings.Contains(got.Reason, "ci/test: failure on linux") {
			t.Errorf("the reason does not name the pull request and the merge gate's output:\n%s", got.Reason)
		}
	})
	// The forge merges --no-ff, so origin's main is never an ancestor of the
	// merged head, which is what a squash or a rebase does as well. A gate
	// running the ancestry check as merge-gate.sh does refuses every such head
	// with exit 4 unless drive says the head was merged.
	t.Run("merged at once and the merge gate checks ancestry", func(t *testing.T) {
		ghLog := mergeProject(t)
		dir, _ := forge(t, ghLog)
		t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
		t.Setenv("DROVER_TEST_GATE_ANCESTRY", "on")

		rows, err := driveRows(t)
		if err != nil {
			t.Fatalf("drive ended on %v, want a clean finish", err)
		}
		got := rows[0]
		if got.Outcome != outcomeMergedAtForge || !got.Merged || !got.Finished {
			t.Fatalf("item 1 is %q merged=%v finished=%v (%s), want %q, merged, finished",
				got.Outcome, got.Merged, got.Finished, got.Reason, outcomeMergedAtForge)
		}
		b, err := os.ReadFile(filepath.Join(dir, "merge-gate.merged")) // #nosec G304 -- written by the fixture's own stub
		if err != nil {
			t.Fatal(err)
		}
		if seen := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"); len(seen) != 2 || seen[0] != "" || seen[1] != "1" {
			t.Errorf("the merge gate saw %s = %q, want empty before the pull request and 1 on the merged head", "DROVER_GATE_MERGED", seen)
		}
	})
	t.Run("merged at once and the merge gate ignores the merged flag", func(t *testing.T) {
		ghLog := mergeProject(t)
		forge(t, ghLog)
		t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
		t.Setenv("DROVER_TEST_GATE_ANCESTRY", "ignore")

		rows, err := driveRows(t)
		if !errors.Is(err, errMergedRed) {
			t.Fatalf("drive ended on %v, want %v", err, errMergedRed)
		}
		got := rows[0]
		if got.Finished {
			t.Error("item 1 finished on a merge gate that never read its checks")
		}
		if !strings.Contains(got.Reason, "exit 4") || !strings.Contains(got.Reason, "DROVER_GATE_MERGED=1") {
			t.Errorf("the reason does not say the merge gate called a merged head behind under %s=1:\n%s", "DROVER_GATE_MERGED", got.Reason)
		}
	})
	t.Run("state unreadable", func(t *testing.T) {
		ghLog := mergeProject(t)
		forge(t, ghLog)
		t.Setenv("DROVER_TEST_MERGED_FAIL", "1")

		got := driveAll(t)[0]
		if got.Outcome != outcomeReviewed || !got.Merged || got.Finished {
			t.Fatalf("item 1 is %q merged=%v finished=%v (%s), want reviewed, merged, not finished",
				got.Outcome, got.Merged, got.Finished, got.Reason)
		}
		if !strings.Contains(got.Reason, "could not be read") || !strings.Contains(got.Reason, "reconciliation decides it") {
			t.Errorf("the reason does not say the merge state went unread: %s", got.Reason)
		}
	})
}

// With no gate declared --auto is the only path, and a merge the forge made on
// the spot still finishes. drive refuses to merge with no gate declared, so the arm
// is reached here directly.
func TestAnAutoMergeWithoutAGateFinishesWhatTheForgeMerged(t *testing.T) {
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Loop.MergeGate = nil
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.promptData(it)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", data.Branch)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	git(t, dir, "push", "-q", "origin", data.Branch)
	git(t, dir, "switch", "-q", "main")

	d := DriveItem{ID: "1", Branch: data.Branch}
	park := func(cause, why string) DriveItem {
		d.Outcome, d.Park, d.Reason = outcomeParked, cause, why
		return d
	}
	got := s.driveMerge(t.Context(), it, &d, park, "main", time.Time{})
	if got.Outcome != outcomeMergedAtForge || !got.Merged || !got.Finished {
		t.Fatalf("item 1 is %q/%q merged=%v finished=%v (%s), want %q, merged and finished",
			got.Outcome, got.Park, got.Merged, got.Finished, got.Reason, outcomeMergedAtForge)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.Contains(c, "required_status_checks") {
			t.Errorf("drive read the required checks with no merge gate to decide on them: %q", c)
		}
	}
}

// B lists A in its deps and A merges at the forge on the spot. A finishes in
// the same drive, so B runs at its turn, whether named after A or appended by
// --follow, rather than skipping as blocked.
func TestADependentRunsBehindAMergeTheForgeMadeAtOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"named", []string{"1", "2"}},
		{"follow", []string{"--follow"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ghLog := mergeProject(t)
			dir, _ := forge(t, ghLog)
			t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
			write(t, filepath.Join(dir, "work", "02-second.md"),
				"---\ntitle: Second\ndeps: 1\n---\n\n1. Do it too.\n")

			rows := driveAll(t, c.args...)
			if len(rows) != 2 {
				t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
			}
			if a := rows[0]; a.Outcome != outcomeMergedAtForge || !a.Finished {
				t.Fatalf("item 1 is %q finished=%v (%s), want %q and finished", a.Outcome, a.Finished, a.Reason, outcomeMergedAtForge)
			}
			if b := rows[1]; b.ID != "2" || b.Outcome == outcomeSkipped {
				t.Errorf("item 2 is %s %q/%q (%s), want it run behind item 1", b.ID, b.Outcome, b.Park, b.Reason)
			}
		})
	}
}

// A gate that refuses at the merge parks the item with the pull request open
// and the gate's own last lines in the reason, and nothing merges.
func TestAGateRefusalAtTheMergeParksWithThePullRequestOpen(t *testing.T) {
	ghLog := mergeProject(t)
	forge(t, ghLog)
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "1")

	rows := driveAll(t)
	got := rows[0]
	if got.Outcome != outcomeParked || got.Park != parkGateRefused || got.PR != 7 || got.Merged {
		t.Fatalf("item 1 is %q/%q pr=%d merged=%v, want %q/%q, PR 7, not merged",
			got.Outcome, got.Park, got.PR, got.Merged, outcomeParked, parkGateRefused)
	}
	if !strings.Contains(got.Reason, "ci/test: failure on linux") {
		t.Errorf("the reason does not carry the merge gate's output:\n%s", got.Reason)
	}
	if merges := mergeCalls(t, ghLog); len(merges) != 1 {
		t.Errorf("merges = %q, want only the refused --auto attempt", merges)
	}
	f := driveFollowUp(got, "work/01-first.md", true, 0, polOperator)
	if !strings.Contains(f.Outcome, "PR #7 is open") {
		t.Errorf("the summary does not name the open pull request: %+v", f)
	}
}

// Without a gate nothing could authorize a merge the forge will not queue, so
// the refusal parks merge_failed. drive refuses to merge with
// no gate declared, so the arm is reached here directly. An error that is not
// the auto-merge refusal parks merge_failed with a gate too: the forge saying
// no to this pull request is not the gate's to overrule.
func TestAnAutoMergeRefusalWithoutAGateStaysMergeFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		gate   bool
		output string
	}{
		{"no merge gate", false, "GraphQL: Auto merge is not allowed for this repository (enablePullRequestAutoMerge)"},
		{"some other refusal", true, "GraphQL: Pull request is not mergeable (mergePullRequest)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ghLog := mergeProject(t)
			dir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			ghPath, err := exec.LookPath("gh")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("DROVER_TEST_GH_LOG", ghLog)
			t.Setenv("DROVER_TEST_GH_STDERR", c.output)
			stub(t, ghPath, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DROVER_TEST_GH_LOG\"\necho \"$DROVER_TEST_GH_STDERR\" >&2\nexit 1\n")
			git(t, dir, "switch", "-q", "-c", "b1")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			if !c.gate {
				s.cfg.Loop.MergeGate = nil
			}
			it, err := s.item("1")
			if err != nil {
				t.Fatal(err)
			}
			d := DriveItem{ID: "1", Branch: "b1"}
			park := func(cause, why string) DriveItem {
				d.Outcome, d.Park, d.Reason = outcomeParked, cause, why
				return d
			}
			got := s.driveMerge(t.Context(), it, &d, park, "main", time.Time{})
			if got.Park != "merge_failed" || got.Merged {
				t.Fatalf("parked %q merged=%v (%s), want merge_failed", got.Park, got.Merged, got.Reason)
			}
			if merges := mergeCalls(t, ghLog); len(merges) != 1 {
				t.Errorf("merges = %q, want the one --auto attempt", merges)
			}
		})
	}
}

// The trunk moved after the branch was pushed. drive merges it in, pushes the
// result, waits for that head's checks, and only then runs the gate, so the gate and
// the merge both read a branch level with the trunk.
func TestATrunkThatMovedIsMergedInBeforeTheGate(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "elsewhere.txt", "theirs")

	rows := driveAll(t)
	got := rows[0]
	if got.Outcome != outcomeReviewed || !got.Merged {
		t.Fatalf("item 1 is %q/%q (%s), want reviewed and merged", got.Outcome, got.Park, got.Reason)
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main")
	if ok, err := isAncestor(dir, moved, got.Branch); err != nil || !ok {
		t.Errorf("the branch does not hold the trunk's new commit %s (err %v)", moved, err)
	}
	head := git(t, dir, "rev-parse", got.Branch)
	if pushed := git(t, dir, "rev-parse", "origin/"+got.Branch); pushed != head {
		t.Errorf("origin holds %s for the branch and the branch is at %s: the branch update was not pushed", pushed, head)
	}
	seen := gateLog(t, dir)
	if len(seen) != 1 {
		t.Fatalf("the merge gate ran %d times, want 1", len(seen))
	}
	if _, local, remote := splitGateLine(t, seen[0]); local != head || remote != head {
		t.Errorf("the merge gate read local %s remote %s, want the updated head %s", local, remote, head)
	}
	var awaited bool
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "api ") && strings.Contains(c, head) {
			awaited = true
		}
		if strings.HasPrefix(c, "pr create") && !awaited {
			t.Error("the pull request opened before the updated head's checks were read")
		}
	}
	if !awaited {
		t.Error("nothing read the updated head's checks")
	}
	runs := checkLog(t, dir)
	if len(runs) != 1 {
		t.Fatalf("the item's verify command ran %d times on the updated tree, want 1: %+v", len(runs), runs)
	}
	if r := runs[0]; r.gate != "default" || r.head != head || !inItemTree(t, r.dir, "1") || r.pushed == head {
		t.Errorf("the item's verify command ran as %+v, want default on %s in the item's worktree, before the push", r, head)
	}
	if c := got.CatchUp; c == nil || c.Verify != catchUpPassed || c.To != moved {
		t.Errorf("update_branch = %+v, want to %s with verify %q", c, moved, catchUpPassed)
	}
}

// With no auto-merge the branch catches up twice: once before the pull request
// and again before the gate's merge. The second finds it level and merges
// nothing, and the row still reports the first.
func TestACatchUpSurvivesTheMergeByGatesSecondLook(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "elsewhere.txt", "theirs")
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	poll := checksPoll
	checksPoll = time.Millisecond
	t.Cleanup(func() { checksPoll = poll })

	got := driveAll(t)[0]
	if got.Outcome != outcomeMergedByGate || !got.Merged {
		t.Fatalf("item 1 is %q/%q (%s), want %q and merged", got.Outcome, got.Park, got.Reason, outcomeMergedByGate)
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main^")
	if c := got.CatchUp; c == nil || c.Verify != catchUpPassed || c.To != moved || c.From == "" {
		t.Errorf("update_branch = %+v, want from the old trunk to %s with verify %q", c, moved, catchUpPassed)
	}
}

// Every row a drive emits says when the item finished, whichever return
// produced it: a deferred stamp on a local the return had already copied would
// leave every one of these empty.
func TestEveryRowCarriesWhenItEnded(t *testing.T) {
	for _, c := range []struct {
		name, outcome, park string
		setup               func(t *testing.T)
	}{
		{"reviewed", outcomeReviewed, "", func(*testing.T) {}},
		{"merged by the merge gate", outcomeMergedByGate, "", func(t *testing.T) {
			t.Setenv("DROVER_TEST_NO_AUTO", "1")
			poll := checksPoll
			checksPoll = time.Millisecond
			t.Cleanup(func() { checksPoll = poll })
		}},
		{"merged at forge", outcomeMergedAtForge, "", func(t *testing.T) {
			t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
		}},
		// Refused before the pull request, where the item is sent back without
		// the park closure; at the merge the closure stamps it.
		{"merge gate refused", outcomeParked, parkGateRefused, func(t *testing.T) {
			t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")
		}},
		{"uncommitted work", outcomeParked, parkUncommitted, func(t *testing.T) {
			path, err := exec.LookPath("impl")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".orig"); err != nil {
				t.Fatal(err)
			}
			stub(t, path, "#!/bin/sh\n\"$0.orig\" \"$@\"; rc=$?\necho left > left-behind.txt\nexit $rc\n")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ghLog := mergeProject(t)
			forge(t, ghLog)
			c.setup(t)

			got := driveAll(t)[0]
			if got.Outcome != c.outcome || got.Park != c.park {
				t.Fatalf("item 1 is %q/%q (%s), want %q/%q", got.Outcome, got.Park, got.Reason, c.outcome, c.park)
			}
			started, err := time.Parse(time.RFC3339, got.Started)
			if err != nil {
				t.Fatalf("started %q: %v", got.Started, err)
			}
			ended, err := time.Parse(time.RFC3339, got.Ended)
			if err != nil {
				t.Fatalf("ended %q: %v", got.Ended, err)
			}
			if ended.Before(started) {
				t.Errorf("ended %s is before started %s", got.Ended, got.Started)
			}
		})
	}
}

// checkRun is one line of check.log: one run of the item's verify command.
type checkRun struct{ gate, dir, head, pushed string }

func checkLog(t *testing.T, dir string) []checkRun {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "check.log")) // #nosec G304 -- written by the fixture's own stub
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []checkRun
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			t.Fatalf("check.log line %q is not <verify> dir= head= pushed=", line)
		}
		out = append(out, checkRun{f[0], strings.TrimPrefix(f[1], "dir="),
			strings.TrimPrefix(f[2], "head="), strings.TrimPrefix(f[3], "pushed=")})
	}
	return out
}

// inItemTree reports whether a directory the check logged is item id's
// worktree. The tree is gone by the time a test reads the log, so its path is
// compared by the components drive names it with rather than resolved.
func inItemTree(t *testing.T, dir, id string) bool {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.treePath(it)
	if err != nil {
		t.Fatal(err)
	}
	tail := filepath.Join(filepath.Base(filepath.Dir(want)), filepath.Base(want))
	return strings.HasSuffix(dir, string(filepath.Separator)+tail)
}

// useVerifyCommands replaces the fixture's [verify] table, which mergeProject points at
// its check stub, with body.
func useVerifyCommands(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)\[verify\]\n.*?\n\n`)
	if !re.Match(raw) {
		t.Fatal("the fixture config has no [verify] table to replace")
	}
	write(t, path, re.ReplaceAllLiteralString(string(raw), "[verify]\n"+body+"\n"))
}

// A branch already level with the trunk was verified by its own sessions, and
// drive spends nothing re-running that.
func TestAnUnmovedTrunkRerunsNoVerify(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)

	got := driveAll(t)[0]
	if got.Outcome != outcomeReviewed || !got.Merged {
		t.Fatalf("item 1 is %q/%q (%s), want reviewed and merged", got.Outcome, got.Park, got.Reason)
	}
	if runs := checkLog(t, dir); len(runs) != 0 {
		t.Errorf("drive ran the item's verify command %d times on a branch the trunk never moved under: %+v", len(runs), runs)
	}
	if got.CatchUp != nil {
		t.Errorf("update_branch = %+v on an item nothing was merged into, want it omitted", got.CatchUp)
	}
}

// An item whose frontmatter selects a verify command gets that command on the
// caught-up tree, the one its own run was told to pass, and not default.
func TestACatchUpRerunsTheVerifyCommandTheItemsFrontmatterSelects(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\nui_only: yes\n---\n\n1. Do it. REVIEW-PASSES\n")
	moveTrunk(t, remote, "elsewhere.txt", "theirs")

	got := driveAll(t)[0]
	runs := checkLog(t, dir)
	if len(runs) != 1 || runs[0].gate != "ui_only" {
		t.Fatalf("the updated tree ran %+v, want the ui_only verify command once", runs)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpPassed {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpPassed)
	}
}

// Two branches can each add a migration with the same number, neither
// conflicting in text, and the tree a catch-up makes from the two fails what
// each passed alone. Here the trunk gains a.txt, the branch adds
// implemented.txt, and the verify command fails only where both exist. The item
// goes back to its branch with the verify command's output and the trunk's
// range, and nothing reaches the forge as mergeable.
func TestACatchUpTheVerifyCommandFailsGoesBackToTheBranch(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")

	got := driveAll(t)[0]
	if got.Outcome != outcomeParked || got.Park != parkGateRefused || got.PR != 0 || got.Merged {
		t.Fatalf("item 1 is %q/%q pr=%d merged=%v (%s), want parked %q with no pull request",
			got.Outcome, got.Park, got.PR, got.Merged, got.Reason, parkGateRefused)
	}
	if got.Rounds != 1 {
		t.Errorf("rounds = %d, want 1: the request for changes counts as a round", got.Rounds)
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main")
	from := git(t, dir, "rev-parse", moved+"^")
	if c := got.CatchUp; c == nil || c.Verify != catchUpFailed || c.From != from || c.To != moved {
		t.Errorf("update_branch = %+v, want from %s to %s, verify %q", c, from, moved, catchUpFailed)
	}
	if ok, err := isAncestor(dir, moved, "origin/"+got.Branch); err != nil || !ok {
		t.Errorf("the merge commit that failed is not on origin/%s (err %v): the next run starts elsewhere", got.Branch, err)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr create") || strings.HasPrefix(c, "pr merge") {
			t.Errorf("a branch whose updated tree failed reached the forge: %q", c)
		}
	}

	runs := checkLog(t, dir)
	if len(runs) != 2 {
		t.Fatalf("the verify command ran %d times, want 2 (the merged tree, then the trunk alone): %+v", len(runs), runs)
	}
	if !inItemTree(t, runs[0].dir, "1") || runs[0].pushed == runs[0].head {
		t.Errorf("the first run was %+v, want the item's worktree before the merge was pushed", runs[0])
	}
	if runs[1].head != moved || inItemTree(t, runs[1].dir, "1") {
		t.Errorf("the second run was %+v, want origin's main %s alone, outside the item's worktree", runs[1], moved)
	}

	body, err := os.ReadFile(filepath.Join(dir, "work", "01-first.md")) // #nosec G304 -- the fixture's own item
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"origin/main (" + shortSHA(from) + ".." + shortSHA(moved) + ")",
		"elsewhere",
		"passes the same verify command on its own",
		"check: a.txt implemented.txt cannot all exist",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the request for changes does not carry %q:\n%s", want, body)
		}
	}
}

// A trunk that fails the item's verify command alone is no item's to fix. drive
// stops, sends nothing back and pushes nothing, and the local branch loses the merge so
// the next drive does not push it either.
func TestATrunkThatFailsTheVerifyCommandAloneStopsTheDrive(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt")

	rows, err := driveRows(t)
	if !errors.Is(err, errTrunkRed) {
		t.Fatalf("drive ended on %v, want %v", err, errTrunkRed)
	}
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1: it stops at the red trunk", len(rows))
	}
	got := rows[0]
	if got.Park != parkTrunkRed || got.Rounds != 0 || !strings.Contains(got.Reason, "cannot all exist") {
		t.Errorf("item 1 is %q rounds=%d (%s), want %q with the trunk run's output, no round", got.Park, got.Rounds, got.Reason, parkTrunkRed)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpTrunkRed {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpTrunkRed)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if n := s.ledger.Counts("1").ChangesRequested; n != 1 {
		t.Errorf("item 1 has %d changes rows, want the fixture's 1: a red trunk is not this item's finding", n)
	}
	git(t, dir, "fetch", "-q", "origin")
	for _, ref := range []string{got.Branch, "origin/" + got.Branch} {
		if ok, _ := isAncestor(dir, "origin/main", ref); ok {
			t.Errorf("%s holds the trunk the verify command failed: the merge was kept", ref)
		}
	}
}

// Two items caught up to the same trunk commit ask the same question of it, so
// the trunk alone is verified once per drive. Item 2's branch exists before the
// trunk moves, which is a branch a previous drive pushed, and its review passes.
func TestTheTrunkAloneIsVerifiedOncePerCommit(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	write(t, filepath.Join(dir, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it too. SECOND-PASSES\n")
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rev, rev+".orig"); err != nil {
		t.Fatal(err)
	}
	stub(t, rev, `#!/bin/sh
"$0.orig" "$@"; rc=$?
case "$*" in *SECOND-PASSES*)
  printf '%s\n' '{"id":"2","level":"reviewed","code_by":"agent","independent":true,"date":"2026-09-20"}' >> "$DROVER_ROOT/ledger.jsonl" ;;
esac
exit $rc
`)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.item("2")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.branchFor(second)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "branch", b2, "main")
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")
	// Off, so item 2 takes the trunk at its catch-up after review, where the
	// trunk alone is verified, rather than before its session starts.
	appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"off\"\n")

	rows := driveAll(t)
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Park != parkGateRefused || r.CatchUp == nil || r.CatchUp.Verify != catchUpFailed {
			t.Errorf("item %s is %q update_branch=%+v (%s), want %q with verify %q", r.ID, r.Park, r.CatchUp, r.Reason, parkGateRefused, catchUpFailed)
		}
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main")
	var alone int
	for _, r := range checkLog(t, dir) {
		if r.head == moved {
			alone++
		}
	}
	if alone != 1 {
		t.Errorf("the trunk alone was gated %d times, want 1", alone)
	}
}

// A project with no verify command has chosen no local check, so the catch-up
// says it went unchecked and merges.
func TestACatchUpWithNoVerifyCommandSaysSoAndMerges(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	useVerifyCommands(t, dir, "")
	moveTrunk(t, remote, "elsewhere.txt", "theirs")

	var rows []DriveItem
	stderr := captureErr(t, func() { rows = driveAll(t) })
	got := rows[0]
	if got.Outcome != outcomeReviewed || !got.Merged {
		t.Fatalf("item 1 is %q/%q (%s), want reviewed and merged", got.Outcome, got.Park, got.Reason)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpSkipped {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpSkipped)
	}
	if n := strings.Count(stderr, "no verify command exists to verify it"); n != 1 {
		t.Errorf("stderr says the updated head went unchecked %d times, want 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "updated to main@") {
		t.Errorf("the summary does not name the branch update:\n%s", stderr)
	}
}

// Where the forge has no auto-merge the catch-up before the merge gets the
// same rerun. The trunk moves while the forge refuses --auto, so the catch-up
// that merges it is mergeByGate's, with the pull request already open: it stays
// open and unmerged, and --match-head-commit never sees the failed head.
func TestARedCatchUpAtTheMergeKeepsThePullRequestOpen(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	moveTrunkAfter(t, remote, "gh", "pr merge --auto*", "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")

	got := driveAll(t)[0]
	if got.Park != parkGateRefused || got.PR != 7 || got.Merged {
		t.Fatalf("item 1 is %q pr=%d merged=%v (%s), want %q with PR 7 open", got.Park, got.PR, got.Merged, got.Reason, parkGateRefused)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpFailed {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpFailed)
	}
	if merges := mergeCalls(t, ghLog); len(merges) != 1 {
		t.Errorf("merges = %q, want only the refused --auto attempt", merges)
	}
	if n := len(gateLog(t, dir)); n != 1 {
		t.Errorf("[drive] merge_gate ran %d times, want 1: the red branch update stops before it", n)
	}
}

// At [review] max_review_rounds the catch-up's finding is refused like any other, and
// the park says so.
func TestARedCatchUpAtTheCapParksWithTheRefusal(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[review]\nmax_review_rounds = 1\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")

	got := driveAll(t)[0]
	if got.Park != parkGateRefused || got.Rounds != 0 || !strings.Contains(got.Reason, "max_review_rounds") {
		t.Errorf("item 1 is %q rounds=%d (%s), want %q naming max_review_rounds, no round", got.Park, got.Rounds, got.Reason, parkGateRefused)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpFailed {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpFailed)
	}
}

// A trunk worktree that will not set up leaves the failure unattributed, so the
// item parks gate_unrunnable, nothing is pushed, and the worktree is gone. The
// setup refuses a detached head, which only the trunk's worktree has.
func TestATrunkTreeThatWillNotSetUpParksUnrunnable(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("worktree_setup = [\"git\", \"symbolic-ref\", \"-q\", \"HEAD\"]\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")

	got := driveAll(t)[0]
	if got.Park != "gate_unrunnable" || !strings.Contains(got.Reason, "worktree_setup") {
		t.Fatalf("item 1 is %q (%s), want gate_unrunnable naming worktree_setup", got.Park, got.Reason)
	}
	if got.CatchUp == nil || got.CatchUp.Verify != catchUpUnrunnable {
		t.Errorf("update_branch = %+v, want verify %q", got.CatchUp, catchUpUnrunnable)
	}
	git(t, dir, "fetch", "-q", "origin")
	if ok, _ := isAncestor(dir, "origin/main", "origin/"+got.Branch); ok {
		t.Error("the unattributed merge was pushed")
	}
	if list := git(t, dir, "worktree", "list", "--porcelain"); strings.Contains(list, "detached") {
		t.Errorf("the trunk's worktree outlived the check:\n%s", list)
	}
}

// An interrupt while the verify command runs on the caught-up tree parks the item where
// it stands: the worktree stays, and the merge is neither kept nor pushed.
func TestAnInterruptDuringTheCatchUpVerifyParks(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "elsewhere.txt", "theirs")
	hung := filepath.Join(t.TempDir(), "hung")
	t.Setenv("DROVER_TEST_CHECK_HANG", hung)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	driveCmd.SetContext(ctx)
	t.Cleanup(func() { driveCmd.SetContext(context.Background()) })
	go func() {
		for {
			if _, err := os.Stat(hung); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	rows, _ := driveRows(t)
	if took := time.Since(start); took > 25*time.Second {
		t.Errorf("drive took %s: the interrupt did not stop the verify command", took)
	}
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Park != "interrupted" || got.CatchUp == nil || got.CatchUp.Verify != catchUpUnrunnable {
		t.Fatalf("item 1 is %q update_branch=%+v (%s), want interrupted with verify %q", got.Park, got.CatchUp, got.Reason, catchUpUnrunnable)
	}
	if got.Tree == "" {
		t.Error("the row names no worktree: an interrupted verify command's tree is the operator's to read")
	} else if _, err := os.Stat(got.Tree); err != nil {
		t.Errorf("the worktree is gone: %v", err)
	}
	git(t, dir, "fetch", "-q", "origin")
	for _, ref := range []string{got.Branch, "origin/" + got.Branch} {
		if ok, _ := isAncestor(dir, "origin/main", ref); ok {
			t.Errorf("%s holds the trunk the interrupted verify command never passed", ref)
		}
	}
}

// The merge hold reads the branch's own change. A catch-up merges origin's trunk
// into the branch and leaves the local trunk where it was, so a hold measured
// against the local trunk would charge the item with every file the trunk
// gained since, a denied one included. The control is the item touching a
// denied path itself, which still holds after the same catch-up.
func TestACatchUpDoesNotChargeTheItemWithTheTrunksFiles(t *testing.T) {
	for _, tc := range []struct {
		name, deny string
		hold       bool
	}{
		{"the trunk touched it", "deny.txt", false},
		{"the item touched it", "implemented.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			denyPaths(t, dir, tc.deny)
			moveTrunk(t, remote, "deny.txt", "theirs")

			got := driveAll(t)[0]
			switch {
			case tc.hold && got.Park != "protected_paths":
				t.Fatalf("item 1 is %q/%q (%s), want parked protected_paths", got.Outcome, got.Park, got.Reason)
			case tc.hold && strings.Contains(got.Reason, "deny.txt"):
				t.Errorf("the hold names the trunk's file as the item's:\n%s", got.Reason)
			case !tc.hold && (got.Outcome != outcomeReviewed || !got.Merged):
				t.Fatalf("item 1 is %q/%q (%s), want reviewed and merged", got.Outcome, got.Park, got.Reason)
			}
			git(t, dir, "fetch", "-q", "origin")
			if ok, err := isAncestor(dir, "origin/main", got.Branch); !tc.hold && (err != nil || !ok) {
				t.Errorf("the branch never took the trunk, so nothing here exercised the branch update (err %v)", err)
			}
		})
	}
}

// Every reader of what a branch carries measures from where it left the trunk,
// with origin's trunk counted as the trunk. Here the branch has taken origin's
// trunk the way catchUp merges it, and the local trunk has gone its own way
// with a commit nobody pushed, so neither trunk alone is the branch's base and
// fast-forwarding the local one could not make it so.
func TestReadersOfABranchSkipWhatACatchUpMergedIn(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	write(t, filepath.Join(dir, "shared.txt"), "one\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "shared")
	remote := git(t, dir, "remote", "get-url", "origin")
	git(t, dir, "push", "-q", "origin", "main")

	other := filepath.Join(t.TempDir(), "other")
	git(t, dir, "clone", "-q", remote, other)
	write(t, filepath.Join(other, "shared.txt"), "two\n")
	write(t, filepath.Join(other, "theirs.txt"), "theirs\n")
	git(t, other, "add", "-A")
	git(t, other, "-c", "user.name=o", "-c", "user.email=o@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "elsewhere")
	git(t, other, "push", "-q", "origin", "HEAD:main")

	git(t, dir, "switch", "-q", "-c", "work")
	write(t, filepath.Join(dir, "own.txt"), "mine\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "own")
	git(t, dir, "fetch", "-q", "origin", "main")
	git(t, dir, "merge", "-q", "--no-edit", "origin/main")
	git(t, dir, "switch", "-q", "main")
	write(t, filepath.Join(dir, "unpushed.txt"), "local\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "unpushed")

	if got, err := s.changedNames("work", "main"); err != nil || !slices.Equal(got, []string{"own.txt"}) {
		t.Errorf("changedNames = %q (err %v), want only own.txt", got, err)
	}
	if modified, _, why := s.scopeOfDiff("work", ""); modified != 0 {
		t.Errorf("scopeOfDiff counts %d modified (%s), want 0: shared.txt changed on the trunk", modified, why)
	}
	commits, why := s.recentCommits("work")
	if why != "" || len(commits) != 1 {
		t.Errorf("recentCommits = %+v (%s), want the one commit the branch made", commits, why)
	}
}

// denyPaths appends [drive] protected_paths to the project config, the last table
// mergeProject writes.
func denyPaths(t *testing.T, dir string, globs ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, "protected_paths = [%q]\n", strings.Join(globs, `", "`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A trunk that will not merge into the branch parks it, names the files, and
// leaves the remote exactly where the last push put it.
func TestATrunkConflictParksAndForcesNothing(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "implemented.txt", "theirs")

	rows := driveAll(t)
	got := rows[0]
	if got.Park != parkTrunkConflict || got.Merged {
		t.Fatalf("item 1 is %q/%q (%s), want parked %q", got.Outcome, got.Park, got.Reason, parkTrunkConflict)
	}
	if !strings.Contains(got.Reason, "implemented.txt") {
		t.Errorf("the park does not name the conflicted file:\n%s", got.Reason)
	}
	git(t, dir, "fetch", "-q", "origin")
	head := git(t, dir, "rev-parse", got.Branch)
	if pushed := git(t, dir, "rev-parse", "origin/"+got.Branch); pushed != head {
		t.Errorf("origin holds %s for the branch and the branch is at %s", pushed, head)
	}
	if ok, _ := isAncestor(dir, "origin/main", got.Branch); ok {
		t.Error("the branch holds the conflicting trunk: the merge was not aborted")
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr create") || strings.HasPrefix(c, "pr merge") {
			t.Errorf("a conflicted branch reached the forge: %q", c)
		}
	}
	if len(gateLog(t, dir)) != 0 {
		t.Error("the merge gate ran over a branch that never updated")
	}
}

// --follow appends what a finish unblocked. Item 3 depends on item 1, so it is
// not in the queue the drive starts with; once item 1 finishes it is ready, and the
// same drive picks it up. Without the flag the list stays as it started.
func TestFollowPicksUpWhatAFinishUnblocked(t *testing.T) {
	for _, follow := range []bool{true, false} {
		t.Run(fmt.Sprintf("follow=%v", follow), func(t *testing.T) {
			ghLog := mergeProject(t)
			dir, _ := forge(t, ghLog)
			t.Setenv("DROVER_TEST_NO_AUTO", "1")
			write(t, filepath.Join(dir, "work", "03-third.md"),
				"---\ntitle: Third\ndeps: 1\n---\n\n1. Build on the first.\n")

			var args []string
			if follow {
				args = append(args, "--follow")
			}
			rows := driveAll(t, args...)
			var ids []string
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			want := []string{"1", "2"}
			if follow {
				want = append(want, "3")
			}
			if !slices.Equal(ids, want) {
				t.Errorf("drove %v, want %v", ids, want)
			}
			if !rows[0].Finished {
				t.Fatalf("item 1 did not finish (%s), so nothing could unblock", rows[0].Reason)
			}
		})
	}
}

// [git] merge_method is the flag every merge carries, queued or not.
func TestMergeMethodPicksTheFlag(t *testing.T) {
	for _, method := range config.MergeMethods() {
		t.Run(method, func(t *testing.T) {
			dir := newRepo(t)
			git(t, dir, "switch", "-q", "-c", "b1")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
			head := git(t, dir, "rev-parse", "b1")
			bin := t.TempDir()
			log := filepath.Join(bin, "gh.log")
			t.Setenv("DROVER_TEST_GH_LOG", log)
			stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DROVER_TEST_GH_LOG\"\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			s := &session{cfg: &config.Config{Root: dir}}
			s.cfg.Git.MergeMethod = method
			for _, auto := range []bool{true, false} {
				if err := s.mergeBranch(t.Context(), "b1", auto); err != nil {
					t.Fatal(err)
				}
			}
			calls := ghCalls(t, log)
			want := []string{
				"pr merge --auto --" + method + " --match-head-commit " + head + " b1",
				"pr merge --" + method + " --match-head-commit " + head + " b1",
			}
			if !slices.Equal(calls, want) {
				t.Errorf("gh saw %q, want %q", calls, want)
			}
		})
	}
}

func TestAutoMergeRefusedMatchesTheRefusalAndNothingElse(t *testing.T) {
	for out, want := range map[string]bool{
		"GraphQL: Auto merge is not allowed for this repository (enablePullRequestAutoMerge)":                      true,
		"GraphQL: Pull request Protected branch rules not configured for this branch (enablePullRequestAutoMerge)": true,
		"GraphQL: Pull request is not mergeable (mergePullRequest)":                                                false,
		"X Pull request #7 is not mergeable: the merge commit cannot be cleanly created.":                          false,
		"": false,
	} {
		if got := autoMergeRefused(out); got != want {
			t.Errorf("autoMergeRefused(%q) = %v, want %v", out, got, want)
		}
	}
}

// standingProject is mergeProject with item 1 carrying the block an earlier
// round sent back, which nothing in the coming pass rewrites.
func standingProject(t *testing.T) (dir, ghLog string) {
	t.Helper()
	ghLog = mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES\n\n"+
			prompts.ChangesHeading+"\n\nR1 unmet: the handler still drops the error.\n")
	return dir, ghLog
}

// A review can find the earlier round's findings still open, record its audit
// and write nothing. The digest does not move, so the rows read as a clean pass
// over a body that says the opposite.
func TestAnAuditOverAStandingBlockParks(t *testing.T) {
	dir, ghLog := standingProject(t)

	rows := driveAll(t)
	if len(rows) == 0 || rows[0].ID != "1" {
		t.Fatalf("drive reported %+v, want item 1 first", rows)
	}
	got := rows[0]
	if got.Outcome != outcomeParked || got.Park != passStanding || got.Merged {
		t.Fatalf("item 1 is %q/%q merged=%v (%s), want parked %q", got.Outcome, got.Park, got.Merged, got.Reason, passStanding)
	}
	for _, want := range []string{"drover request-changes <id>", "drover close-changes <id>", prompts.ChangesHeading} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("the park reason does not name %q: %s", want, got.Reason)
		}
	}
	for _, c := range mergeCalls(t, ghLog) {
		if strings.Contains(c, got.Branch) {
			t.Errorf("gh was asked to merge the parked item: %q", c)
		}
	}

	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reviewPassed(it); err == nil || !strings.Contains(err.Error(), passStanding) {
		t.Errorf("propose after a standing pass: %v, want a refusal naming %q", err, passStanding)
	}
}

// The same item once `drover close-changes` discharged the block: the review's audit
// over a cleared body is a pass, and the item merges.
func TestAClearedBlockPasses(t *testing.T) {
	standingProject(t)
	if err := execute(t, "close-changes", "1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	rows := driveAll(t)
	if len(rows) == 0 || rows[0].ID != "1" {
		t.Fatalf("drive reported %+v, want item 1 first", rows)
	}
	if got := rows[0]; got.Park != "" || !got.Merged {
		t.Errorf("item 1 is %q/%q merged=%v (%s), want merged", got.Outcome, got.Park, got.Merged, got.Reason)
	}
}

// The park names the follow-up for this item, and the clear it names is what
// open-pr then honors, so an item parked this way needs no open-pr --force
// after the clear.
func TestACloseChangesAfterAStandingPassLetsOpenPRRunTheGate(t *testing.T) {
	dir, ghLog := standingProject(t)

	rows := driveAll(t)
	if len(rows) == 0 || rows[0].Park != passStanding {
		t.Fatalf("drive reported %+v, want item 1 parked %q", rows, passStanding)
	}
	// The stub's audit carries no note, so the park names both exits for item 1.
	for _, want := range []string{"drover close-changes 1 && drover open-pr 1", "drover request-changes 1 -f <file>",
		"did not merge 1", "depends on it is blocked"} {
		if !strings.Contains(rows[0].Reason, want) {
			t.Errorf("the park reason does not name %q: %s", want, rows[0].Reason)
		}
	}

	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reviewPassed(it); err == nil || !strings.Contains(err.Error(), passStanding) {
		t.Fatalf("propose before any clear: %v, want a refusal naming %q", err, passStanding)
	}

	if err := execute(t, "close-changes", "1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	s = openIn(t, dir)
	if it, err = s.item("1"); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewPassed(it); err != nil {
		t.Fatalf("propose after a clear that follows the standing pass: %v, want it read as passed", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "merge-gate.log")) // #nosec G304 -- the fixture's own log
	if err := execute(t, "open-pr", "1"); err != nil {
		t.Fatalf("propose: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "merge-gate.log")) // #nosec G304 -- the fixture's own log
	if err != nil || len(after) <= len(before) {
		t.Errorf("propose did not run the merge gate after the clear (merge-gate.log %q)", after)
	}
	created := false
	for _, c := range ghCalls(t, ghLog) {
		created = created || strings.HasPrefix(c, "pr create")
	}
	if !created {
		t.Errorf("propose opened no pull request after the clear: %q", ghCalls(t, ghLog))
	}
}

func TestStandingNextReadsTheAuditNote(t *testing.T) {
	for note, want := range map[string]string{
		"R4 UNMET, unchanged":               "drover request-changes 7 -f <file>",
		"R1-R3 met; R4 unresolved":          "drover request-changes 7 -f <file>",
		"every finding in the block closed": "drover close-changes 7 && drover open-pr 7",
		"all fixed":                         "drover close-changes 7 && drover open-pr 7",
		"All met; each finding resolved.":   "drover close-changes 7 && drover open-pr 7",
	} {
		got := standingNext("7", note)
		if !strings.Contains(got, want) {
			t.Errorf("note %q: %s, want it to name %q", note, got, want)
		}
		if strings.Contains(got, "drover close-changes") && strings.Contains(got, "drover request-changes") {
			t.Errorf("note %q named both exits, want one: %s", note, got)
		}
	}
	if got := standingNext("7", ""); !strings.Contains(got, "drover close-changes 7") || !strings.Contains(got, "drover request-changes 7") {
		t.Errorf("an empty note names %s, want both exits", got)
	}
	// Each of these holds a closed word or none, and none of them says the
	// block is closed. Recommending open-pr for one merges an open finding.
	for _, note := range []string{
		"R4 not closed", "R2 remains unfixed", "R3 not resolved", "R5 not addressed",
		"R2 isn't fixed", "R1 fails", "R4 still broken", "R2 fixed, R4 still open",
		"never resolved", "R3 has no fixed test",
	} {
		got := standingNext("7", note)
		if strings.Contains(got, "drover open-pr") {
			t.Errorf("note %q: %s, want no propose", note, got)
		}
		if !strings.Contains(got, "drover request-changes 7 -f <file>") {
			t.Errorf("note %q: %s, want it to name request for changes", note, got)
		}
	}
	// A closed word inside a longer word is not the word. A closed word that
	// speaks for one finding, or beside a clause that is not a whole-block
	// assertion, cannot say the block is closed: the open finding may be in
	// wording nobody listed.
	for _, note := range []string{
		"R3 disclosed in the PR", "R1 prefixed with a guard",
		"R2 fixed in 1a2b3c, traced to test",
		"R1 fixed; R3 needs work", "R1, R2 fixed. R4 missing a test", "R1 closed. R4 incomplete",
		"R1 fixed, R6 docs absent", "R4 incorrectly closed",
		"all fixed except R4", "all of R1-R3 fixed; R4 needs work", "every finding closed, docs absent",
		"most findings fixed", "fixed all of them",
		"All findings closed and R4 needs a test", "Each of R1 and R2 fixed while R4 lacks docs",
		"all R1-R3 fixed and R4 remains",
		"all 3 of 4 findings fixed", "all findings 50% fixed", "each of 3/4 fixed",
		"all findings fixed 除了一个", "all findings fixed (R4 aside)",
	} {
		got := standingNext("7", note)
		if !strings.Contains(got, "says neither") || !strings.Contains(got, "drover request-changes 7") {
			t.Errorf("note %q: %s, want both exits, not propose alone", note, got)
		}
	}
}

// A ledger written before bodySum carries the exact-bytes digest of a body
// ending in a newline. An item mid-review when the binary changed reads that
// row as its own body, in drive and in open-pr.
func TestAnExactBytesDigestStillMatches(t *testing.T) {
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n")
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") || digestBytes(raw) == bodySum(raw) {
		t.Fatalf("the fixture body does not end in a newline, so both digests agree: %q", raw)
	}
	exact := digestBytes(raw)

	launch := ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Body: exact, Date: "2026-10-01"}
	audit := ledger.Row{ID: "1", Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true, Date: "2026-10-01"}
	p := pass{before: rowCounts(), after: rowCounts(launch, audit), logged: true, res: ok}
	p.readBody(it.Path)
	if got := passResult(p); got != passPassed {
		t.Errorf("drive read an unchanged body against its exact-bytes digest as %q, want %q", got, passPassed)
	}

	if _, err := reviewPassedOver(t, "", []ledger.Row{launch, audit}); err != nil {
		t.Errorf("propose refused an unchanged body against its exact-bytes digest: %v", err)
	}
}

// goldenDrive is the jobs-1 drive of mergeProject's two items, stderr then
// stdout, with every value that differs between two runs of the same queue
// replaced by a placeholder: paths under a temporary directory, commit ids,
// instants and durations.
func goldenDrive(t *testing.T, args ...string) string {
	t.Helper()
	var stdout string
	stderr := captureErr(t, func() {
		stdout = capture(t, func() {
			if err := execute(t, append([]string{"drive", "--json"}, args...)...); err != nil {
				t.Fatalf("drive: %v", err)
			}
		})
	})
	return normalizeDrive("== stderr\n" + stderr + "== stdout\n" + stdout)
}

var driveVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`/[^\s"':,()]*/(TestDrive[^/\s"]*|drover-[a-z]+-[0-9]+)[^\s"':,()]*`), "<path>"},
	{regexp.MustCompile(`\b[0-9a-f]{7,40}\b`), "<sha>"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})`), "<time>"},
	{regexp.MustCompile(`\d{8}-\d{6}(\.\d+)?`), "<stamp>"},
	{regexp.MustCompile(`after [0-9.]+(ns|µs|ms|s)\b`), "after <d>"},
}

func normalizeDrive(s string) string {
	for _, v := range driveVolatile {
		s = v.re.ReplaceAllString(s, v.with)
	}
	return s
}

// The jobs-1 drive is a sequential drive, byte for byte once the volatile
// values are masked. DROVER_UPDATE_GOLDEN=1 rewrites
// the file, which is only ever right on a change that means to alter what a
// sequential drive prints.
func TestDriveJobsOneMatchesTheGolden(t *testing.T) {
	mergeProject(t)
	got := goldenDrive(t, "--jobs", "1")
	path := filepath.Join(goldenDir(t), "drive-jobs-1.golden")
	if os.Getenv("DROVER_UPDATE_GOLDEN") != "" {
		write(t, path, got)
	}
	want, err := os.ReadFile(path) // #nosec G304 -- the test's own fixture
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("a jobs-1 drive no longer prints what it printed before --jobs:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func goldenDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(thisRepo, "internal", "cmd", "testdata")
}

// jobsStubs replaces mergeProject's runners with ones that record, in log, which
// other items were mid-session when each one started. It marks itself before it
// looks, so of two items starting together at least one sees the other. An item counts from its
// implement run's start to its review's start; the review records nothing
// unless the body carries REVIEW-PASSES-<id>, and then records an audit for
// that id. A body carrying SLOW holds its implement run for 3 seconds, after
// the file DROVER_TEST_SLOW_UNTIL names exists where that is set.
//
// Each item commits a file of its own, so two items merged one after the other
// meet on the trunk without a conflict.
func jobsStubs(t *testing.T, log string) {
	t.Helper()
	active := filepath.Join(filepath.Dir(log), "active")
	if err := os.MkdirAll(active, 0o750); err != nil {
		t.Fatal(err)
	}
	impl, err := exec.LookPath("impl")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_ACTIVE", active)
	t.Setenv("DROVER_TEST_JOBS_LOG", log)
	stub(t, impl, `#!/bin/sh
id=$(basename "$(pwd)")
touch "$DROVER_TEST_ACTIVE/$id"
printf '%s:%s\n' "$id" "$(ls "$DROVER_TEST_ACTIVE" | grep -vxF "$id" | tr '\n' ' ')" >> "$DROVER_TEST_JOBS_LOG"
echo work > "implemented-$id.txt"
git add "implemented-$id.txt"
git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m "the run did $id"
case "$*" in
  *SLOW*)
    if [ -n "${DROVER_TEST_SLOW_UNTIL:-}" ]; then
      n=0
      while [ ! -e "$DROVER_TEST_SLOW_UNTIL" ] && [ $n -lt 600 ]; do sleep 0.1; n=$((n + 1)); done
    fi
    sleep 3 ;;
  *) sleep 1 ;;
esac
echo '{"type":"result","subtype":"success","is_error":false}'
`)
	stub(t, rev, `#!/bin/sh
rm -f "$DROVER_TEST_ACTIVE/$(basename "$(pwd)")"
echo '{"type":"result","subtype":"success","is_error":false}'
id=$(printf '%s' "$*" | sed -n 's/.*REVIEW-PASSES-\([0-9][0-9]*\).*/\1/p' | head -n 1)
if [ -n "$id" ]; then
  printf '{"id":"%s","level":"reviewed","code_by":"agent","independent":true,"date":"2026-10-14"}\n' "$id" >> "$DROVER_ROOT/ledger.jsonl"
fi
`)
}

// jobsStarts is what jobsStubs logged: for each item's worktree name, the
// worktree names already mid-session when it started.
func jobsStarts(t *testing.T, log string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(log) // #nosec G304 -- the test's own log
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		id, others, _ := strings.Cut(line, ":")
		out[id] = strings.Fields(others)
	}
	return out
}

// treeName is the last element of an item's worktree path, which is what
// jobsStubs logs it under.
func treeName(t *testing.T, id string) string {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.treePath(it)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(p)
}

// Two items citing one file never run at once, whatever --jobs allows: the
// later is not dispatched until the earlier parks. An item citing nothing runs
// beside the first, which is what shows the slots were really there. The
// preview names the pair and the path before anything runs.
func TestPathOverlappingItemsNeverRunAtOnce(t *testing.T) {
	isolate(t)
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "starts.log")
	jobsStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Change `internal/x/shared.go:10-20`. SLOW\n")
	write(t, filepath.Join(dir, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it too. SLOW\n")
	write(t, filepath.Join(dir, "work", "03-third.md"),
		"---\ntitle: Third\ndeps:\n---\n\n1. Also touch internal/x/shared.go.\n")

	plan := dryRun(t, "--jobs", "3")
	for _, want := range []string{"wave 1: 1 2", "wave 2: 3", "3 waits for 1: both cite internal/x/shared.go"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the preview does not say %q:\n%s", want, plan)
		}
	}

	rows := driveAll(t, "--jobs", "3")
	if len(rows) != 3 {
		t.Fatalf("drive reported %d rows, want 3: %+v", len(rows), rows)
	}
	one, two, three := treeName(t, "1"), treeName(t, "2"), treeName(t, "3")
	starts := jobsStarts(t, log)
	if slices.Contains(starts[three], one) || slices.Contains(starts[one], three) {
		t.Errorf("items 1 and 3 cite one path and ran at once: %v", starts)
	}
	if !slices.Contains(starts[two], one) && !slices.Contains(starts[one], two) {
		t.Errorf("items 1 and 2 cite nothing in common and never overlapped, so nothing ran in parallel: %v", starts)
	}
	slots := map[int]bool{}
	for _, r := range rows {
		if r.Slot == 0 {
			t.Errorf("item %s names no slot under --jobs 3", r.ID)
		}
		slots[r.Slot] = true
	}
	if len(slots) < 2 {
		t.Errorf("every item ran in one slot: %+v", rows)
	}
}

// Two items the planned waves split still meet when a slot frees early, so the
// preview names their hold rather than judging only pairs inside one wave.
func TestPreviewNamesAHoldTheWavesSplit(t *testing.T) {
	isolate(t)
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Change `internal/x/shared.go:10-20`.\n")
	write(t, filepath.Join(dir, "work", "02-second.md"),
		"---\ntitle: Second\ndeps:\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "03-third.md"),
		"---\ntitle: Third\ndeps:\n---\n\n1. Also touch internal/x/shared.go.\n")
	write(t, filepath.Join(dir, "work", "04-fourth.md"),
		"---\ntitle: Fourth\ndeps:\n---\n\n1. Do it as well.\n")

	plan := dryRun(t, "--jobs", "2")
	for _, want := range []string{"wave 1: 1 2\n", "wave 2: 3 4\n", "3 waits for 1: both cite internal/x/shared.go"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the preview does not say %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "none is held apart") {
		t.Errorf("the preview says no pair is held while 1 and 3 cite one path:\n%s", plan)
	}
}

// A trunk that fails the verify command alone stops the drive. Under --jobs the items
// already running finish the stage they are in and park drained, and the item
// still waiting for a slot is never launched.
func TestATrunkRedDrainsTheRunningItemsAndDispatchesNoMore(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	log := filepath.Join(t.TempDir(), "starts.log")
	jobsStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1\n")
	for _, n := range []string{"02-second", "03-third", "04-fourth"} {
		write(t, filepath.Join(dir, "work", n+".md"), "---\ntitle: "+n+"\ndeps:\n---\n\n1. Do it. SLOW\n")
	}
	moveTrunkAfter(t, remote, "rev", "*REVIEW-PASSES-1*", "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt")
	// The slow items hold their implement run until the trunk alone has failed,
	// which a detached HEAD tells apart from the caught-up branch, so the drain
	// reaches them mid-stage however long item 1's trunk stage took.
	check, err := exec.LookPath("check")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(check, check+".orig"); err != nil {
		t.Fatal(err)
	}
	red := filepath.Join(t.TempDir(), "trunk-red")
	t.Setenv("DROVER_TEST_SLOW_UNTIL", red)
	stub(t, check, `#!/bin/sh
"$0.orig" "$@"; rc=$?
if [ $rc -ne 0 ] && ! git symbolic-ref -q HEAD >/dev/null; then touch "$DROVER_TEST_SLOW_UNTIL"; fi
exit $rc
`)

	var rows []DriveItem
	out := capture(t, func() { err = execute(t, "drive", "--json", "--jobs", "3") })
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			rows = append(rows, d)
		}
	}
	if !errors.Is(err, errTrunkRed) {
		t.Fatalf("drive ended on %v, want %v", err, errTrunkRed)
	}
	got := map[string]DriveItem{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(rows) != 3 || got["1"].Park != parkTrunkRed {
		t.Fatalf("rows = %+v, want 1 parked trunk_red and two others", rows)
	}
	for _, id := range []string{"2", "3"} {
		if got[id].Park != parkDrained || got[id].Reviews != 0 {
			t.Errorf("item %s is %q with %d reviews (%s), want %q before its review", id, got[id].Park, got[id].Reviews, got[id].Reason, parkDrained)
		}
	}
	if rows[0].ID != "1" {
		t.Errorf("rows arrived as %+v; item 1 finished first and is reported first", rows)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if n := s.ledger.Counts("4").Launches(); n != 0 {
		t.Errorf("item 4 was launched %d times after the trunk went red", n)
	}
	if _, ran := jobsStarts(t, log)[treeName(t, "4")]; ran {
		t.Error("item 4 started a session after the drive stopped dispatching")
	}
}

// An item named after its own dep in one drive waits for that dep under --jobs
// as it does at --jobs 1, rather than starting beside it and skipping as
// blocked. The preview puts the two in separate waves and names the hold.
func TestAnItemWaitsForItsDepInTheSameDrive(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
	log := filepath.Join(t.TempDir(), "starts.log")
	jobsStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1\n---\n\n1. Do it. REVIEW-PASSES-2\n")

	plan := dryRun(t, "1", "2", "--jobs", "2")
	for _, want := range []string{"wave 1: 1\n", "wave 2: 2\n", "2 waits for 1: depends on it", "runs once 1 finishes earlier in this drive"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the preview does not say %q:\n%s", want, plan)
		}
	}

	rows := driveAll(t, "1", "2", "--jobs", "2")
	if len(rows) != 2 {
		t.Fatalf("drive reported %d rows, want 2: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if !r.Finished {
			t.Errorf("item %s is %s/%s (%s), want finished", r.ID, r.Outcome, r.Park, r.Reason)
		}
	}
	if rows[0].ID != "1" {
		t.Errorf("rows arrived as %+v; item 2 cannot finish before the dep it waits for", rows)
	}
	one, two := treeName(t, "1"), treeName(t, "2")
	if slices.Contains(jobsStarts(t, log)[two], one) {
		t.Errorf("item 2 started while item 1, its dep, was mid-session: %v", jobsStarts(t, log))
	}
}

// Three items writing one ledger at once each fold to their own state: the
// rows are whole, carry the right id, and the two that passed finish one after
// the other through the trunk stage while the silent one stays ran.
func TestConcurrentItemsFoldToTheirOwnStates(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
	log := filepath.Join(t.TempDir(), "starts.log")
	jobsStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "03-third.md"), "---\ntitle: Third\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-3\n")

	rows := driveAll(t, "--jobs", "3")
	if len(rows) != 3 {
		t.Fatalf("drive reported %d rows, want 3: %+v", len(rows), rows)
	}
	slots := map[int]bool{}
	for _, r := range rows {
		slots[r.Slot] = true
	}
	if len(slots) != 3 {
		t.Errorf("three items under --jobs 3 ran in slots %v, want 1, 2 and 3 at once", slots)
	}
	// Both passing items reviewed at about the same moment. The trunk stage
	// takes them in dispatch order, so item 3 is gated only after item 1 merged,
	// against a trunk that holds it.
	var three DriveItem
	for _, r := range rows {
		if r.ID == "3" {
			three = r
		}
	}
	git(t, dir, "fetch", "-q", "origin")
	if three.CatchUp == nil || three.CatchUp.Verify != catchUpPassed {
		t.Fatalf("item 3's branch update is %+v: it was gated against a trunk item 1 had not yet moved", three.CatchUp)
	}
	if ok, err := isAncestor(dir, "origin/first", three.CatchUp.To); err != nil || !ok {
		t.Errorf("item 3 updated to %s, which does not hold item 1's branch (err %v)", three.CatchUp.To, err)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ledger.Malformed) != 0 {
		t.Fatalf("concurrent appends tore a row: %v", s.ledger.Malformed)
	}
	for _, c := range []struct {
		id   string
		pass string
		done bool
	}{{"1", passPassed, true}, {"2", passSilent, false}, {"3", passPassed, true}} {
		var stages, passes []string
		for _, r := range s.ledger.Rows {
			if r.ID != c.id {
				continue
			}
			if r.Stage != "" {
				stages = append(stages, r.Stage)
			}
			if r.Pass != "" {
				passes = append(passes, r.Pass)
			}
		}
		if !slices.Equal(stages, []string{ledger.StageImplement, ledger.StageReview}) {
			t.Errorf("item %s has stage rows %v, want one implement and one review", c.id, stages)
		}
		if !slices.Equal(passes, []string{c.pass}) {
			t.Errorf("item %s has pass rows %v, want [%s]", c.id, passes, c.pass)
		}
		if got := s.ledger.State(c.id).Merged; got != c.done {
			t.Errorf("item %s folds to done=%v, want %v", c.id, got, c.done)
		}
	}
	it, err := s.item("2")
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := s.workState(it, s.cfg.LogDir()); state != stateRan {
		t.Errorf("item 2 folds to %q, want %q", state, stateRan)
	}
}

func TestCitedPathsAndTheirOverlap(t *testing.T) {
	body := "Cite `internal/cmd/drive.go:782` and internal/lease/lease.go, the `internal/cmd` tree,\n" +
		"and/or https://github.com/ravinald/drover/blob/main/docs/x.md and ./docs/automation.md."
	got := citedPaths(body)
	want := []string{"internal/cmd/drive.go", "internal/lease/lease.go", "docs/automation.md", "internal/cmd"}
	if !slices.Equal(got, want) {
		t.Fatalf("citedPaths = %q, want %q", got, want)
	}
	for _, c := range []struct {
		a, b []string
		want string
	}{
		{[]string{"internal/cmd"}, []string{"internal/cmd/drive.go"}, "internal/cmd"},
		{[]string{"internal/cmd/drive.go"}, []string{"internal/cmd/drive.go"}, "internal/cmd/drive.go"},
		{[]string{"internal/cmd/drive.go"}, []string{"internal/cmd/drive_test.go"}, ""},
		{[]string{"internal/cmdx/a.go"}, []string{"internal/cmd"}, ""},
	} {
		if got := overlap(c.a, c.b); got != c.want {
			t.Errorf("overlap(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// The drain releases a turn nobody is waiting on, and an item behind one that
// ended without the trunk stage still gets its turn.
func TestTheTrunkStageSkipsItemsThatNeverEnterIt(t *testing.T) {
	q := newDriveSched()
	q.retire(0)
	done := make(chan error, 1)
	go func() { done <- q.enter(1, lease.Hold{ID: "1"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enter(1) = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("item 1 waited for a turn item 0 retired")
	}
	waiting := make(chan error, 1)
	go func() { waiting <- q.enter(2, lease.Hold{ID: "2"}) }()
	q.drain(errTrunkRed)
	q.leave(1)
	if err := <-waiting; !errors.Is(err, errTrunkRed) {
		t.Errorf("an item waiting for its turn when the drive drained got %v, want %v", err, errTrunkRed)
	}
}

// A catch-up whose verify command passed stays on the row, so a later park for another
// reason must not read as the catch-up's failure. The unrunnable rows are the
// control: those parks did come from the catch-up.
func TestAPassedCatchUpDoesNotWordALaterPark(t *testing.T) {
	for _, c := range []struct {
		name, park, gate, want, not string
	}{
		{"merge gate after a passed branch update", "gate_unrunnable", catchUpPassed, "[drive] merge_gate", "updated tree"},
		{"interrupt after a passed branch update", "interrupted", catchUpPassed, "interrupted between rounds", "updated tree"},
		{"the branch update's own gate", "gate_unrunnable", catchUpUnrunnable, "updated tree", "[drive] merge_gate"},
		{"the branch update's own interrupt", "interrupted", catchUpUnrunnable, "updated tree", "between rounds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := DriveItem{ID: "3", Outcome: outcomeParked, Park: c.park, Reason: "why", Tree: "/tmp/3",
				CatchUp: &CatchUp{From: "a", To: "b", Verify: c.gate}}
			f := driveFollowUp(d, "work/03-x.md", false, 4, polOperator)
			line := f.Outcome + " / " + f.Next
			if !strings.Contains(line, c.want) || strings.Contains(line, c.not) {
				t.Errorf("follow-up %q, want %q and not %q", line, c.want, c.not)
			}
		})
	}
}

// openSibling gives item 2 what an earlier drive leaves an item whose pull
// request is still open: its branch, one commit writing file, pushed to origin,
// and the ledger row naming pull request #8 on it. The forge reports #8 open.
func openSibling(t *testing.T, dir, file, body string) string {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("2")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, file), body+"\n")
	git(t, dir, "add", file)
	git(t, dir, "commit", "-q", "-m", "item 2's work")
	git(t, dir, "push", "-q", "origin", branch)
	git(t, dir, "switch", "-q", "main")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Stamp(ledger.Row{ID: "2", Branch: branch, PR: 8, Date: ledger.Today()})); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_OPEN", branch)
	return branch
}

// mergeWithoutAuto is the forge that merges on the gate's word, with the check
// wait shortened, so the merge happens inside the drive.
func mergeWithoutAuto(t *testing.T) {
	t.Helper()
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	poll := checksPoll
	checksPoll = time.Millisecond
	t.Cleanup(func() { checksPoll = poll; driveEvents = false })
}

// siblingCatchUp is the update_branch stage event for item 2, which drive never
// dispatched.
func siblingCatchUp(t *testing.T, evs []DriveEvent) DriveEvent {
	t.Helper()
	for _, e := range evs {
		if e.Event == evStage && e.Stage == stageCatchUp && e.ID == "2" {
			return e
		}
	}
	t.Fatalf("no update_branch stage event for item 2 in %+v", evs)
	return DriveEvent{}
}

// Item 1 merges, and item 2's pull request, open and waiting, is now behind
// the trunk. drive merges the trunk into item 2's branch in its worktree, runs
// item 2's verify command there, and pushes it, leaving item 2 where it was,
// so no supervisor has to loop git merge-tree over the open pull requests after
// each sibling merges.
func TestAMergeCatchesUpTheOtherOpenPullRequests(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	mergeWithoutAuto(t)
	sibling := openSibling(t, dir, "two.txt", "two")
	before := git(t, dir, "rev-parse", "origin/"+sibling)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	git(t, dir, "fetch", "-q", "origin")
	trunk := git(t, dir, "rev-parse", "origin/main")
	pushed := git(t, dir, "rev-parse", "origin/"+sibling)
	if pushed == before {
		t.Fatalf("origin's %s did not move: the branch update was not pushed\n%s", sibling, stderr)
	}
	if ok, err := isAncestor(dir, trunk, pushed); err != nil || !ok {
		t.Errorf("origin's %s does not hold the trunk item 1 merged into (err %v)", sibling, err)
	}
	if ok, err := isAncestor(dir, before, pushed); err != nil || !ok {
		t.Errorf("the branch update replaced item 2's commit rather than merging onto it (err %v)", err)
	}
	var gated bool
	for _, r := range checkLog(t, dir) {
		if inItemTree(t, r.dir, "2") && r.head == pushed {
			gated = true
		}
	}
	if !gated {
		t.Errorf("item 2's verify command never ran on the merged tree in its worktree: %+v", checkLog(t, dir))
	}
	if e := siblingCatchUp(t, evs); e.Result != catchUpPassed || e.PR != 8 || e.Branch != sibling {
		t.Errorf("item 2's update_branch event is %+v, want result %q on #8 and %s", e, catchUpPassed, sibling)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ledger.State("1").Merged || s.ledger.State("2").Merged {
		t.Error("item 1 should be finished and item 2 left open")
	}
}

// A sibling that conflicts with the trunk is not pushed, and the hand-off names
// the branch, the files and the worktree, at resolve_conflict's default of
// supervisor.
func TestAConflictingSiblingIsReportedAndNotPushed(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	mergeWithoutAuto(t)
	// Item 1's implement run writes implemented.txt too, with other bytes.
	sibling := openSibling(t, dir, "implemented.txt", "theirs")
	before := git(t, dir, "rev-parse", "origin/"+sibling)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	git(t, dir, "fetch", "-q", "origin")
	if after := git(t, dir, "rev-parse", "origin/"+sibling); after != before {
		t.Errorf("origin's %s moved from %s to %s over a conflict", sibling, before, after)
	}
	if e := siblingCatchUp(t, evs); e.Result != catchUpConflict {
		t.Errorf("item 2's update_branch event is %+v, want result %q", e, catchUpConflict)
	}
	var hand *DriveEvent
	for i, e := range evs {
		if e.Event == evNeedsSupervisor && e.ID == "2" {
			hand = &evs[i]
		}
	}
	if hand == nil {
		t.Fatalf("no needs_supervisor event for item 2 in %+v", evs)
	}
	if hand.Action != "resolve_conflict" || hand.Branch != sibling || !slices.Equal(hand.Paths, []string{"implemented.txt"}) {
		t.Errorf("needs_supervisor is %+v, want resolve_conflict on %s naming implemented.txt", *hand, sibling)
	}
	if hand.Tree == "" {
		t.Fatal("needs_supervisor names no worktree to resolve in")
	}
	if on := git(t, hand.Tree, "symbolic-ref", "--short", "HEAD"); on != sibling {
		t.Errorf("the worktree it names holds %s, want %s", on, sibling)
	}
	if st := git(t, hand.Tree, "status", "--porcelain"); st != "" {
		t.Errorf("the worktree was left mid-merge:\n%s", st)
	}
}

// A gate that exits 4 says the head is behind the trunk, which a catch-up
// fixes: drive catches up and asks once more. The trunk moves while the first
// verify runs, so the retry's catch-up has something to merge. A second 4 parks
// gate_refused and sends nothing back.
func TestAGateSayingBehindIsRetriedOnceAfterACatchUp(t *testing.T) {
	for _, c := range []struct {
		name   string
		behind string
		park   string
	}{
		{"one 4 then green", "1", ""},
		{"two 4s park", "2", parkGateRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			t.Setenv("DROVER_TEST_GATE_BEHIND", c.behind)
			moveTrunkAfter(t, remote, "merge-gate", "*", "elsewhere.txt", "theirs")

			out := capture(t, func() {
				if err := execute(t, "drive", "--json", "--events", "1"); err != nil {
					t.Fatalf("drive: %v", err)
				}
			})
			var got DriveItem
			var steps []string
			for _, line := range strings.Split(out, "\n") {
				var e DriveEvent
				if json.Unmarshal([]byte(line), &e) == nil && e.Event != "" {
					// A result marks the catch-up at dispatch, before the session.
					if e.Event == evStage && e.ID == "1" && e.Result == "" && (e.Stage == stageGate || e.Stage == stageCatchUp) {
						steps = append(steps, e.Stage)
					}
					continue
				}
				_ = json.Unmarshal([]byte(line), &got)
			}
			if want := []string{stageCatchUp, stageGate, stageCatchUp, stageGate}; !slices.Equal(steps[:min(4, len(steps))], want) {
				t.Errorf("the trunk steps ran %q, want %q first", steps, want)
			}
			if n := len(gateLog(t, dir)); n != 2 {
				t.Errorf("[drive] merge_gate ran %d times, want 2: the first and one retry", n)
			}
			git(t, dir, "fetch", "-q", "origin")
			moved := git(t, dir, "rev-parse", "origin/main")
			if got.CatchUp == nil || got.CatchUp.To != moved {
				t.Errorf("update_branch = %+v, want the retry's merge of %s", got.CatchUp, moved)
			}
			if got.Park != c.park {
				t.Fatalf("item 1 parked %q (%s), want %q", got.Park, got.Reason, c.park)
			}
			if c.park == "" {
				if !got.Merged {
					t.Errorf("item 1 is %q (%s), want the merge requested after the retry", got.Outcome, got.Reason)
				}
				return
			}
			if !strings.Contains(got.Reason, "exit 4") || got.Rounds != 0 {
				t.Errorf("item 1 rounds=%d (%s), want no round and a reason naming exit 4", got.Rounds, got.Reason)
			}
			if n := len(prCreates(t, ghLog)); n != 0 {
				t.Errorf("%d pull requests opened over a merge gate that never passed", n)
			}
		})
	}
}

// gateDrive runs drive over item 1 and splits what it printed into its events
// and the item's account.
func gateDrive(t *testing.T) (got DriveItem, evs []DriveEvent) {
	t.Helper()
	out := capture(t, func() {
		if err := execute(t, "drive", "--json", "--events", "1"); err != nil {
			t.Fatalf("drive: %v", err)
		}
	})
	for _, line := range strings.Split(out, "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Event != "" {
			evs = append(evs, e)
			continue
		}
		_ = json.Unmarshal([]byte(line), &got)
	}
	return got, evs
}

// changesRows counts the changes rows drive wrote for id, past the one
// mergeProject seeds.
func changesRows(t *testing.T, dir, id string) int {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range l.Rows {
		if r.ID == id && r.Changes != "" && r.Changes != "earlier" {
			n++
		}
	}
	return n
}

// A gate that refuses a moved trunk with exit 1 rather than 4 is reporting the
// trunk, not the diff, and drive asks git before believing it: a head the trunk
// moved past is caught up and gated again, a caught-up head is still sent back,
// and a policy short of auto parks the reread refusal as it would an exit 4.
func TestAGateExitingOneOnAHeadBehindTheTrunkIsReadAsBehind(t *testing.T) {
	for _, c := range []struct {
		name     string
		moved    bool
		policy   string
		park     string
		rounds   int
		changes  int
		gateRuns int
	}{
		{"behind is caught up and gated again", true, "", "", 0, 0, 2},
		{"caught up is still sent back", false, "", parkGateRefused, 1, 1, 1},
		{"operator parks with the hand-off", true, "operator", parkGateRefused, 0, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			if c.policy != "" {
				appendConfig(t, dir, "\n[supervise]\nretry_moved_trunk = \""+c.policy+"\"\n")
			}
			t.Setenv("DROVER_TEST_GATE_STALE", "1")
			if c.moved {
				moveTrunkAfter(t, remote, "merge-gate", "*", "elsewhere.txt", "theirs")
			}

			got, evs := gateDrive(t)
			if n := len(gateLog(t, dir)); n != c.gateRuns {
				t.Errorf("[drive] merge_gate ran %d times, want %d", n, c.gateRuns)
			}
			if got.Park != c.park || got.Rounds != c.rounds {
				t.Fatalf("item 1 parked %q with rounds=%d (%s), want %q and %d", got.Park, got.Rounds, got.Reason, c.park, c.rounds)
			}
			if n := changesRows(t, dir, "1"); n != c.changes {
				t.Errorf("%d changes rows for item 1, want %d", n, c.changes)
			}
			const read = "exit 1 on a head behind main, which drover read as behind"
			switch {
			case c.park == "":
				git(t, dir, "fetch", "-q", "origin")
				moved := git(t, dir, "rev-parse", "origin/main")
				if !got.Merged || got.CatchUp == nil || got.CatchUp.To != moved {
					t.Errorf("item 1 is %q with update_branch %+v (%s), want the retry's merge of %s merged",
						got.Outcome, got.CatchUp, got.Reason, moved)
				}
			case c.policy != "":
				if !strings.Contains(got.Reason, read) {
					t.Errorf("reason %q does not say drover read the exit 1 as behind", got.Reason)
				}
				i := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evNeedsOperator && e.ID == "1" })
				if i < 0 || evs[i].Action != "retry_moved_trunk" || !strings.Contains(evs[i].Reason, read) {
					t.Errorf("no needs_operator for retry_moved_trunk naming the reread exit 1 in %+v", evs)
				}
			default:
				if strings.Contains(got.Reason, read) {
					t.Errorf("reason %q reads a caught-up head as behind", got.Reason)
				}
			}
		})
	}
}

// The head's checks can take most of an hour after the catch-up, so the trunk
// is fetched again just before the gate reads the head. A trunk that moved in
// that wait is caught up first, and a gate that checks ancestry is asked once.
func TestTheTrunkIsCaughtUpAgainWhenItMovesBeforeTheGate(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	t.Setenv("DROVER_TEST_GATE_ANCESTRY", "on")
	moveTrunkAfter(t, remote, "gh", "api*check-runs*", "elsewhere.txt", "theirs")

	got, evs := gateDrive(t)
	var steps []string
	for _, e := range evs {
		if e.Event == evStage && e.ID == "1" && e.Result == "" && (e.Stage == stageGate || e.Stage == stageCatchUp) {
			steps = append(steps, e.Stage)
		}
	}
	if want := []string{stageCatchUp, stageCatchUp, stageGate}; !slices.Equal(steps[:min(3, len(steps))], want) {
		t.Errorf("the trunk steps ran %q, want %q first", steps, want)
	}
	if n := len(gateLog(t, dir)); n != 1 {
		t.Errorf("[drive] merge_gate ran %d times, want once, on the head caught up again", n)
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main")
	if !got.Merged || got.CatchUp == nil || got.CatchUp.To != moved {
		t.Errorf("item 1 is %q with update_branch %+v (%s), want %s merged in before the gate", got.Outcome, got.CatchUp, got.Reason, moved)
	}
}

// A catch-up before the gate spends the one retry, and the policy still owns
// what follows: at operator, a gate that then refuses a trunk moved under it,
// by exit 1 on a head behind it or by exit 4, hands off rather than parking as
// an exhausted retry, and nothing is sent back.
func TestAPolicyShortOfAutoHandsOffAfterACatchUpBeforeTheGate(t *testing.T) {
	for _, exit := range []string{"1", "4"} {
		t.Run("exit "+exit, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			appendConfig(t, dir, "\n[supervise]\nretry_moved_trunk = \"operator\"\n")
			moveTrunkAfter(t, remote, "gh", "api*check-runs*", "first.txt", "first")
			gate, err := exec.LookPath("merge-gate")
			if err != nil {
				t.Fatal(err)
			}
			stub(t, gate, `#!/bin/sh
echo "$1" >> "$DROVER_ROOT/merge-gate.log"
tmp=$(mktemp -d)
git clone -q "$DROVER_TEST_REMOTE" "$tmp/c"
echo second > "$tmp/c/second.txt"
git -C "$tmp/c" add -A
git -C "$tmp/c" -c user.name=o -c user.email=o@example.invalid -c commit.gpgsign=false commit -q -m second
git -C "$tmp/c" push -q origin HEAD:main
rm -rf "$tmp"
echo 'merge-gate: main moved; refusing' >&2
exit `+exit+`
`)

			got, evs := gateDrive(t)
			i := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evNeedsOperator && e.ID == "1" })
			if i < 0 || evs[i].Action != "retry_moved_trunk" {
				t.Fatalf("no needs_operator for retry_moved_trunk: park=%s rounds=%d reason=%s", got.Park, got.Rounds, got.Reason)
			}
			if got.Park != parkGateRefused || got.Rounds != 0 {
				t.Errorf("item 1 parked %q with rounds=%d (%s), want %q and 0", got.Park, got.Rounds, got.Reason, parkGateRefused)
			}
			if n := changesRows(t, dir, "1"); n != 0 {
				t.Errorf("%d changes rows for item 1, want none", n)
			}
			if n := len(gateLog(t, dir)); n != 1 {
				t.Errorf("[drive] merge_gate ran %d times, want once", n)
			}
		})
	}
}

// The retry's catch-up waits on the new head's checks too, and the trunk can
// move in that wait. With the one catch-up spent, drive parks on the fetch it
// makes before the gate rather than hand the gate a head it already knows is
// behind.
func TestATrunkMovingDuringTheRetrysChecksParksBeforeTheGate(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	t.Setenv("DROVER_TEST_GATE_BEHIND", "1")
	t.Setenv("DROVER_TEST_GATE_ANCESTRY", "on")
	moveTrunkAfter(t, remote, "gh", "api*check-runs*", "second.txt", "second")
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(gh) // #nosec G304 -- the wrapper moveTrunkAfter just stubbed
	if err != nil {
		t.Fatal(err)
	}
	// The first check read is the head the gate reads first; the second is the
	// head the retry's catch-up left.
	moved := strings.Replace(string(script), `if [ ! -e "$DROVER_TEST_MOVE_MARKER" ]; then`,
		`if [ "$(cat "$DROVER_TEST_READS")" = 2 ] && [ ! -e "$DROVER_TEST_MOVE_MARKER" ]; then`, 1)
	if moved == string(script) {
		t.Fatal("the trunk-moving wrapper no longer carries the line this test rewrites")
	}
	// stub, never a write through the link: the wrapper's file is shared with
	// every other test that moves the trunk.
	stub(t, gh, moved)

	got, _ := gateDrive(t)
	if n := len(gateLog(t, dir)); n != 1 {
		t.Errorf("[drive] merge_gate ran %d times, want once: the retry's head was already behind", n)
	}
	if got.Park != parkGateRefused || got.Rounds != 0 {
		t.Fatalf("item 1 parked %q with rounds=%d (%s), want %q and 0", got.Park, got.Rounds, got.Reason, parkGateRefused)
	}
	if want := behindAgain(got.Branch, "main", "exit 4", ""); !strings.HasPrefix(got.Reason, strings.TrimSpace(want)) {
		t.Errorf("reason %q, want behindAgain's", got.Reason)
	}
	if n := changesRows(t, dir, "1"); n != 0 {
		t.Errorf("%d changes rows for item 1, want none", n)
	}
}

// A merge made outside drive moves the trunk just the same, and --update-branches
// brings every open drive pull request level with it, launching nothing.
func TestCatchUpByHandFollowsAMergeMadeElsewhere(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	sibling := openSibling(t, dir, "two.txt", "two")
	tmp := t.TempDir()
	git(t, tmp, "clone", "-q", remote, "c")
	write(t, filepath.Join(tmp, "c", "elsewhere.txt"), "a hand merge\n")
	git(t, filepath.Join(tmp, "c"), "add", "-A")
	git(t, filepath.Join(tmp, "c"), "commit", "-q", "-m", "merged by hand")
	git(t, filepath.Join(tmp, "c"), "push", "-q", "origin", "HEAD:main")

	if err := execute(t, "drive", "--update-branches"); err != nil {
		t.Fatalf("drive --update-branches: %v", err)
	}
	git(t, dir, "fetch", "-q", "origin")
	trunk := git(t, dir, "rev-parse", "origin/main")
	if ok, err := isAncestor(dir, trunk, "origin/"+sibling); err != nil || !ok {
		t.Errorf("origin's %s does not hold the hand-merged trunk (err %v)", sibling, err)
	}
	if n := len(gateLog(t, dir)); n != 0 {
		t.Errorf("[drive] merge_gate ran %d times; a branch update asks the item's verify command alone", n)
	}
	if runs := checkLog(t, dir); len(runs) != 1 || !inItemTree(t, runs[0].dir, "2") {
		t.Errorf("the item's verify command ran as %+v, want once in item 2's worktree", runs)
	}
	if err := execute(t, "drive", "--update-branches", "1"); Code(err) != 3 {
		t.Errorf("--update-branches with an id exited %d (%v), want 3", Code(err), err)
	}
}

// --update-branches launches no item, so a deps line deps_lint would refuse in an
// item it never reads does not stand between a hand merge and its catch-up.
func TestCatchUpByHandIgnoresADepsLineNoItemLaunches(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	sibling := openSibling(t, dir, "two.txt", "two")
	write(t, filepath.Join("work", "03-third.md"), "---\ntitle: Third\ndeps: 1, 2\n---\n\n1. Do it after both.\n")
	tmp := t.TempDir()
	git(t, tmp, "clone", "-q", remote, "c")
	write(t, filepath.Join(tmp, "c", "elsewhere.txt"), "a hand merge\n")
	git(t, filepath.Join(tmp, "c"), "add", "-A")
	git(t, filepath.Join(tmp, "c"), "commit", "-q", "-m", "merged by hand")
	git(t, filepath.Join(tmp, "c"), "push", "-q", "origin", "HEAD:main")

	if err := execute(t, "drive", "--update-branches"); err != nil {
		t.Fatalf("drive --update-branches refused over a deps line in an item it launches none of: %v", err)
	}
	git(t, dir, "fetch", "-q", "origin")
	trunk := git(t, dir, "rev-parse", "origin/main")
	if ok, err := isAncestor(dir, trunk, "origin/"+sibling); err != nil || !ok {
		t.Errorf("origin's %s does not hold the hand-merged trunk (err %v)", sibling, err)
	}
}

// enqueueFixture is mergeProject with a third item blocked on a fourth that no
// drive here queues, so --enqueue has a not-ready id to refuse.
func enqueueFixture(t *testing.T) {
	t.Helper()
	mergeProject(t)
	write(t, filepath.Join("work", "03-third.md"), "---\ntitle: Third\ndeps: 4\n---\n\n1. Do it after 4.\n")
	write(t, filepath.Join("work", "04-fourth.md"), "---\ntitle: Fourth\ndeps:\n---\n\n1. Do it first.\n")
}

func eventIndex(evs []DriveEvent, event, id string) int {
	return slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == event && e.ID == id })
}

// An id appended to a running drive is dispatched after the item ahead of it,
// announced with a queued event, and refused where it is running already or
// not ready, while the rest of the list is queued.
func TestAnEnqueueToARunningDriveIsDispatchedAfterWhatIsAhead(t *testing.T) {
	enqueueFixture(t)
	gate := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", gate)
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })

	type result struct {
		evs []DriveEvent
		err error
	}
	ran := make(chan result, 1)
	go func() {
		evs, _, err := driveEventsOf(t, "1")
		ran <- result{evs, err}
	}()

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if l, busy, _ := lease.Read(path); busy && slices.Contains(l.Running, "1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the drive never reported item 1 running")
		}
	}

	res, err := s.enqueue([]string{"2", "3", "1"}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if res.free || len(res.queued) != 1 || res.queued[0].id != "2" || res.queued[0].pos != 1 {
		t.Errorf("enqueue to a running drive queued %+v (free=%v), want 2 at position 1", res.queued, res.free)
	}
	refused := strings.Join(res.refused, "\n")
	for _, want := range []string{"3: not ready: blocked on 4", "1: already running"} {
		if !strings.Contains(refused, want) {
			t.Errorf("refusals do not say %q:\n%s", want, refused)
		}
	}
	if Code(res.refusal()) != ExitRefused {
		t.Errorf("an enqueue that refused an id exits %d, want %d", Code(res.refusal()), ExitRefused)
	}

	write(t, gate, "")
	r := <-ran
	if r.err != nil {
		t.Fatalf("drive: %v", r.err)
	}
	one, two := eventIndex(r.evs, evDispatched, "1"), eventIndex(r.evs, evDispatched, "2")
	if one < 0 || two < 0 || two < one {
		t.Fatalf("dispatched 1 at %d and 2 at %d, want 2 after 1: %+v", one, two, r.evs)
	}
	q := eventIndex(r.evs, evQueued, "2")
	if q < 0 || q > two || r.evs[q].Action != "enqueue" || r.evs[q].Position != 1 {
		t.Errorf("no queued event for 2 at position 1 before its dispatch: %+v", r.evs)
	}
	if eventIndex(r.evs, evDispatched, "3") >= 0 {
		t.Error("the refused id 3 was dispatched")
	}
}

// With no drive running, --enqueue is drive over the ids that are ready, and
// still exits 3 naming the one it refused.
func TestAnEnqueueWithNoDriveRunningStartsOne(t *testing.T) {
	enqueueFixture(t)
	evs, _, err := driveEventsOf(t, "--enqueue", "3", "1")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "3: not ready") {
		t.Errorf("--enqueue 3 1 exited %d (%v), want %d naming 3", Code(err), err, ExitRefused)
	}
	if eventIndex(evs, evDispatched, "1") < 0 {
		t.Errorf("no drive ran item 1: %+v", evs)
	}
	if eventIndex(evs, evDispatched, "3") >= 0 {
		t.Error("the refused id 3 was dispatched")
	}
	if last := evs[len(evs)-1]; last.Event != evExit || last.Code == nil || *last.Code != ExitRefused {
		t.Errorf("the stream ends on %+v, want exit with code %d", last, ExitRefused)
	}
}

// A drive holds its lease through reconciliation, before it has read or
// published a queue, so an --enqueue in that window is told its id is queued.
// A drive with nothing ready runs it rather than finding its queue empty and
// dropping the id on the way out, and one whose own queue already holds the id
// runs it once.
func TestAnEnqueueWhileADriveReconcilesIsDispatchedOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		ready bool
	}{{"nothing else ready", false}, {"already in the drive's queue", true}} {
		t.Run(c.name, func(t *testing.T) {
			enqueueFixture(t)
			first := filepath.Join("work", "01-first.md")
			body, err := os.ReadFile(first) // #nosec G304 -- the fixture's own item
			if err != nil {
				t.Fatal(err)
			}
			gone := []string{"02-second.md", "03-third.md", "04-fourth.md"}
			if !c.ready {
				gone = append(gone, "01-first.md")
			}
			for _, f := range gone {
				if err := os.Remove(filepath.Join("work", f)); err != nil {
					t.Fatal(err)
				}
			}
			reached, release := make(chan struct{}), make(chan struct{})
			driveHoldBeforeQueue = func() { close(reached); <-release }
			t.Cleanup(func() { driveHoldBeforeQueue = nil })
			driveLabel = "window"
			t.Cleanup(func() { driveLabel = "" })

			type result struct {
				evs    []DriveEvent
				stderr string
				err    error
			}
			ran := make(chan result, 1)
			go func() {
				evs, stderr, err := driveEventsOf(t)
				ran <- result{evs, stderr, err}
			}()
			select {
			case <-reached:
			case r := <-ran:
				t.Fatalf("the drive exited before reading its queue: %v\n%s", r.err, r.stderr)
			case <-time.After(30 * time.Second):
				t.Fatal("the drive never reached its queue")
			}

			write(t, first, string(body))
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			// The id is the lease's from the moment the drive holds it, before
			// the queue is read.
			path, err := s.leasePath()
			if err != nil {
				t.Fatal(err)
			}
			if l, _, err := lease.Read(path); err != nil || l.Drive != "D1" || l.Label != "window" {
				t.Errorf("before the queue is read the lease names drive %q label %q (%v), want D1 window", l.Drive, l.Label, err)
			}
			res, err := s.enqueue([]string{"1"}, "")
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if res.free || len(res.queued) != 1 || res.queued[0].id != "1" {
				t.Fatalf("enqueue during reconciliation queued %+v (free=%v), want 1 in the running drive", res.queued, res.free)
			}
			close(release)

			r := <-ran
			if r.err != nil {
				t.Fatalf("drive: %v\n%s", r.err, r.stderr)
			}
			d := eventIndex(r.evs, evDispatched, "1")
			if d < 0 {
				t.Fatalf("the drive never dispatched 1: %+v\n%s", r.evs, r.stderr)
			}
			if !c.ready {
				if q := eventIndex(r.evs, evQueued, "1"); q < 0 || q > d {
					t.Errorf("no queued event for 1 before its dispatch: %+v", r.evs)
				}
			}
			if n := len(slices.DeleteFunc(slices.Clone(r.evs), func(e DriveEvent) bool {
				return e.Event != evDispatched || e.ID != "1"
			})); n != 1 {
				t.Errorf("dispatched 1 %d times, want once: %+v", n, r.evs)
			}
			if strings.Contains(r.stderr, "will not run in this drive") {
				t.Errorf("the drive reported dropping an appended id:\n%s", r.stderr)
			}
			if err := s.reload(); err != nil {
				t.Fatal(err)
			}
			ds := s.ledger.Drives()
			if len(ds) != 1 {
				t.Fatalf("drives = %+v, want one", ds)
			}
			for _, e := range r.evs {
				if e.Drive != ds[0].ID {
					t.Errorf("%s event for %q carries drive %q, want %q", e.Event, e.ID, e.Drive, ds[0].ID)
				}
			}
		})
	}
}

// A supervisor runs --enqueue only where [supervise] enqueue lets it, and the
// operator always may.
func TestAnEnqueueBySupervisorAtOperatorPolicyIsRefused(t *testing.T) {
	enqueueFixture(t)
	path := filepath.Join(".", config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+"\n[supervise]\nenqueue = \"operator\"\n")
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	err = refuse(t, "drive", "--enqueue", "1", "--json")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "enqueue") {
		t.Errorf("--enqueue at enqueue = operator exited %d (%v), want %d naming enqueue", Code(err), err, ExitRefused)
	}
	if l, busy, _ := lease.Read(mustLeasePath(t)); busy {
		t.Errorf("a refused --enqueue left a drive holding the checkout: %+v", l)
	}

	t.Setenv(runner.EnvActor, "")
	if _, _, err := driveEventsOf(t, "--enqueue", "1"); err != nil {
		t.Errorf("the operator's --enqueue at enqueue = operator: %v", err)
	}
}

func mustLeasePath(t *testing.T) string {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// A label names a drive as it starts. --pause starts none, so --label beside it
// is refused before the pause is written, rather than accepted and dropped.
func TestALabelBesidePauseIsRefusedAndPausesNothing(t *testing.T) {
	enqueueFixture(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	d := startStubDrive(t, s.cfg.Root, "D1", "1")
	_, err = driveOut(t, "--pause", "--label", "x")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "--label names a drive only when it starts") {
		t.Errorf("--pause --label exited %d (%v), want %d naming --label", Code(err), err, ExitRefused)
	}
	time.Sleep(50 * time.Millisecond)
	if l := d.lease(t); l.Paused || l.Control != "" {
		t.Errorf("--pause --label reached the drive: paused=%v control=%q", l.Paused, l.Control)
	}
}

// A flag a refused call parsed is not set for the next call: cobra never
// resets one, so a leaked --pause refuses every later drive.
func TestARefusedPauseLeavesNoFlagForTheNextCall(t *testing.T) {
	enqueueFixture(t)
	if _, err := driveOut(t, "--pause", "1"); Code(err) != ExitRefused {
		t.Fatalf("--pause 1 exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	_ = capture(t, func() { _ = execute(t, "version") })
	if drivePause || driveCmd.Flags().Lookup("pause").Changed {
		t.Errorf("after the next call drivePause=%v, Changed=%v; want both false", drivePause, driveCmd.Flags().Lookup("pause").Changed)
	}
}

// An --enqueue that joins a running drive cannot rename it, so --label refuses
// the whole list and queues none of it.
func TestAnEnqueueWithALabelIsRefusedByARunningDrive(t *testing.T) {
	enqueueFixture(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	d := startStubDrive(t, s.cfg.Root, "D1", "4")
	_, err = driveOut(t, "--enqueue", "1", "--label", "x")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "--label names a drive only when it starts") {
		t.Errorf("--enqueue 1 --label against a running drive exited %d (%v), want %d naming --label", Code(err), err, ExitRefused)
	}
	if l := d.lease(t); len(l.Queue) != 0 || l.Label != "" {
		t.Errorf("the running drive's lease took queue %v label %q, want neither", l.Queue, l.Label)
	}
}

// With no drive running, --enqueue --label starts one, and the label is the
// lease's from the moment that drive holds it.
func TestAnEnqueueWithALabelAndNoDriveStartsALabeledDrive(t *testing.T) {
	enqueueFixture(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	var held lease.Lease
	driveHoldBeforeQueue = func() { held, _, _ = lease.Read(path) }
	t.Cleanup(func() { driveHoldBeforeQueue = nil })
	evs, stderr, err := driveEventsOf(t, "--enqueue", "1", "--label", "night run")
	if err != nil {
		t.Fatalf("--enqueue 1 --label with no drive: %v\n%s", err, stderr)
	}
	if eventIndex(evs, evDispatched, "1") < 0 {
		t.Errorf("no drive ran item 1: %+v", evs)
	}
	if held.Command != "drive" || held.Label != "night run" {
		t.Errorf("the drive's lease names command %q label %q, want drive \"night run\"", held.Command, held.Label)
	}
}

// appendConfig adds text to the fixture's config and commits it to the trunk,
// pushed, so no item branch or worktree reads a config the trunk lacks.
func appendConfig(t *testing.T, dir, text string) {
	t.Helper()
	path := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+text)
	git(t, dir, "commit", "-q", "-am", "config")
	git(t, dir, "push", "-q", "origin", "main")
}

// itemBranch is the branch drive renders for id.
func itemBranch(t *testing.T, id string) (backlog.Item, string) {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	return it, branch
}

// staleBranch is what an earlier drive leaves item 1: its branch, cut from the
// trunk as it stands now, one commit writing file, pushed.
func staleBranch(t *testing.T, dir, file, body string) string {
	t.Helper()
	_, branch := itemBranch(t, "1")
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, file), body+"\n")
	git(t, dir, "add", file)
	git(t, dir, "commit", "-q", "-m", "item 1's earlier work")
	git(t, dir, "push", "-q", "origin", branch)
	git(t, dir, "switch", "-q", "main")
	return branch
}

// mergedSibling is item 2 done and merged since: its branch writes shared.txt, the
// ledger records its head and its completion, and origin's main merges it
// keeping its commit, as a merge commit does.
func mergedSibling(t *testing.T, dir string) {
	t.Helper()
	_, branch := itemBranch(t, "2")
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, "shared.txt"), "theirs\n")
	git(t, dir, "add", "shared.txt")
	git(t, dir, "commit", "-q", "-m", "item 2's work")
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "--no-edit", branch)
	git(t, dir, "push", "-q", "origin", "main")
	yes := true
	for _, r := range []ledger.Row{
		{ID: "2", Branch: branch, Head: head, Date: ledger.Today()},
		{ID: "2", Merged: &yes, MergedRunner: "test", Date: ledger.Today()},
	} {
		if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Stamp(r)); err != nil {
			t.Fatal(err)
		}
	}
}

// pushTrunk lands a commit writing file on origin's main from another clone.
func pushTrunk(t *testing.T, remote, file, body string) {
	t.Helper()
	c := filepath.Join(t.TempDir(), "c")
	git(t, filepath.Dir(c), "clone", "-q", remote, c)
	write(t, filepath.Join(c, file), body+"\n")
	git(t, c, "add", "-A")
	git(t, c, "-c", "user.name=o", "-c", "user.email=o@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "elsewhere")
	git(t, c, "push", "-q", "origin", "HEAD:main")
}

// resolvingImpl wraps the implement stub with a session that, finding a merge
// in progress, writes its prompt to conflict.log, resolves every conflicted
// file to "both" and commits the merge before doing its own work. Every start
// also records in start.log whether the file DROVER_TEST_START_HAS names was in
// the tree.
func resolvingImpl(t *testing.T) {
	t.Helper()
	path, err := exec.LookPath("impl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".orig"); err != nil {
		t.Fatal(err)
	}
	stub(t, path, `#!/bin/sh
if [ -n "${DROVER_TEST_START_HAS:-}" ]; then
  if [ -e "$DROVER_TEST_START_HAS" ]; then echo held; else echo missing; fi >> "$DROVER_ROOT/start.log"
fi
if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  printf '%s\n' "$*" >> "$DROVER_ROOT/conflict.log"
  for f in $(git diff --name-only --diff-filter=U); do echo both > "$f"; git add -- "$f"; done
  git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q --no-edit
fi
exec "$0.orig" "$@"
`)
}

// catchUpAt is the update_branch stage event drive emitted for id at dispatch, the
// one carrying a result.
func catchUpAt(t *testing.T, evs []DriveEvent, id string) DriveEvent {
	t.Helper()
	for _, e := range evs {
		if e.Event == evStage && e.Stage == stageCatchUp && e.ID == id && e.Result != "" {
			return e
		}
	}
	t.Fatalf("no update_branch event with a result for item %s in %+v", id, evs)
	return DriveEvent{}
}

// implementRows is id's implement stage rows.
func implementRows(t *testing.T, dir, id string) []ledger.Row {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Row
	for _, r := range l.Rows {
		if r.ID == id && r.Stage == ledger.StageImplement {
			out = append(out, r)
		}
	}
	return out
}

// handOffFor is the needs_supervisor or needs_operator event for id.
func handOffFor(t *testing.T, evs []DriveEvent, event, id string) DriveEvent {
	t.Helper()
	for _, e := range evs {
		if e.Event == event && e.ID == id {
			return e
		}
	}
	t.Fatalf("no %s event for item %s in %+v", event, id, evs)
	return DriveEvent{}
}

// Before its implement session, an item's branch takes the trunk: a branch
// behind a clean trunk gets the merge and the session starts on it, and one
// already holding the trunk merges nothing.
func TestTheTrunkIsMergedBeforeTheImplementSession(t *testing.T) {
	for _, c := range []struct {
		name, result string
		behind       bool
	}{
		{"behind a clean trunk", catchUpMerged, true},
		{"already holding the trunk", catchUpLevel, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			resolvingImpl(t)
			t.Setenv("DROVER_TEST_START_HAS", "other.txt")
			if c.behind {
				staleBranch(t, dir, "own.txt", "own")
				pushTrunk(t, remote, "other.txt", "theirs")
			}

			evs, stderr, err := driveEventsOf(t, "1")
			if err != nil {
				t.Fatalf("drive: %v\n%s", err, stderr)
			}
			e := catchUpAt(t, evs, "1")
			if e.Result != c.result {
				t.Errorf("the update_branch at dispatch is %+v, want result %q", e, c.result)
			}
			at := slices.IndexFunc(evs, func(x DriveEvent) bool { return x.Event == evStage && x.Stage == stageCatchUp && x.Result != "" })
			impl := slices.IndexFunc(evs, func(x DriveEvent) bool { return x.Event == evStage && x.Stage == ledger.StageImplement })
			if impl < 0 || at > impl {
				t.Errorf("the update_branch at dispatch (%d) did not come before the implement stage (%d)", at, impl)
			}
			if c.behind {
				b, err := os.ReadFile(filepath.Join(dir, "start.log")) // #nosec G304 -- the fixture's own log
				if err != nil || strings.TrimSpace(string(b)) != "held" {
					t.Errorf("the implement session started without the trunk's other.txt: %q (%v)", b, err)
				}
			}
			if rows := implementRows(t, dir, "1"); len(rows) == 0 || rows[0].Conflict {
				t.Errorf("implement rows %+v, want one not marked conflict", rows)
			}
		})
	}
}

// At resolve_conflict = "auto", a branch that conflicts with the trunk starts
// its implement session with the merge in the worktree, and the brief names the
// item whose merged work touched the file, with its requirements.
func TestAConflictAtDispatchGoesToTheSessionAtAuto(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"auto\"\n")
	staleBranch(t, dir, "shared.txt", "ours")
	mergedSibling(t, dir)
	resolvingImpl(t)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if e := catchUpAt(t, evs, "1"); e.Result != catchUpConflict || !slices.Equal(e.Paths, []string{"shared.txt"}) {
		t.Errorf("the update_branch at dispatch is %+v, want result %q naming shared.txt", e, catchUpConflict)
	}
	b, err := os.ReadFile(filepath.Join(dir, "conflict.log")) // #nosec G304 -- the fixture's own log
	if err != nil {
		t.Fatalf("the implement session never saw a merge in progress: %v\n%s", err, stderr)
	}
	for _, want := range []string{"Your first task is a trunk merge", "- shared.txt", "2: Second", "1. Do it too."} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, b)
		}
	}
	rows := implementRows(t, dir, "1")
	if len(rows) == 0 || !rows[0].Conflict {
		t.Errorf("implement rows %+v, want the first marked conflict", rows)
	}

	// The data itself, on a session holding the same conflict.
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "fetch", "-q", "origin")
	tree := t.TempDir() + "/tree"
	git(t, dir, "worktree", "add", "-q", "--detach", tree, "origin/main~1")
	s.tree = tree
	c := s.conflictBrief("1", "origin/main", "origin/main~1", git(t, dir, "rev-parse", "origin/main"), []string{"shared.txt"})
	if len(c.Paths) != 1 || len(c.Paths[0].Items) != 1 || c.Paths[0].Items[0].ID != "2" ||
		len(c.Paths[0].Items[0].Requirements) != 1 {
		t.Errorf("Conflict = %+v, want shared.txt owned by item 2 with its requirement", c)
	}
}

// One conflict round per item per drive. Item 1's review sends it back while
// the trunk moves onto the same file again, so its next dispatch conflicts a
// second time, and that one parks for a person.
func TestASecondConflictInOneDriveParksForTheOperator(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"auto\"\n")
	staleBranch(t, dir, "shared.txt", "ours")
	mergedSibling(t, dir)
	resolvingImpl(t)
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rev, rev+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_MOVE_REMOTE", remote)
	t.Setenv("DROVER_TEST_MOVE_MARKER", filepath.Join(t.TempDir(), "moved"))
	stub(t, rev, `#!/bin/sh
"$0.orig" "$@"; rc=$?
if [ ! -e "$DROVER_TEST_MOVE_MARKER" ]; then
  touch "$DROVER_TEST_MOVE_MARKER"
  printf '%s\n' '{"id":"1","changes":"send it back","date":"2026-10-19"}' >> "$DROVER_ROOT/ledger.jsonl"
  tmp=$(mktemp -d)
  {
    git clone -q "$DROVER_TEST_MOVE_REMOTE" "$tmp/c" &&
    echo again > "$tmp/c/shared.txt" &&
    git -C "$tmp/c" -c user.name=o -c user.email=o@example.invalid -c commit.gpgsign=false commit -qam again &&
    git -C "$tmp/c" push -q origin HEAD:main
  } >/dev/null 2>&1
fi
exit $rc
`)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if n := len(implementRows(t, dir, "1")); n != 1 {
		t.Errorf("%d implement sessions launched, want the one conflict round\n%s", n, stderr)
	}
	var parked *DriveEvent
	for i, e := range evs {
		if e.Event == evParked && e.ID == "1" {
			parked = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkTrunkConflict {
		t.Fatalf("item 1 parked %+v, want %q\n%s", parked, parkTrunkConflict, stderr)
	}
	h := handOffFor(t, evs, evNeedsOperator, "1")
	if h.Action != "resolve_conflict" || !slices.Equal(h.Paths, []string{"shared.txt"}) || h.Tree == "" ||
		!strings.Contains(h.Reason, "a second time") {
		t.Errorf("needs_operator is %+v, want resolve_conflict naming a second conflict on shared.txt and the worktree", h)
	}
	if st := git(t, h.Tree, "status", "--porcelain"); st != "" {
		t.Errorf("the worktree was left mid-merge:\n%s", st)
	}
}

// At the default of supervisor, a conflict at dispatch launches nothing: the
// merge is aborted, the item parks, and the hand-off names the files and the
// worktree they are to be resolved in.
func TestAConflictAtDispatchParksAtSupervisor(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	branch := staleBranch(t, dir, "shared.txt", "ours")
	mergedSibling(t, dir)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if n := len(implementRows(t, dir, "1")); n != 0 {
		t.Errorf("%d implement sessions launched over a conflict nobody may hand to one", n)
	}
	if e := catchUpAt(t, evs, "1"); e.Result != catchUpParked || !slices.Equal(e.Paths, []string{"shared.txt"}) {
		t.Errorf("the update_branch at dispatch is %+v, want result %q naming shared.txt", e, catchUpParked)
	}
	h := handOffFor(t, evs, evNeedsSupervisor, "1")
	if h.Action != "resolve_conflict" || !slices.Equal(h.Paths, []string{"shared.txt"}) || h.Branch != branch || h.Tree == "" {
		t.Errorf("needs_supervisor is %+v, want resolve_conflict on %s naming shared.txt and the worktree", h, branch)
	}
	if st := git(t, h.Tree, "status", "--porcelain"); st != "" {
		t.Errorf("the worktree was left mid-merge:\n%s", st)
	}
}

// A conflict round counts against max_review_rounds, so an item already at the cap
// parks rather than starting one.
func TestAConflictAtTheReviewRoundLimitParks(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	// The fixture's one changes row is the cap.
	appendConfig(t, dir, "\n[review]\nmax_review_rounds = 1\n\n[supervise]\nresolve_conflict = \"auto\"\n")
	staleBranch(t, dir, "shared.txt", "ours")
	mergedSibling(t, dir)
	resolvingImpl(t)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if n := len(implementRows(t, dir, "1")); n != 0 {
		t.Errorf("%d implement sessions launched at the cap", n)
	}
	h := handOffFor(t, evs, evNeedsOperator, "1")
	if !strings.Contains(h.Reason, "max_review_rounds") || !slices.Equal(h.Paths, []string{"shared.txt"}) {
		t.Errorf("needs_operator is %+v, want a reason naming max_review_rounds and the path", h)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if n := s.ledger.Counts("1").ChangesRequested; n != 1 {
		t.Errorf("item 1 has %d changes rows, want the fixture's 1: no round started", n)
	}
}

// At auto, another open pull request whose catch-up after a merge conflicts is
// left as it was: nothing pushed, no session, since the trunk stage cannot
// launch one. Its next dispatch takes the conflict to its session.
func TestAnOpenPullRequestsConflictWaitsForItsNextDispatch(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	mergeWithoutAuto(t)
	appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"auto\"\n")
	// Item 1's implement run writes implemented.txt too, with other bytes.
	sibling := openSibling(t, dir, "implemented.txt", "theirs")
	before := git(t, dir, "rev-parse", "origin/"+sibling)

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	git(t, dir, "fetch", "-q", "origin")
	if after := git(t, dir, "rev-parse", "origin/"+sibling); after != before {
		t.Errorf("origin's %s moved from %s to %s over a conflict", sibling, before, after)
	}
	if e := siblingCatchUp(t, evs); e.Result != catchUpConflict {
		t.Errorf("item 2's update_branch event is %+v, want result %q", e, catchUpConflict)
	}
	for _, e := range evs {
		if e.ID == "2" && (e.Event == evParked || e.Event == evNeedsSupervisor || e.Event == evNeedsOperator ||
			(e.Event == evStage && e.Stage == ledger.StageImplement)) {
			t.Errorf("item 2 got %+v in the drive that merged item 1", e)
		}
	}
	if n := len(implementRows(t, dir, "2")); n != 0 {
		t.Errorf("%d implement sessions launched for item 2 inside the trunk stage", n)
	}

	resolvingImpl(t)
	evs, stderr, err = driveEventsOf(t, "2")
	if err != nil {
		t.Fatalf("drive 2: %v\n%s", err, stderr)
	}
	if e := catchUpAt(t, evs, "2"); e.Result != catchUpConflict || !slices.Equal(e.Paths, []string{"implemented.txt"}) {
		t.Errorf("item 2's update_branch at dispatch is %+v, want result %q naming implemented.txt", e, catchUpConflict)
	}
	if rows := implementRows(t, dir, "2"); len(rows) == 0 || !rows[0].Conflict {
		t.Errorf("item 2's implement rows %+v, want a conflict round\n%s", rows, stderr)
	}
}

// loggingRev wraps the review stub so each launch appends its brief to
// review.log, and the first lose launches end as a cut stream does.
func loggingRev(t *testing.T, lose int) {
	t.Helper()
	path, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_REV_COUNT", filepath.Join(t.TempDir(), "count"))
	t.Setenv("DROVER_TEST_REV_LOSSES", strconv.Itoa(lose))
	stub(t, path, `#!/bin/sh
n=$(cat "$DROVER_TEST_REV_COUNT" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$DROVER_TEST_REV_COUNT"
printf '%s\n=== end of prompt ===\n' "$*" >> "$DROVER_ROOT/review.log"
if [ $n -le "$DROVER_TEST_REV_LOSSES" ]; then
  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"The stream was interrupted"}'
  exit 1
fi
exec "$0.orig" "$@"
`)
}

// reviewBriefs is every brief the logging review stub was handed, in order.
func reviewBriefs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "review.log")) // #nosec G304 -- the fixture's own log
	if err != nil {
		t.Fatalf("no review launched: %v", err)
	}
	briefs := strings.Split(string(b), "=== end of prompt ===\n")
	return briefs[:len(briefs)-1]
}

// A review of a round that started from a conflict reads the conflict however
// it is reached: a lost review's rerun on the same head, and a re-drive after a
// drive stopped between the implement run and the review, whose new round
// starts on a branch already holding the trunk.
func TestTheReviewOfAConflictRoundReadsTheConflict(t *testing.T) {
	t.Run("a lost review's rerun", func(t *testing.T) {
		isolate(t)
		ghLog := mergeProject(t)
		dir, _ := forge(t, ghLog)
		appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"auto\"\n")
		staleBranch(t, dir, "shared.txt", "ours")
		mergedSibling(t, dir)
		resolvingImpl(t)
		loggingRev(t, 1)

		if _, stderr, err := driveEventsOf(t, "1"); err != nil {
			t.Fatalf("drive: %v\n%s", err, stderr)
		}
		briefs := reviewBriefs(t, dir)
		if len(briefs) != 2 {
			t.Fatalf("%d reviews launched, want the lost one and its rerun", len(briefs))
		}
		for i, b := range briefs {
			if !strings.Contains(b, "This round started from a conflict") || !strings.Contains(b, "- shared.txt") {
				t.Errorf("review %d does not carry the conflict on shared.txt:\n%s", i+1, b)
			}
		}
	})
	t.Run("a re-drive after a drain before the review", func(t *testing.T) {
		isolate(t)
		ghLog := mergeProject(t)
		dir, _ := forge(t, ghLog)
		appendConfig(t, dir, "\n[supervise]\nresolve_conflict = \"auto\"\n")
		branch := staleBranch(t, dir, "shared.txt", "ours")
		mergedSibling(t, dir)
		// What the drained drive left: the conflict round's changes row and
		// implement row, and the session's resolution committed and pushed.
		trunk := git(t, dir, "rev-parse", "main")
		git(t, dir, "switch", "-q", branch)
		if err := exec.Command("git", "-C", dir, "merge", "-q", "--no-edit", "main").Run(); err == nil {
			t.Fatal("the fixture's merge did not conflict")
		}
		write(t, filepath.Join(dir, "shared.txt"), "both\n")
		git(t, dir, "add", "shared.txt")
		git(t, dir, "commit", "-q", "--no-edit")
		git(t, dir, "push", "-q", "origin", branch)
		git(t, dir, "switch", "-q", "main")
		for _, r := range []ledger.Row{
			{ID: "1", Changes: parkTrunkConflict + " " + shortSHA(trunk), Date: ledger.Today()},
			{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Conflict: true, Date: ledger.Today()},
		} {
			if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Stamp(r)); err != nil {
				t.Fatal(err)
			}
		}
		loggingRev(t, 0)

		evs, stderr, err := driveEventsOf(t, "1")
		if err != nil {
			t.Fatalf("drive: %v\n%s", err, stderr)
		}
		if e := catchUpAt(t, evs, "1"); e.Result != catchUpLevel {
			t.Errorf("the update_branch at dispatch is %+v, want %q: the branch already holds the trunk", e, catchUpLevel)
		}
		briefs := reviewBriefs(t, dir)
		if len(briefs) == 0 || !strings.Contains(briefs[0], "This round started from a conflict") ||
			!strings.Contains(briefs[0], "- shared.txt") || !strings.Contains(briefs[0], shortSHA(trunk)) &&
			!strings.Contains(briefs[0], trunk) {
			t.Errorf("the re-drive's review does not carry the conflict on shared.txt at %s:\n%s", shortSHA(trunk), briefs)
		}
	})
}

// A worktree holding a merge somebody is partway through resolving is left as
// it stands: the catch-up at dispatch merges nothing into it and aborts
// nothing, and the item parks on the uncommitted work.
func TestACatchUpAtDispatchLeavesAMergeInProgress(t *testing.T) {
	for _, c := range []struct {
		name     string
		resolved []string
	}{
		{"resolved and staged", []string{"shared.txt", "second.txt"}},
		{"partly resolved", []string{"shared.txt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			ghLog := mergeProject(t)
			dir, remote := forge(t, ghLog)
			it, branch := itemBranch(t, "1")
			git(t, dir, "switch", "-q", "-c", branch)
			write(t, filepath.Join(dir, "shared.txt"), "ours\n")
			write(t, filepath.Join(dir, "second.txt"), "ours\n")
			git(t, dir, "add", "shared.txt", "second.txt")
			git(t, dir, "commit", "-q", "-m", "item 1's earlier work")
			git(t, dir, "push", "-q", "origin", branch)
			git(t, dir, "switch", "-q", "main")
			mergedSibling(t, dir)
			pushTrunk(t, remote, "second.txt", "theirs")
			git(t, dir, "fetch", "-q", "origin")
			first := git(t, dir, "rev-parse", "origin/main")

			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			tree, err := s.treePath(it)
			if err != nil {
				t.Fatal(err)
			}
			git(t, dir, "worktree", "add", "-q", tree, branch)
			if err := exec.Command("git", "-C", tree, "merge", "-q", "--no-edit", first).Run(); err == nil {
				t.Fatal("the fixture's merge did not conflict")
			}
			for _, p := range c.resolved {
				write(t, filepath.Join(tree, p), "both\n")
				git(t, tree, "add", p)
			}
			mergeHead := git(t, tree, "rev-parse", "MERGE_HEAD")

			evs, stderr, err := driveEventsOf(t, "1")
			if err != nil {
				t.Fatalf("drive: %v\n%s", err, stderr)
			}
			if n := len(implementRows(t, dir, "1")); n != 0 {
				t.Errorf("%d implement sessions launched on a merge in progress", n)
			}
			for _, p := range c.resolved {
				if b, err := os.ReadFile(filepath.Join(tree, p)); err != nil || string(b) != "both\n" { // #nosec G304 -- the fixture's own tree
					t.Errorf("%s is %q (%v), want the resolution in progress", p, b, err)
				}
			}
			if got := git(t, tree, "rev-parse", "MERGE_HEAD"); got != mergeHead {
				t.Errorf("MERGE_HEAD is %q, want the merge in progress at %s", got, mergeHead)
			}
			var parked *DriveEvent
			for i, e := range evs {
				if e.Event == evParked && e.ID == "1" {
					parked = &evs[i]
				}
			}
			if parked == nil || parked.Park != parkUncommitted || !strings.Contains(parked.Reason, "MERGE_HEAD") {
				t.Errorf("item 1 parked %+v, want %q naming MERGE_HEAD\n%s", parked, parkUncommitted, stderr)
			}
			if e := catchUpAt(t, evs, "1"); e.Result != catchUpParked {
				t.Errorf("the update_branch at dispatch is %+v, want result %q", e, catchUpParked)
			}
		})
	}
}

// F148's loop: two send-backs worded differently, each a new digest, both
// naming R4. The second parks the item rather than spending a third round, and
// the summary sends the operator to both kept rounds.
func TestADriveParksOnTheSameRequirementSentBackTwiceInOtherWords(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	reviews := t.TempDir()
	t.Setenv("DROVER_TEST_REVIEWS", reviews)
	stub(t, rev, `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false}'
n=$(cat "$DROVER_TEST_REVIEWS/1" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "$DROVER_TEST_REVIEWS/1"
mkdir -p "$DROVER_ROOT/reports"
printf '# 1 changes %s\n\n### R4 is unmet, wording %s\n' $n $n > "$DROVER_ROOT/reports/01-first.changes-$n.md"
printf '{"id":"1","changes":"wording-%s","reqs":[4],"date":"2026-10-26"}\n' $n >> "$DROVER_ROOT/ledger.jsonl"
`)

	var derr error
	stderr := captureErr(t, func() { derr = execute(t, "drive", "1") })
	if n := reviewCount(t, reviews, "1"); n != 2 {
		t.Errorf("%d reviews ran, want 2: the second names R4 again and parks\n%s", n, stderr)
	}
	if n := len(implementRows(t, dir, "1")); n != 2 {
		t.Errorf("%d implement sessions launched, want 2 and no third round\n%s", n, stderr)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ := l.Pass("1"); got != passRepeating {
		t.Errorf("the last pass row reads %q, want %q (drive: %v)\n%s", got, passRepeating, derr, stderr)
	}
	for _, want := range []string{
		"the review sent back R4 unmet twice in a row; the contract may need narrowing, splitting or an override",
		"drover show 1",
		filepath.Join("reports", "01-first.changes-1.md") + " and " + filepath.Join("reports", "01-first.changes-2.md"),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the drive's output lacks %q:\n%s", want, stderr)
		}
	}
}
