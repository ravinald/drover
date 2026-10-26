package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/reports"
	"github.com/ravinald/drover/internal/runner"
)

type driveRun struct {
	evs    []DriveEvent
	stderr string
	err    error
}

// pausedAtReview starts a drive over ids with item 1's implement run held,
// pauses it while that run is in flight, then lets the run finish, and
// returns once item 1 is held at the boundary before its review.
func pausedAtReview(t *testing.T, ids ...string) <-chan driveRun {
	t.Helper()
	enqueueFixture(t)
	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", release)
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })
	held := make(chan string, 8)
	driveHeld = func(id, next string) { held <- id + " " + next }
	t.Cleanup(func() { driveHeld = nil })

	ran := make(chan driveRun, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, ids...)
		ran <- driveRun{evs, stderr, err}
	}()
	path := mustLeasePath(t)
	waitLease(t, path, func(l lease.Lease) bool { return slices.Contains(l.Running, "1") })
	// Past the boundary before the implement run, so the pause lands while
	// that run is in flight and holds the item at the next one.
	for deadline := time.Now().Add(30 * time.Second); stageRows(t, "1", ledger.StageImplement) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("item 1's implement run never launched")
		}
	}
	if err := pauseDrive(io.Discard); err != nil {
		t.Fatalf("--pause: %v", err)
	}
	if l, _, _ := lease.Read(path); !l.Paused {
		t.Fatalf("the drive answered --pause and its lease does not read paused: %+v", l)
	}
	write(t, release, "")
	select {
	case got := <-held:
		if got != "1 "+ledger.StageReview {
			t.Fatalf("held %q, want item 1 before its review", got)
		}
	case r := <-ran:
		t.Fatalf("the drive exited while paused: %v\n%s", r.err, r.stderr)
	case <-time.After(60 * time.Second):
		t.Fatal("item 1 never reached its review boundary")
	}
	return ran
}

func waitLease(t *testing.T, path string, ok func(lease.Lease) bool) lease.Lease {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if l, busy, _ := lease.Read(path); busy && ok(l) {
			return l
		}
		if time.Now().After(deadline) {
			l, _, _ := lease.Read(path)
			t.Fatalf("the drive's lease never reached the state waited for: %+v", l)
		}
	}
}

func reviewRows(t *testing.T, id string) int {
	t.Helper()
	return stageRows(t, id, ledger.StageReview)
}

func stageRows(t *testing.T, id, stage string) int {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	return s.ledger.Counts(id).ByStage[stage]
}

func editConfig(t *testing.T, old, with string) (restore func()) {
	t.Helper()
	path := filepath.Join(".", config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("the fixture config holds no %q", old)
	}
	write(t, path, strings.Replace(string(raw), old, with, 1))
	return func() { write(t, path, string(raw)) }
}

// A pause holds what is running and dispatches nothing, with the drive and its
// lease still up; the resume carries on from the hold, and nothing parked.
func TestAPausedDriveDispatchesNothingAndParksNothing(t *testing.T) {
	ran := pausedAtReview(t, "1", "2")
	path := mustLeasePath(t)

	time.Sleep(300 * time.Millisecond)
	l, busy, err := lease.Read(path)
	if err != nil || !busy || l.Command != "drive" {
		t.Fatalf("a paused drive's lease reads busy=%v %+v (%v)", busy, l, err)
	}
	if !l.Paused || !slices.Equal(l.Running, []string{"1"}) || !slices.Equal(l.Queue, []string{"2"}) {
		t.Errorf("paused lease reads paused=%v running %v queue %v, want paused, 1 running and 2 queued", l.Paused, l.Running, l.Queue)
	}
	if n := reviewRows(t, "1"); n != 0 {
		t.Errorf("item 1 was reviewed %d times while held", n)
	}
	select {
	case r := <-ran:
		t.Fatalf("the paused drive exited: %v\n%s", r.err, r.stderr)
	default:
	}

	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume: %v", err)
	}
	r := <-ran
	if r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
	paused, resumed := eventIndex(r.evs, evPaused, ""), eventIndex(r.evs, evResumed, "")
	two := eventIndex(r.evs, evDispatched, "2")
	if paused < 0 || resumed < paused || two < resumed {
		t.Errorf("paused at %d, resumed at %d, 2 dispatched at %d: want 2 dispatched after the resume\n%+v", paused, resumed, two, r.evs)
	}
	for _, e := range r.evs {
		if e.Event == evParked && (e.Park == parkDrained || e.Park == "interrupted") {
			t.Errorf("the pause parked %s: %+v", e.ID, e)
		}
	}
	if n := reviewRows(t, "1"); n == 0 {
		t.Error("item 1 was never reviewed after the resume")
	}
}

