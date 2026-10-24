package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var checkJSON bool

// Finding is one validation failure. Kind is the stable token a wrapper keys
// on; Message is the sentence a human reads. Detail carries the fix where one
// exists, because a finding a reader cannot act on is only half a report.
type Finding struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	ID      string `json:"id,omitempty"`
}

// CheckReport is the whole answer, so a wrapper reading --json gets the counts
// it would otherwise have to recompute from an empty findings array.
type CheckReport struct {
	OK     bool `json:"ok"`
	Bodies int  `json:"bodies"`
	Rows   int  `json:"ledger_rows"`
	// Config is every file the effective config was layered from, global first.
	// A setting can now come from outside the repository, so the report that
	// validates it has to say what it validated.
	Config   []string  `json:"config"`
	Findings []Finding `json:"findings"`
	// Stopped is why this report is less than the whole validation, empty when
	// it ran whole. A config this build cannot load stops every check that
	// reads one, so bodies and ledger_rows are zero because nothing was read
	// rather than because the project holds nothing. Prose: switch on the
	// findings, never on this.
	Stopped string `json:"stopped,omitempty"`
	// RegisterRead answers one question only: did the read happen. It is exactly
	// RegisterState != "unreachable", kept for consumers written before the
	// state field existed. It cannot separate a whole read from a truncated one
	// or from a register nobody configured, so switch on RegisterState.
	RegisterRead bool `json:"register_read"`
	// RegisterNote is why the register answered less than the whole question,
	// empty when it answered it whole. Prose: never key on it.
	RegisterNote string `json:"register_note,omitempty"`
	// RegisterState is what the read amounted to, as a token. Every state
	// addRegister computes has one, so a wrapper never has to read the note to
	// tell a floor from a whole answer.
	RegisterState string `json:"register_state"`
	// MirrorNote is why the mirrored issues were not compared with the backlog,
	// present only under [backlog] mirror = "github" when the read failed or
	// stopped at the page limit. Prose: never key on it.
	MirrorNote string `json:"mirror_note,omitempty"`
}

// The states check reports the register in. One field with members rather than
// a flag per state: a consumer switches once, and a state added later reaches
// it as a value rather than as a key it does not read.
const (
	registerRead        = "read"
	registerUnreachable = "unreachable"
	// registerTruncated is a read cut short at the page limit, so every count
	// taken from it is a floor. Distinct from read because the difference is
	// the whole question a wrapper asks before reporting a zero.
	registerTruncated = "truncated"
	// registerOff is a project that declared no register. Nothing was read and
	// nothing failed, which is neither of the two states above.
	registerOff = "off"
	// registerUnresolved is a read that succeeded, held nothing, and asked a
	// question the project's own config cannot have an answer to.
	registerUnresolved = "unresolved"
)

