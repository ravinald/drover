// Package runlog finds and reads the event streams a headless run leaves behind.
//
// The filename is the entire discovery contract, deliberately: any runner that
// writes <base>.<timestamp>.jsonl into the log directory is discoverable here,
// whether or not drover knows what produced it.
package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Base is the filename stem for one item's runs: the padded id and the slug, so
// a directory listing sorts in backlog order.
func Base(stem, slug string) string { return stem + "-" + slug }

// ReviewBase is the filename stem for one item's review passes. The suffix
// keeps a reviewer's stream out of Find's glob for the implementation base, so
// report reads what the run claimed rather than what its reviewer said about
// it.
func ReviewBase(stem, slug string) string { return Base(stem, slug) + "-review" }

// DraftBase is the filename stem for one item's drafting passes. Same reason
// for the suffix as ReviewBase: a draft is a session about the item, not a run
// of it, and report must not read one as the other.
func DraftBase(stem, slug string) string { return Base(stem, slug) + "-draft" }

// Name is a fresh log filename. The timestamp carries nanoseconds because two
// runs of one item started in the same second would otherwise collide, and the
// second would append to the first's stream.
func Name(base string, at time.Time) string {
	return fmt.Sprintf("%s.%s.jsonl", base, at.Format(stampLayout))
}

const stampLayout = "20060102-150405.000000000"

// ParseName is Name's inverse: the base and the instant a log filename
// carries, the instant read in loc, since Name formats the writer's local
// clock and records no zone. False for a name Name could not have produced.
func ParseName(name string, loc *time.Location) (base string, at time.Time, ok bool) {
	rest, found := strings.CutSuffix(name, ".jsonl")
	if !found || len(rest) < len(stampLayout)+2 {
		return "", time.Time{}, false
	}
	cut := len(rest) - len(stampLayout)
	if rest[cut-1] != '.' {
		return "", time.Time{}, false
	}
	at, err := time.ParseInLocation(stampLayout, rest[cut:], loc)
	if err != nil {
		return "", time.Time{}, false
	}
	return rest[:cut-1], at, true
}

// Result is the terminal event of a run, normalized across every schema drover
// reads. Subtype and IsError keep Claude Code's names because it was the only
// runner when they were written; the other schemas map onto them.
type Result struct {
	Subtype  string
	IsError  bool
	NumTurns int
	Cost     float64
	Text     string
	// Schema is which of Schemas() recognized the event, empty on a zero
	// Result. A park naming a cause needs the runner family, and a log's
	// filename does not carry one.
	Schema string
	// Fault is why a failed result failed when the cause is the service or the
	// account rather than the work, and RetryAt the instant the runner named to
	// come back at. fault.go holds both and what reads them.
	Fault   string
	RetryAt time.Time
	// Transport is the runner naming its own connection to the model as what
	// ended the session, for the runners that name it. It decides that a
	// failure is a fault; which fault it is still comes from the message.
	Transport bool
	// Denied is what the runner refused to let the session do, named the way
	// the terminal event names it: agy's denied_actions, claude's
	// permission_denials. Nothing reads it as an outcome, because a denial is
	// not a failure: an implementation run that loses one `gh pr create` to a
	// prompt and then finishes 55 turns is ordinary, and three of this
	// project's own logs are that. It is the cause a report of a session that
	// did nothing can name, and the field nothing read when a review denied
	// every command took one turn, returned an empty response and exited
	// SUCCESS.
	Denied []string
	// Usage is the session's token counts as the terminal event reported them,
	// nil when it carried no usage object. A zero Usage is not a nil one: a
	// result refused for an exhausted account reports zeros, and that session
	// still happened.
	Usage *Usage
	// Model is the first model the terminal event's modelUsage names, and
	// SessionID the runner's own id for the session, which is what a usage row
	// is deduplicated by.
	Model      string
	SessionID  string
	DurationMS int64
	// ContextMax is the largest context any one assistant message carried:
	// input, cache read and cache creation together. A session's total says
	// what it spent and this says how full its window got, which is what a
	// session left running for days gives away.
	ContextMax int64
}

