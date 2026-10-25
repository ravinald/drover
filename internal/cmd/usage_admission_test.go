package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runlog"
)

func usageConfig(t *testing.T, toml string) *config.Config {
	t.Helper()
	return fixture(t, toml).cfg
}

func reading(status, kind string, week, five float64, reset time.Time, overage bool, reason string) *runlog.RateLimit {
	return &runlog.RateLimit{Status: status, RateLimitType: kind, ResetsAt: reset.Unix(),
		SevenDay:       &runlog.LimitWindow{Utilization: week, ResetsAt: reset.Unix()},
		FiveHour:       &runlog.LimitWindow{Utilization: five, ResetsAt: reset.Add(-time.Hour).Unix()},
		IsUsingOverage: overage, OverageDisabledReason: reason}
}

func TestJudgeUsage(t *testing.T) {
	now := time.Now()
	reset := now.Add(30 * time.Hour)
	levels := "\n[usage.weekly]\nnotify_remaining = 20\npause_remaining = 5\n"
	for _, c := range []struct {
		name, toml string
		r          *runlog.RateLimit
		spent      float64
		warn       []string
		hold       string
	}{
		{"plenty left", levels, reading("allowed", "five_hour", 0.5, 0.1, reset, false, ""), 0, nil, ""},
		{"at the notify level", levels, reading("allowed_warning", "seven_day", 0.8, 0.1, reset, false, ""), 0, []string{stepWeekly}, ""},
		{"at the pause level", levels, reading("allowed_warning", "seven_day", 0.95, 0.1, reset, false, ""), 0, []string{stepWeekly}, stepWeekly},
		{"no levels, nothing", "", reading("allowed_warning", "seven_day", 0.95, 0.1, reset, false, ""), 0, nil, ""},
		{"five-hour pause", "\n[usage.five_hour]\npause_remaining = 10\n", reading("allowed", "five_hour", 0.2, 0.95, reset, false, ""), 0, nil, stepFiveHour},
		{"overage under the default pause", levels, reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 0, []string{stepWeekly, stepOverage}, stepWeekly},
		{"overage, no levels, still the fail-safe", "", reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 0, nil, stepOverage},
		{"overage under continue, under the cap", "\n[usage.overage]\nmode = \"continue\"\nmax_usd = 50.0\nnotify_usd = 30.0\n",
			reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 10, nil, ""},
		{"overage under continue, past the notice", "\n[usage.overage]\nmode = \"continue\"\nmax_usd = 50.0\nnotify_usd = 30.0\n",
			reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 31, []string{stepOverageUSD}, ""},
		{"overage under continue, at the cap", "\n[usage.overage]\nmode = \"continue\"\nmax_usd = 50.0\n",
			reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 50, nil, stepOverageUSD},
		{"out of credits holds with no level set", "", reading("rejected", "seven_day", 1, 0, reset, false, runlog.OutOfCredits), 0, nil, stepWeekly},
		{"a refusal past its reset is the past", levels, reading("rejected", "seven_day", 1, 0, now.Add(-time.Minute), false, runlog.OutOfCredits), 0, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := judgeUsage(usageConfig(t, c.toml), accountReading{Reading: c.r, OverageUSD: c.spent}, now)
			var got []string
			for _, w := range v.warnings {
				got = append(got, w.Window)
			}
			if !slices.Equal(got, c.warn) {
				t.Errorf("warnings %v, want %v", got, c.warn)
			}
			hold := ""
			if h := v.hold(nil); h != nil {
				hold = h.Window
				if !strings.Contains(h.Reason, "reset") && !strings.Contains(h.Reason, "max_usd") {
					t.Errorf("the hold does not say when it lifts: %q", h.Reason)
				}
			}
			if hold != c.hold {
				t.Errorf("hold %q, want %q", hold, c.hold)
			}
		})
	}
}