// findingWidth is the finding column, wide enough for the longest kind. Set by
// hand and pinned by TestFindingKindsFitTheColumn: a kind that outgrows it
// pushes its own message a character right and breaks the alignment the wrapped
// detail line below depends on. docs/rtk-filters.toml matches that indent
// literally, so widening this is a change to the shipped filter as well.
const findingWidth = 19

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate the config, the backlog, the ledger and the paths between them.",
	Long: `Exits 2 on any finding, so it is the command to put in CI. Exit 1 means drover
could not run the validation at all, which is a different problem from a project
that failed it.

The root listing names this once, under Setup, where it is step 2 of the
lifecycle. It is also the check every review round closes on, and the listing
groups by kind rather than repeating a command per step it serves.

Renaming a body changes its branch, its log names and its report path, which
orphans reports already named in the ledger. Nothing else catches that, since
the slug is authored in exactly one place: the filename.

Findings, keyed on the stable token kind. message is prose and will change
wording, so never match on it.

  NO BACKLOG          the item directory named by [paths] backlog does not exist
  NO SHELF            [paths] shelf is set and names a directory that does not exist
  SHELF IS BACKLOG    [paths] shelf and [paths] backlog resolve to the same directory
  NO VERIFY           no [verify] default, so an item runs with no way to verify itself
  UNDECLARED KEY      [severity], [run] runner_key, or an entry in [frontmatter]
                      shape, names a frontmatter key the schema does not declare
  UNREADABLE          a body will not parse: bad frontmatter, or an undeclared key
  BAD VALUE           an item's value fails the shape [frontmatter] declares for that
                      key. The body stays readable: it is a finding against the
                      project, not a reason to drop the item out of the backlog
  BAD RUNNER          a declared runner's binary is on PATH and its headless argv
                      contradicts that binary: the prompt lands somewhere the
                      binary ignores, or the stream drover reads back is never asked
                      for. A binary drover ships no knowledge of is silent here
  BAD LEDGER          a ledger line is not the JSON object the reader expects
  ORPHAN ROW          a ledger row names an id with no body, or one that will not parse
  NO REQUIREMENTS     an accepted, unmerged item whose body has no top-level "N. "
                      lines
  HAND EDITED         an accepted, unmerged item whose body changed since drover last
                      recorded its digest, outside any live session; record it with
                      amend (requirements) or touch (prose) before driving it
  NO DISPOSITION     a merged item's closes key names an issue nobody judged, so
                      finish wrote nothing to the tracker and whoever filed it got
                      silence
  SHELF CLASH         one id holds a body in both the backlog and the shelf
  MISSING DEP         an item depends on an id with no body, one that will not parse,
                      or one that is shelved
  BAD DEPS            a deps line written as a YAML list, bracketed or quoted, so
                      drover reads none of its ids. The finding prints the line that
                      parses; an id it names with no body is MISSING DEP as well
  MISSING REPORT      a report named in the ledger is no longer at that path
  BAD REGISTER        [register] backend = "file" and the register file is on disk and
                      will not read. An absent file is an empty follow-up list,
                      never this
  NO LABEL            the follow-up read found nothing under [github]
                      follow_up_label and the repository defines no label by that
                      name
  NO REGISTER         [register] backend = "file" and [paths] register names no file
                      drover can ever have read: the directory it points into does
                      not exist, or the file is absent while a file holding
                      follow-up rows sits beside it
  DUPLICATE ISSUE     an open follow-up is also the issue an open backlog item was
                      taken in from, so one issue sits in both sets. An item the
                      ledger records as merged carries nothing and is never this
  ORPHANED FOLLOW-UP  open follow-ups whose every owner has merged, so no
                      run is scheduled to reach them
  UNMIRRORED          [backlog] mirror = "github" and an accepted, unmerged item
                      names no issue: a publish failed, or the mirror was turned on
                      after it was accepted
  MIRROR DRIFT        a mirrored issue's requirements differ from its file. The
                      file is the contract and drover never reads the issue back.
                      An unaccepted item is exempt: its list is being redrafted
  MIRROR STALE        [backlog] mirror = "github" and a mirrored issue's drover: state
                      label, open or closed state, or blocked-by links lag the
                      backlog, an unaccepted item's included: a tracker write that
                      accept, unaccept, split, run, finish, mark-merged,
                      unmark-merged or unshelve could not make, or a deps: line
                      edited by hand
  MIRROR SHELVED      a shelved body names a mirrored issue that is still open or
                      lacks the drover:deferred label: a tracker write shelve could
                      not make. drover unshelve <id>, then drover shelve <id>,
                      repairs it
  NO CLOSES KEY       issues are owned by items and [frontmatter] declares no closes key,
                      so no item can name what it resolves: link exits 3 and finish
                      writes nothing to any of them
  UNLINKED            an open owned issue whose owner has run and not merged, and
                      whose closes key does not name it, so the merge ahead will
                      close nothing and whoever filed it gets silence
  STALE CONFIG        .drover.toml does not declare a key drover init would write
                      today, so the project is taking a default it never chose, or
                      it declares a schema_version this build does not read. drover
                      config --diff names what differs. A key a rename left behind
                      is an error at load, and that error names the renames
  BAD TEMPLATE        a template [prompts] implement, [prompts] draft or
                      [prompts] review names cannot be read, or will not render
                      against a sample prompt with every field set: it names a field
                      this build does not have, or a .Meta key [frontmatter] does
                      not declare or a .Gates name [verify] does not, so the command
                      that uses it fails
  STALE BLOCK         a generated agent-instruction block no longer matches what this
                      binary, this config and the [agents] addendum would produce
  STALE POLICY        a generated block records a [supervise] policy the config no
                      longer holds, so the sessions reading it are told permissions
                      drover does not enforce; it names each action and which way
                      it moved, and stands in for STALE BLOCK on that file
  BAD ADDENDUM        the file [agents] addendum names cannot be read, or holds a
                      marker line or unclosed fence that would end the block early,
                      so agents refuses to render and the block cannot be compared
  NO POINTER          the block lives outside AGENTS.md and AGENTS.md never names the
                      file holding it, so an agent loading only AGENTS.md never
                      reaches the instructions
  BAD MARKERS         a file's drover markers are unclosed or out of order, so the
                      block cannot be regenerated in place
  PROTECTED UNMATCHED a [drive] protected_paths entry matches no tracked file, so it
                      holds no merge
  SEQUENCE CLASH      two tracked files share a number in a [numbered_files] glob, which
                      git merges without a conflict and drive refuses on a branch

docs/concepts.md carries the same list with what to do about each.

NO CLOSES KEY precedes both of the next two and suppresses UNLINKED. Where the
key is undeclared the remedy those findings name cannot run, so the project is
reported once against its config rather than once per issue against a command
that exits 3.

ORPHANED FOLLOW-UP and UNLINKED split the owned issue by what its owner has
done. Once every owner has merged, no run is scheduled to reach the issue and
ORPHANED FOLLOW-UP counts it; the remedy is another item declaring that owner
on its deps: line. Before the merge the question is a different one, and it has
an answer only while the branch is open: this run either fixed the issue or it
did not. Nothing in an owner link records which, so UNLINKED asks rather than
guesses. Naming a fix that already shipped is what closes it; leaving the key
alone is what says a later run still owes it.

ORPHANED FOLLOW-UP reads the follow-up list, so it degrades rather than fails:
a list drover cannot reach produces no ORPHANED FOLLOW-UP finding and says so,
and a project declaring [register] backend = "none" is silent. An outage is never a reason to fail CI,
and a zero from a read that did not happen is worse than no answer at all.

BAD REGISTER is what separates the two causes of that unreachable read. Under
[register] backend = "file" the follow-up list is a path in this repository, so a
misspelled one is the project's own fault and is reported; under any other
backend the file is never opened and nothing is said about it. An absent file
is an empty follow-up list rather than a finding, since that is where every
project starts.

NO LABEL and NO REGISTER separate a zero from a question nobody could have
answered, one per backend, and neither says the follow-up list is empty. Each
consults a second fact rather than the count: whether the repository defines the
label at all, and whether the path can ever have been the one rows were appended
under.
The file backend reads that second fact twice: the directory the path points
into, and, when the file itself is absent, whether a file beside it parses as a
register file. A zero under a label that exists, or beside a directory holding
no register file at all, is an empty follow-up list and is silent: that is where
every project starts. The label set is one gh call, and it degrades the way
ORPHANED FOLLOW-UP does: a
note saying what could not be asked, never a finding.`,
	Example: `  drover check                      # exit 2 on any finding; this is the CI command
  drover check --json               # one object, counts included:
    {"ok":false,"bodies":14,"ledger_rows":3,"register_read":true,
     "register_state":"read",
     "config":["/home/x/.config/drover/config.toml","/repo/.drover.toml"],
     "findings":[{"kind":"MISSING DEP","message":"7 depends on \"6b\"","id":"7"}]}

  config is every file the effective settings were layered from, global first.
  A setting can come from outside the repository, so the report that validates
  it says what it validated.

  Read register_state before concluding there are no orphaned follow-ups. Only
  "read" says the count is the whole answer:

    read         the follow-up list answered whole; a zero is a zero
    truncated    the read stopped at [github] page_limit, so every count is a
                 floor and a zero is not evidence of an empty follow-up list
    unreachable  nothing was read; the findings above are a floor
    unresolved   the read succeeded, returned nothing, and named a label or a
                 path that resolves to nothing, so no row could have matched
    off          [register] backend = "none"; there was nothing to read and
                 nothing failed

  register_note is prose and only ever elaborates: never key on it. Under
  "read" it is present only when the label set itself could not be reached,
  where drover has no second fact to judge a zero by.

  mirror_note is prose too, present under [backlog] mirror = "github" when the
  mirrored issues were not read whole, so MIRROR DRIFT and MIRROR STALE
  are a floor.

  register_read is the older field and answers one question: did the read
  happen. It is exactly register_state != "unreachable", kept working for
  consumers written before the state existed. It cannot separate a whole read
  from a truncated one, or either from a project with no follow-up list at all.

  ok is redundant with the exit code on purpose, so a consumer that captured
  stdout and dropped the status still has the result.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		rep, err := checkAnyConfig()
		if err != nil {
			return err
		}

		if checkJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(rep); err != nil {
				return err
			}
		} else {
			renderCheck(os.Stdout, rep)
		}

		if !rep.OK {
			return Findings{errors.New(findingsLine(len(rep.Findings)))}
		}
		return nil
	},
}

// checkAnyConfig is the whole report, including for a project whose config
// Load refuses.
//
// A stale schema_version is an error at load, so the ordinary door exits 1 and
// says nothing a CI job can key on, and a config this build would write
// differently is precisely the thing STALE CONFIG exists to report. The
// inspection path validates nothing and reads the file anyway, so the one
// finding reaches the report even though nothing else could.
//
// The project's own file decides staleness, not whether Load succeeded. A
// per-user schema_version that matches lets a project with none load, and
// says nothing about which format the project's file was written against: a
// check that went by the load would report that project clean. So a project that
// declares no version, or one this build does not read, stops here with one
// finding whichever door it came through.
//
// Only a version gap takes this route. Every other reason Load refuses is a
// project that has to be fixed before drover can say anything about it, and
// turning those into findings would report a config drover never understood as
// a config it merely disagrees with. That error is returned before the
// inspection runs: a per-user config that failed to parse for its own reason
// leaves the project's schema_version looking as old as it ever was, and a
// project sitting on both once reported STALE CONFIG and exit 2 while the
// error that stopped the load vanished.
func checkAnyConfig() (CheckReport, error) {
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		return CheckReport{}, wdErr
	}
	// The config alone, and the backlog, ledger and shelf only once the version
	// decision below has let the check proceed. Loading a session outright reads
	// all four, so a project declaring no version answered with whatever its
	// ledger path happened to be: a directory where a file belongs reported
	// "is a directory" and exit 1, while the version gap that is the whole
	// finding never got as far as being noticed.
	cfg, err := config.Load(wd)
	var refused *config.VersionError
	if err != nil && !errors.As(err, &refused) {
		return CheckReport{}, err
	}
	in, inErr := config.Inspect(wd)
	switch {
	case inErr != nil && err != nil:
		return CheckReport{}, err
	case inErr != nil:
		return CheckReport{}, inErr
	case in.Stale():
		return CheckReport{
			Config:   in.Config.Sources(),
			Findings: []Finding{staleVersion(in)},
			// Load's own error is not repeated here. It names the renames that
			// made the version old, which is what the finding sends the reader
			// to `drover config --diff` for, and a multi-line error inside a
			// JSON string field is not where anybody reads it.
			Stopped:       stoppedStale,
			RegisterState: registerUnreachable,
		}, nil
	case err != nil:
		return CheckReport{}, err
	}
	s, err := openFrom(cfg)
	if err != nil {
		return CheckReport{}, err
	}
	return s.checkReport(), nil
}

// stoppedStale is the report's Stopped line for a config at a schema_version
// this build does not read.
var stoppedStale = fmt.Sprintf("%s is not at this build's schema_version, so no body, ledger row "+
	"or path was read: bodies and ledger_rows are unread rather than zero", config.FileName)

// staleVersion is the finding for a config written against a schema this build
// does not read. It names the file and the command that explains it, and never
// the keys: the renames that made the version old are Load's error, and the
// full list is one command away.
//
// Absence and an explicitly written 0 both leave Version at 0 and get
// different text. "declares schema_version 0" about a file with no such line
// sends the reader looking for one, and a project written before the key
// existed is the oldest config there is rather than one that typed a number.
//
// The file named is whichever declared the version, which is the project's own
// except where it inherits the key. A finding naming .drover.toml for a value
// written in the per-user config sends the reader to edit a file that says
// nothing about it.
func staleVersion(in *config.Inspection) Finding {
	file := config.FileName
	src, version := in.VersionSource()
	if src != in.Path {
		file = src
	}
	declares := fmt.Sprintf("declares schema_version %d", version)
	if src == in.Path && !in.VersionDeclared() {
		declares = "declares no schema_version"
	}
	return Finding{
		Kind: "STALE CONFIG",
		Message: fmt.Sprintf("%s %s and this build understands %d",
			file, declares, config.SchemaVersion),
		Detail: "run `drover config --diff`, which reads the file without validating it and names every key the gap left behind",
		Path:   file,
	}
}

// badDeps is check's finding for a deps line that is not space-separated
// ids, naming the line that parses in its place.
func (s *session) badDeps(it backlog.Item) Finding {
	raw := strings.TrimSpace(it.Meta[backlog.KeyDeps])
	fixed, _, _ := correctDeps(raw, s.cfg.Prefixes())
	f := Finding{
		Kind:    "BAD DEPS",
		Message: fmt.Sprintf("%s reads %s: %s, which drover does not parse", it.ID.Bare(), backlog.KeyDeps, raw),
		Path:    rel(s.cfg.Root, it.Path),
		Line:    depsLineNo(it.Path),
		ID:      it.ID.Bare(),
	}
	where := f.Path
	if f.Line > 0 {
		where += ":" + strconv.Itoa(f.Line)
	}
	f.Detail = fmt.Sprintf("write it as `%s: %s` in %s", backlog.KeyDeps, strings.Join(fixed, " "), where)
	return f
}

// renderCheck writes the report a person reads. Separate from RunE so a test
// can replay real output through the shipped rtk filter rather than through a
// second copy of this format.
func renderCheck(w io.Writer, rep CheckReport) {
	// Only when a file outside the repository had a say. Naming the project's
	// own config on every run is noise; naming the one a reader would not think
	// to open is the whole point.
	if len(rep.Config) > 1 {
		fmt.Fprintf(w, "config: %s\n", strings.Join(rep.Config, " < "))
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "%-*s %s\n", findingWidth, f.Kind, f.Message)
		if f.Detail != "" {
			fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", findingWidth+1), f.Detail)
		}
	}
	if rep.RegisterNote != "" {
		fmt.Fprintf(w, "note: %s\n", rep.RegisterNote)
	}
	if rep.MirrorNote != "" {
		fmt.Fprintf(w, "note: %s\n", rep.MirrorNote)
	}
	// Ahead of the result line, because it says the result is a floor.
	if rep.Stopped != "" {
		fmt.Fprintf(w, "stopped: %s\n", rep.Stopped)
	}
	if rep.OK {
		fmt.Fprintln(w, checkLine(rep.Bodies, rep.Rows))
	}
}

// checkLine is the whole result for a project with nothing wrong with it. A
// one-item backlog is every project on its first day, so it agrees in number.
func checkLine(bodies, rows int) string {
	return fmt.Sprintf("ok — %d %s %s, %d ledger %s, no orphans or stale paths",
		bodies, plural(bodies, "body", "bodies"), plural(bodies, "parses", "parse"),
		rows, plural(rows, "row", "rows"))
}

// findingsLine is the same result when something is wrong, and agrees in
// number for the same reason checkLine does.
func findingsLine(n int) string {
	return fmt.Sprintf("%d %s", n, plural(n, "finding", "findings"))
}

// checkReport gathers every finding. Order is deliberate: NO BACKLOG comes first
// because every check after it reads a backlog that is not there.
func (s *session) checkReport() CheckReport {
	rep := CheckReport{
		Bodies:   len(s.backlog.Items),
		Rows:     len(s.ledger.Rows),
		Config:   s.cfg.Sources(),
		Findings: []Finding{},
	}
	add := func(f Finding) { rep.Findings = append(rep.Findings, f) }

	if s.backlog.Missing {
		add(Finding{
			Kind:    "NO BACKLOG",
			Message: fmt.Sprintf("%s does not exist", s.cfg.Paths.Backlog),
			Detail:  "paths.backlog names it; create it or correct the key",
			Path:    s.cfg.Paths.Backlog,
		})
	}

	// A shelf drover cannot open reads as no shelf at all, which restores every
	// behavior the key exists to prevent: ORPHAN ROW returns for shelved ids and
	// intake starts reissuing them. An unset key is silent, so upgrading gains a
	// project no finding.
	if s.shelf != nil && s.shelf.Missing {
		add(Finding{
			Kind:    "NO SHELF",
			Message: fmt.Sprintf("%s does not exist", s.cfg.Paths.Shelf),
			Detail:  "paths.shelf names it; create it or correct the key",
			Path:    s.cfg.Paths.Shelf,
		})
	}

	// One directory cannot be both. Every live body would read as shelved, which
	// suppresses the orphan and dep checks the shelf was only ever meant to
	// soften for retired work.
	oneDir := s.cfg.Paths.Shelf != "" && sameDir(s.cfg.Abs(s.cfg.Paths.Shelf), s.cfg.Abs(s.cfg.Paths.Backlog))
	if oneDir {
		add(Finding{
			Kind:    "SHELF IS BACKLOG",
			Message: s.shelfCollision(),
			Detail:  "point shelf at its own directory, or unset it; every live body reads as shelved",
			Path:    s.cfg.Paths.Shelf,
		})
	}

	// An item run without a gate hands its agent no way to verify the work, so
	// the run reports success on the agent's word alone. That is the one thing
	// this tool exists to stop, and a generated config starts here.
	if s.cfg.Verify["default"] == "" {
		add(Finding{
			Kind:    "NO VERIFY",
			Message: "[verify] default is empty",
			Detail:  "name the command that must pass before an item merges",
		})
	}

	// A protected_paths entry that matches nothing tracked holds nothing, and reads
	// in the config exactly like one that does. Only the entries a project
	// wrote: the built-in set is a default every project gets, and one without
	// workflows would fail CI for it.
	if len(s.cfg.Loop.DenyPaths) > 0 {
		for _, f := range s.unmatchedDenies() {
			add(f)
		}
	}
	for _, f := range s.sequenceClashes() {
		add(f)
	}

	// Only for a project that wrote [severity]: the block defaults on everywhere,
	// so an undeclared key is the ordinary state of a project that does not rank
	// its work, and a finding there would fail CI for every one of them.
	// Configured but undeclared is a typo that fails silently, since no item can
	// carry the key and so nothing ever colours.
	if s.cfg.SeverityConfigured() && !s.cfg.Known(s.cfg.Severity.Key) {
		add(Finding{
			Kind:    "UNDECLARED KEY",
			Message: fmt.Sprintf("[severity] key = %q is not in [frontmatter]", s.cfg.Severity.Key),
			Detail:  "add it to optional, or no item can declare a severity",
		})
	}

	// The same mistake in [run], gated the same way and for the same reason:
	// runner_key defaults to "runner" everywhere, so an undeclared default is the
	// ordinary state of a project that pins no runner per item. Set and
	// undeclared is the typo, and it is quieter than severity's: no item can
	// carry the key, so run silently falls back to [run] default_runner for
	// every item, and nothing anywhere reports that a pin was ignored.
	if s.cfg.RunnerKeyConfigured() && !s.cfg.Known(s.cfg.Run.RunnerKey) {
		add(Finding{
			Kind:    "UNDECLARED KEY",
			Message: fmt.Sprintf("[run] runner_key = %q is not in [frontmatter]", s.cfg.Run.RunnerKey),
			Detail:  "add it to optional, or no item can pin a runner and run falls back to default_runner",
		})
	}

	s.addConfig(add)
	s.addTemplates(add)

	// A runner drover can launch and cannot use. [runners] is project-supplied
	// and executed as given, so an argv that contradicts the binary it names
	// fails at launch, unattended, hours into a drive: this project's own
	// reviewer spent two drives writing an empty log that read as a session
	// that died, and nothing between the config and the launch said a word.
	//
	// Gated on PATH because an argv is only wrong for the binary actually
	// there, and because CI on a host that never installed a runner has nothing
	// to judge and must not fail for it.
	for _, name := range keys(s.cfg.Runners) {
		spec := s.cfg.Runners[name]
		bin := spec.Binary(runner.Headless)
		if bin == "" {
			continue
		}
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		for _, f := range runner.Faults(spec) {
			add(Finding{
				Kind:    "BAD RUNNER",
				Message: fmt.Sprintf("[runners] %s %s: %s", name, f.Mode, f.Reason),
				Detail:  f.Fix,
			})
		}
	}

	// The same mistake one table over, and it fails the same way: no item can
	// carry the key, so nothing is ever checked against the shape.
	for _, key := range sortedKeys(s.cfg.Frontmatter.Shape) {
		if s.cfg.Known(key) {
			continue
		}
		add(Finding{
			Kind:    "UNDECLARED KEY",
			Message: fmt.Sprintf("[frontmatter] shape declares %q, which is not in [frontmatter]", key),
			Detail:  "add it to optional, or no item can carry a value to check",
		})
	}

	for _, b := range s.backlog.Broken {
		add(Finding{
			Kind:    "UNREADABLE",
			Message: rel(s.cfg.Root, b.Path),
			Detail:  b.Err.Error(),
			Path:    rel(s.cfg.Root, b.Path),
		})
	}

	// Items only, never Broken: a body that did not parse has no values to
	// check and already carries UNREADABLE. A value the shape rejects is a
	// finding against the project and never against the body — the frontmatter
	// parsed, so status still shows the item in its real state and run still
	// launches it.
	for _, it := range s.backlog.Items {
		for _, key := range sortedKeys(s.cfg.Frontmatter.Shape) {
			shape, ok := s.cfg.Shapes()[key]
			if !ok {
				continue
			}
			raw := it.Get(key)
			err := shape.Check(raw, s.cfg.Prefixes())
			if err == nil {
				continue
			}
			add(Finding{
				Kind:    "BAD VALUE",
				Message: fmt.Sprintf("%s declares %s = %q, which is not %s", it.ID.Bare(), key, raw, shape.Spec()),
				Detail:  err.Error(),
				Path:    rel(s.cfg.Root, it.Path),
				ID:      it.ID.Bare(),
			})
		}
	}

	// A merged item whose claimed issues nobody judged. The issue is still open
	// and its reporter has heard nothing, which is the state before drover could
	// resolve anything at all — and a gap nothing reports is a gap nobody closes.
	//
	// Merged only: an item still in flight has not reached the point where a
	// disposition is owed, and reporting it would fire on every open item that
	// names an issue.
	for _, it := range s.backlog.Items {
		id := it.ID.Bare()
		if !s.done(id) {
			continue
		}
		for _, n := range undisposed(s.ledger, id, backlog.Ints(it.Get(closesKey))) {
			add(Finding{
				Kind:    "NO DISPOSITION",
				Message: fmt.Sprintf("%s merged claiming to resolve #%d, which nobody judged", id, n),
				Detail: fmt.Sprintf("drover resolve %s %d <%s> --code-by <who> \"<what shipped>\"",
					id, n, strings.Join(ledger.Dispositions(), "|")),
				Path: rel(s.cfg.Root, it.Path),
				ID:   id,
			})
		}
	}

	// An accepted item with no numbered list. `report --save` turns every
	// top-level "N. " line into an audit checkbox, so a requirement written as
	// prose is on no checklist and nobody will ever confirm it.
	//
	// Scaffolds are exempt: the stop marker is the state that says the list is
	// not written yet, and reporting both would fire twice on one item.
	//
	// Merged items are exempt too. The finding warns that nobody will confirm
	// the work, and for merged work that already happened or never will: a list
	// written now would be a contract nobody audited, so the only remedy left is
	// to shelve the body, and a finding nothing can discharge drowns the ones
	// that can.
	for _, it := range s.backlog.Items {
		if s.isScaffold(it) || s.done(it.ID.Bare()) || len(it.Requirements()) > 0 {
			continue
		}
		add(Finding{
			Kind:    "NO REQUIREMENTS",
			Message: fmt.Sprintf("%s has no numbered requirements, so nothing will ever confirm it", it.ID.Bare()),
			Detail:  "report --save builds the audit checklist from top-level \"N. \" lines; prose is on no checklist",
			Path:    rel(s.cfg.Root, it.Path),
			ID:      it.ID.Bare(),
		})
	}

	// A body edited by hand since drover last recorded its digest. drive reads
	// that as a reviewer writing findings by hand and parks the next clean pass,
	// so an operator's legitimate edit has to be recorded before it is driven.
	// An item a live session is working is mid-flight rather than hand edited.
	running := s.sessionItems()
	for _, it := range s.backlog.Items {
		if running[it.ID.Bare()] {
			continue
		}
		if what, fix := s.handEdit(it); what != "" {
			add(Finding{
				Kind:    "HAND EDITED",
				Message: what,
				Detail:  fix,
				Path:    rel(s.cfg.Root, it.Path),
				ID:      it.ID.Bare(),
			})
		}
	}

	// The shelf carries ids, never items: a retired body is not parsed as one, so
	// nothing on it can be UNREADABLE. It answers two questions below, both of
	// which a missing id turns into a wrong answer rather than a smaller one.
	shelved := map[string]string{}
	for _, id := range s.shelf.IDs() {
		path, _ := s.shelf.Path(id)
		shelved[id.Bare()] = path
	}

	// One id, two bodies. backlog.Load cannot see this — its duplicate map is
	// built while reading a single directory — and it is the one state where
	// reviving a shelved body takes over a live item's identity.
	//
	// A body that will not parse still spends its id, so the live side is keyed
	// off the filename rather than the parsed item. A half-revived body sitting
	// unreadable beside its shelved original is the worst version of this state,
	// not an exempt one.
	for _, id := range s.shelf.IDs() {
		live, ok := s.bodyPath(id)
		// One directory under two spellings would make every id clash with itself
		// and name one file twice, which reads as two files to rename. SHELF IS BACKLOG
		// above is the finding for that state, one rather than one per body.
		if !ok || oneDir {
			continue
		}
		path, _ := s.shelf.Path(id)
		add(Finding{
			Kind:    "SHELF CLASH",
			Message: fmt.Sprintf("%s is both live and shelved: %s and %s", id.Bare(), rel(s.cfg.Root, live), rel(s.cfg.Root, path)),
			Detail:  "rename or remove one; reviving the shelved body would take over the live item's identity",
			Path:    rel(s.cfg.Root, path),
			ID:      id.Bare(),
		})
	}

	// A line that will not parse is a lost record. The shell driver's
	// `fromjson? // empty` skipped these, so a truncated write looked exactly
	// like a completion nobody ever recorded.
	for _, m := range s.ledger.Malformed {
		add(Finding{
			Kind:    "BAD LEDGER",
			Message: fmt.Sprintf("%s:%d", rel(s.cfg.Root, s.ledger.Path), m.Line),
			Detail:  m.Err.Error(),
			Path:    rel(s.cfg.Root, s.ledger.Path),
			Line:    m.Line,
		})
	}

	// Every id the ledger mentions must still exist in the backlog, or a
	// completion is recorded against nothing.
	have := map[string]bool{}
	for _, it := range s.backlog.Items {
		have[it.ID.Bare()] = true
	}
	// Shelved is not gone: the completion is still recorded against a body that
	// exists, it just moved out of the backlog. A deleted body stays a finding,
	// since that is the record genuinely lost.
	for _, id := range s.ledger.IDs() {
		if have[id] || shelved[id] != "" {
			continue
		}
		f := Finding{
			Kind:    "ORPHAN ROW",
			Message: fmt.Sprintf("ledger records %q, which has no body", id),
			ID:      id,
		}
		// The file is right there and will not parse, so telling the reader to
		// write the item sends them to create a second one under the same id.
		if path, ok := s.bodyPathBare(id); ok {
			f.Message = fmt.Sprintf("ledger records %q, whose body will not parse", id)
			f.Detail = fmt.Sprintf("repair the frontmatter in %s; the record is not orphaned", rel(s.cfg.Root, path))
			f.Path = rel(s.cfg.Root, path)
		}
		add(f)
	}

	// A dep naming an item that does not exist reads as permanently blocked.
	// A shelved dep blocks its dependent exactly as hard as a missing one, so it
	// stays a finding; saying which it is changes the fix from writing the item
	// to moving it back.
	for _, it := range s.backlog.Items {
		// A malformed line is one BAD DEPS finding however many ids it spoils,
		// and each id it names is still checked under its stripped form, so a
		// missing one dangles beside it.
		deps, malformed := backlog.ParseDeps(it.Meta[backlog.KeyDeps])
		for _, d := range deps {
			state, path := s.resolve(d)
			if state == idReadable {
				continue
			}
			// idAbsent and idUnresolved share a sentence: an id drover cannot
			// parse names no file to go looking for, so "has no body" is the whole
			// truth about it here.
			f := Finding{
				Kind:    "MISSING DEP",
				Message: fmt.Sprintf("%s depends on %q, which has no body", it.ID.Bare(), d),
				ID:      it.ID.Bare(),
			}
			switch state {
			case idUnparsed:
				f.Message = fmt.Sprintf("%s depends on %q, whose body will not parse", it.ID.Bare(), d)
				f.Detail = fmt.Sprintf("repair the frontmatter in %s; the dep is not missing", rel(s.cfg.Root, path))
				f.Path = rel(s.cfg.Root, path)
			case idShelved:
				f.Message = fmt.Sprintf("%s depends on %q, which is shelved", it.ID.Bare(), d)
				f.Detail = fmt.Sprintf("move %s back into %s, or drop the dep", rel(s.cfg.Root, path), s.cfg.Paths.Backlog)
				f.Path = rel(s.cfg.Root, path)
			}
			add(f)
		}
		if malformed {
			add(s.badDeps(it))
		}
	}

	// The ledger names report files by path; a rename leaves the row pointing
	// at nothing.
	for _, row := range s.ledger.Rows {
		if row.Report == "" {
			continue
		}
		if _, err := os.Stat(s.cfg.Abs(row.Report)); err != nil {
			add(Finding{
				Kind:    "MISSING REPORT",
				Message: fmt.Sprintf("%s names %s, which no longer exists", row.ID, row.Report),
				Path:    row.Report,
				ID:      row.ID,
			})
		}
	}

	// The one finding here that is not backlog file integrity, and the reason it
	// comes last: everything above reads files, this reads a tracker.
	//
	// A row whose every owner has merged is MISSING DEP one layer out. The item
	// pointing at a body nobody wrote is a plan that cannot execute; the row
	// pointing at an owner that has already merged carries a label that still
	// routes and no run left to carry it. Both are reported and neither is
	// judged: what to do about a dormant row stays a human's call.
	s.addRegister(&rep)
	s.addMirror(&rep, add)
	s.addInstructions(add)

	rep.OK = len(rep.Findings) == 0
	return rep
}

