// Package t3test is a T3 Code MCP server for tests: the responses recorded from
// T3 Code 0.1.2 on 2026-10-25, one scripted thread per launch, and the provider
// event log T3's Claude driver writes beside it.
package t3test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RecordedRateLimit and RecordedResult are the two Claude Agent SDK payloads
// the probe thread's provider event log carried, verbatim.
const (
	RecordedRateLimit = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","isUsingOverage":true,"overageStatus":"allowed","unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1792986000,"status":"allowed"},"seven_day":{"utilization":1,"resetsAt":1792983600,"status":"rejected","surpassedThreshold":1}}}}`
	RecordedResult    = `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.01235007,"session_id":"5b1c8f0e-probe","usage":{"input_tokens":2,"cache_creation_input_tokens":60365,"cache_read_input_tokens":12817,"output_tokens":48}}`
)

// RecordedCapabilities is orchestrator_capabilities as the probe read it,
// trimmed to the providers the bridge maps and one it must refuse.
const RecordedCapabilities = `{"runtimeMode":"approval-required","interactionMode":"default","providers":[` +
	`{"providerInstanceId":"claudeAgent","driverKind":"claudeAgent","displayName":"Claude","models":[{"id":"claude-opus-5-5","options":[{"id":"effort","values":["low","medium","high","xhigh","max","ultracode","ultrathink"],"default":"medium"},{"id":"fastMode","values":[false,true]}]},{"id":"claude-sonnet-5-5"},{"id":"claude-fable-5-1"},{"id":"claude-haiku-5-5"}],"omittedModels":9,"constraints":[]},` +
	`{"providerInstanceId":"codex","driverKind":"codex","models":[{"id":"gpt-6.1-sol","options":[{"id":"reasoningEffort"},{"id":"serviceTier","values":["default","priority"]}]},{"id":"gpt-6-astra"},{"id":"gpt-6-luna"}],"omittedModels":4,"constraints":[]},` +
	`{"providerInstanceId":"cursor","constraints":["Provider instance is disabled."]}],` +
	`"features":{"asyncPolling":true,"cancellation":true,"incrementalThreadRead":true,"threadManagement":true,"batchThreadCreation":true,"maxBatchThreads":20}}`

// RecordedCapabilityDenied is t3_project_create refused for a Supervised client.
const RecordedCapabilityDenied = `{"_tag":"OrchestratorMcpFailure","code":"capability_denied","message":"Changing projects or environment settings needs a live full-access/default calling thread or a full-access client."}`

// ServerVersion is the version initialize reports unless a test sets another.
const ServerVersion = "0.1.2"

// Item is one scripted timeline item, in the order the run writes them.
type Item struct {
	Type  string
	Title string
	Text  string
}

// ProbeItems are the five items the probe thread's activity view returned.
func ProbeItems() []Item {
	return []Item{
		{Type: "user_message"},
		{Type: "command_execution", Title: "Workspace ready", Text: "$ Preparing workspace\nWorkspace preparation completed."},
		{Type: "reasoning", Title: "Thinking", Text: "The instruction is to reply with \"ok\" and nothing else."},
		{Type: "assistant_message", Text: "ok"},
		{Type: "checkpoint", Text: "[]"},
	}
}

// Call is one tool call the server answered.
type Call struct {
	Tool string
	Args map[string]any
}

// Thread is one launched thread's state.
type Thread struct {
	ID, RunID, Project string
	Status             string
	Items              []map[string]any
	Interrupts         int
}

// Server is the fake. Every field is read under its lock while serving, so a
// test may set them before or between launches.
type Server struct {
	*httptest.Server
	// MCP is the endpoint, the server's URL plus /mcp.
	MCP string

	mu sync.Mutex
	// Token is the bearer every request must carry; anything else is a 401.
	Token string
	// Version is what initialize reports.
	Version string
	// Projects is what t3_project_list returns.
	Projects []map[string]any
	// Items is the timeline each launched thread's run writes.
	Items []Item
	// Final is the status a run ends at; Hold keeps it running until it is
	// interrupted.
	Final string
	Hold  bool
	// LogDir is where a launch writes the thread's provider event log, and
	// NoLog leaves it unwritten.
	LogDir string
	NoLog  bool
	// SSE answers tools/call as an event stream rather than one JSON object.
	SSE bool

	Calls   []Call
	Threads map[string]*Thread
	seq     int
}

// New starts a server with one project, drover, rooted at root, and the probe's
// script.
func New(root, logDir string) *Server {
	s := &Server{
		Token: "test-token", Version: ServerVersion, Items: ProbeItems(), Final: "completed",
		LogDir: logDir, SSE: true, Threads: map[string]*Thread{},
		Projects: []map[string]any{{
			"id": "51f3b3bf-6bdf-4190-ba03-b8eee5bc4d3a", "title": "drover", "workspaceRoot": root,
			"repositoryIdentity": nil, "defaultModelSelection": map[string]any{"instanceId": "claudeAgent", "model": "claude-opus-5-5"},
			"defaultThreadEnvMode": "local", "autoPull": false, "scripts": []any{},
			"createdAt": "2026-10-25T22:00:00.000Z", "updatedAt": "2026-10-25T22:00:00.000Z", "deletedAt": nil,
		}},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	s.MCP = s.URL + "/mcp"
	return s
}

// Set runs f under the server's lock.
func (s *Server) Set(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// CallsTo is every call made to one tool.
func (s *Server) CallsTo(tool string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.Calls {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// Thread is a copy of one thread's state.
func (s *Server) Thread(id string) Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.Threads[id]; ok {
		return *t
	}
	return Thread{}
}

// AddThread registers a thread the bridge did not launch, running, so a test
// can show the bridge never touches it.
func (s *Server) AddThread(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Threads[id] = &Thread{ID: id, RunID: "run:thread:" + id + ":ordinal:1", Status: "running"}
}

type rpcReq struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	token := s.Token
	s.mu.Unlock()
	if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.URL+`/.well-known/oauth-protected-resource"`)
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		return
	}
	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "initialize":
		s.mu.Lock()
		v := s.Version
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", "test-session")
		reply(w, req.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "t3code", "version": v}}, false)
		return
	case "tools/list":
		reply(w, req.ID, map[string]any{"tools": []map[string]any{
			{"name": "t3_thread_launch", "description": "Start a thread from outside T3", "inputSchema": map[string]any{"type": "object"}},
			{"name": "t3_thread_wait", "inputSchema": map[string]any{"type": "object"}},
		}}, false)
		return
	case "tools/call":
	default:
		replyErr(w, req.ID, -32601, "method not found")
		return
	}
	payload, failure := s.tool(r, req.Params.Name, req.Params.Arguments)
	text, _ := json.Marshal(payload)
	if failure != "" {
		text = []byte(failure)
	}
	s.mu.Lock()
	sse := s.SSE
	s.mu.Unlock()
	reply(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": failure != ""}, sse)
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, sse bool) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func replyErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	_, _ = w.Write(b)
}

