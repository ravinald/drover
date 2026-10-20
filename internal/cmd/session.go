package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
)

// session is the three things nearly every command needs: where the project
// keeps its work, what that work is, and what has been recorded about it.
type session struct {
	cfg     *config.Config
	backlog *backlog.Backlog
	ledger  *ledger.Ledger
	// shelf is the retired-bodies directory, empty when the project configures
	// none. Two commands consult it and nothing else may: it carries ids, not
	// work, and anything that rendered from it would show a backlog of items
	// nobody intends to do.
	shelf *backlog.Shelf
	// tree is the worktree drive opened for the item in hand, empty everywhere
	// else. Records stay at cfg.Root whatever it holds: tree answers where the
	// work happens, never where it is written down.
	tree string
	// trunkResults is what each gate said about the trunk alone, by trunk commit
	// and gate command, for the life of one drive. Every item caught up to the
	// same trunk commit asks the same question of it.
	trunkResults map[string]trunkResult
	// forgeWait is whether the trunk has required checks, read once per drive:
	// the trunk's protection does not change between one item's merge and the
	// next's.
	forgeWait *forgeWait
	// slot is this item's place in a drive's schedule, nil everywhere else.
	slot *driveSlot
	// inputs is what a drive's session runs under, so a stage boundary can
	// tell a resume that reloaded them from one that changed nothing.
	inputs *driveInputs
	// resumeAt is the boundary an item carried across a drive's re-exec held
	// at, nil for every item the drive dispatched itself.
	resumeAt *lease.Hold
	// forgeURL is the repository's web address once a link needed it: nil until
	// asked, empty when gh could not say.
	forgeURL *string
	// offline is a command told by --no-tracker to ask the forge nothing. A link
	// is then built only from a URL already in hand, never by asking gh for one.
	offline bool
	// zone is a --tz for this one call, nil to take [status] timezone.
	zone *time.Location
	// window is a --window for this one call, zero for defaultWindow.
	window int
	// verifyID and gateMS are a [verify] run's wall time waiting for the next row
	// drive writes for that item, which carries it as verify_ms.
	verifyID string
	verifyMS int64
	// conflict is the trunk merge drive left conflicted in the worktree before
	// the round in hand, nil when the round started clean. The round's prompt,
	// its implement stage row and its review all read it.
	conflict *prompts.Conflict
}

// location is the zone status reports times in.
func (s *session) location() *time.Location {
	if s.zone != nil {
		return s.zone
	}
	return s.cfg.Status.Location()
}

// workDir is the checkout the item in hand is built in: its own worktree under
// drive, the operator's checkout under every other command.
func (s *session) workDir() string {
	if s.tree != "" {
		return s.tree
	}
	return s.cfg.Root
}

// where is what a launched session needs: the checkout to work in, and the
// project to report to, which differ under drive.
func (s *session) where() runner.Where {
	return runner.Where{Dir: s.workDir(), Root: s.cfg.Root}
}

func open() (*session, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return nil, err
	}
	enroll(cfg)
	return openFrom(cfg)
}

// openFrom is open's second half: the work and the record, once the config is
// settled. The two are apart because check has a decision to make between them.
// A project whose own file declares no schema_version loads fine when the
// per-user file declares the current one, and that project has to be reported
// before anything opens a backlog or a ledger: an unreadable one otherwise
// answers first, with an error about a path rather than about the version.
func openFrom(cfg *config.Config) (*session, error) {
	c, err := backlog.Load(cfg.Abs(cfg.Paths.Backlog), cfg.Prefixes(), cfg)
	if err != nil {
		return nil, err
	}
	l, err := ledger.Load(cfg.Abs(cfg.Paths.Ledger))
	if err != nil {
		return nil, err
	}
	sh, err := backlog.LoadShelf(cfg.Abs(cfg.Paths.Shelf), cfg.Prefixes())
	if err != nil {
		return nil, err
	}
	return &session{cfg: cfg, backlog: c, ledger: l, shelf: sh}, nil
}

// record appends a row and keeps the loaded ledger in step, so a command that
// reads the record back after writing it — `run` capturing a report, `finish`
// resolving who to attribute the work to — sees what it just wrote rather than
// the file as it stood when the process started.
func (s *session) record(row ledger.Row) error {
	row = ledger.Stamp(driveStamp(s.verifyStamp(row)))
	if err := ledger.Append(s.cfg.Abs(s.cfg.Paths.Ledger), row); err != nil {
		return err
	}
	s.ledger.Rows = append(s.ledger.Rows, row)
	appended()
	return nil
}

