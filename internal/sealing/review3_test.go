package sealing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// M3 review round 3: regression tests for the hosting and hiring nodes' sealing.

// r3-hosting-1 (D232): a sealed plain call's input request that reaches the session on another exchange than the
// call's own (a GET stream: the stdio bridge routes every request without a related task there; a URL server resumed
// with Last-Event-ID replays it there) is sealed as the session's one unanswered sealed call's, and the gate's
// plaintext answer to it is refused.
func TestReview3PlainCallInputOnGET(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-get")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-1")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "sampleLLM", "1", `{"prompt":"SECRET-PROMPT"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	get := f.h.MCPOn(mcpPre(s1, "", ""), "up-1") // the session's GET
	samp := fromServer(t, get, `{"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"Resource sampleLLM context: SECRET-PROMPT"}}],"maxTokens":100}}`)
	if strings.Contains(samp, "SECRET-PROMPT") || !strings.Contains(samp, mcpwire.MetaSealed) {
		t.Fatalf("a sealed plain call's sampling request on the session's GET is not sealed: %s", samp)
	}
	m, err := mcpwire.Parse([]byte(samp), mcpwire.FromUpstream)
	if err != nil {
		t.Fatal(err)
	}
	hd, plain := openInputFor(t, b, m)
	if hd.Tool != "sampleLLM" || hd.MessageID != "0" || hd.TaskID != "" || !strings.Contains(string(plain), "SECRET-PROMPT") {
		t.Fatalf("the input request's header %+v body %s", hd, plain)
	}
	ans := []byte(`{"jsonrpc":"2.0","id":0,"result":{"role":"assistant","content":{"type":"text","text":"GATE"},"model":"m"}}`)
	if out, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), "up-1"), ans); r != sealed.RejectBadHeader {
		t.Fatalf("the gate's plaintext answer to it: %q %s", r, out)
	}
}

// r3-hosting-1 (D232): with two sealed plain calls of the session unanswered, nothing tells whose an input request on
// the GET is: it is not relayed, and any answer to it is refused.
func TestReview3UnattributableInputIsNotRelayed(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-two")
	for _, id := range []string{"1", "2"} {
		x := f.h.MCPOn(mcpPre(s1, id, ""), "up-1")
		if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "sampleLLM", id, `{"prompt":"SECRET-PROMPT"}`, false)); reason != "" {
			t.Fatal(reason)
		}
		x.Accepted()
	}
	get := f.h.MCPOn(mcpPre(s1, "", ""), "up-1")
	out, keep, err := get.FromServer([]byte(`{"jsonrpc":"2.0","id":5,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"SECRET-PROMPT"}}],"maxTokens":10}}`))
	if err != nil || keep || len(out) > 0 {
		t.Fatalf("an input request no call can be told to have made is relayed: %v %v %s", keep, err, out)
	}
	ans := []byte(`{"jsonrpc":"2.0","id":5,"result":{"role":"assistant","content":{"type":"text","text":"GATE"},"model":"m"}}`)
	if out, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), "up-1"), ans); r != sealed.RejectBadHeader {
		t.Fatalf("an answer to an input request the node did not relay: %q %s", r, out)
	}
	// A sealed answer to it is refused too: no client saw it.
	sealedAns := []byte(`{"jsonrpc":"2.0","id":5,"result":{"_meta":{"sh.kontra/sealed":` +
		envelope(f.seal(f.answerHeader(b, s1, "sampleLLM", "5", ""), `{"role":"assistant"}`)) + `}}}`)
	if out, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), "up-1"), sealedAns); r != sealed.RejectWrongMessage {
		t.Fatalf("a sealed answer to an input request the node did not relay: %q %s", r, out)
	}
}

