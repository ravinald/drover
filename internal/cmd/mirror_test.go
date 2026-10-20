package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// mirrorConf turns the mirror on over a project declaring no register, so every
// gh call a test sees is the mirror's own.
const mirrorConf = noProject + "\n[backlog]\nmirror = \"github\"\n"

// stubMirrorGH answers the mirror's calls and logs each one on a line of its own.
// `issue view N` and `issue list` read fixtures beside the stub, so a test states
// what the tracker holds by writing a file. Each `issue create` opens the next
// number from #77. fail makes every call exit 1, which
// is how a tracker nobody can reach looks from here.
func stubMirrorGH(t *testing.T, bin string, fail bool) (log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("DROVER_TEST_GH_LOG", log)
	t.Setenv("DROVER_TEST_GH_FIXTURES", bin)
	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" | tr '\\n' ' ' >> \"$DROVER_TEST_GH_LOG\"\nprintf '\\n' >> \"$DROVER_TEST_GH_LOG\"\n"
	if fail {
		script += "echo 'gh: could not resolve api.github.com' >&2\nexit 1\n"
	} else {
		script += "case \"$1 $2\" in\n" +
			"'pr list') echo '[]' ;;\n" +
			"'issue create') n=$(cat \"$DROVER_TEST_GH_FIXTURES/next\" 2>/dev/null || echo 77); echo \"https://github.com/o/r/issues/$n\"; echo $((n+1)) > \"$DROVER_TEST_GH_FIXTURES/next\" ;;\n" +
			"'issue view') cat \"$DROVER_TEST_GH_FIXTURES/view-$3.json\" ;;\n" +
			"'issue list') cat \"$DROVER_TEST_GH_FIXTURES/list.json\" ;;\n" +
			"esac\nexit 0\n"
	}
	stub(t, filepath.Join(bin, "gh"), script)
	return log
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// callsTo is the logged calls that start with prefix.
func callsTo(t *testing.T, log, prefix string) []string {
	t.Helper()
	var out []string
	for _, c := range ghCalls(t, log) {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

const toPublish = "---\ntitle: Publish me\ndeps: B2 B3\nissue:\n---\n\n" +
	"STOP: requirements not yet written\n" +
	"drover refuses to launch this item while this line is present. Delete it\n" +
	"once the list below is real.\n\n" +
	"Act as a maintainer.\n\nRequirements:\n\n" +
	"1. First thing\n   continued here.\n2. Second thing\n\n" +
	"--- raw material: issue #5 — evidence, NOT requirements ---\n\n" +
	"> 9. the reporter's own words\n\n" +
	"--- end raw material ---\n"

// mirrorProject is B1 waiting on accept, B2 a dep that names issue #40, and B3 a
// dep that names none.
func mirrorProject(t *testing.T) string {
	t.Helper()
	dir := registerProject(t, mirrorConf)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeFile(t, filepath.Join(dir, "work", "B01-publish.md"), toPublish)
	writeFile(t, filepath.Join(dir, "work", "B02-dep.md"),
		"---\ntitle: Dep with an issue\ndeps:\nissue: 40\n---\n\nRequirements:\n\n1. Do it.\n")
	writeFile(t, filepath.Join(dir, "work", "B03-bare.md"), body("Dep without an issue", ""))
	return dir
}

// The requirements are the whole of what reaches the tracker from the body:
// continuation lines kept, intake's quoted evidence never.
func TestMirrorRequirementsLeaveTheRawMaterialBehind(t *testing.T) {
	got := mirrorRequirements(toPublish[strings.Index(toPublish, "Act as"):])
	if want := "1. First thing\n   continued here.\n2. Second thing"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAcceptPublishesTheItem(t *testing.T) {
	dir := mirrorProject(t)
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	reqs := "1. First thing\n   continued here.\n2. Second thing"
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Publish me","state":"OPEN","body":`+jsonString(mirrorBody("B1", reqs))+
			`,"labels":[{"name":"drover"}],"blockedBy":{"nodes":[]}}`)
	t.Chdir(dir)

	var err error
	out := captureErr(t, func() { err = execute(t, "accept", "B1") })
	if err != nil {
		t.Fatalf("accept: %v\n%s", err, out)
	}

	creates := callsTo(t, log, "issue create")
	if len(creates) != 1 {
		t.Fatalf("issue create calls: %q, want one", creates)
	}
	for _, want := range []string{"--title Publish me", "1. First thing", "continued here.", "2. Second thing",
		"backlog file is the contract", "--label drover"} {
		if !strings.Contains(creates[0], want) {
			t.Errorf("the published issue lacks %q: %q", want, creates[0])
		}
	}
	if strings.Contains(creates[0], "reporter") {
		t.Errorf("raw material reached the tracker: %q", creates[0])
	}

	path := filepath.Join(dir, "work", "B01-publish.md")
	text, err := os.ReadFile(path) // #nosec G304 -- the fixture's own body
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "\nissue: 77\n") || strings.Contains(string(text), "STOP:") {
		t.Errorf("accept left the body as:\n%s", text)
	}
	// The body row vouches for the bytes on disk, the issue key included, so
	// the next comparison reads the write as drover's own.
	s := openIn(t, dir)
	sum, err := bodyDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ledger.Rows[len(s.ledger.Rows)-1]; got.ID != "B1" || got.Body != sum {
		t.Errorf("last ledger row %+v, want B1's body row for %s", got, sum)
	}

	if links := callsTo(t, log, "issue edit 77 --add-blocked-by"); len(links) != 1 || !strings.HasSuffix(links[0], " 40") {
		t.Errorf("blocked-by calls: %q, want one naming #40", links)
	}
	if labels := callsTo(t, log, "issue edit 77 --add-label drover:blocked"); len(labels) != 1 {
		t.Errorf("no drover:blocked label written: %q", ghCalls(t, log))
	}
	if !strings.Contains(out, "B3") || !strings.Contains(out, "no issue") {
		t.Errorf("the dep with no issue went unnamed:\n%s", out)
	}
}

// A tracker that does not answer costs the mirror and never the accept, and
// check lists what it could not publish.
func TestAcceptSurvivesAnUnreachableTracker(t *testing.T) {
	dir := mirrorProject(t)
	log := stubMirrorGH(t, ghPATH(t), true)
	t.Chdir(dir)

	var err error
	stderr := captureErr(t, func() { err = execute(t, "accept", "B1") })
	if err != nil {
		t.Fatalf("an unreachable tracker failed accept: %v", err)
	}
	if !strings.Contains(stderr, "B1 was not published") || !strings.Contains(stderr, "drover check") {
		t.Errorf("accept did not say what it could not publish:\n%s", stderr)
	}
	if len(callsTo(t, log, "issue create")) != 1 {
		t.Errorf("accept never tried the tracker: %q", ghCalls(t, log))
	}

	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if s.isScaffold(it) || issueOf(it) != 0 {
		t.Errorf("B1 scaffold=%v issue=%d, want accepted with no issue", s.isScaffold(it), issueOf(it))
	}
	rep := s.checkReport()
	var named bool
	for _, f := range findings(rep, "UNMIRRORED") {
		named = named || f.ID == "B1"
	}
	if !named {
		t.Errorf("check does not list B1 as UNMIRRORED: %+v", rep.Findings)
	}
	if rep.MirrorNote == "" {
		t.Error("check compared nothing and said nothing about why")
	}
}

// One read answers both findings, and an issue agreeing with its file answers
// neither.
func TestCheckReportsDriftAndStaleMirrors(t *testing.T) {
	dir := registerProject(t, mirrorConf)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeFile(t, filepath.Join(dir, "work", "B01-drifted.md"),
		"---\ntitle: Drifted\ndeps:\nissue: 77\n---\n\nRequirements:\n\n1. Do it.\n")
	writeFile(t, filepath.Join(dir, "work", "B02-agrees.md"),
		"---\ntitle: Agrees\ndeps:\nissue: 78\n---\n\nRequirements:\n\n1. Do it.\n")
	bin := ghPATH(t)
	stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "list.json"), `[`+
		`{"number":77,"title":"Drifted","state":"OPEN","body":`+jsonString(mirrorBody("B1", "1. Do something else."))+
		`,"labels":[{"name":"drover"},{"name":"drover:merged"}],"blockedBy":{"nodes":[]}},`+
		`{"number":78,"title":"Agrees","state":"OPEN","body":`+jsonString(mirrorBody("B2", "1. Do it."))+
		`,"labels":[{"name":"drover"},{"name":"drover:ready"}],"blockedBy":{"nodes":[]}}]`)

	rep := openIn(t, dir).checkReport()
	drift, stale := findings(rep, "MIRROR DRIFT"), findings(rep, "MIRROR STALE")
	if len(drift) != 1 || drift[0].ID != "B1" {
		t.Errorf("MIRROR DRIFT = %+v, want B1 alone", drift)
	}
	if len(stale) != 1 || stale[0].ID != "B1" || !strings.Contains(stale[0].Message, "drover:merged") {
		t.Errorf("MIRROR STALE = %+v, want B1 alone, naming the label it carries", stale)
	}
	if len(findings(rep, "UNMIRRORED")) != 0 {
		t.Errorf("an item naming its issue reads as unmirrored: %+v", rep.Findings)
	}
}

