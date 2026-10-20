package cmd

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/spf13/cobra"
)

var linkCmd = &cobra.Command{
	Use:   "link <id> <issue>...",
	Short: "Add an issue to the item's closes key, so finish answers whoever filed it.",
	Long: `An item's ` + "`closes`" + ` key names the tracker issues its work resolves, and it
is the only list finish reads. This writes a number into it.

Two things tie an issue to an item and only one of them is automatic. A
follow-up filed during a run carries an owner link (a sub-issue of the owner's
issue, or an Owner: line), which reaches drover follow-ups and the next run's
prompt; the closes key is separate and, until this command,
was written by hand or not at all. An issue a run fixed but nobody linked is
invisible to finish: it closes nothing, comments nothing, and whoever filed the
report gets silence.

The number is checked against the tracker before it is written, because that is
the guard resolve gives up by accepting it. An issue owned by a different item
is refused by name — linking somebody else's report is how a disposition ends up
as a comment on a stranger's. An issue naming no owner is accepted with a note:
filing by hand is ordinary, and an owner link is not what makes a link true.

An owner link says which run introduced the gap, not which item fixes it, and
one item consolidating findings from several runs makes those two answers
differ. So an owned issue is accepted when the linking item declares that
owner on its deps: line — the inheritance rule follow-ups and intake already
read orphaning by — and the link is reported as inherited rather than silent.

Idempotent. A number already in the key is reported and exits 0.`,
	Example: `  drover link B36 259               # the run fixed it; finish closes it
  drover link B36 259 260           # several at once
  drover link B36 259               # again: already linked, exit 0

  drover resolve B36 259 fixed --code-by agent --link "<what shipped>"
                                    # link and judge in one step

  Safety: local writes. Rewrites one frontmatter line under {backlog}, appends a
  body digest row to {ledger}, and reads the tracker to check the number. Writes
  nothing to the tracker: finish applies what resolve records.`,
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("link names an item and at least one issue: drover link <id> <issue>")
		}
		return nil
	},
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		numbers := make([]int, 0, len(args)-1)
		for _, arg := range args[1:] {
			n, err := parsePositive(arg)
			if err != nil {
				return err
			}
			numbers = append(numbers, n)
		}
		return s.link(it, numbers)
	},
}

// link is link: it widens one item's closes key and reports what moved.
//
// The write is one rewrite of the whole key rather than an append per number,
// so a partial failure leaves the file as it was rather than half-claimed.
func (s *session) link(it backlog.Item, numbers []int) error {
	if !s.cfg.Known(closesKey) {
		return Refusal{fmt.Errorf(
			"this project does not declare %q in [frontmatter], so no item can name the issues it resolves.\n"+
				"Add it to required or optional in .drover.toml", closesKey)}
	}
	id := it.ID.Bare()
	claimed := backlog.Ints(it.Get(closesKey))

	var add []int
	for _, n := range numbers {
		switch {
		case containsInt(claimed, n):
			fmt.Printf("already linked: %s names %s\n", id, s.registerRefTo(os.Stdout, n))
		case containsInt(add, n):
			// The same number twice on one command line is one link.
		default:
			if err := s.linkable(it, n); err != nil {
				return err
			}
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil
	}

	widened := append(append([]int{}, claimed...), add...)
	sort.Ints(widened)
	lines, err := bodyLines(it.Path)
	if err != nil {
		return err
	}
	written, err := setFrontmatter(lines, closesKey, renderLinks(widened), s.cfg.DeclaredKeys())
	if err != nil {
		return err
	}
	if err := s.writeBody(it, written); err != nil {
		return err
	}

	fmt.Printf("linked: %s now closes %s (%s)\n", id, s.registerRefsTo(os.Stdout, widened), rel(s.cfg.Root, it.Path))
	if s.done(id) {
		fmt.Printf("  ! %s is recorded done, so finish has already run. Re-run it to apply a disposition on %s\n",
			id, s.registerRefsTo(os.Stdout, add))
	}
	fmt.Printf("NEXT — a reviewer judges each one:\n")
	for _, n := range add {
		fmt.Printf("  drover resolve %s %d <fixed|partial|unrelated> --code-by <who> \"<what shipped>\"\n", id, n)
	}
	return nil
}

// linkable refuses the number that would land a disposition on a report this
// item has no business answering.
//
// resolve's own refusal cannot do this: it accepts any number the
// closes key already holds, so the check has to happen where the number is
// written. A tracker drover cannot reach refuses rather than degrades, because
// the whole value here is having asked: a link written blind is the typo this
// check exists to catch.
//
// An owner link answers which run introduced the gap; the closes key answers
// which item fixes it. Those diverge the moment one item consolidates findings
// filed against several runs, so the deps line decides: an item declaring a
// follow-up's owner inherits that follow-up, the same rule follow-ups and
// intake already read orphaning by. Without it the only way to fold a backlog is
// to relink every issue, which overwrites the provenance an audit filed it
// against.
func (s *session) linkable(it backlog.Item, number int) error {
	id := it.ID.Bare()
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		fmt.Printf("  · #%d not checked: %s\n", number, s.cfg.RegisterDeclaration())
		return nil
	}
	issue, err := gh.View(s.labels(), number)
	if err != nil {
		return Refusal{fmt.Errorf(
			"could not read #%d from %s, so drover cannot tell whether %s has any business closing it: %w.\n"+
				"Claiming an issue nobody checked is how a disposition reaches a stranger's report",
			number, s.trackerName(), id, err)}
	}
	if owners := issue.Owners; len(owners) > 0 && !containsString(owners, id) {
		inherited := firstCommon(owners, it.Deps())
		if inherited == "" {
			return Refusal{fmt.Errorf(
				"#%d is owned by %s, not %s — %q.\n"+
					"Link it on the item that owns it, declare that owner on %s's deps: line, or correct the issue's owner link if it names the wrong item",
				number, strings.Join(owners, ", "), id, issue.Title, id)}
		}
		fmt.Printf("  · %s is owned by %s; %s declares %s on deps, so it inherits the follow-up\n",
			s.registerRefTo(os.Stdout, number), strings.Join(owners, ", "), id, inherited)
	}
	if !strings.EqualFold(issue.State, "open") {
		fmt.Printf("  · %s is %s already; finish will report it rather than close it again\n", s.registerRefTo(os.Stdout, number), strings.ToLower(issue.State))
	}
	if len(issue.Owners) == 0 {
		fmt.Printf("  · %s names no owner (no item's sub-issue, no Owner: line): linked on your word, not the tracker's\n", s.registerRefTo(os.Stdout, number))
	}
	return nil
}

