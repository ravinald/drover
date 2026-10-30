// Package ledger reads and appends the completion and audit record.
//
// The file is JSONL and append-only. That is a deliberate choice over a real
// database: the ledger lives inside a git-tracked documentation tree whose
// whole purpose is auditable history, so it has to stay diffable, and an agent
// that ran an item outside drover must be able to record the fact by appending
// one line with no binary and no network.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/runlog"
)

// Row is one record. Every shape shares a struct so that field order — which
// the file's readability depends on — is declared in exactly one place.
//
// Merged is a pointer so that an explicit false (a tombstone withdrawing an
// earlier completion) is distinguishable from a row that says nothing about
// completion at all.
type Row struct {
	ID           string `json:"id"`
	Merged       *bool  `json:"merged,omitempty"`
	MergedRunner string `json:"implementer,omitempty"`
	// Stage and Runner are the third shape: one row per launch, written by run
	// and review from the runner drover itself resolved. Agent above is an
	// operator's attribution and can be typed; this is an observation.
	Stage  string `json:"stage,omitempty"`
	Runner string `json:"runner,omitempty"`
	// Session is the launch id a stage row's session ran under, the value its
	// DROVER_SESSION carried. It is how `--code-by agent` resolves per session:
	// an auditor whose id sits on one of the item's implement rows wrote the code.
	Session string `json:"session,omitempty"`
	// Conflict marks an implement stage row whose session drive started on a
	// trunk merge it left conflicted in the worktree, for the session to resolve.
	Conflict bool `json:"conflict,omitempty"`
	// Level is the level an audit row records, and Reviewer who the operator
	// says looked. By is --code-by as typed, and Independent is drover's reading
	// of it at write time: the auditor differed from every entry. It is stored
	// rather than re-derived because the auditor's identity is the process that
	// wrote the row, and nothing reading the file later can ask it again.
	Level       string `json:"level,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	CodeBy      string `json:"code_by,omitempty"`
	Independent bool   `json:"independent,omitempty"`
	// Issue and Disposition are the fourth shape: one row per tracker issue the
	// item claims to resolve. Issue is what makes it distinguishable — every
	// other shape leaves it zero — so a reader never has to guess which of five
	// records a line is from the fields that happen to be populated.
	//
	// Applied is written by finish once the tracker actually took the write, and
	// names what reached it: "closed" or "commented". It is a separate row
	// rather than a mutation because the file is append-only, and it is what
	// stops a second finish from commenting on the same issue twice.
	//
	// Locking is finish locking the issue before its tracker call, so that two
	// processes finishing one item cannot both comment: the lock row is appended
	// under the ledger's lock, and whoever finds a live one there makes no call.
	// It carries the lock's time, or LockReleased once a failed call gave the
	// issue back. The time is what lets a lock left behind by a crash expire
	// rather than hold the issue forever.
	Issue       int    `json:"issue,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Applied     string `json:"applied,omitempty"`
	Locking     string `json:"locking,omitempty"`
	// Changes is the fifth shape: one row per `drover request-changes`, the review sending
	// work back to the branch. It carries a digest of the evidence rather than
	// the evidence, which is in the item body request-changes just wrote and in that
	// file's history; the ledger records that the event happened, not a lossier
	// second copy of the prose. The same digest twice is a review repeating
	// itself, which says more about termination than a count does.
	//
	// It is the first shape nothing folds. Every other one resolves
	// last-row-wins because each answers a question with one current answer;
	// "how many times has this gone back" has only a total, and a fold would
	// erase the number the row exists to carry.
	//
	// No round number in the row: that is len(changes rows) at read time. This
	// package promises a row can be appended by hand with no binary, and a
	// hand-written "round": 3 beside three existing rows is a contradiction
	// somebody has to resolve.
	Changes string `json:"changes,omitempty"`
	// ChangesSource names what wrote a changes row: ChangesReview for
	// `drover request-changes` run by a review or a person, or one of the drive
	// paths that send a branch back on their own evidence. Only a review's rows
	// are rounds under [review] max_review_rounds, so a branch drive sent back
	// for a red verify command does not spend the rounds its reviews are owed.
	// Absent on a row written before the field existed, which reads as a review.
	ChangesSource string `json:"changes_source,omitempty"`
	// Reqs is the requirement numbers the evidence's findings name, sorted, on a
	// changes row, and on a refused row the ones the request the cap turned away
	// named. The digest compares wording, so a reviewer rewording one
	// unmet requirement round after round writes a new digest every time; the
	// numbers are the substance it misses. Absent where no finding names one.
	Reqs []int `json:"reqs,omitempty"`
	// StillOpen is the requirement numbers, a subset of the previous changes
	// row's Reqs, whose finding from that request the reviewer reproduced as
	// still open, as opposed to a new defect found under the same requirement.
	// Only the reviewer can tell the two apart, so it says so; drover never
	// infers it from Reqs. Absent where every finding is new.
	StillOpen []int `json:"still_open,omitempty"`
	// Operator is the requirement numbers whose finding the reviewer judged no
	// implementing session can meet as written: an upstream release that does
	// not exist, a decision nobody has made, evidence no source publishes. drive
	// parks the item for the operator instead of sending it back. Absent where
	// every finding is the implementer's.
	Operator []int `json:"operator,omitempty"`
	// Pass is the sixth shape: one row per review pass drive read a result
	// from, carrying the token it computed. Every token is recorded and not
	// only the one that merges, because a file holding passes alone cannot tell
	// an item no drive has reached from one a drive reached and parked, and a
	// command deciding whether a branch may get a pull request wants opposite
	// answers for those two.
	//
	// It folds last-row-wins, unlike Changes above: "did the last review pass"
	// has one current answer, and an older result is not evidence about the
	// branch that exists now.
	Pass string `json:"review_result,omitempty"`
	// Refused is a request for changes the cap turned away. Without it the refusal would
	// write nothing: a review at the cap that wants to send work back records
	// its audit, is refused, and leaves an audit with no request for changes — which is the
	// signature of a review that found nothing. passResult could not tell the
	// two apart, so it would read every pass on a capped item as capped, and an
	// item that had ever taken max_review_rounds rounds could never again come
	// through clean however good the review was.
	//
	// It carries no count: a refusal per pass is what the reader needs, and
	// Counts.RefusedSince answers that against the pass rows around it.
	Refused string `json:"refused,omitempty"`
	// Findings on a refused row is the file, relative to the project root,
	// holding the evidence the cap turned away. The body is left untouched by a
	// refusal, so without the file the findings would live only in a session
	// log, and the operator deciding the item would have nothing to read.
	Findings string `json:"findings,omitempty"`
	// Branch and Head are the eighth shape: one row each time drover saw an
	// item's branch at a commit the trunk did not contain, written before every
	// push. It is the evidence that the branch carried work of its own, which
	// ancestry cannot supply after the fact: a branch that never held a commit
	// sits at or behind the trunk, and `git branch --merged` lists it exactly as
	// it lists one whose commits merged. A stage row cannot say it either, since
	// that is written at launch, before any commit exists.
	//
	// PR is the tenth shape, sharing Branch with the eighth: one row each time
	// drover opened or found the pull request for an item's branch, carrying
	// its number. A non-zero pr is what makes it distinguishable, as a non-zero
	// issue does for a disposition. It is the only place the number outlives the
	// command that learned it, so a merged item can name its pull request
	// without asking the forge, and it folds last-row-wins: a branch that
	// reopens as a second pull request has one current answer.
	//
	// MergeQueued on a pull request row is drive recording, the moment the
	// forge accepted its --auto request, that the merge is queued there. It is
	// a row of its own rather than the end row's outcome because the end row is
	// written when the drive exits: a replacement pull request or a completion
	// recorded in between would read as older than the queue it ended.
	Branch      string `json:"branch,omitempty"`
	Head        string `json:"head,omitempty"`
	PR          int    `json:"pr,omitempty"`
	MergeQueued bool   `json:"merge_queued,omitempty"`
	// Check, Class and Evidence are the eleventh shape, sharing Head with the
	// eighth: one row each time drive read a red check on a pushed head, naming
	// the check, whose failure it took it for (one of the Class constants) and the
	// line that decided it. A non-empty class is what makes it distinguishable,
	// and the absent branch keeps Heads from reading it as a push. A head no check
	// ran on is a row of class none naming no check. Rerun marks a
	// row whose check drive reran, and the count of those on one head is what
	// [forge] reruns bounds, across drives as well as within one.
	Check    string `json:"check,omitempty"`
	Class    string `json:"class,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Rerun    bool   `json:"rerun,omitempty"`
	// Park, Detail and Log are the twelfth shape, sharing Evidence with the
	// eleventh: one row each time drive parked an item, naming the cause, why
	// the session or push under it failed (one of a small vocabulary drive
	// owns), the line that says so and the log it came from. A non-empty park
	// is what makes it this shape. It is the only drive outcome the ledger
	// keeps, because "parked" is what status would otherwise print as ran, and
	// the operator reading that has to open session logs to learn the cause.
	Park   string `json:"park,omitempty"`
	Detail string `json:"detail,omitempty"`
	Log    string `json:"log,omitempty"`
	// Body is a 12-hex digest of the item file's exact bytes, frontmatter
	// included. It is evidence of what the body held when drover last had a hand
	// in it: the review stage row carries the body as the session launched, and
	// the ninth shape, a row of {"id","body","date"} and its author, carries it after each
	// command that writes an item (request-changes, close-changes, invariant,
	// link, split, accept, unaccept, amend, and touch, which adds its reason as
	// Note). A body that no longer matches the latest digest was changed by
	// something drover did not run, which is how a review that writes its
	// findings by hand is told from one that found nothing.
	//
	// The whole file rather than the prose under the frontmatter, because link
	// and split write the frontmatter and a hand edit can land there as easily as
	// anywhere else. A separate row rather than a field on each command's own
	// row, because link and invariant write no row of their own to carry it.
	Body string `json:"body,omitempty"`
	// Cmd, Actor and Agent name who wrote the body a row's digest records:
	// the drover command, the actor it ran as and the interactive agent driving
	// it, the Author BodyRow stamps. Every row carrying Body carries them, and a
	// row written before they existed carries none, which a reader reports as
	// unrecorded rather than as the operator. Append refuses a digest without
	// Cmd and Actor, and MarshalJSON keeps an empty Agent on such a row, so "no
	// agent" reads as recorded rather than as a key nobody wrote.
	Cmd   string `json:"cmd,omitempty"`
	Actor string `json:"actor,omitempty"`
	Agent string `json:"agent,omitempty"`
	// Origin is the eighteenth shape: one row per item, written by the command
	// that created its body, saying how it came to exist. A set origin is what
	// makes it this shape. An item created before drover recorded it has none,
	// and none is ever backfilled: a row drover did not witness would be a
	// record nobody made.
	Origin *Origin `json:"origin,omitempty"`
	// Cleared marks the ninth-shape row `drover close-changes` writes. The digest alone
	// cannot say which command wrote the body, and open-pr needs exactly that:
	// a clear after a standing pass is a reviewer saying the block that parked
	// it is closed, while an invariant or link row in the same place says nothing
	// about the block.
	Cleared bool `json:"cleared,omitempty"`
	// Signoff is the fourteenth shape: one row per `drover signoff`, the operator
	// recording that they met one [operator] requirement, by its number, with
	// Note saying what they checked. A non-zero attest is what makes it this
	// shape. Any row counts and none withdraws one: the check happened, and a
	// later row only adds what a second look found.
	Signoff int `json:"signoff,omitempty"`
	// PostFinish is the fifteenth shape: [finish] post_check exiting 0 once an item
	// merged, carrying the first line of what it printed. A failure writes no
	// row; it files a follow-up against the item instead, because a failed check is
	// work somebody owes and a ledger row is owed by nobody.
	PostFinish string `json:"post_finish,omitempty"`
	// Amend is the sixteenth shape: one row per `drover amend`, the operator
	// replacing an accepted item's numbered requirements, with Note carrying
	// the reason. It sits beside the body row amend writes rather than on it,
	// so every reader of body rows keeps reading a digest and nothing else.
	Amend *Amendment `json:"amend,omitempty"`
	// Drive is the drive that appended the row, D1, D2 and so on, empty on a
	// row written outside one. Every row a drive appends carries it, which is
	// what groups an item's history by the drive that ran it without reading
	// the dependency graph.
	//
	// Label, Queued, Jobs, Exit and Outcomes are the thirteenth shape, two rows
	// per drive under the id DriveRowID: a start row carrying the label, the ids
	// queued at launch and --jobs, and an end row carrying the exit code and what
	// each item it ran came to. A present exit is what makes a row the end.
	// A drive that re-executed itself onto a newer binary writes a second
	// start row, naming the binary it now runs as Version and the one it
	// replaced as Replaced; the first start row still says when it began.
	Drive    string    `json:"drive,omitempty"`
	Label    string    `json:"label,omitempty"`
	Queued   []string  `json:"queued,omitempty"`
	Jobs     int       `json:"jobs,omitempty"`
	Exit     *int      `json:"exit,omitempty"`
	Outcomes []Outcome `json:"outcomes,omitempty"`
	Version  string    `json:"version,omitempty"`
	Replaced string    `json:"replaced,omitempty"`
	// Usage and the fields after it are the sixteenth shape: one row per
	// session whose tokens drover read, a headless session's from its log and
	// an interactive one's from Claude Code's transcript, one row per local day
	// it ran. A set usage is what makes it this shape, and a usage row may name
	// no item, which no other shape may. Load keeps these rows in
	// Ledger.Usage rather than Rows, so a reader that folds stages, passes or
	// launches cannot count a session twice.
	//
	// The counts are tokens as the API reported them, not the subscription's
	// quota, and CostUSD is the runner's list-price figure. No weighted number
	// is stored: the weights are a guess about the quota and will change, and a
	// stored guess outlives the change.
	Usage      *runlog.Usage `json:"usage,omitempty"`
	Model      string        `json:"model,omitempty"`
	SessionID  string        `json:"session_id,omitempty"`
	CostUSD    float64       `json:"cost_usd,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	Turns      int           `json:"turns,omitempty"`
	Source     string        `json:"source,omitempty"`
	// ContextMax is a pointer so a usage row carries it at zero, a 429's or a
	// log with no assistant event, while every other row omits it.
	ContextMax *int64 `json:"context_max,omitempty"`
	// RateLimit is the last rate_limit_event the session's log carried, the
	// account's state as that session left it, and absent on a row from a
	// runner that writes none. drive's usage admission reads the newest one
	// across every registered project, because the limit is the account's.
	RateLimit *runlog.RateLimit `json:"rate_limit,omitempty"`
	// UsageWarning and ResetsAt are the seventeenth shape: one row per
	// usage_warning a drive sent, naming the window (weekly, five_hour,
	// overage, overage_usd) and the reset of the window it was about. A set
	// window is what makes it this shape, and Load keeps these rows in
	// Ledger.Warnings, because they name no item. One row per pair is the
	// whole guard against a second notice: a restarted drive, or a drive in
	// another project on the same account, reads it and sends nothing.
	UsageWarning string `json:"usage_warning,omitempty"`
	ResetsAt     int64  `json:"resets_at,omitempty"`
	// FirstTS is the first message of an interactive row's day, beside TS for
	// the last, and Started the session's first message on any day.
	FirstTS string `json:"first_ts,omitempty"`
	Started string `json:"started,omitempty"`
	// VerifyMS is the wall time in milliseconds of the verify command drive ran
	// for this item, carried on the next row drive appended for the item
	// through its own session after the verify command exited, whatever that
	// row's shape: a head, pull request, stage, park or done row. A changes row
	// never carries it, because drive writes a changes row through
	// request-changes rather than its own session; on a catch-up the time rides
	// the head row of the merged tree, written before the changes row. It is a
	// field rather than a shape of its own because every verify run ends in one of
	// those rows, and a second row per run would double what the ledger holds
	// about each catch-up.
	VerifyMS int64  `json:"verify_ms,omitempty"`
	Date     string `json:"date"`
	// TS is the instant the row was written, RFC 3339 with the writer's offset.
	// Date stays beside it rather than being derived from it, because every
	// reader written before TS existed keys on date, and a row appended by hand
	// or by an older drover carries no TS at all: that row's time is day-only,
	// and a reader must report it absent rather than invent midnight.
	TS     string `json:"ts,omitempty"`
	Report string `json:"report,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Origin is how one item came to exist, as the command that created its body
// saw it. Every field is written, empty where it does not apply, so a reader
// tells "intake made it from no issue" from a row that predates the field.
type Origin struct {
	// Cmd is new, intake or split.
	Cmd string `json:"cmd"`
	// Actor is operator, agent or supervisor, and Session the launch id of the
	// session drover started, empty for the operator.
	Actor   string `json:"actor"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	// FoundIn is new's --found-in, Issue the tracker issue intake read and
	// SplitFrom the item split took the work from.
	FoundIn   string `json:"found_in"`
	Issue     int    `json:"issue"`
	SplitFrom string `json:"split_from"`
}

