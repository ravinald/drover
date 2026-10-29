package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
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

// branchOf is the branch item id renders to.
func branchOf(t *testing.T, id string) string {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	return branch
}

// waitForLine waits until the file at path holds line.
func waitForLine(t *testing.T, path, line string) {
	t.Helper()
	for until := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && slices.Contains(strings.Split(string(b), "\n"), line) { // #nosec G304 -- the fixture's own file
			return
		}
		if time.Now().After(until) {
			t.Fatalf("%s never listed %s", path, line)
		}
	}
}

// appendLine adds line to the file at path.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the fixture's own file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// pendingSecond is item 2 as the queue holds it.
func pendingSecond(t *testing.T, s *session) queued {
	t.Helper()
	it, err := s.item("2")
	if err != nil {
		t.Fatal(err)
	}
	return newQueued(it, true)
}

func noLanding(string, *queuedMerge) error { return nil }

// pollSettled runs pollMerges until no forge read it started is still in
// flight, as the dispatcher's passes do.
func pollSettled(t *testing.T, s *session, pending *[]queued, merging map[string]*queuedMerge,
	account func(DriveItem) error, land func(string, *queuedMerge) error,
) error {
	t.Helper()
	for until := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if err := s.pollMerges(pending, merging, time.Time{}, account, land); err != nil {
			return err
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(merging)), func(m *queuedMerge) bool { return m.read != nil }) {
			return nil
		}
		if time.Now().After(until) {
			t.Fatal("a forge read never answered")
		}
	}
}

// An item depending on two queued merges keeps both held, whichever its deps
// line names first, so the one it lists second is not dropped while the first
// is still queued.
func TestAQueuedMergeStaysHeldWhileAnyPendingItemDependsOnIt(t *testing.T) {
	queuedMergeProject(t, "")
	write(t, filepath.Join("work", "02-second.md"), "---\ntitle: Second\ndeps: 3 1\n---\n\n1. Do it.\n")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	pending := []queued{pendingSecond(t, s)}
	now := time.Now()
	merging := map[string]*queuedMerge{
		"1": {PR: 7, Branch: "first", Since: now, polled: now},
		"3": {PR: 8, Branch: "third", Since: now, polled: now.Add(-time.Hour)},
	}
	t.Setenv("DROVER_TEST_OPEN", "first third")
	if err := pollSettled(t, s, &pending, merging, func(DriveItem) error { return nil }, noLanding); err != nil {
		t.Fatal(err)
	}
	if merging["1"] == nil || merging["3"] == nil {
		t.Fatalf("a queued dependency was dropped while item 2 (deps: 3 1) still depends on both: %v", slices.Collect(maps.Keys(merging)))
	}
	if len(pending) != 1 {
		t.Errorf("item 2 left the queue: %+v", pending)
	}
}

// A dependent of two queued merges, its deps line in the reverse of the order
// they land, waits for both and merges in the same drive.
func TestADependentOfTwoQueuedMergesWaitsForBoth(t *testing.T) {
	queuedMergeProject(t, "")
	write(t, filepath.Join("work", "02-second.md"), "---\ntitle: Second\ndeps: 3 1\n---\n\n1. Do it. REVIEW-PASSES-2\n")
	write(t, filepath.Join("work", "03-third.md"), "---\ntitle: Third\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-3\n")
	one, three := branchOf(t, "1"), branchOf(t, "3")
	t.Setenv("DROVER_TEST_QUEUE", one+" "+three)
	lands := filepath.Join(t.TempDir(), "lands")
	t.Setenv("DROVER_TEST_LANDS_AFTER", lands)

	type result struct {
		rows []DriveItem
		evs  []DriveEvent
		err  error
	}
	ran := make(chan result, 1)
	go func() {
		rows, evs, err := driveWithEvents(t, "1", "3", "2")
		ran <- result{rows, evs, err}
	}()
	waitForLine(t, os.Getenv("DROVER_TEST_QUEUED"), one)
	waitForLine(t, os.Getenv("DROVER_TEST_QUEUED"), three)
	appendLine(t, lands, three)
	waitForLine(t, os.Getenv("DROVER_TEST_MERGED"), three)
	time.Sleep(200 * time.Millisecond)
	appendLine(t, lands, one)
	var r result
	select {
	case r = <-ran:
	case <-time.After(2 * time.Minute):
		t.Fatal("the drive never exited")
	}
	if r.err != nil {
		t.Fatalf("drive: %v", r.err)
	}
	for _, e := range r.evs {
		if e.Event == evSkipped {
			t.Errorf("a skipped event arrived: %+v", e)
		}
	}
	if two := latestRows(r.rows)["2"]; !two.Merged || !two.Finished {
		t.Errorf("item 2 ends %q merged=%v finished=%v (%s), want merged in this drive", two.Outcome, two.Merged, two.Finished, two.Reason)
	}
}