// A resume that changed the item's verify command runs the new one on the
// held item's tree before it goes on to the review the old one would have
// preceded, and says which inputs changed.
func TestAResumeAfterAVerifyChangeRerunsTheHeldItemsVerify(t *testing.T) {
	ran := pausedAtReview(t, "1")
	editConfig(t, `check default"`, `check swapped"`)
	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume: %v", err)
	}
	r := <-ran
	if r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
	log, err := os.ReadFile("check.log")
	if err != nil {
		t.Fatalf("no verify command ran: %v\n%s", err, r.stderr)
	}
	if !strings.Contains(string(log), "swapped dir=") {
		t.Errorf("the new verify command never ran on the held item:\n%s", log)
	}
	i := slices.IndexFunc(r.evs, func(e DriveEvent) bool { return e.Event == evResumed && e.ID == "1" })
	if i < 0 {
		t.Fatalf("no resumed event for item 1: %+v", r.evs)
	}
	if ch := r.evs[i].Changed; !slices.Contains(ch, inputVerify) || !slices.Contains(ch, inputConfig) {
		t.Errorf("item 1 resumed with changed %v, want verify and config", ch)
	}
	gate := slices.IndexFunc(r.evs[i:], func(e DriveEvent) bool { return e.Event == evStage && e.ID == "1" && e.Stage == stageGate })
	review := slices.IndexFunc(r.evs[i:], func(e DriveEvent) bool {
		return e.Event == evStage && e.ID == "1" && e.Stage == ledger.StageReview
	})
	if gate < 0 || review < gate {
		t.Errorf("after the resume the verify run (stage %q) is at %d and the review at %d, want the verify run first: %+v", stageGate, gate, review, r.evs[i:])
	}
}

// A config that will not load is refused before the drive is asked, and one
// that loads and fails the launch's own preflight is refused by the drive. The
// drive stays paused through both, and resumes once the file is fixed.
func TestAResumeWithABrokenConfigRefusesAndStaysPaused(t *testing.T) {
	ran := pausedAtReview(t, "1")
	path := mustLeasePath(t)

	restore := editConfig(t, "[run]", "[run\n")
	err := resumeDrive(io.Discard)
	if Code(err) != ExitRefused {
		t.Errorf("--resume over a config that will not parse exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	restore()
	if l, _, _ := lease.Read(path); !l.Paused || l.Control != "" {
		t.Errorf("after the refused resume the lease reads paused=%v control=%q, want paused and no request", l.Paused, l.Control)
	}

	restore = editConfig(t, `review_runner  = "rev"`, "")
	err = resumeDrive(io.Discard)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "reviewing runner") {
		t.Errorf("--resume over a config the preflight refuses exited %d (%v), want %d naming the reviewer", Code(err), err, ExitRefused)
	}
	if l, _, _ := lease.Read(path); !l.Paused {
		t.Error("the drive refused the resume and stopped being paused")
	}
	if n := reviewRows(t, "1"); n != 0 {
		t.Errorf("item 1 was reviewed %d times after a refused resume", n)
	}
	restore()

	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume over the fixed config: %v", err)
	}
	if r := <-ran; r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
}

