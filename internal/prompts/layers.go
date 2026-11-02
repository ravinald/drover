package prompts

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// A slot is one of the three prompts drover assembles, named the way the
// [prompts] key that overrides it is named.
const (
	SlotImplement = "implement"
	SlotReview    = "review"
	SlotDraft     = "draft"
)

// Slots is every slot, in the order a listing prints them.
var Slots = []string{SlotImplement, SlotDraft, SlotReview}

// Builtin is the template drover ships for slot.
func Builtin(slot string) string {
	switch slot {
	case SlotImplement:
		return defaultTemplate
	case SlotReview:
		return reviewTemplate
	case SlotDraft:
		return draftTemplate
	}
	return ""
}

// Template is a built-in prompt with a project's override layered on top.
//
// The override is parsed into the built-in's template set, so each
// {{define "name"}} in it replaces the built-in's {{block "name"}} and every
// line outside a block stays drover's. That is what keeps the loop mechanics
// current in a project with rules of its own: a project that copied the whole
// file kept every instruction at the version it copied, and two of them fell
// far enough behind that their reviewers were told to file what drover now
// sends back.
type Template struct {
	Slot string
	// Path is the override's file, empty when the project has none.
	Path     string
	Override string
}

// Load reads the override for slot from path. An empty path is the built-in
// alone.
func Load(slot, path string) (Template, error) {
	t := Template{Slot: slot, Path: path}
	if path == "" {
		return t, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, fmt.Errorf("read prompt template: %w", err)
	}
	t.Override = string(data)
	return t, nil
}

// Name is what an error or a banner calls this template: the override's path,
// or the slot when there is none.
func (t Template) Name() string {
	if t.Path != "" {
		return t.Path
	}
	return t.Slot
}

// Source is everything the rendered prompt depends on besides its data, so a
// drive can tell a resume onto a new binary or an edited override from one
// that changed nothing.
func (t Template) Source() string { return Builtin(t.Slot) + "\x00" + t.Override }

// Render executes the layered template against data.
func (t Template) Render(data Data) (string, error) {
	set, err := t.parse()
	if err != nil {
		return "", err
	}
	return execute(t.Name(), set, data)
}

func (t Template) parse() (*template.Template, error) {
	set, err := parseText(t.Slot, Builtin(t.Slot))
	if err != nil {
		return nil, err
	}
	if t.Override == "" {
		return set, nil
	}
	known := blockNames(set)
	over, err := parseText(t.Path, t.Override)
	if err != nil {
		return nil, err
	}
	for name := range blockNames(over) {
		if !known[name] {
			return nil, fmt.Errorf("parse template %s: it defines %q, which the built-in %s template has no block for, so its text would never render; drover schema --prompts lists the blocks",
				t.Path, name, t.Slot)
		}
	}
	if _, err := set.Parse(t.Override); err != nil {
		return nil, fmt.Errorf("parse template %s: %w", t.Path, err)
	}
	return set, nil
}

func parseText(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	return t, nil
}

func execute(name string, t *template.Template, data Data) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	// A template that ends mid-thought is a template bug worth catching before
	// the text reaches an agent that will act on it.
	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("template %s rendered nothing", name)
	}
	return out + "\n", nil
}

// blockNames is every named template in set other than the set itself.
func blockNames(set *template.Template) map[string]bool {
	out := map[string]bool{}
	for _, t := range set.Templates() {
		if t.Name() != set.Name() {
			out[t.Name()] = true
		}
	}
	return out
}

// Block is one section of a built-in template a project may replace.
type Block struct {
	Slot string `json:"slot"`
	Name string `json:"block"`
	// Hash identifies the default's text. An override of a block with a
	// default records it, and `drover check` reports when the two part.
	Hash    string `json:"hash"`
	Default string `json:"default"`
	// Addition is a block whose default is empty: a place for project text,
	// with no drover text in it to fall behind, so its override records no base.
	Addition bool `json:"addition"`
}

