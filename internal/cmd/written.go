package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ravinald/drover/internal/config"
)

// written is the value a config already carries for each dotted key, as the
// TOML literal that would produce it.
//
// It exists so that one generator serves both callers. `drover init` passes
// nothing, because there is no file yet. `drover config` passes what the
// project declared, so the layout it prints is today's with the project's own
// decisions in it — a project reading its settings back as defaults cannot tell
// which of them it chose. A second renderer for the second caller would drift
// from the first, the way a hand-filled copy of a generated artifact drifts
// past the binary, which is why `drover agents` generates its text.
type written map[string]string

// has reports whether the config declared the key itself.
func (w written) has(key string) bool { _, ok := w[key]; return ok }

// or is the literal for key, falling back to what this build would write for a
// project that never set it.
func (w written) or(key, def string) string {
	if v, ok := w[key]; ok {
		return v
	}
	return def
}

// line renders a key the generator otherwise leaves commented out. A project
// that set one gets the live line; every other project keeps the comment,
// because the key is optional and a project that never wanted it must not gain
// a finding by upgrading.
func (w written) line(key, commented string) string {
	if v, ok := w[key]; ok {
		_, name := config.Table(key)
		return name + " = " + v + "\n"
	}
	return commented
}

// Open tables ([verify], [runners], ids.namespaces, severity.ranks,
// frontmatter.shape: config.OpenTables) follow one rule in a report. What the
// project's own file said about one is written back exactly, absent, empty or
// partial, and never mixed with the entries init would suggest. The layout's
// suggestion appears commented where neither file declares the table, and not
// at all where the per-user file does. Asked table by table, each would get a
// different answer: an absent [frontmatter.shape] could come back holding
// closed keys, and an inherited empty [verify] could come back holding a
// project-local `default = ""` that then shadows the global's verify command.

// openTable renders an open table nested inside a section the layout writes,
// such as ids.namespaces under [ids]. The project's declaration is its own
// literal, read off its file alone, so an empty table stays `{}` and a partial
// one names exactly what the project named.
func openTable(m mode, g globalDefaults, w written, table, suggestion string) string {
	switch {
	case w.declares(table):
		_, name := config.Table(table)
		return name + " = " + w.or(table, "{}") + "\n"
	case g.declares(table):
		return ""
	}
	return m.suggest(suggestion)
}

// shapeBlock appends the [frontmatter.shape] section, and appends nothing for
// a project that inherits the table whole.
//
// A global that opened the table has already decided what it holds, including
// when what it holds is nothing. Asked per key, an inherited empty table
// looked like three gaps and the print closed them, handing back a copy that
// rejects a severity the source accepted.
func shapeBlock(b *strings.Builder, m mode, g globalDefaults, w written) {
	why := "# Which keys may appear is closed by [frontmatter]; this closes what three of\n" +
		"# them may hold. severity is the one that fails without saying so: an unknown\n" +
		"# letter maps to unranked, so a typo renders identically to a body that declared\n" +
		"# no rank at all, in every table that shows one. closes is the list finish acts\n" +
		"# on, so a typo'd number reaches a stranger's report as a comment.\n"
	switch {
	case w.declares("frontmatter.shape"):
		// Above the header, because carry places the project's entries directly
		// under it.
		b.WriteString("\n" + why + "[frontmatter.shape]\n")
		if len(w.entries("frontmatter.shape")) == 0 {
			b.WriteString("# This project opened the table and closed no key.\n")
		}
	case g.declares("frontmatter.shape"):
	default:
		b.WriteString(m.suggest("\n[frontmatter.shape]\n" + why +
			`issue    = "int"` + "\n" +
			`closes   = "intlist"` + "\n" +
			`severity = "enum:C,H,M,L"` + "\n"))
	}
}

