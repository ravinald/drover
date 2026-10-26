package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
)

// evPreflight is a check drive ran before spending a session: one event per
// finding, or one with reasonClean for a check that found nothing.
const evPreflight = "preflight"

const reasonClean = "clean"

// parkRefDrift holds an item whose cited lines are gone from the trunk, under
// stale_citations at operator.
const parkRefDrift = "stale_citations"

// preflight emits one check's result.
func (es *eventSink) preflight(id, key, reason string) {
	if es == nil {
		return
	}
	p, _ := es.cfg.Policy(key)
	es.emit(DriveEvent{Event: evPreflight, ID: id, Action: key, Policy: string(p), Reason: reason})
}

// citedRef is one path:line or path:start-end an item body cites.
type citedRef struct {
	Text  string
	Path  string
	Start int
	End   int
}

// citedRoot is a file at the project root cited with a line part, `go.mod:3`,
// which citedFile leaves alone because a bare name carries no slash. The line
// part is what keeps prose like "e.g." out.
var citedRoot = regexp.MustCompile(`(?:^|[^\w./:-])([\w-]+\.[A-Za-z]\w*):(\d+(?:[-,]\d+)*)`)

// citedRefs is every cited path that carries a line part. A bare path names a
// file the item may be about to create, so only a line number makes a claim
// about the trunk that can be wrong.
func citedRefs(body string) []citedRef {
	var out []citedRef
	add := func(p, lines string) {
		p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		if p == "" || lines == "" || strings.Contains(p, "..") {
			return
		}
		r := citedRef{Text: p + ":" + lines, Path: p}
		for _, part := range strings.Split(lines, ",") {
			a, b, ranged := strings.Cut(part, "-")
			start, err := strconv.Atoi(a)
			if err != nil {
				continue
			}
			end := start
			if ranged {
				if end, err = strconv.Atoi(b); err != nil {
					end = start
				}
			}
			if r.Start == 0 || start < r.Start {
				r.Start = start
			}
			r.End = max(r.End, end)
		}
		if r.End == 0 || slices.ContainsFunc(out, func(o citedRef) bool { return o.Text == r.Text }) {
			return
		}
		out = append(out, r)
	}
	for _, m := range citedFile.FindAllStringSubmatch(body, -1) {
		add(m[1], m[2])
	}
	for _, m := range citedTick.FindAllStringSubmatch(body, -1) {
		add(m[1], m[2])
	}
	for _, m := range citedRoot.FindAllStringSubmatch(body, -1) {
		add(m[1], m[2])
	}
	return out
}

// driftRef is the trunk a citation is read against: origin's where this
// checkout has it, because the worktree an item runs in is cut from a trunk the
// drive brings level with origin, and a local trunk the operator has not
// pulled is the drift this check exists to find.
func (s *session) driftRef() string {
	refs := trunkRefs(s.cfg.Root, s.trunk())
	return refs[len(refs)-1]
}

// refDrift is every citation in an item's body the trunk cannot hold: a file
// it lacks, or a range past the file's last line. Lines that moved within the
// file read as present; nothing here knows what the author meant them to say.
// A citation with no slash is held to its range only where the trunk has the
// file, because nothing else separates `go.mod:3` from `db.internal:5432`.
func (s *session) refDrift(it backlog.Item, ref string) []string {
	var out []string
	for _, r := range citedRefs(it.Body) {
		kind, err := s.objectType(ref, r.Path)
		rooted := !strings.Contains(r.Path, "/")
		switch {
		case rooted && (err != nil || kind != "blob"):
			continue
		case err != nil:
			out = append(out, fmt.Sprintf("%s names a file %s does not have", r.Text, ref))
			continue
		case kind != "blob":
			out = append(out, fmt.Sprintf("%s names a directory on %s, which has no lines", r.Text, ref))
			continue
		}
		blob, err := gitcmd.Command(s.cfg.Root, "cat-file", "blob", ref+":"+r.Path).Output()
		if err != nil {
			continue
		}
		n := bytes.Count(blob, []byte("\n"))
		if len(blob) > 0 && blob[len(blob)-1] != '\n' {
			n++
		}
		if r.End > n {
			out = append(out, fmt.Sprintf("%s runs past the end of %s, which has %d %s on %s",
				r.Text, r.Path, n, plural(n, "line", "lines"), ref))
		}
	}
	return out
}

