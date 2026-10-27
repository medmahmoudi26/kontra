package sealing

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
)

// mcpLocal runs one local MCP request through the hiring node: what goes to the gate, and the job for its answer.
func mcpLocal(t *testing.T, hr *hirer, sid, body string) ([]byte, *Job) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/fs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set(mcpwire.HeaderSessionID, sid)
	}
	req.Header.Set("Mcp-Param-City", "MARKER-header")
	sreq, job, err := hr.h.Request(context.Background(), "/mcp/fs", req)
	if err != nil {
		t.Fatalf("hiring Request(%s): %v", body, err)
	}
	if sid != "" && sreq.Header.Get("Mcp-Param-City") != "" {
		t.Error("an Mcp-Param-* header goes to the gate for a sealed session")
	}
	b, _ := io.ReadAll(sreq.Body)
	return b, job
}

// mcpAnswer has the hiring node open the gate's answer (JSON, or an event stream when sse).
func mcpAnswer(t *testing.T, hr *hirer, job *Job, body string, sse bool, hdr http.Header) string {
	t.Helper()
	h := http.Header{"Content-Type": {"application/json"}}
	if sse {
		h.Set("Content-Type", "text/event-stream")
	}
	for k, v := range hdr {
		h[k] = v
	}
	out, err := hr.h.Response(context.Background(), job, &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatalf("hiring Response: %v", err)
	}
	return readAll(t, out)
}

// sealed_no_plaintext (c), at package level (plan corrections WP-08): a sealed durable MCP call through a gate whose
// upstream asks one elicitation. The local client sends and reads plaintext, the server gets plaintext, and the
// gate's bytes (requests, the stored input request, the answer, the result) hold only envelopes; the input request's
// answer is sealed with the request's messageId and the job's replyTo and rk; an input request delivered twice is
// opened once (replayed, dropped).
func TestFakeGateMCPDurable(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	var gate bytes.Buffer
	const sid = "gate-session-xyz"
	session := sessionHash(sid)

	// initialize: the gate's result carries the sealed capability (ext-sealed.md §1).
	_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{}},"clientInfo":{"name":"c","version":"1"}}}`)
	initRes := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{},"tasks":{"requests":{"tools":{"call":{}}}},` +
		`"experimental":{"sh.kontra/sealed":` + string(extensionFor(true, f.node.Recipient())) + `}},"serverInfo":{"name":"s","version":"1"}}}`
	mcpAnswer(t, hr, initJob, initRes, false, http.Header{mcpwire.HeaderSessionID: {sid}})

	// The task-augmented call, sealed.
	fwd, callJob := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"trip","arguments":{"city":"MARKER-city"},"task":{"ttl":60000}}}`)
	gate.Write(fwd)
	if strings.Contains(string(fwd), "MARKER") || !strings.Contains(string(fwd), `"arguments":{}`) || !strings.Contains(string(fwd), mcpwire.MetaSealed) {
		t.Fatalf("the call to the gate: %s", fwd)
	}
	gateTask := uuid.NewString()
	mcpAnswer(t, hr, callJob, `{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"`+gateTask+`","status":"working","ttl":60000,"pollInterval":1000}}}`, false, nil)

	// The dispatch (§11.5): the stored call to the hosting node, the upstream's task, its elicitation on tasks/result.
	x := f.h.MCP(mcpPre(session, "1", gateTask))
	plain, r := fromClient(t, x, fwd)
	if r != "" || !strings.Contains(string(plain), "MARKER-city") {
		t.Fatalf("the server gets %q %s", r, plain)
	}
	x.Accepted()
	gate.WriteString(fromServer(t, x, `{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"up-t","status":"working","statusMessage":"MARKER-status"}}}`))
	x2 := f.h.MCP(mcpPre(session, "1", gateTask))
	fromClient(t, x2, []byte(`{"jsonrpc":"2.0","id":"kontra.dispatch.3","method":"tasks/result","params":{"taskId":"up-t"}}`))
	up := fromServer(t, x2, `{"jsonrpc":"2.0","id":5,"method":"elicitation/create","params":{"message":"MARKER-question","requestedSchema":{"type":"object"},`+
		`"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-t"}}}}`)
	gateID := "k." + gateTask + ".1"
	stored := strings.NewReplacer(`"id":5`, `"id":"`+gateID+`"`, `"taskId":"up-t"`, `"taskId":"`+gateTask+`"`).Replace(up)
	gate.WriteString(stored)

	// The local client's tasks/result: the gate's stream carries the input request (twice: a resumed stream).
	_, trJob := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":2,"method":"tasks/result","params":{"taskId":"`+gateTask+`"}}`)
	stream := "id: " + gateID + "\nevent: message\ndata: " + stored + "\n\n" + "id: " + gateID + "\nevent: message\ndata: " + stored + "\n\n"
	local := mcpAnswer(t, hr, trJob, stream, true, nil)
	if strings.Count(local, "MARKER-question") != 1 || strings.Contains(local, mcpwire.MetaSealed) || !strings.Contains(local, gateID) {
		t.Fatalf("the local client's stream: %s", local)
	}

	// The local client answers; the hiring node seals the answer as a request with the input request's messageId.
	ans, _ := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":"`+gateID+`","result":{"action":"accept","content":{"seat":"MARKER-seat"}}}`)
	gate.Write(ans)
	m, err := mcpwire.Parse(ans, mcpwire.FromClient)
	if err != nil || strings.Contains(string(ans), "MARKER") {
		t.Fatalf("the answer to the gate: %s %v", ans, err)
	}
	res, _ := strictjson.Members(m.Result)
	meta, _ := strictjson.Members(res["_meta"])
	ct, err := sealed.ParseMCPEnvelope(meta[mcpwire.MetaSealed])
	if err != nil {
		t.Fatal(err)
	}
	hd, _, err := sealed.Open([]age.Identity{f.node}, ct)
	if err != nil {
		t.Fatal(err)
	}
	callHd := openCall(t, f, fwd)
	if hd.Dir != sealed.DirRequest || hd.MessageID != "5" || hd.Session != session || hd.Tool != "trip" || hd.ReplyTo != callHd.ReplyTo || hd.RK != callHd.RK {
		t.Fatalf("the answer's header %+v (the call's replyTo %s)", hd, callHd.ReplyTo)
	}
	// The gate writes it upstream under the original id (§11.5 step 5).
	x3 := f.h.MCP(mcpPre(session, "1", gateTask))
	upAns, r := fromClient(t, x3, []byte(strings.Replace(string(ans), `"id":"`+gateID+`"`, `"id":5`, 1)))
	if r != "" || !strings.Contains(string(upAns), "MARKER-seat") {
		t.Fatalf("the server gets the answer %q %s", r, upAns)
	}
	x3.Accepted()

	// The result.
	x4 := f.h.MCP(mcpPre(session, "1", gateTask))
	fromClient(t, x4, []byte(`{"jsonrpc":"2.0","id":"kontra.dispatch.9","method":"tasks/result","params":{"taskId":"up-t"}}`))
	resUp := fromServer(t, x4, `{"jsonrpc":"2.0","id":"kontra.dispatch.9","result":{"content":[{"type":"text","text":"MARKER-result"}],"isError":false,`+
		`"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-t"}}}}`)
	stored = strings.NewReplacer(`"id":"kontra.dispatch.9"`, `"id":3`, `"taskId":"up-t"`, `"taskId":"`+gateTask+`"`).Replace(resUp)
	gate.WriteString(stored)
	_, rJob := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":3,"method":"tasks/result","params":{"taskId":"`+gateTask+`"}}`)
	final := mcpAnswer(t, hr, rJob, stored, false, nil)
	if !strings.Contains(final, "MARKER-result") || strings.Contains(final, sealed.ResourceURI) {
		t.Fatalf("the local client's result: %s", final)
	}
	if strings.Contains(gate.String(), "MARKER") {
		t.Fatalf("the gate held plaintext: %s", gate.String())
	}
}

