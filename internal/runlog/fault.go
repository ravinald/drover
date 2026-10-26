package runlog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A fault is a failed result whose cause is the runner's transport rather than
// the session's work: the model was unreachable, or the account is out of
// budget. It is the one failure class a caller answers by waiting rather than
// by reading the code, and the class that is true of the next launch too.
//
// Which fault it is comes out of the failure's own prose, because no runner
// puts a status code in a field. Claude carries "API Error: 529 Overloaded" as
// its result text; codex carries "You\u2019ve hit your usage limit" as a
// turn.failed message. Text matching is therefore the floor rather than a
// shortcut, and it fails in the safe direction: an unmatched failure reads as
// FaultNone and parks the item, as it would with no classifier at all.
//
// Whether it is a fault at all is structural where a runner says so.
// Result.Transport is Claude Code's terminal_reason naming the API, and a
// failure carrying it is a fault whether or not its message matches anything
// here.
const (
	FaultNone      = ""
	FaultTransient = "transient" // the service was unreachable or overloaded
	FaultQuota     = "quota"     // the account is out of budget until a reset
	FaultAuth      = "auth"      // the runner has no credentials the service accepts
	// FaultRunner is a runner that hands the session to a service of its own
	// reporting that the session ended without completing: a T3 Code run
	// interrupted, failed, or lost with its thread. No wait repairs it and the
	// next item would meet the same service, so it is not retried.
	FaultRunner = "runner"
)

// quotaSigns and transientSigns are matched against a lowercased failure
// message, quota first: a 429 names a rate limit, and answering that with a
// 45-second retry spends the next launch to be told what this one already said.
//
// Every sign is a phrase rather than a number. A bare "429" or "503" matches a
// line number, a port and a byte count as readily as a status, and the text
// being matched is an agent's own closing report: it is prose, and the cost of
// reading prose wrong here is a drive that stops on a session that finished.
// statusPat below is where a code is read, and only where the runner has
// written it as one.
var (
	// A session that never authenticated is the third: not the work either, and
	// the only one of the three no wait repairs. A runner can leave
	// "Not logged in · Please run /login" under the same terminal_reason as a
	// 529, so without this it reads as a service to come back to and costs two
	// retries on the way to a park that names the wrong cause.
	authSigns = []string{
		"not logged in",
		"please run /login",
		"unauthorized",
		"authentication",
		"invalid api key",
		"invalid_api_key",
		"credentials",
	}
	// Claude Code names the spent window rather than saying "usage": "You've
	// hit your weekly limit", "... session limit", "... Opus limit".
	quotaSigns = []string{
		"you've hit your",
		"usage limit",
		"rate limit",
		"rate_limit",
		"insufficient_quota",
		"out of credits",
		"too many requests",
	}
	transientSigns = []string{
		"overloaded",
		"internal server error",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"connection error",
		"connection reset",
	}
)

// statusPat reads an HTTP status a runner wrote as one. Claude Code prefixes
// every transport failure with "API Error: ", which is the only place in any
// stream drover reads where a bare number means a status.
var statusPat = regexp.MustCompile(`(?i)api error:?\s*(\d{3})`)

// statusFault maps a status onto a fault: 401 and 403 are the credentials, 429
// is the account, and a 5xx is the service. Any other 4xx is a request drover
// made wrong, which no wait repairs and no park should blame the service for, so
// it reads as no fault at all.
func statusFault(text string) string {
	m := statusPat.FindStringSubmatch(text)
	if m == nil {
		return FaultNone
	}
	switch {
	case m[1] == "401", m[1] == "403":
		return FaultAuth
	case m[1] == "429":
		return FaultQuota
	case strings.HasPrefix(m[1], "5"):
		return FaultTransient
	}
	return FaultNone
}

// Classify fills Fault and RetryAt on a failed result, and leaves a successful
// one alone.
//
// A session that ran and failed on its own terms is not a fault: error_max_turns
// is the run hitting a limit drover set, and a runner reporting its own refusal
// has produced a result about the work. Only a failure naming the service or the
// account reaches a token here.
func Classify(r Result) Result {
	if r.Succeeded() {
		return r
	}
	text := strings.ToLower(r.Text)
	switch {
	case containsAny(text, authSigns):
		r.Fault = FaultAuth
	case containsAny(text, quotaSigns):
		r.Fault = FaultQuota
	case statusFault(text) != FaultNone:
		r.Fault = statusFault(text)
	case containsAny(text, transientSigns), r.Transport:
		// Transport last: a runner that named the API and then said it was out
		// of budget is out of budget, and retrying that in 45 seconds spends a
		// session to be told so again.
		r.Fault = FaultTransient
	default:
		return r
	}
	r.RetryAt = retryAt(r)
	return r
}

