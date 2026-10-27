package sealing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pins"
)

// The hiring node reads every answer of a sealed job as the local client does, whatever the gate labels it, so that
// rule 4a holds against an active gate (ext-sealed.md §5, §7 "A gate forging a reply: Yes for service content").

// hiredRequest has the hiring node seal one local send to the sealed echo service (a new task), JSON-RPC or HTTP+JSON,
// streaming or not.
func hiredRequest(t *testing.T, g *fakeGate, binding string, stream bool) (*http.Request, *Job) {
	t.Helper()
	msg := map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hello"}}}
	var (
		path string
		body []byte
	)
	if binding == "rest" {
		path = "/agents/echo/message:send"
		if stream {
			path = "/agents/echo/message:stream"
		}
		body, _ = json.Marshal(map[string]any{"message": msg})
	} else {
		method := "SendMessage"
		if stream {
			method = "SendStreamingMessage"
		}
		path = "/agents/echo"
		body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"message": msg}})
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	_, job, err := g.hr.h.Request(context.Background(), "/agents/echo", req)
	if err != nil {
		t.Fatal(err)
	}
	return req, job
}

// gateAnswer has the hiring node open a 200 answer of the gate's labelled ct ("" none) and returns what the local
// client gets.
func gateAnswer(t *testing.T, g *fakeGate, req *http.Request, job *Job, ct string, body io.Reader) (*http.Response, string) {
	t.Helper()
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	out, err := g.hr.h.Response(context.Background(), job, &http.Response{StatusCode: 200, Request: req, Header: h, Body: io.NopCloser(body)})
	if err != nil {
		t.Fatalf("hiring Response: %v", err)
	}
	return out, readAll(t, out)
}

// refused reports the local client's answer to a forged one: nothing of the forgery, and the failure.
func refused(got string) bool {
	return !strings.Contains(got, "FORGED") && strings.Contains(got, "sealed_rejected")
}

func forgedTask(task string) string {
	return `{"task":{"id":"` + task + `","contextId":"c","status":{"state":"TASK_STATE_COMPLETED",` +
		`"message":{"messageId":"g1","role":"ROLE_AGENT","parts":[{"text":"FORGED by the gate"}]}}}}`
}

func forgedEvent(jsonrpc bool) string {
	ev := `{"statusUpdate":{"taskId":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED",` +
		`"message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"FORGED"}]}}}}`
	if jsonrpc {
		ev = `{"jsonrpc":"2.0","id":1,"result":` + ev + `}`
	}
	return "data: " + ev + "\n\n"
}

