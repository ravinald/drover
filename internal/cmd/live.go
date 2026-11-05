package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// The parks a live check can end an item on.
const (
	parkLiveBusy        = "live_busy"
	parkLiveUnavailable = "live_unavailable"
	parkLiveFailed      = "live_failed"
)

// liveExitUnavailable is the code a live check exits with to say the
// environment, not the work, is the problem: hosts down, credentials expired.
const liveExitUnavailable = 4

// liveRun is one live check's outcome. ran is false for an item that runs no
// check; busy is a lock nothing released inside [live] wait.
type liveRun struct {
	ran   bool
	busy  bool
	name  string
	head  string
	exit  int
	out   string
	start error
}

// LiveReport is the latest live result for one item, as show and status
// --json carry it. Stale is a result on a head the branch no longer points at,
// which says nothing about the head that would merge.
type LiveReport struct {
	Check string `json:"check"`
	Head  string `json:"head"`
	Exit  int    `json:"exit"`
	Line  string `json:"line,omitempty"`
	Stale bool   `json:"stale,omitempty"`
}

// liveReport is it's latest live result, nil when none was ever recorded.
func (s *session) liveReport(it backlog.Item) *LiveReport {
	r := s.ledger.LatestLive(it.ID.Bare())
	if r == nil {
		return nil
	}
	rep := &LiveReport{Check: r.Check, Head: r.Head, Exit: r.Exit, Line: r.Line}
	if branch := s.branchRef(it); branch != "" {
		if head, err := revParse(s.cfg.Root, branch); err == nil && !strings.HasPrefix(head, r.Head) && !strings.HasPrefix(r.Head, head) {
			rep.Stale = true
		}
	}
	return rep
}

// liveCheckFor is the check it runs, empty for one that runs none.
func (s *session) liveCheckFor(it backlog.Item) (string, config.LiveCheck, error) {
	return s.cfg.LiveFor(it.Meta[backlog.KeyLive])
}

// errLiveBusy is a lock another check held past [live] wait.
var errLiveBusy = errors.New("lock held")

// liveLockPoll is how often a waiting check asks for its lock again.
var liveLockPoll = time.Second

// acquireLiveLock takes the host-wide lock name, waiting up to wait. The lock is
// an flock on a file under $XDG_STATE_HOME/drover/locks, so it is released
// when the process holding it exits however it exits, and two drives in two
// projects naming one lock still wait on each other.
func acquireLiveLock(name string, wait time.Duration) (func(), error) {
	dir, err := stateDir("locks")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- name is validated at load
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		held, err := tryLiveLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if held {
			return func() {
				releaseLiveLock(f)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errLiveBusy
		}
		time.Sleep(liveLockPoll)
	}
}

// runLive runs it's live check against branch at head, under its lock, and
// records the result: one ledger row and one stage event. drover runs the argv
// itself and never hands it to a session.
func (s *session) runLive(ctx context.Context, it backlog.Item, branch, head string) (liveRun, error) {
	name, check, err := s.liveCheckFor(it)
	if err != nil || name == "" {
		return liveRun{}, err
	}
	r := liveRun{ran: true, name: name, head: head}
	if check.Lock != "" {
		release, err := acquireLiveLock(check.Lock, s.cfg.LiveWait())
		if errors.Is(err, errLiveBusy) {
			r.busy = true
			return r, nil
		}
		if err != nil {
			return r, fmt.Errorf("live check %s: lock %s: %w", name, check.Lock, err)
		}
		defer release()
	}
	cmd := exec.CommandContext(ctx, check.Argv[0], check.Argv[1:]...) // #nosec G204 -- argv is the project's own [live] config
	cmd.Dir = s.cfg.Root
	cmd.Env = append(os.Environ(), "DROVER_ITEM="+it.ID.Bare(), "DROVER_BRANCH="+branch, "DROVER_SHA="+head)
	out, err := cmd.CombinedOutput()
	r.out = string(out)
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		r.exit = exit.ExitCode()
	default:
		// A check that never started is the environment's problem, like exit 4,
		// and never a finding against the work.
		r.start, r.exit = err, liveExitUnavailable
		r.out = err.Error()
	}
	line := firstLine(strings.TrimSpace(r.out))
	if err := s.record(ledger.Row{ID: it.ID.Bare(), Date: ledger.Today(),
		Live: &ledger.LiveRun{Check: name, Head: head, Exit: r.exit, Line: line}}); err != nil {
		return r, fmt.Errorf("live check %s ran, and the ledger would not record it: %w", name, err)
	}
	events.emit(DriveEvent{Event: evStage, ID: it.ID.Bare(), Stage: stageLive, Branch: branch,
		Result: fmt.Sprint(r.exit), Reason: line})
	return r, nil
}

