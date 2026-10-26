package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

func scaffoldSession(t *testing.T) *session {
	t.Helper()
	cfg := &config.Config{
		Frontmatter: config.Frontmatter{
			Required: []string{"title", "deps"},
			Optional: []string{"kind", "issue", "severity"},
		},
		Severity: config.Severity{Key: "severity"},
	}
	return &session{cfg: cfg}
}

// The label is an assessment somebody made. Overwriting it with a default
// publishes a guess in the same field, and nothing downstream says which it is.
func TestScaffoldTakesSeverityFromTheIssueLabel(t *testing.T) {
	s := scaffoldSession(t)
	for _, c := range []struct {
		label, kind, want string
	}{
		{"C", "bug", "severity: C"},
		{"Critical", "bug", "severity: C"},
		{"L", "bug", "severity: L"},
		{"H", "enhancement", "severity: H"},
		{"", "bug", "severity: M"},
		{"P0", "bug", "severity: M"},
		{"", "enhancement", "severity:"},
	} {
		got := s.scaffold(c.kind, 7, gh.Issue{Title: "t", Severity: c.label}, "")
		line := ""
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "severity:") {
				line = strings.TrimRight(l, " ")
				break
			}
		}
		if line != c.want {
			t.Errorf("label %q kind %q: scaffolded %q, want %q", c.label, c.kind, line, c.want)
		}
	}
}

