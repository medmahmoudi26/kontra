package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/mcpwire"
)

// M4 (WP-07): clients of the gate's own /mcp (spec/relay.md §12) through a replica, on raw JSON as the relay is: a
// 2025-11-25 session declaring exactly the capabilities a test gives (GateSession: form and URL elicitation are what
// §12.3 and §12.4 read), and 2026-07-28 requests with no session whose per-request capabilities a test gives
// (GateModern). The answers are read into the shapes §12.2, §12.3 and §12.5 define (ToolResult, ModernTask).

// GateSession is a 2025-11-25 session of /mcp.
type GateSession struct {
	t *testing.T
	*MCPClient
	n int
}

// OpenGateSession initializes a session of g's /mcp with key, declaring caps (JSON; "{}" declares nothing).
func OpenGateSession(t *testing.T, ctx context.Context, g *Gate, key, caps string) *GateSession {
	t.Helper()
	c := NewMCPClient(g.URL+gatemcp.Path, key)
	c.Capabilities = caps
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("/mcp initialize on %s: %v", g.Name, err)
	}
	return &GateSession{t: t, MCPClient: c}
}

// On is the same session on another replica (any replica serves any request of an /mcp session, §12).
func (s *GateSession) On(g *Gate) *GateSession {
	return &GateSession{t: s.t, MCPClient: s.At(g.URL + gatemcp.Path)}
}

// Rpc sends a request of method with params (a JSON object) and returns the whole answer.
func (s *GateSession) Rpc(method, params string) *MCPResponse {
	s.t.Helper()
	s.n++
	r, err := s.Post(context.Background(), fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, s.n, method, params))
	if err != nil {
		s.t.Fatalf("%s: %v", method, err)
	}
	return r
}

// Tool calls a tool with args (a JSON object), plain.
func (s *GateSession) Tool(tool, args string) *MCPResponse {
	s.t.Helper()
	return s.Rpc("tools/call", `{"name":"`+tool+`","arguments":`+args+`}`)
}

// ToolTask calls a tool with args, task-augmented (params.task, §11.6).
func (s *GateSession) ToolTask(tool, args string) *MCPResponse {
	s.t.Helper()
	return s.Rpc("tools/call", `{"name":"`+tool+`","arguments":`+args+`,"task":{"ttl":600000}}`)
}

// Raw posts a message as it is (a client's response to a gate request, a notification) and returns the status.
func (s *GateSession) Raw(body string) int {
	s.t.Helper()
	r, err := s.Post(context.Background(), body)
	if err != nil {
		s.t.Fatalf("post: %v", err)
	}
	return r.Status
}

// StreamCollector holds the messages a GET stream of a session received.
type StreamCollector struct {
	mu   sync.Mutex
	msgs []string
	ids  int // events that carried an id: (a /mcp GET stream carries none, §12.3)
	stop context.CancelFunc
	done chan struct{}
}

// Listen opens the session's GET stream on its replica and collects its messages until Stop.
func (s *GateSession) Listen(ctx context.Context) *StreamCollector {
	s.t.Helper()
	cctx, cancel := context.WithCancel(ctx)
	st, err := s.Open(cctx, http.MethodGet, "", map[string]string{"Accept": "text/event-stream"})
	if err != nil || st.Status != http.StatusOK {
		cancel()
		s.t.Fatalf("the GET stream of /mcp: %v %v", st, err)
	}
	c := &StreamCollector{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			ev, err := st.Next(cctx)
			if err != nil {
				return
			}
			c.mu.Lock()
			if ev.ID != "" {
				c.ids++
			}
			if strings.TrimSpace(ev.Data) != "" {
				c.msgs = append(c.msgs, ev.Data)
			}
			c.mu.Unlock()
		}
	}()
	return c
}

// Messages are the messages received so far.
func (c *StreamCollector) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

// EventIDs is how many events carried an id.
func (c *StreamCollector) EventIDs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ids
}

// Stop closes the stream.
func (c *StreamCollector) Stop() {
	c.stop()
	<-c.done
}

