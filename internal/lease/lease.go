// Package lease keeps two drover commands out of one working tree.
//
// The hazard is not two commands wanting one branch. It is one command moving a
// checkout a second command is writing: `drover drive` holds a tree for hours
// across a branch switch, two agent sessions and a return to the trunk, and a
// `finish` typed in a second terminal switches that tree to the trunk underneath
// it. Both commands are correct and neither can see the other.
//
// So the lease is keyed on the checkout. `git rev-parse --absolute-git-dir`
// answers per work tree, which means a linked worktree reports its own
// `.git/worktrees/<name>` and two worktrees of one repository never block each
// other: they are two trees, and the hazard is one tree.
//
// Liveness is proven rather than inferred. A pid in a file is a guess, and a
// wrong one once the number is reused. Acquire takes an exclusive advisory lock
// and holds the descriptor for the process lifetime, so the kernel releases it
// however the process dies: a killed drive leaves nothing to clean up, no stale
// state, and so nothing for a --force to rescue.
package lease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravinald/drover/internal/gitcmd"
)

// FileName is the lease inside a git dir. A checkout holds one, whatever the
// branch, because the branch is not the axis being protected.
const FileName = "drover.lease"

// QueueSuffix names the lock beside a lease that serializes rewrites of its
// queue. The lease's own flock is the holder's for its whole life, so another
// process appending to the queue cannot take it; both sides take this one
// instead, for the microseconds a rewrite lasts.
const QueueSuffix = ".queue"

// TrunkFileName is the lock on the trunk ref, inside the repository's common git
// dir. The ref is one for every worktree of the repository, so the lock is too.
const TrunkFileName = "drover.trunk.lock"

// DirName holds the leases of projects that have no git dir, under the per-user
// state directory. One file per root rather than one shared file: two such
// projects are two trees and must not block each other.
const DirName = "leases"

// Lease is what a refusal has to print. A message reading only "busy" sends the
// operator to ps, which is where the fields past the pid come from.
type Lease struct {
	Command string `json:"command"`
	PID     int    `json:"pid"`
	// Item is the id the holder is working, absent when the holder took the
	// tree for a queue rather than for one item: `drive` leases once and walks
	// the queue inside it, so the id changes under a lease that does not.
	Item string `json:"item,omitempty"`
	// Branch is what was checked out when the lease was taken, absent in a
	// project with no branches. It is where the tree was, not where it is: the
	// holder is free to move it, and that is the whole reason for the lease.
	Branch  string `json:"branch,omitempty"`
	Root    string `json:"root"`
	Host    string `json:"host"`
	Started string `json:"started"`
	// Drive is a drive's id in its project's ledger, and Label the text its
	// operator gave it, both absent on any other holder.
	Drive string `json:"drive,omitempty"`
	Label string `json:"label,omitempty"`
	// Queue is the ids a drive holds waiting, in the order it will dispatch
	// them, and Running the ids it has dispatched and not yet heard back from.
	// Jobs is its --jobs. The holder rewrites all three as the queue moves, so
	// another process can say what is coming without asking the holder.
	Queue   []string `json:"queue,omitempty"`
	Running []string `json:"running,omitempty"`
	Jobs    int      `json:"jobs,omitempty"`
	// Closed is a drive that takes no more ids: it stopped, or its queue ran
	// out and it is on its way out. Append refuses to write into one, because
	// nothing would read what it wrote.
	Closed bool `json:"closed,omitempty"`
	// Paused is a drive dispatching nothing and holding each running item at
	// its next stage boundary until a resume. The holder writes it.
	Paused bool `json:"paused,omitempty"`
	// Control is a request another process wrote for the holder, "pause" or
	// "resume" followed by a token naming the request, and Answered the token
	// of the last one the holder acted on. Refused is why that one did not
	// take, empty when it did. A caller waits for its own token in Answered,
	// so it never reads an earlier request's answer as its own.
	Control  string `json:"control,omitempty"`
	Answered string `json:"answered,omitempty"`
	Refused  string `json:"refused,omitempty"`
	// Holds is each running item a paused drive holds at a stage boundary. A
	// running id with no hold is still inside a stage: a session, a gate or
	// the trunk stage. Reexec is the binary a resumed drive re-executes onto
	// once every running item holds, empty when it resumes where it is.
	Holds  []Hold `json:"holds,omitempty"`
	Reexec string `json:"reexec,omitempty"`
	// Handoff is what a drive re-executing itself leaves for the process that
	// takes its place, opaque here and cleared once that process has read it.
	Handoff json.RawMessage `json:"handoff,omitempty"`
}

