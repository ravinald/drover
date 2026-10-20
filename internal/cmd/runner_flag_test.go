package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// resetSpellings makes one command safe to drive twice. pflag never clears
// Changed and root is a package singleton, so a test that passes --runner
// leaves it "given" for every test after it.
//
// It clears DROVER_ACTOR for the same reason: every command below is one of the
// six that refuse inside a launched session, and a suite whose result depends
// on whether drover started the shell is testing the shell.
func resetSpellings(t *testing.T, cmd *cobra.Command, vars ...*string) {
	t.Helper()
	t.Setenv(runner.EnvActor, "")
	t.Cleanup(func() {
		for _, name := range []string{"runner", "code-by", "reviewer"} {
			if f := cmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		for _, v := range vars {
			*v = ""
		}
	})
}

// finishedFixture is a project whose item 1 sits on a branch already merged into
// main, which is what finish needs before it will write anything.
func finishedFixture(t *testing.T) string {
	t.Helper()
	dir := fixture(t, "").cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, dir, "switch", "-q", "-c", "first")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	recordedHead(t, dir, "1", "first")
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge first", "first")
	stubPATH(t)
	t.Chdir(dir)
	return dir
}

// finish writes four other things beside the row, so its own pass has to run
// end to end rather than trusting mark-merged's.
func TestFinishTakesTheRunnerFlag(t *testing.T) {
	resetSpellings(t, finishCmd, &finishRunner)
	dir := finishedFixture(t)

	if err := execute(t, "finish", "1", "--runner", "codex"); err != nil {
		t.Fatalf("finish exited %d (%v), want 0", Code(err), err)
	}
	if got := lastRow(t, filepath.Join(dir, "ledger.jsonl"))["implementer"]; got != "codex" {
		t.Errorf("finish recorded implementer %v, want codex", got)
	}
}

// --agent is not a flag on any command: finish and mark-merged spell
// attribution --runner and audit spells it --reviewer, and no handler
// translates another spelling. Each refuses before anything is written.
func TestAgentIsAnUnknownFlag(t *testing.T) {
	old := "--" + "agent"
	for _, c := range []struct {
		name    string
		cmd     *cobra.Command
		args    []string
		vars    []*string
		project func(t *testing.T) string
	}{
		{name: "mark-merged", cmd: doneCmd, args: []string{"mark-merged", "1"}, vars: []*string{&doneRunner}, project: func(t *testing.T) string {
			dir := fixture(t, "").cfg.Root
			t.Chdir(dir)
			return dir
		}},
		{name: "finish", cmd: finishCmd, args: []string{"finish", "1"}, vars: []*string{&finishRunner}, project: finishedFixture},
		{name: "audit", cmd: auditCmd, args: []string{"audit", "1", "reviewed", "--code-by", "agent"}, vars: []*string{&auditCodeBy, &auditReviewer}, project: func(t *testing.T) string {
			dir := fixture(t, "").cfg.Root
			t.Chdir(dir)
			return dir
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetSpellings(t, c.cmd, c.vars...)
			dir := c.project(t)

			path := filepath.Join(dir, "ledger.jsonl")
			before := ledgerBytes(t, path)
			err := execute(t, append(c.args, old, "x")...)
			if err == nil || !strings.Contains(err.Error(), "unknown flag: "+old) {
				t.Fatalf("%s %s = %v, want cobra's unknown-flag error", c.name, old, err)
			}
			if after := ledgerBytes(t, path); string(after) != string(before) {
				t.Errorf("a refusal wrote to the ledger:\n  before %s\n  after  %s", before, after)
			}
		})
	}
}

// run refuses a runner [runners] does not declare, because the value selects an
// executable. mark-merged takes the same value as attribution, so it cannot refuse: a
// runner that did the work outside drover has no entry to match. That leaves a
// typo indistinguishable from an outside run, which is what the warning is for.
func TestMarkMergedWarnsOnAnUndeclaredRunnerAndStillRecords(t *testing.T) {
	resetSpellings(t, doneCmd, &doneRunner)
	// Two declared runners, because the warning has to name the set and one
	// name cannot show whether the set it names holds still between runs.
	s := fixture(t, "\n[runners]\ncodex  = { interactive = [\"codex\"] }\nclaude = { interactive = [\"claude\"] }\n")
	t.Chdir(s.cfg.Root)

	var out string
	var err error
	stderr := captureErr(t, func() {
		out = capture(t, func() { err = execute(t, "mark-merged", "1", "--runner", "not-a-declared-runner") })
	})
	if err != nil {
		t.Fatalf("done exited %d (%v), want 0: a refusal here makes an outside run unrecordable", Code(err), err)
	}
	if got := lastRow(t, filepath.Join(s.cfg.Root, "ledger.jsonl"))["implementer"]; got != "not-a-declared-runner" {
		t.Errorf("ledger implementer = %v, want not-a-declared-runner", got)
	}
	// Both halves: the name that will sit in the ledger forever, and the set it
	// failed to match. A warning naming one of them cannot be acted on. The set
	// is spelled out in order, so an operator diffing two runs sees a change
	// only where the config changed.
	for _, want := range []string{`"not-a-declared-runner"`, "declared: claude, codex"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the warning does not name %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(out, "not declared") {
		t.Errorf("the warning reached stdout, which a wrapper parses:\n%s", out)
	}
}

// A declared runner is the ordinary case and says nothing. A warning that fires
// on every invocation is one nobody reads on the invocation that matters.
func TestDoneIsSilentOnADeclaredRunner(t *testing.T) {
	resetSpellings(t, doneCmd, &doneRunner)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)

	stderr := captureErr(t, func() {
		capture(t, func() {
			if err := execute(t, "mark-merged", "1", "--runner", "claude"); err != nil {
				t.Fatalf("done exited %d (%v), want 0", Code(err), err)
			}
		})
	})
	if strings.Contains(stderr, "not declared") {
		t.Errorf("a declared runner warned:\n%s", stderr)
	}
}

// finish writes the row from its own branch, which mark-merged's pass cannot
// reach: the warning sits inside the conditional that skips a row
// `mark-merged` already wrote.
func TestFinishWarnsOnAnUndeclaredRunnerAndStillRecords(t *testing.T) {
	resetSpellings(t, finishCmd, &finishRunner)
	dir := finishedFixture(t)

	var err error
	stderr := captureErr(t, func() {
		capture(t, func() { err = execute(t, "finish", "1", "--runner", "not-a-declared-runner") })
	})
	if err != nil {
		t.Fatalf("finish exited %d (%v), want 0", Code(err), err)
	}
	if got := lastRow(t, filepath.Join(dir, "ledger.jsonl"))["implementer"]; got != "not-a-declared-runner" {
		t.Errorf("ledger implementer = %v, want not-a-declared-runner", got)
	}
	for _, want := range []string{"not-a-declared-runner", "claude"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the warning does not name %q:\n%s", want, stderr)
		}
	}
}
