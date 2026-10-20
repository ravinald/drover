package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
)

// The preview is the read-only way to see what a run would launch, and a runner
// name on its own does not say what the project pointed it at: a declared
// runner can name any binary on PATH. The argv is the part that has to be there.
func TestPreviewNamesTheResolvedArgv(t *testing.T) {
	cfg := &config.Config{Runners: runner.Builtin()}
	cfg.Runners["local"] = runner.Spec{
		Interactive: []string{"./bin/agent", "--flag", "two words"},
		Headless:    []string{"./bin/agent", "--headless"},
		Stdin:       true,
	}
	s := &session{cfg: cfg}
	data := prompts.Data{ID: "B1", Branch: "b01-slug"}

	for _, c := range []struct {
		runner string
		mode   runner.Mode
		want   string
	}{
		{"claude", runner.Interactive, "# argv claude <prompt>"},
		{"claude", runner.Headless, "# argv claude -p --output-format stream-json --verbose <prompt>"},
		{"local", runner.Interactive, `# argv ./bin/agent --flag "two words" (prompt on stdin)`},
		{"local", runner.Headless, "# argv ./bin/agent --headless (prompt on stdin)"},
		{"missing", runner.Interactive, `# argv (unknown runner "missing"`},
	} {
		var out strings.Builder
		s.preview(&out, data, c.runner, c.mode, "built-in default", "PROMPT BODY\n")
		got := out.String()
		if !strings.Contains(got, c.want) {
			t.Errorf("%s/%s: preview does not carry %q:\n%s", c.runner, c.mode, c.want, got)
		}
		if !strings.Contains(got, "runner "+c.runner) || !strings.Contains(got, "PROMPT BODY") {
			t.Errorf("%s/%s: preview lost its header or its body:\n%s", c.runner, c.mode, got)
		}
	}
}

// stubRunner puts a named binary on the stubbed PATH so a launch can be driven
// end to end without a coding agent. emit is one line of event stream, and ""
// a runner that writes nothing.
//
// The two are one helper because the difference decides the reading: an empty
// log is the session that never started, and a log holding a terminal event is
// the one that finished. A test about what drover says afterwards has to pick.
// echo rather than cat, since the stubbed PATH holds no binary but git.
func stubRunner(t *testing.T, dir, name, emit string) {
	t.Helper()
	script := "#!/bin/sh\nexit 0\n"
	if emit != "" {
		// Keyed on the name, since one test can stub two runners with two streams.
		env := "DROVER_TEST_EMIT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		t.Setenv(env, emit)
		script = "#!/bin/sh\necho \"$" + env + "\"\n"
	}
	stub(t, filepath.Join(dir, name), script)
}

