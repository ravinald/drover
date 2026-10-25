package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
)

// pass is one review's before and after, and what its session left behind.
type pass struct {
	before, after ledger.Counts
	res           runlog.Result
	logged        bool
	// events is how many events the session's log carried and known how many
	// of them a runlog schema claims. Together they separate a runner whose
	// output drover cannot read from one that wrote nothing and one that
	// stopped, which read as the same failure to a reader that knows one
	// schema.
	events, known int
	cap           int
	// refused is whether the cap turned a request-changes away during this pass. It is
	// the difference between a review that found nothing and one that found
	// something it was not allowed to write down.
	refused bool
	// body is the digest of the item file read once the session ended, and
	// after.LastBody is the last one drover recorded. A difference is a write
	// none of drover's commands made. exact is the same file's exact-bytes
	// digest, the form a ledger written before bodySum carries.
	body, exact string
	// text is that same file, which is where a block an earlier round sent back
	// either still stands or has been cleared.
	text string
	// leftRunning is each process the session started and left running when
	// it exited. Its report is about work that had not finished.
	leftRunning []string
}

// readBody fills body, exact and text from the item file as the pass ended. An
// unreadable file leaves all three empty, which no recorded digest matches, so
// a body that vanished during the pass parks rather than merges.
func (p *pass) readBody(path string) {
	b, err := os.ReadFile(path) // #nosec G304 -- the path came from the backlog
	if err != nil {
		return
	}
	p.body, p.exact, p.text = bodySum(b), digestBytes(b), string(b)
}

// openBlock reports a body whose sent-back section holds findings rather than
// the statement `drover close-changes` writes.
func openBlock(body string) bool {
	d := prompts.Data{Body: body}
	return d.Changes() != "" && !d.Cleared()
}

// What one review pass amounted to. The tokens are the vocabulary the JSON and
// the park reasons share, so a caller keying on one is keying on the other.
const (
	passDied              = "died"              // the session left no terminal event
	passFaulted           = "faulted"           // the service or the account failed, not the work
	passUnreadable        = "unreadable"        // it left a stream drover has no schema for
	passSilent            = "no_record"         // it recorded nothing at all
	passCapped            = "at_limit"          // request for changes refused, and it wanted to send work back
	passRepeating         = "repeating"         // the same evidence twice
	passChangesRequested  = "changes_requested" // work went back to the branch
	passStanding          = "findings_open"     // an audit, no request for changes, and an earlier round's block still open in the body
	passChangesUnrecorded = "no_digest"         // an audit, no request for changes, and no body digest to read it against
	passEdited            = "body_edited"       // an audit, no request for changes, and a body drover did not write
	passPassed            = "passed"            // nothing came back
)

// passResult reads one review pass from the rows it left and the log it wrote.
//
// drive merges what it drives, so this is one half of the go/no-go and the gate
// is the other. Only passPassed reaches driveFinish: passSilent, passDied,
// passFaulted, passUnreadable, passCapped, passRepeating, passStanding,
// passChangesUnrecorded and passEdited each park the item, and a parked item is never
// merged. Merging by default is not merging unconditionally, and every token
// below is one of the conditions.
//
// The order is the whole design, and row 5 before rows 8 to 11 is the sharpest
// part of it. max_review_rounds is enforced inside `drover request-changes`, which exits 3, so a
// capped review that wanted to send work back records its audit row, gets
// refused, and leaves exactly the signature of a clean pass: an audit, no
// changes row and an untouched body. A predicate that tested for a pass first would
// merge an item whose review was still finding defects, which is the failure
// the cap exists to prevent.
//
// Row 1 asks whose stream it is before calling anything dead. A reviewer whose
// events no runlog schema claims executed correctly and reads as silence, so
// "it died rather than finished" is a true sentence about a session that never
// started and a false one about this. A log of events drover does recognize
// with no terminal one among them is the session that stopped, and stays row 3:
// the discriminator is whether anything claimed the events, never how many
// there were.
//
// Row 2 is the one a queue depends on. A runner reporting a transport failure
// or an exhausted account reviewed nothing, and "it died" is a sentence about a
// session: it sends an operator to the log hunting a cause the session never
// had. The runner named the cause in its own terminal event, so passFaulted
// carries it into the park, and tells drive that the item after this one would
// meet the same wall.
//
// Row 4 fails closed. `drover request-changes`'s own help states the limit it inherits:
// drover cannot tell a pass that found nothing from one that found something
// and did not write it down. Guessing in the direction that merges is the one
// guess this must not make.
//
// Rows 9 to 11 are that same refusal carried one row further. A review that
// records its audit and then sends its findings back by editing the item body
// leaves an audit and no request-changes row, which is the signature of a
// clean pass, even when each audit note names what it sent back.
//
// What tells the two apart is the body itself, not the reviewer's account of
// it. The review's launch row carries a digest of the item file, and every
// drover command that writes an item records the digest of what it wrote, so
// the last recorded digest is the body as drover last left it. A pass whose
// body still matches carries no findings written by hand and passes; one whose
// body does not is passEdited and parks. A digest is required, not assumed: an
// item with no digest on record cannot be read either way and stays
// passChangesUnrecorded.
//
// Row 8, passStanding, sits ahead of all three, because the body can contradict an
// unchanged digest. A review that finds an earlier round's findings still open
// and writes nothing leaves the block where it was, the digest where drover
// left it, and an audit with no request for changes: the signature of a clean pass over a
// body that says the opposite. A review can record exactly that, "R4 UNMET,
// unchanged", and drive would read it as passed. It parks rather
// than looping as passChangesRequested: no changes row was written, so max_review_rounds never
// counts the round and the loop would have no bound.
//
// No request for changes count enters it. An item sent back earlier whose later pass edits
// the body by hand is the same event as a first pass that does, and reading
// the earlier request for changes as a baseline would merge it.
//
// Each needs its own token: park keys on the cause a wrapper matches, and
// reusing silent would report "recorded nothing" about a pass that recorded an
// audit row and a note.
func passResult(p pass) string {
	if v := runResult(p); v != "" {
		return v
	}
	return rowResult(p)
}

// rowResult is rows 4 to 11 of passResult: what the ledger answers once the log
// has said the session finished.
func rowResult(p pass) string {
	audits := p.after.Audits - p.before.Audits
	changes := p.after.ChangesRequested - p.before.ChangesRequested
	switch {
	case audits == 0 && changes == 0:
		return passSilent
	// The refusal, not the count. An item at the cap whose review sent nothing
	// back is a review that found nothing, and reading the count alone makes that
	// unsayable: once an item has taken max_review_rounds rounds, every later pass on it
	// would be capped whatever the reviewer did, so drive could never merge it and
	// open-pr would refuse it. A refusal row is written when the cap actually
	// turns a request-changes away, which is the evidence the count would stand
	// in for.
	case p.cap > 0 && p.refused:
		return passCapped
	case changes >= 1 && p.after.RepeatedChanges:
		return passRepeating
	case changes >= 1:
		return passChangesRequested
	case audits >= 1 && !p.refused && openBlock(p.text):
		return passStanding
	case audits >= 1 && p.after.LastBody == "":
		return passChangesUnrecorded
	case audits >= 1 && !bodyMatches(p.after.LastBody, p.body, p.exact):
		return passEdited
	case audits >= 1:
		return passPassed
	}
	return passSilent
}

// runResult is what a log alone can say about the session that wrote it, and
// empty for one that finished. Rows 1 to 3 above, held here so the implement
// stage reads them too.
//
// It cannot be passResult. Every row below these three is a ledger delta about a
// review, and an implementation writes neither an audit nor a request for changes, so
// passResult reads every clean implement run as passSilent. Read in the other
// direction, an implement run drive never looked at spends a reviewer on an
// unchanged branch: a log with a few events and no result, a review that reads
// passSilent, and a park blaming the reviewer for a branch the implementer never
// wrote. The operator then reads the reviewer's log for a cause that is in the
// implementer's.
func runResult(p pass) string {
	switch {
	case !p.logged && p.events > 0 && p.known == 0:
		return passUnreadable
	case p.logged && p.res.Faulted():
		return passFaulted
	case !p.logged || !p.res.Succeeded() || len(p.leftRunning) > 0:
		return passDied
	}
	return ""
}

// The park causes for an implement run that left drover nothing to read. They
// are the pass tokens under a stage prefix rather than the bare tokens: park is
// what a wrapper keys on, and a wrapper reading "died" for both stages would
// answer an implement failure by re-running a review against a branch holding
// no work.
const (
	parkImplementDied       = "implement_died"
	parkImplementUnreadable = "implement_unreadable"
	// parkNoCommits is a clean tree on a branch holding nothing past the trunk.
	// Its own token because the recovery differs: a died run leaves a log to
	// read, and this leaves a session that ran and produced nothing, so there is
	// no point pushing the branch or paying for a review of an empty diff.
	parkNoCommits = "no_commits"
	// parkUncommitted is work the item's own sessions left in its worktree. It
	// parks this item and no other: the tree is the item's alone.
	parkUncommitted = "uncommitted_work"
	// parkWorktree is an item whose worktree could not be opened, linked or set
	// up. Nothing was launched for it.
	parkWorktree = "worktree_failed"
	// parkHandEdited is an accepted item whose body changed outside drover
	// since the last digest it recorded, held before any session until the
	// operator records the edit with amend or touch.
	parkHandEdited = "hand_edited"
)

// launch is one stage of one item: the stage row, then the runner, headless, to
// a fresh log. The stage event is emitted here, once the log's path is chosen,
// so the event and the banner name the file the runner writes; each relaunch
// launchOrWait makes after a fault writes a log of its own and emits its own.
//
// It takes the runner and the body as parameters and reads no flag variable,
// because drive drives many items and a flag holds one value. Both callers
// record before launching, for the reason session.launched already gives: a
// session killed halfway still happened.
func (s *session) launch(ctx context.Context, it backlog.Item, stage, name, logBase, body string, banner launchInfo) (runner.Outcome, error) {
	spec, ok := s.cfg.Runners[name]
	if !ok {
		return runner.Outcome{Exit: -1}, fmt.Errorf("unknown runner %q (declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
	}
	logPath := filepath.Join(s.cfg.LogDir(),
		runlog.Name(s.cfg.Paths.LogPrefix+logBase, time.Now()))
	// Printed here rather than in driveItem, so that every stage of every round
	// carries it. A drive that reported only what it resolved at the top would
	// say nothing about the second round, which is the one whose prompt differs.
	banner.Log = displayPath(s.cfg.Root, logPath)
	events.session(it.ID.Bare(), stage, banner.Log)
	banner.from(name, spec, runner.Headless).write(os.Stderr)
	// Linked again before every session, not only when the tree opens: a branch
	// update that merges a trunk which stopped tracking a linked path (a
	// CLAUDE.md moved out of git) deletes the branch's copy, and the next session
	// would otherwise run without the file at all.
	if s.tree != "" {
		if err := s.relinkAsked(s.tree); err != nil {
			return runner.Outcome{Exit: -1}, err
		}
	}
	at, err := s.launched(it, stage, name)
	if err != nil {
		return runner.Outcome{Exit: -1}, err
	}
	oc, err := runner.Run(ctx, at, spec, runner.Headless, body, logPath, os.Stderr)
	s.recordUsage(it.ID.Bare(), stage, at.Session, oc)
	if stage == ledger.StageImplement {
		s.mirrorNow(it)
	}
	return oc, err
}

// How drive answers a transient fault. Two relaunches, backing off 45 seconds
// then 90, which covers the shape the logs actually show: a 500 or a 529 that
// clears in under a minute. A third would spend a fourth session to learn what
// three already said.
const faultRetries = 2

// Both are var rather than const so a test can drive a real wait and the
// relaunch after it in under a second. Nothing else writes them.
var (
	faultBackoff = 45 * time.Second
	// quotaSlack is added to a wait that runs to a stated reset. Two clocks
	// disagree by seconds, and arriving on the instant buys a second refusal
	// and a park.
	quotaSlack = time.Minute
)

// launchOrWait is launch, plus the only two failures worth answering by
// waiting.
//
// A transport failure and an exhausted account are the runner reporting about
// itself rather than about the work, so the same prompt run later is the same
// prompt: relaunching costs a session and changes nothing else. Every other
// failure returns on the first attempt, because a session that ran and failed
// has produced a result and running it again would overwrite it.
func (s *session) launchOrWait(ctx context.Context, it backlog.Item, stage, name, logBase, body string,
	banner launchInfo, deadline time.Time,
) (runner.Outcome, error) {
	for attempt := 0; ; attempt++ {
		oc, err := s.launch(ctx, it, stage, name, logBase, body, banner)
		if !oc.Result.Faulted() || ctx.Err() != nil || s.holdsRefusals() && quotaRefused(oc.Result) {
			return oc, err
		}
		wait, why := faultWait(oc.Result, attempt, deadline, time.Now())
		if wait <= 0 {
			return oc, err
		}
		fmt.Fprintf(os.Stderr, "  · %s: %s reported %s, not a failure of the work; %s\n",
			it.ID.Bare(), name, oc.Result.Fault, why)
		if !sleepFor(ctx, wait) {
			return oc, err
		}
	}
}

// holdsRefusals reports a drive whose usage_admission answers a quota refusal
// by holding rather than by waiting in place or parking: every drive but one
// whose policy is off, where the park stands as it did before the action
// existed.
func (s *session) holdsRefusals() bool {
	if s.slot == nil || s.slot.sched.usage == nil {
		return false
	}
	p, _ := s.cfg.Policy("usage_admission")
	return p != polOff
}

// faultWait is how long to hold before relaunching, and zero for "do not".
//
// The quota arm waits only inside a bound the operator granted. --for and
// --until are the operator saying how long this drive may own the terminal, so
// sleeping to a reset inside one spends time already given; an unbounded drive
// has no such grant, and sleeping until tomorrow morning on a runner's say-so
// is a night nobody agreed to. That drive stops instead, with the instant in
// the park, which is a sentence an operator can act on.
//
// A quota with no stated instant stops for the same reason under a different
// cause: drover would be picking the number, and the one it picked would be
// wrong in whichever direction costs more.
func faultWait(r runlog.Result, attempt int, deadline, now time.Time) (time.Duration, string) {
	switch r.Fault {
	case runlog.FaultTransient:
		if attempt >= faultRetries {
			return 0, ""
		}
		d := faultBackoff << attempt
		if !deadline.IsZero() && now.Add(d).After(deadline) {
			return 0, ""
		}
		return d, "retrying in " + d.String()
	case runlog.FaultQuota:
		if r.RetryAt.IsZero() || deadline.IsZero() || !r.RetryAt.After(now) || r.RetryAt.After(deadline) {
			return 0, ""
		}
		return r.RetryAt.Sub(now) + quotaSlack, "waiting until " + r.RetryAt.Format(time.RFC3339)
	}
	return 0, ""
}

// sleepFor reports whether it slept the whole duration, so a cancelled drive
// stops at the wait rather than after it.
func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// faultReason is the park an operator reads at nine in the morning: which side
// failed, what the runner itself said, where the evidence is, and when it will
// serve again where the runner named an instant.
//
// The runner's own sentence is quoted rather than summarized. It is the only
// part of this that says 529 rather than 500, or names the account rather than
// the service, and a park that paraphrased it would send the reader to the log
// for the one line drover already had.
func faultReason(who string, res runlog.Result, logPath string) string {
	side := "the service was unreachable"
	switch res.Fault {
	case runlog.FaultQuota:
		side = "the account is out of budget"
	case runlog.FaultAuth:
		side = "the runner has no credentials the service accepts"
	}
	line := fmt.Sprintf("%s stopped on %s and not on the work: %q (%s)",
		who, side, trim(res.Text, 300), whereItRan(who, logPath))
	if !res.RetryAt.IsZero() {
		line += ". It serves again at " + res.RetryAt.Format(time.RFC3339)
	}
	return line
}

func trim(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// reload re-reads the backlog and the ledger from disk.
//
// Both go stale the moment a launch returns. open() reads each once and
// session.record appends only the rows this process writes, so an agent's own
// `drover audit` and `drover request-changes` land in a child process and reach nothing
// here. A drive that skipped this would read every count delta as zero, take
// every review for a pass, and merge the lot.
func (s *session) reload() error {
	next, err := open()
	if err != nil {
		return err
	}
	s.backlog, s.ledger, s.shelf = next.backlog, next.ledger, next.shelf
	return nil
}

// DriveItem is one item's whole account, and the unit of --json.
//
// It replaces the items.jsonl an outside harness kept. Park carries the cause a
// wrapper keys on and Reason the sentence a person reads at nine in the
// morning: a wrapper matching on prose breaks the day somebody rewords an
// error.
type DriveItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Branch string `json:"branch,omitempty"`
	// Tree is the item's worktree while one is left standing for the operator:
	// a park on uncommitted work keeps it, and a clean exit removes it.
	Tree    string `json:"tree,omitempty"`
	Outcome string `json:"outcome"`
	Park    string `json:"park,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Detail is why the session or the push under a park failed, one of the
	// detail tokens, with Evidence the line that says so and Log the log it
	// came from. Every park carries a detail; the other two are absent where
	// nothing was read.
	Detail   string `json:"detail,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Log      string `json:"log,omitempty"`
	// Rounds is implement launches past the first; Reviews is review launches.
	// Both unconditional: a drive that reviewed nothing is a fact the reader
	// needs, and an omitted zero reads as a build without the field.
	Rounds  int `json:"rounds"`
	Reviews int `json:"reviews"`
	// Requests for changes and Audits are the rows this drive's reviews added, not the item's
	// totals. `drover show` carries those.
	ChangesRequested int    `json:"changes_requested"`
	Audits           int    `json:"audits"`
	Implementer      string `json:"implementer,omitempty"`
	Reviewer         string `json:"review_runner,omitempty"`
	SameRunner       bool   `json:"same_runner,omitempty"`
	// PR is the pull request this item's branch has, which exists only for a
	// branch the gate and the review both passed. Zero says none opened, which
	// is every park before the gate went green, and also the one case gh printed
	// a URL drover could not read a number out of.
	PR     int  `json:"pr,omitempty"`
	Merged bool `json:"merged,omitempty"`
	// Finished is a merge this drive also recorded, ledger row and tracker closes,
	// which only a merge drover performed itself can be: one the forge queued
	// completes after drive has moved on, and the next drive finishes it.
	Finished bool `json:"finished,omitempty"`
	// CatchUp is present only where drive merged the trunk into the branch, and
	// says what the item's own verify command made of the tree that produced.
	CatchUp *CatchUp `json:"update_branch,omitempty"`
	// Slot is which of --jobs' slots the item ran in, from 1. Absent at --jobs 1,
	// where there is only one and the stream reads as it did before there were
	// more.
	Slot int `json:"slot,omitempty"`
	// Findings is every register row and scaffold attributed to this item's
	// sessions, with the routing result each had when the drive noticed it.
	// RegisterError is why the register could not be read for it, which leaves
	// its outcome as it was.
	Findings      []FiledFinding `json:"findings,omitempty"`
	RegisterError string         `json:"register_error,omitempty"`
	Started       string         `json:"started"`
	Ended         string         `json:"ended"`
	// stop ends the whole drive rather than this item, and carries the sentence
	// drive prints on its way out. Unexported: it is a control signal, not part
	// of the document a wrapper reads.
	stop error
	// login is the command that signs the faulted runner in, on an auth park:
	// what needs_operator tells the operator to run.
	login string
	// head is the commit whose checks drive last settled, which the re-check at
	// the end of the drive reads again for an item parked on them.
	head string
	// signoff is the same for an [operator] requirement park: which ones, and
	// the command that records them.
	signoff string
	// blockedBy is the unmerged dependencies of an item skipped as blocked,
	// which the exit summary names with what each came to.
	blockedBy []string
	// handed is a park whose hand-off drive already emitted, carrying where to
	// work, so the item's own parked event does not emit a second, emptier one.
	handed bool
	// requeue is the boundary a quota-refused launch sends the item back to,
	// at the head of the queue, and refusal the hold that refusal takes. An
	// item carrying one is not done: the dispatcher queues it again.
	requeue *lease.Hold
	refusal *usageStep
}

// CatchUp is one catch-up that merged something: the trunk commit the branch
// last held, the origin commit merged into it, and the gate's word on the
// result.
type CatchUp struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Verify string `json:"verify"`
}

// What the item's gate said about a caught-up tree. failed is this branch
// meeting the trunk; trunk_red is the trunk failing on its own; sequence is a
// numbered file the branch added that clashes with or sorts below the trunk's,
// found before the gate ran.
const (
	catchUpPassed     = "passed"
	catchUpFailed     = "failed"
	catchUpSequence   = "sequence"
	catchUpTrunkRed   = "trunk_red"
	catchUpSkipped    = "skipped"
	catchUpUnrunnable = "unrunnable"
)

// What an item amounted to.
const (
	outcomeReviewed = "ready_to_merge" // green and reviewed, nothing merged it
	outcomeParked   = "parked"         // stopped; a person reads the reason
	outcomeSkipped  = "skipped"        // not attempted: done, scaffolded, blocked, broken
	// outcomeMergedByGate is a merge drover made itself because the forge would
	// not queue one, with [drive] merge_gate as the only authority that said yes.
	// Its own outcome rather than reviewed-and-merged, because a reader auditing
	// what reached the trunk unattended needs these apart from the merges branch
	// protection decided.
	outcomeMergedByGate = "merged_by_drover"
	// outcomeMergedAtForge is an --auto request the forge merged on the spot
	// because nothing at the forge had to wait, rather than queueing it.
	outcomeMergedAtForge = "merged_immediately"
)

// The park causes the merge fallback adds. gate_refused is shared with the first
// verify, which refuses before any pull request exists; this one refuses with the
// pull request open, so the two read apart by d.PR.
const (
	parkGateRefused   = "gate_refused"
	parkTrunkConflict = "trunk_conflict"
	parkTrunkRed      = "trunk_red"
)

var (
	driveFor      time.Duration
	driveUntil    string
	driveMax      int
	driveRunner   string
	driveReviewer string
	driveDry      bool
	driveJSON     bool
	driveNoMerge  bool
	driveFollow   bool
	driveJobs     int
	driveCatchUp  bool
	// driveReviewOnly is --review-only, and reviewOnlyID the item it names,
	// resolved: that item's first round skips its implement run and reviews
	// the branch as it stands. An item --follow or --enqueue adds is not it.
	driveReviewOnly bool
	reviewOnlyID    string
)

