package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
)

// The windows a usage step names, as a usage_warning event and its ledger row
// carry them. weekly and five_hour are the [usage] tables; overage is entering
// purchased credit, and overage_usd its list-price sum reaching a level.
const (
	stepWeekly     = config.UsageWeekly
	stepFiveHour   = config.UsageFiveHour
	stepOverage    = "overage"
	stepOverageUSD = "overage_usd"
)

// outcomeHeld is an item a usage hold kept from starting or finishing when the
// drive's --for or --until ran out first. Nothing failed: the account had
// nothing left, and the item runs on the next drive.
const outcomeHeld = "held"

// errUsageDeadline is the drain cause of a drive whose deadline arrived while
// a usage hold kept it paused, and how an item tells its stop from a --stop.
var errUsageDeadline = errors.New("the deadline arrived while a usage hold kept the drive paused")

// accountReading is the newest rate_limit reading among the usage rows of every
// registered project, the project and instant of the row it came from, and
// what the rest of those rows say about the same account.
type accountReading struct {
	Reading *runlog.RateLimit
	Project string
	At      time.Time
	// OverageUSD is the list-price cost_usd of every usage row whose reading
	// was on overage since the weekly window of the newest reading began.
	OverageUSD float64
	warnings   []ledger.Row
	Unread     []string
}

// warned reports whether any registered project already sent the warning.
func (a accountReading) warned(window string, resets int64) bool {
	for _, r := range a.warnings {
		if r.UsageWarning == window && r.ResetsAt == resets {
			return true
		}
	}
	return false
}

// readAccount reads the reading an admission judges by. The limit is the
// account's, so a reading another project's drive recorded an hour ago says
// more about this launch than this project's own from yesterday.
func (s *session) readAccount() accountReading {
	ledgers, unread := s.accountLedgers()
	a := accountReading{Unread: unread}
	for _, al := range ledgers {
		a.warnings = append(a.warnings, al.ledger.Warnings...)
		for _, r := range al.ledger.Usage {
			at, ok := r.At()
			if r.RateLimit == nil || !ok || (a.Reading != nil && !at.After(a.At)) {
				continue
			}
			a.Reading, a.Project, a.At = r.RateLimit, al.project, at
		}
	}
	week := a.Reading.WindowReset(runlog.WindowSevenDay)
	if week.IsZero() {
		return a
	}
	since := week.Add(-7 * 24 * time.Hour)
	for _, al := range ledgers {
		for _, r := range al.ledger.Usage {
			if at, ok := r.At(); ok && r.RateLimit.InOverage() && !at.Before(since) {
				a.OverageUSD += r.CostUSD
			}
		}
	}
	return a
}

// usageStep is one level a reading crossed: a notice to send or a hold to take.
type usageStep struct {
	Window string
	// Remaining is the percent of the window left, nil for overage, which has
	// no figure.
	Remaining *int
	Resets    time.Time
	Reason    string
}

// key is what a warning row and a waived hold are matched on: the window and
// the reset of the window it is about.
func (u usageStep) key() string { return u.Window + "@" + strconv.FormatInt(u.Resets.Unix(), 10) }

// event fills the fields usage_warning, paused, needs_operator and resumed
// carry about one step.
func (u usageStep) event(ev DriveEvent) DriveEvent {
	ev.Window, ev.Remaining, ev.Reason = u.Window, u.Remaining, u.Reason
	if !u.Resets.IsZero() {
		ev.ResetsAt = u.Resets.Local().Format(time.RFC3339)
	}
	return ev
}

// usageVerdict is what one reading means under the config: the notices to send
// and the hold to take, nil for none.
type usageVerdict struct {
	warnings []usageStep
	hold     *usageStep
}

// localReset is a reset instant as an operator reads it, on their own clock.
func localReset(t time.Time) string {
	if t.IsZero() {
		return "an instant the reading does not name"
	}
	return t.Local().Format("Mon Jan 2 15:04 MST")
}

// windowNames maps a [usage] table onto the window Claude reports it as.
var windowNames = []struct{ table, window, says string }{
	{stepWeekly, runlog.WindowSevenDay, "weekly"},
	{stepFiveHour, runlog.WindowFiveHour, "five-hour"},
}

