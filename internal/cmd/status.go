package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/dashboard"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

// Row is one line of the table, and the unit of --json.
type Row struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Column string `json:"column,omitempty"`
	// Severity is the raw frontmatter value, carried verbatim. A consumer that
	// wants the rank can map it; a rank number in JSON would silently discard
	// whatever a project writes that drover does not recognise.
	Severity string `json:"severity,omitempty"`
	Level    string `json:"level,omitempty"`
	By       string `json:"code_by,omitempty"`
	// Independent is the audit row's reading of By: the grader differed from
	// every entry. By alone cannot say, since it names authors, not graders.
	Independent bool `json:"independent,omitempty"`
	// Auditor is who the operator said looked, from the audit row's reviewer.
	// Absent means the row named nobody; audit never defaults it, so a consumer
	// must not read the absence as the project default the way it would for a
	// completion row.
	Auditor string `json:"reviewer,omitempty"`
	// Implementer and Reviewer are the runners drover resolved for the two
	// stages. Absent means no run of that stage was recorded, which a consumer
	// must not read as the project default.
	Implementer string `json:"implementer,omitempty"`
	Reviewer    string `json:"review_runner,omitempty"`
	// Runs and Audits are how many times the item was launched for implement
	// and how many audit rows it carries. Absent at zero, like every count and
	// name above: nothing recorded is a different answer from none happened.
	//
	// Review launches are deliberately not here. They count only the launches
	// drover resolved a runner for, so a project that pipes `drover review`
	// reads zero forever, and a JSON field whose meaning depends on how the
	// brief was delivered is a trap. The brief and `show` name it in prose,
	// where the qualification fits.
	Runs   int `json:"runs,omitempty"`
	Audits int `json:"audits,omitempty"`
	// Rounds is how many times a review sent this item back, and Capped says the
	// next request-changes would refuse. Both are omitempty here where show carries
	// rounds unconditionally: status is an array over the whole backlog and
	// would otherwise print a zero on every row that never went round.
	Rounds int  `json:"rounds,omitempty"`
	Capped bool `json:"at_limit,omitempty"`
	// Pass is what the most recent drive read from the item's last review, and
	// PassStale says the item has run since. Absent means no drive ever recorded
	// one, which is the manual lifecycle rather than a review that found nothing:
	// `open-pr` reads the ledger instead and says so.
	Pass      string `json:"review_result,omitempty"`
	PassStale bool   `json:"review_result_stale,omitempty"`
	// Park and Detail are the drive park standing on the item: the cause and
	// why the session or push under it failed. Absent once anything launched
	// the item again, so a present park is the reason a ran item is waiting.
	Park    string   `json:"park,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Missing []string `json:"missing,omitempty"`
	// Operator is the numbers of the item's [operator] requirements nobody has
	// attested, which hold its merge until drover signoff records them.
	Operator []int `json:"operator_pending,omitempty"`
	// Deps is every id the item declares on its deps line, and Blocks every id
	// that declares this one, merged or not, so a view links both directions
	// without reading a body.
	Deps   []string `json:"deps,omitempty"`
	Blocks []string `json:"blocks,omitempty"`
	// Issue is the tracker issue the item's frontmatter issue key names, absent
	// when it names none.
	Issue int `json:"issue,omitempty"`
	// Requirements is how many numbered requirements a scaffold already carries,
	// set on scaffold rows only. Absent means none are written yet; present means
	// the list waits on accept rather than on an author.
	Requirements int    `json:"requirements,omitempty"`
	Planned      bool   `json:"planned"`
	Current      bool   `json:"current,omitempty"`
	Error        string `json:"error,omitempty"`
	// PR is absent when the forge lists none for the item's branch, and when
	// the project has no branches. A read that did not complete is present as
	// state unknown with its error, never absent.
	PR *PRState `json:"pr,omitempty"`
	// Started is the first stage row of the item's latest run, Stage and
	// StageStarted the latest stage row, and Finished the completion row that
	// closed the run. Each is RFC 3339 and absent when its row carries no ts,
	// which every row written before ts existed does not.
	Started      string `json:"started,omitempty"`
	Stage        string `json:"stage,omitempty"`
	StageStarted string `json:"stage_started,omitempty"`
	Finished     string `json:"finished,omitempty"`
	// Queued is the item's place in a running drive's queue, absent when no
	// drive holds this checkout or the drive has not queued it.
	Queued *Queued `json:"queued,omitempty"`
	// Drive is the drive named by the item's latest row a drive wrote, absent
	// when no drive has touched it.
	Drive string `json:"drive,omitempty"`
	// ETA is an estimate of when the item's review stage ends, for an item a
	// running drive has dispatched or queued: the median of each remaining
	// stage over this project's timed rows, summed from now. Absent below
	// etaSamples timed runs of either stage, because a guess printed as a time
	// is read as a promise.
	ETA string `json:"eta,omitempty"`
}

// StatusReport is what --json emits: the rows, and who else is in this
// checkout.
//
// It is an object where every other list command emits an array, and it earns
// the exception the way `drive --json` earns its own. A lease belongs to the
// tree rather than to any item (`drive` holds one over a whole queue and names
// no id at all), so there is no row it could ride on, and a lease reported in
// the table but absent from the document would make the two outputs of one
// command disagree.
type StatusReport struct {
	Items []Row `json:"items"`
	// Lease is the holder of this checkout, absent when nobody holds it.
	Lease *lease.Lease `json:"lease,omitempty"`
	// LeaseError is why the holder could not be read, present only then. A
	// degraded read says what it could not ask; reporting it as "nobody here"
	// would be a wrong answer rather than a missing one.
	LeaseError string `json:"lease_error,omitempty"`
	// Drives is every drive the ledger records, newest first, absent before the
	// first. The one the lease names carries "running": true.
	Drives []ledger.Drive `json:"drives,omitempty"`
	// Trends is what the ledger records over the last --window days, day by
	// day and in total.
	Trends Trends `json:"trends"`
	// FindingsNeedingTriage is every open register row whose result is
	// blocked, dormant or unrouted and every scaffold, judged at this read.
	// FindingsError is why no register row was read for it: the register would
	// not answer, or was not asked. The scaffolds are listed either way.
	FindingsNeedingTriage []FiledFinding `json:"findings_needing_triage"`
	FindingsError         string         `json:"findings_error,omitempty"`
	// all is every row before --all and --sev scoped them, which is what a
	// template's attention list reads: a merged item nobody audited is waiting on
	// somebody whether or not the view shows merged rows.
	all []Row
}

func glyph(state string) string {
	switch state {
	case stateDone:
		return "[x]"
	case stateRan:
		return "[~]"
	case stateReady:
		return "[>]"
	case stateBlocked:
		return "[ ]"
	case stateScaffold:
		return "[?]"
	default:
		return "[!]"
	}
}

var (
	statusJSON  bool
	statusAll   bool
	statusOpen  bool
	statusSev   []string
	statusBySev bool
	// statusNoTracker skips every forge read status makes: the pull requests
	// and the register count in the footer.
	statusNoTracker bool
	statusTemplate  string
	statusOut       string
	statusForce     bool
	statusRender    bool
	statusTZ        string
	statusHTML      bool
	// statusPrintTemplate and statusPrintCSS print a built-in template or its
	// stylesheet and read no project, so either works outside one.
	statusPrintTemplate string
	statusPrintCSS      string
	statusWindow        int
	statusAllProjects   bool
)

var statusCmd = &cobra.Command{
	Use:   "status [id]",
	Short: "Every item, its state, and how hard anyone looked at it.",
	Long: `Open items by default: merged rows are dropped, the totals still count them,
