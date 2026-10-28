package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// stubProbe answers whatever the test last set, and nothing until it does.
type stubProbe struct {
	mu     sync.Mutex
	answer string
	reads  int
}

func (p *stubProbe) mark(*config.Config, DriveEvent) answerMark { return answerMark{} }

func (p *stubProbe) answered(*config.Config, DriveEvent, answerMark) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	return p.answer
}

func (p *stubProbe) set(a string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = a
}

// repeatFixture is a drive with an ntfy destination named phone repeating
// needs_operator at the keys repeat names, beside one named desk on the same
// server that takes it without repeating.
func repeatFixture(t *testing.T, repeat string) (*eventSink, *stubProbe, *[]fakeTimer, func() []ntfyRequest) {
	t.Helper()
	srv, seen := ntfyServer(t, http.StatusOK, "")
	table, _ := ntfyTable(t, srv.URL+"/phone", `{ needs_operator = "high" }`)
	table += repeat + "\n[notify.destinations.desk]\ntype = \"ntfy\"\nurl = \"" + srv.URL + "/desk\"\nevents = { needs_operator = \"default\" }\n"
	timers := fakeTimers(t)
	es := notifyFixture(t, table)
	probe := &stubProbe{}
	es.notifier.probe = probe
	return es, probe, timers, seen
}

