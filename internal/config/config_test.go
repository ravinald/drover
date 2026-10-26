package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/runner"
)

// write lays out a project tree and returns its root.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// schemaLine opens every fixture whose version is incidental. Rendered from the
// constant so that bumping the schema does not invalidate them all at once; a
// test asserting behavior at some other version writes that version out.
var schemaLine = fmt.Sprintf("schema_version = %d\n", SchemaVersion)

var minimal = schemaLine + `project = "example"
[paths]
root_marker = "go.mod"
`

func TestLoadMinimal(t *testing.T) {
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "example" || c.Root != root {
		t.Errorf("got project=%q root=%q", c.Project, c.Root)
	}
	// Defaults must be applied, or a minimal config resolves paths to the root.
	if c.Paths.Backlog == "" || c.Paths.Ledger == "" || c.Paths.Reports == "" {
		t.Errorf("path defaults not applied: %+v", c.Paths)
	}
	if c.GitHub.FollowUpLabel != "follow-up" || c.GitHub.SeverityPrefix != "severity:" || c.GitHub.PageLimit == 0 {
		t.Errorf("github defaults not applied: %+v", c.GitHub)
	}
	if len(c.Runners) == 0 {
		t.Error("a project declaring no runners must still get the built-in")
	}
}

// The root is discovered by walking up, so drover works from a subdirectory.
// Deriving it from the binary's location is what broke the shell driver through
// a symlink.
func TestFindWalksUp(t *testing.T) {
	root := write(t, map[string]string{
		FileName:            minimal,
		"go.mod":            "module x\n",
		"deep/nested/x.txt": "",
	})
	got, err := Find(filepath.Join(root, "deep", "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Errorf("Find from a subdirectory = %q, want %q", got, root)
	}
}

// A session drive launched works in a worktree that can carry its own tracked
// copy of the config. Walking up from there finds that copy, and every record
// path it names lands in a tree removed once the item is done.
func TestFindAnswersFromDroverRootBeforeWalking(t *testing.T) {
	project := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	tree := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n", "sub/x.txt": ""})
	t.Setenv(runner.EnvRoot, project)
	got, err := Find(filepath.Join(tree, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if got != project {
		t.Errorf("Find with %s set = %q, want %q", runner.EnvRoot, got, project)
	}

	t.Setenv(runner.EnvRoot, t.TempDir())
	if _, err := Find(tree); err == nil || !strings.Contains(err.Error(), runner.EnvRoot) {
		t.Errorf("a %s holding no config returned %v, want an error naming the variable", runner.EnvRoot, err)
	}
}

func TestFindReportsAbsence(t *testing.T) {
	if _, err := Find(t.TempDir()); err == nil {
		t.Error("expected an error when no config exists in any parent")
	}
}

// root_marker is what catches a config that travelled to a tree it does not
// describe. Without it every path resolves against a stranger's repo.
func TestRootMarkerMustExist(t *testing.T) {
	root := write(t, map[string]string{FileName: minimal})
	_, err := Load(root)
	if err == nil {
		t.Fatal("a missing root_marker must be fatal")
	}
	if !strings.Contains(err.Error(), "root_marker") {
		t.Errorf("error should name root_marker, got: %v", err)
	}
}

// An unrecognised key is a typo that would otherwise take effect as silence.
func TestUnknownKeyIsRejected(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\nbogus_key = 1\n",
		"go.mod": "module x\n",
	})
	_, err := Load(root)
	if err == nil {
		t.Fatal("an unknown key must be rejected")
	}
	if !strings.Contains(err.Error(), "bogus_key") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestSchemaVersionMismatchRefuses(t *testing.T) {
	// 99 stays literal: the test is about a version this build does not
	// understand, and rendering it from SchemaVersion would assert nothing.
	root := write(t, map[string]string{
		FileName: "schema_version = 99\nproject = \"x\"\n",
		"go.mod": "module x\n",
	})
	if _, err := Load(root); err == nil {
		t.Error("a future schema version must be refused, not misread")
	}

	// An older version is refused too: loaded under this schema, its keys would
	// be read by whatever now holds their names. 1 stays literal for the same
	// reason 99 does.
	old := write(t, map[string]string{
		FileName: "schema_version = 1\nproject = \"x\"\n[paths]\nroot_marker = \"go.mod\"\n",
		"go.mod": "module x\n",
	})
	_, err := Load(old)
	if err == nil {
		t.Fatal("a config of an older schema_version loaded")
	}
	if !strings.Contains(err.Error(), "schema_version 1") {
		t.Errorf("the refusal does not name the version it refused:\n%v", err)
	}
}

func TestProjectIsRequired(t *testing.T) {
	root := write(t, map[string]string{FileName: schemaLine})
	if _, err := Load(root); err == nil {
		t.Error("project names the log directory and the banner; it cannot be empty")
	}
}

// A key that is both required and optional makes "required" a lie that reads as
// satisfied.
func TestRequiredAndOptionalCannotOverlap(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\n[frontmatter]\nrequired = [\"title\"]\noptional = [\"title\"]\n",
		"go.mod": "module x\n",
	})
	if _, err := Load(root); err == nil {
		t.Error("a key in both lists must be rejected")
	}
}

func TestNamespacePrefixMustBeUppercaseASCII(t *testing.T) {
	for _, bad := range []string{"b", "1", "b2", "Ä"} {
		root := write(t, map[string]string{
			FileName: minimal + "\n[ids]\nnamespaces = { \"" + bad + "\" = \"bug\" }\n",
			"go.mod": "module x\n",
		})
		if _, err := Load(root); err == nil {
			t.Errorf("namespace prefix %q must be rejected — it becomes a filename component", bad)
		}
	}
}

func TestPlannedMaxCannotBeNegative(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\n[ids]\nplanned_max = -1\n",
		"go.mod": "module x\n",
	})
	if _, err := Load(root); err == nil {
		t.Error("a negative bound must be rejected")
	}
}

