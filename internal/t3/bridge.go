package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/ravinald/drover/internal/runlog"
)

// MaxPrompt is the longest message t3_thread_launch accepts, in the UTF-16 code
// units its schema's maxLength is enforced in by a JavaScript server.
const MaxPrompt = 120000

// Providers maps --provider onto the T3 Code provider instance it launches on.
var Providers = map[string]string{"claude": "claudeAgent", "codex": "codex"}

// Permissions maps --permission onto a thread's runtime mode. There is no
// read-only mode to map a third value to.
var Permissions = map[string]string{"supervised": "approval-required", "full": "full-access"}

// liveRun are the run statuses a run is still in progress at. The rest end it:
// completed, interrupted, failed, cancelled, rolled_back, idle, and anything a
// later release adds. waiting is a run held on a request a person answers, so
// a follow loop that ended on it would report a supervised run as finished.
var liveRun = []string{"preparing", "queued", "starting", "running", "waiting"}

// openItem are the item statuses an item can still change at. An item read in
// one of them is read again rather than passed: afterPosition is exclusive,
// and a cursor moved past a half-written message never sees the rest of it.
var openItem = []string{"preparing", "queued", "starting", "running", "waiting", "pending", "in_progress", "inProgress", "streaming", "started"}

// ThreadURL is the link T3 Code resolves to a thread, in its own app.
func ThreadURL(threadID string) string { return "t3-thread://v1/" + threadID }

// Project is one entry of t3_project_list.
type Project struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	WorkspaceRoot string  `json:"workspaceRoot"`
	DeletedAt     *string `json:"deletedAt"`
}

// FindProject resolves a project title to its entry, paging the whole list.
func FindProject(ctx context.Context, c *Client, title string) (Project, error) {
	var titles []string
	var cursor any
	for range 100 {
		args := map[string]any{"limit": 100}
		if cursor != nil {
			args["cursor"] = cursor
		}
		var page struct {
			Projects   []Project       `json:"projects"`
			NextCursor json.RawMessage `json:"nextCursor"`
		}
		if err := c.Call(ctx, "t3_project_list", args, &page); err != nil {
			return Project{}, err
		}
		for _, p := range page.Projects {
			if p.DeletedAt != nil {
				continue
			}
			if p.Title == title {
				return p, nil
			}
			titles = append(titles, p.Title)
		}
		var next int64
		if json.Unmarshal(page.NextCursor, &next) != nil {
			break
		}
		cursor = next
	}
	listed := "none"
	if len(titles) > 0 {
		listed = strings.Join(titles, ", ")
	}
	return Project{}, fmt.Errorf("T3 Code has no project titled %q (it has: %s).\n"+
		"drover never creates one: add it on the T3 host with `t3 project add --title %s <checkout root>`, or set [t3] project", title, listed, title)
}

// Placement is the provider instance, model and runtime mode a launch asks for.
type Placement struct {
	Instance string
	Model    string
	Runtime  string
}

