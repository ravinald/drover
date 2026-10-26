package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// launchStub runs script as a headless runner through runner.Run, which is the
// launch drive makes, and returns what it left.
func launchStub(t *testing.T, script string) (runner.Outcome, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	stub(t, bin, "#!/bin/sh\n"+script)
	return runner.Run(context.Background(), runner.Where{Dir: dir}, runner.Spec{Headless: []string{bin}},
		runner.Headless, "prompt", filepath.Join(dir, "logs", "run.jsonl"), &strings.Builder{})
}

// Each detail from the launch that produces it: a stub runner doing the thing
// a real session's log shows it doing, read back the way drive reads it.
func TestEachSessionDetailIsReadFromItsTrigger(t *testing.T) {
	const runnerExit = "exit \"$DROVER_TEST_STUB_EXIT\"\n"
	runnerExits := map[string]string{"exit": "3", "exit 137": "137"}
	for _, c := range []struct {
		name, script, want, evidence string
	}{
		{"exit", runnerExit, detailRunnerExit, "exited 3"},
		{"sigkill", "kill -9 $$\n", detailOOM, "signal 9"},
		{"exit 137", runnerExit, detailOOM, "exited 137"},
		{"denied", `echo '{"type":"result","subtype":"success","is_error":false,"result":"done","permission_denials":[{"tool_name":"Bash"}]}'` + "\n",
			detailToolDenied, "Bash"},
		{"background", "sleep 30 >/dev/null 2>&1 &\n" +
			`echo '{"type":"result","subtype":"success","is_error":false,"result":"tests started"}'` + "\n",
			detailLeftRunning, "sleep 30"},
		{"cut", `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"The stream was interrupted"}'` + "\nexit 1\n",
			detailStreamCut, "The stream was interrupted"},
		{"auth", `echo '{"type":"result","subtype":"success","is_error":true,"result":"OAuth token has expired. Please run /login"}'` + "\nexit 1\n",
			detailAuth, "OAuth token has expired"},
		{"unknown", "echo 'line one'\necho 'the last thing it said'\n", detailUnknown, "the last thing it said"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, ok := runnerExits[c.name]; ok {
				t.Setenv("DROVER_TEST_STUB_EXIT", code)
			}
			oc, err := launchStub(t, c.script)
			pd := sessionDetail(oc, err)
			if pd.Detail != c.want {
				t.Fatalf("detail = %q (%s), want %q", pd.Detail, pd.Evidence, c.want)
			}
			if !strings.Contains(pd.Evidence, c.evidence) {
				t.Errorf("evidence %q does not carry %q", pd.Evidence, c.evidence)
			}
			if pd.Log == "" {
				t.Error("the detail names no log")
			}
		})
	}
}

// A session that left its gate running is not a clean run, whatever its
// terminal event says, and the process does not outlive the result.
func TestASessionThatLeftAProcessRunningIsNotClean(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "still-here")
	// Past runner's two-second grace, which is for helpers a session signalled
	// on its way out.
	t.Setenv("DROVER_TEST_MARKER", marker)
	oc, _ := launchStub(t, "(sleep 4; touch \"$DROVER_TEST_MARKER\") >/dev/null 2>&1 &\n"+
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"tests started"}'`+"\n")
	if len(oc.LeftRunning) == 0 {
		t.Fatal("the launch reports nothing left running")
	}
	p := pass{res: oc.Result, logged: oc.Logged, events: oc.Events, known: oc.Known, leftRunning: oc.LeftRunning}
	if got := runResult(p); got != passDied {
		t.Errorf("runResult = %q, want %q: the session's report is about work that had not finished", got, passDied)
	}
	if s := passSentence(passDied, ledger.StageImplement, p, "the implement run", "impl", oc.LogPath); !strings.Contains(s, "still running") {
		t.Errorf("the park sentence does not say what was left running: %s", s)
	}
	time.Sleep(3 * time.Second)
	if fileExists(marker) {
		t.Error("the process the session left ran on after drover read the run")
	}
}

