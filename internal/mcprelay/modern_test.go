package mcprelay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
)

// The 2026-07-28 era at package level (spec/relay.md §11.12, D158–D162, D176–D180; M3 WP-07): replicas over the
// shared Postgres, the dual-era double (mcptest's modern.go) and the legacy-only one, direct and through tunnel
// sessions; the gate-owned modern tasks with Temporal (tasks_pg_test.go's taskEnv).

// modernMetaObj is a 2026-07-28 request's per-request fields, declaring the tasks extension when ext.
func modernMetaObj(ext bool) string {
	caps := `{"elicitation":{}}`
	if ext {
		caps = `{"elicitation":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`
	}
	return `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":` + caps +
		`,"io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"}}`
}

// modernMeta is the _meta member of modernMetaObj.
func modernMeta(ext bool) string { return `"_meta":` + modernMetaObj(ext) }

// modernClient speaks 2026-07-28 to a replica as a dual-era client does: no session, every request with its _meta
// and its headers.
type modernClient struct {
	t    *testing.T
	key  string
	url  string
	ext  bool
	seen *seen
}

func newModern(t *testing.T, key, url string, ext bool) *modernClient {
	return &modernClient{t: t, key: key, url: url, ext: ext, seen: &seen{}}
}

func (c *modernClient) body(id any, method string, params map[string]any) string {
	p := map[string]any{"_meta": json.RawMessage(modernMetaObj(c.ext))}
	for k, v := range params {
		p[k] = v
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p})
	return string(b)
}

// req sends a request with the headers a conforming client writes (Mcp-Name from the body).
func (c *modernClient) req(id any, method string, params map[string]any) reply {
	c.t.Helper()
	hdr := map[string]string{"MCP-Protocol-Version": mcpwire.V20260728, "Mcp-Method": method}
	if src, ok := mcpwire.NameSource(method); ok {
		if v, ok := params[src].(string); ok {
			hdr["Mcp-Name"] = mcpwire.EncodeHeaderValue(v)
		}
	}
	return c.raw(c.body(id, method, params), hdr)
}

func (c *modernClient) raw(body string, hdr map[string]string) reply {
	c.t.Helper()
	r := newClient(c.t, c.key).do(http.MethodPost, c.url, body, hdr)
	c.seen.add(r.body)
	return r
}

func (c *modernClient) at(url string) *modernClient {
	return &modernClient{t: c.t, key: c.key, url: url, ext: c.ext, seen: c.seen}
}

// result is a reply's JSON-RPC result, failing without one.
func result(t *testing.T, r reply) map[string]any {
	t.Helper()
	res, ok := r.msg(t)["result"].(map[string]any)
	if !ok || r.status != http.StatusOK {
		t.Fatalf("no result: %d %s", r.status, r.body)
	}
	return res
}

// rpcError is a reply's JSON-RPC error.
func rpcError(t *testing.T, r reply) (int, map[string]any) {
	t.Helper()
	e, _ := r.msg(t)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error in %d %s", r.status, r.body)
	}
	data, _ := e["data"].(map[string]any)
	return int(e["code"].(float64)), data
}

// dualEra is the dual-era double serving tools, on 127.0.0.1.
func dualEra(t *testing.T, tools ...json.RawMessage) (*mcptest.Server, string) {
	t.Helper()
	up := mcptest.New()
	up.Modern = true
	up.Tools = tools
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return up, srv.URL + "/mcp"
}

// virtualRow is an open virtual mcp_sessions row.
type virtualRow struct {
	hash      []byte
	principal string
	pinned    bool // an upstream session, a tunnel session or a replica named
}

func (e *env) virtualRows(service string) []virtualRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `select client_sid_hash, principal,
		upstream_sid is not null or tunnel_session is not null or replica_id is not null
		from kontra.mcp_sessions where service = $1 and protocol_version = '2026-07-28' and closed_at is null order by principal`, service)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []virtualRow
	for rows.Next() {
		var v virtualRow
		if err := rows.Scan(&v.hash, &v.principal, &v.pinned); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (e *env) sessionRows(service string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.mcp_sessions where service = $1`, service).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// The classification table (§11.12 rules 1–5, D158) end to end: initialize under any header is legacy; 2026-07-28
// is modern and its Mcp-Session-Id is never looked up (a bogus one is no 404, and no session id is minted); a legacy
// or absent header without a session is rule 3's 400; another version is -32022 listing the three; GET and DELETE
// without a session are 405.
func TestModernClassification(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	u := r.url + "/mcp/svc"

	c := newClient(t, key)
	rep := c.do(http.MethodPost, u, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"})
	if rep.status != 200 || rep.header.Get("Mcp-Session-Id") == "" || result(t, rep)["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize under 2026-07-28: %d %v %s", rep.status, rep.header, rep.body)
	}

	m := newModern(t, key, u, false)
	rep = m.raw(m.body(1, "tools/list", nil), map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list",
		"Mcp-Session-Id": "bogus-session-id"})
	if rep.status != 200 || rep.header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("a modern request with a bogus session id: %d %v %s", rep.status, rep.header, rep.body)
	}
	if tools, _ := result(t, rep)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools/list: %s", rep.body)
	}
	for _, r := range up.ModernRequests() {
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("a modern request reached the upstream with a session id: %v", r.Header)
		}
	}

	for _, v := range []string{"2025-11-25", "2025-06-18", ""} {
		rep = newClient(t, key).do(http.MethodPost, u, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": v})
		if rep.status != 400 || string(rep.body) != string(mcpwire.LegacyNoSessionBody) {
			t.Errorf("header %q without a session: %d %s", v, rep.status, rep.body)
		}
	}
	rep = newClient(t, key).do(http.MethodPost, u, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	code, data := rpcError(t, rep)
	if rep.status != 400 || code != -32022 || !reflect.DeepEqual(data["supported"], []any{"2026-07-28", "2025-11-25", "2025-06-18"}) {
		t.Errorf("1999-01-01: %d %s", rep.status, rep.body)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if rep := newClient(t, key).do(method, u, "", nil); rep.status != 405 {
			t.Errorf("%s without a session: %d", method, rep.status)
		}
	}
}

// §11.12's checks (D176) end to end: a mismatched Mcp-Name, a _meta version other than the header, a missing
// protocolVersion or clientCapabilities, a response and a notification are answered at the gate; nothing reaches
// the upstream (not even the era's probe), nothing is metered, and no session row is written.
func TestModernHeaderChecks(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""), mcptest.Tool("add", ""))
	e.service("svc", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", false)
	call := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x"},%s}}`
	hdr := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "echo"}
	for _, tc := range []struct {
		name   string
		body   string
		hdr    map[string]string
		status int
		code   int
	}{
		{"Mcp-Name add, body name echo", fmt.Sprintf(call, modernMeta(false)),
			map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "add"}, 400, -32020},
		{"_meta 2025-11-25 under header 2026-07-28", fmt.Sprintf(call,
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}`), hdr, 400, -32020},
		{"no clientCapabilities", fmt.Sprintf(call, `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`), hdr, 400, -32602},
		{"no protocolVersion", fmt.Sprintf(call, `"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}`), hdr, 400, -32602},
		{"no Mcp-Method", fmt.Sprintf(call, modernMeta(false)),
			map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Name": "echo"}, 400, -32020},
		{"a response", `{"jsonrpc":"2.0","id":5,"result":{}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"}, 400, -32600},
		{"a notification", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{` + modernMeta(false) + `}}`,
			map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "notifications/cancelled"}, 202, 0},
	} {
		rep := m.raw(tc.body, tc.hdr)
		if rep.status != tc.status {
			t.Errorf("%s: %d %s, want %d", tc.name, rep.status, rep.body, tc.status)
			continue
		}
		if tc.code != 0 {
			if code, _ := rpcError(t, rep); code != tc.code || rep.msg(t)["id"] != float64(5) {
				t.Errorf("%s: %s, want %d with id 5", tc.name, rep.body, tc.code)
			}
		}
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("the upstream received %d requests", n)
	}
	if n := len(e.calls("svc")); n != 0 {
		t.Errorf("%d calls rows", n)
	}
	if n := e.sessionRows("svc"); n != 0 {
		t.Errorf("%d session rows", n)
	}
}

