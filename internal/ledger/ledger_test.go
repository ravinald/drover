package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidateCodeByRejectsAuthorVerifying(t *testing.T) {
	err := ValidateCodeBy(LevelVerified, "sam", "--reviewer is sam")
	if err == nil {
		t.Fatal("an author verifying its own work must be rejected")
	}
	// The message has to say what to do instead and which entry matched, or
	// the caller just retries.
	for _, want := range []string{LevelClaimed, "sam"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection should name %q, got: %v", want, err)
		}
	}
}

func TestValidateCodeByAccepts(t *testing.T) {
	ok := []struct{ level, entry string }{
		{LevelClaimed, "sam"},
		{LevelReviewed, "sam"},
		{LevelClaimed, ""},
		{LevelReviewed, ""},
		{LevelVerified, ""},
	}
	for _, c := range ok {
		if err := ValidateCodeBy(c.level, c.entry, "why"); err != nil {
			t.Errorf("ValidateCodeBy(%q, %q) = %v, want nil", c.level, c.entry, err)
		}
	}
}

func TestValidateLevelRejectsTheOldNames(t *testing.T) {
	for _, old := range []string{"self", "review", "requirements", "thorough"} {
		err := ValidateLevel(old)
		if err == nil {
			t.Errorf("ValidateLevel(%q) accepted it", old)
			continue
		}
		for _, want := range Levels() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ValidateLevel(%q) does not name %q: %v", old, want, err)
			}
		}
	}
}

func TestParseCodeBy(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: "agent", want: []string{"agent"}},
		{in: "agent,sam", want: []string{"agent", "sam"}},
		{in: " agent , sam ", want: []string{"agent", "sam"}},
		{in: "", bad: true},
		{in: "agent,", bad: true},
		{in: "author", bad: true},
		{in: "independent", bad: true},
	} {
		got, err := ParseCodeBy(c.in)
		if (err != nil) != c.bad || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("ParseCodeBy(%q) = %v, %v; want %v, bad %v", c.in, got, err, c.want, c.bad)
		}
	}
}

// SameAs is the whole authorship rule, driven case by case: agent resolves per
// session against the item's implement rows, and a name is compared as
// written against --reviewer.
func TestSameAs(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement, Runner: "claude", Session: "impl"},
		{ID: "1", Stage: StageReview, Runner: "claude", Session: "rev"},
	}}
	launched := func(session string) Grader { return Grader{Launched: true, Session: session} }
	for _, c := range []struct {
		name  string
		g     Grader
		by    []string
		match string
	}{
		{name: "agent code, launched reviewer", g: launched("rev"), by: []string{"agent"}},
		{name: "agent code, the implementing session", g: launched("impl"), by: []string{"agent"}, match: "agent"},
		{name: "agent code, a launched session with no id", g: launched(""), by: []string{"agent"}, match: "agent"},
		{name: "agent code, the operator", g: Grader{}, by: []string{"agent"}},
		{name: "sam audited by sam", g: Grader{Name: "sam"}, by: []string{"sam"}, match: "sam"},
		{name: "agent,sam audited by sam", g: Grader{Name: "sam"}, by: []string{"agent", "sam"}, match: "sam"},
		{name: "agent,sam audited by a third name", g: Grader{Name: "dana"}, by: []string{"agent", "sam"}},
		{name: "a named author and a grader nobody named", g: Grader{}, by: []string{"sam"}, match: "sam"},
		{name: "a named author and a launched reviewer", g: launched("rev"), by: []string{"sam"}},
	} {
		if got, _ := l.SameAs("1", c.g, c.by); got != c.match {
			t.Errorf("%s: SameAs = %q, want %q", c.name, got, c.match)
		}
	}
}

// Completion is last-row-wins so that a withdrawal is an append rather than a
// rewrite. prompt.sh removed rows instead, which also destroyed the audit
// history for that id as a side effect.
func TestTombstoneWithdrawsCompletionWithoutLosingAudit(t *testing.T) {
	yes, no := true, false
	l := &Ledger{Rows: []Row{
		{ID: "16", Merged: &yes, MergedRunner: "claude", Date: "2026-07-14"},
		{ID: "16", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true, Date: "2026-07-20", Note: "checked"},
		{ID: "16", Merged: &no, Date: "2026-07-21"},
	}}
	s := l.State("16")
	if s.Merged {
		t.Error("tombstone must withdraw completion")
	}
	if s.Level != LevelReviewed || !s.Independent {
		t.Errorf("audit history must survive a withdrawal, got level %q by %q", s.Level, s.CodeBy)
	}
}

