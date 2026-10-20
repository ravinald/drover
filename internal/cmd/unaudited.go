package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/graph"
	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

var unauditedJSON bool

// UnauditedRow is one unaudited merge, and the unit of --json.
type UnauditedRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Blast is how many items transitively depend on this one, which is how
	// much rework a defect here would cost.
	Blast     int `json:"dependents"`
	FollowUps int `json:"follow_ups"`
	Critical  int `json:"critical"`
	High      int `json:"high"`
	Medium    int `json:"medium"`
	Low       int `json:"low"`
	// FollowUpsKnown is false when the register could not be read. A zero and an
	// unanswered question rank the same item very differently.
	FollowUpsKnown bool `json:"follow_ups_known"`
}

var unauditedCmd = &cobra.Command{
	Use:   "unaudited",
	Short: "Merged items nobody has inspected, worst blast radius first.",
	Long: `A completion record says an item merged. It says nothing about whether anyone
checked it, so this ranks the ones nobody did by how much rests on them.

Ties break on the severity of what an item already admits it left open. When the
follow-up list is unreachable the follow-up columns print "?" and never "0".`,
	Example: `  drover unaudited                  # worst blast radius first
  drover unaudited --json           # an array, one object per unaudited merge:
    [{"id":"4","title":"Resolver","dependents":7,"follow_ups":2,"high":1,"follow_ups_known":true}]

  Read follow_ups_known before the counts. False means the follow-up list could
  not be reached, so the zeroes mean nobody asked, not that nobody filed.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		rows, index := s.unauditedRows()

		if unauditedJSON {
			// An empty result is [], never null: a consumer that has to special-case
			// the empty case is one that will forget to.
			if rows == nil {
				rows = []UnauditedRow{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}

		fmt.Println("unaudited — merged items nobody has inspected, worst blast radius first")
		fmt.Println("dependents = items that transitively depend on this one · follow-ups = open follow-ups it owns")
		fmt.Println()
		fmt.Printf("%10s  %-16s  %-5s %s\n", "dependents", "follow-ups", "id", "item")
		width := titleWidth()
		var totalFollowUps int
		for _, r := range rows {
			cav := " ?              "
			if r.FollowUpsKnown {
				cav = fmt.Sprintf("%2d %-13s", r.FollowUps, sevTag(r.counts()))
				totalFollowUps += r.FollowUps
			}
			fmt.Printf("%10d  %s  %-5s %s\n", r.Blast, cav, r.ID, trunc(r.Title, width))
		}

		fmt.Println()
		fmt.Println(unauditedLine(len(rows), totalFollowUps, index.OK))
		if !index.OK {
			fmt.Printf("  (%v)\n", index.Reason())
		}
		fmt.Printf("record one:  drover audit <id> verified --code-by agent \"<what you re-checked>\"\n")
		return nil
	},
}

// unauditedLine is the footer's arithmetic. An unreachable register drops the follow-up
// count rather than printing a zero, because nobody asked is not nobody filed.
func unauditedLine(unaudited, followUps int, known bool) string {
	if !known {
		return fmt.Sprintf("%d unaudited · follow-ups unavailable, ranked on blast radius alone", unaudited)
	}
	return fmt.Sprintf("%d unaudited · %d open %s attributed",
		unaudited, followUps, plural(followUps, "follow-up", "follow-ups"))
}

func (s *session) unauditedRows() ([]UnauditedRow, gh.Index) {
	index := s.register()
	g := s.graph()

	var rows []UnauditedRow
	for _, it := range s.backlog.Items {
		st := s.ledger.State(it.ID.Bare())
		if !st.Merged || st.Audited() {
			continue
		}
		id := it.ID.Bare()
		r := UnauditedRow{ID: id, Title: it.Title(), Blast: g.BlastRadius(id), FollowUpsKnown: index.OK}
		if index.OK {
			for _, is := range index.For(id) {
				r.bucket(is.Severity)
			}
		}
		rows = append(rows, r)
	}

	sortUnaudited(rows)
	return rows, index
}

// sortUnaudited orders the table: blast radius first, then the severity of what the
// item already admits it left open, then id so two equal rows do not swap
// between runs.
func sortUnaudited(rows []UnauditedRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Blast != b.Blast {
			return a.Blast > b.Blast
		}
		ca, cb := a.counts(), b.counts()
		if worseSev(ca, cb) {
			return true
		}
		if worseSev(cb, ca) {
			return false
		}
		return a.ID < b.ID
	})
}

func (s *session) graph() *graph.Graph {
	deps := make(map[string][]string, len(s.backlog.Items))
	for _, it := range s.backlog.Items {
		deps[it.ID.Bare()] = it.Deps()
	}
	return graph.New(deps)
}

// register reads the configured source once. Callers must consult Index.OK
// before reporting an absence.
//
// Every backend answers with a gh.Index, so this is the only place in drover
// that knows there is more than one: routing, severity, dormancy and the counts
// all read the index, and a second switch above this line is how a file
// register would drift from the tracker it is meant to stand in for.
func (s *session) register() gh.Index {
	switch s.cfg.RegisterBackend() {
	case config.BackendNone:
		return gh.Index{OK: true, Off: true, Unpaged: true}
	case config.BackendFile:
		return register.Load(s.cfg.Abs(s.cfg.Paths.Register))
	default:
		return gh.Load(s.labels())
	}
}

// labels is the tracker vocabulary plus where to ask, and the issue each item
// names, which is what lets a sub-issue parent resolve to its owner.
func (s *session) labels() gh.Labels {
	items := map[int]string{}
	if s.backlog != nil {
		for _, it := range s.backlog.Items {
			if n := issueOf(it); n != 0 {
				items[n] = it.ID.Bare()
			}
		}
	}
	return gh.Labels{
		FollowUp:   s.cfg.GitHub.FollowUpLabel,
		Severity:   s.cfg.GitHub.SeverityPrefix,
		ItemIssues: items,
		Limit:      s.cfg.GitHub.PageLimit,
		Dir:        s.cfg.Root,
		Repo:       s.cfg.GitHub.Repo,
	}
}

// bucket records one open follow-up at whatever rank its label carried.
//
// A label carrying no rank drover recognises still counts toward FollowUps: the
// follow-up is open whether or not anyone ranked it, and dropping it would report an
// item as having nothing outstanding.
func (r *UnauditedRow) bucket(severity string) {
	r.FollowUps++
	switch sev.Of(severity) {
	case sev.Critical:
		r.Critical++
	case sev.High:
		r.High++
	case sev.Medium:
		r.Medium++
	case sev.Low:
		r.Low++
	}
}

// counts is the row's follow-up buckets worst first, in sev.Letters order.
//
// The tag and the tiebreak both read this one slice rather than listing the
// ranks separately. A rank listed in some of those places and not others is
// counted toward a total, shown nowhere and worth zero in the ranking, which
// looks like an item with no open follow-up rather than like a missing case.
func (r UnauditedRow) counts() []int { return []int{r.Critical, r.High, r.Medium, r.Low} }

func sevTag(counts []int) string {
	var parts []string
	for i, n := range counts {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, sev.Letters()[i]))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// worseSev breaks a blast-radius tie on what an item already admits it left
// open, comparing rank by rank from the worst down: the first rank where two
// items differ decides it, so one Critical outranks any number of Highs.
//
// Deliberately not a weighted score. Packing the counts into digits
// (C*1000 + H*100 + M*10 + L) reads as the same ordering and is not one: it
// carries at ten, scoring ten Mediums equal to one High and fifteen Lows above
// a Medium. Any single number has to name an exchange rate between ranks, and
// there is no honest one to name.
func worseSev(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func init() {
	unauditedCmd.Flags().BoolVar(&unauditedJSON, "json", false, "emit rows as JSON")
	root.AddCommand(unauditedCmd)
}
