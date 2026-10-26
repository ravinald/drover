package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// stubPATH replaces PATH with a directory holding nothing but a link to the
// real git. Whether gh exists is then this test's decision rather than the
// host's, and it is returned so a caller can drop a stub beside it.
func stubPATH(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not on PATH: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(bin, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir
}

// stubGH answers the two calls gh.MergedPR makes: the availability probe, then
// the query. Anything else exits non-zero, so a new call site fails here rather
// than reaching the network.
func stubGH(t *testing.T, dir, prList string) {
	t.Helper()
	t.Setenv("DROVER_TEST_GH_PRS", prList)
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'pr list') printf '%s' \"$DROVER_TEST_GH_PRS\" ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	stub(t, filepath.Join(dir, "gh"), script)
}

// thisRepo is captured at package init, before any t.Chdir, from the package
// directory the test binary starts in. repoRoot() cannot serve: it walks up for
// a go.mod, and the fixtures write one.
var thisRepo = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}()

// drover walks up for .drover.toml, so a command run from an unfixtured cwd
// resolves to this repository's own project: `intake 1` would label an issue on
// the live tracker and `finish 1` would fast-forward the tree the test is running
// in. Every such invocation is one regressed gate away from doing it, and the
// verify is the thing these tests exist to assert. Refuse the cwd instead of
// trusting the code under test.
func offRepo(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == thisRepo || strings.HasPrefix(wd, thisRepo+string(os.PathSeparator)) {
		t.Fatalf("cwd is %s, inside %s: drover would resolve this repository's own project. Stand up a fixture first.", wd, thisRepo)
	}
}

// execute drives one command through the real root. The exit code lives in the
// error RunE returns, so nothing short of this pins a refusal.
//
// The flags a call parsed are put back when the next call starts or the test
// ends, not when Execute returns, because a test reads what a flag wrote after
// the call it passed it to.
func execute(t *testing.T, args ...string) error {
	t.Helper()
	offRepo(t)
	install()
	resetFlags(root)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	t.Cleanup(func() {
		root.SetArgs(nil)
		resetFlags(root)
	})
	return root.Execute()
}

// resetFlags puts every flag Execute parsed under c back to its default. cobra
// binds each flag to a package variable and never resets one between calls, so
// without this a flag one test passes is set for every test after it.
func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		// Set appends to a slice flag rather than replacing it.
		if s, ok := f.Value.(pflag.SliceValue); ok {
			var def []string
			if d := strings.Trim(f.DefValue, "[]"); d != "" {
				def = strings.Split(d, ",")
			}
			_ = s.Replace(def)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	c.PersistentFlags().VisitAll(reset)
	c.Flags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

func TestTrunkPrefersConfigThenOriginThenMain(t *testing.T) {
	for _, c := range []struct {
		name   string
		trunk  string
		origin string
		want   string
	}{
		{name: "configured wins", trunk: "release", origin: "origin/develop", want: "release"},
		{name: "origin answers when unset", origin: "origin/develop", want: "develop"},
		{name: "main when nothing answers", want: "main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			if c.origin != "" {
				git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD",
					"refs/remotes/"+c.origin)
			}
			s := &session{cfg: &config.Config{Root: dir}}
			s.cfg.Run.Trunk = c.trunk
			if got := s.trunk(); got != c.want {
				t.Errorf("trunk = %q, want %q", got, c.want)
			}
		})
	}
}

// mergedRepo is a trunk plus two branches: one merged into it, one not.
func mergedRepo(t *testing.T) string {
	t.Helper()
	dir := newRepo(t)
	git(t, dir, "switch", "-q", "-c", "finished")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "finished work")
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge finished", "finished")
	git(t, dir, "switch", "-q", "-c", "pending")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "pending work")
	git(t, dir, "switch", "-q", "main")
	// A branch drover created and nobody committed to, which is the case ancestry
	// answers wrongly: it sits at the trunk, so `git branch --merged` lists it.
	git(t, dir, "branch", "never-started", "main")
	return dir
}

