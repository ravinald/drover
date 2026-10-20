package cmd

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runner"
)

// stubDrive holds a project's lease as a drive does and answers each request
// written into it as answerControl does, with running items that stay inside
// a stage until drain moves them to a boundary.
type stubDrive struct {
	path  string
	mu    sync.Mutex
	quiet bool
}

func startStubDrive(t *testing.T, root, id string, running ...string) *stubDrive {
	t.Helper()
	path, err := (&session{cfg: &config.Config{Root: root}}).leasePath()
	if err != nil {
		t.Fatal(err)
	}
	h, err := lease.Acquire(path, lease.Lease{Command: "drive", Root: root, Drive: id, Running: running})
	if err != nil {
		t.Fatal(err)
	}
	d := &stubDrive{path: path}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			req, _ := h.Requested()
			d.mu.Lock()
			quiet := d.quiet
			d.mu.Unlock()
			_ = h.Update(func(l *lease.Lease) {
				switch verb, _, _ := strings.Cut(req, " "); verb {
				case ctrlPause:
					l.Paused, l.Answered = true, req
				case ctrlResume, ctrlResumeKeep:
					l.Paused, l.Answered = false, req
				}
				l.Holds = nil
				if l.Paused && quiet {
					for _, id := range l.Running {
						l.Holds = append(l.Holds, lease.Hold{ID: id, At: ledger.StageReview})
					}
				}
			})
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		_ = h.Release()
	})
	return d
}

func (d *stubDrive) drain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.quiet = true
}

func (d *stubDrive) lease(t *testing.T) lease.Lease {
	t.Helper()
	l, _, err := lease.Read(d.path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// registered stands up a fixture project per name, each under extra, in a
// registry of its own, and leaves the caller outside all of them.
func registered(t *testing.T, extra map[string]string, names ...string) map[string]string {
	t.Helper()
	t.Cleanup(func() {
		drivePause, driveResume, driveEveryProject, driveWait, driveKeepBinary = false, false, false, false, false
		driveTimeout = 2 * time.Hour
		driveCmd.Flags().Lookup("timeout").Changed = false
	})
	poll := quietPoll
	quietPoll = 20 * time.Millisecond
	t.Cleanup(func() { quietPoll = poll })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	roots := map[string]string{}
	for _, name := range names {
		s := fixture(t, extra[name])
		if err := projects.Append(reg, projects.Row{Project: name, Root: s.cfg.Root}); err != nil {
			t.Fatal(err)
		}
		roots[name] = s.cfg.Root
	}
	t.Chdir(t.TempDir())
	return roots
}

func driveOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	drivePause, driveResume, driveEveryProject, driveWait = false, false, false, false
	var err error
	out := capture(t, func() { captureErr(t, func() { err = execute(t, append([]string{"drive"}, args...)...) }) })
	return out, err
}

// --pause --all reaches every registered project's drive through its lease
// and says, per project, what became of it: paused with its id, no drive
// there, or refused by that project's own policy, which costs the exit 0.
func TestPauseAllPausesEveryRegisteredDriveAndNamesTheIdleProject(t *testing.T) {
	off := "\n[supervise]\npause_for_config_change = \"off\"\n"
	roots := registered(t, map[string]string{"delta": off}, "alpha", "beta", "gamma", "delta")
	alpha := startStubDrive(t, roots["alpha"], "D1", "1")
	beta := startStubDrive(t, roots["beta"], "D4", "2")
	delta := startStubDrive(t, roots["delta"], "D2")

	out, err := driveOut(t, "--pause", "--all")
	if Code(err) != ExitRefused {
		t.Errorf("--pause --all with delta's policy off exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{
		"alpha  paused drive D1 (pid ",
		"beta   paused drive D4 (pid ",
		"gamma  no drive running",
		"delta  could not be paused: [supervise] pause_for_config_change is off there",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--pause --all does not print %q:\n%s", want, out)
		}
	}
	if !alpha.lease(t).Paused || !beta.lease(t).Paused {
		t.Errorf("alpha paused=%v, beta paused=%v; want both paused", alpha.lease(t).Paused, beta.lease(t).Paused)
	}
	if delta.lease(t).Paused {
		t.Error("delta's policy is off and its drive was paused anyway")
	}

	// Without the refusing project, every live drive took it, so exit 0.
	roots = registered(t, nil, "alpha", "beta", "gamma")
	startStubDrive(t, roots["alpha"], "D1", "1")
	startStubDrive(t, roots["beta"], "D4")
	if out, err := driveOut(t, "--pause", "--all"); err != nil {
		t.Errorf("--pause --all over two live drives and an idle project: %v\n%s", err, out)
	}
}

// A supervisor pauses only the projects whose own policy lets it, whatever
// the project it stands in allows.
func TestPauseAllJudgesEachProjectsPolicyForASupervisor(t *testing.T) {
	sup := "\n[supervise]\npause_for_config_change = \"supervisor\"\n"
	roots := registered(t, map[string]string{"alpha": sup}, "alpha", "beta")
	alpha := startStubDrive(t, roots["alpha"], "D1")
	beta := startStubDrive(t, roots["beta"], "D1")
	t.Chdir(roots["alpha"])
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)

	out, err := driveOut(t, "--pause", "--all")
	if Code(err) != ExitRefused || !strings.Contains(out, "beta   could not be paused: [supervise] pause_for_config_change is \"operator\" there") {
		t.Errorf("a supervisor's --pause --all exited %d (%v), want %d naming beta's policy:\n%s", Code(err), err, ExitRefused, out)
	}
	if !alpha.lease(t).Paused || beta.lease(t).Paused {
		t.Errorf("alpha paused=%v, beta paused=%v; want alpha alone paused", alpha.lease(t).Paused, beta.lease(t).Paused)
	}
}

