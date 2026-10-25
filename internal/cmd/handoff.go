package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

// inputBinary is the input a resume onto a replaced binary names as changed.
const inputBinary = "binary"

var (
	// driveKeepBinary is --keep-binary: resume on the binary the drive started
	// with, whatever is on disk now.
	driveKeepBinary bool
	// driveTakeover and driveLeaseFD are the hidden flags a drive re-executing
	// itself adds to its own arguments: its id, and the descriptor its lease is
	// open on. driveResumeCheck is the hidden flag of the probe it runs first.
	driveTakeover    string
	driveLeaseFD     int
	driveResumeCheck bool
	// driveExec replaces the process. Tests stub it; nothing else does.
	driveExec = syscall.Exec
	// startedFrom is the binary this drive was launched from, read at launch.
	startedFrom binaryID
)

// binaryID is an executable as a drive compares it: the path it was started
// from and the file there. An install writes a new file or rewrites the old
// one, and either moves the inode, the size or the modification time, so the
// file's stat says whether it changed without reading tens of megabytes. The
// version string alone cannot, since every build outside the Makefile reports
// dev.
//
// version is what the binary reports, known only for the one running: the one
// replacing it names its own once it runs.
type binaryID struct {
	path    string
	info    os.FileInfo
	version string
}

// String is the version and the file's modification time, the form a
// re-exec's start row and its messages name each binary by.
func (b binaryID) String() string {
	name := b.version
	if name == "" {
		name = "the binary"
	}
	if b.info == nil {
		return name
	}
	return fmt.Sprintf("%s (built %s)", name, b.info.ModTime().UTC().Format(time.RFC3339))
}