// verifyStamp moves a waiting gate time onto row when row is the gated item's.
// A row for any other item leaves it waiting.
func (s *session) verifyStamp(row ledger.Row) ledger.Row {
	if s.verifyID != "" && row.ID == s.verifyID && row.VerifyMS == 0 {
		row.VerifyMS = s.verifyMS
		s.verifyID, s.verifyMS = "", 0
	}
	return row
}

// recordDone records a completion unless the ledger on disk already holds one,
// and reports whether it wrote. A drive and an operator's finish can record one
// merge from two processes, and each read its ledger before the other wrote, so
// the question is asked again under the ledger's lock. A row it skips means the
// other process wrote first, and the loaded ledger is read again rather than
// left missing that row.
func (s *session) recordDone(row ledger.Row) (bool, error) {
	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	row = ledger.Stamp(driveStamp(s.verifyStamp(row)))
	wrote, err := ledger.AppendUnless(path, row, func(l *ledger.Ledger) bool { return l.State(row.ID).Merged })
	switch {
	case err != nil:
		return false, err
	case wrote:
		s.ledger.Rows = append(s.ledger.Rows, row)
		appended()
		return true, nil
	}
	l, err := ledger.Load(path)
	if err != nil {
		return false, err
	}
	s.ledger = l
	return false, nil
}

// launched records which runner drover resolved for one stage of an item. It
// is written before the launch: a session killed halfway still happened, and a
// row written afterwards would be missing from exactly the runs worth auditing.
//
// A review row also carries the body's digest as the session found it, which
// is what the end of the pass is compared against. An implement row does not:
// an implementer edits the body legitimately and nothing reads that digest.
//
// It returns where the session runs, carrying the launch id the row recorded,
// so the caller hands runner.Run the id the ledger already holds.
func (s *session) launched(it backlog.Item, stage, name string) (runner.Where, error) {
	at := s.where()
	at.Session = runner.NewSession()
	row := ledger.Row{ID: it.ID.Bare(), Stage: stage, Runner: name, Session: at.Session, Date: ledger.Today(),
		Conflict: stage == ledger.StageImplement && s.conflict != nil}
	if stage == ledger.StageReview {
		sum, err := bodyDigest(it.Path)
		if err != nil {
			return at, err
		}
		row.Body = sum
	}
	if err := s.record(row); err != nil {
		return at, err
	}
	if stage == ledger.StageImplement {
		s.mirrorState(it, stateRunning)
	}
	return at, nil
}

// wroteBody records the digest of a body drover just wrote, so the next
// comparison reads the write as drover's own rather than a hand edit. Called
// after the write succeeds, never before: a row for a write that failed would
// vouch for bytes that are not on disk.
func (s *session) wroteBody(id string, written []byte) error {
	return s.record(ledger.Row{ID: id, Body: bodySum(written), Date: ledger.Today()})
}

// bodyDigest is the digest of an item file as it stands on disk.
func bodyDigest(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the path came from the backlog
	if err != nil {
		return "", err
	}
	return bodySum(b), nil
}

// attribution resolves who to credit with the work: the --runner flag, then the
// runner the recorded run actually used, then the project default. The flag
// defaults to empty rather than to a runner name so that "the operator said
// nothing" stays distinguishable here from "the operator named claude".
func (s *session) attribution(flag, id string) string {
	if flag != "" {
		return flag
	}
	if r := s.ledger.State(id).Implementer; r != "" {
		return r
	}
	return s.cfg.Run.DefaultRunner
}

// warnUndeclaredRunner reports a --runner on finish or mark-merged naming nobody the
// project declares.
//
// It warns rather than refuses, which is the split run, draft and review do not
// have. There the value selects an executable and an unknown one cannot launch;
// here it is attribution, and a runner that did the work outside drover has no
// [runners] entry to match. Refusing would make the honest record of an outside
// run impossible to write.
//
// So the only thing left is a typo drover cannot tell from a legitimate outside
// runner. Naming the value and the declared set lets the operator see which it
// was, before the row becomes permanent.
func warnUndeclaredRunner(w io.Writer, flag string, declared map[string]runner.Spec) {
	if flag == "" {
		return
	}
	if _, ok := declared[flag]; ok {
		return
	}
	fmt.Fprintf(w, "warn: --runner %q is not declared in [runners] (declared: %s).\n",
		flag, strings.Join(keys(declared), ", "))
	fmt.Fprintf(w, "      Recording it as given. Attribution takes the name you supply, "+
		"because a runner that ran the work outside drover has no entry to match.\n")
}

// done reports completion for a bare id.
func (s *session) done(id string) bool { return s.ledger.State(id).Merged }

// idState is what drover found behind an id, and the token --json carries for
// it. Its underlying type is the token so that a state and the word for it
// cannot drift apart: one vocabulary, in the report, the brief and the table.
type idState string