// A queued --auto merge stays outstanding until the item completes or moves to
// another pull request, in the order the rows were appended. An outcome that
// queues nothing is a drive that left the forge alone, and an end row is a
// snapshot written at exit, so neither starts or revives a queue.
func TestMergeQueuedFollowsTheOrderThingsHappened(t *testing.T) {
	yes, no, exit := true, false, 0
	end := func(drive string, o Outcome) Row {
		return Row{ID: DriveRowID, Drive: drive, Exit: &exit, Outcomes: []Outcome{o}}
	}
	queued := Row{ID: "1", PR: 7, MergeQueued: true}
	staleEnd := end("D1", Outcome{ID: "1", Outcome: "held", MergeQueued: true, PR: 7})
	cases := []struct {
		name   string
		rows   []Row
		wantPR int
		wantOK bool
	}{
		{"never queued", []Row{{ID: "1", PR: 7}, end("D1", Outcome{ID: "1", Outcome: "ready_to_merge"})}, 0, false},
		{"queued", []Row{{ID: "1", PR: 7}, queued}, 7, true},
		{"an end row alone queues nothing", []Row{{ID: "1", PR: 7}, end("D1", Outcome{ID: "1", Outcome: "ready_to_merge", MergeQueued: true, PR: 7})}, 0, false},
		{"then held", []Row{{ID: "1", PR: 7}, queued, end("D2", Outcome{ID: "1", Outcome: "held"})}, 7, true},
		{"then parked on the same pull request", []Row{{ID: "1", PR: 7}, queued, {ID: "1", PR: 7}, end("D2", Outcome{ID: "1", Outcome: "parked"})}, 7, true},
		{"then merged", []Row{{ID: "1", PR: 7}, queued, {ID: "1", Merged: &yes}}, 0, false},
		{"merged then withdrawn", []Row{{ID: "1", PR: 7}, queued, {ID: "1", Merged: &yes}, {ID: "1", Merged: &no}}, 0, false},
		{"then a new pull request", []Row{{ID: "1", PR: 7}, queued, {ID: "1", PR: 9}}, 0, false},
		{"replaced before a late end row", []Row{{ID: "1", PR: 7}, queued, {ID: "1", PR: 9}, staleEnd}, 0, false},
		{"merged before a late end row", []Row{{ID: "1", PR: 7}, queued, {ID: "1", Merged: &yes}, staleEnd}, 0, false},
		{"requeued on the new pull request", []Row{{ID: "1", PR: 7}, queued, {ID: "1", PR: 9}, {ID: "1", PR: 9, MergeQueued: true}}, 9, true},
		{"another item queued", []Row{{ID: "2", PR: 7}, {ID: "2", PR: 7, MergeQueued: true}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr, ok := (&Ledger{Rows: tc.rows}).MergeQueued("1")
			if pr != tc.wantPR || ok != tc.wantOK {
				t.Errorf("MergeQueued = %d, %v, want %d, %v", pr, ok, tc.wantPR, tc.wantOK)
			}
		})
	}
}

// The most recent audit wins, not the strongest: a sweep that claimed more than
// it checked has to be correctable downward.
func TestMostRecentAuditWins(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "20", Level: LevelVerified, CodeBy: CodeAgent, Independent: true, Date: "2026-07-23"},
		{ID: "20", Level: LevelClaimed, CodeBy: CodeAgent, Date: "2026-07-24", Note: "withdrawn"},
	}}
	if got := l.State("20").Level; got != LevelClaimed {
		t.Errorf("level = %q, want the most recent (%q)", got, LevelClaimed)
	}
}

func TestAppendEncodesHostileNotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	// A quote, a backslash, a tab and a newline: prompt.sh built rows with
	// printf and stripped only the first two, so the rest produced a line that
	// parsed as nothing and was skipped on the next read.
	note := "quoted \"x\" \\ tab\there\nsecond line"
	if err := Append(path, Row{ID: "B1", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true, Note: note}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(string(data)), "\n"); n != 0 {
		t.Fatalf("row spans %d newlines; it must be one line", n+1)
	}
	var back Row
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("appended row does not parse: %v", err)
	}
	if back.Note != note {
		t.Errorf("note round-trip lost content:\n got %q\nwant %q", back.Note, note)
	}
}

func TestAppendStampsDateAndIsAppendOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	yes := true
	for _, id := range []string{"1", "2", "3"} {
		if err := Append(path, Row{ID: id, Merged: &yes, MergedRunner: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(l.Rows))
	}
	if l.Rows[0].Date == "" {
		t.Error("Append must stamp a date when one is not supplied")
	}
}