// A resume past one hold waives that window alone. Resuming past the weekly
// percent is spending its last points, not consenting to purchased credit or to
// going past max_usd, and those still hold in the same week, as does a service
// that refuses sessions.
func TestAWaivedWeeklyHoldLeavesTheOthersStanding(t *testing.T) {
	now := time.Now()
	reset := now.Add(30 * time.Hour)
	weekly := "\n[usage.weekly]\nnotify_remaining = 20\npause_remaining = 5\n"
	before := reading("allowed_warning", "seven_day", 0.96, 0.1, reset, false, "")
	for _, c := range []struct {
		name, toml string
		r          *runlog.RateLimit
		spent      float64
		want       string
		refused    bool
	}{
		{"overage under the default pause", weekly, reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 80, stepOverage, false},
		{"overage at max_usd under continue", "\n[usage.weekly]\npause_remaining = 5\n[usage.overage]\nmode = \"continue\"\nmax_usd = 50.0\n",
			reading("rejected", "seven_day", 1, 0.1, reset, true, ""), 80, stepOverageUSD, false},
		{"out of credits", weekly, reading("rejected", "seven_day", 1, 0.1, reset, false, runlog.OutOfCredits), 0, stepWeekly, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := usageConfig(t, c.toml)
			g := newUsageGate(time.Time{})
			first := judgeUsage(cfg, accountReading{Reading: before}, now).hold(g.waived)
			if first == nil || first.Window != stepWeekly {
				t.Fatalf("before: hold %+v, want the weekly level", first)
			}
			g.hold = first
			g.release(true)
			h := judgeUsage(cfg, accountReading{Reading: c.r, OverageUSD: c.spent}, now).hold(g.waived)
			if h == nil || h.Window != c.want || h.refused != c.refused {
				t.Errorf("after the weekly hold was waived: hold %+v, want %s (refused %v)", h, c.want, c.refused)
			}
		})
	}
}

