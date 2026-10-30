package cmd

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// supervised is one action a drive can meet that somebody has to handle: drive
// itself, the supervising session watching it, or the operator. The catalog is
// the one list of them, so the config, the picker, the brief, the event stream
// and the generated instructions all name the same keys.
type supervised struct {
	key      string
	category string
	// description completes "this action is ...", one line, for the picker and
	// every table that lists the catalog.
	description string
	def         config.Policy
	// ceiling is the highest policy a project may set. It sits where a person
	// has to stay in the loop: accept is operator/operator because a session
	// that accepted requirements would be certifying its own contract.
	ceiling config.Policy
	// command is the drover command whose invocation is this action, which is
	// what the supervisor gate keys on. Empty for an action drive performs or a
	// session performs by hand.
	command string
	// supervisor is what a supervising session does when the action is at
	// supervisor, and what it reports. The brief renders it.
	supervisor string
	// operator is what to run when the action stops at operator, carried by a
	// needs_operator event and sent to each [notify] destination routing it. {id}
	// is the item.
	operator string
}

// Categories, in the order the picker walks them and every table lists them.
const (
	catMerge        = "merge"
	catChecks       = "checks"
	catSessions     = "sessions"
	catRequirements = "requirements"
	catQueue        = "queue"
	catPreflight    = "preflight"
)

var categories = []string{catMerge, catChecks, catSessions, catRequirements, catQueue, catPreflight}

const (
	polOff        = config.PolicyOff
	polOperator   = config.PolicyOperator
	polSupervisor = config.PolicySupervisor
	polAuto       = config.PolicyAuto
)

