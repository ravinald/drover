package runlog

import (
	"math"
	"time"
)

// The rate-limit windows Claude Code reports, under the names its
// unifiedWindows object keys them by.
const (
	WindowFiveHour = "five_hour"
	WindowSevenDay = "seven_day"
)

// RateLimit is one rate_limit_event, the only place a Claude account states how
// much of it is left. The service writes one per request, so the last one a
// session wrote is the account as that session left it.
//
// An account runs out twice, in sequence: the weekly allotment, then any
// extra-usage credit bought to work past it. The first is a fraction of each
// window. The second has no figure at all: once the weekly window reads 1, the
// event says only whether overage is in use, and at the end that it is
// out_of_credits.
type RateLimit struct {
	// Status is allowed, allowed_warning or rejected, about the window
	// RateLimitType names, and ResetsAt that window's reset as a Unix second.
	Status        string `json:"status"`
	RateLimitType string `json:"rate_limit_type,omitempty"`
	ResetsAt      int64  `json:"resets_at,omitempty"`
	// FiveHour and SevenDay are each window's own reading, nil where the event
	// carried none.
	FiveHour *LimitWindow `json:"five_hour,omitempty"`
	SevenDay *LimitWindow `json:"seven_day,omitempty"`
	// IsUsingOverage is the session drawing on purchased credit past the weekly
	// allotment. OverageStatus is whether the account would allow it, and
	// OverageDisabledReason why not: org_level_disabled on an account that
	// never bought any, out_of_credits on one that spent what it bought.
	IsUsingOverage        bool   `json:"is_using_overage,omitempty"`
	OverageStatus         string `json:"overage_status,omitempty"`
	OverageDisabledReason string `json:"overage_disabled_reason,omitempty"`
}

// LimitWindow is one window's share of a reading: Utilization from 0 to 1, in
// the service's own steps of 0.01, and the instant it resets.
type LimitWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
	Status      string  `json:"status,omitempty"`
}

// OutOfCredits is the overage reason that means the purchased credit is spent.
const OutOfCredits = "out_of_credits"

// Window is the named window's reading, nil for a name the event did not carry.
func (r *RateLimit) Window(name string) *LimitWindow {
	if r == nil {
		return nil
	}
	switch name {
	case WindowFiveHour:
		return r.FiveHour
	case WindowSevenDay:
		return r.SevenDay
	}
	return nil
}

// Remaining is the percent of a window left as of now, 100 - utilization*100,
// and false where the reading carries no such window. A window whose reset has
// passed reads 100: the reading predates the reset, and the account it
// describes has started over.
func (r *RateLimit) Remaining(name string, now time.Time) (int, bool) {
	w := r.Window(name)
	if w == nil {
		return 0, false
	}
	if w.ResetsAt > 0 && !now.Before(time.Unix(w.ResetsAt, 0)) {
		return 100, true
	}
	return int(math.Round((1 - w.Utilization) * 100)), true
}

// WindowReset is when the named window resets, zero where the reading names
// none. The top-level resetsAt stands in for the window it is about.
func (r *RateLimit) WindowReset(name string) time.Time {
	if w := r.Window(name); w != nil && w.ResetsAt > 0 {
		return time.Unix(w.ResetsAt, 0)
	}
	if r != nil && r.RateLimitType == name && r.ResetsAt > 0 {
		return time.Unix(r.ResetsAt, 0)
	}
	return time.Time{}
}

// Current reports whether the reading still describes the account at now:
// the window it names has not reset since.
func (r *RateLimit) Current(now time.Time) bool {
	if r == nil {
		return false
	}
	reset := time.Unix(r.ResetsAt, 0)
	if r.ResetsAt == 0 {
		reset = r.WindowReset(r.RateLimitType)
	}
	return reset.IsZero() || now.Before(reset)
}

// WeeklyRejected reports the weekly allotment spent: the event's own window is
// the week and it was rejected, or the week's window says so itself.
func (r *RateLimit) WeeklyRejected() bool {
	if r == nil {
		return false
	}
	return (r.RateLimitType == WindowSevenDay && r.Status == "rejected") ||
		(r.SevenDay != nil && r.SevenDay.Status == "rejected")
}

// InOverage reports a session drawing on purchased credit because the weekly
// allotment is spent.
func (r *RateLimit) InOverage() bool {
	return r.WeeklyRejected() && r.IsUsingOverage
}

// Refused reports a reading under which the service turns a session away:
// rejected, and not carried on purchased credit. Both of Claude's refusals
// read this way, a weekly window spent on an account with no overage and the
// overage spent with it (out_of_credits).
func (r *RateLimit) Refused() bool {
	return r != nil && r.Status == "rejected" && !r.IsUsingOverage
}

// claudeRateLimit is rate_limit_info as Claude Code writes it.
type claudeRateLimit struct {
	Status                string `json:"status"`
	ResetsAt              int64  `json:"resetsAt"`
	RateLimitType         string `json:"rateLimitType"`
	OverageStatus         string `json:"overageStatus"`
	OverageDisabledReason string `json:"overageDisabledReason"`
	IsUsingOverage        bool   `json:"isUsingOverage"`
	UnifiedWindows        struct {
		FiveHour *claudeWindow `json:"five_hour"`
		SevenDay *claudeWindow `json:"seven_day"`
	} `json:"unifiedWindows"`
}

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
	Status      string  `json:"status"`
}

func (w *claudeWindow) window() *LimitWindow {
	if w == nil {
		return nil
	}
	return &LimitWindow{Utilization: w.Utilization, ResetsAt: w.ResetsAt, Status: w.Status}
}

func (c claudeRateLimit) reading() *RateLimit {
	return &RateLimit{
		Status: c.Status, RateLimitType: c.RateLimitType, ResetsAt: c.ResetsAt,
		FiveHour: c.UnifiedWindows.FiveHour.window(), SevenDay: c.UnifiedWindows.SevenDay.window(),
		IsUsingOverage: c.IsUsingOverage, OverageStatus: c.OverageStatus,
		OverageDisabledReason: c.OverageDisabledReason,
	}
}
