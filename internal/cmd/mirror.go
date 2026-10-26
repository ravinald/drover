package cmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

// The mirror's label vocabulary. Membership is its own label rather than the
// state prefix, because `gh issue list --label` matches a whole name and a read
// keyed on seven state labels is seven reads. It is also what separates an issue
// drover opened from the one an intaken item was taken in from: that issue is
// the reporter's, so drover labels, closes and rewrites only the ones carrying
// this.
const (
	mirrorMember = "drover"
	mirrorPrefix = "drover:"
	// stateRunning exists on the tracker alone. A session in progress leaves
	// nothing workState can read, and "someone is on this" is the one fact a
	// collaborator browsing issues most needs.
	stateRunning = "running"
	// stateShelved is tracker-only for the same reason: a shelved body leaves
	// the backlog, so no state is ever derived for it, and its issue is closed
	// as not planned under this label rather than left under its last one.
	stateShelved = "shelved"
)

// The fences around the published requirements. drover never reads text back
// out of them as a contract; check compares what sits between them with the
// file, and a mirror whose fences somebody deleted has drifted by definition.
const (
	mirrorOpen  = "<!-- drover:requirements -->"
	mirrorClose = "<!-- /drover:requirements -->"
)

// mirrorIssue is what drover read off one mirrored issue: enough to say where it
// disagrees with the file without asking the tracker again.
type mirrorIssue struct {
	gh.Issue
	// Member is whether drover opened it. An issue without the membership label
	// is somebody else's, and the mirror never writes to it.
	Member bool
}

func asMirror(is gh.Issue) mirrorIssue {
	return mirrorIssue{Issue: is, Member: slices.Contains(is.Labels, mirrorMember)}
}

// mirrorLabels is the one map from an item's state to the label its issue
// carries. A label is a word for a collaborator browsing the tracker, chosen
// for that reader, so it is looked up here rather than spelled as the prefix
// plus a state name: the two vocabularies are free to differ, and check's
// MIRROR DRIFT and MIRROR SHELVED compare against this table and nothing else.
var mirrorLabels = map[string]string{
	stateScaffold: mirrorPrefix + "draft",
	stateReady:    mirrorPrefix + "ready",
	stateBlocked:  mirrorPrefix + "blocked",
	stateRunning:  mirrorPrefix + "in-progress",
	stateRan:      mirrorPrefix + "implemented",
	stateDone:     mirrorPrefix + "merged",
	stateShelved:  mirrorPrefix + "deferred",
}

// mirrorLabel is the label one state renders as. A state outside the table is a
// state somebody added without deciding its label, and it renders under the
// prefix rather than vanishing, so MIRROR DRIFT names it instead of passing it.
func mirrorLabel(state string) string {
	if l, ok := mirrorLabels[state]; ok {
		return l
	}
	return mirrorPrefix + state
}

// stateLabels are the drover:<label> labels the issue carries now.
func (m mirrorIssue) stateLabels() []string {
	var out []string
	for _, l := range m.Labels {
		if strings.HasPrefix(l, mirrorPrefix) {
			out = append(out, l)
		}
	}
	return out
}

// labelFits reports whether the issue's state labels say what the backlog says.
// in-progress stands for ready or ran: a live session writes nothing workState can
// see, so check cannot tell a running label that is true from one left behind.
func (m mirrorIssue) labelFits(state string) bool {
	got := m.stateLabels()
	if len(got) != 1 {
		return false
	}
	if got[0] == mirrorLabel(state) {
		return true
	}
	return got[0] == mirrorLabel(stateRunning) && (state == stateReady || state == stateRan)
}

// missingLinks are the dep issues GitHub does not record this one as blocked by.
func (m mirrorIssue) missingLinks(want []int) []int {
	var out []int
	for _, n := range want {
		if !slices.Contains(m.BlockedBy, n) {
			out = append(out, n)
		}
	}
	return out
}

// mirroring reports whether this project publishes accepted items.
func (s *session) mirroring() bool { return s.cfg.BacklogMirror() == config.MirrorGitHub }

