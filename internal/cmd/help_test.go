package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// placeholder matches the {name} form helpVars resolves, and not a JSON sample,
// whose braces are always followed by a quote.
var placeholder = regexp.MustCompile(`\{[a-z]+\}`)

// Every visible command carries the agent-facing surface. A command added
// without an entry in classified renders with no group and no safety line,
// which is invisible in review and exactly the drift this table exists to stop.
func TestEveryCommandIsClassified(t *testing.T) {
	install()
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		k, ok := classified[c.Name()]
		if !ok {
			t.Errorf("%s: no entry in classified", c.Name())
			continue
		}
		if k.class == "" {
			t.Errorf("%s: no safety class", c.Name())
		}
		if k.effect == "" {
			t.Errorf("%s: no effect: say what it changes, or that it changes nothing", c.Name())
		}
		// An unset who is the zero value, which is neither operator nor either.
		// Left unset, a command sealed by intent is silently open.
		if k.who != operator && k.who != either {
			t.Errorf("%s: who is %q, want operator or either", c.Name(), k.who)
		}
		if k.who == operator && k.because == "" {
			t.Errorf("%s: operator with no because; --help renders a half sentence", c.Name())
		}
		if k.who == either && k.because != "" {
			t.Errorf("%s: because is set on an either command, where nothing renders it", c.Name())
		}
		if c.Example == "" {
			t.Errorf("%s: no Example; --help renders no Examples block", c.Name())
		}
		if c.Long == "" {
			t.Errorf("%s: no Long", c.Name())
		}
	}
}

// A GroupID cobra does not know panics at execute time rather than failing a
// build, so the listing is only safe if every group named here is registered.
func TestGroupsAreRegistered(t *testing.T) {
	install()
	known := map[string]bool{"": true}
	for _, g := range root.Groups() {
		known[g.ID] = true
	}
	for name, k := range classified {
		if !known[k.group] {
			t.Errorf("%s: group %q is not registered", name, k.group)
		}
	}
}

// jsonCommands is every visible command carrying --json, read off the tree.
//
// The judge is the flag, never a list written beside it. The predecessor of
// this test iterated a literal of seven names while the binary had nine, so it
// asserted that seven commands it already believed in still had the flag and
// could not see projects or pending at all.
func jsonCommands() []string {
	var out []string
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, c := range parent.Commands() {
			if c.Hidden || c.Name() == "help" {
				continue
			}
			switch {
			case c.Name() == "intake" && c.Flags().Lookup("list") != nil:
				// intake writes; only its --list form reads, and --json rides on it.
				out = append(out, "intake --list")
			case c.Flags().Lookup("json") != nil:
				out = append(out, c.Name())
			}
			walk(c)
		}
	}
	walk(root)
	sort.Strings(out)
	return out
}

// Every command that advertises --json shows a sample object, because the
// shapes live in docs/automation.md and a sandboxed run has only the binary.
func TestJSONCommandsShowASample(t *testing.T) {
	install()
	names := jsonCommands()
	if len(names) == 0 {
		t.Fatal("no command carries --json; the walk found nothing to check")
	}
	for _, name := range names {
		c, _, err := root.Find(strings.Fields(name)[:1])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(c.Example, `{"`) {
			t.Errorf("%s: Example shows no sample JSON object", name)
		}
	}
}

// Each prose sentence naming which commands take --json, and the text the list
// begins after. Every one of these is read by somebody who cannot run the
// binary to check it: an operator skimming the README, an agent handed the
// generated AGENTS.md, a wrapper author in automation.md.
//
// Maintenance, when this fails: fix the copy the failure names. A command that
// gained or lost --json needs every row here edited, which is the whole reason
// the set is derived rather than listed.
var jsonListCopies = []struct {
	name, path, marker string // path empty means root's own help text
}{
	{name: "drover --help", marker: "--json is on "},
	{name: "internal/cmd/agents.tmpl", path: "agents.tmpl", marker: "`--json` is on"},
	{name: "README.md", path: "../../README.md", marker: "`--json` is on"},
	{name: "docs/agent-guide.md", path: "../../docs/agent-guide.md", marker: "`--json` is on"},
	{name: "docs/automation.md", path: "../../docs/automation.md", marker: "## `--json`\n\nOn"},
}

