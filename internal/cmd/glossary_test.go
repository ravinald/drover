package cmd

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ravinald/drover/internal/ledger"
)

const (
	glossaryDoc = "../../docs/glossary.md"
	schemaFile  = "../config/drover.schema.json"
)

// glossaryRows maps each `##` heading of docs/glossary.md to the terms in the
// first cell of its table rows. A cell can name several terms (`--pause`,
// `--resume`), so every backticked span counts, and so does the whole cell for
// a term written as a plain word.
func glossaryRows(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	var section map[string]bool
	for _, line := range strings.Split(read(t, glossaryDoc), "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			section = map[string]bool{}
			out[strings.TrimSpace(h)] = section
			continue
		}
		if section == nil || !strings.HasPrefix(line, "|") {
			continue
		}
		cell := strings.TrimSpace(strings.Split(strings.Trim(line, "|"), "|")[0])
		section[cell] = true
		for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
			section[m[1]] = true
		}
	}
	return out
}

// constantsByPrefix reads every string constant in this package whose name
// opens with one of the prefixes, from the source rather than a list kept here,
// so a constant added tomorrow is in the set the moment it compiles.
func constantsByPrefix(t *testing.T, prefixes ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	eachSource(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) || !hasAnyPrefix(name.Name, prefixes) {
					continue
				}
				if s, ok := stringLit(spec.Values[i]); ok {
					out[s] = true
				}
			}
			return true
		})
	})
	return out
}

// parkLiterals reads the park causes written inline rather than as constants:
// the first argument of park(...), a Park field set in a composite literal or an
// assignment, and the keys of parkActions.
func parkLiterals(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	eachSource(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "park" && len(n.Args) > 0 {
					if s, ok := stringLit(n.Args[0]); ok && s != "" {
						out[s] = true
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "Park" {
					if s, ok := stringLit(n.Value); ok {
						out[s] = true
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Park" || i >= len(n.Rhs) {
						continue
					}
					if s, ok := stringLit(n.Rhs[i]); ok && s != "" {
						out[s] = true
					}
				}
			}
			return true
		})
	})
	for k := range parkActions {
		out[k] = true
	}
	return out
}

func eachSource(t *testing.T, visit func(*ast.File)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		visit(f)
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func hasAnyPrefix(name string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool {
		rest, ok := strings.CutPrefix(name, p)
		return ok && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z'
	})
}

// commandNames is every command cobra registers under root, as typed after
// `drover`: `finish`, `follow-up open`. cobra adds help and completion on the
// first Execute, so they are added here the same way: without that, the set
// would depend on whether an earlier test in the run had executed a command.
func commandNames() map[string]bool {
	install()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	out := map[string]bool{}
	var walk func(prefix string, c *cobra.Command)
	walk = func(prefix string, c *cobra.Command) {
		for _, sub := range c.Commands() {
			name := strings.TrimSpace(prefix + " " + sub.Name())
			out[name] = true
			walk(name, sub)
		}
	}
	walk("", root)
	return out
}

// schemaSections is every top-level object in the config schema, as `[name]`.
func schemaSections(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", schemaFile, err)
	}
	out := map[string]bool{}
	for name, p := range s.Properties {
		if p.Type == "object" {
			out["["+name+"]"] = true
		}
	}
	return out
}

// Every name drover exposes has a row in docs/glossary.md, under the heading a
// reader would look for it in. The sets come from the code: cobra's command
// tree, the AST of this package, the supervise catalog and the schema. Adding a
// command, a state, a park cause, a check kind or a config section without a
// glossary row fails here.
//
// Maintenance, when this fails: add the row to the section the failure names.
// The test keeps no list of its own to update.
func TestGlossaryNamesEveryExposedTerm(t *testing.T) {
	actions := map[string]bool{}
	for _, a := range catalog {
		actions[a.key] = true
	}
	stages := constantsByPrefix(t, "stage")
	stages[ledger.StageImplement] = true
	stages[ledger.StageReview] = true

	want := []struct {
		section string
		what    string
		names   map[string]bool
	}{
		{"Commands", "command", commandNames()},
		{"Item states", "item state", constantsByPrefix(t, "state")},
		{"Park causes and details", "park cause", parkLiterals(t)},
		{"Park causes and details", "park cause", constantsByPrefix(t, "park")},
		{"Park causes and details", "park detail", constantsByPrefix(t, "detail")},
		{"Review results", "review result", constantsByPrefix(t, `pass`)},
		{"Drive outcomes", "drive outcome", constantsByPrefix(t, "outcome")},
		{"Stages and events", "stage", stages},
		{"Stages and events", "event", constantsByPrefix(t, "ev")},
		{"Update-branch results", "update-branch result", constantsByPrefix(t, "catchUp")},
		{"Check finding kinds", "check finding kind", kindsInCode(t)},
		{"Follow-ups and coverage values", "coverage value", constantsByPrefix(t, "coverage")},
		{"Supervise actions and policies", "supervise action", actions},
		{"Config sections and keys", "config section", schemaSections(t)},
	}

	// A review result that stops an item is also its park cause, and its row
	// sits with the review results rather than twice.
	alsoIn := map[string]string{"park cause": "Review results"}

	rows := glossaryRows(t)
	for _, w := range want {
		if len(w.names) == 0 {
			t.Errorf("found no %s in the code; the scan is broken, not the glossary", w.what)
			continue
		}
		section, ok := rows[w.section]
		if !ok {
			t.Errorf("%s has no ## %s section", glossaryDoc, w.section)
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(w.names)) {
			if !section[name] && !rows[alsoIn[w.what]][name] {
				t.Errorf("%s %q has no row under ## %s in %s", w.what, name, w.section, glossaryDoc)
			}
		}
	}
}
