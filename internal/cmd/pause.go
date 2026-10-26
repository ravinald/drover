package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

// The requests `drive --pause`, `drive --resume` and `drive --stop` write into
// a running drive's lease.
const (
	ctrlPause  = "pause"
	ctrlResume = "resume"
	ctrlStop   = "stop"
	// ctrlResumeKeep is --resume --keep-binary.
	ctrlResumeKeep = "resume-keep-binary"
)

// The inputs a resume names as changed, on the resumed event.
const (
	inputConfig = "config"
	inputPrompt = "prompt"
	inputReview = "review"
	inputVerify = "verify"
)

// errStopRequested is the drain cause a --stop leaves, and how the dispatcher
// tells it from a stop an item raised.
var errStopRequested = errors.New("stopped by drive --stop")

var (
	drivePause, driveResume, driveStop bool
	// controlWait bounds how long --pause and --resume wait for the drive to
	// answer. A resume runs the launch's preflight, sign-in checks included,
	// which take up to authWait per runner.
	controlWait = 2 * time.Minute
	// driveHeld, when set, runs as an item reaches a boundary of a paused
	// drive and before it waits there. Tests set it; nothing else does.
	driveHeld func(id, next string)
)

// driveInputs is what a drive read at launch, or at its last resume, that a
// config change can move under it: the config, the reviewer it resolved, and
// the text of the two templates every brief renders from.
//
// The templates are read again at each launch whatever this holds; the copy is
// what a resume compares against to say which inputs it changed.
type driveInputs struct {
	cfg      *config.Config
	reviewer string
	toml     []byte
	prompt   string
	review   string
	// resumed is inputs a --resume loaded, rather than the launch: a re-exec
	// onto a new binary is a resume too.
	resumed bool
}

// snapshotInputs records what this drive runs under now, validating nothing:
// a template that will not read is reported by the launch that needs it.
func (s *session) snapshotInputs(reviewer string) *driveInputs {
	in := &driveInputs{cfg: s.cfg, reviewer: reviewer}
	in.toml, _ = os.ReadFile(filepath.Join(s.cfg.Root, config.FileName)) // #nosec G304 -- the project's own config
	_, in.prompt, _ = prompts.Load(s.cfg.Abs(s.cfg.Prompts.Implement), "default", prompts.DefaultTemplate())
	_, in.review, _ = prompts.Load(s.cfg.Abs(s.cfg.Prompts.Review), "review", prompts.ReviewTemplate())
	return in
}

// loadDriveInputs reads the config, the backlog and the templates afresh and
// checks them as a launch does: the load open() makes, drivePreflight under
// this drive's own flags, and each template rendered against the sample brief
// `drover check` uses. It returns the session the load opened, so the caller
// takes the backlog it read with the config it read.
func loadDriveInputs() (*driveInputs, *session, error) {
	next, err := open()
	if err != nil {
		return nil, nil, err
	}
	reviewer, err := next.drivePreflight()
	if err != nil {
		return nil, nil, err
	}
	if err := validateTemplates(next.cfg); err != nil {
		return nil, nil, err
	}
	return next.snapshotInputs(reviewer), next, nil
}

