package mcptest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Task is one 2025-11-25 task of the double (spec/relay.md §11.5's upstream): created by a task-augmented tools/call
// of a session, kept in that session's store only (as server-everything@1.0.0 keeps one per session), driven by
// Server.TaskRun. Its input requests (Ask) are delivered on the session's tasks/result streams, carrying the
// related-task key, under JSON-RPC ids of the double's own (numbers); an answer is POSTed back as a response.
type Task struct {
	ID      string
	Session string
	Call    Request // the tools/call that created it

	s       *Server
	mu      sync.Mutex
	status  string
	message string
	result  json.RawMessage
	rpcErr  *RPCError
	queue   [][]byte
	waiting map[string]chan []byte
	changed chan struct{}
	created time.Time
	updated time.Time
}

// Status sets the task's status and statusMessage.
func (t *Task) Status(status, message string) {
	t.mu.Lock()
	if !terminal(t.status) {
		t.status, t.message, t.updated = status, message, time.Now()
	}
	t.mu.Unlock()
	t.notify()
}

// Ask queues an input request (elicitation/create, sampling/createMessage) for the task's tasks/result streams,
// sets input_required, and waits up to wait for the client's answer: the whole JSON-RPC response as POSTed. ok is
// false when none came, or the task ended first.
func (t *Task) Ask(method string, params map[string]any, wait time.Duration) (answer []byte, ok bool) {
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["io.modelcontextprotocol/related-task"] = map[string]any{"taskId": t.ID}
	params["_meta"] = meta
	t.s.mu.Lock()
	t.s.nextReq++
	id := 1000 + t.s.nextReq
	t.s.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return t.AskRaw(strconv.Itoa(id), msg, wait)
}

// AskRaw is Ask with the request written as msg (any bytes), its id key idKey (the id as JSON, a number's digits).
func (t *Task) AskRaw(idKey string, msg []byte, wait time.Duration) ([]byte, bool) {
	ch := make(chan []byte, 1)
	t.mu.Lock()
	t.waiting[idKey] = ch
	t.queue = append(t.queue, msg)
	if !terminal(t.status) {
		t.status, t.updated = "input_required", time.Now()
	}
	t.mu.Unlock()
	t.notify()
	select {
	case a := <-ch:
		t.Status("working", "")
		return a, true
	case <-time.After(wait):
		return nil, false
	}
}

// Complete ends the task completed (or failed when result's isError is true, as a 2025-11-25 receiver reports it)
// with result, a CallToolResult; a cancelled task stays cancelled.
func (t *Task) Complete(result json.RawMessage) {
	t.mu.Lock()
	if !terminal(t.status) {
		t.status, t.result, t.updated = "completed", result, time.Now()
		var r struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(result, &r) == nil && r.IsError {
			t.status = "failed"
		}
	}
	t.mu.Unlock()
	t.notify()
}

// Fail ends the task failed, its tasks/result the JSON-RPC error e.
func (t *Task) Fail(e *RPCError) {
	t.mu.Lock()
	if !terminal(t.status) {
		t.status, t.rpcErr, t.updated = "failed", e, time.Now()
	}
	t.mu.Unlock()
	t.notify()
}

// State is the task's status.
func (t *Task) State() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Task) notify() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

func terminal(s string) bool { return s == "completed" || s == "failed" || s == "cancelled" }

// object is the task as tasks/get shows it (Result & Task).
func (t *Task) object() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := map[string]any{"taskId": t.ID, "status": t.status, "createdAt": t.created.UTC().Format(time.RFC3339Nano),
		"lastUpdatedAt": t.updated.UTC().Format(time.RFC3339Nano), "ttl": 300000, "pollInterval": t.s.pollInterval()}
	if t.message != "" {
		o["statusMessage"] = t.message
	}
	return o
}

func (s *Server) pollInterval() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.TaskPollInterval > 0 {
		return s.TaskPollInterval
	}
	return 100
}

// Tasks are the tasks the double created, in no order.
func (s *Server) Tasks() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

// Answers is the number of JSON-RPC responses with this id (its JSON) the double received.
func (s *Server) Answers(id string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == "" && r.HTTPMethod == http.MethodPost && string(bytes.TrimSpace(r.ID)) == id {
			n++
		}
	}
	return n
}

