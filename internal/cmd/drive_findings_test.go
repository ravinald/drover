package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// findingsProject is mergeProject on a file register, with the review stub
// replaced by one that files what the test hands it: DROVER_TEST_ROW is a
// register line it appends when the body carries FILES-ROW, and
// DROVER_TEST_SCAFFOLD a body it writes to work/03-found.md when the body
// carries FILES-SCAFFOLD. Every review records an audit for the item whose body
// carries REVIEW-PASSES-<id>, so the item reaches the end of the drive green.
// supervise is a [supervise] block, empty for the catalog defaults.
func findingsProject(t *testing.T, supervise string) string {
	t.Helper()
	t.Cleanup(func() { driveEvents = false })
	mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, rev, `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false}'
id=$(printf '%s' "$*" | sed -n 's/.*REVIEW-PASSES-\([0-9][0-9]*\).*/\1/p' | head -n 1)
if [ -n "$id" ]; then
  printf '{"id":"%s","level":"reviewed","code_by":"agent","independent":true,"date":"2026-10-19"}\n' "$id" >> "$DROVER_ROOT/ledger.jsonl"
fi
case "$*" in *FILES-ROW*)
  if [ -n "${DROVER_TEST_ROW:-}" ]; then printf '%s\n' "$DROVER_TEST_ROW" >> "$DROVER_ROOT/.drover/register.jsonl"; fi ;;
esac
case "$*" in *FILES-SCAFFOLD*)
  if [ -n "${DROVER_TEST_SCAFFOLD:-}" ]; then printf '%s' "$DROVER_TEST_SCAFFOLD" > "$DROVER_ROOT/work/03-found.md"; fi ;;
esac
`)
	cfgPath := filepath.Join(dir, ".drover.toml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	conf := strings.Replace(string(raw), "repo         = \"owner/example\"\n", "", 1)
	write(t, cfgPath, conf+"\n[register]\nbackend = \"file\"\n"+supervise)
	ignore, err := os.ReadFile(filepath.Join(dir, ".gitignore")) // #nosec G304 -- the fixture's own file
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".gitignore"), string(ignore)+".drover/\n")
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES-1 FILES-ROW FILES-SCAFFOLD\n")
	if err := os.MkdirAll(filepath.Join(dir, ".drover"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".drover", "register.jsonl"), "")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "a file register")
	git(t, dir, "push", "-q", "origin", "main")
	return dir
}

// driveStream is one drive's stdout under --json --events: each item's report,
// each event, and the totals object.
type driveStream struct {
	items  []DriveItem
	events []DriveEvent
	totals *DriveTotals
}

func driveFindings(t *testing.T, args ...string) driveStream {
	t.Helper()
	out := capture(t, func() {
		if err := execute(t, append([]string{"drive", "--json", "--no-merge"}, args...)...); err != nil {
			t.Fatalf("drive: %v", err)
		}
	})
	var st driveStream
	for _, line := range strings.Split(out, "\n") {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		switch {
		case probe["event"] != nil:
			var e DriveEvent
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatal(err)
			}
			st.events = append(st.events, e)
		case probe["outcome"] != nil:
			var d DriveItem
			if err := json.Unmarshal([]byte(line), &d); err != nil {
				t.Fatal(err)
			}
			st.items = append(st.items, d)
		case probe["findings_by_coverage"] != nil:
			var tot DriveTotals
			if err := json.Unmarshal([]byte(line), &tot); err != nil {
				t.Fatal(err)
			}
			st.totals = &tot
		}
	}
	return st
}

