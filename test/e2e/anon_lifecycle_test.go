package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// anon_lifecycle (spec/tunnel.md §16; §6, §6.1; D277, D285, D286, D287, D292). Multi-tenant,
// tunnel.anonymous compressed (ttl 40 s, purge_after 10 s), the Front a PROXY v2 forwarder, a bucket, peer listeners.
// Asserted:
//
//   - a node with no credential, serving hello (--public), secret and an MCP service calc (--public), from
//     198.51.100.10, gets an anonymous tenant anon-<16 hex>; the same key reconnecting before expiry gets the same
//     tenant id and slug;
//   - hello answers a caller with no credential (completed); secret answers 404 to it and to another anonymous node's
//     local endpoint;
//   - expiring arrives before the end; at the end a task in flight whose upstream task exists fails tunnel_lost and one
//     whose dispatch has no upstream task id yet fails service_unavailable at its next dispatch (a SPEC-CONFLICT with the
//     row's wording, below), and the node exits 1 saying the anonymous session expired; the key's next hello opens a new tenant with a new anon-<16 hex> slug, the
//     key moved there;
//   - a probe after an enrollment that did not commit (the credentials hold the token as unconfirmed) gets unknown_key on
//     this anonymous gate, opens no anonymous tenant, and its token is sent again and enrolls the key (D285);
//   - enroll tokens from a key of a live anonymous tenant convert it: the key moves to the tokens' tenant, the anonymous
//     tenant ends at once, and an audit row records the conversion (D286);
//   - after purge_after the old tenant's keys (another node's key still in its tenant), services, bindings, enrollments
//     (a service and a tenant token an operator minted for it while it was live), tasks with their events, calls, MCP
//     sessions (the gate's own /mcp's, service NULL, included) and bucket objects are gone; its tenants row is a
//     tombstone (deleted_at, created_from 198.51.100.0/24), so the slug is never reused;
//   - no column of any kontra table holds a client's address in any form (dotted, IPv4-mapped, compressed or expanded
//     IPv6, raw bytes): while both sessions are live (their tunnels rows, open MCP sessions, a plain call in flight with
//     its anon_calls row), and at the end after the purge; with a positive control showing the scan reads each type;
//   - an anon-<16 hex> slug is refused for an account tenant.
func TestAnonLifecycle(t *testing.T) {
	seller := StartSeller(t)
	calc := mcptest.New()
	calc.Tools = []json.RawMessage{mcptest.Tool("add", "")}
	release := make(chan struct{}) // a tools/call asking to "hold" waits for it: a plain call in flight
	calc.Hold = func(r mcptest.Request) {
		if r.Method == "tools/call" && strings.Contains(string(r.Params), `"hold"`) {
			<-release
		}
	}
	calcURL := doubleURL(t, calc)
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHeld) // runs before doubleURL's Close, which would wait on a held request
	h := start(t, Options{Multi: true, Anonymous: anonConfig(40*time.Second, 1000), ProxyProtocol: true, Blobs: true, Peer: true})
	ctx := ctxFor(t, 12*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	v4, v6, v4c := netip.MustParseAddr("198.51.100.10"), netip.MustParseAddr("2001:db8:5:5::20"), netip.MustParseAddr("203.0.113.40")
	// late's seller holds a task's first event, so the gate has no upstream task id for it (§6.4's before-the-first-id).
	late, err := h.Agent(slowagent.Config{FirstEventDelay: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	serves := map[string]string{"hello": seller.URL, "secret": seller.URL, "late": late.URL()}

	// The session, and the same key again before expiry: the same tenant.
	h.Front.Route(a)
	l1 := anonNode(t, ctx, h, v4.String(), serves, map[string]string{"calc": calcURL}, "hello", "calc", "late")
	s1 := slugOf(l1)
	t1 := h.TenantOf(ctx, s1)
	if len(s1) != len("anon-")+16 || strings.Trim(strings.TrimPrefix(s1, "anon-"), "0123456789abcdef") != "" || t1 == uuid.Nil {
		t.Fatalf("the anonymous tenant's slug %q (id %s), want anon-<16 lowercase hex>", s1, t1)
	}
	if err := l1.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := l1.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	if s := slugOf(l1); s != s1 || h.TenantOf(ctx, s) != t1 {
		t.Fatalf("the reconnect before expiry got %s, want the same tenant %s", s, s1)
	}
	key1 := l1.KeyID
	if err := h.WaitA2A(ctx, s1+"/hello", "", 60*time.Second); err != nil {
		t.Fatal(err)
	}

	// hello answers anyone; secret answers 404 to an anonymous caller and to another node.
	st, _, body := rawJSONRPC(t, ctx, b.URL+"/agents/"+s1+"/hello", "", "SendMessage", map[string]any{
		"message": map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}}, nil)
	if st != http.StatusOK || !strings.Contains(body, "TASK_STATE_COMPLETED") || !strings.Contains(body, "Hello World") {
		t.Fatalf("the public hello to a caller with no credential: %d %.300s", st, body)
	}
	if st, _, body := rawJSONRPC(t, ctx, b.URL+"/agents/"+s1+"/secret", "", "SendMessage", sendParams("hi"), nil); st != http.StatusNotFound {
		t.Errorf("secret to a caller with no credential: %d %.200s, want 404", st, body)
	}
	l2 := anonNode(t, ctx, h, v6.String(), map[string]string{"other": seller.URL}, nil)
	key2, t2 := l2.KeyID, h.TenantOf(ctx, slugOf(l2))
	local2, err := l2.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, body := rawJSONRPC(t, ctx, local2+"/agents/"+s1+"/secret", "", "SendMessage", sendParams("hi"), nil); st != http.StatusNotFound {
		t.Errorf("secret to another node: %d %.200s, want 404", st, body)
	}

	// What the purge must delete later: a plain MCP call (calls, an MCP session of calc), the gate's own /mcp session
	// of the node (service NULL), a task whose parts are bucket objects, and the task in flight at the end.
	mc := NewMCPClient(b.URL+"/mcp/"+s1+"/calc", "")
	mc.Capabilities = `{}`
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := mc.Call(ctx, 1, "add", map[string]any{"text": "1+2"}); err != nil || r.Result() == nil {
		t.Fatalf("a plain MCP call to calc: %v %v", r, err)
	}
	local1, err := l1.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gm := NewMCPClient(local1+"/mcp", "")
	gm.Capabilities = `{}`
	if _, err := gm.Initialize(ctx); err != nil {
		t.Fatalf("/mcp through the anonymous node: %v", err)
	}

	// While both sessions are live: the rows that exist only then hold no client's address either (their tunnels rows,
	// the open MCP sessions, the anon_calls row of a plain call in flight, held upstream), nor do the enrollment tokens
	// an operator mints for the tenant, which the purge must delete later: a service token (its service's delete would
	// cascade it) and a tenant token (only the purge's own delete removes it).
	tokens := enroll.NewStore(h.Pool, enroll.Options{})
	if _, err := tokens.MintServiceToken(ctx, t1, "hello", "e2e"); err != nil {
		t.Fatalf("an operator's service token for %s/hello: %v", s1, err)
	}
	if _, err := tokens.MintTenantToken(ctx, t1, []string{"hello"}, "e2e"); err != nil {
		t.Fatalf("an operator's tenant token for %s: %v", s1, err)
	}
	heldDone := make(chan struct{})
	go func() {
		defer close(heldDone)
		_, _ = mc.Call(ctx, 2, "add", map[string]any{"text": "hold"})
	}()
	eventually(t, 30*time.Second, "a plain call to calc held upstream", func() bool {
		return count(t, h, `select count(*) from kontra.anon_calls where tenant_id = $1`, t1) == 1
	})
	liveRows := func() (tunnels, open int64) {
		return count(t, h, `select count(*) from kontra.tunnels where tenant_id = any($1)`, []uuid.UUID{t1, t2}),
			count(t, h, `select count(*) from kontra.mcp_sessions where tenant_id = $1 and closed_at is null`, t1)
	}
	if tunnels, open := liveRows(); tunnels != 2 || open < 2 {
		t.Fatalf("before the live scan: %d tunnels rows of the two sessions (want 2), %d open MCP sessions of the tenant (want calc's and /mcp's)", tunnels, open)
	}
	if hits := scanAddresses(t, ctx, h, v4, v6); len(hits) != 0 {
		t.Errorf("while the sessions are live, a client's address is stored in %v", hits)
	}
	if tunnels, open := liveRows(); tunnels != 2 || open < 2 || count(t, h, `select count(*) from kontra.anon_calls where tenant_id = $1`, t1) != 1 {
		t.Fatalf("a session or the held call ended during the live scan (%d tunnels rows, %d open MCP sessions): it read less than it says", tunnels, open)
	}
	t.Logf("the live scan ended %.0f s before the session's end", scalar[float64](t, h, `select extract(epoch from expires_at - now())::float8 from kontra.tenants where id = $1`, t1))
	releaseHeld()
	<-heldDone

	st, _, body = rawJSONRPC(t, ctx, b.URL+"/agents/"+s1+"/hello", "", "SendMessage", map[string]any{
		"message": map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "big:2"}}}}, nil)
	if st != http.StatusOK || !strings.Contains(body, "TASK_STATE_COMPLETED") {
		t.Fatalf("a task with two 300 KiB parts: %d %.200s", st, body)
	}
	tenantObjects := func(tid uuid.UUID) []string {
		keys, err := h.Bucket.Keys()
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(keys, func(k string) bool { return !strings.Contains(k, tid.String()) })
	}
	if objs := tenantObjects(t1); len(objs) < 2 {
		t.Fatalf("the tenant's bucket objects: %v, want its two parts", objs)
	}
	st, _, body = rawJSONRPC(t, ctx, b.URL+"/agents/"+s1+"/hello", "", "SendMessage", sendParams("slow"), nil)
	var created struct {
		Result struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &created) != nil || created.Result.Task.ID == "" {
		t.Fatalf("the slow task: %d %.200s", st, body)
	}
	inFlight := created.Result.Task.ID
	eventually(t, 30*time.Second, "the slow task works", func() bool { return taskState(t, h, inFlight) == "working" })
	st, _, body = rawJSONRPC(t, ctx, b.URL+"/agents/"+s1+"/late", "", "SendMessage", sendParams("late"), nil)
	created.Result.Task.ID = ""
	if json.Unmarshal([]byte(body), &created) != nil || created.Result.Task.ID == "" {
		t.Fatalf("the task with no upstream id: %d %.200s", st, body)
	}
	noUpstream := created.Result.Task.ID
	eventually(t, 30*time.Second, "the late task's dispatch reaches its seller", func() bool { return late.Sends() >= 1 })
	for what, n := range map[string]int64{
		"calls":                             count(t, h, `select count(*) from kontra.calls where tenant_id = $1`, t1),
		"mcp_sessions of calc":              count(t, h, `select count(*) from kontra.mcp_sessions where tenant_id = $1 and service = 'calc'`, t1),
		"mcp_sessions of /mcp":              count(t, h, `select count(*) from kontra.mcp_sessions where tenant_id = $1 and service is null`, t1),
		"key_services":                      count(t, h, `select count(*) from kontra.key_services where tenant_id = $1`, t1),
		"tasks":                             count(t, h, `select count(*) from kontra.tasks where tenant_id = $1`, t1),
		"tunnel_keys of node 2's":           count(t, h, `select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, key2, t2),
		"task_events of its tasks":          count(t, h, `select count(*) from kontra.task_events e join kontra.tasks k on k.id = e.task_id where k.tenant_id = $1`, t1),
		"services (hello, secret, calc)":    count(t, h, `select count(*) from kontra.services where tenant_id = $1`, t1),
		"enrollment_tokens of tenant scope": count(t, h, `select count(*) from kontra.enrollment_tokens where tenant_id = $1 and scope = 'tenant'`, t1),
	} {
		if n == 0 {
			t.Fatalf("before the end the tenant holds no %s: the purge check below would prove nothing", what)
		}
	}

	// The end: expiring first; the task in flight fails service_unavailable; the node exits saying so.
	if code, err := l1.Wait(90 * time.Second); err != nil || code != 1 {
		t.Fatalf("the node at its session's end: exit %d, %v", code, err)
	}
	if !logBefore(l1, "the gate says this anonymous session ends soon", "the gate ended the anonymous session at its end") {
		t.Errorf("the node's log lacks expiring before revoked:\n%s", tail(l1.LogPath, 15))
	}
	if !fileContains(l1.LogPath, "the anonymous session expired at") {
		t.Errorf("the node did not say the anonymous session expired:\n%s", tail(l1.LogPath, 5))
	}
	// SPEC-CONFLICT (tunnel.md §6.1 and its §16 row: "at the end tasks in flight fail service_unavailable", with §12 and
	// relay.md §6.4): the end revokes the session's bindings and closes it, and §12 says a revoked binding's in-flight
	// dispatches "fail tunnel_lost, or are retried if no upstream task exists"; relay §6.4 fails a task tunnel_lost
	// once an upstream task exists. The gate does exactly that, so a task whose upstream task exists fails tunnel_lost,
	// and one whose dispatch has no upstream task id yet is retried and fails service_unavailable at that next dispatch
	// (the service disabled, §13.5). Asserted as built; the proposal (D297) is to word §6.1 and the row so.
	for id, want := range map[string]string{inFlight: "tunnel_lost", noUpstream: "service_unavailable"} {
		eventually(t, 90*time.Second, "a task in flight at the end fails", func() bool { return taskState(t, h, id) == "failed" })
		if f := failureOf(t, h, id); f != want {
			t.Errorf("a task in flight at the end failed %q, want %s", f, want)
		}
	}
	eventually(t, 30*time.Second, "the ended tenant's URLs answer 404", func() bool {
		_, st := getBody(ctx, a.URL+"/agents/"+s1+"/hello/.well-known/agent-card.json", "")
		return st == http.StatusNotFound
	})

	// The key's next hello: a new tenant, a new slug; the key moved there.
	h.Front.SetSource(v4)
	if err := l1.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	s2 := slugOf(l1)
	if s2 == s1 || !strings.HasPrefix(s2, "anon-") || h.TenantOf(ctx, s2) == t1 {
		t.Fatalf("the key's hello after the end got %s, want a new anonymous tenant (not %s)", s2, s1)
	}
	if got := scalar[string](t, h, `select tenant_id::text from kontra.tunnel_keys where key_id = $1`, key1); got != h.TenantOf(ctx, s2).String() {
		t.Errorf("the key's tenant after the new session: %s, want %s", got, h.TenantOf(ctx, s2))
	}
	if err := l1.Kill(); err != nil {
		t.Fatal(err)
	}

	// D285: a probe after an enrollment that did not commit gets unknown_key on this anonymous gate, and the token
	// is sent again.
	if err := h.SeedServices(ctx, SeededService{Tenant: "probe-co", Name: "hello"}); err != nil {
		t.Fatal(err)
	}
	probeCo := h.TenantOf(ctx, "probe-co")
	tok, err := tokens.MintServiceToken(ctx, probeCo, "hello", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	home := tempDir(t, h, "home-probe")
	id, _, err := nodekey.LoadOrCreate(filepath.Join(home, nodekey.DirName, "identity.key"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := node.LoadCredentials(CredentialsFile(home))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Update(h.Front.URL, func(c *node.Credential) {
		if c.Keys == nil {
			c.Keys = map[string]*node.KeyCredential{}
		}
		c.Keys[id.KeyID()] = &node.KeyCredential{Enroll: []string{tok}, Unconfirmed: []string{node.TokenHash(tok)}}
	}); err != nil {
		t.Fatal(err)
	}
	h.Front.SetSource(netip.MustParseAddr("203.0.113.30"))
	probe, err := h.StartNode(ctx, NodeOptions{Home: home, Serves: map[string]string{"hello": seller.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Log("the earlier hello was not admitted: sending it again", `"code":"unknown_key"`)) != 1 {
		t.Errorf("the probe was not answered unknown_key, or its tokens were not sent again:\n%s", tail(probe.LogPath, 20))
	}
	if slugOf(probe) != "probe-co" {
		t.Errorf("the re-sent token enrolled the key in %s, want probe-co", slugOf(probe))
	}
	if n := count(t, h, `select count(*) from kontra.tenants where kind = 'anonymous' and created_from = '203.0.113.0/24'`); n != 0 {
		t.Errorf("the probe opened %d anonymous tenants", n)
	}
	if !scalar[bool](t, h, `select used_at is not null and used_by_key = $2 from kontra.enrollment_tokens where tenant_id = $1`, probeCo, id.KeyID()) {
		t.Error("the re-sent token is not consumed by the probing key")
	}
	if err := probe.Kill(); err != nil {
		t.Fatal(err)
	}

	// D286: enroll tokens from a key of a live anonymous tenant convert it.
	l3 := anonNode(t, ctx, h, v4c.String(), map[string]string{"hello": seller.URL}, nil)
	s3, t3 := slugOf(l3), h.TenantOf(ctx, slugOf(l3))
	if err := l3.Kill(); err != nil {
		t.Fatal(err)
	}
	tok3, err := tokens.MintServiceToken(ctx, probeCo, "hello", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	h.Front.SetSource(v4c)
	conv, err := h.StartNode(ctx, NodeOptions{Home: l3.Home, Serves: map[string]string{"hello": seller.URL}, Enroll: []string{tok3}})
	if err != nil {
		t.Fatal(err)
	}
	if slugOf(conv) != "probe-co" || conv.KeyID != l3.KeyID {
		t.Errorf("enroll from an anonymous key: welcome tenant %s key %s, want probe-co and the same key %s", slugOf(conv), conv.KeyID, l3.KeyID)
	}
	if !scalar[bool](t, h, `select expires_at <= now() from kontra.tenants where id = $1`, t3) {
		t.Errorf("the converted anonymous tenant %s did not end", s3)
	}
	if n := count(t, h, `select count(*) from kontra.audit where convert_from(row_jcs, 'UTF8') like '%tunnel.anonymous_converted%'
		and convert_from(row_jcs, 'UTF8') like '%' || $1 || '%'`, s3); n != 1 {
		t.Errorf("%d audit rows record the conversion of %s, want 1", n, s3)
	}
	if err := conv.Kill(); err != nil {
		t.Fatal(err)
	}

	// The purge, purge_after past the end: only the tombstone is left.
	eventually(t, 3*time.Minute, "the old tenant is purged", func() bool {
		return scalar[bool](t, h, `select deleted_at is not null from kontra.tenants where id = $1`, t1)
	})
	for what, q := range map[string]string{
		"services":          `select count(*) from kontra.services where tenant_id = $1`,
		"key_services":      `select count(*) from kontra.key_services where tenant_id = $1`,
		"tunnel_keys":       `select count(*) from kontra.tunnel_keys where tenant_id = $1`,
		"enrollment_tokens": `select count(*) from kontra.enrollment_tokens where tenant_id = $1`,
		"tasks":             `select count(*) from kontra.tasks where tenant_id = $1`,
		"task_events":       `select count(*) from kontra.task_events where task_id in (select id from kontra.tasks where tenant_id = $1)`,
		"calls":             `select count(*) from kontra.calls where tenant_id = $1`,
		"mcp_sessions":      `select count(*) from kontra.mcp_sessions where tenant_id = $1`,
		"anon_calls":        `select count(*) from kontra.anon_calls where tenant_id = $1`,
		"tunnels":           `select count(*) from kontra.tunnels where tenant_id = $1`,
		"api_keys":          `select count(*) from kontra.api_keys where tenant_id = $1`,
	} {
		if n := count(t, h, q, t1); n != 0 {
			t.Errorf("after the purge the tenant still has %d %s rows", n, what)
		}
	}
	if n := count(t, h, `select count(*) from kontra.task_events where task_id::text = $1`, inFlight); n != 0 {
		t.Errorf("after the purge the task in flight still has %d events", n)
	}
	if objs := tenantObjects(t1); len(objs) != 0 {
		t.Errorf("after the purge the bucket holds the tenant's objects %v", objs)
	}
	if !scalar[bool](t, h, `select slug = $2 and kind = 'anonymous' and created_from = '198.51.100.0/24'::cidr and deleted_at is not null
		from kontra.tenants where id = $1`, t1, s1) {
		t.Error("the purged tenant's row is not a tombstone keeping its slug and created_from 198.51.100.0/24")
	}
	// The other node's tenant ended too (it was not resumed): its key, still in it, goes with it.
	eventually(t, 3*time.Minute, "the other node's tenant is purged", func() bool {
		return scalar[bool](t, h, `select deleted_at is not null from kontra.tenants where id = $1`, t2)
	})
	if n := count(t, h, `select count(*) from kontra.tunnel_keys where key_id = $1`, key2); n != 0 {
		t.Errorf("the purge left the key %s that was still in its tenant", key2)
	}
	if !scalar[bool](t, h, `select created_from = '2001:db8:5::/48'::cidr from kontra.tenants where id = $1`, t2) {
		t.Error("the IPv6 node's tenant is not counted by its /48")
	}

	// No column holds a client's address, with a positive control.
	scanControl(t, ctx, h, v4, v6)
	if hits := scanAddresses(t, ctx, h, v4, v6, v4c, netip.MustParseAddr("203.0.113.30")); len(hits) != 0 {
		t.Errorf("a client's address is stored in %v", hits)
	}

	// An anon-<16 hex> slug is refused for an account tenant (the 0003 CHECK, D287).
	_, err = h.Pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, 'anon-0123456789abcdef', 'account')`, uuid.Must(uuid.NewV7()))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "anonymous_slug" {
		t.Errorf("an account tenant with an anon- slug: %v, want the anonymous_slug check", err)
	}
}

