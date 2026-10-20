package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

const permissiveTable = "\n[supervise]\nmerge_drive_pr = \"supervisor\"\n"

// supervisedFinish is a merged B1 whose ledger carries what drive writes for
// a pull request it opened, plus whichever pass rows the case wants.
func supervisedFinish(t *testing.T, conf string, passes ...string) string {
	t.Helper()
	dir, branch := finishProject(t, noProject+conf)
	path := filepath.Join(dir, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "B1", Branch: branch, PR: 7, Drive: "D1", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	for _, p := range passes {
		if err := ledger.Append(path, ledger.Row{ID: "B1", Pass: p, Drive: "D1", Date: ledger.Today()}); err != nil {
			t.Fatal(err)
		}
	}
	stubPATH(t)
	t.Chdir(dir)
	t.Setenv(runner.EnvActor, runner.ActorSupervisor)
	return dir
}

func TestASupervisorFinishesADrivePullRequestThatPassedReview(t *testing.T) {
	dir := supervisedFinish(t, permissiveTable, passPassed)
	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish under a supervisor at merge_drive_pr = supervisor: %v\n%s", err, out)
	}
	l, lerr := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !l.State("B1").Merged {
		t.Errorf("finish exited 0 and wrote no completion row:\n%s", out)
	}
}

func TestASupervisorFinishRefusesADeniedPathAtOperator(t *testing.T) {
	dir := supervisedFinish(t, "\n[supervise]\nmerge_drive_pr = \"supervisor\"\nmerge_protected_paths = \"operator\"\n", passPassed)
	path := filepath.Join(dir, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "B1", Park: "protected_paths", Drive: "D1", Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	err := refuse(t, "finish", "B1")
	if Code(err) != ExitRefused {
		t.Fatalf("finish exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"merge_protected_paths", `"operator"`, "operator merge and finish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	l, _ := ledger.Load(path)
	if l.State("B1").Merged {
		t.Error("a refusal wrote the completion row")
	}
}

func TestASupervisorFinishesADeniedPathWhenAllowedOrSuperseded(t *testing.T) {
	for _, c := range []struct {
		name, conf string
		rows       []ledger.Row
	}{
		{"merge_protected_paths = supervisor",
			"\n[supervise]\nmerge_drive_pr = \"supervisor\"\nmerge_protected_paths = \"supervisor\"\n",
			[]ledger.Row{{ID: "B1", Park: "protected_paths", Drive: "D1"}}},
		{"park older than the latest pull request row", permissiveTable,
			[]ledger.Row{{ID: "B1", Park: "protected_paths", Drive: "D1"}, {ID: "B1", PR: 8, Drive: "D2"}, {ID: "B1", Pass: passPassed, Drive: "D2"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := supervisedFinish(t, c.conf, passPassed)
			path := filepath.Join(dir, "ledger.jsonl")
			l, err := ledger.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			branch := ""
			for _, r := range l.Rows {
				if r.PR == 7 {
					branch = r.Branch
				}
			}
			for _, r := range c.rows {
				if r.PR > 0 {
					r.Branch = branch
				}
				r.Date = ledger.Today()
				if err := ledger.Append(path, r); err != nil {
					t.Fatal(err)
				}
			}
			out := capture(t, func() { err = execute(t, "finish", "B1") })
			if err != nil {
				t.Fatalf("finish: %v\n%s", err, out)
			}
		})
	}
}

func TestASupervisorFinishRefusesWithoutAPassedReview(t *testing.T) {
	for _, c := range []struct {
		name   string
		passes []string
	}{
		{name: "no pass row"},
		{name: "a later pass sent back", passes: []string{passPassed, "changes_requested"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := supervisedFinish(t, permissiveTable, c.passes...)
			err := refuse(t, "finish", "B1")
			if Code(err) != ExitRefused {
				t.Fatalf("finish exited %d (%v), want %d", Code(err), err, ExitRefused)
			}
			for _, want := range []string{"merge_drive_pr", `"supervisor"`, "latest review pass is passed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %s: %v", want, err)
				}
			}
			l, _ := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
			if l.State("B1").Merged {
				t.Error("a refusal wrote the completion row")
			}
		})
	}
}

func TestTheDefaultPolicyRefusesASupervisorFinish(t *testing.T) {
	supervisedFinish(t, "", passPassed)
	err := refuse(t, "finish", "B1")
	if Code(err) != ExitRefused {
		t.Fatalf("finish at the default policy exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"merge_drive_pr", `"operator"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

func TestFinishForceRefusesASupervisorAtAnyPolicy(t *testing.T) {
	t.Cleanup(func() { finishForce = false })
	supervisedFinish(t, permissiveTable, passPassed)
	err := refuse(t, "finish", "B1", "--force")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("finish --force under a supervisor exited %d (%v), want %d naming --force", Code(err), err, ExitRefused)
	}
}

func TestAgentsRendersTheMergeStepFromThePolicy(t *testing.T) {
	const conditions = "the ledger names the pull request on a row drive wrote"
	body, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "8. **Stop at the merge point.**") || strings.Contains(body, conditions) {
		t.Errorf("the default renders the conditions, or not the stop:\n%s", body)
	}

	body, err = fixture(t, permissiveTable).renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{conditions, "came from a session drover launched", "every check on the head is green",
		"Everything else stops at the merge point", "a pull request drive did not open", "`merge_drive_pr` | supervisor |"} {
		if !strings.Contains(body, want) {
			t.Errorf("merge_drive_pr = supervisor does not render %q", want)
		}
	}
	if strings.Contains(body, "read the diff of each denied file") {
		t.Error("the denied-paths rule rendered while merge_protected_paths is operator")
	}
	body, err = fixture(t, permissiveTable+"merge_protected_paths = \"supervisor\"\n").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "read the diff of each denied file") {
		t.Error("merge_protected_paths = supervisor does not render the denied-paths rule")
	}
}

func TestMergeDrivePRAtAutoFailsLoad(t *testing.T) {
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, config.FileName)
	write(t, path, read(t, path)+"\n[supervise]\nmerge_drive_pr = \"auto\"\n")
	_, err := config.Load(s.cfg.Root)
	if err == nil || !strings.Contains(err.Error(), "ceiling") || !strings.Contains(err.Error(), `"supervisor"`) {
		t.Fatalf("merge_drive_pr = auto loaded or did not name the ceiling: %v", err)
	}
}

func TestAPerUserMergePolicyReachesAProjectThatSetsNone(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "drover"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(xdg, "drover", config.GlobalFileName), "[supervise]\nmerge_drive_pr = \"supervisor\"\n")

	if p, _ := fixture(t, "").cfg.Policy("merge_drive_pr"); p != polSupervisor {
		t.Errorf("a project setting none reads merge_drive_pr = %q, want the per-user supervisor", p)
	}
	if p, _ := fixture(t, "\n[supervise]\nmerge_drive_pr = \"operator\"\n").cfg.Policy("merge_drive_pr"); p != polOperator {
		t.Errorf("a project restricting it reads %q, want operator", p)
	}
}

