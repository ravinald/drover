// Package scripts_test drives the shell drover's own [drive] merge_gate runs.
//
// The script decides whether a branch may merge, and its exits mean different
// things to a drive: 1 sends the work back, 2 stops the queue, 4 catches the
// branch up and asks again, 0 opens a pull request and merges. Nothing in Go can prove that, so the cases
// below run the script itself against a stubbed forge.
package scripts_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gateLimit kills a gate that never exits. It sits far outside any wait a case
// asks for, so a loaded host cannot reach it with a gate that works.
const gateLimit = 60 * time.Second

// gateEnv is one stubbed forge: a gh on PATH answering with fixed check-run
// lines, and a git repository holding the branch the script resolves.
type gateEnv struct {
	dir, bin, sandbox, calls string
}

// sandboxBin is a PATH holding nothing but the tools the script names, linked
// from wherever this host keeps them.
//
// The host's own PATH cannot be inherited: it carries a real gh, and the case
// below that removes the stub would then reach the operator's credentials and
// ask github.com about a SHA in a temporary repository.
func sandboxBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"bash", "git", "awk", "grep", "date", "sleep"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("the gate needs %s and this host has none: %v", tool, err)
		}
		if err := os.Symlink(src, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// newGateEnv writes the stub and a repository with one branch.
//
// page2 is served only when the stub is passed --paginate, which is what makes
// the pagination case evidence: a script reading one page gets the green first
// page and never sees the failure behind it.
//
// LATER, when set, replaces both pages on every call after the first, so a case
// can prove the gate polled by what it concluded rather than by how many times
// it asked before a deadline.
func newGateEnv(t *testing.T, page1, page2 string, ghFails bool) gateEnv {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	e := gateEnv{dir: dir, bin: bin, sandbox: sandboxBin(t), calls: filepath.Join(bin, "calls")}

	body := "#!/bin/sh\n" +
		"echo x >> " + e.calls + "\n"
	if ghFails {
		body += "echo 'gh: HTTP 502' >&2\nexit 1\n"
	} else {
		body += "if [ -n \"$LATER\" ] && [ -e " + e.calls + ".seen ]; then printf '%s' \"$LATER\"; exit 0; fi\n" +
			": > " + e.calls + ".seen\n" +
			"paginate=no\n" +
			"for a in \"$@\"; do if [ \"$a\" = --paginate ]; then paginate=yes; fi; done\n" +
			"printf '%s' \"$PAGE1\"\n" +
			"if [ \"$paginate\" = yes ]; then printf '%s' \"$PAGE2\"; fi\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PAGE1", page1)
	t.Setenv("PAGE2", page2)
	t.Setenv("LATER", "")

	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
		{"switch", "-q", "-c", "topic"},
		{"commit", "-q", "--allow-empty", "-m", "work"},
	} {
		cmd := exec.Command("git", args...) // #nosec G204 -- a literal argv
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.invalid",
			"GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return e
}

// run executes the gate the way drove does: the branch and the trunk, nothing
// else, because that argv is shared with [git] merge_check and pinned in the
// config schema.
func (e gateEnv) run(t *testing.T, path string, wait int) (int, string) {
	t.Helper()
	script, err := filepath.Abs("merge-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "topic", "main") // #nosec G204 -- the script under test
	cmd.Dir = e.dir
	// The gate's own sleep inherits the output pipe, so killing bash alone
	// would leave CombinedOutput waiting on it.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"MERGE_GATE_WAIT="+strconv.Itoa(wait),
		"MERGE_GATE_POLL=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the gate was still running after %s with MERGE_GATE_WAIT=%d: its deadline never fired:\n%s", gateLimit, wait, out)
	}
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !errors.As(err, &ee) {
			t.Fatalf("running the gate: %v: %s", err, out)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

// withGH is the ordinary PATH: the stub first, then the tools the script needs.
func (e gateEnv) withGH() string {
	return e.bin + string(os.PathListSeparator) + e.sandbox
}

func (e gateEnv) ghCalls(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(e.calls) // #nosec G304 -- written by the stub above
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(strings.Fields(string(b)))
}

// Three buckets, and only one of them is green. A conclusion drover does not
// accept is not a failure the diff caused and is not evidence it passed: both
// readings merge something nobody watched finish.
func TestTheGateAcceptsOnlyWhatItNames(t *testing.T) {
	for _, c := range []struct {
		name, page1, page2, want string
		code                     int
	}{{
		name: "every check completed and succeeded",
		// A run whose conclusion is success is the only ordinary green.
		page1: "completed success Gate\n",
		code:  0,
	}, {
		name:  "neutral and skipped are conclusions, not absences",
		page1: "completed neutral Docs\ncompleted skipped Release\n",
		code:  0,
	}, {
		name:  "a failing check is this diff's problem",
		page1: "completed failure Gate\n",
		code:  1,
		want:  "a required check failed",
	}, {
		name:  "a timed out check is a failure too",
		page1: "completed success Docs\ncompleted timed_out Gate\n",
		code:  1,
		want:  "timed_out Gate",
	}, {
		name:  "a startup failure never ran the code, and still blames it",
		page1: "completed startup_failure Gate\n",
		code:  1,
		want:  "startup_failure Gate",
	}, {
		// Somebody pushed again, or hit cancel. Nothing about this diff failed
		// and nothing about it passed.
		name:  "a cancelled check is not a passing one",
		page1: "completed cancelled Gate\n",
		code:  2,
		want:  "unresolved",
	}, {
		name:  "a stale check reports on a commit that moved",
		page1: "completed stale Gate\n",
		code:  2,
		want:  "unresolved",
	}, {
		// The reason the accepted list is a list rather than a failure list:
		// a conclusion GitHub adds after this was written has to wait.
		name:  "a conclusion this gate has never heard of waits",
		page1: "completed action_required Gate\n",
		code:  2,
		want:  "unresolved",
	}, {
		name:  "a check still running is not an answer",
		page1: "in_progress null Gate\n",
		code:  2,
		want:  "unresolved",
	}, {
		// The failure mode a per-check read has: one green check and the drive
		// merges over the one nobody watched.
		name:  "one green check does not cover a cancelled one",
		page1: "completed success Docs\ncompleted cancelled Gate\n",
		code:  2,
		want:  "unresolved",
	}, {
		name:  "a commit no workflow reported on",
		page1: "",
		code:  2,
		want:  "no workflow reported",
	}, {
		// A repository with more checks than fit one page returns a green first
		// page with the failure behind it.
		name:  "a failure on the second page",
		page1: "completed success Docs\n",
		page2: "completed failure Gate\n",
		code:  1,
		want:  "failure Gate",
	}, {
		name:  "a check still running on the second page",
		page1: "completed success Docs\n",
		page2: "in_progress null Gate\n",
		code:  2,
		want:  "unresolved",
	}} {
		t.Run(c.name, func(t *testing.T) {
			e := newGateEnv(t, c.page1, c.page2, false)
			code, out := e.run(t, e.withGH(), 0)
			if code != c.code {
				t.Errorf("exited %d, want %d:\n%s", code, c.code, out)
			}
			if c.want != "" && !strings.Contains(out, c.want) {
				t.Errorf("the output does not say %q:\n%s", c.want, out)
			}
		})
	}
}

// Every unavailable-evidence path exits 2. A drive reads that as "stop the
// queue", which is the only safe reading: 1 would spend a round re-fixing code
// nothing found wrong, and 0 would merge on a check nobody read.
func TestTheGateStopsTheDriveOnEvidenceItCannotGet(t *testing.T) {
	t.Run("the forge would not answer", func(t *testing.T) {
		e := newGateEnv(t, "", "", true)
		code, out := e.run(t, e.withGH(), 0)
		if code != 2 {
			t.Errorf("exited %d, want 2:\n%s", code, out)
		}
		if !strings.Contains(out, "would not answer") {
			t.Errorf("the output does not name what failed:\n%s", out)
		}
	})
	t.Run("gh is not on PATH", func(t *testing.T) {
		e := newGateEnv(t, "completed success Gate\n", "", false)
		code, out := e.run(t, e.sandbox, 0)
		if code != 2 {
			t.Errorf("exited %d, want 2:\n%s", code, out)
		}
		if !strings.Contains(out, "gh is not on PATH") {
			t.Errorf("the output does not name what is missing:\n%s", out)
		}
	})
	t.Run("the branch does not resolve", func(t *testing.T) {
		e := newGateEnv(t, "completed success Gate\n", "", false)
		cmd := exec.Command("git", "switch", "-q", "main") // #nosec G204 -- a literal argv
		cmd.Dir = e.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git switch: %v: %s", err, out)
		}
		cmd = exec.Command("git", "branch", "-q", "-D", "topic") // #nosec G204 -- a literal argv
		cmd.Dir = e.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git branch -D: %v: %s", err, out)
		}
		code, out := e.run(t, e.withGH(), 0)
		if code != 2 {
			t.Errorf("exited %d, want 2:\n%s", code, out)
		}
		if !strings.Contains(out, "cannot resolve") {
			t.Errorf("the output does not name the branch it could not find:\n%s", out)
		}
	})
}