func containsAny(text string, signs []string) bool {
	for _, s := range signs {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// RetryAt is the instant a failure message names as the one to come back at,
// and the zero time when it names none. A sentence with no year in it is read
// against now.
//
// Three forms, because two runners state it three ways and neither offers a
// field in the message. codex writes it for a person: "try again at Sep 22nd,
// 2026 10:03 AM", with an ordinal suffix no time layout in the standard library
// parses. Claude Code once appended a Unix second to its quota message after a
// pipe, and now writes "resets Oct 25 at 8pm (America/Los_Angeles)", with no
// year and the zone it means.
//
// A caller waits on this, so every uncertain read returns zero: an instant
// drover guessed wrong is a drive asleep until tomorrow, while no instant is a
// drive that stops and says why.
func RetryAt(text string) time.Time {
	return retryText(text, time.Now())
}

// retryAt is the instant a classified result names. Claude Code's
// rate_limit_event states a quota reset as an epoch, which no sentence it
// rewords can break, so a rejected one wins over the prose.
func retryAt(r Result) time.Time {
	if rl := r.RateLimit; r.Fault == FaultQuota && rl != nil && rl.Status == "rejected" && rl.ResetsAt > 0 {
		return time.Unix(rl.ResetsAt, 0)
	}
	ref := r.loggedAt
	if ref.IsZero() {
		ref = time.Now()
	}
	return retryText(r.Text, ref)
}

func retryText(text string, ref time.Time) time.Time {
	if t := retryEpoch(text); !t.IsZero() {
		return t
	}
	if t := retryProse(text); !t.IsZero() {
		return t
	}
	return retryResets(text, ref)
}

var (
	epochPat  = regexp.MustCompile(`\|\s*(\d{10,})\b`)
	prosePat  = regexp.MustCompile(`(?i)\b([A-Z][a-z]{2})[a-z]*\.? (\d{1,2})(?:st|nd|rd|th)?,? (\d{4}),? (\d{1,2}):(\d{2})\s*(AM|PM)?`)
	resetsPat = regexp.MustCompile(`(?i)\bresets ([A-Z][a-z]{2})[a-z]*\.? (\d{1,2})(?:st|nd|rd|th)?,? at (\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([A-Za-z0-9_+/-]+)\))?`)
)

// resetsAhead bounds how far out a yearless reset may land. The longest window
// Claude Code reports is seven days, so a year that puts the instant further
// out than this is a misread, not a long wait.
const resetsAhead = 8 * 24 * time.Hour

func retryEpoch(text string) time.Time {
	m := epochPat.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// retryProse reads the human sentence. The zone is the local one: a runner
// writing a wall-clock time with no offset means the operator's own clock, and
// that is the clock the drive's own --until is read against.
func retryProse(text string) time.Time {
	m := prosePat.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	mon, ok := months[strings.ToLower(m[1])]
	if !ok {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	hour, ok = clock12(hour, m[6])
	if !ok || day < 1 || day > 31 || minute > 59 {
		return time.Time{}
	}
	return time.Date(year, mon, day, hour, minute, 0, 0, time.Local)
}

// retryResets reads Claude Code's sentence, which names month, day, hour and
// zone but no year. The year is the first that puts the instant after ref, the
// log's own last timestamp, and a zone that will not load falls back to the
// local one rather than costing the instant.
func retryResets(text string, ref time.Time) time.Time {
	m := resetsPat.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	mon, ok := months[strings.ToLower(m[1])]
	if !ok {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[2])
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	hour, ok = clock12(hour, m[5])
	if !ok || day < 1 || day > 31 || minute > 59 {
		return time.Time{}
	}
	loc := time.Local
	if m[6] != "" {
		if l, err := time.LoadLocation(m[6]); err == nil {
			loc = l
		}
	}
	for _, year := range []int{ref.In(loc).Year(), ref.In(loc).Year() + 1} {
		t := time.Date(year, mon, day, hour, minute, 0, 0, loc)
		if t.Day() != day || !t.After(ref) {
			continue
		}
		if t.Sub(ref) > resetsAhead {
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}

// clock12 turns an hour with an optional AM or PM onto the 24-hour clock, and
// reports false for an hour that clock has no place for.
func clock12(hour int, suffix string) (int, bool) {
	switch strings.ToUpper(suffix) {
	case "PM":
		if hour < 12 {
			hour += 12
		}
	case "AM":
		if hour == 12 {
			hour = 0
		}
	}
	return hour, hour <= 23
}

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}
