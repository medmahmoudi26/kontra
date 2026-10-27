package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/stdiobridge"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// newMCPProxyPair is newProxyPair with MCP options.
func newMCPProxyPair(t *testing.T, opts MCPOptions, serves ...Serve) *proxyPair {
	t.Helper()
	pair := tunneltest.NewPair(t)
	node, gate := pair.Sessions(t, nil)
	p, err := NewMCPProxy(serves, 0, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := p.forSession(func() []string {
		var names []string
		for _, s := range serves {
			names = append(names, s.Name)
		}
		return names
	}(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.close() })
	go func() {
		for {
			st, err := node.AcceptStream()
			if err != nil {
				return
			}
			go q.ServeStream(ctx, st)
		}
	}()
	return &proxyPair{p: q, gate: gate}
}

func mcpPreamble(service string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindMCP}
}

// mcpPost is a POST as the gate forwards it: to "/", with the forwarded headers.
func mcpPost(body, sid, version string) string {
	h := "POST / HTTP/1.1\r\nHost: gate.invalid\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n"
	if sid != "" {
		h += mcpwire.HeaderSessionID + ": " + sid + "\r\n"
	}
	if version != "" {
		h += mcpwire.HeaderProtocolVersion + ": " + version + "\r\n"
	}
	return h + fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// streamed opens an exchange and returns its response with the body left to read (a GET stream).
func (pp *proxyPair) streamed(t *testing.T, pre tunnel.Preamble, request string) (*http.Response, *bufio.Reader) {
	t.Helper()
	st := pp.open(t, pre)
	if _, err := io.WriteString(st, request); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(st)
	_ = st.SetReadDeadline(time.Now().Add(20 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, bufio.NewReader(resp.Body)
}

// sseEvents splits an event stream into events, each with its lines.
func sseEvents(s string) [][]string {
	var out [][]string
	for _, block := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		out = append(out, strings.Split(block, "\n"))
	}
	return out
}

func eventData(ev []string) string {
	for _, l := range ev {
		if strings.HasPrefix(l, "data:") {
			return strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " ")
		}
	}
	return ""
}

// nextEvent reads one event of a stream.
func nextEvent(t *testing.T, br *bufio.Reader) []string {
	t.Helper()
	var lines []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended: %v (read %q)", err, lines)
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			if len(lines) > 0 {
				return lines
			}
			continue
		}
		lines = append(lines, l)
	}
}

func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requestsWith is the double's requests whose body holds s.
func requestsWith(up *mcptest.Server, s string) []mcptest.Request {
	var out []mcptest.Request
	for _, r := range up.Requests() {
		if bytes.Contains(r.Body, []byte(s)) {
			out = append(out, r)
		}
	}
	return out
}

