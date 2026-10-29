package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// heldLease is where a holder child parks its handle. The lock lives on the
// descriptor, and os closes an unreachable *os.File from a finalizer, so a
// handle left to a blank identifier frees the lease at the child's next GC.
// A holder then parks in time.Sleep rather than select{}: with no timer
// pending, the runtime reads an empty select as a deadlock and exits, and the
// kernel releases the lease with the process.
var heldLease *lease.Handle

// Two variables, because the child does two different jobs: one runs a command
// against a tree this process is holding, the other holds a tree this process
// contends for. Neither is on clearedEnv, which TestMain unsets before any
// test body runs and so before the child could read one.
const (
	envLeaseRefuse = "DROVER_TEST_LEASE_REFUSE"
	envLeaseHold   = "DROVER_TEST_LEASE_HOLD"
	envTreeHold    = "DROVER_TEST_TREE_HOLD"
)

// leaseProject is draftProject's tree with git behind it.
//
// The git dir is what makes a cross-process test possible at all: TestMain
// points XDG_CONFIG_HOME at a fresh directory per process, so the per-user
// fallback would give the parent and the child two different lease files and a
// contention test that could never contend. `.git` is a path both agree on
// without sharing an environment, and it is where a real project's lease lives.
func leaseProject(t *testing.T) string {
	t.Helper()
	isolate(t)
	dir := draftProject(t, "")
	git(t, dir, "init", "-q", "-b", "main")
	return dir
}

// leasePath is the fixture's lease, asserted to be the one inside its git dir
// rather than the per-user fallback. Both sides go through EvalSymlinks because
// git reports a resolved path and the temp directory it is handed is a symlink
// on darwin. That is the behavior drover wants: two names for one tree must not
// produce two leases.
func leasePath(t *testing.T, dir string) string {
	t.Helper()
	path, err := lease.Path(dir, "")
	if err != nil {
		t.Fatalf("lease.Path: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(filepath.Dir(path)); got != want {
		t.Fatalf("lease at %s, want it under %s: the fixture is not resolving through its git dir", path, want)
	}
	if filepath.Base(path) != lease.FileName {
		t.Fatalf("lease file is %s, want %s", filepath.Base(path), lease.FileName)
	}
	return path
}

// relaunch runs this test binary again with one variable set, so the assertion
// below is made against a genuinely separate process. An in-process test cannot
// reach the kernel's release-on-death, which is the whole mechanism.
func relaunch(t *testing.T, name, value string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1") // #nosec G204 -- this binary
	c.Env = append(os.Environ(), name+"="+value)
	return c
}

// childExit ends a child branch the way drover's own main does, and takes
// TestMain's temp directory with it: os.Exit runs no deferred cleanup, and a
// suite that leaked one per relaunch would fill /tmp over a week of runs.
func childExit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "drover:", err)
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(Code(err))
}

// TestASecondProcessRefusesACheckoutThisOneHolds is the collision that remains,
// run for real: a run holds the tree with a session on its branch, and the
// operator types finish in another terminal. The exit code and the pid in the
// message are the contract: a refusal the operator has to take to ps is not one
// they can act on.
func TestASecondProcessRefusesACheckoutThisOneHolds(t *testing.T) {
	if dir := os.Getenv(envLeaseRefuse); dir != "" {
		if err := os.Chdir(dir); err != nil {
			childExit(err)
		}
		root.SetArgs([]string{"finish", "1"})
		childExit(Execute())
	}

	dir := leaseProject(t)
	h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "run", Item: "1", Root: dir, Branch: "main"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	out, err := relaunch(t, envLeaseRefuse, dir).CombinedOutput()
	if err == nil {
		t.Fatalf("finish exited 0 against a held checkout:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("finish did not run: %v\n%s", err, out)
	}
	if exit.ExitCode() != ExitRefused {
		t.Errorf("finish exited %d, want %d:\n%s", exit.ExitCode(), ExitRefused, out)
	}
	for _, want := range []string{
		"drover run 1",
		fmt.Sprintf("pid %d", os.Getpid()),
		fmt.Sprintf("kill %d", os.Getpid()),
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the refusal does not name %q, so it sends the operator to ps:\n%s", want, out)
		}
	}
}

