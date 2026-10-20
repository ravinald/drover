package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A row written now carries the instant beside the day, and the instant parses
// as RFC 3339 with an offset: a reader computing a duration from two rows needs
// both, and a date alone cannot order two rows written the same day.
func TestRowsWrittenNowCarryTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	before := time.Now().Add(-time.Second)
	if err := Append(path, Row{ID: "1", Stage: StageImplement}); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, Row{ID: "1", Level: LevelClaimed, CodeBy: CodeAgent, Date: Today()}); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range l.Rows {
		at, ok := r.At()
		if !ok {
			t.Fatalf("row %d carries ts %q, want an RFC 3339 time", i, r.TS)
		}
		if at.Before(before) || at.After(time.Now().Add(time.Second)) {
			t.Errorf("row %d ts %s is not the time it was written", i, r.TS)
		}
		if r.Date != at.Format("2006-01-02") {
			t.Errorf("row %d date %s disagrees with ts %s", i, r.Date, r.TS)
		}
	}
}

// A ledger older than ts mixes with one written after it. Every row still
// loads, and a time drawn from a row carrying no ts is reported absent rather
// than as midnight on its date.
func TestMixedRowsLoadAndDayOnlyTimesReadAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	lines := `{"id":"1","stage":"implement","runner":"r","date":"2026-01-01"}
{"id":"1","stage":"review","runner":"r","date":"2026-01-02","ts":"2026-01-02T10:00:00+02:00"}
{"id":"1","merged":true,"date":"2026-01-02"}
{"id":"2","stage":"implement","runner":"r","date":"2026-01-03","ts":"2026-01-03T09:00:00Z"}
{"id":"2","stage":"review","runner":"r","date":"2026-01-03"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) != 5 || len(l.Malformed) != 0 {
		t.Fatalf("loaded %d rows and %d malformed, want 5 and 0", len(l.Rows), len(l.Malformed))
	}

	one := l.Times("1")
	if !one.Started.IsZero() || !one.Finished.IsZero() {
		t.Errorf("item 1 started %v finished %v; both rows are day-only and must read absent", one.Started, one.Finished)
	}
	if one.Stage != StageReview || one.StageStarted.Format(time.RFC3339) != "2026-01-02T10:00:00+02:00" {
		t.Errorf("item 1 stage %q at %v, want review at its ts", one.Stage, one.StageStarted)
	}

	two := l.Times("2")
	if two.Started.Format(time.RFC3339) != "2026-01-03T09:00:00Z" {
		t.Errorf("item 2 started %v, want its first stage row's ts", two.Started)
	}
	if two.Stage != "" || !two.StageStarted.IsZero() {
		t.Errorf("item 2's latest stage row is day-only, so stage reads absent; got %q at %v", two.Stage, two.StageStarted)
	}
}

// A run opens at the first stage row after a completion or a tombstone, so an
// item reopened by undone reports the resumed work, and its old completion no
// longer reads as its finish.
func TestTimesFollowTheLatestRun(t *testing.T) {
	yes, no := true, false
	at := func(m int) string { return time.Date(2026, 1, 1, 9, m, 0, 0, time.UTC).Format(time.RFC3339) }
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement, TS: at(0)},
		{ID: "1", Stage: StageReview, TS: at(10)},
		{ID: "1", Merged: &yes, TS: at(20)},
	}}
	if got := l.Times("1"); got.Started.Minute() != 0 || got.Finished.Minute() != 20 {
		t.Errorf("first run: started %v finished %v, want :00 and :20", got.Started, got.Finished)
	}
	l.Rows = append(l.Rows, Row{ID: "1", Merged: &no, TS: at(30)}, Row{ID: "1", Stage: StageImplement, TS: at(40)})
	got := l.Times("1")
	if got.Started.Minute() != 40 || !got.Finished.IsZero() || got.Stage != StageImplement {
		t.Errorf("reopened run: started %v finished %v stage %q, want :40, absent, implement", got.Started, got.Finished, got.Stage)
	}
}

// A stage lasts until the next stage, pass or completion row for its item, and
// rows of other shapes in between do not cut it short.
func TestStageDurationsMeasureToTheNextBoundary(t *testing.T) {
	at := func(m int) string { return time.Date(2026, 1, 1, 9, m, 0, 0, time.UTC).Format(time.RFC3339) }
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement, TS: at(0)},
		{ID: "2", Stage: StageImplement, TS: at(1)},
		{ID: "1", Body: "abc", TS: at(3)},
		{ID: "1", Stage: StageReview, TS: at(7)},
		{ID: "1", Pass: "passed", TS: at(9)},
		{ID: "2", Stage: StageReview},
	}}
	d := l.StageDurations()
	if got := d[StageImplement]; len(got) != 1 || got[0] != 7*time.Minute {
		t.Errorf("implement durations %v, want [7m]: item 2's ends at a row with no ts", got)
	}
	if got := d[StageReview]; len(got) != 1 || got[0] != 2*time.Minute {
		t.Errorf("review durations %v, want [2m]", got)
	}
}
