package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
)

const ntfySecret = "tk_s3cret-ntfy-token"

// ntfyRequest is one POST the stub server saw.
type ntfyRequest struct {
	method, path, body string
	header             http.Header
}

// ntfyServer answers every POST with status and body, recording each request.
func ntfyServer(t *testing.T, status int, body string) (*httptest.Server, func() []ntfyRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []ntfyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, ntfyRequest{method: r.Method, path: r.URL.Path, body: string(b), header: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []ntfyRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]ntfyRequest(nil), seen...)
	}
}

// ntfyTable is an ntfy destination named phone posting to url with a token
// file only its owner reads, routing events.
func ntfyTable(t *testing.T, url, events string) (string, string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "ntfy-token")
	write(t, token, ntfySecret+"\n")
	return "\n[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"" + url + "\"\ntoken_file = \"" + token + "\"\nevents = " + events + "\n", token
}

// notifyFixture is a project carrying extra under its config, and the sink a
// drive under id D7 would hold.
func notifyFixture(t *testing.T, extra string) *eventSink {
	t.Helper()
	ownState(t)
	s := fixture(t, extra)
	t.Chdir(s.cfg.Root)
	prev := activeDrive.Swap(&runningDrive{id: "D7"})
	t.Cleanup(func() { activeDrive.Store(prev) })
	es := newEventSink(s.cfg, false)
	t.Cleanup(es.notifier.close)
	return es
}

// ntfyFixture is notifyFixture with one ntfy destination.
func ntfyFixture(t *testing.T, url, events string) (*eventSink, string) {
	t.Helper()
	table, token := ntfyTable(t, url, events)
	return notifyFixture(t, table), token
}

// clockAt pins the send clock, and records the deadline each request carried.
func clockAt(t *testing.T, at time.Time) func() []time.Time {
	t.Helper()
	clock, transport := ntfyClock, ntfyTransport
	t.Cleanup(func() { ntfyClock, ntfyTransport = clock, transport })
	ntfyClock = func() time.Time { return at }
	var mu sync.Mutex
	var deadlines []time.Time
	ntfyTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		d, _ := r.Context().Deadline()
		mu.Lock()
		deadlines = append(deadlines, d)
		mu.Unlock()
		return http.DefaultTransport.RoundTrip(r)
	})
	return func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(deadlines)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The drive's event path posts what a destination routes and nothing else,
// each at its own priority, with the headers a phone renders, bounded at ten
// seconds from the clock, and the token reaches the server and no stream
// drover writes.
func TestNtfySendsTheEventsItRoutesAtTheirPriorities(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, `{"id":"x"}`)
	es, _ := ntfyFixture(t, srv.URL+"/drover", `{ needs_operator = "urgent", merged = "low", exit = "default" }`)
	at := time.Now().Add(time.Hour)
	deadlines := clockAt(t, at)

	var stdout string
	stderr := captureErr(t, func() {
		stdout = capture(t, func() {
			es.w = os.Stdout
			es.stage("1", stageGate)
			es.emit(DriveEvent{Event: evParked, ID: "1", Reason: "parked for a reason"})
			es.needsOperator("1", "auth_expired", "the runner's credentials expired", "claude login, then drover drive 1")
			es.notifier.idle()
			es.emit(DriveEvent{Event: evMerged, ID: "1", PR: 4, Title: "1: the first item"})
			es.notifier.idle()
			es.exit(nil)
		})
	})

	got := seen()
	if len(got) != 3 {
		t.Fatalf("%d sends, want 3 (needs_operator, merged and exit; stage and parked are not routed): %+v", len(got), got)
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != "/drover" {
		t.Errorf("sent %s %s, want POST /drover", r.method, r.path)
	}
	for header, want := range map[string]string{
		"Title":         "drover: example 1",
		"Priority":      "urgent",
		"Tags":          "robot",
		"Authorization": "Bearer " + ntfySecret,
	} {
		if v := r.header.Get(header); v != want {
			t.Errorf("%s is %q, want %q", header, v, want)
		}
	}
	if want := "auth_expired: claude login, then drover drive 1"; r.body != want {
		t.Errorf("body is %q, want %q", r.body, want)
	}
	if m := got[1]; m.header.Get("Priority") != "low" || m.body != "merged #4: 1: the first item" {
		t.Errorf("merged went out at %q as %q", m.header.Get("Priority"), m.body)
	}
	if exit := got[2]; exit.header.Get("Title") != "drover: example" || exit.header.Get("Priority") != "default" ||
		!strings.HasPrefix(exit.body, "D7 exited 0: the queue drained") {
		t.Errorf("exit sent title %q priority %q body %q", exit.header.Get("Title"), exit.header.Get("Priority"), exit.body)
	}
	ds := deadlines()
	for i, d := range ds {
		if !d.Equal(at.Add(10 * time.Second)) {
			t.Errorf("send %d carried deadline %v, want the clock plus 10s, %v", i, d, at.Add(10*time.Second))
		}
	}
	if len(ds) != 3 {
		t.Errorf("%d sends went through the transport, want 3", len(ds))
	}

	file, err := os.ReadFile(fixtureEvents(t, "D7"))
	if err != nil {
		t.Fatal(err)
	}
	for name, stream := range map[string]string{"stdout": stdout, "stderr": stderr, "event file": string(file)} {
		if strings.Contains(stream, ntfySecret) {
			t.Errorf("the token reached %s:\n%s", name, stream)
		}
	}
	if stderr != "" {
		t.Errorf("a clean send reported something: %s", stderr)
	}
}

