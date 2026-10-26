package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
)

var trendNow = time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)

// trendRows is a ledger whose every figure is known by hand:
//
//	A1 implement 10-14 10:00, one review, passed 12:00, merged 13:00: 3h, waited 1h
//	A2 implement 10-15 08:00, sent back, two reviews, passed 11:00, merged 10-16 08:00: 24h, waited 21h
//	A3 implement 10-16 00:00, one review, passed 01:30, merged 02:00: 2h, waited 30m
//	A4 implement 10-16 00:00, no review drover launched, merged 10:00: 10h
//	A5 merged 2026-09-01, outside the window
//	A6 parked twice on 10-17; three verify runs of 120s, 600s and 1200s
func trendRows(t *testing.T) []ledger.Row {
	t.Helper()
	lines := []string{
		`{"id":"A5","stage":"implement","date":"2026-09-01","ts":"2026-09-01T09:00:00Z"}`,
		`{"id":"A5","merged":true,"date":"2026-09-01","ts":"2026-09-01T10:00:00Z"}`,
		`{"id":"A1","stage":"implement","date":"2026-10-14","ts":"2026-10-14T10:00:00Z"}`,
		`{"id":"A1","stage":"review","date":"2026-10-14","ts":"2026-10-14T11:00:00Z"}`,
		`{"id":"A1","review_result":"passed","date":"2026-10-14","ts":"2026-10-14T12:00:00Z"}`,
		`{"id":"A1","branch":"a1","head":"abc","verify_ms":120000,"date":"2026-10-14","ts":"2026-10-14T12:10:00Z"}`,
		`{"id":"A1","merged":true,"date":"2026-10-14","ts":"2026-10-14T13:00:00Z"}`,
		`{"id":"A2","stage":"implement","date":"2026-10-15","ts":"2026-10-15T08:00:00Z"}`,
		`{"id":"A2","stage":"review","date":"2026-10-15","ts":"2026-10-15T09:00:00Z"}`,
		`{"id":"A2","changes":"d1","date":"2026-10-15","ts":"2026-10-15T09:30:00Z"}`,
		`{"id":"A2","stage":"review","date":"2026-10-15","ts":"2026-10-15T10:00:00Z"}`,
		`{"id":"A2","review_result":"passed","date":"2026-10-15","ts":"2026-10-15T11:00:00Z"}`,
		`{"id":"A3","stage":"implement","date":"2026-10-16","ts":"2026-10-16T00:00:00Z"}`,
		`{"id":"A4","stage":"implement","date":"2026-10-16","ts":"2026-10-16T00:00:00Z"}`,
		`{"id":"A3","stage":"review","date":"2026-10-16","ts":"2026-10-16T01:00:00Z"}`,
		`{"id":"A3","review_result":"passed","date":"2026-10-16","ts":"2026-10-16T01:30:00Z"}`,
		`{"id":"A3","branch":"a3","head":"def","verify_ms":1200000,"date":"2026-10-16","ts":"2026-10-16T01:40:00Z"}`,
		`{"id":"A3","merged":true,"date":"2026-10-16","ts":"2026-10-16T02:00:00Z"}`,
		`{"id":"A2","branch":"a2","head":"123","verify_ms":600000,"date":"2026-10-16","ts":"2026-10-16T07:00:00Z"}`,
		`{"id":"A2","merged":true,"date":"2026-10-16","ts":"2026-10-16T08:00:00Z"}`,
		`{"id":"A4","merged":true,"date":"2026-10-16","ts":"2026-10-16T10:00:00Z"}`,
		`{"id":"A6","park":"no_commits","date":"2026-10-17","ts":"2026-10-17T09:00:00Z"}`,
		`{"id":"A6","park":"trunk_red","date":"2026-10-17","ts":"2026-10-17T10:00:00Z"}`,
		`{"id":"A7","stage":"implement","date":"2026-10-12"}`,
	}
	rows := make([]ledger.Row, len(lines))
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func dayOf(t *testing.T, tr Trends, date string) TrendSet {
	t.Helper()
	for _, d := range tr.Days {
		if d.Date == date {
			return d.TrendSet
		}
	}
	t.Fatalf("the trends hold no day %s: %s to %s", date, tr.From, tr.To)
	return TrendSet{}
}

func TestTrendsOverAKnownLedger(t *testing.T) {
	tr := trends(trendRows(t), trendNow, time.UTC, 14)
	if tr.From != "2026-10-04" || tr.To != "2026-10-17" || len(tr.Days) != 14 {
		t.Fatalf("the window is %s to %s over %d days, want 2026-10-04 to 2026-10-17 over 14", tr.From, tr.To, len(tr.Days))
	}
	tot := tr.Total
	if tot.Finished == nil || *tot.Finished != (TrendCount{Value: 4, N: 4}) {
		t.Errorf("finished = %+v, want 4 of 4: A5 merged before the window", tot.Finished)
	}
	// 2h, 3h, 10h, 24h: the median is the mean of the middle two, the p90 the
	// fourth by nearest rank.
	if c := tot.CycleTime; c == nil || *c != (TrendSpread{Median: 6.5 * 3600, P90: 24 * 3600, N: 4}) {
		t.Errorf("cycle_time = %+v, want median 23400s, p90 86400s over 4", c)
	}
	if r := tot.ChangesRate; r == nil || *r != (TrendRatio{Value: 0.25, N: 4}) {
		t.Errorf("changes_rate = %+v, want 0.25 over 4", r)
	}
	if r := tot.ReviewPasses; r == nil || *r != (TrendRatio{Value: 1.333, N: 3}) {
		t.Errorf("review_passes = %+v, want 1.333 over the 3 items drover reviewed", r)
	}
	if w := tot.ReviewToMerge; w == nil || *w != (TrendSpread{Median: 3600, P90: 21 * 3600, N: 3}) {
		t.Errorf("review_to_merge = %+v, want median 3600s, p90 75600s over 3", w)
	}
	if p := tot.Parks; p == nil || p.Value != 2 || p.N != 1 || p.ByReason["no_commits"] != 1 || p.ByReason["trunk_red"] != 1 {
		t.Errorf("parks = %+v, want 2 parks on 1 item, one of each reason", p)
	}
	if g := tot.Verify; g == nil || *g != (TrendSpread{Median: 600, P90: 1200, N: 3}) {
		t.Errorf("verify = %+v, want median 600s, p90 1200s over 3 runs", g)
	}

	d16 := dayOf(t, tr, "2026-10-16")
	if d16.Finished == nil || d16.Finished.Value != 3 {
		t.Errorf("10-16 finished %+v, want A2, A3 and A4", d16.Finished)
	}
	if g := d16.Verify; g == nil || g.Median != 900 || g.N != 2 {
		t.Errorf("10-16 verify = %+v, want the median of 600s and 1200s over 2", g)
	}
	d15 := dayOf(t, tr, "2026-10-15")
	if d15.Finished == nil || d15.Finished.Value != 0 || d15.CycleTime != nil {
		t.Errorf("10-15 recorded rows and finished nothing: finished %+v cycle %+v, want 0 and null", d15.Finished, d15.CycleTime)
	}
	// A7's row carries no ts, so 10-12 holds nothing a trend may place.
	if d12 := dayOf(t, tr, "2026-10-12"); d12.Finished != nil || d12.Parks != nil {
		t.Errorf("10-12 holds only a day-only row and reads finished %+v parks %+v, want null", d12.Finished, d12.Parks)
	}
	if tr.last != "2026-10-16T10:00:00Z" {
		t.Errorf("last finishing = %q, want A4's", tr.last)
	}
}

// A window nothing finished or ran in reports every metric null, in Go and in
// the JSON a dashboard reads, and never a zero.
func TestAnEmptyWindowIsNullNotZero(t *testing.T) {
	tr := trends(trendRows(t), time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC), time.UTC, 7)
	sets := []TrendSet{tr.Total}
	for _, d := range tr.Days {
		sets = append(sets, d.TrendSet)
	}
	for _, s := range sets {
		if s != (TrendSet{}) {
			t.Fatalf("an empty window computed %+v, want every metric nil", s)
		}
	}
	raw, err := json.Marshal(tr.Total)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"finished":null,"cycle_time":null,"review_passes":null,"changes_rate":null,"parks":null,"review_to_merge":null,"verify":null}`
	if string(raw) != want {
		t.Errorf("the empty total encodes as\n%s\nwant\n%s", raw, want)
	}

	// The same through status --json on a project with no ledger at all.
	stubPATH(t)
	t.Chdir(scopeProject(t).cfg.Root)
	resetStatusFlags(t)
	out := capture(t, func() {
		if err := execute(t, "status", "--json", "--no-tracker", "--window", "3"); err != nil {
			t.Fatalf("status --json: %v", err)
		}
	})
	var got struct {
		Trends struct {
			Window int                        `json:"window"`
			Days   []map[string]any           `json:"days"`
			Total  map[string]json.RawMessage `json:"total"`
		} `json:"trends"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if got.Trends.Window != 3 || len(got.Trends.Days) != 3 {
		t.Errorf("--window 3 reported window %d over %d days", got.Trends.Window, len(got.Trends.Days))
	}
	for k, v := range got.Trends.Total {
		if string(v) != "null" {
			t.Errorf("trends.total.%s = %s on an empty ledger, want null", k, v)
		}
	}
}

