package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/graph"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var (
	splitInto   string
	splitFile   string
	splitAfter  bool
	splitBefore bool
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
proposal as its body, then writes the dependency between the two, in one of two
directions:

  --after   the default. This item waits on the new one: its deps line gains
            the new id, and it stays blocked and unmerged until the operator
            accepts the new item and it lands. Right when the new item's work
            stands on the trunk alone, which is the usual case: a shared
            subsystem this item tripped over.
  --before  the new item waits on this one: the scaffold's deps line names this
            item, which gains no dep and may merge first. Right when the new
            item's fix reaches code only this branch has written, because a run
            of the new item starts from the trunk and cannot touch it.

Get it wrong and the two deadlock: the new item cannot finish without code
that only merges with this one, and this one cannot merge before it. So under
--after split reads every path the proposal cites with a line part, and every
backticked name that looks like a function or type, against the trunk and
against this item's branch, and warns about each one only the branch has.
accept repeats the check on the new item and refuses until --yes, and check
reports the same cycle for any dependency, split or not. The names are a
heuristic: a whole-word git grep over the cited files on each ref.

The scaffold records the direction in a line under the stop marker, and
` + "`" + `show` + "`" + ` names it on both items. Nothing else in drover changes: ` + "`" + `status` + "`" + `
already reports an unmerged dep as blocked, and ` + "`" + `drive` + "`" + ` already skips a
blocked item.

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
requirements, the commits and files to move, which way the dependency points
(and so which flag), what evidence to keep, and what condition resumes this item.`,
	Example: `  drover split B58 --into bug "atomic local publication" -f proposal.md
  drover split B58 --into bug "unique index on slugs" -f p.md --before
                         # B59 needs B58's call sites, so B58 merges first

  Then a human decides, because a session cannot accept its own contract:
    $EDITOR .drover/backlog/B59-atomic-local-publication.md
    drover accept B59
    drover run B59

  drover status          # B58 reads blocked until B59 merges
  drover show B58 --json # its deps carry the new id

Safety: local writes. Scaffolds one item under the backlog and, under --after,
rewrites the deps line of another and appends a body digest row for it to the
ledger. Reaches no tracker. Under [ids] allocator = "git" it first pushes refs/drover/ids to
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
		var deps []string
		if splitBefore {
			deps = []string{it.ID.Bare()}
		}
		body, err := s.scaffoldWith(splitInto, title, proposal, splitRecord(it.ID.Bare(), splitBefore), deps)
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
		// A fresh id closes no cycle in either direction unless something
		// already names it. Checked anyway: the id comes from the tree, the shelf
		// and possibly a remote counter, and an id reused after a hand edit would
		// otherwise write a dependency on itself.
		id, path, err := s.mint(prefix, slug, body, func(id backlog.ID) error {
			if splitBefore {
				return s.acyclicIDs(id.Bare(), deps)
			}
			return s.acyclic(it, id.Bare())
		})
		if err != nil {
			return err
		}

		fmt.Printf("\nsplit %s -> %s  (%s, %s)\n\n", it.ID.Bare(), id.Bare(), splitInto, splitDirection(splitBefore))
		fmt.Printf("  %s  the work, with your proposal as its body\n", rel(s.cfg.Root, path))
		if splitBefore {
			// The original's own deps are untouched, so whatever blocked it
			// before the split still does.
			blockers := ""
			if state, missing := s.workState(it, s.cfg.LogDir()); state == stateBlocked {
				blockers = ", still blocked by " + strings.Join(missing, " ")
			}
			fmt.Printf("  %s  unchanged%s; %s waits on it\n\n", rel(s.cfg.Root, it.Path), blockers, id.Bare())
		} else {
			// After the scaffold, never before. A deps line naming an item that
			// does not exist blocks the original on nothing anybody can run, and
			// `check` reports it as a dep it could not read.
			if err := s.blockOn(it, id.Bare()); err != nil {
				return err
			}
			s.mirrorReloaded(it.ID.Bare(), false)
			fmt.Printf("  %s  now blocked on %s\n\n", rel(s.cfg.Root, it.Path), id.Bare())
			s.warnDeadlock(id.Bare())
		}
		fmt.Printf("The commits stay where they are: %s names them and somebody moves them.\n\n", id.Bare())
		fmt.Printf("NEXT — a human decides, because a session cannot accept its own contract:\n")
		fmt.Printf("  $EDITOR %s\n", rel(s.cfg.Root, path))
		fmt.Printf("  drover accept %s\n", id.Bare())
		fmt.Printf("  drover run %s --dry-run\n", id.Bare())
		return nil
	},
}