// catalog is every supervised action. update_open_prs, retry_moved_trunk and
// resolve_conflict decide what drive does. A key whose behavior drive does not
// act on yet is honored as the behavior drive already has, which is what the
// defaults describe.
var catalog = []supervised{
	{key: "update_open_prs", category: catMerge, def: polAuto, ceiling: polAuto,
		description: "after a merge, merge the trunk into every other open drive pull request, run its verify command, and push",
		supervisor:  "Merge the trunk into each open drive branch in its worktree, run the item's verify command, and push without force. Report each branch and the verify command's exit.",
		operator:    "merge the trunk into the open drive branches, run each one's verify command and push it"},
	{key: "retry_moved_trunk", category: catMerge, def: polAuto, ceiling: polAuto,
		description: "retry, once, a merge gate that refused only because the trunk moved under the head",
		supervisor:  "Update the branch and rerun the merge gate once. Report both exits.",
		operator:    "update {id}'s branch from the trunk and run the merge gate again"},
	{key: "resolve_conflict", category: catMerge, def: polSupervisor, ceiling: polAuto,
		description: "resolve a branch update merge conflict in the item's worktree",
		supervisor:  "Resolve the conflict in the worktree the park names, run the item's verify command, commit and push without force, then `drover drive {id}`. Report the files and the verify command's exit.",
		operator:    "resolve the conflict on {id}'s branch, then drover drive {id}"},
	{key: "merge_drive_pr", category: catMerge, def: polOperator, ceiling: polSupervisor,
		command:     "finish",
		description: "merge a pull request drive opened for an item, then finish it",
		supervisor:  mergeDrivePR,
		operator:    "review {id}'s pull request, merge it by hand, then drover finish {id}"},
	{key: "merge_protected_paths", category: catMerge, def: polOperator, ceiling: polSupervisor,
		description: "merge a pull request that changes a [drive] protected_paths file",
		supervisor:  mergeDrivePR + " Before merging, read the diff of each file [drive] protected_paths matches and name each one in the report.",
		operator:    "review {id}'s pull request, merge it by hand, then drover finish {id}"},

	{key: "rerun_infra_failure", category: catChecks, def: polAuto, ceiling: polAuto,
		description: "rerun a check that failed on the forge's infrastructure rather than on the code",
		supervisor:  "Rerun the failed jobs with `gh run rerun --failed`, at most twice per head. Report the check and the log line that marked it infrastructure.",
		operator:    "rerun the failed check on {id}'s pull request"},
	{key: "report_trunk_red", category: catChecks, def: polOperator, ceiling: polAuto,
		description: "report a trunk that fails the verify command on its own, and stop dispatching",
		supervisor:  "Find the commit that turned the trunk red and report it with the failing output. Fix nothing on an item's branch.",
		operator:    "fix the trunk, then drover drive {id}"},
	{key: "post_finish_check", category: catChecks, def: polOff, ceiling: polAuto,
		description: "run a live check after each finish",
		supervisor:  "Run the project's post-finish check against what finished and report its first line.",
		operator:    "run the post-finish check for {id}"},

	{key: "rerun_lost_review", category: catSessions, def: polAuto, ceiling: polAuto,
		description: "rerun, once, a review pass whose session died or was cut",
		supervisor:  "Read the review log the park names, then `drover drive {id}`. Report the cause.",
		operator:    "read the review log, then drover drive {id}"},
	{key: "relaunch_after_runner_death", category: catSessions, def: polSupervisor, ceiling: polAuto,
		description: "relaunch an item whose implementing session died",
		supervisor:  "Read the log the park names, then `drover drive {id}`. Report the cause, and stop relaunching after the second death.",
		operator:    "read the implement log, then drover drive {id}"},
	{key: "auth_expired", category: catSessions, def: polOperator, ceiling: polOperator,
		description: "stop when a runner's credentials expired",
		operator:    "log the runner in again, then drover drive {id}"},
	{key: "push_refused_by_hook", category: catSessions, def: polOperator, ceiling: polOperator,
		description: "stop when a pre-push hook refused the item's branch",
		operator:    "read the hook's output, fix {id}'s branch, then drover drive {id}"},

	{key: "write_scaffold_requirements", category: catRequirements, def: polSupervisor, ceiling: polSupervisor,
		description: "write the numbered requirements of a scaffold a review filed",
		supervisor:  "Write the scaffold's numbered requirements, citing files and lines, and leave the stop marker in place. Report the item: accepting it is the operator's.",
		operator:    "write {id}'s requirements, then drover accept {id}"},
	{key: "triage_finding", category: catRequirements, def: polOperator, ceiling: polSupervisor,
		description: "find an owner for a follow-up a drive's session filed that nothing is scheduled to carry",
		supervisor:  "Read follow-up #{ref} and the item that filed it, then give it an owner a run will reach or say why it should close. Report the follow-up, its coverage and the owner you gave it.",
		operator:    "read follow-up #{ref} (drover follow-ups --coverage), then give it an owner a run will reach, or close it"},
	{key: "close_changes", category: catRequirements, def: polSupervisor, ceiling: polSupervisor,
		command:     "close-changes",
		description: "close a sent-back section whose findings a later pass closed",
		supervisor:  "Run `drover close-changes {id}` once the item's latest review passed with every finding closed. Report the item.",
		operator:    "drover close-changes {id}"},
	{key: "accept", category: catRequirements, def: polOperator, ceiling: polOperator,
		command:     "accept",
		description: "accept an item's requirements",
		operator:    "read {id}'s requirements, then drover accept {id}"},
	{key: "override_review_limit", category: catRequirements, def: polOperator, ceiling: polOperator,
		command:     "request-changes --over-limit",
		description: "send an item back past [review] max_review_rounds",
		operator:    "read {id}'s last review, then drover request-changes {id} --over-limit or shelve it"},
	{key: "decide_requirement", category: catRequirements, def: polOperator, ceiling: polOperator,
		description: "decide a requirement a review found no implementing session can meet as written",
		operator:    "read {id}'s review, then drover amend {id} -f <file> --reason \"<why>\" to change the contract, or drover signoff {id} <n> \"<what was checked>\" where a person can meet it"},
	{key: "move_requirement", category: catRequirements, def: polOperator, ceiling: polOperator,
		command:     "amend",
		description: "move a numbered requirement from one item to another",
		operator:    "drover amend {id} -f <file> --reason \"<why>\" --moved <other id>, then the same on the other item"},
	{key: "signoff", category: catRequirements, def: polOperator, ceiling: polOperator,
		description: "meet a requirement only a person can meet",
		operator:    "meet {id}'s [operator] requirements, then drover signoff {id} <n> \"<what was checked>\""},

	{key: "enqueue", category: catQueue, def: polSupervisor, ceiling: polAuto,
		command:     "drive",
		description: "queue the next drive",
		supervisor:  "Queue work with `drover drive --enqueue <ids> --json --events`: it appends to the running drive, or starts one when none is running. Report the ids and why each is ready.",
		operator:    "drover drive --enqueue {id}"},
	{key: "pause_for_config_change", category: catQueue, def: polOperator, ceiling: polSupervisor,
		description: "pause a running drive to take a config change",
		supervisor:  "Pause the drive and resume it once the operator has changed the config. Never edit .drover.toml yourself.",
		operator:    "pause the drive, change the config, resume it"},
	{key: "cleanup_interrupted_drive", category: catQueue, def: polSupervisor, ceiling: polAuto,
		description: "remove the worktrees and branches an interrupted drive left behind",
		supervisor:  "List what the interrupted drive left and remove only clean worktrees. Report what was kept and why.",
		operator:    "inspect and remove what the interrupted drive left"},
	{key: "host_load_admission", category: catQueue, def: polOff, ceiling: polAuto,
		description: "hold dispatch while the host is loaded",
		supervisor:  "Hold the next drive while the host's load average exceeds its CPU count. Report the hold.",
		operator:    "wait for the host's load to fall, then drover drive {id}"},
	{key: "usage_admission", category: catQueue, def: polAuto, ceiling: polAuto,
		description: "hold dispatch at a [usage] pause level, on purchased credit under [usage.overage], and on a quota refusal, and resume at the window's reset",
		supervisor:  "Report the hold: the window, the percent left and the reset. Resuming spends what the operator kept back, so leave drive --resume to them unless pause_for_config_change is supervisor.",
		operator:    "wait for the window to reset, or drover drive --resume to spend what is left"},
	{key: "drive_digest", category: catQueue, def: polAuto, ceiling: polAuto,
		description: "summarize each drive when it exits",
		supervisor:  "Summarize the drive from its exit event: what merged, what parked and why.",
		operator:    "read the drive's summary"},

	{key: "stale_citations", category: catPreflight, def: polAuto, ceiling: polAuto,
		description: "report cited lines that no longer exist on the trunk before launching",
		supervisor:  "Check each queued item's cited lines against the trunk and report what moved.",
		operator:    "update {id}'s citations, then drover drive {id}"},
	{key: "worktree_links_gap", category: catPreflight, def: polAuto, ceiling: polAuto,
		description: "report files the verify command reads that the worktree lacks",
		supervisor:  "Report each untracked file the verify command reads that [drive] worktree_links does not link.",
		operator:    "add the path to [drive] worktree_links"},
	{key: "deps_lint", category: catPreflight, def: polAuto, ceiling: polAuto,
		description: "refuse a malformed deps line",
		supervisor:  "Report the malformed deps line with its corrected form.",
		operator:    "correct {id}'s deps line"},
}

