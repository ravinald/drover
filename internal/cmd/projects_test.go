package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/projects"
	"github.com/spf13/pflag"
)

// registryRows is the per-user registry as the file holds it, tombstones and
// repeats included.
func registryRows(t *testing.T) []projects.Row {
	t.Helper()
	rows, err := projects.Read(projects.Path(config.GlobalPath()))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// outsideTempDir points TMPDIR at a fresh directory, so the fixtures this test
// made before it, which t.TempDir put under the old one, are no longer scratch
// to status --all-projects. Later t.TempDir calls stay where the first put them.
func outsideTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func resetProjectsFlags(t *testing.T) {
	reset := func() {
		projectsJSON, projectsAll, projectsForget = false, false, ""
		projectsCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	}
	reset()
	t.Cleanup(reset)
}

func runStatus(t *testing.T, args ...string) string {
	t.Helper()
	resetStatusFlags(t)
	return capture(t, func() {
		if err := execute(t, append([]string{"status", "--no-tracker"}, args...)...); err != nil {
			t.Fatalf("status %v: %v", args, err)
		}
	})
}

func projectsJSONRows(t *testing.T, args ...string) []ProjectRow {
	t.Helper()
	resetProjectsFlags(t)
	out := capture(t, func() {
		if err := execute(t, append([]string{"projects", "--json"}, args...)...); err != nil {
			t.Fatalf("projects --json %v: %v", args, err)
		}
	})
	var got []ProjectRow
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("projects --json did not parse: %v\n%s", err, out)
	}
	return got
}

// The fixture sits under the OS temp directory, and it registers anyway: what is
// scratch is decided where the index reads the registry, not where it is written.
func TestAProjectCommandRegistersItsProjectOnce(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)

	runStatus(t)
	rows := registryRows(t)
	if len(rows) != 1 || rows[0].Project != "example" || rows[0].Repo != "owner/example" || rows[0].Registered == "" {
		t.Fatalf("the first status left %+v, want one row for the project", rows)
	}
	runStatus(t)
	if n := len(registryRows(t)); n != 1 {
		t.Errorf("a second status left %d rows, want still 1", n)
	}
}

func TestALaunchedSessionRegistersNothing(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	t.Setenv("DROVER_ACTOR", "agent")
	runStatus(t)
	if rows := registryRows(t); len(rows) != 0 {
		t.Errorf("a launched session registered %+v, want nothing", rows)
	}
}

func TestForgetHidesARootUntilItRegistersAgain(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := fixture(t, "")
	outsideTempDir(t)
	root := s.cfg.Root
	t.Chdir(root)
	runStatus(t)
	t.Chdir(t.TempDir())

	resetProjectsFlags(t)
	if err := execute(t, "projects", "--forget", root); err != nil {
		t.Fatalf("projects --forget: %v", err)
	}
	rows := registryRows(t)
	if len(rows) != 2 || !rows[1].Forgotten || rows[1].Root != root || rows[1].Registered == "" {
		t.Fatalf("--forget left %+v, want the registration and a dated tombstone after it", rows)
	}
	if got := projectsJSONRows(t); len(got) != 0 {
		t.Errorf("projects lists %+v after --forget, want nothing", got)
	}
	if got := projectsJSONRows(t, "--all"); len(got) != 1 || !got[0].Forgotten {
		t.Errorf("projects --all lists %+v, want the root marked forgotten", got)
	}
	var all AllProjects
	if err := json.Unmarshal([]byte(runStatus(t, "--all-projects", "--json")), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Projects) != 0 || all.Skipped != 0 {
		t.Errorf("status --all-projects reads %+v after --forget, want no project and nothing skipped", all)
	}

	t.Chdir(root)
	runStatus(t)
	if got := projectsJSONRows(t); len(got) != 1 || got[0].Root != root || got[0].Forgotten {
		t.Errorf("projects lists %+v after the project ran again, want it back", got)
	}
}

func TestForgetARootTheRegistryNeverNamedIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	resetProjectsFlags(t)
	err := execute(t, "projects", "--forget", "/nowhere/at/all")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "/nowhere/at/all") {
		t.Errorf("--forget of an unknown root: exit %d (%v), want %d naming it", Code(err), err, ExitRefused)
	}
	if rows := registryRows(t); len(rows) != 0 {
		t.Errorf("a refused --forget wrote %+v", rows)
	}
}

func TestStatusAllProjectsLeavesOutRootsWithNoConfig(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	live := fixture(t, "")
	outsideTempDir(t)
	for name, root := range map[string]string{"real": live.cfg.Root, "bare": t.TempDir(), "gone": "/nowhere/gone"} {
		if err := projects.Append(reg, projects.Row{Project: name, Root: root}); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(t.TempDir())

	table := runStatus(t, "--all-projects")
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "real ") {
		t.Fatalf("the table is not a header, one project row and a footer:\n%s", table)
	}
	for _, want := range []string{"2 registered roots left out", "drover projects lists them", "drover projects --forget <root>"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("the footer lacks %q:\n%s", want, lines[2])
		}
	}
	if rows := strings.Join(lines[:2], "\n"); strings.Contains(rows, "bare") || strings.Contains(rows, "gone") || strings.Contains(table, "unreadable") {
		t.Errorf("the table shows a root with no config:\n%s", table)
	}

	var all AllProjects
	if err := json.Unmarshal([]byte(runStatus(t, "--all-projects", "--json")), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Projects) != 1 || all.Skipped != 2 {
		t.Errorf("--json reads %d projects and skipped %d, want 1 and 2", len(all.Projects), all.Skipped)
	}
}

