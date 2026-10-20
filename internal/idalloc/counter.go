// Package idalloc reserves item ids so that no two items are ever issued the
// same one.
//
// On one host that is Lock: allocation and the body write happen under one
// exclusive lock. Across hosts it is Counter: a git ref on a shared remote,
// advanced by compare-and-swap, so an id is reserved before any file exists.
// Syncing the backlog afterwards cannot do that job, because two files with
// different slugs under one id merge in git without a conflict.
package idalloc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/gitcmd"
)

// Ref is the counter on the remote. Its commit's tree holds one file per
// prefix, each containing the highest number issued under it.
const Ref = "refs/drover/ids"

// fetched is where the remote's counter lands locally, so the lease can name
// the exact commit this allocation read.
const fetched = "refs/drover/fetched-ids"

// Attempts bounds how many lost leases one allocation absorbs before refusing.
const Attempts = 5

// Unreachable is a remote that could not be read or written for a reason other
// than another host winning the lease.
type Unreachable struct {
	Remote string
	Err    error
}

func (u Unreachable) Error() string {
	return fmt.Sprintf("cannot reach %s for %s: %v", u.Remote, Ref, u.Err)
}

func (u Unreachable) Unwrap() error { return u.Err }

// TimedOut is an allocation that ran past Counter.Timeout, every attempt
// included, before the remote settled it.
type TimedOut struct {
	Remote string
	After  time.Duration
}

func (t TimedOut) Error() string {
	return fmt.Sprintf("no answer from %s for %s within %s", t.Remote, Ref, t.After)
}

// Rejected is a push the remote answered and refused for a reason other than a
// lost lease: a protected-ref rule, a permission, a hook on the server.
type Rejected struct {
	Remote string
	Reason string
}

func (r Rejected) Error() string {
	return fmt.Sprintf("%s refused the push to %s: %s", r.Remote, Ref, r.Reason)
}

// Contended is every attempt losing its lease to another host.
type Contended struct{ Remote string }

func (c Contended) Error() string {
	return fmt.Sprintf("lost the lease on %s at %s %d times running: another host keeps allocating",
		Ref, c.Remote, Attempts)
}

// Counter allocates against Ref on Remote, running git in Root.
type Counter struct {
	Root   string
	Remote string
	// Log hears about each lost lease, so a retry is visible rather than slow.
	Log io.Writer
	// BeforePush runs between the fetch and the push of every attempt. Nil in
	// production; a test interleaves a second host there.
	BeforePush func(attempt int)
	// Timeout bounds the whole allocation. Zero is no bound.
	Timeout time.Duration

	ctx context.Context
	env []string
}

// Next reserves and returns the next number under prefix.
//
// local is the highest number on disk per prefix. The remote's value and the
// local one are merged by taking the larger for every prefix, so a remote seeded
// late, or behind a backlog that grew while the allocator was off, never
// reissues an id that already names a file.
func (c Counter) Next(prefix string, local map[string]int) (int, error) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if c.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	defer cancel()
	c.ctx = ctx
	c.env = append(c.env, "GIT_TERMINAL_PROMPT=0")
	if ssh := c.sshCommand(); ssh != "" {
		c.env = append(c.env, "GIT_SSH_COMMAND="+ssh)
	}
	n, err := c.next(prefix, local)
	if err != nil && ctx.Err() != nil {
		return 0, TimedOut{c.Remote, c.Timeout}
	}
	return n, err
}

// sshCommand is the ssh git would run, made to fail rather than ask for a
// passphrase or a host key. git prefers GIT_SSH_COMMAND, then core.sshCommand,
// then GIT_SSH, so whichever the operator set is the one extended, and a custom
// key or proxy still applies.
func (c Counter) sshCommand() string {
	const batch = " -o BatchMode=yes"
	if v := os.Getenv("GIT_SSH_COMMAND"); v != "" {
		return v + batch
	}
	if v, err := c.git("config", "--get", "core.sshCommand"); err == nil && v != "" {
		return v + batch
	}
	if v := os.Getenv("GIT_SSH"); v != "" {
		return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" + batch
	}
	return "ssh" + batch
}

func (c Counter) next(prefix string, local map[string]int) (int, error) {
	for attempt := 1; attempt <= Attempts; attempt++ {
		base, values, err := c.fetch()
		if err != nil {
			return 0, err
		}
		for p, n := range local {
			values[p] = max(values[p], n)
		}
		values[prefix]++
		commit, err := c.commit(base, values, prefix)
		if err != nil {
			return 0, err
		}
		if c.BeforePush != nil {
			c.BeforePush(attempt)
		}
		lost, err := c.push(base, commit)
		if err != nil {
			return 0, err
		}
		if !lost {
			return values[prefix], nil
		}
		if c.Log != nil && attempt < Attempts {
			fmt.Fprintf(c.Log, "%s on %s moved since it was read: another host allocated first. Refetching (attempt %d of %d)\n",
				Ref, c.Remote, attempt+1, Attempts)
		}
	}
	return 0, Contended{c.Remote}
}