func at(seen func() []ntfyRequest, path string) []ntfyRequest {
	var out []ntfyRequest
	for _, r := range seen() {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// fire runs the newest timer and waits for whatever it queued to send.
func fire(t *testing.T, es *eventSink, timers *[]fakeTimer) {
	t.Helper()
	if len(*timers) == 0 {
		t.Fatal("no timer is set to fire")
	}
	(*timers)[len(*timers)-1].f()
	es.notifier.idle()
}

// An unanswered needs_operator is resent every repeat, at the priority it was
// first sent at, with its place in the sequence in the title, until repeat_max
// reminders have gone out; then nothing more is sent.
func TestAReminderSequenceStopsAtRepeatMax(t *testing.T) {
	es, probe, timers, seen := repeatFixture(t, "repeat = \"5m\"\nrepeat_max = 3\n")
	es.needsOperator("1", "auth_expired", "", "claude login", 0)
	es.notifier.idle()
	if got := at(seen, "/phone"); len(got) != 1 || got[0].header.Get("Title") != "drover: example 1" {
		t.Fatalf("the first send was %+v, want one untitled by any reminder", got)
	}
	for range 5 {
		fire(t, es, timers)
	}
	got := at(seen, "/phone")
	if len(got) != 4 {
		t.Fatalf("%d sends to phone, want the first and 3 reminders", len(got))
	}
	for i, r := range got[1:] {
		want := "drover: example 1 (reminder " + string(rune('1'+i)) + " of 3)"
		if r.header.Get("Title") != want || r.header.Get("Priority") != "high" || r.body != got[0].body {
			t.Errorf("reminder %d went out as %q at %q:\n%s", i+1, r.header.Get("Title"), r.header.Get("Priority"), r.body)
		}
	}
	for _, tm := range *timers {
		if tm.d != 5*time.Minute {
			t.Errorf("a timer was set for %s, want every one at repeat", tm.d)
		}
	}
	if probe.reads != 5 {
		t.Errorf("the probe was read %d times, want once before each of the 5 timers", probe.reads)
	}
	if got := at(seen, "/desk"); len(got) != 1 {
		t.Errorf("desk, which sets no repeat, got %d sends, want the first alone", len(got))
	}
}

// A needs_operator answered after its second reminder gets no third: one
// answered message goes out at low, to phone, which reminded, and not to desk,
// which did not, and the sequence stops reading.
func TestAnAnsweredReminderStopsAndSaysSo(t *testing.T) {
	es, probe, timers, seen := repeatFixture(t, "repeat = \"10m\"\n")
	es.needsOperator("1", "write_scaffold_requirements", "", "", 0)
	fire(t, es, timers)
	fire(t, es, timers)
	if got := at(seen, "/phone"); len(got) != 3 || !strings.HasSuffix(got[2].header.Get("Title"), "(reminder 2 of 6)") {
		t.Fatalf("before the answer phone got %+v, want the first and reminders 1 and 2 of the default 6", got)
	}
	probe.set(answeredAccepted)
	fire(t, es, timers)
	got := at(seen, "/phone")
	if len(got) != 4 {
		t.Fatalf("%d sends to phone, want no third reminder and one answered", len(got))
	}
	if a := got[3]; a.body != "answered: accepted 1" || a.header.Get("Priority") != "low" || a.header.Get("Title") != "drover: example 1" {
		t.Errorf("the answer went out as %q at %q: %s", a.header.Get("Title"), a.header.Get("Priority"), a.body)
	}
	if n := len(*timers); n != 3 {
		t.Errorf("%d timers set, want none after the answer", n)
	}
	if got := at(seen, "/desk"); len(got) != 1 {
		t.Errorf("desk got %d sends, want the first alone and no answered message", len(got))
	}
}

// A drive that exits mid-sequence sends nothing more: the timer that comes due
// after the exit reads nothing and sends nothing.
func TestAnExitEndsTheReminders(t *testing.T) {
	es, probe, timers, seen := repeatFixture(t, "repeat = \"5m\"\n")
	es.needsOperator("1", "auth_expired", "", "claude login", 0)
	fire(t, es, timers)
	es.exit(nil)
	reads := probe.reads
	(*timers)[len(*timers)-1].f()
	if got := at(seen, "/phone"); len(got) != 2 {
		t.Errorf("%d sends to phone, want the first and one reminder before the exit", len(got))
	}
	if probe.reads != reads || len(*timers) != 2 {
		t.Errorf("after the exit the probe was read %d more times and %d timers set, want neither", probe.reads-reads, len(*timers)-2)
	}
}

// The exit is routed before the notifier closes, and a reminder coming due in
// between sends nothing: the exit message stays the last word.
func TestNoReminderFollowsTheExitMessage(t *testing.T) {
	es, probe, timers, seen := repeatFixture(t, "repeat = \"5m\"\n")
	es.needsOperator("1", "auth_expired", "", "claude login", 0)
	fire(t, es, timers)
	code := 0
	es.notifier.route(DriveEvent{Event: evExit, Drive: "D1", Code: &code, Summary: &ExitSummary{}})
	reads := probe.reads
	fire(t, es, timers)
	if got := at(seen, "/phone"); len(got) != 2 {
		t.Errorf("%d sends to phone, want the first and one reminder, the exit routed to nobody here", len(got))
	}
	if probe.reads != reads || len(*timers) != 2 {
		t.Errorf("between the exit and the close the probe was read %d more times and %d timers set, want neither", probe.reads-reads, len(*timers)-2)
	}
	es.notifier.close()
}

// The drive's own probe reads what the operator's commands write: a stage row
// is a rerun, a done row a merge, accept's body row on a scaffold an accept,
// the shelf a shelve, and drive --resume clearing the lease a resume. A row
// already in the ledger when the stop was raised answers nothing.
func TestTheLedgerProbeReadsTheOperatorsAnswer(t *testing.T) {
	today := time.Now().Format(time.DateOnly)
	ev := DriveEvent{Event: evNeedsOperator, ID: "1", Action: "auth_expired"}
	p := ledgerProbe{}

	s := fixture(t, "")
	if err := s.record(ledger.Row{ID: "1", Stage: "implement", Date: today}); err != nil {
		t.Fatal(err)
	}
	m := p.mark(s.cfg, ev)
	if got := p.answered(s.cfg, ev, m); got != "" {
		t.Errorf("with the stage row from before the stop the probe answered %q, want nothing", got)
	}
	if err := s.record(ledger.Row{ID: "1", Park: "auth_expired", Date: today}); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != "" {
		t.Errorf("a park row answered %q, want nothing", got)
	}
	if err := s.record(ledger.Row{ID: "1", Stage: "implement", Date: today}); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != answeredRerun {
		t.Errorf("a new stage row answered %q, want %q", got, answeredRerun)
	}

	s = fixture(t, "")
	m = p.mark(s.cfg, ev)
	merged := true
	if err := s.record(ledger.Row{ID: "1", Merged: &merged, Date: today}); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != answeredMerged {
		t.Errorf("a done row answered %q, want %q", got, answeredMerged)
	}

	s = fixture(t, "")
	body := filepath.Join(s.cfg.Root, "work", "01-first.md")
	write(t, body, "---\ntitle: First\ndeps:\n---\n\nSTOP: requirements not yet written\n\n1. Do it.\n")
	m = p.mark(s.cfg, ev)
	if !m.scaffold {
		t.Fatal("the scaffold was not marked as one")
	}
	if err := s.record(ledger.BodyRow("1", "aaaaaaaaaaaa", ledger.Author{Cmd: "accept", Actor: "operator"})); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != "" {
		t.Errorf("a body row on a body still a scaffold answered %q, want nothing", got)
	}
	write(t, body, "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n")
	if err := s.record(ledger.BodyRow("1", "bbbbbbbbbbbb", ledger.Author{Cmd: "accept", Actor: "operator"})); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != answeredAccepted {
		t.Errorf("accept's body row answered %q, want %q", got, answeredAccepted)
	}

	s = fixture(t, "")
	s.cfg.Paths.Shelf = "shelf"
	m = p.mark(s.cfg, ev)
	shelf := s.cfg.Abs(s.cfg.Paths.Shelf)
	if err := os.MkdirAll(shelf, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.cfg.Root, "work", "01-first.md"), filepath.Join(shelf, "01-first.md")); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, ev, m); got != answeredShelved {
		t.Errorf("a shelved body answered %q, want %q", got, answeredShelved)
	}

	h, err := lease.Acquire(filepath.Join(t.TempDir(), lease.FileName), lease.Lease{Command: "drive", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	held = h
	t.Cleanup(func() { held = nil; _ = h.Release() })
	_ = h.Update(func(l *lease.Lease) { l.Paused = true })
	usage := DriveEvent{Event: evNeedsOperator, Action: "usage_admission"}
	m = p.mark(s.cfg, usage)
	if got := p.answered(s.cfg, usage, m); got != "" {
		t.Errorf("a drive still paused answered %q, want nothing", got)
	}
	_ = h.Update(func(l *lease.Lease) { l.Paused = false })
	if got := p.answered(s.cfg, usage, m); got != answeredResumed {
		t.Errorf("a resumed drive answered %q, want %q", got, answeredResumed)
	}

	s = fixture(t, "")
	tri := DriveEvent{Event: evNeedsOperator, ID: "1", Action: "triage_finding", Ref: "42"}
	m = p.mark(s.cfg, tri)
	if err := s.record(ledger.Row{ID: "1", Stage: "review", Date: today}); err != nil {
		t.Fatal(err)
	}
	if err := s.record(ledger.Row{ID: "1", Merged: &merged, Date: today}); err != nil {
		t.Fatal(err)
	}
	if got := p.answered(s.cfg, tri, m); got != "" {
		t.Errorf("a single-filer triage after its filer's stage and done rows answered %q, want nothing", got)
	}

	_ = h.Update(func(l *lease.Lease) { l.Paused = true })
	multi := DriveEvent{Event: evNeedsOperator, Action: "triage_finding", Ref: "43"}
	m = p.mark(s.cfg, multi)
	mt := p.mark(s.cfg, tri)
	_ = h.Update(func(l *lease.Lease) { l.Paused = false })
	if got := p.answered(s.cfg, multi, m); got != "" {
		t.Errorf("a no-id triage raised during a pause answered %q after the resume, want nothing", got)
	}
	if got := p.answered(s.cfg, tri, mt); got != "" {
		t.Errorf("a single-filer triage raised during a pause answered %q after the resume, want nothing", got)
	}
}

// repeat and repeat_max are held to what a drive can act on.
func TestRepeatKeysAreValidated(t *testing.T) {
	base := "\n[notify.destinations.box]\ntype = \"command\"\nargv = [\"true\"]\nevents = { needs_operator = \"high\" }\n"
	for _, c := range []struct{ keys, want string }{
		{"repeat = \"1m\"\n", "repeat is 1m: give a duration of at least 5m0s"},
		{"repeat = \"often\"\n", "repeat is often"},
		{"repeat = \"5m\"\nrepeat_max = 0\n", "repeat_max is 0: give a positive whole number"},
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "go.mod"), "module x\n")
		write(t, filepath.Join(dir, config.FileName), schemaLine+"project = \"x\"\n"+base+c.keys)
		_, err := config.Load(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: Load said %v, want %q", c.keys, err, c.want)
		}
	}
	s := fixture(t, base+"repeat = \"15m\"\n")
	d := s.cfg.Notifications().Destinations[0]
	if d.Repeat != 15*time.Minute || d.RepeatMax != config.DefaultRepeatMax {
		t.Errorf("resolved repeat %s × %d, want 15m × %d", d.Repeat, d.RepeatMax, config.DefaultRepeatMax)
	}
}
