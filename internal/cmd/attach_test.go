package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// ownState points the per-user state directory at a directory of this test's
// own, so the event files it reads are the ones its drives wrote: fixtures
// share a project name, and each one's drives are D1 and up.
func ownState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	poll := followPoll
	followPoll = 20 * time.Millisecond
	t.Cleanup(func() { followPoll = poll })
	return dir
}

func fixtureEvents(t *testing.T, id string) string {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := driveEventsPath(s.cfg.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func readEvents(t *testing.T, path string) []DriveEvent {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own state directory
	if err != nil {
		t.Fatal(err)
	}
	var evs []DriveEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e DriveEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("event file line %q: %v", line, err)
		}
		evs = append(evs, e)
	}
	return evs
}

// asDroverCmd runs this test binary as drover, from a stand-in script, with
// args.
func asDroverCmd(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "drover")
	asDrover(t, bin, 1)
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), envAsDrover+"=1")
	return c
}

// A detached drive is in a session of its own: the launcher returns once the
// drive holds the lease, and the drive goes on after the launcher has exited,
// writing every event to its file and its output to its log.
func TestADetachedDriveOutlivesItsLauncherAndWritesItsEventFile(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", release)

	launcher := asDroverCmd(t, "drive", "--detach", "1", "--json", "--events")
	out, err := launcher.CombinedOutput()
	if err != nil {
		t.Fatalf("drive --detach: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "detached drive D1 (pid ") {
		t.Fatalf("drive --detach printed no drive id:\n%s", out)
	}
	l, busy, err := lease.Read(mustLeasePath(t))
	if err != nil || !busy || l.Drive != "D1" || l.PID == launcher.Process.Pid {
		t.Fatalf("after the launcher exited the lease reads busy=%v %+v (%v), want D1 held by another pid", busy, l, err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(l.PID); err == nil {
			_ = p.Kill()
		}
	})
	events := fixtureEvents(t, "D1")
	if !strings.Contains(string(out), events) || !strings.Contains(string(out), strings.TrimSuffix(events, ".jsonl")+".log") {
		t.Errorf("drive --detach did not print the event file and the log:\n%s", out)
	}

	// Held in its implement run until now, so all that follows happens with
	// the launcher gone.
	write(t, release, "")
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(50 * time.Millisecond) {
		if _, busy, _ := lease.Read(mustLeasePath(t)); !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached drive never finished")
		}
	}
	evs := readEvents(t, events)
	if eventIndex(evs, evDispatched, "1") < 0 || evs[len(evs)-1].Event != evExit || *evs[len(evs)-1].Code != 0 {
		t.Errorf("the event file holds %+v, want item 1 dispatched and an exit 0 last", evs)
	}
	logged, err := os.ReadFile(strings.TrimSuffix(events, ".jsonl") + ".log") // #nosec G304 -- the test's own state directory
	if err != nil || !strings.Contains(string(logged), `"event":"exit"`) {
		t.Errorf("the drive's log does not carry its --json stream (%v):\n%s", err, logged)
	}
}

// attach on a drive that already exited prints its file from the first line
// to the exit event and returns 0, raw under --json and one line an event
// without it.
func TestAttachReplaysAFinishedDrivesEventsAndExitsZero(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	streamed, _, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	var aerr error
	out := capture(t, func() { aerr = execute(t, "attach", "--json") })
	if aerr != nil {
		t.Fatalf("attach --json: %v", aerr)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e DriveEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("attach --json printed %q: %v", line, err)
		}
		got = append(got, e.Event+" "+e.ID)
	}
	var want []string
	for _, e := range streamed {
		want = append(want, e.Event+" "+e.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("attach replayed\n%v\nwant what --events streamed\n%v", got, want)
	}

	out = capture(t, func() { aerr = execute(t, "attach", "D1") })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if aerr != nil || len(lines) != len(want) || !strings.Contains(lines[len(lines)-1], "exit") || !strings.Contains(lines[len(lines)-1], "code=0") {
		t.Errorf("attach D1 (%v) printed\n%s\nwant %d lines ending on exit code=0", aerr, out, len(want))
	}

	if err := execute(t, "attach", "D9"); err == nil || Code(err) != ExitError {
		t.Errorf("attach D9 on a project with one drive: %v, want exit 1", err)
	}
}

// watch exits 3 once a drive exits without emitting what it waits for, and
// returns the park the moment a running drive emits it.
func TestWatchUntilParkedReturnsOnAParkAndExitsThreeOnACleanExit(t *testing.T) {
	enqueueFixture(t)
	ownState(t)

	evs, _, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("first drive: %v", err)
	}
	if eventIndex(evs, evParked, "1") >= 0 {
		t.Fatalf("item 1 parked, and this half needs a drive that exits clean: %+v", evs)
	}
	err = execute(t, "watch", "D1", "--until", "parked")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "exited (code 0) without emitting parked") {
		t.Errorf("watch --until parked on a drive that exited clean: %v, want exit 3", err)
	}
	if err := execute(t, "watch", "D7", "--until", "exit"); Code(err) != ExitError {
		t.Errorf("watch on no such drive: %v, want exit 1", err)
	}

	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", release)
	ran := make(chan driveRun, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, "2")
		ran <- driveRun{evs, stderr, err}
	}()
	waitLease(t, mustLeasePath(t), func(l lease.Lease) bool { return l.Drive == "D2" && slices.Contains(l.Running, "2") })

	var stdout, stderr bytes.Buffer
	watcher := asDroverCmd(t, "watch", "--until", "parked,needs_operator")
	watcher.Stdout, watcher.Stderr = &stdout, &stderr
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	watched := make(chan error, 1)
	go func() { watched <- watcher.Wait() }()
	t.Cleanup(func() { _ = watcher.Process.Kill() })
	// Long enough for watch to read the file as it stands and start
	// waiting; the park comes only after an implement run and a review.
	time.Sleep(500 * time.Millisecond)
	write(t, release, "")

	select {
	case err := <-watched:
		if err != nil {
			t.Fatalf("watch --until parked: %v\n%s", err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("watch never returned")
	}
	var e DriveEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &e); err != nil || e.Event != evParked || e.ID != "2" || e.Drive != "D2" {
		t.Errorf("watch printed %q (%v), want item 2's parked event from D2", stdout.String(), err)
	}
	if r := <-ran; r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
}

