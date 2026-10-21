package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
)

// A session drive launches works in the item's own worktree and reports to the
// operator's project: its cwd is a checkout of the item's branch that is not the
// operator's, and DROVER_ROOT names the root its `drover audit` writes under.
func TestADriveSessionRunsInTheItemsWorktree(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	seen := filepath.Join(t.TempDir(), "seen")
	bin := t.TempDir()
	t.Setenv("DROVER_TEST_SEEN", seen)
	stub(t, filepath.Join(bin, "stub-impl"), "#!/bin/sh\n"+
		"git rev-parse --abbrev-ref HEAD >> \"$DROVER_TEST_LAUNCHES\"\n"+
		"printf '%s|%s|%s\\n' \"$(pwd -P)\" \"$DROVER_ROOT\" \"$(git rev-parse --abbrev-ref HEAD)\" >> \"$DROVER_TEST_SEEN\"\n"+
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := driveRows(t); err != nil {
		t.Fatalf("drive ended %v", err)
	}
	b, err := os.ReadFile(seen) // #nosec G304 -- written by the stub
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the stub ran %d times, want 2: %q", len(lines), lines)
	}
	root, _ := filepath.EvalSymlinks(dir)
	for i, want := range []string{"first", "second"} {
		f := strings.Split(lines[i], "|")
		if f[0] == root || strings.HasPrefix(f[0], root+string(filepath.Separator)) {
			t.Errorf("%s ran in the operator's checkout: %s", want, f[0])
		}
		if f[1] != dir {
			t.Errorf("%s saw DROVER_ROOT=%q, want %q", want, f[1], dir)
		}
		if f[2] != want {
			t.Errorf("%s ran on branch %q", want, f[2])
		}
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("the operator's checkout moved to %q", got)
	}
}

// A branch the operator has checked out is one they are working on, and git
// will not check it out twice. That item parks naming where it is checked out,
// and the rest of the queue runs.
func TestABranchCheckedOutByTheOperatorParksOnlyThatItem(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	git(t, dir, "switch", "-q", "-c", "first")

	rows, err := driveRows(t)
	if err != nil {
		t.Fatalf("drive ended %v", err)
	}
	if len(rows) != 2 || rows[0].Park != parkWorktree {
		t.Fatalf("drive reported %+v, want first parked %s", rows, parkWorktree)
	}
	if !strings.Contains(rows[0].Reason, "first") {
		t.Errorf("the park does not name the branch: %s", rows[0].Reason)
	}
	if got := launched(t, launches); len(got) != 1 || got[0] != "second" {
		t.Errorf("launches = %v, want second alone", got)
	}
	if got := headBranch(t, dir); got != "first" {
		t.Errorf("the operator's checkout moved to %q", got)
	}
}

// A path the project asked to have linked has to be one git ignores as a link,
// or the session's `git add -A` would commit a symlink into the operator's
// checkout. The refusal comes before any session is paid for.
func TestATreeLinkGitWouldSeeParksBeforeALaunch(t *testing.T) {
	s, launches := treeProject(t)
	dir := s.cfg.Root
	cfg := filepath.Join(dir, config.FileName)
	b, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(b), "[drive]\n", "[drive]\nworktree_links = [\"notes\"]\n", 1)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\nnotes/\n")
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "notes", "context.md"), "for the agent\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "link notes")

	rows, err := driveRows(t)
	if err != nil {
		t.Fatalf("drive ended %v", err)
	}
	for _, d := range rows {
		if d.Park != parkWorktree || !strings.Contains(d.Reason, "trailing slash") {
			t.Errorf("%s: %s/%s, want %s naming the fix", d.ID, d.Outcome, d.Park, parkWorktree)
		}
	}
	if got := launched(t, launches); len(got) != 0 {
		t.Errorf("a runner launched on %v", got)
	}

	// Ignored the way a link can be matched, the same path reaches the session.
	// The refused trees go first: each holds a branch cut before this commit,
	// and that branch's own ignore rules are the ones a link is judged by.
	for _, d := range rows {
		git(t, dir, "worktree", "remove", "--force", d.Tree)
		git(t, dir, "branch", "-D", d.Branch)
	}
	write(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\nnotes\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "ignore notes as a file too")
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Loop.TreeLinks = []string{"notes"}
	tree, err := s.openItemTree(it, "first", "main")
	defer tree.close()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(tree.Path, "notes", "context.md")); err != nil || string(b) != "for the agent\n" {
		t.Errorf("the linked path did not reach the worktree: %q %v", b, err)
	}
	if st := git(t, tree.Path, "status", "--porcelain"); st != "" {
		t.Errorf("the link dirtied the worktree: %q", st)
	}
}

// A trunk that stops tracking a linked file deletes the branch's copy when the
// branch next merges it, after the tree was opened and linked. The session
// launched after that update still gets the file, from the operator's checkout.
func TestALinkedFileTheTrunkStopsTrackingReachesTheNextSession(t *testing.T) {
	s, _ := treeProject(t)
	dir := s.cfg.Root
	write(t, filepath.Join(dir, "CLAUDE.md"), "tracked\n")
	git(t, dir, "add", "CLAUDE.md")
	git(t, dir, "commit", "-q", "-m", "track CLAUDE.md")
	s.cfg.Loop.TreeLinks = []string{"CLAUDE.md"}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.openItemTree(it, "first", "main")
	defer tree.close()
	if err != nil {
		t.Fatal(err)
	}

	git(t, dir, "rm", "-q", "--cached", "CLAUDE.md")
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore")) // #nosec G304 -- the fixture's own file
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".gitignore"), string(b)+"CLAUDE.md\n")
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-q", "-m", "CLAUDE.md leaves git")
	write(t, filepath.Join(dir, "CLAUDE.md"), "operator's copy\n")
	git(t, tree.Path, "merge", "-q", "--no-edit", "main")
	if _, err := os.Lstat(filepath.Join(tree.Path, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("the merge left CLAUDE.md in the tree (%v); the case under test never arose", err)
	}

	s.tree = tree.Path
	defer func() { s.tree = "" }()
	if _, err := s.launch(context.Background(), it, ledger.StageImplement, "impl", "relink", "body", launchInfo{}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(tree.Path, "CLAUDE.md")); err != nil || string(got) != "operator's copy\n" {
		t.Errorf("the session's tree has CLAUDE.md %q (%v), want the operator's copy", got, err)
	}
	if st := git(t, tree.Path, "status", "--porcelain"); st != "" {
		t.Errorf("the link dirtied the worktree: %q", st)
	}
}

// finish removes the item's worktree before deleting its branch, which git
// refuses while any worktree has the branch checked out.
func TestFinishRemovesTheItemsWorktree(t *testing.T) {
	t.Cleanup(func() { finishDelete = false })
	s, _ := treeProject(t)
	dir := s.cfg.Root
	mergedLaunched(t, s, "1", "first")
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.openItemTree(it, "first", "main")
	if err != nil {
		t.Fatal(err)
	}
	tree.close()

	if err := execute(t, "finish", "1", "--delete-branch"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := os.Stat(tree.Path); !os.IsNotExist(err) {
		t.Errorf("the worktree survived the finish: %v", err)
	}
	if out := git(t, dir, "branch", "--list", "first"); out != "" {
		t.Errorf("the branch survived --delete-branch: %q", out)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !l.State("1").Merged {
		t.Error("finish wrote no completion")
	}
}
