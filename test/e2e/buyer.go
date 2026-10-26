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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Binding is the A2A binding a buyer call uses.
type Binding int

// Bindings.
const (
	JSONRPC  Binding = iota // POST {svc}
	HTTPJSON                // {svc}/message:stream, {svc}/tasks/{id}:subscribe
)

func (b Binding) String() string {
	if b == HTTPJSON {
		return "HTTP+JSON"
	}
	return "JSONRPC"
}

// Buyer is one API key calling the gate.
type Buyer struct {
	Key string
}

// BuyerA is the harness's first buyer; BuyerB another buyer of the same tenant.
func (h *Harness) BuyerA() *Buyer { return &Buyer{Key: h.APIKey} }

// BuyerB is the harness's second buyer.
func (h *Harness) BuyerB() *Buyer { return &Buyer{Key: h.APIKeyB} }

// Call says where and how a buyer call goes.
type Call struct {
	Service string      // default "slow"
	Binding Binding     // default JSONRPC
	Header  http.Header // extra request headers (a Cookie, say)
}

func (c Call) svc() string {
	if c.Service == "" {
		return "slow"
	}
	return c.Service
}

var rpcID atomic.Int64

// Event is one SSE event as the buyer received it.
type Event struct {
	ID    *int64          // nil for a leading snapshot
	Data  json.RawMessage // the StreamResponse (the JSON-RPC result on JSON-RPC)
	Event a2a.Event       // Data decoded
	Kind  string          // task, status, artifact or message
	State a2a.TaskState   // the task state the event carries, when it carries one
}

// Task returns the event's task, nil when it is not a task event.
func (e Event) Task() *a2a.Task {
	t, _ := e.Event.(*a2a.Task)
	return t
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

// StreamError is a streaming call the gate answered without opening a stream: an ordinary JSON-RPC or HTTP
// error before any SSE header (spec §9).
type StreamError struct {
	HTTPStatus  int
	ContentType string
	Body        []byte
	RPC         *RPCError // the JSON-RPC error, when the body is one
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("no stream: HTTP %d %s: %s", e.HTTPStatus, e.ContentType, bytes.TrimSpace(e.Body))
}

// Stream is one buyer SSE stream.
type Stream struct {
	Header  http.Header // the response headers
	binding Binding
	cancel  context.CancelFunc
	body    io.ReadCloser
	items   chan item
	done    chan struct{} // closed by Close: the reader stops sending
	taskID  atomic.Value
	closed  atomic.Bool
}

type item struct {
	ev  Event
	err error
}

func (b *Buyer) request(ctx context.Context, method, u string, body any, c Call) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		js, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(js)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if b.Key != "" {
		req.Header.Set("Authorization", "Bearer "+b.Key)
	}
	req.Header.Set("A2A-Version", "1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// SendStreaming opens SendStreamingMessage on g.
func (b *Buyer) SendStreaming(ctx context.Context, g *Gate, req *a2a.SendMessageRequest, c Call) (*Stream, error) {
	if c.Binding == HTTPJSON {
		return b.open(ctx, HTTPJSON, http.MethodPost, g.Svc(c.svc())+"/message:stream", req, nil, c)
	}
	env := rpcEnvelope{JSONRPC: "2.0", ID: rpcID.Add(1), Method: "SendStreamingMessage", Params: req}
	return b.open(ctx, JSONRPC, http.MethodPost, g.Svc(c.svc()), env, nil, c)
}

// Subscribe opens SubscribeToTask on g, with Last-Event-ID when lastEventID is not nil.
func (b *Buyer) Subscribe(ctx context.Context, g *Gate, taskID a2a.TaskID, lastEventID *int64, c Call) (*Stream, error) {
	if c.Binding == HTTPJSON {
		return b.open(ctx, HTTPJSON, http.MethodGet, g.Svc(c.svc())+"/tasks/"+string(taskID)+":subscribe", nil, lastEventID, c)
	}
	env := rpcEnvelope{JSONRPC: "2.0", ID: rpcID.Add(1), Method: "SubscribeToTask", Params: &a2a.SubscribeToTaskRequest{ID: taskID}}
	return b.open(ctx, JSONRPC, http.MethodPost, g.Svc(c.svc()), env, lastEventID, c)
}

// SubscribeRaw opens SubscribeToTask with a raw Last-Event-ID header value.
func (b *Buyer) SubscribeRaw(ctx context.Context, g *Gate, taskID a2a.TaskID, lastEventID string, c Call) (*Stream, error) {
	if c.Header == nil {
		c.Header = http.Header{}
	}
	c.Header = c.Header.Clone()
	c.Header.Set("Last-Event-ID", lastEventID)
	return b.Subscribe(ctx, g, taskID, nil, c)
}

func (b *Buyer) open(ctx context.Context, binding Binding, method, u string, body any, lastEventID *int64, c Call) (*Stream, error) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	req, err := b.request(sctx, method, u, body, c)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != nil {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(*lastEventID, 10))
	}
	// The stream's own context is not ctx: a buyer's stream lives until Close or the gate ends it. ctx
	// bounds only the wait for the response headers.
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := streamClient.Do(req)
		got <- result{resp, err}
	}()
	var r result
	select {
	case r = <-got:
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	if r.err != nil {
		cancel()
		return nil, r.err
	}
	resp := r.resp
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		defer cancel()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		se := &StreamError{HTTPStatus: resp.StatusCode, ContentType: ct, Body: raw}
		var env rpcEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			se.RPC = env.Error
		}
		return nil, se
	}
	s := &Stream{Header: resp.Header, binding: binding, cancel: cancel, body: resp.Body, items: make(chan item, 1024), done: make(chan struct{})}
	go s.read()
	return s, nil
}

