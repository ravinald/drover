package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
)

const changesBody = `Requirements:

1. First requirement.
2. Second requirement.

## What the review sent back

The first pass missed the encoding.

## Notes

Something after.`

// The section is replaced, never appended to. A body carrying the gaps a later
// pass closed sends the next run to re-verify finished work, and it will,
// because the brief outranks its own reading of the tree.
func TestSpliceSectionReplacesRatherThanAppends(t *testing.T) {
	out := spliceSection(changesBody, "The second pass found the walk.")
	if strings.Contains(out, "missed the encoding") {
		t.Errorf("the closed finding survived:\n%s", out)
	}
	if !strings.Contains(out, "The second pass found the walk.") {
		t.Errorf("the new evidence is missing:\n%s", out)
	}
	if strings.Count(out, prompts.ChangesHeading) != 1 {
		t.Errorf("the heading must appear once:\n%s", out)
	}
	for _, keep := range []string{"1. First requirement.", "2. Second requirement.", "## Notes", "Something after."} {
		if !strings.Contains(out, keep) {
			t.Errorf("request for changes reached outside its section, lost %q:\n%s", keep, out)
		}
	}
}

// Twice in a row is once. A reviewer correcting their own evidence must not
// stack two sections the next run reads as both current.
func TestSpliceSectionIsIdempotent(t *testing.T) {
	once := spliceSection(changesBody, "evidence")
	twice := spliceSection(once, "evidence")
	if once != twice {
		t.Errorf("second request for changes changed the body:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestSpliceSectionAppendsWhenTheHeadingIsAbsent(t *testing.T) {
	out := spliceSection("Requirements:\n\n1. Only one.", "found a gap")
	if !strings.HasSuffix(out, "found a gap") {
		t.Errorf("the section must finish at the end:\n%s", out)
	}
	if n := len(backlog.Item{Body: out}.Requirements()); n != 1 {
		t.Errorf("appending changed the contract: %d requirements, want 1\n%s", n, out)
	}
}

// Requirements are parsed as top-level numbered lines wherever they appear, so
// findings written as a numbered list become requirements 3 and 4, and a review
// brief carries findings dressed as contract.
func TestDemoteNumberingKeepsFindingsOffTheContract(t *testing.T) {
	evidence := "1. **The walk skips a nil manifest.**\n   Reproduced at cmd.go:348.\n2. **The summary undercounts.**"
	got, n := demoteNumbering(evidence)
	if n != 2 {
		t.Errorf("demoted %d lines, want 2", n)
	}
	out := spliceSection(changesBody, got)
	reqs := backlog.Item{Body: out}.Requirements()
	if len(reqs) != 2 {
		t.Fatalf("the contract grew to %d requirements:\n%s", len(reqs), out)
	}
	if !strings.Contains(out, "- **The walk skips a nil manifest.**") {
		t.Errorf("the finding must survive as a bullet:\n%s", out)
	}
	if !strings.Contains(out, "   Reproduced at cmd.go:348.") {
		t.Errorf("a continuation line is not a list marker and must not move:\n%s", out)
	}
}

func TestDemoteNumberingLeavesProseAlone(t *testing.T) {
	evidence := "The walk skips it.\n\nSee cmd.go:348, where 1. is not a marker."
	got, n := demoteNumbering(evidence)
	if n != 0 || got != evidence {
		t.Errorf("demoted %d lines and rewrote the text:\n%s", n, got)
	}
}

func TestSplitFrontmatterKeepsTheFenceVerbatim(t *testing.T) {
	raw := "---\ntitle: A thing\ndeps: E5\n---\n\nRequirements:\n\n1. One.\n"
	head, body, err := splitFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if head != "---\ntitle: A thing\ndeps: E5\n---\n" {
		t.Errorf("frontmatter changed: %q", head)
	}
	if !strings.Contains(body, "1. One.") {
		t.Errorf("body lost its requirement: %q", body)
	}
}

func TestSplitFrontmatterRefusesAnUnfencedBody(t *testing.T) {
	if _, _, err := splitFrontmatter("Requirements:\n\n1. One.\n"); err == nil {
		t.Error("a body with no fence must not be rewritten")
	}
	if _, _, err := splitFrontmatter("---\ntitle: x\n\nRequirements:\n"); err == nil {
		t.Error("an unclosed fence must not be rewritten")
	}
}

// capProject declares a cap and one accepted item, with the evidence file the
// tests hand to request-changes.
func capProject(t *testing.T, limit int) (dir, evidence string) {
	t.Helper()
	s := fixture(t, fmt.Sprintf("\n[review]\nmax_review_rounds = %d\n\n[register]\nbackend = \"none\"\n", limit))
	dir = s.cfg.Root
	evidence = filepath.Join(dir, "findings.md")
	write(t, evidence, "The walk over-excludes on a nullable column.\n")
	t.Chdir(dir)
	return dir, evidence
}

func refusalRows(t *testing.T, dir string) int {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l.Counts("1").Refusals
}

func requestChangesRows(t *testing.T, dir string) int {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l.Counts("1").ChangesRequested
}

// One row per send-back, and it is the fifth shape: nothing else in the file
// reads it, and State must fold exactly as it did before.
func TestRequestChangesWritesOneRowPerSendBack(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	dir, evidence := capProject(t, 0)
	for i := 0; i < 3; i++ {
		requestChangesFile = ""
		if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
			t.Fatalf("request for changes %d: %v", i, err)
		}
	}
	if n := requestChangesRows(t, dir); n != 3 {
		t.Errorf("%d changes rows, want 3", n)
	}
	l, _ := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if st := l.State("1"); st.Merged || st.Level != "" || st.Implementer != "" {
		t.Errorf("a changes row reached State: %+v", st)
	}
}

// The same evidence twice is a review repeating itself, which the count alone
// cannot say.
func TestRequestChangesRowCarriesTheEvidenceDigest(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	dir, evidence := capProject(t, 0)
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	l, _ := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if !l.Counts("1").RepeatedChanges {
		t.Error("two identical requests for changes did not read as a repeat")
	}
	// A rewrap is the same finding, and a different one is not.
	write(t, evidence, "The walk over-excludes\non a nullable column.\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	l, _ = ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if !l.Counts("1").RepeatedChanges {
		t.Error("a reflowed paragraph read as a new finding")
	}
	write(t, evidence, "Something else entirely.\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	l, _ = ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if l.Counts("1").RepeatedChanges {
		t.Error("new evidence read as a repeat")
	}
}

// sevenRequirements is an item body with requirements 1 to 7, so evidence can
// name R4 and R7 without contractContradictions refusing it.
const sevenRequirements = "---\ntitle: First\ndeps:\n---\n\n" +
	"1. One.\n2. Two.\n3. Three.\n4. Four.\n5. Five.\n6. Six.\n7. Seven.\n"

// The row records the requirements the findings name, traced through the
// headings as the operator check traces them, and nothing for evidence whose
// findings name none.
func TestRequestChangesRowRecordsTheRequirementsItsFindingsName(t *testing.T) {
	t.Cleanup(func() { requestChangesFile = "" })
	dir, evidence := capProject(t, 0)
	write(t, filepath.Join(dir, "work", "01-first.md"), sevenRequirements)
	write(t, evidence, "### R7 refuses a non-place entity\n\nStill accepted.\n\n"+
		"### R4: a brewery linked to its place is unmet\n\nNo link is written.\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	write(t, evidence, "The walk over-excludes on a nullable column.\n")
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got [][]int
	for _, r := range l.Rows {
		if r.ID == "1" && r.Changes != "" {
			got = append(got, r.Reqs)
		}
	}
	if len(got) != 2 || !slices.Equal(got[0], []int{4, 7}) || got[1] != nil {
		t.Errorf("reqs per changes row = %v, want [[4 7] []]", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if n := strings.Count(string(raw), `"reqs"`); n != 1 {
		t.Errorf("%d rows carry a reqs key, want 1: evidence naming no requirement records none", n)
	}
}

// --still-open carries a finding over from the previous request only, so it
// refuses a number that request did not name, and a first request outright.
// Either refusal writes no changes row; one that holds records the list.
func TestRequestChangesStillOpenNamesOnlyThePreviousRequestsRequirements(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesStillOpen = "", nil })
	dir, evidence := capProject(t, 0)
	write(t, filepath.Join(dir, "work", "01-first.md"), sevenRequirements)
	send := func(text string, args ...string) error {
		write(t, evidence, text)
		requestChangesFile, requestChangesStillOpen = "", nil
		return execute(t, append([]string{"request-changes", "1", "-f", evidence}, args...)...)
	}

	err := send("### R3 answers 200\n\nProbe one.\n", "--still-open", "3")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "has none") {
		t.Errorf("--still-open on a first request = %v (exit %d), want exit 3 saying there is no previous one", err, Code(err))
	}
	if n := requestChangesRows(t, dir); n != 0 {
		t.Fatalf("%d changes rows after the refusal, want 0", n)
	}
	if err := send("### R3 answers 200\n\nProbe one.\n"); err != nil {
		t.Fatal(err)
	}
	err = send("### R5 answers 200\n\nProbe two.\n", "--still-open", "5")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "named [3]") {
		t.Errorf("--still-open 5 against reqs [3] = %v (exit %d), want exit 3 naming [3]", err, Code(err))
	}
	if n := requestChangesRows(t, dir); n != 1 {
		t.Fatalf("%d changes rows after the refusal, want 1", n)
	}
	if err := send("### R3 still answers 200\n\nProbe one again.\n", "--still-open", "3,3"); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var open [][]int
	for _, r := range l.Rows {
		if r.ID == "1" && r.Changes != "" {
			open = append(open, r.StillOpen)
		}
	}
	if len(open) != 2 || open[0] != nil || !slices.Equal(open[1], []int{3}) {
		t.Errorf("still_open per changes row = %v, want [[] [3]]", open)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if n := strings.Count(string(raw), `"still_open"`); n != 1 {
		t.Errorf("%d rows carry a still_open key, want 1: an empty list is omitted", n)
	}
}

// Exit 3 promises the tree and the ledger are untouched, and that is asserted
// rather than assumed.
func TestRequestChangesRefusesPastTheLimitAndWritesNothing(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	dir, evidence := capProject(t, 2)
	body := filepath.Join(dir, "work", "01-first.md")
	for i := 0; i < 2; i++ {
		requestChangesFile = ""
		if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
			t.Fatalf("under the cap: %v", err)
		}
	}
	before := read(t, body)
	write(t, evidence, "A third finding nobody will read.\n")

	requestChangesFile = ""
	err := execute(t, "request-changes", "1", "-f", evidence)
	if err == nil {
		t.Fatal("sent back past the cap")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if got := read(t, body); got != before {
		t.Error("the refusal rewrote the body")
	}
	if n := requestChangesRows(t, dir); n != 2 {
		t.Errorf("%d changes rows after the refusal, want 2", n)
	}
	// The refusal is evidence, not just a no. Without a row, a review that wanted
	// to send work back and was turned away leaves an audit and no request for changes row,
	// which is what a review that found nothing leaves, so passResult would read
	// every later pass on the item as capped and open-pr would refuse it, however
	// good the review.
	if n := refusalRows(t, dir); n != 1 {
		t.Errorf("%d cap refusal rows, want 1: the refusal has to be readable", n)
	}
	for _, want := range []string{"sent back 2 times", "max_review_rounds is 2", "drover audit", "--over-limit", "drover new"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, err)
		}
	}
}

