package cmd

import (
	"encoding/json"
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
	if a, b := withoutUsage(t, statusBefore), withoutUsage(t, statusAfter); a != b {
		t.Errorf("status changed with usage rows in the ledger beyond its usage keys:\nbefore %s\nafter %s", a, b)
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

// withoutUsage is status --json with the keys that read usage rows removed:
// the report's usage and long_sessions and each item's usage, which are the
// only places a usage row may show.
func withoutUsage(t *testing.T, status string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(status), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, status)
	}
	delete(doc, "usage")
	delete(doc, "long_sessions")
	items, _ := doc["items"].([]any)
	for _, it := range items {
		delete(it.(map[string]any), "usage")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
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

	_, first, err := s.syncUsage(false, time.Now())
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
	if *d1.Usage != (runlog.Usage{Input: 30, Output: 11, CacheRead: 300}) || d1.ContextMax == nil || *d1.ContextMax != 220 || d1.ID != "" || d1.SessionID != "long" {
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
	_, second, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote != 0 || second.Bytes != 0 {
		t.Errorf("second sync over an unchanged directory: %+v, want no row and no byte read", second)
	}

	// Tomorrow, the held day is over and lands without rereading a byte.
	s = reload(t, s)
	_, third, err := s.syncUsage(false, time.Now().AddDate(0, 0, 1))
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

	_, tally, err := s.syncUsage(true, time.Now())
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

// behindSymlink points a fresh symlink at path's resolved form and returns both,
// the way /tmp and /var lead into /private on macOS.
func behindSymlink(t *testing.T, path string) (link, resolved string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}
	return link, resolved
}

// syncOneTranscript writes one finished day's transcript under dir and syncs.
func syncOneTranscript(t *testing.T, s *session, dir string) usageTally {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "x.jsonl"), transcriptLine("assistant", "x", "m", time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), 1, 1, 1)+"\n")
	_, tally, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tally
}

// Claude Code names a transcript directory after the working directory with
// its symlinks resolved, so a root reached through one files its sessions
// under a slug the root as given never produces.
func TestSyncReadsTheResolvedRootSlug(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	s := fixture(t, "")
	link, resolved := behindSymlink(t, s.cfg.Root)
	s.cfg.Root = link

	tally := syncOneTranscript(t, s, filepath.Join(claude, "projects", transcriptSlug(resolved)))
	if tally.Read != 1 || tally.Wrote != 1 {
		t.Errorf("sync of a root behind a symlink: %+v, want its resolved transcript read", tally)
	}
}

func TestSyncStillReadsTheLiteralRootSlug(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	s := fixture(t, "")
	link, _ := behindSymlink(t, s.cfg.Root)
	s.cfg.Root = link

	tally := syncOneTranscript(t, s, filepath.Join(claude, "projects", transcriptSlug(link)))
	if tally.Read != 1 || tally.Wrote != 1 {
		t.Errorf("sync of the literal slug: %+v, want it read", tally)
	}
}

func TestTranscriptDirsListsARootWithNoSymlinkOnce(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	s := fixture(t, "")
	resolved, err := filepath.EvalSymlinks(s.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Root = resolved
	dir := filepath.Join(claude, "projects", transcriptSlug(resolved))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	dirs, err := s.transcriptDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("transcript dirs = %v, want %s once", dirs, dir)
	}
}

func TestSyncReadsAWorktreeUnderTheResolvedStateDir(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	state, _ := behindSymlink(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", state)
	s := fixture(t, "")
	trees, err := s.projectTrees()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(trees, state) {
		t.Fatalf("worktrees %s are not under the symlinked state dir %s", trees, state)
	}
	if err := os.MkdirAll(trees, 0o700); err != nil {
		t.Fatal(err)
	}
	realTrees, err := filepath.EvalSymlinks(trees)
	if err != nil {
		t.Fatal(err)
	}

	tally := syncOneTranscript(t, s, filepath.Join(claude, "projects", transcriptSlug(filepath.Join(realTrees, "01"))))
	if tally.Read != 1 || tally.Wrote != 1 {
		t.Errorf("sync of a worktree under a symlinked state dir: %+v, want its resolved transcript read", tally)
	}
}

// syncFixture is a project whose log directory and root transcript directory
// both exist, with Claude Code's config and the log cache in temp dirs.
func syncFixture(t *testing.T) (s *session, logs, transcripts string) {
	t.Helper()
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s = fixture(t, "")
	logs = s.cfg.LogDir()
	transcripts = filepath.Join(claude, "projects", transcriptSlug(s.cfg.Root))
	for _, d := range []string{logs, transcripts} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return s, logs, transcripts
}

// Claude Code writes a transcript for a headless session too. The sync step
// alone, which is what drive runs before its queue, must record that session
// from its log, with its item and stage, and never as an interactive row that
// backfill's dedupe would then keep out for good.
func TestSyncAloneRecordsALoggedSessionAsHeadless(t *testing.T) {
	s, logs, transcripts := syncFixture(t)
	day := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local)
	logAt(t, logs, "01-first-review.20261001-093000.000000000.jsonl", fmt.Sprintf(usageResult, "sess-h")+"\n")
	write(t, filepath.Join(transcripts, "sess-h.jsonl"), transcriptLine("assistant", "sess-h", "m1", day, 11, 44, 22)+"\n")

	s.driveSync("before the queue")

	rows := usageRows(t, s.cfg.Root)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one for sess-h", rows)
	}
	r := rows[0]
	if r.SessionID != "sess-h" || r.Source != ledger.SourceHeadless || r.ID != "1" || r.Stage != ledger.StageReview ||
		r.Log != "01-first-review.20261001-093000.000000000.jsonl" {
		t.Errorf("row = %+v, want the log's headless review row for item 1", r)
	}

	// A second pass reopens neither the log nor the transcript's day.
	s = reload(t, s)
	b, sy, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b.Wrote != 0 || sy.Wrote != 0 || b.Skipped[skipRecorded] != 1 || len(usageRows(t, s.cfg.Root)) != 1 {
		t.Errorf("second pass: backfill %+v sync %+v", b, sy)
	}
}

