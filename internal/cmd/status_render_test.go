package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// holdDrive stands in for a drive holding this checkout with a queue, the way
// the drive's own process sees it: through held.
func holdDriveQueue(t *testing.T, queue, running []string) lease.Lease {
	t.Helper()
	h, err := lease.Acquire(filepath.Join(t.TempDir(), lease.FileName), lease.Lease{Command: "drive", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(l *lease.Lease) { l.Queue, l.Running, l.Jobs = queue, running, 1 }); err != nil {
		t.Fatal(err)
	}
	held = h
	t.Cleanup(func() { held = nil; _ = h.Release() })
	return h.Current()
}

func statusJSONReport(t *testing.T, args ...string) StatusReport {
	t.Helper()
	resetStatusFlags(t)
	var err error
	out := capture(t, func() { err = execute(t, append([]string{"status", "--json", "--no-tracker"}, args...)...) })
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("status --json is not the report: %v\n%s", err, out)
	}
	return r
}

// A pending item a drive holds queued says so, with the drive's pid and its
// place, in the document and in the table: the lease is the only place another
// process can learn what a drive is about to do.
func TestStatusMarksWhatADriveHasQueued(t *testing.T) {
	stubPATH(t)
	t.Setenv("NO_COLOR", "1")
	s := scopeProject(t)
	t.Chdir(s.cfg.Root)
	l := holdDriveQueue(t, []string{"B1", "B2"}, nil)

	r := statusJSONReport(t, "--all")
	for _, row := range r.Items {
		switch row.ID {
		case "B2":
			if row.Queued == nil || row.Queued.PID != l.PID || row.Queued.Position != 2 {
				t.Errorf("B2 queued = %+v, want pid %d position 2", row.Queued, l.PID)
			}
		case "B1":
			if row.Queued != nil {
				t.Errorf("B1 is merged and still reads queued: %+v", row.Queued)
			}
		}
	}
	if r.Lease == nil || strings.Join(r.Lease.Queue, " ") != "B1 B2" {
		t.Errorf("lease = %+v, want the drive's queue", r.Lease)
	}

	rows := s.rows()
	s.attachDrive(rows, l, time.Now())
	var buf bytes.Buffer
	s.printTable(&buf, rows, nil)
	got := buf.String()
	if !strings.Contains(got, "queued 2") || !strings.Contains(got, "queued:  B1 B2") {
		t.Errorf("the table names no queue:\n%s", got)
	}
}

// timedRuns is n items that each ran implement for 10 minutes and review for 5.
func timedRuns(n int) []ledger.Row {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	var rows []ledger.Row
	for i := range n {
		id := "T" + string(rune('a'+i))
		at := func(m int) string {
			return base.Add(time.Duration(i)*time.Hour + time.Duration(m)*time.Minute).Format(time.RFC3339)
		}
		rows = append(rows,
			ledger.Row{ID: id, Stage: ledger.StageImplement, TS: at(0)},
			ledger.Row{ID: id, Stage: ledger.StageReview, TS: at(10)},
			ledger.Row{ID: id, Pass: "passed", TS: at(15)})
	}
	return rows
}

// An estimate needs five timed runs of each stage. Below that it is absent
// rather than a guess; at five it is the medians summed from now, with a queued
// item waiting for the slot ahead of it.
func TestETAIsAbsentBelowFiveSamplesAndPresentAtFive(t *testing.T) {
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	l := lease.Lease{Command: "drive", PID: 7, Started: now.Add(-time.Hour).Format(time.RFC3339),
		Jobs: 1, Running: []string{"A"}, Queue: []string{"B"}}
	fresh := func() []Row {
		return []Row{
			{ID: "A", State: stateReady, Stage: ledger.StageImplement, StageStarted: now.Add(-4 * time.Minute).Format(time.RFC3339)},
			{ID: "B", State: stateReady},
		}
	}

	s := &session{ledger: &ledger.Ledger{Rows: timedRuns(4)}}
	rows := fresh()
	s.attachDrive(rows, l, now)
	for _, r := range rows {
		if r.ETA != "" {
			t.Errorf("%s eta %s from 4 samples; want absent", r.ID, r.ETA)
		}
	}
	if rows[1].Queued == nil {
		t.Error("the queue mark must not wait on the samples the estimate needs")
	}

	s.ledger.Rows = timedRuns(5)
	rows = fresh()
	s.attachDrive(rows, l, now)
	// A is 4 minutes into a 10-minute implement, with a 5-minute review after.
	if want := now.Add(11 * time.Minute).Format(time.RFC3339); rows[0].ETA != want {
		t.Errorf("running A eta %s, want %s", rows[0].ETA, want)
	}
	// B waits for A's slot, then takes a whole run.
	if want := now.Add(26 * time.Minute).Format(time.RFC3339); rows[1].ETA != want {
		t.Errorf("queued B eta %s, want %s", rows[1].ETA, want)
	}
}

