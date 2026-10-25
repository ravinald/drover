package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/lease"
)

// driveSched is what lets a drive run items side by side: an ordered trunk
// stage, and a signal that tells every running item to stop at its next stage
// boundary.
//
// Implement and review run in each item's own worktree and touch nothing the
// others read, so they overlap freely. Catch-up, the gate, the push after review,
// the pull request and the merge do not: a catch-up merges whatever origin's
// trunk gained and the gate reads that tree, so a second item merging in between
// leaves the first one's gate result about a trunk that no longer exists. Those
// steps run one item at a time, in the order the items were dispatched.
type driveSched struct {
	// trunk is held for the whole of one item's trunk stage, and by syncTrunk
	// while a new worktree brings the local trunk up to date. Both move a ref
	// every later comparison in the stage reads.
	trunk sync.Mutex

	mu   sync.Mutex
	cond *sync.Cond
	// next is the dispatch number whose turn it is; gone holds the numbers past
	// it that finished without needing the stage, so the turn skips them.
	next int
	gone map[int]bool
	// stop is why the drive is draining, nil while it is not.
	stop error
	// paused holds every item at its next stage boundary until a resume, and
	// inputs is what the items take once released: the drive's launch inputs,
	// replaced by each resume.
	paused bool
	inputs *driveInputs
	// holds is each item waiting at a boundary of the paused drive, and
	// reexec the binary a resume re-executes the drive onto once every
	// running item is among them.
	holds  map[string]lease.Hold
	reexec string
	// conflicted is every item this drive already handed a trunk conflict to a
	// session for. A second conflict on one of them parks for a person: the
	// trunk is moving faster than the item's rounds can follow it.
	conflicted map[string]bool

	// Shared across every item's session, and touched only inside the trunk
	// stage: each item caught up to the same trunk commit asks the same question
	// of it, and the trunk's branch protection does not change between merges.
	trunkResults map[string]trunkResult
	forgeWait    *forgeWait
	// usage is usage_admission's hold, read by the dispatcher and by every
	// item at each stage that launches a session.
	usage *usageGate
}

func newDriveSched() *driveSched {
	q := &driveSched{gone: map[int]bool{}, trunkResults: map[string]trunkResult{}, holds: map[string]lease.Hold{},
		conflicted: map[string]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// driveSlot is one dispatched item's place in the schedule: its dispatch
// number, which orders the trunk stage, and the schedule it belongs to.
type driveSlot struct {
	sched *driveSched
	seq   int
}

// parkDrained is the item a stopping drive let finish its stage and then held.
const parkDrained = "stopped"

// enter waits for seq's turn at the trunk stage and holds the stage. It returns
// the drain cause instead when the drive is stopping, holding nothing.
func (q *driveSched) enter(seq int) error {
	q.mu.Lock()
	for q.next != seq && q.stop == nil {
		q.cond.Wait()
	}
	stop := q.stop
	q.mu.Unlock()
	if stop != nil {
		return stop
	}
	q.trunk.Lock()
	return nil
}

// leave releases the stage seq holds and passes the turn on.
func (q *driveSched) leave(seq int) {
	q.trunk.Unlock()
	q.retire(seq)
}

// retire marks seq as needing the stage no longer. Every dispatched item
// retires once it ends, whether or not it entered, or the items behind it
// would wait for a turn nobody takes.
func (q *driveSched) retire(seq int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if seq < q.next {
		return
	}
	q.gone[seq] = true
	for q.gone[q.next] {
		delete(q.gone, q.next)
		q.next++
	}
	q.cond.Broadcast()
}

// drain stops the drive at every running item's next stage boundary. The first
// cause wins: it is the one the drive reports on its way out.
func (q *driveSched) drain(cause error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stop == nil {
		q.stop = cause
	}
	q.cond.Broadcast()
}

// spendConflict takes id's one conflict round for this drive, and reports
// false when an earlier round already took it.
func (q *driveSched) spendConflict(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.conflicted[id] {
		return false
	}
	q.conflicted[id] = true
	return true
}

func (q *driveSched) draining() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stop
}

// watchInterrupt drains the schedule when ctx ends, so an item whose session
// outlived the interrupt goes no further than the stage it was in.
func (q *driveSched) watchInterrupt(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.drain(fmt.Errorf("interrupted"))
		case <-done:
		}
	}()
	return func() { close(done) }
}

