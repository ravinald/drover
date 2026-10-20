package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestArgvPlacesThePrompt(t *testing.T) {
	s := Spec{
		Interactive: []string{"claude"},
		Headless:    []string{"claude", "-p", "--output-format", "stream-json"},
	}
	if got := s.Argv(Interactive, "KICK"); got[len(got)-1] != "KICK" {
		t.Errorf("interactive: prompt not appended: %v", got)
	}
	got := s.Argv(Headless, "KICK")
	if got[0] != "claude" || got[len(got)-1] != "KICK" || len(got) != 5 {
		t.Errorf("headless: %v", got)
	}
}

// Argv must not append to the configured slice in place, or two calls would
// accumulate prompts onto the same backing array.
func TestArgvDoesNotMutateTheSpec(t *testing.T) {
	s := Spec{Headless: []string{"claude", "-p"}}
	_ = s.Argv(Headless, "first")
	second := s.Argv(Headless, "second")
	if len(s.Headless) != 2 {
		t.Fatalf("spec mutated: %v", s.Headless)
	}
	if second[len(second)-1] != "second" {
		t.Errorf("got %v", second)
	}
}

// A runner with a short argument limit takes the prompt on stdin instead.
func TestArgvOmitsThePromptWhenStdin(t *testing.T) {
	s := Spec{Headless: []string{"mytool"}, Stdin: true}
	got := s.Argv(Headless, "KICK")
	if len(got) != 1 || got[0] != "mytool" {
		t.Errorf("stdin runner must not carry the prompt in argv: %v", got)
	}
}

func TestBuiltinIsUsable(t *testing.T) {
	b := Builtin()
	c, ok := b["claude"]
	if !ok {
		t.Fatal("the built-in runner must be named claude")
	}
	if len(c.Interactive) == 0 || len(c.Headless) == 0 {
		t.Errorf("both modes need a command: %+v", c)
	}
}

func TestRenderFeedShowsProseAndToolMarkers(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the spec."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
		`{"type":"result","subtype":"success","result":"done"}`,
	}, "\n")

	var out strings.Builder
	if err := renderFeed(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Reading the spec.") {
		t.Errorf("assistant prose missing: %q", got)
	}
	if !strings.Contains(got, "·Read") {
		t.Errorf("tool marker missing: %q", got)
	}
	// The terminal result is the result's business, not the live feed's.
	if strings.Contains(got, "done") {
		t.Errorf("result text should not appear in the feed: %q", got)
	}
}

// A thinking block carries no text and no name; emitting it leaks a stray
// marker into the narrative.
func TestRenderFeedSkipsThinkingBlocks(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"thinking"},{"type":"text","text":"ok"}]}}`
	var out strings.Builder
	if err := renderFeed(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "ok" {
		t.Errorf("got %q, want just the text block", got)
	}
}

// A truncated final line is what a killed run leaves. The feed must render
// everything before it rather than failing.
func TestRenderFeedToleratesGarbage(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"before"}]}}` + "\n" +
		"not json at all\n" + `{"type":"assistant","message":{"conte`
	var out strings.Builder
	if err := renderFeed(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "before") {
		t.Errorf("output before the garbage was lost: %q", out.String())
	}
}

// The launched child has to see the marker, or nothing downstream can tell it
// apart from a person at a terminal. Asserted through a real exec, because the
// bug this catches is cmd.Env being assigned rather than appended, which drops
// PATH and every other inherited variable while still setting this one.
func TestRunExportsTheActorMarkerWithoutDroppingTheEnvironment(t *testing.T) {
	t.Setenv(EnvActor, "")
	t.Setenv("DROVER_TEST_CANARY", "kept")

	out := filepath.Join(t.TempDir(), "seen")
	script := filepath.Join(t.TempDir(), "runner.sh")
	body := "#!/bin/sh\nprintf '%s %s' \"$" + EnvActor + "\" \"$DROVER_TEST_CANARY\" > " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	spec := Spec{Interactive: []string{script}, Stdin: true}
	if _, err := Run(t.Context(), Where{}, spec, Interactive, "KICK", "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := ActorAgent + " kept"; string(got) != want {
		t.Errorf("child saw %q, want %q", got, want)
	}
}