// --cleanup lists every tree with what it holds, and --apply removes the clean
// one, keeps the dirty one, and deletes the empty orphan branch but not the
// one carrying a commit.
func TestCleanupListsADirtyWorktreeAndApplyKeepsIt(t *testing.T) {
	mergeProject(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, id := range []string{"1", "2"} {
		it, err := s.item(id)
		if err != nil {
			t.Fatal(err)
		}
		branch, err := s.branchFor(it)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := s.openItemTree(it, branch, "main")
		if err != nil {
			t.Fatalf("open %s's tree: %v", id, err)
		}
		tree.close()
		paths[id] = tree.Path
	}
	write(t, filepath.Join(paths["1"], "half-done.txt"), "work nobody committed\n")
	git(t, ".", "branch", "old/01-first", "main")
	git(t, ".", "checkout", "-q", "-b", "kept-first", "main")
	git(t, ".", "-c", "user.name=t", "-c", "user.email=t@x.invalid", "commit", "-q", "--allow-empty", "-m", "real work")
	git(t, ".", "checkout", "-q", "main")
	if err := os.MkdirAll("reports", 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join("reports", "01-first.md"), "# 1\n\n"+reports.NoResult+"\n")

	var out bytes.Buffer
	if err := s.cleanupDrive(&out, false); err != nil {
		t.Fatalf("--cleanup: %v", err)
	}
	list := out.String()
	for _, want := range []string{
		"1  " + resolved(paths["1"]) + "  branch ",
		"uncommitted work: half-done.txt  held by no drive",
		"2  " + resolved(paths["2"]) + "  branch ",
		"clean  held by no drive",
		"old/01-first  empty",
		"kept-first  1 commit(s) past the trunk, kept",
		filepath.Join("reports", "01-first.md"),
	} {
		if !strings.Contains(list, want) {
			t.Errorf("--cleanup does not list %q:\n%s", want, list)
		}
	}
	if _, err := os.Stat(paths["2"]); err != nil {
		t.Fatalf("--cleanup alone removed a tree: %v", err)
	}

	out.Reset()
	if err := s.cleanupDrive(&out, true); err != nil {
		t.Fatalf("--cleanup --apply: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(paths["1"], "half-done.txt")); err != nil {
		t.Errorf("--apply touched the dirty tree: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(paths["2"]); !os.IsNotExist(err) {
		t.Errorf("--apply left the clean tree (%v):\n%s", err, out.String())
	}
	if gitAt(".", "rev-parse", "--verify", "--quiet", "refs/heads/old/01-first") == nil {
		t.Error("--apply kept the empty orphan branch")
	}
	if gitAt(".", "rev-parse", "--verify", "--quiet", "refs/heads/kept-first") != nil {
		t.Error("--apply deleted a branch carrying a commit")
	}
}

// A supervisor pauses only where pause_for_config_change lets it, and the
// refusal names that action rather than enqueue, which every other drive is.
func TestASupervisorPausesOnlyAtSupervisorPolicy(t *testing.T) {
	enqueueFixture(t)
	t.Cleanup(func() { drivePause = false })
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	err := refuse(t, "drive", "--pause")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "pause_for_config_change") {
		t.Errorf("--pause at the default policy exited %d (%v), want %d naming pause_for_config_change", Code(err), err, ExitRefused)
	}

	editConfig(t, "[run]", "[supervise]\npause_for_config_change = \"supervisor\"\nenqueue = \"operator\"\n\n[run]")
	err = refuse(t, "drive", "--pause")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no drive is running") {
		t.Errorf("--pause at supervisor policy exited %d (%v), want the policy check passed and %d for no drive", Code(err), err, ExitRefused)
	}
}

// The lease's holds are the items held at a boundary of a paused drive as of
// the last change, whatever order the items' goroutines publish in: every one
// once all of them hold, and none once a resume let every one go.
func TestTheLeaseHoldsWhatIsHeldNowAcrossConcurrentHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease")
	h, err := lease.Acquire(path, lease.Lease{Command: "drive"})
	if err != nil {
		t.Fatal(err)
	}
	prev := held
	held = h
	t.Cleanup(func() {
		held = prev
		_ = h.Release()
	})
	onDisk := func() []lease.Hold {
		l, _, err := lease.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return l.Holds
	}
	const items = 4
	for round := range 150 {
		q := newDriveSched()
		q.pause()
		var wg sync.WaitGroup
		for i := range items {
			wg.Go(func() { q.hold(t.Context(), lease.Hold{ID: strconv.Itoa(i), At: "review"}) })
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			q.mu.Lock()
			n := len(q.holds)
			q.mu.Unlock()
			if n == items {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %d of %d items ever held", round, n, items)
			}
		}
		if got := onDisk(); len(got) != items {
			t.Fatalf("round %d: the lease holds %v with all %d items held", round, got, items)
		}
		q.resume(nil)
		wg.Wait()
		if got := onDisk(); len(got) != 0 {
			t.Fatalf("round %d: the lease still holds %v after every item left", round, got)
		}
	}
}