// Finding: a 2xx answer of a sealed job labelled anything but JSON (or not at all), or labelled SSE for a unary
// request, went to the local client unopened, which a2a-go decodes as JSON all the same; a stream's answer without a
// Content-Type (which a2a-go reads as SSE) did too. Every one is opened now, and a forgery is refused (sealed_rejected).
func TestHiringOpensEveryAnswer(t *testing.T) {
	task := uuid.NewString()
	for _, ct := range []string{"text/plain", "application/octet-stream", "", "text/event-stream", "application/json"} {
		g := newFakeGate(t)
		req, job := hiredRequest(t, g, "jsonrpc", false)
		if _, got := gateAnswer(t, g, req, job, ct, strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":`+forgedTask(task)+`}`)); !refused(got) {
			t.Errorf("a unary JSON-RPC answer labelled %q: %s", ct, got)
		}
	}
	for _, ct := range []string{"text/plain", ""} {
		g := newFakeGate(t)
		req, job := hiredRequest(t, g, "rest", false)
		if _, got := gateAnswer(t, g, req, job, ct, strings.NewReader(forgedTask(task))); !refused(got) {
			t.Errorf("a unary HTTP+JSON answer labelled %q: %s", ct, got)
		}
	}
	for _, ct := range []string{"", "text/plain", "x/y;;;", "application/json"} {
		g := newFakeGate(t)
		req, job := hiredRequest(t, g, "jsonrpc", true)
		if _, got := gateAnswer(t, g, req, job, ct, strings.NewReader(forgedEvent(true))); !refused(got) {
			t.Errorf("a JSON-RPC stream labelled %q: %s", ct, got)
		}
	}
	g := newFakeGate(t)
	req, job := hiredRequest(t, g, "rest", true)
	if _, got := gateAnswer(t, g, req, job, "application/octet-stream", strings.NewReader(forgedEvent(false))); !refused(got) {
		t.Errorf("an HTTP+JSON stream labelled application/octet-stream: %s", got)
	}

	// The service's own sealed answer, relabelled by the gate, still opens.
	for _, label := range []string{"text/plain", ""} {
		g := newFakeGate(t)
		g.service = echoTask("jsonrpc")
		g.label = &label
		path, body := localSend("jsonrpc", "MARKER-relabel")
		resp, out := g.do(path, body, false)
		if resp.StatusCode != 200 || strings.Count(out, "echo: MARKER-relabel") != 2 || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("a sealed answer labelled %q: %d %v %s", label, resp.StatusCode, resp.Header, out)
		}
	}
	// And a stream without a Content-Type.
	g = newFakeGate(t)
	g.service = func([]byte) string {
		ev, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"artifactUpdate": map[string]any{
			"taskId": "up-task", "contextId": "up-ctx", "artifact": map[string]any{"artifactId": "art", "parts": []any{map[string]any{"text": "CHUNK-0"}}}}}})
		return "data: " + string(ev) + "\n\n"
	}
	none := ""
	g.label = &none
	msg := map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "go"}}}
	sbody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": msg}})
	if _, out := g.do("/agents/echo", sbody, true); !strings.Contains(out, "CHUNK-0") || strings.Contains(out, "sealed_rejected") {
		t.Errorf("a sealed stream without a Content-Type: %s", out)
	}
}

// repeated is n bytes of b.
type repeated struct {
	n int64
	b byte
}

