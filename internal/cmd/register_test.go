package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/gh"
)

// The empty-set line and the header each build a sentence around one scope
// string, and a scope that reads right in the id case can read wrong in the
// other ("no open follow-ups all open"). Both lines are read out of the
// command's own output and compared whole: pinning registerScope while the two
// call sites rebuild its wording would let them drift unnoticed.
func TestRegisterScopeReadsAsASentenceInBothMoods(t *testing.T) {
	for _, c := range []struct {
		kind, owner, header, empty string
	}{
		{"follow-ups", "", "open follow-ups:", `no open follow-ups (nothing in owner/name carries the "follow-up" label)`},
		{"follow-ups", "B1", "open follow-ups owned by B1:", `no open follow-ups owned by B1 (nothing in owner/name carries the "follow-up" label)`},
	} {
		args := []string{c.kind}
		name := c.kind
		if c.owner != "" {
			args = append(args, c.owner)
			name += " " + c.owner
		}
		for _, mood := range []struct{ label, register, want string }{
			{"populated", reachability, c.header},
			{"empty", noRegister, c.empty},
		} {
			t.Run(name+" "+mood.label, func(t *testing.T) {
				dir := registerProject(t, ghProject)
				if err := os.WriteFile(filepath.Join(dir, "work", "B01-owner.md"),
					[]byte(body("Owner", "")), 0o600); err != nil {
					t.Fatal(err)
				}
				stubGHIssues(t, stubPATH(t), mood.register)

				var err error
				out := capture(t, func() { err = execute(t, args...) })
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !hasLine(out, mood.want) {
					t.Errorf("%s wrote:\n%s\nwant the whole line %q", name, out, mood.want)
				}
			})
		}
	}
}

