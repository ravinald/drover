package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
)

// The scaffold a split writes carries the proposal's numbered list and nothing
// else. scaffoldBody ends in an empty one for a human to fill, and appending
// below it leaves requirements 1 and 2 blank with the real work at 3, which is
// the state the stop marker exists to prevent rather than tidy-up for the
// operator.
func TestSplitWritesOneNumberedList(t *testing.T) {
	dir := draftProject(t, "")
	proposal := "Act as the engineer who owns publication.\n\nRequirements:\n\n1. Publish by rename.\n"
	path := filepath.Join(dir, "proposal.md")
	write(t, path, proposal)
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	if err := execute(t, "split", "1", "atomic local publication", "-f", path); err != nil {
		t.Fatal(err)
	}

	body := readItem(t, dir, "B02")
	if n := len((backlog.Item{Body: body}).Requirements()); n != 1 {
		t.Errorf("the scaffold carries %d numbered requirements, want 1:\n%s", n, body)
	}
	if !strings.Contains(body, "STOP:") {
		t.Errorf("the scaffold lost its stop marker, so it would run unaccepted:\n%s", body)
	}
	if !strings.Contains(body, "Publish by rename.") {
		t.Errorf("the proposal did not reach the scaffold:\n%s", body)
	}
	if strings.Contains(body, "Act as a <role>") {
		t.Errorf("the placeholder prose survived beside the proposal:\n%s", body)
	}
}

// The edge is the whole point: an unmerged dep is what `workState` reads as
// blocked and what keeps `drive` from picking the item up again.
func TestSplitBlocksTheItemItSplits(t *testing.T) {
	dir := draftProject(t, "")
	path := filepath.Join(dir, "proposal.md")
	write(t, path, "Requirements:\n\n1. Publish by rename.\n")
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	if err := execute(t, "split", "1", "atomic local publication", "-f", path); err != nil {
		t.Fatal(err)
	}
	original := readItem(t, dir, "01")
	if !strings.Contains(original, "deps: B2") {
		t.Errorf("the original is not blocked on the item split out of it:\n%s", original)
	}
}

// split's one ledger row is the digest of the body whose deps line it widened.
// It says drover wrote that body, so a review that splits mid-pass is not read as
// one that edited the body by hand; the scaffold gets none, because no review of
// it is under way.
//
// Both help surfaces are audited against what the run did rather than against
// that decision, so reversing it stays one edit with three failures: losing the
// write means both surfaces lose the claim with it, or neither does.
func TestSplitWritesOneBodyRow(t *testing.T) {
	dir := draftProject(t, "")
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	// Seeded so a read of the wrong path cannot pass as a split that appended
	// nothing.
	write(t, ledgerPath, `{"id":"1","merged":true,"date":"2026-09-21"}`+"\n")
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Publish by rename.\n")
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	if err := execute(t, "split", "1", "atomic local publication", "-f", proposal); err != nil {
		t.Fatal(err)
	}

	l, err := ledger.Load(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) == 0 || l.Rows[0].Merged == nil || !*l.Rows[0].Merged {
		t.Fatalf("the seeded row did not survive the split, so this reads no ledger: %+v", l.Rows)
	}
	wrote := len(l.Rows) > 1
	if len(l.Rows) != 2 || l.Rows[1].ID != "1" || l.Rows[1].Body == "" {
		t.Fatalf("want one body row for the split item, got %+v", l.Rows[1:])
	}
	sum, err := bodyDigest(filepath.Join(dir, "work", "01-first.md"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Rows[1].Body != sum {
		t.Errorf("the body row carries %s, the body on disk digests %s", l.Rows[1].Body, sum)
	}

	for _, s := range []struct{ where, text string }{
		{"the Safety line in split.go", splitCmd.Example},
		{`classified["split"].effect`, classified["split"].effect},
	} {
		if claims := mentionsLedgerRow(s.text); claims != wrote {
			t.Errorf("%s claims a ledger row: %t, split wrote one: %t\n%s", s.where, claims, wrote, s.text)
		}
	}
}

// mentionsLedgerRow reports whether a help surface says the command writes to the
// ledger. Dropping the denial before looking is what separates "writes no ledger
// row" from a claim, and it catches a surface carrying both at once.
func mentionsLedgerRow(s string) bool {
	return strings.Contains(strings.ReplaceAll(s, "no ledger row", ""), "ledger")
}

func TestBlockOnIsIdempotent(t *testing.T) {
	dir := draftProject(t, "")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.blockOn(it, "B9"); err != nil {
			t.Fatal(err)
		}
		it, err = s.item("1")
		if err != nil {
			t.Fatal(err)
		}
	}
	body := readItem(t, dir, "01")
	if n := strings.Count(body, "B9"); n != 1 {
		t.Errorf("blocking twice on one id wrote it %d times:\n%s", n, body)
	}
}

// A dependency drover cannot satisfy in any order is refused before anything is
// written. Unreachable from the CLI today, because split always allocates a
// fresh id, and checked because an id reused after a hand edit would otherwise
// leave an item blocked on itself forever with no run able to clear it.
func TestAcyclicRefusesADependencyThatClosesACycle(t *testing.T) {
	draftProject(t, "")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	err = s.acyclic(it, it.ID.Bare())
	if err == nil {
		t.Fatal("blocking an item on itself was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("the refusal does not say what it refused: %v", err)
	}
}

func readItem(t *testing.T, dir, stem string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "work", stem+"-*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one %s-*.md under work/, got %v (%v)", stem, matches, err)
	}
	body, err := os.ReadFile(matches[0]) // #nosec G304 -- the fixture's own item
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
