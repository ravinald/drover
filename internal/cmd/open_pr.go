package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

var openPRForce bool

// prFacts is everything the body states. Every field is something drover read
// rather than something a session wrote: a self-report in a pull request body
// would be the round-one copy `reports.Write` refuses to replace, and nobody
// reading the pull request could tell.
type prFacts struct {
	// ID and Title name the item. A pull request body is read by people who have
	// the branch name and nothing else, and the branch name is a slug.
	ID, Title string
	// Gated is whether [drive] merge_gate exited 0. The body names the key and
	// never the argv: a gate declared by absolute path names one machine, and
	// this text is published.
	Gated bool
	// Level and By come from the most recent audit row. Empty says nobody
	// audited, which is a different sentence from a weak level and is written as
	// one.
	Level, By string
	// BlockedOn is the deps that have not merged. A pull request opens with
	// them named and the merge does not follow, because the branch is sound and
	// the work it waits on is somebody else's to accept.
	BlockedOn []string
	// Closes is the issues the item claims to resolve, referenced and never
	// closed from here. GitHub's own keyword reads `Closes #1, #2` as one
	// reference, and `finish` closes on a recorded disposition instead.
	Closes []int
}

// proposal is what open-pr decided.
type proposal struct {
	Number int
	// Created separates a pull request this call opened from one that was
	// already there. A correction pass pushes to the same one on purpose, so
	// finding one is the ordinary case and not a conflict.
	Created bool
}

// openPR is open-pr: it opens the pull request for a branch, or finds the one already open.
//
// Both callers reach this: `drive` after its gate went green, and the command
// below for the lifecycle an operator drives by hand. Neither runs the gate
// here, because the gate belongs to the caller that has the branch in the state
// it is about to claim is mergeable.
func (s *session) openPR(it backlog.Item, branch, trunk string, f prFacts) (proposal, error) {
	// CreatePR reuses an open pull request too, so this read is not what makes the
	// call safe. It is what tells a reused one from an opened one, which is the
	// difference between "already open" and "opened" in the output.
	labels := s.labels()
	if n, ok, err := gh.OpenPR(labels, branch); err != nil {
		return proposal{}, err
	} else if ok {
		s.recordPR(it.ID.Bare(), branch, n)
		return proposal{Number: n}, nil
	}
	n, err := gh.CreatePR(labels, branch, trunk,
		it.ID.Bare()+": "+it.Meta["title"], openPRBody(f))
	if err != nil {
		return proposal{}, err
	}
	s.recordPR(it.ID.Bare(), branch, n)
	return proposal{Number: n, Created: true}, nil
}

// recordPR writes the pull request row, unless the ledger's latest one already
// names this number and branch: a correction pass opens the same pull
// request every round, and a row per round would say nothing new.
//
// A failure warns and never fails the proposal. The pull request is open
// either way, and a drive that parked on the row would report a branch as
// unproposed while it sits open with nobody expecting it.
func (s *session) recordPR(id, branch string, n int) {
	if n <= 0 {
		return
	}
	row := ledger.Stamp(driveStamp(ledger.Row{ID: id, Branch: branch, PR: n, Date: ledger.Today()}))
	wrote, err := ledger.AppendUnless(s.cfg.Abs(s.cfg.Paths.Ledger), row, func(l *ledger.Ledger) bool {
		got, on := l.PR(id)
		return got == n && on == branch
	})
	switch {
	case err != nil:
		fmt.Fprintf(errOut, "warn: pull request %s for %s is open, but the ledger row naming it failed: %v\n", s.prRefTo(os.Stderr, n, ""), id, err)
	case wrote:
		s.ledger.Rows = append(s.ledger.Rows, row)
		appended()
	}
}

// openPRBody is the default body: what drover verified, and nothing else.
//
// A project wanting more configures it; there is no template key yet, and the
// gap is named in docs/configuration.md rather than left to be discovered.
func openPRBody(f prFacts) string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	if f.ID != "" || f.Title != "" {
		b.WriteString(strings.TrimSpace(f.ID+" "+f.Title) + ".\n\n")
	}
	b.WriteString("Verified: ")
	if f.Gated {
		b.WriteString("[drive] merge_gate exit 0")
	} else {
		b.WriteString("no [drive] merge_gate exit 0 on record")
	}
	if f.Level != "" {
		b.WriteString(", review recorded " + f.Level)
		if f.By != "" {
			b.WriteString(", code by " + f.By)
		}
	} else {
		b.WriteString(", and no audit row: nobody has recorded a level")
	}
	b.WriteString(".\n")
	if len(f.BlockedOn) > 0 {
		b.WriteString("\nBlocked on " + strings.Join(f.BlockedOn, ", ") +
			": the merge waits until each has finished.\n")
	}
	if len(f.Closes) > 0 {
		nums := make([]string, 0, len(f.Closes))
		for _, n := range f.Closes {
			nums = append(nums, "#"+strconv.Itoa(n))
		}
		b.WriteString("\nRelated: " + strings.Join(nums, ", ") + "\n")
	}
	return b.String()
}

