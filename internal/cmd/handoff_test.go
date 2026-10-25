package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// envAsDrover runs the test binary as drover itself; see TestMain.
const envAsDrover = "DROVER_TEST_AS_DROVER"

// asDrover is a script standing in for an installed drover at a path of its
// own: it runs this test binary as drover under the script's own name, which
// is the path a drive records it was started from. gen tells two builds apart.
func asDrover(t *testing.T, path string, gen int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stub(t, path, fmt.Sprintf("#!/bin/bash\n# build %d\nexec -a \"$0\" %q \"$@\"\n", gen, self))
}

// resumeOntoReplaced runs args as a drive from a stand-in binary, pauses it
// with item 1 held before its review, replaces the binary and resumes, and
// returns the drive's stderr and its lease as it was paused once the
// re-executed drive exits 0.
func resumeOntoReplaced(t *testing.T, args ...string) (string, lease.Lease) {
	t.Helper()
	enqueueFixture(t)
	_, stderr, before := reexecDrive(t, func() {}, args...)
	return stderr, before
}

// reexecDrive is resumeOntoReplaced on a fixture already built, calling during
// while item 1's implement run waits, and returning the drive's stdout too.
func reexecDrive(t *testing.T, during func(), args ...string) (string, string, lease.Lease) {
	t.Helper()
	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("DROVER_TEST_IMPL_WAIT", release)
	bin := filepath.Join(t.TempDir(), "drover")
	asDrover(t, bin, 1)

	var stdout, stderr bytes.Buffer
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), envAsDrover+"=1")
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	t.Cleanup(func() {
		_ = c.Process.Kill()
		<-exited
	})

	path := mustLeasePath(t)
	waitLease(t, path, func(l lease.Lease) bool { return slices.Contains(l.Running, "1") })
	for deadline := time.Now().Add(30 * time.Second); stageRows(t, "1", ledger.StageImplement) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("item 1's implement run never launched\n%s", stderr.String())
		}
	}
	during()
	if err := pauseDrive(io.Discard); err != nil {
		t.Fatalf("--pause: %v", err)
	}
	write(t, release, "")
	waitLease(t, path, func(l lease.Lease) bool {
		return slices.ContainsFunc(l.Holds, func(h lease.Hold) bool { return h.ID == "1" && h.At == ledger.StageReview })
	})
	before, _, _ := lease.Read(path)

	asDrover(t, bin, 2)
	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-exited:
		exited <- err
		if err != nil {
			t.Fatalf("drive: %v\n%s", err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("the re-executed drive never finished\n%s", stderr.String())
	}

	return stdout.String(), stderr.String(), before
}

// A drive resumed after its binary was replaced re-executes onto the new one
// in the same process: the held item goes on from its review without a second
// implement run, the queued item runs after it, and every row names the one
// drive id, under a second start row naming both binaries.
func TestAResumeOntoAReplacedBinaryReexecutesAndKeepsTheQueueAndId(t *testing.T) {
	log, before := resumeOntoReplaced(t, "drive", "1", "2", "--json", "--events")
	if !strings.Contains(log, "re-executing drive D1 onto") || !strings.Contains(log, "drive D1 re-executed onto") {
		t.Errorf("the drive never said it re-executed:\n%s", log)
	}
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var starts []ledger.Row
	for _, r := range l.Rows {
		if r.ID == ledger.DriveRowID && r.Exit == nil {
			starts = append(starts, r)
		}
		if r.ID == "1" || r.ID == "2" {
			if r.Drive != "" && r.Drive != before.Drive {
				t.Errorf("a row for %s names drive %s, want %s: %+v", r.ID, r.Drive, before.Drive, r)
			}
		}
	}
	if len(starts) != 2 || starts[1].Drive != "D1" || starts[1].Replaced == "" || starts[1].Version == "" ||
		starts[1].Version == starts[1].Replaced || !slices.Equal(starts[1].Queued, []string{"1", "2"}) {
		t.Errorf("start rows %+v, want a second D1 row naming both binaries over 1 and 2", starts)
	}
	ds := l.Drives()
	if len(ds) != 1 || ds[0].ID != "D1" || ds[0].Exit == nil || *ds[0].Exit != 0 || ds[0].Started != starts[0].TS {
		t.Errorf("drives %+v, want D1 alone, started at its first start row and ended exit 0", ds)
	}
	if n := l.Counts("1").ByStage[ledger.StageImplement]; n != 1 {
		t.Errorf("item 1 was implemented %d times, want once: it held at its review", n)
	}
	if l.Counts("1").ByStage[ledger.StageReview] == 0 {
		t.Error("item 1 was never reviewed after the re-exec")
	}
	if l.Counts("2").ByStage[ledger.StageImplement] == 0 {
		t.Error("item 2, queued at the re-exec, never ran")
	}
}

