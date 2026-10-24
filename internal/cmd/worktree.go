package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/runner"
)

// itemTree is the worktree drive builds one item in.
//
// drive never switches the operator's checkout. Every item gets a checkout of
// its own branch under the per-user state directory, so whatever one item
// leaves behind (a generated file its branch ignores and the trunk does not, a
// session that ran out of budget mid-edit) stays in that item's tree and parks
// that item alone. In one shared checkout the same file stopped every item
// queued behind it.
type itemTree struct {
	Path string
	// Created is whether this drive made the tree rather than finding one a
	// previous drive left for the same item. Setup runs only on a new tree.
	Created bool
	lease   *lease.Handle
}

// treesDir is where drive keeps item worktrees: $XDG_STATE_HOME/drover/worktrees,
// keyed below by project root so two checkouts of one project never share one.
// State rather than cache, because a parked tree may hold the only copy of
// uncommitted work.
func treesDir() (string, error) {
	return stateDir("worktrees")
}

// stateDir is name under $XDG_STATE_HOME/drover, or ~/.local/state/drover.
func stateDir(name string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no home directory to keep %s under: %w", name, err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "drover", name), nil
}

// treePath is where an item's worktree lives, whether or not it exists yet.
func (s *session) treePath(it backlog.Item) (string, error) {
	dir, err := s.projectTrees()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strings.ToLower(it.ID.Stem())), nil
}

// projectTrees is the directory holding every worktree drive makes for this
// project root.
func (s *session) projectTrees() (string, error) {
	dir, err := treesDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(s.cfg.Root))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:16]), nil
}

// openItemTree checks the item's branch out into its own worktree: the one a
// previous drive left, when there is one, else a new tree started from the
// trunk, brought up to date first.
//
// git refuses to check one branch out in two trees, and that refusal is passed
// through: a branch the operator has checked out is one they are working on.
func (s *session) openItemTree(it backlog.Item, branch, trunk string) (*itemTree, error) {
	path, err := s.treePath(it)
	if err != nil {
		return nil, err
	}
	t := &itemTree{Path: path}
	switch on, registered, err := s.treeBranch(path); {
	case err != nil:
		return nil, err
	case registered && on != branch:
		return nil, fmt.Errorf("the worktree at %s holds %s, not %s: remove it with `git worktree remove %s` and drive again",
			path, on, branch, path)
	case registered:
	default:
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("%s exists and git does not list it as a worktree: remove it and drive again", path)
		}
		s.syncTrunk(trunk)
		args := []string{"worktree", "add", path, branch}
		if gitAt(s.cfg.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) != nil {
			args = []string{"worktree", "add", "-b", branch, path, trunk}
		}
		if err := gitAt(s.cfg.Root, args...); err != nil {
			return nil, fmt.Errorf("git worktree add: %w", err)
		}
		t.Created = true
	}
	if err := s.linkContext(path); err != nil {
		return t, err
	}
	if t.Created && len(s.cfg.Loop.TreeSetup) > 0 {
		if err := s.setupTree(path); err != nil {
			return t, err
		}
	}
	h, err := lease.Acquire(filepath.Join(gitDirOf(path), lease.FileName), lease.Lease{
		Command: "drive", Item: it.ID.Bare(), Branch: branch, Root: path,
	})
	if err != nil {
		return t, leaseRefusal(err)
	}
	t.lease = h
	return t, nil
}

// syncTrunk fast-forwards the local trunk to origin's, without switching any
// checkout. The local ref, not origin's, is what every later comparison reads
// (commits past the trunk, the files a branch changed, the merge base), so an
// item branched from anything else would count the remote's newer commits as
// its own.
//
// git will not move a branch checked out somewhere under that checkout's feet,
// so when the operator is standing on the trunk it is pulled there instead,
// which is what `git pull` would do and carries nothing between branches. A
// failure is reported and the item branches from the trunk as it stands: a
// stale trunk is the same trunk every comparison reads, which keeps the item's
// diff its own. A trunk lock that never came free is the same report.
func (s *session) syncTrunk(trunk string) {
	defer s.holdTrunk()()
	release, err := s.trunkLock("drive")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  · %s not brought up to date from origin: %v\n", trunk, err)
		return
	}
	defer release()
	if err := gitAt(s.cfg.Root, "fetch", "origin", trunk); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s not brought up to date from origin: %v\n", trunk, err)
		return
	}
	// Already holding everything origin has, including a trunk ahead of it.
	if gitAt(s.cfg.Root, "merge-base", "--is-ancestor", "origin/"+trunk, trunk) == nil {
		return
	}
	args := []string{"fetch", "origin", trunk + ":" + trunk}
	if on, err := s.currentBranch(); err == nil && on == trunk {
		args = []string{"merge", "--ff-only", "origin/" + trunk}
	}
	if err := gitAt(s.cfg.Root, args...); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s not brought up to date from origin: %v\n", trunk, err)
	}
}

// close releases the tree's lease and leaves the tree where it is.
func (t *itemTree) close() {
	if t != nil && t.lease != nil {
		_ = t.lease.Release()
		t.lease = nil
	}
}

// gitDirOf is a worktree's own git dir, falling back to the tree itself so a
// lease still has somewhere to live if git will not say.
func gitDirOf(tree string) string {
	out, err := gitcmd.Command(tree, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return tree
	}
	return strings.TrimSpace(string(out))
}

