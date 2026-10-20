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
	"strings"
	"testing"
	"time"

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
