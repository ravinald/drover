package t3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/t3"
	"github.com/ravinald/drover/internal/t3/t3test"
)

type rig struct {
	srv  *t3test.Server
	root string
	logs string
	cl   *t3.Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root, logs := t.TempDir(), t.TempDir()
	srv := t3test.New(root, logs)
	t.Cleanup(srv.Close)
	return &rig{srv: srv, root: root, logs: logs,
		cl: t3.NewClient(srv.MCP, func(context.Context) (string, error) { return "test-token", nil })}
}

// run resolves and follows one launch from the project root, writing the log to
// a file the way runner.Run would, and returns that file and Run's error.
func (r *rig) run(ctx context.Context, t *testing.T, prompt string) (string, error) {
	t.Helper()
	proj, err := t3.FindProject(ctx, r.cl, "drover")
	if err != nil {
		t.Fatal(err)
	}
	place, err := t3.Place(ctx, r.cl, "claude", "claude-haiku-5-5", "supervised")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = t3.Run(ctx, t3.Options{
		Client: r.cl, Provider: "claude", Poll: 20 * time.Millisecond, ProviderLogs: r.logs, Out: &out,
		Launch: t3.Launch{Project: proj, Place: place, Title: "test", Prompt: prompt, Dir: r.root, Workspace: true},
	})
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if werr := os.WriteFile(path, out.Bytes(), 0o600); werr != nil {
		t.Fatal(werr)
	}
	return path, err
}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// The probe thread, replayed: runlog reads the turns, tokens and cost the SDK
// result in the provider log carried, the rate-limit reading the
// rate_limit_event did, and the final assistant text and checkpoint off the
// thread's items.
func TestACleanRunReadsTheRecordedTurnsTokensAndCost(t *testing.T) {
	r := newRig(t)
	path, err := r.run(context.Background(), t, "Reply with the word ok and nothing else")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rd, err := runlog.ReadResult(path)
	if err != nil || !rd.Found {
		t.Fatalf("ReadResult: found %v, %v", rd.Found, err)
	}
	res := rd.Result
	if res.Schema != "t3" || !res.Succeeded() || res.Text != "ok" {
		t.Errorf("result = schema %q, subtype %q, error %v, text %q; want t3, success, ok", res.Schema, res.Subtype, res.IsError, res.Text)
	}
	if res.NumTurns != 1 || res.Cost != 0.01235007 {
		t.Errorf("turns %d cost %g, want 1 and 0.01235007", res.NumTurns, res.Cost)
	}
	want := runlog.Usage{Input: 2, Output: 48, CacheCreate: 60365, CacheRead: 12817}
	if res.Usage == nil || *res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	if res.SessionID != "5b1c8f0e-probe" {
		t.Errorf("session id %q, want the SDK's, which is what dedupes the interactive transcript", res.SessionID)
	}
	if rl := res.RateLimit; rl == nil || !rl.WeeklyRejected() || !rl.InOverage() {
		t.Errorf("rate limit = %+v, want the recorded seven-day rejection on overage", rl)
	}
	var kinds []string
	var final map[string]any
	for _, l := range lines(t, path) {
		k, _ := l["t3"].(string)
		if k == "" {
			k = "sdk:" + l["type"].(string)
		}
		kinds = append(kinds, k)
		final = l
	}
	wantKinds := "thread.started message tool reasoning message checkpoint sdk:rate_limit_event sdk:result result"
	if got := strings.Join(kinds, " "); got != wantKinds {
		t.Errorf("kinds = %s\nwant    %s", got, wantKinds)
	}
	if cps, _ := final["checkpoints"].([]any); len(cps) != 1 {
		t.Errorf("checkpoints = %v, want the run's one checkpoint item", final["checkpoints"])
	}
	launch := r.srv.CallsTo("t3_thread_launch")[0].Args
	if ws, _ := launch["workspaceStrategy"].(map[string]any); ws["type"] != "root" {
		t.Errorf("workspaceStrategy = %v, want root for a launch from the project root", launch["workspaceStrategy"])
	}
	if launch["runtimeMode"] != "approval-required" {
		t.Errorf("runtimeMode = %v, want approval-required for supervised", launch["runtimeMode"])
	}
	for _, c := range r.srv.CallsTo("t3_thread_read") {
		if c.Args["view"] != "activity" {
			t.Errorf("a read used view %v; the follow loop reads activity", c.Args["view"])
		}
	}
}

