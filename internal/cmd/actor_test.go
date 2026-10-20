package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/runner"
)

// The sealed set is a decision, not an emergent property of the table, so it is
// written out here. A command that joins or leaves it fails this test, which is
// the review conversation the change deserves.
// drive is the seventh, and the only one that seals for a reason the other six
// do not share: it certifies nothing and reaches no tracker, it launches. A
// session that may not start one run may not start a queue of them, and the
// nested-launch check inside runner.Run would refuse it anyway — this says so
// in the table, where the generated docs read it.
// open-pr is the eighth, and it seals for finish's reason rather than drive's: an
// open pull request is the claim that a branch may merge, and the session that
// wrote the branch is the one actor who cannot make it.
var sealedSet = []string{"accept", "unaccept", "signoff", "intake", "finish", "mark-merged", "unmark-merged", "drive", "open-pr", "supervise"}

// Deliberately open, each for a reason recorded in actor.go. `audit` is the one
// that matters: a review session is legitimately independent of the session that
// implemented the work, drover launched both, and the variable cannot tell them
// apart — so sealing it would break the three-stages design rather than protect
// anything.
var openSet = []string{
	"audit", "init", "shelve", "unshelve",
	"status", "show", "check", "unaudited", "follow-ups", "follow-up", "report",
	"agents", "schema", "projects", "skills", "new", "version",
	"run", "draft", "review",
}

func TestSealedSetIsExactlyTheOperatorCommands(t *testing.T) {
	install()
	want := map[string]bool{}
	for _, n := range sealedSet {
		want[n] = true
	}
	for name, k := range classified {
		if (k.who == operator) != want[name] {
			t.Errorf("%s: who=%q but sealedSet membership is %v", name, k.who, want[name])
		}
	}
	for _, n := range openSet {
		if _, ok := classified[n]; !ok {
			t.Errorf("%s: named in openSet but absent from classified", n)
		}
	}
}

func TestGateRefusesOnlyTheSealedSetWhenLaunched(t *testing.T) {
	t.Setenv(runner.EnvActor, runner.ActorAgent)
	for _, name := range sealedSet {
		err := gate(name)
		if err == nil {
			t.Errorf("%s: the actor check allowed it inside a launched session", name)
			continue
		}
		if Code(err) != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, Code(err), ExitRefused)
		}
		// The message has to name the command and the reason, or an agent
		// reading it learns only that something said no.
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: refusal does not name the command: %q", name, err)
		}
	}
	for _, name := range openSet {
		if err := gate(name); err != nil {
			t.Errorf("%s: the actor check refused a command that should be open: %v", name, err)
		}
	}
}

// Outside a launched session nothing is gated, including the sealed set: a
// person at a terminal is the actor the whole table is written for.
func TestGateAllowsEverythingUnlaunched(t *testing.T) {
	t.Setenv(runner.EnvActor, "")
	for _, name := range append(append([]string{}, sealedSet...), openSet...) {
		if err := gate(name); err != nil {
			t.Errorf("%s: refused with no %s set: %v", name, runner.EnvActor, err)
		}
	}
}

func TestNoNestedLaunch(t *testing.T) {
	t.Setenv(runner.EnvActor, "")
	if err := noNestedLaunch(); err != nil {
		t.Errorf("refused a launch from a terminal: %v", err)
	}
	t.Setenv(runner.EnvActor, runner.ActorAgent)
	err := noNestedLaunch()
	if err == nil {
		t.Fatal("allowed a launch from inside a launched session")
	}
	if Code(err) != ExitRefused {
		t.Errorf("exit %d, want %d", Code(err), ExitRefused)
	}
	if !errors.Is(err, runner.ErrNested) {
		t.Errorf("does not unwrap to runner.ErrNested: %v", err)
	}
}

// runner cannot import Refusal without a cycle, so a bare ErrNested escaping a
// launch path has to map to 3 in Code. Without this the nested refusal would
// exit 1 and read as "drover could not answer".
func TestBareNestedErrorStillExitsRefused(t *testing.T) {
	if got := Code(runner.ErrNested); got != ExitRefused {
		t.Errorf("Code(runner.ErrNested) = %d, want %d", got, ExitRefused)
	}
}
