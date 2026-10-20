package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// The level rule is the tool's reason to exist: an item nobody checked and an
// item whose every requirement was re-confirmed must not look alike, and the
// one combination that would let them is a grader who wrote the code recording
// verified. Every refusal below exits 3 and leaves the ledger byte-identical,
// because a refusal that wrote a row would record a claim the rule just
// rejected.
func TestAuditLevelAndAuthorship(t *testing.T) {
	s := fixture(t, "")
	ledgerPath := filepath.Join(s.cfg.Root, "ledger.jsonl")
	t.Chdir(s.cfg.Root)

	for _, c := range []struct {
		name     string
		level    string
		codeBy   string
		reviewer string
		want     int
	}{
		{name: "the operator records the agent's own report", level: "claimed", codeBy: "agent", want: 0},
		{name: "a review of agent code", level: "reviewed", codeBy: "agent", want: 0},
		{name: "the operator verifies agent code", level: "verified", codeBy: "agent", want: 0},
		{name: "a named author may record a review of their own code", level: "reviewed", codeBy: "sam", reviewer: "sam", want: 0},
		{name: "a third name verifies mixed authorship", level: "verified", codeBy: "agent,sam", reviewer: "dana", want: 0},

		{name: "sam cannot verify code sam wrote", level: "verified", codeBy: "sam", reviewer: "sam", want: ExitRefused},
		{name: "sam cannot verify mixed code sam helped write", level: "verified", codeBy: "agent,sam", reviewer: "sam", want: ExitRefused},
		{name: "a grader nothing names cannot be told from a named author", level: "verified", codeBy: "sam", want: ExitRefused},
		{name: "authorship is not optional", level: "claimed", codeBy: "", want: ExitRefused},
		{name: "author is no longer a value", level: "claimed", codeBy: "author", want: ExitRefused},
		{name: "independent is no longer a value", level: "claimed", codeBy: "independent", want: ExitRefused},
		{name: "an empty list entry", level: "claimed", codeBy: "agent,", want: ExitRefused},
		{name: "an unknown level", level: "thorough", codeBy: "agent", want: ExitRefused},
		{name: "self is no longer a level", level: "self", codeBy: "agent", want: ExitRefused},
		{name: "review is no longer a level", level: "review", codeBy: "agent", want: ExitRefused},
		{name: "requirements is no longer a level", level: "requirements", codeBy: "agent", want: ExitRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The flags are package variables, so a case omitting one would
			// otherwise inherit the previous case's answer.
			auditCodeBy, auditReviewer = "", ""
			before := ledgerBytes(t, ledgerPath)

			args := []string{"audit", "1", c.level, "note"}
			if c.codeBy != "" {
				args = append(args, "--code-by", c.codeBy)
			}
			if c.reviewer != "" {
				args = append(args, "--reviewer", c.reviewer)
			}
			err := execute(t, args...)

			if got := Code(err); got != c.want {
				t.Fatalf("exit %d (%v), want %d", got, err, c.want)
			}
			after := ledgerBytes(t, ledgerPath)
			switch c.want {
			case ExitRefused:
				if !bytes.Equal(before, after) {
					t.Errorf("a refusal appended to the ledger:\n%s", after[len(before):])
				}
			default:
				// Without this the "nothing was appended" assertion above would
				// pass on a command that never writes anything.
				if len(after) <= len(before) {
					t.Errorf("an accepted level recorded nothing")
				}
			}
		})
	}
}

// A refusal of an unknown level names the three that exist, and a refusal of
// verified names the --code-by entry the grader matched, because "you cannot"
// with no reason leaves the reviewer guessing which part of the command was
// wrong.
func TestAuditRefusalsNameWhatMatched(t *testing.T) {
	t.Cleanup(func() { auditCodeBy, auditReviewer = "", "" })
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)

	err := execute(t, "audit", "1", "requirements", "--code-by", "agent", "note")
	if Code(err) != ExitRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"claimed", "reviewed", "verified"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-level refusal does not name %q: %v", want, err)
		}
	}

	auditCodeBy, auditReviewer = "", ""
	err = execute(t, "audit", "1", "verified", "--code-by", "agent,sam", "--reviewer", "sam", "note")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "--code-by names sam") {
		t.Errorf("verified by a listed author: exit %d (%v), want a refusal naming sam", Code(err), err)
	}
}

// agent resolves per session against the item's stage rows. A review session
// drover launched did not write the code a launched implement session wrote,
// so it may verify it; the implement session auditing its own work may not,
// and a launched session with no id to place it cannot be told from the
// implementer either.
func TestAuditCodeByAgentResolvesPerSession(t *testing.T) {
	for _, c := range []struct {
		name    string
		session string
		want    int
		indep   bool
	}{
		{name: "a launched reviewer of agent code is independent", session: "rev1", want: 0, indep: true},
		{name: "the launched implementer auditing its own code", session: "impl1", want: ExitRefused},
		{name: "a launched session with no id", session: "", want: ExitRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Cleanup(func() { auditCodeBy, auditReviewer = "", "" })
			s := fixture(t, "")
			path := filepath.Join(s.cfg.Root, "ledger.jsonl")
			for _, r := range []ledger.Row{
				{ID: "1", Stage: ledger.StageImplement, Runner: "claude", Session: "impl1", Date: "2026-10-19"},
				{ID: "1", Stage: ledger.StageReview, Runner: "claude", Session: "rev1", Date: "2026-10-19"},
			} {
				if err := ledger.Append(path, r); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(s.cfg.Root)
			t.Setenv(runner.EnvActor, runner.ActorAgent)
			t.Setenv(runner.EnvSession, c.session)
			before := ledgerBytes(t, path)

			err := execute(t, "audit", "1", "verified", "--code-by", "agent", "R1 traced")
			if got := Code(err); got != c.want {
				t.Fatalf("exit %d (%v), want %d", got, err, c.want)
			}
			if c.want != 0 {
				if !bytes.Equal(before, ledgerBytes(t, path)) {
					t.Errorf("a refusal appended to the ledger")
				}
				return
			}
			row := lastRow(t, path)
			if row["level"] != "verified" || row["code_by"] != "agent" || row["independent"] != true {
				t.Errorf("audit row = %v, want level verified, code_by agent, independent", row)
			}
			if _, old := row["audit"]; old {
				t.Errorf("audit row still carries the audit key: %v", row)
			}
		})
	}
}

