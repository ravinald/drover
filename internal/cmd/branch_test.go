package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
)

// git runs one command against a fixture repository. The identity and the
// signing setting are supplied per invocation because the fixtures deliberately
// see no global config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-C", dir,
		"-c", "user.name=drover-test",
		"-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)
	out, err := exec.Command("git", full...).CombinedOutput() // #nosec G204 -- test fixture
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolate detaches the process from the host's git configuration. A user-level
// init.defaultBranch, hooksPath or commit template otherwise decides what these
// tests are measuring.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	identify(t)
}

// identify gives the commits drover makes itself, such as drive's trunk
// catch-up merge, an author. git() supplies one per invocation, but drover's
// own git calls see only the environment, and a host with no user.email and a
// hostname without a dot (a CI runner) leaves git nothing to guess from.
func identify(t *testing.T) {
	t.Helper()
	for _, who := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+who+"_NAME", "drover-test")
		t.Setenv("GIT_"+who+"_EMAIL", "test@example.invalid")
	}
}

// newRepo is a repository with one commit on main, so there is something to
// switch away from.
func newRepo(t *testing.T) string {
	t.Helper()
	isolate(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	return dir
}

func headBranch(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "symbolic-ref", "--short", "HEAD")
}

// switchBranch decides which branch an agent's work lands on, and it reads
// git's prose to tell "the branch is already there" from a real failure. Prose
// changes between git releases with nothing reporting it, so each state it has
// to survive is pinned to a checked-out branch or to an error a reader can act
// on.
func TestSwitchBranch(t *testing.T) {
	for _, c := range []struct {
		name     string
		noRepo   bool
		setup    func(t *testing.T, dir string)
		branch   string
		start    string
		want     string
		wantBase string
		wantKeep string
		wantErr  string
	}{
		{
			name:   "branch does not exist",
			branch: "b01-new",
			want:   "b01-new",
		},
		{
			// An item sent back returns to its own branch and pushes to the same
			// pull request, so the start point applies to the create alone.
			// `git switch -C` would reset the branch onto it and take a
			// correction pass's commits with it.
			name: "branch already exists and keeps its commits",
			setup: func(t *testing.T, dir string) {
				git(t, dir, "switch", "-q", "-c", "b01-old")
				git(t, dir, "commit", "-q", "--allow-empty", "-m", "work from the last round")
				git(t, dir, "switch", "-q", "main")
			},
			branch:   "b01-old",
			want:     "b01-old",
			wantKeep: "work from the last round",
		},
		{
			// The state the whole function was written for: discarding
			// stderr here would run the agent on whatever commit happened
			// to be out. The start point settles the second half of that: a
			// detached HEAD does not decide the base either.
			name:     "detached HEAD",
			setup:    func(t *testing.T, dir string) { git(t, dir, "switch", "--detach", "-q") },
			branch:   "b01-detached",
			want:     "b01-detached",
			wantBase: "main",
		},
		{
			// The reason the start point exists. An operator ran drive while
			// standing on an unmerged feature branch, and every item in the
			// queue was based on it: the items carried commits they never
			// touched, and it surfaced hours later as a conflict in files the
			// item never named.
			name: "created from the trunk, not from HEAD",
			setup: func(t *testing.T, dir string) {
				git(t, dir, "switch", "-q", "-c", "someones-feature")
				git(t, dir, "commit", "-q", "--allow-empty", "-m", "not mine")
			},
			branch:   "b01-from-trunk",
			want:     "b01-from-trunk",
			wantBase: "main",
		},
		{
			name:    "not a git repository",
			noRepo:  true,
			branch:  "b01-nowhere",
			wantErr: "not a git repository",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := ""
			if c.noRepo {
				isolate(t)
				dir = t.TempDir()
			} else {
				dir = newRepo(t)
			}
			if c.setup != nil {
				c.setup(t, dir)
			}

			start := c.start
			if start == "" {
				start = "main"
			}
			err := switchBranch(dir, c.branch, start)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("switchBranch = %v, want an error naming %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("switchBranch: %v", err)
			}
			if got := headBranch(t, dir); got != c.want {
				t.Errorf("checked out %q, want %q", got, c.want)
			}
			if c.wantKeep != "" {
				if got := git(t, dir, "log", "-1", "--format=%s"); got != c.wantKeep {
					t.Errorf("branch tip is %q, want %q: the existing branch was reset onto the start point", got, c.wantKeep)
				}
			}
			if c.wantBase != "" {
				// Same commit as the trunk tip means nothing from the branch
				// the operator was standing on came along.
				base := git(t, dir, "rev-parse", "HEAD")
				trunk := git(t, dir, "rev-parse", c.wantBase)
				if base != trunk {
					t.Errorf("branched from %s, want the %s tip %s: the checkout decided the base",
						base, c.wantBase, trunk)
				}
			}
		})
	}
}

