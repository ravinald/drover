package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/ravinald/drover/internal/config"
)

// The generated file is the first thing a new project reads, so a syntax error
// or an undeclared key in it fails before the operator has any context to debug
// with. The schema is closed, which makes loading it the whole assertion.
func TestGeneratedConfigParsesAgainstTheSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(renderConfig(forInit, dir, globalDefaults{}, "", nil, nil)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Load is the real gate: it rejects an undeclared key and runs every
	// validation rule, so this proves the generator and the schema agree.
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Paths.RootMarker != "go.mod" {
		t.Errorf("root_marker = %q, want go.mod", cfg.Paths.RootMarker)
	}
	// The empty gate is the whole point: a generated project must fail check
	// until somebody names the command that verifies its work.
	if g, ok := cfg.Verify["default"]; !ok || g != "" {
		t.Errorf("verify.default = %q (present=%v), want present and empty", g, ok)
	}
}

// A tree with no recognised build file gets no root_marker rather than a
// fabricated one: the marker's job is to refuse a config that travelled, and a
// marker naming a file that does not exist fails every load instead.
func TestRootMarkerOmittedWhenNothingRecognised(t *testing.T) {
	dir := t.TempDir()
	if got := rootMarker(dir); got != "" {
		t.Errorf("rootMarker = %q, want empty", got)
	}
	if strings.Contains(renderConfig(forInit, dir, globalDefaults{}, "", nil, nil), "root_marker") {
		t.Error("config names a root_marker for a tree with no build file")
	}
}

// A wrong repo routes intake labels to somebody else's tracker, so anything
// unparseable must yield empty rather than a guess.
func TestOriginRepoRejectsWhatItCannotParse(t *testing.T) {
	for _, url := range []string{
		"https://gitlab.com/owner/name.git",
		"/srv/git/bare.git",
		"",
	} {
		dir := t.TempDir()
		if url != "" {
			gitInit(t, dir, url)
		}
		if got := originRepo(dir); got != "" {
			t.Errorf("%q -> %q, want empty", url, got)
		}
	}
}

func TestOriginRepoReadsGitHubRemotes(t *testing.T) {
	for _, c := range []struct{ url, want string }{
		{"git@github.com:ravinald/drover.git", "ravinald/drover"},
		{"https://github.com/ravinald/drover.git", "ravinald/drover"},
		{"https://github.com/ravinald/drover", "ravinald/drover"},
	} {
		dir := t.TempDir()
		gitInit(t, dir, c.url)
		if got := originRepo(dir); got != c.want {
			t.Errorf("%s -> %q, want %q", c.url, got, c.want)
		}
	}
}

func gitInit(t *testing.T, dir, remote string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A directory that was never a repository is the case [git] exists for. The
// generated config has to declare it, because nothing at run time will: drover
// reads the declaration rather than probing for .git.
func TestInitDeclaresGitOffWithNoWorkTree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	body := renderConfig(forInit, dir, globalDefaults{}, "", nil, nil)
	if !strings.Contains(body, "[git]\n") || !strings.Contains(body, "enabled = false") {
		t.Fatalf("no [git] enabled = false in a directory with no work tree:\n%s", body)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Git.On() {
		t.Error("the generated config reads as git-enabled")
	}
}

// A repository gets nothing new: the default is already what it needs, and a
// key written to say so is a key someone has to keep true.
func TestInitWritesNoGitBlockInAWorkTree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	gitInit(t, dir, "git@github.com:ravinald/drover.git")
	if body := renderConfig(forInit, dir, globalDefaults{}, "", nil, nil); strings.Contains(body, "[git]") {
		t.Errorf("a work tree got a [git] block it does not need:\n%s", body)
	}
}

// Only against a global that turned git off, because that is the one case where
// the project has something to say that the default does not already say.
func TestInitOverridesAGlobalThatTurnedGitOff(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, "git@github.com:ravinald/drover.git")
	global := globalDefaults{path: "/etc/drover.toml", keys: map[string]bool{"git.enabled": true}}
	body := renderConfig(forInit, dir, global, "", nil, nil)
	if !strings.Contains(body, "enabled = true") {
		t.Errorf("a work tree under a git-off global did not override it:\n%s", body)
	}
}

// A key repeated in the project file shadows the global rather than inheriting
// it, so the next edit to the global file would silently miss this project.
func TestInitOmitsWhatTheGlobalAlreadySupplies(t *testing.T) {
	dir := t.TempDir()
	global := globalDefaults{
		path: "/etc/drover.toml",
		keys: map[string]bool{
			"paths.backlog": true, "paths.ledger": true, "paths.reports": true,
			"verify.default": true, "intake.stop_marker": true,
		},
	}
	body := renderConfig(forInit, dir, global, "", nil, nil)
	// Assignments, not bare words: the prose in other blocks legitimately names
	// the ledger and the gate.
	for _, key := range []string{"backlog ", "ledger ", "reports ", "[paths]", "[verify]", "[intake]"} {
		if strings.Contains(body, key) {
			t.Errorf("generated config repeats %q, which the global already sets:\n%s", key, body)
		}
	}
	// Not everything is inherited, and the block that is not must survive.
	if !strings.Contains(body, "[frontmatter]") {
		t.Errorf("generated config dropped a block the global does not set:\n%s", body)
	}
}

