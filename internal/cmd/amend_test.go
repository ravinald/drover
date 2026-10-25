package cmd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

func resetAmendFlags(t *testing.T) {
	t.Cleanup(func() {
		amendFile, amendReason, amendMoved, touchReason = "", "", "", ""
		requestChangesFile, requestChangesOverLimit, requestChangesYes = "", false, false
	})
}

// cleanPassAfter is the review pass drive reads once a reviewer audits the
// body as it stands on disk and writes nothing: the ledger before, the ledger
// with one audit row more, and the file.
func cleanPassAfter(t *testing.T, dir, path string) pass {
	t.Helper()
	l := loadLedger(t, dir)
	before := l.Counts("1")
	l.Rows = append(l.Rows, ledger.Row{ID: "1", Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()})
	p := pass{before: before, after: l.Counts("1")}
	p.readBody(path)
	return p
}

// The whole point of amend: the contract changes and the next clean review
// still reads as one. A hand edit of the same list is the control, and parks.
func TestAmendRecordsTheContractChangeSoACleanPassPasses(t *testing.T) {
	for _, c := range []struct {
		name, list string
		want       int
		byHand     bool
		result     string
	}{
		{name: "a requirement added", list: "1. Do it.\n2. Test it.\n", want: 2, result: passPassed},
		{name: "a requirement replaced", list: "1. Do the other thing,\n   citing `go.mod:1`.\n", want: 1, result: passPassed},
		{name: "the same edit by hand", list: "1. Do it.\n2. Test it.\n", want: 2, byHand: true, result: passEdited},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetAmendFlags(t)
			dir, _ := capProject(t, 0)
			body := filepath.Join(dir, "work", "01-first.md")
			touchReason = ""
			if err := execute(t, "touch", "1", "--reason", "baseline digest"); err != nil {
				t.Fatalf("touch: %v", err)
			}
			rows := ledgerRows(t, dir)
			list := filepath.Join(dir, "reqs.md")
			write(t, list, c.list)

			if c.byHand {
				write(t, body, "---\ntitle: First\ndeps:\n---\n\n"+c.list)
			} else {
				amendFile, amendReason, amendMoved = "", "", ""
				if err := execute(t, "amend", "1", "-f", list, "--reason", "R1 belongs elsewhere"); err != nil {
					t.Fatalf("amend: %v", err)
				}
				l := loadLedger(t, dir)
				if n := len(l.Rows); n != rows+2 {
					t.Fatalf("%d rows after amend, want %d: a body row and an amend row", n, rows+2)
				}
				got := read(t, body)
				if b := l.Rows[rows]; b.Body != bodySum([]byte(got)) || b.Amend != nil {
					t.Errorf("first row is not the body digest alone: %+v", b)
				}
				a := l.Rows[rows+1]
				if a.Amend == nil || a.Amend.From != 1 || a.Amend.To != c.want || a.Note != "R1 belongs elsewhere" || a.Body != "" {
					t.Fatalf("second row is not the amendment: %+v", a)
				}
				if n := len(backlog.Item{Body: got}.Requirements()); n != c.want {
					t.Errorf("%d requirements after amend, want %d:\n%s", n, c.want, got)
				}
				if kept := read(t, filepath.Join(dir, a.Amend.Kept)); !strings.Contains(kept, "1. Do it.") || !strings.Contains(kept, "R1 belongs elsewhere") {
					t.Errorf("the kept file lacks the replaced list or the reason:\n%s", kept)
				}
			}
			if got := rowResult(cleanPassAfter(t, dir, body)); got != c.result {
				t.Errorf("the clean pass after it reads %s, want %s", got, c.result)
			}
		})
	}
}

// Prose around the list stays where it was, and the sections below it too.
func TestAmendReplacesTheListInPlace(t *testing.T) {
	resetAmendFlags(t)
	dir, _ := capProject(t, 0)
	body := filepath.Join(dir, "work", "01-first.md")
	write(t, body, "---\ntitle: First\ndeps:\n---\n\nWhy this exists.\n\n1. Do it.\n   Carefully.\n2. Test it.\n\nAfter the list.\n\n## Invariants\n\n- Holds.\n")
	list := filepath.Join(dir, "reqs.md")
	write(t, list, "1. Do it.\n")
	if err := execute(t, "amend", "1", "-f", list, "--reason", "R2 moves to 2"); err != nil {
		t.Fatalf("amend: %v", err)
	}
	want := "---\ntitle: First\ndeps:\n---\n\nWhy this exists.\n\n1. Do it.\n\nAfter the list.\n\n## Invariants\n\n- Holds.\n"
	if got := read(t, body); got != want {
		t.Errorf("body after amend:\n%s\nwant:\n%s", got, want)
	}
}

