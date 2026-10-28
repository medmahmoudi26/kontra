package sealing

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

func sessionHash(sid string) string {
	s := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(s[:])
}

func mcpPre(session, reqID, task string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "fs", Kind: tunnel.KindMCP, Task: task, MCPSession: session, MCPRequestID: reqID}
}

// mcpHeader is an MCP request header of b (a tools/call, or an answer to an input request).
func (f *fixture) mcpHeader(b buyer, session, tool, msgID string) sealed.Header {
	return sealed.Header{V: 1, Dir: sealed.DirRequest, Service: "fs", IAT: f.now.Unix(), Session: session, Tool: tool,
		MessageID: msgID, Part: 0, Parts: 1, Kind: sealed.KindData, Kids: []string{f.kid()}, ReplyTo: b.replyTo(),
		RK: sealed.EncodeRK(b.rk)}
}

func envelope(ct []byte) string {
	b, _ := json.Marshal(sealed.NewMCPEnvelope(ct))
	return string(b)
}

// sealedCallBody is a sealed tools/call as a hiring node sends it: arguments {} and the envelope in _meta.
func (f *fixture) sealedCallBody(b buyer, session, tool, id, args string, task bool) []byte {
	ct := f.seal(f.mcpHeader(b, session, tool, id), args)
	params := `{"name":"` + tool + `","arguments":{},"_meta":{"sh.kontra/sealed":` + envelope(ct) + `}`
	if task {
		params += `,"task":{"ttl":60000}`
	}
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":` + params + `}}`)
}

func parse(t *testing.T, body []byte) *mcpwire.Message {
	t.Helper()
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil {
		t.Fatalf("Parse(%s): %v", body, err)
	}
	return m
}

// fromClient runs FromClient and returns the forwarded body or the refusal's reason.
func fromClient(t *testing.T, x *MCPExchange, body []byte) ([]byte, sealed.Reject) {
	t.Helper()
	out, err := x.FromClient(body, parse(t, body))
	if err != nil {
		r, ok := sealed.ReasonOf(err)
		if !ok {
			t.Fatalf("FromClient: %v", err)
		}
		return nil, r
	}
	return out, ""
}

func fromServer(t *testing.T, x *MCPExchange, body string) string {
	t.Helper()
	out, keep, err := x.FromServer([]byte(body))
	if err != nil || !keep {
		t.Fatalf("FromServer(%s): %v %v", body, keep, err)
	}
	return string(out)
}

// openMCP opens an envelope a hosting node sealed for b, checking auth.
func openMCP(t *testing.T, b buyer, ct []byte) (sealed.Header, string) {
	t.Helper()
	hd, plain, err := sealed.Open([]age.Identity{b.key}, ct)
	if err != nil {
		t.Fatalf("does not open with the buyer's key: %v", err)
	}
	if err := sealed.VerifyAuth(b.rk, hd, plain); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return hd, string(plain)
}

