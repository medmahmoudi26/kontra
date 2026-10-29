package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// hostingFor is the hosting node's sealing over a fresh job state, with one identity key.
func hostingFor(t *testing.T) (*sealing.Hosting, *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h, err := sealing.NewHosting(sealing.HostingConfig{
		Dir:        filepath.Join(t.TempDir(), "hosted", "gate.test"),
		Identities: func() []age.Identity { return []age.Identity{id} },
		KeyIDs:     func() []string { return []string{tunnel.KeyID(id.Recipient().String())} },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, id
}

// sealedCall is a sealed tools/call as a hiring node sends it to service, for session (its hash) and the JSON-RPC id
// id.
func sealedCall(t *testing.T, node *age.X25519Identity, service, session, tool, id, args string) (string, *age.X25519Identity, []byte) {
	t.Helper()
	job, _ := age.GenerateX25519Identity()
	rk, _ := sealed.NewRK()
	hd := sealed.Header{V: 1, Dir: sealed.DirRequest, Service: service, IAT: time.Now().Unix(), Session: session, Tool: tool, MessageID: id, Op: sealed.OpCall,
		Part: 0, Parts: 1, Kind: sealed.KindData, Kids: []string{tunnel.KeyID(node.Recipient().String())},
		ReplyTo: job.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal([]age.Recipient{node.Recipient()}, hd, []byte(args))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := json.Marshal(sealed.NewMCPEnvelope(ct))
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":{},"_meta":{"sh.kontra/sealed":` +
		string(env) + `}}}`, job, rk
}

func mcpSealedPreamble(service, session, id string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindMCP, MCPSession: session, MCPRequestID: id}
}

// A hosted MCP URL service with sealing (ext-sealed.md §5, plan corrections WP-08): a sealed tools/call reaches the
// server in plaintext after the roots filter, its result goes back sealed to the job's replyTo; the same call again is
// the node's own refusal (400, Kontra-Node: sealed_rejected, replayed); a sealed call whose params carry "name" and
// "NAME" is refused bad_header and the server sees nothing (D172).
func TestMCPURLSealing(t *testing.T) {
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		var p struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(r.Params, &p)
		return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"Echo: ` + p.Arguments.Text + `"}],"isError":false}`)}
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting

	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	sum := sha256.Sum256([]byte("the gate's session id"))
	session := hex.EncodeToString(sum[:])

	call, job, rk := sealedCall(t, node, "web", session, "echo", "1", `{"text":"MARKER-arg"}`)
	resp, body, _ := pp.raw(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "MARKER") {
		t.Fatalf("a sealed call: %d %s", resp.StatusCode, body)
	}
	if len(requestsWith(up, "MARKER-arg")) != 1 {
		t.Fatal("the server did not get the plaintext arguments")
	}
	data := body
	if evs := sseEvents(body); len(evs) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = eventData(evs[len(evs)-1])
	}
	m, err := mcpwire.Parse([]byte(data), mcpwire.FromUpstream)
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	ct, ok, err := sealed.ParseMCPResult(m.Result)
	if !ok || err != nil {
		t.Fatalf("the result is not sealed: %s", data)
	}
	hd, plain, err := sealed.Open([]age.Identity{job}, ct)
	if err != nil || sealed.VerifyAuth(rk, hd, plain) != nil || !strings.Contains(string(plain), "Echo: MARKER-arg") || hd.MessageID != "1" {
		t.Fatalf("the sealed result: %v %+v %s", err, hd, plain)
	}

	// The same call again: replayed, answered by the node.
	resp, _, _ = pp.raw(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderNode) != "sealed_rejected" || resp.Header.Get(tunnel.HeaderSealedReject) != "replayed" {
		t.Fatalf("a replayed call: %d %v", resp.StatusCode, resp.Header)
	}

	// "name" and "NAME": bad_header, the server sees nothing.
	before := len(up.Requests())
	twin, _, _ := sealedCall(t, node, "web", session, "echo", "2", `{"text":"x"}`)
	twin = strings.Replace(twin, `"name":"echo"`, `"name":"echo","NAME":"add"`, 1)
	resp, _, _ = pp.raw(t, mcpSealedPreamble("web", session, "2"), mcpPost(twin, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" {
		t.Fatalf("name and NAME: %d %v", resp.StatusCode, resp.Header)
	}
	if len(up.Requests()) != before {
		t.Fatal("the server saw a refused call")
	}
}

// A hosted stdio MCP service with sealing (ext-sealed.md §5; tunnel.md §10): the bridge's filter opens a sealed
// tools/call before the child reads it, so the child gets the plaintext arguments, and the child's answer goes back
// sealed to the job's replyTo; the same call again is the node's own refusal (replayed); a sealed call whose params
// carry "name" and "NAME" is refused bad_header and the child reads nothing of it (D172).
func TestMCPStdioSealing(t *testing.T) {
	bin := stdioServer(t)
	rec := t.TempDir()
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "fs", Command: []string{bin, "-record", rec}})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	received := func() string {
		files, _ := filepath.Glob(filepath.Join(rec, "received-*.jsonl"))
		var all []byte
		for _, f := range files {
			b, _ := os.ReadFile(f)
			all = append(all, b...)
		}
		return string(all)
	}

	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("fs"), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	if resp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	sum := sha256.Sum256([]byte("the gate's stdio session"))
	session := hex.EncodeToString(sum[:])

	call, job, rk := sealedCall(t, node, "fs", session, "echo", "1", `{"text":"MARKER-stdio"}`)
	resp, body, _ := pp.raw(t, mcpSealedPreamble("fs", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "MARKER") {
		t.Fatalf("a sealed call: %d %s", resp.StatusCode, body)
	}
	if got := received(); !strings.Contains(got, `"arguments":{"text":"MARKER-stdio"}`) || strings.Contains(got, sealed.ResourceURI) ||
		strings.Contains(got, mcpwire.MetaSealed) {
		t.Fatalf("the child read %s", got)
	}
	data := body
	if evs := sseEvents(body); len(evs) > 0 {
		data = eventData(evs[len(evs)-1])
	}
	m, err := mcpwire.Parse([]byte(data), mcpwire.FromUpstream)
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	ct, ok, err := sealed.ParseMCPResult(m.Result)
	if !ok || err != nil {
		t.Fatalf("the result is not sealed: %s", data)
	}
	hd, plain, err := sealed.Open([]age.Identity{job}, ct)
	if err != nil || sealed.VerifyAuth(rk, hd, plain) != nil || !strings.Contains(string(plain), "MARKER-stdio") ||
		hd.Dir != sealed.DirReply || hd.Session != session || hd.Tool != "echo" || hd.MessageID != "1" {
		t.Fatalf("the sealed result: %v %+v %s", err, hd, plain)
	}

	// The same call again: replayed, answered by the node.
	resp, _, _ = pp.raw(t, mcpSealedPreamble("fs", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderNode) != "sealed_rejected" ||
		resp.Header.Get(tunnel.HeaderSealedReject) != "replayed" {
		t.Fatalf("a replayed call: %d %v", resp.StatusCode, resp.Header)
	}
	if n := strings.Count(received(), "MARKER-stdio"); n != 1 {
		t.Fatalf("the child read the call %d times", n)
	}

	// "name" and "NAME": bad_header, the child reads nothing of it.
	twin, _, _ := sealedCall(t, node, "fs", session, "echo", "2", `{"text":"MARKER-twin"}`)
	twin = strings.Replace(twin, `"name":"echo"`, `"name":"echo","NAME":"pid"`, 1)
	resp, _, _ = pp.raw(t, mcpSealedPreamble("fs", session, "2"), mcpPost(twin, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" {
		t.Fatalf("name and NAME: %d %v", resp.StatusCode, resp.Header)
	}
	if got := received(); strings.Contains(got, "NAME") || strings.Contains(got, "MARKER-twin") {
		t.Fatalf("the child read a refused call: %s", got)
	}
}

// sealedSend is a JSON-RPC SendMessage of a new task whose one part is body sealed to node for service, as a hiring
// node sends it (the gate's copy: no contextId upstream), with the job's key and rk.
func sealedSend(t *testing.T, node *age.X25519Identity, service, ctx, msgID, body string) (string, *age.X25519Identity, []byte) {
	t.Helper()
	job, _ := age.GenerateX25519Identity()
	rk, _ := sealed.NewRK()
	hd := sealed.Header{V: 1, Dir: sealed.DirRequest, Service: service, IAT: time.Now().Unix(), ContextID: ctx, MessageID: msgID,
		Part: 0, Parts: 1, Kind: sealed.KindText, Kids: []string{tunnel.KeyID(node.Recipient().String())},
		ReplyTo: job.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal([]age.Recipient{node.Recipient()}, hd, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	part, _ := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text"})
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": map[string]any{
		"messageId": msgID, "role": "ROLE_USER", "parts": []json.RawMessage{part},
		"metadata": map[string]string{sealed.MetadataReplyTo: job.Recipient().String()}}}})
	return string(b), job, rk
}

func a2aPost(body string) string {
	return fmt.Sprintf("POST / HTTP/1.1\r\nHost: gate.invalid\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// A request the node answered itself opens no job (ext-sealed.md §5): a sealed first message to a service that does
// not answer is the node's 502 target_unreachable, and the gate's retry of it is accepted once the service is up; the
// same message again after the service accepted it is replayed.
func TestSealedNodeAnswerIsRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: the target is unreachable
	target, _ := url.Parse("http://" + addr)
	pp := newProxyPair(t, 0, a2aServe("hello", target))
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	pre := hostedPreamble("hello")
	body, _, _ := sealedSend(t, node, "hello", pre.Context, "m-1", "MARKER")
	resp, _, _ := pp.raw(t, pre, a2aPost(body))
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeTargetUnreachable) {
		t.Fatalf("an unreachable service: %d %v", resp.StatusCode, resp.Header)
	}
	rec := &recordingTarget{}
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port was taken meanwhile: %v", err)
	}
	srv := &http.Server{Handler: rec}
	go func() { _ = srv.Serve(ln2) }()
	t.Cleanup(func() { _ = srv.Close() })
	resp, _, _ = pp.raw(t, pre, a2aPost(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the gate's retry: %d %v", resp.StatusCode, resp.Header)
	}
	rec.mu.Lock()
	got := string(rec.bodies[0])
	rec.mu.Unlock()
	if !strings.Contains(got, "MARKER") || strings.Contains(got, sealed.MediaType) {
		t.Fatalf("the service got %s", got)
	}
	resp, _, _ = pp.raw(t, pre, a2aPost(body))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "replayed" {
		t.Fatalf("the same message after the service accepted it: %d %v", resp.StatusCode, resp.Header)
	}
}

// A sealed part over 256 KiB reaches the node as kontra-blob:<ref> (relay.md §10.4) and opens only once its whole age
// file is fetched and authenticated: an object cut short is undecryptable, and the service sees nothing of it.
func TestSealedBlobPart(t *testing.T) {
	bg := &blobGate{objs: map[blob.Ref][]byte{}}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Blobs: bg} })
	rec := &recordingTarget{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: rec}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	cfg := testConfig(t, g.url, "", mustServe(t, "a2a:hello=http://"+ln.Addr().String()))
	tn := mustNode(t, g, cfg).run(t)
	tn.welcomed(t)
	s := g.session(t)
	id, err := nodekey.Load(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	pre := hostedPreamble("hello")
	big := strings.Repeat("MARKER-big ", 300<<10/11)
	job, _ := age.GenerateX25519Identity()
	rk, _ := sealed.NewRK()
	hd := sealed.Header{V: 1, Dir: sealed.DirRequest, Service: "hello", IAT: time.Now().Unix(), ContextID: pre.Context, MessageID: "m-big",
		Part: 0, Parts: 1, Kind: sealed.KindText, Kids: []string{id.KeyID()},
		ReplyTo: job.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal([]age.Recipient{id.Active.Recipient()}, hd, []byte(big))
	if err != nil {
		t.Fatal(err)
	}
	good, _ := blob.NewRef()
	cut, _ := blob.NewRef()
	bg.mu.Lock()
	bg.objs[good], bg.objs[cut] = ct, ct[:len(ct)-100]
	bg.mu.Unlock()
	send := func(ref blob.Ref, msgID string) *http.Response {
		t.Helper()
		part, _ := sealed.MarshalPart(sealed.Part{BlobRef: string(ref), Original: "text"})
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": map[string]any{
			"messageId": msgID, "role": "ROLE_USER", "parts": []json.RawMessage{part},
			"metadata": map[string]string{sealed.MetadataReplyTo: job.Recipient().String()}}}})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tb, err := upstream.NewTunnel("hello", tunnel.KindA2A, preambleOpener{s: s, pre: tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello",
			Kind: tunnel.KindA2A, Task: pre.Task, Context: pre.Context}})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.Endpoint(tb, upstream.Info{Path: "/"}), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := tb.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
		return res
	}
	if res := send(cut, "m-big"); res.StatusCode != http.StatusBadRequest || res.Header.Get(tunnel.HeaderSealedReject) != "undecryptable" {
		t.Fatalf("a blob cut short: %d %v", res.StatusCode, res.Header)
	}
	rec.mu.Lock()
	n := len(rec.bodies)
	rec.mu.Unlock()
	if n != 0 {
		t.Fatal("the service saw a part whose file was not authenticated")
	}
	if res := send(good, "m-big"); res.StatusCode != http.StatusOK {
		t.Fatalf("the whole blob: %d %v", res.StatusCode, res.Header)
	}
	rec.mu.Lock()
	got := rec.bodies[0]
	rec.mu.Unlock()
	var req struct {
		Params struct {
			Message struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(got, &req); err != nil || len(req.Params.Message.Parts) != 1 || req.Params.Message.Parts[0].Text != big {
		t.Fatalf("the service got %.200s", got)
	}
}

// D221: the hosting node names its services as the gate's URL paths do, from the welcome: "<tenant>/<name>" when the
// service's URL ends with the tenant and the name (a multi-tenant gate, /agents/ or /mcp/), "<name>" otherwise. The
// name is always the service's own.
func TestServicePaths(t *testing.T) {
	w := &tunnel.Welcome{Tenant: "acme", Services: []tunnel.WelcomeService{
		{Kind: tunnel.KindA2A, Name: "hello", URL: "https://gate.example/agents/acme/hello"},
		{Kind: tunnel.KindMCP, Name: "fs", URL: "https://gate.example/mcp/acme/fs"},
		{Kind: tunnel.KindA2A, Name: "single", URL: "https://gate.example/agents/single"},
		{Kind: tunnel.KindMCP, Name: "web", URL: "https://gate.example/agents/acme/web"}, // an MCP service under /agents/: not the gate's path
		{Kind: tunnel.KindA2A, Name: "odd", URL: "https://gate.example/agents/other/odd"},
		{Kind: tunnel.KindA2A, Name: "bad", URL: "::not a url"},
	}}
	want := map[string]string{"hello": "acme/hello", "fs": "acme/fs", "single": "single", "web": "web", "odd": "odd", "bad": "bad"}
	if got := servicePaths(w); !maps.Equal(got, want) {
		t.Fatalf("servicePaths = %v, want %v", got, want)
	}
	team := &tunnel.Welcome{Tenant: "default", Services: []tunnel.WelcomeService{{Kind: tunnel.KindA2A, Name: "hello", URL: "http://localhost:8080/agents/hello"}}}
	if got := servicePaths(team); got["hello"] != "hello" {
		t.Fatalf("a single-tenant gate's service: %v", got)
	}
}
