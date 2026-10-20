package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

// parkOperatorRequirement is drive holding a merge on an [operator]
// requirement nobody has attested.
const parkOperatorRequirement = "awaiting_signoff"

var signoffCmd = &cobra.Command{
	Use:   "signoff <id> <n> <what was checked>",
	Short: "Record that the operator met one [operator] requirement.",
	Long: `A numbered requirement whose text opens with ` + "`" + backlog.OperatorTag + "`" + ` is the operator's to
meet: a live check over ssh, a deploy, anything a headless session's permission
mode blocks. The prompt tells the session not to attempt it, a review cannot
request changes on it, and drive opens the pull request and holds the merge until this
records it.

The note is the evidence. Name the command you ran and what it printed, because
the row is all a later reader has to tell a check that happened from a box that
was ticked.

signoff is the operator's: it refuses inside any session drover launched,
supervisor included, for the reason accept does. A session signing off the check
it was told not to attempt certifies its own work.`,
	Example: `  drover signoff F55 6 "ssh dev-guest-1 'systemctl is-active webapp' printed active"
  drover show F55                   # lists what is still pending

  A number that is not an [operator] requirement exits 3 and writes nothing,
  naming the ones that are.`,
	// The count is checked in RunE rather than Args, because cobra validates
	// arguments before the root's PersistentPreRunE, and a launched session
	// must meet the seal before it meets a usage error.
	Args: cobra.MaximumNArgs(3),
	RunE: func(_ *cobra.Command, args []string) error {
		if len(args) < 3 {
			return fmt.Errorf("signoff needs the id, the requirement number and what was checked: drover signoff <id> <n> \"<what you ran and what it printed>\"")
		}
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(args[1]), "R"))
		if err != nil || n <= 0 {
			return Refusal{fmt.Errorf("requirement must be a number, got %q", args[1])}
		}
		note := strings.TrimSpace(args[2])
		if note == "" {
			return Refusal{fmt.Errorf("the note is empty: say what you ran and what it printed")}
		}
		id := it.ID.Bare()
		ops := operatorRequirements(it)
		if !containsReq(ops, n) {
			if len(ops) == 0 {
				return Refusal{fmt.Errorf("%s has no %s requirement: every numbered requirement is the session's to meet", id, backlog.OperatorTag)}
			}
			return Refusal{fmt.Errorf("%s has no %s requirement R%d; the ones it has: %s",
				id, backlog.OperatorTag, n, reqNumbers(ops))}
		}
		if err := s.record(ledger.Row{ID: id, Signoff: n, Note: note, Date: ledger.Today()}); err != nil {
			return err
		}
		fmt.Printf("signed off %s R%d: %s\n", id, n, note)
		if left := s.pendingOperator(it); len(left) > 0 {
			fmt.Printf("still pending: %s\n", reqNumbers(left))
		}
		return nil
	},
}

// operatorRequirements are an item's [operator] requirements, in body order.
func operatorRequirements(it backlog.Item) []backlog.Requirement {
	var out []backlog.Requirement
	for _, r := range it.Requirements() {
		if r.Operator {
			out = append(out, r)
		}
	}
	return out
}

// pendingOperator are the [operator] requirements no signoff row has met yet.
func (s *session) pendingOperator(it backlog.Item) []backlog.Requirement {
	done := s.ledger.SignedOff(it.ID.Bare())
	var out []backlog.Requirement
	for _, r := range operatorRequirements(it) {
		if !done[r.Num] {
			out = append(out, r)
		}
	}
	return out
}

// operatorHold is the merge hold for an unattested [operator] requirement: the
// park token and its reason, empty when the merge may go ahead. It sits beside
// denyHold, after the pull request opens, because the branch is green and
// reviewed and what it waits on is a person rather than another round.
func (s *session) operatorHold(it backlog.Item) (park, reason, run string) {
	left := s.pendingOperator(it)
	if len(left) == 0 {
		return "", "", ""
	}
	id := it.ID.Bare()
	parts := make([]string, 0, len(left))
	for _, r := range left {
		parts = append(parts, fmt.Sprintf("R%d (%s)", r.Num, r.Text))
	}
	return parkOperatorRequirement,
		"waits on " + strings.Join(parts, ", ") + ", which only the operator can meet",
		fmt.Sprintf("meet %s, then drover signoff %s %d \"<what was checked>\"", reqNumbers(left), id, left[0].Num)
}

func containsReq(reqs []backlog.Requirement, n int) bool {
	for _, r := range reqs {
		if r.Num == n {
			return true
		}
	}
	return false
}

// reqNumbers renders requirements as "R2, R6".
func reqNumbers(reqs []backlog.Requirement) string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, "R"+strconv.Itoa(r.Num))
	}
	return strings.Join(out, ", ")
}

func init() {
	root.AddCommand(signoffCmd)
}