// Author is who wrote a body: the drover command, the actor it ran as, and
// the interactive agent driving that actor's terminal, empty when none is
// known.
type Author struct {
	Cmd   string
	Actor string
	Agent string
}

// BodyRow is the row recording that by wrote the body whose digest is
// digest. It is the one constructor for a row carrying Body, so a writer
// cannot record a digest without saying who wrote it, and it stamps ts and
// date here rather than leaving them to the append.
func BodyRow(id, digest string, by Author) Row {
	return Stamp(Row{ID: id, Body: digest, Cmd: by.Cmd, Actor: by.Actor, Agent: by.Agent})
}

// MarshalJSON writes an attributed body row's agent key even when it is
// empty, the one departure from each field's omitempty. A row without Actor
// is a body row written before attribution existed, or no body row at all,
// and encodes as it always has.
func (r Row) MarshalJSON() ([]byte, error) {
	type plain Row
	if r.Body == "" || r.Actor == "" {
		return json.Marshal(plain(r))
	}
	return json.Marshal(struct {
		plain
		Agent string `json:"agent"`
	}{plain(r), r.Agent})
}

// StageOf is the stage row drover wrote for the launch session names, false
// when no stage row carries it. A launch id is minted once per launch, so the
// first match is the only one.
func (l *Ledger) StageOf(session string) (Row, bool) {
	if session == "" {
		return Row{}, false
	}
	for _, r := range l.Rows {
		if r.Stage != "" && r.Session == session {
			return r, true
		}
	}
	return Row{}, false
}

