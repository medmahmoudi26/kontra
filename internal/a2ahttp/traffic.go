package a2ahttp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/traffic"
)

// The A2A traffic record's first point (spec §13.6): every request to an A2A route is one exchange, recorded when its
// answer ends (a stream's included, with its whole duration). The method, task and context come from the request and
// from the head of the answer; the caller, the service's tenant and the gate's reason from the handlers (traffic.Set*).
// Nothing of a body or a header is kept.

// captureHead is how much of an answer is kept, in memory only, to read its task id, context id and JSON-RPC error
// code: a JSON-RPC envelope, a task's first fields, or a stream's first event.
const captureHead = 16 << 10

// ServeHTTP implements http.Handler: the A2A routes, each request recorded as an exchange when Deps.Traffic is set.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := s.deps.Traffic
	rt, ok := parseRoute(r, s.deps.Multi)
	if rec == nil || !ok {
		s.serve(w, r)
		return
	}
	start := time.Now()
	ctx, note := traffic.WithNote(r.Context())
	var body []byte
	if rt.kind == routeJSONRPC || rt.message {
		body, _ = readBody(r) // the edge has read it already; put back for the handler
	}
	cw := &captureWriter{ResponseWriter: w}
	s.serve(cw, r.WithContext(ctx))

	e := traffic.Exchange{At: start, Service: rt.name, Binding: traffic.BindingREST, BytesIn: max(r.ContentLength, int64(len(body))),
		BytesOut: cw.n, Duration: time.Since(start), HTTPStatus: cw.status()}
	if rt.kind == routeJSONRPC {
		e.Binding = traffic.BindingJSONRPC
	}
	e.Method = methodOf(r, rt, body)
	note.Fill(&e)
	taskID, contextID := requestIDs(rt, e.Method, body)
	t, c, code := answerIDs(cw.head.Bytes(), cw.Header().Get("Content-Type"))
	if taskID == "" {
		taskID = t
	}
	if contextID == "" {
		contextID = c
	}
	e.RPCCode = code
	if id, err := uuid.Parse(taskID); err == nil {
		e.TaskID = &id
	}
	e.ContextID = contextID
	rec.Record(e)
}

// RecordRefusal records an edge refusal (413, 429, 408) of a request to an A2A route, which never reached a handler: the
// service as its path names it, method unknown (the body was not read), the edge's reason. Paths that are not A2A
// routes (/mcp, /v1/tunnel, /healthz, the console) are not exchanges. It is edge.Config.OnRefuse's hook.
func (s *Server) RecordRefusal(r *http.Request, status int, reason string) {
	rec := s.deps.Traffic
	rt, ok := parseRoute(r, s.deps.Multi)
	if rec == nil || !ok {
		return
	}
	e := traffic.Exchange{At: time.Now(), Service: rt.name, Method: traffic.MethodUnknown, Binding: traffic.BindingREST,
		BytesIn: max(r.ContentLength, 0), HTTPStatus: status, Reason: reason}
	if rt.kind == routeJSONRPC {
		e.Binding = traffic.BindingJSONRPC
		if status == http.StatusRequestEntityTooLarge || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout {
			code := -32600 // the edge's JSON-RPC refusals (spec §2.2)
			e.RPCCode = &code
		}
	}
	if rt.card() {
		e.Method = "card"
	}
	rec.Record(e)
}

