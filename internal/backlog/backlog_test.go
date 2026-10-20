package backlog

import (
	"os"
	"strings"
	"testing"
)

// testSchema stands in for a project's declared frontmatter.
type testSchema struct {
	known    []string
	required []string
}

func (s testSchema) Known(k string) bool {
	for _, x := range s.known {
		if x == k {
			return true
		}
	}
	return false
}
func (s testSchema) RequiredKeys() []string { return s.required }

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

var sampleSchema = testSchema{
	known: []string{
		"title", "phase", "wave", "spec", "deps", "contract", "migration",
		"web_only", "skills", "agent", "kind", "issue", "severity",
	},
	required: []string{"title", "spec", "deps"},
}

func TestParseFrontmatter(t *testing.T) {
	src := `---
title: Consumer CRUD & Solicit-Recommendation API
spec: design_specifications.md api_specifications.md
deps: 2
contract: yes
---

Act as a Senior Go Backend Developer.

Requirements:

1. Extend the contract.
`
	meta, body, err := ParseFrontmatter("t.md", []byte(src), sampleSchema)
	if err != nil {
		t.Fatal(err)
	}
	if meta["title"] != "Consumer CRUD & Solicit-Recommendation API" {
		t.Errorf("title = %q", meta["title"])
	}
	if !strings.HasPrefix(body, "Act as a Senior") {
		t.Errorf("body should start at the first authored line, got %q", firstLine(body))
	}
	if strings.HasSuffix(body, "\n") {
		t.Error("body should not carry a trailing newline")
	}
}

func TestParseFrontmatterKeepsColonsInTitles(t *testing.T) {
	src := "---\ntitle: P22: the map boundary\nspec:\ndeps:\n---\n\nbody\n"
	meta, _, err := ParseFrontmatter("t.md", []byte(src), sampleSchema)
	if err != nil {
		t.Fatal(err)
	}
	if meta["title"] != "P22: the map boundary" {
		t.Errorf("only the first colon splits, got %q", meta["title"])
	}
}