// finish relabels and closes the item's own issue, whatever its closes key says.
func TestFinishClosesItsMirror(t *testing.T) {
	dir, _ := finishProject(t, mirrorConf)
	writeFile(t, filepath.Join(dir, "work", "B01-merged.md"),
		"---\ntitle: Merged owner\ndeps:\nissue: 77\n---\n\nRequirements:\n\n1. Do it.\n")
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Merged owner","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"drover:implemented"}]}`)
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)

	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	edits := callsTo(t, log, "issue edit 77")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:merged") ||
		!strings.Contains(edits[0], "--remove-label drover:implemented") {
		t.Errorf("label edits: %q, want drover:implemented swapped for drover:merged in one call", edits)
	}
	if len(callsTo(t, log, "issue close 77")) != 1 {
		t.Errorf("the mirrored issue was not closed: %q", ghCalls(t, log))
	}
}

// dependentProject is finishProject under the mirror with B1 named on issue
// #77 and B3, which declares deps: B1, on #78 carrying label.
func dependentProject(t *testing.T, conf, label string) (dir, log string) {
	t.Helper()
	dir, _ = finishProject(t, conf)
	writeFile(t, filepath.Join(dir, "work", "B01-merged.md"),
		"---\ntitle: Merged owner\ndeps:\nissue: 77\n---\n\nRequirements:\n\n1. Do it.\n")
	writeFile(t, filepath.Join(dir, "work", "B03-after.md"),
		"---\ntitle: After\ndeps: B1\nissue: 78\n---\n\nRequirements:\n\n1. Then this.\n")
	// The records stay out of git, as reconcile requires of a checkout it lands into.
	writeFile(t, filepath.Join(dir, ".gitignore"), "ledger.jsonl\nreports/\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "fixtures")
	bin := ghPATH(t)
	log = stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Merged owner","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"drover:implemented"}]}`)
	writeFile(t, filepath.Join(bin, "view-78.json"),
		`{"number":78,"title":"After","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"`+label+`"}],`+
			`"blockedBy":{"nodes":[{"number":77}]}}`)
	writeFile(t, filepath.Join(bin, "list.json"), "[]")
	t.Setenv(runner.EnvActor, "")
	t.Chdir(dir)
	return dir, log
}

