package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
)

// Who implemented and who reviewed has to be answerable without opening the
// ledger file, or the record exists and nobody consults it.
func TestShowNamesBothRunners(t *testing.T) {
	cfg := &config.Config{}
	out := capture(t, func() {
		Show{ID: "1", Title: "First", Implementer: "codex", Reviewer: "claude"}.print(cfg)
	})
	for _, want := range []string{"implemented by  codex", "reviewed by     claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not carry %q:\n%s", want, out)
		}
	}
}

// An item that predates the record reports unknown for both stages. Rendering
// the project default here would answer a question nobody asked and hide the
// one that matters: whether anything was recorded at all.
func TestShowReportsAnUnrecordedRunnerAsUnknown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Run.DefaultRunner = "claude"
	out := capture(t, func() {
		Show{ID: "16", Title: "Old work", Done: true}.print(cfg)
	})
	if strings.Count(out, "unknown (no run recorded)") != 2 {
		t.Errorf("both stages must report unknown:\n%s", out)
	}
	if strings.Contains(out, "implemented by  claude") {
		t.Errorf("the project default was rendered as a recorded fact:\n%s", out)
	}
}

// --json follows the same rule: a key that is absent says nobody recorded a
// run, which a consumer can act on. A key defaulted to a runner name cannot be
// told apart from one drover observed.
func TestShowJSONOmitsUnrecordedRunners(t *testing.T) {
	data, err := json.Marshal(Show{ID: "16"})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"implementer", "review_runner"} {
		if _, ok := obj[key]; ok {
			t.Errorf("%s is present for an item with no recorded run: %s", key, data)
		}
	}
	data, err = json.Marshal(Show{ID: "1", Implementer: "codex", Reviewer: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"implementer":"codex"`) ||
		!strings.Contains(string(data), `"review_runner":"claude"`) {
		t.Errorf("recorded runners are missing from --json: %s", data)
	}
}

// An item with one requirement is what intake scaffolds, so the line every
// show prints has to agree in number.
func TestShowRequirementCountAgrees(t *testing.T) {
	for _, c := range []struct {
		reqs int
		want string
	}{
		{1, "  body       work/B01-one.md  (1 numbered requirement)"},
		{3, "  body       work/B01-one.md  (3 numbered requirements)"},
		{0, "  body       work/B01-one.md  (0 numbered requirements)"},
	} {
		out := capture(t, func() {
			Show{ID: "B1", Path: "work/B01-one.md", Reqs: c.reqs}.print(&config.Config{})
		})
		if !strings.Contains(out, c.want+"\n") {
			t.Errorf("show with %d requirements does not carry %q:\n%s", c.reqs, c.want, out)
		}
	}
}

// The table is built from the ledger, so an id with nothing behind it could
// arrive as a dependency marked UNAUDITED, an audit level for a body nobody wrote.
// Driven through a real tree, because a hand-built ShowDep proves the renderer
// branches on a field in a world where something fills it.
func TestShowSeparatesDepsThatDidNotResolve(t *testing.T) {
	stubPATH(t)
	dir := shelfProject(t, map[string]string{
		"B01-parent.md": body("Parent", "B2 B3 B4 nonsense"),
		"B02-merged.md": body("Merged", ""),
		"B03-broken.md": "no frontmatter here at all\n",
	}, map[string]string{})
	rows := `{"id":"B2","merged":true,"implementer":"claude","date":"2026-01-01"}
{"id":"B2","level":"claimed","code_by":"agent","date":"2026-01-02"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	out := capture(t, func() {
		if err := execute(t, "show", "B1"); err != nil {
			t.Fatal(err)
		}
	})
	deps := out[strings.Index(out, "dependencies"):]
	deps = deps[:strings.Index(deps, "audit history")]

	for _, want := range []string{
		"⚠ B2    done, audited claimed (code by agent, audited by its author)",
		"B3    will not parse",
		"B4    has no body",
		"nonsense does not resolve to an item id",
	} {
		if !strings.Contains(deps, want) {
			t.Errorf("the dep table does not carry %q:\n%s", want, deps)
		}
	}
	// Three of the four are not foundations, so neither an audit level nor the weak
	// flag applies to them.
	if n := strings.Count(deps, "UNAUDITED"); n != 0 {
		t.Errorf("%d unresolved deps carry an audit level:\n%s", n, deps)
	}
	if n := strings.Count(deps, "⚠"); n != 1 {
		t.Errorf("weak-foundation flags = %d, want 1 (the merged dep):\n%s", n, deps)
	}
}

// --json gains the classification and keeps every field a consumer already
// reads, done included.
func TestShowJSONCarriesDepState(t *testing.T) {
	stubPATH(t)
	dir := shelfProject(t, map[string]string{
		"B01-parent.md": body("Parent", "B3 B4 nonsense"),
		"B03-broken.md": "no frontmatter here at all\n",
	}, map[string]string{})
	t.Chdir(dir)

	t.Cleanup(func() { showJSON = false })
	out := capture(t, func() {
		if err := execute(t, "show", "B1", "--json"); err != nil {
			t.Fatal(err)
		}
	})
	var sh Show
	if err := json.Unmarshal([]byte(out), &sh); err != nil {
		t.Fatal(err)
	}
	want := map[string]idState{"B3": idUnparsed, "B4": idAbsent, "nonsense": idUnresolved}
	if len(sh.Deps) != len(want) {
		t.Fatalf("deps = %+v", sh.Deps)
	}
	for _, d := range sh.Deps {
		if d.State != want[d.ID] {
			t.Errorf("%s state = %q, want %q", d.ID, d.State, want[d.ID])
		}
		if d.Done || d.Level != "" {
			t.Errorf("%s carries a completion or a level: %+v", d.ID, d)
		}
	}
}

// One fixture through both surfaces of one command. The table gates its flag on
// the dep having a readable body, and a --json that computed weak from the
// ledger alone would return {"state":"unresolved","weak":true} for a dep named
// nonsense off no row at all. AGENTS.md sends every consumer to --json, so that
// is the surface that has to be right.
func TestShowTableAndJSONAgreePerDep(t *testing.T) {
	stubPATH(t)
	dir := shelfProject(t, map[string]string{
		"B01-parent.md": body("Parent", "B2 B3 B5 nonsense"),
		"B02-merged.md": body("Merged", ""),
		"B03-broken.md": "no frontmatter here at all\n",
	}, map[string]string{
		"B05-retired.md": body("Retired", ""),
	})
	// B5 carries the rows that would make a ledger-only weak flag assert on a
	// body drover will not read: merged, and audited by its own author.
	rows := `{"id":"B2","merged":true,"implementer":"claude","date":"2026-01-01"}
{"id":"B2","level":"claimed","code_by":"agent","date":"2026-01-02"}
{"id":"B5","merged":true,"implementer":"claude","date":"2026-01-03"}
{"id":"B5","level":"claimed","code_by":"agent","date":"2026-01-04"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	table := capture(t, func() {
		if err := execute(t, "show", "B1"); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { showJSON = false })
	out := capture(t, func() {
		if err := execute(t, "show", "B1", "--json"); err != nil {
			t.Fatal(err)
		}
	})
	var sh Show
	if err := json.Unmarshal([]byte(out), &sh); err != nil {
		t.Fatal(err)
	}

	wantState := map[string]idState{
		"B2": idReadable, "B3": idUnparsed, "B5": idShelved, "nonsense": idUnresolved,
	}
	if len(sh.Deps) != len(wantState) {
		t.Fatalf("deps = %+v", sh.Deps)
	}
	for _, d := range sh.Deps {
		if d.State != wantState[d.ID] {
			t.Errorf("%s state = %q, want %q", d.ID, d.State, wantState[d.ID])
		}
		line := depLine(t, table, d.ID)
		// The flag is scoped to a readable body. weak is the same
		// link in the other surface, so one is true exactly when the other
		// renders.
		if flagged := strings.Contains(line, "⚠"); flagged != d.Weak {
			t.Errorf("%s: table flag = %v, --json weak = %v\n  %s", d.ID, flagged, d.Weak, line)
		}
		// And the state itself: the table says why it read nothing, --json
		// names the same state in check's vocabulary.
		if phrase := d.State.phrase(); phrase != "" && !strings.Contains(line, phrase) {
			t.Errorf("%s: table line %q does not carry the %q phrase", d.ID, line, d.State)
		}
	}
	// The one dep with a readable body, merged and audited by its author, is the
	// only weak foundation here. Three ids drover could not read are not weak
	// foundations, they are not foundations.
	weak := 0
	for _, d := range sh.Deps {
		if d.Weak {
			weak++
		}
	}
	if weak != 1 {
		t.Errorf("weak deps = %d, want 1 (B2):\n%+v", weak, sh.Deps)
	}
}

// depLine is the dep's row in the printed table, flag and all.
func depLine(t *testing.T, table, id string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "⚠ ")); len(f) > 0 && f[0] == id {
			return line
		}
	}
	t.Fatalf("no table row for dep %s:\n%s", id, table)
	return ""
}

// The parenthetical only appears above one, so an item that went right the
// first time reads exactly as it did before any of this.
func TestShowCountsRoundsOnlyWhenThereWereSeveral(t *testing.T) {
	out := capture(t, func() {
		Show{ID: "1", Title: "First", Implementer: "claude", Reviewer: "codex", Runs: 3, Reviews: 2}.print(&config.Config{})
	})
	if !strings.Contains(out, "implemented by  claude  (3 runs)") {
		t.Errorf("show does not count the runs:\n%s", out)
	}
	if !strings.Contains(out, "(2 launches drover resolved a runner for)") {
		t.Errorf("show does not count the review launches:\n%s", out)
	}
	one := capture(t, func() {
		Show{ID: "1", Title: "First", Implementer: "claude", Runs: 1}.print(&config.Config{})
	})
	if strings.Contains(one, "(1 run") {
		t.Errorf("show counts a round nobody repeated:\n%s", one)
	}
}

// A branch git could not answer for names the question rather than reporting a
// round that changed nothing.
func TestShowRecentCommitsDegradeToANamedReason(t *testing.T) {
	out := capture(t, func() {
		Show{ID: "1", Title: "First", RecentUnknown: "no branch work in this tree"}.print(&config.Config{})
	})
	if !strings.Contains(out, "recent commits  unknown (no branch work in this tree)") {
		t.Errorf("show does not name why the sizes are absent:\n%s", out)
	}
	if strings.Contains(out, "0 lines") {
		t.Errorf("show reports a zero for a read it never made:\n%s", out)
	}
}

// resolve's help promises this listing. Without it a settled disposition is
// visible only inside a brief an agent reads.
func TestShowListsDispositionsAndWhatIsUndisposed(t *testing.T) {
	out := capture(t, func() {
		Show{ID: "1", Title: "First", Dispositions: []ledger.Resolution{
			{Issue: 47, Disposition: "fixed", CodeBy: "agent", Independent: true, Date: "2026-09-01", Settled: true},
			{Issue: 100, Disposition: "partial", CodeBy: "agent", Date: "2026-09-02"},
			{Issue: 103, Applied: "closed", Disposition: "fixed", CodeBy: "agent", Independent: true, Date: "2026-09-03"},
			{Issue: 110},
		}}.print(&config.Config{})
	})
	for _, want := range []string{
		"#47    fixed", "settled",
		"#100   partial", "author",
		"#103", "closed on the tracker",
		"#110", "undisposed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not report %q:\n%s", want, out)
		}
	}
	// Applied binds first: it reached a tracker other people read, and that has
	// a different remedy from agreement inside the ledger.
	if strings.Contains(out, "closed on the tracker  settled") {
		t.Error("show marks an applied row settled as well")
	}
}

// A project whose items declare no closes key sees nothing new.
func TestShowIsSilentWhenNothingWasLinked(t *testing.T) {
	out := capture(t, func() { Show{ID: "1", Title: "First"}.print(&config.Config{}) })
	if strings.Contains(out, "dispositions") {
		t.Errorf("show prints an empty disposition block:\n%s", out)
	}
}

// detail is the one way to read the prompt without opening the file, and it
// has to print the body as it sits there: below the fence, nothing rewritten.
func TestShowDetailPrintsTheBody(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": accepted})
	t.Chdir(dir)

	plain := capture(t, func() {
		if err := showCmd.RunE(showCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(plain, "Act as a maintainer.") {
		t.Errorf("show without detail printed the body:\n%s", plain)
	}

	out := capture(t, func() {
		if err := showCmd.RunE(showCmd, []string{"B1", "detail"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.HasPrefix(out, plain) {
		t.Errorf("detail changed the summary above the body:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n  body\n\nAct as a maintainer.\n\nRequirements:\n\n1.\n2.\n") {
		t.Errorf("detail did not end on the body as written:\n%q", out)
	}
	if strings.Contains(out, "title: Empty feed") {
		t.Errorf("detail printed the frontmatter:\n%s", out)
	}
}

func TestShowDetailJSONCarriesBody(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": accepted})
	t.Chdir(dir)
	showJSON = true
	t.Cleanup(func() { showJSON = false })

	for _, c := range []struct {
		args []string
		want bool
	}{{[]string{"B1"}, false}, {[]string{"B1", "detail"}, true}} {
		out := capture(t, func() {
			if err := showCmd.RunE(showCmd, c.args); err != nil {
				t.Fatal(err)
			}
		})
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatal(err)
		}
		body, ok := obj["body"].(string)
		if ok != c.want {
			t.Errorf("%v: body present = %v, want %v", c.args, ok, c.want)
		}
		if c.want && !strings.Contains(body, "Requirements:") {
			t.Errorf("%v: body is %q", c.args, body)
		}
	}
}

// A misspelled detail must not fall back to the summary, which would read as
// an item with nothing in its body.
func TestShowRefusesAnyWordButDetail(t *testing.T) {
	if err := showCmd.Args(showCmd, []string{"B1", "detail"}); err != nil {
		t.Errorf("detail refused: %v", err)
	}
	err := showCmd.Args(showCmd, []string{"B1", "deatil"})
	if err == nil || !strings.Contains(err.Error(), "detail") {
		t.Errorf("a misspelled detail: %v", err)
	}
	if err := showCmd.Args(showCmd, []string{"B1", "detail", "x"}); err == nil {
		t.Error("three arguments accepted")
	}
	if err := showCmd.Args(showCmd, nil); err == nil {
		t.Error("no id accepted")
	}
}