// recordedHead appends the row drover writes before a push, for a fixture that
// merges a branch by hand. It goes in before the merge: afterwards the commit is
// the trunk's too, and nothing could tell this branch from one that never moved.
func recordedHead(t *testing.T, dir, id, branch string) {
	t.Helper()
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Row{
		ID: id, Branch: branch, Head: git(t, dir, "rev-parse", branch), Date: ledger.Today(),
	}); err != nil {
		t.Fatal(err)
	}
}

// headLedger is the rows drover writes before each push: one per branch it saw
// holding a commit the trunk did not. An ancestry yes stands only for a branch
// one of these vouches for, so a fixture asserting a merge needs them.
func headLedger(t *testing.T, dir string, branches ...string) *ledger.Ledger {
	t.Helper()
	l := &ledger.Ledger{}
	for _, b := range branches {
		l.Rows = append(l.Rows, ledger.Row{
			ID: "1", Branch: b, Head: git(t, dir, "rev-parse", b), Date: ledger.Today(),
		})
	}
	return l
}

// stubMergeCheck writes a [git] merge_check that records its arguments and
// exits with the given status. The recording is what pins the branch-and-trunk
// contract: a check that exits non-zero because it was handed the wrong
// arguments still reads as "not merged", so the exit status alone cannot show
// it was called correctly.
func stubMergeCheck(t *testing.T, dir string, exit int) (argv []string, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "argv")
	t.Setenv("DROVER_TEST_MERGE_CHECK_LOG", log)
	t.Setenv("DROVER_TEST_MERGE_CHECK_EXIT", strconv.Itoa(exit))
	stub(t, filepath.Join(dir, "merge-check"),
		"#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DROVER_TEST_MERGE_CHECK_LOG\"\nexit \"$DROVER_TEST_MERGE_CHECK_EXIT\"\n")
	return []string{"merge-check"}, log
}