// captureWriter counts what the answer writes and keeps its first captureHead bytes; it passes Flush and the
// connection's controls through (Unwrap), so streams are unchanged.
type captureWriter struct {
	http.ResponseWriter
	code int
	n    int64
	head bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	if room := captureHead - c.head.Len(); room > 0 {
		c.head.Write(b[:min(room, len(b))])
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

// Flush flushes the answer (the SSE writer flushes after each event).
func (c *captureWriter) Flush() { _ = http.NewResponseController(c.ResponseWriter).Flush() }

// Unwrap lets http.ResponseController reach the writer beneath.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}

// a2aMethods are the JSON-RPC methods of A2A 1.0; any other name a caller sends is recorded as unknown, so the record
// never keeps a string the caller chose.
var a2aMethods = map[string]bool{
	"SendMessage": true, "SendStreamingMessage": true, "GetTask": true, "ListTasks": true, "CancelTask": true,
	"SubscribeToTask": true, "CreateTaskPushNotificationConfig": true, "GetTaskPushNotificationConfig": true,
	"ListTaskPushNotificationConfigs": true, "DeleteTaskPushNotificationConfig": true, "GetExtendedAgentCard": true,
}

// methodOf names the call: the JSON-RPC method, the HTTP+JSON route's method, or card, upstream_card and seller_jwks.
func methodOf(r *http.Request, rt route, body []byte) string {
	switch rt.kind {
	case routeCard:
		return "card"
	case routeUpstreamCard:
		return "upstream_card"
	case routeSellerJWKS:
		return "seller_jwks"
	case routeJSONRPC:
		var req struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &req) == nil && a2aMethods[req.Method] {
			return req.Method
		}
		return traffic.MethodUnknown
	case routeRESTStream:
		return "SendStreamingMessage"
	case routeRESTSubscribe:
		return "SubscribeToTask"
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, rt.prefix), "/")
	switch {
	case rest == "message:send":
		return "SendMessage"
	case rest == "tasks":
		return "ListTasks"
	case rest == "extendedAgentCard":
		return "GetExtendedAgentCard"
	case strings.HasSuffix(rest, ":cancel"):
		return "CancelTask"
	case strings.Contains(rest, "/pushNotificationConfigs"):
		switch {
		case r.Method == http.MethodPost:
			return "CreateTaskPushNotificationConfig"
		case r.Method == http.MethodDelete:
			return "DeleteTaskPushNotificationConfig"
		case strings.HasSuffix(rest, "/pushNotificationConfigs"):
			return "ListTaskPushNotificationConfigs"
		}
		return "GetTaskPushNotificationConfig"
	case strings.HasPrefix(rest, "tasks/"):
		return "GetTask"
	}
	return traffic.MethodUnknown
}

// requestIDs are the task and context ids the request names: a message's taskId and contextId, a task method's id
// (JSON-RPC params.id or params.taskId, the HTTP+JSON path's {id}).
func requestIDs(rt route, method string, body []byte) (taskID, contextID string) {
	var p struct {
		ID      string `json:"id"`
		TaskID  string `json:"taskId"`
		Message *struct {
			TaskID    string `json:"taskId"`
			ContextID string `json:"contextId"`
		} `json:"message"`
	}
	src := body
	if rt.kind == routeJSONRPC {
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
		if rt.kind == routeJSONRPC && method != "SendMessage" && method != "SendStreamingMessage" {
			if p.ID != "" {
				return p.ID, ""
			}
			return p.TaskID, ""
		}
	}
	if rt.taskID != "" {
		return rt.taskID, ""
	}
	return "", ""
}

// idsOf reads a task's or a message's ids from one A2A object: a Task (id, contextId), a Message or an update
// (taskId, contextId), or a wrapper holding one (task, message, statusUpdate, artifactUpdate).
func idsOf(raw json.RawMessage) (taskID, contextID string) {
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
			return idsOf(inner)
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

// answerIDs reads the head of an answer: a JSON-RPC envelope (result, or error and its code), an HTTP+JSON object,
// or a stream's first event (SSE data: lines, each a JSON-RPC envelope or a StreamResponse).
func answerIDs(head []byte, contentType string) (taskID, contextID string, rpcCode *int) {
	if strings.HasPrefix(contentType, "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(head))
		sc.Buffer(make([]byte, 0, len(head)), len(head)+1)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				t, c, code := answerIDs([]byte(strings.TrimSpace(data)), "application/json")
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
		t, c := idsOf(head)
		return t, c, nil
	}
	if env.Error != nil {
		code := env.Error.Code
		return "", "", &code
	}
	t, c := idsOf(env.Result)
	return t, c, nil
}
