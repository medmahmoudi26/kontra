package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// anonConfig is tunnel.anonymous compressed for the anonymous tests (spec/tunnel.md §16: compressed where a row waits
// on a clock). Test configuration only, never deploy/ (the plan's WP-07 note).
func anonConfig(ttl time.Duration, maxTenants int) string {
	return fmt.Sprintf("{ ttl: %s, purge_after: 10s, tenants_per_network: 3, services: 4, tasks: 4, calls: 8, hiring_streams: 8, "+
		"parts_bytes: 1MiB, max_tenants: %d }", ttl, maxTenants)
}

// anonNode starts a node with no credential (it asks for an anonymous session, D285) whose connections the Front
// presents as coming from src, serving serves (a2a) and mcp (mcp), --public each of public, and waits for its welcome
// and its tunnels row.
func anonNode(t *testing.T, ctx context.Context, h *Harness, src string, serves, mcp map[string]string, public ...string) *Node {
	t.Helper()
	h.Front.SetSource(netip.MustParseAddr(src))
	var args []string
	for _, p := range public {
		args = append(args, "--public", p)
	}
	n, err := h.StartNode(ctx, NodeOptions{Serves: serves, MCP: mcp, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if slug := slugOf(n); !strings.HasPrefix(slug, "anon-") {
		t.Fatalf("the node's welcome names the tenant %q, not an anonymous one", slug)
	}
	return n
}

// slugOf is the tenant the node's latest welcome names.
func slugOf(n *Node) string {
	ws := n.Welcomes()
	if len(ws) == 0 {
		return ""
	}
	return ws[len(ws)-1].Tenant
}

// waitLog waits until the node's log holds a line with every one of substrs, and returns how many it holds.
func waitLog(t *testing.T, n *Node, within time.Duration, what string, substrs ...string) int {
	t.Helper()
	var k int
	eventually(t, within, what, func() bool { k = len(n.Log(substrs...)); return k > 0 })
	return k
}

// rawJSONRPC posts a JSON-RPC request to u with key (none when "") and extra headers, and returns the status, the
// headers and the body.
func rawJSONRPC(t *testing.T, ctx context.Context, u, key, method string, params any, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("A2A-Version", "1.0")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, resp.Header, string(body)
}

// sendParams is SendMessage's params: a new message with text, answered at once (returnImmediately).
func sendParams(text string) map[string]any {
	return map[string]any{
		"message":       map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}},
		"configuration": map[string]any{"returnImmediately": true},
	}
}

// rateLimited reports whether an answer is §6.2's runtime refusal: HTTP 429, Retry-After, JSON-RPC -32600 "rate limited".
func rateLimited(st int, hdr http.Header, body string) bool {
	var rpc struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &rpc)
	return st == http.StatusTooManyRequests && hdr.Get("Retry-After") != "" && rpc.Error != nil && rpc.Error.Code == -32600 &&
		rpc.Error.Message == "rate limited"
}

// anonLimitsMaxTenants is TestAnonLimits' max_tenants.
const anonLimitsMaxTenants = 12