// A row appended by hand with printf or an editor can leave the last line
// unterminated; the next append must not join it into one line parsing as
// neither, or the hand-written merged row reads as an item that never merged.
func TestAppendTerminatesAnUnterminatedLastLine(t *testing.T) {
	appends := map[string]func(string, Row) error{
		"Append": Append,
		"AppendUnless": func(path string, r Row) error {
			wrote, err := AppendUnless(path, r, func(*Ledger) bool { return false })
			if err == nil && !wrote {
				t.Error("AppendUnless reported no write with a skip returning false")
			}
			return err
		},
	}
	for name, appendRow := range appends {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.jsonl")
			if err := os.WriteFile(path, []byte(`{"id":"1","merged":true,"date":"2026-01-01"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := appendRow(path, Row{ID: "2", Level: LevelClaimed, CodeBy: CodeAgent, Note: "x"}); err != nil {
				t.Fatal(err)
			}
			l, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.Malformed) != 0 {
				t.Fatalf("malformed lines: %+v", l.Malformed)
			}
			if len(l.Rows) != 2 {
				t.Fatalf("got %d rows, want 2", len(l.Rows))
			}
			if !l.State("1").Merged {
				t.Error("the hand-written merged row no longer reads as merged")
			}
		})
	}
}

func TestAppendToEmptyOrMissingFileWritesOneLine(t *testing.T) {
	for name, seed := range map[string]bool{"empty": true, "missing": false} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.jsonl")
			if seed {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Append(path, Row{ID: "1", Level: LevelClaimed, CodeBy: CodeAgent}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if data[0] == '\n' {
				t.Error("file opens with a blank line")
			}
			if n := strings.Count(string(data), "\n"); n != 1 || data[len(data)-1] != '\n' {
				t.Errorf("file holds %d newlines, want exactly one terminated line: %q", n, data)
			}
		})
	}
}

func TestAppendAfterATerminatedLineAddsOnlyTheRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	before := []byte(`{"id":"1","merged":true,"date":"2026-01-01"}` + "\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Row{ID: "2", Level: LevelClaimed, CodeBy: CodeAgent, Date: "2026-01-02", TS: "2026-01-02T00:00:00Z"}
	if err := Append(path, r); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want = append(append(slices.Clone(before), want...), '\n')
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("file after append:\n got %q\nwant %q", got, want)
	}
}

// A truncated or hand-mangled line must be reported, not skipped. prompt.sh's
// `fromjson? // empty` made a lost row look identical to one never written.
func TestLoadReportsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	body := `{"id":"1","merged":true,"date":"2026-01-01"}
{"id":"2","done":tru
{"date":"2026-01-02"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rows) != 1 {
		t.Errorf("got %d good rows, want 1", len(l.Rows))
	}
	if len(l.Malformed) != 2 {
		t.Fatalf("got %d malformed, want 2 (a truncated line and one with no id)", len(l.Malformed))
	}
	if l.Malformed[0].Line != 2 || l.Malformed[1].Line != 3 {
		t.Errorf("malformed lines misnumbered: %+v", l.Malformed)
	}
}

func TestLoadMissingFileIsEmptyNotAnError(t *testing.T) {
	l, err := Load(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("a project with no completions yet is normal: %v", err)
	}
	if len(l.Rows) != 0 {
		t.Errorf("got %d rows from a missing file", len(l.Rows))
	}
}

// The stages resolve independently and last-wins, so re-running an item with a
// second runner corrects the record by appending rather than by rewriting.
func TestStageRowsResolveEachRunner(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement, Runner: "claude", Date: "2026-08-01"},
		{ID: "1", Stage: StageReview, Runner: "codex", Date: "2026-08-02"},
		{ID: "1", Stage: StageImplement, Runner: "gemini", Date: "2026-08-03"},
		{ID: "2", Stage: StageReview, Runner: "codex", Date: "2026-08-03"},
	}}
	if got := l.State("1"); got.Implementer != "gemini" || got.Reviewer != "codex" {
		t.Errorf("state = implementer %q, reviewer %q; want gemini, codex", got.Implementer, got.Reviewer)
	}
	// One stage recorded says nothing about the other.
	if got := l.State("2"); got.Implementer != "" || got.Reviewer != "codex" {
		t.Errorf("state = implementer %q, reviewer %q; want unknown, codex", got.Implementer, got.Reviewer)
	}
}

// Rows written before the stage existed are read unchanged, and report unknown.
// "Nobody recorded it" and "claude did it" are the two answers the stage rows
// exist to separate, so an old row must never be resolved into a runner name.
func TestRowsWithoutAStageReportUnknown(t *testing.T) {
	yes := true
	l := &Ledger{Rows: []Row{
		{ID: "16", Merged: &yes, MergedRunner: "claude", Date: "2026-07-14"},
		{ID: "16", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true, Date: "2026-07-20"},
	}}
	st := l.State("16")
	if st.Implementer != "" || st.Reviewer != "" {
		t.Errorf("implementer %q, reviewer %q; both must be unknown", st.Implementer, st.Reviewer)
	}
	if !st.Merged || st.MergedRunner != "claude" || st.Level != LevelReviewed {
		t.Errorf("an old row lost data it still carries: %+v", st)
	}
}

// A disposition is correctable until finish applies it, so newest wins per issue
// exactly as the audit ladder does. Applied is the exception: it records
// something that already happened on a tracker other people read, so a later
// row cannot un-say it.
func TestResolutionsFoldNewestPerIssue(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "F8", Issue: 47, Disposition: DispositionSatisfied, CodeBy: CodeAgent, Independent: true, Note: "first read", Date: "2026-09-01"},
		{ID: "F8", Issue: 100, Disposition: DispositionSatisfied, CodeBy: CodeAgent, Date: "2026-09-01"},
		{ID: "F8", Issue: 47, Disposition: DispositionPartial, CodeBy: CodeAgent, Independent: true, Note: "the migration is F9", Date: "2026-09-02"},
		{ID: "F8", Issue: 100, Applied: AppliedClosed, Date: "2026-09-02"},
		// Another item's rows must not reach F8's fold.
		{ID: "F9", Issue: 47, Disposition: DispositionUnrelated, Date: "2026-09-02"},
		// A completion row carries no issue and is not a disposition.
		{ID: "F8", Merged: boolp(true), Date: "2026-09-02"},
	}}

	got := l.Resolutions("F8")
	if len(got) != 2 {
		t.Fatalf("Resolutions: %+v, want 2", got)
	}
	if got[0].Issue != 47 || got[1].Issue != 100 {
		t.Fatalf("Resolutions are not ordered by issue: %+v", got)
	}
	if got[0].Disposition != DispositionPartial || got[0].Note != "the migration is F9" {
		t.Errorf("#47 did not fold to the newest row: %+v", got[0])
	}
	if got[0].Closes() {
		t.Errorf("#47 folded to %q, which must not close", got[0].Disposition)
	}
	if got[1].Applied != AppliedClosed {
		t.Errorf("#100 lost the applied row: %+v", got[1])
	}
	if !got[1].Closes() {
		t.Errorf("#100 folded to %q, want it to close", got[1].Disposition)
	}

	if _, ok := l.Resolution("F8", 999); ok {
		t.Error("Resolution reported a judgement nobody recorded")
	}
	if r, ok := l.Resolution("F8", 47); !ok || !r.Independent {
		t.Errorf("Resolution(47): %+v ok=%v", r, ok)
	}
}

// A lock row holds an issue for the lease and is never a write: Applied stays
// empty under it, so whatever counts pending issues still counts this one. A
// release gives it back at once, and a lock left by a finish that died gives it
// back when the lease runs out.
func TestALockHoldsWithoutApplying(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	l := &Ledger{Rows: []Row{
		{ID: "F8", Issue: 47, Disposition: DispositionPartial, CodeBy: CodeAgent, Independent: true, Date: "2026-10-15"},
		{ID: "F8", Issue: 47, Locking: at(-time.Minute), Date: "2026-10-15"},
		{ID: "F8", Issue: 100, Disposition: DispositionPartial, CodeBy: CodeAgent, Independent: true, Date: "2026-10-15"},
		{ID: "F8", Issue: 100, Locking: at(-time.Minute), Date: "2026-10-15"},
		{ID: "F8", Issue: 100, Locking: LockReleased, Date: "2026-10-15"},
		{ID: "F8", Issue: 103, Disposition: DispositionPartial, CodeBy: CodeAgent, Independent: true, Date: "2026-10-15"},
		{ID: "F8", Issue: 103, Locking: at(-LockLease), Date: "2026-10-15"},
	}}

	for _, c := range []struct {
		issue     int
		claimable bool
	}{{47, false}, {100, true}, {103, true}} {
		r, ok := l.Resolution("F8", c.issue)
		if !ok || r.Applied != "" {
			t.Errorf("#%d: %+v ok=%v, want a pending disposition", c.issue, r, ok)
		}
		if r.Lockable(now) != c.claimable {
			t.Errorf("#%d claimable=%v, want %v", c.issue, !c.claimable, c.claimable)
		}
	}
}

func boolp(b bool) *bool { return &b }

func TestCountsCountsRatherThanFolds(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "16", Stage: StageImplement, Runner: "claude"},
		{ID: "16", Stage: StageReview, Runner: "agy"},
		{ID: "16", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true},
		{ID: "16", Stage: StageImplement, Runner: "claude"},
		{ID: "16", Stage: StageReview, Runner: "agy"},
		{ID: "16", Level: LevelVerified, CodeBy: CodeAgent, Independent: true},
		{ID: "16", Stage: StageImplement, Runner: "claude"},
		{ID: "17", Stage: StageImplement, Runner: "claude"},
		{ID: "17", Level: LevelClaimed, CodeBy: CodeAgent},
	}}
	c := l.Counts("16")
	if got := c.Of(StageImplement); got != 3 {
		t.Errorf("implement launches = %d, want 3", got)
	}
	if got := c.Of(StageReview); got != 2 {
		t.Errorf("review launches = %d, want 2", got)
	}
	if c.Audits != 2 {
		t.Errorf("audits = %d, want 2", c.Audits)
	}
	// State folds the same rows and keeps one answer; the two readers must not
	// be confused for each other.
	if st := l.State("16"); st.Level != LevelVerified {
		t.Errorf("State still folds last-wins: level = %q, want %q", st.Level, LevelVerified)
	}
}

// A repeat is the reviewer saying two runs in a row were sent one finding and
// neither closed it. Two send-backs naming the same requirements with no
// still_open are two different defects under one number, which is progress.
func TestCountsStillOpenWantsTwoMarkedRowsSharingARequirement(t *testing.T) {
	type row struct{ reqs, open []int }
	for _, c := range []struct {
		name string
		rows []row
		want []int
	}{
		{"equal reqs, nothing marked", []row{{[]int{2}, nil}, {[]int{2}, nil}}, nil},
		{"both marked, shared", []row{{[]int{3}, []int{3}}, {[]int{3}, []int{3}}}, []int{3}},
		{"both marked, disjoint", []row{{[]int{3, 4}, []int{4}}, {[]int{3}, []int{3}}}, nil},
		{"only the last marked", []row{{[]int{3}, nil}, {[]int{3}, []int{3}}}, nil},
		{"an unmarked row between", []row{{[]int{3}, []int{3}}, {[]int{3}, nil}, {[]int{3}, []int{3}}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &Ledger{}
			for i, r := range c.rows {
				l.Rows = append(l.Rows, Row{ID: "16", Changes: []string{"a", "b", "c"}[i], Reqs: r.reqs, StillOpen: r.open})
			}
			l.Rows = append(l.Rows, Row{ID: "17", Changes: "other", Reqs: []int{3}, StillOpen: []int{3}})
			got := l.Counts("16")
			if got.StillOpen != (c.want != nil) || !slices.Equal(got.StillOpenReqs, c.want) {
				t.Errorf("StillOpen = %v %v, want %v", got.StillOpen, got.StillOpenReqs, c.want)
			}
			if got.RepeatedChanges {
				t.Error("distinct digests read as RepeatedChanges")
			}
		})
	}
}

func TestCountsIgnoresEveryOtherItem(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement},
		{ID: "2", Stage: StageImplement},
		{ID: "2", Stage: StageImplement},
	}}
	if got := l.Counts("1").Of(StageImplement); got != 1 {
		t.Errorf("counted %d implement rows for 1, want 1", got)
	}
}

func TestCountsReadsZeroForWhatNobodyWrote(t *testing.T) {
	// A stage no command writes today, and an id nothing mentions: both are
	// reads through a nil map, which is the escape hatch a third stage uses.
	if got := (&Ledger{}).Counts("nope").Of(StageReview); got != 0 {
		t.Errorf("empty ledger counted %d review launches, want 0", got)
	}
	l := &Ledger{Rows: []Row{{ID: "1", Stage: StageImplement}}}
	if got := l.Counts("1").Of("draft"); got != 0 {
		t.Errorf("counted %d draft launches, want 0", got)
	}
}

// LastBody is the latest digest in file order, whichever row carried it: a body
// row written after the review launched is drover's own later write, and a
// review row written after a body row is a new launch reading the body afresh.
// Rows carrying no digest, and other items' digests, leave it alone.
func TestCountsLastBodyIsTheLatestDigestAcrossBothRows(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Stage: StageImplement},
		{ID: "1", Stage: StageReview, Body: "aaaaaaaaaaaa"},
		{ID: "1", Body: "bbbbbbbbbbbb"},
		{ID: "1", Level: LevelVerified, CodeBy: CodeAgent, Independent: true},
		{ID: "2", Body: "ffffffffffff"},
		{ID: "1", Changes: "123456789abc"},
	}}
	if got := l.Counts("1").LastBody; got != "bbbbbbbbbbbb" {
		t.Errorf("after a body row: LastBody = %q, want the body row's digest", got)
	}
	l.Rows = append(l.Rows, Row{ID: "1", Stage: StageReview, Body: "cccccccccccc"})
	if got := l.Counts("1").LastBody; got != "cccccccccccc" {
		t.Errorf("after a later review row: LastBody = %q, want the review row's digest", got)
	}
	if got := (&Ledger{Rows: []Row{{ID: "1", Stage: StageReview}}}).Counts("1").LastBody; got != "" {
		t.Errorf("no digest anywhere: LastBody = %q, want empty", got)
	}
}

// The two cases are an item whose reviews wrote twenty-four disposition rows
// for two answers. Both are the ones a "keep the first row" rule gets wrong.
func TestSettledNeedsTwoTrailingIndependentAgreements(t *testing.T) {
	ind, auth := "independent", "author"
	for _, tc := range []struct {
		name    string
		rows    [][2]string // disposition, by
		want    string
		settled bool
	}{
		{"one pass is not agreement", [][2]string{{DispositionSatisfied, ind}}, DispositionSatisfied, false},
		{"two independent passes agree", [][2]string{{DispositionSatisfied, ind}, {DispositionSatisfied, ind}}, DispositionSatisfied, true},
		{"an author cannot be the second", [][2]string{{DispositionSatisfied, ind}, {DispositionSatisfied, auth}}, DispositionSatisfied, false},
		{"#256: an early wrong answer is corrected, then agreed", [][2]string{{DispositionPartial, ind}, {DispositionSatisfied, ind}, {DispositionSatisfied, ind}}, DispositionSatisfied, true},
		{"a later disagreement unfreezes it", [][2]string{{DispositionSatisfied, ind}, {DispositionSatisfied, ind}, {DispositionPartial, ind}}, DispositionPartial, false},
		{"an author disagreeing also unfreezes it", [][2]string{{DispositionSatisfied, ind}, {DispositionSatisfied, ind}, {DispositionPartial, auth}}, DispositionPartial, false},
		{"authorship unrecorded is not independence", [][2]string{{DispositionSatisfied, ""}, {DispositionSatisfied, ""}}, DispositionSatisfied, false},
		{"#257: an author overwrote it and two passes put it back", [][2]string{{DispositionUnrelated, ind}, {DispositionSatisfied, auth}, {DispositionUnrelated, ind}, {DispositionUnrelated, ind}}, DispositionUnrelated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var l Ledger
			for _, r := range tc.rows {
				row := Row{ID: "1", Issue: 47, Disposition: r[0]}
				if r[1] != "" {
					row.CodeBy, row.Independent = CodeAgent, r[1] == ind
				}
				l.Rows = append(l.Rows, row)
			}
			got := l.Resolutions("1")
			if len(got) != 1 {
				t.Fatalf("folded to %d resolutions, want 1", len(got))
			}
			if got[0].Disposition != tc.want {
				t.Errorf("disposition = %q, want %q", got[0].Disposition, tc.want)
			}
			if got[0].Settled != tc.settled {
				t.Errorf("settled = %v, want %v", got[0].Settled, tc.settled)
			}
		})
	}
}

// Applied describes what reached a tracker other people read; settled describes
// agreement inside this file. Two answers with two different remedies, so they
// stay two fields.
func TestAppliedIsNotSettled(t *testing.T) {
	l := Ledger{Rows: []Row{
		{ID: "1", Issue: 47, Disposition: DispositionSatisfied, CodeBy: CodeAgent, Independent: true},
		{ID: "1", Issue: 47, Applied: AppliedClosed},
	}}
	r, _ := l.Resolution("1", 47)
	if r.Applied != AppliedClosed {
		t.Errorf("applied = %q, want %q", r.Applied, AppliedClosed)
	}
	if r.Settled {
		t.Error("one independent pass plus a tracker write reads as settled")
	}
}

// The pass row folds last-wins, and a later drive correcting an earlier result
// is the whole reason it does.
func TestTheMostRecentPassWins(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Pass: "changes_requested", Date: "2026-09-01"},
		{ID: "1", Pass: "passed", Date: "2026-09-02"},
		{ID: "2", Pass: "no_record", Date: "2026-09-02"},
	}}
	token, date, stale, ok := l.Pass("1")
	if !ok {
		t.Fatal("a recorded pass must read back")
	}
	if token != "passed" || date != "2026-09-02" {
		t.Errorf("got %q on %s, want passed on 2026-09-02", token, date)
	}
	if stale {
		t.Error("nothing was appended after the pass row, so it is not stale")
	}
	if token, _, _, _ := l.Pass("2"); token != "no_record" {
		t.Errorf("a park is recorded too, got %q", token)
	}
}

// An id no drive has reached and an id a drive parked want opposite answers, so
// the absence has to be distinguishable from any token.
func TestNoPassRowIsNotAPark(t *testing.T) {
	l := &Ledger{Rows: []Row{{ID: "1", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true, Date: "2026-09-01"}}}
	token, _, _, ok := l.Pass("1")
	if ok {
		t.Errorf("an item with only an audit row has no recorded pass, got %q", token)
	}
	if _, _, _, ok := l.Pass("never-seen"); ok {
		t.Error("an id the ledger has never heard of has no recorded pass")
	}
}

// The result stands as a record and stops being a statement about the branch
// the moment the branch moves.
func TestAPassGoesStaleWhenTheItemRunsAgain(t *testing.T) {
	base := []Row{
		{ID: "1", Changes: "abc123", Date: "2026-09-01"},
		{ID: "1", Pass: "passed", Date: "2026-09-02"},
	}
	for _, after := range []Row{
		{ID: "1", Stage: StageImplement, Runner: "claude", Date: "2026-09-03"},
		{ID: "1", Level: LevelReviewed, CodeBy: CodeAgent, Independent: true, Date: "2026-09-03"},
		{ID: "1", Changes: "def456", Date: "2026-09-03"},
	} {
		l := &Ledger{Rows: append(append([]Row{}, base...), after)}
		if _, _, stale, _ := l.Pass("1"); !stale {
			t.Errorf("a %+v row after the pass row leaves the result stale", after)
		}
	}
	// Another item's work says nothing about this one's branch.
	l := &Ledger{Rows: append(append([]Row{}, base...),
		Row{ID: "2", Stage: StageImplement, Runner: "claude", Date: "2026-09-03"})}
	if _, _, stale, _ := l.Pass("1"); stale {
		t.Error("a sibling item's launch must not stale this item's pass")
	}
}

// The package promises a row can be appended by hand with no binary, so the
// sixth shape has to survive being typed.
func TestAHandWrittenPassRowParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	body := `{"id":"1","review_result":"no_digest","date":"2026-09-02"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Malformed) != 0 {
		t.Fatalf("a hand-written pass row must parse: %+v", l.Malformed)
	}
	if token, _, _, ok := l.Pass("1"); !ok || token != "no_digest" {
		t.Errorf("got %q ok=%v, want no request for changes", token, ok)
	}
}

// A ledger written before the pull request row reads unchanged and answers
// none; once rows exist the latest wins, and the branch/head row beside it is
// neither a pull request nor disturbed by one.
func TestPRFoldsLastRowWinsAndOlderLedgersRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	old := `{"id":"B51","stage":"implement","runner":"claude","date":"2026-10-14"}
{"id":"B51","branch":"bug/b51","head":"c0ffee","date":"2026-10-14"}
{"id":"B51","merged":true,"date":"2026-10-15"}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil || len(l.Malformed) > 0 {
		t.Fatalf("an older ledger must read: %v %v", err, l.Malformed)
	}
	if n, _ := l.PR("B51"); n != 0 {
		t.Errorf("PR = %d on a ledger with no pull request row, want 0", n)
	}
	for _, r := range []Row{{ID: "B51", Branch: "bug/b51", PR: 235}, {ID: "B51", Branch: "bug/b51", PR: 236}} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	if l, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if n, branch := l.PR("B51"); n != 236 || branch != "bug/b51" {
		t.Errorf("PR = %d on %q, want 236 on bug/b51", n, branch)
	}
	if got := l.Heads("bug/b51"); len(got) != 1 || got[0] != "c0ffee" {
		t.Errorf("Heads = %v, want the one head row alone", got)
	}
}

// A clear discharges a standing pass only when it follows both the pass and the
// audit that pass read; an invariant or link digest in the same place says nothing
// about the block.
func TestClearedSincePass(t *testing.T) {
	audit := Row{ID: "1", Level: LevelVerified, CodeBy: CodeAgent, Independent: true, Date: "2026-10-15"}
	pass := Row{ID: "1", Pass: "findings_open", Date: "2026-10-15"}
	cleared := Row{ID: "1", Body: "aaaaaaaaaaaa", Cleared: true, Date: "2026-10-16"}
	digest := Row{ID: "1", Body: "aaaaaaaaaaaa", Date: "2026-10-16"}
	for _, c := range []struct {
		name string
		rows []Row
		want bool
	}{
		{"a clear after the pass and its audit", []Row{audit, pass, cleared}, true},
		{"a clear before the pass", []Row{cleared, audit, pass}, false},
		{"a plain body digest after the pass", []Row{audit, pass, digest}, false},
		{"a clear with no pass row", []Row{audit, cleared}, false},
		{"another item's clear", []Row{audit, pass, {ID: "2", Body: "aaaaaaaaaaaa", Cleared: true, Date: "2026-10-16"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (&Ledger{Rows: c.rows}).ClearedSincePass("1"); got != c.want {
				t.Errorf("ClearedSincePass = %v, want %v", got, c.want)
			}
		})
	}
}

// NextDrive reads the highest drive any row names and writes nothing, so a
// drive refused before its first row leaves a project with no ledger as it
// found it.
func TestNextDriveWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if id, err := NextDrive(path); err != nil || id != "D1" {
		t.Fatalf("NextDrive on no ledger = %q %v, want D1", id, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("NextDrive created the ledger: %v", err)
	}
	if err := Append(path, Row{ID: "1", Drive: "D3", Date: Today()}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if id, err := NextDrive(path); err != nil || id != "D4" {
			t.Fatalf("NextDrive past D3 = %q %v, want D4", id, err)
		}
	}
}

// Only a review's changes rows are rounds under max_review_rounds. A row with
// no source was written before the source existed, when only request-changes
// wrote changes rows, so it still counts as one; drive's own send-backs are
// counted apart by source and move none of the fields a review's rows answer.
func TestCountsKeepReviewRoundsApartFromDriveSendBacks(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Changes: "old", Reqs: []int{2}},
		{ID: "1", Changes: "gate-a", ChangesSource: ChangesVerify},
		{ID: "1", Changes: "gate-a", ChangesSource: ChangesVerify},
		{ID: "1", Changes: "review", ChangesSource: ChangesReview, Reqs: []int{3}, Date: "2026-10-29"},
		{ID: "1", Changes: "trunk_conflict abc", ChangesSource: ChangesTrunkConflict},
		{ID: "1", Changes: "seq", ChangesSource: ChangesNumberedFiles},
		{ID: "2", Changes: "elsewhere"},
	}}
	c := l.Counts("1")
	if c.ChangesRequested != 2 {
		t.Errorf("ChangesRequested = %d, want 2: the sourceless row and the review's", c.ChangesRequested)
	}
	if c.VerifySendBacks != 2 || c.ConflictSendBacks != 1 || c.NumberedSendBacks != 1 {
		t.Errorf("send-backs verify %d, conflict %d, numbered %d; want 2, 1 and 1",
			c.VerifySendBacks, c.ConflictSendBacks, c.NumberedSendBacks)
	}
	// Two identical verify rows are a gate failing twice, not a review repeating.
	if c.RepeatedChanges || !slices.Equal(c.Reqs, []int{3}) || c.LastChanges != "2026-10-29" {
		t.Errorf("review fields read drive's rows: repeated %v, reqs %v, last %q", c.RepeatedChanges, c.Reqs, c.LastChanges)
	}

	// With max_review_rounds = 2, two verify send-backs and one review send-back
	// leave the next review's request under the cap.
	under := &Ledger{Rows: []Row{
		{ID: "1", Changes: "v1", ChangesSource: ChangesVerify},
		{ID: "1", Changes: "v2", ChangesSource: ChangesVerify},
		{ID: "1", Changes: "r1", ChangesSource: ChangesReview},
	}}
	if n := under.Counts("1").ChangesRequested; n >= 2 {
		t.Errorf("ChangesRequested = %d, want under a cap of 2", n)
	}
}

func TestE92OnlyReviewOrAbsentSourceConsumesReviewRounds(t *testing.T) {
	l := &Ledger{Rows: []Row{
		{ID: "1", Changes: "old"},
		{ID: "1", Changes: "review", ChangesSource: ChangesReview},
		{ID: "1", Changes: "other", ChangesSource: "external_check"},
	}}
	if got := l.Counts("1").ChangesRequested; got != 2 {
		t.Fatalf("review rounds = %d, want 2 (review and absent source only)", got)
	}
}
