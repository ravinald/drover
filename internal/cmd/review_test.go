package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

// A reviewer needs one bit about each dependency: can I rest on this. Four
// distinct ledger states collapse to "no", and treating any of them as a solid
// foundation is how an unverified claim becomes load-bearing.
func TestWeakFoundation(t *testing.T) {
	cases := []struct {
		name string
		st   ledger.State
		weak bool
	}{
		{"nobody looked", ledger.State{}, true},
		{"claimed", ledger.State{Level: ledger.LevelClaimed, CodeBy: ledger.CodeAgent, Independent: true}, true},
		{"audited by its author", ledger.State{Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent}, true},
		{"authorship unrecorded", ledger.State{Level: ledger.LevelVerified}, true},
		{"reviewed independently", ledger.State{Level: ledger.LevelReviewed, CodeBy: ledger.CodeAgent, Independent: true}, false},
		{"traced independently", ledger.State{Level: ledger.LevelVerified, CodeBy: ledger.CodeAgent, Independent: true}, false},
	}
	for _, c := range cases {
		if got := weakFoundation(c.st); got != c.weak {
			t.Errorf("%s: weakFoundation = %v, want %v", c.name, got, c.weak)
		}
	}
}

// `status <id>` is show; a second word is still a usage error, and the error
// points at show's detail rather than leaving the caller to guess.
func TestStatusArgsTakeOneID(t *testing.T) {
	if err := statusArgs(nil, nil); err != nil {
		t.Errorf("no args must still be valid: %v", err)
	}
	if err := statusArgs(nil, []string{"B13"}); err != nil {
		t.Errorf("one id must be valid: %v", err)
	}
	err := statusArgs(nil, []string{"B13", "detail"})
	if err == nil || !strings.Contains(err.Error(), "drover show B13 detail") {
		t.Errorf("a second argument should be refused naming show's detail, got: %v", err)
	}
}

// The whole point of a separate reviewing runner: an item pinning who
// implements it must not thereby pin who reviews it, because that is the
// self-certification `audit` refuses to record as verified. With no flag and no
// project default, review prints rather than launching the item's own agent.
func TestReviewRunnerNeverInheritsTheItemAgent(t *testing.T) {
	t.Cleanup(func() { reviewRunner, runRunner = "", "" })
	it := backlog.Item{Meta: map[string]string{"agent": "codex"}}
	s := &session{cfg: &config.Config{Run: config.Run{RunnerKey: "agent", DefaultRunner: "claude"}}}

	if got := s.runnerName(it); got != "codex" {
		t.Fatalf("run resolves the item's agent to %q, want codex — the fixture proves nothing otherwise", got)
	}
	if got := s.reviewRunnerName(); got != "" {
		t.Errorf("review resolved runner %q from the item; an empty name is what makes it print", got)
	}

	s.cfg.Run.ReviewRunner = "gemini"
	if got := s.reviewRunnerName(); got != "gemini" {
		t.Errorf("[run] review_runner = %q, want gemini", got)
	}
	reviewRunner = "codex"
	if got := s.reviewRunnerName(); got != "codex" {
		t.Errorf("--runner = %q, want it to override the project default", got)
	}
}