// Usage is one session's token counts, under the names a ledger usage row
// carries them by.
type Usage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheCreate int64 `json:"cache_create"`
	CacheRead   int64 `json:"cache_read"`
}

// Add sums o into u.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheCreate += o.CacheCreate
	u.CacheRead += o.CacheRead
}

// APIUsage is a usage object as the Anthropic API names its fields, the form
// both a stream-json event and a Claude Code transcript carry.
type APIUsage struct {
	Input       int64 `json:"input_tokens"`
	Output      int64 `json:"output_tokens"`
	CacheCreate int64 `json:"cache_creation_input_tokens"`
	CacheRead   int64 `json:"cache_read_input_tokens"`
}

// Usage renames the counts.
func (a APIUsage) Usage() Usage {
	return Usage(a)
}

// Context is how much of the window one message filled.
func (a APIUsage) Context() int64 { return a.Input + a.CacheRead + a.CacheCreate }

// Denials is the clause naming what the runner refused this session, and empty
// when the terminal event named none. Rendered here rather than at the two call
// sites so drive's park and review's report cannot word the same cause
// differently.
func (r Result) Denials() string {
	if len(r.Denied) == 0 {
		return ""
	}
	return "the runner denied " + strings.Join(r.Denied, ", ")
}

// Succeeded reports a run that executed cleanly to completion.
func (r Result) Succeeded() bool { return r.Subtype == "success" && !r.IsError }

// Faulted reports a failure the next launch would hit too: the service was
// unreachable, or the account is out of budget. A caller answers it by waiting
// or by stopping, never by reading the diff.
func (r Result) Faulted() bool { return r.Fault != FaultNone }

// Headline renders the one-line outcome a result header carries. The cost
// segment is dropped when the schema reports none: agy accounts for a run in
// tokens and never in dollars, and "$0" reads as a run that was free.
func (r Result) Headline() string {
	line := fmt.Sprintf("%s · error %t · %d turns", r.Subtype, r.IsError, r.NumTurns)
	if r.Cost > 0 {
		line += fmt.Sprintf(" · $%g", r.Cost)
	}
	return line
}

// Schema is one runner family's event stream: how every event of it names its
// own kind, which kind is the terminal one, and how to read that one.
//
// drover ships a schema per runner it knows rather than letting [runners] name
// the keys beside the argv, which would have been the smaller change. A log is
// read back by callers that never see the spec: Ran walks a directory by
// filename and `drover report` reads whichever log is newest, and neither
// records which binary wrote it. The filename cannot start carrying one without
// every log already on disk becoming unreadable. Matching on the event's own
// shape needs no such record, so one read serves a log written before a runner
// was declared and one written after.
//
// Kinds is what makes a foreign stream nameable rather than merely unreadable. A
// log holding events none of these schemas claims is a runner drover cannot
// read; a log of claimed events with no terminal one among them is a session
// that stopped. Both leave no result, and drive owes them different sentences.
//
// Locking on the key alone was the first design and it could not survive a
// third runner: claude and codex both name their kinds under "type", so claude
// claimed all 292 events of a codex review, found no terminal one among them,
// and drive reported a reviewer that had finished as one that died. Enumerating
// the kinds goes stale in the safe direction instead. An event kind a runner
// adds later is counted and unclaimed, which costs a Known and never the
// terminal read.
type Schema struct {
	// Name is the runner family, and what Result.Schema reports.
	Name string
	// Key is the field every event of this schema carries to name its kind:
	// "type" for claude and codex, "event" for agy.
	Key string
	// Kinds is every value Key carries in this schema's streams, and what
	// claims a line for it.
	Kinds []string
	// Terminal is the subset of Kinds that ends a session. A runner reporting
	// failure as its own event has more than one: codex ends on turn.completed,
	// turn.failed or turn.aborted, and reading only the first would report a
	// turn that failed as a session that stopped.
	Terminal []string
	// Read maps a terminal event onto a Result, and reports false for a line
	// that carries one of Terminal and will not decode.
	Read func(line []byte) (Result, bool)
	// Carry reads a non-terminal event for what the terminal one does not
	// carry. codex reports its outcome as a turn event and its final message as
	// an item event, so a Result read from the terminal line alone has no text
	// and a result written from it has no report.
	Carry func(line []byte, r *Result)
}

