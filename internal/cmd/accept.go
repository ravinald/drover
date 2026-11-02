package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
)

// acceptYes overrides the refusals accept makes about what a body says.
var acceptYes bool

var acceptCmd = &cobra.Command{
	Use:   "accept <id>...",
	Short: "Delete the stop marker, making a scaffolded item runnable.",
	Long: `intake plants a stop marker and draft leaves it there, because deciding that a
proposed list of requirements is the contract is the operator's judgement and
not a runner's. This is that decision, spelled as a command.

It deletes the block intake wrote and nothing else: the marker line, the two
lines under it saying what the marker does, and the blank that separates them
from the body. unaccept writes the same block back, so accept followed by
unaccept returns the file byte for byte.

The lines under the marker are matched against intake's own text rather than
scanned to the next blank, so a marker somebody typed by hand with a paragraph
directly under it loses the marker line alone.

A body that is not finished is refused, naming each line: an angle-bracket
placeholder such as <provider>, in prose or in a code span with no space in it
(a path like internal/<provider>/client.go), or a line carrying one of [intake]
hold_markers, by default "OPERATOR DECISION PENDING" and "BEFORE ACCEPT". A
fenced block, and a code span holding a space (drover show <id>), is a syntax
example and is not read. Neither is a code span holding one HTML or SVG element
or a capitalized component and nothing else (<h1>, <Dialog>). Write a variable
part in braces, internal/{provider-dir}/client.go, which is never a placeholder.

A body that cites a directory in the project tree, as a path dispatch reads,
gets one note per directory: under drive --jobs the item waits for any item
citing a path below it. The note changes nothing and the exit stays 0. A
paragraph whose first line is <!-- drover: context --> cites nothing for that
purpose, here or at dispatch, except for its numbered requirement lines.

An item another unmerged item waits on is read against that item's branch:
every path it cites with a line part, and every backticked name that looks like
a function or type, that the trunk lacks and the waiting item's branch has. The
accepted item runs from the trunk and cannot change that code, and the item
waiting on it cannot merge first, so the two would deadlock. accept refuses
it, names each citation, and changes nothing.

Every path:line the body cites is read against the trunk the way dispatch
reads it, and a file the trunk lacks or a range past its last line is refused,
naming each one. While a dependency is still unmerged the same drift is a
note instead, since the dependency may add the file before this item runs,
and a citation that dependency's branch already holds is not reported.

An [operator] requirement holds the merge until drover signoff records it, so
one that can only be met after the merge is refused: one saying so ("after the
merge", "post-merge", "once merged", "after deploying"), and one running a
backticked flag whose name the trunk holds nowhere, which only this branch's
build can run. Meet it before the merge, put an automated check in [finish]
post_check, or track the post-merge work as its own item.

--yes overrides every refusal accept makes about the body's content: the
deadlock, an unfinished body, a requirement naming live work, a citation the
trunk lacks, and an operator step the merge waits on. It never overrides the
stop-marker or actor checks, or a deps line drover would misread.

Idempotent. An item with no marker is already accepted: that is reported and
exits 0, because the operator named a state rather than a transition.

Under [backlog] mirror = "github" an accepted item is also published: an issue
carrying its title, its numbered requirements and a drover: state label, with
a blocked-by link for each dep that names an issue. The new number is written
into the item's issue key. Accepting an item that already has its issue
rewrites that issue from the file, which is how the mirror is repaired after
` + "`drover check`" + ` reports it behind. A tracker drover cannot reach costs the
mirror and never the accept: the failure is named and the exit stays 0.

Several ids accept each in turn. One that fails (no such item, a padded id, an
unwritable body) is reported on stderr by name and the rest still go ahead; the
exit is then non-zero, 3 when every failure was a refusal and 1 otherwise, and
the last line lists the ids that were not accepted.`,
	Example: `  drover accept B1                  # the requirements are the contract; run may launch
  drover accept B1                  # again: already accepted, exit 0
  drover accept B45 B46 E28         # each in turn; a bad id is named and skipped
  drover accept B59 --yes           # past the deadlock refusal, having read it
  drover accept B60 --yes           # past a placeholder or hold line, having read it

  The three steps around it:

    drover draft B1 --runner claude   # an agent proposes the numbered list
    drover show B1                    # where the body is; read what it proposed
    drover accept B1                  # you accept it

  drover unaccept B1                # put the marker back and redraft`,
	Args: needSomeIDs("accept <id>..."),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if err := s.markerConfigured(); err != nil {
			return err
		}
		if len(args) == 1 {
			return s.acceptOne(args[0])
		}
		var failed []string
		refusedAll := true
		for _, a := range args {
			if err := s.acceptOne(a); err != nil {
				fmt.Fprintf(errOut, "accept %s: %v\n", a, err)
				failed = append(failed, a)
				if Code(err) != ExitRefused {
					refusedAll = false
				}
			}
		}
		if len(failed) == 0 {
			return nil
		}
		err = fmt.Errorf("%d of %d not accepted: %s (each reason is above)",
			len(failed), len(args), strings.Join(failed, " "))
		if refusedAll {
			return Refusal{err}
		}
		return err
	},
}