// A merge_wait shorter than checksPoll ends on time: the bound is read on every
// pass, not only on a pass that polls the forge.
func TestAnExpiredMergeWaitReleasesBeforeTheNextPoll(t *testing.T) {
	queuedMergeProject(t, `merge_wait = "1ms"`)
	checksPoll = time.Hour
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	pending := []queued{pendingSecond(t, s)}
	merging := map[string]*queuedMerge{"1": {PR: 7, Branch: "first", Since: time.Now().Add(-time.Minute), polled: time.Now()}}
	var skipped []DriveItem
	if err := s.pollMerges(&pending, merging, time.Time{},
		func(d DriveItem) error { skipped = append(skipped, d); return nil }, noLanding); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "[drive] merge_wait") {
		t.Fatalf("an expired merge_wait still holds item 2 until the next poll: pending %d, skipped %+v", len(pending), skipped)
	}
}

// The drive wakes at the bound as well as at the poll, so a merge_wait shorter
// than checksPoll ends the wait when it says.
func TestAShortMergeWaitIsHonoredBetweenPolls(t *testing.T) {
	queuedMergeProject(t, `merge_wait = "300ms"`)
	checksPoll = time.Hour
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")

	start := time.Now()
	rows, _, err := driveWithEvents(t, "1", "2")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("the drive took %s, as if it waited for the next poll", took)
	}
	if two := latestRows(rows)["2"]; two.Outcome != outcomeSkipped || !strings.Contains(two.Reason, "[drive] merge_wait") {
		t.Errorf("item 2 is %q (%s), want skipped naming merge_wait", two.Outcome, two.Reason)
	}
}

// A drive whose deadline arrives between two polls skips each dependent it
// held, naming the deadline, and leaves the dep ready_to_merge.
func TestTheDeadlineSkipsADependentOfAQueuedMerge(t *testing.T) {
	queuedMergeProject(t, "")
	checksPoll = time.Hour
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")

	rows, _, err := driveWithEvents(t, "1", "2", "--for", "6s")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	got := latestRows(rows)
	if one := got["1"]; !one.MergeQueued || one.Outcome != outcomeReviewed {
		t.Fatalf("setup: item 1 is %q queued=%v, want ready_to_merge with its merge queued", one.Outcome, one.MergeQueued)
	}
	two := got["2"]
	if two.Outcome != outcomeSkipped {
		t.Fatalf("the deadline left item 2 %q with no skip (%s)", two.Outcome, two.Reason)
	}
	for _, want := range []string{"blocked by 1", "#7", "the drive's deadline", "1 stays ready_to_merge"} {
		if !strings.Contains(two.Reason, want) {
			t.Errorf("the skip reason does not carry %q: %s", want, two.Reason)
		}
	}
}