// mergeDrivePR is what a supervisor does with a pull request at
// merge_drive_pr = "supervisor". finish checks the two ledger conditions itself;
// the forge's two are the session's to read, because finish cannot see a check
// that is still running.
const mergeDrivePR = "Merge a pull request drive opened for {id} with the project's [git] merge_method, then run `drover finish {id}`, only when all of these hold: the ledger names the pull request on a row drive wrote; the item's latest review pass is `passed` and came from a session drover launched; every check on the head is green; and the forge reports it mergeable. Report each merge."

// parkActions names the catalog action each park drive already makes belongs
// to, so the event stream and [notify] can say who owns it before the
// item implementing the action has changed what drive does about it.
var parkActions = map[string]string{
	parkTrunkRed:            "report_trunk_red",
	parkInfraFailure:        "rerun_infra_failure",
	parkTrunkConflict:       "resolve_conflict",
	"protected_paths":       "merge_protected_paths",
	parkOperatorRequirement: "signoff",
	"blocked_on_deps":       "write_scaffold_requirements",
	parkImplementDied:       "relaunch_after_runner_death",
	passDied:                "rerun_lost_review",
	passNeedsDecision:       "decide_requirement",
	parkDrained:             "cleanup_interrupted_drive",
	"interrupted":           "cleanup_interrupted_drive",
	parkRefDrift:            "stale_citations",
}

