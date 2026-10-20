package cmd

import (
	"math"
	"slices"
	"time"

	"github.com/ravinald/drover/internal/ledger"
)

// defaultWindow is how many days the trends cover when --window is not given:
// two weeks holds enough landings to read a median from, and a gate that crept
// from two minutes to twenty shows on the sparkline inside the first day.
const defaultWindow = 14

// maxWindow bounds --window. Every day is an object in the report and a point
// on every sparkline, so a typo of 14000 would print a page nobody can read.
const maxWindow = 366

// Trends is what the ledger says about the project over the last Window days,
// day by day and in total. Days are calendar days in the zone status reports
// in, oldest first, the last one today.
type Trends struct {
	Window int        `json:"window"`
	From   string     `json:"from"`
	To     string     `json:"to"`
	Days   []TrendDay `json:"days"`
	Total  TrendSet   `json:"total"`
	// last is the newest finish in the whole ledger, window or not, which is
	// what --all-projects prints as a project's last finish.
	last string
}

// TrendDay is one calendar day's TrendSet.
type TrendDay struct {
	Date string `json:"date"`
	TrendSet
}

// TrendSet is every trend over one period. A nil metric is a period with no
// data for it, which is a different answer from a zero and is never reported
// as one: no timed row at all for the two counts, no item the metric can be
// computed from for the rest.
//
// Finishes, and everything measured at a finish, belong to the day of the
// completion row. Parks and gate runs belong to the day of their own row.
type TrendSet struct {
	Finished      *TrendCount  `json:"finished"`
	CycleTime     *TrendSpread `json:"cycle_time"`
	ReviewPasses  *TrendRatio  `json:"review_passes"`
	ChangesRate   *TrendRatio  `json:"changes_rate"`
	Parks         *TrendParks  `json:"parks"`
	ReviewToMerge *TrendSpread `json:"review_to_merge"`
	Verify        *TrendSpread `json:"verify"`
}

// TrendCount is a count, and how many items it counts.
type TrendCount struct {
	Value int `json:"value"`
	N     int `json:"n"`
}

// TrendSpread is a duration in seconds as a median and a 90th percentile, and
// how many samples they are read from.
type TrendSpread struct {
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	N      int     `json:"n"`
}

// TrendRatio is a mean or a share, and how many items it is the mean of.
type TrendRatio struct {
	Value float64 `json:"value"`
	N     int     `json:"n"`
}

// TrendParks is how many parks a period recorded, by reason, and how many
// items they fell on.
type TrendParks struct {
	Value    int            `json:"value"`
	ByReason map[string]int `json:"by_reason"`
	N        int            `json:"n"`
}

// finishing is what one item's finished merge measured.
type finishing struct {
	day              int
	cycle            time.Duration
	hasCycle         bool
	reviews          int
	changesRequested bool
	wait             time.Duration
	hasWait          bool
}

// itemClock is what an item's rows have said since its last finish.
type itemClock struct {
	implement        time.Time
	reviews          int
	changesRequested bool
	passed           time.Time
}

// trendSample is one row-dated observation: a park's reason or a gate's time.
type trendSample struct {
	day    int
	id     string
	reason string
	verify time.Duration
}