// issueOf is the number in an item's issue key, zero when it names none.
func issueOf(it backlog.Item) int {
	n, err := strconv.Atoi(strings.TrimSpace(it.Get(backlog.KeyIssue)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// stateLabel is the label one state renders as, created on first use.
func stateLabel(state string) gh.Label {
	name := mirrorLabel(state)
	return gh.Label{Name: name, Color: "C5DEF5", Description: "drover item state: " + strings.TrimPrefix(name, mirrorPrefix)}
}

var memberLabel = gh.Label{Name: mirrorMember, Color: "0E8A16",
	Description: "Mirrors a drover backlog item; the backlog file is the contract"}

// mirrorBody is the whole issue body: the line saying which side is the
// contract, then the requirements between fences.
func mirrorBody(id, requirements string) string {
	return fmt.Sprintf("drover item %s. The backlog file is the contract and this issue is a mirror of it: "+
		"edit the file, not this text, because nothing here is read back.\n\n%s\n%s\n%s\n",
		id, mirrorOpen, requirements, mirrorClose)
}

var rawMaterial = regexp.MustCompile(`(?ms)^--- raw material:.*?^--- end raw material ---[ \t]*$`)

// mirrorRequirements is the numbered list as the file holds it, continuation
// lines included, and nothing else.
//
// Intake's raw-material block is cut before anything is matched. It quotes the
// issue an item was taken in from, which is evidence rather than contract, and
// publishing it would repost somebody's report under drover's name.
func mirrorRequirements(body string) string {
	lines := strings.Split(rawMaterial.ReplaceAllString(body, ""), "\n")
	var out []string
	in := false
	for _, line := range lines {
		switch {
		case numberedLine.MatchString(line):
			in = true
		case !in:
			continue
		case strings.TrimSpace(line) != "" && line[0] != ' ' && line[0] != '\t':
			in = false
			continue
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// numberedLine is backlog's requirement pattern with text required, so the
// scaffold's bare "1." placeholders are not a list.
var numberedLine = regexp.MustCompile(`^\d+\.[ \t]+\S`)

// published is the requirements text an issue body carries between the fences,
// and false when either fence is gone.
func published(body string) (string, bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	_, rest, ok := strings.Cut(body, mirrorOpen)
	if !ok {
		return "", false
	}
	text, _, ok := strings.Cut(rest, mirrorClose)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(text), true
}

// depIssues resolves each dep to the issue its item names. A dep with no
// readable item or no issue key is returned by id, for the caller to name.
func (s *session) depIssues(it backlog.Item) (nums []int, unlinked []string) {
	for _, dep := range it.Deps() {
		di, err := s.item(dep)
		if err != nil || issueOf(di) == 0 {
			unlinked = append(unlinked, dep)
			continue
		}
		nums = append(nums, issueOf(di))
	}
	return nums, unlinked
}

// mirrorWarn reports a tracker write that did not happen. Every caller carries
// on afterwards: the file is the contract and it is already written, so a
// tracker nobody can reach costs the mirror and never the command.
//
// It and every progress line here go to stderr, as the launch banner does: run
// and drive reach the mirror mid-command, and stdout under drive --json carries
// one object per line and nothing else.
func mirrorWarn(format string, args ...any) {
	fmt.Fprintf(errOut, "  ! mirror: "+format+"; drover check lists it until it finishes\n", args...)
}

// publish brings the item's issue in line with its file, opening the issue when
// the item names none. accept is the one caller: it is where an item becomes a
// contract, and running it again on an accepted item is how a mirror that fell
// behind is repaired.
//
// An issue opened here changes what its dependents' issues should carry, since
// each of them is now blocked by a number that did not exist a moment ago. A
// batch accept in backlog order publishes the dependent first, so the link is
// written from this side as well as from the dependent's own publish.
//
// The only error returned is a local one: an issue opened whose number could not
// be written back. Every tracker failure is reported and swallowed.
func (s *session) publish(it backlog.Item) error {
	if !s.mirroring() {
		return nil
	}
	id := it.ID.Bare()
	if issueOf(it) == 0 {
		n, err := gh.CreateIssue(s.labels(), it.Title(), mirrorBody(id, mirrorRequirements(it.Body)), []gh.Label{memberLabel}, 0)
		if err != nil {
			mirrorWarn("%s was not published: %v", id, err)
			return nil
		}
		if n == 0 {
			mirrorWarn("%s was published and gh printed no issue number, so %s carries none; "+
				"find it under the %q label and set issue: by hand before accepting again", id, rel(s.cfg.Root, it.Path), mirrorMember)
			return nil
		}
		if err := s.recordIssue(it, n); err != nil {
			return fmt.Errorf("issue #%d opened for %s, but its number would not write to %s: %w.\n"+
				"Set issue: %d there by hand, or the next accept opens a second one",
				n, id, rel(s.cfg.Root, it.Path), err, n)
		}
		fmt.Printf("published: %s as issue %s (the file stays the contract)\n", id, s.issueRefTo(os.Stdout, n))
		// The dependents resolve this item's issue through the loaded backlog,
		// which still holds the body without the number.
		s.mirrorReloaded(id, true)
		return nil
	}
	s.mirrorSync(it, true)
	return nil
}

// recordIssue writes the issue key and records the body row, so the write reads
// as drover's own rather than a hand edit.
func (s *session) recordIssue(it backlog.Item, number int) error {
	lines, err := bodyLines(it.Path)
	if err != nil {
		return err
	}
	written, err := setFrontmatter(lines, backlog.KeyIssue, strconv.Itoa(number), s.cfg.DeclaredKeys())
	if err != nil {
		return err
	}
	return s.writeBody(it, written)
}

// mirrorState rewrites the item's state label after drover changed the state.
// One read and at most one write; nothing when the project does not mirror or
// the issue is not drover's.
func (s *session) mirrorState(it backlog.Item, state string) {
	m, ok := s.mirrored(it)
	if !ok {
		return
	}
	s.writeState(m, it.ID.Bare(), state)
}

// mirrorNow is mirrorState with the state read off the backlog as it stands.
// It is for a change that moves the item alone: a run ending moves it to ran,
// which no other item's state or links derive from.
func (s *session) mirrorNow(it backlog.Item) {
	state, _ := s.workState(it, s.cfg.LogDir())
	s.mirrorState(it, state)
}

// mirrorAround is the mirror's half of every command that changes what the
// mirror derives for an item without publishing it: finish, drive's
// reconciliation of a merge, mark-merged, unmark-merged, split and unaccept. The caller passes
// the item as it now stands on disk.
func (s *session) mirrorAround(it backlog.Item) {
	if s.mirroring() {
		s.mirrorSync(it, false)
	}
}

// mirrorReloaded is mirrorSync for a caller that has just rewritten the item's
// body: workState reads deps and the stop marker off the loaded item, so
// the backlog is read again first. The body is already written, so a backlog
// that will not reload costs the mirror and never the command.
//
// publishing is honored only for an item that reloads accepted: unshelve can
// revive a scaffold, and a scaffold's draft is published by accept alone.
func (s *session) mirrorReloaded(id string, publishing bool) {
	if !s.mirroring() {
		return
	}
	err := s.reload()
	var it backlog.Item
	if err == nil {
		it, err = s.item(id)
	}
	if err != nil {
		mirrorWarn("%s's issue was not rewritten, because the backlog would not reload: %v", id, err)
		return
	}
	s.mirrorSync(it, publishing && !s.isScaffold(it))
}

// mirrorShelved retires the issue of a body just moved to the shelf: the
// drover:deferred label in place of its state's, and closed as not planned. A
// shelved body is never parsed again, so this is the last write its issue gets
// until unshelve, and check's MIRROR SHELVED is what reports one that failed.
func (s *session) mirrorShelved(id backlog.ID, path string) {
	if !s.mirroring() {
		return
	}
	bare := id.Bare()
	meta, err := s.shelvedMeta(path)
	if err != nil {
		fmt.Fprintf(errOut, "  ! mirror: %s moved, and its issue, if it names one, was not closed: %v\n", bare, err)
		return
	}
	m, ok := s.mirrored(backlog.Item{ID: id, Path: path, Meta: meta})
	if !ok {
		return
	}
	s.writeState(m, bare, stateShelved)
	if !strings.EqualFold(m.State, "OPEN") {
		return
	}
	comment := fmt.Sprintf("%s is shelved: written, and deliberately not going to be done. `drover unshelve %s` brings it back.", bare, bare)
	if err := gh.CloseNotPlanned(s.labels(), m.Number, comment); err != nil {
		mirrorWarn("%v", err)
		return
	}
	fmt.Fprintf(errOut, "  ✓ %s closed as not planned: %s is shelved\n", s.issueRefTo(os.Stderr, m.Number), bare)
}

// shelvedMeta reads a shelved body's frontmatter and nothing else. Nothing
// renders off the shelf, so the one key read here is the issue it names.
func (s *session) shelvedMeta(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a body under the configured shelf
	if err != nil {
		return nil, err
	}
	meta, _, err := backlog.ParseFrontmatter(path, data, s.cfg)
	return meta, err
}

// mirrorSync writes everything the mirror derives from one item: its own
// issue's state label, open or closed state and blocked-by links, the
// requirements too when publishing, and then each mirrored dependent's label and
// its link to this item's issue.
//
// The dependents are written because workState derives blocked from deps that
// have not merged and depIssues reads each dep's issue key, so a change to this
// item moves them without anything being written about them. Only direct
// dependents move: an item two hops away is still blocked on the one between.
//
// The item's own label is written outright, because the caller just changed its
// state. A dependent's label is left alone where it already fits, so a running
// label a live session put there survives a finish that unblocked it.
// Closing is not a disposition and needs none: the issue was drover's index
// entry for the item.
func (s *session) mirrorSync(it backlog.Item, publishing bool) {
	id := it.ID.Bare()
	logDir := s.cfg.LogDir()
	if m, ok := s.mirrored(it); ok {
		if publishing {
			s.writeRequirements(m, it)
		}
		state, _ := s.workState(it, logDir)
		s.writeState(m, id, state)
		s.writeOpen(m, id, state == stateDone)
		s.writeLinks(m, it, publishing)
	}
	for _, dep := range s.mirroredDependents(id) {
		m, ok := s.mirrored(dep)
		if !ok {
			continue
		}
		if state, _ := s.workState(dep, logDir); !m.labelFits(state) {
			s.writeState(m, dep.ID.Bare(), state)
		}
		s.writeLinks(m, dep, false)
	}
}

// mirroredDependents are the items naming id on deps: that name an issue.
func (s *session) mirroredDependents(id string) []backlog.Item {
	var out []backlog.Item
	for _, it := range s.backlog.Items {
		if issueOf(it) != 0 && slices.Contains(it.Deps(), id) {
			out = append(out, it)
		}
	}
	return out
}

// reportMirror is finish --no-tracker's line: the mirror writes it did not make.
func (s *session) reportMirror(it backlog.Item) {
	if !s.mirroring() {
		return
	}
	var issues []int
	if n := issueOf(it); n != 0 {
		issues = append(issues, n)
	}
	var ids []string
	for _, dep := range s.mirroredDependents(it.ID.Bare()) {
		issues = append(issues, issueOf(dep))
		ids = append(ids, dep.ID.Bare())
	}
	if len(issues) == 0 {
		return
	}
	fix := "drover accept " + strings.Join(append([]string{it.ID.Bare()}, ids...), " ")
	fmt.Fprintf(errOut, "  · --no-tracker: %s not relabeled or closed; %s brings the mirror up to date\n",
		s.issueRefsTo(os.Stderr, issues), fix)
}

// mirrored reads the item's issue, links included, when the project mirrors and
// drover opened it. An intaken item's issue is the report it came from, and
// rewriting anything on it would replace somebody's words with drover's.
func (s *session) mirrored(it backlog.Item) (mirrorIssue, bool) {
	number := issueOf(it)
	if !s.mirroring() || number == 0 {
		return mirrorIssue{}, false
	}
	is, err := gh.ViewLinked(s.labels(), number)
	if err != nil {
		mirrorWarn("issue %s for %s could not be read, so its labels and links were not written: %v", s.issueRefTo(os.Stderr, number), it.ID.Bare(), err)
		return mirrorIssue{}, false
	}
	m := asMirror(is)
	return m, m.Member
}

// writeState leaves exactly one state label on the issue: state's.
func (s *session) writeState(m mirrorIssue, id, state string) {
	want := mirrorLabel(state)
	var remove []string
	have := false
	for _, l := range m.stateLabels() {
		if l == want {
			have = true
			continue
		}
		remove = append(remove, l)
	}
	if have && len(remove) == 0 {
		return
	}
	var add []gh.Label
	if !have {
		add = []gh.Label{stateLabel(state)}
	}
	if err := gh.EditIssue(s.labels(), m.Number, add, remove, nil); err != nil {
		mirrorWarn("issue %s is not labeled %s for %s: %v", s.issueRefTo(os.Stderr, m.Number), want, id, err)
		return
	}
	fmt.Fprintf(errOut, "  ✓ %s: %s\n", s.issueRefTo(os.Stderr, m.Number), want)
}

// writeOpen closes or reopens the issue to match whether the item merged.
func (s *session) writeOpen(m mirrorIssue, id string, done bool) {
	open := strings.EqualFold(m.State, "OPEN")
	switch {
	case done && open:
		if err := gh.Close(s.labels(), m.Number, id+" finished."); err != nil {
			mirrorWarn("%v", err)
			return
		}
		fmt.Fprintf(errOut, "  ✓ %s closed: %s is recorded done\n", s.issueRefTo(os.Stderr, m.Number), id)
	case !done && !open:
		if err := gh.Reopen(s.labels(), m.Number, id+" is not recorded done, so the work is open again."); err != nil {
			mirrorWarn("%v", err)
			return
		}
		fmt.Fprintf(errOut, "  ✓ %s reopened: %s is not recorded done\n", s.issueRefTo(os.Stderr, m.Number), id)
	}
}

// writeRequirements rewrites the issue's requirements where they differ from the file's.
func (s *session) writeRequirements(m mirrorIssue, it backlog.Item) {
	reqs := mirrorRequirements(it.Body)
	got, ok := published(m.Body)
	if ok && got == reqs {
		return
	}
	body := mirrorBody(it.ID.Bare(), reqs)
	if err := gh.EditIssue(s.labels(), m.Number, nil, nil, &body); err != nil {
		mirrorWarn("issue %s still carries %s's old requirements: %v", s.issueRefTo(os.Stderr, m.Number), it.ID.Bare(), err)
	} else if ok {
		fmt.Fprintf(errOut, "  ✓ %s: requirements rewritten from %s\n", s.issueRefTo(os.Stderr, m.Number), rel(s.cfg.Root, it.Path))
	}
}

// writeLinks adds the blocked-by links the issue lacks for the item's deps.
// named is whether to say which deps have no issue to link, which publish does
// and the relabeling commands do not repeat.
func (s *session) writeLinks(m mirrorIssue, it backlog.Item, named bool) {
	deps, unlinked := s.depIssues(it)
	if named && len(unlinked) > 0 {
		fmt.Fprintf(errOut, "  · %s: no blocked-by link for %s, which %s no issue\n",
			s.issueRefTo(os.Stderr, m.Number), strings.Join(unlinked, ", "), plural(len(unlinked), "names", "name"))
	}
	missing := m.missingLinks(deps)
	if len(missing) == 0 {
		return
	}
	if err := gh.AddBlockedBy(s.labels(), m.Number, missing); err != nil {
		mirrorWarn("%v", err)
		return
	}
	fmt.Fprintf(errOut, "  ✓ %s: blocked by %s\n", s.issueRefTo(os.Stderr, m.Number), s.issueRefsTo(os.Stderr, missing))
}
