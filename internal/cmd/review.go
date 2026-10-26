package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var (
	reviewHeadless bool
	reviewRunner   string
)

var reviewCmd = &cobra.Command{
	Use:   "review <id>",
	Short: "Assemble the prompt for reviewing a finished item.",
	Long: `The mirror of run: run assembles what the implementer is handed, review
assembles what the reviewer is handed.

It gathers what a reviewer would otherwise collect by hand from four separate
commands — the numbered contract, the dependencies and how well each is
audited, the open follow-ups, the dependency reports, the run's self-report, and
the verify command — so that skipping one is a choice rather than an oversight.

With no runner it prints the prompt and returns. Given one it launches instead,
the same way run does, minus the branch switch: review runs before the merge,
on the branch run already left the tree standing on, and moving it would review
something other than the work. The reviewing runner comes from --runner or
[run] review_runner and never from the item's own frontmatter, because that key
names who implements the item and reviewing with the same name is the
self-certification audit refuses to record as verified. Declared: {runners}.

When a headless runner returns it prints the pass token, what the pass amounted
to and the next step, in the same tokens and sentences a drive parks on, and
names what the runner refused the session where the terminal event carried it:
a review denied every command reads as one that could not reach drover audit,
not as one that found nothing. An interactive review prints nothing after the
runner returns, because it writes no log to read a pass from. Nothing is
recorded either way: a pass row is drive's, and open-pr reads one as the
authority that a branch may merge.`,
	Example: `  drover review 1                   # the prompt: contract, deps, follow-ups, report, verify command
  drover review 1 --runner codex    # hand the prompt to a reviewing runner instead
  drover review 1 --headless        # unattended; events stream to a review log
                                    # and it ends on: Pass: silent

  The split, end to end — one runner implements, another reviews, and the
  ledger records who did which:

    drover run 1 --runner claude --headless
    drover review 1 --runner codex --headless
    drover audit 1 verified --code-by agent "R1-R4 traced to shipped code"

  The prompt ends with that audit line pre-filled. Record it once you have
  traced the requirements, and name in --code-by everyone who wrote the code
  under review: agent for the implement session drover launched, plus your own
  name if you wrote any of it.

  A dependency review marks as weak is one nobody independently confirmed.
  What it claims to have built is a claim.

  While the item body carries a sent-back block nobody cleared, the prompt ends
  on drover's own instruction naming the two exits, and no template removes
  it: drover close-changes <id> before the audit when every finding is closed, drover
  request-changes <id> when one is open. An audit with neither reads as "findings_open":
  drive parks the item and open-pr refuses it until a later drover close-changes.`,
	Args: needID("review <id>"),
	RunE: func(c *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		data, err := s.reviewData(it)
		if err != nil {
			return err
		}
		tmpl, text, err := prompts.Load(s.cfg.Abs(s.cfg.Prompts.Review), "review", prompts.ReviewTemplate())
		if err != nil {
			return err
		}
		out, err := prompts.Render(tmpl, text, data)
		if err != nil {
			return err
		}
		out += reviewExits(it)

		name := s.reviewRunnerName()
		if name == "" {
			fmt.Print(out)
			return nil
		}
		spec, ok := s.cfg.Runners[name]
		if !ok {
			return fmt.Errorf("unknown runner %q (declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
		}

		mode := runner.Interactive
		if reviewHeadless {
			mode = runner.Headless
		}
		if err := noNestedLaunch(); err != nil {
			return err
		}
		// Before the stage row, for the reason the nested-launch check is:
		// exit 3 says the ledger is untouched. review switches no branch, but
		// it holds an agent inside the tree, which is the thing being kept to
		// one command.
		release, err := s.takeLease("review", it.ID.Bare())
		if err != nil {
			return err
		}
		defer release()

		logPath := ""
		if reviewHeadless {
			base := s.cfg.Paths.LogPrefix + runlog.ReviewBase(it.ID.Stem(), it.Slug)
			logPath = filepath.Join(s.cfg.LogDir(), runlog.Name(base, time.Now()))
		}
		// The branch is reported rather than switched: review runs on whatever
		// run left standing, and naming it here is how an operator sees that the
		// brief is about the work in the tree.
		launchInfo{
			ID: it.ID.Bare(), Stage: ledger.StageReview, Branch: data.Branch,
			Item: data.Path, Prompt: s.promptPath(s.cfg.Prompts.Review, "review"),
			Log: displayPath(s.cfg.Root, logPath),
		}.from(name, spec, mode).write(errOut)

		before := s.ledger.Counts(it.ID.Bare())
		at, err := s.launched(it, ledger.StageReview, name)
		if err != nil {
			return err
		}

		oc, runErr := runner.Run(c.Context(), at, spec, mode, out, logPath, os.Stdout)
		s.recordUsage(it.ID.Bare(), ledger.StageReview, at.Session, oc)
		// An interactive session writes no log, so there is no terminal event to
		// read and every pass would report as died. The operator watched it; the
		// runner's own exit is the whole answer.
		if !reviewHeadless {
			return runErr
		}
		token, f := s.reviewFollowUp(it, name, before, oc)
		if token != "" {
			fmt.Fprintf(errOut, "Pass:       %s\n", token)
		}
		f.write(errOut, "")
		return runErr
	},
}

// reviewFollowUp reads a headless pass the way drive reads one, log included,
// which is what names a runner's denials beside a pass that recorded nothing,
// and words it in drive's own tokens and sentences.
//
// Printed and never recorded. A pass row is drive's, and `open-pr` reads one
// as the authority that a branch may merge: writing one here would hand a
// review nobody supervised the standing of a drive's own result.
func (s *session) reviewFollowUp(it backlog.Item, name string, before ledger.Counts,
	oc runner.Outcome) (string, followUp) {
	id := it.ID.Bare()
	again := reviewAgain(id, name)
	// The rows the session wrote landed in a child process, so the ledger this
	// one loaded at startup cannot see them.
	if err := s.reload(); err != nil {
		return "", followUp{"the ledger could not be read back after the review: " + err.Error(), "drover show " + id}
	}
	p := pass{before: before, after: s.ledger.Counts(id), res: oc.Result, logged: oc.Logged,
		events: oc.Events, known: oc.Known, cap: s.cfg.Review.MaxReviewRounds}
	p.readBody(it.Path)
	token := passResult(p)
	if token == passRepeating {
		p.sentBack = s.sentBack(it)
	}
	return token, passFollowUp(token, p, id, rel(s.cfg.Root, it.Path), name,
		displayPath(s.cfg.Root, oc.LogPath), again)
}

// reviewExits is the instruction a brief ends on while the item body carries a
// sent-back block nobody closed, and empty otherwise.
//
// drover's text rather than the template's, because the outcome it guards is
// drover's: an audit beside an open block is passStanding whatever the project
// template says, and a template that dropped or reworded the two exits left
// reviewers recording passing audits that parked six items in one drive. Appended after the render, so no template can reach it.
func reviewExits(it backlog.Item) string {
	if !openBlock(it.Body) {
		return ""
	}
	id := it.ID.Bare()
	return fmt.Sprintf(`
drover's own closing instruction, which no project template sets:

This item's body still carries the %q block an earlier round
wrote. End this pass on one of two exits, before you record the audit:

- Every finding in that block is closed: drover close-changes %s
  (add -f <file> with what you traced, per finding). Then record the audit.
- Any finding is still open: drover request-changes %s -f <file>

An audit recorded with neither reads as %q: drover parks the item, open-pr
refuses it, and nothing that depends on it runs, however high the level.
`, prompts.ChangesHeading, id, id, passStanding)
}

// reviewRunnerName resolves the reviewing runner: the flag, then the project
// default. It takes no item on purpose — reading the item's runner key here is
// the one thing this must not do, and a signature that cannot see the item
// cannot regress into doing it.
func (s *session) reviewRunnerName() string { return s.reviewRunnerFor(reviewRunner) }

// reviewRunnerFor takes the override rather than reading the package flag, for
// runnerFor's reason. It still never consults the item's frontmatter: that key
// names who implements, and reviewing under it is the self-certification
// audit refuses to record as verified.
func (s *session) reviewRunnerFor(override string) string {
	if override != "" {
		return override
	}
	return s.cfg.Run.ReviewRunner
}

// reviewData is the prompt data plus what only a reviewer needs.
func (s *session) reviewData(it backlog.Item) (prompts.Data, error) {
	d, err := s.briefData(it)
	if err != nil {
		return d, err
	}
	d.Requirements = it.Requirements()
	if c := s.reviewConflict(it.ID.Bare(), d.Branch); c != nil {
		d.Conflict = c
	}

	st := s.ledger.State(it.ID.Bare())
	d.Level, d.By, d.Independent, d.Note, d.AuditDate = st.Level, st.CodeBy, st.Independent, st.Note, st.Date

	// The pass counts arrive from localData, which every brief shares. What only
	// a reviewer needs is the cap: it bounds what this pass may spend, and a
	// brief instructing `drover request-changes` beside a command that will refuse is the
	// tool arguing with itself.
	d.Recent.Commits, d.Recent.Unavailable = s.recentCommits(d.Branch)
	d.Modified, d.Unnamed, d.ScopeUnavailable = s.scopeOfDiff(d.Branch, it.Body)
	d.MaxReviewRounds = s.cfg.Review.MaxReviewRounds
	d.Capped = d.MaxReviewRounds > 0 && d.Rounds >= d.MaxReviewRounds

	// A missing log is a normal state; a log with no result is a different one.
	if vd, logPath, err := s.reportData(it); err == nil {
		d.SelfReport, d.Outcome, d.LogPath = vd.Report, vd.Headline, logPath
		d.Unfinished = logPath != "" && vd.Report == ""
	}
	d.Closes = s.closesData(it)
	return d, nil
}

// closesData reads the issues the item claims to resolve, each fetched live.
//
// Fresh rather than from the body's own copy, for draft's reason in origin: an
// issue gains detail after it was quoted, and a reviewer judging against the
// stale text judges what was known that day. It also catches the issue somebody
// already closed by hand, which no local record knows about.
func (s *session) closesData(it backlog.Item) []prompts.Resolution {
	numbers := backlog.Ints(it.Get(closesKey))
	if len(numbers) == 0 {
		return nil
	}
	id := it.ID.Bare()
	available := gh.Available()
	out := make([]prompts.Resolution, 0, len(numbers))
	for _, n := range numbers {
		r := prompts.Resolution{Number: n}
		if res, ok := s.ledger.Resolution(id, n); ok || res.Applied != "" {
			r.Disposition, r.By, r.Note, r.Applied = res.Disposition, res.CodeBy, res.Note, res.Applied
			r.Settled = res.Settled
		}
		switch {
		case !available:
			r.Unavailable = "gh is not on PATH, or `gh auth login` has not been run"
		default:
			issue, err := gh.View(s.labels(), n)
			if err != nil {
				r.Unavailable = err.Error()
			} else {
				r.Title, r.Body, r.State = issue.Title, issue.Body, issue.State
			}
		}
		out = append(out, r)
	}
	return out
}

// weakFoundation reports a dependency nobody has independently confirmed.
//
// Four states collapse to the same practical meaning for a reviewer: nobody
// looked, only the agent's own report was captured, the level came from
// whoever wrote the code, or the level carries no recorded authorship and
// cannot be distinguished from those.
func weakFoundation(st ledger.State) bool {
	switch {
	case !st.Audited():
		return true
	case st.Level == ledger.LevelClaimed:
		return true
	case st.CodeBy == "":
		return true
	case !st.Independent:
		return true
	default:
		return false
	}
}

func init() {
	reviewCmd.Flags().BoolVar(&reviewHeadless, "headless", false, "run the review unattended, streaming events to a log")
	reviewCmd.Flags().StringVar(&reviewRunner, "runner", "", "hand the prompt to this runner instead of printing it, overriding [run] review_runner; declared: {runners}")
	root.AddCommand(reviewCmd)
}