// A branch template may reach every field the implement prompt sees, .Issues among them.
// Rendering the name after the register is attached makes it depend on whether
// gh answered, so run would create one branch while gh was reachable and finish
// would look for another while it was not. Both states have to render the same
// string, which is what pinning promptData's branch to branchFor buys.
func TestBranchDoesNotMoveWithTheRegister(t *testing.T) {
	it := backlog.Item{
		ID:   backlog.ID{Prefix: "B", Num: 1},
		Slug: "finish-refuses-after-done",
		Meta: map[string]string{"title": "finish refuses after done"},
	}
	s := &session{
		cfg: &config.Config{
			Project: "drover",
			Run:     config.Run{Branch: "{{ lower .Stem }}-{{ .Slug }}{{ range .FollowUps }}-{{ .Number }}{{ end }}"},
		},
		ledger: &ledger.Ledger{},
	}

	want, err := s.branchFor(it)
	if err != nil {
		t.Fatalf("branchFor: %v", err)
	}

	routed := gh.Index{OK: true, Issues: []gh.Issue{
		{Number: 42, Title: "an owned row", Owners: []string{"B1"}},
	}}
	for _, c := range []struct {
		name  string
		index gh.Index
	}{
		{"gh unreachable", gh.Index{}},
		{"gh answers with an owned issue", routed},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := s.promptDataWith(it, c.index)
			if err != nil {
				t.Fatalf("promptDataWith: %v", err)
			}
			if d.Branch != want {
				t.Errorf("branch = %q, want %q — finish renders the latter and would not find the former",
					d.Branch, want)
			}
		})
	}

	// The fixture proves nothing unless the register reaches the template, so
	// confirm the issue arrived rather than trusting the routing.
	d, err := s.promptDataWith(it, routed)
	if err != nil {
		t.Fatalf("promptDataWith: %v", err)
	}
	if len(d.FollowUps) != 1 {
		t.Fatalf("template saw %d rows, want 1 — the fixture routes nothing", len(d.FollowUps))
	}
}

// sharedBranchRepo is a project that is also a git repository, and whose two
// planned bodies share a slug — so the default template renders both to the
// branch "shared". A collision is legal: the template belongs to the project,
// and nothing in drover forbids two items working on one branch. It is also
// the state finish refuses by name, which status has to match.
func sharedBranchRepo(t *testing.T) *session {
	t.Helper()
	dir := shelfProject(t, map[string]string{
		"01-shared.md": body("First", ""),
		"02-shared.md": body("Second", ""),
		"B01-alone.md": body("Alone", ""),
	}, nil)
	isolate(t)
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	return openIn(t, dir)
}

// onBranch checks out a branch in the session's tree, creating it if needed.
func onBranch(t *testing.T, s *session, branch string) {
	t.Helper()
	if branch == headBranch(t, s.cfg.Root) {
		return
	}
	git(t, s.cfg.Root, "switch", "-q", "-C", branch)
}

