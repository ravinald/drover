package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logfile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":12,"total_cost_usd":1.5,"result":"shipped it"}`

func TestReadResultFindsTheTerminalEvent(t *testing.T) {
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		`{"type":"system"}`+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n"+okResult+"\n")

	rd, err := ReadResult(p)
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if !rd.Result.Succeeded() {
		t.Error("a clean result must read as succeeded")
	}
	if rd.Result.Text != "shipped it" || rd.Result.NumTurns != 12 {
		t.Errorf("got %+v", rd.Result)
	}
	if rd.Result.Schema != "claude" {
		t.Errorf("schema = %q, want claude", rd.Result.Schema)
	}
	if !strings.Contains(rd.Result.Headline(), "12 turns") {
		t.Errorf("headline = %q", rd.Result.Headline())
	}
}

// A killed run leaves a truncated final line. Reading only the last line — what
// the shell driver did — reports such a run as having no result at all.
func TestReadResultSurvivesATruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		okResult+"\n"+`{"type":"assistant","message":{"conte`)

	rd, err := ReadResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Found {
		t.Fatal("the result before a truncated line must still be found")
	}
	if rd.Result.Text != "shipped it" {
		t.Errorf("got %q", rd.Result.Text)
	}
	// The truncated line is not an event: drive reads a non-zero count as
	// proof the runner wrote something, and half a line is not that.
	if rd.Events != 1 {
		t.Errorf("events = %d, want 1: a truncated line is not an event", rd.Events)
	}
}

func TestReadResultKeepsTheLastOfSeveral(t *testing.T) {
	dir := t.TempDir()
	first := strings.Replace(okResult, "shipped it", "first", 1)
	second := strings.Replace(okResult, "shipped it", "second", 1)
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl", first+"\n"+second+"\n")

	rd, _ := ReadResult(p)
	if !rd.Found || rd.Result.Text != "second" {
		t.Errorf("got found=%v text=%q, want the last result event", rd.Found, rd.Result.Text)
	}
}

func TestReadResultOnALogWithNone(t *testing.T) {
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl", `{"type":"system"}`+"\n")
	rd, err := ReadResult(p)
	if rd.Found || err != nil {
		t.Errorf("found=%v err=%v; an unfinished run has no result", rd.Found, err)
	}
	// Locked, though: a schema drover reads wrote it, so the log is a session
	// that stopped rather than a runner drover cannot read.
	if rd.Known != 1 {
		t.Errorf("known = %d, want 1", rd.Known)
	}
}

func TestFailedResultIsNotSucceeded(t *testing.T) {
	for _, body := range []string{
		`{"type":"result","subtype":"success","is_error":true}`,
		`{"type":"result","subtype":"error_max_turns","is_error":false}`,
	} {
		dir := t.TempDir()
		p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl", body+"\n")
		rd, _ := ReadResult(p)
		if !rd.Found {
			t.Fatalf("result should parse: %s", body)
		}
		if rd.Result.Succeeded() {
			t.Errorf("%s must not read as succeeded", body)
		}
	}
}

func TestFindReturnsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range []string{"20260801-100000.000000000", "20260801-120000.000000000", "20260801-110000.000000000"} {
		logfile(t, dir, "01-x."+ts+".jsonl", okResult+"\n")
	}
	// A different item's logs must not be picked up.
	logfile(t, dir, "02-y.20260801-130000.000000000.jsonl", okResult+"\n")

	got, err := Find(dir, "01-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d logs, want 3: %v", len(got), got)
	}
	if !strings.Contains(got[0], "120000") {
		t.Errorf("newest first violated: %v", got)
	}

	latest, err := Latest(dir, "01-x")
	if err != nil || !strings.Contains(latest, "120000") {
		t.Errorf("Latest = %q err=%v", latest, err)
	}
}

func TestFindAndLatestOnAnEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	if got, err := Find(dir, "01-x"); err != nil || len(got) != 0 {
		t.Errorf("got %v err=%v", got, err)
	}
	latest, err := Latest(dir, "01-x")
	if err != nil || latest != "" {
		t.Errorf("Latest on an empty dir = %q err=%v", latest, err)
	}
}

