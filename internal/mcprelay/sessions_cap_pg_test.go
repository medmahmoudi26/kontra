package mcprelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/tenant"
)

// max_sessions_per_principal (relay.md §11.2, D213, D231): an initialize beyond the cap closes only the principal's
// sessions whose last_seen_at is EvictIdle old, none with a request or a stream in flight, and is refused 429 when
// too few are; the cap applies to tunnel services and to principals other than anonymous.

// tunnelService is a tunnel MCP service on replica r, hosted by an mcptest double with an echo tool.
func (e *env) tunnelService(r *replica, name string, mod func(*Service)) *mcptest.Server {
	e.t.Helper()
	e.service(name, "", mod)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.node(r, up, name)
	return up
}

// backdate sets a session's last_seen_at that long ago.
func (e *env) backdate(sid string, ago time.Duration) {
	e.t.Helper()
	h := HashID(sid)
	if _, err := e.pool.Exec(context.Background(), `update kontra.mcp_sessions set last_seen_at = now() - $2::bigint * interval '1 microsecond'
		 where client_sid_hash = $1`, h[:], ago.Microseconds()); err != nil {
		e.t.Fatal(err)
	}
}

// tryInitialize is an initialize that may be refused: the session id is kept only when it opened one.
func (c *client) tryInitialize(url string) reply {
	c.t.Helper()
	c.sid = ""
	r := c.do(http.MethodPost, url, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": ""})
	if r.status == http.StatusOK {
		c.sid = r.header.Get("Mcp-Session-Id")
	}
	return r
}

// tooMany checks a refused initialize: 429, Retry-After, -32600 "too many sessions" naming the cap, no session id.
func tooMany(t *testing.T, what string, r reply, limit int) {
	t.Helper()
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("%s: %d %v %s, want 429 with Retry-After and no session", what, r.status, r.header, r.body)
	}
	m := r.msg(t)
	e, _ := m["error"].(map[string]any)
	data, _ := e["data"].(map[string]any)
	if e == nil || e["code"] != float64(-32600) || e["message"] != msgTooManySessions || data["max_sessions_per_principal"] != float64(limit) {
		t.Fatalf("%s: %s", what, r.body)
	}
}