and the footer says how many it hid, because a list that quietly shortens
itself is worse than a long one. --all is the whole census.

The default answers "what is left", which is the question anyone typing status
is holding. A backlog is mostly merged rows within a month of starting, so a
census by default buries the four rows that still need somebody under fifty
that do not.

--sev is a filter and repeats: --sev C,H keeps two ranks, and none keeps the
items declaring nothing drover ranks, which is not the lowest rank but the
absence of one. That is why follow-up open --sev takes a single rank and refuses
none: a row cannot be filed at the absence of a rank, only left without one.
--by-sev reorders worst first within the planned and intake sections; without
it the table stays in id order, which is what people navigate by.

The item whose branch is checked out is marked with ▸ and named in the
footer, and a drover command holding this checkout is named under it. status
reports that and never refuses it: the question it answers is where you are,
which now includes who else is here.

The PR column is each item's pull request and its state: open, draft, merged
or closed. One gh pr list call reads them all, matched on the branch each item
renders to, and a merged item whose number open-pr recorded answers from the
ledger without it. A read that fails says unknown and why in the column, and
status still prints everything else and exits 0. --no-tracker asks the forge
nothing.

--template <file> renders the --json object through a Go text/template instead
of the table, under the same field names, plus now. -o <path> writes the
rendered template or --json to a file through a temp file and a rename, so a
reader never sees half of it, and refuses a file already there without --force.
--render renders every [status] render pair once, as drive does after each row
it appends; those carry every item, whatever --all, --sev or --by-sev say.

--template builtin:obsidian and --template builtin:html render the templates
drover ships, and --html is the second. Both carry every item, as a [status]
render does, because a dashboard's done list is part of the view; --sev and
--by-sev are refused beside them. --print-template builtin:<name> prints one
to copy and change, and --print-css obsidian prints the CSS snippet the
Obsidian one expects under .obsidian/snippets.

Every time is reported in [status] timezone, UTC unless the project sets it;
--tz names another zone for one call: UTC, local, or an IANA name.

--json and a template carry trends: what the ledger records over the last
--window days (14 unless given), day by day and in total. Each metric says how
many items it is read from, and one with nothing to read is null, never 0.

--all-projects reads every project in the per-user registry and prints one row
each: its running drive and that drive's items, its queue, how many entries
need you, its finishes in the window and its last finish. A project that will
not read is a row carrying the error. A registered root that is gone or holds no
.drover.toml is not a project and gets no row, and neither does a project under
the OS temp directory, which is a reproduction. One footer line counts each kind
apart ("N scratch projects under <temp dir>" for the second) and names drover
projects, which lists them, and drover projects --forget <root>, which takes one
off the list. Under --json the counts are "skipped" and "scratch", and with a
template or --html the footer goes to stderr. A git worktree of a registered
project is shown under that project's row as "worktree <path>", never as a row
of its own; under --json it is the parent's "worktrees". Any project command
registers its project on first use, except in a session drover launched (see
drover projects --help). --json and --template work on it, and the template data
is {projects, now, skipped, scratch, temp_dir}; the built-in templates render it
as an index.