// --timeout bounds --wait and nothing else: a drive passed one is refused
// before it takes the lease, rather than started with the flag dropped.
func TestTimeoutWithoutWaitRefusesADrive(t *testing.T) {
	enqueueFixture(t)
	err := refuse(t, "drive", "--timeout", "1m", "1")
	if Code(err) != ExitRefused || err == nil || !strings.Contains(err.Error(), "--timeout bounds --wait") {
		t.Fatalf("drive --timeout 1m 1 exited %d (%v), want %d naming --wait", Code(err), err, ExitRefused)
	}
	if _, busy, _ := lease.Read(mustLeasePath(t)); busy {
		t.Error("the refused drive left its lease held")
	}
	if n := stageRows(t, "1", ledger.StageImplement); n != 0 {
		t.Errorf("the refused drive ran item 1 (%d implement rows)", n)
	}
}

// configEdits runs each id's edit as the drive dispatches that item, before
// the drive compares the config on disk.
func configEdits(t *testing.T, edits map[string]func()) {
	t.Helper()
	driveDispatching = func(id string) {
		if edit := edits[id]; edit != nil {
			edit()
		}
	}
	t.Cleanup(func() { driveDispatching = nil })
}

// configChanges is the index in evs of each config_changed event.
func configChanges(evs []DriveEvent) []int {
	var out []int
	for i, e := range evs {
		if e.Event == evConfigChanged {
			out = append(out, i)
		}
	}
	return out
}

// An edit to .drover.toml mid-drive is reported once per distinct content and
// applied nowhere: the item after it under the same bytes says nothing more, a
// revert to the bytes the drive runs under says nothing, and the same edit
// made again after that revert is reported again.
func TestADriveReportsAConfigEditOncePerContent(t *testing.T) {
	enqueueFixture(t)
	write(t, filepath.Join("work", "05-fifth.md"), "---\ntitle: Fifth\ndeps:\n---\n\n1. Do it last.\n")
	raw, err := os.ReadFile(config.FileName)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "[drive]\n", "[drive]\nprotected_paths = [\"migrations/*\"]\n", 1)
	if edited == string(raw) {
		t.Fatal("the fixture config holds no [drive] table")
	}
	configEdits(t, map[string]func(){
		"2": func() { write(t, config.FileName, edited) },
		"3": func() { write(t, config.FileName, string(raw)) },
		"5": func() { write(t, config.FileName, edited) },
	})

	evs, stderr, err := driveEventsOf(t, "1", "2", "4", "3", "5")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if n := strings.Count(stderr, "config changed on disk at "); n != 2 {
		t.Errorf("the drive printed the config line %d times, want 2 (the edit, and the edit again after a revert):\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "this drive still runs the .drover.toml it started with; drover drive --pause, then --resume, applies it; changed: [drive]") {
		t.Errorf("the config line does not say what runs, how to apply it and which table changed:\n%s", stderr)
	}
	got := configChanges(evs)
	if len(got) != 2 {
		t.Fatalf("%d config_changed events, want 2: %+v", len(got), evs)
	}
	for _, i := range got {
		if e := evs[i]; !slices.Equal(e.Tables, []string{"[drive]"}) || e.Error != "" {
			t.Errorf("config_changed names tables %v error %q, want [drive] alone", e.Tables, e.Error)
		}
	}
	two, four, five := eventIndex(evs, evDispatched, "2"), eventIndex(evs, evDispatched, "4"), eventIndex(evs, evDispatched, "5")
	if got[0] < two || got[0] > four {
		t.Errorf("the first config_changed is at %d, want between 2's dispatch (%d) and 4's (%d)", got[0], two, four)
	}
	if got[1] < five {
		t.Errorf("the second config_changed is at %d, before 5's dispatch at %d: the third item or the revert reported", got[1], five)
	}
}