// jsonListIn pulls the command names out of one such sentence. The list runs to
// the first . or ; because every copy ends its sentence differently and only
// the punctuation is common to all of them.
func jsonListIn(text, marker string) ([]string, error) {
	i := strings.Index(text, marker)
	if i < 0 {
		return nil, fmt.Errorf("marker %q not found", marker)
	}
	rest := text[i+len(marker):]
	end := strings.IndexAny(rest, ".;")
	if end < 0 {
		return nil, fmt.Errorf("no sentence end after %q", marker)
	}
	list := strings.ReplaceAll(rest[:end], "`", "")
	list = strings.Join(strings.Fields(list), " ")
	list = strings.ReplaceAll(list, " and ", ", ")
	var out []string
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

func TestEveryCopyOfTheJSONListIsComplete(t *testing.T) {
	install()
	want := jsonCommands()
	for _, src := range jsonListCopies {
		text := root.Long
		if src.path != "" {
			b, err := os.ReadFile(src.path)
			if err != nil {
				t.Fatalf("%s: %v", src.name, err)
			}
			text = string(b)
		}
		got, err := jsonListIn(text, src.marker)
		if err != nil {
			t.Errorf("%s: %v", src.name, err)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s names %v, the binary has %v", src.name, got, want)
		}
	}
}

// Help read outside a project still has to name where a value comes from, so a
// placeholder resolves to its config key rather than to an empty string.
func TestSubstituteFallsBackToTheConfigKey(t *testing.T) {
	t.Chdir(t.TempDir())
	vars := helpVars()
	if got := vars["ledger"]; got != "[paths] ledger" {
		t.Errorf("ledger = %q, want the config key", got)
	}
	if got := substitute("appends to {ledger}", vars); got != "appends to [paths] ledger" {
		t.Errorf("substitute = %q", got)
	}
}

// Nothing may ship a placeholder the expander does not know: it would reach a
// reader verbatim as "{runner}" and read as a literal to type.
func TestNoUnknownPlaceholders(t *testing.T) {
	install()
	vars := helpVars()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{substitute(c.Long, vars), substitute(c.Example, vars)} {
			for _, m := range placeholder.FindAllString(text, -1) {
				t.Errorf("%s: unresolved placeholder %s", c.Name(), m)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// Cobra's own "accepts 1 arg(s), received 0" names no recovery, and the root
// silences usage on error, so a caller that omits the id gets nothing to act on.
func TestMissingIDNamesTheRecoveryCommand(t *testing.T) {
	err := needID("run <id>")(nil, nil)
	if err == nil {
		t.Fatal("no error for a missing id")
	}
	for _, want := range []string{"drover run <id>", "drover status", "B01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if err := needID("run <id>")(nil, []string{"1"}); err != nil {
		t.Errorf("one id: %v", err)
	}
}

// The class table names a command; this table says what it touches. A command
// listed in one and missing from the other leaves one copy of its sentence:
// generated instructions that group `split` under local writes and never say
// what it writes leave only the one in --help, which can drift into claiming
// a ledger row split never appends.
//
// The base name is what is checked, because a flagged invocation carries no
// entry of its own: `report --save` is described inside report's effect.
func TestEveryCommandInTheSafetyTableSaysWhatItChanges(t *testing.T) {
	install()
	effects := map[string]string{}
	for _, e := range CommandEffects(helpVars()) {
		effects[e.Command] = e.Effect
	}
	for _, row := range SafetyTable() {
		for _, entry := range row.Commands {
			base := strings.Fields(strings.ReplaceAll(entry, "`", ""))[0]
			if _, ok := effects[base]; !ok {
				t.Errorf("%s is listed under %q and no effect row says what it changes",
					base, row.Class)
			}
		}
	}
	for name := range classified {
		if _, ok := effects[name]; !ok {
			t.Errorf("%s: classified, and CommandEffects drops it", name)
		}
	}
}

// Some commands end their Example with a Safety line written by hand, beside
// the one expand appends from classified. Two copies can disagree, say split's
// claiming a ledger row the table says it does not write, unless something
// compares the two. Checked on the ledger question rather
// than on equal wording, because the hand-written line is deliberately the
// shorter sentence and only the table resolves a project's paths.
//
// A line that never names the ledger abstains and is skipped. request-changes's is the
// case: "appends one changes row" says where nothing, so it disagrees with the
// table about wording and not about what writes.
func TestHandWrittenSafetyLinesAgreeWithTheTable(t *testing.T) {
	install()
	graded := 0
	for _, c := range root.Commands() {
		hand := handWrittenSafety(c.Example)
		if hand == "" || !strings.Contains(hand, "ledger") {
			continue
		}
		k, ok := classified[c.Name()]
		if !ok {
			t.Errorf("%s: a hand-written Safety line and no entry in classified", c.Name())
			continue
		}
		graded++
		if got, want := mentionsLedgerRow(hand), mentionsLedgerRow(k.effect); got != want {
			t.Errorf("%s: its hand-written Safety line claims a ledger row: %t, the table: %t\n%s\n%s",
				c.Name(), got, want, hand, k.effect)
		}
	}
	// Without this the test passes on zero commands, which is what a detector
	// that stopped matching looks like from the outside.
	if graded == 0 {
		t.Error("no hand-written Safety line takes a position on the ledger: either they all went, or handWrittenSafety stopped finding them")
	}
}

// handWrittenSafety returns the Safety line a command literal carries in its
// Example, or the empty string. It runs to the end of the Example because the
// line wraps and is always the last thing in one.
//
// Matched on the trimmed line rather than at a line start: claim.go indents its
// copy two spaces to sit with the commands above it, and an anchored match reads
// a file that has the second sentence as a file that does not.
func handWrittenSafety(example string) string {
	lines := strings.Split(example, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "Safety: ") {
			return strings.TrimSpace(strings.Join(lines[i:], "\n"))
		}
	}
	return ""
}

// An unresolved placeholder reaches both --help and the generated table as
// literal braces. The table escapes a pipe or a newline itself, so an effect
// needs no guard against those; TestConfigValuesStayInTheirTableCell pins it.
func TestEffectsResolveEveryPlaceholder(t *testing.T) {
	install()
	for _, e := range CommandEffects(helpVars()) {
		for _, m := range placeholder.FindAllString(e.Effect, -1) {
			t.Errorf("%s: unresolved placeholder %s reaches the reader verbatim", e.Command, m)
		}
	}
}

// The detector is the half of TestHandWrittenSafetyLinesAgreeWithTheTable that
// can fail silently: a line it does not find is a file it reports as carrying no
// second sentence. claim.go indents its copy, which an anchored match missed.
func TestHandWrittenSafetyFindsEveryCopy(t *testing.T) {
	install()
	var missed []string
	for _, c := range root.Commands() {
		if strings.Contains(c.Example, "Safety: ") && handWrittenSafety(c.Example) == "" {
			missed = append(missed, c.Name())
		}
	}
	if len(missed) > 0 {
		t.Errorf("handWrittenSafety found no line in: %s", strings.Join(missed, ", "))
	}
}

// mirrorWriters are the entry points through which a command writes to the
// item's issue. Each returns at once in a project without [backlog] mirror,
// which is why the commands reaching them stay localWrite.
var mirrorWriters = []string{"publish", "mirrorAround", "mirrorReloaded", "mirrorNow", "mirrorShelved", "mirrorState"}

// mirrorEdge is one call, from one function to another, that the walk for a
// command skips because it never fires on that command's path.
type mirrorEdge struct{ from, to, why string }

// mirrorExempt names, per command, the one edge the call graph below reports
// and that never writes at runtime. The graph matches calls by name, so it
// over-reaches. Exempting the edge rather than the command keeps every other
// path from that command's RunE to a mirror writer under the check.
var mirrorExempt = map[string]mirrorEdge{
	"review": {"launched", "mirrorState", "launched relabels the issue for the implement stage only, and review records the review stage"},
}

// A command that writes the item's issue through the mirror says so on its
// safety line. Without this, accept said "Touches no tracker." while it opened
// and rewrote issues other people read.
func TestMirrorWritersNameTheMirror(t *testing.T) {
	runE, calls := mirrorGraph(t)
	reach := map[string]string{}
	for name, direct := range runE {
		var skip *mirrorEdge
		if e, ok := mirrorExempt[name]; ok {
			skip = &e
		}
		if w := reaches(direct, calls, skip); w != "" {
			reach[name] = w
		}
	}
	var got []string
	for name := range reach {
		got = append(got, name)
	}
	sort.Strings(got)
	if len(got) == 0 {
		t.Fatal("no command reaches a mirror writer; the call graph read nothing")
	}
	for _, name := range got {
		k, ok := classified[name]
		if !ok {
			t.Errorf("%s reaches %s and has no entry in classified", name, reach[name])
			continue
		}
		if !strings.Contains(k.effect, "mirror") {
			t.Errorf("%s reaches %s, and its effect line never names the mirror write it makes", name, reach[name])
		}
	}
	for name, e := range mirrorExempt {
		if !slices.Contains(calls[e.from], e.to) || reaches(runE[name], calls, nil) == "" {
			t.Errorf("%s exempts %s > %s, but that edge no longer reaches a mirror writer from it; drop the entry", name, e.from, e.to)
			continue
		}
		t.Logf("%s: exempt edge %s > %s: %s", name, e.from, e.to, e.why)
	}
}

// mirrorGraph returns what each command's RunE calls, keyed by the first word
// of its Use, and what each function in this package calls.
func mirrorGraph(t *testing.T) (runE, calls map[string][]string) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls = map[string][]string{}
	runE = map[string][]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				calls[fn.Name.Name] = append(calls[fn.Name.Name], calledNames(fn.Body)...)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isCobraCommand(lit.Type) {
				return true
			}
			use, body := "", ast.Node(nil)
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch key, _ := kv.Key.(*ast.Ident); {
				case key == nil:
				case key.Name == "Use":
					if bl, ok := kv.Value.(*ast.BasicLit); ok {
						use, _ = strconv.Unquote(bl.Value)
					}
				case key.Name == "RunE":
					body = kv.Value
				}
			}
			if name, _, _ := strings.Cut(use, " "); name != "" && body != nil {
				runE[name] = append(runE[name], calledNames(body)...)
			}
			return true
		})
	}
	return runE, calls
}

