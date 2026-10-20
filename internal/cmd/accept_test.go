package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaffold is what intake writes: the marker on its own line at the top of the
// body, the two explanation lines under it, and the blank that closes the block.
// accept removes all four, so the fixture has to carry all of them or the round
// trip is not being measured.
const scaffold = "---\ntitle: Empty feed\ndeps:\nissue: 12\n---\n\n" +
	"STOP: requirements not yet written\n" +
	"drover refuses to launch this item while this line is present. Delete it\n" +
	"once the list below is real.\n\n" +
	"Act as a maintainer.\n\nRequirements:\n\n1.\n2.\n"

// accepted is the same body with intake's whole block gone: what accept has to
// leave, and what a human who followed the marker's own instruction leaves.
const accepted = "---\ntitle: Empty feed\ndeps:\nissue: 12\n---\n\n" +
	"Act as a maintainer.\n\nRequirements:\n\n1.\n2.\n"

// quoting is the body this predicate exists for: an item about intake, whose
// requirements quote the marker inside a fenced block the way this repository's
// own configuration doc does. Under a substring match it is a scaffold, status
// calls it one, and run refuses to launch or even preview an accepted contract.
const quoting = "---\ntitle: The marker is matched twice\ndeps:\n---\n\n" +
	"Act as the maintainer of the intake path.\n\n" +
	"Requirements:\n\n" +
	"1. The marker this project declares is written into .drover.toml:\n\n" +
	"```toml\n[intake]\nstop_marker = \"STOP: requirements not yet written\"\n```\n\n" +
	"2. A body that quotes it on a line of its own inside a fence:\n\n" +
	"```\nSTOP: requirements not yet written\n```\n\n" +
	"is documentation, not a scaffold.\n"

