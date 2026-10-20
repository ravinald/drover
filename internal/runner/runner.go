// Package runner executes a prompt against a coding agent.
//
// A runner is a whole binary, not a flag: swapping runners swaps how the work
// executes. Projects declare them in config, so adding one needs no change here.
package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ravinald/drover/internal/runlog"
)

// EnvActor is exported into every session drover launches, so a command run
// inside one can tell that it is not being typed by a person. Only drover sets
// it: to ActorAgent for every session a run, review, draft or drive launches,
// and to ActorSupervisor for the one `drover supervise` starts. Any value at
// all means "launched".
//
// It is a guardrail, not a boundary. `env -u DROVER_ACTOR` defeats it, and that
// is fine — the failure it exists to stop is an agent that reads a fenced
// command out of AGENTS.md and helpfully runs it, not one that sets out to lie
// about who it is. Stripping the variable is a deliberate act, and a deliberate
// act shows up in a transcript as intent.
const (
	EnvActor   = "DROVER_ACTOR"
	ActorAgent = "agent"
	// ActorSupervisor is the session that watches a drive for the operator.
	// It may start a drive, which launches agents, and it may never be one of
	// the sessions a drive launches: those are ActorAgent whatever their parent.
	ActorSupervisor = "supervisor"
)

// EnvRoot names the project a launched session belongs to. config.Find reads it
// before walking up, because a session drive starts in a worktree of its own
// would otherwise find that worktree's tracked .drover.toml, or none at all when
// the config lives outside version control, and write its ledger rows and
// requests for changes into a tree nobody reads them from.
const EnvRoot = "DROVER_ROOT"

// EnvSession carries a launch's own id, the one the stage row drover wrote for
// it records. It is what lets `audit --code-by agent` tell the session that
// wrote an item's code from a later session reviewing it: both carry EnvActor,
// so the actor alone cannot.
const EnvSession = "DROVER_SESSION"

// Session is the id this process was launched under, empty outside a session
// drover started.
func Session() string { return os.Getenv(EnvSession) }

// NewSession mints a launch id. It names one launch and nothing else, so a
// random value serves and a counter would need a store to survive restarts.
func NewSession() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SessionEnv names every variable Run sets on a launched session. A test suite
// started from inside a session inherits all of them, and each one points a
// command somewhere no test chose: EnvRoot at the operator's live project,
// EnvActor at the seal. So every package whose tests run drover clears this list
// in its TestMain, and TestSessionEnvNamesEveryVariableALaunchSets fails when
// Run grows a variable the list does not carry.
var SessionEnv = []string{EnvActor, EnvRoot, EnvSession}

// Where is the directory a session runs in and the project it reports to. The
// zero value inherits drover's own working directory and sets no root, which is
// what a launch outside any project needs.
type Where struct {
	Dir  string
	Root string
	// Actor is the EnvActor value the session gets, ActorAgent when empty.
	// Only `drover supervise` sets it.
	Actor string
	// Session is the launch id the stage row recorded, minted fresh when empty
	// so that every launched session carries one.
	Session string
}

// Launched reports whether this process is running inside a session drover
// started. Commands that belong to the operator read it to refuse.
func Launched() bool { return os.Getenv(EnvActor) != "" }

// Supervising reports whether this process runs inside the session `drover
// supervise` started.
func Supervising() bool { return os.Getenv(EnvActor) == ActorSupervisor }

// Mode is how a run is executed.
type Mode string

const (
	// Interactive hands the prompt to a session the operator drives.
	Interactive Mode = "interactive"
	// Headless runs unattended and writes an event stream to a log.
	Headless Mode = "headless"
)

// Spec is one runner's invocation, as declared by a project.
type Spec struct {
	Interactive []string `toml:"interactive"`
	Headless    []string `toml:"headless"`
	// Stdin passes the prompt on standard input instead of as the final
	// argument. Worth setting for a runner with a short argument limit.
	Stdin bool `toml:"stdin"`
}

// Builtin is the runner assumed when a project declares none.
func Builtin() map[string]Spec {
	return map[string]Spec{
		"claude": {
			Interactive: []string{"claude"},
			Headless:    []string{"claude", "-p", "--output-format", "stream-json", "--verbose"},
		},
	}
}

// Argv is the command for a mode, with the prompt placed as configured.
func (s Spec) Argv(mode Mode, prompt string) []string {
	base := s.Interactive
	if mode == Headless {
		base = s.Headless
	}
	argv := make([]string, len(base))
	copy(argv, base)
	if !s.Stdin {
		argv = append(argv, prompt)
	}
	return argv
}

