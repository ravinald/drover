package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// holdStubs is jobsStubs with an implementing run that, for an item whose body
// says HOLD-C, waits for item 1 to park protected_paths and the dispatcher to
// hear of it, copies the lease, and notes whether item 2 had started by then.
// Under DROVER_TEST_MARK it then records item 1 merged, as a mark-merged the
// operator ran mid-drive would, and waits for item 2 to start before it ends.
func holdStubs(t *testing.T, log string) (state string) {
	t.Helper()
	jobsStubs(t, log)
	impl, err := exec.LookPath("impl")
	if err != nil {
		t.Fatal(err)
	}
	state = t.TempDir()
	t.Setenv("DROVER_TEST_HOLD", state)
	stub(t, impl, `#!/bin/sh
id=$(basename "$(pwd)")
touch "$DROVER_TEST_ACTIVE/$id"
printf '%s:%s\n' "$id" "$(ls "$DROVER_TEST_ACTIVE" | grep -vxF "$id" | tr '\n' ' ')" >> "$DROVER_TEST_JOBS_LOG"
echo work > "implemented-$id.txt"
git add "implemented-$id.txt"
git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m "the run did $id"
case "$*" in
  *HOLD-C*)
    n=0
    while ! grep -q '"park":"protected_paths"' "$DROVER_ROOT/ledger.jsonl" && [ $n -lt 600 ]; do sleep 0.1; n=$((n + 1)); done
    sleep 1
    cp "$DROVER_TEST_LEASE" "$DROVER_TEST_HOLD/lease.json"
    if grep -q "^$DROVER_TEST_B:" "$DROVER_TEST_JOBS_LOG"; then touch "$DROVER_TEST_HOLD/b-early"; fi
    if [ -n "${DROVER_TEST_MARK:-}" ]; then
      printf '{"id":"1","merged":true,"date":"2026-10-23"}\n' >> "$DROVER_ROOT/ledger.jsonl"
      n=0
      while ! grep -q "^$DROVER_TEST_B:" "$DROVER_TEST_JOBS_LOG" && [ $n -lt 300 ]; do sleep 0.1; n=$((n + 1)); done
    fi ;;
  *SNAP*) cp "$DROVER_TEST_LEASE" "$DROVER_TEST_HOLD/lease.json" ;;
esac
sleep 1
echo '{"type":"result","subtype":"success","is_error":false}'
`)
	return state
}

// driveWithEvents runs a drive with --json --events and splits the stream into
// item reports and events.
func driveWithEvents(t *testing.T, args ...string) (rows []DriveItem, evs []DriveEvent, err error) {
	t.Helper()
	out := capture(t, func() { err = execute(t, append([]string{"drive", "--json", "--events"}, args...)...) })
	for _, line := range strings.Split(out, "\n") {
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		if _, ok := probe["event"]; ok {
			var e DriveEvent
			_ = json.Unmarshal([]byte(line), &e)
			evs = append(evs, e)
			continue
		}
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			rows = append(rows, d)
		}
	}
	return rows, evs, err
}

// holdProject writes A, B depending on A, and C, with A's file under
// protected_paths, and points the stub at the lease and at B's worktree.
func holdProject(t *testing.T) (log, state string) {
	t.Helper()
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })
	log = filepath.Join(t.TempDir(), "starts.log")
	state = holdStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "03-third.md"), "---\ntitle: Third\ndeps:\n---\n\n1. Do it. HOLD-C\n")
	f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("protected_paths = [\"implemented-" + treeName(t, "1") + ".txt\"]\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-am", "protect what item 1 writes")
	git(t, dir, "push", "-q", "origin", "main")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_LEASE", path)
	t.Setenv("DROVER_TEST_B", treeName(t, "2"))
	return log, state
}