var driveCmd = &cobra.Command{
	Use:   "drive [id...]",
	Short: "Walk the ready queue unattended: implement, review, and merge what the merge gate passes.",
	Long: `drive is run and review in a loop, over a queue, bounded by a clock.

With no ids it walks every ready item in backlog order. With ids it drives those,
in the order given, which is also how an item a previous drive parked resumes:
a parked item has a clean log behind it and so reads as ` + "`implemented`" + `, which keeps it
out of the queue until somebody who read the reason names it.

Each item runs, then reviews, and the ledger says what the review did. A review
that sent work back means another run; one that recorded nothing parks the item,
because drover cannot tell a pass that found nothing from a pass that found
something and did not write it down, and guessing toward "finished" is the guess
that ships a defect. [review] max_review_rounds bounds the loop; at the cap the item
parks rather than passing, since an at-limit review is one that wanted to send work
back and was refused.

A runner that fails on the service rather than on the work is answered before
the ledger is asked anything. A transport failure is retried twice, 45 seconds
then 90; a spent account is waited out only when the runner named the instant it
serves again and that instant falls inside --for or --until; a runner with no
credentials is never waited on. Claude Code's own reset instant is read from its
stream, the last rate_limit_event's epoch before the sentence in its result.
--until takes a time of day, so a reset more than a day out, as a weekly limit's
is, can only be covered by --for. Anything left parks the item ` + "`runner_fault`" + `,
quoting what the runner said, and ends the drive: an account with nothing in it
is empty for the next item too.

Every park carries a detail beside its cause: why the session or the push under
it failed. runner_exit, tool_denied, background_left_running, stream_cut, auth,
oom, push_hook, push_network, or unknown with the log's last lines. The ledger
keeps it, and status shows the item as parked: <park> (<detail>) until it runs
again. A review that died, was cut, or was killed is rerun once on the same
head ([supervise] rerun_lost_review); a second loss parks. A runner whose
credentials expired stops the drive, and before the first launch drive asks
each runner it will launch whether it is signed in, where the runner offers a
check, and refuses with exit 3 when one is not. A pre-push hook that refuses a
branch parks push_failed (push_hook) for the operator: drover never pushes with
--no-verify.

--review-only <id> skips that item's implement run and reviews its branch as it
stands, then runs the merge gate, opens the pull request and merges it as a
clean pass would. It is how a lost review is rerun by hand.

--dry-run resolves the same queue under the same merge gate, so an item it
reports as skipped is one the run skips and the count is what the run would
attempt. A preview answering a different question than the run is worse than
no preview.

Before the first launch it prints what it resolved for the whole queue — the
ids, both runners, both prompt templates and what stops it — and before each
launch the item, the branch, the template, the binary PATH found, the argv and
the log. A drive that carried something unexpected is then in the scrollback
rather than only in --dry-run.

Every launch is headless, and an item that comes through green and reviewed is
merged: running drive is the operator saying the code may merge, and the merge gate
and the review are what stand between a session and the trunk. Both have to
speak. A park is never merged, [drive] merge_gate has to exit 0, and a branch
touching [drive] protected_paths is left for a human. --no-merge holds all of it and
stops each item on its branch, which is the exception rather than the norm.

drover pushes the branch and opens the pull request itself, and opens one only
where those two have spoken. Work that came back or parked leaves its branch on
the remote and no pull request, because an open one is the claim that this branch
may merge. Three things withhold the merge and none of them takes that back: a
denied path, --no-merge, and a dep a mid-drive split just added. Each leaves a
pull request for a human, which is the whole reason it exists.

Where the trunk has required status checks, the merge goes through the
forge's own --auto, so branch protection and those checks stay the authority
and the merge completes after drive has moved on. Nothing is finished then: the
next drive's reconciliation writes the ledger row for what the forge finished.
drive asks the forge afterwards whether it merged, because --auto with nothing
to wait on merges on the spot: that item reads merged_immediately and finishes in
this drive, after the merge gate reads the merged head where one is declared. A merge gate
that refuses a head already merged stops the drive.

Where [drive] merge_gate is declared and the trunk has no required checks (or
they cannot be read, or the forge refuses auto-merge), [drive] merge_gate is
the only authority, and drive acts on it without --auto: it waits for the head
commit's checks, runs the merge gate against that head, and on exit 0 merges with
--match-head-commit and [git] merge_method, then finishes the item in the same
drive. A refusal there parks gate_refused with the pull request open.

Every merge moves the trunk under the items after it, so before the merge gate reads
a branch drive merges in whatever the trunk gained since the branch was
pushed. No session tested that tree, so drive runs the item's own [verify]
command on it, the one its run was told to pass, before anything is pushed.
Green, it pushes and waits for the new head's checks. Red, it runs the same
verify command on the trunk alone: a trunk that passes means the two fail together, so
the merge is pushed, its output goes back to the branch as a request for changes, and the
item parks gate_refused; a trunk that fails alone parks trunk_red, pushes
nothing and stops the drive. A conflict parks trunk_conflict naming the files,
and nothing is ever force-pushed.

Before the verify command or the merge gate reads a pushed head, drive waits for its checks and
classifies each red one ([forge] in docs/configuration.md): infra, a log line
from the built-in list or [forge] infra_patterns, a job that ran no step or a
canceled run, is rerun with gh run rerun --failed up to [forge] reruns times
per head; trunk, a check that also failed on the trunk's latest completed
run, parks trunk_red without a request for changes and stops the drive; code goes to the
merge gate, and a refusal that follows parks gate_refused naming the check and
the lines its log says failed ([forge] failure_patterns). A head with no check run after [forge] checks_wait is reported
as no workflow ran on this head. Each classification is a ledger row. An infra
check drive parked rather than reran (rerun_infra_failure below auto) is read
once more after the queue drains, unless the drive is stopping: a head whose
checks all passed and that the branch still points at gets a green row and
goes through the merge gate and the merge then, and one still red is left parked.

Each park is said on stderr as it happens, before the next item starts:
  ! <id> parked <cause> at <short head>: <first line of the reason>

Each merge drive makes or finishes moves the trunk under every other open drive
pull request too: one waiting at the forge, or on the operator for a denied
path. So after each one, under [supervise] update_open_prs, drive updates
every item whose recorded pull request is still open, one at a time inside the
same trunk stage: the trunk merged in, the item's verify command on the
merged tree, the push. The item stays where it was. A verify command that fails is sent back
it as above; a conflict is aborted, pushes nothing, and goes to whoever
[supervise] resolve_conflict names, with the files and the worktree.
--update-branches does the same by hand, for a merge made outside drive, and exits.

[drive] merge_gate exiting 4 says the head is only behind the trunk. Under
[supervise] retry_moved_trunk drive updates the branch and asks once more;
a second 4 parks gate_refused and sends nothing back.

--follow adds, after each finish, the ready items that finish unblocked to
the items waiting for a slot, so a chain of dependent items runs in one drive.
Without it the list drive started with is the list it walks.

--jobs N implements and reviews up to N items at once, each in its own
worktree. Everything that reads or moves the trunk stays one item at a time, in
the order the items were dispatched: the branch update, the item's [verify] run on the
merged tree, the push after the review, the pull request and the merge. A
branch update merges what the trunk gained and the verify command reads that tree, so two
items updating at once would each be verified against a trunk the other had
already moved. Two items whose bodies cite an overlapping path never run at
once, nor does an item beside a dep named earlier in the same drive: the later
one waits until the earlier finishes or parks, and --dry-run prints the waves it
would dispatch and names each pair it holds apart. A stop
(trunk_red, a runner fault, a verify command or merge gate that cannot answer) or an interrupt
dispatches nothing more, and each running item finishes the stage it is in and
parks stopped. --max-items and the deadline count items dispatched. Under
--json each object names the slot it ran in, and objects arrive in the order
items finish. --jobs 1, the default, is the drive as it ran before slots.

--enqueue appends the ids to the queue of the drive already running in this
checkout and exits, printing each id's position. The running drive takes them
at its next dispatch, behind what it already holds, and holds them apart from
a dep queued earlier or an item citing an overlapping path, as it does the ids
it started with. With no drive running it starts one with those ids, so the
same command works either way, and the flags beside it apply only then. An id
already queued or running there, or not ready (a scaffold, done, or blocked on
a dep nothing queued ahead of it will run) is refused, the rest are queued, and
the exit is 3 if any id was refused. A drive that has stopped dispatching
takes no more ids.

--pause tells the drive running in this checkout to dispatch nothing more and
hold each running item at its next stage boundary, parking nothing; the drive
and its lease stay up. --resume reloads .drover.toml, the prompt and review
templates and [verify], checks them as a launch does, and continues each item
from where it was held; one whose verify command changed runs the new one
before it goes on. A resume whose config or preflight fails exits 3 and the
drive stays paused. A running drive keeps the config it started or last
resumed with: when .drover.toml on disk changes, it says so once per edit,
with a "config changed on disk" line and a config_changed event naming the
tables that differ, and applies nothing; --pause, then --resume, applies it.
--cleanup lists the worktrees under the drive tree
directory, the branches an item rendered under a branch template since
changed, and the report stubs with no result; --cleanup --apply removes the
clean worktrees no drive holds and the orphan branches holding no commit, and
never touches uncommitted work. A supervisor runs --pause and --resume under
[supervise] pause_for_config_change and --cleanup under
cleanup_interrupted_drive.

--stop tells the drive running in this checkout, through its lease and with
no signal, to drain as an interrupt does: it dispatches nothing more, each
running item finishes the stage it is in and parks stopped, and the drive
exits once they have. A paused drive stops the same way.

--detach starts the drive in a session of its own (setsid), with no
terminal, stdin on /dev/null and stdout and stderr in a log file, so the
process that launched it can exit or restart under it. It prints the drive's
id and the paths of its log and its event file once the drive holds the
lease, and exits 0; a drive that fails before then has its error printed and
its exit code passed on. Every other flag means what it does without
--detach. drover attach follows a drive and drover watch waits on one.

--pause --all and --resume --all act on the drive in every project in the
per-user registry at once, each project judged by its own
pause_for_config_change, and print one line per project: paused or resumed
with the drive's id, no drive running, or why not. Each exits 3 when a live
drive did not take it; --resume --all checks each project's config first, and
one that will not load stays paused while the rest resume. --pause --wait
returns only once no paused drive has an item in a session or a trunk stage,
says what it still waits on at most once a minute, and exits 3 at --timeout
(default 2h) naming each drive still busy. An interrupt leaves them paused.

A drive keeps the binary it was launched from. On --resume it compares the
file now at that path with the one it started from, and when an install
replaced it, has the new binary load the project under the drive's own flags
first: a failure is refused as any resume is, and the drive stays paused.
Otherwise it re-executes itself onto the new binary once every running item
holds at a boundary. The lease stays locked across the exec, and the new
process keeps the drive's id, its queue, its deadline and each held item,
which goes on from where it held; its start row names both binaries.
--resume --keep-binary resumes on the binary the drive has.

Each drive that launches takes the next id in its project, D1, D2 and so on,
the moment it holds the lease, and --label gives it a line of text beside the
id. The lease names it from then on, and every row the drive appends and every
event it emits carries it, reconciliation's included. A label names a drive
as it starts and never renames a running one, so --label beside --pause,
--resume, --stop, --cleanup, --apply, --all, --wait, --keep-binary or
--timeout, or on an --enqueue that joins a running drive, is refused (exit 3)
and nothing is paused or queued. A start row records the
ids it queued and --jobs once the queue is read, and an end row records the
exit code and each item's outcome. A drive whose queue comes up empty writes
both with nothing queued; one refused (exit 3) before reading its queue writes
neither unless reconciliation already wrote a row naming it. drover drives
lists them. --dry-run takes no id.

--events, beside --json, also writes one event object per line as things
happen: started, queued, dispatched, preflight, stage, check, parked, filed,
needs_operator, needs_supervisor, merged, paused, resumed, config_changed,
usage_warning and exit. Each carries "event", which is how a reader tells it from an item's
report, "ts", "drive", "id" where one item is meant, and "action" where a [supervise]
catalog action produced it. A park whose action is at operator policy emits
needs_operator. exit carries a summary: what merged, parked, skipped and was
held, each item that did not merge with its cause, the drive's weighted
tokens, and why it ended. Each [notify] destination is sent the events it
routes, at the priority it routes each at, with or without --events; an exit
that left an item parked, skipped or held goes at exit_undone's priority. A
destination that fails is reported once and never stops the drive or the
others. Every drive also appends each event to
$XDG_STATE_HOME/drover/drives/<project>/<drive id>.jsonl whatever the flags,
which is the file attach and watch read; a write there that fails is reported
once and never stops the drive.

After each item's result, drive reads the follow-up list for follow-ups that
were not open when the item was dispatched and the backlog for items it gained.
Each is a finding, and emits filed with "kind" (follow-up or scaffold), "ref"
(the issue number or the item id), "filed_by" (the items running when it
appeared, with "ambiguous" when that was more than one; a scaffold names the
item its "Found while working" line does), and "owners", "coverage" and
"blocked_on". The coverage is queued (an owner is in this drive's queue),
unqueued (an owner is ready or ran), blocked (every unmerged owner is blocked),
orphaned (every owner merged), unowned (no owner link) or scaffold. A follow-up
at blocked, orphaned or unowned stops under [supervise] triage_finding. The
filing item's report carries the same objects as "findings", and
"register_error" when the follow-up list would not read, which changes nothing
else about the item. The summary and the last --json object total the findings
by coverage and list those nothing will carry, with or without --events.

Before spending a session, drive runs the preflight checks [supervise] names:
stale_citations (cited path:line ranges the trunk no longer holds, printed as each
item is dispatched, and held for the operator at operator), deps_lint (a deps
line with a comma or an id that does not parse refuses the drive with exit 3),
worktree_links_gap (files the verify command reads that the worktrees lack, warned before
the first verify command runs) and host_load_admission (at auto, dispatch holds while the
1-minute load exceeds [drive] max_load). --dry-run prints a preflight section;
on Linux with strace on PATH it runs each item's verify command once in your checkout to
see what it reads.

usage_admission reads the newest rate_limit reading Claude Code left on a
usage row of any registered project, before each dispatch and before each
stage that launches a session. Crossing [usage.weekly] or [usage.five_hour]
notify_remaining (the percent of the window left) emits usage_warning once per
window per reset, recorded in the ledger so no drive in any project sends it
twice, and sends it to each [notify] destination routing usage_warning. Crossing pause_remaining, entering
purchased credit under [usage.overage] mode = "pause", reaching max_usd under
mode = "continue", or a reading the service refuses sessions under, holds the
drive: running sessions finish their stage, nothing new starts, and the drive
pauses through its lease as --pause does, emitting paused and needs_operator.
A session refused for quota goes back to the head of the queue at the stage
that was refused instead of parking runner_fault. A hold whose reset falls
inside --for or --until lifts on its own at the reset, emitting resumed;
drive --resume lifts any hold at once. A deadline that arrives first ends the
drive with exit 0 and each item it held reports held. At usage_admission =
"off" nothing holds and a refusal parks runner_fault as before.`,
	Example: `  drover drive --for 8h              # the ready queue, until the clock runs out
  drover drive B1 B4                # these two, in this order
  drover drive --for 8h --no-merge  # drive it all, merge none of it
  drover drive --dry-run            # what it would do, launching nothing
  drover drive --for 2h --json      # one object per line, for a wrapper to keep
  drover drive --for 8h --follow    # also drive what each finish unblocks
  drover drive --for 8h --jobs 3    # three items' sessions at once, one merge at a time
  drover drive B1 --json --events   # events as they happen, beside each item's report
  drover drive --label "night run"  # the ready queue, under a label drover drives prints
  drover drive --enqueue B5 B6      # append to the drive running here, or start one
  drover drive --review-only B3     # review B3's branch as it stands, then the merge gate and the merge
  drover drive --pause              # hold the drive running here, edit the config, then:
  drover drive --resume             # reload it and carry on from each held item
  drover drive --pause --all --wait # hold every registered project's drive until each is quiet
  drover drive --resume --all       # resume them, each onto the binary installed now
  drover drive --cleanup --apply    # remove the clean trees and empty branches drives left
  drover drive --for 48h            # held on a spent week, it resumes at the reset
  drover drive --detach --for 8h    # the same drive, in a session of its own, then:
  drover attach                     # its events from the start, until it exits
  drover drive --stop               # drain it and let it exit, sending no signal
  drover drive --update-branches    # after a hand merge: bring every open drive pull request level

  queued B5 at position 2 in drive pid 4242
  queued B6 at position 3 in drive pid 4242

  {"id":"B1","title":"First","branch":"b1-first","outcome":"ready_to_merge","rounds":1,
   "reviews":2,"changes_requested":1,"audits":2,"implementer":"claude","review_runner":"agy",
   "started":"2026-09-13T22:00:00Z","ended":"2026-09-13T22:41:00Z"}

  {"event":"parked","ts":"2026-09-13T22:41:00Z","drive":"D4","id":"B2","action":"merge_protected_paths",
   "park":"protected_paths","reason":"this branch changes Makefile, ...","policy":"operator","pr":7}

  Bound every drive. --for takes a duration and --until a 24-hour local time;
  passing both is refused rather than settled by precedence, because a wrapper
  would come to depend on which one drover picked.`,
	Args: needIDs("drive [id...]"),
	RunE: func(c *cobra.Command, args []string) (err error) {
		if driveCatchUp && (len(args) > 0 || driveDry) {
			return Refusal{fmt.Errorf("--update-branches updates every open drive pull request and launches nothing, so it takes no ids and no --dry-run")}
		}
		if driveEvents && !driveJSON {
			return Refusal{fmt.Errorf("--events writes into the --json stream; pass both")}
		}
		if err := checkLabel(driveLabel); err != nil {
			return err
		}
		if driveResumeCheck {
			_, _, err := loadDriveInputs()
			return err
		}
		if drivePause || driveResume || driveStop || driveCleanup || driveApply || driveEveryProject || driveWait || driveKeepBinary || c.Flags().Changed("timeout") {
			if driveLabel != "" {
				return Refusal{fmt.Errorf("%s, and this command controls or cleans up drives rather than starting one; drop --label", labelOnlyAtStart)}
			}
			return driveControl(c.Context(), args, c.Flags().Changed("timeout"))
		}
		if driveDetach {
			return detachDrive(c.Context(), os.Stdout)
		}
		if driveTakeover != "" {
			// The process this replaced already resolved its queue, its
			// --enqueue and its --review-only; what it held comes out of the
			// lease instead.
			driveEnqueue, driveReviewOnly, driveDry = false, false, false
		}
		if driveFor > 0 && driveUntil != "" {
			return Refusal{fmt.Errorf("--for and --until both bound the drive; pass one")}
		}
		if driveJobs < 1 {
			return Refusal{fmt.Errorf("--jobs is how many items run at once, so it is at least 1 (got %d)", driveJobs)}
		}
		reviewOnlyID = ""
		if driveReviewOnly && (len(args) != 1 || driveEnqueue) {
			return Refusal{fmt.Errorf("--review-only reviews one item's branch as it stands; name exactly one id, without --enqueue")}
		}

		// refused is the ids --enqueue turned away from the drive it starts,
		// which exit 3 once that drive has run, and the exit event says so.
		var refused error
		if driveEnqueue && driveDry {
			return Refusal{fmt.Errorf("--dry-run previews a drive and --enqueue changes one; pass one")}
		}
		if driveEnqueue && len(args) == 0 {
			return Refusal{fmt.Errorf("--enqueue appends the ids it is given, and none was")}
		}

		// Before the queue is read and before anything is launched, per the
		// contract exit 3 keeps: the tree and the ledger are untouched.
		if err := noNestedDrive(); err != nil {
			return err
		}
		s, err := open()
		if err != nil {
			return err
		}
		if driveReviewOnly {
			it, ierr := s.item(args[0])
			if ierr != nil {
				return ierr
			}
			reviewOnlyID = it.ID.Bare()
		}
		// Before the lease and before --enqueue reaches a running drive, so a
		// line drover would misread costs nothing but this refusal. A takeover
		// carries a queue linted when it was launched or enqueued, and a refusal
		// there would end a drive that is holding items. --update-branches launches
		// no item, so a line in one has nothing to be misread by.
		if driveTakeover == "" && !driveCatchUp {
			if lerr := s.lintQueue(args); lerr != nil {
				sink := newEventSink(s.cfg, driveEvents)
				sink.preflight("", "deps_lint", lerr.Error())
				sink.settle()
				return lerr
			}
		}
		if driveEnqueue {
			res, eerr := s.enqueue(args, driveLabel)
			if eerr != nil {
				return eerr
			}
			if !res.free {
				res.report(os.Stdout, os.Stderr)
				return res.refusal()
			}
			// No drive is running, so this one is it, over the ids that passed.
			res.report(os.Stdout, os.Stderr)
			if len(res.ready) == 0 {
				return res.refusal()
			}
			args, refused = res.ready, res.refusal()
		}
		deadline, err := driveDeadline()
		if err != nil {
			return err
		}
		reviewer, err := s.drivePreflight()
		if err != nil {
			return err
		}
		// One lease on the operator's checkout for the whole drive, and one on
		// each item's worktree while that item runs. The checkout's refuses a
		// second drive, which would run the same queue twice, and run, draft and
		// review, which would work in the tree reconciliation finishes into.
		// finish and open-pr pass it: they wait on the trunk lock instead, and
		// finish refuses on the worktree's.
		//
		// Its own reconciliation calls finish's RunE inside this process, and
		// finish's check reads the same package state this sets, so the drive
		// does not refuse itself.
		//
		// --dry-run is exempt: it resolves every step and launches nothing, and
		// a preview that refused while a drive was running would deny the
		// operator the one read that explains why.
		if !driveDry {
			startedFrom, _ = launchedBinary()
			// lerr, not err: the exit event below must read the named return.
			var release func()
			var lerr error
			if driveTakeover != "" {
				release, lerr = s.adoptLease()
			} else {
				release, lerr = s.takeLease("drive", "")
			}
			if h, busy := errors.AsType[lease.Held](lerr); busy && driveEnqueue && h.Lease.Command == "drive" {
				// Another --enqueue started a drive between this one finding
				// the checkout free and taking it: append to that drive instead.
				return s.enqueueTo(args, driveLabel)
			}
			if lerr != nil {
				return lerr
			}
			defer release()
			defer closeQueue("the drive is exiting")
			if len(s.cfg.Status.Render) > 0 {
				activeRender.Store(&driveRenderer{targets: s.cfg.Status.Render})
				defer activeRender.Store(nil)
			}
			// Made whether or not --events streams, because [notify] is owed
			// every event it routes either way.
			events = newEventSink(s.cfg, driveEvents)
			events.forge(s.labels())
			defer func() {
				s.endDrive(err)
				s.driveSync("after the drive")
				events.exit(err)
				events = nil
				activeDrive.Store(nil)
			}()
			if driveTakeover != "" {
				return s.driveTakeoverRun(c.Context(), reviewer)
			}
			if err := s.allocDrive(); err != nil {
				return err
			}
			s.driveSync("before the queue")
			if p, _ := s.cfg.Policy("deps_lint"); p != polOff && !driveCatchUp {
				events.preflight("", "deps_lint", reasonClean)
			}
		}
		if driveCatchUp {
			return s.catchUpByHand(c.Context())
		}
		if err := s.drive(c.Context(), args, reviewer, deadline); err != nil {
			return err
		}
		return refused
	},
}

// driveLabel is --label.
var driveLabel string

// labelMax bounds --label. It is printed on the lease line beside a pid and a
// timestamp, and a paragraph there pushes the queue off the screen.
const labelMax = 80

// labelOnlyAtStart opens the refusal of --label where no drive starts, which
// would otherwise exit 0 with the label recorded nowhere.
const labelOnlyAtStart = "--label names a drive only when it starts"

// checkLabel refuses a label the lease line and the drives table cannot print
// on one line.
func checkLabel(label string) error {
	if strings.ContainsAny(label, "\r\n") {
		return Refusal{fmt.Errorf("--label is one line, and this one holds a line break")}
	}
	if n := utf8.RuneCountInString(label); n > labelMax {
		return Refusal{fmt.Errorf("--label is at most %d characters (got %d)", labelMax, n)}
	}
	return nil
}

