package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var invariantFile string

var invariantCmd = &cobra.Command{
	Use:   "invariant <id> -f <file>",
	Short: "Record an invariant this code must hold, for every pass after this one.",
	Long: `An item body carries three things and only this one accumulates.

The numbered requirements are the contract and are frozen once written: ` + "`request-changes`" + `
refuses a write that would change their count, because a reviewer who can edit
them while auditing against them is auditing a target they moved. The ` + "`" + prompts.ChangesHeading + "`" + `
section is replaced every pass, because the next run reads it as current
instructions and cannot tell a closed finding from an open one.

So a property the third round discovered had nowhere to live. It went back to
the branch as a finding, the fix closed the case that exposed it, and the fourth
round's request-changes replaced the text that named it. The next run meets the same
guarantee as a fresh surprise and trades one violation of it for another, which
is the shape of a review loop that converges without terminating.

invariant writes under ` + "`" + prompts.GuaranteeHeading + "`" + `, appending rather than replacing, and
it touches nothing else in the file. Both prompts render the section above the
findings: the implementing agent reads what must stay true before it reads what
broke, and the reviewer gets a checklist that grows as the layer's shape is
learned.

Write a property, not a defect. "Replacing the object preserves the mode an
operator set" is a guarantee; "the fix dropped the mode to 0644" is the finding
that taught it, and that one belongs in request-changes.`,
	Example: `  drover invariant B1 -f invariants.md
  printf -- '- A failed write leaves no partial object.\n' | drover invariant B1 -f -

  drover run B1 --dry-run | grep -A20 'Invariants'   # what the next run will read

Safety: local writes. Appends to one section of the item body, refuses to change
the numbered requirements, appends a body digest row to the ledger, and reaches
no tracker.`,
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
		if invariantFile == "" {
			return Refusal{fmt.Errorf("invariant needs the property: -f <file>, or -f - for stdin")}
		}
		text, err := readEvidence(invariantFile, os.Stdin)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return Refusal{fmt.Errorf("%s is empty: a guarantee nobody wrote down is one the next pass rediscovers", invariantFile)}
		}

		raw, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		head, body, err := splitFrontmatter(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}

		// Demoted for request-changes' reason: a top-level "N." anywhere in the body is
		// parsed as a requirement, so a numbered list of guarantees would enter
		// the contract the review audits against.
		section, demoted, sunk := demoteEvidence(strings.TrimSpace(text))
		next := appendSection(body, section)

		before := len(it.Requirements())
		after := len(backlog.Item{Body: next}.Requirements())
		if before != after {
			return Refusal{fmt.Errorf(
				"refusing to write: the body has %d numbered requirements and this would leave %d. "+
					"The requirements are the contract; a guarantee goes below the heading as prose or bullets", before, after)}
		}

		written := []byte(head + next + "\n")
		if err := os.WriteFile(it.Path, written, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", it.Path, err)
		}
		if err := s.wroteBody(it.ID.Bare(), written); err != nil {
			return err
		}
		fmt.Printf("recorded against %s -> %s (%d requirement(s) unchanged)\n",
			it.ID.Bare(), rel(s.cfg.Root, it.Path), after)
		reportDemotions(demoted, sunk)
		fmt.Printf("Every pass after this one reads it: drover run %s --dry-run\n", it.ID.Bare())
		return nil
	},
}

// appendSection adds body under the guarantee heading, keeping whatever is
// already there. The mirror of spliceSection, which replaces.
//
// A new section lands above the review heading rather than at the end of the
// file, so the properties that must hold are read before the findings that
// broke them. Absent that heading it goes last, which is where a body with no
// sections at all wants it.
func appendSection(text, body string) string {
	heading, before := prompts.GuaranteeHeading, prompts.ChangesHeading
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		section := heading + "\n\n" + body
		for i, l := range lines {
			if strings.TrimSpace(l) == before {
				out := append([]string{}, lines[:i]...)
				out = append(out, strings.Split(section, "\n")...)
				out = append(out, "")
				out = append(out, lines[i:]...)
				return strings.TrimRight(strings.Join(out, "\n"), "\n")
			}
		}
		return strings.TrimRight(text, "\n") + "\n\n" + section
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	kept := strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n")
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.Split(kept+gap(kept, body)+body, "\n")...)
	if end < len(lines) {
		out = append(out, "")
		out = append(out, lines[end:]...)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// gap is the separator between what a section already holds and what is being
// added: one newline when both sides are bullets, so a section built up over
// four passes stays one list rather than four.
func gap(kept, body string) string {
	if bullet(lastLine(kept)) && bullet(firstLine(body)) {
		return "\n"
	}
	return "\n\n"
}

func bullet(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
}

func firstLine(text string) string { return strings.SplitN(text, "\n", 2)[0] }

func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}

func init() {
	invariantCmd.Flags().StringVarP(&invariantFile, "file", "f", "",
		"the guarantee to record, or - for stdin (required)")
	root.AddCommand(invariantCmd)
}
