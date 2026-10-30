package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// sequenceProject is mergeProject behind a forge, declaring db/{n}_*.up.sql as
// a sequence, with the trunk holding the migrations named and pushed.
func sequenceProject(t *testing.T, trunk ...string) (dir, remote, ghLog string) {
	t.Helper()
	isolate(t)
	ghLog = mergeProject(t)
	dir, remote = forge(t, ghLog)
	cfg := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, string(raw)+"\n[numbered_files]\nmigrations = \"db/{n}_*.up.sql\"\n")
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range trunk {
		write(t, filepath.Join(dir, "db", f), "-- "+f+"\n")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "migrations")
	git(t, dir, "push", "-q", "origin", "main")
	return dir, remote, ghLog
}

func firstBody(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "work", "01-first.md")) // #nosec G304 -- the fixture's own item
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func noForge(t *testing.T, ghLog string) {
	t.Helper()
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr create") || strings.HasPrefix(c, "pr merge") {
			t.Errorf("a branch whose numbers clash reached the forge: %q", c)
		}
	}
}

// The branch adds 000112 and the trunk gains 000113 while it
// runs. git merges the two cleanly, and a database already at 113 would skip
// 112. The catch-up refuses it, names 000114, and the merge is undone rather
// than pushed.
func TestABranchNumberedBelowTheTrunkIsSentBackNamingTheNext(t *testing.T) {
	dir, remote, ghLog := sequenceProject(t, "000111_base.up.sql")
	moveTrunk(t, remote, "db/000113_theirs.up.sql", "-- theirs")
	t.Setenv("DROVER_TEST_ADD", "db/000112_ours.up.sql")

	got := driveAll(t)[0]
	if got.Outcome != outcomeParked || got.Park != parkGateRefused || got.Merged {
		t.Fatalf("item 1 is %q/%q merged=%v (%s), want parked %q", got.Outcome, got.Park, got.Merged, got.Reason, parkGateRefused)
	}
	if got.Rounds != 1 {
		t.Errorf("rounds = %d, want 1: the request for changes counts against max_review_rounds", got.Rounds)
	}
	git(t, dir, "fetch", "-q", "origin")
	moved := git(t, dir, "rev-parse", "origin/main")
	if c := got.CatchUp; c == nil || c.Verify != catchUpSequence || c.To != moved {
		t.Errorf("update_branch = %+v, want to %s with verify %q", c, moved, catchUpSequence)
	}
	for _, ref := range []string{got.Branch, "origin/" + got.Branch} {
		if ok, err := isAncestor(dir, moved, ref); err != nil || ok {
			t.Errorf("%s holds the trunk's %s (err %v): the merge was not undone", ref, moved, err)
		}
	}
	noForge(t, ghLog)
	body := firstBody(t, dir)
	for _, want := range []string{"db/000112_ours.up.sql", "db/000113_theirs.up.sql", "use 000114", "never renumbers"} {
		if !strings.Contains(body, want) {
			t.Errorf("the request for changes does not carry %q:\n%s", want, body)
		}
	}
}

// Two files numbered 000112 merge without a conflict, and the findings sent back name the
// trunk's file it clashes with.
func TestTwoFilesSharingANumberAreSentBack(t *testing.T) {
	dir, remote, ghLog := sequenceProject(t, "000111_base.up.sql")
	moveTrunk(t, remote, "db/000112_theirs.up.sql", "-- theirs")
	t.Setenv("DROVER_TEST_ADD", "db/000112_ours.up.sql")

	got := driveAll(t)[0]
	if got.Park != parkGateRefused || got.CatchUp == nil || got.CatchUp.Verify != catchUpSequence {
		t.Fatalf("item 1 is %q/%q update_branch=%+v (%s), want a sequence request for changes", got.Outcome, got.Park, got.CatchUp, got.Reason)
	}
	noForge(t, ghLog)
	body := firstBody(t, dir)
	for _, want := range []string{"db/000112_ours.up.sql shares its number with db/000112_theirs.up.sql", "use 000113"} {
		if !strings.Contains(body, want) {
			t.Errorf("the request for changes does not carry %q:\n%s", want, body)
		}
	}
}

