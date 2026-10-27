package cmd

import (
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
)

// ExitSummary is what a drive left done and undone, on exit alone. It is the
// account an operator away from the terminal needs: a drive that exits 0 with
// one item parked and another skipped behind it has still left work waiting.
type ExitSummary struct {
	Merged       int `json:"merged"`
	ReadyToMerge int `json:"ready_to_merge"`
	Parked       int `json:"parked"`
	Skipped      int `json:"skipped"`
	Held         int `json:"held"`
	// Undone is each item that did not merge, in the order the drive finished
	// it.
	Undone []UndoneItem `json:"undone,omitempty"`
	// Weighted is runlog.Weighted over the usage rows the drive's sessions
	// wrote.
	Weighted int64 `json:"weighted"`
	// Ended is why the drive ended: one of the ended* constants.
	Ended string `json:"ended"`
}

// UndoneItem is one item a drive did not merge, and why.
type UndoneItem struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Park    string `json:"park,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Reason  string `json:"reason,omitempty"`
	PR      int    `json:"pr,omitempty"`
	// BlockedBy is each unmerged dependency of an item skipped as blocked, with
	// what it came to: its outcome in this drive where the drive ran it, else
	// its state in the backlog.
	BlockedBy []Blocker `json:"blocked_by,omitempty"`
}

// Blocker is one dependency standing between an item and its run.
type Blocker struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Why a drive ended.
const (
	endedDrained     = "drained"
	endedDeadline    = "deadline"
	endedMaxItems    = "max_items"
	endedStopped     = "stopped"
	endedInterrupted = "interrupted"
	endedFault       = "fault"
)

// undone reports an exit that leaves an item parked, skipped or held, which is
// what routes it at exit_undone.
func (s *ExitSummary) undone() bool {
	return s != nil && s.Parked+s.Skipped+s.Held > 0
}

// exitSummary counts what the drive's items came to, one outcome per item, its
// latest: an item parked and then taken again from --enqueue is done twice. done
// is what this process finished; the outcomes the drive noted before a re-exec
// carried it here are added for the ids done does not hold, with the outcome
// alone, since that is all the handoff kept.
func (s *session) exitSummary(done []DriveItem, ended string) *ExitSummary {
	all := ledger.Latest(done, func(d DriveItem) string { return d.ID })
	if d := activeDrive.Load(); d != nil {
		d.mu.Lock()
		for _, o := range ledger.Latest(d.outcomes, func(o ledger.Outcome) string { return o.ID }) {
			if !slices.ContainsFunc(all, func(x DriveItem) bool { return x.ID == o.ID }) {
				all = append(all, DriveItem{ID: o.ID, Outcome: o.Outcome})
			}
		}
		d.mu.Unlock()
	}
	byID := make(map[string]DriveItem, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	sum := &ExitSummary{Ended: ended}
	for _, d := range all {
		if d.Merged || d.Outcome == outcomeMergedByGate || d.Outcome == outcomeMergedAtForge {
			sum.Merged++
			continue
		}
		u := UndoneItem{ID: d.ID, Outcome: d.Outcome, Park: d.Park, Detail: d.Detail, Reason: d.Reason, PR: d.PR}
		switch d.Outcome {
		case outcomeReviewed:
			sum.ReadyToMerge++
		case outcomeParked:
			sum.Parked++
		case outcomeHeld:
			sum.Held++
		case outcomeSkipped:
			sum.Skipped++
			for _, dep := range d.blockedBy {
				u.BlockedBy = append(u.BlockedBy, Blocker{ID: dep, State: s.blockerState(dep, byID)})
			}
		}
		sum.Undone = append(sum.Undone, u)
	}
	return sum
}

// blockerState is what a dependency came to: its park in this drive where it
// parked here, its outcome where it ran here otherwise, and its backlog state
// where the drive never reached it.
func (s *session) blockerState(id string, byID map[string]DriveItem) string {
	if d, ok := byID[id]; ok {
		if d.Park != "" {
			return d.Outcome + " " + d.Park
		}
		return d.Outcome
	}
	it, err := s.item(id)
	if err != nil {
		return "unknown"
	}
	state, _ := s.workState(it, s.cfg.LogDir())
	return state
}

// driveWeighted sums the weighted tokens of every usage row naming drive id. A
// ledger that will not read sums to zero: the summary is a report, and the
// exit it rides on must still go out.
func driveWeighted(path, id string) int64 {
	if id == "" {
		return 0
	}
	l, err := ledger.Load(path)
	if err != nil {
		return 0
	}
	var total float64
	for _, r := range l.Rows {
		if r.Drive == id && r.Usage != nil {
			total += runlog.Weighted(*r.Usage)
		}
	}
	return int64(math.Round(total))
}

// ntfyLimit is ntfy's message size limit. A longer body arrives as an
// attachment, which a phone's lock screen shows as a file name.
const ntfyLimit = 4096

// notice is one message bound for one destination: the event it stands for,
// or the last of the events it folds, the priority it was routed at and the
// body a person reads.
type notice struct {
	event    DriveEvent
	priority string
	body     string
	folded   []DriveEvent
	// reminder is "(reminder N of M)" on a repeated needs_operator, which the
	// title carries.
	reminder string
}

// NotifyPayload is what a command destination reads on stdin: the event, the
// priority it was routed at, the body an ntfy destination would send, and the
// events a coalesced or held message stands for.
type NotifyPayload struct {
	DriveEvent
	Priority string       `json:"priority"`
	Message  string       `json:"message"`
	Folded   []DriveEvent `json:"folded,omitempty"`
	Reminder string       `json:"reminder,omitempty"`
}

// evHeld is the event a command destination reads for what quiet hours held,
// sent when the window ends. It is not a drive event: it exists only on a
// destination's stdin.
const evHeld = "held"

// notifyClock and notifyAfter are what routing reads the time from and how it
// waits, apart so a test can stand inside quiet hours and fire a timer without
// sleeping.
var (
	notifyClock = time.Now
	notifyAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
)

// queueDepth bounds what one destination may fall behind by. A destination
// routing every stage event behind a server timing out at ten seconds a send
// would otherwise grow without bound for the length of the drive.
const queueDepth = 256

// notifier routes a drive's events to [notify]'s destinations. Each destination
// has a queue and a goroutine of its own, so a slow one delays nothing else:
// not the dispatcher, and not another destination.
type notifier struct {
	project, root string
	cfg           *config.Config
	routing       config.Routing
	probe         answerProbe
	// forge resolves the repository URL a pull request's Click header links
	// under, once per drive and only when a message needs it.
	forge     func() string
	forgeOnce sync.Once
	forgeURL  string
	// opened records a thread_opened in the drive's event file. threads is the
	// thread each item's command destination last answered with, which a
	// needs_operator carries to the bridge so it reuses the thread, and which
	// a message with no pull request links its Click header to.
	opened    func(DriveEvent)
	threadsMu sync.Mutex
	threads   map[string]DriveEvent

	mu     sync.Mutex
	dests  []*destination
	closed bool
	// exited is the exit routed, after which no reminder goes out: the exit
	// message is the last word, and close releases mu before it sets closed.
	exited    bool
	releaseAt time.Time
	wg        sync.WaitGroup
}

// destination is one [notify.destinations.<name>] with its queue. pending,
// held and retired are read under the notifier's mu.
type destination struct {
	config.Destination
	n        *notifier
	queue    chan notice
	inflight sync.WaitGroup
	// pending is the events of each kind coalescing, and held what quiet hours
	// keep back.
	pending map[string][]DriveEvent
	held    []notice
	retired bool
	said    *stderrSaid
}

// stderrSaid is what a destination has already said on stderr, each said once per
// drive. It outlives a reload for a destination the reload left pointing where
// it did, and is atomic because a retired destination's goroutine may still be
// draining its queue while its successor sends.
type stderrSaid struct{ failed, loose, dropped atomic.Bool }

// sameEndpoint reports whether b sends where a did, which is what decides
// whether a failure a reported still speaks for b.
func sameEndpoint(a, b config.Destination) bool {
	return a.Name == b.Name && a.Type == b.Type && a.URL == b.URL && a.TokenFile == b.TokenFile && slices.Equal(a.Argv, b.Argv)
}

func newNotifier(cfg *config.Config) *notifier {
	n := &notifier{project: cfg.Project, root: cfg.Root, probe: ledgerProbe{}}
	n.load(cfg)
	return n
}

// load starts a goroutine per destination cfg names. Called with mu held, or
// before the notifier is shared.
func (n *notifier) load(cfg *config.Config) {
	prev := n.dests
	n.cfg = cfg
	n.routing = cfg.Notifications()
	n.dests = nil
	for _, d := range n.routing.Destinations {
		said := &stderrSaid{}
		for _, p := range prev {
			if sameEndpoint(p.Destination, d) {
				said = p.said
			}
		}
		dest := &destination{Destination: d, n: n, queue: make(chan notice, queueDepth), pending: map[string][]DriveEvent{}, said: said}
		n.dests = append(n.dests, dest)
		n.wg.Add(1)
		go dest.run()
	}
}

// routes reports whether any [notify] destination routes the event.
func routes(cfg *config.Config, event string) bool {
	return slices.ContainsFunc(cfg.Notifications().Destinations, func(d config.Destination) bool {
		_, ok := d.Priority(event)
		return ok
	})
}

// immediate are the events never coalesced or held: each is somebody being
// asked to act, or the last word on the drive.
func immediate(event string) bool {
	return event == evNeedsOperator || event == evNeedsSupervisor || event == evExit
}

func (n *notifier) quiet(now time.Time) bool {
	return n.routing.Quiet != nil && n.routing.Quiet.Contains(now)
}

// route hands one event to every destination that names it.
func (n *notifier) route(e DriveEvent) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	if e.Event == evExit {
		n.exited = true
	}
	now := notifyClock()
	n.releaseLocked(now)
	var repeat []*destination
	prio := map[string]string{}
	for _, d := range n.dests {
		if e.Event == evExit {
			n.exitLocked(d, e, now)
			continue
		}
		p, ok := d.Priority(e.Event)
		if !ok {
			continue
		}
		if n.routing.Coalesce > 0 && !immediate(e.Event) {
			first := len(d.pending[e.Event]) == 0
			d.pending[e.Event] = append(d.pending[e.Event], e)
			if first {
				kind := e.Event
				notifyAfter(n.routing.Coalesce, func() { n.flushKind(d, kind) })
			}
			continue
		}
		n.deliverLocked(d, notice{event: e, priority: p, body: noticeBody(e)}, now)
		if e.Event == evNeedsOperator && d.Repeat > 0 {
			repeat = append(repeat, d)
			prio[d.Name] = p
		}
	}
	if len(repeat) > 0 {
		n.watchLocked(e, repeat, prio)
	}
}

// flushKind sends what coalesced of one kind, as one message.
func (n *notifier) flushKind(d *destination, kind string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if d.retired || n.closed {
		return
	}
	n.flushKindLocked(d, kind, notifyClock())
}

func (n *notifier) flushKindLocked(d *destination, kind string, now time.Time) {
	evs := d.pending[kind]
	delete(d.pending, kind)
	if len(evs) == 0 {
		return
	}
	p, _ := d.Priority(kind)
	n.deliverLocked(d, coalesced(kind, p, evs), now)
}

// coalesced is one message for several events of a kind, listing their ids.
func coalesced(kind, priority string, evs []DriveEvent) notice {
	last := evs[len(evs)-1]
	if len(evs) == 1 {
		return notice{event: last, priority: priority, body: noticeBody(last)}
	}
	var ids []string
	for _, e := range evs {
		switch {
		case e.ID != "":
			ids = append(ids, e.ID)
		case e.Ref != "":
			ids = append(ids, e.Ref)
		}
	}
	body := fmt.Sprintf("%s: %d events", kind, len(evs))
	if len(ids) > 0 {
		body = kind + ": " + strings.Join(ids, " ")
	}
	last.ID, last.PR = "", 0
	return notice{event: last, priority: priority, body: body, folded: evs}
}

// deliverLocked queues a notice, or holds it while quiet hours stand and it
// was routed below high.
func (n *notifier) deliverLocked(d *destination, nt notice, now time.Time) {
	if n.quiet(now) && config.PriorityRank(nt.priority) < config.PriorityRank("high") && !immediate(nt.event.Event) {
		d.held = append(d.held, nt)
		if end := n.routing.Quiet.Until(now); !end.Equal(n.releaseAt) {
			n.releaseAt = end
			notifyAfter(end.Sub(now), n.release)
		}
		return
	}
	n.enqueueLocked(d, nt)
}

// release sends what quiet hours held, once the window has ended. A timer that
// fires early finds the window still standing and sets itself again.
func (n *notifier) release() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	now := notifyClock()
	n.releaseAt = time.Time{}
	if n.quiet(now) {
		for _, d := range n.dests {
			if len(d.held) > 0 {
				n.releaseAt = n.routing.Quiet.Until(now)
				notifyAfter(n.releaseAt.Sub(now), n.release)
				return
			}
		}
		return
	}
	n.releaseLocked(now)
}

func (n *notifier) releaseLocked(now time.Time) {
	if n.quiet(now) {
		return
	}
	for _, d := range n.dests {
		if len(d.held) > 0 {
			n.enqueueLocked(d, n.heldNotice(d.held, ""))
			d.held = nil
		}
	}
}

// heldNotice is one message for what quiet hours held, at the highest priority
// among them unless priority names one.
func (n *notifier) heldNotice(held []notice, priority string) notice {
	if priority == "" {
		for _, h := range held {
			if config.PriorityRank(h.priority) > config.PriorityRank(priority) {
				priority = h.priority
			}
		}
	}
	var folded []DriveEvent
	for _, h := range held {
		if len(h.folded) > 0 {
			folded = append(folded, h.folded...)
		} else {
			folded = append(folded, h.event)
		}
	}
	e := DriveEvent{Event: evHeld, TS: time.Now().UTC().Format(time.RFC3339), Project: n.project}
	if len(held) > 0 {
		e.Drive = held[len(held)-1].event.Drive
	}
	return notice{event: e, priority: priority, body: fit(nil, heldLines(held), nil), folded: folded}
}

// heldLines is one line per held notice: its item, and its body's first line.
func heldLines(held []notice) []string {
	lines := []string{fmt.Sprintf("held during quiet hours (%d):", len(held))}
	for _, h := range held {
		first, _, _ := strings.Cut(h.body, "\n")
		if h.event.ID != "" {
			first = h.event.ID + " " + first
		}
		lines = append(lines, first)
	}
	return lines
}

// exitLocked sends the exit to one destination: whatever coalesced first, then
// the exit with what quiet hours held folded into it.
func (n *notifier) exitLocked(d *destination, e DriveEvent, now time.Time) {
	for _, kind := range slices.Sorted(maps.Keys(d.pending)) {
		n.flushKindLocked(d, kind, now)
	}
	p, ok := d.Priority(evExit)
	if e.Summary.undone() {
		if pu, set := d.Priority(config.EventExitUndone); set {
			p, ok = pu, true
		}
	}
	held := d.held
	d.held = nil
	quiet := n.quiet(now)
	switch {
	case !ok && len(held) == 0:
		return
	case !ok:
		h := n.heldNotice(held, "")
		if quiet {
			h.priority = "min"
		}
		n.enqueueLocked(d, h)
		return
	}
	nt := notice{event: e, priority: p, body: exitBody(e, held)}
	for _, h := range held {
		if len(h.folded) > 0 {
			nt.folded = append(nt.folded, h.folded...)
		} else {
			nt.folded = append(nt.folded, h.event)
		}
	}
	// ntfy delivers min without sound, which is what an exit at three in the
	// morning that asks nothing of anybody should be.
	if quiet && config.PriorityRank(p) < config.PriorityRank("high") {
		nt.priority = "min"
	}
	n.enqueueLocked(d, nt)
}

func (n *notifier) enqueueLocked(d *destination, nt notice) {
	if n.closed || d.retired {
		return
	}
	d.inflight.Add(1)
	select {
	case d.queue <- nt:
	default:
		d.inflight.Done()
		if d.said.dropped.CompareAndSwap(false, true) {
			fmt.Fprintf(errOut, "· [notify.destinations.%s] is %d messages behind; dropping this one and any more it cannot take, without reporting them\n",
				d.Name, queueDepth)
		}
	}
}

// idle waits for every message already queued to be sent or to fail.
func (n *notifier) idle() {
	if n == nil {
		return
	}
	n.mu.Lock()
	dests := slices.Clone(n.dests)
	n.mu.Unlock()
	for _, d := range dests {
		d.inflight.Wait()
	}
}

// close sends whatever coalescing or quiet hours still hold, waits for every
// queue to drain, and stops the goroutines. Each send is bounded, so this is
// too. Nothing routes after it.
func (n *notifier) close() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.flushAllLocked()
	n.closed = true
	for _, d := range n.dests {
		close(d.queue)
	}
	n.mu.Unlock()
	n.wg.Wait()
}

// flush queues everything coalescing or held and waits for it to send.
func (n *notifier) flush() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if !n.closed {
		n.flushAllLocked()
	}
	n.mu.Unlock()
	n.idle()
}

// flushAllLocked queues everything coalescing or held, whatever the clock, for
// a notifier about to stop routing.
func (n *notifier) flushAllLocked() {
	now := notifyClock()
	for _, d := range n.dests {
		for _, kind := range slices.Sorted(maps.Keys(d.pending)) {
			evs := d.pending[kind]
			delete(d.pending, kind)
			p, _ := d.Priority(kind)
			n.enqueueLocked(d, coalesced(kind, p, evs))
		}
		if len(d.held) > 0 {
			h := n.heldNotice(d.held, "")
			if n.quiet(now) {
				h.priority = "min"
			}
			n.enqueueLocked(d, h)
			d.held = nil
		}
	}
}

// reload points the notifier at the config a resume loaded. What the old
// destinations still hold goes out through them, and their goroutines stop
// once it has.
func (n *notifier) reload(cfg *config.Config) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.flushAllLocked()
	for _, d := range n.dests {
		d.retired = true
		close(d.queue)
	}
	n.releaseAt = time.Time{}
	n.load(cfg)
}

func (d *destination) run() {
	defer d.n.wg.Done()
	for nt := range d.queue {
		d.send(nt)
		d.inflight.Done()
	}
}

// send delivers one notice, reporting the first failure of the drive and
// nothing after it.
func (d *destination) send(nt notice) {
	where := "[notify.destinations." + d.Name + "]"
	switch d.Type {
	case config.DestNtfy:
		if loose := tokenLoose(d.TokenFile); loose != "" && d.said.loose.CompareAndSwap(false, true) {
			fmt.Fprintf(errOut, "· %s %s; the send goes out\n", where, loose)
		}
		res, err := postNtfy(d.Destination, nt, d.n.click(nt.event))
		if err != nil && d.said.failed.CompareAndSwap(false, true) {
			fmt.Fprintf(errOut, "· %s POST %s failed (%v)%s; the drive goes on, and later failures here are not reported\n",
				where, d.URL, err, res.bodyClause())
		}
	case config.DestCommand:
		p := nt.payload()
		if th, ok := d.n.thread(p.ID); ok && asksSomebody(p.Event) {
			p.Thread, p.URL = th.Thread, th.URL
		}
		out, err := runArgvNotifier(d.n.root, d.Argv, p)
		d.n.threadsFrom(out)
		if err != nil && d.said.failed.CompareAndSwap(false, true) {
			fmt.Fprintf(errOut, "· %s %s failed (%v)%s; the drive goes on, and later failures here are not reported\n",
				where, strings.Join(d.Argv, " "), err, outClause(out))
		}
	}
}

func (nt notice) payload() NotifyPayload {
	return NotifyPayload{DriveEvent: nt.event, Priority: nt.priority, Message: nt.body, Folded: nt.folded, Reminder: nt.reminder}
}

// click is the pull request URL an event concerns, or, where it names no pull
// request or the repository would not resolve, the T3 Code thread opened for
// its item. Empty when it has neither.
func (n *notifier) click(e DriveEvent) string {
	if e.PR != 0 && n.forge != nil {
		n.forgeOnce.Do(func() { n.forgeURL = n.forge() })
		if n.forgeURL != "" {
			return n.forgeURL + "/pull/" + strconv.Itoa(e.PR)
		}
	}
	if th, ok := n.thread(e.ID); ok {
		return th.URL
	}
	return ""
}

// asksSomebody is an event the bridge opens a thread for.
func asksSomebody(event string) bool {
	return event == evNeedsOperator || event == evNeedsSupervisor
}

// thread is the thread_opened this drive holds for an item.
func (n *notifier) thread(id string) (DriveEvent, bool) {
	if n == nil || id == "" {
		return DriveEvent{}, false
	}
	n.threadsMu.Lock()
	defer n.threadsMu.Unlock()
	th, ok := n.threads[id]
	return th, ok
}

// threadsFrom reads the thread_opened lines a command destination printed and
// keeps each, recording one whose link this drive did not already hold for
// that item: a thread the bridge reused is not news.
func (n *notifier) threadsFrom(out string) {
	for th := range threadLines(out) {
		n.threadsMu.Lock()
		prev, held := n.threads[th.ID]
		if n.threads == nil {
			n.threads = map[string]DriveEvent{}
		}
		n.threads[th.ID] = th
		n.threadsMu.Unlock()
		if (!held || prev.URL != th.URL) && n.opened != nil {
			n.opened(th)
		}
	}
}

// threadLines are the thread_opened events in a destination's output that
// name an item and a link, the bridge's answer among whatever else it printed.
func threadLines(out string) iter.Seq[DriveEvent] {
	return func(yield func(DriveEvent) bool) {
		for line := range strings.Lines(out) {
			var e DriveEvent
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil || e.Event != evThreadOpened || e.ID == "" || e.URL == "" {
				continue
			}
			if !yield(DriveEvent{Event: evThreadOpened, ID: e.ID, Thread: e.Thread, URL: e.URL}) {
				return
			}
		}
	}
}

// noticeBody is what a person reads on the phone for one event: what to do
// where somebody has to act, and otherwise what happened, in a line or a few.
func noticeBody(e DriveEvent) string {
	switch e.Event {
	case evStarted:
		b := "started " + e.Drive + ": " + strings.Join(e.Queue, " ")
		if e.Jobs > 1 {
			b += fmt.Sprintf(", %d at once", e.Jobs)
		}
		if e.Deadline != "" {
			return b + ", until " + localTime(e.Deadline)
		}
		return b + ", no deadline"
	case evMerged:
		b := "merged"
		if e.PR > 0 {
			b += fmt.Sprintf(" #%d", e.PR)
		}
		if e.Title != "" {
			b += ": " + e.Title
		}
		return b
	case evParked:
		b := "parked " + e.Park
		if e.Detail != "" {
			b += " (" + e.Detail + ")"
		}
		if e.Reason != "" {
			b += ": " + e.Reason
		}
		if e.Evidence != "" {
			b += "\n" + firstLines(e.Evidence, 5)
		}
		return b
	case evNeedsOperator, evNeedsSupervisor:
		switch {
		case e.Run != "" && e.Action != "":
			return e.Action + ": " + e.Run
		case e.Run != "":
			return e.Run
		case e.Reason != "":
			return e.Reason
		}
	case evThreadOpened:
		return "T3 Code thread opened: " + e.URL
	case evUsageWarning:
		b := e.Window
		if e.Remaining != nil {
			b += fmt.Sprintf(": %d%% left", *e.Remaining)
		}
		if e.ResetsAt != "" {
			b += ", resets " + localTime(e.ResetsAt)
		}
		return b
	case evExit:
		return exitBody(e, nil)
	case evQueued:
		return fmt.Sprintf("queued at position %d", e.Position)
	case evDispatched:
		if e.Slot > 0 {
			return fmt.Sprintf("dispatched to slot %d", e.Slot)
		}
		return "dispatched"
	case evSkipped:
		b := "skipped " + e.State
		if e.Reason != "" {
			b += ": " + e.Reason
		}
		return b
	case evStage:
		if e.Result != "" {
			return e.Stage + ": " + e.Result
		}
		return e.Stage
	case evCheck:
		b := "check " + e.Check + " " + e.Class
		if e.Evidence != "" {
			b += ": " + firstLines(e.Evidence, 1)
		}
		return b
	case evFiled:
		return "filed " + e.Kind + " " + e.Ref
	case evPreflight:
		return e.Action + ": " + e.Reason
	case evConfigChanged:
		switch {
		case e.Error != "":
			return "config changed on disk and will not load: " + e.Error
		case len(e.Tables) > 0:
			return "config changed on disk, not applied until --pause then --resume: " + strings.Join(e.Tables, ", ")
		}
		return "config changed on disk, not applied until --pause then --resume"
	case evPaused, evResumed:
		switch {
		case e.Reason != "":
			return e.Event + ": " + e.Reason
		case len(e.Changed) > 0:
			return e.Event + ", reloaded " + strings.Join(e.Changed, ", ")
		}
	}
	if e.Reason != "" {
		return e.Reason
	}
	return e.Event
}

// exitBody renders an exit's summary, then whatever quiet hours held, within
// ntfy's limit.
func exitBody(e DriveEvent, held []notice) string {
	code := 0
	if e.Code != nil {
		code = *e.Code
	}
	s := e.Summary
	if s == nil {
		s = &ExitSummary{Ended: endedDrained}
	}
	name := "the drive"
	if e.Drive != "" {
		name = e.Drive
	}
	head := []string{fmt.Sprintf("%s exited %d: %s", name, code, endedPhrase(s.Ended, e.Reason))}
	counts := fmt.Sprintf("%d merged · %d parked · %d skipped · %d held", s.Merged, s.Parked, s.Skipped, s.Held)
	if s.ReadyToMerge > 0 {
		counts += fmt.Sprintf(" · %d ready to merge", s.ReadyToMerge)
	}
	head = append(head, counts)
	var items []string
	for _, u := range s.Undone {
		items = append(items, undoneLine(u))
	}
	var tail []string
	if s.Weighted > 0 {
		tail = append(tail, tokenCount(float64(s.Weighted))+" weighted tokens")
	}
	if len(held) > 0 {
		tail = append(tail, heldLines(held)...)
	}
	return fit(head, items, tail)
}

func endedPhrase(ended, reason string) string {
	switch ended {
	case endedDeadline:
		return "the deadline arrived"
	case endedMaxItems:
		return "--max-items was reached"
	case endedStopped:
		return "stopped by drive --stop"
	case endedInterrupted:
		return "interrupted"
	case endedFault:
		if reason != "" {
			return "stopped on a fault: " + reason
		}
		return "stopped on a fault"
	}
	return "the queue drained"
}

// undoneLine is one item the drive did not merge, as `E72 skipped: blocked by
// F14 (parked gate_refused)`.
func undoneLine(u UndoneItem) string {
	switch u.Outcome {
	case outcomeParked:
		b := u.ID + " parked " + u.Park
		if u.Detail != "" {
			b += " (" + u.Detail + ")"
		}
		if u.Reason != "" {
			b += ": " + firstLines(u.Reason, 1)
		}
		return b
	case outcomeSkipped:
		if len(u.BlockedBy) > 0 {
			var by []string
			for _, b := range u.BlockedBy {
				by = append(by, b.ID+" ("+b.State+")")
			}
			return u.ID + " skipped: blocked by " + strings.Join(by, ", ")
		}
		return u.ID + " skipped: " + u.Park
	case outcomeReviewed:
		if u.PR > 0 {
			return fmt.Sprintf("%s ready to merge: #%d", u.ID, u.PR)
		}
		return u.ID + " ready to merge"
	}
	if u.Reason != "" {
		return u.ID + " " + u.Outcome + ": " + firstLines(u.Reason, 1)
	}
	return u.ID + " " + u.Outcome
}