// Hold is one item a paused drive holds: the stage it goes on to, and the
// verify command the stage it finished was told to pass.
type Hold struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	Verify string `json:"verify,omitempty"`
}

// Held is the error Acquire returns when another process has the tree, and the
// refusal finish prints without acquiring anything.
type Held struct {
	// Lease is the holder's payload. Zero when Payload is false.
	Lease Lease
	// Payload reports whether the holder's own account could be read. A lease
	// taken microseconds ago is locked before it is written, and a holder named
	// as "unknown" is a true answer where a fabricated pid is not.
	Payload bool
}

func (h Held) Error() string {
	if !h.Payload {
		return "another drover process is holding this checkout; it has not written its account yet.\n" +
			"Retry in a moment, or run drover status to see who is here"
	}
	l := h.Lease
	who := "drover " + l.Command
	if l.Item != "" {
		who += " " + l.Item
	}
	where := ""
	if l.Branch != "" {
		where = ", on " + l.Branch
	}
	if host, err := os.Hostname(); err == nil && l.Host != "" && l.Host != host {
		where += ", from " + l.Host
	}
	return fmt.Sprintf(
		"%s (pid %d) is holding this checkout since %s%s.\n"+
			"Two commands in one tree is how a branch switch lands under a live session.\n"+
			"Wait for it to finish, or stop it: kill %d",
		who, l.PID, l.Started, where, l.PID)
}

// Handle is a held lease. The descriptor stays open for as long as the lease
// does: flock lives on the open file description, not on the path, so closing
// the file is what releases the lease and nothing else has to.
type Handle struct {
	f     *os.File
	Lease Lease
	// mu guards Lease and the payload once Update can rewrite them while other
	// goroutines of the holder read Current.
	mu sync.Mutex
}

// Current is the holder's own account as last written.
func (h *Handle) Current() Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Lease
}

// Update rewrites the holder's account in place, under the queue lock. The new
// payload is written over the old one and the file cut to its length afterwards
// rather than emptied first: a reader landing between the two steps decodes the
// first JSON value and ignores the tail, where an emptied file would read as a
// holder that has not named itself.
//
// Ids another process appended since the holder last took them stay on the end
// of the queue fn writes, so a rewrite never drops an append it raced. They are
// the holder's once Take hands them over.
func (h *Handle) Update(fn func(*Lease)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return errors.New("lease already released")
	}
	unlock, err := queueLock(h.f.Name())
	if err != nil {
		return err
	}
	defer unlock()
	extra := h.appended()
	next := h.Lease
	fn(&next)
	onDisk := next
	if !next.Closed {
		onDisk.Queue = append(slices.Clone(next.Queue), without(extra, next.Queue)...)
	}
	onDisk.Control = h.pending(next.Answered)
	if err := rewrite(h.f, onDisk); err != nil {
		return err
	}
	h.Lease = next
	return nil
}