// intake is the read-only embarrassment turning into a write: without a tracker
// it labels an issue on whatever repository encloses this tree. Every
// declaration that resolves to no tracker has to reach the same refusal, not
// only the one that predates [register] — a file-backed project has no tracker
// either, and exit 1 from the gh-availability check below is a different answer
// a wrapper reads differently.
//
// PATH carries no gh, so reaching the tracker would fail on its own; the
// assertion that matters is the exit code.
func TestIntakeRefusesWithoutATracker(t *testing.T) {
	for _, c := range []struct{ name, block, want string }{
		{"backend none", "[register]\nbackend = \"none\"\n", `backend = "none"`},
		{"backend file", "[register]\nbackend = \"file\"\n", `backend = "file"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			registerProject(t, c.block)
			stubPATH(t)

			err := execute(t, "intake", "5")
			if got := Code(err); got != ExitRefused {
				t.Fatalf("intake exited %d (%v), want %d", got, err, ExitRefused)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not name the declaration that caused it: %v", err)
			}
		})
	}
}

// intakeProject writes a project carrying both namespaces the severity cases
// need, plus a gh stub answering the single-issue read intake makes and the
// routing label it writes back. Anything else exits non-zero, so a call this
// fixture does not model fails here rather than reaching the tracker.
func intakeProject(t *testing.T, issues map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug", E = "enhancement" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "severity"]

[verify]
default = "true"

[github]
repo = "owner/name"
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	// One file per issue, newline-terminated for the shell read builtin: the
	// stubbed PATH holds no cat.
	fixtures := t.TempDir()
	for n, issue := range issues {
		if err := os.WriteFile(filepath.Join(fixtures, fmt.Sprintf("%d.json", n)), []byte(issue+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DROVER_TEST_GH_FIXTURES", fixtures)
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'label create'|'issue edit') exit 0 ;;\n" +
		"'issue view')\n" +
		"  f=\"$DROVER_TEST_GH_FIXTURES/$3.json\"\n" +
		"  if [ -f \"$f\" ]; then IFS= read -r row < \"$f\"; printf '%s' \"$row\"\n" +
		"  else echo \"stub gh: no issue $3\" >&2; exit 1; fi ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	bin := stubPATH(t)
	stub(t, filepath.Join(bin, "gh"), script)
	return dir
}

// severityLine returns the frontmatter severity from the only body intake wrote.
func severityLine(t *testing.T, dir string) string {
	t.Helper()
	bodies, err := filepath.Glob(filepath.Join(dir, "work", "*.md"))
	if err != nil || len(bodies) != 1 {
		t.Fatalf("intake wrote %v (%v), want exactly one body", bodies, err)
	}
	data, err := os.ReadFile(bodies[0]) // #nosec G304 -- a path this test just globbed
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "severity:") {
			return strings.TrimRight(l, " ")
		}
	}
	t.Fatalf("%s carries no severity key:\n%s", bodies[0], data)
	return ""
}

// The unit table above proves scaffold reads Issue.Severity. This proves the
// field arrives: intake reads the tracker through gh.View, and a View that
// returned it empty would scaffold the fallback for a bug with a severity,
// with nothing downstream saying it was a guess.
func TestIntakeScaffoldsTheTrackersSeverity(t *testing.T) {
	for _, c := range []struct {
		name   string
		number int
		issue  string
		want   string
	}{
		{
			name: "ranked bug", number: 44,
			issue: `{"number":44,"title":"ranked bug","body":"b","state":"OPEN",` +
				`"labels":[{"name":"bug"},{"name":"severity:low"}]}`,
			want: "severity: L",
		},
		{
			name: "ranked enhancement", number: 52,
			issue: `{"number":52,"title":"ranked enhancement","body":"e","state":"OPEN",` +
				`"labels":[{"name":"enhancement"},{"name":"severity:medium"}]}`,
			want: "severity: M",
		},
		{
			name: "ungraded bug falls back", number: 53,
			issue: `{"number":53,"title":"ungraded bug","body":"u","state":"OPEN",` +
				`"labels":[{"name":"bug"}]}`,
			want: "severity: M",
		},
		{
			name: "ungraded enhancement stays empty", number: 54,
			issue: `{"number":54,"title":"ungraded enhancement","body":"u","state":"OPEN",` +
				`"labels":[{"name":"enhancement"}]}`,
			want: "severity:",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := intakeProject(t, map[int]string{c.number: c.issue})
			var err error
			capture(t, func() { err = execute(t, "intake", fmt.Sprintf("%d", c.number)) })
			if err != nil {
				t.Fatalf("intake %d: %v", c.number, err)
			}
			if got := severityLine(t, dir); got != c.want {
				t.Errorf("issue #%d scaffolded %q, want %q", c.number, got, c.want)
			}
		})
	}
}

// A body whose frontmatter a human broke is still that issue's item. Scanning
// parsed items alone read it as a fresh issue and allocated a second id, so one
// issue held two files and nothing detected it.
func TestIntakeSeesAnUnreadableBodyLinkingTheIssue(t *testing.T) {
	dir := intakeProject(t, map[int]string{
		44: `{"number":44,"title":"ranked bug","body":"b","state":"OPEN",` +
			`"labels":[{"name":"bug"},{"name":"severity:low"}]}`,
	})
	broken := "---\ntitle: already taken in\ndeps:\nissue: 44\nbogus: 1\n---\n\n1. Do it.\n"
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-already-taken-in.md"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := capture(t, func() { err = execute(t, "intake", "44") })
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if !strings.Contains(out, "already intaken") || !strings.Contains(out, "does not parse") {
		t.Errorf("intake wrote:\n%s\nwant it to name the unreadable body it found", out)
	}
	bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md"))
	if len(bodies) != 1 {
		t.Errorf("work holds %v, want the one body — intake allocated a second id for one issue", bodies)
	}
}

// oneMerged is the ledger every promotion fixture reads: item 1 merged and item
// 2 open, so one row is dormant and one is live off the same register.
const oneMerged = `{"id":"1","merged":true,"runner":"upstream","date":"2026-01-01"}`

// promotionProject writes a project whose gh stub holds the register in a
// directory of files, one per row, so removing the follow-up label removes the row.
//
// A stub answering a fixed page would let a promotion "succeed" while the
// register still listed the row: the assertion that matters is what `follow-ups`
// prints after intake ran, not that intake called the label API.
func promotionProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug", E = "enhancement" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "severity"]

[verify]
default = "true"

[github]
repo = "owner/name"
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(oneMerged), 0o600); err != nil {
		t.Fatal(err)
	}

	// Item 1 merged, item 2 open, so #101 is dormant and #102 is live off the
	// same fixture. Planned ids keep the owners clear of the B namespace intake
	// allocates into.
	rows := map[int]string{
		101: `{"number":101,"title":"orphaned row","body":"d\nOwner: 1","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}`,
		102: `{"number":102,"title":"live row","body":"l\nOwner: 2","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}`,
		103: `{"number":103,"title":"unowned row","body":"u","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}`,
		104: `{"number":104,"title":"kinded orphaned row","body":"k\nOwner: 1","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"bug"},{"name":"severity:high"}]}`,
	}
	reg := filepath.Join(dir, "stub-register")
	all := filepath.Join(dir, "stub-issues")
	for _, d := range []string{reg, all} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for n, row := range rows {
		name := fmt.Sprintf("%d.json", n)
		// One line, newline-terminated: the stub reads each row with the shell
		// read builtin, which needs the terminator.
		for _, d := range []string{reg, all} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(row+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Only git and gh sit on the stubbed PATH, so the script uses shell builtins
	// alone: no cat, no rm. Removing the follow-up label truncates the row's
	// file, and an empty file is a row the register does not hold.
	//
	// The removal edits both copies, because `issue view` and `issue list` are
	// two reads of one tracker. Truncating only the register would let a view
	// keep reporting a label the list had dropped, which is a state no tracker
	// can be in and which any command that re-reads an issue would act on.
	t.Setenv("DROVER_TEST_GH_REGISTER", reg)
	t.Setenv("DROVER_TEST_GH_ISSUES_DIR", all)
	script := fmt.Sprintf(`#!/bin/sh
REG="$DROVER_TEST_GH_REGISTER"
ALL="$DROVER_TEST_GH_ISSUES_DIR"
LBL='{"name":"%s"},'
emit() { while IFS= read -r line; do printf '%%s' "$line"; done < "$1"; }
case "$1 $2" in
'auth status') exit 0 ;;
'label create') exit 0 ;;
'issue edit')
  n="$3"; shift 3
  while [ $# -gt 0 ]; do
    if [ "$1" = "--remove-label" ] && [ "$2" = '%s' ]; then
      if [ -n "$STUB_REMOVE_FAILS" ]; then echo "stub gh: label removal failed" >&2; exit 1; fi
      : > "$REG/$n.json"
      IFS= read -r row < "$ALL/$n.json"
      case "$row" in *"$LBL"*) printf '%%s%%s\n' "${row%%%%$LBL*}" "${row#*$LBL}" > "$ALL/$n.json" ;; esac
    fi
    shift
  done
  exit 0 ;;
'issue view')
  if [ -s "$ALL/$3.json" ]; then emit "$ALL/$3.json"
  else echo "stub gh: no issue $3" >&2; exit 1; fi ;;
'issue list')
  printf '['
  sep=''
  for f in "$REG"/*.json; do
    [ -s "$f" ] || continue
    printf '%%s' "$sep"; emit "$f"; sep=','
  done
  printf ']' ;;
*) echo "stub gh: unexpected $*" >&2; exit 1 ;;
esac
`, "follow-up", "follow-up")
	bin := stubPATH(t)
	stub(t, filepath.Join(bin, "gh"), script)
	t.Chdir(dir)
	return dir
}

// registerRows reads the register through the command a wrapper reads, so the
// promoted row's absence is asserted against what drover prints rather than
// against the label it wrote.
func registerRows(t *testing.T) []RegisterRow {
	t.Helper()
	var err error
	out := capture(t, func() { err = execute(t, "follow-ups", "--json") })
	if err != nil {
		t.Fatalf("follow-ups --json: %v", err)
	}
	var report RegisterReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("follow-ups --json wrote %q: %v", out, err)
	}
	return report.Rows
}

// The promotion this item exists for: a dormant row becomes an item and leaves
// the register in the same act. Both halves are asserted, because a scaffold
// whose row is still listed puts one issue in both sets, which is the state the
// membership label exists to prevent.
func TestIntakePromotesADormantRegisterRow(t *testing.T) {
	dir := promotionProject(t)
	before := registerRows(t)
	if len(before) != 4 {
		t.Fatalf("the register holds %d rows, want 4: %+v", len(before), before)
	}

	var err error
	out := capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101: %v", err)
	}
	if !strings.Contains(out, `dropped "follow-up" from issue #101`) {
		t.Errorf("intake wrote:\n%s\nwant it to say the row left the register", out)
	}
	bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md"))
	if len(bodies) != 1 {
		t.Fatalf("work holds %v, want the one scaffold", bodies)
	}

	after := registerRows(t)
	var want []RegisterRow
	for _, r := range before {
		if r.Number != 101 {
			want = append(want, r)
		}
	}
	if !reflect.DeepEqual(after, want) {
		t.Errorf("follow-ups now reports\n%+v\nwant\n%+v — only #101 should have left", after, want)
	}
}

// A live row already has a run scheduled to carry it. Promoting it writes a
// second item for the same gap, and neither one knows about the other.
func TestIntakeRefusesALiveRegisterRow(t *testing.T) {
	dir := promotionProject(t)

	var err error
	capture(t, func() { err = execute(t, "intake", "102", "--kind", "bug") })
	if got := Code(err); got != ExitRefused {
		t.Fatalf("intake 102 exited %d (%v), want %d", got, err, ExitRefused)
	}
	if !strings.Contains(err.Error(), "owned by 2") {
		t.Errorf("the refusal does not name the owner still open: %v", err)
	}
	if bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(bodies) != 0 {
		t.Errorf("the refusal left %v behind; exit 3 must write nothing", bodies)
	}
	if rows := registerRows(t); len(rows) != 4 {
		t.Errorf("the register holds %d rows, want the 4 it started with", len(rows))
	}
}

// The namespace refusal is on the promotion path rather than the exception
// there: a register row commonly carries the follow-up label and nothing else, and a
// reader must not have to guess whether drover would pick a kind for them.
func TestIntakeSaysKindIsRequiredForAnUnkindedRegisterRow(t *testing.T) {
	promotionProject(t)

	var err error
	capture(t, func() { err = execute(t, "intake", "101") })
	if got := Code(err); got != ExitRefused {
		t.Fatalf("intake 101 exited %d (%v), want %d", got, err, ExitRefused)
	}
	for _, want := range []string{"is a follow-up carrying no label", "--kind is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never says %q: %v", want, err)
		}
	}
}