func TestParseFrontmatterRejects(t *testing.T) {
	cases := map[string]string{
		"no fence":         "title: x\n",
		"unclosed fence":   "---\ntitle: x\nspec:\ndeps:\n",
		"unknown key":      "---\ntitle: x\nspec:\ndeps:\nnope: 1\n---\n",
		"slug key":         "---\ntitle: x\nspec:\ndeps:\nslug: y\n---\n",
		"missing required": "---\ntitle: x\nspec:\n---\n",
		"empty title":      "---\ntitle:\nspec:\ndeps:\n---\n",
		"not key value":    "---\ntitle: x\nspec:\ndeps:\njust a line\n---\n",
		"duplicate key":    "---\ntitle: x\nspec:\ndeps:\ndeps: 2\n---\n",
	}
	for name, src := range cases {
		if _, _, err := ParseFrontmatter("t.md", []byte(src), sampleSchema); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A required key that is present but empty is valid. That distinction is the
// whole point: presence proves the author considered it, so a typo cannot
// masquerade as "no dependencies".
func TestRequiredKeysMayBeEmpty(t *testing.T) {
	src := "---\ntitle: x\nspec:\ndeps:\n---\n\nbody\n"
	meta, _, err := ParseFrontmatter("t.md", []byte(src), sampleSchema)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["deps"]; !ok {
		t.Error("an empty required key must still be recorded as present")
	}
}

// The live corpus is the fixture. Point DROVER_CORPUS_FIXTURE at a bodies
// directory to check drover reads a real backlog with nothing broken.
func TestLoadRealCorpus(t *testing.T) {
	dir := os.Getenv("DROVER_CORPUS_FIXTURE")
	if dir == "" {
		t.Skip("set DROVER_CORPUS_FIXTURE to a bodies directory to run")
	}
	c, err := Load(dir, prefixes, sampleSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range c.Broken {
		t.Errorf("unreadable: %s: %v", b.Path, b.Err)
	}
	t.Logf("%d items, %d broken", len(c.Items), len(c.Broken))
	if len(c.Items) == 0 {
		t.Fatal("no items loaded")
	}

	// Ordering is what the status table depends on.
	for i := 1; i < len(c.Items); i++ {
		if !c.Items[i-1].ID.Less(c.Items[i].ID) {
			t.Fatalf("corpus out of order at %d: %s then %s",
				i, c.Items[i-1].ID.Bare(), c.Items[i].ID.Bare())
		}
	}

	// Every dep must name an item that exists, or the graph has a dangling edge
	// that reads as a permanently blocked item.
	have := map[string]bool{}
	for _, it := range c.Items {
		have[it.ID.Bare()] = true
	}
	for _, it := range c.Items {
		for _, d := range it.Deps() {
			if !have[d] {
				t.Errorf("%s depends on %q, which is not in the backlog", it.ID.Bare(), d)
			}
		}
	}
}

func TestRequirementsAreStructuredAndPlain(t *testing.T) {
	it := Item{Body: `Preamble prose.

Requirements:

1. **Fail first, at the store.** Write an integration case that fails on HEAD.
   This continuation belongs to R1, not to a new requirement.
2. One admission rule, read and write.
10. __Gates.__ Everything green.

Not a requirement: 1999. was a year.
`}
	got := it.Requirements()
	if len(got) != 3 {
		t.Fatalf("got %d requirements, want 3: %+v", len(got), got)
	}
	if got[0].Num != 1 || got[2].Num != 10 {
		t.Errorf("numbers wrong: %+v", got)
	}
	// Emphasis markers are source, not content; a checklist entry reads as a claim.
	for _, r := range got {
		if strings.Contains(r.Text, "**") || strings.Contains(r.Text, "__") {
			t.Errorf("R%d still carries emphasis markers: %q", r.Num, r.Text)
		}
	}
	if !strings.HasPrefix(got[0].Text, "Fail first, at the store.") {
		t.Errorf("R1 = %q", got[0].Text)
	}
	if strings.Contains(got[0].Text, "continuation") {
		t.Error("a continuation line must not become its own requirement")
	}
}

func TestRequirementsEmptyWhenNoneNumbered(t *testing.T) {
	if got := (Item{Body: "Just prose."}).Requirements(); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

// Severity orders a view; it does not gate correctness. A backlog that will not
// open because an audit tool wrote "Sev1" instead of "H" is a worse outcome
// than an uncoloured cell, so every one of these must parse clean.
func TestSeverityValuesNeverBreakParsing(t *testing.T) {
	for name, value := range map[string]string{
		"letter":      "C",
		"lowercase":   "c",
		"word":        "Critical",
		"unranked":    "Sev1",
		"punctuation": "P0/blocker",
		"empty":       "",
	} {
		src := "---\ntitle: t\nspec: s\ndeps:\nseverity: " + value + "\n---\n\nbody\n"
		meta, _, err := ParseFrontmatter("x.md", []byte(src), sampleSchema)
		if err != nil {
			t.Errorf("%s: severity %q failed to parse: %v", name, value, err)
			continue
		}
		if meta["severity"] != value {
			t.Errorf("%s: severity = %q, want %q, carried verbatim", name, meta["severity"], value)
		}
	}

	// Omitted entirely reads as empty, not as an error, because severity is
	// optional and Get on a missing key is how every optional field behaves.
	meta, _, err := ParseFrontmatter("x.md", []byte("---\ntitle: t\nspec: s\ndeps:\n---\n\nbody\n"), sampleSchema)
	if err != nil {
		t.Fatalf("an item with no severity failed to parse: %v", err)
	}
	if got := (Item{Meta: meta}).Get("severity"); got != "" {
		t.Errorf("Get(severity) = %q on an item that declares none, want empty", got)
	}
}

func TestParseDepsNamesTheIDsAndFlagsAnythingButSpaceSeparated(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		ids       string
		malformed bool
	}{
		{"1 2", "1 2", false},
		{"1   2", "1 2", false},
		{"", "", false},
		{"1,2", "1 2", true},
		{"[ 1, 2 ]", "1 2", true},
		{`["1", '2']`, "1 2", true},
		{"[ ]", "", true},
	} {
		ids, malformed := ParseDeps(tc.raw)
		if got := strings.Join(ids, " "); got != tc.ids || malformed != tc.malformed {
			t.Errorf("ParseDeps(%q) = %q, %v; want %q, %v", tc.raw, got, malformed, tc.ids, tc.malformed)
		}
	}
}