// A command destination reads the event as one JSON line with the priority it
// was routed at and the body an ntfy destination would send.
func TestACommandDestinationReadsThePriority(t *testing.T) {
	sent := filepath.Join(t.TempDir(), "sent")
	es := notifyFixture(t, "\n[notify.destinations.desk]\ntype = \"command\"\nargv = [\"sh\", \"-c\", \"cat >> "+sent+"\"]\n"+
		"events = { parked = \"high\", usage_warning = \"low\" }\n")
	es.emit(DriveEvent{Event: evParked, ID: "3", Park: "gate_refused", Reason: "lint failed"})
	es.emit(DriveEvent{Event: evMerged, ID: "3"})
	es.notifier.close()
	raw, err := os.ReadFile(sent) // #nosec G304 -- written by the command above
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d lines on stdin, want 1 (merged is not routed):\n%s", len(lines), raw)
	}
	var p NotifyPayload
	if err := json.Unmarshal([]byte(lines[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != evParked || p.ID != "3" || p.Priority != "high" || p.Message != "parked gate_refused: lint failed" || p.Project != "example" {
		t.Errorf("the command read %+v", p)
	}
}

// Two destinations do not wait on each other: one whose server never answers
// holds nothing up at the other, and each failure is reported once for the
// destination it belongs to, carrying the status and what ntfy said, with the
// token cut from a server that echoed it.
func TestDestinationsFailAndWaitAlone(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { unblock(); hung.Close() })
	bad, badSeen := ntfyServer(t, http.StatusForbidden, `{"error":"forbidden, token `+ntfySecret+` rejected"}`)
	ok, okSeen := ntfyServer(t, http.StatusOK, "")
	table, token := ntfyTable(t, bad.URL+"/drover", `{ needs_operator = "high" }`)
	extra := table +
		"\n[notify.destinations.slow]\ntype = \"ntfy\"\nurl = \"" + hung.URL + "/drover\"\nevents = { needs_operator = \"high\" }\n" +
		"\n[notify.destinations.good]\ntype = \"ntfy\"\nurl = \"" + ok.URL + "/drover\"\ntoken_file = \"" + token + "\"\nevents = { needs_operator = \"high\" }\n"
	es := notifyFixture(t, extra)

	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("2", "auth_expired", "", "claude login")
		deadline := time.Now().Add(5 * time.Second)
		for len(okSeen()) < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := len(okSeen()); n != 2 {
			t.Errorf("the good destination got %d sends while another hung, want 2", n)
		}
		unblock()
		es.notifier.idle()
	})

	if n := len(badSeen()); n != 2 {
		t.Errorf("%d sends to the failing destination, want 2: a failure must not stop the next one", n)
	}
	if n := strings.Count(stderr, "[notify.destinations.phone]"); n != 1 {
		t.Fatalf("%d reports across two failed sends, want 1:\n%s", n, stderr)
	}
	for _, want := range []string{"403 Forbidden", "forbidden, token [token] rejected", "the drive goes on"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the report lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, ntfySecret) || strings.Contains(stderr, "[notify.destinations.good]") {
		t.Errorf("the token or the good destination reached stderr:\n%s", stderr)
	}
}