// judgeUsage is what the reading in a means under cfg at now. It reads nothing
// and writes nothing, so every level it can cross is a table test.
//
// A window whose reset has passed reads 100% left: the reading predates the
// reset, and a drive resumed at the reset would otherwise hold again on the
// reading that held it.
func judgeUsage(cfg *config.Config, a accountReading, now time.Time) usageVerdict {
	var v usageVerdict
	r := a.Reading
	if r == nil {
		return v
	}
	take := func(u usageStep) {
		if v.hold == nil {
			v.hold = &u
		}
	}
	for _, w := range windowNames {
		left, ok := r.Remaining(w.window, now)
		if !ok {
			continue
		}
		reset := r.WindowReset(w.window)
		pct := left
		if n, set := cfg.UsageLevel(w.table, "notify"); set && left <= n {
			v.warnings = append(v.warnings, usageStep{Window: w.table, Remaining: &pct, Resets: reset,
				Reason: fmt.Sprintf("the %s window has %d%% left, at or below [usage.%s] notify_remaining %d; it resets %s",
					w.says, left, w.table, n, localReset(reset))})
		}
		if p, set := cfg.UsageLevel(w.table, "pause"); set && left <= p {
			take(usageStep{Window: w.table, Remaining: &pct, Resets: reset,
				Reason: fmt.Sprintf("the %s window has %d%% left, at or below [usage.%s] pause_remaining %d; it resets %s",
					w.says, left, w.table, p, localReset(reset))})
		}
	}
	week := r.WindowReset(runlog.WindowSevenDay)
	if r.InOverage() && (week.IsZero() || now.Before(week)) {
		if _, set := cfg.UsageLevel(stepWeekly, "notify"); set {
			v.warnings = append(v.warnings, usageStep{Window: stepOverage, Resets: week,
				Reason: "the weekly allotment is spent and sessions now draw on purchased credit; the week resets " + localReset(week)})
		}
		if cfg.OverageMode() == config.OveragePause {
			take(usageStep{Window: stepOverage, Resets: week,
				Reason: "the weekly allotment is spent and sessions would draw on purchased credit, which [usage.overage] mode = \"pause\" holds; the week resets " + localReset(week)})
		} else {
			spent := dollarsCents(a.OverageUSD)
			if n, set := cfg.OverageUSD("notify"); set && a.OverageUSD >= n {
				v.warnings = append(v.warnings, usageStep{Window: stepOverageUSD, Resets: week,
					Reason: fmt.Sprintf("sessions on purchased credit this week reach %s at list price, at or above [usage.overage] notify_usd %s; that is not what the account was charged, and no reading says how much credit is left",
						spent, dollarsCents(n))})
			}
			if m, set := cfg.OverageUSD("max"); set && a.OverageUSD >= m {
				take(usageStep{Window: stepOverageUSD, Resets: week,
					Reason: fmt.Sprintf("sessions on purchased credit this week reach %s at list price, at or above [usage.overage] max_usd %s; the week resets %s",
						spent, dollarsCents(m), localReset(week))})
			}
		}
	}
	if r.Refused() && r.Current(now) {
		take(refusalStep(r, now))
	}
	return v
}

// refusalStep is the hold a reading the service refuses sessions under takes,
// whether or not a level was crossed on the way: the next launch would be
// turned away too.
func refusalStep(r *runlog.RateLimit, now time.Time) usageStep {
	table, says := stepWeekly, "weekly"
	if r.RateLimitType == runlog.WindowFiveHour {
		table, says = stepFiveHour, "five-hour"
	}
	reset := time.Unix(r.ResetsAt, 0)
	if r.ResetsAt == 0 {
		reset = r.WindowReset(r.RateLimitType)
	}
	why := "the " + says + " window is spent and the account has no purchased credit to carry it"
	if r.OverageDisabledReason == runlog.OutOfCredits {
		why = "the " + says + " window is spent and so is the purchased credit (out_of_credits)"
	}
	u := usageStep{Window: table, Resets: reset, Reason: why + ", so the service refuses sessions; it resets " + localReset(reset)}
	if left, ok := r.Remaining(r.RateLimitType, now); ok {
		u.Remaining = &left
	}
	return u
}

// dollarsCents is a list-price sum as a person reads it.
func dollarsCents(f float64) string { return fmt.Sprintf("$%.2f", f) }

// quotaRefused reports a session the service turned away for quota, as the
// session's own reading says: the one fault a usage hold answers instead of a
// park. A quota fault with no reading, codex's and agy's, is not one.
func quotaRefused(r runlog.Result) bool {
	return r.Fault == runlog.FaultQuota && r.RateLimit.Refused()
}