// TestKillingTheHolderFreesTheLeaseAtOnce covers the release path no in-process
// test reaches. SIGKILL runs nothing the holder wrote, so what frees the lease
// is the kernel closing the descriptor, which is why there is no stale state for
// a --force to rescue and no reaper to write.
func TestKillingTheHolderFreesTheLeaseAtOnce(t *testing.T) {
	if dir := os.Getenv(envLeaseHold); dir != "" {
		path, err := lease.Path(dir, "")
		if err != nil {
			childExit(err)
		}
		heldLease, err = lease.Acquire(path, lease.Lease{Command: "drive", Root: dir})
		if err != nil {
			childExit(err)
		}
		runtime.GC()
		runtime.GC()
		fmt.Println("held")
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
	}

	dir := leaseProject(t)
	path := leasePath(t, dir)
	child := relaunch(t, envLeaseHold, dir)
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	if line, err := bufio.NewReader(pipe).ReadString('\n'); err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("the holder never took the lease: %q %v", line, err)
	}

	l, busy, err := lease.Read(path)
	switch {
	case err != nil:
		t.Fatalf("read: %v", err)
	case !busy:
		t.Fatal("the lease reads free while a live process holds it")
	case l.PID != child.Process.Pid:
		t.Errorf("the lease names pid %d, the holder is %d", l.PID, child.Process.Pid)
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Process.Wait(); err != nil {
		t.Fatal(err)
	}

	// No retry and no sleep. A lease that needed either would be one drover was
	// inferring liveness for rather than being told.
	h, err := lease.Acquire(path, lease.Lease{Command: "run", Item: "1", Root: dir})
	if err != nil {
		t.Fatalf("a killed holder left the lease unacquirable: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })
	if h.Lease.PID != os.Getpid() {
		t.Errorf("the payload still names %d: the dead holder's account was reported rather than overwritten", h.Lease.PID)
	}
}

// TestStatusReportsAHeldLeaseAndRefusesNothing pins the half of the rule that
// is not a refusal. status is how an operator finds out why the branch under
// them moved, and a read-only command that exited 3 on somebody else's work
// would be useless for exactly that.
func TestStatusReportsAHeldLeaseAndRefusesNothing(t *testing.T) {
	dir := leaseProject(t)
	h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "drive", Root: dir, Branch: "main"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	out := capture(t, func() {
		if err := execute(t, "status"); err != nil {
			t.Fatalf("status refused a held checkout: %v", err)
		}
	})
	for _, want := range []string{"also here", "drover drive", fmt.Sprintf("pid %d", os.Getpid())} {
		if !strings.Contains(out, want) {
			t.Errorf("the footer does not name %q:\n%s", want, out)
		}
	}
}

// TestDriveDoesNotRefuseItsOwnFinish holds the one deadlock this design can
// produce: drive leases the whole queue and then calls finish's own RunE to
// reconcile what merged, which opens the same lease file inside the same
// process. flock lives on the open file description, so that second open is
// refused exactly as another terminal would be, and only the package state
// tells the two apart.
func TestDriveDoesNotRefuseItsOwnFinish(t *testing.T) {
	dir := leaseProject(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.takeLease("drive", "")
	if err != nil {
		t.Fatalf("drive could not take the tree: %v", err)
	}
	t.Cleanup(func() {
		release()
		held = nil
	})

	if err := s.leaseFree(); err != nil {
		t.Fatalf("finish refused the drive that is holding the lease for it: %v", err)
	}
	if _, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "run", Root: dir}); err == nil {
		t.Fatal("a second open in this process took the lease: flock is not per open file description here, " +
			"and the in-process half of this test proves nothing")
	}
}

// TestFinishRefusesAHolderThatHasNotNamedItself is the window Acquire opens
// between taking the tree and writing its account, seen from finish.
//
// drover can see the checkout is taken and only the name is missing, which is a
// rule refusing rather than a read failing. Exit 1 here would tell a wrapper the
// tool did not work, when what it is reporting is the state of the tree.
func TestFinishRefusesAHolderThatHasNotNamedItself(t *testing.T) {
	dir := leaseProject(t)
	path := leasePath(t, dir)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: dir, Branch: "main"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	err = s.leaseFree()
	if err == nil {
		t.Fatal("finish accepted a checkout somebody is holding")
	}
	if got := Code(err); got != ExitRefused {
		t.Errorf("finish exits %d on an unnamed holder, want %d: %v", got, ExitRefused, err)
	}
	if !strings.Contains(err.Error(), "has not written its account yet") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// The same holder in the footer. status reports and never refuses, and a line
// reading "pid 0" would name a process rather than the microsecond it is.
func TestStatusNamesAHolderThatHasNotWrittenItsAccount(t *testing.T) {
	dir := leaseProject(t)
	path := leasePath(t, dir)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: dir, Branch: "main"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() {
		if err := execute(t, "status"); err != nil {
			t.Fatalf("status refused a held checkout: %v", err)
		}
	})
	if !strings.Contains(out, leaseUnnamed) {
		t.Errorf("the footer does not name the unwritten holder:\n%s", out)
	}
	if strings.Contains(out, "pid 0") {
		t.Errorf("the footer invented pid 0 as a holder:\n%s", out)
	}
}

// holdDrive takes the checkout's lease as a drive would, without setting the
// package state that lets the drive's own finish through: what is asserted is
// how finish and open-pr treat somebody else's drive.
func holdDrive(t *testing.T, dir string) {
	t.Helper()
	h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "drive", Root: dir, Branch: "main"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })
}

