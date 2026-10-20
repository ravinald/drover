package prompts_test

import (
	"os"
	"slices"
	"testing"

	"github.com/ravinald/drover/internal/runner"
)

// TestAFindingReachesTheSurfaceThatMustActOnIt runs the drover binary in
// subprocesses, and each one inherits this process's environment before stand
// narrows it. A suite started inside a launched session carries DROVER_ROOT, so
// those subprocesses resolve to the operator's live project instead of the
// fixture: they compare its AGENTS.md against this branch's rendering, and they
// append request for changes and audit rows against its real B1 and B2. Clearing the whole of
// runner.SessionEnv here, before any test runs, covers every subprocess a test
// in this package starts, not only the ones stand prepares.
//
// COLUMNS is cleared for the reason internal/cmd clears it: the table the
// binary prints sizes itself to it.
func TestMain(m *testing.M) {
	for _, name := range append(slices.Clone(runner.SessionEnv), "COLUMNS") {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
