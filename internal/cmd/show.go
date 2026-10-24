package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/spf13/cobra"
)

var (
	showJSON bool
	// showNoTracker skips the forge reads: the pull request and the register.
	showNoTracker bool
)

// showArgs is needID with one optional word after it. A word that is not
// detail is refused rather than ignored: show B1 deatil printing the summary
// alone reads as an item with nothing in its body.
func showArgs(c *cobra.Command, args []string) error {
	if len(args) == 2 {
		if args[1] != "detail" {
			return fmt.Errorf("unknown argument %q: drover show <id> [detail]\nthe only word show takes after the id is detail, which prints the body", args[1])
		}
		args = args[:1]
	}
	return needID("show <id> [detail]")(c, args)
}

// ShowOperator is one [operator] requirement and whether anyone attested it.
type ShowOperator struct {
	Num       int    `json:"num"`
	Text      string `json:"text"`
	SignedOff bool   `json:"signed_off"`
}

// ShowSplit is one split: the item split, the item it wrote, and which
// direction the dependency between them runs, "--after" when From waits on
// Into and "--before" when Into waits on From.
type ShowSplit struct {
	From      string `json:"from"`
	Into      string `json:"into"`
	Direction string `json:"direction"`
}

// line is the split as show prints it.
func (sp ShowSplit) line() string {
	if sp.Direction == "--before" {
		return fmt.Sprintf("%s split out of %s, --before: %s waits on %s", sp.Into, sp.From, sp.Into, sp.From)
	}
	return fmt.Sprintf("%s split out of %s, --after: %s waits on %s", sp.Into, sp.From, sp.From, sp.Into)
}

// splitsOf reads the split record from the item's own body and from every body
// split out of it.
func (s *session) splitsOf(it backlog.Item) []ShowSplit {
	id := it.ID.Bare()
	var out []ShowSplit
	if m := splitLine.FindStringSubmatch(it.Body); m != nil {
		out = append(out, ShowSplit{From: m[1], Into: id, Direction: "--" + m[2]})
	}
	for _, other := range s.backlog.Items {
		if m := splitLine.FindStringSubmatch(other.Body); m != nil && m[1] == id && other.ID.Bare() != id {
			out = append(out, ShowSplit{From: id, Into: other.ID.Bare(), Direction: "--" + m[2]})
		}
	}
	return out
}