// usageGate is usage_admission's state across one drive: the hold in force, the
// instant it lifts on its own, and the warnings and holds already dealt with.
type usageGate struct {
	mu sync.Mutex
	// hold is the step the drive is paused on, nil while it is not.
	hold     *usageStep
	resumeAt time.Time
	// deadline is the drive's --for or --until, zero when it has none; a hold
	// lifts on its own only inside it.
	deadline time.Time
	// waived is every hold a drive --resume released, which admission does not
	// take again on the same window and reset: resuming is the operator
	// spending what is left on purpose.
	waived map[string]bool
	// sent is every warning this process sent or tried to, so a ledger that
	// will not take the row costs one attempt and not one per dispatch.
	sent map[string]bool
}

func newUsageGate(deadline time.Time) *usageGate {
	return &usageGate{deadline: deadline, waived: map[string]bool{}, sent: map[string]bool{}}
}

// holding is the step the drive is paused on, and when it lifts on its own.
func (g *usageGate) holding() (*usageStep, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hold, g.resumeAt
}

// release clears the hold, and waives it when an operator released it.
func (g *usageGate) release(waive bool) *usageStep {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.hold
	if h != nil && waive {
		g.waived[h.key()] = true
	}
	g.hold, g.resumeAt = nil, time.Time{}
	return h
}

// admitUsage is usage_admission before a dispatch or a stage that launches a
// session: it sends the warnings the newest reading crossed, and holds the
// drive on a level it crossed. It reports whether the drive is now held.
func (s *session) admitUsage(q *driveSched) bool {
	if q == nil || q.usage == nil {
		return false
	}
	a := s.readAccount()
	v := judgeUsage(s.cfg, a, time.Now())
	s.sendUsageWarnings(q.usage, v.warnings, a)
	p, _ := s.cfg.Policy("usage_admission")
	if p == polOff || v.hold == nil {
		return false
	}
	q.usage.mu.Lock()
	waived := q.usage.waived[v.hold.key()]
	q.usage.mu.Unlock()
	if waived {
		return false
	}
	return s.holdForUsage(q, *v.hold)
}

// sendUsageWarnings emits each warning no registered project already sent,
// writing its ledger row first: the row is what keeps a restarted drive and
// a drive in another project from sending it again.
func (s *session) sendUsageWarnings(g *usageGate, warnings []usageStep, a accountReading) {
	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	for _, u := range warnings {
		g.mu.Lock()
		sent := g.sent[u.key()]
		g.sent[u.key()] = true
		g.mu.Unlock()
		if sent || a.warned(u.Window, u.Resets.Unix()) {
			continue
		}
		row := driveStamp(ledger.Row{ID: ledger.UsageRowID, UsageWarning: u.Window, ResetsAt: u.Resets.Unix(), Note: u.Reason})
		wrote, err := ledger.AppendUnless(path, row, func(l *ledger.Ledger) bool { return l.Warned(u.Window, u.Resets.Unix()) })
		if err != nil {
			fmt.Fprintf(os.Stderr, "· usage_warning: the ledger row would not write, so the notice is not sent: %v\n", err)
			continue
		}
		if !wrote {
			continue
		}
		fmt.Fprintf(os.Stderr, "· usage_warning: %s\n", u.Reason)
		events.emit(u.event(DriveEvent{Event: evUsageWarning, Action: "usage_admission"}))
	}
}

// holdForUsage pauses the drive on one step through the state --pause writes,
// and reports whether the drive is held. Sessions already running finish their
// stage, since the work in flight is what the remaining percent was kept for,
// and each item holds at its next boundary. A drive an operator already
// paused is left to them: a hold taken on top of it would lift their pause at
// the reset.
func (s *session) holdForUsage(q *driveSched, u usageStep) bool {
	p, _ := s.cfg.Policy("usage_admission")
	g := q.usage
	g.mu.Lock()
	if g.hold != nil {
		g.mu.Unlock()
		return true
	}
	if q.isPaused() {
		g.mu.Unlock()
		return true
	}
	g.hold = &u
	auto := "it waits for drover drive --resume"
	if at := u.Resets.Add(quotaSlack); p == polAuto && !u.Resets.IsZero() && !g.deadline.IsZero() && !at.After(g.deadline) {
		g.resumeAt = at
		auto = "it resumes on its own at " + localReset(at) + ", or on drover drive --resume"
	}
	g.mu.Unlock()
	q.pause()
	if held != nil {
		_ = held.Update(func(l *lease.Lease) { l.Paused = true })
	}
	fmt.Fprintf(os.Stderr, "· usage_admission: %s; nothing more is dispatched, each running item finishes its stage and holds, and %s\n", u.Reason, auto)
	events.emit(u.event(DriveEvent{Event: evPaused, Action: "usage_admission", Policy: string(p)}))
	events.emit(u.event(DriveEvent{Event: evNeedsOperator, Action: "usage_admission", Policy: string(p), Run: "drover drive --resume"}))
	return true
}

