package cmd

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/prompts"
)

// renderBranch applies the project's branch template. It is the single place a
// branch name is derived, so run creates and finish looks for the same string.
func (s *session) renderBranch(d prompts.Data) (string, error) {
	name, err := prompts.Render("branch", s.cfg.Run.Branch, d)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(name), nil
}

// branchFor renders the branch an item's work belongs on.
func (s *session) branchFor(it backlog.Item) (string, error) {
	return s.renderBranch(s.localData(it))
}

// currentBranch is the checked-out branch, or an error naming why there is none.
func (s *session) currentBranch() (string, error) {
	if !s.cfg.Git.On() {
		return "", fmt.Errorf("[git] enabled is false — this project has no branches")
	}
	out, err := gitcmd.Command(s.cfg.Root, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("no branch checked out (detached HEAD, or not a work tree)")
	}
	return strings.TrimSpace(string(out)), nil
}

// branchOwners is the checked-out branch and every item whose branch template
// renders to it.
//
// It matches rendered names rather than parsing the branch, because run.branch
// is a template and its default renders a planned item to a bare {{ .Slug }}
// carrying no id at all. There is nothing in that string to parse, and a
// project that retemplates the branch would break any parser that tried.
//
// None and several are returned rather than raised: a shared branch is legal,
// and only the caller knows whether it can answer. Collapsing the two into one
// error is what let status report a branch two items own as owned by nobody.
func (s *session) branchOwners() (string, []backlog.Item, error) {
	branch, err := s.currentBranch()
	if err != nil {
		return "", nil, err
	}
	var hits []backlog.Item
	for _, it := range s.backlog.Items {
		// A template that will not render for one item is that item's problem,
		// surfaced by run; here it just means this item is not the match.
		if name, err := s.branchFor(it); err == nil && name == branch {
			hits = append(hits, it)
		}
	}
	return branch, hits, nil
}

// bareIDs names a set of items the way every message listing them does.
func bareIDs(items []backlog.Item) string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID.Bare()
	}
	return strings.Join(ids, ", ")
}

// currentItem is the one item whose branch is checked out, or an error naming
// what a caller can do about none and about several.
func (s *session) currentItem() (backlog.Item, error) {
	branch, hits, err := s.branchOwners()
	if err != nil {
		return backlog.Item{}, err
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return backlog.Item{}, fmt.Errorf("no item's branch renders to %q — name the id", branch)
	default:
		return backlog.Item{}, fmt.Errorf(
			"%q is the branch for %s — name the id", branch, bareIDs(hits))
	}
}

// itemArg resolves the id a command was given, falling back to the checked-out
// branch when it was given none. Standing on the branch is already a statement
// about which item you mean.
func (s *session) itemArg(args []string) (backlog.Item, error) {
	if len(args) > 0 {
		return s.item(args[0])
	}
	return s.currentItem()
}

// recentCommits is how big the last n commits on a branch were, newest first,
// or the reason there are none.
//
// Both are returned because they are different answers. A branch that changed
// nothing and a branch drover never asked about both render as an empty slice,
// and the second is the common case: post-merge the ref is gone, and a project
// with [git] enabled false has no branches at all. Reporting either as zero
// lines would be the degraded read this project's principles forbid.
//
// Commits, not rounds. Nothing correlates the two — ledger dates are
// day-granularity and an item can go round the loop several times in one day —
// so drover states the fact it has rather than the one a reader wants.
func (s *session) recentCommits(branch string) ([]prompts.Commit, string) {
	// Three is enough to read a trend and short enough that a reader takes it
	// in at a glance. It is not configurable because a project choosing its own
	// window would be choosing how convincing its own decay looks.
	const n = 3

	if !s.cfg.Git.On() {
		return nil, "[git] enabled is false, so drover did not ask git"
	}
	if branch == "" {
		return nil, "this item renders no branch name"
	}
	root, trunk := s.cfg.Root, s.trunk()
	if err := gitcmd.Command(root, "rev-parse", "--verify", "--quiet", branch+"^{commit}").Run(); err != nil {
		return nil, fmt.Sprintf("no branch %s in this tree; it may have merged and been deleted", branch)
	}
	// --no-merges so a trunk merged back into the branch is not reported as a
	// round of work. The NUL separator keeps a commit subject from ever being
	// parsed as a numstat line.
	args := append([]string{"log", "--no-merges", "-n", fmt.Sprint(n), "--format=%x00%h", "--numstat",
		branch, "--not"}, trunkRefs(root, trunk)...)
	out, err := gitcmd.Command(root, append(args, "--")...).Output()
	if err != nil {
		return nil, "git could not read this tree"
	}
	var commits []prompts.Commit
	for _, rec := range strings.Split(string(out), "\x00") {
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		if lines[0] == "" {
			continue
		}
		c := prompts.Commit{Abbrev: lines[0]}
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			// A binary file reports "-" for both counts; it changed, but not by a
			// number of lines, so it adds nothing rather than failing the read.
			c.Lines += atoiOr0(f[0]) + atoiOr0(f[1])
		}
		commits = append(commits, c)
	}
	if len(commits) == 0 {
		return nil, fmt.Sprintf("%s has no commits past %s", branch, trunk)
	}
	return commits, ""
}

