package cmd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// check's finding kinds are enumerated in five places, and only one of them is
// the code. Three of the other four are prose a reader trusts; the fifth is
// executable and drops what it does not name, so a kind missing from
// docs/rtk-filters.toml renders a real finding as an orphaned detail line under
// no headline. Any of the four can drift behind the code.
//
// The judge is the Kind literals in check.go, read from the AST rather than
// from a list kept here. A second hand-kept list is the defect, not the fix.
//
// Maintenance, when this fails: add the kind to whichever copies the failure
// names. Never to a list in this file — there is none.
const (
	checkSource   = "check.go"
	conceptsDoc   = "../../docs/concepts.md"
	lifecycleDoc  = "../../docs/lifecycle.md"
	rtkFiltersDoc = "../../docs/rtk-filters.toml"
)

// kindsInCode collects every value assigned to a Finding's Kind field.
//
// A Kind that is not a string literal fails rather than being skipped: the
// whole comparison rests on this set being complete, and a set that quietly
// shrinks would pass every assertion below while pinning nothing.
func kindsInCode(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, checkSource, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", checkSource, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Kind" {
			return true
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s:%d: Kind is not a string literal, so this test can no longer read the set from the code",
				checkSource, fset.Position(kv.Pos()).Line)
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("%s: %v", checkSource, err)
		}
		out[s] = true
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s: found no Kind literals; the scan is broken, not the docs", checkSource)
	}
	return out
}

// An all-caps token of two or more characters, words joined by a space or a
// hyphen, which is the shape every kind has: matching the shape rather than the known set is what lets an unknown
// kind in a copy be reported instead of silently ignored.
var kindToken = regexp.MustCompile(`\b[A-Z]{2,}(?:[ -][A-Z]{2,})*\b`)

// The two-space-indented block in check's own Long text. The prose below it
// names kinds at column zero, which is what keeps them out of the set.
func kindsInHelp(t *testing.T) map[string]bool {
	t.Helper()
	row := regexp.MustCompile(`^  ([A-Z]{2,}(?:[ -][A-Z]{2,})*) +\S`)
	out := map[string]bool{}
	for _, line := range strings.Split(checkCmd.Long, "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// The first cell of each row in the Validation table.
func kindsInConcepts(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(read(t, conceptsDoc), "\n") {
		cells := strings.Split(strings.Trim(line, "| "), "|")
		if len(cells) < 2 {
			continue
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[0], -1) {
			if kindToken.FindString(m[1]) == m[1] {
				out[m[1]] = true
			}
		}
	}
	return out
}

// A prose sentence enumerating the kinds, from its opening to the blank line
// that ends the paragraph. Bounded rather than whole-file: both docs name
// individual kinds elsewhere, and a set built from every mention would agree
// with the code by accident.
func kindsInParagraph(t *testing.T, path, opening string) map[string]bool {
	t.Helper()
	lines := strings.Split(read(t, path), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, opening) })
	if start < 0 {
		t.Fatalf("%s: no line containing %q; the enumeration moved or was reworded", path, opening)
	}
	out := map[string]bool{}
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		for _, m := range backticked.FindAllStringSubmatch(line, -1) {
			if kindToken.FindString(m[1]) == m[1] {
				out[m[1]] = true
			}
		}
	}
	return out
}

// rtkFilter is the shipped filter file, decoded rather than pattern-matched:
// these strings carry TOML escapes, and reading them as text would compare
// something no rtk install ever sees.
type rtkFilter struct {
	Keep []string `toml:"keep_lines_matching"`
}

type rtkFile struct {
	Filters map[string]rtkFilter `toml:"filters"`
}

func rtkKeep(t *testing.T, name string) []string {
	t.Helper()
	var f rtkFile
	if _, err := toml.DecodeFile(rtkFiltersDoc, &f); err != nil {
		t.Fatalf("%s: %v", rtkFiltersDoc, err)
	}
	filter, ok := f.Filters[name]
	if !ok {
		t.Fatalf("%s: no [filters.%s]", rtkFiltersDoc, name)
	}
	if len(filter.Keep) == 0 {
		t.Fatalf("%s: [filters.%s] keeps nothing", rtkFiltersDoc, name)
	}
	return filter.Keep
}