func (st driveStream) item(t *testing.T, id string) DriveItem {
	t.Helper()
	for _, d := range st.items {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no report for %s: %+v", id, st.items)
	return DriveItem{}
}

func (st driveStream) of(event, action string) []DriveEvent {
	var out []DriveEvent
	for _, e := range st.events {
		if e.Event == event && (action == "" || e.Action == action) {
			out = append(out, e)
		}
	}
	return out
}

func registerLine(number, owners string) string {
	line := `{"number":` + number + `,"title":"a finding","severity":"M",`
	if owners != "" {
		line += `"owners":[` + owners + `],`
	}
	return line + `"state":"open","date":"2026-10-19"}`
}

// A review that files a register row is a finding the drive has to report.
func TestAReviewThatFilesARegisterRowIsReported(t *testing.T) {
	findingsProject(t, "")
	t.Setenv("DROVER_TEST_ROW", registerLine("5", `"2"`))
	st := driveFindings(t, "1", "--events")

	filed := st.of(evFiled, "")
	if len(filed) != 1 || filed[0].Kind != findingRegister || filed[0].Ref != "5" || !slices.Equal(filed[0].FiledBy, []string{"1"}) {
		t.Fatalf("filed events = %+v, want one register row #5 filed by 1", filed)
	}
	d := st.item(t, "1")
	if len(d.Findings) != 1 || d.Findings[0].Ref != "5" || d.Findings[0].Kind != findingRegister {
		t.Errorf("item 1's report carries findings %+v, want register row #5", d.Findings)
	}
	if st.totals == nil {
		t.Fatal("the drive's --json stream ends with no totals object")
	}
	exit := st.of(evExit, "")
	if len(exit) != 1 || exit[0].DriveTotals == nil || exit[0].FindingsByCoverage[d.Findings[0].Coverage] != 1 {
		t.Errorf("the exit event does not total the finding: %+v", exit)
	}
}

// An owner that merged long ago: the row routes to it, and no run is left to
// carry it.
func TestARowRoutedToAMergedOwnerIsDormantAndStopsForTheOperator(t *testing.T) {
	dir := findingsProject(t, "")
	write(t, filepath.Join(dir, "work", "04-old.md"), "---\ntitle: Old\ndeps:\n---\n\n1. Long done.\n")
	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- the fixture's ledger
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"4","merged":true,"date":"2026-09-01"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Setenv("DROVER_TEST_ROW", registerLine("6", `"4"`))
	st := driveFindings(t, "1", "--events")

	d := st.item(t, "1")
	if len(d.Findings) != 1 || d.Findings[0].Coverage != coverageOrphaned {
		t.Fatalf("findings = %+v, want #6 orphaned", d.Findings)
	}
	stops := st.of(evNeedsOperator, "triage_finding")
	if len(stops) != 1 || stops[0].Ref != "6" || stops[0].Coverage != coverageOrphaned || !strings.Contains(stops[0].Reason, "filed by 1") {
		t.Errorf("needs_operator under triage_finding = %+v, want one naming #6, item 1 and orphaned", stops)
	}
	if st.totals == nil || len(st.totals.FindingsNeedingTriage) != 1 || st.totals.FindingsNeedingTriage[0].Ref != "6" {
		t.Errorf("totals = %+v, want #6 listed for coverage", st.totals)
	}
}

// A row routed to an item this drive will reach next needs nobody.
func TestARowRoutedToAQueuedOwnerIsQueuedAndAsksNothing(t *testing.T) {
	findingsProject(t, "")
	t.Setenv("DROVER_TEST_ROW", registerLine("7", `"2"`))
	st := driveFindings(t, "1", "2", "--events")

	d := st.item(t, "1")
	if len(d.Findings) != 1 || d.Findings[0].Coverage != coverageQueued {
		t.Fatalf("findings = %+v, want #7 queued", d.Findings)
	}
	if got := append(st.of(evNeedsOperator, "triage_finding"), st.of(evNeedsSupervisor, "triage_finding")...); len(got) != 0 {
		t.Errorf("a queued row stopped for somebody: %+v", got)
	}
}

func TestARowWithNoRoutingLabelIsUnrouted(t *testing.T) {
	findingsProject(t, "")
	t.Setenv("DROVER_TEST_ROW", registerLine("8", ""))
	st := driveFindings(t, "1", "--events")

	d := st.item(t, "1")
	if len(d.Findings) != 1 || d.Findings[0].Coverage != coverageUnowned {
		t.Fatalf("findings = %+v, want #8 unowned", d.Findings)
	}
	if len(st.of(evNeedsOperator, "triage_finding")) != 1 {
		t.Errorf("an unowned row did not stop for the operator: %+v", st.events)
	}
}

// A scaffold keeps the stop it always had, and is not stopped a second time
// under triage_finding.
func TestAScaffoldIsAttributedByItsProvenanceLine(t *testing.T) {
	findingsProject(t, "\n[supervise]\nwrite_scaffold_requirements = \"operator\"\n")
	t.Setenv("DROVER_TEST_SCAFFOLD", "---\ntitle: Found\ndeps:\n---\n\n"+
		strings.Join(stopBlock("STOP: requirements not yet written"), "\n")+"\n"+
		"Found while working 1, First.\n\nRequirements:\n\n1.\n")
	st := driveFindings(t, "1", "--events")

	filed := st.of(evFiled, "")
	if len(filed) != 1 || filed[0].Kind != findingScaffold || filed[0].Ref != "3" ||
		filed[0].Coverage != coverageScaffold || !slices.Equal(filed[0].FiledBy, []string{"1"}) {
		t.Fatalf("filed events = %+v, want scaffold 3 filed by 1", filed)
	}
	d := st.item(t, "1")
	if len(d.Findings) != 1 || d.Findings[0].Ref != "3" {
		t.Errorf("item 1's findings = %+v, want scaffold 3", d.Findings)
	}
	stops := st.of(evNeedsOperator, "")
	if len(stops) != 1 || stops[0].Action != "write_scaffold_requirements" || stops[0].ID != "3" {
		t.Errorf("needs_operator = %+v, want one, under write_scaffold_requirements, for 3", stops)
	}
}

// Under --jobs the register carries no author, so a row that appeared while two
// items ran names both and says so.
func TestARowFiledWhileTwoItemsRanIsAttributedToBothAsAmbiguous(t *testing.T) {
	dir := findingsProject(t, "")
	impl, err := exec.LookPath("impl")
	if err != nil {
		t.Fatal(err)
	}
	sync := t.TempDir()
	t.Setenv("DROVER_TEST_SYNC", sync)
	t.Setenv("DROVER_TEST_IMPL_ROW", registerLine("9", `"4"`))
	stub(t, impl, `#!/bin/sh
id=$(basename "$(pwd)")
wait_for() { n=0; while [ ! -e "$1" ] && [ $n -lt 200 ]; do sleep 0.05; n=$((n + 1)); done; }
case "$*" in
  *FILER*)
    wait_for "$DROVER_TEST_SYNC/other"
    printf '%s\n' "$DROVER_TEST_IMPL_ROW" >> "$DROVER_ROOT/.drover/register.jsonl"
    touch "$DROVER_TEST_SYNC/filed" ;;
  *OTHER*)
    touch "$DROVER_TEST_SYNC/other"
    wait_for "$DROVER_TEST_SYNC/filed" ;;
esac
echo work > "implemented-$id.txt"
git add "implemented-$id.txt"
git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m "the run did $id"
echo '{"type":"result","subtype":"success","is_error":false}'
`)
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it. FILER\n")
	write(t, filepath.Join(dir, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Do it too. OTHER\n")
	st := driveFindings(t, "1", "2", "--jobs", "2", "--events")

	for _, id := range []string{"1", "2"} {
		d := st.item(t, id)
		if len(d.Findings) != 1 || d.Findings[0].Ref != "9" || !d.Findings[0].Ambiguous ||
			!slices.Equal(d.Findings[0].FiledBy, []string{"1", "2"}) {
			t.Errorf("item %s's findings = %+v, want #9 filed by 1 and 2, ambiguous", id, d.Findings)
		}
	}
	if filed := st.of(evFiled, ""); len(filed) != 1 || !filed[0].Ambiguous || filed[0].ID != "" {
		t.Errorf("filed events = %+v, want one, ambiguous, naming no single id", filed)
	}
}

// A register that will not read is reported on the item and decides nothing
// about it.
func TestARegisterThatWillNotReadLeavesTheOutcomeAlone(t *testing.T) {
	findingsProject(t, "")
	t.Setenv("DROVER_TEST_ROW", `{"number":`)
	st := driveFindings(t, "1")

	d := st.item(t, "1")
	if d.Outcome != outcomeReviewed {
		t.Errorf("outcome = %q (%s), want %q: a register read decided the item", d.Outcome, d.Reason, outcomeReviewed)
	}
	if !strings.Contains(d.RegisterError, "follow-up list would not read") {
		t.Errorf("register_error = %q, want the failed read named", d.RegisterError)
	}
}

// status reads the findings from the register, the ledger and the backlog, with
// no drive running, and judges each again on every read: a row whose owner
// merges after it was filed reads dormant from then on.
func TestStatusListsFindingsNeedingTriageWithNoDrive(t *testing.T) {
	t.Cleanup(func() { statusJSON, registerCoverage, registerJSON = false, false, false })
	dir := fileRegisterProject(t, "",
		registerLine("1", `"B1"`),
		registerLine("2", ""),
		registerLine("3", `"B9"`))
	write(t, filepath.Join(dir, "work", "B01-a-bug.md"), "---\ntitle: a bug\ndeps:\n---\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "B02-found.md"), "---\ntitle: Found\ndeps:\n---\n\n"+
		strings.Join(stopBlock("STOP: requirements not yet written"), "\n")+"\nFound while working B1, a bug.\n\n1.\n")

	triage := func() map[string]FiledFinding {
		t.Helper()
		out := capture(t, func() {
			if err := execute(t, "status", "--json"); err != nil {
				t.Fatalf("status --json: %v", err)
			}
		})
		var report struct {
			Findings []FiledFinding `json:"findings_needing_triage"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("status --json: %v\n%s", err, out)
		}
		got := map[string]FiledFinding{}
		for _, f := range report.Findings {
			got[f.Kind+":"+f.Ref] = f
		}
		return got
	}

	got := triage()
	if _, ok := got["follow-up:1"]; ok {
		t.Errorf("#1 is owned by a ready owner and was listed for coverage: %+v", got)
	}
	if got["follow-up:2"].Coverage != coverageUnowned {
		t.Errorf("#2 = %+v, want unowned", got["follow-up:2"])
	}
	if f := got["follow-up:3"]; f.Coverage != coverageBlocked || len(f.BlockedOn["B9"]) == 0 {
		t.Errorf("#3 = %+v, want blocked, naming why B9 cannot run", f)
	}
	if f := got["scaffold:B2"]; f.Coverage != coverageScaffold || !slices.Equal(f.FiledBy, []string{"B1"}) {
		t.Errorf("scaffold B2 = %+v, want listed, filed by B1", f)
	}

	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- the fixture's ledger
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"B1","merged":true,"date":"2026-10-19"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := triage(); got["follow-up:1"].Coverage != coverageOrphaned {
		t.Errorf("after B1 merged, #1 = %+v, want orphaned", got["follow-up:1"])
	}

	out := capture(t, func() {
		if err := execute(t, "follow-ups", "--coverage"); err != nil {
			t.Fatalf("follow-ups --coverage: %v", err)
		}
	})
	for _, want := range []string{"#1", "#2", "#3", "orphaned", "unowned", "blocked; B9 waits on not in the backlog"} {
		if !strings.Contains(out, want) {
			t.Errorf("follow-ups --coverage does not say %q:\n%s", want, out)
		}
	}
}

// A drive's render on a GitHub register leaves the forge alone, so it lists the
// rows the drive last read, and before the drive has read any it says so rather
// than presenting no rows as nothing to triage.
func TestADrivesRenderListsTheRowsTheDriveLastRead(t *testing.T) {
	t.Cleanup(func() { activeDrive.Store(nil) })
	dir := registerProject(t, "[github]\nrepo = \"owner/name\"\n")
	stubGHIssues(t, stubPATH(t), onePage)
	write(t, filepath.Join(dir, "coverage.tmpl"),
		"{{len .findings_needing_triage}}|{{with .findings_error}}{{.}}{{end}}")
	r := &driveRenderer{targets: []config.Render{{Template: "coverage.tmpl", Path: "coverage.out"}}}
	rendered := func() string {
		t.Helper()
		r.render()
		raw, err := os.ReadFile(filepath.Join(dir, "coverage.out")) // #nosec G304 -- the fixture's own render
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	activeDrive.Store(&runningDrive{id: "D1"})
	if got := rendered(); !strings.HasPrefix(got, "0|") || !strings.Contains(got, "not read the follow-up list") {
		t.Errorf("before the drive read the register, the render = %q, want no rows and the reason", got)
	}
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	s.newFindingWatch().dispatched(s, "B1")
	if got := rendered(); got != "1|" {
		t.Errorf("after the drive read the register, the render = %q, want unowned #13 listed", got)
	}
}
