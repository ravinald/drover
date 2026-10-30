package cmd

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/prompts"
)

// Where a review finding goes is stated in five places, and nothing else holds
// them together. Two of them are templates an agent acts on (the review brief,
// and the AGENTS.md block it loads before the brief exists), and if they drift
// into stating opposite rules, say the brief sending an unmet requirement back
// to the branch while the generated instructions file it as a follow-up
// against its owner, whichever an agent reads first decides whether the
// backlog grows.
//
// The judge is prompts.Destinations(). The two templates render it, so their
// half of this test only proves the render still happens; the documents restate
// it in their own register and are pinned by token in both directions.
// concepts.md is the sixth place and states no routable rule at all: it links
// to lifecycle's table rather than repeating it, so what is pinned there is
// that the link still lands. A token set would catch a rename and miss the
// realistic failure, which is a paraphrase drifting out of agreement.
//
// Maintenance, when this fails: fix the copy the failure names. Never a list in
// this file; there is none.
const (
	guideDoc          = "../../docs/agent-guide.md"
	readmeDoc         = "../../README.md"
	gettingStartedDoc = "../../docs/getting-started.md"
	playbookDoc       = "../../docs/playbook.md"
)

// A destination token: all caps, at least one hyphen, in backticks. The hyphen
// is what keeps it apart from check's finding kinds, which are all caps with
// spaces and appear in the same documents.
var destinationToken = regexp.MustCompile("`([A-Z]+(?:-[A-Z]+)+)`")

// renderedRow matches one destination as a template actually emits it, at
// either indent. Counting them is what the per-token check below cannot do: a
// template naming every token in a hand-written sentence, with its range
// filtered or wrapped in an {{ if }}, passes on tokens alone and renders a list
// short of the value.
var renderedRow = regexp.MustCompile("(?m)^\\s*`[A-Z]+(?:-[A-Z]+)+` > ")

// docRow matches a destination where a document gives it a row of its own:
// lifecycle's table opens the cell with the token, and agent-guide's bullet
// opens the same way. Set membership alone cannot see a deleted row, because
// the rows cross-reference each other by token now and one row naming another
// keeps that other token in the section.
var docRow = regexp.MustCompile("(?m)^\\s*(?:\\||-) `([A-Z]+(?:-[A-Z]+)+)`[:\\s]")