// outOfCreditsLines is what Claude Code wrote for the review D8 lost, with the
// reset moved to RESET seconds from the stub's own clock.
const outOfCreditsLines = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":RESET,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":RESET,"status":"allowed"},"seven_day":{"utilization":1,"resetsAt":RESET,"status":"rejected","surpassedThreshold":1}}},"session_id":"0380041e"}
{"type":"result","subtype":"success","is_error":true,"num_turns":1,"terminal_reason":"api_error","total_cost_usd":0,"session_id":"0380041e","usage":{"input_tokens":0,"output_tokens":0},"result":"You've hit your weekly limit · resets Oct 25 at 8pm (America/Los_Angeles)"}`

// refusingStub prints the out_of_credits lines with their reset after seconds,
// on every launch or only the first, and commits and succeeds on the rest.
func refusingStub(seconds string, once bool) string {
	refuse := "  r=$(($(date +%s)+" + seconds + "))\n  sed \"s/RESET/$r/g\" <<'STREAM'\n" + outOfCreditsLines + "\nSTREAM\n  exit 0\n"
	if !once {
		return "#!/bin/sh\n" + refuse
	}
	return "#!/bin/sh\n" +
		"n=$(cat \"$DROVER_TEST_IMPL_COUNT\" 2>/dev/null || echo 0); echo $((n+1)) > \"$DROVER_TEST_IMPL_COUNT\"\n" +
		"if [ \"$n\" = 0 ]; then\n" + refuse + "fi\n" +
		"echo after > after.txt\ngit add after.txt\n" +
		"git -c user.name=r -c user.email=r@example.invalid -c commit.gpgsign=false commit -q -m 'after the reset'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":9,\"result\":\"done\"}'\n"
}

func usageEventsIn(out string) []DriveEvent {
	var evs []DriveEvent
	for _, line := range strings.Split(out, "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Event != "" {
			evs = append(evs, e)
		}
	}
	return evs
}

// usageEvent is the first event of the kind usage_admission emitted.
func usageEvent(evs []DriveEvent, event string) *DriveEvent {
	for i := range evs {
		if evs[i].Event == event && evs[i].Action == "usage_admission" {
			return &evs[i]
		}
	}
	return nil
}

// D8's death, replayed: a session refused out_of_credits pauses the drive with
// the item queued at the stage that was refused, rather than parking it
// runner_fault and ending the drive. The deadline arrives before the reset, so
// the drive exits 0 and the item reports held.
func TestARefusedSessionPausesTheDriveWithTheItemQueued(t *testing.T) {
	t.Cleanup(func() { driveFor, driveNoMerge, driveJSON, driveEvents = 0, false, false, false })
	poll := enqueuePoll
	enqueuePoll = 20 * time.Millisecond
	t.Cleanup(func() { enqueuePoll = poll })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := driveProject(t)
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "stub-impl"), refusingStub("172800", false))
	stub(t, filepath.Join(bin, "stub-rev"), revRefuses)
	t.Setenv("DROVER_TEST_REV_STDERR", "the review ran against a run that never happened")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	path, err := s.leasePath()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan lease.Lease, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			l, busy, err := lease.Read(path)
			if err == nil && busy && l.Paused && slices.Contains(l.Queue, "1") {
				seen <- l
				return
			}
		}
	}()
	var derr error
	out := capture(t, func() { derr = execute(t, "drive", "--json", "--events", "--for", "6s", "--no-merge", "1") })
	close(stop)
	if derr != nil {
		t.Fatalf("drive returned %v, want exit 0: the deadline ended a held drive\n%s", derr, out)
	}
	select {
	case <-seen:
	default:
		t.Errorf("the lease never read paused with 1 queued\n%s", out)
	}
	if strings.Contains(out, `"park":"runner_fault"`) {
		t.Errorf("the refusal parked the item:\n%s", out)
	}
	var item *DriveItem
	for _, line := range strings.Split(out, "\n") {
		var d DriveItem
		if json.Unmarshal([]byte(line), &d) == nil && d.ID == "1" && d.Outcome != "" {
			item = &d
		}
	}
	if item == nil || item.Outcome != outcomeHeld {
		t.Fatalf("item 1 = %+v, want outcome held\n%s", item, out)
	}
	evs := usageEventsIn(out)
	if e := usageEvent(evs, evNeedsOperator); e == nil || e.Run != "drover drive --resume" ||
		e.Window != stepWeekly || !strings.Contains(e.Reason, "out_of_credits") || e.ResetsAt == "" {
		t.Errorf("needs_operator = %+v, want the weekly window, out_of_credits, its reset and drover drive --resume", e)
	}
	if usageEvent(evs, evPaused) == nil {
		t.Error("no paused event from usage_admission")
	}
	if e := usageEvent(evs, evQueued); e == nil || e.Stage != ledger.StageImplement || e.Position != 1 {
		t.Errorf("queued = %+v, want 1 back at the head at implement", e)
	}
	b, _ := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	if strings.Contains(string(b), `"park":`) {
		t.Errorf("a park row was written for an item the account refused:\n%s", b)
	}
	if strings.Contains(string(b), `"stage":"review"`) {
		t.Error("a review launched behind a refused implement run")
	}
	if !strings.Contains(string(b), `"rate_limit":{"status":"rejected"`) {
		t.Errorf("the usage row carries no reading:\n%s", b)
	}
}

// A hold inside the drive's bound lifts at the reset on its own, says so, and
// the item goes on from the stage that was refused.
func TestAHeldDriveResumesAtTheReset(t *testing.T) {
	t.Cleanup(func() { driveFor, driveNoMerge, driveJSON, driveEvents = 0, false, false, false })
	poll, slack := enqueuePoll, quotaSlack
	enqueuePoll, quotaSlack = 20*time.Millisecond, 0
	t.Cleanup(func() { enqueuePoll, quotaSlack = poll, slack })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := driveProject(t)
	bin := t.TempDir()
	t.Setenv("DROVER_TEST_IMPL_COUNT", filepath.Join(bin, "n"))
	stub(t, filepath.Join(bin, "stub-impl"), refusingStub("2", true))
	t.Setenv("DROVER_TEST_RESULT_LINE", `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"looked"}`)
	stub(t, filepath.Join(bin, "stub-rev"), printsResultLine)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var derr error
	out := capture(t, func() { derr = execute(t, "drive", "--json", "--events", "--for", "1m", "--no-merge", "1") })
	if derr != nil {
		t.Fatalf("drive: %v\n%s", derr, out)
	}
	if e := usageEvent(usageEventsIn(out), evResumed); e == nil || !strings.Contains(e.Reason, "reset") {
		t.Errorf("resumed = %+v, want one from usage_admission naming the reset\n%s", e, out)
	}
	b, _ := os.ReadFile(filepath.Join(s.cfg.Root, "ledger.jsonl"))
	got := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"stage":"implement"`) && !strings.Contains(line, `"usage":`) {
			got++
		}
	}
	if got != 2 {
		t.Errorf("implement rows = %d, want 2: the refused launch, then the one after the reset", got)
	}
	if !strings.Contains(string(b), `"stage":"review"`) {
		t.Error("the item never reached its review after the resume")
	}
}

