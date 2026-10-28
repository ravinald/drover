package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

// split's one body row is the digest of the body whose deps line it widened.
// It says drover wrote that body, so a review that splits mid-pass is not read as
// one that edited the body by hand; the scaffold gets none, because no review of
// it is under way. The scaffold gets its origin row instead.
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
	if len(l.Rows) != 3 || l.Rows[1].ID != "B2" || l.Rows[1].Origin == nil || l.Rows[2].ID != "1" || l.Rows[2].Body == "" {
		t.Fatalf("want the scaffold's origin row, then one body row for the split item, got %+v", l.Rows[1:])
	}
	sum, err := bodyDigest(filepath.Join(dir, "work", "01-first.md"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Rows[2].Body != sum {
		t.Errorf("the body row carries %s, the body on disk digests %s", l.Rows[2].Body, sum)
	}
	if l.Rows[2].Cmd != "split" || l.Rows[2].Actor != "operator" {
		t.Errorf("the body row names %q by %q, want split by operator", l.Rows[2].Cmd, l.Rows[2].Actor)
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

// splitRepo is draftProject under git: a trunk whose store.go holds Unique,
// and item 1's branch, which adds widget.go and an insertWidget call to
// store.go. A split citing either of those cites code the trunk lacks.
func splitRepo(t *testing.T) string {
	t.Helper()
	dir := draftProject(t, "")
	isolate(t)
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "pkg", "store.go"), "package pkg\n\nfunc Unique() {}\n")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "trunk")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "-q", "-b", branch)
	write(t, filepath.Join(dir, "pkg", "widget.go"), "package pkg\n\nfunc createWidget() {}\n")
	write(t, filepath.Join(dir, "pkg", "store.go"), "package pkg\n\nfunc Unique() { insertWidget() }\n\nfunc insertWidget() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "feature")
	git(t, dir, "checkout", "-q", "main")
	return dir
}

// resetSplitFlags clears what one split leaves on the shared command, Changed
// included: cobra's mutual-exclusion check reads it, so a --before left set
// refuses the next test's --after.
func resetSplitFlags(t *testing.T) {
	t.Cleanup(func() {
		splitFile, splitInto, splitAfter, splitBefore, acceptYes = "", "", false, false, false
		for _, f := range []string{"after", "before"} {
			splitCmd.Flags().Lookup(f).Changed = false
		}
		acceptCmd.Flags().Lookup("yes").Changed = false
	})
}

