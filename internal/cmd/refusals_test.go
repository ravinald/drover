package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// docs/automation.md carries two hand-kept lists of every command that exits 3:
// the "Who exits this way" cell of the exit-code table, and the per-command
// refusal table under it. A list checked by inspection drifts the moment a
// command gains or loses a refusal.
//
// The judge is Code(err) on a real invocation, not a scan for `Refusal{`: the
// nested-launch path exits 3 without constructing one (exit.go, ErrNested), and
// a textual scan would miss it and every later path built the same way.
//
// Maintenance, when one of these fails: fix the doc the failure names, or fix
// the case table below. Never the assertion. A command that gained a refusal
// needs a case here and a row in every table listed in refusalTables; one that
// lost its last refusal loses all of them, which is the direction a spot check
// never catches.
const automationDoc = "../../docs/automation.md"

// docs/agent-guide.md carries a third copy, read by an unattended agent that has
// no operator to ask when a command exits 3 for a reason its table omits. A
// third copy drifts as easily as the other two, so it is compared against the
// same set rather than left to inspection.
const agentGuideDoc = "../../docs/agent-guide.md"

// Each per-command refusal table, the heading it follows, and the column holding
// its exit code. agent-guide.md's table also carries the near misses that exit
// 1, so its rows are filtered on that column; automation.md's table is refusals
// only and has none.
var refusalTables = []struct {
	doc, heading string
	exitCol      int // -1 when every row in the table refuses
}{
	{automationDoc, "Every refusal that exits", -1},
	{agentGuideDoc, "## Refusals you will hit", 1},
}

// refusalCase is one invocation that must exit 3, driven through the real root
// so the code comes from the path a caller's shell reads.
type refusalCase struct {
	// why is the refusal in the doc's own words, so a failure names the row.
	why string
	run func(t *testing.T) error
}

// Commands with no path to exit 3. Declared rather than inferred: a command
// added to the tree belongs in exactly one of this list and the case table, and
// the tree walk below refuses to guess which.
var neverRefuses = []string{
	"attach", "check", "drives", "report", "follow-ups", "schema", "show", "unaudited", "version",
}