// Under --follow with nothing pending, a wait on a queued merge still ends at
// the deadline rather than at the next poll.
func TestTheDeadlineEndsAFollowWaitWithNothingPending(t *testing.T) {
	queuedMergeProject(t, "")
	checksPoll = time.Hour
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")
	t.Cleanup(func() { driveFollow = false })

	start := time.Now()
	var err error
	errOut := captureErr(t, func() { _, _, err = driveWithEvents(t, "--follow", "--for", "4s", "1") })
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, errOut)
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("the drive took %s, as if it waited for the next poll", took)
	}
	if !strings.Contains(errOut, "· no longer waiting for 1: the drive waited") || !strings.Contains(errOut, "the drive's deadline") {
		t.Errorf("the --follow wait did not end at the deadline:\n%s", errOut)
	}
}

// A poll that sees a merge land hands the finish to a slot and returns, so the
// dispatcher answers controls while another item holds the trunk stage. The
// merge stays queued, holding its dependent, until that slot reports back.
func TestPollingALandedMergeLeavesTheDispatcherFree(t *testing.T) {
	branch := queuedMergeProject(t, "")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	pending := []queued{pendingSecond(t, s)}
	write(t, os.Getenv("DROVER_TEST_MERGED"), branch+"\n")
	merging := map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now(), polled: time.Now().Add(-time.Hour)}}
	sched := newDriveSched()
	landed := make(chan DriveItem, 1)
	land := func(dep string, m *queuedMerge) error {
		it, err := s.item(dep)
		if err != nil {
			return err
		}
		w, err := s.fork(nil)
		if err != nil {
			return err
		}
		go func() { landed <- w.landQueuedMerge(t.Context(), sched, it, m) }()
		return nil
	}
	sched.trunk.Lock()
	done := make(chan error, 1)
	go func() {
		done <- pollSettled(t, s, &pending, merging, func(DriveItem) error { return nil }, land)
	}()
	select {
	case err := <-done:
		if err != nil {
			sched.trunk.Unlock()
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		sched.trunk.Unlock()
		<-done
		t.Fatal("pollMerges blocked on the trunk stage another item holds")
	}
	select {
	case d := <-landed:
		sched.trunk.Unlock()
		t.Fatalf("the merge finished while another item held the trunk stage: %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
	if m := merging["1"]; m == nil || !m.landing || len(pending) != 1 {
		t.Errorf("the landing merge stopped holding item 2: merging %+v, pending %d", m, len(pending))
	}
	sched.trunk.Unlock()
	select {
	case d := <-landed:
		if d.ID != "1" || !d.Merged {
			t.Errorf("the landing slot reported %+v", d)
		}
	case <-time.After(time.Minute):
		t.Fatal("the merge never finished once the trunk stage was free")
	}
}

// A --stop sent while a landed merge waits for the trunk stage another item
// holds is answered on the usual poll, and the drive exits once that item's
// stage ends, leaving the merge for the next drive's reconciliation.
func TestAStopIsAnsweredWhileALandedMergeWaitsForTheTrunk(t *testing.T) {
	queuedMergeProject(t, "")
	ownState(t)
	write(t, filepath.Join("work", "03-third.md"), "---\ntitle: Third\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-3\n")
	one, three := branchOf(t, "1"), branchOf(t, "3")
	lands := filepath.Join(t.TempDir(), "lands")
	t.Setenv("DROVER_TEST_LANDS_AFTER", lands)
	gates := t.TempDir()
	entered, release := filepath.Join(gates, "entered"), filepath.Join(gates, "release")
	gate, err := exec.LookPath("merge-gate")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := filepath.Join(gates, "merge-gate")
	if err := os.Rename(gate, wrapped); err != nil {
		t.Fatal(err)
	}
	stub(t, gate, `#!/bin/sh
if [ "$1" = "`+three+`" ]; then
  touch "`+entered+`"
  n=0
  while [ ! -e "`+release+`" ] && [ $n -lt 600 ]; do sleep 0.1; n=$((n + 1)); done
fi
exec "`+wrapped+`" "$@"
`)

	ran := make(chan driveRun, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, "--jobs", "2", "1", "3", "2")
		ran <- driveRun{evs, stderr, err}
	}()
	waitForLine(t, os.Getenv("DROVER_TEST_QUEUED"), one)
	for until := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("item 3 never reached its merge gate")
		}
	}
	appendLine(t, lands, one)
	waitForLine(t, os.Getenv("DROVER_TEST_MERGED"), one)
	time.Sleep(100 * time.Millisecond)

	stopped := make(chan error, 1)
	go func() { stopped <- stopDrive(io.Discard) }()
	select {
	case err := <-stopped:
		if err != nil {
			write(t, release, "")
			t.Fatalf("--stop: %v", err)
		}
	case <-time.After(15 * time.Second):
		write(t, release, "")
		<-stopped
		t.Fatal("--stop went unanswered while a landed merge waited for the trunk stage")
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
	if eventIndex(r.evs, evDispatched, "2") >= 0 {
		t.Errorf("item 2 was dispatched after --stop: %+v", r.evs)
	}
	if !strings.Contains(r.stderr, "before finishing it, so the next drive's reconciliation finishes it") {
		t.Errorf("the stopped drive did not leave the merge to reconciliation:\n%s", r.stderr)
	}
}