// A sealed plain tools/call opens (rule 4), its plaintext arguments reach the server, and its result is sealed into
// the kontra:sealed resource with isError outside; resent on the same session it is replayed (rule 5), on another
// session wrong_message (rule 4), with another tool wrong_message.
func TestHostingMCPPlainCall(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-1")
	body := f.sealedCallBody(b, s1, "echo", "1", `{"message":"secret marker"}`, false)
	x := f.h.MCP(mcpPre(s1, "1", ""))
	out, reason := fromClient(t, x, body)
	if reason != "" {
		t.Fatal(reason)
	}
	if !strings.Contains(string(out), `"arguments":{"message":"secret marker"}`) || strings.Contains(string(out), "sh.kontra/sealed") {
		t.Fatalf("forwarded %s", out)
	}
	x.Accepted()
	res := fromServer(t, x, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Echo: secret marker"}],"structuredContent":{"s":"secret"},"isError":false}}`)
	if strings.Contains(res, "secret") {
		t.Fatalf("plaintext in the result: %s", res)
	}
	m, err := mcpwire.Parse([]byte(res), mcpwire.FromUpstream)
	if err != nil {
		t.Fatal(err)
	}
	ct, ok, err := sealed.ParseMCPResult(m.Result)
	if !ok || err != nil {
		t.Fatalf("not a sealed result: %s %v", res, err)
	}
	hd, plain := openMCP(t, b, ct)
	if hd.Dir != sealed.DirReply || hd.Session != s1 || hd.Tool != "echo" || hd.MessageID != "1" || hd.TaskID != "" {
		t.Errorf("result header %+v", hd)
	}
	if plain != `{"content":[{"text":"Echo: secret marker","type":"text"}],"structuredContent":{"s":"secret"}}` {
		t.Errorf("result body %s", plain)
	}
	if !strings.Contains(res, `"isError":false`) {
		t.Errorf("isError is not outside the envelope: %s", res)
	}

	// Replays.
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "1", "")), body); r != sealed.RejectReplayed {
		t.Errorf("resent on the same session: %q, want replayed", r)
	}
	s2 := sessionHash("gate-session-2")
	if _, r := fromClient(t, f.h.MCP(mcpPre(s2, "1", "")), body); r != sealed.RejectWrongMessage {
		t.Errorf("resent on another session: %q, want wrong_message", r)
	}
	other := f.sealedCallBody(b, s1, "echo", "2", `{}`, false)
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "2", "")), []byte(strings.Replace(string(other), `"name":"echo"`, `"name":"add"`, 1))); r != sealed.RejectWrongMessage {
		t.Errorf("another tool: %q, want wrong_message", r)
	}
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "3", "")), other); r != sealed.RejectWrongMessage {
		t.Errorf("a messageId that is not the preamble's: %q, want wrong_message", r)
	}
	// Another buyer on the same session (a gate forging a call of its own into the job): wrong_job.
	b2 := newBuyer(t)
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "4", "")), f.sealedCallBody(b2, s1, "echo", "4", `{}`, false)); r != sealed.RejectWrongJob {
		t.Errorf("another replyTo on the job's session: %q, want wrong_job", r)
	}
}

// durable runs a sealed durable task's call on the dispatch's exchanges and has the upstream ask one elicitation,
// which it returns sealed with the buyer's view of its header.
type durable struct {
	f       *fixture
	b       buyer
	session string
	task    string // the gate task
	tool    string
}

func newDurable(t *testing.T) *durable {
	f := newFixture(t)
	d := &durable{f: f, b: newBuyer(t), session: sessionHash("client-session"), task: "0193c0de-0000-7000-8000-00000000d001", tool: "trip"}
	x := f.h.MCP(mcpPre(d.session, "1", d.task))
	if _, r := fromClient(t, x, f.sealedCallBody(d.b, d.session, d.tool, "1", `{"city":"secret city"}`, true)); r != "" {
		t.Fatal(r)
	}
	x.Accepted()
	res := fromServer(t, x, `{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"up-t","status":"working","statusMessage":"looking for secret flights"}}}`)
	if strings.Contains(res, "secret") || !strings.Contains(res, `"statusMessage":"sealed"`) {
		t.Fatalf("a CreateTaskResult's statusMessage: %s", res)
	}
	return d
}

// ask has the upstream send an elicitation with JSON-RPC id upID on a tasks/result exchange; it returns the sealed
// request as relayed.
func (d *durable) ask(t *testing.T, upID string) (sealed.Header, string, string) {
	x := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	if _, r := fromClient(t, x, []byte(`{"jsonrpc":"2.0","id":"kontra.dispatch.7","method":"tasks/result","params":{"taskId":"up-t"}}`)); r != "" {
		t.Fatal(r)
	}
	req := fromServer(t, x, `{"jsonrpc":"2.0","id":`+upID+`,"method":"elicitation/create","params":{"message":"secret question","requestedSchema":{"type":"object"},"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-t"}}}}`)
	if strings.Contains(req, "secret") {
		t.Fatalf("plaintext in an input request: %s", req)
	}
	m, err := mcpwire.Parse([]byte(req), mcpwire.FromUpstream)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := strictjson.Members(m.Params)
	meta, _ := strictjson.Members(params["_meta"])
	if len(params) != 1 || meta[mcpwire.MetaRelatedTask] == nil {
		t.Fatalf("an input request's params: %s", m.Params)
	}
	ct, err := sealed.ParseMCPEnvelope(meta[mcpwire.MetaSealed])
	if err != nil {
		t.Fatal(err)
	}
	hd, plain := openMCP(t, d.b, ct)
	return hd, plain, req
}