// One usage_warning per window per reset, across drives and projects: the
// ledger row the first drive writes is what the second reads.
func TestOneUsageWarningAcrossTwoDrives(t *testing.T) {
	t.Cleanup(func() { driveNoMerge = false })
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	sent := filepath.Join(t.TempDir(), "sent")
	extra := "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[usage.weekly]\nnotify_remaining = 20\n\n" +
		"[notify.destinations.sent]\ntype = \"command\"\nargv = [\"sh\", \"-c\", \"cat >> " + sent + "\"]\n" +
		"events = { usage_warning = \"default\" }\n"
	a := driveProjectWith(t, extra)
	b := driveProjectWith(t, extra)
	reg := projects.Path(config.GlobalPath())
	for _, s := range []*session{a, b} {
		if err := projects.Append(reg, projects.Row{Project: s.cfg.Project, Root: s.cfg.Root}); err != nil {
			t.Fatal(err)
		}
	}
	reset := time.Now().Add(40 * time.Hour)
	row := ledger.Row{Usage: &runlog.Usage{Input: 1}, SessionID: "earlier", Source: ledger.SourceHeadless,
		RateLimit: reading("allowed_warning", "seven_day", 0.85, 0.1, reset, false, "")}
	if err := ledger.Append(a.cfg.Abs(a.cfg.Paths.Ledger), row); err != nil {
		t.Fatal(err)
	}

	for _, s := range []*session{a, b, a} {
		t.Chdir(s.cfg.Root)
		_ = capture(t, func() { _ = execute(t, "drive", "--no-merge", "1") })
	}
	got, _ := os.ReadFile(sent)
	if n := strings.Count(string(got), `"event":"usage_warning"`); n != 1 {
		t.Errorf("notify ran on %d usage_warning events, want 1:\n%s", n, got)
	}
	l, err := ledger.Load(a.cfg.Abs(a.cfg.Paths.Ledger))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Warnings) != 1 || !l.Warned(stepWeekly, reset.Unix()) {
		t.Errorf("warning rows = %+v, want one for the weekly window at its reset", l.Warnings)
	}
}

