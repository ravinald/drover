package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// ntfyFixture is a project whose [supervise.ntfy] posts to url with a token
// file only its owner reads, and the sink a drive under id D7 would hold.
func ntfyFixture(t *testing.T, url, events string) *eventSink {
	t.Helper()
	ownState(t)
	token := filepath.Join(t.TempDir(), "ntfy-token")
	write(t, token, ntfySecret+"\n")
	s := fixture(t, "\n[supervise.ntfy]\nurl = \""+url+"\"\ntoken_file = \""+token+"\"\nevents = "+events+"\npriority = \"urgent\"\n")
	t.Chdir(s.cfg.Root)
	prev := activeDrive.Swap(&runningDrive{id: "D7"})
	t.Cleanup(func() { activeDrive.Store(prev) })
	return newEventSink(s.cfg, true)
}

// clockAt pins the send clock, and records the deadline each request carried.
func clockAt(t *testing.T, at time.Time) *[]time.Time {
	t.Helper()
	clock, transport := ntfyClock, ntfyTransport
	t.Cleanup(func() { ntfyClock, ntfyTransport = clock, transport })
	ntfyClock = func() time.Time { return at }
	var deadlines []time.Time
	ntfyTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		d, _ := r.Context().Deadline()
		deadlines = append(deadlines, d)
		return http.DefaultTransport.RoundTrip(r)
	})
	return &deadlines
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The drive's event path posts what [supervise.ntfy] names and nothing else,
// with the headers a phone renders, bounded at ten seconds from the clock, and
// the token reaches the server and no stream drover writes.
func TestNtfySendsTheEventsItNames(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, `{"id":"x"}`)
	es := ntfyFixture(t, srv.URL+"/drover", `["needs_operator", "exit"]`)
	at := time.Now().Add(time.Hour)
	deadlines := clockAt(t, at)

	var stdout string
	stderr := captureErr(t, func() {
		stdout = capture(t, func() {
			es.w = os.Stdout
			es.stage("1", stageGate)
			es.emit(DriveEvent{Event: evParked, ID: "1", Reason: "parked for a reason"})
			es.needsOperator("1", "auth_expired", "the runner's credentials expired", "claude login, then drover drive 1")
			es.emit(DriveEvent{Event: evMerged, ID: "1", PR: 4})
			es.exit(nil)
		})
	})

	got := seen()
	if len(got) != 2 {
		t.Fatalf("%d sends, want 2 (needs_operator and exit; stage, parked and merged are not named): %+v", len(got), got)
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
	if exit := got[1]; exit.header.Get("Title") != "drover: example" || exit.body != "exit" {
		t.Errorf("exit sent title %q body %q, want the project alone and the event name", exit.header.Get("Title"), exit.body)
	}
	for i, d := range *deadlines {
		if !d.Equal(at.Add(10 * time.Second)) {
			t.Errorf("send %d carried deadline %v, want the clock plus 10s, %v", i, d, at.Add(10*time.Second))
		}
	}
	if len(*deadlines) != 2 {
		t.Errorf("%d sends went through the transport, want 2", len(*deadlines))
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
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if !strings.Contains(line, `"project":"example"`) {
			t.Errorf("event carries no project: %s", line)
		}
	}
	if stderr != "" {
		t.Errorf("a clean send reported something: %s", stderr)
	}
}

// Two sends that fail cost one stderr line between them, carrying the status
// and what ntfy said, with the token cut from a server that echoed it.
func TestNtfyFailureIsReportedOncePerDrive(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusForbidden, `{"error":"forbidden, token `+ntfySecret+` rejected"}`)
	es := ntfyFixture(t, srv.URL+"/drover", `["needs_operator"]`)

	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("2", "auth_expired", "", "claude login")
	})

	if n := len(seen()); n != 2 {
		t.Errorf("%d sends, want 2: a failure must not stop the next one", n)
	}
	if n := strings.Count(stderr, "[supervise.ntfy]"); n != 1 {
		t.Fatalf("%d reports across two failed sends, want 1:\n%s", n, stderr)
	}
	for _, want := range []string{"403 Forbidden", "forbidden, token [token] rejected", "the drive goes on"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the report lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, ntfySecret) {
		t.Errorf("the token reached stderr:\n%s", stderr)
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
	es := ntfyFixture(t, srv.URL+"/drover", `["needs_operator"]`)
	clockAt(t, time.Now().Add(-10*time.Second))

	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("1", "auth_expired", "", "claude login")
	})
	if n := strings.Count(stderr, "[supervise.ntfy]"); n != 1 || !strings.Contains(stderr, "deadline exceeded") {
		t.Errorf("want one report of the deadline, got:\n%s", stderr)
	}
}

// A token file others can read draws one warning naming the fix, and the send
// goes out anyway.
func TestNtfyLooseTokenFileWarnsOnce(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	es := ntfyFixture(t, srv.URL+"/drover", `["needs_operator"]`)
	if err := os.Chmod(es.ntfy.TokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	stderr := captureErr(t, func() {
		es.needsOperator("1", "auth_expired", "", "claude login")
		es.needsOperator("1", "auth_expired", "", "claude login")
	})
	if n := strings.Count(stderr, "chmod 600 "+es.ntfy.TokenFile); n != 1 {
		t.Errorf("%d warnings naming chmod 600, want 1:\n%s", n, stderr)
	}
	if n := len(seen()); n != 2 {
		t.Errorf("%d sends, want 2: a loose token file warns and still sends", n)
	}
}

// notify --test reaches every destination, says what each answered, and exits
// 0, 1 or 2 as they did.
func TestNotifyTestExitsByWhatTheDestinationsDid(t *testing.T) {
	t.Cleanup(func() { notifyTest = false })
	ok, seen := ntfyServer(t, http.StatusOK, "")
	es := ntfyFixture(t, ok.URL+"/drover", `["exit"]`)
	root := es.cfg.Root

	var err error
	out := capture(t, func() { err = execute(t, "notify", "--test") })
	if err != nil || !strings.Contains(out, "HTTP 200") {
		t.Fatalf("notify --test against a server answering 200: %v\n%s", err, out)
	}
	if got := seen(); len(got) != 1 || got[0].header.Get("Title") != "drover: example TEST" ||
		got[0].body != "test: notification test from drover, nothing to do" {
		t.Errorf("the test event went out as %+v", got)
	}

	bin := t.TempDir()
	stub(t, filepath.Join(bin, "notify"), "#!/bin/sh\ncat >/dev/null\necho 'notifier broke'\nexit 3\n")
	conf := filepath.Join(root, ".drover.toml")
	data, rerr := os.ReadFile(conf)
	if rerr != nil {
		t.Fatal(rerr)
	}
	write(t, conf, strings.Replace(string(data), "[supervise.ntfy]", "[supervise]\nnotify = [\""+filepath.Join(bin, "notify")+"\"]\n\n[supervise.ntfy]", 1))
	out = capture(t, func() { err = execute(t, "notify", "--test") })
	if Code(err) != 1 || !strings.Contains(out, "exit 3: notifier broke") || !strings.Contains(out, "HTTP 200") {
		t.Errorf("a failing argv beside a working ntfy: exit %d\n%s", Code(err), out)
	}

	write(t, conf, string(data[:strings.Index(string(data), "[supervise.ntfy]")]))
	out = capture(t, func() { err = execute(t, "notify", "--test") })
	if Code(err) != 2 {
		t.Errorf("no destination configured exits %d, want 2:\n%s", Code(err), out)
	}
}