// A server that never answers is cut at the deadline the clock sets, so a
// clock moved ten seconds back fails the send at once without the test waiting
// for it.
func TestNtfyHungServerIsCutAtTheBound(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	es, _ := ntfyFixture(t, srv.URL+"/drover", `{ needs_operator = "high" }`)
	clockAt(t, time.Now().Add(-10*time.Second))

	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.notifier.idle()
	})
	if n := strings.Count(stderr, "[notify.destinations.phone]"); n != 1 || !strings.Contains(stderr, "deadline exceeded") {
		t.Errorf("want one report of the deadline, got:\n%s", stderr)
	}
}

// A token file others can read draws one warning naming the fix, and the send
// goes out anyway.
func TestNtfyLooseTokenFileWarnsOnce(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	es, token := ntfyFixture(t, srv.URL+"/drover", `{ needs_operator = "high" }`)
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.notifier.idle()
	})
	if n := strings.Count(stderr, "chmod 600 "+token); n != 1 {
		t.Errorf("%d warnings naming chmod 600, want 1:\n%s", n, stderr)
	}
	if n := len(seen()); n != 2 {
		t.Errorf("%d sends, want 2: a loose token file warns and still sends", n)
	}
}

// A pull request drover recorded is what a tap on the message opens.
func TestNtfyClickOpensThePullRequest(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	es, _ := ntfyFixture(t, srv.URL+"/drover", `{ merged = "default", stage = "low" }`)
	asked := 0
	es.notifier.forge = func() string { asked++; return "https://github.com/o/r" }
	es.emit(DriveEvent{Event: evMerged, ID: "2", PR: 17})
	es.emit(DriveEvent{Event: evStage, ID: "2", Stage: stageGate})
	es.emit(DriveEvent{Event: evMerged, ID: "3", PR: 18})
	es.notifier.idle()
	got := seen()
	if len(got) != 3 {
		t.Fatalf("%d sends, want 3", len(got))
	}
	for i, want := range []string{"https://github.com/o/r/pull/17", "", "https://github.com/o/r/pull/18"} {
		if c := got[i].header.Get("Click"); c != want {
			t.Errorf("send %d Click is %q, want %q", i, c, want)
		}
	}
	if asked != 1 {
		t.Errorf("the repository was resolved %d times, want once per drive", asked)
	}
}

// Every event kind has a body that says what happened, or what to do.
func TestEveryEventKindHasABody(t *testing.T) {
	two := 12
	code := 0
	for _, c := range []struct {
		e    DriveEvent
		want string
	}{
		{DriveEvent{Event: evStarted, Drive: "D10", Queue: []string{"B89", "E71", "F14"}, Jobs: 2, Deadline: "2026-10-24T21:00:00Z"},
			"started D10: B89 E71 F14, 2 at once, until " + localTime("2026-10-24T21:00:00Z")},
		{DriveEvent{Event: evStarted, Drive: "D10", Queue: []string{"B89"}, Jobs: 1}, "started D10: B89, no deadline"},
		{DriveEvent{Event: evQueued, ID: "4", Position: 2}, "queued at position 2"},
		{DriveEvent{Event: evDispatched, ID: "4", Slot: 2}, "dispatched to slot 2"},
		{DriveEvent{Event: evPreflight, Action: "deps_lint", Reason: "clean"}, "deps_lint: clean"},
		{DriveEvent{Event: evStage, ID: "4", Stage: stageGate}, "gate"},
		{DriveEvent{Event: evCheck, ID: "4", Check: "lint", Class: "code", Evidence: "x.go:3: unused\nmore"}, "check lint code: x.go:3: unused"},
		{DriveEvent{Event: evParked, ID: "F14", Park: "gate_refused", Detail: "code", Reason: "revive failed: notify.go:28",
			Evidence: "l1\nl2\nl3\nl4\nl5\nl6\nl7"},
			"parked gate_refused (code): revive failed: notify.go:28\nl1\nl2\nl3\nl4\nl5"},
		{DriveEvent{Event: evFiled, Kind: "register", Ref: "91"}, "filed register 91"},
		{DriveEvent{Event: evNeedsOperator, Action: "merge_protected_paths", Run: "read the diff, then gh pr merge 4"},
			"merge_protected_paths: read the diff, then gh pr merge 4"},
		{DriveEvent{Event: evNeedsSupervisor, Action: "resolve_conflict", Run: "resolve it in the tree"},
			"resolve_conflict: resolve it in the tree"},
		{DriveEvent{Event: evMerged, ID: "B89", PR: 12, Title: "B89: the title"}, "merged #12: B89: the title"},
		{DriveEvent{Event: evPaused}, "paused"},
		{DriveEvent{Event: evPaused, Reason: "the weekly window is at 4%"}, "paused: the weekly window is at 4%"},
		{DriveEvent{Event: evResumed, Changed: []string{"config"}}, "resumed, reloaded config"},
		{DriveEvent{Event: evUsageWarning, Window: "weekly", Remaining: &two, ResetsAt: "2026-10-25T20:00:00-07:00"},
			"weekly: 12% left, resets " + localTime("2026-10-25T20:00:00-07:00")},
		{DriveEvent{Event: evExit, Drive: "D3", Code: &code, Summary: &ExitSummary{Merged: 2, Ended: endedDeadline}},
			"D3 exited 0: the deadline arrived\n2 merged · 0 parked · 0 skipped · 0 held"},
	} {
		if got := noticeBody(c.e); got != c.want {
			t.Errorf("%s body is\n%q\nwant\n%q", c.e.Event, got, c.want)
		}
	}
}

