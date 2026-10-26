package cmd

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runlog"
)

// UsageTotals is a set of usage rows summed. Weighted is runlog.Weighted over
// the summed tokens, a ranking and not the subscription's accounting, and
// CostUSD the runners' list-price figures added up.
type UsageTotals struct {
	Runs     int          `json:"runs"`
	Tokens   runlog.Usage `json:"tokens"`
	Weighted int64        `json:"weighted"`
	CostUSD  float64      `json:"cost_usd"`
}

func (t *UsageTotals) add(r ledger.Row) {
	t.Runs++
	t.Tokens.Add(*r.Usage)
	t.Weighted = int64(math.Round(runlog.Weighted(t.Tokens)))
	t.CostUSD += r.CostUSD
}

// ReportUsage is the report-level usage object: every usage row of this
// project in the window, whichever item it names or none, and the budget
// measured across every registered project when [usage] declares one.
type ReportUsage struct {
	// Window is "7d", or the RFC 3339 time of the last [usage] reset the
	// window runs from.
	Window string `json:"window"`
	UsageTotals
	// ByStage holds the same totals per stage, an interactive row's under
	// "interactive".
	ByStage map[string]UsageTotals `json:"by_stage"`
	// Budget and BudgetUsed are [usage] weekly_budget and the weighted total of
	// every registered project's window over it. BudgetUnread names each
	// project whose ledger would not read, which the fraction then leaves out.
	Budget       int64    `json:"budget,omitempty"`
	BudgetUsed   *float64 `json:"budget_used,omitempty"`
	BudgetUnread []string `json:"budget_unread,omitempty"`
	start        time.Time
}

// LongSession is one session in the window that ran longer than [usage]
// session_max_hours or reached a context past [usage] context_warn: the shape
// of work drover exists to keep short.
type LongSession struct {
	SessionID  string  `json:"session_id"`
	Source     string  `json:"source"`
	ID         string  `json:"id"`
	Hours      float64 `json:"hours"`
	ContextMax int64   `json:"context_max"`
	Weighted   int64   `json:"weighted"`
	// Reason is why it qualified, in words a view prints as they are.
	Reason string `json:"reason"`
	// Started and LastSeen are the earliest and latest instants its rows
	// place it at.
	Started  string `json:"started"`
	LastSeen string `json:"last_seen"`
	// State is "ended" when every row is headless, written once the session
	// exits, and "last_seen" otherwise. Never "open": an interactive row is
	// written per finished day, so no row says a session is open now.
	State string `json:"state"`
}

// Long session states. The ledger holds no row written while a session runs.
const (
	sessionEnded    = "ended"
	sessionLastSeen = "last_seen"
)

// stageInteractive is the by_stage key of a usage row no stage launched.
const stageInteractive = "interactive"

// itemUsage sums every usage row by the item it names, all time.
func itemUsage(rows []ledger.Row) map[string]*UsageTotals {
	out := map[string]*UsageTotals{}
	for _, r := range rows {
		if r.ID == "" {
			continue
		}
		if out[r.ID] == nil {
			out[r.ID] = &UsageTotals{}
		}
		out[r.ID].add(r)
	}
	return out
}

// usageWindow is where the report-level window starts and what it is called.
func usageWindow(cfg *config.Config, now time.Time) (time.Time, string) {
	if r, err := config.ParseReset(cfg.Usage.Reset); cfg.Usage.Reset != "" && err == nil {
		start := r.Last(now)
		return start, start.Format(time.RFC3339)
	}
	return now.Add(-7 * 24 * time.Hour), "7d"
}

// inWindow reports whether a row's ts falls in [start, now].
func inWindow(r ledger.Row, start, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, r.TS)
	return err == nil && !t.Before(start) && !t.After(now)
}