// addMirror compares the backlog with the issues it is mirrored to, under
// [backlog] mirror = "github" and nowhere else.
//
// Every tracker write accept, unaccept, split, run, finish, mark-merged,
// unmark-merged, shelve and unshelve make degrades: the file is the contract and is already written,
// so a tracker nobody could reach costs the mirror and never the command. This
// is where what they could not publish surfaces, and one remedy covers nearly
// all of it: accept on an accepted item rewrites its issue from the file, and
// unaccept does the same for a scaffold's label. A shelved body is the
// exception, since nothing parses it as an item: unshelve then shelve repeats
// both writes.
//
// UNMIRRORED needs no tracker. The other three read every issue carrying the
// membership label in one call, and degrade the way ORPHANED FOLLOW-UP does: an
// unreachable tracker is a note, never a finding, so an outage cannot fail CI.
// Merged items are exempt from UNMIRRORED for NO REQUIREMENTS's reason: a project
// turning the mirror on would otherwise fail on every item it ever merged.
func (s *session) addMirror(rep *CheckReport, add func(Finding)) {
	if !s.mirroring() {
		return
	}
	logDir := s.cfg.LogDir()
	var mirrored []backlog.Item
	for _, it := range s.backlog.Items {
		id := it.ID.Bare()
		switch {
		case issueOf(it) != 0:
			mirrored = append(mirrored, it)
		case s.isScaffold(it):
		case !s.done(id):
			add(Finding{
				Kind:    "UNMIRRORED",
				Message: fmt.Sprintf("%s is accepted and names no issue, so nobody outside this checkout can see it", id),
				Detail:  fmt.Sprintf("drover accept %s publishes it; a tracker that was unreachable at accept left it here", id),
				Path:    rel(s.cfg.Root, it.Path),
				ID:      id,
			})
		}
	}
	// A shelved body is read for its issue key alone and best effort: one that
	// will not parse has nothing for this check to compare.
	type shelvedIssue struct {
		id, path string
		number   int
	}
	var shelved []shelvedIssue
	for _, id := range s.shelf.IDs() {
		path, _ := s.shelf.Path(id)
		meta, err := s.shelvedMeta(path)
		if err != nil {
			continue
		}
		if n := issueOf(backlog.Item{Meta: meta}); n != 0 {
			shelved = append(shelved, shelvedIssue{id.Bare(), path, n})
		}
	}
	if len(mirrored) == 0 && len(shelved) == 0 {
		return
	}
	index := gh.LoadMirror(s.labels(), mirrorMember)
	if !index.OK {
		rep.MirrorNote = fmt.Sprintf("the mirrored issues were not read, so none was compared with its file: %v", index.Reason())
		return
	}
	if index.Truncated(s.cfg.GitHub.PageLimit) {
		rep.MirrorNote = fmt.Sprintf("the mirrored issues hit the %d-issue page limit, so an issue past it went uncompared",
			s.cfg.GitHub.PageLimit)
	}
	byNumber := make(map[int]mirrorIssue, len(index.Issues))
	for _, is := range index.Issues {
		byNumber[is.Number] = asMirror(is)
	}
	for _, it := range mirrored {
		id, number := it.ID.Bare(), issueOf(it)
		// Absent from the read: an intaken item's issue, which is the
		// reporter's and not a mirror, or one past the page limit, which the
		// note above already covers.
		m, ok := byNumber[number]
		if !ok {
			continue
		}
		path := rel(s.cfg.Root, it.Path)
		remedy := fmt.Sprintf("drover accept %s rewrites the issue from the file; drover never reads it back", id)
		// An unaccepted item's issue keeps the list it was published with under
		// a drover:draft label. Its requirements are being redrafted, and
		// accept is the publish that ends that, so a scaffold is compared on its
		// label, state and links alone, and accept is not its remedy.
		scaffold := s.isScaffold(it)
		if scaffold {
			remedy = fmt.Sprintf("drover unaccept %s rewrites the issue's label; accept publishes it once the list is a contract again", id)
		}
		if got, ok := published(m.Body); !scaffold && (!ok || got != mirrorRequirements(it.Body)) {
			add(Finding{
				Kind:    "MIRROR DRIFT",
				Message: fmt.Sprintf("issue #%d does not carry the requirements %s holds", number, id),
				Detail:  remedy, Path: path, ID: id,
			})
		}
		if behind := s.mirrorBehind(it, m, logDir); len(behind) > 0 {
			add(Finding{
				Kind:    "MIRROR STALE",
				Message: fmt.Sprintf("issue #%d for %s %s", number, id, strings.Join(behind, "; ")),
				Detail:  remedy, Path: path, ID: id,
			})
		}
	}
	for _, sh := range shelved {
		m, ok := byNumber[sh.number]
		if !ok {
			continue
		}
		var behind []string
		if strings.EqualFold(m.State, "OPEN") {
			behind = append(behind, "is still open")
		}
		if !slices.Contains(m.Labels, mirrorLabel(stateShelved)) {
			behind = append(behind, "is not labeled "+mirrorLabel(stateShelved))
		}
		if len(behind) == 0 {
			continue
		}
		add(Finding{
			Kind:    "MIRROR SHELVED",
			Message: fmt.Sprintf("issue #%d for shelved %s %s", sh.number, sh.id, strings.Join(behind, " and ")),
			Detail:  fmt.Sprintf("drover unshelve %s, then drover shelve %s: a shelve whose tracker write failed is repaired by repeating it", sh.id, sh.id),
			Path:    rel(s.cfg.Root, sh.path), ID: sh.id,
		})
	}
}

