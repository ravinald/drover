package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var (
	draftHeadless bool
	draftRunner   string
)

var draftCmd = &cobra.Command{
	Use:   "draft <id>",
	Short: "Assemble the prompt for drafting a scaffolded item's requirements.",
	Long: `intake writes the scaffold and refuses to write the requirements. This is the
help for writing them: the issue read fresh from the tracker, the project's
principles, the numbered-list contract, the rest of the backlog so a requirement
belonging elsewhere goes to the item that owns it, and the body file to write into.

Drafting and accepting are different acts. The prompt tells the agent to leave
the stop marker in place, because deleting it is what makes the body runnable
and that judgement is the operator's. Nothing draft does clears it, and no flag
here can. drover accept <id> is the operator's command for it.

With no runner it prints the prompt and returns. Given one it launches instead,
without switching the branch: a draft edits one file in the tree it is already
standing in. The drafting runner comes from --runner or [run] draft_runner and
never from the item's own frontmatter, which names who implements the item.
Declared: {runners}.`,
	Example: `  drover draft B1                   # the prompt: issue, principles, contract, siblings
  drover draft B1 --runner claude   # hand the prompt to a drafting runner instead
  drover draft B1 --headless        # unattended; events stream to a draft log

  The three stages, each with its own runner:

    drover intake 12                             # scaffold, marker planted
    drover draft B1 --runner claude --headless   # propose the requirements
    drover accept B1                             # a human accepts the list
    drover run B1 --runner codex --headless      # implement
    drover review B1 --runner gemini --headless  # review

  Between the draft and the run, a human reads the proposed list in
  {backlog}/B01-*.md and runs drover accept. That is the accepting act, the one
  step no runner performs, and no flag here performs it either.

  draft refuses an item whose marker is already gone and exits 3. A body past
  intake is somebody's accepted contract; redrafting it would overwrite work
  nobody asked to have replaced.`,
	Args: needID("draft <id>"),
	RunE: func(c *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		if err := s.draftable(it); err != nil {
			return err
		}

		data := s.draftData(it)
		tmpl, text, err := prompts.Load(s.cfg.Abs(s.cfg.Prompts.Draft), "draft", prompts.DraftTemplate())
		if err != nil {
			return err
		}
		out, err := prompts.Render(tmpl, text, data)
		if err != nil {
			return err
		}

		name := s.draftRunnerName()
		if name == "" {
			fmt.Print(out)
			return nil
		}
		spec, ok := s.cfg.Runners[name]
		if !ok {
			return fmt.Errorf("unknown runner %q (declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
		}
		if err := noNestedLaunch(); err != nil {
			return err
		}
		// A drafting session switches no branch, and it still holds an agent
		// inside the tree for as long as it runs. The checkout is what the
		// lease protects, not the branch.
		release, err := s.takeLease("draft", it.ID.Bare())
		if err != nil {
			return err
		}
		defer release()

		mode := runner.Interactive
		if draftHeadless {
			mode = runner.Headless
		}
		logPath := ""
		if draftHeadless {
			base := s.cfg.Paths.LogPrefix + runlog.DraftBase(it.ID.Stem(), it.Slug)
			logPath = filepath.Join(s.cfg.LogDir(), runlog.Name(base, time.Now()))
		}
		launchInfo{
			ID: it.ID.Bare(), Stage: "draft",
			Item:   rel(s.cfg.Root, it.Path),
			Prompt: s.promptPath(s.cfg.Prompts.Draft, "draft"),
			Log:    displayPath(s.cfg.Root, logPath),
		}.from(name, spec, mode).write(os.Stderr)
		fmt.Fprintln(os.Stderr, "==> the agent writes the body above; the stop marker stays until you delete it")
		_, err = runner.Run(c.Context(), s.where(), spec, mode, out, logPath, os.Stdout)
		return err
	},
}

// draftable refuses an item that is not a scaffold any more.
//
// The marker is the whole test. A body past intake carries requirements a human
// accepted, and handing that to a drafting agent invites a rewrite of somebody
// else's contract — the one edit in this tree nobody would notice until the run
// executed the new version.
func (s *session) draftable(it backlog.Item) error {
	// Two questions, and only the second is isScaffold's. A project declaring no
	// marker cannot tell a scaffold from a contract at all, which is a fault in
	// its config; an item whose marker is gone is a contract somebody accepted.
	// Collapsed into one test, a project with no marker would be told its items
	// are all accepted, which is the wrong fix for the wrong problem.
	if s.cfg.Intake.StopMarker == "" {
		return Refusal{fmt.Errorf(
			"this project declares no [intake] stop_marker, so drover cannot tell a scaffold from an accepted contract.\nSet one and re-scaffold, or write the requirements by hand: %s",
			rel(s.cfg.Root, it.Path))}
	}
	if !s.isScaffold(it) {
		return Refusal{fmt.Errorf(
			"%s is past intake: its stop marker is gone, so its requirements are somebody's accepted contract (%s).\nEdit it by hand, or file what you found against the item that owns it",
			it.ID.Bare(), rel(s.cfg.Root, it.Path))}
	}
	return nil
}

// draftRunnerName resolves the drafting runner: the flag, then the project
// default. Like reviewRunnerName it takes no item, because the frontmatter key
// it would read names who implements the item — and an item with no
// requirements yet has nothing to say about who should propose them.
func (s *session) draftRunnerName() string {
	if draftRunner != "" {
		return draftRunner
	}
	return s.cfg.Run.DraftRunner
}

// draftData is what an author needs and an implement prompt does not: where to write, the
// bar to write against, the issue behind the scaffold, and the siblings a
// misplaced requirement belongs to.
//
// It deliberately skips promptData's tracker read for routed issues. Those
// answer "what is already open against this item", which is a question about
// work in flight; a draft has none yet.
func (s *session) draftData(it backlog.Item) prompts.Data {
	d := s.localData(it)
	d.Posture = s.cfg.Agents.Principles
	d.StopMarker = s.cfg.Intake.StopMarker
	d.Origin = s.origin(it)
	d.Finding = s.finding(it)

	for _, other := range s.backlog.Items {
		if other.ID == it.ID {
			continue
		}
		d.Siblings = append(d.Siblings, prompts.Sibling{
			ID:    other.ID.Bare(),
			Title: other.Title(),
			Path:  rel(s.cfg.Root, other.Path),
			Done:  s.done(other.ID.Bare()),
		})
	}
	return d
}

// origin reads the tracker issue the scaffold came from.
//
// Read live rather than lifted out of the quoted block the scaffold holds: an
// issue gains detail after intake, and a draft written from the copy is written
// from what was known that day. An unreachable tracker degrades to a named
// reason, which the brief prints, rather than to silence that reads as an empty
// issue.
func (s *session) origin(it backlog.Item) prompts.Origin {
	raw := it.Get("issue")
	if raw == "" {
		return prompts.Origin{}
	}
	number, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || number <= 0 {
		return prompts.Origin{Unavailable: fmt.Sprintf("the item's issue: key reads %q, which is not an issue number", raw)}
	}
	if !gh.Available() {
		return prompts.Origin{Number: number, Unavailable: "gh is not on PATH, or `gh auth login` has not been run"}
	}
	issue, err := gh.View(s.labels(), number)
	if err != nil {
		return prompts.Origin{Number: number, Unavailable: err.Error()}
	}
	return prompts.Origin{Number: number, Title: issue.Title, Body: issue.Body}
}

// finding reads the audit notes on the item a scaffold's provenance line names.
// Read from the ledger at draft time, so a note recorded after `new` ran is in
// the brief; newest first, because a later pass usually corrects an earlier one.
func (s *session) finding(it backlog.Item) prompts.Finding {
	from := foundIn(it.Body, s.cfg.Intake.StopMarker)
	if from == "" {
		return prompts.Finding{}
	}
	f := prompts.Finding{From: from}
	for i := len(s.ledger.Rows) - 1; i >= 0; i-- {
		r := s.ledger.Rows[i]
		if r.ID != from || r.Level == "" || strings.TrimSpace(r.Note) == "" {
			continue
		}
		f.Notes = append(f.Notes, prompts.AuditNote{Date: r.Date, Level: r.Level, By: r.CodeBy, Note: r.Note})
	}
	return f
}

func init() {
	draftCmd.Flags().BoolVar(&draftHeadless, "headless", false, "run the draft unattended, streaming events to a log")
	draftCmd.Flags().StringVar(&draftRunner, "runner", "", "hand the prompt to this runner instead of printing it, overriding [run] draft_runner; declared: {runners}")
	root.AddCommand(draftCmd)
}
