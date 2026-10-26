package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
	"github.com/ravinald/drover/internal/t3"
	"github.com/ravinald/drover/internal/t3/t3test"
)

// t3Host is a T3 Code host for one test: the recorded server, its provider log
// directory, a per-user [t3] pointing at both with a grant, and drover on PATH
// as this test binary, so runner.Run execs the bridge as a process of its own.
func t3Host(t *testing.T) (*t3test.Server, string) {
	t.Helper()
	root, logs, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	srv := t3test.New(root, logs)
	t.Cleanup(srv.Close)
	token := filepath.Join(xdg, "t3-token")
	if err := t3.WriteGrant(token, t3.Grant{URL: srv.MCP, AccessToken: "test-token"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "drover"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(xdg, "drover", "config.toml"), fmt.Sprintf(
		"[t3]\nurl = %q\ntoken_file = %q\nproject = \"drover\"\npoll = \"20ms\"\nprovider_logs = %q\n", srv.MCP, token, logs))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	bin := t.TempDir()
	asDrover(t, filepath.Join(bin, "drover"), 1)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(envAsDrover, "1")
	return srv, root
}

func t3Spec(stdin bool) runner.Spec {
	if stdin {
		return runner.Spec{Headless: []string{"drover", "t3", "run", "--provider", "claude", "--stdin"}, Stdin: true}
	}
	return runner.Spec{Headless: []string{"drover", "t3", "run", "--provider", "claude", "-p"}}
}

// A t3 spec runs through runner.Run unchanged, the prompt on argv and on stdin,
// and the log it leaves reads like a claude session's: the turns, tokens and
// cost, and the rate-limit reading reaching the usage row.
func TestRunnerRunDrivesTheT3BridgeEndToEnd(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdin=%v", stdin), func(t *testing.T) {
			srv, root := t3Host(t)
			spec := t3Spec(stdin)
			if f := runner.Faults(spec); len(f) != 0 {
				t.Fatalf("Faults(%v) = %+v", spec.Headless, f)
			}
			logPath := filepath.Join(t.TempDir(), "F17-x.20261026-120000.000000000.jsonl")
			var feed strings.Builder
			oc, err := runner.Run(context.Background(), runner.Where{Dir: root}, spec, runner.Headless,
				"Reply with the word ok and nothing else", logPath, &feed)
			if err != nil {
				b, _ := os.ReadFile(logPath)
				t.Fatalf("Run: %v (exit %d)\n%s", err, oc.Exit, b)
			}
			if !oc.Logged || oc.Result.Schema != "t3" || !oc.Result.Succeeded() || oc.Result.NumTurns != 1 {
				t.Fatalf("outcome = logged %v, %+v", oc.Logged, oc.Result)
			}
			if got := srv.CallsTo("t3_thread_launch")[0].Args["message"]; got != "Reply with the word ok and nothing else" {
				t.Errorf("the thread's message is %q", got)
			}
			if !strings.Contains(feed.String(), "ok") {
				t.Errorf("the feed shows no assistant text: %q", feed.String())
			}
			row, ok := usageRow("F17", ledger.StageImplement, logPath, oc.Result)
			if !ok || row.Usage.CacheCreate != 60365 || row.Turns != 1 || row.CostUSD != 0.01235007 {
				t.Errorf("usage row = %+v, %v", row, ok)
			}
			if row.RateLimit == nil || !row.RateLimit.WeeklyRejected() {
				t.Errorf("the usage row carries no rate-limit reading: %+v", row.RateLimit)
			}
		})
	}
}

// A run that ended interrupted exits 1 with a FaultRunner result, which is the
// pass drive answers with a runner_fault park.
func TestAnInterruptedT3RunParksRunnerFault(t *testing.T) {
	srv, root := t3Host(t)
	srv.Set(func(s *t3test.Server) { s.Final = "interrupted" })
	logPath := filepath.Join(t.TempDir(), "F17-x.20261026-120000.000000000.jsonl")
	oc, err := runner.Run(context.Background(), runner.Where{Dir: root}, t3Spec(false), runner.Headless, "hi", logPath, &strings.Builder{})
	if err == nil || oc.Exit != 1 {
		t.Fatalf("Run = %v, exit %d; want exit 1", err, oc.Exit)
	}
	if got := runResult(pass{res: oc.Result, logged: oc.Logged, events: oc.Events, known: oc.Known}); got != passFaulted {
		t.Errorf("drive reads the run as %q, want %q (runner_fault)", got, passFaulted)
	}
}