// The alternation in the drover-check filter's kind pattern.
func kindsInRTK(t *testing.T) map[string]bool {
	t.Helper()
	alt := regexp.MustCompile(`^\^\(([A-Z |-]+)\)`)
	out := map[string]bool{}
	for _, p := range rtkKeep(t, "drover-check") {
		m := alt.FindStringSubmatch(p)
		if m == nil {
			continue
		}
		for _, k := range strings.Split(m[1], "|") {
			out[k] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: [filters.drover-check] has no kind alternation", rtkFiltersDoc)
	}
	return out
}

func TestFindingKindsMatchEveryCopy(t *testing.T) {
	code := kindsInCode(t)
	copies := map[string]map[string]bool{
		"check --help": kindsInHelp(t),
		conceptsDoc:    kindsInConcepts(t),
		lifecycleDoc:   kindsInParagraph(t, lifecycleDoc, "Findings are "),
		automationDoc:  kindsInParagraph(t, automationDoc, "the stable token to key on"),
		rtkFiltersDoc:  kindsInRTK(t),
	}
	for _, kind := range slices.Sorted(maps.Keys(code)) {
		var absent []string
		for _, where := range slices.Sorted(maps.Keys(copies)) {
			if !copies[where][kind] {
				absent = append(absent, where)
			}
		}
		if len(absent) > 0 {
			t.Errorf("check.go emits %q and these do not list it: %s", kind, strings.Join(absent, ", "))
		}
	}
	for _, where := range slices.Sorted(maps.Keys(copies)) {
		for _, kind := range slices.Sorted(maps.Keys(copies[where])) {
			if !code[kind] {
				t.Errorf("%s lists %q, which check.go does not emit", where, kind)
			}
		}
	}
}

// The finding column is a constant, so a kind that outgrows it pushes its own
// message a character right and leaves the wrapped detail line below it
// misaligned. docs/rtk-filters.toml matches
// that indent literally, so the drift reaches the shipped filter too.
func TestFindingKindsFitTheColumn(t *testing.T) {
	for _, kind := range slices.Sorted(maps.Keys(kindsInCode(t))) {
		if len(kind) > findingWidth {
			t.Errorf("%q is %d characters and findingWidth is %d: widen the constant and the "+
				"^ {%d}\\S pattern in %s together", kind, len(kind), findingWidth, findingWidth+1, rtkFiltersDoc)
		}
	}
}

// Every line real check output can produce, replayed through the patterns the
// shipped filter actually carries. keep_lines_matching drops what it does not
// match, so a line missing here is a line an operator running under rtk never
// sees — and the note is the one that turns a degraded read into a clean bill.
//
// Rendered by renderCheck rather than written out here: a second copy of the
// format would agree with a filter that no longer matches the first.
//
// The reverse direction is its own defect: rtk reads stdout, so a pattern for a
// line check writes to stderr matches nothing and the file claims a coverage it
// does not have. Asserted here because both halves need the same rendering.
func TestShippedFilterKeepsEveryLineCheckPrints(t *testing.T) {
	var keep []*regexp.Regexp
	for _, p := range rtkKeep(t, "drover-check") {
		re, err := regexp.Compile(p)
		if err != nil {
			t.Fatalf("%s: %q: %v", rtkFiltersDoc, p, err)
		}
		keep = append(keep, re)
	}

	rep := CheckReport{
		Bodies:       3,
		Rows:         2,
		RegisterNote: "the register was not read, so no orphaned rows were counted: gh is not on PATH",
		Stopped:      stoppedStale,
	}
	for _, kind := range slices.Sorted(maps.Keys(kindsInCode(t))) {
		rep.Findings = append(rep.Findings, Finding{
			Kind:    kind,
			Message: "the headline a reader diagnoses from",
			Detail:  "the wrapped line carrying the fix",
		})
	}

	var out bytes.Buffer
	renderCheck(&out, rep)
	// The clean result, which only a report with no findings reaches.
	renderCheck(&out, CheckReport{Bodies: 3, Rows: 2, OK: true})

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for _, line := range lines {
		if !slices.ContainsFunc(keep, func(re *regexp.Regexp) bool { return re.MatchString(line) }) {
			t.Errorf("[filters.drover-check] drops a line check prints: %q", line)
		}
	}
	for _, re := range keep {
		if !slices.ContainsFunc(lines, re.MatchString) {
			t.Errorf("[filters.drover-check] keeps %q, which nothing check writes to stdout can match", re)
		}
	}
}