// Absent and zero are one answer, and neither refuses.
func TestNoCapNeverRefuses(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	for _, limit := range []int{0} {
		_, evidence := capProject(t, limit)
		for i := 0; i < 6; i++ {
			requestChangesFile = ""
			if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
				t.Fatalf("cap %d, request for changes %d: %v", limit, i, err)
			}
		}
	}
}

// The one assertion that makes the cap a bound rather than a suggestion. A
// refusal naming its own override is an instruction, and the agreeable reviewer
// the brief warns about would take it.
func TestPastCapIsTheOperatorsAndSealedFromASession(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	dir, evidence := capProject(t, 1)
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	write(t, evidence, "A second finding.\n")

	t.Setenv(runner.EnvActor, "agent")
	requestChangesFile, requestChangesOverLimit = "", false
	err := execute(t, "request-changes", "1", "-f", evidence, "--over-limit")
	if err == nil {
		t.Fatal("a launched session voted itself past its own cap")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if n := requestChangesRows(t, dir); n != 1 {
		t.Errorf("%d changes rows, want 1", n)
	}

	// Sealing the flag must not seal the command: an agent requests changes below the cap.
	requestChangesFile, requestChangesOverLimit = "", false
	if err := execute(t, "request-changes", "1", "-f", evidence); err == nil {
		t.Error("a session under the cap was refused too")
	} else if !strings.Contains(err.Error(), "max_review_rounds") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// The operator's escape works, and says so.
func TestPastCapRecordsWhenTheOperatorRunsIt(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	dir, evidence := capProject(t, 1)
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	write(t, evidence, "A second finding.\n")
	requestChangesFile, requestChangesOverLimit = "", false
	out := capture(t, func() {
		if err := execute(t, "request-changes", "1", "-f", evidence, "--over-limit"); err != nil {
			t.Fatalf("--over-limit refused the operator: %v", err)
		}
	})
	if !strings.Contains(out, "--over-limit sent it back anyway") {
		t.Errorf("the override is silent:\n%s", out)
	}
	if n := requestChangesRows(t, dir); n != 2 {
		t.Errorf("%d changes rows, want 2", n)
	}
}

// Past the cap with no -f, the caller hears about the flag they can fix rather
// than the rule that is not theirs to decide.
func TestTheCapReportsAfterTheUsageErrors(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOverLimit = "", false })
	_, evidence := capProject(t, 1)
	requestChangesFile = ""
	if err := execute(t, "request-changes", "1", "-f", evidence); err != nil {
		t.Fatal(err)
	}
	requestChangesFile = ""
	err := execute(t, "request-changes", "1")
	if err == nil {
		t.Fatal("accepted a request for changes with no evidence")
	}
	if !strings.Contains(err.Error(), "-f <file>") {
		t.Errorf("named the cap before the missing flag: %v", err)
	}
}