// reportUsage is the report-level usage object, nil when the ledger holds no
// usage row: no row is no measurement, which a zero would misreport.
func (s *session) reportUsage(now time.Time) *ReportUsage {
	if len(s.ledger.Usage) == 0 {
		return nil
	}
	start, window := usageWindow(s.cfg, now)
	u := &ReportUsage{Window: window, ByStage: map[string]UsageTotals{}, start: start}
	for _, r := range s.ledger.Usage {
		if !inWindow(r, start, now) {
			continue
		}
		u.add(r)
		stage := r.Stage
		if stage == "" {
			stage = stageInteractive
		}
		t := u.ByStage[stage]
		t.add(r)
		u.ByStage[stage] = t
	}
	if b := s.cfg.Usage.WeeklyBudget; b > 0 && s.cfg.Usage.Reset != "" {
		total, unread := s.accountUsage(start, now)
		used := total / float64(b)
		u.Budget, u.BudgetUsed, u.BudgetUnread = b, &used, unread
	}
	return u
}

// accountUsage is the weighted total of every registered project's usage rows
// in [start, now], this one's included, and the projects whose ledger would
// not read. The limit is per account, so one project's share of it says
// nothing about how close the account is.
func (s *session) accountUsage(start, now time.Time) (float64, []string) {
	var total float64
	ledgers, unread := s.accountLedgers()
	for _, al := range ledgers {
		for _, r := range al.ledger.Usage {
			if inWindow(r, start, now) {
				total += runlog.Weighted(*r.Usage)
			}
		}
	}
	return total, unread
}

// accountLedger is one registered project's ledger, as accountLedgers read it.
type accountLedger struct {
	project string
	ledger  *ledger.Ledger
}

// accountLedgers is this project's ledger and every other registered
// project's, and the projects whose ledger would not read, nil when none.
//
// Ledgers are read once each by resolved path: a drive's worktree reaches its
// project's ledger through a link, and a worktree registered beside its parent
// would otherwise count the same rows twice. A registered root that is gone or
// holds no config is not a project, as in status --all-projects, and counts
// nothing without being unread.
func (s *session) accountLedgers() ([]accountLedger, []string) {
	out := []accountLedger{{project: s.cfg.Project, ledger: s.ledger}}
	seen := map[string]bool{resolved(s.cfg.Abs(s.cfg.Paths.Ledger)): true}
	unread := []string{}
	reg, err := projects.Read(projects.Path(config.GlobalPath()))
	if err != nil {
		return out, append(unread, fmt.Sprintf("the project registry: %v", err))
	}
	live, parents, _ := hostProjects(reg)
	for _, p := range live {
		if _, ok := parents[p.Root]; ok {
			continue
		}
		cfg, err := config.LoadAt(p.Root)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%s): %v", p.Project, p.Root, err))
			continue
		}
		path := resolved(cfg.Abs(cfg.Paths.Ledger))
		if seen[path] {
			continue
		}
		seen[path] = true
		l, err := ledger.Load(path)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%s): %v", p.Project, p.Root, err))
			continue
		}
		out = append(out, accountLedger{project: p.Project, ledger: l})
	}
	if len(unread) == 0 {
		return out, nil
	}
	return out, unread
}