// Items can merge through the forge's queue mid-drive, and finish must not
// refuse them for as long as the drive runs. The drive's items are in worktrees
// of their own, so nothing it is doing is in the tree finish switches.
func TestFinishPassesADriveHoldingTheCheckout(t *testing.T) {
	dir, branch := finishProject(t, noProject)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)
	holdDrive(t, dir)

	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish refused while a drive held the checkout: %v\n%s", err, out)
	}
	for _, want := range []string{branch + " is merged", "ledger: merged", "on main"} {
		if !strings.Contains(out, want) {
			t.Errorf("finish wrote:\n%s\nwant it to name %q", out, want)
		}
	}
}

func TestOpenPRPassesADriveHoldingTheCheckout(t *testing.T) {
	dir, _, ghLog := openPRReady(t)
	holdDrive(t, dir)

	if err := execute(t, "open-pr", "1"); err != nil {
		t.Fatalf("propose refused while a drive held the checkout: %v", err)
	}
	if n := prCreates(t, ghLog); len(n) != 1 {
		t.Errorf("opened %d pull requests, want 1: %v", len(n), n)
	}
}

// Two drives over one queue run the same items twice, so narrowing what the
// lease refuses for finish and open-pr must not narrow it for drive.
func TestASecondDriveStillRefuses(t *testing.T) {
	dir := leaseProject(t)
	holdDrive(t, dir)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.takeLease("drive", "")
	if err == nil {
		release()
		t.Fatal("a second drive took a checkout a drive is holding")
	}
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("exited %d, want %d naming the holder: %v", Code(err), ExitRefused, err)
	}
}

// finish removes the item's worktree, and a live session holding that tree's
// lease is working in it. The holder is a second process, because this one's
// own tree lease is the drive finishing its own item and is let through.
func TestFinishRefusesWhileASessionHoldsTheItemsWorktree(t *testing.T) {
	if path := os.Getenv(envTreeHold); path != "" {
		lp, err := lease.Path(path, "")
		if err != nil {
			childExit(err)
		}
		heldLease, err = lease.Acquire(lp, lease.Lease{Command: "drive", Item: "B1", Root: path})
		if err != nil {
			childExit(err)
		}
		runtime.GC()
		runtime.GC()
		fmt.Println("held")
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
	}

	dir, branch := finishProject(t, noProject)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.treePath(it)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "worktree", "add", "-q", tree, branch)

	child := relaunch(t, envTreeHold, tree)
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if line, err := bufio.NewReader(pipe).ReadString('\n'); err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("the holder never took the tree's lease: %q %v", line, err)
	}

	err = execute(t, "finish", "B1")
	if Code(err) != ExitRefused {
		t.Fatalf("finish exited %d with a live session in the item's worktree, want %d: %v", Code(err), ExitRefused, err)
	}
	for _, want := range []string{tree, fmt.Sprintf("pid %d", child.Process.Pid)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, err := os.Stat(tree); err != nil {
		t.Errorf("the worktree a session holds is gone: %v", err)
	}
	if openIn(t, dir).ledger.State("B1").Merged {
		t.Error("finish refused and recorded the completion anyway")
	}
}