// answer is the client's sealed answer to the input request upID, as the gate forwards it on an exchange of the task.
func (d *durable) answer(t *testing.T, upID string, hd sealed.Header, result string) []byte {
	ct := d.f.seal(hd, result)
	return []byte(`{"jsonrpc":"2.0","id":` + upID + `,"result":{"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-t"},"sh.kontra/sealed":` + envelope(ct) + `}}}`)
}

// hdr is the header of the client's answer to the input request msgID of the task: the input request's session, tool
// and messageId, and its gate task (sealing's mcpRule1 SPEC-CONFLICT).
func (d *durable) hdr(msgID string) sealed.Header {
	hd := d.f.mcpHeader(d.b, d.session, d.tool, msgID)
	hd.TaskID = d.task
	return hd
}

// A sealed durable task (D175): its elicitation is sealed with the upstream's id as messageId and the creating call's
// session and tool; the sealed answer passes rule 4b once and reaches the server in plaintext; the result is sealed
// with the gate task id.
func TestHostingMCPDurableElicitation(t *testing.T) {
	d := newDurable(t)
	hd, plain, _ := d.ask(t, "5")
	if hd.Dir != sealed.DirReply || hd.MessageID != "5" || hd.Session != d.session || hd.Tool != d.tool || hd.TaskID != d.task {
		t.Fatalf("input request header %+v", hd)
	}
	if plain != `{"message":"secret question","requestedSchema":{"type":"object"}}` {
		t.Fatalf("input request body %s", plain)
	}
	ans := d.answer(t, "5", d.hdr("5"), `{"action":"accept","content":{"seat":"secret seat"}}`)
	x := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	out, r := fromClient(t, x, ans)
	if r != "" {
		t.Fatal(r)
	}
	if !strings.Contains(string(out), `"seat":"secret seat"`) || strings.Contains(string(out), "sh.kontra/sealed") {
		t.Fatalf("the server gets %s", out)
	}
	x.Accepted()
	if _, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), ans); r != sealed.RejectReplayed {
		t.Fatalf("the same answer twice: %q, want replayed", r)
	}
	// The task's result.
	rx := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	fromClient(t, rx, []byte(`{"jsonrpc":"2.0","id":"kontra.dispatch.9","method":"tasks/result","params":{"taskId":"up-t"}}`))
	res := fromServer(t, rx, `{"jsonrpc":"2.0","id":"kontra.dispatch.9","result":{"content":[{"type":"text","text":"booked secret"}],"isError":false,"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-t"}}}}`)
	m, _ := mcpwire.Parse([]byte(res), mcpwire.FromUpstream)
	ct, ok, err := sealed.ParseMCPResult(m.Result)
	if !ok || err != nil || strings.Contains(res, "secret") {
		t.Fatalf("the task's result: %s %v", res, err)
	}
	rh, _ := openMCP(t, d.b, ct)
	if rh.TaskID != d.task || rh.MessageID != "1" || rh.Tool != d.tool {
		t.Errorf("result header %+v", rh)
	}
	// tasks/get and notifications/tasks/status carry "sealed" for the task's statusMessage.
	gx := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	fromClient(t, gx, []byte(`{"jsonrpc":"2.0","id":"kontra.dispatch.10","method":"tasks/get","params":{"taskId":"up-t"}}`))
	if got := fromServer(t, gx, `{"jsonrpc":"2.0","id":"kontra.dispatch.10","result":{"taskId":"up-t","status":"input_required","statusMessage":"secret need"}}`); strings.Contains(got, "secret") {
		t.Errorf("tasks/get: %s", got)
	}
	if got := fromServer(t, gx, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"up-t","status":"working","statusMessage":"secret progress"}}`); strings.Contains(got, "secret") {
		t.Errorf("notifications/tasks/status: %s", got)
	}
}

// Rule 4b's refusals, and "answers must be sealed" (plan corrections WP-08; ext-sealed.md §5, D175).
func TestHostingMCPAnswerRules(t *testing.T) {
	t.Run("a messageId never sealed is wrong_message", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		_, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "6", d.hdr("6"), `{"action":"accept"}`))
		if r != sealed.RejectWrongMessage {
			t.Fatalf("%q", r)
		}
	})
	t.Run("one sealed more than max_age ago is wrong_message", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		d.f.now = d.f.now.Add(DefaultMaxAge + time.Minute)
		_, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "5", d.hdr("5"), `{"action":"accept"}`))
		if r != sealed.RejectWrongMessage {
			t.Fatalf("%q", r)
		}
	})
	t.Run("another session in the header is wrong_message", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		hd := d.hdr("5")
		hd.Session = sessionHash("elsewhere")
		_, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "5", hd, `{"action":"accept"}`))
		if r != sealed.RejectWrongMessage {
			t.Fatalf("%q", r)
		}
	})
	t.Run("another tool in the header is wrong_message", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		hd := d.hdr("5")
		hd.Tool = "other"
		_, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "5", hd, `{"action":"accept"}`))
		if r != sealed.RejectWrongMessage {
			t.Fatalf("%q", r)
		}
	})
	t.Run("another buyer's answer is wrong_job", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		other := newBuyer(t)
		hd := d.f.mcpHeader(other, d.session, d.tool, "5")
		hd.TaskID = d.task
		_, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "5", hd, `{}`))
		if r != sealed.RejectWrongJob {
			t.Fatalf("%q", r)
		}
	})
	t.Run("an answer whose messageId is an earlier tools/call id is accepted", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "1") // the upstream's id 1, the same as the client's tools/call id 1 of the session
		x := d.f.h.MCP(mcpPre(d.session, "1", d.task))
		if _, r := fromClient(t, x, d.answer(t, "1", d.hdr("1"), `{"action":"accept"}`)); r != "" {
			t.Fatalf("refused %q: rule 5 applies to requests only", r)
		}
	})
	t.Run("the job state survives a node restart between the request and its answer", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		d.f.open() // a new process on the same ~/.kontra/hosted
		x := d.f.h.MCP(mcpPre(d.session, "1", d.task))
		if _, r := fromClient(t, x, d.answer(t, "5", d.hdr("5"), `{"action":"accept"}`)); r != "" {
			t.Fatalf("refused %q after a restart", r)
		}
		x.Accepted()
		d.f.open()
		if _, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), d.answer(t, "5", d.hdr("5"), `{"action":"accept"}`)); r != sealed.RejectReplayed {
			t.Fatalf("answered before a restart: %q, want replayed", r)
		}
	})
	t.Run("an unsealed answer is bad_header and the server sees nothing", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		out, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), []byte(`{"jsonrpc":"2.0","id":5,"result":{"action":"accept","content":{"seat":"gate's choice"}}}`))
		if r != sealed.RejectBadHeader || out != nil {
			t.Fatalf("%q %s", r, out)
		}
	})
	t.Run("an envelope with another member is bad_header", func(t *testing.T) {
		d := newDurable(t)
		d.ask(t, "5")
		ct := d.f.seal(d.hdr("5"), `{"action":"accept"}`)
		body := []byte(`{"jsonrpc":"2.0","id":5,"result":{"_meta":{"sh.kontra/sealed":{"v":1,"envelope":"` +
			base64.StdEncoding.EncodeToString(ct) + `","x":1}}}}`)
		if _, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), body); r != sealed.RejectBadHeader {
			t.Fatalf("%q", r)
		}
		// The same answer sealed exactly passes.
		x := d.f.h.MCP(mcpPre(d.session, "1", d.task))
		if _, r := fromClient(t, x, d.answer(t, "5", d.hdr("5"), `{"action":"accept"}`)); r != "" {
			t.Fatalf("the sealed answer: %q", r)
		}
	})
	t.Run("a sealed tools/call resent on another session is wrong_message", func(t *testing.T) {
		d := newDurable(t)
		body := d.f.sealedCallBody(d.b, d.session, d.tool, "1", `{"city":"x"}`, true)
		if _, r := fromClient(t, d.f.h.MCP(mcpPre(sessionHash("another"), "1", d.task)), body); r != sealed.RejectWrongMessage {
			t.Fatalf("%q", r)
		}
	})
}