// acceptOne is the whole of accept for one id. The list form calls it per id
// and keeps going, so one mistyped id in a batch costs that id and not the rest.
func (s *session) acceptOne(bare string) error {
	it, err := s.item(bare)
	if err != nil {
		return s.prefixedHint(bare, err)
	}
	if err := s.depsLint(it); err != nil {
		return err
	}
	lines, err := bodyLines(it.Path)
	if err != nil {
		return err
	}
	at := markerLine(strings.Join(lines, "\n"), s.cfg.Intake.StopMarker)
	if at < 0 {
		fmt.Printf("already accepted: %s carries no stop marker (%s)\n",
			it.ID.Bare(), rel(s.cfg.Root, it.Path))
		return s.publish(it)
	}
	if found, waiters := s.deadlocksOf(it); len(found) > 0 && !acceptYes {
		return Refusal{fmt.Errorf("%s\nNothing was accepted. Point the dependency the other way (%s),\n"+
			"rewrite the body so it stands on the trunk alone, or accept anyway: drover accept %s --yes",
			strings.Join(found, "\n"), flipDeps(it.ID.Bare(), waiters), it.ID.Bare())}
	}
	n := stopBlockLen(lines, at, s.cfg.Intake.StopMarker)
	// Three-index slice: appending onto lines[:at] would otherwise write
	// through the backing array and shift the file under itself.
	kept := append(lines[:at:at], lines[at+n:]...)
	if holds := bodyHolds(kept, s.cfg.Intake.Holds()); len(holds) > 0 && !acceptYes {
		where := rel(s.cfg.Root, it.Path)
		msgs := make([]string, len(holds))
		for i, h := range holds {
			// Nothing is written, so the author opens the file with the marker
			// block still in it: a line below the block sits n lines further down.
			line := h.line
			if line > at {
				line += n
			}
			msgs[i] = fmt.Sprintf("%s:%d carries %s", where, line, h.what)
		}
		return Refusal{fmt.Errorf("%s\nNothing was accepted. The body is not finished: fill in or remove each line above,\n"+
			"or accept anyway: drover accept %s --yes\n"+
			"A variable part is written in braces, {name}, which is never read as a placeholder", strings.Join(msgs, "\n"), it.ID.Bare())}
	}
	if live := liveRequirements(kept); len(live) > 0 && !acceptYes {
		msgs := make([]string, len(live))
		for i, r := range live {
			msgs[i] = fmt.Sprintf("R%d names %q", r.Num, r.what)
		}
		return Refusal{fmt.Errorf("%s\nNothing was accepted. A headless session cannot reach a live host, a deploy or a dev run, "+
			"and a review sends such a requirement back every round until the item parks.\n"+
			"Open each with %s so a person signs it off, rewrite it so the session can meet it in the repository, "+
			"or accept anyway: drover accept %s --yes", strings.Join(msgs, "\n"), backlog.OperatorTag, it.ID.Bare())}
	}
	if late := s.operatorTiming(kept); len(late) > 0 && !acceptYes {
		msgs := make([]string, len(late))
		for i, r := range late {
			msgs[i] = fmt.Sprintf("R%d %s", r.Num, r.what)
		}
		return Refusal{fmt.Errorf("%s\nNothing was accepted. An %s requirement holds the merge until drover signoff records it, "+
			"so a step that can only happen after the merge is one nobody can sign off, and the item parks awaiting_signoff with every check green.\n"+
			"Make it a step the operator meets before the merge, move an automated check to [finish] post_check, "+
			"or track the post-merge work as its own item outside this one's deps chain, or accept anyway: drover accept %s --yes",
			strings.Join(msgs, "\n"), backlog.OperatorTag, it.ID.Bare())}
	}
	drift, waiting := s.acceptDrift(it)
	if len(drift) > 0 && len(waiting) == 0 && !acceptYes {
		return Refusal{fmt.Errorf("%s\nNothing was accepted. A session reads each citation as a claim about the trunk, and these are not on %s.\n"+
			"Correct each one against git show %s:<path>, or accept anyway: drover accept %s --yes",
			strings.Join(drift, "\n"), s.driftRef(), s.driftRef(), it.ID.Bare())}
	}
	if err := s.writeBody(it, "accept", kept); err != nil {
		return err
	}
	fmt.Printf("accepted: %s (stop marker deleted from %s)\n",
		it.ID.Bare(), rel(s.cfg.Root, it.Path))
	for _, d := range drift {
		if len(waiting) > 0 {
			fmt.Printf("note: %s; it has to exist once %s merges, since %s runs on the trunk that merge leaves\n",
				d, strings.Join(waiting, " and "), it.ID.Bare())
		}
	}
	for _, dir := range s.citedDirs(strings.Join(kept[bodyStart(kept):], "\n")) {
		fmt.Printf("note: %s\n", dirNote(it.ID.Bare(), dir))
	}
	if !s.mirroring() {
		return nil
	}
	// The item in hand still holds the marker, and the mirror publishes and
	// labels from the body as it now stands on disk.
	if err := s.reload(); err != nil {
		return err
	}
	if it, err = s.item(bare); err != nil {
		return err
	}
	return s.publish(it)
}