// objectType is what git holds at path on ref: blob, tree, or an error for
// nothing at all.
func (s *session) objectType(ref, path string) (string, error) {
	kind, err := gitcmd.Command(s.cfg.Root, "cat-file", "-t", ref+":"+path).Output()
	return strings.TrimSpace(string(kind)), err
}

// checkRefDrift reports an item's drifted citations as it is dispatched, and
// says whether stale_citations holds it.
func (s *session) checkRefDrift(it backlog.Item) (hold string) {
	p, _ := s.cfg.Policy("stale_citations")
	if p == polOff {
		return ""
	}
	drift := s.refDrift(it, s.driftRef())
	if len(drift) == 0 {
		events.preflight(it.ID.Bare(), "stale_citations", reasonClean)
		return ""
	}
	for _, d := range drift {
		fmt.Fprintf(errOut, "  ! %s: stale_citations: %s\n", it.ID.Bare(), d)
		events.preflight(it.ID.Bare(), "stale_citations", d)
	}
	if p != polOperator {
		return ""
	}
	return "its cited lines are gone from the trunk, and [supervise] stale_citations is operator: " + strings.Join(drift, "; ")
}

// depsLint is the refusal for a deps: value drover would misread. Deps splits
// on whitespace, so "F65,F66" is one id naming no item, and the item reads as
// blocked forever by a dependency nobody can write; check finds it as
// MISSING DEP or BAD DEPS, which is after the drive that skipped it.
func (s *session) depsLint(it backlog.Item) error {
	if p, _ := s.cfg.Policy("deps_lint"); p == polOff {
		return nil
	}
	raw := strings.TrimSpace(it.Meta[backlog.KeyDeps])
	if raw == "" {
		return nil
	}
	fixed, bad, malformed := correctDeps(raw, s.cfg.Prefixes())
	if !malformed && len(bad) == 0 {
		return nil
	}
	corrected := strings.Join(fixed, " ")
	where := rel(s.cfg.Root, it.Path)
	if n := depsLineNo(it.Path); n > 0 {
		where += ":" + strconv.Itoa(n)
	}
	var why []string
	switch {
	case strings.ContainsAny(raw, `[]"'`):
		why = append(why, "drover reads a deps line as ids separated by spaces, not a YAML list")
	case strings.Contains(raw, ","):
		why = append(why, "ids on a deps line are separated by spaces, so drover reads the commas as part of one id")
	}
	if len(bad) > 0 {
		why = append(why, "not an id here: "+strings.Join(bad, ", "))
	}
	return Refusal{fmt.Errorf("%s: %s reads `%s: %s`: %s.\nWrite it as `%s: %s`",
		it.ID.Bare(), where, backlog.KeyDeps, raw, strings.Join(why, "; "), backlog.KeyDeps, corrected)}
}

// correctDeps reads a raw deps value through backlog.ParseDeps and returns
// the ids it was meant to name, in bare form, and the pieces that name none
// even once the list marks are gone. A bad piece is kept in fixed unchanged,
// so the correction never drops something the author wrote.
func correctDeps(raw string, prefixes []string) (fixed, bad []string, malformed bool) {
	ids, malformed := backlog.ParseDeps(raw)
	for _, tok := range ids {
		id, err := backlog.ParseID(tok, prefixes)
		if err == nil {
			fixed = append(fixed, id.Bare())
			continue
		}
		bad = append(bad, fmt.Sprintf("%q (%v)", tok, err))
		if id, serr := backlog.ParseStem(tok, prefixes); serr == nil {
			tok = id.Bare()
		}
		fixed = append(fixed, tok)
	}
	return fixed, bad, malformed
}