// --sealed-required refuses an unsealed tools/call (bad_header); a case variant of the envelope's key is bad_header.
func TestHostingMCPRequired(t *testing.T) {
	f := newFixture(t, "fs")
	s := sessionHash("s")
	if _, r := fromClient(t, f.h.MCP(mcpPre(s, "1", "")), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)); r != sealed.RejectBadHeader {
		t.Fatalf("%q", r)
	}
	g := newFixture(t)
	b := newBuyer(t)
	body := strings.Replace(string(g.sealedCallBody(b, s, "echo", "1", `{}`, false)), `"sh.kontra/sealed"`, `"SH.kontra/sealed"`, 1)
	if _, r := fromClient(t, g.h.MCP(mcpPre(s, "1", "")), []byte(body)); r != sealed.RejectBadHeader {
		t.Fatalf("a case variant of the envelope's key: %q", r)
	}
	// Plaintext arguments beside the envelope: bad_header.
	body = strings.Replace(string(g.sealedCallBody(b, s, "echo", "1", `{}`, false)), `"arguments":{}`, `"arguments":{"x":"plain"}`, 1)
	if _, r := fromClient(t, g.h.MCP(mcpPre(s, "1", "")), []byte(body)); r != sealed.RejectBadHeader {
		t.Fatalf("arguments beside the envelope: %q", r)
	}
}