// Every forge read a poll makes, and the finish of a merge it saw land, happen
// with the merge listed as running in the drive's lease, so finish,
// mark-merged, unmark-merged and open-pr from another process refuse it.
func TestPollingAQueuedMergeListsItAsRunning(t *testing.T) {
	s, path, snaps := reconcileFixture(t)
	driveFollow = true
	t.Cleanup(func() { driveFollow = false })
	branch := branchOf(t, "1")
	var pending []queued
	merging := map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now(), polled: time.Now().Add(-time.Hour)}}
	var finished []DriveItem
	land := func(dep string, m *queuedMerge) error {
		it, err := s.item(dep)
		if err != nil {
			return err
		}
		finished = append(finished, s.landQueuedMerge(t.Context(), newDriveSched(), it, m))
		return nil
	}
	if err := pollSettled(t, s, &pending, merging, func(DriveItem) error { return nil }, land); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(snaps) // #nosec G304 -- the fixture's own file
	if err != nil {
		t.Fatal(err)
	}
	var reads int
	for _, chunk := range strings.SplitAfter(string(b), "}") {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		reads++
		if !strings.Contains(chunk, `"running":["1"]`) {
			t.Errorf("a forge call ran with queued merge 1 unlisted: %s", chunk)
		}
	}
	if reads == 0 {
		t.Fatal("no forge call was observed")
	}
	l, _, err := lease.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(finished) == 1 && !slices.Contains(l.Running, "1") {
		t.Errorf("the lease freed 1 before the dispatcher accounted its merge: %+v", l.Running)
	}
	if len(finished) == 0 && slices.Contains(l.Running, "1") {
		t.Errorf("a poll that saw no landing left 1 listed as running: %+v", l.Running)
	}
}

