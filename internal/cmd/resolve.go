package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

var (
	resolveCodeBy string
	resolveLink   bool
)

var resolveCmd = &cobra.Command{
	Use:   "resolve <id> <issue> <" + "fixed|partial|unrelated" + "> [note]",
	Short: "Record what an item's work does to one issue it claims to resolve.",
	Long: `An item's ` + "`closes`" + ` key names the tracker issues its work is meant to
resolve. This records, per issue, whether it actually did.

  fixed       the merged work closes it; finish closes the issue
  partial     some of it shipped; finish comments and leaves the issue open
  unrelated   this item does not address it after all; finish comments and leaves it open

Nothing here reaches the tracker. Recording is judgement and the reviewer's
step; writing is the operator's, and finish does it once the merge is real. That
split is the same one finish already draws between finishing an item and auditing
it, for the same reason: a session must not close an issue on the strength of
its own account of what it built.

--code-by is the axis that survives into the tracker: agent, a person's name,
or a comma list of them, read the way audit reads it. finish warns when it closes
an issue on a disposition the author of the code recorded, because whoever wrote
it cannot certify that it resolves someone else's report. resolve takes no
--reviewer, so a named entry is independent only of a session drover launched.

The most recent row wins, so a disposition can be corrected in either direction
until finish applies it. After that the close or the comment is on a tracker other
people read, and correcting it is their business rather than the ledger's.

Once two independent passes record the same answer in a row, drover marks it
settled and the review prompt hands it over as fact instead of asking for it
again. Recording over a settled answer still works and still wins: this warns
rather than refuses, because an early pass can be wrong and the supersede rule
is the only way to say so. Reopening one is a claim that the code moved, so name
in the note what moved it.`,
	Example: `  drover resolve F8 47 fixed --code-by agent "R2 ships the fix; traced to internal/cmd/resolve.go:120"
  drover resolve F8 100 partial --code-by agent "schema shipped; the migration is F9"
  drover resolve F8 103 unrelated --code-by agent "this reports the CLI, not the resolver"

  The number must appear in the item's own closes: key. A number it does not
  name exits 3 and writes nothing — the alternative is a typo ending up as a
  comment on a stranger's issue. The refusal reads the issue's owner link and
  says which case you are in, because the three want opposite answers: a number
  owned by this item is yours to link, one owned by a dep it declares is
  inherited, and one owned elsewhere is somebody else's report. --link widens
  the key first, checking the number against the tracker the way drover link
  does:

    drover resolve B36 259 fixed --code-by agent --link "the npm arm ships here"

  drover show <id> lists what has been recorded and what is still undisposed.`,
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) == 2 {
			return fmt.Errorf("resolve records a disposition: %s", strings.Join(ledger.Dispositions(), ", "))
		}
		return cobra.RangeArgs(3, 4)(nil, args)
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
		number, err := parsePositive(args[1])
		if err != nil {
			return err
		}
		disposition := args[2]
		if !ledger.ValidDisposition(disposition) {
			return Refusal{fmt.Errorf("%q is not a disposition (%s)",
				disposition, strings.Join(ledger.Dispositions(), ", "))}
		}
		codeBy, err := ledger.ParseCodeBy(resolveCodeBy)
		if err != nil {
			return Refusal{err}
		}

		claimed := backlog.Ints(it.Get(closesKey))
		if !containsInt(claimed, number) {
			if !resolveLink {
				return Refusal{fmt.Errorf(
					"%s does not claim to resolve #%d — its %s key names %s.\n%s\n"+
						"Add --link to the command you just ran, or name it first: drover link %s %d",
					it.ID.Bare(), number, closesKey, renderInts(claimed),
					s.linkHint(it, number), it.ID.Bare(), number)}
			}
			if err := s.link(it, []int{number}); err != nil {
				return err
			}
			// Re-read: link rewrote the fence, and the undisposed line below
			// reports against the widened key rather than the one loaded before it.
			if it, err = s.item(args[0]); err != nil {
				return err
			}
			claimed = backlog.Ints(it.Get(closesKey))
		}

		note := ""
		if len(args) == 4 {
			note = strings.TrimSpace(args[3])
		}

		id := it.ID.Bare()
		prev, had := s.ledger.Resolution(id, number)
		if prev.Applied != "" {
			return Refusal{fmt.Errorf(
				"#%d was already %s on the tracker for %s — that is on a tracker other people read.\n"+
					"Correct it there rather than in the ledger", number, prev.Applied, id)}
		}
		// Settled warns and never refuses. Applied above refuses because that
		// write already happened somewhere drover does not own; this one is
		// entirely inside an append-only file, and the supersede rule is what
		// makes an early wrong answer correctable at all. Refusing here would
		// turn agreement into a lock and cost more than the re-judging does.
		if prev.Settled {
			switch {
			case prev.Disposition == disposition:
				fmt.Printf("  ! %s was settled %s by two independent passes. This row records the same answer again and changes nothing.\n",
					s.registerRefTo(os.Stdout, number), prev.Disposition)
			case note == "":
				fmt.Printf("  ! %s was settled %s by two independent passes; this moves it to %s with no note.\n"+
					"    The ledger keeps both rows and a later reader has only the note to go on. Say what changed.\n",
					s.registerRefTo(os.Stdout, number), prev.Disposition, disposition)
			default:
				fmt.Printf("  ! %s was settled %s by two independent passes; this moves it to %s.\n",
					s.registerRefTo(os.Stdout, number), prev.Disposition, disposition)
			}
		}
		entry, _ := s.ledger.SameAs(id, levelWriter(""), codeBy)
		row := ledger.Row{ID: id, Issue: number, Disposition: disposition,
			CodeBy: strings.Join(codeBy, ","), Independent: entry == "", Note: note, Date: ledger.Today()}
		if err := s.record(row); err != nil {
			return err
		}

		if had {
			fmt.Printf("disposition recorded: %s %s -> %s (%s)  (was %s)\n",
				id, s.registerRefTo(os.Stdout, number), disposition, "code by "+row.CodeBy, prev.Disposition)
		} else {
			fmt.Printf("disposition recorded: %s %s -> %s (%s)\n", id, s.registerRefTo(os.Stdout, number), disposition, "code by "+row.CodeBy)
		}
		if left := undisposed(s.ledger, id, claimed); len(left) > 0 {
			fmt.Printf("  still undisposed: %s\n", s.registerRefsTo(os.Stdout, left))
		}
		return nil
	},
}

