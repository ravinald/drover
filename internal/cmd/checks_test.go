package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
)

// checksForge is forge with its check runs answered by a script playing one
// scenario, which is how a test puts a red check on the head drive gates:
//
//	ratelimit  build fails on Docker Hub's rate limit until its run is rerun
//	trunk      vuln fails, and fails on main's latest completed run too
//	behind     vuln fails; main's tip, by name or $DROVER_TEST_TRUNK_TIP, is
//	           still running it, and every commit before the tip failed it
//	pending    vuln fails; no trunk commit has a completed run of it
//	stale      vuln fails; main and every commit but $DROVER_TEST_TRUNK_OLD
//	           completed build alone, and $DROVER_TEST_TRUNK_OLD failed vuln
//	paged      vuln fails, and fails on main too, on the second page of
//	           main's check runs, which only gh api --paginate prints
//	disk       build fails out of disk space however many times it is rerun
//	none       no check run at all
//	nosteps    build fails with no step run, however often it is read
//	recovers   build fails with no step run on a head's first read, and every
//	           later read of that head finds it green
//	infratrunk the first head read fails build with no step run; every other
//	           head fails vuln, which main fails too
//	joblog     build fails with $DROVER_TEST_CHECKS_LOG as its job log
//
// gh run rerun counts into reruns.
func checksForge(t *testing.T, scenario string) string {
	t.Helper()
	ghLog := mergeProject(t)
	forge(t, ghLog)
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	poll := checksPoll
	checksPoll = time.Millisecond
	t.Cleanup(func() { checksPoll = poll })

	state := t.TempDir()
	t.Setenv("DROVER_TEST_TRUNK_TIP", git(t, ".", "rev-parse", "main"))
	script := filepath.Join(state, "checks")
	t.Setenv("DROVER_TEST_CHECKS_STATE", state)
	t.Setenv("DROVER_TEST_CHECKS_SCENARIO", scenario)
	stub(t, script, `#!/bin/sh
reruns=$(cat "$DROVER_TEST_CHECKS_STATE/reruns" 2>/dev/null || echo 0)
case "$*" in
  'run rerun '*)
    echo $((reruns + 1)) > "$DROVER_TEST_CHECKS_STATE/reruns" ;;
  *'/actions/jobs/'*'/logs')
    case "$DROVER_TEST_CHECKS_SCENARIO" in
      ratelimit) echo '2026-10-17T01:02:03.4567890Z Error response from daemon: toomanyrequests: You have reached your pull rate limit.' ;;
      disk) echo '2026-10-17T01:02:03.4567890Z write /tmp/go-build: No space left on device' ;;
      joblog) cat "$DROVER_TEST_CHECKS_LOG" ;;
      *) echo '2026-10-17T01:02:03.4567890Z GO-2026-6617 found in golang.org/x/net' ;;
    esac ;;
  *'/actions/jobs/'*)
    case "$DROVER_TEST_CHECKS_SCENARIO:$*" in
      nosteps:*|recovers:*|infratrunk:*/jobs/40) echo '{"steps":[]}' ;;
      *) echo '{"steps":[{"name":"Set up job","conclusion":"success"},{"name":"test","conclusion":"failure"}]}' ;;
    esac ;;
  *'/commits/main/check-runs'*)
    case "$DROVER_TEST_CHECKS_SCENARIO" in
      trunk|infratrunk) echo '{"total_count":1,"check_runs":[{"id":99,"name":"vuln","status":"completed","conclusion":"failure","html_url":"https://github.com/o/r/runs/99","completed_at":"2026-10-17T00:00:00Z"}]}' ;;
      behind|pending) echo '{"total_count":1,"check_runs":[{"id":97,"name":"vuln","status":"in_progress","conclusion":null}]}' ;;
      paged)
        echo '{"total_count":101,"check_runs":[{"id":93,"name":"build","status":"completed","conclusion":"success","completed_at":"2026-10-17T00:00:00Z"}]}'
        case "$*" in
          *--paginate*) echo '{"total_count":101,"check_runs":[{"id":94,"name":"vuln","status":"completed","conclusion":"failure","html_url":"https://github.com/o/r/runs/94","completed_at":"2026-10-17T00:00:00Z"}]}' ;;
        esac ;;
      *) echo '{"total_count":1,"check_runs":[{"id":98,"name":"build","status":"completed","conclusion":"success","completed_at":"2026-10-17T00:00:00Z"}]}' ;;
    esac ;;
  *'/check-runs?filter=all'*)
    case "$*" in
      *"/commits/$DROVER_TEST_TRUNK_TIP/"*) running=1 ;;
      *) running= ;;
    esac
    if [ "$DROVER_TEST_CHECKS_SCENARIO" = stale ]; then
      case "$*" in
        *"/commits/$DROVER_TEST_TRUNK_OLD/"*) echo '{"total_count":1,"check_runs":[{"id":92,"name":"vuln","status":"completed","conclusion":"failure","html_url":"https://github.com/o/r/runs/92","completed_at":"2026-10-16T00:00:00Z"}]}' ;;
        *) echo '{"total_count":1,"check_runs":[{"id":91,"name":"build","status":"completed","conclusion":"success","completed_at":"2026-10-17T00:00:00Z"}]}' ;;
      esac
    elif [ "$DROVER_TEST_CHECKS_SCENARIO" = behind ] && [ -z "$running" ]; then
      echo '{"total_count":1,"check_runs":[{"id":96,"name":"vuln","status":"completed","conclusion":"failure","html_url":"https://github.com/o/r/runs/96","completed_at":"2026-10-16T00:00:00Z"}]}'
    else
      echo '{"total_count":1,"check_runs":[{"id":95,"name":"vuln","status":"in_progress","conclusion":null}]}'
    fi ;;
  *'/check-runs'*)
    id=$((30 + reruns))
    sha=$(printf '%s' "$*" | sed -n 's|.*/commits/\([^/]*\)/check-runs.*|\1|p')
    case "$DROVER_TEST_CHECKS_SCENARIO" in
      none) echo '{"total_count":0,"check_runs":[]}' ; exit 0 ;;
      nosteps) name=build ;;
      recovers)
        n=$(cat "$DROVER_TEST_CHECKS_STATE/reads-$sha" 2>/dev/null || echo 0)
        echo $((n + 1)) > "$DROVER_TEST_CHECKS_STATE/reads-$sha"
        [ "$n" -gt 0 ] && { echo '{"total_count":1,"check_runs":[{"id":'$id',"name":"build","status":"completed","conclusion":"success"}]}' ; exit 0 ; }
        name=build ;;
      infratrunk)
        [ -f "$DROVER_TEST_CHECKS_STATE/first" ] || echo "$sha" > "$DROVER_TEST_CHECKS_STATE/first"
        if [ "$(cat "$DROVER_TEST_CHECKS_STATE/first")" = "$sha" ]; then id=40; name=build; else name=vuln; fi ;;
      ratelimit) [ "$reruns" -gt 0 ] && { echo '{"total_count":1,"check_runs":[{"id":'$id',"name":"build","status":"completed","conclusion":"success"}]}' ; exit 0 ; } ; name=build ;;
      trunk|behind|pending|stale|paged) name=vuln ;;
      *) name=build ;;
    esac
    echo '{"total_count":1,"check_runs":[{"id":'$id',"name":"'$name'","status":"completed","conclusion":"failure","details_url":"https://github.com/o/r/actions/runs/500/job/'$id'"}]}' ;;
  *) echo '[]' ;;
esac
`)
	t.Setenv("DROVER_TEST_CHECKS", script)
	return ghLog
}

