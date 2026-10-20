package prompts

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
)

func TestSelectVerifyMatchesAFrontmatterFlag(t *testing.T) {
	gates := map[string]string{
		"default":   "go build ./... && npm run build",
		"web_only":  "npm run build",
		"migration": "makemigrations --check",
	}
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"no flag set", map[string]string{"web_only": "no"}, gates["default"]},
		{"flag set", map[string]string{"web_only": "yes"}, gates["web_only"]},
		{"different flag", map[string]string{"migration": "yes"}, gates["migration"]},
		{"empty meta", map[string]string{}, gates["default"]},
		// Sorted order decides, so the result is stable rather than map-random.
		{"two flags", map[string]string{"migration": "yes", "web_only": "yes"}, gates["migration"]},
	}
	for _, c := range cases {
		if got := SelectVerify(gates, c.meta); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSelectVerifyWithNoVerifyCommandsDeclared(t *testing.T) {
	if got := SelectVerify(map[string]string{}, map[string]string{"web_only": "yes"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func sample() Data {
	return Data{
		Project: "webapp", ID: "F11", Stem: "F11", Slug: "review-attribution",
		Title: "A review never says who wrote it", Branch: "f11-review-attribution",
		Body: "Requirements:\n\n1. Do the thing.", Planned: false,
		Path:  ".drover/prompt-bodies/F11-review-attribution.md",
		Meta:  map[string]string{"kind": "feature", "severity": "M"},
		Deps:  []Dep{{ID: "F5", Done: true, Report: ".drover/reports/F05-x.md"}, {ID: "11", Done: true}},
		Specs: []string{".drover/design_specifications.md"},
		Skills: []Skill{
			{Name: "golang-security", Path: ".drover/skills/golang-security/SKILL.md"},
			{Name: "dataviz"},
		},
		VerifyCommand: "make codegen && go build ./...",
		FollowUps: []Issue{
			{Number: 238, Title: "One unparseable review empties the feed", Severity: "L"},
			{Number: 81, Title: "A broadcast reaches nobody"},
		},
	}
}

func TestDefaultTemplateRenders(t *testing.T) {
	out, err := Render("default", DefaultTemplate(), sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are executing F11: A review never says who wrote it.",
		".drover/design_specifications.md",
		"--- F11 ---",
		"1. Do the thing.",
		"--- END F11 ---",
		".drover/skills/golang-security/SKILL.md",
		"dataviz (not vendored",
		"#238 [L] One unparseable review empties the feed",
		"#81 [-] A broadcast reaches nobody",
		".drover/reports/F05-x.md",
		"make codegen && go build ./...",
		"f11-review-attribution",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

// The shell driver prefixed every dep with "P", producing forms like "PF5" that
// match no label and no command-line argument. Ids render bare.
func TestDepsRenderBare(t *testing.T) {
	out, err := Render("default", DefaultTemplate(), sample())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Depends on (already merged): F5 11.") {
		t.Errorf("deps line wrong:\n%s", out)
	}
	if strings.Contains(out, "PF5") {
		t.Error("dep ids must not carry a P prefix")
	}
}

func TestTemplateOmitsEmptySections(t *testing.T) {
	bare := Data{ID: "1", Title: "Schema", Body: "1. Make it.", Planned: true, Meta: map[string]string{}}
	out, err := Render("default", DefaultTemplate(), bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"Depends on", "Blocked by", "could not read", "Before writing code", "Before you start", "Open follow-ups", "Dependency self-reports"} {
		if strings.Contains(out, absent) {
			t.Errorf("section %q should be omitted when it has no content:\n%s", absent, out)
		}
	}

	// Deps that all merged and all parse leave nothing for the other two dep
	// sections to say. A brief that prints an empty "Blocked by" heading is the
	// same defect as one that prints a false claim: prose the tree does not
	// support.
	clean := bare
	clean.Deps = []Dep{{ID: "2", Done: true}, {ID: "F3", Done: true}}
	out, err = Render("default", DefaultTemplate(), clean)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Depends on (already merged): 2 F3.") {
		t.Errorf("merged deps should still be named:\n%s", out)
	}
	for _, absent := range []string{"Blocked by", "could not read"} {
		if strings.Contains(out, absent) {
			t.Errorf("section %q should be omitted when every dep merged and parses:\n%s", absent, out)
		}
	}

	// Same for the review brief, which carries its own copy of all three.
	rev := reviewSample()
	rev.Deps = clean.Deps
	out, err = Render("review", ReviewTemplate(), rev)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"Not merged", "could not read"} {
		if strings.Contains(out, absent) {
			t.Errorf("review section %q should be omitted when every dep merged and parses:\n%s", absent, out)
		}
	}
}

func TestRenderRejectsAnUnknownField(t *testing.T) {
	// A template naming a field that does not exist must fail loudly rather than
	// hand an agent a prompt with a hole in it.
	if _, err := Render("t", "{{ .NotAField }}", sample()); err == nil {
		t.Error("expected an error for an unknown field")
	}
}

func TestRenderRejectsAnEmptyResult(t *testing.T) {
	if _, err := Render("t", "{{ if false }}x{{ end }}", sample()); err == nil {
		t.Error("a template that renders nothing is a bug worth catching")
	}
}

func reviewSample() Data {
	d := sample()
	d.Requirements = []backlog.Requirement{
		{Num: 1, Text: "Fail first, at the store."},
		{Num: 2, Text: "One admission rule."},
	}
	// Both merged: a foundation is a dep the ledger recorded done, and the level
	// half of the line only renders for one.
	d.Deps = []Dep{
		{ID: "2b", Done: true, Level: "reviewed", By: "", Weak: true, Report: ".drover/reports/02b-x.md"},
		{ID: "12", Done: true, Level: "verified", By: "agent", Independent: true, Weak: false},
	}
	return d
}

func TestReviewTemplateRenders(t *testing.T) {
	d := reviewSample()
	d.SelfReport = "shipped it"
	d.Outcome = "success · 12 turns"
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Review F11:",
		"- [ ] R1: Fail first, at the store.",
		"- [ ] R2: One admission rule.",
		"treat what it claims as unproven", // the weak dep
		"audited verified (code by agent)",
		"--- what the run claimed ---",
		"shipped it",
		"drover audit F11",
		"drover finish F11",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

// A log with no result event is not the same as never having run, and a
// reviewer told the wrong one reviews the wrong tree.
func TestReviewTemplateDistinguishesUnfinishedFromNeverRan(t *testing.T) {
	d := reviewSample()
	d.Unfinished = true
	d.LogPath = "/logs/x.jsonl"
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "still in flight or it died partway") {
		t.Errorf("unfinished run not reported:\n%s", out)
	}

	d.Unfinished, d.LogPath = false, ""
	out, err = Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No run log for this item") {
		t.Errorf("never-ran not reported:\n%s", out)
	}
}

func TestReviewTemplateHandlesNoRequirements(t *testing.T) {
	d := reviewSample()
	d.Requirements = nil
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no numbered requirements") {
		t.Errorf("an item with no contract must say so rather than show an empty list:\n%s", out)
	}
}

// A re-review needs the prior pass verbatim, framed as something to check
// against rather than inherit. Which requirements it traced is prose drover
// cannot parse, so the note rides whole.
func TestReviewTemplateCarriesThePriorAuditVerbatim(t *testing.T) {
	d := reviewSample()
	d.Level, d.By, d.Independent, d.AuditDate = "reviewed", "agent", true, "2026-08-02"
	d.Note = "Traced R2, R3, R7; did not trace R1, R4, R5."
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a prior pass already audited this",
		"reviewed · code by agent · 2026-08-02",
		d.Note,
		"trace every requirement above **yourself**",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestReviewTemplateOmitsThePriorBlockWhenUnaudited(t *testing.T) {
	out, err := Render("review", ReviewTemplate(), reviewSample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a prior pass") {
		t.Errorf("an unaudited item must not claim a prior pass:\n%s", out)
	}
}

// The review brief opens with a role, derived from the item's own skills, and
// names the reviewer's characteristic failure. A builder fails by being wrong;
// a reviewer fails by being agreeable, and the brief has to push against that.
func TestReviewTemplateSetsAnAdversarialRole(t *testing.T) {
	out, err := Render("review", ReviewTemplate(), reviewSample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Act as a staff engineer conducting an independent audit",
		"golang-security", // domain comes from the item's skills
		"agreeableness",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestReviewRoleOmitsDomainWhenNoSkills(t *testing.T) {
	d := reviewSample()
	d.Skills = nil
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "independent audit.") {
		t.Errorf("an item with no skills must still get a clean role line:\n%s", out)
	}
}

// A reviewer that rebuilds a stack or seeds fixture rows and stays quiet leaves
// the next run debugging phantom data.
func TestReviewTemplateAsksForSideEffects(t *testing.T) {
	out, err := Render("review", ReviewTemplate(), reviewSample())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Declare anything you changed outside the branch") {
		t.Errorf("side effects must be requested:\n%s", out)
	}
}

// A finding that goes back to the branch reaches the next run through the item
// body and nowhere else. The reviewer's report is not in the brief, the audit
// note is not in the brief, and a run given neither rediscovers the gap or
// ships past it a second time.
func TestReviewTemplateSendsFindingsIntoTheItemBody(t *testing.T) {
	out, err := Render("review", ReviewTemplate(), reviewSample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".drover/prompt-bodies/F11-review-attribution.md",
		ChangesHeading,
		"drover request-changes F11 -f <file>",
		"replaces that section rather than appending",
		"refuses to change",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

// With no path there is no file to name, and an instruction to write into a
// blank is worse than none: it reads as a real destination.
func TestReviewTemplateOmitsTheBodyInstructionWithNoPath(t *testing.T) {
	d := reviewSample()
	d.Path = ""
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "## What the review sent back") {
		t.Errorf("the instruction must not render without a path:\n%s", out)
	}
}

// Three dep sections ask three questions, and a dep that answers two of them
// still belongs in one place. A brief listing every answer would vouch for a
// body it could not read, under a heading claiming this item rests on it, then
// name the same dep two sections later as unreadable.
func TestReviewTemplateListsEachDepOnce(t *testing.T) {
	d := reviewSample()
	d.Deps = []Dep{
		{ID: "B2", Done: true, Level: "reviewed", By: "agent", Independent: true},
		{ID: "B5", Done: true, Level: "verified", By: "agent", Independent: true, Unreadable: "is shelved"},
		{ID: "B7", Done: false},
		{ID: "B9", Done: false, Unreadable: "has no body"},
	}
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}

	heads := []string{"Foundations.", "Not merged, so not foundations.", "Deps drover could not read.", "The contract."}
	at := make([]int, len(heads))
	for i, h := range heads {
		if at[i] = strings.Index(out, h); at[i] < 0 {
			t.Fatalf("section %q missing:\n%s", h, out)
		}
	}
	sections := []struct{ name, body string }{
		{"Foundations", out[at[0]:at[1]]},
		{"Not merged", out[at[1]:at[2]]},
		{"Unreadable", out[at[2]:at[3]]},
	}

	for _, dep := range d.Deps {
		var in []string
		for _, s := range sections {
			if strings.Contains(s.body, "- "+dep.ID) {
				in = append(in, s.name)
			}
		}
		if len(in) != 1 {
			t.Errorf("%s appears in %v, want exactly one section:\n%s", dep.ID, in, out)
		}
	}
}

// A register block that points one way names the rows already open so a review
// would not re-file one, and asks nothing about the rows this branch fixed.
// Work like that merges with the report still open and nobody told, because
// `closes` is the only list finish reads.
func TestReviewTemplateAsksForADispositionOnFollowUpsThisItemOwns(t *testing.T) {
	d := reviewSample()
	d.FollowUps = []Issue{
		{Number: 81, Title: "this item's gap", Severity: "L", Owners: []string{"F11"}, Mine: true},
		{Number: 90, Title: "a dep's gap", Severity: "M", Owners: []string{"12"}},
	}
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}

	owned, inherited, ok := strings.Cut(out, "Open follow-ups owned by this item's dependencies")
	if !ok {
		t.Fatalf("the prompt does not separate owned follow-ups from inherited ones:\n%s", out)
	}
	for _, want := range []string{
		"#81 [L] this item's gap",
		"drover resolve F11 81 <fixed|partial|unrelated> --code-by <who> --link",
	} {
		if !strings.Contains(owned, want) {
			t.Errorf("owned block missing %q\n---\n%s", want, owned)
		}
	}
	if strings.Contains(owned, "#90") {
		t.Errorf("a dep's row rendered as this item's own:\n%s", owned)
	}
	if !strings.Contains(inherited, "#90 [M] a dep's gap · owned by 12") {
		t.Errorf("inherited block does not name the owner:\n%s", inherited)
	}
	// Pre-filling resolve for an inherited row invites a claim on somebody
	// else's report; the block says --link accepts it, and stops there.
	if strings.Contains(inherited, "drover resolve F11 90") {
		t.Errorf("inherited row carries a pre-filled claim:\n%s", inherited)
	}
}

// A register with nothing routed to this item renders no discharge prompt at
// all: a heading over an empty list reads as a question the reviewer failed to
// answer.
func TestReviewTemplateOmitsTheDischargeBlockWithNoOwnedFollowUps(t *testing.T) {
	d := reviewSample()
	d.FollowUps = []Issue{{Number: 90, Title: "a dep's gap", Owners: []string{"12"}}}
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Open follow-ups owned by F11") {
		t.Errorf("discharge block rendered with nothing to discharge:\n%s", out)
	}
	if !strings.Contains(out, "#90") {
		t.Errorf("inherited row went missing:\n%s", out)
	}
}

