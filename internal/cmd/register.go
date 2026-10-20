package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/spf13/cobra"
)

var (
	registerJSON     bool
	registerLive     bool
	registerCoverage bool
)

// RegisterReport is what --json emits. The rows sit under a key rather than at
// the top level because a bare array cannot say whether the read was cut short,
// and an empty array from a page that hit the limit is the least honest answer
// this tool can give: a wrapper cannot tell it from a project with no
// follow-ups.
type RegisterReport struct {
	Rows      []RegisterRow `json:"rows"`
	Truncated bool          `json:"truncated"`
	PageLimit int           `json:"page_limit"`
}

// RegisterRow is one follow-up as drover reads it. Owners is empty when no
// owner link names one, so a wrapper can find those without re-deriving the
// links. Orphaned separates the follow-up whose owners have all merged from the
// one still waiting on an open owner: both name an owner, and only one of them
// has a run left to reach.
type RegisterRow struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Severity string   `json:"severity,omitempty"`
	Owners   []string `json:"owners"`
	Unowned  bool     `json:"unowned"`
	Orphaned bool     `json:"orphaned"`
	// Coverage and BlockedOn say whether anything will carry it, under
	// --coverage alone.
	Coverage  string              `json:"coverage,omitempty"`
	BlockedOn map[string][]string `json:"blocked_on,omitempty"`
}

// orphaned reports that every owner of a follow-up has merged. The ledger is
// the only authority: an owner link says who owns it, never whether they
// shipped. An orphaned follow-up keeps its link and keeps travelling — the next
// item naming one of those ids on its deps line inherits it — so this is a
// reachability state, not a reason to close anything.
func (s *session) orphaned(owners []string) bool {
	if len(owners) == 0 {
		return false
	}
	for _, id := range owners {
		if !s.done(id) {
			return false
		}
	}
	return true
}

// registerTally is the follow-up list's arithmetic: how many are open, and how
// many of those reach a run. Live is the remainder rather than a third scan —
// a follow-up is unowned, orphaned or live, and nothing is both.
type registerTally struct {
	Open     int
	Live     int
	Orphaned int
	Unowned  int
}

// registerCounts counts one index. The loop lived inline in the command that
// printed the numbers first, and every question about what the follow-up list
// still reaches is answered off them: a second copy would disagree with the
// footer the moment either one changed.
func (s *session) registerCounts(index gh.Index) registerTally {
	t := registerTally{Open: len(index.Issues)}
	for _, is := range index.Issues {
		switch {
		case len(is.Owners) == 0:
			t.Unowned++
		case s.orphaned(is.Owners):
			t.Orphaned++
		default:
			t.Live++
		}
	}
	return t
}

// registerSuffix marks the follow-up nothing is scheduled to carry.
//
// Orphaning is deliberately not marked here. On a project whose items are
// written and merged in one sitting, every owned follow-up is orphaned the
// moment its owner merges, so a per-row annotation fires on all of them and
// costs the title the columns it needs. registerFooter counts the state once
// instead.
func registerSuffix(unowned bool) string {
	if unowned {
		return "  (unowned — reaches no run)"
	}
	return ""
}

// emptyPredicate names the question a zero answered.
//
// `gh issue list --label` returns an empty array and exit 0 for a label nobody
// created, so a typo'd follow_up_label and a project with nothing filed print
// the same line. Naming the label turns a silent empty list into one that says
// what it asked for, which is the same rule as reporting an unreachable read
// rather than a zero.
func (s *session) emptyPredicate() string {
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		return ""
	}
	return fmt.Sprintf(" (nothing in %s carries the %q label)", s.trackerName(), s.cfg.GitHub.FollowUpLabel)
}

// trackerName names the repository a tracker read asked. An unset [github] repo
// is gh inferring one from the work tree, which has no name to print, so every
// sentence naming where a query went says it the one way.
func (s *session) trackerName() string {
	if s.cfg.GitHub.Repo == "" {
		return "this repository"
	}
	return s.cfg.GitHub.Repo
}