// A template reads the object --json emits under the names --json gives it,
// so every key of every item is reachable by the name a JSON consumer uses.
func TestTemplateSeesTheJSONFieldNames(t *testing.T) {
	stubPATH(t)
	s := scopeProject(t)
	t.Chdir(s.cfg.Root)
	holdDriveQueue(t, []string{"B2"}, nil)
	tmpl := filepath.Join(t.TempDir(), "keys.tmpl")
	write(t, tmpl, "{{range .items}}{{.id}}:{{range $k, $v := .}}{{$k}},{{end}}\n{{end}}lease:{{.lease.command}} now:{{.now}}\n")

	report := statusJSONReport(t, "--all")
	raw, _ := json.Marshal(report)
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	resetStatusFlags(t)
	var err error
	out := capture(t, func() { err = execute(t, "status", "--no-tracker", "--all", "--template", tmpl) })
	if err != nil {
		t.Fatalf("status --template: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(doc.Items)+1 {
		t.Fatalf("template rendered %d lines for %d items:\n%s", len(lines), len(doc.Items), out)
	}
	for i, item := range doc.Items {
		var keys []string
		for k := range item {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := item["id"].(string) + ":" + strings.Join(keys, ",") + ","
		if lines[i] != want {
			t.Errorf("template saw %q, --json carries %q", lines[i], want)
		}
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "lease:drive now:") {
		t.Errorf("the report's own fields or now are missing: %q", last)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(last, "lease:drive now:")); err != nil {
		t.Errorf("now is not an RFC 3339 time: %q", last)
	}
}

// A broken template exits 1 naming its file and the line, and is refused
// beside --json rather than one of the two quietly winning.
func TestTemplateErrorsNameTheFileAndLine(t *testing.T) {
	stubPATH(t)
	t.Chdir(scopeProject(t).cfg.Root)
	tmpl := filepath.Join(t.TempDir(), "bad.tmpl")
	write(t, tmpl, "fine\n{{.items | nope}}\n")
	resetStatusFlags(t)
	err := execute(t, "status", "--no-tracker", "--template", tmpl)
	if err == nil || Code(err) != ExitError || !strings.Contains(err.Error(), tmpl+":2") {
		t.Errorf("err = %v (exit %d), want exit 1 naming %s:2", err, Code(err), tmpl)
	}
	resetStatusFlags(t)
	if err := execute(t, "status", "--json", "--template", tmpl); err == nil {
		t.Error("--json and --template together were accepted")
	}
}

// -o refuses a file already there, and with --force replaces it by rename: the
// path then names a different file rather than the old one rewritten, which is
// what keeps a reader from ever seeing half of either.
func TestOutputRefusesAnExistingFileAndReplacesItAtomically(t *testing.T) {
	stubPATH(t)
	t.Chdir(scopeProject(t).cfg.Root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dest := filepath.Join(home, "status.json")
	write(t, dest, "old\n")
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}

	resetStatusFlags(t)
	err = execute(t, "status", "--json", "--no-tracker", "-o", "~/status.json")
	if _, ok := errors.AsType[Refusal](err); !ok {
		t.Fatalf("an existing file: err = %v, want a refusal (exit 3)", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "old\n" {
		t.Fatalf("a refused write changed the file: %q", b)
	}

	resetStatusFlags(t)
	if err := execute(t, "status", "--json", "--no-tracker", "-o", "~/status.json", "--force"); err != nil {
		t.Fatalf("--force: %v", err)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the file was rewritten in place; a reader could have seen half of it")
	}
	var r StatusReport
	if b, _ := os.ReadFile(dest); json.Unmarshal(b, &r) != nil || len(r.Items) == 0 {
		t.Errorf("the replacement is not the report: %s", b)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Errorf("the write left %d entries beside it, want only the file", len(entries))
	}
}

// A drive keeps every [status] render pair current and reports a pair that
// fails once, without that failure reaching the drive: the same two items run,
// and the drive still exits 0.
func TestADriveRendersAndAFailedRenderLeavesItsExitAlone(t *testing.T) {
	t.Cleanup(func() { driveJSON, driveNoMerge, driveFor, driveUntil, driveMax = false, false, 0, "", 0 })
	mergeProject(t)
	dir, _ := os.Getwd()
	out := t.TempDir()
	good, view := filepath.Join(out, "good.tmpl"), filepath.Join(out, "status.md")
	write(t, good, "{{range .items}}{{.id}} {{.state}}{{with .queued}} queued {{.position}}{{end}}{{with .stage}} stage {{.}}{{end}}\n{{end}}")
	cfgPath := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfgPath, string(raw)+"\n[[status.render]]\ntemplate = \""+good+"\"\npath = \""+view+"\"\n"+
		"\n[[status.render]]\ntemplate = \""+filepath.Join(out, "missing.tmpl")+"\"\npath = \""+filepath.Join(out, "never.md")+"\"\n")
	git(t, dir, "commit", "-q", "-am", "render")
	git(t, dir, "push", "-q", "origin", "main")

	// The implementing session copies the lease and the view as they stand
	// while it runs, which is the only moment the queue is not empty.
	seen := filepath.Join(out, "seen")
	t.Setenv("DROVER_TEST_SEEN", seen)
	t.Setenv("DROVER_TEST_VIEW", view)
	bin := filepath.Dir(mustLook(t, "impl"))
	implPath := filepath.Join(bin, "impl")
	body, _ := os.ReadFile(implPath) // #nosec G304 -- the fixture's own stub
	stub(t, implPath, strings.Replace(string(body), "#!/bin/sh\n",
		"#!/bin/sh\n{ cat \"$DROVER_ROOT/.git/"+lease.FileName+"\"; echo; cat \"$DROVER_TEST_VIEW\"; echo ---; } >> \"$DROVER_TEST_SEEN\"\n", 1))

	var rows []DriveItem
	stderr := captureErr(t, func() { rows = driveAll(t) })
	if len(rows) != 2 {
		t.Fatalf("drive reported %d items, want 2", len(rows))
	}
	if n := strings.Count(stderr, "missing.tmpl"); n != 1 {
		t.Errorf("the failed render was reported %d times, want once:\n%s", n, stderr)
	}

	got, err := os.ReadFile(seen) // #nosec G304 -- written by the stub above
	if err != nil {
		t.Fatalf("the implementing session saw nothing: %v", err)
	}
	first := strings.SplitN(string(got), "---", 2)[0]
	var l lease.Lease
	if err := json.NewDecoder(strings.NewReader(first)).Decode(&l); err != nil {
		t.Fatalf("the lease during item 1 did not parse: %v\n%s", err, first)
	}
	if strings.Join(l.Queue, " ") != "2" || strings.Join(l.Running, " ") != "1" {
		t.Errorf("during item 1 the lease held queue %v running %v, want [2] [1]", l.Queue, l.Running)
	}
	if !strings.Contains(first, "2 ready queued 1") || !strings.Contains(first, "1 ready stage implement") {
		t.Errorf("the view during item 1 does not show the queue and the stage:\n%s", first)
	}
	if _, err := os.Stat(view); err != nil {
		t.Errorf("the view is gone after the drive: %v", err)
	}
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if p := filepath.Join(d, name); fileExists(p) {
			return p
		}
	}
	t.Fatalf("%s is not on PATH", name)
	return ""
}

// viewProject is a backlog with one merged item, a parked one, one blocked
// by it, and an unwritten scaffold, its ledger written by a writer at UTC.
func viewProject(t *testing.T, statusTOML string) *session {
	t.Helper()
	dir := shelfProject(t, map[string]string{
		"B01-merged.md":   body("Merged", ""),
		"B02-parked.md":   body("Parked", ""),
		"B03-blocked.md":  body("Blocked", "B2"),
		"B04-scaffold.md": "---\ntitle: Scaffold\ndeps:\n---\nSTOP: requirements not yet written\n\n1.\n",
	}, nil)
	ledgerRows := strings.Join([]string{
		`{"id":"B1","stage":"implement","date":"2026-01-15","ts":"2026-01-15T17:00:00Z"}`,
		`{"id":"B1","merged":true,"date":"2026-01-15","ts":"2026-01-15T18:00:00Z"}`,
		`{"id":"drive","drive":"D1","label":"night run","queued":["B2"],"date":"2026-07-15","ts":"2026-07-15T16:59:00Z"}`,
		`{"id":"B2","stage":"implement","drive":"D1","date":"2026-07-15","ts":"2026-07-15T17:00:00Z"}`,
		`{"id":"B2","park":"no_commits","detail":"runner_exit","date":"2026-07-15","ts":"2026-07-15T17:30:00Z"}`,
	}, "\n") + "\n"
	write(t, filepath.Join(dir, "ledger.jsonl"), ledgerRows)
	if statusTOML != "" {
		conf, err := os.ReadFile(filepath.Join(dir, ".drover.toml")) // #nosec G304 -- the fixture's own config
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, ".drover.toml"), string(conf)+"\n[status]\n"+statusTOML+"\n")
	}
	isolate(t)
	return openIn(t, dir)
}

