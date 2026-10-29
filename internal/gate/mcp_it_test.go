package gate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// dialMCPNode connects a node hosting the MCP service fs to the gate at base, serving its streams with h (a
// mcptest.FakeNode, which ignores the control stream).
func dialMCPNode(t *testing.T, base string, tokens []string, h http.Handler) (*mcptest.FakeNode, uuid.UUID) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := node.Dialer{UserAgent: node.UserAgent("test")}.Dial(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	hello := &tunnel.Hello{Name: "mcp-node", Agent: node.UserAgent("test"), Enroll: tokens,
		Services: []tunnel.Service{{Kind: tunnel.KindMCP, Name: "fs"}}}
	w, err := node.Handshake(ctx, d, newNodeIdentity(t), hello, false)
	if err != nil {
		_ = d.WS.CloseNow()
		t.Fatal(err)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	fake := &mcptest.FakeNode{Handler: h}
	go fake.Serve(m)
	return fake, uuid.MustParse(w.Session)
}

// mcpPost sends one MCP POST with the API key and the session id, and returns the status, the headers and the body.
func mcpPost(t *testing.T, endpoint, key, sid, body string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// /mcp/{svc} through two whole replicas (M3 WP-04): a direct mcp: {url} service served by either; a tunnel service
// whose node is connected to B, reached by a client on A through B's peer listener (relay §11.2, D163), its row
// naming B; both replicas advertise their peer address in gate_replicas.peer_addr; the policy of the services:
// block applies; the calls rows are written.
func TestMCPThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	t.Setenv("KONTRA_TEST_PEER_SECRET", "0123456789abcdef0123456789abcdef-test")
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", ""), mcptest.Tool("get-env", "")}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	nodeUp := mcptest.New()
	nodeUp.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	nodeUp.SSE = true
	services := `peer: { listen: "127.0.0.1:0", secret_env: KONTRA_TEST_PEER_SECRET }
services:
  web:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["org:default"]
    tools: { deny: ["get-env"] }
  fs:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	for _, run := range []*running{a, b} {
		if run.r.PeerAddr() == "" {
			t.Fatalf("replica %s has no peer address", run.r.ID())
		}
		eventually(t, 5*time.Second, "peer_addr written", func() bool {
			return scalar[string](t, e.pool, `select coalesce(peer_addr, '') from kontra.gate_replicas where replica_id = $1`, run.r.ID()) == run.r.PeerAddr()
		})
	}
	key := e.mintKey("buyer")
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`

	// The direct service, from either replica.
	for _, run := range []*running{a, b} {
		st, h, body := mcpPost(t, run.url+"/mcp/web", key, "", init)
		sid := h.Get("Mcp-Session-Id")
		if st != 200 || len(sid) != 22 {
			t.Fatalf("initialize web on %s: %d %s", run.r.ID(), st, body)
		}
		if st, _, body := mcpPost(t, run.url+"/mcp/web", key, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get-env"}}`); st != 200 ||
			!strings.Contains(body, "Unknown tool: get-env") {
			t.Errorf("a denied tool: %d %s", st, body)
		}
		if st, _, body := mcpPost(t, run.url+"/mcp/web", key, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`); st != 200 ||
			!strings.Contains(body, `"hi"`) {
			t.Errorf("echo: %d %s", st, body)
		}
	}
	if up.Calls("get-env") != 0 || up.Calls("echo") != 2 {
		t.Errorf("upstream calls: get-env %d, echo %d", up.Calls("get-env"), up.Calls("echo"))
	}

	// The tunnel service: the node on B, the client on A.
	team, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(ctx, team, "fs", "test")
	if err != nil {
		t.Fatal(err)
	}
	fake, session := dialMCPNode(t, b.url, []string{tok}, nodeUp)
	eventually(t, 15*time.Second, "the node's tunnels row on B", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.tunnels where session_id = $1 and replica_id = $2`, session, b.r.ID()) == 1
	})
	st, h, body := mcpPost(t, a.url+"/mcp/fs", key, "", init)
	sid := h.Get("Mcp-Session-Id")
	if st != 200 || sid == "" || !strings.Contains(body, "protocolVersion") {
		t.Fatalf("initialize fs on A: %d %s", st, body)
	}
	if got := scalar[string](t, e.pool, `select replica_id from kontra.mcp_sessions where tunnel_session = $1`, session); got != b.r.ID() {
		t.Errorf("replica_id %q, want the holder %s", got, b.r.ID())
	}
	if st, _, body := mcpPost(t, a.url+"/mcp/fs", key, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"via B"}}}`); st != 200 ||
		!strings.Contains(body, "via B") {
		t.Errorf("a call through the hop: %d %s", st, body)
	}
	if len(fake.Preambles()) == 0 || fake.Preambles()[0].Kind != tunnel.KindMCP {
		t.Errorf("preambles %+v", fake.Preambles())
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.calls`); n != 3 {
		t.Errorf("%d calls rows, want 3", n)
	}
}