// Against the dual-era double (§11.12): server/discover lists the upstream's 2026-07-28 and the legacy versions,
// keeps no tasks extension where the gate owns no tasks, carries the operator's initialize_meta and no Kontra key of
// the upstream's, cacheScope private; tools/list is filtered and annotated, ttlMs capped at 600000, cacheScope
// private; requests reach the upstream with their headers, without a session, params.task or the tasks extension;
// subscriptions/listen's taskIds are -32021 without the extension and removed with it; tasks/result is 404 with
// -32601 and tasks/get without the extension -32021; one virtual session row per principal and service, no other
// row, named by the calls rows; no Mcp-Session-Id answered.
func TestModernDualEra(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""), mcptest.Tool("secret", ""), mcptest.Tool("priced", `{"sh.kontra/price":"9","vendor/x":1}`))
	up.InitMeta = json.RawMessage(`{"sh.kontra/escrow":"held"}`)
	up.ModernCapabilities = json.RawMessage(`{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{},"sh.kontra/x":{}},"experimental":{"sh.kontra/sealed":{}}}`)
	e.service("svc", url, func(s *Service) {
		s.Tools = Policy{Deny: []string{"secret"}}
		s.ToolMeta = map[string]map[string]json.RawMessage{"*": {"sh.kontra/price": json.RawMessage(`"1 USD"`)}}
		s.InitializeMeta = map[string]json.RawMessage{"sh.kontra/operator": json.RawMessage(`"op"`)}
	})
	r := e.replica("a", replicaOpts{})
	keyA, pA := e.apiKey("buyer-a")
	keyB, pB := e.apiKey("buyer-b")
	u := r.url + "/mcp/svc"
	a := newModern(t, keyA, u, true)

	rep := a.req(1, "server/discover", nil)
	d := result(t, rep)
	if !reflect.DeepEqual(d["supportedVersions"], []any{"2026-07-28", "2025-11-25", "2025-06-18"}) || d["cacheScope"] != "private" {
		t.Fatalf("server/discover: %s", rep.body)
	}
	caps, _ := d["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if _, ok := ext["io.modelcontextprotocol/tasks"]; ok || ext["sh.kontra/x"] != nil || caps["experimental"].(map[string]any)["sh.kontra/sealed"] != nil {
		t.Fatalf("the tasks extension or a Kontra key of the upstream's in the capabilities: %s", rep.body)
	}
	meta, _ := d["_meta"].(map[string]any)
	if meta["sh.kontra/operator"] != "op" || meta["sh.kontra/escrow"] != nil || meta["io.modelcontextprotocol/serverInfo"] == nil {
		t.Fatalf("server/discover's _meta: %s", rep.body)
	}
	if rep.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("a session id answered: %v", rep.header)
	}

	rep = a.req(2, "tools/list", nil)
	l := result(t, rep)
	tools, _ := l["tools"].([]any)
	var names []string
	for _, x := range tools {
		tl := x.(map[string]any)
		names = append(names, tl["name"].(string))
		if tm, _ := tl["_meta"].(map[string]any); tm["sh.kontra/price"] != "1 USD" {
			t.Errorf("tool_meta not applied to %v", tl)
		}
	}
	if strings.Join(names, ",") != "echo,priced" || l["cacheScope"] != "private" || l["ttlMs"] != float64(600000) ||
		l["nextCursor"] != nil || l["resultType"] != "complete" {
		t.Fatalf("tools/list: %s", rep.body)
	}

	rep = a.req(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hello"}, "task": map[string]any{"ttl": 1}})
	if res := result(t, rep); !strings.Contains(string(rep.body), "hello") || res["resultType"] != "complete" {
		t.Fatalf("tools/call: %s", rep.body)
	}
	if rep := a.req(4, "tools/call", map[string]any{"name": "secret", "arguments": map[string]any{}}); func() int { c, _ := rpcError(t, rep); return c }() != -32602 {
		t.Errorf("a denied tool: %s", rep.body)
	}

	// subscriptions/listen (D176).
	plain := newModern(t, keyA, u, false)
	rep = plain.req(5, "subscriptions/listen", map[string]any{"notifications": map[string]any{"taskIds": []string{"x"}}})
	if code, data := rpcError(t, rep); rep.status != 400 || code != -32021 || data["requiredCapabilities"] == nil {
		t.Errorf("listen for taskIds without the extension: %d %s", rep.status, rep.body)
	}
	rep = a.req(6, "subscriptions/listen", map[string]any{"notifications": map[string]any{"taskIds": []string{"x"}, "toolsListChanged": true}})
	if rep.status != 200 {
		t.Errorf("listen: %d %s", rep.status, rep.body)
	}
	listen := lastRequest(t, up, "subscriptions/listen")
	if strings.Contains(string(listen.Params), "taskIds") || !strings.Contains(string(listen.Params), "toolsListChanged") {
		t.Errorf("the upstream's subscriptions/listen: %s", listen.Params)
	}

	// The extension's client side on a service that owns no tasks here (no Tasks): -32021 without it, 404 with it.
	rep = plain.req(7, "tasks/get", map[string]any{"taskId": uuid.NewString()})
	if code, data := rpcError(t, rep); rep.status != 400 || code != -32021 ||
		!reflect.DeepEqual(data, map[string]any{"requiredCapabilities": map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}}}) {
		t.Errorf("tasks/get without the extension: %d %s", rep.status, rep.body)
	}
	for _, method := range []string{"tasks/result", "tasks/list", "tasks/get"} {
		rep = a.req(8, method, map[string]any{"taskId": uuid.NewString()})
		if code, _ := rpcError(t, rep); rep.status != 404 || code != -32601 {
			t.Errorf("%s: %d %s", method, rep.status, rep.body)
		}
	}

	// What the upstream received: the gate's own probe and the client's server/discover, then the rest, each with its
	// headers checked, none with a session id, params.task or the tasks extension.
	if up.HeaderMismatches() != 0 {
		t.Errorf("%d requests reached the upstream with mismatched headers", up.HeaderMismatches())
	}
	if n := up.Count("server/discover"); n != 2 {
		t.Errorf("%d server/discover, want the probe and the client's", n)
	}
	for _, q := range up.ModernRequests() {
		if q.Header.Get("Mcp-Session-Id") != "" || q.Header.Get("Authorization") != "" || strings.Contains(string(q.Params), "io.modelcontextprotocol/tasks") ||
			strings.Contains(string(q.Params), `"task"`) {
			t.Errorf("forwarded: %v %s", q.Header, q.Params)
		}
	}

	// Another principal: its own virtual session.
	b := newModern(t, keyB, u, false)
	result(t, b.req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "b"}}))
	rows := e.virtualRows("svc")
	if len(rows) != 2 || rows[0].principal > rows[1].principal || rows[0].pinned || rows[1].pinned {
		t.Fatalf("virtual rows %+v", rows)
	}
	byPrincipal := map[string][]byte{rows[0].principal: rows[0].hash, rows[1].principal: rows[1].hash}
	if byPrincipal[pA] == nil || byPrincipal[pB] == nil {
		t.Fatalf("virtual rows %+v, want %s and %s", rows, pA, pB)
	}
	if n := e.sessionRows("svc"); n != 2 {
		t.Errorf("%d session rows, want the two virtual ones", n)
	}
	calls := e.calls("svc")
	if len(calls) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	if hex.EncodeToString(calls[0].session) != hex.EncodeToString(byPrincipal[pA]) || hex.EncodeToString(calls[1].session) != hex.EncodeToString(byPrincipal[pB]) ||
		calls[0].isError == nil || *calls[0].isError {
		t.Errorf("calls rows %+v do not name the virtual sessions", calls)
	}
}

