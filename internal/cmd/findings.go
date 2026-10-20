package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

// The two ways a session files a finding: a follow-up through the tracker, and
// a scaffold through `drover new` or `split`.
const (
	findingRegister = "follow-up"
	findingScaffold = "scaffold"
)

// The coverage values: whether anything will carry a filed finding, judged when
// it is filed and again on every read that shows one. Nothing stores them: an
// owner that merges after the follow-up was judged queued orphans it, and the
// next read has to say so.
const (
	coverageQueued   = "queued"   // an owner is in this drive's queue
	coverageUnqueued = "unqueued" // an owner is ready or ran, and is not queued
	coverageBlocked  = "blocked"  // every unmerged owner is blocked
	coverageOrphaned = "orphaned" // every owner has merged
	coverageUnowned  = "unowned"  // no owner link
	coverageScaffold = "scaffold" // a scaffold whose requirements are unwritten
)

var coverages = []string{coverageQueued, coverageUnqueued, coverageBlocked, coverageOrphaned, coverageUnowned, coverageScaffold}

// FiledFinding is one thing a session filed, and whether anything will carry it.
//
// Ref is the issue number for a follow-up and the item id for a scaffold.
// BlockedOn maps each unmerged owner to what holds it: the deps it waits on, or
// the state that keeps it from running (scaffold, broken, or "not in the
// backlog" for an owner no body answers to). FiledBy is every item whose
// sessions were running when the row appeared, and Ambiguous says there was
// more than one.
type FiledFinding struct {
	Ref       string              `json:"ref"`
	Kind      string              `json:"kind"`
	Title     string              `json:"title,omitempty"`
	Owners    []string            `json:"owners"`
	Coverage  string              `json:"coverage"`
	BlockedOn map[string][]string `json:"blocked_on,omitempty"`
	Ambiguous bool                `json:"ambiguous,omitempty"`
	FiledBy   []string            `json:"filed_by,omitempty"`
}

// uncovered is a finding nothing is scheduled to carry: a register row whose
// owners cannot run, have all merged, or do not exist, and every scaffold.
func (f FiledFinding) uncovered() bool {
	switch f.Coverage {
	case coverageBlocked, coverageOrphaned, coverageUnowned, coverageScaffold:
		return true
	}
	return false
}

// DriveTotals is the findings a drive saw, by result, and those needing a
// person. It is the drive's last --json object, and rides on the exit event
// under --events.
type DriveTotals struct {
	FindingsByCoverage    map[string]int `json:"findings_by_coverage"`
	FindingsNeedingTriage []FiledFinding `json:"findings_needing_triage"`
}

func driveTotals(all []FiledFinding) DriveTotals {
	t := DriveTotals{FindingsByCoverage: make(map[string]int, len(coverages)), FindingsNeedingTriage: []FiledFinding{}}
	for _, v := range coverages {
		t.FindingsByCoverage[v] = 0
	}
	for _, f := range all {
		t.FindingsByCoverage[f.Coverage]++
		if f.uncovered() {
			t.FindingsNeedingTriage = append(t.FindingsNeedingTriage, f)
		}
	}
	return t
}

// ownerCoverage judges a register row's owners against the ledger, the backlog
// and queued, the ids a drive holds waiting or running. Precedence runs from
// what is certain to what is not: no owner, every owner merged, an owner this
// drive will reach, an owner that could run, and otherwise blocked.
func (s *session) ownerCoverage(owners []string, queued map[string]bool) (string, map[string][]string) {
	if len(owners) == 0 {
		return coverageUnowned, nil
	}
	if s.orphaned(owners) {
		return coverageOrphaned, nil
	}
	logDir := s.cfg.LogDir()
	blocked := map[string][]string{}
	open := false
	for _, id := range owners {
		if s.done(id) {
			continue
		}
		if queued[id] {
			return coverageQueued, nil
		}
		i := slices.IndexFunc(s.backlog.Items, func(it backlog.Item) bool { return it.ID.Bare() == id })
		if i < 0 {
			blocked[id] = []string{"not in the backlog"}
			continue
		}
		switch state, missing := s.workState(s.backlog.Items[i], logDir); state {
		case stateReady, stateRan:
			open = true
		case stateBlocked:
			blocked[id] = missing
		default:
			blocked[id] = []string{state}
		}
	}
	if open {
		return coverageUnqueued, nil
	}
	return coverageBlocked, blocked
}

