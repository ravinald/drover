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

// reexec replaces this process with the binary now on disk, over its own
// arguments plus its drive id and the descriptor its lease is open on. The
// lease stays locked through the exec, so no other drive can take the tree in
// between, and the new process reads the held items, the queue and the
// handoff out of it. Called with q.mu held by reexecReady; it returns only
// when the exec failed, with the drive still paused on the binary it has.
func (s *session) reexec(q *driveSched, pending []queued, running map[int]queued, dispatched int, deadline time.Time, watch *findingWatch) {
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
	ho := handoff{From: startedFrom.String(), Dispatched: dispatched, Findings: watch.state(), Waived: q.usage.waivedKeys()}
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
	var holds []lease.Hold
	for _, n := range slices.Sorted(maps.Keys(running)) {
		holds = append(holds, q.holds[running[n].it.ID.Bare()])
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
		l.Paused, l.Reexec, l.Handoff, l.Holds, l.Running = false, "", nil, nil, nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "· drive %s re-executed onto %s, replacing %s; %d held items go on, then the queue\n",
		d.id, startedFrom, ho.From, len(l.Holds))
	events.emit(DriveEvent{Event: evResumed, Changed: []string{inputBinary}})
	return s.driveQueued(ctx, pending, reviewer, deadline, ho.Dispatched, s.adoptFindingWatch(ho.Findings), ho.Waived)
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