// r3-hosting-1 (D232): an input request that comes on the GET while the session's first sealed call is with the
// server, not recorded yet (a URL server that answers JSON sends its headers with its result), is the server's
// acceptance of that call: the call is recorded and the request sealed as its.
func TestReview3InputWhileFirstCallInFlight(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-first")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-1")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "sampleLLM", "1", `{"prompt":"SECRET-PROMPT"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	get := f.h.MCPOn(mcpPre(s1, "", ""), "up-1")
	samp := fromServer(t, get, `{"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"SECRET-PROMPT"}}],"maxTokens":10}}`)
	if strings.Contains(samp, "SECRET-PROMPT") || !strings.Contains(samp, mcpwire.MetaSealed) {
		t.Fatalf("an input request during the session's first sealed call: %s", samp)
	}
	x.Accepted() // the call's answer's headers, later: nothing more to record
	res := fromServer(t, x, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"SECRET-RESULT"}],"isError":false}}`)
	if strings.Contains(res, "SECRET-RESULT") {
		t.Fatalf("the call's result: %s", res)
	}
}

// openInputFor opens a sealed input request's envelope (params._meta["sh.kontra/sealed"]) with b's key.
func openInputFor(t *testing.T, b buyer, m *mcpwire.Message) (sealed.Header, []byte) {
	t.Helper()
	meta, _ := mcpwire.Member(m.Params, "_meta")
	env, _ := mcpwire.Member(meta, mcpwire.MetaSealed)
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		t.Fatalf("not a sealed input request: %v", err)
	}
	hd, body := openMCP(t, b, ct)
	return hd, []byte(body)
}

// answerHeader is the header of b's answer to the input request msgID (of the gate task taskID, "" for a plain
// call's).
func (f *fixture) answerHeader(b buyer, session, tool, msgID, taskID string) sealed.Header {
	hd := f.mcpHeader(b, session, tool, msgID)
	hd.TaskID, hd.Op = taskID, sealed.OpAnswer
	return hd
}

// r3Buyer opens the buyer's sealed job T1 in gate context ctx-buyer on service echo, answered with upstream task up-B
// in upstream context upctx-B.
func r3Buyer(t *testing.T, f *fixture) buyer {
	t.Helper()
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-0000000000b1", "ctx-buyer"
	body := send("mB", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "mB", 0, 1), "BUYER-SECRET-INPUT")))
	_, x, r := f.request(pre(task, ctx), body)
	if r != "" {
		t.Fatal(r)
	}
	out := readAll(t, f.answer(x, http.StatusOK, taskAnswer("up-B", "upctx-B", "TASK_STATE_COMPLETED", "done", "BUYER-SECRET-RESULT")))
	if strings.Contains(out, "BUYER-SECRET") {
		t.Fatalf("the buyer's own reply: %s", out)
	}
	return b
}

// r3Do runs Request for an HTTP+JSON request target as the gate writes it (no cleaning), and the reason.
func (f *fixture) r3Do(p tunnel.Preamble, method, target string) (*Exchange, sealed.Reject) {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://node.invalid"+target, nil)
	_, x, err := f.h.Request(context.Background(), p, r)
	if err != nil {
		reason, _ := sealed.ReasonOf(err)
		return x, reason
	}
	return x, ""
}

// r3-hosting-4 (D235): the sealing classifies the path as the node forwards it (cleaned): a dot segment hides neither a
// task list (D227) nor a push config route (D217).
func TestReview3DotSegmentRoutes(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const t1, ctx = "0193c0de-0000-7000-8000-0000000000b9", "ctx-buyer"
	body := send("mB", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "mB", 0, 1), "BUYER-SECRET-INPUT")))
	_, x, r := f.request(pre(t1, ctx), body)
	if r != "" {
		t.Fatal(r)
	}
	x.handed.Store(true)
	x.End()
	p := pre("0193c0de-0000-7000-8000-0000000000ea", "ctx-gate")
	for _, target := range []string{"/tasks?historyLength=10", "/tasks/x/..?historyLength=10", "/tasks/x/../?historyLength=10", "/a/../tasks"} {
		if _, r := f.r3Do(p, http.MethodGet, target); r != sealed.RejectBadHeader {
			t.Errorf("GET %s: %q", target, r)
		}
	}
	f2 := newFixture(t)
	r3Buyer(t, f2)
	const tb = "0193c0de-0000-7000-8000-0000000000b1"
	for _, target := range []string{"/tasks/up-B/pushNotificationConfigs", "/tasks/up-B/./pushNotificationConfigs",
		"/tasks/up-B/x/../pushNotificationConfigs", "/tasks/up-B//pushNotificationConfigs"} {
		if _, r := f2.r3Do(pre(tb, "ctx-buyer"), http.MethodPost, target); r != sealed.RejectBadHeader {
			t.Errorf("POST %s: %q", target, r)
		}
	}
}

// r3-hosting-7 (D237): a message's referenceTaskIds (A2A 1.0) naming another sealed job's upstream task is wrong_task,
// sealed or not; an unsealed message with any reference, to a service with sealed jobs, is bad_header.
func TestReview3ReferenceTaskIDs(t *testing.T) {
	f := newFixture(t)
	r3Buyer(t, f)
	msg := func(ref string) []byte {
		m := map[string]any{"messageId": "mR", "role": "ROLE_USER", "referenceTaskIds": []string{ref},
			"parts": []any{map[string]any{"text": "summarize the referenced task"}}}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "SendMessage", "params": map[string]any{"message": m}})
		return b
	}
	if fwd, _, r := f.request(pre("0193c0de-0000-7000-8000-0000000000e3", "ctx-other"), msg("up-B")); r != sealed.RejectWrongTask {
		t.Fatalf("an unsealed message referencing the buyer's sealed task: %q %s", r, fwd)
	}
	if fwd, _, r := f.request(pre("0193c0de-0000-7000-8000-0000000000e4", "ctx-other"), msg("up-unknown")); r != sealed.RejectBadHeader {
		t.Fatalf("an unsealed message with a reference, to a service with sealed jobs: %q %s", r, fwd)
	}
	// The gate's own sealed first message referencing the buyer's task.
	g := newBuyer(t)
	const gtask, gctx = "0193c0de-0000-7000-8000-0000000000e5", "ctx-g"
	body := send("mG", "", "", g.replyTo(), sealedPartJSON(f.seal(f.reqHeader(g, "", gctx, "mG", 0, 1), "summarize the referenced task")))
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["params"].(map[string]any)["message"].(map[string]any)["referenceTaskIds"] = []string{"up-B"}
	body, _ = json.Marshal(m)
	if fwd, _, r := f.request(pre(gtask, gctx), body); r != sealed.RejectWrongTask {
		t.Fatalf("a sealed message referencing another job's sealed task: %q %s", r, fwd)
	}
}

// r3-hosting-9: on an exchange of a job whose upstream task is not known (recorded by End after its first message's
// exchange was cut), a request naming another sealed job's upstream task is wrong_task.
func TestReview3OtherChecksIndexWithoutUpstreamTask(t *testing.T) {
	f := newFixture(t)
	r3Buyer(t, f)
	g := newBuyer(t)
	const tg, gctx = "0193c0de-0000-7000-8000-0000000000ef", "ctx-gate"
	body := send("mG", "", "", g.replyTo(), sealedPartJSON(f.seal(f.reqHeader(g, "", gctx, "mG", 0, 1), "x")))
	_, x, r := f.request(pre(tg, gctx), body)
	if r != "" {
		t.Fatal(r)
	}
	x.handed.Store(true)
	x.End()
	cancel := []byte(`{"jsonrpc":"2.0","id":7,"method":"CancelTask","params":{"id":"up-B"}}`)
	if fwd, _, r := f.request(pre(tg, gctx), cancel); r != sealed.RejectWrongTask {
		t.Fatalf("CancelTask of the buyer's upstream task on the gate's own job's exchange: %q %s", r, fwd)
	}
}

// r3-hiring-3 (D244): a CreateTaskResult answers only a sealed call that asked for a task, and holds nothing a client
// would read as the tool's result: a plain call answered with {"task", "content"} is refused, as JSON and as SSE.
func TestReview3ForgedCreateTaskResult(t *testing.T) {
	_, hr, sid := sealedMCPSession(t)
	forged := `{"jsonrpc":"2.0","id":5,"result":{"task":{"taskId":"forged-task","status":"working"},"content":[{"type":"text","text":"FORGED-BY-GATE"}],"isError":false}}`
	_, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"}}}`)
	if got := mcpAnswer(t, hr, job, forged, false, nil); strings.Contains(got, "FORGED-BY-GATE") || strings.Contains(got, "forged-task") {
		t.Fatalf("a plain call's forged CreateTaskResult (JSON): %s", got)
	}
	_, job2 := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"}}}`)
	if got := mcpAnswer(t, hr, job2, "data: "+strings.Replace(forged, `"id":5`, `"id":6`, 1)+"\n\n", true, nil); strings.Contains(got, "FORGED-BY-GATE") {
		t.Fatalf("a plain call's forged CreateTaskResult (SSE): %s", got)
	}
	// A call that asked for a task: a CreateTaskResult with content is refused, one with the task only passes.
	_, job3 := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"},"task":{"ttl":60000}}}`)
	if got := mcpAnswer(t, hr, job3, strings.Replace(forged, `"id":5`, `"id":7`, 1), false, nil); strings.Contains(got, "FORGED-BY-GATE") {
		t.Fatalf("a task call's CreateTaskResult with content: %s", got)
	}
	_, job4 := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"},"task":{"ttl":60000}}}`)
	ok := `{"jsonrpc":"2.0","id":8,"result":{"task":{"taskId":"t-8","status":"working","createdAt":"2026-10-28T00:00:00Z","ttl":60000}}}`
	if got := mcpAnswer(t, hr, job4, ok, false, nil); !strings.Contains(got, `"t-8"`) {
		t.Fatalf("a task call's CreateTaskResult: %s", got)
	}
}