// driveDeadline resolves --for or --until to a wall-clock instant, or zero when
// the drive is unbounded. --until rolls to tomorrow when the time has passed,
// which is what "stop at 06:00" means at eleven at night.
func driveDeadline() (time.Time, error) {
	switch {
	case driveFor > 0:
		return time.Now().Add(driveFor), nil
	case driveUntil != "":
		hm, err := time.Parse("15:04", driveUntil)
		if err != nil {
			return time.Time{}, Refusal{fmt.Errorf("--until wants a 24-hour local time, 00:00 to 23:59 (got %q)", driveUntil)}
		}
		now := time.Now()
		at := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	return time.Time{}, nil
}

func init() {
	driveCmd.Flags().DurationVar(&driveFor, "for", 0, "stop after this long (8h, 90m); mutually exclusive with --until")
	driveCmd.Flags().StringVar(&driveUntil, "until", "", "stop at this 24-hour local time; mutually exclusive with --for")
	driveCmd.Flags().IntVar(&driveMax, "max-items", 0, "stop after this many items (default: the whole queue)")
	driveCmd.Flags().StringVar(&driveRunner, "runner", "", "override the implementing runner for this drive")
	driveCmd.Flags().StringVar(&driveReviewer, "review-runner", "", "override [run] review_runner for this drive")
	driveCmd.Flags().BoolVar(&driveDry, "dry-run", false, "resolve every step and launch nothing")
	driveCmd.Flags().BoolVar(&driveJSON, "json", false, "one object per line on stdout, flushed as each item ends")
	driveCmd.Flags().BoolVar(&driveEvents, "events", false,
		"with --json, also write one event object per line as things happen: queued, dispatched, preflight, stage, check, parked, filed, needs_operator, needs_supervisor, merged, paused, resumed, reexec_waiting, exit")
	driveCmd.Flags().BoolVar(&driveNoMerge, "no-merge", false,
		"drive the queue and merge nothing, leaving each item green-and-reviewed on its branch; the exception, not the norm")
	driveCmd.Flags().BoolVar(&driveFollow, "follow", false,
		"after each finish, add the ready items it unblocked to the queue, until it empties or a bound stops it")
	driveCmd.Flags().BoolVar(&driveCatchUp, "update-branches", false,
		"merge the trunk into every open drive pull request, run each one's verify command and push it, then exit; for a merge made outside drive")
	driveCmd.Flags().BoolVar(&driveEnqueue, "enqueue", false,
		"append the ids to the queue of the drive running in this checkout, or start one with them when none is")
	driveCmd.Flags().BoolVar(&driveReviewOnly, "review-only", false,
		"review the one named item's branch as it stands, with no implement run, then run the merge gate, open its pull request and merge it as any clean pass")
	driveCmd.Flags().BoolVar(&drivePause, "pause", false,
		"tell the drive running here to dispatch nothing more and hold each running item at its next stage boundary")
	driveCmd.Flags().BoolVar(&driveResume, "resume", false,
		"reload .drover.toml, the templates and [verify] into the paused drive running here, and continue it")
	driveCmd.Flags().BoolVar(&driveStop, "stop", false,
		"tell the drive running here to dispatch nothing more, let each running item finish its stage and park stopped, then exit")
	driveCmd.Flags().BoolVar(&driveDetach, "detach", false,
		"start the drive in a session of its own, logging to a file, and return once it holds the lease; drover attach follows it")
	driveCmd.Flags().BoolVar(&driveEveryProject, "all", false,
		"with --pause or --resume, act on the drive in every project in the per-user registry")
	driveCmd.Flags().BoolVar(&driveWait, "wait", false,
		"with --pause, return only once no paused drive has an item in a session or a trunk stage")
	driveCmd.Flags().DurationVar(&driveTimeout, "timeout", 2*time.Hour,
		"with --wait, give up after this long and exit 3, naming each drive still busy")
	driveCmd.Flags().BoolVar(&driveKeepBinary, "keep-binary", false,
		"with --resume, carry on under the binary the drive started with even when a newer one is installed at its path")
	driveCmd.Flags().StringVar(&driveTakeover, "takeover", "", "")
	driveCmd.Flags().IntVar(&driveLeaseFD, "lease-fd", 0, "")
	driveCmd.Flags().BoolVar(&driveResumeCheck, "resume-check", false, "")
	for _, name := range []string{"takeover", "lease-fd", "resume-check"} {
		_ = driveCmd.Flags().MarkHidden(name)
	}
	driveCmd.Flags().BoolVar(&driveCleanup, "cleanup", false,
		"list the worktrees, orphan branches and report stubs interrupted drives left in this project")
	driveCmd.Flags().BoolVar(&driveApply, "apply", false,
		"with --cleanup, remove the clean worktrees no drive holds and the orphan branches holding no commit")
	driveCmd.Flags().StringVar(&driveLabel, "label", "",
		"a free-text label for the drive this starts, at most 80 characters on one line, recorded with its id; refused where no drive starts")
	driveCmd.Flags().IntVar(&driveJobs, "jobs", 1,
		"implement and review up to this many items at once; branch update, verify command, merge gate, push, pull request and merge stay one at a time")
	root.AddCommand(driveCmd)
}

// drivePreflight refuses a project no drive could run in, and names the
// reviewing runner. It runs before the drive takes the checkout's lease: a
// drive that refuses after taking it has opened a queue that --enqueue appends
// to and that nothing will read.
func (s *session) drivePreflight() (reviewer string, err error) {
	if !s.cfg.Git.On() {
		return "", Refusal{fmt.Errorf("[git] enabled is false, so this project has no branches to drive")}
	}
	reviewer = s.reviewRunnerFor(driveReviewer)
	if reviewer == "" {
		return "", Refusal{fmt.Errorf(
			"no reviewing runner: set [run] review_runner or pass --review-runner.\n" +
				"Without one `drover review` prints the prompt and launches nothing, so a drive would " +
				"merge a night's work with no review having run")}
	}
	if _, ok := s.cfg.Runners[reviewer]; !ok {
		return "", Refusal{fmt.Errorf("unknown reviewing runner %q (declared: %s)", reviewer, strings.Join(keys(s.cfg.Runners), ", "))}
	}
	if err := s.preflightBinaries(reviewer); err != nil {
		return "", err
	}
	if err := s.preflightAuth(reviewer); err != nil {
		return "", err
	}

	// Before the queue is read and before a session is paid for. drive merges
	// what it drives, so an absent gate means the operator asked for a merge and
	// declared nothing that could say green. That is a config error, and
	// reporting it per item after a night of launches reports it too late.
	if !driveNoMerge && len(s.cfg.Loop.MergeGate) == 0 {
		return "", Refusal{fmt.Errorf(
			"drive merges what it drives, and no [drive] merge_gate is declared: nothing here could say a branch is green.\n" +
				"Declare the merge gate, or pass --no-merge to drive the queue and leave every item on its branch")}
	}

	// The push is unconditional, so the push target is too: --no-merge holds the
	// merge and still pushes, and a queue that parks every item on a missing
	// remote reports a config error one paid session at a time. gh is only
	// reached where a gate could pass, because that is what opens a pull
	// request.
	if err := gitAt(s.cfg.Root, "remote", "get-url", "origin"); err != nil {
		return "", Refusal{fmt.Errorf(
			"drover pushes what it drives, and this tree has no origin: %s.\n"+
				"The merge gate reads the forge about a commit and the merge is bound to one, so a branch "+
				"nobody pushed leaves both answering about code the forge has not seen.\n"+
				"Add the remote, or drive a tree that has one", err)}
	}
	if len(s.cfg.Loop.MergeGate) > 0 && !gh.Available() {
		return "", Refusal{fmt.Errorf(
			"a green merge gate opens a pull request and gh cannot be reached, so nothing here could open one.\n" +
				"Install gh and run `gh auth login`, or pass --no-merge on a project declaring no " +
				"[drive] merge_gate to drive the queue and leave every branch alone")}
	}

	return reviewer, nil
}

// preflightBinaries refuses a drive whose implementing runner or reviewer names
// a binary that does not resolve. The reviewer is first launched after an
// implementation session has run and been paid for, so its launch finding the
// typo finds it one paid session per item too late. --review-only launches no
// implementer, so that one is not asked.
func (s *session) preflightBinaries(reviewer string) error {
	names := []string{reviewer}
	if !driveReviewOnly {
		impl := driveRunner
		if impl == "" {
			impl = s.cfg.Run.DefaultRunner
		}
		names = append([]string{impl}, names...)
	}
	for _, name := range names {
		spec, ok := s.cfg.Runners[name]
		bin := spec.Binary(runner.Headless)
		if !ok || bin == "" {
			continue
		}
		if err := runner.Resolvable(bin); err != nil {
			return Refusal{fmt.Errorf("runner %s: %w.\nFix [runners.%s] headless, then drive again. Nothing was launched", name, err, name)}
		}
	}
	return nil
}

// authWait bounds one runner's sign-in check. A check that cannot answer in
// that long is not a check the drive should wait on before every launch.
const authWait = 30 * time.Second

// preflightAuth asks each runner this drive launches by default whether it is
// signed in, once, where the runner offers that question, and refuses before
// any session when one says no.
//
// A runner whose OAuth token expires mid-drive parks every item after it one
// paid launch at a time. A token already expired at the start is the same
// failure, found before the first launch. Only the implementing runner the
// drive resolves (--runner, else [run] default_runner) and the reviewer are
// asked: an item naming its own runner reaches it at launch, where an auth
// fault parks it and stops the drive.
//
// A binary that will not start is not refused here. That is a different
// sentence: preflightBinaries says it for one that does not resolve, and the
// launch for one that resolves and still cannot start.
func (s *session) preflightAuth(reviewer string) error {
	impl := driveRunner
	if impl == "" {
		impl = s.cfg.Run.DefaultRunner
	}
	asked := map[string]bool{}
	for _, name := range []string{impl, reviewer} {
		spec, ok := s.cfg.Runners[name]
		argv := runner.AuthCheck(spec)
		if !ok || argv == nil || asked[strings.Join(argv, "\x00")] {
			continue
		}
		asked[strings.Join(argv, "\x00")] = true
		ctx, cancel := context.WithTimeout(context.Background(), authWait)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv[0] is project config, the rest drover's own
		cmd.Dir = s.cfg.Root
		out, err := cmd.CombinedOutput()
		cancel()
		// ProcessState is nil when Start failed: exec.ErrNotFound from a PATH
		// lookup, or an fs.ErrNotExist for an argv[0] naming a path.
		if err != nil && cmd.ProcessState == nil {
			continue
		}
		if err != nil {
			said := ""
			if t := strings.TrimSpace(string(out)); t != "" {
				said = ":\n" + lastLines(t, 3)
			}
			return Refusal{fmt.Errorf("runner %s is not signed in: `%s` failed (%v)%s\n"+
				"Run `%s`, then drive again. Nothing was launched", name, strings.Join(argv, " "), err, said, runner.Login(spec))}
		}
	}
	return nil
}

// drive walks the queue. It returns nil whenever it walked, parked items
// included: a park is a reported outcome rather than a failure of the tool, and
// a wrapper reads the outcomes rather than the exit code.
func (s *session) drive(ctx context.Context, ids []string, reviewer string, deadline time.Time) error {
	if driveDry {
		items, err := s.driveQueue(ids)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(os.Stderr, "queue empty")
			return nil
		}
		s.driveHeader(os.Stdout, items, reviewer, deadline)
		s.preflightPlan(os.Stdout, items)
		return s.drivePlan(items, reviewer, len(ids) > 0)
	}

	// Reconciliation is the one step that writes into the operator's checkout:
	// finish appends to the ledger and saves a report there. A checkout already
	// holding uncommitted work skips it, so those writes never mix with work
	// nobody here owns. The items do not care, since none of them runs there.
	entry, err := s.dirtyPaths()
	if err != nil {
		return err
	}
	if len(entry) > 0 {
		fmt.Fprintf(os.Stderr, "· your checkout holds uncommitted work (%s); not reconciling what merged. "+
			"The queue runs in worktrees of its own\n", pathList(entry))
	} else {
		// Before the queue is read, not after. Reconciling changes which items are
		// done, so a queue built first reports every item it finishes as attempted and
		// skipped, which reads as a drive that tried and failed to work.
		landed, left, err := s.reconcile()
		if landed+left > 0 || err != nil {
			fmt.Fprintf(os.Stderr, "· reconciled: %d finished, %d merged without a drover launch\n", landed, left)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "· reconciliation stopped; not driving\n")
			return err
		}
		if landed > 0 {
			if err := s.catchUpAfter(ctx, ""); err != nil {
				fmt.Fprintf(os.Stderr, "· %v; not driving\n", err)
				return err
			}
		}
	}
	if p, _ := s.cfg.Policy("cleanup_interrupted_drive"); p == polAuto {
		if l, err := s.findLeftovers(); err != nil {
			fmt.Fprintf(os.Stderr, "· cleanup_interrupted_drive: what earlier drives left would not list: %v\n", err)
		} else if err := l.apply(s, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "· cleanup_interrupted_drive: %v\n", err)
		}
	}
	if driveHoldBeforeQueue != nil {
		driveHoldBeforeQueue()
	}
	items, err := s.driveQueue(ids)
	if err != nil {
		return err
	}
	named := len(ids) > 0
	if len(items) == 0 {
		// The lease has been open since before reconciliation, so an --enqueue
		// in that window was told its ids are queued. Taken and closed in one
		// step, as when a queue runs out: an empty drive either runs them or
		// refuses the next append as closed.
		for _, q := range s.takeAppended(true, map[string]bool{}, func(string) bool { return false }, 0) {
			items, named = append(items, q.it), true
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "queue empty")
		return nil
	}
	if err := s.startDrive(items); err != nil {
		return err
	}
	started := DriveEvent{Event: evStarted, Jobs: driveJobs}
	for _, it := range items {
		started.Queue = append(started.Queue, it.ID.Bare())
	}
	if !deadline.IsZero() {
		started.Deadline = deadline.Format(time.RFC3339)
	}
	events.emit(started)
	s.driveHeader(os.Stderr, items, reviewer, deadline)
	s.checkTreeLinks(os.Stderr, items, false)

	pending := make([]queued, 0, len(items))
	for _, it := range items {
		pending = append(pending, newQueued(it, named))
	}
	return s.driveQueued(ctx, pending, reviewer, deadline, 0, s.newFindingWatch(), nil)
}

// finished is one item's account and the slot it ran in, sent back to the
// dispatcher.
type finished struct {
	d    DriveItem
	slot int
}

// driveQueued dispatches the queue into --jobs slots and reports each item as
// it finishes. Everything it prints and encodes happens here, on one goroutine,
// in the order the items finish; at --jobs 1 that is the order they were
// dispatched, and the drive reads as it did before it had slots.
//
// The bounds count dispatches: --max-items is how many items this drive
// starts, and the deadline is checked before each one starts, because an item
// already running is let finish whatever the clock says.
//
// An item --follow adds joins the pending list the moment its dependency lands,
// and is dispatched into the next free slot nothing earlier can take, rather
// than after a wave the list was cut into at the start.
//
// dispatched is how many items count against --max-items already: those a
// drive dispatched before it re-executed itself, zero for any other. watch is
// what that drive knew of the findings its sessions filed, new for any other,
// and waived the usage holds a resume of it released.
func (s *session) driveQueued(ctx context.Context, pending []queued, reviewer string, deadline time.Time, dispatched int, watch *findingWatch, waived []string) error {
	enc := json.NewEncoder(os.Stdout)
	var done []DriveItem
	seen := make(map[string]bool, len(pending))
	for _, q := range pending {
		seen[q.it.ID.Bare()] = true
	}
	sched := newDriveSched()
	sched.usage = newUsageGate(deadline, waived...)
	// seq numbers this process's dispatches, which order the trunk stage. It
	// is not dispatched: an item a re-exec carried over is dispatched again
	// here, and counts against --max-items only once, in the process that
	// started it.
	seq := 0
	s.inputs = s.snapshotInputs(reviewer)
	s.inputs.resumed = driveTakeover != ""
	sched.inputs = s.inputs
	publishConfig(s.inputs)
	if driveJobs > 1 {
		defer sched.watchInterrupt(ctx)()
	}
	results := make(chan finished)
	running := map[int]queued{}
	stopped := false
	// ended is why the drive stopped dispatching, for the exit summary: the
	// first cause that stopped it, drained where nothing did.
	ended := ""
	end := func(why string) {
		if ended == "" {
			ended = why
		}
	}
	var stop error
	var published []string
	var admit admission
	loadHeld := false

	holds := func(id string) bool {
		for _, q := range running {
			if q.it.ID.Bare() == id {
				return true
			}
		}
		return slices.ContainsFunc(pending, func(q queued) bool { return q.it.ID.Bare() == id })
	}

	closed := false
	for {
		s.answerControl(sched)
		if !stopped && errors.Is(sched.draining(), errStopRequested) {
			stopped = true
			end(endedStopped)
		}
		if !stopped && sched.reexecReady(len(running)) {
			s.reexec(sched, pending, running, dispatched, deadline, watch)
		}
		sayReexecWaiting(sched, running, time.Now())
		reviewer = s.inputs.reviewer
		if u, at := sched.usage.holding(); u != nil && !stopped {
			now := time.Now()
			switch {
			case !at.IsZero() && !now.Before(at):
				s.resumeFromUsage(sched)
			case !deadline.IsZero() && !now.Before(deadline):
				// Exit 0: the drive did what its bound allowed, and every item it
				// did not reach is held rather than failed.
				fmt.Fprintln(os.Stderr, "· the deadline arrived while the drive was held on usage; stopping, and each item it holds reports held")
				stopped = true
				end(endedDeadline)
				sched.drain(errUsageDeadline)
				for _, q := range pending {
					d := heldItem(q.it, u)
					done = append(done, d)
					activeDrive.Load().note(d)
					if driveJSON {
						if err := enc.Encode(d); err != nil {
							return err
						}
					}
				}
				pending = nil
			}
		}
		paused := sched.isPaused()
		if paused && !stopped && ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "· interrupted while paused; stopping")
			stopped = true
			end(endedInterrupted)
		}
		if !stopped {
			pending = append(pending, s.takeAppended(false, seen, holds, len(pending))...)
		}
		loadHeld = false
		for !stopped && !paused && len(running) < driveJobs && len(pending) > 0 {
			i, _ := dispatchable(pending, running)
			// An item a re-exec carried over was started under the bounds
			// already, and the process it ran in would have let it finish.
			carried := i >= 0 && pending[i].hold != nil
			if !carried && driveMax > 0 && dispatched >= driveMax {
				fmt.Fprintf(os.Stderr, "· stopping at --max-items %d\n", driveMax)
				stopped = true
				end(endedMaxItems)
				break
			}
			if !carried && !deadline.IsZero() && !time.Now().Before(deadline) {
				fmt.Fprintf(os.Stderr, "· stopping at the deadline\n")
				stopped = true
				end(endedDeadline)
				break
			}
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "· interrupted; stopping at the item boundary")
				stopped = true
				end(endedInterrupted)
				break
			}
			if i < 0 {
				break
			}
			if !carried && admit.hold(s.cfg) {
				loadHeld = true
				break
			}
			// Carried items too: an item a refusal sent back is the first one
			// this hold exists to keep from launching again.
			if s.admitUsage(sched) {
				paused = true
				break
			}
			q := pending[i]
			q.seq = seq
			pending = append(pending[:i:i], pending[i+1:]...)
			slot := 1
			for _, busy := running[slot]; busy; _, busy = running[slot] {
				slot++
			}
			running[slot] = q
			// Forked here rather than in the goroutine: this one reloads s after
			// every item, and a copy taken over there would race that write.
			ds := &driveSlot{sched: sched, seq: seq}
			w, err := s.fork(ds)
			ev := DriveEvent{Event: evDispatched, ID: q.it.ID.Bare()}
			if driveJobs > 1 {
				ev.Slot = slot
			}
			events.emit(ev)
			watch.dispatched(s, q.it.ID.Bare())
			go runSlot(ctx, w, err, q, ds, slot, reviewer, deadline, results)
			seq++
			if !carried {
				dispatched++
			}
		}
		if stopped && !closed {
			closed = true
			closeQueue("the drive is stopping")
		}
		published = publishQueue(published, pending, running, stopped)
		if len(running) == 0 {
			// Taken and closed in one step, so an id appended now is either
			// run by this drive or refused as closed, and never left waiting.
			if stopped {
				break
			}
			// A paused drive with nothing running stays up for its resume,
			// holding its lease and its queue. One held on load has no result
			// to wake it either, so it reads the load again on the same poll
			// that answers a --pause or a --stop.
			if paused || loadHeld {
				wait := enqueuePoll
				if loadHeld && loadPoll < wait {
					wait = loadPoll
				}
				sleepFor(ctx, wait)
				continue
			}
			if more := s.takeAppended(true, seen, holds, 0); len(more) > 0 {
				pending = append(pending, more...)
				continue
			}
			break
		}
		// Woken on the poll even with every slot full, so a --pause or a
		// --resume is answered while the items run.
		var r finished
		if stopped {
			r = <-results
		} else {
			select {
			case r = <-results:
			case <-time.After(enqueuePoll):
				continue
			}
		}
		from := running[r.slot]
		delete(running, r.slot)
		d := r.d
		if driveJobs > 1 {
			d.Slot = r.slot
		}
		// Each item ran on a session of its own, so what it recorded reaches this
		// one only through the files.
		if err := s.reload(); err != nil {
			fmt.Fprintf(os.Stderr, "· the backlog and ledger would not reload after %s: %v\n", d.ID, err)
		}
		if d.requeue != nil && !stopped {
			back := newQueued(from.it, true)
			if it, err := s.item(d.ID); err == nil {
				back = newQueued(it, true)
			}
			back.hold = d.requeue
			pending = append([]queued{back}, pending...)
			fmt.Fprintf(os.Stderr, "· %s: %s; it goes back to the head of the queue at its %s stage\n", d.ID, d.Reason, d.requeue.At)
			events.emit(DriveEvent{Event: evQueued, ID: d.ID, Action: "usage_admission", Stage: d.requeue.At, Position: 1})
			s.holdForUsage(sched, *d.refusal)
			continue
		}
		events.itemEvents(d)
		watch.finished(s, &d, running, queuedIDs(pending, running))
		done = append(done, d)
		activeDrive.Load().note(d)
		if driveFollow && d.Finished && stop == nil {
			for _, next := range s.unblockedBy(d.ID, seen) {
				fmt.Fprintf(os.Stderr, "· --follow: %s finished, so %s is ready; appended\n", d.ID, next.ID.Bare())
				pending = append(pending, newQueued(next, false))
			}
		}
		if driveJSON {
			if err := enc.Encode(d); err != nil {
				return err
			}
		}
		if d.stop != nil && stop == nil {
			fmt.Fprintf(os.Stderr, "· %v; stopping the drive\n", d.stop)
			stop, stopped = d.stop, true
			end(endedFault)
			sched.drain(d.stop)
		}
	}
	// A drive stopping on a fault stops on a cause the re-check would walk
	// into, and one the operator stopped or interrupted asked for no merges.
	if stop == nil && sched.draining() == nil && ctx.Err() == nil {
		if err := s.recheckInfra(ctx, done, deadline, enc); err != nil {
			stop = err
			end(endedFault)
		}
	}
	if ended == "" && ctx.Err() != nil {
		end(endedInterrupted)
	}
	end(endedDrained)
	totals := driveTotals(watch.all)
	events.setTotals(totals)
	events.setSummary(s.exitSummary(done, ended))
	if driveJSON {
		if err := enc.Encode(totals); err != nil {
			return err
		}
	}
	s.driveSummary(done, totals)
	return stop
}

// queuedIDs is every id the drive holds waiting or running.
func queuedIDs(pending []queued, running map[int]queued) map[string]bool {
	out := make(map[string]bool, len(pending)+len(running))
	for _, q := range pending {
		out[q.it.ID.Bare()] = true
	}
	for _, q := range running {
		out[q.it.ID.Bare()] = true
	}
	return out
}

// backlogIDs is every item id the backlog holds now.
func (s *session) backlogIDs() map[string]bool {
	out := make(map[string]bool, len(s.backlog.Items))
	for _, it := range s.backlog.Items {
		out[it.ID.Bare()] = true
	}
	return out
}

// publishQueue rewrites the drive's lease with what it holds waiting and what
// it is running, when either changed since last, and re-renders [status]
// render. A stopped drive dispatches nothing more, so it publishes an empty
// queue rather than items nobody will start.
//
// A lease that will not rewrite costs the view and never the drive: the
// account a status reads goes stale, and the queue itself is in memory.
func publishQueue(last []string, pending []queued, running map[int]queued, stopped bool) []string {
	if held == nil {
		return last
	}
	var queue, now []string
	if !stopped {
		for _, q := range pending {
			queue = append(queue, q.it.ID.Bare())
		}
	}
	slots := slices.Sorted(maps.Keys(running))
	for _, n := range slots {
		now = append(now, running[n].it.ID.Bare())
	}
	key := append(append(slices.Clone(queue), "|"), now...)
	if slices.Equal(key, last) {
		return last
	}
	_ = held.Update(func(l *lease.Lease) { l.Queue, l.Running, l.Jobs = queue, now, driveJobs })
	appended()
	return key
}

// runSlot runs one item on the session forked for it and hands back what it
// amounted to. It retires the item's turn at the trunk stage whatever happened,
// and drains the schedule on a stop before it does, so no later item enters the
// stage between this one stopping and the dispatcher hearing of it.
func runSlot(ctx context.Context, w *session, err error, q queued, slot *driveSlot, n int, reviewer string,
	deadline time.Time, results chan<- finished,
) {
	var d DriveItem
	if err != nil {
		now := time.Now().Format(time.RFC3339)
		d = DriveItem{ID: q.it.ID.Bare(), Title: q.it.Title(), Outcome: outcomeParked, Park: "branch_failed",
			Reason: "the backlog and ledger would not load for this item: " + err.Error(), Started: now, Ended: now,
			Detail: detailUnknown, Evidence: "no session ran: " + err.Error()}
	} else {
		w.resumeAt = q.hold
		d = w.driveItem(ctx, q.it, reviewer, q.named, deadline)
	}
	if d.stop != nil {
		slot.sched.drain(d.stop)
	}
	slot.sched.retire(slot.seq)
	results <- finished{d: d, slot: n}
}

// drivable reports whether a drive would attempt an item in this state.
//
// A park leaves a clean log behind it, so the item reads as `ran` and the queue
// skips it from then on. Naming the id is what resumes it, which is the contract
// drive's help and driveQueue's filter both state, and nothing anywhere clears a
// run log: without that arm the named path can never launch. Every other state
// skips whatever the operator typed, since deps that have not merged and an item
// already merged are not opinions a name overrides.
//
// driveItem and drivePlan both ask, and one answer serves both because two did
// not: the gate lived in driveItem alone, --dry-run resolved every step without
// it, and the preview promised a launch the run then refused with one word.
func drivable(state string, named bool) bool {
	return state == stateReady || (named && state == stateRan)
}

