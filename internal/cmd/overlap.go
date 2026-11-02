package cmd

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
)

// branchOverlap is one unmerged item whose branch already changes files
// another item cites, with no deps edge between the two in either direction.
// Two such items run and merge in whatever order the queue reaches them, and
// the second rewrites what the first was written against: E86 cited a
// function B101's branch was rewriting, merged first, and B101's merge then
// conflicted with it in code and in meaning.
type branchOverlap struct {
	Other  string   `json:"other"`
	Branch string   `json:"branch"`
	Paths  []string `json:"paths"`
}

// ordered reports whether either item reaches the other through deps, at any
// depth, so one is already scheduled behind the other.
func (s *session) ordered(a, b string) bool {
	g := s.graph()
	return slices.Contains(g.Dependents(a), b) || slices.Contains(g.Dependents(b), a)
}

// branchChanges is the files an item's branch changes against the trunk, and
// ok false with the reason when the branch is there and cannot be read. A
// branch nothing has created, or one holding no commit past the trunk, changes
// nothing.
func (s *session) branchChanges(it backlog.Item) (paths []string, ok bool, why string) {
	branch := s.branchRef(it)
	if branch == "" {
		return nil, true, ""
	}
	trunk := s.driftRef()
	n, err := commitsPast(s.cfg.Root, trunk, branch)
	if err != nil {
		return nil, false, err.Error()
	}
	if n == 0 {
		return nil, true, ""
	}
	paths, err = gitNames(s.cfg.Root, "diff", "-z", "--name-only", trunk+"..."+branch)
	if err != nil {
		return nil, false, err.Error()
	}
	return paths, true, ""
}

// branchOverlaps is every unmerged item other than it whose branch changes a
// file it cites, with no deps edge ordering the two. A branch that will not
// read is written to stderr by name and compared as changing nothing: a git
// read that fails is not evidence of an overlap.
func (s *session) branchOverlaps(it backlog.Item) []branchOverlap {
	if !s.cfg.Git.On() || !s.resolves(s.driftRef()) {
		return nil
	}
	cited := citedPaths(it.Body)
	if len(cited) == 0 {
		return nil
	}
	var out []branchOverlap
	for _, other := range s.backlog.Items {
		id := other.ID.Bare()
		if id == it.ID.Bare() || s.done(id) || s.ordered(it.ID.Bare(), id) {
			continue
		}
		changed, ok, why := s.branchChanges(other)
		if !ok {
			fmt.Fprintf(errOut, "warning: %s's branch did not read, so it is not compared with %s: %s\n", id, it.ID.Bare(), why)
			continue
		}
		var hit []string
		for _, p := range cited {
			if slices.Contains(changed, p) {
				hit = append(hit, p)
			}
		}
		if len(hit) > 0 {
			out = append(out, branchOverlap{Other: id, Branch: s.branchRef(other), Paths: hit})
		}
	}
	return out
}

// overlapRefusal is accept's refusal for branchOverlaps, naming the two deps
// edits that would order the items.
func overlapRefusal(id string, found []branchOverlap) error {
	var b strings.Builder
	for _, o := range found {
		fmt.Fprintf(&b, "%s's branch %s changes %s, which %s cites, and neither names the other on its deps line\n",
			o.Other, o.Branch, strings.Join(o.Paths, ", "), id)
		fmt.Fprintf(&b, "  to run %s first: add %s to %s's deps line; to run %s first: add %s to %s's\n",
			o.Other, o.Other, id, id, id, o.Other)
	}
	return Refusal{fmt.Errorf("%sNothing was accepted. Order the two with one of the edits above, or accept anyway: drover accept %s --yes",
		b.String(), id)}
}

// rangeOverlap is two unmerged items citing overlapping lines of one file:
// a hint that they may be one change written as two.
type rangeOverlap struct {
	A        string   `json:"a"`
	B        string   `json:"b"`
	Cites    []string `json:"cites"`
	Combined int      `json:"combined_requirements"`
	Ordered  bool     `json:"ordered"`
}

func (r rangeOverlap) String() string {
	order := "no deps edge orders them"
	if r.Ordered {
		order = "a deps edge already orders them"
	}
	return fmt.Sprintf("%s and %s cite overlapping lines (%s); %d requirements together; %s",
		r.A, r.B, strings.Join(r.Cites, ", "), r.Combined, order)
}

// rangeOverlaps is each pair of unmerged items whose cited line ranges overlap
// in one file. A file cited by more than half of the unmerged items is left
// out: a file nearly every item touches says nothing about which two belong
// together. Advisory only, so nothing that reads it changes an exit code.
func (s *session) rangeOverlaps() []rangeOverlap {
	type cite struct {
		id  string
		ref citedRef
	}
	var open []backlog.Item
	for _, it := range s.backlog.Items {
		if !s.done(it.ID.Bare()) {
			open = append(open, it)
		}
	}
	byFile := map[string][]cite{}
	for _, it := range open {
		for _, r := range citedRefs(it.Body) {
			byFile[r.Path] = append(byFile[r.Path], cite{it.ID.Bare(), r})
		}
	}
	pairs := map[[2]string]*rangeOverlap{}
	for _, cs := range byFile {
		items := map[string]bool{}
		for _, c := range cs {
			items[c.id] = true
		}
		if len(items) < 2 || 2*len(items) > len(open) {
			continue
		}
		for i, x := range cs {
			for _, y := range cs[i+1:] {
				if x.id == y.id || x.ref.End < y.ref.Start || y.ref.End < x.ref.Start {
					continue
				}
				k := [2]string{x.id, y.id}
				if k[1] < k[0] {
					k[0], k[1] = k[1], k[0]
				}
				p := pairs[k]
				if p == nil {
					p = &rangeOverlap{A: k[0], B: k[1], Ordered: s.ordered(k[0], k[1])}
					pairs[k] = p
				}
				for _, t := range []string{x.ref.Text, y.ref.Text} {
					if !slices.Contains(p.Cites, t) {
						p.Cites = append(p.Cites, t)
					}
				}
			}
		}
	}
	counts := map[string]int{}
	for _, it := range open {
		counts[it.ID.Bare()] = len(it.Requirements())
	}
	out := make([]rangeOverlap, 0, len(pairs))
	for _, p := range pairs {
		sort.Strings(p.Cites)
		p.Combined = counts[p.A] + counts[p.B]
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}