// GateModern sends 2026-07-28 requests to /mcp: no session; every request carries MCP-Protocol-Version, Mcp-Method,
// Mcp-Name when the method has one, and the version and Caps in params._meta.
type GateModern struct {
	t    *testing.T
	URL  string
	Key  string
	Caps string // the per-request clientCapabilities, JSON
	n    int
}

// Capabilities a modern request may declare.
const (
	CapsTasks      = `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`
	CapsTasksForm  = `{"extensions":{"io.modelcontextprotocol/tasks":{}},"elicitation":{"form":{}}}`
	CapsTasksBoth  = `{"extensions":{"io.modelcontextprotocol/tasks":{}},"elicitation":{"form":{},"url":{}}}`
	CapsNothing    = `{}`
	CapsFormOnly   = `{"elicitation":{"form":{}}}`
	CapsURLAndForm = `{"elicitation":{"url":{},"form":{}}}`
)

// NewGateModern is a modern client of g's /mcp.
func NewGateModern(t *testing.T, g *Gate, key, caps string) *GateModern {
	return &GateModern{t: t, URL: g.URL + gatemcp.Path, Key: key, Caps: caps}
}

// On is the client against another replica.
func (m *GateModern) On(g *Gate) *GateModern {
	cp := *m
	cp.URL = g.URL + gatemcp.Path
	return &cp
}

// With is the client declaring other capabilities.
func (m *GateModern) With(caps string) *GateModern {
	cp := *m
	cp.Caps = caps
	return &cp
}