// A missing provider log is the codex provider, or a T3 release that moved it.
// Either way the log says so, and the result carries no counts rather than
// zeros a usage row would record as a free session.
func TestAMissingProviderLogPrintsUsageUnavailableAndNoZeros(t *testing.T) {
	r := newRig(t)
	r.srv.Set(func(s *t3test.Server) { s.NoLog = true })
	path, err := r.run(context.Background(), t, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var unavailable, result map[string]any
	for _, l := range lines(t, path) {
		switch l["t3"] {
		case runlog.T3UsageUnavailable:
			unavailable = l
		case runlog.T3Result:
			result = l
		}
	}
	if unavailable == nil || !strings.Contains(unavailable["reason"].(string), "does not exist") {
		t.Fatalf("usage_unavailable = %v, want one naming the missing file", unavailable)
	}
	for _, k := range []string{"usage", "num_turns", "cost_usd"} {
		if _, ok := result[k]; ok {
			t.Errorf("the result carries %s = %v; an unread count is omitted, not zero", k, result[k])
		}
	}
	rd, _ := runlog.ReadResult(path)
	if rd.Result.Usage != nil || !strings.Contains(rd.Result.UsageUnavailable, "not reported") {
		t.Errorf("Result usage %+v, unavailable %q", rd.Result.Usage, rd.Result.UsageUnavailable)
	}
}

// A run that did not complete is a FaultRunner: drive parks it runner_fault
// and does not retry it, as it would a transport failure.
func TestAnInterruptedRunIsARunnerFault(t *testing.T) {
	r := newRig(t)
	r.srv.Set(func(s *t3test.Server) { s.Final = "interrupted" })
	path, err := r.run(context.Background(), t, "hi")
	if !errors.Is(err, t3.ErrRunFailed) {
		t.Fatalf("Run = %v, want ErrRunFailed", err)
	}
	rd, _ := runlog.ReadResult(path)
	if res := rd.Result; !res.IsError || res.Subtype != "interrupted" || res.Fault != runlog.FaultRunner {
		t.Errorf("result = subtype %q error %v fault %q, want interrupted, true, runner", res.Subtype, res.IsError, res.Fault)
	}
}

// Cancelling the bridge, which is what SIGTERM does through drover's root
// context, interrupts the run it launched and leaves every other thread alone.
func TestCancellingInterruptsTheLaunchedRunAndNoOther(t *testing.T) {
	r := newRig(t)
	r.srv.AddThread("mcp:someone-else")
	r.srv.Set(func(s *t3test.Server) { s.Hold = true })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(r.srv.CallsTo("t3_thread_wait")) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	path, err := r.run(ctx, t, "hold on")
	if !errors.Is(err, t3.ErrRunFailed) {
		t.Fatalf("Run = %v, want ErrRunFailed", err)
	}
	calls := r.srv.CallsTo("t3_thread_interrupt")
	if len(calls) != 1 {
		t.Fatalf("interrupt calls = %d, want 1", len(calls))
	}
	launched := lines(t, path)[0]["thread_id"].(string)
	if calls[0].Args["threadId"] != launched || calls[0].Args["runId"] == "" {
		t.Errorf("interrupted %v, want the launched thread %s and its run", calls[0].Args, launched)
	}
	if got := r.srv.Thread(launched).Status; got != "interrupted" {
		t.Errorf("launched thread status %q, want interrupted", got)
	}
	if other := r.srv.Thread("mcp:someone-else"); other.Interrupts != 0 || other.Status != "running" {
		t.Errorf("a thread the bridge did not launch was touched: %+v", other)
	}
	rd, _ := runlog.ReadResult(path)
	if rd.Result.Subtype != "interrupted" || !rd.Result.IsError {
		t.Errorf("result %q error %v, want interrupted", rd.Result.Subtype, rd.Result.IsError)
	}
}

func TestAMissingProjectNamesTProjectAdd(t *testing.T) {
	r := newRig(t)
	_, err := t3.FindProject(context.Background(), r.cl, "nowhere")
	if err == nil || !strings.Contains(err.Error(), "t3 project add") || !strings.Contains(err.Error(), "drover") {
		t.Errorf("FindProject = %v, want a refusal naming t3 project add and the projects there are", err)
	}
}

func TestA401NamesTheLogin(t *testing.T) {
	r := newRig(t)
	cl := t3.NewClient(r.srv.MCP, func(context.Context) (string, error) { return "stale", nil })
	_, err := cl.Initialize(context.Background())
	if !errors.Is(err, t3.ErrUnauthorized) || !strings.Contains(err.Error(), "drover t3 login") {
		t.Errorf("Initialize = %v, want ErrUnauthorized naming drover t3 login", err)
	}
	res, _ := runlog.ReadResult(writeFail(t, err))
	if res.Result.Fault != runlog.FaultAuth {
		t.Errorf("a 401's result line classifies as %q, want auth, so drive names the login", res.Result.Fault)
	}
}

func writeFail(t *testing.T, err error) string {
	t.Helper()
	var out bytes.Buffer
	t3.Fail(&out, "claude", "", err)
	path := filepath.Join(t.TempDir(), "fail.jsonl")
	if werr := os.WriteFile(path, out.Bytes(), 0o600); werr != nil {
		t.Fatal(werr)
	}
	return path
}

func TestAPromptOverTheLimitIsRefusedAndNotCut(t *testing.T) {
	r := newRig(t)
	proj, _ := t3.FindProject(context.Background(), r.cl, "drover")
	long := strings.Repeat("x", t3.MaxPrompt+1)
	_, err := t3.Start(context.Background(), r.cl, t3.Launch{Project: proj, Prompt: long, Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "120001") {
		t.Errorf("Start = %v, want a refusal naming the length", err)
	}
	if n := len(r.srv.CallsTo("t3_thread_launch")); n != 0 {
		t.Errorf("%d launches reached the server", n)
	}
}

func TestPlaceMapsProvidersAndRefusesWhatTheServerWillNot(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p, err := t3.Place(ctx, r.cl, "codex", "", "full")
	if err != nil || p.Instance != "codex" || p.Model != "gpt-6.1-sol" || p.Runtime != "full-access" {
		t.Errorf("codex = %+v, %v; want codex, its first model, full-access", p, err)
	}
	if _, err := t3.Place(ctx, r.cl, "claude", "claude-nope", "full"); err == nil || !strings.Contains(err.Error(), "claude-opus-5-5") {
		t.Errorf("an unlisted model = %v, want a refusal listing the models", err)
	}
	if _, err := t3.Place(ctx, r.cl, "claude", "", "read-only"); err == nil {
		t.Error("read-only permission was accepted; T3 Code has no read-only runtime mode")
	}
}

func TestCapabilityDeniedComesBackWithItsCode(t *testing.T) {
	r := newRig(t)
	err := r.cl.Call(context.Background(), "t3_project_create", map[string]any{"title": "x"}, nil)
	if t3.Code(err) != "capability_denied" {
		t.Errorf("t3_project_create = %v, want capability_denied", err)
	}
}

func TestStrategyPutsAWorktreeLaunchInThatWorktree(t *testing.T) {
	root, tree := t.TempDir(), t.TempDir()
	if got := t3.Strategy(root, root); got["type"] != "root" {
		t.Errorf("root = %v", got)
	}
	if got := t3.Strategy(root, tree); got["type"] != "existing_worktree" || got["worktreePath"] != tree {
		t.Errorf("worktree = %v", got)
	}
}

func TestGrantFileIsPrivateAndExpiryRespectsTheRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "t3-token")
	g := t3.Grant{URL: "https://x/mcp", AccessToken: "a", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := t3.WriteGrant(path, g); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	tok := &t3.Tokens{Path: path}
	if _, err := tok.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "drover t3 login") {
		t.Errorf("an expired grant with no refresh token = %v, want the login named", err)
	}
}