// streamClient never times out a response: SSE streams stay open as long as the task runs.
var streamClient = &http.Client{Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 16}}

// send hands one item to Next, unless the buyer closed the stream.
func (s *Stream) send(it item) bool {
	select {
	case s.items <- it:
		return true
	case <-s.done:
		return false
	}
}

func (s *Stream) read() {
	defer close(s.items)
	br := bufio.NewReaderSize(s.body, 64<<10)
	var (
		id   *int64
		data []string
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && len(data) == 0 {
				s.send(item{err: io.EOF})
			} else if s.closed.Load() {
				s.send(item{err: errClosed})
			} else {
				s.send(item{err: fmt.Errorf("stream broke: %w", err)})
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) == 0 {
				id = nil
				continue
			}
			ev, err := s.decode(id, strings.Join(data, "\n"))
			if !s.send(item{ev: ev, err: err}) {
				return
			}
			id, data = nil, nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			v := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				s.send(item{err: fmt.Errorf("an id that is not a seq: %q", v)})
				return
			}
			id = &n
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

var errClosed = errors.New("stream closed by the buyer")

func (s *Stream) decode(id *int64, data string) (Event, error) {
	raw := json.RawMessage(data)
	if s.binding == JSONRPC {
		var env rpcEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return Event{}, fmt.Errorf("data is not a JSON-RPC envelope: %w: %s", err, data)
		}
		if env.Error != nil {
			return Event{}, env.Error
		}
		raw = env.Result
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return Event{}, fmt.Errorf("data is not a StreamResponse: %w: %s", err, data)
	}
	ev := Event{ID: id, Data: raw, Event: sr.Event}
	switch e := sr.Event.(type) {
	case *a2a.Task:
		ev.Kind, ev.State = "task", e.Status.State
	case *a2a.TaskStatusUpdateEvent:
		ev.Kind, ev.State = "status", e.Status.State
	case *a2a.TaskArtifactUpdateEvent:
		ev.Kind = "artifact"
	case *a2a.Message:
		ev.Kind = "message"
	}
	if tid := sr.Event.TaskInfo().TaskID; tid != "" && s.taskID.Load() == nil {
		s.taskID.Store(tid)
	}
	return ev, nil
}

