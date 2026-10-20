// Package dashboard holds the status templates drover ships in the binary, so
// a dashboard can start from one that works rather than from a blank file.
package dashboard

import (
	"embed"
	"fmt"
	"slices"
	"strings"
)

// Prefix marks a template reference as built in rather than a file path.
const Prefix = "builtin:"

//go:embed obsidian.md.tmpl html.html.tmpl obsidian.css
var files embed.FS

var sources = map[string]string{
	"html":     "html.html.tmpl",
	"obsidian": "obsidian.md.tmpl",
}

var stylesheets = map[string]string{
	"obsidian": "obsidian.css",
}

// Names is every built-in template, sorted.
func Names() []string {
	return sortedKeys(sources)
}

// Builtin reports whether ref names a built-in template, and the name after
// the prefix. A ref without the prefix is a file path.
func Builtin(ref string) (string, bool) {
	return strings.CutPrefix(ref, Prefix)
}

// Source is the text of the built-in template name. An unknown name is refused
// with every name there is, because the operator typed it from memory.
func Source(name string) (string, error) {
	file, ok := sources[name]
	if !ok {
		return "", fmt.Errorf("%s%s is not a built-in template (known: %s)", Prefix, name, known(Names()))
	}
	b, err := files.ReadFile(file)
	return string(b), err
}

// HTML reports whether the built-in template name renders HTML, which is the
// case that has to execute through html/template so every value is escaped.
func HTML(name string) bool { return name == "html" }

// CSS is the stylesheet that goes with the built-in template name. The HTML
// page carries its own inline, so only a template rendered elsewhere has one.
func CSS(name string) (string, error) {
	file, ok := stylesheets[name]
	if !ok {
		return "", fmt.Errorf("no stylesheet named %q (known: %s)", name, strings.Join(sortedKeys(stylesheets), ", "))
	}
	b, err := files.ReadFile(file)
	return string(b), err
}

func known(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = Prefix + n
	}
	return strings.Join(out, ", ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