// Against a legacy-only upstream (D177): the first modern request waits for the gate's own server/discover and is
// answered at the gate with rule 3's body, as is every later one; the upstream receives the probe and no modern
// request of a client, no virtual session opens, and the client's fallback to initialize succeeds. A sealed service
// is never probed (D162). Through a tunnel, a node that reconnects drops the era: the next modern request probes
// again.
func TestModernLegacyOnly(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("legacy", url, nil)
	sealedUp, sealedURL := upstreamDouble(t)
	e.service("sealed", sealedURL, func(s *Service) { s.Sealed = true })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/legacy", false)
	for i := range 3 {
		rep := m.req(i, "tools/list", nil)
		if rep.status != 400 || string(rep.body) != string(mcpwire.LegacyNoSessionBody) {
			t.Fatalf("modern request %d: %d %s", i, rep.status, rep.body)
		}
	}
	if n := up.Count("server/discover"); n != 1 {
		t.Fatalf("%d server/discover, want the gate's one probe", n)
	}
	if mr := up.ModernRequests(); len(mr) != 1 || string(mr[0].ID) != `"kontra-discover"` || mr[0].Header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("modern requests at the upstream: %+v", mr)
	}
	if n := e.sessionRows("legacy"); n != 0 {
		t.Fatalf("%d session rows after the legacy 400", n)
	}
	// The dual-era client falls back to initialize.
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/legacy")
	if rep := c.call(r.url+"/mcp/legacy", 2, "echo", "fallback"); rep.status != 200 || !strings.Contains(string(rep.body), "fallback") {
		t.Fatalf("after the fallback: %d %s", rep.status, rep.body)
	}
	// A sealed service: rule 3's body, never probed.
	rep := m.at(r.url+"/mcp/sealed").req(1, "tools/list", nil)
	if rep.status != 400 || string(rep.body) != string(mcpwire.LegacyNoSessionBody) || len(sealedUp.Requests()) != 0 {
		t.Fatalf("a sealed service: %d %s, %d upstream requests", rep.status, rep.body, len(sealedUp.Requests()))
	}

	// Through a tunnel: the node's reconnection drops the era.
	tup := mcptest.New()
	tup.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.service("tun", "", nil)
	n1 := e.node(r, tup, "tun")
	mt := m.at(r.url + "/mcp/tun")
	if rep := mt.req(1, "tools/list", nil); rep.status != 400 || tup.Count("server/discover") != 1 {
		t.Fatalf("through the tunnel: %d %s, %d probes", rep.status, rep.body, tup.Count("server/discover"))
	}
	if rep := mt.req(2, "tools/list", nil); rep.status != 400 || tup.Count("server/discover") != 1 {
		t.Fatalf("a second request: %d, %d probes", rep.status, tup.Count("server/discover"))
	}
	drops := r.eraDrops.Load()
	_ = n1.gateMux.Close()
	eventually(t, 10*time.Second, "the node's session is removed", func() bool { return r.eraDrops.Load() > drops })
	e.node(r, tup, "tun")
	if rep := mt.req(3, "tools/list", nil); rep.status != 400 || tup.Count("server/discover") != 2 {
		t.Fatalf("after the node reconnected: %d %s, %d probes, want 2", rep.status, rep.body, tup.Count("server/discover"))
	}
}

// A probe that ends in a modern error (-32022), a node-written answer or a timeout leaves the era unknown, and the
// request that waited for it is forwarded (D177).
func TestModernProbeInconclusive(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{probeTimeout: 300 * time.Millisecond})
	key, _ := e.apiKey("buyer")
	gateProbe := func(q mcptest.Request) bool {
		return q.Method == "server/discover" && string(q.ID) == `"kontra-discover"`
	}

	// -32022 from the probe.
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	up.OnModern = func(q mcptest.Request) *mcptest.Reply {
		if gateProbe(q) {
			return &mcptest.Reply{Status: 400, Error: &mcptest.RPCError{Code: -32022, Message: "Unsupported protocol version"}}
		}
		return nil
	}
	e.service("v", url, nil)
	m := newModern(t, key, r.url+"/mcp/v", false)
	if rep := m.req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "fwd"}}); rep.status != 200 ||
		!strings.Contains(string(rep.body), "fwd") {
		t.Fatalf("after a -32022 probe: %d %s", rep.status, rep.body)
	}
	if era, ok, _ := r.tools.Era(context.Background(), ToolsKey{TenantID: e.team, Service: "v"}, nil); ok {
		t.Errorf("the era is known after an inconclusive probe: %v", era)
	}

	// A timeout.
	slow, slowURL := dualEra(t, mcptest.Tool("echo", ""))
	slow.OnModern = func(q mcptest.Request) *mcptest.Reply {
		if gateProbe(q) {
			return &mcptest.Reply{Hang: 5 * time.Second}
		}
		return nil
	}
	e.service("slow", slowURL, nil)
	start := time.Now()
	if rep := m.at(r.url+"/mcp/slow").req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "late"}}); rep.status != 200 ||
		!strings.Contains(string(rep.body), "late") || time.Since(start) > 4*time.Second {
		t.Fatalf("after a probe that timed out: %d %s after %v", rep.status, rep.body, time.Since(start))
	}

	// A node's own answer (Kontra-Node: target_unreachable) to the probe.
	nup, _ := dualEra(t, mcptest.Tool("echo", ""))
	nodeAnswer := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if strings.Contains(string(b), `"kontra-discover"`) {
			w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeTargetUnreachable))
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req.Body = io.NopCloser(strings.NewReader(string(b)))
		nup.ServeHTTP(w, req)
	})
	e.service("node", "", nil)
	e.node(r, nodeAnswer, "node")
	if rep := m.at(r.url+"/mcp/node").req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "nodeok"}}); rep.status != 200 ||
		!strings.Contains(string(rep.body), "nodeok") {
		t.Fatalf("after a node-written probe answer: %d %s", rep.status, rep.body)
	}
}

// A 404 that a dual-era upstream returns to one principal's modern request reaches that client with its status and
// body, and teaches the gate nothing: the era stays modern, and another principal's next modern request is forwarded
// and answered modern (D177).
func TestModernClientRefusalTeachesNothing(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""), mcptest.Tool("gone", ""))
	up.OnModern = func(q mcptest.Request) *mcptest.Reply {
		if q.Method == "tools/call" && strings.Contains(string(q.Params), `"name":"gone"`) {
			return &mcptest.Reply{Status: 404, Error: &mcptest.RPCError{Code: -32601, Message: "Method not found"}}
		}
		return nil
	}
	e.service("svc", url, nil)
	r := e.replica("a", replicaOpts{})
	keyA, _ := e.apiKey("a")
	keyB, _ := e.apiKey("b")
	a := newModern(t, keyA, r.url+"/mcp/svc", false)
	rep := a.req(1, "tools/call", map[string]any{"name": "gone", "arguments": map[string]any{}})
	if code, _ := rpcError(t, rep); rep.status != 404 || code != -32601 {
		t.Fatalf("the upstream's 404: %d %s", rep.status, rep.body)
	}
	if era, ok, _ := r.tools.Era(context.Background(), ToolsKey{TenantID: e.team, Service: "svc"}, nil); !ok || era != mcpwire.EraModern {
		t.Fatalf("the era after a client's 404: %v %v", era, ok)
	}
	b := newModern(t, keyB, r.url+"/mcp/svc", false)
	rep = b.req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "still modern"}})
	if res := result(t, rep); res["resultType"] != "complete" || !strings.Contains(string(rep.body), "still modern") {
		t.Fatalf("another principal: %d %s", rep.status, rep.body)
	}
	if n := up.Count("server/discover"); n != 1 {
		t.Errorf("%d probes", n)
	}
}

