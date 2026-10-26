package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeMarker(t *testing.T, dir string, m Marker) string {
	t.Helper()
	path := MarkerPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deadPID is the pid of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// The isolation TestMain sets up is only real if the walk would have read a
// marker sitting above the working directory. This puts one there.
func TestAMarkerAboveTheWorkingDirectorySeals(t *testing.T) {
	if Launched() || Session() != "" {
		t.Fatal("the suite reads as launched before any marker was written: TestMain's isolation is not holding")
	}
	top := t.TempDir()
	writeMarker(t, top, Marker{Actor: ActorAgent, Session: "s-above", Root: "/r", PID: os.Getpid()})
	below := filepath.Join(top, "a", "b")
	if err := os.MkdirAll(below, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(below)
	if !Launched() || Supervising() || Session() != "s-above" {
		t.Errorf("under a marker: Launched=%v Supervising=%v Session=%q, want true false s-above",
			Launched(), Supervising(), Session())
	}

	writeMarker(t, top, Marker{Actor: ActorSupervisor, Session: "s-sup", PID: os.Getpid()})
	if !Launched() || !Supervising() {
		t.Errorf("under a supervisor marker: Launched=%v Supervising=%v", Launched(), Supervising())
	}

	// The environment wins over the file.
	t.Setenv(EnvActor, ActorAgent)
	t.Setenv(EnvSession, "s-env")
	if Supervising() || Session() != "s-env" {
		t.Errorf("with %s set: Supervising=%v Session=%q, want the environment's", EnvActor, Supervising(), Session())
	}

	t.Setenv(EnvActor, ActorOperator)
	if Launched() || Supervising() || Session() != "" {
		t.Errorf("as %s under a marker: Launched=%v Supervising=%v Session=%q, want a person",
			ActorOperator, Launched(), Supervising(), Session())
	}
}

// A project checked out inside another one is its own project, whatever the
// outer tree's session is doing.
func TestTheWalkStopsAtAProjectRoot(t *testing.T) {
	top := t.TempDir()
	writeMarker(t, top, Marker{Actor: ActorAgent, Session: "outer", PID: os.Getpid()})
	inner := filepath.Join(top, "vendor", "other")
	if err := os.MkdirAll(filepath.Join(inner, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, ".drover.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(inner, "pkg"))
	if Launched() {
		t.Error("a marker above a .drover.toml sealed the project below it")
	}
	if _, _, ok := FindMarker("."); ok {
		t.Error("FindMarker walked past a .drover.toml")
	}
}

// Run leaves the three values it exports in the tree, and takes them away
// again when the session ends.
func TestRunWritesTheMarkerForTheSessionAndRemovesIt(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	bin := script(t, "cat .drover/session > '"+seen+"'\n")
	at := Where{Dir: dir, Root: "/the/root", Session: "s-run"}
	if _, err := Run(t.Context(), at, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(seen) // #nosec G304 -- written above
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	var m Marker
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("the session read %q from its marker: %v", line, err)
	}
	if m.Actor != ActorAgent || m.Session != "s-run" || m.Root != "/the/root" || m.PID != os.Getpid() || m.Started.IsZero() {
		t.Errorf("marker = %+v, want agent, s-run, /the/root, pid %d and a start time", m, os.Getpid())
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(line), &raw)
	if s, _ := raw["started"].(string); s == "" {
		t.Errorf("started = %v, want an RFC 3339 string", raw["started"])
	} else if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("started = %q: %v", s, err)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after the session the tree still holds %s (%v)", MarkerDir, err)
	}
}

// An agent that stages everything must not commit the seal along with its work.
func TestTheMarkerIsInvisibleToGitStatus(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	bin := script(t, "test -f .drover/session || echo missing > '"+seen+"'\ngit status --porcelain >> '"+seen+"'\n")
	if _, err := Run(t.Context(), Where{Dir: dir}, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b, _ := os.ReadFile(seen); len(b) != 0 { // #nosec G304 -- written above
		t.Errorf("inside the session git status printed %q, want nothing", b)
	}
}

// A directory the project already uses keeps everything that was in it.
func TestRunLeavesAnExistingDroverDirectoryAsItFoundIt(t *testing.T) {
	dir := t.TempDir()
	backlog := filepath.Join(dir, MarkerDir, "backlog")
	if err := os.MkdirAll(backlog, 0o750); err != nil {
		t.Fatal(err)
	}
	ignore := filepath.Join(dir, MarkerDir, ".gitignore")
	if err := os.WriteFile(ignore, []byte("ledger.jsonl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), Where{Dir: dir}, Spec{Interactive: []string{"sh", "-c", "test -f .drover/session"}},
		Interactive, "K", "", io.Discard); err != nil {
		t.Fatalf("Run: %v (the interactive session saw no marker)", err)
	}
	if b, _ := os.ReadFile(ignore); string(b) != "ledger.jsonl\n" { // #nosec G304 -- written above
		t.Errorf("the project's own .gitignore became %q", b)
	}
	if _, err := os.Stat(backlog); err != nil {
		t.Errorf("the backlog directory went with the marker: %v", err)
	}
	if _, err := os.Stat(MarkerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker outlived its session: %v", err)
	}
}

// A cancel is the path a SIGINT or SIGTERM to drover takes, and the one that
// most needs the marker gone, because the tree outlives the drive.
func TestACancelledLaunchRemovesItsMarker(t *testing.T) {
	dir := t.TempDir()
	bin := script(t, "sleep 60\n")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for range 200 {
			if _, err := os.Stat(MarkerPath(dir)); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if _, err := Run(ctx, Where{Dir: dir}, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard); err == nil {
		t.Fatal("a cancelled launch returned no error")
	}
	if _, err := os.Stat(MarkerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cancelled launch left its marker: %v", err)
	}
}

// Two sessions in one tree write over each other's work, and the second one
// is the one that has to be told.
func TestALiveMarkerRefusesTheLaunch(t *testing.T) {
	dir := t.TempDir()
	path := writeMarker(t, dir, Marker{Actor: ActorAgent, Session: "s-holder", PID: os.Getpid()})
	ran := filepath.Join(t.TempDir(), "ran")
	_, err := Run(t.Context(), Where{Dir: dir}, Spec{Headless: []string{script(t, "touch '"+ran+"'\n")}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard)
	if !errors.Is(err, ErrOccupied) {
		t.Fatalf("Run into a held tree returned %v, want ErrOccupied", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "s-holder") {
		t.Errorf("the refusal %q names neither the marker nor its session", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the session started anyway")
	}
	if m, err := ReadMarker(path); err != nil || m.Session != "s-holder" {
		t.Errorf("the holder's marker became %+v (%v)", m, err)
	}
}

// A marker whose drover is gone holds nothing.
func TestAStaleMarkerIsReplaced(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, Marker{Actor: ActorAgent, Session: "s-dead", PID: deadPID(t)})
	seen := filepath.Join(t.TempDir(), "seen")
	bin := script(t, "cat .drover/session > '"+seen+"'\n")
	if _, err := Run(t.Context(), Where{Dir: dir, Session: "s-new"}, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard); err != nil {
		t.Fatalf("Run over a stale marker: %v", err)
	}
	b, _ := os.ReadFile(seen) // #nosec G304 -- written above
	if !strings.Contains(string(b), `"s-new"`) {
		t.Errorf("the session read %q, want its own marker", b)
	}
	if _, err := os.Stat(MarkerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the replacement outlived its session: %v", err)
	}
}
