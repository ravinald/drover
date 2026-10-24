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
	t.Cleanup(func() { driveEvents = false })
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
	resetDrive(t)
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
	resetDrive(t)
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
	drivesRunning(ds, l, true)
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
		if i, held := dispatchable(c.pending, map[int]queued{}, operator); i != c.want {
			t.Errorf("%s: dispatchable = %d (held %+v), want %d", c.name, i, held, c.want)
		}
	}
	running := map[int]queued{1: q(5, "")}
	if i, held := dispatchable([]queued{q(2, "1"), q(3, "")}, running, operator); i != -1 {
		t.Errorf("an item overlapping a running one dispatched at %d (held %+v)", i, held)
	}
	delete(operator, "1")
	if i, held := dispatchable([]queued{q(2, "1"), q(3, "")}, map[int]queued{}, operator); i != 0 {
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
