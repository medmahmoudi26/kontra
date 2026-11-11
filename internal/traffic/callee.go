package traffic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The callee leg (D355): every A2A call the gate makes to a service's agent, through a tunnel or to a direct backend,
// is an exchange. CalleeTransport records it, around the transport internal/upstream's clients use, when the request's
// context carries a Callee (WithCallee): the dispatch, forward_cancel and the card fetches set one. Calls without it
// (MCP, anything else) pass through unrecorded.

// Callee is what a callee-leg exchange needs from its caller's context: the recorder, the service, and the gate task the
// call works for (none for a card fetch). It also holds the id of the latest exchange made with it, so the relay can
// name the exchange that carried an upstream event (D357): one dispatch makes its calls one at a time.
type Callee struct {
	Recorder *Recorder
	TenantID uuid.UUID
	Service  string
	GateTask *uuid.UUID

	mu   sync.Mutex
	last uuid.UUID
}

type calleeKey struct{}

// WithCallee returns ctx whose A2A calls to an agent are recorded as c's (nothing when c or its recorder is nil).
func WithCallee(ctx context.Context, c *Callee) context.Context {
	if c == nil || c.Recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, calleeKey{}, c)
}

// CalleeFrom is the Callee ctx carries.
func CalleeFrom(ctx context.Context) (*Callee, bool) {
	c, ok := ctx.Value(calleeKey{}).(*Callee)
	return c, ok
}