// "Ran" means executed clean and awaiting validation, not merged. An
// interrupted run must not falsely claim it is ready for a PR.
func TestRanRequiresASuccessfulResult(t *testing.T) {
	dir := t.TempDir()
	logfile(t, dir, "01-x.20260801-100000.000000000.jsonl", `{"type":"system"}`+"\n")
	if Ran(dir, "01-x") {
		t.Error("a run with no result must not read as ran")
	}
	logfile(t, dir, "01-x.20260801-110000.000000000.jsonl", okResult+"\n")
	if !Ran(dir, "01-x") {
		t.Error("a successful run must read as ran")
	}
}

// Second granularity collided when two runs of one item started in the same
// second: the second appended to the first's stream.
func TestNameIsUniqueWithinASecond(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a := Name("01-x", base)
	b := Name("01-x", base.Add(time.Millisecond))
	if a == b {
		t.Errorf("two runs a millisecond apart collide: %s", a)
	}
	if !strings.HasPrefix(a, "01-x.") || !strings.HasSuffix(a, ".jsonl") {
		t.Errorf("unexpected shape: %s", a)
	}
}

// A review stream and a run stream for one item share a directory, and result
// reads the newest log for the run base. A review log matching that glob would
// be handed to result as the run's own self-report.
func TestReviewLogsAreInvisibleToTheRunBase(t *testing.T) {
	dir := t.TempDir()
	base, review := Base("01", "x"), ReviewBase("01", "x")
	logfile(t, dir, Name(base, time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)), okResult+"\n")
	logfile(t, dir, Name(review, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)), okResult+"\n")

	runs, err := Find(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || !strings.Contains(runs[0], "100000") {
		t.Errorf("the run base picked up %d logs, want only its own: %v", len(runs), runs)
	}
	reviews, err := Find(dir, review)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || !strings.Contains(reviews[0], "-review.") {
		t.Errorf("the review base found %v, want one review log", reviews)
	}
}

func TestBaseJoinsStemAndSlug(t *testing.T) {
	if got := Base("02b", "consumer-recs-api"); got != "02b-consumer-recs-api" {
		t.Errorf("got %q", got)
	}
}

// The fixtures are captured from live `agy --output-format stream-json -p`
// runs, not written here. A hand-written line proves only that the reader
// matches what its author believed the runner emits: a schema copied from
// Claude Code's field names reads agy's terminal event as absent and reports a
// reviewer that had finished as one that died.
func TestReadResultReadsACapturedAgyStream(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "agy-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Found {
		t.Fatal("no terminal event found in a real agy stream")
	}
	if !rd.Result.Succeeded() {
		t.Errorf("a clean agy run read as %+v", rd.Result)
	}
	if rd.Result.Schema != "agy" {
		t.Errorf("schema = %q, want agy", rd.Result.Schema)
	}
	if rd.Result.NumTurns != 1 || !strings.Contains(rd.Result.Text, "hello") {
		t.Errorf("got %+v", rd.Result)
	}
	// Every line of that stream is one of agy's own events, so nothing in it
	// reads as a runner drover cannot claim.
	if rd.Events == 0 || rd.Known != rd.Events {
		t.Errorf("events=%d known=%d, want every line claimed", rd.Events, rd.Known)
	}
	// agy accounts for a run in tokens, so the dollar segment is absent rather
	// than reporting a run that was free.
	if strings.Contains(rd.Result.Headline(), "$") {
		t.Errorf("headline = %q", rd.Result.Headline())
	}
}

func TestACapturedAgyFailureIsNotSucceeded(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "agy-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Found {
		t.Fatal("agy's failure is a terminal event and must be read as one")
	}
	if rd.Result.Succeeded() {
		t.Errorf("status ERROR read as succeeded: %+v", rd.Result)
	}
	// The reason stands in for the response agy never produced, so a result
	// carries why rather than an empty report.
	if !strings.Contains(rd.Result.Text, "invalid model selection") {
		t.Errorf("text = %q, want agy's own reason", rd.Result.Text)
	}
}

// A runner drover ships no schema for writes a log full of events that no
// schema claims. That is not the same failure as a session that stopped, and
// drive owes the two different sentences: one names a config to fix, the other
// a run to repeat.
func TestAForeignStreamIsCountedAndNeverClaimed(t *testing.T) {
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		`{"kind":"start"}`+"\n"+`{"kind":"result","outcome":"ok"}`+"\n")

	rd, err := ReadResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Found {
		t.Error("a schema drover does not read must not yield a result")
	}
	if rd.Events != 2 || rd.Known != 0 {
		t.Errorf("events=%d known=%d, want 2 and 0", rd.Events, rd.Known)
	}
}