// The direction of a split, and whether the new item stands on the trunk.
// Under --after, a proposal citing what only the original's branch has makes
// a pair neither of which can finish; split warns, accept refuses without
// --yes, and check reports it however the pair was written. Under --before the
// original stays runnable and nothing deadlocks.
func TestSplitDirection(t *testing.T) {
	for _, c := range []struct {
		name     string
		flags    []string
		cites    string
		deadlock []string // what split and accept name; empty is no deadlock
		yes      bool     // override the refusal rather than run the printed remedy
	}{
		{
			name:  "trunk citations write --after as today",
			cites: "Fix `pkg/store.go:3`, where `Unique()` lets duplicates in.",
		},
		{
			name:     "a file only the original's branch has",
			cites:    "Fix `pkg/widget.go:3`, where `createWidget()` writes the row.",
			deadlock: []string{"B2 cites pkg/widget.go, which only 1's branch has; with --after these items deadlock", "`createWidget`"},
		},
		{
			name:     "a function only the original's branch has, in a file the trunk has",
			cites:    "Fix `pkg/store.go:3`, where `insertWidget` writes the row.",
			deadlock: []string{"B2 cites `insertWidget`, which only 1's branch has; with --after these items deadlock"},
		},
		{
			name:     "accept --yes overrides the refusal",
			cites:    "Fix `pkg/store.go:3`, where `insertWidget` writes the row.",
			deadlock: []string{"B2 cites `insertWidget`, which only 1's branch has; with --after these items deadlock"},
			yes:      true,
		},
		{
			name:  "--before reverses the deps and leaves the original runnable",
			flags: []string{"--before"},
			cites: "Fix `pkg/widget.go:3`, where `createWidget()` writes the row.",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := splitRepo(t)
			resetSplitFlags(t)
			proposal := filepath.Join(dir, "proposal.md")
			write(t, proposal, c.cites+"\n\nRequirements:\n\n1. Reject duplicates.\n")
			before := len(c.flags) > 0
			args := append([]string{"split", "1", "unique widgets", "--into", "bug", "-f", proposal}, c.flags...)
			var err error
			stderr := captureErr(t, func() { err = execute(t, args...) })
			if err != nil {
				t.Fatal(err)
			}

			original, split := readItem(t, dir, "01"), readItem(t, dir, "B02")
			if before {
				if !strings.Contains(split, "deps: 1\n") || !strings.Contains(original, "deps:\n") {
					t.Errorf("--before did not put the dep on the new item:\n%s\n---\n%s", original, split)
				}
				if !strings.Contains(split, "Split from 1 with --before: this item waits on 1") {
					t.Errorf("the scaffold does not record --before:\n%s", split)
				}
				s, err := open()
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range s.rows() {
					if r.ID == "1" && r.State != "ready" {
						t.Errorf("the original reads %s after --before, want ready", r.State)
					}
				}
			} else {
				if !strings.Contains(original, "deps: B2") || !strings.Contains(split, "\ndeps: \n") {
					t.Errorf("--after did not block the original on the new item:\n%s\n---\n%s", original, split)
				}
				if !strings.Contains(split, "Split from 1 with --after: 1 waits on this item.") {
					t.Errorf("the scaffold does not record --after:\n%s", split)
				}
			}
			for _, want := range c.deadlock {
				if !strings.Contains(stderr, want) {
					t.Errorf("split did not warn %q:\n%s", want, stderr)
				}
			}
			if len(c.deadlock) == 0 && strings.Contains(stderr, "deadlock") {
				t.Errorf("split warned about a deadlock it does not have:\n%s", stderr)
			}

			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			var cycles []string
			for _, f := range s.checkReport().Findings {
				if f.Kind == "BRANCH CYCLE" {
					cycles = append(cycles, f.Message)
				}
			}
			got := strings.Join(cycles, "\n")
			for _, want := range c.deadlock {
				if !strings.Contains(got, want) {
					t.Errorf("check does not report %q: %q", want, cycles)
				}
			}
			if len(c.deadlock) == 0 && got != "" {
				t.Errorf("check reports a cycle that is not there: %q", cycles)
			}

			err = execute(t, "accept", "B2")
			if len(c.deadlock) == 0 {
				if err != nil {
					t.Fatalf("accept refused a split with no deadlock: %v", err)
				}
				return
			}
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), c.deadlock[0]) {
				t.Fatalf("accept of a deadlocked split: %v, want a refusal naming %q", err, c.deadlock[0])
			}
			if !strings.Contains(readItem(t, dir, "B02"), "STOP:") {
				t.Fatal("the refused accept deleted the stop marker anyway")
			}
			if c.yes {
				if err := execute(t, "accept", "B2", "--yes"); err != nil {
					t.Fatalf("accept --yes still refused: %v", err)
				}
				if strings.Contains(readItem(t, dir, "B02"), "STOP:") {
					t.Error("accept --yes left the stop marker in place")
				}
				return
			}

			// The remedy split printed has to be one drover accepts: after it,
			// check finds no cycle and accept goes through without --yes.
			for _, text := range []string{stderr, err.Error()} {
				runRemedy(t, dir, text)
			}
			s, err = open()
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range s.checkReport().Findings {
				if f.Kind == "BRANCH CYCLE" {
					t.Errorf("check still reports the cycle after the printed remedy: %+v", f)
				}
			}
			if err := execute(t, "accept", "B2"); err != nil {
				t.Fatalf("accept refused after the printed remedy: %v", err)
			}
		})
	}
}

// remedy is the dependency flip split, accept and check print.
var remedy = regexp.MustCompile(`drop (\S+) from the deps line of (\S+), and add (\S+) to (\S+)'s`)

// runRemedy applies the flip text names to the fixture's bodies, and fails
// when text names none or names one no body carries.
func runRemedy(t *testing.T, dir, text string) {
	t.Helper()
	m := remedy.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no remedy drover can act on in:\n%s", text)
	}
	if strings.Contains(text, "Shelve") {
		t.Errorf("the remedy still names shelve, which refuses a dep another item names:\n%s", text)
	}
	dep, waiter := m[1], m[2]
	if m[3] != waiter || m[4] != dep {
		t.Fatalf("the remedy does not flip one pair: %q", m[0])
	}
	setDeps(t, dir, waiter, func(deps []string) []string {
		var kept []string
		for _, d := range deps {
			if d != dep {
				kept = append(kept, d)
			}
		}
		return kept
	})
	setDeps(t, dir, dep, func(deps []string) []string {
		for _, d := range deps {
			if d == waiter {
				return deps
			}
		}
		return append(deps, waiter)
	})
}

var depsLine = regexp.MustCompile(`(?m)^deps:.*$`)

// setDeps rewrites one fixture item's deps line by hand, the way an operator
// following a remedy would.
func setDeps(t *testing.T, dir, id string, edit func([]string) []string) {
	t.Helper()
	stem := id
	if len(stem) < 2 || stem[0] < 'A' {
		stem = "0" + stem
	} else if len(stem) < 3 {
		stem = stem[:1] + "0" + stem[1:]
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "work", stem+"-*.md"))
	if len(matches) != 1 {
		t.Fatalf("no single body for %s: %v", id, matches)
	}
	body := readItem(t, dir, stem)
	line := depsLine.FindString(body)
	deps := edit(strings.Fields(strings.TrimPrefix(line, "deps:")))
	write(t, matches[0], strings.Replace(body, line, strings.TrimSpace("deps: "+strings.Join(deps, " ")), 1))
}