// A completion moves every item naming it on deps between blocked and ready
// without writing anything about them, so the label on each of their issues
// is finish's to rewrite.
func TestFinishRelabelsTheItemsItUnblocked(t *testing.T) {
	_, log := dependentProject(t, mirrorConf, "drover:blocked")
	var err error
	out := capture(t, func() { err = execute(t, "finish", "B1") })
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	edits := callsTo(t, log, "issue edit 78")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:ready") ||
		!strings.Contains(edits[0], "--remove-label drover:blocked") {
		t.Errorf("#78 edits: %q, want drover:blocked swapped for drover:ready", edits)
	}
	if c := callsTo(t, log, "issue close 78"); len(c) != 0 {
		t.Errorf("a dependent's issue was closed: %q", c)
	}
}

// unmark-merged puts the dependent back where finish took it from, and a label
// that already fits is left alone: a running one says more than ready does.
func TestUndoneRelabelsTheItemsItBlocks(t *testing.T) {
	_, log := dependentProject(t, mirrorConf, "drover:ready")
	if err := execute(t, "mark-merged", "B1"); err != nil {
		t.Fatalf("done: %v", err)
	}
	if edits := callsTo(t, log, "issue edit 78"); len(edits) != 0 {
		t.Errorf("done rewrote a label that already fit: %q", edits)
	}
	if edits := callsTo(t, log, "issue edit 77"); len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:merged") {
		t.Errorf("done left B1's own label: %q", edits)
	}
	if len(callsTo(t, log, "issue close 77")) != 1 {
		t.Errorf("done left B1's issue open: %q", ghCalls(t, log))
	}

	var err error
	out := captureErr(t, func() { err = execute(t, "unmark-merged", "B1") })
	if err != nil {
		t.Fatalf("undone: %v\n%s", err, out)
	}
	edits := callsTo(t, log, "issue edit 78")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:blocked") ||
		!strings.Contains(edits[0], "--remove-label drover:ready") {
		t.Errorf("#78 edits: %q, want drover:ready swapped for drover:blocked", edits)
	}
}

