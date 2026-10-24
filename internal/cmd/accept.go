package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ravinald/drover/internal/backlog"
)

// acceptYes overrides the refusals accept makes about what a body says.
var acceptYes bool

var acceptCmd = &cobra.Command{
	Use:   "accept <id>...",
	Short: "Delete the stop marker, making a scaffolded item runnable.",
	Long: `intake plants a stop marker and draft leaves it there, because deciding that a
proposed list of requirements is the contract is the operator's judgement and
not a runner's. This is that decision, spelled as a command.

It deletes the block intake wrote and nothing else: the marker line, the two
lines under it saying what the marker does, and the blank that separates them
from the body. unaccept writes the same block back, so accept followed by
unaccept returns the file byte for byte.

The lines under the marker are matched against intake's own text rather than
scanned to the next blank, so a marker somebody typed by hand with a paragraph
directly under it loses the marker line alone.

A body that cites a directory in the project tree, as a path dispatch reads,
gets one note per directory: under drive --jobs the item waits for any item
citing a path below it. The note changes nothing and the exit stays 0.

An item another unmerged item waits on is read against that item's branch:
every path it cites with a line part, and every backticked name that looks like
a function or type, that the trunk lacks and the waiting item's branch has. The
accepted item runs from the trunk and cannot change that code, and the item
waiting on it cannot merge first, so the two would deadlock. accept refuses
it, names each citation, and changes nothing. --yes overrides that refusal,
which is the one accept makes about the body's content. It never overrides the
stop-marker or actor checks, or a deps line drover would misread.

Idempotent. An item with no marker is already accepted: that is reported and
exits 0, because the operator named a state rather than a transition.

Under [backlog] mirror = "github" an accepted item is also published: an issue
carrying its title, its numbered requirements and a drover: state label, with
a blocked-by link for each dep that names an issue. The new number is written
into the item's issue key. Accepting an item that already has its issue
rewrites that issue from the file, which is how the mirror is repaired after
` + "`drover check`" + ` reports it behind. A tracker drover cannot reach costs the
mirror and never the accept: the failure is named and the exit stays 0.

Several ids accept each in turn. One that fails (no such item, a padded id, an
unwritable body) is reported on stderr by name and the rest still go ahead; the
exit is then non-zero, 3 when every failure was a refusal and 1 otherwise, and
the last line lists the ids that were not accepted.`,
	Example: `  drover accept B1                  # the requirements are the contract; run may launch
  drover accept B1                  # again: already accepted, exit 0
  drover accept B45 B46 E28         # each in turn; a bad id is named and skipped
  drover accept B59 --yes           # past the deadlock refusal, having read it

  The three steps around it:

    drover draft B1 --runner claude   # an agent proposes the numbered list
    drover show B1                    # where the body is; read what it proposed
    drover accept B1                  # you accept it

  drover unaccept B1                # put the marker back and redraft`,
	Args: needSomeIDs("accept <id>..."),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if err := s.markerConfigured(); err != nil {
			return err
		}
		if len(args) == 1 {
			return s.acceptOne(args[0])
		}
		var failed []string
		refusedAll := true
		for _, a := range args {
			if err := s.acceptOne(a); err != nil {
				fmt.Fprintf(os.Stderr, "accept %s: %v\n", a, err)
				failed = append(failed, a)
				if Code(err) != ExitRefused {
					refusedAll = false
				}
			}
		}
		if len(failed) == 0 {
			return nil
		}
		err = fmt.Errorf("%d of %d not accepted: %s (each reason is above)",
			len(failed), len(args), strings.Join(failed, " "))
		if refusedAll {
			return Refusal{err}
		}
		return err
	},
}

