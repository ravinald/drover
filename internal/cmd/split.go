package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/graph"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var (
	splitInto string
	splitFile string
)

var splitCmd = &cobra.Command{
	Use:   `split <id> --into <kind> "<title>" -f <proposal>`,
	Short: "Move work a contract never named into its own item, blocking this one.",
	Long: `A review finding whose fix changes a contract the numbered requirements never
named has one way open to it without this: back to the branch, where the
architectural work lands inside an item nobody accepted it for. That is how a
bug about an audit row becomes a storage rewrite reviewed one consequence at a
time.

split writes the second contract. It scaffolds the work as its own item with the
proposal as its body, then adds that id to this item's deps, which leaves this
one blocked and unmerged until the operator accepts the new item and it lands.
Nothing else in drover changes: ` + "`" + `status` + "`" + ` already reports an unmerged dep as
blocked, and ` + "`" + `drive` + "`" + ` already skips a blocked item.

What it does not do is the half that matters. The scaffold is inert, because
` + "`" + `run` + "`" + ` refuses it until a human writes the numbered requirements and runs
` + "`" + `drover accept` + "`" + `, which is one of the commands a launched session cannot reach. So
a reviewer proposes the split and an operator decides it, and no session can
park an accepted contract on its own authority.

It moves no code. The commits stay on the branch they were written on; the
proposal names them and the operator cherry-picks, or the new run writes it
again. It moves no blame either: a regression this branch introduced is still
this branch's to answer for, and what moves is the implementation.

The proposal is prose and should answer six things: the subsystem, draft
requirements, the commits and files to move, which way the dependency points,
what evidence to keep, and what condition resumes this item.`,
	Example: `  drover split B58 --into bug "atomic local publication" -f proposal.md

  Then a human decides, because a session cannot accept its own contract:
    $EDITOR .drover/backlog/B59-atomic-local-publication.md
    drover accept B59
    drover run B59

  drover status          # B58 reads blocked until B59 merges
  drover show B58 --json # its deps carry the new id

Safety: local writes. Scaffolds one item under the backlog, rewrites the deps
line of another and appends a body digest row for it to the ledger, and reaches
no tracker. Under [ids] allocator = "git" it first pushes refs/drover/ids to
[ids] remote.`,
	Args: cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if err := s.markerConfigured(); err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		title := strings.TrimSpace(args[1])
		if title == "" {
			return Refusal{fmt.Errorf("the title is empty, and it is what names the file")}
		}
		if splitInto == "" {
			return Refusal{fmt.Errorf("split needs the kind of item to write: --into <kind>")}
		}
		if splitFile == "" {
			return Refusal{fmt.Errorf("split needs the proposal: -f <file>, or -f - for stdin")}
		}
		proposal, err := readEvidence(splitFile, os.Stdin)
		if err != nil {
			return err
		}
		if strings.TrimSpace(proposal) == "" {
			return Refusal{fmt.Errorf(
				"%s is empty: a split with no proposal parks an item and tells the operator nothing about what it is deciding", splitFile)}
		}
		if !s.cfg.Known(backlog.KeyDeps) {
			return Refusal{fmt.Errorf(
				"this project does not declare %q in [frontmatter], so no item can block on another.\n"+
					"Add it to required or optional in .drover.toml", backlog.KeyDeps)}
		}

		byKind := map[string]string{}
		for prefix, k := range s.cfg.IDs.Namespaces {
			byKind[k] = prefix
		}
		prefix, ok := byKind[splitInto]
		if !ok {
			return Refusal{fmt.Errorf("unknown kind %q (declared: %s)",
				splitInto, strings.Join(sortedKeys(byKind), ", "))}
		}

		slug := slugify(title)
		if slug == "" {
			return Refusal{fmt.Errorf("the title yields no filename component: %q", title)}
		}
		body, err := s.scaffoldWith(splitInto, title, proposal)
		if err != nil {
			return err
		}
		// The properties earlier passes established while the work lived on the
		// original item describe the code that is moving, so they move with it.
		// Copied rather than cut: the original's own guarantees still bound
		// whatever of it stays behind.
		if g := (prompts.Data{Body: it.Body}).Guarantees(); g != "" {
			body += "\n" + prompts.GuaranteeHeading + "\n\n" + g + "\n"
		}
		// The new item depends on nothing, so the only cycle reachable here is
		// the one where the original already depends on this id. Checked anyway:
		// the id comes from the tree, the shelf and possibly a remote counter,
		// and an id reused after a hand edit would otherwise write a dependency
		// on itself.
		id, path, err := s.mint(prefix, slug, body, func(id backlog.ID) error {
			return s.acyclic(it, id.Bare())
		})
		if err != nil {
			return err
		}

		// After the scaffold, never before. A deps line naming an item that does
		// not exist blocks the original on nothing anybody can run, and `check`
		// reports it as a dep it could not read.
		if err := s.blockOn(it, id.Bare()); err != nil {
			return err
		}
		s.mirrorReloaded(it.ID.Bare(), false)

		fmt.Printf("\nsplit %s -> %s  (%s)\n\n", it.ID.Bare(), id.Bare(), splitInto)
		fmt.Printf("  %s  the work, with your proposal as its body\n", rel(s.cfg.Root, path))
		fmt.Printf("  %s  now blocked on %s\n\n", rel(s.cfg.Root, it.Path), id.Bare())
		fmt.Printf("The commits stay where they are: %s names them and somebody moves them.\n\n", id.Bare())
		fmt.Printf("NEXT — a human decides, because a session cannot accept its own contract:\n")
		fmt.Printf("  $EDITOR %s\n", rel(s.cfg.Root, path))
		fmt.Printf("  drover accept %s\n", id.Bare())
		fmt.Printf("  drover run %s --dry-run\n", id.Bare())
		return nil
	},
}