// spelledCount is a count of the destinations written out by hand. Four copies
// of it said "Five" while the value carried five, and nothing would have
// reported them the day it carried six. The list is short enough to read whole,
// which is what the number was for; the prose says that instead.
var spelledCount = regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|\d+) destinations\b`)

func destinationsInCode(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, d := range prompts.Destinations() {
		if d.Token == "" || d.Found == "" || d.Do == "" || d.Where == "" {
			t.Errorf("destination %q has an empty field; every copy renders all four", d.Token)
		}
		if !destinationToken.MatchString("`" + d.Token + "`") {
			t.Errorf("token %q is not the shape the documents are scanned for", d.Token)
		}
		out[d.Token] = true
	}
	if len(out) != len(prompts.Destinations()) {
		t.Fatal("two destinations share a token")
	}
	return out
}

// section returns the lines from the one containing start up to the first line
// after it matching end. Bounded rather than whole-file: both documents name
// individual destinations elsewhere, and a set built from every mention would
// agree with the code by accident.
func section(t *testing.T, path, start string, end *regexp.Regexp) string {
	t.Helper()
	lines := strings.Split(read(t, path), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, start) })
	if i < 0 {
		t.Fatalf("%s: no line containing %q; the section moved or was reworded", path, start)
	}
	rest := lines[i+1:]
	j := slices.IndexFunc(rest, func(l string) bool { return end.MatchString(l) })
	if j < 0 {
		j = len(rest)
	}
	return strings.Join(lines[i:i+1+j], "\n")
}

func destinationsIn(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range destinationToken.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

func TestEveryCopyOfTheRoutingDoctrineAgrees(t *testing.T) {
	code := destinationsInCode(t)

	brief, err := prompts.Render("review", prompts.ReviewTemplate(), prompts.Data{
		Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test",
	})
	if err != nil {
		t.Fatalf("review.tmpl: %v", err)
	}
	instructions, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatalf("agents.tmpl: %v", err)
	}

	// The templates render the value, so what is checked is that they still
	// render it: prose as well as token, because a template naming the tokens
	// in a hand-written sentence would pass on tokens alone.
	for _, where := range []struct{ name, text string }{
		{"internal/prompt/review.tmpl", brief},
		{"internal/cmd/agents.tmpl", instructions},
	} {
		for _, d := range prompts.Destinations() {
			if !strings.Contains(where.text, d.Token) {
				t.Errorf("%s does not name %s", where.name, d.Token)
			}
			// Rendered prose is rewrapped, so compare on words.
			if !strings.Contains(strings.Join(strings.Fields(where.text), " "), strings.Join(strings.Fields(d.Found), " ")) {
				t.Errorf("%s renders %s without its prose", where.name, d.Token)
			}
		}
		if n := len(renderedRow.FindAllString(where.text, -1)); n != len(prompts.Destinations()) {
			t.Errorf("%s renders %d destinations, the value carries %d", where.name, n, len(prompts.Destinations()))
		}
		if m := spelledCount.FindString(where.text); m != "" {
			t.Errorf("%s counts the destinations by hand (%q); nothing updates that number", where.name, m)
		}
	}

	// The documents restate the same set in their own register, so only the
	// tokens are comparable, and both directions matter. A destination the
	// value gained and a document never learned is the drift this catches; a
	// destination a document keeps after the value dropped it is a rule nothing
	// implements.
	docs := map[string]map[string]bool{
		lifecycleDoc: destinationsIn(section(t, lifecycleDoc, "### Where a finding goes", regexp.MustCompile(`^#{1,6} `))),
		guideDoc:     destinationsIn(section(t, guideDoc, "**Send each finding to its owner.**", regexp.MustCompile(`^\d+\. `))),
	}
	for path, text := range map[string]string{
		lifecycleDoc: section(t, lifecycleDoc, "### Where a finding goes", regexp.MustCompile(`^#{1,6} `)),
		guideDoc:     section(t, guideDoc, "**Send each finding to its owner.**", regexp.MustCompile(`^\d+\. `)),
	} {
		if m := spelledCount.FindString(text); m != "" {
			t.Errorf("%s counts the destinations by hand (%q); nothing updates that number", path, m)
		}
		rows := map[string]bool{}
		for _, m := range docRow.FindAllStringSubmatch(text, -1) {
			rows[m[1]] = true
		}
		for _, d := range prompts.Destinations() {
			if !rows[d.Token] {
				t.Errorf("%s mentions %s but gives it no row of its own", path, d.Token)
			}
		}
	}

	// The sixth copy delegates instead of restating, so what is held is that the
	// delegation still lands. section() above fails if lifecycle's heading moves,
	// which is the other half of this anchor.
	if !strings.Contains(read(t, conceptsDoc), "lifecycle.md#where-a-finding-goes") {
		t.Errorf("%s no longer links to lifecycle's owner table, so nothing points a reader at the rule", conceptsDoc)
	}

	for _, path := range slices.Sorted(maps.Keys(docs)) {
		for _, token := range slices.Sorted(maps.Keys(code)) {
			if !docs[path][token] {
				t.Errorf("%s does not list %s, which prompt.Destinations() carries", path, token)
			}
		}
		for _, token := range slices.Sorted(maps.Keys(docs[path])) {
			if !code[token] {
				t.Errorf("%s lists %s, which prompt.Destinations() does not carry", path, token)
			}
		}
	}
}

// A finding filed with `drover new` and no -f leaves its description in the
// audit note on the item under review, which nobody opens when writing the new
// item's requirements. Every copy telling a reviewer to file one names -f, so
// no copy teaches the filing that loses the description.
func TestEveryFileNewCopyPassesTheDescription(t *testing.T) {
	var do string
	for _, d := range prompts.Destinations() {
		if d.Token == "NEW-ITEM" {
			do = d.Do
		}
	}
	instructions, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatalf("agents.tmpl: %v", err)
	}
	// Rendered prose is rewrapped, so the template is compared on words.
	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if !strings.Contains(words(instructions), words(do)) {
		t.Error("internal/cmd/agents.tmpl does not render NEW-ITEM's instruction as written")
	}

	lifecycle := section(t, lifecycleDoc, "| `NEW-ITEM` |", regexp.MustCompile(`^\| `))
	guide := section(t, guideDoc, "- `NEW-ITEM`:", regexp.MustCompile(`^\s*- `))

	// The tracker-free project is the one whose cap refusal sends a reviewer to
	// `new`; under the file register it names `follow-up open` instead. It changes
	// directory, which is why the documents are read above.
	newProject(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	refusal := s.capRefusal("1", 3, 3).Error()

	for _, c := range []struct{ name, text string }{
		{"prompt.Destinations() NEW-ITEM", do},
		{lifecycleDoc, lifecycle},
		{guideDoc, guide},
		{"request for changes cap refusal", refusal},
	} {
		if !strings.Contains(c.text, "drover new <kind>") || !strings.Contains(c.text, "-f <file>") {
			t.Errorf("%s files a finding without -f <file>:\n%s", c.name, c.text)
		}
	}
}

