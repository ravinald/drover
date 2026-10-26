package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/projects"
)

var usageNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// usageProject is a two-item project whose ledger holds rows, with [usage]
// appended to its config when extra is set. XDG_CONFIG_HOME is its own, so
// the registry the budget reads holds only what the test registers.
func usageProject(t *testing.T, extra string, rows ...string) *session {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := shelfProject(t, map[string]string{
		"B01-spent.md": body("Spent tokens", ""),
		"B02-idle.md":  body("Never ran", ""),
	}, nil)
	if extra != "" {
		conf := filepath.Join(dir, config.FileName)
		raw, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
		if err != nil {
			t.Fatal(err)
		}
		write(t, conf, string(raw)+"\n"+extra)
	}
	write(t, filepath.Join(dir, "ledger.jsonl"), strings.Join(rows, "\n")+"\n")
	s := openIn(t, dir)
	s.offline = true
	return s
}

// reportJSON is the report as --json prints it, decoded.
func usageReport(t *testing.T, s *session) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s.statusReport(false, false, usageNow))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func itemJSON(t *testing.T, report map[string]any, id string) map[string]any {
	t.Helper()
	for _, it := range report["items"].([]any) {
		if m := it.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no item %s in the report", id)
	return nil
}

const (
	usageOld    = `{"id":"B1","stage":"implement","usage":{"input":1,"output":2,"cache_create":3,"cache_read":40},"session_id":"a","source":"headless","cost_usd":1.5,"date":"2026-09-01","ts":"2026-09-01T10:00:00Z"}`
	usageRecent = `{"id":"B1","stage":"review","usage":{"input":10,"output":20,"cache_create":30,"cache_read":400},"session_id":"b","source":"headless","cost_usd":2.25,"date":"2026-10-23","ts":"2026-10-23T10:00:00Z"}`
	usageNoItem = `{"id":"","usage":{"input":100,"output":200,"cache_create":300,"cache_read":4000},"session_id":"c","source":"interactive","date":"2026-10-20","ts":"2026-10-20T10:00:00Z"}`
)

// An item with usage rows carries their sum over all time, and an item with
// none carries no usage key at all.
func TestStatusItemUsageSumsAllTimeAndIsAbsentWithoutRows(t *testing.T) {
	s := usageProject(t, "", `{"id":"B1","stage":"implement","date":"2026-09-01"}`, usageOld, usageRecent, usageNoItem)
	report := usageReport(t, s)

	u, ok := itemJSON(t, report, "B1")["usage"].(map[string]any)
	if !ok {
		t.Fatalf("B1 has no usage object: %v", itemJSON(t, report, "B1"))
	}
	tokens := u["tokens"].(map[string]any)
	for k, want := range map[string]float64{"input": 11, "output": 22, "cache_create": 33, "cache_read": 440} {
		if tokens[k] != want {
			t.Errorf("B1 usage.tokens.%s = %v, want %v", k, tokens[k], want)
		}
	}
	if u["runs"] != 2.0 || u["weighted"] != 110.0 || u["cost_usd"] != 3.75 {
		t.Errorf("B1 usage = %v, want runs 2, weighted 110 (66 + 0.1*440), cost_usd 3.75", u)
	}
	if _, ok := itemJSON(t, report, "B2")["usage"]; ok {
		t.Errorf("B2 has no usage rows and still carries usage: %v", itemJSON(t, report, "B2"))
	}
}

// The report-level object sums the rows inside the window, the item-less one
// included, per stage as well; a ledger with no usage row has neither it nor
// long_sessions.
func TestStatusReportUsageSumsTheWindow(t *testing.T) {
	s := usageProject(t, "", usageOld, usageRecent, usageNoItem)
	u, ok := usageReport(t, s)["usage"].(map[string]any)
	if !ok {
		t.Fatal("the report has no usage object")
	}
	if u["window"] != "7d" || u["runs"] != 2.0 || u["weighted"] != 1100.0 || u["cost_usd"] != 2.25 {
		t.Errorf("usage = %v, want window 7d, runs 2, weighted 1100 (660 + 0.1*4400), cost_usd 2.25", u)
	}
	stages := u["by_stage"].(map[string]any)
	if len(stages) != 2 {
		t.Errorf("by_stage = %v, want review and interactive only: the implement row is outside the window", stages)
	}
	if r := stages["review"].(map[string]any); r["runs"] != 1.0 || r["weighted"] != 100.0 {
		t.Errorf("by_stage.review = %v", r)
	}
	if r := stages["interactive"].(map[string]any); r["runs"] != 1.0 || r["weighted"] != 1000.0 {
		t.Errorf("by_stage.interactive = %v", r)
	}
	for _, k := range []string{"budget", "budget_used", "budget_unread"} {
		if _, ok := u[k]; ok {
			t.Errorf("usage carries %s with no [usage] weekly_budget", k)
		}
	}

	empty := usageReport(t, usageProject(t, "", `{"id":"B1","stage":"implement","date":"2026-09-01"}`))
	for _, k := range []string{"usage", "long_sessions"} {
		if _, ok := empty[k]; ok {
			t.Errorf("a ledger with no usage row carries %s", k)
		}
	}
}

// With a reset, the window starts at the last one, crossing it drops what came
// before, and the budget sums every registered project, naming the one whose
// ledger would not read.
func TestStatusUsageWindowRunsFromTheResetAndTheBudgetSpansProjects(t *testing.T) {
	// 2026-10-24 is a Saturday; 20:00 PDT that day is after usageNow, so the
	// last reset is 2026-10-17T20:00-07:00, 2026-10-18T03:00Z.
	before := `{"id":"B1","stage":"implement","usage":{"input":5000,"output":0,"cache_create":0,"cache_read":0},"session_id":"x","source":"headless","date":"2026-10-18","ts":"2026-10-18T02:59:00Z"}`
	after := `{"id":"B1","stage":"implement","usage":{"input":7000,"output":0,"cache_create":0,"cache_read":0},"session_id":"y","source":"headless","date":"2026-10-18","ts":"2026-10-18T03:01:00Z"}`
	s := usageProject(t, "[usage]\nweekly_budget = 10000\nreset = \"Sat 20:00 America/Los_Angeles\"\n", before, after)

	sibling := shelfProject(t, map[string]string{"B01-x.md": body("x", "")}, nil)
	write(t, filepath.Join(sibling, "ledger.jsonl"), `{"id":"","usage":{"input":1000,"output":0,"cache_create":0,"cache_read":0},"session_id":"z","source":"interactive","date":"2026-10-20","ts":"2026-10-20T10:00:00Z"}`+"\n")
	broken := shelfProject(t, map[string]string{"B01-x.md": body("x", "")}, nil)
	led := filepath.Join(broken, "ledger.jsonl")
	write(t, led, "")
	if err := os.Chmod(led, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(led, 0o600) })
	reg := projects.Path(config.GlobalPath())
	for _, r := range []projects.Row{
		{Project: "self", Root: s.cfg.Root}, {Project: "sibling", Root: sibling}, {Project: "broken", Root: broken},
	} {
		if err := projects.Append(reg, r); err != nil {
			t.Fatal(err)
		}
	}

	u := usageReport(t, s)["usage"].(map[string]any)
	start, err := time.Parse(time.RFC3339, u["window"].(string))
	if err != nil || !start.Equal(time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %v, want the reset at 2026-10-17T20:00:00-07:00", u["window"])
	}
	if u["runs"] != 1.0 || u["weighted"] != 7000.0 {
		t.Errorf("usage = %v, want only the row after the reset", u)
	}
	if u["budget"] != 10000.0 || u["budget_used"] != 0.8 {
		t.Errorf("budget = %v, budget_used = %v, want 10000 and 0.8: this project's 7000 and the sibling's 1000, counted once each", u["budget"], u["budget_used"])
	}
	unread, _ := u["budget_unread"].([]any)
	if len(unread) != 1 || !strings.HasPrefix(unread[0].(string), "broken (") {
		t.Errorf("budget_unread = %v, want the broken project alone", u["budget_unread"])
	}
}

