package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// olderConfig is a project file as a schema_version 1 project wrote it, under
// the key names version 2 replaced. It declares no open table, so each case below adds the one
// it is about without colliding with a declaration already there.
const olderConfig = `schema_version = 1
project = "widget"

[paths]
bodies = ".drover/bodies"
ledger = ".drover/ledger.jsonl"

[github]
caveat_label = "follow-up"
no_tracker = true

[frontmatter]
required = ["title", "deps"]
optional = ["severity"]
`

// currentConfig is the same project written against this build, which Load
// accepts, so the answer Load gives can be held against the one Inspect gives.
const currentConfig = `schema_version = 2
project = "widget"

[paths]
backlog = ".drover/backlog"
ledger = ".drover/ledger.jsonl"

[register]
backend = "none"

[frontmatter]
required = ["title", "deps"]
optional = ["severity"]
`

// perUser is a per-user file carrying a key a rename retired and an empty
// [verify]: the two layouts that a printed config reported as the project's
// choice and closed as a gap.
const perUser = `schema_version = 2

[github]
caveat_label = "legacy"

[verify]
`

// entry is one name written inside each open table, with a value that table
// decodes. A table without a line here fails TestEveryOpenTableHasACase, so an
// open table added to Config cannot go uncovered.
var entry = map[string]struct{ name, value string }{
	"verify":              {"docs", `"make docs"`},
	"runners":             {"agy", `{ headless = ["agy"] }`},
	"ids.namespaces":      {"B", `"bug"`},
	"severity.ranks":      {"C", `"red"`},
	"frontmatter.shape":   {"severity", `"enum:C,H"`},
	"numbered_files":      {"migrations", `"db/{n}_*.up.sql"`},
	"supervise":           {"resolve_conflict", `"operator"`},
	"notify.destinations": {"desk", `{ type = "command", argv = ["notify-send"], events = { exit = "low" } }`},
}

// declaration is one way a file writes an open table. top is placed before
// every header, where a bare or dotted key belongs to the top level; tail is
// appended after the rest of the file.
type declaration struct {
	form      string
	top, tail string
	want      TableState
	entries   []string
}

func declarations(table string) []declaration {
	e := entry[table]
	named := []string{Join(table, e.name)}
	return []declaration{
		{form: "absent", want: TableAbsent},
		{form: "empty header", tail: fmt.Sprintf("\n[%s]\n", table), want: TableEmpty},
		{form: "empty at top level", top: table + " = {}\n", want: TableEmpty},
		{form: "partial header", tail: fmt.Sprintf("\n[%s]\n%s = %s\n", table, e.name, e.value), want: TablePartial, entries: named},
		{form: "partial at top level", top: fmt.Sprintf("%s.%s = %s\n", table, e.name, e.value), want: TablePartial, entries: named},
	}
}

func TestEveryOpenTableHasACase(t *testing.T) {
	var cased []string
	for k := range entry {
		cased = append(cased, k)
	}
	open := OpenTables()
	sort.Strings(cased)
	sort.Strings(open)
	if !slices.Equal(cased, open) {
		t.Fatalf("OpenTables() is %v and the cases cover %v", open, cased)
	}
}

// One rule for every open table, asked of an older project file layered over a
// per-user file, both on disk. The per-user [verify] is empty, so the
// inherited answer for gates is a decision the project's absence leaves in
// force, and every other inherited table is absent.
func TestOpenTableStatesInAnOlderConfig(t *testing.T) {
	withGlobal(t, perUser)
	global := GlobalPath()
	for _, table := range OpenTables() {
		for _, d := range declarations(table) {
			t.Run(table+"/"+d.form, func(t *testing.T) {
				root := project(t, d.top+olderConfig+d.tail)
				in, err := Inspect(root)
				if err != nil {
					t.Fatalf("inspect: %v", err)
				}
				got, open := in.Config.OpenTable(table)
				if !open {
					t.Fatalf("%s is not an open table", table)
				}
				if got.State != d.want || !slices.Equal(got.Entries, d.entries) {
					t.Errorf("project answer is %s %v, want %s %v", got.State, got.Entries, d.want, d.entries)
				}
				if got.Path != in.Path {
					t.Errorf("project answer names %q, want %q", got.Path, in.Path)
				}
				if wantScope, _ := splitLast(table); got.Scope != wantScope {
					t.Errorf("scope is %q, want %q", got.Scope, wantScope)
				}

				inherited, _ := in.Config.InheritedTable(table)
				wantInherited := TableAbsent
				if table == "verify" {
					wantInherited = TableEmpty
				}
				if inherited.State != wantInherited || inherited.Path != global {
					t.Errorf("inherited answer is %s from %q, want %s from %q", inherited.State, inherited.Path, wantInherited, global)
				}

				// An empty table is a value the project wrote; a partial one's
				// value is its entries; an absent one wrote nothing.
				if slices.Contains(in.Values, table) != (d.want == TableEmpty) {
					t.Errorf("Values %v, table listed should be %v", in.Values, d.want == TableEmpty)
				}
				// Missing only when neither file said anything.
				missing := slices.ContainsFunc(in.Missing(), func(k Key) bool { return k.Dotted == table })
				if want := d.want == TableAbsent && wantInherited == TableAbsent; missing != want {
					t.Errorf("Missing lists %s: %v, want %v", table, missing, want)
				}
			})
		}
	}
}

