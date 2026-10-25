package runlog

import (
	"encoding/json"
	"strings"
)

// The kinds of Activity.
const (
	ActivityTool = "tool"
	ActivityText = "text"
)

// summaryRunes bounds an Activity summary, which is one terminal line under an
// item rather than a record of what the session did: the log keeps that.
const summaryRunes = 160

// Activity is one thing a session did, read from one line of its log: a tool
// it called, or text it wrote to the operator.
type Activity struct {
	// Kind is ActivityTool or ActivityText.
	Kind string
	// Tool is the tool's name, on ActivityTool alone.
	Tool string
	// Summary is one line: the tool's name and its most telling input, or the
	// text, with every run of whitespace collapsed to one space and cut at
	// summaryRunes followed by "...".
	Summary string
}

// Activities is what one log line says the session did, from whichever schema
// claims it. A line no schema claims, one that does not decode, and one whose
// schema reads no activity all yield nothing: a log is read while its writer
// is still writing it, so a line that does not parse is the ordinary case.
func Activities(line []byte) []Activity {
	var ev map[string]json.RawMessage
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	for _, s := range Schemas() {
		kind, ok := eventKind(ev, s.Key)
		if !ok || !s.claims(kind) {
			continue
		}
		if s.Activity == nil {
			return nil
		}
		return s.Activity(line)
	}
	return nil
}

// toolActivity names a tool and the first of its inputs that is present.
func toolActivity(name string, input map[string]any, keys ...string) Activity {
	summary := name
	for _, k := range keys {
		if v, ok := input[k].(string); ok && v != "" {
			summary += " " + v
			break
		}
	}
	return Activity{Kind: ActivityTool, Tool: name, Summary: oneLine(summary)}
}

func textActivity(text string) (Activity, bool) {
	s := oneLine(text)
	return Activity{Kind: ActivityText, Summary: s}, s != ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > summaryRunes {
		return string(r[:summaryRunes]) + "..."
	}
	return s
}

// claudeActivity reads an assistant event's content blocks. Claude Code writes
// each block of a message as its own event, so one line is usually one entry;
// thinking blocks are skipped, because what a session thinks is not what it
// did.
func claudeActivity(line []byte) []Activity {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
				Text  string         `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" {
		return nil
	}
	var out []Activity
	for _, b := range ev.Message.Content {
		switch b.Type {
		case "tool_use":
			out = append(out, toolActivity(b.Name, b.Input, "command", "file_path", "pattern", "url"))
		case "text":
			if a, ok := textActivity(b.Text); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// codexActivity reads codex's items. A command or a file change is read when
// it starts, since that is when the session is doing it, and an agent message
// when it completes, since only the completed item carries the whole text.
func codexActivity(line []byte) []Activity {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Text    string `json:"text"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch {
	case ev.Type == "item.started" && ev.Item.Type == "command_execution":
		return []Activity{toolActivity(ev.Item.Type, map[string]any{"command": ev.Item.Command}, "command")}
	case ev.Type == "item.started" && ev.Item.Type == "file_change":
		paths := make([]string, 0, len(ev.Item.Changes))
		for _, c := range ev.Item.Changes {
			paths = append(paths, c.Path)
		}
		return []Activity{toolActivity(ev.Item.Type, map[string]any{"paths": strings.Join(paths, ", ")}, "paths")}
	case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
		if a, ok := textActivity(ev.Item.Text); ok {
			return []Activity{a}
		}
	}
	return nil
}

// agyActivity reads the step agy opens for each tool call, which it writes
// once as ACTIVE and once as DONE; the first is read. agy streams its text as
// fragments of a step and never as a whole message until the terminal event,
// so it yields no text entries.
func agyActivity(line []byte) []Activity {
	var ev struct {
		Event string `json:"event"`
		Step  struct {
			State    string `json:"state"`
			StepType string `json:"step_type"`
			ToolName string `json:"tool_name"`
			ToolInfo struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"tool_info"`
		} `json:"step_update"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Event != "step_update" ||
		ev.Step.StepType != "tool" || ev.Step.State != "ACTIVE" || ev.Step.ToolName == "" {
		return nil
	}
	return []Activity{toolActivity(ev.Step.ToolName, ev.Step.ToolInfo.Parameters,
		"CommandLine", "AbsolutePath", "TargetFile", "Pattern", "Query", "Url")}
}