// factsFor gathers what the body states for one item.
func (s *session) factsFor(it backlog.Item) prFacts {
	st := s.ledger.State(it.ID.Bare())
	return prFacts{
		ID:        it.ID.Bare(),
		Title:     it.Meta["title"],
		Gated:     len(s.cfg.Loop.MergeGate) > 0 && !openPRForce,
		Level:     st.Level,
		By:        st.CodeBy,
		BlockedOn: backlog.MissingDeps(it, s.done),
		Closes:    backlog.Ints(it.Get(closesKey)),
	}
}

// sinceLastReview is every row an item gained after its most recent review stage
// row, and whether one exists at all.
//
// The manual fallback reasons over this window rather than over Counts. A
// lifetime request for changes total says a review here writes changes rows and says nothing
// about whether the last one accepted the work; an audit level records how hard
// somebody looked rather than what they concluded. Both read as acceptance when
// neither is, which is how a green gate could publish work a review had just
// sent back.
func (s *session) sinceLastReview(id string) (rows []ledger.Row, ok bool) {
	at := -1
	for i, r := range s.ledger.Rows {
		if r.ID == id && r.Stage == ledger.StageReview {
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	for _, r := range s.ledger.Rows[at+1:] {
		if r.ID == id {
			rows = append(rows, r)
		}
	}
	return rows, true
}

// reviewedSincePass says a review stage row follows the item's latest pass row.
//
// ledger.Pass calls a pass stale on any later stage, audit or changes row, which
// is right for show and status. open-pr needs the narrower fact: whether a
// review ran after it, because only then is there a later review the ledger can
// read in its place.
func (s *session) reviewedSincePass(id string) bool {
	at := -1
	for i, r := range s.ledger.Rows {
		if r.ID == id && r.Pass != "" {
			at = i
		}
	}
	if at < 0 {
		return false
	}
	for _, r := range s.ledger.Rows[at+1:] {
		if r.ID == id && r.Stage == ledger.StageReview {
			return true
		}
	}
	return false
}

// latestAudit folds the audit rows in one window, last-row-wins, the way
// ledger.State folds them over a whole history.
func latestAudit(window []ledger.Row) (st ledger.State) {
	for _, r := range window {
		if r.Level != "" {
			st.Level, st.CodeBy, st.Independent, st.Auditor = r.Level, r.CodeBy, r.Independent, r.Reviewer
		}
	}
	return st
}

// reportDeniedPaths says what the branch changed that [drive] protected_paths names,
// and never refuses.
//
// drive parks such a branch because a merge it performs unattended would carry
// the change; here a person is about to read the pull request, and withholding
// it would take away the thing they use to review a diff the loop is not allowed
// to merge for them. A read that fails reports the failure for the same reason.
func (s *session) reportDeniedPaths(branch, trunk string) {
	hits, err := s.deniedPaths(branch, trunk)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stdout, "warn: could not read what %s changed against %s: %v\n", branch, trunk, err)
	case len(hits) > 0:
		fmt.Fprintf(os.Stdout,
			"note: %s changes %s, which [drive] protected_paths names. A drive would leave this for a human; "+
				"the pull request opens either way, and the merge is yours.\n",
			branch, displayPaths(hits))
	}
}