// A reviewer's evidence can carry a "## " heading, and request-changes -f writes
// what it is given. spliceSection ends the section at the next "## ", so such a
// heading would split the block: the replacement lands above it and everything
// below stays, which leaves a run reading settled findings as current
// instructions.
//
// The evidence is what gets corrected, not the boundary. A project's prompt
// template writes its own "## " sections into a body and those survive request-changes,
// which TestSpliceSectionReplacesRatherThanAppends pins, and nothing about a
// heading's position says which of the two wrote it.
func TestEvidenceCannotSplitTheSectionItIsWrittenInto(t *testing.T) {
	evidence, _, headings := demoteEvidence("The pass found this.\n\n## Evidence\n\nWhat I traced.\n")
	if headings != 1 {
		t.Fatalf("demoted %d headings, want 1", headings)
	}
	body := "1. Do it.\n\n" + prompts.ChangesHeading + "\n\nolder findings\n"
	next := spliceSection(body, evidence)
	if strings.Contains(next, "older findings") {
		t.Errorf("the replacement stopped short and kept the old block:\n%s", next)
	}
	for _, want := range []string{"### Evidence", "What I traced.", "1. Do it."} {
		if !strings.Contains(next, want) {
			t.Errorf("the body lost %q:\n%s", want, next)
		}
	}
	// The round trip is the assertion: a second pass over the body it just wrote
	// has to replace the whole of it, which is what a "## " in there prevented.
	again := spliceSection(next, "Nothing is outstanding.")
	for _, gone := range []string{"### Evidence", "What I traced."} {
		if strings.Contains(again, gone) {
			t.Errorf("a second pass left %q behind:\n%s", gone, again)
		}
	}
}