func readLeaseCopy(t *testing.T, state string) lease.Lease {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "lease.json")) // #nosec G304 -- the test's own copy
	if err != nil {
		t.Fatalf("the stub copied no lease: %v", err)
	}
	var l lease.Lease
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// A dep parked for the operator's merge holds its dependent at its place while
// other work runs, and a merge recorded mid-drive dispatches it.
func TestADependentWaitsForAnOperatorHeldDepAndDispatchesOnItsMerge(t *testing.T) {
	log, state := holdProject(t)
	t.Setenv("DROVER_TEST_MARK", "1")

	rows, evs, err := driveWithEvents(t, "1", "2", "3", "--jobs", "3")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	got := map[string]DriveItem{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if got["1"].Park != "protected_paths" {
		t.Fatalf("item 1 is %s/%s (%s), want parked protected_paths", got["1"].Outcome, got["1"].Park, got["1"].Reason)
	}
	if _, early := os.Stat(filepath.Join(state, "b-early")); early == nil {
		t.Error("item 2 started while item 1 waited on the operator and item 3 ran")
	}
	l := readLeaseCopy(t, state)
	if !slices.Contains(l.Queue, "2") || !slices.Equal(l.Waiting, []lease.Wait{{ID: "2", On: "1"}}) {
		t.Errorf("mid-drive lease has queue %v waiting %+v, want 2 queued and waiting on 1", l.Queue, l.Waiting)
	}
	two, three := treeName(t, "2"), treeName(t, "3")
	starts := jobsStarts(t, log)
	if _, ran := starts[two]; !ran {
		t.Fatalf("item 2 never started after item 1 was recorded merged: %v", starts)
	}
	if !slices.Contains(starts[two], three) {
		t.Errorf("item 2 did not start while item 3 still ran: %v", starts)
	}
	if got["2"].Outcome == outcomeSkipped {
		t.Errorf("item 2 skipped (%s) though its dep was recorded merged", got["2"].Reason)
	}
	for _, e := range evs {
		if e.Event == evSkipped {
			t.Errorf("a skipped event arrived in a drive that skipped nothing: %+v", e)
		}
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	var rs []Row
	rs = append(rs, Row{ID: "2"})
	s.attachDrive(rs, lease.Lease{Command: "drive", PID: 7, Queue: []string{"2"}, Waiting: []lease.Wait{{ID: "2", On: "1"}}}, time.Now())
	if rs[0].Queued == nil || rs[0].Queued.WaitsFor != "1" || !strings.Contains(rs[0].note(), "queued 1, waits for 1's merge") {
		t.Errorf("status reads a held item as %+v / %q, want queued and waiting for 1", rs[0].Queued, rs[0].note())
	}
}

// With no merge, the held dependent skips with an event once the last other
// item ends, and the drive exits as a drained drive does.
func TestAHeldDependentSkipsWhenNothingElseIsLeftToRun(t *testing.T) {
	log, state := holdProject(t)

	rows, evs, err := driveWithEvents(t, "1", "2", "3", "--jobs", "3")
	if err != nil {
		t.Fatalf("drive exited %v, want 0", err)
	}
	if _, early := os.Stat(filepath.Join(state, "b-early")); early == nil {
		t.Error("item 2 started while item 1 waited on the operator and item 3 ran")
	}
	if _, ran := jobsStarts(t, log)[treeName(t, "2")]; ran {
		t.Error("item 2 started a session though its dep never merged")
	}
	var two *DriveItem
	for i := range rows {
		if rows[i].ID == "2" {
			two = &rows[i]
		}
	}
	want := "blocked by 1, which parked protected_paths for the operator"
	if two == nil || two.Outcome != outcomeSkipped || two.Reason != want {
		t.Fatalf("item 2 reported %+v, want skipped: %s", two, want)
	}
	var skip, threeEnd, exit int
	for i, e := range evs {
		switch {
		case e.Event == evSkipped && e.ID == "2":
			skip = i + 1
			if e.State != stateBlocked || e.Reason != want {
				t.Errorf("skipped event %+v, want state blocked and reason %q", e, want)
			}
		case e.Event == evDispatched && e.ID == "2":
			t.Errorf("item 2 was dispatched: %+v", e)
		case e.Event == evExit:
			exit = i + 1
			if e.Summary == nil || !slices.ContainsFunc(e.Summary.Undone, func(u UndoneItem) bool {
				return u.ID == "2" && len(u.BlockedBy) == 1 && u.BlockedBy[0].ID == "1"
			}) {
				t.Errorf("exit summary %+v, want item 2 undone and blocked by 1", e.Summary)
			}
		}
	}
	for i, e := range evs {
		if e.ID == "3" && e.Event != evSkipped {
			threeEnd = i + 1
		}
	}
	if skip == 0 || threeEnd == 0 || exit == 0 || skip > exit || skip < threeEnd {
		t.Errorf("events %+v: want item 2 skipped before the exit, after item 3's last event", evs)
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	ds := s.ledger.Drives()
	if len(ds) == 0 || !slices.Contains(ds[0].Items, ledger.Outcome{ID: "2", Outcome: outcomeSkipped, Reason: want}) {
		t.Errorf("the end row carries %+v, want 2 skipped with its reason", ds)
	}
}

// At --jobs 1, a dep that parked for a cause nobody merges leaves its
// dependent to dispatch and skip as blocked, and the skip is an event that the
// running drive's lease and drives --json both carry.
func TestADependentOfAnOtherwiseParkedDepSkipsWithAnEvent(t *testing.T) {
	isolate(t)
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "starts.log")
	state := holdStubs(t, log)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "03-third.md"), "---\ntitle: Third\ndeps:\n---\n\n1. Do it. SNAP\n")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_LEASE", path)

	rows, evs, err := driveWithEvents(t, "1", "2", "3", "--jobs", "1")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(rows) != 3 || rows[0].ID != "1" || heldForOperator(s.cfg, rows[0]) {
		t.Fatalf("rows %+v: want item 1 first, ended without an operator hold", rows)
	}
	var skip *DriveEvent
	for i := range evs {
		if evs[i].Event == evSkipped {
			skip = &evs[i]
		}
	}
	if skip == nil || skip.ID != "2" || skip.State != stateBlocked || skip.Reason != "blocked by 1" {
		t.Fatalf("skipped event %+v, want item 2 blocked by 1", skip)
	}

	l := readLeaseCopy(t, state)
	if !slices.Equal(l.Skipped, []lease.Skip{{ID: "2", Reason: "blocked by 1"}}) {
		t.Errorf("mid-drive lease carries skipped %+v, want item 2 blocked by 1", l.Skipped)
	}
	ds := []ledger.Drive{{ID: "D1", Items: []ledger.Outcome{{ID: "1"}, {ID: "2"}, {ID: "3"}}}}
	l.Drive = "D1"
	s.drivesRunning(ds, l, true)
	if ds[0].Items[1] != (ledger.Outcome{ID: "2", Outcome: outcomeSkipped, Reason: "blocked by 1"}) {
		t.Errorf("drives --json mid-drive reads item 2 as %+v, want skipped blocked by 1", ds[0].Items[1])
	}
}