var openPRCmd = &cobra.Command{
	Use:   "open-pr [id]",
	Short: "Open the pull request for a branch the merge gate and the review passed.",
	Long: `An open pull request is drover's claim that a branch may merge. Nothing else
opens one, and nothing opens one for work that came back or parked: the branch
stays pushed, what came back stays in the item body, and no pull request implies a
merge nobody would make.

Two things have to have spoken. [drive] merge_gate has to exit 0, which open-pr
runs itself. And the most recent review has to have passed, which drive records
as a pass row and this reads back. A branch touching [drive] protected_paths still
gets its pull request: it is green and reviewed, and what it changes is why a
human merges it rather than why it should be invisible.

Omit the id to open one for the item whose branch you are standing on.

With no pass row, or a pass row a later review has outdated, the ledger answers
instead, and the output says which authority did and which drive pass it set
aside: the most recent audit is reviewed or verified and independent, at least
one request-changes row has ever been recorded, and the item is under [review] max_review_rounds.
That reading cannot see a review session that died, because a human ran it and
the question does not arise. It can see the cap, and it does.

A review_result row reading "findings_open" is a review that recorded its audit beside a
sent-back block an earlier round wrote, and said neither that the block was closed
nor that it was open. open-pr refuses it until somebody says which: run
` + "`drover close-changes <id>`" + ` when every finding is closed, and a close-changes
recorded after that pass and its audit makes open-pr read the pass as passed and
run the merge gate as usual; run ` + "`drover request-changes <id>`" + ` when one is still open.

--force opens one on evidence drover could not gather, for a project that
declares no merge gate, and names in its output what it skipped.

It is idempotent: a branch with a pull request already open reports that one. A
correction pass pushes to the same one on purpose, since two split the review
history of one contract across two places.`,
	Example: `  drover open-pr            # the item whose branch you are standing on
  drover open-pr F12        # by id
  drover open-pr F12 --force  # no merge gate declared; open it on my judgment
  drover close-changes F12 && drover open-pr F12  # a drive parked it as findings_open and the block is closed`,
	Args: optionalID("open-pr [id]"),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if !s.cfg.Git.On() {
			return Refusal{fmt.Errorf(
				"[git] enabled is false, so there is no branch to open a pull request for")}
		}
		// Before --force short-circuits anything. --force overrides drover's own
		// evidence, which is a judgement an operator may substitute for; it cannot
		// conjure a forge, so a missing gh is a refusal either way.
		if !gh.Available() {
			return Refusal{fmt.Errorf(
				"gh cannot be reached, so nothing here can open a pull request.\n" +
					"Install gh and run `gh auth login`. --force overrides the merge gate and the recorded " +
					"review, not the forge")}
		}
		// Before the gate and before any read of the tree, for finish's reason: a
		// run, draft or review has a session mid-edit in this checkout, and the
		// verify runs against whatever branch is out. A drive's sessions work in
		// worktrees of their own, so its lease is no refusal here.
		if err := s.leaseFree(); err != nil {
			return err
		}
		it, err := s.itemArg(args)
		if err != nil {
			return err
		}
		if err := s.itemNotDriving("open-pr", it.ID.Bare()); err != nil {
			return err
		}
		branch, err := s.branchFor(it)
		if err != nil {
			return err
		}
		trunk := s.trunk()

		if err := s.reviewPassed(it); err != nil {
			return err
		}
		// Before the gate, because the gate asks the forge about a commit and the
		// forge only knows what has been pushed. Without this a manual proposal
		// either strands its work, or passes a local gate while the pull request
		// names an older remote head: the reviewed commit and the published one
		// are different, and nothing says so.
		if err := s.pushBranch(branch); err != nil {
			return fmt.Errorf(
				"%s would not push, so the forge cannot be asked about the commit that was reviewed: %w",
				it.ID.Bare(), err)
		}
		if err := s.gatePassed(cmd.Context(), branch, trunk); err != nil {
			return err
		}
		s.reportDeniedPaths(branch, trunk)
		p, err := s.openPR(it, branch, trunk, s.factsFor(it))
		if err != nil {
			return err
		}
		what := "pull request already open"
		if p.Created {
			what = "pull request opened"
		}
		if p.Number > 0 {
			fmt.Fprintf(os.Stdout, "%s: %s %s for %s\n", it.ID.Bare(), what, s.prRefTo(os.Stdout, p.Number, ""), branch)
		} else {
			fmt.Fprintf(os.Stdout, "%s: %s for %s (gh printed no number)\n", it.ID.Bare(), what, branch)
		}
		return nil
	},
}