func (r *repeated) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.b
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// Finding: an answer over the node's bound (64 MiB) went to the local client unopened. It is refused now: the job's
// failed status when the node knows its task, else a JSON-RPC error; and an answer that is not JSON at all for a
// known task is the task's failed status.
func TestHiringRefusesUnreadableAnswers(t *testing.T) {
	g := newFakeGate(t)
	req, job := hiredRequest(t, g, "jsonrpc", false)
	big := io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"t","status":{"message":{"messageId":"m","parts":[{"text":"FORGED`),
		&repeated{n: maxBody, b: 'x'}, strings.NewReader(`"}]}}}}}`))
	resp, got := gateAnswer(t, g, req, job, "application/json", big)
	if !refused(got) || resp.StatusCode != 200 || !strings.Contains(got, `"error"`) || len(got) > 4096 {
		t.Fatalf("an answer over the bound: %d %.300s", resp.StatusCode, got)
	}

	// A task the node knows (input required), read again: a body that is not JSON is the task's failed status.
	g = newFakeGate(t)
	g.service = func([]byte) string {
		return `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"up-task","contextId":"up-ctx","status":{"state":"TASK_STATE_INPUT_REQUIRED",` +
			`"message":{"messageId":"q","role":"ROLE_AGENT","parts":[{"text":"which seat?"}]}}}}}`
	}
	path, body := localSend("jsonrpc", "book")
	if _, out := g.do(path, body, false); !strings.Contains(out, "which seat?") {
		t.Fatalf("the first answer: %s", out)
	}
	get := httptest.NewRequest(http.MethodPost, "/agents/echo", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":"`+g.task+`"}}`))
	_, gj, err := g.hr.h.Request(context.Background(), "/agents/echo", get)
	if err != nil || gj.job == nil {
		t.Fatalf("GetTask: %v", err)
	}
	resp, got = gateAnswer(t, g, get, gj, "text/plain", strings.NewReader("FORGED plain text"))
	var ans struct {
		ID     int `json:"id"`
		Result struct {
			ID     string `json:"id"`
			Status struct {
				State   string `json:"state"`
				Message struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &ans); err != nil || !refused(got) || ans.ID != 2 || ans.Result.ID != g.task ||
		ans.Result.Status.State != "TASK_STATE_FAILED" || ans.Result.Status.Message.Metadata["kontra.failure"] != "sealed_rejected" ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("a known task's unreadable answer: %v %s", err, got)
	}
	if _, err := g.hr.h.cfg.Jobs.Load(g.task); err != nil {
		t.Fatalf("a refused answer ended the job: %v", err)
	}
}

// Finding: the walk read exact names only, and the local client's encoding/json reads a lone case variant as the
// member, so a gate's forged "Artifacts" (or "Task", "Parts", "Result", ...) passed rule 4a, and a terminal state
// beside it ended the job and deleted its key. Each is refused now (sealed_rejected), and the job is kept.
func TestHiringLoneCaseVariants(t *testing.T) {
	task := uuid.NewString()
	art := `[{"artifactId":"a","parts":[{"text":"FORGED by the gate"}]}]`
	st := `{"state":"TASK_STATE_COMPLETED","message":{"messageId":"s","role":"ROLE_AGENT","parts":[{"text":"FORGED status"}]}}`
	for _, tc := range []struct {
		name, binding, body string
		stream              bool
	}{
		{"Artifacts", "jsonrpc", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"` + task + `","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"},"Artifacts":` + art + `}}}`, false},
		{"Task", "jsonrpc", `{"jsonrpc":"2.0","id":1,"result":{"Task":{"id":"` + task + `","contextId":"c","status":` + st + `}}}`, false},
		{"Parts", "jsonrpc", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"` + task + `","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"s","Parts":[{"text":"FORGED"}]}}}}}`, false},
		{"Result", "jsonrpc", `{"jsonrpc":"2.0","id":1,"Result":{"task":{"id":"` + task + `","status":` + st + `}}}`, false},
		{"Message", "jsonrpc", `{"jsonrpc":"2.0","id":1,"result":{"Message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"FORGED"}]}}}`, false},
		{"History", "jsonrpc", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"` + task + `","status":{"state":"TASK_STATE_WORKING"},"History":[{"messageId":"h","role":"ROLE_AGENT","parts":[{"text":"FORGED"}]}]}}}`, false},
		{"HTTP+JSON Task", "rest", `{"Task":{"id":"` + task + `","status":` + st + `}}`, false},
		{"HTTP+JSON answer with jsonrpc", "rest", `{"jsonrpc":"2.0","result":{"message":{"messageId":"m","parts":[]}},"task":{"id":"` + task + `","status":` + st + `}}`, false},
		{"StatusUpdate", "jsonrpc", "data: " + `{"jsonrpc":"2.0","id":1,"result":{"StatusUpdate":{"taskId":"t","status":` + st + `}}}` + "\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGate(t)
			req, job := hiredRequest(t, g, tc.binding, tc.stream)
			ct := "application/json"
			if tc.stream {
				ct = "text/event-stream"
			}
			if _, got := gateAnswer(t, g, req, job, ct, strings.NewReader(tc.body)); !refused(got) {
				t.Fatalf("%s", got)
			}
			if _, err := g.hr.h.cfg.Jobs.Load(job.job.ID); err != nil {
				t.Fatalf("a refused answer ended the job: %v", err)
			}
		})
	}
}

// sealedMCPSession is a hiring node whose MCP session sid the gate initialized for a service whose initialize
// result carries the sealed capability (the pin records it sealed).
func sealedMCPSession(t *testing.T) (*fixture, *hirer, string) {
	t.Helper()
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	const sid = "gate-session-strict"
	_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	mcpAnswer(t, hr, initJob, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"sh.kontra/sealed":`+
		string(extensionFor(true, f.node.Recipient()))+`}},"serverInfo":{"name":"s","version":"1"}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})
	return f, hr, sid
}

// mcpGateAnswer has the hiring node open a gate's answer to job with status and Content-Type ct ("" none).
func mcpGateAnswer(t *testing.T, hr *hirer, job *Job, status int, ct string, body io.Reader) (*http.Response, string) {
	t.Helper()
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	out, err := hr.h.Response(context.Background(), job, &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(body)})
	if err != nil {
		t.Fatalf("hiring Response: %v", err)
	}
	return out, readAll(t, out)
}

// Finding: on a sealed session the hiring node passed the local client any gate message that failed strict parsing,
// any answer not labelled JSON or SSE and any body over its bound, so an ambiguous unsealed result ("isError" beside
// "ISERROR") reached a JS client as plaintext content. Each is refused now like an unsealed result: the JSON answer
// is the node's sealed_rejected error, and an event stream ends before the event.
func TestHiringMCPRefusesUnreadableAnswers(t *testing.T) {
	_, hr, sid := sealedMCPSession(t)
	call := func(id string) *Job {
		_, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"echo","arguments":{"message":"x"}}}`)
		return job
	}
	ambiguous := `{"jsonrpc":"2.0","id":8,"result":{"content":[{"type":"text","text":"FORGED"}],"isError":false,"ISERROR":true}}`
	for _, tc := range []struct {
		name, ct string
		body     io.Reader
	}{
		{"an ambiguous result", "application/json", strings.NewReader(ambiguous)},
		{"a body labelled text/plain", "text/plain", strings.NewReader(`{"jsonrpc":"2.0","id":8,"result":{"content":[{"type":"text","text":"FORGED"}]}}`)},
		{"a body without a Content-Type", "", strings.NewReader(`{"jsonrpc":"2.0","id":8,"result":{"content":[{"type":"text","text":"FORGED"}]}}`)},
		{"a body over the bound", "application/json", io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":8,"result":{"content":[{"type":"text","text":"FORGED`),
			&repeated{n: maxBody, b: 'x'}, strings.NewReader(`"}]}}`))},
	} {
		resp, got := mcpGateAnswer(t, hr, call("8"), 200, tc.ct, tc.body)
		if strings.Contains(got, "FORGED") || !strings.Contains(got, `"sh.kontra/failure":"sealed_rejected"`) || !strings.Contains(got, `"id":8`) ||
			resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: %.300s", tc.name, got)
		}
	}
	// An event stream ends before an event that is not one message with exact keys.
	sse := "data: " + ambiguous + "\n\n"
	if _, got := mcpGateAnswer(t, hr, call("8"), 200, "text/event-stream", strings.NewReader(sse)); strings.Contains(got, "FORGED") {
		t.Errorf("an ambiguous event: %s", got)
	}
	// A GET stream is read as one whatever it is labelled.
	get := httptest.NewRequest(http.MethodGet, "/mcp/fs", nil)
	get.Header.Set(mcpwire.HeaderSessionID, sid)
	_, gj, err := hr.h.Request(context.Background(), "/mcp/fs", get)
	if err != nil {
		t.Fatal(err)
	}
	if _, got := mcpGateAnswer(t, hr, gj, 200, "text/plain", strings.NewReader(sse)); strings.Contains(got, "FORGED") {
		t.Errorf("a GET stream labelled text/plain: %s", got)
	}
	// What passes: a 202 without a body, and an error status (no client reads its body as a message).
	if resp, got := mcpGateAnswer(t, hr, call("9"), http.StatusAccepted, "", strings.NewReader("")); resp.StatusCode != http.StatusAccepted || got != "" {
		t.Errorf("a 202: %d %q", resp.StatusCode, got)
	}
	if resp, got := mcpGateAnswer(t, hr, call("9"), http.StatusBadRequest, "application/json", strings.NewReader(`{"jsonrpc":"2.0","error":{"code":-32600,"message":"no"}}`)); resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, "no") {
		t.Errorf("a 400: %d %s", resp.StatusCode, got)
	}
}