// registerFooter says what the rows above it no longer say. It is empty when
// every follow-up is owned by an item still open, because that is the state a
// reader assumes and naming it adds a line carrying nothing.
func registerFooter(open, orphaned, unowned int) string {
	if orphaned == 0 && unowned == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d open", open)}
	if orphaned > 0 {
		parts = append(parts, fmt.Sprintf("%d orphaned (every owner merged)", orphaned))
	}
	if unowned > 0 {
		parts = append(parts, fmt.Sprintf("%d unowned", unowned))
	}
	return strings.Join(parts, " · ")
}

var followUpsCmd = &cobra.Command{
	Use:   "follow-ups [id]",
	Short: "The open follow-up list: what this project knows is unresolved.",
	Long: `With an id, shows what that item owns. Without one, the whole follow-up list.

A follow-up is an issue carrying the follow-up label, and nothing else in the
tracker is read. That label is membership rather than a kind: a repository's
ordinary backlog is not drover's follow-up list, and a bug worth its own unit of
work becomes an item with ` + "`drover intake <n>`" + ` rather than a follow-up.

Its owner is a link, not a label: the GitHub sub-issue parent, when that parent
is a backlog item's issue, and every "Owner: <id>" line in the issue body. A
follow-up with neither is deliberately inert — it records provenance for a human
and no run will carry it. That is the honest state for a gap no item owns, but
it is also what a typo looks like, so check it was meant.

A follow-up whose every owner has merged is orphaned. It is not lost: the link
still names its owners, and the next item naming one of those ids on its deps
line inherits it. What it has lost is a run of its own, so nothing is scheduled
to carry it until somebody declares that dependency. The footer counts them and
--live hides them.

--coverage shows only the follow-ups nothing is scheduled to carry, each judged
now against the ledger, the backlog and the queue of the drive holding this
checkout: blocked (every owner not merged is blocked, and the line names what
each waits on), orphaned, or unowned. A drive emits the same coverage on each
follow-up its sessions file, and stops at [supervise] triage_finding for these.

` + "`drover intake <n>`" + ` is the other answer: it promotes an orphaned follow-up to an
item and drops the follow-up label as it goes, so it leaves this list rather
than sitting in both. It refuses one still owned by an open item, since that
run already carries it.`,
	Example: `  drover follow-ups                 # the whole follow-up list
  drover follow-ups 4               # what item 4 owns
  drover follow-ups --live          # only follow-ups an open owner still reaches
  drover follow-ups --coverage      # only follow-ups nothing is scheduled to carry
  drover follow-ups --json          # the rows, plus whether the read was cut short:
    {"truncated":false,"page_limit":300,"rows":[
      {"number":12,"title":"contract checks assert shape","severity":"medium",
       "owners":["4"],"unowned":false,"orphaned":false}]}

  "truncated" is true when the read hit page_limit. The rows are then a floor
  rather than the list, and an empty "rows" beside it is not an empty list — it
  is a page nobody finished reading.

  File one against the item that OWNS the gap, not the one that noticed it: the
  follow-up label plus severity:<critical|high|medium|low>, as a sub-issue of
  the owner's issue or with an "Owner: <owning id>" line in the body.`,
	Args: optionalID("follow-ups [id]"),
	RunE: func(_ *cobra.Command, args []string) error {
		return listRegister(args)
	},
}