// Last is the id of the latest exchange made with c, uuid.Nil before the first.
func (c *Callee) Last() uuid.UUID {
	if c == nil {
		return uuid.Nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *Callee) began(id uuid.UUID) {
	c.mu.Lock()
	c.last = id
	c.mu.Unlock()
}

// ReasonUnreachable is a callee-leg call that got no answer (no connection, no tunnel session, a broken stream open).
const ReasonUnreachable = "upstream_unreachable"

// CalleeTransport records the callee leg around Next.
type CalleeTransport struct{ Next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (t CalleeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c, ok := CalleeFrom(r.Context())
	if !ok {
		return t.Next.RoundTrip(r)
	}
	start := time.Now()
	e := Exchange{ID: NewID(), At: start, Leg: LegCallee, TenantID: &c.TenantID, Service: c.Service, GateTaskID: c.GateTask}
	c.began(e.ID)
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	e.Method, e.Binding, e.A2AVersion = CalleeMethod(r, body)
	e.BytesIn = int64(len(body))
	tid, cid := calleeRequestIDs(e.Binding, body, r)
	setIDs(&e, tid, cid)
	bodyMax := c.Recorder.BodyMax()
	if bodyMax > 0 {
		e.Request = KeepBody(body, bodyMax)
	}
	resp, err := t.Next.RoundTrip(r)
	if err != nil {
		e.HTTPStatus, e.Reason, e.Duration = http.StatusBadGateway, ReasonUnreachable, time.Since(start)
		c.Recorder.Record(e)
		return nil, err
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	rb := &calleeBody{rc: resp.Body, rec: c.Recorder, e: e, start: start, status: resp.StatusCode, max: bodyMax}
	if mt == "text/event-stream" && bodyMax > 0 {
		rb.frames = NewFrames(start, bodyMax)
	}
	resp.Body = rb
	return resp, nil
}

// calleeBody tees the agent's answer into the exchange, and records the exchange once: at the end of the body, its
// failure, or its Close.
type calleeBody struct {
	rc     io.ReadCloser
	rec    *Recorder
	e      Exchange
	start  time.Time
	status int
	max    int
	n      int64
	head   bytes.Buffer
	body   bytes.Buffer
	frames *Frames
	once   sync.Once
}

func (b *calleeBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.n += int64(n)
		chunk := p[:n]
		if room := captureHead - b.head.Len(); room > 0 {
			b.head.Write(chunk[:min(room, n)])
		}
		if b.frames != nil {
			_, _ = b.frames.Write(chunk)
		} else if room := b.max - b.body.Len(); b.max > 0 && room > 0 {
			b.body.Write(chunk[:min(room, n)])
		}
	}
	if err != nil {
		b.done(err != io.EOF)
	}
	return n, err
}

func (b *calleeBody) Close() error {
	err := b.rc.Close()
	b.done(false)
	return err
}

// done records the exchange; broken says the answer failed before its end.
func (b *calleeBody) done(broken bool) {
	b.once.Do(func() {
		e := b.e
		e.HTTPStatus, e.BytesOut, e.Duration = b.status, b.n, time.Since(b.start)
		if broken {
			e.Reason = ReasonUnreachable
		}
		ct := "application/json"
		if b.frames != nil {
			ct = "text/event-stream"
		}
		t, c, code := AnswerIDs(b.head.Bytes(), ct)
		setIDs(&e, t, c)
		e.RPCCode = code
		if b.max > 0 {
			if b.frames != nil {
				e.Frames, e.FramesDropped = b.frames.Done()
			} else {
				e.Response = KeepPrefix(b.body.Bytes(), b.n, b.max)
			}
		}
		b.rec.Record(e)
	})
}

// captureHead is how much of an answer is held to read its ids and JSON-RPC error code.
const captureHead = 16 << 10

// setIDs fills a callee-leg exchange's agent task id and context id where they are still empty.
func setIDs(e *Exchange, taskID, contextID string) {
	if e.CalleeTaskID == "" {
		e.CalleeTaskID = taskID
	}
	if e.ContextID == "" {
		e.ContextID = contextID
	}
}

// v03Methods are A2A 0.3's JSON-RPC method names, by the A2A 1.0 method they are.
var v03Methods = map[string]string{
	"message/send": "SendMessage", "message/stream": "SendStreamingMessage", "tasks/get": "GetTask",
	"tasks/list": "ListTasks", "tasks/cancel": "CancelTask", "tasks/resubscribe": "SubscribeToTask",
	"tasks/pushNotificationConfig/set":    "CreateTaskPushNotificationConfig",
	"tasks/pushNotificationConfig/get":    "GetTaskPushNotificationConfig",
	"tasks/pushNotificationConfig/list":   "ListTaskPushNotificationConfigs",
	"tasks/pushNotificationConfig/delete": "DeleteTaskPushNotificationConfig",
	"agent/getAuthenticatedExtendedCard":  "GetExtendedAgentCard",
}

// A2AMethods are A2A 1.0's methods.
var A2AMethods = map[string]bool{
	"SendMessage": true, "SendStreamingMessage": true, "GetTask": true, "ListTasks": true, "CancelTask": true,
	"SubscribeToTask": true, "CreateTaskPushNotificationConfig": true, "GetTaskPushNotificationConfig": true,
	"ListTaskPushNotificationConfigs": true, "DeleteTaskPushNotificationConfig": true, "GetExtendedAgentCard": true,
}

// CalleeMethod names a call the gate makes to an agent by its A2A 1.0 method (a 0.3 call by the 1.0 method it is),
// card for a card fetch, unknown otherwise; with its binding and the A2A version it speaks ("" when not known).
func CalleeMethod(r *http.Request, body []byte) (method, binding, version string) {
	version = MajorMinor(r.Header.Get("A2A-Version"))
	path := r.URL.Path
	if strings.HasSuffix(path, "/.well-known/agent-card.json") || strings.HasSuffix(path, "/.well-known/agent.json") {
		return "card", BindingREST, version
	}
	if len(bytes.TrimSpace(body)) > 0 && bytes.TrimSpace(body)[0] == '{' {
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
		}
		if json.Unmarshal(body, &req) == nil && req.JSONRPC == "2.0" {
			switch {
			case A2AMethods[req.Method]:
				return req.Method, BindingJSONRPC, version
			case v03Methods[req.Method] != "":
				if version == "" {
					version = "0.3"
				}
				return v03Methods[req.Method], BindingJSONRPC, version
			}
			return MethodUnknown, BindingJSONRPC, version
		}
	}
	return restMethod(r.Method, path), BindingREST, version
}

// restMethod names an HTTP+JSON call by its path (1.0's and 0.3's /v1/ paths alike).
func restMethod(httpMethod, path string) string {
	switch {
	case strings.HasSuffix(path, "message:send"):
		return "SendMessage"
	case strings.HasSuffix(path, "message:stream"):
		return "SendStreamingMessage"
	case strings.HasSuffix(path, ":cancel"):
		return "CancelTask"
	case strings.HasSuffix(path, ":subscribe"):
		return "SubscribeToTask"
	case strings.HasSuffix(path, "extendedAgentCard"), strings.HasSuffix(path, "/card"):
		return "GetExtendedAgentCard"
	case strings.Contains(path, "/pushNotificationConfigs"):
		switch {
		case httpMethod == http.MethodPost:
			return "CreateTaskPushNotificationConfig"
		case httpMethod == http.MethodDelete:
			return "DeleteTaskPushNotificationConfig"
		case strings.HasSuffix(path, "/pushNotificationConfigs"):
			return "ListTaskPushNotificationConfigs"
		}
		return "GetTaskPushNotificationConfig"
	case strings.HasSuffix(path, "/tasks") && httpMethod == http.MethodGet:
		return "ListTasks"
	case strings.Contains(path, "/tasks/") && httpMethod == http.MethodGet:
		return "GetTask"
	}
	return MethodUnknown
}

// calleeRequestIDs are the agent's task and context ids a call names: a message's taskId and contextId, a task
// method's id (JSON-RPC params.id or params.taskId, an HTTP+JSON path's tasks/{id}).
func calleeRequestIDs(binding string, body []byte, r *http.Request) (taskID, contextID string) {
	var p struct {
		ID      string `json:"id"`
		TaskID  string `json:"taskId"`
		Message *struct {
			TaskID    string `json:"taskId"`
			ContextID string `json:"contextId"`
		} `json:"message"`
	}
	src := body
	if binding == BindingJSONRPC {
		var req struct {
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "", ""
		}
		src = req.Params
	}
	if len(src) > 0 && json.Unmarshal(src, &p) == nil {
		if p.Message != nil {
			return p.Message.TaskID, p.Message.ContextID
		}
		if binding == BindingJSONRPC {
			if p.ID != "" {
				return p.ID, ""
			}
			return p.TaskID, ""
		}
	}
	if _, rest, ok := strings.Cut(r.URL.Path, "/tasks/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		id, _, _ = strings.Cut(id, ":")
		return id, ""
	}
	return "", ""
}

// MajorMinor is v ("1.0", "1.0.1", "0.3.0") as major.minor, or "" when v is not a version: the record never keeps a
// string a caller chose.
func MajorMinor(v string) string {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" || len(p) > 4 || strings.Trim(p, "0123456789") != "" {
			return ""
		}
	}
	return parts[0] + "." + parts[1]
}