// launchedBinary resolves the path this process was started from: argv[0]
// as the shell found it, not os.Executable, which follows symlinks to the file
// the old binary was and on Linux reads as deleted once an install replaces it.
func launchedBinary() (binaryID, error) {
	path := os.Args[0]
	if !strings.ContainsRune(path, filepath.Separator) {
		found, err := exec.LookPath(path)
		if err != nil {
			return binaryID{}, err
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return binaryID{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return binaryID{}, err
	}
	return binaryID{path: abs, info: info, version: version}, nil
}

// replaced is the binary now at the path this drive started from, when it is
// not the file the drive was started from. A path that cannot be read is no
// binary to re-execute onto, so the drive resumes where it is.
func (b binaryID) replaced() (binaryID, bool) {
	if b.info == nil {
		return binaryID{}, false
	}
	now, err := os.Stat(b.path)
	if err != nil || !now.Mode().IsRegular() {
		return binaryID{}, false
	}
	if os.SameFile(b.info, now) && now.Size() == b.info.Size() && now.ModTime().Equal(b.info.ModTime()) {
		return binaryID{}, false
	}
	return binaryID{path: b.path, info: now}, true
}

// handoff is what a drive re-executing itself leaves in its lease for the
// process that takes its place: what that process cannot read back from the
// flags it is started with or from the ledger.
type handoff struct {
	From       string           `json:"from"`
	Deadline   string           `json:"deadline,omitempty"`
	Dispatched int              `json:"dispatched"`
	Outcomes   []ledger.Outcome `json:"outcomes,omitempty"`
	Findings   *watchState      `json:"findings,omitempty"`
	// Waived is every usage hold a resume released, so the resume that
	// re-executed the drive does not leave it held again on the same reading.
	Waived []string `json:"waived,omitempty"`
	// Operator is each dep the drive holds its dependents for, with its park.
	Operator map[string]string `json:"operator,omitempty"`
}

// takeoverArgs is args less the flags a previous re-exec or probe added, with
// extra on the end.
func takeoverArgs(args []string, extra ...string) []string {
	out := make([]string, 0, len(args)+len(extra))
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(args[i], "=")
		switch name {
		case "--takeover", "--lease-fd":
			if !inline {
				i++
			}
			continue
		case "--resume-check":
			continue
		}
		out = append(out, args[i])
	}
	return append(out, extra...)
}

// probeResume runs the replaced binary over this drive's own flags to load
// the config, the templates and the preflight, as the drive would after the
// exec. A failure there refuses the resume while this process still holds
// everything; after the exec, it would end the drive.
//
// --ignore-stale because the operator's own --resume already ran the staleness
// check, or chose to skip it with -I.
func probeResume(to binaryID) error {
	args := takeoverArgs(os.Args[1:], "--resume-check", "--ignore-stale")
	out, err := exec.Command(to.path, args...).CombinedOutput() // #nosec G204 -- the path this drive was started from, over its own arguments
	if err != nil {
		return fmt.Errorf("%s at %s would not load this project (%v): %s", to, to.path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// reexecReady reports whether a resume is waiting to re-execute the drive and
// every running item now holds at a boundary, which is the only moment no
// goroutine is in a session, a gate or the trunk stage. It leaves q.mu held
// when it says yes, so nothing leaves its hold before the exec.
func (q *driveSched) reexecReady(running int) bool {
	q.mu.Lock()
	if q.paused && q.reexec != "" && q.stop == nil && len(q.holds) == running {
		return true
	}
	q.mu.Unlock()
	return false
}

// reexecPatience is how long a re-exec waits on running items that do not hold
// before the drive says which, and how often it says so again.
var reexecPatience = time.Minute

// bySeq is the running items in the order they were dispatched.
func bySeq(running map[int]queued) []queued {
	return slices.SortedFunc(maps.Values(running), func(a, b queued) int { return a.seq - b.seq })
}

// reexecWaiting is each running item a re-exec has waited on for reexecPatience
// without it holding, with the stage it was last seen in, when the drive is due
// to say so: once the wait passes reexecPatience and once per reexecPatience
// after. retract reports that an earlier report no longer stands.
func (q *driveSched) reexecWaiting(running map[int]queued, now time.Time) (waiting []WaitingItem, waited time.Duration, retract bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	waited = now.Sub(q.reexecSince)
	if !q.paused || q.reexec == "" || q.stop != nil || len(q.holds) >= len(running) || waited < reexecPatience {
		retract, q.reexecSaid = !q.reexecSaid.IsZero(), time.Time{}
		return nil, 0, retract
	}
	if !q.reexecSaid.IsZero() && now.Sub(q.reexecSaid) < reexecPatience {
		return nil, 0, false
	}
	q.reexecSaid = now
	for _, r := range bySeq(running) {
		id := r.it.ID.Bare()
		if _, ok := q.holds[id]; !ok {
			waiting = append(waiting, WaitingItem{ID: id, Stage: events.stageOf(id)})
		}
	}
	return waiting, waited, false
}

// sayReexecWaiting reports a re-exec that is not progressing, on stderr, as a
// reexec_waiting event and in the lease, where status reads it; and takes the
// lease's line down once the condition no longer holds.
func sayReexecWaiting(q *driveSched, running map[int]queued, now time.Time) {
	waiting, waited, retract := q.reexecWaiting(running, now)
	if retract && held != nil {
		_ = held.Update(func(l *lease.Lease) { l.ReexecWaiting = "" })
	}
	if len(waiting) == 0 {
		return
	}
	q.mu.Lock()
	to := q.reexec
	q.mu.Unlock()
	line := reexecWaitingLine(to, waited, waiting)
	fmt.Fprintf(os.Stderr, "· %s\n", line)
	events.emit(DriveEvent{Event: evReexecWaiting, Reason: line, Waiting: waiting})
	if held != nil {
		_ = held.Update(func(l *lease.Lease) { l.ReexecWaiting = line })
	}
}

// reexecWaitingLine names the items a re-exec waits on and the stage each is in.
func reexecWaitingLine(to string, waited time.Duration, waiting []WaitingItem) string {
	names := make([]string, 0, len(waiting))
	for _, w := range waiting {
		stage := w.Stage
		if stage == "" {
			stage = "dispatched, no stage yet"
		}
		names = append(names, fmt.Sprintf("%s (in %s)", w.ID, stage))
	}
	return fmt.Sprintf("the re-exec onto %s has waited %s for %s to hold at a stage boundary; "+
		"it goes on once each does, and drive --stop parks them instead",
		to, waited.Truncate(time.Second), strings.Join(names, ", "))
}

// reexec replaces this process with the binary now on disk, over its own
// arguments plus its drive id and the descriptor its lease is open on. The
// lease stays locked through the exec, so no other drive can take the tree in
// between, and the new process reads the held items, the queue and the
// handoff out of it. Called with q.mu held by reexecReady; it returns only
// when the exec failed, with the drive still paused on the binary it has.
func (s *session) reexec(q *driveSched, pending []queued, running map[int]queued, dispatched int, deadline time.Time, watch *findingWatch, operator map[string]string) {
	failed := func(err error) {
		q.reexec = ""
		q.mu.Unlock()
		_ = held.Update(func(l *lease.Lease) { l.Reexec, l.Handoff = "", nil })
		fmt.Fprintf(os.Stderr, "· the re-exec onto the replaced binary failed: %v\n"+
			"  The drive stays paused on %s; drive --resume --keep-binary resumes it there\n", err, startedFrom)
	}
	d := activeDrive.Load()
	if d == nil {
		failed(errors.New("the drive has no id to carry across"))
		return
	}
	ho := handoff{From: startedFrom.String(), Dispatched: dispatched, Findings: watch.state(), Waived: q.usage.waivedKeys(), Operator: operator}
	if !deadline.IsZero() {
		ho.Deadline = deadline.Format(time.RFC3339Nano)
	}
	d.mu.Lock()
	ho.Outcomes = slices.Clone(d.outcomes)
	d.mu.Unlock()
	raw, err := json.Marshal(ho)
	if err != nil {
		failed(err)
		return
	}
	// In dispatch order, which the new process dispatches them in again, so
	// each trunk turn still follows every item dispatched before it.
	var holds []lease.Hold
	for _, r := range bySeq(running) {
		holds = append(holds, q.holds[r.it.ID.Bare()])
	}
	queue := make([]string, 0, len(pending))
	for _, p := range pending {
		queue = append(queue, p.it.ID.Bare())
	}
	fd, err := held.Inherit()
	if err == nil {
		err = held.Update(func(l *lease.Lease) { l.Queue, l.Holds, l.Handoff = queue, holds, raw })
	}
	if err != nil {
		failed(err)
		return
	}
	fmt.Fprintf(os.Stderr, "· re-executing drive %s onto %s; %d held, %d queued\n", d.id, q.reexec, len(holds), len(queue))
	// Whatever coalescing or quiet hours hold goes out now: the process that
	// holds it is about to be replaced, and its timers with it.
	events.flushNotify()
	argv := append([]string{os.Args[0]}, takeoverArgs(os.Args[1:], "--takeover", d.id, "--lease-fd", strconv.Itoa(int(fd)), "--ignore-stale")...)
	failed(driveExec(startedFrom.path, argv, os.Environ()))
}

// adoptLease takes over the lease the process this one replaced held open on
// --lease-fd, in place of acquiring one.
func (s *session) adoptLease() (func(), error) {
	path, err := s.leasePath()
	if err != nil {
		return nil, err
	}
	h, err := lease.Adopt(uintptr(driveLeaseFD), path) // #nosec G115 -- a descriptor number this process's predecessor printed
	if err != nil {
		return nil, fmt.Errorf("--takeover: %w", err)
	}
	held = h
	return func() {
		_ = h.Release()
		held = nil
	}, nil
}

// driveTakeoverRun is the drive a re-exec started: the id, the queue, the held
// items and the deadline of the process it replaced, read out of the lease.
// Each held item goes on from the boundary it held at, and one whose [verify]
// command changed under it runs the new one there first.
func (s *session) driveTakeoverRun(ctx context.Context, reviewer string) error {
	l := held.Current()
	var ho handoff
	if err := json.Unmarshal(l.Handoff, &ho); err != nil {
		return fmt.Errorf("--takeover: the lease carries no handoff from the drive this replaced: %w", err)
	}
	d := &runningDrive{id: driveTakeover, started: true, outcomes: ho.Outcomes}
	activeDrive.Store(d)
	var deadline time.Time
	if ho.Deadline != "" {
		t, err := time.Parse(time.RFC3339Nano, ho.Deadline)
		if err != nil {
			return fmt.Errorf("--takeover: deadline %q: %w", ho.Deadline, err)
		}
		deadline = t
	}
	var pending []queued
	var ids []string
	for _, h := range l.Holds {
		it, err := s.item(h.ID)
		if err != nil {
			return err
		}
		q := newQueued(it, true)
		q.hold = &h
		pending, ids = append(pending, q), append(ids, h.ID)
	}
	for _, id := range l.Queue {
		it, err := s.item(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "· %s was queued in the drive this replaced and will not load: %v\n", id, err)
			continue
		}
		pending, ids = append(pending, newQueued(it, true)), append(ids, id)
	}
	if err := s.record(ledger.Row{ID: ledger.DriveRowID, Label: driveLabel, Queued: ids, Jobs: driveJobs,
		Version: startedFrom.String(), Replaced: ho.From}); err != nil {
		return err
	}
	if err := held.Update(func(l *lease.Lease) {
		l.Paused, l.Reexec, l.ReexecWaiting, l.Handoff, l.Holds, l.Running = false, "", "", nil, nil, nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "· drive %s re-executed onto %s, replacing %s; %d held items go on, then the queue\n",
		d.id, startedFrom, ho.From, len(l.Holds))
	events.emit(DriveEvent{Event: evResumed, Changed: []string{inputBinary}})
	return s.driveQueued(ctx, pending, reviewer, deadline, ho.Dispatched, s.adoptFindingWatch(ho.Findings), ho.Waived, ho.Operator)
}

// resumeHeld sends an item a re-exec carried over to the boundary it held at:
// its review, or the trunk stage, with the verify command rerun first when
// the new process reads a different one than the stage before was told to
// pass. It reports where the item goes on from: review true for the review,
// finish true for the trunk stage, neither for its next implement run.
func (s *session) resumeHeld(it backlog.Item) (review, regate, land bool) {
	h := s.resumeAt
	if h == nil {
		return false, false, false
	}
	regate = h.At != ledger.StageImplement && h.Verify != prompts.SelectVerify(s.cfg.Verify, it.Meta)
	return h.At == ledger.StageReview, regate, h.At == stageCatchUp
}