// scaffoldWith is the scaffold a split writes: the frontmatter and the stop
// marker, then the proposal in place of the placeholder prose.
//
// In place of, not after. scaffoldBody ends in an empty numbered list for a
// human to fill, and a body carrying that below a real one has requirements 1
// and 2 blank and requirement 3 written, which is the state the marker exists to
// prevent rather than one more thing for the operator to tidy.
func (s *session) scaffoldWith(kind, title, proposal string) (string, error) {
	full := s.scaffoldBody(kind, title, map[string]string{s.cfg.Severity.Key: seedSeverity(kind, "")})
	block := stopBlock(s.cfg.Intake.StopMarker)
	last := block[len(block)-2]
	_, after, ok := strings.Cut(full, last+"\n")
	if !ok {
		// scaffoldBody and stopBlock are the same package and the same call, so
		// this is unreachable until somebody changes one of them. Reported
		// rather than papered over: the alternative is a scaffold with no stop
		// marker, which runs.
		return "", fmt.Errorf("the scaffold carries no stop marker to write below")
	}
	head := full[:len(full)-len(after)]
	return head + "\n" + strings.TrimSpace(proposal) + "\n", nil
}

// blockOn widens one item's deps line, the way link widens its closes line.
func (s *session) blockOn(it backlog.Item, dep string) error {
	deps := it.Deps()
	if slices.Contains(deps, dep) {
		return nil
	}
	deps = append(deps, dep)
	lines, err := bodyLines(it.Path)
	if err != nil {
		return err
	}
	written, err := setFrontmatter(lines, backlog.KeyDeps, strings.Join(deps, " "), s.cfg.DeclaredKeys())
	if err != nil {
		return err
	}
	return s.writeBody(it, written)
}

// acyclic refuses a dependency the backlog could not satisfy in any order.
//
// The graph is read from the tree rather than tracked, so the check costs a
// backlog walk and catches the case an id reused by hand would otherwise write:
// an item blocked on itself, which `status` reports as blocked forever and no
// run can clear.
func (s *session) acyclic(it backlog.Item, dep string) error {
	deps := map[string][]string{}
	for _, other := range s.backlog.Items {
		deps[other.ID.Bare()] = other.Deps()
	}
	id := it.ID.Bare()
	deps[id] = append(append([]string{}, it.Deps()...), dep)
	for _, cycle := range graph.New(deps).Cycles() {
		if slices.Contains(cycle, id) {
			return Refusal{fmt.Errorf(
				"refusing to write: blocking %s on %s closes a cycle (%s). Nothing was written",
				id, dep, strings.Join(cycle, " > "))}
		}
	}
	return nil
}

func init() {
	splitCmd.Flags().StringVar(&splitInto, "into", "",
		"the kind of item to write, from [ids] namespaces (required)")
	splitCmd.Flags().StringVarP(&splitFile, "file", "f", "",
		"the extraction proposal, or - for stdin (required)")
	localFlag(splitCmd)
	root.AddCommand(splitCmd)
}