// A queued merge that lands moves the trunk as any merge does, so every other
// open drive pull request is caught up before the dependent dispatches.
func TestAQueuedMergeThatLandsCatchesUpTheOpenPullRequests(t *testing.T) {
	queuedMergeProject(t, "")
	write(t, filepath.Join("work", "03-third.md"), "---\ntitle: Third\ndeps: 1\n---\n\n1. Do it. REVIEW-PASSES-3\n")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	openSibling(t, s.cfg.Root, "two.txt", "two")
	t.Setenv("DROVER_TEST_LANDS_ON", "3")

	rows, evs, err := driveWithEvents(t, "1", "3")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if one := latestRows(rows)["1"]; !one.Finished {
		t.Fatalf("setup: item 1 did not finish in this drive: %q (%s)", one.Outcome, one.Reason)
	}
	// The finish stage of the landing slot, which catches up before the
	// dispatcher accounts the merge, as an item's own merge does.
	landedAt := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evStage && e.Stage == stageFinish && e.ID == "1" })
	dispatched := eventIndex(evs, evDispatched, "3")
	caught := slices.IndexFunc(evs, func(e DriveEvent) bool {
		return e.Event == evStage && e.Stage == stageCatchUp && e.ID == "2"
	})
	if landedAt < 0 || caught < landedAt || dispatched < caught {
		t.Fatalf("item 2's pull request was not caught up between 1 landing and 3 dispatching: catch-up at %d, 1 finishing at %d, 3 dispatched at %d",
			caught, landedAt, dispatched)
	}
	if c := evs[caught]; c.Result != "passed" || !strings.Contains(c.Reason, "pushed") {
		t.Errorf("item 2's catch-up after 1 landed is %q (%s), want verified and pushed", c.Result, c.Reason)
	}
}

// slowMergedReads makes every read of a merged pull request take delay, as a
// forge near its 30-second timeout does.
func slowMergedReads(t *testing.T, delay string) {
	t.Helper()
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := filepath.Join(t.TempDir(), "gh")
	if err := os.Rename(ghPath, wrapped); err != nil {
		t.Fatal(err)
	}
	stub(t, ghPath, `#!/bin/sh
case "$*" in *'--state merged'*) sleep `+delay+` ;; esac
exec "`+wrapped+`" "$@"
`)
}

// A poll due on a pass the bound has already ended asks the forge nothing and
// finishes nothing: the dependent skips naming the bound, whether merge_wait or
// the drive's deadline ended it.
func TestAnExpiredBoundWinsOverADuePoll(t *testing.T) {
	for _, c := range []struct {
		name, extra, want string
		deadline          time.Time
	}{
		{"merge_wait", `merge_wait = "1ms"`, "[drive] merge_wait", time.Time{}},
		{"deadline", "", "the drive's deadline", time.Now().Add(-time.Second)},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := queuedMergeProject(t, c.extra)
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			pending := []queued{pendingSecond(t, s)}
			write(t, os.Getenv("DROVER_TEST_MERGED"), branch+"\n")
			merging := map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now().Add(-time.Minute), polled: time.Now().Add(-time.Hour)}}
			landed := false
			var skips []DriveItem
			if err := s.pollMerges(&pending, merging, c.deadline, func(d DriveItem) error { skips = append(skips, d); return nil },
				func(string, *queuedMerge) error { landed = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if landed || len(skips) != 1 || len(merging) != 0 {
				t.Fatalf("an expired wait with a poll due launched finish=%v, skips=%d, merging=%d", landed, len(skips), len(merging))
			}
			for _, want := range []string{c.want, "#7", "1 stays ready_to_merge"} {
				if !strings.Contains(skips[0].Reason, want) {
					t.Errorf("the skip reason does not carry %q: %s", want, skips[0].Reason)
				}
			}
		})
	}
}