// liveWork is wording that puts a requirement outside what a headless session
// in a worktree can do: reach a host, deploy, or run against a live system.
// Every field project's longest review loops were requirements like these,
// sent back round after round because no implementing session could meet them.
// It matches phrases rather than words, since "guest", "staging" and "ssh"
// alone name ordinary code: guest users, git staging, an ssh config parser.
var liveWork = regexp.MustCompile(`(?i)\b(systemctl|tailscale|` +
	`(?:over|via|through) ssh|ssh (?:to|into|onto)|scp (?:to|from)|` +
	`on the (?:test |dev |staging |live )?(?:guest|host|server|vm|box)\b|test guests?|` +
	`live (?:host|server|system|run|check)|staging (?:run|host|server|environment)|production (?:host|server|run)|` +
	`deploy(?:s|ed|ing)? (?:to|on)|dev (?:run|server|host|box|environment)|real (?:host|device|guest))`)

// postMerge is wording that puts a step after the merge.
var postMerge = regexp.MustCompile(`(?i)\b(after (?:the |this |it )?(?:merge[sd]?|merging|deploy(?:s|ed|ing|ment)?)|post-merge|once (?:it is |this is |it's )?merged)\b`)

// codeSpan is a backticked span, and cliFlag a command-line flag inside one.
var (
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	cliFlag  = regexp.MustCompile(`(?:^|\s)--?([a-zA-Z][a-zA-Z0-9-]{3,})\b`)
)