// --stop reaches the drive through its lease, and the drive drains as an
// interrupt makes it: the running item stops at its next boundary and parks
// drained, nothing more is dispatched, and the drive exits. Nothing signals
// it: the drive's context is never canceled.
func TestStopDrainsARunningDriveWithNoSignal(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", release)
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })

	ran := make(chan driveRun, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, "1", "2")
		ran <- driveRun{evs, stderr, err}
	}()
	path := mustLeasePath(t)
	waitLease(t, path, func(l lease.Lease) bool { return slices.Contains(l.Running, "1") })
	for deadline := time.Now().Add(30 * time.Second); stageRows(t, "1", ledger.StageImplement) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("item 1's implement run never launched")
		}
	}
	var said bytes.Buffer
	if err := stopDrive(&said); err != nil {
		t.Fatalf("--stop: %v", err)
	}
	if !strings.Contains(said.String(), "stopping drive D1") {
		t.Errorf("--stop said %q", said.String())
	}
	const reply = "it dispatches nothing more, and 1 finish the stage each is in and park stopped, then it exits\n"
	if _, after, _ := strings.Cut(said.String(), "): "); after != reply {
		t.Errorf("--stop must name the park the drive records, stopped: %q", said.String())
	}
	write(t, release, "")

	var r driveRun
	select {
	case r = <-ran:
	case <-time.After(2 * time.Minute):
		t.Fatal("the stopped drive never exited")
	}
	if r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
	i := eventIndex(r.evs, evParked, "1")
	if i < 0 || r.evs[i].Park != parkDrained {
		t.Errorf("item 1 did not park stopped: %+v", r.evs)
	}
	if eventIndex(r.evs, evDispatched, "2") >= 0 {
		t.Errorf("item 2 was dispatched after --stop: %+v", r.evs)
	}
	if n := stageRows(t, "1", ledger.StageReview); n != 0 {
		t.Errorf("item 1 was reviewed %d times after --stop", n)
	}
	if _, busy, _ := lease.Read(path); busy {
		t.Error("the stopped drive still holds the lease")
	}
	if err := stopDrive(io.Discard); Code(err) != ExitRefused {
		t.Errorf("--stop with no drive running: %v, want exit 3", err)
	}
}

