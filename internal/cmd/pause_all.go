package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runner"
)

var (
	driveEveryProject, driveWait bool
	driveTimeout                 time.Duration
	// quietPoll is how often --wait reads the paused drives' leases, and
	// quietReport how often at most it says what it is still waiting on.
	quietPoll   = 2 * time.Second
	quietReport = time.Minute
)

// driveTarget is one registered project as --pause --all and --resume --all
// reach it: by its lease, which needs no config that loads.
type driveTarget struct {
	project, root, path string
}

// registeredTargets is every project in the per-user registry, one per lease:
// two names registered over one checkout are one drive. A root that is gone or
// holds no config has no drive to reach, so it is no target. A worktree of
// another registered project answers to that project: its name, and its
// policy through supervisedHere. It keeps its own lease, because a git
// worktree has a git dir of its own, and a drive running there must still
// take the pause.
func registeredTargets() ([]driveTarget, string, error) {
	reg := projects.Path(config.GlobalPath())
	rows, err := projects.Read(reg)
	if err != nil {
		return nil, reg, err
	}
	live, parents, _ := hostProjects(rows)
	byRoot := map[string]projects.Row{}
	for _, r := range live {
		byRoot[r.Root] = r
	}
	var out []driveTarget
	seen := map[string]bool{}
	for _, r := range live {
		path, err := (&session{cfg: &config.Config{Root: r.Root}}).leasePath()
		if err != nil {
			return nil, reg, fmt.Errorf("%s: %w", r.Project, err)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		t := driveTarget{project: r.Project, root: r.Root, path: path}
		if p, ok := parents[r.Root]; ok {
			t.project, t.root = byRoot[p].Project+" worktree", p
		}
		out = append(out, t)
	}
	return out, reg, nil
}

// supervisedHere refuses a host-wide verb in a project whose own policy keeps
// it from the caller: off for anyone pausing, and below supervisor for a
// supervisor. The gate already judged the project the caller stands in; this
// judges each project the verb reaches. A config that will not load refuses a
// supervisor, which cannot be shown the policy, and lets the operator through,
// because a pause is how an operator gets to fix one.
func supervisedHere(root string, pausing bool) error {
	cfg, err := config.LoadAt(root)
	if err != nil {
		if runner.Supervising() {
			return fmt.Errorf("its config would not load, so its pause_for_config_change policy is unknown: %v", err)
		}
		return nil
	}
	p, _ := cfg.Policy("pause_for_config_change")
	switch {
	case pausing && p == polOff:
		return errors.New("[supervise] pause_for_config_change is off there")
	case runner.Supervising() && !supervisorMay(p):
		return fmt.Errorf("[supervise] pause_for_config_change is %q there, so the operator owns it", p)
	}
	return nil
}

// eachTarget runs fn over every target at once and prints the line each
// returns in registry order, its project's name padded to a column. A drive
// answers within one of its polls, so one at a time would cost the host the
// sum of them. It reports how many came back failed.
func eachTarget(w io.Writer, targets []driveTarget, fn func(int, driveTarget) (string, bool)) int {
	lines := make([]string, len(targets))
	failed := make([]bool, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { lines[i], failed[i] = fn(i, t) })
	}
	wg.Wait()
	width := 0
	for _, t := range targets {
		width = max(width, len(t.project))
	}
	n := 0
	for i, t := range targets {
		fmt.Fprintf(w, "%-*s  %s\n", width, t.project, lines[i])
		if failed[i] {
			n++
		}
	}
	return n
}