// The record has to survive the run that made it: finish and mark-merged read it back to
// attribute the work, and a log is not an answer — an interactive run writes
// none, and two runs with two runners make the newest log a guess.
func TestRunRecordsTheResolvedRunner(t *testing.T) {
	t.Cleanup(func() { runRunner = "" })
	s := fixture(t, `
[run]
default_runner = "gemini"

[runners.stub]
interactive = ["stub-agent"]
headless    = ["stub-agent"]
`)
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	stubRunner(t, stubPATH(t), "stub-agent", "")
	t.Chdir(dir)

	if err := execute(t, "run", "1", "--runner", "stub"); err != nil {
		t.Fatalf("run: %v", err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := l.State("1").Implementer; got != "stub" {
		t.Fatalf("implementer = %q, want the runner run resolved (stub)", got)
	}

	// The whole point of the row: mark-merged attributes the work without being told.
	git(t, dir, "switch", "-q", "main")
	if err := execute(t, "mark-merged", "1"); err != nil {
		t.Fatalf("done: %v", err)
	}
	l, err = ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := l.State("1").MergedRunner; got != "stub" {
		t.Errorf("done recorded agent %q, want stub — the run is the record, not the default", got)
	}
}

// The prompt is an instruction, not a label. A project with no work tree must
// not be told to commit on a branch nothing will ever create. The assertion
// tracks the branch arm's own wording, because a string this looks for and the
// template does not write is an assertion that passes by being vacuous.
func TestPromptNamesNoBranchWithoutGit(t *testing.T) {
	s := fixture(t, "\n[git]\nenabled = false\n")
	write(t, filepath.Join(s.cfg.Root, "work", "B01-nil-handler.md"),
		"---\ntitle: guard the nil handler\ndeps: \"\"\n---\n\n1. Return an error.\n")
	s = openIn(t, s.cfg.Root)

	it, err := s.item("B1")
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	d, err := s.promptDataWith(it, gh.Index{})
	if err != nil {
		t.Fatalf("promptDataWith: %v", err)
	}
	if d.Branch != "" {
		t.Errorf("branch = %q, want empty — nothing will create it", d.Branch)
	}

	name, text, err := prompts.Load("", "default", prompts.DefaultTemplate())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	body, err := prompts.Render(name, text, d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "Commit on branch") {
		t.Errorf("prompt still names a branch:\n%s", body)
	}
	if !strings.Contains(body, "Work in place") {
		t.Errorf("prompt does not say where the work goes instead:\n%s", body)
	}
}

// sevenStateProject is one item depending on every state a
// dep id can be in. The names are the states, so a failure below reads as the
// state that regressed rather than as an id.
func sevenStateProject(t *testing.T) string {
	t.Helper()
	bodies := map[string]string{
		"B01-parent.md":      body("Parent needing four kinds of dep", "B2 B3 B4 B5 B6 B7 nonsense"),
		"B02-merged.md":      body("Merged with a body", ""),
		"B03-open.md":        body("Open and ready", ""),
		"B04-unparseable.md": "---\ntitle: Broken\ndeps:\nBody.\n---\n\n1. Do it.\n",
	}
	shelved := map[string]string{"B06-shelved.md": body("Shelved", "")}
	dir := shelfProjectAt(t, "shelved", bodies, shelved)
	// B7 is the state the ledger and the tree disagree about: recorded done,
	// with no body anywhere to read.
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"B2","merged":true,"date":"2026-08-27"}`+"\n"+
			`{"id":"B7","merged":true,"date":"2026-08-27"}`+"\n")
	return dir
}

// The prompt is the only product this tool has, and it must not announce every
// dep as merged code whatever the ledger and the tree say. check and status
// classify these seven from the same files, and the brief that speaks to the
// agent has to agree with them.
func TestPromptSeparatesMergedFromReadable(t *testing.T) {
	s := openIn(t, sevenStateProject(t))
	it, err := s.item("B1")
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	d := s.localData(it)

	want := map[string]prompts.Dep{
		"B2":       {ID: "B2", Done: true},
		"B3":       {ID: "B3"},
		"B4":       {ID: "B4", Unreadable: "will not parse"},
		"B5":       {ID: "B5", Unreadable: "has no body"},
		"B6":       {ID: "B6", Unreadable: "is shelved"},
		"B7":       {ID: "B7", Done: true, Unreadable: "has no body"},
		"nonsense": {ID: "nonsense", Unreadable: "does not resolve to an item id"},
	}
	if len(d.Deps) != len(want) {
		t.Fatalf("got %d deps, want %d: %+v", len(d.Deps), len(want), d.Deps)
	}
	for _, got := range d.Deps {
		w, ok := want[got.ID]
		if !ok {
			t.Errorf("unexpected dep %q", got.ID)
			continue
		}
		if got.Done != w.Done || got.Unreadable != w.Unreadable {
			t.Errorf("%s: done=%v unreadable=%q, want done=%v unreadable=%q",
				got.ID, got.Done, got.Unreadable, w.Done, w.Unreadable)
		}
	}

	out := renderBrief(t, "default", prompts.DefaultTemplate(), d)
	for _, want := range []string{
		// Merged and unreadable at once: the ledger and the tree answer
		// different questions, and B7 is the id where they disagree. Only the
		// unreadable section can say something true about it, so only that
		// section names it — the merged line stops at B2.
		"Depends on (already merged): B2.",
		"Blocked by, not merged: B3.",
		"- B4 will not parse",
		"- B5 has no body",
		"- B6 is shelved",
		"- B7 has no body",
		"- nonsense does not resolve to an item id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
	// Scoped to the section, not the brief. A readable dep is named elsewhere
	// for other reasons (the prompt lists B2 as a foundation nobody confirmed),
	// and an unscoped search reads any of those as a claim that drover could
	// not read it.
	_, unreadable, ok := strings.Cut(out, "Deps drover could not read.")
	if !ok {
		t.Fatalf("prompt does not separate unreadable deps:\n%s", out)
	}
	for _, absent := range []string{"- B2 ", "- B3 "} {
		if strings.Contains(unreadable, absent) {
			t.Errorf("readable dep %q named as unreadable:\n%s", absent, unreadable)
		}
	}
}

// The review brief carries a copy of the same claim, and getting it wrong
// invites a reviewer to weigh how well a file that does not exist was audited.
func TestReviewBriefSeparatesMergedFromReadable(t *testing.T) {
	s := openIn(t, sevenStateProject(t))
	it, err := s.item("B1")
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	d, err := s.reviewData(it)
	if err != nil {
		t.Fatalf("reviewData: %v", err)
	}
	out := renderBrief(t, "review", prompts.ReviewTemplate(), d)

	foundations, rest, ok := strings.Cut(out, "Not merged, so not foundations.")
	if !ok {
		t.Fatalf("review prompt does not separate unmerged deps:\n%s", out)
	}
	// An unmerged dep is not a weak foundation, it is not a foundation, and
	// neither is a merged one whose body is gone: no audit level renders
	// beside either at all.
	for _, id := range []string{"B3", "B4", "B5", "B6", "B7", "nonsense"} {
		if strings.Contains(foundations, "- "+id) {
			t.Errorf("%s named as a foundation:\n%s", id, foundations)
		}
		if strings.Contains(rest, "- "+id+"  ") {
			t.Errorf("%s carries an audit level it cannot have:\n%s", id, rest)
		}
	}
	for _, want := range []string{"- B2  ⚠", "- B4 will not parse", "- B7 has no body"} {
		if !strings.Contains(out, want) {
			t.Errorf("review prompt missing %q:\n%s", want, out)
		}
	}
}

// One function answers "what is this dep". Both readers of it are driven from
// the same tree here, because two classifiers that agree when written drift
// apart later, and check and run would then disagree.
func TestCheckAndPromptClassifyDepsAlike(t *testing.T) {
	s := openIn(t, sevenStateProject(t))
	want := map[string]string{
		"B4":       `B1 depends on "B4", whose body will not parse`,
		"B5":       `B1 depends on "B5", which has no body`,
		"B6":       `B1 depends on "B6", which is shelved`,
		"B7":       `B1 depends on "B7", which has no body`,
		"nonsense": `B1 depends on "nonsense", which has no body`,
	}
	got := map[string]bool{}
	for _, f := range findings(s.checkReport(), "MISSING DEP") {
		got[f.Message] = true
	}
	for id, msg := range want {
		if !got[msg] {
			t.Errorf("%s: check does not report %q; findings: %v", id, msg, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d MISSING DEP findings, want %d: %v", len(got), len(want), got)
	}
}

func renderBrief(t *testing.T, name, text string, d prompts.Data) string {
	t.Helper()
	out, err := prompts.Render(name, text, d)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// A brief carries rows routed to the item beside rows routed to its
// dependencies, and only the register knows which is which. Dropping Owners at
// this boundary leaves the template unable to ask for a disposition on either
// without asking for one on both, so a follow-up the branch fixed closes nothing.
func TestPromptMarksFollowUpsThisItemOwns(t *testing.T) {
	s := openIn(t, shelfProject(t, map[string]string{
		"01-foundation.md": body("Foundation", ""),
		"02-budgets.md":    body("Budgets", "1"),
	}, nil))
	it, err := s.item("2")
	if err != nil {
		t.Fatalf("item: %v", err)
	}

	d, err := s.promptDataWith(it, openIndex(
		gh.Issue{Number: 81, Title: "this item's gap", Owners: []string{"2"}, Severity: "L"},
		gh.Issue{Number: 90, Title: "a dep's gap", Owners: []string{"1"}, Severity: "M"},
		gh.Issue{Number: 95, Title: "owned by both", Owners: []string{"1", "2"}},
	))
	if err != nil {
		t.Fatalf("promptDataWith: %v", err)
	}

	want := map[int]bool{81: true, 90: false, 95: true}
	if len(d.FollowUps) != len(want) {
		t.Fatalf("template saw %d rows, want %d", len(d.FollowUps), len(want))
	}
	for _, r := range d.FollowUps {
		if r.Mine != want[r.Number] {
			t.Errorf("#%d Mine = %v, want %v", r.Number, r.Mine, want[r.Number])
		}
		if len(r.Owners) == 0 {
			t.Errorf("#%d carries no owners, so the prompt cannot name who it is owned by", r.Number)
		}
	}
}

// routedTracker answers the register read with the one follow-up row, and each route
// label with what that label carries on the tracker: B1's holds the follow-up row as
// well as the bug, because gh matches the route label alone. routedFails makes
// the route-label read fail while the register still answers.
func routedTracker(t *testing.T, dir string, routedFails bool) {
	t.Helper()
	const (
		riskRow = `{"number":10,"title":"hedge on B1","body":"RISK-10\nOwner: B1","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:high"}]}`
		bugRow = `{"number":11,"title":"red test on B1","body":"REPRO-11\nOwner: B1","state":"OPEN",` +
			`"labels":[{"name":"bug"}]}`
		depRow = `{"number":12,"title":"bug on the dep","body":"REPRO-12\nOwner: B2","state":"OPEN",` +
			`"labels":[{"name":"bug"}]}`
	)
	routed := "*'Owner: B1'*) printf '%s' '[" + riskRow + "," + bugRow + "]' ;;\n" +
		"*'Owner: B2'*) printf '%s' '[" + depRow + "]' ;;\n"
	if routedFails {
		routed = "*'--search'*) echo 'HTTP 502 from the tracker' >&2; exit 1 ;;\n"
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') case \"$*\" in\n" +
		"*'--label follow-up'*) printf '%s' '[" + riskRow + "]' ;;\n" +
		routed +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	stub(t, filepath.Join(dir, "gh"), script)
}

// routedBriefProject is B1 depending on B2, both plain items, under conf.
func routedBriefProject(t *testing.T, conf string) string {
	t.Helper()
	dir := registerProject(t, conf)
	write(t, filepath.Join(dir, "work", "B01-under-review.md"), body("Under review", "B2"))
	write(t, filepath.Join(dir, "work", "B02-the-dep.md"), body("The dep", ""))
	return dir
}

// A bug routed to an item and carrying no follow-up label would reach no brief,
// because the register is read with the follow-up label server-side. Both briefs
// carry it in its own block, the follow-up row stays in the follow-ups block, and
// the route-label read returning the follow-up row too does not list it twice.
func TestRoutedIssuesOutsideTheRegisterReachBothBriefs(t *testing.T) {
	for _, args := range [][]string{{"review", "B1"}, {"run", "B1", "--dry-run"}} {
		t.Run(args[0], func(t *testing.T) {
			dir := routedBriefProject(t, ghProject)
			routedTracker(t, stubPATH(t), false)
			t.Chdir(dir)

			var err error
			out := capture(t, func() { err = execute(t, args...) })
			if err != nil {
				t.Fatalf("%s: %v\n%s", args[0], err, out)
			}
			for _, n := range []string{"#10 ", "#11 ", "#12 "} {
				if got := strings.Count(out, n); got != 1 {
					t.Errorf("%s appears %d times, want once:\n%s", n, got, out)
				}
			}
			block := strings.Index(out, "that are not follow-ups")
			if block < 0 {
				t.Fatalf("no owned block:\n%s", out)
			}
			if risk := strings.Index(out, "#10 "); risk > block {
				t.Errorf("the follow-up row rendered in the owned block:\n%s", out)
			}
			for _, n := range []string{"#11 red test on B1 [bug] · owned by B1", "#12 bug on the dep [bug] · owned by B2"} {
				if i := strings.Index(out, n); i < block {
					t.Errorf("want %q inside the owned block:\n%s", n, out)
				}
			}
			// The owner's report rides with it; the dep's stays a headline,
			// the same rule the follow-ups block follows.
			if !strings.Contains(out, "REPRO-11") {
				t.Errorf("the owned issue's body is missing:\n%s", out)
			}
			if strings.Contains(out, "REPRO-12") {
				t.Errorf("the dep's owned issue rendered its body:\n%s", out)
			}
		})
	}
}

// A routed read that failed renders the reason, so an unreachable tracker does
// not read as nothing routed.
func TestRoutedReadFailureReachesTheBrief(t *testing.T) {
	for _, args := range [][]string{{"review", "B1"}, {"run", "B1", "--dry-run"}} {
		t.Run(args[0], func(t *testing.T) {
			dir := routedBriefProject(t, ghProject)
			routedTracker(t, stubPATH(t), true)
			t.Chdir(dir)

			var err error
			out := capture(t, func() { err = execute(t, args...) })
			if err != nil {
				t.Fatalf("%s: %v\n%s", args[0], err, out)
			}
			for _, want := range []string{"none listed is not none owned", "Owner: B1", "HTTP 502 from the tracker"} {
				if !strings.Contains(out, want) {
					t.Errorf("prompt does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// A file register has no route labels outside itself, and a project declaring
// none asked for no reads: neither may reach gh for routed issues.
func TestRoutedReadStaysOffTheTrackerWithoutTheGitHubBackend(t *testing.T) {
	for _, conf := range []string{fileProject, noProject} {
		t.Run(conf, func(t *testing.T) {
			dir := routedBriefProject(t, conf)
			log := filepath.Join(t.TempDir(), "gh.log")
			stubIssueWriteGH(t, ghPATH(t), log, 0)
			t.Chdir(dir)

			var err error
			out := capture(t, func() { err = execute(t, "run", "B1", "--dry-run") })
			if err != nil {
				t.Fatalf("run --dry-run: %v\n%s", err, out)
			}
			if calls := ghCalls(t, log); len(calls) != 0 {
				t.Errorf("gh was called: %v", calls)
			}
			if strings.Contains(out, "not register rows") {
				t.Errorf("an owned block rendered with no tracker to read:\n%s", out)
			}
		})
	}
}