// addressForms are the ways a client address can be written: an IPv4 address dotted, IPv4-mapped (dotted and hex,
// compressed and expanded); an IPv6 address compressed, uncompressed and expanded (the M4 audit's address scan; as
// internal/anon's scan_pg_test.go). Lower case: the scan lowers what it reads.
func addressForms(a netip.Addr) []string {
	a = a.Unmap()
	var out []string
	if a.Is4() {
		b := a.As4()
		hexw := fmt.Sprintf("%02x%02x:%02x%02x", b[0], b[1], b[2], b[3])
		mapped := netip.AddrFrom16(a.As16())
		out = append(out, a.String(), "::ffff:"+a.String(), "::ffff:"+strings.TrimLeft(hexw[:4], "0")+":"+strings.TrimLeft(hexw[5:], "0"),
			"0:0:0:0:0:ffff:"+hexw, "0000:0000:0000:0000:0000:ffff:"+hexw, mapped.StringExpanded())
	} else {
		b := a.As16()
		var groups, short []string
		for i := 0; i < 16; i += 2 {
			g := fmt.Sprintf("%02x%02x", b[i], b[i+1])
			groups = append(groups, g)
			short = append(short, strings.TrimLeft(g, "0"))
		}
		for i, s := range short {
			if s == "" {
				short[i] = "0"
			}
		}
		out = append(out, a.String(), a.StringExpanded(), strings.Join(groups, ":"), strings.Join(short, ":"))
	}
	for i := range out {
		out[i] = strings.ToLower(out[i])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// scanAddresses reads every column of schema kontra of type text, varchar, text[], json, jsonb, bytea (escaped, and
// for the address's 4 or 16 bytes), inet and cidr, audit.row_jcs included, for any form of any of addrs, and returns
// "table.column" for each that holds one.
func scanAddresses(t *testing.T, ctx context.Context, h *Harness, addrs ...netip.Addr) []string {
	t.Helper()
	type column struct{ table, column, typ string }
	rows, err := h.Pool.Query(ctx, `select table_name, column_name, data_type from information_schema.columns
		where table_schema = 'kontra' and (data_type in ('text', 'character varying', 'json', 'jsonb', 'bytea', 'inet', 'cidr')
		   or (data_type = 'ARRAY' and udt_name in ('_text', '_varchar')))
		  and table_name in (select table_name from information_schema.tables where table_schema = 'kontra' and table_type = 'BASE TABLE')
		order by table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.column, &c.typ); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	if !slices.ContainsFunc(cols, func(c column) bool { return c.table == "audit" && c.column == "row_jcs" }) {
		t.Fatal("the scan does not read audit.row_jcs")
	}
	var patterns []string
	var bins [][]byte
	for _, a := range addrs {
		for _, f := range addressForms(a) {
			patterns = append(patterns, "%"+f+"%")
		}
		if a.Unmap().Is4() {
			b := a.Unmap().As4()
			bins = append(bins, b[:])
		}
		b := a.As16()
		bins = append(bins, b[:])
	}
	var hits []string
	for _, c := range cols {
		col := `"` + c.column + `"`
		var expr string
		switch c.typ {
		case "json", "jsonb", "inet", "cidr":
			expr = col + "::text"
		case "ARRAY":
			expr = "array_to_string(" + col + ", ' ')"
		case "bytea":
			expr = "encode(" + col + ", 'escape')"
		default:
			expr = col
		}
		where := "lower(" + expr + ") like any($1::text[])"
		args := []any{patterns}
		if c.typ == "bytea" {
			for _, b := range bins {
				args = append(args, b)
				where += fmt.Sprintf(" or position($%d::bytea in %s) > 0", len(args), col)
			}
		}
		// inet and cidr read as text carry the address and its prefix length (198.51.100.10/32), which the patterns
		// match; a network (198.51.100.0/24) holds no address.
		var n int
		q := fmt.Sprintf(`select count(*) from kontra.%q where %s`, c.table, where)
		if err := h.Pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n > 0 {
			hits = append(hits, c.table+"."+c.column)
		}
	}
	return hits
}

// scanControl is the scan's positive control: a scratch table with a column of each type the scan reads, each form of
// each address written in turn, must be found in every column that holds it.
func scanControl(t *testing.T, ctx context.Context, h *Harness, addrs ...netip.Addr) {
	t.Helper()
	if _, err := h.Pool.Exec(ctx, `create table kontra.scan_control (t text, v varchar(100), a text[], j json, jb jsonb, b bytea, i inet, c cidr)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = h.Pool.Exec(context.Background(), `drop table kontra.scan_control`) }()
	for _, a := range addrs {
		for _, f := range addressForms(a) {
			if _, err := h.Pool.Exec(ctx, `delete from kontra.scan_control`); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(ctx, `insert into kontra.scan_control (t, v, a, j, jb, b) values ($1::text, $1::text, array['x', $1::text],
				json_build_object('from', $1::text), jsonb_build_array(upper($1::text)), convert_to('addr=' || $1::text, 'UTF8'))`, f); err != nil {
				t.Fatal(err)
			}
			hits := slices.DeleteFunc(scanAddresses(t, ctx, h, a), func(s string) bool { return !strings.HasPrefix(s, "scan_control.") })
			if want := []string{"scan_control.a", "scan_control.b", "scan_control.j", "scan_control.jb", "scan_control.t", "scan_control.v"}; !slices.Equal(hits, want) {
				t.Fatalf("the control holding %q: the scan found %v, want %v", f, hits, want)
			}
		}
		if _, err := h.Pool.Exec(ctx, `delete from kontra.scan_control`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Pool.Exec(ctx, `insert into kontra.scan_control (b, i, c) values ($1, $2::inet, $3::cidr)`,
			append([]byte("\x00\x01"), a.Unmap().AsSlice()...), a.Unmap().String(), netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()).String()); err != nil {
			t.Fatal(err)
		}
		hits := slices.DeleteFunc(scanAddresses(t, ctx, h, a), func(s string) bool { return !strings.HasPrefix(s, "scan_control.") })
		if want := []string{"scan_control.b", "scan_control.c", "scan_control.i"}; !slices.Equal(hits, want) {
			t.Fatalf("the control holding %s's bytes, inet and cidr: the scan found %v, want %v", a, hits, want)
		}
	}
}