// claims reports whether an event of this kind belongs to this schema.
func (s Schema) claims(kind string) bool { return contains(s.Kinds, kind) }

// terminates reports whether an event of this kind ends a session.
func (s Schema) terminates(kind string) bool { return contains(s.Terminal, kind) }

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// Schemas is every event stream drover reads, tried in order. No two claim the
// same kind, which is what keeps one runner's reader off another's log.
func Schemas() []Schema {
	return []Schema{
		{
			Name: "claude", Key: "type",
			Kinds:    []string{"system", "assistant", "user", "tool_progress", "rate_limit_event", "result"},
			Terminal: []string{"result"},
			Read:     readClaude,
			Carry:    carryClaude,
		},
		{
			Name: "agy", Key: "event",
			Kinds:    []string{"init", "step_update", "result"},
			Terminal: []string{"result"},
			Read:     readAgy,
		},
		{
			Name: "codex", Key: "type",
			Kinds: []string{
				"thread.started", "turn.started", "turn.completed", "turn.failed", "turn.aborted",
				"item.started", "item.updated", "item.completed", "error",
			},
			Terminal: []string{"turn.completed", "turn.failed", "turn.aborted"},
			Read:     readCodex,
			Carry:    carryCodex,
		},
	}
}

// readClaude reads Claude Code's terminal event, which is flat.
//
// terminal_reason is the one field that separates a session ended by the API
// from one ended by its own work, and it is worth more than the subtype beside
// it: Claude Code reports an HTTP failure as subtype "success" with is_error
// true, so the subtype alone says a clean run. Across 243 logs on this machine
// it read "completed" 587 times and "api_error" on exactly the five sessions
// that died against the service.
//
// The api_retry events earlier in the same stream are deliberately not read.
// They report a retry, not a death: a run that met one 529 and went on to
// finish carries the same event as the run that met ten and gave up.
//
// Its denials name the tool rather than the action, and the list carries one
// entry per refused call, so the same tool appears as many times as it was
// asked for.
func readClaude(line []byte) (Result, bool) {
	var ev struct {
		Subtype  string  `json:"subtype"`
		IsError  bool    `json:"is_error"`
		NumTurns int     `json:"num_turns"`
		Cost     float64 `json:"total_cost_usd"`
		Text     string  `json:"result"`
		Terminal string  `json:"terminal_reason"`
		Denials  []struct {
			Tool string `json:"tool_name"`
		} `json:"permission_denials"`
		Usage      *APIUsage       `json:"usage"`
		ModelUsage json.RawMessage `json:"modelUsage"`
		SessionID  string          `json:"session_id"`
		DurationMS int64           `json:"duration_ms"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return Result{}, false
	}
	names := make([]string, 0, len(ev.Denials))
	for _, d := range ev.Denials {
		names = append(names, d.Tool)
	}
	r := Result{
		Subtype: ev.Subtype, IsError: ev.IsError, NumTurns: ev.NumTurns,
		Cost: ev.Cost, Text: ev.Text, Transport: ev.Terminal == "api_error",
		Denied: distinct(names), Model: firstKey(ev.ModelUsage),
		SessionID: ev.SessionID, DurationMS: ev.DurationMS,
	}
	if ev.Usage != nil {
		u := ev.Usage.Usage()
		r.Usage = &u
	}
	return r, true
}

// carryClaude keeps the fullest context any assistant message reported. Each
// content block of one message arrives as its own event carrying the same
// usage, so the repeats cannot raise the maximum.
func carryClaude(line []byte, r *Result) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage *APIUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" || ev.Message.Usage == nil {
		return
	}
	r.ContextMax = max(r.ContextMax, ev.Message.Usage.Context())
}

// firstKey is the first key of a JSON object as written, "" for anything else.
// Decoded token by token because a Go map forgets the order, and the first
// model modelUsage names is the one the session was launched on.
func firstKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	key, _ := tok.(string)
	return key
}

// readAgy reads agy's terminal event, which nests its payload under "result"
// and reports the outcome as a status rather than as a subtype and a flag:
// {"event":"result","result":{"status":"SUCCESS",...}}.
//
// The status is lowercased onto Subtype so Succeeded needs no schema of its
// own, and anything but SUCCESS sets IsError. agy's own failures arrive as
// status ERROR with the reason under "error" and no response at all, so the
// reason becomes Text where a response would have been. Cost stays zero: agy
// accounts for a run in tokens and never in dollars.
func readAgy(line []byte) (Result, bool) {
	var ev struct {
		Result struct {
			Status   string `json:"status"`
			Response string `json:"response"`
			Error    string `json:"error"`
			NumTurns int    `json:"num_turns"`
			Denials  []struct {
				Action  string `json:"action"`
				Display string `json:"display_name"`
			} `json:"denied_actions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Result.Status == "" {
		return Result{}, false
	}
	text := ev.Result.Response
	if text == "" {
		text = ev.Result.Error
	}
	// display_name is what the operator saw in the prompt they never got, so it
	// is the name to report; action is the coarse class behind it and stands in
	// for a stream that carries only that.
	names := make([]string, 0, len(ev.Result.Denials))
	for _, d := range ev.Result.Denials {
		if d.Display != "" {
			names = append(names, d.Display)
			continue
		}
		names = append(names, d.Action)
	}
	return Result{
		Subtype:  strings.ToLower(ev.Result.Status),
		IsError:  !strings.EqualFold(ev.Result.Status, "SUCCESS"),
		NumTurns: ev.Result.NumTurns,
		Text:     text,
		Denied:   distinct(names),
	}, true
}