// hasLine compares whole lines, because the likely mutation at these two call
// sites keeps the scope string and appends to it, which a substring match passes.
func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// registerProject writes a project whose only variable is its [github] block,
// so anything that differs between two of them is caused by that block.
func registerProject(t *testing.T, github string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
reports = "reports"

[ids]
namespaces = { B = "bug" }

# The generated default, spelled out because declaring either list replaces it
# whole. It is here so these fixtures test the register rather than the key: a
# project owner issues with no closes key is NO CLOSES KEY's finding, and it
# would fire in every case here that puts a row on an owner.
[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "closes", "severity", "skills", "agent"]
shape = {}

[verify]
default = "true"

# The rest of what drover init writes. Declared so these fixtures assert what
# they are about: a config omitting one is STALE CONFIG's finding, and it would
# fire in every case below that counts findings. shape is empty on purpose —
# shaping a key the lists above do not declare is UNDECLARED KEY's finding. enabled
# is true because a scratch directory is not a work tree and these tests run
# with branch automation on, which is what an unset key already meant here.

[intake]
stop_marker = "STOP: requirements not yet written"

[git]
enabled = true
` + github
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

// stubGHIssues answers the calls a register read makes with a fixed page, on a
// repository that defines the default follow-up label. That is every fixture whose
// subject is the rows rather than the vocabulary they were asked for under.
func stubGHIssues(t *testing.T, dir, issueList string) {
	t.Helper()
	stubGHRegister(t, dir, issueList, `[{"name":"follow-up"},{"name":"severity:low"}]`)
}

// firstLabelPage is what `gh label list` answers when nobody passes --limit:
// the first 30 names, and no more. A repository with more labels than that can
// hold the one the check asks about past index 30, off this page.
// The stub answers with it whenever --limit is absent, which is what turns
// gh.DefinesLabel's --limit from a line a reader has to notice into a line whose
// deletion fails a test.
func firstLabelPage() string {
	names := make([]string, 0, 30)
	for i := range 30 {
		names = append(names, fmt.Sprintf(`{"name":"pad%02d"}`, i))
	}
	return "[" + strings.Join(names, ",") + "]"
}

// stubGHRegister answers both halves of the read: the page, and the label set
// check consults when the page came back empty. Anything else exits non-zero,
// so a call these fixtures do not model fails here rather than reaching the
// network. A labelList of "" is a label call that fails, which is the degraded
// path: drover cannot tell an empty register from a typo'd one either way, and
// it has to say so rather than guess.
//
// The label arm answers labelList only when --limit is on the command line, and
// a truncated first page otherwise, so a caller that forgets the flag reads a
// page the label it asked about fell off.
func stubGHRegister(t *testing.T, dir, issueList, labelList string) {
	t.Helper()
	labels := "'label list') echo \"stub gh: the label set is unavailable\" >&2; exit 1 ;;\n"
	if labelList != "" {
		t.Setenv("DROVER_TEST_GH_LABELS", labelList)
		labels = "'label list') case \"$*\" in\n" +
			"*--limit*) printf '%s' \"$DROVER_TEST_GH_LABELS\" ;;\n" +
			"*) printf '%s' '" + firstLabelPage() + "' ;;\n" +
			"esac ;;\n"
	}
	t.Setenv("DROVER_TEST_GH_ISSUES", issueList)
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') printf '%s' \"$DROVER_TEST_GH_ISSUES\" ;;\n" +
		labels +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	stub(t, filepath.Join(dir, "gh"), script)
}

// onePage is a register of exactly one row, which fills a page_limit of 1. A
// truncation note printed after the row loop is one an early return for the
// empty set never reaches.
const onePage = `[{"number":13,"title":"go install reports the tag but no commit",` +
	`"body":"","state":"OPEN","labels":[{"name":"follow-up"},{"name":"severity:low"}]}]`

// noRegister is a page with nothing on it, which is the one fixture that
// reaches the empty-set line in both moods: onePage still answers with a row.
const noRegister = `[]`

func TestTruncatedReadIsReportedEvenWithNoMatchingRows(t *testing.T) {
	registerProject(t, "[github]\nrepo = \"owner/name\"\npage_limit = 1\n")
	stubGHIssues(t, stubPATH(t), onePage)

	note := "note: hit the 1-issue page limit; there may be more"
	var err error
	out := capture(t, func() { err = execute(t, "follow-ups") })
	if err != nil {
		t.Fatalf("follow-ups: %v", err)
	}
	if !strings.Contains(out, "#13") {
		t.Errorf("follow-ups wrote:\n%s\nwant it to contain #13", out)
	}
	if !strings.Contains(out, note) {
		t.Errorf("follow-ups answered without naming the page it lost:\n%s", out)
	}
}

// A wrapper is told to parse the JSON rather than the table, so the fact the
// prose carries has to be in the JSON too. A bare array cannot hold it, which
// is why the rows sit under a key.
func TestTruncatedReadIsReportedInJSON(t *testing.T) {
	registerProject(t, "[github]\nrepo = \"owner/name\"\npage_limit = 1\n")
	stubGHIssues(t, stubPATH(t), onePage)
	t.Cleanup(func() { registerJSON = false })

	var err error
	out := capture(t, func() { err = execute(t, "follow-ups", "--json") })
	if err != nil {
		t.Fatalf("follow-ups --json: %v", err)
	}
	var got RegisterReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got.Rows) != 1 {
		t.Errorf("rows = %v, want the one row on the page", got.Rows)
	}
	if !got.Truncated || got.PageLimit != 1 {
		t.Errorf("truncated = %v, page_limit = %d; want true, 1 — a full page is a page nobody finished",
			got.Truncated, got.PageLimit)
	}
}

// gh infers a repository from the project root's git remote, so a project
// nested inside another one reads its parent's register. PATH carries no gh at
// all here: reaching the tracker would error, so success proves nothing asked.
func TestNoTrackerReadsNoRegister(t *testing.T) {
	registerProject(t, "[register]\nbackend = \"none\"\n")
	stubPATH(t)
	want := `no follow-ups: fixture declares [register] backend = "none"`
	var err error
	out := capture(t, func() { err = execute(t, "follow-ups") })
	if err != nil {
		t.Fatalf("follow-ups: %v", err)
	}
	if !strings.Contains(out, want) {
		t.Errorf("follow-ups wrote:\n%s\nwant it to contain %q", out, want)
	}
}

// The summary line is where the inherited counts showed up: `cd examples &&
// drover status` appended the enclosing repository's register.
func TestNoTrackerCountsNoRegisterInStatus(t *testing.T) {
	registerProject(t, "[register]\nbackend = \"none\"\n")
	stubPATH(t)
	var err error
	out := capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, unwanted := range []string{"open follow-ups", "page limit"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("status reported %q from a register it was never given:\n%s", unwanted, out)
		}
	}
}