// Binary is argv[0] for a mode, before PATH resolution, or "" when the mode
// declares no command.
func (s Spec) Binary(mode Mode) string {
	base := s.Interactive
	if mode == Headless {
		base = s.Headless
	}
	if len(base) == 0 {
		return ""
	}
	return base[0]
}

// Display is the argv for a mode on one line, with the prompt shown as a
// placeholder rather than inlined: the prompt is the whole item body, and it
// would bury the binary the line exists to name.
func (s Spec) Display(mode Mode) string {
	argv := s.Argv(mode, "<prompt>")
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t'\"") {
			a = strconv.Quote(a)
		}
		quoted[i] = a
	}
	line := strings.Join(quoted, " ")
	if s.Stdin {
		line += " (prompt on stdin)"
	}
	return line
}

// Fault is one thing wrong with a declared runner that drover can see without
// launching it. Reason is the sentence an operator reads, and Fix the argv that
// works.
type Fault struct {
	Mode   Mode
	Reason string
	Fix    string
}

// family is what drover knows about one runner binary: how it is put into
// non-interactive mode, and whether that flag eats the next argument.
type family struct {
	print  []string
	prompt bool
}

// families is the binaries drover ships knowledge of, by argv[0]'s basename.
//
// Faults judges a declared argv against this table and says nothing about a
// binary absent from it. Whether an unknown binary's output is readable is a
// question only its output answers, and drover has that output the moment a run
// ends: drive reports it there, by runner and by log path.
func families() map[string]family {
	return map[string]family{
		"claude": {print: []string{"-p", "--print"}},
		// agy's -p is an alias for --print and takes the prompt as its value,
		// which claude's boolean -p does not. An argv copied from claude's shape
		// therefore hands agy "--output-format" as the entire prompt and leaves
		// the prompt as a positional it ignores, and a drive reads the empty log
		// as a dead session.
		"agy": {print: []string{"-p", "--print", "--prompt"}, prompt: true},
	}
}

// Faults reports what is wrong with a runner's headless argv, for a binary
// drover knows. An empty slice is every check passing, not every check running.
func Faults(spec Spec) []Fault {
	argv := spec.Headless
	if len(argv) == 0 {
		return nil
	}
	fam, known := families()[filepath.Base(argv[0])]
	if !known {
		return nil
	}

	var out []Fault
	at := -1
	for i, a := range argv {
		if slices.Contains(fam.print, a) {
			at = i
		}
	}
	switch {
	case at < 0:
		out = append(out, Fault{Mode: Headless, Reason: fmt.Sprintf(
			"%s declares none of %s, so it runs interactively and writes no event stream",
			argv[0], strings.Join(fam.print, ", ")),
			Fix: "add the print flag this binary reads"})
	case fam.prompt && !spec.Stdin && at != len(argv)-1:
		reordered := slices.Concat(argv[:at], argv[at+1:], []string{argv[at]})
		out = append(out, Fault{Mode: Headless, Reason: fmt.Sprintf(
			"%s takes the next argument as its prompt, so %s reads %q as the whole prompt and ignores the prompt appended after it",
			argv[at], argv[0], argv[at+1]),
			Fix: "move it last: " + Spec{Headless: reordered}.Display(Headless)})
	}

	if !streamJSON(argv) {
		out = append(out, Fault{Mode: Headless, Reason: fmt.Sprintf(
			"%s does not request --output-format stream-json, so it writes prose and drover reads no terminal event from the log",
			argv[0]),
			Fix: `add --output-format stream-json`})
	}
	return out
}

// signIn is what drover knows about a runner's credentials, by argv[0]'s
// basename: the arguments that ask whether it holds any, where the binary
// offers such a question, and what a person runs to give it some.
type signIn struct {
	check []string
	login string
}

func signIns() map[string]signIn {
	return map[string]signIn{
		"claude": {check: []string{"auth", "status"}, login: "claude auth login"},
		"codex":  {check: []string{"login", "status"}, login: "codex login"},
	}
}

// binary is the headless argv[0], else the interactive one.
func (s Spec) binary() string {
	if b := s.Binary(Headless); b != "" {
		return b
	}
	return s.Binary(Interactive)
}

// AuthCheck is the argv that asks the runner whether it is signed in, exiting
// non-zero when it is not, and nil for a binary drover knows no such question
// for. argv[0] is the declared one, so a runner named by path is asked at that
// path.
func AuthCheck(spec Spec) []string {
	bin := spec.binary()
	si, ok := signIns()[filepath.Base(bin)]
	if bin == "" || !ok {
		return nil
	}
	return append([]string{bin}, si.check...)
}

