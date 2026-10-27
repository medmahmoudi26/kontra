package e2e

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
	"time"

	"kontra.sh/kontra/internal/mcpwire"
)

// The M3 tests' MCP clients (WP-09), on raw JSON as the relay itself is (spec/relay.md §11): MCPClient speaks
// 2025-11-25 (initialize, Mcp-Session-Id, event streams with their ids, a GET stream and Last-Event-ID, tasks/*,
// answers to gate request ids, DELETE), and ModernClient speaks 2026-07-28 (no session; _meta, MCP-Protocol-Version,
// Mcp-Method and Mcp-Name on every request; tasks/get, tasks/update and tasks/cancel). Either talks to a replica's
// /mcp/{svc} with an API key, or to a hiring node's local /mcp/{svc} with none.

// SSEEvent is one event of an event stream.
type SSEEvent struct {
	ID    string
	Event string
	Data  string
}

// MCPResponse is the answer to one POST: its status and headers, and either a JSON body or the events of a stream.
type MCPResponse struct {
	Status int
	Header http.Header
	Body   []byte     // a JSON body as received (nil for an event stream)
	Events []SSEEvent // an event stream's events, in order
}

// Messages are the JSON-RPC messages the response carries: the JSON body, or every data event.
func (r *MCPResponse) Messages() []map[string]any {
	var out []map[string]any
	if r.Body != nil {
		var m map[string]any
		if json.Unmarshal(r.Body, &m) == nil {
			out = append(out, m)
		}
		return out
	}
	for _, e := range r.Events {
		var m map[string]any
		if e.Data != "" && json.Unmarshal([]byte(e.Data), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// Answer is the response message (a result or an error, no method): the JSON body's, or the stream's last one.
func (r *MCPResponse) Answer() map[string]any {
	var last map[string]any
	for _, m := range r.Messages() {
		if _, isReq := m["method"]; !isReq {
			last = m
		}
	}
	return last
}

// Text is the whole response as text, for logs and for "contains" checks.
func (r *MCPResponse) Text() string {
	if r.Body != nil {
		return string(r.Body)
	}
	var b strings.Builder
	for _, e := range r.Events {
		fmt.Fprintf(&b, "id:%s event:%s data:%s\n", e.ID, e.Event, e.Data)
	}
	return b.String()
}

// Result is the answer's result object, nil when there is none.
func (r *MCPResponse) Result() map[string]any {
	res, _ := r.Answer()["result"].(map[string]any)
	return res
}

// Error is the answer's error code and message; ok is false when the answer is no error.
func (r *MCPResponse) Error() (code int, message string, data map[string]any, ok bool) {
	e, _ := r.Answer()["error"].(map[string]any)
	if e == nil {
		return 0, "", nil, false
	}
	c, _ := e["code"].(float64)
	m, _ := e["message"].(string)
	d, _ := e["data"].(map[string]any)
	return int(c), m, d, true
}

// ToolText is a CallToolResult's text content, joined, and its isError.
func (r *MCPResponse) ToolText() (string, bool) {
	res := r.Result()
	var b strings.Builder
	content, _ := res["content"].([]any)
	for _, c := range content {
		if c, ok := c.(map[string]any); ok {
			s, _ := c["text"].(string)
			b.WriteString(s)
		}
	}
	isErr, _ := res["isError"].(bool)
	return b.String(), isErr
}

// MCPClient is a 2025-11-25 client.
type MCPClient struct {
	Endpoint string // http://127.0.0.1:<port>/mcp/<svc>
	Key      string // the API key; "" sends no Authorization (a hiring node's local endpoint)
	SID      string // the gate's Mcp-Session-Id, once initialize has answered
	Version  string // MCP-Protocol-Version on requests after initialize (default 2025-11-25)
	// Capabilities are the client's initialize capabilities (default {"elicitation":{}}).
	Capabilities string
	HTTP         *http.Client
}

// NewMCPClient is a client of endpoint with key.
func NewMCPClient(endpoint, key string) *MCPClient {
	return &MCPClient{Endpoint: endpoint, Key: key}
}

func (c *MCPClient) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// At is a copy of the client for another endpoint (another replica's or service's), with the same session id.
func (c *MCPClient) At(endpoint string) *MCPClient {
	cp := *c
	cp.Endpoint = endpoint
	return &cp
}

// request builds a request with the client's headers: the key, and the session's id and version when it has one.
func (c *MCPClient) request(ctx context.Context, method, body string, hdr map[string]string) (*http.Request, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint, rd)
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.SID != "" {
		req.Header.Set(mcpwire.HeaderSessionID, c.SID)
		v := c.Version
		if v == "" {
			v = mcpwire.V20251125
		}
		req.Header.Set(mcpwire.HeaderProtocolVersion, v)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return req, nil
}

// Post sends one message and reads the whole answer: a JSON body, or an event stream to its end.
func (c *MCPClient) Post(ctx context.Context, body string) (*MCPResponse, error) {
	return c.PostHeaders(ctx, body, nil)
}

// PostHeaders is Post with headers set (a "" value removes one).
func (c *MCPClient) PostHeaders(ctx context.Context, body string, hdr map[string]string) (*MCPResponse, error) {
	req, err := c.request(ctx, http.MethodPost, body, hdr)
	if err != nil {
		return nil, err
	}
	return do(c.http(), req)
}

// do sends req and reads its answer whole.
func do(hc *http.Client, req *http.Request) (*MCPResponse, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &MCPResponse{Status: resp.StatusCode, Header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		s := &MCPStream{resp: resp, br: bufio.NewReader(resp.Body)}
		for {
			ev, err := s.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return out, err
			}
			out.Events = append(out.Events, ev)
		}
		return out, nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, err
	}
	out.Body = b
	if out.Body == nil {
		out.Body = []byte{}
	}
	return out, nil
}

// Initialize opens a session: initialize with the client's capabilities, then notifications/initialized. It
// returns the initialize answer.
func (c *MCPClient) Initialize(ctx context.Context) (*MCPResponse, error) {
	caps := c.Capabilities
	if caps == "" {
		caps = `{"elicitation":{}}`
	}
	c.SID = ""
	r, err := c.Post(ctx, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":`+caps+`,"clientInfo":{"name":"kontra-e2e","version":"1"}}}`)
	if err != nil {
		return nil, err
	}
	sid := r.Header.Get(mcpwire.HeaderSessionID)
	if r.Status != http.StatusOK || sid == "" || r.Result() == nil {
		return r, fmt.Errorf("e2e: initialize %s: %d %s", c.Endpoint, r.Status, r.Text())
	}
	c.SID = sid
	n, err := c.Post(ctx, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err != nil {
		return r, err
	}
	if n.Status != http.StatusAccepted {
		return r, fmt.Errorf("e2e: notifications/initialized %s: %d %s", c.Endpoint, n.Status, n.Text())
	}
	return r, nil
}

// Call sends tools/call of tool with args (any JSON value) under the JSON-RPC id id.
func (c *MCPClient) Call(ctx context.Context, id any, tool string, args any) (*MCPResponse, error) {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return nil, err
	}
	return c.Post(ctx, string(b))
}

// Request sends a request of method with params.
func (c *MCPClient) Request(ctx context.Context, id any, method string, params any) (*MCPResponse, error) {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return c.Post(ctx, string(b))
}

// Delete ends the session (DELETE with its id) and returns the status.
func (c *MCPClient) Delete(ctx context.Context) (int, error) {
	req, err := c.request(ctx, http.MethodDelete, "", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Open sends a POST or a GET and returns its answer as a stream, read as it comes (a tasks/result held open, a
// GET stream). A JSON answer is a stream of one event whose data is the body.
func (c *MCPClient) Open(ctx context.Context, method, body string, hdr map[string]string) (*MCPStream, error) {
	req, err := c.request(ctx, method, body, hdr)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req) // no client timeout: the stream lives as long as ctx
	if err != nil {
		return nil, err
	}
	s := &MCPStream{Status: resp.StatusCode, Header: resp.Header, resp: resp, br: bufio.NewReader(resp.Body)}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s.json = b
	}
	return s, nil
}

// Get opens the session's GET stream, resuming after lastEventID when it is not "".
func (c *MCPClient) Get(ctx context.Context, lastEventID string) (*MCPStream, error) {
	hdr := map[string]string{"Accept": "text/event-stream"}
	if lastEventID != "" {
		hdr[mcpwire.HeaderLastEventID] = lastEventID
	}
	return c.Open(ctx, http.MethodGet, "", hdr)
}

// MCPStream is an answer read event by event.
type MCPStream struct {
	Status int
	Header http.Header

	resp *http.Response
	br   *bufio.Reader
	json []byte // a JSON answer
	done bool

	mu     sync.Mutex
	closed bool
}

// next reads the next event (io.EOF at the end of the stream). Comments and retry: lines are skipped; an event
// with no data line is skipped as clients do.
func (s *MCPStream) next() (SSEEvent, error) {
	if s.json != nil {
		if s.done {
			return SSEEvent{}, io.EOF
		}
		s.done = true
		return SSEEvent{Data: string(s.json)}, nil
	}
	var ev SSEEvent
	var data []string
	hasData := false
	for {
		line, err := s.br.ReadString('\n')
		if err != nil && line == "" {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return SSEEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			ev = SSEEvent{}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
			hasData = true
		}
	}
}

// Next waits for the next event until ctx ends; io.EOF at the end of the stream.
func (s *MCPStream) Next(ctx context.Context) (SSEEvent, error) {
	type res struct {
		ev  SSEEvent
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ev, err := s.next()
		ch <- res{ev, err}
	}()
	select {
	case r := <-ch:
		return r.ev, r.err
	case <-ctx.Done():
		s.Close()
		<-ch
		return SSEEvent{}, ctx.Err()
	}
}

// Message decodes an event's data as a JSON-RPC message.
func (e SSEEvent) Message() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(e.Data), &m)
	return m
}

// Close ends the stream (the client goes away).
func (s *MCPStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.resp != nil {
			_ = s.resp.Body.Close()
		}
	}
}

// ModernClient is a 2026-07-28 client: no session; every request carries its _meta and its headers.
type ModernClient struct {
	Endpoint string
	Key      string
	// Tasks declares the io.modelcontextprotocol/tasks extension in the per-request capabilities.
	Tasks bool
	HTTP  *http.Client
}

// ModernMeta is a modern request's per-request _meta: the protocol version, the client's capabilities
// (elicitation, and the tasks extension when tasks) and its info.
func ModernMeta(version string, tasks bool) map[string]any {
	caps := map[string]any{"elicitation": map[string]any{}}
	if tasks {
		caps["extensions"] = map[string]any{mcpwire.ExtTasks: map[string]any{}}
	}
	return map[string]any{mcpwire.MetaProtocolVersion: version, mcpwire.MetaClientCapabilities: caps,
		mcpwire.MetaClientInfo: map[string]any{"name": "kontra-e2e", "version": "1"}}
}

// Body is a modern request's body: params with the client's _meta.
func (c *ModernClient) Body(id any, method string, params map[string]any) string {
	p := map[string]any{"_meta": ModernMeta(mcpwire.V20260728, c.Tasks)}
	for k, v := range params {
		p[k] = v
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p})
	return string(b)
}

// Headers are the headers a conforming client writes for method and params: the version, Mcp-Method, and
// Mcp-Name from the body (Base64 sentinel form when needed).
func Headers(method string, params map[string]any) map[string]string {
	hdr := map[string]string{mcpwire.HeaderProtocolVersion: mcpwire.V20260728, mcpwire.HeaderMethod: method}
	if src, ok := mcpwire.NameSource(method); ok {
		if v, ok := params[src].(string); ok {
			hdr[mcpwire.HeaderName] = mcpwire.EncodeHeaderValue(v)
		}
	}
	return hdr
}

// Request sends a conforming request.
func (c *ModernClient) Request(ctx context.Context, id any, method string, params map[string]any) (*MCPResponse, error) {
	return c.Raw(ctx, c.Body(id, method, params), Headers(method, params))
}

// Raw sends body with exactly the headers hdr (and the key, Content-Type and Accept).
func (c *ModernClient) Raw(ctx context.Context, body string, hdr map[string]string) (*MCPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return do(hc, req)
}

// At is a copy of the client for another endpoint.
func (c *ModernClient) At(endpoint string) *ModernClient {
	cp := *c
	cp.Endpoint = endpoint
	return &cp
}