// closesKey is the frontmatter key naming the issues an item resolves. It is a
// constant rather than a literal in five files because a project renaming it
// would otherwise find four of them.
const closesKey = backlog.KeyCloses

// linkHint is the middle line of resolve's refusal: what claiming this number
// would mean, once the tracker has been asked.
//
// It reads the same issue claimable does, and never fails. The refusal stands
// whatever comes back — the number is absent from the key either way — so the
// read decides wording rather than outcome, and a tracker that cannot answer
// falls through to the warning that is true when nothing is known. That is the
// opposite of claimable's rule, which refuses an unreachable tracker because
// there the read decides whether a write happens.
func (s *session) linkHint(it backlog.Item, number int) string {
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		return linkRoute(number, nil, false, it.ID.Bare(), it.Deps())
	}
	issue, err := gh.View(s.labels(), number)
	if err != nil {
		return linkRoute(number, nil, false, it.ID.Bare(), it.Deps())
	}
	return linkRoute(number, issue.Owners, true, it.ID.Bare(), it.Deps())
}

// linkRoute turns what the tracker knows about one issue into the sentence a
// reader needs to decide whether to link it.
//
// Four answers, because one would be wrong in the common case. A single
// warning that "a disposition on an issue nobody declared lands as a comment on
// a stranger's report", given whoever the issue belongs to, also fires on an
// issue routed to the very item under review, which is what a review pass that
// fixes a caveat routed to its own item meets. A reviewer who believes the
// warning leaves the loop open and hands the link to the operator; one who does
// not has learned to read past a refusal, which is worse. The check the warning
// describes lives in claimable and refuses by name, so stating it
// unconditionally here would protect nothing.
func linkRoute(number int, owners []string, known bool, id string, deps []string) string {
	switch {
	case !known:
		return fmt.Sprintf("Whether #%d is owned here went unread, so linking it rests on your reading rather than the tracker's.", number)
	case len(owners) == 0:
		return fmt.Sprintf("Nothing names an owner for #%d, so linking it rests on your reading rather than the tracker's.", number)
	case containsString(owners, id):
		return fmt.Sprintf("#%d is owned by %s, so linking it is uncontested.", number, id)
	default:
		if inherited := firstCommon(owners, deps); inherited != "" {
			return fmt.Sprintf("#%d is owned by %s, which %s declares on deps, so the claim records as inherited.",
				number, strings.Join(owners, ", "), inherited)
		}
		return fmt.Sprintf("#%d is owned by %s, not %s — linking another item's report is how a disposition reaches a stranger.",
			number, strings.Join(owners, ", "), id)
	}
}

// undisposed is every claimed issue with no disposition recorded. It is what
// makes the gap visible in three places (resolve, finish and check) from one
// definition, so they cannot disagree about which issues are still open
// questions.
func undisposed(l *ledger.Ledger, id string, claimed []int) []int {
	var out []int
	for _, n := range claimed {
		if _, ok := l.Resolution(id, n); !ok {
			out = append(out, n)
		}
	}
	return out
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// renderInts writes a number list the way the tracker does, so a reader can
// paste one straight back into a command.
func renderInts(xs []int) string {
	if len(xs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, fmt.Sprintf("#%d", x))
	}
	return strings.Join(parts, ", ")
}

func init() {
	resolveCmd.Flags().StringVar(&resolveCodeBy, "code-by", "",
		"who wrote the code: agent (a session drover launched), a person's name, or a comma list (required)")
	resolveCmd.Flags().BoolVar(&resolveLink, "link", false,
		"add the issue to the item's closes key first, the way drover link does")
	root.AddCommand(resolveCmd)
}