// driveQueue is the items to attempt: those named, in the order given, or every
// ready one in backlog order. A named id is not filtered by state, because
// naming it is how an operator resumes something the queue would skip.
func (s *session) driveQueue(ids []string) ([]backlog.Item, error) {
	if len(ids) == 0 {
		return s.queue(), nil
	}
	out := make([]backlog.Item, 0, len(ids))
	for _, id := range ids {
		it, err := s.item(id)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// driveItem is one item: branch, run, then review until the ledger says to
// stop.
//
// named is whether the operator asked for this id rather than the queue
// offering it, which is the one fact the state gate below cannot read off the
// item.
//
// Ended is stamped on the named result rather than on d: every return copies d
// first, so a deferred write to d reaches no caller.
func (s *session) driveItem(ctx context.Context, it backlog.Item, reviewer string, named bool, deadline time.Time) (res DriveItem) {
	if driveDispatching != nil {
		driveDispatching(it.ID.Bare())
	}
	s.noticeConfig()
	d := DriveItem{ID: it.ID.Bare(), Title: it.Title(), Started: time.Now().Format(time.RFC3339)}
	// The last launch and what Run returned with it, which is where a park
	// that names no detail of its own reads one from.
	var last *runner.Outcome
	var lastErr error
	defer func() {
		res.Ended = time.Now().Format(time.RFC3339)
		if res.Outcome == outcomeParked {
			s.recordPark(&res, last, lastErr)
			s.announcePark(res)
		}
	}()

	park := func(cause, why string) DriveItem {
		d.Outcome, d.Park, d.Reason = outcomeParked, cause, why
		// A deadline that arrived behind a usage hold stopped nothing the item
		// did: the account had nothing left to give it.
		if cause == parkDrained && s.slot != nil && errors.Is(s.slot.sched.draining(), errUsageDeadline) {
			d.Outcome, d.Park = outcomeHeld, ""
		}
		s.leaveItem(&d)
		return d
	}
	skip := func(cause string) DriveItem {
		d.Outcome, d.Park = outcomeSkipped, cause
		return d
	}
	// A fault parks this item and ends the drive, for the reason runGate
	// already gives about a gate that will not run: a service that is down and
	// an account with nothing left in it are both true of the next item too, and
	// walking on spends a branch, a push and two dead sessions per item to
	// rediscover it. launchOrWait has already waited wherever waiting could
	// have helped, so reaching here means it could not.
	faulted := func(who, stage string, oc runner.Outcome) DriveItem {
		// A refusal the reading names is the account and not the runner: the
		// item goes back to the head of the queue at the stage that was
		// refused, and the drive holds until the window resets. The launch
		// counts against no rerun and no round, since nothing ran.
		if s.holdsRefusals() && quotaRefused(oc.Result) {
			u := refusalStep(oc.Result.RateLimit, time.Now())
			u.Reason = fmt.Sprintf("%s's %s session was refused: %s", who, stage, u.Reason)
			d.Outcome, d.Reason = outcomeHeld, u.Reason
			d.requeue, d.refusal = s.requeueAt(it, stage), &u
			return d
		}
		pd := sessionDetail(oc, nil)
		d.setDetail(pd)
		if pd.Detail == detailAuth {
			d.login = runner.Login(s.cfg.Runners[who])
		}
		out := park("runner_fault", faultReason(who, oc.Result, oc.LogPath))
		// Not overwritten where the park already ended the drive for a reason of
		// its own: a fault that left work in the tree stops on the tree, because
		// that is the condition the operator has to clear before any later item
		// can run.
		if out.stop == nil {
			out.stop = errRunnerFault
		}
		return out
	}

	// An item a re-exec carried over was mid-flight, so its state reads ran or
	// reviewed and the gate below would skip it. It also passed stale_citations when
	// it was dispatched, and a sibling's merge since then is no reason to hold
	// it halfway.
	state, missing := s.workState(it, s.cfg.LogDir())
	if s.resumeAt == nil && !drivable(state, named) {
		if state == stateBlocked {
			d.Reason = "blocked by " + strings.Join(missing, " ")
			d.blockedBy = missing
		}
		return skip(state)
	}
	if s.resumeAt == nil {
		if why := s.checkRefDrift(it); why != "" {
			return park(parkRefDrift, why)
		}
		// Before any session, because the pass after it would read the edit as
		// a reviewer's findings written by hand and park body_edited only once
		// a whole implement and review had been spent.
		if what, fix := s.handEdit(it); what != "" {
			return park(parkHandEdited, what+"; "+fix)
		}
	}

	data, err := s.promptData(it)
	if err != nil {
		return park("branch_failed", err.Error())
	}
	d.Branch = data.Branch
	implementer := s.runnerFor(it, driveRunner)
	d.Implementer, d.Reviewer = implementer, reviewer
	// Warned, never refused: two sessions of one runner can be two reviewers,
	// and only the operator knows. audit's same-runner warning draws the same line.
	if implementer == reviewer {
		d.SameRunner = true
		fmt.Fprintf(os.Stderr, "  ! %s implements and reviews with one runner (%s). Driving anyway.\n",
			d.ID, implementer)
	}
	// The item's own checkout, never the operator's. Nothing here checks the
	// operator's tree for uncommitted work, because nothing here touches it: a
	// switch in a shared checkout carried whatever the last item left onto the
	// next one, and the dirty checks that grew around that switch stopped the
	// whole queue over a file one item's branch ignored and the trunk did not.
	tree, err := s.openItemTree(it, data.Branch, s.trunk())
	defer tree.close()
	if err != nil {
		if tree != nil {
			d.Tree = tree.Path
		}
		return park(parkWorktree, err.Error())
	}
	s.tree, d.Tree = tree.Path, tree.Path
	defer func() { s.tree = "" }()

	base := runlog.Base(it.ID.Stem(), it.Slug)
	// reviewOnly skips the implement stage of the next round: --review-only
	// on the first, and the one rerun a lost review gets. rerun is whether
	// this item has spent that rerun, so a second loss parks.
	reviewOnly, rerun := reviewOnlyID != "" && reviewOnlyID == d.ID, false
	// adopt is a stage boundary a paused drive holds the item at. A resume
	// that reloaded the config can name other runners, which the stages after
	// it launch.
	adopt := func(next string) []string {
		s.noticeConfig()
		changed := s.heldAt(ctx, it, next)
		if len(changed) > 0 {
			implementer, reviewer = s.runnerFor(it, driveRunner), s.inputs.reviewer
			d.Implementer, d.Reviewer = implementer, reviewer
		}
		return changed
	}
	// regated is a resume that changed the item's verify command after a
	// stage held to the old one: the new one runs on the tree, and a refusal
	// sends the item back to implement with the output as its findings.
	regated := func(changed []string) (out DriveItem, again, stop bool) {
		if !slices.Contains(changed, inputVerify) {
			return DriveItem{}, false, false
		}
		if out, again, stop = s.reverify(ctx, it, &d, park); stop || !again {
			return out, again, stop
		}
		if err := s.reload(); err != nil {
			return park("branch_failed", err.Error()), false, true
		}
		if it, err = s.item(d.ID); err != nil {
			return park("branch_failed", err.Error()), false, true
		}
		return DriveItem{}, true, false
	}
	// The --review-only round catches up before its review, as an implement
	// round does before its session. A lost review's rerun and a resume held at
	// the review do not: each is about the head it already had.
	catchUpReview := reviewOnly
	defer func() { s.conflict = nil }()
	review, regate, land := s.resumeHeld(it)
	reviewOnly = reviewOnly || review
	if regate {
		if out, again, stop := regated([]string{inputVerify}); stop {
			return out
		} else if again {
			reviewOnly, land = false, false
		}
	}
	if land {
		return s.inTrunkStage(it, func() DriveItem { return s.driveFinish(ctx, it, &d, park, deadline) }, park)
	}
	for {
		if !reviewOnly {
			adopt(ledger.StageImplement)
		}
		s.conflict = nil
		if !reviewOnly || catchUpReview {
			if out, stop := s.dispatchCatchUp(&d, park); stop {
				return out
			}
			// A conflict is a session's to resolve, and a review has nothing to
			// read until one has: the round becomes an implement round.
			if s.conflict != nil {
				reviewOnly = false
			}
		}
		catchUpReview = false
		banner := launchInfo{
			ID: d.ID, Branch: d.Branch, Round: d.Rounds + 1,
			Item: rel(s.cfg.Root, it.Path),
		}
		if reviewOnly {
			if out, stop := s.reviewHead(&d, park); stop {
				return out
			}
		} else if out, stop := s.implementRound(ctx, it, &d, implementer, base, banner, deadline, park, faulted, &last, &lastErr); stop {
			return out
		}
		reviewOnly = false
		if it, err = s.item(d.ID); err != nil {
			return park("branch_failed", err.Error())
		}

		changed := adopt(ledger.StageReview)
		if why := s.drained("its review; the implement run's commits are pushed"); why != "" {
			return park(parkDrained, why)
		}
		if out, again, stop := regated(changed); stop {
			return out
		} else if again {
			continue
		}
		before := s.ledger.Counts(d.ID)
		brief, err := s.renderReview(it)
		if err != nil {
			return park("branch_failed", err.Error())
		}
		banner.Stage, banner.Prompt = ledger.StageReview, s.promptPath(s.cfg.Prompts.Review, "review")
		banner.Item = rel(s.cfg.Root, it.Path)
		oc, rerr := s.launchOrWait(ctx, it, ledger.StageReview, reviewer, runlog.ReviewBase(it.ID.Stem(), it.Slug), brief, banner, deadline)
		last, lastErr = &oc, rerr
		if err := s.reload(); err != nil {
			return park("branch_failed", err.Error())
		}
		it, _ = s.item(d.ID)
		after := s.ledger.Counts(d.ID)

		d.Reviews++
		d.ChangesRequested += after.ChangesRequested - before.ChangesRequested
		d.Audits += after.Audits - before.Audits

		rp := pass{before: before, after: after, res: oc.Result,
			logged: oc.Logged, events: oc.Events, known: oc.Known, cap: s.cfg.Review.MaxReviewRounds,
			refused: after.Refusals > before.Refusals, leftRunning: oc.LeftRunning}
		rp.readBody(it.Path)
		v := passResult(rp)
		// Recorded before the branch below acts on it, for the reason
		// session.launched gives: a process killed inside driveFinish still had a
		// result, and a row written after the merge would be missing from
		// exactly the passes worth reading back. A ledger that refuses the row
		// parks the item rather than merging on a result nothing can read
		// again, which is the same fail-closed reading passResult's own order
		// is built on.
		if err := s.record(ledger.Row{ID: d.ID, Pass: v, Date: ledger.Today()}); err != nil {
			return park("ledger_unwritable", "the review read "+v+" and the ledger refused the row: "+err.Error())
		}
		// rerun_lost_review. A pass that never reached a result about the work
		// is rerun once on the head it was given, the implement stage skipped:
		// the branch is the one the lost pass was asked about, and a second
		// implement run would hand the rerun a different one. Once, because a
		// reviewer that dies twice on the same head is telling the operator
		// something a third launch will not.
		if pd := sessionDetail(oc, rerr); lostReview(v, pd.Detail) && !rerun && ctx.Err() == nil {
			if p, _ := s.cfg.Policy("rerun_lost_review"); p == polAuto {
				rerun, reviewOnly = true, true
				fmt.Fprintf(os.Stderr, "  · %s: the review was lost (%s, %s: %s); rerunning it once on the same head\n",
					d.ID, v, pd.Detail, firstLine(pd.Evidence))
				continue
			}
		}
		switch v {
		case passPassed:
			if out, again, stop := regated(adopt(stageCatchUp)); stop {
				return out
			} else if again {
				continue
			}
			return s.inTrunkStage(it, func() DriveItem { return s.driveFinish(ctx, it, &d, park, deadline) }, park)
		case passChangesRequested:
			d.Rounds++
			if ctx.Err() != nil {
				return park("interrupted", "interrupted between rounds")
			}
			continue
		case passFaulted:
			return faulted(reviewer, ledger.StageReview, oc)
		case passStanding:
			return park(v, passSentence(v, ledger.StageReview, rp, "the review", reviewer, oc.LogPath)+
				". "+standingNext(d.ID, s.ledger.State(d.ID).Note))
		default:
			return park(v, passSentence(v, ledger.StageReview, rp, "the review", reviewer, oc.LogPath))
		}
	}
}

// reviewHead is the round --review-only and a lost review's rerun start at:
// the branch as it stands, with no implement run before it. It parks where
// the implement stage would have, on work left uncommitted and on a branch
// holding nothing to review.
func (s *session) reviewHead(d *DriveItem, park func(cause, why string) DriveItem) (DriveItem, bool) {
	dirty, err := s.dirtyPaths()
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	if len(dirty) > 0 {
		s.stayDirty(d, dirty)
		return *d, true
	}
	n, err := commitsPast(s.cfg.Root, s.trunk(), d.Branch)
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	if n == 0 {
		return park(parkNoCommits, d.Branch+" holds no commit past the trunk, so there is no diff to review"), true
	}
	return DriveItem{}, false
}

// implementRound is one implement run and everything drive reads off it before
// a reviewer is paid for: the fault, the result, an empty or dirty branch, and
// the push. stop is true when the item parked, with its account in out.
//
// last and lastErr are driveItem's, written here so a park anywhere after this
// launch reads its detail from it.
func (s *session) implementRound(ctx context.Context, it backlog.Item, d *DriveItem, implementer, base string,
	banner launchInfo, deadline time.Time, park func(cause, why string) DriveItem,
	faulted func(string, string, runner.Outcome) DriveItem, last **runner.Outcome, lastErr *error,
) (out DriveItem, stop bool) {
	// A stage boundary. A drive that is stopping lets the stage in flight
	// finish and launches nothing after it.
	if why := s.drained("its next implement run"); why != "" {
		if s.conflict != nil {
			_ = gitAt(s.workDir(), "merge", "--abort")
		}
		return park(parkDrained, why), true
	}
	body, err := s.renderPrompt(it)
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	banner.Stage, banner.Prompt = ledger.StageImplement, s.promptPath(s.cfg.Prompts.Implement, "default")
	ic, err := s.launchOrWait(ctx, it, ledger.StageImplement, implementer, base, body, banner, deadline)
	*last, *lastErr = &ic, err
	// Before the push and before the review. A run that never reached the
	// work has nothing to push and nothing to review, and pushing anyway
	// asks the forge about an empty branch while the review reads a diff
	// that does not exist.
	if ic.Result.Faulted() {
		return faulted(implementer, ledger.StageImplement, ic), true
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "  · %s: run exited %d (%v); asking the ledger anyway\n", d.ID, ic.Exit, err)
	}
	// A faulted run never reaches this, deliberately: the report file is
	// the item's saved self-report, and "API Error: 529 Overloaded" saved
	// under that heading is read by every later run and review.
	s.captureReport(it)

	// Read here and acted on below, because the two cases want opposite
	// things from the push. A run that died is pushed first so the commits it
	// did make survive, which is what the push-before-park ordering exists
	// for. A run that finished cleanly and committed nothing has nothing to
	// preserve, so it is parked before the push rather than after: an empty
	// branch on the remote is one `git branch --merged` lists as merged
	// because it never moved.
	rc := pass{res: ic.Result, logged: ic.Logged, events: ic.Events, known: ic.Known, leftRunning: ic.LeftRunning}
	ran := runResult(rc)
	// Read before the push and acted on after it, alongside the result and
	// for the same reason: a session can end with its work still in the tree,
	// and the push and the review would each read that tree rather than the
	// branch, and a review can then report work "settled in the working tree,
	// still absent from the PR head".
	dirty, err := s.dirtyPaths()
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	// A merge the session never concluded can leave nothing for status to
	// report, and the push would then publish the branch without the trunk.
	if s.conflict != nil && gitAt(s.workDir(), "rev-parse", "--quiet", "--verify", "MERGE_HEAD") == nil {
		dirty = append(dirty, "MERGE_HEAD (the trunk merge, unconcluded)")
	}
	// A dirty tree is not an empty run: the work exists and wants committing,
	// which is a different sentence and a different recovery.
	if ran == "" && len(dirty) == 0 {
		if n, err := commitsPast(s.cfg.Root, s.trunk(), d.Branch); err != nil {
			return park("branch_failed", err.Error()), true
		} else if n == 0 {
			return park(parkNoCommits, implementer+" finished and left no commit on "+d.Branch+
				": there is no diff to review, and a branch that never moved reads as merged"), true
		}
	}
	// Before the push, not after: the push can fail and the branch still holds
	// the commits, and it is the branch holding them that a later finish has to
	// be able to tell from one that never moved.
	if err := s.recordHead(d.ID, d.Branch); err != nil {
		return park("ledger_unwritable", "the branch holds commits and the ledger would not record that: "+err.Error()), true
	}
	if err := s.pushBranch(d.Branch); err != nil {
		d.setDetail(pushDetail(s.cfg.Root, err))
		return park("push_failed", "the branch would not push, so nothing the forge says about it is about this code: "+err.Error()), true
	}
	if err := s.reload(); err != nil {
		return park("branch_failed", err.Error()), true
	}

	// Acted on after the push and before the reviewer, which is the whole
	// point of reading it at all: what a dead run must not do is buy a review
	// pass, because the pass that follows reads the implementer's silence as
	// the reviewer's.
	switch ran {
	case passUnreadable:
		return park(parkImplementUnreadable,
			passSentence(passUnreadable, ledger.StageImplement, rc, "the implement run", implementer, ic.LogPath)), true
	case passDied:
		return park(parkImplementDied,
			passSentence(passDied, ledger.StageImplement, rc, "the implement run", implementer, ic.LogPath)), true
	}
	// After the push, so the commits the run did make reach the remote, and
	// before the reviewer, who would otherwise judge a tree the head does not
	// carry. A run that died with work in the tree never arrives here: park
	// above already stayed put and said so.
	if len(dirty) > 0 {
		s.stayDirty(d, dirty)
		return *d, true
	}

	return DriveItem{}, false
}

// passSentence is what one token means, and the only place it is worded. drive
// parks on it, review reports it, and passFollowUp carries it as the outcome,
// so an operator who learned what "no_record" means from a park reads the same
// sentence from a review they ran by hand.
//
// subject is which session the sentence is about. Both stages reach passDied and
// passUnreadable, and a park that said "the review" about an implement run would
// send the operator to the wrong log. runnerName is empty where no launch is in
// hand, and the sentence then names no runner rather than a blank one.
//
// passPassed and passChangesRequested are here although drive never parks on either: a
// review run by hand reports every outcome, and a token whose sentence lived
// only in a park would print as a bare word on the two that matter most.
func passSentence(v, stage string, p pass, subject, runnerName, logPath string) string {
	where := ""
	if runnerName != "" {
		where = " (" + whereItRan(runnerName, logPath) + ")"
	}
	switch v {
	case passPassed:
		return subject + " recorded an audit and sent nothing back"
	case passChangesRequested:
		return subject + " sent work back; the findings are in the item body, under its review heading"
	case passDied:
		// Two states reach this token, and the wording has to cover both.
		// runResult returns passDied for a log with no terminal event and for a
		// log whose terminal event reports a failure, and the second is the only
		// state in which Denials can be non-empty, so a denial clause has to say
		// the session ended rather than that it left no result.
		if len(p.leftRunning) > 0 && p.logged && p.res.Succeeded() {
			return subject + " exited while a process it started was still running (" +
				strings.Join(p.leftRunning, "; ") + "), so what it reported is about work that had not finished; " +
				"drover killed the process" + where
		}
		if p.logged {
			return withCause(subject+" ended on "+blankSubtype(p.res.Subtype)+where, p.res, stage)
		}
		return withCause(subject+" ended without finishing: it left no terminal event in its log"+where, p.res, stage)
	case passFaulted:
		// The one renderer, not a second wording of it. A token has to
		// read the same whichever command the operator ran, and a fault is the
		// token where the difference costs most: the kind, the runner's own
		// words and the retry instant are the whole of what a person acts on.
		// Subject rather than runner name where no runner is known, so the
		// sentence still names who stopped.
		who := runnerName
		if who == "" {
			who = subject
		}
		return faultReason(who, p.res, logPath)
	case passUnreadable:
		return fmt.Sprintf(
			"%s wrote %d events and drover recognized a terminal result in none of them, so a session that finished reads as one that died; the runner's output is not a schema drover reads%s",
			subject, p.events, where)
	case passSilent:
		return withCause(subject+" recorded nothing: no audit and no request for changes, so what it found is unknown", p.res, stage)
	case passChangesUnrecorded:
		return subject + " recorded an audit and no request for changes, and no body digest exists to read it against, so a clean pass and a body edited by hand read the same"
	case passEdited:
		return fmt.Sprintf("the item body changed outside drover after the review launched (%s then, %s now), "+
			"so findings written into it by hand cannot be told from a clean pass; a block deleted by hand reads the same, "+
			"and drover close-changes <id> is how a closed block is discharged without moving the digest",
			blank(p.after.LastBody), blank(p.body))
	case passStanding:
		return subject + " recorded an audit and no request for changes, and the block an earlier round sent back still stands " +
			"under " + prompts.ChangesHeading + ": drover request-changes <id> if its findings are still open, " +
			"drover close-changes <id> if they are closed"
	case passCapped:
		return fmt.Sprintf("%s wanted to send work back, and the item is at [review] max_review_rounds (%d)", subject, p.cap)
	case passRepeating:
		return subject + " sent back the same evidence as the request for changes before it, so the last run changed nothing it checks"
	}
	return subject + " ended as " + v
}

// standingNext is the follow-up a standing park names for one item: what to
// run, that this drive did not merge it, and what that costs the queue.
//
// The audit note picks the command because it is the only account of the
// block the review left. A note naming a finding as open or unmet sends the
// item back; one naming it closed sends it to open-pr. One that says neither
// names both rather than guessing: a guess toward open-pr merges an open
// finding, and a guess toward request-changes spends a round on a closed one.
func standingNext(id, note string) string {
	discharge := "drover close-changes " + id + " && drover open-pr " + id
	sentBack := "drover request-changes " + id + " -f <file>"
	var next string
	switch noteReads(note) {
	case "open":
		next = "The audit note reads as open, so for " + id + ": " + sentBack
	case "closed":
		next = "The audit note reads as closed, so for " + id + ": " + discharge
	default:
		next = "The audit note says neither, so for " + id + ": " + discharge + " if every finding is closed, " +
			sentBack + " if one is open"
	}
	return next + ". This drive did not merge " + id + ", and every later item that depends on it is blocked until it finishes"
}

// noteReads classifies an audit note as "open", "closed" or "" for neither.
// Open wins over closed, because "R1 met, R4 unmet" is an open block.
//
// "closed" is the only reading that sends an item to open-pr, so it is the
// one reading that is never a default. A note reads as closed only when every
// clause in it asserts the whole block closed: a closed word after "all",
// "every" or "each", with no negation and no word wholeBlockClosed does not
// recognize. That path splits a clause on whitespace alone, so no digit, symbol
// or non-ASCII letter is dropped before the allow-list sees it: "3/4", "50%"
// and "r1-r3" are each one word outside blockFiller. The open and negation
// checks keep the looser split, where a dropped character costs one round
// rather than a merge. A clause naming one
// finding closed ("R2 fixed in 1a2b3c") cannot speak for the block, since the
// classifier never sees what else the block holds, and a clause saying
// anything else may be an open finding in wording no list anticipated
// ("R3 needs work"). Either one makes the note neither, which names both
// commands. A closed word under "not", "n't", "never" or "no", or with an
// "un" prefix, reads as open: "R4 not closed" contains "closed".
func noteReads(note string) string {
	n := strings.ToLower(note)
	for _, w := range []string{"unmet", "not met", "still", "open", "fail", "broken", "regress"} {
		if strings.Contains(n, w) {
			return "open"
		}
	}
	closedWord := func(w string) bool {
		return w == "closed" || w == "fixed" || w == "resolved" || w == "addressed" || w == "met"
	}
	words := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool {
			return (r < 'a' || r > 'z') && r != '\'' && r != '\u2019'
		})
	}
	negated, closed := false, false
	for _, w := range words(n) {
		switch {
		case w == "not" || w == "no" || w == "never" || w == "nor" || w == "cannot" ||
			strings.HasSuffix(w, "n't") || strings.HasSuffix(w, "n\u2019t"):
			negated = true
		case strings.HasPrefix(w, "un") && closedWord(w[2:]):
			return "open"
		case closedWord(w):
			closed = true
		}
	}
	if closed && negated {
		return "open"
	}
	clauses := strings.FieldsFunc(n, func(r rune) bool { return strings.ContainsRune(".,;:!?\n", r) })
	if !closed || len(clauses) == 0 {
		return ""
	}
	for _, c := range clauses {
		if !wholeBlockClosed(strings.Fields(c), closedWord) {
			return ""
		}
	}
	return "closed"
}

// wholeBlockClosed reports whether one clause asserts every finding closed: a
// quantifier ahead of a closed word, and no word outside blockFiller.
//
// An allow-list, not a list of exceptions, because "and", "while" and every
// other way of joining an open finding to a closed claim carry no punctuation
// to split on, and each deny-list let through the wording it did not name.
// A word this does not know costs the operator one command; a wrong "closed"
// opens the PR over an open finding.
func wholeBlockClosed(words []string, closedWord func(string) bool) bool {
	quantified, asserted := false, false
	for _, w := range words {
		switch {
		case w == "all" || w == "every" || w == "each":
			quantified = true
		case closedWord(w):
			asserted = asserted || quantified
		case !blockFiller[w]:
			return false
		}
	}
	return asserted
}

// blockFiller is the words a whole-block assertion may carry besides its
// quantifier and closed word, none of which can name a finding. A requirement
// id, a numeral or a percentage is never on it, so "all of R1-R3 fixed" and
// "all 3 of 4 findings fixed" speak for some findings, not the block.
var blockFiller = map[string]bool{
	"finding": true, "findings": true, "requirement": true, "requirements": true,
	"the": true, "in": true, "of": true, "this": true, "block": true,
	"are": true, "is": true, "were": true, "now": true,
}

// withCause names what the runner refused the session, which is why a review
// that recorded nothing recorded nothing.
//
// Appended only to a token that already read badly. A denial is not itself an
// outcome: an implementation run that loses one command to a prompt and then
// finishes is ordinary, and a sentence reporting it would fire on most runs
// this project makes. Silence with the cause beside it was the whole of the
// operator's question, and answering it took the ledger by hand and then the
// log by hand.
func withCause(sentence string, res runlog.Result, stage string) string {
	d := res.Denials()
	if d == "" {
		return sentence
	}
	return sentence + "; " + d + ", so " + couldNot(stage)
}

// couldNot is what a denied session could not do, by the stage it was. An
// implement run calls neither `drover audit` nor `drover request-changes`, so naming them
// in its park sent an operator looking for a row a run of that stage never
// writes. Keyed on the ledger's own stage vocabulary rather than on the subject
// prose beside it, which is a sentence somebody will reword.
func couldNot(stage string) string {
	if stage == ledger.StageImplement {
		return "it could not finish the work"
	}
	return "it could not reach `drover audit` or `drover request-changes`"
}

// blankSubtype names what a terminal event reported, for a runner that reported
// a failure without one.
func blankSubtype(subtype string) string {
	if strings.TrimSpace(subtype) == "" {
		return "a failure it did not name"
	}
	return subtype
}

// whereItRan names the runner and its log, for a park an operator reads at nine
// in the morning. The runner because [runners] is where the cause lives, and the
// path because the events are the evidence; without both, a park on a launch
// that failed before it wrote anything is indistinguishable from one on a
// reviewer that ran and was misread.
func whereItRan(name, logPath string) string {
	if logPath == "" {
		return fmt.Sprintf("runner %s, no log written", name)
	}
	return fmt.Sprintf("runner %s, log %s", name, logPath)
}

// renderPrompt and renderReview assemble what run and review hand a runner,
// from the same templates and the same data those commands use. A drive that
// rendered its own would be a second copy of the doctrine, which is the failure
// doctrine_test.go exists to report.
func (s *session) renderPrompt(it backlog.Item) (string, error) {
	data, err := s.briefData(it)
	if err != nil {
		return "", err
	}
	name, text, err := prompts.Load(s.cfg.Abs(s.cfg.Prompts.Implement), "default", prompts.DefaultTemplate())
	if err != nil {
		return "", err
	}
	return prompts.Render(name, text, data)
}

func (s *session) renderReview(it backlog.Item) (string, error) {
	data, err := s.reviewData(it)
	if err != nil {
		return "", err
	}
	name, text, err := prompts.Load(s.cfg.Abs(s.cfg.Prompts.Review), "review", prompts.ReviewTemplate())
	if err != nil {
		return "", err
	}
	out, err := prompts.Render(name, text, data)
	if err != nil {
		return "", err
	}
	return out + reviewExits(it), nil
}

// driveHeader is the one read an operator gets before a queue takes the
// terminal for a night: what it will walk, who implements and who reviews,
// which templates assemble the two prompts, and what stops it.
//
// The templates are here rather than only in the per-launch preamble because a
// wrong one is wrong for the whole drive, and the answer an operator needs is
// available before the first launch rather than after it.
func (s *session) driveHeader(w io.Writer, items []backlog.Item, reviewer string, deadline time.Time) {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID.Bare())
	}
	fmt.Fprintf(w, "==> drive · %d %s: %s\n", len(items), plural(len(items), "item", "items"), strings.Join(ids, " "))

	implementer := driveRunner
	if implementer == "" {
		implementer = "per item (default " + s.cfg.Run.DefaultRunner + ")"
	}
	fmt.Fprintf(w, "==> runners: implement %s · review %s\n", implementer, reviewer)
	fmt.Fprintf(w, "==> prompts: implement %s · review %s\n",
		s.promptPath(s.cfg.Prompts.Implement, "default"), s.promptPath(s.cfg.Prompts.Review, "review"))

	bound := "unbounded"
	if !deadline.IsZero() {
		bound = "until " + deadline.Format(time.RFC3339)
	}
	if driveMax > 0 {
		bound += fmt.Sprintf(", at most %d %s", driveMax, plural(driveMax, "item", "items"))
	}
	if driveFollow {
		bound += ", following what each finish unblocks"
	}
	merge := "merge: [drive] merge_gate decides"
	if driveNoMerge {
		merge = "--no-merge: every item stops green-and-reviewed"
	}
	fmt.Fprintf(w, "==> bounds: %s · %s\n", bound, merge)
	if driveJobs > 1 {
		fmt.Fprintf(w, "==> jobs: implement and review up to %d items at once, in worktrees of their own · "+
			"branch update, verify command, merge gate, push, pull request and merge one item at a time, in dispatch order · "+
			"items citing one path never run at once\n", driveJobs)
	}
	// The preview has to name the writes, not only the launches. A push and a
	// pull request are what an operator reading --dry-run is deciding about, and
	// a preview that listed neither would answer a narrower question than the run.
	fmt.Fprintf(w, "==> forge: push each branch to origin · %s\n", openPRLine())
	if n := s.cfg.Review.MaxReviewRounds; n > 0 {
		fmt.Fprintf(w, "==> max review rounds: %d per item\n", n)
	}
}