// checkRows are the ledger's classification rows for item 1, in order.
func checkRows(t *testing.T) []ledger.Row {
	t.Helper()
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Row
	for _, r := range l.Rows {
		if r.ID == "1" && r.Class != "" {
			out = append(out, r)
		}
	}
	return out
}

func reruns(t *testing.T, ghLog string) int {
	t.Helper()
	n := 0
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "run rerun 500 --failed") {
			n++
		}
	}
	return n
}

// The supervisors' most common rerun: a job that failed pulling an image, and
// passed unchanged once rerun. drive reruns it, waits for the new attempt and
// merges, with no round spent.
func TestARateLimitedJobIsRerunAndPasses(t *testing.T) {
	ghLog := checksForge(t, "ratelimit")

	rows := driveAll(t)
	got := rows[0]
	if got.Outcome != outcomeMergedByGate || got.Rounds != 0 {
		t.Fatalf("item 1 is %s/%s rounds=%d (%s), want merged by the merge gate with no round", got.Outcome, got.Park, got.Rounds, got.Reason)
	}
	if n := reruns(t, ghLog); n != 1 {
		t.Errorf("gh run rerun ran %d times, want 1", n)
	}
	cs := checkRows(t)
	if len(cs) != 1 || cs[0].Class != ledger.ClassInfra || !cs[0].Rerun || cs[0].Check != "build" ||
		!strings.Contains(cs[0].Evidence, "toomanyrequests") || strings.HasPrefix(cs[0].Evidence, "2026-") {
		t.Errorf("classification rows %+v, want one rerun infra row for build carrying the log line without its timestamp", cs)
	}
}