// Provenance is what the ledger knows about where one item came from and who
// last wrote its body.
type Provenance struct {
	// Origin is the item's origin row, nil for an item minted before drover
	// recorded one. That reads as unrecorded, never as an error.
	Origin *Row
	// LastBody is the latest row carrying a body digest, a review stage row
	// or a body row, nil when no drover that records digests wrote one.
	LastBody *Row
	// Matches is whether the file's digest as it stands equals LastBody's.
	// False with LastBody set is a hand edit nobody recorded.
	Matches bool
	// Launch is the stage row whose session created the item, nil when the
	// origin names no session or no stage row carries it. Its Drive, Stage
	// and ID say which drive, which stage and which item's session it was.
	Launch *Row
}

// Recorded reports whether the item's origin was written down when it
// happened.
func (p Provenance) Recorded() bool { return p.Origin != nil }

// Provenance reads one item's origin and latest body row. current is the
// file's digests as it stands now, every form a row may hold it in: a match
// against any of them is a match.
func (l *Ledger) Provenance(id string, current ...string) Provenance {
	var p Provenance
	for i := range l.Rows {
		r := &l.Rows[i]
		if r.ID != id {
			continue
		}
		if r.Origin != nil && p.Origin == nil {
			p.Origin = r
		}
		if r.Body != "" {
			p.LastBody = r
		}
	}
	if p.LastBody != nil {
		p.Matches = slices.Contains(current, p.LastBody.Body)
	}
	if p.Origin != nil {
		if st, ok := l.StageOf(p.Origin.Origin.Session); ok {
			p.Launch = &st
		}
	}
	return p
}

// Amendment is what one `drover amend` changed: the requirement count before
// and after, the other item when a requirement moved between two, and where
// the replaced list was kept.
type Amendment struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Moved string `json:"moved,omitempty"`
	Kept  string `json:"kept,omitempty"`
}

