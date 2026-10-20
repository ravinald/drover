package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var (
	auditCodeBy   string
	auditReviewer string
)

var auditCmd = &cobra.Command{
	Use:   "audit <id> <" + "claimed|reviewed|verified" + "> [note]",
	Short: "Record how hard someone looked, and who looked.",
	Long: `Completion says an item merged. It says nothing about whether anyone checked it,
so an item whose agent merely self-reported and one whose every requirement was
re-confirmed against shipped code look identical without this.

The audit levels are a ladder, each subsuming the one before:
  claimed   the agent's own report was captured; nobody else looked
  reviewed  a review pass ran against the code
  verified  every numbered requirement confirmed against shipped code

--code-by is required and is a separate axis from the level: who wrote the
code. It takes agent, a person's name, or a comma list of them (agent,sam) for
mixed authorship. agent is reserved for a session drover launched and resolves
per session against the item's stage rows, so a review session drover launched
is independent of the implement session it follows. Any other entry is compared
as written against --reviewer. Whoever wrote the code cannot certify every
requirement against it, so recording verified needs a grader who differs from
every entry. A FIX-IN-REVIEW correction no numbered requirement traces to is not
code the level certifies, so it does not make a review session an author of it;
anything else does. An over-claimed verified is worse than no row at all, which
is at least honest about what nobody knows.

--reviewer names who looked. Without it a human reviewer is representable only
as the absence of a launched review session, which reads the same as nobody
having looked, and a named --code-by entry has nobody to be compared against.
Omitting it records nothing: no default and no fallback, because a name drover
chose is a reviewer nobody can go and ask.

The most recent row wins, not the strongest, so a level can be corrected in
either direction. The note is the value: name what you re-checked and what you
found, not that you looked.`,
	Example: `  drover audit B1 claimed --code-by agent "the run's own report, captured"
  drover audit B1 reviewed --code-by agent "read the diff against internal/cmd/resolve.go"
  drover audit B1 verified --code-by agent "R1-R4 traced to shipped code, verify command re-run"
  drover audit B1 verified --code-by agent,dana --reviewer sam "R1-R4 traced by hand"

  verified exits 3 when the grader matches a --code-by entry, and names it. If
  you ran the item and are now reviewing it, the honest level is claimed or
  reviewed, and someone else records verified later.

  An independent row warns when the ledger records one runner for both stages
  of the item. It records the row and exits 0: two sessions of one runner can
  be two reviewers, and only the operator can say which. A --reviewer naming
  somebody else answers that question, so the warning stays quiet.

  Finding one requirement unmet does not lower the level. The gap becomes a
  follow-up filed against its owner, and the row still records that someone traced
  them all. Drop to reviewed only when you could not trace one.`,
	Args: func(_ *cobra.Command, args []string) error {
		// Reading the history is a different question from writing to it, and
		// reaching for it here is reasonable enough to answer rather than reject.
		if len(args) == 1 {
			return fmt.Errorf("audit records a level; to read the history use: drover show %s", args[0])
		}
		return cobra.RangeArgs(2, 3)(nil, args)
	},
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		level := args[1]
		if err := ledger.ValidateLevel(level); err != nil {
			return Refusal{err}
		}
		codeBy, err := ledger.ParseCodeBy(auditCodeBy)
		if err != nil {
			return Refusal{err}
		}
		note := ""
		if len(args) == 3 {
			note = strings.TrimSpace(args[2])
		}
		reviewer := strings.TrimSpace(auditReviewer)

		id := it.ID.Bare()
		entry, why := s.ledger.SameAs(id, levelWriter(reviewer), codeBy)
		if err := ledger.ValidateCodeBy(level, entry, why); err != nil {
			return Refusal{err}
		}
		prev := s.ledger.State(id)
		if entry == "" {
			warnSameRunner(os.Stderr, id, reviewer, prev)
		}
		row := ledger.Row{ID: id, Level: level, Reviewer: reviewer, CodeBy: strings.Join(codeBy, ","),
			Independent: entry == "", Date: ledger.Today(), Note: note}
		if err := s.record(row); err != nil {
			return err
		}

		who := "code by " + row.CodeBy
		if reviewer != "" {
			who += ", reviewed by " + reviewer
		}
		switch {
		case prev.Level == "":
			fmt.Printf("audit recorded: %s -> %s (%s)\n", id, level, who)
		case ledger.Rank(level) < ledger.Rank(prev.Level):
			// Correcting downward is the point of last-row-wins, but it should
			// never happen unnoticed.
			fmt.Printf("audit recorded: %s -> %s (%s)  (DOWNGRADED from %s)\n",
				id, level, who, prev.Level)
		default:
			fmt.Printf("audit recorded: %s -> %s (%s)  (was %s)\n", id, level, who, prev.Level)
		}
		return nil
	},
}

// levelWriter is the party running this command: the session drover launched it
// under, if any, and the name --reviewer gave.
func levelWriter(name string) ledger.Grader {
	return ledger.Grader{Launched: runner.Launched(), Session: runner.Session(), Name: name}
}

// warnSameRunner reports a claim of independence the record contradicts: the
// same runner recorded for both stages of the item being audited.
//
// It warns rather than refuses on purpose. Two sessions of one runner, with
// different context and different operators at the keyboard, are a legitimate
// split that drover cannot see from a name, and a refusal there would push
// people to record a runner they did not use, which costs more than the
// over-claim it prevents. Naming both runners and letting the operator proceed
// keeps the record honest about what drover observed.
//
// A --reviewer naming somebody other than the implementer silences it. The
// warning asks who the second look came from, the operator has just said, and a
// warning that fires after its own question is answered teaches people to
// ignore the ones that matter.
func warnSameRunner(w io.Writer, id, reviewer string, st ledger.State) {
	if st.Reviewer == "" || st.Reviewer != st.Implementer {
		return
	}
	if reviewer != "" && reviewer != st.Implementer {
		return
	}
	fmt.Fprintf(w, "warn: recording an independent level, but one runner is recorded for both stages of %s: "+
		"implemented by %s, reviewed by %s.\n", id, st.Implementer, st.Reviewer)
	fmt.Fprintf(w, "      Recording it as stated. Two sessions of one runner can be two reviewers; "+
		"one session auditing its own work names itself in --code-by.\n")
}

func init() {
	auditCmd.Flags().StringVar(&auditCodeBy, "code-by", "",
		"who wrote the code: agent (a session drover launched), a person's name, or a comma list (required)")
	auditCmd.Flags().StringVar(&auditReviewer, "reviewer", "",
		"who looked, as the operator attributes it: a person or a runner. Omitted records nobody, never a default")
	root.AddCommand(auditCmd)
}
