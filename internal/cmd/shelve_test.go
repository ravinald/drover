package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The move is the whole command, and the filename is what survives it: the id
// keeps its allocation and the slug keeps the branch an item runs on.
func TestShelveAndUnshelvePreserveTheFilename(t *testing.T) {
	dir := shelfProject(t, map[string]string{"F03-wont-do.md": body("Not doing this", "")}, map[string]string{})
	t.Chdir(dir)

	out := capture(t, func() {
		if err := shelveCmd.RunE(shelveCmd, []string{"F3"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "shelved: F3") {
		t.Errorf("shelve said %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "F03-wont-do.md")); !os.IsNotExist(err) {
		t.Error("the live body is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "shelved", "F03-wont-do.md")); err != nil {
		t.Fatalf("the shelved body is not there: %v", err)
	}

	out = capture(t, func() {
		if err := unshelveCmd.RunE(unshelveCmd, []string{"F3"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "unshelved: F3") {
		t.Errorf("unshelve said %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "F03-wont-do.md")); err != nil {
		t.Fatalf("the body did not come back: %v", err)
	}
}

// The shelf is optional, so the refusal has to name the key rather than report
// a directory that was never configured as missing.
func TestShelveRefusesWithNoShelfConfigured(t *testing.T) {
	dir := shelfProject(t, map[string]string{"F03-wont-do.md": body("Not doing this", "")}, nil)
	t.Chdir(dir)

	err := shelveCmd.RunE(shelveCmd, []string{"F3"})
	if err == nil {
		t.Fatal("shelve moved a body with no shelf configured")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
	if !strings.Contains(err.Error(), "[paths] shelf") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

// One directory cannot be both, and a rename into itself would report a success
// that moved nothing.
func TestShelveRefusesAShelfPointingAtTheBodies(t *testing.T) {
	dir := shelfProjectAt(t, "work", map[string]string{"F03-wont-do.md": body("Not doing this", "")}, nil)
	t.Chdir(dir)

	err := shelveCmd.RunE(shelveCmd, []string{"F3"})
	if err == nil {
		t.Fatal("shelve renamed a body into its own directory")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
}

// SHELF CLASH is a state check reports after somebody reached it with mv. The
// point of the verbs is that it is unreachable through them, in either
// direction.
func TestTheVerbsRefuseAShelfClash(t *testing.T) {
	t.Run("shelve onto an id already shelved", func(t *testing.T) {
		dir := shelfProject(t,
			map[string]string{"F03-new-slug.md": body("Live", "")},
			map[string]string{"F03-old-slug.md": "retired\n"})
		t.Chdir(dir)

		err := shelveCmd.RunE(shelveCmd, []string{"F3"})
		if err == nil {
			t.Fatal("shelve put a second body for one id on the shelf")
		}
		if Code(err) != ExitRefused {
			t.Errorf("exit %d, want %d", Code(err), ExitRefused)
		}
		for _, want := range []string{"F03-old-slug.md", "F03-new-slug.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		}
	})

	t.Run("unshelve onto a live id", func(t *testing.T) {
		dir := shelfProject(t,
			map[string]string{"F03-new-slug.md": body("Live", "")},
			map[string]string{"F03-old-slug.md": "retired\n"})
		t.Chdir(dir)

		err := unshelveCmd.RunE(unshelveCmd, []string{"F3"})
		if err == nil {
			t.Fatal("unshelve revived a body over a live item's identity")
		}
		if Code(err) != ExitRefused {
			t.Errorf("exit %d, want %d", Code(err), ExitRefused)
		}
		if _, err := os.Stat(filepath.Join(dir, "shelved", "F03-old-slug.md")); err != nil {
			t.Error("the refusal moved the file anyway")
		}
	})
}

// Shelving something another item waits on trades one finding for several, and
// the refusal names them so the operator does not have to grep the deps lines.
func TestShelveRefusesAnIDOthersDependOn(t *testing.T) {
	dir := shelfProject(t, map[string]string{
		"F03-wont-do.md": body("Not doing this", ""),
		"B01-first.md":   body("First", "F3"),
		"B02-second.md":  body("Second", "F3 B1"),
	}, map[string]string{})
	t.Chdir(dir)

	err := shelveCmd.RunE(shelveCmd, []string{"F3"})
	if err == nil {
		t.Fatal("shelve left two items MISSING DEP")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
	for _, want := range []string{"B1", "B2", "MISSING DEP"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "F03-wont-do.md")); err != nil {
		t.Error("the refusal moved the file anyway")
	}
}

// A refusal is only useful if the state it describes is the one check reports.
// This drives both: reach the clash by hand, then confirm the finding.
func TestTheRefusedStateIsWhatCheckReports(t *testing.T) {
	dir := shelfProject(t,
		map[string]string{"F03-new-slug.md": body("Live", "")},
		map[string]string{"F03-old-slug.md": "retired\n"})

	got := findings(openIn(t, dir).checkReport(), "SHELF CLASH")
	if len(got) != 1 {
		t.Fatalf("SHELF CLASH findings = %d, want 1: %+v", len(got), got)
	}
}

// unshelve is the only way back, so an id that is not there has to say where it
// looked rather than report a move that did not happen.
func TestUnshelveNamesWhereItLooked(t *testing.T) {
	dir := shelfProject(t, map[string]string{"F03-live.md": body("Live", "")}, map[string]string{})
	t.Chdir(dir)

	err := unshelveCmd.RunE(unshelveCmd, []string{"B1"})
	if err == nil {
		t.Fatal("unshelve moved a body that is not on the shelf")
	}
	if !strings.Contains(err.Error(), "shelved/B01-*.md") {
		t.Errorf("the error does not name where it looked: %v", err)
	}
}

// The one body the verb exists for. A body that will not read is exactly what
// gets retired, and resolving the id through the backlog map alone reported it
// as a file nobody had written while check was reading it in the same process.
func TestShelveRetiresABodyThatWillNotParse(t *testing.T) {
	dir := shelfProject(t,
		map[string]string{"B03-broken.md": "no frontmatter here at all\n"},
		map[string]string{})
	t.Chdir(dir)

	out := capture(t, func() {
		if err := shelveCmd.RunE(shelveCmd, []string{"B3"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "shelved: B3") {
		t.Errorf("shelve said %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "B03-broken.md")); !os.IsNotExist(err) {
		t.Error("the unreadable body is still live")
	}
	if _, err := os.Stat(filepath.Join(dir, "shelved", "B03-broken.md")); err != nil {
		t.Fatalf("the shelved body is not there: %v", err)
	}
}

// An id with no file behind it is still refused: there is nothing to move, and
// a shelf entry conjured from a bare id would spend an id nobody authored.
func TestShelveStillRefusesAnIDWithNoBody(t *testing.T) {
	dir := shelfProject(t, map[string]string{"B01-live.md": body("Live", "")}, map[string]string{})
	t.Chdir(dir)

	err := shelveCmd.RunE(shelveCmd, []string{"B9"})
	if err == nil {
		t.Fatal("shelve moved a body that does not exist")
	}
	if !strings.Contains(err.Error(), "no such item: B9") {
		t.Errorf("the refusal does not say the body is missing: %v", err)
	}
}

// Refusing is right; calling the file missing is not. Every command taking an
// id resolves through one function, so the sentence is the same wherever the
// body will not parse.
func TestCommandsRefuseAnUnparseableBodyByName(t *testing.T) {
	dir := shelfProject(t, map[string]string{
		"B01-live.md":   body("Live", ""),
		"B03-broken.md": "no frontmatter here at all\n",
	}, map[string]string{})
	t.Chdir(dir)

	for _, args := range [][]string{{"show", "B3"}, {"audit", "B3", "claimed"}, {"mark-merged", "B3"}} {
		err := execute(t, args...)
		if err == nil {
			t.Errorf("%v: accepted a body that will not parse", args)
			continue
		}
		for _, want := range []string{"work/B03-broken.md", "will not parse", "Repair the frontmatter"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: refusal does not carry %q: %v", args, want, err)
			}
		}
		if strings.Contains(err.Error(), "no body at") {
			t.Errorf("%v: refusal calls a body on disk missing: %v", args, err)
		}
	}
}

// The shelved case is a third answer, not the missing one: the body is on the
// shelf, and the way back is a command rather than a text editor.
func TestCommandsNameUnshelveForAShelvedID(t *testing.T) {
	dir := shelfProject(t,
		map[string]string{"B01-live.md": body("Live", "")},
		map[string]string{"F03-retired.md": "retired\n"})
	t.Chdir(dir)

	err := execute(t, "show", "F3")
	if err == nil {
		t.Fatal("show rendered a shelved item")
	}
	for _, want := range []string{"is shelved", "shelved/F03-retired.md", "drover unshelve F3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not carry %q: %v", want, err)
		}
	}
}
