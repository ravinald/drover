package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// ignoreStale is -I: run the command over files the stale check refuses.
var ignoreStale bool

// staleExempt are the top-level commands that never refuse on staleness: the
// ones that repair it (agents, config), and the ones that answer without a
// project or about the binary itself. A check that refused `agents --write`
// would refuse the fix it names.
var staleExempt = map[string]bool{
	"agents": true, "config": true, "init": true, "schema": true,
	"version": true, "help": true, "completion": true, "projects": true,
	cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// staleFile is one project file the running binary has moved past, with the
// command that brings it level.
type staleFile struct {
	Name string
	// Kind is the finding a policy change raises, empty for every other one.
	Kind string
	Why  string
	Fix  string
}

// releaseVer is a version reduced to what staleness compares. Patch releases fix
// behavior without adding anything a config or the agent instructions would
// need to say, so a patch bump alone never makes a project stale.
type releaseVer struct{ major, minor, patch int }

// releasePrefix reads vX.Y.Z off the front of a version. `make install` from a
// work tree stamps git describe's form (v0.1.0-35-g8b16d75-dirty), which is
// that release plus commits, and compares as the release it builds on.
var releasePrefix = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

func parseReleaseVer(v string) (releaseVer, bool) {
	m := releasePrefix.FindStringSubmatch(v)
	if m == nil {
		return releaseVer{}, false
	}
	var r releaseVer
	r.major, _ = strconv.Atoi(m[1])
	r.minor, _ = strconv.Atoi(m[2])
	r.patch, _ = strconv.Atoi(m[3])
	return r, true
}

func (r releaseVer) String() string { return fmt.Sprintf("v%d.%d.%d", r.major, r.minor, r.patch) }

func (r releaseVer) olderMinor(than releaseVer) bool {
	return r.major < than.major || (r.major == than.major && r.minor < than.minor)
}

// checkStale refuses a command whose project carries a config older than this
// binary, or agent instructions other than it renders, before the command does
// anything. The header names no direction: a block a newer binary wrote (a
// branch's build run in the checkout) differs exactly as an old one does.
//
// The two have different failure modes, and both are silent. A config written
// against an older release runs with a zero value for every key added since,
// which reads exactly like a decision. Instructions rendered by an older binary
// tell every agent that reads them about a command surface that has moved, and
// the agent believes them. Neither ever errors, so the check runs on every
// command rather than inside `drover check`, where nobody looks until something
// has gone wrong.
//
// It answers nothing about a project it cannot load: that command's own load
// reports the problem better than this would.
func checkStale(c *cobra.Command) error {
	if ignoreStale || staleExempt[topLevel(c)] || runner.Launched() {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return nil
	}
	found := staleFiles(cfg)
	if len(found) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "this project's files differ from what this binary renders (%s):\n", versionLine())
	for _, f := range found {
		why := f.Why
		if f.Kind != "" {
			why = f.Kind + ": " + why
		}
		fmt.Fprintf(&b, "  %s: %s\n    fix: %s\n", f.Name, why, f.Fix)
	}
	fmt.Fprintf(&b, "Nothing was run. Bring them level, or pass -I (--ignore-stale) to run %s anyway", c.CommandPath())
	return Refusal{fmt.Errorf("%s", b.String())}
}

// topLevel is the command directly under root, so `completion zsh` and
// `help status` are judged as completion and help.
func topLevel(c *cobra.Command) string {
	for c.HasParent() && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c.Name()
}