// Each refusal leaves the body and the ledger as they were.
func TestAmendRefuses(t *testing.T) {
	for _, c := range []struct {
		name, body, list, reason, actor, want string
	}{
		{name: "no reason", list: "1. A.\n", want: "--reason"},
		{name: "a file with no list", list: "Just prose.\n", reason: "x", want: "no top-level"},
		{name: "a scaffold", body: "STOP: requirements not yet written\n\n1. Do it.\n", list: "1. A.\n", reason: "x", want: "scaffold"},
		{name: "a list split by prose", body: "1. Do it.\n\nAside.\n\n2. Then this.\n", list: "1. A.\n", reason: "x", want: "splits the numbered list"},
		{name: "a launched session", list: "1. A.\n", reason: "x", actor: runner.ActorAgent, want: "operator's step"},
		{name: "a supervising session", list: "1. A.\n", reason: "x", actor: runner.ActorSupervisor, want: "move_requirement"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetAmendFlags(t)
			dir, _ := capProject(t, 0)
			path := filepath.Join(dir, "work", "01-first.md")
			if c.body != "" {
				write(t, path, "---\ntitle: First\ndeps:\n---\n\n"+c.body)
			}
			before, rows := read(t, path), ledgerRows(t, dir)
			list := filepath.Join(dir, "reqs.md")
			write(t, list, c.list)
			if c.actor != "" {
				t.Setenv(runner.EnvActor, c.actor)
			}
			args := []string{"amend", "1", "-f", list}
			if c.reason != "" {
				args = append(args, "--reason", c.reason)
			}
			err := execute(t, args...)
			if err == nil || Code(err) != ExitRefused {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say %q:\n%v", c.want, err)
			}
			if read(t, path) != before || ledgerRows(t, dir) != rows {
				t.Error("a refused amend wrote")
			}
		})
	}
}

// A move is amend on both items, each naming the other.
func TestAmendRecordsBothHalvesOfAMove(t *testing.T) {
	resetAmendFlags(t)
	dir, _ := capProject(t, 0)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n2. Move me.\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Other.\n")
	one, two := filepath.Join(dir, "one.md"), filepath.Join(dir, "two.md")
	write(t, one, "1. Do it.\n")
	write(t, two, "1. Other.\n2. Move me.\n")
	if err := execute(t, "amend", "1", "-f", one, "--reason", "R2 moves to 2", "--moved", "2"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "amend", "2", "-f", two, "--reason", "R2 of 1 lands here", "--moved", "1"); err != nil {
		t.Fatal(err)
	}
	l := loadLedger(t, dir)
	for id, other := range map[string]string{"1": "2", "2": "1"} {
		i := slices.IndexFunc(l.Rows, func(r ledger.Row) bool { return r.ID == id && r.Amend != nil })
		if i < 0 || l.Rows[i].Amend.Moved != other {
			t.Fatalf("%s carries no amend row naming %s: %+v", id, other, l.Rows)
		}
		kept := read(t, filepath.Join(dir, l.Rows[i].Amend.Kept))
		if !strings.Contains(kept, id) || !strings.Contains(kept, other) {
			t.Errorf("%s's kept file does not name both ids:\n%s", id, kept)
		}
	}
}

// A hand edit is reported by check and refused by drive, until touch or amend
// records it. Scaffolds and items with no digest on record are exempt.
func TestAHandEditIsReportedUntilRecorded(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		baseline bool
		reported bool
	}{
		{name: "prose edited by hand", body: "1. Do it.\n\nA new paragraph.\n", baseline: true, reported: true},
		{name: "a requirement removed by hand", body: "Nothing left.\n", baseline: true, reported: true},
		{name: "no digest on record", body: "1. Do it, now.\n", reported: false},
		{name: "a scaffold", body: "STOP: requirements not yet written\n\n1. Do it.\n", baseline: true, reported: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetAmendFlags(t)
			s := fixture(t, "")
			t.Chdir(s.cfg.Root)
			path := filepath.Join(s.cfg.Root, "work", "01-first.md")
			if c.baseline {
				if err := execute(t, "touch", "1", "--reason", "baseline digest"); err != nil {
					t.Fatal(err)
				}
			}
			write(t, path, "---\ntitle: First\ndeps:\n---\n\n"+c.body)
			if err := s.reload(); err != nil {
				t.Fatal(err)
			}
			found := func() *Finding {
				for _, f := range s.checkReport().Findings {
					if f.Kind == "HAND EDITED" {
						return &f
					}
				}
				return nil
			}
			f := found()
			if (f != nil) != c.reported {
				t.Fatalf("HAND EDITED reported = %v, want %v", f != nil, c.reported)
			}
			if !c.reported {
				return
			}
			for _, want := range []string{"body_edited", "drover amend 1", "drover touch 1"} {
				if !strings.Contains(f.Message+" "+f.Detail, want) {
					t.Errorf("the finding does not say %q: %+v", want, f)
				}
			}
			if err := execute(t, "touch", "1", "--reason", "the edit was mine"); err != nil {
				t.Fatalf("touch: %v", err)
			}
			if err := s.reload(); err != nil {
				t.Fatal(err)
			}
			if f := found(); f != nil {
				t.Errorf("still reported after touch: %+v", f)
			}
			l := loadLedger(t, s.cfg.Root)
			if last := l.Rows[len(l.Rows)-1]; last.Body != bodySum([]byte(read(t, path))) || last.Note != "the edit was mine" {
				t.Errorf("touch's row is not the digest and the reason: %+v", last)
			}
			if err := execute(t, "touch", "1", "--reason", "again"); err == nil || Code(err) != ExitRefused {
				t.Errorf("touch on a body that matches its digest: %v, want a refusal", err)
			}
		})
	}
}