// finish --no-tracker writes nothing and names every issue it left behind, the
// dependents' included.
func TestFinishNoTrackerNamesTheDependents(t *testing.T) {
	_, log := dependentProject(t, mirrorConf, "drover:blocked")
	var err error
	stderr := captureErr(t, func() { err = execute(t, "finish", "B1", "--no-tracker") })
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if w := callsTo(t, log, "issue edit"); len(w) != 0 {
		t.Errorf("--no-tracker wrote %q", w)
	}
	if !strings.Contains(stderr, "#77, #78") || !strings.Contains(stderr, "drover accept B1 B3") {
		t.Errorf("--no-tracker did not name both issues:\n%s", stderr)
	}
}

// Reconciliation skips dispositions, which answer somebody's report, and still
// writes the mirror, whose issues drover filed itself.
func TestReconcileWritesTheMirrorAndNoDisposition(t *testing.T) {
	dir, log := dependentProject(t, ghProject+"\n[backlog]\nmirror = \"github\"\n", "drover:blocked")
	writeFile(t, filepath.Join(dir, "work", "B01-merged.md"),
		"---\ntitle: Merged owner\ndeps:\nissue: 77\ncloses: 47\n---\n\nRequirements:\n\n1. Do it.\n")
	git(t, dir, "commit", "-q", "-am", "closes")
	for _, row := range []ledger.Row{
		{ID: "B1", Stage: ledger.StageImplement, Runner: "impl", Date: ledger.Today()},
		{ID: "B1", Issue: 47, Disposition: ledger.DispositionSatisfied, CodeBy: ledger.CodeAgent, Independent: true,
			Note: "fixed", Date: ledger.Today()},
	} {
		if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"), row); err != nil {
			t.Fatal(err)
		}
	}

	s := openIn(t, dir)
	var landed int
	var err error
	out := captureErr(t, func() { landed, _, err = s.reconcile() })
	if err != nil || landed != 1 {
		t.Fatalf("reconcile finished %d: %v\n%s", landed, err, out)
	}
	if len(callsTo(t, log, "issue close 77")) != 1 {
		t.Errorf("reconcile left the mirrored issue open: %q", ghCalls(t, log))
	}
	if edits := callsTo(t, log, "issue edit 78"); len(edits) != 1 || !strings.Contains(edits[0], "drover:ready") {
		t.Errorf("reconcile left the dependent's label: %q", edits)
	}
	for _, c := range ghCalls(t, log) {
		if strings.Contains(c, " 47") {
			t.Errorf("reconcile reached a closes issue: %q", c)
		}
	}
}

// A dependent accepted before its dep gets its blocked-by link when the dep is
// published, which is the order a batch accept in backlog order runs in.
func TestAcceptLinksTheDependentPublishedFirst(t *testing.T) {
	dir := registerProject(t, mirrorConf)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	scaffold := func(title, deps string) string {
		return "---\ntitle: " + title + "\ndeps: " + deps + "\nissue:\n---\n\n" +
			"STOP: requirements not yet written\n" +
			"drover refuses to launch this item while this line is present. Delete it\n" +
			"once the list below is real.\n\nRequirements:\n\n1. Do it.\n"
	}
	writeFile(t, filepath.Join(dir, "work", "B01-after.md"), scaffold("After", "B2"))
	writeFile(t, filepath.Join(dir, "work", "B02-first.md"), scaffold("First", ""))
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	for n, title := range map[string]string{"77": "After", "78": "First"} {
		writeFile(t, filepath.Join(bin, "view-"+n+".json"),
			`{"number":`+n+`,"title":"`+title+`","state":"OPEN","body":"","labels":[{"name":"drover"}],"blockedBy":{"nodes":[]}}`)
	}
	t.Chdir(dir)

	var err error
	out := captureErr(t, func() { err = execute(t, "accept", "B1", "B2") })
	if err != nil {
		t.Fatalf("accept: %v\n%s", err, out)
	}
	if links := callsTo(t, log, "issue edit 77 --add-blocked-by"); len(links) != 1 || !strings.HasSuffix(links[0], " 78") {
		t.Errorf("blocked-by calls on #77: %q, want one naming #78\n%s", links, out)
	}
}