// A ~ path that resolves inside the project would write multi-MB run logs into
// the repo it was configured to keep them out of.
func TestAbsExpandsTilde(t *testing.T) {
	c := &Config{Root: "/project"}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got := c.Abs("~/.cache/drover")
	if want := filepath.Join(home, ".cache", "drover"); got != want {
		t.Errorf("Abs(~/.cache/drover) = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "/project") {
		t.Error("a ~ path must not resolve inside the project")
	}
}

func TestAbsResolvesRelativeAndKeepsAbsolute(t *testing.T) {
	c := &Config{Root: "/project"}
	if got := c.Abs("docs/x.md"); got != "/project/docs/x.md" {
		t.Errorf("relative: got %q", got)
	}
	if got := c.Abs("/elsewhere/x.md"); got != "/elsewhere/x.md" {
		t.Errorf("absolute: got %q", got)
	}
	if got := c.Abs(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

func TestLogDirPrefersConfigThenCache(t *testing.T) {
	c := &Config{Root: "/project", Project: "p", Paths: Paths{Logs: "/var/logs"}}
	if got := c.LogDir(); got != "/var/logs" {
		t.Errorf("configured logs ignored: %q", got)
	}

	t.Setenv("XDG_CACHE_HOME", "/xdg")
	c = &Config{Root: "/project", Project: "p"}
	if want, got := filepath.Join("/xdg", "drover", "p"), c.LogDir(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixesAreSorted(t *testing.T) {
	c := &Config{IDs: IDs{Namespaces: map[string]string{"F": "feature", "B": "bug", "E": "enh"}}}
	got := c.Prefixes()
	want := []string{"B", "E", "F"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Prefixes() = %v, want %v", got, want)
		}
	}
}

func TestKnownAndRequiredKeys(t *testing.T) {
	c := &Config{Frontmatter: Frontmatter{Required: []string{"title"}, Optional: []string{"kind"}}}
	for _, k := range []string{"title", "kind"} {
		if !c.Known(k) {
			t.Errorf("%q should be known", k)
		}
	}
	if c.Known("nope") {
		t.Error("an undeclared key must not be known")
	}
	if got := c.RequiredKeys(); len(got) != 1 || got[0] != "title" {
		t.Errorf("RequiredKeys() = %v", got)
	}
}

// A project that never heard of severity gets the defaults and is none the
// wiser: nothing it writes changes, and nothing it omits fails.
func TestSeverityDefaultsWithoutAnyConfig(t *testing.T) {
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Severity.Key != "severity" {
		t.Errorf("severity.key = %q, want %q", c.Severity.Key, "severity")
	}
	for rank, want := range map[string]string{"C": "red", "H": "orange", "M": "yellow", "L": "blue"} {
		if got := c.Severity.Ranks[rank]; got != want {
			t.Errorf("ranks[%s] = %q, want %q", rank, got, want)
		}
	}
	// The block was never written, so an undeclared key is the ordinary state of
	// this project rather than a typo drover check should fail CI over.
	if c.SeverityConfigured() {
		t.Error("SeverityConfigured on a config with no [severity] block")
	}
	if c.Known("severity") {
		t.Error("minimal config should not declare severity in [frontmatter]")
	}
}

// Written as a word, read as a letter: the rank table and the item frontmatter
// are authored by different people and neither should have to match the other.
func TestSeverityRanksCanonicaliseToLetters(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + `
[severity]
key = "follow-up"
ranks = { Critical = "red", h = "grey" }
`,
		"go.mod": "module x\n",
	})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Severity.Key != "follow-up" {
		t.Errorf("severity.key = %q, want follow-up", c.Severity.Key)
	}
	if c.Severity.Ranks["C"] != "red" || c.Severity.Ranks["H"] != "grey" {
		t.Errorf("ranks not canonicalised to letters: %v", c.Severity.Ranks)
	}
	if !c.SeverityConfigured() {
		t.Error("SeverityConfigured false after writing a [severity] block")
	}
	// An explicit table replaces the defaults rather than merging into them, so
	// a project can drop a rank it does not use.
	if _, ok := c.Severity.Ranks["M"]; ok {
		t.Error("default M survived an explicit ranks table")
	}
	if code := c.SevColour("critical"); code == "" {
		t.Error("SevColour(critical) empty: the word should reach the C entry")
	}
	if code := c.SevColour("M"); code != "" {
		t.Errorf("SevColour(M) = %q, want empty: M is unmapped here", code)
	}
	if code := c.SevColour("Sev1"); code != "" {
		t.Errorf("SevColour(Sev1) = %q, want empty", code)
	}
}

// Rendering a typo'd colour plain would hide it, and the whole point of the
// block is that the colour is visible.
func TestSeverityRejectsTyposWithTheKnownSet(t *testing.T) {
	for name, c := range map[string]struct{ toml, wants string }{
		"colour": {"[severity]\nranks = { C = \"puce\" }\n", "orange"},
		"rank":   {"[severity]\nranks = { P0 = \"red\" }\n", "C, H, M, L"},
	} {
		root := write(t, map[string]string{FileName: minimal + c.toml, "go.mod": "module x\n"})
		_, err := Load(root)
		if err == nil {
			t.Errorf("%s: accepted %q", name, c.toml)
			continue
		}
		if !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: error %q does not name the known set (%q)", name, err, c.wants)
		}
	}
}

// examples/ is the first thing a reader runs, so it has to keep loading under
// whatever schema this build understands. Without this, a version bump strands
// the example and the failure surfaces in a fresh clone rather than in CI.
func TestExampleProjectLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("examples/.drover.toml does not load: %v", err)
	}
	// Declared, not defaulted: applyDefaults fills an absent schema_version with
	// the current one, so equality alone would pass on a file that omits it.
	if !strings.Contains(readFile(t, filepath.Join(dir, FileName)),
		fmt.Sprintf("schema_version = %d", SchemaVersion)) {
		t.Errorf("examples/.drover.toml does not declare schema_version = %d", SchemaVersion)
	}
	if c.SchemaVersion != SchemaVersion {
		t.Errorf("example loads as schema_version %d, this build understands %d",
			c.SchemaVersion, SchemaVersion)
	}
	// The README sends a reader to `drover check` first, and it exits 0 only
	// while the example has a gate. An empty one is a NO VERIFY finding.
	if c.Verify["default"] == "" {
		t.Error("the example project has no [verify] default, so drover check reports NO VERIFY there")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two settings that disagree are refused at load rather than resolved by
// precedence: a project reading a register it thought it had configured
// elsewhere is the failure, and picking one silently is what produces it.
func TestRegisterBackendContradictionsAreRejected(t *testing.T) {
	for _, c := range []struct {
		name, block, want string
	}{
		{
			name:  "file names a repo",
			block: "[register]\nbackend = \"file\"\n[github]\nrepo = \"owner/name\"\n",
			want:  "register.backend is \"file\" and github.repo names \"owner/name\"",
		},
		{
			name:  "unknown backend",
			block: "[register]\nbackend = \"gitlab\"\n",
			want:  "register.backend is \"gitlab\" (known: github, file, none)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := write(t, map[string]string{FileName: minimal + c.block, "go.mod": "module x\n"})
			_, err := Load(root)
			if err == nil {
				t.Fatal("loaded a config that says two things")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the contradiction %q", err, c.want)
			}
		})
	}
}

