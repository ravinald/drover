package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

var (
	amendFile   string
	amendReason string
	amendMoved  string
	touchReason string
)

var amendCmd = &cobra.Command{
	Use:   "amend <id> -f <file> --reason <text>",
	Short: "Replace an accepted item's numbered requirements.",
	Long: `The numbered requirements are the contract, and every command a review can
reach refuses to change them: request-changes, close-changes and invariant each
refuse a write that would move the count. That is right for a reviewer and
leaves the operator, who may change a contract, with one way to do it, by hand.

A hand edit leaves the last body digest drover recorded stale, and drive reads
a body that no longer matches it as a reviewer writing findings by hand: the
next clean review parks body_edited. amend is the edit drover makes for you, so
the digest it records is the body on disk.

The file holds the new list, top-level "N. " lines with their continuation
lines, and nothing else: it replaces the body's one contiguous numbered list in
place, and prose around the list stays where it is. A list broken by prose is
refused rather than guessed at.

The replaced list is not lost. It is kept beside the saved reports as
<stem>-<slug>.amend-N.md with the reason, and the ledger gains a body row
carrying the new digest and an amend row recording the count before and after,
the reason and the file it kept.

Moving a requirement from one item to another is amend on both, each naming
the other with --moved, so the kept file and the row of each say where the
requirement went.

Only an accepted item that has not merged can be amended. A scaffold's
requirements are written by hand, which is the documented workflow, and a
merged item's contract is what its audit was measured against.`,
	Example: `  drover amend 7 -f reqs.md --reason "R4 belongs to the cache redesign"
  drover amend 7 -f reqs7.md --reason "R4 moves to 9" --moved 9
  drover amend 9 -f reqs9.md --reason "R4 of 7 lands here" --moved 7

  drover run 7 --dry-run                  # what the next run reads

Safety: local writes. Rewrites the numbered list of one item body, keeps the
replaced list under [paths] reports, appends a body row and an amend row to the
ledger, and reaches no tracker. The operator's: it refuses inside any session
drover launched, supervisor included.`,
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
		if amendFile == "" {
			return Refusal{fmt.Errorf("amend needs the new requirements: -f <file>, or -f - for stdin")}
		}
		reason := strings.TrimSpace(amendReason)
		if reason == "" {
			return Refusal{fmt.Errorf("amend needs --reason: a contract changed with no reason recorded is one the next reviewer cannot audit against")}
		}
		if err := s.contractOpen(it, "amend"); err != nil {
			return err
		}
		moved := ""
		if amendMoved != "" {
			other, err := s.item(amendMoved)
			if err != nil {
				return err
			}
			if other.ID.Bare() == it.ID.Bare() {
				return Refusal{fmt.Errorf("--moved names %s itself; it names the other item the requirement moved to or from", it.ID.Bare())}
			}
			moved = other.ID.Bare()
		}
		text, err := readEvidence(amendFile, os.Stdin)
		if err != nil {
			return err
		}
		list := strings.TrimSpace(text)
		want := len(backlog.Item{Body: list}.Requirements())
		if want == 0 {
			return Refusal{fmt.Errorf("%s holds no top-level \"N. \" line: amend replaces the numbered list, and an empty one is a contract nothing can confirm", amendFile)}
		}
		for _, l := range strings.Split(list, "\n") {
			if strings.HasPrefix(l, "#") {
				return Refusal{fmt.Errorf("%s carries a heading (%q): the file is the numbered list alone, and a heading would split the body's sections", amendFile, l)}
			}
		}

		raw, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		head, body, err := splitFrontmatter(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}
		lines := strings.Split(body, "\n")
		start, end, err := requirementSpan(lines)
		if err != nil {
			return Refusal{fmt.Errorf("%s: %w", rel(s.cfg.Root, it.Path), err)}
		}
		replaced := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(replaced) == list {
			return Refusal{fmt.Errorf("%s already holds that list: nothing to amend", it.ID.Bare())}
		}
		out := append([]string{}, lines[:start]...)
		out = append(out, strings.Split(list, "\n")...)
		out = append(out, lines[end:]...)
		next := strings.Join(out, "\n")
		before := len(it.Requirements())
		if after := len(backlog.Item{Body: next}.Requirements()); after != want {
			return Refusal{fmt.Errorf("refusing to write: the file holds %d requirements and the body would carry %d", want, after)}
		}

		// Kept before the body is written: a body amended whose replaced list
		// was lost is the one outcome with no record of what the contract was.
		keptAt, err := s.keepAmendment(it, replaced, reason, moved, before, want)
		if err != nil {
			return fmt.Errorf("keep the replaced list: %w", err)
		}
		written := []byte(head + next)
		if err := os.WriteFile(it.Path, written, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", it.Path, err)
		}
		if err := s.wroteBody(it.ID.Bare(), written); err != nil {
			return err
		}
		keptRel := rel(s.cfg.Root, keptAt)
		if err := s.record(ledger.Row{ID: it.ID.Bare(), Note: reason, Date: ledger.Today(),
			Amend: &ledger.Amendment{From: before, To: want, Moved: moved, Kept: keptRel}}); err != nil {
			return err
		}
		fmt.Printf("amended %s -> %s (%d requirement(s), was %d)\n", it.ID.Bare(), rel(s.cfg.Root, it.Path), want, before)
		fmt.Printf("replaced list kept: %s\n", keptRel)
		if moved != "" {
			if s.amendedWith(moved, it.ID.Bare()) {
				fmt.Printf("%s already records the other half of this move.\n", moved)
			} else {
				fmt.Printf("A move is amend on both items. The other half:\n\n    drover amend %s -f <file> --reason %q --moved %s\n\n",
					moved, reason, it.ID.Bare())
			}
		}
		fmt.Printf("The next run reads it: drover run %s --dry-run\n", it.ID.Bare())
		return nil
	},
}

