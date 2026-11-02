package cmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/spf13/cobra"
)

var foldInto string

var foldCmd = &cobra.Command{
	Use:   "fold <from> --into <to>",
	Short: "Move one item's numbered requirements onto another's list, then shelve it.",
	Long: `Two items written as two that are one change cost two implementing sessions,
two reviews and a merge that conflicts with itself. check and accept report a
pair whose cited lines overlap; fold is the edit that makes them one.

The numbered requirements of <from> are appended to the list of <to>,
renumbered after its last, through the write amend makes: the replaced list is
kept, and the body row and an amend row naming <from> as where the
requirements moved from are appended to the ledger. <from> is then shelved.

It prints every moved line that names either id or a requirement number, since
the move renumbered them and a reference to "R2 of <from>" now means something
else. Rewrite those by hand, then record the edit with drover touch <to>.

Refused, with exit 3 and nothing written, when either item is a scaffold or has
merged, when the deps line of <to> names <from> (the dependency has to land
first, so they are not one change), when another item depends on <from>, when
the shelf cannot take it, and when the combined list would pass [backlog]
fold_max.`,
	Example: `  drover fold 9 --into 7            # 7 gains 9's requirements as R5..R7; 9 is shelved
  drover show 7                     # read the combined list
  drover touch 7 --reason "rewrote references the fold renumbered"`,
	Args: needID("fold <from> --into <to>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if foldInto == "" {
			return Refusal{fmt.Errorf("fold needs the item that keeps the requirements: --into <to>")}
		}
		from, err := s.item(args[0])
		if err != nil {
			return err
		}
		to, err := s.item(foldInto)
		if err != nil {
			return err
		}
		fid, tid := from.ID.Bare(), to.ID.Bare()
		if fid == tid {
			return Refusal{fmt.Errorf("fold names %s twice: --into names the other item", fid)}
		}
		for _, it := range []backlog.Item{from, to} {
			if err := s.contractOpen(it, "fold"); err != nil {
				return err
			}
		}
		if slices.Contains(to.Deps(), fid) {
			return Refusal{fmt.Errorf("%s's deps line names %s, so %s has to land first and the two are not one change.\n"+
				"Drop the dependency first if they are", tid, fid, fid)}
		}
		if on := slices.DeleteFunc(s.dependents(fid), func(id string) bool { return id == tid }); len(on) > 0 {
			return Refusal{fmt.Errorf("%s is on the deps line of %s, and shelving it would leave %s MISSING DEP.\n"+
				"Point that dependency at %s first", fid, strings.Join(on, ", "), plural(len(on), "that item", "those items"), tid)}
		}
		limit := s.cfg.BacklogFoldMax()
		have, add := len(to.Requirements()), len(from.Requirements())
		if have+add > limit {
			return Refusal{fmt.Errorf("%s holds %d requirements and %s %d, and %d is past [backlog] fold_max %d.\n"+
				"Narrow one of them first, or raise fold_max in .drover.toml", tid, have, fid, add, have+add, limit)}
		}
		dir, err := s.shelfDir()
		if err != nil {
			return err
		}
		if path, ok := s.shelf.Path(from.ID); ok {
			return Refusal{fmt.Errorf("%s already has a body on the shelf: %s; rename or remove it before folding %s",
				fid, rel(s.cfg.Root, path), fid)}
		}

		toList, err := numberedList(to)
		if err != nil {
			return err
		}
		fromList, err := numberedList(from)
		if err != nil {
			return err
		}
		moved := renumber(fromList, have)
		list := strings.TrimRight(toList, "\n") + "\n" + moved
		if err := s.replaceList(to, list, have+add, "folds "+fid+" into "+tid, fid); err != nil {
			return err
		}
		dest, err := moveBody(from.Path, dir)
		if err != nil {
			return fmt.Errorf("%s now carries %s's requirements, and %s would not move to the shelf: %w; shelve it with drover shelve %s",
				tid, fid, fid, err, fid)
		}
		fmt.Printf("shelved: %s -> %s\n", fid, rel(s.cfg.Root, dest))
		s.mirrorShelved(from.ID, dest)
		if refs := movedReferences(moved, fid, tid); len(refs) > 0 {
			fmt.Printf("These moved lines name an id or a requirement number the fold may have changed:\n")
			for _, l := range refs {
				fmt.Printf("  %s\n", l)
			}
			fmt.Printf("Rewrite them in %s, then record it: drover touch %s --reason \"...\"\n", rel(s.cfg.Root, to.Path), tid)
		}
		return nil
	},
}

// numberedList is the text of an item's numbered list, as amend replaces it.
func numberedList(it backlog.Item) (string, error) {
	raw, err := os.ReadFile(it.Path)
	if err != nil {
		return "", err
	}
	_, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", it.Path, err)
	}
	lines := strings.Split(body, "\n")
	start, end, err := requirementSpan(lines)
	if err != nil {
		return "", Refusal{fmt.Errorf("%s: %w", it.Path, err)}
	}
	return strings.Join(lines[start:end], "\n"), nil
}

var topNumber = regexp.MustCompile(`^(\d+)\. `)

// renumber renumbers list's top-level "N. " lines from after+1, in order, and
// leaves every other line, a continuation included, as written.
func renumber(list string, after int) string {
	lines := strings.Split(strings.TrimRight(list, "\n"), "\n")
	n := after
	for i, l := range lines {
		if m := topNumber.FindStringSubmatch(l); m != nil {
			n++
			lines[i] = strconv.Itoa(n) + ". " + strings.TrimPrefix(l, m[0])
		}
	}
	return strings.Join(lines, "\n")
}

// numberRef is a reference to a requirement by number, the kind a
// renumbering makes wrong.
var numberRef = regexp.MustCompile(`\bR\d+\b|(?i)\brequirements? \d+`)

// movedReferences is each moved line that names either id or a requirement
// number.
func movedReferences(moved, from, to string) []string {
	ids := regexp.MustCompile(`\b(` + regexp.QuoteMeta(from) + `|` + regexp.QuoteMeta(to) + `)\b`)
	var out []string
	for _, l := range strings.Split(moved, "\n") {
		if ids.MatchString(l) || numberRef.MatchString(l) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

func init() {
	foldCmd.Flags().StringVar(&foldInto, "into", "", "the item that keeps the combined list")
	root.AddCommand(foldCmd)
}
