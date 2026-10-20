package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/idalloc"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

var (
	intakeKind string
	intakeList bool
	intakeJSON bool
)

var intakeCmd = &cobra.Command{
	Use:   "intake <issue-number>",
	Short: "Turn a tracker issue into a scaffolded work item.",
	Long: `Reads the issue, allocates the next free id in the matching namespace, writes a
body with the frontmatter filled and the issue quoted as raw material. The
body's issue key is the link back: the issue becomes the item's own, and no
label is written to say so.

It does NOT write the requirements. The scaffold carries a stop marker and
drover refuses to launch, or even preview, while it is present. Those numbered
requirements become the audit checklist, so one lifted mechanically from a bug
report is a checkbox nobody can falsify. A human writes them.

An issue carrying the follow-up label is a follow-up, and taking one in
promotes it: drover drops the follow-up label in the same act, so it leaves
` + "`drover follow-ups`" + ` as it enters the backlog. Nothing may sit in both sets.

Two rules on that path. A follow-up still owned by an item the ledger does not
record as merged is refused with exit 3, naming the owner — that run already
carries it, and a second item duplicates it. And a follow-up whose only label is
the follow-up one matches no namespace, so --kind is required: the follow-up
label is membership, not a kind, and drover will not rank the promotion for you.

--list reads instead of writing: the import queue, every open issue carrying a
label that matches an id namespace minus the ones a body already claims. An
owned issue stays on it until it is taken in. Its owner link (a sub-issue parent
that is an item's issue, or an Owner: line) says which item owns the gap, which
is what an audit filed it against; it does not say anybody has scheduled it. An
issue carrying neither the follow-up label nor a namespace kind is the tracker's
business and appears neither there nor in ` + "`drover follow-ups`" + `.`,
	Example: `  drover intake 12                  # issue #12 becomes a scaffolded item
  drover intake 12 --kind bug       # override the kind its labels imply
  drover intake 68 --kind bug       # promote an orphaned follow-up to its own item

  Idempotent: re-running names the item it already wrote rather than allocating a
  second id, and re-attempts the follow-up label removal when the issue still
  carries it, so a promotion whose label write failed is repaired by running it again.
  The scaffold then blocks run AND run --dry-run until a human writes the
  numbered requirements and deletes the stop marker.

  drover intake --list              # what the tracker holds that drover has not
  drover intake --list --json       # one object per issue:
    [{"number":324,"title":"deleting a group leaves a solicitation",
      "kind":"bug","severity":"medium","owners":["2b"]}]`,
	Args: func(c *cobra.Command, args []string) error {
		if intakeList {
			return cobra.NoArgs(c, args)
		}
		return needIssue("intake <issue-number>",
			"that is your tracker's number, not a drover id: drover intake 12")(c, args)
	},
	RunE: func(_ *cobra.Command, args []string) error {
		if intakeList {
			return listIntake()
		}
		if intakeJSON {
			return fmt.Errorf("--json goes with --list: intake <issue-number> prints what it wrote")
		}
		s, err := open()
		if err != nil {
			return err
		}
		number, err := parsePositive(args[0])
		if err != nil {
			return err
		}
		// Refused before gh is asked anything, and keyed on the register backend
		// rather than on the tracker being reachable: a project declaring
		// backend = "file" has no tracker either, and without one the repository
		// gh infers is whatever tree encloses this one, so labelling an issue
		// there is a write on somebody else's project.
		if s.cfg.RegisterBackend() != config.BackendGitHub {
			return Refusal{fmt.Errorf(
				"%s declares %s, so there is no tracker to take issue #%d from.\n"+
					"Hand-write the body instead: drover only ever reads the file",
				s.cfg.Project, s.cfg.RegisterDeclaration(), number)}
		}
		if !gh.Available() {
			return fmt.Errorf("intake needs gh on PATH and `gh auth login`.\n" +
				"Or hand-write the body: drover only ever reads the file")
		}

		// Idempotency keys on the body file, not the tracker label: labelling can
		// fail after the file has landed, and the file is what drover reads. The
		// label is not abandoned there — repairPromotion re-attempts it, so a
		// promotion whose removal failed is repaired by running intake again.
		if path, readable, ok := s.itemForIssue(number); ok {
			note := ""
			if !readable {
				note = " (the body does not parse — drover check says why)"
			}
			fmt.Printf("issue %s is already intaken: %s%s\n", s.issueRefTo(os.Stdout, number), rel(s.cfg.Root, path), note)
			s.repairPromotion(number)
			return nil
		}

		issue, err := gh.View(s.labels(), number)
		if err != nil {
			return err
		}
		if issue.State != "OPEN" {
			fmt.Fprintf(os.Stderr, "note: issue %s is %s — scaffolding anyway\n", s.issueRefTo(os.Stderr, number), issue.State)
		}
		promoting := s.isFollowUp(issue)
		if promoting {
			if err := s.promotable(issue); err != nil {
				return err
			}
		}

		prefix, kind, err := s.namespaceFor(issue)
		if err != nil {
			return err
		}
		slug := slugify(issue.Title)
		if slug == "" {
			slug = fmt.Sprintf("issue-%d", number)
		}
		id, path, err := s.mint(prefix, slug, s.scaffold(kind, number, issue, slug), nil)
		if err != nil {
			return err
		}

		fmt.Printf("\nscaffolded %s -> %s  (issue %s, %s)\n\n", id.Bare(), rel(s.cfg.Root, path), s.issueRefTo(os.Stdout, number), kind)
		if promoting {
			if err := s.dropFollowUpLabel(number); err != nil {
				fmt.Fprintf(os.Stderr,
					"warn: %v — the body is still valid, but issue %s still carries %q and drover follow-ups still lists it.\n"+
						"Run intake again to re-attempt the removal\n",
					err, s.issueRefTo(os.Stderr, number), s.cfg.GitHub.FollowUpLabel)
			}
		}
		fmt.Printf("NEXT — a human writes the numbered requirements:\n")
		fmt.Printf("  $EDITOR %s\n", rel(s.cfg.Root, path))
		fmt.Printf("  then delete the stop marker and run: drover run %s --dry-run\n", id.Bare())
		return nil
	},
}