// Two first modern requests of a principal in flight at once, on one replica and on two, leave exactly one open
// virtual session row, which both calls rows name (D177); on one replica Admission session is asked once.
func TestModernVirtualSessionOnce(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	var arrived atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	up.OnCall = func(q mcptest.Request) mcptest.Reply {
		if arrived.Add(1)%2 == 0 {
			once.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"isError":false}`)}
	}
	e.service("svc", url, nil)
	adm := &fakeAdmission{}
	ra := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpSession}})
	rb := e.replica("b", replicaOpts{})
	keyP, _ := e.apiKey("p")
	keyQ, _ := e.apiKey("q")
	both := func(c1, c2 *modernClient) {
		var wg sync.WaitGroup
		for i, c := range []*modernClient{c1, c2} {
			wg.Go(func() {
				rep := c.req(i+1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "x"}})
				if rep.status != 200 {
					t.Errorf("call: %d %s", rep.status, rep.body)
				}
			})
		}
		wg.Wait()
	}
	// One replica.
	both(newModern(t, keyP, ra.url+"/mcp/svc", false), newModern(t, keyP, ra.url+"/mcp/svc", false))
	if n := adm.n(); n != 1 {
		t.Errorf("Admission session asked %d times on one replica, want 1", n)
	}
	for _, q := range adm.asked {
		if q.Operation != hooks.OpSession {
			t.Errorf("admission asked %s", q.Operation)
		}
	}
	// Two replicas.
	arrived.Store(0)
	gate = make(chan struct{})
	once = sync.Once{}
	both(newModern(t, keyQ, ra.url+"/mcp/svc", false), newModern(t, keyQ, rb.url+"/mcp/svc", false))
	rows := e.virtualRows("svc")
	if len(rows) != 2 {
		t.Fatalf("virtual rows %+v, want one per principal", rows)
	}
	hashes := map[string]bool{hex.EncodeToString(rows[0].hash): true, hex.EncodeToString(rows[1].hash): true}
	calls := e.calls("svc")
	if len(calls) != 4 {
		t.Fatalf("%d calls rows", len(calls))
	}
	per := map[string]int{}
	for _, c := range calls {
		h := hex.EncodeToString(c.session)
		if !hashes[h] {
			t.Errorf("a calls row names no open virtual session: %+v", c)
		}
		per[h]++
	}
	for h, n := range per {
		if n != 2 {
			t.Errorf("session %s named by %d calls rows, want 2", h, n)
		}
	}
}

// A modern request for a tunnel session another replica holds goes through the peer hop (§11.2, §11.12) with its
// headers, and no preamble of a modern exchange carries mcp_session or mcp_request_id (D180).
func TestModernPeerHop(t *testing.T) {
	e := newEnv(t)
	up := mcptest.New()
	up.Modern = true
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.service("tun", "", nil)
	ra := e.replica("a", replicaOpts{peer: true})
	rb := e.replica("b", replicaOpts{peer: true})
	n := e.node(rb, up, "tun")
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, ra.url+"/mcp/tun", false)
	rep := m.req(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "via the hop"}})
	if rep.status != 200 || !strings.Contains(string(rep.body), "via the hop") {
		t.Fatalf("through the peer hop: %d %s", rep.status, rep.body)
	}
	if up.HeaderMismatches() != 0 {
		t.Errorf("headers lost on the hop: %d mismatches", up.HeaderMismatches())
	}
	pre := n.fake.Preambles()
	if len(pre) < 3 { // the probe, the tools/list drain, the call
		t.Fatalf("%d exchanges", len(pre))
	}
	for _, p := range pre {
		if p.MCPSession != "" || p.MCPRequestID != "" {
			t.Errorf("a modern exchange's preamble carries MCP session fields: %+v", p)
		}
	}
	if len(e.virtualRows("tun")) != 1 {
		t.Errorf("virtual rows %+v", e.virtualRows("tun"))
	}
}

// A modern tools/call is answered with the upstream's MRTR InputRequiredResult as a plain call (a service whose
// gate owns no modern tasks), and that answer is metered with is_error NULL (no outcome yet); the call sent again
// with the answers is its own row.
func TestModernPlainMRTRMetering(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("ask", ""))
	up.OnCall = func(q mcptest.Request) mcptest.Reply {
		if strings.Contains(string(q.Params), "inputResponses") {
			return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"done"}],"isError":false}`)}
		}
		return mcptest.Reply{Result: mcptest.InputRequired(map[string]any{"q": mcptest.Elicit("which?")}, "st")}
	}
	e.service("svc", url, func(s *Service) { s.Tasks = config.TasksOff })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", true)
	rep := m.req(1, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}})
	if res := result(t, rep); res["resultType"] != "input_required" || res["requestState"] != "st" {
		t.Fatalf("the InputRequiredResult: %s", rep.body)
	}
	rep = m.req(2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}, "requestState": "st",
		"inputResponses": map[string]any{"q": map[string]any{"action": "accept", "content": map[string]any{"answer": "a"}}}})
	if !strings.Contains(string(result(t, rep)["content"].([]any)[0].(map[string]any)["text"].(string)), "done") {
		t.Fatalf("the call sent again: %s", rep.body)
	}
	calls := e.calls("svc")
	if len(calls) != 2 || calls[0].isError != nil || calls[1].isError == nil || *calls[1].isError {
		t.Fatalf("calls rows %+v", calls)
	}
	if !strings.Contains(string(lastRequest(t, up, "tools/call").Params), `"requestState":"st"`) {
		t.Errorf("the requestState did not pass untouched")
	}
}

// server-everything@2.0.0 (dual era, the tasks extension) needs Node 24 (D171): the modern relay is run against it
// when Node 24 is installed (KONTRA_NODE24 naming its node binary, or a `node` on PATH at version 24 or later), and
// the test is skipped, saying why, otherwise, or when npm cannot install the pinned package here (no network). It
// runs bound to loopback with a minimal environment (D185): server/discover lists 2026-07-28 and the legacy versions
// (cacheScope private), tools/list is filtered (get-env and gzip-file-as-resource denied) with cacheScope private and
// ttlMs at most 600000, and echo answers through the gate, which opens one virtual session.
func TestModernServerEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	nodePath := node24(t, "server-everything@2.0.0 (the modern era is tested against the dual-era double, mcptest, only)")
	npm := filepath.Join(filepath.Dir(nodePath), "npm")
	if _, err := os.Stat(npm); err != nil {
		if npm, err = exec.LookPath("npm"); err != nil {
			npmFailed(t, "server-everything@2.0.0", errors.New("npm is not installed beside node"), nil)
		}
	}
	// From test/e2e/testdata/everything-2's lock (npm ci), as the e2e test installs it: the transitive dependencies are
	// the lock's, never what the registry resolves today.
	dir := t.TempDir()
	for _, f := range []string{"package.json", "package-lock.json"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "testdata", "everything-2", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + filepath.Dir(nodePath) + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir,
		"npm_config_cache=" + filepath.Join(dir, ".npm"), "npm_config_update_notifier=false"}
	ictx, icancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer icancel()
	install := exec.CommandContext(ictx, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	install.Dir, install.Env = dir, env
	if b, err := install.CombinedOutput(); err != nil {
		npmFailed(t, "server-everything@2.0.0", err, b)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	preload, err := filepath.Abs("../../examples/mcp/everything/loopback.cjs")
	if err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(nodePath, filepath.Join(dir, "node_modules/@modelcontextprotocol/server-everything/dist/index.js"), "streamableHttp")
	srv.Dir = dir
	srv.Env = append(env, "PORT="+port, "LANG=C.UTF-8", "GZIP_ALLOWED_DOMAINS=example.invalid", "NODE_OPTIONS=--require="+preload)
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-srv.Process.Pid, syscall.SIGKILL)
		_, _ = srv.Process.Wait()
	})
	eventually(t, 30*time.Second, "server-everything listens", func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	e := newEnv(t)
	e.service("everything", "http://127.0.0.1:"+port+"/mcp", func(s *Service) {
		s.Tools = Policy{Deny: []string{"get-env", "gzip-file-as-resource"}}
	})
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/everything", false)
	d := result(t, m.req(1, "server/discover", nil))
	if !reflect.DeepEqual(d["supportedVersions"], []any{"2026-07-28", "2025-11-25", "2025-06-18"}) || d["cacheScope"] != "private" {
		t.Fatalf("server/discover: %v", d)
	}
	l2 := result(t, m.req(2, "tools/list", nil))
	tools, _ := l2["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		names[x.(map[string]any)["name"].(string)] = true
	}
	if !names["echo"] || names["get-env"] || names["gzip-file-as-resource"] || l2["cacheScope"] != "private" || l2["ttlMs"].(float64) > 600000 {
		t.Fatalf("tools/list: %v", l2)
	}
	rep := m.req(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"message": "through the gate"}})
	if !strings.Contains(string(rep.body), "through the gate") {
		t.Fatalf("echo: %d %s", rep.status, rep.body)
	}
	if len(e.virtualRows("everything")) != 1 {
		t.Fatalf("virtual rows %+v", e.virtualRows("everything"))
	}
}

// --- gate-owned modern tasks (§11.12 "Gate-owned tasks", D159, D178; mcp_modern_task, mcp_task_resume's modern variant) ---

// modernResearch is the double's modern task: working, one question under the key "interp", then a report naming the
// answer; the tool "broken" completes after 2.5 s with isError true; "quick" completes after 100 ms; "slow" after
// 1 s; "long" never ends by itself.
func modernResearch(task *mcptest.Task) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(task.Call.Params, &p)
	text := func(s string, isErr bool) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr})
		return b
	}
	switch p.Name {
	case "broken":
		time.Sleep(2500 * time.Millisecond)
		task.Complete(text("it broke", true))
		return
	case "quick":
		time.Sleep(100 * time.Millisecond)
		task.Complete(text("quick answer", false))
		return
	case "slow":
		time.Sleep(time.Second)
		task.Complete(text("slow answer", false))
		return
	case "long":
		time.Sleep(time.Minute)
		task.Complete(text("long answer", false))
		return
	}
	task.Status("working", "Gathering sources...")
	time.Sleep(100 * time.Millisecond)
	ans, ok := task.AskModern("interp", mcptest.Elicit(elicitText), time.Minute)
	if !ok {
		task.Fail(&mcptest.RPCError{Code: -32603, Message: "no clarification"})
		return
	}
	var a struct {
		Content map[string]any `json:"content"`
	}
	_ = json.Unmarshal(ans, &a)
	task.Status("working", "Continuing...")
	task.Complete(text(fmt.Sprintf("report: %v", a.Content["answer"]), false))
}

