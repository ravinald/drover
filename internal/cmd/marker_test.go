package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// plantMarker writes a session marker into dir, as runner.Run would.
func plantMarker(t *testing.T, dir string, m runner.Marker) string {
	t.Helper()
	path := runner.MarkerPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func goneDrover(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// A session drover did not exec carries no DROVER_ACTOR, and the marker in its
// tree seals the same set the variable does. DROVER_ACTOR=operator is the one
// way a person in that tree opens it again.
func TestAMarkerSealsWhatTheVariableSealsAndOperatorOpensIt(t *testing.T) {
	dir := t.TempDir()
	plantMarker(t, dir, runner.Marker{Actor: runner.ActorAgent, Session: "s-t3", PID: os.Getpid()})
	t.Chdir(dir)
	for _, name := range sealedSet {
		if err := gate(name); err == nil || Code(err) != ExitRefused {
			t.Errorf("%s under a marker: %v, want a refusal with exit %d", name, err, ExitRefused)
		}
	}
	if err := noNestedLaunch(); err == nil {
		t.Error("a launch from a sealed tree was allowed")
	}
	if got := runner.Session(); got != "s-t3" {
		t.Errorf("Session() = %q, want the marker's s-t3", got)
	}

	t.Setenv(runner.EnvActor, runner.ActorOperator)
	for _, name := range sealedSet {
		if err := gate(name); err != nil {
			t.Errorf("%s as %s=%s: %v", name, runner.EnvActor, runner.ActorOperator, err)
		}
	}
	if err := noNestedLaunch(); err != nil {
		t.Errorf("a person's launch from a sealed tree: %v", err)
	}
	if err := noNestedDrive(); err != nil {
		t.Errorf("a person's drive from a sealed tree: %v", err)
	}
}

// The gate tested through the real root, on a command that writes: the marker
// refuses accept with exit 3, and operator lets the same accept through.
func TestAcceptUnderAMarkerRefusesUntilAPersonSaysOtherwise(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	plantMarker(t, dir, runner.Marker{Actor: runner.ActorAgent, Session: "s-t3", Root: dir, PID: os.Getpid()})
	t.Chdir(dir)
	if err := execute(t, "accept", "B1"); Code(err) != ExitRefused {
		t.Fatalf("accept under a marker: %v (exit %d), want exit %d", err, Code(err), ExitRefused)
	}
	t.Setenv(runner.EnvActor, runner.ActorOperator)
	if err := execute(t, "accept", "B1"); err != nil {
		t.Fatalf("accept as %s: %v", runner.ActorOperator, err)
	}
	if got := read(t, filepath.Join(dir, "work", "B01-scaffold.md")); got != accepted {
		t.Errorf("accept as %s left %q", runner.ActorOperator, got)
	}
}

func TestCheckReportsAStaleOrTrackedMarkerAndNotALiveOne(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-live.md": body("Live", "")})
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	path := plantMarker(t, dir, runner.Marker{Actor: runner.ActorAgent, Session: "s-live", PID: os.Getpid()})
	if got := findings(openIn(t, dir).checkReport(), "SESSION MARKER"); len(got) != 0 {
		t.Errorf("a live session's marker was reported: %+v", got)
	}

	plantMarker(t, dir, runner.Marker{Actor: runner.ActorAgent, Session: "s-gone", PID: goneDrover(t)})
	got := findings(openIn(t, dir).checkReport(), "SESSION MARKER")
	if len(got) != 1 || !strings.Contains(got[0].Message, "s-gone") || got[0].Detail != "rm "+path {
		t.Fatalf("a stale marker: %+v, want one finding naming s-gone and rm %s", got, path)
	}

	trees, err := openIn(t, dir).projectTrees()
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(trees, "b01")
	stale := plantMarker(t, tree, runner.Marker{Actor: runner.ActorAgent, Session: "s-tree", PID: goneDrover(t)})
	got = findings(openIn(t, dir).checkReport(), "SESSION MARKER")
	if len(got) != 2 || got[1].Path != stale {
		t.Errorf("a stale marker in a drive worktree: %+v, want a second finding at %s", got, stale)
	}
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}

	plantMarker(t, dir, runner.Marker{Actor: runner.ActorAgent, Session: "s-live", PID: os.Getpid()})
	if out, err := exec.Command("git", "-C", dir, "add", "-f", ".drover/session").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	got = findings(openIn(t, dir).checkReport(), "SESSION MARKER")
	if len(got) != 1 || got[0].Path != ".drover/session" || !strings.Contains(got[0].Detail, "git rm --cached") {
		t.Errorf("a tracked marker: %+v, want one finding naming .drover/session and git rm --cached", got)
	}
	if err := execute(t, "check"); Code(err) != ExitFindings {
		t.Errorf("check with a tracked marker exited %d, want %d", Code(err), ExitFindings)
	}
}

func TestInitIgnoresTheMarkerAndNotTheBacklog(t *testing.T) {
	dir := t.TempDir()
	if added, err := ignoreMarker(dir); err != nil || !added {
		t.Fatalf("ignoreMarker on no .gitignore: %v, %v", added, err)
	}
	if added, err := ignoreMarker(dir); err != nil || added {
		t.Errorf("a second ignoreMarker wrote again: %v, %v", added, err)
	}
	if got := read(t, filepath.Join(dir, ".gitignore")); got != "/.drover/session\n" {
		t.Errorf(".gitignore = %q", got)
	}

	other := t.TempDir()
	write(t, filepath.Join(other, ".gitignore"), "/bin\n.drover/")
	if added, err := ignoreMarker(other); err != nil || added {
		t.Errorf("a .gitignore already ignoring .drover/ was written: %v, %v", added, err)
	}

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	write(t, filepath.Join(repo, "go.mod"), "module x\n")
	t.Chdir(repo)
	initVerify = "true"
	t.Cleanup(func() { initVerify = "" })
	if err := execute(t, "init", "--verify", "true"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := read(t, filepath.Join(repo, ".gitignore")); got != "/.drover/session\n" {
		t.Errorf("init left .gitignore as %q", got)
	}
}

// An operator reading a park needs to know which launch holds the tree.
func TestShowNamesTheSessionHoldingTheWorktree(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-live.md": body("Live", "")})
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if m := s.marker(it); m != nil {
		t.Fatalf("an item with no worktree has a marker: %+v", m)
	}
	tree, err := s.treePath(it)
	if err != nil {
		t.Fatal(err)
	}
	plantMarker(t, tree, runner.Marker{Actor: runner.ActorAgent, Session: "s-impl", PID: os.Getpid()})
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"B1","stage":"implement","runner":"claude","session":"s-impl","date":"2026-10-25"}`+"\n")

	out := capture(t, func() {
		if err := execute(t, "show", "B1", "--no-tracker"); err != nil {
			t.Fatal(err)
		}
	})
	want := "held by    session s-impl (agent, drover pid "
	if !strings.Contains(out, want) || !strings.Contains(out, "the implement stage row of 2026-10-25") {
		t.Errorf("show printed\n%s\nwant a line with %q naming the implement stage row", out, want)
	}
}

// The opt-out is named where an agent reads the refusal's reason, and every
// launch command says what it leaves in the tree. Rendered on a stand-in per
// name, because expand appends to the command it is given and the real ones
// render once per process.
func TestHelpNamesTheMarkerAndTheOptOut(t *testing.T) {
	help := func(name string) string {
		c := &cobra.Command{Use: name}
		expand(c, map[string]string{})
		return strings.Join(strings.Fields(c.Long), " ")
	}
	for _, name := range sealedSet {
		if !strings.Contains(help(name), "exports DROVER_ACTOR=operator first") {
			t.Errorf("%s --help: the Actor line does not name DROVER_ACTOR=operator", name)
		}
	}
	for _, name := range []string{"run", "review", "draft", "drive"} {
		if !strings.Contains(help(name), "writes .drover/session") {
			t.Errorf("%s --help: the Safety line does not name the session marker", name)
		}
	}
}