// The tracker is the unset answer, and the declaration a command prints has to
// name the resolved backend whether or not anybody wrote the line.
func TestRegisterBackendResolvesFromNoTracker(t *testing.T) {
	for _, c := range []struct{ name, block, want, declaration string }{
		{"nothing declared", "", BackendGitHub, "[register] backend = \"github\""},
		{"none declared", "[register]\nbackend = \"none\"\n", BackendNone, "[register] backend = \"none\""},
		{"backend wins", "[register]\nbackend = \"file\"\n", BackendFile, "[register] backend = \"file\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := write(t, map[string]string{FileName: minimal + c.block, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.RegisterBackend(); got != c.want {
				t.Errorf("backend = %q, want %q", got, c.want)
			}
			if got := cfg.RegisterDeclaration(); got != c.declaration {
				t.Errorf("declaration = %q, want %q — a message naming the wrong key sends a reader to a setting they never made", got, c.declaration)
			}
			if cfg.Paths.Register == "" {
				t.Error("paths.register has no default, so the file backend has nowhere to read")
			}
		})
	}
}

// A cap is a policy about how much a loop may spend. Absent and zero are one
// answer because nothing reads the difference, and a negative is a typo.
func TestTheReviewRoundLimitIsOffUnlessSomebodyTypedIt(t *testing.T) {
	for _, c := range []struct {
		name, extra string
		want        int
		refuses     bool
	}{
		{"absent", "", 0, false},
		{"zero", "\n[review]\nmax_review_rounds = 0\n", 0, false},
		{"a number", "\n[review]\nmax_review_rounds = 4\n", 4, false},
		{"negative", "\n[review]\nmax_review_rounds = -1\n", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := write(t, map[string]string{FileName: minimal + c.extra, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if c.refuses {
				if err == nil {
					t.Fatal("accepted a negative cap")
				}
				if !strings.Contains(err.Error(), "max_review_rounds must be 0") {
					t.Errorf("the refusal does not name the rule: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Review.MaxReviewRounds != c.want {
				t.Errorf("max_review_rounds = %d, want %d", cfg.Review.MaxReviewRounds, c.want)
			}
		})
	}
}

// An additive table must not need a schema bump: versionMatches refuses any
// mismatch in either direction, so bumping would refuse every existing config
// on the new binary to describe a rename that did not happen.
func TestAnAbsentLoopTableIsTheOrdinaryState(t *testing.T) {
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Loop.MergeGate != nil || c.Loop.DenyPaths != nil {
		t.Errorf("an absent [drive] resolved to %+v, want zero", c.Loop)
	}
	if c.SchemaVersion != SchemaVersion {
		t.Errorf("schema version %d, want %d", c.SchemaVersion, SchemaVersion)
	}
}

// The gate is an argv array rather than a shell string, matching [git]
// merge_check and [runners] and unlike [verify], which drover never executes.
func TestLoopMergeGateIsArgv(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\n[drive]\nmerge_gate = [\"make\", \"merge-gate\"]\nprotected_paths = [\".github/workflows/**\"]\n",
		"go.mod": "module x\n",
	})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Loop.MergeGate) != 2 || c.Loop.MergeGate[0] != "make" {
		t.Errorf("merge_gate = %q", c.Loop.MergeGate)
	}
	if len(c.Loop.DenyPaths) != 1 {
		t.Errorf("protected_paths = %q", c.Loop.DenyPaths)
	}
}

// A key nobody declared is refused rather than dropped, which is what makes a
// config written for a newer binary fail loudly on an older one.
func TestAnUnknownLoopKeyIsRefused(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\n[drive]\nci_timeout = 45\n",
		"go.mod": "module x\n",
	})
	if _, err := Load(root); err == nil {
		t.Fatal("accepted an unknown key under [drive]")
	}
}