// The matrix finish's merge check actually has to walk: which authority is configured,
// what it answered, and what ancestry says underneath it. Ancestry is consulted
// on a definitive no as well as on an outage — a branch merged by direct push
// to the trunk leaves no pull request behind — and every cell has to name the
// source that produced it, because that is what a wrong answer is debugged
// with.
func TestMergedIntoWalksBothAuthorities(t *testing.T) {
	const mergedPR = `[{"number":42,"mergedAt":"2026-02-01T00:00:00Z"}]`

	for _, c := range []struct {
		name   string
		branch string // landed: ancestry says merged. pending: ancestry says no.
		prList string // "" leaves gh off PATH entirely
		check  bool   // declare [git] merge_check
		exit   int    // its exit status
		merged bool
		// unvouched marks the no that must not look like every other no: ancestry
		// says merged and nothing shows the branch ever holding a commit.
		unvouched bool
		how       string
	}{
		{
			name:   "gh says merged, ancestry agrees",
			branch: "finished", prList: mergedPR,
			merged: true, how: "PR #42",
		},
		{
			name:   "squash merge: gh says merged, ancestry says no",
			branch: "pending", prList: mergedPR,
			merged: true, how: "PR #42",
		},
		{
			name:   "direct push to trunk: gh definitive no, ancestry says merged",
			branch: "finished", prList: `[]`,
			merged: true, how: "no merged PR for this branch; git ancestry says merged",
		},
		{
			name:   "gh definitive no, ancestry says no",
			branch: "pending", prList: `[]`,
			merged: false, how: "no merged PR for this branch; git ancestry says no, which a squash merge would also say",
		},
		{
			name:   "gh unavailable, ancestry says merged",
			branch: "finished",
			merged: true, how: "gh unavailable; git ancestry says merged",
		},
		{
			name:   "gh unavailable, ancestry says no",
			branch: "pending",
			merged: false, how: "gh unavailable; git ancestry says no, which a squash merge would also say",
		},
		{
			name:   "merge_check says merged, ancestry agrees",
			branch: "finished", check: true,
			merged: true, how: "[git] merge_check says merged",
		},
		{
			name:   "squash merge off GitHub: merge_check says merged, ancestry says no",
			branch: "pending", check: true,
			merged: true, how: "[git] merge_check says merged",
		},
		{
			name:   "merge_check says no, ancestry says merged",
			branch: "finished", check: true, exit: 1,
			merged: true, how: "[git] merge_check says no; git ancestry says merged",
		},
		{
			name:   "merge_check says no, ancestry says no",
			branch: "pending", check: true, exit: 1,
			merged: false, how: "[git] merge_check says no; git ancestry says no, which a squash merge would also say",
		},
		{
			name:   "merge_check answers instead of gh, not after it",
			branch: "pending", prList: mergedPR, check: true, exit: 1,
			merged: false, how: "[git] merge_check says no; git ancestry says no, which a squash merge would also say",
		},
		// The run committed nothing, the branch stayed at the trunk, and
		// ancestry calls it merged: recording a completion would claim work
		// that is not there.
		{
			name:   "a branch that never held a commit: ancestry says merged, no row vouches for it",
			branch: "never-started", prList: `[]`,
			merged: false, unvouched: true,
			how: "no merged PR for this branch; git ancestry says merged, " +
				"but no row shows the branch holding commits of its own",
		},
		{
			name:   "a fast-forwarded branch that did carry work is still merged",
			branch: "ffwd", prList: `[]`,
			merged: true, how: "no merged PR for this branch; git ancestry says merged",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := mergedRepo(t)
			bin := stubPATH(t)
			if c.prList != "" {
				stubGH(t, bin, c.prList)
			}
			// ffwd is built here rather than in mergedRepo: the head row has to name
			// the commit the branch carried, and the fast-forward that follows makes
			// that commit the trunk's too, which is what carriedWork has to see past.
			git(t, dir, "switch", "-q", "-c", "ffwd")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "ffwd work")
			s := &session{cfg: &config.Config{Root: dir},
				ledger: headLedger(t, dir, "finished", "pending", "ffwd")}
			git(t, dir, "switch", "-q", "main")
			git(t, dir, "merge", "-q", "--ff-only", "ffwd")
			if c.check {
				s.cfg.Git.MergeCheck, _ = stubMergeCheck(t, bin, c.exit)
			}

			answer, how, err := s.mergedInto(c.branch, "main")
			if err != nil {
				t.Fatalf("mergedInto: %v", err)
			}
			if (answer == mergeYes) != c.merged || how != c.how {
				t.Errorf("mergedInto(%s) = %v, %q; want merged=%v, %q", c.branch, answer, how, c.merged, c.how)
			}
			if c.unvouched && answer != mergeUnvouched {
				t.Errorf("mergedInto(%s) answered %v, want mergeUnvouched", c.branch, answer)
			}
		})
	}
}

// The check is handed the branch and the trunk, in that order, as its last two
// arguments. Anything else and a correct check answers about the wrong branch.
func TestMergeCheckReceivesBranchAndTrunk(t *testing.T) {
	dir := mergedRepo(t)
	argv, log := stubMergeCheck(t, stubPATH(t), 0)
	s := &session{cfg: &config.Config{Root: dir}}
	s.cfg.Git.MergeCheck = append(argv, "--merged")

	if _, _, err := s.mergedInto("pending", "main"); err != nil {
		t.Fatalf("mergedInto: %v", err)
	}
	got, err := os.ReadFile(log) // #nosec G304 -- written by this test
	if err != nil {
		t.Fatalf("merge_check was never run: %v", err)
	}
	if want := "--merged\npending\nmain\n"; string(got) != want {
		t.Errorf("merge_check argv = %q, want %q", got, want)
	}
}

