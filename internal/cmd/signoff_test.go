package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/reports"
	"github.com/ravinald/drover/internal/runner"
)

const operatorBody = "---\ntitle: First\ndeps:\n---\n\n" +
	"1. The migration ships.\n" +
	"2. [operator] psql on the dev host shows the migration applied.\n"

// operatorProject is one accepted item whose second requirement only the
// operator can meet, with an evidence file for request-changes.
func operatorProject(t *testing.T) (dir, evidence string) {
	t.Helper()
	s := fixture(t, "\n[register]\nbackend = \"none\"\n")
	dir = s.cfg.Root
	write(t, filepath.Join(dir, "work", "01-first.md"), operatorBody)
	evidence = filepath.Join(dir, "findings.md")
	t.Chdir(dir)
	return dir, evidence
}

func signoffRows(t *testing.T, dir string) map[int]bool {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l.SignedOff("1")
}

// A requirement whose check the session's permission mode blocks can burn
// every review round. A request-changes whose findings all trace to an
// [operator] requirement is refused and writes nothing; one carrying a finding
// the branch can fix goes through.
func TestRequestChangesOnAnOperatorRequirementAloneIsRefused(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	for _, c := range []struct {
		name     string
		evidence string
		refused  bool
	}{
		{name: "operator heading alone", refused: true,
			evidence: "### R2 was never checked live\n\nNo psql output in the report.\n"},
		{name: "settled heading beside it", refused: true,
			evidence: "### Settled\n\nR1 holds.\n\n### R2 unmet\n\nNothing ran psql.\n"},
		{name: "no headings, operator only", refused: true,
			evidence: "R2 was never checked on the dev host.\n"},
		{name: "a session finding beside it",
			evidence: "### R1: the migration is missing its down file\n\n### R2 unmet\n"},
		{name: "a regression beside it",
			evidence: "### The loader drops a key (BRANCH-REGRESSION)\n\n### R2 unmet\n"},
		{name: "no requirement named", evidence: "The loader drops a key.\n"},
		{name: "an untraced heading beside it",
			evidence: "### finish.go:900 swallows the merge error\n\nThe branch drops it.\n\n### R2 unmet\n\nNothing ran psql.\n"},
		{name: "an unmet requirement token beside it",
			evidence: "### BRANCH-REQUIREMENT: the loader drops a key\n\n### R2 unmet\n"},
		{name: "a fenced comment is not a finding", refused: true,
			evidence: "### R2 unmet\n\n```\n# the psql never ran\n```\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, evidence := operatorProject(t)
			write(t, evidence, c.evidence)
			requestChangesFile = ""
			err := execute(t, "request-changes", "1", "-f", evidence)
			body, rerr := os.ReadFile(filepath.Join(dir, "work", "01-first.md")) // #nosec G304 -- written by this test
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !c.refused {
				if err != nil {
					t.Fatalf("a request for changes carrying a finding the branch can fix was refused: %v", err)
				}
				if n := requestChangesRows(t, dir); n != 1 {
					t.Errorf("%d changes rows, want 1", n)
				}
				return
			}
			if Code(err) != ExitRefused {
				t.Fatalf("request for changes exited %d (%v), want %d", Code(err), err, ExitRefused)
			}
			if !strings.Contains(err.Error(), "R2: psql on the dev host") {
				t.Errorf("the refusal does not name the requirement: %v", err)
			}
			if strings.Contains(c.evidence, "### Settled") && !strings.Contains(err.Error(), "Read as structure, not findings:\n    ### Settled") {
				t.Errorf("the refusal does not name the heading it read as structure: %v", err)
			}
			if n := requestChangesRows(t, dir); n != 0 {
				t.Errorf("a refused request for changes wrote %d changes rows", n)
			}
			if string(body) != operatorBody {
				t.Errorf("a refused request for changes rewrote the body:\n%s", body)
			}
		})
	}
}

