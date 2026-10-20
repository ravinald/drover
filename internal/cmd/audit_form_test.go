package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prescribedAudit matches a rendered `drover audit` whose --code-by value is a
// placeholder: the form handed to an operator to fill in, as against an example
// with the arguments already chosen. It stops at the first quote or backtick,
// which is where every copy puts the note.
var prescribedAudit = regexp.MustCompile("drover audit [^\"`\n]*--code-by <[^\"`\n]*")

// Every copy of the audit form is checked here because a copy checked by hand
// drifts. The reviewer flag is how a human reviewer is recorded at all, and
// the prompt finish prints when nothing has audited the item is the one a human reads,
// so a copy that omits it tells the reader the flag does not exist.
//
// Examples with concrete arguments are exempt: `drover audit B1 reviewed
// --code-by agent "…"` is showing one invocation, not the shape to fill in.
func TestEveryPrescribedAuditFormOffersReviewer(t *testing.T) {
	root := repoRoot(t)
	targets := []string{
		filepath.Join(root, "internal"),
	}
	seen := 0
	for _, target := range targets {
		err := filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(path) {
			case ".go", ".tmpl", ".md":
			default:
				return nil
			}
			if strings.HasSuffix(path, "audit_form_test.go") {
				return nil
			}
			data, err := os.ReadFile(path) // #nosec G304 -- a path walked from the repo root
			if err != nil {
				return err
			}
			for _, form := range prescribedAudit.FindAllString(string(data), -1) {
				seen++
				if !strings.Contains(form, "--reviewer") {
					t.Errorf("%s renders the audit form without --reviewer:\n  %s",
						rel(root, path), strings.TrimSpace(form))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", target, err)
		}
	}
	// A regex that matched nothing would pass this test while every copy drifted.
	if seen < 4 {
		t.Fatalf("found %d prescribed audit forms, want at least the 4 known copies —"+
			" the pattern stopped matching and this test is now vacuous", seen)
	}
}

// repoRoot walks up from the package directory to the module root, so the test
// reads the same tree regardless of what a sibling test chdir'd into.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