// A merge_check nothing can run is a misconfiguration, and answering "not
// merged" to it would turn a typo into a refusal whose message names the branch
// rather than the config.
func TestMergeCheckThatCannotRunIsAnError(t *testing.T) {
	dir := mergedRepo(t)
	stubPATH(t)
	s := &session{cfg: &config.Config{Root: dir}}
	s.cfg.Git.MergeCheck = []string{"no-such-merge-check"}

	_, _, err := s.mergedInto("finished", "main")
	if err == nil {
		t.Fatal("mergedInto accepted a merge_check that is not on PATH")
	}
	if !strings.Contains(err.Error(), "merge_check") {
		t.Errorf("error does not name the config key: %v", err)
	}
}

// The refusal is the path users meet. It exits 3 rather than 1 so a wrapper can
// tell "drover said no" from "drover could not answer", and it must leave the
// ledger alone: recording a completion for work still on a branch makes the
// ledger claim something the tree does not support.
func TestFinishRefusesAnUnmergedBranch(t *testing.T) {
	s := fixture(t, "")
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, dir, "switch", "-q", "-c", "first")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	git(t, dir, "switch", "-q", "main")
	stubPATH(t)
	t.Chdir(dir)

	err := execute(t, "finish", "1")
	if got := Code(err); got != ExitRefused {
		t.Fatalf("finish exited %d (%v), want %d", got, err, ExitRefused)
	}
	if !strings.Contains(err.Error(), "does not look merged") {
		t.Errorf("refusal does not say what it refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a refusal wrote to the ledger: %v", err)
	}
	if got := headBranch(t, dir); got != "main" {
		t.Errorf("a refusal moved the tree to %q", got)
	}
}

// The declaration scopes to the issue register. A project with no tracker still
// merges pull requests, and gh is the only thing that can see a squash merge —
// letting it reach MergedPR would report every merged branch unmerged forever.
func TestNoTrackerLeavesMergeDetectionAlone(t *testing.T) {
	dir := mergedRepo(t)
	stubGH(t, stubPATH(t), `[{"number":42,"mergedAt":"2026-02-01T00:00:00Z"}]`)
	s := &session{cfg: &config.Config{Root: dir}}
	s.cfg.Register.Backend = config.BackendNone

	answer, how, err := s.mergedInto("pending", "main")
	if err != nil {
		t.Fatalf("mergedInto: %v", err)
	}
	if answer != mergeYes || how != "PR #42" {
		t.Errorf("mergedInto = %v, %q; want mergeYes, \"PR #42\"", answer, how)
	}
}

// finish is the merge check and nothing else is. In a project with no work tree
// there is no branch to see merged, so it refuses by design and names the two
// commands that do finish an item — rather than shelling out and reporting
// whatever exit status git chose.
func TestFinishRefusesWithoutGit(t *testing.T) {
	s := fixture(t, "\n[git]\nenabled = false\n")
	dir := s.cfg.Root
	t.Chdir(dir)

	err := execute(t, "finish", "1")
	if got := Code(err); got != ExitRefused {
		t.Fatalf("finish exited %d (%v), want %d", got, err, ExitRefused)
	}
	for _, want := range []string{"[git] enabled is false", "drover mark-merged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a refusal wrote to the ledger: %v", err)
	}
}