// Finding: a sealed task's statusMessage reached other sessions in plaintext: FromServer passed every message of an
// exchange whose session has no job, and sealed tasks/list entries of the exchange's own job only, so an upstream that
// does not scope tasks/list per session listed another session's sealed task with its statusMessage, and its
// notifications/tasks/status came on another session's stream. Every sealed task's statusMessage is "sealed" now
// wherever the node reads one, and another session's status notification is dropped.
func TestHostingMCPStatusOnOtherSessions(t *testing.T) {
	d := newDurable(t) // binds upstream task up-t to the job of session client-session
	list := `{"jsonrpc":"2.0","id":9,"result":{"tasks":[{"taskId":"up-t","status":"working","statusMessage":"looking for SECRET flights"},` +
		`{"taskId":"plain","status":"working","statusMessage":"an unsealed task's message"}]}}`
	for _, session := range []string{sessionHash("another-session"), d.session, ""} {
		x := d.f.h.MCP(mcpPre(session, "9", ""))
		if _, r := fromClient(t, x, []byte(`{"jsonrpc":"2.0","id":9,"method":"tasks/list","params":{}}`)); r != "" {
			t.Fatalf("tasks/list on session %q: %s", session, r)
		}
		got := fromServer(t, x, list)
		if strings.Contains(got, "SECRET") || !strings.Contains(got, `"statusMessage":"sealed"`) || !strings.Contains(got, "an unsealed task's message") {
			t.Errorf("tasks/list on session %q: %s", session, got)
		}
	}
	// A status notification of the sealed task on another session's stream is dropped; on its own session it is sealed.
	note := `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"up-t","status":"working","statusMessage":"SECRET progress"}}`
	x := d.f.h.MCP(mcpPre(sessionHash("another-session"), "", ""))
	if out, keep, err := x.FromServer([]byte(note)); err != nil || keep {
		t.Errorf("another session's status notification: %v %v %s", err, keep, out)
	}
	x = d.f.h.MCP(mcpPre(d.session, "", ""))
	if out, keep, err := x.FromServer([]byte(note)); err != nil || !keep || strings.Contains(string(out), "SECRET") || !strings.Contains(string(out), `"statusMessage":"sealed"`) {
		t.Errorf("the session's own status notification: %v %v %s", err, keep, out)
	}
	// A tasks/get answer naming the sealed task on an exchange with no job.
	x = d.f.h.MCP(mcpPre("", "4", ""))
	if _, r := fromClient(t, x, []byte(`{"jsonrpc":"2.0","id":4,"method":"tasks/get","params":{"taskId":"other"}}`)); r != "" {
		t.Fatal(r)
	}
	if got := fromServer(t, x, `{"jsonrpc":"2.0","id":4,"result":{"taskId":"up-t","status":"working","statusMessage":"SECRET"}}`); strings.Contains(got, "SECRET") {
		t.Errorf("tasks/get naming a sealed task on an exchange with no job: %s", got)
	}
}