// --wait returns once every paused drive's running items hold at a boundary,
// and at --timeout refuses naming the drive still inside a stage, which stays
// paused.
func TestPauseAllWaitReturnsOnceTheDrivesAreQuiet(t *testing.T) {
	roots := registered(t, nil, "alpha")
	alpha := startStubDrive(t, roots["alpha"], "D1", "1", "3")
	go func() {
		time.Sleep(200 * time.Millisecond)
		alpha.drain()
	}()
	out, err := driveOut(t, "--pause", "--all", "--wait", "--timeout", "30s")
	if err != nil {
		t.Fatalf("--wait over a drive that drains: %v\n%s", err, out)
	}
	if !strings.Contains(out, "waiting on:\n  alpha drive D1 (pid ") || !strings.Contains(out, "): 1 3") {
		t.Errorf("--wait never said it was waiting on alpha's 1 and 3:\n%s", out)
	}
	if !strings.Contains(out, "quiet: no paused drive") {
		t.Errorf("--wait returned without saying the drives are quiet:\n%s", out)
	}

	roots = registered(t, nil, "alpha", "beta")
	startStubDrive(t, roots["alpha"], "D1", "1").drain()
	beta := startStubDrive(t, roots["beta"], "D3", "2")
	out, err = driveOut(t, "--pause", "--all", "--wait", "--timeout", "300ms")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "beta drive D3 (pid ") || strings.Contains(err.Error(), "alpha") {
		t.Errorf("--wait at a short --timeout exited %d (%v), want %d naming beta alone\n%s", Code(err), err, ExitRefused, out)
	}
	if !beta.lease(t).Paused {
		t.Error("the --wait that timed out left beta unpaused")
	}
}

// --resume --all checks each project's config before asking its drive, so a
// project whose config will not load stays paused and is named, and the
// others resume.
func TestResumeAllLeavesABrokenConfigPausedAndResumesTheRest(t *testing.T) {
	roots := registered(t, nil, "alpha", "beta")
	alpha := startStubDrive(t, roots["alpha"], "D1", "1")
	beta := startStubDrive(t, roots["beta"], "D2", "2")
	if out, err := driveOut(t, "--pause", "--all"); err != nil {
		t.Fatalf("--pause --all: %v\n%s", err, out)
	}
	write(t, roots["beta"]+"/"+config.FileName, "[run\n")

	out, err := driveOut(t, "--resume", "--all")
	if Code(err) != ExitRefused {
		t.Errorf("--resume --all over a broken config exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	if !strings.Contains(out, "alpha  resumed drive D1 (pid ") ||
		!strings.Contains(out, "beta   stays paused: the config or a template would not load") {
		t.Errorf("--resume --all does not name alpha resumed and beta paused:\n%s", out)
	}
	if alpha.lease(t).Paused || !beta.lease(t).Paused {
		t.Errorf("alpha paused=%v, beta paused=%v; want alpha resumed and beta still paused", alpha.lease(t).Paused, beta.lease(t).Paused)
	}
}