// dropFollowUpLabel takes the issue off the follow-up list and announces it. The
// caller says what a failure means from where it is standing, because the first
// run has a scaffold to reassure the operator about and a repair does not.
func (s *session) dropFollowUpLabel(number int) error {
	label := s.cfg.GitHub.FollowUpLabel
	if err := gh.RemoveLabel(s.labels(), number, label); err != nil {
		return err
	}
	fmt.Printf("dropped %q from issue %s: it is a backlog item now, not a follow-up\n\n", label, s.issueRefTo(os.Stdout, number))
	return nil
}

// repairPromotion finishes a promotion whose label removal did not land.
//
// intake short-circuits on the body file, so without this the second run is a
// no-op against an issue on the follow-up list and in the backlog at once, which
// is the one state membership exists to prevent. The tracker is re-asked rather
// than the first run's outcome trusted: somebody may have dropped the label by
// hand in between, and an unconditional removal would report work it did not do.
//
// promotable is deliberately not consulted. It guards the decision to promote,
// which is already made and on disk, and the issue's owners may still include
// one that has not merged, so asking again would refuse every repair.
func (s *session) repairPromotion(number int) {
	if s.cfg.GitHub.FollowUpLabel == "" {
		return
	}
	issue, err := gh.View(s.labels(), number)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warn: %v — drover could not check whether issue %s is still a follow-up\n", err, s.issueRefTo(os.Stderr, number))
		return
	}
	if !s.isFollowUp(issue) {
		return
	}
	if err := s.dropFollowUpLabel(number); err != nil {
		label := s.cfg.GitHub.FollowUpLabel
		fmt.Fprintf(os.Stderr,
			"warn: %v — issue %s is a follow-up and a backlog item at once.\n"+
				"Drop it by hand: gh issue edit %d --remove-label %q\n",
			err, s.issueRefTo(os.Stderr, number), number, label)
	}
}

// isFollowUp reports that this issue is a follow-up rather than an ordinary
// tracker issue. Taking one in is a promotion: it leaves the follow-up list in
// the same act, so the two sets stay disjoint.
func (s *session) isFollowUp(issue gh.Issue) bool {
	label := s.cfg.GitHub.FollowUpLabel
	return label != "" && slices.Contains(issue.Labels, label)
}

