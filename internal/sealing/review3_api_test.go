package sealing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// M3 review round 3: regression tests that use what the round-3 fixes added (SetModern, HostingConfig.Shared).

// r3-hosting-2 (D234): a 2026-07-28 request to a sealed MCP service is refused by the node, whatever the gate says;
// and a reply to a sealed call in a 2026-07-28 shape (a multi round-trip's inputRequests and requestState, a
// CreateTaskResult at the top of result) is never relayed.
func TestReview3ModernRefusedOnSealedService(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-modern")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-1")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "ask", "1", `{"secret":"SECRET"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	for name, res := range map[string]string{
		"mrtr": `{"resultType":"input_required","inputRequests":{"q1":{"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"SECRET"}}]}}},"requestState":"state-SECRET"}`,
		"task": `{"resultType":"task","taskId":"up-mtask-1","status":"working"}`,
	} {
		if out, keep, err := x.FromServer([]byte(`{"jsonrpc":"2.0","id":1,"result":` + res + `}`)); err == nil || keep {
			t.Fatalf("%s: a sealed call's 2026-07-28 reply is relayed: %s", name, out)
		}
	}
	// A 2026-07-28 request on the service, sealed or not, with sealed state on it.
	for _, body := range []string{
		string(f.sealedCallBody(b, s1, "ask", "7", `{"secret":"SECRET"}`, false)),
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"ask","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
	} {
		y := f.h.MCP(mcpPre("", "8", ""))
		y.SetModern()
		if _, r := fromClient(t, y, []byte(body)); r != sealed.RejectBadHeader {
			t.Fatalf("a 2026-07-28 request to a sealed service: %q", r)
		}
	}
}

// r3-hosting-5 (D236): two A2A services of one node on one upstream share their sealed state: a GetTask on echo2
// naming echo's sealed job's upstream task is wrong_task, as on echo.
func TestReview3TwoServicesOneUpstreamA2A(t *testing.T) {
	f := newFixture(t)
	f.h.cfg.Shared = map[string]string{"echo": "echo", "echo2": "echo"}
	r3Buyer(t, f)
	get := []byte(`{"jsonrpc":"2.0","id":7,"method":"GetTask","params":{"id":"up-B","historyLength":10}}`)
	for _, svc := range []string{"echo", "echo2"} {
		p := pre("0193c0de-0000-7000-8000-0000000000e1", "ctx-gate")
		p.Service = svc
		if _, _, r := f.request(p, get); r != sealed.RejectWrongTask {
			t.Fatalf("GetTask of the buyer's upstream task on %s: %q", svc, r)
		}
	}
	// A reply naming it on an exchange of echo2 is refused too.
	p := pre("0193c0de-0000-7000-8000-0000000000e2", "ctx-gate2")
	p.Service = "echo2"
	_, x, r := f.request(p, []byte(`{"jsonrpc":"2.0","id":7,"method":"GetTask","params":{"id":"up-other"}}`))
	if r != "" {
		t.Fatal(r)
	}
	out := readAll(t, f.answer(x, http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{"id":"up-B","contextId":"upctx-B","status":{"state":"TASK_STATE_COMPLETED"},`+
		`"artifacts":[{"artifactId":"a1","parts":[{"text":"BUYER-SECRET-RESULT"}]}]}}`))
	if strings.Contains(out, "BUYER-SECRET") {
		t.Fatalf("echo's sealed task read through echo2: %s", out)
	}
}

// sealReplyFor seals a reply body as the hosting node does for the sealed request whose header is callHdr (its
// replyTo and rk), with the header's op, messageId and taskId.
func sealReplyFor(t *testing.T, callHdr sealed.Header, op sealed.Op, msgID, taskID, body string) []byte {
	t.Helper()
	rcpt, err := age.ParseX25519Recipient(callHdr.ReplyTo)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := sealed.DecodeRK(callHdr.RK)
	hd := sealed.Header{V: 1, Dir: sealed.DirReply, Service: callHdr.Service, TaskID: taskID, Session: callHdr.Session,
		Tool: callHdr.Tool, MessageID: msgID, Op: op, Part: 0, Parts: 1, Kind: sealed.KindData, Kids: []string{tunnel.KeyID(rcpt.String())}}
	if hd.Auth, err = sealed.Auth(rk, hd, []byte(body)); err != nil {
		t.Fatal(err)
	}
	ct, err := sealed.Seal([]age.Recipient{rcpt}, hd, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

// r3-hiring-6 (D241): a sealed call that outlasts the gate's session_idle_s across a sweep keeps its session's key:
// the honest gate's sealed result opens.
func TestReview3SweepKeepsAnOpenCallsKey(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	const sid = "gate-session-long"
	_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	mcpAnswer(t, hr, initJob, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"sh.kontra/sealed":`+
		string(extensionFor(true, f.node.Recipient()))+`}},"serverInfo":{"name":"s","version":"1"},"_meta":{"sh.kontra/session_idle_s":1}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})
	sent, cj := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"m":"SECRET"}}}`)
	hr.now = hr.now.Add(time.Hour)
	hr.h.Sweep()
	res, _ := sealed.MCPResult(sealReplyFor(t, openCall(t, f, sent), sealed.OpResult, "9", "", `{"content":[{"type":"text","text":"SEALED-RESULT"}]}`), false)
	if got := mcpAnswer(t, hr, cj, `{"jsonrpc":"2.0","id":9,"result":`+string(res)+`}`, false, nil); !strings.Contains(got, "SEALED-RESULT") {
		t.Fatalf("the sealed call's own result after the sweep: %s", got)
	}
}

// r3-hiring-5 (D239): the envelope's op tells the four MCP roles apart. The buyer's sealed answer to an input request
// (server id 7), sent by the gate as a tools/call with id 7, is wrong_message at the hosting node; an input request's
// envelope whose server id equals a later call's id, delivered as that call's result, is refused at the hiring node.
func TestReview3RolesAreNotSpliced(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-p7b")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-1")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "transfer", "1", `{"to":"alice"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	_ = fromServer(t, x, `{"jsonrpc":"2.0","id":7,"method":"elicitation/create","params":{"message":"How much?","requestedSchema":{"type":"object"}}}`)
	answerCT := f.seal(f.answerHeader(b, s1, "transfer", "7", ""), `{"action":"accept","content":{"amount":"9999"}}`)
	call := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"transfer","arguments":{},"_meta":{"sh.kontra/sealed":` + envelope(answerCT) + `}}}`)
	if out, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "7", ""), "up-1"), call); r != sealed.RejectWrongMessage {
		t.Fatalf("the buyer's sealed answer as a tools/call: %q %s", r, out)
	}

	fh, hr, sid := sealedMCPSession(t)
	sent, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"m":"x"}}}`)
	q := sealReplyFor(t, openCall(t, fh, sent), sealed.OpInput, "2", "", `{"message":"Q","requestedSchema":{"type":"object"}}`)
	res, _ := sealed.MCPResult(q, false)
	if got := mcpAnswer(t, hr, job, `{"jsonrpc":"2.0","id":2,"result":`+string(res)+`}`, false, nil); !strings.Contains(got, "sealed part rejected") {
		t.Fatalf("an input request's envelope as the call's result: %s", got)
	}
}

// r3-hiring-4 (D243): two input requests of the session delivered with one gate request id: the second is dropped,
// and the client's answer to the first is sealed as the first's; a second answer to the id is refused here.
func TestReview3InputRequestIDReuse(t *testing.T) {
	f, hr, sid := sealedMCPSession(t)
	sent, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"files","arguments":{"op":"x"}}}`)
	callHdr := openCall(t, f, sent)
	q1 := sealReplyFor(t, callHdr, sealed.OpInput, "0", "", `{"message":"Q1: may I READ notes.txt?","requestedSchema":{"type":"object"}}`)
	q2 := sealReplyFor(t, callHdr, sealed.OpInput, "1", "", `{"message":"Q2: may I DELETE everything?","requestedSchema":{"type":"object"}}`)
	ev := func(ct []byte) string {
		return "data: " + `{"jsonrpc":"2.0","id":"k.g.7","method":"elicitation/create","params":{"_meta":{"sh.kontra/sealed":` + envelope(ct) + `}}}` + "\n\n"
	}
	got := mcpAnswer(t, hr, job, ev(q1)+ev(q2), true, nil)
	if !strings.Contains(got, "Q1: may I READ") || strings.Contains(got, "Q2: may I DELETE") {
		t.Fatalf("one gate id, two questions: %s", got)
	}
	out, _ := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":"k.g.7","result":{"action":"accept","content":{}}}`)
	m, _ := mcpwire.Parse(out, mcpwire.FromClient)
	meta, _ := mcpwire.Member(m.Result, "_meta")
	env, _ := mcpwire.Member(meta, mcpwire.MetaSealed)
	act, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		t.Fatalf("answer not sealed: %s", out)
	}
	ah, _, err := sealed.Open([]age.Identity{f.node}, act)
	if err != nil || ah.MessageID != "0" || ah.Op != sealed.OpAnswer {
		t.Fatalf("the answer to Q1: %+v %v", ah, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":"k.g.7","result":{"action":"accept","content":{}}}`))
	req.Header.Set(mcpwire.HeaderSessionID, sid)
	var le *LocalError
	if _, _, err := hr.h.Request(context.Background(), "/mcp/fs", req); !errors.As(err, &le) {
		t.Fatalf("a second answer to the gate id: %v", err)
	}
}

// r3-hiring-8 (D243): an input request with no taskId is opened once per session, whichever call's stream it comes
// on; and a durable task's input request that names no task is not the task's.
func TestReview3InputRequestReplayedAcrossCalls(t *testing.T) {
	f, hr, sid := sealedMCPSession(t)
	sentA, jobA := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pay","arguments":{"to":"alice","amount":5}}}`)
	q := sealReplyFor(t, openCall(t, f, sentA), sealed.OpInput, "0", "", `{"message":"Confirm paying alice 5?","requestedSchema":{"type":"object"}}`)
	ev := "data: " + `{"jsonrpc":"2.0","id":"k.a.0","method":"elicitation/create","params":{"_meta":{"sh.kontra/sealed":` + envelope(q) + `}}}` + "\n\n"
	if got := mcpAnswer(t, hr, jobA, ev, true, nil); !strings.Contains(got, "Confirm paying alice") {
		t.Fatalf("first delivery: %s", got)
	}
	_, jobB := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"pay","arguments":{"to":"bob","amount":5000}}}`)
	if got := mcpAnswer(t, hr, jobB, strings.Replace(ev, "k.a.0", "k.b.0", 1), true, nil); strings.Contains(got, "Confirm paying alice") {
		t.Fatalf("an input request opened on call 3's stream is opened again on call 4's: %s", got)
	}
}

// r3-hiring-9 (D246): a client's error answer to a sealed input request (a rejected sampling) reaches the service with
// its code and fixed text: the hiring node sends it so, the hosting node takes it once for that request.
func TestReview3ErrorAnswerToSealedInputRequest(t *testing.T) {
	f, hr, sid := sealedMCPSession(t)
	sent, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sampleLLM","arguments":{"prompt":"x"}}}`)
	q := sealReplyFor(t, openCall(t, f, sent), sealed.OpInput, "0", "", `{"messages":[],"maxTokens":10}`)
	_ = mcpAnswer(t, hr, job, "data: "+`{"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"_meta":{"sh.kontra/sealed":`+envelope(q)+`}}}`+"\n\n", true, nil)
	out, _ := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":0,"error":{"code":-1,"message":"User rejected sampling request: SECRET-REASON"}}`)
	if strings.Contains(string(out), "SECRET-REASON") || !strings.Contains(string(out), `"code":-1`) {
		t.Fatalf("the client's error answer as the gate gets it: %s", out)
	}

	// The hosting node: a plain call's sampling request, then the client's error answer, once.
	b := newBuyer(t)
	s1 := sessionHash("gate-session-decline")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-1")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "sampleLLM", "1", `{"prompt":"SECRET"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	_ = fromServer(t, x, `{"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"messages":[],"maxTokens":10}}`)
	decline := []byte(`{"jsonrpc":"2.0","id":0,"error":{"code":-1,"message":"GATE TEXT"}}`)
	y := f.h.MCPOn(mcpPre(s1, "", ""), "up-1")
	fwd, r := fromClient(t, y, decline)
	if r != "" || strings.Contains(string(fwd), "GATE TEXT") || !strings.Contains(string(fwd), `"code":-1`) {
		t.Fatalf("an error answer to a sealed input request: %q %s", r, fwd)
	}
	y.Accepted()
	if _, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), "up-1"), decline); r != sealed.RejectReplayed {
		t.Fatalf("a second error answer: %q", r)
	}
}

// r3-hosting-1 (D232), the hiring half: a sealed plain call's input request on the session's GET stream (the stdio
// bridge's route) opens for the local client while the call is unanswered, and its answer is sealed for it.
func TestReview3HiringOpensPlainCallInputOnGET(t *testing.T) {
	f, hr, sid := sealedMCPSession(t)
	get := httptest.NewRequest(http.MethodGet, "/mcp/fs", nil)
	get.Header.Set(mcpwire.HeaderSessionID, sid)
	_, gj, err := hr.h.Request(context.Background(), "/mcp/fs", get)
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"text":"SECRET"}}}`)
	q := sealReplyFor(t, openCall(t, f, sent), sealed.OpInput, "sample-2", "", `{"messages":[{"role":"user","content":{"type":"text","text":"prompt: SECRET"}}],"maxTokens":10}`)
	ev := "data: " + `{"jsonrpc":"2.0","id":"sample-2","method":"sampling/createMessage","params":{"_meta":{"sh.kontra/sealed":` + envelope(q) + `}}}` + "\n\n"
	if _, got := mcpGateAnswer(t, hr, gj, 200, "text/event-stream", strings.NewReader(ev)); !strings.Contains(got, "prompt: SECRET") {
		t.Fatalf("a plain call's input request on the GET: %s", got)
	}
	out, _ := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":"sample-2","result":{"role":"assistant","content":{"type":"text","text":"ANSWER"},"model":"m"}}`)
	if strings.Contains(string(out), "ANSWER") || !strings.Contains(string(out), mcpwire.MetaSealed) {
		t.Fatalf("the answer: %s", out)
	}
}
