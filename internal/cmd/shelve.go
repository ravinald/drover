package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/spf13/cobra"
)

var shelveCmd = &cobra.Command{
	Use:   "shelve <id>",
	Short: "Retire a body to the shelf without recording it as done.",
	Long: `An item written, reviewed and deliberately not going to be done is not a
completion. Recording it as one would be a false row in an append-only ledger,
so the mechanism is a directory: the body moves to {shelf} and keeps its
filename, which keeps its id spent and its text readable.

Nothing on the shelf renders, enters the graph or moves a count. intake still
allocates above the highest shelved id, so a retired id is never reissued.

It refuses three states rather than reporting them afterwards: a project with no
shelf configured, an id that would end up holding a body in both directories,
and an id another item still declares on its deps line.

Under [backlog] mirror = "github", the moved body's issue key is read and, when
it names an issue drover opened, that issue trades its drover: state label for
drover:deferred and closes as not planned, with a comment naming unshelve. A
tracker that cannot be reached costs the mirror and never the move: the body
still moves, the command exits 0, and check reports MIRROR SHELVED until the
write lands. A body whose frontmatter will not parse moves without the write.`,
	Example: `  drover shelve F3                  # F03-<slug>.md moves to {shelf}, name intact
  drover check                      # the shelf is consistent; nothing dangles

  drover unshelve F3                # bring it back into the backlog

  Refused when another item depends on it: shelving would leave each dependent
  MISSING DEP at the next check, so drop the dependency first or shelve them too.

  A body that will not parse still moves, which is the mirror of unshelve:
  retiring one is the case this verb exists for. An id with no file behind it is
  refused, because there is nothing to move.`,
	Args: needID("shelve <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		// CodeBy path, not by parsed item: a body that will not read is the case this
		// verb exists for, and resolving through the backlog map alone reported it
		// as a file nobody had written.
		itemID, live, err := s.body(args[0])
		if err != nil {
			return err
		}
		dir, err := s.shelfDir()
		if err != nil {
			return err
		}
		id := itemID.Bare()
		if path, ok := s.shelf.Path(itemID); ok {
			return Refusal{fmt.Errorf(
				"%s already has a body on the shelf: %s.\nThat is the SHELF CLASH check reports; rename or remove one before moving %s",
				id, rel(s.cfg.Root, path), rel(s.cfg.Root, live))}
		}
		if on := s.dependents(id); len(on) > 0 {
			return Refusal{fmt.Errorf(
				"%s is on the deps line of %s.\nShelving it would leave %s MISSING DEP at the next check. Drop the dependency, or shelve %s too",
				id, strings.Join(on, ", "),
				plural(len(on), "that item", "those items"), plural(len(on), "it", "them"))}
		}
		dest, err := moveBody(live, dir)
		if err != nil {
			return err
		}
		fmt.Printf("shelved: %s -> %s\n", id, rel(s.cfg.Root, dest))
		s.mirrorShelved(itemID, dest)
		return nil
	},
}

var unshelveCmd = &cobra.Command{
	Use:   "unshelve <id>",
	Short: "Bring a shelved body back into the backlog.",
	Long: `The reverse of shelve, for work that turned out to be worth doing after all.
The body moves out of {shelf} and back into {backlog},
keeping its filename, so the id and the slug survive the round trip and the
branch an item runs on is unchanged.

Ids are the whole reason this refuses: a live body already holding this id would
make the revived one take over its identity, which is the SHELF CLASH state
check reports after the fact.

Under [backlog] mirror = "github", the issue the body names is brought back in
line with the file the way accept would: reopened, its requirements rewritten,
drover:deferred replaced by the live state's label, and any blocked-by link it
lacks added. A scaffold's issue is reopened and relabeled drover:draft, but
its requirements are left as they were: the draft on the shelf is published by
accept, never by unshelve. A tracker write that fails is reported and the body
still moves.`,
	Example: `  drover unshelve F3                # F03-<slug>.md moves back to {backlog}
  drover status                     # it is back in the backlog

  Nothing on the shelf is parsed, so a body that will not read still moves. The
  first command to say so is check, once it is live again.`,
	Args: needID("unshelve <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if _, err := s.shelfDir(); err != nil {
			return err
		}
		id, err := backlog.ParseID(args[0], s.cfg.Prefixes())
		if err != nil {
			return err
		}
		path, ok := s.shelf.Path(id)
		if !ok {
			return fmt.Errorf("%s is not on the shelf (no body at %s/%s-*.md)",
				id.Bare(), s.cfg.Paths.Shelf, id.Stem())
		}
		if live, ok := s.bodyPath(id); ok {
			return Refusal{fmt.Errorf(
				"%s already has a live body: %s.\nReviving %s would take over that item's identity; rename or remove one first",
				id.Bare(), rel(s.cfg.Root, live), rel(s.cfg.Root, path))}
		}
		dest, err := moveBody(path, s.backlog.Dir)
		if err != nil {
			return err
		}
		fmt.Printf("unshelved: %s -> %s\n", id.Bare(), rel(s.cfg.Root, dest))
		s.mirrorReloaded(id.Bare(), true)
		return nil
	},
}

// shelfDir resolves the shelf, refusing the two configurations under which a
// move cannot mean what it says: no shelf at all, and a shelf pointing at the
// bodies directory, where the rename would be a no-op reported as a success.
func (s *session) shelfDir() (string, error) {
	if s.cfg.Paths.Shelf == "" {
		return "", Refusal{errors.New(
			"this project declares no shelf.\nSet [paths] shelf in .drover.toml to the directory retired bodies belong in, then retry")}
	}
	dir := s.cfg.Abs(s.cfg.Paths.Shelf)
	if sameDir(dir, s.cfg.Abs(s.cfg.Paths.Backlog)) {
		return "", Refusal{fmt.Errorf(
			"%s, so there is nowhere to move to.\nPoint shelf at its own directory; that is the SHELF IS BACKLOG finding check reports",
			s.shelfCollision())}
	}
	return dir, nil
}

// dependents is every item declaring this id on its deps line. Naming them is
// the difference between a refusal an operator can act on and one that sends
// them to grep.
func (s *session) dependents(bare string) []string {
	var out []string
	for _, it := range s.backlog.Items {
		for _, dep := range it.Deps() {
			if dep == bare {
				out = append(out, it.ID.Bare())
				break
			}
		}
	}
	return out
}

// moveBody renames a body into dir under the same filename, which is what keeps
// the id and the slug across the move. os.Rename does not cross filesystems;
// both directories live in one project tree, and the error says so when it does.
func moveBody(path, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("move %s to %s: %w (a shelf on another filesystem cannot be renamed into; move it by hand)", path, dest, err)
	}
	return dest, nil
}

func init() {
	root.AddCommand(shelveCmd, unshelveCmd)
}