// finishProject is a git repo whose B1 branch is merged into the trunk, beside
// B2, an item nobody has finished. Every register row below routes to one or
// both, so what separates a dormant row from a live one is the state of the
// owner it names and nothing else.
//
// The branch is rendered rather than spelled, because a hand-written name that
// stops matching [run] branch makes every cell here refuse instead of finishing.
func finishProject(t *testing.T, conf string, rows ...string) (dir, branch string) {
	t.Helper()
	dir = registerProject(t, conf)
	for name, title := range map[string]string{
		"B01-merged.md": "Merged owner",
		"B02-open.md":   "Open owner",
	} {
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(body(title, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if rows != nil {
		fileRegister(t, dir, rows...)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")

	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if branch, err = s.branchFor(it); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	recordedHead(t, dir, "B1", branch)
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
	return dir, branch
}

// The moment a routed follow-up goes dormant is the merge, and finish is the only
// command holding both facts. Three register contents against three states the
// read can be in: the cell that matters most is the unreachable one, which used
// to be the only honest place a zero could come from and must not print one.
func TestFinishReportsOrphanedFollowUpsAcrossEveryRegisterState(t *testing.T) {
	const (
		dormantL = `{"number":1,"title":"low, owner merged","severity":"L","owners":["B1"],"state":"open","date":"2026-01-01"}`
		dormantH = `{"number":2,"title":"high, owner merged","severity":"H","owners":["B1"],"state":"open","date":"2026-01-01"}`
		coOwned  = `{"number":3,"title":"owned by B1 and an open item","severity":"C","owners":["B1","B2"],"state":"open","date":"2026-01-01"}`
		elsewhre = `{"number":4,"title":"owned by an open item","severity":"H","owners":["B2"],"state":"open","date":"2026-01-01"}`
		malforme = `{"number":9,`
	)
	for _, rows := range []struct {
		name    string
		rows    []string
		dormant int
		worst   string
	}{
		{name: "no owned rows", rows: []string{elsewhre}},
		{name: "owned and all orphaned", rows: []string{dormantL, dormantH, elsewhre}, dormant: 2, worst: "worst H"},
		// C outranks both dormant rows and must not reach the line: an open
		// co-owner still has a run left to carry #3.
		{name: "owned with a live owner", rows: []string{dormantL, coOwned}, dormant: 1, worst: "worst L"},
	} {
		for _, reg := range []struct {
			name string
			conf string
			// extra is appended to the register file, which is how a readable
			// register becomes an unreadable one holding the same rows.
			extra   string
			read    bool
			silent  bool // the project declared no register at all
			because string
		}{
			{name: "register readable", conf: fileProject, read: true},
			{name: "register unreachable", conf: fileProject, extra: malforme, because: "is not a register row"},
			{name: "backend none", conf: noProject, silent: true},
		} {
			t.Run(rows.name+", "+reg.name, func(t *testing.T) {
				content := rows.rows
				if reg.extra != "" {
					content = append(append([]string{}, content...), reg.extra)
				}
				dir, branch := finishProject(t, reg.conf, content...)
				stubPATH(t)
				t.Setenv(runner.EnvActor, "")
				t.Chdir(dir)

				var err error
				out := capture(t, func() { err = execute(t, "finish", "B1") })
				if err != nil {
					t.Fatalf("finish: %v\n%s", err, out)
				}

				// Every finish does its own five steps whatever the register
				// said, and a missing one here is the register failing a merge
				// that already happened.
				for _, want := range []string{branch + " is merged", "ledger: merged", "on main"} {
					if !strings.Contains(out, want) {
						t.Errorf("finish wrote:\n%s\nwant it to name %q", out, want)
					}
				}

				switch {
				case reg.silent:
					for _, unwanted := range []string{"orphaned", "register", "follow-up"} {
						if strings.Contains(out, unwanted) {
							t.Errorf("finish mentioned %q on a project declaring no register:\n%s", unwanted, out)
						}
					}
				case !reg.read:
					if !strings.Contains(out, "follow-up list unread") || !strings.Contains(out, reg.because) {
						t.Errorf("finish wrote:\n%s\nwant it to say the register went unread, and why", out)
					}
					// The whole point of saying so: a count nobody could
					// compute must not appear as one nobody filed.
					if strings.Contains(out, "now orphaned") || strings.Contains(out, "owned by B1") {
						t.Errorf("finish reported a count off a register it never read:\n%s", out)
					}
				case rows.dormant == 0:
					if strings.Contains(out, "orphaned") {
						t.Errorf("finish wrote a line about nothing:\n%s", out)
					}
				default:
					for _, want := range []string{
						fmt.Sprintf("%d %s owned by B1", rows.dormant, plural(rows.dormant, "follow-up", "follow-ups")),
						"now orphaned", "(" + rows.worst + ")", "deps: B1", "drover follow-ups B1",
					} {
						if !strings.Contains(out, want) {
							t.Errorf("finish wrote:\n%s\nwant it to name %q", out, want)
						}
					}
				}
			})
		}
	}
}

// The register is the last thing finish consults and the least it may act on. A
// read that fails, that is cut short, or that never happens leaves the ledger
// row, the trunk and the exit code exactly where a working read leaves them.
func TestNoRegisterFailureReachesTheExitCode(t *testing.T) {
	for _, c := range []struct {
		name string
		conf string
		rows []string
		page string // the gh page; empty leaves no gh on PATH at all
	}{
		{
			name: "file that will not read", conf: fileProject,
			rows: []string{`{"number":1,"title":"fine","owners":["B1"],"state":"open","date":"2026-01-01"}`, `{"number":2,`},
		},
		{name: "gh unreachable", conf: ghProject},
		{name: "page cut short", conf: ghProject + "page_limit = 1\n", page: reachability},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := finishProject(t, c.conf, c.rows...)
			bin := stubPATH(t)
			if c.page != "" {
				stubGHIssues(t, bin, c.page)
			}
			t.Setenv(runner.EnvActor, "")
			t.Chdir(dir)

			var err error
			out := capture(t, func() { err = execute(t, "finish", "B1") })
			if got := Code(err); got != 0 {
				t.Fatalf("finish exited %d (%v):\n%s", got, err, out)
			}
			if !strings.Contains(out, "ledger: merged") {
				t.Errorf("the ledger row did not survive the register read:\n%s", out)
			}
			line, ferr := os.ReadFile(filepath.Join(dir, "ledger.jsonl")) // #nosec G304 -- written by this test
			if ferr != nil || !strings.Contains(string(line), `"id":"B1"`) {
				t.Errorf("ledger.jsonl does not record the finishing: %v %q", ferr, line)
			}
			if got := headBranch(t, dir); got != "main" {
				t.Errorf("finish left the tree on %q", got)
			}
		})
	}
}

// A page that stopped at the limit makes the count a floor, and check says so
// off the same index. The cell that matters is the zero: zero routed rows
// print nothing, which is a true statement about a whole read and a false one
// about a page nobody counted past.
func TestFinishSaysWhenTheOrphanedCountIsAFloor(t *testing.T) {
	const (
		routedHere = `[{"number":101,"title":"orphaned row","body":"Owner: B1","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}]`
		routedElsewhere = `[{"number":102,"title":"live row","body":"Owner: B2","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}]`
		floor = "the follow-up list hit the 1-issue page limit, so the orphaned count is a floor"
	)
	for _, c := range []struct {
		name   string
		conf   string
		page   string
		want   []string
		absent []string
	}{
		{
			name: "truncated, an owned row went orphaned",
			conf: ghProject + "page_limit = 1\n", page: routedHere,
			want: []string{"1 follow-up owned by B1 is now orphaned", floor},
		},
		{
			name: "truncated, nothing owned here",
			conf: ghProject + "page_limit = 1\n", page: routedElsewhere,
			want:   []string{"no follow-up owned by B1 counted as orphaned, and " + floor},
			absent: []string{"now orphaned"},
		},
		{
			// The only zero that prints nothing: the read was whole, so the
			// silence is a statement.
			name: "a whole read, nothing owned here",
			conf: ghProject, page: routedElsewhere,
			absent: []string{"orphaned", "page limit"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := finishProject(t, c.conf)
			stubGHIssues(t, stubPATH(t), c.page)
			t.Setenv(runner.EnvActor, "")
			t.Chdir(dir)

			var err error
			out := capture(t, func() { err = execute(t, "finish", "B1") })
			if err != nil {
				t.Fatalf("finish: %v\n%s", err, out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("finish wrote:\n%s\nwant it to say %q", out, want)
				}
			}
			for _, unwanted := range c.absent {
				if strings.Contains(out, unwanted) {
					t.Errorf("finish wrote:\n%s\nwant nothing about %q", out, unwanted)
				}
			}
		})
	}
}

// finish walks the closes key and nothing else, so without this it would merge
// past a routed issue in silence: the branch that may have fixed it merges,
// the report stays open, and nothing says the two are related.
func TestFinishNamesTheRoutedIssuesItWalkedPast(t *testing.T) {
	dir, _ := finishProject(t, fileProject,
		`{"number":259,"title":"owned by B1, unclaimed","owners":["B1"],"state":"open","date":"2026-01-01"}`,
		`{"number":400,"title":"owned elsewhere","owners":["B2"],"state":"open","date":"2026-01-01"}`,
	)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)

	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	for _, want := range []string{"#259", "closes", "drover link B1 <issue>"} {
		if !strings.Contains(out, want) {
			t.Errorf("finish wrote:\n%s\nwant it to name %q", out, want)
		}
	}
	// Somebody else's routed issue is not this finish's to report.
	if strings.Contains(out, "#400") {
		t.Errorf("finish named an issue owned by another item:\n%s", out)
	}
}

// Nothing to say is said with nothing: a project whose routed issues are all
// claimed gets the dispositions and no second line about them.
func TestFinishIsQuietWhenNothingWasWalkedPast(t *testing.T) {
	dir, _ := finishProject(t, fileProject,
		`{"number":400,"title":"owned elsewhere","owners":["B2"],"state":"open","date":"2026-01-01"}`,
	)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)

	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	if strings.Contains(out, "drover link") {
		t.Errorf("finish offered a claim with nothing owned by B1:\n%s", out)
	}
}

