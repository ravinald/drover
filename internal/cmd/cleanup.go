package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/reports"
)

var driveCleanup, driveApply bool

// leftTree is one worktree under this project's drive tree directory.
type leftTree struct {
	path, branch string
	// item is the id the tree's directory names, empty when no item in the
	// backlog has that stem any more.
	item string
	it   *backlog.Item
	// dirty is what `git status` lists there, or why it could not say, which
	// counts as dirty: a tree nobody could read is not one to delete.
	dirty []string
	// holder is the live process holding the tree's lease, empty when none.
	holder string
}

// orphanBranch is a local branch that names an item under a branch template
// [run] branch no longer renders.
type orphanBranch struct {
	name, item string
	ahead      int
}

// leftovers is what interrupted drives left in this project: worktrees, orphan
// branches, and report stubs whose run log held no result.
type leftovers struct {
	dir      string
	trees    []leftTree
	branches []orphanBranch
	stubs    []string
}

func (t leftTree) removable() bool { return len(t.dirty) == 0 && t.holder == "" }

// findLeftovers lists what --cleanup reports. It writes nothing.
func (s *session) findLeftovers() (leftovers, error) {
	var out leftovers
	base, err := treesDir()
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256([]byte(s.cfg.Root))
	out.dir = filepath.Join(base, hex.EncodeToString(sum[:])[:16])
	if out.trees, err = s.leftTrees(out.dir); err != nil {
		return out, err
	}
	if out.branches, err = s.orphanBranches(); err != nil {
		return out, err
	}
	out.stubs, err = s.reportStubs()
	return out, err
}

// leftTrees is every worktree git lists under dir, the drive tree directory
// for this project's root.
func (s *session) leftTrees(dir string) ([]leftTree, error) {
	raw, err := gitcmd.Command(s.cfg.Root, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	under := resolved(dir) + string(os.PathSeparator)
	var trees []leftTree
	var cur *leftTree
	for _, field := range strings.Split(string(raw), "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			cur = nil
			path := resolved(strings.TrimPrefix(field, "worktree "))
			if strings.HasPrefix(path, under) {
				trees = append(trees, leftTree{path: path})
				cur = &trees[len(trees)-1]
			}
		case cur != nil && strings.HasPrefix(field, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(field, "branch "), "refs/heads/")
		}
	}
	for i := range trees {
		t := &trees[i]
		stem := filepath.Base(t.path)
		for j, it := range s.backlog.Items {
			if strings.ToLower(it.ID.Stem()) == stem {
				t.item, t.it = it.ID.Bare(), &s.backlog.Items[j]
				break
			}
		}
		if st, err := gitcmd.Command(t.path, "status", "--porcelain").Output(); err != nil {
			t.dirty = []string{"git status would not read it: " + err.Error()}
		} else {
			for _, line := range strings.Split(strings.TrimRight(string(st), "\n"), "\n") {
				if len(line) > 3 {
					t.dirty = append(t.dirty, line[3:])
				}
			}
		}
		l, busy, err := lease.Read(filepath.Join(gitDirOf(t.path), lease.FileName))
		switch {
		case err != nil:
			t.holder = "unknown: the lease would not read (" + err.Error() + ")"
		case busy && l.PID == 0:
			t.holder = "a drover process that has not named itself"
		case busy:
			t.holder = fmt.Sprintf("drover %s pid %d", l.Command, l.PID)
		}
	}
	return trees, nil
}