// setFrontmatter rewrites one key inside the leading fence, adding the line
// when the item never declared it.
//
// A new line lands in the order the project declared its keys rather than at
// the end of the fence, so an item that gains a link reads like the ones
// beside it. Declared order is the only ordering drover knows: an item is free
// to write its keys in any sequence, and matching a neighbour would mean
// guessing which neighbour.
func setFrontmatter(lines []string, key, value string, order []string) ([]string, error) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("no frontmatter fence on line 1")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("frontmatter fence is never closed")
	}

	out := append([]string{}, lines...)
	line := key + ": " + value
	for i := 1; i < end; i++ {
		if frontmatterKey(out[i]) == key {
			out[i] = line
			return out, nil
		}
	}
	at := insertionPoint(out[1:end], key, order) + 1
	return append(out[:at:at], append([]string{line}, out[at:]...)...), nil
}

// insertionPoint is the index within the fence's key lines where key belongs.
//
// It reads backwards from key's declared position for a key already on disk and
// lands after it, then forwards for one to land before. An item declaring
// neither neighbour puts the line last, which is also where a project that
// declares no order at all puts every one of them.
func insertionPoint(fence []string, key string, order []string) int {
	pos := -1
	for i, k := range order {
		if k == key {
			pos = i
			break
		}
	}
	if pos < 0 {
		return len(fence)
	}
	for i := pos - 1; i >= 0; i-- {
		if at := indexOfKey(fence, order[i]); at >= 0 {
			return at + 1
		}
	}
	for i := pos + 1; i < len(order); i++ {
		if at := indexOfKey(fence, order[i]); at >= 0 {
			return at
		}
	}
	return len(fence)
}

func indexOfKey(fence []string, key string) int {
	for i, line := range fence {
		if frontmatterKey(line) == key {
			return i
		}
	}
	return -1
}

// frontmatterKey is the key one fence line declares, empty for a blank or a
// comment. The first colon only: a title contains them.
func frontmatterKey(line string) string {
	if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	colon := strings.Index(line, ":")
	if colon < 0 {
		return ""
	}
	return strings.TrimSpace(line[:colon])
}

// renderLinks writes the value side of the key, which is the shape intlist
// declares and Ints reads back.
func renderLinks(xs []int) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, strconv.Itoa(x))
	}
	return strings.Join(parts, " ")
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// firstCommon returns the first of xs that also appears in ys, empty when they
// are disjoint. Owner order is the tracker's, so the name reported is the one a
// reader sees on the issue rather than the one the deps line happens to list.
func firstCommon(xs, ys []string) string {
	for _, x := range xs {
		if containsString(ys, x) {
			return x
		}
	}
	return ""
}

func init() {
	root.AddCommand(linkCmd)
}