// Take hands the holder the ids appended to its queue since it last took them,
// in the order they were appended. closeIfNone closes the queue in the same
// step when there are none, so an append either lands before the close and is
// returned here, or finds the queue closed: there is no moment at which an
// append succeeds and nobody reads it.
func (h *Handle) Take(closeIfNone bool) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil, errors.New("lease already released")
	}
	unlock, err := queueLock(h.f.Name())
	if err != nil {
		return nil, err
	}
	defer unlock()
	extra := h.appended()
	switch {
	case len(extra) > 0:
		h.Lease.Queue = append(h.Lease.Queue, extra...)
	case closeIfNone && !h.Lease.Closed:
		next := h.Lease
		next.Closed = true
		next.Control = h.pending(next.Answered)
		if err := rewrite(h.f, next); err != nil {
			return nil, err
		}
		h.Lease = next
	}
	return extra, nil
}

// Close closes the queue whatever is waiting in it and returns what was
// appended and not yet taken, which nothing will now dispatch. It is the stop:
// a drive that dispatches nothing more takes nothing more either.
func (h *Handle) Close() ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil, errors.New("lease already released")
	}
	unlock, err := queueLock(h.f.Name())
	if err != nil {
		return nil, err
	}
	defer unlock()
	dropped := h.appended()
	if h.Lease.Closed && len(dropped) == 0 {
		return nil, nil
	}
	next := h.Lease
	next.Closed = true
	next.Control = h.pending(next.Answered)
	if err := rewrite(h.f, next); err != nil {
		return nil, err
	}
	h.Lease = next
	return dropped, nil
}

// Requested is the request another process wrote for the holder and the holder
// has not answered, empty when there is none. The holder answers it by setting
// Answered to it in an Update, which clears it from the file in the same write.
func (h *Handle) Requested() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return "", errors.New("lease already released")
	}
	unlock, err := queueLock(h.f.Name())
	if err != nil {
		return "", err
	}
	defer unlock()
	return h.pending(h.Lease.Answered), nil
}

// pending is the file's request less one already answered. Every rewrite the
// holder makes carries it, so a request written between two of them survives
// the second. Called with the queue lock held.
func (h *Handle) pending(answered string) string {
	disk, ok := decode(h.f)
	if !ok || disk.Control == answered {
		return ""
	}
	return disk.Control
}

// Signal writes a request into the account of the holder at path, under the
// queue lock, after check has seen that account and whether anyone holds it.
// check returns an error to write nothing. A request already waiting is
// replaced: the holder acts on the latest one it reads.
func Signal(path, request string, check func(l Lease, busy bool) error) error {
	unlock, err := queueLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	l, busy, err := Read(path)
	if err != nil {
		return err
	}
	if busy && l.PID == 0 {
		return Held{}
	}
	if err := check(l, busy); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the path is derived from the git dir or the per-user state directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	l.Control = request
	return rewrite(f, l)
}

// appended is what the file's queue holds that the holder's does not: the ids
// Append wrote since the holder last took them. Called with the queue lock
// held, so the file cannot move under the read.
func (h *Handle) appended() []string {
	disk, ok := decode(h.f)
	if !ok {
		return nil
	}
	return without(disk.Queue, h.Lease.Queue)
}