// labelProject is B1 accepted on issue #77, which carries drover:ready.
func labelProject(t *testing.T) (dir, log string) {
	t.Helper()
	dir = registerProject(t, mirrorConf)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeFile(t, filepath.Join(dir, "work", "B01-watched.md"),
		"---\ntitle: Watched\ndeps:\nissue: 77\n---\n\nRequirements:\n\n1. Do it.\n")
	bin := ghPATH(t)
	log = stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Watched","state":"OPEN","body":`+jsonString(mirrorBody("B1", "1. Do it."))+
			`,"labels":[{"name":"drover"},{"name":"drover:ready"}],"blockedBy":{"nodes":[]}}`)
	t.Chdir(dir)
	return dir, log
}

// split puts a new dep on the item it splits, which moves that item to blocked.
func TestSplitRelabelsTheItemItBlocks(t *testing.T) {
	dir, log := labelProject(t)
	proposal := filepath.Join(dir, "proposal.md")
	writeFile(t, proposal, "Requirements:\n\n1. Split work.\n")
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	if err := execute(t, "split", "B1", "Split work", "-f", proposal); err != nil {
		t.Fatal(err)
	}
	edits := callsTo(t, log, "issue edit 77")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:blocked") ||
		!strings.Contains(edits[0], "--remove-label drover:ready") {
		t.Errorf("#77 edits: %q, want drover:ready swapped for drover:blocked", edits)
	}
}

// unaccept returns the item to a scaffold, and its issue says so.
func TestUnacceptRelabelsAsScaffold(t *testing.T) {
	_, log := labelProject(t)
	if err := execute(t, "unaccept", "B1"); err != nil {
		t.Fatal(err)
	}
	edits := callsTo(t, log, "issue edit 77")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:draft") ||
		!strings.Contains(edits[0], "--remove-label drover:ready") {
		t.Errorf("#77 edits: %q, want drover:ready swapped for drover:draft", edits)
	}
}

// A scaffold naming drover's issue is still compared, on everything but the
// requirements it is being redrafted from.
func TestCheckComparesAScaffoldsIssue(t *testing.T) {
	dir := registerProject(t, mirrorConf)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeFile(t, filepath.Join(dir, "work", "B01-withdrawn.md"),
		"---\ntitle: Withdrawn\ndeps:\nissue: 77\n---\n\nSTOP: requirements not yet written\n\n"+
			"Requirements:\n\n1. Redrafted.\n")
	bin := ghPATH(t)
	stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "list.json"), `[`+
		`{"number":77,"title":"Withdrawn","state":"OPEN","body":`+jsonString(mirrorBody("B1", "1. As published."))+
		`,"labels":[{"name":"drover"},{"name":"drover:ready"}],"blockedBy":{"nodes":[]}}]`)

	rep := openIn(t, dir).checkReport()
	stale := findings(rep, "MIRROR STALE")
	if len(stale) != 1 || stale[0].ID != "B1" || !strings.Contains(stale[0].Message, "scaffold") ||
		!strings.Contains(stale[0].Detail, "unaccept") {
		t.Errorf("MIRROR STALE = %+v, want B1 named as a scaffold with unaccept as the remedy", stale)
	}
	if d := findings(rep, "MIRROR DRIFT"); len(d) != 0 {
		t.Errorf("a scaffold's redrafted list reads as drift: %+v", d)
	}
}

// jsonString quotes s as a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// mirrorShelf is shelfProject declaring the issue key and the mirror mode, so
// the shelf's verbs are tested on the fixture they were written against.
func mirrorShelf(t *testing.T, mode string, bodies, shelved map[string]string) string {
	t.Helper()
	dir := shelfProject(t, bodies, shelved)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	conf := filepath.Join(dir, ".drover.toml")
	b, err := os.ReadFile(conf) // #nosec G304 -- the fixture just written
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(b), `optional = ["kind", "severity"]`, `optional = ["kind", "severity", "issue"]`, 1)
	writeFile(t, conf, text+"\n[backlog]\nmirror = \""+mode+"\"\n")
	return dir
}

