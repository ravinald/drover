// Package testenv moves a test binary out of reach of a session marker.
//
// runner.Launched reads the nearest .drover/session above the working
// directory, and go test starts every package in its own source directory. A
// suite run from inside a drive's worktree, which is where an agent runs the
// project's own verify command, therefore reads the marker of the session
// running it, and every command the seal covers refuses under every test that
// did not set the actor on purpose. Clearing runner.SessionEnv does not reach a
// file.
package testenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// markerDir is runner.MarkerDir and configFile is config.FileName. Spelled out
// because runner's own TestMain calls Isolate, and runner's tests cannot import
// a package that imports runner.
const (
	markerDir  = ".drover"
	configFile = ".drover.toml"
)

// Mirror is the package directory rebuilt under a temporary root: the same
// path below the module root, every entry a symlink to the original except the
// directories on the way down, which are real so that a relative ../../docs
// resolves inside the mirror. The marker directory and the project config are
// left out: the first is what the mirror exists to escape, and the second would
// let a command run from the mirror resolve this repository's own project
// through a symlink, past every guard that refuses the real path.
type Mirror struct {
	// Dir is the package's directory inside the mirror, the working directory
	// once Isolate returns.
	Dir string
	// Source is the package directory go test started in.
	Source string
	root   string
}

// Isolate builds the mirror and changes into it. Call it from TestMain before
// m.Run, and Close after.
func Isolate() (*Mirror, error) {
	src, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	module, ok := moduleOf(src)
	if !ok {
		// No module above: this binary was re-run as a helper from a directory a
		// test chose, not started by go test in its package. That directory is
		// the helper's to keep, so only the marker check applies.
		m := &Mirror{Dir: src, Source: src}
		return m, m.unsealed()
	}
	rel, err := filepath.Rel(module, src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "drover-wd")
	if err != nil {
		return nil, err
	}
	m := &Mirror{Source: src, root: tmp}
	// The real path, so that a test comparing os.Getwd against a path it
	// built from TMPDIR does not trip over /var against /private/var.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		m.root = resolved
	}
	down := []string{}
	if rel != "." {
		down = strings.Split(rel, string(filepath.Separator))
	}
	from, into := module, m.root
	for i := 0; ; i++ {
		next := ""
		if i < len(down) {
			next = down[i]
		}
		if err := link(from, into, next); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if next == "" {
			break
		}
		from, into = filepath.Join(from, next), filepath.Join(into, next)
	}
	m.Dir = into
	if err := os.Chdir(m.Dir); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if err := m.unsealed(); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

// unsealed refuses a working directory with a session marker anywhere above it.
func (m *Mirror) unsealed() error {
	for d := m.Dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, markerDir, "session")); err == nil {
			return fmt.Errorf("a session marker in %s is above the test's working directory; point TMPDIR outside that tree", d)
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// Close returns to the package directory and removes the mirror. The symlinks
// go, never what they point at.
func (m *Mirror) Close() error {
	if m.root == "" {
		return nil
	}
	return errors.Join(os.Chdir(m.Source), os.RemoveAll(m.root))
}

// link fills into with a symlink per entry of from, except the two it leaves
// out and the one named next, which becomes a real directory to descend into.
func link(from, into, next string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case markerDir, configFile:
			continue
		case next:
			if err := os.Mkdir(filepath.Join(into, next), 0o750); err != nil {
				return err
			}
		default:
			if err := os.Symlink(filepath.Join(from, e.Name()), filepath.Join(into, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func moduleOf(dir string) (string, bool) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}
