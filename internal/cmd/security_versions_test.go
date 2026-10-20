package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The supported-versions table in SECURITY.md is the four-second answer to
// "does my version still get fixes", and without this test the only thing
// holding it to the releases is somebody remembering. A release that ships with
// the table still naming the previous minor promises fixes for a minor the prose
// directly above it has retired.
//
// The judge is the newest tag reachable from the commit under test. This runs
// under `make check`, which .github/workflows/release.yml runs after checking
// out at fetch-depth: 0 and before `make dist` — so a tag pushed over a stale
// table fails the gate and publishes nothing, instead of being caught on
// whichever pull request comes next.
//
// Maintenance, when this fails: edit the two rows the failure prints. Never
// this file; it names no version.
const (
	securityDoc     = "SECURITY.md"
	securityHeading = "## Supported versions"
)

var (
	tagPattern   = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)
	minorPattern = regexp.MustCompile(`^v(\d+)\.(\d+)$`)
)

// A major.minor pair, held as numbers because the strings sort wrong: "v0.10"
// precedes "v0.6" lexically, and this project reaches v0.10 on its cadence.
type release struct{ major, minor int }

func (r release) String() string { return fmt.Sprintf("v%d.%d", r.major, r.minor) }

func (r release) before(o release) bool {
	if r.major != o.major {
		return r.major < o.major
	}
	return r.minor < o.minor
}

// succeeds reports whether r is the very next minor after prev, counting a
// major bump as the successor of anything: v0.9 is followed by v0.10 or v1.0.
func (r release) succeeds(prev release) bool {
	return (r.major == prev.major && r.minor == prev.minor+1) ||
		(r.major == prev.major+1 && r.minor == 0)
}

func parseRelease(m []string) release {
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return release{major, minor}
}

// A row of the table, reduced to what the policy is about: which versions, and
// whether they get fixes. The right-hand cell's wording is nobody's contract.
type versionRow struct {
	version   string
	supported bool
}

func (r versionRow) String() string {
	if r.supported {
		return r.version + " supported"
	}
	return r.version + " unsupported"
}

func rowList(rows []versionRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.String()
	}
	return strings.Join(out, ", ")
}

// The table the policy prescribes for one minor: that minor, and a single
// catch-all below it.
func rowsFor(r release) []versionRow {
	return []versionRow{{r.String() + ".x", true}, {"< " + r.String(), false}}
}

func fixTo(r release) string {
	return fmt.Sprintf("| %s.x | Yes |\n| < %s | No — upgrade |", r, r)
}

// tableRelease reads the one minor the table names, and reports false when the
// table is some other shape than the policy's two rows.
func tableRelease(rows []versionRow) (release, bool) {
	if len(rows) == 0 || !rows[0].supported {
		return release{}, false
	}
	m := minorPattern.FindStringSubmatch(strings.TrimSuffix(rows[0].version, ".x"))
	if m == nil {
		return release{}, false
	}
	r := parseRelease(m)
	return r, slices.Equal(rows, rowsFor(r))
}

// tableProblem reports why the table may not stand against tag, or "" when it
// may. The comparison is directional on purpose: the runbook has a minor bump
// move these rows on the pull request that precedes the tag, so a table naming
// the minor being cut is a release in progress rather than drift. Only a table
// left behind goes stale by inaction, and that is the failure this file exists
// for. A table several minors ahead is neither, so it is called a typo.
func tableProblem(tag string, rows []versionRow) string {
	cur := parseRelease(tagPattern.FindStringSubmatch(tag))
	table, ok := tableRelease(rows)
	header := fmt.Sprintf("  newest tag: %s\n  table says: %s\n", tag, rowList(rows))
	switch {
	case !ok:
		return fmt.Sprintf("%s: the %q table is not one supported minor with a single catch-all below it.\n%s"+
			"Fix: in %s, replace the table rows with:\n\n%s",
			securityDoc, securityHeading, header, securityDoc, fixTo(cur))
	case table.before(cur):
		return fmt.Sprintf("%s has drifted from the newest tag.\n%s  policy is:  %s\n"+
			"Fix: in %s, replace the table rows with:\n\n%s",
			securityDoc, header, rowList(rowsFor(cur)), securityDoc, fixTo(cur))
	case table == cur || table.succeeds(cur):
		return ""
	default:
		return fmt.Sprintf("%s names a minor no tag is near.\n%s  allowed is: %s as shipped, or the next minor once you are cutting it.\n"+
			"Fix: if this is a typo, in %s replace the table rows with:\n\n%s",
			securityDoc, header, cur, securityDoc, fixTo(cur))
	}
}