// acceptOne is the whole of accept for one id. The list form calls it per id
// and keeps going, so one mistyped id in a batch costs that id and not the rest.
func (s *session) acceptOne(bare string) error {
	it, err := s.item(bare)
	if err != nil {
		return s.prefixedHint(bare, err)
	}
	if err := s.depsLint(it); err != nil {
		return err
	}
	lines, err := bodyLines(it.Path)
	if err != nil {
		return err
	}
	at := markerLine(strings.Join(lines, "\n"), s.cfg.Intake.StopMarker)
	if at < 0 {
		fmt.Printf("already accepted: %s carries no stop marker (%s)\n",
			it.ID.Bare(), rel(s.cfg.Root, it.Path))
		return s.publish(it)
	}
	if found, waiters := s.deadlocksOf(it); len(found) > 0 && !acceptYes {
		return Refusal{fmt.Errorf("%s\nNothing was accepted. Point the dependency the other way (%s),\n"+
			"rewrite the body so it stands on the trunk alone, or accept anyway: drover accept %s --yes",
			strings.Join(found, "\n"), flipDeps(it.ID.Bare(), waiters), it.ID.Bare())}
	}
	n := stopBlockLen(lines, at, s.cfg.Intake.StopMarker)
	// Three-index slice: appending onto lines[:at] would otherwise write
	// through the backing array and shift the file under itself.
	kept := append(lines[:at:at], lines[at+n:]...)
	if err := s.writeBody(it, kept); err != nil {
		return err
	}
	fmt.Printf("accepted: %s (stop marker deleted from %s)\n",
		it.ID.Bare(), rel(s.cfg.Root, it.Path))
	for _, dir := range s.citedDirs(strings.Join(kept[bodyStart(kept):], "\n")) {
		fmt.Printf("note: %s cites the directory %s; under drive --jobs it waits for any item citing a path below it\n",
			it.ID.Bare(), dir)
	}
	if !s.mirroring() {
		return nil
	}
	// The item in hand still holds the marker, and the mirror publishes and
	// labels from the body as it now stands on disk.
	if err := s.reload(); err != nil {
		return err
	}
	if it, err = s.item(bare); err != nil {
		return err
	}
	return s.publish(it)
}

// prefixedHint adds, to the refusal for a planned id in a project whose items
// all carry a namespace prefix, the prefixed ids ending in that number. An
// operator typing 35 for E35 meant an item that exists, and a refusal naming
// only the planned range leaves them to work out which.
func (s *session) prefixedHint(bare string, err error) error {
	id, perr := backlog.ParseID(bare, s.cfg.Prefixes())
	if perr != nil || !id.Planned() || len(s.backlog.Items) == 0 {
		return err
	}
	var near []string
	for _, it := range s.backlog.Items {
		if it.ID.Planned() {
			return err
		}
		if it.ID.Num == id.Num && it.ID.Sub == id.Sub {
			near = append(near, it.ID.Bare())
		}
	}
	if len(near) == 0 {
		return err
	}
	return fmt.Errorf("%w.\nEvery item here carries a prefix, and %s %s in %s: drover accept %s",
		err, strings.Join(near, " and "), plural(len(near), "ends", "end"), bare, near[0])
}