// depsLineNo is the 1-based line of the deps key in a body's frontmatter, or
// 0 when the file will not read.
func depsLineNo(path string) int {
	f, err := os.Open(path) // #nosec G304 -- a backlog item's own path
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if n > 1 && strings.TrimSpace(sc.Text()) == "---" {
			return 0
		}
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), backlog.KeyDeps+":") {
			return n
		}
	}
	return 0
}

// lintQueue refuses a drive over any item whose deps line would be misread:
// the named ones, or with none named every item not yet merged, because a
// malformed line keeps its item out of the ready queue rather than in it.
func (s *session) lintQueue(ids []string) error {
	items, err := s.driveQueue(ids)
	if err != nil {
		return nil
	}
	if len(ids) == 0 {
		items = nil
		for _, it := range s.backlog.Items {
			if !s.ledger.State(it.ID.Bare()).Merged {
				items = append(items, it)
			}
		}
	}
	for _, it := range items {
		if err := s.depsLint(it); err != nil {
			return err
		}
	}
	return nil
}

// linkGap is one file a gate reads that a drive's worktree will lack.
type linkGap struct {
	Verify string
	Path   string
}

// itemVerifies is each distinct verify command the items run, in queue order.
func (s *session) itemVerifies(items []backlog.Item) []string {
	var out []string
	for _, it := range items {
		if g := strings.TrimSpace(prompts.SelectVerify(s.cfg.Verify, it.Meta)); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// canTrace reports whether this host can see what a gate opens. strace is the
// one tracer that needs no privilege; dtruss on macOS needs root and SIP off.
func canTrace() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := exec.LookPath("strace")
	return err == nil
}

// treeLinksGap is every file a gate reads that is untracked or ignored here
// and that no context link carries into a worktree. trace runs each gate once
// under strace in this checkout; otherwise the answer is the untracked files
// under each path the gate's command line names, which misses a file reached
// only through a Makefile or a script.
func (s *session) treeLinksGap(gates []string, trace bool) ([]linkGap, error) {
	var out []linkGap
	for _, gate := range gates {
		var read []string
		var err error
		if trace {
			read, err = s.tracedReads(gate)
		} else {
			read, err = s.namedUntracked(gate)
		}
		if err != nil {
			return out, err
		}
		for _, p := range read {
			if !s.linked(p) && !slices.ContainsFunc(out, func(g linkGap) bool { return g.Path == p }) {
				out = append(out, linkGap{Verify: gate, Path: p})
			}
		}
	}
	return out, nil
}

// linked reports whether a context link carries p into every worktree.
func (s *session) linked(p string) bool {
	for _, l := range s.contextLinks() {
		if p == l.Rel || strings.HasPrefix(p, l.Rel+"/") {
			return true
		}
	}
	return false
}

// verifyPaths is every token of a gate's command line naming a path inside the
// project other than its root: `go test ./test/e2e/...` names test/e2e.
func (s *session) verifyPaths(gate string) []string {
	var out []string
	for _, tok := range strings.Fields(gate) {
		if _, v, ok := strings.Cut(tok, "="); ok {
			tok = v
		}
		tok = strings.Trim(tok, `"'`)
		tok = strings.TrimSuffix(strings.TrimSuffix(tok, "..."), "/")
		tok = filepath.ToSlash(filepath.Clean(tok))
		if tok == "." || tok == "" || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "..") || filepath.IsAbs(tok) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.cfg.Root, tok)); err == nil && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

