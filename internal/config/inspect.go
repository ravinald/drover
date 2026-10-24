package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Inspection is a project config read without being validated.
//
// Load is the wrong door for this on purpose. The configs worth asking about
// are the old ones, and Load refuses those before it can say anything a reader
// could act on: an older schema_version is an error, and so is every key a
// rename left behind.
//
// Nothing here checks a root marker, a severity rank, a shape or a register
// backend, so an Inspection is never a substitute for a Config a command acts
// on.
type Inspection struct {
	// Root is the directory holding the project file.
	Root string
	// Path is the project's own .drover.toml.
	Path string
	// Version is schema_version as the project's own file wrote it, 0 when it
	// declares none. VersionDeclared tells absence from an explicit 0, and
	// VersionSource names the per-user file where that one supplies the value.
	Version int
	// Config carries every layered file's values with this build's defaults
	// behind them.
	Config *Config
	// Retired is every key a layered file declares that no field reads, with
	// the file that declared it. Load refuses these by name; this reports them
	// because whoever wrote that value is entitled to know it stopped mattering,
	// and a per-user key is not the project's to remove.
	Retired []Retired
	// Values are the dotted keys the project's own file wrote that carry a
	// value rather than opening a table, an empty open table counted as a
	// value because it is a decision on its own. Anything printed from this
	// config that does not declare one of these has dropped a setting.
	Values []string
	// Bare are the fixed tables the project's own file opened as a header with
	// nothing under it. The declaration is the decision: SeverityConfigured
	// reads whether [severity] exists, not what is in it, so a copy assembled
	// from values alone dropped an empty one and turned UNDECLARED KEY into a clean
	// report. Open tables are absent from this list, since Written already
	// round-trips their emptiness as `{}`.
	Bare []string

	// own is the project's file decoded with nothing layered under it, the only
	// decode where a value is the project's by construction. The layered Config
	// cannot say which file wrote what a merged table holds: a project
	// declaring one namespace beside a global declaring two reads back as a
	// project that declared three.
	own *Config

	// global is the per-user file that had a say, empty where none did, and
	// globalVersion the schema_version it declared, 0 where it declared none.
	global        string
	globalVersion int
}

// Retired is one key nothing in this build reads, and the file carrying it.
type Retired struct {
	Key  string
	Path string
}

// Inspect reads the config for the tree containing dir, layering the per-user
// file under the project's the way Load does and validating neither.
func Inspect(dir string) (*Inspection, error) {
	root, err := Find(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, FileName)

	in := &Inspection{Root: root, Path: path, Config: &Config{}, own: &Config{}}
	c := in.Config

	if g := GlobalPath(); g != "" {
		if _, err := os.Stat(g); err == nil {
			md, err := toml.DecodeFile(g, c)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", g, err)
			}
			in.global = g
			in.globalVersion = c.SchemaVersion
			in.absorb(md, g, false)
			c.sources = append(c.sources, g)
		}
	}
	c.sources = append(c.sources, path)

	// Read once and decoded twice: the layered value is what the project runs
	// on, and the bare one is what the project itself decided. Defaults are
	// not applied to the second, so a key it does not carry is one the project
	// never wrote.
	data, err := os.ReadFile(path) // #nosec G304 -- path is the project file Find resolved.
	if err != nil {
		return nil, err
	}
	globalNtfy := c.Supervise[SuperviseNtfyKey]
	md, err := toml.Decode(string(data), c)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c.layerNtfy(globalNtfy)
	ownMD, err := toml.Decode(string(data), in.own)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	in.own.absorbDeclared(ownMD, path, true)
	in.absorb(md, path, true)
	in.Version = in.own.SchemaVersion
	c.Root = root
	// Paths stay as written. Rendering {{ .Project }} here would print a
	// resolved directory where the file holds a template, and a reader pasting
	// that back would pin one project's path into a setting meant to serve
	// every project.
	c.applyDefaults()
	return in, nil
}