func markerProject(t *testing.T, bodies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["issue"]

[intake]
stop_marker = "STOP: requirements not yet written"

[verify]
default = "true"
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, text := range bodies {
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// One question, one answer. Three inline copies of the test is how status came
// to disagree with run about what a scaffold is.
func TestIsScaffoldIsTheOnlyMarkerMatch(t *testing.T) {
	dir := markerProject(t, map[string]string{
		"B01-scaffold.md": scaffold,
		"B02-quoting.md":  quoting,
	})
	s := openIn(t, dir)

	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.isScaffold(it) {
		t.Error("B1 carries the marker intake planted and is not read as a scaffold")
	}
	// draft answers the same question through the same predicate.
	if err := s.draftable(it); err != nil {
		t.Errorf("draftable refuses a real scaffold: %v", err)
	}

	quoted, err := s.item("B2")
	if err != nil {
		t.Fatal(err)
	}
	if s.isScaffold(quoted) {
		t.Error("a body quoting the marker inside a fence reads as a scaffold")
	}
	if err := s.draftable(quoted); err == nil {
		t.Error("draft would redraft an accepted contract that merely quotes the marker")
	}
}

// The two ends of the predicate, so a rewrite cannot pass by matching nothing.
func TestMarkerLine(t *testing.T) {
	const marker = "STOP: requirements not yet written"
	for _, c := range []struct {
		name string
		text string
		want bool
	}{
		{"own line", "prose\n" + marker + "\nmore", true},
		{"trailing space", "prose\n" + marker + "  \n", true},
		{"inside a fence", "```\n" + marker + "\n```\n", false},
		{"inside a tilde fence", "~~~\n" + marker + "\n~~~\n", false},
		{"quoted in a config line", "```toml\nstop_marker = \"" + marker + "\"\n```\n", false},
		{"embedded in prose", "the body carries " + marker + " until accepted\n", false},
		{"no marker configured", marker + "\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			set := marker
			if c.name == "no marker configured" {
				set = ""
			}
			if got := markerLine(c.text, set) >= 0; got != c.want {
				t.Errorf("markerLine = %v, want %v", got, c.want)
			}
		})
	}
}

// status reports the state and run previews it. Both read the predicate, and
// a fenced quote of the marker must not make either refuse the body.
func TestQuotingBodyIsReadyAndPreviews(t *testing.T) {
	dir := markerProject(t, map[string]string{"B02-quoting.md": quoting})
	s := openIn(t, dir)

	var row Row
	for _, r := range s.rows() {
		if r.ID == "B2" {
			row = r
		}
	}
	if row.State != stateReady {
		t.Errorf("state = %q, want %q", row.State, stateReady)
	}

	runDry = true
	t.Cleanup(func() { runDry = false })
	out := capture(t, func() {
		if err := runCmd.RunE(runCmd, []string{"B2"}); err != nil {
			t.Errorf("run --dry-run refuses a body that only quotes the marker: %v", err)
		}
	})
	if !strings.Contains(out, "B2") {
		t.Errorf("the preview did not render:\n%s", out)
	}
}

// The real scaffold still refuses, so the fix above cannot have been "stop
// checking". A preview of an unfinished item is a contract nobody wrote.
func TestRunStillRefusesARealScaffold(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	openIn(t, dir)

	runDry = true
	t.Cleanup(func() { runDry = false })
	err := runCmd.RunE(runCmd, []string{"B1"})
	if err == nil {
		t.Fatal("run previewed an unfinished scaffold")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
}

// Both halves of the round trip. accept took the marker line alone and left the
// two explanation lines behind, and the trip was byte-identical only because
// nothing had removed them: the accepted body is the assertion that catches it.
func TestAcceptUnacceptRoundTripsTheWholeBlock(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)

	out := capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "accepted: B1") {
		t.Errorf("accept said %q", out)
	}
	if got := read(t, path); got != accepted {
		t.Errorf("accept left part of intake's block behind:\n got %q\nwant %q", got, accepted)
	}

	capture(t, func() {
		if err := unacceptCmd.RunE(unacceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	if got := read(t, path); got != scaffold {
		t.Errorf("round trip is not byte-identical:\n got %q\nwant %q", got, scaffold)
	}
}

// The trip entered from the other end: the body of somebody who read the marker
// and deleted the block it told them to. unaccept has no orphans to lean on and
// has to write the explanation itself, or redrafting starts from a bare marker.
func TestUnacceptRestoresTheBlockAHumanDeleted(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": accepted})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)

	capture(t, func() {
		if err := unacceptCmd.RunE(unacceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	if got := read(t, path); got != scaffold {
		t.Errorf("unaccept restored a bare marker:\n got %q\nwant %q", got, scaffold)
	}
}

// The rule that decides where the block ends, stated as the body that separates
// it from the alternatives: a marker typed by hand with prose directly under it.
// Matching intake's text removes the marker alone; scanning to the next blank
// line would have taken the paragraph with it.
func TestAcceptKeepsProseUnderAHandWrittenMarker(t *testing.T) {
	const handWritten = "---\ntitle: Empty feed\ndeps:\nissue: 12\n---\n\n" +
		"STOP: requirements not yet written\n" +
		"Act as a maintainer. The list below is half of one.\n\n" +
		"Requirements:\n\n1. Do it.\n"
	dir := markerProject(t, map[string]string{"B01-hand.md": handWritten})
	path := filepath.Join(dir, "work", "B01-hand.md")
	t.Chdir(dir)

	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	want := strings.Replace(handWritten, "STOP: requirements not yet written\n", "", 1)
	if got := read(t, path); got != want {
		t.Errorf("accept ate the prose under a hand-written marker:\n got %q\nwant %q", got, want)
	}
}

// The implement prompt is where a surviving line of the block gets read: run renders the
// body, so the assertion belongs on its output rather than on the file. An
// accepted item whose brief opens on "delete this line" names a condition the
// run it is briefing cannot act on.
func TestAcceptedPromptCarriesNoLineOfTheBlock(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	t.Chdir(dir)

	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})

	runDry = true
	t.Cleanup(func() { runDry = false })
	out := capture(t, func() {
		if err := runCmd.RunE(runCmd, []string{"B1"}); err != nil {
			t.Fatalf("run --dry-run refuses an accepted item: %v", err)
		}
	})
	if !strings.Contains(out, "B1") {
		t.Fatalf("the preview did not render:\n%s", out)
	}
	for _, line := range stopBlock("STOP: requirements not yet written") {
		if line == "" {
			continue
		}
		if strings.Contains(out, line) {
			t.Errorf("the prompt carries %q:\n%s", line, out)
		}
	}
}

// Both verbs name a state, not a transition. Asking for a state that already
// holds is not an error, and reporting it as one would make a re-run of a
// script fail on the step that already succeeded.
func TestAcceptAndUnacceptAreIdempotent(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)

	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	after := read(t, path)

	out := capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Errorf("accept on an accepted item: %v", err)
		}
	})
	if !strings.Contains(out, "already accepted") {
		t.Errorf("second accept said %q", out)
	}
	if read(t, path) != after {
		t.Error("the second accept rewrote the file")
	}

	capture(t, func() {
		if err := unacceptCmd.RunE(unacceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	out = capture(t, func() {
		if err := unacceptCmd.RunE(unacceptCmd, []string{"B1"}); err != nil {
			t.Errorf("unaccept on a scaffold: %v", err)
		}
	})
	if !strings.Contains(out, "already a scaffold") {
		t.Errorf("second unaccept said %q", out)
	}
	if read(t, path) != scaffold {
		t.Error("the second unaccept rewrote the file")
	}
}

// A list is the batch an operator types after reading several drafts. One
// mistyped id must cost that id alone: the others are accepted, the failure is
// named on stderr, and the exit is non-zero so a script still sees it.
func TestAcceptTakesAListAndSkipsWhatFails(t *testing.T) {
	dir := markerProject(t, map[string]string{
		"B01-scaffold.md": scaffold,
		"B02-scaffold.md": scaffold,
	})
	t.Chdir(dir)

	var err error
	var out string
	stderr := captureErr(t, func() {
		out = capture(t, func() {
			err = acceptCmd.RunE(acceptCmd, []string{"B1", "B9", "B2"})
		})
	})
	if err == nil {
		t.Fatal("a list with a missing id exited 0")
	}
	if !strings.Contains(err.Error(), "1 of 3 not accepted: B9") {
		t.Errorf("the summary does not name the failed id: %v", err)
	}
	for _, id := range []string{"B1", "B2"} {
		if !strings.Contains(out, "accepted: "+id) {
			t.Errorf("%s was not reported accepted:\n%s", id, out)
		}
	}
	for _, name := range []string{"B01-scaffold.md", "B02-scaffold.md"} {
		if got := read(t, filepath.Join(dir, "work", name)); got != accepted {
			t.Errorf("%s was not accepted past the failure:\n%q", name, got)
		}
	}
	if !strings.Contains(stderr, "accept B9:") {
		t.Errorf("stderr does not name B9:\n%s", stderr)
	}
	if strings.Contains(stderr, "B1") || strings.Contains(stderr, "B2") {
		t.Errorf("stderr reports ids that succeeded:\n%s", stderr)
	}
}

// No id at all is a missing argument, not an empty batch that succeeds.
func TestAcceptNeedsAnID(t *testing.T) {
	err := acceptCmd.Args(acceptCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no id given") {
		t.Errorf("accept with no id: %v", err)
	}
	if err := acceptCmd.Args(acceptCmd, []string{"B1", "B2"}); err != nil {
		t.Errorf("accept refused a list: %v", err)
	}
}

// A merged item is not a scaffold, and the refusal has to name the command that
// withdraws the completion or the operator is left guessing at the order.
func TestUnacceptRefusesADoneItem(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"),
		[]byte(`{"id":"B1","merged":true,"date":"2026-01-01"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	err := unacceptCmd.RunE(unacceptCmd, []string{"B1"})
	if err == nil {
		t.Fatal("unaccept restored the marker on a merged item")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
	if !strings.Contains(err.Error(), "drover unmark-merged B1") {
		t.Errorf("the refusal does not name undone: %v", err)
	}
}

// Neither verb is reachable as a flag. draft's help promises that no flag on it
// clears the marker, and that promise is now the only reason accept exists.
func TestNeitherVerbIsAFlagOnDraftOrRun(t *testing.T) {
	install()
	for _, name := range []string{"draft", "run"} {
		c, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"accept", "unaccept", "shelve", "unshelve"} {
			if c.Flags().Lookup(flag) != nil {
				t.Errorf("%s has a --%s flag", name, flag)
			}
		}
	}
	c, _, err := root.Find([]string{"draft"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Long, "no flag\nhere can") {
		t.Errorf("draft's help no longer claims that no flag on it clears the marker:\n%s", c.Long)
	}
}

// citing is a scaffold whose only citation is the given path, set in backticks.
func citing(path string) string {
	return "---\ntitle: Cites one path\ndeps:\n---\n\n" +
		"STOP: requirements not yet written\n" +
		"drover refuses to launch this item while this line is present. Delete it\n" +
		"once the list below is real.\n\n" +
		"Act as a maintainer.\n\nRequirements:\n\n1. Change `" + path + "`.\n"
}

func acceptOut(t *testing.T, path string) string {
	t.Helper()
	dir := markerProject(t, map[string]string{"B01-cites.md": citing(path)})
	if err := os.MkdirAll(filepath.Join(dir, "internal", "cmd"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "internal", "cmd", "jobs.go"), "package cmd\n")
	t.Chdir(dir)
	return capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatalf("accept on a body citing %s: %v", path, err)
		}
	})
}

// A directory in backticks holds the item apart from everything under it, and
// accept is the last moment its writer reads the body before a drive does.
func TestAcceptNotesACitedDirectory(t *testing.T) {
	out := acceptOut(t, "internal/cmd")
	if !strings.Contains(out, "note: B1 cites the directory internal/cmd;") ||
		!strings.Contains(out, "--jobs") {
		t.Errorf("accept did not note the cited directory:\n%s", out)
	}
}

func TestAcceptNotesNoFileOrMissingPath(t *testing.T) {
	for _, path := range []string{"internal/cmd/jobs.go:197", "internal/nope"} {
		if out := acceptOut(t, path); strings.Contains(out, "note:") {
			t.Errorf("accept on a body citing %s printed a note:\n%s", path, out)
		}
	}
}

// The note and the hold read one definition of a cited path. Were accept to
// match its own pattern, it could warn about a directory dispatch ignores, or
// stay quiet about one dispatch holds on.
func TestAcceptNoteAndDispatchHoldAgree(t *testing.T) {
	out := acceptOut(t, "internal/cmd")
	if !strings.Contains(out, "note: B1 cites the directory internal/cmd;") {
		t.Fatalf("no note at accept:\n%s", out)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	other := b1
	other.Body = "1. Change `internal/cmd/jobs.go:197`.\n"
	other.ID.Num = 2
	_, held := planWaves([]queued{newQueued(other, true), newQueued(b1, true)}, 2)
	if len(held) != 1 || held[0].why != "both cite internal/cmd" {
		t.Errorf("planWaves held = %+v, want B1 held on internal/cmd", held)
	}
}