// drive holds a hand-edited item before any session, with check's sentences,
// rather than spending an implement and a review to park body_edited.
func TestDriveRefusesAHandEditedItemBeforeLaunch(t *testing.T) {
	resetAmendFlags(t)
	t.Cleanup(func() { driveJSON, driveEvents, driveFor, driveUntil, driveMax = false, false, 0, "", 0 })
	s := driveProject(t)
	if err := execute(t, "touch", "1", "--reason", "baseline digest"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfg.Root, "work", "01-first.md")
	write(t, path, "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n\nEdited by hand.\n")
	git(t, s.cfg.Root, "commit", "-q", "-am", "hand edit")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	what, fix := s.handEdit(it)

	evs, _, err := driveEventsOf(t, "1")
	if err != nil && Code(err) == ExitRefused {
		t.Fatalf("drive refused outright: %v", err)
	}
	i := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evParked && e.Park == parkHandEdited })
	if i < 0 {
		t.Fatalf("no hand_edited park among %+v", evs)
	}
	if !strings.Contains(evs[i].Reason, what) || !strings.Contains(evs[i].Reason, fix) {
		t.Errorf("the park does not carry check's sentences:\n%s", evs[i].Reason)
	}
	if slices.ContainsFunc(evs, func(e DriveEvent) bool { return e.Event == evStage }) {
		t.Error("a hand-edited item reached a stage")
	}
}

// The evidence describes a contract the body does not hold: refused, or under
// --over-limit warned and written only with --yes.
func TestRequestChangesCatchesEvidenceThatContradictsTheContract(t *testing.T) {
	for _, c := range []struct {
		name, evidence string
		amendFirst     bool
		overLimit, yes bool
		refused        bool
		want           string
	}{
		{name: "a requirement number the list lacks", evidence: "## R3 unmet\n\nStill failing.\n", refused: true, want: "no requirement 3"},
		{name: "requirement N in prose", evidence: "Requirement 2 still fails.\n", refused: true, want: "no requirement 2"},
		{name: "a removal the count does not show", evidence: "Requirement 1 was removed; strip its code.\n", refused: true, want: "no drover amend"},
		{name: "a removal after an amend", evidence: "Requirement 2 was removed; strip its code.\n", amendFirst: true},
		{name: "a number no amend removed", evidence: "R9 unmet.\n", amendFirst: true, refused: true, want: "no requirement 9"},
		{name: "a requirement the list holds", evidence: "R1 is unmet: the walk over-excludes.\n"},
		{name: "a number inside a fence", evidence: "The gate failed:\n\n```\nR9 FAIL\n```\n"},
		{name: "code that was removed", evidence: "R1's dead code was removed, but the walk over-excludes.\n"},
		{name: "over the limit without --yes", evidence: "R3 unmet.\n", overLimit: true, refused: true, want: "--yes"},
		{name: "over the limit with --yes", evidence: "R3 unmet.\n", overLimit: true, yes: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetAmendFlags(t)
			dir, evidence := capProject(t, 0)
			path := filepath.Join(dir, "work", "01-first.md")
			if c.amendFirst {
				write(t, path, "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n2. Drop me.\n")
				list := filepath.Join(dir, "reqs.md")
				write(t, list, "1. Do it.\n")
				if err := execute(t, "amend", "1", "-f", list, "--reason", "R2 belongs elsewhere"); err != nil {
					t.Fatal(err)
				}
			}
			write(t, evidence, c.evidence)
			before := read(t, path)
			args := []string{"request-changes", "1", "-f", evidence}
			if c.overLimit {
				args = append(args, "--over-limit")
			}
			if c.yes {
				args = append(args, "--yes")
			}
			var err error
			stderr := captureErr(t, func() { err = execute(t, args...) })
			if !c.refused {
				if err != nil {
					t.Fatalf("refused evidence that matches the contract: %v", err)
				}
				if c.overLimit && !strings.Contains(stderr, "may describe a contract") {
					t.Errorf("--over-limit --yes wrote with no warning:\n%s", stderr)
				}
				return
			}
			if err == nil || Code(err) != ExitRefused {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error()+stderr, c.want) {
				t.Errorf("the refusal does not say %q:\n%v\n%s", c.want, err, stderr)
			}
			if read(t, path) != before {
				t.Error("a refused request rewrote the body")
			}
		})
	}
}

func TestRequirementSpanEndsAtTheListsEnd(t *testing.T) {
	lines := strings.Split("Intro.\n\n1. A.\n   more of A.\n2. B.\nlazy B.\n\n   indented B.\n\nAfter.\n", "\n")
	start, end, err := requirementSpan(lines)
	if err != nil {
		t.Fatal(err)
	}
	if start != 2 || end != 8 {
		t.Errorf("span = %d..%d, want 2..8: %q", start, end, lines[start:end])
	}
}