// Asking git to --verify two revisions at once is refused with "Needed a single
// revision", and a caller reading that error as "not the same" would make the
// check against adopting a branch that never moved answer no every time,
// silently. Both paths are covered for that reason.
func TestSameCommitAnswersBothWays(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "branch", "untouched", "main")
	git(t, dir, "switch", "-q", "-c", "moved")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "work")
	git(t, dir, "switch", "-q", "main")

	if same, err := sameCommit(dir, "untouched", "main"); err != nil || !same {
		t.Errorf("a branch nobody advanced compared %v (%v), want true", same, err)
	}
	if same, err := sameCommit(dir, "moved", "main"); err != nil || same {
		t.Errorf("a branch holding a commit compared %v (%v), want false", same, err)
	}
	// A name git cannot resolve is an error, not two unknowns comparing equal.
	if _, err := sameCommit(dir, "nope", "also-nope"); err == nil {
		t.Error("two refs that do not exist compared without an error")
	}
	if _, err := sameCommit(dir, "nope", "main"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an unresolvable ref gave %v, want an error naming it", err)
	}
}

// Two noes reach finish and the remedy differs, so one refusal covering both
// would teach the operator to reach for --force either way. A branch name the
// template does not render is benign. A branch git ancestry calls merged that
// nothing shows carrying a commit would be recorded done with none of its code
// on the trunk.
func TestFinishRefusesTheTwoNoesDifferently(t *testing.T) {
	for _, c := range []struct {
		name            string
		answer          mergeAnswer
		wants, wantsNot []string
	}{
		{
			name:   "nothing says it finished",
			answer: mergeNo,
			wants:  []string{"does not look merged", "[run] branch template"},
			// The benign refusal must not describe the ancestry hazard, or the
			// wording that matters stops carrying weight by appearing everywhere.
			wantsNot: []string{"never moved", "may not be on it"},
		},
		{
			name:   "ancestry says merged and no row vouches",
			answer: mergeUnvouched,
			// It has to say what git is doing, what is at stake, and the other
			// way out: a run that died before its first commit wants re-running,
			// not forcing.
			wants:    []string{"never moved", "ancestor of the trunk", "may not be on it", "drover drive B1"},
			wantsNot: []string{"[run] branch template"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := refuseFinish("bug/b1-slug", "B1", "gh unavailable", c.answer).Error()
			for _, want := range c.wants {
				if !strings.Contains(got, want) {
					t.Errorf("the refusal does not carry %q:\n%s", want, got)
				}
			}
			for _, no := range c.wantsNot {
				if strings.Contains(got, no) {
					t.Errorf("the refusal carries %q, which belongs to the other no:\n%s", no, got)
				}
			}
			if !strings.Contains(got, "bug/b1-slug") || !strings.Contains(got, "gh unavailable") {
				t.Errorf("the refusal drops the branch or the authority that answered:\n%s", got)
			}
		})
	}
}