// mirrorBehind names each way a mirrored issue lags the backlog: a tracker write
// some command could not make, or a deps: line edited by hand.
func (s *session) mirrorBehind(it backlog.Item, m mirrorIssue, logDir string) []string {
	state, _ := s.workState(it, logDir)
	var behind []string
	if !m.labelFits(state) {
		have := strings.Join(m.stateLabels(), ", ")
		if have == "" {
			have = "no state label"
		}
		behind = append(behind, fmt.Sprintf("carries %s where the item is %s", have, state))
	}
	switch open := strings.EqualFold(m.State, "OPEN"); {
	case state == stateDone && open:
		behind = append(behind, "is open though the item merged")
	case state != stateDone && !open:
		behind = append(behind, "is closed though the item has not merged")
	}
	deps, _ := s.depIssues(it)
	if missing := m.missingLinks(deps); len(missing) > 0 {
		behind = append(behind, "is not marked blocked by "+renderInts(missing))
	}
	return behind
}

// addConfig reports a config this binary would write differently.
//
// It is STALE BLOCK's argument applied to the other generated artifact. A
// .drover.toml is written once, against whatever this build understood that
// day, and nothing afterwards tells the project the binary has moved: a key
// added since gets its zero value, a table added since is absent, and both read
// exactly like a project that declared them and chose the default. A project
// can run for weeks with no [review] table, and so no max_review_rounds bound,
// without anything saying the bound exists.
//
// The judge is the generator, rendered against what this project already
// carries, so only a key the project never decided counts. Nothing compares the
// text: a .drover.toml carries hand-written comments recording why each value
// is what it is, and a text comparison would report every project with a
// comment in it.
//
// A retired key and an older schema_version are not reported here, because both
// are errors at load and no check runs at all on a config carrying either.
// `drover config --diff` reads the file without validating it and is where
// those two are named.
func (s *session) addConfig(add func(Finding)) {
	missing, err := undeclared(s.cfg.Root, s.cfg.Verify["default"], s.cfg.Declared)
	if err != nil {
		// The generator would not render, so nothing here knows whether the
		// config is current. Saying so beats reporting the silence as a pass.
		add(Finding{
			Kind:    "STALE CONFIG",
			Message: fmt.Sprintf("drover could not render this project's layout, so %s went unchecked", config.FileName),
			Detail:  fmt.Sprintf("%v; run `drover config --diff`", err),
			Path:    config.FileName,
		})
		return
	}
	if len(missing) == 0 {
		return
	}
	add(Finding{
		Kind: "STALE CONFIG",
		Message: fmt.Sprintf("%s does not declare %d %s `drover init` would write today",
			config.FileName, len(missing), plural(len(missing), "key", "keys")),
		Detail: "run `drover config --diff`, which names each one at the value drover resolves it to",
		Path:   config.FileName,
	})
}

