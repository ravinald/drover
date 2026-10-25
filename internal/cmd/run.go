package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var (
	runDry      bool
	runHeadless bool
	runRunner   string
)

var runCmd = &cobra.Command{
	Use:   "run <id>",
	Short: "Assemble the prompt for an item and hand it to a runner.",
	Long: `--dry-run prints the assembled prompt and exits, touching nothing. It is the
way to see what a run will actually carry before spending one.

Dependencies are advisory here: an unmerged dep is reported, not enforced,
because the operator sometimes has a reason.

A runner is a whole binary, not a flag, so a project declares its own under
[runners] and pins one per item with the frontmatter key. Declared here:
{runners}.

A body still carrying the intake stop marker refuses both the launch and the
preview, and exits 3.`,
	Example: `  drover run 1 --dry-run            # print the prompt, touch nothing
  drover run 1                      # branch, then hand it to the runner
  drover run 1 --runner codex       # override the item and the project default
  drover run 1 --headless           # unattended; events stream to a log

  drover run 1 --dry-run | grep -A20 'Open follow-ups'  # what this run will carry

  Ownership travels one hop: a prompt reads what the item itself owns plus what
  each id on its deps line owns. Confirm the run you expect to close a follow-up
  lists its owner as a direct dependency before filing against that owner.`,
	Args: needID("run <id>"),
	RunE: func(c *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		if err := s.depsLint(it); err != nil {
			return err
		}
		if s.isScaffold(it) {
			return Refusal{fmt.Errorf(
				"%s is an unfinished scaffold (%s).\nWrite the numbered requirements and delete the stop marker, then retry",
				it.ID.Bare(), rel(s.cfg.Root, it.Path))}
		}

		data, err := s.briefData(it)
		if err != nil {
			return err
		}
		tmpl, text, err := prompts.Load(s.cfg.Abs(s.cfg.Prompts.Implement), "default", prompts.DefaultTemplate())
		if err != nil {
			return err
		}
		body, err := prompts.Render(tmpl, text, data)
		if err != nil {
			return err
		}

		mode := runner.Interactive
		if runHeadless {
			mode = runner.Headless
		}
		name := s.runnerName(it)

		// Resolved before the preview, not only before the launch. A --dry-run
		// that printed "(unknown runner)" and exited 0 is a preflight consenting
		// to a run that cannot start: an agent reads the exit code, sees the
		// prompt, and launches into an exit 1.
		spec, ok := s.cfg.Runners[name]
		if !ok {
			return fmt.Errorf("unknown runner %q (declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
		}

		if runDry {
			s.preview(os.Stdout, data, name, mode, s.promptPath(s.cfg.Prompts.Implement, "default"), body)
			return nil
		}
		if err := noNestedLaunch(); err != nil {
			return err
		}
		// Before the branch switch, so a tree somebody else is holding costs
		// this invocation nothing: exit 3 promises the tree and the ledger are
		// untouched, and a switch made first would have broken that promise on
		// the way to keeping it.
		release, err := s.takeLease("run", data.ID)
		if err != nil {
			return err
		}
		defer release()

		if s.cfg.Git.On() {
			// A git switch carries uncommitted changes onto the branch it lands on
			// and refuses only where they conflict, so work left from another item
			// would become part of this one's diff with nothing downstream saying
			// so. Nothing is stashed, reset or cleaned on the operator's behalf:
			// only they know which branch the work belongs to.
			paths, err := s.dirtyPaths()
			if err != nil {
				return err
			}
			if len(paths) > 0 {
				// The record paths go in the same sentence rather than a separate
				// one, because a project that tracks its ledger otherwise reads a
				// list of rows drover just wrote and looks for who left them.
				own := ""
				if rec := s.ownRecords(paths); len(rec) > 0 {
					own = ". drover and the sessions it launches write " + strings.Join(rec, ", ") +
						": ignore those paths or keep them outside the host repo, or every run stops here"
				}
				return Refusal{fmt.Errorf("not running %s: %w: %s.\nCommit it on the branch it belongs to or discard it, "+
					"then drover run %s. Nothing was switched%s", data.ID, errDirtyTree, pathList(paths), data.ID, own)}
			}
			if err := switchBranch(s.cfg.Root, data.Branch, s.trunk()); err != nil {
				return err
			}
		}

		logPath := ""
		if runHeadless {
			base := s.cfg.Paths.LogPrefix + runlog.Base(it.ID.Stem(), it.Slug)
			logPath = filepath.Join(s.cfg.LogDir(), runlog.Name(base, time.Now()))
		}
		// [runners] is project-supplied and drover executes it as given, so the
		// launch names the binary PATH resolved and the arguments it gets. A run
		// that started something unexpected is then in the scrollback already,
		// alongside the body and the template the prompt was assembled from.
		banner := launchInfo{
			ID: data.ID, Stage: ledger.StageImplement, Branch: data.Branch,
			Item: data.Path, Prompt: s.promptPath(s.cfg.Prompts.Implement, "default"),
			Log: displayPath(s.cfg.Root, logPath),
		}.from(name, spec, mode)
		if !s.cfg.Git.On() {
			banner.NoGit = fmt.Sprintf("in %s (no branch: [git] enabled is false)", s.cfg.Root)
		}
		banner.write(os.Stderr)

		// Recorded before the launch and from the resolved name, so `finish` and
		// `mark-merged` can attribute the work without parsing a log — which an
		// interactive run never writes, and a second run with another runner
		// would make ambiguous anyway.
		at, err := s.launched(it, ledger.StageImplement, name)
		if err != nil {
			return err
		}

		oc, runErr := runner.Run(c.Context(), at, spec, mode, body, logPath, os.Stdout)
		s.recordUsage(data.ID, ledger.StageImplement, at.Session, oc)
		s.mirrorNow(it)
		// Whatever the exit: a session that failed after committing still left work
		// on the branch, and this row is what lets a later finish tell that branch
		// from one never started, once both sit behind the trunk.
		if s.cfg.Git.On() {
			if err := s.recordHead(data.ID, data.Branch); err != nil {
				fmt.Fprintf(os.Stderr, "  ! %v\n", err)
			}
		}
		if runErr != nil {
			return runErr
		}
		// Only headless runs leave a log. Capturing after an interactive session
		// would reach back to a stale one and misattribute the report.
		if runHeadless {
			s.captureReport(it)
		}
		return nil
	},
}

// preview is --dry-run's whole output: what the run resolved to, then the
// prompt itself. The argv is here because the runner name alone does not say
// what a project's [runners] table pointed that name at.
func (s *session) preview(w io.Writer, d prompts.Data, name string, mode runner.Mode, prompt, body string) {
	where := "no branch"
	if d.Branch != "" {
		where = "branch " + d.Branch
	}
	fmt.Fprintf(w, "# item %s · %s · runner %s\n", d.ID, where, name)
	fmt.Fprintf(w, "# body %s · prompt %s\n", d.Path, prompt)
	fmt.Fprintf(w, "# argv %s\n", s.argvLine(name, mode))
	fmt.Fprintf(w, "%s", body)
}

// argvLine is what the named runner resolves to, for the preview and the
// launch. Both callers resolve the runner first, so the unknown branch is
// unreachable from the CLI and stands as the answer for a caller that did not.
func (s *session) argvLine(name string, mode runner.Mode) string {
	spec, ok := s.cfg.Runners[name]
	if !ok {
		return fmt.Sprintf("(unknown runner %q; declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
	}
	return spec.Display(mode)
}

// lookBinary resolves argv[0] against PATH, falling back to the bare name so a
// missing binary is reported once, by the error runner.Run returns.
func lookBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// promptData resolves everything the template may reach.
func (s *session) promptData(it backlog.Item) (prompts.Data, error) {
	return s.promptDataWith(it, s.register())
}

// briefData is promptData plus the routed issues outside the register, for the
// callers that render a brief. show and drive's planning pass want the branch
// alone, and a tracker query per id there would cost every item listed.
func (s *session) briefData(it backlog.Item) (prompts.Data, error) {
	d, err := s.promptData(it)
	if err != nil {
		return d, err
	}
	s.attachOwned(&d, it, func(ids ...string) ([]gh.Issue, error) { return gh.Owned(s.labels(), ids...) })
	d.Conflict = s.conflict
	return d, nil
}

// attachOwned fills Owned over the same one hop FollowUps uses. Only the GitHub
// backend has route labels on issues outside the register: a file register is
// the whole of what was filed, and a project declaring none asked for no reads.
func (s *session) attachOwned(d *prompts.Data, it backlog.Item, read func(...string) ([]gh.Issue, error)) {
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		return
	}
	mine := it.ID.Bare()
	owned, err := read(append([]string{mine}, it.Deps()...)...)
	if err != nil {
		d.OwnedUnreadable = err.Error()
		return
	}
	for _, is := range owned {
		d.Owned = append(d.Owned, prompts.Issue{
			Number: is.Number, Title: is.Title, Severity: is.Severity,
			Owners: is.Owners, Mine: slices.Contains(is.Owners, mine),
			Body: is.Body, Labels: is.Labels,
		})
	}
}

// promptDataWith takes the register already read, so the branch guarantee below
// can be driven from a reachable and an unreachable tracker rather than from
// whatever gh answers on the day.
func (s *session) promptDataWith(it backlog.Item, index gh.Index) (prompts.Data, error) {
	d := s.localData(it)
	// Derived through branchFor, which never sees the tracker, so run, finish and
	// status all render the same string. Rendering from d once the issues below
	// are attached would let a template reaching .Issues name the branch one
	// thing while gh answers and another while it does not, and finish would then
	// hunt for a name run never created.
	//
	// A project with [git] enabled false leaves it empty rather than rendering a
	// name nothing will ever create: the prompt is what an agent is told to do,
	// and naming a branch there is an instruction, not a label.
	if s.cfg.Git.On() {
		branch, err := s.branchFor(it)
		if err != nil {
			return d, err
		}
		d.Branch = branch
	}
	// Inheritance is one hop: this item plus each id on its own deps line, and
	// nothing further. A prompt carrying every ancestor's open gaps would bury
	// the ones its author can act on. The unaudited report walks transitively; this
	// deliberately does not.
	if index.OK {
		mine := it.ID.Bare()
		// A file register keeps a title, a severity and a route, and has no
		// column for the report behind the row. Saying so per row is the
		// difference between a headline whose evidence lives elsewhere and a
		// headline nobody wrote anything under.
		unavailable := ""
		if s.cfg.RegisterBackend() == config.BackendFile {
			unavailable = "the file register carries no body — read the report where it was filed"
		}
		for _, is := range index.For(append([]string{mine}, it.Deps()...)...) {
			d.FollowUps = append(d.FollowUps, prompts.Issue{
				Number: is.Number, Title: is.Title, Severity: is.Severity,
				Owners: is.Owners, Mine: slices.Contains(is.Owners, mine),
				Body: is.Body, Unavailable: unavailable,
			})
		}
	} else {
		// A register nobody could read renders as a register with nothing in it
		// unless the brief carries the reason: an empty .FollowUps is byte for byte
		// what a clean project produces. Off never reaches here — it keeps OK
		// true, because a project that declared no register has failed nothing.
		d.RegisterUnreadable = index.Reason().Error()
	}
	return d, nil
}

// kindOf is what an item is, frontmatter first and the namespace second.
//
// The frontmatter wins because an item may carry a kind its prefix does not
// imply: intake writes one from the tracker's own label, and a project may
// declare kind on a planned item that has no prefix at all. The namespace is the
// fallback rather than the source, so `[ids] namespaces` stays the map from id to
// kind and never becomes a second place an item's own kind is decided.
//
// Empty is a real answer: a planned item in a project declaring no kind key has
// none, and inventing one would put a word in a branch name that appears nowhere
// in the item.
func (s *session) kindOf(it backlog.Item) string {
	if k := strings.TrimSpace(it.Meta[backlog.KeyKind]); k != "" {
		return k
	}
	return s.cfg.IDs.Namespaces[it.ID.Prefix]
}

// localData is everything a template reaches without asking the tracker. The
// branch name renders from this alone: derived from the register, a branch
// would change name the moment gh was unreachable, and finish would then hunt for
// a branch run never created.
func (s *session) localData(it backlog.Item) prompts.Data {
	d := prompts.Data{
		Project:        s.cfg.Project,
		ID:             it.ID.Bare(),
		Stem:           it.ID.Stem(),
		Slug:           it.Slug,
		Title:          it.Title(),
		Body:           it.Body,
		Path:           rel(s.cfg.Root, it.Path),
		Planned:        it.ID.Planned(),
		Kind:           s.kindOf(it),
		Meta:           it.Meta,
		VerifyCommands: s.cfg.Verify,
		VerifyCommand:  prompts.SelectVerify(s.cfg.Verify, it.Meta),

		MergeGate: strings.Join(s.cfg.Loop.MergeGate, " "),
	}

	// Every brief, not the review alone. The party that decides how wide to look
	// is the one implementing, and it was the one drover told nothing: a fifth
	// correction pass read a brief shaped exactly like the first, so each fix
	// aimed at the case it was handed and the next review found the neighboring
	// one. Read before the stage row the caller is about to write, so a brief
	// says which pass it is rather than which pass it follows.
	c := s.ledger.Counts(it.ID.Bare())
	d.Runs, d.Reviews, d.Audits = c.Of(ledger.StageImplement), c.Of(ledger.StageReview), c.Audits
	d.Rounds = c.ChangesRequested

	// Every earlier round, headline only. A directory drover cannot read costs
	// the progression and no brief: the current findings are in the body either
	// way, and reporting the read failure here would put an operator's disk
	// problem in front of an agent that can do nothing about it.
	if trail, err := s.rounds(it); err == nil {
		d.Trail = trail
	}

	for _, dep := range it.Deps() {
		// Two independent questions, asked separately on purpose. The ledger says
		// whether it merged; the tree says whether there is a body to read. A dep
		// recorded done whose body is gone answers yes to one and no to the other,
		// and a brief reporting only the first sends an agent to a file that is
		// not there.
		e := prompts.Dep{ID: dep, Done: s.done(dep)}
		state, _ := s.resolve(dep)
		e.Unreadable = state.phrase()
		// Level provenance rides every brief rather than the review alone. A
		// zero Weak on a field nothing populated reads as a foundation somebody
		// independently confirmed, which is the one answer a template must not
		// be handed.
		ds := s.ledger.State(dep)
		e.Level, e.By, e.Independent, e.Weak = ds.Level, ds.CodeBy, ds.Independent, weakFoundation(ds)
		if di, err := s.item(dep); err == nil {
			e.Title = di.Title()
			// The report path is derived, never read from the ledger: a rename
			// leaves the recorded path behind, and the file on disk is the truth.
			if p := s.reportPath(di); fileExists(p) {
				e.Report = rel(s.cfg.Root, p)
			}
		}
		d.Deps = append(d.Deps, e)
	}

	for _, spec := range strings.Fields(it.Get("spec")) {
		d.Specs = append(d.Specs, filepath.Join(s.cfg.Paths.Specs, spec))
	}
	for _, name := range strings.Fields(it.Get("skills")) {
		sk := prompts.Skill{Name: name}
		if dir := s.cfg.Paths.Skills; dir != "" {
			guide := filepath.Join(dir, name, "SKILL.md")
			if fileExists(s.cfg.Abs(guide)) {
				sk.Path = guide
			}
		}
		d.Skills = append(d.Skills, sk)
	}

	return d
}

func (s *session) reportPath(it backlog.Item) string {
	return s.cfg.Abs(filepath.Join(s.cfg.Paths.Reports, it.ID.Stem()+"-"+it.Slug+".md"))
}

// runnerName resolves the runner: the flag, then the item's frontmatter, then
// the project default.
func (s *session) runnerName(it backlog.Item) string { return s.runnerFor(it, runRunner) }

// runnerFor is the same resolution with the override passed in rather than read
// off a package variable.
//
// A command that drives many items cannot use runnerName: cobra does not reset
// flag variables between invocations, so one command would be reading another's
// flag, and a loop's override is one value across a queue rather than the flag
// of whichever command ran last.
func (s *session) runnerFor(it backlog.Item, override string) string {
	if override != "" {
		return override
	}
	if v := it.Get(s.cfg.Run.RunnerKey); v != "" {
		return v
	}
	return s.cfg.Run.DefaultRunner
}

// switchBranch creates the branch from start, or moves to it when it is already
// there. A failure is fatal rather than ignored: ignored, a detached HEAD or a
// held index lock runs the agent on whatever branch happens to be out.
//
// The start point is the trunk rather than HEAD, which is the same hazard one
// step earlier. `git switch -c` with no start point branches from wherever the
// checkout was standing, so a drive started from an unmerged feature branch
// bases every item in the queue on it. The items inherit commits they never
// touched, and the defect surfaces hours later as a merge conflict in files
// the item never named. An existing branch is switched to rather than
// recreated, because that is how an item sent back returns to its own work.
func switchBranch(root, branch, start string) error {
	create := gitcmd.Command(root, "switch", "-c", branch, start)
	if out, err := create.CombinedOutput(); err == nil {
		return nil
	} else if !strings.Contains(string(out), "already exists") {
		// Any failure other than "the branch is already there" is real.
		if err := gitcmd.Command(root, "rev-parse", "--git-dir").Run(); err != nil {
			return fmt.Errorf("not a git repository: %s", root)
		}
	}
	sw := gitcmd.Command(root, "switch", branch)
	if out, err := sw.CombinedOutput(); err != nil {
		return fmt.Errorf("git switch %s: %s", branch, strings.TrimSpace(string(out)))
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// keys is the declared runner set as it appears to an operator, in five
// refusals and one warning. Sorted because Go randomizes map order per range,
// so the same unknown-runner message otherwise reorders its declared set
// between two invocations that differ in nothing else.
func keys(m map[string]runner.Spec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	runCmd.Flags().BoolVar(&runDry, "dry-run", false, "print the assembled prompt and exit")
	runCmd.Flags().BoolVar(&runHeadless, "headless", false, "run unattended, streaming events to a log")
	runCmd.Flags().StringVar(&runRunner, "runner", "", "runner to use, overriding the item and the project default; declared: {runners}")
	root.AddCommand(runCmd)
}