// longSessions is every session with a usage row in [start, now] whose rows,
// all of them, span more than [usage] session_max_hours or reach a context
// past [usage] context_warn. Nil when the ledger holds no usage row, and empty
// when it does and none qualifies, so "none ran long" reads differently from
// "nothing measured".
func (s *session) longSessions(start, now time.Time) *[]LongSession {
	if len(s.ledger.Usage) == 0 {
		return nil
	}
	type span struct {
		LongSession
		first, last time.Time
		tokens      runlog.Usage
		inWindow    bool
		interactive bool
	}
	byID := map[string]*span{}
	var order []string
	for _, r := range s.ledger.Usage {
		if r.SessionID == "" {
			continue
		}
		sp := byID[r.SessionID]
		if sp == nil {
			sp = &span{LongSession: LongSession{SessionID: r.SessionID, Source: r.Source, ID: r.ID}}
			byID[r.SessionID] = sp
			order = append(order, r.SessionID)
		}
		sp.inWindow = sp.inWindow || inWindow(r, start, now)
		sp.interactive = sp.interactive || r.Source != ledger.SourceHeadless
		if sp.ID == "" {
			sp.ID = r.ID
		}
		sp.tokens.Add(*r.Usage)
		if r.ContextMax != nil {
			sp.ContextMax = max(sp.ContextMax, *r.ContextMax)
		}
		for _, t := range rowSpan(r) {
			if sp.first.IsZero() || t.Before(sp.first) {
				sp.first = t
			}
			if t.After(sp.last) {
				sp.last = t
			}
		}
	}
	maxHours, warn := s.cfg.UsageSessionMaxHours(), s.cfg.UsageContextWarn()
	out := []LongSession{}
	for _, id := range order {
		sp := byID[id]
		if !sp.inWindow {
			continue
		}
		hours := sp.last.Sub(sp.first).Hours()
		var reasons []string
		if hours > float64(maxHours) {
			reasons = append(reasons, fmt.Sprintf("open %s, past %dh", clock(sp.last.Sub(sp.first)), maxHours))
		}
		if sp.ContextMax > warn {
			reasons = append(reasons, fmt.Sprintf("context reached %s, past %s", tokenCount(float64(sp.ContextMax)), tokenCount(float64(warn))))
		}
		if len(reasons) == 0 {
			continue
		}
		sp.Hours = math.Round(hours*10) / 10
		sp.Weighted = int64(math.Round(runlog.Weighted(sp.tokens)))
		sp.Reason = reasons[0]
		if len(reasons) > 1 {
			sp.Reason += "; " + reasons[1]
		}
		sp.Started, sp.LastSeen = sp.first.Format(time.RFC3339), sp.last.Format(time.RFC3339)
		sp.State = sessionEnded
		if sp.interactive {
			sp.State = sessionLastSeen
		}
		out = append(out, sp.LongSession)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weighted > out[j].Weighted })
	return &out
}

// rowSpan is every instant a usage row says its session was open at: the
// session's start and the day's first message on an interactive row, the
// start a headless row's duration implies, and the row's own ts.
func rowSpan(r ledger.Row) []time.Time {
	var out []time.Time
	for _, v := range []string{r.Started, r.FirstTS, r.TS} {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out = append(out, t)
		}
	}
	if t, err := time.Parse(time.RFC3339, r.TS); err == nil && r.DurationMS > 0 {
		out = append(out, t.Add(-time.Duration(r.DurationMS)*time.Millisecond))
	}
	return out
}

// tokenCount is a count in its largest unit, one decimal at most: 840, 12.5K,
// 840K, 12.5M, 1.2B. A value that rounds up to the next unit's 1000 is written
// in that unit, so 999,960 is 1M and never 1000K.
func tokenCount(f float64) string {
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	if math.Round(f) < 1000 {
		return sign + fmt.Sprintf("%.0f", f)
	}
	units := []string{"K", "M", "B"}
	v := f
	for i, u := range units {
		v /= 1000
		r := math.Round(v*10) / 10
		if r < 1000 || i == len(units)-1 {
			s := fmt.Sprintf("%.1f", r)
			if len(s) > 2 && s[len(s)-2:] == ".0" {
				s = s[:len(s)-2]
			}
			return sign + s + u
		}
	}
	return ""
}

// dollars is a float as whole dollars with thousands separators: $0, $12,
// $1,234.
func dollars(f float64) string {
	n := int64(math.Round(math.Abs(f)))
	digits := fmt.Sprintf("%d", n)
	var b []byte
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, digits[i])
	}
	if f < 0 && n > 0 {
		return "-$" + string(b)
	}
	return "$" + string(b)
}

// budgetClass is the class a budget fraction is styled with: warning from
// 0.8, critical from 1.0, and none below.
func budgetClass(v any) string {
	f, ok := number(v)
	switch {
	case !ok:
		return ""
	case f >= 1:
		return "critical"
	case f >= 0.8:
		return "warning"
	}
	return ""
}