// One case per refusing command. Not one per documented refusal: what the doc
// pins is the command, and one executed refusal proves the command can exit 3.
var refusalCases = map[string][]refusalCase{
	"config": {{
		why: "--stamp on a build carrying no release version",
		run: func(t *testing.T) error {
			asRelease(t, devVersion)
			staleProject(t)
			return refuse(t, "config", "--stamp")
		},
	}},
	"run": {{
		why: "the body still carries the stop marker",
		run: func(t *testing.T) error {
			draftProject(t, "")
			return refuse(t, "run", "B1")
		},
	}},
	"draft": {{
		why: "the body is past intake",
		run: func(t *testing.T) error {
			draftProject(t, "")
			return refuse(t, "draft", "1")
		},
	}},
	"review": {{
		why: "a launched session cannot launch another",
		run: func(t *testing.T) error {
			t.Cleanup(func() { reviewRunner = "" })
			draftProject(t, "\n[runners.rev]\ninteractive = [\"stub-agent\"]\nheadless    = [\"stub-agent\"]\n")
			t.Setenv(runner.EnvActor, runner.ActorAgent)
			return refuse(t, "review", "1", "--runner", "rev")
		},
	}},
	"open-pr": {{
		why: "the last review did not pass",
		run: func(t *testing.T) error {
			t.Cleanup(func() { openPRForce = false })
			s := driveProject(t)
			write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"),
				"---\ntitle: First\ndeps:\n---\n\n1. Do it.\n")
			if err := ledger.Append(filepath.Join(s.cfg.Root, "ledger.jsonl"),
				ledger.Row{ID: "1", Pass: "changes_requested", Date: "2026-09-20"}); err != nil {
				t.Fatal(err)
			}
			return refuse(t, "open-pr", "1")
		},
	}},
	"link": {{
		why: "the project declares no closes key",
		run: func(t *testing.T) error {
			t.Chdir(noClosesProject(t))
			return refuse(t, "link", "B1", "47")
		},
	}},
	"resolve": {{
		why: "the item's closes key does not name that issue",
		run: func(t *testing.T) error {
			t.Cleanup(func() { resolveCodeBy = "" })
			t.Chdir(closesProject(t))
			return refuse(t, "resolve", "B1", "999", "fixed", "--code-by", "agent")
		},
	}},
	"audit": {{
		why: "whoever wrote the code cannot certify every requirement",
		run: func(t *testing.T) error {
			t.Cleanup(func() { auditCodeBy, auditReviewer = "", "" })
			draftProject(t, "")
			return refuse(t, "audit", "1", "verified", "--code-by", "sam", "--reviewer", "sam")
		},
	}},
	"split": {{
		why: "a split with no proposal parks an item and says nothing about the decision",
		run: func(t *testing.T) error {
			t.Cleanup(func() { splitFile, splitInto = "", "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "nothing.md")
			write(t, path, "   \n")
			splitInto = "bug"
			return refuse(t, "split", "1", "a second contract", "-f", path)
		},
	}, {
		why: "there is no kind to write the new item as",
		run: func(t *testing.T) error {
			t.Cleanup(func() { splitFile, splitInto = "", "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "proposal.md")
			write(t, path, "Extract the storage contract.\n")
			return refuse(t, "split", "1", "a second contract", "-f", path)
		},
	}, {
		why: "the kind names no namespace this project declares",
		run: func(t *testing.T) error {
			t.Cleanup(func() { splitFile, splitInto = "", "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "proposal.md")
			write(t, path, "Extract the storage contract.\n")
			splitInto = "epic"
			return refuse(t, "split", "1", "a second contract", "-f", path)
		},
	}},
	"invariant": {{
		why: "a guarantee nobody wrote down is one the next pass rediscovers",
		run: func(t *testing.T) error {
			t.Cleanup(func() { invariantFile = "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "blank.md")
			write(t, path, "   \n")
			return refuse(t, "invariant", "1", "-f", path)
		},
	}, {
		why: "there is nothing to record without -f",
		run: func(t *testing.T) error {
			t.Cleanup(func() { invariantFile = "" })
			draftProject(t, "")
			return refuse(t, "invariant", "1")
		},
	}},
	"close-changes": {{
		why: "the body carries no block to clear",
		run: func(t *testing.T) error {
			t.Cleanup(func() { clearFile = "" })
			draftProject(t, "")
			return refuse(t, "close-changes", "1")
		},
	}, {
		why: "the latest review sent work back itself",
		run: func(t *testing.T) error {
			t.Cleanup(func() { requestChangesFile, clearFile = "", "" })
			dir, evidence := capProject(t, 0)
			if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
				ledger.Row{ID: "1", Stage: ledger.StageReview, Runner: "rev", Date: "2026-09-21"}); err != nil {
				t.Fatal(err)
			}
			requestChangesFile = ""
			if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
				t.Fatal(err)
			}
			return refuse(t, "close-changes", "1")
		},
	}},
	"request-changes": {{
		why: "a request for changes with no evidence tells the next run nothing",
		run: func(t *testing.T) error {
			t.Cleanup(func() { requestChangesFile = "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "empty.md")
			write(t, path, "   \n")
			return refuse(t, "request-changes", "1", "-f", path)
		},
	}, {
		why: "there is nothing to write without -f",
		run: func(t *testing.T) error {
			t.Cleanup(func() { requestChangesFile = "" })
			draftProject(t, "")
			return refuse(t, "request-changes", "1")
		},
	}, {
		why: "the item is at [review] max_review_rounds",
		run: func(t *testing.T) error {
			t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
			_, evidence := capProject(t, 1)
			requestChangesFile = ""
			if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
				return err
			}
			requestChangesFile = ""
			return refuse(t, "request-changes", "1", "-f", evidence)
		},
	}, {
		why: "--over-limit is the operator's, inside a session drover launched",
		run: func(t *testing.T) error {
			t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
			_, evidence := capProject(t, 1)
			requestChangesFile = ""
			if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
				return err
			}
			t.Setenv(runner.EnvActor, "agent")
			requestChangesFile, requestChangesOverLimit = "", false
			return refuse(t, "request-changes", "1", "-f", evidence, "--over-limit")
		},
	}},
	"watch": {{
		why: "the drive exited without emitting one of the --until events",
		run: func(t *testing.T) error {
			enqueueFixture(t)
			ownState(t)
			if _, _, err := driveEventsOf(t, "1"); err != nil {
				t.Fatalf("drive: %v", err)
			}
			return refuse(t, "watch", "--until", "needs_operator")
		},
	}},
	"drive": {{
		why: "--for and --until both bound the drive",
		run: func(t *testing.T) error {
			t.Cleanup(func() { driveFor, driveUntil = 0, "" })
			draftProject(t, "")
			driveFor, driveUntil = time.Hour, "06:00"
			return refuse(t, "drive", "--for", "1h", "--until", "06:00")
		},
	}, {
		why: "the project declares no reviewing runner, so a drive would merge unreviewed work",
		run: func(t *testing.T) error {
			t.Cleanup(func() { driveFor, driveUntil = 0, "" })
			draftProject(t, "")
			return refuse(t, "drive")
		},
	}, {
		why: "drive merges what it drives, and the project declares no [drive] merge_gate",
		run: func(t *testing.T) error {
			t.Cleanup(func() { driveFor, driveUntil, driveNoMerge = 0, "", false })
			driveProjectWith(t, "")
			return refuse(t, "drive")
		},
	}},
	"agents": {{
		why: "the end marker sits above the begin marker",
		run: func(t *testing.T) error {
			t.Cleanup(func() { agentsWrite = "" })
			dir := draftProject(t, "")
			path := filepath.Join(dir, "AGENTS.md")
			write(t, path, agentsEnd+"\n\n"+agentsBegin+"\n")
			return refuse(t, "agents", "--write", path)
		},
	}},
	"init": {{
		why: "a .drover.toml is already there",
		run: func(t *testing.T) error {
			draftProject(t, "")
			return refuse(t, "init")
		},
	}, {
		why: "a per-user config is already there",
		run: func(t *testing.T) error {
			// init --user is the one form that needs no project, so it gets a bare
			// directory rather than a fixture. It still may not run from this
			// repository.
			t.Chdir(t.TempDir())
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			if err := os.MkdirAll(filepath.Join(home, "drover"), 0o750); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(home, "drover", "config.toml"), "")
			return refuse(t, "init", "--user")
		},
	}},
	"new": {{
		why: "the kind matches no namespace",
		run: func(t *testing.T) error {
			newProject(t)
			return refuse(t, "new", "epic", "a title the project has no kind for")
		},
	}},
	"intake": {{
		why: "the register backend is not github",
		run: func(t *testing.T) error {
			newProject(t)
			return refuse(t, "intake", "--list")
		},
	}},
	"projects": {{
		why: "--forget names a root the registry never named",
		run: func(t *testing.T) error {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			resetProjectsFlags(t)
			return refuse(t, "projects", "--forget", "/nowhere/at/all")
		},
	}},
	"follow-up": {{
		why: "the register backend is not file",
		run: func(t *testing.T) error {
			newProject(t)
			return refuse(t, "follow-up", "open", "a title")
		},
	}},
	"supervise": {{
		why: "a supervising session cannot start another",
		run: func(t *testing.T) error {
			s := fixture(t, "")
			t.Chdir(s.cfg.Root)
			t.Setenv(runner.EnvActor, runner.ActorSupervisor)
			return refuse(t, "supervise", "1")
		},
	}},
	"shelve": {{
		why: "no shelf is configured",
		run: func(t *testing.T) error {
			draftProject(t, "")
			return refuse(t, "shelve", "1")
		},
	}},
	"unshelve": {{
		why: "no shelf is configured",
		run: func(t *testing.T) error {
			draftProject(t, "")
			return refuse(t, "unshelve", "1")
		},
	}},
	"skills": {{
		why: "the project declares no [paths] skills",
		run: func(t *testing.T) error {
			t.Cleanup(func() { skillsFrom = "" })
			newProject(t)
			return refuse(t, "skills", "--from", t.TempDir())
		},
	}},
	"status": {{
		why: "a severity the vocabulary does not know",
		run: func(t *testing.T) error {
			t.Cleanup(func() { statusSev = nil })
			newProject(t)
			return refuse(t, "status", "--sev", "Z")
		},
	}},
}

// sealedCases derives one case per command actor.go seals. Those refuse on the
// classification table alone, with nothing in their own file to find, so they
// are generated from the table rather than listed a second time here.
func sealedCases() map[string][]refusalCase {
	out := map[string][]refusalCase{}
	for name, k := range classified {
		if k.who != operator {
			continue
		}
		out[name] = []refusalCase{{
			why: "drover launched this session",
			run: func(t *testing.T) error {
				// The fixture is what keeps a regressed gate an assertion. Without
				// it these run against this repository: intake reaches the tracker,
				// finish fast-forwards the tree the test is running in.
				newProject(t)
				t.Setenv(runner.EnvActor, runner.ActorAgent)
				return refuse(t, name, "1")
			},
		}}
	}
	return out
}

func allRefusalCases() map[string][]refusalCase {
	out := map[string][]refusalCase{}
	for name, cs := range refusalCases {
		out[name] = append(out[name], cs...)
	}
	for name, cs := range sealedCases() {
		out[name] = append(out[name], cs...)
	}
	return out
}

// refuse runs one command with both streams swallowed: several of these print a
// resolved argv or a progress line before they refuse.
func refuse(t *testing.T, args ...string) error {
	t.Helper()
	var err error
	capture(t, func() { captureErr(t, func() { err = execute(t, args...) }) })
	return err
}

// The set that can exit 3, which is what the doc's two lists are compared to.
func canRefuse() map[string]bool {
	set := map[string]bool{}
	for name := range allRefusalCases() {
		set[name] = true
	}
	return set
}

func TestEveryRefusalCaseExitsThree(t *testing.T) {
	cases := allRefusalCases()
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		for _, c := range cases[name] {
			t.Run(name+"/"+c.why, func(t *testing.T) {
				err := c.run(t)
				if err == nil {
					t.Fatalf("%s: exit 0; the doc says this refuses", name)
				}
				if got := Code(err); got != ExitRefused {
					t.Errorf("%s: exit %d, want %d: %v", name, got, ExitRefused, err)
				}
			})
		}
	}
}

// A command added to the tree with neither a refusal case nor a place on the
// open list is the drift both doc tables suffered: nobody knows which side it
// belongs on, and the comparison below would silently agree with whatever the
// doc happened to say.
func TestEveryCommandIsCoveredOrDeclaredOpen(t *testing.T) {
	install()
	refuses := canRefuse()
	open := map[string]bool{}
	for _, n := range neverRefuses {
		open[n] = true
	}
	tree := map[string]bool{}
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		name := c.Name()
		tree[name] = true
		switch {
		case refuses[name] && open[name]:
			t.Errorf("%s: has a refusal case and is on neverRefuses; it cannot be both", name)
		case !refuses[name] && !open[name]:
			t.Errorf("%s: no refusal case and not on neverRefuses; add a case or declare it open", name)
		}
	}
	for _, n := range neverRefuses {
		if !tree[n] {
			t.Errorf("%s: on neverRefuses but not a command any more", n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(refuses)) {
		if !tree[n] {
			t.Errorf("%s: has a refusal case but is not a command any more", n)
		}
	}
}

func TestExitCodeTableMatchesTheCommandsThatRefuse(t *testing.T) {
	compare(t, automationDoc, "the exit-code table's `3` row", documentedExitThree(t), canRefuse())
}

func TestRefusalTableMatchesTheCommandsThatRefuse(t *testing.T) {
	for _, tab := range refusalTables {
		t.Run(tab.doc, func(t *testing.T) {
			compare(t, tab.doc, "the per-command refusal table",
				documentedRefusalRows(t, tab.doc, tab.heading, tab.exitCol), canRefuse())
		})
	}
}

// compare names which side each command falls on. "Documented and cannot" and
// "can and undocumented" are two different edits, and a bare inequality names
// neither.
func compare(t *testing.T, doc, where string, documented, actual map[string]bool) {
	t.Helper()
	for _, n := range slices.Sorted(maps.Keys(documented)) {
		if !actual[n] {
			t.Errorf("%s lists %s, which cannot exit 3: drop it from %s", where, n, doc)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(actual)) {
		if !documented[n] {
			t.Errorf("%s exits 3 and %s does not list it: add it to %s", n, where, doc)
		}
	}
}

// backticked spans, from which the first word is the command: a cell reading
// "`follow-up open` / `follow-up close`" names one command twice.
var backticked = regexp.MustCompile("`([^`]+)`")

func namesIn(cell string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
		if f := strings.Fields(m[1]); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

func docLines(t *testing.T, doc string) []string {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("%s: %v", doc, err)
	}
	return strings.Split(string(b), "\n")
}

// cells splits a table row on its pipes, dropping the empty edges.
func cells(line string) []string {
	return strings.Split(strings.Trim(line, "| "), "|")
}

// The "Who exits this way" cell of the `3` row.
func documentedExitThree(t *testing.T) map[string]bool {
	t.Helper()
	for _, line := range docLines(t, automationDoc) {
		if !strings.HasPrefix(line, "| `3` |") {
			continue
		}
		c := cells(line)
		set := map[string]bool{}
		for _, n := range namesIn(c[len(c)-1]) {
			set[n] = true
		}
		return set
	}
	t.Fatalf("%s: no `3` row in the exit-code table", automationDoc)
	return nil
}

// The first column of the table under heading, skipping rows whose exit column
// says something other than 3.
func documentedRefusalRows(t *testing.T, doc, heading string, exitCol int) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	in := false
	for _, line := range docLines(t, doc) {
		switch {
		case strings.HasPrefix(line, heading):
			in = true
		case in && strings.HasPrefix(line, "|"):
			c := cells(line)
			if exitCol >= 0 && (len(c) <= exitCol || strings.TrimSpace(c[exitCol]) != "3") {
				continue
			}
			for _, n := range namesIn(c[0]) {
				set[n] = true
			}
		case in && strings.HasPrefix(line, "#"):
			in = false
		}
	}
	if len(set) == 0 {
		t.Fatalf("%s: no per-command refusal table under %q", doc, heading)
	}
	return set
}

// neverRefuses is a declaration, and nothing above checks it against the code.
// A command already on that list which gains its first refusal is invisible to
// all four tests: no case exists so canRefuse does not grow, the tree walk is
// satisfied because the name sits on the list, and both doc comparisons agree
// with a doc that never mentions it. Measured by injecting a Refusal into
// unauditedCmd.RunE: drover unaudited --json exits 3 and every test above passes.
//
// This polices the declared list. It does not judge whether a command exits 3:
// Code(err) does that, above. The nested-launch path exits 3 without
// constructing a Refusal, and no scan can see it.
func TestNeverRefusesCommandsConstructNoRefusal(t *testing.T) {
	files := cmdSources(t)
	refuses := refusingFuncs(files)
	for _, name := range neverRefuses {
		lit, file := commandLiteral(t, files, name)
		if buildsRefusal(files[file]) {
			t.Errorf("%s: %s constructs Refusal{}, but %s is on neverRefuses."+
				" Give it a refusal case and a row in %s, or stop refusing", name, file, name, automationDoc)
		}
		for _, callee := range funcsNamedIn(lit, importsOf(files[file])) {
			if refuses[callee] {
				t.Errorf("%s is on neverRefuses but reaches %s(), which constructs a Refusal{}."+
					" Give it a refusal case and a row in %s, or stop refusing", name, callee, automationDoc)
			}
		}
	}
}

// cmdSources parses this package's own source, keyed by file name. Tests are
// excluded: a Refusal built in a fixture proves nothing about the command.
func cmdSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "cmd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = f
	}
	if len(out) == 0 {
		t.Fatalf("%s: no sources parsed; this test is vacuous", dir)
	}
	return out
}

// refusingFuncs is every package-level function that constructs a Refusal, plus
// everything that transitively calls one. A helper is how a refusal arrives at a
// command whose own file is clean.
func refusingFuncs(files map[string]*ast.File) map[string]bool {
	refuses := map[string]bool{}
	calls := map[string][]string{}
	for _, f := range files {
		imports := importsOf(f)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if buildsRefusal(fn.Body) {
				refuses[fn.Name.Name] = true
			}
			calls[fn.Name.Name] = append(calls[fn.Name.Name], calleesIn(fn.Body, imports)...)
		}
	}
	for changed := true; changed; {
		changed = false
		for name, cs := range calls {
			if refuses[name] {
				continue
			}
			for _, c := range cs {
				if refuses[c] {
					refuses[name], changed = true, true
					break
				}
			}
		}
	}
	return refuses
}

func buildsRefusal(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Refusal" {
			found = true
		}
		return true
	})
	return found
}