// agentsFlags resets the agents flags, which cobra leaves set between Execute
// calls in one process.
func agentsFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		agentsWrite, agentsPermissive, agentsRestrict, agentsYes = "", false, false, false
	}
	reset()
	t.Cleanup(reset)
}

// agentsRun runs agents with args from a clean flag state, returning stdout,
// stderr and the error.
func agentsRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	agentsWrite, agentsPermissive, agentsRestrict, agentsYes = "", false, false, false
	var err error
	var stderr string
	stdout := capture(t, func() { stderr = captureErr(t, func() { err = execute(t, append([]string{"agents"}, args...)...) }) })
	return stdout, stderr, err
}

func TestPermissiveMergeAppendsTheTableAndRegenerationKeepsIt(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	host := filepath.Join(s.cfg.Root, "AGENTS.md")

	if _, stderr, err := agentsRun(t, "--write", "AGENTS.md", "--permissive-merge"); err != nil {
		t.Fatalf("agents --permissive-merge: %v\n%s", err, stderr)
	}
	cfg, err := config.Load(s.cfg.Root)
	if err != nil {
		t.Fatalf("the appended table does not load: %v", err)
	}
	for _, k := range mergeFlagKeys {
		if p, declared := cfg.Policy(k); p != polSupervisor || !declared {
			t.Errorf("%s = %q (declared %v) after --permissive-merge, want supervisor", k, p, declared)
		}
	}
	first := read(t, host)
	if !strings.Contains(first, "the ledger names the pull request on a row drive wrote") {
		t.Errorf("the block does not render the permissive text:\n%s", first)
	}

	if _, stderr, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatalf("agents --write with no flag: %v\n%s", err, stderr)
	}
	if again := read(t, host); again != first {
		t.Errorf("regenerating with no flag changed the block:\n%s", again)
	}
}

func TestMergeFlagOverAnExistingTablePrintsTheLines(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "\n[supervise]\nenqueue = \"supervisor\"\n")
	t.Chdir(s.cfg.Root)
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	write(t, host, "# Mine\n")
	cfgPath := filepath.Join(s.cfg.Root, config.FileName)
	before := read(t, cfgPath)

	stdout, _, err := agentsRun(t, "--write", "AGENTS.md", "--permissive-merge")
	if Code(err) != ExitRefused {
		t.Fatalf("--permissive-merge over a [supervise] table exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{`merge_drive_pr   = "supervisor"`, `merge_protected_paths = "supervisor"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the lines to paste do not include %s:\n%s", want, stdout)
		}
	}
	if got := read(t, host); got != "# Mine\n" {
		t.Errorf("AGENTS.md was written:\n%s", got)
	}
	if read(t, cfgPath) != before {
		t.Error(".drover.toml was rewritten")
	}
}