// The status footer is read every day and the register clause was the one place
// that never learned reachability: 71 dormant, 10 unrouted and zero live
// rendered the same line as thirty live rows with a plan for each. Driven
// through the command rather than the function, because a clause the footer
// never appends is a clause nobody reads.
func TestStatusFooterNamesLiveRowsWhenTheRegisterStopsReachingThem(t *testing.T) {
	for _, c := range []struct {
		name   string
		ledger string
		want   string
		deny   string
	}{
		// B1 merged, B2 open: 1 dormant and 1 unrouted against 1 live.
		{
			name:   "orphaned and unowned outnumber live",
			ledger: `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}`,
			want:   "3 open follow-ups · 1 live (drover follow-ups)",
		},
		// Nothing merged, so only the unrouted row fails to reach a run.
		{
			name:   "live rows in the majority keep the line they had",
			ledger: "",
			want:   "3 open follow-ups (drover follow-ups)",
			deny:   "live",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := registerProject(t, ghProject)
			stubGHIssues(t, stubPATH(t), reachability)
			if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(c.ledger), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			out := capture(t, func() { err = execute(t, "status") })
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("status wrote:\n%s\nwant it to carry %q", out, c.want)
			}
			if c.deny != "" && strings.Contains(out, c.deny) {
				t.Errorf("status widened a healthy register with %q:\n%s", c.deny, out)
			}
		})
	}
}