// SIGTERM, which runner.Run sends the bridge's group when its context ends,
// interrupts the run the bridge launched and no other thread.
func TestSIGTERMInterruptsTheLaunchedRunAndNoOther(t *testing.T) {
	srv, root := t3Host(t)
	srv.AddThread("mcp:someone-else")
	srv.Set(func(s *t3test.Server) { s.Hold = true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for len(srv.CallsTo("t3_thread_wait")) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	logPath := filepath.Join(t.TempDir(), "F17-x.20261026-120000.000000000.jsonl")
	oc, _ := runner.Run(ctx, runner.Where{Dir: root}, t3Spec(false), runner.Headless, "hold", logPath, &strings.Builder{})
	calls := srv.CallsTo("t3_thread_interrupt")
	if len(calls) != 1 {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("interrupt calls = %d, want 1 (exit %d)\n%s", len(calls), oc.Exit, b)
	}
	if id := calls[0].Args["threadId"]; id == "mcp:someone-else" {
		t.Error("the bridge interrupted a thread it did not launch")
	}
	if other := srv.Thread("mcp:someone-else"); other.Interrupts != 0 || other.Status != "running" {
		t.Errorf("a thread the bridge did not launch was touched: %+v", other)
	}
	if oc.Result.Subtype != "interrupted" {
		t.Errorf("result subtype %q, want interrupted", oc.Result.Subtype)
	}
}

// Inside a session drover launched, the bridge is a launch like any other and
// refuses; only the session drover itself started as the bridge runs.
func TestT3RunRefusesInsideALaunchedSession(t *testing.T) {
	_, root := t3Host(t)
	c := asDroverCmd(t, "t3", "run", "--provider", "claude", "-p", "hi")
	c.Dir = root
	c.Env = append(c.Env, runner.EnvActor+"="+runner.ActorAgent, runner.EnvSession+"=abc")
	out, err := c.CombinedOutput()
	if c.ProcessState == nil || c.ProcessState.ExitCode() != ExitRefused {
		t.Fatalf("t3 run inside a session exited %v: %v\n%s", c.ProcessState, err, out)
	}
}

// t3BridgeProject is a project on a T3 host whose [runners] t3 is the bridge, with
// extra appended to its config.
func t3BridgeProject(t *testing.T, srv *t3test.Server, token, extra string) (string, string) {
	t.Helper()
	dir, logs := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, ".drover.toml"), schemaLine+fmt.Sprintf(`project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
logs = %q

[verify]
default = "true"

[runners]
t3 = { headless = ["drover", "t3", "run", "--provider", "claude", "-p"] }

[t3]
url = %q
token_file = %q
provider_logs = %q
`, logs, srv.MCP, token, filepath.Join(dir, "provider"))+extra)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "provider"), 0o750); err != nil {
		t.Fatal(err)
	}
	return dir, logs
}