// worktreeOf commits the fixture at root and adds a git worktree of it, which
// carries the committed .drover.toml with it.
func worktreeOf(t *testing.T, root string) string {
	t.Helper()
	git(t, root, "init", "-q")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "fixture")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", wt)
	return wt
}

func allProjectsJSON(t *testing.T) AllProjects {
	t.Helper()
	var all AllProjects
	out := runStatus(t, "--all-projects", "--json")
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatalf("status --all-projects --json did not parse: %v\n%s", err, out)
	}
	return all
}

// A git worktree of a registered project is that project's child: registration
// records the parent, the index shows the worktree on its parent's row and
// never as a row of its own, and drives --all reads the parent alone.
func TestAWorktreeRendersUnderItsParent(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	parent := fixture(t, "").cfg.Root
	wt := worktreeOf(t, parent)
	outsideTempDir(t)

	t.Chdir(parent)
	runStatus(t)
	t.Chdir(wt)
	runStatus(t)
	rows := registryRows(t)
	if len(rows) != 2 || rows[0].Parent != "" || rows[1].Parent != parent {
		t.Fatalf("the registry holds %+v, want the parent and then the worktree naming it", rows)
	}

	check := func(when string) {
		t.Helper()
		all := allProjectsJSON(t)
		if len(all.Projects) != 1 || all.Projects[0].Root != parent ||
			len(all.Projects[0].Worktrees) != 1 || all.Projects[0].Worktrees[0] != rows[1].Root {
			t.Errorf("%s: --json reads %+v, want the parent alone carrying the worktree", when, all.Projects)
		}
		table := runStatus(t, "--all-projects")
		lines := strings.Split(strings.TrimSpace(table), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[1], "example ") ||
			strings.TrimSpace(lines[2]) != "worktree "+rows[1].Root {
			t.Errorf("%s: the table is not a header, the parent and its worktree under it:\n%s", when, table)
		}
		if page := runStatus(t, "--all-projects", "--html"); !strings.Contains(page, "worktree "+rows[1].Root) ||
			strings.Count(page, ">example<") != 1 {
			t.Errorf("%s: the html index does not show the worktree on its parent's row alone:\n%s", when, page)
		}
		if md := runStatus(t, "--all-projects", "--template", "builtin:obsidian"); !strings.Contains(md, "| worktree ") ||
			strings.Count(md, "| example |") != 1 {
			t.Errorf("%s: the Obsidian index does not show the worktree on its parent's row alone:\n%s", when, md)
		}
		if ds := drivesAllJSON(t); len(ds) != 1 || ds[0].Root != parent {
			t.Errorf("%s: drives --all reads %+v, want the parent alone", when, ds)
		}
	}
	check("recorded at registration")

	// A registry written before rows carried a parent, the worktree first.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := projects.Path(config.GlobalPath())
	for _, r := range []projects.Row{{Project: "example", Root: rows[1].Root}, {Project: "example", Root: parent}} {
		if err := projects.Append(reg, r); err != nil {
			t.Fatal(err)
		}
	}
	check("found at read time")
}

// A loadable project under the temp directory registers and stays listed by
// projects, but the index counts it as scratch rather than giving it a row.
func TestAProjectUnderTheTempDirIsCountedAsScratch(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	live := fixture(t, "").cfg.Root
	raw, err := os.ReadFile(filepath.Join(live, config.FileName)) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	tmp := outsideTempDir(t)
	reg := projects.Path(config.GlobalPath())
	if err := projects.Append(reg, projects.Row{Project: "real", Root: live}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repro1", "repro2"} {
		dir := filepath.Join(tmp, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "go.mod"), "module x\n")
		write(t, filepath.Join(dir, config.FileName), string(raw))
		if err := projects.Append(reg, projects.Row{Project: name, Root: dir}); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(live)

	table := runStatus(t, "--all-projects")
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "real ") || strings.Contains(table, "repro") {
		t.Fatalf("the table is not a header, the one project outside the temp dir and a footer:\n%s", table)
	}
	if want := "2 scratch projects under " + filepath.Clean(tmp); !strings.Contains(lines[2], want) ||
		strings.Contains(lines[2], "registered root") || !strings.Contains(lines[2], "drover projects lists them") {
		t.Errorf("the footer lacks %q, or counts the scratch as gone:\n%s", want, lines[2])
	}
	if all := allProjectsJSON(t); len(all.Projects) != 1 || all.Scratch != 2 || all.Skipped != 0 || all.TempDir != filepath.Clean(tmp) {
		t.Errorf("--json reads %d projects, scratch %d under %q, skipped %d; want 1, 2 under %s, 0",
			len(all.Projects), all.Scratch, all.TempDir, all.Skipped, tmp)
	}
	if got := projectsJSONRows(t); len(got) != 3 {
		t.Errorf("projects lists %+v, want the scratch projects too", got)
	}
}

// The table header names the step as the JSON beside it does, and the sample
// in docs/automation.md carries the header the printer writes.
func TestProjectsTableHeaderSaysFinished(t *testing.T) {
	var b strings.Builder
	printProjects(&b, []ProjectStatus{{Project: "webapp"}}, "registry")
	header := strings.Fields(strings.SplitN(b.String(), "\n", 2)[0])
	got := strings.Join(header, " ")
	if !strings.Contains(got, "FINISHED") || strings.Contains(got, "LAND") {
		t.Errorf("header %q, want FINISHED and no LAND", got)
	}
	doc, err := os.ReadFile("../../docs/automation.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "```\nPROJECT ")
	if !ok {
		t.Fatal("docs/automation.md has no status --all-projects sample")
	}
	sample := strings.Join(strings.Fields("PROJECT "+strings.SplitN(after, "\n", 2)[0]), " ")
	if sample != got {
		t.Errorf("docs/automation.md sample header %q, printer writes %q", sample, got)
	}
}
