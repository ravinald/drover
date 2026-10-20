package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
)

// loadsAsGlobal writes body where GlobalPath resolves, points a throwaway
// project at it, and returns the merged config. Load is the real gate: it
// rejects an undeclared key, holds a global path to the absolute-or-~ rule and
// runs every validation, so this proves the generator and the loader agree.
func loadsAsGlobal(t *testing.T, body string) *config.Config {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "drover")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, config.GlobalFileName), body)

	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), schemaLine+"project = \"widget\"\n")
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("generated global config does not load: %v", err)
	}
	return cfg
}

// The file init --user writes is the first thing drover reads on every project on the
// machine, so a key it cannot parse fails before the operator has any context.
func TestInitUserDefaultsProduceALoadableConfig(t *testing.T) {
	cfg := loadsAsGlobal(t, renderGlobal(defaultAnswers()))

	if cfg.Paths.Backlog != ".drover/backlog" {
		t.Errorf("paths.backlog = %q, want the in-project default left alone", cfg.Paths.Backlog)
	}
	// The empty verify command has to survive init --user, or every project inherits a check
	// that passes without verifying anything.
	if g := cfg.Verify["default"]; g != "" {
		t.Errorf("verify.default = %q, want empty — a machine-wide verify command satisfies check for a project where it is wrong", g)
	}
	if cfg.Run.DefaultRunner != "claude" {
		t.Errorf("run.default_runner = %q, want claude", cfg.Run.DefaultRunner)
	}
}

// The centralized choice is the one that can produce an illegal file: a global
// location must be absolute or ~-rooted, and must name the project it resolves
// for or every project shares one directory.
func TestInitUserCentralPathsResolvePerProject(t *testing.T) {
	a := defaultAnswers()
	a.Central = true
	a.Verify = "make check"
	a.Posture = "Trace every requirement to shipped code."
	cfg := loadsAsGlobal(t, renderGlobal(a))

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "share", "drover", "widget", "items")
	if got := cfg.Abs(cfg.Paths.Backlog); got != want {
		t.Errorf("paths.backlog = %q, want %q", got, want)
	}
	if cfg.Verify["default"] != "make check" {
		t.Errorf("verify.default = %q, want the answered verify command", cfg.Verify["default"])
	}
	if cfg.Agents.Principles == "" {
		t.Error("answered principles did not reach the file")
	}
}

// Pressing return through the whole walk must land on the same file --defaults
// writes. Two ways to take the defaults that disagree is a wizard lying about
// what it is offering.
func TestWalkAcceptingEveryDefaultMatchesDefaults(t *testing.T) {
	p := newPrompter(strings.NewReader(strings.Repeat("\n", 8)), io.Discard)
	got, err := walk(p, defaultAnswers())
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if renderGlobal(got) != renderGlobal(defaultAnswers()) {
		t.Errorf("the walk taking every default differs from --defaults:\n%s", renderGlobal(got))
	}
}

func TestWalkCarriesTheAnswers(t *testing.T) {
	script := strings.Join([]string{
		"2",                  // centrally
		"make check",         // gate
		"codex",              // runner
		"D",                  // bug prefix
		"title, deps, owner", // required
		"kind, severity",     // optional
		"Trace every requirement to shipped code.",
	}, "\n") + "\n"

	got, err := walk(newPrompter(strings.NewReader(script), io.Discard), defaultAnswers())
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !got.Central {
		t.Error("choosing 2 did not select the central layout")
	}
	for _, c := range []struct{ name, got, want string }{
		{"verify", got.Verify, "make check"},
		{"runner", got.Runner, "codex"},
		{"bug prefix", got.Bug, "D"},
		{"principles", got.Posture, "Trace every requirement to shipped code."},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	body := renderGlobal(got)
	if !strings.Contains(body, `required = ["title", "deps", "owner"]`) {
		t.Errorf("required list did not render as TOML:\n%s", body)
	}
	loadsAsGlobal(t, body)
}

// Treating a typo as assent is how a wizard writes a config nobody chose, so an
// answer outside the menu re-asks rather than falling through to the default.
func TestChooseRejectsWhatIsNotOnTheMenu(t *testing.T) {
	var out strings.Builder
	p := newPrompter(strings.NewReader("9\nyes\n2\n"), &out)
	if got := p.choose("pick", []string{"a", "b"}, 0); got != 1 {
		t.Errorf("choose = %d, want 1 — it took an off-menu answer", got)
	}
	if n := strings.Count(out.String(), "not one of 1-2"); n != 2 {
		t.Errorf("re-asked %d times, want 2", n)
	}
}

// Input that runs out mid-walk takes the remaining defaults rather than
// abandoning the walk, and never reports a failure the operator cannot act on.
func TestWalkSurvivesInputRunningOut(t *testing.T) {
	got, err := walk(newPrompter(strings.NewReader("2\nmake check\n"), io.Discard), defaultAnswers())
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !got.Central || got.Verify != "make check" {
		t.Errorf("answers given before the input ran out were lost: %+v", got)
	}
	if got.Runner != "claude" {
		t.Errorf("run.default_runner = %q, want the default once input ran out", got.Runner)
	}
	loadsAsGlobal(t, renderGlobal(got))
}

func TestTomlListDropsEmptyEntries(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"title, deps", `["title", "deps"]`},
		{"title,,deps,", `["title", "deps"]`},
		{"  ", `[]`},
	} {
		if got := tomlList(c.in); got != c.want {
			t.Errorf("tomlList(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// The per-user config is layered under every project on the machine, so an
// omission here reaches all of them: a project inheriting these lists cannot
// declare closes and no merge it records closes anything.
func TestInitUserDefaultsDeclareTheClosesKey(t *testing.T) {
	cfg := loadsAsGlobal(t, renderGlobal(defaultAnswers()))

	if !cfg.Known(closesKey) {
		t.Errorf("frontmatter optional = %v, which declares no %q", cfg.Frontmatter.Optional, closesKey)
	}
}