// resumeFromUsage lifts a hold at its reset, under the inputs the drive
// already runs on. The notifier runs once more, since whoever was told the
// drive stopped is owed being told it started.
func (s *session) resumeFromUsage(q *driveSched) {
	u := q.usage.release(false)
	if u == nil {
		return
	}
	q.resume(s.inputs)
	if held != nil {
		_ = held.Update(func(l *lease.Lease) { l.Paused = false })
	}
	why := "the " + u.Window + " hold reached its reset at " + localReset(u.Resets) + ", so the drive dispatches again"
	fmt.Fprintf(os.Stderr, "· usage_admission: %s\n", why)
	ev := u.event(DriveEvent{Event: evResumed, Action: "usage_admission"})
	ev.Reason = why
	events.emit(ev)
}

// heldItem is what an item the deadline caught behind a usage hold amounts to.
func heldItem(it backlog.Item, u *usageStep) DriveItem {
	now := time.Now().Format(time.RFC3339)
	d := DriveItem{ID: it.ID.Bare(), Title: it.Title(), Outcome: outcomeHeld, Started: now, Ended: now}
	if u != nil {
		d.Reason = "the drive's deadline arrived while it was held: " + u.Reason
	}
	return d
}

// requeueAt is the boundary a quota-refused launch sends its item back to, so
// the next dispatch starts the stage that was refused and nothing before it.
func (s *session) requeueAt(it backlog.Item, stage string) *lease.Hold {
	return &lease.Hold{ID: it.ID.Bare(), At: stage, Verify: prompts.SelectVerify(s.cfg.Verify, it.Meta)}
}

// usagePlan is --dry-run's usage_admission row: whether it would hold now, and
// why.
func (s *session) usagePlan() string {
	p, _ := s.cfg.Policy("usage_admission")
	a := s.readAccount()
	if a.Reading == nil {
		return "no usage row in any registered project carries a rate_limit reading; the run dispatches"
	}
	now := time.Now()
	v := judgeUsage(s.cfg, a, now)
	left := readingLine(a.Reading, now)
	switch {
	case p == polOff:
		return left + "; not acted on"
	case v.hold != nil:
		return left + "; the run would hold now: " + v.hold.Reason
	}
	return left + " (" + a.Project + ", " + a.At.Local().Format("Jan 2 15:04") + "); the run dispatches"
}

// readingLine is a reading as one line: each window's percent left, overage,
// and the weekly reset in local time.
func readingLine(r *runlog.RateLimit, now time.Time) string {
	line := ""
	for _, w := range windowNames {
		if left, ok := r.Remaining(w.window, now); ok {
			if line != "" {
				line += ", "
			}
			line += fmt.Sprintf("%s %d%% left", w.says, left)
		}
	}
	if line == "" {
		line = "no window figures"
	}
	switch {
	case r.InOverage():
		line += ", on purchased credit"
	case r.OverageDisabledReason == runlog.OutOfCredits:
		line += ", purchased credit spent"
	default:
		line += ", not on purchased credit"
	}
	if week := r.WindowReset(runlog.WindowSevenDay); !week.IsZero() {
		line += ", week resets " + localReset(week)
	}
	return line
}

// StatusRateLimit is status --json's rate_limit object: the newest reading, the
// project and time of the usage row it came from, and the percent of each
// window left now, which is the reading's own figure until that window resets.
type StatusRateLimit struct {
	Reading   *runlog.RateLimit `json:"reading"`
	Project   string            `json:"project"`
	TS        string            `json:"ts"`
	Remaining map[string]int    `json:"remaining"`
	Overage   bool              `json:"overage"`
	// Unread names each registered project whose ledger would not read, which
	// the newest reading may then be older than.
	Unread []string `json:"unread,omitempty"`
}

// statusRateLimit is the rate_limit object for a, nil when no row carried a
// reading.
func statusRateLimit(a accountReading, now time.Time) *StatusRateLimit {
	if a.Reading == nil {
		return nil
	}
	out := &StatusRateLimit{Reading: a.Reading, Project: a.Project, TS: a.At.Format(time.RFC3339),
		Remaining: map[string]int{}, Overage: a.Reading.InOverage(), Unread: a.Unread}
	for _, w := range windowNames {
		if left, ok := a.Reading.Remaining(w.window, now); ok {
			out.Remaining[w.table] = left
		}
	}
	return out
}
