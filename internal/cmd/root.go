// Package cmd wires the drover CLI together.
package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var root = &cobra.Command{
	Use:   "drover",
	Short: "Drive AI coding agents from Markdown requirements to reviewed code, with an audit trail for every work item.",
	Long: `drover runs a work item as a coding-agent session, or a bounded queue of them
with ` + "`drover drive`" + `, tracks which items merged, captures each run's
self-report, and records how hard anyone looked at the result. A work item is one markdown file; the directory holding them is the
backlog; the ledger is append-only.

Driving this from an agent: run ` + "`drover agents`" + ` for the instructions generated
from this project's own config, and ` + "`drover run <id> --dry-run`" + ` to see exactly
what a run will carry before spending one. Parse --json, never a table.

Ids are bare: B1, never B01. Filenames carry the padded form (B01-<slug>.md) so
a directory listing sorts in backlog order; every command takes the bare form
that ` + "`drover status`" + ` prints.

Safety, repeated in each command's own help:
{safety}
Whose step it is. These refuse inside a session drover launched, because each
certifies work somebody else has to trust, reaches a tracker other people read,
or launches sessions:
{operator}
A launched session also cannot launch another, so run, draft --runner and
review --runner refuse there too, and so does init supervise, which decides
what a supervisor may do. Everything else is safe from a session.
The one exception is the session ` + "`drover supervise`" + ` starts: it may run drive
while [supervise] enqueue allows it, and any command that is a supervised action
only at supervisor or auto policy.

Exit codes:
  0  clean
  1  drover could not answer: unreadable config, no such item, gh unreachable
  2  findings against the project (check)
  3  a rule drover enforces refused the operation; nothing was written

--json is on status, show, unaudited, drive, drives, attach, check, follow-ups, intake --list,
projects, skills and report. Each of those carries a sample object under Examples in its own --help.
` + "`drover schema`" + ` prints the .drover.toml contract for a validator or a generator.`,
	Example: `  drover agents                 # this project's agent instructions, from its config
  drover status                 # the backlog and what state each item is in
  drover run 1 --dry-run        # exactly what an agent will be handed
  drover run 1                  # branch, then hand it to the runner
  drover review 1               # the reviewer's prompt, once the run finishes
  drover audit 1 verified --code-by agent "R1-R4 traced to shipped code"
  drover finish 1               # after merging: ledger row, report, back to trunk`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Inherited by every subcommand, so a command marked `operator` in the
	// classification table is gated by that entry alone. See actor.go.
	PersistentPreRunE: func(c *cobra.Command, _ []string) error {
		if err := gate(invocation(c)); err != nil {
			return err
		}
		return checkStale(c)
	},
}

func init() {
	root.PersistentFlags().BoolVarP(&ignoreStale, "ignore-stale", "I", false,
		"run even though .drover.toml or the generated agent instructions are older than this binary")
}

// Execute runs the root command.
func Execute() error {
	install()
	// One context for the process, cancelled by an interrupt or a terminate, and
	// handed to every command through cmd.Context(). The launch paths pass it to
	// runner.Run, which signals the child's process group: without this the
	// signal reaches drover and leaves the agent it started running.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return root.ExecuteContext(ctx)
}