// The trunk already held 000113 when the branch was cut, so no catch-up merges
// anything, and only the check before the pull request can see the branch's
// 000112 sorts below it.
func TestTheHeadIsComparedWhenNothingWasCaughtUp(t *testing.T) {
	dir, _, ghLog := sequenceProject(t, "000113_theirs.up.sql")
	t.Setenv("DROVER_TEST_ADD", "db/000112_ours.up.sql")

	got := driveAll(t)[0]
	if got.Park != parkGateRefused || got.CatchUp != nil || got.Rounds != 1 {
		t.Fatalf("item 1 is %q/%q rounds=%d update_branch=%+v (%s), want a sequence request for changes with no branch update",
			got.Outcome, got.Park, got.Rounds, got.CatchUp, got.Reason)
	}
	noForge(t, ghLog)
	if body := firstBody(t, dir); !strings.Contains(body, "is at or below the trunk's highest, db/000113_theirs.up.sql; use 000114") {
		t.Errorf("the request for changes does not name the trunk's highest and 000114:\n%s", body)
	}
}

// A branch adding 000114 above a trunk at 000113 is in order, through a
// catch-up and at the head, and merges.
func TestABranchNumberedAboveTheTrunkMerges(t *testing.T) {
	_, remote, _ := sequenceProject(t, "000112_base.up.sql")
	moveTrunk(t, remote, "db/000113_theirs.up.sql", "-- theirs")
	t.Setenv("DROVER_TEST_ADD", "db/000114_ours.up.sql")

	got := driveAll(t)[0]
	if !got.Merged || got.CatchUp == nil || got.CatchUp.Verify != catchUpPassed {
		t.Fatalf("item 1 is %q/%q merged=%v update_branch=%+v (%s), want merged after a passing branch update",
			got.Outcome, got.Park, got.Merged, got.CatchUp, got.Reason)
	}
}

// A clash a hand merge already put on the trunk is a finding, and check exits 2
// on it.
func TestCheckReportsASequenceClashOnTheTrunk(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	cfg := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, string(raw)+"\n[numbered_files]\nmigrations = \"db/{n}_*.up.sql\"\n")
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"000112_a.up.sql", "112_b.up.sql", "000113_c.up.sql"} {
		write(t, filepath.Join(dir, "db", f), "--\n")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "a hand merge")
	s, err = open()
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, f := range s.checkReport().Findings {
		if f.Kind == "SEQUENCE CLASH" {
			hits = append(hits, f.Message)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "db/000112_a.up.sql, db/112_b.up.sql") {
		t.Errorf("SEQUENCE CLASH findings %q, want one naming the two 112s and not 000113", hits)
	}
	err = checkCmd.RunE(checkCmd, nil)
	if _, ok := errors.AsType[Findings](err); !ok {
		t.Errorf("check returned %v, want a findings error (exit 2)", err)
	}
}

// --dry-run names the trunk's highest number and the one the next item uses.
func TestDryRunPrintsEachSequencesHighest(t *testing.T) {
	sequenceProject(t, "000112_a.up.sql", "000113_b.up.sql")
	out := capture(t, func() {
		if err := execute(t, "drive", "--dry-run"); err != nil {
			t.Fatalf("drive --dry-run: %v", err)
		}
	})
	if want := "main holds up to 000113 (db/000113_b.up.sql); the next item uses 000114"; !strings.Contains(out, want) {
		t.Errorf("--dry-run does not print %q:\n%s", want, out)
	}
}

func TestSequenceNumberReadsAWholeDigitRun(t *testing.T) {
	for _, c := range []struct {
		glob, name string
		n          uint64
		ok         bool
	}{
		{"db/{n}_*.up.sql", "db/000112_a.up.sql", 112, true},
		{"db/{n}_*.up.sql", "db/112_a.up.sql", 112, true},
		{"db/{n}_*.up.sql", "db/000112_a.down.sql", 0, false},
		{"db/{n}_*.up.sql", "db/sub/000112_a.up.sql", 0, false},
		{"adr/{n}-*.md", "adr/0007-use-2-things.md", 7, true},
		{"adr/v{n}.md", "adr/v12.md", 12, true},
		{"adr/v{n}.md", "adr/vx.md", 0, false},
	} {
		f, ok := sequenceNumber(c.glob, c.name)
		if ok != c.ok || f.n != c.n {
			t.Errorf("sequenceNumber(%q, %q) = %d, %v; want %d, %v", c.glob, c.name, f.n, ok, c.n, c.ok)
		}
	}
}