// parkAction is the catalog action that owns one park, or empty. The detail
// decides two of them: a runner fault is auth_expired when the runner had no
// credentials, and a refused push is push_refused_by_hook when the project's
// own hook refused it. Every other fault and push failure keeps the owner it
// had, which is none.
func parkAction(d DriveItem) string {
	switch {
	case d.Park == "runner_fault" && d.Detail == detailAuth:
		return "auth_expired"
	case d.Park == "push_failed" && d.Detail == detailPushHook:
		return "push_refused_by_hook"
	}
	return parkActions[d.Park]
}

// flagActions are the supervised actions a drive flag invokes. The gate asks
// these before the command's own action, because a pause or a cleanup is not
// the enqueue every other drive is.
var flagActions = map[string]string{
	"drive --pause":   "pause_for_config_change",
	"drive --resume":  "pause_for_config_change",
	"drive --cleanup": "cleanup_interrupted_drive",
}

// invocation is the name the gate judges a command by: the top-level command,
// or that command and the flag when the flag is a supervised action of its own.
func invocation(c *cobra.Command) string {
	name := topLevel(c)
	for inv := range flagActions {
		cmd, flag, _ := strings.Cut(inv, " --")
		if f := c.Flags().Lookup(flag); cmd == name && f != nil && f.Value.String() == "true" {
			return inv
		}
	}
	if f := c.Flags().Lookup("list"); name == "intake" && f != nil && f.Value.String() == "true" {
		return "intake --list"
	}
	return name
}

// action looks a key up in the catalog, the zero value for a key it lacks.
func action(key string) supervised {
	i := slices.IndexFunc(catalog, func(a supervised) bool { return a.key == key })
	if i < 0 {
		return supervised{}
	}
	return catalog[i]
}

// commandAction is the action a command's invocation is, if any.
func commandAction(name string) (supervised, bool) {
	i := slices.IndexFunc(catalog, func(a supervised) bool { return a.command == name })
	if i < 0 {
		return supervised{}, false
	}
	return catalog[i], true
}

func init() {
	bounds := make(map[string]config.ActionBounds, len(catalog))
	for _, a := range catalog {
		bounds[a.key] = config.ActionBounds{Default: a.def, Ceiling: a.ceiling}
	}
	config.RegisterActions(bounds)
}

// policyRow is one action as a table renders it, with this project's policy.
type policyRow struct {
	Key         string
	Category    string
	Description string
	Policy      string
	Default     string
	Ceiling     string
	// Declared is a policy the config wrote rather than the catalog default.
	Declared bool
	// Do is what a supervisor does at supervisor; Run is what the operator
	// runs at operator.
	Do  string
	Run string
}

// policyRows is the catalog with cfg's effective policies, in catalog order.
func policyRows(cfg *config.Config) []policyRow {
	rows := make([]policyRow, 0, len(catalog))
	for _, a := range catalog {
		p, declared := cfg.Policy(a.key)
		rows = append(rows, policyRow{
			Key: a.key, Category: a.category, Description: a.description,
			Policy: string(p), Default: string(a.def), Ceiling: string(a.ceiling), Declared: declared,
			Do: a.supervisor, Run: a.operator,
		})
	}
	return rows
}

// SupervisorRefused is every command a supervising session may not run under
// cfg, sorted, each in backticks: the operator's commands, less drive while
// enqueue lets a supervisor queue one and finish while merge_drive_pr lets it
// merge, and each command that is a supervised
// action its policy keeps from a supervisor.
func SupervisorRefused(cfg *config.Config) []string {
	var out []string
	for name, k := range classified {
		if k.who != operator {
			continue
		}
		if a, ok := commandAction(name); ok && policyOpens[name] {
			if p, _ := cfg.Policy(a.key); supervisorMay(p) {
				continue
			}
		}
		out = append(out, "`"+name+"`")
	}
	for _, a := range catalog {
		if a.command == "" {
			continue
		}
		if _, sealedToo := classified[a.command]; sealedToo && classified[a.command].who == operator {
			continue
		}
		if p, _ := cfg.Policy(a.key); !supervisorMay(p) {
			out = append(out, "`"+a.command+"`")
		}
	}
	slices.Sort(out)
	return out
}

