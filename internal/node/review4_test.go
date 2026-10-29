package node

import (
	"encoding/json"
	"io"
	"net"
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
	"kontra.sh/kontra/internal/tunnelgate"
)

// M3 review round 4: regression tests of the hosting node's MCP sealing through the proxy.

// r4-hosting-1 (D252): the buyer's sealed task-augmented call reaches a URL server, and the gate ends the exchange
// before the server's CreateTaskResult comes: End records the call (D233), and the task is never bound to the job. On
// the buyer's own session (its preamble and Mcp-Session-Id, both the gate's to choose) tasks/result of that task is
// wrong_task, and tasks/list lists none of it: the task's result and statusMessage never reach the gate in plaintext.
func TestReview4UnboundTaskURL(t *testing.T) {
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("longRun", "")}
	taskID := make(chan string, 1)
	up.TaskRun = func(task *mcptest.Task) {
		var p struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(task.Call.Params, &p)
		taskID <- task.ID
		task.Complete(json.RawMessage(`{"content":[{"type":"text","text":"result of ` + p.Arguments.Text + `"}],"isError":false}`))
	}
	hold := make(chan struct{})
	got := make(chan struct{}, 1)
	up.Hold = func(r mcptest.Request) {
		if r.Method == "tools/call" {
			got <- struct{}{}
			<-hold
		}
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "web")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, node, "web", h, "longRun", "1", `{"text":"SECRET-SEALED-ARG"}`)
	call = strings.Replace(call, `"arguments":{}`, `"arguments":{},"task":{"ttl":60000}`, 1)
	st := pp.open(t, mcpSealedPreamble("web", h, "1"))
	if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never got the call")
	}
	_ = st.Close() // the gate ends the exchange before the CreateTaskResult
	time.Sleep(300 * time.Millisecond)
	close(hold)
	var id string
	select {
	case id = <-taskID:
	case <-time.After(10 * time.Second):
		t.Fatal("no task")
	}
	time.Sleep(200 * time.Millisecond)
	tr := `{"jsonrpc":"2.0","id":51,"method":"tasks/result","params":{"taskId":"` + id + `"}}`
	resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, ""), mcpPost(tr, sid, mcpwire.V20251125))
	if strings.Contains(body, "SECRET-SEALED-ARG") || resp.StatusCode != http.StatusBadRequest ||
		resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectWrongTask) {
		t.Fatalf("tasks/result of the buyer's unbound task on its session: %d %q %s", resp.StatusCode,
			resp.Header.Get(tunnel.HeaderSealedReject), body)
	}
	tl := `{"jsonrpc":"2.0","id":52,"method":"tasks/list","params":{}}`
	resp, body, _ = pp.raw(t, mcpSealedPreamble("web", h, ""), mcpPost(tl, sid, mcpwire.V20251125))
	if strings.Contains(body, id) || strings.Contains(body, "SECRET") {
		t.Fatalf("tasks/list on the buyer's session lists the unbound task: %d %s", resp.StatusCode, body)
	}
}

