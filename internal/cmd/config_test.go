package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// A schema_version 1 config, in that schema's own spelling: [paths] bodies
// where today's reads backlog, and github.caveat_label, v1's spelling of
// follow_up_label, to drive the retired key path.
//
// Built as a real older config on purpose. Today's layout with lines deleted
// would be today's layout wearing an old version number, and the whole question
// is what an upgrade path actually meets: a directory key under a name nothing
// reads, a label key under a name nothing reads, and every table added since
// simply absent.
const v1Config = `schema_version = 1
project = "herdsay"

[paths]
# Everything this project needs is in this directory, so a reader can see the
# whole thing at once.
bodies      = "prompt-bodies"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[ids]
namespaces = { B = "bug", E = "enhancement", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "severity", "skills", "agent"]

[status]
column = "kind"

[verify]
default = "go build ./... && go test ./..."

[agents]
principles = "a failure names the URL, the status and the hop it happened on"

[intake]
stop_marker = "STOP: requirements not yet written"

[github]
caveat_label = "follow-up"

[register]
backend = "none"
`

// projectWith writes one config into a scratch tree and stands in it.
func projectWith(t *testing.T, conf string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

func diffOf(t *testing.T, dir string) string {
	t.Helper()
	return runConfig(t, dir, true)
}

// runConfig is `drover config` run whole in dir, through the command itself
// rather than a copy of what its RunE does.
func runConfig(t *testing.T, dir string, diff bool) string {
	t.Helper()
	t.Chdir(dir)
	prev := configDiff
	configDiff = diff
	t.Cleanup(func() { configDiff = prev })
	var err error
	out := capture(t, func() { err = configCmd.RunE(configCmd, nil) })
	if err != nil {
		t.Fatalf("drover config in %s: %v", dir, err)
	}
	return out
}

// The configs worth a diff are the old ones, and Load refuses those: an older
// schema_version is an error before anything else is said, so a report reached
// through the ordinary door would refuse exactly the files it exists for.
func TestDiffAnswersForAConfigLoadRefuses(t *testing.T) {
	dir := projectWith(t, v1Config)
	if _, err := config.Load(dir); err == nil {
		t.Fatal("Load accepted a schema_version 1 config; this fixture no longer tests the case")
	}

	got := diffOf(t, dir)
	for _, want := range []string{
		"schema_version = 2",
		"the file declares 1",
		// Every table schema_version 1 did not have. [review] matters most:
		// a project that declares no [review] table at all still gets
		// max_review_rounds.
		"[review]", "max_review_rounds = 0",
		"[prompts]", "[severity]", "[run]", "[git]",
		"trunk = \"\"",
		"runner_key = \"runner\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--diff never names %q:\n%s", want, got)
		}
	}
}

// A retired key is reported and never dropped. config.go rejects an undeclared
// frontmatter key as a parse error for a stated reason, and a config key that
// stopped mattering deserves the same care in the other direction: the project
// chose that value.
func TestDiffNamesEveryKeyNothingReads(t *testing.T) {
	got := diffOf(t, projectWith(t, v1Config))
	for _, want := range []string{"paths.bodies", "github.caveat_label"} {
		if !strings.Contains(got, "# "+want) {
			t.Errorf("--diff never names the retired key %q:\n%s", want, got)
		}
	}
	// Commented, because the block is meant to be pasted and pasting a retired
	// key back is the one thing this report must not cause.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "bodies") && !strings.HasPrefix(line, "#") {
			t.Errorf("a retired key is reported as a live line, which pastes it back: %q", line)
		}
	}
}