func (s *session) registerFinding(is gh.Issue, filedBy []string, queued map[string]bool) FiledFinding {
	owners := is.Owners
	if owners == nil {
		owners = []string{}
	}
	v, on := s.ownerCoverage(is.Owners, queued)
	return FiledFinding{Ref: strconv.Itoa(is.Number), Kind: findingRegister, Title: is.Title, Owners: owners,
		Coverage: v, BlockedOn: on, FiledBy: filedBy, Ambiguous: len(filedBy) > 1}
}

// scaffoldFinding is a scaffold, attributed to the item its provenance line
// names and to none when it carries no such line.
func (s *session) scaffoldFinding(it backlog.Item) FiledFinding {
	f := FiledFinding{Ref: it.ID.Bare(), Kind: findingScaffold, Title: it.Title(), Owners: []string{}, Coverage: coverageScaffold}
	if by := foundIn(it.Body, s.cfg.Intake.StopMarker); by != "" {
		f.FiledBy = []string{by}
	}
	return f
}

// findingsNeedingTriage is every open register row nothing will carry and every
// scaffold, judged now. queued is the running drive's queue, empty with none.
func (s *session) findingsNeedingTriage(index gh.Index, queued map[string]bool) []FiledFinding {
	out := []FiledFinding{}
	for _, is := range index.Issues {
		if f := s.registerFinding(is, nil, queued); f.uncovered() {
			out = append(out, f)
		}
	}
	logDir := s.cfg.LogDir()
	for _, it := range s.backlog.Items {
		if state, _ := s.workState(it, logDir); state == stateScaffold {
			out = append(out, s.scaffoldFinding(it))
		}
	}
	return out
}

// heldQueue is the ids the drive holding this checkout has waiting or running,
// empty when no drive holds it, so a read outside the drive judges queued the
// way the drive itself does.
func (s *session) heldQueue() map[string]bool {
	out := map[string]bool{}
	l, busy, err := s.leaseHolder()
	if err != nil || !busy || l.Command != "drive" {
		return out
	}
	for _, id := range append(slices.Clone(l.Queue), l.Running...) {
		out[id] = true
	}
	return out
}

// findingWatch is what a drive knows about the findings its sessions file.
//
// The register is read at each dispatch and again at each result, and a row
// open at the result but not at the dispatch appeared while that item ran. A
// row is attributed once, to every item whose window it fell in: under --jobs
// the register carries no author, so two items running when it appeared are
// both named and the finding says it is ambiguous. carry holds the findings of
// items still running, for their own report when they finish, and known is the
// backlog as of the last result, which a new scaffold is the difference from.
//
// All of it crosses a re-exec in the handoff. A carried item's window opened in
// the process before, and one reopened in the process after would put every
// row it filed in between into the baseline.
type findingWatch struct {
	off      bool
	windows  map[string]map[int]bool
	failed   map[string]string
	reported map[int]bool
	carry    map[string][]FiledFinding
	all      []FiledFinding
	known    map[string]bool
}

func (s *session) newFindingWatch() *findingWatch {
	return &findingWatch{
		off:      s.cfg.RegisterBackend() == config.BackendNone,
		windows:  map[string]map[int]bool{},
		failed:   map[string]string{},
		reported: map[int]bool{},
		carry:    map[string][]FiledFinding{},
		known:    s.backlogIDs(),
	}
}

// watchState is a findingWatch as the handoff carries it. A window is the
// register numbers open at its item's dispatch; a failed one is absent from
// Windows and named in Failed.
type watchState struct {
	Windows  map[string][]int          `json:"windows,omitempty"`
	Failed   map[string]string         `json:"failed,omitempty"`
	Reported []int                     `json:"reported,omitempty"`
	Carry    map[string][]FiledFinding `json:"carry,omitempty"`
	All      []FiledFinding            `json:"all,omitempty"`
	Known    []string                  `json:"known,omitempty"`
}

func (w *findingWatch) state() *watchState {
	st := &watchState{Windows: map[string][]int{}, Failed: maps.Clone(w.failed), Carry: w.carry, All: w.all,
		Reported: slices.Sorted(maps.Keys(w.reported)), Known: slices.Sorted(maps.Keys(w.known))}
	for id, open := range w.windows {
		if open != nil {
			st.Windows[id] = slices.Sorted(maps.Keys(open))
		}
	}
	return st
}

