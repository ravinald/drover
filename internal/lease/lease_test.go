package lease_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/lease"
)

// tree is a root and the state directory its lease falls back to. No git here
// on purpose: the git-dir path is what a checkout uses and internal/cmd drives
// it against a real repository, while this package's own contract is the lock
// and the payload, which the fallback exercises with nothing else in the way.
func tree(t *testing.T) (root, path string) {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	path, err := lease.Path(root, state)
	if err != nil {
		t.Fatalf("lease.Path: %v", err)
	}
	return root, path
}

// A lease that outlived its process would be reported as a holder that is not
// there, which is the failure a pid file has. Release is the ordinary path.
func TestReleaseMakesTheLeaseFreeAgain(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "run", Item: "1", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, busy, _ := lease.Read(path); !busy {
		t.Fatal("a held lease reads free")
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	l, busy, err := lease.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if busy {
		t.Errorf("a released lease still names %s (pid %d)", l.Command, l.PID)
	}
}

// The lock lives on the open file description, so a second open in this process
// is refused exactly as another terminal would be. Everything else here rests on
// that, and it is the one fact a single-process test can establish.
func TestASecondOpenInThisProcessIsRefusedToo(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })

	_, err = lease.Acquire(path, lease.Lease{Command: "finish", Root: root})
	held, ok := errors.AsType[lease.Held](err)
	if !ok {
		t.Fatalf("a second open took the lease: %v", err)
	}
	if !held.Payload {
		t.Fatal("the refusal carries no payload, so it can name no holder")
	}
	if held.Lease.PID != os.Getpid() || held.Lease.Command != "drive" {
		t.Errorf("the refusal names %s pid %d, the holder is drive pid %d",
			held.Lease.Command, held.Lease.PID, os.Getpid())
	}
	for _, want := range []string{"drover drive", "kill", "Wait for it"} {
		if !strings.Contains(held.Error(), want) {
			t.Errorf("the message does not say %q:\n%s", want, held.Error())
		}
	}
}

// A payload the holder cannot be reached through is a message that sends the
// operator to ps, which is the whole reason there are seven fields and not one.
func TestThePayloadNamesWhatARefusalHasToPrint(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "run", Item: "B1", Branch: "b1-first", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })

	l, busy, err := lease.Read(path)
	if err != nil || !busy {
		t.Fatalf("read: %v busy=%v", err, busy)
	}
	if _, err := time.Parse(time.RFC3339, l.Started); err != nil {
		t.Errorf("started %q: %v", l.Started, err)
	}
	if l.PID != os.Getpid() || l.Command != "run" || l.Item != "B1" || l.Branch != "b1-first" {
		t.Errorf("payload lost a field: %+v", l)
	}
	if l.Root != root || l.Host == "" {
		t.Errorf("payload names root %q host %q, want root %q and a host", l.Root, l.Host, root)
	}
}

// The payload of a holder that is gone belongs to nothing, so it is overwritten
// rather than reported. Rewritten in place because Release leaves the file: the
// alternative is unlinking under a process that already has it open.
func TestAStalePayloadIsOverwrittenRatherThanReported(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if _, busy, _ := lease.Read(path); busy {
		t.Fatal("the stale payload reads as a holder")
	}
	next, err := lease.Acquire(path, lease.Lease{Command: "finish", Root: root})
	if err != nil {
		t.Fatalf("a lease nobody holds was not acquirable: %v", err)
	}
	t.Cleanup(func() { _ = next.Release() })
	l, _, err := lease.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Command != "finish" {
		t.Errorf("the file still names %s", l.Command)
	}
}

// Nobody has taken the tree, which is not the same answer as an error, and a
// command that treated a missing file as one would refuse every first run.
func TestAnAbsentLeaseIsFreeRatherThanAFailure(t *testing.T) {
	_, path := tree(t)
	l, busy, err := lease.Read(path)
	if err != nil || busy {
		t.Fatalf("read of an absent lease: %+v busy=%v err=%v", l, busy, err)
	}
}