// verifyBlock appends the [verify] section. init keeps its own rule, a block
// omitted only when the global supplies the default gate, because a new file
// with no gate is what NO VERIFY exists to report.
func verifyBlock(b *strings.Builder, m mode, g globalDefaults, w written, gate string) {
	set := "# The command that must pass before an item may merge. An item that runs\n" +
		"# without a verify command hands its agent no way to verify the work.\n"
	required := "# REQUIRED, and empty on purpose. This is the command that must pass before an\n" +
		"# item may merge, and nothing can infer it. An item that runs without one\n" +
		"# hands its agent no way to verify the work. drover check reports this until\n" +
		"# it is filled.\n"
	switch {
	case w.declares("verify"):
		b.WriteString("\n" + set + "[verify]\n")
		if len(w.entries("verify")) == 0 {
			b.WriteString("# This project opened the table and named no verify command.\n")
		}
	case m == forReport:
		if !g.declares("verify") {
			b.WriteString(commented("\n[verify]\n" + required + `default = ""` + "\n"))
		}
	case gate != "":
		block(b, m, g, w, "verify", []string{"default"}, set+fmt.Sprintf("default = %q\n", gate))
	default:
		block(b, m, g, w, "verify", []string{"default"}, required+`default = ""`+"\n")
	}
}

// commented turns a block into a suggestion: every line that is not already a
// comment gains a leading "# ", so nothing in it loads.
func commented(text string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			b.WriteString("# ")
		}
		b.WriteString(line)
	}
	return b.String()
}

// live renders one `name = value` line: the project's own value where it wrote
// one, nothing where the per-user config supplies the key and this project
// does not, and the layout's default otherwise, live for init and commented in
// a report (see mode.suggest).
//
// A line written here shadows the global value rather than inheriting it, so
// the next edit to that file silently misses this project. w.or alone cannot
// see that: it falls back to drover's default, which is the global's value
// only by coincidence and only today.
func live(m mode, g globalDefaults, w written, key, name, pad, def string) string {
	if v, ok := w[key]; ok {
		return name + pad + "= " + v + "\n"
	}
	if g.has(key) {
		return ""
	}
	return m.suggest(name + pad + "= " + def + "\n")
}

// declares reports whether the project opened a table or wrote anything inside
// it. Opening one is a decision about what it holds, so a print may neither
// drop it nor fill it in.
func (w written) declares(table string) bool {
	return w.has(table) || len(w.entries(table)) > 0
}

// entries are the names a project wrote inside an open table, dotted and
// sorted. The table's own key is not one of them.
func (w written) entries(table string) []string {
	prefix := table + "."
	var out []string
	for key := range w {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, key)
	}
	return config.Leaves(out)
}

// keyLines renders one key per line, padded so the values line up the way
// every other table this generator writes does.
func keyLines(keys []string, w written) string {
	width := 0
	for _, key := range keys {
		if _, name := config.Table(key); len(name) > width {
			width = len(name)
		}
	}
	var b strings.Builder
	for _, key := range keys {
		_, name := config.Table(key)
		fmt.Fprintf(&b, "%-*s = %s\n", width, name, w[key])
	}
	return b.String()
}