const shelvable = "---\ntitle: Not doing this\ndeps:\nissue: 77\n---\n\nRequirements:\n\n1. Do it.\n"

func TestShelveClosesItsIssueAsNotPlanned(t *testing.T) {
	dir := mirrorShelf(t, "github", map[string]string{"F03-wont-do.md": shelvable}, map[string]string{})
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Not doing this","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"drover:ready"}]}`)
	t.Chdir(dir)

	if err := execute(t, "shelve", "F3"); err != nil {
		t.Fatal(err)
	}
	edits := callsTo(t, log, "issue edit 77")
	if len(edits) != 1 || !strings.Contains(edits[0], "--add-label drover:deferred") ||
		!strings.Contains(edits[0], "--remove-label drover:ready") {
		t.Errorf("#77 edits: %q, want drover:ready swapped for drover:deferred", edits)
	}
	closes := callsTo(t, log, "issue close 77")
	if len(closes) != 1 || !strings.Contains(closes[0], "--reason not planned") ||
		!strings.Contains(closes[0], "drover unshelve F3") {
		t.Errorf("#77 closes: %q, want one as not planned naming unshelve", closes)
	}
}

// The move is the command and the tracker is its mirror: an outage costs the
// close, never the shelve, and check is where the missing close surfaces.
func TestShelveUnderAnUnreachableTrackerStillMoves(t *testing.T) {
	dir := mirrorShelf(t, "github", map[string]string{"F03-wont-do.md": shelvable}, map[string]string{})
	bin := ghPATH(t)
	stubMirrorGH(t, bin, true)
	t.Chdir(dir)

	var err error
	out := captureErr(t, func() { err = execute(t, "shelve", "F3") })
	if err != nil {
		t.Fatalf("shelve exited non-zero under an unreachable tracker: %v", err)
	}
	if !strings.Contains(out, "! mirror:") {
		t.Errorf("the failed write went unreported: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "shelved", "F03-wont-do.md")); err != nil {
		t.Fatalf("the body did not move: %v", err)
	}

	stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "list.json"),
		`[{"number":77,"title":"Not doing this","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"drover:ready"}],"blockedBy":{"nodes":[]}}]`)
	got := findings(openIn(t, dir).checkReport(), "MIRROR SHELVED")
	if len(got) != 1 || got[0].ID != "F3" || !strings.Contains(got[0].Detail, "drover unshelve F3, then drover shelve F3") {
		t.Errorf("MIRROR SHELVED = %+v, want F3 with unshelve then shelve as the remedy", got)
	}
}

// An issue without the membership label is the report an item was taken in
// from, and retiring the item does not retire somebody else's report.
func TestShelveLeavesAnotherIssueAlone(t *testing.T) {
	dir := mirrorShelf(t, "github", map[string]string{"F03-wont-do.md": shelvable}, map[string]string{})
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"A report","state":"OPEN","body":"","labels":[{"name":"bug"}]}`)
	t.Chdir(dir)

	if err := execute(t, "shelve", "F3"); err != nil {
		t.Fatal(err)
	}
	if w := append(callsTo(t, log, "issue edit"), callsTo(t, log, "issue close")...); len(w) != 0 {
		t.Errorf("shelve wrote to an issue drover does not own: %q", w)
	}
}

func TestUnshelveBringsTheIssueBack(t *testing.T) {
	dir := mirrorShelf(t, "github",
		map[string]string{"B02-dep.md": "---\ntitle: Dep\ndeps:\nissue: 40\n---\n\nRequirements:\n\n1. First.\n"},
		map[string]string{"F03-back.md": "---\ntitle: Back after all\ndeps: B2\nissue: 77\n---\n\n" +
			"Requirements:\n\n1. Edited on the shelf.\n"})
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Back after all","state":"CLOSED","body":`+jsonString(mirrorBody("F3", "1. As published."))+
			`,"labels":[{"name":"drover"},{"name":"drover:deferred"}],"blockedBy":{"nodes":[]}}`)
	t.Chdir(dir)

	if err := execute(t, "unshelve", "F3"); err != nil {
		t.Fatal(err)
	}
	if r := callsTo(t, log, "issue reopen 77"); len(r) != 1 {
		t.Errorf("#77 reopens: %q, want one", r)
	}
	if b := callsTo(t, log, "issue edit 77 --body"); len(b) != 1 || !strings.Contains(b[0], "1. Edited on the shelf.") {
		t.Errorf("#77 body edits: %q, want the requirements rewritten from the file", b)
	}
	if l := callsTo(t, log, "issue edit 77 --add-label drover:blocked"); len(l) != 1 ||
		!strings.Contains(l[0], "--remove-label drover:deferred") {
		t.Errorf("#77 label edits: %q, want drover:deferred swapped for drover:blocked", l)
	}
	if l := callsTo(t, log, "issue edit 77 --add-blocked-by"); len(l) != 1 || !strings.Contains(l[0], "40") {
		t.Errorf("#77 links: %q, want blocked by #40", l)
	}
}

