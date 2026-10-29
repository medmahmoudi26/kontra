package mcptest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The double's 2026-07-28 era (spec/relay.md §11.12; M3 WP-07), served beside 2025-11-25 on the same endpoint as a
// dual-era server does (2026-07-28 Versioning, "Backward Compatibility"): a request whose MCP-Protocol-Version is
// 2026-07-28 and whose method is not initialize is modern. Without Modern the double is legacy-only and answers it as
// the TypeScript SDK's legacy transport answers a POST without a session: 400 with the -32000 body.
//
// Modern requests are checked as a 2026-07-28 server checks them (_meta's protocolVersion and clientCapabilities,
// MCP-Protocol-Version, Mcp-Method and Mcp-Name after Base64 decoding: -32602 and -32020), and recorded like every
// other. It serves server/discover, tools/list (ttlMs ModernTTL, cacheScope public), tools/call (OnCall, echo by
// default, which can answer an InputRequiredResult: a multi round-trip request), subscriptions/listen (one
// acknowledgement and its result), and, with ModernTaskRun, the tasks extension: it declares it in server/discover,
// answers a tools/call that declares it with a CreateTaskResult, and serves tasks/get (a DetailedTask), tasks/update
// and tasks/cancel from one process-wide store, as server-everything@2.0.0 does.

// ModernVersion is the modern revision.
const ModernVersion = "2026-07-28"

// HeaderMismatches counts the modern requests the double refused for their headers or _meta (a gate that forwards
// what it checked never makes one).
func (s *Server) HeaderMismatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mismatches
}

// HangsCut counts the requests OnModern held (Reply.Hang) whose client closed the exchange first: a gate that closes
// its upstream stream when its client closes its own (2026-07-28 "Cancellation").
func (s *Server) HangsCut() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hangsCut
}

// ModernRequests are the modern requests the double received (method, in order).
func (s *Server) ModernRequests() []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Header.Get("MCP-Protocol-Version") == ModernVersion && r.Method != "initialize" {
			out = append(out, r)
		}
	}
	return out
}

// ModernTasks are the double's modern tasks, in no order.
func (s *Server) ModernTasks() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.modernTasks))
	for _, t := range s.modernTasks {
		out = append(out, t)
	}
	return out
}

// InputRequired is an InputRequiredResult (MRTR): its inputRequests (key → {method, params}) and requestState ("":
// none).
func InputRequired(requests map[string]any, state string) json.RawMessage {
	r := map[string]any{"resultType": "input_required"}
	if len(requests) > 0 {
		r["inputRequests"] = requests
	}
	if state != "" {
		r["requestState"] = state
	}
	b, _ := json.Marshal(r)
	return b
}

// Elicit is an elicitation/create request object for an inputRequests map.
func Elicit(message string) map[string]any {
	return map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": message,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}}}}
}

// modernRequest reports whether a request is a modern one: a POST other than initialize with MCP-Protocol-Version
// 2026-07-28, or a GET or DELETE with it to a dual-era double (405).
func (s *Server) modernRequest(r *http.Request, method string) bool {
	if r.Header.Get("MCP-Protocol-Version") != ModernVersion || method == "initialize" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.Method == http.MethodPost || s.Modern
}

