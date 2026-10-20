package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/idalloc"
)

// nextID is the id mint would issue under prefix with the local allocator.
func nextID(t *testing.T, s *session, prefix string) string {
	t.Helper()
	highest, err := s.highestIDs()
	if err != nil {
		t.Fatal(err)
	}
	return backlog.ID{Prefix: prefix, Num: highest[prefix] + 1}.Bare()
}

// stems lists the ids of the bodies in dir, one entry per file, so a duplicate
// shows up twice.
func stems(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if stem, _, ok := strings.Cut(e.Name(), "-"); ok {
			out = append(out, stem)
		}
	}
	return out
}

// Review sessions under drive --jobs share one backlog, and each can run new at
// the same moment. Distinct titles make distinct slugs, so an existence check
// on the full path passes every one of them.
func TestConcurrentNewIssuesDistinctIDs(t *testing.T) {
	dir := newProject(t)
	const n = 12
	errs := make([]error, n)
	capture(t, func() {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = newCmd.RunE(newCmd, []string{"bug", fmt.Sprintf("concurrent finding %d", i)})
			}()
		}
		close(start)
		wg.Wait()
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("new %d: %v", i, err)
		}
	}
	got := stems(t, filepath.Join(dir, "work"))
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Errorf("%s was issued twice: %v", id, got)
		}
		seen[id] = true
	}
	if len(got) != n {
		t.Errorf("%d bodies written, want %d: %v", len(got), n, got)
	}
}

// idHost is one machine's checkout of a project whose ids come from remote.
func idHost(t *testing.T, remote string) string {
	t.Helper()
	dir := newProject(t)
	conf := filepath.Join(dir, ".drover.toml")
	raw, err := os.ReadFile(conf) // #nosec G304 -- the fixture just written
	if err != nil {
		t.Fatal(err)
	}
	ns := `namespaces = { B = "bug", F = "feature" }`
	text := strings.Replace(string(raw), ns, ns+fmt.Sprintf("\nallocator = %q\nremote = %q", config.AllocatorGit, remote), 1)
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	return dir
}

func bareRemote(t *testing.T) string {
	t.Helper()
	isolate(t)
	dir := filepath.Join(t.TempDir(), "ids.git")
	git(t, filepath.Dir(dir), "init", "-q", "--bare", dir)
	return dir
}