func renderString(t *testing.T, s *session, src string, now time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "view.tmpl")
	write(t, path, src)
	out, err := s.renderTemplate(path, s.statusReport(false, false, now), now)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(out)
}

// A ledger written at UTC reads in the project's zone, at the offset the date
// had there: Los Angeles is -08:00 in January and -07:00 in July. --tz names
// another zone for one call.
func TestStatusTimesReadInTheConfiguredZone(t *testing.T) {
	stubPATH(t)
	viewProject(t, `timezone = "America/Los_Angeles"`)

	report := statusJSONReport(t, "--all")
	started := map[string]string{}
	for _, r := range report.Items {
		started[r.ID] = r.Started
	}
	if len(report.Drives) != 1 || report.Drives[0].Started != "2026-07-15T09:59:00-07:00" {
		t.Errorf("drives = %+v, want D1 started 2026-07-15T09:59:00-07:00", report.Drives)
	}
	if got := started["B1"]; got != "2026-01-15T09:00:00-08:00" {
		t.Errorf("January row started %q, want 2026-01-15T09:00:00-08:00", got)
	}
	if got := started["B2"]; got != "2026-07-15T10:00:00-07:00" {
		t.Errorf("July row started %q, want 2026-07-15T10:00:00-07:00", got)
	}
	for _, r := range statusJSONReport(t, "--all", "--tz", "UTC").Items {
		if r.ID == "B2" && r.Started != "2026-07-15T17:00:00Z" {
			t.Errorf("--tz UTC started %q, want 2026-07-15T17:00:00Z", r.Started)
		}
	}
	resetStatusFlags(t)
	if err := execute(t, "status", "--json", "--no-tracker", "--tz", "Mars/Olympus"); err == nil || !strings.Contains(err.Error(), "--tz") {
		t.Errorf("an unknown --tz was not refused naming the flag: %v", err)
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	s.offline = true
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	if got := renderString(t, s, "{{.now}}", now); got != "2026-01-20T04:00:00-08:00" {
		t.Errorf("now rendered %q, want 2026-01-20T04:00:00-08:00", got)
	}
}

// Each template function renders the example docs/automation.md gives it.
func TestTemplateFunctionsRenderTheirDocumentedExamples(t *testing.T) {
	stubPATH(t)
	s := viewProject(t, "")
	s.offline = true
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	s.zone = la
	repo := "https://github.com/acme/widgets"
	s.forgeURL = &repo
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	for src, want := range map[string]string{
		`{{class "ready"}}`:                                     "ready",
		`{{class "implemented"}}`:                               "running",
		`{{class "blocked"}}`:                                   "blocked",
		`{{class "no_commits"}}`:                                "parked",
		`{{class "trunk_red"}}`:                                 "needs-operator",
		`{{class "scaffold"}}`:                                  "scaffold",
		`{{class "verified"}}`:                                  "done",
		`{{class "broken"}}`:                                    "broken",
		`{{class "UNAUDITED"}}`:                                 "unaudited",
		`{{ago "2026-10-17T11:46:00Z"}}`:                        "14m ago",
		`{{ago "2026-10-17T10:00:00Z"}}`:                        "2h ago",
		`{{ago "2026-10-17T12:30:00Z"}}`:                        "in 30m",
		`{{ago ""}}`:                                            "",
		`{{dur "2026-10-17T09:00:00Z" "2026-10-17T10:12:00Z"}}`: "1h 12m",
		`{{time "Jan 2 15:04 MST" "2026-10-17T16:30:00Z"}}`:     "Oct 17 09:30 PDT",
		`{{anchor "E37"}}`:                                      "e37",
		`{{pr 236}}`:                                            "https://github.com/acme/widgets/pull/236",
		`{{issue 12}}`:                                          "https://github.com/acme/widgets/issues/12",
		`{{md "a|b"}}`:                                          `a\|b`,
		`{{html "<b>&"}}`:                                       "&lt;b&gt;&amp;",
		`{{range .items}}{{if eq .id "B2"}}{{class .park}} {{ago .started}}{{end}}{{end}}`: "parked 93d ago",
	} {
		if got := renderString(t, s, src, now); got != want {
			t.Errorf("%s rendered %q, want %q", src, got, want)
		}
	}

	group := `{{range .drives}}{{.id}} {{.label}}:{{$d := .id}}{{range $.items}}{{if eq (or .drive "") $d}} {{.id}}{{end}}{{end}}{{end}}`
	if got := renderString(t, s, group, now); got != "D1 night run: B2" {
		t.Errorf("grouping by drive rendered %q, want %q", got, "D1 night run: B2")
	}

	s.forgeURL = nil
	if got := renderString(t, s, "{{pr 236}}", now); got != "" {
		t.Errorf("an offline render with no forge address linked %q, want empty", got)
	}
}

// An item blocked by another carries it in missing and deps, and the blocker
// carries the item in blocks, in --json and in the template data alike.
func TestDepsAndBlocksLinkBothDirections(t *testing.T) {
	stubPATH(t)
	s := viewProject(t, "")
	byID := map[string]Row{}
	for _, r := range statusJSONReport(t, "--all").Items {
		byID[r.ID] = r
	}
	if b3 := byID["B3"]; b3.State != stateBlocked || !slices.Equal(b3.Missing, []string{"B2"}) || !slices.Equal(b3.Deps, []string{"B2"}) {
		t.Errorf("B3 = state %s missing %v deps %v, want blocked by B2 in both", b3.State, b3.Missing, b3.Deps)
	}
	if b2 := byID["B2"]; !slices.Equal(b2.Blocks, []string{"B3"}) {
		t.Errorf("B2 blocks %v, want [B3]", b2.Blocks)
	}
	s.offline = true
	got := renderString(t, s, `{{range .items}}{{.id}}<{{range .deps}}{{.}}{{end}}>{{range .blocks}}{{.}}{{end}};{{end}}`, time.Now())
	if !strings.Contains(got, "B2<>B3;") || !strings.Contains(got, "B3<B2>;") {
		t.Errorf("the template data does not link B2 and B3: %q", got)
	}
}

// A parked item and an unwritten scaffold each appear in attention with the
// command that answers them, and a register the render could not read is an
// entry saying so rather than no entries.
func TestAttentionListsWhatWaitsOnAPerson(t *testing.T) {
	stubPATH(t)
	s := viewProject(t, "")
	s.offline = true
	got := renderString(t, s, "{{range .attention}}{{.kind}}|{{with .id}}{{.}}{{end}}|{{with .since}}{{.}}{{end}}|{{.command}}\n{{end}}", time.Now())
	for _, want := range []string{
		"unaudited|B1|2026-01-15T18:00:00Z|drover audit B1 verified --code-by agent",
		"parked|B2|2026-07-15T17:30:00Z|drover drive B2\n",
		"scaffold|B4||write B4's requirements, then drover accept B4\n",
		"follow-up|||drover follow-ups\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("attention lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "|B3|") {
		t.Errorf("a blocked item waits on its dep, not a person:\n%s", got)
	}
}