// Every line --diff prints is meant to be pasted into the file, so every one of
// them has to load where the report says it goes. A value rendered in a form
// TOML rejects, or a key reported under a table it does not belong to, sends an
// operator to break their own config on drover's advice.
//
// Pasted key by key rather than as one block, which is what the report says to
// do: TOML refuses a second [table] header, so appending a whole [paths] stanza
// to a file that already declares [paths] produces a file that will not parse.
func TestEveryReportedLinePastesIntoItsTable(t *testing.T) {
	dir := projectWith(t, v1Config)
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range in.Missing() {
		keys, err := config.KeysIn(paste(v1Config, k.Table, k.Name+" = "+k.Default))
		if err != nil {
			t.Errorf("%s = %s does not parse where the report puts it: %v", k.Dotted, k.Default, err)
			continue
		}
		if !contains(keys, k.Dotted) {
			t.Errorf("%s = %s pasted under [%s] declares no key by that name", k.Dotted, k.Default, k.Table)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// paste inserts one line into a table, the way an operator reading the report
// would: into the table if the file has it, as a new stanza at the end if not.
func paste(conf, table, line string) string {
	if table == "" {
		return line + "\n" + conf
	}
	header := "[" + table + "]\n"
	if i := strings.Index(conf, header); i >= 0 {
		at := i + len(header)
		return conf[:at] + line + "\n" + conf[at:]
	}
	return conf + "\n" + header + line + "\n"
}

// A project that has decided everything gets one line saying so. Reporting
// nothing is an answer, and a report that printed an empty section instead
// reads as a report that failed.
func TestDiffIsOneLineOnAConfigThatDeclaresEverything(t *testing.T) {
	// The version and the two renames come first. Pasting the missing keys into
	// the unmigrated file would declare paths.backlog twice: the rename supplies
	// it, and the report asked for it because the old spelling did not.
	full := v1Config
	full = strings.Replace(full, "schema_version = 1", "schema_version = 2", 1)
	full = strings.Replace(full, "bodies      =", "backlog     =", 1)
	full = strings.Replace(full, "caveat_label =", "follow_up_label   =", 1)

	in, err := config.Inspect(projectWith(t, full))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := undeclared(in.Root, in.Config.Verify["default"], in.Declared)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range append(generated, except(in.Missing(), generated)...) {
		full = paste(full, k.Table, k.Name+" = "+k.Default)
	}

	got := diffOf(t, projectWith(t, full))
	if !strings.HasPrefix(got, "ok — ") {
		t.Errorf("a config declaring everything still reports a difference:\n%s", got)
	}
}

// The command answers "what would today's config look like with my decisions in
// it", not "what does a fresh project get". A project reading its own settings
// back as defaults cannot tell which of them it chose.
func TestThePrintedConfigCarriesWhatTheProjectSet(t *testing.T) {
	dir := projectWith(t, v1Config)
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := renderConfig(forInit, dir, globalDefaults{}, in.Config.Verify["default"], written(in.Written()), in.Bare)

	for _, want := range []string{
		`project = "herdsay"`,
		`default = "go build ./... && go test ./..."`,
		`principles = "a failure names the URL, the status and the hop it happened on"`,
		`root_marker = "README.md"`,
		`column = "kind"`,  // a table the generated layout never writes
		`backend = "none"`, // written commented for a project that set nothing
		`ledger      = "ledger.jsonl"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the printed config drops %q:\n%s", want, body)
		}
	}
	// A value this build would have defaulted, where the project said nothing.
	if !strings.Contains(body, "schema_version = 2") {
		t.Errorf("the printed config does not carry this build's schema_version:\n%s", body)
	}
}

// Printing a config a reader then pastes is only safe if what is printed loads.
// Dropping one key would silently change how the project runs, and the reader
// would have followed drover's own output to get there.
func TestThePrintedConfigLoadsAndKeepsEverySetting(t *testing.T) {
	dir := projectWith(t, v1Config)
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := renderConfig(forInit, dir, globalDefaults{}, in.Config.Verify["default"], written(in.Written()), in.Bare)

	printed := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(printed, "prompt-bodies"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(printed)
	if err != nil {
		t.Fatalf("the printed config does not load: %v\n%s", err, body)
	}
	for _, c := range []struct{ what, got, want string }{
		{"verify.default", cfg.Verify["default"], "go build ./... && go test ./..."},
		{"status.column", cfg.Status.Column, "kind"},
		{"register.backend", cfg.Register.Backend, "none"},
		{"intake.stop_marker", cfg.Intake.StopMarker, "STOP: requirements not yet written"},
		{"agents.principles", cfg.Agents.Principles, "a failure names the URL, the status and the hop it happened on"},
	} {
		if c.got != c.want {
			t.Errorf("%s survived the print as %q, want %q", c.what, c.got, c.want)
		}
	}

	// A retired key is reported by --diff and never migrated into the print.
	// paths.bodies was v1's spelling of paths.backlog, and carrying the value
	// across would be drover guessing at a rename: the operator owns that, and
	// a guess that landed on the wrong key would move a whole backlog directory
	// without saying so.
	if strings.Contains(body, "bodies") {
		t.Errorf("the printed config carries the retired key paths.bodies:\n%s", body)
	}
	if cfg.Paths.Backlog != ".drover/backlog" {
		t.Errorf("paths.backlog printed as %q; a key the v1 file never declared takes this build's default",
			cfg.Paths.Backlog)
	}
}

// A key whose table the generated layout already opened has to land inside that
// table. TOML refuses a second [table] header, so carrying one at the foot of
// the file printed a config drover itself could not parse — and the reader
// pasting it would have been following drover's own output.
func TestASettingOutsideTheLayoutLandsInsideItsOwnTable(t *testing.T) {
	// Every one of these sits under a table the layout writes and is a key the
	// layout does not: [paths] shelf, a [github] label, a flagged gate, and a
	// [frontmatter] shape entry beyond the three.
	conf := schemaLine + `project = "wide"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
shelf       = "retired"
log_prefix  = "old-"

[ids]
namespaces = { B = "bug" }
planned_max = 12

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "closes", "severity"]

[frontmatter.shape]
issue    = "int"
closes   = "intlist"
severity = "enum:C,H,M,L"
kind     = "enum:bug,feature"

[verify]
default = "true"
docs    = "make docs"

[intake]
stop_marker = "STOP: requirements not yet written"

[github]
follow_up_label  = "follow-up"
page_limit  = 50

[register]
backend = "none"
`
	dir := projectWith(t, conf)
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := renderConfig(forInit, dir, globalDefaults{}, in.Config.Verify["default"], written(in.Written()), in.Bare)

	printed := projectWith(t, body)
	for _, d := range []string{"work", "retired"} {
		if err := os.MkdirAll(filepath.Join(printed, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(printed)
	if err != nil {
		t.Fatalf("the printed config does not load: %v\n%s", err, body)
	}
	for _, c := range []struct{ what, got, want string }{
		{"paths.shelf", cfg.Paths.Shelf, "retired"},
		{"paths.log_prefix", cfg.Paths.LogPrefix, "old-"},
		{"github.follow_up_label", cfg.GitHub.FollowUpLabel, "follow-up"},
		{"verify.docs", cfg.Verify["docs"], "make docs"},
		{"frontmatter.shape.kind", cfg.Frontmatter.Shape["kind"], "enum:bug,feature"},
	} {
		if c.got != c.want {
			t.Errorf("%s survived the print as %q, want %q", c.what, c.got, c.want)
		}
	}
	if cfg.IDs.PlannedMax != 12 {
		t.Errorf("ids.planned_max survived the print as %d, want 12", cfg.IDs.PlannedMax)
	}
	if cfg.GitHub.PageLimit != 50 {
		t.Errorf("github.page_limit survived the print as %d, want 50", cfg.GitHub.PageLimit)
	}
}

// There is no --write, and this is the assertion that keeps it that way. A
// .drover.toml carries hand-written comments recording why each value is what it
// is, and no TOML round-trip preserves them, so a regenerating writer would
// flatten the more valuable half of the file. A flag added later without
// answering that is caught here.
func TestConfigNeverWrites(t *testing.T) {
	install()
	c, _, err := root.Find([]string{"config"})
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Flags().Lookup("write"); f != nil {
		t.Error("drover config grew a --write: no TOML round-trip preserves the comments a config carries")
	}

	dir := projectWith(t, v1Config)
	before, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, diff := range []bool{false, true} {
		configDiff = diff
		capture(t, func() {
			if err := c.RunE(c, nil); err != nil {
				t.Fatalf("--diff=%v: %v", diff, err)
			}
		})
	}
	configDiff = false
	after, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("drover config rewrote .drover.toml")
	}
}

// STALE CONFIG is STALE BLOCK's argument applied to the config: one finding
// naming the file and the command that explains it, never an enumeration.
func TestStaleConfigPointsAtTheDiff(t *testing.T) {
	// A current config missing two keys the generated layout writes. Loadable,
	// so check reaches it at all, which is what separates this from the v1 case.
	dir := projectWith(t, schemaLine+`project = "thin"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind"]

[verify]
default = "true"

[register]
backend = "none"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}

	got := findings(openIn(t, dir).checkReport(), "STALE CONFIG")
	if len(got) != 1 {
		t.Fatalf("STALE CONFIG findings = %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Message, config.FileName) {
		t.Errorf("the finding does not name the file: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Detail, "drover config --diff") {
		t.Errorf("the finding does not point at the command that explains it: %q", got[0].Detail)
	}
	// It reports how many, never which. The names live in the command, where a
	// reader can paste them.
	if strings.Contains(got[0].Message, "stop_marker") || strings.Contains(got[0].Detail, "stop_marker") {
		t.Errorf("the finding enumerates keys instead of pointing at the diff: %q / %q",
			got[0].Message, got[0].Detail)
	}
}

// A project whose config declares everything the layout writes reports nothing.
// This is the assertion that keeps the finding from firing on every project that
// deliberately left a default alone: `drover init` writes the set, and a project
// generated by it passes.
func TestAGeneratedProjectIsNeverStale(t *testing.T) {
	dir := t.TempDir()
	body := renderConfig(forInit, dir, globalDefaults{}, "true", nil, nil)
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".drover/backlog", ".drover/reports"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if got := findings(openIn(t, dir).checkReport(), "STALE CONFIG"); len(got) != 0 {
		t.Errorf("a freshly generated project reports STALE CONFIG: %+v", got)
	}
}

// An open table's entries are names the project invented, so a project that
// declared one namespace has not failed to declare the two the layout suggests.
func TestStaleConfigIgnoresTheNamesInsideAnOpenTable(t *testing.T) {
	dir := t.TempDir()
	body := renderConfig(forInit, dir, globalDefaults{}, "true", nil, nil)
	// One namespace where the generated layout writes three, which is a
	// decision rather than a gap.
	body = strings.Replace(body,
		`namespaces = { B = "bug", E = "enhancement", F = "feature" }`,
		`namespaces = { B = "bug" }`, 1)
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".drover/backlog", ".drover/reports"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if got := findings(openIn(t, dir).checkReport(), "STALE CONFIG"); len(got) != 0 {
		t.Errorf("declaring one namespace reports STALE CONFIG: %+v", got)
	}
}

// globalConfig writes a per-user config for one test. XDG_CONFIG_HOME is
// already an empty directory for this package, so a test that wants a global
// config says so here.
func globalConfig(t *testing.T, body string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "drover")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.GlobalFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// printed is the whole of what `drover config` writes for the tree it stands
// in: the generated layout and the notices under it.
func printed(t *testing.T, dir string) string {
	t.Helper()
	return runConfig(t, dir, false)
}

// An inherited value is not a project decision, and printing it as one turns
// inheritance into an override. The proof is the edit that comes afterwards:
// the original picks the global's new value up and the printed copy does not,
// which is the whole difference between the two files and is invisible on the
// day they are compared.
func TestThePrintWritesNoValueTheGlobalSupplies(t *testing.T) {
	globalConfig(t, `[run]
default_runner = "claude"

[review]
max_review_rounds = 3
`)
	dir := projectWith(t, schemaLine+`project = "inherits"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := printed(t, dir)
	// Matched as a key line, live or commented, rather than a substring: the
	// supervised-action block names override_review_limit, which pins nothing.
	for _, key := range []string{"default_runner", "max_review_rounds"} {
		if regexp.MustCompile(`(?m)^#?\s*` + key + `\s*=`).MatchString(body) {
			t.Errorf("the print pins %s, which the per-user config supplies:\n%s", key, body)
		}
	}

	// Stand the print up as its own project, then move the global underneath
	// both. A copy that inherits resolves the new value; a copy that was
	// handed a pinned line resolves last week's.
	copyDir := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	globalConfig(t, `[run]
default_runner = "changed"

[review]
max_review_rounds = 9
`)
	for _, d := range []struct{ what, path string }{{"the source", dir}, {"the print", copyDir}} {
		cfg, err := config.Load(d.path)
		if err != nil {
			t.Fatalf("%s does not load: %v", d.what, err)
		}
		if cfg.Run.DefaultRunner != "changed" {
			t.Errorf("%s resolves default_runner = %q after the global changed, want \"changed\"",
				d.what, cfg.Run.DefaultRunner)
		}
		if cfg.Review.MaxReviewRounds != 9 {
			t.Errorf("%s resolves max_review_rounds = %d after the global changed, want 9",
				d.what, cfg.Review.MaxReviewRounds)
		}
	}
}

// A project that opens [frontmatter.shape] and closes nothing has made a
// decision: every value stays unchecked. Filling in the three keys drover
// would write for a project that never opened the table tightens validation
// the project declined, in a file it is about to paste back.
func TestAnEmptyOpenTableSurvivesThePrint(t *testing.T) {
	dir := projectWith(t, schemaLine+`project = "closes-nothing"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "closes", "severity"]

[frontmatter.shape]

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	source, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Frontmatter.Shape) != 0 {
		t.Fatalf("the fixture closes %d keys; it is no longer the empty case", len(source.Frontmatter.Shape))
	}

	copyDir := projectWith(t, printed(t, dir))
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(copyDir)
	if err != nil {
		t.Fatalf("the printed config does not load: %v", err)
	}
	if len(cfg.Frontmatter.Shape) != 0 {
		t.Errorf("the print closed %d keys the source left open: %v", len(cfg.Frontmatter.Shape), cfg.Frontmatter.Shape)
	}
}

// A TOML key segment may hold a dot when it is quoted, and only a map entry
// can: every other key here is a struct tag. Joining segments with a bare dot
// made verify."ci.linux" read as a key named linux under a table called
// verify.ci, so the gate matched nothing the project declared and left the
// print silently.
func TestAQuotedKeySegmentSurvivesThePrint(t *testing.T) {
	dir := projectWith(t, schemaLine+`project = "dotted"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[verify]
default     = "true"
"ci.linux"  = "make ci"
"ci.darwin" = "make ci-mac"

[runners."my.runner"]
headless = ["mytool", "exec"]
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	source, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	copyDir := projectWith(t, printed(t, dir))
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(copyDir)
	if err != nil {
		t.Fatalf("the printed config does not load: %v", err)
	}
	for name, want := range source.Verify {
		if got := cfg.Verify[name]; got != want {
			t.Errorf("verify command %q survived the print as %q, want %q", name, got, want)
		}
	}
	if len(cfg.Verify) != len(source.Verify) {
		t.Errorf("the print declares %d verify commands, the source %d: %v", len(cfg.Verify), len(source.Verify), cfg.Verify)
	}
	if got := cfg.Runners["my.runner"].Headless; len(got) != 2 || got[0] != "mytool" {
		t.Errorf("runners.\"my.runner\" survived the print as %v", got)
	}
}

// The full report is read as a complete account of the source config, so a key
// it drops has to be named in it. Naming it only under --diff would hide it
// from somebody reading the file they were about to paste.
func TestTheFullReportNamesEveryRetiredKey(t *testing.T) {
	body := printed(t, projectWith(t, v1Config))
	for _, want := range []string{"paths.bodies", "github.caveat_label"} {
		if !strings.Contains(body, "# "+want) {
			t.Errorf("the full report never names the retired key %q:\n%s", want, body)
		}
	}
	// Named, never revived: a live line would put back the key the notice is
	// warning about, and drover guessing at the rename would move a whole
	// backlog directory without saying so.
	for _, line := range strings.Split(body, "\n") {
		for _, dead := range []string{"bodies", "caveat_label"} {
			if strings.Contains(line, dead) && !strings.HasPrefix(line, "#") {
				t.Errorf("a retired key is printed as a live line: %q", line)
			}
		}
	}
}

// The guarantee behind the two cases above, rather than a third instance of
// it: every value the source declares is a live line in the print or is named
// under it. A key nothing places and nothing names is a setting that vanished
// from a file a reader pastes.
func TestNothingTheSourceDeclaresVanishesFromThePrint(t *testing.T) {
	for _, c := range []struct{ name, conf string }{
		{"v1", v1Config},
		{"dotted keys and an empty open table", schemaLine + `project = "wide"

[paths]
backlog     = ".drover/backlog"
ledger      = "ledger.jsonl"
reports     = "reports"
shelf       = "retired"
root_marker = "README.md"

[frontmatter.shape]

[verify]
default    = "true"
"ci.linux" = "make ci"

[review]
max_review_rounds = 4

[drive]
merge_gate = ["make", "check"]
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := projectWith(t, c.conf)
			in, err := config.Inspect(dir)
			if err != nil {
				t.Fatal(err)
			}
			body := printed(t, dir)
			if got := unplaced(body, in); len(got) != 0 {
				t.Errorf("the print drops %v:\n%s", got, body)
			}
		})
	}
}

// `drover check` has to answer for the configs worth asking about, and Load
// refuses exactly those: a stale schema_version is an error before anything
// else is said, so the ordinary door exits 1 with nothing a CI job can key on.
func TestCheckReportsAStaleSchemaVersion(t *testing.T) {
	install()
	c, _, err := root.Find([]string{"check"})
	if err != nil {
		t.Fatal(err)
	}
	dir := projectWith(t, v1Config)
	if _, err := config.Load(dir); err == nil {
		t.Fatal("Load accepted a schema_version 1 config; this fixture no longer tests the case")
	}

	rep, err := checkAnyConfig()
	if err != nil {
		t.Fatalf("check could not report on a config Load refuses: %v", err)
	}
	got := findings(rep, "STALE CONFIG")
	if len(got) != 1 {
		t.Fatalf("STALE CONFIG findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	if !strings.Contains(got[0].Message, config.FileName) {
		t.Errorf("the finding does not name the file: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Detail, "drover config --diff") {
		t.Errorf("the finding does not point at the command that explains it: %q", got[0].Detail)
	}
	// A zero count off a read that never happened is the worst answer there
	// is, so the report says the validation stopped rather than passing.
	if rep.Stopped == "" {
		t.Error("the report counts 0 bodies and 0 rows without saying nothing was read")
	}

	// Exit 2, which is the whole reason this reaches check at all.
	checkJSON = true
	defer func() { checkJSON = false }()
	var runErr error
	capture(t, func() { runErr = c.RunE(c, nil) })
	if _, ok := runErr.(Findings); !ok {
		t.Errorf("check on a stale config returned %T (%v), want Findings so it exits 2", runErr, runErr)
	}
}

// An open table merges across the two files rather than overriding, so a
// project that declared one namespace runs on three. The literal the merged
// Config carries cannot say which file wrote which entry, and printing it pins
// the two this project inherits. The pin is invisible on the day it is made:
// both files resolve the same three names, and they stop agreeing the next time
// the per-user config is edited.
func TestThePrintKeepsAnOpenTableSplitAcrossTwoFiles(t *testing.T) {
	globalConfig(t, `[ids]
namespaces = { B = "bug", E = "enhancement" }
`)
	dir := projectWith(t, schemaLine+`project = "split"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[ids]
namespaces = { F = "project-feature" }

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := printed(t, dir)
	for _, pinned := range []string{`B = "bug"`, `E = "enhancement"`} {
		if strings.Contains(body, pinned) {
			t.Errorf("the print pins %s, which the per-user config supplies:\n%s", pinned, body)
		}
	}
	if !strings.Contains(body, `F = "project-feature"`) {
		t.Errorf("the print drops the one namespace this project declared:\n%s", body)
	}

	copyDir := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	globalConfig(t, `[ids]
namespaces = { B = "defect", E = "change" }
`)
	want := map[string]string{"B": "defect", "E": "change", "F": "project-feature"}
	for _, d := range []struct{ what, path string }{{"the source", dir}, {"the print", copyDir}} {
		cfg, err := config.Load(d.path)
		if err != nil {
			t.Fatalf("%s does not load: %v", d.what, err)
		}
		if len(cfg.IDs.Namespaces) != len(want) {
			t.Errorf("%s resolves %v, want %v", d.what, cfg.IDs.Namespaces, want)
			continue
		}
		for prefix, kind := range want {
			if got := cfg.IDs.Namespaces[prefix]; got != kind {
				t.Errorf("%s resolves %s = %q after the global changed, want %q", d.what, prefix, got, kind)
			}
		}
	}
}

// The generator reads three things off the tree: the build file that becomes
// paths.root_marker, whether there is a work tree, and the origin remote.
// `init` writes each over a per-user value deliberately, because it is
// producing the first file this project has and a global root_marker naming
// go.mod would refuse every project that is not a Go module. A report must not:
// the project is already running on the inherited value, and a pasted line
// replaces it with whatever the tree happened to say the day it was printed.
func TestThePrintWritesNoInferredValueOverAnInheritedOne(t *testing.T) {
	for _, c := range []struct {
		name   string
		global string
		// pinned is what the inference must not write over the global.
		pinned string
		// resolves reads the value off a loaded config, so the assertion is what
		// the project runs on rather than what its text says.
		resolves func(*config.Config) string
		want     string
	}{
		{
			name:     "root_marker off the build file",
			global:   "[paths]\nroot_marker = \"README.md\"\n",
			pinned:   "root_marker",
			resolves: func(c *config.Config) string { return c.Paths.RootMarker },
			want:     "README.md",
		},
		{
			name:     "git.enabled off the work tree",
			global:   "[git]\nenabled = false\n",
			pinned:   "enabled =",
			resolves: func(c *config.Config) string { return strconv.FormatBool(c.Git.On()) },
			want:     "false",
		},
		{
			name:     "github.repo off the origin remote",
			global:   "[github]\nrepo = \"global/register\"\n",
			pinned:   "repo =",
			resolves: func(c *config.Config) string { return c.GitHub.Repo },
			want:     "global/register",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalConfig(t, c.global)
			dir := projectWith(t, schemaLine+`project = "inherits"

[paths]
backlog  = "work"
ledger   = "ledger.jsonl"
reports = "reports"

[verify]
default = "true"
`)
			// All three facts present at once, so a case never passes because
			// the input its own branch reads is absent.
			if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitInit(t, dir, "git@github.com:owner/project.git")

			body := printed(t, dir)
			if strings.Contains(body, c.pinned) {
				t.Errorf("the print writes %q over the value the per-user config supplies:\n%s", c.pinned, body)
			}

			copyDir := projectWith(t, body)
			if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(copyDir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(copyDir)
			if err != nil {
				t.Fatalf("the printed config does not load: %v\n%s", err, body)
			}
			if got := c.resolves(cfg); got != c.want {
				t.Errorf("the print resolves %q, want %q, which is the inherited value", got, c.want)
			}
		})
	}
}

// `init` keeps its inference, and this is the half of the pair that says so. A
// global root_marker naming a file this tree does not hold would refuse to load
// every project generated under it.
func TestInitStillWritesWhatItReadsOffTheTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := globalDefaults{path: "/etc/drover.toml", keys: map[string]bool{"paths.root_marker": true}}
	if body := renderConfig(forInit, dir, global, "", nil, nil); !strings.Contains(body, `root_marker = "go.mod"`) {
		t.Errorf("init deferred to a global root_marker instead of naming this tree's:\n%s", body)
	}
}

// checkAnyConfig routes on the error Load returned, never on the version
// Inspect read. Those are different questions the moment a per-user config is
// in play: one that fails for its own reason leaves the project's
// schema_version looking as old as it ever was, so routing on the version
// would report STALE CONFIG and exit 2 while the error that stopped the load
// disappeared.
func TestAStaleSchemaDoesNotSwallowAnotherLoadError(t *testing.T) {
	globalConfig(t, "[run]\ndefault_runnre = \"claude\"\n")
	projectWith(t, v1Config)

	rep, err := checkAnyConfig()
	if err == nil {
		t.Fatalf("check reported %+v instead of the error that stopped the load", rep.Findings)
	}
	if !strings.Contains(err.Error(), "default_runnre") {
		t.Errorf("the error check returned does not name the unknown key: %v", err)
	}
	if _, ok := err.(Findings); ok {
		t.Errorf("an unreadable per-user config exits 2 as findings rather than 1: %v", err)
	}
}

// The same split one level deeper, where an entry inside the open table is a
// struct. TOML replaces a map value wholesale rather than merging into it, so
// a project writing [runners.claude] owns that entry and the global's argv for
// it is gone. The print has to reproduce that, which is not the same rule as
// the one namespaces follow: the judge is the source, and the assertion is
// that the two resolve the same runner whatever the per-user file says next.
func TestThePrintKeepsAStructEntrySplitAcrossTwoFiles(t *testing.T) {
	globalConfig(t, `[runners.claude]
headless = ["claude", "-p", "--global"]

[runners.agy]
headless = ["agy", "--global"]
`)
	dir := projectWith(t, schemaLine+`project = "split-entry"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[verify]
default = "true"

[runners.claude]
stdin = true
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := printed(t, dir)
	if strings.Contains(body, "--global") {
		t.Errorf("the print pins an argv the per-user config supplies:\n%s", body)
	}
	if !strings.Contains(body, "stdin = true") {
		t.Errorf("the print drops the one field this project declared:\n%s", body)
	}

	copyDir := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	globalConfig(t, `[runners.claude]
headless = ["claude", "-p", "--changed"]

[runners.agy]
headless = ["agy", "--changed"]
`)
	source, err := config.Load(dir)
	if err != nil {
		t.Fatalf("the source does not load: %v", err)
	}
	copied, err := config.Load(copyDir)
	if err != nil {
		t.Fatalf("the printed config does not load: %v\n%s", err, body)
	}
	if !reflect.DeepEqual(source.Runners, copied.Runners) {
		t.Errorf("the print resolves %+v, the source %+v", copied.Runners, source.Runners)
	}
	// The entry the project never touched is the one the global still owns, so
	// it has to pick the edit up rather than carry what it said at print time.
	if got := copied.Runners["agy"].Headless; len(got) != 2 || got[1] != "--changed" {
		t.Errorf("the print resolves runners.agy headless = %v after the global changed", got)
	}
}

// A top-level open table declared empty is the one carried setting with
// nowhere safe to append. TOML reads a bare key as belonging to whatever
// header came last, so `runners = {}` written at the foot of the print
// reloaded as github.runners — a key nothing reads, in a file drover refuses,
// reached by following drover's own instruction to paste what it printed.
func TestAnEmptyTopLevelOpenTableSurvivesThePrint(t *testing.T) {
	dir := projectWith(t, schemaLine+`project = "no-runners"
runners = {}

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := printed(t, dir)

	keys, err := config.KeysIn(body)
	if err != nil {
		t.Fatalf("the print does not parse: %v\n%s", err, body)
	}
	if !contains(keys, "runners") {
		t.Errorf("the print drops the top-level table the project declared:\n%s", body)
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ".runners") {
			t.Errorf("the print writes %s, so a bare key finished under a table header:\n%s", k, body)
		}
	}

	copyDir := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(copyDir); err != nil {
		t.Fatalf("the printed config does not load: %v\n%s", err, body)
	}
}

// The per-user half of the empty-open-table case. A global that opens
// [frontmatter.shape] and closes nothing has decided what the table holds, and
// the entry names inside an open table are chosen rather than read — so asking
// after the three a fresh config suggests reads one decision as three gaps.
// The print closed them, and the copy rejected a severity the source accepted.
func TestAnEmptyOpenTableTheGlobalDeclaresStaysInherited(t *testing.T) {
	globalConfig(t, "[frontmatter.shape]\n")
	dir := projectWith(t, schemaLine+`project = "inherits-shape"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[frontmatter]
required = ["title", "deps"]
optional = ["severity"]

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	source, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Frontmatter.Shape) != 0 {
		t.Fatalf("the fixture closes %d keys; it is no longer the inherited-empty case", len(source.Frontmatter.Shape))
	}

	body := printed(t, dir)
	copyDir := projectWith(t, body)
	if err := os.MkdirAll(filepath.Join(copyDir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	copied, err := config.Load(copyDir)
	if err != nil {
		t.Fatalf("the printed config does not load: %v\n%s", err, body)
	}
	if len(copied.Frontmatter.Shape) != 0 {
		t.Errorf("the print closed %v, which the source left open:\n%s", copied.Frontmatter.Shape, body)
	}
}

// A project written before schema_version existed is the oldest config there
// is, and the one an upgrade report exists for. Treating version 0 as current
// would close the only route to a finding against it, so `drover check` would
// hand back Load's raw refusal and exit 1 with nothing a CI job can key on.
//
// Absence and an explicitly written 0 both read as version 0 and get different
// text: "declares schema_version 0" about a file with no such line sends the
// reader looking for one.
func TestCheckReportsAConfigWithNoSchemaVersion(t *testing.T) {
	body := `project = "old"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
root_marker = "README.md"

[verify]
default = "true"
`
	for _, c := range []struct{ name, conf, want string }{
		{"absent", body, "declares no schema_version"},
		{"explicitly zero", "schema_version = 0\n" + body, "declares schema_version 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := projectWith(t, c.conf)
			if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(dir); err == nil {
				t.Fatal("Load accepted a version-0 config; this fixture no longer tests the case")
			}

			rep, err := checkAnyConfig()
			if err != nil {
				t.Fatalf("check could not report on a config Load refuses: %v", err)
			}
			got := findings(rep, "STALE CONFIG")
			if len(got) != 1 {
				t.Fatalf("STALE CONFIG findings = %d, want 1: %+v", len(got), rep.Findings)
			}
			if !strings.Contains(got[0].Message, c.want) {
				t.Errorf("the finding says %q, want it to carry %q", got[0].Message, c.want)
			}
			if !strings.Contains(got[0].Message, config.FileName) {
				t.Errorf("the finding does not name the file: %q", got[0].Message)
			}
			if rep.Stopped == "" {
				t.Error("the report counts 0 bodies and 0 rows without saying nothing was read")
			}

			// One line about the version, not two. The stanza owns the key, so
			// leaving it in the undeclared list as well reports one gap twice
			// and has the reader paste the line into their file twice.
			diff := diffOf(t, dir)
			if n := strings.Count(diff, "schema_version"); n != 1 {
				t.Errorf("--diff names schema_version %d times, want 1:\n%s", n, diff)
			}
		})
	}
}

// Two files answer for a project, and a parse error that names neither leaves
// the operator to guess which one refused. Inspect reads the project file as a
// string so that one read serves two decodes, and toml.Decode cannot name what
// it was handed the way DecodeFile can.
func TestAMalformedConfigNamesTheFileThatWillNotParse(t *testing.T) {
	install()
	c, _, err := root.Find([]string{"config"})
	if err != nil {
		t.Fatal(err)
	}
	good := schemaLine + `project = "fine"

[verify]
default = "true"
`
	t.Run("the project file", func(t *testing.T) {
		projectWith(t, schemaLine+"project = \"broken\"\n[paths\n")
		var runErr error
		capture(t, func() { runErr = c.RunE(c, nil) })
		if runErr == nil {
			t.Fatal("drover config accepted a .drover.toml that does not parse")
		}
		if !strings.Contains(runErr.Error(), config.FileName) {
			t.Errorf("the error does not name the project file: %v", runErr)
		}
	})

	t.Run("the per-user file", func(t *testing.T) {
		globalConfig(t, "[paths\n")
		projectWith(t, good)
		var runErr error
		capture(t, func() { runErr = c.RunE(c, nil) })
		if runErr == nil {
			t.Fatal("drover config accepted a per-user config that does not parse")
		}
		if !strings.Contains(runErr.Error(), config.GlobalFileName) {
			t.Errorf("the error does not name the per-user file: %v", runErr)
		}
		if strings.Contains(runErr.Error(), config.FileName) {
			t.Errorf("the error names the project file, which parses: %v", runErr)
		}
	})
}

// effective is what Load resolves for dir: every exported setting, defaults
// and inheritance applied, the directory itself left out.
func effective(t *testing.T, dir string) string {
	t.Helper()
	c, err := config.Load(dir)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	c.Root = ""
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The print has to resolve the way its source does, and not only on the day
// it was printed: a copy that writes an inherited or defaulted value as its own
// agrees with the source until the per-user file moves, and then silently does
// not. So each case loads source and copy under one per-user file, changes that
// file, and loads both again.
func TestThePrintedCopyResolvesAsItsSourceUnderEitherGlobal(t *testing.T) {
	base := fmt.Sprintf("schema_version = %d\nproject = \"rt\"\n\n[paths]\nroot_marker = \"README.md\"\n",
		config.SchemaVersion)
	for _, c := range []struct{ name, global, project, after string }{
		{
			name:    "absent from both files",
			project: "\n[frontmatter]\noptional = [\"severity\", \"issue\", \"closes\"]\n",
			after:   "[frontmatter.shape]\nseverity = \"enum:C,H,M,L\"\n",
		},
		{
			name:   "empty and inherited",
			global: "[verify]\n",
			after:  "[verify]\ndefault = \"true\"\n",
		},
		{
			name:    "empty in the project over a partial global",
			global:  "[ids.namespaces]\nB = \"bug\"\n",
			project: "\n[ids]\nnamespaces = {}\n",
			after:   "[ids.namespaces]\nB = \"bug\"\nE = \"enhancement\"\n",
		},
		{
			name:    "partial across both files",
			global:  "[verify]\ndefault = \"true\"\n",
			project: "\n[verify]\ndocs = \"true\"\n",
			after:   "[verify]\ndefault = \"make\"\n",
		},
		{
			name:    "empty at the top level",
			global:  "[verify]\ndefault = \"true\"\n",
			project: "\n[verify]\n",
			after:   "[verify]\ndefault = \"make\"\n",
		},
		{
			name:    "partial at the top level",
			global:  "[runners.codex]\nheadless = [\"codex\", \"exec\"]\n",
			project: "\n[runners.agy]\nheadless = [\"agy\"]\n",
			after:   "[runners.codex]\nheadless = [\"codex\"]\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalConfig(t, c.global)
			src := projectWith(t, base+c.project)
			out := printed(t, src)
			cp := projectWith(t, out)
			if a, b := effective(t, src), effective(t, cp); a != b {
				t.Fatalf("the copy resolves differently from its source\nsource: %s\ncopy:   %s\nprinted:\n%s", a, b, out)
			}
			globalConfig(t, c.after)
			if a, b := effective(t, src), effective(t, cp); a != b {
				t.Errorf("after the per-user file changed, the copy resolves differently\nsource: %s\ncopy:   %s\nprinted:\n%s", a, b, out)
			}
		})
	}
}

// A project's own file is the one that has to say which format it was written
// against. A per-user schema_version lets a project with none load, and a
// check that went by the load reported that project clean; one that went by
// the layered union named a mismatch nobody could find in the project.
func TestCheckStopsOnEveryProjectVersionGap(t *testing.T) {
	body := "project = \"old\"\n\n[paths]\nroot_marker = \"README.md\"\n\n[verify]\ndefault = \"true\"\n"
	current := fmt.Sprintf("schema_version = %d\n", config.SchemaVersion)
	future := fmt.Sprintf("schema_version = %d\n", config.SchemaVersion+1)
	for _, c := range []struct {
		name, global, conf, want string
		inGlobal                 bool
	}{
		{name: "global current, project absent", global: current, conf: body, want: "declares no schema_version"},
		{name: "global stale, project absent", global: "schema_version = 1\n", conf: body,
			want: "declares schema_version 1", inGlobal: true},
		{name: "project zero", conf: "schema_version = 0\n" + body, want: "declares schema_version 0"},
		{name: "project future", conf: future + body,
			want: fmt.Sprintf("declares schema_version %d", config.SchemaVersion+1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalConfig(t, c.global)
			dir := projectWith(t, c.conf)

			prev := checkJSON
			checkJSON = true
			t.Cleanup(func() { checkJSON = prev })
			var err error
			out := capture(t, func() { err = checkCmd.RunE(checkCmd, nil) })
			if code := Code(err); code != 2 {
				t.Fatalf("exit %d (%v), want 2:\n%s", code, err, out)
			}
			var rep CheckReport
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			got := findings(rep, "STALE CONFIG")
			if len(got) != 1 || len(rep.Findings) != 1 {
				t.Fatalf("want exactly one STALE CONFIG finding, got %+v", rep.Findings)
			}
			if !strings.Contains(got[0].Message, c.want) {
				t.Errorf("the finding says %q, want %q", got[0].Message, c.want)
			}
			file := config.FileName
			if c.inGlobal {
				file = config.GlobalPath()
			}
			if got[0].Path != file {
				t.Errorf("the finding names %q, want %q", got[0].Path, file)
			}
			if rep.Stopped == "" || rep.OK {
				t.Errorf("ok = %v, stopped = %q: the report reads as a whole validation", rep.OK, rep.Stopped)
			}

			if diff := diffOf(t, dir); !strings.Contains(diff, fmt.Sprintf("schema_version = %d", config.SchemaVersion)) {
				t.Errorf("--diff does not carry the version line:\n%s", diff)
			}
		})
	}
}

// Whoever wrote a retired key chose its value, and that is the per-user file's
// author as often as the project's. Both reports name the key with the file it
// sits in and attribute the choice to nobody else.
func TestARetiredPerUserKeyIsNamedWithItsFile(t *testing.T) {
	globalConfig(t, "[github]\ncaveat_label = \"legacy\"\n")
	dir := projectWith(t, fmt.Sprintf("schema_version = %d\nproject = \"rt\"\n", config.SchemaVersion))
	for name, out := range map[string]string{"--diff": diffOf(t, dir), "the full print": printed(t, dir)} {
		var line string
		for l := range strings.SplitSeq(out, "\n") {
			if strings.Contains(l, "github.caveat_label") {
				line = l
			}
		}
		if !strings.HasPrefix(line, "# ") || !strings.Contains(line, config.GlobalFileName) {
			t.Errorf("%s names the retired key as %q, want a comment naming %s:\n%s", name, line, config.GlobalFileName, out)
		}
		if strings.Contains(out, "project chose") {
			t.Errorf("%s says the project chose a value the per-user file wrote:\n%s", name, out)
		}
	}
}

// checkKinds is the finding kinds `drover check` reports in the tree it stands
// in, with its exit code. The declaration-sensitive comparison effective() cannot
// make: a config's behavior, not its serialized values.
func checkKinds(t *testing.T, dir string) (int, []string) {
	t.Helper()
	t.Chdir(dir)
	prev := checkJSON
	checkJSON = true
	t.Cleanup(func() { checkJSON = prev })
	var err error
	out := capture(t, func() { err = checkCmd.RunE(checkCmd, nil) })
	var rep CheckReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("parse %q: %v", out, jerr)
	}
	kinds := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		kinds = append(kinds, f.Kind)
	}
	return Code(err), kinds
}

// A bare [severity] is a decision with no value in it. SeverityConfigured reads
// whether the table exists, so a printed copy assembled from values alone dropped
// the header and turned the source's UNDECLARED KEY into a clean report. Values do
// not settle this: the two files serialize identically and behave differently,
// which is why the assertion is a check run rather than a comparison of Config.
func TestAnEmptyTableDeclarationSurvivesThePrint(t *testing.T) {
	globalConfig(t, "")
	body := fmt.Sprintf("schema_version = %d\nproject = \"audit\"\n", config.SchemaVersion) + `
[paths]
root_marker = "README.md"
ledger = "ledger.jsonl"

[verify]
default = "true"

[git]
enabled = false

[register]
backend = "none"

[severity]
`
	src := projectWith(t, body)
	srcCode, srcKinds := checkKinds(t, src)
	if srcCode != 2 || !slices.Contains(srcKinds, "UNDECLARED KEY") {
		t.Fatalf("the source reported exit %d %v, want 2 with UNDECLARED KEY", srcCode, srcKinds)
	}

	copied := projectWith(t, printed(t, src))
	if !strings.Contains(printed(t, src), "\n[severity]") {
		t.Errorf("the print dropped the empty table:\n%s", printed(t, src))
	}
	// The header and nothing under it. Filling key or ranks would pin today's
	// default where the original inherits whatever the per-user file says next.
	for _, filled := range []string{"key =", "ranks ="} {
		if strings.Contains(printed(t, src), filled) {
			t.Errorf("the print filled a leaf the project never wrote (%s):\n%s", filled, printed(t, src))
		}
	}
	code, kinds := checkKinds(t, copied)
	if code != srcCode || !slices.Equal(kinds, srcKinds) {
		t.Errorf("the copy reported exit %d %v; the source reported %d %v", code, kinds, srcCode, srcKinds)
	}
}

// check reads the config, decides the version, and only then opens the backlog,
// ledger and shelf. Loading a session outright read all four, so a project whose
// own file declares no version answered with whatever its ledger path happened to
// be: a directory where a file belongs reported "is a directory" and exit 1 while
// the version gap went unnoticed.
func TestCheckDecidesTheVersionBeforeReadingData(t *testing.T) {
	current := fmt.Sprintf("schema_version = %d\n", config.SchemaVersion)
	base := "project = \"old\"\n\n[paths]\nroot_marker = \"README.md\"\nledger = \"ledger.jsonl\"\n\n[verify]\ndefault = \"true\"\n"
	for _, c := range []struct {
		name, conf string
		wantCode   int
		wantKind   string
		wantErr    string
	}{
		{
			name: "project declares no version", conf: base,
			wantCode: 2, wantKind: "STALE CONFIG",
		},
		{
			name: "a current project still surfaces the read", conf: current + base,
			wantCode: 1, wantErr: "is a directory",
		},
		{
			name:     "an unrelated config error keeps priority",
			conf:     current + base + "\n[github]\ncaveat_label = \"follow-up\"\n",
			wantCode: 1, wantErr: "caveat_label",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalConfig(t, current)
			dir := projectWith(t, c.conf)
			// A directory where the ledger file belongs: unreadable, and readable
			// enough that the failure is a read rather than an absent path.
			if err := os.MkdirAll(filepath.Join(dir, "ledger.jsonl"), 0o750); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			prev := checkJSON
			checkJSON = true
			t.Cleanup(func() { checkJSON = prev })
			var err error
			out := capture(t, func() { err = checkCmd.RunE(checkCmd, nil) })
			if code := Code(err); code != c.wantCode {
				t.Fatalf("exit %d (%v), want %d:\n%s", code, err, c.wantCode, out)
			}
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error %v, want one naming %q", err, c.wantErr)
				}
				return
			}
			var rep CheckReport
			if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
				t.Fatalf("parse %q: %v", out, jerr)
			}
			if len(rep.Findings) != 1 || rep.Findings[0].Kind != c.wantKind {
				t.Fatalf("reported %+v, want one %s", rep.Findings, c.wantKind)
			}
			if rep.Stopped == "" || rep.OK {
				t.Errorf("ok = %v, stopped = %q: the report reads as a whole validation", rep.OK, rep.Stopped)
			}
		})
	}
}

// A key the layout reads off the checkout is not a default the project declined.
// [github] repo renders live for any project inside a repo with an origin remote,
// so STALE CONFIG would report every such project that leaves gh to infer,
// this repository's own examples/ among them. Pasting the key is the wrong
// remedy too: it pins
// whatever the remote said that day into a file that was inferring on purpose.
func TestTheStaleConfigFindingIgnoresWhatTheLayoutReadsOffTheTree(t *testing.T) {
	globalConfig(t, "")
	dir := projectWith(t, fmt.Sprintf("schema_version = %d\n", config.SchemaVersion)+`project = "derived"

[paths]
root_marker = "README.md"
ledger = "ledger.jsonl"

[frontmatter]
required = ["title", "deps"]
optional = []
shape = { severity = "enum:C,H,M,L" }

[verify]
default = "true"

[git]
enabled = false

[register]
backend = "none"
`)
	// An origin remote is what makes the layout write [github] repo live.
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "remote", "add", "origin", "git@github.com:someone/elsewhere.git")

	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := undeclared(in.Root, in.Config.Verify["default"], in.Declared)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range missing {
		if derivedFromTree(k.Dotted) {
			t.Errorf("%s is reported missing, and the layout takes its value from the checkout", k.Dotted)
		}
	}
	// The diff still names it, under the keys nothing reports: a key you do not
	// know about is one you cannot go looking for, and that section is where the
	// command says so.
	if diff := diffOf(t, dir); !strings.Contains(diff, "github.repo") && !strings.Contains(diff, "repo =") {
		t.Errorf("--diff does not mention the key at all:\n%s", diff)
	}
}

// drover config names where [git] merge_method came from: the project, the
// per-user file that may not set it, or drover's default. init writes nothing,
// so no existing project reads as STALE CONFIG for leaving it unset.
func TestTheMergeMethodLineNamesItsSource(t *testing.T) {
	for _, c := range []struct {
		name   string
		m      mode
		global globalDefaults
		w      written
		want   string
	}{
		{"project", forReport, globalDefaults{}, written{"git.merge_method": `"merge"`}, "\nmerge_method = \"merge\"\n"},
		{"per-user", forReport, globalDefaults{path: "/u/config.toml", keys: map[string]bool{"git.merge_method": true}}, nil,
			"set in /u/config.toml, and drover refuses it there"},
		{"default", forReport, globalDefaults{}, nil, `# merge_method = "squash"   # drover's default`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := mergeMethodLine(c.m, c.global, c.w); !strings.Contains(got, c.want) {
				t.Errorf("got %q, want it to contain %q", got, c.want)
			}
		})
	}
	if got := mergeMethodLine(forInit, globalDefaults{}, nil); got != "" {
		t.Errorf("init writes %q for a key it should leave to the default", got)
	}
}

// drover config names where [backlog] mirror came from: the project, or drover's
// default. The per-user file cannot be a source, because Load refuses it there.
func TestTheMirrorLineNamesItsSource(t *testing.T) {
	if got := mirrorLine(forReport, written{"backlog.mirror": `"github"`}); !strings.Contains(got, "set by this project") ||
		!strings.Contains(got, "\nmirror = \"github\"\n") {
		t.Errorf("project: got %q", got)
	}
	if got := mirrorLine(forReport, nil); !strings.Contains(got, `# mirror = "none"   # drover's default`) {
		t.Errorf("default: got %q", got)
	}
	if got := mirrorLine(forInit, nil); got != "" {
		t.Errorf("init writes %q for a key it should leave to the default", got)
	}
}

// A level switched off reads, in the print, as "off" and the file that wrote
// it, never as a key the project could add: a project's "off" over a per-user 5
// and a per-user "off" the project inherits both look, from the body alone,
// like a level nobody set.
func TestThePrintNamesASwitchedOffLevelAndTheDiffDoesNotOfferIt(t *testing.T) {
	globalConfig(t, "[usage.weekly]\npause_remaining = 5\n\n[usage.overage]\nmax_usd = \"off\"\n")
	dir := projectWith(t, v1Config+"\n[usage.weekly]\npause_remaining = \"off\"\n")
	out := printed(t, dir)
	for _, want := range []string{
		"pause_remaining = \"off\"\n",
		"# [usage.weekly] pause_remaining = \"off\" in .drover.toml (the per-user file sets 5)\n",
		"# [usage.overage] max_usd = \"off\" in ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the printed config does not carry %q:\n%s", want, out)
		}
	}
	diff := diffOf(t, dir)
	for _, key := range []string{"usage.weekly.pause_remaining", "usage.overage.max_usd"} {
		table, name := config.Table(key)
		section := diff[strings.Index(diff, "["+table+"]"):]
		if end := strings.Index(section[1:], "\n["); end >= 0 {
			section = section[:end+1]
		}
		if strings.Contains(section, name+" = ") {
			t.Errorf("--diff offers %s, which a file switched off:\n%s", key, diff)
		}
	}
}