// [forge] failure_patterns are regular expressions, so one that does not
// compile is refused at load rather than skipped at the first red check.
func TestFailurePatternsMustCompile(t *testing.T) {
	root := write(t, map[string]string{
		FileName: minimal + "\n[forge]\nfailure_patterns = ['^ERROR ', '(unclosed']\n",
		"go.mod": "module x\n",
	})
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), `forge.failure_patterns holds "(unclosed"`) {
		t.Fatalf("Load = %v, want a refusal naming the pattern", err)
	}
	root = write(t, map[string]string{
		FileName: minimal + "\n[forge]\nfailure_patterns = ['^ERROR ']\n",
		"go.mod": "module x\n",
	})
	c, err := Load(root)
	if err != nil || len(c.Forge.FailurePatterns) != 1 {
		t.Fatalf("Load = %+v, %v, want the one pattern", c, err)
	}
}

// [git] merge_method picks the flag drive hands `gh pr merge`. Unset is squash;
// anything outside the three methods gh knows is refused at load rather than at
// the first merge of a drive.
func TestMergeMethod(t *testing.T) {
	for _, c := range []struct {
		name, line, want, refused string
	}{
		{name: "default", want: "squash"},
		{name: "merge", line: `merge_method = "merge"`, want: "merge"},
		{name: "squash", line: `merge_method = "squash"`, want: "squash"},
		{name: "rebase", line: `merge_method = "rebase"`, want: "rebase"},
		{name: "invalid", line: `merge_method = "fast-forward"`, refused: `git.merge_method is "fast-forward"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := minimal
			if c.line != "" {
				body += "\n[git]\n" + c.line + "\n"
			}
			root := write(t, map[string]string{FileName: body, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("load = %v, want a refusal naming %q", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Git.MergeMethod != c.want {
				t.Errorf("merge_method = %q, want %q", cfg.Git.MergeMethod, c.want)
			}
		})
	}
}

// How a repository merges is a fact about that repository, so a per-user value
// would apply one project's history policy to every project on the machine.
// The refusal names the file the value belongs in.
func TestMergeMethodIsRefusedInThePerUserFile(t *testing.T) {
	withGlobal(t, "[git]\nmerge_method = \"merge\"\n")
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	_, err := Load(root)
	if err == nil {
		t.Fatal("a per-user merge_method loaded")
	}
	for _, want := range []string{GlobalPath(), "git.merge_method", FileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// [backlog] mirror publishes item text, so an unknown target is refused at load
// and so is github on a project whose items cannot record the issue: every
// accept would open another one.
func TestBacklogMirror(t *testing.T) {
	const issueKey = "\n[frontmatter]\nrequired = [\"title\", \"deps\"]\noptional = [\"issue\"]\n"
	for _, c := range []struct {
		name, extra, want, refused string
	}{
		{name: "default", want: MirrorNone},
		{name: "none", extra: "\n[backlog]\nmirror = \"none\"\n", want: MirrorNone},
		{name: "github", extra: issueKey + "\n[backlog]\nmirror = \"github\"\n", want: MirrorGitHub},
		{name: "unknown", extra: issueKey + "\n[backlog]\nmirror = \"gitlab\"\n", refused: `backlog.mirror is "gitlab"`},
		{name: "github without the issue key", extra: "\n[backlog]\nmirror = \"github\"\n", refused: `does not declare "issue"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := write(t, map[string]string{FileName: minimal + c.extra, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("load = %v, want a refusal naming %q", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.BacklogMirror(); got != c.want {
				t.Errorf("mirror = %q, want %q", got, c.want)
			}
		})
	}
}

// One per-user value would publish every project on the machine, including the
// ones whose backlog lives in a private overlay.
func TestBacklogMirrorIsRefusedInThePerUserFile(t *testing.T) {
	withGlobal(t, "[backlog]\nmirror = \"github\"\n")
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	_, err := Load(root)
	if err == nil {
		t.Fatal("a per-user backlog.mirror loaded")
	}
	for _, want := range []string{GlobalPath(), "backlog.mirror", FileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// [ids] allocator decides whether a new item pushes to a remote, so an unknown
// value is refused at load rather than read as local. The remote reaches git's
// argv, where a leading dash is an option.
func TestIDAllocator(t *testing.T) {
	for _, c := range []struct {
		name, extra, allocator, remote, refused string
	}{
		{name: "default", allocator: AllocatorLocal, remote: "origin"},
		{name: "git", extra: "\n[ids]\nallocator = \"git\"\n", allocator: AllocatorGit, remote: "origin"},
		{name: "git with a URL", extra: "\n[ids]\nallocator = \"git\"\nremote = \"git@example.com:o/r.git\"\n",
			allocator: AllocatorGit, remote: "git@example.com:o/r.git"},
		{name: "unknown", extra: "\n[ids]\nallocator = \"svn\"\n", refused: `ids.allocator is "svn"`},
		{name: "option as remote", extra: "\n[ids]\nremote = \"--upload-pack=x\"\n", refused: "ids.remote"},
		{name: "whitespace in remote", extra: "\n[ids]\nremote = \"a b\"\n", refused: "ids.remote"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := write(t, map[string]string{FileName: minimal + c.extra, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("load = %v, want a refusal naming %q", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.IDAllocator(); got != c.allocator {
				t.Errorf("allocator = %q, want %q", got, c.allocator)
			}
			if got := cfg.IDRemote(); got != c.remote {
				t.Errorf("remote = %q, want %q", got, c.remote)
			}
		})
	}
}

// A [numbered_files] glob drive cannot read a number out of would check nothing while
// reading as declared, so each is refused at load with the entry named.
func TestSequences(t *testing.T) {
	for _, c := range []struct {
		name, glob, refused string
	}{
		{name: "migrations", glob: "backend/migrations/{n}_*.up.sql"},
		{name: "no placeholder", glob: "db/*.sql", refused: "carries 0 {n} placeholders"},
		{name: "two placeholders", glob: "db/{n}_{n}.sql", refused: "carries 2 {n} placeholders"},
		{name: "absolute", glob: "/db/{n}.sql", refused: "relative to the repository root"},
		{name: "parent", glob: "../db/{n}.sql", refused: "relative to the repository root"},
		{name: "unclean", glob: "./db/{n}.sql", refused: "relative to the repository root"},
		{name: "bad glob", glob: "db/[{n}.sql", refused: "not a glob path.Match reads"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := minimal + "\n[numbered_files]\nm = " + strconv.Quote(c.glob) + "\n"
			root := write(t, map[string]string{FileName: body, "go.mod": "module x\n"})
			cfg, err := Load(root)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) || !strings.Contains(err.Error(), "numbered_files.m") {
					t.Fatalf("load = %v, want a refusal of numbered_files.m naming %q", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sequences["m"] != c.glob {
				t.Errorf("sequences = %v, want m = %q", cfg.Sequences, c.glob)
			}
		})
	}
}

// A zone is checked at load, so a typo fails naming the key rather than
// rendering every time in UTC.
func TestStatusTimezoneIsValidatedAtLoad(t *testing.T) {
	for zone, ok := range map[string]bool{"UTC": true, "local": true, "America/Los_Angeles": true, "Mars/Olympus": false} {
		root := write(t, map[string]string{FileName: minimal + "[status]\ntimezone = \"" + zone + "\"\n", "go.mod": "module x\n"})
		c, err := Load(root)
		switch {
		case ok && err != nil:
			t.Errorf("%s: %v", zone, err)
		case !ok && (err == nil || !strings.Contains(err.Error(), "status.timezone") || !strings.Contains(err.Error(), zone)):
			t.Errorf("%s: err = %v, want one naming status.timezone and the value", zone, err)
		case ok && zone == "America/Los_Angeles" && c.Status.Location().String() != zone:
			t.Errorf("Location() = %s, want %s", c.Status.Location(), zone)
		}
	}
}

// index takes a path or a URL a page can link to, and refuses a scheme
// html/template would turn into a dead link.
func TestStatusIndexIsValidatedAtLoad(t *testing.T) {
	for ref, ok := range map[string]bool{
		"": true, "index.html": true, "../index.html": true, "/drover/": true,
		"https://example.com/drover/": true, "javascript:alert(1)": false, " index.html": false, "%zz": false,
	} {
		root := write(t, map[string]string{FileName: minimal + "[status]\nindex = " + strconv.Quote(ref) + "\n", "go.mod": "module x\n"})
		c, err := Load(root)
		switch {
		case ok && err != nil:
			t.Errorf("%q: %v", ref, err)
		case ok && c.Status.Index != ref:
			t.Errorf("%q: Index = %q", ref, c.Status.Index)
		case !ok && (err == nil || !strings.Contains(err.Error(), "status.index")):
			t.Errorf("%q: err = %v, want one naming status.index", ref, err)
		}
	}
}

// refresh defaults to 60 seconds, 0 turns the reload off, and a negative
// interval fails at load naming the key.
func TestStatusRefreshIsValidatedAtLoad(t *testing.T) {
	for src, want := range map[string]int{"": 60, "[status]\nrefresh = 0\n": 0, "[status]\nrefresh = 15\n": 15, "[status]\nrefresh = -1\n": -1} {
		root := write(t, map[string]string{FileName: minimal + src, "go.mod": "module x\n"})
		c, err := Load(root)
		switch {
		case want < 0 && (err == nil || !strings.Contains(err.Error(), "status.refresh")):
			t.Errorf("%q: err = %v, want one naming status.refresh", src, err)
		case want >= 0 && err != nil:
			t.Errorf("%q: %v", src, err)
		case want >= 0 && c.Status.RefreshSeconds() != want:
			t.Errorf("%q: RefreshSeconds() = %d, want %d", src, c.Status.RefreshSeconds(), want)
		}
	}
}

// [usage] reads from either file, defaults its thresholds, and refuses a value
// that would measure nothing.
func TestUsageTableLayersAndRefusesWhatMeasuresNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	root := write(t, map[string]string{FileName: minimal, "go.mod": "module x\n"})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.UsageSessionMaxHours() != DefaultSessionMaxHours || c.UsageContextWarn() != DefaultContextWarn || c.Usage.WeeklyBudget != 0 {
		t.Errorf("an undeclared [usage] resolved to %+v", c.Usage)
	}

	global := filepath.Join(home, "drover", GlobalFileName)
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[usage]\nweekly_budget = 600000000\nreset = \"Sat 20:00 America/Los_Angeles\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(root); err != nil || c.Usage.WeeklyBudget != 600000000 || c.Usage.Reset == "" {
		t.Errorf("the per-user [usage] did not reach the project: %+v, %v", c.Usage, err)
	}
	if err := os.Remove(global); err != nil {
		t.Fatal(err)
	}

	for body, want := range map[string]string{
		"weekly_budget = 0\nreset = \"Sat 20:00 UTC\"": "usage.weekly_budget is 0",
		"weekly_budget = 100":                          "usage.reset is unset",
		"reset = \"Caturday 20:00 UTC\"":               "not a weekday",
		"session_max_hours = 0":                        "usage.session_max_hours is 0",
		"context_warn = -1":                            "usage.context_warn is -1",
	} {
		root := write(t, map[string]string{FileName: minimal + "\n[usage]\n" + body + "\n", "go.mod": "module x\n"})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("[usage] %s: err = %v, want one naming %q", body, err, want)
		}
	}
}

// The two levels are percent of the window left, and the pause has to come
// after the notice: a pair the other way round would hold the drive before
// anybody was told it was close.
func TestUsageLevelsLoadAndRefuseBadPairs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	root := write(t, map[string]string{FileName: minimal + "\n[usage.weekly]\nnotify_remaining = 20\npause_remaining = 5\n" +
		"[usage.five_hour]\npause_remaining = 0\n", "go.mod": "module x\n"})
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := c.UsageLevel(UsageWeekly, "notify"); !ok || n != 20 {
		t.Errorf("weekly notify = %d %v, want 20", n, ok)
	}
	if p, ok := c.UsageLevel(UsageFiveHour, "pause"); !ok || p != 0 {
		t.Errorf("five-hour pause = %d %v, want 0 and set: 0 is a level, not unset", p, ok)
	}
	if _, ok := c.UsageLevel(UsageFiveHour, "notify"); ok {
		t.Error("an unset five-hour notify reads as set")
	}
	if c.OverageMode() != OveragePause {
		t.Errorf("overage mode = %q, want the default %q", c.OverageMode(), OveragePause)
	}

	global := filepath.Join(home, "drover", GlobalFileName)
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[usage.overage]\nmode = \"continue\"\nmax_usd = 50.0\nnotify_usd = 30.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(root); err != nil || c.OverageMode() != OverageContinue {
		t.Fatalf("the per-user [usage.overage] did not reach the project: %+v, %v", c.Usage.Overage, err)
	}
	if m, ok := c.OverageUSD("max"); !ok || m != 50 {
		t.Errorf("max_usd = %g %v, want 50", m, ok)
	}
	if err := os.Remove(global); err != nil {
		t.Fatal(err)
	}

	for body, want := range map[string]string{
		"[usage.weekly]\nnotify_remaining = 5\npause_remaining = 20": "pause_remaining is 20 and notify_remaining is 5",
		"[usage.weekly]\nnotify_remaining = 5\npause_remaining = 5":  "pause_remaining is 5 and notify_remaining is 5",
		"[usage.five_hour]\nnotify_remaining = 100":                  "usage.five_hour.notify_remaining is 100",
		"[usage.weekly]\npause_remaining = -1":                       "usage.weekly.pause_remaining is -1",
		"[usage.overage]\nmode = \"spend\"":                          "usage.overage.mode is \"spend\"",
		"[usage.overage]\nmax_usd = 0.0":                             "usage.overage.max_usd is 0",
		"[usage.overage]\nmax_usd = 10.0\nnotify_usd = 10.0":         "notify_usd must be below max_usd",
	} {
		root := write(t, map[string]string{FileName: minimal + "\n" + body + "\n", "go.mod": "module x\n"})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one naming %q", body, err, want)
		}
	}
}

