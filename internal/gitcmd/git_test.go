package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The variables that move git's idea of which repository it is operating on come
// off. A command that names its own root with -C has to act on that root.
func TestRedirectingVariablesDoNotReachGit(t *testing.T) {
	for _, name := range Redirecting() {
		t.Setenv(name, "/somewhere/else")
	}
	cmd := Command("/project", "status")
	for _, kv := range cmd.Env {
		name, _, _ := strings.Cut(kv, "=")
		for _, dropped := range Redirecting() {
			if name == dropped {
				t.Errorf("%s reached git", name)
			}
		}
	}
}

// Everything else stays. Stripping the variables that authenticate or attribute
// would trade a redirect for a push that cannot log in.
func TestTheVariablesThatAuthenticateStay(t *testing.T) {
	keep := map[string]string{
		"GIT_AUTHOR_NAME":    "Sam",
		"GIT_COMMITTER_NAME": "Sam",
		"GIT_SSH_COMMAND":    "ssh -i /key",
		"GIT_CONFIG_GLOBAL":  "/dev/null",
	}
	for k, v := range keep {
		t.Setenv(k, v)
	}
	env := strings.Join(Command("/project", "status").Env, "\n")
	for k, v := range keep {
		if !strings.Contains(env, k+"="+v) {
			t.Errorf("%s did not reach git", k)
		}
	}
}

// The behavior the sanitizing exists for, driven against real repositories: an
// exported GIT_WORK_TREE would make an unsanitized command answer about the
// wrong tree.
func TestAnExportedWorkTreeDoesNotRedirectTheCommand(t *testing.T) {
	bin, err := exec.LookPath("git")
	if err != nil || bin == "" {
		t.Skip("no git on PATH")
	}
	here, elsewhere := t.TempDir(), t.TempDir()
	for _, dir := range []string{here, elsewhere} {
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("init %s: %v: %s", dir, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "stray.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_WORK_TREE", elsewhere)

	out, err := Command(here, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(string(out), "stray.txt") {
		t.Errorf("the command answered about the redirected tree:\n%s", out)
	}
	// And the raw form still does, which is what every unsanitized call site is.
	raw, err := exec.Command("git", "-C", here, "status", "--porcelain").Output() // #nosec G204 -- the control
	if err == nil && !strings.Contains(string(raw), "stray.txt") {
		t.Skip("this git does not honour GIT_WORK_TREE here; the control cannot run")
	}
}