// The acceptance test for the generated config: answered, a fresh project
// passes its own validation; unanswered, it fails with exactly one line to act
// on. The empty gate was the only reason `drover init && drover check` could
// not exit 0, which made a finding the newcomer's introduction to the tool.
func TestGeneratedProjectPassesCheckOnceTheVerifyCommandIsNamed(t *testing.T) {
	for _, c := range []struct {
		name string
		gate string
		want int
		find string
	}{
		{"verify command given", "true", 0, ""},
		{"verify command left empty", "", ExitFindings, "NO VERIFY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			initVerify = c.gate
			t.Cleanup(func() { initVerify = "" })

			capture(t, func() {
				if err := execute(t, "init"); err != nil {
					t.Fatalf("init: %v", err)
				}
			})

			var err error
			out := capture(t, func() { err = execute(t, "check") })
			if got := Code(err); got != c.want {
				t.Errorf("check exited %d (%v), want %d:\n%s", got, err, c.want, out)
			}
			if c.find != "" && !strings.Contains(out, c.find) {
				t.Errorf("check wrote:\n%s\nwant the one finding %q", out, c.find)
			}
			// The shape table is what stops a fresh project accepting
			// `severity: Z` as unranked, which renders identically to a body
			// that declared no rank at all.
			body, err := os.ReadFile(filepath.Join(dir, ".drover.toml")) // #nosec G304 -- a path this test just wrote
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"[frontmatter.shape]", `severity = "enum:C,H,M,L"`} {
				if !strings.Contains(string(body), want) {
					t.Errorf("the generated config is missing %q", want)
				}
			}
		})
	}
}

// closes is what link, resolve and finish read, and [frontmatter] is closed, so a
// generated project that omits it cannot record what any merge resolved. It
// arrived after these defaults were written and reached only drover's own config.
func TestGeneratedConfigDeclaresTheClosesKey(t *testing.T) {
	body := renderConfig(forInit, t.TempDir(), globalDefaults{}, "", nil, nil)

	for _, want := range []string{`"closes"`, `closes   = "intlist"`} {
		if !strings.Contains(body, want) {
			t.Errorf("generated config omits %s:\n%s", want, body)
		}
	}
}

// The shape table is its own block because a per-user config commonly sets the
// two lists and no shape. Folded into one, such a project inherits the lists and
// gets no shape, so nothing it declares is ever checked against one.
func TestGeneratedShapeSurvivesAGlobalThatSetsTheLists(t *testing.T) {
	global := globalDefaults{
		path: "/etc/drover.toml",
		keys: map[string]bool{"frontmatter.required": true, "frontmatter.optional": true},
	}

	body := renderConfig(forInit, t.TempDir(), global, "", nil, nil)

	if strings.Contains(body, "optional = [") {
		t.Errorf("generated config repeats a list the global already sets:\n%s", body)
	}
	if !strings.Contains(body, "[frontmatter.shape]") {
		t.Errorf("generated config dropped the shape table the global does not set:\n%s", body)
	}
}

// Every key the layout suggests has to be a key drover reads. The suggestions are
// comments, so nothing type-checked them: [runners.codex] was offered with
// `argv = ["codex", "exec"]`, and runner.Spec reads interactive, headless and
// stdin. A reader who uncommented it got a config Load refuses, from a line drover
// told them to write.
//
// The assertion uncomments the layout's own suggestions and asks the decoder what
// it could not place. Value-level conflicts are not the question here — an
// unplaceable key is.
func TestEveryKeyTheLayoutSuggestsIsOneDroverReads(t *testing.T) {
	body := renderConfig(forInit, t.TempDir(), globalDefaults{}, "true", nil, nil)

	assignment := regexp.MustCompile(`^#\s*([A-Za-z_][\w.]*)\s*=`)
	table := regexp.MustCompile(`^#\s*(\[[\w.]+\])\s*$`)
	var live []string
	for _, l := range strings.Split(body, "\n") {
		switch {
		case table.MatchString(l):
			live = append(live, table.FindStringSubmatch(l)[1])
		case assignment.MatchString(l):
			// The trailing "# unset lets gh infer…" kind of aside would make the
			// value a comment on its own line, which is still valid TOML.
			live = append(live, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "#")))
		case !strings.HasPrefix(strings.TrimSpace(l), "#"):
			live = append(live, l)
		}
	}

	var c config.Config
	md, err := toml.Decode(strings.Join(live, "\n"), &c)
	if err != nil {
		t.Fatalf("the layout with its suggestions uncommented is not valid TOML: %v\n%s",
			err, strings.Join(live, "\n"))
	}
	var dead []string
	for _, k := range md.Undecoded() {
		dead = append(dead, k.String())
	}
	if len(dead) > 0 {
		t.Errorf("the layout suggests %d key(s) nothing reads: %v", len(dead), dead)
	}
}