// The runner's sign-in check and login route to the bridge, and check reports
// a missing grant and a server version the bridge was not tested against.
func TestT3RunnerRoutesSignInAndCheckReportsIt(t *testing.T) {
	spec := t3Spec(false)
	if got := strings.Join(runner.AuthCheck(spec), " "); got != "drover t3 login --check" {
		t.Errorf("AuthCheck = %q", got)
	}
	if got := runner.Login(spec); got != "drover t3 login" {
		t.Errorf("Login = %q", got)
	}
	srv, _ := t3Host(t)
	token := filepath.Join(t.TempDir(), "t3-token")
	dir, _ := t3BridgeProject(t, srv, token, "")
	if got := findings(openIn(t, dir).checkReport(), "BRIDGE SIGNED OUT"); len(got) != 1 || !strings.Contains(got[0].Detail, "drover t3 login") {
		t.Fatalf("BRIDGE SIGNED OUT = %+v, want one naming the login", got)
	}
	if err := t3.WriteGrant(token, t3.Grant{URL: srv.MCP, AccessToken: "test-token"}); err != nil {
		t.Fatal(err)
	}
	if got := findings(openIn(t, dir).checkReport(), "BRIDGE VERSION"); len(got) != 0 {
		t.Errorf("a server at the tested version reported %+v", got)
	}
	srv.Set(func(s *t3test.Server) { s.Version = "0.2.0" })
	got := findings(openIn(t, dir).checkReport(), "BRIDGE VERSION")
	if len(got) != 1 || !strings.Contains(got[0].Message, "0.2.0") || !strings.Contains(got[0].Message, "0.1.2") ||
		!strings.Contains(got[0].Detail, "provider event log") {
		t.Errorf("BRIDGE VERSION = %+v, want both versions and the provider log named", got)
	}
}