// Prose written into a body must not create structure the body's own readers act
// on. demoteNumbering already held that line for a top-level "N."; a "## " is the
// same problem one level up: spliceSection ends a section at any "## " line.
func TestEvidenceHeadingsSinkBelowTheBodysOwn(t *testing.T) {
	out, numbered, headings := demoteEvidence("# Report\n\n## Finding\n\n1. The first.\n\n### Detail\n\nprose\n")
	if headings != 3 {
		t.Errorf("demoted %d headings, want 3 (every one, by the same amount)", headings)
	}
	if numbered != 1 {
		t.Errorf("demoted %d numbered lines, want 1", numbered)
	}
	for _, gone := range []string{"\n# Report", "\n## Finding"} {
		if strings.Contains("\n"+out, gone) {
			t.Errorf("a heading survived at the body's own level (%q):\n%s", gone, out)
		}
	}
	// A shift, not a floor: a floor would put all three at ### and make Detail a
	// sibling of the Finding it belongs to.
	for _, want := range []string{"\n### Report\n", "\n#### Finding\n", "\n##### Detail\n"} {
		if !strings.Contains("\n"+out, want) {
			t.Errorf("the headings lost their relative depth (want %q):\n%s", strings.TrimSpace(want), out)
		}
	}
}

// Evidence whose shallowest heading is already ### is the depth a body expects,
// so nothing moves; a fenced "## " still sinks, because spliceSection reads
// fenced lines too; and a "#62" opening a line is prose, not a heading.
func TestEvidenceHeadingsShiftOnlyWhenTheyMust(t *testing.T) {
	in := "### R3 cannot be met\n\n#### Reason\n\nprose\n\n```md\n## quoted\n```\n\n#62 fixed the rest\n"
	out, _, headings := demoteEvidence(in)
	if headings != 1 {
		t.Errorf("demoted %d headings, want 1 (the fenced one)", headings)
	}
	for _, want := range []string{"### R3 cannot be met\n", "\n#### Reason\n", "\n### quoted\n", "\n#62 fixed the rest"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q:\n%s", strings.TrimSpace(want), out)
		}
	}
	body := "1. Do it.\n\n" + prompts.ChangesHeading + "\n\nolder\n"
	if again := spliceSection(spliceSection(body, out), "Nothing is outstanding."); strings.Contains(again, "quoted") {
		t.Errorf("a fenced heading split the section:\n%s", again)
	}
}