// Only level is read. A row carrying any other audit key is not an audit row.
func TestLedgerReadsOnlyTheLevelKey(t *testing.T) {
	l := &ledger.Ledger{}
	for _, line := range []string{
		`{"id":"1","audit":"requirements","code_by":"independent","date":"2026-10-01"}`,
		`{"id":"2","level":"verified","code_by":"agent","independent":true,"reviewer":"sam","date":"2026-10-01"}`,
	} {
		var r ledger.Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		l.Rows = append(l.Rows, r)
	}
	if st := l.State("1"); st.Audited() {
		t.Errorf("a row keyed audit read as an audit: %+v", st)
	}
	if st := l.State("2"); st.Level != ledger.LevelVerified || !st.Independent || st.Auditor != "sam" {
		t.Errorf("a row keyed level did not read: %+v", st)
	}
}

func ledgerBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The one rule drover enforces is separation of duties, and without the stage
// rows it would be enforced against a value nobody checked. The check
// warns and records: a project may run two sessions of one runner with two
// operators, and refusing there teaches people to record a runner they did not
// use, which costs more than the over-claim it would prevent.
func TestAuditWarnsWhenIndependenceContradictsTheRecord(t *testing.T) {
	for _, c := range []struct {
		name     string
		reviewer string
		st       ledger.State
		warn     bool
	}{
		{
			name: "one runner recorded for both stages",
			st:   ledger.State{Implementer: "claude", Reviewer: "claude"},
			warn: true,
		},
		{
			name:     "a --reviewer naming somebody else has answered the question",
			reviewer: "sam",
			st:       ledger.State{Implementer: "claude", Reviewer: "claude"},
		},
		{
			name:     "a --reviewer naming the implementer makes the same claim again",
			reviewer: "claude",
			st:       ledger.State{Implementer: "claude", Reviewer: "claude"},
			warn:     true,
		},
		{
			name: "two runners is the case independence claims",
			st:   ledger.State{Implementer: "claude", Reviewer: "codex"},
		},
		{
			name: "no review recorded is not a contradiction",
			st:   ledger.State{Implementer: "claude"},
		},
		{
			name: "nothing recorded at all",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			warnSameRunner(&out, "B1", c.reviewer, c.st)
			if got := out.Len() > 0; got != c.warn {
				t.Fatalf("warned = %v, want %v: %q", got, c.warn, out.String())
			}
			if !c.warn {
				return
			}
			// Naming both runners is the whole value: a warning that says only
			// "same runner" leaves the operator to go and read the ledger.
			for _, want := range []string{"implemented by claude", "reviewed by claude", "B1"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("warning does not name %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// The warning must not become a refusal by accident: the row is written and the
// command exits 0, or an honest record becomes the expensive path to take.
func TestAuditRecordsDespiteTheWarning(t *testing.T) {
	t.Cleanup(func() { auditCodeBy, auditReviewer = "", "" })
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(path, ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "claude"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(s.cfg.Root)

	if err := execute(t, "audit", "1", "verified", "--code-by", "agent", "traced"); err != nil {
		t.Fatalf("audit exited %d (%v), want 0", Code(err), err)
	}
	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := l.State("1"); st.Level != ledger.LevelVerified || !st.Independent {
		t.Errorf("the audit was not recorded: %+v", st)
	}
}

// A human reviewer is representable only through --reviewer: an agent reviewer
// leaves a stage row, a person leaves nothing, and nothing is what "nobody
// looked" already means. It must never be defaulted the way finish and mark-merged
// default --runner, because a name drover chose is a reviewer nobody can go
// and ask.
func TestAuditRecordsTheNamedReviewerAndNeverInventsOne(t *testing.T) {
	t.Cleanup(func() { auditCodeBy, auditReviewer = "", "" })
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	t.Chdir(s.cfg.Root)

	if err := execute(t, "audit", "1", "reviewed", "--code-by", "agent", "--reviewer", "sam", "read by hand"); err != nil {
		t.Fatalf("audit exited %d (%v), want 0", Code(err), err)
	}
	if got := lastRow(t, path)["reviewer"]; got != "sam" {
		t.Errorf("audit row records reviewer %v, want sam", got)
	}

	// The flag is a package variable, so the second run would otherwise inherit
	// the first one's answer and pass while defaulting was broken.
	auditReviewer = ""
	if err := execute(t, "audit", "1", "claimed", "--code-by", "agent", "captured"); err != nil {
		t.Fatalf("audit exited %d (%v), want 0", Code(err), err)
	}
	if _, named := lastRow(t, path)["reviewer"]; named {
		t.Errorf("an omitted --reviewer named a reviewer anyway: %v", lastRow(t, path))
	}

	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Last row wins for every audit field, so the unnamed row above withdraws
	// the earlier attribution rather than leaving it standing under a new level.
	if st := l.State("1"); st.Auditor != "" {
		t.Errorf("State carries reviewer %q from a row that named nobody", st.Auditor)
	}
}

func lastRow(t *testing.T, path string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(ledgerBytes(t, path))), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatal(err)
	}
	return row
}