// addTemplates renders each template the project names against a populated
// brief.
//
// Only the commands that use a template ever load it, and Render resolves a
// field on the path it executes and nowhere else, so a template naming a field
// this build removed passes every check and fails at launch, unattended. The
// sample fills every slice so a field reached only inside a range is resolved
// too, and Meta and Gates hold the project's declared keys and gates, so a
// template reading one the project declares renders and a typo does not. An
// unset key renders a built-in template, which the prompt package's own tests
// render against the same sample.
func (s *session) addTemplates(add func(Finding)) {
	for _, path := range []string{s.cfg.Prompts.Implement, s.cfg.Prompts.Draft, s.cfg.Prompts.Review} {
		if path == "" {
			continue
		}
		why := "does not render against this build's prompt"
		name, text, err := prompts.Load(s.cfg.Abs(path), "", "")
		if err != nil {
			why = "could not be read"
		} else {
			_, err = prompts.Render(name, text, prompts.SampleFor(s.cfg.DeclaredKeys(), s.cfg.Verify))
		}
		if err != nil {
			add(Finding{
				Kind:    "BAD TEMPLATE",
				Message: path + " " + why,
				Detail:  err.Error(),
				Path:    path,
			})
		}
	}
}

// addInstructions checks the generated agent instructions against what this
// binary and this config would produce now.
//
// A project with no generated block gets nothing. Writing one is a suggestion
// `init` makes, not a requirement, and a finding for its absence would fail CI
// for every project that declined. An addendum that cannot be read is reported
// either way: it is configured, and `agents` will refuse to render without it.
func (s *session) addInstructions(add func(Finding)) {
	if err := s.addendumErr(); err != nil {
		add(Finding{
			Kind:    "BAD ADDENDUM",
			Message: fmt.Sprintf("[agents] addendum %s cannot be placed in the generated block", s.cfg.Agents.Addendum),
			Detail:  err.Error(),
			Path:    s.cfg.Agents.Addendum,
		})
	}
	hosts, err := findAgentsHosts(s.cfg.Root)
	if err != nil || len(hosts) == 0 {
		return
	}
	// Without a render there is nothing to compare a block against, but the
	// marker and pointer findings read only the host files.
	current, renderErr := s.currentBlock()

	for _, host := range hosts {
		if host.Malformed {
			add(Finding{
				Kind:    "BAD MARKERS",
				Message: fmt.Sprintf("%s has the drover markers out of order or unclosed", host.Name),
				Detail:  "agents --write refuses this rather than guess which half to keep; fix the markers by hand",
				Path:    host.Name,
			})
			continue
		}
		if f, ok := stalePolicy(s.cfg, host.Name, host.Block); ok {
			add(Finding{
				Kind:    "STALE POLICY",
				Message: fmt.Sprintf("%s records a [supervise] policy the config no longer holds", host.Name),
				Detail:  fmt.Sprintf("run `%s`: %s", f.Fix, f.Why),
				Path:    host.Name,
			})
		} else if renderErr == nil && host.Block != current {
			add(Finding{
				Kind:    "STALE BLOCK",
				Message: fmt.Sprintf("%s does not match what this binary would generate", host.Name),
				Detail:  fmt.Sprintf("run `drover agents --write %s`: %s", host.Name, s.staleWhy(host.Block)),
				Path:    host.Name,
			})
		}
		// Only for a block kept somewhere other than the file an agent harness
		// loads on its own. The instructions are unreachable without a pointer,
		// and unreachable safety rules are the ones an agent runs past.
		if host.Name == pointerConvention {
			continue
		}
		switch found, err := referencesHost(s.cfg.Root, host.Name); {
		case err != nil:
			// No AGENTS.md at all is a deliberate arrangement, not a defect: the
			// project keeps its instructions under a name it chose and tells its
			// agents about it some other way.
			continue
		case !found:
			add(Finding{
				Kind:    "NO POINTER",
				Message: fmt.Sprintf("%s never names %s, which holds the instructions", pointerConvention, host.Name),
				Detail:  fmt.Sprintf("paste `drover agents --pointer %s` into %s", host.Name, pointerConvention),
				Path:    pointerConvention,
			})
		}
	}
}