// r4-hosting-1 (D252) on the stdio bridge: the gate closes the call's POST before the child's CreateTaskResult, which
// the bridge then drops (no POST stream is open for it). The task the child created stays out of the gate's reach on
// the buyer's own session: tasks/result is wrong_task, and tasks/list lists none of it.
func TestReview4UnboundTaskStdio(t *testing.T) {
	bin := stdioServer(t)
	rec := t.TempDir()
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "fs", Command: []string{bin, "-record", rec}})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "fs")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, node, "fs", h, "longrun", "1", `{"text":"SECRET-SEALED-ARG","delayMs":600}`)
	call = strings.Replace(call, `"arguments":{}`, `"arguments":{},"task":{"ttl":60000}`, 1)
	st := pp.open(t, mcpSealedPreamble("fs", h, "1"))
	if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the child reads the call", func() bool {
		files, _ := filepath.Glob(filepath.Join(rec, "received-*.jsonl"))
		for _, f := range files {
			if b, _ := os.ReadFile(f); strings.Contains(string(b), "longrun") {
				return true
			}
		}
		return false
	})
	_ = st.Close() // the gate ends the exchange before the CreateTaskResult
	time.Sleep(time.Second)
	tr := `{"jsonrpc":"2.0","id":51,"method":"tasks/result","params":{"taskId":"task-1"}}`
	resp, body, _ := pp.raw(t, mcpSealedPreamble("fs", h, ""), mcpPost(tr, sid, mcpwire.V20251125))
	if strings.Contains(body, "SECRET-SEALED-ARG") || resp.StatusCode != http.StatusBadRequest ||
		resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectWrongTask) {
		t.Fatalf("tasks/result of the buyer's unbound task on its session: %d %q %s", resp.StatusCode,
			resp.Header.Get(tunnel.HeaderSealedReject), body)
	}
	tl := `{"jsonrpc":"2.0","id":52,"method":"tasks/list","params":{}}`
	resp, body, _ = pp.raw(t, mcpSealedPreamble("fs", h, ""), mcpPost(tl, sid, mcpwire.V20251125))
	if strings.Contains(body, "task-1") || strings.Contains(body, "SECRET") {
		t.Fatalf("tasks/list on the buyer's session lists the unbound task: %d %s", resp.StatusCode, body)
	}
}

