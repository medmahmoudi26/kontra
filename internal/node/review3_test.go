package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
)

// M3 review round 3: regression tests of the hosting node's MCP sealing through the proxy.

func r3Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func r3Init(t *testing.T, pp *proxyPair, service string) string {
	t.Helper()
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"sampling":{}},"clientInfo":{"name":"c","version":"1"}}}`
	resp, _, _ := pp.raw(t, mcpPreamble(service), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	return sid
}

// r3GET is the client's GET stream request on session sid, as the gate forwards it (lastEventID "": none).
func r3GET(sid, lastEventID string) string {
	h := "GET / HTTP/1.1\r\nHost: gate.invalid\r\nAccept: text/event-stream\r\n" + mcpwire.HeaderSessionID + ": " + sid + "\r\n" +
		mcpwire.HeaderProtocolVersion + ": " + mcpwire.V20251125 + "\r\n"
	if lastEventID != "" {
		h += mcpwire.HeaderLastEventID + ": " + lastEventID + "\r\n"
	}
	return h + "Connection: close\r\n\r\n"
}

// r3OpenInput opens a sealed input request's envelope with the job key.
func r3OpenInput(t *testing.T, key *age.X25519Identity, data string) sealed.Header {
	t.Helper()
	m, err := mcpwire.Parse([]byte(data), mcpwire.FromUpstream)
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	meta, _ := mcpwire.Member(m.Params, "_meta")
	env, ok := mcpwire.Member(meta, mcpwire.MetaSealed)
	if !ok {
		t.Fatalf("the input request is not sealed: %s", data)
	}
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	hd, _, err := sealed.Open([]age.Identity{key}, ct)
	if err != nil {
		t.Fatalf("the input request does not open with the job's key: %v", err)
	}
	return hd
}

// r3-hosting-1 (D232): a sealed plain call on a stdio service whose server asks the client (sampling/createMessage)
// with a prompt built from the sealed arguments. The bridge routes the request to the session's GET stream; it
// reaches the gate sealed to the buyer's job, and the gate's plaintext answer to it is refused and never reaches the
// child.
func TestReview3StdioPlainCallInputOnGET(t *testing.T) {
	bin := stdioServer(t)
	rec := t.TempDir()
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "fs", Command: []string{bin, "-record", rec}})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "fs")
	session := r3Hash("the buyer's gate session")
	gresp, gbr := pp.streamed(t, mcpSealedPreamble("fs", session, ""), r3GET(sid, ""))
	if gresp.StatusCode != 200 {
		t.Fatalf("GET: %d", gresp.StatusCode)
	}
	call, key, _ := sealedCall(t, node, "fs", session, "ask", "1", `{"text":"SECRET-SEALED-ARG"}`)
	st := pp.open(t, mcpSealedPreamble("fs", session, "1"))
	if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
		t.Fatal(err)
	}
	data := eventData(nextEvent(t, gbr))
	if strings.Contains(data, "SECRET-SEALED-ARG") || !strings.Contains(data, "sampling/createMessage") {
		t.Fatalf("the sealed plain call's sampling request on the GET stream: %s", data)
	}
	if hd := r3OpenInput(t, key, data); hd.Tool != "ask" || hd.MessageID != "sample-2" || hd.TaskID != "" {
		t.Fatalf("the input request's header: %+v", hd)
	}
	// The gate answers the buyer's input request itself, in plaintext, on the buyer's own session exchange.
	ans := `{"jsonrpc":"2.0","id":"sample-2","result":{"role":"assistant","content":{"type":"text","text":"GATE-WROTE-THIS"},"model":"m"}}`
	resp, body, _ := pp.raw(t, mcpSealedPreamble("fs", session, ""), mcpPost(ans, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectBadHeader) {
		t.Fatalf("the gate's plaintext answer: %d %q %s", resp.StatusCode, resp.Header.Get(tunnel.HeaderSealedReject), body)
	}
	time.Sleep(200 * time.Millisecond)
	files, _ := filepath.Glob(filepath.Join(rec, "sample-*.json"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "GATE-WROTE-THIS") {
			t.Fatalf("the child took the gate's plaintext answer: %s", b)
		}
	}
}

// r3-hosting-1 (D232): a URL server (answering JSON) sends its input request about a sealed call in flight on the
// session's GET stream, here one the gate opened with Last-Event-ID: it reaches the gate sealed.
func TestReview3URLInputOnResumedGET(t *testing.T) {
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("ask", "")}
	release := make(chan struct{})
	got := make(chan string, 1)
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		got <- r.SessionID
		<-release
		return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"answered"}],"isError":false}`)}
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "web")
	session := r3Hash("the buyer's gate session")
	call, key, _ := sealedCall(t, node, "web", session, "ask", "1", `{"text":"SECRET-SEALED-ARG"}`)
	st := pp.open(t, mcpSealedPreamble("web", session, "1"))
	if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never got the call")
	}
	gresp, gbr := pp.streamed(t, mcpSealedPreamble("web", session, ""), r3GET(sid, "ev-1"))
	if gresp.StatusCode != 200 {
		t.Fatalf("GET: %d", gresp.StatusCode)
	}
	if err := up.Push(sid, []byte(`{"jsonrpc":"2.0","id":"s1","method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"prompt: SECRET-SEALED-ARG"}}],"maxTokens":10}}`)); err != nil {
		t.Fatal(err)
	}
	var data string
	for data == "" {
		data = eventData(nextEvent(t, gbr)) // the priming event first
	}
	if strings.Contains(data, "SECRET-SEALED-ARG") {
		t.Fatalf("the sealed call's input request on a resumed GET: %s", data)
	}
	if hd := r3OpenInput(t, key, data); hd.Tool != "ask" || hd.MessageID != "s1" {
		t.Fatalf("the input request's header: %+v", hd)
	}
}