// Every event a drive emits can be routed: a new event that config does not
// accept would be one no destination can ever hear.
func TestEveryDriveEventIsRoutable(t *testing.T) {
	for _, e := range append(slices.Clone(driveEventNames), evNeedsSupervisor) {
		if !slices.Contains(config.NotifyEvents(), e) {
			t.Errorf("%s is a drive event no [notify] destination may route", e)
		}
	}
}

// D10: one merge, one park, one dependent skipped behind it, exit 0. The exit
// says what was left undone and why, and goes out at exit_undone's priority,
// high by default, rather than exit's.
func TestAnExitThatLeftWorkUndoneSaysSo(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	es, _ := ntfyFixture(t, srv.URL+"/drover", `{ exit = "low" }`)
	s := &session{cfg: es.cfg}
	done := []DriveItem{
		{ID: "B89", Outcome: outcomeMergedByGate, Merged: true},
		{ID: "F14", Outcome: outcomeParked, Park: "gate_refused", Detail: "code", Reason: "revive failed: internal/cmd/notify.go:28: var-declaration; sent back"},
		{ID: "E72", Outcome: outcomeSkipped, Park: stateBlocked, Reason: "blocked by F14", blockedBy: []string{"F14"}},
	}
	es.setSummary(s.exitSummary(done, endedDrained))
	es.exit(nil)
	got := seen()
	if len(got) != 1 {
		t.Fatalf("%d sends, want 1", len(got))
	}
	want := "D7 exited 0: the queue drained\n" +
		"1 merged · 1 parked · 1 skipped · 0 held\n" +
		"F14 parked gate_refused (code): revive failed: internal/cmd/notify.go:28: var-declaration; sent back\n" +
		"E72 skipped: blocked by F14 (parked gate_refused)"
	if got[0].body != want {
		t.Errorf("exit body is\n%s\nwant\n%s", got[0].body, want)
	}
	if p := got[0].header.Get("Priority"); p != "high" {
		t.Errorf("an exit leaving work undone went out at %q, want exit_undone's default, high", p)
	}

	// The same drive with every item merged goes out at exit's own priority.
	srv2, seen2 := ntfyServer(t, http.StatusOK, "")
	es2, _ := ntfyFixture(t, srv2.URL+"/drover", `{ exit = "low", exit_undone = "urgent" }`)
	es2.setSummary((&session{cfg: es2.cfg}).exitSummary(done[:1], endedDrained))
	es2.exit(nil)
	if g := seen2(); len(g) != 1 || g[0].header.Get("Priority") != "low" {
		t.Errorf("a clean exit went out as %+v, want one send at low", g)
	}
}

// An exit body listing more items than fit in ntfy's limit lists what fits and
// counts the rest.
func TestAnExitBodyFitsNtfysLimit(t *testing.T) {
	code := 0
	s := &ExitSummary{Ended: endedDrained}
	for i := range 200 {
		s.Parked++
		s.Undone = append(s.Undone, UndoneItem{ID: "B" + strings.Repeat("9", 3) + string(rune('a'+i%26)), Outcome: outcomeParked,
			Park: "gate_refused", Reason: strings.Repeat("a long reason ", 4)})
	}
	body := exitBody(DriveEvent{Event: evExit, Drive: "D1", Code: &code, Summary: s}, nil)
	if len(body) > ntfyLimit {
		t.Errorf("the body is %d bytes, over ntfy's %d", len(body), ntfyLimit)
	}
	last := body[strings.LastIndex(body, "\n")+1:]
	if !strings.HasPrefix(last, "and ") || !strings.HasSuffix(last, " more") {
		t.Errorf("the body does not end on the count of what it left out: %q", last)
	}
}