// supervisorMay reports whether a supervising session may carry out an action
// under p: at supervisor it does it and reports it, and at auto drive would do
// it anyway, so a supervisor doing it by hand changes nothing.
func supervisorMay(p config.Policy) bool { return p == polSupervisor || p == polAuto }

// policyNow loads the project's config to answer one action's policy for the
// verify. A config that will not load refuses the action rather than guessing a
// policy for it: the command would fail on the same config a moment later.
func policyNow(key string) (config.Policy, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return "", err
	}
	p, _ := cfg.Policy(key)
	return p, nil
}

// operatorOwns is the supervisor's refusal for an action its policy keeps from
// it, naming the action, the policy and who owns it, so the report it writes
// instead can say all three.
func operatorOwns(what string, a supervised, p config.Policy) error {
	return Refusal{fmt.Errorf(
		"%s is the %s action, at policy %q in this project, so the operator owns it.\n"+
			"Report what you found and let the operator run it; [supervise] %s in .drover.toml decides this",
		what, a.key, p, a.key)}
}

//go:embed supervise.tmpl
var superviseTemplate string

// superviseData is what the supervisor brief renders from.
type superviseData struct {
	Project  string
	IDs      []string
	Runner   string
	Policies []policyRow
	// Supervisor, Operator and Auto are Policies split by who acts, so the
	// brief can say what the session does without the template filtering.
	Supervisor []policyRow
	Operator   []policyRow
	Auto       []policyRow
	Notify     bool
	// Sealed is the operator's commands a supervisor may never run, whatever
	// the table says: every sealed command no policy opens.
	Sealed []string
}

func (s *session) superviseData(ids []string, runnerName string) superviseData {
	d := superviseData{Project: s.cfg.Project, IDs: ids, Runner: runnerName,
		Policies: policyRows(s.cfg), Notify: routes(s.cfg, evNeedsOperator)}
	for name, k := range classified {
		if k.who == operator && !policyOpens[name] {
			d.Sealed = append(d.Sealed, "`"+name+"`")
		}
	}
	slices.Sort(d.Sealed)
	for _, r := range d.Policies {
		placeholders := strings.NewReplacer("{id}", "<id>", "{ref}", "<n>")
		r.Do = placeholders.Replace(r.Do)
		r.Run = placeholders.Replace(r.Run)
		switch config.Policy(r.Policy) {
		case polSupervisor:
			d.Supervisor = append(d.Supervisor, r)
		case polOperator:
			d.Operator = append(d.Operator, r)
		case polAuto:
			d.Auto = append(d.Auto, r)
		}
	}
	return d
}

// superviseTemplatePath is [supervise] template as the file wrote it, for the
// launch banner, which resolves it the way it resolves the implement template's.
func (s *session) superviseTemplatePath() string {
	t, _ := s.cfg.Supervise[config.SuperviseTemplateKey].(string)
	return t
}

// renderSupervise renders the brief from [supervise] template, or the built-in
// one, the way the implement and review templates resolve.
func (s *session) renderSupervise(ids []string, runnerName string) (string, error) {
	name, text, err := prompts.Load(s.cfg.SuperviseTemplate(), "supervise", superviseTemplate)
	if err != nil {
		return "", err
	}
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, s.superviseData(ids, runnerName)); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("template %s rendered nothing", name)
	}
	return out + "\n", nil
}

var (
	superviseRunner string
	superviseDry    bool
)

