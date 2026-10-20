package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var clearFile string

var clearCmd = &cobra.Command{
	Use:   "close-changes <id>",
	Short: "Record that the block a review sent back is closed.",
	Long: `A review that sends work back writes the ` + "`" + prompts.ChangesHeading + "`" + ` section.
A review that finds every finding in it closed has to say so, and before this
command the two ways of saying it were both wrong.

Leaving the block is the first. ` + "`run`" + ` reads the body as current instructions and
the prompt outranks the run's own reading of the tree, so nine settled gaps
arrive as nine open ones and the next run spends its turn re-verifying finished
work.

Requesting changes on "nothing came back" is the second. ` + "`request-changes`" + ` appends a row to the
ledger, and ` + "`open-pr`" + ` refuses a branch carrying a changes row inside the window
after the most recent review stage row. So the reviewer clears the block and
refuses the branch it just cleared, and spends a round against
[review] max_review_rounds doing it.

close-changes writes the section and no changes row, which is the split ` + "`invariant`" + ` already
has: what the ledger records is whether a review sent work back, so the command
recording the opposite records no request for changes. Its one row is the body digest every
command that writes an item records, which says drover wrote the body and
nothing about what the review found, marked as close-changes' own. Nothing else reads
it either — no cap, no kept round, no tracker — except open-pr, below.

A drive parks an item whose review recorded an audit and left the block
standing, and open-pr refuses that "findings_open" result. A close-changes recorded
after that pass and its audit is the reviewer's missing half, so open-pr reads
the pass as passed and runs the merge gate as usual. Without a later close-changes the
refusal stands. Inside a review, close-changes before ` + "`audit`" + `: the drive then reads the
pass as passed and merges it.

The statement is drover's own, so a run meets one wording for a closed block
whoever cleared it. -f is what you traced, per finding, and goes below it.

Two refusals bound what the command may claim. A body with no block has nothing
to discharge, and a block the latest review wrote itself is current: the pass
that finds it closed is the pass that clears it, never the pass that wrote it.`,
	Example: `  drover close-changes B1
  drover close-changes B1 -f confirmed.md      # with what you traced, per finding
  cat confirmed.md | drover close-changes B1 -f -

  drover run B1 --dry-run                      # what the next run now reads

Safety: local writes. Rewrites one section of the item body, refuses to change
the numbered requirements, appends a body digest row marked cleared and no changes row to the
ledger, keeps no round, and reaches no tracker.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		var evidence string
		if clearFile != "" {
			if evidence, err = readEvidence(clearFile, os.Stdin); err != nil {
				return err
			}
		}

		raw, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		head, body, err := splitFrontmatter(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}

		// Read off the file rather than off the loaded item: the reviewer wrote
		// the block with request-changes minutes ago, and a stale body would report
		// nothing to clear on the one item that has something.
		if (prompts.Data{Body: body}).Changes() == "" {
			return Refusal{fmt.Errorf(
				"nothing has been sent back to %s: the body carries no %q section, so there is no block "+
					"to clear and clearing it would write a claim about findings nobody made.\n"+
					"A review that found nothing records the look and stops there:\n\n"+
					"    drover audit %s <reviewed|verified> --code-by agent \"<what you traced>\"",
				it.ID.Bare(), prompts.ChangesHeading, it.ID.Bare())}
		}
		if err := s.blockIsAnotherPass(it.ID.Bare()); err != nil {
			return err
		}

		section := clearedBlock(ledger.Today())
		demoted, sunk := 0, 0
		if strings.TrimSpace(evidence) != "" {
			text, n, h := demoteEvidence(strings.TrimSpace(evidence))
			section, demoted, sunk = section+"\n\n"+text, n, h
		}
		next := spliceSection(body, section)

		before := len(it.Requirements())
		after := len(backlog.Item{Body: next}.Requirements())
		if before != after {
			return Refusal{fmt.Errorf(
				"refusing to write: the body has %d numbered requirements and this would leave %d. "+
					"The requirements are the contract; what you confirmed closed goes below the heading "+
					"as prose or bullets", before, after)}
		}

		written := []byte(head + next + "\n")
		if err := os.WriteFile(it.Path, written, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", it.Path, err)
		}
		if err := s.record(ledger.Row{ID: it.ID.Bare(), Body: bodySum(written), Cleared: true, Date: ledger.Today()}); err != nil {
			return err
		}
		fmt.Printf("cleared the block on %s -> %s (%d requirement(s) unchanged)\n",
			it.ID.Bare(), rel(s.cfg.Root, it.Path), after)
		fmt.Println("No changes row: nothing sent work back, so this branch is still one open-pr can open, " +
			"and a drive pass that parked it as findings_open now reads as passed there.")
		reportDemotions(demoted, sunk)
		fmt.Printf("The next run reads it: drover run %s --dry-run\n", it.ID.Bare())
		return nil
	},
}

// clearedBlock is what a discharged block says to the next run.
//
// drover's sentence rather than the reviewer's, because a run reads the section
// as current instructions and has to be able to tell "nothing is outstanding"
// from a finding on sight. A block cleared in whatever words the reviewer
// reached for is a block the next run has to interpret.
//
// It names the date for the reader who arrives later: the rounds it discharges
// are kept beside the saved reports and both briefs list them, so what the body
// cannot otherwise say is when somebody last read them against the tree.
//
// Wrapped to match the bodies around it, which are written by hand.
func clearedBlock(date string) string {
	return fmt.Sprintf(
		"%s The review of %s read the block this replaces\n"+
			"and found every finding in it closed against the tree.", prompts.ClearedMark, date)
}

// blockIsAnotherPass refuses to clear a block the latest review wrote itself.
//
// The ledger's changes row is the only thing that can say so, and it answers
// only inside the window after a review stage row: absent one, the rows cannot
// be attributed to a pass at all, which is the manual lifecycle rather than a
// failure. `open-pr` reads the same window for the same reason.
func (s *session) blockIsAnotherPass(id string) error {
	window, ok := s.sinceLastReview(id)
	if !ok {
		return nil
	}
	for _, r := range window {
		if r.Changes != "" {
			return Refusal{fmt.Errorf(
				"the latest review of %s sent work back on %s, so that block is this pass's own and "+
					"clearing it would tell the next run to skip what a reviewer just wrote. Nothing was "+
					"written.\n"+
					"Fix what came back and review it again: the pass that finds the block closed is the "+
					"pass that clears it", id, r.Date)}
		}
	}
	return nil
}

func init() {
	clearCmd.Flags().StringVarP(&clearFile, "file", "f", "",
		"what you confirmed closed, or - for stdin; optional, and the statement drover writes stands without it")
	root.AddCommand(clearCmd)
}