// The same split from the other side: a stream drover does read, cut off before
// its terminal event, is a session that stopped and must stay claimed.
func TestAnInterruptedAgyStreamStaysClaimed(t *testing.T) {
	whole, err := os.ReadFile(filepath.Join("testdata", "agy-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(whole), "\n"), "\n")
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		strings.Join(lines[:len(lines)-1], "\n")+"\n")

	rd, err := ReadResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Found {
		t.Error("the terminal event was removed and one was still read")
	}
	if rd.Known == 0 {
		t.Error("agy's own events read as a foreign schema, so an interrupted run reports a config defect")
	}
}

// Captured from a live `codex exec --json --yolo` run, like the agy fixtures
// and for the same reason: a schema written from what its author believed the
// runner emits can read codex's terminal event as absent.
func TestReadResultReadsACapturedCodexStream(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "codex-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Found {
		t.Fatal("no terminal event found in a real codex stream")
	}
	if !rd.Result.Succeeded() {
		t.Errorf("a clean codex run read as %+v", rd.Result)
	}
	if rd.Result.Schema != "codex" {
		t.Errorf("schema = %q, want codex: claude claims the same key and must not claim the kinds", rd.Result.Schema)
	}
	// codex's final message is an item event, so a result read from the
	// terminal line alone leaves a result with an empty report.
	if rd.Result.Text != "ok" {
		t.Errorf("text = %q, want the last agent message", rd.Result.Text)
	}
	if rd.Result.NumTurns != 1 {
		t.Errorf("turns = %d, want 1: codex exec runs one and counts none", rd.Result.NumTurns)
	}
	if rd.Events == 0 || rd.Known != rd.Events {
		t.Errorf("events=%d known=%d, want every line claimed", rd.Events, rd.Known)
	}
	// codex accounts for a turn in tokens, so the dollar segment is absent
	// rather than reporting a run that was free.
	if strings.Contains(rd.Result.Headline(), "$") {
		t.Errorf("headline = %q", rd.Result.Headline())
	}
}

// codex counts both cache fields inside input_tokens, so the fixture's 27331
// input is 405 uncached tokens and 26926 written to the cache.
func TestReadResultCarriesCodexUsage(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "codex-stream.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	want := Usage{Input: 405, CacheCreate: 26926, CacheRead: 0, Output: 5}
	if rd.Result.Usage == nil || *rd.Result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", rd.Result.Usage, want)
	}
	const thread = "01a0b561-3923-7d13-9bac-316b8a767584"
	if rd.Result.SessionID != thread || rd.SessionID != thread {
		t.Errorf("session = %q (read %q), want the thread.started thread_id", rd.Result.SessionID, rd.SessionID)
	}
}