// statefulUp is an MCP URL double whose sessions keep state: login {secret} stores the secret in the upstream
// session, whoami answers it (a login, an opened document: what D226 protects). block, when set, holds the first
// login until it is closed, after signalling got.
func statefulUp(t *testing.T, got, block chan struct{}) (*mcptest.Server, *url.URL) {
	t.Helper()
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("login", ""), mcptest.Tool("whoami", "")}
	var mu sync.Mutex
	state := map[string]string{}
	logins := 0
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Secret string `json:"secret"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(r.Params, &p)
		switch p.Name {
		case "login":
			mu.Lock()
			logins++
			first := logins == 1
			state[r.SessionID] = p.Arguments.Secret
			mu.Unlock()
			if first && block != nil {
				close(got)
				<-block
			}
			return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"logged in"}],"isError":false}`)}
		case "whoami":
			mu.Lock()
			s := state[r.SessionID]
			mu.Unlock()
			return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"session secret: ` + s + `"}],"isError":false}`)}
		}
		return mcptest.Reply{Result: json.RawMessage(`{"content":[],"isError":true}`)}
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	return up, target
}

// r3-hosting-3 (D233): a URL MCP server that answers a call once it is done (JSON). The gate sends the buyer's sealed
// call, waits until the server has it, and ends the tunnel stream before the answer: the call was handed whole and is
// recorded. The gate's own sealed call on the buyer's session is then wrong_job (it cannot bind the session's job to
// its key and read the buyer's session state), and the buyer's call resent is replayed (it does not run again).
func TestReview3MCPCutBeforeAnswerIsRecorded(t *testing.T) {
	for _, mode := range []string{"bind", "replay"} {
		t.Run(mode, func(t *testing.T) {
			got, block := make(chan struct{}), make(chan struct{})
			up, target := statefulUp(t, got, block)
			pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
			hosting, node := hostingFor(t)
			pp.p.seal = hosting
			sid := r3Init(t, pp, "web")
			h := r3Hash("the buyer's gate session")
			call, _, _ := sealedCall(t, node, "web", h, "login", "1", `{"secret":"BUYER-SECRET-LOGIN"}`)
			st := pp.open(t, mcpSealedPreamble("web", h, "1"))
			if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-got:
			case <-time.After(10 * time.Second):
				t.Fatal("the server never got the call")
			}
			_ = st.Close() // the gate ends the exchange before the answer
			time.Sleep(300 * time.Millisecond)
			close(block)
			time.Sleep(100 * time.Millisecond)
			switch mode {
			case "bind":
				gcall, _, _ := sealedCall(t, node, "web", h, "whoami", "900", `{}`)
				resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "900"), mcpPost(gcall, sid, mcpwire.V20251125))
				if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectWrongJob) {
					t.Fatalf("the gate's own sealed call on the buyer's session: %d %q %s", resp.StatusCode,
						resp.Header.Get(tunnel.HeaderSealedReject), body)
				}
			case "replay":
				resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "1"), mcpPost(call, sid, mcpwire.V20251125))
				if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectReplayed) {
					t.Fatalf("the buyer's call resent: %d %q %s", resp.StatusCode, resp.Header.Get(tunnel.HeaderSealedReject), body)
				}
				if n := up.Calls("login"); n != 1 {
					t.Fatalf("the server ran the buyer's call %d times", n)
				}
			}
		})
	}
}

// r3modern is a 2026-07-28 POST as the gate would forward it.
func r3modern(method, name, body string) string {
	return "POST / HTTP/1.1\r\nHost: gate.invalid\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
		"MCP-Protocol-Version: 2026-07-28\r\nMcp-Method: " + method + "\r\nMcp-Name: " + name + "\r\n" +
		"Content-Length: " + itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// r3-hosting-2 (D234): the buyer's sealed tools/call, re-sent by the gate as a 2026-07-28 request (no session; the
// modern _meta keys beside the envelope, which binds only sh.kontra/sealed), is refused by the node itself: the
// dual-era server's multi round-trip answer (inputRequests, requestState) and modern CreateTaskResult would carry
// what the arguments built outside what the node seals. Nothing reaches the server.
func TestReview3SealedCallAsModernIsRefused(t *testing.T) {
	for _, tasks := range []bool{false, true} {
		t.Run(map[bool]string{false: "mrtr", true: "task"}[tasks], func(t *testing.T) {
			up := mcptest.New()
			up.Modern = true
			up.Tools = []json.RawMessage{mcptest.Tool("ask", "")}
			up.OnCall = func(r mcptest.Request) mcptest.Reply {
				var p struct {
					Arguments struct {
						Secret string `json:"secret"`
					} `json:"arguments"`
				}
				_ = json.Unmarshal(r.Params, &p)
				return mcptest.Reply{Result: mcptest.InputRequired(map[string]any{"q1": map[string]any{"method": "sampling/createMessage",
					"params": map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text",
						"text": "prompt built from the arguments: " + p.Arguments.Secret}}}, "maxTokens": 10}}}, "state-"+p.Arguments.Secret)}
			}
			up.ModernTaskRun = func(task *mcptest.Task) {
				task.Complete(json.RawMessage(`{"content":[{"type":"text","text":"result"}],"isError":false}`))
			}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			target, _ := url.Parse(srv.URL + "/mcp")
			pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
			hosting, node := hostingFor(t)
			pp.p.seal = hosting
			h := r3Hash("the buyer's gate session")
			call, _, _ := sealedCall(t, node, "web", h, "ask", "1", `{"secret":"BUYER-SECRET-ARG"}`)
			caps := `{}`
			if tasks {
				caps = `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`
			}
			call = strings.Replace(call, `"_meta":{`, `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":`+caps+`,`, 1)
			resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "1"), r3modern("tools/call", "ask", call))
			if strings.Contains(body, "BUYER-SECRET-ARG") || resp.StatusCode != http.StatusBadRequest ||
				resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectBadHeader) {
				t.Fatalf("a sealed call as a 2026-07-28 request: %d %q %s", resp.StatusCode, resp.Header.Get(tunnel.HeaderSealedReject), body)
			}
			if n := up.Calls("ask"); n != 0 {
				t.Fatalf("the server ran the call %d times", n)
			}
		})
	}
}

