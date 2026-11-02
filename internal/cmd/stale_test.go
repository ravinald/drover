package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
)

// asRelease runs the test as a binary built at v, and clears -I afterwards:
// cobra leaves a persistent flag set between executions in one process.
func asRelease(t *testing.T, v string) {
	t.Helper()
	old := version
	t.Cleanup(func() { version, ignoreStale = old, false })
	version = v
}

func staleProject(t *testing.T) *session {
	t.Helper()
	s := fixture(t, "")
	t.Chdir(s.cfg.Root)
	return s
}

// A config nobody has reviewed against this release stops every project
// command before it does anything, and names the two commands that resolve it.
func TestAnUnstampedConfigStopsTheCommand(t *testing.T) {
	asRelease(t, "v0.8.0")
	staleProject(t)

	var err error
	out := captureErr(t, func() { err = execute(t, "status") })
	if Code(err) != ExitRefused {
		t.Fatalf("status over an unstamped config ended %v (exit %d), want exit %d", err, Code(err), ExitRefused)
	}
	for _, want := range []string{config.FileName, "no drover_version", "drover config --stamp", "-I (--ignore-stale)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q:\n%v", want, err)
		}
	}
	if strings.Contains(out, "drover status") && strings.Contains(out, "ready") {
		t.Errorf("status ran before the refusal:\n%s", out)
	}

	for _, args := range [][]string{{"-I", "status"}, {"status", "--ignore-stale"}} {
		ignoreStale = false
		var err error
		capture(t, func() { err = execute(t, args...) })
		if err != nil {
			t.Errorf("%v ended %v, want the command to run", args, err)
		}
	}
}

// The stamp compares minor releases. A patch adds nothing a config would need
// to declare, so it never makes a project stale; a minor release can.
func TestTheStampComparesMinorReleases(t *testing.T) {
	for _, c := range []struct {
		stamp, binary string
		stale         bool
	}{
		{"v0.8.0", "v0.8.0", false},
		{"v0.8.0", "v0.8.4", false},
		{"v0.8.0", "v0.8.0-12-gabc1234-dirty", false},
		{"v0.7.3", "v0.8.0", true},
		{"v0.9.0", "v1.0.0", true},
		{"v0.9.0", "v0.8.0", false},
		{"0.8", "v0.8.0", true},
	} {
		t.Run(c.stamp+" under "+c.binary, func(t *testing.T) {
			asRelease(t, c.binary)
			s := staleProject(t)
			path := filepath.Join(s.cfg.Root, config.FileName)
			if err := stampConfig(path, c.stamp); err != nil {
				t.Fatal(err)
			}
			var err error
			capture(t, func() { err = execute(t, "status") })
			if got := Code(err) == ExitRefused; got != c.stale {
				t.Errorf("refused = %v (%v), want %v", got, err, c.stale)
			}
		})
	}
}

// A build with no release version has nothing to compare a stamp against, so
// a dev binary never refuses on the config.
func TestADevBuildNeverJudgesTheStamp(t *testing.T) {
	asRelease(t, devVersion)
	staleProject(t)
	var err error
	capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Errorf("a dev build refused an unstamped config: %v", err)
	}
}