// trends reads the ledger once, in file order, and folds it into the window
// ending on now's day in loc. A row with no ts is skipped: its time is day-only
// in the writer's zone, and placing it on a day of loc would guess.
func trends(rows []ledger.Row, now time.Time, loc *time.Location, window int) Trends {
	if window <= 0 {
		window = defaultWindow
	}
	now = now.In(loc)
	y, m, d := now.Date()
	t := Trends{Window: window, Days: make([]TrendDay, window)}
	day := make(map[string]int, window)
	for i := range window {
		date := time.Date(y, m, d-(window-1-i), 0, 0, 0, 0, loc).Format("2006-01-02")
		t.Days[i].Date, day[date] = date, i
	}
	t.From, t.To = t.Days[0].Date, t.Days[window-1].Date

	active := make([]bool, window)
	clocks := map[string]*itemClock{}
	landed := map[string]finishing{}
	var samples []trendSample
	var last time.Time
	for _, r := range rows {
		at, ok := r.At()
		if !ok || r.ID == ledger.DriveRowID {
			continue
		}
		i, inWindow := day[at.In(loc).Format("2006-01-02")]
		if inWindow {
			active[i] = true
		}
		c := clocks[r.ID]
		if c == nil {
			c = &itemClock{}
			clocks[r.ID] = c
		}
		switch {
		case r.Stage == ledger.StageImplement && c.implement.IsZero():
			c.implement = at
		case r.Stage == ledger.StageReview:
			c.reviews++
		}
		if r.Changes != "" {
			c.changesRequested = true
		}
		if r.Pass == passPassed {
			c.passed = at
		}
		if inWindow && r.Park != "" {
			samples = append(samples, trendSample{day: i, id: r.ID, reason: r.Park})
		}
		if inWindow && r.VerifyMS > 0 {
			samples = append(samples, trendSample{day: i, id: r.ID, verify: time.Duration(r.VerifyMS) * time.Millisecond})
		}
		if r.Merged == nil {
			continue
		}
		if !*r.Merged {
			// A tombstone withdraws the completion, so the finish it recorded
			// did not happen as far as any trend is concerned.
			delete(landed, r.ID)
			continue
		}
		if at.After(last) {
			last = at
		}
		if inWindow {
			l := finishing{day: i, reviews: c.reviews, changesRequested: c.changesRequested}
			if !c.implement.IsZero() {
				l.cycle, l.hasCycle = at.Sub(c.implement), true
			}
			if !c.passed.IsZero() {
				l.wait, l.hasWait = at.Sub(c.passed), true
			}
			landed[r.ID] = l
		}
		clocks[r.ID] = &itemClock{}
	}
	if !last.IsZero() {
		t.last = last.In(loc).Format(time.RFC3339)
	}

	all := func(int) bool { return true }
	t.Total = trendSet(landed, samples, slices.Contains(active, true), all)
	for i := range t.Days {
		t.Days[i].TrendSet = trendSet(landed, samples, active[i], func(d int) bool { return d == i })
	}
	return t
}

// trendSet computes one period's metrics from the landings and samples whose
// day in reports true. active says the period holds any timed row, which is
// what separates a count of zero from a period nothing recorded.
func trendSet(landed map[string]finishing, samples []trendSample, active bool, in func(int) bool) TrendSet {
	var set TrendSet
	var cycles, waits, gates []time.Duration
	var count, reviewed, reviews, changesRequested int
	for _, l := range landed {
		if !in(l.day) {
			continue
		}
		count++
		if l.hasCycle {
			cycles = append(cycles, l.cycle)
		}
		if l.hasWait {
			waits = append(waits, l.wait)
		}
		if l.reviews > 0 {
			reviewed++
			reviews += l.reviews
		}
		if l.changesRequested {
			changesRequested++
		}
	}
	parks := &TrendParks{ByReason: map[string]int{}}
	parked := map[string]bool{}
	for _, s := range samples {
		if !in(s.day) {
			continue
		}
		if s.verify > 0 {
			gates = append(gates, s.verify)
			continue
		}
		parks.Value++
		parks.ByReason[s.reason]++
		parked[s.id] = true
	}
	parks.N = len(parked)
	if active {
		set.Finished = &TrendCount{Value: count, N: count}
		set.Parks = parks
	}
	if count > 0 {
		set.ChangesRate = &TrendRatio{Value: round3(float64(changesRequested) / float64(count)), N: count}
	}
	if reviewed > 0 {
		set.ReviewPasses = &TrendRatio{Value: round3(float64(reviews) / float64(reviewed)), N: reviewed}
	}
	set.CycleTime = spread(cycles)
	set.ReviewToMerge = spread(waits)
	set.Verify = spread(gates)
	return set
}

// spread is the median and the nearest-rank 90th percentile of ds, nil for
// none. The median of an even count is the mean of the middle two, as the ETA's
// is; a percentile is always one of the samples, so a p90 of three runs is the
// slowest of them rather than a value no run took.
func spread(ds []time.Duration) *TrendSpread {
	if len(ds) == 0 {
		return nil
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	rank := int(math.Ceil(0.9*float64(len(sorted)))) - 1
	return &TrendSpread{Median: seconds(median(sorted)), P90: seconds(sorted[rank]), N: len(sorted)}
}

// seconds is d in seconds to the millisecond, which is as fine as a gate's
// recorded time goes.
func seconds(d time.Duration) float64 {
	return round3(d.Seconds())
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
