package cmd

import (
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
)

// seqFile is one file a [numbered_files] glob numbers: its path from the repository
// root, the digits as written, and their value.
type seqFile struct {
	path   string
	digits string
	n      uint64
}

// seqProblem is a file the branch added that cannot keep its number, what it
// collides with, and the number it has to take instead.
type seqProblem struct {
	file seqFile
	// against is the file sharing its number, the trunk's highest when it sits
	// at or below that, or the branch's own file it has to stay after.
	against seqFile
	reason  string
	use     string
}

// seqReport is one [numbered_files] glob judged against one trunk commit.
type seqReport struct {
	name     string
	high     *seqFile
	problems []seqProblem
	// trunkClashes are groups of the trunk's own files sharing a number: no
	// branch put them there, so no branch can be sent back to fix them.
	trunkClashes [][]seqFile
}

// sequenceNumber reads the number glob puts at its placeholder in name. The
// placeholder matches a whole run of digits, never part of one, so 0001120 is
// never read as 112 with a digit either side.
func sequenceNumber(glob, name string) (seqFile, bool) {
	before, after := config.SequenceGlob(glob)
	for i := 0; i < len(name); i++ {
		if !isDigit(name[i]) || (i > 0 && isDigit(name[i-1])) {
			continue
		}
		j := i
		for j < len(name) && isDigit(name[j]) {
			j++
		}
		pre, _ := path.Match(before, name[:i])
		post, _ := path.Match(after, name[j:])
		if !pre || !post {
			continue
		}
		n, err := strconv.ParseUint(name[i:j], 10, 64)
		if err != nil {
			return seqFile{}, false
		}
		return seqFile{path: name, digits: name[i:j], n: n}, true
	}
	return seqFile{}, false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// numbered is the files among names that glob numbers, in number order and then
// path order, so every report names them the same way on every run.
func numbered(glob string, names []string) []seqFile {
	var out []seqFile
	for _, name := range names {
		if f, ok := sequenceNumber(glob, name); ok {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b seqFile) int {
		if a.n != b.n {
			if a.n < b.n {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})
	return out
}

// clashes groups files sharing a number, each group in path order.
func clashes(files []seqFile) [][]seqFile {
	var out [][]seqFile
	for i := 0; i < len(files); {
		j := i + 1
		for j < len(files) && files[j].n == files[i].n {
			j++
		}
		if j-i > 1 {
			out = append(out, files[i:j])
		}
		i = j
	}
	return out
}

// treeNames lists every path rev holds, from the repository root wherever dir
// sits in it.
func treeNames(dir, rev string) ([]string, error) {
	return gitNames(dir, "ls-tree", "-r", "-z", "--name-only", "--full-tree", rev)
}

// checkSequences judges every [numbered_files] glob on tree, the commit drive is
// about to push or open a pull request for, against trunkRev, the trunk commit it has to follow.
// The branch's own files are the ones it added between base and tip; renames
// count as adds, so a file moved to a new number is judged at that number.
//
// The number each problem file has to take is one above the trunk's highest,
// in the branch's own order, so a branch adding two files keeps them in the
// order it wrote them.
func checkSequences(dir string, seqs map[string]string, tree, base, tip, trunkRev string) ([]seqReport, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	trunkNames, err := treeNames(dir, trunkRev)
	if err != nil {
		return nil, err
	}
	treeAll, err := treeNames(dir, tree)
	if err != nil {
		return nil, err
	}
	addedAll, err := gitNames(dir, "diff", "-z", "--name-only", "--no-renames", "--diff-filter=A", base, tip)
	if err != nil {
		return nil, err
	}
	var out []seqReport
	for _, name := range sortedKeys(seqs) {
		glob := seqs[name]
		r := seqReport{name: name}
		onTrunk := numbered(glob, trunkNames)
		if len(onTrunk) > 0 {
			r.high = &onTrunk[len(onTrunk)-1]
		}
		r.trunkClashes = clashes(onTrunk)
		inTree := numbered(glob, treeAll)
		var added []seqFile
		for _, f := range numbered(glob, addedAll) {
			if slices.ContainsFunc(inTree, func(t seqFile) bool { return t.path == f.path }) {
				added = append(added, f)
			}
		}
		var next uint64
		if r.high != nil {
			next = r.high.n + 1
		}
		var prev *seqFile
		for i, f := range added {
			want := max(f.n, next)
			next = want + 1
			if want != f.n {
				p := seqProblem{file: f, use: fmt.Sprintf("%0*d", len(f.digits), want)}
				switch other := sharing(inTree, f); {
				case other != nil:
					p.against, p.reason = *other, "shares its number with"
				case r.high != nil && f.n <= r.high.n:
					p.against, p.reason = *r.high, "is at or below the trunk's highest,"
				default:
					p.against, p.reason = *prev, "has to stay after this branch's"
				}
				r.problems = append(r.problems, p)
			}
			prev = &added[i]
		}
		out = append(out, r)
	}
	return out, nil
}

// sharing is another file in files with f's number, or nil.
func sharing(files []seqFile, f seqFile) *seqFile {
	for i, o := range files {
		if o.n == f.n && o.path != f.path {
			return &files[i]
		}
	}
	return nil
}

// sequenceFindings is the request-changes body: one line per file, naming what it meets
// and the number to use.
func sequenceFindings(reports []seqReport) string {
	var b strings.Builder
	for _, r := range reports {
		for _, p := range r.problems {
			fmt.Fprintf(&b, "[numbered_files] %s: %s %s %s; use %s\n", r.name, p.file.path, p.reason, p.against.path, p.use)
		}
	}
	return b.String()
}

// trunkClashText names the trunk's own clashes, for the park that stops a drive
// on them.
func trunkClashText(reports []seqReport) string {
	var b strings.Builder
	for _, r := range reports {
		for _, g := range r.trunkClashes {
			paths := make([]string, len(g))
			for i, f := range g {
				paths[i] = f.path
			}
			fmt.Fprintf(&b, "[numbered_files] %s: %s share %s\n", r.name, strings.Join(paths, ", "), g[0].digits)
		}
	}
	return b.String()
}

// sequenceHeader opens a numbered-files request. drover names the number and never
// renames the file itself: a migration's content can refer to its own version,
// and only the session that wrote it can say where.
func sequenceHeader(trunk, sha string) string {
	return fmt.Sprintf("drover drive compared this branch's numbered files with %s at %s. "+
		"Each file below has to take the number named, in this order, before it can merge: "+
		"rename it, and change anything inside it or elsewhere that refers to the old number. "+
		"drover checks and never renumbers.", trunk, shortSHA(sha))
}

// trunkRef is the trunk commit a numbered-files glob is judged against: origin's where it
// exists, for the reason catchUp merges origin's, and the local trunk where
// there is no remote to read.
func (s *session) trunkRef(trunk string) (string, error) {
	if sha, err := revParse(s.cfg.Root, "origin/"+trunk); err == nil {
		return sha, nil
	}
	return revParse(s.cfg.Root, trunk)
}

// sequenceHold judges the branch head against the trunk before a pull request
// opens. A catch-up that merged nothing, or never ran, never compared the
// branch's numbers with the trunk's, and a clean merge is exactly the case
// where git says nothing.
func (s *session) sequenceHold(it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem, trunk string) (DriveItem, bool) {
	if len(s.cfg.Sequences) == 0 {
		return DriveItem{}, false
	}
	to, err := s.trunkRef(trunk)
	if err != nil {
		return park("branch_failed", "the [numbered_files] check could not resolve "+trunk+": "+err.Error()), true
	}
	base, err := mergeBase(s.cfg.Root, d.Branch, to)
	if err != nil {
		return park("branch_failed", err.Error()), true
	}
	reports, err := checkSequences(s.cfg.Root, s.cfg.Sequences, d.Branch, base, d.Branch, to)
	if err != nil {
		return park("gate_unrunnable", "the [numbered_files] check would not read "+d.Branch+": "+err.Error()), true
	}
	if out := sequenceFindings(reports); out != "" {
		return s.requestChangesNumbered(it, d, park, trunk, to, out), true
	}
	if out := trunkClashText(reports); out != "" {
		d.stop = errTrunkRed
		return park(parkTrunkRed, trunk+" at "+shortSHA(to)+" holds two files sharing a number, so no branch can fix it:\n"+out), true
	}
	return DriveItem{}, false
}

// requestChangesNumbered sends a branch back with the files that cannot keep their
// numbers. It counts against max_review_rounds as a gate failure does.
func (s *session) requestChangesNumbered(it backlog.Item, d *DriveItem, park func(cause, why string) DriveItem, trunk, sha, out string) DriveItem {
	if err := s.requestChangesFromVerify(it, sequenceHeader(trunk, sha), out); err != nil {
		return park(parkGateRefused, "numbered files on "+d.Branch+" clash with or sort below "+trunk+
			" and the finding could not be written back: "+err.Error())
	}
	d.Rounds++
	d.Outcome, d.Park = outcomeParked, parkGateRefused
	d.Reason = "numbered files in [numbered_files] clash with or sort below " + trunk + "@" + shortSHA(sha) + "; sent back"
	s.leaveItem(d)
	return *d
}

// sequencePlan is --dry-run's line per [numbered_files] glob: the trunk's highest
// number and the one the next item would have to use.
func (s *session) sequencePlan(w io.Writer) {
	if len(s.cfg.Sequences) == 0 {
		return
	}
	trunk := s.trunk()
	to, err := s.trunkRef(trunk)
	var names []string
	if err == nil {
		names, err = treeNames(s.cfg.Root, to)
	}
	for _, name := range sortedKeys(s.cfg.Sequences) {
		glob := s.cfg.Sequences[name]
		switch files := numbered(glob, names); {
		case err != nil:
			fmt.Fprintf(w, "numbered_files.%s (%s): %s would not read: %v\n", name, glob, trunk, err)
		case len(files) == 0:
			fmt.Fprintf(w, "numbered_files.%s (%s): %s holds none\n", name, glob, trunk)
		default:
			high := files[len(files)-1]
			fmt.Fprintf(w, "numbered_files.%s (%s): %s holds up to %s (%s); the next item uses %0*d\n",
				name, glob, trunk, high.digits, high.path, len(high.digits), high.n+1)
		}
	}
	fmt.Fprintln(w)
}