// A forge read that answers after the bound is discarded: the wait ends at the
// bound while the read is in flight, and the merge it reports is left for the
// next drive's reconciliation.
func TestAReadCrossingTheBoundFinishesNothing(t *testing.T) {
	branch := queuedMergeProject(t, `merge_wait = "300ms"`)
	slowMergedReads(t, "1")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	pending := []queued{pendingSecond(t, s)}
	write(t, os.Getenv("DROVER_TEST_MERGED"), branch+"\n")
	merging := map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now(), polled: time.Now().Add(-time.Hour)}}
	landed := false
	var skips []DriveItem
	start := time.Now()
	if err := pollSettled(t, s, &pending, merging, func(d DriveItem) error { skips = append(skips, d); return nil },
		func(string, *queuedMerge) error { landed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("the wait ended after %s, when the read answered rather than at the bound", took)
	}
	if landed || len(skips) != 1 || !strings.Contains(skips[0].Reason, "[drive] merge_wait") {
		t.Fatalf("a read crossing the bound landed=%v, skips %+v", landed, skips)
	}

	// An answer already waiting when the bound is read is discarded too.
	pending = []queued{pendingSecond(t, s)}
	answer := make(chan mergeRead, 1)
	answer <- mergeRead{pr: 7, merged: true}
	merging = map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now().Add(-time.Minute), polled: time.Now(), read: answer}}
	skips = nil
	if err := s.pollMerges(&pending, merging, time.Time{}, func(d DriveItem) error { skips = append(skips, d); return nil },
		func(string, *queuedMerge) error { landed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if landed || len(skips) != 1 {
		t.Fatalf("a late merged answer landed=%v, skips %d", landed, len(skips))
	}
}

// A forge read never holds the dispatcher: pollMerges returns at once while gh
// takes seconds to answer, and collects the answer on a later pass.
func TestASlowForgeReadLeavesTheDispatcherFree(t *testing.T) {
	branch := queuedMergeProject(t, "")
	slowMergedReads(t, "1")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	pending := []queued{pendingSecond(t, s)}
	t.Setenv("DROVER_TEST_OPEN", branch)
	merging := map[string]*queuedMerge{"1": {PR: 7, Branch: branch, Since: time.Now(), polled: time.Now().Add(-time.Hour)}}
	start := time.Now()
	if err := s.pollMerges(&pending, merging, time.Time{}, func(DriveItem) error { return nil }, noLanding); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("the dispatcher was held %s by a forge read (enqueuePoll=%s)", took, enqueuePoll)
	}
	if merging["1"] == nil || merging["1"].read == nil {
		t.Fatalf("no read was started: %+v", merging)
	}
	if err := pollSettled(t, s, &pending, merging, func(DriveItem) error { return nil }, noLanding); err != nil {
		t.Fatal(err)
	}
	if merging["1"] == nil || len(pending) != 1 {
		t.Errorf("an open pull request stopped holding item 2: merging %+v, pending %d", merging, len(pending))
	}
}

// A --stop sent while a forge read of a queued merge is slow is answered on the
// usual poll, and nothing waiting on the merge dispatches.
func TestAStopIsAnsweredWhileAForgeReadIsSlow(t *testing.T) {
	queuedMergeProject(t, "")
	ownState(t)
	one := branchOf(t, "1")
	t.Setenv("DROVER_TEST_LANDS_ON", "1000")

	ran := make(chan driveRun, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, "1", "2")
		ran <- driveRun{evs, stderr, err}
	}()
	waitForLine(t, os.Getenv("DROVER_TEST_QUEUED"), one)
	time.Sleep(200 * time.Millisecond)
	slowMergedReads(t, "10")
	time.Sleep(200 * time.Millisecond)

	stopped := make(chan error, 1)
	start := time.Now()
	go func() { stopped <- stopDrive(io.Discard) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("--stop: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("--stop went unanswered while a forge read was in flight")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("--stop took %s, as if it waited for the forge read", took)
	}
	var r driveRun
	select {
	case r = <-ran:
	case <-time.After(2 * time.Minute):
		t.Fatal("the stopped drive never exited")
	}
	if r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
	if eventIndex(r.evs, evDispatched, "2") >= 0 {
		t.Errorf("item 2 was dispatched after --stop: %+v", r.evs)
	}
}

