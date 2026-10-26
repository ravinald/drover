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

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runner"
)

// twoDrives drives item 2 of mergeProject twice. Its review records nothing, so
// each drive parks it, and a named park is driven again: two drives, one item,
// one park row each.
func twoDrives(t *testing.T) (root string, events string) {
	t.Helper()
	mergeProject(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"2", "--label", "first night"}, {"2", "--label", "", "--events"}} {
		out := capture(t, func() {
			if err := execute(t, append([]string{"drive", "--json"}, args...)...); err != nil {
				t.Fatalf("drive %v: %v", args, err)
			}
		})
		events = out
	}
	return root, events
}

func loadLedger(t *testing.T, root string) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Load(filepath.Join(root, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestTwoDrivesInOneProjectAreD1AndD2(t *testing.T) {
	root, events := twoDrives(t)
	ds := loadLedger(t, root).Drives()
	if len(ds) != 2 || ds[0].ID != "D2" || ds[1].ID != "D1" {
		t.Fatalf("the ledger records drives %+v, want D2 then D1", ds)
	}
	first := ds[1]
	if first.Label != "first night" || len(first.Queued) != 1 || first.Queued[0] != "2" || first.Jobs != 1 {
		t.Errorf("D1's start row reads label %q, queued %v, jobs %d; want \"first night\", [2], 1",
			first.Label, first.Queued, first.Jobs)
	}
	if first.Exit == nil || *first.Exit != 0 || len(first.Items) != 1 || first.Items[0] != (ledger.Outcome{ID: "2", Outcome: outcomeParked}) {
		t.Errorf("D1's end row reads exit %v, items %+v; want 0 and 2 parked", first.Exit, first.Items)
	}

	var seen int
	for _, line := range strings.Split(events, "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) != nil || e.Event == "" {
			continue
		}
		seen++
		if e.Drive != "D2" {
			t.Errorf("the %s event names drive %q, want D2: %s", e.Event, e.Drive, line)
		}
	}
	if seen == 0 {
		t.Fatalf("drive --events wrote no event:\n%s", events)
	}
}

func TestADrivesParkRowCarriesItsID(t *testing.T) {
	root, _ := twoDrives(t)
	var parks []string
	for _, r := range loadLedger(t, root).Rows {
		if r.Park != "" {
			parks = append(parks, r.Drive)
		}
	}
	if len(parks) != 2 || parks[0] != "D1" || parks[1] != "D2" {
		t.Errorf("the park rows name drives %q, want [D1 D2]: a park has to say which drive wrote it", parks)
	}
	// Written outside a drive, a row names none.
	s := openIn(t, root)
	if err := s.record(ledger.Row{ID: "2", Changes: "x", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	l := loadLedger(t, root)
	if last := l.Rows[len(l.Rows)-1]; last.Drive != "" {
		t.Errorf("a row written outside a drive names drive %q", last.Drive)
	}
}

func TestStatusJSONNamesEachItemsDriveAndListsTheDrives(t *testing.T) {
	twoDrives(t)
	out := capture(t, func() {
		if err := execute(t, "status", "--json", "--no-tracker"); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	var report StatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json did not parse: %v\n%s", err, out)
	}
	drives := map[string]string{}
	for _, r := range report.Items {
		drives[r.ID] = r.Drive
	}
	if drives["2"] != "D2" || drives["1"] != "" {
		t.Errorf("status names item 2's drive %q and item 1's %q, want D2 and none", drives["2"], drives["1"])
	}
	if len(report.Drives) != 2 || report.Drives[0].ID != "D2" || report.Drives[1].ID != "D1" {
		t.Fatalf("status lists drives %+v, want D2 then D1", report.Drives)
	}
	if report.Drives[1].Label != "first night" || report.Drives[0].Running {
		t.Errorf("D1 reads label %q and D2 running=%v; want the label and no drive running",
			report.Drives[1].Label, report.Drives[0].Running)
	}
}

func TestDrivesAllReadsEveryRegisteredProject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	labels := map[string]string{}
	for _, name := range []string{"alpha", "beta"} {
		s := fixture(t, "")
		for i := range 2 {
			path := s.cfg.Abs(s.cfg.Paths.Ledger)
			id, err := ledger.NextDrive(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ledger.Append(path, ledger.Row{ID: ledger.DriveRowID, Drive: id,
				Label: name + string(rune('0'+i)), Queued: []string{"1"}, Jobs: 1}); err != nil {
				t.Fatal(err)
			}
		}
		labels[name] = name + "1"
		if err := projects.Append(reg, projects.Row{Project: name, Root: s.cfg.Root}); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(t.TempDir())

	out := capture(t, func() {
		if err := execute(t, "drives", "--all", "--json"); err != nil {
			t.Fatalf("drives --all: %v", err)
		}
	})
	var got []ProjectDrives
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("drives --all --json did not parse: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("drives --all read %d projects, want 2:\n%s", len(got), out)
	}
	for _, p := range got {
		if p.Error != "" || len(p.Drives) != 2 || p.Drives[0].ID != "D2" || p.Drives[0].Label != labels[p.Project] {
			t.Errorf("%s reads %+v (error %q), want D2 labelled %q then D1", p.Project, p.Drives, p.Error, labels[p.Project])
		}
	}
}

// registerDriven builds a fixture project holding one drive labeled name,
// registers it under name, and returns its root.
func registerDriven(t *testing.T, reg, name string) string {
	t.Helper()
	s := fixture(t, "")
	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	id, err := ledger.NextDrive(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(path, ledger.Row{ID: ledger.DriveRowID, Drive: id,
		Label: name, Queued: []string{"1"}, Jobs: 1}); err != nil {
		t.Fatal(err)
	}
	if err := projects.Append(reg, projects.Row{Project: name, Root: s.cfg.Root}); err != nil {
		t.Fatal(err)
	}
	return s.cfg.Root
}

func drivesAllJSON(t *testing.T) []ProjectDrives {
	t.Helper()
	out := capture(t, func() {
		if err := execute(t, "drives", "--all", "--json"); err != nil {
			t.Fatalf("drives --all: %v", err)
		}
	})
	var got []ProjectDrives
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("drives --all --json did not parse: %v\n%s", err, out)
	}
	return got
}

func TestDrivesAllIgnoresDroverRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	alpha := registerDriven(t, reg, "alpha")
	registerDriven(t, reg, "beta")
	t.Setenv(runner.EnvRoot, alpha)
	t.Chdir(t.TempDir())

	got := drivesAllJSON(t)
	if len(got) != 2 {
		t.Fatalf("drives --all read %d projects, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Error != "" || len(p.Drives) != 1 || p.Drives[0].Label != p.Project {
			t.Errorf("under %s=%s, %s reads %+v (error %q), want its own drive labeled %q",
				runner.EnvRoot, alpha, p.Project, p.Drives, p.Error, p.Project)
		}
	}
}

func TestDrivesAllSkipsARegisteredRootWithNoConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	alpha := registerDriven(t, reg, "alpha")
	bare := t.TempDir()
	if err := projects.Append(reg, projects.Row{Project: "bare", Root: bare}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runner.EnvRoot, alpha)
	t.Chdir(t.TempDir())

	got := drivesAllJSON(t)
	if len(got) != 1 || got[0].Project != "alpha" {
		t.Errorf("drives --all read %+v, want alpha alone: a root with no %s is not a project", got, config.FileName)
	}
}

func TestALabelWithANewlineIsRefused(t *testing.T) {
	driveProject(t)
	for _, label := range []string{"one\ntwo", strings.Repeat("x", labelMax+1)} {
		err := execute(t, "drive", "--label", label)
		if Code(err) != ExitRefused {
			t.Errorf("drive --label %q exited %d (%v), want %d", label, Code(err), err, ExitRefused)
		}
	}
	if ds := loadLedger(t, ".").Drives(); len(ds) != 0 {
		t.Errorf("a refused label allocated %+v", ds)
	}
}

// A drive that parks an item and takes it again from --enqueue holds one
// outcome for it, the latest: its end row names item 2 once, merged, and its
// exit summary counts it merged and leaves nothing undone for it. Item 1 holds
// the drive open on a gate while item 2 parks, so the enqueue lands in the
// same drive; item 2's first review records nothing and its second passes.
func TestARequeuedItemThatMergesHasOneOutcome(t *testing.T) {
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_REQUIRED", "404")
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-ID-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Do it too. REVIEW-ID-2\n")
	gate := filepath.Join(t.TempDir(), "go")
	reviews := t.TempDir()
	t.Setenv("DROVER_TEST_GATE", gate)
	t.Setenv("DROVER_TEST_REVIEWS", reviews)
	impl, err := exec.LookPath("impl")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, impl, `#!/bin/sh
id=$(printf '%s' "$*" | sed -n 's/.*REVIEW-ID-\([0-9][0-9]*\).*/\1/p' | head -n 1)
if [ "$id" = 1 ]; then while [ ! -e "$DROVER_TEST_GATE" ]; do sleep 0.05; done; fi
echo "work $$" > "implemented-$id.txt"
git add "implemented-$id.txt"
git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m 'the run did something'
echo '{"type":"result","subtype":"success","is_error":false}'
`)
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, rev, `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false}'
id=$(printf '%s' "$*" | sed -n 's/.*REVIEW-ID-\([0-9][0-9]*\).*/\1/p' | head -n 1)
n=$(cat "$DROVER_TEST_REVIEWS/$id" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "$DROVER_TEST_REVIEWS/$id"
if [ "$id" = 2 ] && [ $n -eq 1 ]; then exit 0; fi
printf '{"id":"%s","level":"reviewed","code_by":"agent","independent":true,"date":"2026-10-26"}\n' "$id" >> "$DROVER_ROOT/ledger.jsonl"
`)
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })

	type result struct {
		evs    []DriveEvent
		stderr string
		err    error
	}
	ran := make(chan result, 1)
	go func() {
		evs, stderr, err := driveEventsOf(t, "1", "2", "--jobs", "2")
		ran <- result{evs, stderr, err}
	}()

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		l, busy, _ := lease.Read(path)
		parked := slices.ContainsFunc(loadLedger(t, dir).Rows, func(r ledger.Row) bool { return r.ID == "2" && r.Park != "" })
		if busy && parked && slices.Equal(l.Running, []string{"1"}) && len(l.Queue) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drive never reached item 2 parked with item 1 running: %+v", l)
		}
	}
	if res, err := s.enqueue([]string{"2"}, ""); err != nil || len(res.queued) != 1 {
		t.Fatalf("enqueue 2: %+v, %v", res, err)
	}
	write(t, gate, "")
	r := <-ran
	if r.err != nil {
		t.Fatalf("drive: %v\n%s", r.err, r.stderr)
	}
	if eventIndex(r.evs, evParked, "2") < 0 {
		t.Fatalf("item 2 never parked, so the drive did not take it twice: %+v", r.evs)
	}

	var end *ledger.Row
	for _, row := range loadLedger(t, dir).Rows {
		if row.ID == ledger.DriveRowID && row.Exit != nil {
			end = &row
		}
	}
	if end == nil {
		t.Fatal("the drive wrote no end row")
	}
	var twos []ledger.Outcome
	for _, o := range end.Outcomes {
		if o.ID == "2" {
			twos = append(twos, o)
		}
	}
	if len(twos) != 1 || twos[0].Outcome != outcomeMergedByGate {
		t.Errorf("the end row holds %+v for item 2, want one %s: %+v", twos, outcomeMergedByGate, end.Outcomes)
	}

	last := r.evs[len(r.evs)-1]
	if last.Event != evExit || last.Summary == nil {
		t.Fatalf("the stream ends on %+v, want an exit carrying a summary", last)
	}
	if sum := last.Summary; sum.Merged != 2 || sum.Parked != 0 || sum.undone() {
		t.Errorf("the exit summary reads %+v, want 2 merged and nothing undone", sum)
	}
}

