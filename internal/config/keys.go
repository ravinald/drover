package config

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Key is one setting this build reads, named the way a file writes it.
//
// The set is walked off the Config types rather than listed here, and the
// defaults come from applyDefaults rather than from a second copy of them. A
// hand-kept inventory drifts the first time somebody adds a field and not the
// line naming it.
type Key struct {
	// Table is the [section] the key sits under, empty at the top level.
	Table string
	// Name is the bare key as a file writes it.
	Name string
	// Dotted is Table and Name joined, each segment quoted where TOML needs it.
	// toml.Key.String names keys this way, so every comparison against a file is
	// made on this form and a gate called "ci.linux" survives as one key rather
	// than becoming a table called ci.
	Dotted string
	// Default is the TOML literal drover resolves the key to when nothing
	// declares it, so a reported line pastes into a file unchanged.
	Default string
	// Open marks a table whose entry names a project chooses: [verify],
	// [runners], ids.namespaces. Nothing can enumerate those, so the table is
	// the unit reported and its entries are never missing.
	Open bool
}

// Keys is every setting this build reads, in the order the Config types declare
// them.
func Keys() []Key {
	base := &Config{}
	base.applyDefaults()
	out := walkKeys(reflect.ValueOf(*base), "", false)
	for i, k := range out {
		if def, ok := resolved[k.Dotted]; ok {
			out[i].Default = def
		}
	}
	return out
}

// resolved are the keys whose effective default is computed where they are read
// rather than filled in by applyDefaults, so the zero value sitting in the
// struct is not what a paste has to carry. [register] backend is empty there
// and resolves to the tracker (see Config.RegisterBackend), because unset had
// to stay distinguishable from a project that named a backend.
var resolved = map[string]string{
	"register.backend":        strconv.Quote(BackendGitHub),
	"backlog.mirror":          strconv.Quote(MirrorNone),
	"ids.allocator":           strconv.Quote(AllocatorLocal),
	"ids.remote":              strconv.Quote("origin"),
	"forge.reruns":            strconv.Itoa(DefaultReruns),
	"forge.checks_wait":       strconv.Quote("10m"),
	"usage.session_max_hours": strconv.Itoa(DefaultSessionMaxHours),
	"usage.context_warn":      strconv.Itoa(DefaultContextWarn),
}

// walkKeys collects one Key per toml-tagged field, descending into nested
// structs. Default carries the literal of whatever value was walked, so the
// same walk answers "what would drover write by default" over a defaulted
// Config and "what does this project carry" over a decoded one.
//
// entries controls whether the names inside an open table are collected too.
// Keys() leaves them out: a map entry is a name the project invented, not a
// setting this build reads, and reporting one as missing would tell a project
// it had failed to declare a gate it never wanted.
func walkKeys(v reflect.Value, table string, entries bool) []Key {
	t := v.Type()
	var out []Key
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		dotted := Join(table, tag)
		f := v.Field(i)
		if f.Kind() == reflect.Struct {
			out = append(out, walkKeys(f, dotted, entries)...)
			continue
		}
		key := Key{Table: table, Name: tag, Dotted: dotted, Default: literal(f)}
		if f.Kind() == reflect.Map {
			key.Open = true
			out = append(out, key)
			if entries {
				out = append(out, mapEntries(f, dotted)...)
			}
			continue
		}
		out = append(out, key)
	}
	return out
}

// mapEntries names what a project put inside an open table, one level down and
// then into a struct value where the entry holds one. [runners] is why: its
// entries are structs, and a project that declared one wrote the fields inside
// it rather than the entry as a whole.
func mapEntries(m reflect.Value, table string) []Key {
	var out []Key
	for _, name := range sortedMapKeys(m) {
		dotted := Join(table, name)
		v := m.MapIndex(reflect.ValueOf(name))
		out = append(out, Key{Table: table, Name: Quote(name), Dotted: dotted, Default: literal(v)})
		if v.Kind() == reflect.Struct {
			out = append(out, walkKeys(v, dotted, true)...)
		}
	}
	return out
}