// status names the account as the newest reading left it, and says nothing
// where no usage row carries a reading.
func TestStatusCarriesTheNewestReading(t *testing.T) {
	t.Cleanup(func() { statusJSON, statusNoTracker = false, false })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := driveProject(t)
	out := capture(t, func() { _ = execute(t, "status", "--json", "--no-tracker") })
	if strings.Contains(out, `"rate_limit"`) {
		t.Errorf("a ledger with no reading reports one:\n%s", out)
	}
	statusJSON = false
	reset := time.Now().Add(30 * time.Hour)
	for _, r := range []*runlog.RateLimit{
		reading("allowed", "five_hour", 0.5, 0.2, reset, false, ""),
		reading("allowed_warning", "seven_day", 0.79, 0.09, reset, false, ""),
	} {
		if err := ledger.Append(s.cfg.Abs(s.cfg.Paths.Ledger), ledger.Row{Usage: &runlog.Usage{Input: 1}, Source: ledger.SourceHeadless, RateLimit: r}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // ts is one second wide
	}
	out = capture(t, func() { _ = execute(t, "status", "--json", "--no-tracker") })
	var rep StatusReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.RateLimit == nil || rep.RateLimit.Remaining[stepWeekly] != 21 || rep.RateLimit.Remaining[stepFiveHour] != 91 ||
		rep.RateLimit.Project != s.cfg.Project {
		t.Errorf("rate_limit = %+v, want the newer reading: weekly 21, five_hour 91", rep.RateLimit)
	}
	statusJSON = false
	out = capture(t, func() { _ = execute(t, "status", "--no-tracker") })
	if !strings.Contains(out, "account: weekly 21% left, five-hour 91% left, not on purchased credit, week resets ") {
		t.Errorf("the footer does not name the account:\n%s", out)
	}
}

// --dry-run says whether usage_admission would hold now, and why.
func TestADryRunSaysWhetherUsageWouldHold(t *testing.T) {
	t.Cleanup(func() { driveDry = false })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := driveProject(t)
	out := capture(t, func() { _ = execute(t, "drive", "--dry-run") })
	if !strings.Contains(out, "usage_admission") || !strings.Contains(out, "no usage row in any registered project carries a rate_limit reading") {
		t.Errorf("the preflight does not say no reading was found:\n%s", out)
	}
	r := reading("rejected", "seven_day", 1, 0, time.Now().Add(30*time.Hour), false, runlog.OutOfCredits)
	if err := ledger.Append(s.cfg.Abs(s.cfg.Paths.Ledger), ledger.Row{Usage: &runlog.Usage{}, Source: ledger.SourceHeadless, RateLimit: r}); err != nil {
		t.Fatal(err)
	}
	out = capture(t, func() { _ = execute(t, "drive", "--dry-run") })
	if !strings.Contains(out, "the run would hold now: ") || !strings.Contains(out, "out_of_credits") {
		t.Errorf("the preflight does not say the run would hold on the refusal:\n%s", out)
	}
}

// A resume past a usage hold that re-executes onto a newer binary carries the
// waiver across: the new process dispatches on the reading the resume released
// rather than pausing and notifying on it again.
func TestAResumeOntoAReplacedBinaryKeepsTheUsageHoldReleased(t *testing.T) {
	enqueueFixture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfgPath := filepath.Join(".", config.FileName)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfgPath, string(raw)+"\n[usage.weekly]\npause_remaining = 5\n")
	row := ledger.Row{Usage: &runlog.Usage{Input: 1}, SessionID: "earlier", Source: ledger.SourceHeadless,
		RateLimit: reading("allowed_warning", "seven_day", 0.97, 0.1, time.Now().Add(30*time.Hour), false, "")}
	if err := ledger.Append("ledger.jsonl", row); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "drover")
	asDrover(t, bin, 1)

	var stdout, stderr bytes.Buffer
	c := exec.Command(bin, "drive", "1", "--json", "--events")
	c.Env = append(os.Environ(), envAsDrover+"=1")
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	t.Cleanup(func() {
		_ = c.Process.Kill()
		<-exited
	})

	path := mustLeasePath(t)
	waitLease(t, path, func(l lease.Lease) bool { return l.Paused && slices.Contains(l.Queue, "1") })
	asDrover(t, bin, 2)
	if err := resumeDrive(io.Discard); err != nil {
		t.Fatalf("--resume: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-exited:
		exited <- err
		if err != nil {
			t.Fatalf("drive: %v\n%s", err, stderr.String())
		}
	case <-time.After(time.Minute):
		t.Fatalf("the re-executed drive never finished: it held again on the reading the resume released\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "re-executed onto") {
		t.Errorf("the resume never re-executed:\n%s", stderr.String())
	}
	var paused int
	for _, e := range usageEventsIn(stdout.String()) {
		if e.Event == evPaused && e.Action == "usage_admission" {
			paused++
		}
	}
	if paused != 1 {
		t.Errorf("usage_admission paused the drive %d times, want 1\n%s", paused, stdout.String())
	}
}
