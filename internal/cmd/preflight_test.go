package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// An item can cite lines that have drifted on the trunk, and nothing else
// catches it before the drive. The fixture's go.mod is one line long.
func TestACitedRangePastTheEndOfItsFileIsReported(t *testing.T) {
	s := driveProject(t)
	write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Fix `go.mod:1-40` and `gone/away.go:3`, beside go.mod:1.\n")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}

	drift := s.refDrift(it, s.driftRef())
	if len(drift) != 2 {
		t.Fatalf("drift = %q, want the range past go.mod's end and the missing file, and not go.mod:1", drift)
	}
	all := strings.Join(drift, "\n")
	if !strings.Contains(all, "go.mod:1-40 runs past the end of go.mod, which has 1 line") {
		t.Errorf("the range past the end is not reported: %q", drift)
	}
	if !strings.Contains(all, "gone/away.go:3 names a file") {
		t.Errorf("the missing file is not reported: %q", drift)
	}

	out := dryRun(t, "1")
	if !strings.Contains(out, "==> preflight") {
		t.Errorf("the dry run prints no preflight section:\n%s", out)
	}
	if !strings.Contains(out, "stale_citations: go.mod:1-40 runs past the end") {
		t.Errorf("the dry run does not list the drift under the item:\n%s", out)
	}
}

// Without a slash a host:port reads like a root file and a line, so only a
// file the trunk has is held to its range.
func TestAHostAndPortIsNotACitedFile(t *testing.T) {
	s := driveProject(t)
	write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. The e2e suite dials db.internal:5432 and api.example.com:443.\n2. Bump to go1.25 and node.js:20 per go.mod:1.\n")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	if drift := s.refDrift(it, s.driftRef()); len(drift) != 0 {
		t.Errorf("drift = %q, want nothing: none of these names a file", drift)
	}
}

// At operator the item is held with needs_operator, before any session.
func TestRefDriftAtOperatorHoldsTheItem(t *testing.T) {
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[supervise]\nstale_citations = \"operator\"\n")
	write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Fix `go.mod:9`.\n")

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !strings.Contains(stderr, "stale_citations: go.mod:9 runs past the end") {
		t.Errorf("the drive does not print the drift:\n%s", stderr)
	}
	pre := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evPreflight && e.Action == "stale_citations" })
	parked := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evParked && e.Park == parkRefDrift })
	needs := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evNeedsOperator && e.Action == "stale_citations" })
	if pre < 0 || parked < 0 || needs < 0 {
		t.Fatalf("want preflight, parked stale_citations and needs_operator events, got %+v", evs)
	}
	if slices.ContainsFunc(evs, func(e DriveEvent) bool { return e.Event == evStage }) {
		t.Error("a held item reached a stage")
	}
}

// carriedDrift drives item 1, citing a line go.mod lacks, as an item a re-exec
// carried over at the review boundary, and returns its result, the stderr it
// printed and the preflight events it emitted.
func carriedDrift(t *testing.T, extra string) (res DriveItem, stderr string, pre []DriveEvent) {
	t.Helper()
	t.Cleanup(func() { events = nil })
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n"+extra)
	write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Fix `go.mod:9`.\n")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	s.resumeAt = &lease.Hold{ID: "1", At: ledger.StageReview}
	out := capture(t, func() {
		events = newEventSink(s.cfg, true)
		stderr = captureErr(t, func() { res = s.driveItem(t.Context(), it, "", true, time.Time{}) })
	})
	for _, line := range strings.Split(out, "\n") {
		var e DriveEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Event == evPreflight {
			pre = append(pre, e)
		}
	}
	return res, stderr, pre
}

// A carried item passed admission when it was dispatched; a sibling's merge
// shortening a file it cites does not hold it mid-flight.
func TestACarriedItemIsNotHeldOnRefDriftAtOperator(t *testing.T) {
	res, _, _ := carriedDrift(t, "\n[supervise]\nstale_citations = \"operator\"\n")
	if res.Park == parkRefDrift {
		t.Errorf("a carried item parked on stale_citations: %+v", res)
	}
}

func TestACarriedItemDoesNotReportRefDriftAgain(t *testing.T) {
	_, stderr, pre := carriedDrift(t, "")
	if strings.Contains(stderr, "stale_citations:") {
		t.Errorf("the resumed pass printed the drift again:\n%s", stderr)
	}
	if slices.ContainsFunc(pre, func(e DriveEvent) bool { return e.Action == "stale_citations" }) {
		t.Errorf("the resumed pass emitted a stale_citations preflight event: %+v", pre)
	}
}