// A scaffold comes back to its issue as one being redrafted: reopened and
// labeled, with the published requirements left for accept to replace.
func TestUnshelveKeepsAScaffoldsDraftOffItsIssue(t *testing.T) {
	dir := mirrorShelf(t, "github", map[string]string{},
		map[string]string{"F03-back.md": "---\ntitle: Back after all\ndeps:\nissue: 77\n---\n\n" +
			"STOP: requirements not yet written\n\nRequirements:\n\n1. Drafted on the shelf.\n"})
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "view-77.json"),
		`{"number":77,"title":"Back after all","state":"CLOSED","body":`+jsonString(mirrorBody("F3", "1. As published."))+
			`,"labels":[{"name":"drover"},{"name":"drover:deferred"}],"blockedBy":{"nodes":[]}}`)
	t.Chdir(dir)

	if err := execute(t, "unshelve", "F3"); err != nil {
		t.Fatal(err)
	}
	if b := callsTo(t, log, "issue edit 77 --body"); len(b) != 0 {
		t.Errorf("#77 body edits: %q, want none for a scaffold", b)
	}
	if r := callsTo(t, log, "issue reopen 77"); len(r) != 1 {
		t.Errorf("#77 reopens: %q, want one", r)
	}
	if l := callsTo(t, log, "issue edit 77 --add-label drover:draft"); len(l) != 1 ||
		!strings.Contains(l[0], "--remove-label drover:deferred") {
		t.Errorf("#77 label edits: %q, want drover:deferred swapped for drover:draft", l)
	}
}

// Only an open or unlabeled issue under a shelved body is a finding; one the
// shelve fully wrote is not.
func TestCheckReportsAnOpenIssueUnderAShelvedBody(t *testing.T) {
	dir := mirrorShelf(t, "github", map[string]string{}, map[string]string{
		"F03-open.md":   shelvable,
		"F04-closed.md": strings.Replace(shelvable, "issue: 77", "issue: 78", 1),
	})
	bin := ghPATH(t)
	stubMirrorGH(t, bin, false)
	writeFile(t, filepath.Join(bin, "list.json"), `[`+
		`{"number":77,"title":"Open","state":"OPEN","body":"","labels":[{"name":"drover"},{"name":"drover:deferred"}],"blockedBy":{"nodes":[]}},`+
		`{"number":78,"title":"Closed","state":"CLOSED","body":"","labels":[{"name":"drover"},{"name":"drover:deferred"}],"blockedBy":{"nodes":[]}}]`)

	got := findings(openIn(t, dir).checkReport(), "MIRROR SHELVED")
	if len(got) != 1 || got[0].ID != "F3" || !strings.Contains(got[0].Message, "still open") {
		t.Errorf("MIRROR SHELVED = %+v, want F3 alone, named as still open", got)
	}
}

func TestShelfVerbsNeverCallGHWithoutTheMirror(t *testing.T) {
	dir := mirrorShelf(t, "none", map[string]string{"F03-wont-do.md": shelvable}, map[string]string{})
	bin := ghPATH(t)
	log := stubMirrorGH(t, bin, false)
	t.Chdir(dir)

	for _, verb := range []string{"shelve", "unshelve"} {
		if err := execute(t, verb, "F3"); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}
	if calls := ghCalls(t, log); len(calls) != 0 {
		t.Errorf("gh was called with the mirror off: %q", calls)
	}
}