func (s *session) namedUntracked(gate string) ([]string, error) {
	paths := s.verifyPaths(gate)
	if len(paths) == 0 {
		return nil, nil
	}
	// --others without --exclude-standard lists ignored files too, which is
	// the half that matters: an env file is ignored precisely so it is never
	// committed, and that is what keeps it out of a worktree.
	return gitNames(s.cfg.Root, append([]string{"ls-files", "-z", "--others", "--full-name", "--"}, paths...)...)
}

// tracedReads runs a gate once under strace and returns the files it read
// that git does not track here. A file the gate wrote is left out, so a build
// output read back by the next step is not mistaken for an input.
func (s *session) tracedReads(gate string) ([]string, error) {
	log, err := os.CreateTemp("", "drover-trace-*.txt")
	if err != nil {
		return nil, err
	}
	_ = log.Close()
	defer func() { _ = os.Remove(log.Name()) }()
	fmt.Fprintf(errOut, "· worktree_links_gap: running the verify command once under strace to see what it reads: %s\n", gate)
	cmd := exec.CommandContext(context.Background(), "strace", "-f", "-qq", "-y", "-e", "trace=file", "-o", log.Name(), "sh", "-c", gate) // #nosec G204 -- the gate is project config
	cmd.Dir = s.cfg.Root
	cmd.Env = append(os.Environ(), runner.EnvRoot+"="+s.cfg.Root)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if ee := (*exec.ExitError)(nil); !errors.As(err, &ee) {
			return nil, fmt.Errorf("strace would not run the verify command: %w", err)
		}
	}
	raw, err := os.ReadFile(log.Name())
	if err != nil {
		return nil, err
	}
	cands := traceReads(raw, s.cfg.Root)
	if len(cands) == 0 {
		return nil, nil
	}
	tracked, err := gitNames(s.cfg.Root, append([]string{"ls-files", "-z", "--full-name", "--"}, cands...)...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cands {
		if !slices.Contains(tracked, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// -y has strace print the path behind every descriptor: the dirfd an *at call
// resolves against (AT_FDCWD included, as the caller's working directory) and
// the file a successful open returns. A gate that changes directory, as `go
// test` does into each package, opens "hosts.env" meaning test/e2e/hosts.env;
// joined to the project root instead, the read is lost.
var traceLine = regexp.MustCompile(`^(?:\d+\s+)?(\w+)\((?:(?:AT_FDCWD|\d+)(?:<([^>]*)>)?, )?"([^"]+)"(.*)= (-?\d+)(?:<([^>]*)>)?`)

// traceReads is every regular file under root an strace log shows opened or
// stat'ed successfully and never written, root-relative. A file is named by the
// path the gate asked for, joined to the directory it asked from: the opened
// path strace prints has every link resolved, so a root .env linked to a file
// outside the project would fall outside root and vanish. The opened path
// stands in only for a call that printed no directory, and root after that,
// which is right only for a process that never changed directory.
func traceReads(raw []byte, root string) []string {
	roots := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	var reads, written []string
	for _, line := range strings.Split(string(raw), "\n") {
		m := traceLine.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[5], "-") {
			continue
		}
		call, dir, p, rest, opened := m[1], m[2], m[3], m[4], m[6]
		switch {
		case filepath.IsAbs(p):
		case filepath.IsAbs(dir):
			p = filepath.Join(dir, p)
		case filepath.IsAbs(opened):
			p = opened
		default:
			p = filepath.Join(root, p)
		}
		r, ok := underRoot(filepath.Clean(p), roots)
		if !ok || r == ".git" || strings.HasPrefix(r, ".git"+string(filepath.Separator)) {
			continue
		}
		r = filepath.ToSlash(r)
		switch call {
		case "open", "openat", "openat2", "creat":
			if call == "creat" || strings.Contains(rest, "O_WRONLY") || strings.Contains(rest, "O_RDWR") || strings.Contains(rest, "O_CREAT") {
				written = append(written, r)
				continue
			}
		case "stat", "lstat", "newfstatat", "statx", "access", "faccessat", "faccessat2", "readlink", "readlinkat", "execve":
		case "unlink", "unlinkat", "rename", "renameat", "renameat2", "mkdir", "mkdirat", "truncate",
			"link", "linkat", "symlink", "symlinkat", "chmod", "fchmodat", "utimensat":
			written = append(written, r)
			continue
		default:
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, r)); err == nil && fi.Mode().IsRegular() && !slices.Contains(reads, r) {
			reads = append(reads, r)
		}
	}
	return slices.DeleteFunc(reads, func(r string) bool { return slices.Contains(written, r) })
}

// underRoot is p relative to whichever spelling of the root it sits under:
// strace reports a descriptor's path with symlinks resolved, so a project under
// a linked directory shows up under its real path.
func underRoot(p string, roots []string) (string, bool) {
	for _, root := range roots {
		if r, ok := strings.CutPrefix(p, root+string(filepath.Separator)); ok {
			return r, true
		}
	}
	return "", false
}

// treeLinksLine is the [drive] worktree_links line that carries every gap.
func (s *session) treeLinksLine(gaps []linkGap) string {
	links := slices.Clone(s.cfg.Loop.TreeLinks)
	for _, g := range gaps {
		if !slices.Contains(links, g.Path) {
			links = append(links, g.Path)
		}
	}
	quoted := make([]string, len(links))
	for i, l := range links {
		quoted[i] = strconv.Quote(l)
	}
	return "worktree_links = [" + strings.Join(quoted, ", ") + "]"
}

// checkTreeLinks warns about each gap before a drive's first gate. Never a
// refusal: a file the gate reads only on one path through a Makefile may never
// be read, and the gate's own failure names it if it is.
func (s *session) checkTreeLinks(w io.Writer, items []backlog.Item, trace bool) {
	if p, _ := s.cfg.Policy("worktree_links_gap"); p == polOff {
		return
	}
	gaps, err := s.treeLinksGap(s.itemVerifies(items), trace)
	if err != nil {
		fmt.Fprintf(w, "· worktree_links_gap: could not list what the verify command reads (%v)\n", err)
	}
	if len(gaps) == 0 {
		events.preflight("", "worktree_links_gap", reasonClean)
		return
	}
	line := s.treeLinksLine(gaps)
	for _, g := range gaps {
		why := fmt.Sprintf("the verify command `%s` reads %s, which git does not track here, so drive's worktrees lack it; "+
			"carry it with [drive] %s", g.Verify, g.Path, line)
		fmt.Fprintf(w, "· worktree_links_gap: %s\n", why)
		events.preflight("", "worktree_links_gap", why)
	}
}

// loadAverage reads the host's 1-minute load average. A variable so a test can
// stand in a loaded host.
var loadAverage = readLoadAverage

// loadPoll is how long a held dispatch waits before reading the load again.
var loadPoll = 15 * time.Second

func readLoadAverage() (float64, error) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			return strconv.ParseFloat(f[0], 64)
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, fmt.Errorf("neither /proc/loadavg nor sysctl vm.loadavg answered: %w", err)
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) == 0 {
		return 0, fmt.Errorf("sysctl vm.loadavg printed %q", out)
	}
	return strconv.ParseFloat(f[0], 64)
}