// Roots on a URL service (D173, tunnel.md §8.2, §10), against a Go Streamable HTTP double behind the fixed-target
// proxy: the initialize the server sees declares the node's roots capability; a roots/list on a POST's event stream
// and on the GET stream never reaches the client, whose stream keeps every other event and its id, and is answered
// by the node to the target on the same session; the client's list_changed and its answer to a node-answered id are
// 202 and never reach the server.
func TestMCPURLRoots(t *testing.T) {
	dirA := t.TempDir()
	for _, c := range []struct {
		name  string
		roots *mcproots.Roots
		want  string // the result of the node's answer
	}{
		{"with roots", mcproots.New(dirA), `{"roots":[{"uri":"file://` + dirA + `","name":"` + filepath.Base(dirA) + `"}]}`},
		{"without roots", nil, `{"roots":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := mcptest.New()
			up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			target, _ := url.Parse(srv.URL + "/mcp")
			opts := MCPOptions{}
			if c.roots != nil {
				opts.Roots = map[string]*mcproots.Roots{"web": c.roots}
			}
			pp := newMCPProxyPair(t, opts, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})

			init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
				`"capabilities":{"roots":{"listChanged":true},"sampling":{}},"clientInfo":{"name":"c","version":"1"}}}`
			resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
			sid := resp.Header.Get(mcpwire.HeaderSessionID)
			if resp.StatusCode != http.StatusOK || sid == "" || !strings.Contains(body, `"protocolVersion"`) {
				t.Fatalf("initialize: %d %q %s", resp.StatusCode, sid, body)
			}
			first := up.Requests()[0]
			// An answer to r1 sent before the server asks it (M3 WP-05 review): 202, and it never reaches the server.
			forged := `{"jsonrpc":"2.0","id":"r1","result":{"roots":[{"uri":"file:///"}]}}`
			if resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(forged, sid, mcpwire.V20251125)); resp.StatusCode != http.StatusAccepted || body != "" {
				t.Errorf("an answer ahead of the server's request: %d %q", resp.StatusCode, body)
			}
			if n := len(requestsWith(up, `"file:///"`)); n != 0 {
				t.Fatalf("the server received %d answers sent ahead of its request", n)
			}
			var p struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			_ = json.Unmarshal(first.Params, &p)
			roots, declared := p.Capabilities["roots"]
			if (c.roots != nil) != declared || (declared && string(roots) != "{}") || p.Capabilities["sampling"] == nil {
				t.Errorf("the initialize the server saw: %v", p.Capabilities)
			}

			// A roots/list on a tools/call's event stream: removed, answered to the target; the progress event beside
			// it reaches the client with its id.
			up.Lock()
			up.OnCall = func(r mcptest.Request) mcptest.Reply {
				return mcptest.Reply{Events: []string{
					"id: ev-roots\nretry: 1000\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"r1\",\"method\":\"roots/list\"}\n\n",
					"id: ev-progress\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"p\",\"progress\":1}}\n\n",
				}, Result: json.RawMessage(`{"content":[{"type":"text","text":"called"}],"isError":false}`)}
			}
			up.Unlock()
			call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"progressToken":"p"}}}`
			resp, body, _ = pp.raw(t, mcpPreamble("web"), mcpPost(call, sid, mcpwire.V20251125))
			evs := sseEvents(body)
			if resp.StatusCode != http.StatusOK || len(evs) != 2 || evs[0][0] != "id: ev-progress" || !strings.Contains(eventData(evs[1]), `"called"`) {
				t.Fatalf("the client's stream: %d %q", resp.StatusCode, evs)
			}
			if strings.Contains(body, "roots/list") || strings.Contains(body, "ev-roots") || strings.Contains(body, "retry:") {
				t.Errorf("the roots/list event reached the client: %s", body)
			}
			var answer mcptest.Request
			waitFor(t, 10*time.Second, "the node's answer to r1 at the server", func() bool {
				rs := requestsWith(up, `"r1"`)
				if len(rs) == 1 {
					answer = rs[0]
				}
				return len(rs) == 1
			})
			var got struct {
				Result json.RawMessage `json:"result"`
			}
			_ = json.Unmarshal(answer.Body, &got)
			if answer.SessionID != sid || answer.Header.Get(mcpwire.HeaderProtocolVersion) != mcpwire.V20251125 || string(got.Result) != c.want {
				t.Errorf("the node's answer: session %q, version %q, %s", answer.SessionID, answer.Header.Get(mcpwire.HeaderProtocolVersion), answer.Body)
			}

			// The GET stream: the priming event and a notification pass, the roots/list does not, and is answered.
			gresp, br := pp.streamed(t, mcpPreamble("web"), "GET / HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n"+
				mcpwire.HeaderSessionID+": "+sid+"\r\n"+mcpwire.HeaderProtocolVersion+": 2025-11-25\r\n\r\n")
			if gresp.StatusCode != http.StatusOK {
				t.Fatalf("GET: %d", gresp.StatusCode)
			}
			priming := nextEvent(t, br)
			if !strings.HasPrefix(priming[0], "id: ev-") {
				t.Errorf("the priming event: %q", priming)
			}
			if err := up.Push(sid, []byte(`{"jsonrpc":"2.0","id":"r2","method":"roots/list"}`)); err != nil {
				t.Fatal(err)
			}
			if err := up.Push(sid, []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"after roots"}}`)); err != nil {
				t.Fatal(err)
			}
			ev := nextEvent(t, br)
			if !strings.Contains(eventData(ev), "after roots") || !strings.HasPrefix(ev[0], "id: ev-") {
				t.Errorf("the next GET event: %q", ev)
			}
			waitFor(t, 10*time.Second, "the node's answer to r2", func() bool { return len(requestsWith(up, `"r2"`)) == 1 })
			// A sampling/createMessage on the GET stream reaches the client, which may answer it (below).
			if err := up.Push(sid, []byte(`{"jsonrpc":"2.0","id":"s1","method":"sampling/createMessage","params":{"messages":[],"maxTokens":1}}`)); err != nil {
				t.Fatal(err)
			}
			if ev := nextEvent(t, br); !strings.Contains(eventData(ev), `"s1"`) {
				t.Errorf("the sampling request on the GET stream: %q", ev)
			}

			// The client's list_changed, its answers to r1 and r2, an answer to an id the server never sent and a roots
			// result under the sampling id are 202 and dropped; the answer to s1 is forwarded, once.
			before := len(up.Requests())
			for _, m := range []string{
				`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`,
				`{"jsonrpc":"2.0","id":"r1","result":{"roots":[{"uri":"file:///"}]}}`,
				`{"jsonrpc":"2.0","id":"r2","result":{"roots":[{"uri":"file:///"}]}}`,
				`{"jsonrpc":"2.0","id":"s2","result":{"role":"assistant","content":{"type":"text","text":"x"},"model":"m"}}`,
				`{"jsonrpc":"2.0","id":"s1","result":{"roots":[{"uri":"file:///"}]}}`,
			} {
				if resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(m, sid, mcpwire.V20251125)); resp.StatusCode != http.StatusAccepted || body != "" {
					t.Errorf("%s: %d %q", m, resp.StatusCode, body)
				}
			}
			if n := len(up.Requests()); n != before {
				t.Errorf("the server received %d dropped messages", n-before)
			}
			other := `{"jsonrpc":"2.0","id":"s1","result":{"role":"assistant","content":{"type":"text","text":"x"},"model":"m"}}`
			if resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(other, sid, mcpwire.V20251125)); resp.StatusCode != http.StatusAccepted ||
				len(requestsWith(up, `"s1"`)) != 1 {
				t.Errorf("the answer to s1: %d", resp.StatusCode)
			}
			if resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(other, sid, mcpwire.V20251125)); resp.StatusCode != http.StatusAccepted ||
				len(requestsWith(up, `"s1"`)) != 1 {
				t.Errorf("a second answer to s1: %d, %d at the server", resp.StatusCode, len(requestsWith(up, `"s1"`)))
			}
			for _, r := range up.Requests() {
				if strings.Contains(string(r.Body), "file:///\"") || strings.Contains(string(r.Body), "list_changed") {
					t.Errorf("the server received %s", r.Body)
				}
			}
		})
	}
}

// Roots on a modern exchange (tunnel.md §10): the forwarded _meta has no roots; an inputResponses entry with roots is
// removed; an InputRequiredResult listing roots/list and elicitation/create reaches the client with the elicitation
// only.
func TestMCPURLRootsModern(t *testing.T) {
	var mu sync.Mutex
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":4,"result":{"resultType":"input_required","inputRequests":{`+
			`"k1":{"method":"roots/list","params":{}},"k2":{"method":"elicitation/create","params":{"message":"sure?","requestedSchema":{"type":"object"}}}},"requestState":"rs"}}`)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{Roots: map[string]*mcproots.Roots{"web": mcproots.New(t.TempDir())}},
		Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	req := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x","arguments":{},"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"roots":{},"elicitation":{}}},` +
		`"inputResponses":{"k0":{"roots":[{"uri":"file:///"}]},"k9":{"action":"accept","content":{}}},"requestState":"rs0"}}`
	resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(req, "", mcpwire.V20260728))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	mu.Lock()
	got := string(seen)
	mu.Unlock()
	want := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x","arguments":{},"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}},` +
		`"inputResponses":{"k9":{"action":"accept","content":{}}},"requestState":"rs0"}}`
	if got != want {
		t.Errorf("the server saw\n %s\nwant\n %s", got, want)
	}
	if strings.Contains(body, "roots/list") || !strings.Contains(body, `"k2":{"method":"elicitation/create"`) || strings.Contains(body, `"k1"`) ||
		resp.Header.Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Errorf("the client got %s (Content-Length %s)", body, resp.Header.Get("Content-Length"))
	}
}