// modernTaskDouble is the dual-era double with the tasks extension and the tools of modernResearch, on 127.0.0.1.
func modernTaskDouble(t *testing.T) (*mcptest.Server, string) {
	up, url := dualEra(t, toolWith("research", ""), toolWith("broken", ""), toolWith("quick", ""), toolWith("slow", ""), toolWith("long", ""))
	up.ModernTaskRun = modernResearch
	return up, url
}

// createdModern reads a modern CreateTaskResult: the gate task's id.
func createdModern(t *testing.T, r reply) string {
	t.Helper()
	res := result(t, r)
	id, _ := res["taskId"].(string)
	if _, err := uuid.Parse(id); err != nil || res["resultType"] != "task" || res["pollIntervalMs"] != float64(1000) ||
		res["createdAt"] == nil || res["lastUpdatedAt"] == nil || res["ttlMs"].(float64) <= 0 || res["task"] != nil {
		t.Fatalf("not a modern CreateTaskResult with a gate task id: %s", r.body)
	}
	return id
}

// The acceptance flow of mcp_modern_task (§11.12, D159): a modern tools/call on a durable service whose upstream
// declares the extension is a gate task (CreateTaskResult, resultType task, a gate id); tasks/get is input_required
// with one request keyed by its gate id, the same from either replica; tasks/update logs the answer once (a second
// answer to the key is ignored) and the upstream receives it once under its own key; the task completes; tasks/result
// is -32601; another buyer's tasks/get is -32602; a legacy reader of the task sees the same log; the upstream's task
// id reaches no client; the calls row names the virtual session; nothing relayed enters Temporal history.
func TestModernTask(t *testing.T) {
	e := newTaskEnv(t)
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	ra := e.replica("a", replicaOpts{})
	rb := e.replica("b", replicaOpts{})
	key, principal := e.apiKey("buyer")
	other, _ := e.apiKey("other")
	m := newModern(t, key, ra.url+"/mcp/research", true)

	d := result(t, m.req(1, "server/discover", nil))
	if ext := d["capabilities"].(map[string]any)["extensions"].(map[string]any); ext["io.modelcontextprotocol/tasks"] == nil {
		t.Fatalf("a durable service keeps the tasks extension: %v", d)
	}
	taskID := createdModern(t, m.req(2, "tools/call", map[string]any{"name": "research", "arguments": map[string]any{"topic": "python"}}))

	var gateKey string
	var get map[string]any
	eventually(t, 15*time.Second, "tasks/get shows input_required", func() bool {
		get = result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID}))
		return get["status"] == "input_required"
	})
	reqs, _ := get["inputRequests"].(map[string]any)
	if len(reqs) != 1 || get["resultType"] != "complete" || get["pollIntervalMs"] != float64(1000) || get["taskId"] != taskID {
		t.Fatalf("tasks/get: %v", get)
	}
	for k, v := range reqs {
		gateKey = k
		entry := v.(map[string]any)
		if entry["method"] != "elicitation/create" || entry["params"].(map[string]any)["message"] != elicitText {
			t.Fatalf("the input request: %v", entry)
		}
	}
	if tid, _, ok := store.ParseGateRequestID(gateKey); !ok || tid.String() != taskID {
		t.Fatalf("the input request's key %q is not a gate id of the task", gateKey)
	}
	// The same answer from the other replica.
	getB := result(t, m.at(rb.url+"/mcp/research").req(4, "tasks/get", map[string]any{"taskId": taskID}))
	if !reflect.DeepEqual(get, getB) {
		t.Errorf("the replicas answer differently:\n%v\n%v", get, getB)
	}
	// Another buyer: -32602, as for an unknown task.
	if code, _ := rpcError(t, newModern(t, other, ra.url+"/mcp/research", true).req(5, "tasks/get", map[string]any{"taskId": taskID})); code != -32602 {
		t.Errorf("another buyer's tasks/get: %d", code)
	}
	// tasks/result is removed by the extension.
	if rep := m.req(6, "tasks/result", map[string]any{"taskId": taskID}); rep.status != 404 || func() int { c, _ := rpcError(t, rep); return c }() != -32601 {
		t.Errorf("tasks/result: %d %s", rep.status, rep.body)
	}
	// A legacy reader of the same buyer sees the same request, under the same gate id.
	lc := newTaskClient(t, key, ra.url+"/mcp/research")
	if s := status(t, lc.tasks("tasks/get", taskID, 9)); s != "input_required" {
		t.Errorf("a legacy reader's tasks/get: %s", s)
	}
	stream := lc.stream(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":10,"method":"tasks/result","params":{"taskId":%q}}`, taskID), nil)
	if ev := stream.next(15 * time.Second); ev.msg(t)["id"] != gateKey || ev.id() != gateKey {
		t.Fatalf("a legacy reader's input request: %s", ev.data)
	}

	answer := func(id int, text string) reply {
		return m.req(id, "tasks/update", map[string]any{"taskId": taskID, "inputResponses": map[string]any{
			gateKey: map[string]any{"action": "accept", "content": map[string]any{"answer": text}}}})
	}
	if res := result(t, answer(11, answerText)); res["resultType"] != "complete" || len(res) != 1 {
		t.Fatalf("tasks/update: %v", res)
	}
	if res := result(t, answer(12, "the-snake")); res["resultType"] != "complete" {
		t.Fatalf("a second tasks/update: %v", res)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_response'`, taskID); n != 1 {
		t.Fatalf("%d input_response events, want 1", n)
	}
	eventually(t, 20*time.Second, "tasks/get shows completed", func() bool {
		get = result(t, m.req(13, "tasks/get", map[string]any{"taskId": taskID}))
		return get["status"] == "completed"
	})
	if res, _ := get["result"].(map[string]any); res == nil || !strings.Contains(fmt.Sprint(res["content"]), "report: "+answerText) {
		t.Fatalf("the completed task: %v", get)
	}
	// The legacy reader's tasks/result ends with the result.
	if ev := stream.next(15 * time.Second); ev == nil || ev.msg(t)["id"] != float64(10) || !strings.Contains(ev.data, "report: "+answerText) {
		t.Fatalf("the legacy reader's tasks/result: %v", ev)
	}

	// The upstream: the call declared the extension and carried no params.task, under the client's id; the answer
	// arrived once, as tasks/update under its own key; tasks/get named its task in Mcp-Name; every header matched.
	ups := up.ModernTasks()
	if len(ups) != 1 {
		t.Fatalf("%d upstream tasks", len(ups))
	}
	call := lastRequest(t, up, "tools/call")
	if !strings.Contains(string(call.Params), "io.modelcontextprotocol/tasks") || strings.Contains(string(call.Params), `"task":`) || string(call.ID) != "2" {
		t.Errorf("the dispatch's tools/call: id %s %s", call.ID, call.Params)
	}
	var updates []string
	for _, q := range up.Requests() {
		switch q.Method {
		case "tasks/update":
			updates = append(updates, string(q.Params))
		case "tasks/get":
			if q.Header.Get("Mcp-Name") != ups[0].ID {
				t.Errorf("tasks/get's Mcp-Name %q, want the upstream id", q.Header.Get("Mcp-Name"))
			}
		}
	}
	if len(updates) != 1 || !strings.Contains(updates[0], `"interp"`) || !strings.Contains(updates[0], answerText) || strings.Contains(updates[0], "the-snake") {
		t.Fatalf("the upstream's tasks/update: %q", updates)
	}
	if up.HeaderMismatches() != 0 {
		t.Errorf("%d mismatched requests at the upstream", up.HeaderMismatches())
	}
	if strings.Contains(m.seen.String(), ups[0].ID) || strings.Contains(lc.seen.String(), ups[0].ID) || strings.Contains(m.seen.String(), `"interp"`) {
		t.Fatal("the upstream's task id or key reached a client")
	}
	// One calls row with task_id, naming the principal's virtual session.
	rows := e.virtualRows("research")
	if len(rows) != 1 || rows[0].principal != principal {
		t.Fatalf("virtual rows %+v", rows)
	}
	var n int
	for _, c := range e.calls("research") {
		if c.task != nil && c.task.String() == taskID {
			n++
			if c.isError == nil || *c.isError || hex.EncodeToString(c.session) != hex.EncodeToString(rows[0].hash) || c.tool != "research" {
				t.Errorf("calls row %+v", c)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d calls rows for the task", n)
	}
	h := e.historyText(taskID)
	for _, s := range []string{elicitText, answerText, "report: ", "Gathering sources", ups[0].ID} {
		if strings.Contains(h, s) {
			t.Errorf("the workflow history holds %q", s)
		}
	}

	// A CallToolResult with isError true is completed to a modern reader (D159), failed to a legacy one (§11.8).
	broken := createdModern(t, m.req(20, "tools/call", map[string]any{"name": "broken", "arguments": map[string]any{}}))
	eventually(t, 20*time.Second, "the broken task ends", func() bool {
		get = result(t, m.req(21, "tasks/get", map[string]any{"taskId": broken}))
		return get["status"] != "working"
	})
	if res, _ := get["result"].(map[string]any); get["status"] != "completed" || res["isError"] != true {
		t.Fatalf("a result with isError true to a modern reader: %v", get)
	}
	if s := status(t, lc.tasks("tasks/get", broken, 22)); s != "failed" {
		t.Errorf("the same task to a legacy reader: %s", s)
	}
}

// A task that ends within the inline wait is answered with its outcome in one round trip, as a plain
// CallToolResult; the gate task exists all the same (§11.12 step 4, D159).
func TestModernTaskInline(t *testing.T) {
	e := newTaskEnv(t)
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/research", true)
	result(t, m.req(1, "tools/list", nil)) // the era and the cached capabilities
	start := time.Now()
	rep := m.req(2, "tools/call", map[string]any{"name": "quick", "arguments": map[string]any{}})
	res := result(t, rep)
	if res["resultType"] != "complete" || res["taskId"] != nil || !strings.Contains(string(rep.body), "quick answer") || time.Since(start) > modernInlineWait+time.Second {
		t.Fatalf("the inline answer after %v: %s", time.Since(start), rep.body)
	}
	if n := e.count(`select count(*) from kontra.tasks where service = 'research' and protocol = 'mcp' and state = 'completed'`); n != 1 {
		t.Fatalf("%d completed gate tasks, want the one behind the inline answer", n)
	}
	if len(up.ModernTasks()) != 1 {
		t.Fatalf("%d upstream tasks", len(up.ModernTasks()))
	}
}

// mrtrDouble is a dual-era double that declares the tasks extension but answers its tool ask with an
// InputRequiredResult (a multi round-trip request) until the call comes again with the answer and requestState S1.
func mrtrDouble(t *testing.T) (*mcptest.Server, string) {
	up, url := dualEra(t, toolWith("ask", ""))
	up.ModernCapabilities = json.RawMessage(`{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`)
	up.OnCall = func(q mcptest.Request) mcptest.Reply {
		var p struct {
			InputResponses map[string]struct {
				Content map[string]any `json:"content"`
			} `json:"inputResponses"`
			RequestState string `json:"requestState"`
		}
		_ = json.Unmarshal(q.Params, &p)
		if a, ok := p.InputResponses["q1"]; ok && p.RequestState == "S1" {
			b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("got %v", a.Content["answer"])}}})
			return mcptest.Reply{Result: b}
		}
		return mcptest.Reply{Result: mcptest.InputRequired(map[string]any{"q1": mcptest.Elicit("Which one?")}, "S1")}
	}
	return up, url
}

