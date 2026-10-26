// Package cmd wires the drover CLI together.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
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

Color is on by default where a stream is a terminal and off everywhere else.
--color=auto|always|never sets it for one invocation (--no-color is never),
ahead of NO_COLOR, CLICOLOR_FORCE and TERM=dumb. --json, --events and every
file drover writes are never colored, and every colored state is a word too.

--json is on status, show, unaudited, drive, drives, attach, check, follow-ups, intake --list,
projects, skills, t3 discover and report. Each of those carries a sample object under Examples in its own --help.
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
		"run even though .drover.toml or the generated agent instructions do not match this binary")
	root.PersistentFlags().Var(colorValue{}, "color",
		"color terminal output: auto, always or never; --json, --events and files are never colored")
	noColor := root.PersistentFlags().VarPF(noColorValue{}, "no-color", "", "the same as --color=never")
	noColor.NoOptDefVal = "true"
}

// colorValue is --color, refusing at parse anything but the three modes, so a
// typo fails loudly rather than reading as auto.
type colorValue struct{}

func (colorValue) String() string { return colorMode }
func (colorValue) Type() string   { return "mode" }
func (colorValue) Set(v string) error {
	switch v {
	case colorAuto, colorAlways, colorNever:
		colorMode = v
		return nil
	}
	return fmt.Errorf("--color takes auto, always or never; got %q", v)
}

// noColorValue is --no-color, an alias writing never into the same mode.
type noColorValue struct{}

func (noColorValue) String() string { return strconv.FormatBool(colorMode == colorNever) }
func (noColorValue) Type() string   { return "bool" }
func (noColorValue) Set(v string) error {
	off, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	if off {
		colorMode = colorNever
	}
	return nil
}

// Execute runs the root command.
func Execute() error {
	install()
	// One context for the process, cancelled by an interrupt, a terminate or a
	// hangup (a closed terminal tab, which ends an interactive run), and
	// handed to every command through cmd.Context(). The launch paths pass it to
	// runner.Run, which signals the child's process group: without this the
	// signal reaches drover and leaves the agent it started running.
	//
	// The hangup is registered only when drover did not inherit it ignored.
	// Notify replaces an inherited SIG_IGN with a handler, so registering it
	// unconditionally makes `nohup drover drive` die on terminal close, the one
	// thing nohup is there to prevent, and the session it execs loses the
	// ignore too. Ignored has to be read before any Notify for the signal.
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	ctx, stop := signal.NotifyContext(context.Background(), sigs...)
	defer stop()
	return root.ExecuteContext(ctx)
}