// --before leaves the original's deps alone, so an original something already
// blocks is still blocked, and split must not call it runnable.
func TestSplitBeforeOfABlockedItem(t *testing.T) {
	dir := draftProject(t, "")
	resetSplitFlags(t)
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Reject duplicates.\n")
	if err := execute(t, "split", "1", "first split", "--into", "bug", "-f", proposal); err != nil {
		t.Fatal(err)
	}
	splitCmd.Flags().Lookup("after").Changed = false
	var err error
	out := capture(t, func() {
		err = execute(t, "split", "1", "second split", "--into", "bug", "-f", proposal, "--before")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "runnable") {
		t.Errorf("split --before calls an item blocked by B2 runnable:\n%s", out)
	}
	if !strings.Contains(out, "unchanged, still blocked by B2; B3 waits on it") {
		t.Errorf("split --before does not name what still blocks the original:\n%s", out)
	}
}

// show takes the direction from the deps lines, so a pair flipped by hand
// after an --after split reads --before on both items, and says the record
// disagrees.
func TestShowReadsTheDirectionFromTheDeps(t *testing.T) {
	dir := draftProject(t, "")
	resetSplitFlags(t)
	t.Cleanup(func() { showJSON, showNoTracker = false, false })
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Reject duplicates.\n")
	if err := execute(t, "split", "1", "unique widgets", "--into", "bug", "-f", proposal); err != nil {
		t.Fatal(err)
	}
	runRemedy(t, dir, "drop B2 from the deps line of 1, and add 1 to B2's")
	for _, id := range []string{"1", "B2"} {
		var err error
		out := capture(t, func() { err = execute(t, "show", id, "--no-tracker") })
		if err != nil {
			t.Fatal(err)
		}
		want := "B2 split out of 1, --before: B2 waits on 1 (the split recorded --after"
		if !strings.Contains(out, want) {
			t.Errorf("show %s after a hand flip does not say %q:\n%s", id, want, out)
		}
		out = capture(t, func() { err = execute(t, "show", id, "--no-tracker", "--json") })
		showJSON = false
		if err != nil {
			t.Fatal(err)
		}
		var sh Show
		if err := json.Unmarshal([]byte(out), &sh); err != nil {
			t.Fatalf("show --json: %v\n%s", err, out)
		}
		if len(sh.Splits) != 1 || sh.Splits[0].Direction != "--before" || sh.Splits[0].Recorded != "--after" {
			t.Errorf("show %s --json splits = %+v, want direction --before recorded --after", id, sh.Splits)
		}
	}
}

// A pair whose deps lines name each other has no direction the graph can
// give, so show keeps the record and says neither item can run.
func TestShowFlagsSplitPairsThatWaitOnEachOther(t *testing.T) {
	dir := draftProject(t, "")
	resetSplitFlags(t)
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Reject duplicates.\n")
	if err := execute(t, "split", "1", "unique widgets", "--into", "bug", "-f", proposal); err != nil {
		t.Fatal(err)
	}
	setDeps(t, dir, "B2", func(deps []string) []string { return append(deps, "1") })
	var err error
	out := capture(t, func() { err = execute(t, "show", "1", "--no-tracker") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--after: 1 waits on B2 (as recorded; both deps lines name the other") {
		t.Errorf("show does not flag a pair waiting on each other:\n%s", out)
	}
}

// show names the direction on both items, from the one line split wrote.
func TestShowNamesTheSplitDirectionOnBothItems(t *testing.T) {
	for _, c := range []struct {
		flags []string
		want  string
	}{
		{want: "B2 split out of 1, --after: 1 waits on B2"},
		{flags: []string{"--before"}, want: "B2 split out of 1, --before: B2 waits on 1"},
	} {
		t.Run(c.want, func(t *testing.T) {
			dir := draftProject(t, "")
			resetSplitFlags(t)
			proposal := filepath.Join(dir, "proposal.md")
			write(t, proposal, "Requirements:\n\n1. Reject duplicates.\n")
			if err := execute(t, append([]string{"split", "1", "unique widgets", "--into", "bug", "-f", proposal}, c.flags...)...); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"1", "B2"} {
				var err error
				out := capture(t, func() { err = execute(t, "show", id, "--no-tracker") })
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, "split      "+c.want) {
					t.Errorf("show %s does not name the split %q:\n%s", id, c.want, out)
				}
			}
		})
	}
}

// check reads the pair from the deps graph, not from the line split writes, so
// a pair written by hand, or by a split older than the check, is still found.
func TestCheckReportsABranchCycleNoSplitWrote(t *testing.T) {
	dir := splitRepo(t)
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps: B2\n---\n\nRequirements:\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "B02-widgets.md"),
		"---\ntitle: Widgets\ndeps:\n---\n\nRequirements:\n\n1. Fix `createWidget()` in `pkg/widget.go:3`.\n")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	var got []Finding
	for _, f := range s.checkReport().Findings {
		if f.Kind == "BRANCH CYCLE" {
			got = append(got, f)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want the path and the name reported, got %+v", got)
	}
	if got[0].Message != "B2 cites pkg/widget.go, which only 1's branch has; with --after these items deadlock" || got[0].ID != "B2" {
		t.Errorf("finding = %+v", got[0])
	}
}
