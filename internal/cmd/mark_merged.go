package cmd

import (
	"fmt"
	"os"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

var (
	doneRunner string
)

var doneCmd = &cobra.Command{
	Use:   "mark-merged <id>",
	Short: "Record that an item merged.",
	Long: "Completion is recorded by hand. A green run is not a merge, and squash-merge " +
		"makes git state an unreliable witness, so the operator states it.",
	Example: `  drover mark-merged B1             # ledger row only, for a merge recorded by hand
  drover mark-merged B1 --runner codex  # attribute the work to another runner

  Without --runner the row names the runner the recorded run used, falling back
  to [run] default_runner when no run was recorded.

  A --runner [runners] does not declare warns and records anyway. Here the value
  is attribution, and an agent that ran the item outside drover has no entry to
  match; on run, draft and review it selects an executable and is refused.

  Under [backlog] mirror = "github" it also relabels and closes the item's
  mirrored issue and relabels the issues of items naming it on deps, as finish
  and unmark-merged do.

  This is the subset of finish. Running it first does not block finish, which skips
  the row it finds and does the rest.`,
	Args: needID("mark-merged <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		runner := doneRunner
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		id := it.ID.Bare()
		if s.ledger.State(id).Merged {
			fmt.Printf("already done: %s\n", id)
			return nil
		}
		warnUndeclaredRunner(os.Stderr, runner, s.cfg.Runners)
		yes := true
		agent := s.attribution(runner, id)
		row := ledger.Row{ID: id, Merged: &yes, MergedRunner: agent, Date: ledger.Today()}
		if err := s.record(row); err != nil {
			return err
		}
		fmt.Printf("marked merged: %s (implementer %s)\n", id, agent)
		s.mirrorAround(it)
		return nil
	},
}

var undoneCmd = &cobra.Command{
	Use:   "unmark-merged <id>",
	Short: "Withdraw a completion.",
	Long: "Appends a tombstone rather than deleting rows. Completion resolves to the " +
		"most recent statement about it, so a withdrawal is an append — which keeps the " +
		"audit history for that item, and keeps the file append-only for every reader.",
	Example: `  drover unmark-merged B1          # withdraw the completion; the history stays`,
	Args:    needID("unmark-merged <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		id := it.ID.Bare()
		if !s.ledger.State(id).Merged {
			fmt.Printf("not done, nothing to withdraw: %s\n", id)
			return nil
		}
		no := false
		row := ledger.Row{ID: id, Merged: &no, Date: ledger.Today()}
		if err := s.record(row); err != nil {
			return err
		}
		fmt.Printf("withdrawn: %s (audit history kept)\n", id)
		s.mirrorAround(it)
		return nil
	},
}

func init() {
	doneCmd.Flags().StringVar(&doneRunner, "runner", "",
		"runner to attribute the work to (default: the recorded run's runner, else [run] default_runner)")
	root.AddCommand(doneCmd, undoneCmd)
}