// lastBodyMatches asserts the newest row in the ledger at ledgerPath is a body
// row for id carrying the digest of the file at path as it now stands.
func lastBodyMatches(t *testing.T, ledgerPath, id, path, after string) {
	t.Helper()
	l, err := ledger.Load(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := bodyDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Counts(id).LastBody; got != sum {
		t.Errorf("after %s: LastBody = %q, the body on disk digests %q", after, got, sum)
	}
	var last ledger.Row
	for _, r := range l.Rows {
		if r.ID == id && r.Body != "" {
			last = r
		}
	}
	if last.Stage != "" || last.Changes != "" || last.Level != "" {
		t.Errorf("after %s: the digest rode on another row rather than a body row of its own: %+v", after, last)
	}
}

// Every command that writes an item body records what it wrote, so a review
// that uses them is not read as one that edited the body by hand. split has a
// test of its own.
func TestEveryBodyWriterRecordsWhatItWrote(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, clearFile, invariantFile = "", "", "" })
	dir := draftProject(t, "")
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	first := filepath.Join(dir, "work", "01-first.md")
	scaffold := filepath.Join(dir, "work", "B01-empty-feed.md")
	notes := filepath.Join(dir, "notes.md")
	write(t, notes, "- The mode dropped to 0644.\n")

	for _, c := range []struct {
		args     []string
		id, path string
		set      func()
	}{
		{args: []string{"accept", "B1"}, id: "B1", path: scaffold},
		{args: []string{"unaccept", "B1"}, id: "B1", path: scaffold},
		{args: []string{"invariant", "1"}, id: "1", path: first, set: func() { invariantFile = notes }},
		{args: []string{"request-changes", "1"}, id: "1", path: first, set: func() { requestChangesFile = notes }},
		{args: []string{"close-changes", "1"}, id: "1", path: first, set: func() { requestChangesFile, clearFile = "", "" }},
	} {
		if c.set != nil {
			c.set()
		}
		if err := execute(t, c.args...); err != nil {
			t.Fatalf("%s: %v", strings.Join(c.args, " "), err)
		}
		lastBodyMatches(t, ledgerPath, c.id, c.path, c.args[0])
	}

	// link and resolve --link write the closes line, which this project does
	// not declare.
	for _, args := range [][]string{
		{"link", "B1", "259"},
		{"resolve", "B1", "260", "fixed", "--code-by", "agent", "--link", "shipped here"},
	} {
		t.Run(args[0], func(t *testing.T) {
			dir := linkProject(t, "")
			t.Chdir(dir)
			if err := execute(t, args...); err != nil {
				t.Fatalf("%s: %v", args[0], err)
			}
			lastBodyMatches(t, filepath.Join(dir, "ledger.jsonl"), "B1",
				filepath.Join(dir, "work", "B01-a-claim.md"), args[0])
		})
	}
}