// operatorTiming is each [operator] requirement of the body that cannot be met
// before the merge: one saying it happens after it, and one running a flag the
// trunk has nowhere, which only this branch's build can run.
//
// An [operator] requirement holds the merge until it is signed off, so both
// leave the item waiting on a step that waits on the merge. The flag test is a
// fixed-string grep for the flag's name over the trunk, so it misses a flag
// whose name the trunk spells in another sense, and a trunk drover cannot read
// reports nothing.
func (s *session) operatorTiming(lines []string) []liveHit {
	body := lines[bodyStart(lines):]
	blocks := requirementBlocks(body)
	var out []liveHit
	for _, r := range (backlog.Item{Body: strings.Join(body, "\n")}).Requirements() {
		if !r.Operator {
			continue
		}
		text := blocks[r.Num]
		if text == "" {
			text = r.Text
		}
		if m := postMerge.FindString(text); m != "" {
			out = append(out, liveHit{r.Num, fmt.Sprintf("says %q", m)})
			continue
		}
		if flag := s.newFlag(text); flag != "" {
			out = append(out, liveHit{r.Num, fmt.Sprintf("runs %s, which %s does not have anywhere, so only this branch's build can run it", flag, s.driftRef())})
		}
	}
	return out
}

// resolves reports whether ref names a commit here. A tree with no trunk to
// read has no evidence about a citation either way.
func (s *session) resolves(ref string) bool {
	return gitcmd.Command(s.cfg.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}

// newFlag is the first backticked flag in text whose name the trunk does not
// hold, or empty.
func (s *session) newFlag(text string) string {
	ref := s.driftRef()
	if !s.cfg.Git.On() || !s.resolves(ref) {
		return ""
	}
	for _, span := range codeSpan.FindAllStringSubmatch(text, -1) {
		for _, m := range cliFlag.FindAllStringSubmatch(span[1], -1) {
			err := gitcmd.Command(s.cfg.Root, "grep", "-q", "-F", "-e", m[1], ref, "--").Run()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return strings.TrimSpace(m[0])
			}
		}
	}
	return ""
}

// acceptDrift is each citation in it's body the trunk cannot hold, read the
// way dispatch reads it, and the unmerged dependencies whose merge could
// supply one. A dependent may cite a file its dependency creates, and that
// citation comes true before the dependent runs, so with a dependency still
// open drift is a note rather than a refusal; a citation that dependency's
// branch already holds is not reported at all.
func (s *session) acceptDrift(it backlog.Item) (drift, waiting []string) {
	if !s.cfg.Git.On() || !s.resolves(s.driftRef()) {
		return nil, nil
	}
	var branches []string
	for _, id := range it.Deps() {
		if s.done(id) {
			continue
		}
		waiting = append(waiting, id)
		if d, err := s.item(id); err == nil {
			if b := s.branchRef(d); b != "" {
				branches = append(branches, b)
			}
		}
	}
	held := map[string]bool{}
	for _, b := range branches {
		missing := map[string]bool{}
		for _, d := range s.drifted(it, b) {
			missing[d.cite] = true
		}
		for _, r := range citedRefs(it.Body) {
			if !missing[r.Text] {
				held[r.Text] = true
			}
		}
	}
	for _, d := range s.drifted(it, s.driftRef()) {
		if !held[d.cite] {
			drift = append(drift, d.msg)
		}
	}
	return drift, waiting
}

// liveHit is one requirement liveWork matched, and the words it matched.
type liveHit struct {
	Num  int
	what string
}

// liveRequirements is each numbered requirement of the body that liveWork
// matches and that does not already open with [operator]. It reads each
// requirement's whole block, its first line and every line up to the next
// requirement or heading, since a requirement wrapped onto a second line
// says the live part there as often as not.
func liveRequirements(lines []string) []liveHit {
	body := lines[bodyStart(lines):]
	blocks := requirementBlocks(body)
	var out []liveHit
	for _, r := range (backlog.Item{Body: strings.Join(body, "\n")}).Requirements() {
		if r.Operator {
			continue
		}
		text := blocks[r.Num]
		if text == "" {
			text = r.Text
		}
		if m := liveWork.FindString(text); m != "" {
			out = append(out, liveHit{r.Num, m})
		}
	}
	return out
}

// topRequirement is a top-level numbered line, the form Requirements reads.
var topRequirement = regexp.MustCompile(`^(\d+)\. `)

