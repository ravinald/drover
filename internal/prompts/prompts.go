// Package prompts assembles what an agent is handed.
//
// The prose lives in a project-owned template, not in Go. Every rule about what
// "done" means — the verify command, what a bug owes over a feature, how wide a
// diff may go — is a property of the project rather than of the tool, and the
// shell driver this replaces had all of it compiled in.
package prompts

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/ravinald/drover/internal/backlog"
)

//go:embed default.tmpl
var defaultTemplate string

//go:embed review.tmpl
var reviewTemplate string

//go:embed draft.tmpl
var draftTemplate string

// DefaultTemplate is the built-in implementing prompt, used when a project names none.
func DefaultTemplate() string { return defaultTemplate }

// ReviewTemplate is the built-in review brief.
func ReviewTemplate() string { return reviewTemplate }

// DraftTemplate is the built-in drafting brief.
func DraftTemplate() string { return draftTemplate }

// Dep is one dependency, resolved so the template can say more than its id.
type Dep struct {
	ID    string
	Title string
	Done  bool
	// Unreadable is why drover could not read this dep's body, empty when it
	// read it: the three states check reports as MISSING DEP, plus an id that does
	// not parse and so names nothing to look for. It is prose rather than a
	// token because the alternative is every template reinventing the sentence.
	//
	// Independent of Done. "Merged" is the ledger's answer and "readable" is the
	// tree's, and a dep recorded done whose body is gone is both.
	Unreadable string
	// Level, By and Independent are its audit provenance: the level, who
	// wrote the code, and whether the auditor differed from every one of them.
	// Weak marks a foundation nobody has independently confirmed: unaudited,
	// claimed, audited by its own author, or audited before authorship was
	// recorded at all. A reviewer treating such a dependency as given is
	// resting on an unverified claim.
	Level       string
	By          string
	Independent bool
	Weak        bool
	// Report is the path to that dep's saved self-report, empty when none
	// exists. A register row records what an item left open; the saved report records what
	// it closed by inspection, and a later run that cannot see it re-litigates
	// the finding or regresses the invariant it rested on.
	Report string
}

// Skill is a guide the item asks the agent to read.
type Skill struct {
	Name string
	// Path is where the guide is vendored, empty when it is not.
	Path string
}

// Origin is the tracker issue an item was scaffolded from, read fresh rather
// than from the copy the scaffold quoted: an issue gains detail after intake,
// and drafting from the stale copy is drafting from what was known that day.
//
// Unavailable says why it could not be read, so a brief assembled without the
// network reports the gap instead of implying the issue was empty.
type Origin struct {
	Number      int
	Title       string
	Body        string
	Unavailable string
}

// Finding is the item a scaffold's provenance line names, with the audit notes
// the ledger recorded against it, newest first. A reviewer filing with
// `drover new --found-in` describes the defect in the note on the item under
// review, so for a scaffold written before `new -f` existed that note is the
// only description there is.
//
// From is empty for a body with no provenance line. Notes is empty when the
// named item has none, which the brief states rather than omitting.
type Finding struct {
	From  string
	Notes []AuditNote
}

// AuditNote is one audit row's note, with the level and authorship it was
// recorded under.
type AuditNote struct {
	Date, Level, By, Note string
}

// Sibling is another item in the backlog. A drafting agent needs the list to
// route a requirement that belongs elsewhere, which it cannot do while the
// only item it can see is the one it was pointed at.
type Sibling struct {
	ID    string
	Title string
	Path  string
	Done  bool
}

// Issue is one open register row routed to this work.
//
// Owners are the ids the row's route labels name, and Mine says whether this
// item is one of them. A brief carries rows routed to the item and rows routed
// to its dependencies in the same list, and only the first kind is this
// branch's to answer for by default: a template that cannot tell them apart
// either asks a reviewer to discharge somebody else's gap or asks nothing at
// all.
type Issue struct {
	Number   int
	Title    string
	Severity string
	Owners   []string
	Mine     bool
	// Body is the report that filed the row: the file, the line, the command
	// that reproduces it. Empty when the row carries none, and a template
	// renders the headline alone there — a row that says nothing more is a
	// different answer from one whose text drover could not reach.
	//
	// Unavailable is why this row's body could not be read, empty when it was
	// read. Every row from a file register carries it, because that backend has
	// no column for a body: a template rendering neither field would print a
	// title and imply whoever filed it wrote nothing under it.
	Body        string
	Unavailable string
	// Labels is every label the tracker carries on the issue. Set on Owned
	// entries only: a register row's severity and owners are already its labels
	// read, and a routed issue has nothing else that says what kind of report it is.
	Labels []string
}

