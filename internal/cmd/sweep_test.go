package cmd

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
)

// envSweep marks the suite a sweep starts, so the copy of this package inside it
// does not start another.
const envSweep = "DROVER_TEST_SWEEP"

const sweepTest = "TestNoPackageWritesThroughAnInheritedRoot"

// sweep is every other package's suite running as a child, rooted at a scratch
// project through the variables a launched session exports.
//
// TestMain starts it, before it clears or repoints anything, so the child sees
// the environment a plain `go test` would, and so it runs alongside this
// package's serial tests rather than after them. Started from the test itself,
// on a host where this package takes five minutes, the two together crossed the
// default ten-minute timeout.
type sweep struct {
	root   string
	before map[string]string
	out    bytes.Buffer
	child  *exec.Cmd
	done   chan struct{}
	err    error
}

// running is the sweep TestMain started, nil when it started none.
var running *sweep

// sweepProject is the scratch project the child is rooted at: an item with a
// merged dependency, which is what a package following an inherited root into a
// live ledger would send back and audit.
var sweepProject = map[string]string{
	"go.mod": "module scratch\n",
	config.FileName: schemaLine + `project = "scratch"

[paths]
backlog     = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
register    = "register.jsonl"
root_marker = "go.mod"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "severity"]

[verify]
default = "true"

[git]
enabled = false
`,
	"work/B01-first.md":  "---\ntitle: First\ndeps:\n---\n\nRequirements:\n\n1. Build it.\n",
	"work/B02-second.md": "---\ntitle: Second\ndeps: B1\n---\n\nRequirements:\n\n1. Use it.\n",
	"ledger.jsonl":       `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}` + "\n",
	"register.jsonl":     "",
}

// startSweep starts the child when this run will execute sweepTest, and returns
// nil otherwise: under -short, inside a sweep, or when -run or -skip leaves the
// test out, so `go test -run TestFoo` does not pay for the whole module.
func startSweep() *sweep {
	if testing.Short() || os.Getenv(envSweep) != "" || !selected(sweepTest) {
		return nil
	}
	s := &sweep{done: make(chan struct{})}
	running = s
	if s.err = s.start(); s.err != nil {
		close(s.done)
	}
	return s
}

func (s *sweep) start() error {
	root, err := os.MkdirTemp("", "drover-sweep")
	if err != nil {
		return err
	}
	s.root = root
	if err := os.Mkdir(filepath.Join(root, "reports"), 0o750); err != nil {
		return err
	}
	for rel, body := range sweepProject {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
	}
	if s.before, err = tree(root); err != nil {
		return err
	}
	module, err := moduleRoot()
	if err != nil {
		return err
	}
	pkgs, err := sweptPackages(module)
	if err != nil {
		return err
	}
	// The child needs the deadline the caller chose for the parent, not go
	// test's default, because the parent waits on it.
	args := append([]string{"test", "-count=1", "-timeout=" + flag.Lookup("test.timeout").Value.String()}, pkgs...)
	s.child = exec.Command("go", args...)
	s.child.Dir = module
	s.child.Stdout, s.child.Stderr = &s.out, &s.out
	s.child.Env = append(os.Environ(),
		runner.EnvRoot+"="+root,
		runner.EnvActor+"="+runner.ActorAgent,
		envSweep+"=1",
	)
	if err := s.child.Start(); err != nil {
		return err
	}
	go func() {
		s.err = s.child.Wait()
		close(s.done)
	}()
	return nil
}

// sweptPackages is every package in the module except this one.
//
// This package is left out because it already has the guard the sweep exists
// to supply: TestTheAmbientEnvironmentNeverReachesATest reruns this binary
// with runner.SessionEnv set and fails if TestMain stops clearing it. Its copy
// in the child was also most of the child's cost: internal/cmd runs for about
// ten minutes, and two copies of it side by side under one deadline overrun
// that deadline on a host where either copy alone fits.
func sweptPackages(module string) ([]string, error) {
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = module
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w", err)
	}
	self := reflect.TypeFor[sweep]().PkgPath()
	var pkgs []string
	for _, pkg := range strings.Fields(string(out)) {
		if pkg != self {
			pkgs = append(pkgs, pkg)
		}
	}
	if len(pkgs) == 0 {
		return nil, errors.New("go list ./... named no package but this one")
	}
	return pkgs, nil
}

// stop ends a child the test never waited on, which -failfast or a panic in
// another test leaves running, and removes the scratch project.
func (s *sweep) stop() {
	if s == nil {
		return
	}
	select {
	case <-s.done:
	default:
		_ = s.child.Process.Kill()
		<-s.done
	}
	if s.root != "" {
		_ = os.RemoveAll(s.root)
	}
}

// A suite started inside a launched session inherits runner.SessionEnv, and a
// package that forgets to clear it reaches the operator's live project through
// DROVER_ROOT, and can append fabricated audits and changes rows to a real
// ledger that way on runs that all pass. A package's own TestMain cannot
// catch its own omission, so this runs every other package in the module rooted
// at a scratch project and fails on any byte that changed there.
func TestNoPackageWritesThroughAnInheritedRoot(t *testing.T) {
	if running == nil {
		t.Skip("no sweep: -short, or already inside one")
	}
	<-running.done
	if running.before == nil {
		t.Fatalf("setting up the sweep: %v", running.err)
	}
	if running.err != nil {
		t.Errorf("go test ./... under %s=%s: %v\n%s", runner.EnvRoot, running.root, running.err, running.out.String())
	}
	after, err := tree(running.root)
	if err != nil {
		t.Fatal(err)
	}
	for path, was := range running.before {
		now, ok := after[path]
		switch {
		case !ok:
			t.Errorf("%s was removed", path)
		case now != was:
			t.Errorf("%s changed:\n--- before\n%s--- after\n%s", path, was, now)
		}
	}
	for path := range after {
		if _, ok := running.before[path]; !ok {
			t.Errorf("%s was created", path)
		}
	}
}

// selected reports whether -run and -skip leave a top-level test named name in
// the run. Only the first element of each pattern applies to a top-level name.
func selected(name string) bool {
	matches := func(flagName string) (bool, bool) {
		f := flag.Lookup(flagName)
		if f == nil || f.Value.String() == "" {
			return false, false
		}
		first, _, _ := strings.Cut(f.Value.String(), "/")
		re, err := regexp.Compile(first)
		if err != nil {
			return false, true
		}
		return re.MatchString(name), true
	}
	if f := flag.Lookup("test.list"); f != nil && f.Value.String() != "" {
		return false
	}
	if ok, set := matches("test.run"); set && !ok {
		return false
	}
	if ok, set := matches("test.skip"); set && ok {
		return false
	}
	return true
}

// tree reads every file under dir, keyed by its slash-separated path relative
// to dir.
func tree(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path) // #nosec G304 -- walking the sweep's own temp dir
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(body)
		return err
	})
	return files, err
}

// moduleRoot walks up from the package directory to go.mod.
func moduleRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the package directory")
		}
		dir = parent
	}
}