func isCobraCommand(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "cobra" && sel.Sel.Name == "Command"
}

// calledNames are the names n calls, a method by its name alone, plus any
// identifier passed as a value, so RunE: runX and a callback both count.
func calledNames(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			out = append(out, x.Sel.Name)
		case *ast.Ident:
			out = append(out, x.Name)
		}
		return true
	})
	return out
}

// reaches is the first mirror writer found walking calls from start, never
// following skip when it is set.
func reaches(start []string, calls map[string][]string, skip *mirrorEdge) string {
	seen := map[string]bool{}
	queue := slices.Clone(start)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		if slices.Contains(mirrorWriters, name) {
			return name
		}
		for _, callee := range calls[name] {
			if skip != nil && name == skip.from && callee == skip.to {
				continue
			}
			queue = append(queue, callee)
		}
	}
	return ""
}

// drive merges by default and --no-merge holds it, so a sentence denying the
// merge in its help describes an opt-in flag drive does not have and tells an operator the
// command is safer than it is. Only drive: a list of commands or flags would be
// a general checker, and the --no-merge usage string says "merge nothing" of
// itself on purpose.
func TestDriveHelpNeverClaimsItMergesNothing(t *testing.T) {
	install()
	effects := CommandEffects(helpVars())
	i := slices.IndexFunc(effects, func(e CommandEffect) bool { return e.Command == "drive" })
	if i < 0 {
		t.Fatal("drive: no effect in CommandEffects")
	}
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	texts := []struct{ name, text, states string }{
		{"effect", collapse(effects[i].Effect), "merges it with [git] merge_method"},
		{"reach", collapse(classified["drive"].reach), "merges what the merge gate passes"},
		{"Short", collapse(driveCmd.Short), ""},
		{"Long", collapse(driveCmd.Long), "green and reviewed is merged"},
	}
	denial := regexp.MustCompile(`(?i)merges nothing|never merges|merge nothing|performs no merge|does not merge`)
	for _, x := range texts {
		if m := denial.FindString(x.text); m != "" {
			t.Errorf("drive %s claims no merge: %q", x.name, m)
		}
		if x.states != "" && !strings.Contains(x.text, x.states) {
			t.Errorf("drive %s no longer states the default merge: missing %q", x.name, x.states)
		}
	}
}

// Matching only "drover <name>" and backticked forms would let prose naming a
// bare invocation ("so does <name> supervise") tell a reader to type a command
// that exits 1.
func TestHelpNamesNoRemovedCommand(t *testing.T) {
	install()
	removed := []string{"land", "done", "undone", "request for changes", "clear", "guard", "claim",
		"attest", "propose", "pending", "result", "debt", "setup"}
	subs := map[string]bool{}
	var collect func(*cobra.Command)
	collect = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if c != root {
				subs[s.Name()] = true
			}
			collect(s)
		}
	}
	collect(root)
	var names []string
	for n := range subs {
		names = append(names, regexp.QuoteMeta(n))
	}
	invoked := regexp.MustCompile(`\b(` + strings.Join(removed, "|") + `) (--[a-z]|<|[0-9]|(` + strings.Join(names, "|") + `)\b)`)
	vars := helpVars()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{substitute(c.Long, vars), substitute(c.Example, vars)} {
			for _, m := range invoked.FindAllString(text, -1) {
				t.Errorf("%s: help names removed command %q", c.CommandPath(), m)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	if !strings.Contains(root.Long, "init supervise") {
		t.Error("root help does not name init supervise")
	}
}