// carry places every setting the project declared that the layout above does
// not write, and returns the finished file.
//
// Without it a printed config drops the project's own decisions — [run]
// review_runner, [review] max_review_rounds, [drive] merge_gate — and a reader pasting
// what they saw would silently change how their project runs. The curated
// layout covers what init can reason about on a project's first day, which is a
// smaller set than what a project running for six months carries.
//
// A key whose table the layout already opened is inserted into that table's own
// section rather than appended at the foot of the file. TOML refuses a second
// [table] header, so appending one prints a config that will not load: a
// project setting [paths] shelf or a [github] label key would read its own
// settings back in a file drover itself could not parse.
//
// What the layout already wrote is read back off its own text rather than
// listed here: a second list drifts from the generator the first time somebody
// adds a key to one and not the other.
func carry(body string, w written, bare []string) string {
	if len(w) == 0 && len(bare) == 0 {
		return body
	}
	already, err := config.KeysIn(body)
	if err != nil {
		// The generated layout failed to parse, which is a defect in it rather
		// than in this config. Say so where a reader will see it instead of
		// dropping their settings silently.
		return body + fmt.Sprintf("\n# drover could not re-read the layout above: %v\n", err)
	}
	wrote := make(map[string]bool, len(already))
	for _, k := range already {
		wrote[k] = true
	}

	tables := map[string][]string{}
	for _, key := range config.Leaves(keysOf(w)) {
		if wrote[key] {
			continue
		}
		table, _ := config.Table(key)
		tables[table] = append(tables[table], key)
	}
	// A table the project opened and left empty joins them with no keys under it.
	// The header is the setting: [severity] with nothing in it is what
	// SeverityConfigured reads, so a copy that wrote only values dropped it and
	// reported the project clean where the original reported UNDECLARED KEY.
	for _, table := range bare {
		if wrote[table] {
			continue
		}
		if _, ok := tables[table]; !ok {
			tables[table] = nil
		}
	}
	if len(tables) == 0 {
		return body
	}

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	inserts := map[int][]string{}
	var appended []string
	for _, table := range sortedTables(tables) {
		at, open := sectionEnd(lines, table)
		if !open {
			appended = append(appended, table)
			continue
		}
		inserts[at] = append(inserts[at], strings.Split(strings.TrimRight(keyLines(tables[table], w), "\n"), "\n")...)
	}

	var b strings.Builder
	// Before the first line, for a top-level key in a layout that opens with a
	// table header. sectionEnd answers -1 there, and a bare key written
	// anywhere below a header reads as a key of that table.
	for _, added := range inserts[beforeFirstLine] {
		b.WriteString(added + "\n")
	}
	for i, line := range lines {
		b.WriteString(line + "\n")
		for _, added := range inserts[i] {
			b.WriteString(added + "\n")
		}
	}
	if len(appended) == 0 {
		return b.String()
	}

	b.WriteString("\n# Settings this project declared that the layout above does not write.\n")
	b.WriteString("# Carried through so this is the whole of what the project runs on.\n")
	for _, table := range appended {
		fmt.Fprintf(&b, "\n[%s]\n", table)
		b.WriteString(keyLines(tables[table], w))
	}
	return b.String()
}

// beforeFirstLine is the insertion point ahead of every line of the body. It
// is not an index into lines, and carry writes it out on its own.
const beforeFirstLine = -1

// sectionEnd is the index of the last assignment inside a table the text has
// already opened, and whether it opened one at all.
//
// The last assignment rather than the last line: the generator writes a comment
// block describing the next table before its header, so "the last line before
// the next [" would put a carried key underneath prose about something else.
// Valid TOML, and unreadable.
//
// The empty table is the top level, which every text opens and no text gives a
// header, so it is the one section that is always found. That matters because
// a bare key appended at the foot of the file sits under whatever header came
// last: `runners = {}` carried past [github] reloaded as github.runners, a key
// nothing reads, in a file drover refuses, reached by pasting what drover
// printed.
func sectionEnd(lines []string, table string) (int, bool) {
	start := beforeFirstLine
	if table != "" {
		header := "[" + table + "]"
		found := -1
		for i, line := range lines {
			if line == header {
				found = i
				break
			}
		}
		if found < 0 {
			return 0, false
		}
		start = found
	}
	at := start
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[") {
			break
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			at = i
		}
	}
	return at, true
}

// undeclared is every key the config generator would write for this project
// that no layered file declares.
//
// One computation, read by the finding and by the report it points at. Two
// would disagree the first time one of them grew a filter, and an operator
// running the command a finding named to see a different count than the finding
// carried has been told two things.
//
// The generator is the judge rather than the whole set of keys this build
// reads. Those are different questions: a key drover reads and init does not
// write is a default nobody has to decide, while a key init writes today is
// what a config written today would carry.
//
// It renders with nothing written in, because the answer is keys the project
// did not declare, and the project's own values are exactly the ones excluded.
// derivedFromTree reports whether the layout takes this key's value from the
// checkout it is rendering for rather than from a default. Such a key is absent
// from a config on purpose as often as by omission, and the two are
// indistinguishable from here.
func derivedFromTree(dotted string) bool {
	return dotted == "github.repo" || dotted == "paths.root_marker"
}