// --max-items counts the items a drive started, and a re-exec carries the held
// ones over already started: they go on in the new process without counting
// again, so the queued item still fits under the bound and runs.
func TestAReexecCountsHeldItemsOnceAgainstMaxItems(t *testing.T) {
	log, _ := resumeOntoReplaced(t, "drive", "1", "2", "--max-items", "2", "--json", "--events")
	if !strings.Contains(log, "drive D1 re-executed onto") {
		t.Fatalf("the drive never re-executed:\n%s", log)
	}
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if l.Counts("1").ByStage[ledger.StageReview] == 0 {
		t.Errorf("held item 1 was never reviewed after the re-exec:\n%s", log)
	}
	if l.Counts("2").ByStage[ledger.StageImplement] == 0 {
		t.Errorf("item 2 never ran under --max-items 2:\n%s", log)
	}
}

// A row and a scaffold item 1 filed before the re-exec fell in the window its
// dispatch opened in the process before, so the process after reports both on
// item 1 and totals them, rather than taking them for what was already there.
func TestFindingsFiledBeforeAReexecAreReportedAfterIt(t *testing.T) {
	enqueueFixture(t)
	cfg, err := os.ReadFile(config.FileName)
	if err != nil {
		t.Fatal(err)
	}
	conf := strings.Replace(string(cfg), "repo         = \"owner/example\"\n", "", 1)
	write(t, config.FileName, conf+"\n[register]\nbackend = \"file\"\n")
	ignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ".gitignore", string(ignore)+".drover/\n")
	if err := os.MkdirAll(".drover", 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(".drover", "register.jsonl"), "")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "a file register")
	git(t, dir, "push", "-q", "origin", "main")

	stdout, stderr, _ := reexecDrive(t, func() {
		write(t, filepath.Join(".drover", "register.jsonl"), registerLine("5", `"2"`)+"\n")
		write(t, filepath.Join("work", "05-found.md"), "---\ntitle: Found\ndeps:\n---\n\n"+
			strings.Join(stopBlock("STOP: requirements not yet written"), "\n")+"\nFound while working 1, First.\n\n1.\n")
	}, "drive", "1", "2", "--json", "--events")
	if !strings.Contains(stderr, "drive D1 re-executed onto") {
		t.Fatalf("the drive never re-executed:\n%s", stderr)
	}
	var st driveStream
	for _, line := range strings.Split(stdout, "\n") {
		var d DriveItem
		var tot DriveTotals
		switch {
		case strings.Contains(line, `"outcome"`) && json.Unmarshal([]byte(line), &d) == nil:
			st.items = append(st.items, d)
		case strings.HasPrefix(line, `{"findings_by_coverage"`) && json.Unmarshal([]byte(line), &tot) == nil:
			st.totals = &tot
		}
	}
	got := map[string]FiledFinding{}
	for _, f := range st.item(t, "1").Findings {
		got[f.Kind+":"+f.Ref] = f
	}
	if f := got["follow-up:5"]; !slices.Equal(f.FiledBy, []string{"1"}) {
		t.Errorf("item 1's findings = %+v, want #5 filed by 1", got)
	}
	if f := got["scaffold:5"]; !slices.Equal(f.FiledBy, []string{"1"}) {
		t.Errorf("item 1's findings = %+v, want scaffold 5 filed by 1", got)
	}
	if st.totals == nil || st.totals.FindingsByCoverage[coverageScaffold] != 1 ||
		st.totals.FindingsByCoverage[coverageQueued]+st.totals.FindingsByCoverage[coverageUnqueued] != 1 {
		t.Errorf("totals = %+v, want the row and the scaffold counted\n%s", st.totals, stdout)
	}
}

// trunkWaitStubs replaces the review stub with one keyed on a REVIEW-ID-<n>
// marker in the brief: item 1's first review waits for the file sendBack
// names and then sends the item back, and every other review passes. Each
// review is counted in reviews/<id>.
func trunkWaitStubs(t *testing.T) (sendBack, reviews string) {
	t.Helper()
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	sendBack = filepath.Join(t.TempDir(), "send-back")
	reviews = t.TempDir()
	t.Setenv("DROVER_TEST_SEND_BACK", sendBack)
	t.Setenv("DROVER_TEST_REVIEWS", reviews)
	stub(t, rev, `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false}'
id=$(printf '%s' "$*" | sed -n 's/.*REVIEW-ID-\([0-9][0-9]*\).*/\1/p' | head -n 1)
n=$(cat "$DROVER_TEST_REVIEWS/$id" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "$DROVER_TEST_REVIEWS/$id"
if [ "$id" = 1 ] && [ $n -eq 1 ]; then
  while [ ! -e "$DROVER_TEST_SEND_BACK" ]; do sleep 0.05; done
  printf '{"id":"1","changes":"send it back","date":"2026-10-23"}\n' >> "$DROVER_ROOT/ledger.jsonl"
  exit 0
fi
printf '{"id":"%s","level":"reviewed","code_by":"agent","independent":true,"date":"2026-10-23"}\n' "$id" >> "$DROVER_ROOT/ledger.jsonl"
`)
	return sendBack, reviews
}