// The answer is enough to write the declaration back at the scope it had. What
// a renderer reads off OpenTable and Written, written inside [Scope], reloads to
// the same answer.
func TestOpenTableReconstructsAtItsScope(t *testing.T) {
	for _, table := range OpenTables() {
		for _, d := range declarations(table) {
			if d.want == TableAbsent {
				continue
			}
			t.Run(table+"/"+d.form, func(t *testing.T) {
				in, err := Inspect(project(t, d.top+olderConfig+d.tail))
				if err != nil {
					t.Fatalf("inspect: %v", err)
				}
				before, _ := in.Config.OpenTable(table)
				w := in.Written()
				parts := make([]string, 0, len(before.Entries))
				for _, e := range before.Entries {
					_, name := splitLast(e)
					parts = append(parts, name+" = "+w[e])
				}
				line := before.Name + " = " + inline(parts) + "\n"
				// TOML refuses a second header for a table, so a scope the file
				// already opens takes the line under its existing header.
				header := fmt.Sprintf("\n[%s]\n", before.Scope)
				text := olderConfig + header + line
				switch {
				case before.Scope == "":
					text = line + olderConfig
				case strings.Contains(olderConfig, header):
					text = strings.Replace(olderConfig, header, header+line, 1)
				}

				again, err := Inspect(project(t, text))
				if err != nil {
					t.Fatalf("reinspect %q: %v", text, err)
				}
				after, _ := again.Config.OpenTable(table)
				after.Path = before.Path
				if !reflect.DeepEqual(before, after) {
					t.Errorf("written back as\n%s\nreads %+v, was %+v", text, after, before)
				}
			})
		}
	}
}

// Load and Inspect ask the same function, so a check reading a loaded config
// and a renderer reading an inspected one cannot disagree about a table.
func TestLoadAndInspectAgreeOnOpenTables(t *testing.T) {
	withGlobal(t, "schema_version = 2\n\n[verify]\n")
	for _, table := range OpenTables() {
		for _, d := range declarations(table) {
			t.Run(table+"/"+d.form, func(t *testing.T) {
				root := project(t, d.top+currentConfig+d.tail)
				loaded, err := Load(root)
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				in, err := Inspect(root)
				if err != nil {
					t.Fatalf("inspect: %v", err)
				}
				for _, ask := range []func(*Config, string) (OpenTable, bool){(*Config).OpenTable, (*Config).InheritedTable} {
					a, _ := ask(loaded, table)
					b, _ := ask(in.Config, table)
					if !reflect.DeepEqual(a, b) {
						t.Errorf("Load answers %+v, Inspect answers %+v", a, b)
					}
				}
			})
		}
	}
}

func TestOpenTableRefusesAKeyThatIsNotOne(t *testing.T) {
	in, err := Inspect(project(t, olderConfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"paths.ledger", "verify.docs", "ids"} {
		if _, open := in.Config.OpenTable(key); open {
			t.Errorf("%s answered as an open table", key)
		}
	}
}

// schema_version is the project's own fact. Each case is a pair of files on
// disk, and each asks the three questions a report about the version needs.
func TestVersionIsTheProjectsOwn(t *testing.T) {
	cases := []struct {
		name, global, project string
		declared, stale       bool
		version               int
		fromGlobal            bool
	}{
		{name: "global current, project absent", global: "schema_version = 2\n", project: `project = "widget"` + "\n",
			stale: true},
		{name: "global stale, project absent", global: "schema_version = 1\n", project: `project = "widget"` + "\n",
			stale: true, version: 1, fromGlobal: true},
		{name: "project explicit zero", global: "schema_version = 2\n", project: "schema_version = 0\nproject = \"widget\"\n",
			declared: true, stale: true},
		{name: "project from a later build", global: "schema_version = 2\n", project: "schema_version = 99\nproject = \"widget\"\n",
			declared: true, stale: true, version: 99},
		{name: "project current over a stale global", global: "schema_version = 1\n", project: "schema_version = 2\nproject = \"widget\"\n",
			declared: true, version: 2},
		{name: "older project file, no global", project: olderConfig,
			declared: true, stale: true, version: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.global != "" {
				withGlobal(t, tc.global)
			}
			in, err := Inspect(project(t, tc.project))
			if err != nil {
				t.Fatal(err)
			}
			if in.VersionDeclared() != tc.declared {
				t.Errorf("VersionDeclared = %v, want %v", in.VersionDeclared(), tc.declared)
			}
			if in.Stale() != tc.stale {
				t.Errorf("Stale = %v, want %v", in.Stale(), tc.stale)
			}
			path, version := in.VersionSource()
			wantPath := in.Path
			if tc.fromGlobal {
				wantPath = GlobalPath()
			}
			if path != wantPath || version != tc.version {
				t.Errorf("VersionSource = %q %d, want %q %d", path, version, wantPath, tc.version)
			}
		})
	}
}