func TestTheWrittenBlockCarriesThePolicyLine(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	if _, stderr, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatalf("agents --write: %v\n%s", err, stderr)
	}
	var line string
	for l := range strings.SplitSeq(read(t, filepath.Join(s.cfg.Root, "AGENTS.md")), "\n") {
		if strings.HasPrefix(l, policyLinePrefix) {
			line = l
		}
	}
	if line == "" {
		t.Fatal("the written block carries no policy line")
	}
	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, policyLinePrefix), " -->"))
	if len(fields) != len(catalog) || !slices.IsSorted(fields) {
		t.Errorf("the policy line names %d actions, sorted %v; want all %d, sorted: %s",
			len(fields), slices.IsSorted(fields), len(catalog), line)
	}
	if !slices.Contains(fields, "merge_drive_pr=operator") {
		t.Errorf("the policy line does not record merge_drive_pr at its default: %s", line)
	}
}

func TestAWideningWriteNeedsYes(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	cfgPath := filepath.Join(s.cfg.Root, config.FileName)
	if _, _, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	block, conf := read(t, host), read(t, cfgPath)

	_, stderr, err := agentsRun(t, "--write", "AGENTS.md", "--permissive-merge")
	if Code(err) != ExitRefused {
		t.Fatalf("a widening with no --yes exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"merge_drive_pr widens: operator > supervisor", "merge_protected_paths widens: operator > supervisor"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal did not print %q:\n%s", want, stderr)
		}
	}
	if read(t, host) != block || read(t, cfgPath) != conf {
		t.Fatal("a refused widening wrote AGENTS.md or .drover.toml")
	}

	if _, stderr, err := agentsRun(t, "--write", "AGENTS.md", "--permissive-merge", "--yes"); err != nil {
		t.Fatalf("the same with --yes: %v\n%s", err, stderr)
	}
	if !strings.Contains(read(t, host), "merge_drive_pr=supervisor") || !strings.Contains(read(t, cfgPath), "[supervise]") {
		t.Error("--yes did not write the block and the config")
	}
}

func TestANarrowingWritesAndSaysSo(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, permissiveTable)
	t.Chdir(s.cfg.Root)
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	if _, _, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(s.cfg.Root, config.FileName)
	write(t, cfgPath, strings.Replace(read(t, cfgPath), `merge_drive_pr = "supervisor"`, `merge_drive_pr = "operator"`, 1))

	_, stderr, err := agentsRun(t, "--write", "AGENTS.md")
	if err != nil {
		t.Fatalf("a narrowing with no --yes: %v", err)
	}
	if !strings.Contains(stderr, "merge_drive_pr narrows: supervisor > operator") {
		t.Errorf("the narrowing was not printed:\n%s", stderr)
	}
	if !strings.Contains(read(t, host), "merge_drive_pr=operator") {
		t.Error("the narrowing was not written")
	}
}

func TestAWordingChangeWithTheSamePolicyWritesSilently(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	if _, _, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	// The block an older template rendered: same policy line, other words.
	write(t, host, strings.Replace(read(t, host), "Stop at the merge point.", "Halt at the merge point.", 1))

	_, stderr, err := agentsRun(t, "--write", "AGENTS.md")
	if err != nil {
		t.Fatalf("a wording change: %v", err)
	}
	if strings.Contains(stderr, "changes policy") {
		t.Errorf("a wording change printed a policy change:\n%s", stderr)
	}
	if !strings.Contains(read(t, host), "Stop at the merge point.") {
		t.Error("the block was not regenerated")
	}
}

func TestCheckReportsAWidenedPolicyAsStalePolicy(t *testing.T) {
	agentsFlags(t)
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	if _, _, err := agentsRun(t, "--write", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(s.cfg.Root, config.FileName)
	write(t, cfgPath, read(t, cfgPath)+permissiveTable)

	rep := openIn(t, s.cfg.Root).checkReport()
	got := findings(rep, "STALE POLICY")
	if len(got) != 1 {
		t.Fatalf("check reported %d STALE POLICY findings, want 1: %+v", len(got), rep.Findings)
	}
	for _, want := range []string{"merge_drive_pr widens: operator > supervisor", "drover agents --write AGENTS.md --yes"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("the finding does not name %q: %s", want, got[0].Detail)
		}
	}
	if len(findings(rep, "STALE BLOCK")) != 0 {
		t.Error("STALE BLOCK reported beside STALE POLICY for the same file")
	}

	cfg, err := config.Load(s.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	stale := staleFiles(cfg)
	if len(stale) != 1 || stale[0].Kind != "STALE POLICY" {
		t.Errorf("the stale check every command runs reported %+v, want one STALE POLICY", stale)
	}
}

func TestABlockWithNoPolicyLineReadsAsTheDefaults(t *testing.T) {
	got := recordedPolicy("<!-- generated by `drover agents` -->\n\nno line here")
	for _, a := range catalog {
		if got[a.key] != a.def {
			t.Errorf("%s reads %q from a block with no policy line, want its default %q", a.key, got[a.key], a.def)
		}
	}
	s := fixture(t, "")
	if _, stale := stalePolicy(s.cfg, "AGENTS.md", "no line"); stale {
		t.Error("a block with no line under a default config reported STALE POLICY")
	}
	if _, stale := stalePolicy(fixture(t, permissiveTable).cfg, "AGENTS.md", "no line"); !stale {
		t.Error("a block with no line under a widened config reported no STALE POLICY")
	}
}