// A drive whose event file cannot be written says so once and runs its queue
// to the end, streaming every event under --events as before.
func TestADriveWhoseEventFileWillNotWriteFinishesItsQueue(t *testing.T) {
	enqueueFixture(t)
	dir := ownState(t)
	// A file where the directory of every project's event files goes.
	if err := os.MkdirAll(filepath.Join(dir, "drover"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "drover", "drives"), "")

	evs, stderr, err := driveEventsOf(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if n := strings.Count(stderr, "event file would not write"); n != 1 {
		t.Errorf("the failing event file was reported %d times, want once:\n%s", n, stderr)
	}
	for _, id := range []string{"1", "2"} {
		if eventIndex(evs, evDispatched, id) < 0 {
			t.Errorf("item %s was never dispatched: %+v", id, evs)
		}
	}
	if len(evs) == 0 || evs[len(evs)-1].Event != evExit || *evs[len(evs)-1].Code != 0 {
		t.Errorf("the drive did not exit 0 last on its stream: %+v", evs)
	}
}

// watchFrom runs watch --until until --after seq and returns the event it
// printed with the error it returned.
func watchFrom(t *testing.T, until string, seq int) (DriveEvent, error) {
	t.Helper()
	var werr error
	out := capture(t, func() { werr = execute(t, "watch", "D1", "--until", until, "--after", strconv.Itoa(seq)) })
	var e DriveEvent
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &e); err != nil {
			t.Fatalf("watch printed %q: %v", out, err)
		}
	}
	return e, werr
}

// --after is a cursor that names one event: a supervisor passing the seq of
// the event it handled gets the next one, never the same one again, and an
// exit it already handled still ends the watch at once.
func TestWatchAfterTheSeqOfAParkReturnsTheExitThatFollowsIt(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	if _, stderr, err := driveEventsOf(t, "2"); err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	evs := readEvents(t, fixtureEvents(t, "D1"))
	for i, e := range evs {
		if e.Seq != i+1 {
			t.Fatalf("event %d (%s) carries seq %d, want its line %d", i, e.Event, e.Seq, i+1)
		}
	}
	parked := eventIndex(evs, evParked, "2")
	if parked < 0 || evs[len(evs)-1].Event != evExit {
		t.Fatalf("this needs a drive that parks item 2 and exits: %+v", evs)
	}
	exit := evs[len(evs)-1].Seq

	e, err := watchFrom(t, "parked,exit", evs[parked].Seq)
	if err != nil || e.Event != evExit || e.Seq != exit {
		t.Errorf("watch --after the park's seq: %+v (%v), want the exit at seq %d", e, err, exit)
	}
	e, err = watchFrom(t, "parked,exit", exit)
	if err != nil || e.Event != evExit {
		t.Errorf("watch --after the exit's seq, --until exit: %+v (%v), want the exit and 0", e, err)
	}
	if _, err := watchFrom(t, "parked", exit); Code(err) != ExitRefused {
		t.Errorf("watch --after the exit's seq, --until parked: %v, want exit 3", err)
	}
	e, err = watchFrom(t, "parked", 0)
	if err != nil || e.Event != evParked || e.Seq != evs[parked].Seq {
		t.Errorf("watch --after 0 --until parked: %+v (%v), want the park already in the file", e, err)
	}
}

// Events stamped in one second are told apart by seq: walking the file with
// --after returns every event once, in order.
func TestWatchAfterReturnsEventsOfOneSecondEachOnce(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	if _, stderr, err := driveEventsOf(t, "1", "2"); err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	evs := readEvents(t, fixtureEvents(t, "D1"))
	shared := false
	for i := 1; i < len(evs); i++ {
		shared = shared || evs[i].TS == evs[i-1].TS
	}
	if !shared {
		t.Fatalf("no two events share a ts, so this proves nothing: %+v", evs)
	}
	all := strings.Join(driveEventNames, ",")
	var got []int
	for seq := 0; ; {
		e, err := watchFrom(t, all, seq)
		if err != nil {
			t.Fatalf("watch --after %d: %v", seq, err)
		}
		got = append(got, e.Seq)
		if e.Event == evExit {
			break
		}
		if e.Seq <= seq || len(got) > len(evs) {
			t.Fatalf("watch --after %d returned seq %d; walked %v", seq, e.Seq, got)
		}
		seq = e.Seq
	}
	var want []int
	for _, e := range evs {
		want = append(want, e.Seq)
	}
	if !slices.Equal(got, want) {
		t.Errorf("walking --after returned seqs %v, want each of %v once", got, want)
	}
}