// Login is the command that signs the runner in, for a person to type. A
// binary drover knows nothing about gets a sentence naming it rather than a
// guessed flag.
func Login(spec Spec) string {
	bin := spec.binary()
	if si, ok := signIns()[filepath.Base(bin)]; ok {
		return si.login
	}
	if bin == "" {
		return "sign the runner in again"
	}
	return "sign " + filepath.Base(bin) + " in again"
}

// streamJSON reports an argv that asks for the JSONL event stream, in either
// spelling a flag parser accepts.
func streamJSON(argv []string) bool {
	for i, a := range argv {
		if a == "--output-format=stream-json" {
			return true
		}
		if a == "--output-format" && i+1 < len(argv) && argv[i+1] == "stream-json" {
			return true
		}
	}
	return false
}

// ErrNested is the refusal for a launch attempted from inside a launched
// session. cmd wraps it so the process exits 3.
var ErrNested = errors.New(
	"this session was launched by drover, and a session cannot launch another.\n" +
		"Report what you found and let the operator start the next run")

// Outcome is what one launch amounted to, beyond whether it errored.
//
// A caller driving more than one launch needs all of them. The exit status alone
// cannot separate a runner that is absent from a session that failed; and a
// session that exits 0 having emitted no terminal event died rather than
// finished, which only the log can say. Run returned an error and nothing else
// until a loop had to decide whether to keep going.
type Outcome struct {
	// Exit is the child's status, and -1 when no child ran or a signal took it.
	Exit int
	// LogPath is where the stream landed, empty outside headless mode.
	LogPath string
	// Result is the terminal event the log carries, and Logged whether one was
	// found at all. Logged false with Exit 0 is the died-quietly case.
	Result runlog.Result
	Logged bool
	// Events is how many events the log carried and Known how many of them a
	// runlog schema claims. Logged false with Events above zero and Known zero
	// is a runner whose output drover cannot read, which is a different report
	// from a session that wrote nothing and a different fix.
	Events int
	Known  int
	// Signal is the signal that ended the child, zero when it exited on its
	// own or none ran.
	Signal int
	// LeftRunning is each process the session started and left alive when it
	// exited, which drover killed. A session that backgrounds its gate and
	// exits reports on a check that had not finished.
	LeftRunning []string
}

// Resolvable is the launch's own refusal for an argv[0] that names nothing
// runnable, so a caller checking ahead of the launch says what the launch would.
// A name with a path separator never goes through PATH, and the sentence says so.
func Resolvable(bin string) error {
	if _, err := exec.LookPath(bin); err != nil {
		if strings.ContainsRune(bin, '/') || strings.ContainsRune(bin, filepath.Separator) {
			return fmt.Errorf("runner binary %q not found", bin)
		}
		return fmt.Errorf("runner binary %q not found on PATH", bin)
	}
	return nil
}