// An end row written before a drive held one outcome per id carries both of a
// requeued item's, and drives --json reports the latest alone, in the place of
// the first.
func TestDrivesJSONReportsARequeuedItemOnceAsItsLatestOutcome(t *testing.T) {
	proj := fixture(t, "")
	t.Chdir(proj.cfg.Root)
	path := proj.cfg.Abs(proj.cfg.Paths.Ledger)
	code := 0
	for _, r := range []ledger.Row{
		{ID: ledger.DriveRowID, Drive: "D13", Queued: []string{"B83", "E62"}, Date: ledger.Today()},
		{ID: ledger.DriveRowID, Drive: "D13", Exit: &code, Date: ledger.Today(), Outcomes: []ledger.Outcome{
			{ID: "B83", Outcome: outcomeParked}, {ID: "E62", Outcome: outcomeParked}, {ID: "B83", Outcome: outcomeMergedByGate},
		}},
	} {
		if err := ledger.Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	out := capture(t, func() {
		if err := execute(t, "drives", "--json"); err != nil {
			t.Fatalf("drives: %v", err)
		}
	})
	var ds []ledger.Drive
	if err := json.Unmarshal([]byte(out), &ds); err != nil {
		t.Fatalf("drives --json did not parse: %v\n%s", err, out)
	}
	want := []ledger.Outcome{{ID: "B83", Outcome: outcomeMergedByGate}, {ID: "E62", Outcome: outcomeParked}}
	if len(ds) != 1 || !slices.Equal(ds[0].Items, want) {
		t.Errorf("drives --json reports %+v, want D13 with items %+v", ds, want)
	}
}
