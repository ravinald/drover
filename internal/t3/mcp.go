// Package t3 is drover's client for a T3 Code server: the OAuth grant an
// outside agent signs in with, the MCP calls that launch and follow a thread,
// and the bridge that turns one followed thread into a stream-json log runlog
// reads.
//
// It speaks MCP over streamable HTTP rather than T3 Code's WebSocket RPC. The
// RPC is the app's internal client contract, typed in TypeScript and versioned
// with each release; MCP is the surface T3 Code documents for outside agents,
// with OAuth and revocation built in.
package t3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the MCP revision drover asks for in initialize.
const ProtocolVersion = "2025-06-18"

// ErrUnauthorized is a 401 from the server: no token, an expired one, or a grant
// revoked in Settings > Connections.
var ErrUnauthorized = errors.New("the T3 Code server answered 401 Unauthorized: run `drover t3 login` to sign drover in again")

// ToolError is a tool call the server refused, in the OrchestratorMcpFailure
// shape: a machine code (capability_denied, thread_not_found, ...) and the
// server's own sentence.
type ToolError struct {
	Tool    string
	Code    string
	Message string
}

func (e *ToolError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("%s refused: %s", e.Tool, e.Message)
	}
	return fmt.Sprintf("%s refused with %s: %s", e.Tool, e.Code, e.Message)
}

// Code is the refusal code a call failed with, empty for any other error.
func Code(err error) string {
	var te *ToolError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}

// ServerInfo is what initialize reports about the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is one entry of tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Client is one MCP session against one endpoint. It is safe for concurrent
// calls, which the bridge makes when it interrupts a run while a wait is in
// flight.
type Client struct {
	URL   string
	Token func(context.Context) (string, error)
	HTTP  *http.Client

	ids     atomic.Int64
	mu      sync.Mutex
	session string
	inited  bool
	server  ServerInfo
}

// NewClient is a client for url, authorizing each request with what token
// returns.
func NewClient(url string, token func(context.Context) (string, error)) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Initialize opens the MCP session and returns what the server says it is.
// Every other call runs it first, once.
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	c.mu.Lock()
	if c.inited {
		defer c.mu.Unlock()
		return c.server, nil
	}
	c.mu.Unlock()
	var res struct {
		ServerInfo ServerInfo `json:"serverInfo"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "drover", "version": "1"},
	}
	if err := c.rpc(ctx, "initialize", params, &res); err != nil {
		return ServerInfo{}, err
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return ServerInfo{}, err
	}
	c.mu.Lock()
	c.inited, c.server = true, res.ServerInfo
	c.mu.Unlock()
	return res.ServerInfo, nil
}

// ListTools is the server's whole catalog, every page of it.
func (c *Client) ListTools(ctx context.Context) ([]Tool, json.RawMessage, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, nil, err
	}
	var all []Tool
	var pages []json.RawMessage
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var raw json.RawMessage
		if err := c.rpc(ctx, "tools/list", params, &raw); err != nil {
			return nil, nil, err
		}
		pages = append(pages, raw)
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, nil, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	if len(pages) == 1 {
		return all, pages[0], nil
	}
	merged, _ := json.Marshal(map[string]any{"tools": all})
	return all, merged, nil
}

// Call runs one tool and decodes its payload into out, which may be nil. A
// refusal comes back as a *ToolError.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any, out any) error {
	if _, err := c.Initialize(ctx); err != nil {
		return err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &res); err != nil {
		return err
	}
	payload := res.Structured
	var text string
	for _, ct := range res.Content {
		if ct.Type == "text" {
			text = ct.Text
			break
		}
	}
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage(text)
	}
	if fail, ok := failure(tool, payload); ok {
		return fail
	}
	if res.IsError {
		return &ToolError{Tool: tool, Message: strings.TrimSpace(text)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s answered %q, which is not the JSON drover reads: %w", tool, trimTo(string(payload), 200), err)
	}
	return nil
}

// failure reads the server's refusal shape out of a payload, whether it came
// flagged isError or not.
func failure(tool string, payload json.RawMessage) (*ToolError, bool) {
	var f struct {
		Tag     string `json:"_tag"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &f) != nil || !strings.HasSuffix(f.Tag, "Failure") {
		return nil, false
	}
	return &ToolError{Tool: tool, Code: f.Code, Message: f.Message}, true
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (c *Client) rpc(ctx context.Context, method string, params any, out any) error {
	id := c.ids.Add(1)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}
	msg, err := readResponse(resp, id)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if msg.Error != nil {
		return fmt.Errorf("%s: the server answered JSON-RPC error %d: %s", method, msg.Error.Code, msg.Error.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Result, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := c.post(ctx, body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// post sends one JSON-RPC message, and maps the statuses a caller acts on
// differently from the rest: 401 is the grant, anything else not 2xx is the
// server.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	c.mu.Unlock()
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_ = resp.Body.Close()
		return nil, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the server answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// readResponse finds the response to id in a reply that is either one JSON
// object or an event stream. A stream may carry the server's own notifications
// and requests ahead of the response; none of them is anything the bridge
// answers, so they are skipped.
func readResponse(resp *http.Response, id int64) (rpcResponse, error) {
	want := fmt.Sprint(id)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var msg rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
			return rpcResponse{}, fmt.Errorf("the reply is not JSON-RPC: %w", err)
		}
		return msg, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data strings.Builder
	flush := func() (rpcResponse, bool) {
		defer data.Reset()
		var msg rpcResponse
		if data.Len() == 0 || json.Unmarshal([]byte(data.String()), &msg) != nil {
			return rpcResponse{}, false
		}
		return msg, string(msg.ID) == want
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if msg, ok := flush(); ok {
				return msg, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return rpcResponse{}, err
	}
	return rpcResponse{}, errors.New("the event stream ended before the server answered")
}