func reviewCount(t *testing.T, reviews, id string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(reviews, id)) // #nosec G304 -- the test's own counter
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// An item waiting for its trunk turn behind a held item holds there, so a
// resume onto a replaced binary re-executes rather than waiting on it forever.
// Item 2's review passes while item 1, dispatched first, is still in review;
// item 1 is sent back and a pause holds it before its implement run. After the
// re-exec item 2 goes on from its trunk stage, unreviewed again, and takes its
// turn after item 1.
func TestAReexecAdoptsAnItemWaitingForItsTrunkTurn(t *testing.T) {
	resetDrive(t)
	isolate(t)
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_AUTO_MERGES", "1")
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-ID-1\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Do it too. REVIEW-ID-2\n")
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "2", Changes: "earlier", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	sendBack, reviews := trunkWaitStubs(t)

	bin := filepath.Join(t.TempDir(), "drover")
	asDrover(t, bin, 1)
	var stdout, stderr bytes.Buffer
	c := exec.Command(bin, "drive", "1", "2", "--jobs", "2", "--json")
	c.Env = append(os.Environ(), envAsDrover+"=1")
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	t.Cleanup(func() {
		_ = c.Process.Kill()
		<-exited
	})

	path := mustLeasePath(t)
	passed := func() bool {
		s, err := open()
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(s.ledger.Rows, func(r ledger.Row) bool { return r.ID == "2" && r.Pass == passPassed })
	}
	for deadline := time.Now().Add(60 * time.Second); !passed(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("item 2's review never passed\n%s", stderr.String())
		}
	}
	// From the pass row to the wait in enter is a ledger write and a lock;
	// the pause has to land after it for this to be the trunk wait.
	time.Sleep(time.Second)
	if err := pauseDrive(io.Discard); err != nil {
		t.Fatalf("--pause: %v", err)
	}
	write(t, sendBack, "")
	waitLease(t, path, func(l lease.Lease) bool {
		return slices.ContainsFunc(l.Holds, func(h lease.Hold) bool { return h.ID == "1" && h.At == ledger.StageImplement }) &&
			slices.ContainsFunc(l.Holds, func(h lease.Hold) bool { return h.ID == "2" && h.At == stageCatchUp && h.Verify != "" })
	})

	asDrover(t, bin, 2)
	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-exited:
		exited <- err
		if err != nil {
			t.Fatalf("drive: %v\n%s", err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("the drive never re-executed and finished\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "drive D1 re-executed onto") {
		t.Fatalf("the drive never re-executed:\n%s", stderr.String())
	}
	var rows []DriveItem
	for _, line := range strings.Split(stdout.String(), "\n") {
		var d DriveItem
		if strings.Contains(line, `"outcome"`) && json.Unmarshal([]byte(line), &d) == nil {
			rows = append(rows, d)
		}
	}
	if len(rows) != 2 || rows[0].ID != "1" || rows[1].ID != "2" || !rows[0].Finished || !rows[1].Finished {
		t.Fatalf("rows %+v, want 1 then 2, both finished\n%s", rows, stderr.String())
	}
	if n := reviewCount(t, reviews, "2"); n != 1 {
		t.Errorf("item 2 was reviewed %d times, want once: it held at its trunk stage", n)
	}
}

// A re-exec that has waited past reexecPatience on a running item that does
// not hold says so once, naming the item and its stage, and again only after
// another reexecPatience.
func TestAStalledReexecNamesWhatItWaitsOn(t *testing.T) {
	var out bytes.Buffer
	prev := events
	events = &eventSink{w: &out, cfg: &config.Config{}}
	t.Cleanup(func() { events = prev })
	events.stage("1", ledger.StageImplement)
	events.stage("2", ledger.StageReview)

	q := newDriveSched()
	now := time.Now()
	q.paused, q.reexec, q.reexecSince = true, "v9 (built 2026-10-23T00:00:00Z)", now
	q.holds["1"] = lease.Hold{ID: "1", At: ledger.StageImplement}
	running := map[int]queued{
		1: {it: backlog.Item{ID: backlog.ID{Num: 1}}, seq: 0},
		2: {it: backlog.Item{ID: backlog.ID{Num: 2}}, seq: 1},
	}

	sayReexecWaiting(q, running, now.Add(30*time.Second))
	sayReexecWaiting(q, running, now.Add(reexecPatience+time.Second))
	sayReexecWaiting(q, running, now.Add(reexecPatience+30*time.Second))

	var got []DriveEvent
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e DriveEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Event == evReexecWaiting {
			got = append(got, e)
		}
	}
	if len(got) != 1 || !slices.Equal(got[0].Waiting, []WaitingItem{{ID: "2", Stage: ledger.StageReview}}) ||
		!strings.Contains(got[0].Reason, "2 (in review)") {
		t.Fatalf("reexec_waiting events %+v, want one naming 2 in review", got)
	}
	sayReexecWaiting(q, running, now.Add(2*reexecPatience+2*time.Second))
	if n := strings.Count(out.String(), evReexecWaiting); n != 2 {
		t.Errorf("%d reexec_waiting events after a second minute, want 2", n)
	}
}