// literal renders a value as the TOML that would produce it. Tables and arrays
// come out inline, because the line is meant to be pasted somewhere a reader
// chose rather than written into a structure this cannot see.
func literal(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return strconv.Quote(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Pointer:
		// The only pointer applyDefaults leaves nil is [git] enabled, a pointer
		// so that unset and false stay distinguishable. Unset reads as on (see Git.On), so
		// that is the value a paste has to carry.
		if v.IsNil() {
			return "true"
		}
		return literal(v.Elem())
	case reflect.Interface:
		// [supervise] is a table of untyped values, so each entry arrives
		// wrapped; the literal is the value inside.
		if v.IsNil() {
			return `""`
		}
		return literal(v.Elem())
	case reflect.Slice:
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = literal(v.Index(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		parts := make([]string, 0, v.Len())
		for _, name := range sortedMapKeys(v) {
			parts = append(parts, Quote(name)+" = "+literal(v.MapIndex(reflect.ValueOf(name))))
		}
		return inline(parts)
	case reflect.Struct:
		var parts []string
		t := v.Type()
		for i := range t.NumField() {
			tag := t.Field(i).Tag.Get("toml")
			if tag == "" || tag == "-" {
				continue
			}
			parts = append(parts, tag+" = "+literal(v.Field(i)))
		}
		return inline(parts)
	}
	return fmt.Sprintf("%v", v.Interface())
}

func inline(parts []string) string {
	if len(parts) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// sortedMapKeys orders a table so that a rendered line names its entries in the
// same order on the next run.
func sortedMapKeys(m reflect.Value) []string {
	out := make([]string, 0, m.Len())
	for _, k := range m.MapKeys() {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}

// Quote renders one key segment the way a TOML file writes it, bare where
// every character allows and double-quoted otherwise. It is toml.Key.String's
// own rule, because the dotted keys this package builds are compared against
// the ones that function produced.
func Quote(segment string) string {
	if segment == "" {
		return `""`
	}
	for _, r := range segment {
		bare := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !bare {
			return strconv.Quote(segment)
		}
	}
	return segment
}

// Join appends one raw segment to an already-dotted table path.
func Join(table, segment string) string {
	if table == "" {
		return Quote(segment)
	}
	return table + "." + Quote(segment)
}

// OpenTables are the tables whose entry names a project chooses: [verify],
// [runners], ids.namespaces, severity.ranks, frontmatter.shape. The list is
// every map-typed field in Config, so a table added there joins it without a
// line here.
//
// A comparison of key sets stops at one of these. Counting the entries inside
// would report a project that declared one namespace as missing the other two,
// which is not a gap in its config: it is the project having decided.
func OpenTables() []string {
	var out []string
	for _, k := range Keys() {
		if k.Open {
			out = append(out, k.Dotted)
		}
	}
	return out
}

// InOpenTable reports whether a dotted key is a name inside an open table
// rather than a setting this build reads.
func InOpenTable(key string) bool {
	return slices.ContainsFunc(OpenTables(), func(table string) bool {
		return strings.HasPrefix(key, table+".")
	})
}

// TableState is what one file said about an open table.
type TableState int

const (
	// TableAbsent is a file that wrote neither the table nor anything in it.
	// The table is left to another layer or to drover's default.
	TableAbsent TableState = iota
	// TableEmpty is a file that opened the table and named nothing in it: it
	// looked and closed nothing. No gate, no namespace, no shape, and no
	// fallback to the default either, which is why it is not absent.
	TableEmpty
	// TablePartial is a file that named entries. It closed what it named and
	// nothing else; the names it left out are not gaps.
	TablePartial
)

func (s TableState) String() string {
	switch s {
	case TableEmpty:
		return "empty"
	case TablePartial:
		return "partial"
	}
	return "absent"
}

// OpenTable is one file's answer about one open table.
//
// It is the only place that answer is derived. A renderer that re-derives it
// from key prefixes and a check that re-derives it from a decoded map disagree
// the first time a file writes a table the other did not expect, which is how
// an inherited empty [frontmatter.shape] came back filled in.
type OpenTable struct {
	// Key is the table, dotted: "ids.namespaces".
	Key string
	// Scope is the table Key sits under, empty for [verify] and [runners]. A
	// declaration written back as `name = { … }` belongs in [Scope]; written
	// at the foot of a file it lands under whatever header came last.
	Scope string
	// Name is Key's last segment, the bare name written inside Scope.
	Name string
	// State is absent, empty or partial.
	State TableState
	// Entries are the dotted names the file wrote directly inside the table,
	// sorted: verify.docs, runners.agy. Empty unless State is TablePartial.
	Entries []string
	// Path is the file this answer is about, empty where that file does not
	// exist. An absent answer names it too: absent from which file is the
	// whole question when two are layered.
	Path string
}

// OpenTable is the project's own file's answer about an open table, and false
// when key is not one.
func (c *Config) OpenTable(key string) (OpenTable, bool) { return c.openTable(key, true) }

// InheritedTable is the per-user file's answer about an open table, absent
// with an empty Path where there is no per-user file, and false when key is
// not an open table.
func (c *Config) InheritedTable(key string) (OpenTable, bool) { return c.openTable(key, false) }

func (c *Config) openTable(key string, own bool) (OpenTable, bool) {
	var found *Key
	for _, k := range Keys() {
		if k.Open && k.Dotted == key {
			found = &k
			break
		}
	}
	if found == nil {
		return OpenTable{}, false
	}
	out := OpenTable{Key: key, Scope: found.Table, Name: found.Name}
	i := slices.IndexFunc(c.layers, func(l layer) bool { return l.own == own })
	if i < 0 {
		return out, true
	}
	l := c.layers[i]
	out.Path = l.path
	if !l.declares(key) {
		return out, true
	}
	// Open table names are field tags, which never need quoting, so the
	// segment count of the dotted form is the table's depth.
	depth := strings.Count(key, ".") + 1
	prefix := key + "."
	seen := map[string]bool{}
	for _, k := range l.keys {
		if len(k) <= depth || !strings.HasPrefix(k.String(), prefix) {
			continue
		}
		seen[k[:depth+1].String()] = true
	}
	out.State = TableEmpty
	if len(seen) > 0 {
		out.State = TablePartial
		for e := range seen {
			out.Entries = append(out.Entries, e)
		}
		sort.Strings(out.Entries)
	}
	return out, true
}

// Rendered reads a TOML text back as the keys it declares and the literal each
// one carries.
//
// It exists so a generator can learn what its own output wrote instead of
// carrying a second list of the keys it writes. That list drifts the first time
// somebody adds a key to one and not the other, which is the failure this whole
// file is here to stop. Defaults are deliberately not filled in: the answer is
// what the text says, so a value the generator inferred from the tree comes
// back as the inferred one rather than as the default it overrode.
func Rendered(text string) ([]Key, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return nil, err
	}
	// One text with nothing layered under it, so every key in it is the
	// project's own.
	c.absorbDeclared(md, "", true)
	var out []Key
	for _, k := range walkKeys(reflect.ValueOf(c), "", true) {
		if c.Declared(k.Dotted) {
			out = append(out, k)
		}
	}
	return out, nil
}

// KeysIn is Rendered reduced to the dotted key set, table headers included.
func KeysIn(text string) ([]string, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(md.Keys()))
	for _, k := range md.Keys() {
		out = append(out, k.String())
	}
	return out, nil
}

// Table splits a dotted key into the section a file writes it under and the
// name inside it, ready to write. An entry in an open table sits under that
// table: verify.docs is written under [verify], and runners.claude.headless
// under [runners.claude].
//
// The split is on the last separator outside a quoted segment. A bare
// LastIndex would cut verify."ci.linux" in the middle of its own name and
// report a table nothing reads, which is how that gate disappeared from a
// printed config rather than being carried into it.
func Table(dotted string) (table, name string) {
	if i := lastSep(dotted); i >= 0 {
		return dotted[:i], dotted[i+1:]
	}
	return "", dotted
}

// lastSep is the index of the final key separator outside a quoted segment,
// or -1 for a single-segment key.
func lastSep(dotted string) int {
	quoted := false
	last := -1
	for i := 0; i < len(dotted); i++ {
		switch dotted[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case '.':
			if !quoted {
				last = i
			}
		}
	}
	return last
}

// Leaves drops every key in the set that another key in it sits under, so a
// rendered block declares each value once. Without it a project that wrote
// [severity] ranks gets both the table and its entries, and TOML rejects the
// second spelling of the same path.
func Leaves(dotted []string) []string {
	out := make([]string, 0, len(dotted))
	for _, k := range dotted {
		prefix := k + "."
		if slices.ContainsFunc(dotted, func(other string) bool {
			return strings.HasPrefix(other, prefix)
		}) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