// finish closes #47 and must not then count it dormant off a register read that
// still lists it open. The stub answers every read with #47 open, which is the stale
// read at its worst, so only subtracting what was closed keeps it off the count.
func TestFinishDoesNotCountAFollowUpItJustClosed(t *testing.T) {
	const page = `[{"number":47,"title":"closed by this finishing","body":"Owner: B1","state":"OPEN",` +
		`"labels":[{"name":"follow-up"},{"name":"severity:high"}]},` +
		`{"number":48,"title":"still open","body":"Owner: B1","state":"OPEN",` +
		`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}]`
	dir, _ := finishProject(t, ghProject)
	write(t, filepath.Join(dir, "work", "B01-merged.md"),
		"---\ntitle: Merged owner\ndeps: \ncloses: 47\n---\n\nRequirements:\n\n1. Do it.\n")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), ledger.Row{ID: "B1", Issue: 47,
		Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_GH_ISSUES", page)
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') printf '%s' \"$DROVER_TEST_GH_ISSUES\" ;;\n" +
		"'issue close') exit 0 ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	stub(t, filepath.Join(stubPATH(t), "gh"), script)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)

	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	for _, want := range []string{"#47 closed", "1 follow-up owned by B1 is now orphaned (worst L)"} {
		if !strings.Contains(out, want) {
			t.Errorf("finish wrote:\n%s\nwant it to say %q", out, want)
		}
	}
}