func undeclared(root, gate string, declared func(string) bool) ([]config.Key, error) {
	rendered, err := config.Rendered(renderConfig(forInit, root, globalDefaults{}, gate, nil, nil))
	if err != nil {
		return nil, err
	}
	var out []config.Key
	for _, k := range rendered {
		// [verify] default is NO VERIFY's, which says what to do about it, and a
		// project with no gate would otherwise be reported twice. The names
		// inside an open table are the project's own: one namespace where the
		// layout suggests three is a decision, not a gap.
		if k.Dotted == "verify.default" || config.InOpenTable(k.Dotted) {
			continue
		}
		// A key the layout reads off the tree is not a default the project
		// declined. [github] repo renders live for any project inside a repo with
		// an origin remote, so demanding it would report every such project, and
		// pasting it pins whatever the remote said that day into a file that
		// otherwise lets gh infer. renderConfig says the same thing where it
		// declines to write one over an inherited value.
		if derivedFromTree(k.Dotted) {
			continue
		}
		if !declared(k.Dotted) {
			out = append(out, k)
		}
	}
	return out, nil
}

func keysOf(w written) []string {
	out := make([]string, 0, len(w))
	for k := range w {
		out = append(out, k)
	}
	return out
}

// sortedTables orders the carried sections. Alphabetical: the output is read
// as a diff against a file somebody wrote, and an order that moves between
// runs is one nobody can diff. The top level sorts first and is placed rather
// than appended, so nothing below depends on where it lands.
func sortedTables(tables map[string][]string) []string {
	out := make([]string, 0, len(tables))
	for t := range tables {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// notices are the commented lines a printed config ends with. Two kinds, and
// neither is TOML: a reader pastes the file above them, so anything here that
// loaded would put back exactly what it is warning about.
//
// Retired keys come first. internal/config/config.go rejects an undeclared
// frontmatter key as a parse error for a stated reason, and a config key that
// stopped mattering deserves the same care in the other direction: whoever
// wrote that value, in the project's file or the per-user one, chose it and is
// entitled to know it stopped mattering. Without this
// the full report reads as a complete account of the source file while two of
// its settings are simply gone.
//
// Then anything the print could not place, which should always be empty. It is
// the guarantee made self-enforcing rather than argued: every value the source
// declares is either a live line above or is named here. A verify command
// whose name holds a dot can vanish from a printed config with the report
// saying nothing, and this closes that failure mode for the cases nobody has
// met yet.
func notices(body string, in *config.Inspection) string {
	var b strings.Builder
	if len(in.Retired) > 0 {
		fmt.Fprintf(&b, "\n# %d %s nothing in this build reads, so %s not above. Each is named\n",
			len(in.Retired), plural(len(in.Retired), "key", "keys"),
			plural(len(in.Retired), "it is", "they are"))
		b.WriteString("# with the file that declares it: whoever wrote that file chose the value, and\n")
		b.WriteString("# that file is where it comes out. Commented, because this is meant to be pasted.\n")
		for _, r := range in.Retired {
			fmt.Fprintf(&b, "# %s   (%s)\n", r.Key, rel(in.Root, r.Path))
		}
	}
	for _, key := range unplaced(body, in) {
		if b.Len() == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "# drover could not place %s, which %s declares. Report this: a printed\n",
			key, rel(in.Root, in.Path))
		b.WriteString("# config that drops a setting is a config a reader must not paste.\n")
	}
	return b.String()
}

// unplaced is every value the project's own file declares that the printed
// config neither writes nor names as retired.
func unplaced(body string, in *config.Inspection) []string {
	declared, err := config.KeysIn(body)
	if err != nil {
		// carry already says so where a reader will see it, and a second copy
		// of the same failure would report the layout twice.
		return nil
	}
	placed := make(map[string]bool, len(declared)+len(in.Retired))
	for _, k := range declared {
		placed[k] = true
	}
	for _, r := range in.Retired {
		placed[r.Key] = true
	}
	var out []string
	for _, k := range in.Values {
		if !placed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
