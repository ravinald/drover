package cmd

import (
	"fmt"
	"strings"

	"github.com/ravinald/drover/internal/runner"
)

// Two rules, each enforced in exactly one place, and a third actor beside them.
//
// This file holds the first: a command the classification table marks `operator`
// refuses inside a session drover launched. Those six either certify work that
// somebody else has to trust or reach a tracker other people read, and a session
// certifying its own output is the failure the whole audit ladder exists to
// prevent. `accept` is already a command rather than a flag so that no runner's
// argv can carry it; this is the same argument applied to the environment.
//
// The second lives in runner.Run: a launched session cannot launch another.
// It sits there rather than here because [run] draft_runner starts a session
// with no flag to key on, so no gate reading argv could see it.
//
// Deliberately not sealed:
//
//   - `audit`, at every level. A review session is legitimately independent of
//     the implement session, drover launched both, and the actor variable
//     cannot tell them apart. Sealing it would break the three-stages design.
//     The --code-by refusal of verified covers the dishonest case instead,
//     resolving agent per session against the item's stage rows.
//   - `init`, with or without --user. An agent running `init --verify "go test ./..."` with a
//     gate the operator supplied is doing what it was told. The question this
//     axis asks is whether an agent may run the command, not who decides the
//     answers it carries.
//   - `shelve` and `unshelve`. Reversible, and they certify nothing.
//
// The supervisor is the session `drover supervise` starts. It answers to the
// sealed set too, with two exceptions a policy can open: `drive`, which is the
// enqueue action, and `finish`, which is merge_drive_pr. Every command that is a
// supervised action (see the catalog in supervise.go) also answers to that
// action's policy, and runs only at supervisor or auto. finish then checks the
// ledger itself, because the policy says what a supervisor may merge, not that
// this branch is one of those.

// sealed reports whether name belongs to the operator, and returns the reason.
func sealed(name string) (string, bool) {
	k, ok := classified[name]
	if !ok || k.who != operator {
		return "", false
	}
	return k.because, true
}

// noNestedLaunch is the same refusal for a launch, checked before the caller
// takes its first side effect. runner.Run enforces this too, but by then `run`
// has already switched the branch and recorded a stage row, and exit 3 promises
// the tree and the ledger are untouched. So each launch path calls this at the
// point it commits, and runner.Run stays the backstop for one added later.
func noNestedLaunch() error {
	if !runner.Launched() {
		return nil
	}
	return Refusal{runner.ErrNested}
}

// noNestedDrive is noNestedLaunch for drive, the one launch a supervisor may
// make. The gate has already checked the enqueue policy by the time RunE asks.
func noNestedDrive() error {
	if runner.Supervising() {
		return nil
	}
	return noNestedLaunch()
}

// operatorStep refuses a subcommand inside any launched session, supervisor
// included, where the gate cannot: it keys on the top-level name, and `init`
// itself stays open to an agent.
func operatorStep(name, because string) error {
	if !runner.Launched() {
		return nil
	}
	return Refusal{fmt.Errorf(
		"%s is the operator's step, and drover launched this session: %s.\n"+
			"Report what you would have chosen and let the operator run it", name, because)}
}

// sealedFlag refuses one flag inside a launched session, for a command that is
// otherwise the agent's to run.
//
// `request-changes --over-limit` is the first. An agent must be able to send work back below the
// cap, so the command stays open; deciding a finding is worth exceeding the cap
// is the operator's, because the agent is the party the cap is bounding. A
// refusal that names its own override is an instruction, and the reviewer the
// brief warns about being agreeable is exactly who would take it.
func sealedFlag(name, flag, because string) error {
	if !runner.Launched() {
		return nil
	}
	if a, ok := commandAction(name + " --" + flag); ok && runner.Supervising() {
		p, err := policyNow(a.key)
		if err != nil {
			return err
		}
		return operatorOwns(name+" --"+flag, a, p)
	}
	return Refusal{fmt.Errorf(
		"%s --%s is the operator's call, and drover launched this session: %s.\n"+
			"Send the finding to its owner instead, and say in your report that the cap stopped it", name, flag, because)}
}

// verify refuses an operator command inside a launched session. It is installed as
// the root's PersistentPreRunE, so a command added to the table as `operator`
// is covered without touching its own file.
func gate(name string) error {
	if !runner.Launched() {
		return nil
	}
	if runner.Supervising() {
		return superviseGate(name)
	}
	if name == "intake --list" {
		return nil
	}
	top, _, _ := strings.Cut(name, " ")
	because, is := sealed(top)
	if !is {
		return nil
	}
	return Refusal{fmt.Errorf(
		"%s is the operator's step, and drover launched this session: %s.\n"+
			"Report the item's state and let the operator run it", name, because)}
}

// policyOpens are the sealed commands a supervised action's policy may open to
// a supervisor. Every other sealed command stays refused whatever the policy,
// and accept's ceiling of operator keeps it from being listed here usefully.
var policyOpens = map[string]bool{"drive": true, "finish": true}

// superviseGate is the gate for a supervisor. The action's policy is asked
// first, so a refusal names the setting that decides it; the sealed set is
// asked after, so no policy can open accept.
func superviseGate(name string) error {
	if key, ok := flagActions[name]; ok {
		a := action(key)
		p, err := policyNow(key)
		if err != nil {
			return Refusal{fmt.Errorf("%s answers to [supervise] %s, and the config would not load to say what it is: %w", name, key, err)}
		}
		if !supervisorMay(p) {
			return operatorOwns(name, a, p)
		}
		return nil
	}
	if a, ok := commandAction(name); ok {
		p, err := policyNow(a.key)
		if err != nil {
			return err
		}
		if !supervisorMay(p) {
			return operatorOwns(name, a, p)
		}
		if policyOpens[name] {
			return nil
		}
	}
	because, is := sealed(name)
	if !is {
		return nil
	}
	return Refusal{fmt.Errorf(
		"%s is the operator's step whatever [supervise] says, and this is a supervising session: %s.\n"+
			"Report the item's state and let the operator run it", name, because)}
}