// anon_limits (spec/tunnel.md §16; §6.2; D277, D285, D287, D292). Multi-tenant, tunnel.anonymous compressed
// (ttl 60 s, the caps 3/4/4/8/8, parts 1 MiB, max_tenants 12), the Front a PROXY v2 forwarder (nodes come from the
// networks the test names). Asserted:
//
//   - a 4th anonymous tenant from one IPv4 /24 is rate_limited, and a 4th from one IPv6 /48 (four different /64s); the
//     refused node backs off and does not exit; the tenants' created_from are the /24 and the /48;
//   - at max_tenants' boundary: with max_tenants - 1 live anonymous tenants a node is welcomed, and the next, one past
//     max_tenants, is anonymous_unavailable;
//   - a hello declaring 5 services is service_denied, with no tenant, key or binding written; a tenant's services that
//     are not disabled are at most 4 across a reconnect with 4 new names (the old ones disabled, 404);
//   - with 4 tasks of the tenant's public service working, a 5th from an anonymous caller is 429 (-32600 "rate limited",
//     Retry-After) and writes no task; with 4 tasks the node hired through its local endpoint working, a 5th is 429
//     too (as hirer);
//   - with 8 hiring streams of the session in flight, a 9th is 429;
//   - with 8 plain MCP calls to the tenant's public MCP service in flight, a 9th is 429 with -32600;
//   - a caller's message whose parts pass the tenant's 1 MiB is InvalidParams "anonymous storage limit" before any task
//     row; an upstream's parts past it fail the task upstream_error;
//   - the anonymous services are absent from search, for another tenant's API key and through the anonymous node (a
//     listed team service is there, the positive control);
//   - at expires_at the node gets expiring, then revoked, and exits saying the session expired; its /agents/anon-…/ and
//     /mcp/anon-…/ URLs answer 404 on both replicas.
func TestAnonLimits(t *testing.T) {
	seller := StartSeller(t)
	calc := mcptest.New()
	calc.Tools = []json.RawMessage{mcptest.Tool("add", "")}
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	calc.Hold = func(r mcptest.Request) {
		if r.Method == "tools/call" {
			<-release
		}
	}
	calcURL := doubleURL(t, calc)
	h := start(t, Options{Multi: true, Anonymous: anonConfig(60*time.Second, anonLimitsMaxTenants), ProxyProtocol: true, Peer: true})
	ctx := ctxFor(t, 12*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	echo := map[string]string{"echo": seller.URL}

	// Networks: three tenants from 198.51.100.0/24, a 4th rate_limited; the same for 2001:db8:1::/48 (four /64s).
	var networkNodes []*Node
	for _, set := range []struct{ net, fourth string }{
		{"198.51.100.0/24", "198.51.100.4"},
		{"2001:db8:1::/48", "2001:db8:1:4::1"},
	} {
		for i := 1; i <= 3; i++ {
			src := fmt.Sprintf("198.51.100.%d", i)
			if strings.Contains(set.net, ":") {
				src = fmt.Sprintf("2001:db8:1:%d::1", i)
			}
			networkNodes = append(networkNodes, anonNode(t, ctx, h, src, echo, nil))
		}
		if n := count(t, h, `select count(*) from kontra.tenants where kind = 'anonymous' and created_from = $1::cidr`, set.net); n != 3 {
			t.Fatalf("%d anonymous tenants created from %s, want 3", n, set.net)
		}
		h.Front.SetSource(netip.MustParseAddr(set.fourth))
		n4, err := h.LaunchNode(NodeOptions{Serves: echo})
		if err != nil {
			t.Fatal(err)
		}
		waitLog(t, n4, 30*time.Second, "the 4th node from "+set.net+" is refused rate_limited", "the gate refused the handshake", `"code":"rate_limited"`)
		eventually(t, 30*time.Second, "the refused node tries again", func() bool {
			return len(n4.Log("the gate refused the handshake", `"code":"rate_limited"`)) >= 2
		})
		if n4.Exited() {
			t.Fatalf("the node refused rate_limited exited: it must back off and retry")
		}
		if len(n4.Welcomes()) != 0 {
			t.Fatalf("the 4th node from %s was welcomed", set.net)
		}
		if err := n4.Kill(); err != nil {
			t.Fatal(err)
		}
	}

	// max_tenants (12), at its boundary: with 11 live anonymous tenants a node is welcomed (the 12th), and the next node
	// is anonymous_unavailable. The network checks' six tenants would reach their 60 s ttl while the gate fills, moving
	// the count under the test, so their nodes stop and their tenants end now (expires_at, as their end sets it); then
	// live anonymous tenant rows fill the gate to one below max_tenants.
	for _, n := range networkNodes {
		if err := n.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Pool.Exec(ctx, `update kontra.tenants set expires_at = now()
		where kind = 'anonymous' and deleted_at is null and expires_at > now()`); err != nil {
		t.Fatal(err)
	}
	liveTenants := func() int64 {
		return count(t, h, `select count(*) from kontra.tenants where kind = 'anonymous' and deleted_at is null and expires_at > now()`)
	}
	if n := liveTenants(); n != 0 {
		t.Fatalf("%d anonymous tenants still live after the network checks' tenants ended", n)
	}
	var fillers []uuid.UUID
	for i := range anonLimitsMaxTenants - 1 {
		id := uuid.Must(uuid.NewV7())
		if _, err := h.Pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind, expires_at, created_from)
			values ($1, $2, 'anonymous', now() + interval '1 hour', $3::cidr)`, id, "anon-"+randomHex(8), fmt.Sprintf("192.0.2.%d/32", i)); err != nil {
			t.Fatal(err)
		}
		fillers = append(fillers, id)
	}
	lastPlace := anonNode(t, ctx, h, "2001:db8:2::1", echo, nil)
	if n := liveTenants(); n != anonLimitsMaxTenants {
		t.Fatalf("after the node that took the last place: %d live anonymous tenants, want max_tenants %d", n, anonLimitsMaxTenants)
	}
	h.Front.SetSource(netip.MustParseAddr("203.0.113.50"))
	nf, err := h.LaunchNode(NodeOptions{Serves: echo})
	if err != nil {
		t.Fatal(err)
	}
	waitLog(t, nf, 30*time.Second, "a node past max_tenants is refused anonymous_unavailable", "the gate refused the handshake", `"code":"anonymous_unavailable"`)
	if nf.Exited() || len(nf.Welcomes()) != 0 {
		t.Fatal("the node past max_tenants was welcomed, or exited")
	}
	if n := liveTenants(); n != anonLimitsMaxTenants {
		t.Errorf("the refused node changed the live anonymous tenants to %d", n)
	}
	for _, n := range []*Node{nf, lastPlace} {
		if err := n.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Pool.Exec(ctx, `delete from kontra.tenants where id = any($1)`, fillers); err != nil {
		t.Fatal(err)
	}

	// Services: 5 declared is service_denied with nothing written; a reconnect with 4 new names keeps the tenant at 4.
	h.Front.SetSource(netip.MustParseAddr("203.0.113.60"))
	five := map[string]string{}
	for i := 1; i <= 5; i++ {
		five[fmt.Sprintf("s%d", i)] = seller.URL
	}
	n5, err := h.LaunchNode(NodeOptions{Serves: five})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := n5.Wait(60 * time.Second); err != nil || code == 0 {
		t.Fatalf("the node declaring 5 services: exit %d, %v; want it refused for good", code, err)
	}
	if len(n5.Log("the gate refused the handshake", `"code":"service_denied"`)) == 0 {
		t.Errorf("the node declaring 5 services was not refused service_denied:\n%s", tail(n5.LogPath, 10))
	}
	if n := count(t, h, `select count(*) from kontra.tenants where created_from = '203.0.113.0/24'`) +
		count(t, h, `select count(*) from kontra.key_services k join kontra.tunnel_keys t using (key_id) where t.node_name = $1`, n5.Name); n != 0 {
		t.Errorf("the refused 5-service hello wrote %d tenants or bindings", n)
	}
	four := map[string]string{"s1": seller.URL, "s2": seller.URL, "s3": seller.URL, "s4": seller.URL}
	c1 := anonNode(t, ctx, h, "203.0.113.61", four, nil)
	slugC := slugOf(c1)
	tenantC := h.TenantOf(ctx, slugC)
	if err := c1.Kill(); err != nil {
		t.Fatal(err)
	}
	h.Front.SetSource(netip.MustParseAddr("203.0.113.61"))
	c2, err := h.StartNode(ctx, NodeOptions{Home: c1.Home, Serves: map[string]string{"s5": seller.URL, "s6": seller.URL, "s7": seller.URL, "s8": seller.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if slugOf(c2) != slugC {
		t.Fatalf("the reconnect got the tenant %s, want its own %s", slugOf(c2), slugC)
	}
	enabled := texts(t, h, `select name from kontra.services where tenant_id = $1 and disabled_at is null order by name`, tenantC)
	if strings.Join(enabled, ",") != "s5,s6,s7,s8" {
		t.Errorf("the tenant's services after a reconnect with 4 new names: %v, want s5..s8 only", enabled)
	}
	for _, g := range h.Gates {
		if _, st := getBody(ctx, g.URL+"/agents/"+slugC+"/s1/.well-known/agent-card.json", ""); st != http.StatusNotFound {
			t.Errorf("an undeclared service of the resumed tenant on %s: %d, want 404", g.Name, st)
		}
	}
	if err := c2.Kill(); err != nil {
		t.Fatal(err)
	}

	// The runtime limits, on one session: its tunnel on A, the callers on B.
	if err := h.SeedServices(ctx, SeededService{Tenant: TeamTenant, Name: "open", URL: seller.URL, Allow: []string{"anonymous"}},
		SeededService{Tenant: TeamTenant, Name: "catalog", URL: seller.URL, Listed: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.WaitA2A(ctx, "default/open", "", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	h.Front.Route(a)
	d := anonNode(t, ctx, h, "203.0.113.70", map[string]string{"work": seller.URL}, map[string]string{"calc": calcURL}, "work", "calc")
	opened := time.Now()
	slug := slugOf(d)
	tenant := h.TenantOf(ctx, slug)
	if r := nodeReplica(t, ctx, h, d); r != a.ReplicaID {
		t.Fatalf("the node's session is on %s, want A", r)
	}
	if err := h.WaitA2A(ctx, slug+"/work", "", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	local, err := d.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tasksOf := func(tid uuid.UUID) int64 {
		return count(t, h, `select count(*) from kontra.tasks where tenant_id = $1`, tid)
	}

	// As host: 4 slow tasks from an anonymous caller, then a 5th.
	work := b.URL + "/agents/" + slug + "/work"
	for i := range 4 {
		if st, _, body := rawJSONRPC(t, ctx, work, "", "SendMessage", sendParams("slow"), nil); st != http.StatusOK || !strings.Contains(body, `"result"`) {
			t.Fatalf("task %d as host: %d %s", i, st, body)
		}
	}
	eventually(t, 30*time.Second, "4 tasks of the tenant working", func() bool {
		return count(t, h, `select count(*) from kontra.tasks where tenant_id = $1 and state = 'working'`, tenant) == 4
	})
	if st, hdr, body := rawJSONRPC(t, ctx, work, "", "SendMessage", sendParams("slow"), nil); !rateLimited(st, hdr, body) {
		t.Errorf("a 5th task as host: %d %v %s, want 429 -32600 rate limited with Retry-After", st, hdr, body)
	}
	if n := tasksOf(tenant); n != 4 {
		t.Errorf("the refused 5th task as host wrote a row: %d tasks", n)
	}

	// As hirer: 4 slow tasks of the team's open service through the node's local endpoint, then a 5th.
	teamID := h.TenantOf(ctx, TeamTenant)
	hired := func() int64 {
		return count(t, h, `select count(*) from kontra.tasks where tenant_id = $1 and principal like 'node:%'`, teamID)
	}
	for i := range 4 {
		if st, _, body := rawJSONRPC(t, ctx, local+"/agents/default/open", "", "SendMessage", sendParams("slow"), nil); st != http.StatusOK ||
			!strings.Contains(body, `"result"`) {
			t.Fatalf("task %d as hirer: %d %s", i, st, body)
		}
	}
	eventually(t, 30*time.Second, "4 hired tasks working", func() bool {
		return count(t, h, `select count(*) from kontra.tasks where tenant_id = $1 and principal like 'node:%' and state = 'working'`, teamID) == 4
	})
	if st, hdr, body := rawJSONRPC(t, ctx, local+"/agents/default/open", "", "SendMessage", sendParams("slow"), nil); !rateLimited(st, hdr, body) {
		t.Errorf("a 5th task as hirer: %d %v %s, want 429 -32600 rate limited", st, hdr, body)
	}
	if n := hired(); n != 4 {
		t.Errorf("the refused 5th task as hirer wrote a row: %d tasks", n)
	}

	// Hiring streams: 8 SubscribeToTask held open through the local endpoint, then a 9th request.
	hiredIDs := texts(t, h, `select id::text from kontra.tasks where tenant_id = $1 and principal like 'node:%'`, teamID)
	sctx, stopStreams := context.WithCancel(ctx)
	var streams sync.WaitGroup
	opens := make(chan int, 8)
	for i := range 8 {
		streams.Add(1)
		go func() {
			defer streams.Done()
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i, "method": "SubscribeToTask", "params": map[string]any{"id": hiredIDs[i%4]}})
			req, _ := http.NewRequestWithContext(sctx, http.MethodPost, local+"/agents/default/open", strings.NewReader(string(b)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("A2A-Version", "1.0")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				opens <- 0
				return
			}
			defer resp.Body.Close()
			if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
				opens <- -resp.StatusCode
				return
			}
			opens <- resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	for range 8 {
		if st := <-opens; st != http.StatusOK {
			t.Fatalf("a hiring stream of the 8: %d", st)
		}
	}
	if st, _, body := rawJSONRPC(t, ctx, local+"/agents/default/open", "", "GetTask", map[string]any{"id": hiredIDs[0]}, nil); st != http.StatusTooManyRequests {
		t.Errorf("a 9th hiring stream: %d %s, want 429", st, body)
	}
	stopStreams()
	streams.Wait()

	// Plain MCP calls: 8 held upstream, then a 9th.
	mc := NewMCPClient(b.URL+"/mcp/"+slug+"/calc", "")
	mc.Capabilities = `{}`
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatalf("an anonymous caller's session of the public MCP service: %v", err)
	}
	var calls sync.WaitGroup
	for i := range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			_, _ = mc.Call(ctx, 100+i, "add", map[string]any{"a": 1, "b": 2})
		}()
	}
	eventually(t, 30*time.Second, "8 plain calls held upstream", func() bool { return calc.Count("tools/call") == 8 })
	if n := count(t, h, `select count(*) from kontra.anon_calls where tenant_id = $1`, tenant); n != 8 {
		t.Errorf("anon_calls holds %d rows for 8 calls in flight", n)
	}
	r9, err := mc.Call(ctx, 109, "add", map[string]any{"a": 1, "b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, _, ok := r9.Error(); r9.Status != http.StatusTooManyRequests || !ok || code != -32600 || msg != "rate limited" {
		t.Errorf("a 9th plain MCP call: %d %s, want 429 -32600 rate limited", r9.Status, r9.Text())
	}
	if n := calc.Count("tools/call"); n != 8 {
		t.Errorf("the 9th call reached the upstream: %d calls", n)
	}
	releaseOnce.Do(func() { close(release) })
	calls.Wait()

	// Parts, on a tenant of their own (the host cap and the stored total are per tenant): a caller's message whose parts
	// pass 1 MiB, before any task row; an upstream's parts past it fail the task.
	pn := anonNode(t, ctx, h, "203.0.113.80", map[string]string{"big": seller.URL}, nil, "big")
	pslug, ptenant := slugOf(pn), h.TenantOf(ctx, slugOf(pn))
	if err := h.WaitA2A(ctx, pslug+"/big", "", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	before := tasksOf(ptenant)
	var parts []any
	for i := range 5 {
		parts = append(parts, map[string]any{"text": fmt.Sprintf("%d:", i) + strings.Repeat("x", 220<<10)})
	}
	over := map[string]any{"message": map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": parts},
		"configuration": map[string]any{"returnImmediately": true}}
	st, _, body := rawJSONRPC(t, ctx, b.URL+"/agents/"+pslug+"/big", "", "SendMessage", over, nil)
	var refusal struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &refusal) != nil || refusal.Error.Code != -32602 || refusal.Error.Message != "anonymous storage limit" {
		t.Errorf("a caller's message past parts_bytes: %d %.300s", st, body)
	}
	if n := tasksOf(ptenant); n != before {
		t.Errorf("the refused message wrote a task row")
	}
	st, _, body = rawJSONRPC(t, ctx, b.URL+"/agents/"+pslug+"/big", "", "SendMessage", sendParams("big:6:200"), nil)
	var created struct {
		Result struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &created) != nil || created.Result.Task.ID == "" {
		t.Fatalf("a task whose upstream answers 1.2 MiB: %d %.300s", st, body)
	}
	bigTask := created.Result.Task.ID
	eventually(t, 30*time.Second, "the task whose upstream passed the total fails", func() bool { return taskState(t, h, bigTask) == "failed" })
	if f := failureOf(t, h, bigTask); f != "upstream_error" {
		t.Errorf("the task whose upstream passed parts_bytes failed %q, want upstream_error", f)
	}

	// search never shows an anonymous tenant's service: another tenant's key, and the anonymous node itself.
	for _, s := range []*GateSession{OpenGateSession(t, ctx, b, h.APIKey, `{}`)} {
		r := Tool(t, s.Tool("search", `{"query":"anon"}`))
		r2 := Tool(t, s.Tool("search", `{}`))
		if strings.Contains(string(r.StructuredRaw)+string(r2.StructuredRaw), "anon-") || !strings.Contains(string(r2.StructuredRaw), `"ref":"default/catalog"`) {
			t.Errorf("search with another tenant's key shows an anonymous service: %s %s", r.StructuredRaw, r2.StructuredRaw)
		}
	}
	lc := NewMCPClient(local+"/mcp", "")
	lc.Capabilities = `{}`
	if _, err := lc.Initialize(ctx); err != nil {
		t.Fatalf("/mcp through the anonymous node: %v", err)
	}
	sr, err := lc.Call(ctx, 1, "search", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if txt, isErr := sr.ToolText(); isErr || strings.Contains(txt, "anon-") || !strings.Contains(txt, `"ref":"default/catalog"`) {
		t.Errorf("search through the anonymous node: %s", sr.Text())
	}
	t.Logf("the runtime limits took %v of the session's 60 s", time.Since(opened).Round(time.Second))

	// The end: expiring, then revoked; the node exits saying so; every URL of the tenant is 404 on both replicas.
	if code, err := d.Wait(90 * time.Second); err != nil || code != 1 {
		t.Fatalf("the node at its session's end: exit %d, %v", code, err)
	}
	exp := d.Log("the gate says this anonymous session ends soon")
	rev := d.Log("the gate ended the anonymous session at its end")
	if len(exp) == 0 || len(rev) == 0 || !logBefore(d, "the gate says this anonymous session ends soon", "the gate ended the anonymous session at its end") {
		t.Errorf("the node's log lacks expiring before revoked:\n%s", tail(d.LogPath, 15))
	}
	if !fileContains(d.LogPath, "the anonymous session expired at") {
		t.Errorf("the node did not say the anonymous session expired:\n%s", tail(d.LogPath, 5))
	}
	eventually(t, 30*time.Second, "the tenant's URLs answer 404 on both replicas", func() bool {
		for _, g := range h.Gates {
			for _, u := range []string{g.URL + "/agents/" + slug + "/work/.well-known/agent-card.json", g.URL + "/agents/" + slug + "/work"} {
				if _, st := getBody(ctx, u, ""); st != http.StatusNotFound {
					return false
				}
			}
			if st, _ := noCredentialPost(t, g.URL+"/mcp/"+slug+"/calc", initializeBody); st != http.StatusNotFound {
				return false
			}
		}
		return true
	})
}

// logBefore reports whether the node's log has a line containing first before one containing second.
func logBefore(n *Node, first, second string) bool {
	b := readFile(n.LogPath)
	i, j := strings.Index(b, first), strings.Index(b, second)
	return i >= 0 && j >= 0 && i < j
}