// scaffoldWith is the scaffold a split writes: the frontmatter and the stop
// marker, then the record of the split and the proposal in place of the
// placeholder prose.
//
// In place of, not after. scaffoldBody ends in an empty numbered list for a
// human to fill, and a body carrying that below a real one has requirements 1
// and 2 blank and requirement 3 written, which is the state the marker exists to
// prevent rather than one more thing for the operator to tidy.
func (s *session) scaffoldWith(kind, title, proposal, record string, deps []string) (string, error) {
	full := s.scaffoldBody(kind, title, map[string]string{
		s.cfg.Severity.Key: seedSeverity(kind, ""),
		backlog.KeyDeps:    strings.Join(deps, " "),
	})
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
	return head + "\n" + record + "\n\n" + strings.TrimSpace(proposal) + "\n", nil
}

// splitLine is the record split writes into the scaffold, naming the item it
// came out of and which of the two waits. It is prose in the body rather than
// a frontmatter key because the frontmatter is the project's closed schema,
// and a project that never declared a key for it would refuse the scaffold.
var splitLine = regexp.MustCompile(`(?m)^Split from (\S+) with --(after|before):`)

func splitDirection(before bool) string {
	if before {
		return "--before"
	}
	return "--after"
}

func splitRecord(from string, before bool) string {
	if before {
		return fmt.Sprintf("Split from %s with --before: this item waits on %s, which may merge first.", from, from)
	}
	return fmt.Sprintf("Split from %s with --after: %s waits on this item.", from, from)
}

// warnDeadlock prints, after a split under --after, each citation in the new
// item that only the original's branch holds. A warning and not a refusal:
// the scaffold is inert, its proposal is still being written, and accept
// refuses the same finding at the point it would start to cost something.
func (s *session) warnDeadlock(id string) {
	if err := s.reload(); err != nil {
		return
	}
	it, err := s.item(id)
	if err != nil {
		return
	}
	found, waiters := s.deadlocksOf(it)
	for _, d := range found {
		fmt.Fprintf(os.Stderr, "warning: %s\n", d)
	}
	if len(found) > 0 {
		fmt.Fprintf(os.Stderr, "  %s runs from the trunk, which cannot give it that code. Point the dependency the other way: %s,\n"+
			"  or rewrite its proposal so it stands on the trunk alone; accept refuses it until then, or until --yes.\n\n", id, flipDeps(id, waiters))
	}
}

// deadlocksOf is every branch-only citation in it, read against each unmerged
// item that waits on it, and the ids of the waiting items whose branches
// those citations came from.
func (s *session) deadlocksOf(it backlog.Item) (found, waiters []string) {
	for _, d := range s.backlog.Items {
		if slices.Contains(d.Deps(), it.ID.Bare()) && !s.done(d.ID.Bare()) {
			if msgs := s.branchOnly(it, d); len(msgs) > 0 {
				found = append(found, msgs...)
				waiters = append(waiters, d.ID.Bare())
			}
		}
	}
	return found, waiters
}

// flipDeps is the edit that reverses a deadlocked dependency, spelled out by
// id. Not "shelve it and split again": shelve refuses an id another item's
// deps line names, and under --after the waiting item names this one.
func flipDeps(dep string, waiters []string) string {
	return fmt.Sprintf("drop %s from the deps line of %s, and add %s to %s's",
		dep, strings.Join(waiters, " and "), strings.Join(waiters, " "), dep)
}