// addRegister is the register half of the report: one read, and everything the
// index can be asked about it.
//
// It degrades in three directions. Unreachable is not zero: the note says the
// finding set is a floor, following unaudited, which prints "?" rather than 0 for the
// same reason. Off says nothing at all — a project that declared no register has
// failed nothing, and a finding there would fail CI for every project that never
// wanted one. Truncated is the third: the count is a floor, so a zero cannot
// report the register clean.
func (s *session) addRegister(rep *CheckReport) {
	index := s.register()
	rep.RegisterRead = index.OK
	switch {
	case !index.OK:
		rep.RegisterState = registerUnreachable
		// The finding carries the reader's own error where there is one, so the
		// note points at it rather than printing it a second time.
		if s.addBadRegister(rep, index) {
			rep.RegisterNote = "no orphaned rows were counted, so the findings are a floor; BAD REGISTER above says why"
		} else {
			rep.RegisterNote = fmt.Sprintf("the follow-up list was not read, so no orphaned rows were counted: %v", index.Reason())
		}
		return
	case index.Off:
		rep.RegisterState = registerOff
		return
	case index.Truncated(s.cfg.GitHub.PageLimit):
		rep.RegisterState = registerTruncated
		rep.RegisterNote = fmt.Sprintf("the follow-up list hit the %d-issue page limit, so the orphaned count is a floor", s.cfg.GitHub.PageLimit)
	default:
		rep.RegisterState = registerRead
	}
	s.addUnresolvedRegister(rep, index)
	s.addRowClash(rep, index)
	s.addDormant(rep, index)
	s.addNoClosesKey(rep, index)
	s.addUnlinked(rep, index)
}