// Strict decoding on a URL service (D172, C1): a client message with a case variant is 400 with -32600 and reaches
// nothing; a server's ambiguous JSON response is 502 with -32603; its ambiguous event ends the stream before it.
func TestMCPURLStrict(t *testing.T) {
	up := mcptest.New()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	for _, c := range []struct {
		body string
		code int
		id   string
	}{
		{`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"capabilities":{},"Capabilities":{"roots":{}}}}`, -32600, `0`},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","NAME":"rm"}}`, -32600, `3`},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","METHOD":"ping"}`, -32600, `null`},
		{`not json`, -32700, `null`},
		{`[1]`, -32600, `null`},
	} {
		resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(c.body, "s", mcpwire.V20251125))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, fmt.Sprintf(`"code":%d`, c.code)) ||
			!strings.Contains(body, `"id":`+c.id) || resp.Header.Get(tunnel.HeaderNode) != "" {
			t.Errorf("%s: %d %s", c.body, resp.StatusCode, body)
		}
	}
	if n := len(up.Requests()); n != 0 {
		t.Fatalf("the server received %d refused messages", n)
	}
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	up.Lock()
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		if strings.Contains(string(r.Params), "sse") {
			return mcptest.Reply{Events: []string{
				"id: ok\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"fine\"}}\n\n",
				"id: bad\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"METHOD\":\"roots/list\"}\n\n",
			}, Result: json.RawMessage(`{"content":[],"isError":false}`)}
		}
		return mcptest.Reply{Raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":true,"ISERROR":false}}`)}
	}
	up.Unlock()
	resp, body, _ := pp.raw(t, mcpPreamble("web"), mcpPost(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, `-32603`) || !strings.Contains(body, `"id":1`) {
		t.Errorf("an ambiguous JSON response: %d %s", resp.StatusCode, body)
	}
	resp, body, _ = pp.raw(t, mcpPreamble("web"), mcpPost(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sse"}}`, sid, mcpwire.V20251125))
	if evs := sseEvents(body); resp.StatusCode != http.StatusOK || len(evs) != 1 || evs[0][0] != "id: ok" {
		t.Errorf("an ambiguous event: %d %q", resp.StatusCode, evs)
	}
}

// The gate's "/" reaches the MCP target's endpoint path exactly; a GET stream as well.
func TestMCPURLEndpoint(t *testing.T) {
	rec := newRecorder(t)
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		return true
	})
	for _, c := range []struct{ base, want string }{{"/mcp", "/mcp"}, {"/mcp/", "/mcp/"}, {"/", "/"}} {
		pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: withBase(rec.url(), c.base)})
		resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "s", mcpwire.V20251125))
		if got := rec.last(t); resp.StatusCode != http.StatusOK || got.uri != c.want || got.host != rec.url().Host {
			t.Errorf("base %q: %d, the target saw %q (Host %q)", c.base, resp.StatusCode, got.uri, got.host)
		}
	}
}