func TestAWindowOutsideItsRangeIsRefused(t *testing.T) {
	stubPATH(t)
	t.Chdir(scopeProject(t).cfg.Root)
	for _, args := range [][]string{
		{"status", "--json", "--window", "0"},
		{"status", "--json", "--window", "400"},
		{"status", "--window", "7"},
	} {
		resetStatusFlags(t)
		if err := execute(t, args...); Code(err) != ExitRefused {
			t.Errorf("%v: err %v, want a refusal", args, err)
		}
	}
}

// --all-projects lists every registered project, and one whose config will
// not load is a row carrying the error rather than a gap.
func TestStatusAllProjectsListsEveryProjectAndItsErrors(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	alpha := fixture(t, "\n[[status.render]]\ntemplate = \"builtin:html\"\npath = \"site/status.html\"\n")
	beta := fixture(t, "")
	write(t, beta.cfg.Abs(beta.cfg.Paths.Ledger),
		`{"id":"1","stage":"implement","date":"2026-10-17","ts":"`+time.Now().Add(-2*time.Hour).Format(time.RFC3339)+`"}`+"\n"+
			`{"id":"1","merged":true,"date":"2026-10-17","ts":"`+time.Now().Add(-time.Hour).Format(time.RFC3339)+`"}`+"\n")
	broken := t.TempDir()
	write(t, filepath.Join(broken, config.FileName), "this is = = not toml\n")
	outsideTempDir(t)
	for name, root := range map[string]string{"alpha": alpha.cfg.Root, "beta": beta.cfg.Root, "gamma": broken} {
		if err := projects.Append(reg, projects.Row{Project: name, Root: root}); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(t.TempDir())
	// A session drive launched carries its project here, and the registry's
	// other roots must still be read as themselves.
	t.Setenv("DROVER_ROOT", alpha.cfg.Root)

	resetStatusFlags(t)
	out := capture(t, func() {
		if err := execute(t, "status", "--all-projects", "--json", "--no-tracker"); err != nil {
			t.Fatalf("status --all-projects --json: %v", err)
		}
	})
	var got AllProjects
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--all-projects --json did not parse: %v\n%s", err, out)
	}
	if len(got.Projects) != 3 || got.Now == "" {
		t.Fatalf("--all-projects listed %d projects (now %q), want 3:\n%s", len(got.Projects), got.Now, out)
	}
	byName := map[string]ProjectStatus{}
	for _, p := range got.Projects {
		byName[p.Project] = p
	}
	for _, name := range []string{"alpha", "beta"} {
		if p := byName[name]; p.Error != "" || p.Attention == nil {
			t.Errorf("%s reads %+v, want no error and an attention count", name, p)
		}
	}
	if a := byName["alpha"]; a.Finished != nil {
		t.Errorf("alpha has no ledger and reads finished %+v, want null", a.Finished)
	}
	if b := byName["beta"]; b.Finished == nil || b.Finished.Value != 1 || b.LastFinished == "" {
		t.Errorf("beta finished %+v last %q, want 1 and a last finishing", b.Finished, b.LastFinished)
	}
	if a := byName["alpha"]; len(a.Renders) != 1 || a.Renders[0].Format != "html" ||
		a.Renders[0].Path != filepath.Join(alpha.cfg.Root, "site", "status.html") {
		t.Errorf("alpha's renders are %+v, want its one html dashboard", a.Renders)
	}
	if g := byName["gamma"]; g.Root != broken || g.Error == "" || g.Attention != nil {
		t.Errorf("gamma reads %+v, want listed with its config error and nothing else", g)
	}

	resetStatusFlags(t)
	table := capture(t, func() {
		if err := execute(t, "status", "--all-projects", "--no-tracker"); err != nil {
			t.Fatalf("status --all-projects: %v", err)
		}
	})
	for _, want := range []string{"alpha", "beta", "gamma", "unreadable: "} {
		if !strings.Contains(table, want) {
			t.Errorf("the table lacks %q:\n%s", want, table)
		}
	}

	index := filepath.Join(t.TempDir(), "index.html")
	resetStatusFlags(t)
	if err := execute(t, "status", "--all-projects", "--no-tracker", "--html", "-o", index); err != nil {
		t.Fatalf("status --all-projects --html: %v", err)
	}
	page, err := os.ReadFile(index) // #nosec G304 -- written above
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(index), filepath.Join(alpha.cfg.Root, "site", "status.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<title>drover projects</title>", `href="` + filepath.ToSlash(rel) + `">alpha</a>`, "beta", "unreadable"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the index lacks %q:\n%s", want, page)
		}
	}

	resetStatusFlags(t)
	md := capture(t, func() {
		if err := execute(t, "status", "--all-projects", "--no-tracker", "--template", "builtin:obsidian"); err != nil {
			t.Fatalf("status --all-projects --template builtin:obsidian: %v", err)
		}
	})
	if !strings.Contains(md, "# drover projects") || !strings.Contains(md, "| gamma | unreadable: ") {
		t.Errorf("the Obsidian index lacks its heading or gamma's error:\n%s", md)
	}
}