// Where `audit` sits relative to the merge is stated by hand in six places, and
// copies that disagree send a reviewer following the brief to audit before a
// merge the lifecycle document says to audit after. The
// judge is prompts.AuditPosition(); every copy below is read against it and
// none of them is the answer.
//
// The relation is checked rather than a sentence, because the copies state the
// order in several registers: an ASCII diagram, tables, briefs and command
// blocks. What they share is that each lists the commands in some
// order, and a copy that names two of the three steps still has to put those
// two the right way round.
//
// Maintenance, when this fails: move the step in the copy the failure names.
var auditPositionCopies = []struct {
	name, path, start string
	end               *regexp.Regexp
	drawn             bool // the shape diagram draws bare steps; the rest write commands
}{
	{name: "docs/lifecycle.md shape", path: lifecycleDoc, start: "write ", end: regexp.MustCompile("^```$"), drawn: true},
	{name: "docs/lifecycle.md loop table", path: lifecycleDoc, start: "### The whole loop", end: regexp.MustCompile(`^#{1,6} `)},
	{name: "docs/lifecycle.md three runners", path: lifecycleDoc, start: "### Three stages, three runners", end: regexp.MustCompile(`^#{1,6} `)},
	{name: "docs/agent-guide.md loop table", path: guideDoc, start: "## The loop, end to end", end: regexp.MustCompile(`^## `)},
	{name: "README.md item sequence", path: readmeDoc, start: "drover new bug", end: regexp.MustCompile("^```$")},
	{name: "docs/getting-started.md step 8", path: gettingStartedDoc, start: "## 8. Run, review", end: regexp.MustCompile(`^## `)},
	{name: "docs/playbook.md whole split", path: playbookDoc, start: "### The whole split, end to end", end: regexp.MustCompile(`^#{1,6} `)},
}

// stepIndex is where a copy first names one step. -1 means it does not, which
// is not a failure on its own: the worked example ends at the review pass and
// names no merge, and holding it to one would be asking for a line about
// merging in a block about runners.
func stepIndex(text, step string, drawn bool) int {
	if drawn {
		if loc := regexp.MustCompile(`\b` + step + `\b`).FindStringIndex(text); loc != nil {
			return loc[0]
		}
		return -1
	}
	return strings.Index(text, "drover "+step)
}

func TestEveryCopyOfTheAuditPositionAgrees(t *testing.T) {
	pos := prompts.AuditPosition()

	brief, err := prompts.Render("review", prompts.ReviewTemplate(), prompts.Data{
		Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test",
	})
	if err != nil {
		t.Fatalf("review.tmpl: %v", err)
	}
	instructions, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatalf("agents.tmpl: %v", err)
	}

	copies := []struct {
		name, text string
		drawn      bool
	}{
		{name: "internal/prompt/review.tmpl", text: brief},
		{name: "internal/cmd/agents.tmpl", text: instructions},
		{name: "drover --help", text: root.Example},
	}
	for _, src := range auditPositionCopies {
		copies = append(copies, struct {
			name, text string
			drawn      bool
		}{src.name, section(t, src.path, src.start, src.end), src.drawn})
	}

	for _, c := range copies {
		at := stepIndex(c.text, pos.Step, c.drawn)
		if at < 0 {
			t.Errorf("%s never names %s, so it walks the lifecycle past the one row that records judgement", c.name, pos.Step)
			continue
		}
		for _, earlier := range pos.After {
			if i := stepIndex(c.text, earlier, c.drawn); i > at {
				t.Errorf("%s puts %s after %s; the audit level closes the review pass, so nothing in it follows the audit", c.name, earlier, pos.Step)
			}
		}
		for _, later := range pos.Before {
			if i := stepIndex(c.text, later, c.drawn); i >= 0 && i < at {
				t.Errorf("%s puts %s before %s; the audit level is written while the branch is still there", c.name, later, pos.Step)
			}
		}
	}
}