// Show is everything drover knows about one item, and the unit of --json.
type Show struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	State  string            `json:"state"`
	Path   string            `json:"path"`
	Branch string            `json:"branch"`
	Meta   map[string]string `json:"meta,omitempty"`
	Deps   []ShowDep         `json:"deps,omitempty"`
	// Splits is every split this item took part in, from the line split
	// writes into the new item's body: the one it came out of, and each one
	// split out of it.
	Splits []ShowSplit `json:"splits,omitempty"`
	Reqs   int         `json:"requirements"`
	// Operator is every [operator] requirement, each saying whether the
	// operator has attested it. A pending one holds the merge.
	Operator []ShowOperator `json:"operator,omitempty"`
	Verify   string         `json:"verify,omitempty"`
	Report   string         `json:"report,omitempty"`
	Logs     []string       `json:"logs,omitempty"`
	// Modified is the body's file-system mtime as a date. It needs nothing but
	// the file, so it is the one timestamp drover can always answer with.
	Modified string `json:"modified,omitempty"`
	// Created is the date of the commit that added the body, and CreatedUnknown
	// is why there is none. A body the project keeps outside git has no creation
	// date to give, and a zero there would read as a file with no history rather
	// than as a question nobody could ask.
	Created        string `json:"created,omitempty"`
	CreatedUnknown string `json:"created_unknown,omitempty"`
	// Implementer and Reviewer are the runners that ran the two stages, as
	// drover resolved them. Absent means nobody recorded a run of that stage,
	// which is a different answer from the project default.
	Implementer string `json:"implementer,omitempty"`
	Reviewer    string `json:"review_runner,omitempty"`
	// Runs is how many times the item was launched for implement. There is no
	// audit count beside it because the audits array below already is one, and a
	// second spelling of the same number is a second thing to keep in step.
	//
	// Reviews is printed and never encoded. It counts only the launches drover
	// resolved a runner for, so a project that pipes `drover review` reads zero
	// forever, and a JSON field whose meaning depends on how the brief was
	// delivered is a trap. The line that prints it says what it counts.
	Runs    int `json:"runs,omitempty"`
	Reviews int `json:"-"`
	// Rounds is how many times a review sent this item back, and it carries no
	// omitempty: a zero is the answer, and omitting it leaves a consumer unable
	// to tell "never sent back" from a missing field. MaxReviewRounds and Capped are
	// the project's limit and whether the next request-changes refuses.
	Rounds          int  `json:"rounds"`
	MaxReviewRounds int  `json:"max_review_rounds,omitempty"`
	Capped          bool `json:"at_limit,omitempty"`
	// Pass is what the most recent drive read from this item's last review, and
	// PassStale says the item has run since, which leaves the pass a record of
	// that pass rather than a statement about the branch as it stands. Absent
	// means no drive recorded one.
	Pass      string `json:"review_result,omitempty"`
	PassStale bool   `json:"review_result_stale,omitempty"`
	// Parked is the drive park standing on the item, as status reads it:
	// absent once anything launched the item again.
	Parked *ShowPark `json:"parked,omitempty"`
	// Recent is how big the last commits on the branch were, and RecentUnknown
	// is why there are none. Same pair as Created above, for the same reason: a
	// branch that merged and was deleted has no sizes to give, and a zero would
	// read as a round that changed nothing.
	Recent        []prompts.Commit `json:"recent,omitempty"`
	RecentUnknown string           `json:"recent_unknown,omitempty"`
	// Audits is every audit row for this item, oldest first. The last one is
	// current; the ones before it are how the assessment moved, which a single
	// level cannot show.
	Audits []ledger.Row `json:"audits,omitempty"`
	// Dispositions is what the item's closes key has been judged to be, plus a
	// row per issue nobody has judged yet. Ledger-only: show reaches no tracker,
	// so these carry no issue title and no tracker state.
	Dispositions []ledger.Resolution `json:"dispositions,omitempty"`
	Done         bool                `json:"merged"`
	// PR is the pull request from the item's branch, with its URL and the
	// head's check conclusion. Absent when the forge lists none, or the project
	// has no branches; a read that did not complete is present with its error.
	PR        *PRState   `json:"pr,omitempty"`
	FollowUps []gh.Issue `json:"follow_ups,omitempty"`
	// noPRs is a project with no pull requests to report: no branches, or no
	// GitHub remote. print then leaves the pr line out, as JSON leaves out pr.
	noPRs bool
	// s links the issue and pull request numbers print writes. Nil writes
	// them plain, which is what a Show built outside a session gets.
	s *session
	// Body is set only under detail, so its absence in --json means nobody
	// asked rather than an empty prompt.
	Body string `json:"body,omitempty"`
}

// authorship is how a row's --code-by reads in show: who wrote the code, and
// whether the row was recorded by one of them. by alone cannot say the second,
// since it names authors, not graders.
func authorship(by string, independent bool) string {
	switch {
	case by == "":
		return "no authorship"
	case independent:
		return "code by " + by
	default:
		return "code by " + by + ", audited by its author"
	}
}