// A truncated read makes every count a floor, so the line names the cut rather
// than leaving a reader to infer it from a count that is not there.
func TestStatusNamesATruncatedRegister(t *testing.T) {
	full := gh.Index{OK: true, Issues: []gh.Issue{{Number: 1}}}
	got := registerParts(full, 1, registerTally{Open: 1, Live: 1})
	want := []string{
		"1 open follow-up (drover follow-ups)",
		"follow-up list cut off at the 1-issue page limit",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// Unreachable stays omitted: "nobody could ask" is not "nobody filed one".
	if parts := registerParts(gh.Index{}, 1, registerTally{}); parts != nil {
		t.Errorf("an unreadable register produced %q", parts)
	}
	if parts := registerParts(gh.Index{OK: true, Off: true}, 300, registerTally{}); parts != nil {
		t.Errorf("a declared-absent tracker produced %q", parts)
	}
}

// reachability is one register carrying every state a routing label can reach,
// three rows differing only in their routing label. Read against a ledger where
// B1 merged and B2 did not, so anything that separates two rows here is the
// owner's state and nothing else.
const reachability = `[` +
	`{"number":101,"title":"orphaned row","body":"Owner: B1","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:low"}]},` +
	`{"number":102,"title":"live row","body":"Owner: B2","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:low"}]},` +
	`{"number":103,"title":"unowned row","body":"","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}` +
	`]`

// registerRowLine returns the table row for one issue number. A suffix asserted
// against the whole output would pass while it sat on the wrong row, which is
// the confusion this command exists to end.
func registerRowLine(t *testing.T, out string, number int) string {
	t.Helper()
	prefix := fmt.Sprintf("  #%d ", number)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no row for #%d in:\n%s", number, out)
	return ""
}

// A routed row whose owner merged must not print and serialize identically to
// one whose owner is still open, or the register answers "has a label" while
// claiming to answer "reaches a run". Both output paths are driven from one
// fixture: asserting the JSON alone passed while the table still printed one
// sentence for two states.
func TestRegisterSeparatesDormantFromUnroutedAndOpenOwner(t *testing.T) {
	dir := registerProject(t, ghProject)
	stubGHIssues(t, stubPATH(t), reachability)
	rows := `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}
{"id":"B2","merged":false,"implementer":"upstream","date":"2026-01-02"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registerJSON = false })

	const unroutedSuffix = "(unowned — reaches no run)"
	type state struct {
		unrouted, dormant bool
		suffix            string
	}
	for _, c := range []struct {
		cmd     string
		want    map[int]state
		dormant int
		live    []int
	}{
		{"follow-ups", map[int]state{
			101: {dormant: true},
			102: {},
			103: {unrouted: true, suffix: unroutedSuffix},
		}, 101, []int{102, 103}},
	} {
		t.Run(c.cmd+" table", func(t *testing.T) {
			registerJSON = false
			var err error
			out := capture(t, func() { err = execute(t, c.cmd) })
			if err != nil {
				t.Fatalf("%s: %v", c.cmd, err)
			}
			for number, want := range c.want {
				line := registerRowLine(t, out, number)
				if want.suffix == "" {
					if strings.Contains(line, "(orphaned") || strings.Contains(line, "(unowned") {
						t.Errorf("#%d carries a suffix it should not: %q", number, line)
					}
					continue
				}
				if !strings.Contains(line, want.suffix) {
					t.Errorf("#%d printed %q, want it to carry %q", number, line, want.suffix)
				}
			}
			// The state every routed row reaches once its owner merges is counted
			// once rather than stamped on each row it applies to.
			footer := "3 open · 1 orphaned (every owner merged) · 1 unowned"
			if !strings.Contains(out, footer) {
				t.Errorf("%s wrote:\n%s\nwant the footer %q", c.cmd, out, footer)
			}
		})

		t.Run(c.cmd+" live", func(t *testing.T) {
			registerJSON = false
			registerLive = true
			t.Cleanup(func() { registerLive = false })
			var err error
			out := capture(t, func() { err = execute(t, c.cmd, "--live") })
			if err != nil {
				t.Fatalf("%s --live: %v", c.cmd, err)
			}
			if strings.Contains(out, fmt.Sprintf("#%d", c.dormant)) {
				t.Errorf("--live kept orphaned #%d:\n%s", c.dormant, out)
			}
			// The unrouted row survives --live on purpose: nothing owns it, so
			// no owner can have merged, and it is the row most worth seeing.
			for _, number := range c.live {
				if !strings.Contains(out, fmt.Sprintf("#%d", number)) {
					t.Errorf("--live dropped #%d, whose owner has not merged:\n%s", number, out)
				}
			}
		})

		t.Run(c.cmd+" json", func(t *testing.T) {
			registerJSON = false
			var err error
			out := capture(t, func() { err = execute(t, c.cmd, "--json") })
			if err != nil {
				t.Fatalf("%s --json: %v", c.cmd, err)
			}
			var got RegisterReport
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			seen := map[int]RegisterRow{}
			for _, r := range got.Rows {
				seen[r.Number] = r
			}
			for number, want := range c.want {
				row, ok := seen[number]
				if !ok {
					t.Fatalf("no row for #%d in %s --json:\n%s", number, c.cmd, out)
				}
				if row.Unowned != want.unrouted {
					t.Errorf("#%d unowned = %v, want %v — the key predates this field and must not move",
						number, row.Unowned, want.unrouted)
				}
				if row.Orphaned != want.dormant {
					t.Errorf("#%d orphaned = %v, want %v", number, row.Orphaned, want.dormant)
				}
			}
		})
	}
}

// The line that replaces eight repeated suffixes has to say what it hid, or an
// operator reads an empty result as an empty register and stops looking.
func TestLiveNamesWhatItHidWhenEveryRowIsDormant(t *testing.T) {
	dir := registerProject(t, ghProject)
	stubGHIssues(t, stubPATH(t), reachability)
	rows := `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}
{"id":"B2","merged":true,"implementer":"upstream","date":"2026-01-02"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	registerLive = true
	t.Cleanup(func() { registerLive = false })

	var err error
	out := capture(t, func() { err = execute(t, "follow-ups", "--live") })
	if err != nil {
		t.Fatalf("follow-ups --live: %v", err)
	}
	// Two of the three are dormant; the unrouted one is never dormant, so it
	// survives and the empty-set line must not fire.
	if !strings.Contains(out, "#103") {
		t.Errorf("--live dropped the unowned row:\n%s", out)
	}
	if strings.Contains(out, "no live follow-ups") {
		t.Errorf("--live reported nothing live while #103 stands:\n%s", out)
	}
}
