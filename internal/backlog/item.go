package backlog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Item is one work item: a leading frontmatter fence and the text below it.
//
// Meta holds every declared key rather than a fixed set of fields, because the
// keys a project needs are the project's business — a monorepo with a generated
// API contract and a Django plugin do not want the same axes. drover itself
// reads only Title and Deps; everything else exists to be handed to the prompt
// template.
type Item struct {
	ID   ID
	Slug string
	// Path is the file this was read from.
	Path string
	Meta map[string]string
	Body string
}

// The frontmatter keys drover reads by name. A project's [frontmatter] lists are
// closed and authored per project, so a key drover reads that no generated
// default declares is a key no item may carry: every read returns empty, nothing
// refuses, and the only sign is whichever command happens to check.
//
// A key configurable through the config is deliberately absent, because its name
// is a setting rather than a literal: [severity] key and [run] runner_key both
// point at whatever the project names, and check reports an undeclared one.
const (
	KeyTitle  = "title"
	KeyDeps   = "deps"
	KeyCloses = "closes"
	KeyIssue  = "issue"
	KeySkills = "skills"
	KeySpec   = "spec"
	// KeyKind is a literal rather than a setting, unlike [severity] key and
	// [run] runner_key: a project chooses which kinds exist through [ids]
	// namespaces, never what the key naming one is called.
	KeyKind = "kind"
)

// ReadKeys is that set, for the generated defaults to be checked against. A
// project may declare more; what it may not do is leave one of these out and
// still have the command that reads it work.
func ReadKeys() []string {
	return []string{KeyTitle, KeyDeps, KeyCloses, KeyIssue, KeySkills, KeySpec, KeyKind}
}

// Title is the one metadata value drover renders itself.
func (it Item) Title() string { return it.Meta[KeyTitle] }

// Deps are the ids that must be merged before this item runs.
func (it Item) Deps() []string { return strings.Fields(it.Meta[KeyDeps]) }

// depMarks are what a YAML list writes around ids. drover keeps the deps line a
// space-separated list rather than reading these, so they are stripped only to
// name the ids the author meant.
const depMarks = `[]"'`

// ParseDeps reads a raw deps value as its author meant it: split on whitespace
// and commas, list brackets and quotes stripped from each piece, empty pieces
// dropped. malformed reports that the value is not already those ids joined by
// spaces, which is the only form Deps reads correctly. check and the deps lint
// both read through it, so no value can be malformed to one and clean to the
// other.
func ParseDeps(raw string) (ids []string, malformed bool) {
	split := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	for _, p := range strings.FieldsFunc(raw, split) {
		if p = strings.Trim(p, depMarks); p != "" {
			ids = append(ids, p)
		}
	}
	return ids, strings.Join(ids, " ") != strings.Join(strings.Fields(raw), " ")
}

// Get returns a metadata value, empty when the key is not declared.
func (it Item) Get(key string) string { return it.Meta[key] }

// Schema is what a project declares its frontmatter may contain.
type Schema interface {
	// Known reports whether a key is declared.
	Known(key string) bool
	// RequiredKeys must each be present, though they may be empty.
	RequiredKeys() []string
}

// ParseFrontmatter reads the fence and the body from one file's contents.
//
// A key outside the schema is an error rather than a warning. The cost of the
// alternative is specific: a typo'd `deps` silently reads as no dependencies,
// which renders a blocked item READY — a wrong answer, not a degraded one.
func ParseFrontmatter(path string, data []byte, schema Schema) (map[string]string, string, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || !isFence(lines[0]) {
		return nil, "", fmt.Errorf("%s: no frontmatter (expected a '---' fence on line 1)", path)
	}

	meta := map[string]string{}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isFence(lines[i]) {
			end = i
			break
		}
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			return nil, "", fmt.Errorf("%s:%d: not 'key: value': %s", path, i+1, line)
		}
		// First colon only — titles contain them.
		key := strings.TrimSpace(line[:colon])
		val := unquote(strings.TrimSpace(line[colon+1:]))

		if key == "slug" {
			return nil, "", fmt.Errorf("%s:%d: 'slug' comes from the filename, not frontmatter", path, i+1)
		}
		if !schema.Known(key) {
			return nil, "", fmt.Errorf("%s:%d: unknown frontmatter key %q", path, i+1, key)
		}
		if _, dup := meta[key]; dup {
			return nil, "", fmt.Errorf("%s:%d: duplicate frontmatter key %q", path, i+1, key)
		}
		meta[key] = val
	}
	if end < 0 {
		return nil, "", fmt.Errorf("%s: frontmatter fence is never closed", path)
	}

	for _, k := range schema.RequiredKeys() {
		if _, ok := meta[k]; !ok {
			return nil, "", fmt.Errorf("%s: missing required frontmatter key %q", path, k)
		}
	}
	if meta["title"] == "" && schema.Known("title") {
		return nil, "", fmt.Errorf("%s: title is empty", path)
	}

	return meta, body(lines[end+1:]), nil
}

// body drops the blank lines between the fence and the first real line, so the
// text an agent receives starts where the author started writing.
func body(lines []string) string {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	return strings.TrimRight(strings.Join(lines[start:], "\n"), "\n")
}

func isFence(s string) bool { return strings.TrimSpace(s) == "---" }

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// Requirement is one numbered line from an item's body.
//
// The numbering is load-bearing: it is what makes a requirement auditable, so a
// requirement written as prose has no number, never becomes a checkbox, and
// nobody ever confirms it.
type Requirement struct {
	Num  int
	Text string
	// Operator marks a requirement only a person can meet: a live check over
	// ssh, a deploy, anything the session's permission mode blocks. Text
	// carries it with the tag removed.
	Operator bool
}

// OperatorTag opens a requirement's text when the operator, not a session,
// has to meet it. A headless session cannot discharge one, so a review that
// sent back on it would spend every round up to the cap on work no round can do.
const OperatorTag = "[operator]"

// CutOperator reports whether a requirement's text opens with OperatorTag and
// returns the text without it. Emphasis around the tag is tolerated, because an
// author who bolds the whole line wrote the same claim.
func CutOperator(text string) (string, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(text), "**")
	rest, ok := strings.CutPrefix(s, OperatorTag)
	if !ok {
		return text, false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "**")), true
}

// numbered matches a top-level "N. " line. Continuation lines belong to the
// requirement above them, not to a new one.
var numbered = regexp.MustCompile(`(?m)^(\d+)\.[ \t]+(.*)$`)

// Requirements extracts the numbered contract from an item's body.
func (it Item) Requirements() []Requirement {
	matches := numbered.FindAllStringSubmatch(it.Body, -1)
	out := make([]Requirement, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		text, op := CutOperator(plainText(m[2]))
		out = append(out, Requirement{Num: n, Text: text, Operator: op})
	}
	return out
}

// plainText strips markdown emphasis so a requirement reads as a claim rather
// than as source. Only the first line survives: a checklist entry is a pointer
// back to the body, not a copy of it.
func plainText(s string) string {
	s = strings.TrimSpace(s)
	for _, mark := range []string{"**", "__"} {
		for strings.Contains(s, mark) {
			s = strings.Replace(s, mark, "", 1)
		}
	}
	return strings.TrimSpace(s)
}