var superviseCmd = &cobra.Command{
	Use:   "supervise [id...]",
	Short: "Launch a supervising session that watches drives and handles what [supervise] lets it.",
	Long: `A drive runs unattended, and somebody still has to read what it parks: a
conflict after a sibling merged, a scaffold a review filed with no requirements,
a review session that died. supervise names that somebody. It launches the
runner interactively in this checkout with DROVER_ACTOR=supervisor and a prompt
listing, for every supervised action, who owns it in this project.

Under that actor a command that is a supervised action runs only when the
action's policy is supervisor or auto, and refuses with exit 3 otherwise,
naming the action and its policy. drive is one (enqueue), so a supervisor may
queue the next drive; the sessions that drive launches are agents, never
supervisors. finish is another (merge_drive_pr), and at supervisor it still
refuses a branch whose ledger shows no pull request drive opened or no passed
review, and refuses --force at any policy. accept, open-pr and the rest of the
operator's commands stay refused whatever [supervise] says.

With ids the prompt names them as the queue to start with. --dry-run prints the
prompt and launches nothing. [supervise] template overrides the built-in prompt,
as [prompts] implement does the run's.`,
	Example: `  drover supervise --dry-run       # the prompt, with this project's policies
  drover supervise E36 E38         # a supervisor that starts by driving these two
  drover supervise --runner agy    # a runner other than [run] default_runner

  drover init supervise           # choose the policies first`,
	Args: needIDs("supervise [id...]"),
	RunE: func(c *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		for _, id := range args {
			if _, err := s.item(id); err != nil {
				return err
			}
		}
		name := superviseRunner
		if name == "" {
			name = s.cfg.Run.DefaultRunner
		}
		spec, ok := s.cfg.Runners[name]
		if !ok {
			return fmt.Errorf("unknown runner %q (declared: %s)", name, strings.Join(keys(s.cfg.Runners), ", "))
		}
		ids := make([]string, 0, len(args))
		for _, id := range args {
			it, _ := s.item(id)
			ids = append(ids, it.ID.Bare())
		}
		brief, err := s.renderSupervise(ids, name)
		if err != nil {
			return err
		}
		if superviseDry {
			fmt.Print(brief)
			return nil
		}
		if err := noNestedLaunch(); err != nil {
			return err
		}
		launchInfo{
			ID: s.cfg.Project, Stage: "supervise",
			Prompt: s.promptPath(s.superviseTemplatePath(), "supervise"),
		}.from(name, spec, runner.Interactive).write(errOut)
		_, err = runner.Run(c.Context(),
			runner.Where{Dir: s.cfg.Root, Root: s.cfg.Root, Actor: runner.ActorSupervisor},
			spec, runner.Interactive, brief, "", os.Stdout)
		return err
	},
}

var initSuperviseDefaults bool

var initSuperviseCmd = &cobra.Command{
	Use:   "supervise",
	Short: "Choose, per supervised action, who handles it in this project.",
	Long: `Walks the supervised actions by category, shows what each one is and its
default, and takes a policy up to that action's ceiling: auto (drive does it),
supervisor (a session ` + "`drover supervise`" + ` started may do it, and reports it),
operator (drover stops, notifies and waits), or off (nothing happens; today's
park stands).

drover does not rewrite .drover.toml, for the reason ` + "`drover config --help`" + ` gives:
no TOML round-trip keeps the comments that carry a project's reasoning. So when
the file has no [supervise] table the picker appends one, and when it already
has one the picker prints the table for you to paste over it. Either way it
exits 0. A policy left at its default is written commented out, so the default
still decides it, unless the project's own table already pinned it.

Over an existing table, the walk offers each action the policy that table
sets, so return keeps it, and the printed table carries its notify, template
and any key this build does not know as written. --defaults resets the
policies and still carries those lines. The per-user file is never copied in.

--defaults asks nothing. Without a terminal on stdin and without --defaults it
refuses with exit 3: a picker with nobody to ask would answer its own questions.
Inside a session drover launched, a supervisor included, it refuses with exit 3
whatever the flags: the policies bound that session, so it cannot choose them.`,
	Example: `  drover init supervise             # walk the catalog, then append or print
  drover init supervise --defaults  # every default, no questions`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if err := operatorStep("init supervise",
			"it decides what a supervising session may do, so a session it would bound cannot be the one choosing"); err != nil {
			return err
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		root, err := config.Find(wd)
		if err != nil {
			return err
		}
		path := filepath.Join(root, config.FileName)
		raw, err := os.ReadFile(path) // #nosec G304 -- the project file Find resolved
		if err != nil {
			return err
		}
		declared, err := config.KeysIn(string(raw))
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		exists := slices.ContainsFunc(declared, func(k string) bool { return k == "supervise" || strings.HasPrefix(k, "supervise.") })
		var prior superviseState
		if exists {
			in, err := config.Inspect(root)
			if err != nil {
				return err
			}
			prior = readSuperviseState(in)
		}
		chosen := map[string]config.Policy{}
		if !initSuperviseDefaults {
			if !interactive() {
				return Refusal{fmt.Errorf(
					"init supervise asks a question per action and there is no terminal to ask on.\n" +
						"Run it from a shell, or pass --defaults to take every default")}
			}
			if chosen, err = pickPolicies(newPrompter(os.Stdin, os.Stdout), prior.policies); err != nil {
				return err
			}
		}
		if initSuperviseDefaults {
			prior.policies = nil
		}
		table := superviseTable(chosen, prior)
		if exists {
			fmt.Printf("%s already has a [supervise] table, and drover does not rewrite it.\n"+
				"Paste this over it:\n\n%s", rel(root, path), table)
			return nil
		}
		return appendTable(path, raw, table, os.Stdout)
	},
}