// Zero and several are different answers, and a caller that can only read the
// text of one error cannot act on either. status wants the candidate ids;
// collapsing them is what let it report a branch two items own as owned by
// nobody.
func TestBranchOwnersSeparatesNoneFromSeveral(t *testing.T) {
	s := sharedBranchRepo(t)
	for _, c := range []struct {
		branch string
		want   string
	}{
		{branch: "main", want: ""},
		{branch: "b01-alone", want: "B1"},
		{branch: "shared", want: "1, 2"},
	} {
		t.Run(c.branch, func(t *testing.T) {
			onBranch(t, s, c.branch)
			branch, owners, err := s.branchOwners()
			if err != nil {
				t.Fatalf("branchOwners: %v", err)
			}
			if branch != c.branch {
				t.Errorf("branch = %q, want %q", branch, c.branch)
			}
			if got := bareIDs(owners); got != c.want {
				t.Errorf("owners = %q, want %q", got, c.want)
			}
		})
	}
}

// The errors are what finish prints, so each has to name the branch and say what
// to do about it. The two-hit message names the candidates: a refusal that
// withholds the two ids it just compared makes the operator go and find them.
func TestCurrentItemAtZeroOneAndMany(t *testing.T) {
	s := sharedBranchRepo(t)
	for _, c := range []struct {
		name    string
		branch  string
		want    string
		wantErr []string
	}{
		{
			name:    "no item renders to it",
			branch:  "main",
			wantErr: []string{`no item's branch renders to "main"`, "name the id"},
		},
		{name: "exactly one owns it", branch: "b01-alone", want: "B1"},
		{
			name:    "several own it",
			branch:  "shared",
			wantErr: []string{`"shared" is the branch for 1, 2`, "name the id"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			onBranch(t, s, c.branch)
			it, err := s.currentItem()
			if c.want != "" {
				if err != nil {
					t.Fatalf("currentItem: %v", err)
				}
				if got := it.ID.Bare(); got != c.want {
					t.Errorf("currentItem = %q, want %q", got, c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("currentItem resolved %q on %s", it.ID.Bare(), c.branch)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}
}

// Standing on a branch is a statement about which item you mean, and it is the
// only statement finish gets when it is called with no id.
func TestItemArgFallsBackToTheBranch(t *testing.T) {
	s := sharedBranchRepo(t)

	onBranch(t, s, "main")
	if it, err := s.itemArg([]string{"B1"}); err != nil || it.ID.Bare() != "B1" {
		t.Errorf("itemArg([B1]) = %q, %v; want B1 off the argument, not the branch", it.ID.Bare(), err)
	}

	onBranch(t, s, "b01-alone")
	it, err := s.itemArg(nil)
	if err != nil {
		t.Fatalf("itemArg on b01-alone: %v", err)
	}
	if it.ID.Bare() != "B1" {
		t.Errorf("itemArg resolved %q, want B1", it.ID.Bare())
	}

	onBranch(t, s, "shared")
	if it, err := s.itemArg(nil); err == nil {
		t.Errorf("itemArg picked %q on a branch two items own", it.ID.Bare())
	} else if !strings.Contains(err.Error(), "1, 2") {
		t.Errorf("error %q does not name the candidates", err)
	}
}

// The path an operator meets: finish with no id on a shared branch. It has to
// refuse by name rather than finish whichever item sorted first, and leave the
// tree and the ledger alone doing it.
func TestFinishWithNoIDRefusesASharedBranch(t *testing.T) {
	s := sharedBranchRepo(t)
	dir := s.cfg.Root
	onBranch(t, s, "shared")
	stubPATH(t)
	t.Chdir(dir)

	err := execute(t, "finish")
	if err == nil {
		t.Fatal("finish picked an item on a branch two items own")
	}
	for _, want := range []string{`"shared" is the branch for 1, 2`, "name the id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a refusal wrote to the ledger: %v", err)
	}
	if got := headBranch(t, dir); got != "shared" {
		t.Errorf("a refusal moved the tree to %q", got)
	}
}

// kindOf reads the item first and the namespace second, because an item may
// carry a kind its prefix does not imply: intake writes one from the tracker's
// label, and a planned item has no prefix to imply anything.
func TestKindOfPrefersTheItemThenTheNamespace(t *testing.T) {
	namespaces := map[string]string{"B": "bug", "E": "enhancement", "F": "feature"}
	for _, c := range []struct {
		name string
		it   backlog.Item
		want string
	}{
		{"the item's own kind wins",
			backlog.Item{ID: backlog.ID{Prefix: "B", Num: 1}, Meta: map[string]string{"kind": "regression"}},
			"regression"},
		{"the namespace answers when the item does not",
			backlog.Item{ID: backlog.ID{Prefix: "F", Num: 3}, Meta: map[string]string{}},
			"feature"},
		{"whitespace is not a kind",
			backlog.Item{ID: backlog.ID{Prefix: "E", Num: 2}, Meta: map[string]string{"kind": "  "}},
			"enhancement"},
		{"a planned item in a project declaring none has no kind",
			backlog.Item{ID: backlog.ID{Num: 7}, Meta: map[string]string{}},
			""},
		{"an unmapped prefix has no kind",
			backlog.Item{ID: backlog.ID{Prefix: "Z", Num: 1}, Meta: map[string]string{}},
			""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &session{cfg: &config.Config{IDs: config.IDs{Namespaces: namespaces}}}
			if got := s.kindOf(c.it); got != c.want {
				t.Errorf("kindOf = %q, want %q", got, c.want)
			}
		})
	}
}

// The kind segment is written with `with` rather than `if` on purpose. An item
// carrying no kind has to render the plainer name: a branch template that errors
// drops the item out of branchOwners without a word (branch.go:59-61), while run
// and finish on the same id refuse with the template error, so `drover status`
// reports no item owns a branch the item is standing on.
func TestAnItemWithNoKindStillRendersABranch(t *testing.T) {
	const tmpl = "{{ with .Kind }}{{ . }}/{{ end }}" +
		"{{ if .Planned }}{{ .Slug }}{{ else }}{{ lower .Stem }}-{{ .Slug }}{{ end }}"
	for _, c := range []struct {
		name       string
		namespaces map[string]string
		it         backlog.Item
		want       string
	}{
		{"a kinded item takes the prefix",
			map[string]string{"B": "bug"},
			backlog.Item{ID: backlog.ID{Prefix: "B", Num: 1}, Slug: "finish-refuses-after-done"},
			"bug/b01-finish-refuses-after-done"},
		{"a project declaring no namespaces renders today's name",
			map[string]string{},
			backlog.Item{ID: backlog.ID{Prefix: "B", Num: 1}, Slug: "finish-refuses-after-done"},
			"b01-finish-refuses-after-done"},
		{"a planned item keeps its bare slug",
			map[string]string{"B": "bug"},
			backlog.Item{ID: backlog.ID{Num: 4}, Slug: "status-hides-merged"},
			"status-hides-merged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &session{
				cfg: &config.Config{
					Project: "drover",
					IDs:     config.IDs{Namespaces: c.namespaces},
					Run:     config.Run{Branch: tmpl},
				},
				ledger: &ledger.Ledger{},
			}
			got, err := s.branchFor(c.it)
			if err != nil {
				t.Fatalf("branchFor: %v", err)
			}
			if got != c.want {
				t.Errorf("branch = %q, want %q", got, c.want)
			}
		})
	}
}

// git reads GIT_DIR, GIT_WORK_TREE and their relatives from the environment
// in preference to the repository it was pointed at, so an exported variable
// redirects a command that names its own root with -C. A call site inheriting
// the caller's environment can misread which branch is checked out, whether a
// branch merged, what the trunk is, and whether a record write would dirty the
// tree.
//
// It fails on a call site added later rather than on one somebody reviewed.
func TestEveryGitCallGoesThroughTheHelper(t *testing.T) {
	root := repoRoot(t)
	var stray []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The helper is the one place allowed to run git itself.
		if filepath.Base(filepath.Dir(path)) == "gitcmd" {
			return nil
		}
		b, err := os.ReadFile(path) // #nosec G304 -- a path walked from the repo root
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, `exec.Command("git"`) || strings.Contains(line, `"git",`) &&
				strings.Contains(line, "exec.CommandContext") {
				stray = append(stray, fmt.Sprintf("%s:%d", mustRel(root, path), i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) > 0 {
		t.Errorf("git invoked outside internal/gitcmd, so these inherit a redirecting environment:\n  %s",
			strings.Join(stray, "\n  "))
	}
}

func mustRel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}