// admission is host_load_admission's state across one drive's dispatches.
type admission struct {
	holding  bool
	admitted bool
	failed   bool
	// next is when a hold reads the load again. The drive loop wakes more
	// often than that to answer --pause and --stop, and a hold stays held
	// between readings rather than re-sampling on every wake.
	next time.Time
}

// hold reports whether dispatch waits for the load to fall, printing once per
// hold. The stream carries one event per change of state rather than one per
// dispatch: clean on the first admission and on each hold's end, the reason at
// a hold's start and at the first failed reading. A load that cannot be read
// never holds: admission is a courtesy to the host, and a drive that waited on
// a reading it will never get would stop for good.
func (a *admission) hold(cfg *config.Config) bool {
	if p, _ := cfg.Policy("host_load_admission"); p != polAuto {
		return false
	}
	if a.holding && time.Now().Before(a.next) {
		return true
	}
	load, err := loadAverage()
	if err != nil {
		if !a.failed {
			a.failed = true
			why := fmt.Sprintf("%v; dispatching without it", err)
			fmt.Fprintf(errOut, "· host_load_admission: %s\n", why)
			events.preflight("", "host_load_admission", why)
		}
		return false
	}
	ceiling := cfg.Loop.LoadCeiling()
	if load <= ceiling {
		a.holding = false
		if !a.admitted {
			a.admitted = true
			events.preflight("", "host_load_admission", reasonClean)
		}
		return false
	}
	a.next = time.Now().Add(loadPoll)
	if !a.holding {
		a.holding, a.admitted = true, false
		why := fmt.Sprintf("the 1-minute load average is %.2f, above [drive] max_load %g; holding dispatch until it falls", load, ceiling)
		fmt.Fprintf(errOut, "· host_load_admission: %s\n", why)
		events.preflight("", "host_load_admission", why)
	}
	return true
}