With an id, status is show: drover status B1 --json prints what drover show
B1 --json prints.`,
	Example: `  drover status                     # what is left; the footer says how many it hid
  drover status --all               # the whole census, merged rows included
  drover status --by-sev            # what is left, worst severity first
  drover status --sev C,H           # critical and high only
  drover status --sev none          # items declaring nothing drover recognises
  drover status --no-tracker        # ask the forge nothing; the PR column says so
  drover status B1                  # the same as drover show B1, flags included
  drover status --template docs/status.tmpl -o ~/notes/drover.md --force
                                    # the project's own view, written to a file
  drover status --render            # every [status] render pair, once
  drover status --html -o site/status.html --force
                                    # a self-contained page any static server can serve
  drover status --print-template builtin:obsidian > status.tmpl
                                    # start a template from the one drover ships
  drover status --print-css obsidian > .obsidian/snippets/drover.css
  drover status --json --tz America/Los_Angeles
                                    # every time in Pacific, whatever the ledger wrote
  drover status --json --window 30  # trends over the last 30 days
  drover status --all-projects      # one row per registered project
  drover status --all-projects --html -o ~/www/index.html --force
                                    # an index linking each project's own dashboard

  drover status --json              # the rows in backlog order, and who else is in this checkout:
    {"items":[{"id":"3","title":"Resolver","state":"blocked","missing":["2"],"planned":true},
     {"id":"B1","title":"finish refuses after done","state":"merged","level":"reviewed","code_by":"agent",
      "independent":true,"reviewer":"sam","implementer":"claude","review_runner":"codex","runs":3,
      "audits":2,"rounds":2,"planned":false,
      "pr":{"number":236,"state":"merged"}},
     {"id":"B2","title":"drive parks twice","state":"implemented","planned":false,"drive":"D2",
      "pr":{"number":240,"state":"unknown","error":"not authenticated: ..."}}],
     "lease":{"command":"drive","pid":41234,"branch":"b1-first","root":"/src/drover",
      "host":"boxcar","started":"2026-09-17T09:12:03Z","drive":"D2","label":"night run",
      "queue":["B3"],"running":["B2"],"jobs":1,"paused":true,
      "holds":[{"id":"B2","at":"review","verify":"make check"}]},
     "drives":[{"id":"D2","label":"night run","started":"2026-09-17T09:12:05Z","queued":["B2"],
      "items":[{"id":"B2","outcome":""}],"running":true}]}

  state is one of merged, implemented, ready, blocked, scaffold, broken. level and by are
  absent when nobody has audited it; by is --code-by as typed, and independent
  says the grader differed from every entry. reviewer is who the operator told
  audit had looked, and may be a person: nothing defaults it, so an absent key
  means the row named nobody rather than the project default. implementer and
  review_runner name the runners run and review recorded, and are absent when no run of that
  stage was recorded, which is again not the project default. The checked-out
  item carries "current": true. requirements is set on a scaffold row only, the
  numbered requirements already written; absent means it still needs drafting.
  pr is the item's pull request, absent when the forge lists none; state is
  open, draft, merged, closed or unknown, and error names a read that did not
  complete. deps is every id the item declares and blocks every id declaring
  it. started, stage, stage_started and finished are RFC 3339 times from the
  ledger's ts in [status] timezone or --tz, absent on rows written before ts
  existed. queued is the
  item's pid and position in a running drive's queue, and eta, an estimate, is
  when its review stage should end, absent below 5 timed runs of a stage.
  drive is the drive that wrote the item's latest drive row, and drives is
  every drive the ledger records, newest first, as drover drives --json prints
  them; the one the lease names carries "running": true.

  drover status --json --window 3   # trends, one entry per day and the total:
    {"items":[],"trends":{"window":3,"from":"2026-10-15","to":"2026-10-17",
     "days":[{"date":"2026-10-15","finished":null,"cycle_time":null,"verify":null}],
     "total":{"finished":{"value":2,"n":2},"cycle_time":{"median":23400,"p90":86400,"n":2},
      "review_passes":{"value":1.5,"n":2},"changes_rate":{"value":0.5,"n":2},
      "parks":{"value":1,"by_reason":{"no_commits":1},"n":1},
      "review_to_merge":{"median":3600,"p90":5400,"n":2},"verify":{"median":610.5,"p90":1184.2,"n":2}}}}

  Each day carries the same metrics as total. Durations are seconds, as a
  median and a nearest-rank p90; n is how many items a value is read from, verify
  command runs for verify, and a metric with nothing to read is null rather than 0.

  lease.paused is a drive --pause holds, and holds each running item it holds
  at a stage boundary with the stage it goes on to; a running id with no hold
  is still inside a stage. reexec, when present, is the binary a resumed drive
  re-executes onto once every running item holds.

  lease is present only while another drover command holds this checkout, and
  lease_error only when drover could not find out. Neither is a row, which is
  why this one command emits an object where every other list emits an array:
  a drive holds the tree over a whole queue and names no item at all.`,
	Args: statusArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if statusPrintTemplate != "" || statusPrintCSS != "" {
			return printBuiltin(args)
		}
		if len(args) == 1 {
			if err := statusAsShow(cmd); err != nil {
				return err
			}
			showJSON, showNoTracker = statusJSON, statusNoTracker
			return showCmd.RunE(showCmd, args)
		}
		if err := statusScope(cmd); err != nil {
			return err
		}
		if statusAllProjects {
			return statusAllProjectsRun()
		}
		s, err := open()
		if err != nil {
			return err
		}
		s.offline, s.window = statusNoTracker, statusWindow
		if statusTZ != "" {
			if s.zone, err = config.ParseZone(statusTZ); err != nil {
				return fmt.Errorf("--tz %q: %v (known: UTC, local, or an IANA name such as America/Los_Angeles)", statusTZ, err)
			}
		}
		if sevWanted, err = parseSevSpec(statusSev); err != nil {
			return err
		}
		now := time.Now()
		if statusRender {
			if len(s.cfg.Status.Render) == 0 {
				return fmt.Errorf("--render renders each [status] render pair, and .drover.toml declares none")
			}
			errs := s.renderAll(s.cfg.Status.Render, !statusNoTracker)
			for i, t := range s.cfg.Status.Render {
				if errs[i] == nil {
					fmt.Printf("rendered %s\n", s.cfg.Abs(t.Path))
				}
			}
			return errors.Join(errs...)
		}
		tmpl := statusTemplate
		if statusHTML {
			tmpl = dashboard.Prefix + "html"
		}
		_, builtin := dashboard.Builtin(tmpl)
		if builtin && (len(statusSev) > 0 || statusBySev) {
			return fmt.Errorf("%s renders every item, as a [status] render does: drop --sev and --by-sev, or copy it with --print-template and filter in the copy", tmpl)
		}
		if statusJSON || tmpl != "" {
			report := s.statusReport(!builtin, !statusNoTracker, now)
			var out []byte
			if tmpl != "" {
				out, err = s.renderTemplate(tmpl, report, now)
			} else {
				out, err = json.MarshalIndent(report, "", "  ")
				out = append(out, '\n')
			}
			if err != nil {
				return err
			}
			if statusOut != "" {
				return writeOutput(expandHome(statusOut), out, statusForce)
			}
			_, err = os.Stdout.Write(out)
			return err
		}
		rows := s.rows()
		s.attachTimes(rows)
		if l, busy, err := s.leaseHolder(); err == nil && busy {
			s.attachDrive(rows, l, now)
		}
		if statusBySev {
			rows = bySeverity(rows)
		}
		prErr := s.attachPRs(rows, func(r Row) bool { return (statusAll || r.State != stateDone) && keep(r) }, !statusNoTracker)
		s.printTable(os.Stdout, rows, prErr)
		return nil
	},
}

// statusArgs takes one id at most. A second word is a usage error rather than
// show's detail: status <id> answers with show's summary, and the body is
// show's to print.
func statusArgs(_ *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("one id at most: drover status [id] (got %d arguments: %s)\nfor the body, drover show %s detail",
			len(args), strings.Join(args, " "), args[0])
	}
	return nil
}

// statusAsShow refuses the flags that shape the table when status hands an id
// to show. Ignoring them would print one item while the operator believes a
// filter applied.
func statusAsShow(cmd *cobra.Command) error {
	for name, set := range map[string]bool{
		"all": statusAll, "open": cmd.Flags().Changed("open"), "sev": len(statusSev) > 0, "by-sev": statusBySev,
		"template": statusTemplate != "", "o": statusOut != "", "render": statusRender, "tz": statusTZ != "",
		"html": statusHTML, "window": cmd.Flags().Changed("window"), "all-projects": statusAllProjects,
	} {
		if set {
			return fmt.Errorf("--%s shapes the table, and status <id> is show <id>, which prints one item: drop --%s or the id", name, name)
		}
	}
	return nil
}

// statusScope refuses flag combinations that select no coherent output. --open
// stays registered, hidden, so a wrapper passing it is told what to do instead
// rather than handed cobra's unknown-flag error.
func statusScope(cmd *cobra.Command) error {
	if statusOut != "" && !statusJSON && statusTemplate == "" && !statusHTML {
		return fmt.Errorf("-o writes the rendered --template, --html or --json, and the table is neither: add one of them")
	}
	if cmd.Flags().Changed("window") {
		if statusWindow < 1 || statusWindow > maxWindow {
			return Refusal{fmt.Errorf("--window is %d: it counts days, from 1 to %d", statusWindow, maxWindow)}
		}
		if !statusJSON && statusTemplate == "" && !statusHTML && !statusRender && !statusAllProjects {
			return Refusal{fmt.Errorf("--window sizes the trends --json, a template and --all-projects report, and the table shows none: add one of them")}
		}
	}
	if cmd.Flags().Changed("open") {
		return Refusal{fmt.Errorf(
			"--open is not a flag: open items are the default, so drop it; --all is the whole census")}
	}
	return nil
}

// printBuiltin prints the built-in template or stylesheet the operator named.
func printBuiltin(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("--print-template and --print-css print what drover ships and take no id (got %s)", strings.Join(args, " "))
	}
	var text string
	var err error
	if statusPrintTemplate != "" {
		name, _ := dashboard.Builtin(statusPrintTemplate)
		text, err = dashboard.Source(name)
	} else {
		text, err = dashboard.CSS(statusPrintCSS)
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.WriteString(text)
	return err
}

func openRows(rows []Row) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if r.State != stateDone {
			out = append(out, r)
		}
	}
	return out
}

// sevWanted is the parsed --sev set, nil when the flag is absent. Package state
// because printTable must see the whole backlog, so tally stays a fact about the
// project, and skip rows as it prints.
var sevWanted map[sev.Rank]bool

// parseSevSpec turns --sev tokens into the set of ranks to keep. An
// unrecognised token is rejected naming what it could have been: silently
// matching nothing would read as "no items at that severity", which is a wrong
// answer rather than a degraded one.
func parseSevSpec(spec []string) (map[sev.Rank]bool, error) {
	if len(spec) == 0 {
		return nil, nil
	}
	want := map[sev.Rank]bool{}
	for _, tok := range spec {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.EqualFold(tok, "none") {
			want[sev.Unranked] = true
			continue
		}
		r := sev.Of(tok)
		if r == sev.Unranked {
			// Refused, not reported: the vocabulary is closed and the project
			// declared it, which is the same class of rule `follow-up --sev`
			// enforces at the same exit code.
			return nil, Refusal{fmt.Errorf("unknown severity %q (known: %s, none)",
				tok, strings.Join(sev.Letters(), ", "))}
		}
		want[r] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	return want, nil
}

// keep reports whether a row survives --sev. A broken row always survives: it
// has no readable frontmatter to rank, and hiding the one row that says the
// backlog is damaged behind a severity filter is how a filter starts lying.
func keep(r Row) bool {
	if sevWanted == nil || r.State == stateBroken {
		return true
	}
	return sevWanted[sev.Of(r.Severity)]
}

// sevRows applies --sev to a slice. Only --json uses it; the table filters
// inline so its footer can still count the whole backlog.
func sevRows(rows []Row) []Row {
	if sevWanted == nil {
		return rows
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// bySeverity reorders worst first, within the planned and intake partitions
// rather than across them — the table prints an `-- intake --` header at the
// first unplanned row, so a global sort would file planned items under it.
//
// The sort is stable, which is what makes the tiebreak right for free: rows
// arrive in id order, so equal ranks keep it without comparing ids as strings
// (where "10" sorts before "2").
func bySeverity(rows []Row) []Row {
	out := append([]Row(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Planned != b.Planned {
			return a.Planned
		}
		return sev.Of(a.Severity) > sev.Of(b.Severity)
	})
	return out
}

func (s *session) rows() []Row {
	logDir := s.cfg.LogDir()
	rows := make([]Row, 0, len(s.backlog.Items)+len(s.backlog.Broken))

	for _, b := range s.backlog.Broken {
		rows = append(rows, Row{
			ID: strings.SplitN(trimDir(b.Path), "-", 2)[0], Title: "(unreadable)",
			State: stateBroken, Error: b.Err.Error(), Planned: true,
		})
	}

	// Not being on an item's branch is the ordinary case, not a failure: the
	// marker is simply absent. Several items rendering to one branch leaves it
	// absent too — marking one would pick a winner drover has just said it
	// cannot pick, and the footer names them all instead.
	current := ""
	if _, owners, err := s.branchOwners(); err == nil && len(owners) == 1 {
		current = owners[0].ID.Bare()
	}

	for _, it := range s.backlog.Items {
		st := s.ledger.State(it.ID.Bare())
		c := s.ledger.Counts(it.ID.Bare())
		pass, _, stale, _ := s.ledger.Pass(it.ID.Bare())
		row := Row{
			ID:          it.ID.Bare(),
			Title:       it.Title(),
			Column:      s.column(it),
			Level:       st.Level,
			By:          st.CodeBy,
			Independent: st.Independent,
			Auditor:     st.Auditor,
			Runs:        c.Of(ledger.StageImplement),
			Audits:      c.Audits,
			Rounds:      c.ChangesRequested,
			Capped:      s.cfg.Review.MaxReviewRounds > 0 && c.ChangesRequested >= s.cfg.Review.MaxReviewRounds,
			Pass:        pass,
			PassStale:   stale,
			Implementer: st.Implementer,
			Reviewer:    st.Reviewer,
			Planned:     it.ID.Planned(),
			Current:     current != "" && it.ID.Bare() == current,
			Severity:    it.Get(s.cfg.Severity.Key),
			Issue:       issueOf(it),
			Drive:       s.ledger.DriveOf(it.ID.Bare()),
		}
		row.State, row.Missing = s.workState(it, logDir)
		if pk, ok := s.ledger.Parked(it.ID.Bare()); ok {
			row.Park, row.Detail = pk.Park, pk.Detail
		}
		if row.State == stateScaffold {
			row.Requirements = writtenRequirements(it)
		} else {
			for _, r := range s.pendingOperator(it) {
				row.Operator = append(row.Operator, r.Num)
			}
		}
		row.Deps = it.Deps()
		rows = append(rows, row)
	}
	blocks := map[string][]string{}
	for _, r := range rows {
		for _, d := range r.Deps {
			blocks[d] = append(blocks[d], r.ID)
		}
	}
	for i := range rows {
		rows[i].Blocks = blocks[rows[i].ID]
	}
	return rows
}

// column is the cell beside the id: the configured frontmatter key, falling
// back to the item's namespace kind when it carries no value for that key.
func (s *session) column(it backlog.Item) string {
	if key := s.cfg.Status.Column; key != "" {
		if v := it.Get(key); v != "" {
			return s.cfg.Status.ColumnPrefix + v
		}
	}
	return s.cfg.IDs.Namespaces[it.ID.Prefix]
}

// isScaffold reports whether an item still carries the intake stop marker.
//
// This is the only place the marker is matched. `status` reads it to colour a
// row, `run` to refuse a launch and `draft` to refuse a redraft, and three
// copies of one question is how two of them come to answer it differently.
func (s *session) isScaffold(it backlog.Item) bool {
	return markerLine(it.Body, s.cfg.Intake.StopMarker) >= 0
}

// writtenRequirements counts the numbered requirements that carry text. The
// scaffold template writes bare "1." and "2." as placeholders, which the
// numbered pattern never matches, so an untouched scaffold counts zero and a
// drafted one counts what its author wrote.
func writtenRequirements(it backlog.Item) int {
	n := 0
	for _, r := range it.Requirements() {
		if r.Text != "" {
			n++
		}
	}
	return n
}

// markerLine is the index of the line holding the stop marker, or -1.
//
// A whole line outside any fenced block, never a substring. A body may quote
// the marker: an item about intake carries it in a config sample or in the
// sentence describing what intake plants, and matching that quote makes
// `status` report an accepted contract as a scaffold and makes `run` refuse to
// launch or even preview it. The real marker has nothing above it — intake
// writes it as the first line under the frontmatter — so nothing a scaffold
// needs is lost by skipping fences.
func markerLine(text, marker string) int {
	if marker == "" {
		return -1
	}
	fenced := false
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced && trimmed == marker {
			return i
		}
	}
	return -1
}

// counts is the footer's arithmetic.
type counts struct {
	plannedTotal, plannedDone int
	intakeTotal, intakeDone   int
	ran, unaudited            int
	toDraft, toAccept, broken int
}

// countParts is what the footer can say about the backlog, beside
// registerParts for what it can say about the tracker. It answers on two lines:
// how far the project has come, and what is waiting on somebody. One line of
// both wrapped mid-clause on any backlog old enough to carry them all, and the
// clauses a reader acts on are the ones that got split.
func countParts(c counts) (progress, waiting []string) {
	add := func(out *[]string, format string, a ...any) { *out = append(*out, fmt.Sprintf(format, a...)) }

	// A project with no planned backlog is a supported shape, not a project
	// scoring zero. "0 of 0 planned complete" reads as the latter.
	if c.plannedTotal > 0 {
		add(&progress, "%d of %d planned complete", c.plannedDone, c.plannedTotal)
	}
	if c.intakeTotal > 0 {
		add(&progress, "%d of %d intake done", c.intakeDone, c.intakeTotal)
	}
	if c.toDraft > 0 {
		add(&waiting, "%d to draft", c.toDraft)
	}
	if c.toAccept > 0 {
		add(&waiting, "%d to accept", c.toAccept)
	}
	if c.ran > 0 {
		add(&waiting, "%d awaiting PR", c.ran)
	}
	if c.unaudited > 0 {
		add(&waiting, "%d merged unaudited (drover unaudited)", c.unaudited)
	}
	if c.broken > 0 {
		add(&waiting, "%d unreadable", c.broken)
	}
	return progress, waiting
}

// tally counts the whole backlog and knows nothing about which rows get printed.
// That separation is the point: hiding merged rows is a view, while "55 of 55
// planned complete" is a fact about the project, and a footer that shrank with
// the list would quietly restate the project as smaller than it is.
// registerParts is what the summary line can say about the tracker.
//
// Omitted rather than zeroed when the register is unreachable: "nobody filed
// one" and "nobody could ask" are different answers. A truncated read is the
// third: the counts are a floor, and a zero among them is a claim about issues
// nobody fetched, so the cut-short read is named rather than left to be
// inferred from a count that is not there.
//
// Reachability is named on registerFooter's rule: only when it is not the state
// a reader already assumes. Beside two ratios measured against a known total, a
// bare count of open rows reads as the ordinary backlog of known gaps, so a
// register nothing is scheduled to reach renders identically to a healthy one.
func registerParts(index gh.Index, limit int, t registerTally) []string {
	if !index.OK {
		return nil
	}
	var out []string
	if t.Open > 0 {
		clause := fmt.Sprintf("%d open %s", t.Open, plural(t.Open, "follow-up", "follow-ups"))
		if t.Live == 0 || t.Orphaned+t.Unowned > t.Live {
			clause += fmt.Sprintf(" · %d live", t.Live)
		}
		out = append(out, clause+" (drover follow-ups)")
	}
	if index.Truncated(limit) {
		out = append(out, fmt.Sprintf("follow-up list cut off at the %d-issue page limit", limit))
	}
	return out
}

func tally(rows []Row) counts {
	var c counts
	for _, r := range rows {
		if r.Planned {
			c.plannedTotal++
		} else {
			c.intakeTotal++
		}
		switch r.State {
		case stateDone:
			if r.Planned {
				c.plannedDone++
			} else {
				c.intakeDone++
			}
			if r.Level == "" {
				c.unaudited++
			}
		case stateRan:
			c.ran++
		case stateScaffold:
			if r.Requirements > 0 {
				c.toAccept++
			} else {
				c.toDraft++
			}
		case stateBroken:
			c.broken++
		}
	}
	return c
}

func (s *session) printTable(w io.Writer, rows []Row, prErr error) {
	fmt.Fprintf(w, "%s status  (ledger: %s)\n\n", s.cfg.Project, s.cfg.Paths.Ledger)

	// The column holds whatever the project put there, so it is sized to the
	// widest value present rather than to a guess. A fixed width silently shoves
	// every title right the moment one value runs long.
	colWidth := 5
	for _, r := range rows {
		if n := len(r.Column); n > colWidth {
			colWidth = n
		}
	}
	if colWidth > maxColumn {
		colWidth = maxColumn
	}
	// The note cell is sized the same way, from the rows that will print, so the
	// title yields what the notes need rather than a guess that "ran · pass 8 ·
	// capped · validate + PR" overruns.
	noteWidth := 0
	for _, r := range rows {
		if (!statusAll && r.State == stateDone) || !keep(r) {
			continue
		}
		if n := utf8.RuneCountInString(r.note()); n > noteWidth {
			noteWidth = n
		}
	}
	if noteWidth > maxNote {
		noteWidth = maxNote
	}
	// The PR column exists only when some printed row has a pull request to
	// name or a read to report, so a project with no forge keeps its table.
	prWidth := 0
	for _, r := range rows {
		if (!statusAll && r.State == stateDone) || !keep(r) {
			continue
		}
		prWidth = max(prWidth, r.PR.visible())
	}
	prWidth = min(prWidth, maxPR)
	width := titleWidthBeside(noteWidth+1) - (colWidth - 5)
	if prWidth > 0 {
		width -= prWidth + 1
	}
	if width < minTitle {
		width = minTitle
	}
	c := tally(rows)
	intakeSeen := false
	shown, hidden, offRank := 0, 0, 0
	var here Row
	tint := colour()
	// The cell only carries severity when the project pointed it there. Tinting
	// a wave or a phase by an axis it is not showing would be a lie in colour.
	sevColumn := s.cfg.Status.Column != "" && s.cfg.Status.Column == s.cfg.Severity.Key

	for _, r := range rows {
		if r.Current {
			here = r
		}
		if !statusAll && r.State == stateDone {
			hidden++
			continue
		}
		if !keep(r) {
			offRank++
			continue
		}
		// Deferred to the first visible intake row: on a project whose intake is
		// all merged, a header over nothing reads as a rendering bug.
		// It separates two sections, so a project with only one is owed none, and
		// the blank line above it is spacing owed only once a row has printed.
		if !r.Planned && !intakeSeen && c.plannedTotal > 0 {
			if shown > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "-- intake --\n")
			intakeSeen = true
		}
		shown++
		mark := " "
		if r.Current {
			mark = "▸"
		}
		// Padded first, coloured second: fmt counts escape bytes as width, so a
		// cell wrapped before it is padded shears every column to its right.
		cell := fmt.Sprintf("%-*s", colWidth, trunc(r.Column, colWidth))
		// Not on the current row: its whole-line green ends at the first nested
		// reset, so an inner colour would strip the marker off the rest of it.
		if sevColumn && !r.Current {
			cell = sgr(cell, s.cfg.SevColour(r.Severity), tint)
		}
		if prWidth > 0 {
			cell += " " + r.PR.cell(s, prWidth)
		}
		line := fmt.Sprintf("%s %s %-5s %s %-*s %s",
			glyph(r.State), mark, r.ID, cell, width, trunc(r.Title, width), r.note())
		fmt.Fprintln(w, paint(line, tint && r.Current))
	}
	// An empty table is an answer, and two blank lines is not a way to give it.
	// Three different things produce one, and they need different fixes: a
	// misspelled path, a project nobody has filed against, and a finished one.
	switch {
	case shown > 0:
	case offRank > 0:
		fmt.Fprintf(w, "no items at that severity (%d filtered out).\n", offRank)
	case hidden > 0:
		fmt.Fprintln(w, "nothing open.")
	case s.backlog.Missing:
		fmt.Fprintf(w, "no backlog: %s does not exist (drover check)\n", s.cfg.Paths.Backlog)
	default:
		fmt.Fprintf(w, "no items in %s\n", s.cfg.Paths.Backlog)
	}

	progress, waiting := countParts(c)
	index := gh.Index{Err: errNoTracker}
	if !statusNoTracker {
		index = s.register()
	}
	waiting = append(waiting, registerParts(index, s.cfg.GitHub.PageLimit, s.registerCounts(index))...)
	if hidden > 0 {
		progress = append(progress, fmt.Sprintf("%d done %s hidden", hidden, plural(hidden, "row", "rows")))
	}
	if offRank > 0 {
		progress = append(progress, fmt.Sprintf("%d %s hidden by --sev", offRank, plural(offRank, "row", "rows")))
	}
	if len(progress)+len(waiting) > 0 {
		fmt.Fprintln(w)
	}
	for _, line := range [][]string{progress, waiting} {
		if len(line) > 0 {
			fmt.Fprintln(w, strings.Join(line, "  ·  "))
		}
	}
	// The cell carries the reason in a word; this is what gh said, which is
	// the part someone acts on.
	if prErr != nil && prWidth > 0 {
		fmt.Fprintf(w, "pull requests unread: %v\n", prErr)
	}

	// Named in text as well as marked in the table: the marker is one glyph in a
	// hundred lines, and the colour is no help at all to a reader who cannot see
	// it. This line is the one that always answers "where am I".
	branch, owners, branchErr := s.branchOwners()
	said := true
	switch {
	// Detached HEAD or no work tree: there is no "here" to name, and the table
	// above already said everything drover knows.
	case branchErr != nil:
		said = false
	case here.ID != "":
		fmt.Fprintf(w, "\n▸ here: %s  %s  (branch %s)\n", here.ID, here.Title, branch)
	// Several owners is the one case with too many answers rather than none, so
	// it never reaches describeBranch, whose whole subject is a branch no item
	// renders to. It names the candidates in the form finish refuses them with.
	case len(owners) > 1:
		fmt.Fprintf(w, "\n▸ here: branch %s  (%s — name the id)\n", branch, bareIDs(owners))
	default:
		fmt.Fprintf(w, "\n▸ here: %s\n", describeBranch(branch, index, func(n int) string { return s.registerRefTo(os.Stdout, n) }))
	}
	s.printLease(w, said)
}

// leaseUnnamed is the holder between taking the tree and writing its account.
// Both outputs say the same sentence, because they are answering one question.
const leaseUnnamed = "a drover command holds this checkout and has not written its account yet"

// printLease names the other command in this checkout, under the line that
// answers where the reader is. "Where am I" now has a second half: a drive
// holding the tree is why the branch under you moves, and finding that out from
// ps is how the collision this footer exists to surface stays invisible.
//
// It reports and never refuses. status reads, and a read-only command that
// exits 3 on the state of somebody else's work is a command nobody can use to
// find out what is happening.
func (s *session) printLease(w io.Writer, saidHere bool) {
	lead := "\n"
	if saidHere {
		lead = ""
	}
	switch l, busy, err := s.leaseHolder(); {
	case err != nil:
		fmt.Fprintf(w, "%s▸ also here: unknown: %v\n", lead, err)
	case !busy:
	// Held, and the holder has not named itself yet. Printing pid 0 and an
	// empty command would read as a holder rather than as the microsecond it
	// is, and a reader who kills pid 0 is having a worse day than the one who
	// reads this line and runs status again.
	case l.PID == 0:
		fmt.Fprintf(w, "%s▸ also here: %s\n", lead, leaseUnnamed)
	default:
		where := ""
		if l.Branch != "" {
			where = fmt.Sprintf("  (took it on %s)", l.Branch)
		}
		what := "drover " + l.Command
		if l.Item != "" {
			what += " " + l.Item
		}
		if l.Drive != "" {
			what += " " + l.Drive
		}
		if l.Label != "" {
			what += fmt.Sprintf(" %q", l.Label)
		}
		held := make(map[string]bool, len(l.Holds))
		for _, h := range l.Holds {
			held[h.ID] = true
		}
		var moving []string
		for _, id := range l.Running {
			if !held[id] {
				moving = append(moving, id)
			}
		}
		// A paused drive lets an item finish the stage it is inside before it
		// holds it, so "paused" is said only once nothing is still moving.
		switch {
		case l.Paused && len(moving) > 0:
			what += "  pausing"
		case l.Paused:
			what += "  paused"
		}
		fmt.Fprintf(w, "%s▸ also here: %s  pid %d  since %s%s\n", lead, what, l.PID, tableTime(l.Started, s.location()), where)
		if len(moving) > 0 {
			label := "running: "
			if l.Paused {
				label = "finishing:"
			}
			fmt.Fprintf(w, "  %s %s\n", label, strings.Join(moving, " "))
		}
		if len(l.Queue) > 0 {
			fmt.Fprintf(w, "  queued:  %s\n", strings.Join(l.Queue, " "))
		}
		if len(l.Holds) > 0 {
			holds := make([]string, 0, len(l.Holds))
			for _, h := range l.Holds {
				holds = append(holds, h.ID+" before "+h.At)
			}
			fmt.Fprintf(w, "  held:    %s\n", strings.Join(holds, ", "))
		}
	}
}

// describeBranch answers "where am I" for a branch no item owns — work filed
// against the tracker rather than planned in the backlog, which is otherwise
// indistinguishable from standing on main.
//
// The issue number is read out of the branch name rather than rendered from a
// template, because no template produced it: these branches are hand-made. So
// the number is only ever reported once the register confirms it, and a branch
// carrying a year or a port number names no issue and says so.
func describeBranch(branch string, index gh.Index, ref func(int) string) string {
	if is, ok := issueForBranch(branch, index); ok {
		owners := "unowned"
		if len(is.Owners) > 0 {
			owners = strings.Join(is.Owners, " ")
		}
		return fmt.Sprintf("branch %s  %s %s  (%s)", branch, ref(is.Number), is.Title, owners)
	}
	return fmt.Sprintf("branch %s  (no item owns it)", branch)
}

// issueForBranch matches a digit run in the branch against the open register.
// Exactly one match is a match; two are a coincidence and report nothing.
func issueForBranch(branch string, index gh.Index) (gh.Issue, bool) {
	if !index.OK {
		return gh.Issue{}, false
	}
	open := map[int]gh.Issue{}
	for _, is := range index.Issues {
		open[is.Number] = is
	}
	var hits []gh.Issue
	seen := map[int]bool{}
	for _, run := range digitRuns.FindAllString(branch, -1) {
		n, err := strconv.Atoi(run)
		if err != nil {
			continue
		}
		if is, ok := open[n]; ok && !seen[n] {
			seen[n] = true
			hits = append(hits, is)
		}
	}
	if len(hits) != 1 {
		return gh.Issue{}, false
	}
	return hits[0], true
}

var digitRuns = regexp.MustCompile(`[0-9]+`)

// note is the trailing cell. A merged item that nobody inspected is the state
// worth seeing, so it is named rather than left blank, and a level with no
// authorship recorded is marked, never assumed independent.
func (r Row) note() string {
	var pending, queued string
	if len(r.Operator) > 0 {
		pending = "operator pending: R" + strings.Join(intStrings(r.Operator), " R")
	}
	if r.Queued != nil {
		queued = fmt.Sprintf("queued %d", r.Queued.Position)
		if r.Queued.WaitsFor != "" {
			queued += ", waits for " + r.Queued.WaitsFor + "'s merge"
		}
	}
	return join(" · ", r.stateNote(), pending, queued)
}

func intStrings(ns []int) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, strconv.Itoa(n))
	}
	return out
}

func (r Row) stateNote() string {
	switch r.State {
	case stateDone:
		if r.Level == "" {
			return "done · UNAUDITED"
		}
		note := "done · " + r.Level
		var who []string
		switch {
		case r.By == "":
			who = append(who, "no authorship")
		case !r.Independent:
			who = append(who, "author")
		}
		if r.Auditor != "" {
			who = append(who, r.Auditor)
		}
		if len(who) > 0 {
			note += " (" + strings.Join(who, ", ") + ")"
		}
		return note
	case stateRan:
		head, next := stateRan, "validate + PR"
		if r.Park != "" {
			head, next = "parked: "+r.Park+" ("+blankDetail(r.Detail)+")", ""
		}
		return join(" · ", head, r.pass(), r.capped(), next)
	case stateReady:
		return join(" · ", "READY", r.pass(), r.capped())
	case stateBlocked:
		return "blocked by " + strings.Join(r.Missing, " ")
	case stateScaffold:
		if r.Requirements > 0 {
			return fmt.Sprintf("drafted · %d %s · drover accept %s",
				r.Requirements, plural(r.Requirements, "req", "reqs"), r.ID)
		}
		return "scaffold · needs requirements"
	default:
		return r.Error
	}
}

// pass names a repeated round, and nothing at all on a first one.
//
// Only above one: an item nobody has re-run is every item in a young backlog,
// and "pass 1" on all of them is width spent to say the ordinary case. The
// number belongs where an operator is deciding whether to re-run, which is
// exactly the two states that carry it.
func (r Row) pass() string {
	if r.Runs < 2 {
		return ""
	}
	return fmt.Sprintf("pass %d", r.Runs)
}

// capped marks the round at which the next request-changes refuses. Passes count
// launches; this is the point where sending the item back stops being an option
// and somebody has to split it, file it, or accept it as it stands.
func (r Row) capped() string {
	if !r.Capped {
		return ""
	}
	return "at-limit"
}

// join drops the empty parts before joining, so an absent pass number leaves no
// doubled separator behind it.
func join(sep string, parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func trimDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit rows as JSON")
	statusCmd.Flags().BoolVar(&statusAll, "all", false, "include merged items; the default shows only what is left")
	statusCmd.Flags().BoolVar(&statusOpen, "open", false, "not a flag: open items are the default")
	_ = statusCmd.Flags().MarkHidden("open")
	statusCmd.Flags().StringSliceVar(&statusSev, "sev", nil,
		"keep only these ranks, repeatable: C,H,M,L or none, where none means no rank at all, not the lowest one")
	statusCmd.Flags().BoolVar(&statusBySev, "by-sev", false, "order worst severity first instead of by id")
	statusCmd.Flags().BoolVar(&statusNoTracker, "no-tracker", false,
		"ask the forge nothing: the PR column reads unknown unless the ledger answers, and the footer omits the follow-up count")
	statusCmd.Flags().StringVar(&statusTemplate, "template", "",
		"render the --json object through this Go text/template file, or builtin:obsidian or builtin:html, instead of the table")
	statusCmd.Flags().StringVarP(&statusOut, "output", "o", "",
		"write the rendered --template or --json to this path, through a temp file and a rename")
	statusCmd.Flags().BoolVar(&statusForce, "force", false, "let -o replace a file already there")
	statusCmd.Flags().BoolVar(&statusRender, "render", false, "render every [status] render pair once")
	statusCmd.Flags().StringVar(&statusTZ, "tz", "",
		"report every time in this zone instead of [status] timezone: UTC, local, or an IANA name")
	statusCmd.Flags().BoolVar(&statusHTML, "html", false, "render builtin:html, the self-contained page drover ships")
	statusCmd.Flags().StringVar(&statusPrintTemplate, "print-template", "",
		"print the built-in template builtin:<name> to copy and change, and exit")
	statusCmd.Flags().StringVar(&statusPrintCSS, "print-css", "",
		"print the stylesheet a built-in template expects (obsidian), and exit")
	statusCmd.Flags().IntVar(&statusWindow, "window", defaultWindow,
		"the days the trends cover, ending today")
	statusCmd.Flags().BoolVar(&statusAllProjects, "all-projects", false,
		"one row per project in the per-user registry instead of this project's items")
	statusCmd.MarkFlagsMutuallyExclusive("all-projects", "render")
	statusCmd.MarkFlagsMutuallyExclusive("json", "template")
	statusCmd.MarkFlagsMutuallyExclusive("html", "template")
	statusCmd.MarkFlagsMutuallyExclusive("html", "json")
	statusCmd.MarkFlagsMutuallyExclusive("render", "html")
	statusCmd.MarkFlagsMutuallyExclusive("print-template", "print-css")
	for _, other := range []string{"json", "template", "html", "render", "output"} {
		statusCmd.MarkFlagsMutuallyExclusive("print-template", other)
		statusCmd.MarkFlagsMutuallyExclusive("print-css", other)
	}
	statusCmd.MarkFlagsMutuallyExclusive("render", "json")
	statusCmd.MarkFlagsMutuallyExclusive("render", "template")
	statusCmd.MarkFlagsMutuallyExclusive("render", "output")
	root.AddCommand(statusCmd)
}