// Load's refusal of an inherited version names the file that declared it, and
// stays a VersionError so a caller can tell it from every other load failure.
func TestLoadNamesTheFileThatDeclaredTheVersion(t *testing.T) {
	withGlobal(t, "schema_version = 1\n")
	_, err := Load(project(t, `project = "widget"`+"\n"))
	var refused *VersionError
	if !errors.As(err, &refused) {
		t.Fatalf("want a VersionError, got %v", err)
	}
	if refused.Path != GlobalPath() || !strings.HasPrefix(err.Error(), GlobalPath()+":") {
		t.Errorf("refusal names %q: %v", refused.Path, err)
	}

	// A per-user file that fails for its own reason is not a version gap.
	withGlobal(t, "schema_version = 2\nproject = \"everyone\"\n")
	_, err = Load(project(t, `project = "widget"`+"\n"))
	if err == nil || errors.As(err, &refused) {
		t.Errorf("a per-user file declaring project reads as a version gap: %v", err)
	}
}

// Inspect names the file that would not parse, per-user or project.
func TestInspectNamesTheFileThatFailed(t *testing.T) {
	withGlobal(t, "[verify\n")
	_, err := Inspect(project(t, olderConfig))
	if err == nil || !strings.Contains(err.Error(), GlobalPath()) {
		t.Errorf("a broken per-user file reported as %v", err)
	}
	withGlobal(t, perUser)
	root := project(t, olderConfig+"\n[verify\n")
	_, err = Inspect(root)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(root, FileName)) {
		t.Errorf("a broken project file reported as %v", err)
	}
}

// A retired key carries the file that wrote it, so a per-user key is never
// read back as the project's choice.
func TestRetiredKeysNameTheirFile(t *testing.T) {
	withGlobal(t, perUser)
	in, err := Inspect(project(t, olderConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"github.caveat_label/" + GlobalPath(): "per-user",
		"github.caveat_label/" + in.Path:      "project",
		"paths.bodies/" + in.Path:             "project",
		"github.no_tracker/" + in.Path:        "project",
	}
	got := map[string]bool{}
	for _, r := range in.Retired {
		got[r.Key+"/"+r.Path] = true
	}
	for k, who := range want {
		if !got[k] {
			t.Errorf("%s retired key %s not reported; got %v", who, k, in.Retired)
		}
	}
	if len(in.Retired) != len(want) {
		t.Errorf("retired %v, want exactly %d", in.Retired, len(want))
	}
	// Written is the project's alone, so the per-user value is not in it.
	if in.Written()["github.caveat_label"] == `"legacy"` {
		t.Error("the per-user caveat_label reads as the project's")
	}
}

// Declared counts a table as written when anything beneath it is, the way the
// two answers it replaced did: a project writing only [severity.ranks] has
// configured severity.
func TestDeclaredCountsWhatIsBeneathAKey(t *testing.T) {
	c, err := Load(project(t, currentConfig+"\n[severity.ranks]\nC = \"red\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.SeverityConfigured() {
		t.Error("[severity.ranks] did not count as configuring severity")
	}
	if c.RunnerKeyConfigured() {
		t.Error("runner_key reads as configured with no [run] block")
	}
	withGlobal(t, "schema_version = 2\n[run]\nrunner_key = \"owner\"\n")
	c, err = Load(project(t, currentConfig))
	if err != nil {
		t.Fatal(err)
	}
	if !c.RunnerKeyConfigured() || c.ProjectDeclared("run.runner_key") {
		t.Error("a per-user runner_key is declared for the project and never by it")
	}
}

// splitLast splits a dotted key whose segments need no quoting.
func splitLast(dotted string) (string, string) {
	if i := strings.LastIndex(dotted, "."); i >= 0 {
		return dotted[:i], dotted[i+1:]
	}
	return "", dotted
}