// preflightPlan is --dry-run's preflight section: each check, what it found
// and what the run will do about it.
func (s *session) preflightPlan(w io.Writer, items []backlog.Item) {
	fmt.Fprintln(w, "==> preflight")
	row := func(key, said string) {
		p, _ := s.cfg.Policy(key)
		fmt.Fprintf(w, "  %-20s %-10s %s\n", key, p, said)
	}

	if p, _ := s.cfg.Policy("stale_citations"); p == polOff {
		row("stale_citations", "not checked")
	} else {
		ref, n := s.driftRef(), 0
		for _, it := range items {
			n += len(s.refDrift(it, ref))
		}
		switch {
		case n == 0:
			row("stale_citations", "every cited line exists on "+ref)
		case p == polOperator:
			row("stale_citations", fmt.Sprintf("%d %s past %s, listed under each item; the run holds those items for you", n, plural(n, "citation", "citations"), ref))
		default:
			row("stale_citations", fmt.Sprintf("%d %s past %s, listed under each item; the run launches them anyway", n, plural(n, "citation", "citations"), ref))
		}
	}

	if p, _ := s.cfg.Policy("deps_lint"); p == polOff {
		row("deps_lint", "not checked")
	} else {
		row("deps_lint", "every deps line parses")
	}

	if p, _ := s.cfg.Policy("worktree_links_gap"); p == polOff {
		row("worktree_links_gap", "not checked")
	} else {
		how := "untracked files under the paths each verify command names"
		if canTrace() {
			how = "what each verify command opened under strace"
		}
		gaps, err := s.treeLinksGap(s.itemVerifies(items), canTrace())
		switch {
		case err != nil:
			row("worktree_links_gap", "could not list what the verify command reads: "+err.Error())
		case len(gaps) == 0:
			row("worktree_links_gap", "no gap found in "+how)
		default:
			for _, g := range gaps {
				row("worktree_links_gap", fmt.Sprintf("the verify command `%s` reads %s, which drive's worktrees lack", g.Verify, g.Path))
			}
			row("worktree_links_gap", "carry them with [drive] "+s.treeLinksLine(gaps))
		}
	}

	if p, _ := s.cfg.Policy("host_load_admission"); p != polAuto {
		row("host_load_admission", "load not read")
	} else if load, err := loadAverage(); err != nil {
		row("host_load_admission", err.Error()+"; the run dispatches without it")
	} else {
		verb := "dispatches"
		if load > s.cfg.Loop.LoadCeiling() {
			verb = "holds dispatch"
		}
		row("host_load_admission", fmt.Sprintf("load %.2f against [drive] max_load %g; the run %s", load, s.cfg.Loop.LoadCeiling(), verb))
	}
	row("usage_admission", s.usagePlan())
	fmt.Fprintln(w)
}
