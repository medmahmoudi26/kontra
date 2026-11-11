package a2ahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
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
	if !ok {
		s.serve(w, r)
		return
	}
	if rec == nil {
		// Unrecorded, the call still says which A2A method brought its message (D357).
		var body []byte
		if rt.kind == routeJSONRPC || rt.message {
			body, _ = readBody(r)
		}
		s.serve(w, r.WithContext(withOrigin(r.Context(), methodOf(r, rt, body), uuid.Nil)))
		return
	}
	start := time.Now()
	ctx, note := traffic.WithNote(r.Context()) // the exchange's id, minted now (D356), for what the call writes
	bodyMax := rec.BodyMax()
	var body []byte
	if rt.kind == routeJSONRPC || rt.message || bodyMax > 0 {
		body, _ = readBody(r) // the edge has read it already; put back for the handler
	}
	ctx = withOrigin(ctx, methodOf(r, rt, body), note.ID())
	version := versionOf(r) // before Versions rewrites the header
	cw := &captureWriter{ResponseWriter: w, bodyMax: bodyMax, start: start}
	s.serve(cw, r.WithContext(ctx))

	e := traffic.Exchange{At: start, Leg: traffic.LegCaller, Service: rt.name, Binding: traffic.BindingREST,
		A2AVersion: version, BytesIn: max(r.ContentLength, int64(len(body))), BytesOut: cw.n, Duration: time.Since(start),
		HTTPStatus: cw.status()}
	if rt.kind == routeJSONRPC {
		e.Binding = traffic.BindingJSONRPC
	}
	e.Method = methodOf(r, rt, body)
	if bodyMax > 0 && !note.NoBodies() {
		e.Request = traffic.KeepBody(body, bodyMax)
		if cw.frames != nil {
			e.Frames, e.FramesDropped = cw.frames.Done()
		} else {
			e.Response = traffic.KeepPrefix(cw.body.Bytes(), cw.n, bodyMax)
		}
	}
	note.Fill(&e)
	taskID, contextID := requestIDs(rt, e.Method, body)
	t, c, code := traffic.AnswerIDs(cw.head.Bytes(), cw.Header().Get("Content-Type"))
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

// withOrigin is ctx whose caller's message, if the call carries one, came by method in exchange (D357).
func withOrigin(ctx context.Context, method string, exchange uuid.UUID) context.Context {
	o := &store.Origin{Actor: store.ActorCaller, Type: method, Method: method}
	if exchange != uuid.Nil {
		o.ExchangeID = exchange.String()
	}
	return store.WithRequestOrigin(ctx, o)
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

// captureWriter counts what the answer writes and keeps its first captureHead bytes; with bodies on it also keeps the
// answer's first bodyMax bytes, or a stream's frames. It passes Flush and the connection's controls through (Unwrap),
// so streams are unchanged.
type captureWriter struct {
	http.ResponseWriter
	code int
	n    int64
	head bytes.Buffer

	bodyMax int
	start   time.Time
	decided bool            // whether the answer is a stream is known (its first write)
	body    bytes.Buffer    // a plain answer's first bodyMax bytes
	frames  *traffic.Frames // a stream's frames
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
	if c.bodyMax > 0 {
		if !c.decided {
			c.decided = true
			if strings.HasPrefix(c.Header().Get("Content-Type"), "text/event-stream") {
				c.frames = traffic.NewFrames(c.start, c.bodyMax)
			}
		}
		if c.frames != nil {
			_, _ = c.frames.Write(b)
		} else if room := c.bodyMax - c.body.Len(); room > 0 {
			c.body.Write(b[:min(room, len(b))])
		}
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

// versionOf is the A2A version a request claims (A2A-Version, header else query parameter) as major.minor, or "" when
// it claims none or something that is not a version: the record never keeps a string the caller chose.
func versionOf(r *http.Request) string {
	v := r.Header.Get(a2a.SvcParamVersion)
	if v == "" {
		v = r.URL.Query().Get(a2a.SvcParamVersion)
	}
	return traffic.MajorMinor(v)
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
		if json.Unmarshal(body, &req) == nil && traffic.A2AMethods[req.Method] {
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