const (
	idReadable   idState = "readable"   // a body in the backlog, parsed
	idUnparsed   idState = "unparsed"   // a body on disk whose frontmatter will not parse
	idShelved    idState = "shelved"    // retired to the shelf
	idAbsent     idState = "absent"     // no body in either directory
	idUnresolved idState = "unresolved" // not an id this project can parse
)

// phrase is the sentence for a state with no readable body, empty for one
// drover read. check's wording, so a reader who has seen one report recognizes
// the brief and the table.
func (st idState) phrase() string {
	switch st {
	case idUnparsed:
		return "will not parse"
	case idShelved:
		return "is shelved"
	case idAbsent:
		return "has no body"
	case idUnresolved:
		return "does not resolve to an item id"
	}
	return ""
}

// resolve answers "what is this id", and is the only thing that answers it.
// check, the prompt, show and every command taking an id read it: two
// classifiers over the same states let check call a dep shelved while run
// calls the same dep merged, or shelve call a body missing that check read
// seconds earlier.
//
// The path is where the body sits, empty for the two states with no file.
func (s *session) resolve(bare string) (idState, string) {
	id, err := backlog.ParseID(bare, s.cfg.Prefixes())
	if err != nil {
		return idUnresolved, ""
	}
	if it, ok := s.backlog.Find(id); ok {
		return idReadable, it.Path
	}
	// On disk first: a body in the backlog directory is the one that would render,
	// so repairing it is the fix even when a shelved copy of the id exists.
	if path, ok := s.bodyPath(id); ok {
		return idUnparsed, path
	}
	if path, ok := s.shelf.Path(id); ok {
		return idShelved, path
	}
	return idAbsent, ""
}

// item resolves a bare id to its body, with an error naming what a caller can
// do about a miss.
func (s *session) item(bare string) (backlog.Item, error) {
	id, err := s.parseID(bare)
	if err != nil {
		return backlog.Item{}, err
	}
	state, path := s.resolve(bare)
	if state != idReadable {
		return backlog.Item{}, s.unresolved(id, state, path)
	}
	it, _ := s.backlog.Find(id)
	return it, nil
}

// body resolves a bare id to the file holding it, parsed or not. shelve is why
// this is separate from item: retiring a body that will not read is the case
// that verb exists for, and a rename needs a path rather than a parsed item.
func (s *session) body(bare string) (backlog.ID, string, error) {
	id, err := s.parseID(bare)
	if err != nil {
		return backlog.ID{}, "", err
	}
	state, path := s.resolve(bare)
	if state == idReadable || state == idUnparsed {
		return id, path, nil
	}
	return backlog.ID{}, "", s.unresolved(id, state, path)
}

// parseID reads an id and holds it to the range the project declares.
func (s *session) parseID(bare string) (backlog.ID, error) {
	id, err := backlog.ParseID(bare, s.cfg.Prefixes())
	if err != nil {
		return backlog.ID{}, err
	}
	if limit := s.cfg.IDs.PlannedMax; limit > 0 && id.Planned() && id.Num > limit {
		return backlog.ID{}, fmt.Errorf(
			"planned id %s is outside the backlog (1-%d); intake ids carry a namespace prefix (%v)",
			bare, limit, s.cfg.Prefixes())
	}
	return id, nil
}

// unresolved is the refusal for an id with no readable body, one sentence per
// state. They answer different questions: a body drover read and could not
// parse is on disk, and reporting it as missing sends a reader to write a file
// that is already there.
func (s *session) unresolved(id backlog.ID, state idState, path string) error {
	switch state {
	case idUnparsed:
		return fmt.Errorf("%s: %s will not parse.\nRepair the frontmatter there; the body is not missing",
			id.Bare(), rel(s.cfg.Root, path))
	case idShelved:
		return fmt.Errorf("%s is shelved: %s.\nRun `drover unshelve %s` to bring it back into %s",
			id.Bare(), rel(s.cfg.Root, path), id.Bare(), s.cfg.Paths.Backlog)
	}
	return fmt.Errorf("no such item: %s (no body at %s/%s-*.md)",
		id.Bare(), s.cfg.Paths.Backlog, id.Stem())
}

// bodyPath finds the file in the bodies directory holding an id, parsed or not.
// backlog.Broken keeps only the path, and the stem at the head of that filename
// is where the id was authored.
func (s *session) bodyPath(id backlog.ID) (string, bool) {
	for _, it := range s.backlog.Items {
		if it.ID == id {
			return it.Path, true
		}
	}
	for _, b := range s.backlog.Broken {
		if got, ok := b.ID(s.cfg.Prefixes()); ok && got == id {
			return b.Path, true
		}
	}
	return "", false
}