// fit joins head, as many items as fit and tail within ntfyLimit, listing the
// items left out as "and N more". head and tail are short and always kept,
// each line cut where one alone would overflow.
func fit(head, items, tail []string) string {
	cut := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			if len(l) > ntfyLimit/4 {
				l = trunc(l, ntfyLimit/8)
			}
			out[i] = l
		}
		return out
	}
	head, items, tail = cut(head), cut(items), cut(tail)
	size := func(lines []string) int {
		n := 0
		for _, l := range lines {
			n += len(l) + 1
		}
		return n
	}
	budget := ntfyLimit - size(head) - size(tail)
	kept := items
	if size(items) > budget {
		kept = nil
		for k := len(items) - 1; k >= 0; k-- {
			more := fmt.Sprintf("and %d more", len(items)-k)
			if size(items[:k])+len(more)+1 <= budget {
				kept = append(slices.Clone(items[:k]), more)
				break
			}
		}
	}
	lines := append(append(slices.Clone(head), kept...), tail...)
	out := strings.Join(lines, "\n")
	if len(out) > ntfyLimit {
		out = strings.ToValidUTF8(out[:ntfyLimit], "")
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// localTime renders an RFC 3339 instant in local time for a phone, and leaves
// anything else as it came.
func localTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("Mon Jan 2 15:04 MST")
}