// A process that reopens a drive's event file (a re-exec onto a new binary)
// numbers on from the lines it holds, and ends a line an earlier process left
// cut short, so seq stays the line follow counts.
func TestAReopenedEventFileNumbersOnFromItsLines(t *testing.T) {
	enqueueFixture(t)
	ownState(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path := fixtureEvents(t, "D4")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"event":"dispatched","ts":"2026-10-18T09:00:00Z","seq":1,"drive":"D4","id":"1"}`+"\n"+`{"event":"stage","ts":"2026-`)
	prev := activeDrive.Swap(&runningDrive{id: "D4"})
	t.Cleanup(func() { activeDrive.Store(prev) })

	es := newEventSink(s.cfg, false)
	es.stage("1", stageGate)
	es.emit(DriveEvent{Event: evExit, Code: new(int)})

	var seqs, lines []int
	err = followed{id: "D4", events: path, lease: filepath.Join(t.TempDir(), "lease")}.follow(t.Context(),
		func(_ []byte, n int, _ int64, e DriveEvent) bool {
			seqs, lines = append(seqs, e.Seq), append(lines, n)
			return false
		})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seqs, []int{1, 3, 4}) || !slices.Equal(lines, seqs) {
		t.Errorf("reopened file carries seqs %v on lines %v, want 1, 3, 4 on the lines they name", seqs, lines)
	}
}

// lockedBuffer is an output attach writes from its own goroutine while the
// test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// sessionRig is a drive's event file and session logs written by hand, with a
// lease held in this process while the drive is meant to be running.
type sessionRig struct {
	t    *testing.T
	root string
	f    followed
	held *lease.Handle
}

func newSessionRig(t *testing.T, running bool) *sessionRig {
	t.Helper()
	poll := followPoll
	followPoll = 10 * time.Millisecond
	t.Cleanup(func() { followPoll = poll })
	root := t.TempDir()
	r := &sessionRig{t: t, root: root, f: followed{
		id: "D1", events: filepath.Join(root, "D1.jsonl"), lease: filepath.Join(root, lease.FileName), root: root,
	}}
	if running {
		h, err := lease.Acquire(r.f.lease, lease.Lease{Command: "drive", Root: root, Drive: "D1"})
		if err != nil {
			t.Fatal(err)
		}
		r.held = h
		t.Cleanup(func() { _, _ = h.Close() })
	}
	return r
}

func (r *sessionRig) appendTo(path, line string) {
	r.t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the test's own directory
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sessionRig) emit(e DriveEvent) {
	r.t.Helper()
	e.TS, e.Drive = time.Now().UTC().Format(time.RFC3339), "D1"
	b, err := json.Marshal(e)
	if err != nil {
		r.t.Fatal(err)
	}
	r.appendTo(r.f.events, string(b))
}

func (r *sessionRig) launched(id, log string) {
	r.emit(DriveEvent{Event: evDispatched, ID: id})
	r.emit(DriveEvent{Event: evStage, ID: id, Stage: ledger.StageImplement, Log: log})
}

// say appends one assistant event of Claude's stream-json to a session log.
func (r *sessionRig) say(log, text string) {
	r.appendTo(filepath.Join(r.root, log),
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+strconv.Quote(text)+`}]}}`)
}

func (r *sessionRig) bash(log, command string) {
	r.appendTo(filepath.Join(r.root, log),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":`+strconv.Quote(command)+`}}]}}`)
}

type attachOpts struct {
	session, json bool
	item          string
}

// attach starts following the rig's drive as attach does, and returns its
// stdout and stderr and a channel carrying what follow returned.
func (r *sessionRig) attach(o attachOpts) (out, errOut *lockedBuffer, done chan error) {
	out, errOut, done = &lockedBuffer{}, &lockedBuffer{}, make(chan error, 1)
	a := newAttacher(r.f, out, errOut)
	a.session, a.json, a.item = o.session, o.json, o.item
	f := r.f
	f.tick = a.tick
	go func() {
		done <- f.follow(r.t.Context(), func(line []byte, _ int, end int64, e DriveEvent) bool {
			a.event(line, end, e)
			return false
		})
	}()
	return out, errOut, done
}

func eventually(t *testing.T, what string, buf *lockedBuffer, cond func(string) bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(buf.String()); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s; printed:\n%s", what, buf.String())
		}
	}
}