var touchCmd = &cobra.Command{
	Use:   "touch <id> --reason <text>",
	Short: "Record a hand edit to an accepted item's prose as the operator's.",
	Long: `drive reads an item body that no longer matches the last digest drover
recorded as a write none of its commands made, and parks the review after it
as body_edited. drover check reports such an item, and drive refuses to launch
it, until the edit is recorded.

touch records it: one body row carrying the current digest and your reason.
It is for prose, a typo or a clearer paragraph. A change to the numbered
requirements is amend's, which keeps the list it replaced; touch keeps nothing
and cannot tell what changed, so recording a contract change with it leaves no
trace of the contract that was.`,
	Example: `  drover touch 7 --reason "reworded the background paragraph"
  drover check                            # the HAND EDITED finding is gone

Safety: local writes. Appends one body row to the ledger, and writes nothing
else. The operator's: it refuses inside any session drover launched,
supervisor included.`,
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
		reason := strings.TrimSpace(touchReason)
		if reason == "" {
			return Refusal{fmt.Errorf("touch needs --reason: it is the only record of what the hand edit was")}
		}
		if err := s.contractOpen(it, "touch"); err != nil {
			return err
		}
		b, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		sum := bodySum(b)
		if bodyMatches(s.ledger.Counts(it.ID.Bare()).LastBody, sum, digestBytes(b)) {
			return Refusal{fmt.Errorf("%s's body matches the digest drover last recorded: there is no hand edit to record", it.ID.Bare())}
		}
		if err := s.record(ledger.Row{ID: it.ID.Bare(), Body: sum, Note: reason, Date: ledger.Today()}); err != nil {
			return err
		}
		fmt.Printf("recorded %s's body as it stands (%s): %s\n", it.ID.Bare(), sum, reason)
		return nil
	},
}

// contractOpen refuses a scaffold and a merged item, the two states whose
// requirements are not the operator's to amend through drover.
func (s *session) contractOpen(it backlog.Item, command string) error {
	id := it.ID.Bare()
	if s.isScaffold(it) {
		return Refusal{fmt.Errorf("%s is a scaffold: its requirements are written by hand and accepted with drover accept %s, so there is nothing for %s to record", id, id, command)}
	}
	if s.done(id) {
		return Refusal{fmt.Errorf("%s has merged: its requirements are what its audit was measured against, and %s would rewrite that record", id, command)}
	}
	return nil
}

// requirementSpan is the line range of a body's numbered list: the first
// top-level "N. " line through the last line belonging to the last one. A
// continuation is an indented line, or any non-blank line directly under a
// line of the list. A heading ends the list, and so does an unindented line
// after a blank one.
//
// A numbered line past that end means prose splits the list, and a range
// replaced across it would delete the prose. That is refused, not guessed at.
func requirementSpan(lines []string) (start, end int, err error) {
	start = -1
	for i, l := range lines {
		if topLevelNumbered.MatchString(l) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("the body has no numbered list to amend")
	}
	end = start + 1
scan:
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		switch {
		case topLevelNumbered.MatchString(l), l != strings.TrimLeft(l, " \t") && strings.TrimSpace(l) != "":
			end = i + 1
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "#"):
			break scan
		case strings.TrimSpace(lines[i-1]) != "":
			end = i + 1
		default:
			break scan
		}
	}
	for i := end; i < len(lines); i++ {
		if topLevelNumbered.MatchString(lines[i]) {
			return 0, 0, fmt.Errorf("prose at line %d splits the numbered list, which resumes at line %d; amend replaces one contiguous list, so join it by hand and record that with drover touch first", end+1, i+1)
		}
	}
	return start, end, nil
}