// openCall opens a sealed tools/call's envelope with the hosting node's key.
func openCall(t *testing.T, f *fixture, body []byte) sealed.Header {
	t.Helper()
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := strictjson.Members(m.Params)
	meta, _ := strictjson.Members(params["_meta"])
	ct, err := sealed.ParseMCPEnvelope(meta[mcpwire.MetaSealed])
	if err != nil {
		t.Fatal(err)
	}
	hd, _, err := sealed.Open([]age.Identity{f.node}, ct)
	if err != nil {
		t.Fatal(err)
	}
	return hd
}

// A sealed plain call through the gate: the result opens for the local client; an unsealed result in its place is
// refused (rule 4a) with a JSON-RPC error carrying sh.kontra/failure sealed_rejected.
func TestFakeGateMCPPlain(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	const sid = "gate-session-plain"
	session := sessionHash(sid)
	_, initJob := mcpLocal(t, hr, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	mcpAnswer(t, hr, initJob, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"sh.kontra/sealed":`+
		string(extensionFor(false, f.node.Recipient()))+`}},"serverInfo":{"name":"s","version":"1"}}}`, false, http.Header{mcpwire.HeaderSessionID: {sid}})

	fwd, job := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"message":"MARKER-echo"}}}`)
	x := f.h.MCP(mcpPre(session, "7", ""))
	plain, r := fromClient(t, x, fwd)
	if r != "" || !strings.Contains(string(plain), "MARKER-echo") {
		t.Fatalf("%q %s", r, plain)
	}
	x.Accepted()
	res := fromServer(t, x, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"Echo: MARKER-echo"}],"isError":false}}`)
	if strings.Contains(res, "MARKER") {
		t.Fatal(res)
	}
	if local := mcpAnswer(t, hr, job, res, false, nil); !strings.Contains(local, "Echo: MARKER-echo") || strings.Contains(local, sealed.ResourceURI) {
		t.Fatalf("the local client gets %s", local)
	}
	// A gate answering the next call in plaintext.
	_, job2 := mcpLocal(t, hr, sid, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo","arguments":{"message":"x"}}}`)
	local := mcpAnswer(t, hr, job2, `{"jsonrpc":"2.0","id":8,"result":{"content":[{"type":"text","text":"forged"}],"isError":false}}`, false, nil)
	if strings.Contains(local, "forged") || !strings.Contains(local, `"sh.kontra/failure":"sealed_rejected"`) {
		t.Fatalf("an unsealed result: %s", local)
	}
}