// drivePlan is --dry-run: every step resolved, nothing launched, no row written.
//
// It reports the skip rather than resolving a branch and a runner for an item
// the run will not touch, and it counts what it would attempt rather than what
// it was handed. A preview that resolves every step regardless is the one read
// an operator has before spending a night, and it said "1 item" about a queue
// that launched nothing.
func (s *session) drivePlan(items []backlog.Item, reviewer string, named bool) error {
	s.sequencePlan(os.Stdout)
	var attempt int
	var drive []queued
	// An item blocked only by deps this drive attempts before it is driven at its
	// turn, once they land, so the preview lists it rather than skipping it.
	ahead := map[string]bool{}
	for _, it := range items {
		state, missing := s.workState(it, s.cfg.LogDir())
		waits := state == stateBlocked && len(missing) > 0 &&
			!slices.ContainsFunc(missing, func(d string) bool { return !ahead[d] })
		if !drivable(state, named) && !waits {
			why := ""
			if state == stateBlocked {
				why = ": blocked by " + strings.Join(missing, " ")
			}
			fmt.Printf("%-6s %-9s %s%s\n", it.ID.Bare(), state, outcomeSkipped, why)
			continue
		}
		attempt++
		ahead[it.ID.Bare()] = true
		drive = append(drive, newQueued(it, named))
		branch, err := s.branchFor(it)
		if err != nil {
			branch = "(" + err.Error() + ")"
		}
		impl := s.runnerFor(it, driveRunner)
		fmt.Printf("%-6s %-9s branch %-46s run %s, review %s\n", it.ID.Bare(), state, branch, impl, reviewer)
		fmt.Printf("%-6s %-9s %s\n", "", "", s.catchUpPlan(it))
		if p, _ := s.cfg.Policy("stale_citations"); p != polOff {
			for _, d := range s.refDrift(it, s.driftRef()) {
				fmt.Printf("%-6s %-9s stale_citations: %s\n", "", "", d)
			}
		}
		if waits {
			fmt.Printf("%-6s %-9s runs once %s finishes earlier in this drive, and skips as blocked if not\n",
				"", "", strings.Join(missing, " "))
		}
	}
	if driveJobs > 1 && len(drive) > 0 {
		writeWaves(os.Stdout, drive, driveJobs)
	}

	fmt.Printf("\n%d item(s) to drive, %d skipped. Nothing launched: --dry-run.\n",
		attempt, len(items)-attempt)
	return nil
}

// catchUpPlan is what a moved trunk costs an item: the gate drive reruns on the
// merged tree, or why none runs.
func (s *session) catchUpPlan(it backlog.Item) string {
	trunk := s.trunk()
	if len(s.cfg.Loop.MergeGate) == 0 {
		return "if " + trunk + " moves: no branch update, since no [drive] merge_gate reads the branch"
	}
	if gate := prompts.SelectVerify(s.cfg.Verify, it.Meta); strings.TrimSpace(gate) != "" {
		return "if " + trunk + " moves: merged in, then the verify command reruns on the result: " + gate
	}
	return "if " + trunk + " moves: merged in, and no verify command exists to rerun on the result"
}

// driveSummary is the count on stderr, so --json's stdout carries the document
// and nothing else.
func (s *session) driveSummary(done []DriveItem, totals DriveTotals) {
	var reviewed, byGate, atForge, parked, skipped, heldBack int
	for _, d := range done {
		switch d.Outcome {
		case outcomeHeld:
			heldBack++
		case outcomeReviewed:
			reviewed++
		case outcomeMergedByGate:
			byGate++
		case outcomeMergedAtForge:
			atForge++
		case outcomeParked:
			parked++
		case outcomeSkipped:
			skipped++
		}
	}
	if !driveJSON {
		for _, d := range done {
			note := d.Park
			if d.Reason != "" {
				note = d.Park + ": " + d.Reason
			}
			fmt.Printf("%-6s %-9s rounds %d  reviews %d  %s\n", d.ID, d.Outcome, d.Rounds, d.Reviews, note)
		}
	}
	merged := ""
	if byGate > 0 {
		merged = fmt.Sprintf(" · %d merged by drover", byGate)
	}
	if atForge > 0 {
		merged += fmt.Sprintf(" · %d merged immediately", atForge)
	}
	if heldBack > 0 {
		merged += fmt.Sprintf(" · %d held on usage", heldBack)
	}
	fmt.Fprintf(os.Stderr, "\n%d attempted · %d ready to merge%s · %d parked · %d skipped\n",
		len(done), reviewed, merged, parked, skipped)
	if line := findingsTally(totals); line != "" {
		fmt.Fprintf(os.Stderr, "%s\n", line)
		for _, f := range totals.FindingsNeedingTriage {
			fmt.Fprintf(os.Stderr, "  not covered: %s\n", findingLine(f))
		}
	}
	for _, d := range done {
		path, drafted := "", false
		if it, err := s.item(d.ID); err == nil {
			path = rel(s.cfg.Root, it.Path)
			drafted = writtenRequirements(it) > 0
		}
		fmt.Fprintf(os.Stderr, "\n%s\n", d.ID)
		deny, _ := s.cfg.Policy("merge_protected_paths")
		f := driveFollowUp(d, path, drafted, s.cfg.Review.MaxReviewRounds, deny)
		if d.PR > 0 {
			// Linked here rather than in driveFollowUp, which builds the text
			// without a session and is what the tests read.
			plain := fmt.Sprintf("PR #%d", d.PR)
			linked := "PR " + s.prRefTo(os.Stderr, d.PR, "")
			f.Outcome = strings.ReplaceAll(f.Outcome, plain, linked)
			f.Next = strings.ReplaceAll(f.Next, plain, linked)
		}
		if d.CatchUp != nil {
			f.Outcome += "; " + catchUpClause(s.trunk(), d.CatchUp)
		}
		f.write(os.Stderr, "  ")
	}
}

// findingsTally is the findings line under the drive's count, empty when its
// sessions filed nothing.
func findingsTally(t DriveTotals) string {
	var parts []string
	total := 0
	for _, v := range coverages {
		if n := t.FindingsByCoverage[v]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, v))
			total += n
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s filed: %s", total, plural(total, "finding", "findings"), strings.Join(parts, " · "))
}

// catchUpClause is the catch-up in the words the summary appends to an item's
// outcome.
func catchUpClause(trunk string, c *CatchUp) string {
	said := map[string]string{
		catchUpPassed:     "verify command passed",
		catchUpFailed:     "verify command failed",
		catchUpSequence:   "numbered files out of order, merge undone",
		catchUpTrunkRed:   "the trunk failed the verify command alone",
		catchUpSkipped:    "no verify command to check it",
		catchUpUnrunnable: "verify command did not finish",
	}[c.Verify]
	return "updated to " + trunk + "@" + shortSHA(c.To) + "; " + said
}

// followUp is what an outcome leaves the operator to do. A result token says
// what happened; it does not say who acts next or with which command, and an
// operator reading "no_digest" or "blocked_on_deps" at the end of a long drive
// should not have to know the vocabulary to find the next step.
type followUp struct {
	Outcome string
	Next    string
}

func (f followUp) write(w io.Writer, indent string) {
	fmt.Fprintf(w, "%sOutcome:    %s\n%sNext steps: %s\n", indent, f.Outcome, indent, f.Next)
}

// reviewAgain is the command that relaunches a headless review with the runner
// the last one used, so a copied next step reproduces the pass it follows. Only
// a headless pass is reported, so it is the only mode this names.
func reviewAgain(id, runnerName string) string {
	return "drover review " + id + " --runner " + runnerName + " --headless"
}

// passFollowUp answers for one review pass, by the token passResult read.
// again is the command that runs the same review again.
func passFollowUp(token string, p pass, id, item, runnerName, logPath, again string) followUp {
	outcome := passSentence(token, ledger.StageReview, p, "the review", runnerName, logPath)
	body := item
	if body == "" {
		body = "the item body"
	}
	switch token {
	case passChangesRequested:
		return followUp{
			outcome,
			"drover run " + id + ", then " + again,
		}
	case passRepeating:
		return followUp{
			outcome,
			"read " + body + " and decide by hand; another drover run " + id + " meets the same finding",
		}
	case passCapped:
		return followUp{
			outcome,
			"decide by hand: drover split " + id + " for what is left, or raise [review] max_review_rounds and drover run " + id,
		}
	case passChangesUnrecorded:
		return followUp{
			outcome,
			"read the audit with drover show " + id + "; if it holds, drover open-pr " + id + " --force",
		}
	case passStanding:
		return followUp{
			outcome,
			"read " + body + " under its review heading, then either drover request-changes " + id +
				" -f <file> if the findings are still open, or drover close-changes " + id + " if they are closed",
		}
	case passEdited:
		return followUp{
			outcome,
			"diff " + body + " against its history, then either drover request-changes " + id +
				" -f <file> the findings properly, or drover open-pr " + id + " --force if the change was not a finding",
		}
	case passPassed:
		return followUp{
			outcome,
			"drover open-pr " + id,
		}
	case passSilent:
		return followUp{
			outcome,
			again + ", or record what you found with drover audit " + id,
		}
	case passDied:
		where := ""
		if logPath != "" {
			where = "read " + logPath + ", then "
		}
		return followUp{
			outcome,
			where + again,
		}
	case passFaulted:
		return followUp{
			outcome,
			again + " once the service recovers",
		}
	case passUnreadable:
		return followUp{
			outcome,
			"drover show " + id + " lists the rows the review recorded",
		}
	}
	return followUp{outcome, "drover show " + id}
}

// driveFollowUp answers for one item a drive attempted or skipped. A pass token
// in Park means a review stopped the item, and passFollowUp already answers
// for every one of those; the rest are drive's own causes. deny is the
// project's merge_protected_paths policy, which decides who merges a denied path.
func driveFollowUp(d DriveItem, item string, drafted bool, maxReviewRounds int, deny config.Policy) followUp {
	id := d.ID
	again := "drover drive " + id
	pr := "the pull request"
	if d.PR > 0 {
		pr = fmt.Sprintf("PR #%d", d.PR)
	}
	because := ""
	if d.Reason != "" {
		because = ": " + d.Reason
	}
	switch d.Outcome {
	case outcomeReviewed:
		switch {
		case d.Merged:
			return followUp{
				"green and reviewed; " + pr + " went to the forge to merge" + because,
				"nothing yet: the next drover drive finishes it, or drover finish " + id + " once the forge has merged it",
			}
		case d.PR > 0:
			return followUp{
				"green and reviewed; " + pr + " is open and the merge was held" + because,
				"merge " + pr + ", then drover finish " + id,
			}
		}
		return followUp{
			"reviewed, and no pull request opened" + because,
			"declare [drive] merge_gate, or drover open-pr " + id + " --force",
		}
	case outcomeMergedByGate:
		if d.Finished {
			return followUp{pr + " merged on [drive] merge_gate's word, and finished" + because, "nothing"}
		}
		return followUp{
			pr + " merged on [drive] merge_gate's word, and not finished" + because,
			"nothing yet: the next drover drive finishes it, or drover finish " + id,
		}
	case outcomeMergedAtForge:
		switch {
		case d.Finished:
			return followUp{pr + " merged at the forge at once, and finished" + because, "nothing"}
		case errors.Is(d.stop, errMergedRed):
			return followUp{
				pr + " merged at the forge at once, and the merge gate refused it after" + because,
				"read the merge gate's output and fix the trunk, then drover finish " + id + " and drover drive again",
			}
		}
		return followUp{
			pr + " merged at the forge at once, and not finished" + because,
			"nothing yet: the next drover drive finishes it, or drover finish " + id,
		}
	case outcomeHeld:
		return followUp{
			"held: " + d.Reason,
			"nothing yet: drover drive " + id + " once the window resets, or now to spend what is left",
		}
	case outcomeSkipped:
		switch d.Park {
		case stateDone:
			return followUp{"already merged", "nothing"}
		case stateScaffold:
			if drafted {
				return followUp{
					"the requirements are written and nobody has accepted them",
					"read them in " + item + ", then drover accept " + id,
				}
			}
			return followUp{
				"the requirements are not written yet",
				"write them in " + item + ", then drover accept " + id,
			}
		case stateBlocked:
			return followUp{"not attempted" + because, "finish what blocks it, then " + again}
		case stateRan:
			return followUp{
				"it ran already, and an unnamed drive only picks up ready items",
				again + " to resume it by name",
			}
		}
		return followUp{"not attempted (" + d.Park + ")" + because, "drover show " + id}
	}

	switch d.Park {
	case passChangesRequested, passRepeating, passCapped, passStanding, passChangesUnrecorded, passEdited, passSilent, passDied, passUnreadable:
		// The park's reason is passSentence already, worded with the runner, the
		// log and any denial in hand; the summary has none of those to word it
		// again.
		f := passFollowUp(d.Park, pass{cap: maxReviewRounds}, id, item, "", "", again)
		if d.Reason != "" {
			f.Outcome = d.Reason
		}
		return f
	case parkImplementDied, parkImplementUnreadable:
		return followUp{
			"the implement run left drover nothing to read, so no review launched" + because,
			"read the implement log the reason names, then " + again,
		}
	case "runner_fault":
		return followUp{
			"the runner failed on its own service or account, and the drive stopped" + because,
			again + " once the service recovers",
		}
	case parkGateRefused:
		if d.CatchUp != nil && d.CatchUp.Verify == catchUpSequence {
			return followUp{
				"numbered files the branch added clash with or sort below the trunk's" + because,
				"read what came back in " + item + ", then " + again + ", or drover run " + id,
			}
		}
		if d.CatchUp != nil && d.CatchUp.Verify == catchUpFailed {
			return followUp{
				"the item's verify command failed on the branch merged with the trunk, which passes it alone" + because,
				"read what came back in " + item + ", then " + again + ", or drover run " + id,
			}
		}
		if d.PR > 0 {
			return followUp{
				pr + " is open and the merge gate refused it at the merge" + because,
				"read the merge gate's output above, then merge " + pr + " by hand and drover finish " + id + ", or " + again,
			}
		}
		return followUp{
			"the merge gate blamed this diff and wrote its output back to " + item,
			again + ", or drover run " + id,
		}
	case parkTrunkConflict:
		return followUp{
			"the trunk moved under the branch and would not merge into it" + because,
			"merge the trunk into " + d.Branch + " by hand, resolve, push, then " + again,
		}
	case "forge_down":
		return followUp{
			"the merge gate failed for a reason this diff did not cause, and the drive stopped",
			"check the forge's own status, then " + again,
		}
	case parkTrunkRed:
		return followUp{
			"the trunk fails the item's verify command on its own, and the drive stopped" + because,
			"fix the trunk, then " + again,
		}
	case "gate_unrunnable":
		if caughtUpGateUnrunnable(d) {
			return followUp{"the item's verify command would not run on the updated tree" + because, "fix what the reason names, then " + again}
		}
		return followUp{"[drive] merge_gate would not run" + because, "fix [drive] merge_gate, then " + again}
	case "protected_paths":
		next := "review and merge " + pr + " by hand, then drover finish " + id
		if supervisorMay(deny) {
			next = "read the diff of each denied file, merge " + pr + ", then drover finish " + id
		}
		return followUp{pr + " is open and changes a path under [drive] protected_paths" + because, next}
	case parkOperatorRequirement:
		return followUp{
			pr + " is open and " + d.Reason,
			"meet each one, drover signoff " + id + " <n> \"<what was checked>\", then merge " + pr + " by hand and drover finish " + id,
		}
	case "blocked_on_deps":
		return followUp{
			pr + " is open and waits on work the review split out" + because,
			"accept and finish what it waits on, then merge " + pr + " and drover finish " + id,
		}
	case parkDrained:
		return followUp{
			"the drive stopped for another item's reason while this one ran, and it stopped at a stage boundary" + because,
			again + " once that reason is dealt with",
		}
	case "interrupted":
		if caughtUpGateUnrunnable(d) {
			return followUp{"interrupted while the item's verify command ran on the updated tree; the worktree stays at " + d.Tree, again + " to continue"}
		}
		return followUp{"interrupted between rounds", again + " to continue"}
	case parkUncommitted:
		return followUp{
			"the run ended with work it never committed" + because,
			"cd " + d.Tree + ", commit the work on " + d.Branch + " or discard it, then " + again,
		}
	case parkWorktree:
		return followUp{"the item's worktree would not open" + because, "fix what the reason names, then " + again}
	case parkNoCommits:
		return followUp{
			"the run committed nothing, so nothing was pushed or reviewed" + because,
			again + " to run it again from the start",
		}
	}
	return followUp{"parked (" + d.Park + ")" + because, "fix what the reason names, then " + again}
}

// caughtUpGateUnrunnable reports whether the item's gate on the caught-up tree is
// what stopped. A catch-up that merged stays on the row whatever its gate said,
// so a passed one followed by a park for another reason must not claim it.
func caughtUpGateUnrunnable(d DriveItem) bool {
	return d.CatchUp != nil && d.CatchUp.Verify == catchUpUnrunnable
}

// What the gate said. The exit codes carry different questions, and a red
// check means opposite things depending on which one it answers.
const (
	gateGreen  = 0 // merge
	gateDiff   = 1 // red, and this diff caused it
	gateForge  = 2 // red, and the code did not; anything else lands here too
	gateBehind = 4 // refused only because the trunk moved under the head
	gateAbsent = -1
)

// envGateMerged tells [drive] merge_gate that the head it reads is already on
// the trunk. After a squash or a rebase the head is never an ancestor of the
// trunk, so a gate implementing exit 4 as an ancestry test refuses every merged
// head, and catching up a merged branch is not something drive can do.
const envGateMerged = "DROVER_GATE_MERGED"

// runGate runs [drive] merge_gate with the branch and the trunk appended, the
// contract [git] merge_check already uses, and reads its exit status.
//
// A binary that cannot be found is a misconfiguration rather than a "no", the
// distinction mergeCheck already draws: a gate that will not run will not run
// for the next item either, so walking the rest of the queue would park every
// item for one reason and merge none.
func (s *session) runGate(ctx context.Context, branch, trunk string) (code int, out string, err error) {
	return s.runGateOn(ctx, branch, trunk, false)
}

// runGateOn is runGate, saying whether the head is one the forge already
// merged. The variable is set on every run, empty before the merge, so a value
// left in the operator's environment cannot switch the behind check off.
func (s *session) runGateOn(ctx context.Context, branch, trunk string, merged bool) (code int, out string, err error) {
	argv := s.cfg.Loop.MergeGate
	if len(argv) == 0 {
		return gateAbsent, "", nil
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return gateForge, "", fmt.Errorf("[drive] merge_gate names %q, which is not on PATH", argv[0])
	}
	full := append(append([]string{}, argv[1:]...), branch, trunk)
	cmd := exec.CommandContext(ctx, argv[0], full...) // #nosec G204 -- argv is project config
	cmd.Dir = s.workDir()
	flag := ""
	if merged {
		flag = "1"
	}
	cmd.Env = append(os.Environ(), runner.EnvRoot+"="+s.cfg.Root, envGateMerged+"="+flag)
	b, runErr := cmd.CombinedOutput()
	out = string(b)
	if runErr == nil {
		return gateGreen, out, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if c := ee.ExitCode(); c == gateDiff || c == gateBehind {
			return c, out, nil
		}
		// Every other status, named or not, stops the drive. An unclassified
		// failure is one nobody has told drover how to read, and stopping is the
		// answer that cannot merge on it.
		return gateForge, out, nil
	}
	return gateForge, out, runErr
}

// deniedPaths are the files a merge refuses to carry.
//
// An agent that edits the gate, a workflow or the config in its own pull request
// changes what every later item that night is measured by, and that change would
// be reviewed by the loop it just altered. The built-in set is what a project
// gets when it declares none.
func (s *session) deniedPaths(branch, trunk string) ([]string, error) {
	globs := s.cfg.Loop.DenyPaths
	if len(globs) == 0 {
		globs = []string{".drover.toml", ".github/workflows/*", ".github/*.sh", "Makefile"}
	}
	files, err := s.changedNames(branch, trunk)
	if err != nil {
		return nil, fmt.Errorf("could not read what %s changed: %w", branch, err)
	}
	var hit []string
	for _, f := range files {
		for _, g := range globs {
			if denyMatch(g, f) {
				hit = append(hit, f)
				break
			}
		}
	}
	return hit, nil
}

// denyHold is the merge hold: the park token and its reason when the branch
// changes a denied path, empty when the merge may go ahead. The names are
// quoted for the reason only after the decision is made on the raw bytes.
func (s *session) denyHold(branch, trunk string) (park, reason string) {
	hits, err := s.deniedPaths(branch, trunk)
	switch {
	case err != nil:
		return "deny_unreadable", err.Error()
	case len(hits) > 0:
		how := "merge it by hand"
		if p, _ := s.cfg.Policy("merge_protected_paths"); supervisorMay(p) {
			how = "read the diff of each before merging it"
		}
		return "protected_paths", "this branch changes " + displayPaths(hits) +
			", which decides what every later item tonight is measured by; " + how
	}
	return "", ""
}

// changedNames and trackedNames are the only two readings of the tree a
// protected_paths entry is ever matched against, and both go through gitNames so
// the merge hold and check see the same bytes for the same file.
//
// --no-renames reports a rename as the deletion and the addition it is, so a
// denied file moved out of its directory still names the path it left.
// --no-relative keeps diff.relative from narrowing a nested project's diff to
// its own directory and naming what is left relative to it.
func (s *session) changedNames(branch, trunk string) ([]string, error) {
	base, err := forkPoint(s.cfg.Root, trunk, branch)
	if err != nil {
		return nil, err
	}
	return gitNames(s.cfg.Root, "diff", "-z", "--name-only", "--no-renames", "--no-relative", base, branch, "--")
}

// trunkRefs are what a branch's own work is measured against: the local trunk,
// and origin's beside it when there is one. catchUp merges origin's trunk into
// a branch and leaves the local one where it was, and a tree resumed from an
// earlier drive never brings it level either, so a reading against the local
// trunk alone counts everything origin gained since as this branch's change.
// The local trunk stays in the set because an item cut from it can carry the
// operator's unpushed commits, which are no more the item's than origin's are.
func trunkRefs(root, trunk string) []string {
	refs := []string{trunk}
	if remote := "origin/" + trunk; gitcmd.Command(root, "rev-parse", "--verify", "--quiet", remote+"^{commit}").Run() == nil {
		refs = append(refs, remote)
	}
	return refs
}

// forkPoint is where branch left the trunk, counting both trunkRefs as the
// trunk: git merge-base given more than two commits answers against a merge of
// all but the first, which is the commit the branch would be compared to had
// the two trunks been one.
func forkPoint(root, trunk, branch string) (string, error) {
	cmd := gitcmd.Command(root, append([]string{"merge-base", branch}, trunkRefs(root, trunk)...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not find where %s left %s: %w %s", branch, trunk, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *session) trackedNames() ([]string, error) {
	return gitNames(s.cfg.Root, "ls-files", "-z", "--full-name", "--", ":/")
}

// gitNames runs a git command that lists paths and returns each one as the
// bytes git stored. Without -z, core.quotePath (on by default) wraps a
// non-ASCII, tab, newline or quote-bearing name in quotes and escapes it, and a
// newline in a name splits one path into two; either way the name reaching the
// matcher is not the file. Every caller passes -z, and the output is split on
// NUL alone.
func gitNames(root string, args ...string) ([]string, error) {
	cmd := gitcmd.Command(root, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	var names []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			names = append(names, f)
		}
	}
	return names, nil
}

// denyMatch is the one reading of a protected_paths entry, shared by the merge hold
// and by check, so an entry check counts as covering a file is an entry that
// holds a branch changing it. A trailing slash names a directory and a trailing
// star a prefix, both reaching every depth below; anything else is a path.Match
// glob against the repo-relative path. path.Match alone never matches below a
// directory, which is how "migrations/" once held nothing.
func denyMatch(pattern, file string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(file, pattern)
	}
	if ok, _ := path.Match(pattern, file); ok {
		return true
	}
	return strings.HasSuffix(pattern, "*") && strings.HasPrefix(file, strings.TrimSuffix(pattern, "*"))
}

// displayPaths joins names for a person to read. A name git would have quoted,
// or one a reader could not tell apart from its neighbors (a space at either
// end, a comma), is Go-quoted so a newline in it cannot forge a second line of
// the message.
func displayPaths(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		q := strconv.Quote(n)
		if q[1:len(q)-1] != n || strings.TrimSpace(n) != n || strings.Contains(n, ", ") {
			out[i] = q
		} else {
			out[i] = n
		}
	}
	return strings.Join(out, ", ")
}

// pushBranch puts the branch on the remote, which is what makes every later
// question about it answerable.
//
// The gate reads check runs for a SHA and the merge is bound to one with
// --match-head-commit, so a remote head drover never wrote leaves both asking
// about a commit the forge has not seen. It runs twice per round for that
// reason: once after the implement stage, so CI builds through the review rather
// than after it, and once entering driveFinish, because a FIX-IN-REVIEW commit
// moves the head after the first push.
//
// Never --force or --force-with-lease. A branch can legitimately carry a
// human's commits, and a rejected push is an answer to report rather than an
// obstacle to overwrite.
func (s *session) pushBranch(branch string) error {
	// [run] branch renders a name from the item, and a name that renders is not a
	// ref that exists. Without this, git's own "src refspec does not match any"
	// names neither the template that produced the name nor the fact that nothing
	// ever created it.
	if err := gitAt(s.cfg.Root, "rev-parse", "--verify", "--quiet", branch); err != nil {
		return fmt.Errorf(
			"no branch %q exists here: [run] branch renders that name from the item, and nothing created it", branch)
	}
	return gitAt(s.cfg.Root, "push", "--set-upstream", "origin", branch)
}

// mergeBranch asks the forge to merge, bound to the commit the gate tested, by
// the project's [git] merge_method.
//
// auto lets branch protection and the required checks decide, which is an
// authority an agent's own pull request cannot weaken without that change
// itself going through a pull request. --match-head-commit is what stops a push
// finishing between the gate and the merge: without it `gh` merges whatever the
// head is now, which may be a commit nothing tested.
//
// A forge that refuses auto-merge as a feature returns errNoAutoMerge, matched
// on the refusal's own wording: every other error is the forge declining this
// pull request, and reading those as "no auto-merge" would hand the gate a merge
// the forge had a reason to refuse.
func (s *session) mergeBranch(ctx context.Context, branch string, auto bool) error {
	head, err := revParse(s.cfg.Root, branch)
	if err != nil {
		return err
	}
	method := s.cfg.Git.MergeMethod
	if method == "" {
		method = config.MergeMethods()[0]
	}
	args := []string{"pr", "merge"}
	if auto {
		args = append(args, "--auto")
	}
	args = append(args, "--"+method, "--match-head-commit", head, branch)
	cmd := exec.CommandContext(ctx, "gh", args...) // #nosec G204 -- branch and head come from git, method from validated config
	cmd.Dir = s.cfg.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(out))
		if auto && autoMergeRefused(text) {
			return fmt.Errorf("%w: %s", errNoAutoMerge, text)
		}
		return fmt.Errorf("gh pr merge: %w: %s", err, text)
	}
	return nil
}