// A killed headless session leaves a log with no result event, so it gets no
// row; its transcript is still not an interactive session's.
func TestSyncSkipsASessionAKilledLogNames(t *testing.T) {
	s, logs, transcripts := syncFixture(t)
	day := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local)
	logAt(t, logs, "01-first.20261001-093000.000000000.jsonl", `{"type":"system","subtype":"init","session_id":"sess-k"}`+"\n")
	write(t, filepath.Join(transcripts, "sess-k.jsonl"), transcriptLine("assistant", "sess-k", "m1", day, 1, 1, 1)+"\n")

	dryB, dryS, err := s.syncUsage(true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dryB.Wrote != 0 || dryS.Wrote != 0 || dryS.Skipped[skipHeadless] != 1 {
		t.Errorf("dry run: backfill %+v sync %+v, want nothing counted", dryB, dryS)
	}
	b, sy, err := s.syncUsage(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b.Skipped[skipNoResult] != 1 || sy.Wrote != 0 || sy.Skipped[skipHeadless] != 1 || len(usageRows(t, s.cfg.Root)) != 0 {
		t.Errorf("backfill %+v sync %+v, rows %+v", b, sy, usageRows(t, s.cfg.Root))
	}
}

// The dry run counts what the real one writes: a session a log names is not
// counted as interactive just because nothing was written for it yet.
func TestSyncDryRunCountsALoggedSessionOnce(t *testing.T) {
	s, logs, transcripts := syncFixture(t)
	day := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local)
	logAt(t, logs, "01-first.20261001-093000.000000000.jsonl", fmt.Sprintf(usageResult, "sess-h")+"\n")
	write(t, filepath.Join(transcripts, "sess-h.jsonl"), transcriptLine("assistant", "sess-h", "m1", day, 1, 1, 1)+"\n")

	b, sy, err := s.syncUsage(true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b.Wrote != 1 || sy.Wrote != 0 || len(usageRows(t, s.cfg.Root)) != 0 {
		t.Errorf("dry run: backfill %+v sync %+v", b, sy)
	}
}

// A codex session's log reads to a usage row named by its thread, which is
// what --sync deduplicates by and skips a row without.
func TestUsageRowFromACodexStream(t *testing.T) {
	rd, err := runlog.ReadResult(filepath.Join("..", "runlog", "testdata", "codex-stream.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	row, ok := usageRow("1", ledger.StageReview, "x.jsonl", rd.Result)
	if !ok {
		t.Fatal("a codex turn.completed with usage wrote no usage row")
	}
	if row.SessionID != "01a0b561-3923-7d13-9bac-316b8a767584" {
		t.Errorf("session_id = %q", row.SessionID)
	}
	want := runlog.Usage{Input: 405, Output: 5, CacheCreate: 26926}
	if row.Usage == nil || *row.Usage != want {
		t.Errorf("usage = %+v, want %+v", row.Usage, want)
	}
}

// A usage row carries context_max even at zero, and no other row carries it.
func TestUsageRowCarriesContextMaxAtZero(t *testing.T) {
	rd, err := runlog.ReadResult(filepath.Join("..", "runlog", "testdata", "claude-usage-limit.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	row, ok := usageRow("1", ledger.StageImplement, "x.jsonl", rd.Result)
	if !ok {
		t.Fatal("a zero usage object is still a usage row")
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"context_max":0`) {
		t.Errorf("usage row %s lacks context_max at zero", b)
	}
	b, err = json.Marshal(ledger.Row{ID: "1", Stage: ledger.StageImplement})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "context_max") {
		t.Errorf("stage row %s carries context_max", b)
	}
}

// A backfill that read the log first leaves the launch nothing to write.
func TestRecordWritesOneUsageRowPerSession(t *testing.T) {
	s := fixture(t, "")
	u := runlog.Usage{Input: 1}
	for range 2 {
		if err := s.record(ledger.Row{ID: "1", Usage: &u, SessionID: "dup", Source: ledger.SourceHeadless}); err != nil {
			t.Fatal(err)
		}
	}
	if rows := usageRows(t, s.cfg.Root); len(rows) != 1 {
		t.Errorf("usage rows = %d, want 1", len(rows))
	}
}