// Run executes the runner and waits for it. In headless mode the raw event
// stream is written to logPath while a readable feed goes to out; the log is the
// replayable copy and the feed is for the human watching.
//
// ctx cancels the launch: the child's whole process group is signalled, then
// killed if it has not gone within WaitDelay. Without that a caller's deadline
// cannot interrupt a session already in flight, which is the difference between
// a bound on when work starts and a bound on how long it runs.
func Run(ctx context.Context, at Where, spec Spec, mode Mode, prompt, logPath string, out io.Writer) (Outcome, error) {
	oc := Outcome{Exit: -1}
	argv := spec.Argv(mode, prompt)
	if len(argv) == 0 {
		return oc, fmt.Errorf("runner has no command for mode %s", mode)
	}
	if err := Resolvable(argv[0]); err != nil {
		return oc, err
	}
	// One launch per launch. A session that spawns another spends a second one
	// nobody asked for, and the nesting is invisible from outside: the operator
	// sees one run and pays for two. Caught here rather than per command,
	// because [run] draft_runner launches with no flag to key on.
	//
	// The supervisor is the one exception, for the headless sessions of the
	// drive it may start: noNestedLaunch has already refused it every other
	// launch path, and it may never start a second supervisor or anything
	// interactive.
	actor := at.Actor
	if actor == "" {
		actor = ActorAgent
	}
	if Launched() && (!Supervising() || actor == ActorSupervisor || mode != Headless) {
		return oc, ErrNested
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv is project config, not user input
	if mode == Headless {
		// Only unattended. A child in its own group is a background group as
		// far as the controlling terminal is concerned, so the first tty read
		// raises SIGTTIN and stops it: an interactive agent is suspended before
		// it draws anything and Wait never returns. Headless has no terminal to
		// lose and does need the group, because that is what makes an agent's
		// own grandchildren killable on a deadline.
		setGroup(cmd)
	}
	// Cancel signals the group rather than the process, and WaitDelay is what
	// stops a child that ignores SIGTERM from holding the caller open forever.
	cmd.Cancel = func() error { return killGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	// Inherited, plus the marker. Set here because this is the only place drover
	// starts a coding agent, so no launch path can forget it.
	session := at.Session
	if session == "" {
		session = NewSession()
	}
	cmd.Env = append(os.Environ(), EnvActor+"="+actor, EnvSession+"="+session)
	if at.Root != "" {
		cmd.Env = append(cmd.Env, EnvRoot+"="+at.Root)
	}
	cmd.Dir = at.Dir
	cmd.Stderr = os.Stderr
	switch {
	case spec.Stdin:
		cmd.Stdin = strings.NewReader(prompt)
	case mode == Headless:
		// nil is the null device. Inherited instead, codex announces "Reading
		// additional input from stdin..." and blocks for an EOF a terminal
		// never sends, so the session hangs before its first event and the
		// operator sees a drive that started nothing. Held open on a fifo it
		// emitted no events in 25 seconds; on /dev/null it runs.
		cmd.Stdin = nil
	default:
		cmd.Stdin = os.Stdin
	}

	if mode != Headless {
		cmd.Stdout = os.Stdout
		err := cmd.Run()
		oc.Exit = cmd.ProcessState.ExitCode()
		return oc, err
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return oc, fmt.Errorf("create log directory: %w", err)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return oc, fmt.Errorf("create log %s: %w", logPath, err)
	}
	defer func() { _ = log.Close() }()
	oc.LogPath = logPath

	// The pipe is drover's own rather than cmd.StdoutPipe's, because Wait closes
	// the one it made as soon as the process exits. os/exec says so plainly:
	// "it is incorrect to call Wait before all reads from the pipe have
	// completed." Whatever the reader had not drained by then was never copied
	// to the log, the read returned "file already closed", and a session that
	// finished cleanly came back with no events at all. On a loaded box that is
	// a park with no cause, and three CI runs in one evening hit it.
	//
	// Nothing here closes what the child writes to, so the reader ends on EOF:
	// the parent drops its own copy of the write end below, and the last
	// descriptor goes when the child and any grandchildren exit.
	pr, pw, err := os.Pipe()
	if err != nil {
		return oc, err
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return oc, err
	}
	// The parent's copy, the moment the child has one of its own. Held open, it
	// is a writer that never writes and the reader below never sees EOF.
	_ = pw.Close()

	// Tee to the log first so an interrupted run still leaves everything the
	// agent emitted before it died. The rendered feed is derived and lossy.
	feed := make(chan error, 1)
	go func() {
		feedReady()
		feed <- renderFeed(io.TeeReader(pr, log), out)
	}()
	waitErr := cmd.Wait()

	// Wait returns when the process is gone, which is not when the stream ends:
	// an agent that started something of its own leaves a grandchild holding
	// the write end, and a read to EOF would wait on it forever. So the drain
	// is bounded rather than unbounded, and closing the read end is what ends
	// it. Every byte already written is in the pipe buffer and readable, which
	// closing the pipe before the reader ran could not offer.
	feedErr := drain(feed, pr)
	oc.Exit, oc.Signal = cmd.ProcessState.ExitCode(), exitSignal(cmd.ProcessState)
	// Not on a cancelled launch: Cancel signalled the whole group, and what is
	// still dying there was stopped by drover rather than left by the session.
	if ctx.Err() == nil {
		oc.LeftRunning = leftRunning(cmd.Process.Pid)
	}
	// Read the log back rather than the stream: renderFeed is lossy by design,
	// and the terminal event is the only thing that separates a session that
	// finished from one that stopped.
	if rd, err := runlog.ReadResult(logPath); err == nil {
		oc.Result, oc.Logged, oc.Events, oc.Known = rd.Result, rd.Found, rd.Events, rd.Known
	}
	if waitErr != nil {
		return oc, waitErr
	}
	return oc, feedErr
}

// lingerGrace is how long a group may outlive its leader before what is left
// counts as left running, and lingerPoll how often it is asked. Vars so a test
// can drive the wait in milliseconds; nothing else writes them.
var (
	lingerGrace = 2 * time.Second
	lingerPoll  = 50 * time.Millisecond
)

// feedReady is a seam, and the only reason it exists is that the race this
// ordering prevents cannot be reproduced without one. Output is lost when the
// reader has not been scheduled before Wait closes the pipe, which a loaded CI
// box does and an idle machine almost never does. A test that delays this
// reproduces it every time, on any machine.
var feedReady = func() {}

// feedDrain is how long the reader has once the child is gone, and it is a
// scheduling grace rather than a bound on the stream. The child has stopped
// writing by then, so everything it produced is sitting in the pipe buffer and
// one read empties it; the failure to avoid is closing before the goroutine
// has run at all, which a loaded box makes likely and an idle one
// almost never does. A second is thousands of times the copy and is reached
// only when a grandchild holds the write end open, which is the case this
// cannot wait out and must not hang on.
const feedDrain = time.Second

// drain waits for the reader, then takes the pipe out from under it.
//
// The close is how a grandchild's inherited write end stops being the caller's
// problem. It surfaces as "file already closed", which is the end of the read
// rather than a fault, so it is dropped: everything the child wrote reached the
// log before the grandchild kept the descriptor open.
func drain(feed <-chan error, pr *os.File) error {
	var err error
	select {
	case err = <-feed:
	case <-time.After(feedDrain):
		_ = pr.Close()
		err = <-feed
	}
	_ = pr.Close()
	if errors.Is(err, fs.ErrClosed) {
		return nil
	}
	return err
}

// renderFeed turns a stream-json event stream into readable prose: assistant
// text as it arrives, tool calls as inline markers. Thinking blocks are skipped.
//
// Doing this in Go rather than shelling to jq removes the last hard dependency
// from the run path — the shell driver fell back to dumping raw JSON without it.
//
// One decode covers every event shape rather than a pass each: claude nests
// content blocks under "message", agy carries one step's delta under
// "step_update", codex names an item under "item". runlog.Schemas is the other
// half of this and reads the terminal event the same way. A runner writing none
// of these shapes runs with a silent feed, and the log says so when it ends.
// codexMarker is the inline name for one of codex's item types, and empty for
// an item the feed does not mark: reasoning is a thinking block, and a message
// prints as prose when it completes rather than as a marker when it starts.
func codexMarker(item string) string {
	switch item {
	case "command_execution":
		return "exec"
	case "file_change":
		return "edit"
	case "mcp_tool_call":
		return "mcp"
	case "collab_tool_call":
		return "collab"
	case "web_search":
		return "search"
	case "todo_list":
		return "todo"
	}
	return ""
}

func renderFeed(r io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
			Event string `json:"event"`
			Step  struct {
				Type  string `json:"step_type"`
				State string `json:"state"`
				Text  string `json:"text_delta"`
				Tool  string `json:"tool_name"`
			} `json:"step_update"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
				// An error item states itself under "message" where every
				// other codex item uses "text".
				Message string `json:"message"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch {
		case ev.Type == "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					_, _ = fmt.Fprintf(out, "\n%s", c.Text)
				case "tool_use":
					_, _ = fmt.Fprintf(out, "·%s ", c.Name)
				}
			}
		case ev.Event == "step_update":
			// A tool step arrives twice, ACTIVE then DONE, so the marker keys
			// on the first: keying on the type alone prints every call twice.
			switch {
			case ev.Step.Type == "agent_response" && ev.Step.Text != "":
				_, _ = fmt.Fprintf(out, "%s", ev.Step.Text)
			case ev.Step.Type == "tool" && ev.Step.State == "ACTIVE":
				_, _ = fmt.Fprintf(out, "·%s ", ev.Step.Tool)
			}
		case ev.Type == "item.started":
			// codex emits started and completed for one item, so the marker
			// keys on started alone and the text on completed alone. Both
			// would print every tool call twice and every message twice.
			if m := codexMarker(ev.Item.Type); m != "" {
				_, _ = fmt.Fprintf(out, "·%s ", m)
			}
		case ev.Type == "item.completed":
			switch ev.Item.Type {
			case "agent_message":
				_, _ = fmt.Fprintf(out, "\n%s", ev.Item.Text)
			case "error":
				_, _ = fmt.Fprintf(out, "\n! %s", ev.Item.Message)
			}
		}
	}
	_, _ = fmt.Fprintln(out)
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event stream: %w", err)
	}
	return nil
}