// fork is the session one dispatched item runs on. Items running side by side
// each change the worktree in hand and reload the backlog and the ledger after
// every launch, so each gets its own copy of both; the config is shared, and
// nothing in a drive writes it.
func (s *session) fork(slot *driveSlot) (*session, error) {
	w := *s
	w.tree, w.slot = "", slot
	if err := w.reload(); err != nil {
		return nil, err
	}
	return &w, nil
}

// drained is the park an item takes at a stage boundary once the drive is
// stopping, and empty while it is not.
func (s *session) drained(next string) string {
	if s.slot == nil {
		return ""
	}
	if cause := s.slot.sched.draining(); cause != nil {
		return fmt.Sprintf("the drive stopped while this item ran (%v), so it stopped before %s", cause, next)
	}
	return ""
}

// inTrunkStage runs fn as this item's turn at the trunk stage, or parks the
// item when the drive stops before its turn comes. A stop fn raises drains the
// schedule before the turn passes on, so the next item in line never catches up
// to a trunk this one just found red.
func (s *session) inTrunkStage(fn func() DriveItem, park func(cause, why string) DriveItem) DriveItem {
	if s.slot == nil {
		return fn()
	}
	q, seq := s.slot.sched, s.slot.seq
	if cause := q.enter(seq); cause != nil {
		return park(parkDrained, fmt.Sprintf(
			"the drive stopped while this item waited for the trunk (%v); its review passed, and nothing was updated, verified, opened as a pull request or merged", cause))
	}
	s.trunkResults, s.forgeWait = q.trunkResults, q.forgeWait
	out := fn()
	q.forgeWait = s.forgeWait
	if out.stop != nil {
		q.drain(out.stop)
	}
	q.leave(seq)
	return out
}

// holdTrunk keeps the trunk stage out while a worktree brings the local trunk up
// to date, and is a no-op outside a schedule.
func (s *session) holdTrunk() func() {
	if s.slot == nil {
		return func() {}
	}
	s.slot.sched.trunk.Lock()
	return s.slot.sched.trunk.Unlock
}

// citedPath matches what an item body names as a file or directory: a
// slash-separated path whose last part carries an extension, or any
// slash-separated path set in backticks. Prose like "and/or" carries neither.
// A line reference after a colon is not part of the path; the second group
// keeps it for the stale_citations check.
var (
	citedFile = regexp.MustCompile(`(?:^|[^\w./:-])((?:[\w.-]+/)+[\w-]+\.[\w]+)(?::(\d+(?:-\d+)?))?`)
	citedTick = regexp.MustCompile("`((?:[\\w.-]+/)+[\\w.-]*)(?::([\\d,-]+))?`")
)

// citedPaths is every path an item's body cites, cleaned of a leading ./ and a
// trailing slash. A URL is not a path in this tree, so a host-qualified one is
// left out.
func citedPaths(body string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		if p == "" || strings.Contains(p, "..") {
			return
		}
		for _, have := range out {
			if have == p {
				return
			}
		}
		out = append(out, p)
	}
	for _, m := range citedFile.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range citedTick.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	return out
}

// overlap is the first path two citation lists share, counting a directory as
// overlapping every path below it, and empty when they share none.
func overlap(a, b []string) string {
	for _, x := range a {
		for _, y := range b {
			switch {
			case x == y, strings.HasPrefix(y, x+"/"):
				return x
			case strings.HasPrefix(x, y+"/"):
				return y
			}
		}
	}
	return ""
}

// queued is an item waiting to be dispatched, with the paths its body cites.
type queued struct {
	it    backlog.Item
	named bool
	paths []string
	// hold is where a re-exec found the item held, nil otherwise.
	hold *lease.Hold
}

func newQueued(it backlog.Item, named bool) queued {
	return queued{it: it, named: named, paths: citedPaths(it.Body)}
}