// The SSE filter: lines ending \n, \r\n or \r; events without data and comments pass as they came; a rewritten
// event keeps its other lines; an unterminated last event is dropped.
func TestSSEFilter(t *testing.T) {
	in := ": comment\n\nid: 1\r\ndata: {\"keep\":1}\r\n\r\nid: 2\rretry: 5\rdata: {\"drop\":1}\r\rid: 3\nevent: message\ndata: {\"rewrite\":\ndata: 1}\n\n" +
		"id: 4\ndata: \n\nid: 5\ndata: {\"keep\":2}"
	f := &sseFilter{src: bufio.NewReader(strings.NewReader(in)), body: io.NopCloser(nil), max: 1 << 20,
		filter: func(data []byte) ([]byte, bool, bool) {
			switch {
			case bytes.Contains(data, []byte("drop")):
				return nil, false, true
			case bytes.Contains(data, []byte("rewrite")):
				return []byte(`{"rewritten": true}`), true, true
			}
			return data, true, true
		}}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	want := ": comment\n\nid: 1\r\ndata: {\"keep\":1}\r\n\r\nid: 3\nevent: message\ndata: {\"rewritten\":true}\n\nid: 4\ndata: \n\n"
	if string(out) != want {
		t.Errorf("filtered:\n%q\nwant\n%q", out, want)
	}
	// A refused event ends the stream before it.
	f = &sseFilter{src: bufio.NewReader(strings.NewReader("id: a\ndata: {}\n\nid: b\ndata: {}\n\n")), body: io.NopCloser(nil), max: 1 << 20,
		filter: func(data []byte) ([]byte, bool, bool) { return nil, false, false }}
	if out, _ := io.ReadAll(f); len(out) != 0 {
		t.Errorf("after a refusal: %q", out)
	}
	// An event over the bound ends the stream.
	f = &sseFilter{src: bufio.NewReader(strings.NewReader("data: " + strings.Repeat("x", 100) + "\n\n")), body: io.NopCloser(nil), max: 50,
		filter: func(data []byte) ([]byte, bool, bool) { return data, true, true }}
	if out, err := io.ReadAll(f); len(out) != 0 || err == nil {
		t.Errorf("an event over the bound: %q %v", out, err)
	}
}

// stdioServer builds internal/stdiobridge's Go stdio MCP server double.
func stdioServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stdioserver")
	if out, err := exec.Command("go", "build", "-o", bin, "../stdiobridge/testdata/stdioserver").CombinedOutput(); err != nil {
		t.Fatalf("build the stdio double: %v\n%s", err, out)
	}
	return bin
}