// Two worktrees of one repository are two checkouts, and the hazard is one
// checkout. --absolute-git-dir is what draws the line, so this asserts the
// distinction drover depends on rather than the code that reads it.
func TestTwoWorktreesOfOneRepositoryGetTwoLeases(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	base := t.TempDir()
	main := filepath.Join(base, "main")
	if err := os.Mkdir(main, 0o750); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x.invalid"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil { // #nosec G204 -- test fixture
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	run(main, "commit", "-q", "--allow-empty", "-m", "root")
	linked := filepath.Join(base, "linked")
	run(main, "worktree", "add", "-q", linked, "-b", "second")

	a, err := lease.Path(main, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := lease.Path(linked, "")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("both worktrees lease %s, so one blocks the other", a)
	}
	h, err := lease.Acquire(a, lease.Lease{Command: "drive", Root: main})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	if _, busy, err := lease.Read(b); err != nil || busy {
		t.Errorf("holding %s made %s read held: busy=%v err=%v", main, linked, busy, err)
	}
}

// The fallback keyed on the root, for a tree git names no dir for. It must be
// outside the tree, since a lease committed to a backlog hands every clone a
// holder that does not exist, and it must differ per root, or two such projects
// block each other for no reason.
func TestALeaseWithNoGitDirLivesUnderTheStateDirectory(t *testing.T) {
	state := t.TempDir()
	one, two := t.TempDir(), t.TempDir()
	a, err := lease.Path(one, state)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lease.Path(two, state)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two roots share one lease at %s", a)
	}
	for root, path := range map[string]string{one: a, two: b} {
		if !strings.HasPrefix(path, filepath.Join(state, lease.DirName)+string(os.PathSeparator)) {
			t.Errorf("%s: lease at %s, not under the state directory %s", root, path, state)
		}
		if strings.HasPrefix(path, root+string(os.PathSeparator)) {
			t.Errorf("%s: lease at %s, inside the tree", root, path)
		}
	}
	if _, err := lease.Path(one, ""); err == nil {
		t.Error("no git dir and no state directory produced a path anyway")
	}
}

// A holder between taking the tree and naming itself is busy with no payload,
// not a read that failed. The distinction is the whole difference between
// finish's exit 3 and its exit 1: drover can see the checkout is taken, and only
// the account is missing.
func TestAHeldTreeWithNoAccountIsBusyRatherThanUnreadable(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	// The on-disk state of the window Acquire opens: locked, not yet named.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	l, busy, err := lease.Read(path)
	if err != nil {
		t.Fatalf("a held tree read as a failure: %v", err)
	}
	if !busy {
		t.Fatal("a held tree read as free because its account was unreadable")
	}
	if l.PID != 0 {
		t.Errorf("Read invented a holder from an unreadable account: %+v", l)
	}
}

// Release takes the account with it. A payload naming a process that no longer
// holds the tree is the pid-reuse failure the lock exists to make impossible,
// and it arrives by the back door if the file keeps its last holder's name.
func TestReleaseTakesTheAccountWithIt(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Item: "F11", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("the released holder's account survives it: %q", b)
	}
}

