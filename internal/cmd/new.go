package cmd

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

var (
	newSev     string
	newFoundIn string
	newFile    string
)

var newCmd = &cobra.Command{
	Use:   "new <kind> <title> [-f <file>]",
	Short: "Write a scaffolded item, with no tracker involved.",
	Long: `Allocates the next free id in the namespace matching <kind> and writes a
scaffold there: the frontmatter this project declares, the stop marker, and an
empty numbered list for a human to fill in.

It is ` + "`drover intake`" + ` without the issue. A project with no tracker had no way to
create an item at all before this, and a project with one still wants it: work
that starts as a decision rather than as a bug report has no issue to take in.

The scaffold is not runnable. ` + "`run`" + ` refuses it and so does ` + "`--dry-run`" + `, until a
human writes the requirements and runs ` + "`drover accept`" + `. That is the same boundary
intake's scaffolds sit behind, for the same reason: a numbered list nobody wrote
is a checklist nobody can falsify.

-f writes a description into the scaffold, below the stop marker and the
--found-in line and above the placeholder prose: the file and line, the command
that reproduces the defect with its actual output, and what is wrong. A review
finding filed without one leaves its description in the audit note of the item
under review, which nobody reads when writing the new item's requirements. The
marker stays, so the item is no more runnable than without it. An empty text is
refused, and so is one carrying the stop marker, which would survive accept
deleting the first.`,
	Example: `  drover new bug "the resolver drops a trailing dot"
  drover new feature "publish a release binary" --sev H
  drover new bug "re-vet skips proxy-cached objects" --found-in F41 -f -   # a NEW-ITEM finding, described on stdin

  Then write the requirements and accept them:
    $EDITOR .drover/backlog/B14-the-resolver-drops-a-trailing-dot.md
    drover accept B14

  --sev seeds the frontmatter rank. Omitted, a bug scaffolds M and everything
  else scaffolds unranked, which is what intake does with an issue the tracker
  never ranked.`,
	Args: cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		// A scaffold whose marker nothing reads is an item that runs with two
		// empty numbered lines, which is the state the marker exists to prevent.
		if err := s.markerConfigured(); err != nil {
			return err
		}
		kind, title := args[0], args[1]
		if strings.TrimSpace(title) == "" {
			return Refusal{fmt.Errorf("the title is empty, and it is what names the file")}
		}
		// Refused rather than recorded, for the reason `follow-up` refuses one: a
		// rank the vocabulary does not know ranks last everywhere it is read, so
		// a typo files a critical item that sorts below every ranked one.
		if newSev != "" && sev.Of(newSev) == sev.Unranked {
			return Refusal{fmt.Errorf("unknown severity %q (known: %s)",
				newSev, strings.Join(sev.Letters(), ", "))}
		}

		byKind := map[string]string{}
		for prefix, k := range s.cfg.IDs.Namespaces {
			byKind[k] = prefix
		}
		prefix, ok := byKind[kind]
		if !ok {
			return Refusal{fmt.Errorf("unknown kind %q (declared: %s)",
				kind, strings.Join(sortedKeys(byKind), ", "))}
		}

		slug := slugify(title)
		if slug == "" {
			return Refusal{fmt.Errorf("the title yields no filename component: %q", title)}
		}
		rank := newSev
		if rank == "" {
			rank = seedSeverity(kind, "")
		} else {
			rank = sev.Of(rank).Letter()
		}
		description, err := s.newDescription()
		if err != nil {
			return err
		}
		body := s.scaffoldBody(kind, title, map[string]string{s.cfg.Severity.Key: rank})
		if newFoundIn != "" {
			found, err := s.item(newFoundIn)
			if err != nil {
				return err
			}
			body = withProvenance(body, found.ID.Bare(), found.Title())
		}
		if description != "" {
			body = aboveRole(body, description+"\n\n")
		}
		id, path, err := s.mint(prefix, slug, body, nil)
		if err != nil {
			return err
		}

		fmt.Printf("\nscaffolded %s -> %s  (%s)\n\n", id.Bare(), rel(s.cfg.Root, path), kind)
		if newFoundIn != "" {
			fmt.Printf("  found while working %s, which the body now names\n\n", newFoundIn)
		}
		for _, dir := range s.topDirs(body) {
			fmt.Printf("  note: %s\n\n", dirNote(id.Bare(), dir))
		}
		fmt.Printf("NEXT — a human writes the numbered requirements:\n")
		fmt.Printf("  $EDITOR %s\n", rel(s.cfg.Root, path))
		fmt.Printf("  drover accept %s\n", id.Bare())
		fmt.Printf("  drover run %s --dry-run\n", id.Bare())
		return nil
	},
}