// A runner whose last session printed usage_unavailable leaves usage_admission
// nothing to read, so a drive with a [usage.weekly] level refuses to start on
// it unless told otherwise, and --dry-run says so.
func TestADriveRefusesABlindT3RunnerUnderAWeeklyLevel(t *testing.T) {
	srv, _ := t3Host(t)
	dir, logs := t3BridgeProject(t, srv, filepath.Join(t.TempDir(), "t3-token"), "\n[usage.weekly]\npause_remaining = 10\n")
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"F17","stage":"implement","runner":"t3","session":"abc","date":"2026-10-26"}`+"\n")
	write(t, filepath.Join(logs, "F17-x.20261026-120000.000000000.jsonl"),
		`{"t3":"thread.started","thread_id":"mcp:x","session":"abc"}`+"\n"+
			`{"t3":"usage_unavailable","path":"/p","reason":"gone"}`+"\n"+
			`{"t3":"result","subtype":"success","checkpoints":[],"usage_unavailable":"not reported: gone"}`+"\n")
	s := openIn(t, dir)
	defer func(r string, a bool) { driveRunner, driveAllowBlind = r, a }(driveRunner, driveAllowBlind)
	driveRunner = "t3"
	err := s.preflightBlindUsage("t3")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "--allow-blind-usage") {
		t.Fatalf("preflightBlindUsage = %v, want a refusal naming --allow-blind-usage", err)
	}
	var plan strings.Builder
	s.blindPlan(&plan, "t3")
	if !strings.Contains(plan.String(), "usage_admission: blind for runner t3") {
		t.Errorf("dry run plan = %q", plan.String())
	}
	driveAllowBlind = true
	if err := s.preflightBlindUsage("t3"); err != nil {
		t.Errorf("--allow-blind-usage still refused: %v", err)
	}
}

// A grant issued for one endpoint never reaches the server another [t3] url
// names: discover, the bridge and check each refuse naming the login, and the
// server [t3] url points at receives nothing.
func TestAGrantForAnotherEndpointReachesNoServer(t *testing.T) {
	srv, root := t3Host(t)
	token := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "t3-token")
	if err := t3.WriteGrant(token, t3.Grant{URL: "https://t3-host-a.example/mcp", AccessToken: "test-token"}); err != nil {
		t.Fatal(err)
	}

	c := asDroverCmd(t, "t3", "discover")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "drover t3 login") || !strings.Contains(string(out), "t3-host-a.example") {
		t.Errorf("discover = %v\n%s\nwant a refusal naming both endpoints and the login", err, out)
	}

	logPath := filepath.Join(t.TempDir(), "F17-x.20261026-120000.000000000.jsonl")
	oc, err := runner.Run(context.Background(), runner.Where{Dir: root}, t3Spec(false), runner.Headless, "hi", logPath, &strings.Builder{})
	if b, _ := os.ReadFile(logPath); err == nil || oc.Exit != 1 || !strings.Contains(string(b), "drover t3 login") {
		t.Errorf("t3 run = %v, exit %d\n%s\nwant exit 1 naming the login", err, oc.Exit, b)
	}

	dir, _ := t3BridgeProject(t, srv, token, "")
	got := findings(openIn(t, dir).checkReport(), "BRIDGE SIGNED OUT")
	if len(got) != 1 || !strings.Contains(got[0].Message, "t3-host-a.example") || !strings.Contains(got[0].Detail, "drover t3 login") {
		t.Errorf("check BRIDGE SIGNED OUT = %+v, want one naming the grant's endpoint and the login", got)
	}

	srv.Set(func(s *t3test.Server) {
		if s.Hits != 0 {
			t.Errorf("the server [t3] url names received %d requests, want 0", s.Hits)
		}
	})
}

// An expired grant carrying a refresh token is not signed out: check renews it
// in memory, initializes with the renewed token, and leaves the file as it was.
func TestCheckRenewsAnExpiredGrantWithoutWritingIt(t *testing.T) {
	srv, _ := t3Host(t)
	srv.Set(func(s *t3test.Server) { s.Token, s.Refresh, s.Renewed = "never-issued", "r1", "renewed" })
	token := filepath.Join(t.TempDir(), "t3-token")
	if err := t3.WriteGrant(token, t3.Grant{URL: srv.MCP, AccessToken: "old", RefreshToken: "r1", ClientID: "c",
		TokenEndpoint: srv.URL + "/token", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(token)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := t3BridgeProject(t, srv, token, "")
	rep := openIn(t, dir).checkReport()
	for _, kind := range []string{"BRIDGE SIGNED OUT", "BRIDGE UNREACHABLE", "BRIDGE VERSION"} {
		if got := findings(rep, kind); len(got) != 0 {
			t.Errorf("%s = %+v, want none for a grant that renews", kind, got)
		}
	}
	if after, _ := os.ReadFile(token); string(after) != string(before) {
		t.Errorf("check rewrote the token file:\nbefore %s\nafter  %s", before, after)
	}
	if len(srv.CallsTo("t3_thread_launch")) != 0 {
		t.Error("check launched a thread")
	}
}

// An interrupted session says nothing about whether the provider log moved, so
// it neither trips the blind-usage refusal nor hides an earlier completed
// session that did print usage_unavailable.
func TestAnInterruptedSessionIsNoEvidenceOfABlindRunner(t *testing.T) {
	srv, _ := t3Host(t)
	dir, logs := t3BridgeProject(t, srv, filepath.Join(t.TempDir(), "t3-token"), "\n[usage.weekly]\npause_remaining = 10\n")
	blindLog := func(name, session, subtype string) {
		write(t, filepath.Join(logs, name),
			`{"t3":"thread.started","thread_id":"mcp:x","session":"`+session+`"}`+"\n"+
				`{"t3":"usage_unavailable","path":"/p","reason":"gone"}`+"\n"+
				`{"t3":"result","subtype":"`+subtype+`","is_error":`+fmt.Sprint(subtype != "success")+`,"checkpoints":[],"usage_unavailable":"not reported: gone"}`+"\n")
	}
	blindLog("F17-x.20261026-130000.000000000.jsonl", "late", "interrupted")
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"F17","stage":"implement","runner":"t3","session":"late","date":"2026-10-26"}`+"\n")
	defer func(r string, a bool) { driveRunner, driveAllowBlind = r, a }(driveRunner, driveAllowBlind)
	driveRunner, driveAllowBlind = "t3", false
	if err := openIn(t, dir).preflightBlindUsage("t3"); err != nil {
		t.Errorf("an interrupted last session refused the drive: %v", err)
	}

	blindLog("F17-x.20261026-120000.000000000.jsonl", "early", "success")
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"F17","stage":"implement","runner":"t3","session":"early","date":"2026-10-26"}`+"\n"+
			`{"id":"F17","stage":"implement","runner":"t3","session":"late","date":"2026-10-26"}`+"\n")
	if err := openIn(t, dir).preflightBlindUsage("t3"); Code(err) != ExitRefused {
		t.Errorf("a completed blind session behind an interrupted one = %v, want the refusal", err)
	}
}