// answer routes a POSTed response to the task of the session waiting for it.
func (s *Server) answer(session string, id json.RawMessage, body []byte) {
	key := string(bytes.TrimSpace(id))
	s.mu.Lock()
	var hit *Task
	for _, t := range s.tasks {
		if t.Session != session {
			continue
		}
		t.mu.Lock()
		if _, ok := t.waiting[key]; ok {
			hit = t
		}
		t.mu.Unlock()
	}
	s.mu.Unlock()
	if hit == nil {
		return
	}
	hit.mu.Lock()
	ch := hit.waiting[key]
	delete(hit.waiting, key)
	hit.mu.Unlock()
	ch <- bytes.Clone(body)
}

// task is the session's task id names, nil when the session has none of that id.
func (s *Server) task(session string, params json.RawMessage) *Task {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[p.TaskID]
	if t == nil || t.Session != session {
		return nil
	}
	return t
}

// serveTask serves the task methods and a task-augmented tools/call; false for anything else.
func (s *Server) serveTask(w http.ResponseWriter, hr *http.Request, req Request) bool {
	switch req.Method {
	case "tools/call":
		var p struct {
			Task json.RawMessage `json:"task"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if len(p.Task) == 0 {
			return false
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		now := time.Now()
		t := &Task{ID: "up-task-" + hex.EncodeToString(b), Session: req.SessionID, Call: req, s: s, status: "working",
			waiting: map[string]chan []byte{}, changed: make(chan struct{}, 1), created: now, updated: now}
		s.mu.Lock()
		s.tasks[t.ID] = t
		run := s.TaskRun
		s.mu.Unlock()
		go run(t)
		res, _ := json.Marshal(map[string]any{"task": t.object()})
		s.reply(w, req, Reply{Result: res}, "")
		return true
	case "tasks/get":
		t := s.task(req.SessionID, req.Params)
		if t == nil {
			s.reply(w, req, Reply{Error: &RPCError{Code: -32602, Message: "Failed to retrieve task: Task not found"}}, "")
			return true
		}
		res, _ := json.Marshal(t.object())
		s.reply(w, req, Reply{Result: res}, "")
		return true
	case "tasks/cancel":
		t := s.task(req.SessionID, req.Params)
		if t == nil {
			s.reply(w, req, Reply{Error: &RPCError{Code: -32602, Message: "Task not found"}}, "")
			return true
		}
		s.mu.Lock()
		ignore := s.IgnoreCancel
		s.mu.Unlock()
		t.mu.Lock()
		if terminal(t.status) {
			t.mu.Unlock()
			s.reply(w, req, Reply{Error: &RPCError{Code: -32602, Message: "Cannot cancel task in terminal status"}}, "")
			return true
		}
		if !ignore {
			t.status, t.updated = "cancelled", time.Now()
		}
		t.mu.Unlock()
		t.notify()
		res, _ := json.Marshal(t.object())
		s.reply(w, req, Reply{Result: res}, "")
		return true
	case "tasks/result":
		t := s.task(req.SessionID, req.Params)
		if t == nil {
			s.reply(w, req, Reply{Error: &RPCError{Code: -32602, Message: "Task not found"}}, "")
			return true
		}
		s.taskResult(w, hr, req, t)
		return true
	}
	return false
}

// taskResult serves tasks/result as an event stream: the task's queued requests, then, once it is terminal, its
// result with the related-task key (or its JSON-RPC error).
func (s *Server) taskResult(w http.ResponseWriter, hr *http.Request, req Request, t *Task) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	write := func(msg []byte) {
		_, _ = fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", s.eventID(), msg)
		if fl != nil {
			fl.Flush()
		}
	}
	for {
		t.mu.Lock()
		queued := t.queue
		t.queue = nil
		status, result, rpcErr := t.status, t.result, t.rpcErr
		t.mu.Unlock()
		for _, m := range queued {
			write(m)
		}
		if terminal(status) {
			id := req.ID
			m := map[string]any{"jsonrpc": "2.0", "id": id}
			switch {
			case rpcErr != nil:
				m["error"] = rpcErr
			case result != nil:
				var r map[string]any
				_ = json.Unmarshal(result, &r)
				meta, _ := r["_meta"].(map[string]any)
				if meta == nil {
					meta = map[string]any{}
				}
				meta["io.modelcontextprotocol/related-task"] = map[string]any{"taskId": t.ID}
				r["_meta"] = meta
				m["result"] = r
			default:
				m["error"] = &RPCError{Code: -32603, Message: "Task has no result stored"}
			}
			b, _ := json.Marshal(m)
			write(b)
			return
		}
		select {
		case <-hr.Context().Done():
			return
		case <-t.changed:
		case <-time.After(50 * time.Millisecond):
		}
	}
}
