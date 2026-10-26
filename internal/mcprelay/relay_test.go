package mcprelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/tunnel"
)

// upstreamDouble is a mcptest double with an echo tool, on its own test server.
func upstreamDouble(t *testing.T) (*mcptest.Server, string) {
	t.Helper()
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return up, srv.URL + "/mcp"
}

func lastRequest(t *testing.T, up *mcptest.Server, method string) mcptest.Request {
	t.Helper()
	rs := up.Requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Method == method {
			return rs[i]
		}
	}
	t.Fatalf("the upstream received no %s", method)
	return mcptest.Request{}
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// §11.2: initialize returns a 22-character Mcp-Session-Id that differs from the upstream's, the row stores its
// sha256 bound to the principal, and later calls carry the upstream's id upstream; the client's Authorization never
// reaches the upstream, and the upstream's headers other than Content-Type and Cache-Control never reach the client.
// Direct backend and tunnel backend (through a FakeNode), whose preambles carry mcp_session and mcp_request_id.
func TestSessions(t *testing.T) {
	e := newEnv(t)
	direct, url := upstreamDouble(t)
	e.service("direct", url, nil)
	e.service("tun", "", nil)
	r := e.replica("a", replicaOpts{})
	tun := mcptest.New()
	tun.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(r, tun, "tun")
	key, principal := e.apiKey("buyer")

	for _, tc := range []struct {
		name string
		up   *mcptest.Server
	}{{"direct", direct}, {"tun", tun}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, key)
			rep := c.initialize(r.url + "/mcp/" + tc.name)
			if len(c.sid) != 22 {
				t.Errorf("client id %q has %d characters", c.sid, len(c.sid))
			}
			ups := tc.up.Sessions()
			if len(ups) != 1 || ups[0] == c.sid {
				t.Fatalf("upstream sessions %v, client %s", ups, c.sid)
			}
			if rep.header.Get("X-Upstream-Secret") != "" {
				t.Error("an upstream header other than Content-Type and Cache-Control reached the client")
			}
			row := e.row(c.sid)
			if row.upstreamSID != ups[0] || row.closed || row.version != "2025-11-25" {
				t.Errorf("row %+v", row)
			}
			var stored string
			h := HashID(c.sid)
			if err := e.pool.QueryRow(context.Background(), `select principal from kontra.mcp_sessions where client_sid_hash = $1`, h[:]).Scan(&stored); err != nil || stored != principal {
				t.Errorf("principal %q %v", stored, err)
			}
			if tc.name == "direct" && (row.replica != nil || row.tunnel != nil) {
				t.Errorf("a direct backend's row names a replica or a tunnel: %+v", row)
			}
			if tc.name == "tun" && (row.replica == nil || *row.replica != r.id || row.tunnel == nil || *row.tunnel != n.session.ID) {
				t.Errorf("a tunnel row: %+v", row)
			}
			rep = c.call(r.url+"/mcp/"+tc.name, 1, "echo", "hello")
			res, _ := rep.msg(t)["result"].(map[string]any)
			if rep.status != 200 || res == nil {
				t.Fatalf("call: %d %s", rep.status, rep.body)
			}
			got := lastRequest(t, tc.up, "tools/call")
			if got.SessionID != ups[0] || got.Header.Get("Authorization") != "" {
				t.Errorf("upstream saw session %q, Authorization %q", got.SessionID, got.Header.Get("Authorization"))
			}
			if rep.header.Get("Mcp-Session-Id") != c.sid {
				t.Errorf("Mcp-Session-Id %q", rep.header.Get("Mcp-Session-Id"))
			}
		})
	}
	// The tunnel's preambles: the service, kind mcp, the client session's hash, and the call's id on tools/call.
	var sawCall bool
	for _, p := range n.fake.Preambles() {
		if p.Service != "tun" || p.Kind != tunnel.KindMCP || len(p.MCPSession) != 64 {
			t.Errorf("preamble %+v", p)
		}
		if p.MCPRequestID == "1" {
			sawCall = true
		}
	}
	if !sawCall {
		t.Errorf("no preamble carried the tools/call id: %+v", n.fake.Preambles())
	}
}