// Blocks is every overridable block of the built-in template for slot, by name.
func Blocks(slot string) []Block {
	set, err := parseText(slot, Builtin(slot))
	if err != nil {
		return nil
	}
	var out []Block
	for name := range blockNames(set) {
		src := blockSource(Builtin(slot), name)
		out = append(out, Block{Slot: slot, Name: name, Hash: hashOf(src), Default: src, Addition: src == ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var (
	action     = regexp.MustCompile(`(?s)\{\{-?\s*(.*?)\s*-?\}\}`)
	blockOpen  = regexp.MustCompile(`^block\s+"([^"]+)"`)
	opensScope = regexp.MustCompile(`^(if|range|with|block|define)\b`)
)

// blockSource is the text between {{block "name" ...}} and its {{end}} in src,
// exactly as written. The parse tree's own String drops every trim marker,
// which is whitespace a project copying the default would then get wrong.
func blockSource(src, name string) string {
	start, depth := -1, 0
	for _, m := range action.FindAllStringSubmatchIndex(src, -1) {
		body := src[m[2]:m[3]]
		switch {
		case strings.HasPrefix(body, "/*"):
		case start < 0:
			if g := blockOpen.FindStringSubmatch(body); g != nil && g[1] == name {
				start, depth = m[1], 1
			}
		case opensScope.MatchString(body):
			depth++
		case body == "end":
			if depth--; depth == 0 {
				return src[start:m[0]]
			}
		}
	}
	return ""
}

func hashOf(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])[:12]
}

// baseMarker is the comment an override opens a replaced block with, naming
// the default it was written against.
var baseMarker = regexp.MustCompile(`\{\{-?\s*define\s+"([^"]+)"\s*-?\}\}\s*\{\{-?\s*/\*\s*drover-base:\s*([0-9a-f]*)\s*\*/\s*-?\}\}`)

// Behind is an override of a block whose default is no longer the one it was
// written against, or that never said which one that was.
type Behind struct {
	Block    string
	Recorded string // empty when the override carries no marker
	Current  string
}

// Inspection is what an override does to its built-in, beyond rendering.
type Inspection struct {
	// Whole is an override with text outside any define: it replaces the
	// built-in's body, so none of drover's own lines reach the prompt.
	Whole  bool
	Behind []Behind
}

// Inspect reports how far an override stands from the built-in it layers on.
// It assumes Render already accepted the template.
func (t Template) Inspect() Inspection {
	var out Inspection
	if t.Override == "" {
		return out
	}
	over, err := parseText(t.Path, t.Override)
	if err != nil {
		return out
	}
	out.Whole = over.Tree != nil && !parse.IsEmptyTree(over.Root)
	recorded := map[string]string{}
	for _, m := range baseMarker.FindAllStringSubmatch(t.Override, -1) {
		recorded[m[1]] = m[2]
	}
	defined := blockNames(over)
	for _, b := range Blocks(t.Slot) {
		if !defined[b.Name] || b.Addition {
			continue
		}
		if got := recorded[b.Name]; got != b.Hash {
			out.Behind = append(out.Behind, Behind{Block: b.Name, Recorded: got, Current: b.Hash})
		}
	}
	return out
}

// history holds every default each block has shipped, oldest first, keyed
// "slot/block". It is what lets `drover check --diff` show an override's
// recorded base beside today's default: a hash alone says that a default
// moved, and the binary carries only the current text. TestEveryDefaultIsInHistory
// fails when a default changes without its text being appended here.
//
//go:embed history.json
var historyJSON []byte

// Past is one default a block has shipped.
type Past struct {
	Hash string `json:"hash"`
	Text string `json:"text"`
}

// History is every default each block has shipped, keyed "slot/block".
func History() map[string][]Past {
	out := map[string][]Past{}
	if err := json.Unmarshal(historyJSON, &out); err != nil {
		return map[string][]Past{}
	}
	return out
}

// Earlier is the default slot/block shipped as hash, if this binary knows it.
func Earlier(slot, block, hash string) (string, bool) {
	for _, p := range History()[slot+"/"+block] {
		if p.Hash == hash {
			return p.Text, true
		}
	}
	return "", false
}