// r3-hiring-6 (D241): a session's job is never dropped while one of its exchanges is open (the sweep with the gate's
// session_idle_s, a gate 404 on another exchange), and a result that answers no tracked request is never relayed on a
// GET, or on a session the node no longer holds: the gate's plaintext result for the pending sealed call is refused.
func TestReview3OpenGETAfterSessionJobDropped(t *testing.T) {
	for _, how := range []string{"sweep", "404"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t)
			hr := newHirer(t, f, pins.Options{})
			const sid = "gate-session-drop"
			_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
			mcpAnswer(t, hr, initJob, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"sh.kontra/sealed":`+
				string(extensionFor(true, f.node.Recipient()))+`}},"serverInfo":{"name":"s","version":"1"},"_meta":{"sh.kontra/session_idle_s":1}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})
			get := httptest.NewRequest(http.MethodGet, "/mcp/fs", nil)
			get.Header.Set(mcpwire.HeaderSessionID, sid)
			_, gj, err := hr.h.Request(context.Background(), "/mcp/fs", get)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"}}}`)
			switch how {
			case "sweep":
				hr.now = hr.now.Add(time.Hour)
				hr.h.Sweep()
			case "404":
				_, pj := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)
				if _, err := hr.h.Response(context.Background(), pj, &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}); err != nil {
					t.Fatal(err)
				}
				pj.Done()
			}
			forged := "data: " + `{"jsonrpc":"2.0","id":9,"result":{"content":[{"type":"text","text":"FORGED-UNSEALED-RESULT"}],"isError":false}}` + "\n\n"
			if _, got := mcpGateAnswer(t, hr, gj, 200, "text/event-stream", strings.NewReader(forged)); strings.Contains(got, "FORGED-UNSEALED-RESULT") {
				t.Fatalf("the open GET hands the local client the gate's plaintext result for the pending sealed call: %s", got)
			}
		})
	}
}