// citedDirs is every path citedPaths finds in body that is a directory in the
// project tree. Dispatch holds such an item apart from every item citing a path
// below it, and cannot tell a directory named for context from one the item
// will edit, so accept tells the writer while the body is still theirs to change.
func (s *session) citedDirs(body string) []string {
	var dirs []string
	for _, p := range citedPaths(body) {
		if fi, err := os.Stat(filepath.Join(s.cfg.Root, p)); err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

var unacceptCmd = &cobra.Command{
	Use:   "unaccept <id>",
	Short: "Restore the stop marker, returning an item to a scaffold.",
	Long: `The withdrawal of accept, for a contract that turned out not to be one. The
whole block goes back at the top of the body where intake writes it, marker and
explanation both, so an item accepted and then unaccepted is byte-identical to
the scaffold it came from and draft will take it again. A body whose block a
human deleted by hand gets the explanation back with the marker.

Idempotent, and it refuses an item the ledger records done: an item that merged
is not a scaffold, and unrunning a completion is unmark-merged's job.`,
	Example: `  drover unaccept B1                # back to a scaffold; draft will take it again
  drover unaccept B1                # again: already a scaffold, exit 0

  drover unaccept B1                # on a merged item: refused, exit 3
  drover unmark-merged B1           # withdraw the completion first, then unaccept`,
	Args: needID("unaccept <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		if err := s.markerConfigured(); err != nil {
			return err
		}
		id := it.ID.Bare()
		if s.done(id) {
			return Refusal{fmt.Errorf(
				"%s is recorded done, so it is merged work rather than a scaffold.\nWithdraw the completion first: drover unmark-merged %s", id, id)}
		}
		lines, err := bodyLines(it.Path)
		if err != nil {
			return err
		}
		marker := s.cfg.Intake.StopMarker
		if markerLine(strings.Join(lines, "\n"), marker) >= 0 {
			fmt.Printf("already a scaffold: %s carries the stop marker (%s)\n",
				id, rel(s.cfg.Root, it.Path))
			s.mirrorAround(it)
			return nil
		}
		at := bodyStart(lines)
		restored := append(lines[:at:at], append(stopBlock(marker), lines[at:]...)...)
		if err := s.writeBody(it, restored); err != nil {
			return err
		}
		fmt.Printf("unaccepted: %s (stop marker restored in %s)\n", id, rel(s.cfg.Root, it.Path))
		s.mirrorReloaded(id, false)
		return nil
	},
}

// markerConfigured refuses both verbs in a project that declares no marker.
// Nothing here has a marker to write, delete or restore, and inventing one the
// config does not name plants a line no other command reads: a scaffold whose
// marker `run` cannot see is an item that launches with an empty numbered list.
func (s *session) markerConfigured() error {
	if s.cfg.Intake.StopMarker != "" {
		return nil
	}
	return Refusal{errors.New(
		"this project declares no [intake] stop_marker, so there is no marker to write, accept or restore.\nSet one in .drover.toml if scaffolded items should stay inert until a human accepts them")}
}

// stopBlockLen is how many lines of the block intake wrote survive at index
// at. The marker line is the only one guaranteed to be there.
//
// Where the block ends is decided by matching intake's own continuation text,
// not by scanning to the next blank line. A marker somebody typed by hand can
// have real prose directly under it, and a scan to the blank would delete that
// paragraph; matching the text removes only lines drover authored and leaves
// anything else where its author put it.
func stopBlockLen(lines []string, at int, marker string) int {
	block := stopBlock(marker)
	n := 1
	for n < len(block) && at+n < len(lines) && lines[at+n] == block[n] {
		n++
	}
	return n
}

// bodyLines reads a body as lines, splitting on "\n" so that joining them back
// reproduces the file exactly, trailing newline included. Both verbs edit a few
// lines of somebody's prose and must be unable to touch the rest.
func bodyLines(path string) ([]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path came from the backlog
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// writeBody rewrites a body in place and records the digest of what it wrote.
// The mode is only applied on creation, so a file already on disk keeps
// whatever permissions it had.
func (s *session) writeBody(it backlog.Item, lines []string) error {
	written := []byte(strings.Join(lines, "\n"))
	if err := os.WriteFile(it.Path, written, 0o600); err != nil {
		return err
	}
	return s.wroteBody(it.ID.Bare(), written)
}

// bodyStart is the index of the first body line: past the closing frontmatter
// fence and the blank lines under it, which is where intake plants the marker.
// A file whose frontmatter does not parse gets index 0 rather than an error —
// the backlog would have refused it long before here.
func bodyStart(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		start := i + 1
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
		return start
	}
	return 0
}

func init() {
	acceptCmd.Flags().BoolVar(&acceptYes, "yes", false,
		"accept past a refusal about the body's content (a citation only a waiting item's branch has); never past the stop-marker or actor checks")
	root.AddCommand(acceptCmd, unacceptCmd)
}