// orphanBranches is every local branch whose last path part is an item's slug,
// alone or after a prefix and a hyphen, and is not the branch [run] branch
// renders for any item now. The slug is matched because a template since
// changed cannot be rendered again, and every template drover has shipped
// carries it.
func (s *session) orphanBranches() ([]orphanBranch, error) {
	raw, err := gitcmd.Command(s.cfg.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w", err)
	}
	current := map[string]bool{}
	for _, it := range s.backlog.Items {
		if b, err := s.branchFor(it); err == nil {
			current[b] = true
		}
	}
	trunk := s.trunk()
	var out []orphanBranch
	for _, b := range strings.Fields(string(raw)) {
		if b == trunk || current[b] {
			continue
		}
		last := b[strings.LastIndex(b, "/")+1:]
		for _, it := range s.backlog.Items {
			if it.Slug == "" || (last != it.Slug && !strings.HasSuffix(last, "-"+it.Slug)) {
				continue
			}
			n, err := commitsPast(s.cfg.Root, trunk, b)
			if err != nil {
				return nil, err
			}
			out = append(out, orphanBranch{name: b, item: it.ID.Bare(), ahead: n})
			break
		}
	}
	return out, nil
}

// reportStubs is every saved report whose self-report is the marker a run
// log with no result event leaves.
func (s *session) reportStubs() ([]string, error) {
	dir := s.cfg.Abs(s.cfg.Paths.Reports)
	var out []string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if e.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path) // #nosec G304 -- under the project's own reports directory
		if err == nil && strings.Contains(string(b), reports.NoResult) {
			out = append(out, rel(s.cfg.Root, path))
		}
		return nil
	})
	return out, err
}

// write prints the listing --cleanup reports.
func (l leftovers) write(w io.Writer) {
	fmt.Fprintf(w, "Worktrees under %s:\n", l.dir)
	if len(l.trees) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, t := range l.trees {
		item := t.item
		if item == "" {
			item = "(no item)"
		}
		state := "clean"
		if len(t.dirty) > 0 {
			state = fmt.Sprintf("uncommitted work: %s", pathList(t.dirty))
		}
		holder := "held by no drive"
		if t.holder != "" {
			holder = "held by " + t.holder
		}
		fmt.Fprintf(w, "  %s  %s  branch %s  %s  %s\n", item, t.path, orNone(t.branch), state, holder)
	}
	fmt.Fprintln(w, "Branches named for an item under a branch template [run] branch no longer renders:")
	if len(l.branches) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, b := range l.branches {
		state := "empty"
		if b.ahead > 0 {
			state = fmt.Sprintf("%d commit(s) past the trunk, kept", b.ahead)
		}
		fmt.Fprintf(w, "  %s  %s  %s\n", b.item, b.name, state)
	}
	fmt.Fprintln(w, "Report stubs with no result:")
	if len(l.stubs) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, p := range l.stubs {
		fmt.Fprintf(w, "  %s\n", p)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// apply removes the clean worktrees no drive holds and the orphan branches
// holding no commit past the trunk. git refuses a dirty tree on its own, so the
// check above it is the report and the refusal is the guarantee.
func (l leftovers) apply(s *session, w io.Writer) error {
	var failed []string
	for _, t := range l.trees {
		if !t.removable() {
			why := "uncommitted work"
			if t.holder != "" {
				why = "held by " + t.holder
			}
			fmt.Fprintf(w, "kept worktree %s: %s\n", t.path, why)
			continue
		}
		var err error
		if t.it != nil {
			err = s.removeItemTree(*t.it)
		} else {
			err = gitAt(s.cfg.Root, "worktree", "remove", t.path)
		}
		if err != nil {
			failed = append(failed, t.path+": "+err.Error())
			continue
		}
		fmt.Fprintf(w, "removed worktree %s\n", t.path)
	}
	for _, b := range l.branches {
		if b.ahead > 0 {
			continue
		}
		if err := gitAt(s.cfg.Root, "branch", "-D", b.name); err != nil {
			failed = append(failed, b.name+": "+err.Error())
			continue
		}
		fmt.Fprintf(w, "deleted branch %s\n", b.name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("--cleanup --apply left %d in place:\n  %s", len(failed), strings.Join(failed, "\n  "))
	}
	return nil
}

// cleanupDrive is --cleanup, and --cleanup --apply.
func (s *session) cleanupDrive(w io.Writer, apply bool) error {
	l, err := s.findLeftovers()
	if err != nil {
		return err
	}
	l.write(w)
	if !apply {
		return nil
	}
	return l.apply(s, w)
}