// serveModern serves a modern request; req is recorded already.
func (s *Server) serveModern(w http.ResponseWriter, hr *http.Request, req Request) {
	s.mu.Lock()
	modern, on := s.Modern, s.OnModern
	s.mu.Unlock()
	if !modern {
		writeJSON(w, http.StatusBadRequest, []byte(legacyNoSession))
		return
	}
	if hr.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if on != nil {
		if rep := on(req); rep != nil {
			if rep.Hang > 0 {
				select {
				case <-time.After(rep.Hang):
				case <-hr.Context().Done():
					s.mu.Lock()
					s.hangsCut++
					s.mu.Unlock()
				}
				return
			}
			s.reply(w, req, *rep, "")
			return
		}
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if code, msg := checkModern(hr.Header, req); code != 0 {
		s.mu.Lock()
		s.mismatches++
		s.mu.Unlock()
		s.reply(w, req, Reply{Error: &RPCError{Code: code, Message: msg}, Status: http.StatusBadRequest}, "")
		return
	}
	var rep Reply
	switch req.Method {
	case "server/discover":
		rep = s.discover()
	case "tools/list":
		rep = s.toolsList(req)
		s.mu.Lock()
		ttl := s.ModernTTL
		s.mu.Unlock()
		if ttl == 0 {
			ttl = 900000
		}
		var r map[string]any
		_ = json.Unmarshal(rep.Result, &r)
		if r != nil {
			r["resultType"], r["ttlMs"], r["cacheScope"] = "complete", ttl, "public"
			rep.Result, _ = json.Marshal(r)
		}
	case "tools/call":
		s.mu.Lock()
		run, on := s.ModernTaskRun, s.OnCall
		s.mu.Unlock()
		if run != nil && declaresTasks(req.Params) {
			rep = s.createModernTask(req, run)
			break
		}
		if on != nil {
			rep = on(req)
		} else {
			rep = echo(req)
		}
		if rep.Error == nil && rep.Raw == nil {
			var r map[string]any
			if json.Unmarshal(rep.Result, &r) == nil && r != nil {
				if _, ok := r["resultType"]; !ok {
					r["resultType"] = "complete"
					rep.Result, _ = json.Marshal(r)
				}
			}
		}
	case "tasks/get", "tasks/update", "tasks/cancel":
		rep = s.modernTaskMethod(req)
	case "subscriptions/listen":
		ack, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged",
			"params": map[string]any{"notifications": map[string]any{}}})
		rep = Reply{Result: json.RawMessage(`{"resultType":"complete"}`), Events: []string{fmt.Sprintf("event: message\ndata: %s\n\n", ack)}}
	case "ping":
		rep = Reply{Result: json.RawMessage(`{"resultType":"complete"}`)}
	default:
		s.mu.Lock()
		onReq := s.OnRequest
		s.mu.Unlock()
		if onReq != nil {
			rep = onReq(req)
		} else {
			rep = Reply{Error: &RPCError{Code: -32601, Message: "Method not found"}, Status: http.StatusNotFound}
		}
	}
	s.reply(w, req, rep, "")
}

// legacyNoSession is what the TypeScript SDK's legacy transport answers a POST without a session.
const legacyNoSession = `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}`

func writeJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// checkModern is a 2026-07-28 server's check of a request's headers against its body: -32602 for a missing _meta
// field, -32020 for a header that is missing or does not match.
func checkModern(h http.Header, req Request) (int, string) {
	var p struct {
		Meta   map[string]json.RawMessage `json:"_meta"`
		Name   *string                    `json:"name"`
		URI    *string                    `json:"uri"`
		TaskID *string                    `json:"taskId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	var pv string
	if raw, ok := p.Meta["io.modelcontextprotocol/protocolVersion"]; !ok || json.Unmarshal(raw, &pv) != nil {
		return -32602, "missing protocolVersion"
	}
	if _, ok := p.Meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
		return -32602, "missing clientCapabilities"
	}
	if pv != h.Get("MCP-Protocol-Version") {
		return -32020, "protocol version header mismatch"
	}
	if h.Get("Mcp-Method") != req.Method {
		return -32020, "Mcp-Method mismatch"
	}
	var want *string
	switch req.Method {
	case "tools/call", "prompts/get":
		want = p.Name
	case "resources/read":
		want = p.URI
	case "tasks/get", "tasks/update", "tasks/cancel":
		want = p.TaskID
	}
	if want != nil {
		got := h.Get("Mcp-Name")
		if strings.HasPrefix(got, "=?base64?") && strings.HasSuffix(got, "?=") {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(got, "=?base64?"), "?="))
			if err != nil {
				return -32020, "Mcp-Name malformed"
			}
			got = string(b)
		}
		if got != *want {
			return -32020, "Mcp-Name mismatch"
		}
	}
	return 0, ""
}

// declaresTasks reports whether a modern request's per-request capabilities declare the tasks extension.
func declaresTasks(params json.RawMessage) bool {
	var p struct {
		Meta struct {
			Caps struct {
				Extensions map[string]json.RawMessage `json:"extensions"`
			} `json:"io.modelcontextprotocol/clientCapabilities"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(params, &p)
	_, ok := p.Meta.Caps.Extensions["io.modelcontextprotocol/tasks"]
	return ok
}

// discover answers server/discover.
func (s *Server) discover() Reply {
	s.mu.Lock()
	versions, tasks, meta, caps := s.DiscoverVersions, s.ModernTaskRun != nil, s.InitMeta, s.ModernCapabilities
	s.mu.Unlock()
	if len(versions) == 0 {
		versions = []string{ModernVersion, Version}
	}
	if len(caps) == 0 {
		c := map[string]any{"tools": map[string]any{"listChanged": true}}
		if tasks {
			c["extensions"] = map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}
		}
		caps, _ = json.Marshal(c)
	}
	res := map[string]any{"resultType": "complete", "supportedVersions": versions, "capabilities": caps,
		"ttlMs": 3600000, "cacheScope": "public",
		"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "mcptest", "version": "2.0.0"}}}
	if len(meta) > 0 {
		var m map[string]any
		_ = json.Unmarshal(meta, &m)
		for k, v := range m {
			res["_meta"].(map[string]any)[k] = v
		}
	}
	b, _ := json.Marshal(res)
	return Reply{Result: b}
}

// createModernTask creates a task for a tools/call that declares the extension, answered with a CreateTaskResult.
func (s *Server) createModernTask(req Request, run func(t *Task)) Reply {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	now := time.Now()
	t := &Task{ID: "up-mtask-" + hex.EncodeToString(b), Call: req, s: s, status: "working", modern: true,
		waiting: map[string]chan []byte{}, inputs: map[string]json.RawMessage{}, changed: make(chan struct{}, 1),
		created: now, updated: now}
	s.mu.Lock()
	s.modernTasks[t.ID] = t
	s.mu.Unlock()
	go run(t)
	o := t.modernObject(false)
	o["resultType"] = "task"
	res, _ := json.Marshal(o)
	return Reply{Result: res}
}

// modernTaskMethod serves tasks/get, tasks/update and tasks/cancel (ext-tasks v0.2.2).
func (s *Server) modernTaskMethod(req Request) Reply {
	var p struct {
		TaskID         string                     `json:"taskId"`
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
	}
	_ = json.Unmarshal(req.Params, &p)
	s.mu.Lock()
	t := s.modernTasks[p.TaskID]
	ignore := s.IgnoreCancel
	s.mu.Unlock()
	if t == nil {
		return Reply{Error: &RPCError{Code: -32602, Message: "Task not found"}}
	}
	switch req.Method {
	case "tasks/get":
		o := t.modernObject(true)
		o["resultType"] = "complete"
		b, _ := json.Marshal(o)
		return Reply{Result: b}
	case "tasks/update":
		for k, v := range p.InputResponses {
			t.mu.Lock()
			ch, ok := t.waiting[k]
			if ok {
				delete(t.waiting, k)
				delete(t.inputs, k)
			}
			t.mu.Unlock()
			if ok {
				ch <- append([]byte(nil), v...)
			}
		}
		return Reply{Result: json.RawMessage(`{"resultType":"complete"}`)}
	default: // tasks/cancel
		if !ignore {
			t.mu.Lock()
			if !terminal(t.status) {
				t.status, t.updated = "cancelled", time.Now()
			}
			t.mu.Unlock()
			t.notify()
		}
		return Reply{Result: json.RawMessage(`{"resultType":"complete"}`)}
	}
}

// AskModern adds an input request under key to the task's inputRequests, sets input_required, and waits up to wait for
// the client's answer as tasks/update delivered it (the inputResponses value). ok is false when none came.
func (t *Task) AskModern(key string, request map[string]any, wait time.Duration) (json.RawMessage, bool) {
	b, _ := json.Marshal(request)
	ch := make(chan []byte, 1)
	t.mu.Lock()
	t.waiting[key] = ch
	t.inputs[key] = b
	if !terminal(t.status) {
		t.status, t.updated = "input_required", time.Now()
	}
	t.mu.Unlock()
	t.notify()
	select {
	case a := <-ch:
		t.mu.Lock()
		if len(t.inputs) == 0 && !terminal(t.status) {
			t.status, t.updated = "working", time.Now()
		}
		t.mu.Unlock()
		return a, true
	case <-time.After(wait):
		return nil, false
	}
}

// modernObject is the task as the extension shows it: a Task, and with detailed a DetailedTask (inputRequests when
// input_required, result when completed, error when failed).
func (t *Task) modernObject(detailed bool) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := map[string]any{"taskId": t.ID, "status": t.status, "createdAt": t.created.UTC().Format(time.RFC3339Nano),
		"lastUpdatedAt": t.updated.UTC().Format(time.RFC3339Nano), "ttlMs": 300000, "pollIntervalMs": t.s.pollInterval()}
	if t.message != "" {
		o["statusMessage"] = t.message
	}
	if !detailed {
		return o
	}
	switch t.status {
	case "input_required":
		reqs := map[string]json.RawMessage{}
		keys := make([]string, 0, len(t.inputs))
		for k := range t.inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			reqs[k] = t.inputs[k]
		}
		o["inputRequests"] = reqs
	case "completed":
		o["result"] = t.result
	case "failed":
		if t.rpcErr != nil {
			o["error"] = t.rpcErr
		} else {
			o["error"] = &RPCError{Code: -32603, Message: "failed"}
		}
	}
	return o
}