// heldApart is a later item waiting on an earlier one, and why: both cite one
// path, or the later depends on the earlier.
type heldApart struct {
	later, earlier, why string
}

// dispatchable is the index of the first pending item that waits on no running
// item and no earlier pending one, and -1 when every one of them waits. An item
// waits on another it depends on or that cites a path it cites. The earlier
// pending items count too, so an item held behind a running one is never
// overtaken by a later item that overlaps it or depends on it.
//
// A dep in neither list is left alone: it already finished, or it is not in
// this drive, and driveItem reads its state when the item starts, as it does at
// --jobs 1. A dep pending behind the item is left alone for the same reason, so
// an item named before its own dep skips as blocked whatever --jobs is.
func dispatchable(pending []queued, running map[int]queued) (int, []heldApart) { //nolint:unparam // the holds name why each skipped item waits, kept for a caller that reports them
	slots := make([]int, 0, len(running))
	for n := range running {
		slots = append(slots, n)
	}
	slices.Sort(slots)
	var held []heldApart
	for i, q := range pending {
		ahead := make([]queued, 0, len(slots)+i)
		for _, n := range slots {
			ahead = append(ahead, running[n])
		}
		ahead = append(ahead, pending[:i]...)
		if h, waits := waitsOn(q, ahead); waits {
			held = append(held, h)
			continue
		}
		return i, held
	}
	return -1, held
}

// waitsOn is the first item in ahead that q must not run beside: one q depends
// on, before one that shares a cited path, since the dependency is the hold
// that would still apply were the paths disjoint.
func waitsOn(q queued, ahead []queued) (heldApart, bool) {
	deps := q.it.Deps()
	for _, e := range ahead {
		if slices.Contains(deps, e.it.ID.Bare()) {
			return heldApart{q.it.ID.Bare(), e.it.ID.Bare(), "depends on it"}, true
		}
	}
	for _, e := range ahead {
		if p := overlap(q.paths, e.paths); p != "" {
			return heldApart{q.it.ID.Bare(), e.it.ID.Bare(), "both cite " + p}, true
		}
	}
	return heldApart{}, false
}

// planWaves is --dry-run's account of a --jobs drive: the items it would start
// together, wave by wave, assuming each wave finishes before the next starts,
// and every pair it would hold apart with what caused it.
func planWaves(items []queued, jobs int) (waves [][]string, held []heldApart) {
	pending := append([]queued(nil), items...)
	for len(pending) > 0 {
		running := map[int]queued{}
		var wave []string
		for len(running) < jobs {
			i, _ := dispatchable(pending, running)
			if i < 0 {
				break
			}
			running[len(running)] = pending[i]
			wave = append(wave, pending[i].it.ID.Bare())
			pending = append(pending[:i:i], pending[i+1:]...)
		}
		waves = append(waves, wave)
	}
	return waves, heldPairs(items)
}

// heldPairs is every pair in the queue that dispatch would hold apart, compared
// pairwise rather than wave by wave: a slot that frees early starts the next
// item while the rest of its wave still runs, so two items the waves split can
// still meet, and that is when the hold applies.
func heldPairs(items []queued) []heldApart {
	var held []heldApart
	for i, q := range items {
		for j := range i {
			if h, waits := waitsOn(q, items[j:j+1]); waits {
				held = append(held, h)
			}
		}
	}
	return held
}

// writeWaves prints planWaves for --dry-run.
func writeWaves(w io.Writer, items []queued, jobs int) {
	waves, held := planWaves(items, jobs)
	fmt.Fprintf(w, "\nWaves at --jobs %d, assuming each wave finishes before the next starts:\n", jobs)
	for i, wave := range waves {
		fmt.Fprintf(w, "  wave %d: %s\n", i+1, strings.Join(wave, " "))
	}
	if len(held) == 0 {
		fmt.Fprintln(w, "No two items cite an overlapping path or depend on each other, so none is held apart.")
		return
	}
	fmt.Fprintln(w, "Held apart whatever wave each lands in, because two items citing one path, or an item and its dep, never run at once:")
	for _, h := range held {
		fmt.Fprintf(w, "  %s waits for %s: %s\n", h.later, h.earlier, h.why)
	}
}