// An edit that will not load is reported with the load error rather than a
// table list, and the drive goes on to dispatch the next item.
func TestADriveReportsAnUnloadableConfigEdit(t *testing.T) {
	enqueueFixture(t)
	raw, err := os.ReadFile(config.FileName)
	if err != nil {
		t.Fatal(err)
	}
	configEdits(t, map[string]func(){
		"1": func() { write(t, config.FileName, strings.Replace(string(raw), "[run]", "[run\n", 1)) },
		"2": func() { write(t, config.FileName, string(raw)) },
	})

	evs, stderr, err := driveEventsOf(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	got := configChanges(evs)
	if len(got) != 1 || evs[got[0]].Error == "" || len(evs[got[0]].Tables) != 0 {
		t.Fatalf("config_changed at %v in %+v, want one carrying the load error and no tables", got, evs)
	}
	if !strings.Contains(stderr, "the new file will not load, so a --resume refuses it: ") {
		t.Errorf("the config line does not report the load error:\n%s", stderr)
	}
	if eventIndex(evs, evDispatched, "2") < 0 {
		t.Errorf("the drive stopped on an unloadable edit instead of going on: %+v", evs)
	}
}

// status names a drive whose config moved on from the file on disk, from the
// lease alone, and says nothing for a match or for a lease with no digest.
func TestStatusSaysTheConfigChangedUnderARunningDrive(t *testing.T) {
	dir := leaseProject(t)
	raw, err := os.ReadFile(filepath.Join(dir, config.FileName)) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(s.cfg.Abs(s.cfg.Paths.Ledger),
		ledger.Row{ID: ledger.DriveRowID, Drive: "D1", Queued: []string{"1"}, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	const line = "config changed since this drive started; --pause, then --resume, applies it"
	for _, c := range []struct {
		name, digest string
		want         bool
	}{
		{"differing", configDigest(append(slices.Clone(raw), "# edited\n"...)), true},
		{"matching", configDigest(raw), false},
		{"absent", "", false},
	} {
		h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "drive", Root: dir, Drive: "D1", ConfigSHA256: c.digest})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		out := capture(t, func() {
			if err := execute(t, "status", "--no-tracker"); err != nil {
				t.Fatalf("status: %v", err)
			}
		})
		if strings.Contains(out, line) != c.want {
			t.Errorf("%s digest: status says the config changed = %v, want %v:\n%s", c.name, !c.want, c.want, out)
		}
		js := capture(t, func() {
			if err := execute(t, "status", "--no-tracker", "--json"); err != nil {
				t.Fatalf("status --json: %v", err)
			}
		})
		var rep StatusReport
		if err := json.Unmarshal([]byte(js), &rep); err != nil {
			t.Fatalf("status --json: %v\n%s", err, js)
		}
		if len(rep.Drives) != 1 || !rep.Drives[0].Running || rep.Drives[0].ConfigChanged != c.want {
			t.Errorf("%s digest: status --json drives %+v, want D1 running with config_changed %v", c.name, rep.Drives, c.want)
		}
		_ = h.Release()
	}
}
