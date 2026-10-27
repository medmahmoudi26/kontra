// Package mcptest is a Go MCP 2025-11-25 upstream for the relay's tests (spec/relay.md §11): a Streamable HTTP
// server on raw JSON that mints its own Mcp-Session-Id at initialize and requires it after, pages its tools/list,
// answers tools/call from a hook (echo by default), answers in JSON or in event streams, serves a GET stream that
// carries what a test pushes, and records every request it receives, so a test can show what the upstream saw and
// what it never saw. FakeNode serves it on the node's side of a tunnel session (spec/tunnel.md §8).
//
// M3 WP-04 builds it; M3 WP-07 extends it to dual era (modern.go: server/discover, the tasks extension, MRTR).
package mcptest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Version is the protocol version the double negotiates unless told otherwise.
const Version = "2025-11-25"

// Request is one request the double received.
type Request struct {
	HTTPMethod string
	Header     http.Header
	Body       []byte
	SessionID  string          // its Mcp-Session-Id
	Method     string          // the JSON-RPC method, "" for a response or a GET or DELETE
	ID         json.RawMessage // the JSON-RPC id
	Params     json.RawMessage
}

// Reply is what a hook answers: a result or a JSON-RPC error for the request's id, or Raw, the whole message as
// it is written (any bytes, for the strict decoding tests); Events are written before it on an event stream (the
// reply is then sent as one); Status replaces 200.
type Reply struct {
	Result json.RawMessage
	Error  *RPCError
	Raw    []byte
	Events []string
	SSE    bool
	Status int
	// Hang, for OnModern only, holds the request that long (or until the client goes) and answers nothing.
	Hang time.Duration
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Server is the double. Its fields may be set before it serves and, under Lock, while it does.
type Server struct {
	mu sync.Mutex

	// Tools are the tool objects tools/list pages through; PageSize tools per page (0: all on one).
	Tools    []json.RawMessage
	PageSize int
	// Negotiate is the protocolVersion its initialize answers (Version by default).
	Negotiate string
	// Capabilities are the InitializeResult's capabilities (tools with listChanged by default); InitMeta its _meta.
	Capabilities json.RawMessage
	InitMeta     json.RawMessage
	// InitError, when set, answers initialize with this JSON-RPC error and mints no session.
	InitError *RPCError
	// SSE answers every request with an event stream instead of JSON.
	SSE bool
	// Status, when set, answers every request with it and WWWAuthenticate (an upstream that refuses the gate).
	Status          int
	WWWAuthenticate string
	// OnCall answers tools/call (echo of params.arguments.text by default); OnRequest any other method it does not
	// know (a JSON-RPC error -32601 by default).
	OnCall    func(r Request) Reply
	OnRequest func(r Request) Reply
	// TaskRun, when set, makes the double a 2025-11-25 task receiver (tasks.go): its initialize declares tasks
	// {list, cancel, requests.tools.call}, and a tools/call with params.task creates a task, answered with a
	// CreateTaskResult, whose work TaskRun does on a goroutine of its own.
	TaskRun func(t *Task)
	// TaskPollInterval is the pollInterval (ms) of its tasks (100 by default); IgnoreCancel answers tasks/cancel
	// with the task as it is, which goes on. CancelAnswerDelay, when set, is how long tasks/cancel waits, its task
	// already cancelled, before it answers: an upstream whose cancelled status (on tasks/get, or as the end of a
	// pending tasks/result) reaches the gate before its answer to the cancel does.
	TaskPollInterval  int
	IgnoreCancel      bool
	CancelAnswerDelay time.Duration
	// Hold, when set, is called with each request before the double serves it (a test blocks in it to make the
	// double slow, or hang, on a method).
	Hold func(r Request)

	// Modern makes the double dual-era (modern.go): it serves 2026-07-28 requests beside 2025-11-25 sessions.
	Modern bool
	// OnModern, when set, answers a modern request before anything else when it returns a reply (nil: served as
	// usual); a test scripts a 404, a modern error or a hang with it.
	OnModern func(r Request) *Reply
	// ModernTaskRun makes the double a tasks extension receiver: server/discover declares
	// io.modelcontextprotocol/tasks, and a tools/call that declares it creates a task whose work this does.
	ModernTaskRun func(t *Task)
	// ModernTTL is the ttlMs of its modern tools/list (900000 by default, over the gate's cap); DiscoverVersions its
	// server/discover's supportedVersions; ModernCapabilities replaces its server/discover's capabilities.
	ModernTTL          int64
	DiscoverVersions   []string
	ModernCapabilities json.RawMessage

	requests    []Request
	mismatches  int
	hangsCut    int
	modernTasks map[string]*Task
	sessions    map[string]*session
	nextEv      int
	tasks       map[string]*Task
	nextTask    int
	nextReq     int
}

type session struct {
	push    chan []byte
	deleted bool
}

// New returns a double with no tools.
func New() *Server {
	return &Server{sessions: map[string]*session{}, tasks: map[string]*Task{}, modernTasks: map[string]*Task{}}
}

// Lock and Unlock guard the fields while the double serves.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// Requests returns what the double received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count is the number of JSON-RPC requests of method it received.
func (s *Server) Count(method string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// Calls is the number of tools/call of tool it received.
func (s *Server) Calls(tool string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method != "tools/call" {
			continue
		}
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(r.Params, &p)
		if p.Name == tool {
			n++
		}
	}
	return n
}

// Sessions are the session ids it minted and has not deleted.
func (s *Server) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, x := range s.sessions {
		if !x.deleted {
			out = append(out, id)
		}
	}
	return out
}

