package runlog

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf8"
)

// TestActivitiesReadsEachSchema drives every schema with lines from a real
// log, keyed by line number. A line absent from want yields nothing: Claude's
// system, thinking and tool_result events, codex's thread and turn events and
// the completion of an item already read when it started, and agy's every
// step but an opening tool one.
func TestActivitiesReadsEachSchema(t *testing.T) {
	cases := []struct {
		file string
		want map[int][]Activity
	}{
		{"claude-activity.jsonl", map[int][]Activity{
			3: {{Kind: ActivityTool, Tool: "Bash", Summary: `Bash python3 - <<'EOF' p='internal/runlog/fault.go' s=open(p).read() old=s[s.index("// clock12 turns"):s.index("var months")] new='''// clock12 turns an hour wi...`}},
			5: {{Kind: ActivityTool, Tool: "Read", Summary: "Read /work/b83/internal/cmd/handoff.go"}},
			6: {{Kind: ActivityTool, Tool: "ToolSearch", Summary: "ToolSearch"}},
			8: {{Kind: ActivityText, Summary: "The monitor is watching the verify run. I'll commit once it exits 0."}},
			9: {{Kind: ActivityText, Summary: "Done: F14 correction pass. CI's lint failure is fixed and committed as `d9108ba` on `feature/f14-drover-sends-needs-operator-events-to-an-ntfy-se`. Nothing push..."}},
		}},
		{"codex-activity.jsonl", map[int][]Activity{
			3: {{Kind: ActivityText, Summary: "I’ll trace all six requirements against B32’s current tip, check the full diff and saved guarantee, then rerun the gate. I’ll record the audit and clear the pri..."}},
			4: {{Kind: ActivityTool, Tool: "command_execution", Summary: "command_execution /bin/zsh -lc 'drover review B32'"}},
			6: {{Kind: ActivityTool, Tool: "file_change", Summary: "file_change docs-internal/verdicts/B32-fifth-audit-clear.md, docs-internal/verdicts/B32-fifth-audit.md"}},
		}},
		{"agy-stream.jsonl", map[int][]Activity{
			5: {{Kind: ActivityTool, Tool: "run_command", Summary: "run_command echo hello"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", c.file))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 1<<20)
			n := 0
			for sc.Scan() {
				n++
				got := Activities(sc.Bytes())
				if !reflect.DeepEqual(got, c.want[n]) {
					t.Errorf("line %d: got %#v, want %#v", n, got, c.want[n])
				}
				for _, a := range got {
					if r := utf8.RuneCountInString(a.Summary); r > summaryRunes+3 {
						t.Errorf("line %d: summary is %d runes", n, r)
					}
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestActivitiesClaudeInputOrder pins which input names a Claude tool call
// when several are present, and that a line nothing claims is not an error.
func TestActivitiesClaudeInputOrder(t *testing.T) {
	cases := []struct {
		line string
		want []Activity
	}{
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"path":"x","pattern":"a\nb"}}]}}`,
			[]Activity{{Kind: ActivityTool, Tool: "Grep", Summary: "Grep a b"}}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"WebFetch","input":{"url":"https://example.com","prompt":"p"}}]}}`,
			[]Activity{{Kind: ActivityTool, Tool: "WebFetch", Summary: "WebFetch https://example.com"}}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go","pattern":"p"}},{"type":"text","text":"  "}]}}`,
			[]Activity{{Kind: ActivityTool, Tool: "Edit", Summary: "Edit a.go"}}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"X"`, nil},
		{`not json`, nil},
		{`{"type":"something_new"}`, nil},
	}
	for _, c := range cases {
		if got := Activities([]byte(c.line)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.line, got, c.want)
		}
	}
}
