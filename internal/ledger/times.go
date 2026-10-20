package ledger

import "time"

// Times is when one item's latest run moved, read from the ts rows carry. A
// zero time is absent rather than unknown midnight: a row written before ts
// existed, or by hand without one, has a date and no time of day.
type Times struct {
	// Started is the first stage row of the latest run. A run opens at the
	// first stage row after a completion or a tombstone, so an item reopened by
	// undone reports when the work resumed, not when it first began.
	Started time.Time
	// Stage is the latest stage row's stage, and StageStarted its time. Both
	// are empty when that row carries no ts.
	Stage        string
	StageStarted time.Time
	// Finished is the completion row closing the latest run, zero while the
	// run is open or once a tombstone withdrew it.
	Finished time.Time
}

// Times reads the latest run's times for one id.
func (l *Ledger) Times(id string) Times {
	var t Times
	var first *Row
	closed := false
	for i := range l.Rows {
		r := &l.Rows[i]
		if r.ID != id {
			continue
		}
		if r.Stage != "" {
			if first == nil || closed {
				first, closed = r, false
				t.Finished = time.Time{}
			}
			t.Stage, t.StageStarted = "", time.Time{}
			if at, ok := r.At(); ok {
				t.Stage, t.StageStarted = r.Stage, at
			}
		}
		if r.Merged != nil {
			closed = true
			t.Finished = time.Time{}
			if at, ok := r.At(); ok && *r.Merged {
				t.Finished = at
			}
		}
	}
	if first != nil {
		t.Started, _ = first.At()
	}
	return t
}

// StageDurations is how long each stage took, across every item in the file,
// for the stage rows that carry ts. A stage ends at the next stage, pass or
// completion row for the same item, which is the next thing a drive writes once
// that stage's session returns. A stage whose end carries no ts, or that has no
// end yet, contributes nothing: a duration is measured or it is left out.
func (l *Ledger) StageDurations() map[string][]time.Duration {
	out := map[string][]time.Duration{}
	for i, r := range l.Rows {
		if r.Stage == "" {
			continue
		}
		start, ok := r.At()
		if !ok {
			continue
		}
		for _, next := range l.Rows[i+1:] {
			if next.ID != r.ID || (next.Stage == "" && next.Pass == "" && next.Merged == nil) {
				continue
			}
			if end, ok := next.At(); ok && !end.Before(start) {
				out[r.Stage] = append(out[r.Stage], end.Sub(start))
			}
			break
		}
	}
	return out
}