func listRegister(args []string) error {
	s, err := open()
	if err != nil {
		return err
	}
	index := s.register()
	if !index.OK {
		return index.Reason()
	}

	// Read before an id narrows the index below: Truncated compares a row count
	// against the page limit, and a scoped count would report a whole read as
	// complete because one item happens to own fewer rows than a page holds.
	truncated := index.Truncated(s.cfg.GitHub.PageLimit)

	owner := ""
	if len(args) == 1 {
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		index.Issues = index.For(it.ID.Bare())
		owner = it.ID.Bare()
	}

	rows := index.Issues
	const kind = "follow-ups"

	// Counted before --live filters, so the line reporting an empty result can
	// say how many rows it hid rather than reading as an empty register.
	counts := s.registerCounts(index)
	total := counts.Open
	if registerLive {
		live := make([]gh.Issue, 0, len(rows))
		for _, is := range rows {
			if !s.orphaned(is.Owners) {
				live = append(live, is)
			}
		}
		rows = live
	}
	coverage := map[int]FiledFinding{}
	if registerCoverage {
		queued := s.heldQueue()
		uncovered := make([]gh.Issue, 0, len(rows))
		for _, is := range rows {
			if f := s.registerFinding(is, nil, queued); f.uncovered() {
				coverage[is.Number] = f
				uncovered = append(uncovered, is)
			}
		}
		rows = uncovered
	}

	if registerJSON {
		out := make([]RegisterRow, 0, len(rows))
		for _, is := range rows {
			owners := is.Owners
			if owners == nil {
				owners = []string{}
			}
			out = append(out, RegisterRow{
				Number: is.Number, Title: is.Title, Severity: is.Severity,
				Owners: owners, Unowned: len(is.Owners) == 0,
				Orphaned: s.orphaned(is.Owners),
				Coverage: coverage[is.Number].Coverage, BlockedOn: coverage[is.Number].BlockedOn,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(RegisterReport{
			Rows:      out,
			Truncated: truncated,
			PageLimit: s.cfg.GitHub.PageLimit,
		})
	}

	switch {
	case index.Off:
		fmt.Printf("no %s: %s declares %s, so there is no follow-up list to read\n", kind, s.cfg.Project, s.cfg.RegisterDeclaration())
	case len(rows) == 0 && registerCoverage:
		fmt.Printf("no %s left uncovered — every one of %d open %s an owner a run will reach\n", kind, total, plural(total, "follow-up has", "follow-ups have"))
	case len(rows) == 0 && registerLive && counts.Orphaned > 0:
		fmt.Printf("no live %s — all %d open %s orphaned\n", kind, total, plural(total, "follow-up is", "follow-ups are"))
	case len(rows) == 0:
		fmt.Println("no " + registerScope(kind, owner) + s.emptyPredicate())
	default:
		fmt.Printf("%s:\n", registerScope(kind, owner))
		width := titleWidth()
		for _, is := range rows {
			sev := is.Severity
			if sev == "" {
				sev = "-"
			}
			suffix := registerSuffix(len(is.Owners) == 0)
			if f, ok := coverage[is.Number]; ok {
				suffix = "  (" + f.Coverage + blockedClause(f.BlockedOn) + ")"
			}
			fmt.Printf("  %s [%s] %s%s\n", padRef(s.registerRefTo(os.Stdout, is.Number), is.Number), sev, trunc(is.Title, width), suffix)
		}
		if footer := registerFooter(total, counts.Orphaned, counts.Unowned); footer != "" {
			fmt.Printf("\n%s\n", footer)
		}
	}
	// Outside the switch, because truncation is a property of the read rather
	// than of the rows that survived the filter. A page limit hit while nothing
	// of this kind came back still lost issues, and the empty-set line alone
	// reports a zero the read cannot support.
	if truncated {
		fmt.Printf("\nnote: hit the %d-issue page limit; there may be more\n", s.cfg.GitHub.PageLimit)
	}
	return nil
}

// registerScope names the set a command answered for. The header and the
// empty-set line are the same phrase in two moods, so both are built here: an
// unscoped read carries no trailing qualifier at all, and a phrase that reads
// only under an id is how "open issues all open:" survived.
func registerScope(kind, owner string) string {
	if owner == "" {
		return "open " + kind
	}
	return "open " + kind + " owned by " + owner
}

func init() {
	followUpsCmd.Flags().BoolVar(&registerJSON, "json", false, "emit rows as JSON")
	followUpsCmd.Flags().BoolVar(&registerLive, "live", false, "hide follow-ups whose every owner has merged")
	followUpsCmd.Flags().BoolVar(&registerCoverage, "coverage", false, "only follow-ups nothing is scheduled to carry: blocked, orphaned or unowned")
	root.AddCommand(followUpsCmd)
}