// treeBranch reports whether git lists path as a worktree, and the branch it
// holds. Paths are compared after resolving symlinks, because git records the
// resolved form and a macOS temp directory is not the path it was asked for.
func (s *session) treeBranch(path string) (branch string, registered bool, err error) {
	out, err := gitcmd.Command(s.cfg.Root, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return "", false, fmt.Errorf("git worktree list: %w", err)
	}
	want := resolved(path)
	var at string
	for _, field := range strings.Split(string(out), "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			at = resolved(strings.TrimPrefix(field, "worktree "))
			if at == want {
				registered = true
			}
		case at == want && strings.HasPrefix(field, "branch "):
			branch = strings.TrimPrefix(strings.TrimPrefix(field, "branch "), "refs/heads/")
		}
	}
	return branch, registered, nil
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// contextLink is one path a worktree is given from the operator's checkout.
// Asked is whether the project named it in [drive] worktree_links, which decides
// what happens when git would not ignore it.
type contextLink struct {
	Rel   string
	Asked bool
}

// contextLinks is what a worktree is given from the operator's checkout: the
// top directory of every record path inside the root, then [drive] worktree_links.
// The records are linked even though drover itself reads them through
// DROVER_ROOT, because a session reads the same directories as files, and a
// project keeping specs and skills beside its backlog keeps them for the agent.
func (s *session) contextLinks() []contextLink {
	var out []contextLink
	add := func(rel string, asked bool) {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) || strings.HasPrefix(rel, "~") {
			return
		}
		for i := range out {
			if out[i].Rel == rel {
				out[i].Asked = out[i].Asked || asked
				return
			}
		}
		out = append(out, contextLink{Rel: rel, Asked: asked})
	}
	p := s.cfg.Paths
	for _, rel := range []string{p.Backlog, p.Ledger, p.Reports, p.Specs, p.Skills, p.Shelf} {
		if rel != "" {
			add(strings.SplitN(filepath.ToSlash(filepath.Clean(rel)), "/", 2)[0], false)
		}
	}
	for _, rel := range s.cfg.Loop.TreeLinks {
		add(rel, true)
	}
	return out
}

// linkContext symlinks each context path into the tree where the checkout did
// not produce it. A path the branch tracks is left as the branch has it.
//
// Each link has to be one git ignores, or a session's `git add -A` commits a
// symlink to the operator's checkout into the branch and the item's own dirty
// check reports it as work left behind. A link is a file to git, so a directory
// ignored as `name/` does not cover it. A record directory drover links unasked
// is skipped with a note when git would see it, since drover reaches its records
// through DROVER_ROOT either way; a path the project asked for refuses the
// item, before any session is paid for, naming the fix.
func (s *session) linkContext(tree string) error {
	return s.linkPaths(tree, s.contextLinks())
}

// relinkAsked is linkContext for the paths the project named in [drive]
// worktree_links alone, run before every session in a tree already open. The
// record directories drover links unasked were settled when the tree opened,
// and judging them again would repeat their note at every launch.
func (s *session) relinkAsked(tree string) error {
	var asked []contextLink
	for _, l := range s.contextLinks() {
		if l.Asked {
			asked = append(asked, l)
		}
	}
	return s.linkPaths(tree, asked)
}

func (s *session) linkPaths(tree string, links []contextLink) error {
	for _, l := range links {
		src := filepath.Join(s.cfg.Root, l.Rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(tree, l.Rel)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("link %s into the worktree: %w", l.Rel, err)
		}
		if gitcmd.Command(tree, "check-ignore", "--quiet", "--no-index", l.Rel).Run() == nil {
			continue
		}
		_ = os.Remove(dst)
		if !l.Asked {
			fmt.Fprintf(os.Stderr, "  · %s not linked into the worktree: git would not ignore a link there "+
				"(ignore it as %s, without the trailing slash, to give sessions a copy)\n", l.Rel, l.Rel)
			continue
		}
		return fmt.Errorf("[drive] worktree_links names %s, which git would not ignore as a link in the worktree: "+
			"ignore it without a trailing slash (a link is a file to git, so %s/ does not match it), or drop it from worktree_links",
			l.Rel, l.Rel)
	}
	return nil
}

// setupTree runs [drive] worktree_setup in a new tree.
func (s *session) setupTree(tree string) error {
	argv := s.cfg.Loop.TreeSetup
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is project config
	cmd.Dir = tree
	cmd.Env = append(os.Environ(), runner.EnvRoot+"="+s.cfg.Root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("[drive] worktree_setup failed in %s: %w\n%s", tree, err, tail(string(out), 20))
	}
	return nil
}

// removeItemTree deletes an item's worktree when it holds nothing uncommitted.
// git refuses a dirty one, and that refusal stands: it may be the only copy.
func (s *session) removeItemTree(it backlog.Item) error {
	path, err := s.treePath(it)
	if err != nil {
		return err
	}
	if _, registered, err := s.treeBranch(path); err != nil || !registered {
		return err
	}
	return gitAt(s.cfg.Root, "worktree", "remove", path)
}

// tail is the last n lines of out, so a failed setup reports its error rather
// than a package manager's progress.
func tail(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