// MRTR (§11.12, D178): an upstream InputRequiredResult with a requestState surfaces as input_required with its
// request under a gate id; after tasks/update the double receives the tools/call again with a new id, the answer as
// inputResponses under its own key and the same requestState; the task completes. The first InputRequiredResult is
// the attempt's first upstream id (its first key).
func TestModernMRTR(t *testing.T) {
	e := newTaskEnv(t)
	up, url := mrtrDouble(t)
	e.service("mrtr", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/mrtr", true)
	result(t, m.req(1, "server/discover", nil))
	taskID := createdModern(t, m.req(2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}}))
	var get map[string]any
	eventually(t, 15*time.Second, "tasks/get shows input_required", func() bool {
		get = result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID}))
		return get["status"] == "input_required" && len(get["inputRequests"].(map[string]any)) == 1
	})
	var gateKey string
	for k := range get["inputRequests"].(map[string]any) {
		gateKey = k
	}
	var upTask string
	if err := e.pool.QueryRow(context.Background(), `select coalesce(upstream_task_id, '') from kontra.attempts where task_id = $1 and first_upstream_id_at is not null`,
		taskID).Scan(&upTask); err != nil || upTask != "q1" {
		t.Fatalf("the attempt's first upstream id: %q %v, want the first key", upTask, err)
	}
	result(t, m.req(4, "tasks/update", map[string]any{"taskId": taskID, "inputResponses": map[string]any{
		gateKey: map[string]any{"action": "accept", "content": map[string]any{"answer": "the second"}}}}))
	eventually(t, 15*time.Second, "tasks/get shows completed", func() bool {
		get = result(t, m.req(5, "tasks/get", map[string]any{"taskId": taskID}))
		return get["status"] == "completed"
	})
	if !strings.Contains(fmt.Sprint(get["result"]), "got the second") {
		t.Fatalf("the result: %v", get)
	}
	var calls []mcptest.Request
	for _, q := range up.Requests() {
		if q.Method == "tools/call" {
			calls = append(calls, q)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("%d tools/call at the upstream, want the call and the one sent again", len(calls))
	}
	if string(calls[0].ID) != "2" || string(calls[1].ID) == "2" || strings.Contains(string(calls[0].Params), "inputResponses") {
		t.Errorf("the calls' ids: %s, %s", calls[0].ID, calls[1].ID)
	}
	if !strings.Contains(string(calls[1].Params), `"requestState":"S1"`) || !strings.Contains(string(calls[1].Params), `"q1":{`) ||
		!strings.Contains(string(calls[1].Params), "the second") || strings.Contains(string(calls[1].Params), gateKey) {
		t.Errorf("the call sent again: %s", calls[1].Params)
	}
	if strings.Contains(m.seen.String(), `"q1"`) || strings.Contains(m.seen.String(), "S1") {
		t.Error("the upstream's key or requestState reached the client")
	}
}

// mcp_task_resume's modern variant at package level (D178): the tunnel killed after the upstream answered the
// dispatch's tools/call with an InputRequiredResult fails the task tunnel_lost, which tasks/get shows; the upstream
// never receives that tools/call again.
func TestModernMRTRTunnelLost(t *testing.T) {
	e := newTaskEnv(t)
	up, _ := mrtrDouble(t)
	e.service("mrtr", "", nil)
	r := e.replica("a", replicaOpts{})
	n := e.node(r, up, "mrtr")
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/mrtr", true)
	result(t, m.req(1, "server/discover", nil))
	taskID := createdModern(t, m.req(2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}}))
	eventually(t, 15*time.Second, "tasks/get shows input_required", func() bool {
		return result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID}))["status"] == "input_required"
	})
	// The dispatch's exchanges carry the task in their preambles and no MCP session fields (D180).
	var dispatched int
	for _, p := range n.fake.Preambles() {
		if p.MCPSession != "" || p.MCPRequestID != "" {
			t.Errorf("a modern exchange's preamble carries MCP session fields: %+v", p)
		}
		if p.Task == taskID {
			dispatched++
		}
	}
	if dispatched == 0 {
		t.Error("no exchange of the dispatch names the task")
	}
	_ = n.gateMux.Close()
	var get map[string]any
	eventually(t, 20*time.Second, "the task fails tunnel_lost", func() bool {
		get = result(t, m.req(4, "tasks/get", map[string]any{"taskId": taskID}))
		return get["status"] == "failed"
	})
	e2, _ := get["error"].(map[string]any)
	data, _ := e2["data"].(map[string]any)
	if data["sh.kontra/failure"] != "tunnel_lost" || get["_meta"].(map[string]any)["sh.kontra/failure"] != "tunnel_lost" {
		t.Fatalf("the failure: %v", get)
	}
	time.Sleep(time.Second)
	if c := up.Calls("ask"); c != 1 {
		t.Fatalf("the upstream received the tools/call %d times", c)
	}
}

