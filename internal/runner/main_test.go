package runner

import (
	"os"
	"testing"
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
func TestMain(m *testing.M) {
	for _, name := range SessionEnv {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