// addNoClosesKey reports the project that cannot record what a merge resolved.
//
// [frontmatter] is closed, so an item carries closes only where the project
// declares it, and link, resolve and finish read that one key for everything they
// do to a tracker. Undeclared, every routed issue is unlinkable: link exits 3,
// finish closes nothing, and whoever filed the report gets silence. A project
// whose frontmatter lists, or whose per-user config, leave the key out is in
// this state having done nothing wrong.
//
// Gated on the register holding routed rows, because a project routing nothing
// owes no key. That is also what makes it degrade the way ORPHANED FOLLOW-UP does: a
// register drover could not reach produces no finding rather than a false one.
func (s *session) addNoClosesKey(rep *CheckReport, index gh.Index) {
	if s.cfg.Known(closesKey) {
		return
	}
	ids := make([]string, 0, len(s.backlog.Items))
	for _, it := range s.backlog.Items {
		ids = append(ids, it.ID.Bare())
	}
	owned := len(index.For(ids...))
	if owned == 0 {
		return
	}
	rep.Findings = append(rep.Findings, Finding{
		Kind: "NO CLOSES KEY",
		Message: fmt.Sprintf("%d open %s owned by an item and [frontmatter] declares no %q, so no merge here can close one",
			owned, plural(owned, "issue is", "issues are"), closesKey),
		Detail: fmt.Sprintf("add %q to required or optional in [frontmatter]; until then drover link exits 3 and finish writes nothing to the tracker", closesKey),
	})
}

// addUnlinked reports the routed issue a merge is about to walk past.
//
// finish reads the closes key and nothing else, so an issue routed to an item by
// label but absent from that key is closed by nobody: the run may have fixed it
// and the report still reads as open. The window where that is answerable is
// narrow, which is why the finding is: it fires only while the owner has run
// and not merged, so the operator meets the question at the one moment both
// answers are still cheap.
//
// Every owner having merged is ORPHANED FOLLOW-UP's state, not this one. There the fix has
// either shipped unrecorded or was never written, the branch is gone, and the
// remedy is a later item rather than a key. Firing on both would report one
// issue twice and name two different fixes for it.
//
// A run log is the test for "has run" because it is the same one status uses.
// An item nothing has executed owes no answer yet: its routed issues are the
// backlog working as intended.
func (s *session) addUnlinked(rep *CheckReport, index gh.Index) {
	// NO CLOSES KEY owns the undeclared case. Every routed issue in the project
	// is unclaimable then, and one finding against the config beats one per issue
	// each naming a command that exits 3.
	if !s.cfg.Known(closesKey) {
		return
	}
	logDir := s.cfg.LogDir()
	for _, it := range s.backlog.Items {
		id := it.ID.Bare()
		if s.done(id) || s.isScaffold(it) {
			continue
		}
		if !runlog.Ran(logDir, s.cfg.Paths.LogPrefix+runlog.Base(it.ID.Stem(), it.Slug)) {
			continue
		}
		claimed := backlog.Ints(it.Get(closesKey))
		for _, is := range index.For(id) {
			if containsInt(claimed, is.Number) {
				continue
			}
			rep.Findings = append(rep.Findings, Finding{
				Kind: "UNLINKED",
				Message: fmt.Sprintf("#%d routes to %s, which has run and not merged, and its %s key names %s",
					is.Number, id, closesKey, renderInts(claimed)),
				Detail: fmt.Sprintf("if the run fixed it: drover link %s %d, then resolve it. If a later run owes it, leave the key alone and finish will not touch #%d",
					id, is.Number, is.Number),
				ID: id,
			})
		}
	}
}

// addUnresolvedRegister is the finding for a zero nobody can act on: a read that
// succeeded, returned nothing, and asked a question this project's config cannot
// have an answer to.
//
// Only on a zero, because a register holding rows resolved by demonstration. The
// count alone cannot separate the two states, so each backend consults a second
// fact instead: whether the repository defines the label at all, and whether the
// directory the path points into exists. Both leave the state every project
// starts in silent — a label that exists and holds nothing, a directory that
// exists and holds no file — which is the whole reason the count is not the test.
//
// Neither message says the register is empty. That is the claim drover cannot
// make here and the reason the finding exists.
func (s *session) addUnresolvedRegister(rep *CheckReport, index gh.Index) {
	if len(index.Issues) > 0 {
		return
	}
	switch s.cfg.RegisterBackend() {
	case config.BackendGitHub:
		label := s.cfg.GitHub.FollowUpLabel
		defined, err := gh.DefinesLabel(s.labels(), label)
		if err != nil {
			// A note and no finding, following ORPHANED FOLLOW-UP: an outage is never a
			// reason to fail CI, and this one cannot even report a floor.
			rep.RegisterNote = fmt.Sprintf("the label set was not read, so a zero here is not evidence %s defines %q: %v",
				s.trackerName(), label, err)
			return
		}
		if defined {
			return
		}
		rep.RegisterState = registerUnresolved
		rep.Findings = append(rep.Findings, Finding{
			Kind:    "NO LABEL",
			Message: fmt.Sprintf("[github] follow_up_label = %q is not a label %s defines, and the read returned nothing", label, s.trackerName()),
			Detail:  "create the label or correct the key; every row filed under another name answers no query drover makes",
		})
	case config.BackendFile:
		path := s.cfg.Abs(s.cfg.Paths.Register)
		dir := filepath.Dir(path)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			rep.RegisterState = registerUnresolved
			rep.Findings = append(rep.Findings, Finding{
				Kind:    "NO REGISTER",
				Message: fmt.Sprintf("[paths] register = %q points into %s, which does not exist", s.cfg.Paths.Register, rel(s.cfg.Root, dir)),
				Detail:  "create the directory or correct the key; no row can have been appended under this path",
				Path:    s.cfg.Paths.Register,
			})
			return
		}
		// A file that is on disk and read nothing is an empty register, and
		// stays silent. What reaches below is a path with no file at all.
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return
		}
		stray, rows := strayRegister(dir, path)
		if stray == "" {
			return
		}
		rep.RegisterState = registerUnresolved
		rep.Findings = append(rep.Findings, Finding{
			Kind: "NO REGISTER",
			Message: fmt.Sprintf("[paths] register = %q is absent, and %s beside it holds %d follow-up %s",
				s.cfg.Paths.Register, rel(s.cfg.Root, stray), rows, plural(rows, "row", "rows")),
			Detail: fmt.Sprintf("point the key at %s, or rename the file; register.Append creates what it writes to, so rows parsed out of a file drover was never asked to read are a path that changed under the project rather than a follow-up list nobody has filed against",
				rel(s.cfg.Root, stray)),
			Path: s.cfg.Paths.Register,
		})
	}
}

// strayRegister looks for a register beside the one the config names.
//
// It is the second fact behind NO REGISTER under the file backend, and it exists
// because the count cannot be one: a project that has filed nothing reads zero
// rows under a correct path and a misspelled one alike. register.Append creates
// the file it writes to, so rows sitting at a path drover was never asked to
// read are a key that no longer names where they went.
//
// Files in this one directory, never a walk, and only a file whose every line
// validates as a register row. A ledger row carries no number and no state, and
// prose fails to parse as JSON, so both are rejected on their first line.
func strayRegister(dir, configured string) (path string, rows int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		candidate := filepath.Join(dir, e.Name())
		if candidate == configured {
			continue
		}
		f, err := register.Read(candidate)
		if err != nil || len(f.Rows) == 0 {
			continue
		}
		return candidate, len(f.Rows)
	}
	return "", 0
}

