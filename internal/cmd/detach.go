package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
)

var (
	// driveDetach is --detach.
	driveDetach bool
	// detachWait bounds how long --detach waits for the drive it started to
	// hold the lease. The preflight before that runs each runner's sign-in
	// check, bounded at authWait apiece.
	detachWait = 5 * time.Minute
)

// detachDrive is --detach: the same drive, over the same arguments less
// --detach, in a session of its own with no terminal, so the process that
// launched it can exit, or be restarted, under it. It returns once the drive
// holds the lease and has its id, or with the drive's own error and code when
// it exits before that.
//
// The drive is this binary at the path it was started from, as a re-exec is,
// so a later --resume compares against the file the operator installed.
func detachDrive(ctx context.Context, w io.Writer) error {
	if err := noNestedDrive(); err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return err
	}
	leasePath, err := (&session{cfg: cfg}).leasePath()
	if err != nil {
		return err
	}
	bin, err := launchedBinary()
	if err != nil {
		return fmt.Errorf("--detach starts this binary again and cannot find it: %w", err)
	}
	dir, err := driveFilesDir(cfg.Project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	logPath := filepath.Join(dir, fmt.Sprintf("detach-%d-%d.log", os.Getpid(), time.Now().UnixNano()))
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- under the per-user state directory
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = null.Close() }()

	c := exec.Command(bin.path, withoutDetach(os.Args[1:])...) // #nosec G204 -- this binary, over its own arguments
	c.Args[0] = os.Args[0]
	c.Stdin, c.Stdout, c.Stderr = null, logf, logf
	if err := newSession(c); err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("--detach: the drive would not start: %w", err)
	}
	exited := make(chan int, 1)
	go func() {
		_ = c.Wait()
		exited <- c.ProcessState.ExitCode()
	}()

	pid := c.Process.Pid
	deadline := time.Now().Add(detachWait)
	for {
		if l, busy, _ := lease.Read(leasePath); busy && l.PID == pid && l.Drive != "" {
			return detached(w, cfg, l, logPath)
		}
		select {
		case code := <-exited:
			return detachFailed(w, code, logPath)
		case <-ctx.Done():
			fmt.Fprintf(w, "interrupted while drive pid %d started; it runs on regardless, and its log is %s\n", pid, logPath)
			return nil
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drive pid %d has not taken the lease after %s and may still be starting; its log is %s", pid, detachWait, logPath)
		}
	}
}

// withoutDetach is args less --detach, so the drive it starts runs rather than
// detaching again.
func withoutDetach(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--detach" || a == "--detach=true" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// detached reports a drive that took the lease, its log renamed to its id.
func detached(w io.Writer, cfg *config.Config, l lease.Lease, logPath string) error {
	events, err := driveEventsPath(cfg.Project, l.Drive)
	if err != nil {
		return err
	}
	named := strings.TrimSuffix(events, ".jsonl") + ".log"
	if os.Rename(logPath, named) == nil {
		logPath = named
	}
	fmt.Fprintf(w, "detached %s\n  log:    %s\n  events: %s\n", driveName(l), logPath, events)
	fmt.Fprintf(w, "Follow it with drover attach %s, wait on it with drover watch %s --until <event,...>, stop it with drover drive --stop\n", l.Drive, l.Drive)
	return nil
}

// detachFailed is a drive that exited before it held the lease: what it
// printed, and its exit code as this process's own.
func detachFailed(w io.Writer, code int, logPath string) error {
	raw, _ := os.ReadFile(logPath) // #nosec G304 -- the log this process opened
	out := strings.TrimSpace(string(raw))
	if code == 0 {
		if out != "" {
			fmt.Fprintln(w, out)
		}
		fmt.Fprintf(w, "the drive exited 0 before it held the lease; its log is %s\n", logPath)
		return nil
	}
	msg := fmt.Errorf("the detached drive exited %d before it held the lease; its log is %s:\n%s", code, logPath, lastLines(out, 20))
	if code == ExitRefused {
		return Refusal{msg}
	}
	return msg
}
