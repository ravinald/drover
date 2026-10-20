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
	t.Cleanup(func() {
		followPoll = poll
		attachJSON, attachSince, watchUntil, watchSince, watchAfter = false, "", nil, "", -1
		driveStop, driveDetach = false, false
	})
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

	attachJSON = false
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
	watchUntil, watchAfter = nil, -1
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