// The two stages have to be separable in the record, or the question of which
// runner implemented and which reviewed is answered by a single value that the
// second launch would overwrite.
func TestReviewRecordsItsRunnerApartFromTheImplementer(t *testing.T) {
	t.Cleanup(func() { runRunner, reviewRunner, reviewHeadless = "", "", false })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s := fixture(t, `
[runners.impl]
interactive = ["stub-agent"]
headless    = ["stub-agent"]

[runners.rev]
interactive = ["stub-agent"]
headless    = ["stub-agent"]
`)
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	stubRunner(t, stubPATH(t), "stub-agent", "")
	t.Chdir(dir)

	if err := execute(t, "run", "1", "--runner", "impl"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := execute(t, "review", "1", "--runner", "rev", "--headless"); err != nil {
		t.Fatalf("review: %v", err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	st := l.State("1")
	if st.Implementer != "impl" || st.Reviewer != "rev" {
		t.Fatalf("implementer %q, reviewer %q; want impl, rev", st.Implementer, st.Reviewer)
	}
	// review reads merged work, so recording the launch must not have moved the
	// tree off the branch run put it on.
	if got := headBranch(t, dir); got != "first" {
		t.Errorf("review moved the tree to %q", got)
	}
}

// The flag beats the record, the record beats the project default, and the
// default is only reached when nobody recorded a run. Falling through to a
// literal "claude" would make the default a guess presented as a fact.
func TestAttributionPrefersTheRecordOverTheDefault(t *testing.T) {
	s := fixture(t, "")
	s.cfg.Run.DefaultRunner = "gemini"

	if got := s.attribution("", "1"); got != "gemini" {
		t.Errorf("with no run recorded, attribution = %q, want the project default", got)
	}
	s.ledger.Rows = append(s.ledger.Rows, ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "codex"})
	if got := s.attribution("", "1"); got != "codex" {
		t.Errorf("attribution = %q, want the recorded runner", got)
	}
	if got := s.attribution("hand-typed", "1"); got != "hand-typed" {
		t.Errorf("attribution = %q, want the flag to win", got)
	}
}

// The brief's whole job on a long item is to say how long it has been going.
// Three counts, each labeled by what wrote it: a project that pipes `drover
// review` writes no review launch, so one derived round number would read zero
// there forever.
func TestTheBriefSaysWhichPassThisIs(t *testing.T) {
	d := prompts.Data{
		Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test",
		// Three different numbers on purpose. Equal counts hid a sentence that
		// attached the review count to the audit count with "N of them", which
		// read as nonsense the first time a real item had more reviews than
		// audits.
		Runs: 8, Reviews: 6, Audits: 5,
		Recent: prompts.Recent{Commits: []prompts.Commit{{Lines: 158}, {Lines: 121}, {Lines: 158}}},
	}
	out := renderBrief(t, "review", prompts.ReviewTemplate(), d)
	for _, want := range []string{
		"8 runs of 1", "5 recorded audits", "6 review launches",
		"changed 158, 121 and 158 lines", "marginal finding",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt does not say %q", want)
		}
	}
	// No count may borrow another's subject.
	if strings.Contains(out, "of them behind") {
		t.Errorf("the review count still hangs off the audit count:\n%s", out)
	}
}

// Gating on Repeated is what keeps every single-pass brief in every project
// free of the repeated-pass section.
func TestTheBriefIsSilentOnAFirstPass(t *testing.T) {
	d := prompts.Data{Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test"}
	one := prompts.Data{Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test", Runs: 1, Audits: 1}
	for _, d := range []prompts.Data{d, one} {
		if out := renderBrief(t, "review", prompts.ReviewTemplate(), d); strings.Contains(out, "Where this pass sits") {
			t.Error("prompt counts a pass nobody has repeated")
		}
	}
}

// The project's principles forbid a degraded read reporting a zero. A branch git could not
// answer for has to name why, or a reader takes "0 lines" for a quiet round.
func TestTheBriefNamesWhatGitCouldNotSay(t *testing.T) {
	d := prompts.Data{
		Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test",
		Runs: 3, Audits: 3,
		Recent: prompts.Recent{Unavailable: "[git] enabled is false, so drover did not ask git"},
	}
	out := renderBrief(t, "review", prompts.ReviewTemplate(), d)
	if !strings.Contains(out, "went unread: [git] enabled is false") {
		t.Error("prompt does not name why the sizes are absent")
	}
	if strings.Contains(out, "0 lines") {
		t.Error("prompt reports a zero for a read drover never made")
	}
}

// recentCommits answers about the item's branch, and every way it can fail to
// answer names itself rather than returning an empty slice.
func TestRecentCommitsNameWhyThereAreNone(t *testing.T) {
	s := openIn(t, shelfProject(t, map[string]string{}, nil))
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")

	if _, why := s.recentCommits("no-such-branch"); !strings.Contains(why, "no branch no-such-branch") {
		t.Errorf("a missing branch said %q", why)
	}
	if _, why := s.recentCommits(""); why == "" {
		t.Error("an item with no branch name reported no reason")
	}
	if _, why := s.recentCommits("main"); !strings.Contains(why, "no commits past") {
		t.Errorf("a branch level with the trunk said %q", why)
	}

	git(t, dir, "switch", "-q", "-c", "work")
	write(t, filepath.Join(dir, "f.txt"), "a\nb\nc\n")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "three lines")
	got, why := s.recentCommits("work")
	if why != "" {
		t.Fatalf("a branch with a commit said %q", why)
	}
	if len(got) != 1 || got[0].Lines != 3 {
		t.Fatalf("read %+v, want one commit of 3 lines", got)
	}
}

// The pair is the whole change: a settled answer arrives as fact, and one that
// is not still asks for a judgement. Twenty-four rows for two answers is what
// the second branch alone produced.
func TestTheBriefHandsOverASettledDispositionAsFact(t *testing.T) {
	base := func(r prompts.Resolution) prompts.Data {
		return prompts.Data{Project: "example", ID: "1", Title: "First", Body: "1. Do it.",
			VerifyCommand: "make test", Closes: []prompts.Resolution{r}}
	}
	settled := renderBrief(t, "review", prompts.ReviewTemplate(),
		base(prompts.Resolution{Number: 47, Disposition: "fixed", By: "agent", Settled: true}))
	if !strings.Contains(settled, "settled fixed") {
		t.Error("prompt does not mark a settled disposition")
	}
	if strings.Contains(settled, "Re-judge it yourself") {
		t.Error("prompt asks for a re-judgement of an answer two passes agreed on")
	}

	one := renderBrief(t, "review", prompts.ReviewTemplate(),
		base(prompts.Resolution{Number: 47, Disposition: "fixed", By: "agent"}))
	if !strings.Contains(one, "Re-judge it yourself") {
		t.Error("prompt stopped asking for a re-judgement of a single row")
	}
	// Narrower than "settled": the static instruction below the list names the
	// word too, and asserting on that would pass for the wrong reason.
	if strings.Contains(one, "settled fixed") {
		t.Error("prompt marks one row settled")
	}

	// Applied binds first: it reached a tracker other people read.
	applied := renderBrief(t, "review", prompts.ReviewTemplate(),
		base(prompts.Resolution{Number: 47, Disposition: "fixed", By: "agent", Settled: true, Applied: "closed"}))
	if !strings.Contains(applied, "do not judge it again") || strings.Contains(applied, "settled fixed") {
		t.Error("prompt does not put applied ahead of settled")
	}
}

// agyDeniedResult is the terminal event of a `drover review` that could not
// run a command, copied whole from internal/runlog/testdata/agy-denied.jsonl:
// one turn, an empty response, status SUCCESS, and the reason under a field
// nothing read. Duplicated rather than read across packages because this
// asserts what an operator sees and runlog's copy asserts what the parser
// reads; a stub runner takes a string either way.
const agyDeniedResult = `{"event":"result","result":{"conversation_id":"a918ec5b-8e4d-4d0f-a715-d5244b91e769",` +
	`"status":"SUCCESS","response":"","duration_seconds":11.936734,"num_turns":1,` +
	`"usage":{"input_tokens":32377,"output_tokens":1114,"thinking_tokens":853,` +
	`"cache_read_tokens":40763,"total_tokens":33491},` +
	`"denied_actions":[{"action":"command","display_name":"RunCommand"}]}}`

// A review that can reach neither `drover audit` nor `drover request-changes`
// records a stage row and nothing else, and the operator's next question is
// whether the findings were good news. Without this, answering it takes the
// ledger by hand and then the log by hand.
func TestAReviewDeniedItsCommandsSaysSoAtReviewTime(t *testing.T) {
	t.Cleanup(func() { runRunner, reviewRunner, reviewHeadless = "", "", false })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s := fixture(t, `
[runners.impl]
interactive = ["stub-agent"]
headless    = ["stub-agent"]

[runners.rev]
interactive = ["stub-review"]
headless    = ["stub-review"]
`)
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	path := stubPATH(t)
	stubRunner(t, path, "stub-agent", "")
	stubRunner(t, path, "stub-review", agyDeniedResult)
	t.Chdir(dir)

	if err := execute(t, "run", "1", "--runner", "impl"); err != nil {
		t.Fatalf("run: %v", err)
	}
	var err error
	out := captureErr(t, func() {
		err = execute(t, "review", "1", "--runner", "rev", "--headless")
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	// The token first, because it is the word drive parks on and a wrapper
	// keys on; then the cause, which is the half that was unreadable.
	for _, want := range []string{
		"Pass:       " + passSilent,
		"recorded nothing: no audit and no request for changes",
		"the runner denied RunCommand",
		"drover audit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the review-time report does not carry %q:\n%s", want, out)
		}
	}

	// Reported, never recorded. `drover open-pr` reads a pass row as the
	// authority that a branch may merge, and a review nobody supervised must
	// not acquire the standing of a drive's own review pass.
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := l.Pass("1"); ok {
		t.Error("review wrote a pass row; propose would read it as a merge authority")
	}
}

// The other half: a terminal event naming no denial reads as if the field were
// not read at all. Most runs are this one, so a cause
// clause that fired here would be noise on every pass drover reports.
func TestAReviewWithNoDenialReportsNoCause(t *testing.T) {
	t.Cleanup(func() { runRunner, reviewRunner, reviewHeadless = "", "", false })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s := fixture(t, `
[runners.rev]
interactive = ["stub-review"]
headless    = ["stub-review"]
`)
	dir := s.cfg.Root
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	stubRunner(t, stubPATH(t), "stub-review",
		`{"event":"result","result":{"status":"SUCCESS","response":"reviewed it","num_turns":9}}`)
	t.Chdir(dir)

	var err error
	out := captureErr(t, func() {
		err = execute(t, "review", "1", "--runner", "rev", "--headless")
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	// It recorded no audit and no request for changes of its own, so it is still silent.
	// What it must not carry is a cause, because the runner refused it nothing.
	if !strings.Contains(out, "Pass:       "+passSilent) {
		t.Fatalf("a review that recorded nothing reads silent:\n%s", out)
	}
	if strings.Contains(out, "denied") {
		t.Errorf("no denial in the terminal event, so no cause clause:\n%s", out)
	}
}

// An interactive review writes no log, so there is no terminal event to read
// and every pass would report as one that died. Nothing after the runner
// returns is read or printed: no token, no outcome, no next step, and a runner
// that failed hands its error back uninterpreted.
func TestAnInteractiveReviewReportsNoPass(t *testing.T) {
	const audit = `{"id":"1","level":"verified","code_by":"agent","independent":true,"date":"2026-09-22"}`
	for _, c := range []struct {
		name string
		exit int
		rows []string
	}{
		{"it recorded nothing", 0, nil},
		{"it recorded an audit", 0, []string{audit}},
		{"it exited non-zero", 1, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := reviewProject(t, c.exit, "", c.rows...)
			var err error
			out := captureErr(t, func() { err = execute(t, "review", "1", "--runner", "rev") })
			if (err != nil) != (c.exit != 0) {
				t.Fatalf("review returned %v for a runner exiting %d", err, c.exit)
			}
			for _, line := range []string{"Pass:", "Outcome:", "Next steps:"} {
				if strings.Contains(out, line) {
					t.Errorf("an interactive review printed %q after the runner returned:\n%s", line, out)
				}
			}
			l, err := ledger.Load(s.cfg.Abs(s.cfg.Paths.Ledger))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, ok := l.Pass("1"); ok {
				t.Error("an interactive review wrote a pass row")
			}
		})
	}
}

// A reviewer who finds every finding closed and is never told about drover
// close-changes records an audit and leaves the block, and the item parks as
// standing. The exits are drover's text, so a project template
// that says nothing about them still ends on them, in both renderers.
func TestTheBriefNamesBothExitsOverAnOpenBlock(t *testing.T) {
	s := fixture(t, "[prompts]\nreview = \"review.tmpl\"\n")
	dir := s.cfg.Root
	write(t, filepath.Join(dir, "review.tmpl"), "review {{ .ID }}\n")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	t.Chdir(dir)

	wants := []string{"drover close-changes 1", "drover request-changes 1 -f <file>", prompts.ChangesHeading, `"` + passStanding + `"`}
	brief := func() (manual, driven string) {
		t.Helper()
		manual = capture(t, func() {
			if err := execute(t, "review", "1"); err != nil {
				t.Fatalf("review: %v", err)
			}
		})
		s := openIn(t, dir)
		it, err := s.item("1")
		if err != nil {
			t.Fatal(err)
		}
		if driven, err = s.renderReview(it); err != nil {
			t.Fatal(err)
		}
		return manual, driven
	}

	manual, driven := brief()
	for _, out := range []string{manual, driven} {
		if strings.Contains(out, "drover close-changes") {
			t.Errorf("a body with no block names the exits:\n%s", out)
		}
	}

	item := filepath.Join(dir, "work", "01-first.md")
	write(t, item, "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n\n"+prompts.ChangesHeading+"\n\nR1 unmet: still open.\n")
	manual, driven = brief()
	for name, out := range map[string]string{"drover review": manual, "drive": driven} {
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s's prompt does not name %q:\n%s", name, want, out)
			}
		}
	}

	write(t, item, "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n\n"+prompts.ChangesHeading+"\n\n"+prompts.ClearedMark+" Closed.\n")
	if manual, _ = brief(); strings.Contains(manual, "drover close-changes") {
		t.Errorf("a cleared block still names the exits:\n%s", manual)
	}
}