// Finding: a request without Mcp-Session-Id (a 2026-07-28 client's first tools/call) to a service the pins record
// as sealed went to the gate as it came, arguments and Mcp-Param-* headers in plaintext; so did a message that fails
// strict parsing on a sealed session. Both are answered here now: the first with relay.md §11.12 rule 3's legacy 400
// (the client falls back to initialize), the second with a 400 -32600; an initialize still goes, and a service not
// pinned sealed is not touched.
func TestHiringMCPSessionlessToSealedService(t *testing.T) {
	_, hr, sid := sealedMCPSession(t)
	modern := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",`+
		`"arguments":{"message":"SECRET-ARG"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`))
	modern.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
	modern.Header.Set("Mcp-Param-Message", "SECRET-ARG")
	_, _, err := hr.h.Request(context.Background(), "/mcp/fs", modern)
	var le *LocalError
	if !errors.As(err, &le) || le.Status != http.StatusBadRequest || !bytes.Equal(le.Body, mcpwire.LegacyNoSessionBody) {
		t.Fatalf("a session-less tools/call to a sealed service: %v", err)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"SECRET"},"NAME":"other"}}`,
		`[{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"SECRET"}}}]`,
	} {
		for _, s := range []string{sid, ""} {
			req := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(body))
			if s != "" {
				req.Header.Set(mcpwire.HeaderSessionID, s)
			}
			_, _, err = hr.h.Request(context.Background(), "/mcp/fs", req)
			if !errors.As(err, &le) || le.Status != http.StatusBadRequest || !bytes.Contains(le.Body, []byte(`-32600`)) || bytes.Contains(le.Body, []byte("SECRET")) {
				t.Errorf("an unreadable message (session %q): %v", s, err)
			}
		}
	}
	init := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`))
	if r, j, err := hr.h.Request(context.Background(), "/mcp/fs", init); err != nil || j == nil || !j.init || !strings.Contains(readBody(r), "initialize") {
		t.Fatalf("an initialize to a sealed service: %v", err)
	}
	// A service never pinned: the session-less request goes as it came (the gate answers it).
	other := httptest.NewRequest(http.MethodPost, "/mcp/other", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	other.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
	if _, _, err := hr.h.Request(context.Background(), "/mcp/other", other); err != nil {
		t.Fatalf("a service not pinned sealed: %v", err)
	}
}