// validateTemplates renders the implement and review templates cfg names against
// the sample brief, so a resume refuses a template every launch would refuse.
func validateTemplates(cfg *config.Config) error {
	for _, t := range []struct{ path, name, fallback string }{
		{cfg.Prompts.Implement, "default", prompts.DefaultTemplate()},
		{cfg.Prompts.Review, "review", prompts.ReviewTemplate()},
	} {
		name, text, err := prompts.Load(cfg.Abs(t.path), t.name, t.fallback)
		if err == nil {
			_, err = prompts.Render(name, text, prompts.SampleFor(cfg.DeclaredKeys(), cfg.Verify))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// changed names the inputs next differs from in on. With an item, verify is
// that item's own verify command; without one, the whole table.
func (in *driveInputs) changed(next *driveInputs, it *backlog.Item) []string {
	var out []string
	if !bytes.Equal(in.toml, next.toml) {
		out = append(out, inputConfig)
	}
	if in.prompt != next.prompt {
		out = append(out, inputPrompt)
	}
	if in.review != next.review {
		out = append(out, inputReview)
	}
	gates := !maps.Equal(in.cfg.Verify, next.cfg.Verify)
	if it != nil {
		gates = prompts.SelectVerify(in.cfg.Verify, it.Meta) != prompts.SelectVerify(next.cfg.Verify, it.Meta)
	}
	if gates {
		out = append(out, inputVerify)
	}
	return out
}

func (q *driveSched) pause() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = true
	q.publishHolds()
	// An item already waiting for its trunk turn holds there from now on.
	q.cond.Broadcast()
}

// resume releases every held item onto in, which each adopts at the boundary
// it was held at.
func (q *driveSched) resume(in *driveInputs) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused, q.inputs, q.reexec = false, in, ""
	q.publishHolds()
	q.cond.Broadcast()
}

func (q *driveSched) isPaused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

// hold waits while the drive is paused, and returns the inputs it runs under
// once it is not. A drain or an interrupt lets the item go too, to park at the
// boundary as it would have unpaused. While it waits, at is in the lease's
// holds, which is how --pause --wait tells a held item from a busy one.
func (q *driveSched) hold(ctx context.Context, at lease.Hold) *driveInputs {
	q.mu.Lock()
	if !q.paused || q.stop != nil || ctx.Err() != nil {
		defer q.mu.Unlock()
		return q.inputs
	}
	q.holds[at.ID] = at
	q.publishHolds()
	wake := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	for q.paused && q.stop == nil && ctx.Err() == nil {
		q.cond.Wait()
	}
	delete(q.holds, at.ID)
	q.publishHolds()
	in := q.inputs
	q.mu.Unlock()
	wake()
	return in
}

// publishHolds writes the items held now into the lease: none while the drive
// is not paused, since an item a resume woke is back at work before it takes
// q.mu to leave q.holds. Called with q.mu held, by every change to paused or to
// q.holds, so the lease is written in the order those changes were made and
// the last write is the state now. A snapshot taken under the lock and written
// after it can land behind a newer one.
func (q *driveSched) publishHolds() {
	if held == nil {
		return
	}
	var holds []lease.Hold
	if q.paused {
		holds = slices.SortedFunc(maps.Values(q.holds), func(a, b lease.Hold) int { return strings.Compare(a.ID, b.ID) })
	}
	_ = held.Update(func(l *lease.Lease) { l.Holds = holds })
}

// heldAt is a stage boundary: the item waits here while the drive is paused,
// then takes whatever a resume reloaded, and reports which of its inputs that
// changed. next is the stage it goes on to.
func (s *session) heldAt(ctx context.Context, it backlog.Item, next string) []string {
	if s.slot == nil || s.inputs == nil {
		return nil
	}
	// usage_admission before each stage that launches a session, so a drive
	// whose account ran low while this item's last session ran starts nothing
	// more. The hold below is where the item then waits.
	if (next == ledger.StageImplement || next == ledger.StageReview) && s.slot.sched.draining() == nil {
		s.admitUsage(s.slot.sched)
	}
	if driveHeld != nil && s.slot.sched.isPaused() {
		driveHeld(it.ID.Bare(), next)
	}
	in := s.slot.sched.hold(ctx, lease.Hold{ID: it.ID.Bare(), At: next, Verify: prompts.SelectVerify(s.cfg.Verify, it.Meta)})
	if in == nil || in == s.inputs {
		return nil
	}
	changed := s.inputs.changed(in, &it)
	s.inputs, s.cfg = in, in.cfg
	events.emit(DriveEvent{Event: evResumed, ID: it.ID.Bare(), Stage: next, Changed: changed})
	if len(changed) > 0 {
		fmt.Fprintf(errOut, "  · %s: resumed before %s under the reloaded %s\n", it.ID.Bare(), next, strings.Join(changed, ", "))
	}
	return changed
}

// reverify runs an item's verify command after a resume changed it, on the
// worktree as it stands: the stage the item just finished was told to pass the
// old one. A refusal is sent back as the merge gate's is, and again sends the
// item to its next implement run.
func (s *session) reverify(ctx context.Context, it backlog.Item, d *DriveItem,
	park func(cause, why string) DriveItem,
) (out DriveItem, again, stop bool) {
	gate := prompts.SelectVerify(s.cfg.Verify, it.Meta)
	if strings.TrimSpace(gate) == "" {
		return DriveItem{}, false, false
	}
	events.stage(d.ID, stageGate)
	fmt.Fprintf(errOut, "  · %s: its verify command changed while the drive was paused; running the new one on %s: %s\n",
		d.ID, d.Branch, gate)
	code, gout, err := s.runItemVerify(ctx, d.ID, s.workDir(), gate)
	switch {
	case ctx.Err() != nil:
		return park("interrupted", "interrupted while the item's new verify command ran after a resume"), false, true
	case err != nil:
		return park("gate_unrunnable", "the item's new verify command would not start after a resume: "+err.Error()), false, true
	case code == 0:
		return DriveItem{}, false, false
	}
	// request-changes points the process's stdout elsewhere while it writes, which two
	// slots must not do at once; the trunk stage does it under this lock too.
	release := s.holdTrunk()
	err = s.requestChangesFromVerify(it, "The item's verify command changed while the drive was paused, and the new one refused this branch. Its output:", gout)
	release()
	if err != nil {
		return park(parkGateRefused, "the new verify command refused this branch after a resume, and the finding could not be written back: "+err.Error()), false, true
	}
	d.Rounds++
	return DriveItem{}, true, false
}

// answerControl acts on a request another process wrote into this drive's
// lease, and answers it there. A resume that will not load leaves the drive
// paused and the reason in the answer.
func (s *session) answerControl(sched *driveSched) {
	if held == nil {
		return
	}
	req, err := held.Requested()
	if err != nil || req == "" {
		return
	}
	refused := ""
	switch verb, _, _ := strings.Cut(req, " "); verb {
	case ctrlPause:
		if !sched.isPaused() {
			sched.pause()
			fmt.Fprintln(errOut, "· paused by drive --pause: nothing more is dispatched, and each running item holds at its next stage boundary until drive --resume")
			events.emit(DriveEvent{Event: evPaused})
		}
	case ctrlStop:
		if !errors.Is(sched.draining(), errStopRequested) {
			sched.drain(errStopRequested)
			fmt.Fprintln(errOut, "· stopped by drive --stop: nothing more is dispatched, and each running item finishes the stage it is in and parks stopped")
		}
	case ctrlResume, ctrlResumeKeep:
		if !sched.isPaused() {
			refused = "the drive is not paused"
			break
		}
		if to, ok := startedFrom.replaced(); ok && verb == ctrlResume {
			// Loaded by the binary it is going to run under, never by this
			// one: a key only the newer binary knows is the usual reason the
			// binary was replaced.
			if err := probeResume(to); err != nil {
				refused = err.Error()
				fmt.Fprintf(errOut, "· drive --resume refused, and the drive stays paused: %v\n", err)
				break
			}
			releaseUsage(sched)
			sched.mu.Lock()
			if sched.reexec == "" {
				sched.reexecSince = time.Now()
			}
			sched.reexec = to.String()
			sched.mu.Unlock()
			fmt.Fprintf(errOut, "· resumed by drive --resume onto %s, which replaced %s at %s; "+
				"re-executing once every running item holds at a boundary\n", to, startedFrom, to.path)
			break
		}
		in, next, err := loadDriveInputs()
		if err != nil {
			refused = err.Error()
			fmt.Fprintf(errOut, "· drive --resume refused, and the drive stays paused: %v\n", err)
			break
		}
		in.resumed = true
		changed := s.inputs.changed(in, nil)
		s.cfg, s.inputs = in.cfg, in
		publishConfig(in)
		s.backlog, s.ledger, s.shelf = next.backlog, next.ledger, next.shelf
		events.reconfigure(in.cfg)
		releaseUsage(sched)
		sched.resume(in)
		what := "nothing it reads had changed"
		if len(changed) > 0 {
			what = "reloaded " + strings.Join(changed, ", ")
		}
		fmt.Fprintf(errOut, "· resumed by drive --resume; %s\n", what)
		events.emit(DriveEvent{Event: evResumed, Changed: changed})
	default:
		refused = "drover does not know the request " + verb
	}
	sched.mu.Lock()
	paused, reexec := sched.paused, sched.reexec
	sched.mu.Unlock()
	_ = held.Update(func(l *lease.Lease) { l.Paused, l.Answered, l.Refused, l.Reexec = paused, req, refused, reexec })
}

// releaseUsage lifts a usage hold an operator resumed past, and waives it, so
// the next admission does not take the same hold on the same reading.
func releaseUsage(sched *driveSched) {
	if sched.usage == nil {
		return
	}
	if u := sched.usage.release(true); u != nil {
		fmt.Fprintf(errOut, "· drive --resume releases the usage hold on the %s window, which stays released until it resets\n", u.Window)
	}
}

// errNoDrive is a control request with no drive to take it.
var errNoDrive = errors.New("no drive is running")

// flagOf is the flag a request verb came from, as a message names it.
func flagOf(verb string) string {
	if verb == ctrlResumeKeep {
		return ctrlResume
	}
	return verb
}

// driveName is how a message names the drive a lease belongs to.
func driveName(l lease.Lease) string {
	if l.Drive == "" {
		return fmt.Sprintf("drive pid %d", l.PID)
	}
	return fmt.Sprintf("drive %s (pid %d)", l.Drive, l.PID)
}

// controlDrive writes one request into the lease of the drive running in this
// checkout and waits for its answer. It needs no config that loads: a pause is
// how an operator gets to fix one.
func controlDrive(verb string) (lease.Lease, error) {
	path, err := controlPath()
	if err != nil {
		return lease.Lease{}, err
	}
	return controlAt(path, verb)
}

// controlPath is the lease of this checkout's project.
func controlPath() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := config.Find(wd)
	if err != nil {
		return "", err
	}
	return (&session{cfg: &config.Config{Root: root}}).leasePath()
}

