package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gh"
)

// PRState is an item's pull request as status and show report it, and the unit
// both encode under "pr".
type PRState struct {
	// Number is absent when the read failed and the ledger recorded none.
	Number int `json:"number,omitempty"`
	// State is open, draft, merged or closed, or unknown when the read did not
	// complete, and Error then names why. A blank state would read as no pull
	// request, which is a different answer from one nobody could ask about.
	State  string `json:"state"`
	URL    string `json:"url,omitempty"`
	Checks string `json:"checks,omitempty"`
	Error  string `json:"error,omitempty"`
	// reason is Error in a word a table cell can carry.
	reason string
}

const prUnknown = "unknown"

// errNoTracker is the read --no-tracker declined to make. It reports through
// the same unknown state as a failure, because "not asked" is not "none".
var errNoTracker = &gh.Unread{Reason: "--no-tracker", Err: errors.New("the forge was not asked")}

// noGitHub says a read failed because the checkout has no GitHub remote, so
// there are no pull requests to report rather than ones nobody could read.
// Keyed on what gh found, not on [git] merge_check, which a GitHub project
// that rebase-merges sets too.
func noGitHub(err error) bool {
	u, ok := errors.AsType[*gh.Unread](err)
	return ok && u.Reason == gh.UnreadNoGitHub
}

func unknownPR(number int, err error) *PRState {
	reason := gh.UnreadFailed
	if u, ok := errors.AsType[*gh.Unread](err); ok {
		reason = u.Reason
	}
	return &PRState{Number: number, State: prUnknown, Error: err.Error(), reason: reason}
}

// matchPR picks the item's pull request out of a read. A fork's branch can
// share the item's name and is skipped. Several from one branch is a pull
// request closed and reopened as a new one: the recorded number wins, then an
// open one, then the newest, which is the order gh returns them in.
//
// older says the read was cut off at its page limit and the item is merged, so
// a pull request missing from it may simply be older than the page.
func matchPR(prs []gh.PR, branch string, recorded int, older bool) *PRState {
	rank := func(p *gh.PR) int {
		switch {
		case p.Number == recorded:
			return 2
		case p.State == "open" || p.State == "draft":
			return 1
		}
		return 0
	}
	var pick *gh.PR
	for i := range prs {
		if p := &prs[i]; p.Head == branch && !p.Fork && (pick == nil || rank(p) > rank(pick)) {
			pick = p
		}
	}
	switch {
	case pick != nil:
		return &PRState{Number: pick.Number, State: pick.State, URL: pick.URL, Checks: pick.Checks}
	case recorded > 0:
		return unknownPR(recorded, &gh.Unread{Reason: "not found",
			Err: fmt.Errorf("the ledger records #%d for %s, and the forge listed no pull request from that branch", recorded, branch)})
	case older:
		return unknownPR(0, &gh.Unread{Reason: "past the page",
			Err: fmt.Errorf("no pull request from %s among the newest the forge returned; [github] page_limit bounds the read", branch)})
	}
	return nil
}

// attachPRs fills the PR on every row shown says will print, with one forge
// read for all of them. A merged item with a recorded number answers from the
// ledger and needs no read, so a table of merged rows asks nothing at all. The
// error is the read's, nil when it completed or was never needed.
//
// A project with [git] enabled false has no branches and so no pull requests:
// the column is absent rather than unknown. So is a checkout with no GitHub
// remote, except on a row whose number the ledger recorded, which is a pull
// request that exists somewhere and is reported unknown with the reason.
func (s *session) attachPRs(rows []Row, shown func(Row) bool, ask bool) error {
	if !s.cfg.Git.On() {
		return nil
	}
	items := make(map[string]backlog.Item, len(s.backlog.Items))
	for _, it := range s.backlog.Items {
		items[it.ID.Bare()] = it
	}
	type need struct {
		at       int
		branch   string
		recorded int
	}
	var needs []need
	for i, r := range rows {
		it, ok := items[r.ID]
		if !ok || !shown(r) {
			continue
		}
		n, _ := s.ledger.PR(r.ID)
		if r.State == stateDone && n > 0 {
			rows[i].PR = &PRState{Number: n, State: "merged"}
			continue
		}
		// A template that will not render for this item is run's to report;
		// with no branch there is nothing to match a pull request on.
		branch, err := s.branchFor(it)
		if err != nil || branch == "" {
			continue
		}
		needs = append(needs, need{i, branch, n})
	}
	if len(needs) == 0 {
		return nil
	}
	if !ask {
		for _, w := range needs {
			rows[w.at].PR = unknownPR(w.recorded, errNoTracker)
		}
		return nil
	}
	limit := s.cfg.GitHub.PageLimit
	if limit <= 0 {
		limit = 300
	}
	prs, err := gh.PullRequests(s.labels(), limit)
	for _, w := range needs {
		if err != nil {
			if !noGitHub(err) || w.recorded > 0 {
				rows[w.at].PR = unknownPR(w.recorded, err)
			}
			continue
		}
		rows[w.at].PR = matchPR(prs, w.branch, w.recorded, len(prs) >= limit && rows[w.at].State == stateDone)
	}
	return err
}