// Place checks a provider and model against orchestrator_capabilities and
// returns what the launch sends. An empty model takes the first the provider
// lists, which is T3 Code's own default ordering.
func Place(ctx context.Context, c *Client, provider, model, permission string) (Placement, error) {
	instance, ok := Providers[provider]
	if !ok {
		return Placement{}, fmt.Errorf("--provider is %q: give claude or codex", provider)
	}
	runtime, ok := Permissions[permission]
	if !ok {
		return Placement{}, fmt.Errorf("--permission is %q: give supervised or full", permission)
	}
	type capabilities struct {
		Providers []struct {
			Instance    string   `json:"providerInstanceId"`
			Constraints []string `json:"constraints"`
			Models      []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	look := func(search string) (ids []string, found bool, err error) {
		args := map[string]any{}
		if search != "" {
			args["search"] = search
		}
		var caps capabilities
		if err := c.Call(ctx, "orchestrator_capabilities", args, &caps); err != nil {
			return nil, false, err
		}
		for _, p := range caps.Providers {
			if p.Instance != instance {
				continue
			}
			if len(p.Constraints) > 0 {
				return nil, true, fmt.Errorf("T3 Code will not launch on provider %s: %s", instance, strings.Join(p.Constraints, "; "))
			}
			for _, m := range p.Models {
				ids = append(ids, m.ID)
			}
			return ids, true, nil
		}
		return nil, false, nil
	}
	ids, found, err := look("")
	if err != nil {
		return Placement{}, err
	}
	if !found {
		return Placement{}, fmt.Errorf("T3 Code lists no provider %s in orchestrator_capabilities", instance)
	}
	switch {
	case model == "" && len(ids) == 0:
		return Placement{}, fmt.Errorf("T3 Code lists no model for provider %s: pass --model", instance)
	case model == "":
		model = ids[0]
	case !slices.Contains(ids, model):
		// The default list leaves models out; search is how T3 finds the rest.
		more, _, err := look(model)
		if err != nil {
			return Placement{}, err
		}
		if !slices.Contains(more, model) {
			return Placement{}, fmt.Errorf("T3 Code lists no model %q for provider %s (listed: %s)", model, instance, strings.Join(ids, ", "))
		}
	}
	return Placement{Instance: instance, Model: model, Runtime: runtime}, nil
}

// Launched is t3_thread_launch's answer.
type Launched struct {
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
	Status   string `json:"status"`
}

// Launch is one thread launch, everything resolved.
type Launch struct {
	Project   Project
	Place     Placement
	Title     string
	Prompt    string
	Dir       string
	Workspace bool
}

// PromptLength is a prompt's length as t3_thread_launch measures it.
func PromptLength(prompt string) int { return len(utf16.Encode([]rune(prompt))) }

// CheckPrompt refuses a prompt the server would, rather than cutting it.
func CheckPrompt(prompt string) error {
	if n := PromptLength(prompt); n > MaxPrompt {
		return fmt.Errorf("the prompt is %d characters and t3_thread_launch accepts at most %d: drover does not cut a prompt, so shorten the item or run it on a direct runner", n, MaxPrompt)
	}
	return nil
}

// Strategy is the workspaceStrategy for a launch from dir: the project root
// itself, or the worktree dir is, which is how a drive's thread works in the
// drive's own tree. The server decides whether it accepts a worktree it did not
// create.
func Strategy(root, dir string) map[string]any {
	if samePath(root, dir) {
		return map[string]any{"type": "root"}
	}
	return map[string]any{"type": "existing_worktree", "worktreePath": dir}
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// Start launches one thread and returns what the server answered.
func Start(ctx context.Context, c *Client, l Launch) (Launched, error) {
	if err := CheckPrompt(l.Prompt); err != nil {
		return Launched{}, err
	}
	args := map[string]any{
		"title":          l.Title,
		"projectId":      l.Project.ID,
		"runtimeMode":    l.Place.Runtime,
		"modelSelection": map[string]any{"provider": l.Place.Instance, "model": l.Place.Model},
	}
	if l.Prompt != "" {
		args["message"] = l.Prompt
	}
	if l.Workspace {
		args["workspaceStrategy"] = Strategy(l.Project.WorkspaceRoot, l.Dir)
	}
	var out Launched
	if err := c.Call(ctx, "t3_thread_launch", args, &out); err != nil {
		return Launched{}, err
	}
	if out.ThreadID == "" {
		return Launched{}, errors.New("t3_thread_launch answered with no threadId")
	}
	return out, nil
}

// Interrupt ends a thread's run, the newest interruptible one when runID is
// empty. A run that has already ended answers no_active_run, which is the
// outcome asked for rather than a failure.
func Interrupt(ctx context.Context, c *Client, threadID, runID, reason string) (string, error) {
	args := map[string]any{"threadId": threadID, "reason": reason}
	if runID != "" {
		args["runId"] = runID
	}
	var out struct {
		Status string `json:"status"`
	}
	err := c.Call(ctx, "t3_thread_interrupt", args, &out)
	if Code(err) == "no_active_run" {
		return "no_active_run", nil
	}
	return out.Status, err
}

// Options are what `drover t3 run` resolved before following a thread.
type Options struct {
	Client       *Client
	Launch       Launch
	Poll         time.Duration
	ProviderLogs string
	Provider     string
	// Session is the drover launch id the bridge runs under, written on its
	// first line so a stage row can find the log.
	Session string
	Out     io.Writer
	// Grace is how long after the run ends the bridge keeps reading the
	// provider log for the SDK's result, which T3 can write after it marks the
	// run complete. Zero is three poll intervals.
	Grace time.Duration
}

// stopBudget bounds everything a bridge does after it is told to stop. It sits
// inside the 10 seconds runner.Run waits after SIGTERM before it kills the
// group, leaving the rest for the result line to be written.
const stopBudget = 8 * time.Second

// ErrRunFailed is the bridge's exit for a run that did not complete; its
// result line has already said why.
var ErrRunFailed = errors.New("the T3 Code run did not complete")

type item struct {
	Position      int64  `json:"position"`
	ItemID        string `json:"itemId"`
	RunID         string `json:"runId"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Title         string `json:"title"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"textTruncated"`
	NextOffset    *int64 `json:"nextTextOffset"`
}

type readPage struct {
	Thread struct {
		Pending int `json:"pendingRequestCount"`
	} `json:"thread"`
	Items   []item `json:"items"`
	HasMore bool   `json:"hasMore"`
}

type follower struct {
	o        Options
	out      io.Writer
	thread   string
	run      string
	cursor   int64
	pending  int
	asked    map[string]bool
	waiting  bool
	answer   string
	checks   []string
	log      *providerLog
	launched time.Time
}

// Run launches the thread and follows its run to the end, printing one JSON
// line per event. It returns nil for a run that completed, ErrRunFailed for
// one that did not, and any other error for a launch that never happened; in
// every case past argument checks the last line written is a result.
func Run(ctx context.Context, o Options) (err error) {
	f := &follower{o: o, out: o.Out, cursor: -1, asked: map[string]bool{}}
	if o.Poll <= 0 {
		f.o.Poll = 2 * time.Second
	}
	if f.o.Grace <= 0 {
		f.o.Grace = 3 * f.o.Poll
	}
	l, err := Start(ctx, o.Client, o.Launch)
	if err != nil {
		f.fail(subtypeOf(err, "launch_failed"), err)
		return err
	}
	f.thread, f.run, f.launched = l.ThreadID, l.RunID, time.Now()
	f.log = &providerLog{path: ProviderLogPath(o.ProviderLogs, l.ThreadID)}
	defer f.log.close()
	f.emit(runlog.T3Event{
		Kind: runlog.T3ThreadStarted, ThreadID: l.ThreadID, RunID: l.RunID,
		Project: o.Launch.Project.Title, Provider: o.Provider, Model: o.Launch.Place.Model,
		RuntimeMode: o.Launch.Place.Runtime, Session: o.Session, URL: ThreadURL(l.ThreadID),
	})

	ended := false
	// Every return before the run ends stops the run this bridge launched, and
	// only that one: the thread id and run id are the launch's own answer.
	stop := func(ictx context.Context) {
		if ended {
			return
		}
		ended = true
		if _, ierr := Interrupt(ictx, o.Client, f.thread, f.run, "drover stopped the stage"); ierr != nil {
			fmt.Fprintf(os.Stderr, "drover t3: interrupting run %s of thread %s failed: %v\n", f.run, f.thread, ierr)
		}
	}
	defer func() {
		ictx, cancel := context.WithTimeout(context.Background(), stopBudget)
		defer cancel()
		stop(ictx)
	}()

	status := l.Status
	for {
		if ctx.Err() != nil {
			// The interrupt, the last read and the wait for the SDK's result
			// share one budget, so the result line is written before the kill
			// that follows SIGTERM.
			sctx, cancel := context.WithTimeout(context.Background(), stopBudget)
			defer cancel()
			stop(sctx)
			_ = f.drain(sctx, true)
			f.lastRead(sctx, stopBudget)
			f.finish("interrupted", fmt.Sprintf("drover stopped the stage (%v), and interrupted run %s", context.Cause(ctx), f.run))
			return ErrRunFailed
		}
		began := time.Now()
		var w struct {
			Status   string `json:"status"`
			TimedOut bool   `json:"timedOut"`
		}
		werr := o.Client.Call(ctx, "t3_thread_wait",
			map[string]any{"threadId": f.thread, "runId": f.run, "timeoutMs": (3 * f.o.Poll).Milliseconds()}, &w)
		if werr != nil {
			if ctx.Err() != nil {
				continue
			}
			ended = Code(werr) != ""
			_ = f.drain(context.Background(), true)
			f.fail(subtypeOf(werr, "wait_failed"), werr)
			return ErrRunFailed
		}
		if w.Status != "" {
			status = w.Status
		}
		live := slices.Contains(liveRun, status)
		if rerr := f.drain(ctx, !live); rerr != nil && ctx.Err() == nil {
			ended = Code(rerr) != ""
			f.fail(subtypeOf(rerr, "read_failed"), rerr)
			return ErrRunFailed
		}
		f.noteWaiting(status)
		if !live {
			ended = true
			break
		}
		if d := f.o.Poll - time.Since(began); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
	}
	f.lastRead(ctx, f.o.Grace)
	if status != "completed" {
		f.finish(status, fmt.Sprintf("the T3 Code run %s ended %s", f.run, status))
		return ErrRunFailed
	}
	f.finish("success", "")
	return nil
}

func subtypeOf(err error, fallback string) string {
	if c := Code(err); c != "" {
		return c
	}
	if errors.Is(err, ErrUnauthorized) {
		return "unauthorized"
	}
	return fallback
}

// drain reads every item written since the cursor, the pending requests, and
// the provider log. final passes items still open, since nothing will change
// them after the run.
func (f *follower) drain(ctx context.Context, final bool) error {
	defer f.log.poll(f.relay)
	for range 1000 {
		args := map[string]any{"threadId": f.thread, "view": "activity", "limit": 100, "maxCharsPerItem": 50000}
		if f.cursor >= 0 {
			args["afterPosition"] = f.cursor
		}
		var page readPage
		if err := f.o.Client.Call(ctx, "t3_thread_read", args, &page); err != nil {
			return err
		}
		f.pending = page.Thread.Pending
		held := false
		for _, it := range page.Items {
			if !final && slices.Contains(openItem, it.Status) {
				held = true
				break
			}
			f.cursor = it.Position
			if it.RunID != "" && it.RunID != f.run {
				continue
			}
			f.item(ctx, it)
		}
		if held || !page.HasMore || len(page.Items) == 0 {
			break
		}
	}
	f.requests(ctx)
	return nil
}

func (f *follower) item(ctx context.Context, it item) {
	pos := it.Position
	ev := runlog.T3Event{ItemID: it.ItemID, Position: &pos, Status: it.Status, Title: it.Title, Text: it.Text, TextTruncated: it.TextTruncated}
	switch t := it.Type; {
	case t == "user_message":
		ev.Kind, ev.Role = runlog.T3Message, "user"
	case t == "assistant_message":
		ev.Kind, ev.Role = runlog.T3Message, "assistant"
		if it.TextTruncated {
			ev.Text, ev.TextTruncated = f.wholeText(ctx, it)
		}
		f.answer = ev.Text
	case t == "reasoning":
		ev.Kind = runlog.T3Reasoning
	case t == "checkpoint":
		ev.Kind = runlog.T3Checkpoint
		f.checks = append(f.checks, it.ItemID)
	case strings.Contains(t, "approval"):
		ev.Kind, ev.ItemType = runlog.T3Approval, t
	default:
		ev.Kind, ev.ItemType = runlog.T3Tool, t
	}
	f.emit(ev)
}

// wholeText pages the rest of a truncated message by text offset. It reports
// whether the text is still short, which a read failing part-way leaves it.
func (f *follower) wholeText(ctx context.Context, it item) (string, bool) {
	text, next := it.Text, it.NextOffset
	for range 100 {
		if next == nil {
			return text, false
		}
		var page readPage
		args := map[string]any{"threadId": f.thread, "itemId": it.ItemID, "textOffset": *next, "maxCharsPerItem": 50000}
		if err := f.o.Client.Call(ctx, "t3_thread_read", args, &page); err != nil || len(page.Items) == 0 {
			return text, true
		}
		text += page.Items[0].Text
		if page.Items[0].NextOffset != nil && *page.Items[0].NextOffset <= *next {
			return text, true
		}
		next = page.Items[0].NextOffset
	}
	return text, true
}

// requests prints each pending request once, and answers none of them: a
// request is a person's to answer, in T3 Code.
func (f *follower) requests(ctx context.Context) {
	if f.pending == 0 {
		return
	}
	var raw json.RawMessage
	if err := f.o.Client.Call(ctx, "t3_pending_request_list", map[string]any{"threadId": f.thread}, &raw); err != nil {
		return
	}
	for _, r := range requestList(raw) {
		var id struct {
			RequestID string `json:"requestId"`
			ID        string `json:"id"`
		}
		_ = json.Unmarshal(r, &id)
		key := id.RequestID
		if key == "" {
			key = id.ID
		}
		seen := key
		if seen == "" {
			seen = string(r)
		}
		if f.asked[seen] {
			continue
		}
		f.asked[seen] = true
		f.emit(runlog.T3Event{Kind: runlog.T3Approval, RequestID: key, Request: r, Status: "pending"})
	}
}

// requestList finds the requests in a pending-request answer whose shape no
// probe has seen: a bare array, or the first array-valued field of an object.
func requestList(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	for _, key := range []string{"requests", "pendingRequests", "items"} {
		if json.Unmarshal(obj[key], &list) == nil && list != nil {
			return list
		}
	}
	return nil
}

// noteWaiting prints an approval line the first time a run enters waiting.
// t3_pending_request_list leaves approval requests out, so a run held on one
// is visible here and in the phone's thread view, and nowhere else.
func (f *follower) noteWaiting(status string) {
	if status != "waiting" {
		f.waiting = false
		return
	}
	if f.waiting {
		return
	}
	f.waiting = true
	f.emit(runlog.T3Event{Kind: runlog.T3Approval, RunID: f.run, Status: "waiting",
		Text: "the run is waiting on a request; drover answers none, so a person answers it in T3 Code or the run stays held"})
}

// lastRead gives the provider log the grace a result written after the run
// ended needs: the SDK writes it once the query closes, which can be after T3
// marks the run complete, and an interrupted query closes only once it has
// aborted. It stops early at ctx's deadline.
func (f *follower) lastRead(ctx context.Context, grace time.Duration) {
	deadline := time.Now().Add(grace)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for f.log.result == nil {
		f.log.poll(f.relay)
		left := time.Until(deadline)
		if f.log.result != nil || left <= 0 || ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(min(f.o.Poll/4, left)):
		case <-ctx.Done():
		}
	}
}

func (f *follower) relay(payload json.RawMessage) {
	_, _ = f.out.Write(append(slices.Clone(payload), '\n'))
}

func (f *follower) emit(ev runlog.T3Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = f.out.Write(append(b, '\n'))
}

// fail is the result line for a bridge that stopped on an error of its own.
func (f *follower) fail(subtype string, err error) {
	f.finish(subtype, err.Error())
}

// finish writes the terminal line: the run's outcome, and the SDK's counts
// where the provider log gave them.
func (f *follower) finish(subtype, why string) {
	checks := slices.Clone(f.checks)
	if checks == nil {
		checks = []string{}
	}
	failed := subtype != "success"
	ev := runlog.T3Event{
		Kind: runlog.T3Result, ThreadID: f.thread, RunID: f.run, Provider: f.o.Provider,
		Model: f.o.Launch.Place.Model, Subtype: subtype, IsError: &failed,
		Result: f.answer, Checkpoints: &checks,
	}
	if failed {
		ev.Result = why
		if f.answer != "" {
			ev.Result += "; its last message: " + f.answer
		}
	}
	if !f.launched.IsZero() {
		ev.DurationMS = time.Since(f.launched).Milliseconds()
	}
	if f.log != nil {
		if r := f.log.result; r != nil {
			turns, cost := r.NumTurns, r.Cost
			ev.NumTurns, ev.CostUSD, ev.Usage, ev.SessionID = &turns, &cost, r.Usage, r.SessionID
			if r.DurationMS > 0 {
				ev.DurationMS = r.DurationMS
			}
			if m := firstKey(r.ModelUsage); m != "" {
				ev.Model = m
			}
		} else {
			reason := f.log.unavailable()
			f.emit(runlog.T3Event{Kind: runlog.T3UsageUnavailable, ThreadID: f.thread, Path: f.log.path, Reason: reason})
			ev.UsageUnavailable = "turns, tokens, cost and the rate-limit reading were not reported: " + reason
		}
	}
	f.emit(ev)
}

func firstKey(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	tok, _ := dec.Token()
	key, _ := tok.(string)
	return key
}

// Fail writes the result line for a stage that never reached a launch: the
// project, provider or model refused, or the server would not answer. drive
// reads it as the session's outcome, so the park names the cause rather than a
// runner that wrote nothing.
func Fail(w io.Writer, provider, model string, err error) {
	f := &follower{o: Options{Provider: provider, Launch: Launch{Place: Placement{Model: model}}}, out: w}
	f.fail(subtypeOf(err, "refused"), err)
}