// A follow-up drover files names its owner by a link and never by a label: a
// sub-issue of the owner's issue when the owner names one, an Owner: line in
// the body when it does not. The follow-up label is created before it is
// applied, because gh refuses a label the repository lacks.
func TestFileFollowUpLinksTheOwnerRatherThanLabelingIt(t *testing.T) {
	dir := registerProject(t, ghProject)
	writeFile(t, filepath.Join(dir, "work", "B01-has-issue.md"),
		"---\ntitle: Has an issue\ndeps:\nissue: 40\n---\n\nRequirements:\n\n1. Do it.\n")
	writeFile(t, filepath.Join(dir, "work", "B02-no-issue.md"), body("No issue", ""))
	log := stubMirrorGH(t, ghPATH(t), false)
	s := openIn(t, dir)

	for _, c := range []struct {
		id, want, absent string
	}{
		{"B1", "--parent 40", "Owner: B1"},
		{"B2", "Owner: B2", "--parent"},
	} {
		if _, err := s.fileFollowUp(c.id, "post_check failed for "+c.id, "it exited 1"); err != nil {
			t.Fatalf("file for %s: %v", c.id, err)
		}
		creates := callsTo(t, log, "issue create")
		got := creates[len(creates)-1]
		if !strings.Contains(got, c.want) || strings.Contains(got, c.absent) {
			t.Errorf("%s: issue create %q, want %q and no %q", c.id, got, c.want, c.absent)
		}
		if !strings.Contains(got, "--label follow-up") || strings.Count(got, "--label") != 1 {
			t.Errorf("%s: issue create %q, want the follow-up label and no other", c.id, got)
		}
	}
	if made := callsTo(t, log, "label create follow-up --color"); len(made) == 0 {
		t.Errorf("the follow-up label was applied without being created first: %q", ghCalls(t, log))
	}
}