// Resolution is one issue the item's `closes` key claims to resolve, read fresh
// from the tracker rather than from anything the body quoted.
//
// State is the issue's own, so a reviewer can see one somebody already closed
// by hand. Disposition is what a prior pass recorded, empty when nobody has
// judged it yet, and Applied names what finish has already written to the tracker
// — a reviewer who cannot see that re-litigates a comment that is already
// public.
//
// Unavailable says why the issue could not be read. Without it a tracker nobody
// could reach renders as an issue with no title, which reads as an empty report
// rather than a question drover could not ask.
type Resolution struct {
	Number      int
	Title       string
	Body        string
	State       string
	Disposition string
	By          string
	Note        string
	Applied     string
	Unavailable string
	// Settled says two independent passes agreed and nothing since disagreed.
	// The brief hands such an answer over as fact rather than asking for it
	// again: re-judging a settled issue is where a correct answer gets
	// overwritten and a later pass spends its turn putting it back.
	Settled bool
	// Amended is the date an amend replaced the requirements after this
	// disposition was recorded. The answer rested on a contract that changed,
	// so the brief asks for it again rather than handing it over.
	Amended string
}

// Undisposed reports an issue no pass has judged yet. It is the reviewer's
// actual worklist, and the template asks for it by name so the list cannot
// drift from what finish will refuse to act on.
func (r Resolution) Undisposed() bool { return r.Disposition == "" }

// ChangesHeading is the section of an item body that carries what a review sent
// back to the branch.
//
// One copy: review.tmpl tells the reviewer to write under it and `drover request-changes`
// is the only thing that writes there. Two literals would drift the moment
// either was reworded, and the failure is silent: a sent-back section written
// under a heading the next brief does not name reaches nobody.
const ChangesHeading = "## What the review sent back"

// ChangesHeading exposes the constant to a template.
func (d Data) ChangesHeading() string { return ChangesHeading }

// GuaranteeHeading is the section of an item body that carries what this code
// must keep true, whatever else changes.
//
// The third section of a body, and the only one that accumulates. The numbered
// requirements are frozen once written — `request-changes` refuses a write that would
// change their count — and the sent-back section is replaced every pass, so
// without this one a property learned on round three would have nowhere to
// live and would be thrown away with the findings that taught it. A run then meets the same guarantee as a fresh
// surprise and trades one violation of it for another.
const GuaranteeHeading = "## Invariants"

// GuaranteeHeading exposes the constant to a template.
func (d Data) GuaranteeHeading() string { return GuaranteeHeading }

// Guarantees is what every pass so far established this code must keep true,
// the heading line excluded. Empty when nobody has recorded one.
func (d Data) Guarantees() string { return section(d.Body, GuaranteeHeading) }

// Changes is what the last review sent back: the ChangesHeading section of the
// body, the heading line excluded. Empty when the body carries no such section.
//
// The reader for what `drover request-changes` writes. Nothing else can show a reviewer
// the previous pass's evidence: the ledger's changes row is a digest, and the
// section is replaced rather than appended, so the text of round N leaves the
// tree when N+1 lands. A brief that asks which earlier findings still hold
// without rendering this is asking against a body it never showed.
//
// The section ends at the next "## " heading, the same boundary `request-changes` writes
// to.
func (d Data) Changes() string { return section(d.Body, ChangesHeading) }

// ClearedMark opens the statement `drover close-changes` writes in place of a block
// whose findings a review found closed.
//
// Here for ChangesHeading's reason, one section further in: both briefs read it
// to tell a discharged block from findings a run owes something about, and
// `close-changes` writes it. Two literals would drift the moment either was reworded,
// and the failure is silent in the direction that matters — a brief that cannot
// recognize a cleared block renders it as current findings, which is the state
// clearing exists to end.
const ClearedMark = "Nothing is outstanding."

// Cleared reports a block a review discharged rather than one it wrote.
func (d Data) Cleared() bool { return strings.HasPrefix(d.Changes(), ClearedMark) }