// `deps: F65,F66` is a comma-separated line, which Deps reads as one id.
func TestACommaInDepsIsRefusedWithTheCorrectedForm(t *testing.T) {
	dir := markerProject(t, map[string]string{
		"B01-scaffold.md": strings.Replace(scaffold, "deps:\n", "deps: B2,B3\n", 1),
		"B02-two.md":      "---\ntitle: Two\ndeps:\n---\n\n1. Two.\n",
		"B03-three.md":    "---\ntitle: Three\ndeps:\n---\n\n1. Three.\n",
	})
	t.Chdir(dir)

	for _, args := range [][]string{{"accept", "B1"}, {"run", "B1", "--dry-run"}} {
		err := execute(t, args...)
		if Code(err) != ExitRefused {
			t.Fatalf("%v: exit %d (%v), want 3", args, Code(err), err)
		}
		for _, want := range []string{"work/B01-scaffold.md:3", "`deps: B2,B3`", "Write it as `deps: B2 B3`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: the refusal %q does not name %q", args, err, want)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "work", "B01-scaffold.md")); !strings.Contains(string(b), "STOP:") {
		t.Error("accept deleted the marker before refusing")
	}
}

func TestDriveRefusesAMalformedDepsLine(t *testing.T) {
	s := driveProject(t)
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1, 03\n---\n\n1. Two.\n")

	err := execute(t, "drive", "--dry-run")
	if Code(err) != ExitRefused {
		t.Fatalf("exit %d (%v), want 3", Code(err), err)
	}
	if !strings.Contains(err.Error(), "Write it as `deps: 1 3`") || !strings.Contains(err.Error(), "zero-padded") {
		t.Errorf("the refusal does not name the corrected form and the padded id: %v", err)
	}
}

// Two large drives on one host turn passing tests into flakes.
func TestALoadAboveMaxLoadHoldsDispatch(t *testing.T) {
	driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\nmax_load = 4\n\n[supervise]\nhost_load_admission = \"auto\"\n")
	var reads atomic.Int32
	was, poll := loadAverage, loadPoll
	loadAverage = func() (float64, error) {
		if reads.Add(1) <= 3 {
			return 9.5, nil
		}
		return 1, nil
	}
	loadPoll = time.Millisecond
	t.Cleanup(func() { loadAverage, loadPoll = was, poll })

	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil && Code(err) == ExitRefused {
		t.Fatalf("drive refused: %v", err)
	}
	if n := strings.Count(stderr, "holding dispatch"); n != 1 {
		t.Errorf("the hold printed %d times, want once:\n%s", n, stderr)
	}
	if reads.Load() < 4 {
		t.Errorf("the load was read %d times, want it reread until it fell", reads.Load())
	}
	held := slices.IndexFunc(evs, func(e DriveEvent) bool { return e.Event == evPreflight && e.Action == "host_load_admission" })
	sent := eventIndex(evs, evDispatched, "1")
	if held < 0 || sent < 0 || held > sent {
		t.Errorf("want the hold's preflight event before the dispatch, got %+v", evs)
	}
}

func TestLoadAdmissionAtAutoEmitsClean(t *testing.T) {
	driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\nmax_load = 4\n\n[supervise]\nhost_load_admission = \"auto\"\n")
	was := loadAverage
	loadAverage = func() (float64, error) { return 1, nil }
	t.Cleanup(func() { loadAverage = was })

	evs, _, err := driveEventsOf(t, "1")
	if Code(err) == ExitRefused {
		t.Fatalf("drive refused: %v", err)
	}
	var got []string
	for _, e := range evs {
		if e.Event == evPreflight && e.Action == "host_load_admission" {
			got = append(got, e.Reason)
		}
	}
	if !slices.Equal(got, []string{reasonClean}) {
		t.Errorf("host_load_admission events = %q, want one %q", got, reasonClean)
	}
}

func TestDepsLintOffSaysNotChecked(t *testing.T) {
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[supervise]\ndeps_lint = \"off\"\n")
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps: 1,03\n---\n\n1. Two.\n")

	out := dryRun(t)
	if !regexp.MustCompile(`deps_lint +off +not checked`).MatchString(out) {
		t.Errorf("the deps_lint row at off claims a check that did not run:\n%s", out)
	}
}