// errNoAutoMerge is a forge that will not queue a merge for any pull request,
// as opposed to one declining this one.
var errNoAutoMerge = errors.New("the forge does not allow auto-merge")

// autoMergeRefused reads gh's output for the forge refusing auto-merge itself.
// GitHub names the GraphQL mutation in both forms it uses: "Auto merge is not
// allowed for this repository (enablePullRequestAutoMerge)" where the setting
// is off or the plan lacks it, and "Protected branch rules not configured for
// this branch (enablePullRequestAutoMerge)" where nothing exists to wait on.
func autoMergeRefused(out string) bool {
	low := strings.ToLower(out)
	for _, sign := range []string{"enablepullrequestautomerge", "auto merge is not allowed", "auto-merge is not allowed"} {
		if strings.Contains(low, sign) {
			return true
		}
	}
	return false
}

// reconcile finishes what merged since the last drive.
//
// A kill between the merge and the ledger row leaves an item merged, unrecorded,
// and reading as `ran`, which keeps it out of the queue forever: the worst state
// a drive can leave, and the one nothing else repairs. --auto makes it the
// ordinary state rather than a crash, because the forge completes the merge
// after drive has moved on.
//
// It invokes finish as a command rather than reimplementing it. That skips the
// root's PersistentPreRunE, and what closes the hole is the classification
// table: drive is operator too, so a session that cannot reach finish cannot
// reach it through drive. TestDriveIsNoMoreOpenThanTheStepsItDrives holds them
// together.
// reconcile finishes what a previous drive merged and did not get to record,
// which is the state a kill between the merge and the finish leaves: the item
// reads `ran` forever and the queue skips it from then on.
//
// It finishes only items drover launched. A merged branch alone does not say a
// drive produced the work; an operator implementing an item in their own
// session leaves exactly the same branch and the same merge, and adopting that
// would have drive record a completion for work it never drove and never
// reviewed. A stage row is the discriminator because drover writes it itself
// at every launch, and nothing else in the tree can forge one.
//
// No disposition reaches the tracker. Finishing on the way past is recovery, not
// certification, and an issue closed unattended is visible to whoever filed it.
// The mirror is written all the same: its issues are drover's own, and one left
// behind keeps check exiting 2 until somebody accepts each item by hand.
//
// A finish writes the ledger and a report into the operator's checkout, so the
// clean-tree boundary has to hold across the writes as well as before them: a
// record git would report leaves that checkout dirty, and every later drive
// skips reconciliation over it. It is refused before finish writes anything, and
// the stop comes back as err: counts alone cannot tell the caller that reconciliation
// ended early with a merged item still unrecorded.
func (s *session) reconcile() (landed, left int, err error) {
	trunk := s.trunk()
	if trunk == "" {
		return 0, 0, nil
	}
	restore := finishNoTracker
	finishNoTracker = true
	defer func() { finishNoTracker = restore }()

	for _, it := range s.backlog.Items {
		id := it.ID.Bare()
		if s.ledger.State(id).Merged {
			continue
		}
		branch, err := s.branchFor(it)
		if err != nil || branch == "" {
			continue
		}
		answer, _, err := s.mergedInto(branch, trunk)
		if err != nil || answer == mergeNo {
			continue
		}
		// Named rather than skipped. `finish` refuses an unvouched ancestry yes on
		// its own, so reaching it would refuse rather than record — but it would do
		// so one error line per item, and a drive that walked past the item in
		// silence tells the operator nothing about an id whose work may not be on
		// the trunk. This is the report the tip-equality case below already gives,
		// for the wider case where the branch sits behind the trunk rather than on
		// it.
		if answer == mergeUnvouched {
			left++
			fmt.Fprintf(os.Stderr, "· %s is merged only because nothing shows its branch holding a commit; leaving it alone\n", id)
			fmt.Fprintf(os.Stderr, "    check whether %s is on %s, then drover finish %s --force\n", id, trunk, id)
			continue
		}
		// A branch that carried no commit is an ancestor of the trunk by
		// definition, so ancestry reports it merged and it shipped nothing. A
		// run killed before its first commit leaves exactly that: the branch
		// still points at the trunk it was cut from. Adopting one records a
		// completion for work that does not exist and unblocks every item
		// declaring it on deps, which is how a night's queue builds on an
		// empty branch.
		//
		// Tip equality is the test rather than a commit count, because a
		// merge commit leaves the branch an ancestor and a squash leaves it
		// unrelated; only a branch nobody advanced still resolves to the same
		// object as the trunk. A fast-forward merge does too, and lands here
		// as a report rather than an adoption, which costs the operator one
		// `drover finish` and never a false completion.
		if same, err := sameCommit(s.cfg.Root, branch, trunk); err == nil && same {
			left++
			fmt.Fprintf(os.Stderr, "· %s is merged only because its branch has no commits; leaving it alone\n", id)
			fmt.Fprintf(os.Stderr, "    %s is the trunk's own tip, so nothing shipped\n", branch)
			continue
		}
		if s.ledger.Counts(id).Launches() == 0 {
			left++
			fmt.Fprintf(os.Stderr, "· %s is merged, but drover never launched it; leaving it alone\n", id)
			fmt.Fprintf(os.Stderr, "    drover finish %s\n", id)
			continue
		}
		// Asked before every finish rather than once at the top: a finish writes
		// the ledger and a report, so a project tracking either has a dirty tree
		// by the second item. cleanForFinish answers both halves,
		// the tree as it stands and what finish is about to write to it.
		if err := s.cleanForFinish(it); err != nil {
			return landed, left, err
		}
		fmt.Fprintf(os.Stderr, "· %s merged since the last drive; finishing it\n", id)
		finishInDrive = true
		lerr := finishCmd.RunE(finishCmd, []string{id})
		finishInDrive = false
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "  ! finish %s: %v\n", id, lerr)
			continue
		}
		landed++
		if err := s.reload(); err != nil {
			return landed, left, err
		}
	}
	return landed, left, nil
}

// errDirtyTree is the sentinel a dirty tree raises: a switch would carry that
// work onto a branch it does not belong to.
var errDirtyTree = errors.New("the working tree holds uncommitted work, and a switch would carry it")

// leaveItem is done with an item's worktree. A clean one is removed, since the
// branch holds everything it had and a later drive recreates it; one holding
// uncommitted work stays, parks this item on it, and the queue moves on, because
// no later item runs in it. Nothing here switches a branch.
func (s *session) leaveItem(d *DriveItem) {
	if s.tree == "" {
		return
	}
	paths, err := s.dirtyPaths()
	if err != nil {
		d.Reason += "; the worktree stays at " + s.tree + ", because " + err.Error()
		return
	}
	if len(paths) > 0 {
		s.stayDirty(d, paths)
		return
	}
	if err := gitAt(s.cfg.Root, "worktree", "remove", s.tree); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s: worktree kept at %s: %v\n", d.ID, s.tree, err)
		return
	}
	d.Tree = ""
}

// stayDirty parks d on the uncommitted work its own sessions left in its
// worktree, keeping whatever it had concluded before in the reason. The tree
// stays for the operator to inspect, and the drive goes on to the next item.
func (s *session) stayDirty(d *DriveItem, paths []string) {
	branch := d.Branch
	if branch == "" {
		branch = "the item's branch"
	}
	why := fmt.Sprintf("the run ended with work it never committed: %s. It is in %s, on %s: commit it there or discard it, "+
		"then drover drive %s", pathList(paths), s.tree, branch, d.ID)
	if d.Park != "" || d.Reason != "" {
		why += fmt.Sprintf(" (before that: %s)", strings.TrimSpace(d.Park+" "+d.Reason))
	}
	d.Outcome, d.Park, d.Reason = outcomeParked, parkUncommitted, why
	_ = s.record(ledger.Row{ID: d.ID, Pass: parkUncommitted, Date: ledger.Today()})
}

// ownRecords is the configured record paths among paths: what a park names so a
// project tracking its ledger or backlog learns why its drives stop, rather than
// reading a list of rows drover itself wrote and wondering who left them.
func (s *session) ownRecords(paths []string) []string {
	var own []string
	for _, p := range []string{s.cfg.Paths.Ledger, s.cfg.Paths.Backlog, s.cfg.Paths.Reports,
		s.cfg.Paths.Register, s.cfg.Paths.Logs} {
		if p == "" {
			continue
		}
		rel, err := filepath.Rel(s.cfg.Root, s.cfg.Abs(p))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		rel = filepath.ToSlash(rel)
		for _, path := range paths {
			if path == rel || strings.HasPrefix(path, rel+"/") {
				own = append(own, rel)
				break
			}
		}
	}
	return own
}

// cleanForFinish refuses a finish that would leave the tree dirty or start from a
// dirty one. It runs before finish writes anything, so a refusal leaves no
// completion row behind and the branch the operator stood on unchanged.
//
// Each record finish writes counts the way porcelain would count it once
// written: a path git ignores, or one outside the worktree, never dirties the
// tree; a tracked one, or an untracked one git does not ignore, does. An
// append also counts every other name of the file it changes, so an ignored or
// external hard link to a tracked file dirties the tree through that file. The
// report is counted only when a run log exists for finish to write it from.
func (s *session) cleanForFinish(it backlog.Item) error {
	id := it.ID.Bare()
	paths, err := s.dirtyPaths()
	if err != nil {
		return fmt.Errorf("not finishing %s: %w", id, err)
	}
	if len(paths) > 0 {
		return Refusal{fmt.Errorf("not finishing %s: %w: %s.\nCommit it on the branch it belongs to or discard it, then drover drive",
			id, errDirtyTree, pathList(paths))}
	}
	writes := []recordWrite{appendsTo(s.cfg.Abs(s.cfg.Paths.Ledger))}
	if _, _, err := s.reportData(it); err == nil {
		writes = append(writes, replaces(s.reportPath(it)))
	}
	top, err := s.worktreeRoot()
	if err != nil {
		return fmt.Errorf("not finishing %s: %w", id, err)
	}
	var seen []string
	for _, w := range writes {
		dest, err := w.destination()
		if err != nil {
			return fmt.Errorf("not finishing %s: %w", id, err)
		}
		if rel, ok := inWorktree(top, dest); ok {
			ignored, err := gitIgnores(top, rel)
			if err != nil {
				return fmt.Errorf("not finishing %s: %w", id, err)
			}
			if !ignored {
				seen = append(seen, rel)
				continue
			}
		}
		also, err := w.aliases(top, dest)
		if err != nil {
			return fmt.Errorf("not finishing %s: %w", id, err)
		}
		for _, rel := range also {
			if !slices.Contains(seen, rel) {
				seen = append(seen, rel)
			}
		}
	}
	if len(seen) > 0 {
		return Refusal{fmt.Errorf("not finishing %s: finish changes %s, which git does not ignore, "+
			"so the finish would leave your checkout dirty and every later drive would skip reconciliation over it.\n"+
			"Ignore those paths or keep them outside the host repo, "+
			"or finish it yourself with drover finish %s and commit the record where it belongs. Nothing was written or switched",
			id, strings.Join(seen, ", "), id)}
	}
	return nil
}

// worktreeRoot is the top of the Git worktree the project sits in, which is
// what a checkout changes. The project root is only where .drover.toml lives:
// a project nested in a larger repo has records beside it, under the same
// checkout, that a containment test against the project root calls external.
func (s *session) worktreeRoot() (string, error) { return showToplevel(s.cfg.Root) }

// showToplevel is the top of the Git worktree dir sits in.
func showToplevel(dir string) (string, error) {
	cmd := gitcmd.Command(dir, "rev-parse", "--show-toplevel")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %s", dir, strings.TrimSpace(stderr.String()))
	}
	// Only git's own terminator comes off: whitespace ending the directory
	// name is part of the path, and trimming it moves every record outside.
	return strings.TrimSuffix(string(out), "\n"), nil
}

// recordWrite is one path finish writes, reduced to the directory entry the
// write changes. git prints the worktree top with symlinks resolved, so the
// entry is compared resolved too, and how far a symlink resolves depends on how
// its writer mutates the path, which the path alone cannot say.
//
// A destination that is ignored or outside the worktree is exempt only for its
// own name. aliases answers what else the write changes: the Git-visible paths
// under top that name the same file, which porcelain reports once written.
type recordWrite interface {
	destination() (string, error)
	aliases(top, dest string) ([]string, error)
}

// appendsTo is a path opened with O_APPEND|O_CREATE and written, as
// ledger.Append does. The bytes land at the referent, so every symlink on the
// way resolves, a dangling one included: O_CREATE follows it and creates the
// file it names.
type appendsTo string

func (p appendsTo) destination() (string, error) { return followLinks(string(p)) }

// aliases is every hard link to dest that git sees, since an append changes
// the inode and so every name it has. A dest the open would create has none.
func (appendsTo) aliases(top, dest string) ([]string, error) {
	fi, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dest, err)
	}
	return visibleLinks(top, fi)
}

// replaces is a path a new file is renamed over, as reports.Write does. The
// rename swaps the directory entry itself: a symlink at the final component is
// replaced where it stands, wherever it pointed, so only the parents resolve.
type replaces string

func (p replaces) destination() (string, error) {
	clean := filepath.Clean(string(p))
	dir, err := followLinks(filepath.Dir(clean))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(clean)), nil
}

// aliases is empty: a rename unlinks the old inode from this one entry and
// leaves its other names, and their bytes, as they were.
func (replaces) aliases(string, string) ([]string, error) { return nil, nil }

// visibleLinks lists the paths under top that git status reports on, tracked
// or untracked and not ignored, which are hard links to fi. The listing is
// git's own, -z so each name arrives as the bytes on disk; a symlink among
// them is its own file to git, so it is compared unfollowed.
func visibleLinks(top string, fi os.FileInfo) ([]string, error) {
	files, err := visibleFiles(top, "")
	if err != nil {
		return nil, err
	}
	var links []string
	for _, rel := range files {
		other, err := os.Lstat(filepath.Join(top, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", rel, err)
		}
		if os.SameFile(fi, other) && !slices.Contains(links, rel) {
			links = append(links, rel)
		}
	}
	return links, nil
}

// visibleFiles is every file git status observes under the worktree at
// prefix inside top, submodules included. A parent repo lists a submodule as
// one gitlink entry, yet status with --ignore-submodules=none reports the
// submodule modified when any file inside it changes, so each populated
// submodule is listed by its own repo, recursively. A gitlink with nothing
// checked out has no content status can see and is skipped; one whose repo
// cannot be read is an error, never an empty listing.
func visibleFiles(top, prefix string) ([]string, error) {
	repo := filepath.Join(top, filepath.FromSlash(prefix))
	files, err := lsFiles(repo, "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	staged, err := lsFiles(repo, "--stage")
	if err != nil {
		return nil, err
	}
	for i := range files {
		files[i] = prefix + files[i]
	}
	for _, entry := range staged {
		meta, rel, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		dir := filepath.Join(repo, filepath.FromSlash(rel))
		if fi, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) || (err == nil && !fi.IsDir()) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat %s: %w", prefix+rel, err)
		}
		sub, err := showToplevel(dir)
		if err != nil {
			return nil, err
		}
		if sub != dir {
			continue
		}
		inner, err := visibleFiles(top, prefix+rel+"/")
		if err != nil {
			return nil, err
		}
		files = append(files, inner...)
	}
	return files, nil
}

// lsFiles runs git ls-files -z in repo with flags, one name per entry, as
// the bytes git read them.
func lsFiles(repo string, flags ...string) ([]string, error) {
	cmd := gitcmd.Command(repo, append([]string{"ls-files", "-z"}, flags...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// maxLinks bounds a symlink chain the way the kernel's own ELOOP does, so a
// cycle is an error rather than a hang.
const maxLinks = 40

// followLinks resolves every symlink in the absolute path p, the final
// component included, and carries on through a link whose referent is missing:
// that referent is where an open with O_CREATE puts the file, and
// filepath.EvalSymlinks fails there instead. A relative target is read from
// the directory holding the link. Past the first missing component the rest
// stays as written, since nothing under a missing directory can be a link.
func followLinks(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("resolve %s: not an absolute path", p)
	}
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	resolved := vol + sep
	pending := strings.Split(p[len(vol):], sep)
	for links := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return filepath.Join(append([]string{next}, pending...)...), nil
		case err != nil:
			return "", fmt.Errorf("resolve %s: %w", p, err)
		case fi.Mode()&os.ModeSymlink == 0:
			resolved = next
			continue
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("resolve %s: more than %d symlinks deep", p, maxLinks)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", p, err)
		}
		if filepath.IsAbs(target) {
			vol = filepath.VolumeName(target)
			resolved, target = vol+sep, target[len(vol):]
		}
		pending = append(strings.Split(target, sep), pending...)
	}
	return resolved, nil
}