// r3-hosting-4 (D235): the sealing reads the route the service gets. A dot segment no longer hides a task list
// (GET /tasks/x/.. reaches the service as GET /tasks, a2a-go's ListTasks) or a push config route
// (/tasks/{id}/./pushNotificationConfigs) from it: both are refused on a guarded service and reach nothing.
func TestReview3DotSegmentRoutesAreRefused(t *testing.T) {
	rec := newRecorder(t)
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tasks":[],"nextPageToken":"","pageSize":0,"totalSize":0}`)
		return true
	})
	pp := newProxyPair(t, 0, Serve{Kind: tunnel.KindA2A, Name: "echo", Target: rec.url()})
	id, _ := age.GenerateX25519Identity()
	h, err := sealingHosting(t, id, "echo")
	if err != nil {
		t.Fatal(err)
	}
	pp.p.seal = h
	for _, target := range []string{"/tasks", "/tasks/x/..", "/tasks/x/../", "//tasks", "/tasks/up-B/./pushNotificationConfigs",
		"/tasks/up-B//pushNotificationConfigs"} {
		before := rec.count()
		resp, body, _ := pp.raw(t, hostedPreamble("echo"), get(target, "gate.invalid"))
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectBadHeader) {
			t.Errorf("GET %s: %d %q %s", target, resp.StatusCode, resp.Header.Get(tunnel.HeaderSealedReject), body)
		}
		if rec.count() != before {
			t.Errorf("GET %s reached the service as %q", target, rec.last(t).uri)
		}
	}
}

// sealingHosting is a hosting node's sealing with id, required for the services named.
func sealingHosting(t *testing.T, id *age.X25519Identity, required ...string) (*sealing.Hosting, error) {
	t.Helper()
	return sealing.NewHosting(sealing.HostingConfig{Dir: filepath.Join(t.TempDir(), "hosted", "g"),
		Identities: func() []age.Identity { return []age.Identity{id} },
		KeyIDs:     func() []string { return []string{tunnel.KeyID(id.Recipient().String())} }, Required: required}, nil)
}

