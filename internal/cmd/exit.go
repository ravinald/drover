package cmd

import (
	"errors"

	"github.com/ravinald/drover/internal/runner"
)

// Exit codes. Every failure is non-zero, so a wrapper testing for non-zero
// needs nothing more. What these add is the distinction that matters to a wrapper: a
// project failing its own validation, a rule drover enforces saying no, and
// drover being unable to answer at all are three different situations, and only
// the first two mean the tool worked.
const (
	// ExitError: drover could not do the thing. Unreadable config, no such item,
	// gh unreachable, a git command that failed.
	ExitError = 1
	// ExitFindings: drover ran and reported findings against the project. Only
	// check exits this way.
	ExitFindings = 2
	// ExitRefused: drover ran and a rule it enforces refused the operation. The
	// tree and the ledger are untouched.
	ExitRefused = 3
)

// Refusal is an error carrying ExitRefused. Wrap the message a human needs;
// the code is what a wrapper reads.
type Refusal struct{ Err error }

func (r Refusal) Error() string { return r.Err.Error() }
func (r Refusal) Unwrap() error { return r.Err }

// Findings is an error carrying ExitFindings.
type Findings struct{ Err error }

func (f Findings) Error() string { return f.Err.Error() }
func (f Findings) Unwrap() error { return f.Err }

// Code maps an error from Execute to the process exit code.
func Code(err error) int {
	if err == nil {
		return 0
	}
	if _, ok := errors.AsType[Findings](err); ok {
		return ExitFindings
	}
	if _, ok := errors.AsType[Refusal](err); ok {
		return ExitRefused
	}
	// A nested-launch refusal is raised by internal/runner, which cannot import
	// Refusal without a cycle. Mapped here rather than wrapped at each launch
	// site, so a stage added later exits 3 without remembering to.
	if errors.Is(err, runner.ErrNested) {
		return ExitRefused
	}
	return ExitError
}