func TestLoadAdmissionOffNeverReadsTheLoad(t *testing.T) {
	driveProject(t)
	was := loadAverage
	loadAverage = func() (float64, error) { t.Error("the load was read at off"); return 99, nil }
	t.Cleanup(func() { loadAverage = was })
	if _, _, err := driveEventsOf(t, "1"); Code(err) == ExitRefused {
		t.Fatalf("drive refused: %v", err)
	}
}

// An operator types 35 for E35 in a project where every item is prefixed.
func TestAcceptABareNumberNamesThePrefixedIDs(t *testing.T) {
	dir := markerProject(t, map[string]string{"E35-thirty-five.md": scaffold})
	conf := filepath.Join(dir, ".drover.toml")
	b, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, conf, strings.Replace(string(b), `B = "bug"`, `E = "enhancement"`, 1))
	t.Chdir(dir)

	err = execute(t, "accept", "35")
	if err == nil {
		t.Fatal("accept 35 accepted something")
	}
	if !strings.Contains(err.Error(), "E35 ends in 35: drover accept E35") {
		t.Errorf("the refusal does not name E35: %v", err)
	}
}

// A verify command can read an ignored file such as test/e2e/hosts.env, which no
// worktree has unless [drive] worktree_links carries it.
func TestAVerifyCommandReadingAnIgnoredFileIsReported(t *testing.T) {
	s := driveProject(t)
	dir := s.cfg.Root
	conf := filepath.Join(dir, ".drover.toml")
	b, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	// Read from inside test/e2e, as go test reads from the package directory:
	// the trace sees "hosts.env", which means nothing joined to the root.
	write(t, conf, strings.Replace(string(b), `default = "make test"`, `default = "cd test/e2e && cat hosts.env"`, 1))
	mkdir(t, filepath.Join(dir, "test", "e2e"))
	write(t, filepath.Join(dir, "test", "e2e", "e2e_test.go"), "package e2e\n")
	write(t, filepath.Join(dir, "test", "e2e", "hosts.env"), "E2E_SERVER_HOST=x\n")
	write(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\nhosts.env\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "e2e")

	out := dryRun(t, "1")
	if !strings.Contains(out, "reads test/e2e/hosts.env") {
		t.Errorf("the dry run does not name the file the verify command reads:\n%s", out)
	}
	if !strings.Contains(out, `worktree_links = ["test/e2e/hosts.env"]`) {
		t.Errorf("the dry run does not give the worktree_links line:\n%s", out)
	}
}

func TestTraceReadsKeepsInputsAndDropsOutputs(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "test"))
	mkdir(t, filepath.Join(root, "bin"))
	write(t, filepath.Join(root, "test", "hosts.env"), "x\n")
	write(t, filepath.Join(root, "bin", "out"), "x\n")
	log := strings.Join([]string{
		`101 openat(AT_FDCWD, "test/hosts.env", O_RDONLY|O_CLOEXEC) = 3`,
		`101 openat(AT_FDCWD, "` + filepath.Join(root, "bin", "out") + `", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 4`,
		`101 openat(AT_FDCWD, "bin/out", O_RDONLY) = 5`,
		`101 openat(AT_FDCWD, "missing.env", O_RDONLY) = -1 ENOENT (No such file or directory)`,
		`101 newfstatat(AT_FDCWD, "/etc/passwd", {st_mode=S_IFREG|0644, ...}, 0) = 0`,
	}, "\n")
	got := traceReads([]byte(log), root)
	if !slices.Equal(got, []string{"test/hosts.env"}) {
		t.Errorf("traceReads = %q, want only test/hosts.env", got)
	}
}