// Green checks on a head the trunk has since moved past say nothing about the
// merge, and a drive can fix that itself: exit 4, named on stderr, before the
// forge is asked anything. A gate that refused "main has moved 7 commit(s)"
// with exit 1 would have the drive read its own merges as the item's failure.
func TestTheGateSaysABranchIsBehindTheTrunk(t *testing.T) {
	e := newGateEnv(t, "completed success Gate\n", "", false)
	for _, args := range [][]string{
		{"switch", "-q", "main"},
		{"commit", "-q", "--allow-empty", "-m", "the trunk moved"},
		{"switch", "-q", "topic"},
	} {
		cmd := exec.Command("git", args...) // #nosec G204 -- a literal argv
		cmd.Dir = e.dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.invalid",
			"GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	code, out := e.run(t, e.withGH(), 0)
	if code != 4 {
		t.Errorf("exited %d, want 4:\n%s", code, out)
	}
	if !strings.Contains(out, "behind main") {
		t.Errorf("the output does not say the branch is behind:\n%s", out)
	}
	if n := e.ghCalls(t); n != 0 {
		t.Errorf("asked the forge %d time(s) about a head the trunk moved past", n)
	}
}

// A head the forge already merged is on the trunk, not behind it, and drive
// reads it once more after an --auto merge the forge made on the spot. A merge
// commit leaves the head reachable from the trunk, which the script sees for
// itself. A squash leaves a trunk commit the head does not carry, the same
// history as a trunk that moved, so only DROVER_GATE_MERGED can tell them apart.
func TestTheGateDoesNotCallAMergedHeadBehind(t *testing.T) {
	for _, c := range []struct {
		name   string
		merge  []string
		merged string
	}{{
		name:  "merged with a merge commit",
		merge: []string{"merge", "-q", "--no-ff", "--no-edit", "topic"},
	}, {
		name:   "squashed, with drive saying it merged",
		merge:  []string{"commit", "-q", "--allow-empty", "-m", "topic, squashed"},
		merged: "1",
	}} {
		t.Run(c.name, func(t *testing.T) {
			e := newGateEnv(t, "completed success Gate\n", "", false)
			t.Setenv("DROVER_GATE_MERGED", c.merged)
			for _, args := range [][]string{{"switch", "-q", "main"}, c.merge, {"switch", "-q", "topic"}} {
				cmd := exec.Command("git", args...) // #nosec G204 -- a literal argv
				cmd.Dir = e.dir
				cmd.Env = append(os.Environ(),
					"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.invalid",
					"GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.invalid")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			code, out := e.run(t, e.withGH(), 0)
			if code != 0 {
				t.Errorf("exited %d, want 0 on the merged head's green checks:\n%s", code, out)
			}
			if n := e.ghCalls(t); n == 0 {
				t.Error("never asked the forge about the merged head")
			}
		})
	}
}

// CI starts on the push and a review that commits a fix restarts it, so the
// first answer is routinely "not yet". A gate that decided on it would stop
// every drive whose review touched a file.
//
// The forge goes green on the second call, so a gate that waits exits 0 about a
// poll in and one that decided on the first answer exits 2. The bound only has
// to be out of reach: a count of calls under a short one failed on a host slow
// enough to spend it on the first call.
func TestTheGateWaitsRatherThanDecidingOnTheFirstAnswer(t *testing.T) {
	e := newGateEnv(t, "in_progress null Gate\n", "", false)
	t.Setenv("LATER", "completed success Gate\n")
	code, out := e.run(t, e.withGH(), 120)
	if code != 0 {
		t.Errorf("exited %d, want 0 once the check went green:\n%s", code, out)
	}
	if n := e.ghCalls(t); n < 2 {
		t.Errorf("asked the forge %d time(s); the gate decided on the first answer", n)
	}
}

// clockAfter puts a date ahead of the real one whose +%s stands still until
// the stub forge has been asked twice, then reads start + wait. The gate reads
// time only through date +%s, so its deadline passes when the forge has
// answered twice rather than when the host says two seconds went by, and a
// first answer slower than the whole wait cannot end the case after one poll.
func (e gateEnv) clockAfter(t *testing.T, path string, wait int) string {
	t.Helper()
	const start = 1_000_000_000
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"if [ \"$1\" != +%s ]; then exec " + filepath.Join(e.sandbox, "date") + " \"$@\"; fi\n" +
		"n=0\n" +
		"if [ -e " + e.calls + " ]; then n=$(awk 'END { print NR }' " + e.calls + "); fi\n" +
		"if [ \"$n\" -lt 2 ]; then echo " + strconv.Itoa(start) + "; else echo " + strconv.Itoa(start+wait) + "; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte(body), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + path
}

// slowFirstAnswer puts a gh ahead of the stub that holds its first call for
// longer than the gate's whole wait, the way a loaded host or a slow forge does.
func (e gateEnv) slowFirstAnswer(t *testing.T, path string, secs int) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"if [ ! -e " + e.calls + " ]; then sleep " + strconv.Itoa(secs) + "; fi\n" +
		"exec " + filepath.Join(e.bin, "gh") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + path
}

// A check that never finishes has to stop the drive once MERGE_GATE_WAIT runs
// out. Every other exit-2 case runs at wait 0 and stops on the first answer, so
// only this one reaches the deadline after a poll: a deadline reset on each
// poll never fires, and the drive waiting on the gate hangs with it.
func TestTheGateGivesUpWhenTheWaitRunsOut(t *testing.T) {
	for _, c := range []struct {
		name, page1, want string
		firstAnswer       int
	}{{
		name:  "a check that stays in progress",
		page1: "in_progress null Gate\n",
		want:  "1 of 1 checks on %s are unresolved after 2s",
	}, {
		name:  "a commit no workflow ever reports on",
		page1: "",
		want:  "no workflow reported on %s after 2s",
	}, {
		name:        "a first answer slower than the whole wait",
		page1:       "in_progress null Gate\n",
		want:        "1 of 1 checks on %s are unresolved after 2s",
		firstAnswer: 3,
	}} {
		t.Run(c.name, func(t *testing.T) {
			e := newGateEnv(t, c.page1, "", false)
			cmd := exec.Command("git", "rev-parse", "topic") // #nosec G204 -- a literal argv
			cmd.Dir = e.dir
			sha, err := cmd.Output()
			if err != nil {
				t.Fatalf("git rev-parse: %v", err)
			}
			path := e.withGH()
			if c.firstAnswer > 0 {
				path = e.slowFirstAnswer(t, path, c.firstAnswer)
			}
			code, out := e.run(t, e.clockAfter(t, path, 2), 2)
			if code != 2 {
				t.Errorf("exited %d, want 2:\n%s", code, out)
			}
			if want := fmt.Sprintf(c.want, strings.TrimSpace(string(sha))); !strings.Contains(out, want) {
				t.Errorf("the output does not say %q:\n%s", want, out)
			}
			if n := e.ghCalls(t); n != 2 {
				t.Errorf("asked the forge %d time(s), want 2: once before the deadline and once that found it passed", n)
			}
		})
	}
}