// withProvenance names the item a finding came out of, in the body rather than in
// the frontmatter.
//
// Not deps: that means "must merge before this runs", and a defect found while
// reviewing an item is rarely blocked by it — reading provenance as a dependency
// would block work that could ship today. Not a new frontmatter key either,
// because [frontmatter] is a closed schema and a key drover reads that a project
// has not declared is a key no item may carry.
//
// The prose is the record because the prose is what the next run reads. The cap's
// own refusal tells a reviewer to file the finding and name the id in their
// report, and a report reaches the operator and nobody else; this reaches whoever
// opens the item.
func withProvenance(body, id, title string) string {
	return aboveRole(body, fmt.Sprintf(provenanceFormat, id, title)+"\n\n")
}

// provenanceFormat is the line withProvenance writes and foundIn reads back. One
// string for both, because draft finds the audit notes through it and a writer
// reworded alone would leave every scaffold after it with no notes to show.
const provenanceFormat = "Found while working %s, %s."

// foundIn is the id on a body's provenance line, or "" for a body with none.
// Only the first line below the frontmatter and the stop block is read, because
// that is where withProvenance writes it: a description quoting another
// finding's provenance line sits below, and must not be taken for this one's.
func foundIn(body, marker string) string {
	skip := map[string]bool{"": true}
	for _, line := range stopBlock(marker) {
		skip[strings.TrimSpace(line)] = true
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				lines = lines[i+1:]
				break
			}
		}
	}
	parts := strings.Split(provenanceFormat, "%s")
	lead, sep, tail := parts[0], parts[1], parts[2]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if skip[line] {
			continue
		}
		rest, ok := strings.CutPrefix(line, lead)
		if !ok || !strings.HasSuffix(rest, tail) {
			return ""
		}
		if id, _, ok := strings.Cut(rest, sep); ok && id != "" {
			return id
		}
		return ""
	}
	return ""
}

// aboveRole puts text directly above the scaffold's placeholder prose, which is
// below the stop marker, so whatever lands there is read before the role and
// cannot make the item runnable.
func aboveRole(body, text string) string {
	marker := "Act as a <role>."
	if i := strings.Index(body, marker); i >= 0 {
		return body[:i] + text + body[i:]
	}
	return body + "\n" + text
}

// newDescription reads -f, or returns "" when it was not given.
//
// An empty text is refused rather than treated as no flag: a reviewer who meant
// to describe the finding and piped nothing gets told so, instead of filing the
// bare scaffold the flag exists to replace. A text holding the stop marker is
// refused because accept deletes the first marker it finds, and the second one
// would leave an accepted item that run still refuses.
func (s *session) newDescription() (string, error) {
	if newFile == "" {
		return "", nil
	}
	text, err := readEvidence(newFile, os.Stdin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", Refusal{fmt.Errorf(
			"%s is empty, so there is no description to write. Omit -f to write a scaffold with no description", newFile)}
	}
	if marker := s.cfg.Intake.StopMarker; markerLine(text, marker) >= 0 {
		return "", Refusal{fmt.Errorf(
			"%s carries the stop marker %q: the scaffold already has one, and a second would survive accept deleting the first", newFile, marker)}
	}
	return trimBlankLines(text), nil
}

// trimBlankLines drops the blank lines around text and the whitespace at its
// end, and nothing else. TrimSpace would also strip the first line's indent,
// and a finding that opens with an indented command and its output, the form
// the review guidance asks for, would lose its code block.
func trimBlankLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace)
}

func init() {
	newCmd.Flags().StringVar(&newFoundIn, "found-in", "",
		"the item this was found while working, named in the scaffold body")
	newCmd.Flags().StringVarP(&newFile, "file", "f", "",
		"a description written into the scaffold above the placeholder, - for stdin")
	newCmd.Flags().StringVar(&newSev, "sev", "",
		"severity rank: C, H, M or L (default: M for a bug, unranked otherwise)")
	localFlag(newCmd)
	root.AddCommand(newCmd)
}
