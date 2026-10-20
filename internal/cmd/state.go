package cmd

import (
	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/runlog"
)

// Work states, in the order an item moves through them.
const (
	stateBroken   = "broken"      // the body will not parse
	stateScaffold = "scaffold"    // intaken, requirements not yet written
	stateBlocked  = "blocked"     // a dependency has not merged
	stateReady    = "ready"       // deps met, nothing has run
	stateRan      = "implemented" // executed clean, awaiting the review and the pull request that follows it
	stateDone     = "merged"      // the ledger records it merged
)

// workState is the one classifier over the states an item moves through, and
// the missing deps behind a blocked one.
//
// status renders it and drive selects on it. It was inline in status's row
// builder until a second caller needed it, and a second classifier over the
// same states is how check came to call a dep shelved while run called the same
// dep merged. A loop asking the question itself would have reimplemented
// "ready" as "not done and no log", which is the same answer until a scaffold
// or an unparseable body reaches the queue.
//
// logDir is a parameter because the caller classifies a whole backlog and
// cfg.LogDir() resolves the same string every time.
func (s *session) workState(it backlog.Item, logDir string) (state string, missing []string) {
	switch {
	case s.ledger.State(it.ID.Bare()).Merged:
		return stateDone, nil
	case s.isScaffold(it):
		return stateScaffold, nil
	}
	missing = backlog.MissingDeps(it, s.done)
	switch {
	case len(missing) > 0:
		return stateBlocked, missing
	case runlog.Ran(logDir, s.cfg.Paths.LogPrefix+runlog.Base(it.ID.Stem(), it.Slug)):
		return stateRan, nil
	}
	return stateReady, nil
}

// queue is every item a drive would attempt with no ids named: ready, in
// backlog order.
//
// An item a drive parked has a clean log behind it and so reads as `ran`, which
// keeps it out of here. That is the wanted answer rather than an oversight: a
// parked item stopped for a reason somebody has to read, and naming it
// positionally is how it resumes.
func (s *session) queue() []backlog.Item {
	logDir := s.cfg.LogDir()
	var out []backlog.Item
	for _, it := range s.backlog.Items {
		if state, _ := s.workState(it, logDir); state == stateReady {
			out = append(out, it)
		}
	}
	return out
}