// drive launches each session in the item's worktree, and the session finds
// its project through EnvRoot rather than by walking up from there.
func TestRunStartsInTheGivenDirectoryAndNamesTheRoot(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	out := filepath.Join(t.TempDir(), "seen")
	body := "#!/bin/sh\nprintf '%s\\n%s' \"$(pwd -P)\" \"$" + EnvRoot + "\" > " + out + "\n"
	spec := Spec{Interactive: []string{script(t, body)}, Stdin: true}
	if _, err := Run(t.Context(), Where{Dir: dir, Root: root}, spec, Interactive, "KICK", "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	wantDir, _ := filepath.EvalSymlinks(dir)
	if want := wantDir + "\n" + root; string(got) != want {
		t.Errorf("child saw %q, want %q", got, want)
	}
}

// Every package's TestMain clears SessionEnv and nothing else drover exports, so
// a variable Run sets that the list omits reaches every suite run from inside a
// session. The child here is env itself, so what it prints is exactly what Run
// handed it, with no shell adding PWD or SHLVL on the way.
func TestSessionEnvNamesEveryVariableALaunchSets(t *testing.T) {
	for _, name := range SessionEnv {
		t.Setenv(name, "")
	}
	parent := map[string]bool{}
	for _, line := range strings.Split(strings.Join(os.Environ(), "\n"), "\n") {
		parent[line] = true
	}
	listed := map[string]bool{}
	for _, name := range SessionEnv {
		listed[name] = true
	}

	logPath := filepath.Join(t.TempDir(), "env.log")
	spec := Spec{Headless: []string{"env"}, Stdin: true}
	at := Where{Dir: t.TempDir(), Root: t.TempDir()}
	if _, err := Run(t.Context(), at, spec, Headless, "KICK", logPath, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	set := 0
	for _, line := range strings.Split(strings.TrimSpace(string(seen)), "\n") {
		if parent[line] {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		set++
		if !listed[name] {
			t.Errorf("Run sets %s on a launched session and SessionEnv does not name it", name)
		}
	}
	if set == 0 {
		t.Fatal("the child saw nothing Run set; the comparison is not reading the launch")
	}
}

// The backstop. Each launch path checks before its first side effect, but a
// stage added later would inherit only this.
func TestRunRefusesInsideALaunchedSession(t *testing.T) {
	t.Setenv(EnvActor, ActorAgent)
	_, err := Run(t.Context(), Where{}, Spec{Interactive: []string{"sh", "-c", "true"}}, Interactive, "KICK", "", io.Discard)
	if !errors.Is(err, ErrNested) {
		t.Errorf("Run inside a launched session returned %v, want ErrNested", err)
	}
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { // #nosec G306 -- a fixture
		t.Fatal(err)
	}
	return p
}

// The failure this rework exists for. An agent that starts something of its own
// leaves a grandchild holding the write end of stdout, so reading the feed to
// EOF never returns even though the child is long gone. Before Wait bounded the
// read, this test hung rather than failed, which is why it has its own deadline.
func TestAGrandchildHoldingStdoutDoesNotHangTheLaunch(t *testing.T) {
	// The grandchild must outlive the deadline below, or both orderings pass and
	// the test measures nothing: reading the feed first blocks for exactly as
	// long as the grandchild holds the pipe, so a short one is indistinguishable
	// from a fixed one.
	dir := t.TempDir()
	bin := script(t, `
printf '{"type":"result","subtype":"success","is_error":false}\n'
sleep 120 &
echo $! > "`+filepath.Join(dir, "pid")+`"
exit 0
`)
	// Reap it rather than leaving it to expire. An orphan holding a pipe outlives
	// the test that made it and lands in whatever runs next, which showed up here
	// as the package failing once and passing on a rerun.
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(dir, "pid")) // #nosec G304 -- written above
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := Run(t.Context(), Where{}, Spec{Headless: []string{bin}}, Headless, "K",
			filepath.Join(t.TempDir(), "r.jsonl"), io.Discard)
		done <- err
	}()
	// The bound sits far from both edges: feedDrain below it, the grandchild's
	// two minutes above. Five seconds against an eight-second grandchild left
	// three of slack, and a host at load 50 spent them, so the margin is what
	// load cannot erase rather than what an idle host needs.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return while a grandchild held stdout")
	}
}