// funcsNamedIn names every function a command literal could reach, whether it
// calls it or hands it over by name. `RunE: someFunc` is a reference and not a
// call, so calleesIn alone leaves a command whose whole body lives in another
// file unexamined, which is the shape that reopens the hole this test closes.
//
// A key names a struct field and an identifier qualified by an import names
// something outside this package, so neither can be a local function.
func funcsNamedIn(n ast.Node, imports map[string]bool) []string {
	var out []string
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			ast.Inspect(v.Value, visit)
			return false
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && imports[id.Name] {
				return false
			}
			out = append(out, v.Sel.Name)
			ast.Inspect(v.X, visit)
			return false
		case *ast.Ident:
			out = append(out, v.Name)
		}
		return true
	}
	ast.Inspect(n, visit)
	return out
}

// calleesIn names every function called under n. A call through an imported
// package is skipped: Refusal is declared here and nothing outside can build one.
func calleesIn(n ast.Node, imports map[string]bool) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out = append(out, fn.Name)
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); !ok || !imports[id.Name] {
				out = append(out, fn.Sel.Name)
			}
		}
		return true
	})
	return out
}

func importsOf(f *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Name != nil {
			out[imp.Name.Name] = true
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out[path[strings.LastIndex(path, "/")+1:]] = true
	}
	return out
}

// commandLiteral finds the cobra.Command whose Use names the command, and the
// file holding it. Found by Use rather than by file name, because follow-ups lives in
// register.go and a convention nothing enforces is the drift this test is about.
func commandLiteral(t *testing.T, files map[string]*ast.File, name string) (ast.Node, string) {
	t.Helper()
	for fname, f := range files {
		var found ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Command" {
				return true
			}
			if use(lit) == name {
				found = lit
			}
			return true
		})
		if found != nil {
			return found, fname
		}
	}
	t.Fatalf("%s: on neverRefuses but no cobra.Command declares it", name)
	return nil, ""
}

// use is the first word of a command literal's Use field: "show <id>" is show.
func use(lit *ast.CompositeLit) string {
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Use" {
			continue
		}
		v, ok := kv.Value.(*ast.BasicLit)
		if !ok {
			continue
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			continue
		}
		if f := strings.Fields(s); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}
