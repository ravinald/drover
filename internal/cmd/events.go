package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

// The event types `drive --events` writes. Documented with a sample each in
// docs/automation.md; a wrapper keys on these, so renaming one is a breaking
// change to the stream.
const (
	// evStarted is the drive holding its lease with its queue read: what it
	// will run, how many at once, and until when.
	evStarted       = "started"
	evQueued        = "queued"
	evDispatched    = "dispatched"
	evStage         = "stage"
	evParked        = "parked"
	evFiled         = "filed"
	evNeedsOperator = "needs_operator"
	// evNeedsSupervisor is an action at supervisor policy that drive met and
	// left for the supervising session, which reports what it did about it.
	evNeedsSupervisor = "needs_supervisor"
	evMerged          = "merged"
	evCheck           = "check"
	evExit            = "exit"
	evPaused          = "paused"
	evResumed         = "resumed"
	// evUsageWarning is a [usage] notify level crossed, sent once per window
	// per reset. It pauses nothing.
	evUsageWarning = "usage_warning"
)

// The stages a stage event names. implement and review are ledger stages; the
// rest are steps of the trunk stage, named here because nothing else names them.
const (
	stageCatchUp = "update_branch"
	stageGate    = "gate"
	stagePR      = "pr"
	stageMerge   = "merge"
	stageFinish  = "finish"
)