// Every [usage] and [t3] key this build reads is documented where an operator
// looks for it. The keys are walked off the Config types, so a key added there
// and not to the page fails here rather than reading as undocumented silence.
func TestEveryUsageAndT3KeyIsInTheConfigurationReference(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, k := range config.Keys() {
		if !strings.HasPrefix(k.Dotted, "usage.") && !strings.HasPrefix(k.Dotted, "t3.") {
			continue
		}
		if !strings.Contains(doc, "`"+k.Name+"`") {
			t.Errorf("docs/configuration.md never names `%s` (%s)", k.Name, k.Dotted)
		}
		if !strings.Contains(doc, "["+k.Table+"]") {
			t.Errorf("docs/configuration.md never names the table [%s] that %s sits in", k.Table, k.Dotted)
		}
	}
}

// An unmerged item has no bugs: a defect in its own work is unfinished
// implementation and goes back to its branch, and only a gap in merged code is
// filed. The prompts once routed a harmless diff defect to NEW-ITEM, told a
// capped reviewer to file against the item under review, and offered filing
// for a marginal finding on a late pass, and the backlog filled with items
// found while their source was still open. Each phrase below is one of those
// routes; the render is checked capped and uncapped because the cap block is
// where the filing route lived.
func TestNoReviewPromptFilesAnUnmergedItemsOwnWork(t *testing.T) {
	rows := map[string]prompts.Destination{}
	for _, d := range prompts.Destinations() {
		rows[d.Token] = d
	}
	regression, item := rows["BRANCH-REGRESSION"], rows["NEW-ITEM"]

	if !strings.HasPrefix(regression.Found, "Any defect this branch's own diff introduced") {
		t.Errorf("BRANCH-REGRESSION no longer takes every defect the diff introduced:\n%s", regression.Found)
	}
	for _, gone := range []string{"that would break something in use", "`NEW-ITEM`"} {
		if strings.Contains(regression.Found, gone) || strings.Contains(regression.Do, gone) {
			t.Errorf("BRANCH-REGRESSION still conditions or reroutes a diff defect (%q)", gone)
		}
	}
	if !strings.HasPrefix(item.Found, "A gap in merged code that no existing item owns") {
		t.Errorf("NEW-ITEM takes more than a gap in merged code:\n%s", item.Found)
	}
	if !strings.Contains(item.Do, "without `--found-in`") || strings.Contains(item.Do, "--found-in <id>") {
		t.Errorf("NEW-ITEM files with --found-in, naming an unmerged item as the source:\n%s", item.Do)
	}
	if !strings.Contains(rows["FOLLOW-UP-TO-OWNER"].Found, "an item that has merged owns") {
		t.Error("FOLLOW-UP-TO-OWNER no longer names an owner that has merged")
	}

	banned := []string{"let the branch merge", "file it against this item", "becomes `NEW-ITEM`", "row that ends a review loop"}
	base := prompts.Data{
		Project: "example", ID: "1", Title: "First", Body: "1. Do it.", VerifyCommand: "make test",
		Path: "docs-internal/backlog/01-first.md",
	}
	capped := base
	capped.Rounds, capped.MaxReviewRounds, capped.Capped = 3, 3, true
	capped.Runs = 4

	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for name, data := range map[string]prompts.Data{"uncapped": base, "capped": capped} {
		brief, err := prompts.Render("review", prompts.ReviewTemplate(), data)
		if err != nil {
			t.Fatalf("review.tmpl %s: %v", name, err)
		}
		flat := words(brief)
		for _, phrase := range banned {
			if strings.Contains(flat, phrase) {
				t.Errorf("the %s review prompt still says %q", name, phrase)
			}
		}
		for _, want := range []string{
			"drover request-changes 1 -f <file>",
			"carries a test that fails on the branch as it stands",
			"check the run's requirement-to-test map",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("the %s review prompt does not say %q", name, want)
			}
		}
		if data.Capped && !strings.Contains(flat, "refuse the send-back, keep the findings and park the item") {
			t.Error("the capped review prompt does not say drover parks the item with the findings kept")
		}
	}

	instructions, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatalf("agents.tmpl: %v", err)
	}
	for _, phrase := range banned {
		if strings.Contains(words(instructions), phrase) {
			t.Errorf("internal/cmd/agents.tmpl still says %q", phrase)
		}
	}

	implement, err := prompts.Render("implement", prompts.DefaultTemplate(), base)
	if err != nil {
		t.Fatalf("Default.tmpl: %v", err)
	}
	for _, want := range []string{
		"has a test that fails without the change",
		"a requirement with no test is reported unmet",
		"committed unchanged and passes",
	} {
		if !strings.Contains(words(implement), want) {
			t.Errorf("the implement prompt does not say %q", want)
		}
	}
}