// distinct keeps the first of each name and drops the empties. A session denied
// eleven Bash calls names one tool eleven times, and a report listing it eleven
// times buries the one other tool it also lost.
func distinct(all []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range all {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// readCodex reads codex's terminal turn events. Success carries only token
// usage, so the outcome is the event's own name and the reason for a failure is
// nested one level under it: {"type":"turn.failed","error":{"message":"..."}}.
//
// NumTurns is 1 on a completed turn because `codex exec` runs exactly one and
// reports no count of its own; 0 would render a session that ran as one that
// never started. Cost stays zero: codex accounts for a turn in tokens.
func readCodex(line []byte) (Result, bool) {
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return Result{}, false
	}
	switch ev.Type {
	case "turn.completed":
		return Result{Subtype: "success", NumTurns: 1}, true
	case "turn.failed", "turn.aborted":
		text := ev.Error.Message
		if text == "" {
			text = "codex reported " + ev.Type + " and no reason"
		}
		return Result{Subtype: strings.TrimPrefix(ev.Type, "turn."), IsError: true, Text: text}, true
	}
	return Result{}, false
}

// carryCodex keeps the latest agent message, which is the report a result
// wants and is never on the terminal event. A turn that failed states its own
// reason, so this only ever fills a gap rather than overwriting one.
func carryCodex(line []byte, r *Result) {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "item.completed" {
		return
	}
	if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
		r.Text = ev.Item.Text
	}
}

// Find returns every log for a base, newest first.
func Find(dir, base string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, base+".*.jsonl"))
	if err != nil {
		return nil, err
	}
	// The timestamp is fixed-width and lexically ordered, so sorting the names
	// beats stat-ing every file for its mtime.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	return matches, nil
}