// A drive and an operator's finish can record one merge from two processes, each
// having read the ledger before the other wrote. Both sessions are opened first
// to stand for that, then they record in each order, and the command's own path
// runs last against the result.
func TestFinishAndADriveRecordOneCompletionInEitherOrder(t *testing.T) {
	for _, driveFirst := range []bool{true, false} {
		name := "finish first"
		if driveFirst {
			name = "drive first"
		}
		t.Run(name, func(t *testing.T) {
			dir, _ := finishProject(t, noProject)
			stubPATH(t)
			t.Setenv(runner.EnvActor, "")
			drive, operator := openIn(t, dir), openIn(t, dir)
			yes := true
			first, second := operator, drive
			if driveFirst {
				first, second = drive, operator
			}
			for i, s := range []*session{first, second} {
				wrote, err := s.recordDone(ledger.Row{ID: "B1", Merged: &yes, MergedRunner: "impl", Date: ledger.Today()})
				if err != nil {
					t.Fatal(err)
				}
				if wrote != (i == 0) {
					t.Errorf("recorder %d wrote=%v, want only the first to write", i+1, wrote)
				}
			}
			if !second.ledger.State("B1").Merged {
				t.Error("the recorder that skipped still reads B1 as not done")
			}

			finishInDrive = !driveFirst
			t.Cleanup(func() { finishInDrive = false })
			var err error
			out := capture(t, func() { err = execute(t, "finish", "B1") })
			if err != nil {
				t.Fatalf("finish: %v\n%s", err, out)
			}
			if !strings.Contains(out, "already done") {
				t.Errorf("finish did not report the row it found:\n%s", out)
			}
			l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			done := 0
			for _, r := range l.Rows {
				if r.ID == "B1" && r.Merged != nil {
					done++
				}
			}
			if done != 1 {
				t.Errorf("the ledger holds %d completion rows for B1, want 1", done)
			}
		})
	}
}

// The trunk lock waits for its holder rather than refusing, and a holder that
// outlasts the wait costs the fast-forward, reported, and nothing else.
func TestTheTrunkLockWaitsThenReports(t *testing.T) {
	dir, _ := finishProject(t, noProject)
	s := openIn(t, dir)
	path := filepath.Join(dir, ".git", lease.TrunkFileName)
	restore := trunkWait
	t.Cleanup(func() { trunkWait = restore })

	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	trunkWait = 200 * time.Millisecond
	errOut := captureErr(t, func() { s.syncTrunk("main") })
	out := capture(t, func() {
		if err := s.backToTrunk("main"); err != nil {
			t.Errorf("a held trunk lock failed the return to the trunk: %v", err)
		}
	})
	for stream, text := range map[string]string{"syncTrunk": errOut, "backToTrunk": out} {
		if !strings.Contains(text, "held the trunk for longer than") || !strings.Contains(text, "drover drive") {
			t.Errorf("%s did not report the lock it waited on:\n%s", stream, text)
		}
	}

	trunkWait = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = h.Release()
	}()
	release, err := s.trunkLock("finish")
	if err != nil {
		t.Fatalf("the lock did not come free once its holder released it: %v", err)
	}
	release()
}