func TestCodexTurnWithoutUsageLeavesItNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.jsonl")
	body := `{"type":"thread.started","thread_id":"t"}` + "\n" +
		`{"type":"turn.started","session_id":"s"}` + "\n" +
		`{"type":"turn.completed"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := ReadResult(path)
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if rd.Result.Usage != nil {
		t.Errorf("usage = %+v, want nil for a turn with no usage object", *rd.Result.Usage)
	}
	if rd.SessionID != "s" || rd.Result.SessionID != "s" {
		t.Errorf("Read.SessionID=%q Result.SessionID=%q, want the session_id key over the thread_id in both", rd.SessionID, rd.Result.SessionID)
	}
}

// codex's breakdown overrunning its total floors uncached input at zero
// rather than writing a negative count.
func TestCodexUsageFloorsInputAtZero(t *testing.T) {
	u := codexUsage{Input: 10, CacheRead: 8, CacheWrite: 8, Output: 1}.usage()
	if u.Input != 0 {
		t.Errorf("input = %d, want 0", u.Input)
	}
}

// The error strings in this fixture are shortened: the live failure carried an
// upstream proxy hostname and a trace URL. Every event name, nesting and key
// is as captured.
func TestACapturedCodexFailureIsNotSucceeded(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "codex-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Found {
		t.Fatal("turn.failed is a terminal event and must be read as one")
	}
	if rd.Result.Succeeded() {
		t.Errorf("turn.failed read as succeeded: %+v", rd.Result)
	}
	// The reason stands in for the message codex never produced, so a result
	// carries why rather than an empty report.
	if !strings.Contains(rd.Result.Text, "404") {
		t.Errorf("text = %q, want codex's own reason", rd.Result.Text)
	}
	// codex retries an unreachable model and reports each attempt as an
	// `error` event before the turn ends. Terminating on those would call a
	// turn that recovered a failure, and one that failed a session that
	// stopped.
	if rd.Result.Subtype != "failed" {
		t.Errorf("subtype = %q, want the terminal turn event, not a retry", rd.Result.Subtype)
	}
}

// The split drive keys on, from the codex side: a stream drover reads with its
// terminal event removed is a session that stopped, and must stay claimed so
// the park names a run to repeat rather than a config to fix.
func TestAnInterruptedCodexStreamStaysClaimed(t *testing.T) {
	whole, err := os.ReadFile(filepath.Join("testdata", "codex-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(whole), "\n"), "\n")
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		strings.Join(lines[:len(lines)-1], "\n")+"\n")

	rd, err := ReadResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Found {
		t.Error("the terminal event was removed and one was still read")
	}
	if rd.Known == 0 {
		t.Error("codex's own events read as a foreign schema, so an interrupted run reports a config defect")
	}
}

// An item a codex session implemented has to reach `ran`, or the queue offers
// it forever and `drive <id>` is the only way to touch it.
func TestRanReadsACodexRun(t *testing.T) {
	whole, err := os.ReadFile(filepath.Join("testdata", "codex-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logfile(t, dir, "01-x.20260801-110000.000000000.jsonl", string(whole))
	if !Ran(dir, "01-x") {
		t.Error("a clean codex run must read as ran")
	}
}

// Every schema has to keep its own log. Two runners name their kinds under
// "type", and the reader that claimed on the key alone handed claude every
// codex event.
func TestEachSchemaClaimsOnlyItsOwnKinds(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    string
	}{
		{"codex-stream.jsonl", "codex"},
		{"codex-error.jsonl", "codex"},
		{"agy-stream.jsonl", "agy"},
	} {
		rd, err := ReadResult(filepath.Join("testdata", c.fixture))
		if err != nil {
			t.Fatal(err)
		}
		if rd.Result.Schema != c.want {
			t.Errorf("%s read as schema %q, want %q", c.fixture, rd.Result.Schema, c.want)
		}
	}
}

// The denial cases read captured terminal events committed under testdata, each
// the whole line its runner wrote. The field names are the entire claim here,
// so a fixture somebody edited proves only that drover reads a shape nothing
// emits:
//
//   - claude-denied.jsonl, from a captured `drover run`: two
//     `git commit` calls lost to one permission prompt, then 88 turns and a
//     clean finish. Two refusals of one tool, which is what collapse has to
//     survive.
//   - agy-denied.jsonl, from a captured `drover review`: one turn,
//     an empty response, status SUCCESS, and the reason under a field nothing
//     read.
//   - agy-stream.jsonl, a clean agy run. Its terminal event names no denial,
//     and that reading is the one that must not change.
func TestReadResultCarriesWhatTheRunnerDenied(t *testing.T) {
	for _, c := range []struct {
		fixture  string
		turns    int
		want     []string
		sentence string
	}{
		{"claude-denied.jsonl", 88, []string{"Bash"}, "the runner denied Bash"},
		{"agy-denied.jsonl", 1, []string{"RunCommand"}, "the runner denied RunCommand"},
		{"agy-stream.jsonl", 1, nil, ""},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			rd, err := ReadResult(filepath.Join("testdata", c.fixture))
			if err != nil || !rd.Found {
				t.Fatalf("found=%v err=%v: the fixture proves nothing unless it parses", rd.Found, err)
			}
			if !rd.Result.Succeeded() {
				t.Error("every one of these exited SUCCESS; a denial is not an outcome")
			}
			// The turn count is the fixture's fingerprint. A line edited by
			// hand to carry the denials a test wanted reads as the run it
			// names until something asserts a second field off it.
			if rd.Result.NumTurns != c.turns {
				t.Errorf("num_turns = %d, want %d: this is no longer the log it claims to be",
					rd.Result.NumTurns, c.turns)
			}
			if got := rd.Result.Denied; !equal(got, c.want) {
				t.Errorf("Denied = %q, want %q", got, c.want)
			}
			if got := rd.Result.Denials(); got != c.sentence {
				t.Errorf("Denials() = %q, want %q", got, c.sentence)
			}
		})
	}
}

// display_name is what the operator saw in the prompt they never got. A stream
// carrying only the coarse class still has to name something. No log in this
// project carries one, so the event below is constructed and proves the
// fallback alone, never that a runner emits it.
func TestADenialWithNoDisplayNameFallsBackToItsAction(t *testing.T) {
	dir := t.TempDir()
	p := logfile(t, dir, "01-x.20260801-120000.000000000.jsonl",
		`{"event":"result","result":{"status":"SUCCESS","num_turns":1,`+
			`"denied_actions":[{"action":"command"},{"action":""}]}}`+"\n")

	rd, err := ReadResult(p)
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if got := rd.Result.Denied; !equal(got, []string{"command"}) {
		t.Errorf("Denied = %q, want [command]: the empty entry names nothing and is dropped", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReadResultCarriesUsage(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "claude-usage.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	r := rd.Result
	want := Usage{Input: 6, Output: 60, CacheCreate: 1700, CacheRead: 2500}
	if r.Usage == nil || *r.Usage != want {
		t.Fatalf("usage = %+v, want %+v", r.Usage, want)
	}
	if r.Model != "claude-opus-5-5[1m]" {
		t.Errorf("model = %q, want the first key of modelUsage", r.Model)
	}
	if r.SessionID != "5f0c1a7e-0000-4000-8000-000000000001" || r.DurationMS != 61234 {
		t.Errorf("session %q duration %d", r.SessionID, r.DurationMS)
	}
	// The messages grow 1003, 1502, 1701: the last is the fullest, and the
	// repeated msg_b event cannot count twice toward a maximum.
	if r.ContextMax != 1701 {
		t.Errorf("context_max = %d, want 1701", r.ContextMax)
	}
}

func TestReadResultZeroUsageIsNotNoUsage(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "claude-usage-limit.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if rd.Result.Usage == nil {
		t.Fatal("a result reporting zero tokens must carry a zero Usage, not nil")
	}
	if *rd.Result.Usage != (Usage{}) {
		t.Errorf("usage = %+v, want zeros", *rd.Result.Usage)
	}
	if rd.Result.Model != "" {
		t.Errorf("an empty modelUsage names no model, got %q", rd.Result.Model)
	}
}

func TestReadResultWithoutUsageLeavesItNil(t *testing.T) {
	rd, err := ReadResult(filepath.Join("testdata", "claude-no-usage.jsonl"))
	if err != nil || !rd.Found {
		t.Fatalf("found=%v err=%v", rd.Found, err)
	}
	if rd.Result.Usage != nil {
		t.Errorf("usage = %+v, want nil", *rd.Result.Usage)
	}
	if rd.Result.SessionID != "5f0c1a7e-0000-4000-8000-000000000003" {
		t.Errorf("session = %q", rd.Result.SessionID)
	}
}

func TestWeightedCountsACacheReadAtItsWeight(t *testing.T) {
	u := Usage{Input: 1, Output: 20, CacheCreate: 300, CacheRead: 4000}
	if got := Weighted(u); got != 721 {
		t.Errorf("Weighted(%+v) = %v, want 1 + 20 + 300 + 0.1*4000", u, got)
	}
}

// Four rate_limit_event lines copied from logs on this machine: the five-hour
// allowed row, the weekly allowed_warning row, and both rejected rows, one on
// purchased credit and one out of it. uuid and session_id are kept as written.
const (
	limitAllowed      = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1788426000,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.09,"resetsAt":1788426000},"seven_day":{"utilization":0.13,"resetsAt":1788750000}}},"uuid":"56c383ca-3cea-4043-a542-da57e5f7961b","session_id":"f1ff57f7-3561-4176-a3ca-e59d2e256c67"}`
	limitWarning      = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792983600,"rateLimitType":"seven_day","utilization":0.79,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"surpassedThreshold":0.75,"unifiedWindows":{"five_hour":{"utilization":0.09,"resetsAt":1792747800},"seven_day":{"utilization":0.79,"resetsAt":1792983600}},"overageEnabled":false},"uuid":"3b8cf781-b1f8-4f53-a6b2-b1fea02127a2","session_id":"4405dc6f-76be-4e49-b057-1f1ac716f195"}`
	limitOverage      = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792983600,"rateLimitType":"seven_day","overageStatus":"allowed","overageResetsAt":1793491200,"isUsingOverage":true,"overageInUse":true,"unifiedWindows":{"five_hour":{"utilization":0.07,"resetsAt":1792801800,"status":"allowed"},"seven_day":{"utilization":1,"resetsAt":1792983600,"status":"rejected","surpassedThreshold":1}}},"uuid":"0b40a1ca-5bcb-4e9d-b89f-aa65bd962b5c","session_id":"a52520f8-1959-4fca-a2c2-ab89b12ae048"}`
	limitOutOfCredits = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792983600,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1792834200,"status":"allowed"},"seven_day":{"utilization":1,"resetsAt":1792983600,"status":"rejected","surpassedThreshold":1}}},"uuid":"d23f9d46-1928-4efa-9cd2-ee3f20cfec20","session_id":"0380041e-a385-4e1c-a11e-32dd3f6cadbb"}`
)

func TestEveryRateLimitRowKindReadsIntoTheResult(t *testing.T) {
	before := time.Unix(1788000000, 0)
	for _, c := range []struct {
		name, line, status, kind string
		week, five               int
		overage, refused         bool
		reason                   string
	}{
		{"five-hour allowed", limitAllowed, "allowed", WindowFiveHour, 87, 91, false, false, "org_level_disabled"},
		{"weekly allowed_warning", limitWarning, "allowed_warning", WindowSevenDay, 21, 91, false, false, "org_level_disabled"},
		{"weekly rejected on overage", limitOverage, "rejected", WindowSevenDay, 0, 93, true, false, ""},
		{"weekly rejected out of credits", limitOutOfCredits, "rejected", WindowSevenDay, 0, 100, false, true, OutOfCredits},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := logfile(t, t.TempDir(), "01-x.20261024-120000.000000000.jsonl",
				limitAllowed+"\n"+c.line+"\n"+okResult+"\n")
			rd, err := ReadResult(p)
			if err != nil || !rd.Found {
				t.Fatalf("found=%v err=%v", rd.Found, err)
			}
			rl := rd.Result.RateLimit
			if rl == nil {
				t.Fatal("the result carries no reading")
			}
			if rl.Status != c.status || rl.RateLimitType != c.kind {
				t.Errorf("status %q type %q, want %q %q: the last event of the session is the one kept", rl.Status, rl.RateLimitType, c.status, c.kind)
			}
			if got, ok := rl.Remaining(WindowSevenDay, before); !ok || got != c.week {
				t.Errorf("weekly remaining = %d (%v), want %d", got, ok, c.week)
			}
			if got, ok := rl.Remaining(WindowFiveHour, before); !ok || got != c.five {
				t.Errorf("five-hour remaining = %d (%v), want %d", got, ok, c.five)
			}
			if rl.InOverage() != c.overage || rl.Refused() != c.refused {
				t.Errorf("overage %v refused %v, want %v %v", rl.InOverage(), rl.Refused(), c.overage, c.refused)
			}
			if rl.OverageDisabledReason != c.reason {
				t.Errorf("overage reason %q, want %q", rl.OverageDisabledReason, c.reason)
			}
			if rl.ResetsAt == 0 || rl.SevenDay == nil || rl.SevenDay.ResetsAt == 0 {
				t.Errorf("resets lost: %+v", rl)
			}
		})
	}
}

// A window read before its reset says nothing about the window after it.
func TestAWindowPastItsResetReadsFull(t *testing.T) {
	p := logfile(t, t.TempDir(), "01-x.20261024-120000.000000000.jsonl", limitOutOfCredits+"\n"+okResult+"\n")
	rd, _ := ReadResult(p)
	after := time.Unix(1792983600, 0)
	if got, _ := rd.Result.RateLimit.Remaining(WindowSevenDay, after); got != 100 {
		t.Errorf("remaining after the reset = %d, want 100", got)
	}
	if rd.Result.RateLimit.Current(after) {
		t.Error("a refusal whose window reset still reads as current")
	}
}

// codex and agy write no such event, and their results carry no reading.
func TestARunnerWithoutTheEventCarriesNoReading(t *testing.T) {
	p := logfile(t, t.TempDir(), "01-x.20261024-120000.000000000.jsonl",
		`{"type":"turn.completed","usage":{"input_tokens":1}}`+"\n")
	rd, _ := ReadResult(p)
	if rd.Result.RateLimit != nil {
		t.Errorf("a codex session reads %+v", rd.Result.RateLimit)
	}
}
