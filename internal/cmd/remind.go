package cmd

import (
	"fmt"
	"time"

	"github.com/ravinald/drover/internal/config"
)

// evAnswered is the event a destination reads when a needs_operator it sent
// reminders for was answered while the drive ran. Like evHeld it is not a
// drive event: it exists only on a destination's stdin.
const evAnswered = "answered"

// What answered a needs_operator, as the answered message names it.
const (
	answeredAccepted = "accepted"
	answeredMerged   = "merged"
	answeredRerun    = "rerun"
	answeredShelved  = "shelved"
	answeredResumed  = "resumed"
)

// reminder is one needs_operator being repeated, shared by every destination
// that repeats it. Read and written under the notifier's mu.
type reminder struct {
	ev   DriveEvent
	mark answerMark
	// priority is what each destination first sent the event at, which every
	// reminder repeats, and sent how many reminders each has sent.
	priority map[string]string
	sent     map[string]int
	done     bool
}

// answerMark is what the project looked like when a needs_operator was
// raised, which is what a later read compares against to tell whether
// somebody acted on it.
type answerMark struct {
	// rows is how many ledger rows named the item, -1 where the ledger would
	// not read, so a row already there is never taken for the answer.
	rows int
	// scaffold is the item still carrying the intake stop marker, which a body
	// row answers only by removing.
	scaffold bool
	// paused is the drive's lease reading paused, for a needs_operator naming
	// no item: the pause raised it, so a resume answers it.
	paused bool
}

// answerProbe reads whether a needs_operator was answered. A test replaces it;
// the drive reads the ledger, the backlog and its lease.
type answerProbe interface {
	mark(cfg *config.Config, e DriveEvent) answerMark
	answered(cfg *config.Config, e DriveEvent, m answerMark) string
}

// ledgerProbe answers from the files the operator's own commands write: a done
// row, a stage row, the body row accept writes, the shelf, and the lease's
// paused flag, which drive --resume clears.
type ledgerProbe struct{}

func (ledgerProbe) mark(cfg *config.Config, e DriveEvent) answerMark {
	if e.ID == "" {
		return answerMark{rows: -1, paused: held != nil && held.Current().Paused}
	}
	s, err := openFrom(cfg)
	if err != nil {
		return answerMark{rows: -1}
	}
	m := answerMark{}
	for _, r := range s.ledger.Rows {
		if r.ID == e.ID {
			m.rows++
		}
	}
	if it, err := s.item(e.ID); err == nil {
		m.scaffold = s.isScaffold(it)
	}
	return m
}

// answered names what answered e since m was taken, empty while nothing has.
// A ledger that will not read answers nothing, so the reminder goes out: a
// reminder too many costs a glance, and one too few is the idle queue this
// exists to prevent.
func (ledgerProbe) answered(cfg *config.Config, e DriveEvent, m answerMark) string {
	if e.ID == "" {
		if m.paused && held != nil && !held.Current().Paused {
			return answeredResumed
		}
		return ""
	}
	s, err := openFrom(cfg)
	if err != nil {
		return ""
	}
	if state, _ := s.resolve(e.ID); state == idShelved {
		return answeredShelved
	}
	if m.rows < 0 {
		return ""
	}
	seen := 0
	for _, r := range s.ledger.Rows {
		if r.ID != e.ID {
			continue
		}
		seen++
		if seen <= m.rows {
			continue
		}
		switch {
		case r.Merged != nil && *r.Merged:
			return answeredMerged
		case r.Stage != "":
			return answeredRerun
		case r.Body != "" && m.scaffold:
			if it, err := s.item(e.ID); err == nil && !s.isScaffold(it) {
				return answeredAccepted
			}
		}
	}
	return ""
}

// watchLocked starts repeating e at each destination in dests, which sent it at
// the priority prio names.
func (n *notifier) watchLocked(e DriveEvent, dests []*destination, prio map[string]string) {
	r := &reminder{ev: e, mark: n.probe.mark(n.cfg, e), priority: prio, sent: map[string]int{}}
	for _, d := range dests {
		name := d.Name
		notifyAfter(d.Repeat, func() { n.remind(r, name) })
	}
}

// remind is one destination's reminder coming due. It reads whether the event
// was answered before sending anything, outside the lock, since that reads
// files. Once a destination has sent repeat_max reminders it keeps reading at
// the same interval, sending nothing, so an answer that comes after the last
// reminder still gets its answered message.
func (n *notifier) remind(r *reminder, name string) {
	n.mu.Lock()
	if n.closed || r.done {
		n.mu.Unlock()
		return
	}
	probe, cfg := n.probe, n.cfg
	n.mu.Unlock()
	action := probe.answered(cfg, r.ev, r.mark)

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || r.done {
		return
	}
	if action != "" {
		r.done = true
		n.answeredLocked(r, action)
		return
	}
	d := n.destLocked(name)
	if d == nil || d.Repeat == 0 {
		return
	}
	if _, ok := d.Priority(evNeedsOperator); !ok {
		return
	}
	if r.sent[name] < d.RepeatMax {
		r.sent[name]++
		n.enqueueLocked(d, notice{event: r.ev, priority: r.priority[name], body: noticeBody(r.ev),
			reminder: fmt.Sprintf("(reminder %d of %d)", r.sent[name], d.RepeatMax)})
	}
	notifyAfter(d.Repeat, func() { n.remind(r, name) })
}

// answeredLocked tells each destination that sent a reminder for r that it was
// answered. One that only ever sent the first message hears nothing more: it
// never pressed the operator, so it has nothing to call off.
func (n *notifier) answeredLocked(r *reminder, action string) {
	body := "answered: " + action
	if r.ev.ID != "" {
		body += " " + r.ev.ID
	}
	e := DriveEvent{Event: evAnswered, TS: time.Now().UTC().Format(time.RFC3339), Drive: r.ev.Drive,
		Project: r.ev.Project, ID: r.ev.ID, Action: r.ev.Action}
	now := notifyClock()
	for _, d := range n.dests {
		if r.sent[d.Name] > 0 {
			n.deliverLocked(d, notice{event: e, priority: "low", body: body}, now)
		}
	}
}

// destLocked is the current destination of that name, nil once a reload
// dropped it.
func (n *notifier) destLocked(name string) *destination {
	for _, d := range n.dests {
		if d.Name == name {
			return d
		}
	}
	return nil
}
