package runlog

import (
	"path/filepath"
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