// hostingFor's sealing with the node's upstream groups of serves (setupSealing's Shared, D236).
func hostingShared(t *testing.T, serves ...Serve) (*sealing.Hosting, *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h, err := sealing.NewHosting(sealing.HostingConfig{
		Dir:        filepath.Join(t.TempDir(), "hosted", "gate.test"),
		Identities: func() []age.Identity { return []age.Identity{id} },
		KeyIDs:     func() []string { return []string{tunnel.KeyID(id.Recipient().String())} },
		Shared:     upstreamGroups(serves),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, id
}

// r3-hosting-5 (D236): two MCP services of one node on one URL server (a read-only and a full tier, say). The buyer's
// sealed login on web sets up its upstream session; an unsealed call on web2 with the buyer's Mcp-Session-Id is
// refused as one on web is (D226), since the two services share one sealed state.
func TestReview3TwoServicesOneUpstreamMCP(t *testing.T) {
	_, target := statefulUp(t, nil, nil)
	serves := []Serve{{Kind: tunnel.KindMCP, Name: "web", Target: target}, {Kind: tunnel.KindMCP, Name: "web2", Target: target}}
	pp := newMCPProxyPair(t, MCPOptions{}, serves...)
	hosting, node := hostingShared(t, serves...)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "web")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, node, "web", h, "login", "1", `{"secret":"BUYER-SECRET-LOGIN"}`)
	resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != 200 || strings.Contains(body, "BUYER") {
		t.Fatalf("the buyer's sealed login: %d %s", resp.StatusCode, body)
	}
	whoami := `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`
	for _, svc := range []string{"web", "web2"} {
		resp, body, _ = pp.raw(t, mcpPreamble(svc), mcpPost(whoami, sid, mcpwire.V20251125))
		if strings.Contains(body, "BUYER-SECRET-LOGIN") || resp.StatusCode != http.StatusBadRequest ||
			resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectBadHeader) {
			t.Fatalf("an unsealed call on %s with the buyer's Mcp-Session-Id: %d %q %s", svc, resp.StatusCode,
				resp.Header.Get(tunnel.HeaderSealedReject), body)
		}
	}
}

// r3-hosting-5 (D236): which services share an upstream.
func TestReview3UpstreamGroups(t *testing.T) {
	u := func(s string) *url.URL { v, _ := url.Parse(s); return v }
	serves := []Serve{
		{Kind: tunnel.KindA2A, Name: "echo", Target: u("http://127.0.0.1:9999")},
		{Kind: tunnel.KindA2A, Name: "echo2", Target: u("http://localhost:9999/")},
		{Kind: tunnel.KindMCP, Name: "mcp", Target: u("http://127.0.0.1:9999/mcp")},
		{Kind: tunnel.KindMCP, Name: "a", Target: u("http://10.0.0.1:80/a")},
		{Kind: tunnel.KindMCP, Name: "b", Target: u("http://10.0.0.1/b")},
		{Kind: tunnel.KindMCP, Name: "c", Target: u("http://10.0.0.1/a/c")},
		{Kind: tunnel.KindMCP, Name: "d", Target: u("https://10.0.0.1/a")},
		{Kind: tunnel.KindMCP, Name: "fs", Command: []string{"x"}},
		{Kind: tunnel.KindMCP, Name: "fs2", Command: []string{"x"}},
	}
	got := upstreamGroups(serves)
	want := map[string]string{"echo": "echo", "echo2": "echo", "mcp": "echo", "a": "a", "c": "a"}
	if len(got) != len(want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("groups %v, want %v", got, want)
		}
	}
}

// r3-hosting-8 (D238): a URL MCP server that answers a sealed tools/call with a non-2xx whose body is not JSON (Go's
// http.Error, a framework's text error page), quoting the argument it refused: the gate gets the status and the
// node's own JSON-RPC error with fixed text, never the server's text.
func TestReview3MCPNon2xxTextErrorIsScrubbed(t *testing.T) {
	up := mcptest.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"tools/call"`) {
			var m struct {
				Params struct {
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.Unmarshal(b, &m)
			http.Error(w, "invalid arguments: "+string(must(json.Marshal(m.Params.Arguments))), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		up.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "web")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, node, "web", h, "echo", "1", `{"secret":"BUYER-SECRET-ARG"}`)
	resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if strings.Contains(body, "BUYER-SECRET-ARG") || resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(resp.Header.Get("Content-Type"), "application/json") || !strings.Contains(body, `"id":1`) {
		t.Fatalf("a sealed call answered 400 text/plain: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