func atoiOr0(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// scopeOfDiff is how many source files this branch modified and which of them
// the item body never names.
//
// The question it exists for is whether the work is still the item. An item
// written about one file can end with its branch modifying an interface and
// every implementation of it, and without this count nothing reports that: the
// reviews catch one consequence of the expansion per round.
//
// Modified only. A feature creates files no body could have named, so counting
// additions would flag every clean feature and say nothing. Tests are out
// for the same reason in reverse: a body cites the code it is about, never the
// test that does not exist yet.
//
// Naming is a substring match against the whole body rather than against the
// requirements alone, because a path cited in the prose above the list is still
// a path the contract points at. Generous on purpose: the false answer that
// costs something here is calling a file unnamed when the body mentions it.
func (s *session) scopeOfDiff(branch, body string) (modified int, unnamed []string, why string) {
	if !s.cfg.Git.On() {
		return 0, nil, "[git] enabled is false, so drover did not ask git"
	}
	if branch == "" {
		return 0, nil, "this item renders no branch name"
	}
	root, trunk := s.cfg.Root, s.trunk()
	if err := gitcmd.Command(root, "rev-parse", "--verify", "--quiet", branch+"^{commit}").Run(); err != nil {
		return 0, nil, fmt.Sprintf("no branch %s in this tree; it may have merged and been deleted", branch)
	}
	// The branch's own work, measured from where it left the trunk: the same
	// boundary BRANCH-REGRESSION already tells a reviewer to use.
	base, err := forkPoint(root, trunk, branch)
	if err != nil {
		return 0, nil, "git could not read this tree"
	}
	out, err := gitcmd.Command(root, "diff", "--name-status", base, branch, "--").Output()
	if err != nil {
		return 0, nil, "git could not read this tree"
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "M") {
			continue
		}
		// A rename reports both paths; the last is where the file is now.
		path := fields[len(fields)-1]
		if isTestPath(path) {
			continue
		}
		modified++
		if !strings.Contains(body, path) {
			unnamed = append(unnamed, path)
		}
	}
	if modified == 0 {
		return 0, nil, fmt.Sprintf("%s modifies no source file past %s", branch, trunk)
	}
	return modified, unnamed, ""
}

// isTestPath reports a file whose absence from an item body says nothing. The
// list is deliberately broad across languages: a project drover cannot classify
// is better served by a shorter list than by a body blamed for not citing its
// own test files.
func isTestPath(path string) bool {
	base := filepath.Base(path)
	switch {
	case strings.Contains(base, "_test."), strings.HasPrefix(base, "test_"),
		strings.Contains(base, ".test."), strings.Contains(base, ".spec."):
		return true
	}
	for _, dir := range []string{"test/", "tests/", "testdata/", "__tests__/"} {
		if strings.HasPrefix(path, dir) || strings.Contains(path, "/"+dir) {
			return true
		}
	}
	return false
}

// dirtyPaths is every path `git status` reports in the work tree that git does
// not ignore.
//
// Every git switch carries uncommitted changes onto the branch it lands on and
// refuses only where they conflict, so work left in the tree becomes part of
// whatever branch the switch lands on, with no error anywhere. Untracked files
// count because they ride along the same way; ignored files do not, because no
// branch carries them. The flags override the repo's own status settings,
// because status.showUntrackedFiles=no or a submodule set to ignore would
// otherwise hide the exact files a switch carries.
//
// It reads the checkout the item in hand is built in, which under drive is the
// item's own worktree and everywhere else the operator's.
func (s *session) dirtyPaths() ([]string, error) {
	dir := s.workDir()
	cmd := gitcmd.Command(dir, "status", "--porcelain", "-z",
		"--untracked-files=all", "--ignore-submodules=none")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %s", dir, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, entry[3:])
		// A rename or copy carries its source as the next field.
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
		}
	}
	return paths, nil
}

// commitsPast counts the commits branch holds that trunk does not.
//
// A branch nobody advanced sits at the trunk, and `git branch --merged` lists it
// exactly as it lists one whose commits merged. So a count of zero is the only
// thing that separates a session which did nothing surviving from one whose work
// went in, and ancestry cannot supply it after the fact.
func commitsPast(root, trunk, branch string) (int, error) {
	out, err := gitcmd.Command(root, "rev-list", "--count", trunk+".."+branch).Output()
	if err != nil {
		return 0, fmt.Errorf("count the commits %s holds past %s: %w", branch, trunk, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// pathList names up to ten paths and counts the rest, so a refusal over a build
// directory nobody ignored stays one readable line.
func pathList(paths []string) string {
	const shown = 10
	if len(paths) <= shown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:shown], ", "), len(paths)-shown)
}
