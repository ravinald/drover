package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// originOf is the origin row the ledger at path holds for id, failing the test
// when there is none.
func originOf(t *testing.T, path, id string) ledger.Row {
	t.Helper()
	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Provenance(id)
	if !p.Recorded() {
		t.Fatalf("%s has no origin row in %+v", id, l.Rows)
	}
	return *p.Origin
}

// A command typed at the operator's own terminal is the operator's, and an
// agent driving that terminal is named beside the actor rather than in place of
// it: actor answers whom drover launched, agent who typed.
func TestNewRecordsTheOperatorAndTheAgentDrivingTheTerminal(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	dir := newProject(t)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")

	capture(t, func() {
		if err := execute(t, "new", "bug", "typed by a person"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o := originOf(t, ledgerPath, "B1")
	if o.Origin.Cmd != "new" || o.Origin.Actor != runner.ActorOperator || o.Origin.Agent != "" || o.Origin.Session != "" {
		t.Errorf("a person's new recorded %+v, want cmd new, actor operator, no agent and no session", *o.Origin)
	}
	if o.TS == "" || o.Date == "" {
		t.Errorf("the origin row carries ts %q and date %q, want both", o.TS, o.Date)
	}

	t.Setenv(envClaudeCode, "1")
	capture(t, func() {
		if err := execute(t, "new", "bug", "typed by claude"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o = originOf(t, ledgerPath, "B2")
	if o.Origin.Actor != runner.ActorOperator || o.Origin.Agent != "claude" {
		t.Errorf("new under CLAUDECODE=1 recorded actor %q agent %q, want operator and claude", o.Origin.Actor, o.Origin.Agent)
	}
}

// A finding filed from inside a review session resolves to that session's
// stage row, and its agent comes from the runner drover recorded there rather
// than from whatever the session's environment claims.
func TestNewFromAReviewSessionResolvesToItsDrive(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	dir := newProject(t)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	write(t, filepath.Join(dir, "work", "B01-reviewed.md"), "---\ntitle: reviewed\ndeps:\nkind: bug\n---\n\n1. Fix it.\n")
	write(t, ledgerPath, `{"id":"B1","stage":"review","runner":"codex","session":"5e55","drive":"D3","date":"2026-10-27"}`+"\n")
	t.Setenv(runner.EnvActor, runner.ActorAgent)
	t.Setenv(runner.EnvSession, "5e55")
	t.Setenv(envClaudeCode, "1")

	newFoundIn = "B1"
	capture(t, func() {
		if err := execute(t, "new", "bug", "found in review"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o := originOf(t, ledgerPath, "B2")
	if o.Origin.Actor != runner.ActorAgent || o.Origin.Agent != "codex" || o.Origin.Session != "5e55" || o.Origin.FoundIn != "B1" {
		t.Errorf("recorded %+v, want actor agent, agent codex from the stage row, session 5e55, found_in B1", *o.Origin)
	}

	l, err := ledger.Load(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Provenance("B2")
	if p.Launch == nil {
		t.Fatalf("the origin's session resolved to no stage row: %+v", p)
	}
	if p.Launch.Drive != "D3" || p.Launch.Stage != ledger.StageReview || p.Launch.ID != "B1" {
		t.Errorf("resolved to drive %q stage %q item %q, want D3, review, B1", p.Launch.Drive, p.Launch.Stage, p.Launch.ID)
	}
}

func TestIntakeRecordsTheIssueItCameFrom(t *testing.T) {
	dir := intakeProject(t, map[int]string{44: `{"number":44,"title":"ranked bug","body":"b","state":"OPEN",` +
		`"labels":[{"name":"bug"},{"name":"severity:low"}]}`})
	var err error
	capture(t, func() { err = execute(t, "intake", "44") })
	if err != nil {
		t.Fatalf("intake 44: %v", err)
	}
	o := originOf(t, filepath.Join(dir, "ledger.jsonl"), "B1")
	if o.Origin.Cmd != "intake" || o.Origin.Issue != 44 || o.Origin.Actor != runner.ActorOperator {
		t.Errorf("intake recorded %+v, want cmd intake, issue 44, actor operator", *o.Origin)
	}
}

func TestSplitRecordsTheItemItSplit(t *testing.T) {
	dir := draftProject(t, "")
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Publish by rename.\n")
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	capture(t, func() {
		if err := execute(t, "split", "1", "atomic local publication", "-f", proposal); err != nil {
			t.Fatal(err)
		}
	})
	o := originOf(t, filepath.Join(dir, "ledger.jsonl"), "B2")
	if o.Origin.Cmd != "split" || o.Origin.SplitFrom != "1" {
		t.Errorf("split recorded %+v, want cmd split, split_from 1", *o.Origin)
	}
}

// The latest body row says who drover last saw write the body, and a file that
// no longer digests to it was changed by someone drover did not run. An item
// minted before origins were recorded reads as unrecorded, not as a failure.
func TestAcceptThenAHandEditReadsBackAsUnrecorded(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)
	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})

	provenance := func() ledger.Provenance {
		t.Helper()
		l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the fixture's own item
		if err != nil {
			t.Fatal(err)
		}
		return l.Provenance("B1", bodySum(b), digestBytes(b))
	}

	p := provenance()
	if p.LastBody == nil || p.LastBody.Cmd != "accept" || p.LastBody.Actor != runner.ActorOperator || p.LastBody.TS == "" {
		t.Fatalf("the latest body row is %+v, want accept by operator with a ts", p.LastBody)
	}
	if !p.Matches {
		t.Error("the body accept just wrote reads as edited")
	}
	if p.Recorded() || p.Launch != nil {
		t.Errorf("an item written before any origin row reads as recorded: %+v", p)
	}

	write(t, path, read(t, path)+"\nA line nobody recorded.\n")
	p = provenance()
	if p.LastBody == nil || p.LastBody.Cmd != "accept" {
		t.Fatalf("the hand edit moved the latest body row: %+v", p.LastBody)
	}
	if p.Matches {
		t.Error("a hand edit reads as matching the digest accept recorded")
	}
}

// touch certifies the operator's edit, so its row names the operator.
func TestTouchRecordsTheOperator(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)
	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	write(t, path, read(t, path)+"\nA line the operator wrote.\n")
	t.Setenv(envCodexThread, "019a")
	capture(t, func() {
		if err := execute(t, "touch", "B1", "--reason", "fixed a typo"); err != nil {
			t.Fatal(err)
		}
	})
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	last := l.Provenance("B1").LastBody
	if last == nil || last.Cmd != "touch" || last.Actor != runner.ActorOperator || last.Agent != "codex" {
		t.Errorf("touch recorded %+v, want cmd touch by operator, agent codex", last)
	}
}
