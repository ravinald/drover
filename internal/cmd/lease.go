package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/projects"
)

// held is this process's lease, when it took one.
//
// Package state because the two commands that share a process share a tree:
// `drive` leases the whole queue at its RunE and then invokes finish's own RunE
// to reconcile what merged. flock lives on the open file description, so that
// finish opening the lease file a second time is refused exactly as a second
// terminal would be, and only this variable tells the two apart.
var held *lease.Handle

// takeLease claims the checkout for the length of the command, and returns the
// release. command names the holder in a refusal, item the id it is working,
// empty for a lease taken over a queue.
//
// The release is a function rather than the handle so that a caller defers one
// thing and cannot release a lease some other frame took.
func (s *session) takeLease(command, item string) (func(), error) {
	if held != nil {
		return func() {}, nil
	}
	path, err := s.leasePath()
	if err != nil {
		return nil, err
	}
	branch := ""
	if s.cfg.Git.On() {
		branch, _ = s.currentBranch()
	}
	h, err := lease.Acquire(path, lease.Lease{
		Command: command,
		Item:    item,
		Branch:  branch,
		Root:    s.cfg.Root,
	})
	if err != nil {
		return nil, leaseRefusal(err)
	}
	held = h
	return func() {
		_ = h.Release()
		held = nil
	}, nil
}

// leaseFree refuses when another live process is working in this checkout.
//
// It is the half of the rule for a command that takes no lease of its own:
// `finish` and `open-pr`, which run in seconds and need to know whether the
// branch they switch or gate belongs to a session in this tree. `run`, `draft`
// and `review` work here, so either refuses while one of them holds it.
//
// A `drive` holds the checkout too, and is let through: its items run in
// worktrees of their own, so what it does here is move the trunk ref, push, and
// add and remove worktrees. The trunk move is under trunkLock and the worktree
// removal under itemTreeFree. What the drive's lease still refuses is a second
// drive, which would run the same queue twice, and that is an acquire.
func (s *session) leaseFree() error {
	if held != nil {
		return nil
	}
	path, err := s.leasePath()
	if err != nil {
		return err
	}
	l, busy, err := lease.Read(path)
	if err != nil {
		return err
	}
	if !busy || l.Command == "drive" {
		return nil
	}
	return leaseRefusal(lease.Held{Lease: l, Payload: l.PID != 0})
}

// itemTreeFree refuses when another live process holds the lease on an item's
// worktree, naming it and the tree: finish removes that tree, and a session is
// working in it. The holder being this process is the drive finishing its own
// item, whose tree lease is still open.
func (s *session) itemTreeFree(it backlog.Item) error {
	path, err := s.treePath(it)
	if err != nil {
		return err
	}
	if _, registered, err := s.treeBranch(path); err != nil || !registered {
		return err
	}
	l, busy, err := lease.Read(filepath.Join(gitDirOf(path), lease.FileName))
	if err != nil || !busy || l.PID == os.Getpid() {
		return err
	}
	return Refusal{fmt.Errorf("%s's worktree at %s is in use, and finishing removes it: %w",
		it.ID.Bare(), path, lease.Held{Lease: l, Payload: l.PID != 0})}
}

// trunkWait bounds how long a trunk fast-forward waits for another one. The
// holder is a fetch and a ref update, so a wait past this is a process stuck
// on the network rather than one about to finish.
var trunkWait = 30 * time.Second

// trunkLock serializes the commands that move the local trunk ref: a drive
// bringing it up to date before it branches an item, and a finish bringing the
// operator's checkout level after a merge. Both read where the ref is and then
// move it, and two of them interleaved make one of them fail. It waits rather
// than refusing, because the holder finishes in seconds.
func (s *session) trunkLock(command string) (func(), error) {
	// Resolved here rather than with --path-format=absolute, which git before
	// 2.31 rejects: git answers relative to the directory it ran in.
	out, err := gitcmd.Command(s.cfg.Root, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("find the git dir for the trunk lock: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.cfg.Root, dir)
	}
	path := filepath.Join(dir, lease.TrunkFileName)
	h, err := lease.AcquireWithin(path, lease.Lease{Command: command, Root: s.cfg.Root}, trunkWait)
	if held, ok := errors.AsType[lease.Held](err); ok {
		who := "another drover process"
		if held.Payload {
			who = fmt.Sprintf("drover %s (pid %d)", held.Lease.Command, held.Lease.PID)
		}
		return nil, fmt.Errorf("%s held the trunk for longer than %s", who, trunkWait)
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = h.Release() }, nil
}

// leaseHolder answers "who else is here" and refuses nothing. status is the
// only caller: it is read-only, and the question it exists to answer now
// includes the other process in the tree.
func (s *session) leaseHolder() (lease.Lease, bool, error) {
	if held != nil {
		return held.Current(), true, nil
	}
	path, err := s.leasePath()
	if err != nil {
		return lease.Lease{}, false, err
	}
	return lease.Read(path)
}

func (s *session) leasePath() (string, error) {
	state := ""
	if reg := projects.Path(config.GlobalPath()); reg != "" {
		state = filepath.Dir(reg)
	}
	return lease.Path(s.cfg.Root, state)
}

// leaseRefusal maps a busy tree to exit 3. It is a refusal rather than an error
// for the reason every exit 3 is one: drover worked, the tree and the ledger
// are untouched, and what it is reporting is about this checkout rather than
// about drover.
//
// There is deliberately no --force and no --anyway. The kernel releases the
// lock however the holder dies, so a lease nobody holds is already acquirable
// and there is no stale state an override could rescue; the only thing one
// could do is admit a second command to a tree a live one is writing, which is
// the collision the lease exists to stop.
func leaseRefusal(err error) error {
	if h, ok := errors.AsType[lease.Held](err); ok {
		return Refusal{h}
	}
	return err
}