// Events of one kind within the window go out as one message listing their
// ids; needs_operator never waits for the window.
func TestCoalescingFoldsEventsOfAKind(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	table, _ := ntfyTable(t, srv.URL+"/drover", `{ filed = "default", needs_operator = "high" }`)
	timers := fakeTimers(t)
	es := notifyFixture(t, "\n[notify]\ncoalesce = \"2m\"\n"+table)

	es.emit(DriveEvent{Event: evFiled, ID: "B91", Kind: "register", Ref: "40"})
	es.emit(DriveEvent{Event: evFiled, ID: "B91", Kind: "register", Ref: "41"})
	es.needsOperator("B92", "triage_finding", "", "drover follow-ups")
	es.emit(DriveEvent{Event: evFiled, ID: "B92", Kind: "register", Ref: "42"})
	es.notifier.idle()
	if got := seen(); len(got) != 1 || !strings.HasPrefix(got[0].body, "triage_finding") {
		t.Fatalf("before the window closed sent %+v, want needs_operator alone", got)
	}
	if len(*timers) != 1 {
		t.Fatalf("%d timers set, want one for the filed window", len(*timers))
	}
	(*timers)[0].f()
	es.notifier.idle()
	got := seen()
	if len(got) != 2 || got[1].body != "filed: B91 B91 B92" || got[1].header.Get("Title") != "drover: example" {
		t.Errorf("the window closed and sent %+v, want one filed: B91 B91 B92", got)
	}
}

type fakeTimer struct {
	d time.Duration
	f func()
}

// fakeTimers records each timer routing sets, for the test to fire.
func fakeTimers(t *testing.T) *[]fakeTimer {
	t.Helper()
	prev := notifyAfter
	var mu sync.Mutex
	var timers []fakeTimer
	notifyAfter = func(d time.Duration, f func()) {
		mu.Lock()
		defer mu.Unlock()
		timers = append(timers, fakeTimer{d, f})
	}
	t.Cleanup(func() { notifyAfter = prev })
	return &timers
}

// quietClock stands the routing clock at hh:mm today, movable by the test.
func quietClock(t *testing.T, hh, mm int) *time.Time {
	t.Helper()
	prev := notifyClock
	now := time.Now()
	at := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, time.Local)
	notifyClock = func() time.Time { return at }
	t.Cleanup(func() { notifyClock = prev })
	return &at
}

// Quiet hours hold what is routed below high until the window ends, then send
// it as one message; high and needs_operator go out at once.
func TestQuietHoursHoldAndRelease(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	table, _ := ntfyTable(t, srv.URL+"/drover", `{ merged = "default", parked = "high", stage = "low", needs_operator = "low" }`)
	timers := fakeTimers(t)
	clock := quietClock(t, 23, 30)
	es := notifyFixture(t, "\n[notify]\nquiet_hours = \"22:00-07:00\"\n"+table)

	es.emit(DriveEvent{Event: evMerged, ID: "1", PR: 3, Title: "1: one"})
	es.emit(DriveEvent{Event: evStage, ID: "2", Stage: stageGate})
	es.emit(DriveEvent{Event: evParked, ID: "2", Park: "gate_refused"})
	es.needsOperator("2", "auth_expired", "", "claude login")
	es.notifier.idle()
	got := seen()
	if len(got) != 2 || got[0].header.Get("Priority") != "high" || got[1].header.Get("Priority") != "low" {
		t.Fatalf("inside quiet hours sent %+v, want the high park and the needs_operator alone", got)
	}
	if len(*timers) != 1 || (*timers)[0].d != 7*time.Hour+30*time.Minute {
		t.Fatalf("timers %+v, want one release at 07:00", *timers)
	}

	*clock = clock.Add(8 * time.Hour)
	(*timers)[0].f()
	es.notifier.idle()
	got = seen()
	if len(got) != 3 {
		t.Fatalf("after the window sent %d messages, want the held two as one", len(got))
	}
	held := got[2]
	if held.header.Get("Priority") != "default" || !strings.Contains(held.body, "held during quiet hours (2):") ||
		!strings.Contains(held.body, "1 merged #3: 1: one") || !strings.Contains(held.body, "2 gate") {
		t.Errorf("the release went out at %q as\n%s", held.header.Get("Priority"), held.body)
	}
}