// showPR is the one-item read: the pull requests from the item's branch, with
// the head's checks. A merged item keeps its recorded number and state through
// a failed read, since the ledger already answers both; only the URL and the
// checks went unread, and Error says so.
//
// none says the project has no pull requests to report at all, which show
// prints as no pr line rather than as none from this branch.
func (s *session) showPR(id, branch string, done, ask bool) (p *PRState, none bool) {
	if branch == "" || !s.cfg.Git.On() {
		return nil, true
	}
	recorded, _ := s.ledger.PR(id)
	var prs []gh.PR
	err := error(errNoTracker)
	if ask {
		prs, err = gh.PullRequestsFor(s.labels(), branch)
	}
	if err != nil {
		switch {
		case done && recorded > 0:
			return &PRState{Number: recorded, State: "merged", Error: err.Error()}, false
		case recorded == 0 && noGitHub(err):
			return nil, true
		}
		return unknownPR(recorded, err), false
	}
	p = matchPR(prs, branch, recorded, false)
	if done && recorded > 0 && (p == nil || p.State == prUnknown) {
		return &PRState{Number: recorded, State: "merged"}, false
	}
	return p, false
}

// cell is the PR as one table cell, padded to width before the number is
// linked: fmt counts escape bytes as width, so a link wrapped first shears the
// columns to its right.
func (p *PRState) cell(s *session, width int) string {
	if p == nil {
		return fmt.Sprintf("%-*s", width, "")
	}
	num, rest := "", p.State
	if p.Number > 0 {
		num = fmt.Sprintf("#%d", p.Number)
		rest = " " + rest
	}
	if p.State == prUnknown {
		rest += " (" + p.reason + ")"
	}
	text := trunc(num+rest, width)
	pad := fmt.Sprintf("%-*s", width, text)[len(text):]
	if num == "" || len(text) < len(num) {
		return text + pad
	}
	return s.prRefTo(os.Stdout, p.Number, p.URL) + text[len(num):] + pad
}

// visible is the cell's width before padding.
func (p *PRState) visible() int {
	if p == nil {
		return 0
	}
	n := len(p.State)
	if p.Number > 0 {
		n += len(fmt.Sprintf("#%d ", p.Number))
	}
	if p.State == prUnknown {
		n += len(p.reason) + 3
	}
	return n
}

// line is show's account of the pull request: the number linked, the state,
// the checks and the URL in text, since a terminal that cannot follow a link
// still has to be able to copy one. A read that failed prints what gh said.
func (p *PRState) line(s *session) string {
	if p == nil {
		return "none from this branch"
	}
	parts := []string{}
	if p.Number > 0 {
		parts = append(parts, s.prRefTo(os.Stdout, p.Number, p.URL))
	}
	parts = append(parts, p.State)
	if p.Checks != "" {
		parts = append(parts, "checks "+p.Checks)
	}
	if p.URL != "" {
		parts = append(parts, p.URL)
	}
	out := strings.Join(parts, "  ")
	if p.Error != "" {
		out += "  (" + p.Error + ")"
	}
	return out
}