// A doc comment that drifts onto the function below its own reads as that
// function's doc to go doc and leaves its own function with none, and no
// compiler or linter in the gate notices either half.
func TestRequestChangesDocCommentsOpenWithTheirOwnName(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "request_changes.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "init" {
			continue
		}
		name := fn.Name.Name
		if fn.Doc == nil {
			t.Errorf("func %s has no doc comment", name)
			continue
		}
		if doc := fn.Doc.Text(); !strings.HasPrefix(doc, name+" ") {
			first, _, _ := strings.Cut(doc, "\n")
			t.Errorf("func %s has a doc comment opening %q", name, first)
		}
	}
}

// --operator names requirements of the body's list, each carrying its reason
// under a heading in the evidence. A number past the list, or one no finding
// names, is refused and writes no row; one that holds records the list.
func TestRequestChangesOperatorNamesOnlyTheBodysRequirements(t *testing.T) {
	t.Cleanup(func() { requestChangesFile, requestChangesOperator = "", nil })
	dir, evidence := capProject(t, 0)
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. One.\n2. Two.\n3. Three.\n4. Four.\n5. Five.\n6. Six.\n")
	send := func(text string, args ...string) error {
		write(t, evidence, text)
		requestChangesFile, requestChangesOperator = "", nil
		return execute(t, append([]string{"request-changes", "1", "-f", evidence}, args...)...)
	}

	err := send("### R9 needs a decision\n\nNobody has made it.\n", "--operator", "9")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "has 6 numbered requirement(s)") {
		t.Errorf("--operator 9 on six requirements = %v (exit %d), want exit 3 naming the count", err, Code(err))
	}
	err = send("### R2 answers 200\n\nProbe one.\n", "--operator", "3")
	if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), "no finding in the evidence names R3") {
		t.Errorf("--operator 3 with no R3 finding = %v (exit %d), want exit 3", err, Code(err))
	}
	if n := requestChangesRows(t, dir); n != 0 {
		t.Fatalf("%d changes rows after the refusals, want 0", n)
	}
	if err := send("### R2 answers 200\n\nProbe one.\n\n### R3 needs a decision\n\nNobody has made it.\n", "--operator", "3,3"); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []ledger.Row
	for _, r := range l.Rows {
		if r.ID == "1" && r.Changes != "" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || !slices.Equal(rows[0].Operator, []int{3}) || !slices.Equal(rows[0].Reqs, []int{2, 3}) {
		t.Errorf("changes rows = %+v, want one with operator [3] and reqs [2 3]", rows)
	}
}

// The reason handed to the operator is the prose of the section a heading
// naming the requirement opens: subordinate headings and fenced code stay in
// that section without becoming the reason, a sibling heading ends it, and a
// section with no prose is its own title.
func TestOperatorReasonsReadTheWholeSectionOfTheFinding(t *testing.T) {
	for _, c := range []struct {
		name, text string
		nums       []int
		want       []string
	}{
		{"unheaded", "R3 needs a release. More.", []int{3}, []string{"R3: R3 needs a release."}},
		{"subordinate heading", "### R3 title\n\n#### Reason\n\nNo release exists. Amend it.\n", []int{3},
			[]string{"R3: No release exists."}},
		{"fence before prose", "### R3 title\n\n```\n# not a heading. Nor this.\n```\n\nThe prose. Rest.\n", []int{3},
			[]string{"R3: The prose."}},
		{"sibling ends the section", "### R3 title\n\n### R4 other\n\nR4's reason. Rest.\n", []int{3, 4},
			[]string{"R3: R3 title", "R4: R4's reason."}},
		{"named in a subordinate heading", "## Findings\n\n### R3 title\n\nIts reason. Rest.\n\n## Settled\n\nNot it.\n", []int{3},
			[]string{"R3: Its reason."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := operatorReasons(c.text, c.nums); !slices.Equal(got, c.want) {
				t.Errorf("operatorReasons = %q, want %q", got, c.want)
			}
		})
	}
}