// fetch reads the remote counter. An absent ref is an empty base and no values,
// which the caller seeds from disk.
func (c Counter) fetch() (string, map[string]int, error) {
	values := map[string]int{}
	if tip, err := c.tip(); err != nil {
		return "", nil, Unreachable{c.Remote, err}
	} else if tip == "" {
		return "", values, nil
	}
	if _, err := c.git("fetch", "--quiet", "--no-tags", "--no-write-fetch-head", c.Remote, "+"+Ref+":"+fetched); err != nil {
		return "", nil, Unreachable{c.Remote, err}
	}
	base, err := c.git("rev-parse", "--verify", fetched+"^{commit}")
	if err != nil {
		return "", nil, err
	}
	tree, err := c.git("ls-tree", "--name-only", base)
	if err != nil {
		return "", nil, err
	}
	for _, name := range strings.Fields(tree) {
		raw, err := c.git("cat-file", "blob", base+":"+name)
		if err != nil {
			return "", nil, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return "", nil, fmt.Errorf("%s on %s: %s holds %q, not a number", Ref, c.Remote, name, raw)
		}
		values[name] = n
	}
	return base, values, nil
}

// commit writes the counter's next state as a commit on base, touching no
// index and no work tree.
func (c Counter) commit(base string, values map[string]int, prefix string) (string, error) {
	names := make([]string, 0, len(values))
	for p := range values {
		names = append(names, p)
	}
	sort.Strings(names)
	var tree strings.Builder
	for _, p := range names {
		blob, err := c.gitIn(strconv.Itoa(values[p])+"\n", "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", blob, p)
	}
	treeID, err := c.gitIn(tree.String(), "mktree")
	if err != nil {
		return "", err
	}
	// The nonce is what makes the lease mean anything. Two hosts computing the
	// same next value in the same second, under one identity, otherwise build
	// byte-identical commits; the second push then finds the remote already at
	// its sha, succeeds as a no-op, and both hosts issue the same id.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	args := []string{"commit-tree", treeID, "-m", fmt.Sprintf("%s%d\n\nnonce %x", prefix, values[prefix], nonce)}
	// A host with no identity configured still has to be able to allocate. The
	// counter's author is never read; the id is.
	if _, err := c.git("var", "GIT_COMMITTER_IDENT"); err != nil {
		args = append([]string{"-c", "user.name=drover", "-c", "user.email=drover@localhost"}, args...)
	}
	if base != "" {
		args = append(args, "-p", base)
	}
	return c.git(args...)
}

// push swaps the remote ref from base to commit. lost reports that the lease
// failed because the ref moved, which is the one rejection a retry can fix.
//
// An empty base leases on absence, so two hosts seeding at once cannot both
// win.
//
// The result comes from reading the ref again, not from the rejection's text.
// git words a lease it checked against the advertisement as "stale info", but
// two pushes that both pass that check meet in the remote's own ref update, and
// the loser gets "[remote rejected]" with a reason the server writes (a bare
// repository and GitHub word it differently). Only a ref still at base means
// the remote refused for some other reason, and a porcelain "!" line says the
// remote answered rather than never being reached.
//
// --no-verify keeps the project's pre-push hook, often its test suite, off a
// ref that carries no code.
func (c Counter) push(base, commit string) (lost bool, err error) {
	cmd := c.command("push", "--porcelain", "--quiet", "--no-verify",
		"--force-with-lease="+Ref+":"+base, c.Remote, commit+":"+Ref)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err == nil {
		return false, nil
	}
	var refused error = Unreachable{c.Remote, fmt.Errorf("push: %s", firstLine(stderr.String(), out.String()))}
	if status := rejection(out.String()); status != "" {
		refused = Rejected{c.Remote, strings.TrimSpace(firstLine(stderr.String()) + " " + status)}
	}
	tip, err := c.tip()
	switch {
	case err != nil, tip == base:
		return false, refused
	case tip == commit:
		// The update landed and only its report was lost. The nonce makes this
		// commit no other host's, so the id is this allocation's.
		return false, nil
	}
	return true, nil
}

// tip is the sha Ref names on the remote, or "" when the remote has no such
// ref. ls-remote's exit code separates "no such ref" (2) from "no such
// remote", where fetch reports both as one fatal error worded by the locale.
func (c Counter) tip() (string, error) {
	out, err := c.git("ls-remote", "--exit-code", c.Remote, Ref)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", nil
		}
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	return sha, nil
}

// rejection is the status git's porcelain output gives a refused ref, such as
// "[remote rejected] (pre-receive hook declined)", or "" when no ref line was
// written because the remote never answered.
func rejection(porcelain string) string {
	for _, line := range strings.Split(porcelain, "\n") {
		if fields := strings.SplitN(line, "\t", 3); len(fields) == 3 && fields[0] == "!" {
			return strings.TrimSpace(fields[2])
		}
	}
	return ""
}

// command is git in Root under the allocation's deadline and in an environment
// where nothing can prompt.
func (c Counter) command(args ...string) *exec.Cmd {
	cmd := gitcmd.CommandContext(c.ctx, c.Root, args...)
	cmd.Env = append(cmd.Env, c.env...)
	// ssh outlives a killed git and keeps its stderr open, so without a bound
	// on the wait the deadline would end git and still block on the pipe.
	cmd.WaitDelay = time.Second
	return cmd
}

func (c Counter) git(args ...string) (string, error) { return c.gitIn("", args...) }

// gitIn runs git with stdin and returns trimmed stdout. A failure carries git's
// own first line of stderr, which is what names the remote's problem.
func (c Counter) gitIn(stdin string, args ...string) (string, error) {
	cmd := c.command(args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s: %w", args[0], msg, err)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func firstLine(texts ...string) string {
	for _, t := range texts {
		for _, line := range strings.Split(t, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}