// requirementBlocks is each top-level requirement's text with the lines that
// continue it, keyed by its number, outside fenced blocks.
func requirementBlocks(lines []string) map[int]string {
	out := map[int]string{}
	cur := 0
	gap := false
	var f fence
	for _, l := range lines {
		if f.skip(l) {
			continue
		}
		if m := topRequirement.FindStringSubmatch(l); m != nil {
			cur, _ = strconv.Atoi(m[1])
			out[cur] = l
			gap = false
			continue
		}
		if headingDepth(l) > 0 {
			cur = 0
			continue
		}
		if strings.TrimSpace(l) == "" {
			gap = true
			continue
		}
		// Past a blank line only an indented line still belongs to the list
		// item; a paragraph at the margin is prose after the list, which is
		// where bodies put operator steps and the post-finish Live check line.
		if gap && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			cur = 0
		}
		gap = false
		if cur > 0 {
			out[cur] += "\n" + l
		}
	}
	return out
}

// prefixedHint adds, to the refusal for a planned id in a project whose items
// all carry a namespace prefix, the prefixed ids ending in that number. An
// operator typing 35 for E35 meant an item that exists, and a refusal naming
// only the planned range leaves them to work out which.
func (s *session) prefixedHint(bare string, err error) error {
	id, perr := backlog.ParseID(bare, s.cfg.Prefixes())
	if perr != nil || !id.Planned() || len(s.backlog.Items) == 0 {
		return err
	}
	var near []string
	for _, it := range s.backlog.Items {
		if it.ID.Planned() {
			return err
		}
		if it.ID.Num == id.Num && it.ID.Sub == id.Sub {
			near = append(near, it.ID.Bare())
		}
	}
	if len(near) == 0 {
		return err
	}
	return fmt.Errorf("%w.\nEvery item here carries a prefix, and %s %s in %s: drover accept %s",
		err, strings.Join(near, " and "), plural(len(near), "ends", "end"), bare, near[0])
}

