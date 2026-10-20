package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Keys is walked off the Config types, so a field added to Config reaches it
// without anybody editing a list. This asserts the walk actually does that: a
// filter that quietly stopped descending would leave Keys() reporting a subset
// while every caller treated it as the whole set, and the report an operator
// reads would be short by exactly the keys nobody thought about.
func TestKeysReachesEveryConfigField(t *testing.T) {
	found := map[string]bool{}
	for _, k := range Keys() {
		found[k.Dotted] = true
	}
	for _, dotted := range fields(reflect.TypeOf(Config{}), "") {
		if !found[dotted] {
			t.Errorf("Keys() never reports %q, which a Config field reads", dotted)
		}
	}
}

// fields is the walk this test judges by, written independently of walkKeys so
// that one filter cannot pass both.
func fields(t reflect.Type, prefix string) []string {
	var out []string
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		dotted := tag
		if prefix != "" {
			dotted = prefix + "." + tag
		}
		if t.Field(i).Type.Kind() == reflect.Struct {
			out = append(out, fields(t.Field(i).Type, dotted)...)
			continue
		}
		out = append(out, dotted)
	}
	return out
}

// Every reported line is meant to be pasted into a .drover.toml, so every one
// of them has to load. A default rendered in a form TOML rejects, or one whose
// key no field reads, sends an operator to break their own config on drover's
// advice.
func TestEveryDefaultPastesBackIn(t *testing.T) {
	// Top level first, which is not the order the types declare them in: gates
	// and runners are top-level maps sitting after several tables, and a bare
	// key written after a [table] header belongs to that header. Rendering in
	// declaration order would put `runners = {…}` inside [run], where it
	// decodes as run.runners.
	var top, nested []Key
	for _, k := range Keys() {
		if k.Table == "" {
			top = append(top, k)
			continue
		}
		nested = append(nested, k)
	}

	var b strings.Builder
	table := "\x00"
	for _, k := range append(top, nested...) {
		if k.Table != table {
			table = k.Table
			if table != "" {
				fmt.Fprintf(&b, "[%s]\n", table)
			}
		}
		fmt.Fprintf(&b, "%s = %s\n", k.Name, k.Default)
	}

	keys, err := KeysIn(b.String())
	if err != nil {
		t.Fatalf("the reported defaults do not parse as TOML: %v\n%s", err, b.String())
	}
	for _, k := range Keys() {
		if !slices.Contains(keys, k.Dotted) {
			t.Errorf("%q rendered as %q, which decodes to no key by that name", k.Dotted, k.Default)
		}
	}
}

// The defaults come from applyDefaults rather than a second copy of them, which
// is the whole reason this is a walk and not a table. A spot check on the four
// that matter: two filled in by applyDefaults, and two resolved where they are
// read, whose zero value in the struct is not what a paste has to carry.
func TestDefaultsAreTheOnesDroverActuallyResolves(t *testing.T) {
	want := map[string]string{
		"paths.backlog":    `".drover/backlog"`,
		"run.runner_key":   `"runner"`,
		"git.enabled":      "true",
		"register.backend": `"github"`,
	}
	got := map[string]string{}
	for _, k := range Keys() {
		got[k.Dotted] = k.Default
	}
	for dotted, value := range want {
		if got[dotted] != value {
			t.Errorf("%s default is %q, want %q", dotted, got[dotted], value)
		}
	}
}

// An open table's entries are names the project invented. Reporting one as a
// key this build reads would tell a project that declared a single namespace it
// had failed to declare the other two.
func TestOpenTablesAreReportedAsTablesAndNotAsEntries(t *testing.T) {
	for _, want := range []string{"verify", "runners", "ids.namespaces", "severity.ranks", "frontmatter.shape"} {
		if !slices.Contains(OpenTables(), want) {
			t.Errorf("OpenTables() omits %q, so its entries would be compared as settings", want)
		}
	}
	for _, k := range Keys() {
		if InOpenTable(k.Dotted) {
			t.Errorf("Keys() reports %q, which sits inside an open table", k.Dotted)
		}
	}
	if InOpenTable("verify") {
		t.Error("InOpenTable(\"verify\") is true: the table itself is a setting drover reads")
	}
	if !InOpenTable("verify.docs") {
		t.Error("InOpenTable(\"verify.docs\") is false: a flagged verify command is a name the project chose")
	}
}

// Leaves is what keeps a rendered block from declaring one path twice, which
// TOML rejects.
func TestLeavesDropsEveryKeyAnotherSitsUnder(t *testing.T) {
	got := Leaves([]string{"severity", "severity.ranks", "severity.ranks.C", "run.trunk"})
	want := []string{"run.trunk", "severity.ranks.C"}
	if !slices.Equal(got, want) {
		t.Errorf("Leaves() = %v, want %v", got, want)
	}
}