// --stamp writes one line: after schema_version where there is none, in place
// where there is one. Every comment, and a drover_version that belongs to some
// table rather than the top level, stays as it was.
func TestStampRewritesOneLine(t *testing.T) {
	asRelease(t, "v0.8.2-3-gdeadbee")
	s := staleProject(t)
	path := filepath.Join(s.cfg.Root, config.FileName)
	b, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	before := string(b) + "\n# why this table exists\n[agents]\n# drover_version = \"v0.1.0\" is prose here\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := execute(t, "config", "--stamp"); err != nil {
			t.Fatalf("config --stamp: %v", err)
		}
	}
	b, err = os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	after := string(b)
	if n := strings.Count(after, `drover_version = "v0.8.2"`); n != 1 {
		t.Fatalf("stamped %d times, want once:\n%s", n, after)
	}
	lines := strings.Split(after, "\n")
	if !strings.HasPrefix(lines[0], "schema_version") || lines[1] != `drover_version = "v0.8.2"` {
		t.Errorf("the stamp is not directly under schema_version:\n%s", after)
	}
	if strings.Replace(after, "drover_version = \"v0.8.2\"\n", "", 1) != before {
		t.Errorf("--stamp changed more than its own line:\n--- before\n%s\n--- after\n%s", before, after)
	}

	var runErr error
	capture(t, func() { runErr = execute(t, "status") })
	if runErr != nil {
		t.Errorf("status after stamping ended %v", runErr)
	}
}

// A dev build has no release to write, and writing "dev" would read as stale
// to every binary after it.
func TestStampRefusesADevBuild(t *testing.T) {
	asRelease(t, devVersion)
	staleProject(t)
	if err := execute(t, "config", "--stamp"); Code(err) != ExitRefused {
		t.Errorf("config --stamp on a dev build ended %v, want exit %d", err, ExitRefused)
	}
}

// levelProject is a fixture whose config is stamped at ver's release, so the
// only thing that can be stale is the generated block.
func levelProject(t *testing.T, ver string) (*session, string) {
	t.Helper()
	asRelease(t, ver)
	s := staleProject(t)
	conf := filepath.Join(s.cfg.Root, config.FileName)
	b, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := parseReleaseVer(ver)
	write(t, conf, "drover_version = \""+rel.String()+"\"\n"+string(b))
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	write(t, host, "# House rules\n\nKeep this.\n")
	captureErr(t, func() {
		if err := execute(t, "agents", "--write", "AGENTS.md"); err != nil {
			t.Fatal(err)
		}
	})
	return s, host
}

// restamp rewrites the block's header to name stamp, or the header from before
// blocks were stamped when stamp is empty, and moves one instruction inside it
// so the text differs from what this binary renders.
func restamp(t *testing.T, host, stamp string) []byte {
	t.Helper()
	b, err := os.ReadFile(host) // #nosec G304 -- the fixture's own file
	if err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile("generated by `drover agents` \\S+ \\([^)]*\\);")
	repl := "generated by `drover agents` " + stamp + " (abc1234);"
	if stamp == "" {
		repl = "generated by `drover agents`;"
	}
	text := header.ReplaceAllString(string(b), repl)
	text = strings.Replace(text, agentsBegin+"\n", agentsBegin+"\nAn instruction from last month.\n", 1)
	write(t, host, text)
	return []byte(text)
}

func runNewBug(t *testing.T) (stderr string, err error) {
	t.Helper()
	t.Cleanup(func() { newSev, newFoundIn, newFile = "", "", "" })
	stderr = captureErr(t, func() { capture(t, func() { err = execute(t, "new", "bug", "a stale block") }) })
	return stderr, err
}

// An older block costs a read nothing but a warning, and a write regenerates it
// before it runs, because this binary is the authority on its own surface.
func TestAnOlderBlockWarnsAReadAndIsRegeneratedByAWrite(t *testing.T) {
	for _, c := range []struct{ name, stamp string }{
		{"stamped older", "v0.8.0-2-gbbbbbbb"},
		{"unstamped", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, host := levelProject(t, "v0.8.0-5-gaaaaaaa")
			before := restamp(t, host, c.stamp)

			var err error
			stderr := captureErr(t, func() { capture(t, func() { err = execute(t, "status") }) })
			if err != nil || !strings.Contains(stderr, "warning: AGENTS.md") || !strings.Contains(stderr, "an older drover wrote it") {
				t.Fatalf("status = %v, stderr %q; want a warning naming AGENTS.md and exit 0", err, stderr)
			}
			if after, _ := os.ReadFile(host); !bytes.Equal(before, after) { // #nosec G304 -- the fixture's own file
				t.Error("status changed AGENTS.md")
			}

			stderr, err = runNewBug(t)
			if err != nil || !strings.Contains(stderr, "AGENTS.md") {
				t.Fatalf("new = %v, stderr %q; want it to regenerate AGENTS.md and run", err, stderr)
			}
			after, _ := os.ReadFile(host) // #nosec G304 -- the fixture's own file
			if strings.Contains(string(after), "An instruction from last month.") || !strings.Contains(string(after), "Keep this.") {
				t.Errorf("new did not regenerate the block, or lost the text around it:\n%s", after)
			}
		})
	}
}