// IDsOf reads a task's or a message's ids from one A2A object: a Task (id, contextId), a Message or an update
// (taskId, contextId), or a wrapper holding one (task, message, statusUpdate, artifactUpdate).
func IDsOf(raw json.RawMessage) (taskID, contextID string) {
	var o struct {
		ID             string          `json:"id"`
		TaskID         string          `json:"taskId"`
		ContextID      string          `json:"contextId"`
		Task           json.RawMessage `json:"task"`
		Message        json.RawMessage `json:"message"`
		StatusUpdate   json.RawMessage `json:"statusUpdate"`
		ArtifactUpdate json.RawMessage `json:"artifactUpdate"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &o) != nil {
		return "", ""
	}
	for _, inner := range []json.RawMessage{o.Task, o.Message, o.StatusUpdate, o.ArtifactUpdate} {
		if len(inner) > 0 {
			return IDsOf(inner)
		}
	}
	if o.TaskID != "" {
		return o.TaskID, o.ContextID
	}
	if o.ContextID != "" { // a Task: its id is the task's
		return o.ID, o.ContextID
	}
	return "", ""
}

// AnswerIDs reads the head of an answer: a JSON-RPC envelope (result, or error and its code), an HTTP+JSON object, or
// a stream's first event (SSE data: lines, each a JSON-RPC envelope or a StreamResponse).
func AnswerIDs(head []byte, contentType string) (taskID, contextID string, rpcCode *int) {
	if strings.HasPrefix(contentType, "text/event-stream") {
		for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r", ""), "\n") {
			if data, ok := strings.CutPrefix(line, "data:"); ok {
				t, c, code := AnswerIDs([]byte(strings.TrimSpace(data)), "application/json")
				if t != "" || code != nil {
					return t, c, code
				}
			}
		}
		return "", "", nil
	}
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(head, &env) != nil {
		return "", "", nil // an answer longer than the head is read no further
	}
	if env.JSONRPC == "" {
		t, c := IDsOf(head)
		return t, c, nil
	}
	if env.Error != nil {
		code := env.Error.Code
		return "", "", &code
	}
	t, c := IDsOf(env.Result)
	return t, c, nil
}