// adoptFindingWatch is the watch a re-exec carried over, or a new one when the
// process before wrote none.
func (s *session) adoptFindingWatch(st *watchState) *findingWatch {
	w := s.newFindingWatch()
	if st == nil {
		return w
	}
	for id, nums := range st.Windows {
		open := make(map[int]bool, len(nums))
		for _, n := range nums {
			open[n] = true
		}
		w.windows[id] = open
	}
	for id, why := range st.Failed {
		w.windows[id], w.failed[id] = nil, why
	}
	for _, n := range st.Reported {
		w.reported[n] = true
	}
	maps.Copy(w.carry, st.Carry)
	w.all = st.All
	if st.Known != nil {
		w.known = make(map[string]bool, len(st.Known))
		for _, id := range st.Known {
			w.known[id] = true
		}
	}
	return w
}

// read is the register as the watch reads it, kept on the running drive when
// it reads, for the drive's own render.
func (w *findingWatch) read(s *session) gh.Index {
	index := s.register()
	if index.OK {
		activeDrive.Load().sawRegister(index)
	}
	return index
}

// dispatched opens id's window, unless a re-exec carried it open.
func (w *findingWatch) dispatched(s *session, id string) {
	if _, carried := w.windows[id]; w.off || carried {
		return
	}
	index := w.read(s)
	if !index.OK {
		w.windows[id] = nil
		w.failed[id] = fmt.Sprintf("the follow-up list would not read at dispatch (%v), so no row filed while %s ran is attributed to it", index.Reason(), id)
		return
	}
	open := make(map[int]bool, len(index.Issues))
	for _, is := range index.Issues {
		open[is.Number] = true
	}
	w.windows[id] = open
}

// finished closes d's window and the backlog diff since known, records each
// finding on the report of every item it is attributed to, and emits it. A
// register read that fails is reported on d and changes nothing else about it.
func (w *findingWatch) finished(s *session, d *DriveItem, running map[int]queued, queued map[string]bool) {
	d.Findings = append(d.Findings, w.carry[d.ID]...)
	delete(w.carry, d.ID)
	attach := func(f FiledFinding) {
		w.all = append(w.all, f)
		for _, id := range f.FiledBy {
			if id == d.ID {
				d.Findings = append(d.Findings, f)
				continue
			}
			for _, q := range running {
				if q.it.ID.Bare() == id {
					w.carry[id] = append(w.carry[id], f)
				}
			}
		}
		events.filed(f)
	}

	if !w.off {
		window, opened := w.windows[d.ID]
		delete(w.windows, d.ID)
		index := w.read(s)
		switch {
		case w.failed[d.ID] != "":
			d.RegisterError = w.failed[d.ID]
		case !index.OK:
			d.RegisterError = fmt.Sprintf("the follow-up list would not read after %s finished (%v), so no row it filed is reported", d.ID, index.Reason())
		case opened:
			for _, is := range index.Issues {
				if window[is.Number] || w.reported[is.Number] {
					continue
				}
				w.reported[is.Number] = true
				by := []string{d.ID}
				for _, q := range running {
					other := q.it.ID.Bare()
					if ow, ok := w.windows[other]; ok && ow != nil && !ow[is.Number] {
						by = append(by, other)
					}
				}
				slices.Sort(by)
				attach(s.registerFinding(is, by, queued))
			}
		}
		delete(w.failed, d.ID)
	}

	now := s.backlogIDs()
	logDir := s.cfg.LogDir()
	for _, it := range s.backlog.Items {
		if w.known[it.ID.Bare()] {
			continue
		}
		if state, _ := s.workState(it, logDir); state == stateScaffold {
			attach(s.scaffoldFinding(it))
		} else {
			events.emit(DriveEvent{Event: evFiled, ID: it.ID.Bare()})
		}
	}
	w.known = now
}

// findingLine is one finding as the drive's summary and `follow-ups --coverage` name
// it: the ref, its owners, the result and what blocks it.
func findingLine(f FiledFinding) string {
	ref := f.Ref
	if f.Kind == findingRegister {
		ref = "#" + ref
	}
	line := fmt.Sprintf("%s %s %s", f.Kind, ref, f.Coverage)
	if len(f.Owners) > 0 {
		line += " (owners " + strings.Join(f.Owners, " ") + ")"
	}
	line += blockedClause(f.BlockedOn)
	if len(f.FiledBy) > 0 {
		line += ", filed by " + strings.Join(f.FiledBy, " ")
		if f.Ambiguous {
			line += " (ambiguous)"
		}
	}
	return line
}

// blockedClause names what each blocked owner waits on, in owner order, as
// "; <id> waits on <what>" per owner.
func blockedClause(on map[string][]string) string {
	out := ""
	for _, id := range slices.Sorted(maps.Keys(on)) {
		out += fmt.Sprintf("; %s waits on %s", id, strings.Join(on[id], " "))
	}
	return out
}