// A deadline has to reach a session already in flight, or --for bounds when work
// starts rather than how long it runs.
func TestACancelledContextEndsTheLaunch(t *testing.T) {
	bin := script(t, "sleep 60\n")
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := Run(ctx, Where{}, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(t.TempDir(), "r.jsonl"), io.Discard)
	if err == nil {
		t.Error("a cancelled launch returned no error")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("cancel took %s; the signal did not reach the child", d)
	}
}

// Run returned an error and nothing else, so a caller could not tell a runner
// that is absent from a session that failed, nor a session that finished from
// one that stopped having written no terminal event.
func TestTheOutcomeSaysMoreThanTheError(t *testing.T) {
	log := filepath.Join(t.TempDir(), "r.jsonl")
	bin := script(t, `printf '{"type":"result","subtype":"success","is_error":false,"num_turns":3}\n'`)
	oc, err := Run(t.Context(), Where{}, Spec{Headless: []string{bin}}, Headless, "K", log, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Exit != 0 || oc.LogPath != log || !oc.Logged || !oc.Result.Succeeded() {
		t.Errorf("a clean run reported %+v", oc)
	}

	// Exited zero, emitted nothing: died rather than finished, and only the log
	// separates the two.
	quiet := filepath.Join(t.TempDir(), "q.jsonl")
	oc, err = Run(t.Context(), Where{}, Spec{Headless: []string{script(t, "exit 0\n")}}, Headless, "K", quiet, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Exit != 0 || oc.Logged {
		t.Errorf("a silent run reported %+v, want exit 0 and no terminal event", oc)
	}

	// A failing child carries its own status rather than collapsing to 1.
	oc, _ = Run(t.Context(), Where{}, Spec{Headless: []string{script(t, "exit 7\n")}}, Headless, "K",
		filepath.Join(t.TempDir(), "f.jsonl"), io.Discard)
	if oc.Exit != 7 {
		t.Errorf("a child exiting 7 reported exit %d", oc.Exit)
	}
}

// The group is for unattended runs only. Setpgid puts the child in a group the
// controlling terminal does not consider foreground, so an interactive agent
// reading the tty takes SIGTTIN and stops before it draws anything, with the
// parent waiting on a process that will never exit. Asserted on the pgid rather
// than on the stop, because a test has no controlling terminal to be stopped by
// and would pass against the bug.
func TestOnlyHeadlessPutsTheChildInItsOwnGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a unix concept")
	}
	t.Setenv(EnvActor, "")

	pgidOf := func(mode Mode) int {
		t.Helper()
		out := filepath.Join(t.TempDir(), "pgid")
		bin := script(t, "ps -o pgid= -p $$ | tr -d ' ' > "+out+"\n")
		spec := Spec{Interactive: []string{bin}, Headless: []string{bin}, Stdin: true}
		if _, err := Run(t.Context(), Where{}, spec, mode, "K", filepath.Join(t.TempDir(), "log"), io.Discard); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("%s: child reported pgid %q: %v", mode, raw, err)
		}
		return n
	}

	mine, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got := pgidOf(Interactive); got != mine {
		t.Errorf("interactive child is in group %d, drover is in %d; a child outside the terminal's foreground group stops on its first read", got, mine)
	}
	if got := pgidOf(Headless); got == mine {
		t.Errorf("headless child shares drover's group %d, so its grandchildren survive a cancelled launch", got)
	}
}