func failure(code, msg string) string {
	b, _ := json.Marshal(map[string]any{"_tag": "OrchestratorMcpFailure", "code": code, "message": msg})
	return string(b)
}

// tool answers one call: a payload, or a failure in the server's own shape.
func (s *Server) tool(r *http.Request, name string, args map[string]any) (any, string) {
	s.mu.Lock()
	s.Calls = append(s.Calls, Call{Tool: name, Args: args})
	s.mu.Unlock()
	switch name {
	case "t3_project_list":
		s.mu.Lock()
		defer s.mu.Unlock()
		return map[string]any{"projects": s.Projects, "nextCursor": nil}, ""
	case "t3_project_create":
		return nil, RecordedCapabilityDenied
	case "orchestrator_capabilities":
		return json.RawMessage(RecordedCapabilities), ""
	case "t3_thread_launch":
		return s.launch(args)
	case "t3_thread_wait":
		return s.wait(r, args)
	case "t3_thread_read":
		return s.read(args)
	case "t3_thread_interrupt":
		return s.interrupt(args)
	case "t3_pending_request_list":
		return map[string]any{"requests": []any{}}, ""
	}
	return nil, failure("unknown_tool", "no tool "+name)
}

func (s *Server) launch(args map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg, _ := args["message"].(string); len(msg) > 120000 {
		return nil, failure("invalid_input", "message is longer than 120000")
	}
	s.seq++
	id := fmt.Sprintf("mcp:68eab9c9-e102-4e85-8e5b-%012d", s.seq)
	run := "run:thread:" + strings.ReplaceAll(id, ":", "%3A") + ":ordinal:1"
	t := &Thread{ID: id, RunID: run, Status: "running"}
	if p, _ := args["projectId"].(string); p != "" {
		t.Project = p
	}
	msg, _ := args["message"].(string)
	for i, it := range s.Items {
		text := it.Text
		if it.Type == "user_message" {
			text = msg
		}
		itemID := fmt.Sprintf("turn-item:%s:%d", it.Type, i)
		if it.Type == "checkpoint" {
			itemID = "turn-item:provider:claudeAgent:native-item:checkpoint%3Acheckpoint%3Ascope%3Aroot%3Aname%3A1"
		}
		t.Items = append(t.Items, map[string]any{
			"position": i, "itemId": itemID, "runId": run, "type": it.Type, "status": "completed",
			"title": nilIfEmpty(it.Title), "text": text, "textTruncated": false, "nextTextOffset": nil,
		})
	}
	if s.Hold {
		t.Items = t.Items[:min(2, len(t.Items))]
	}
	s.Threads[id] = t
	if !s.NoLog && s.LogDir != "" {
		_ = os.MkdirAll(s.LogDir, 0o700)
		_ = os.WriteFile(filepath.Join(s.LogDir, "events."+strings.ReplaceAll(id, ":", "-")+".log"), []byte(providerLog(id)), 0o600)
	}
	return map[string]any{"threadId": id, "projectId": t.Project,
		"modelSelection": args["modelSelection"], "runId": run, "status": "preparing"}, ""
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// providerLog is the provider event log the probe's Claude driver wrote, cut
// to the lines that matter: the query opening, the two payloads drover reads,
// and an event of another kind in between.
func providerLog(threadID string) string {
	line := func(direction, payload string) string {
		return fmt.Sprintf(`[2026-10-25T22:42:17.100Z] NTIVE: {"provider":"claudeAgent","protocol":"claude-agent-sdk.query","providerSessionId":%q,"event":{"direction":%q,"stage":"stream","payload":%s}}`+"\n",
			threadID, direction, payload)
	}
	return line("outgoing", `{"type":"query.open","options":{"model":"claude-haiku-5-5","cwd":"/tmp","sessionId":"5b1c8f0e-probe"}}`) +
		line("incoming", `{"type":"system","subtype":"init"}`) +
		line("incoming", RecordedRateLimit) +
		line("incoming", `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`) +
		line("incoming", RecordedResult)
}

func (s *Server) thread(args map[string]any) (*Thread, string) {
	id, _ := args["threadId"].(string)
	t, ok := s.Threads[id]
	if !ok {
		return nil, failure("thread_not_found", "no thread "+id)
	}
	return t, ""
}

func (s *Server) wait(r *http.Request, args map[string]any) (any, string) {
	timeout := 0.0
	if v, ok := args["timeoutMs"].(float64); ok {
		timeout = v
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	for {
		s.mu.Lock()
		t, fail := s.thread(args)
		if fail != "" {
			s.mu.Unlock()
			return nil, fail
		}
		if !s.Hold && t.Status == "running" {
			t.Status = s.Final
		}
		st, run := t.Status, t.RunID
		s.mu.Unlock()
		if st != "running" || !time.Now().Before(deadline) || r.Context().Err() != nil {
			return map[string]any{"threadId": t.ID, "runId": run, "status": st, "timedOut": st == "running"}, ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Server) read(args map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, fail := s.thread(args)
	if fail != "" {
		return nil, fail
	}
	after := -1.0
	if v, ok := args["afterPosition"].(float64); ok {
		after = v
	}
	view, _ := args["view"].(string)
	var items []map[string]any
	for _, it := range t.Items {
		if float64(it["position"].(int)) <= after {
			continue
		}
		if view != "activity" {
			if typ := it["type"]; typ != "user_message" && typ != "assistant_message" {
				continue
			}
		}
		items = append(items, it)
	}
	next := any(nil)
	if len(items) > 0 {
		next = items[len(items)-1]["position"]
	}
	active := any(t.RunID)
	if t.Status != "running" {
		active = nil
	}
	return map[string]any{
		"thread": map[string]any{"threadId": t.ID, "status": t.Status, "latestRunId": t.RunID, "activeRunId": active,
			"runCount": 1, "itemCount": len(t.Items), "pendingRequestCount": 0, "archived": false, "settled": false},
		"recentRuns": []any{map[string]any{"runId": t.RunID, "ordinal": 1, "status": t.Status}},
		"items":      items, "nextPosition": next, "hasMore": false,
	}, ""
}

func (s *Server) interrupt(args map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, fail := s.thread(args)
	if fail != "" {
		return nil, fail
	}
	t.Interrupts++
	if t.Status != "running" {
		return map[string]any{"threadId": t.ID, "runId": nil, "status": "no_active_run"}, ""
	}
	t.Status = "interrupted"
	return map[string]any{"threadId": t.ID, "runId": t.RunID, "status": "interrupted"}, ""
}