// addRowClash reports one issue sitting in both sets: an open register row whose
// number is the issue an open backlog item was taken in from.
//
// intake drops the follow-up label as it promotes, so this state is what a failed
// removal leaves behind — the row keeps travelling as work nothing is carrying
// while the item that carries it is open. Neither side reports it alone: the
// register reads a labelled row, the backlog reads an ordinary item.
//
// The ledger's state is what makes it two claims on one issue. A merged item
// carries nothing, so the clause naming why the label matters would be false on
// every row it fired against, and the leftover it names is already counted by
// ORPHANED FOLLOW-UP once the row's own owners merge.
//
// The tracker backend only. A file register numbers its own rows, so a number
// matching an item's issue: key there is a coincidence rather than a claim.
//
// Parsed items only: a body whose frontmatter did not parse already carries
// UNREADABLE, and reporting it twice names one file for two fixes.
func (s *session) addRowClash(rep *CheckReport, index gh.Index) {
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		return
	}
	claimed := map[int]string{}
	for _, it := range s.backlog.Items {
		n, err := strconv.Atoi(strings.TrimSpace(it.Get("issue")))
		if err != nil {
			continue
		}
		if s.done(it.ID.Bare()) {
			continue
		}
		claimed[n] = it.ID.Bare()
	}
	for _, is := range index.Issues {
		id, ok := claimed[is.Number]
		if !ok {
			continue
		}
		rep.Findings = append(rep.Findings, Finding{
			Kind:    "DUPLICATE ISSUE",
			Message: fmt.Sprintf("#%d is an open follow-up and the issue %s was taken in from", is.Number, id),
			Detail: fmt.Sprintf("remove the %q label from #%d; intake drops it as it promotes, and the row reads as work no run carries while %s carries it",
				s.cfg.GitHub.FollowUpLabel, is.Number, id),
			ID: id,
		})
	}
}

// addDormant reports the rows nothing is scheduled to reach.
func (s *session) addDormant(rep *CheckReport, index gh.Index) {
	n := s.registerCounts(index).Orphaned
	if n == 0 {
		return
	}
	rep.Findings = append(rep.Findings, Finding{
		Kind: "ORPHANED FOLLOW-UP",
		Message: fmt.Sprintf("%d open %s orphaned: every owner has merged, so no run is scheduled to reach %s",
			n, plural(n, "follow-up is", "follow-ups are"), plural(n, "it", "them")),
		Detail: fmt.Sprintf("an item declaring %s on its deps: line inherits %s; drover follow-ups lists the rows",
			plural(n, "that owner id", "one of those owner ids"), plural(n, "it", "them")),
	})
}

// addBadRegister reports a register file the project owns and cannot read.
//
// Only the file backend, because it is the only one that opens the path: gh
// failing is an outage, and a project on none has no path at all. Absence is
// deliberately not here — register.Read answers a missing file with an empty
// register, which is where every project starts, so a finding would fail CI on
// the day one declares the backend. What reaches this is a file on disk that
// will not open, or a line that is not a register row.
//
// The message is register.Read's own error, relative to the root: it already
// names the file, the line and the cause, and a generic sentence sends the
// reader back to run the command by hand. Reporting the path a second time in
// the detail said nothing the message had not, so the detail carries the fix
// instead, which is what every other finding puts there.
//
// Reports whether it filed, because the caller's note is the floor marker and
// must not repeat an error this finding already carries.
func (s *session) addBadRegister(rep *CheckReport, index gh.Index) bool {
	if s.cfg.RegisterBackend() != config.BackendFile {
		return false
	}
	rep.Findings = append(rep.Findings, Finding{
		Kind: "BAD REGISTER",
		Message: strings.ReplaceAll(index.Reason().Error(),
			s.cfg.Abs(s.cfg.Paths.Register), s.cfg.Paths.Register),
		Detail: "correct the line or the file mode; an absent file is an empty follow-up list, never this",
		Path:   s.cfg.Paths.Register,
	})
	return true
}

// bodyPathBare is bodyPath for the bare ids the ledger carries.
func (s *session) bodyPathBare(bare string) (string, bool) {
	id, err := backlog.ParseID(bare, s.cfg.Prefixes())
	if err != nil {
		return "", false
	}
	return s.bodyPath(id)
}

// shelfCollision names the two keys that resolved to one directory. They do not
// always carry one spelling: a case variant and a symlink both reach this, and
// reporting either as "both name work" describes a config nobody wrote.
func (s *session) shelfCollision() string {
	if s.cfg.Paths.Shelf == s.cfg.Paths.Backlog {
		return fmt.Sprintf("[paths] shelf and backlog both name %s", s.cfg.Paths.Backlog)
	}
	return fmt.Sprintf("[paths] shelf %s and bodies %s are one directory", s.cfg.Paths.Shelf, s.cfg.Paths.Backlog)
}

// sameDir reports whether two configured paths name one directory. The string
// comparison this replaced answered a question drover was not asking: filepath
// cleaning catches ./work/../work and nothing else, so a symlinked shelf on any
// platform, and a case variant on the case-insensitive filesystems this is
// developed on, read as two directories and every live body clashed with
// itself. os.SameFile reads the identity the shelf key is about.
//
// A path with no stat is not the same directory as one that exists: a shelf
// that does not exist is NO SHELF, which is a different finding.
func sameDir(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

func init() {
	checkCmd.Flags().BoolVar(&checkJSON, "json", false, "emit the report as JSON")
	root.AddCommand(checkCmd)
}

// unmatchedDenies is a finding per [drive] protected_paths entry that matches no
// tracked file. It reads trackedNames through denyMatch, the same transport and
// matcher the merge hold reads changedNames through, so an entry this counts as
// covering a file holds a branch that changes it. A tree git cannot list is one
// finding saying so rather than every entry reading as dead.
func (s *session) unmatchedDenies() []Finding {
	files, err := s.trackedNames()
	if err != nil {
		return []Finding{{
			Kind:    "PROTECTED UNMATCHED",
			Message: "could not list tracked files to check [drive] protected_paths against",
			Detail:  err.Error(),
		}}
	}
	var found []Finding
	for _, g := range s.cfg.Loop.DenyPaths {
		hit := false
		for _, f := range files {
			if denyMatch(g, f) {
				hit = true
				break
			}
		}
		if !hit {
			found = append(found, Finding{
				Kind:    "PROTECTED UNMATCHED",
				Message: fmt.Sprintf("[drive] protected_paths entry %q matches no tracked file", g),
				Detail: "it holds no merge; name paths relative to the repository root, with a trailing / for a directory, " +
					"or remove the entry",
			})
		}
	}
	return found
}

// sequenceClashes is a check finding per group of tracked files sharing a
// number in a [numbered_files] glob. drive catches a branch that would put one
// there; this catches the one a hand merge already did.
func (s *session) sequenceClashes() []Finding {
	if len(s.cfg.Sequences) == 0 {
		return nil
	}
	files, err := s.trackedNames()
	if err != nil {
		return []Finding{{
			Kind:    "SEQUENCE CLASH",
			Message: "could not list tracked files to check [numbered_files] against",
			Detail:  err.Error(),
		}}
	}
	var found []Finding
	for _, name := range sortedKeys(s.cfg.Sequences) {
		for _, g := range clashes(numbered(s.cfg.Sequences[name], files)) {
			paths := make([]string, len(g))
			for i, f := range g {
				paths[i] = f.path
			}
			found = append(found, Finding{
				Kind:    "SEQUENCE CLASH",
				Message: fmt.Sprintf("[numbered_files] %s: %s share %s", name, strings.Join(paths, ", "), g[0].digits),
				Detail:  "renumber the later one above the highest, and change what refers to its old number; drover never renumbers",
				Path:    g[0].path,
			})
		}
	}
	return found
}