// Push queues msg on a session's GET stream (written when one is open, in order).
func (s *Server) Push(sessionID string, msg []byte) error {
	s.mu.Lock()
	x := s.sessions[sessionID]
	s.mu.Unlock()
	if x == nil {
		return fmt.Errorf("mcptest: no session %s", sessionID)
	}
	select {
	case x.push <- msg:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("mcptest: session %s's GET stream does not drain", sessionID)
	}
}

// Tool is a tool object with name and, when meta is not empty, its _meta.
func Tool(name string, meta string) json.RawMessage {
	t := map[string]any{"name": name, "description": "the " + name + " tool",
		"inputSchema": map[string]any{"type": "object"}, "execution": map[string]any{"taskSupport": "optional"}}
	if meta != "" {
		t["_meta"] = json.RawMessage(meta)
	}
	b, _ := json.Marshal(t)
	return b
}

// ServeHTTP implements http.Handler: POST, GET and DELETE on any path.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := Request{HTTPMethod: r.Method, Header: r.Header.Clone(), Body: body, SessionID: r.Header.Get("Mcp-Session-Id")}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &msg)
		req.Method, req.ID, req.Params = msg.Method, msg.ID, msg.Params
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	status, www, hold := s.Status, s.WWWAuthenticate, s.Hold
	s.mu.Unlock()
	if hold != nil {
		hold(req)
	}
	if status != 0 {
		if www != "" {
			w.Header().Set("WWW-Authenticate", www)
		}
		w.Header().Set("Set-Cookie", "upstream=1")
		w.WriteHeader(status)
		return
	}
	if s.modernRequest(r, msg.Method) {
		s.serveModern(w, r, req)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, req)
		return
	case http.MethodDelete:
		if !s.known(req.SessionID) {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.sessions[req.SessionID].deleted = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if msg.Method == "initialize" {
		s.initialize(w, req)
		return
	}
	if req.SessionID == "" {
		// What the TypeScript SDK's legacy transport answers a POST without a session (and so a 2026-07-28 request).
		writeJSON(w, http.StatusBadRequest, []byte(legacyNoSession))
		return
	}
	if !s.known(req.SessionID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Session not found"}}`)
		return
	}
	if len(msg.ID) == 0 || msg.Method == "" {
		if msg.Method == "" && len(msg.ID) > 0 {
			s.answer(req.SessionID, msg.ID, body) // a response to one of the double's own requests (a task's)
		}
		w.WriteHeader(http.StatusAccepted) // a notification or a response
		return
	}
	s.mu.Lock()
	tasks := s.TaskRun != nil
	s.mu.Unlock()
	if tasks && s.serveTask(w, r, req) {
		return
	}
	var rep Reply
	switch msg.Method {
	case "tools/list":
		rep = s.toolsList(req)
	case "tools/call":
		s.mu.Lock()
		on := s.OnCall
		s.mu.Unlock()
		if on != nil {
			rep = on(req)
		} else {
			rep = echo(req)
		}
	case "ping":
		rep = Reply{Result: json.RawMessage(`{}`)}
	default:
		s.mu.Lock()
		on := s.OnRequest
		s.mu.Unlock()
		if on != nil {
			rep = on(req)
		} else {
			rep = Reply{Error: &RPCError{Code: -32601, Message: "Method not found"}}
		}
	}
	s.reply(w, req, rep, "")
}

