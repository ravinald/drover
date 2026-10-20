package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
)

// GlobalFileName is the per-user config layered under every project's
// .drover.toml. It is not named .drover.toml, so that the upward walk in Find
// can never mistake a home directory for a project root.
const GlobalFileName = "config.toml"

// GlobalPath is where the per-user config lives: $XDG_CONFIG_HOME/drover, else
// ~/.config/drover. It returns empty on a machine with no resolvable home,
// which drover treats as "no global config" rather than as an error — the file
// is optional, and a project that names everything it needs must still load.
func GlobalPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "drover", GlobalFileName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "drover", GlobalFileName)
}

// Sources are the files that produced this Config, in the order they were
// layered: the global file first where one is in effect, then the project's.
// `drover check` reports them, because a setting whose origin a reader cannot
// find is worse than a setting nobody made.
func (c *Config) Sources() []string { return slices.Clone(c.sources) }

// GlobalKeys reports the file the per-user config was read from and every key
// it defines, dotted: "paths.backlog", "verify.default". Both are empty when
// there is no global config.
//
// `drover init` uses it to leave those keys out of the file it generates. A
// project that repeats a global value is not inheriting it, it is shadowing it,
// and the next edit to the global file then silently misses that project.
//
// An unparseable file is an error rather than a shrug: a config that will not
// load is one the operator believes is in effect.
func GlobalKeys() (string, []string, error) {
	path := GlobalPath()
	if path == "" {
		return "", nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, nil
	}
	var probe Config
	md, err := toml.DecodeFile(path, &probe)
	if err != nil {
		return path, nil, fmt.Errorf("read %s: %w", path, err)
	}
	defined := md.Keys()
	keys := make([]string, len(defined))
	for i, k := range defined {
		keys[i] = k.String()
	}
	return path, keys, nil
}

// layerGlobal decodes the per-user config into c before the project's own file
// is decoded over the top of it.
func (c *Config) layerGlobal(path string) error {
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Before the unknown keys, matching the order Load uses for the project file
	// and for the same reason: the keys a stale config carries are unknown as a
	// consequence of the version being old, so reporting them first names the
	// symptom and leaves the reader to work out that a rename happened. The
	// refusal versionMatches writes names the keys that changed between versions,
	// which is the whole value of reaching it.
	//
	// c holds this file's values and nothing else, because the project's own file
	// is decoded over the top of it afterwards. So c.SchemaVersion here is the
	// per-user file's own declaration, which is what has to decide: a project at
	// the current version would otherwise mask a per-user file left behind at an
	// older one, and the reader would be told a key does not exist.
	//
	// An absent schema_version is not version 1. Refusing a per-user file that
	// predates the key would stop every command on machines whose file has been
	// fine all along, and it says nothing about which format that file was
	// written against.
	if md.IsDefined("schema_version") {
		if err := c.versionMatches(path); err != nil {
			return err
		}
	}
	if err := unknownKeys(path, md); err != nil {
		return err
	}
	// project names the log directory and the status banner. One value here
	// would give every project on the machine the same name, and the collision
	// is silent: two projects' run logs land in one directory.
	if md.IsDefined("project") {
		return fmt.Errorf("%s: project is per-project — set it in each %s, not here", path, FileName)
	}
	if md.IsDefined("git", "merge_method") {
		return fmt.Errorf("%s: git.merge_method is how one repository merges, so it is per-project: "+
			"set it in that project's %s, not here", path, FileName)
	}
	if md.IsDefined("backlog", "mirror") {
		return fmt.Errorf("%s: backlog.mirror publishes one project's item text, so it is per-project: "+
			"set it in that project's %s, not here", path, FileName)
	}
	c.absorbDeclared(md, path, false)
	return c.checkGlobalPaths(md, path)
}

// locationKeys are the [paths] values that name a directory or a file.
// root_marker and log_prefix are deliberately absent: the marker is a filename
// resolved against whatever root reads it, which is the whole point of it, and
// the prefix is not a path at all.
var locationKeys = []string{"backlog", "ledger", "reports", "register", "specs", "skills", "shelf", "logs"}

// checkGlobalPaths holds the global file's locations to absolute or ~-rooted
// values.
//
// A relative path here has no root of its own to resolve against, so it would
// resolve against whichever project happened to read it — giving every project
// a backlog at the same spot inside itself, which is not what one setting
// serving every project can mean. The prefix must be literal, so a value
// opening with {{ .Project }} is rejected too: it is the same mistake wearing a
// template.
func (c *Config) checkGlobalPaths(md toml.MetaData, path string) error {
	v := reflect.ValueOf(c.Paths)
	t := v.Type()
	for i := range t.NumField() {
		key := t.Field(i).Tag.Get("toml")
		if !slices.Contains(locationKeys, key) || !md.IsDefined("paths", key) {
			continue
		}
		raw := v.Field(i).String()
		if raw == "" || filepath.IsAbs(raw) || strings.HasPrefix(raw, "~/") {
			continue
		}
		return fmt.Errorf(
			"%s: paths.%s = %q is relative — a global path must start with / or ~/, "+
				"and may name the project it resolves for with {{ .Project }}",
			path, key, raw)
	}
	return nil
}

// expandPaths renders the template in every [paths] value.
//
// It exists for the global config, where one setting serves every project and
// so needs a way to name the project it is resolving for at the moment it
// resolves: ~/drover/{{ .Project }}/items. Project files get the same
// treatment because one rule is easier to hold than two, and a directory whose
// name contains a literal {{ was never going to survive a shell anyway.
func (c *Config) expandPaths() error {
	v := reflect.ValueOf(&c.Paths).Elem()
	t := v.Type()
	data := struct{ Project string }{Project: c.Project}
	for i := range t.NumField() {
		f := v.Field(i)
		raw := f.String()
		if !strings.Contains(raw, "{{") {
			continue
		}
		key := "paths." + t.Field(i).Tag.Get("toml")
		tmpl, err := template.New(key).Parse(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, data); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		f.SetString(b.String())
	}
	return nil
}

// unknownKeys reports every key in a decoded file that no field reads. A typo
// would otherwise take effect as silence, and it does so in the global file
// exactly as readily as in a project's.
func unknownKeys(path string, md toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, k := range undecoded {
		keys[i] = k.String()
	}
	slices.Sort(keys)
	return fmt.Errorf("%s: unknown key(s): %v", path, keys)
}