// A killed holder's account is the one Release cannot clear, and the next
// Acquire must not let a refusal print its pid. This pins the end state rather
// than the window: the window between Acquire's lock and its write is not
// observable from outside the package, so what is asserted here is that nothing
// of the dead holder reaches a reader once the new one has the tree.
func TestAKilledHoldersPidIsNeverWhatARefusalPrints(t *testing.T) {
	root, path := tree(t)
	dead := lease.Lease{Command: "drive", Item: "F11", PID: 999999, Root: root,
		Host: "boxcar", Started: "2026-09-17T09:12:03Z"}
	line, err := json.Marshal(dead)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := lease.Acquire(path, lease.Lease{Command: "finish", Root: root})
	if err != nil {
		t.Fatalf("a lease nobody holds was not acquirable: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	l, busy, err := lease.Read(path)
	if err != nil || !busy {
		t.Fatalf("read of a held tree: busy=%v err=%v", busy, err)
	}
	if l.PID == dead.PID {
		t.Errorf("the killed holder's pid is still what a refusal would print: %+v", l)
	}
}

// A drive rewrites its account as the queue moves, and a reader in another
// process sees the new queue, never a holder with no account.
func TestUpdateRewritesTheAccountInPlace(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if err := h.Update(func(l *lease.Lease) { l.Queue, l.Running, l.Jobs = []string{"2", "3", "B10"}, []string{"1"}, 2 }); err != nil {
		t.Fatal(err)
	}
	got, busy, err := lease.Read(path)
	if err != nil || !busy {
		t.Fatalf("Read: busy=%v err=%v", busy, err)
	}
	if strings.Join(got.Queue, " ") != "2 3 B10" || strings.Join(got.Running, " ") != "1" || got.Jobs != 2 {
		t.Errorf("read queue %v running %v jobs %d, want [2 3 B10] [1] 2", got.Queue, got.Running, got.Jobs)
	}

	// Shorter than the payload before it: the file is cut to length, so what
	// is read is the new account and nothing of the old one.
	if err := h.Update(func(l *lease.Lease) { l.Queue, l.Running = nil, []string{"2"} }); err != nil {
		t.Fatal(err)
	}
	got, _, _ = lease.Read(path)
	if len(got.Queue) != 0 || strings.Join(got.Running, " ") != "2" {
		t.Errorf("after the queue emptied, read queue %v running %v", got.Queue, got.Running)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "{") != 1 {
		t.Errorf("the file holds more than one account: %s", raw)
	}
	if h.Current().PID != got.PID || len(h.Current().Running) != 1 {
		t.Errorf("the holder's own view %+v disagrees with the file %+v", h.Current(), got)
	}
}

// An append and the holder's dispatch both rewrite the queue, from two
// processes in the field and two goroutines here. Every id appended while the
// holder takes, dispatches and republishes must reach the holder exactly once.
func TestAnAppendRacingTheHoldersDispatchIsNeverLost(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })

	const n = 60
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			id := fmt.Sprintf("A%d", i)
			if _, err := lease.Append(path, func(l lease.Lease, busy bool) ([]string, error) {
				if !busy || l.Closed {
					return nil, fmt.Errorf("append %s found busy=%v closed=%v", id, busy, l.Closed)
				}
				return []string{id}, nil
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	got := map[string]int{}
	var pending []string
	dispatch := func(closing bool) bool {
		ids, err := h.Take(closing)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			got[id]++
		}
		pending = append(pending, ids...)
		var running []string
		if len(pending) > 0 {
			running, pending = pending[:1], pending[1:]
		}
		if err := h.Update(func(l *lease.Lease) { l.Queue, l.Running = slices.Clone(pending), running }); err != nil {
			t.Fatal(err)
		}
		return len(ids) > 0
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			dispatch(false)
		}
	}
	for dispatch(true) {
	}
	for i := range n {
		if id := fmt.Sprintf("A%d", i); got[id] != 1 {
			t.Errorf("%s reached the holder %d times, want once", id, got[id])
		}
	}
	if !h.Current().Closed {
		t.Error("taking with nothing appended did not close the queue")
	}
	if _, err := lease.Append(path, func(l lease.Lease, _ bool) ([]string, error) {
		if !l.Closed {
			t.Error("an append after the close reads the queue open")
		}
		return []string{"late"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := h.Take(false); len(ids) != 0 {
		t.Errorf("an append into a closed queue was written: %v", ids)
	}
}

// Close is the stop: ids appended and not yet taken are handed back as
// dropped, and nothing appends after it.
func TestCloseHandsBackWhatNothingWillDispatch(t *testing.T) {
	root, path := tree(t)
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	if _, err := lease.Append(path, func(lease.Lease, bool) ([]string, error) { return []string{"7"}, nil }); err != nil {
		t.Fatal(err)
	}
	dropped, err := h.Close()
	if err != nil || strings.Join(dropped, " ") != "7" {
		t.Fatalf("Close returned %v, %v; want [7]", dropped, err)
	}
	l, _, _ := lease.Read(path)
	if !l.Closed || len(l.Queue) != 0 {
		t.Errorf("after Close the account reads closed=%v queue=%v", l.Closed, l.Queue)
	}
}