// A block a newer binary wrote is refused to a write, which would hand agents
// the older surface, and read past with a warning.
func TestANewerBlockRefusesAWriteAndWarnsARead(t *testing.T) {
	_, host := levelProject(t, "v0.8.0-5-gaaaaaaa")
	before := restamp(t, host, "v0.9.0")

	_, err := runNewBug(t)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "install the newer drover") {
		t.Fatalf("new = %v, want exit 3 naming the newer binary", err)
	}
	var serr error
	stderr := captureErr(t, func() { capture(t, func() { serr = execute(t, "status") }) })
	if serr != nil || !strings.Contains(stderr, "a newer drover wrote its generated block") {
		t.Errorf("status = %v, stderr %q; want a warning and exit 0", serr, stderr)
	}
	if after, _ := os.ReadFile(host); !bytes.Equal(before, after) { // #nosec G304 -- the fixture's own file
		t.Error("a refused write or a read changed AGENTS.md")
	}
}

// The commands that repair staleness, or answer without a project, run over a
// stale one, and so does anything inside a session drover launched: the
// operator already decided when they started the drive, and an agent's
// `drover audit` refused mid-review would lose the review.
func TestTheRepairsAndLaunchedSessionsAreNeverRefused(t *testing.T) {
	asRelease(t, "v0.8.0")
	staleProject(t)
	for _, args := range [][]string{
		{"agents"}, {"config", "--diff"}, {"version"}, {"schema"}, {"help", "status"},
	} {
		var err error
		capture(t, func() { err = execute(t, args...) })
		if Code(err) == ExitRefused {
			t.Errorf("%v refused over a stale config: %v", args, err)
		}
	}

	t.Setenv(runner.EnvActor, runner.ActorAgent)
	var err error
	capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Errorf("status inside a launched session ended %v", err)
	}
}

// An addendum edit stales the block like a config change, and the refusal says
// the addendum moved rather than blaming the binary. One that cannot be read is
// check's to report, so the comparison is skipped rather than refusing.
func TestAnEditedAddendumWarnsTheCommand(t *testing.T) {
	asRelease(t, devVersion)
	s := addendumProject(t)
	t.Chdir(s.cfg.Root)
	captureErr(t, func() {
		if err := execute(t, "agents", "--write", "AGENTS.md"); err != nil {
			t.Fatal(err)
		}
	})
	var err error
	capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Fatalf("status over freshly written instructions ended %v", err)
	}

	notes := filepath.Join(s.cfg.Root, "notes.md")
	write(t, notes, addendumText+"\nA new convention.\n")
	stderr := captureErr(t, func() { capture(t, func() { err = execute(t, "status") }) })
	if err != nil {
		t.Fatalf("status over an edited addendum ended %v, want a warning and exit 0", err)
	}
	for _, want := range []string{"[agents] addendum " + notes, "drover agents --write AGENTS.md"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "differs from what this binary renders") {
		t.Errorf("the warning blames the binary for an addendum edit:\n%s", stderr)
	}

	if err := os.Remove(notes); err != nil {
		t.Fatal(err)
	}
	capture(t, func() { err = execute(t, "status") })
	if Code(err) == ExitRefused {
		t.Errorf("status refused over an unreadable addendum, which check reports: %v", err)
	}
}