// The golden page draws its trends as inline SVG and still runs nothing.
func TestTheGoldenHTMLDrawsSparklinesAndNoScript(t *testing.T) {
	page, err := os.ReadFile(filepath.Join(goldenDir(t), "dashboard-html.html.golden")) // #nosec G304 -- the test's own fixture
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h2>Trends</h2>", `<svg class="spark"`, "<polyline points=", "<circle "} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the golden page lacks %q", want)
		}
	}
	trends := string(page)[strings.Index(string(page), "<h2>Trends</h2>"):strings.Index(string(page), "<h2>Key</h2>")]
	if strings.Contains(strings.ToLower(trends), "<script") {
		t.Error("the golden page's Trends section carries a <script>")
	}
}

// A [verify] run's wall time lands on the row drive writes after it: here the
// head row of a catch-up whose verify command passed.
func TestTheCatchUpVerifyTimeIsOnTheRowAfterIt(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "elsewhere.txt", "theirs")
	if got := driveAll(t)[0]; got.CatchUp == nil || got.CatchUp.Verify != catchUpPassed {
		t.Fatalf("item 1 updated as %+v, want a passed verify command", got.CatchUp)
	}
	var timed []ledger.Row
	for _, r := range loadLedger(t, dir).Rows {
		if r.VerifyMS > 0 {
			timed = append(timed, r)
		}
	}
	if len(timed) != 1 || timed[0].ID != "1" || timed[0].Head == "" || timed[0].Branch == "" {
		t.Fatalf("rows carrying verify_ms: %+v, want item 1's head row alone", timed)
	}
}