// branchOnly is every citation in dep's body that the trunk lacks and the
// branch of dependent, an item waiting on dep, has. dep runs from the trunk,
// so it cannot change what it cites there, and dependent cannot merge until
// dep does: neither can finish.
//
// Paths come from citedRefs, so only a path with a line part counts, the same
// claim about a tree refDrift reads. Names are a heuristic: a whole-word git
// grep for each backticked identifier over those paths, which misses a name
// living in a file the body never cites and can match a word that only
// happens to be spelled like one. A ref drover cannot read reports nothing,
// because it has no evidence either way.
func (s *session) branchOnly(dep, dependent backlog.Item) []string {
	refs := citedRefs(dep.Body)
	if len(refs) == 0 || !s.cfg.Git.On() {
		return nil
	}
	branch := s.branchRef(dependent)
	if branch == "" {
		return nil
	}
	trunk := s.driftRef()
	if gitcmd.Command(s.cfg.Root, "rev-parse", "--verify", "--quiet", trunk+"^{commit}").Run() != nil {
		return nil
	}
	var out []string
	say := func(what string) {
		out = append(out, fmt.Sprintf("%s cites %s, which only %s's branch has; with --after these items deadlock",
			dep.ID.Bare(), what, dependent.ID.Bare()))
	}
	var files []string
	for _, r := range refs {
		if slices.Contains(files, r.Path) {
			continue
		}
		files = append(files, r.Path)
		if s.isBlob(branch, r.Path) && !s.isBlob(trunk, r.Path) {
			say(r.Path)
		}
	}
	for _, name := range citedIdents(dep.Body) {
		onBranch, ok := s.greps(branch, name, files)
		if !ok || !onBranch {
			continue
		}
		if onTrunk, ok := s.greps(trunk, name, files); ok && !onTrunk {
			say("`" + name + "`")
		}
	}
	return out
}

// branchRef is the ref holding an item's branch: the local one, else origin's.
// Empty when neither exists, which is every item nobody has run.
func (s *session) branchRef(it backlog.Item) string {
	name, err := s.branchFor(it)
	if err != nil || name == "" {
		return ""
	}
	for _, ref := range []string{name, "origin/" + name} {
		if gitcmd.Command(s.cfg.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil {
			return ref
		}
	}
	return ""
}

func (s *session) isBlob(ref, path string) bool {
	kind, err := s.objectType(ref, path)
	return err == nil && kind == "blob"
}

// greps says whether name occurs as a whole word in any of files on ref, and
// whether git could answer at all: exit 1 is "no match", anything else is a
// read that failed.
func (s *session) greps(ref, name string, files []string) (found, ok bool) {
	args := append([]string{"grep", "-q", "-w", "-F", "-e", name, ref, "--"}, files...)
	err := gitcmd.Command(s.cfg.Root, args...).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, true
	}
	return false, false
}

var (
	citedSpan  = regexp.MustCompile("`([^`\\s]+)`")
	identShape = regexp.MustCompile(`^(?:[A-Za-z_]\w*\.)*([A-Za-z_]\w*)(\(\))?$`)
)

// citedIdents is every backticked name in body that looks like a function or
// type: called with (), or spelled in camel or Pascal case or with an
// underscore. A lowercase word on its own is prose more often than code, and
// an all-caps one is a constant or a finding kind, so neither is read; a
// qualified name is grepped by its last part, the one a definition spells.
func citedIdents(body string) []string {
	var out []string
	for _, m := range citedSpan.FindAllStringSubmatch(body, -1) {
		sub := identShape.FindStringSubmatch(m[1])
		if sub == nil {
			continue
		}
		name := sub[1]
		lower := strings.ToUpper(name) != name
		shaped := sub[2] != "" || strings.ContainsAny(name, "_ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		if !lower || !shaped || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
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
	return s.acyclicIDs(it.ID.Bare(), append(append([]string{}, it.Deps()...), dep))
}

// acyclicIDs is acyclic for an item that may not have a body yet, given the
// whole deps line it is about to carry.
func (s *session) acyclicIDs(id string, want []string) error {
	deps := map[string][]string{}
	for _, other := range s.backlog.Items {
		deps[other.ID.Bare()] = other.Deps()
	}
	deps[id] = want
	dep := want[len(want)-1]
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
	splitCmd.Flags().BoolVar(&splitAfter, "after", false,
		"the item split waits on the new one (the default): the new item's work stands on the trunk alone")
	splitCmd.Flags().BoolVar(&splitBefore, "before", false,
		"the new item waits on the item split, which gains no dep: the new work needs code only its branch has")
	splitCmd.MarkFlagsMutuallyExclusive("after", "before")
	localFlag(splitCmd)
	root.AddCommand(splitCmd)
}