// A drive that exits inside quiet hours folds what it held into its exit and
// sends it at min, which ntfy delivers without sound, unless exit is routed at
// high or above.
func TestAnExitInQuietHoursFoldsWhatWasHeld(t *testing.T) {
	for _, c := range []struct {
		events, want string
	}{
		{`{ merged = "default", exit = "default" }`, "min"},
		{`{ merged = "default", exit = "urgent" }`, "urgent"},
	} {
		srv, seen := ntfyServer(t, http.StatusOK, "")
		table, _ := ntfyTable(t, srv.URL+"/drover", c.events)
		fakeTimers(t)
		quietClock(t, 2, 0)
		es := notifyFixture(t, "\n[notify]\nquiet_hours = \"22:00-07:00\"\n"+table)
		es.emit(DriveEvent{Event: evMerged, ID: "1", PR: 3, Title: "1: one"})
		es.exit(nil)
		got := seen()
		if len(got) != 1 {
			t.Fatalf("%s: %d sends, want the exit alone", c.events, len(got))
		}
		if p := got[0].header.Get("Priority"); p != c.want {
			t.Errorf("%s: the exit went out at %q, want %q", c.events, p, c.want)
		}
		if !strings.HasPrefix(got[0].body, "D7 exited 0") || !strings.Contains(got[0].body, "held during quiet hours (1):\n1 merged #3: 1: one") {
			t.Errorf("%s: the exit does not carry what was held:\n%s", c.events, got[0].body)
		}
	}
}

// notify --test reaches every destination, says what each answered, and exits
// 0, 1 or 2 as they did; --event sends a sample of that kind at each
// destination's routed priority, to those that route it.
func TestNotifyTestExitsByWhatTheDestinationsDid(t *testing.T) {
	t.Cleanup(func() { notifyTest, notifyEvent = false, "" })
	ok, seen := ntfyServer(t, http.StatusOK, "")
	es, _ := ntfyFixture(t, ok.URL+"/drover", `{ exit = "low" }`)
	root := es.cfg.Root

	var err error
	out := capture(t, func() { err = execute(t, "notify", "--test") })
	if err != nil || !strings.Contains(out, "HTTP 200") {
		t.Fatalf("notify --test against a server answering 200: %v\n%s", err, out)
	}
	if got := seen(); len(got) != 1 || got[0].header.Get("Title") != "drover: example TEST" ||
		got[0].body != "test: notification test from drover, nothing to do" || got[0].header.Get("Priority") != "high" {
		t.Errorf("the test event went out as %+v", got)
	}

	out = capture(t, func() { err = execute(t, "notify", "--test", "--event", "exit_undone") })
	if err != nil || !strings.Contains(out, "at high: HTTP 200") {
		t.Fatalf("notify --test --event exit_undone: %v\n%s", err, out)
	}
	if got := seen(); len(got) != 2 || !strings.Contains(got[1].body, "TEST parked gate_refused") {
		t.Errorf("the exit_undone sample went out as %+v", got[len(got)-1])
	}

	bin := t.TempDir()
	stub(t, filepath.Join(bin, "notify"), "#!/bin/sh\ncat >/dev/null\necho 'notifier broke'\nexit 3\n")
	conf := filepath.Join(root, ".drover.toml")
	data, rerr := os.ReadFile(conf)
	if rerr != nil {
		t.Fatal(rerr)
	}
	notifyEvent = ""
	write(t, conf, string(data)+"\n[notify.destinations.desk]\ntype = \"command\"\nargv = [\""+filepath.Join(bin, "notify")+"\"]\nevents = { needs_operator = \"default\" }\n")
	out = capture(t, func() { err = execute(t, "notify", "--test") })
	if Code(err) != 1 || !strings.Contains(out, "exit 3: notifier broke") || !strings.Contains(out, "HTTP 200") {
		t.Errorf("a failing command beside a working ntfy: exit %d\n%s", Code(err), out)
	}
	out = capture(t, func() { err = execute(t, "notify", "--test", "--event", "merged") })
	if Code(err) != 2 || !strings.Contains(out, "routes no merged") {
		t.Errorf("--event nobody routes exits %d, want 2:\n%s", Code(err), out)
	}

	notifyEvent = ""
	write(t, conf, string(data[:strings.Index(string(data), "[notify.destinations.phone]")]))
	out = capture(t, func() { err = execute(t, "notify", "--test") })
	if Code(err) != 2 {
		t.Errorf("no destination configured exits %d, want 2:\n%s", Code(err), out)
	}
}
