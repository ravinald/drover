package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// forceColor sets --color for the test and puts auto back after it, since the
// mode is package state every later execute would inherit.
func forceColor(t *testing.T, mode string) {
	t.Helper()
	colorMode = mode
	t.Cleanup(func() {
		colorMode = colorAuto
		root.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	})
}

var dateOnlyRe = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

var sgrSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripSGR(s string) string { return sgrSeq.ReplaceAllString(s, "") }

// The precedence, highest first: the flag, NO_COLOR, CLICOLOR_FORCE, TERM=dumb,
// then whether the stream is a terminal.
func TestColorForPrecedence(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	restore := onTerminal
	t.Cleanup(func() { onTerminal = restore })

	for _, c := range []struct {
		name                     string
		mode                     string
		noColor, force, termName string
		tty                      bool
		want                     bool
	}{
		{name: "a terminal", mode: colorAuto, termName: "xterm", tty: true, want: true},
		{name: "a pipe", mode: colorAuto, termName: "xterm", want: false},
		{name: "always beats NO_COLOR", mode: colorAlways, noColor: "1", want: true},
		{name: "always on a pipe", mode: colorAlways, want: true},
		{name: "never beats CLICOLOR_FORCE on a terminal", mode: colorNever, force: "1", tty: true, want: false},
		{name: "NO_COLOR beats CLICOLOR_FORCE", mode: colorAuto, noColor: "1", force: "1", tty: true, want: false},
		{name: "CLICOLOR_FORCE colors a pipe", mode: colorAuto, force: "1", want: true},
		{name: "CLICOLOR_FORCE=0 forces nothing", mode: colorAuto, force: "0", want: false},
		{name: "CLICOLOR_FORCE beats TERM=dumb", mode: colorAuto, force: "1", termName: "dumb", want: true},
		{name: "TERM=dumb beats a terminal", mode: colorAuto, termName: "dumb", tty: true, want: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			forceColor(t, c.mode)
			t.Setenv("NO_COLOR", c.noColor)
			t.Setenv("CLICOLOR_FORCE", c.force)
			t.Setenv("TERM", c.termName)
			tty := c.tty
			onTerminal = func(*os.File) bool { return tty }
			if got := colorFor(w); got != c.want {
				t.Errorf("colorFor = %v, want %v", got, c.want)
			}
			if got := links(w); got != c.want {
				t.Errorf("links = %v, want %v: links follow the color decision", got, c.want)
			}
		})
	}
}

// Each stream answers for itself: a drive piped to a file keeps color on the
// terminal its stderr still reaches.
func TestColorIsDecidedPerStream(t *testing.T) {
	forceColor(t, colorAuto)
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("TERM", "xterm")
	_, out, _ := os.Pipe()
	_, errs, _ := os.Pipe()
	restore := onTerminal
	t.Cleanup(func() { onTerminal = restore })
	onTerminal = func(f *os.File) bool { return f == errs }
	if colorFor(out) || !colorFor(errs) {
		t.Errorf("stdout %v, stderr %v: want stdout plain and stderr colored", colorFor(out), colorFor(errs))
	}
}

func TestColorFlagParses(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--color=always", "version"}, colorAlways},
		{[]string{"--no-color", "version"}, colorNever},
		{[]string{"--color", "always", "--no-color", "version"}, colorNever},
		{[]string{"--no-color", "--color=always", "version"}, colorAlways},
	} {
		forceColor(t, colorAuto)
		_ = capture(t, func() {
			if err := execute(t, c.args...); err != nil {
				t.Fatalf("%v: %v", c.args, err)
			}
		})
		if colorMode != c.want {
			t.Errorf("%v: mode %q, want %q", c.args, colorMode, c.want)
		}
	}
	forceColor(t, colorAuto)
	if err := execute(t, "--color=sometimes", "version"); err == nil || !strings.Contains(err.Error(), "auto, always or never") {
		t.Errorf("--color=sometimes: %v, want a refusal naming the three modes", err)
	}
}