// pushRepo is a branch with a commit and a bare origin to push it to.
func pushRepo(t *testing.T) string {
	t.Helper()
	identify(t)
	dir, remote := t.TempDir(), filepath.Join(t.TempDir(), "o.git")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "remote", "add", "origin", remote)
	return dir
}

// git names no hook when one refuses, so the hook comes from the tree and its
// first line from the output. A gosec pre-push hook refusing the push must not
// show in status as ran.
func TestAPushRefusedByItsHookIsPushHook(t *testing.T) {
	dir := pushRepo(t)
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")
	stub(t, hook, "#!/bin/sh\necho 'gosec: G304 potential file inclusion via variable' >&2\necho 'second line' >&2\nexit 1\n")
	s := &session{cfg: &config.Config{Root: dir}}
	err := s.pushBranch("main")
	if err == nil {
		t.Fatal("the hook refused and the push reports success")
	}
	pd := pushDetail(dir, err)
	if pd.Detail != detailPushHook {
		t.Fatalf("detail = %q (%s), want %q", pd.Detail, pd.Evidence, detailPushHook)
	}
	if !strings.Contains(pd.Evidence, "pre-push") || !strings.Contains(pd.Evidence, "gosec: G304") ||
		strings.Contains(pd.Evidence, "second line") {
		t.Errorf("evidence %q, want the hook and its first line alone", pd.Evidence)
	}

	// The same output with no hook in the tree is not the hook's.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if got := pushDetail(dir, err).Detail; got != detailUnknown {
		t.Errorf("with no hook, detail = %q, want %q", got, detailUnknown)
	}
	// A remote rejection is the forge's answer even with a hook declared.
	stub(t, hook, "#!/bin/sh\nexit 0\n")
	rejected := errors.New(" ! [remote rejected] main -> main (protected branch hook declined)\nerror: failed to push some refs")
	if got := pushDetail(dir, rejected).Detail; got != detailUnknown {
		t.Errorf("a remote rejection read as %q, want %q", got, detailUnknown)
	}
}

func TestAPushThatLostItsConnectionIsPushNetwork(t *testing.T) {
	dir := pushRepo(t)
	// git's own output, capitalized as ssh writes it.
	out := "Connection to github.com closed by remote host.\nfatal: Could not read from remote repository."
	err := errors.New(out)
	pd := pushDetail(dir, err)
	if pd.Detail != detailPushNetwork || !strings.Contains(pd.Evidence, "closed by remote host") {
		t.Errorf("detail = %q (%s), want %q naming the line", pd.Detail, pd.Evidence, detailPushNetwork)
	}
}

// A hook refusal and an expired login stop for the operator under their own
// actions, and the login stop names the command that signs the runner in.
func TestTheDetailPicksTheActionThatOwnsThePark(t *testing.T) {
	for _, c := range []struct {
		d    DriveItem
		want string
	}{
		{DriveItem{Park: "push_failed", Detail: detailPushHook}, "push_refused_by_hook"},
		{DriveItem{Park: "push_failed", Detail: detailPushNetwork}, ""},
		{DriveItem{Park: "runner_fault", Detail: detailAuth}, "auth_expired"},
		{DriveItem{Park: "runner_fault", Detail: detailRunnerExit}, ""},
		{DriveItem{Park: passDied, Detail: detailStreamCut}, "rerun_lost_review"},
	} {
		if got := parkAction(c.d); got != c.want {
			t.Errorf("%s (%s) belongs to %q, want %q", c.d.Park, c.d.Detail, got, c.want)
		}
	}

	var out strings.Builder
	es := &eventSink{w: &out, cfg: &config.Config{}}
	es.itemEvents(DriveItem{ID: "7", Outcome: outcomeParked, Park: "runner_fault", Detail: detailAuth,
		Reason: "not logged in", login: runner.Login(runner.Spec{Headless: []string{"claude"}})})
	var needs *DriveEvent
	for _, line := range strings.Split(out.String(), "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Event == evNeedsOperator {
			needs = &e
		}
	}
	if needs == nil || needs.Action != "auth_expired" || !strings.Contains(needs.Run, "claude auth login") {
		t.Fatalf("needs_operator = %+v, want auth_expired naming `claude auth login`:\n%s", needs, out.String())
	}
}