// citedDirs is every path overlapPaths finds in body that is a directory in
// the project tree. Dispatch holds such an item apart from every item citing a
// path below it, and cannot tell a directory named for context from one the
// item will edit, so accept tells the writer while the body is still theirs to
// change.
func (s *session) citedDirs(body string) []string {
	var dirs []string
	for _, p := range overlapPaths(body) {
		if fi, err := os.Stat(filepath.Join(s.cfg.Root, p)); err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// topDirs is the subset of citedDirs at the top level of the repository: the
// citations that hold an item apart from most of the tree. check and new name
// these before accept, since accept is the moment the body stops being the
// author's to rewrite freely.
func (s *session) topDirs(body string) []string {
	var top []string
	for _, d := range s.citedDirs(body) {
		if !strings.Contains(d, "/") {
			top = append(top, d)
		}
	}
	return top
}

// dirNote is the consequence of citing dir, the sentence accept, check and new
// all print.
func dirNote(id, dir string) string {
	return fmt.Sprintf("%s cites the directory %s; under drive --jobs it waits for any item citing a path below it", id, dir)
}

// placeholderSlot is an angle-bracket slot an author meant to fill in: <provider>,
// <role>. Letters, digits and hyphens only, so <!-- --> and <https://...> are
// not one.
var placeholderSlot = regexp.MustCompile(`<[A-Za-z0-9-]+>`)

// loneTag is a code span holding nothing but one tag, the form a body for a web
// project writes when it cites what renders: `<h1>`, `<Dialog>`.
var loneTag = regexp.MustCompile(`^<([A-Za-z0-9-]+)>$`)

// markupElements is every element name in the WHATWG HTML Living Standard's
// element index (html.spec.whatwg.org/multipage/indices.html#elements-3) and the
// SVG 2 element index (www.w3.org/TR/SVG2/eltindex.html), lowercased, since a
// span naming one is citing markup rather than leaving a slot to fill.
var markupElements = map[string]bool{}

func init() {
	for _, name := range strings.Fields(`
		a abbr address area article aside audio b base bdi bdo blockquote body br
		button canvas caption cite code col colgroup data datalist dd del details
		dfn dialog div dl dt em embed fieldset figcaption figure footer form h1 h2
		h3 h4 h5 h6 head header hgroup hr html i iframe img input ins kbd label
		legend li link main map mark math menu meta meter nav noscript object ol
		optgroup option output p picture pre progress q rp rt ruby s samp script
		search section select selectedcontent slot small source span strong style sub summary sup
		svg table tbody td template textarea tfoot th thead time title tr track u
		ul var video wbr

		animate animatemotion animatetransform circle clippath defs desc discard
		ellipse feblend fecolormatrix fecomponenttransfer fecomposite
		feconvolvematrix fediffuselighting fedisplacementmap fedistantlight
		fedropshadow feflood fefunca fefuncb fefuncg fefuncr fegaussianblur
		feimage femerge femergenode femorphology feoffset fepointlight
		fespecularlighting fespotlight fetile feturbulence filter foreignobject g
		image line lineargradient marker mask metadata mpath path pattern polygon
		polyline radialgradient rect set stop switch symbol text textpath tspan
		use view`) {
		markupElements[name] = true
	}
}

// citesMarkup reports whether a code span's content is one tag naming an HTML
// or SVG element, or a JSX component, which by convention starts uppercase.
func citesMarkup(content string) bool {
	m := loneTag.FindStringSubmatch(content)
	if m == nil {
		return false
	}
	name := m[1]
	return markupElements[strings.ToLower(name)] || (name[0] >= 'A' && name[0] <= 'Z')
}

// bodyHold is one line that says a body is not finished.
type bodyHold struct {
	// line is 1-based within the lines handed to bodyHolds.
	line int
	what string
	// marker is set for a hold-marker line, unset for a placeholder.
	marker bool
}

// bodyHolds is every line from the body start on that carries a placeholder
// or a hold marker. A fenced block is a syntax example and is skipped whole.
// So is an inline code span holding a space, which is a command example
// (`drover show <id>`), and one holding nothing but an element or component
// tag (`<h1>`, `<Dialog>`). Any other span is a path the run will be told to
// edit (`internal/<provider>/client.go`), and is read like prose.
func bodyHolds(lines []string, markers []string) []bodyHold {
	var out []bodyHold
	var f fence
	for i := bodyStart(lines); i < len(lines); i++ {
		if f.skip(lines[i]) {
			continue
		}
		text := withoutExampleSpans(lines[i])
		for _, m := range markers {
			if m != "" && strings.Contains(text, m) {
				out = append(out, bodyHold{i + 1, fmt.Sprintf("the hold marker %q", m), true})
			}
		}
		for _, p := range placeholderSlot.FindAllString(text, -1) {
			out = append(out, bodyHold{i + 1, "the placeholder " + p, false})
		}
	}
	return out
}

// withoutExampleSpans blanks every inline code span on line whose content
// holds a space or cites markup, leaving the rest as written. A span opens on a run of
// backticks and closes on the next run of the same length; an unclosed run is
// literal text.
func withoutExampleSpans(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		tick := line[i:j]
		end := closingRun(line, j, len(tick))
		if end < 0 {
			b.WriteString(tick)
			i = j
			continue
		}
		if content := line[j:end]; !strings.Contains(content, " ") && !citesMarkup(content) {
			b.WriteString(tick + content + tick)
		}
		i = end + len(tick)
	}
	return b.String()
}

// closingRun is the index of the next run of exactly n backticks at or after
// from, or -1.
func closingRun(line string, from, n int) int {
	for i := from; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}

var unacceptCmd = &cobra.Command{
	Use:   "unaccept <id>",
	Short: "Restore the stop marker, returning an item to a scaffold.",
	Long: `The withdrawal of accept, for a contract that turned out not to be one. The
whole block goes back at the top of the body where intake writes it, marker and
explanation both, so an item accepted and then unaccepted is byte-identical to
the scaffold it came from and draft will take it again. A body whose block a
human deleted by hand gets the explanation back with the marker.

Idempotent, and it refuses an item the ledger records done: an item that merged
is not a scaffold, and unrunning a completion is unmark-merged's job.`,
	Example: `  drover unaccept B1                # back to a scaffold; draft will take it again
  drover unaccept B1                # again: already a scaffold, exit 0

  drover unaccept B1                # on a merged item: refused, exit 3
  drover unmark-merged B1           # withdraw the completion first, then unaccept`,
	Args: needID("unaccept <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		if err := s.markerConfigured(); err != nil {
			return err
		}
		id := it.ID.Bare()
		if s.done(id) {
			return Refusal{fmt.Errorf(
				"%s is recorded done, so it is merged work rather than a scaffold.\nWithdraw the completion first: drover unmark-merged %s", id, id)}
		}
		lines, err := bodyLines(it.Path)
		if err != nil {
			return err
		}
		marker := s.cfg.Intake.StopMarker
		if markerLine(strings.Join(lines, "\n"), marker) >= 0 {
			fmt.Printf("already a scaffold: %s carries the stop marker (%s)\n",
				id, rel(s.cfg.Root, it.Path))
			s.mirrorAround(it)
			return nil
		}
		at := bodyStart(lines)
		restored := append(lines[:at:at], append(stopBlock(marker), lines[at:]...)...)
		if err := s.writeBody(it, "unaccept", restored); err != nil {
			return err
		}
		fmt.Printf("unaccepted: %s (stop marker restored in %s)\n", id, rel(s.cfg.Root, it.Path))
		s.mirrorReloaded(id, false)
		return nil
	},
}

