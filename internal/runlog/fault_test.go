package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two fixtures are captured streams, not written ones: an implementing run
// and a review pass that each hit a service failure. A hand-written failure proves the classifier reads what somebody thought a
// runner emits.
func TestClaudeOverloadedReadsAsTransient(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "claude-overloaded.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if rd.Result.Succeeded() {
		t.Fatal("a session that died against the API must not read as succeeded")
	}
	if got := rd.Result.Fault; got != FaultTransient {
		t.Errorf("fault = %q, want %q", got, FaultTransient)
	}
	if !rd.Result.Faulted() {
		t.Error("Faulted must agree with Fault")
	}
	// Nothing in the message names an instant, so nothing may be waited on.
	if !rd.Result.RetryAt.IsZero() {
		t.Errorf("RetryAt = %v, want zero: the message names no instant", rd.Result.RetryAt)
	}
}

func TestCodexQuotaReadsAsQuotaWithItsInstant(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "codex-quota.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if got := rd.Result.Fault; got != FaultQuota {
		t.Errorf("fault = %q, want %q", got, FaultQuota)
	}
	want := time.Date(2026, time.September, 22, 10, 3, 0, 0, time.Local)
	if !rd.Result.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %v, want %v", rd.Result.RetryAt, want)
	}
}

// Claude Code reports an HTTP failure as subtype "success" with is_error true,
// so the subtype alone says a run that finished cleanly.
func TestTheSubtypeAloneCannotSeeAFault(t *testing.T) {
	rd, _ := ReadResult(filepath.Join("testdata", "claude-overloaded.jsonl"))
	if rd.Result.Subtype != "success" {
		t.Fatalf("fixture drifted: subtype = %q", rd.Result.Subtype)
	}
	if !rd.Result.Transport {
		t.Error("terminal_reason api_error must reach Transport")
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name string
		in   Result
		want string
	}{
		{"a clean run is never a fault",
			Result{Subtype: "success"}, FaultNone},
		{"a run that exhausted its turns failed at its own work",
			Result{Subtype: "error_max_turns", IsError: true, Text: "reached the turn limit"}, FaultNone},
		{"an agent reporting a bug in the code is not a fault",
			Result{Subtype: "success", IsError: true, Text: "the verify command refused: 3 tests failed"}, FaultNone},
		{"a 529 in the message",
			Result{Subtype: "success", IsError: true, Text: "API Error: 529 Overloaded."}, FaultTransient},
		{"a 500 in the message",
			Result{Subtype: "success", IsError: true, Text: "API Error: 500 Internal server error."}, FaultTransient},
		{"the API named as the cause, with nothing in the message",
			Result{Subtype: "success", IsError: true, Transport: true}, FaultTransient},
		{"a usage limit outranks the transport that reported it",
			Result{Subtype: "success", IsError: true, Transport: true, Text: "You've hit your usage limit."}, FaultQuota},
		{"a 429 is a quota rather than something to retry in a minute",
			Result{Subtype: "failed", IsError: true, Text: "API Error: 429 Too Many Requests"}, FaultQuota},
		{"a 400 is drover's own request, which no wait repairs",
			Result{Subtype: "success", IsError: true, Text: "API Error: 400 invalid_request_error"}, FaultNone},
		{"a number in an agent's report is not a status",
			Result{Subtype: "success", IsError: true, Text: "handler.go:429 still returns 503 rows"}, FaultNone},
		{"a session that never authenticated is not a service to come back to",
			Result{Subtype: "success", IsError: true, Transport: true, Text: "Not logged in · Please run /login"}, FaultAuth},
		{"a 401 is the credentials",
			Result{Subtype: "success", IsError: true, Text: "API Error: 401 authentication_error"}, FaultAuth},
		{"agy reporting its own error",
			Result{Subtype: "error", IsError: true, Text: "connection error: no owner to host"}, FaultTransient},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in).Fault; got != c.want {
				t.Errorf("fault = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRetryAt(t *testing.T) {
	local := func(y int, m time.Month, d, h, minute int) time.Time {
		return time.Date(y, m, d, h, minute, 0, 0, time.Local)
	}
	for _, c := range []struct {
		name string
		text string
		want time.Time
	}{
		{"codex names it for a person",
			"try again at Sep 22nd, 2026 10:03 AM", local(2026, time.September, 22, 10, 3)},
		{"an afternoon reset",
			"try again at Dec 1st, 2026 4:30 PM", local(2026, time.December, 1, 16, 30)},
		{"midnight is hour zero, not hour twelve",
			"try again at Jan 3rd, 2027 12:15 AM", local(2027, time.January, 3, 0, 15)},
		{"a Unix second after a pipe",
			"Claude AI usage limit reached|1789545000", time.Unix(1789545000, 0)},
		{"a message naming no instant",
			"API Error: 529 Overloaded. Try again in a moment.", time.Time{}},
		{"an impossible clock time is no instant at all",
			"try again at Sep 22nd, 2026 99:99", time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RetryAt(c.text)
			if !got.Equal(c.want) {
				t.Errorf("RetryAt = %v, want %v", got, c.want)
			}
		})
	}
}

// Claude Code's weekly-limit refusal as F14's review log recorded it: the last
// rate_limit_event before the result, and the result itself.
const (
	weeklyEvent  = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792983600,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1792903800,"status":"allowed"},"seven_day":{"utilization":1,"resetsAt":1792983600,"status":"rejected","surpassedThreshold":1}}}}`
	weeklyResult = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"You've hit your weekly limit · resets Oct 25 at 8pm (America/Los_Angeles)"}`
	// The last timestamped event of the same log, which the yearless sentence
	// is read against.
	weeklyAssistant = `{"type":"assistant","timestamp":"2026-10-24T23:57:54.854Z","message":{"content":[]}}`
)