// promotable refuses a follow-up an open item is already scheduled to carry.
//
// An owned follow-up travels with its owner: the run implementing that id
// inherits it and is expected to discharge it. Promoting it to an item of its
// own while the owner is open writes a second unit of work for the same gap,
// and neither one knows about the other. Only a follow-up every owner has
// merged past has lost its run, and an unowned one never had one.
func (s *session) promotable(issue gh.Issue) error {
	var live []string
	for _, id := range issue.Owners {
		if !s.done(id) {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		return nil
	}
	sort.Strings(live)
	owners := strings.Join(live, ", ")
	return Refusal{fmt.Errorf(
		"issue #%d is a follow-up owned by %s, which the ledger does not record as merged.\n"+
			"That run is already scheduled to carry it; a second item would duplicate the work.\n"+
			"Take it in once %s has finished, or correct its owner link (sub-issue parent or %q line) if it names the wrong item",
		issue.Number, owners, owners, gh.OwnerLine(live[0]))}
}

// itemForIssue finds an existing body already claiming this issue number.
//
// Unreadable bodies are searched too, by reading the raw file. Scanning parsed
// items alone would leave a scaffold whose frontmatter was broken by hand
// invisible: intake would read it as a fresh issue and allocate a second id,
// putting two files under one issue with nothing detecting it. `nextID` counts
// those bodies for the same reason.
//
// The shelf is deliberately not searched. Its contract is that drover reads ids
// from the filenames and never opens the files, and a shelved item is one
// somebody decided not to do — taking its issue in again is a new decision, not
// a duplicate.
func (s *session) itemForIssue(number int) (path string, readable, ok bool) {
	want := fmt.Sprintf("%d", number)
	for _, it := range s.backlog.Items {
		if it.Get("issue") == want {
			return it.Path, true, true
		}
	}
	for _, b := range s.backlog.Broken {
		if rawFrontmatterValue(b.Path, "issue") == want {
			return b.Path, false, true
		}
	}
	return "", false, false
}

// rawFrontmatterValue reads one key out of a body whose frontmatter did not
// parse. It stops at the closing fence so a line in the prose cannot answer.
func rawFrontmatterValue(path, key string) string {
	data, err := os.ReadFile(path) // #nosec G304 -- a path drover found in its own backlog
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return ""
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// namespaceFor picks the id namespace from the issue's labels, or from --kind.
//
// When labels match more than one namespace the strictest wins, which is the
// one whose kind sorts first: a bug carries a stricter definition of done than
// an enhancement, and reading it as the looser one loses that.
func (s *session) namespaceFor(issue gh.Issue) (string, string, error) {
	byKind := map[string]string{}
	for prefix, kind := range s.cfg.IDs.Namespaces {
		byKind[kind] = prefix
	}
	if intakeKind != "" {
		prefix, ok := byKind[intakeKind]
		if !ok {
			return "", "", fmt.Errorf("unknown kind %q (declared: %s)", intakeKind, strings.Join(sortedKeys(byKind), ", "))
		}
		return prefix, intakeKind, nil
	}

	var matched []string
	for _, l := range issue.Labels {
		if _, ok := byKind[l]; ok {
			matched = append(matched, l)
		}
	}
	if len(matched) == 0 {
		// A follow-up carries the follow-up label and commonly nothing else, so
		// this refusal is on the promotion path rather than the exception there.
		// No kind is defaulted: the namespace decides which definition of done
		// the item is held to, and a promotion drover ranked itself would be a
		// guess nothing downstream marks as one.
		if s.isFollowUp(issue) {
			kinds := sortedKeys(byKind)
			hint := ""
			if len(kinds) > 0 {
				hint = fmt.Sprintf("\n  drover intake %d --kind %s", issue.Number, kinds[0])
			}
			return "", "", Refusal{fmt.Errorf(
				"issue #%d is a follow-up carrying no label matching a namespace (%s).\n"+
					"The %q label is membership, not a kind, so promoting it cannot infer one: --kind is required.%s",
				issue.Number, strings.Join(kinds, ", "), s.cfg.GitHub.FollowUpLabel, hint)}
		}
		return "", "", Refusal{fmt.Errorf(
			"issue #%d carries no label matching a namespace (%s); pass --kind",
			issue.Number, strings.Join(sortedKeys(byKind), ", "))}
	}
	sort.Strings(matched)
	return byKind[matched[0]], matched[0], nil
}

// localAlloc is --local on new, intake and split: allocate from disk for this
// one call whatever [ids] allocator says.
var localAlloc bool

func localFlag(c *cobra.Command) {
	c.Flags().BoolVar(&localAlloc, "local", false,
		`allocate the id from disk for this call, even under [ids] allocator = "git"`)
}

// idPushHook runs between the counter's fetch and push. Nil outside tests.
var idPushHook func(attempt int)

// idDeadline bounds one git allocation, every attempt included, so a remote
// that never answers costs the backlog lock this long and no longer. A var so a
// test can shorten it.
var idDeadline = 30 * time.Second

// mint allocates the next id under prefix and creates its body, as one step
// under the backlog's lock.
//
// The lock spans the read of the highest id and the write of the file. Without
// it two processes on one host read the same highest number and write two
// bodies under one id, which an existence check on the full path cannot see
// because the slugs differ. The body is created O_EXCL as well, so a collision
// the lock cannot prevent (a platform without flock) fails rather than
// overwrites.
//
// vet sees the id before anything is written, and may refuse it.
func (s *session) mint(prefix, slug, body string, vet func(backlog.ID) error) (backlog.ID, string, error) {
	dir := s.cfg.Abs(s.cfg.Paths.Backlog)
	unlock, err := idalloc.Lock(dir)
	if err != nil {
		return backlog.ID{}, "", fmt.Errorf("lock %s: %w", dir, err)
	}
	defer unlock()

	highest, err := s.highestIDs()
	if err != nil {
		return backlog.ID{}, "", err
	}
	id := backlog.ID{Prefix: prefix, Num: highest[prefix] + 1}
	if s.cfg.IDAllocator() == config.AllocatorGit && !localAlloc {
		counter := idalloc.Counter{Root: s.cfg.Root, Remote: s.cfg.IDRemote(), Log: os.Stderr, BeforePush: idPushHook, Timeout: idDeadline}
		if id.Num, err = counter.Next(prefix, highest); err != nil {
			return backlog.ID{}, "", allocRefusal(err)
		}
	}
	if vet != nil {
		if err := vet(id); err != nil {
			return backlog.ID{}, "", err
		}
	}

	path := filepath.Join(dir, id.Stem()+"-"+slug+".md")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path built from the backlog and a slug
	if errors.Is(err, fs.ErrExist) {
		return backlog.ID{}, "", Refusal{fmt.Errorf("refusing to overwrite %s", path)}
	}
	if err != nil {
		return backlog.ID{}, "", fmt.Errorf("write scaffold: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return backlog.ID{}, "", fmt.Errorf("write scaffold: %w", err)
	}
	if err := f.Close(); err != nil {
		return backlog.ID{}, "", fmt.Errorf("write scaffold: %w", err)
	}
	return id, path, nil
}

// allocRefusal says what a failed git allocation means and what to run instead.
// drover never falls back to disk on its own, because a silent fallback is the
// race the counter exists to close.
func allocRefusal(err error) error {
	var down idalloc.Unreachable
	if errors.As(err, &down) {
		return Refusal{fmt.Errorf("%w.\n"+
			"[ids] allocator = %q reserves every id on %s before writing, and nothing was written.\n"+
			"Retry once it is reachable, or pass --local to allocate from disk for this one call, "+
			"which can reissue an id another host has already taken", down, config.AllocatorGit, down.Remote)}
	}
	var slow idalloc.TimedOut
	if errors.As(err, &slow) {
		return Refusal{fmt.Errorf("%w.\n"+
			"[ids] allocator = %q reserves every id on %s before writing, and nothing was written.\n"+
			"Retry once it answers, or pass --local to allocate from disk for this one call, "+
			"which can reissue an id another host has already taken", slow, config.AllocatorGit, slow.Remote)}
	}
	var refused idalloc.Rejected
	if errors.As(err, &refused) {
		return Refusal{fmt.Errorf("%w.\n"+
			"The remote answered and refused the ref, so this is a rule or a permission on %s for %s, not a host that is down; nothing was written.\n"+
			"Allow the push there, or pass --local to allocate from disk for this one call, "+
			"which can reissue an id another host has already taken", refused, refused.Remote, idalloc.Ref)}
	}
	var busy idalloc.Contended
	if errors.As(err, &busy) {
		return Refusal{fmt.Errorf("%w.\nNothing was written; run the command again", busy)}
	}
	return err
}

// highestIDs reads the highest number per prefix from the backlog and the
// shelf as they stand now, not as open loaded them: under mint's lock the
// difference is another process's body.
//
// The shelf counts. A retired body still spends its id, and reissuing it puts
// two files under one identity in different directories, which nothing else
// detects: the duplicate check in backlog.Load is built while reading a single
// directory.
//
// So does an unreadable body. Its id was authored in the filename and is spent
// whether the frontmatter parses or not; handing it out again puts two files
// under one identity in the same directory, where SHELF CLASH cannot see them
// either.
func (s *session) highestIDs() (map[string]int, error) {
	prefixes := s.cfg.Prefixes()
	c, err := backlog.Load(s.cfg.Abs(s.cfg.Paths.Backlog), prefixes, s.cfg)
	if err != nil {
		return nil, err
	}
	shelf, err := backlog.LoadShelf(s.cfg.Abs(s.cfg.Paths.Shelf), prefixes)
	if err != nil {
		return nil, err
	}
	highest := make(map[string]int, len(prefixes))
	for _, p := range prefixes {
		highest[p] = shelf.Highest(p)
	}
	raise := func(id backlog.ID) {
		if n, ok := highest[id.Prefix]; ok && id.Num > n {
			highest[id.Prefix] = id.Num
		}
	}
	for _, it := range c.Items {
		raise(it.ID)
	}
	for _, b := range c.Broken {
		if id, ok := b.ID(prefixes); ok {
			raise(id)
		}
	}
	return highest, nil
}

// scaffold is scaffoldBody plus the issue text, quoted as evidence rather than
// as requirements.
func (s *session) scaffold(kind string, number int, issue gh.Issue, _ string) string {
	var b strings.Builder
	// The issue key is intake's alone: it is what makes a re-run idempotent, and
	// a hand-written item has no issue to name.
	b.WriteString(s.scaffoldBody(kind, issue.Title, map[string]string{
		"issue":            fmt.Sprintf("%d", number),
		s.cfg.Severity.Key: seedSeverity(kind, issue.Severity),
	}))
	fmt.Fprintf(&b, "--- raw material: issue #%d — evidence, NOT requirements ---\n\n", number)
	fmt.Fprintf(&b, "> %s\n>\n", issue.Title)
	for _, line := range strings.Split(strings.ReplaceAll(issue.Body, "\r\n", "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}
	b.WriteString("\n--- end raw material ---\n")
	return b.String()
}

// scaffoldBody is the half of a scaffold that needs no tracker: the frontmatter
// the project declares, the stop marker, and an empty numbered list.
//
// `drover new` writes this and nothing else, so an item authored by hand and one
// taken in from an issue reach `accept` in the same shape. The issue number and
// the quoted body are the only difference, and only intake has them.
//
// The marker is written verbatim from [intake] stop_marker with no fallback.
// Inventing one produced a scaffold whose marker no other command recognised:
// `run` read the configured value, found nothing, and launched an item whose
// requirements were two empty numbers. Both callers refuse before reaching here
// when the key is unset.
func (s *session) scaffoldBody(kind, title string, extra map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	seed := map[string]string{"title": title, "kind": kind}
	for k, v := range extra {
		if v != "" {
			seed[k] = v
		}
	}
	for _, key := range s.declaredKeys() {
		fmt.Fprintf(&b, "%s: %s\n", key, seed[key])
	}
	b.WriteString("---\n\n")

	b.WriteString(strings.Join(stopBlock(s.cfg.Intake.StopMarker), "\n") + "\n")
	b.WriteString("Act as a <role>. <One paragraph in your own words: what is broken, or what\nmust exist, and where in the codebase it lives. Name files.>\n\n")
	b.WriteString("Requirements:\n\n1.\n2.\n\n")
	return b.String()
}

// stopBlock is the marker as intake writes it: the marker line, the two lines
// under it saying what the marker does, and the blank that separates them from
// the body.
//
// accept deletes exactly these lines and unaccept writes exactly these back, so
// all three callers read the text from here. A second copy that drifts from
// this one turns accept into a command that leaves two lines telling every
// reader of the prompt to delete a marker that is already gone.
func stopBlock(marker string) []string {
	return []string{
		marker,
		"drover refuses to launch this item while this line is present. Delete it",
		"once the list below is real.",
		"",
	}
}

// seedSeverity is the rank a scaffold carries before anybody reads the code.
//
// The tracker already ranked an intaken issue, and gh.Issue.Severity carries
// whatever rode the severity_prefix label. Medium is the fallback for an ungraded
// bug rather than an assessment; a Critical scaffolded as Medium reads as one
// somebody made, and nothing downstream marks it as a guess.
func seedSeverity(kind, graded string) string {
	if r := sev.Of(graded); r != sev.Unranked {
		return r.Letter()
	}
	if kind == "bug" {
		return "M"
	}
	return ""
}

// declaredKeys is required then optional, each in declaration order, so a
// scaffold's frontmatter reads the way the project wrote its schema.
func (s *session) declaredKeys() []string {
	out := append([]string(nil), s.cfg.Frontmatter.Required...)
	return append(out, s.cfg.Frontmatter.Optional...)
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify makes a filename component from a title.
func slugify(title string) string {
	s := slugUnsafe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return strings.Trim(s, "-")
}

func parsePositive(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("issue must be a number, got %q", s)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("issue must be a positive number, got %q", s)
	}
	return n, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	intakeCmd.Flags().StringVar(&intakeKind, "kind", "", "override the kind inferred from the issue's labels")
	intakeCmd.Flags().BoolVar(&intakeList, "list", false, "list the open tracker issues drover could take in and has not; reads only")
	intakeCmd.Flags().BoolVar(&intakeJSON, "json", false, "with --list, emit rows as JSON")
	localFlag(intakeCmd)
	root.AddCommand(intakeCmd)
}