// strace -y output: the second process has changed into test/e2e, so its
// relative opens and stats resolve there and not at the root.
func TestTraceReadsResolvesAgainstTheCallersDirectory(t *testing.T) {
	root := t.TempDir()
	e2e := filepath.Join(root, "test", "e2e")
	mkdir(t, e2e)
	mkdir(t, filepath.Join(root, "bin"))
	write(t, filepath.Join(root, "top.env"), "x\n")
	write(t, filepath.Join(e2e, "hosts.env"), "x\n")
	write(t, filepath.Join(e2e, "seen.env"), "x\n")
	write(t, filepath.Join(root, "hosts.env"), "decoy\n")
	write(t, filepath.Join(root, "bin", "out"), "x\n")
	out := filepath.Join(root, "bin", "out")
	log := strings.Join([]string{
		`72 openat(AT_FDCWD<` + root + `>, "top.env", O_RDONLY) = 3<` + filepath.Join(root, "top.env") + `>`,
		`71 chdir("` + e2e + `") = 0`,
		`73 openat(AT_FDCWD<` + e2e + `>, "hosts.env", O_RDONLY) = 3<` + filepath.Join(e2e, "hosts.env") + `>`,
		`71 newfstatat(AT_FDCWD<` + e2e + `>, "seen.env", {st_mode=S_IFREG|0644, st_size=2, ...}, 0) = 0`,
		`71 openat(AT_FDCWD<` + e2e + `>, "/dev/null", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 3</dev/null>`,
		`71 openat(AT_FDCWD<` + e2e + `>, "` + out + `", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 3<` + out + `>`,
		`74 openat(AT_FDCWD<` + e2e + `>, "../../bin/out", O_RDONLY) = 3<` + out + `>`,
	}, "\n")
	got := traceReads([]byte(log), root)
	want := []string{"top.env", "test/e2e/hosts.env", "test/e2e/seen.env"}
	if !slices.Equal(got, want) {
		t.Errorf("traceReads = %q, want %q", got, want)
	}
}

// A link at the root to a file outside it is how secrets stay out of a
// checkout, and a link to another file inside the root is opened under its own
// name. strace -y prints the opened file with every link resolved, so the name
// has to come from the path the verify command asked for.
func TestTraceReadsNamesALinkByThePathTheVerifyCommandOpened(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "r")
	e2e := filepath.Join(root, "test", "e2e")
	mkdir(t, e2e)
	mkdir(t, filepath.Join(base, "secrets"))
	write(t, filepath.Join(base, "secrets", ".env"), "x\n")
	write(t, filepath.Join(e2e, "hosts.env"), "x\n")
	write(t, filepath.Join(root, "real.env"), "x\n")
	if err := os.Symlink(filepath.Join(base, "secrets", ".env"), filepath.Join(root, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.env", filepath.Join(root, "alias.env")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(filepath.Join(base, "secrets", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		`101 openat(AT_FDCWD<` + resolved + `>, ".env", O_RDONLY|O_CLOEXEC) = 3<` + outside + `>`,
		`101 openat(AT_FDCWD<` + filepath.Join(resolved, "test", "e2e") + `>, "hosts.env", O_RDONLY|O_CLOEXEC) = 3<` + filepath.Join(resolved, "test", "e2e", "hosts.env") + `>`,
		`101 openat(AT_FDCWD<` + resolved + `>, "alias.env", O_RDONLY|O_CLOEXEC) = 3<` + filepath.Join(resolved, "real.env") + `>`,
	}, "\n")
	got := traceReads([]byte(log), root)
	want := []string{".env", "test/e2e/hosts.env", "alias.env"}
	if !slices.Equal(got, want) {
		t.Errorf("traceReads = %q, want %q", got, want)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}

// `deps: [F1]` is a list form Deps does not parse, and the refusal must not
// suggest the same line back.
func TestAListFormDepsLineSuggestsALineThatParses(t *testing.T) {
	s := openIn(t, shelfProject(t, map[string]string{
		"F01-one.md":   body("One", ""),
		"F02-two.md":   body("Two", "[F1]"),
		"F03-three.md": body("Three", `["F1", "F2"]`),
		"F04-four.md":  body("Four", "[F65,F66]"),
	}, nil))

	for _, c := range []struct{ id, want string }{
		{"F2", "Write it as `deps: F1`"},
		{"F3", "Write it as `deps: F1 F2`"},
		{"F4", "Write it as `deps: F65 F66`"},
	} {
		it, err := s.item(c.id)
		if err != nil {
			t.Fatal(err)
		}
		err = s.depsLint(it)
		if Code(err) != ExitRefused {
			t.Fatalf("%s: depsLint = %v, want a refusal", c.id, err)
		}
		if !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: the refusal %q does not end %q", c.id, err, c.want)
		}
		if !strings.Contains(err.Error(), "not a YAML list") || strings.Contains(err.Error(), "not an id here") {
			t.Errorf("%s: the refusal does not name the list form alone: %v", c.id, err)
		}
	}
}