// mcp_session_cross_node (§11.2): the id on another service's path, the id presented by another principal, and the
// id after DELETE are 404, and no stream is opened for any of them.
func TestSession404s(t *testing.T) {
	e := newEnv(t)
	direct, url := upstreamDouble(t)
	e.service("direct", url, nil)
	e.service("tun", "", nil)
	r := e.replica("a", replicaOpts{})
	tun := mcptest.New()
	tun.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(r, tun, "tun")
	key, _ := e.apiKey("buyer")
	other, _ := e.apiKey("other")

	c := newClient(t, key)
	c.initialize(r.url + "/mcp/tun")
	sid := c.sid
	streams, seen := len(n.fake.Preambles()), len(direct.Requests())+len(tun.Requests())
	quiet := func(what string) {
		t.Helper()
		if len(n.fake.Preambles()) != streams || len(direct.Requests())+len(tun.Requests()) != seen {
			t.Errorf("%s opened a stream or reached an upstream", what)
		}
	}
	if rep := c.call(r.url+"/mcp/direct", 1, "echo", "x"); rep.status != 404 {
		t.Errorf("another service's path: %d %s", rep.status, rep.body)
	}
	quiet("another service's path")
	o := newClient(t, other)
	o.sid = sid
	if rep := o.call(r.url+"/mcp/tun", 2, "echo", "x"); rep.status != 404 {
		t.Errorf("another principal: %d %s", rep.status, rep.body)
	}
	if rep := o.do(http.MethodGet, r.url+"/mcp/tun", "", nil); rep.status != 404 {
		t.Errorf("another principal's GET: %d", rep.status)
	}
	quiet("another principal")
	if e.row(sid).closed {
		t.Fatal("another principal's request closed the session")
	}
	if rep := c.do(http.MethodDelete, r.url+"/mcp/tun", "", nil); rep.status != 200 {
		t.Fatalf("DELETE: %d", rep.status)
	}
	if tun.Count("") == 0 || lastRequest(t, tun, "").HTTPMethod != http.MethodDelete {
		t.Error("the DELETE was not forwarded")
	}
	if row := e.row(sid); !row.closed || *row.reason != string(CloseDeleted) {
		t.Errorf("after DELETE: %+v", row)
	}
	streams, seen = len(n.fake.Preambles()), len(direct.Requests())+len(tun.Requests())
	if rep := c.call(r.url+"/mcp/tun", 3, "echo", "x"); rep.status != 404 || rep.errCode(t) != codeNoSession {
		t.Errorf("after DELETE: %d %s", rep.status, rep.body)
	}
	if rep := c.do(http.MethodGet, r.url+"/mcp/tun", "", nil); rep.status != 404 {
		t.Errorf("GET after DELETE: %d", rep.status)
	}
	quiet("a deleted session")
	c.sid = "AAAAAAAAAAAAAAAAAAAAAA"
	if rep := c.call(r.url+"/mcp/tun", 4, "echo", "x"); rep.status != 404 {
		t.Errorf("an unknown id: %d", rep.status)
	}
	quiet("an unknown id")
}

// §11.2, tunnel.md §11: closing the tunnel session makes the next request 404, closes the row tunnel_lost, and opens
// no stream.
func TestTunnelLoss(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	r := e.replica("a", replicaOpts{})
	tun := mcptest.New()
	tun.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(r, tun, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/tun")
	if rep := c.call(r.url+"/mcp/tun", 1, "echo", "x"); rep.status != 200 {
		t.Fatalf("call: %d %s", rep.status, rep.body)
	}
	if err := n.session.Close("test: the node went away"); err != nil {
		t.Fatal(err)
	}
	streams := len(n.fake.Preambles())
	rep := c.call(r.url+"/mcp/tun", 2, "echo", "x")
	if rep.status != 404 {
		t.Errorf("after the tunnel closed: %d %s", rep.status, rep.body)
	}
	if len(n.fake.Preambles()) != streams {
		t.Error("a stream was opened on a closed session")
	}
	eventually(t, 5*time.Second, "the row closed tunnel_lost", func() bool {
		row := e.row(c.sid)
		return row.closed && *row.reason == string(CloseTunnelLost)
	})
}

// mcp_session_revoked (§6.1, §11.2, tunnel.md §12): revoking the binding closes the pinned row revoked, even with a
// node that ignores the control stream (a FakeNode never reads it), and the next request is 404 and opens no
// stream. A key revocation closes revoked too.
func TestRevocationClosesRevoked(t *testing.T) {
	for _, what := range []string{"binding", "key"} {
		t.Run(what, func(t *testing.T) {
			e := newEnv(t)
			e.service("tun", "", nil)
			r := e.replica("a", replicaOpts{})
			tun := mcptest.New()
			tun.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
			n := e.node(r, tun, "tun")
			key, _ := e.apiKey("buyer")
			c := newClient(t, key)
			c.initialize(r.url + "/mcp/tun")
			var err error
			if what == "binding" {
				err = e.store.RevokeBinding(context.Background(), n.key, "tun", "test")
			} else {
				err = e.store.RevokeKey(context.Background(), n.key, "operator", "test")
			}
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, 10*time.Second, "the row closed revoked", func() bool {
				row := e.row(c.sid)
				return row.closed && *row.reason == string(CloseRevoked)
			})
			streams := len(n.fake.Preambles())
			if rep := c.call(r.url+"/mcp/tun", 1, "echo", "x"); rep.status != 404 {
				t.Errorf("after the revocation: %d %s", rep.status, rep.body)
			}
			if len(n.fake.Preambles()) != streams {
				t.Error("a stream was opened after the revocation")
			}
		})
	}
}