func sessionAt(t *testing.T, dir string) *session {
	t.Helper()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openFrom(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// remoteValue is what the counter on remote says for prefix.
func remoteValue(t *testing.T, remote, prefix string) string {
	t.Helper()
	return git(t, remote, "cat-file", "blob", idalloc.Ref+":"+prefix)
}

// Two hosts read the counter before either pushes. The second push carries a
// lease on a commit that is no longer the tip, so it loses, refetches and takes
// the next number rather than the one the winner already holds.
func TestGitAllocatorInterleavedHostsGetDistinctIDs(t *testing.T) {
	remote := bareRemote(t)
	a, b := sessionAt(t, idHost(t, remote)), sessionAt(t, idHost(t, remote))

	var idB backlog.ID
	fired := false
	idPushHook = func(int) {
		if fired {
			return
		}
		fired = true
		var err error
		if idB, _, err = b.mint("B", "from-b", "body\n", nil); err != nil {
			t.Errorf("host b: %v", err)
		}
	}
	t.Cleanup(func() { idPushHook = nil })

	var idA backlog.ID
	stderr := captureErr(t, func() {
		var err error
		if idA, _, err = a.mint("B", "from-a", "body\n", nil); err != nil {
			t.Errorf("host a: %v", err)
		}
	})
	if idB.Bare() != "B1" || idA.Bare() != "B2" {
		t.Errorf("host b got %s and host a got %s, want B1 and B2", idB.Bare(), idA.Bare())
	}
	if !strings.Contains(stderr, "Refetching (attempt 2 of 5)") {
		t.Errorf("the losing host's retry is not visible:\n%s", stderr)
	}
	if got := remoteValue(t, remote, "B"); got != "2" {
		t.Errorf("remote B = %s, want 2", got)
	}
}

// Turning the allocator on in a project that already has items must not hand
// out an id on disk. The seed carries every prefix, so a second host whose disk
// is empty still allocates above the first host's F items.
func TestGitAllocatorSeedsFromDisk(t *testing.T) {
	remote := bareRemote(t)
	first := idHost(t, remote)
	for _, name := range []string{"B05-old.md", "F04-old.md"} {
		if err := os.WriteFile(filepath.Join(first, "work", name), []byte(body("Old", "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id, _, err := sessionAt(t, first).mint("B", "new", "body\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.Bare() != "B6" {
		t.Errorf("first allocation = %s, want B6", id.Bare())
	}
	if got := remoteValue(t, remote, "F"); got != "4" {
		t.Errorf("seeded F = %s, want 4", got)
	}

	id, _, err = sessionAt(t, idHost(t, remote)).mint("F", "elsewhere", "body\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.Bare() != "F5" {
		t.Errorf("second host allocated %s, want F5", id.Bare())
	}
}

// A host that loses every lease stops after five and writes nothing.
func TestGitAllocatorRefusesAfterFiveLostLeases(t *testing.T) {
	remote := bareRemote(t)
	dirA := idHost(t, remote)
	a, b := sessionAt(t, dirA), sessionAt(t, idHost(t, remote))
	inside := false
	idPushHook = func(int) {
		if inside {
			return
		}
		inside = true
		defer func() { inside = false }()
		if _, _, err := b.mint("B", fmt.Sprintf("b-%d", len(stems(t, b.cfg.Abs("work")))), "body\n", nil); err != nil {
			t.Errorf("host b: %v", err)
		}
	}
	t.Cleanup(func() { idPushHook = nil })

	var err error
	captureErr(t, func() { _, _, err = a.mint("B", "starved", "body\n", nil) })
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), remote) {
		t.Fatalf("err = %v (exit %d), want a refusal naming %s", err, Code(err), remote)
	}
	if got := stems(t, filepath.Join(dirA, "work")); len(got) != 0 {
		t.Errorf("a refused allocation wrote %v", got)
	}
}

// An unreachable remote refuses rather than falling back to disk, which would
// reopen the race the counter closes. --local is the operator's decision.
func TestGitAllocatorUnreachableRemoteRefuses(t *testing.T) {
	isolate(t)
	remote := filepath.Join(t.TempDir(), "missing.git")
	dir := idHost(t, remote)
	t.Cleanup(func() { localAlloc = false })

	err := execute(t, "new", "bug", "the remote is down")
	if Code(err) != ExitRefused {
		t.Fatalf("exit = %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{remote, "--local", "fatal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
	if got := stems(t, filepath.Join(dir, "work")); len(got) != 0 {
		t.Errorf("a refused allocation wrote %v", got)
	}

	capture(t, func() {
		if err := execute(t, "new", "bug", "the remote is down", "--local"); err != nil {
			t.Fatalf("new --local: %v", err)
		}
	})
	if got := stems(t, filepath.Join(dir, "work")); len(got) != 1 || got[0] != "B01" {
		t.Errorf("--local wrote %v, want [B01]", got)
	}
}

// pushBarrier holds every push to remote in pre-receive until two have arrived,
// so both have already passed git's client-side lease check against the same
// advertised ref. The remote's own ref transaction then rejects the second,
// which git reports as "[remote rejected]" with a reason the server words,
// never as "stale info".
func pushBarrier(t *testing.T, remote string) {
	t.Helper()
	hook := `#!/bin/sh
cat >/dev/null
dir=$(dirname "$0")
touch "$dir/arrived.$$"
i=0
while [ "$(ls "$dir" | grep -c '^arrived\.')" -lt 2 ] && [ $i -lt 200 ]; do
	sleep 0.05
	i=$((i+1))
done
`
	stub(t, filepath.Join(remote, "hooks", "pre-receive"), hook)
}

// Two hosts whose pushes meet inside the remote both pass the client-side
// lease check. The one the remote rejects lost the lease all the same, and has
// to retry rather than report the remote as unreachable and advise --local.
func TestGitAllocatorPushesRacingInsideTheRemoteRetry(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("seeded=%v", seeded), func(t *testing.T) {
			remote := bareRemote(t)
			a, b := sessionAt(t, idHost(t, remote)), sessionAt(t, idHost(t, remote))
			want := map[string]bool{"B1": true, "B2": true}
			if seeded {
				if _, _, err := a.mint("B", "seed", "body\n", nil); err != nil {
					t.Fatal(err)
				}
				want = map[string]bool{"B2": true, "B3": true}
			}
			pushBarrier(t, remote)

			ids := make([]backlog.ID, 2)
			errs := make([]error, 2)
			stderr := captureErr(t, func() {
				var wg sync.WaitGroup
				for i, s := range []*session{a, b} {
					wg.Add(1)
					go func() {
						defer wg.Done()
						ids[i], _, errs[i] = s.mint("B", fmt.Sprintf("racing-%d", i), "body\n", nil)
					}()
				}
				wg.Wait()
			})
			for i, err := range errs {
				if err != nil {
					t.Fatalf("host %d: %v\nstderr:\n%s", i, err, stderr)
				}
			}
			if ids[0] == ids[1] || !want[ids[0].Bare()] || !want[ids[1].Bare()] {
				t.Errorf("hosts got %s and %s, want %v", ids[0].Bare(), ids[1].Bare(), want)
			}
			if !strings.Contains(stderr, "Refetching (attempt 2 of 5)") {
				t.Errorf("the losing host's retry is not visible:\n%s", stderr)
			}
		})
	}
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	stub(t, path, "#!/bin/sh\n"+text)
	return path
}

// A remote that never answers holds the backlog lock for the deadline and no
// longer, and the next allocation on the host takes the lock once it refuses.
func TestGitAllocatorHungRemoteRefusesAtTheDeadline(t *testing.T) {
	isolate(t)
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	t.Setenv("DROVER_TEST_SSH_PIDS", pids)
	t.Setenv("GIT_SSH_COMMAND", script(t, bin, "ssh", "echo $$ >> \"$DROVER_TEST_SSH_PIDS\"\nexec sleep 120\n"))
	t.Cleanup(func() {
		raw, _ := os.ReadFile(pids) // #nosec G304 -- the fixture's own pid file
		for _, f := range strings.Fields(string(raw)) {
			var pid int
			if _, err := fmt.Sscan(f, &pid); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	idDeadline = 2 * time.Second
	t.Cleanup(func() { idDeadline = 30 * time.Second; localAlloc = false })
	remote := "ssh://git@drover.invalid/ids.git"
	dir := idHost(t, remote)
	s := sessionAt(t, dir)

	mint := func(slug string) error {
		done := make(chan error, 1)
		go func() { _, _, err := s.mint("B", slug, "body\n", nil); done <- err }()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatalf("mint %s still waiting after 30s", slug)
			return nil
		}
	}
	var err error
	captureErr(t, func() { err = mint("hung") })
	if Code(err) != ExitRefused {
		t.Fatalf("exit = %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{remote, "2s", "--local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
	localAlloc = true
	if err := mint("after"); err != nil {
		t.Fatalf("the next mint: %v", err)
	}
	if got := stems(t, filepath.Join(dir, "work")); len(got) != 1 || got[0] != "B01" {
		t.Errorf("wrote %v, want [B01]", got)
	}
}

// No allocator git call can stop to ask. Each runs with terminal prompts off,
// and ssh in batch mode on top of whatever ssh the operator configured.
func TestGitAllocatorCommandsCannotPrompt(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, env, config, want string }{
		{"default", "", "", "ssh -o BatchMode=yes"},
		{"GIT_SSH_COMMAND", "ssh -i /k/id", "", "ssh -i /k/id -o BatchMode=yes"},
		{"core.sshCommand", "", "ssh -J jump", "ssh -J jump -o BatchMode=yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := bareRemote(t)
			dir := idHost(t, remote)
			if tc.config != "" {
				git(t, dir, "config", "core.sshCommand", tc.config)
			}
			t.Setenv("GIT_SSH_COMMAND", tc.env)
			t.Setenv("GIT_SSH", "")
			bin := t.TempDir()
			log := filepath.Join(bin, "log")
			t.Setenv("DROVER_TEST_GIT_LOG", log)
			script(t, bin, "git", fmt.Sprintf(`printf '%%s|%%s|%%s\n' "${GIT_TERMINAL_PROMPT-unset}" "${GIT_SSH_COMMAND-unset}" "$(echo "$*" | tr '\n' ' ')" >> "$DROVER_TEST_GIT_LOG"
exec %q "$@"
`, realGit))
			s := sessionAt(t, dir)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if _, _, err := s.mint("B", "quiet", "body\n", nil); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(log) // #nosec G304 -- the shim's own log
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			pushed := false
			for _, line := range lines {
				parts := strings.SplitN(line, "|", 3)
				// Reading core.sshCommand is how the ssh to extend is found, so
				// it alone runs before GIT_SSH_COMMAND is settled. It is local.
				lookup := strings.Contains(parts[2], " config --get core.sshCommand")
				if parts[0] != "0" || (parts[1] != tc.want && !lookup) {
					t.Errorf("GIT_TERMINAL_PROMPT=%s GIT_SSH_COMMAND=%q for git %s, want 0 and %q", parts[0], parts[1], parts[2], tc.want)
				}
				pushed = pushed || strings.Contains(parts[2], " push ")
			}
			if !pushed {
				t.Errorf("the shim saw no push:\n%s", raw)
			}
		})
	}
}

// refs/drover/ids carries no code, so the project's pre-push hook, often its
// test suite, has no business deciding whether an id can be issued.
func TestGitAllocatorSkipsPrePushHooks(t *testing.T) {
	remote := bareRemote(t)
	dir := idHost(t, remote)
	script(t, filepath.Join(dir, ".git", "hooks"), "pre-push", "exit 1\n")
	id, _, err := sessionAt(t, dir).mint("B", "hooked", "body\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.Bare() != "B1" {
		t.Errorf("allocated %s, want B1", id.Bare())
	}
}

// A remote that answers and refuses the ref needs a rule or a permission fixed,
// not a network, so the refusal quotes the remote and does not call it
// unreachable.
func TestGitAllocatorRejectedPushIsNotUnreachable(t *testing.T) {
	remote := bareRemote(t)
	dir := idHost(t, remote)
	script(t, filepath.Join(remote, "hooks"), "pre-receive", "cat >/dev/null\necho 'refs/drover/* is protected' >&2\nexit 1\n")
	_, _, err := sessionAt(t, dir).mint("B", "refused", "body\n", nil)
	if Code(err) != ExitRefused {
		t.Fatalf("exit = %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"refused the ref", "refs/drover/* is protected", "--local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("a rejected push reads as unreachable:\n%v", err)
	}
}