// The defect this table pins cost two drives of this project: an agy headless
// argv copied from claude's shape, where -p is a boolean rather than a flag
// that eats the prompt. Nothing between the config and the launch said a word,
// and the empty log it left read as a session that died.
func TestFaultsJudgesAKnownBinarysArgv(t *testing.T) {
	for _, c := range []struct {
		name  string
		spec  Spec
		want  string // a phrase the reason must carry, empty for no fault
		fixed string // a phrase the fix must carry
	}{
		{
			name:  "agy with claude's ordering",
			spec:  Spec{Headless: []string{"agy", "-p", "--output-format", "stream-json"}},
			want:  `reads "--output-format" as the whole prompt`,
			fixed: "agy --output-format stream-json -p <prompt>",
		},
		{
			name: "agy reordered",
			spec: Spec{Headless: []string{"agy", "--output-format", "stream-json", "-p"}},
		},
		{
			name: "claude's own builtin",
			spec: Builtin()["claude"],
		},
		{
			name: "claude with no print flag",
			spec: Spec{Headless: []string{"claude", "--output-format", "stream-json"}},
			want: "runs interactively and writes no event stream",
		},
		{
			name: "agy asking for prose",
			spec: Spec{Headless: []string{"agy", "-p"}},
			want: "does not request --output-format stream-json",
		},
		{
			// The argv is wrong for agy and drover says nothing, because it
			// knows nothing about this binary. Its output is judged where the
			// output exists, which is the log drive reads at the end of a run.
			name: "a binary drover ships no knowledge of",
			spec: Spec{Headless: []string{"mycoder", "-p", "--output-format", "stream-json"}},
		},
		{
			// An absolute path resolves to the same family: [runners] is free to
			// pin a version, and the argv is wrong for the binary either way.
			name:  "a known binary named by path",
			spec:  Spec{Headless: []string{"/opt/bin/agy", "-p", "--output-format", "stream-json"}},
			want:  "as the whole prompt",
			fixed: "/opt/bin/agy --output-format stream-json -p",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Faults(c.spec)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("Faults = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("Faults = %+v, want one", got)
			}
			if !strings.Contains(got[0].Reason, c.want) {
				t.Errorf("reason = %q, want it to carry %q", got[0].Reason, c.want)
			}
			if got[0].Mode != Headless {
				t.Errorf("mode = %q, want %q", got[0].Mode, Headless)
			}
			if c.fixed != "" && !strings.Contains(got[0].Fix, c.fixed) {
				t.Errorf("fix = %q, want it to carry %q", got[0].Fix, c.fixed)
			}
		})
	}
}

// The feed is what an operator watches during a drive, and it read nothing at
// all from agy while the log filled up. Driven from the same captured stream
// runlog reads, rather than a second capture that could drift from it.
func TestRenderFeedReadsACapturedAgyStream(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "runlog", "testdata", "agy-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var out strings.Builder
	if err := renderFeed(f, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "hello") {
		t.Errorf("the agent's own text never reached the feed: %q", got)
	}
	if !strings.Contains(got, "·run_command") {
		t.Errorf("the tool call never reached the feed: %q", got)
	}
	// ACTIVE and DONE both name the tool, so keying on the step type alone
	// prints every call twice.
	if n := strings.Count(got, "·run_command"); n != 1 {
		t.Errorf("the tool call appears %d times, want 1: %q", n, got)
	}
}

// The feed read nothing at all from codex while the log filled up, same failure
// as agy's and found the same way: an operator watching a drive saw a blank
// screen for eleven minutes. Driven from the stream runlog reads, rather than a
// second capture that could drift from it.
func TestRenderFeedReadsACapturedCodexStream(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "runlog", "testdata", "codex-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var out strings.Builder
	if err := renderFeed(f, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "ok") {
		t.Errorf("the agent's own text never reached the feed: %q", got)
	}
	// codex reports a shortened-skill-descriptions notice as an error item, and
	// an operator watching wants to see it.
	if !strings.Contains(got, "Skill descriptions") {
		t.Errorf("the error item never reached the feed: %q", got)
	}
}