// Outcome is what one item a drive ran came to, as the drive reported it.
type Outcome struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	// Reason is why a skipped item was skipped, absent on every other outcome.
	Reason string `json:"reason,omitempty"`
	// MergeQueued is an item whose --auto merge the forge accepted, in this
	// drive, and the drive did not see merge. A later take of the item that
	// held, skipped or parked it keeps the flag, so it reads on any outcome,
	// and only ready_to_merge counts in the exit summary. It reports what the
	// drive saw as it exited; reconciliation reads the pull request row
	// written when the forge accepted the request instead.
	MergeQueued bool `json:"merge_queued,omitempty"`
	// PR is the pull request a queued merge belongs to, absent on every
	// outcome that queued nothing.
	PR int `json:"pr,omitempty"`
}

// Latest keeps one entry per id, the last one, at the position of that id's
// first. A drive that parks an item and then takes it again from --enqueue
// gives it one outcome, and an end row written before it held to that carries
// both, so a reader counting entries would count the park that no longer stands.
func Latest[T any](xs []T, id func(T) string) []T {
	at := make(map[string]int, len(xs))
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if i, ok := at[id(x)]; ok {
			out[i] = x
			continue
		}
		at[id(x)] = len(out)
		out = append(out, x)
	}
	return out
}

// DriveRowID is the id a drive's start and end rows carry. It holds no digit,
// so no namespace can mint an item under it, where the drive's own id would
// collide with an item in a project that declares a D namespace.
const DriveRowID = "drive"

// DrivePrefix leads every drive id.
const DrivePrefix = "D"

// driveNumber reads n out of Dn, zero for anything else.
func driveNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, DrivePrefix))
	if err != nil || !strings.HasPrefix(id, DrivePrefix) || n < 0 {
		return 0
	}
	return n
}

