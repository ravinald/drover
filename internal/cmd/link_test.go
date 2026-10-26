package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/gh"
)

// linkProject is closesProject with the register turned off, so the tracker
// check short-circuits and these tests never reach the network. What the check
// itself refuses is covered by the refusal case; what it guards is the write
// below.
func linkProject(t *testing.T, closes string) string {
	t.Helper()
	return linkProjectWith(t, `optional = ["kind", "issue", "closes", "severity"]`, closes)
}

// A project whose schema has no closes key at all: nothing here can name the
// issues it resolves, so link has nowhere to write.
func noClosesProject(t *testing.T) string {
	t.Helper()
	dir := linkProjectWith(t, `optional = ["kind"]`, "")
	write(t, filepath.Join(dir, "work", "B01-a-claim.md"),
		"---\ntitle: No closes key here\ndeps:\nkind: bug\n---\n\nRequirements:\n\n1. Ship it.\n")
	return dir
}

func linkProjectWith(t *testing.T, optional, closes string) string {
	t.Helper()
	dir := t.TempDir()
	conf := `schema_version = 2
project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
` + optional + `

[register]
backend = "none"

[verify]
default = "true"
`
	write(t, filepath.Join(dir, ".drover.toml"), conf)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\ntitle: A claim widens this\ndeps:\nkind: bug\nissue:\n"
	if closes != "" {
		body += "closes: " + closes + "\n"
	}
	body += "severity: M\n---\n\nRequirements:\n\n1. Ship it.\n"
	write(t, filepath.Join(dir, "work", "B01-a-claim.md"), body)
	return dir
}