// The header over each --diff section says a line appends as it stands, so the
// whole report, pasted onto the minimal config it was printed for, has to load
// and resolve every key to what it resolved to before. A key whose unset state
// has no literal is where that breaks: max_usd = 0 refuses to load, and
// pause_remaining = 0 is a hold at 100% used that nobody chose.
func TestTheWholeDiffPastesWithoutChangingWhatResolves(t *testing.T) {
	const minimal = "schema_version = 2\nproject = \"paste\"\n"
	// What init writes lives in the per-user file, as it does on a host whose
	// projects share one layout, so the report is the one section a current
	// project gets. Written into the project instead, its stanzas would open
	// the tables the report opens again, and the header already says a stanza
	// appends only where the file has no such table yet.
	dir := projectWith(t, minimal)
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := undeclared(in.Root, in.Config.Verify["default"], in.Declared)
	if err != nil {
		t.Fatal(err)
	}
	shared, store := "", t.TempDir()
	for _, k := range generated {
		value := k.Default
		if k.Table == "paths" { // a per-user path is absolute
			value = strconv.Quote(filepath.Join(store, k.Name))
		}
		shared = paste(shared, k.Table, k.Name+" = "+value)
	}
	globalConfig(t, shared)
	before := resolvedKeys(t, dir)

	out := diffOf(t, dir)
	pasted := minimal + "\n" + out
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(pasted), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dir); err != nil {
		t.Fatalf("the config with --diff appended does not load: %v\n%s", err, out)
	}
	after := resolvedKeys(t, dir)
	for key, was := range before {
		if after[key] != was {
			t.Errorf("%s resolved to %s before the paste and %s after", key, was, after[key])
		}
	}
}

// resolvedKeys is every key Inspect reports, at the literal its file wrote or
// the default drover resolves it to, plus each usage level as the accessor
// every caller reads it through. The literal alone cannot tell a pasted
// pause_remaining = 0 from an unset one, and only the accessor says which of
// the two holds.
func resolvedKeys(t *testing.T, dir string) map[string]string {
	t.Helper()
	in, err := config.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(in.Path)
	if err != nil {
		t.Fatal(err)
	}
	written, err := config.Rendered(string(text))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range config.Keys() {
		out[k.Dotted] = k.Default
	}
	for _, k := range written {
		out[k.Dotted] = k.Default
	}
	for _, window := range []string{config.UsageWeekly, config.UsageFiveHour} {
		for _, step := range []string{"notify", "pause"} {
			v, ok := in.Config.UsageLevel(window, step)
			out["UsageLevel("+window+", "+step+")"] = fmt.Sprint(v, ok)
		}
	}
	for _, step := range []string{"max", "notify"} {
		v, ok := in.Config.OverageUSD(step)
		out["OverageUSD("+step+")"] = fmt.Sprint(v, ok)
	}
	return out
}
