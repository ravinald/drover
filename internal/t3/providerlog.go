package t3

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravinald/drover/internal/runlog"
)

// SDKProtocol is the protocol T3 Code's Claude driver tags each provider log
// line with. Its payloads are the Claude Agent SDK's own events, the shape a
// headless `claude` log carries.
const SDKProtocol = "claude-agent-sdk.query"

// ProviderLogPath is where T3 Code writes one thread's provider event log: the
// thread id with every ":" as "-". Observed on T3 Code 0.1.2 and documented
// nowhere, which is why a missing file is reported rather than assumed away.
func ProviderLogPath(dir, threadID string) string {
	return filepath.Join(dir, "events."+strings.ReplaceAll(threadID, ":", "-")+".log")
}

// sdkResult is the fields of the SDK's terminal event the bridge's own result
// line carries.
type sdkResult struct {
	NumTurns   int              `json:"num_turns"`
	Cost       float64          `json:"total_cost_usd"`
	Usage      *runlog.APIUsage `json:"usage"`
	SessionID  string           `json:"session_id"`
	DurationMS int64            `json:"duration_ms"`
	ModelUsage json.RawMessage  `json:"modelUsage"`
}

// providerLog tails one thread's provider event log from its first line. Each
// line is "[<time>] <tag>: <json>"; the tag was NTIVE on 0.1.2, and nothing
// here depends on it.
type providerLog struct {
	path    string
	f       *os.File
	rd      *bufio.Reader
	partial []byte
	openErr error

	lines, unparsed int
	sawRate         bool
	result          *sdkResult
}

// poll reads every complete line written since the last call and hands each
// SDK rate_limit_event and result payload to emit, unchanged. A file that does
// not exist yet is not an error until the run is over: T3 opens it when the
// provider session starts, which can be after the launch returns.
func (p *providerLog) poll(emit func(json.RawMessage)) {
	if p.f == nil {
		f, err := os.Open(p.path)
		if err != nil {
			p.openErr = err
			return
		}
		p.f, p.rd, p.openErr = f, bufio.NewReaderSize(f, 64*1024), nil
	}
	for {
		chunk, err := p.rd.ReadBytes('\n')
		p.partial = append(p.partial, chunk...)
		if err != nil {
			// A line without its newline is still being written; keep it and
			// read the rest next time.
			if !errors.Is(err, io.EOF) {
				p.openErr = err
			}
			return
		}
		line := bytes.TrimSpace(p.partial)
		p.partial = nil
		if len(line) == 0 {
			continue
		}
		p.lines++
		payload, ok := sdkPayload(line)
		if !ok {
			p.unparsed++
			continue
		}
		if payload == nil {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &head)
		switch head.Type {
		case "rate_limit_event":
			p.sawRate = true
			emit(payload)
		case "result":
			var r sdkResult
			if json.Unmarshal(payload, &r) == nil {
				p.result = &r
			}
			emit(payload)
		}
	}
}

// sdkPayload is the Claude SDK payload one log line carries: ok false for a
// line that is not "[time] tag: {json}", and a nil payload for a well-formed
// line of another protocol.
func sdkPayload(line []byte) (json.RawMessage, bool) {
	if len(line) == 0 || line[0] != '[' {
		return nil, false
	}
	end := bytes.IndexByte(line, ']')
	if end < 0 {
		return nil, false
	}
	rest := line[end+1:]
	colon := bytes.Index(rest, []byte(": "))
	if colon < 0 {
		return nil, false
	}
	var env struct {
		Protocol string `json:"protocol"`
		Event    struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"event"`
	}
	if json.Unmarshal(rest[colon+2:], &env) != nil {
		return nil, false
	}
	if env.Protocol != SDKProtocol || len(env.Event.Payload) == 0 {
		return nil, true
	}
	var compact bytes.Buffer
	if json.Compact(&compact, env.Event.Payload) != nil {
		return nil, false
	}
	return compact.Bytes(), true
}

// unavailable is why the log yielded no usage, empty when it yielded the SDK's
// result.
func (p *providerLog) unavailable() string {
	switch {
	case p.result != nil:
		return ""
	case errors.Is(p.openErr, fs.ErrNotExist):
		return fmt.Sprintf("the provider event log %s does not exist: a provider other than Claude, or a T3 Code release that moved it", p.path)
	case p.openErr != nil:
		return fmt.Sprintf("the provider event log %s would not read: %v", p.path, p.openErr)
	case p.lines == 0:
		return fmt.Sprintf("the provider event log %s is empty", p.path)
	case p.unparsed == p.lines:
		return fmt.Sprintf("none of the %d lines of the provider event log %s parsed as \"[time] TAG: {json}\": T3 Code may have changed its format", p.lines, p.path)
	}
	what := "no " + SDKProtocol + " result"
	if !p.sawRate {
		what += " and no rate_limit_event"
	}
	return fmt.Sprintf("the provider event log %s held %d lines and %s", p.path, p.lines, what)
}

func (p *providerLog) close() {
	if p.f != nil {
		_ = p.f.Close()
	}
}