// Append writes ids into the queue of the drive holding the lease at path,
// under the queue lock that drive rewrites it under. pick sees the holder's
// account as it stands and whether anyone holds the lease at all, and returns
// the ids to append, or an error to write nothing. It is not called when the
// holder has not named itself yet, which Append reports as Held with no
// payload.
//
// The returned lease is the account as written, or as read when nothing was.
func Append(path string, pick func(l Lease, busy bool) ([]string, error)) (Lease, error) {
	unlock, err := queueLock(path)
	if err != nil {
		return Lease{}, err
	}
	defer unlock()
	l, busy, err := Read(path)
	if err != nil {
		return Lease{}, err
	}
	if busy && l.PID == 0 {
		return Lease{}, Held{}
	}
	ids, err := pick(l, busy)
	if err != nil || len(ids) == 0 || !busy || l.Closed {
		return l, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the path is derived from the git dir or the per-user state directory
	if err != nil {
		return Lease{}, err
	}
	defer func() { _ = f.Close() }()
	l.Queue = append(l.Queue, ids...)
	return l, rewrite(f, l)
}

// queueWait bounds how long a queue rewrite waits for another. The holder
// writes a line and syncs, so a wait past this is a process stuck on the disk.
const queueWait = 10 * time.Second

// queueLock takes the lock beside the lease at path, waiting for a holder that
// has it for one rewrite.
func queueLock(path string) (func(), error) {
	f, err := os.OpenFile(path+QueueSuffix, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- beside the lease, whose path is derived from the git dir or the state directory
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(queueWait)
	for lockExclusive(f) != nil {
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another drover process held the queue lock %s for longer than %s", f.Name(), queueWait)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() {
		_ = unlock(f)
		_ = f.Close()
	}, nil
}

// without is a in order, less every id in b.
func without(a, b []string) []string {
	var out []string
	for _, id := range a {
		if !slices.Contains(b, id) {
			out = append(out, id)
		}
	}
	return out
}

// rewrite writes l over the payload in f and cuts the file to its length.
func rewrite(f *os.File, l Lease) error {
	line, err := json.Marshal(l)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := f.WriteAt(line, 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(line))); err != nil {
		return err
	}
	return f.Sync()
}

// Path resolves where a checkout's lease lives.
//
// Never under the backlog, the ledger or any path the project tracks: a lease
// is one machine's state for one tree, and a committed one hands every clone a
// holder that does not exist. The git dir is outside the work tree by
// construction, which is why it is the first answer.
//
// stateDir is the directory holding projects.jsonl, and is used when git names
// no dir at all: a project with [git] enabled = false, and any other tree git
// does not claim. Those are keyed by a hash of the absolute root, because the
// root is the only name such a project has. git is asked first either way,
// because a tree it claims has one checkout whatever the project thinks of
// branches.
func Path(root, stateDir string) (string, error) {
	if dir, err := gitDir(root); err == nil {
		return filepath.Join(dir, FileName), nil
	}
	if stateDir == "" {
		return "", errors.New("no git dir and no per-user state directory: nowhere to keep a lease")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(stateDir, DirName, hex.EncodeToString(sum[:])[:16]+".lease"), nil
}

// gitDir asks git where this work tree's own git dir is. --absolute-git-dir
// rather than --git-dir: the relative form resolves against the caller's
// working directory, and a lease path that moves with the shell is not a lease.
func gitDir(root string) (string, error) {
	out, err := gitcmd.Command(root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("git named no git dir")
	}
	return dir, nil
}

// Acquire takes the tree, or reports who has it.
//
// The lock is non-blocking on purpose: a caller that waited would hold a
// terminal on a condition nothing bounds, since the holder is an agent session
// measured in hours. Refusing with the holder's pid gives the operator the two
// answers there are (wait, or stop it) and costs nothing if they pick the
// first.
func Acquire(path string, l Lease) (*Handle, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- the path is derived from the git dir or the per-user state directory
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(f); err != nil {
		held := Held{}
		if prev, ok := decode(f); ok {
			held.Lease, held.Payload = prev, true
		}
		_ = f.Close()
		return nil, held
	}

	// Emptied under the lock and before this holder names itself. Between
	// taking the tree and writing the payload the file still holds whatever was
	// there, and a Read landing in that window would decode a process the
	// kernel has already released and report it as the holder: a pid to kill
	// that belongs to something else by now, which is the reuse this design
	// claims the lock makes impossible. Release empties it too; this covers the
	// holder that was killed and ran nothing.
	if err := f.Truncate(0); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, err
	}

	l.PID = os.Getpid()
	l.Started = time.Now().Format(time.RFC3339)
	if l.Host == "" {
		l.Host, _ = os.Hostname()
	}
	if err := write(f, l); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, err
	}
	return &Handle{f: f, Lease: l}, nil
}

// Inherit keeps the lease open across an exec of the holder, and returns the
// descriptor the next program finds it on. The lock stays on the open file
// description the whole time, so no other process can take the tree between
// the exec and the new program adopting it.
func (h *Handle) Inherit() (uintptr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return 0, errors.New("lease already released")
	}
	fd := h.f.Fd()
	return fd, inheritable(fd)
}