// A stdio service through the proxy (§10): the bridge answers an initialize and a tools/call on the tunnel stream,
// keeps its own Kontra-Node answers (the 2nd initialize over --max-sessions 1 is 503 full), and a session's children
// stop when its tunnel session closes (release).
func TestProxyStdioService(t *testing.T) {
	bin := stdioServer(t)
	rec := t.TempDir()
	pp := newMCPProxyPair(t, MCPOptions{MaxSessions: 1},
		Serve{Kind: tunnel.KindMCP, Name: "fs", Command: []string{bin, "-record", rec}})
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	resp, body, _ := pp.raw(t, mcpPreamble("fs"), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	if resp.StatusCode != http.StatusOK || sid == "" || !strings.Contains(body, `"stdioserver"`) {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	resp, body, _ = pp.raw(t, mcpPreamble("fs"), mcpPost(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"through the tunnel"}}}`, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(body, "through the tunnel") {
		t.Fatalf("tools/call: %d %s", resp.StatusCode, body)
	}
	resp, _, _ = pp.raw(t, mcpPreamble("fs"), mcpPost(init, "", ""))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Errorf("over --max-sessions: %d %q", resp.StatusCode, resp.Header.Get(tunnel.HeaderNode))
	}
	// The session's tunnel session closes: its child stops, and its session is gone.
	pp.p.release()
	resp, _, _ = pp.raw(t, mcpPreamble("fs"), mcpPost(`{"jsonrpc":"2.0","id":2,"method":"ping"}`, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after release: %d", resp.StatusCode)
	}
	waitFor(t, 15*time.Second, "the released child's slot", func() bool { return pp.p.bridges["fs"].Sessions() == 0 })
	// Kind and service checks still come first.
	pre := mcpPreamble("fs")
	pre.Kind = tunnel.KindA2A
	if resp, _, _ := pp.raw(t, pre, mcpPost(init, "", "")); resp.StatusCode != http.StatusNotFound || resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeKindMismatch) {
		t.Errorf("kind a2a: %d %q", resp.StatusCode, resp.Header.Get(tunnel.HeaderNode))
	}
}

// Content coding on a URL service (M3 WP-05 review; tunnel.md §8.2): the gate's tunnel transport asks for gzip on
// every exchange, but the node reads the target's MCP messages (roots), so it asks the target for identity
// encoding: a target that compresses when asked answers plainly, its JSON response arrives whole and its event
// stream is filtered (the roots/list removed and answered by the node). A target that compresses unasked is
// refused 502 with -32603, JSON or event stream, rather than relayed unread.
func TestMCPURLContentCoding(t *testing.T) {
	var mu sync.Mutex
	var sawAE []string
	var answers [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sawAE = append(sawAE, r.Header.Get("Accept-Encoding"))
		if bytes.Contains(b, []byte(`"result"`)) {
			answers = append(answers, b)
		}
		mu.Unlock()
		if bytes.Contains(b, []byte(`"result"`)) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		sse := bytes.Contains(b, []byte(`"name":"sse`))
		var out io.Writer = w
		var zw *gzip.Writer
		if bytes.Contains(b, []byte("unasked")) || strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw = gzip.NewWriter(w)
			out = zw
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(out, "id: e1\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"r1\",\"method\":\"roots/list\"}\n\n")
			fmt.Fprint(out, "id: e2\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[],\"isError\":false}}\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(out, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}],"isError":false}}`)
		}
		if zw != nil {
			_ = zw.Close()
		}
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{Roots: map[string]*mcproots.Roots{"web": mcproots.New(t.TempDir())}},
		Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	post := func(body string) (*http.Response, string) {
		req := mcpPost(body, "up-1", mcpwire.V20251125)
		req = strings.Replace(req, "\r\nContent-Type:", "\r\nAccept-Encoding: gzip\r\nContent-Type:", 1)
		resp, out, _ := pp.raw(t, mcpPreamble("web"), req)
		return resp, out
	}

	resp, body := post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sse","arguments":{}}}`)
	evs := sseEvents(body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || len(evs) != 1 || evs[0][0] != "id: e2" {
		t.Errorf("an event stream: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	waitFor(t, 10*time.Second, "the node's answer to r1", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(answers) == 1
	})
	resp, body = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"json","arguments":{}}}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || !strings.Contains(body, `"text":"ok"`) {
		t.Errorf("a JSON response: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	mu.Lock()
	for _, ae := range sawAE {
		if strings.Contains(ae, "gzip") {
			t.Errorf("the target was asked for %q", ae)
		}
	}
	mu.Unlock()

	for _, c := range []struct{ name, body string }{
		{"json", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"json unasked","arguments":{}}}`},
		{"sse", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sse unasked","arguments":{}}}`},
	} {
		resp, body := post(c.body)
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Content-Encoding") != "" || !strings.Contains(body, `-32603`) ||
			!strings.Contains(body, `"id":3`) || strings.Contains(body, "roots/list") {
			t.Errorf("%s compressed unasked: %d %q %q", c.name, resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
		}
	}
}

// The SSE filter never waits for the byte after a \r (M3 WP-05 review): an event whose lines end with a lone \r is
// relayed as soon as its blank line arrives; a \r\n split across reads stays \r\n, kept with a kept event and
// dropped with a removed one.
func TestSSEFilterLoneCR(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	f := &sseFilter{src: bufio.NewReader(pr), body: pr, max: 1 << 20,
		filter: func(data []byte) ([]byte, bool, bool) {
			if bytes.Contains(data, []byte("drop")) {
				return nil, false, true
			}
			return data, true, true
		}}
	got := make(chan string, 16)
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
			if n > 0 {
				got <- string(b[:n])
			}
			if err != nil {
				close(got)
				return
			}
		}
	}()
	read := func(want string) {
		t.Helper()
		var have string
		deadline := time.After(2 * time.Second)
		for have != want {
			select {
			case s, ok := <-got:
				if !ok {
					t.Fatalf("the stream ended after %q, want %q", have, want)
				}
				have += s
			case <-deadline:
				t.Fatalf("relayed %q within 2 s, want %q", have, want)
			}
		}
	}
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(pw, s); err != nil {
			t.Fatal(err)
		}
	}
	write("id: 1\rdata: {\"n\":1}\r\r")
	read("id: 1\rdata: {\"n\":1}\r\r")
	// A \r\n whose \n comes in a later read: the event goes when its blank line's \r arrives, its \n after it.
	write("id: 2\r")
	write("\ndata: {\"n\":2}\r\n\r")
	read("id: 2\r\ndata: {\"n\":2}\r\n\r")
	write("\n")
	read("\n")
	// A removed event's split \r\n goes with it; the next event passes whole.
	write("id: 3\r\ndata: {\"drop\":3}\r\n\r")
	write("\nid: 4\r\ndata: {\"n\":4}\r\n\r\n")
	read("id: 4\r\ndata: {\"n\":4}\r\n\r\n")
	_ = pw.Close()
	if s, ok := <-got; ok {
		t.Errorf("after the end: %q", s)
	}
}