// inWorktree reports abs relative to top, slash-separated, and whether it lies
// inside it. abs is taken as already resolved, since only its writer knows how
// far that goes. Containment compares whole path components: "..ledger" is a
// file inside top, "../ledger" is not.
func inWorktree(top, abs string) (string, bool) {
	rel, err := filepath.Rel(top, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// gitIgnores reports whether git ignores path. A tracked path is never
// ignored, whatever the ignore rules say, because writing it shows in status.
func gitIgnores(root, path string) (bool, error) {
	err := gitcmd.Command(root, "check-ignore", "-q", "--", path).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git check-ignore %s: %w", path, err)
	}
}

// errGateForge is the sentinel a gate's exit 2 raises: not this item's failure, so
// not this item's park, and every later item would build on a trunk that never
// received this one.
var errGateForge = errors.New("the merge gate reported a failure the code did not cause")

// errTrunkRed is the sentinel a trunk that fails an item's gate on its own
// raises. Every item behind this one would catch up to the same trunk and fail
// the same way, and no request-changes could fix what none of them caused.
var errTrunkRed = errors.New("the trunk fails the verify command on its own")

// errRunnerFault is the sentinel a transport failure or an exhausted account
// raises. Not this item's failure, and true of every item behind it in the
// queue: a drive that kept walking would park the lot for one reason and merge
// none of them.
var errRunnerFault = errors.New("the runner failed on the service or the account, not on the work")

// driveFinish is everything after a review passes: the gate, the paths a merge
// may not carry, and the merge itself.
// d is a pointer because park closes over driveItem's own copy: a field set on a
// value here would reach every path that returns it and none that parks, so a
// parked item would report no pull request over one that is open.
func (s *session) driveFinish(ctx context.Context, it backlog.Item, d *DriveItem,
	park func(cause, why string) DriveItem, deadline time.Time,
) DriveItem {
	trunk := s.trunk()
	// A review can leave uncommitted work as a run can, and the push, the gate and
	// the pull request below would each describe a head that lacks it.
	if paths, err := s.dirtyPaths(); err != nil {
		return park("branch_failed", err.Error())
	} else if len(paths) > 0 {
		s.stayDirty(d, paths)
		return *d
	}
	// Again, after the review. The first push was before it, and a
	// FIX-IN-REVIEW commit lands between the two: without this the gate reads
	// check runs for a commit the reviewer has already superseded.
	if err := s.recordHead(d.ID, d.Branch); err != nil {
		return park("ledger_unwritable", "the reviewed branch holds commits and the ledger would not record that: "+err.Error())
	}
	if err := s.pushBranch(d.Branch); err != nil {
		d.setDetail(pushDetail(s.cfg.Root, err))
		return park("push_failed", "the branch would not push after the review, so the merge gate would read a commit that is not the head: "+err.Error())
	}
	// Only where a gate will read the branch: with none, nothing below asks
	// whether the branch is level with the trunk, and a merge commit nobody
	// verifyCommands is a change to the branch with no reason behind it.
	if len(s.cfg.Loop.MergeGate) > 0 {
		events.stage(d.ID, stageCatchUp)
		c, p, why := s.catchUp(ctx, it, d, trunk)
		if out, stop := s.routeCatchUp(it, d, park, c, p, why, trunk); stop {
			return out
		}
	}
	if out, stop := s.sequenceHold(it, d, park, trunk); stop {
		return out
	}
	var checks settled
	if len(s.cfg.Loop.MergeGate) > 0 {
		head, err := revParse(s.cfg.Root, d.Branch)
		if err != nil {
			return park("branch_failed", err.Error())
		}
		if checks = s.settleChecks(ctx, d, head, deadline); checks.park != "" {
			if checks.stop != nil {
				d.stop = checks.stop
			}
			return park(checks.park, checks.reason)
		}
	}
	return s.finishAtHead(ctx, it, d, park, trunk, deadline, &checks)
}

// finishAtHead is driveFinish from the gate on, once the head's checks have
// settled: the gate, the pull request and the merge. The re-check at the end
// of a drive enters here for a head whose checks have since gone green.
func (s *session) finishAtHead(ctx context.Context, it backlog.Item, d *DriveItem,
	park func(cause, why string) DriveItem, trunk string, deadline time.Time, checks *settled,
) DriveItem {
	code, out, res, err := s.gateAtHead(ctx, it, d, park, trunk, deadline, checks)
	if res != nil {
		return *res
	}
	switch {
	case err != nil:
		d.Reason = err.Error()
		d.Outcome, d.Park = outcomeParked, "gate_unrunnable"
		return *d
	case code == gateAbsent:
		d.Outcome = outcomeReviewed
		d.Reason = "no [drive] merge_gate declared, so nothing could say green"
		s.leaveItem(d)
		return *d
	case code == gateDiff:
		// The gate blamed the diff, which makes it a finding like any other and
		// sends it back the way a reviewer's would. It counts against
		// max_review_rounds, because a round spent on a red check is still a round.
		if err := s.requestChangesFromVerify(it, noneFirst(checks.none, "The merge gate refused this branch. Its output:"), out, checks.code...); err != nil {
			return park("gate_refused", noneFirst(checks.none, "the merge gate blamed this diff and the finding could not be written back: "+err.Error()))
		}
		d.Rounds++
		d.Reason = noneFirst(checks.none, "the merge gate blamed this diff; sent back")
		if len(checks.code) > 0 {
			// The cause is the check, so the detail is its class: the last
			// session's log describes a review that passed.
			d.Reason = codeReason(checks.code[0]) + "; sent back"
			d.setDetail(parkDetail{Detail: ledger.ClassCode, Evidence: codeEvidence(checks.code)})
		}
		d.Outcome = outcomeParked
		d.Park = "gate_refused"
		s.leaveItem(d)
		return *d
	case code == gateBehind:
		return park(parkGateRefused, behindAgain(d.Branch, trunk, out))
	case code == gateForge:
		d.Outcome, d.Park = outcomeParked, "forge_down"
		d.Reason = noneFirst(checks.none, "the merge gate reported a failure this diff did not cause; stopping the drive")
		d.stop = errGateForge
		// After the stop is set, so a tree the gate dirtied keeps the stop it
		// raises instead of this one: the forge recovers on its own and the tree
		// does not.
		s.leaveItem(d)
		return *d
	}

	// The pull request opens here and nowhere later. A green gate on a passed
	// review is the whole claim it makes, so everything below decides whether the
	// merge follows and none of it takes the pull request back: a denied path and
	// an unmet dep are both a human's to merge, and they need something to merge.
	facts := s.factsFor(it)
	events.stage(d.ID, stagePR)
	p, err := s.openPR(it, d.Branch, trunk, facts)
	if err != nil {
		return park("open_pr_failed", "the merge gate passed and the pull request would not open: "+err.Error())
	}
	d.PR = p.Number

	if p, why := s.denyHold(d.Branch, trunk); p != "" {
		return park(p, why)
	}
	if p, why, run := s.operatorHold(it); p != "" {
		d.signoff = run
		return park(p, why)
	}

	// A dep that went unmet during this drive is a SPLIT-OUT the review filed:
	// `drover split` rewrote the deps line, and the branch is sound and waiting on
	// work nobody has accepted. Another round would re-implement against the same
	// gap, so it parks with the pull request open and the blocking ids named in
	// its body.
	if len(facts.BlockedOn) > 0 {
		return park("blocked_on_deps",
			"the review filed work this item now depends on ("+strings.Join(facts.BlockedOn, ", ")+
				"); the branch is green and reviewed and waits for it to finish")
	}
	// After the dependency check, not before it. --no-merge holds a merge that was
	// going to happen; a blocked item was never going to merge, and reporting it
	// reviewed would hide the split that blocked it behind a flag the operator set
	// for an unrelated reason.
	if driveNoMerge {
		d.Outcome = outcomeReviewed
		d.Reason = "--no-merge: green, reviewed and left on its branch"
		s.leaveItem(d)
		return *d
	}
	return s.driveMerge(ctx, it, d, park, trunk, deadline)
}

// driveMerge asks the forge to queue the merge where the forge has something to
// wait on, and merges on the gate's word where it has not. Any refusal other
// than auto-merge itself is the forge saying no about this pull request, and the
// verify does not get to overrule that.
//
// A declared gate goes straight to mergeByGate when the trunk has no required
// checks. GitHub merges an --auto request on the spot when nothing is required,
// and refuses --auto only while checks are still running, so keeping --auto
// there would let a head merge unchecked whenever its CI happened to finish
// before its review did.
func (s *session) driveMerge(ctx context.Context, it backlog.Item, d *DriveItem,
	park func(cause, why string) DriveItem, trunk string, deadline time.Time,
) DriveItem {
	gate := len(s.cfg.Loop.MergeGate) > 0
	if gate {
		if why := s.nothingWaits(trunk); why != "" {
			fmt.Fprintf(os.Stderr, "  · %s: %s; [drive] merge_gate decides at the head\n", d.ID, why)
			return s.mergeByGate(ctx, it, d, park, trunk, deadline)
		}
	}
	events.stage(d.ID, stageMerge)
	err := s.mergeBranch(ctx, d.Branch, true)
	switch {
	case err == nil:
		return s.afterAutoMerge(ctx, it, d, trunk, deadline)
	case errors.Is(err, errNoAutoMerge) && gate:
		fmt.Fprintf(os.Stderr, "  · %s: the forge will not queue an auto-merge; [drive] merge_gate decides at the head\n", d.ID)
		return s.mergeByGate(ctx, it, d, park, trunk, deadline)
	}
	return park("merge_failed", err.Error())
}

// forgeWait is the cached answer to whether the trunk has required checks.
type forgeWait struct {
	required bool
	err      error
}

// nothingWaits is why an auto-merge would not wait at the forge, or "" when the
// trunk has required checks. A lookup that fails reads as nothing waiting: the
// verify then decides, which costs CI wall time and never merges a red head.
func (s *session) nothingWaits(trunk string) string {
	if s.forgeWait == nil {
		ok, err := gh.RequiredChecks(s.labels(), trunk)
		s.forgeWait = &forgeWait{required: ok, err: err}
	}
	switch {
	case s.forgeWait.err != nil:
		return "the required checks on " + trunk + " could not be read (" + s.forgeWait.err.Error() + "), so nothing says the forge would wait"
	case !s.forgeWait.required:
		return trunk + " has no required checks, so nothing at the forge would wait"
	}
	return ""
}

// afterAutoMerge reads what an accepted --auto did. gh returns nil both when
// the forge queued the merge and when it merged on the spot, and only the forge
// can say which.
//
// A merge already made is finished in this drive, so dependents run behind it. Where
// a gate is declared it still reads the merged head: the merge cannot be undone,
// but a red head on the trunk is something every later item would build on, so
// a refusal stops the drive.
func (s *session) afterAutoMerge(ctx context.Context, it backlog.Item, d *DriveItem, trunk string, deadline time.Time) DriveItem {
	d.Merged = true
	n, merged, err := gh.MergedPR(s.labels(), d.Branch)
	switch {
	case err != nil:
		d.Outcome = outcomeReviewed
		d.Reason = "merge requested, and its state could not be read (" + err.Error() + "); the next drive's reconciliation decides it"
		fmt.Fprintf(os.Stderr, "  · %s: %s\n", d.ID, d.Reason)
		s.leaveItem(d)
		return *d
	case !merged:
		d.Outcome = outcomeReviewed
		d.Reason = "merge queued at the forge; the next drive finishes it"
		fmt.Fprintf(os.Stderr, "  · %s: %s\n", d.ID, d.Reason)
		s.leaveItem(d)
		return *d
	}
	if n > 0 {
		d.PR = n
	}
	head, err := revParse(s.cfg.Root, d.Branch)
	if err != nil {
		head = d.Branch
	}
	d.Outcome = outcomeMergedAtForge
	d.Reason = "the forge merged " + shortSHA(head) + " at once, with nothing to wait on"
	if len(s.cfg.Loop.MergeGate) > 0 {
		_, _ = s.waitChecks(ctx, d.ID, head, deadline, nil)
		events.stage(d.ID, stageGate)
		code, out, err := s.runGateOn(ctx, d.Branch, trunk, true)
		if err != nil {
			out = err.Error()
		}
		if err != nil || code != gateGreen {
			d.stop = errMergedRed
			d.Reason = fmt.Sprintf("PR #%d merged at the forge at once, and [drive] merge_gate then refused %s (exit %d), so it is not finished:\n%s",
				d.PR, shortSHA(head), code, lastLines(out, verifyTail))
			if code == gateBehind {
				// No catch-up follows: the head is on the trunk, and the gate
				// never said whether its checks passed.
				d.Reason = fmt.Sprintf("PR #%d merged at the forge at once, and [drive] merge_gate then said %s is behind %s (exit 4) "+
					"with %s=1 set; a merged head cannot be updated, so the merge gate has to skip that check under it. Not finished:\n%s",
					d.PR, shortSHA(head), trunk, envGateMerged, lastLines(out, verifyTail))
			}
			s.leaveItem(d)
			return *d
		}
		d.Reason += "; [drive] merge_gate passed it after"
	}
	s.leaveItem(d)
	if d.Outcome == outcomeMergedAtForge {
		s.finishMerged(it, d)
	}
	s.afterMerge(ctx, d)
	return *d
}

// errMergedRed is the sentinel a gate refusing a head the forge already merged
// raises. Nothing can unmerge it, and every later item would catch up to a trunk
// carrying it.
var errMergedRed = errors.New("the forge merged a head the merge gate refuses")

// mergeByGate merges a pull request the forge would not queue, on the gate's
// word about the exact head that merges.
//
// The gate already passed once, before the pull request opened, and that pass
// is not reused: the checks it read may still have been running, and with no
// auto-merge nothing at the forge waits for them. So the checks finish first,
// the gate reads the head again, and --match-head-commit binds the merge to the
// commit it read. A plain merge without the gate would merge whatever CI
// happened to say at that moment.
func (s *session) mergeByGate(ctx context.Context, it backlog.Item, d *DriveItem,
	park func(cause, why string) DriveItem, trunk string, deadline time.Time,
) DriveItem {
	// A red catch-up parks with the pull request open and unmerged, as a gate
	// refusal here does: --match-head-commit is never handed a head the item's
	// own gate failed.
	events.stage(d.ID, stageCatchUp)
	c, p, why := s.catchUp(ctx, it, d, trunk)
	if out, stop := s.routeCatchUp(it, d, park, c, p, why, trunk); stop {
		return out
	}
	head, err := revParse(s.cfg.Root, d.Branch)
	if err != nil {
		return park("merge_failed", err.Error())
	}
	checks := s.settleChecks(ctx, d, head, deadline)
	if checks.park != "" {
		if checks.stop != nil {
			d.stop = checks.stop
		}
		return park(checks.park, checks.reason)
	}
	code, out, res, gerr := s.gateAtHead(ctx, it, d, park, trunk, deadline, &checks)
	if res != nil {
		return *res
	}
	// A retry caught the branch up, which moved the head the gate just read.
	if head, err = revParse(s.cfg.Root, d.Branch); err != nil {
		return park("merge_failed", err.Error())
	}
	switch {
	case gerr != nil:
		return park("gate_unrunnable", gerr.Error())
	case code == gateBehind:
		return park(parkGateRefused, behindAgain(d.Branch, trunk, out)+", so the pull request stays open")
	case code == gateForge:
		// Still a refusal of this merge, and still a forge that will refuse the
		// next item's for the same reason, so the drive stops as it does at the
		// first gate.
		d.stop = errGateForge
		fallthrough
	case code != gateGreen:
		return park(parkGateRefused, noneFirst(checks.none, fmt.Sprintf("the merge gate refused %s at the merge (exit %d), so the pull request stays open:\n%s",
			shortSHA(head), code, lastLines(out, verifyTail))))
	}
	events.stage(d.ID, stageMerge)
	if err := s.mergeBranch(ctx, d.Branch, false); err != nil {
		return park("merge_failed", err.Error())
	}
	d.Merged = true
	d.Outcome = outcomeMergedByGate
	d.Reason = "the forge has no auto-merge, so [drive] merge_gate passed " + shortSHA(head) + " and drover merged it"
	s.leaveItem(d)
	// A tree the gate dirtied keeps the item parked on it: the merge stands, and
	// the next drive's reconciliation finishes it once the tree is dealt with.
	if d.Outcome == outcomeMergedByGate {
		s.finishMerged(it, d)
	}
	s.afterMerge(ctx, d)
	return *d
}

// finishMerged records a merge this drive made, as finish does: the ledger row, the
// tracker closes and the report. It runs finish itself rather than a copy of it,
// for the reason reconcile does.
//
// Unlike reconcile it reaches the tracker. reconcile adopts a merge some earlier
// drive left behind, which is recovery; this is the finish of a merge made a
// moment ago, which is what an operator running finish would have done next.
//
// A checkout finish cannot write into cleanly is not this item's failure: the
// merge stands, the reason names why it went unrecorded, and the next drive's
// reconciliation finishes it.
func (s *session) finishMerged(it backlog.Item, d *DriveItem) {
	// The item's tree is gone by now and the records live at the root, which is
	// what cleanForFinish has to read.
	s.tree = ""
	events.stage(d.ID, stageFinish)
	if err := s.cleanForFinish(it); err != nil {
		d.Reason += "; not finished here, so the next drive's reconciliation finishes it: " + err.Error()
		return
	}
	restoreTracker, stdout := finishNoTracker, os.Stdout
	finishNoTracker, finishInDrive = false, true
	// finish prints its progress, and stdout under --json carries one object per
	// line and nothing else.
	os.Stdout = os.Stderr
	err := finishCmd.RunE(finishCmd, []string{d.ID})
	os.Stdout = stdout
	finishNoTracker, finishInDrive = restoreTracker, false
	if err != nil {
		d.Reason += "; merged, and finish refused: " + err.Error()
		return
	}
	d.Finished = true
	if err := s.reload(); err != nil {
		d.Reason += "; finished, and the records would not reload: " + err.Error()
	}
}

// unblockedBy is the ready items that list id among their deps and that this
// drive has not yet listed, in queue order. Reading deps rather than taking every
// ready item keeps --follow to what a finish changed: an item that was ready
// all along and left out of a named drive stays out.
func (s *session) unblockedBy(id string, seen map[string]bool) []backlog.Item {
	var out []backlog.Item
	for _, it := range s.queue() {
		if bare := it.ID.Bare(); !seen[bare] && slices.Contains(it.Deps(), id) {
			seen[bare] = true
			out = append(out, it)
		}
	}
	return out
}

// catchUp brings a branch level with the trunk before a gate reads it, and
// returns a park when it cannot.
//
// Each merge this drive makes moves the trunk under every item after it, and a
// verify that requires a branch to be up to date refuses all of them. Merging the
// trunk in, rather than rebasing, keeps every commit already pushed where it is,
// so the push that follows is a fast-forward and nothing is ever forced.
//
// origin's trunk is the reference rather than the local one: the local trunk
// can carry the operator's unpushed commits, and merging those into an item's
// branch would publish them under that item.
//
// The merge produces a tree no session ever ran the item's gate on, and the
// forge may never check it either: CI that runs only on pull requests and on
// the trunk registers nothing for a push to an item branch. So the item's gate
// runs on it here, before anything is pushed. A tree it fails goes back to the
// branch, pushed so the next run starts from it, and the caller sends it back; a
// trunk that fails alone, an unrunnable check or an interrupt undo the merge
// and push nothing.
func (s *session) catchUp(ctx context.Context, it backlog.Item, d *DriveItem, trunk string) (c caughtUp, park, reason string) {
	remote := "origin/" + trunk
	if err := gitAt(s.cfg.Root, "fetch", "--quiet", "origin", trunk); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s: %s not fetched, so the branch update reads the last one fetched: %v\n", d.ID, trunk, err)
	}
	if gitAt(s.cfg.Root, "rev-parse", "--verify", "--quiet", remote) != nil {
		return c, "", ""
	}
	switch behind, err := isAncestor(s.cfg.Root, remote, d.Branch); {
	case err != nil:
		return c, "branch_failed", err.Error()
	case behind:
		return c, "", ""
	}
	dir := s.workDir()
	before, err := revParse(dir, "HEAD")
	if err != nil {
		return c, "branch_failed", err.Error()
	}
	to, err := revParse(s.cfg.Root, remote)
	if err != nil {
		return c, "branch_failed", err.Error()
	}
	from, err := mergeBase(dir, "HEAD", to)
	if err != nil {
		return c, "branch_failed", err.Error()
	}
	fmt.Fprintf(os.Stderr, "  · %s: %s moved since %s was pushed; merging it in\n", d.ID, trunk, d.Branch)
	if err := gitAt(dir, "merge", "--no-edit", remote); err != nil {
		conflicted, _ := gitNames(dir, "diff", "-z", "--name-only", "--diff-filter=U")
		_ = gitAt(dir, "merge", "--abort")
		if len(conflicted) == 0 {
			return c, "branch_failed", "merging " + remote + " into " + d.Branch + " failed: " + err.Error()
		}
		return c, parkTrunkConflict, "merging " + remote + " into " + d.Branch + " conflicts on " + displayPaths(conflicted) +
			"; the merge was aborted and nothing was pushed"
	}
	c.row = &CatchUp{From: from, To: to}
	c.row.Verify, c.out, park, reason = s.verifyCatchUp(ctx, it, d, trunk, before, c.row)
	if park != "" || c.row.Verify == catchUpSequence {
		// Undone rather than left on the local branch: the next drive pushes the
		// branch before it catches up, and would publish a merge nothing checked.
		// A numbered-files failure is undone too, unlike a red gate: the fix is a
		// rename on the branch's own commits, and a pushed merge would only be
		// one more commit carrying the old number.
		if err := gitAt(dir, "reset", "--keep", before); err != nil {
			why := "the merge stays on the local " + d.Branch + ", unpushed, because it would not undo: " + err.Error()
			if park == "" {
				return c, "branch_failed", why
			}
			reason += "; " + why
		}
		return c, park, reason
	}
	if err := s.recordHead(d.ID, d.Branch); err != nil {
		return c, "ledger_unwritable", "the branch took the trunk and the ledger would not record its head: " + err.Error()
	}
	if err := s.pushBranch(d.Branch); err != nil {
		d.setDetail(pushDetail(s.cfg.Root, err))
		return c, "push_failed", "the branch took the trunk and would not push: " + err.Error()
	}
	return c, "", ""
}

// What a catch-up at dispatch came to, beside level and conflict: merged
// committed the trunk onto the branch for the session to start on, and parked
// stopped the item on a conflict nobody here may hand to a session.
const (
	catchUpMerged = "merged"
	catchUpParked = "parked"
)

// dispatchCatchUp merges the trunk into the item's branch in its worktree
// before the round's session starts, so a branch a sibling's merge left behind
// is not implemented, reviewed and caught up only to conflict again after the
// review. A clean merge is committed and left unpushed: the session's own push
// carries it. A conflicted one is left in the worktree for the implementing
// session to resolve, at resolve_conflict = "auto", once per item per drive and
// only while max_review_rounds has a round to spend on it. Anywhere else the merge is
// aborted and the item parks trunk_conflict, the worktree kept for whoever the
// hand-off names.
//
// stop is true when the item parked, with its account in out. s.conflict is
// set when the round starts from a conflict.
func (s *session) dispatchCatchUp(d *DriveItem, park func(cause, why string) DriveItem) (out DriveItem, stop bool) {
	const key = "resolve_conflict"
	p, _ := s.cfg.Policy(key)
	if p == polOff || !s.cfg.Git.On() || s.drained("") != "" {
		return DriveItem{}, false
	}
	trunk := s.trunk()
	remote := "origin/" + trunk
	dir := s.workDir()
	ev := DriveEvent{Event: evStage, ID: d.ID, Stage: stageCatchUp, Branch: d.Branch, Tree: s.tree}
	report := func(result, why string, paths []string) {
		ev.Result, ev.Reason, ev.Paths = result, why, paths
		events.emit(ev)
	}
	release := s.holdTrunk()
	if err := gitAt(s.cfg.Root, "fetch", "--quiet", "origin", trunk); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s: %s not fetched, so the branch update reads the last one fetched: %v\n", d.ID, trunk, err)
	}
	to, err := revParse(s.cfg.Root, remote)
	release()
	if err != nil {
		return DriveItem{}, false
	}
	// Nothing below may write into a tree holding work nobody committed: a
	// merge refuses to start over an unconcluded one, and the abort after it
	// would discard the resolution in progress along with whatever the merge
	// touched. A level branch needs no merge, so a dirty one goes on as it did
	// before this catch-up existed; one mid-merge does not, since a session
	// started on it is told nothing about the merge.
	dirty, err := s.dirtyPaths()
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	merging := gitAt(dir, "rev-parse", "--quiet", "--verify", "MERGE_HEAD") == nil
	if merging {
		dirty = append(dirty, "MERGE_HEAD (a merge, unconcluded)")
	}
	switch level, err := isAncestor(dir, to, "HEAD"); {
	case err != nil:
		return park("branch_failed", err.Error()), true
	case level && !merging:
		report(catchUpLevel, "", nil)
		return DriveItem{}, false
	case len(dirty) > 0:
		s.stayDirty(d, dirty)
		d.Reason += "; " + remote + " was not merged in, because drive merges only into a clean worktree"
		report(catchUpParked, d.Reason, nil)
		return *d, true
	}
	fmt.Fprintf(os.Stderr, "  · %s: %s moved since %s last took it; merging it in before the session starts\n", d.ID, trunk, d.Branch)
	msg := "Merge " + remote + " into " + d.Branch
	err = gitAt(dir, "merge", "--no-edit", "-m", msg, to)
	if err == nil {
		report(catchUpMerged, "merged "+remote+" at "+shortSHA(to), nil)
		return DriveItem{}, false
	}
	paths, _ := gitNames(dir, "diff", "-z", "--name-only", "--diff-filter=U")
	if len(paths) == 0 {
		_ = gitAt(dir, "merge", "--abort")
		return park("branch_failed", "merging "+remote+" into "+d.Branch+" failed: "+err.Error()), true
	}
	conflict := "merging " + remote + " at " + shortSHA(to) + " into " + d.Branch + " conflicts on " + displayPaths(paths)
	hold := func(who config.Policy, why string) (DriveItem, bool) {
		if err := gitAt(dir, "merge", "--abort"); err != nil {
			why += "; the merge would not abort, so the worktree is mid-merge: " + err.Error()
		}
		report(catchUpParked, why, paths)
		d.Outcome, d.Park, d.Reason, d.handed = outcomeParked, parkTrunkConflict, why, true
		events.handOff(key, who, DriveEvent{ID: d.ID, Reason: why, PR: d.PR, Branch: d.Branch, Tree: s.tree, Paths: paths})
		return *d, true
	}
	limit := s.cfg.Review.MaxReviewRounds
	switch {
	case p != polAuto:
		return hold(p, conflict+"; the merge was aborted, and [supervise] "+key+" is "+string(p)+
			", so no session was started to resolve it")
	case limit > 0 && s.ledger.Counts(d.ID).ChangesRequested >= limit:
		return hold(polOperator, fmt.Sprintf("%s; the merge was aborted, because a conflict round counts against "+
			"[review] max_review_rounds and %s has used all %d", conflict, d.ID, limit))
	case s.slot != nil && !s.slot.sched.spendConflict(d.ID):
		return hold(polOperator, conflict+" a second time in this drive; the merge was aborted, and a person resolves this one")
	}
	// A round like a request-changes round, and counted as one: without the row a trunk
	// that keeps moving buys an item a new round in every drive.
	if err := s.record(ledger.Row{ID: d.ID, Changes: parkTrunkConflict + " " + shortSHA(to), Date: ledger.Today()}); err != nil {
		_ = gitAt(dir, "merge", "--abort")
		return park("ledger_unwritable", conflict+"; the merge was aborted, because the ledger would not count the round: "+err.Error()), true
	}
	s.conflict = s.conflictBrief(d.ID, remote, d.Branch, to, paths)
	report(catchUpConflict, conflict+"; the session resolves it", paths)
	return DriveItem{}, false
}

// conflictBrief is what the session resolving a dispatch conflict is told:
// the trunk commit merged into ours, and for each conflicted path the items
// whose trunk commits since the two's merge base touched it.
func (s *session) conflictBrief(self, remote, ours, to string, paths []string) *prompts.Conflict {
	c := &prompts.Conflict{Trunk: remote, Commit: to}
	base, err := mergeBase(s.cfg.Root, ours, to)
	if err != nil {
		for _, p := range paths {
			c.Paths = append(c.Paths, prompts.ConflictPath{Path: p})
		}
		return c
	}
	owners := s.trunkOwners(self, base, to)
	for _, p := range paths {
		cp := prompts.ConflictPath{Path: p}
		commits, _ := gitNames(s.cfg.Root, "log", "-z", "--format=%H", base+".."+to, "--", p)
		seen := map[string]bool{}
		for _, h := range commits {
			id := owners[strings.TrimSpace(h)]
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ci := prompts.ConflictItem{ID: id}
			if it, err := s.item(id); err != nil {
				ci.Unreadable = err.Error()
			} else {
				ci.Title, ci.Requirements = it.Title(), it.Requirements()
			}
			cp.Items = append(cp.Items, ci)
		}
		c.Paths = append(c.Paths, cp)
	}
	return c
}

// reviewConflict is the conflict a review of id's branch reads, rebuilt from
// the ledger and the branch rather than carried from dispatch: a lost review's
// rerun, a re-drive after a drain before the review, and a re-exec resuming at
// the review each reach a review no dispatch in that process saw conflict.
//
// The round is the latest implement row marked conflict that no review has
// concluded on since, its trunk commit the trunk_conflict changes row before
// it, and its paths what the branch's merge of that commit conflicts on when
// its two parents are merged again. nil when there is no such round, or its
// merge is not on the branch.
func (s *session) reviewConflict(id, branch string) *prompts.Conflict {
	var short, pending string
	for _, r := range s.ledger.Rows {
		if r.ID != id {
			continue
		}
		switch {
		case r.Stage == ledger.StageImplement && r.Conflict:
			pending = short
		case strings.HasPrefix(r.Changes, parkTrunkConflict+" "):
			short, pending = strings.TrimPrefix(r.Changes, parkTrunkConflict+" "), ""
		case r.Level != "" || r.Pass == passPassed || r.Pass == passChangesRequested:
			short, pending = "", ""
		}
	}
	if pending == "" {
		return nil
	}
	root := s.cfg.Root
	to, err := revParse(root, pending)
	if err != nil {
		return nil
	}
	out, err := gitcmd.Command(root, "rev-list", "--parents", "--merges", to+".."+branch).Output()
	if err != nil {
		return nil
	}
	var ours string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 2 && slices.Contains(f[2:], to) {
			ours = f[1]
			break
		}
	}
	if ours == "" {
		return nil
	}
	paths, err := mergeConflicts(root, ours, to)
	if err != nil {
		paths = bothChanged(root, ours, to)
	}
	return s.conflictBrief(id, "origin/"+s.trunk(), ours, to, paths)
}

// bothChanged is the files a and b each changed since their merge base: every
// file a merge of the two could have conflicted on, for a git whose merge-tree
// cannot name the ones that did.
func bothChanged(root, a, b string) []string {
	base, err := mergeBase(root, a, b)
	if err != nil {
		return nil
	}
	ours, _ := gitNames(root, "diff", "-z", "--name-only", base, a)
	theirs, _ := gitNames(root, "diff", "-z", "--name-only", base, b)
	var both []string
	for _, p := range ours {
		if slices.Contains(theirs, p) {
			both = append(both, p)
		}
	}
	return both
}

// trunkOwners maps each trunk commit in base..to to the done item whose work
// it is, read from the ledger. A merge that keeps the branch's commits leaves
// a head row of the item's on the trunk, and the commits on its first-parent
// line are the item's own; a squash leaves one commit whose subject carries
// the item's id or its pull request number, which is how drive titles both.
func (s *session) trunkOwners(self, base, to string) map[string]string {
	owners := map[string]string{}
	out, err := gitcmd.Command(s.cfg.Root, "log", "--format=%H %s", base+".."+to).Output()
	if err != nil {
		return owners
	}
	subjects := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if h, subject, ok := strings.Cut(line, " "); ok {
			subjects[h] = subject
		}
	}
	heads := map[string][]string{}
	for _, r := range s.ledger.Rows {
		if r.Head != "" && r.Branch != "" {
			heads[r.ID] = append(heads[r.ID], r.Head)
		}
	}
	for _, id := range s.ledger.IDs() {
		if id == self || !s.ledger.State(id).Merged {
			continue
		}
		for _, h := range heads[id] {
			if _, onTrunk := subjects[h]; !onTrunk {
				continue
			}
			own, err := gitcmd.Command(s.cfg.Root, "rev-list", "--first-parent", "--no-merges", base+".."+h).Output()
			if err != nil {
				continue
			}
			for _, c := range strings.Fields(string(own)) {
				if owners[c] == "" {
					owners[c] = id
				}
			}
		}
		pr, _ := s.ledger.PR(id)
		for h, subject := range subjects {
			if owners[h] == "" && (strings.HasPrefix(subject, id+":") || (pr > 0 && strings.Contains(subject, fmt.Sprintf("(#%d)", pr)))) {
				owners[h] = id
			}
		}
	}
	return owners
}

// caughtUp is what catchUp did. row is nil when nothing merged; out is the
// verify's output on the merged tree, kept for request-changes.
type caughtUp struct {
	row *CatchUp
	out string
}

// trunkResult is the gate's word on a trunk commit alone.
type trunkResult struct {
	passed bool
	out    string
}

// verifyCatchUp runs the gate the item's own run was told to pass on the tree
// the catch-up merged, and when it fails, runs it on the trunk alone to learn
// whose failure it is. A non-empty park means the merge must not be pushed.
//
// [numbered_files] is judged first, on the merged tree against the trunk commit just
// merged: git merges two files numbered 000112 without a conflict, and a
// migration tool that only moves forward skips one numbered below the trunk's
// newest without an error, so no gate is sure to notice either. before is the
// branch head the merge was made on, which is where the branch's own files end.
func (s *session) verifyCatchUp(ctx context.Context, it backlog.Item, d *DriveItem, trunk, before string, c *CatchUp) (result, out, park, reason string) {
	reports, err := checkSequences(s.workDir(), s.cfg.Sequences, "HEAD", c.From, before, c.To)
	switch {
	case err != nil:
		return catchUpUnrunnable, "", "gate_unrunnable", "the [numbered_files] check would not read the updated tree: " + err.Error()
	case sequenceFindings(reports) != "":
		return catchUpSequence, sequenceFindings(reports), "", ""
	case trunkClashText(reports) != "":
		return catchUpTrunkRed, "", parkTrunkRed, fmt.Sprintf(
			"%s at %s holds two files sharing a number, so no branch can fix it; the merge was undone and nothing was pushed:\n%s",
			trunk, shortSHA(c.To), trunkClashText(reports))
	}
	gate := prompts.SelectVerify(s.cfg.Verify, it.Meta)
	if strings.TrimSpace(gate) == "" {
		fmt.Fprintf(os.Stderr, "  · %s: the updated head of %s was merged with %s and no verify command exists to verify it\n",
			d.ID, d.Branch, trunk)
		return catchUpSkipped, "", "", ""
	}
	fmt.Fprintf(os.Stderr, "  · %s: running its verify command on %s merged with %s (%s..%s): %s\n",
		d.ID, d.Branch, trunk, shortSHA(c.From), shortSHA(c.To), gate)
	code, out, err := s.runItemVerify(ctx, d.ID, s.workDir(), gate)
	interrupted := "interrupted while the verify command ran on " + d.Branch + " merged with " + trunk + "; the merge was undone and nothing was pushed"
	switch {
	case ctx.Err() != nil:
		return catchUpUnrunnable, out, "interrupted", interrupted
	case err != nil:
		return catchUpUnrunnable, out, "gate_unrunnable", "the item's verify command would not start on the updated tree: " + err.Error()
	case code == 0:
		return catchUpPassed, out, "", ""
	}
	passed, trunkOut, err := s.trunkVerify(ctx, d.ID, gate, trunk, c.To)
	switch {
	case ctx.Err() != nil:
		return catchUpUnrunnable, out, "interrupted", interrupted
	case err != nil:
		return catchUpUnrunnable, out, "gate_unrunnable",
			"the verify command failed on the updated tree and " + trunk + " alone could not be checked to say whose failure it is: " + err.Error()
	case !passed:
		return catchUpTrunkRed, out, parkTrunkRed, fmt.Sprintf(
			"%s at %s fails the item's verify command on its own, so no branch can fix it; the merge was undone and nothing was pushed:\n%s",
			trunk, shortSHA(c.To), lastLines(trunkOut, verifyTail))
	}
	return catchUpFailed, out, "", ""
}