// reviewPassed refuses a branch whose most recent review did not pass.
//
// The pass row first, because drive computed it from the run log and the row
// deltas and this cannot: passSilent, passDied and passUnreadable are readings
// of a session's log, not of the ledger. Inferring a pass from an audit row with
// no changes row is the predicate drive refuses to use, and a second command
// deriving it would bring it back one command over.
//
// A stale pass row with a review stage row after it yields to the ledger
// reading of that later review. Any other stale pass still refuses: nothing has
// reviewed the branch since, so there is no later review to read.
func (s *session) reviewPassed(it backlog.Item) error {
	id := it.ID.Bare()
	if openPRForce {
		fmt.Fprintln(errOut, "--force: opening on your judgement, with neither the review_result row nor the merge gate consulted")
		return nil
	}
	// lead names the authority in the fallback's own sentences. aside is empty
	// unless a drive did speak, and then it opens everything the fallback prints,
	// so no answer reached past a pass row reads as one drive computed.
	lead := "no drive recorded a review pass for " + id
	aside := ""
	token, date, stale, ok := s.ledger.Pass(id)
	// A standing pass is a review that recorded its audit and never said whether
	// the block it read was closed. A clear after it, and after that audit, is
	// the reviewer saying so, which is the one thing the pass lacked; reading it
	// as anything short of passed refuses the branch close-changes just told
	// the operator open-pr would open.
	if ok && !stale && token == passStanding && s.ledger.ClearedSincePass(id) {
		fmt.Fprintf(errOut, "%s: the drive pass read %q on %s, and drover close-changes has discharged the block since; reading it as %q\n",
			id, token, date, passPassed)
		token = passPassed
	}
	switch {
	case ok && stale && s.reviewedSincePass(id):
		// A review drover did not drive ran after the pass row, so the pass is
		// about a commit that review has since looked past. The ledger can read
		// that review; refusing here would leave only drive or --force able to
		// clear a branch a manual review already accepted.
		aside = fmt.Sprintf(
			"set aside the drive pass for %s (%q on %s), which a later review outdates; the ledger answered instead.\n",
			id, token, date)
		lead = "no pass row covers the latest review of " + id
	case ok:
		defer func() {
			if !stale && token == passPassed {
				fmt.Fprintf(errOut, "%s: a recorded drive pass answered: %s on %s\n", id, token, date)
			}
		}()
		switch {
		case stale:
			return Refusal{fmt.Errorf(
				"the last recorded review of %s read %q on %s, and the item has run since.\n"+
					"That review result is about a commit this branch has moved past, and no review has looked since. "+
					"Review it again with `drover review %s --runner <x>` or a drive, or --force",
				id, token, date, id)}
		case token != passPassed:
			return Refusal{fmt.Errorf(
				"the last review of %s read %q, not %q: %s.\n"+
					"An open pull request says this branch may merge, and that review did not say so",
				id, token, passPassed, date)}
		}
		return nil
	}
	refuse := func(format string, a ...any) error {
		return Refusal{fmt.Errorf("%s"+format, append([]any{aside}, a...)...)}
	}
	// No current drive result here, which is the manual lifecycle rather than a
	// failure. The ledger answers what it can, and the message says so.
	c := s.ledger.Counts(id)
	window, ok := s.sinceLastReview(id)
	if !ok {
		return refuse(
			"no drive recorded a review pass for %s and no review stage row exists, so the ledger "+
				"cannot say which rows belong to the latest review.\n"+
				"Launch the review with `drover review %s --runner <x>`, which records one, or --force",
			id, id)
	}
	for _, r := range window {
		switch {
		case r.Changes != "":
			return refuse(
				"the latest review of %s sent work back on %s.\n"+
					"An audit level records how hard somebody looked, not that they accepted the work, so a "+
					"request for changes inside the same review outranks it. Fix what came back and review it again",
				id, r.Date)
		case r.Stage != "":
			// Any stage, not only implement. A stage row after the review is a
			// launch the review did not see, and naming the two that exist today
			// would read a stage added later as no launch at all.
			return refuse(
				"%s ran %s again on %s and no review has looked at it since.\n"+
					"A pull request would claim a review covered a commit nobody reviewed",
				id, r.Stage, r.Date)
		}
	}
	// The audit has to belong to that same review. State folds last-row-wins over
	// the item's whole history, so an older pass's level would stand in for a
	// review that recorded nothing at all.
	st := latestAudit(window)
	switch {
	case st.Level != ledger.LevelReviewed && st.Level != ledger.LevelVerified:
		return refuse(
			"%s, and the latest review's audit row reads %q.\n"+
				"A pull request needs a review that ran against the code: record one with "+
				"`drover audit %s reviewed --code-by <who> [--reviewer <name>]`, or --force",
			lead, blank(st.Level), id)
	case !st.Independent:
		return refuse(
			"%s, and the latest review's audit row was recorded by whoever wrote the code (--code-by %q).\n"+
				"Whoever wrote the code cannot certify that it may merge. Have somebody else audit it, or --force",
			lead, blank(st.CodeBy))
	// No cap case here, deliberately. request-changes' own refusal at the cap
	// tells the reviewer to file the finding and "let the work finish", so a second
	// verify refusing the pull request on the same count would contradict the
	// instruction the reviewer was given: findings filed exactly as asked, and
	// their items still unable to merge. The cap bounds how many rounds a review
	// may spend, not whether a human may open a pull request about what shipped.
	// drive's own passCapped still holds the unattended merge, and it reads a
	// recorded refusal rather than the item's history.
	// drive's rows 8 to 11, read against the body as it stands now: an edit
	// after the review is as much a write drover did not make as one during it.
	case s.blockStands(it):
		return refuse(
			"%s, and the latest review recorded an audit and no request for changes while the %q block an earlier round "+
				"wrote still stands in the body, which is why drive parks it (%s).\n"+
				"If its findings are still open, `drover request-changes %s -f <file>`; if they are closed, `drover close-changes %s`, or --force",
			lead, prompts.ChangesHeading, passStanding, id, id)
	case c.LastBody == "":
		return refuse(
			"%s, and no body digest exists for it.\n"+
				"An audit with no request for changes and nothing to compare the body against reads exactly like a "+
				"review that sent work back by editing the body, which is why drive parks it (%s). "+
				"Review it again with `drover review %s --runner <x>`, which records one, or --force",
			lead, passChangesUnrecorded, id)
	case s.bodyEdited(it, c.LastBody):
		return refuse(
			"%s, and its body changed outside drover since the last digest drover recorded (%s).\n"+
				"Findings written into it by hand cannot be told from a clean pass, which is why drive parks it (%s). "+
				"Send them back with `drover request-changes %s -f <file>`, or --force if the change was not a finding",
			lead, c.LastBody, passEdited, id)
	}
	fmt.Fprintf(errOut,
		"%s%s; the ledger answered: audit %s --code-by %s, %d changes requested.\n"+
			"That reading cannot see a review session that died, only what it wrote down.\n",
		aside, lead, st.Level, st.CodeBy, c.ChangesRequested)
	return nil
}