// A revoked binding stops the service's stdio children (M3 WP-05 review; tunnel.md §12): Drop on a session's proxy
// stops the children that session opened for the service, with D184's sequence, and leaves another session's.
func TestProxyDropStopsStdioChildren(t *testing.T) {
	bin := stdioServer(t)
	rec := t.TempDir()
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "fs", Command: []string{bin, "-record", rec}})
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("fs"), mcpPost(init, "", ""))
	if resp.StatusCode != http.StatusOK || resp.Header.Get(mcpwire.HeaderSessionID) == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	b := pp.p.bridges["fs"]
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.ServeHTTP(w, r.WithContext(stdiobridge.WithOwner(r.Context(), "another tunnel session")))
	}))
	t.Cleanup(other.Close)
	req, _ := http.NewRequest(http.MethodPost, other.URL+"/", strings.NewReader(init))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	oresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = oresp.Body.Close()
	if oresp.StatusCode != http.StatusOK || b.Sessions() != 2 {
		t.Fatalf("another session's initialize: %d, %d children", oresp.StatusCode, b.Sessions())
	}
	pp.p.Drop("fs")
	waitFor(t, 15*time.Second, "the dropped service's child stopped", func() bool { return b.Sessions() == 1 })
	if resp, _, _ := pp.raw(t, mcpPreamble("fs"), mcpPost(init, "", "")); resp.StatusCode != http.StatusNotFound ||
		resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeNotHosted) {
		t.Errorf("after Drop: %d %q", resp.StatusCode, resp.Header.Get(tunnel.HeaderNode))
	}
	time.Sleep(200 * time.Millisecond)
	if n := b.Sessions(); n != 1 {
		t.Errorf("another session's child: %d children left, want 1", n)
	}
}