// staleFiles is every file checkStale would name: the config, then each
// root-level file carrying the generated agent block.
func staleFiles(cfg *config.Config) []staleFile {
	var out []staleFile
	if f, ok := staleConfig(cfg); ok {
		out = append(out, f)
	}
	s := &session{cfg: cfg}
	hosts, err := findAgentsHosts(cfg.Root)
	if err != nil || len(hosts) == 0 {
		return out
	}
	// An addendum that will not render skips the comparison rather than
	// refusing: check reports it, and refusing every command over a missing
	// notes file would refuse check too. Malformed markers need no render.
	want, renderErr := s.currentBlock()
	for _, h := range hosts {
		if h.Malformed {
			out = append(out, staleFile{Name: h.Name,
				Why: "its drover markers are out of order, so nothing can tell which text is generated",
				Fix: "fix the markers by hand, then drover agents --write " + h.Name})
			continue
		}
		// A policy change also moves the text, and the policy finding names the
		// same fix plus the --yes a widening needs, so it stands in for the
		// stale block rather than beside it.
		if f, ok := stalePolicy(cfg, h.Name, h.Block); ok {
			out = append(out, f)
			continue
		}
		switch {
		case renderErr == nil && h.Block != want:
			out = append(out, staleFile{Name: h.Name, Why: s.staleWhy(h.Block),
				Fix: "drover agents --write " + h.Name + " when this binary is the newer one; " +
					"rebuild or reinstall drover when a newer one wrote the file"})
		}
	}
	return out
}

// staleWhy says which part of a stale block moved. An addendum is edited far
// more often than the binary or the config changes, and a reason blaming the
// binary for a notes edit sends the reader looking for an upgrade.
func (s *session) staleWhy(block string) string {
	const moved = "its generated block differs from what this binary renders, so agents reading it are told about a surface that has moved"
	addendum, err := s.readAddendum()
	if err != nil || addendum == "" {
		return moved
	}
	generated, err := s.generatedAgents()
	if err != nil {
		return moved
	}
	edited := fmt.Sprintf("[agents] addendum %s changed since it was generated, so agents reading it are missing the edit",
		s.cfg.AddendumPath())
	section := strings.TrimSpace(addendumSection(addendum))
	switch genOK, addOK := strings.Contains(block, generated+"\n\n"+addendumHeading+"\n"), strings.HasSuffix(block, "\n"+section); {
	case genOK && !addOK:
		return edited
	case !genOK && !addOK:
		return edited + "; " + moved
	default:
		return moved
	}
}

// staleConfig compares the project's drover_version stamp against this
// binary's release. A binary with no release version (a plain `go build`)
// has nothing to compare and reports nothing.
func staleConfig(cfg *config.Config) (staleFile, bool) {
	bin, ok := parseReleaseVer(version)
	if !ok {
		return staleFile{}, false
	}
	name := config.FileName
	fix := "drover config --diff, decide what to declare, then drover config --stamp"
	if !cfg.ProjectDeclared("drover_version") {
		return staleFile{Name: name, Fix: fix,
			Why: "it declares no drover_version, so nothing says which release it was last reviewed against"}, true
	}
	stamp, ok := parseReleaseVer(cfg.DroverVersion)
	if !ok {
		return staleFile{Name: name, Fix: "drover config --stamp",
			Why: fmt.Sprintf("drover_version %q is not a release version (vX.Y.Z)", cfg.DroverVersion)}, true
	}
	if stamp.olderMinor(bin) {
		return staleFile{Name: name, Fix: fix,
			Why: fmt.Sprintf("it was last reviewed against %s, and this binary is %s", stamp, bin)}, true
	}
	return staleFile{}, false
}

// stampLine matches a top-level drover_version assignment. Only lines above the
// first table header are top-level in TOML, which stampConfig enforces by
// looking no further.
var stampLine = regexp.MustCompile(`^\s*drover_version\s*=`)

// stampConfig records that the config was reviewed against stamp, rewriting the
// one line and nothing else. It is a line edit rather than a TOML round-trip,
// which would flatten the comments that carry a project's reasoning.
func stampConfig(path, stamp string) error {
	b, err := os.ReadFile(path) // #nosec G304 -- the project's own config
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("drover_version = %q", stamp)
	lines := strings.Split(string(b), "\n")
	at, after := -1, 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			break
		}
		if stampLine.MatchString(l) {
			at = i
			break
		}
		if strings.HasPrefix(t, "schema_version") {
			after = i + 1
		}
	}
	if at >= 0 {
		lines[at] = line
	} else {
		lines = append(lines[:after], append([]string{line}, lines[after:]...)...)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), st.Mode().Perm())
}

// releaseStamp is the version a stamp records: this binary's release, without
// git describe's commit suffix. False on a build that carries none.
func releaseStamp() (string, bool) {
	r, ok := parseReleaseVer(version)
	if !ok {
		return "", false
	}
	return r.String(), true
}
