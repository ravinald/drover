package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// rawRows decodes every line of the ledger at path into its keys as written,
// which is the only way to see a key the encoder omitted: a Row decoded from a
// line without "agent" and one with "agent":"" are the same value.
func rawRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(read(t, path)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	return rows
}

// assertAttributed fails unless raw names who wrote its digest under every
// key a body row owes: an operator with no agent still writes "agent":"".
func assertAttributed(t *testing.T, raw map[string]any, cmd, actor string) {
	t.Helper()
	for _, k := range []string{"body", "cmd", "actor", "agent", "ts"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("body row %v has no %q key", raw, k)
		}
	}
	if raw["cmd"] != cmd || raw["actor"] != actor {
		t.Errorf("body row %v names cmd %v actor %v, want %s and %s", raw, raw["cmd"], raw["actor"], cmd, actor)
	}
}

// A body row is refused at the ledger itself when it cannot say who wrote the
// digest, whichever path built it, and leaves the file as it was.
func TestABodyRowWithoutAttributionIsRefused(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	s := openIn(t, dir)
	for _, by := range []ledger.Author{{Cmd: "accept"}, {Actor: runner.ActorOperator}, {}} {
		if err := s.record(ledger.BodyRow("B1", "aaaaaaaaaaaa", by)); err == nil {
			t.Errorf("a body row by %+v was recorded", by)
		}
		if err := ledger.Append(ledgerPath, ledger.Row{ID: "B1", Body: "aaaaaaaaaaaa", Cmd: by.Cmd, Actor: by.Actor}); err == nil {
			t.Errorf("ledger.Append wrote a body row by %+v", by)
		}
	}
	if b, err := os.ReadFile(ledgerPath); err == nil && len(b) > 0 {
		t.Errorf("a refused body row reached the ledger: %s", b)
	}

	if err := s.record(ledger.BodyRow("B1", "bbbbbbbbbbbb", s.author("accept"))); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, ledgerPath)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %v", rows)
	}
	assertAttributed(t, rows[0], "accept", runner.ActorOperator)
	if rows[0]["agent"] != "" {
		t.Errorf("an operator with no agent recorded agent %v, want empty", rows[0]["agent"])
	}
}

// The review stage row carries the body as the session found it, so it owes
// the same attribution as a body row, and rows without a digest gain no key.
func TestTheReviewStageRowIsAttributed(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.launched(it, "review", ledger.StageReview, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.launched(it, "run", ledger.StageImplement, "claude"); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, ledgerPath)
	if len(rows) != 2 {
		t.Fatalf("want two stage rows, got %v", rows)
	}
	assertAttributed(t, rows[0], "review", runner.ActorOperator)
	for _, k := range []string{"body", "cmd", "actor", "agent"} {
		if _, ok := rows[1][k]; ok {
			t.Errorf("the implement stage row %v carries %q", rows[1], k)
		}
	}
}