func TestResetLastIsTheMostRecentAtOrBeforeNow(t *testing.T) {
	r, err := config.ParseReset("sat 20:00 America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	la := r.Zone
	for _, c := range []struct{ now, want time.Time }{
		{time.Date(2026, 10, 24, 19, 59, 0, 0, la), time.Date(2026, 10, 17, 20, 0, 0, 0, la)},
		{time.Date(2026, 10, 24, 20, 0, 0, 0, la), time.Date(2026, 10, 24, 20, 0, 0, 0, la)},
		{time.Date(2026, 10, 27, 9, 0, 0, 0, la), time.Date(2026, 10, 24, 20, 0, 0, 0, la)},
		// Across the switch to standard time on 2026-11-01: still 20:00 local.
		{time.Date(2026, 11, 2, 9, 0, 0, 0, la), time.Date(2026, 10, 31, 20, 0, 0, 0, la)},
		{time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 20, 0, 0, 0, la)},
	} {
		if got := r.Last(c.now); !got.Equal(c.want) {
			t.Errorf("Last(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	for _, bad := range []string{"Sat 20:00", "Someday 20:00 UTC", "Sat 25:00 UTC", "Sat 20:00 Mars/Olympus", "Sat 20:00 local"} {
		if _, err := config.ParseReset(bad); err == nil {
			t.Errorf("ParseReset(%q) accepted it", bad)
		}
	}
}

// One session is caught by its age, one by its context, one by neither, and a
// ledger whose sessions all stay short has an empty list rather than none.
func TestStatusLongSessionsNamesAgeAndContext(t *testing.T) {
	old := `{"id":"","usage":{"input":1,"output":1,"cache_create":0,"cache_read":0},"session_id":"aged","source":"interactive","context_max":1000,"started":"2026-10-20T08:00:00Z","first_ts":"2026-10-23T08:00:00Z","date":"2026-10-23","ts":"2026-10-23T09:00:00Z"}`
	wide := `{"id":"B1","stage":"implement","usage":{"input":1,"output":1,"cache_create":0,"cache_read":0},"session_id":"wide","source":"headless","context_max":500000,"duration_ms":3600000,"date":"2026-10-23","ts":"2026-10-23T10:00:00Z"}`
	short := `{"id":"B1","stage":"review","usage":{"input":1,"output":1,"cache_create":0,"cache_read":0},"session_id":"short","source":"headless","context_max":2000,"duration_ms":600000,"date":"2026-10-23","ts":"2026-10-23T11:00:00Z"}`
	s := usageProject(t, "", old, wide, short)
	list, ok := usageReport(t, s)["long_sessions"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("long_sessions = %v, want aged and wide", usageReport(t, s)["long_sessions"])
	}
	got := map[string]map[string]any{}
	for _, l := range list {
		m := l.(map[string]any)
		got[m["session_id"].(string)] = m
	}
	if a := got["aged"]; a == nil || a["hours"] != 73.0 || a["source"] != "interactive" || !strings.Contains(a["reason"].(string), "past 24h") {
		t.Errorf("aged = %v, want 73 hours open, named for its age", a)
	}
	if w := got["wide"]; w == nil || w["id"] != "B1" || w["context_max"] != 500000.0 || w["hours"] != 1.0 || !strings.Contains(w["reason"].(string), "past 400K") {
		t.Errorf("wide = %v, want B1 at 500000, named for its context", w)
	}
	if _, ok := got["short"]; ok {
		t.Error("a 10-minute session at 2000 tokens is listed")
	}

	quiet := usageReport(t, usageProject(t, "", short))
	if list, ok := quiet["long_sessions"].([]any); !ok || len(list) != 0 {
		t.Errorf("long_sessions = %v (present %v), want an empty list", quiet["long_sessions"], ok)
	}
}

// A session whose every row is headless exited when its row was written, so
// it reads ended. Any interactive row leaves it at last seen, because the day
// it is in has no row until that day ends.
func TestStatusLongSessionsSayWhetherTheyEnded(t *testing.T) {
	usage := `"usage":{"input":1,"output":1,"cache_create":0,"cache_read":0},"context_max":500000`
	rows := []string{
		`{"id":"B1","stage":"implement",` + usage + `,"session_id":"headless","source":"headless","duration_ms":3600000,"date":"2026-10-23","ts":"2026-10-23T10:00:00Z"}`,
		`{"id":"",` + usage + `,"session_id":"interactive","source":"interactive","started":"2026-10-22T08:00:00Z","first_ts":"2026-10-23T08:00:00Z","date":"2026-10-23","ts":"2026-10-23T09:00:00Z"}`,
		`{"id":"B1","stage":"implement",` + usage + `,"session_id":"both","source":"headless","duration_ms":600000,"date":"2026-10-22","ts":"2026-10-22T12:00:00Z"}`,
		`{"id":"",` + usage + `,"session_id":"both","source":"interactive","first_ts":"2026-10-23T07:00:00Z","date":"2026-10-23","ts":"2026-10-23T11:30:00Z"}`,
	}
	list, _ := usageReport(t, usageProject(t, "", rows...))["long_sessions"].([]any)
	got := map[string]map[string]any{}
	for _, l := range list {
		m := l.(map[string]any)
		got[m["session_id"].(string)] = m
	}
	for _, c := range []struct{ id, state, started, lastSeen string }{
		{"headless", "ended", "2026-10-23T09:00:00Z", "2026-10-23T10:00:00Z"},
		{"interactive", "last_seen", "2026-10-22T08:00:00Z", "2026-10-23T09:00:00Z"},
		{"both", "last_seen", "2026-10-22T11:50:00Z", "2026-10-23T11:30:00Z"},
	} {
		m := got[c.id]
		if m == nil {
			t.Errorf("%s is not listed: %v", c.id, list)
			continue
		}
		if m["state"] != c.state || m["started"] != c.started || m["last_seen"] != c.lastSeen {
			t.Errorf("%s: state %v, started %v, last_seen %v; want %s, %s, %s", c.id, m["state"], m["started"], m["last_seen"], c.state, c.started, c.lastSeen)
		}
	}
}

func TestTokensAndUSDTemplateFunctions(t *testing.T) {
	s := usageProject(t, "")
	funcs := s.templateFuncs(usageNow, time.UTC)
	tokens := funcs["tokens"].(func(any) string)
	usd := funcs["usd"].(func(any) string)
	for in, want := range map[any]string{
		nil: "", json.Number("0"): "0", json.Number("999"): "999", json.Number("999.6"): "1K",
		json.Number("1000"): "1K", json.Number("1250"): "1.3K", json.Number("840000"): "840K",
		json.Number("999949"): "999.9K", json.Number("999950"): "1M", json.Number("12500000"): "12.5M",
		json.Number("999950000"): "1B", json.Number("1200000000"): "1.2B", json.Number("4500000000000"): "4500B",
	} {
		if got := tokens(in); got != want {
			t.Errorf("tokens(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[any]string{
		nil: "", json.Number("0"): "$0", json.Number("0.49"): "$0", json.Number("0.5"): "$1",
		json.Number("999.4"): "$999", json.Number("999.5"): "$1,000", json.Number("1234.56"): "$1,235",
		json.Number("1234567"): "$1,234,567", json.Number("-1500"): "-$1,500",
	} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %q, want %q", in, got, want)
		}
	}
	budget := funcs["budget"].(func(any) string)
	for in, want := range map[any]string{nil: "", json.Number("0.79"): "", json.Number("0.8"): "warning", json.Number("0.99"): "warning", json.Number("1"): "critical", json.Number("1.4"): "critical"} {
		if got := budget(in); got != want {
			t.Errorf("budget(%v) = %q, want %q", in, got, want)
		}
	}
}

// Both templates show the budget fraction in the footer, classed by how close
// it is.
func TestBuiltinTemplatesMarkTheBudget(t *testing.T) {
	row := `{"id":"","usage":{"input":9000,"output":0,"cache_create":0,"cache_read":0},"session_id":"q","source":"interactive","date":"2026-10-23","ts":"2026-10-23T10:00:00Z"}`
	s := usageProject(t, "[usage]\nweekly_budget = 10000\nreset = \"Sat 20:00 UTC\"\n", row)
	for _, name := range []string{"html", "obsidian"} {
		out, err := s.renderTemplate("builtin:"+name, s.statusReport(false, false, usageNow), usageNow)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `drover-warning">budget 90%</span> of 10K across every project`) {
			t.Errorf("builtin:%s does not mark a budget at 90%% as a warning:\n%s", name, out)
		}
	}
}