// Adopt is the lease a process inherited on fd from the program it replaced
// with an exec. It is refused unless the lock is this process's and the
// account names this pid, so a stray descriptor cannot pass for a lease.
func Adopt(fd uintptr, path string) (*Handle, error) {
	f := os.NewFile(fd, path)
	if f == nil {
		return nil, fmt.Errorf("descriptor %d is not open", fd)
	}
	if err := lockExclusive(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("descriptor %d does not hold the lease at %s: %w", fd, path, err)
	}
	l, ok := decode(f)
	if !ok || l.PID != os.Getpid() {
		_ = f.Close()
		return nil, fmt.Errorf("the lease on descriptor %d names pid %d, not this process", fd, l.PID)
	}
	// Closed on exec again, or every session the holder launches inherits the
	// lock and a child outliving the holder keeps the tree taken.
	closeOnExec(fd)
	return &Handle{f: f, Lease: l}, nil
}

// AcquireWithin is Acquire for a lock held for seconds rather than hours: it
// retries until wait has passed, then returns the last refusal, which names the
// holder that outlasted it.
func AcquireWithin(path string, l Lease, wait time.Duration) (*Handle, error) {
	deadline := time.Now().Add(wait)
	for {
		h, err := Acquire(path, l)
		if _, busy := errors.AsType[Held](err); !busy || !time.Now().Before(deadline) {
			return h, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Release gives the tree back. It leaves the file in place: another process may
// already have it open and be about to lock it, and unlinking underneath that
// hands it a lock on an inode with no name, which protects nothing. The account
// inside it goes, because a payload naming a process that no longer holds the
// tree is the one thing a reader must never be handed.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil
	}
	// Emptied before the unlock, so the payload never outlives the lock by a
	// path anybody can observe. The file stays; only the account goes.
	if terr := h.f.Truncate(0); terr != nil {
		_ = unlock(h.f)
		_ = h.f.Close()
		h.f = nil
		return terr
	}
	err := unlock(h.f)
	if cerr := h.f.Close(); err == nil {
		err = cerr
	}
	h.f = nil
	return err
}

// Read reports the holder and takes nothing that outlives the call.
//
// Liveness is the shared lock: it is refused only while somebody holds the
// exclusive one, so a lease file whose holder died reads as free and its
// payload is never printed. That is the whole reason the payload is not
// trusted on its own: the pid in a file outlives the process, and the lock
// does not.
//
// The probe does briefly hold the shared lock, so a read landing inside another
// process's Acquire can push that acquire to a refusal naming no payload.
// Microseconds wide, one retry to clear, and the alternative is trusting a
// number.
func Read(path string) (Lease, bool, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is derived from the git dir or the per-user state directory
	if err != nil {
		if os.IsNotExist(err) {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	defer func() { _ = f.Close() }()

	if err := lockShared(f); err == nil {
		_ = unlock(f)
		return Lease{}, false, nil
	}
	l, ok := decode(f)
	if !ok {
		// Held, and the holder has not written its account yet: Acquire empties
		// the file under the lock before it names itself. Busy with no payload
		// is a true answer about the tree, and an error here would report a
		// checkout drover can see is taken as one drover could not read, which
		// costs finish its exit 3.
		return Lease{}, true, nil
	}
	return l, true, nil
}

func write(f *os.File, l Lease) error {
	line, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(append(line, '\n'), 0); err != nil {
		return err
	}
	return f.Sync()
}

// decode reads the first JSON value in the file. Only the first: Update writes
// over the old payload before cutting the file to length, so a read between the
// two finds the new account followed by the tail of the old one.
func decode(f *os.File) (Lease, bool) {
	var l Lease
	if json.NewDecoder(io.NewSectionReader(f, 0, 1<<20)).Decode(&l) != nil || l.PID == 0 {
		return Lease{}, false
	}
	return l, true
}
