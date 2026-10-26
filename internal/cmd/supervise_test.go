package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// everyKeyAtCeiling is a [supervise] table handing every action as far from
// the operator as its ceiling allows, which is the most a project can do.
func everyKeyAtCeiling() string {
	var b strings.Builder
	b.WriteString("\n[supervise]\n")
	for _, a := range catalog {
		fmt.Fprintf(&b, "%s = %q\n", a.key, a.ceiling)
	}
	return b.String()
}

// A ceiling clamped silently would read as an action handed away. The refusal
// names all three things the reader has to change: the key, what they asked
// for, and how far it may go.
func TestAPolicyAboveItsCeilingFailsLoad(t *testing.T) {
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+"\n[supervise]\nmerge_protected_paths = \"auto\"\n")
	_, err = config.Load(s.cfg.Root)
	if err == nil {
		t.Fatal("merge_protected_paths = \"auto\" loaded; its ceiling is supervisor")
	}
	for _, want := range []string{"supervise.merge_protected_paths", `"auto"`, `"supervisor"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// At the ceiling, and below it, the same key loads.
	for _, p := range []string{"supervisor", "operator", "off"} {
		write(t, path, string(raw)+"\n[supervise]\nmerge_protected_paths = \""+p+"\"\n")
		if _, err := config.Load(s.cfg.Root); err != nil {
			t.Errorf("merge_protected_paths = %q refused: %v", p, err)
		}
	}
	write(t, path, string(raw)+everyKeyAtCeiling())
	if _, err := config.Load(s.cfg.Root); err != nil {
		t.Errorf("every key at its ceiling refused: %v", err)
	}
}

// drover does not rewrite .drover.toml, so the picker writes only where no
// table exists to be rewritten, and prints otherwise. Both exit 0.
func TestInitSuperviseAppendsOnlyWhereNoTableExists(t *testing.T) {
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	path := filepath.Join(s.cfg.Root, config.FileName)
	before, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := capture(t, func() { runErr = execute(t, "init", "supervise", "--defaults") })
	if runErr != nil {
		t.Fatalf("setup supervise --defaults: %v", runErr)
	}
	after, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || !strings.Contains(string(after[len(before):]), "[supervise]") {
		t.Fatalf("the table was not appended after what was there:\n%s", after)
	}
	if !strings.Contains(out, "appended") {
		t.Errorf("the append was not reported:\n%s", out)
	}
	if _, err := config.Load(s.cfg.Root); err != nil {
		t.Errorf("the appended table does not load: %v", err)
	}

	out = capture(t, func() { runErr = execute(t, "init", "supervise", "--defaults") })
	if runErr != nil {
		t.Fatalf("second setup supervise exited %d: %v", Code(runErr), runErr)
	}
	again, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(after) {
		t.Error("the picker rewrote a config that already had a [supervise] table")
	}
	if !strings.Contains(out, "[supervise]") || !strings.Contains(out, "Paste") {
		t.Errorf("the table was not printed for pasting:\n%s", out)
	}

	// A table the project wrote by hand: the printed replacement is pasted over
	// it, so whatever it does not carry is deleted with no error. A lost
	// template means the supervising session is briefed from the wrong prompt.
	existing := string(before) + "\n[supervise]\n" +
		"report_trunk_red = \"auto\"\n" +
		"resolve_conflict = \"supervisor\"\n" +
		"accept = 3\n" +
		"template = \"prompt.tmpl\"\n" +
		"from_a_later_release = \"x\"\n"
	write(t, path, existing)
	carried := []string{
		`template = "prompt.tmpl"`,
		`from_a_later_release = "x"`,
	}
	out = capture(t, func() { runErr = execute(t, "init", "supervise", "--defaults") })
	if runErr != nil {
		t.Fatalf("setup supervise --defaults over a hand-written table: %v", runErr)
	}
	for _, want := range carried {
		if !strings.Contains(out, "\n"+want+"\n") {
			t.Errorf("--defaults dropped %s from the table to paste:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nreport_trunk_red ") {
		t.Errorf("--defaults kept a policy it says it resets:\n%s", out)
	}

	// The walk, answering return to everything, keeps what the project chose,
	// including a default it pinned on purpose.
	in, err := config.Inspect(s.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	prior := readSuperviseState(in)
	var asked strings.Builder
	chosen, err := pickPolicies(newPrompter(strings.NewReader(strings.Repeat("\n", len(catalog))), &asked), prior.policies)
	if err != nil {
		t.Fatal(err)
	}
	table := superviseTable(chosen, prior)
	for _, want := range append(carried, "report_trunk_red ", "resolve_conflict ") {
		if !strings.Contains(table, "\n"+want) {
			t.Errorf("return-to-everything lost %s:\n%s", want, table)
		}
	}
	if !strings.Contains(asked.String(), "Default operator.") {
		t.Errorf("the prompt no longer shows the catalog default:\n%s", asked.String())
	}

	// And the paste itself loads with every one of those settings live.
	write(t, path, string(before)+"\n"+strings.Replace(table, `from_a_later_release = "x"`+"\n", "", 1))
	cfg, err := config.Load(s.cfg.Root)
	if err != nil {
		t.Fatalf("the printed table does not load once pasted: %v", err)
	}
	if p, _ := cfg.Policy("report_trunk_red"); p != config.PolicyAuto {
		t.Errorf("report_trunk_red is %q after the paste, want auto", p)
	}
	if cfg.SuperviseTemplate() == "" {
		t.Error("template is gone after the paste")
	}
}

// With no terminal to ask on and no --defaults, the picker would answer its own
// questions.
func TestInitSuperviseRefusesWithoutATerminal(t *testing.T) {
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	// A pipe, as a script or a launched session hands it. /dev/null would do as
	// well: it is a character device and still no terminal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	stdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin; _ = r.Close() })
	if err := refuse(t, "init", "supervise"); Code(err) != ExitRefused {
		t.Errorf("exit %d (%v), want %d", Code(err), err, ExitRefused)
	}
}

// The picker offers nothing above a ceiling, so no walk can produce a table
// Load refuses.
func TestThePickerOffersNothingAboveACeiling(t *testing.T) {
	answers := strings.Repeat("4\n", len(catalog))
	var out strings.Builder
	chosen, err := pickPolicies(newPrompter(strings.NewReader(answers), &out), nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, p := range chosen {
		a := action(key)
		if p.Above(a.ceiling) {
			t.Errorf("%s picked %q above its ceiling %q", key, p, a.ceiling)
		}
	}
}

func TestASupervisorAnswersToTheActionPolicy(t *testing.T) {
	s := fixture(t, "\n[supervise]\nclose_changes = \"operator\"\n")
	t.Chdir(s.cfg.Root)
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)

	err := refuse(t, "close-changes", "1")
	if Code(err) != ExitRefused {
		t.Fatalf("clear at operator policy exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"close_changes", `"operator"`, "operator owns it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// At supervisor the gate lets it through, and whatever clear then says is
	// clear's own answer about the body, not the policy.
	s = fixture(t, "\n[supervise]\nclose_changes = \"supervisor\"\n")
	t.Chdir(s.cfg.Root)
	if err := gate("close-changes"); err != nil {
		t.Fatalf("the actor check refused clear at supervisor policy: %v", err)
	}
	if err := refuse(t, "close-changes", "1"); err != nil && strings.Contains(err.Error(), "operator owns it") {
		t.Errorf("clear at supervisor policy was refused by the policy: %v", err)
	}

	// An agent session is not a supervisor and is not asked: clear stays open
	// to it whatever [supervise] says.
	t.Setenv(runner.EnvActor, runner.ActorAgent)
	s = fixture(t, "\n[supervise]\nclose_changes = \"operator\"\n")
	t.Chdir(s.cfg.Root)
	if err := gate("close-changes"); err != nil {
		t.Errorf("an agent was refused clear by a supervisor policy: %v", err)
	}
}

// No policy opens the operator's commands, but for the two a supervised action
// is: drive (enqueue) and finish (merge_drive_pr), whose own checks are tested
// beside them. accept is an action whose ceiling is operator, and it is also
// sealed, so it refuses from both directions; the test gives every action the
// highest policy a project can set.
func TestAcceptRefusesASupervisorWhateverThePolicy(t *testing.T) {
	s := fixture(t, everyKeyAtCeiling())
	t.Chdir(s.cfg.Root)
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	for _, name := range sealedSet {
		if policyOpens[name] {
			continue
		}
		err := gate(name)
		if Code(err) != ExitRefused {
			t.Errorf("%s: a supervisor got exit %d (%v), want %d", name, Code(err), err, ExitRefused)
		}
	}
	if err := refuse(t, "accept", "1"); Code(err) != ExitRefused {
		t.Errorf("accept exited %d for a supervisor (%v), want %d", Code(err), err, ExitRefused)
	}
}

// drive is the enqueue action: a supervisor may run it, and an agent still may
// not, because an agent starting a queue of sessions is the nesting the seal
// exists to stop.
func TestDriveRunsUnderASupervisorAndRefusesUnderAnAgent(t *testing.T) {
	mergeProject(t)

	t.Setenv(runner.EnvActor, runner.ActorAgent)
	if err := refuse(t, "drive", "2", "--json"); Code(err) != ExitRefused {
		t.Fatalf("drive under an agent exited %d (%v), want %d", Code(err), err, ExitRefused)
	}

	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	rows := driveAll(t, "2")
	if len(rows) != 1 || rows[0].ID != "2" || rows[0].Outcome == outcomeSkipped {
		t.Fatalf("drive under a supervisor reported %+v, want item 2 driven", rows)
	}

	// At operator, the enqueue action is the operator's and drive refuses.
	path := filepath.Join(".", config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+"\n[supervise]\nenqueue = \"operator\"\n")
	err = refuse(t, "drive", "2", "--json")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "enqueue") {
		t.Errorf("drive at enqueue = operator exited %d (%v), want %d naming enqueue", Code(err), err, ExitRefused)
	}
}

// The sessions a supervised drive launches are agents. A supervisor launching
// a second supervisor, or anything interactive, is still the nesting runner.Run
// refuses.
func TestASupervisorLaunchesOnlyAgents(t *testing.T) {
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	spec := runner.Spec{Interactive: []string{"true"}, Headless: []string{"true"}}
	if _, err := runner.Run(t.Context(), runner.Where{Actor: runner.ActorSupervisor}, spec, runner.Headless, "x",
		filepath.Join(t.TempDir(), "a.jsonl"), os.Stderr); !errors.Is(err, runner.ErrNested) {
		t.Errorf("a supervisor started a supervisor: %v", err)
	}
	if _, err := runner.Run(t.Context(), runner.Where{}, spec, runner.Interactive, "x", "", os.Stderr); !errors.Is(err, runner.ErrNested) {
		t.Errorf("a supervisor started an interactive session: %v", err)
	}

	bin := t.TempDir()
	envOut := filepath.Join(bin, "env")
	t.Setenv("DROVER_TEST_ENV_OUT", envOut)
	stub(t, filepath.Join(bin, "probe"), "#!/bin/sh\necho \"$DROVER_ACTOR\" > \"$DROVER_TEST_ENV_OUT\"\n")
	spec = runner.Spec{Headless: []string{filepath.Join(bin, "probe")}}
	if _, err := runner.Run(t.Context(), runner.Where{}, spec, runner.Headless, "x",
		filepath.Join(t.TempDir(), "b.jsonl"), os.Stderr); err != nil {
		t.Fatalf("a supervisor's drive could not launch its agent: %v", err)
	}
	got, err := os.ReadFile(envOut) // #nosec G304 -- written by the probe above
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != runner.ActorAgent {
		t.Errorf("the launched session saw DROVER_ACTOR=%q, want %q", strings.TrimSpace(string(got)), runner.ActorAgent)
	}
}

// driveEventsOf drives through the real command with --json --events and
// returns the event lines alone.
func driveEventsOf(t *testing.T, args ...string) (evs []DriveEvent, stderr string, err error) {
	t.Helper()
	out := capture(t, func() {
		stderr = captureErr(t, func() { err = execute(t, append([]string{"drive", "--json", "--events"}, args...)...) })
	})
	for _, line := range strings.Split(out, "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Event != "" {
			evs = append(evs, e)
		}
	}
	return evs, stderr, err
}

func TestEventsCarryAParkAsItHappens(t *testing.T) {
	mergeProject(t)

	evs, _, err := driveEventsOf(t)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	var parked *DriveEvent
	seen := map[string]bool{}
	for i, e := range evs {
		seen[e.Event] = true
		if e.TS == "" {
			t.Errorf("%s event carries no ts", e.Event)
		}
		if e.Event == evParked && e.ID == "2" {
			parked = &evs[i]
		}
	}
	if parked == nil {
		t.Fatalf("no parked event for item 2 among %+v", evs)
	}
	if parked.Park != passSilent {
		t.Errorf("parked event names park %q, want %q", parked.Park, passSilent)
	}
	for _, want := range []string{evDispatched, evStage, evParked, evExit} {
		if !seen[want] {
			t.Errorf("no %s event in the stream", want)
		}
	}
	if last := evs[len(evs)-1]; last.Event != evExit || last.Code == nil || *last.Code != 0 {
		t.Errorf("the stream ends on %+v, want exit with code 0", last)
	}

	// Before anything is dispatched the drive says what it is about to run,
	// and it closes on what it left undone.
	at := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evStarted })
	if at < 0 || at > slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evDispatched }) {
		t.Fatalf("no started event ahead of the first dispatch among %+v", evs)
	}
	if st := evs[at]; st.Drive == "" || st.Jobs != 1 || !slices.Contains(st.Queue, "2") || st.Deadline != "" {
		t.Errorf("started is %+v, want the drive, its queue and --jobs, with no deadline", st)
	}
	sum := evs[len(evs)-1].Summary
	if sum == nil || sum.Ended != endedDrained || sum.Parked < 1 ||
		!slices.ContainsFunc(sum.Undone, func(u UndoneItem) bool { return u.ID == "2" && u.Park == passSilent }) {
		t.Errorf("the exit summary is %+v, want item 2's park among what the drained drive left undone", sum)
	}
}

// The exit event is the drive's exit status as a supervisor reads it, so a
// drive that fails must say so there, not only in its own return.
func TestEventsExitCarriesTheDrivesFailure(t *testing.T) {
	mergeProject(t)

	evs, _, err := driveEventsOf(t, "999")
	if err == nil {
		t.Fatal("a drive of an id with no body succeeded")
	}
	if len(evs) == 0 {
		t.Fatal("the drive emitted no events")
	}
	last := evs[len(evs)-1]
	if last.Event != evExit || last.Code == nil || *last.Code != Code(err) || last.Reason == "" {
		t.Errorf("the stream ends on %+v, want exit with code %d and a reason (drive returned %v)", last, Code(err), err)
	}
}

// The picker decides what a supervisor may do, so no session drover launched
// may run it, with or without a terminal, and the project file stays as it was.
func TestInitSuperviseRefusesALaunchedSession(t *testing.T) {
	for _, actor := range []string{runner.ActorAgent, runner.ActorSupervisor} {
		t.Run(actor, func(t *testing.T) {
			s := fixture(t, "")
			t.Chdir(s.cfg.Root)
			path := filepath.Join(s.cfg.Root, config.FileName)
			before, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(runner.EnvActor, actor)
			if err := refuse(t, "init", "supervise", "--defaults"); Code(err) != ExitRefused {
				t.Errorf("setup supervise --defaults as %s exited %d (%v), want %d", actor, Code(err), err, ExitRefused)
			}
			after, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("setup supervise as %s changed %s:\n%s", actor, config.FileName, after)
			}
		})
	}
}

func TestEventsNeedJSON(t *testing.T) {
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	if err := refuse(t, "drive", "--events"); Code(err) != ExitRefused {
		t.Errorf("--events without --json exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
}

// A notifier is a courtesy to the operator, and a broken one must not cost the
// drive anything: the park, the rows and the exit read as they would without
// it, and the failure is said once.
func TestAFailingNotifierLeavesTheDriveAlone(t *testing.T) {
	for _, c := range []struct {
		name   string
		notify func(bin string) string
	}{
		{"exits non-zero", func(bin string) string {
			stub(t, filepath.Join(bin, "notify"), "#!/bin/sh\ncat > \"$DROVER_ROOT_NOTIFIED\"\necho 'notifier broke' >&2\nexit 1\n")
			return filepath.Join(bin, "notify")
		}},
		{"is missing", func(bin string) string { return filepath.Join(bin, "no-such-notifier") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			mergeProject(t)
			bin := t.TempDir()
			notified := filepath.Join(bin, "notified.json")
			t.Setenv("DROVER_ROOT_NOTIFIED", notified)
			raw, err := os.ReadFile(config.FileName)
			if err != nil {
				t.Fatal(err)
			}
			write(t, config.FileName, string(raw)+"\n[notify.destinations.desk]\ntype = \"command\"\nargv = [\""+c.notify(bin)+"\"]\nevents = { needs_operator = \"high\" }\n")
			git(t, ".", "commit", "-q", "-am", "notify")
			git(t, ".", "push", "-q", "origin", "main")
			// A denied path holds item 1's merge, and merge_protected_paths is at
			// operator by default: the stop that owes a notification.
			t.Setenv("DROVER_TEST_ADD", "Makefile")

			evs, stderr, err := driveEventsOf(t, "1")
			if err != nil {
				t.Fatalf("a failing notifier changed the drive's exit: %d (%v)", Code(err), err)
			}
			var needs bool
			for _, e := range evs {
				if e.Event == evNeedsOperator && e.ID == "1" && e.Action == "merge_protected_paths" && e.Run != "" {
					needs = true
				}
				if e.Event == evParked && e.ID == "1" && e.Park != "protected_paths" {
					t.Errorf("item 1 parked %q, want protected_paths", e.Park)
				}
			}
			if !needs {
				t.Fatalf("no needs_operator for item 1's denied path among %+v", evs)
			}
			if n := strings.Count(stderr, "[notify.destinations.desk]"); n != 1 {
				t.Errorf("the notifier's failure was reported %d times, want once:\n%s", n, stderr)
			}
			if c.name == "exits non-zero" {
				got, err := os.ReadFile(notified) // #nosec G304 -- written by the stub above
				if err != nil {
					t.Fatalf("the notifier never ran: %v", err)
				}
				var e NotifyPayload
				if json.Unmarshal(got, &e) != nil || e.Event != evNeedsOperator || e.Priority != "high" {
					t.Errorf("the notifier was handed %q, want the needs_operator event at high", got)
				}
			}
		})
	}
}

// The brief names every action with this project's policy, and --dry-run
// launches nothing to show it.
func TestSuperviseDryRunPrintsTheBrief(t *testing.T) {
	s := fixture(t, "\n[supervise]\nresolve_conflict = \"operator\"\n")
	t.Chdir(s.cfg.Root)
	var err error
	out := capture(t, func() { err = execute(t, "supervise", "1", "--dry-run") })
	if err != nil {
		t.Fatalf("supervise --dry-run: %v", err)
	}
	for _, a := range catalog {
		if !strings.Contains(out, "`"+a.key+"`") {
			t.Errorf("the prompt does not name %s", a.key)
		}
	}
	for _, want := range []string{"| `resolve_conflict` | operator |", "drover drive 1 --json --events", "needs_operator", "`supervise`, `touch`, `unaccept`"} {
		if !strings.Contains(out, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, out)
		}
	}
}

// A stage that launches a session names its log, the file the runner's stream
// went to, so attach can follow it; a stage that launches nothing names none.
func TestStageEventsNameTheSessionLog(t *testing.T) {
	mergeProject(t)

	evs, stderr, err := driveEventsOf(t)
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	wd, _ := os.Getwd()
	cfg, err := config.Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, e := range evs {
		if e.Event != evStage {
			continue
		}
		if e.Stage != ledger.StageImplement && e.Stage != ledger.StageReview {
			if e.Log != "" {
				t.Errorf("%s stage of %s names log %q and launches no session", e.Stage, e.ID, e.Log)
			}
			continue
		}
		if e.Log == "" {
			t.Errorf("%s stage of %s names no log", e.Stage, e.ID)
			continue
		}
		path := sessionLog(cfg.Root, e.Log)
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own log directory
		if err != nil || !strings.Contains(string(b), `"type":"result"`) {
			t.Errorf("%s stage of %s names %s, which holds %q (%v), not the fake runner's stream", e.Stage, e.ID, path, b, err)
		}
		named[path] = true
	}
	written, err := filepath.Glob(filepath.Join(cfg.LogDir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) == 0 || len(written) != len(named) {
		t.Errorf("the runners wrote %v and the stage events named %v", written, named)
	}
	for _, w := range written {
		if !named[w] {
			t.Errorf("%s was written and no stage event names it", w)
		}
	}
}