// The stream styler colors a line by what it reports, and colors nothing on a
// stream that takes no color.
func TestStyleLineRoles(t *testing.T) {
	forceColor(t, colorAuto)
	install()
	for _, c := range []struct {
		line, role string
	}{
		{"==> drive · 2 items: 1 2", pal.Heading},
		{"· reconciled: 0 finished, 0 merged without a drover launch", pal.Detail},
		{"  · 2: parked silent (unknown)", pal.Fail},
		{"· paused by drive --pause: nothing more is dispatched", pal.Warn},
		{"· 4 merged since the last drive; finishing it", pal.OK},
		{"  ! 4 implements and reviews with one runner (claude). Driving anyway.", pal.Warn},
		{"  · 4: run exited 2 (exit status 2); asking the ledger anyway", pal.Fail},
		{"  · 4: the verify command exited 0 after 3s", pal.Detail},
		{"· 10 parked since the last drive", pal.Fail},
		{"· 20 failed since the last drive", pal.Fail},
		{"· 0 parked since the last drive", pal.Detail},
	} {
		got := styleLine(c.line)
		if !strings.HasPrefix(got, "\x1b["+c.role+"m") {
			t.Errorf("%q styled %q, want it opened in %q", c.line, got, c.role)
		}
		if stripSGR(got) != c.line {
			t.Errorf("%q styled to %q, which reads differently without its escapes", c.line, stripSGR(got))
		}
	}
	got := styleLine("Next steps: drover drive --resume to continue, then drover finish B4 --force.")
	for _, want := range []string{
		sgr("drover drive --resume", pal.Command, true) + " to continue",
		sgr("drover finish B4 --force", pal.Command, true) + ".",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("styled %q, want it to carry %q", got, want)
		}
	}
	for _, line := range []string{
		"==> 2 · implement · branch second · round 1",
		"  ! 2 parked no_record at abc: the review recorded nothing",
		"==> drive · 2 items: 1 2",
	} {
		if got := styleLine(line); !strings.Contains(got, sgr("2", pal.ID, true)) {
			t.Errorf("styled %q, want 2 in id", got)
		}
	}
	if got := styleLine("==> drive · 2 items: 1 2"); !strings.Contains(got, sgr("1", pal.ID, true)) ||
		!strings.Contains(got, "\x1b[1m==> drive · 2 items: ") {
		t.Errorf("styled %q, want each listed id in id and the count left alone", got)
	}
	if got := styleLine("· 4 merged since the last drive"); strings.Contains(got, sgr("4", pal.ID, true)) {
		t.Errorf("styled %q, want the count left out of id", got)
	}
	if got := styleLine("  Next steps: nothing yet: the next drover drive finishes it"); strings.Contains(got, pal.Command+"m") {
		t.Errorf("styled %q, want prose naming a drive left uncolored", got)
	}
	if got := styleLine("· B84 is merged, but drover never launched it"); !strings.Contains(got, sgr("B84", pal.ID, true)) ||
		strings.Contains(got, pal.Command+"m") {
		t.Errorf("styled %q: want B84 as an id and drover never as prose", got)
	}
}

// Requirement 3: none of these carries an escape byte, whatever the flag says.
func TestNeverColoredUnderColorAlways(t *testing.T) {
	resetStatusFlags(t)
	forceColor(t, colorAlways)
	root, events := twoDrives(t)
	if strings.Contains(events, "\x1b[") {
		t.Errorf("drive --events carried an escape:\n%q", events)
	}

	for _, args := range [][]string{
		{"--color=always", "status", "--json", "--no-tracker"},
		{"--color=always", "show", "2", "--json", "--no-tracker"},
		{"--color=always", "drives", "--json"},
		{"--color=always", "check", "--json"},
		{"--color=always", "attach", "D2", "--json"},
	} {
		resetStatusFlags(t)
		out := capture(t, func() { _ = execute(t, args...) })
		if out == "" || strings.Contains(out, "\x1b[") {
			t.Errorf("%v printed %q, want JSON with no escape", args, out)
		}
	}

	resetStatusFlags(t)
	outFile := filepath.Join(t.TempDir(), "status.json")
	_ = capture(t, func() {
		if err := execute(t, "--color=always", "status", "--json", "--no-tracker", "-o", outFile); err != nil {
			t.Fatalf("status -o: %v", err)
		}
	})
	resetStatusFlags(t)
	agents := filepath.Join(t.TempDir(), "AGENTS.md")
	write(t, agents, agentsBegin+"\n"+agentsEnd+"\n")
	_ = capture(t, func() { _ = execute(t, "--color=always", "agents", "--write", agents) })

	files := []string{outFile, agents, filepath.Join(root, "ledger.jsonl")}
	reports, _ := filepath.Glob(filepath.Join(root, "reports", "*"))
	files = append(files, reports...)
	for _, path := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own files
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "\x1b[") {
			t.Errorf("%s carries an escape under --color=always", path)
		}
	}

	body := noticeBody(DriveEvent{Event: evParked, ID: "2", Park: "silent", Reason: "the review recorded nothing"})
	if strings.Contains(body, "\x1b[") {
		t.Errorf("a notification body carries an escape: %q", body)
	}

	if got := detachArgs([]string{"drive", "--detach", "--color=always"}); got[len(got)-1] != "--color=never" {
		t.Errorf("the detached drive runs with %v, want --color=never last so its log is plain", got)
	}
}

