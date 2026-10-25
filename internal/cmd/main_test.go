package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
)

// schemaLine opens every fixture config in this package. Rendered from the
// constant so that bumping the schema does not invalidate every fixture here
// at once, which is what the bump to 2 did.
var schemaLine = fmt.Sprintf("schema_version = %d\n", config.SchemaVersion)

// Load layers a per-user config under every project, so a test run on a
// machine that has one reads settings no test wrote. Pointing XDG_CONFIG_HOME
// at an empty directory for the whole package makes the absence of a global
// config the default, and a test that wants one writes it there deliberately.
//
// DROVER_ACTOR is cleared for the same reason. `drover run` exports it into the
// session it launches, so a suite started from inside one drives every command
// the actor gate seals against a value no test wrote, and reports red on the one
// command every item tells an agent to trust. Clearing it here leaves
// actor_test.go free to set it deliberately: t.Setenv layers over the process
// environment and restores it, so the gate is still exercised.
//
// COLUMNS is the same defect in a second variable: titleWidth sizes the table
// to it, so a suite run from a 40-column terminal truncates every title and
// fails the assertions that read one. status_test.go sets it on purpose.
//
// XDG_CACHE_HOME is the same defect a third time, and the one with teeth.
// LogDir resolves to $XDG_CACHE_HOME/drover/<project>, keyed on the project
// name alone, so every fixture calling itself "fixture" shared one log
// directory with every other and with whatever the developer's own runs left
// there. runlog.Ran reads that directory to decide `ran` against `ready`, so a
// test writing a log could move another test's answer and a status golden with
// it.
func TestMain(m *testing.M) {
	// A test that needs drover as a process of its own, one an exec can
	// replace, runs this binary as it. Before anything below, which would
	// point the child at directories the parent test is not using.
	if os.Getenv(envAsDrover) != "" {
		if err := Execute(); err != nil {
			fmt.Fprintln(os.Stderr, "drover:", err)
			os.Exit(Code(err))
		}
		os.Exit(0)
	}
	flag.Parse()
	sweep := startSweep()
	for _, name := range clearedEnv {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	dir, err := os.MkdirTemp("", "drover-xdg")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CACHE_HOME", dir); err != nil {
		panic(err)
	}
	// XDG_STATE_HOME holds drive's worktrees, which a suite must never leave in
	// the developer's own state directory.
	if err := os.Setenv("XDG_STATE_HOME", dir); err != nil {
		panic(err)
	}
	// drive reads Claude Code's transcripts before and after every run, and a
	// suite must never read the developer's.
	if err := os.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude")); err != nil {
		panic(err)
	}
	stubCache = filepath.Join(dir, "stubs")
	if err := os.Mkdir(stubCache, 0o700); err != nil {
		panic(err)
	}
	code := m.Run()
	sweep.stop()
	if err := os.RemoveAll(dir); err != nil {
		panic(err)
	}
	os.Exit(code)
}

// stubCache holds one file per distinct stub body, named by the body's SHA-256,
// for the life of the test binary. macOS runs a malware check the first time
// any newly written executable runs, about 286 ms a file, so a suite writing a
// fresh stub for every test ran 6-11 times slower on a Mac than on Linux. A file
// that has already run once is not checked again.
var stubCache string

// stub makes path an executable that runs body: a symlink to the one cached copy
// of that body. A value one test varies (a log path, an exit code, a canned
// response) therefore belongs in the environment, set with t.Setenv, rather than
// in the body, or every test pays the check for a file of its own.
//
// The cached file is read-only and an existing path is replaced rather than
// written through, because every test running the same body shares that file: a
// write landing on it would change what the next test's stub does.
func stub(t *testing.T, path, body string) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	cached := filepath.Join(stubCache, hex.EncodeToString(sum[:]))
	if _, err := os.Stat(cached); errors.Is(err, fs.ErrNotExist) {
		tmp := cached + ".tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o500); err != nil { // #nosec G306 -- a stub has to be executable
			t.Fatal(err)
		}
		if err := os.Rename(tmp, cached); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(cached, path); err != nil {
		t.Fatal(err)
	}
}