// superviseState is what a project's own [supervise] table already says: the
// policies it sets, and every other line verbatim. A table printed to paste
// over the original has to carry both, or pasting it drops a template the
// operator relies on with nothing reporting the loss.
type superviseState struct {
	policies map[string]config.Policy
	// carried are the non-policy lines, template first, then any key this
	// build does not know, which is not the picker's to delete.
	carried []string
}

// readSuperviseState reads the project's own file, never the per-user one: a
// global value written into the project's table would pin what it inherits.
func readSuperviseState(in *config.Inspection) superviseState {
	st := superviseState{policies: map[string]config.Policy{}}
	written := in.Written()
	var rest []string
	for dotted, literal := range written {
		key, ok := strings.CutPrefix(dotted, "supervise.")
		if !ok {
			continue
		}
		// An action key always gets the picker's own line, so even a value
		// Load would refuse is replaced rather than carried beside it as a
		// duplicate key.
		if slices.ContainsFunc(catalog, func(a supervised) bool { return a.key == key }) {
			if p, declared := in.Policy(key); declared {
				st.policies[key] = p
			}
			continue
		}
		if key == config.SuperviseTemplateKey {
			continue
		}
		rest = append(rest, key+" = "+literal)
	}
	if literal, ok := written["supervise."+config.SuperviseTemplateKey]; ok {
		st.carried = append(st.carried, config.SuperviseTemplateKey+" = "+literal)
	}
	slices.Sort(rest)
	st.carried = append(st.carried, rest...)
	return st
}

// pickPolicies walks the catalog by category and returns the policy chosen for
// each action, offering the project's current policy where it sets one so that
// return keeps it. Only the policies up to an action's ceiling are offered, so
// the picker cannot produce a table Load refuses.
func pickPolicies(p *prompter, current map[string]config.Policy) (map[string]config.Policy, error) {
	out := map[string]config.Policy{}
	fmt.Fprintf(p.out, "Supervised actions. Press return to keep the bracketed choice: this project's\n"+
		"policy where it sets one, the default where it does not.\n")
	for _, cat := range categories {
		fmt.Fprintf(p.out, "\n== %s ==\n\n", cat)
		for _, a := range catalog {
			if a.category != cat {
				continue
			}
			keep := a.def
			if cur, ok := current[a.key]; ok && slices.Contains(config.Policies(), cur) && !cur.Above(a.ceiling) {
				keep = cur
			}
			var options []string
			def := 0
			for _, pol := range config.Policies() {
				if pol.Above(a.ceiling) {
					break
				}
				if pol == keep {
					def = len(options)
				}
				options = append(options, string(pol))
			}
			if len(options) == 1 {
				fmt.Fprintf(p.out, "%s: %s.\n  %s (the only policy it allows)\n\n", a.key, a.description, options[0])
				continue
			}
			i := p.choose(fmt.Sprintf("%s: %s. Default %s.", a.key, a.description, a.def), options, def)
			out[a.key] = config.Policy(options[i])
			fmt.Fprintln(p.out)
		}
	}
	if p.err != nil && p.err != io.EOF {
		return nil, fmt.Errorf("reading answers: %w", p.err)
	}
	return out, nil
}