func (s *Server) known(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.sessions[id]
	return id != "" && x != nil && !x.deleted
}

func (s *Server) initialize(w http.ResponseWriter, req Request) {
	s.mu.Lock()
	refuse := s.InitError
	s.mu.Unlock()
	if refuse != nil {
		s.reply(w, req, Reply{Error: refuse}, "")
		return
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := "up-" + hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[id] = &session{push: make(chan []byte, 64)}
	v, caps, meta, tasks := s.Negotiate, s.Capabilities, s.InitMeta, s.TaskRun != nil
	s.mu.Unlock()
	if v == "" {
		v = Version
	}
	if len(caps) == 0 {
		caps = json.RawMessage(`{"tools":{"listChanged":true}}`)
		if tasks {
			caps = json.RawMessage(`{"tools":{"listChanged":true},"tasks":{"list":{},"cancel":{},"requests":{"tools":{"call":{}}}}}`)
		}
	}
	res := map[string]any{"protocolVersion": v, "capabilities": caps,
		"serverInfo": map[string]any{"name": "mcptest", "version": "1.0.0"}}
	if len(meta) > 0 {
		res["_meta"] = meta
	}
	rb, _ := json.Marshal(res)
	s.reply(w, req, Reply{Result: rb}, id)
}

// toolsList pages Tools: the cursor is the index of the page's first tool.
func (s *Server) toolsList(req Request) Reply {
	s.mu.Lock()
	all, size := append([]json.RawMessage(nil), s.Tools...), s.PageSize
	s.mu.Unlock()
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.Params, &p)
	start := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil || n < 0 || n > len(all) {
			return Reply{Error: &RPCError{Code: -32602, Message: "invalid cursor"}}
		}
		start = n
	}
	end := len(all)
	if size > 0 && start+size < end {
		end = start + size
	}
	res := map[string]any{"tools": all[start:end]}
	if end < len(all) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	b, _ := json.Marshal(res)
	return Reply{Result: b}
}

// echo answers tools/call with the text of params.arguments.text.
func echo(req Request) Reply {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Text string `json:"text"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": p.Arguments.Text}}, "isError": false})
	return Reply{Result: b}
}

// reply writes rep for req, as JSON or as an event stream, with the session id header when sid is set.
func (s *Server) reply(w http.ResponseWriter, req Request, rep Reply, sid string) {
	msg := rep.Raw
	if msg == nil {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if rep.Error != nil {
			m["error"] = rep.Error
		} else {
			m["result"] = rep.Result
		}
		msg, _ = json.Marshal(m)
	}
	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	w.Header().Set("X-Upstream-Secret", "never-relayed")
	s.mu.Lock()
	sse := s.SSE || rep.SSE || len(rep.Events) > 0
	s.mu.Unlock()
	status := rep.Status
	if status == 0 {
		status = http.StatusOK
	}
	if !sse {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(msg)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	fl, _ := w.(http.Flusher)
	for _, e := range rep.Events {
		_, _ = io.WriteString(w, e)
		if fl != nil {
			fl.Flush()
		}
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", s.eventID(), msg)
	if fl != nil {
		fl.Flush()
	}
}

func (s *Server) eventID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextEv++
	return "ev-" + strconv.Itoa(s.nextEv)
}

// get serves a session's GET stream: a priming event, then every message Push queues, until the client goes or
// the session is deleted.
func (s *Server) get(w http.ResponseWriter, r *http.Request, req Request) {
	s.mu.Lock()
	x := s.sessions[req.SessionID]
	s.mu.Unlock()
	if x == nil || x.deleted {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	_, _ = fmt.Fprintf(w, "id: %s\ndata: \n\n", s.eventID())
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-x.push:
			if bytes.HasPrefix(m, []byte("raw:")) {
				_, _ = w.Write(m[4:])
			} else {
				_, _ = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", s.eventID(), m)
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