// controlAt writes one request into the lease at path and waits for the drive
// holding it to answer.
func controlAt(path, verb string) (lease.Lease, error) {
	flag := flagOf(verb)
	req := fmt.Sprintf("%s %d-%d", verb, os.Getpid(), time.Now().UnixNano())
	var err error
	for range 50 {
		err = lease.Signal(path, req, func(l lease.Lease, busy bool) error {
			switch {
			case !busy || l.Command != "drive":
				return Refusal{fmt.Errorf("%w in this checkout, so there is nothing to %s", errNoDrive, flag)}
			case flag == ctrlResume && !l.Paused:
				return Refusal{fmt.Errorf("%s is not paused, so there is nothing to resume", driveName(l))}
			}
			return nil
		})
		if _, unnamed := errors.AsType[lease.Held](err); !unnamed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return lease.Lease{}, err
	}
	deadline := time.Now().Add(controlWait)
	for {
		l, busy, err := lease.Read(path)
		switch {
		case err != nil:
			return l, err
		case !busy && flag == ctrlStop:
			return l, nil
		case !busy:
			return l, fmt.Errorf("the drive exited before it answered --%s", flag)
		case flag == ctrlStop && l.Closed:
			// Already on its way out, and a drive that is stopping answers
			// nothing until its running items come back.
			return l, nil
		case l.Answered == req && l.Refused != "":
			return l, Refusal{fmt.Errorf("%s refused --%s and stays paused: %s", driveName(l), flag, l.Refused)}
		case l.Answered == req:
			return l, nil
		case time.Now().After(deadline):
			return l, fmt.Errorf("%s did not answer --%s within %s; the request stays in its lease, and it acts on it when it next looks", driveName(l), flag, controlWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pausedLine is what --pause prints about a drive that took it.
func pausedLine(l lease.Lease) string {
	running := "nothing is running"
	if len(l.Running) > 0 {
		running = strings.Join(l.Running, " ") + " hold at the next stage boundary each reaches"
	}
	return fmt.Sprintf("paused %s: it dispatches nothing more, and %s", driveName(l), running)
}

// resumedLine is what --resume prints about a drive that took it.
func resumedLine(l lease.Lease) string {
	if l.Reexec != "" {
		return fmt.Sprintf("resumed %s onto %s: it re-executes once every running item holds at a boundary", driveName(l), l.Reexec)
	}
	return "resumed " + driveName(l)
}

// resumeVerb is the request --resume writes, under --keep-binary or not.
func resumeVerb() string {
	if driveKeepBinary {
		return ctrlResumeKeep
	}
	return ctrlResume
}

// stopDrive is --stop.
func stopDrive(w io.Writer) error {
	l, err := controlDrive(ctrlStop)
	if err != nil {
		return err
	}
	if l.PID == 0 {
		fmt.Fprintln(w, "the drive exited before it answered --stop")
		return nil
	}
	running := "nothing is running, so it exits now"
	if len(l.Running) > 0 {
		running = strings.Join(l.Running, " ") + " finish the stage each is in and park stopped, then it exits"
	}
	fmt.Fprintf(w, "stopping %s: it dispatches nothing more, and %s\n", driveName(l), running)
	return nil
}

// pauseDrive is --pause.
func pauseDrive(w io.Writer) error {
	l, err := controlDrive(ctrlPause)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s.\nChange the config, then run drover drive --resume\n", pausedLine(l))
	return nil
}

// resumeDrive is --resume. The config and the templates are checked here
// first, so a file that will not load is refused before the drive is asked;
// the drive checks again with its own flags, and refuses the same way.
func resumeDrive(w io.Writer) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.Load(wd)
	if err == nil {
		err = validateTemplates(cfg)
	}
	if err != nil {
		return Refusal{fmt.Errorf("the config or a template would not load, so the drive stays paused: %w", err)}
	}
	l, err := controlDrive(resumeVerb())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, resumedLine(l))
	return nil
}

// driveControl is drive's modes that act on a drive rather than run one:
// --pause, --resume, --stop and --cleanup, and --all, --wait and --keep-binary
// beside the first two. None takes ids or the flags a drive runs under.
//
// timeoutSet is whether --timeout was passed.
func driveControl(ctx context.Context, args []string, timeoutSet bool) error {
	n := 0
	for _, on := range []bool{drivePause, driveResume, driveStop, driveCleanup} {
		if on {
			n++
		}
	}
	switch {
	case driveApply && !driveCleanup:
		return Refusal{fmt.Errorf("--apply removes what --cleanup lists; pass it with --cleanup")}
	case n > 1:
		return Refusal{fmt.Errorf("--pause, --resume, --stop and --cleanup each do one thing; pass one")}
	case driveEveryProject && !drivePause && !driveResume:
		return Refusal{fmt.Errorf("--all pauses or resumes the drive in every registered project; pass it with --pause or --resume")}
	case driveWait && !drivePause:
		return Refusal{fmt.Errorf("--wait waits for the drives --pause paused to go quiet; pass it with --pause")}
	case timeoutSet && !driveWait:
		return Refusal{fmt.Errorf("--timeout bounds --wait; pass it with --wait")}
	case driveWait && driveTimeout <= 0:
		return Refusal{fmt.Errorf("--timeout is how long --wait waits, so it is more than zero (got %s)", driveTimeout)}
	case driveKeepBinary && !driveResume:
		return Refusal{fmt.Errorf("--keep-binary resumes a drive on the binary it started with; pass it with --resume")}
	case len(args) > 0:
		return Refusal{fmt.Errorf("--pause, --resume, --stop and --cleanup act on this checkout, not on ids; pass none")}
	case driveDetach:
		return Refusal{fmt.Errorf("--detach starts a drive, and --pause, --resume, --stop and --cleanup act on one; pass one")}
	case driveStop:
		return stopDrive(os.Stdout)
	case drivePause && driveEveryProject:
		return pauseAll(ctx, os.Stdout)
	case driveResume && driveEveryProject:
		return resumeAll(os.Stdout)
	case drivePause:
		if cfg, err := config.Load("."); err == nil {
			if p, _ := cfg.Policy("pause_for_config_change"); p == polOff {
				return Refusal{fmt.Errorf("[supervise] pause_for_config_change is off in this project, so no drive here pauses")}
			}
		}
		if !driveWait {
			return pauseDrive(os.Stdout)
		}
		path, err := controlPath()
		if err != nil {
			return err
		}
		if err := pauseDrive(os.Stdout); err != nil {
			return err
		}
		return waitQuiet(ctx, os.Stdout, []driveTarget{{project: "this checkout", path: path}})
	case driveResume:
		return resumeDrive(os.Stdout)
	}
	s, err := open()
	if err != nil {
		return err
	}
	if !s.cfg.Git.On() {
		return Refusal{fmt.Errorf("[git] enabled is false, so no drive here left worktrees or branches")}
	}
	if p, _ := s.cfg.Policy("cleanup_interrupted_drive"); driveApply && p == polOff {
		return Refusal{fmt.Errorf("[supervise] cleanup_interrupted_drive is off in this project, so --apply removes nothing; --cleanup alone lists")}
	}
	return s.cleanupDrive(os.Stdout, driveApply)
}

// driveDispatching, when set, runs as an item is dispatched and before the
// drive compares the config on disk. Tests set it; nothing else does.
var driveDispatching func(id string)

// noticeConfig reports a .drover.toml on disk that differs from the one this
// drive runs under, once per distinct content, and applies nothing: a running
// drive keeps the config it started or last resumed with, and taking an edit
// stays --pause then --resume, under pause_for_config_change.
//
// The file is read under the schedule's lock so that two slots meeting the
// same edit at once report it once.
func (s *session) noticeConfig() {
	if s.slot == nil {
		return
	}
	q := s.slot.sched
	q.mu.Lock()
	in := q.inputs
	if q.paused || in == nil {
		q.mu.Unlock()
		return
	}
	if q.configBase != in {
		q.configBase, q.configSeen = in, ""
	}
	path := filepath.Join(in.cfg.Root, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the project's own config
	seen := ""
	switch {
	case err != nil:
		seen = "error: " + err.Error()
	case !bytes.Equal(raw, in.toml):
		seen = configDigest(raw)
	}
	report := seen != "" && seen != q.configSeen
	q.configSeen = seen
	q.mu.Unlock()
	if !report {
		return
	}

	since := "started"
	if in.resumed {
		since = "last resumed"
	}
	ev := DriveEvent{Event: evConfigChanged}
	if err != nil {
		ev.Error = err.Error()
		fmt.Fprintf(errOut, "  ! config on disk will not read: %v; this drive goes on under the .drover.toml it %s with\n", err, since)
		events.emit(ev)
		return
	}
	at := "an unknown time"
	if fi, serr := os.Stat(path); serr == nil {
		at = fi.ModTime().Local().Format(time.DateTime)
	}
	line := fmt.Sprintf("  ! config changed on disk at %s: this drive still runs the .drover.toml it %s with; "+
		"drover drive --pause, then --resume, applies it", at, since)
	if _, lerr := config.LoadAt(in.cfg.Root); lerr != nil {
		ev.Error = lerr.Error()
		line += fmt.Sprintf("; the new file will not load, so a --resume refuses it: %v", lerr)
	} else if ev.Tables = changedTables(in.toml, raw); len(ev.Tables) > 0 {
		line += "; changed: " + strings.Join(ev.Tables, ", ")
	} else {
		line += "; no setting changed, only comments or layout"
	}
	fmt.Fprintln(errOut, line)
	events.emit(ev)
}

// configDigest is the SHA-256 of a config's bytes, hex, as the lease's
// config_sha256 carries it.
func configDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// changedTables names each top-level table, bracketed, and each top-level key
// whose decoded contents differ between two configs, sorted. Comments and
// layout differ in no table.
func changedTables(was, now []byte) []string {
	var a, b map[string]any
	_, _ = toml.Decode(string(was), &a)
	_, _ = toml.Decode(string(now), &b)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if reflect.DeepEqual(a[k], b[k]) {
			continue
		}
		_, ta := a[k].(map[string]any)
		_, tb := b[k].(map[string]any)
		if ta || tb {
			k = "[" + k + "]"
		}
		out = append(out, k)
	}
	return out
}

// publishConfig writes the digest of the config this drive runs under into its
// lease, so status in another process can tell when the file moved on.
func publishConfig(in *driveInputs) {
	if held == nil || in == nil {
		return
	}
	_ = held.Update(func(l *lease.Lease) { l.ConfigSHA256, l.ConfigResumed = configDigest(in.toml), in.resumed })
}

// configDrift reports whether the .drover.toml on disk differs from the one
// the drive holding l runs under. A lease from a binary that never wrote the
// digest, or a file that will not read, reports nothing.
func (s *session) configDrift(l lease.Lease) bool {
	if l.Command != "drive" || l.ConfigSHA256 == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.Root, config.FileName)) // #nosec G304 -- the project's own config
	return err == nil && configDigest(raw) != l.ConfigSHA256
}