// signoff is sealed: a session signing off the check it was told not to attempt
// certifies its own work. The operator's call records the row, and a number
// that is not an [operator] requirement is refused by name.
func TestSignoffRefusesALaunchedSession(t *testing.T) {
	for _, actor := range []string{runner.ActorAgent, "supervisor"} {
		t.Run(actor, func(t *testing.T) {
			dir, _ := operatorProject(t)
			t.Setenv(runner.EnvActor, actor)
			err := execute(t, "signoff", "1", "2", "ran psql")
			if Code(err) != ExitRefused {
				t.Fatalf("attest under DROVER_ACTOR=%s exited %d (%v), want %d", actor, Code(err), err, ExitRefused)
			}
			if got := signoffRows(t, dir); len(got) != 0 {
				t.Errorf("a refused attest wrote %v", got)
			}
		})
	}

	dir, _ := operatorProject(t)
	t.Setenv(runner.EnvActor, "")
	err := execute(t, "signoff", "1", "1", "looked at it")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "R2") {
		t.Errorf("attest on a session requirement exited %d (%v), want 3 naming R2", Code(err), err)
	}
	if err := execute(t, "signoff", "1", "2", "psql -c 'select max(version) from schema_migrations' printed 112"); err != nil {
		t.Fatalf("the operator's attest: %v", err)
	}
	if got := signoffRows(t, dir); !got[2] || len(got) != 1 {
		t.Errorf("attested %v, want R2 alone", got)
	}
}

// The merge holds on an unattested [operator] requirement, with the pull
// request open and needs_operator naming it, and goes through once signoff has
// recorded it. Two drives of the same fixture, differing by one ledger row.
func TestTheMergeHoldsUntilSignoff(t *testing.T) {
	t.Cleanup(func() { driveJSON, driveEvents, driveFor, driveUntil, driveMax = false, false, 0, "", 0 })
	for _, attested := range []bool{false, true} {
		name := "unattested"
		if attested {
			name = "signed_off"
		}
		t.Run(name, func(t *testing.T) {
			ghLog := mergeProject(t)
			dir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "work", "01-first.md"),
				"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES\n2. [operator] Check it live.\n")
			if attested {
				if err := execute(t, "signoff", "1", "2", "checked it live"); err != nil {
					t.Fatal(err)
				}
			}

			evs, stderr, err := driveEventsOf(t, "1")
			if err != nil {
				t.Fatalf("drive: %v\n%s", err, stderr)
			}
			var merges []string
			for _, c := range ghCalls(t, ghLog) {
				if strings.HasPrefix(c, "pr merge") {
					merges = append(merges, c)
				}
			}
			var parked, needs *DriveEvent
			for i, e := range evs {
				switch {
				case e.Event == evParked && e.ID == "1":
					parked = &evs[i]
				case e.Event == evNeedsOperator && e.ID == "1":
					needs = &evs[i]
				}
			}

			if attested {
				if parked != nil {
					t.Errorf("an attested item parked %s: %s", parked.Park, parked.Reason)
				}
				if len(merges) != 1 {
					t.Errorf("the forge was asked to merge %d times, want 1: %v", len(merges), merges)
				}
				return
			}
			if parked == nil || parked.Park != parkOperatorRequirement || parked.PR == 0 {
				t.Fatalf("item 1 parked %+v, want %s with its pull request open", parked, parkOperatorRequirement)
			}
			if len(merges) != 0 {
				t.Errorf("the forge was asked to merge an item with a pending [operator] requirement: %v", merges)
			}
			if needs == nil || needs.Action != "signoff" ||
				!strings.Contains(needs.Reason, "R2") || !strings.Contains(needs.Run, "drover signoff 1 2") {
				t.Errorf("needs_operator is %+v, want signoff naming R2 and the attest", needs)
			}
		})
	}
}