// driveEnqueue is --enqueue.
var driveEnqueue bool

// enqueuePoll is how often a drive with a free slot looks for appended ids
// while it waits on the items it is running. A slot that frees reads them at
// once; this is for the slot that was already free.
var enqueuePoll = 2 * time.Second

// driveHoldBeforeQueue, when set, runs after reconciliation and before the
// drive reads its queue, which is the window an --enqueue can land in with the
// lease already taken and nothing yet queued. Tests set it; nothing else does.
var driveHoldBeforeQueue func()

// enqueued is what one --enqueue did against a running drive, or found that
// none was running.
type enqueued struct {
	// free is no drive holding this checkout, so the caller starts one.
	free bool
	pid  int
	// queued is each appended id with its 1-based place in the drive's queue.
	queued []idPosition
	// ready is the ids that passed the readiness check, in the order given:
	// what a drive started for them would run.
	ready   []string
	refused []string
}

type idPosition struct {
	id  string
	pos int
}

// refusal is exit 3 naming every refused id, nil when none was.
func (e enqueued) refusal() error {
	if len(e.refused) == 0 {
		return nil
	}
	return Refusal{fmt.Errorf("--enqueue refused %d of the ids:\n  %s", len(e.refused), strings.Join(e.refused, "\n  "))}
}