// Drive is one drive as its start and end rows record it.
type Drive struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Started string   `json:"started"`
	Queued  []string `json:"queued"`
	Jobs    int      `json:"jobs,omitempty"`
	// Ended and Exit are absent until the drive wrote its end row. A drive
	// killed before it could write one stays without them for good, so the
	// caller reads the lease to tell running from interrupted.
	Ended string `json:"ended,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	// Items is what each item the drive ran came to, from its end row, and the
	// ids queued at launch with no outcome while it has none.
	Items []Outcome `json:"items"`
	// Running is the lease naming this drive, set by a caller that read it,
	// and Paused that lease reading paused. ConfigChanged is the config on
	// disk differing from the one that lease says the drive runs under, which
	// a running drive does not apply until a resume.
	Running       bool `json:"running,omitempty"`
	Paused        bool `json:"paused,omitempty"`
	ConfigChanged bool `json:"config_changed,omitempty"`
	// UsageHolds is each usage hold a paused drive's lease names as standing,
	// which a resume releases one at a time.
	UsageHolds []lease.UsageHold `json:"usage_holds,omitempty"`
}

// Drives folds the start and end rows into one Drive each, newest first.
func (l *Ledger) Drives() []Drive {
	byID := map[string]*Drive{}
	var order []string
	for _, r := range l.Rows {
		if r.ID != DriveRowID || r.Drive == "" {
			continue
		}
		d, seen := byID[r.Drive]
		if !seen {
			d = &Drive{ID: r.Drive, Queued: []string{}}
			byID[r.Drive] = d
			order = append(order, r.Drive)
		}
		when := r.TS
		if when == "" {
			when = r.Date
		}
		if r.Exit == nil && r.Replaced != "" && d.Started != "" {
			continue
		}
		if r.Exit == nil {
			d.Label, d.Started, d.Jobs = r.Label, when, r.Jobs
			if r.Queued != nil {
				d.Queued = r.Queued
			}
			continue
		}
		d.Ended, d.Exit, d.Items = when, r.Exit, nil
		if r.Outcomes != nil {
			d.Items = Latest(r.Outcomes, func(o Outcome) string { return o.ID })
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return driveNumber(order[i]) > driveNumber(order[j]) })
	out := make([]Drive, 0, len(order))
	for _, id := range order {
		d := byID[id]
		if d.Items == nil {
			d.Items = make([]Outcome, 0, len(d.Queued))
			for _, q := range d.Queued {
				d.Items = append(d.Items, Outcome{ID: q})
			}
		}
		out = append(out, *d)
	}
	return out
}

// MergeQueued is the pull request whose --auto merge a drive left queued at
// the forge for id, and whether that request is still outstanding. It reads
// the item's rows in the order they were appended, which is the order things
// happened: a queued pull request row starts it, and a completion row or a row
// naming another pull request ends it. A drive's end row is not read, since it
// is written at exit and would replay a queue something earlier ended. A drive
// that held, skipped or parked the item writes neither, and the merge still
// lands.
func (l *Ledger) MergeQueued(id string) (pr int, ok bool) {
	for _, r := range l.Rows {
		switch {
		case r.ID != id:
		case r.Merged != nil && *r.Merged:
			pr, ok = 0, false
		case r.PR > 0 && r.MergeQueued:
			pr, ok = r.PR, true
		case r.PR > 0 && r.PR != pr:
			pr, ok = 0, false
		}
	}
	return pr, ok
}

// DriveOf is the drive named by the latest row for id that a drive wrote,
// empty when no drive has touched the item.
func (l *Ledger) DriveOf(id string) string {
	drive := ""
	for _, r := range l.Rows {
		if r.ID == id && r.Drive != "" {
			drive = r.Drive
		}
	}
	return drive
}

// What a reviewer concluded about one issue the item claims to resolve. Only
// Satisfied closes anything; the other two are the tracker being told what
// happened, which is the answer an issue left silently open never got.
const (
	DispositionSatisfied = "fixed"     // the merged work closes it
	DispositionPartial   = "partial"   // some of it shipped; the issue stays open
	DispositionUnrelated = "unrelated" // this item does not address it after all
)

// Dispositions lists the tokens in the order the reference documents them.
func Dispositions() []string {
	return []string{DispositionSatisfied, DispositionPartial, DispositionUnrelated}
}

// ValidDisposition reports whether a token is one drover acts on.
func ValidDisposition(d string) bool {
	for _, known := range Dispositions() {
		if d == known {
			return true
		}
	}
	return false
}

// What finish recorded doing to the tracker.
const (
	AppliedClosed    = "closed"
	AppliedCommented = "commented"
)

// LockReleased is the claiming value that gives an issue back after the
// tracker refused the write, so the next finish tries again at once.
const LockReleased = "unlocked"

// LockLease is how long a lock row holds an issue with no applied row behind
// it. It is the window a crashed finish leaves an issue unappliable for, and it has to
// outlast one tracker call, which has no timeout of its own.
const LockLease = 10 * time.Minute

// Resolution is what the rows resolve to for one issue of one item.
type Resolution struct {
	Issue       int    `json:"issue"`
	Disposition string `json:"disposition,omitempty"`
	CodeBy      string `json:"code_by,omitempty"`
	Independent bool   `json:"independent,omitempty"`
	Note        string `json:"note,omitempty"`
	Date        string `json:"date,omitempty"`
	// Applied is empty until finish wrote to the tracker, then names what it did.
	Applied string `json:"applied,omitempty"`
	// Settled marks an answer that has stopped moving: the current disposition
	// is the one at least two independent passes recorded in a row, with nothing
	// since disagreeing.
	//
	// Advisory, not a lock. The supersede rule above stays, because an early
	// pass can be wrong and re-judging is how that gets corrected. What this
	// carries is the evidence that re-judging has stopped changing the answer,
	// so a brief can hand the answer over as fact and a later pass spends its
	// turn on something else.
	//
	// Two independent rows is the evidence, and drover cannot check they were
	// different people: a disposition row names no reviewer, so independence
	// there is the --code-by list against the grading session alone.
	Settled bool `json:"settled,omitempty"`
	// Locked is when finish last took the issue, zero once released. It is not
	// a write and never reads as one: Applied stays empty until the tracker took
	// it, so everything that asks what is still pending still counts it.
	Locked time.Time `json:"-"`
}

// Lockable reports whether a finish may take the issue at now: no live lock
// holds it. Whether it is applied or disposed is the caller's question.
func (r Resolution) Lockable(now time.Time) bool {
	return r.Locked.IsZero() || now.Sub(r.Locked) >= LockLease
}

// Closes reports whether the disposition is one that closes the issue.
func (r Resolution) Closes() bool { return r.Disposition == DispositionSatisfied }

// Resolutions folds the disposition rows for one item, newest wins per issue.
//
// Last-row-wins is the audit ladder's rule applied to the same problem: a
// reviewer who called an issue satisfied and then found the gap must be able to
// correct it downward, and an append-only file has no other way to say so.
// Applied is the exception and sticks once set, because it records something
// that already happened on a tracker other people read.
func (l *Ledger) Resolutions(id string) []Resolution {
	byIssue := map[int]*Resolution{}
	said := map[int][]Row{}
	var order []int
	for _, row := range l.Rows {
		if row.ID != id || row.Issue == 0 {
			continue
		}
		cur, seen := byIssue[row.Issue]
		if !seen {
			cur = &Resolution{Issue: row.Issue}
			byIssue[row.Issue] = cur
			order = append(order, row.Issue)
		}
		if row.Disposition != "" {
			cur.Disposition, cur.CodeBy, cur.Independent, cur.Note, cur.Date = row.Disposition, row.CodeBy, row.Independent, row.Note, row.Date
			said[row.Issue] = append(said[row.Issue], row)
		}
		if row.Applied != "" {
			cur.Applied = row.Applied
		}
		switch at, err := time.Parse(time.RFC3339, row.Locking); {
		case row.Locking == LockReleased:
			cur.Locked = time.Time{}
		case row.Locking != "" && err == nil:
			cur.Locked = at
		}
	}
	sort.Ints(order)
	out := make([]Resolution, 0, len(order))
	for _, n := range order {
		byIssue[n].Settled = settled(said[n])
		out = append(out, *byIssue[n])
	}
	return out
}

// settled reads whether the last answer has stopped moving: at least two
// independent rows agreeing with it, in an unbroken run back from the
// most recent.
//
// Trailing is what makes it an answer about the present. satisfied, satisfied,
// partial is not settled: the current answer is partial and nothing stands
// behind it. An author row that agrees sits in the run and adds nothing, and one
// that disagrees ends it and becomes the current answer with no independent
// agreement behind it, which is right in both directions. An author must not be
// able to freeze an answer, and any later disagreement must be able to unfreeze
// one. A row with no authorship recorded counts as neither: an absent by is not
// a claim of independence.
func settled(rows []Row) bool {
	agree := 0
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Disposition != rows[len(rows)-1].Disposition {
			break
		}
		if rows[i].Independent {
			agree++
		}
	}
	return agree >= 2
}

// SignedOff is the numbers of one item's [operator] requirements the operator
// has recorded meeting.
func (l *Ledger) SignedOff(id string) map[int]bool {
	out := map[int]bool{}
	for _, r := range l.Rows {
		if r.ID == id && r.Signoff > 0 {
			out[r.Signoff] = true
		}
	}
	return out
}

// Resolution reads one issue's disposition, false when nobody recorded any.
func (l *Ledger) Resolution(id string, issue int) (Resolution, bool) {
	for _, r := range l.Resolutions(id) {
		if r.Issue == issue {
			return r, r.Disposition != ""
		}
	}
	return Resolution{Issue: issue}, false
}

// The stages a runner can be launched for. They are separate rows rather than
// one row with two fields because a run and its review are separate events,
// often days apart, and an append-only file cannot revisit the earlier one.
const (
	StageImplement = "implement" // `run` handed the prompt to a runner
	StageReview    = "review"    // `review` handed the brief to a runner
	StageDraft     = "draft"     // `draft` handed the drafting prompt to a runner
)

// What wrote a changes row, the values of ChangesSource.
const (
	ChangesReview        = "review"         // `drover request-changes`, by a review or a person
	ChangesVerify        = "verify"         // drive, on a verify command or merge gate the branch failed
	ChangesTrunkConflict = "trunk_conflict" // drive, on a trunk merge that conflicted and a session resolves
	ChangesNumberedFiles = "numbered_files" // drive, on [numbered_files] the branch clashes with or sorts below
)

// Where a usage row's counts were read from.
const (
	SourceHeadless    = "headless"    // the stream-json log of a session drover launched
	SourceInteractive = "interactive" // a Claude Code transcript
)

// Level levels, weakest first. Each subsumes the one before.
const (
	LevelClaimed  = "claimed"  // the agent's own report, captured and nothing more
	LevelReviewed = "reviewed" // a review pass ran against the code
	LevelVerified = "verified" // every numbered requirement confirmed against shipped code
)

// CodeAgent is the one reserved --code-by entry: a session drover launched.
// Every other entry is a person's name.
//
// Who wrote the code is a separate axis from the level, and it exists because
// the level alone cannot tell the two apart: an implementer grading its own
// work at the top of the ladder reads identically to an independent pass.
// Recording it is what stops a claim nobody checked from outranking every
// later correction.
const CodeAgent = "agent"

// Levels lists the audit levels in ladder order.
func Levels() []string { return []string{LevelClaimed, LevelReviewed, LevelVerified} }

// Rank orders the ladder. Zero means unknown, which never outranks anything.
func Rank(level string) int {
	switch level {
	case LevelClaimed:
		return 1
	case LevelReviewed:
		return 2
	case LevelVerified:
		return 3
	default:
		return 0
	}
}

// Malformed is a line that did not parse. They are surfaced rather than
// skipped: prompt.sh dropped them silently, so a truncated write looked
// identical to a record that was never made.
type Malformed struct {
	Line int
	Text string
	Err  error
}

// Ledger is a parsed file.
type Ledger struct {
	Path string
	Rows []Row
	// Usage is every usage row, in file order, and the only place they are.
	Usage []Row
	// Warnings is every usage_warning row, in file order.
	Warnings  []Row
	Malformed []Malformed
}

// UsageRowID is the id a usage_warning row carries. Like DriveRowID it holds
// no digit, so no namespace can mint an item under it.
const UsageRowID = "usage"

// Warned reports whether a usage_warning row already names the window and the
// reset.
func (l *Ledger) Warned(window string, resetsAt int64) bool {
	for _, r := range l.Warnings {
		if r.UsageWarning == window && r.ResetsAt == resetsAt {
			return true
		}
	}
	return false
}

// HasSession reports whether a usage row from source names the runner's
// session id. An empty source matches either.
func (l *Ledger) HasSession(source, sessionID string) bool {
	for _, r := range l.Usage {
		if r.SessionID == sessionID && (source == "" || r.Source == source) {
			return true
		}
	}
	return false
}

// HasSessionDay reports whether an interactive usage row already covers one
// session on one local date.
func (l *Ledger) HasSessionDay(sessionID, date string) bool {
	for _, r := range l.Usage {
		if r.Source == SourceInteractive && r.SessionID == sessionID && r.Date == date {
			return true
		}
	}
	return false
}

// State is what the rows resolve to for one id.
type State struct {
	Merged bool
	// MergedRunner and Report (the saved report) come from the row that last set Done.
	MergedRunner string
	Report       string
	// Implementer and Reviewer are the runners the most recent run of each
	// stage used. Empty means no run was recorded, which is not the same
	// answer as the project default and must never be rendered as one.
	Implementer string
	Reviewer    string
	// Level, By and Independent come from the most recent audit row, which is not
	// necessarily the strongest. A sweep that claimed more than it checked
	// must be correctable downward, and last-row-wins is what allows that.
	Level string
	// Auditor is the audit row's Reviewer: who the operator says looked. Empty
	// means the row named nobody, which is the only honest reading — an audit
	// never defaults the name, so a value here was typed by somebody.
	Auditor     string
	CodeBy      string
	Independent bool
	Note        string
	Date        string
}

// Audited reports whether anybody has recorded a look at all.
func (s State) Audited() bool { return s.Level != "" }

// Load reads the file. A missing file is an empty ledger, not an error — a
// project with no completions yet is a normal state.
func Load(path string) (*Ledger, error) {
	l := &Ledger{Path: path}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// Notes cite files and lines and run long; the default 64K token limit would
	// truncate one into a parse error that reads as corruption.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			l.Malformed = append(l.Malformed, Malformed{Line: n, Text: text, Err: err})
			continue
		}
		if r.Usage != nil {
			l.Usage = append(l.Usage, r)
			continue
		}
		if r.UsageWarning != "" {
			l.Warnings = append(l.Warnings, r)
			continue
		}
		if r.ID == "" {
			l.Malformed = append(l.Malformed, Malformed{Line: n, Text: text, Err: fmt.Errorf("no id")})
			continue
		}
		l.Rows = append(l.Rows, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return l, nil
}

// State resolves the rows for one id.
func (l *Ledger) State(id string) State {
	var s State
	for _, r := range l.Rows {
		if r.ID != id {
			continue
		}
		if r.Merged != nil {
			s.Merged = *r.Merged
			s.MergedRunner, s.Report = r.MergedRunner, r.Report
		}
		switch r.Stage {
		case StageImplement:
			s.Implementer = r.Runner
		case StageReview:
			s.Reviewer = r.Runner
		}
		if r.Level != "" {
			s.Level, s.CodeBy, s.Independent, s.Note, s.Date = r.Level, r.CodeBy, r.Independent, r.Note, r.Date
			// Assigned unconditionally: last-row-wins means a later row naming
			// nobody withdraws the earlier attribution rather than inheriting it.
			s.Auditor = r.Reviewer
		}
	}
	return s
}

// Counts is how many rows of each shape one item carries.
//
// It is the first reader here that counts rather than folds. State answers
// "what is true now" and resolves last-row-wins, which is right for every
// question with one current answer and erases the one this type exists for: a
// review loop that converges without terminating is correct on every pass, and
// only the total says so.
//
// ByStage is keyed rather than a field per stage so a third stage needs a
// constant and no call site. Audits sits beside it rather than in it because an
// audit is not a launch, and sharing the key space with a stage somebody adds
// later would silently merge the two counts.
type Counts struct {
	ByStage map[string]int
	Audits  int
	// ChangesRequested is how many times a review sent this item back, the
	// rounds [review] max_review_rounds bounds, and LastChanges is the date of
	// the most recent. Drive's own send-backs are counted apart, in
	// VerifySendBacks, ConflictSendBacks and NumberedSendBacks, and no field
	// below reads them: a red gate between two review rounds is not the review
	// repeating itself or carrying a finding over. RepeatedChanges marks the last two carrying
	// the same evidence digest, which is a review that has stopped finding
	// anything new.
	//
	// StillOpen marks the last two each carrying a still_open list and the two
	// sharing a requirement: two consecutive runs were sent the same finding and
	// neither closed it. Reqs is the last one's list, and StillOpenReqs the
	// requirements the two lists share. Operator is the last one's operator
	// list, the requirements it handed to the operator.
	//
	// An equal Reqs pair alone is not a repeat. A requirement such as "every
	// refused object answers 403" admits any number of distinct defects, and a
	// reviewer writing new probes each round sends the same number back while
	// the branch improves; [review] max_review_rounds is the bound on that.
	ChangesRequested int
	LastChanges      string
	RepeatedChanges  bool
	StillOpen        bool
	StillOpenReqs    []int
	Reqs             []int
	Operator         []int
	// The send-backs drive wrote on its own evidence, by ChangesSource.
	VerifySendBacks   int
	ConflictSendBacks int
	NumberedSendBacks int
	// Refusals is how many requests for changes the cap turned away over the item's history.
	// A reader deciding about one pass wants RefusedSince instead. RefusedReqs
	// is the requirements the last refused request named.
	Refusals    int
	RefusedReqs []int
	// RefusedFindings is the findings file the last refused request named,
	// empty on a row written before refusals kept one.
	RefusedFindings string
	// LastBody is the body digest from the most recent row carrying one, review
	// stage row or body row, in file order. Empty for an item no drover that
	// records digests has touched.
	LastBody string
}

// Of is the count for one stage. A stage nobody launched reads zero, and so
// does an id the ledger has never heard of.
func (c Counts) Of(stage string) int { return c.ByStage[stage] }

// Launches is every stage row, whatever the stage. Zero says drover never
// handed this item to a runner, which is not the same as no work happening:
// an operator who implemented it in their own session leaves the branch, the
// commits and the merge, and none of the three is a launch drover resolved.
// Summing ByStage rather than naming the stages keeps a stage added later from
// reading as never launched here.
func (c Counts) Launches() int {
	n := 0
	for _, v := range c.ByStage {
		n += v
	}
	return n
}

// Counts counts the stage and audit rows for one id.
//
// The two are deliberately not added together. A stage row records a launch
// drover resolved a runner for, so a project that pipes `drover review` writes
// none, and an audit row records a pass that reached a conclusion however the
// brief was delivered. One derived round number would undercount the first
// project silently; two counts labelled by what wrote them cannot.
func (l *Ledger) Counts(id string) Counts {
	c := Counts{ByStage: map[string]int{}}
	var last string
	var prevStillOpen []int
	for _, r := range l.Rows {
		if r.ID != id {
			continue
		}
		if r.Stage != "" {
			c.ByStage[r.Stage]++
		}
		if r.Level != "" {
			c.Audits++
		}
		switch {
		case r.Changes == "":
		case r.ChangesSource == ChangesVerify:
			c.VerifySendBacks++
		case r.ChangesSource == ChangesTrunkConflict:
			c.ConflictSendBacks++
		case r.ChangesSource == ChangesNumberedFiles:
			c.NumberedSendBacks++
		default:
			c.ChangesRequested++
			c.RepeatedChanges = last != "" && r.Changes == last
			c.StillOpenReqs = shared(prevStillOpen, r.StillOpen)
			c.StillOpen = len(c.StillOpenReqs) > 0
			c.Reqs, prevStillOpen = r.Reqs, r.StillOpen
			c.Operator = r.Operator
			c.LastChanges, last = r.Date, r.Changes
		}
		if r.Refused != "" {
			c.Refusals++
			c.RefusedReqs, c.RefusedFindings = r.Reqs, r.Findings
		}
		if r.Body != "" {
			c.LastBody = r.Body
		}
	}
	return c
}

// shared is the numbers in both a and b, in a's order.
func shared(a, b []int) []int {
	var out []int
	for _, n := range a {
		if slices.Contains(b, n) {
			out = append(out, n)
		}
	}
	return out
}

// RefusedSince reports whether the cap turned a request for changes away after the row count
// a caller took before the pass it is judging.
//
// The count rather than a date, because two passes of one item commonly land on
// one day and a date cannot order them. A caller holds the Counts it read before
// launching and asks this afterwards, which is the same before/after pair every
// other pass predicate already uses.
func (l *Ledger) RefusedSince(id string, before int) bool {
	return l.Counts(id).Refusals > before
}

// Pass is what the most recent drive concluded about one item's review, and
// whether the branch has moved since.
//
// stale marks a stage, audit or changes row appended after the pass row. The
// result still stands as a record of what that pass found; it is not a
// statement about this branch, and a caller acting on it would be acting on a
// review of code that has since changed.
//
// ok is false when no drive ever recorded one. That is the manual lifecycle
// rather than a failure, so a caller reads it as "nobody computed this" and
// decides for itself, the way a degraded tracker read reports what it could not
// ask rather than a zero.
func (l *Ledger) Pass(id string) (token, date string, stale, ok bool) {
	at := -1
	for i, r := range l.Rows {
		if r.ID != id || r.Pass == "" {
			continue
		}
		token, date, at, ok = r.Pass, r.Date, i, true
	}
	if !ok {
		return "", "", false, false
	}
	for _, r := range l.Rows[at+1:] {
		if r.ID == id && (r.Stage != "" || r.Level != "" || r.Changes != "") {
			stale = true
			break
		}
	}
	return token, date, stale, true
}

// Parked is the park row standing for id: the latest one, while no launch and
// no completion has been recorded after it. A launch is somebody acting on the
// park, and the row then describes a run that is no longer the latest.
func (l *Ledger) Parked(id string) (Row, bool) {
	var park Row
	found := false
	for _, r := range l.Rows {
		if r.ID != id {
			continue
		}
		switch {
		case r.Park != "":
			park, found = r, true
		case r.Stage != "", r.Merged != nil, r.Class == ClassGreen:
			found = false
		}
	}
	return park, found
}

// ClearedSincePass reports a `drover close-changes` row for id after both the most
// recent pass row and the most recent audit row, which is a block discharged
// after the review that read it as standing. False with no pass row at all.
func (l *Ledger) ClearedSincePass(id string) bool {
	pass, audit, cleared := -1, -1, -1
	for i, r := range l.Rows {
		if r.ID != id {
			continue
		}
		switch {
		case r.Pass != "":
			pass = i
		case r.Level != "":
			audit = i
		case r.Cleared:
			cleared = i
		}
	}
	return pass >= 0 && cleared > pass && cleared > audit
}

// What drive took a red check for. ClassNone is the head no check ran on at
// all, recorded beside the three so the report of it has a row too. ClassGreen
// is a head parked on its checks that drive read again at the end of the drive
// and found passing, the row that marks the item picked up after its park.
const (
	ClassInfra = "infra"
	ClassTrunk = "trunk"
	ClassCode  = "code"
	ClassNone  = "none"
	ClassGreen = "green"
)

// Reruns is how many times drive reran a red check on head for id.
func (l *Ledger) Reruns(id, head string) int {
	n := 0
	for _, r := range l.Rows {
		if r.ID == id && r.Head == head && r.Rerun {
			n++
		}
	}
	return n
}

// Heads is every commit a row recorded the branch holding outside the trunk,
// oldest first. Keyed by branch rather than id because the branch is what a
// merge check is asked about, and one template renders one name per item.
func (l *Ledger) Heads(branch string) []string {
	var out []string
	for _, r := range l.Rows {
		if r.Branch == branch && r.Head != "" {
			out = append(out, r.Head)
		}
	}
	return out
}

// PR is the pull request drover last recorded for one item, and the branch it
// was opened from. A zero says none was recorded, and is not an answer about
// the forge.
func (l *Ledger) PR(id string) (number int, branch string) {
	for _, r := range l.Rows {
		if r.ID == id && r.PR > 0 {
			number, branch = r.PR, r.Branch
		}
	}
	return number, branch
}

// IDs returns every item id the ledger mentions, sorted for stable output. A
// drive's own rows name no item and are left out.
func (l *Ledger) IDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range l.Rows {
		if r.ID == DriveRowID {
			continue
		}
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

// ParseCodeBy reads a --code-by value: agent, a person's name, or a comma
// list of them. It holds the authorship axis on its own, because two different
// claims are judged against it: how hard someone looked at an item, and whether
// an item's work resolves an issue.
func ParseCodeBy(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("authorship is required: --code-by %s, a person's name, or a comma list of them (%s,sam)", CodeAgent, CodeAgent)
	}
	var out []string
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		switch e {
		case "":
			return nil, fmt.Errorf("--code-by %q has an empty entry: name who wrote the code, %s for a session drover launched", v, CodeAgent)
		case "author", "independent":
			return nil, fmt.Errorf("--code-by names who wrote the code, not how you relate to it: %q is not accepted; pass %s for a session drover launched or a person's name", e, CodeAgent)
		}
		out = append(out, e)
	}
	return out, nil
}

// Grader is the party recording an audit or disposition: the session drover
// launched it under, if any, and the name --reviewer gave.
type Grader struct {
	Launched bool
	Session  string
	Name     string
}

// SameAs reports the first --code-by entry the grader cannot be told apart
// from, with why, or "" when the grader differs from every entry.
//
// agent resolves per session against the item's implement stage rows, so a
// review session drover launched is independent of the implement session it
// follows. A launched grader with no session id cannot be placed, so it counts
// as the author. Any other entry is compared as written
// against --reviewer, and a grader nothing names cannot be told from anybody.
func (l *Ledger) SameAs(id string, g Grader, entries []string) (entry, why string) {
	for _, e := range entries {
		switch {
		case g.Name == e:
			return e, fmt.Sprintf("--reviewer is %s", e)
		case e == CodeAgent && g.Launched && g.Session == "":
			return e, "this session drover launched carries no session id to tell it from the one that wrote the code"
		case e == CodeAgent && g.Launched && l.implementedIn(id, g.Session):
			return e, fmt.Sprintf("this session (%s) is the one drover launched to implement %s", g.Session, id)
		case e != CodeAgent && g.Name == "" && !g.Launched:
			return e, "nothing names who is auditing: pass --reviewer <name>"
		}
	}
	return "", ""
}

// implementedIn reports whether session is one an implement stage row of id
// recorded.
func (l *Ledger) implementedIn(id, session string) bool {
	for _, r := range l.Rows {
		if r.ID == id && r.Stage == StageImplement && r.Session == session {
			return true
		}
	}
	return false
}

// ValidateLevel holds a level to the ladder.
func ValidateLevel(level string) error {
	if Rank(level) == 0 {
		return fmt.Errorf("unknown audit level %q (known: %s)", level, strings.Join(Levels(), ", "))
	}
	return nil
}

// ValidateCodeBy enforces the rule the authorship axis exists for: whoever
// wrote the code cannot record verified. entry and why are SameAs's answer.
//
// An implementer that verifies its own work produces a record
// indistinguishable from an independent trace, and because the ledger is
// append-only the only way to clear it also discards the completion record.
// Rejecting it at the write path is cheaper than correcting it in prose
// afterwards.
func ValidateCodeBy(level, entry, why string) error {
	if err := ValidateLevel(level); err != nil {
		return err
	}
	if entry != "" && level == LevelVerified {
		return fmt.Errorf(
			"cannot record %q: --code-by names %s and %s. Whoever wrote the code cannot be the one to certify every requirement against it.\n"+
				"record %q or %q for what you confirmed, then have someone who did not write it record %q",
			LevelVerified, entry, why, LevelClaimed, LevelReviewed, LevelVerified)
	}
	return nil
}

// Append writes one row. The file is opened O_APPEND under an advisory lock so
// two concurrent writers interleave whole rows rather than fragments.
func Append(path string, r Row) error {
	_, err := AppendUnless(path, r, nil)
	return err
}

// AppendUnless writes r unless skip, asked of the ledger as it stands under
// Append's lock, says the record already holds it. It reports whether it wrote.
//
// The read happens under the lock because a read before it decides nothing: two
// processes recording one fact can both read the file without it, then both
// append. Under the lock the second one reads the first one's row.
func AppendUnless(path string, r Row, skip func(*Ledger) bool) (bool, error) {
	if r.ID == "" && r.Usage == nil {
		return false, fmt.Errorf("row has no id")
	}
	if r.Body != "" && (r.Cmd == "" || r.Actor == "") {
		return false, fmt.Errorf("internal: the body row for %s names no command or no actor (cmd %q, actor %q); build it with ledger.BodyRow and an Author naming both", r.ID, r.Cmd, r.Actor)
	}
	return appendLocked(path, r, skip)
}

// NextDrive allocates the next drive id: one past the highest any row in the
// ledger names, read under Append's lock. It writes nothing, so the id is
// reserved by the first row that carries it; the checkout lease is what stops a
// second drive allocating before that row lands. A ledger not yet created is
// left uncreated: it holds no drive, and a drive refused before writing a row
// leaves the tree as it found it.
func NextDrive(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return DrivePrefix + "1", nil
	}
	var next string
	_, err := appendLocked(path, Row{ID: DriveRowID}, func(l *Ledger) bool {
		high := 0
		for _, have := range l.Rows {
			high = max(high, driveNumber(have.Drive))
		}
		next = DrivePrefix + strconv.Itoa(high+1)
		return true
	})
	return next, err
}

// appendLocked writes r unless skip, reading the ledger as it stands under the
// lock, says not to, and reports whether it wrote.
func appendLocked(path string, r Row, skip func(*Ledger) bool) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create ledger directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	// Sync below is the durability point, so a Close error adds nothing.
	defer func() { _ = f.Close() }()
	unlock, err := lock(f)
	if err != nil {
		return false, fmt.Errorf("lock %s: %w", path, err)
	}
	defer unlock()

	if skip != nil {
		l, err := Load(path)
		if err != nil {
			return false, err
		}
		if skip(l) {
			return false, nil
		}
	}
	r = Stamp(r)
	// Encoding through the marshaller, never through string formatting: a note
	// carrying a quote, a backslash or a tab would otherwise produce a line that
	// parses as nothing and is skipped on the next read.
	line, err := json.Marshal(r)
	if err != nil {
		return false, fmt.Errorf("encode row: %w", err)
	}
	out := append(line, '\n')
	unterminated, err := endsUnterminated(f)
	if err != nil {
		return false, fmt.Errorf("read the end of %s: %w", path, err)
	}
	if unterminated {
		out = append([]byte{'\n'}, out...)
	}
	if _, err := f.Write(out); err != nil {
		return false, fmt.Errorf("append to %s: %w", path, err)
	}
	return true, f.Sync()
}

// endsUnterminated reports whether f holds a last line with no newline, as a
// row appended by hand with printf or an editor can leave it. Appending to that
// would join the two rows into one line that parses as neither.
func endsUnterminated(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// Stamp defaults a row's date and time to now, leaving whatever the writer set.
// A caller that keeps an in-memory copy of what it appended stamps first, so
// the copy and the line on disk carry the same instant.
func Stamp(r Row) Row {
	now := time.Now()
	if r.Date == "" {
		r.Date = now.Format("2006-01-02")
	}
	if r.TS == "" {
		r.TS = now.Format(time.RFC3339)
	}
	return r
}

// At is the instant the row was written, false for a row carrying no ts or one
// that does not parse: a day-only row has no time of day to report.
func (r Row) At() (time.Time, bool) {
	if r.TS == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, r.TS)
	return t, err == nil
}

// Today is the date stamp rows carry.
func Today() string { return time.Now().Format("2006-01-02") }