// sessionlessUp is a URL MCP server that mints no Mcp-Session-Id. Its tool "sample" sends a sampling/createMessage
// built from its arguments on the call's own SSE stream, waits for the client's answer (POSTed as a response, with no
// session), and returns what the answer said. answers lists every answer it took.
func sessionlessUp(t *testing.T) (*url.URL, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var answers []string
	waiting := make(chan json.RawMessage, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(b, &m)
		switch {
		case m.Method == "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"sl","version":"1"}}}`)
		case m.Method == "" && m.Result != nil:
			mu.Lock()
			answers = append(answers, string(b))
			mu.Unlock()
			waiting <- m.Result
			w.WriteHeader(http.StatusAccepted)
		case m.Method == "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"prompt from the arguments: `+m.Params.Arguments.Text+`"}}],"maxTokens":10}}`+"\n\n")
			fl.Flush()
			var ans json.RawMessage
			select {
			case ans = <-waiting:
			case <-r.Context().Done():
				// The node went (the test is over): an active handler would hold srv.Close (httptest logs "blocked
				// in Close after 5 seconds") for the 10 s below.
				return
			case <-time.After(10 * time.Second):
				ans = json.RawMessage(`"none"`)
			}
			q, _ := json.Marshal(string(ans))
			_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"content":[{"type":"text","text":`+string(q)+`}],"isError":false}}`+"\n\n")
			fl.Flush()
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL + "/mcp")
	return u, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), answers...) }
}

// r4-hosting-2 (D253): a URL server that mints no Mcp-Session-Id. The buyer's sealed plain call's sampling request
// comes sealed on the call's own stream; the gate's own plaintext answer to it, POSTed with no Mcp-Session-Id under
// a preamble that names no session, is refused bad_header and never reaches the server.
func TestReview4SessionlessAnswerURL(t *testing.T) {
	target, answers := sessionlessUp(t)
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"sampling":{}},"clientInfo":{"name":"c","version":"1"}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
	if resp.StatusCode != 200 || resp.Header.Get(mcpwire.HeaderSessionID) != "" {
		t.Fatalf("initialize: %d %q", resp.StatusCode, resp.Header.Get(mcpwire.HeaderSessionID))
	}
	session := r3Hash("the buyer's gate session")
	call, key, _ := sealedCall(t, node, "web", session, "sample", "1", `{"text":"SECRET-SEALED-ARG"}`)
	cresp, cbr := pp.streamed(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, "", mcpwire.V20251125))
	if cresp.StatusCode != 200 {
		t.Fatalf("the sealed call: %d", cresp.StatusCode)
	}
	data := eventData(nextEvent(t, cbr))
	if strings.Contains(data, "SECRET-SEALED-ARG") {
		t.Fatalf("the input request is not sealed: %s", data)
	}
	r3OpenInput(t, key, data)
	ans := `{"jsonrpc":"2.0","id":0,"result":{"role":"assistant","content":{"type":"text","text":"GATE-WROTE-THIS"},"model":"m"}}`
	aresp, abody, _ := pp.raw(t, mcpPreamble("web"), mcpPost(ans, "", mcpwire.V20251125))
	if aresp.StatusCode != http.StatusBadRequest || aresp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectBadHeader) {
		t.Errorf("the gate's plaintext answer: %d %q %s", aresp.StatusCode, aresp.Header.Get(tunnel.HeaderSealedReject), abody)
	}
	time.Sleep(200 * time.Millisecond)
	for _, a := range answers() {
		if strings.Contains(a, "GATE-WROTE-THIS") {
			t.Fatalf("the server took the gate's plaintext answer to the buyer's sealed input request: %s", a)
		}
	}
}

// r4-hosting-3 (D254): the node restarts (a crash, an OOM kill, a reboot) while the buyer's first sealed call on a
// session is with a URL server that answers JSON (its headers come with its result). The call's records were on disk
// before it was handed, and are applied as the node starts: the buyer's upstream session is its job's, and the gate's
// unsealed call on it, under a preamble that names no session, is refused; the buyer's call resent is replayed.
func TestReview4RestartWhileFirstCallInFlight(t *testing.T) {
	got, block := make(chan struct{}), make(chan struct{})
	up, target := statefulUp(t, got, block)
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "hosted", "gate.test")
	open := func() *sealing.Hosting {
		h, err := sealing.NewHosting(sealing.HostingConfig{Dir: dir,
			Identities: func() []age.Identity { return []age.Identity{id} },
			KeyIDs:     func() []string { return []string{tunnel.KeyID(id.Recipient().String())} }}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	pp.p.seal = open()
	sid := r3Init(t, pp, "web")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, id, "web", h, "login", "1", `{"secret":"BUYER-SECRET-LOGIN"}`)
	st := pp.open(t, mcpSealedPreamble("web", h, "1"))
	if _, err := io.WriteString(st, mcpPost(call, sid, mcpwire.V20251125)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never got the call")
	}
	// The node restarts: its in-memory state is gone, its job directory stays.
	pp.p.seal = open()
	// The old process's exchange never answers: it died. In this test it lives on, in this process, until the server is
	// released when the test is over; it then records the server's answer in dir. It is waited for (its stream ends
	// once its handler has returned) before the TempDir holding dir is removed: this cleanup, registered after the
	// TempDir's, runs before it. Released at the test's end without the wait, it wrote there during the removal
	// ("unlinkat .../web/jobs: directory not empty") or after it (a leftover directory in $TMPDIR).
	t.Cleanup(func() {
		close(block)
		_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.Copy(io.Discard, st); err != nil {
			t.Errorf("the old process's exchange did not end: %v", err)
		}
	})
	whoami := `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`
	resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(whoami, sid, mcpwire.V20251125))
	if strings.Contains(body, "BUYER-SECRET-LOGIN") || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("after a restart, the gate's unsealed call on the buyer's upstream session: %d %s", resp.StatusCode, body)
	}
	resp, body, _ = pp.raw(t, mcpSealedPreamble("web", h, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != string(sealed.RejectReplayed) {
		t.Fatalf("after a restart, the buyer's call resent: %d %q %s", resp.StatusCode, resp.Header.Get(tunnel.HeaderSealedReject), body)
	}
	if n := up.Calls("login"); n != 1 {
		t.Fatalf("the server ran the buyer's call %d times", n)
	}
}

// r4-hosting-4 (D255): a DELETE of a sealed job's upstream session, answered 2xx by the server, ends the job's hold on
// it: the server has nothing of the job left there. Until then an unsealed call on it under another preamble is
// refused.
func TestReview4DeleteEndsTheHold(t *testing.T) {
	_, target := statefulUp(t, nil, nil)
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting
	sid := r3Init(t, pp, "web")
	h := r3Hash("the buyer's gate session")
	call, _, _ := sealedCall(t, node, "web", h, "login", "1", `{"secret":"BUYER-SECRET-LOGIN"}`)
	if resp, body, _ := pp.raw(t, mcpSealedPreamble("web", h, "1"), mcpPost(call, sid, mcpwire.V20251125)); resp.StatusCode != 200 {
		t.Fatalf("the sealed call: %d %s", resp.StatusCode, body)
	}
	whoami := `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`
	if resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(whoami, sid, mcpwire.V20251125)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unsealed call on the sealed job's upstream session: %d %s", resp.StatusCode, body)
	}
	del := "DELETE / HTTP/1.1\r\nHost: gate.invalid\r\n" + mcpwire.HeaderSessionID + ": " + sid + "\r\n" +
		mcpwire.HeaderProtocolVersion + ": " + mcpwire.V20251125 + "\r\nConnection: close\r\n\r\n"
	if resp, _, _ := pp.raw(t, mcpSealedPreamble("web", h, ""), del); resp.StatusCode/100 != 2 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	// The session is gone on the server: the request now reaches it, and the server answers it as for any unknown
	// session.
	resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(whoami, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "BUYER-SECRET-LOGIN") {
		t.Fatalf("a call on the deleted session: %d %s", resp.StatusCode, body)
	}
}

// r4-hosting-6 (D257): D236 groups services whose targets reach one upstream, the local host by any of its names:
// 0.0.0.0 and [::] (what a server prints as its listen address, which Linux dials as the local host), localhost. (the
// trailing dot), a name under localhost.
func TestReview4UpstreamGroupAliases(t *testing.T) {
	u := func(s string) *url.URL { v, _ := url.Parse(s); return v }
	for _, alias := range []string{"http://0.0.0.0:3001/mcp", "http://localhost.:3001/mcp", "http://[::]:3001/mcp",
		"http://LOCALHOST:3001/mcp", "http://mcp.localhost:3001/mcp", "http://127.0.0.2:3001/mcp", "http://[::ffff:127.0.0.1]:3001/mcp"} {
		serves := []Serve{{Kind: tunnel.KindMCP, Name: "web", Target: u("http://127.0.0.1:3001/mcp")},
			{Kind: tunnel.KindMCP, Name: "web2", Target: u(alias)}}
		if g := upstreamGroups(serves); g["web2"] != "web" {
			t.Errorf("%s and http://127.0.0.1:3001/mcp are not one upstream group: %v", alias, g)
		}
	}
	serves := []Serve{{Kind: tunnel.KindMCP, Name: "web", Target: u("http://127.0.0.1:3001/mcp")},
		{Kind: tunnel.KindMCP, Name: "web2", Target: u("http://10.0.0.1:3001/mcp")}}
	if g := upstreamGroups(serves); len(g) != 0 {
		t.Errorf("another host is grouped: %v", g)
	}
}

// r4-hiring-5 (D262): the node sends its local clients to localhost, which a stock Linux resolves to ::1 first. A
// loopback --listen binds the port on both loopback families, so no other local user can bind [::1]:<port> and serve
// the node's clients; a client of [::1] reaches the node; and a port another process holds on ::1 stops the start.
func TestReview4HiringBindsBothLoopbackFamilies(t *testing.T) {
	if ln, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("this host has no IPv6 loopback")
	} else {
		_ = ln.Close()
	}
	router := &hireRouter{}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Hire: router} })
	tn := mustNode(t, g, testConfig(t, g.url, "")).run(t)
	tn.welcomed(t)
	port := tn.localPort(t)
	if ln, err := net.Listen("tcp", "[::1]:"+port); err == nil {
		_ = ln.Close()
		t.Fatalf("another process can bind [::1]:%s while the node serves localhost:%s", port, port)
	}
	res, err := http.Get("http://[::1]:" + port + "/agents/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "/agents/hello") {
		t.Fatalf("a client of [::1]: %d %s", res.StatusCode, body)
	}

	squat, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squat.Close()
	_, p, _ := net.SplitHostPort(squat.Addr().String())
	cfg := testConfig(t, g.url, "")
	cfg.Listen = "127.0.0.1:" + p
	tn2 := mustNode(t, g, cfg).run(t)
	if err := tn2.result(t, 10*time.Second); err == nil || !strings.Contains(err.Error(), "[::1]:"+p) {
		t.Fatalf("a --listen whose port another process holds on ::1: %v", err)
	}
}