// An item held for the operator is not running, so an item that only cites a
// path it cites overtakes it, while one depending on it, directly or through
// another held item, stays behind.
func TestAnItemOnlyOverlappingAnOperatorHeldOneIsNotHeldByIt(t *testing.T) {
	q := func(num int, deps string) queued {
		return newQueued(backlog.Item{ID: backlog.ID{Num: num}, Meta: map[string]string{backlog.KeyDeps: deps},
			Body: "1. Change `internal/cmd/drive.go:10`.\n"}, true)
	}
	operator := map[string]string{"1": "protected_paths"}
	cases := []struct {
		name    string
		pending []queued
		want    int
	}{
		{"overlaps the held item", []queued{q(2, "1"), q(3, "")}, 1},
		{"overlaps an item held through a held dep", []queued{q(2, "1"), q(4, "2"), q(3, "")}, 2},
		{"depends on the held item", []queued{q(2, "1"), q(3, "2")}, -1},
	}
	for _, c := range cases {
		if i, held := dispatchable(c.pending, map[int]queued{}, operator, nil); i != c.want {
			t.Errorf("%s: dispatchable = %d (held %+v), want %d", c.name, i, held, c.want)
		}
	}
	running := map[int]queued{1: q(5, "")}
	if i, held := dispatchable([]queued{q(2, "1"), q(3, "")}, running, operator, nil); i != -1 {
		t.Errorf("an item overlapping a running one dispatched at %d (held %+v)", i, held)
	}
	delete(operator, "1")
	if i, held := dispatchable([]queued{q(2, "1"), q(3, "")}, map[int]queued{}, operator, nil); i != 0 {
		t.Errorf("released item 2 is not first to dispatch: %d (held %+v)", i, held)
	}
}