// A drive whose re-exec is waiting on an item that does not hold reads as
// pausing to re-exec, with the line naming that item under it, rather than as
// a bare pausing.
func TestStatusNamesWhatAStalledReexecWaitsOn(t *testing.T) {
	dir := leaseProject(t)
	const line = "the re-exec onto v9 has waited 2m0s for 2 (in review) to hold at a stage boundary"
	h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "drive", Root: dir, Paused: true,
		Running: []string{"1", "2"}, Holds: []lease.Hold{{ID: "1", At: "implement"}}, Reexec: "v9", ReexecWaiting: line})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	out := capture(t, func() {
		if err := execute(t, "status"); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	for _, want := range []string{"pausing to re-exec", "waiting:   " + line, "held:    1 before implement"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
}

// holdDriveRunning is holdDrive with the ids the drive has dispatched, the
// state itemNotDriving reads. The handle comes back so a test can move what
// the drive lists without letting the checkout go.
func holdDriveRunning(t *testing.T, dir string, running ...string) *lease.Handle {
	t.Helper()
	h, err := lease.Acquire(leasePath(t, dir), lease.Lease{Command: "drive", Drive: "D7", Root: dir,
		Branch: "main", Running: running})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })
	return h
}

func setRunning(t *testing.T, h *lease.Handle, running ...string) {
	t.Helper()
	if err := h.Update(func(l *lease.Lease) { l.Running = running }); err != nil {
		t.Fatal(err)
	}
}

// assertRefusedForDrive runs one command against an item the drive lists and
// holds it to the contract: exit 3, the drive and how to wait for it named, and
// not one ledger row written.
func assertRefusedForDrive(t *testing.T, dir string, args ...string) {
	t.Helper()
	before := readLedger(t, dir)
	var err error
	out := capture(t, func() { err = execute(t, args...) })
	if Code(err) != ExitRefused {
		t.Fatalf("%s exited %d for an item a live drive is running, want %d: %v\n%s",
			strings.Join(args, " "), Code(err), ExitRefused, err, out)
	}
	for _, want := range []string{"drive D7", fmt.Sprintf("pid %d", os.Getpid()),
		"drover watch D7 --until exit", "drover drive --stop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: the refusal does not name %q: %v", args[0], want, err)
		}
	}
	if after := readLedger(t, dir); after != before {
		t.Errorf("%s refused and wrote the ledger anyway:\n%s", args[0], strings.TrimPrefix(after, before))
	}
}

func TestMergeStateWritersRefuseAnItemTheDriveIsRunning(t *testing.T) {
	t.Run("mark-merged and unmark-merged", func(t *testing.T) {
		dir, _ := finishProject(t, noProject)
		t.Chdir(dir)
		yes := true
		if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
			ledger.Row{ID: "B1", Merged: &yes, Date: ledger.Today()}); err != nil {
			t.Fatal(err)
		}
		holdDriveRunning(t, dir, "B1")

		assertRefusedForDrive(t, dir, "unmark-merged", "B1")
		if !openIn(t, dir).ledger.State("B1").Merged {
			t.Error("unmark-merged refused and withdrew the completion anyway")
		}
		assertRefusedForDrive(t, dir, "mark-merged", "B1")

		for _, args := range [][]string{{"mark-merged", "B2"}, {"unmark-merged", "B2"}} {
			var err error
			out := capture(t, func() { err = execute(t, args...) })
			if err != nil {
				t.Fatalf("%s refused an item the drive is not running: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		if openIn(t, dir).ledger.State("B2").Merged {
			t.Error("B2 reads merged after mark-merged then unmark-merged")
		}
	})

	t.Run("finish", func(t *testing.T) {
		dir, branch := finishProject(t, noProject)
		stubPATH(t)
		t.Setenv(runner.EnvActor, "")
		t.Chdir(dir)
		h := holdDriveRunning(t, dir, "B1")

		assertRefusedForDrive(t, dir, "finish", "B1")
		// With no id finish resolves the item from the branch, and the check
		// has to see the resolved id rather than the empty argument.
		git(t, dir, "switch", "-q", branch)
		assertRefusedForDrive(t, dir, "finish")
		git(t, dir, "switch", "-q", "main")

		setRunning(t, h, "B2")
		var err error
		out := capture(t, func() { err = execute(t, "finish", "B1") })
		if err != nil {
			t.Fatalf("finish refused an item the drive is not running: %v\n%s", err, out)
		}
		if !openIn(t, dir).ledger.State("B1").Merged {
			t.Error("finish passed and recorded no completion")
		}
	})

	t.Run("open-pr", func(t *testing.T) {
		dir, _, ghLog := openPRReady(t)
		h := holdDriveRunning(t, dir, "1")

		assertRefusedForDrive(t, dir, "open-pr", "1")
		if n := prCreates(t, ghLog); len(n) != 0 {
			t.Errorf("open-pr refused and opened %d pull requests: %v", len(n), n)
		}

		setRunning(t, h, "2")
		if err := execute(t, "open-pr", "1"); err != nil {
			t.Fatalf("open-pr refused an item the drive is not running: %v", err)
		}
		if n := prCreates(t, ghLog); len(n) != 1 {
			t.Errorf("opened %d pull requests, want 1: %v", len(n), n)
		}
	})
}

const envDriveRunningHold = "DROVER_TEST_DRIVE_RUNNING_HOLD"

// A drive killed with an item in Running leaves that list on disk, and the
// kernel has already released the lock. The list is then a dead process's
// account, and refusing on it would strand the item until somebody edits a
// file drover tells them never to touch.
func TestADeadDrivesRunningListRefusesNothing(t *testing.T) {
	if dir := os.Getenv(envDriveRunningHold); dir != "" {
		path, err := lease.Path(dir, "")
		if err != nil {
			childExit(err)
		}
		heldLease, err = lease.Acquire(path, lease.Lease{Command: "drive", Drive: "D7", Root: dir, Running: []string{"B1"}})
		if err != nil {
			childExit(err)
		}
		runtime.GC()
		runtime.GC()
		fmt.Println("held")
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
	}

	dir, _ := finishProject(t, noProject)
	stubPATH(t)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)
	path := leasePath(t, dir)

	child := relaunch(t, envDriveRunningHold, dir)
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if line, err := bufio.NewReader(pipe).ReadString('\n'); err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("the holder never took the lease: %q %v", line, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), `"running":["B1"]`) { // #nosec G304 -- a test fixture
		t.Fatalf("the dead drive's lease does not still list B1, so this test proves nothing: %q %v", b, err)
	}

	var ferr error
	out := capture(t, func() { ferr = execute(t, "finish", "B1") })
	if ferr != nil {
		t.Fatalf("finish refused on a dead drive's running list: %v\n%s", ferr, out)
	}
	out = capture(t, func() { ferr = execute(t, "unmark-merged", "B1") })
	if ferr != nil {
		t.Fatalf("unmark-merged refused on a dead drive's running list: %v\n%s", ferr, out)
	}
}