// TestSampleFillsData is what keeps Sample current. A field added to Data, or
// to a type Data reaches, and left zero in Sample is a field check renders no
// template against: a stale reference to it passes for as long as nobody adds
// it here.
func TestSampleFillsData(t *testing.T) {
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				walk(path+"."+v.Type().Field(i).Name, v.Field(i))
			}
		case reflect.Slice:
			if v.Len() != 1 {
				t.Errorf("Sample()%s holds %d elements, want 1", path, v.Len())
				return
			}
			walk(path+"[0]", v.Index(0))
		case reflect.Map:
			if v.Len() != 1 {
				t.Errorf("Sample()%s holds %d entries, want 1", path, v.Len())
				return
			}
			iter := v.MapRange()
			for iter.Next() {
				walk(path+"[key]", iter.Key())
				walk(path+"[value]", iter.Value())
			}
		default:
			if v.IsZero() {
				t.Errorf("Sample()%s is the zero value", path)
			}
		}
	}
	walk("", reflect.ValueOf(Sample()))
}

func TestBuiltInTemplatesRenderAgainstTheSample(t *testing.T) {
	for name, text := range map[string]string{
		"default": DefaultTemplate(),
		"draft":   DraftTemplate(),
		"review":  ReviewTemplate(),
	} {
		if _, err := Render(name, text, Sample()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Under a merge gate the merge already waits on a green head, so the brief
// must not send the reviewer through the full verify command a second time.
// Without one, the reviewer's run is the only one before a manual merge.
func TestReviewBriefLeavesTheFullGateToAMergeGate(t *testing.T) {
	d := reviewSample()
	d.MergeGate = "./scripts/merge-gate.sh"
	out, err := Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Re-run the verify command yourself") {
		t.Errorf("a brief under a merge gate still asks for the full verify command\n%s", out)
	}
	if !strings.Contains(out, "the merge waits on `./scripts/merge-gate.sh`") {
		t.Errorf("a brief under a merge gate does not name it\n%s", out)
	}

	d.MergeGate = ""
	out, err = Render("review", ReviewTemplate(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- Re-run the verify command yourself: "+d.VerifyCommand+"\n") {
		t.Errorf("a brief with no merge gate lost the full verify command\n%s", out)
	}
}