// driveLive is drive's live step: after the review passed and before the merge
// gate, inside the trunk stage. stop is true when the item went anywhere but
// on to the merge.
func (s *session) driveLive(ctx context.Context, it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem) (DriveItem, bool) {
	head, err := revParse(s.cfg.Root, d.Branch)
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	events.stage(d.ID, stageLive)
	r, err := s.runLive(ctx, it, d.Branch, head)
	if err != nil {
		return park(parkLiveFailed, err.Error()), true
	}
	switch {
	case !r.ran:
		return DriveItem{}, false
	case r.busy:
		return park(parkLiveBusy, fmt.Sprintf("live check %s waited [live] wait (%s) for another check holding its lock", r.name, s.cfg.LiveWait())), true
	case r.exit == 0:
		fmt.Fprintf(errOut, "  · %s: live check %s passed on %s\n", d.ID, r.name, shortSHA(head))
		return DriveItem{}, false
	case r.exit == liveExitUnavailable:
		d.setDetail(parkDetail{Detail: detailUnknown, Evidence: firstLine(strings.TrimSpace(r.out))})
		return park(parkLiveUnavailable, fmt.Sprintf("live check %s could not reach what it checks (exit %d), so nothing went back to the branch: %s",
			r.name, r.exit, firstLine(strings.TrimSpace(r.out)))), true
	}
	header := fmt.Sprintf("drover drive ran the live check %s on %s at %s, and it exited %d. It exercises the change against what a real client runs on, "+
		"which no session reaches, so its output below is the finding.", r.name, d.Branch, shortSHA(head), r.exit)
	if err := s.requestChangesFromVerify(it, ledger.ChangesLive, "### Live check "+r.name+"\n\n"+header, r.out); err != nil {
		return park(parkLiveFailed, fmt.Sprintf("live check %s exited %d on %s, and the finding could not be written back: %v", r.name, r.exit, shortSHA(head), err)), true
	}
	d.Reason = fmt.Sprintf("live check %s exited %d on %s; sent back", r.name, r.exit, shortSHA(head))
	return s.sendBackOrPark(it, d), true
}

// liveGate refuses a head whose item names a live check with no passing run on
// that head. It is what open-pr and a supervisor's finish ask, because each is
// a claim that the branch may merge.
func (s *session) liveGate(it backlog.Item, head string) error {
	name, _, err := s.liveCheckFor(it)
	if err != nil {
		return Refusal{err}
	}
	if name == "" || s.ledger.LivePassed(it.ID.Bare(), name, head) {
		return nil
	}
	return Refusal{fmt.Errorf("%s names the live check %s, and no run of it passed on %s: a pull request is the claim the branch may merge, and nothing checked it against a real client.\n"+
		"Run it: drover live %s", it.ID.Bare(), name, shortSHA(head), it.ID.Bare())}
}

var liveCmd = &cobra.Command{
	Use:   "live <id>",
	Short: "Run an item's live check against its branch head and record the result.",
	Long: `drive runs an item's live check after its review passes and before the merge
gate. This runs the same check by hand, against the head the item's branch
points at now, and records the same row, so open-pr can open a pull request
for a head the check passed on.

The check is the one the item's live key names, or [live] default when it
names none; live: none opts it out. drover runs its argv from the project root
with DROVER_ITEM, DROVER_BRANCH and DROVER_SHA in the environment, holding its
lock across every drive and project on this host.

It exits as the check did: 0 passed, 4 could not reach what it checks, and any
other code a failure. A check whose lock stays held past [live] wait exits 3.
It runs against real hosts, so it refuses inside a session drover launched.`,
	Example: `  drover live 7                     # run 7's check on its branch head
  drover open-pr 7                  # opens once that head passed`,
	Args: needID("live <id>"),
	RunE: func(c *cobra.Command, args []string) error {
		if runner.Launched() {
			return Refusal{fmt.Errorf("live runs a check against real hosts, and drover launched this session: report the item's state and let the operator run it")}
		}
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		name, _, err := s.liveCheckFor(it)
		if err != nil {
			return Refusal{err}
		}
		if name == "" {
			return Refusal{fmt.Errorf("%s runs no live check: it sets live: none, or names none and [live] has no default", it.ID.Bare())}
		}
		branch := s.branchRef(it)
		if branch == "" {
			return Refusal{fmt.Errorf("%s has no branch to check: run it first", it.ID.Bare())}
		}
		head, err := revParse(s.cfg.Root, branch)
		if err != nil {
			return err
		}
		r, err := s.runLive(c.Context(), it, branch, head)
		if err != nil {
			return err
		}
		if r.busy {
			return Refusal{fmt.Errorf("live check %s waited [live] wait (%s) for another check holding its lock", name, s.cfg.LiveWait())}
		}
		fmt.Print(r.out)
		if r.exit == 0 {
			fmt.Printf("live check %s passed on %s at %s\n", name, branch, shortSHA(head))
			return nil
		}
		return ExitWith{Code: r.exit, Err: fmt.Errorf("live check %s exited %d on %s at %s", name, r.exit, branch, shortSHA(head))}
	},
}

func init() {
	root.AddCommand(liveCmd)
}