// An interrupt between the forge's answer and the finish finishes nothing, at
// the default --jobs 1 where no watcher drains the schedule: no completion row,
// and the merge is left to the next drive's reconciliation.
func TestAnInterruptedLandingFinishesNothing(t *testing.T) {
	s, _, _ := reconcileFixture(t)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := s.landQueuedMerge(ctx, newDriveSched(), it, &queuedMerge{PR: 7, Branch: branch, Since: time.Now()})
	if d.Finished {
		t.Fatalf("a canceled landing finished the merge: %+v", d)
	}
	if !strings.Contains(d.Reason, "the next drive's reconciliation finishes it") {
		t.Errorf("the landing did not leave the merge to reconciliation: %s", d.Reason)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.ledger.State("1").Merged {
		t.Error("a canceled landing wrote the completion row")
	}
}

// A catch-up that finds the trunk red after a landing drains the drive before
// the landing lets go of the trunk, so the item next in line for the stage
// parks rather than catching up to it.
func TestALandingThatFindsTheTrunkRedDrainsBeforeReleasingIt(t *testing.T) {
	branch := queuedMergeProject(t, "")
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	openSibling(t, s.cfg.Root, "two.txt", "two")
	rows, _, err := driveWithEvents(t, "1")
	if err != nil {
		t.Fatal(err)
	}
	if !latestRows(rows)["1"].MergeQueued {
		t.Fatal("setup: item 1's merge was not queued")
	}
	if out, err := exec.Command("gh", "pr", "merge", branch).CombinedOutput(); err != nil {
		t.Fatalf("land: %v %s", err, out)
	}
	if s, err = open(); err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	check, err := exec.LookPath("check")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, check, "#!/bin/sh\necho red\nexit 1\n")

	sched := newDriveSched()
	d := s.landQueuedMerge(t.Context(), sched, it, &queuedMerge{PR: 7, Branch: branch, Since: time.Now()})
	if d.stop == nil {
		t.Fatalf("setup: the catch-up raised no stop: %+v", d)
	}
	if sched.draining() == nil {
		t.Fatalf("the landing returned stop=%v and released the trunk with the schedule still admitting work", d.stop)
	}
	if err := sched.enter(0, lease.Hold{ID: "3", At: stageCatchUp}); err == nil {
		sched.leave(0)
		t.Fatal("the next item entered the trunk stage after the landing found the trunk red")
	}
}

// An item whose turn came while a landing held the trunk finds the drain that
// landing raised once it gets the trunk, and enters nothing.
func TestAnItemWaitingOnTheTrunkFindsADrainRaisedUnderIt(t *testing.T) {
	sched := newDriveSched()
	sched.trunk.Lock()
	entered := make(chan error, 1)
	go func() { entered <- sched.enter(0, lease.Hold{ID: "3", At: stageCatchUp}) }()
	time.Sleep(100 * time.Millisecond)
	sched.drain(errors.New("the trunk fails the verify command on its own"))
	sched.trunk.Unlock()
	select {
	case err := <-entered:
		if err == nil {
			sched.leave(0)
			t.Fatal("the item entered the trunk stage after a drain raised while it waited for the lock")
		}
	case <-time.After(time.Minute):
		t.Fatal("the waiting item never returned")
	}
	if !sched.trunk.TryLock() {
		t.Fatal("the refused item kept the trunk")
	}
}

// The text summary counts each item once, at what it came to: a merge queued
// and then landed in the same drive is merged, with nothing left to run.
func TestTheDriveSummaryReportsALandedMergeOnce(t *testing.T) {
	s := driveProject(t)
	var out string
	errOut := captureErr(t, func() {
		out = capture(t, func() {
			s.driveSummary([]DriveItem{
				{ID: "1", Outcome: outcomeReviewed, MergeQueued: true, PR: 7},
				{ID: "1", Outcome: outcomeMergedAtForge, Merged: true, Finished: true, PR: 7},
			}, DriveTotals{})
		})
	})
	if !strings.Contains(errOut, "1 attempted · 0 ready to merge · 1 merged immediately") {
		t.Errorf("one landed merge is not counted once:\n%s", errOut)
	}
	if strings.Contains(errOut, "the next drover drive finishes it") || strings.Count(errOut, "Next steps:") != 1 {
		t.Errorf("the superseded ready_to_merge advice is still printed:\n%s", errOut)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("the item table prints one row per report, not per item:\n%s", out)
	}
}