// markerConfigured refuses both verbs in a project that declares no marker.
// Nothing here has a marker to write, delete or restore, and inventing one the
// config does not name plants a line no other command reads: a scaffold whose
// marker `run` cannot see is an item that launches with an empty numbered list.
func (s *session) markerConfigured() error {
	if s.cfg.Intake.StopMarker != "" {
		return nil
	}
	return Refusal{errors.New(
		"this project declares no [intake] stop_marker, so there is no marker to write, accept or restore.\nSet one in .drover.toml if scaffolded items should stay inert until a human accepts them")}
}

// stopBlockLen is how many lines of the block intake wrote survive at index
// at. The marker line is the only one guaranteed to be there.
//
// Where the block ends is decided by matching intake's own continuation text,
// not by scanning to the next blank line. A marker somebody typed by hand can
// have real prose directly under it, and a scan to the blank would delete that
// paragraph; matching the text removes only lines drover authored and leaves
// anything else where its author put it.
func stopBlockLen(lines []string, at int, marker string) int {
	block := stopBlock(marker)
	n := 1
	for n < len(block) && at+n < len(lines) && lines[at+n] == block[n] {
		n++
	}
	return n
}

// bodyLines reads a body as lines, splitting on "\n" so that joining them back
// reproduces the file exactly, trailing newline included. Both verbs edit a few
// lines of somebody's prose and must be unable to touch the rest.
func bodyLines(path string) ([]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path came from the backlog
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// writeBody rewrites a body in place and records the digest of what it wrote.
// The mode is only applied on creation, so a file already on disk keeps
// whatever permissions it had.
func (s *session) writeBody(it backlog.Item, cmd string, lines []string) error {
	written := []byte(strings.Join(lines, "\n"))
	if err := os.WriteFile(it.Path, written, 0o600); err != nil {
		return err
	}
	return s.wroteBody(it.ID.Bare(), cmd, written)
}

// bodyStart is the index of the first body line: past the closing frontmatter
// fence and the blank lines under it, which is where intake plants the marker.
// A file whose frontmatter does not parse gets index 0 rather than an error —
// the backlog would have refused it long before here.
func bodyStart(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		start := i + 1
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
		return start
	}
	return 0
}

func init() {
	acceptCmd.Flags().BoolVar(&acceptYes, "yes", false,
		"accept past a refusal about the body's content (a placeholder, a hold line, a citation only a waiting item's branch has); never past the stop-marker or actor checks")
	root.AddCommand(acceptCmd, unacceptCmd)
}