func TestClaudeWeeklyLimitNamesItsReset(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	reset := time.Date(2026, time.October, 25, 20, 0, 0, 0, la)
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792900000}}`
	// An epoch the prose disagrees with, so a pass proves which one was read.
	elsewhere := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792990800}}`
	for _, c := range []struct {
		name  string
		lines []string
		want  time.Time
	}{
		{"the event's epoch", []string{weeklyAssistant, weeklyEvent, weeklyResult}, reset},
		{"the event's epoch wins over the prose", []string{weeklyAssistant, elsewhere, weeklyResult}, time.Unix(1792990800, 0)},
		{"the last event is the one read", []string{weeklyAssistant, elsewhere, weeklyEvent, weeklyResult}, reset},
		{"the sentence alone, read against the log's own clock", []string{weeklyAssistant, weeklyResult}, reset},
		{"an allowed event names no reset, so the sentence does", []string{weeklyAssistant, allowed, weeklyResult}, reset},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.jsonl")
			if err := os.WriteFile(p, []byte(strings.Join(c.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rd, err := ReadResult(p)
			if err != nil || !rd.Found {
				t.Fatalf("found=%v err=%v", rd.Found, err)
			}
			if rd.Result.Fault != FaultQuota {
				t.Errorf("fault = %q, want %q", rd.Result.Fault, FaultQuota)
			}
			if !rd.Result.RetryAt.Equal(c.want) {
				t.Errorf("RetryAt = %v, want %v", rd.Result.RetryAt, c.want)
			}
		})
	}
}

// A rejected event on a result that is not a quota fault is not a reset to
// wait on: the session failed on something else.
func TestARateLimitEventAloneIsNotAQuota(t *testing.T) {
	r := Classify(Result{IsError: true, Transport: true, Text: "API Error: 529 Overloaded",
		RateLimitStatus: "rejected", RateLimitReset: 1792983600})
	if r.Fault != FaultTransient || !r.RetryAt.IsZero() {
		t.Errorf("fault %q at %v, want %q and no instant", r.Fault, r.RetryAt, FaultTransient)
	}
}

func TestRetryResets(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	oct24 := time.Date(2026, time.October, 24, 23, 57, 54, 0, time.UTC)
	for _, c := range []struct {
		name string
		text string
		ref  time.Time
		want time.Time
	}{
		{"Claude's sentence",
			"You've hit your weekly limit · resets Oct 25 at 8pm (America/Los_Angeles)", oct24,
			time.Date(2026, time.October, 25, 20, 0, 0, 0, la)},
		{"minutes past the hour",
			"You've hit your weekly limit · resets Oct 25 at 8:30pm (America/Los_Angeles)", oct24,
			time.Date(2026, time.October, 25, 20, 30, 0, 0, la)},
		{"a zone that does not load is the local one",
			"You've hit your weekly limit · resets Oct 25 at 8pm (Mars/Olympus_Mons)", oct24,
			time.Date(2026, time.October, 25, 20, 0, 0, 0, time.Local)},
		{"December read in late December stays in its year",
			"resets Dec 30 at 9am (America/Los_Angeles)", time.Date(2026, time.December, 27, 12, 0, 0, 0, la),
			time.Date(2026, time.December, 30, 9, 0, 0, 0, la)},
		{"January into next year's January",
			"resets Jan 2 at 9am (America/Los_Angeles)", time.Date(2026, time.December, 30, 12, 0, 0, 0, la),
			time.Date(2027, time.January, 2, 9, 0, 0, 0, la)},
		{"a December 31st reset read on January 1st is not 12 months out",
			"resets Dec 31 at 9am (America/Los_Angeles)", time.Date(2027, time.January, 1, 12, 0, 0, 0, la),
			time.Time{}},
		{"a day the month lacks is no instant",
			"resets Feb 30 at 9am (America/Los_Angeles)", time.Date(2027, time.February, 25, 0, 0, 0, 0, la),
			time.Time{}},
		{"the year-and-time form still parses ahead of it",
			"try again at Sep 22nd, 2026 10:03 AM", oct24,
			time.Date(2026, time.September, 22, 10, 3, 0, 0, time.Local)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := retryText(c.text, c.ref)
			if !got.Equal(c.want) {
				t.Errorf("retryText = %v, want %v", got, c.want)
			}
		})
	}
}
