package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravinald/drover/internal/runner"
)

// launchInfo is what drover resolved before it hands the checkout to somebody
// else's binary: which body the prompt was assembled from, which template
// assembled it, which binary PATH found, and where the events land.
//
// One type rather than a copy per command, because per-command copies drift:
// run, review, draft and drive all launch sessions, and a banner each one
// assembles for itself ends up naming different things, or nothing, about the
// body and template it launched with.
type launchInfo struct {
	ID     string // bare item id
	Stage  string // implement, review, draft
	Round  int    // which pass over one item; 0 for a command that runs one
	Branch string // empty when the project declares no branches
	NoGit  string // why there is no branch, when there is none
	Item   string // the body the prompt was assembled from
	Prompt string // the template that assembled it
	Runner string
	Binary string // argv[0] as PATH resolved it, or the bare name when it did not
	Argv   string
	Log    string // empty for an interactive session, which writes none
}

// write prints the preamble to w, one label per line, skipping every field the
// caller left empty. A label with nothing behind it would read as a value
// drover resolved to nothing, which is a different fact from one this command
// has no answer for.
func (l launchInfo) write(w io.Writer) {
	head := l.ID + " · " + l.Stage
	switch {
	case l.Branch != "":
		head += " · branch " + l.Branch
	case l.NoGit != "":
		head += " · " + l.NoGit
	}
	if l.Round > 0 {
		head += fmt.Sprintf(" · round %d", l.Round)
	}
	fmt.Fprintf(w, "==> %s\n", head)

	rows := [][2]string{{"item", l.Item}, {"prompt", l.Prompt}}
	if l.Runner != "" {
		rows = append(rows, [2]string{"runner", l.Runner + ": " + l.Binary})
	}
	rows = append(rows, [2]string{"argv", l.Argv}, [2]string{"log", l.Log})
	for _, r := range rows {
		if r[1] == "" {
			continue
		}
		fmt.Fprintf(w, "==> %-6s %s\n", r[0], r[1])
	}
}

// from fills the runner half of the preamble, which every caller resolves the
// same way: the declared name, argv[0] as PATH found it, and the argv drover
// will execute.
func (l launchInfo) from(name string, spec runner.Spec, mode runner.Mode) launchInfo {
	l.Runner, l.Binary, l.Argv = name, lookBinary(spec.Binary(mode)), spec.Display(mode)
	return l
}

// promptPath names the template a prompt is assembled from: the project's own,
// or the built-in one prompts.Load falls back to. configured is the path as
// .drover.toml declared it, builtin the name of the fallback.
//
// The distinction is the whole reason for printing it. A project that meant to
// override a template and left the key unset gets its runs assembled from
// drover's default, and nothing downstream says so: the prompt reads like a
// prompt either way.
func (s *session) promptPath(configured, builtin string) string {
	if configured == "" {
		return "built-in " + builtin
	}
	return rel(s.cfg.Root, s.cfg.Abs(configured))
}

// displayPath is a path as an operator reads it: relative to the project when
// it sits inside the tree, ~-prefixed when it sits under their home, absolute
// otherwise.
//
// The log directory defaults to ~/.cache/drover/<project>/, outside the tree,
// so rel() alone renders it as a stack of "..". All three forms paste into a
// shell, which is the only thing anybody does with a path drover prints.
func displayPath(root, path string) string {
	if path == "" {
		return ""
	}
	if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	// A home drover cannot read, or one that is the filesystem root, leaves the
	// path alone: "~/Users/me/x" would be a path that resolves to nothing.
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if path == home {
		return "~"
	}
	if rest := strings.TrimPrefix(path, home+string(filepath.Separator)); rest != path {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}