// "off" is how a project removes a level the per-user file sets: TOML has no
// null, and every number, 0 included, is a level. The last file decoded wins
// either way, as for every other key.
func TestUsageLevelOffOverridesTheOtherLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	global := filepath.Join(home, "drover", GlobalFileName)
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	layered := func(perUser, project string) (*Config, error) {
		t.Helper()
		if err := os.WriteFile(global, []byte(perUser), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(write(t, map[string]string{FileName: minimal + "\n" + project + "\n", "go.mod": "module x\n"}))
	}

	c, err := layered("[usage.weekly]\npause_remaining = 5\n", "[usage.weekly]\npause_remaining = \"off\"")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.UsageLevel(UsageWeekly, "pause"); ok {
		t.Errorf("a project's \"off\" over a per-user 5 reads as set, at %d", p)
	}
	off := c.SwitchedOff()
	if len(off) != 1 || off[0].Key != "usage.weekly.pause_remaining" || off[0].Path != filepath.Join(c.Root, FileName) || off[0].Inherited != "5" {
		t.Errorf("SwitchedOff = %+v, want weekly pause_remaining off in the project over the per-user 5", off)
	}

	c, err = layered("[usage.weekly]\npause_remaining = \"off\"\n", "[usage.weekly]\npause_remaining = 5")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.UsageLevel(UsageWeekly, "pause"); !ok || p != 5 {
		t.Errorf("a project's 5 over a per-user \"off\" = %d %v, want 5 and set", p, ok)
	}
	if off := c.SwitchedOff(); len(off) != 0 {
		t.Errorf("SwitchedOff = %+v, want nothing: the project's number won", off)
	}

	c, err = layered("[usage.overage]\nmode = \"continue\"\nmax_usd = 50\n", "[usage.overage]\nmax_usd = \"off\"")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := c.OverageUSD("max"); ok {
		t.Errorf("max_usd = \"off\" reads as a cap of %g", m)
	}

	// The pair check reads "off" as unset, the way it reads a key nobody wrote.
	if c, err = layered("", "[usage.weekly]\npause_remaining = \"off\"\nnotify_remaining = 20"); err != nil {
		t.Errorf("pause_remaining = \"off\" beside notify_remaining = 20: %v", err)
	} else if n, ok := c.UsageLevel(UsageWeekly, "notify"); !ok || n != 20 {
		t.Errorf("notify_remaining = %d %v, want 20", n, ok)
	}

	for _, c := range []struct{ perUser, project, want string }{
		{"", "[usage.weekly]\npause_remaining = \"of\"", "usage.weekly.pause_remaining"},
		{"[usage.five_hour]\nnotify_remaining = \"none\"\n", "", "usage.five_hour.notify_remaining"},
		{"", "[usage.overage]\nnotify_usd = \"OFF\"", "usage.overage.notify_usd"},
		{"", "[usage.weekly]\npause_remaining = 5.5", "usage.weekly.pause_remaining"},
		{"", "[usage.overage]\nmode = \"of\"", "usage.overage.mode"},
		{"", "[usage.overage]\nmode = \"off\"", "usage.overage.mode"},
		{"[usage.overage]\nmode = \"off\"\n", "", "usage.overage.mode"},
	} {
		_, err := layered(c.perUser, c.project)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q over %q: err = %v, want one naming %s", c.project, c.perUser, err, c.want)
			continue
		}
		file := filepath.Base(global)
		if c.perUser == "" {
			file = FileName
		}
		if !strings.Contains(err.Error(), file) {
			t.Errorf("%q over %q: err = %v, want one naming %s", c.project, c.perUser, err, file)
		}
		if strings.HasPrefix(c.want, "usage.overage.mode") {
			continue
		}
		if !strings.Contains(err.Error(), `"off"`) || !strings.Contains(err.Error(), "give a") {
			t.Errorf("%q: err = %v, want one naming both accepted forms", c.project, err)
		}
	}
}