func attachReturned(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach never returned after the exit event")
	}
}

func exitEvent() DriveEvent { return DriveEvent{Event: evExit, Code: new(int)} }

// A session running when attach starts is followed as its log grows, from a
// log that does not exist yet, and dropped at the item's next event: what the
// log gains after that is not printed.
func TestAttachSessionFollowsARunningSessionsLog(t *testing.T) {
	r := newSessionRig(t, true)
	r.launched("1", "logs/1.jsonl")
	if err := os.MkdirAll(filepath.Join(r.root, "logs"), 0o750); err != nil {
		t.Fatal(err)
	}
	out, _, done := r.attach(attachOpts{session: true})
	eventually(t, "the stage line", out, func(s string) bool { return strings.Contains(s, "stage=implement") })

	r.bash("logs/1.jsonl", "make build &&\n  make check")
	eventually(t, "the tool call", out, func(s string) bool { return strings.Contains(s, "1 > Bash make build && make check") })
	r.say("logs/1.jsonl", "Committing.")
	eventually(t, "the text", out, func(s string) bool { return strings.Contains(s, "1 · Committing.") })

	r.say("logs/1.jsonl", "Last words before the park.")
	r.emit(DriveEvent{Event: evParked, ID: "1", Park: passSilent})
	eventually(t, "the park", out, func(s string) bool { return strings.Contains(s, "park="+passSilent) })
	r.say("logs/1.jsonl", "After the park.")
	time.Sleep(10 * followPoll)
	r.emit(exitEvent())
	attachReturned(t, done)

	got := out.String()
	if strings.Contains(got, "After the park.") {
		t.Errorf("attach printed activity after the item's next event:\n%s", got)
	}
	last := strings.Index(got, "Last words before the park.")
	if last < 0 || last > strings.Index(got, "park="+passSilent) {
		t.Errorf("the log's last line before the park is missing or follows it:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, " 1 ") && !strings.HasPrefix(line, "20") && !strings.HasPrefix(line, activityIndent+"1 ") {
			t.Errorf("activity line %q is not indented under the item", line)
		}
	}
}

// Replaying, a session that finished before attach started prints its stage
// line alone, and one still running is read from its first line.
func TestAttachSessionReplaysAFinishedSessionWithoutActivity(t *testing.T) {
	r := newSessionRig(t, true)
	r.launched("1", "1.jsonl")
	r.say("1.jsonl", "finished long ago")
	r.emit(DriveEvent{Event: evParked, ID: "1", Park: passSilent})
	r.launched("2", "2.jsonl")
	r.say("2.jsonl", "still at it")

	out, _, done := r.attach(attachOpts{session: true})
	eventually(t, "item 2's activity", out, func(s string) bool { return strings.Contains(s, "2 · still at it") })
	r.emit(exitEvent())
	attachReturned(t, done)
	if got := out.String(); strings.Contains(got, "finished long ago") || strings.Count(got, "stage=implement") != 2 {
		t.Errorf("attach replayed a finished session's activity, or dropped a stage line:\n%s", got)
	}

	// A drive that has exited has no session running, whatever its last
	// events were.
	gone := newSessionRig(t, false)
	gone.launched("1", "1.jsonl")
	gone.say("1.jsonl", "cut off")
	gone.emit(exitEvent())
	out, _, done = gone.attach(attachOpts{session: true})
	attachReturned(t, done)
	if got := out.String(); strings.Contains(got, "cut off") || !strings.Contains(got, "stage=implement") {
		t.Errorf("attach on an exited drive printed:\n%s", got)
	}
}

