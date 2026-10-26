package runner

import (
	"os"
	"testing"

	"github.com/ravinald/drover/internal/testenv"
)

// The suite clears SessionEnv for the whole package. The actor variable matters
// most here, because this is the one package whose production code refuses on it.
//
// Run returns ErrNested when EnvActor is set, which is correct and is exactly
// what a suite started from inside a launched session inherits. Every test that
// drives Run then fails on the refusal instead of on the behavior it asserts,
// and the failure reads as a broken actor gate rather than as a suite running
// somewhere it did not expect. CI never meets it; an agent running the
// project's own gate meets it every time.
//
// Tests that want the variable set still set it: t.Setenv layers over the
// process environment and restores it, so the nested-launch refusal keeps its
// own coverage.
//
// The marker is the same defect in a file, and isolated rather than cleared:
// the suite runs from a mirror of this directory under the temp dir, where no
// .drover/session sits above it. TestAMarkerAboveTheWorkingDirectorySeals is
// what shows the walk would have found one.
func TestMain(m *testing.M) {
	for _, name := range SessionEnv {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	wd, err := testenv.Isolate()
	if err != nil {
		panic(err)
	}
	code := m.Run()
	if err := wd.Close(); err != nil {
		panic(err)
	}
	os.Exit(code)
}