// section is the text under one "## " heading of an item body, ending at the
// next one. Empty when the body carries no such heading, which is how a brief
// tells a section nobody has written from one somebody emptied.
func section(body, heading string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

// Round is one sent-back section an earlier pass wrote, kept because the section it was
// written into is replaced every time. Headline is its first line of evidence
// and Path where the whole of it still is.
//
// A brief renders the headline and the path, never the body: a closed finding
// rendered whole reads as an open one, and re-verifying finished work is the
// failure the replaced section exists to prevent. What the list carries is the
// progression, which is the evidence that five rounds have been circling one
// unstated guarantee rather than closing five defects.
type Round struct {
	Number   int
	Date     string
	Headline string
	Path     string
}

// Position is one lifecycle step and the steps a copy of the lifecycle has to
// place it between, each named as the bare command. A copy states the order by
// listing commands and most of them list only some, so the value is a set of
// relative placements rather than a sequence: whichever steps a copy names, it
// has to name them in this order.
type Position struct {
	Step   string
	After  []string
	Before []string
}

// AuditPosition is where the audit row lands, as the one value every copy of
// that claim is pinned against. Copies stated by hand drift apart: one draws
// `audit` after the merge while another asks for the row and then says not to
// merge.
//
// Before the merge wins. The session that has just traced every requirement is
// the one holding what the row has to say, and the independence a level needs
// comes from `--code-by` refusing `verified` from an author whenever it is
// written, not from waiting until the branch is gone. A later row is still
// allowed and the most recent level wins, which is what makes the UNAUDITED
// state `drover unaudited` reports correctable at all.
//
// internal/cmd/doctrine_test.go pins every copy against this.
func AuditPosition() Position {
	return Position{Step: "audit", After: []string{"review"}, Before: []string{"merge", "finish"}}
}

// Destination is one place a review finding goes.
//
// Token is a stable handle: a report names it, and the tests that pin every
// copy of this doctrine match on it rather than on a sentence somebody
// reworded. Where is the place, Found is the discriminator, and Do is why that
// is the place.
type Destination struct {
	Token string
	Where string
	Found string
	Do    string
}

// Destinations is the routing doctrine, and the only copy of its prose in the
// tree. Two templates render it and two documents restate it in their own
// register; internal/cmd/doctrine_test.go pins all of them against this, and a
// third document links to one of those rather than restating anything.
//
// Two axes, applied in that order. The contract and the branch's own diff say
// where a finding goes: a run answers for what it changed, so a regression no
// requirement names still has somewhere to go, and a pre-existing defect
// standing between the run and a requirement cannot be somebody else's problem
// when the contract cannot be discharged without touching it. What a defect
// outside the diff would break in use then says whether it is worth a run at
// all; one inside it is unfinished work at any size. Provenance
// alone answers who caused a defect, which decides attribution and not whether
// a pull request should stay open.
//
// The severity test lives inside the Found prose of the rows it narrows rather
// than in a field of its own. Found is the discriminator a reviewer matches on,
// and a list whose rows are separated only by their order resolves a finding to
// whichever row it reaches first, so every row that can be beaten names the row
// that beats it in its own text. A Severity field would also stand blank on
// most rows, which doctrine_test.go reads as a defect, and only Found is pinned
// into both rendered templates.
func Destinations() []Destination { return slices.Clone(destinations) }

var destinations = []Destination{{
	Token: "BRANCH-REQUIREMENT",
	Where: "back to the branch",
	Found: "A numbered requirement of this item, unmet.",
	Do:    "Do not file it. Severity does not enter here: the numbered list is the contract, and an unmet requirement is unmet at any size. Nothing has merged and no pull request exists yet, so the branch is still open and the fix belongs on it: report which requirement failed and how you know. Where discharging it would change a contract this list never named, the implementation goes to `SPLIT-OUT` and the requirement stays unmet here. The operator re-runs the item, the run returns to the same branch, and this review runs again. Filing an unmet requirement as an issue merges the item incomplete and leaves a report whose only owner just shipped.",
}, {
	Token: "BRANCH-REGRESSION",
	Where: "back to the branch",
	Found: "Any defect this branch's own diff introduced, whatever it would break in use: a wrong answer, lost data, a silent failure, a command that stops working, or something smaller. One that is a typo, a stale comment or a dead link is `FIX-IN-REVIEW` by that row's own definition.",
	Do:    "`git diff <trunk>...HEAD` is the boundary, not the numbered list, and both halves of that have to hold. An unmerged item has no bugs: a defect in its own work is unfinished implementation, so a regression nobody wrote a requirement for goes back the same way an unmet requirement does, at any size and on any pass. It is never filed, because an issue or a new item for it lets the branch merge carrying a defect its own run wrote. When the fix for it changes a contract no requirement names, the regression is still yours and the implementation is `SPLIT-OUT`. The report is where a real one evaporates and another item is where it lands on somebody who did not cause it.",
}, {
	Token: "BRANCH-BLOCKER",
	Where: "back to the branch",
	Found: "A pre-existing defect that blocks a numbered requirement.",
	Do:    "Outside the diff, and this run's problem anyway: the contract cannot be discharged without touching it. It is fixed here, and your report says why the diff is wider than the requirements name. A blocker whose fix is its own architectural task is `SPLIT-OUT` instead, which is the difference between a wider diff and a different contract. Severity does not enter here either, for the same reason it does not enter the first row.",
}, {
	Token: "SPLIT-OUT",
	Where: "a new item, with this one blocked on it",
	Found: "A finding you have already sent back to the branch whose fix changes a contract this item's numbered requirements never named: another subsystem, every caller of an interface, a storage or wire format.",
	Do:    "`drover split <id> --into <kind> \"<title>\" -f <proposal>` scaffolds the work as its own item and adds it to this item's deps, which leaves this one blocked and unmerged until the operator accepts that item and it is finished. The finding stays where it was sent and so does the blame: a regression this branch introduced is still this branch's to answer for, and what moves is the implementation. Read the bar before reaching for this, because it is the row a fourth round is tempted by: a wider diff is not a different contract, a fix touching three files because the requirement reaches three files is still this item, and work you could describe as one more requirement on this list belongs on this list. What qualifies is work somebody would have written its own numbered requirements for had they seen it coming.",
}, {
	Token: "FIX-IN-REVIEW",
	Where: "fixed in this pass, with nothing sent back",
	Found: "A defect no numbered requirement traces to, that no verify command can observe, and that you can correct in the file you found it in: a typo, a stale comment, two sentences of prose that contradict each other, a dead link.",
	Do:    "Fix it and move on. Trace every numbered requirement first, so the files a requirement reaches are written down before you edit anything: a fix made before the trace is one you cannot afterwards show was outside it. Confirm `git rev-parse --abbrev-ref HEAD` is this item's branch, because review never switches branches and a session standing anywhere else would commit onto the trunk; if it disagrees, fix nothing and report the branch instead. Then correct it, re-run the checks that cover what you changed after rather than before, and commit it alone with a subject opening `review:` so the diff you wrote can be told from the run's. Name every one of them in the audit note. Touch anything a requirement traces to and you wrote the code you are auditing: the honest row names you in `--code-by`, which cannot record `verified`, and the item loses the independent certification this review exists to produce.",
}, {
	Token: "FOLLOW-UP-TO-OWNER",
	Where: "filed against its owner",
	Found: "A pre-existing defect outside this branch's diff that blocks no requirement here, in code an item that has merged owns.",
	Do:    "File it against the merged item that owns the gap, not the one that noticed it, and name it in your report. It does not get fixed here. An unmerged item owns nothing filed: its gaps go back to its own branch when it is reviewed.",
}, {
	Token: "NEW-ITEM",
	Where: "filed as a new item",
	Found: "A gap in merged code that no existing item owns and that blocks no requirement here. Never a defect this branch's own diff introduced: that is `BRANCH-REGRESSION`.",
	Do:    "`drover new <kind> \"<title>\" -f <file>` writes it, without `--found-in`: the gap was in the trunk before this branch existed, and this item is not where it came from. Name the new id in your report and in the audit note. The file is the finding for whoever writes the requirements, who was not here: the file and line, the command that reproduces it with its actual output, and what is wrong. The audit note names the new id rather than carrying the description, because nobody opens the ledger when writing the new item. The scaffold is inert: `run` refuses it until a human writes the numbered requirements and runs `drover accept`, and `accept` is one of the commands a launched session cannot reach, so filing one records the finding and schedules nobody. A project declaring no intake stop marker has no inert scaffold to write and `new` refuses there, so the finding is `REPORT-ONLY` instead.",
}, {
	Token: "REPORT-ONLY",
	Where: "the report, and nowhere else",
	Found: "Anything you would not spend a run on, would not file, and cannot correct in this pass.",
	Do:    "A finding nobody would schedule needs no tracker row and no backlog body. Say it in the report and stop there.",
}}

// Wrap renders one destination as an indented paragraph, every line prefixed
// with indent spaces.
//
// The prose is one value read by two templates whose list markers indent
// differently, so a literal wrapped in the source would be right in at most one
// of them.
func (d Destination) Wrap(indent int) string {
	const width = 79
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	col := 0
	for _, w := range strings.Fields(d.Found + " " + d.Do) {
		switch {
		case col == 0:
			b.WriteString(pad + w)
			col = indent + len(w)
		case col+1+len(w) > width:
			b.WriteString("\n" + pad + w)
			col = indent + len(w)
		default:
			b.WriteString(" " + w)
			col += 1 + len(w)
		}
	}
	return b.String()
}

// Commit is one commit on an item's branch, and how many lines it changed.
//
// Tagged because show encodes it: a template reads the fields by name and a
// consumer reads the keys, and the two spellings have to be decided rather than
// inherited from Go.
type Commit struct {
	Abbrev string `json:"abbrev"`
	Lines  int    `json:"lines"`
}

// Recent is the last few commits on an item's branch, or why there are none.
//
// Unavailable is prose rather than a token for the same reason Dep.Unreadable
// is: the alternative is every template reinventing the sentence. It is never
// empty at the same time as Commits, so a brief either reports sizes or reports
// what it could not ask.
type Recent struct {
	Commits     []Commit
	Unavailable string
}

// Sizes reads the commit sizes as prose: "158, 121 and 158".
func (r Recent) Sizes() string {
	n := make([]string, len(r.Commits))
	for i, c := range r.Commits {
		n[i] = strconv.Itoa(c.Lines)
	}
	switch len(n) {
	case 0:
		return ""
	case 1:
		return n[0]
	default:
		return strings.Join(n[:len(n)-1], ", ") + " and " + n[len(n)-1]
	}
}

// Conflict is a merge of the trunk into the item's branch that stopped on
// conflicts. Trunk is the ref merged and Commit the commit it named; each path
// carries the items whose trunk commits since the branch's merge base touched
// it, so the session knows whose requirements the other side of a hunk meets.
type Conflict struct {
	Trunk  string
	Commit string
	Paths  []ConflictPath
}

// ConflictPath is one conflicted file. Items is empty when no item in the
// ledger owns the trunk commits that touched it: a commit made outside drive.
type ConflictPath struct {
	Path  string
	Items []ConflictItem
}

// ConflictItem is an item whose merged work touched a conflicted file, with
// the requirements a resolution must not drop. Unreadable is why its body
// could not be read, which leaves Requirements empty.
type ConflictItem struct {
	ID           string
	Title        string
	Requirements []backlog.Requirement
	Unreadable   string
}

// Data is everything a template may reach.
type Data struct {
	Project string
	ID      string
	Stem    string
	Slug    string
	Title   string
	Branch  string
	Body    string
	Planned bool
	// Kind is what the item is: its frontmatter `kind` where it declares one,
	// else the kind its id's namespace maps to. Empty when neither answers, so a
	// template reaching it wants `with` rather than `if`: a branch template that
	// errors takes the item out of branchOwners silently while run and finish on
	// the same id refuse, and the two disagreeing is worse than a plainer name.
	Kind string

	// Meta is the item's frontmatter, so a project-specific key needs no change
	// here to reach its own template.
	Meta map[string]string

	Deps   []Dep
	Specs  []string
	Skills []Skill

	// VerifyCommand is the command selected for this item; Gates is every command the
	// project declares, for a template that wants to choose differently.
	VerifyCommand  string
	VerifyCommands map[string]string
	// MergeGate is [drive] merge_gate joined into one line, empty when the
	// project has none. Under one, the merge waits on it, so the review brief
	// skips the full verify command the implementing session already ran.
	MergeGate string

	FollowUps []Issue
	// RegisterUnreadable is why the register could not be read, empty when it
	// was read. FollowUps is then a floor rather than the list, and a template
	// branching on FollowUps alone renders an unreachable tracker byte for byte the
	// way it renders a clean one. Same shape as Recent.Unavailable below, one
	// read further out.
	//
	// A project that declared no register leaves this empty: it has failed
	// nothing, which is why gh.Index.Off keeps OK true, and a brief reporting an
	// absence there would report a setting as a fault.
	RegisterUnreadable string
	// Owned is the open issues routed to this item or one of its deps that do
	// not carry the follow-up label, so are not register rows and never reach
	// FollowUps: a confirmed bug filed against the branch is routed exactly like
	// a follow-up and would otherwise be invisible to every brief. Read only under the GitHub backend.
	//
	// OwnedUnreadable is why that read failed, empty when it answered or was
	// never asked. Same reason RegisterUnreadable exists: an empty Owned and an
	// unreachable tracker render alike otherwise.
	Owned           []Issue
	OwnedUnreadable string

	// Review-only. An implementing template never reaches these; a review template
	// needs them, because the whole point of the review is to check the claim
	// against the contract rather than re-derive either.
	Requirements []backlog.Requirement
	// SelfReport is what the run claimed, Outcome its one-line result, LogPath
	// where the stream lives. Empty when no run log exists.
	SelfReport string
	Outcome    string
	LogPath    string
	// Level, By, Note and AuditDate are this item's own current audit
	// provenance, empty when nobody has recorded one. The note rides verbatim
	// because a re-review needs to read what the last pass actually claimed;
	// structuring it would cost the prose that makes it worth reading.
	Level       string
	By          string
	Independent bool
	Note        string
	AuditDate   string
	// Unfinished marks a log that exists but carries no result event: the run is
	// still in flight, or it died. That is a different answer from "never ran",
	// and a reviewer told the wrong one reviews the wrong tree.
	Unfinished bool
	// Closes is the tracker issues the item claims to resolve, with whatever a
	// prior pass concluded about each. Empty when the item declares none.
	Closes []Resolution
	// Runs, Reviews and Audits are how many of each the ledger records before
	// this pass. Each is labeled by what wrote it rather than added into one
	// round number, because they come apart: a project that pipes `drover
	// review` records no review launch, and a person who reads the diff by hand
	// records no launch at all. A number that undercounts silently is worse than
	// three a reader can tell apart.
	Runs, Reviews, Audits int
	// Recent is how big the last commits on this item's branch were, or why git
	// could not say.
	Recent Recent
	// Modified is how many source files this branch changed, Unnamed the ones
	// the item body never mentions, and ScopeUnavailable why git could not say.
	// Review-only: the question they answer is whether the work is still this
	// item, which is the reviewer's to route and not a run's to act on.
	//
	// The list is rendered, never a count alone and never a threshold. A body
	// citing three paths has most of its diff unnamed by construction, so the
	// number is ambiguous where the list is not: a reviewer reading three
	// storage backends recognizes a contract this item never named. Measured
	// over 49 items in two projects, the ones whose diff mostly left their cited
	// files took about twice as many passes as the rest, which is enough to ask
	// the question and nowhere near enough to answer it.
	Modified         int
	Unnamed          []string
	ScopeUnavailable string

	// Rounds is how many times a review has sent this item back, MaxReviewRounds the
	// project's limit, and Capped whether the next request-changes refuses. A brief that
	// instructs `drover request-changes` without saying it will refuse sends an agent
	// retrying, so the capped brief keeps the instruction and says the refusal parks
	// the item with its findings; it never offers filing, because an unmerged item's
	// own defects are unfinished work rather than bugs.
	Rounds, MaxReviewRounds int
	Capped                  bool

	// Trail is every sent-back section before the one in the body, oldest first, empty
	// until a second round lands. See prompts.Round.
	Trail []Round

	// Conflict is the trunk merge drive left conflicted in the item's worktree
	// before this round, nil when the round started on a clean branch. Both the
	// implementing prompt and the review of that round carry it: the one resolves the
	// conflict, the other checks the resolution kept both sides.
	Conflict *Conflict

	// Path is the item body on disk, project-relative. Every brief carries it:
	// a draft writes the file, and a review that sends a finding back to the
	// branch rewrites the section the next run reads. An instruction naming the
	// file gets followed; one saying "the item body" starts a search.
	Path string

	// Draft-only. Posture is [agents] principles, the project's bar; StopMarker is
	// the line the draft must leave in place; Origin is the issue behind the
	// scaffold; Finding is the item a provenance line names and what its
	// reviewers wrote; and Siblings is the rest of the backlog, for routing.
	Posture    string
	StopMarker string
	Origin     Origin
	Finding    Finding
	Siblings   []Sibling
}

// Earlier is the trail minus the round the body already carries. A brief that
// renders both lists the current findings twice, once as instructions and once
// as history, and an agent reading the second copy has been told the open
// finding is closed.
//
// Only a standing block carries one. A cleared block replaced that evidence
// with the statement that nothing is outstanding, so nothing is duplicated and
// the newest round is the round a reader of a cleared body has no other way to
// see: dropping it costs the brief the very evidence the clear claims somebody
// read against the tree. A body whose section is gone altogether answers the
// same way, for the same reason.
func (d Data) Earlier() []Round {
	if d.Changes() == "" || d.Cleared() {
		return d.Trail
	}
	if len(d.Trail) < 2 {
		return nil
	}
	return d.Trail[:len(d.Trail)-1]
}

// Named is the count of modified source files the body does name, which is the
// half a reviewer reads as reassurance. Derived rather than carried so the two
// numbers cannot disagree, and a method so no template has to subtract.
func (d Data) Named() int { return d.Modified - len(d.Unnamed) }

// Repeated reports an item that has been round the loop before. It is the one
// condition the brief tests, so the threshold lives here rather than in the
// template: on a first pass there is no trend to report, and a line saying so
// on every brief in every project is noise that invites a reader to wonder what
// it is for.
func (d Data) Repeated() bool { return d.Runs > 1 || d.Audits > 1 }

// MergedDeps is every dep the ledger records done whose body drover could read.
//
// This and the two below partition the dependency list, and the split lives
// here rather than in a template because a brief that names an unmerged dep
// under a merged claim sends an agent hunting for code nobody wrote, which it
// then either invents or reports as a broken dependency. A template that
// reaches .Deps still gets all of them.
//
// Unreadability wins over either merge answer, so no dep lands in two sections.
// A merged dep whose body is gone still carries a level the ledger will print,
// and printing it under a heading saying this item's correctness rests on the
// dep hands a reviewer a claim about code nothing can show them.
func (d Data) MergedDeps() []Dep {
	return filterDeps(d.Deps, func(p Dep) bool { return p.Done && p.Unreadable == "" })
}

// UnmergedDeps is every dep the ledger does not record done whose body drover
// could read. Advisory: run reports these and does not refuse to launch over
// them.
func (d Data) UnmergedDeps() []Dep {
	return filterDeps(d.Deps, func(p Dep) bool { return !p.Done && p.Unreadable == "" })
}

// UnreadableDeps is every dep with no readable body, merged or not.
func (d Data) UnreadableDeps() []Dep {
	return filterDeps(d.Deps, func(p Dep) bool { return p.Unreadable != "" })
}

// UndisposedCloses is every claimed issue still waiting on a judgement.
func (d Data) UndisposedCloses() []Resolution {
	var out []Resolution
	for _, r := range d.Closes {
		if r.Undisposed() {
			out = append(out, r)
		}
	}
	return out
}

func filterDeps(deps []Dep, keep func(Dep) bool) []Dep {
	var out []Dep
	for _, dep := range deps {
		if keep(dep) {
			out = append(out, dep)
		}
	}
	return out
}

// Destinations is where a finding goes, reachable from a template. The list is
// a property of the tool rather than of the item, so it is a method rather than
// a field nothing populates.
func (d Data) Destinations() []Destination { return Destinations() }

// OperatorRequirements are the body's [operator] requirements. A session told
// that every numbered requirement is its contract would attempt these too, and
// each is one only the operator can meet.
func (d Data) OperatorRequirements() []backlog.Requirement {
	var out []backlog.Requirement
	for _, r := range (backlog.Item{Body: d.Body}).Requirements() {
		if r.Operator {
			out = append(out, r)
		}
	}
	return out
}

// Get reads a frontmatter value, so a template can write {{ .Get "severity" }}
// without erroring on a project that does not declare the key.
func (d Data) Get(key string) string { return d.Meta[key] }

// Is reports a frontmatter key set to "yes".
func (d Data) Is(key string) bool { return d.Meta[key] == "yes" }

// SelectVerify picks the command for an item.
//
// A gate named for a frontmatter key wins when that key is "yes", so a project
// declares `[verify] web_only = "..."` and any item with `web_only: yes` gets it.
// Names are tried in sorted order and the first match wins; everything else
// falls to "default". The convention keeps gate selection out of the template
// for the common case without hiding it when a project needs more.
func SelectVerify(gates map[string]string, meta map[string]string) string {
	names := make([]string, 0, len(gates))
	for name := range gates {
		if name != "default" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if meta[name] == "yes" {
			return gates[name]
		}
	}
	return gates["default"]
}

var funcs = template.FuncMap{
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"join":  strings.Join,
	"trim":  strings.TrimSpace,
}

// Render executes text against data. name is used in error messages only.
func Render(name, text string, data Data) (string, error) {
	t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	// A template that ends mid-thought is a template bug worth catching before
	// the text reaches an agent that will act on it.
	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("template %s rendered nothing", name)
	}
	return out + "\n", nil
}

