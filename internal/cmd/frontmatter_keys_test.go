package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
)

// A [frontmatter] list is closed and authored per project, so a key drover reads
// that the generated defaults do not declare is a key no item may carry. Nothing
// refuses: the read returns empty, the command does less than it says, and the
// only sign is whichever check happens to look. A key like closes can sit in
// exactly that state and reach only the project that declared it by hand.
//
// The two directions below are separate failures with separate fixes. Above:
// drover reads a key its own defaults withhold. Below: the source reads a key
// backlog.ReadKeys never heard of, which would make the list here a second
// hand-kept copy — the defect these tests exist to prevent, not the fix.

// Both generated defaults must declare every key drover reads. init writes one
// project's config and init --user seeds the per-user file layered under all of them,
// so an omission in either reaches a project that has done nothing wrong.
func TestGeneratedDefaultsDeclareEveryKeyDroverReads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no per-user config: init writes the block itself
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n")
	t.Chdir(dir)
	initVerify = "true"
	t.Cleanup(func() { initVerify = "" })
	capture(t, func() {
		if err := execute(t, "init"); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	generated, err := config.Load(dir)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}

	seeded := loadsAsGlobal(t, renderGlobal(defaultAnswers()))

	for _, key := range backlog.ReadKeys() {
		if !generated.Known(key) {
			t.Errorf("drover init writes a config declaring no %q, so no item it generates can carry one", key)
		}
		if !seeded.Known(key) {
			t.Errorf("drover init --user seeds a per-user config declaring no %q, which every project on the machine inherits", key)
		}
	}
}

// The reverse: a literal read the registry does not name. Reading it from the
// source rather than from a list kept here is what stops the registry drifting
// behind the code the way the schema's shape pattern did.
func TestEveryFrontmatterKeyReadInSourceIsRegistered(t *testing.T) {
	known := backlog.ReadKeys()
	found := 0
	for _, dir := range []string{".", "../backlog"} {
		for key, pos := range literalKeyReads(t, dir) {
			found++
			if !slices.Contains(known, key) {
				t.Errorf("%s reads frontmatter key %q, which backlog.ReadKeys does not name:"+
					" add it there, or the generated defaults will not declare it", pos, key)
			}
		}
	}
	if found == 0 {
		t.Fatal("the scan found no literal key reads at all; it is broken, not the registry")
	}
}

// literalKeyReads collects the frontmatter keys a package names as string
// literals, through either accessor: it.Get("k") and it.Meta["k"].
//
// A non-literal argument is skipped rather than failed, which is the opposite of
// how kinds_test reads check's Kind literals. There the set has to be complete;
// here the two sanctioned ways to name a key are a literal and a backlog.Key
// constant, and a constant is registered by construction. What is left over is
// a variable holding a configurable key — [severity] key, [run] runner_key —
// whose name is a project's setting rather than anything this test could know.
func literalKeyReads(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".go" || len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			key, lit := "", (*ast.BasicLit)(nil)
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Get" || len(node.Args) != 1 {
					return true
				}
				lit, _ = node.Args[0].(*ast.BasicLit)
			case *ast.IndexExpr:
				sel, ok := node.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Meta" {
					return true
				}
				lit, _ = node.Index.(*ast.BasicLit)
			default:
				return true
			}
			if lit == nil || lit.Kind != token.STRING {
				return true
			}
			if key, err = strconv.Unquote(lit.Value); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			out[key] = fset.Position(lit.Pos()).String()
			return true
		})
	}
	return out
}