// Latest returns the newest log for a base, or "" when there is none.
func Latest(dir, base string) (string, error) {
	all, err := Find(dir, base)
	if err != nil || len(all) == 0 {
		return "", err
	}
	return all[0], nil
}

// Read is what one log amounted to.
type Read struct {
	// Result is the terminal event, and Found whether any schema matched one.
	Result Result
	Found  bool
	// Events is how many of the log's lines are JSON objects, and Known how
	// many of those a schema in Schemas claims. Found false splits three ways
	// on them, and each way wants its own sentence: Events zero is a launch
	// that wrote nothing, Known zero with Events above it is a runner drover
	// cannot read, and Known above zero is a session that stopped before it
	// finished. The middle one had nowhere to go for as long as drover read one
	// schema, because the argv that made agy unreadable also stopped it writing
	// anything, so every case arrived wearing the words of a session that died.
	Events int
	Known  int
	// SessionID is the first session_id any event carried, which a killed
	// session's log names from its first line though it never reached a result.
	SessionID string
}

// ReadResult returns what one log carries: its terminal event under whichever
// schema recognizes it, and what the rest of the lines were regardless.
//
// A killed run leaves a truncated final line, so the scan keeps the last line
// that parses rather than reading only the final one. Reading only the final
// line, as the shell driver did, reports an interrupted run as having no result
// at all.
func ReadResult(path string) (Read, error) {
	f, err := os.Open(path)
	if err != nil {
		return Read{}, err
	}
	defer func() { _ = f.Close() }()

	var rd Read
	schemas := Schemas()
	// What a schema reads off events before the terminal one. Held apart from
	// rd.Result so a stream that never ends reports no result at all.
	var carried Result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Decoded to the keys rather than validated, because the kind every
		// schema names its events by is one of them. A truncated final line
		// counts as neither: Events has to mean what the runner emitted, since
		// drive reads it as proof the runner ran at all.
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		rd.Events++
		if rd.SessionID == "" {
			if raw, ok := ev["session_id"]; ok {
				_ = json.Unmarshal(raw, &rd.SessionID)
			}
		}
		for _, s := range schemas {
			kind, ok := eventKind(ev, s.Key)
			if !ok || !s.claims(kind) {
				continue
			}
			rd.Known++
			switch {
			case s.terminates(kind):
				if r, matched := s.Read([]byte(line)); matched {
					r.Schema = s.Name
					rd.Result, rd.Found = Classify(r), true
				}
			case s.Carry != nil:
				s.Carry([]byte(line), &carried)
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		return Read{}, fmt.Errorf("read %s: %w", path, err)
	}
	if rd.Found {
		rd.Result.ContextMax = carried.ContextMax
	}
	// Classified again once the carried text lands: codex states a failure's
	// reason on the terminal event and its report on an item event, so a
	// terminal event that carried neither is only nameable from what Carry kept.
	if rd.Found && rd.Result.Text == "" {
		rd.Result.Text = carried.Text
		rd.Result = Classify(rd.Result)
	}
	return rd, nil
}

// eventKind is the string a decoded event carries at key, and false when the
// key is absent or holds anything else.
func eventKind(ev map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := ev[key]
	if !ok {
		return "", false
	}
	var kind string
	if json.Unmarshal(raw, &kind) != nil || kind == "" {
		return "", false
	}
	return kind, true
}

// Ran reports whether any of an item's logs ended in a successful result.
//
// "Ran" means executed clean and awaiting validation, not merged. An
// interrupted run has no such event and stays READY rather than falsely
// claiming it is ready for a PR.
func Ran(dir, base string) bool {
	logs, err := Find(dir, base)
	if err != nil {
		return false
	}
	for _, l := range logs {
		if rd, err := ReadResult(l); err == nil && rd.Found && rd.Result.Succeeded() {
			return true
		}
	}
	return false
}