// At --jobs 3, an item citing a path only the held dependent cites starts
// beside the item still running, rather than waiting behind the hold.
func TestAnOverlappingItemOvertakesAHeldDependent(t *testing.T) {
	log, _ := holdProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1\n---\n\n1. Change `internal/shared.go:1`.\n")
	write(t, filepath.Join(dir, "work", "04-fourth.md"), "---\ntitle: Fourth\ndeps:\n---\n\n1. Change `internal/shared.go:2`.\n")

	rows, _, err := driveWithEvents(t, "1", "2", "3", "4", "--jobs", "3")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	got := map[string]DriveItem{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if got["1"].Park != "protected_paths" || got["2"].Outcome != outcomeSkipped {
		t.Fatalf("rows %+v: want 1 parked protected_paths and 2 skipped", rows)
	}
	starts := jobsStarts(t, log)
	four := treeName(t, "4")
	if _, ran := starts[four]; !ran {
		t.Fatalf("item 4 never started: %v", starts)
	}
	if !slices.Contains(starts[four], treeName(t, "3")) {
		t.Errorf("item 4 waited for item 3 to end behind held item 2: %v", starts)
	}
}

// queuedMergeProject writes item 1 and item 2 depending on it, each passing
// review, on a forge whose required checks hold item 1's --auto merge in its
// queue and merge item 2's at once. drive polls every 10ms. extra is appended
// to the [drive] table and committed. It returns item 1's branch.
func queuedMergeProject(t *testing.T, extra string) (branch string) {
	t.Helper()
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	jobsStubs(t, filepath.Join(t.TempDir(), "starts.log"))
	for v, d := range map[*time.Duration]time.Duration{&enqueuePoll: 20 * time.Millisecond, &checksPoll: 10 * time.Millisecond} {
		was := *v
		*v = d
		t.Cleanup(func() { *v = was })
	}
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1\n---\n\n1. Do it. REVIEW-PASSES-2\n")
	if extra != "" {
		f, err := os.OpenFile(filepath.Join(dir, config.FileName), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(extra + "\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "commit", "-q", "-am", "configure the merge wait")
		git(t, dir, "push", "-q", "origin", "main")
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if branch, err = s.branchFor(it); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_QUEUE", branch)
	t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
	return branch
}

// latestRows is each item's last report in a drive's stream.
func latestRows(rows []DriveItem) map[string]DriveItem {
	got := map[string]DriveItem{}
	for _, r := range rows {
		got[r.ID] = r
	}
	return got
}

// A merge queued at the forge holds its dependent rather than skipping it, and
// once a poll sees it land the drive finishes it and dispatches the dependent,
// which merges in the same drive.
func TestADependentWaitsForAQueuedMergeAndDispatchesOnItsLanding(t *testing.T) {
	queuedMergeProject(t, "")
	t.Setenv("DROVER_TEST_LANDS_ON", "4")

	var rows []DriveItem
	var evs []DriveEvent
	var err error
	errOut := captureErr(t, func() { rows, evs, err = driveWithEvents(t, "1", "2") })
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, errOut)
	}
	if len(rows) == 0 || rows[0].ID != "1" || !rows[0].MergeQueued {
		t.Fatalf("item 1 was not first reported with its merge queued: %+v", rows)
	}
	got := latestRows(rows)
	if one := got["1"]; one.Outcome != outcomeMergedAtForge || !one.Merged || !one.Finished || one.PR != 7 {
		t.Errorf("item 1 ends %q merged=%v finished=%v pr=%d (%s), want merged at the forge on #7 and finished",
			one.Outcome, one.Merged, one.Finished, one.PR, one.Reason)
	}
	if two := got["2"]; two.Outcome != outcomeMergedAtForge || !two.Merged || !two.Finished {
		t.Errorf("item 2 ends %q merged=%v finished=%v (%s), want merged and finished in this drive",
			two.Outcome, two.Merged, two.Finished, two.Reason)
	}
	var merged []string
	for _, e := range evs {
		switch e.Event {
		case evSkipped:
			t.Errorf("a skipped event arrived: %+v", e)
		case evMerged:
			merged = append(merged, e.ID)
			if e.ID == "1" && e.PR != 7 {
				t.Errorf("merged for item 1 names PR %d, want 7", e.PR)
			}
		}
	}
	if !slices.Equal(merged, []string{"1", "2"}) {
		t.Errorf("merged events for %v, want 1 then 2", merged)
	}
	if n := strings.Count(errOut, "· waiting for 1's merge, queued at the forge on #7, holding 2;"); n != 1 {
		t.Errorf("stderr carries %d waiting lines for 1, want 1:\n%s", n, errOut)
	}
	if !strings.Contains(errOut, "up to 30m0s ([drive] merge_wait)") {
		t.Errorf("the waiting line does not name the bound:\n%s", errOut)
	}
	b, err := os.ReadFile(os.Getenv("DROVER_TEST_POLLS"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 4 {
		t.Errorf("the merged state was read %d times before it landed, want 4: the read at the merge and three polls", n)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if !s.ledger.State(id).Merged {
			t.Errorf("item %s is not recorded merged", id)
		}
	}
}

// The wait is bounded: past [drive] merge_wait the dependent skips naming the
// queued dep, its pull request and how long the drive waited, and the dep
// stays queued for the next drive's reconciliation.
func TestADependentOfAQueuedMergeSkipsAtMergeWait(t *testing.T) {
	queuedMergeProject(t, `merge_wait = "100ms"`)
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")

	rows, _, err := driveWithEvents(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	got := latestRows(rows)
	two := got["2"]
	if two.Outcome != outcomeSkipped || two.Park != stateBlocked {
		t.Fatalf("item 2 is %q/%q (%s), want skipped blocked", two.Outcome, two.Park, two.Reason)
	}
	for _, want := range []string{"blocked by 1", "#7", "the drive waited", "[drive] merge_wait", "1 stays ready_to_merge"} {
		if !strings.Contains(two.Reason, want) {
			t.Errorf("the skip reason does not carry %q: %s", want, two.Reason)
		}
	}
	if one := got["1"]; one.Outcome != outcomeReviewed || !one.MergeQueued || one.Merged {
		t.Errorf("item 1 is %q queued=%v merged=%v, want ready_to_merge with its merge still queued", one.Outcome, one.MergeQueued, one.Merged)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if pr, queued := s.ledger.MergeQueued("1"); !queued || pr != 7 {
		t.Errorf("the ledger reads 1 queued=%v on #%d, want queued on #7 for reconciliation", queued, pr)
	}
}

// A pull request the forge closes without merging ends the wait at once.
func TestADependentOfAQueuedMergeSkipsWhenThePullRequestCloses(t *testing.T) {
	queuedMergeProject(t, "")

	start := time.Now()
	rows, _, err := driveWithEvents(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	two := latestRows(rows)["2"]
	if two.Outcome != outcomeSkipped || !strings.Contains(two.Reason, "blocked by 1") ||
		!strings.Contains(two.Reason, "#7 was closed at the forge without merging") {
		t.Errorf("item 2 is %q (%s), want skipped naming the closed pull request", two.Outcome, two.Reason)
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("the drive took %s, as if it waited out merge_wait", took)
	}
}

// A merge state gh cannot read three polls running ends the wait.
func TestADependentOfAQueuedMergeSkipsWhenTheMergeStateWillNotRead(t *testing.T) {
	branch := queuedMergeProject(t, "")
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")
	// The read at the merge itself must answer, or the merge reads as unreadable
	// at once; the poll's reads fail once the queue file names the branch.
	checks := filepath.Join(t.TempDir(), "gh")
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ghPath, checks); err != nil {
		t.Fatal(err)
	}
	stub(t, ghPath, `#!/bin/sh
case "$*" in *'--state merged'*)
  if [ -s "$DROVER_TEST_POLLS" ]; then echo 'gh: Server Error (HTTP 502)' >&2; exit 1; fi ;;
esac
exec "`+checks+`" "$@"
`)

	rows, _, err := driveWithEvents(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	two := latestRows(rows)["2"]
	if two.Outcome != outcomeSkipped || !strings.Contains(two.Reason, "could not be read 3 polls running") {
		t.Errorf("item 2 is %q (%s), want skipped naming the unreadable merge state of %s", two.Outcome, two.Reason, branch)
	}
}