// A row carrying a namespace label needs no flag: promotion allocates against
// the kind the tracker already declared, exactly as an ordinary intake does.
func TestIntakePromotesAKindedRegisterRowWithoutAFlag(t *testing.T) {
	dir := promotionProject(t)

	var err error
	out := capture(t, func() { err = execute(t, "intake", "104") })
	if err != nil {
		t.Fatalf("intake 104: %v", err)
	}
	if !strings.Contains(out, "(issue #104, bug)") {
		t.Errorf("intake wrote:\n%s\nwant it to allocate against the bug namespace", out)
	}
	if got := severityLine(t, dir); got != "severity: H" {
		t.Errorf("the scaffold ranked %q, want the tracker's severity: H", got)
	}
	for _, r := range registerRows(t) {
		if r.Number == 104 {
			t.Errorf("#104 is still a register row after promotion: %+v", r)
		}
	}
}

// Idempotency keys on the body file, and promotion does not change that: the
// second run names the item it already wrote rather than allocating a second id
// for one issue.
func TestIntakeIsIdempotentOnAPromotedRow(t *testing.T) {
	dir := promotionProject(t)

	var err error
	capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101: %v", err)
	}
	out := capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101 again: %v", err)
	}
	if !strings.Contains(out, "already intaken") {
		t.Errorf("the second intake wrote:\n%s\nwant it to name the item it already wrote", out)
	}
	if bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(bodies) != 1 {
		t.Errorf("work holds %v, want one body — intake allocated a second id for one issue", bodies)
	}
}