// r3-hiring-7 (D245): the gate cannot put a second local client into a sealed session (an initialize result naming a
// session id the node holds is refused), and a request reusing a sealed call's id is refused here rather than deleting
// its record: the gate's plaintext result for the call never passes on the session's GET.
func TestReview3SessionFixation(t *testing.T) {
	f, hr, sid := sealedMCPSession(t)
	get := httptest.NewRequest(http.MethodGet, "/mcp/fs", nil)
	get.Header.Set(mcpwire.HeaderSessionID, sid)
	_, gj, err := hr.h.Request(context.Background(), "/mcp/fs", get)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET-A"}}}`)
	_, initB := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"b","version":"1"}}}`)
	ib := mcpAnswer(t, hr, initB, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"sh.kontra/sealed":`+
		string(extensionFor(true, f.node.Recipient()))+`}},"serverInfo":{"name":"s","version":"1"}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})
	if !strings.Contains(ib, `"error"`) {
		t.Fatalf("a second initialize answered with an existing session id: %s", ib)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set(mcpwire.HeaderSessionID, sid)
	if _, _, err := hr.h.Request(context.Background(), "/mcp/fs", req); err == nil {
		t.Fatal("a request reusing a sealed call's id is sent")
	}
	forged := "data: " + `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"FORGED-FOR-A"}],"isError":false}}` + "\n\n"
	if _, got := mcpGateAnswer(t, hr, gj, 200, "text/event-stream", strings.NewReader(forged)); strings.Contains(got, "FORGED-FOR-A") {
		t.Fatalf("A's GET hands it the gate's plaintext result for A's sealed call: %s", got)
	}
}

// r3-hiring-2 (D240): an MCP session id is never an A2A service's path: a gate that answers an MCP initialize with
// Mcp-Session-Id "/agents/echo" gets the initialize refused, and the pinned sealed A2A service's next message is
// still sealed.
func TestReview3SessionIDCollidesWithA2APath(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{PinRecipients: true})
	path, body := localSend("jsonrpc", "FIRST")
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	sreq, _, err := hr.h.Request(context.Background(), "/agents/echo", req)
	if err != nil {
		t.Fatal(err)
	}
	if first, _ := io.ReadAll(sreq.Body); strings.Contains(string(first), "FIRST") {
		t.Fatalf("control: %s", first)
	}
	ireq := httptest.NewRequest(http.MethodPost, "/mcp/plain", strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`))
	_, ij, err := hr.h.Request(context.Background(), "/mcp/plain", ireq)
	if err != nil {
		t.Fatal(err)
	}
	out, err := hr.h.Response(context.Background(), ij, &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": {"application/json"}, mcpwire.HeaderSessionID: {"/agents/echo"}},
		Body:   io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, out); !strings.Contains(got, `"error"`) {
		t.Fatalf("an initialize result naming a path as its session id: %s", got)
	}
	path, body = localSend("jsonrpc", "SECRET-A2A-MARKER")
	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	sreq, _, err = hr.h.Request(context.Background(), "/agents/echo", req)
	if err != nil {
		t.Fatalf("A2A request: %v", err)
	}
	if fwd, _ := io.ReadAll(sreq.Body); strings.Contains(string(fwd), "SECRET-A2A-MARKER") {
		t.Fatalf("the pinned sealed A2A service's message goes to the gate in plaintext: %s", fwd)
	}
}

// r3-hiring-10 (D242): an MCP session the client left without a DELETE, with no job (an unsealed service's), is
// forgotten once idle past its bound: kontra connect does not grow with every session.
func TestReview3UnsealedSessionEntryIsSwept(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	const sid = "gate-session-unsealed"
	_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	mcpAnswer(t, hr, initJob, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})
	try := func() error {
		req := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set(mcpwire.HeaderSessionID, sid)
		_, j, err := hr.h.Request(context.Background(), "/mcp/fs", req)
		j.Done()
		return err
	}
	if err := try(); err != nil {
		t.Fatalf("the session: %v", err)
	}
	hr.now = hr.now.Add(sessionJobIdle + time.Minute)
	hr.h.Sweep()
	var le *LocalError
	if err := try(); !errors.As(err, &le) || le.Status != http.StatusNotFound {
		t.Fatalf("a session left idle past its bound is still held: %v", err)
	}
}