// The review fails the way agy's did, "The stream was interrupted", and the
// drive reruns it once on the same head. lossesBefore is how many of its
// launches are cut before it passes; past one, the second loss parks.
func driveCutReview(t *testing.T, lossesBefore int) DriveItem {
	t.Helper()
	mergeProject(t)
	rev, err := exec.LookPath("rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rev, rev+".orig"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_REV_COUNT", filepath.Join(t.TempDir(), "count"))
	t.Setenv("DROVER_TEST_REV_LOSSES", strconv.Itoa(lossesBefore))
	stub(t, rev, `#!/bin/sh
n=$(cat "$DROVER_TEST_REV_COUNT" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$DROVER_TEST_REV_COUNT"
if [ $n -le "$DROVER_TEST_REV_LOSSES" ]; then
  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"The stream was interrupted"}'
  exit 1
fi
exec "$0.orig" "$@"
`)
	rows := driveAll(t, "1")
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func TestACutReviewIsRerunOnceOnTheSameHead(t *testing.T) {
	got := driveCutReview(t, 1)
	if got.Reviews != 2 {
		t.Errorf("reviews = %d, want 2: the cut pass and its one rerun", got.Reviews)
	}
	if got.Rounds != 0 {
		t.Errorf("rounds = %d, want 0: the rerun is review only, on the head the cut pass was given", got.Rounds)
	}
	if got.Outcome == outcomeParked {
		t.Errorf("the rerun passed and the item parked anyway: %s (%s) %s", got.Park, got.Detail, got.Reason)
	}
	l, err := ledger.Load("ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if n := l.Counts("1").Of(ledger.StageImplement); n != 1 {
		t.Errorf("%d implement launches, want 1", n)
	}
}

func TestASecondCutParksWithItsDetail(t *testing.T) {
	got := driveCutReview(t, 2)
	if got.Reviews != 2 || got.Outcome != outcomeParked || got.Park != passDied || got.Detail != detailStreamCut {
		t.Fatalf("item 1: reviews %d, %s %s (%s), want 2 reviews and parked %s (%s)",
			got.Reviews, got.Outcome, got.Park, got.Detail, passDied, detailStreamCut)
	}
	if !strings.Contains(got.Evidence, "The stream was interrupted") || got.Log == "" {
		t.Errorf("evidence %q, log %q: want the runner's line and the log", got.Evidence, got.Log)
	}

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := openIn(t, dir)
	pk, ok := s.ledger.Parked("1")
	if !ok || pk.Park != passDied || pk.Detail != detailStreamCut || pk.Log != got.Log {
		t.Errorf("ledger park row = %+v (found %v), want %s (%s) at %s", pk, ok, passDied, detailStreamCut, got.Log)
	}
	for _, r := range s.rows() {
		if r.ID != "1" {
			continue
		}
		if r.State != stateRan || r.Park != passDied || r.Detail != detailStreamCut {
			t.Errorf("status row is %s, park %q detail %q", r.State, r.Park, r.Detail)
		}
		if note := r.note(); !strings.HasPrefix(note, "parked: died (stream_cut)") || strings.Contains(note, "implemented") {
			t.Errorf("status note %q, want parked: died (stream_cut) instead of ran", note)
		}
	}

	text := capture(t, func() {
		if err := execute(t, "show", "1"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(text, "parked     died (stream_cut)") || !strings.Contains(text, "The stream was interrupted") ||
		!strings.Contains(text, "park log") {
		t.Errorf("show does not print the park, its evidence and its log:\n%s", text)
	}
	out := capture(t, func() {
		if err := execute(t, "show", "1", "--json"); err != nil {
			t.Fatal(err)
		}
	})
	var sh Show
	if err := json.Unmarshal([]byte(out), &sh); err != nil {
		t.Fatal(err)
	}
	if sh.Parked == nil || sh.Parked.Detail != detailStreamCut || sh.Parked.Log == "" {
		t.Errorf("show --json parked = %+v, want stream_cut with its log", sh.Parked)
	}

	// Launching the item again is acting on the park, and status stops naming it.
	if _, err := s.launched(mustItem(t, s, "1"), ledger.StageImplement, "impl"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ledger.Parked("1"); ok {
		t.Error("a launch after the park leaves it standing")
	}
}

func mustItem(t *testing.T, s *session, id string) backlog.Item {
	t.Helper()
	it, err := s.item(id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// --review-only is the rerun by hand: no implement run, the branch as it
// stands reviewed, then the gate, the pull request and the merge a clean pass
// gets.
func TestReviewOnlyReachesTheMerge(t *testing.T) {
	t.Cleanup(func() { driveReviewOnly, reviewOnlyID = false, "" })
	ghLog := mergeProject(t)
	dir, _ := forge(t, ghLog)
	t.Setenv("DROVER_TEST_NO_AUTO", "1")
	poll := checksPoll
	checksPoll = time.Millisecond
	t.Cleanup(func() { checksPoll = poll })

	s := openIn(t, dir)
	branch, err := s.branchFor(mustItem(t, s, "1"))
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "switch", "-q", "-c", branch)
	write(t, filepath.Join(dir, "by-hand.txt"), "work a session did before its review was lost\n")
	git(t, dir, "add", "by-hand.txt")
	git(t, dir, "commit", "-q", "-m", "the work")
	git(t, dir, "switch", "-q", "main")

	rows := driveAll(t, "--review-only", "1")
	if len(rows) != 1 {
		t.Fatalf("drive reported %d rows, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Outcome != outcomeMergedByGate || !got.Merged || !got.Finished {
		t.Fatalf("item 1 is %q merged=%v finished=%v (%s %s), want %q, merged and finished",
			got.Outcome, got.Merged, got.Finished, got.Park, got.Reason, outcomeMergedByGate)
	}
	if got.Reviews != 1 || got.Rounds != 0 {
		t.Errorf("reviews %d rounds %d, want one review and no implement round", got.Reviews, got.Rounds)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := l.Counts("1").Of(ledger.StageImplement); n != 0 {
		t.Errorf("%d implement launches under --review-only, want none", n)
	}

	if err := execute(t, "drive", "--review-only", "1", "2"); Code(err) != 3 {
		t.Errorf("--review-only with two ids: %v (exit %d), want exit 3", err, Code(err))
	}
}

// An expired login found at the start costs nothing: the drive refuses before
// any session, and names the command that signs the runner in.
func TestAnAuthFailureAtPreflightRefusesBeforeAnyLaunch(t *testing.T) {
	bin := t.TempDir()
	launched := filepath.Join(bin, "launched")
	claude := filepath.Join(bin, "claude")
	t.Setenv("DROVER_TEST_LAUNCHED", launched)
	stub(t, claude, "#!/bin/sh\n"+
		"if [ \"$1 $2\" = 'auth status' ]; then echo 'Not logged in' >&2; exit 1; fi\n"+
		"touch \"$DROVER_TEST_LAUNCHED\"\n")
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[runners.claude]\nheadless = [\""+claude+"\"]\n")

	var err error
	stderr := captureErr(t, func() { err = execute(t, "drive", "--runner", "claude") })
	if Code(err) != 3 {
		t.Fatalf("drive with a signed-out runner: %v (exit %d), want exit 3\n%s", err, Code(err), stderr)
	}
	if !strings.Contains(err.Error(), "claude auth login") || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("the refusal does not name the login command and what the check said: %v", err)
	}
	if fileExists(launched) {
		t.Error("a session launched before the refusal")
	}
	if b, rerr := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl")); rerr == nil && strings.Contains(string(b), "stage") {
		t.Errorf("the refused drive wrote a stage row:\n%s", b)
	}
}

// A runner path naming no file is a typo, not a signed-out runner. exec only
// returns ErrNotFound from a PATH lookup, so a path that fails to start has to
// be told apart some other way, or the operator is sent to log in.
func TestAMissingRunnerPathIsNotRefusedAsSignedOut(t *testing.T) {
	claude := filepath.Join(t.TempDir(), "claude")
	driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[runners.claude]\nheadless = [\""+claude+"\"]\n")

	var err error
	var stderr string
	stdout := capture(t, func() {
		stderr = captureErr(t, func() { err = execute(t, "drive", "--runner", "claude") })
	})
	said := stdout + stderr
	if err != nil {
		said += err.Error()
	}
	if strings.Contains(said, "signed in") {
		t.Errorf("a missing runner path was refused as signed out:\n%s", said)
	}
	if !strings.Contains(said, "runner binary") || !strings.Contains(said, claude) {
		t.Errorf("the drive does not report the launch's missing-binary error for %s:\n%s", claude, said)
	}
}

// The reviewer is first launched after an implementation session has run, so
// a reviewer path naming no file is refused before the drive pays for one.
func TestAMissingReviewerPathRefusesBeforeAnyLaunch(t *testing.T) {
	t.Cleanup(func() { driveRunner, driveReviewer, driveReviewOnly = "", "", false })
	bin := t.TempDir()
	launched := filepath.Join(bin, "launched")
	claude := filepath.Join(bin, "claude")
	codex := filepath.Join(t.TempDir(), "codex")
	t.Setenv("DROVER_TEST_LAUNCHED", launched)
	stub(t, claude, "#!/bin/sh\n"+
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n"+
		"touch \"$DROVER_TEST_LAUNCHED\"\n")
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[runners.claude]\nheadless = [\""+claude+"\"]\n"+
		"\n[runners.codex]\nheadless = [\""+codex+"\"]\n")

	var err error
	stderr := captureErr(t, func() { err = execute(t, "drive", "--runner", "claude", "--review-runner", "codex") })
	if Code(err) != 3 {
		t.Fatalf("drive with a missing reviewer binary: %v (exit %d), want exit 3\n%s", err, Code(err), stderr)
	}
	if strings.Contains(err.Error(), "signed in") {
		t.Errorf("a missing reviewer path was refused as signed out: %v", err)
	}
	if !strings.Contains(err.Error(), "runner binary") || !strings.Contains(err.Error(), codex) {
		t.Errorf("the refusal does not name the missing binary %s: %v", codex, err)
	}
	if fileExists(launched) {
		t.Error("an implementation session launched before the refusal")
	}

	// --review-only launches no implementer, so a missing one is not refused.
	driveReviewOnly, driveRunner = true, "codex"
	if err := s.preflightBinaries("claude"); err != nil {
		t.Errorf("--review-only refused over an implementer it never launches: %v", err)
	}
}

// The implement prompt is where a session learns not to background its verify command.
func TestThePromptForbidsBackgroundingTheVerifyCommand(t *testing.T) {
	s := driveProject(t)
	body, err := s.renderPrompt(mustItem(t, s, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Never background the verify command") {
		t.Errorf("the prompt does not tell the session to keep the verify command in the foreground:\n%s", body)
	}
}

// Lost is a pass that reached no result. A pass that sent work back and then exited
// non-zero reached one, and a denial meets the same denial on a rerun.
func TestOnlyAPassThatReachedNoResultIsLost(t *testing.T) {
	for _, c := range []struct {
		v, detail string
		want      bool
	}{
		{passDied, detailUnknown, true},
		{passDied, detailLeftRunning, true},
		{passSilent, detailStreamCut, true},
		{passSilent, detailOOM, true},
		{passSilent, detailRunnerExit, true},
		{passSilent, detailToolDenied, false},
		{passSilent, detailUnknown, false},
		{passChangesRequested, detailRunnerExit, false},
		{passPassed, detailOOM, false},
		{passFaulted, detailAuth, false},
	} {
		if got := lostReview(c.v, c.detail); got != c.want {
			t.Errorf("lostReview(%s, %s) = %v, want %v", c.v, c.detail, got, c.want)
		}
	}
}
