// Package gitcmd builds every git command drover runs, in an environment drover
// controls.
//
// git reads GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE and their relatives from the
// environment in preference to the repository it was pointed at, so an exported
// variable redirects a command that names its own root. Eighteen call sites
// inherited the caller's environment, and they are the reads that decide which
// branch is checked out, whether a branch merged, what the trunk is, whether the
// tree is clean, and whether a record write would dirty it.
//
// A narrower case has the same cause: a populated submodule reads as empty
// when `rev-parse --show-toplevel` answers about the parent, so a hard-linked
// record inside it escapes the finish preflight and the completion is written
// anyway.
package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// Redirecting names the variables that move git's idea of which repository it is
// operating on. They are dropped from every command's environment: the repository
// comes from -C and from nowhere else.
//
// Anything git reads that does not move the repository stays. GIT_AUTHOR_*,
// GIT_COMMITTER_*, GIT_SSH_COMMAND and GIT_CONFIG_* authenticate and attribute,
// and stripping them would trade a redirect for a push that cannot log in.
func Redirecting() []string {
	return []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
		"GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	}
}

// Env is the process environment with the redirecting variables removed.
func Env() []string {
	drop := map[string]bool{}
	for _, k := range Redirecting() {
		drop[k] = true
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Command is git in root, with the environment sanitized. Callers pass the
// subcommand and its arguments; -C root is prepended here so no caller can omit
// it and inherit a repository from the working directory instead.
func Command(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", at(root, args)...) // #nosec G204 -- fixed git argv
	cmd.Env = Env()
	return cmd
}

// CommandContext is Command bound to a context, for a call a deadline has to
// interrupt.
func CommandContext(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", at(root, args)...) // #nosec G204 -- fixed git argv
	cmd.Env = Env()
	return cmd
}

func at(root string, args []string) []string {
	if root == "" {
		return args
	}
	return append([]string{"-C", root}, args...)
}