// One codex item arrives as started and again as completed, so a marker keyed
// on both prints every tool call twice.
func TestRenderFeedMarksACodexToolCallOnce(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"reasoning"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"reasoning"}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"done"}}`,
	}, "\n")

	var out strings.Builder
	if err := renderFeed(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "·exec"); n != 1 {
		t.Errorf("the tool call appears %d times, want 1: %q", n, got)
	}
	// A thinking block carries no text and no name; marking it leaks a stray
	// marker into the narrative.
	if strings.Contains(got, "·reasoning") || strings.Contains(got, "· ") {
		t.Errorf("a reasoning item was marked: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("the agent message never reached the feed: %q", got)
	}
}

// codex reads stdin for additional input and waits for an EOF a terminal never
// sends, so a headless session inheriting it hangs before its first event: a
// drive that launched nothing and said nothing. Proved with a runner that
// drains stdin before it writes its terminal event, against an os.Stdin that
// stays open.
func TestHeadlessDoesNotInheritStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close(); _ = r.Close() }()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved })

	s := script(t, "cat > /dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	oc, err := Run(ctx, Where{}, Spec{Headless: []string{s}}, Headless,
		"KICK", filepath.Join(t.TempDir(), "log.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !oc.Logged {
		t.Error("the runner never reached its terminal event: it was still waiting on a stdin that never closed")
	}
}

// Interactive keeps the terminal, because that is the whole of an attended
// session: a TUI reading from the null device draws once and exits.
func TestInteractiveStillInheritsStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved })
	if _, err := w.WriteString("typed\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	out := filepath.Join(t.TempDir(), "seen")
	s := script(t, "cat > "+out+"\n")
	if _, err := Run(t.Context(), Where{}, Spec{Interactive: []string{s}}, Interactive, "KICK", "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "typed" {
		t.Errorf("the child read %q from stdin, want what the operator typed", got)
	}
}

// A child that writes its terminal event and exits at once leaves the reader
// racing whoever closes the pipe. Under cmd.StdoutPipe, Wait closes it the
// moment the process is gone and the buffered bytes are never copied, so a
// session that finished cleanly comes back Logged:false with no events: in
// drive that is a park reading "the review left no terminal event in its log".
//
// The reader is held back deliberately. Left to the scheduler this passes
// against the broken ordering too, on an idle machine, while a loaded CI runner
// fails it. A delay makes the case the scheduler picks at random the case every
// run takes.
func TestAChildThatExitsImmediatelyStillLandsInTheLog(t *testing.T) {
	restore := feedReady
	t.Cleanup(func() { feedReady = restore })
	feedReady = func() { time.Sleep(20 * time.Millisecond) }

	bin := script(t, `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":3}'`+"\n")
	dir := t.TempDir()
	for i := range 20 {
		log := filepath.Join(dir, fmt.Sprintf("r%03d.jsonl", i))
		oc, err := Run(t.Context(), Where{}, Spec{Headless: []string{bin}}, Headless, "K", log, io.Discard)
		if err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
		if !oc.Logged || oc.Events != 1 {
			b, _ := os.ReadFile(log) // #nosec G304 -- written by this test
			t.Fatalf("launch %d came back Logged=%v Events=%d; the log holds %q",
				i, oc.Logged, oc.Events, b)
		}
		if !oc.Result.Succeeded() {
			t.Fatalf("launch %d: %+v", i, oc.Result)
		}
	}
}

// The other half of the same contract: what the child wrote is in the log even
// when a grandchild keeps the write end open, rather than being traded away for
// the launch returning promptly.
func TestAGrandchildDoesNotCostTheChildsOutput(t *testing.T) {
	dir := t.TempDir()
	bin := script(t, `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":7}'
sh -c 'echo $$ > `+dir+`/pid; sleep 30' &
exit 0
`)
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(dir, "pid")) // #nosec G304 -- written above
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	oc, err := Run(t.Context(), Where{}, Spec{Headless: []string{bin}}, Headless, "K",
		filepath.Join(dir, "r.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !oc.Logged || !oc.Result.Succeeded() || oc.Result.NumTurns != 7 {
		t.Errorf("a grandchild cost the child's own terminal event: %+v logged=%v", oc.Result, oc.Logged)
	}
}