// Requirements 7 and 8: each surface renders its golden in both variants, and
// the colored one, stripped of its escapes, is the plain one byte for byte.
// DROVER_UPDATE_GOLDEN=1 rewrites them. Named for drive so normalizeDrive
// masks its temporary paths.
func TestDriveColorGoldens(t *testing.T) {
	resetStatusFlags(t)
	t.Setenv("COLUMNS", "100")
	mergeProject(t)
	plainDrive := goldenDrive(t, "--jobs", "1", "--color=never")
	mergeProject(t)
	coloredDrive := coloredGoldenDrive(t, "--jobs", "1", "--color=always")
	forceColor(t, colorAuto)
	checkGolden(t, "drive-stream", plainDrive, coloredDrive)
	if want, err := os.ReadFile(filepath.Join(goldenDir(t), "drive-jobs-1.golden")); err != nil || string(want) != plainDrive {
		t.Errorf("the plain drive stream is not the stream drive-jobs-1.golden holds (%v)", err)
	}

	resetStatusFlags(t)
	twoDrives(t)
	render := func(args ...string) (string, string) {
		run := func(mode string) string {
			resetStatusFlags(t)
			forceColor(t, colorAuto)
			out := capture(t, func() { _ = execute(t, append([]string{"--color=" + mode}, args...)...) })
			for _, c := range root.Commands() {
				if f := c.Flags().Lookup("help"); f != nil {
					_ = f.Value.Set("false")
				}
			}
			return dateOnlyRe.ReplaceAllString(normalizeDrive(out), "<date>")
		}
		return run("never"), run("always")
	}
	for _, c := range []struct {
		name string
		args []string
	}{
		{"status", []string{"status", "--no-tracker"}},
		{"show", []string{"show", "2", "--no-tracker"}},
		{"drives", []string{"drives"}},
		{"check", []string{"check"}},
	} {
		plain, colored := render(c.args...)
		checkGolden(t, c.name, plain, colored)
	}

	// execute discards cobra's own output, so help renders straight to the
	// captured stdout.
	install()
	long, example := drivesCmd.Long, drivesCmd.Example
	help := func(mode string) string {
		forceColor(t, mode)
		// expand appends the Safety line on each render, once per process.
		defer func() { drivesCmd.Long, drivesCmd.Example = long, example }()
		return capture(t, func() {
			drivesCmd.SetOut(os.Stdout)
			defer drivesCmd.SetOut(nil)
			if err := drivesCmd.Help(); err != nil {
				t.Fatal(err)
			}
		})
	}
	checkGolden(t, "help", help(colorNever), help(colorAlways))

	// An error prints through main, which this package cannot run, so the
	// golden is PrintError on stderr as main calls it.
	err := errors.New("no item 9 in work (drover status lists them)")
	forceColor(t, colorNever)
	plain := captureErr(t, func() { PrintError(os.Stderr, err) })
	forceColor(t, colorAlways)
	colored := captureErr(t, func() { PrintError(os.Stderr, err) })
	checkGolden(t, "error", plain, colored)
}

// coloredGoldenDrive is goldenDrive with each escape kept apart from the
// value before it, so masking a path does not swallow the reset that follows.
func coloredGoldenDrive(t *testing.T, args ...string) string {
	t.Helper()
	var stdout string
	stderr := captureErr(t, func() {
		stdout = capture(t, func() {
			if err := execute(t, append([]string{"drive", "--json"}, args...)...); err != nil {
				t.Fatalf("drive: %v", err)
			}
		})
	})
	raw := strings.ReplaceAll("== stderr\n"+stderr+"== stdout\n"+stdout, "\x1b[", " \x1b[")
	return strings.ReplaceAll(normalizeDrive(raw), " \x1b[", "\x1b[")
}

func checkGolden(t *testing.T, name, plain, colored string) {
	t.Helper()
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("%s: the plain variant carries an escape", name)
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Errorf("%s: the colored variant carries no escape", name)
	}
	if stripSGR(colored) != plain {
		t.Errorf("%s: the colored variant without its escapes is not the plain one:\n--- colored, stripped\n%s\n--- plain\n%s",
			name, stripSGR(colored), plain)
	}
	for _, v := range []struct{ file, got string }{
		{"color-" + name + ".plain.golden", plain},
		{"color-" + name + ".color.golden", colored},
	} {
		path := filepath.Join(goldenDir(t), v.file)
		if os.Getenv("DROVER_UPDATE_GOLDEN") != "" {
			write(t, path, v.got)
		}
		want, err := os.ReadFile(path) // #nosec G304 -- the test's own fixture
		if err != nil {
			t.Fatal(err)
		}
		if v.got != string(want) {
			t.Errorf("%s no longer renders its golden:\n--- got\n%q\n--- want\n%q", v.file, v.got, want)
		}
	}
}
