package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guideSource writes a source tree holding one SKILL.md per named guide.
func guideSource(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n, "SKILL.md"), []byte("# "+n+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A project referencing one guide is the first sync every project runs.
func TestSyncSkillsCountAgreesWithWhatItCopied(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{[]string{"go-review"}, "vendored 1 guide from "},
		{[]string{"go-review", "shell-review", "sql-review"}, "vendored 3 guides from "},
	} {
		src := guideSource(t, c.names...)
		uses := make([]SkillUse, 0, len(c.names))
		for _, n := range c.names {
			uses = append(uses, SkillUse{Name: n, Refs: []string{"B1"}})
		}
		out := captureErr(t, func() {
			if err := syncSkills(uses, src, t.TempDir()); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, c.want+src+"\n") {
			t.Errorf("syncSkills of %d guides does not carry %q:\n%s", len(c.names), c.want, out)
		}
	}

	// Nothing referenced still reports, and zero takes the plural.
	out := captureErr(t, func() {
		if err := syncSkills([]SkillUse{{Name: "unused"}}, guideSource(t, "unused"), t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "vendored 0 guides from ") {
		t.Errorf("an empty sync does not report itself:\n%s", out)
	}
}

// skillsProject writes a project whose one item references a guide, which is
// what makes --from copy anything at all.
func skillsProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
skills = "guides"

[ids]
namespaces = { B = "bug" }

[verify]
default = "true"

[frontmatter]
optional = ["skills"]
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"work", "guides"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	item := "---\ntitle: Needs a guide\ndeps:\nskills: go-review\n---\n\nRequirements:\n\n1. Do it.\n"
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-needs-a-guide.md"), []byte(item), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

// --json is what a wrapper parses, so the whole stream has to be an array. A
// vendoring report printed ahead of it fails every consumer on the first byte.
// Unmarshalled whole rather than searched, because a substring assertion passes
// over that report.
func TestSkillsJSONWritesNothingButJSONWhileVendoring(t *testing.T) {
	skillsProject(t)
	src := guideSource(t, "go-review")
	t.Cleanup(func() { skillsJSON, skillsFrom = false, "" })

	var (
		err error
		out string
	)
	report := captureErr(t, func() {
		out = capture(t, func() { err = execute(t, "skills", "--json", "--from", src) })
	})
	if err != nil {
		t.Fatalf("skills --json --from: %v", err)
	}
	var got []SkillUse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON alone: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Name != "go-review" || got[0].State != "vendored" {
		t.Errorf("skills --json --from returned %+v, want one vendored go-review", got)
	}
	if !strings.Contains(report, "vendored 1 guide from "+src) {
		t.Errorf("stderr does not say what was vendored:\n%s", report)
	}
}

// vendorGuide plants a guide in the project's own {skills} directory, which is
// the state --prune exists for: on disk, referenced by nothing.
func vendorGuide(t *testing.T, project, name string) string {
	t.Helper()
	dir := filepath.Join(project, "guides", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --prune is the one flag in the tool that deletes from the work tree. Splitting
// the candidate list onto stdout and the count onto stderr would make a captured
// stdout record the plan and read identically to a run that never reached the
// removal.
func TestSkillsPruneDeletesAndReportsOnOneStream(t *testing.T) {
	dead := vendorGuide(t, skillsProject(t), "dead")
	t.Cleanup(func() { skillsPrune = false })

	var (
		err error
		out string
	)
	report := captureErr(t, func() {
		out = capture(t, func() { err = execute(t, "skills", "--prune") })
	})
	if err != nil {
		t.Fatalf("skills --prune: %v", err)
	}
	if _, statErr := os.Stat(dead); !os.IsNotExist(statErr) {
		t.Errorf("guides/dead survives --prune: %v", statErr)
	}
	if !strings.Contains(report, "pruned 1 vendored but unreferenced: dead\n") {
		t.Errorf("stderr does not record the removal:\n%s", report)
	}
	// The whole report, not half of it: a stream carrying the candidates and
	// not the outcome is the state this test exists to keep out.
	for _, half := range []string{"pruned", "vendored but unreferenced"} {
		if strings.Contains(out, half) {
			t.Errorf("stdout carries %q, so the prune report is split across both streams:\n%s", half, out)
		}
	}
}

// The removal runs above the --json return, so --json --prune deletes. This pins
// that ordering, and that stdout is still the array alone while it happens.
func TestSkillsJSONPruneDeletesAndLeavesStdoutParseable(t *testing.T) {
	dead := vendorGuide(t, skillsProject(t), "dead")
	t.Cleanup(func() { skillsPrune, skillsJSON = false, false })

	var (
		err error
		out string
	)
	report := captureErr(t, func() {
		out = capture(t, func() { err = execute(t, "skills", "--json", "--prune") })
	})
	if err != nil {
		t.Fatalf("skills --json --prune: %v", err)
	}
	if _, statErr := os.Stat(dead); !os.IsNotExist(statErr) {
		t.Errorf("guides/dead survives --json --prune: %v", statErr)
	}
	var got []SkillUse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON alone: %v\n%s", err, out)
	}
	// The array names what went, so the document records the deletion too.
	var found bool
	for _, u := range got {
		if u.Name == "dead" {
			found = u.State == "unreferenced"
		}
	}
	if !found {
		t.Errorf("skills --json --prune returned %+v, want dead listed unreferenced", got)
	}
	if !strings.Contains(report, "pruned 1 vendored but unreferenced: dead\n") {
		t.Errorf("stderr does not record the removal:\n%s", report)
	}
}