// pauseAll is --pause --all: --pause in every registered project at once, then
// --wait over the drives that took it.
func pauseAll(ctx context.Context, w io.Writer) error {
	targets, reg, err := registeredTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintf(w, "no projects registered (%s), so no drive to pause\n", reg)
		return nil
	}
	took := make([]bool, len(targets))
	failed := eachTarget(w, targets, func(i int, t driveTarget) (string, bool) {
		if err := supervisedHere(t.root, true); err != nil {
			if _, busy, _ := lease.Read(t.path); !busy {
				return "no drive running", false
			}
			return "could not be paused: " + err.Error(), true
		}
		l, err := controlAt(t.path, ctrlPause)
		switch {
		case errors.Is(err, errNoDrive):
			return "no drive running", false
		case err != nil:
			return "could not be paused: " + err.Error(), true
		}
		took[i] = true
		return pausedLine(l), false
	})
	if failed > 0 {
		return Refusal{fmt.Errorf("%d of the registered projects' drives did not take the pause; the rest are paused", failed)}
	}
	var paused []driveTarget
	for i, t := range targets {
		if took[i] {
			paused = append(paused, t)
		}
	}
	if !driveWait || len(paused) == 0 {
		return nil
	}
	return waitQuiet(ctx, w, paused)
}

// resumeAll is --resume --all: --resume in every registered project whose
// drive is paused, each checked against its own config first. One that will
// not load stays paused and is named, and the rest resume.
func resumeAll(w io.Writer) error {
	targets, reg, err := registeredTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintf(w, "no projects registered (%s), so no drive to resume\n", reg)
		return nil
	}
	failed := eachTarget(w, targets, func(_ int, t driveTarget) (string, bool) {
		switch l, busy, err := lease.Read(t.path); {
		case err != nil:
			return "could not be read: " + err.Error(), true
		case !busy || l.Command != "drive":
			return "no drive running", false
		case !l.Paused:
			return driveName(l) + " is not paused", false
		}
		if err := supervisedHere(t.root, false); err != nil {
			return "stays paused: " + err.Error(), true
		}
		cfg, err := config.LoadAt(t.root)
		if err == nil {
			err = validateTemplates(cfg)
		}
		if err != nil {
			return "stays paused: the config or a template would not load: " + err.Error(), true
		}
		l, err := controlAt(t.path, resumeVerb())
		if err != nil {
			return "stays paused: " + err.Error(), true
		}
		return resumedLine(l), false
	})
	if failed > 0 {
		return Refusal{fmt.Errorf("%d paused drive(s) did not resume; each is named above and stays paused", failed)}
	}
	return nil
}

// busyItems is what a paused drive still has inside a stage: each running id
// that holds at no boundary.
func busyItems(l lease.Lease) []string {
	var out []string
	for _, id := range l.Running {
		if !slices.ContainsFunc(l.Holds, func(h lease.Hold) bool { return h.ID == id }) {
			out = append(out, id)
		}
	}
	return out
}

// waitQuiet is --wait: it returns once no drive in targets has an item in a
// session or a trunk stage, and refuses at --timeout naming each one that
// still does. A drive that exited is quiet. Nothing here resumes anything, so
// an interrupt leaves every drive paused.
func waitQuiet(ctx context.Context, w io.Writer, targets []driveTarget) error {
	deadline := time.Now().Add(driveTimeout)
	var said time.Time
	for {
		var busy []string
		for _, t := range targets {
			l, live, err := lease.Read(t.path)
			switch {
			case err != nil:
				busy = append(busy, fmt.Sprintf("%s: the lease would not read: %v", t.project, err))
			case !live || l.Command != "drive":
			default:
				if ids := busyItems(l); len(ids) > 0 {
					busy = append(busy, fmt.Sprintf("%s %s: %s", t.project, driveName(l), strings.Join(ids, " ")))
				}
			}
		}
		if len(busy) == 0 {
			fmt.Fprintln(w, "quiet: no paused drive has an item in a session or a trunk stage")
			return nil
		}
		if time.Now().After(deadline) {
			return Refusal{fmt.Errorf("still busy after --timeout %s, and every drive stays paused:\n  %s",
				driveTimeout, strings.Join(busy, "\n  "))}
		}
		if said.IsZero() || time.Since(said) >= quietReport {
			fmt.Fprintf(w, "waiting on:\n  %s\n", strings.Join(busy, "\n  "))
			said = time.Now()
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "· interrupted; every drive --pause paused stays paused")
			return ctx.Err()
		case <-time.After(quietPoll):
		}
	}
}
