package runlog

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// T3Key is the field every line the t3 bridge writes names its kind under. Not
// "type": claude and codex both key on that, codex already owns
// "thread.started", and Claude's own "result" and "rate_limit_event" lines pass
// through the bridge unchanged for readClaude, so a t3 line carrying "type"
// would be claimed by whichever schema came first.
const T3Key = "t3"

// The kinds of t3 line, in the order a run writes them.
const (
	T3ThreadStarted    = "thread.started"
	T3Message          = "message"
	T3Reasoning        = "reasoning"
	T3Tool             = "tool"
	T3Approval         = "approval"
	T3Checkpoint       = "checkpoint"
	T3UsageUnavailable = "usage_unavailable"
	T3Result           = "result"
)

// T3Kinds is every kind the t3 schema claims.
func T3Kinds() []string {
	return []string{T3ThreadStarted, T3Message, T3Reasoning, T3Tool, T3Approval, T3Checkpoint, T3UsageUnavailable, T3Result}
}

// T3Event is one line of a t3 log, every kind in one struct so the bridge that
// writes it and the schema that reads it cannot disagree about a field name.
// A field belongs to the kinds its comment names and is omitted from the rest.
type T3Event struct {
	Kind string `json:"t3"`

	// thread.started, and the result. Session is the drover launch id the
	// bridge ran under, which is how a stage row finds the log it wrote.
	ThreadID    string `json:"thread_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	Project     string `json:"project,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	RuntimeMode string `json:"runtime_mode,omitempty"`
	Session     string `json:"session,omitempty"`
	URL         string `json:"url,omitempty"`

	// message, reasoning, tool, checkpoint: one thread item. Role is user or
	// assistant on a message; ItemType is T3 Code's own type for a tool item,
	// command_execution or whatever else the server names.
	ItemID        string `json:"item_id,omitempty"`
	Position      *int64 `json:"position,omitempty"`
	Role          string `json:"role,omitempty"`
	ItemType      string `json:"item_type,omitempty"`
	Status        string `json:"status,omitempty"`
	Title         string `json:"title,omitempty"`
	Text          string `json:"text,omitempty"`
	TextTruncated bool   `json:"text_truncated,omitempty"`

	// approval: one pending request, as the server listed it.
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`

	// usage_unavailable: the provider log the bridge tried, and why it yielded
	// nothing. The result line repeats the reason.
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`

	// result. Usage, NumTurns and CostUSD are pointers so a result whose
	// counts could not be read omits them, rather than reporting zeros a usage
	// row would record as a free session; UsageUnavailable then says why.
	// Checkpoints is a pointer so the result carries it empty and every other
	// kind omits it.
	Subtype          string    `json:"subtype,omitempty"`
	IsError          bool      `json:"is_error,omitempty"`
	Result           string    `json:"result,omitempty"`
	NumTurns         *int      `json:"num_turns,omitempty"`
	Usage            *APIUsage `json:"usage,omitempty"`
	CostUSD          *float64  `json:"cost_usd,omitempty"`
	Checkpoints      *[]string `json:"checkpoints,omitempty"`
	SessionID        string    `json:"session_id,omitempty"`
	DurationMS       int64     `json:"duration_ms,omitempty"`
	UsageUnavailable string    `json:"usage_unavailable,omitempty"`
}

// readT3 reads the bridge's terminal line. A failed one is a FaultRunner unless
// its text names something more specific: a 401 reads as auth through
// Classify, because the bridge's sentence for it says unauthorized.
func readT3(line []byte) (Result, bool) {
	var ev T3Event
	if err := json.Unmarshal(line, &ev); err != nil || ev.Kind != T3Result {
		return Result{}, false
	}
	r := Result{
		Subtype: ev.Subtype, IsError: ev.IsError, Text: ev.Result, Model: ev.Model,
		SessionID: ev.SessionID, DurationMS: ev.DurationMS, UsageUnavailable: ev.UsageUnavailable,
	}
	if ev.NumTurns != nil {
		r.NumTurns = *ev.NumTurns
	}
	if ev.CostUSD != nil {
		r.Cost = *ev.CostUSD
	}
	if ev.Usage != nil {
		u := ev.Usage.Usage()
		r.Usage = &u
	}
	if !r.Succeeded() {
		r.Fault = FaultRunner
	}
	return r, true
}

// carryT3 keeps the last assistant message and why usage went unread, for a
// log whose result line never arrived or carried neither.
func carryT3(line []byte, r *Result) {
	var ev T3Event
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch {
	case ev.Kind == T3Message && ev.Role == "assistant" && ev.Text != "":
		r.Text = ev.Text
	case ev.Kind == T3UsageUnavailable:
		r.UsageUnavailable = ev.Reason
	}
}

// t3Activity reads what a thread was doing: each tool item by its title and
// the first line of its text, and each assistant message.
func t3Activity(line []byte) []Activity {
	var ev T3Event
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch {
	case ev.Kind == T3Tool:
		name := ev.ItemType
		if name == "" {
			name = "tool"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(ev.Text), "\n")
		if ev.Title != "" {
			first = ev.Title + " " + first
		}
		return []Activity{toolActivity(name, map[string]any{"detail": first}, "detail")}
	case ev.Kind == T3Approval:
		return []Activity{toolActivity("approval", map[string]any{"request": ev.RequestID}, "request")}
	case ev.Kind == T3Message && ev.Role == "assistant":
		if a, ok := textActivity(ev.Text); ok {
			return []Activity{a}
		}
	}
	return nil
}

// T3Session is the drover session a t3 log's thread.started line names, empty
// for a log that is not the bridge's. Only the head of the file is read: the
// bridge writes that line first.
func T3Session(path string) string {
	f, err := os.Open(path) // #nosec G304 -- a log under drover's own log directory
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for range 5 {
		if !sc.Scan() {
			return ""
		}
		var ev T3Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Kind == T3ThreadStarted {
			return ev.Session
		}
	}
	return ""
}