// ShowPark is the park a drive recorded: the cause, the detail, the line that
// is the detail's evidence, the log it came from, and when.
type ShowPark struct {
	Park     string `json:"park"`
	Detail   string `json:"detail,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Log      string `json:"log,omitempty"`
	Date     string `json:"date"`
}

// ShowDep is a dependency and how well it is known.
type ShowDep struct {
	ID string `json:"id"`
	// State is what drover found behind the id, in check's terms: readable,
	// unparsed, shelved, absent or unresolved. An id with nothing behind it is
	// not a dependency audited UNAUDITED, it is not a dependency, and the fields
	// below describe a foundation only when this is readable.
	State       idState `json:"state"`
	Done        bool    `json:"merged"`
	Level       string  `json:"level,omitempty"`
	By          string  `json:"code_by,omitempty"`
	Independent bool    `json:"independent,omitempty"`
	Weak        bool    `json:"weak"`
}

var showCmd = &cobra.Command{
	Use:   "show <id> [detail]",
	Short: "Everything drover knows about one item.",
	Long: `State, metadata, dependencies with their audit levels, the full audit history,
the run logs, the saved report, and the follow-ups it owns.

The audit history is the part no other command gives you: ` + "`status`" + ` shows the
current level, and a level that was corrected downward is only legible next to
the one it replaced.

detail appends the item's body: everything below the frontmatter fence, which
is the prompt a run is handed, printed as it sits on disk. With --json it adds
the same text as body.`,
	Example: `  drover show 1                     # a planned item
  drover show B1                    # an intake item; bare id, not the padded B01
  drover show B1 detail             # the same, then the body the run is handed

  drover show B1 --json             # one object:
    {"id":"B1","state":"merged","requirements":4,"verify":"{verify}",
     "modified":"2026-03-04","created":"2026-02-19",
     "implementer":"claude","review_runner":"codex","runs":3,"rounds":2,
     "pr":{"number":236,"state":"merged","url":"https://github.com/o/r/pull/236","checks":"success"},
     "deps":[{"id":"2","state":"readable","merged":true,"level":"reviewed","weak":true}],"audits":[...]}

  a dep's state is what drover found behind the id: readable, unparsed,
  shelved, absent or unresolved. Only readable carries an audit level worth
  weighing; the rest are not foundations at all.

  pr is the pull request from the item's branch: state is open, draft, merged,
  closed or unknown, checks is the head's conclusion, and error names a forge
  read that did not complete. --no-tracker asks the forge nothing.

  implementer and review_runner are the runners drover resolved for run and
  review. reviewer, on an audit row, is the person or session that looked.
  Either is absent when no run of that stage was recorded, which is not the
  same answer as the project default.

  modified is the body's mtime and is always there. created is the commit that
  added it, so it is absent for a body git does not track, and created_unknown
  then says which question drover could not ask.

  requirements counts the top-level "N. " lines, which is what report --save
  turns into checkboxes. A zero on an item that reads as fully specified means
  the requirements were written as prose and nothing will ever confirm them.`,
	Args: showArgs,
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		s.offline = showNoTracker
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		id := it.ID.Bare()

		st := s.ledger.State(id)
		sh := Show{
			ID: id, Title: it.Title(), Path: rel(s.cfg.Root, it.Path),
			Meta: it.Meta, Reqs: len(it.Requirements()),
			Done: st.Merged, Implementer: st.Implementer, Reviewer: st.Reviewer,
		}
		index := gh.Index{Err: errNoTracker}
		if !showNoTracker {
			index = s.register()
		}
		if d, err := s.promptDataWith(it, index); err == nil {
			sh.Branch, sh.Verify = d.Branch, d.VerifyCommand
		}
		sh.PR, sh.noPRs = s.showPR(id, sh.Branch, st.Merged, !showNoTracker)
		// The undisposed half comes from the shared definition, so show cannot
		// disagree with finish and check about which issues are still open
		// questions. resolve's help promises this listing.
		sh.Dispositions = s.ledger.Resolutions(id)
		for _, n := range undisposed(s.ledger, id, backlog.Ints(it.Get(closesKey))) {
			sh.Dispositions = append(sh.Dispositions, ledger.Resolution{Issue: n})
		}
		sort.Slice(sh.Dispositions, func(i, j int) bool {
			return sh.Dispositions[i].Issue < sh.Dispositions[j].Issue
		})
		attested := s.ledger.SignedOff(id)
		for _, r := range operatorRequirements(it) {
			sh.Operator = append(sh.Operator, ShowOperator{Num: r.Num, Text: r.Text, SignedOff: attested[r.Num]})
		}
		c := s.ledger.Counts(id)
		sh.Runs, sh.Reviews, sh.Rounds = c.Of(ledger.StageImplement), c.Of(ledger.StageReview), c.ChangesRequested
		sh.MaxReviewRounds = s.cfg.Review.MaxReviewRounds
		sh.Capped = sh.MaxReviewRounds > 0 && sh.Rounds >= sh.MaxReviewRounds
		sh.Pass, _, sh.PassStale, _ = s.ledger.Pass(id)
		if pk, ok := s.ledger.Parked(id); ok {
			sh.Parked = &ShowPark{Park: pk.Park, Detail: pk.Detail, Evidence: pk.Evidence, Log: pk.Log, Date: pk.Date}
		}
		sh.Recent, sh.RecentUnknown = s.recentCommits(sh.Branch)
		for _, dep := range it.Deps() {
			// The ledger and the tree answer different questions. Grading a dep
			// the ledger has never heard of reads as an audited foundation, which
			// is the one thing an id with no body behind it is not.
			state, _ := s.resolve(dep)
			ds := s.ledger.State(dep)
			// weak is drover's result on a foundation, and an id with no body
			// behind it is not one, so the state gates it here exactly as it
			// verifyCommands the table's flag. done, level and by stay unscoped: those
			// echo ledger rows, and a shelved item that merged did merge.
			sh.Deps = append(sh.Deps, ShowDep{
				ID: dep, State: state, Done: ds.Merged, Level: ds.Level, By: ds.CodeBy, Independent: ds.Independent,
				Weak: state == idReadable && weakFoundation(ds),
			})
		}
		sh.Splits = s.splitsOf(it)
		for _, r := range s.ledger.Rows {
			if r.ID == id && r.Level != "" {
				sh.Audits = append(sh.Audits, r)
			}
		}
		if p := s.reportPath(it); fileExists(p) {
			sh.Report = rel(s.cfg.Root, p)
		}
		if info, err := os.Stat(it.Path); err == nil {
			sh.Modified = info.ModTime().Format(dateOnly)
		}
		sh.Created, sh.CreatedUnknown = s.created(it.Path)
		base := s.cfg.Paths.LogPrefix + runlog.Base(it.ID.Stem(), it.Slug)
		if logs, err := runlog.Find(s.cfg.LogDir(), base); err == nil {
			sh.Logs = logs
		}
		for _, row := range s.rows() {
			if row.ID == id {
				sh.State = row.State
			}
		}
		if index.OK {
			sh.FollowUps = index.For(id)
		}

		if len(args) == 2 {
			sh.Body = it.Body
		}

		if showJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(sh)
		}
		sh.s = s
		sh.print(s.cfg)
		return nil
	},
}

// print takes the config rather than reading a package global, so which
// metadata line gets a colour is the project's decision, not the renderer's.
func (sh Show) print(cfg *config.Config) {
	s := sh.s
	fmt.Printf("%s  %s\n", sh.ID, sh.Title)
	fmt.Printf("  state      %s\n", sh.State)
	if p := sh.Parked; p != nil {
		fmt.Printf("  parked     %s (%s) on %s\n", p.Park, blankDetail(p.Detail), p.Date)
		for _, line := range strings.Split(p.Evidence, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Printf("             %s\n", trunc(line, 160))
			}
		}
		if p.Log != "" {
			fmt.Printf("  park log   %s\n", displayPath(cfg.Root, p.Log))
		}
	}
	fmt.Printf("  body       %s  (%d numbered %s)\n", sh.Path, sh.Reqs, plural(sh.Reqs, "requirement", "requirements"))
	for i, o := range sh.Operator {
		label := "  operator   "
		if i > 0 {
			label = "             "
		}
		if o.SignedOff {
			fmt.Printf("%sR%d signed off: %s\n", label, o.Num, o.Text)
			continue
		}
		fmt.Printf("%sR%d pending: %s  (drover signoff %s %d \"<what was checked>\")\n", label, o.Num, o.Text, sh.ID, o.Num)
	}
	if sh.Branch != "" {
		fmt.Printf("  branch     %s\n", sh.Branch)
		if !sh.noPRs {
			fmt.Printf("  pr         %s\n", sh.PR.line(s))
		}
	}
	if sh.Modified != "" {
		fmt.Printf("  modified   %s\n", sh.Modified)
	}
	switch {
	case sh.Created != "":
		fmt.Printf("  created    %s\n", sh.Created)
	case sh.CreatedUnknown != "":
		fmt.Printf("  created    unknown (%s)\n", sh.CreatedUnknown)
	}
	tint := colour()
	for _, k := range sortedKeys(sh.Meta) {
		if v := sh.Meta[k]; v != "" && k != "title" {
			if k == cfg.Severity.Key {
				v = sgr(v, cfg.SevColour(v), tint)
			}
			fmt.Printf("  %-10s %s\n", k, v)
		}
	}

	for i, sp := range sh.Splits {
		label := "  split      "
		if i > 0 {
			label = "             "
		}
		fmt.Printf("%s%s\n", label, sp.line())
	}

	if len(sh.Deps) > 0 {
		fmt.Printf("\n  dependencies\n")
		for _, d := range sh.Deps {
			// Nothing resolved behind the id, so there is no level to weigh and no
			// foundation to flag. phrase is empty for a body drover read, which is
			// the only state the audit columns describe.
			if p := d.State.phrase(); p != "" {
				fmt.Printf("      %-5s %s\n", d.ID, p)
				continue
			}
			note := "audited " + d.Level + " (" + authorship(d.By, d.Independent) + ")"
			if d.Level == "" {
				note = "UNAUDITED"
			}
			// done is status's word for merged, and an unmerged dep is not a weak
			// foundation, it is not a foundation. The table dropped both facts and
			// printed a level alone.
			if d.Done {
				note = "done, " + note
			} else {
				note = "not done, " + note
			}
			flag := "  "
			if d.Done && d.Weak {
				flag = "⚠ "
			}
			fmt.Printf("    %s%-5s %s\n", flag, d.ID, note)
		}
	}

	fmt.Printf("\n  audit history\n")
	fmt.Printf("    implemented by  %s%s\n", runnerOrUnknown(sh.Implementer), rounds(sh.Runs, "runs"))
	fmt.Printf("    reviewed by     %s%s\n", runnerOrUnknown(sh.Reviewer), rounds(sh.Reviews, "launches drover resolved a runner for"))
	if sh.Rounds > 0 {
		limit := ""
		if sh.MaxReviewRounds > 0 {
			limit = fmt.Sprintf(" of %d", sh.MaxReviewRounds)
		}
		capped := ""
		if sh.Capped {
			capped = "  (at-limit — the next request-changes refuses)"
		}
		fmt.Printf("    sent back      %d%s%s\n", sh.Rounds, limit, capped)
	}
	if sh.RecentUnknown != "" {
		fmt.Printf("    recent commits  unknown (%s)\n", sh.RecentUnknown)
	} else if len(sh.Recent) > 0 {
		fmt.Printf("    recent commits  %s lines changed\n", prompts.Recent{Commits: sh.Recent}.Sizes())
	}
	// Oldest first, so a downgrade reads as the correction it was.
	if len(sh.Audits) > 0 {
		for _, a := range sh.Audits {
			by := authorship(a.CodeBy, a.Independent)
			// The runners above are what drover observed; this is who the
			// operator says looked, and it is the only place a human reviewer
			// appears at all.
			if a.Reviewer != "" {
				by += ", reviewed by " + a.Reviewer
			}
			fmt.Printf("    %s  %-12s %s\n", a.Date, a.Level, by)
			if a.Note != "" {
				fmt.Printf("      %s\n", trunc(a.Note, 96))
			}
		}
	} else {
		fmt.Printf("    (no audit level — nobody has recorded a look)\n")
	}

	if len(sh.Dispositions) > 0 {
		fmt.Printf("\n  dispositions\n")
		for _, d := range sh.Dispositions {
			if d.Disposition == "" {
				fmt.Printf("    %s %-11s %s\n", padRef(s.registerRefTo(os.Stdout, d.Issue), d.Issue), "—", "undisposed")
				continue
			}
			by := authorship(d.CodeBy, d.Independent)
			fmt.Printf("    %s %-11s %-22s %s%s\n", padRef(s.registerRefTo(os.Stdout, d.Issue), d.Issue), d.Disposition, by, d.Date, mark(d))
		}
	}

	if sh.Report != "" {
		fmt.Printf("\n  report     %s\n", sh.Report)
	}
	if n := len(sh.Logs); n > 0 {
		fmt.Printf("  logs       %d, newest %s\n", n, sh.Logs[0])
	}
	if sh.Verify != "" {
		fmt.Printf("  verify     %s\n", sh.Verify)
	}
	for _, group := range []struct {
		label string
		rows  []gh.Issue
	}{{"follow-ups", sh.FollowUps}} {
		if len(group.rows) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", group.label)
		for _, is := range group.rows {
			sev := is.Severity
			if sev == "" {
				sev = "-"
			}
			fmt.Printf("    %s [%s] %s\n", padRef(s.registerRefTo(os.Stdout, is.Number), is.Number), sev, trunc(is.Title, 80))
		}
	}
	if sh.Body != "" {
		// Unindented, unlike every line above, so the prompt copies out of a
		// terminal the way it sits in the file.
		fmt.Printf("\n  body\n\n%s", strings.TrimLeft(sh.Body, "\n"))
		if !strings.HasSuffix(sh.Body, "\n") {
			fmt.Println()
		}
	}
}

// dateOnly is the format every date drover prints uses: the ledger rows, the
// audit history, and the two on an item's body.
const dateOnly = "2006-01-02"

// created is the date of the commit that added a body, or the reason there is
// none. Both are returned because they are answers to different questions, and
// collapsing the second into an empty string is what turns "drover did not ask"
// into "this file has no history".
//
// The git read is skipped entirely when the project declares no work tree,
// which is read through On rather than off Enabled: the field is a pointer so
// that unset and false stay apart, and a Config assembled without Load has a
// nil there.
func (s *session) created(path string) (string, string) {
	if !s.cfg.Git.On() {
		return "", "[git] enabled is false — drover did not ask git"
	}
	// --follow keeps the answer right across a shelve round trip, which renames
	// the body into another directory and back.
	out, err := gitcmd.Command(s.cfg.Root, "log",
		"--diff-filter=A", "--follow", "--format=%ad", "--date=short", "--", path).Output()
	if err != nil {
		return "", "git could not read this tree"
	}
	dates := strings.Fields(string(out))
	if len(dates) == 0 {
		return "", fmt.Sprintf("%s is not tracked in the work tree", rel(s.cfg.Root, path))
	}
	// Oldest last: git log is newest first, and a body added, deleted and
	// re-added has two adds.
	return dates[len(dates)-1], ""
}

// runnerOrUnknown renders a runner nobody recorded as unknown rather than as
// the project default. The two are the answers this record exists to keep
// apart: one says a runner ran the stage, the other says nothing did or that
// whatever did went unrecorded.
// rounds is the parenthetical beside a runner, and nothing at all below two.
// One run of an item is every item that went right the first time.
//
// The caller supplies the plural whole rather than a noun to be suffixed: one
// of the two reads "review launches drover resolved a runner for", which no
// pluralizer gets right.
func rounds(n int, plural string) string {
	if n < 2 {
		return ""
	}
	return fmt.Sprintf("  (%d %s)", n, plural)
}

// mark names the two states that stop a disposition being an open question, in
// the order they bind: applied reached a tracker other people read, settled is
// agreement inside this file. Both need saying, because they have different
// remedies and only one of them is still correctable here.
func mark(r ledger.Resolution) string {
	switch {
	case r.Applied != "":
		return "  " + r.Applied + " on the tracker"
	case r.Settled:
		return "  settled"
	}
	return ""
}

func runnerOrUnknown(name string) string {
	if name == "" {
		return "unknown (no run recorded)"
	}
	return name
}

func init() {
	showCmd.Flags().BoolVar(&showJSON, "json", false, "emit as JSON")
	showCmd.Flags().BoolVar(&showNoTracker, "no-tracker", false,
		"ask the forge nothing: the pull request reads unknown unless the ledger answers, and follow-ups are omitted")
	root.AddCommand(showCmd)
}