// enqueue appends ids to the queue of the drive running in this checkout, under
// the lock that drive rewrites its queue under. An id is refused when it is
// already queued or running there, or when it is not ready: a scaffold, a done
// item, or one blocked on a dep that neither the drive nor an earlier id in
// this list will run first. The rest are appended either way. A label refuses
// the whole list when a drive is running, since it could only have named a
// drive this call started.
//
// It never touches held: a drive in this process has its own handle, and the
// append goes through the file as it would from another terminal.
func (s *session) enqueue(ids []string, label string) (enqueued, error) {
	path, err := s.leasePath()
	if err != nil {
		return enqueued{}, err
	}
	logDir := s.cfg.LogDir()
	type checked struct {
		id      string
		why     string
		missing []string
	}
	var list []checked
	for _, raw := range ids {
		it, err := s.item(raw)
		if err != nil {
			list = append(list, checked{id: raw, why: err.Error()})
			continue
		}
		c := checked{id: it.ID.Bare()}
		switch state, missing := s.workState(it, logDir); {
		case state == stateBlocked:
			c.missing = missing
		case !drivable(state, true):
			c.why = "not ready: it is " + state
		}
		list = append(list, c)
	}

	var res enqueued
	for range 50 {
		res = enqueued{}
		l, err := lease.Append(path, func(l lease.Lease, busy bool) ([]string, error) {
			if !busy || l.Command != "drive" {
				res.free = true
			}
			if l.Closed {
				return nil, nil
			}
			if !res.free && label != "" {
				return nil, Refusal{fmt.Errorf("%s, and drive pid %d is already running here, so nothing was queued; drop --label to join it", labelOnlyAtStart, l.PID)}
			}
			ahead := append(slices.Clone(l.Running), l.Queue...)
			var add []string
			for _, c := range list {
				why := c.why
				switch {
				case why != "":
				case !res.free && slices.Contains(l.Running, c.id):
					why = fmt.Sprintf("already running in drive pid %d", l.PID)
				case slices.Contains(ahead, c.id):
					why = fmt.Sprintf("already queued at position %d", slices.Index(ahead, c.id)-len(l.Running)+1)
				default:
					gone := slices.DeleteFunc(slices.Clone(c.missing), func(d string) bool { return slices.Contains(ahead, d) })
					if len(gone) > 0 {
						why = "not ready: blocked on " + strings.Join(gone, ", ") + ", which nothing queued ahead of it will run"
					}
				}
				if why != "" {
					res.refused = append(res.refused, c.id+": "+why)
					continue
				}
				ahead = append(ahead, c.id)
				add = append(add, c.id)
			}
			res.ready = add
			if res.free {
				return nil, nil
			}
			return add, nil
		})
		if _, unnamed := errors.AsType[lease.Held](err); unnamed {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err != nil {
			return enqueued{}, err
		}
		if res.free {
			return res, nil
		}
		if l.Closed {
			if len(l.Running) > 0 {
				return enqueued{}, Refusal{fmt.Errorf(
					"drive pid %d has stopped taking ids and is finishing %s; run --enqueue again once it exits",
					l.PID, strings.Join(l.Running, " "))}
			}
			// Its queue ran out and it is on its way out: the next pass finds
			// the checkout free and starts a drive.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		res.pid = l.PID
		for _, id := range res.ready {
			res.queued = append(res.queued, idPosition{id, slices.Index(l.Queue, id) + 1})
		}
		return res, nil
	}
	return enqueued{}, Refusal{fmt.Errorf("the drive in this checkout is still exiting; run --enqueue again in a moment")}
}

// report prints what an append to a running drive did.
func (e enqueued) report(w, errw io.Writer) {
	for _, q := range e.queued {
		fmt.Fprintf(w, "queued %s at position %d in drive pid %d\n", q.id, q.pos, e.pid)
	}
	for _, r := range e.refused {
		fmt.Fprintf(errw, "· refused %s\n", r)
	}
}

// takeAppended is the ids another process appended to this drive's queue since
// it last looked, resolved and announced. closeIfNone closes the queue when
// there are none, which is how a drive whose queue ran out stops taking ids
// in the same step it finds none waiting. at is how many items are already
// pending, so each id is announced at its place. holds says whether this drive
// already has an id waiting or running, which an append made before the drive
// first published its queue cannot have been refused for.
//
// A take that yields nothing runnable takes again under closeIfNone, because
// Take closes only when it finds nothing, and a queue left open by a take whose
// ids all failed to resolve would accept an append nobody reads.
func (s *session) takeAppended(closeIfNone bool, seen map[string]bool, holds func(string) bool, at int) []queued {
	if held == nil {
		return nil
	}
	var out []queued
	for {
		ids, err := held.Take(closeIfNone)
		if err != nil {
			fmt.Fprintf(os.Stderr, "· the queue in this drive's lease would not read, so no appended id is taken: %v\n", err)
			return out
		}
		if len(ids) == 0 {
			return out
		}
		if err := s.reload(); err != nil {
			fmt.Fprintf(os.Stderr, "· the backlog would not reload to read appended ids: %v\n", err)
		}
		for _, id := range ids {
			it, err := s.item(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "· %s was appended and no longer resolves, so it is skipped: %v\n", id, err)
				continue
			}
			if holds(it.ID.Bare()) {
				fmt.Fprintf(os.Stderr, "· --enqueue: %s is already in this drive's queue\n", id)
				continue
			}
			seen[it.ID.Bare()] = true
			out = append(out, newQueued(it, true))
			fmt.Fprintf(os.Stderr, "· --enqueue: %s appended at position %d\n", id, at+len(out))
			events.emit(DriveEvent{Event: evQueued, ID: id, Action: "enqueue", Position: at + len(out)})
		}
		if len(out) > 0 || !closeIfNone {
			return out
		}
	}
}

// closeQueue stops the drive taking ids, and names any appended ids it will now
// never dispatch.
func closeQueue(why string) {
	if held == nil {
		return
	}
	dropped, err := held.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "· the queue in this drive's lease would not close: %v\n", err)
		return
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "· %s, so %s, appended with --enqueue, will not run in this drive\n", why, strings.Join(dropped, " "))
	}
}

// enqueueTo is --enqueue against a drive already known to be running, for the
// --enqueue that lost the race to start one.
func (s *session) enqueueTo(ids []string, label string) error {
	res, err := s.enqueue(ids, label)
	if err != nil {
		return err
	}
	if res.free {
		return Refusal{fmt.Errorf("the drive that took this checkout has already exited; run --enqueue again")}
	}
	res.report(os.Stdout, os.Stderr)
	return res.refusal()
}