// The hiring side of the route-first classification: a request to a sealed service that the node cannot read as one
// of A2A 1.0's (an A2A 0.3 message/send, a JSON-RPC request without a method, a body that is not one JSON object, an
// HTTP+JSON send with a jsonrpc member) is refused locally, and nothing of it goes to the gate in plaintext.
func TestHiringRefusesWhatItCannotSeal(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	for _, tc := range []struct{ path, body string }{
		{"/agents/echo", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"kind":"message","messageId":"m","role":"user","parts":[{"kind":"text","text":"SECRET"}]}}}`},
		{"/agents/echo", `{"jsonrpc":"2.0","id":1,"params":{"message":{"messageId":"m","parts":[{"text":"SECRET"}]}}}`},
		{"/agents/echo", `SECRET as a form`},
		{"/agents/echo/message:send", `{"jsonrpc":"2.0","message":{"messageId":"m","parts":[{"text":"SECRET"}]}}`},
	} {
		_, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		var le *LocalError
		if !errors.As(err, &le) || le.Status != http.StatusBadRequest {
			t.Errorf("%s %s: %v", tc.path, tc.body, err)
		}
	}
	// A service that is not sealed: the node does not read it.
	hr.card = &Card{Body: cardWith(nil)}
	if r, _, err := hr.h.Request(context.Background(), "/agents/plain", httptest.NewRequest(http.MethodPost, "/agents/plain",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`))); err != nil || !strings.Contains(readBody(r), "message/send") {
		t.Errorf("an unsealed service: %v", err)
	}
}