// absorb records what one file declared and what it declared that nothing
// reads.
func (in *Inspection) absorb(md toml.MetaData, path string, own bool) {
	in.Config.absorbDeclared(md, path, own)
	for _, k := range undecoded(md) {
		in.Retired = append(in.Retired, Retired{Key: k.String(), Path: path})
	}
	if !own {
		return
	}
	for _, k := range md.Keys() {
		dotted := k.String()
		// A bare [table] header carries no value, except on an open table,
		// where empty is a decision absent is not.
		if md.Type(k...) == "Hash" {
			t, open := in.Config.OpenTable(dotted)
			if !open {
				if !hasChild(md.Keys(), dotted) {
					in.Bare = append(in.Bare, dotted)
				}
				continue
			}
			if t.State != TableEmpty {
				continue
			}
		}
		in.Values = append(in.Values, dotted)
	}
}

// hasChild reports whether any key sits under the dotted table.
//
// A table with one leaf is carried by that leaf, which writes the header on its
// way out. Only a header with nothing beneath it has to be carried on its own.
func hasChild(keys []toml.Key, dotted string) bool {
	prefix := dotted + "."
	for _, k := range keys {
		if strings.HasPrefix(k.String(), prefix) {
			return true
		}
	}
	return false
}

// Declared reports whether any layered file wrote the dotted key.
func (in *Inspection) Declared(key string) bool { return in.Config.Declared(key) }

// Written is the TOML literal of every key the project's own file declared,
// keyed dotted, entries inside an open table included.
//
// Read off the project's file on its own. A key the per-user file supplies is
// one this project inherits, and a printed copy writing it would pin today's
// global value where the original resolves whatever that file says next week.
func (in *Inspection) Written() map[string]string {
	out := map[string]string{}
	for _, k := range walkKeys(reflect.ValueOf(*in.own), "", true) {
		if in.own.ProjectDeclared(k.Dotted) {
			out[k.Dotted] = k.Default
		}
	}
	return out
}

// Policy is the policy the project's own file sets for an action, and whether
// it sets one. A per-user value is not the project's: a table printed for the
// project to paste would pin it where the original inherits.
func (in *Inspection) Policy(action string) (Policy, bool) { return in.own.Policy(action) }

// Missing is every setting this build reads that no layered file declared, in
// reference order, top-level keys first.
//
// A key the per-user file supplies is not missing: a project repeating it would
// shadow the global value rather than inherit it. An open table is missing only
// when both files leave it absent; empty and partial are decisions. Top-level
// keys lead because a bare key printed after a [table] header belongs to that
// table, and `runners = {…}` emitted below [run] pastes back as run.runners.
func (in *Inspection) Missing() []Key {
	var top, nested []Key
	for _, k := range Keys() {
		if in.decided(k) {
			continue
		}
		if k.Table == "" {
			top = append(top, k)
			continue
		}
		nested = append(nested, k)
	}
	return append(top, nested...)
}

func (in *Inspection) decided(k Key) bool {
	if !k.Open {
		return in.Declared(k.Dotted)
	}
	own, _ := in.Config.OpenTable(k.Dotted)
	inherited, _ := in.Config.InheritedTable(k.Dotted)
	return own.State != TableAbsent || inherited.State != TableAbsent
}

// Stale reports whether the project's own file fails to declare the
// schema_version this build understands.
//
// Absence is stale whatever the per-user file says. A global schema_version
// lets the project load today, and says nothing about which format the
// project's file was written against; the project is the one file that has to
// carry the answer.
func (in *Inspection) Stale() bool {
	return !in.VersionDeclared() || in.Version != SchemaVersion
}

// VersionDeclared reports whether the project's own file wrote schema_version.
// An explicit 0 is declared; a per-user value never is.
func (in *Inspection) VersionDeclared() bool { return in.own.ProjectDeclared("schema_version") }

// VersionSource is the file a report about schema_version names, and the
// version that file declares.
//
// That is the project's own file, except where the project declares none and
// inherits a version from the per-user file that this build does not read.
// That mismatch is the per-user file's, and naming .drover.toml for it sends
// the reader to a file that says nothing about it. An inherited version that
// matches is no mismatch at all, so absence is reported against the project.
func (in *Inspection) VersionSource() (string, int) {
	if !in.VersionDeclared() && in.global != "" && in.globalVersion != SchemaVersion &&
		slices.ContainsFunc(in.Config.layers, func(l layer) bool { return !l.own && l.declares("schema_version") }) {
		return in.global, in.globalVersion
	}
	return in.Path, in.Version
}