// A vuln check red on main as well: no branch can fix it. The item
// parks trunk_red naming the check and main's run, nothing is sent back, the
// operator is told, and the drive stops.
func TestAFailureRedOnTheTrunkParksTrunkRedWithoutSendingChangesBack(t *testing.T) {
	checksForge(t, "trunk")
	before := ledgerCounts(t).ChangesRequested

	evs, _, err := driveEventsOf(t)
	if Code(err) == 0 {
		t.Errorf("drive exited 0 over a red trunk, want the stop a red trunk raises")
	}
	var parked, check, operator *DriveEvent
	for i, e := range evs {
		switch {
		case e.Event == evParked && e.ID == "1":
			parked = &evs[i]
		case e.Event == evCheck && e.ID == "1":
			check = &evs[i]
		case e.Event == evNeedsOperator && e.ID == "1":
			operator = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkTrunkRed || !strings.Contains(parked.Reason, "vuln") ||
		!strings.Contains(parked.Reason, "https://github.com/o/r/runs/99") {
		t.Fatalf("item 1 parked %+v, want trunk_red naming vuln and main's run", parked)
	}
	if check == nil || check.Class != ledger.ClassTrunk || check.Check != "vuln" || check.Action != "report_trunk_red" {
		t.Errorf("check event %+v, want vuln classified trunk under report_trunk_red", check)
	}
	if operator == nil || operator.Action != "report_trunk_red" {
		t.Errorf("needs_operator %+v, want one for report_trunk_red at its default operator policy", operator)
	}
	if after := ledgerCounts(t).ChangesRequested; after != before {
		t.Errorf("changes rows went from %d to %d, want none added", before, after)
	}
	if n := len(gateLog(t, ".")); n != 0 {
		t.Errorf("the merge gate ran %d times over a trunk-red check, want none", n)
	}
}

// The trunk under a drive: the last merge moved main's tip, whose run of vuln
// has not finished, while the commit before it failed vuln. That is the
// trunk's latest completed run, so the item parks trunk_red as it would had
// the tip finished red.
func TestAFailureRedOnTheTrunkBehindItsRunningTipParksTrunkRed(t *testing.T) {
	ghLog := checksForge(t, "behind")
	parent := git(t, ".", "rev-parse", "main")
	write(t, "trunk-moved.txt", "x\n")
	git(t, ".", "add", "trunk-moved.txt")
	git(t, ".", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the trunk moved")
	git(t, ".", "push", "-q", "origin", "main")
	t.Setenv("DROVER_TEST_TRUNK_TIP", git(t, ".", "rev-parse", "main"))
	before := ledgerCounts(t).ChangesRequested

	evs, _, err := driveEventsOf(t)
	if Code(err) == 0 {
		t.Errorf("drive exited 0 over a red trunk, want the stop a red trunk raises")
	}
	var parked *DriveEvent
	for i, e := range evs {
		if e.Event == evParked && e.ID == "1" {
			parked = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkTrunkRed || !strings.Contains(parked.Reason, "https://github.com/o/r/runs/96") {
		t.Fatalf("item 1 parked %+v, want trunk_red naming the run on the commit before main's tip", parked)
	}
	for _, want := range []string{parent[:7], "1 first-parent commit behind main's tip", "completed 2026-10-16T00:00:00Z"} {
		if !strings.Contains(parked.Reason, want) {
			t.Errorf("trunk_red reason %q does not name %q, so a stale read would not show", parked.Reason, want)
		}
	}
	if calls := strings.Join(ghCalls(t, ghLog), "\n"); !strings.Contains(calls, "/commits/"+parent+"/check-runs") {
		t.Errorf("no check-runs read for %s, the commit before the tip:\n%s", parent, calls)
	}
	if after := ledgerCounts(t).ChangesRequested; after != before {
		t.Errorf("changes rows went from %d to %d, want none added", before, after)
	}
	if n := len(gateLog(t, ".")); n != 0 {
		t.Errorf("the merge gate ran %d times over a trunk-red check, want none", n)
	}
}

// No trunk commit in the window has finished vuln, so nothing says the trunk
// fails it: the check is the code's, and the gate reads it.
func TestAFailureNoTrunkCommitCompletedIsTheCodes(t *testing.T) {
	checksForge(t, "pending")
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")

	rows := driveAll(t)
	if got := rows[0]; got.Park != parkGateRefused || got.Rounds != 1 {
		t.Errorf("item 1 is %s rounds=%d (%s), want gate_refused with one round", got.Park, got.Rounds, got.Reason)
	}
	cs := checkRows(t)
	if len(cs) != 1 || cs[0].Class != ledger.ClassCode || cs[0].Check != "vuln" {
		t.Errorf("classification rows %+v, want vuln classified code", cs)
	}
}

// The trunk stopped running vuln: six newer commits completed every run they
// carry and none of them is vuln, above an older commit that failed it. That
// failure says nothing about the trunk now, so the check is the code's and the
// drive goes on to the gate rather than stopping.
func TestAFailureTheTrunkNoLongerRunsIsTheCodes(t *testing.T) {
	ghLog := checksForge(t, "stale")
	old := git(t, ".", "rev-parse", "main")
	t.Setenv("DROVER_TEST_TRUNK_OLD", old)
	for i := range 6 {
		name := "trunk-moved-" + string(rune('a'+i)) + ".txt"
		write(t, name, "x\n")
		git(t, ".", "add", name)
		git(t, ".", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the trunk moved")
	}
	git(t, ".", "push", "-q", "origin", "main")
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")

	rows := driveAll(t)
	if got := rows[0]; got.Park == parkTrunkRed || got.Park != parkGateRefused || got.Rounds != 1 {
		t.Errorf("item 1 is %s rounds=%d (%s), want gate_refused with one round, never trunk_red", got.Park, got.Rounds, got.Reason)
	}
	cs := checkRows(t)
	if len(cs) != 1 || cs[0].Class != ledger.ClassCode || cs[0].Check != "vuln" {
		t.Errorf("classification rows %+v, want vuln classified code", cs)
	}
	if calls := strings.Join(ghCalls(t, ghLog), "\n"); strings.Contains(calls, "/commits/"+old+"/check-runs") {
		t.Errorf("check-runs read for %s, behind six completed commits:\n%s", old, calls)
	}
}

// main carries more than 100 check runs and its failed vuln is on the second
// page, so a read of the first page alone would find the trunk no longer runs it.
func TestAFailureOnTheTrunksSecondPageOfRunsParksTrunkRed(t *testing.T) {
	checksForge(t, "paged")

	evs, _, err := driveEventsOf(t)
	if Code(err) == 0 {
		t.Errorf("drive exited 0 over a red trunk, want the stop a red trunk raises")
	}
	var parked *DriveEvent
	for i, e := range evs {
		if e.Event == evParked && e.ID == "1" {
			parked = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkTrunkRed || !strings.Contains(parked.Reason, "https://github.com/o/r/runs/94") ||
		!strings.Contains(parked.Reason, "main's tip") {
		t.Fatalf("item 1 parked %+v, want trunk_red naming the run on the second page at main's tip", parked)
	}
}

// Two reruns of one head are what [forge] reruns allows by default. The third
// failure on it is the code's: the gate reads it and sends it back.
func TestAThirdInfraFailureAfterTwoRerunsIsTheCodes(t *testing.T) {
	ghLog := checksForge(t, "disk")
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")

	rows := driveAll(t)
	got := rows[0]
	if got.Park != parkGateRefused || got.Rounds != 1 {
		t.Errorf("item 1 is %s rounds=%d (%s), want gate_refused with one round", got.Park, got.Rounds, got.Reason)
	}
	if n := reruns(t, ghLog); n != 2 {
		t.Errorf("gh run rerun ran %d times, want 2", n)
	}
	var classes []string
	for _, r := range checkRows(t) {
		classes = append(classes, r.Class)
	}
	if strings.Join(classes, " ") != "infra infra code" {
		t.Errorf("classified %v, want infra infra code", classes)
	}
}

// A head no workflow reported on is not a red check, and
// the park says so instead of reading as a failure.
func TestAHeadWithNoRunsIsNamedAsSuch(t *testing.T) {
	checksForge(t, "none")
	gate, err := exec.LookPath("merge-gate")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_MERGE_GATE_EXIT", "2")
	stub(t, gate, "#!/bin/sh\necho 'merge-gate: no workflow reported'\nexit \"$DROVER_TEST_MERGE_GATE_EXIT\"\n")
	cfg, err := os.ReadFile(config.FileName)
	if err != nil {
		t.Fatal(err)
	}
	write(t, config.FileName, string(cfg)+"\n[forge]\nchecks_wait = \"20ms\"\n")

	rows, _ := driveRows(t)
	got := rows[0]
	if got.Park != "forge_down" || !strings.Contains(got.Reason, "no workflow ran on this head") {
		t.Errorf("item 1 parked %q (%s), want forge_down naming a head no workflow ran on", got.Park, got.Reason)
	}
	cs := checkRows(t)
	if len(cs) != 1 || cs[0].Class != ledger.ClassNone {
		t.Errorf("classification rows %+v, want one of class none", cs)
	}
}

// The same head, with a gate that blames the diff rather than the forge. The
// round goes back to the item, and both the park and the request-changes the next run
// reads say no workflow ran, so the round is spent on CI that never fired
// rather than on code.
func TestAHeadWithNoRunsIsNamedAsSuchWhenTheGateBlamesTheDiff(t *testing.T) {
	checksForge(t, "none")
	gate, err := exec.LookPath("merge-gate")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_MERGE_GATE_EXIT", "1")
	stub(t, gate, "#!/bin/sh\necho 'merge-gate: no workflow reported'\nexit \"$DROVER_TEST_MERGE_GATE_EXIT\"\n")
	cfg, err := os.ReadFile(config.FileName)
	if err != nil {
		t.Fatal(err)
	}
	write(t, config.FileName, string(cfg)+"\n[forge]\nchecks_wait = \"20ms\"\n")

	rows, _ := driveRows(t)
	got := rows[0]
	if got.Park != parkGateRefused || !strings.Contains(got.Reason, "no workflow ran on this head") {
		t.Errorf("item 1 parked %q (%s), want gate_refused naming a head no workflow ran on", got.Park, got.Reason)
	}
	body, err := os.ReadFile(filepath.Join("work", "01-first.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, sentBack, _ := strings.Cut(string(body), "## What the review sent back")
	if !strings.Contains(sentBack, "no workflow ran on this head") {
		t.Errorf("the request for changes does not say no workflow ran on this head:\n%s", sentBack)
	}
}

func ledgerCounts(t *testing.T) ledger.Counts {
	t.Helper()
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return l.Counts("1")
}

// parkLine is the stderr line drive prints the moment item id parks on cause,
// and the index it starts at in stderr, -1 when it is absent.
func parkLine(stderr, id, cause string) (string, int) {
	prefix := "  ! " + id + " parked " + cause + " at "
	i := strings.Index(stderr, prefix)
	if i < 0 {
		return "", -1
	}
	line, _, _ := strings.Cut(stderr[i:], "\n")
	return line, i
}

// headOf is the short head of the branch drive reported for item 1.
func headOf(t *testing.T, rows []DriveItem) string {
	t.Helper()
	for _, r := range rows {
		if r.ID == "1" && r.Branch != "" {
			return shortSHA(git(t, ".", "rev-parse", r.Branch))
		}
	}
	t.Fatalf("no row for item 1 carries a branch: %+v", rows)
	return ""
}

// driveRowsErr is driveRows with the stderr the drive wrote.
func driveRowsErr(t *testing.T) (rows []DriveItem, stderr string, err error) {
	t.Helper()
	stderr = captureErr(t, func() { rows, err = driveRows(t) })
	return rows, stderr, err
}

// finalRow is the last row the drive reported for item id: a re-check reports
// the item again once it is picked up.
func finalRow(t *testing.T, rows []DriveItem, id string) DriveItem {
	t.Helper()
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].ID == id {
			return rows[i]
		}
	}
	t.Fatalf("no row for item %s among %+v", id, rows)
	return DriveItem{}
}

// A code-class red check sends item 1 back as gate_refused, and the
// operator watching stderr reads that before item 2's banner rather than in
// the summary after the queue drains.
func TestAGateRefusalIsSaidBeforeTheNextItemStarts(t *testing.T) {
	checksForge(t, "pending")
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")

	rows, stderr, err := driveRowsErr(t)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	line, at := parkLine(stderr, "1", parkGateRefused)
	if at < 0 {
		t.Fatalf("no park line for item 1 on stderr:\n%s", stderr)
	}
	if want := "  ! 1 parked gate_refused at " + headOf(t, rows) + ": vuln failed: no infrastructure line in its log, and main does not fail it; sent back"; line != want {
		t.Errorf("park line %q, want %q", line, want)
	}
	if banner := strings.Index(stderr, "==> 2 · implement"); banner < 0 || banner < at {
		t.Errorf("item 2's banner at %d, park line at %d; want the park said first:\n%s", banner, at, stderr)
	}
	if strings.Count(stderr, "  ! 1 parked ") != 1 {
		t.Errorf("item 1's park said %d times, want once:\n%s", strings.Count(stderr, "  ! 1 parked "), stderr)
	}
}

// The two gate parks that never pass through driveItem's park closure say
// themselves too: a gate that will not run, and one blaming the forge.
func TestAGateParkThatBypassesParkIsStillSaid(t *testing.T) {
	t.Run("gate_unrunnable", func(t *testing.T) {
		checksForge(t, "none")
		cfg := read(t, config.FileName)
		gate, err := exec.LookPath("merge-gate")
		if err != nil {
			t.Fatal(err)
		}
		write(t, config.FileName, strings.Replace(cfg, gate, gate+"-missing", 1)+"\n[forge]\nchecks_wait = \"20ms\"\n")

		rows, stderr, _ := driveRowsErr(t)
		if got := finalRow(t, rows, "1"); got.Park != "gate_unrunnable" {
			t.Fatalf("item 1 is %s/%s (%s), want gate_unrunnable", got.Outcome, got.Park, got.Reason)
		}
		line, at := parkLine(stderr, "1", "gate_unrunnable")
		if at < 0 || !strings.Contains(line, " at "+headOf(t, rows)+": ") || !strings.Contains(line, "not on PATH") {
			t.Fatalf("park line %q, want one naming the head and the missing merge gate:\n%s", line, stderr)
		}
		if banner := strings.Index(stderr, "==> 2 · implement"); banner < at {
			t.Errorf("item 2's banner at %d, park line at %d; want the park said first", banner, at)
		}
	})
	t.Run("forge_down", func(t *testing.T) {
		checksForge(t, "none")
		gate, err := exec.LookPath("merge-gate")
		if err != nil {
			t.Fatal(err)
		}
		stub(t, gate, "#!/bin/sh\necho 'merge-gate: the forge is down'\nexit 2\n")
		write(t, config.FileName, read(t, config.FileName)+"\n[forge]\nchecks_wait = \"20ms\"\n")

		rows, stderr, _ := driveRowsErr(t)
		line, at := parkLine(stderr, "1", "forge_down")
		if want := "  ! 1 parked forge_down at " + headOf(t, rows) + ": no workflow ran on this head"; at < 0 || !strings.HasPrefix(line, want) {
			t.Fatalf("park line %q, want one opening %q:\n%s", line, want, stderr)
		}
		if stop := strings.Index(stderr, "stopping the drive"); stop < at {
			t.Errorf("the drive's stop at %d, park line at %d; want the park said first", stop, at)
		}
	})
}

// recheckProject is checksForge with rerun_infra_failure at operator, so a red
// check on the forge's infrastructure parks rather than reruns.
func recheckProject(t *testing.T, scenario string) string {
	t.Helper()
	ghLog := checksForge(t, scenario)
	write(t, config.FileName, read(t, config.FileName)+"\n[supervise]\nrerun_infra_failure = \"operator\"\n")
	return ghLog
}

// Every job fails with no step when the Actions allowance runs out, and goes
// green once somebody reruns it. The re-check at the
// end of the drive finds it green and merges it, and the ledger reads the park
// and then the pickup.
func TestAnInfraParkThatGoesGreenIsPickedUpBeforeTheDriveEnds(t *testing.T) {
	recheckProject(t, "recovers")

	rows, stderr, err := driveRowsErr(t)
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	if _, at := parkLine(stderr, "1", parkInfraFailure); at < 0 {
		t.Fatalf("no infra_failure park line for item 1:\n%s", stderr)
	}
	if got := finalRow(t, rows, "1"); got.Outcome != outcomeMergedByGate && got.Outcome != outcomeReviewed {
		t.Fatalf("item 1 ended %s/%s (%s), want merged or reviewed after the re-check", got.Outcome, got.Park, got.Reason)
	}
	if strings.Contains(stderr, "still red at the end of the drive") {
		t.Errorf("a head that went green was reported still red:\n%s", stderr)
	}
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	park, green := -1, -1
	for i, r := range l.Rows {
		switch {
		case r.ID == "1" && r.Park == parkInfraFailure:
			park = i
		case r.ID == "1" && r.Class == ledger.ClassGreen:
			green = i
		}
	}
	if park < 0 || green < park {
		t.Errorf("park row at %d, green row at %d; want the pickup recorded after the park", park, green)
	}
	if _, parked := l.Parked("1"); parked {
		t.Errorf("the ledger still reads item 1 as parked after its pickup")
	}
}

// The same park on a head that stays red stays parked, and the drive says so
// once at its end.
func TestAnInfraParkThatStaysRedIsLeftParked(t *testing.T) {
	recheckProject(t, "nosteps")

	rows, stderr, err := driveRowsErr(t)
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	got := finalRow(t, rows, "1")
	if got.Outcome != outcomeParked || got.Park != parkInfraFailure {
		t.Fatalf("item 1 ended %s/%s (%s), want parked infra_failure", got.Outcome, got.Park, got.Reason)
	}
	want := "  · 1: checks on " + headOf(t, rows) + " still red at the end of the drive; left parked"
	if strings.Count(stderr, want) != 1 {
		t.Errorf("want %q once on stderr:\n%s", want, stderr)
	}
}

// A drive that stops on a red trunk re-checks nothing: item 1 parked on its
// infrastructure, item 2 found the trunk red, and item 1's head is read once.
func TestADriveStoppedOnARedTrunkRechecksNothing(t *testing.T) {
	ghLog := recheckProject(t, "infratrunk")
	write(t, filepath.Join("work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Do it too. REVIEW-PASSES-2\n")
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	stub(t, rev, `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false}'
case "$*" in *REVIEW-PASSES-2*) id=2 ;; *REVIEW-PASSES*) id=1 ;; *) exit 0 ;; esac
printf '{"id":"%s","level":"reviewed","code_by":"agent","independent":true,"date":"2026-09-20"}\n' "$id" >> "$DROVER_ROOT/ledger.jsonl"
echo "$id" > review-fix.txt
git add review-fix.txt
git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m 'review: a typo'
`)

	rows, stderr, err := driveRowsErr(t)
	if Code(err) == 0 {
		t.Fatalf("drive exited 0 over a red trunk, want its stop:\n%s", stderr)
	}
	if got := finalRow(t, rows, "2"); got.Park != parkTrunkRed {
		t.Fatalf("item 2 is %s/%s (%s), want trunk_red", got.Outcome, got.Park, got.Reason)
	}
	if _, at := parkLine(stderr, "2", parkTrunkRed); at < 0 {
		t.Errorf("no trunk_red park line for item 2:\n%s", stderr)
	}
	got := finalRow(t, rows, "1")
	if got.Park != parkInfraFailure {
		t.Fatalf("item 1 is %s/%s (%s), want infra_failure", got.Outcome, got.Park, got.Reason)
	}
	head := git(t, ".", "rev-parse", got.Branch)
	reads := 0
	for _, c := range ghCalls(t, ghLog) {
		if strings.Contains(c, "/commits/"+head+"/check-runs") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("item 1's head %s read %d times, want once: the stop leaves it unread", shortSHA(head), reads)
	}
	if strings.Contains(stderr, "still red at the end of the drive") {
		t.Errorf("a stopped drive re-checked:\n%s", stderr)
	}
}

// F14's two real red Gate logs: a lint finding on ae30c3c and a govulncheck
// finding on d9108ba. Each is read down to the lines that say what failed,
// without the timestamp, the ##[error] annotation or the colors.
func TestFailureLinesReadF14sLogs(t *testing.T) {
	cases := []struct {
		log  string
		want []string
	}{
		{"testdata/f14-revive-ae30c3c.log", []string{
			"internal/cmd/notify.go:28:16: var-declaration: should omit type http.RoundTripper from declaration of var ntfyTransport; it will be inferred from the right-hand side (revive)",
		}},
		{"testdata/f14-govulncheck-d9108ba.log", []string{
			"Vulnerability #1: GO-2026-6809",
			"Found in: crypto/x509@go1.26.9",
			"Fixed in: crypto/x509@go1.26.10",
		}},
	}
	s := &session{cfg: &config.Config{}}
	for _, c := range cases {
		b, err := os.ReadFile(c.log)
		if err != nil {
			t.Fatal(err)
		}
		if got := failureLines(string(b), s.failurePatterns()); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s: failure lines\n%q\nwant\n%q", c.log, got, c.want)
		}
	}
}

// A log no pattern matches is read back from the make target that gave up,
// and a [forge] failure_patterns entry is matched beside the built-in ones.
func TestFailureLinesFallBackAndTakeConfiguredPatterns(t *testing.T) {
	log := strings.Join([]string{
		"2026-10-17T01:02:03.0000000Z ##[group]Run make check",
		"2026-10-17T01:02:03.0000000Z one",
		"2026-10-17T01:02:03.0000000Z ",
		"2026-10-17T01:02:03.0000000Z two",
		"2026-10-17T01:02:03.0000000Z three",
		"2026-10-17T01:02:03.0000000Z four",
		"2026-10-17T01:02:03.0000000Z five",
		"2026-10-17T01:02:03.0000000Z six",
		"2026-10-17T01:02:03.0000000Z make: *** [Makefile:9: check] Error 1",
		"2026-10-17T01:02:03.0000000Z after",
	}, "\n")
	s := &session{cfg: &config.Config{}}
	if got, want := failureLines(log, s.failurePatterns()), []string{"two", "three", "four", "five", "six"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("fallback lines %q, want %q", got, want)
	}
	s.cfg.Forge.FailurePatterns = []string{`^f\w+$`}
	if got := failureLines(log, s.failurePatterns()); strings.Join(got, ",") != "four,five" {
		t.Errorf("configured pattern read %q, want four and five", got)
	}
	if got := failureLines("nothing failed here\n", s.failurePatterns()); got != nil {
		t.Errorf("a log with no failure line and no make error read %q, want nil", got)
	}
}

// F14's govulncheck finding is in the standard library, and a rerun lands on
// the runner's cached toolchain again, so it is code: sent back without a rerun,
// with the release that fixes it and the go.mod line that installs it.
func TestAStandardLibraryVulnerabilityIsCode(t *testing.T) {
	logPath, err := filepath.Abs("testdata/f14-govulncheck-d9108ba.log")
	if err != nil {
		t.Fatal(err)
	}
	ghLog := checksForge(t, "joblog")
	t.Setenv("DROVER_TEST_CHECKS_LOG", logPath)
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")

	driveAll(t)
	if n := reruns(t, ghLog); n != 0 {
		t.Errorf("gh run rerun ran %d times, want none", n)
	}
	if cs := checkRows(t); len(cs) == 0 || cs[0].Class != ledger.ClassCode {
		t.Errorf("classification rows %+v, want code", cs)
	}
	body, err := os.ReadFile("work/01-first.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Found in: crypto/x509@go1.26.9",
		"Fixed in: crypto/x509@go1.26.10",
		"The fix is a `toolchain go1.26.10` line in go.mod, or one naming a newer release.",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("sent-back body lacks %q:\n%s", want, body)
		}
	}
}

// Only a Go release in a Fixed in line asks for a toolchain; a module's fix is
// a require line, and the newest of several releases is the one to name.
func TestToolchainFix(t *testing.T) {
	cases := []struct {
		failure []string
		want    string
	}{
		{[]string{"Fixed in: golang.org/x/net@v0.38.0"}, ""},
		{[]string{"Fixed in: crypto/x509@go1.26.10", "Fixed in: net/http@go1.26.9"}, "go1.26.10"},
		{[]string{"Fixed in: net/http@go1.26.9", "Fixed in: crypto/x509@go1.27.2"}, "go1.27.2"},
		{nil, ""},
	}
	for _, c := range cases {
		got := toolchainFix(c.failure)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, "`toolchain "+c.want+"`") {
			t.Errorf("toolchainFix(%q) = %q, want %q", c.failure, got, c.want)
		}
	}
}

// D10's F14 park: a code failure the merge gate blames on the diff names the
// check and its first failure line in the reason, carries the class as its
// detail and the lines as its evidence, on the park row, the parked event and
// in the body sent back with the run's URL.
func TestACodeFailureParkSaysWhatFailed(t *testing.T) {
	logPath, err := filepath.Abs("testdata/f14-revive-ae30c3c.log")
	if err != nil {
		t.Fatal(err)
	}
	checksForge(t, "joblog")
	t.Setenv("DROVER_TEST_CHECKS_LOG", logPath)
	t.Setenv("DROVER_TEST_GATE_REFUSE_FROM", "0")
	const line = "internal/cmd/notify.go:28:16: var-declaration: should omit type http.RoundTripper from declaration of var ntfyTransport; it will be inferred from the right-hand side (revive)"

	evs, _, _ := driveEventsOf(t)
	var parked *DriveEvent
	for i, e := range evs {
		if e.Event == evParked && e.ID == "1" {
			parked = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkGateRefused || parked.Detail != ledger.ClassCode ||
		!strings.HasPrefix(parked.Reason, "build failed: "+line) || parked.Evidence != line {
		t.Fatalf("item 1 parked %+v, want gate_refused, detail code, the lint line in reason and evidence", parked)
	}
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var row *ledger.Row
	for i, r := range l.Rows {
		if r.ID == "1" && r.Park == parkGateRefused {
			row = &l.Rows[i]
		}
	}
	if row == nil || row.Detail != ledger.ClassCode || row.Evidence != line {
		t.Errorf("park row %+v, want detail code and the lint line as evidence", row)
	}
	body, err := os.ReadFile("work/01-first.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The merge gate refused this branch.", "build failed (https://github.com/o/r/actions/runs/500/job/30). Its log:", line} {
		if !strings.Contains(string(body), want) {
			t.Errorf("sent-back body lacks %q:\n%s", want, body)
		}
	}
}