// trunkVerify runs gate on the trunk commit sha alone, in a worktree of its own
// prepared as an item's is, and remembers the answer for the rest of the drive.
// The worktree goes whatever the gate said.
func (s *session) trunkVerify(ctx context.Context, id, gate, trunk, sha string) (passed bool, out string, err error) {
	key := sha + "\x00" + gate
	if v, ok := s.trunkResults[key]; ok {
		said := "failed"
		if v.passed {
			said = "passed"
		}
		fmt.Fprintf(os.Stderr, "  · %s: %s@%s already %s this verify command on its own in this drive\n", id, trunk, shortSHA(sha), said)
		return v.passed, v.out, nil
	}
	tmp, err := os.MkdirTemp("", "drover-trunk-*")
	if err != nil {
		return false, "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	tree := filepath.Join(tmp, "tree")
	if err := gitAt(s.cfg.Root, "worktree", "add", "--quiet", "--detach", tree, sha); err != nil {
		return false, "", fmt.Errorf("git worktree add: %w", err)
	}
	defer func() { _ = gitAt(s.cfg.Root, "worktree", "remove", "--force", tree) }()
	if err := s.linkContext(tree); err != nil {
		return false, "", err
	}
	if len(s.cfg.Loop.TreeSetup) > 0 {
		if err := s.setupTree(tree); err != nil {
			return false, "", err
		}
	}
	fmt.Fprintf(os.Stderr, "  · %s: running the same verify command on %s@%s alone, to see whose failure it is\n", id, trunk, shortSHA(sha))
	code, out, err := s.runItemVerify(ctx, id, tree, gate)
	if err != nil || ctx.Err() != nil {
		return false, out, err
	}
	if s.trunkResults == nil {
		s.trunkResults = map[string]trunkResult{}
	}
	s.trunkResults[key] = trunkResult{passed: code == 0, out: out}
	return code == 0, out, nil
}

// runItemVerify runs a verify command as a session runs it from the prompt: a
// shell line, in dir, with DROVER_ROOT naming the project. err is a gate that
// never started; any exit status, including a signal's, is code.
func (s *session) runItemVerify(ctx context.Context, id, dir, gate string) (code int, out string, err error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", gate) // #nosec G204 -- the gate is project config
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), runner.EnvRoot+"="+s.cfg.Root)
	// A gate's children can hold its output open after an interrupt kills the
	// shell, and the drive would wait on them.
	cmd.WaitDelay = 10 * time.Second
	start := time.Now()
	b, runErr := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		code = ee.ExitCode()
	default:
		code, err = -1, runErr
	}
	took := time.Since(start)
	// The first run waiting wins: a red gate on the caught-up tree is followed
	// by the same gate on the trunk alone, and the item's own run is the one a
	// trend of this project's gate times is about.
	if err == nil && ctx.Err() == nil && s.verifyID != id {
		s.verifyID, s.verifyMS = id, max(took.Milliseconds(), 1)
	}
	fmt.Fprintf(os.Stderr, "  · %s: the verify command exited %d after %s\n", id, code, took.Round(time.Millisecond))
	return code, string(b), err
}

// mergeBase is the commit two refs last shared.
func mergeBase(dir, a, b string) (string, error) {
	out, err := gitcmd.Command(dir, "merge-base", a, b).Output()
	if err != nil {
		return "", fmt.Errorf("git merge-base %s %s: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// routeCatchUp acts on what catchUp returned, for both of its callers, and
// reports whether the item stops here. An interrupt keeps the worktree as it
// stood: the gate was killed partway, and what it left is for the operator to
// read rather than for drive to judge clean.
func (s *session) routeCatchUp(it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem,
	c caughtUp, p, why, trunk string,
) (DriveItem, bool) {
	// A branch can catch up twice in one drive, in driveFinish and again in
	// mergeByGate. The second usually finds it level and merges nothing, which
	// must not erase the merge the first one did.
	if c.row != nil {
		d.CatchUp = c.row
	}
	switch {
	case p == "interrupted" && c.row != nil:
		d.Outcome, d.Park, d.Reason = outcomeParked, p, why
		return *d, true
	case p == parkTrunkRed:
		d.stop = errTrunkRed
		return park(p, why), true
	case p != "":
		return park(p, why), true
	case c.row != nil && c.row.Verify == catchUpFailed:
		return s.requestChangesCatchUp(it, d, park, c, trunk), true
	case c.row != nil && c.row.Verify == catchUpSequence:
		return s.requestChangesNumbered(it, d, park, trunk, c.row.To, c.out), true
	}
	return DriveItem{}, false
}

// requestChangesCatchUp sends a caught-up branch back with the gate's output, headed by
// what the trunk brought in. It counts against max_review_rounds the way a gate's exit
// 1 does: a round spent on a tree the branch and the trunk made together is
// still a round.
func (s *session) requestChangesCatchUp(it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem, c caughtUp, trunk string) DriveItem {
	if err := s.requestChangesFromVerify(it, s.catchUpHeader(trunk, c.row), c.out); err != nil {
		return park(parkGateRefused, "the verify command failed on "+d.Branch+" merged with "+trunk+
			", which passes it alone, and the finding could not be written back: "+err.Error())
	}
	d.Rounds++
	d.Outcome, d.Park = outcomeParked, parkGateRefused
	d.Reason = "the verify command failed on " + d.Branch + " merged with " + trunk + "@" + shortSHA(c.row.To) +
		", which passes it alone; sent back"
	s.leaveItem(d)
	return *d
}

// catchUpCap is how many of the trunk's commits a catch-up request lists.
const catchUpCap = 20

// catchUpHeader is the request's opening: what the trunk brought in, and that
// the trunk alone passes, so the next run looks at the meeting of the two
// rather than at its own diff in isolation.
func (s *session) catchUpHeader(trunk string, c *CatchUp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "drover drive merged origin/%s (%s..%s) into this branch", trunk, shortSHA(c.From), shortSHA(c.To))
	lines, err := gitNames(s.cfg.Root, "log", "-z", "--format=%h %s", c.From+".."+c.To)
	switch {
	case err != nil:
		fmt.Fprintf(&b, " (the commits would not list: %v).\n", err)
	default:
		b.WriteString(", bringing in:\n\n")
		for i, l := range lines {
			if i == catchUpCap {
				fmt.Fprintf(&b, "- and %d more\n", len(lines)-catchUpCap)
				break
			}
			b.WriteString("- " + l + "\n")
		}
	}
	fmt.Fprintf(&b, "\n%s at %s passes the same verify command on its own, so the failure comes from the two meeting. "+
		"The verify command's output on the merged tree:", trunk, shortSHA(c.To))
	return b.String()
}

// isAncestor asks git whether a is an ancestor of b. Exit 1 is the answer no;
// anything else is git failing to answer.
func isAncestor(root, a, b string) (bool, error) {
	err := gitcmd.Command(root, "merge-base", "--is-ancestor", a, b).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("could not ask whether %s is in %s: %w", a, b, err)
}

func revParse(root, ref string) (string, error) {
	out, err := gitcmd.Command(root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("could not resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// verifyTail is how much of a refusing gate's output the park carries: enough for
// the check that failed and its reason, short enough for the summary line.
const verifyTail = 10

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// How often waitChecks asks and the most it waits. Variables so a test can
// drive the loop in milliseconds. How long a head may go without any check
// reporting is [forge] checks_wait.
var (
	checksPoll  = 20 * time.Second
	checksLimit = time.Hour
)

// waitChecks holds until every check run on sha has finished, so the gate reads
// final results rather than a pending one, and returns the runs it last read.
//
// It decides nothing. Whatever it sees, including a forge it cannot read, a
// check still running at the limit, or no checks at all, the caller decides
// next; this only keeps it from deciding early. The checks_wait window covers
// the push that has not registered its checks yet, which otherwise reads as a
// repository with none. A run in superseded is one drive reran, which reads as
// pending until the forge replaces it with the new attempt.
func (s *session) waitChecks(ctx context.Context, id, sha string, deadline time.Time, superseded map[int64]bool) ([]forgeCheck, error) {
	start := time.Now()
	limit := start.Add(checksLimit)
	if !deadline.IsZero() && deadline.Before(limit) {
		limit = deadline
	}
	for {
		runs, err := s.checkRuns(ctx, sha)
		pending := 0
		for _, r := range runs {
			if r.Status != "completed" || superseded[r.ID] {
				pending++
			}
		}
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "  · %s: could not read the checks on %s, so the merge gate reads them as they are: %v\n",
				id, shortSHA(sha), err)
			return nil, err
		case len(runs) > 0 && pending == 0:
			return runs, nil
		case len(runs) == 0 && time.Since(start) >= s.cfg.ForgeChecksWait():
			return runs, nil
		case !time.Now().Before(limit):
			fmt.Fprintf(os.Stderr, "  · %s: %d of %d checks on %s still running; the merge gate reads them as they are\n",
				id, pending, len(runs), shortSHA(sha))
			return runs, nil
		}
		if !sleepFor(ctx, checksPoll) {
			return runs, ctx.Err()
		}
	}
}

// checkRuns reads the check runs the forge holds for sha, the latest attempt
// of each.
func (s *session) checkRuns(ctx context.Context, sha string) ([]forgeCheck, error) {
	out, err := s.ghAPI(ctx, "repos/"+s.repo()+"/commits/"+sha+"/check-runs?per_page=100")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Total *int         `json:"total_count"`
		Runs  []forgeCheck `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Total == nil {
		return nil, fmt.Errorf("gh api answered %q, which is not a check-run listing", lastLines(strings.TrimSpace(string(out)), 1))
	}
	return doc.Runs, nil
}

// requestChangesFromVerify writes the gate's own output back to the branch, through request-changes,
// which stays the only writer of that section and the only place the cap is
// enforced. Each failed check follows the output with its run and what its log
// says failed, because a gate's output names a check and a commit, and the
// session that gets the item back would otherwise start by finding the log.
func (s *session) requestChangesFromVerify(it backlog.Item, header, out string, failed ...judged) error {
	f, err := os.CreateTemp("", "drover-verify-*.md")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	body := header + "\n\n```\n" + strings.TrimSpace(out) + "\n```\n"
	for _, j := range failed {
		body += "\n" + j.run.Name + " failed"
		if u := checkURL(j.run); u != "" {
			body += " (" + u + ")"
		}
		if len(j.failure) == 0 {
			body += ": " + j.evidence + "\n"
			continue
		}
		body += ". Its log:\n\n```\n" + strings.Join(j.failure, "\n") + "\n```\n"
	}
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	requestChangesFile = f.Name()
	defer func() { requestChangesFile = "" }()
	// request-changes reports on stdout, and stdout under --json carries one object per
	// line and nothing else.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = stdout }()
	return requestChangesCmd.RunE(requestChangesCmd, []string{it.ID.Bare()})
}

// gateAtHead runs [drive] merge_gate, and where it answers exit 4, that the head
// is only behind the trunk, catches the branch up once and asks again. A gate
// that sees the trunk move under a branch is reporting drive's own merges, not
// a fault in the item, and treating it as one parks the item behind its own
// supervisor's merge chain. res is the item's account
// when it ended here, and nil when the gate's answer stands.
//
// At any policy other than auto, exit 4 parks gate_refused at once, with the
// hand-off emitted for whoever owns it.
//
// checks is what the caller settled on the head the gate reads first. The
// catch-up moves the head, so checks is replaced by what settles on the new one
// before the gate is asked again: a red check there is classified as on any
// head the gate reads, rather than sent back to the item by the gate.
func (s *session) gateAtHead(ctx context.Context, it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem,
	trunk string, deadline time.Time, checks *settled,
) (code int, out string, res *DriveItem, err error) {
	events.stage(d.ID, stageGate)
	code, out, err = s.runGate(ctx, d.Branch, trunk)
	if err != nil || code != gateBehind {
		return code, out, nil, err
	}
	const key = "retry_moved_trunk"
	if p, _ := s.cfg.Policy(key); p != polAuto {
		why := fmt.Sprintf("[drive] merge_gate refused %s because %s moved under it (exit 4), and [supervise] %s is %s, "+
			"so drive did not retry; nothing was sent back:\n%s", d.Branch, trunk, key, p, lastLines(out, verifyTail))
		events.handOff(key, p, DriveEvent{ID: d.ID, Branch: d.Branch, PR: d.PR, Reason: why})
		r := park(parkGateRefused, why)
		return code, out, &r, nil
	}
	fmt.Fprintf(os.Stderr, "  · %s: [drive] merge_gate says %s is behind %s (exit 4); catching it up and asking once more\n",
		d.ID, d.Branch, trunk)
	events.stage(d.ID, stageCatchUp)
	c, p, why := s.catchUp(ctx, it, d, trunk)
	if r, stopped := s.routeCatchUp(it, d, park, c, p, why, trunk); stopped {
		return code, out, &r, nil
	}
	head, err := revParse(s.cfg.Root, d.Branch)
	if err != nil {
		r := park("branch_failed", err.Error())
		return code, out, &r, nil
	}
	if *checks = s.settleChecks(ctx, d, head, deadline); checks.park != "" {
		if checks.stop != nil {
			d.stop = checks.stop
		}
		r := park(checks.park, checks.reason)
		return code, out, &r, nil
	}
	events.stage(d.ID, stageGate)
	code, out, err = s.runGate(ctx, d.Branch, trunk)
	return code, out, nil, err
}

// behindAgain is the park reason for a gate that still says the head is behind
// after the one catch-up a merge is allowed: the trunk is moving faster than
// this item can merge, which nobody fixes by sending the work back.
func behindAgain(branch, trunk, out string) string {
	return fmt.Sprintf("[drive] merge_gate refused %s because %s moved under it (exit 4), and again after a branch update; "+
		"nothing was sent back:\n%s", branch, trunk, lastLines(out, verifyTail))
}

// What a catch-up of another open pull request came to, beside the CatchUp gate
// words: up_to_date merged nothing because the branch already held the trunk,
// conflict would not merge, and kept was not attempted for the reason given.
const (
	catchUpLevel    = "up_to_date"
	catchUpConflict = "conflict"
	catchUpLeft     = "kept"
)

// openPR is an item whose branch drive pushed and whose pull request is still
// open at the forge: waiting on its checks or its queued merge, or on the
// operator for a denied path.
type openPR struct {
	it     backlog.Item
	branch string
	pr     int
}

// afterMerge catches up every other open drive pull request once a merge has
// moved the trunk, as [supervise] update_open_prs says. Each one left behind
// goes stale or conflicting at the forge with nothing in drive to bring it back,
// because drive catches a branch up only on that item's own turn.
func (s *session) afterMerge(ctx context.Context, d *DriveItem) {
	if err := s.catchUpAfter(ctx, d.ID); err != nil && d.stop == nil {
		d.stop = err
	}
}

// catchUpAfter is afterMerge under its policy. merged is the item whose merge
// moved the trunk, empty for reconciliation's.
func (s *session) catchUpAfter(ctx context.Context, merged string) error {
	const key = "update_open_prs"
	switch p, _ := s.cfg.Policy(key); p {
	case polAuto:
		return s.catchUpOpen(ctx, merged)
	case polSupervisor, polOperator:
		why := "the trunk moved, and every other open drive pull request is behind it"
		if merged != "" {
			why = merged + " merged, so the trunk moved under every other open drive pull request"
		}
		events.handOff(key, p, DriveEvent{ID: merged, Reason: why})
	}
	return nil
}

// catchUpOpen catches up every open drive pull request but skip's, one at a
// time, and returns the stop a trunk failing the gate on its own raises: no
// branch can pass a gate its trunk fails, so the rest would each find the same.
func (s *session) catchUpOpen(ctx context.Context, skip string) error {
	if !gh.Available() {
		fmt.Fprintln(os.Stderr, "  · the open pull requests were not updated: gh cannot be reached to say which are open")
		return nil
	}
	trunk := s.trunk()
	for _, o := range s.openPRs(skip) {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.catchUpPR(ctx, o, trunk); err != nil {
			return err
		}
	}
	return nil
}

// openPRs is every item but skip, not yet done, whose ledger records a pull
// request on the branch the item renders today and whose pull request the
// forge still holds open. The rendered branch has to match the recorded one: a
// project that retemplated [run] branch leaves rows naming branches drive no
// longer owns.
func (s *session) openPRs(skip string) []openPR {
	var out []openPR
	labels := s.labels()
	for _, it := range s.backlog.Items {
		id := it.ID.Bare()
		if id == skip || s.ledger.State(id).Merged {
			continue
		}
		n, on := s.ledger.PR(id)
		if n <= 0 {
			continue
		}
		branch, err := s.branchFor(it)
		if err != nil || branch != on {
			continue
		}
		open, ok, err := gh.OpenPR(labels, branch)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "  · %s: the forge would not say whether #%d is open, so it is not updated: %v\n", id, n, err)
			continue
		case !ok:
			continue
		}
		out = append(out, openPR{it: it, branch: branch, pr: open})
	}
	return out
}

// catchUpPR is one open pull request's catch-up: the trunk merged into its
// branch in the item's worktree, the item's verify command on the merged tree,
// and the push, by the same catchUp the item's own turn uses. It leaves the
// item where it was, still waiting on the forge or on the operator. A gate that
// fails sends the item back as its own catch-up would.
//
// It runs on a copy of the session with no slot, because the caller already
// holds the trunk stage and opening a worktree would wait on it.
func (s *session) catchUpPR(ctx context.Context, o openPR, trunk string) error {
	w := *s
	w.tree, w.slot = "", nil
	id := o.it.ID.Bare()
	ev := DriveEvent{Event: evStage, ID: id, Stage: stageCatchUp, Branch: o.branch, PR: o.pr}
	report := func(result, why string) {
		ev.Result, ev.Reason = result, why
		events.emit(ev)
		if why == "" {
			why = result
		}
		fmt.Fprintf(os.Stderr, "  · %s: updated #%d (%s): %s\n", id, o.pr, o.branch, why)
	}
	remote := "origin/" + trunk
	if why := w.branchAtOrigin(o.branch); why != "" {
		report(catchUpLeft, why)
		return nil
	}
	if gitAt(w.cfg.Root, "fetch", "--quiet", "origin", trunk) != nil || gitAt(w.cfg.Root, "rev-parse", "--verify", "--quiet", remote) != nil {
		report(catchUpLeft, remote+" would not fetch, so there is nothing to update to")
		return nil
	}
	if level, err := isAncestor(w.cfg.Root, remote, o.branch); err != nil {
		report(catchUpLeft, err.Error())
		return nil
	} else if level {
		report(catchUpLevel, "")
		return nil
	}
	tree, err := w.openItemTree(o.it, o.branch, trunk)
	defer tree.close()
	if err != nil {
		report(catchUpLeft, "its worktree would not open, so nothing was merged: "+err.Error())
		return nil
	}
	w.tree = tree.Path
	ev.Tree = tree.Path
	d := DriveItem{ID: id, Title: o.it.Title(), Branch: o.branch, PR: o.pr, Tree: tree.Path}
	switch dirty, err := w.dirtyPaths(); {
	case err != nil:
		report(catchUpLeft, err.Error())
		return nil
	case len(dirty) > 0:
		report(catchUpLeft, "its worktree holds uncommitted work ("+pathList(dirty)+"), so nothing was merged into it")
		return nil
	}

	if paths, _ := mergeConflicts(w.cfg.Root, remote, o.branch); len(paths) > 0 {
		w.conflicted(&d, ev, paths, "merging "+remote+" into "+o.branch+" conflicts on "+displayPaths(paths)+
			"; nothing was merged or pushed")
		return nil
	}
	c, park, why := w.catchUp(ctx, o.it, &d, trunk)
	switch {
	case park == parkTrunkConflict:
		w.conflicted(&d, ev, nil, why)
		return nil
	case park == parkTrunkRed:
		report(catchUpTrunkRed, why)
		w.leaveItem(&d)
		return errTrunkRed
	case park != "":
		result := catchUpLeft
		if c.row != nil {
			result = c.row.Verify
		}
		report(result, park+": "+why)
		if park != "interrupted" {
			w.leaveItem(&d)
		}
		return nil
	case c.row == nil:
		report(catchUpLevel, "")
	case c.row.Verify == catchUpFailed:
		why := "the verify command failed on the merged tree, which " + trunk + " passes alone; pushed and sent back"
		if err := w.requestChangesFromVerify(o.it, w.catchUpHeader(trunk, c.row), c.out); err != nil {
			why = "the verify command failed on the merged tree, which " + trunk + " passes alone; pushed, and the finding could not be written back: " + err.Error()
		}
		report(catchUpFailed, why)
	case c.row.Verify == catchUpSequence:
		why := "numbered files clash with or sort below " + trunk + "; the merge was undone and the item sent back"
		if err := w.requestChangesFromVerify(o.it, sequenceHeader(trunk, c.row.To), c.out); err != nil {
			why = "numbered files clash with or sort below " + trunk + "; the merge was undone, and the finding could not be written back: " + err.Error()
		}
		report(catchUpSequence, why)
	default:
		report(c.row.Verify, "merged "+remote+" ("+shortSHA(c.row.From)+".."+shortSHA(c.row.To)+"), verify "+c.row.Verify+", pushed")
	}
	w.leaveItem(&d)
	return nil
}

// conflicted reports a catch-up that would not merge and hands it to whoever
// [supervise] resolve_conflict names. At supervisor or operator the worktree
// stays for them to resolve in; at off the item parks trunk_conflict as its own
// turn would, and a clean tree goes; at auto the clean tree goes and nothing
// parks, for the item's next dispatch to resolve. paths is nil when only the merge itself
// found the conflict, and why then carries the files.
func (s *session) conflicted(d *DriveItem, ev DriveEvent, paths []string, why string) {
	ev.Result, ev.Reason, ev.Paths = catchUpConflict, why, paths
	events.emit(ev)
	fmt.Fprintf(os.Stderr, "  · %s: updated #%d (%s): %s\n", d.ID, d.PR, d.Branch, why)

	const key = "resolve_conflict"
	p, _ := s.cfg.Policy(key)
	// Nothing parks at auto. This runs inside the trunk stage, where no session
	// can be launched, so the conflict waits for the item's next dispatch,
	// whose catch-up takes it to one.
	if p == polAuto {
		s.leaveItem(d)
		return
	}
	events.emit(DriveEvent{Event: evParked, ID: d.ID, Park: parkTrunkConflict, Reason: why, PR: d.PR,
		Action: key, Policy: string(p)})
	if p == polSupervisor || p == polOperator {
		events.handOff(key, p, DriveEvent{ID: d.ID, Reason: why, PR: d.PR, Branch: d.Branch, Tree: d.Tree, Paths: paths})
		return
	}
	s.leaveItem(d)
}

// branchAtOrigin readies a local branch for a catch-up: fetched, made from
// origin's where this checkout lacks it, and refused where the two disagree. A
// local branch ahead of origin's holds commits nobody pushed, which a catch-up
// would publish; one behind it holds a push this checkout never saw, which the
// catch-up's own push would be refused over.
func (s *session) branchAtOrigin(branch string) string {
	remote := "origin/" + branch
	if err := gitAt(s.cfg.Root, "fetch", "--quiet", "origin", "+"+branch+":refs/remotes/"+remote); err != nil {
		return remote + " would not fetch: " + err.Error()
	}
	if gitAt(s.cfg.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) != nil {
		if err := gitAt(s.cfg.Root, "branch", "--quiet", "--no-track", branch, remote); err != nil {
			return "no local " + branch + ", and one would not be made from " + remote + ": " + err.Error()
		}
		return ""
	}
	local, err := revParse(s.cfg.Root, "refs/heads/"+branch)
	if err != nil {
		return err.Error()
	}
	theirs, err := revParse(s.cfg.Root, remote)
	if err != nil {
		return err.Error()
	}
	if local != theirs {
		return "the local " + branch + " is at " + shortSHA(local) + " and " + remote + " at " + shortSHA(theirs) +
			"; a branch update pushes only a branch the two agree on"
	}
	return ""
}

// mergeConflicts is the files merging a into b would conflict on, read with git
// merge-tree so nothing is checked out or undone to learn it. A git too old for
// --write-tree is an error, and the merge in the worktree finds the conflict
// instead.
func mergeConflicts(root, a, b string) ([]string, error) {
	out, err := gitcmd.Command(root, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", a, b).Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil, nil
	case !errors.As(err, &ee) || ee.ExitCode() != 1:
		return nil, fmt.Errorf("git merge-tree %s %s: %w", a, b, err)
	}
	var paths []string
	for i, f := range strings.Split(string(out), "\x00") {
		if i > 0 && f != "" {
			paths = append(paths, f)
		}
	}
	return paths, nil
}

// catchUpByHand is --update-branches: every open drive pull request caught up as a
// merge drive made would catch it up, for a merge made somewhere else. The
// operator asked for it by name, so [supervise] update_open_prs does not
// decide it.
func (s *session) catchUpByHand(ctx context.Context) error {
	if !s.cfg.Git.On() {
		return Refusal{fmt.Errorf("[git] enabled is false, so this project has no branches to update")}
	}
	if err := gitAt(s.cfg.Root, "remote", "get-url", "origin"); err != nil {
		return Refusal{fmt.Errorf("a branch update pushes to origin, and this tree has none: %s", err)}
	}
	if !gh.Available() {
		return Refusal{fmt.Errorf("which pull requests are open is the forge's to say, and gh cannot be reached.\n" +
			"Install gh and run `gh auth login`")}
	}
	return s.catchUpOpen(ctx, "")
}