// superviseTable renders [supervise] with prior's carried lines, then one line
// per action: live where the choice differs from the default or the project
// already pinned it, commented otherwise, so a default that changes in a later
// release still reaches a project that never chose.
func superviseTable(chosen map[string]config.Policy, prior superviseState) string {
	var b strings.Builder
	b.WriteString("[supervise]\n")
	b.WriteString("# Who handles each supervised action: auto (drive does it), supervisor (a\n")
	b.WriteString("# `drover supervise` session may, and reports it), operator (drover stops\n")
	b.WriteString("# and waits), off (nothing happens). Commented lines take the default.\n")
	if len(prior.carried) > 0 {
		b.WriteString("\n" + strings.Join(prior.carried, "\n") + "\n")
	}
	width := 0
	for _, a := range catalog {
		width = max(width, len(a.key))
	}
	for _, cat := range categories {
		fmt.Fprintf(&b, "\n# %s\n", cat)
		for _, a := range catalog {
			if a.category != cat {
				continue
			}
			p, ok := chosen[a.key]
			line := fmt.Sprintf("%-*s = %q", width, a.key, string(p))
			if !ok || (p == a.def && prior.policies[a.key] != p) {
				line = fmt.Sprintf("# %-*s = %q", width, a.key, string(a.def))
			}
			fmt.Fprintf(&b, "%s   # %s; ceiling %s\n", line, a.description, a.ceiling)
		}
	}
	return b.String()
}

// appendTable writes table to the end of the project file, separated from what
// is there by a blank line, and says what it wrote.
func appendTable(path string, raw []byte, table string, w io.Writer) error {
	sep := "\n"
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		sep = "\n\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) // #nosec G304 -- the project file Find resolved
	if err != nil {
		return err
	}
	if _, err := f.WriteString(sep + table); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(w, "appended to %s:\n\n%s", path, table)
	return nil
}

// configPolicies is the policy block `drover config` prints after the file:
// every action's effective policy, and whether a file chose it. Commented, so
// the printed config still loads when pasted whole.
func configPolicies(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("\n# Supervised actions, effective policy (see `drover supervise --help`).\n")
	b.WriteString("# \"default\" means no file sets it and the catalog decides.\n")
	width := 0
	for _, a := range catalog {
		width = max(width, len(a.key))
	}
	for _, r := range policyRows(cfg) {
		from := "default"
		if r.Declared {
			from = "[supervise]"
		}
		fmt.Fprintf(&b, "#   %-*s  %-10s  %s\n", width, r.Key, r.Policy, from)
	}
	return b.String()
}

// unsetActions is every catalog action no layered file sets, in catalog order:
// the ones running on a default nobody chose.
func unsetActions(cfg *config.Config) []supervised {
	var out []supervised
	for _, a := range catalog {
		if _, declared := cfg.Policy(a.key); !declared {
			out = append(out, a)
		}
	}
	return out
}

// atSupervisor is the keys of the actions whose default is supervisor.
func atSupervisor(actions []supervised) []string {
	var out []string
	for _, a := range actions {
		if a.def == polSupervisor {
			out = append(out, a.key)
		}
	}
	return out
}

func init() {
	superviseCmd.Flags().StringVar(&superviseRunner, "runner", "", "launch this runner instead of [run] default_runner")
	superviseCmd.Flags().BoolVar(&superviseDry, "dry-run", false, "print the prompt and launch nothing")
	root.AddCommand(superviseCmd)
	initSuperviseCmd.Flags().BoolVar(&initSuperviseDefaults, "defaults", false,
		"take every default and ask nothing; the only form that works without a terminal")
	initCmd.AddCommand(initSuperviseCmd)
}