// reconcile asks the forge whether an item merged and then records it, which
// is the window a second process's mark-merged or finish would land in. The
// gh wrapper snapshots the lease at every forge call, which mergedInto and the
// finish it leads to both make, so what is asserted is the lease as another
// process would have read it mid-reconcile.
func TestReconcileListsTheItemItIsFinishing(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.branchFor(it)
	if err != nil || branch == "" {
		t.Fatalf("branchFor: %q %v", branch, err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, "shipped.txt"), "work\n")
	git(t, dir, "add", "shipped.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	if err := s.recordHead("1", branch); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", branch)
	if err := s.record(ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "impl", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}

	release, err := s.takeLease("drive", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		held = nil
	})
	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	realGH, err := exec.LookPath("gh")
	if err != nil {
		t.Fatalf("the fixture puts no gh on PATH, so no forge call happens to observe: %v", err)
	}
	bin, snaps := t.TempDir(), filepath.Join(t.TempDir(), "snaps")
	stub(t, filepath.Join(bin, "gh"), fmt.Sprintf("#!/bin/sh\n/bin/cat %q >> %q\nexec %q \"$@\"\n",
		path, snaps, realGH))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	landed, _, err := s.reconcile()
	if err != nil || landed != 1 {
		t.Fatalf("reconcile finished %d (%v), want 1", landed, err)
	}
	b, err := os.ReadFile(snaps) // #nosec G304 -- a test fixture
	if err != nil {
		t.Fatalf("no forge call ran while reconciling, so nothing saw the lease: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(line, `"running":["1"]`) {
			t.Errorf("mid-reconcile the lease read %s, want it to list 1 as running", line)
		}
	}
	if l := held.Current(); len(l.Running) != 0 {
		t.Errorf("reconcile ended with %v still listed as running", l.Running)
	}
	if !s.ledger.State("1").Merged {
		t.Error("reconcile recorded no completion")
	}
}