// A client that closes its response stream during the inline wait is sent neither answer, and its task ends canceled
// within 10 s, the upstream seeing the cancel (D178, D57).
func TestModernCancelOnClose(t *testing.T) {
	e := newTaskEnv(t)
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/research", true)
	result(t, m.req(1, "tools/list", nil))
	body := m.body(2, "tools/call", map[string]any{"name": "slow", "arguments": map[string]any{}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/mcp/research", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "slow")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("an answer reached the client that left: %d %s", resp.StatusCode, b)
	}
	var taskID uuid.UUID
	eventually(t, 5*time.Second, "the task exists", func() bool {
		return e.pool.QueryRow(context.Background(), `select id from kontra.tasks where service = 'research'`).Scan(&taskID) == nil
	})
	start := time.Now()
	eventually(t, 10*time.Second, "the task is canceled", func() bool {
		var st string
		_ = e.pool.QueryRow(context.Background(), `select state from kontra.tasks where id = $1`, taskID).Scan(&st)
		return st == string(store.StateCanceled)
	})
	t.Logf("canceled %v after the check began", time.Since(start))
	if up.Count("tasks/cancel") == 0 && len(up.ModernTasks()) != 0 {
		t.Errorf("the upstream task was neither cancelled nor avoided")
	}
}

// tasks/cancel acks at once, without waiting, and the task is cancelled within 10 s (D57), the upstream told.
func TestModernTaskCancel(t *testing.T) {
	e := newTaskEnv(t)
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/research", true)
	taskID := createdModern(t, m.req(1, "tools/call", map[string]any{"name": "long", "arguments": map[string]any{}}))
	start := time.Now()
	res := result(t, m.req(2, "tasks/cancel", map[string]any{"taskId": taskID}))
	if res["resultType"] != "complete" || len(res) != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("tasks/cancel after %v: %v", time.Since(start), res)
	}
	eventually(t, 10*time.Second, "tasks/get shows cancelled", func() bool {
		return result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID}))["status"] == "cancelled"
	})
	eventually(t, 10*time.Second, "the upstream is told", func() bool { return up.Count("tasks/cancel") == 1 })
	// A terminal task: nothing, the same ack.
	if res := result(t, m.req(4, "tasks/cancel", map[string]any{"taskId": taskID})); res["resultType"] != "complete" {
		t.Fatalf("tasks/cancel of a terminal task: %v", res)
	}
}

// A legacy upstream whose era the probe left unknown answers the forwarded modern request with its own 400, which is
// relayed to that client with its status and body, so a dual-era client falls back; it teaches the gate nothing
// (§11.12, D177).
func TestModernLegacyAnswerRelayed(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	up.Hold = func(q mcptest.Request) {
		if q.Method == "server/discover" && string(q.ID) == `"kontra-discover"` {
			time.Sleep(time.Second) // past the probe's bound: the era stays unknown
		}
	}
	e.service("legacy", url, nil)
	r := e.replica("a", replicaOpts{probeTimeout: 200 * time.Millisecond})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/legacy", false)
	for i, method := range []string{"tools/list", "tools/call", "resources/list"} {
		params := map[string]any{}
		if method == "tools/call" {
			params = map[string]any{"name": "echo", "arguments": map[string]any{}}
		}
		rep := m.req(i, method, params)
		if rep.status != 400 || string(rep.body) != string(mcpwire.LegacyNoSessionBody) {
			t.Fatalf("%s: %d %s, want the upstream's own 400", method, rep.status, rep.body)
		}
	}
	if _, ok, _ := r.tools.Era(context.Background(), ToolsKey{TenantID: e.team, Service: "legacy"}, nil); ok {
		t.Fatal("a client's answer taught the gate the era")
	}
	if n := e.sessionRows("legacy"); n != 0 {
		t.Fatalf("%d session rows", n)
	}
}

// Closing the client's response stream closes the upstream's (2026-07-28 "Cancellation").
func TestModernClientCloseClosesUpstream(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("hang", ""))
	up.OnModern = func(q mcptest.Request) *mcptest.Reply {
		if q.Method == "tools/call" {
			return &mcptest.Reply{Hang: 20 * time.Second}
		}
		return nil
	}
	e.service("svc", url, func(s *Service) { s.Tasks = config.TasksOff })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", false)
	result(t, m.req(1, "tools/list", nil))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/mcp/svc", strings.NewReader(m.body(2, "tools/call", map[string]any{"name": "hang"})))
	for k, v := range map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
		"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "hang"} {
		req.Header.Set(k, v)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("an answer to a request the client cancelled")
	}
	eventually(t, 5*time.Second, "the upstream's exchange is closed", func() bool { return up.HangsCut() == 1 })
}

// A virtual session is closed idle after the service's session_idle_s without a request, by any replica's sweep and
// with no upstream DELETE; the principal's next modern request opens another (D160).
func TestModernVirtualSessionIdle(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", false)
	result(t, m.req(1, "tools/list", nil))
	first := e.virtualRows("svc")
	if len(first) != 1 {
		t.Fatalf("virtual rows %+v", first)
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `update kontra.services set session_idle_s = 60 where name = 'svc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.mcp_sessions set last_seen_at = now() - interval '2 minutes' where service = 'svc'`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.h.SweepIdle(ctx); err != nil || n != 1 {
		t.Fatalf("SweepIdle: %d %v", n, err)
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `select close_reason from kontra.mcp_sessions where client_sid_hash = $1`, first[0].hash).Scan(&reason); err != nil || reason != "idle" {
		t.Fatalf("the virtual session: %q %v", reason, err)
	}
	result(t, m.req(2, "tools/list", nil))
	next := e.virtualRows("svc")
	if len(next) != 1 || hex.EncodeToString(next[0].hash) == hex.EncodeToString(first[0].hash) {
		t.Fatalf("after the idle close: %+v", next)
	}
	time.Sleep(200 * time.Millisecond)
	for _, q := range up.Requests() {
		if q.HTTPMethod == http.MethodDelete {
			t.Fatal("a virtual session's close sent the upstream a DELETE")
		}
	}
}

// Admission session refuses a principal's first modern request: its refusal is the answer, nothing of the request
// reaches the upstream, and no virtual session opens (§13.1, D177).
func TestModernAdmissionSessionDeny(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	adm := &fakeAdmission{}
	adm.set(hooks.Decision{Outcome: hooks.Deny, Refusal: &hooks.Refusal{Code: -32001, Message: "no budget"}}, nil)
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpSession}})
	key, _ := e.apiKey("buyer")
	rep := newModern(t, key, r.url+"/mcp/svc", false).req(1, "tools/list", nil)
	if code, _ := rpcError(t, rep); code != -32001 || !strings.Contains(string(rep.body), "no budget") {
		t.Fatalf("the refusal: %d %s", rep.status, rep.body)
	}
	if adm.n() != 1 || adm.asked[0].Operation != hooks.OpSession {
		t.Fatalf("admission asked %+v", adm.asked)
	}
	if up.Count("tools/list") != 0 || len(e.virtualRows("svc")) != 0 {
		t.Fatalf("a refused principal reached the upstream (%d tools/list) or a session opened", up.Count("tools/list"))
	}
}

// A quoted modern task is answered at once, working, with the approve URL in statusMessage and
// _meta["sh.kontra/approve-url"] (§11.12 step 4, D21); nothing is dispatched; and a service whose gate owns no
// modern tasks (passthrough) is shown no tasks extension by server/discover.
func TestModernTaskQuoted(t *testing.T) {
	e := newTaskEnv(t)
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	_, purl := modernTaskDouble(t)
	e.service("pass", purl, func(s *Service) { s.Tasks = config.TasksPassthrough })
	adm := &fakeAdmission{}
	adm.set(hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{ApproveURL: "https://app.example/#/t/approve",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}, nil)
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpTask}})
	key, _ := e.apiKey("buyer")
	d := result(t, newModern(t, key, r.url+"/mcp/pass", true).req(1, "server/discover", nil))
	if ext, _ := d["capabilities"].(map[string]any)["extensions"].(map[string]any); ext["io.modelcontextprotocol/tasks"] != nil {
		t.Fatalf("a passthrough service shows the tasks extension: %v", d)
	}
	m := newModern(t, key, r.url+"/mcp/research", true)
	result(t, m.req(1, "tools/list", nil))
	start := time.Now()
	rep := m.req(2, "tools/call", map[string]any{"name": "research", "arguments": map[string]any{}})
	taskID := createdModern(t, rep)
	res := result(t, rep)
	if res["status"] != "working" || res["statusMessage"] != "https://app.example/#/t/approve" ||
		res["_meta"].(map[string]any)["sh.kontra/approve-url"] != "https://app.example/#/t/approve" || time.Since(start) > time.Second {
		t.Fatalf("the quoted task after %v: %s", time.Since(start), rep.body)
	}
	if get := result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID})); get["status"] != "working" {
		t.Fatalf("tasks/get of the quoted task: %v", get)
	}
	if up.Calls("research") != 0 {
		t.Fatal("a quoted task was dispatched")
	}
}