// amendName matches a kept amendment and yields its number.
var amendName = regexp.MustCompile(`\.amend-(\d+)\.md$`)

// keepAmendment writes the list an amend replaced beside the saved reports and
// answers where. Numbered past the highest already there, as kept rounds are.
func (s *session) keepAmendment(it backlog.Item, replaced, reason, moved string, from, to int) (string, error) {
	dir := s.cfg.Abs(s.cfg.Paths.Reports)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := it.ID.Stem() + "-" + it.Slug + "."
	n := 1
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if m := amendName.FindStringSubmatch(e.Name()); m != nil {
			if k, _ := strconv.Atoi(m[1]); k >= n {
				n = k + 1
			}
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.amend-%d.md", it.ID.Stem(), it.Slug, n))
	var b strings.Builder
	fmt.Fprintf(&b, "# %s amendment %d · %s\n\n", it.ID.Bare(), n, ledger.Today())
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)
	if moved != "" {
		fmt.Fprintf(&b, "Moved: between %s and %s; each records its own amendment.\n\n", it.ID.Bare(), moved)
	}
	fmt.Fprintf(&b, "Requirements: %d before, %d after. The list this replaced:\n\n%s\n", from, to, strings.TrimSpace(replaced))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// amendedWith reports whether id carries an amend row naming other as the item
// a requirement moved with.
func (s *session) amendedWith(id, other string) bool {
	for _, r := range s.ledger.Rows {
		if r.ID == id && r.Amend != nil && r.Amend.Moved == other {
			return true
		}
	}
	return false
}

// handEdit is an accepted, unmerged item whose body no longer matches the last
// digest drover recorded: what happened, and the commands that re-record it.
// Both empty for any other item. check reports it and drive refuses to launch
// it with the same two sentences, because left alone the next clean review
// parks body_edited, reading an operator's edit as a reviewer's findings.
//
// A scaffold is exempt, since writing its requirements by hand is the
// workflow, and so is an item with no digest on record, which has nothing to
// compare against.
func (s *session) handEdit(it backlog.Item) (what, fix string) {
	id := it.ID.Bare()
	if s.isScaffold(it) || s.done(id) {
		return "", ""
	}
	last := s.ledger.Counts(id).LastBody
	if last == "" {
		return "", ""
	}
	b, err := os.ReadFile(it.Path)
	if err != nil || bodyMatches(last, bodySum(b), digestBytes(b)) {
		return "", ""
	}
	what = fmt.Sprintf("%s's body changed outside drover (recorded %s, now %s), and its next clean review would park body_edited",
		id, last, bodySum(b))
	fix = fmt.Sprintf("record the edit: drover amend %s -f <requirements> --reason \"<why>\" if the numbered requirements changed, "+
		"drover touch %s --reason \"<why>\" if only prose did", id, id)
	return what, fix
}

// sessionItems is every id a live session in this checkout is working: the
// lease holder's item, and each item a drive has running. A body such a session
// edits is mid-flight, not a hand edit. A lease drover cannot read reads as no
// session, so the report errs toward naming an item.
func (s *session) sessionItems() map[string]bool {
	l, busy, err := s.leaseHolder()
	if err != nil || !busy {
		return nil
	}
	ids := map[string]bool{}
	if l.Item != "" {
		ids[l.Item] = true
	}
	for _, id := range l.Running {
		ids[id] = true
	}
	return ids
}

func init() {
	amendCmd.Flags().StringVarP(&amendFile, "file", "f", "",
		"the new numbered list, or - for stdin (required)")
	amendCmd.Flags().StringVar(&amendReason, "reason", "", "why the contract changed (required)")
	amendCmd.Flags().StringVar(&amendMoved, "moved", "",
		"the other item, when a requirement moved between two; amend that one too")
	touchCmd.Flags().StringVar(&touchReason, "reason", "", "what the hand edit changed (required)")
	root.AddCommand(amendCmd, touchCmd)
}