// Next returns the next event; io.EOF when the gate closed the stream.
func (s *Stream) Next(ctx context.Context) (Event, error) {
	select {
	case it, ok := <-s.items:
		if !ok {
			return Event{}, io.EOF
		}
		return it.ev, it.err
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

// TaskID is the gate task id of the stream (from its first event).
func (s *Stream) TaskID() a2a.TaskID {
	if v := s.taskID.Load(); v != nil {
		return v.(a2a.TaskID)
	}
	return ""
}

// Close drops the connection, as a buyer whose network went away.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.done)
	s.cancel()
	return s.body.Close()
}

// ReadAll reads events until the gate closes the stream (io.EOF), or an error.
func (s *Stream) ReadAll(ctx context.Context) ([]Event, error) {
	var evs []Event
	for {
		ev, err := s.Next(ctx)
		if errors.Is(err, io.EOF) {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

// --- unary calls ---

// RPCResult is a unary JSON-RPC answer.
type RPCResult struct {
	HTTPStatus  int
	ContentType string
	Result      json.RawMessage
	Error       *RPCError
	Body        []byte
}

// RPC posts one JSON-RPC request to {svc} on g.
func (b *Buyer) RPC(ctx context.Context, g *Gate, svc, method string, params any) (*RPCResult, error) {
	if svc == "" {
		svc = "slow"
	}
	env := rpcEnvelope{JSONRPC: "2.0", ID: rpcID.Add(1), Method: method, Params: params}
	req, err := b.request(ctx, http.MethodPost, g.Svc(svc), env, Call{})
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	r := &RPCResult{HTTPStatus: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: raw}
	var out rpcEnvelope
	if err := json.Unmarshal(raw, &out); err == nil {
		r.Result, r.Error = out.Result, out.Error
	}
	return r, nil
}

// RESTResult is a unary HTTP+JSON answer.
type RESTResult struct {
	HTTPStatus int
	Body       []byte
}

// REST sends one HTTP+JSON request to {svc}+path on g.
func (b *Buyer) REST(ctx context.Context, g *Gate, svc, method, path string, body any) (*RESTResult, error) {
	if svc == "" {
		svc = "slow"
	}
	req, err := b.request(ctx, method, g.Svc(svc)+path, body, Call{})
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return &RESTResult{HTTPStatus: resp.StatusCode, Body: raw}, err
}

type bearer struct {
	key  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(r)
}

// Client is a2a-go's own JSON-RPC client on the service at g, with the buyer's key: a stock A2A client.
func (b *Buyer) Client(t testing.TB, g *Gate, svc string) *a2aclient.Client {
	t.Helper()
	return b.client(t, g, svc, JSONRPC)
}

// ClientREST is a2a-go's own HTTP+JSON client on the service at g, with the buyer's key.
func (b *Buyer) ClientREST(t testing.TB, g *Gate, svc string) *a2aclient.Client {
	t.Helper()
	return b.client(t, g, svc, HTTPJSON)
}

func (b *Buyer) client(t testing.TB, g *Gate, svc string, binding Binding) *a2aclient.Client {
	t.Helper()
	if svc == "" {
		svc = "slow"
	}
	hc := &http.Client{Transport: bearer{key: b.Key, next: http.DefaultTransport}, Timeout: 2 * time.Minute}
	iface := &a2a.AgentInterface{URL: g.Svc(svc), ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version}
	opt := a2aclient.WithJSONRPCTransport(hc)
	if binding == HTTPJSON {
		iface.ProtocolBinding, opt = a2a.TransportProtocolHTTPJSON, a2aclient.WithRESTTransport(hc)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{iface}, opt)
	if err != nil {
		t.Fatalf("a2a-go client for %s: %v", g.Svc(svc), err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

// SendStreaming opens SendStreamingMessage on g (JSON-RPC) with buyer A's key and a new message.
func (h *Harness) SendStreaming(ctx context.Context, g *Gate, text string) (*Stream, error) {
	return h.BuyerA().SendStreaming(ctx, g, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))}, Call{})
}

// Subscribe opens SubscribeToTask on g (JSON-RPC) with buyer A's key, with Last-Event-ID when lastEventID
// is not nil.
func (h *Harness) Subscribe(ctx context.Context, g *Gate, taskID a2a.TaskID, lastEventID *int64) (*Stream, error) {
	return h.BuyerA().Subscribe(ctx, g, taskID, lastEventID, Call{})
}