// blockStands reports whether the item file carries a sent-back block nobody
// cleared. An unreadable file carries none: bodyEdited refuses it.
func (s *session) blockStands(it backlog.Item) bool {
	b, err := os.ReadFile(it.Path) // #nosec G304 -- the path came from the backlog
	return err == nil && openBlock(string(b))
}

// bodyEdited reports whether the item file no longer matches want. An
// unreadable file counts as edited: it is not the body drover recorded.
func (s *session) bodyEdited(it backlog.Item, want string) bool {
	b, err := os.ReadFile(it.Path) // #nosec G304 -- the path came from the backlog
	return err != nil || !bodyMatches(want, bodySum(b), digestBytes(b))
}

// gatePassed refuses a branch the project's own go/no-go did not pass.
func (s *session) gatePassed(ctx context.Context, branch, trunk string) error {
	if openPRForce {
		return nil
	}
	if len(s.cfg.Loop.MergeGate) == 0 {
		return Refusal{fmt.Errorf(
			"no [drive] merge_gate is declared, so nothing here could say %s is green.\n"+
				"Declare the merge gate, or pass --force to open a pull request on your own judgement",
			branch)}
	}
	code, out, err := s.runGate(ctx, branch, trunk)
	if err != nil {
		return fmt.Errorf("the merge gate could not run: %w", err)
	}
	if code != gateGreen {
		return Refusal{fmt.Errorf(
			"the merge gate exited %d for %s, so nothing has said this branch may merge:\n%s",
			code, branch, strings.TrimSpace(out))}
	}
	return nil
}

func blank(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func init() {
	openPRCmd.Flags().BoolVar(&openPRForce, "force", false,
		"open a pull request without the merge gate or the review_result row; say so in the output")
	root.AddCommand(openPRCmd)
}

// openPRLine is how the drive header names what it will do at the forge.
func openPRLine() string {
	if driveNoMerge {
		return "open a pull request for what the merge gate passes, and merge none of them"
	}
	return "open a pull request for what the merge gate passes, then ask the forge to merge it"
}