// The relay's own check refuses a revoked binding (the stream check reads the revocation view) before any registry
// statement has closed the row: the row still closes revoked (tunnelgate.ErrRevoked), not tunnel_lost.
func TestRevokedBeforeTheRegistryCloses(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	r := e.replica("a", replicaOpts{unsubscribed: true})
	tun := mcptest.New()
	n := e.node(r, tun, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/tun")
	if err := e.store.RevokeBinding(context.Background(), n.key, "tun", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the revocation view holds the binding revoked", func() bool { return r.rev.BindingRevoked(n.key, "tun") })
	if row := e.row(c.sid); row.closed {
		t.Fatalf("the row closed without the registry: %+v", row)
	}
	streams := len(n.fake.Preambles())
	if rep := c.call(r.url+"/mcp/tun", 1, "echo", "x"); rep.status != 404 {
		t.Errorf("%d %s", rep.status, rep.body)
	}
	if len(n.fake.Preambles()) != streams {
		t.Error("a stream was opened for a revoked binding")
	}
	if row := e.row(c.sid); !row.closed || *row.reason != string(CloseRevoked) {
		t.Errorf("row %+v", row)
	}
}

// mcp_batch_refused (§11.1): [call, call] is 400 with an id-less -32600, and the upstream sees nothing.
func TestBatchRefused(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("direct", url, func(s *Service) { s.Tools = Policy{Deny: []string{"add"}} })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/direct")
	seen := len(up.Requests())
	for _, body := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add"}}]`,
		`[]`, `"tools/call"`, `42`,
	} {
		rep := c.do(http.MethodPost, r.url+"/mcp/direct", body, nil)
		m := rep.msg(t)
		e, _ := m["error"].(map[string]any)
		if rep.status != 400 || e == nil || e["code"].(float64) != -32600 || e["message"] != "batch not supported" || m["id"] != nil {
			t.Errorf("%s: %d %s", body, rep.status, rep.body)
		}
	}
	if rep := c.do(http.MethodPost, r.url+"/mcp/direct", `{"jsonrpc":"2.0","id":1,`, nil); rep.status != 400 || rep.errCode(t) != -32700 {
		t.Errorf("not JSON: %d %s", rep.status, rep.body)
	}
	if len(up.Requests()) != seen || len(e.calls("direct")) != 0 {
		t.Error("a refused body reached the upstream or the meter")
	}
}

// §11.1, §11.12: the legacy GET probe without a session is 405; a POST that is not initialize without a session
// is the legacy 400 (-32000); an unsupported version is 400 with -32022. None reaches the upstream.
func TestLegacyRules(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("direct", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	if rep := c.do(http.MethodGet, r.url+"/mcp/direct", "", nil); rep.status != 405 {
		t.Errorf("GET probe: %d", rep.status)
	}
	if rep := c.do(http.MethodDelete, r.url+"/mcp/direct", "", nil); rep.status != 405 {
		t.Errorf("DELETE without a session: %d", rep.status)
	}
	rep := c.do(http.MethodPost, r.url+"/mcp/direct", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if rep.status != 400 || string(rep.body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}` {
		t.Errorf("no session: %d %s", rep.status, rep.body)
	}
	c.initialize(r.url + "/mcp/direct")
	seen := len(up.Requests())
	rep = c.do(http.MethodPost, r.url+"/mcp/direct", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "2025-03-26"})
	if rep.status != 400 || rep.errCode(t) != -32022 {
		t.Errorf("2025-03-26: %d %s", rep.status, rep.body)
	}
	// A 2026-07-28 request is answered as a legacy-only server answers it until M3 WP-07 serves the modern era.
	rep = c.do(http.MethodPost, r.url+"/mcp/direct", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"})
	if rep.status != 400 || rep.errCode(t) != -32000 {
		t.Errorf("modern: %d %s", rep.status, rep.body)
	}
	if len(up.Requests()) != seen {
		t.Error("a refused request reached the upstream")
	}
	// No credential: 401 with the gate's WWW-Authenticate; an unknown service and one the principal is not allowed: 404.
	anon := newClient(t, "")
	if rep := anon.do(http.MethodPost, r.url+"/mcp/direct", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil); rep.status != 404 {
		t.Errorf("anonymous on a service that does not allow it: %d", rep.status)
	}
	bad := newClient(t, "kk_nope_00000000000000000000000000000000")
	if rep := bad.do(http.MethodPost, r.url+"/mcp/nothing", `{}`, nil); rep.status != 401 || rep.header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("bad credential: %d %v", rep.status, rep.header)
	}
	if rep := c.do(http.MethodPost, r.url+"/mcp/nothing", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil); rep.status != 404 || rep.errCode(t) != -32601 {
		t.Errorf("unknown service: %d %s", rep.status, rep.body)
	}
}