func (e *env) openSessions(service string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.mcp_sessions where service = $1 and closed_at is null`,
		service).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// §11.2, D213, D231: a principal holds at most max_sessions_per_principal sessions of a tunnel service. An initialize
// beyond it while the principal's sessions are in recent use closes none and is refused 429 before anything reaches
// the upstream; once one has been idle for EvictIdle, an initialize closes that one (idle, with an upstream DELETE
// before the new upstream initialize), the longest idle first, never another principal's. A closed session answers 404.
func TestSessionsPerPrincipal(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	up := e.tunnelService(r, "fs", func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	keyA, _ := e.apiKey("buyer-a")
	keyB, _ := e.apiKey("buyer-b")
	u := r.url + "/mcp/fs"
	open := func(key string) *client {
		c := newClient(t, key)
		c.initialize(u)
		return c
	}
	a1, a2 := open(keyA), open(keyA)
	b1, b2 := open(keyB), open(keyB)
	if n := len(up.Sessions()); n != 4 {
		t.Fatalf("%d upstream sessions, want 4", n)
	}
	// A's sessions were all used within EvictIdle: the third is refused, and nothing is closed or opened upstream.
	a3 := newClient(t, keyA)
	tooMany(t, "A's third initialize, its sessions in use", a3.tryInitialize(u), 2)
	inits := 0
	for _, q := range up.Requests() {
		if q.Method == "initialize" {
			inits++
		}
	}
	if inits != 4 || len(up.Sessions()) != 4 || e.row(a1.sid).closed || e.row(a2.sid).closed {
		t.Fatalf("a refused initialize reached the upstream (%d initializes) or closed a session", inits)
	}
	// a1 is the longest idle of A's sessions, though it was opened first: a2 was last used earlier, but within EvictIdle.
	e.backdate(a1.sid, EvictIdle+10*time.Minute)
	e.backdate(a2.sid, EvictIdle+time.Minute)
	e.backdate(b1.sid, EvictIdle+20*time.Minute)
	a3.initialize(u)
	if row := e.row(a1.sid); !row.closed || *row.reason != string(CloseIdle) {
		t.Fatalf("A's longest-idle session over the cap: %+v", row)
	}
	if e.row(a2.sid).closed || e.row(b1.sid).closed {
		t.Fatal("an initialize closed more than it needed, or another principal's session")
	}
	if n := len(up.Sessions()); n != 4 {
		t.Fatalf("%d upstream sessions after A's third initialize, want 4: the evicted one gets a DELETE", n)
	}
	if rep := a1.call(u, 1, "echo", "x"); rep.status != http.StatusNotFound {
		t.Errorf("the evicted session: %d %s", rep.status, rep.body)
	}
	for _, c := range []*client{a2, a3, b1, b2} {
		if rep := c.call(u, 2, "echo", "still here"); rep.status != http.StatusOK {
			t.Errorf("a session within the cap: %d %s", rep.status, rep.body)
		}
	}
	// Another principal's sessions are never counted against A's: B's third is refused, its own just used.
	tooMany(t, "B's third initialize", newClient(t, keyB).tryInitialize(u), 2)
	if n := e.openSessions("fs"); n != 4 {
		t.Errorf("%d open sessions, want 4 (2 per principal)", n)
	}
}

// D231 (M3 review D225-1): every unauthenticated caller is the one principal anonymous, so the cap is not applied to
// it: one anonymous caller's initialize never closes another's session, however idle, nor is refused for theirs.
// An authenticated principal of the same service is still capped.
func TestSessionsCapLeavesAnonymousAlone(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	allow, err := auth.ParseAllowList([]string{"org:" + tenant.TeamSlug, "anonymous"})
	if err != nil {
		t.Fatal(err)
	}
	up := e.tunnelService(r, "pub", func(s *Service) { s.MaxSessionsPerPrincipal = 2; s.Allow = allow })
	u := r.url + "/mcp/pub"
	var anon []*client
	for i := range 5 {
		for _, c := range anon {
			e.backdate(c.sid, EvictIdle+time.Hour) // the oldest an anonymous session can look
		}
		c := newClient(t, "")
		c.initialize(u)
		if rep := c.call(u, 1, "echo", fmt.Sprint("anonymous ", i)); rep.status != http.StatusOK {
			t.Fatalf("anonymous %d: %d %s", i, rep.status, rep.body)
		}
		anon = append(anon, c)
	}
	for i, c := range anon {
		if row := e.row(c.sid); row.closed {
			t.Errorf("anonymous %d's session was closed: %+v", i, row)
		}
		if rep := c.call(u, 2, "echo", "still here"); rep.status != http.StatusOK {
			t.Errorf("anonymous %d: %d %s", i, rep.status, rep.body)
		}
	}
	if n := len(up.Sessions()); n != 5 {
		t.Errorf("%d upstream sessions, want 5", n)
	}
	key, _ := e.apiKey("buyer")
	k1, k2 := newClient(t, key), newClient(t, key)
	k1.initialize(u)
	k2.initialize(u)
	tooMany(t, "a key's third initialize beside anonymous callers", newClient(t, key).tryInitialize(u), 2)
}

// D231 (M3 review D225-2): a direct backend is an HTTP server the operator runs, with no stdio children to share:
// the cap does not apply to it, and one principal's sessions beyond it all stay open.
func TestSessionsCapLeavesDirectBackendsAlone(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("web", url, func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	u := r.url + "/mcp/web"
	var cs []*client
	for i := range 4 {
		c := newClient(t, key)
		c.initialize(u)
		if rep := c.call(u, 1, "echo", fmt.Sprint("client ", i)); rep.status != http.StatusOK {
			t.Fatalf("client %d: %d %s", i, rep.status, rep.body)
		}
		cs = append(cs, c)
	}
	for i, c := range cs {
		if e.row(c.sid).closed {
			t.Errorf("session %d of a direct backend was closed", i)
		}
	}
	if n := len(up.Sessions()); n != 4 {
		t.Errorf("%d upstream sessions, want 4", n)
	}
}

// D231 (M3 review D225-3): a session with a request or a GET stream in flight is never closed for a principal's new
// session, whichever replica serves it: the replica serving it keeps its last_seen_at fresh while it is in flight.
// Clients reach replica A, which hops to B for the tunnel; the new initializes reach B, which has none of them in
// flight itself. s1 holds a long tools/call and s2 a GET stream; s3 made a quick call and is idle since. The next
// initialize closes s3 only; the one after it is refused (every other session is in use or new). s1's call ends with
// its result.
func TestSessionsCapNeverClosesASessionInUse(t *testing.T) {
	oldWrite, oldEvict := IdleWrite, EvictIdle
	IdleWrite, EvictIdle = 200*time.Millisecond, 1500*time.Millisecond
	t.Cleanup(func() { IdleWrite, EvictIdle = oldWrite, oldEvict })
	e := newEnv(t)
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true})
	up := e.tunnelService(b, "tun", func(s *Service) { s.MaxSessionsPerPrincipal = 3 })
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{}, 1)
	up.Hold = func(r mcptest.Request) {
		if r.Method == "tools/call" && strings.Contains(string(r.Params), "LONG") {
			started <- struct{}{}
			<-release
		}
	}
	key, _ := e.apiKey("buyer")
	ua, ub := a.url+"/mcp/tun", b.url+"/mcp/tun"
	s1, s2, s3 := newClient(t, key), newClient(t, key), newClient(t, key)
	for _, c := range []*client{s1, s2, s3} {
		c.initialize(ua)
	}
	done := make(chan reply, 1)
	go func() { done <- s1.call(ua, 1, "echo", "LONG") }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("s1's call did not reach the upstream")
	}
	get := openGET(t, s2, ua, up, nil)
	defer get.Body.Close()
	if rep := s3.call(ua, 1, "echo", "quick"); rep.status != http.StatusOK {
		t.Fatalf("s3: %d %s", rep.status, rep.body)
	}
	time.Sleep(EvictIdle + 2*IdleWrite)

	s4 := newClient(t, key)
	s4.initialize(ub)
	if row := e.row(s3.sid); !row.closed || *row.reason != string(CloseIdle) {
		t.Fatalf("the idle session s3: %+v", row)
	}
	for name, c := range map[string]*client{"s1 (a call in flight)": s1, "s2 (a GET stream open)": s2} {
		if row := e.row(c.sid); row.closed {
			t.Fatalf("%s was closed for a new session: %+v", name, row)
		}
	}
	tooMany(t, "an initialize while every other session is in use", newClient(t, key).tryInitialize(ub), 3)
	releaseOnce.Do(func() { close(release) })
	select {
	case rep := <-done:
		if rep.status != http.StatusOK || !strings.Contains(string(rep.body), "LONG") {
			t.Fatalf("s1's call: %d %s", rep.status, rep.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("s1's call did not end")
	}
	if rep := s2.call(ua, 2, "echo", "after"); rep.status != http.StatusOK {
		t.Errorf("s2 after its stream: %d %s", rep.status, rep.body)
	}
	if rep := s3.call(ua, 2, "echo", "after"); rep.status != http.StatusNotFound {
		t.Errorf("the closed s3: %d", rep.status)
	}
}

// D231: concurrent initializes of one principal leave at most the cap open: the check is made again as each row is
// written, under one lock per principal and service, and an initialize that finds no room then is refused 429 and its
// upstream session gets a DELETE.
func TestSessionsCapConcurrentInitializes(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	up := e.tunnelService(r, "fs", func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	key, _ := e.apiKey("buyer")
	u := r.url + "/mcp/fs"
	const n = 6
	statuses := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			c := newClient(t, key)
			statuses <- c.tryInitialize(u).status
		})
	}
	wg.Wait()
	close(statuses)
	var got []int
	for s := range statuses {
		got = append(got, s)
	}
	slices.Sort(got)
	if want := []int{200, 200, 429, 429, 429, 429}; !slices.Equal(got, want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	if o := e.openSessions("fs"); o != 2 {
		t.Fatalf("%d open sessions, want 2", o)
	}
	eventually(t, 10*time.Second, "the refused initializes' upstream DELETEs", func() bool { return len(up.Sessions()) == 2 })
}

// D231 (M3 review D225-4): the cap counts one principal, the credential a session is bound to: two keys minted for
// one buyer each hold it. (Keys of one buyer and a buyer's durable tasks are outside the cap, as D231 records.)
func TestSessionsCapCountsTheCredential(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	e.tunnelService(r, "fs", func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	ctx := context.Background()
	m1, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "first", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "second", BuyerID: &m1.BuyerID, Actor: "test"})
	if err != nil || m2.BuyerID != m1.BuyerID {
		t.Fatalf("a second key of the buyer: %v", err)
	}
	u := r.url + "/mcp/fs"
	for _, key := range []string{m1.Secret, m2.Secret} {
		for range 2 {
			newClient(t, key).initialize(u)
		}
	}
	if n := e.openSessions("fs"); n != 4 {
		t.Fatalf("%d open sessions, want 4: each key holds the cap", n)
	}
	tooMany(t, "the first key's third", newClient(t, m1.Secret).tryInitialize(u), 2)
}