// Body is a modern request's JSON: params (members without braces) and the _meta 2026-07-28 requires.
func (m *GateModern) Body(method, params string) string {
	m.n++
	meta := `"_meta":{"` + mcpwire.MetaProtocolVersion + `":"2026-07-28","` + mcpwire.MetaClientCapabilities + `":` + m.Caps + `}`
	if params != "" {
		params += ","
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s%s}}`, m.n, method, params, meta)
}

// Post sends body with the modern headers (Mcp-Name = name when not ""; hdr overrides, "" removes) on ctx.
func (m *GateModern) Post(ctx context.Context, method, name, body string, hdr map[string]string) (*MCPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if m.Key != "" {
		req.Header.Set("Authorization", "Bearer "+m.Key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpwire.HeaderProtocolVersion, "2026-07-28")
	req.Header.Set(mcpwire.HeaderMethod, method)
	if name != "" {
		req.Header.Set(mcpwire.HeaderName, name)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return do(&http.Client{Timeout: 2 * time.Minute}, req)
}

// Rpc sends method with params (members, without braces); name is its Mcp-Name.
func (m *GateModern) Rpc(method, name, params string) *MCPResponse {
	m.t.Helper()
	r, err := m.Post(context.Background(), method, name, m.Body(method, params), nil)
	if err != nil {
		m.t.Fatalf("%s: %v", method, err)
	}
	if sid := r.Header.Get(mcpwire.HeaderSessionID); sid != "" {
		m.t.Errorf("a modern answer named a session: %s", sid)
	}
	return r
}

// Tool calls a tool.
func (m *GateModern) Tool(tool, args string) *MCPResponse {
	m.t.Helper()
	return m.Rpc("tools/call", tool, `"name":"`+tool+`","arguments":`+args)
}

// --- the answers ---

// AnswerRaw is the response message of r as JSON: the body, or the stream's last message without a method.
func AnswerRaw(r *MCPResponse) json.RawMessage {
	if r.Body != nil {
		return json.RawMessage(r.Body)
	}
	var last json.RawMessage
	for _, e := range r.Events {
		m := e.Message()
		if _, isReq := m["method"]; !isReq && m != nil {
			last = json.RawMessage(e.Data)
		}
	}
	return last
}

// RPCAnswer is a JSON-RPC response.
type RPCAnswer struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// Answer decodes r's response message.
func Answer(t *testing.T, r *MCPResponse) RPCAnswer {
	t.Helper()
	var a RPCAnswer
	if raw := AnswerRaw(r); raw != nil {
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("not a JSON-RPC response: %s", r.Text())
		}
	}
	return a
}

// ErrCode is r's JSON-RPC error code, 0 when it is no error.
func ErrCode(t *testing.T, r *MCPResponse) int {
	t.Helper()
	if a := Answer(t, r); a.Error != nil {
		return a.Error.Code
	}
	return 0
}

// ToolResult is a CallToolResult (or a CreateTaskResult) as the tests read it.
type ToolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		URI      string `json:"uri"`
		Resource *struct {
			URI      string `json:"uri"`
			Blob     string `json:"blob"`
			MimeType string `json:"mimeType"`
		} `json:"resource"`
	} `json:"content"`
	Structured *struct {
		TaskID string `json:"taskId"`
		State  string `json:"state"`
		URL    string `json:"url"`
		Quote  *struct {
			ApproveURL string `json:"approveUrl"`
			ExpiresAt  string `json:"expiresAt"`
		} `json:"quote"`
		Text string `json:"text"`
		Data []any  `json:"data"`
	} `json:"structuredContent"`
	StructuredRaw json.RawMessage `json:"-"`
	IsError       bool            `json:"isError"`
	Meta          map[string]any  `json:"_meta"`
	ResultType    string          `json:"resultType"`
	Task          *struct {
		TaskID        string `json:"taskId"`
		Status        string `json:"status"`
		StatusMessage string `json:"statusMessage"`
		PollInterval  int    `json:"pollInterval"`
	} `json:"task"`
}

// Tool decodes r as a tool result; a JSON-RPC error, or no result, fails the test.
func Tool(t *testing.T, r *MCPResponse) ToolResult {
	t.Helper()
	a := Answer(t, r)
	if a.Error != nil || a.Result == nil {
		t.Fatalf("not a result (%d): %s", r.Status, r.Text())
	}
	var tr ToolResult
	if err := json.Unmarshal(a.Result, &tr); err != nil {
		t.Fatalf("not a tool result: %s", a.Result)
	}
	var raw struct {
		S json.RawMessage `json:"structuredContent"`
	}
	_ = json.Unmarshal(a.Result, &raw)
	tr.StructuredRaw = raw.S
	return tr
}

// Text is a tool result's text content, joined.
func (r ToolResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// TaskID is the structured result's taskId ("" without one).
func (r ToolResult) TaskID() string {
	if r.Structured == nil {
		return ""
	}
	return r.Structured.TaskID
}

// State is the structured result's state ("" without one).
func (r ToolResult) State() string {
	if r.Structured == nil {
		return ""
	}
	return r.Structured.State
}

// ModernTask is a CreateTaskResult or a modern tasks/get result.
type ModernTask struct {
	ResultType     string                     `json:"resultType"`
	TaskID         string                     `json:"taskId"`
	Status         string                     `json:"status"`
	StatusMessage  string                     `json:"statusMessage"`
	PollIntervalMs int                        `json:"pollIntervalMs"`
	InputRequests  map[string]json.RawMessage `json:"inputRequests"`
	Result         json.RawMessage            `json:"result"`
	Meta           map[string]any             `json:"_meta"`
}

// Task decodes r as a modern task.
func Task(t *testing.T, r *MCPResponse) ModernTask {
	t.Helper()
	a := Answer(t, r)
	if a.Error != nil || a.Result == nil {
		t.Fatalf("not a result (%d): %s", r.Status, r.Text())
	}
	var m ModernTask
	if err := json.Unmarshal(a.Result, &m); err != nil {
		t.Fatalf("not a task: %s", a.Result)
	}
	return m
}

// LegacyTask is a 2025-11-25 tasks/get result or a CreateTaskResult's task.
type LegacyTask struct {
	TaskID        string         `json:"taskId"`
	Status        string         `json:"status"`
	StatusMessage string         `json:"statusMessage"`
	Meta          map[string]any `json:"_meta"`
}

// GetTask decodes r as a legacy tasks/get result.
func GetTask(t *testing.T, r *MCPResponse) LegacyTask {
	t.Helper()
	a := Answer(t, r)
	if a.Error != nil || a.Result == nil {
		t.Fatalf("not a result (%d): %s", r.Status, r.Text())
	}
	var lt LegacyTask
	if err := json.Unmarshal(a.Result, &lt); err != nil {
		t.Fatalf("not a task: %s", a.Result)
	}
	return lt
}