// postFinishProject finishes B1 under a file register with [finish] post_check pointing
// at a stub that records its environment and exits with code.
func postFinishProject(t *testing.T, code, policy string) (dir, envLog string) {
	t.Helper()
	bin := t.TempDir()
	envLog = filepath.Join(bin, "env.log")
	after := filepath.Join(bin, "after")
	stub(t, after, "#!/bin/sh\n"+
		"printf 'item=%s pr=%s sha=%s\\n' \"$DROVER_ITEM\" \"$DROVER_PR\" \"$DROVER_SHA\" >> "+envLog+"\n"+
		"echo 'schema_migrations at 112'\necho 'second line'\nexit "+code+"\n")
	conf := fileProject + "\n[finish]\npost_check = [\"" + after + "\"]\n"
	if policy != "" {
		conf += "\n[supervise]\npost_finish_check = \"" + policy + "\"\n"
	}
	dir, _ = finishProject(t, conf)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)
	return dir, envLog
}

// [finish] post_check exit 0 records the first line it printed; non-zero files a
// follow-up routed to the item, and finish exits 0 either way, because the merge
// already happened. post_finish_check at its default runs nothing.
func TestFinishAfterRecordsOrFilesAndNeverFailsTheFinish(t *testing.T) {
	for _, c := range []struct {
		name, code, policy string
		ran, row, filed    bool
	}{
		{name: "exit 0", code: "0", policy: "auto", ran: true, row: true},
		{name: "exit 1", code: "1", policy: "auto", ran: true, filed: true},
		{name: "policy off", code: "0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, envLog := postFinishProject(t, c.code, c.policy)
			var err error
			out := capture(t, func() { err = execute(t, "finish", "B1") })
			if Code(err) != 0 {
				t.Fatalf("finish exited %d (%v):\n%s", Code(err), err, out)
			}
			if !strings.Contains(out, "ledger: merged") {
				t.Errorf("finish did not record the merge:\n%s", out)
			}

			env, _ := os.ReadFile(envLog) // #nosec G304 -- written by the stub
			if c.ran != (len(env) > 0) {
				t.Fatalf("[finish] post_check ran=%v, want %v:\n%s", len(env) > 0, c.ran, out)
			}
			if c.ran && (!strings.Contains(string(env), "item=B1 ") || strings.Contains(string(env), "sha=\n")) {
				t.Errorf("[finish] post_check saw %q, want DROVER_ITEM=B1 and a DROVER_SHA", env)
			}

			l, lerr := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
			if lerr != nil {
				t.Fatal(lerr)
			}
			var rows []string
			for _, r := range l.Rows {
				if r.ID == "B1" && r.PostFinish != "" {
					rows = append(rows, r.PostFinish)
				}
			}
			switch {
			case c.row && (len(rows) != 1 || rows[0] != "schema_migrations at 112"):
				t.Errorf("post_finish rows %q, want the first line of the output", rows)
			case !c.row && len(rows) != 0:
				t.Errorf("post_finish rows %q, want none", rows)
			}

			reg, rerr := register.Read(openIn(t, dir).cfg.Abs(openIn(t, dir).cfg.Paths.Register))
			if rerr != nil {
				t.Fatal(rerr)
			}
			var owned []register.Row
			for _, r := range reg.Rows {
				if len(r.Owners) == 1 && r.Owners[0] == "B1" {
					owned = append(owned, r)
				}
			}
			if c.filed != (len(owned) == 1) {
				t.Errorf("filed %d follow-ups against B1, want filed=%v:\n%s", len(owned), c.filed, out)
			}
		})
	}
}

// The implement prompt names an [operator] requirement under its own heading,
// and the report checklist marks its box.
func TestAnOperatorRequirementIsMarkedWhereverItIsListed(t *testing.T) {
	d := prompts.Data{ID: "1", Title: "First", Body: operatorBody}
	out, err := prompts.Render("default", prompts.DefaultTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.Index(out, "Operator requirements: not yours to attempt.")
	if heading < 0 || !strings.Contains(out[heading:], "R2: psql on the dev host shows the migration applied.") {
		t.Errorf("the prompt does not list R2 under its own heading:\n%s", out)
	}
	checks := strings.Join(reports.Requirements(operatorBody), "\n")
	if !strings.Contains(checks, "- [ ] R2 (operator): psql on the dev host") ||
		strings.Contains(checks, "R1 (operator)") {
		t.Errorf("result checklist:\n%s\nwant R2 alone marked operator", checks)
	}
}