// directModernTask is a 2026-07-28 task of the research service as the relay creates it, its first attempt begun on
// the direct backend (a dispatch-level test's fixture).
func directModernTask(t *testing.T, e *env, log *store.Log, tool string) (uuid.UUID, uuid.UUID, store.Owner) {
	t.Helper()
	ctx := context.Background()
	svc, err := log.LoadService(ctx, e.team, "research")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	owner := store.Owner{BuyerID: uuid.New(), Principal: "key:x", Via: store.ViaAPIKey}
	nt, err := store.NewMCPTask(id, svc, owner, store.StateSubmitted, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	wid := uuid.Must(uuid.NewV7())
	req := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + tool + `","arguments":{},` + modernMeta(true) + `}}`
	work := &store.MCPWork{Request: []byte(req), Tool: tool, RequestID: json.RawMessage(`7`), Version: mcpwire.V20260728,
		Session: strings.Repeat("2e", 32), ClientCapabilities: json.RawMessage(`{"elicitation":{}}`),
		Headers: map[string][]string{"Mcp-Param-Region": {"eu"}}}
	if err := log.InsertWorkItem(ctx, nil, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute, Payload: store.WorkPayload{Owner: owner, MCP: work}}); err != nil {
		t.Fatal(err)
	}
	if err := log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
		t.Fatal(err)
	}
	return id, wid, owner
}

// A modern dispatch re-attached (D42) to its attempt polls the upstream task it recorded and never sends the call
// again, nor an answer the lost dispatch delivered; every request carries the stored _meta, its headers and, for the
// call, the client's Mcp-Param-* headers.
func TestModernDispatchReattach(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	up, url := modernTaskDouble(t)
	e.service("research", url, nil)
	log := store.NewLog(e.pool)
	id, wid, owner := directModernTask(t, e, log, "research")
	d1 := &Dispatcher{ReplicaID: "r1", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	done1 := make(chan lifecycle.DispatchResult, 1)
	go func() {
		res, _ := d1.Dispatch(ctx1, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String()})
		done1 <- res
	}()
	var seq int64
	eventually(t, 15*time.Second, "the input request is logged", func() bool {
		_ = e.pool.QueryRow(ctx, `select coalesce(max(seq), 0) from kontra.task_events where task_id = $1 and kind = 'input_request'`, id).Scan(&seq)
		return seq > 0
	})
	resp := `{"jsonrpc":"2.0","id":"` + store.GateRequestID(id, seq) + `","result":{"action":"accept","content":{"answer":"A1"}}}`
	if _, ok, err := log.AppendInputResponse(ctx, store.Scope{TenantID: e.team, Service: "research", BuyerID: owner.BuyerID}, id, seq, json.RawMessage(resp)); err != nil || !ok {
		t.Fatalf("the answer: %v %v", ok, err)
	}
	eventually(t, 10*time.Second, "the first dispatch wrote the answer upstream", func() bool { return up.Count("tasks/update") == 1 })
	cancel1()
	if res := <-done1; res.Outcome != lifecycle.DispatchContextLost {
		t.Fatalf("the lost dispatch: %+v", res)
	}
	d2 := &Dispatcher{ReplicaID: "r2", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := d2.Dispatch(dctx, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String(), Resume: true})
	if err != nil || res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("the resume: %+v %v", res, err)
	}
	row, err := log.Load(ctx, id)
	if err != nil || row.State != store.StateCompleted {
		t.Fatalf("the task: %v %v", row.State, err)
	}
	if up.Calls("research") != 1 || up.Count("tasks/update") != 1 || len(up.ModernTasks()) != 1 {
		t.Fatalf("the upstream got %d calls, %d tasks/update, %d tasks", up.Calls("research"), up.Count("tasks/update"), len(up.ModernTasks()))
	}
	if up.HeaderMismatches() != 0 {
		t.Errorf("%d mismatched requests", up.HeaderMismatches())
	}
	call := lastRequest(t, up, "tools/call")
	if call.Header.Get("Mcp-Param-Region") != "eu" || call.Header.Get("Mcp-Session-Id") != "" {
		t.Errorf("the call's headers: %v", call.Header)
	}
	for _, q := range up.ModernRequests() {
		if q.Header.Get("Mcp-Param-Region") != "" && q.Method != "tools/call" {
			t.Errorf("%s carries the call's Mcp-Param-* header", q.Method)
		}
	}
	var upTask, upSession string
	if err := e.pool.QueryRow(ctx, `select upstream_task_id, upstream_session from kontra.attempts where task_id = $1`, id).Scan(&upTask, &upSession); err != nil ||
		upTask != up.ModernTasks()[0].ID || upSession != upTask {
		t.Fatalf("the attempt: %q %q %v", upTask, upSession, err)
	}
}

// A modern dispatch re-attached in the middle of a multi round-trip call waits for the round's answers and sends the
// call again once, with them and the round's requestState; the original call is never sent again (D178).
func TestModernDispatchReattachMRTR(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	up, url := mrtrDouble(t)
	up.Tools = []json.RawMessage{toolWith("research", "")}
	e.service("research", url, nil)
	log := store.NewLog(e.pool)
	id, wid, owner := directModernTask(t, e, log, "research")
	d1 := &Dispatcher{ReplicaID: "r1", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	done1 := make(chan lifecycle.DispatchResult, 1)
	go func() {
		res, _ := d1.Dispatch(ctx1, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String()})
		done1 <- res
	}()
	var seq int64
	eventually(t, 15*time.Second, "the round's input request is logged", func() bool {
		_ = e.pool.QueryRow(ctx, `select coalesce(max(seq), 0) from kontra.task_events where task_id = $1 and kind = 'input_request'`, id).Scan(&seq)
		return seq > 0
	})
	cancel1()
	if res := <-done1; res.Outcome != lifecycle.DispatchContextLost {
		t.Fatalf("the lost dispatch: %+v", res)
	}
	resp := `{"jsonrpc":"2.0","id":"` + store.GateRequestID(id, seq) + `","result":{"action":"accept","content":{"answer":"later"}}}`
	if _, ok, err := log.AppendInputResponse(ctx, store.Scope{TenantID: e.team, Service: "research", BuyerID: owner.BuyerID}, id, seq, json.RawMessage(resp)); err != nil || !ok {
		t.Fatalf("the answer: %v %v", ok, err)
	}
	d2 := &Dispatcher{ReplicaID: "r2", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := d2.Dispatch(dctx, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String(), Resume: true})
	if err != nil || res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("the resume: %+v %v", res, err)
	}
	if row, err := log.Load(ctx, id); err != nil || row.State != store.StateCompleted {
		t.Fatalf("the task: %v %v", row.State, err)
	}
	var calls []string
	for _, q := range up.Requests() {
		if q.Method == "tools/call" {
			calls = append(calls, string(q.Params))
		}
	}
	if len(calls) != 2 || strings.Contains(calls[0], "inputResponses") || !strings.Contains(calls[1], `"requestState":"S1"`) ||
		!strings.Contains(calls[1], "later") {
		t.Fatalf("the upstream's calls: %q", calls)
	}
}

// A multi round-trip task cancelled while it waits for its answers ends cancelled within 10 s (D57); the upstream
// keeps no task for it, so it is sent no tasks/cancel, and never the call again.
func TestModernMRTRCancel(t *testing.T) {
	e := newTaskEnv(t)
	up, url := mrtrDouble(t)
	e.service("mrtr", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/mrtr", true)
	result(t, m.req(1, "server/discover", nil))
	taskID := createdModern(t, m.req(2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}}))
	eventually(t, 15*time.Second, "tasks/get shows input_required", func() bool {
		return result(t, m.req(3, "tasks/get", map[string]any{"taskId": taskID}))["status"] == "input_required"
	})
	result(t, m.req(4, "tasks/cancel", map[string]any{"taskId": taskID}))
	eventually(t, 10*time.Second, "tasks/get shows cancelled", func() bool {
		return result(t, m.req(5, "tasks/get", map[string]any{"taskId": taskID}))["status"] == "cancelled"
	})
	time.Sleep(500 * time.Millisecond)
	if up.Count("tasks/cancel") != 0 || up.Calls("ask") != 1 {
		t.Fatalf("the upstream got %d tasks/cancel and %d calls", up.Count("tasks/cancel"), up.Calls("ask"))
	}
}