func itemBody(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "work", "B01-a-claim.md")) // #nosec G304 -- a fixture this test wrote
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The whole point of the command: finish reads closes and nothing else, so an
// issue a run fixed reaches the filer only if the key names it.
func TestLinkWidensTheClosesKey(t *testing.T) {
	dir := linkProject(t, "240 239")
	t.Chdir(dir)

	if err := execute(t, "link", "B1", "259"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got, want := frontmatterValue(t, itemBody(t, dir)), "239 240 259"; got != want {
		t.Errorf("closes = %q, want %q", got, want)
	}
}

// A key the item never declared has to be written, not just appended to: the
// case is an item carrying routed work and no closes line at all.
func TestLinkAddsTheKeyWhenTheItemHasNone(t *testing.T) {
	dir := linkProject(t, "")
	t.Chdir(dir)

	if err := execute(t, "link", "B1", "259"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	body := itemBody(t, dir)
	if got, want := frontmatterValue(t, body), "259"; got != want {
		t.Errorf("closes = %q, want %q", got, want)
	}
	// Declared order, not the end of the fence: closes sits between issue and
	// severity because that is where [frontmatter] optional puts it.
	keys := frontmatterKeys(t, body)
	if got, want := strings.Join(keys, " "), "title deps kind issue closes severity"; got != want {
		t.Errorf("fence keys = %q, want %q", got, want)
	}
}

// The body below the fence is somebody's prose and a frontmatter write must be
// unable to reach it.
func TestLinkLeavesTheBodyAlone(t *testing.T) {
	dir := linkProject(t, "")
	t.Chdir(dir)
	before := itemBody(t, dir)

	if err := execute(t, "link", "B1", "259"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got, want := afterFence(t, itemBody(t, dir)), afterFence(t, before); got != want {
		t.Errorf("body changed:\n got %q\nwant %q", got, want)
	}
}

// An operator naming a state rather than a transition gets exit 0, the way
// accept does.
func TestLinkIsIdempotent(t *testing.T) {
	dir := linkProject(t, "259")
	t.Chdir(dir)

	if err := execute(t, "link", "B1", "259"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got, want := frontmatterValue(t, itemBody(t, dir)), "259"; got != want {
		t.Errorf("closes = %q, want %q; a second claim duplicated the number", got, want)
	}
}

// Refusing this invocation would leave the number in a note instead of the
// key, so resolve widens the key and then records.
func TestResolveLinkWidensThenRecords(t *testing.T) {
	dir := linkProject(t, "240")
	t.Chdir(dir)

	if err := execute(t, "resolve", "B1", "259", "fixed", "--code-by", "agent", "--link", "shipped here"); err != nil {
		t.Fatalf("resolve --link: %v", err)
	}
	if got, want := frontmatterValue(t, itemBody(t, dir)), "240 259"; got != want {
		t.Errorf("closes = %q, want %q", got, want)
	}
	if led := readLedger(t, dir); !strings.Contains(led, `"issue":259`) || !strings.Contains(led, `"disposition":"fixed"`) {
		t.Errorf("no disposition row for #259: %s", led)
	}
}

// Without the flag the refusal stands, and it has to name the way out or the
// reader is back to hand-editing frontmatter.
func TestResolveWithoutLinkStillRefusesAndNamesLink(t *testing.T) {
	dir := linkProject(t, "240")
	t.Chdir(dir)

	err := execute(t, "resolve", "B1", "259", "fixed", "--code-by", "agent")
	if err == nil {
		t.Fatal("resolve accepted an unclaimed issue")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	for _, want := range []string{"drover link B1 259", "--link"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

func frontmatterValue(t *testing.T, body string) string {
	t.Helper()
	for _, line := range fenceLines(t, body) {
		if frontmatterKey(line) == closesKey {
			return strings.TrimSpace(line[strings.Index(line, ":")+1:])
		}
	}
	return ""
}

func frontmatterKeys(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range fenceLines(t, body) {
		if k := frontmatterKey(line); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func fenceLines(t *testing.T, body string) []string {
	t.Helper()
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		t.Fatalf("no fence: %q", body)
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return lines[1:i]
		}
	}
	t.Fatalf("fence never closes: %q", body)
	return nil
}

func afterFence(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	t.Fatalf("fence never closes: %q", body)
	return ""
}

// unclaimedProject has two items carrying routed work: B1 has run and B2 has
// not. The register is the file backend so nothing here reaches a network, and
// [paths] logs is inside the fixture so the run log is one this test wrote.
func unclaimedProject(t *testing.T, closes string, merged ...string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".drover.toml"), `schema_version = 2
project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
logs = "logs"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["closes"]

[frontmatter.shape]
closes = "intlist"

[register]
backend = "file"

[verify]
default = "true"
`)
	for _, d := range []string{"work", "logs", ".drover"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	ran := "---\ntitle: Has run\ndeps:\n"
	if closes != "" {
		ran += "closes: " + closes + "\n"
	}
	write(t, filepath.Join(dir, "work", "B01-has-run.md"), ran+"---\n\nRequirements:\n\n1. Do it.\n")
	write(t, filepath.Join(dir, "work", "B02-has-not-run.md"), body("Has not run", ""))
	write(t, filepath.Join(dir, "logs", "B01-has-run.20260101-000000.000000000.jsonl"),
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.1,"result":"done"}`+"\n")

	rows := ""
	for _, id := range merged {
		rows += fmt.Sprintf("{\"id\":%q,\"merged\":true,\"implementer\":\"upstream\",\"date\":\"2026-01-01\"}\n", id)
	}
	write(t, filepath.Join(dir, "ledger.jsonl"), rows)
	fileRegister(t, dir,
		`{"number":259,"title":"owned by the item that has run","owners":["B1"],"state":"open","date":"2026-01-01"}`,
		`{"number":260,"title":"owned by the item that has not","owners":["B2"],"state":"open","date":"2026-01-01"}`,
	)
	return dir
}

// The window the finding exists for: B1 has run, has not merged, and its closes
// key does not name the issue routed to it, so the merge ahead closes nothing.
func TestCheckReportsAnUnlinkedRoutedIssue(t *testing.T) {
	rep := openIn(t, unclaimedProject(t, "")).checkReport()

	got := findings(rep, "UNLINKED")
	if len(got) != 1 {
		t.Fatalf("UNLINKED findings = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "#259") || got[0].ID != "B1" {
		t.Errorf("finding does not name #259 against B1: %+v", got[0])
	}
	if !strings.Contains(got[0].Detail, "drover link B1 259") {
		t.Errorf("finding does not name the fix: %q", got[0].Detail)
	}
}

// Linking it is what discharges the finding, which is the whole loop: the key
// now names the issue, so finish will act on it.
func TestCheckIsSilentOnceTheIssueIsLinked(t *testing.T) {
	if got := findings(openIn(t, unclaimedProject(t, "259")).checkReport(), "UNLINKED"); len(got) != 0 {
		t.Errorf("UNLINKED findings = %d, want 0: %+v", len(got), got)
	}
}

// B2 owes nothing yet. An item nothing has executed carrying routed work is the
// backlog doing its job, and a finding there fires on every project.
func TestCheckIgnoresAnItemThatHasNotRun(t *testing.T) {
	for _, f := range findings(openIn(t, unclaimedProject(t, "")).checkReport(), "UNLINKED") {
		if f.ID == "B2" {
			t.Errorf("UNLINKED fired on an item that has not run: %+v", f)
		}
	}
}

// Once the owner merges the branch is gone and the remedy is a deps: line, not
// a key. ORPHANED FOLLOW-UP owns that state, and reporting both would name two fixes for
// one issue.
func TestCheckLeavesAMergedOwnerToDormant(t *testing.T) {
	rep := openIn(t, unclaimedProject(t, "", "B1")).checkReport()

	if got := findings(rep, "UNLINKED"); len(got) != 0 {
		t.Errorf("UNLINKED findings = %d, want 0 once B1 merged: %+v", len(got), got)
	}
	if got := findings(rep, "ORPHANED FOLLOW-UP"); len(got) != 1 {
		t.Errorf("ORPHANED FOLLOW-UP findings = %d, want 1: %+v", len(got), got)
	}
}

// routedProject is the consolidation shape: B2 folds a finding filed against
// B1, so the tracker routes the issue to B1 while B2 is the item that fixes it.
// deps decides which of those two answers link reads.
func routedProject(t *testing.T, deps string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".drover.toml"), `schema_version = 2
project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["closes"]

[register]
backend = "github"

[github]
repo = "owner/name"

[verify]
default = "true"
`)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "B01-owns-the-gap.md"), body("Owns the gap", ""))
	write(t, filepath.Join(dir, "work", "B02-folds-it.md"),
		"---\ntitle: Folds it\ndeps: "+deps+"\n---\n\nRequirements:\n\n1. Fix it.\n")
	stubGHView(t, stubPATH(t),
		`{"number":259,"body":"Owner: B1","title":"filed against B1","state":"OPEN","labels":[{"name":"follow-up"}]}`)
	return dir
}

// stubGHView answers the single call claimable makes. Anything else exits
// non-zero, so a link that grew a second tracker read fails here rather than
// reaching the network.
func stubGHView(t *testing.T, dir, issue string) {
	t.Helper()
	t.Setenv("DROVER_TEST_GH_ISSUE", issue)
	stub(t, filepath.Join(dir, "gh"), ghIssueView)
}

// ghIssueView answers the availability probe and prints $DROVER_TEST_GH_ISSUE
// for an issue read. Anything else exits non-zero.
const ghIssueView = "#!/bin/sh\ncase \"$1 $2\" in\n" +
	"'auth status') exit 0 ;;\n" +
	"'issue view') printf '%s' \"$DROVER_TEST_GH_ISSUE\" ;;\n" +
	"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
	"esac\n"

func foldedBody(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "work", "B02-folds-it.md")) // #nosec G304 -- a fixture this test wrote
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A route label says which run introduced the gap. An item consolidating
// findings from several runs declares those runs on deps, and inherits the rows
// routed to them — the alternative is relabelling every issue, which overwrites
// the provenance an audit filed it against.
func TestLinkInheritsARowRoutedToADeclaredDep(t *testing.T) {
	dir := routedProject(t, "B1")
	t.Chdir(dir)

	if err := execute(t, "link", "B2", "259"); err != nil {
		t.Fatalf("claim refused an inherited row: %v", err)
	}
	if got, want := frontmatterValue(t, foldedBody(t, dir)), "259"; got != want {
		t.Errorf("closes = %q, want %q", got, want)
	}
}

// Without the dep the refusal stands, and it names the third way out so an
// operator does not read the refusal as unconditional.
func TestLinkStillRefusesARowRoutedElsewhere(t *testing.T) {
	dir := routedProject(t, "")
	t.Chdir(dir)

	err := execute(t, "link", "B2", "259")
	if err == nil {
		t.Fatal("claim accepted a row owned by an item it does not depend on")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	for _, want := range []string{"#259 is owned by B1", "deps:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if got := frontmatterValue(t, foldedBody(t, dir)); got != "" {
		t.Errorf("closes = %q after a refusal, want empty", got)
	}
}

// dropClosesKey returns the same fixture with closes declared nowhere, which is
// where a project generated without the key stands. The shape entry goes
// with it: a shape for a key [frontmatter] does not declare is UNDECLARED KEY's
// finding, and leaving it would test two gaps at once.
func dropClosesKey(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".drover.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "optional = [\"closes\"]\n", "optional = []\n", 1)
	s = strings.Replace(s, "\n[frontmatter.shape]\ncloses = \"intlist\"\n", "", 1)
	if s == string(b) {
		t.Fatalf("fixture no longer declares closes the way this helper strips it:\n%s", b)
	}
	write(t, path, s)
	return dir
}

// The project that cannot record what any merge resolved. Reported once against
// the config, because the per-issue finding names a command that exits 3 here.
func TestCheckReportsAnUndeclaredClosesKey(t *testing.T) {
	rep := openIn(t, dropClosesKey(t, unclaimedProject(t, ""))).checkReport()

	got := findings(rep, "NO CLOSES KEY")
	if len(got) != 1 {
		t.Fatalf("NO CLOSES KEY findings = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Detail, "required or optional") {
		t.Errorf("finding does not name the fix: %q", got[0].Detail)
	}
	if unclaimed := findings(rep, "UNLINKED"); len(unclaimed) != 0 {
		t.Errorf("UNLINKED fired beside it, naming drover link where claim exits 3: %+v", unclaimed)
	}
}

// A project routing nothing owes no key. The finding is about issues nobody can
// close, so with no issues routed there is nothing to report.
func TestCheckIgnoresAnUndeclaredClosesKeyWithNothingRouted(t *testing.T) {
	dir := dropClosesKey(t, unclaimedProject(t, ""))
	fileRegister(t, dir)

	if got := findings(openIn(t, dir).checkReport(), "NO CLOSES KEY"); len(got) != 0 {
		t.Errorf("NO CLOSES KEY findings = %d, want 0 with an empty register: %+v", len(got), got)
	}
}

// Declaring the key is what discharges it, and hands the per-issue finding back.
func TestCheckIsSilentOnceTheClosesKeyIsDeclared(t *testing.T) {
	rep := openIn(t, unclaimedProject(t, "")).checkReport()

	if got := findings(rep, "NO CLOSES KEY"); len(got) != 0 {
		t.Errorf("NO CLOSES KEY findings = %d, want 0: %+v", len(got), got)
	}
	if got := findings(rep, "UNLINKED"); len(got) != 1 {
		t.Errorf("UNLINKED findings = %d, want 1 once the key is declarable: %+v", len(got), got)
	}
}

// finish's advisory recommends drover link, so where link cannot run it names
// the config instead. The routed numbers are reported either way: they are the
// facts, and only the remedy depends on the project.
func TestFinishUnlinkedNamesTheConfigWhereLinkCannotRun(t *testing.T) {
	s := openIn(t, dropClosesKey(t, unclaimedProject(t, "")))
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	routed := []gh.Issue{{Number: 259, Owners: []string{"B1"}}}

	out := capture(t, func() { s.finishUnlinked(it, routed) })

	if !strings.Contains(out, "#259") {
		t.Errorf("advisory dropped the owned issue: %q", out)
	}
	// link still appears, as the second of two steps. What must not appear is
	// the unconditional line, which offers it as the whole remedy.
	if strings.Contains(out, "if this merge fixed one") {
		t.Errorf("advisory offers a claim that exits 3 here as the whole remedy: %q", out)
	}
	if !strings.Contains(out, "add it to required or optional in .drover.toml") {
		t.Errorf("advisory does not name the config fix: %q", out)
	}
}

func TestFinishUnlinkedRecommendsLinkWhereItRuns(t *testing.T) {
	s := openIn(t, unclaimedProject(t, ""))
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() { s.finishUnlinked(it, []gh.Issue{{Number: 259, Owners: []string{"B1"}}}) })

	if !strings.Contains(out, "drover link B1 <issue>") {
		t.Errorf("advisory does not recommend the claim: %q", out)
	}
}
