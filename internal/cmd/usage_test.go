package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
)

const usageResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":0.5,"duration_ms":9000,"result":"done","session_id":"%s","usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":33,"cache_read_input_tokens":44},"modelUsage":{"claude-opus-5-5":{}}}`

// usageRows is the usage rows on disk.
func usageRows(t *testing.T, root string) []ledger.Row {
	t.Helper()
	l, err := ledger.Load(filepath.Join(root, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l.Usage
}

func headlessFixture(t *testing.T) *session {
	t.Helper()
	// Package flag state outlives the test that set it.
	runRunner, runHeadless, runDry = "", false, false
	t.Cleanup(func() { runRunner, runHeadless, runDry = "", false, false })
	s := fixture(t, `
[runners.stub]
interactive = ["stub-agent"]
headless    = ["stub-agent"]
`)
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	t.Chdir(dir)
	return s
}

func TestHeadlessRunRecordsItsUsage(t *testing.T) {
	s := headlessFixture(t)
	stubRunner(t, stubPATH(t), "stub-agent", fmt.Sprintf(usageResult, "sess-run"))

	if err := execute(t, "run", "1", "--runner", "stub", "--headless"); err != nil {
		t.Fatalf("run: %v", err)
	}
	rows := usageRows(t, s.cfg.Root)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	u := rows[0]
	if *u.Usage != (runlog.Usage{Input: 11, Output: 22, CacheCreate: 33, CacheRead: 44}) {
		t.Errorf("tokens = %+v, want the result event's", *u.Usage)
	}
	if u.ID != "1" || u.Stage != ledger.StageImplement || u.Source != ledger.SourceHeadless {
		t.Errorf("id %q stage %q source %q", u.ID, u.Stage, u.Source)
	}
	if u.SessionID != "sess-run" || u.Model != "claude-opus-5-5" || u.Turns != 4 || u.CostUSD != 0.5 || u.DurationMS != 9000 {
		t.Errorf("row = %+v", u)
	}
	if u.TS == "" || u.Date == "" || !strings.HasSuffix(u.Log, ".jsonl") || strings.Contains(u.Log, "/") {
		t.Errorf("ts %q date %q log %q: want stamps and the log's basename", u.TS, u.Date, u.Log)
	}
	l, err := ledger.Load(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var stage string
	for _, r := range l.Rows {
		if r.Stage == ledger.StageImplement {
			stage = r.Session
		}
	}
	if stage == "" || u.Session != stage {
		t.Errorf("usage row session %q, want the stage row's %q", u.Session, stage)
	}
}

// Tokens are spent whether or not the session succeeded.
func TestFailedHeadlessRunStillRecordsUsage(t *testing.T) {
	s := headlessFixture(t)
	t.Setenv("DROVER_TEST_EMIT_FAIL", fmt.Sprintf(usageResult, "sess-fail"))
	stub(t, filepath.Join(stubPATH(t), "stub-agent"), "#!/bin/sh\necho \"$DROVER_TEST_EMIT_FAIL\"\nexit 1\n")

	if err := execute(t, "run", "1", "--runner", "stub", "--headless"); err == nil {
		t.Fatal("a runner exiting 1 must fail the run")
	}
	if rows := usageRows(t, s.cfg.Root); len(rows) != 1 || rows[0].SessionID != "sess-fail" {
		t.Fatalf("usage rows = %+v, want the failed session's", rows)
	}
}

func TestHeadlessRunWithoutUsageRecordsNone(t *testing.T) {
	s := headlessFixture(t)
	stubRunner(t, stubPATH(t), "stub-agent", `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"x","session_id":"s"}`)

	if err := execute(t, "run", "1", "--runner", "stub", "--headless"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rows := usageRows(t, s.cfg.Root); len(rows) != 0 {
		t.Fatalf("usage rows = %+v, want none for a result with no usage object", rows)
	}
}

// Every existing reader ignores a usage row: check finds nothing new, and the
// stage and pass counts status reports stay where they were.
func TestUsageRowsChangeNoReader(t *testing.T) {
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, "ledger.jsonl")
	for _, r := range []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Runner: "claude", Session: "d1"},
		{ID: "1", Pass: "passed"},
	} {
		if err := ledger.Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(s.cfg.Root)
	t.Cleanup(func() { statusJSON, statusNoTracker, checkJSON = false, false, false })
	read := func() (status, check string, checkErr error) {
		status = capture(t, func() {
			if err := execute(t, "status", "--json", "--no-tracker"); err != nil {
				t.Fatalf("status: %v", err)
			}
		})
		check = capture(t, func() { checkErr = execute(t, "check", "--json") })
		return status, check, checkErr
	}
	statusBefore, checkBefore, errBefore := read()

	u := runlog.Usage{Input: 1, Output: 2}
	for _, r := range []ledger.Row{
		{ID: "1", Stage: ledger.StageImplement, Session: "d1", Usage: &u, Source: ledger.SourceHeadless, SessionID: "a"},
		{ID: "1", Stage: ledger.StageReview, Usage: &u, Source: ledger.SourceHeadless, SessionID: "b"},
		{ID: "", Usage: &u, Source: ledger.SourceInteractive, SessionID: "c"},
	} {
		if err := ledger.Append(path, r); err != nil {
			t.Fatalf("append %+v: %v", r, err)
		}
	}
	statusAfter, checkAfter, errAfter := read()

	if Code(errBefore) != Code(errAfter) || checkBefore != checkAfter {
		t.Errorf("check changed with usage rows in the ledger:\nbefore (%v) %s\nafter (%v) %s", errBefore, checkBefore, errAfter, checkAfter)
	}
	if statusBefore != statusAfter {
		t.Errorf("status changed with usage rows in the ledger:\nbefore %s\nafter %s", statusBefore, statusAfter)
	}
	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c := l.Counts("1"); c.Of(ledger.StageImplement) != 1 || c.Of(ledger.StageReview) != 0 || len(l.Malformed) != 0 {
		t.Errorf("counts %+v malformed %v", c, l.Malformed)
	}
	if token, _, stale, _ := l.Pass("1"); token != "passed" || stale {
		t.Errorf("pass %q stale %v: a usage row must not stale a pass", token, stale)
	}
}

func TestUsageWithNoFlagPrintsHelp(t *testing.T) {
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	if err := execute(t, "usage"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Root, "ledger.jsonl")); err == nil {
		t.Error("usage with no flag wrote a ledger")
	}
}

func logAt(t *testing.T, dir, name, body string) {
	t.Helper()
	write(t, filepath.Join(dir, name), body)
}

func TestBackfillWritesEachLogOnce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s := fixture(t, "")
	s.cfg.Paths.LogPrefix = "ex-"
	dir := s.cfg.LogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logAt(t, dir, "ex-01-first.20261001-093000.000000000.jsonl", fmt.Sprintf(usageResult, "s-impl")+"\n")
	logAt(t, dir, "ex-01-first-review.20261001-100000.000000000.jsonl", fmt.Sprintf(usageResult, "s-review")+"\n")
	logAt(t, dir, "ex-07-gone.20261001-110000.000000000.jsonl", fmt.Sprintf(usageResult, "s-gone")+"\n")
	logAt(t, dir, "ex-01-first.20261002-090000.000000000.jsonl", `{"type":"system","subtype":"init"}`+"\n")
	logAt(t, dir, "ex-01-first.20261003-090000.000000000.jsonl", fmt.Sprintf(usageResult, "s-known")+"\n")
	known := runlog.Usage{Input: 1}
	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	if err := ledger.Append(path, ledger.Row{ID: "1", Usage: &known, SessionID: "s-known", Source: ledger.SourceHeadless}); err != nil {
		t.Fatal(err)
	}
	s = reload(t, s)

	dry, err := s.backfill(true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Read != 5 || dry.Wrote != 3 || len(usageRows(t, s.cfg.Root)) != 1 {
		t.Fatalf("dry run: %+v, rows on disk %d; want 5 read, 3 counted and nothing written", dry, len(usageRows(t, s.cfg.Root)))
	}

	s = reload(t, s)
	first, err := s.backfill(false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Read != 5 || first.Wrote != 3 || first.Skipped[skipNoResult] != 1 || first.Skipped[skipRecorded] != 1 {
		t.Errorf("first run: %+v", first)
	}
	var out strings.Builder
	first.write(&out, "backfill: logs", false)
	if got := out.String(); !strings.Contains(got, "read 5, wrote 3, skipped 2 (1 already recorded, 1 no result event)") {
		t.Errorf("printed %q", got)
	}

	bySession := map[string]ledger.Row{}
	for _, r := range usageRows(t, s.cfg.Root) {
		bySession[r.SessionID] = r
	}
	at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local)
	if r := bySession["s-impl"]; r.ID != "1" || r.Stage != ledger.StageImplement || r.TS != at.Format(time.RFC3339) || r.Date != "2026-10-01" ||
		r.Log != "ex-01-first.20261001-093000.000000000.jsonl" {
		t.Errorf("implement row = %+v", r)
	}
	if r := bySession["s-review"]; r.ID != "1" || r.Stage != ledger.StageReview {
		t.Errorf("review row = %+v", r)
	}
	if r, ok := bySession["s-gone"]; !ok || r.ID != "" {
		t.Errorf("a stem naming no item must record id \"\", got %+v", r)
	}

	s = reload(t, s)
	second, err := s.backfill(false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote != 0 || second.Skipped[skipRecorded] != 4 {
		t.Errorf("second run: %+v, want nothing written", second)
	}
}

func reload(t *testing.T, s *session) *session {
	t.Helper()
	l, err := ledger.Load(s.cfg.Abs(s.cfg.Paths.Ledger))
	if err != nil {
		t.Fatal(err)
	}
	s.ledger = l
	return s
}

// transcriptLine is one Claude Code transcript line.
func transcriptLine(kind, session, msgID string, at time.Time, in, cacheRead, out int) string {
	ts := at.UTC().Format(time.RFC3339Nano)
	if kind == "user" {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":"go"}}`, session, ts)
	}
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":0,"output_tokens":%d}}}`,
		session, ts, msgID, in, cacheRead, out)
}

func TestSyncRecordsInteractiveSessionsPerDay(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	s := fixture(t, "")
	path := s.cfg.Abs(s.cfg.Paths.Ledger)

	rootDir := filepath.Join(claude, "projects", transcriptSlug(s.cfg.Root))
	trees, err := s.projectTrees()
	if err != nil {
		t.Fatal(err)
	}
	treeDir := filepath.Join(claude, "projects", transcriptSlug(filepath.Join(trees, "01")))
	other := filepath.Join(claude, "projects", transcriptSlug(s.cfg.Root+"-other"))
	for _, d := range []string{rootDir, treeDir, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	day1 := time.Date(2026, 10, 1, 23, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 10, 2, 8, 0, 0, 0, time.Local)
	long := strings.Join([]string{
		transcriptLine("user", "long", "", day1, 0, 0, 0),
		transcriptLine("assistant", "long", "m1", day1.Add(time.Minute), 10, 100, 5),
		// The same message again, as Claude Code writes one line per block.
		transcriptLine("assistant", "long", "m1", day1.Add(time.Minute), 10, 100, 5),
		transcriptLine("assistant", "long", "m2", day1.Add(2*time.Minute), 20, 200, 6),
		`{"type":"assistant","sessionId":"long","timest`,
		transcriptLine("user", "long", "", day2, 0, 0, 0),
		transcriptLine("assistant", "long", "m3", day2.Add(time.Minute), 1, 50, 7),
		// Today's message waits for the day to end.
		transcriptLine("assistant", "long", "m4", time.Now(), 1, 1, 1),
	}, "\n") + "\n"
	write(t, filepath.Join(rootDir, "long.jsonl"), long)
	write(t, filepath.Join(treeDir, "headless.jsonl"), transcriptLine("assistant", "headless", "h1", day1, 5, 5, 5)+"\n")
	write(t, filepath.Join(other, "elsewhere.jsonl"), transcriptLine("assistant", "elsewhere", "e1", day1, 5, 5, 5)+"\n")
	covered := runlog.Usage{Input: 5}
	if err := ledger.Append(path, ledger.Row{ID: "1", Usage: &covered, SessionID: "headless", Source: ledger.SourceHeadless}); err != nil {
		t.Fatal(err)
	}
	s = reload(t, s)

	first, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first.Read != 2 || first.Wrote != 2 || first.Unparsed != 1 || first.Pending != 1 || first.Skipped[skipHeadless] != 1 {
		t.Fatalf("first sync: %+v", first)
	}

	byDate := map[string]ledger.Row{}
	for _, r := range usageRows(t, s.cfg.Root) {
		if r.Source == ledger.SourceInteractive {
			byDate[r.Date] = r
		}
	}
	if len(byDate) != 2 {
		t.Fatalf("interactive rows by date = %v", byDate)
	}
	d1 := byDate["2026-10-01"]
	if *d1.Usage != (runlog.Usage{Input: 30, Output: 11, CacheRead: 300}) || d1.ContextMax != 220 || d1.ID != "" || d1.SessionID != "long" {
		t.Errorf("day 1 = %+v usage %+v", d1, *d1.Usage)
	}
	if d1.FirstTS != day1.Format(time.RFC3339) || d1.TS != day1.Add(2*time.Minute).Format(time.RFC3339) || d1.Started != day1.Format(time.RFC3339) {
		t.Errorf("day 1 first_ts %q ts %q started %q", d1.FirstTS, d1.TS, d1.Started)
	}
	d2 := byDate["2026-10-02"]
	if *d2.Usage != (runlog.Usage{Input: 1, Output: 7, CacheRead: 50}) || d2.Started != day1.Format(time.RFC3339) || d2.FirstTS != day2.Format(time.RFC3339) {
		t.Errorf("day 2 = %+v usage %+v", d2, *d2.Usage)
	}

	s = reload(t, s)
	second, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote != 0 || second.Bytes != 0 {
		t.Errorf("second sync over an unchanged directory: %+v, want no row and no byte read", second)
	}

	// Tomorrow, the held day is over and lands without rereading a byte.
	s = reload(t, s)
	third, err := s.syncUsage(false, time.Now().AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if third.Wrote != 1 || third.Bytes != 0 || third.Pending != 0 {
		t.Errorf("sync the next day: %+v, want today's held day written from state", third)
	}
}

func TestSyncDryRunWritesNothing(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	s := fixture(t, "")
	dir := filepath.Join(claude, "projects", transcriptSlug(s.cfg.Root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "x.jsonl"), transcriptLine("assistant", "x", "m", time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), 1, 1, 1)+"\n")

	tally, err := s.syncUsage(true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tally.Wrote != 1 || len(usageRows(t, s.cfg.Root)) != 0 {
		t.Errorf("dry run %+v wrote rows", tally)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Root, usageSyncName)); err == nil {
		t.Error("dry run wrote the state file")
	}
}