// --item keeps one item's events and activity, and the exit, out of two
// items whose sessions run at once; an id the drive never reaches prints the
// exit alone.
func TestAttachItemFiltersTwoInterleavedItems(t *testing.T) {
	r := newSessionRig(t, true)
	out, _, done := r.attach(attachOpts{session: true, item: "2"})
	r.launched("1", "1.jsonl")
	r.launched("2", "2.jsonl")
	r.bash("1.jsonl", "echo one")
	r.bash("2.jsonl", "echo two")
	eventually(t, "item 2's tool call", out, func(s string) bool { return strings.Contains(s, "2 > Bash echo two") })
	r.emit(DriveEvent{Event: evParked, ID: "1", Park: passSilent})
	r.emit(DriveEvent{Event: evParked, ID: "2", Park: passSilent})
	r.emit(exitEvent())
	attachReturned(t, done)
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, "one") || strings.Contains(line, "  1") || strings.Contains(line, " 1 ") {
			t.Errorf("--item 2 printed item 1's %q", line)
		}
	}
	if got := out.String(); !strings.Contains(got, "dispatched     2") || !strings.Contains(got, "exit") {
		t.Errorf("--item 2 printed:\n%s", got)
	}

	out, errOut, done := r.attach(attachOpts{item: "F9"})
	attachReturned(t, done)
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "exit") || errOut.String() != "" {
		t.Errorf("--item for an id the drive never reached printed\n%s\nstderr %q", out.String(), errOut.String())
	}
}

// A log that will not read is named once, with the error, and attach goes on
// following the drive's events.
func TestAttachSessionReportsAnUnreadableLogAndGoesOn(t *testing.T) {
	r := newSessionRig(t, true)
	// A directory opens and will not read.
	if err := os.Mkdir(filepath.Join(r.root, "1.jsonl"), 0o750); err != nil {
		t.Fatal(err)
	}
	out, errOut, done := r.attach(attachOpts{session: true})
	r.launched("1", "1.jsonl")
	eventually(t, "the read error", errOut, func(s string) bool { return strings.Contains(s, filepath.Join(r.root, "1.jsonl")) })
	time.Sleep(10 * followPoll)
	r.emit(DriveEvent{Event: evParked, ID: "1", Park: passSilent})
	r.emit(exitEvent())
	attachReturned(t, done)
	if n := strings.Count(errOut.String(), "\n"); n != 1 {
		t.Errorf("the unreadable log was reported %d times:\n%s", n, errOut.String())
	}
	if !strings.Contains(out.String(), "park="+passSilent) {
		t.Errorf("attach stopped following the events after the unreadable log:\n%s", out.String())
	}
}

// Under --json every line is a drive event keyed event or an activity keyed
// activity, never both and never neither.
func TestAttachSessionJSONKeysEachLineOnce(t *testing.T) {
	r := newSessionRig(t, true)
	out, _, done := r.attach(attachOpts{session: true, json: true})
	r.launched("1", "1.jsonl")
	r.bash("1.jsonl", "go test ./...")
	r.say("1.jsonl", "green")
	eventually(t, "the text activity", out, func(s string) bool { return strings.Contains(s, `"text":"green"`) })
	r.emit(DriveEvent{Event: evParked, ID: "1", Park: passSilent})
	r.emit(exitEvent())
	attachReturned(t, done)

	var acts []SessionActivity
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &keys); err != nil {
			t.Fatalf("attach --json printed %q: %v", line, err)
		}
		_, ev := keys["event"]
		_, act := keys["activity"]
		if ev == act {
			t.Errorf("line %s carries event=%v activity=%v, want exactly one", line, ev, act)
		}
		if act {
			var a SessionActivity
			if err := json.Unmarshal([]byte(line), &a); err != nil {
				t.Fatal(err)
			}
			acts = append(acts, a)
		}
	}
	want := []SessionActivity{
		{Activity: "tool", Drive: "D1", ID: "1", Stage: ledger.StageImplement, Tool: "Bash", Text: "Bash go test ./..."},
		{Activity: "text", Drive: "D1", ID: "1", Stage: ledger.StageImplement, Text: "green"},
	}
	for i := range acts {
		if _, err := time.Parse(time.RFC3339, acts[i].TS); err != nil {
			t.Errorf("activity %d carries ts %q", i, acts[i].TS)
		}
		acts[i].TS = ""
	}
	if !slices.Equal(acts, want) {
		t.Errorf("activities %+v, want %+v", acts, want)
	}
}