// The label write degrades the way RemoveLabel does: the scaffold is on disk and
// the file is what drover reads. What the operator must not lose is that the
// issue is now in both sets, so the warning says which half did not land.
func TestIntakeWarnsWhenTheFollowUpLabelSurvives(t *testing.T) {
	dir := promotionProject(t)
	t.Setenv("STUB_REMOVE_FAILS", "1")

	var err error
	msg := captureErr(t, func() {
		capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	})
	if err != nil {
		t.Fatalf("a failed label write must not fail intake: %v", err)
	}
	if bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(bodies) != 1 {
		t.Fatalf("work holds %v, want the scaffold the warning says is valid", bodies)
	}
	if !strings.Contains(msg, "still carries") {
		t.Errorf("intake warned:\n%s\nwant it to say #101 is still a register row", msg)
	}
	found := false
	for _, r := range registerRows(t) {
		if r.Number == 101 {
			found = true
		}
	}
	if !found {
		t.Error("the stub dropped the row anyway, so this case proves nothing")
	}
}

// The repair the warning above names. A promotion whose label write failed
// leaves the issue in the register and the backlog at once, and the second run
// short-circuits on the body — so without repairPromotion, the command the
// warning sends the operator to does nothing at the point the invariant is
// already broken.
func TestIntakeRepairsAPromotionWhoseLabelSurvived(t *testing.T) {
	dir := promotionProject(t)
	t.Setenv("STUB_REMOVE_FAILS", "1")

	var err error
	capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101: %v", err)
	}
	if !hasRegisterRow(t, 101) {
		t.Fatal("the stub dropped the row anyway, so this case proves nothing")
	}

	t.Setenv("STUB_REMOVE_FAILS", "")
	out := capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101 again: %v", err)
	}
	if !strings.Contains(out, "already intaken") {
		t.Errorf("the second intake wrote:\n%s\nwant it to name the item it already wrote", out)
	}
	if !strings.Contains(out, `dropped "follow-up" from issue #101`) {
		t.Errorf("the second intake wrote:\n%s\nwant it to say the row left the register", out)
	}
	if hasRegisterRow(t, 101) {
		t.Error("#101 is still a register row after re-running intake")
	}
	if bodies, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(bodies) != 1 {
		t.Errorf("work holds %v, want one body — the repair allocated a second id", bodies)
	}
}

// A re-run against an issue nobody labelled must not announce a removal it did
// not make: the tracker is re-asked, so a label dropped by hand in between is
// already gone and the repair has nothing to say.
func TestIntakeRepairIsQuietWhenTheRowAlreadyLeft(t *testing.T) {
	promotionProject(t)

	var err error
	capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101: %v", err)
	}
	out := capture(t, func() { err = execute(t, "intake", "101", "--kind", "bug") })
	if err != nil {
		t.Fatalf("intake 101 again: %v", err)
	}
	if strings.Contains(out, "dropped") {
		t.Errorf("the second intake wrote:\n%s\nwant no removal reported for a row already gone", out)
	}
}

// hasRegisterRow reports whether the register drover prints still holds the
// issue, read through `follow-ups` rather than through the label the stub wrote.
func hasRegisterRow(t *testing.T, number int) bool {
	t.Helper()
	for _, r := range registerRows(t) {
		if r.Number == number {
			return true
		}
	}
	return false
}