// No tag is a broken checkout rather than a reason to pass: a shallow clone
// reaches none, which is why both workflows check out at fetch-depth: 0. Only a
// tree with no git behind it at all skips.
func newestReachableTag(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	git := func(args ...string) *exec.Cmd {
		return exec.Command("git", append([]string{"-C", root}, args...)...) // #nosec G204 -- fixed arguments
	}
	if err := git("rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("no git work tree above this package; nothing to read a tag from")
	}
	out, err := git("tag", "--list", "v*", "--sort=-v:refname", "--merged", "HEAD").Output()
	if err != nil {
		t.Fatalf("git tag: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if tag := strings.TrimSpace(line); tagPattern.MatchString(tag) {
			return tag
		}
	}
	t.Fatal("no vX.Y.Z tag is reachable from HEAD, so the table has nothing to be checked against.\n" +
		"Fix: git fetch --tags, or check out at fetch-depth: 0")
	return ""
}

func supportedVersionsTable(t *testing.T) []versionRow {
	t.Helper()
	text := read(t, filepath.Join(repoRoot(t), securityDoc))
	i := strings.Index(text, securityHeading)
	if i < 0 {
		t.Fatalf("%s has no %q section", securityDoc, securityHeading)
	}
	section := text[i+len(securityHeading):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	var rows []versionRow
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("%s: row %q is not two cells", securityDoc, line)
		}
		version, support := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])
		switch {
		case version == "Version" || strings.HasPrefix(version, "---"):
			continue
		case strings.HasPrefix(support, "Yes"):
			rows = append(rows, versionRow{version, true})
		case strings.HasPrefix(support, "No"):
			rows = append(rows, versionRow{version, false})
		default:
			t.Fatalf("%s: row %q answers neither Yes nor No", securityDoc, line)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("%s: the %q section holds no table rows", securityDoc, securityHeading)
	}
	return rows
}

func TestSupportedVersionsTableNamesTheCurrentMinor(t *testing.T) {
	if msg := tableProblem(newestReachableTag(t), supportedVersionsTable(t)); msg != "" {
		t.Errorf("\n%s", msg)
	}
}

func TestSupportedVersionsCheckIsDirectional(t *testing.T) {
	cases := []struct {
		name  string
		tag   string
		table []versionRow
		want  string // a phrase from the expected failure, empty for a table that may stand
	}{
		{"two minors behind", "v0.6.0", rowsFor(release{0, 4}), "has drifted"},
		{"the shipped minor", "v0.6.0", rowsFor(release{0, 6}), ""},
		{"the minor being cut", "v0.6.0", rowsFor(release{0, 7}), ""},
		{"the major being cut", "v0.9.0", rowsFor(release{1, 0}), ""},
		{"three minors ahead", "v0.6.0", rowsFor(release{0, 9}), "no tag is near"},
		{"v0.10 released, table on v0.6", "v0.10.0", rowsFor(release{0, 6}), "has drifted"},
		{"v0.10 being cut", "v0.9.0", rowsFor(release{0, 10}), ""},
		{"one row", "v0.6.0", []versionRow{{"v0.6.x", true}}, "not one supported minor"},
		{"two supported minors", "v0.6.0", []versionRow{{"v0.6.x", true}, {"v0.5.x", true}}, "not one supported minor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tableProblem(tc.tag, tc.table)
			switch {
			case tc.want == "" && got != "":
				t.Errorf("table %s against %s should stand, got:\n%s", rowList(tc.table), tc.tag, got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("table %s against %s should fail with %q, got:\n%s", rowList(tc.table), tc.tag, tc.want, got)
			}
		})
	}
}

// `go test -run` that matches nothing exits 0, so the Makefile line that
// re-runs this file's check past the go test cache goes vacuous the moment the
// test is renamed, with the gate reading a cached stale pass and nothing
// saying so. docs/releasing.md carries the name too.
func TestCacheBustNamesATestThatExists(t *testing.T) {
	root := repoRoot(t)
	m := regexp.MustCompile(`go test -count=1 -run (\w+)`).FindStringSubmatch(read(t, filepath.Join(root, "Makefile")))
	if m == nil {
		t.Fatalf("the Makefile no longer re-runs any test with `go test -count=1 -run <name>`.\n" +
			"Without it, a tag created after a passing run gets that run's cached result:\n" +
			"`git tag` is a subprocess, so it is not a cache input.")
	}
	name := m[1]
	pkg := filepath.Join(root, "internal", "cmd")
	entries, err := os.ReadDir(pkg)
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") && strings.Contains(read(t, filepath.Join(pkg, e.Name())), "func "+name+"(") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the Makefile re-runs %s past the go test cache, but no test in internal/cmd has that name,\n"+
			"so the line matches nothing, exits 0, and the check is off.\n"+
			"Fix: put the new name in the Makefile `test` target and in docs/releasing.md.", name)
	}
	if runbook := filepath.Join("docs", "releasing.md"); !strings.Contains(read(t, filepath.Join(root, runbook)), name) {
		t.Errorf("%s no longer names %s, so the release runbook describes a check by a name that is not the check's.\n"+
			"Fix: name the test the Makefile re-runs.", runbook, name)
	}
}