// Load reads a template file, falling back to fallback when path is empty.
func Load(path, fallbackName, fallback string) (string, string, error) {
	if path == "" {
		return fallbackName, fallback, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read prompt template: %w", err)
	}
	return path, string(data), nil
}

// Sample is a brief with every field populated, for checking a template without
// an item to render it for.
//
// Populated rather than zero because text/template resolves a field only on the
// path it executes: against a zero Data every range has nothing to iterate, so
// a field a project's template names only inside `{{ range .FollowUps }}` is never
// looked up and a template naming a field this build removed passes. Every
// slice and map holds one element for the same reason, and TestSampleFillsData
// fails when a field is added that this leaves at its zero value.
func Sample() Data {
	issue := Issue{
		Number: 1, Title: "sample issue", Severity: "high", Owners: []string{"1"}, Mine: true,
		Body: "sample report", Unavailable: "sample reason", Labels: []string{"follow-up"},
	}
	return Data{
		Project: "sample", ID: "1", Stem: "01-sample", Slug: "sample", Title: "Sample item",
		Branch: "feature/01-sample", Body: "1. [operator] Sample requirement.\n", Planned: true, Kind: "feature",
		Meta: map[string]string{"kind": "feature"},
		Deps: []Dep{{
			ID: "2", Title: "Sample dep", Done: true, Unreadable: "sample reason",
			Level: "reviewed", By: "agent", Independent: true, Weak: true, Report: "reports/2.md",
		}},
		Specs:              []string{"docs/sample.md"},
		Skills:             []Skill{{Name: "sample", Path: "skills/sample/SKILL.md"}},
		VerifyCommand:      "make check",
		MergeGate:          "./scripts/merge-gate.sh",
		VerifyCommands:     map[string]string{"default": "make check"},
		FollowUps:          []Issue{issue},
		RegisterUnreadable: "sample reason",
		Owned:              []Issue{issue},
		OwnedUnreadable:    "sample reason",
		Requirements:       []backlog.Requirement{{Num: 1, Text: "Sample requirement.", Operator: true}},
		SelfReport:         "sample report",
		Outcome:            "success",
		LogPath:            "logs/01-sample.jsonl",
		Level:              "reviewed",
		By:                 "agent",
		Independent:        true,
		Note:               "sample note",
		AuditDate:          "2026-01-01",
		Unfinished:         true,
		Closes: []Resolution{{
			Number: 1, Title: "sample issue", Body: "sample report", State: "OPEN",
			Disposition: "fixed", By: "agent", Note: "sample note",
			Applied: "closed", Unavailable: "sample reason", Settled: true, Amended: "2026-01-01",
		}},
		Runs: 1, Reviews: 1, Audits: 1,
		Recent:           Recent{Commits: []Commit{{Abbrev: "abc1234", Lines: 1}}, Unavailable: "sample reason"},
		Modified:         1,
		Unnamed:          []string{"sample.go"},
		ScopeUnavailable: "sample reason",
		Rounds:           1,
		MaxReviewRounds:  1,
		Capped:           true,
		Trail:            []Round{{Number: 1, Date: "2026-01-01", Headline: "sample headline", Path: "reports/1.md"}},
		Conflict: &Conflict{Trunk: "origin/main", Commit: "abc1234", Paths: []ConflictPath{{Path: "sample.go",
			Items: []ConflictItem{{ID: "2", Title: "Sample dep", Unreadable: "sample reason",
				Requirements: []backlog.Requirement{{Num: 1, Text: "Sample requirement."}}}}}}},
		Path:       "backlog/01-sample.md",
		Posture:    "sample principles",
		StopMarker: "<!-- sample stop marker -->",
		Origin:     Origin{Number: 1, Title: "sample issue", Body: "sample report", Unavailable: "sample reason"},
		Finding:    Finding{From: "3", Notes: []AuditNote{{Date: "2026-01-01", Level: "reviewed", By: "agent", Note: "sample note"}}},
		Siblings:   []Sibling{{ID: "2", Title: "Sample dep", Path: "backlog/02-sample.md", Done: true}},
	}
}

// SampleFor is Sample with Meta holding a value for every key a project
// declares and Gates holding the project's own gates.
//
// Render runs under missingkey=error, so `{{ .Meta.phase }}` against Sample's
// one-key Meta fails although every item carrying phase renders it. Seeding the
// maps from the declared set, rather than tolerating a missing key, keeps a
// typo for a key nobody declared failing.
func SampleFor(keys []string, gates map[string]string) Data {
	d := Sample()
	for _, k := range keys {
		if _, ok := d.Meta[k]; !ok {
			d.Meta[k] = "sample"
		}
	}
	d.VerifyCommands = maps.Clone(gates)
	return d
}