// DriveEvent is one line of `drive --events`. Event is always set, which is how
// a reader tells an event from the per-item report the same stream carries.
type DriveEvent struct {
	Event string `json:"event"`
	TS    string `json:"ts"`
	// Seq is the event's line in the drive's event file, from 1, which is the
	// cursor watch --after takes: ts is one second wide and two events share
	// one often. Absent only where Drive is.
	Seq int `json:"seq,omitempty"`
	// Drive is the drive's id, taken with the lease. Absent only on the exit of
	// a drive whose id would not allocate, because the ledger would not open.
	Drive string `json:"drive,omitempty"`
	// Project is the project's name as `drover projects` lists it, so a
	// notifier serving several projects need not work it out from where it runs.
	Project string `json:"project"`
	ID      string `json:"id,omitempty"`
	// Action is the catalog key that produced the event, where one did.
	Action string `json:"action,omitempty"`
	Stage  string `json:"stage,omitempty"`
	// Log is the session log a stage launching a session streams to, as the
	// launch banner prints it: relative to the project root, or under ~/.
	Log    string `json:"log,omitempty"`
	Park   string `json:"park,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Detail is the park's detail token, on parked, as the item's report
	// carries it.
	Detail string `json:"detail,omitempty"`
	// Policy is the action's policy in this project, on parked and
	// needs_operator.
	Policy string `json:"policy,omitempty"`
	// Run is what the operator has to do, on needs_operator, and what the
	// supervising session does, on needs_supervisor.
	Run  string `json:"run,omitempty"`
	PR   int    `json:"pr,omitempty"`
	Slot int    `json:"slot,omitempty"`
	// Result is what a catch-up of another open pull request came to, on its
	// update_branch stage event: a CatchUp gate word, or level, conflict or left.
	Result string `json:"result,omitempty"`
	// Branch, Tree and Paths say where a hand-off has to work: the branch, the
	// worktree left standing for it, and the files a merge conflicts on.
	Branch string   `json:"branch,omitempty"`
	Tree   string   `json:"tree,omitempty"`
	Paths  []string `json:"paths,omitempty"`
	// Position is an appended id's 1-based place among the items waiting, on
	// queued alone.
	Position int `json:"position,omitempty"`
	// Head, Check, Class and Evidence are one red check drive classified, or a
	// head no check ran on, on check alone, as the ledger row records them.
	// Evidence is also a park's, on parked.
	Head     string `json:"head,omitempty"`
	Check    string `json:"check,omitempty"`
	Class    string `json:"class,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Code is the drive's exit status, on exit alone.
	Code *int `json:"code,omitempty"`
	// Summary is what the drive left done and undone, on exit alone.
	Summary *ExitSummary `json:"summary,omitempty"`
	// Queue, Jobs and Deadline are the drive's queue in order, its --jobs, and
	// the instant --for or --until bounds it at in local time, RFC 3339, on
	// started alone. Deadline is absent on an unbounded drive.
	Queue    []string `json:"queue,omitempty"`
	Jobs     int      `json:"jobs,omitempty"`
	Deadline string   `json:"deadline,omitempty"`
	// Title is the merged pull request's title, on merged alone.
	Title string `json:"title,omitempty"`
	// Changed is which inputs a resume reloaded differ from those before it,
	// on resumed alone: config, prompt, review, verify, binary.
	Changed []string `json:"changed,omitempty"`
	// Window, Remaining and ResetsAt are the usage level behind a
	// usage_warning, and behind the paused, needs_operator and resumed that
	// usage_admission emits: weekly, five_hour, overage or overage_usd, the
	// percent of that window left (absent for overage, which has no figure),
	// and its reset in local time, RFC 3339.
	Window    string `json:"window,omitempty"`
	Remaining *int   `json:"remaining,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
	// Kind, Ref, FiledBy, Owners, Coverage, BlockedOn and Ambiguous are one
	// finding a session filed, on filed and on the triage_finding stop, as the
	// filing item's report carries it.
	Kind      string              `json:"kind,omitempty"`
	Ref       string              `json:"ref,omitempty"`
	FiledBy   []string            `json:"filed_by,omitempty"`
	Owners    []string            `json:"owners,omitempty"`
	Coverage  string              `json:"coverage,omitempty"`
	BlockedOn map[string][]string `json:"blocked_on,omitempty"`
	Ambiguous bool                `json:"ambiguous,omitempty"`
	// DriveTotals is the drive's findings, on exit alone, and absent on the
	// exit of a drive that never reached its queue.
	*DriveTotals
}

// eventSink is where a drive's events go: stdout under --events, and each
// [notify] destination that routes the event, whatever the flags.
//
// Stage events arrive from every slot's goroutine, so writes are serialized
// here. The writer is captured when the sink is made, because finishMerged points
// os.Stdout at stderr while finish runs and an event emitted from another slot in
// that window would otherwise land on the wrong stream.
type eventSink struct {
	mu       sync.Mutex
	w        io.Writer
	cfg      *config.Config
	notifier *notifier
	// file is the drive's event file (driveEventsPath), opened for the drive
	// id it carries. broken records that a write to it failed and was
	// reported, after which the drive stops trying: the file is how a process
	// that did not launch the drive follows it, and never a reason to stop it.
	file   *os.File
	broken bool
	// seq is the last line the event file holds, as this process knows it.
	seq int
	// totals is what the exit event reports about findings, and summary what
	// it reports about items, both set once the drive's queue is done.
	totals  *DriveTotals
	summary *ExitSummary
}

// events is the running drive's sink, nil outside a drive that launches.
var events *eventSink

// driveEvents is --events.
var driveEvents bool

func newEventSink(cfg *config.Config, stream bool) *eventSink {
	es := &eventSink{cfg: cfg, notifier: newNotifier(cfg)}
	if stream {
		es.w = os.Stdout
	}
	return es
}

// emit writes one event, stamping it, to stdout under --events and to the
// drive's event file whatever the flags. A write that fails is dropped: the
// per-item report is still the account of record, and an event stream nobody is
// reading must not stop the drive that feeds it.
func (es *eventSink) emit(e DriveEvent) {
	if es == nil {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339)
	e.Project = es.cfg.Project
	if d := activeDrive.Load(); d != nil {
		e.Drive = d.id
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if e.Drive != "" {
		es.openLocked(e.Drive, false)
		es.seq++
		e.Seq = es.seq
	}
	line, err := json.Marshal(e)
	if err == nil {
		line = append(line, '\n')
		if es.w != nil {
			_, _ = es.w.Write(line)
		}
		es.toFile(line)
	}
	es.notifier.route(e)
	if e.Event == evExit {
		es.closeFile()
		es.notifier.close()
	}
}

// settle sends what this sink's destinations still hold and waits for them,
// for a sink that will see no exit: one made outside a drive, or a drive about
// to re-execute, whose goroutines go with the process.
func (es *eventSink) settle() {
	if es == nil {
		return
	}
	es.notifier.close()
}

// flushNotify sends what coalescing and quiet hours hold now and waits for it,
// leaving the destinations open for a drive that goes on.
func (es *eventSink) flushNotify() {
	if es == nil {
		return
	}
	es.notifier.flush()
}

// forge names the repository a pull request's Click header links under,
// resolved through gh the first time a message needs it.
func (es *eventSink) forge(labels gh.Labels) {
	if es == nil {
		return
	}
	es.notifier.forge = func() string {
		u, err := gh.RepoURL(labels)
		if err != nil {
			return ""
		}
		return u
	}
}

// openFile starts the event file of a drive that just took its id, emptying
// whatever an earlier drive under that id in a ledger since reset left there:
// attach stops at the first exit it reads. A drive that re-executed itself
// keeps its id and opens the file again on its first event, appending.
func (es *eventSink) openFile(id string) {
	if es == nil {
		return
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	es.openLocked(id, true)
}

// openLocked opens the drive's event file if it is not open, and sets seq to
// the lines it holds: none when fresh empties it, else those an earlier process
// under this id wrote. A last line cut short (the process died mid-write) is
// ended first, so the next event starts a line of its own and is still the line
// its seq names. Called with mu held. A failure is reported once and drops
// every later line.
func (es *eventSink) openLocked(id string, fresh bool) {
	if es.broken || es.file != nil {
		return
	}
	path, err := driveEventsPath(es.cfg.Project, id)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o750)
	}
	if err == nil {
		flags := os.O_CREATE | os.O_RDWR | os.O_APPEND
		if fresh {
			flags |= os.O_TRUNC
		}
		es.file, err = os.OpenFile(path, flags, 0o600) // #nosec G304 -- under the per-user state directory
	}
	var held []byte
	if err == nil {
		held, err = io.ReadAll(es.file)
	}
	if err == nil && len(held) > 0 && held[len(held)-1] != '\n' {
		held = append(held, '\n')
		_, err = es.file.Write([]byte{'\n'})
	}
	if err != nil {
		es.fileFailed(err)
		return
	}
	es.seq = bytes.Count(held, []byte{'\n'})
}

// toFile appends one line to the drive's event file, which openLocked opened.
// Called with mu held.
func (es *eventSink) toFile(line []byte) {
	if es.broken || es.file == nil {
		return
	}
	if _, err := es.file.Write(line); err != nil {
		es.fileFailed(err)
	}
}

func (es *eventSink) fileFailed(err error) {
	es.broken = true
	es.closeFile()
	fmt.Fprintf(os.Stderr, "· the drive's event file would not write (%v); the drive goes on, attach and watch see nothing more from it, and later failures are not reported\n", err)
}

func (es *eventSink) closeFile() {
	if es.file != nil {
		_ = es.file.Close()
		es.file = nil
	}
}

// reconfigure points the sink at the config a resume loaded, so a policy and
// the destinations are read from the file as it now stands.
func (es *eventSink) reconfigure(cfg *config.Config) {
	if es == nil {
		return
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	es.cfg = cfg
	es.notifier.reload(cfg)
}

// stage is the event for an item reaching one step.
func (es *eventSink) stage(id, stage string) {
	es.emit(DriveEvent{Event: evStage, ID: id, Stage: stage})
}

// session is the stage event for a step that launches a session, naming the
// log it streams to.
func (es *eventSink) session(id, stage, log string) {
	es.emit(DriveEvent{Event: evStage, ID: id, Stage: stage, Log: log})
}

// notifyWait bounds one notifier run. A notifier that hangs would otherwise
// hold the dispatcher, and with it every item waiting for a slot.
const notifyWait = 30 * time.Second

// runArgvNotifier runs one notifier argv with the payload on stdin as one JSON
// line, bounded at notifyWait, and returns what it printed, trimmed.
func runArgvNotifier(dir string, argv []string, p NotifyPayload) (string, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv is project config, as [drive] merge_gate is
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// itemEvents emits what one finished item amounted to: its park, the operator
// stop that park is at operator policy, and its merge.
func (es *eventSink) itemEvents(d DriveItem) {
	if es == nil {
		return
	}
	if d.Outcome == outcomeParked {
		ev := DriveEvent{Event: evParked, ID: d.ID, Park: d.Park, Reason: d.Reason, Detail: d.Detail, Evidence: d.Evidence, PR: d.PR, Slot: d.Slot}
		if key := parkAction(d); key != "" {
			p, _ := es.cfg.Policy(key)
			ev.Action, ev.Policy = key, string(p)
		}
		es.emit(ev)
		if ev.Action != "" && config.Policy(ev.Policy) == polOperator && !d.handed {
			run := ""
			switch {
			case ev.Action == "auth_expired" && d.login != "":
				run = d.login + ", then drover drive " + d.ID
			case ev.Action == "signoff":
				run = d.signoff
			}
			es.needsOperator(d.ID, ev.Action, d.Reason, run, d.PR)
		}
	}
	if d.Merged {
		es.emit(DriveEvent{Event: evMerged, ID: d.ID, PR: d.PR, Title: d.ID + ": " + d.Title})
	}
}

// needsOperator emits the stop for one action at operator policy. run is what
// the operator has to do where the park knows better than the catalog's
// sentence, and empty to take that sentence. pr is the pull request the stop
// concerns, 0 for none, and is what a notifier links the message to.
func (es *eventSink) needsOperator(id, key, reason, run string, pr int) {
	if run == "" {
		run = strings.ReplaceAll(action(key).operator, "{id}", id)
	}
	es.emit(DriveEvent{Event: evNeedsOperator, ID: id, Action: key, Policy: string(polOperator),
		Reason: reason, Run: run, PR: pr})
}

// handOff emits the stop for an action drive leaves to somebody else:
// needs_supervisor at supervisor, needs_operator at operator, and nothing at
// auto or off, where drive acts or nobody does. ev carries the item and
// whatever says where to work.
func (es *eventSink) handOff(key string, p config.Policy, ev DriveEvent) {
	if es == nil {
		return
	}
	a := action(key)
	ev.Action, ev.Policy = key, string(p)
	switch p {
	case polSupervisor:
		ev.Event, ev.Run = evNeedsSupervisor, strings.ReplaceAll(a.supervisor, "{id}", ev.ID)
	case polOperator:
		ev.Event, ev.Run = evNeedsOperator, strings.ReplaceAll(a.operator, "{id}", ev.ID)
	default:
		return
	}
	es.emit(ev)
}

// filed emits one finding a session filed. A scaffold is the
// write_scaffold_requirements action, which stops for the operator at operator,
// and keeps the id it always carried: the scaffold. A register row carries the
// filing item as its id when exactly one item was running, and goes on to
// triage_finding when nothing is scheduled to carry it.
func (es *eventSink) filed(f FiledFinding) {
	if es == nil {
		return
	}
	ev := DriveEvent{Event: evFiled, Kind: f.Kind, Ref: f.Ref, FiledBy: f.FiledBy, Owners: f.Owners,
		Coverage: f.Coverage, BlockedOn: f.BlockedOn, Ambiguous: f.Ambiguous}
	scaffold := f.Kind == findingScaffold
	switch {
	case scaffold:
		ev.ID = f.Ref
		key := "write_scaffold_requirements"
		p, _ := es.cfg.Policy(key)
		ev.Action, ev.Policy = key, string(p)
	case len(f.FiledBy) == 1:
		ev.ID = f.FiledBy[0]
	}
	es.emit(ev)
	switch {
	case scaffold && config.Policy(ev.Policy) == polOperator:
		es.needsOperator(f.Ref, ev.Action, "a session filed this scaffold, and its requirements are unwritten", "", 0)
	case !scaffold && f.uncovered():
		es.triage(f)
	}
}

// triage hands a register row nothing will carry to whoever triage_finding
// names: the operator, the supervising session, or nobody at off.
func (es *eventSink) triage(f FiledFinding) {
	key := "triage_finding"
	p, _ := es.cfg.Policy(key)
	by := "no item"
	if len(f.FiledBy) > 0 {
		by = strings.Join(f.FiledBy, " ")
	}
	ev := DriveEvent{ID: strings.Join(f.FiledBy, " "), Action: key, Policy: string(p),
		Kind: f.Kind, Ref: f.Ref, FiledBy: f.FiledBy, Owners: f.Owners, Coverage: f.Coverage,
		BlockedOn: f.BlockedOn, Ambiguous: f.Ambiguous,
		Reason: fmt.Sprintf("follow-up #%s, filed by %s, is %s: %s", f.Ref, by, f.Coverage, coverageMeaning[f.Coverage])}
	ev.Reason += blockedClause(f.BlockedOn)
	if len(f.FiledBy) != 1 {
		ev.ID = ""
	}
	a := action(key)
	switch p {
	case polOperator:
		ev.Event, ev.Run = evNeedsOperator, strings.ReplaceAll(a.operator, "{ref}", f.Ref)
	case polSupervisor:
		ev.Event, ev.Run = evNeedsSupervisor, strings.ReplaceAll(a.supervisor, "{ref}", f.Ref)
	default:
		return
	}
	es.emit(ev)
}

// coverageMeaning is a coverage nothing will carry, in the words a stop names it.
var coverageMeaning = map[string]string{
	coverageBlocked:  "every owner that has not merged is blocked",
	coverageOrphaned: "every owner has merged, so no run is left to carry it",
	coverageUnowned:  "no owner link names an owner, so no run carries it",
}

// setSummary records what the exit event reports about the drive's items.
func (es *eventSink) setSummary(s *ExitSummary) {
	if es == nil {
		return
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	es.summary = s
}

// setTotals records the findings the exit event reports.
func (es *eventSink) setTotals(t DriveTotals) {
	if es == nil {
		return
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	es.totals = &t
}

// exit emits the drive's last event.
func (es *eventSink) exit(err error) {
	if es == nil {
		return
	}
	code := Code(err)
	es.mu.Lock()
	totals, summary := es.totals, es.summary
	es.mu.Unlock()
	if summary == nil {
		summary = &ExitSummary{Ended: endedDrained}
	}
	sum := *summary
	if err != nil && sum.Ended == endedDrained {
		sum.Ended = endedFault
	}
	if d := activeDrive.Load(); d != nil {
		sum.Weighted = driveWeighted(es.cfg.Abs(es.cfg.Paths.Ledger), d.id)
	}
	ev := DriveEvent{Event: evExit, Code: &code, DriveTotals: totals, Summary: &sum}
	if err != nil {
		ev.Reason = err.Error()
	}
	es.emit(ev)
}