// A catch-up whose verify command fails, on a trunk that passes it alone, ends
// in a changes row. drive writes that row through request-changes rather than its
// own session, so the time rides the head row it recorded for the merged tree
// before the changes row.
func TestARequestedChangesCatchUpsVerifyTimeIsNotOnTheChangesRow(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt implemented.txt")
	if got := driveAll(t)[0]; got.CatchUp == nil || got.CatchUp.Verify != catchUpFailed {
		t.Fatalf("item 1 updated as %+v, want a failed verify command", got.CatchUp)
	}
	git(t, dir, "fetch", "-q", "origin")
	merged := git(t, dir, "rev-parse", "origin/first")
	var timed []ledger.Row
	changesRequested := false
	for _, r := range loadLedger(t, dir).Rows {
		if r.ID != "1" {
			continue
		}
		if r.Changes != "" && r.Changes != "earlier" {
			changesRequested = true
			if r.VerifyMS != 0 {
				t.Errorf("the changes row carries verify_ms %d: %+v", r.VerifyMS, r)
			}
		}
		if r.VerifyMS > 0 {
			if changesRequested {
				t.Errorf("verify_ms rides a row after the request for changes: %+v", r)
			}
			timed = append(timed, r)
		}
	}
	if !changesRequested {
		t.Fatal("the drive wrote no changes row for item 1")
	}
	if len(timed) != 1 || timed[0].Head != merged {
		t.Fatalf("rows carrying verify_ms: %+v, want the head row of the merged tree %s alone", timed, merged)
	}
}

// A trunk that fails the verify command alone undoes the merge before drive
// records a head row for it, so the item's verify time rides the trunk_red park row.
func TestARedTrunksVerifyTimeRidesTheParkRow(t *testing.T) {
	isolate(t)
	ghLog := mergeProject(t)
	dir, remote := forge(t, ghLog)
	moveTrunk(t, remote, "a.txt", "a")
	t.Setenv("DROVER_TEST_CHECK_REFUSE", "a.txt")
	if _, err := driveRows(t); !errors.Is(err, errTrunkRed) {
		t.Fatalf("drive ended on %v, want %v", err, errTrunkRed)
	}
	var timed []ledger.Row
	for _, r := range loadLedger(t, dir).Rows {
		if r.ID == "1" && r.VerifyMS > 0 {
			timed = append(timed, r)
		}
	}
	if len(timed) != 1 || timed[0].Park != parkTrunkRed {
		t.Fatalf("rows carrying verify_ms: %+v, want item 1's %s park row alone", timed, parkTrunkRed)
	}
}