// Two tests stubbing the same behavior share one file on disk, which is the
// whole saving: a second regular file would be a second malware check.
func TestOneStubBodyIsOneFile(t *testing.T) {
	const body = "#!/bin/sh\nexit 0\n"
	paths := []string{filepath.Join(t.TempDir(), "gh"), filepath.Join(t.TempDir(), "gh")}
	for _, p := range paths {
		stub(t, p, body)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s is %v, want a symlink into the stub cache", p, fi.Mode())
		}
	}
	a, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if !a.Mode().IsRegular() || !os.SameFile(a, b) {
		t.Errorf("one body stubbed at %s and %s resolved to two files, or not to a regular one", paths[0], paths[1])
	}
	other := filepath.Join(t.TempDir(), "gh")
	stub(t, other, "#!/bin/sh\nexit 1\n")
	c, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, c) {
		t.Error("two different bodies resolved to one file")
	}
}

// A stub that reads its exit code from the environment exits with that code,
// and every code runs the same file: the code is the test's, the behavior the
// stub's.
func TestAStubExitsWithTheCodeItsEnvironmentCarries(t *testing.T) {
	var first os.FileInfo
	for _, code := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			gate := gateScript(t, t.TempDir(), code)
			err := exec.Command(gate, "b1", "main").Run() // #nosec G204 -- the stub just written
			got := 0
			if exit, ok := errors.AsType[*exec.ExitError](err); ok {
				got = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != code {
				t.Errorf("the merge gate stub exited %d, want %d", got, code)
			}
			fi, err := os.Stat(gate)
			if err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = fi
			} else if !os.SameFile(first, fi) {
				t.Errorf("exit code %d wrote a stub of its own", code)
			}
		})
	}
}

// clearedEnv is the list TestMain unsets. Every name got here the same way: a
// variable the ambient shell sets, which drover reads, which drove the suite red
// on a machine nobody could reproduce from CI. runner.SessionEnv is the part a
// launched session exports, kept there so a variable Run gains reaches this list
// without anyone remembering to add it.
var clearedEnv = append(slices.Clone(runner.SessionEnv), "COLUMNS")

// ambient is what the pin sets in the child, one value per name that changes an
// answer if it survives: the actor gate seals six commands against DROVER_ACTOR,
// and 40 columns truncates every title the status assertions read.
//
// It is written out separately from clearedEnv on purpose. A pin driven off the
// same list it guards deletes itself along with the clear: drop a name from one
// list and the child is never told to set it, so the suite stays green. Two
// lists disagreeing is the failure this test is here to report, in both
// directions: a name cleared but unpinned, and a name pinned but no longer
// cleared.
var ambient = map[string]string{
	runner.EnvActor:   runner.ActorAgent,
	runner.EnvRoot:    "/nonexistent",
	runner.EnvSession: "ambient-session",
	"COLUMNS":         "40",
}

// envRelaunch marks the child process the pin starts, so it runs the assertion
// instead of forking again.
const envRelaunch = "DROVER_TEST_RELAUNCHED"

// TestMain clears clearedEnv before any test body runs, which means no test in
// this package can catch that clear going missing: the names are already gone
// either way by the time an assertion executes. Re-running this binary with each
// of them set is the only place a deletion shows up as red, and without it the
// fix is one refactor from returning — which is how COLUMNS came to be cleared
// only by inspection while DROVER_ACTOR was pinned.
func TestTheAmbientEnvironmentNeverReachesATest(t *testing.T) {
	if os.Getenv(envRelaunch) != "" {
		for name := range ambient {
			if got := os.Getenv(name); got != "" {
				t.Errorf("%s = %q inside the suite: TestMain no longer clears it", name, got)
			}
		}
		return
	}
	for _, name := range clearedEnv {
		if _, ok := ambient[name]; !ok {
			t.Errorf("TestMain clears %s and nothing pins it: give ambient a value that would change an answer", name)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), envRelaunch+"=1")
	for name, value := range ambient {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with %v set in the environment: %v\n%s", ambient, err, out)
	}
}
