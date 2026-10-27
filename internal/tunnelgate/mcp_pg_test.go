package tunnelgate

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

// PickMCP (relay.md §11.2, §6.1): this replica's sessions first, round-robin among them; another live replica's
// only with remote set and when it advertises a peer address; without remote, only local ones; none is
// ErrNoSession.
func TestPickMCP(t *testing.T) {
	e := newPickEnv(t)
	local, other, quiet := e.replica("r-local"), e.replica("r-other"), e.replica("r-quiet")
	e.exec(`update kontra.gate_replicas set peer_addr = '10.0.0.2:8444' where replica_id = $1`, other)
	k1, k2, k3, k4 := e.key("hello"), e.key("hello"), e.key("hello"), e.key("hello")
	o1 := e.session(k1, other, "hello")
	q1 := e.session(k2, quiet, "hello") // a replica without a peer address
	ctx := context.Background()
	pick := func(remote bool, avoid ...uuid.UUID) (MCPPick, error) {
		return e.picker.PickMCP(ctx, e.team, "hello", local, remote, avoid...)
	}
	if _, err := pick(false); !errors.Is(err, lifecycle.ErrNoSession) {
		t.Errorf("without remote and no local session: %v", err)
	}
	p, err := pick(true)
	if err != nil || p.Session != o1 || p.Replica != other || p.PeerAddr != "10.0.0.2:8444" {
		t.Errorf("remote: %+v %v (a replica without a peer address is never chosen, %s)", p, err, q1)
	}
	if _, err := pick(true, o1); !errors.Is(err, lifecycle.ErrNoSession) {
		t.Errorf("the one remote candidate avoided: %v", err)
	}
	l1, l2 := e.session(k3, local, "hello"), e.session(k4, local, "hello")
	seen := map[uuid.UUID]int{}
	for range 4 {
		p, err := pick(true)
		if err != nil || p.Replica != local || p.PeerAddr != "" {
			t.Fatalf("with local sessions: %+v %v", p, err)
		}
		seen[p.Session]++
	}
	if seen[l1] != 2 || seen[l2] != 2 {
		t.Errorf("round-robin: %v", seen)
	}
	if p, err := pick(false, l1); err != nil || p.Session != l2 {
		t.Errorf("avoid: %+v %v", p, err)
	}
	// A revoked binding is no candidate.
	e.exec(`update kontra.key_services set revoked_at = now() where key_id in ($1, $2)`, k3, k4)
	if p, err := pick(true); err != nil || p.Session != o1 {
		t.Errorf("after the local bindings were revoked: %+v %v", p, err)
	}
}

// The registry closes the MCP sessions pinned to a tunnel session when it closes (tunnel_lost, relay.md §11.2), or
// revoked when its key was revoked (§12), and MCPBackend says which refusal it is (ErrRevoked, ErrSessionGone).
func TestMCPRowsFollowTheSession(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	for _, s := range []string{"fs", "web"} {
		if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
			values ($1, $2, 'mcp', '{"tunnel": {}}'::jsonb, 'config')`, e.team, s); err != nil {
			t.Fatal(err)
		}
	}
	r := e.replica("a", replicaOpts{})
	connect := func() (*Session, *nodekey.Identity) {
		t.Helper()
		id := newIdentity(t)
		h := &tunnel.Hello{Name: "n", Agent: "kontra/test", Services: []tunnel.Service{{Kind: tunnel.KindMCP, Name: "fs"}, {Kind: tunnel.KindMCP, Name: "web"}}}
		for _, s := range []string{"fs", "web"} {
			tok, err := e.store.MintServiceToken(ctx, e.team, s, "test")
			if err != nil {
				t.Fatal(err)
			}
			h.Enroll = append(h.Enroll, tok)
		}
		n, err := r.startNode(t, id, h, newAgent())
		if err != nil {
			t.Fatal(err)
		}
		var s *Session
		eventually(t, 10*time.Second, "the session registered", func() bool {
			var ok bool
			s, ok = r.reg.Get(n.session)
			return ok
		})
		return s, id
	}
	pin := func(s *Session, service, name string) [32]byte {
		h := sha256.Sum256([]byte(name))
		if _, err := e.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via,
			tunnel_session, replica_id, protocol_version) values ($1, $2, $3, $4, 'key:kk_t_x', 'api_key', $5, $6, '2025-11-25')`,
			h[:], e.team, service, uuid.New(), s.ID, r.id); err != nil {
			t.Fatal(err)
		}
		return h
	}
	reason := func(h [32]byte) string {
		var s *string
		if err := e.pool.QueryRow(ctx, `select close_reason from kontra.mcp_sessions where client_sid_hash = $1`, h[:]).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return "open"
		}
		return *s
	}

	// MCPBackend: a session this replica holds, for its tenant and an MCP service it serves.
	s, _ := connect()
	if _, err := r.reg.MCPBackend(s.ID, e.team, "fs"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reg.MCPBackend(s.ID, uuid.New(), "fs"); !errors.Is(err, ErrSessionGone) || !errors.Is(err, upstream.ErrTunnelLost) || errors.Is(err, ErrRevoked) {
		t.Errorf("another tenant: %v", err)
	}
	if _, err := r.reg.MCPBackend(uuid.New(), e.team, "fs"); !errors.Is(err, ErrSessionGone) || errors.Is(err, ErrRevoked) {
		t.Errorf("an unknown session: %v", err)
	}
	// A binding revoked: that service's rows close revoked, the other's stay; MCPBackend says revoked.
	fs, web := pin(s, "fs", "fs-1"), pin(s, "web", "web-1")
	if err := e.store.RevokeBinding(ctx, s.KeyID, "fs", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the fs row closed revoked", func() bool { return reason(fs) == "revoked" })
	if reason(web) != "open" {
		t.Errorf("web: %s", reason(web))
	}
	if _, err := r.reg.MCPBackend(s.ID, e.team, "fs"); !errors.Is(err, ErrRevoked) {
		t.Errorf("a revoked binding: %v", err)
	}
	// The session closes: the rest close tunnel_lost.
	_ = s.Close("test")
	eventually(t, 5*time.Second, "the web row closed tunnel_lost", func() bool { return reason(web) == "tunnel_lost" })
	if reason(fs) != "revoked" {
		t.Errorf("a closed row changed: %s", reason(fs))
	}
	if _, err := r.reg.MCPBackend(s.ID, e.team, "web"); !errors.Is(err, ErrSessionGone) || errors.Is(err, ErrRevoked) {
		t.Errorf("a closed session: %v", err)
	}

	// A key revoked: its rows close revoked, never tunnel_lost, though the session closes too.
	s2, _ := connect()
	k := pin(s2, "web", "web-2")
	if err := e.store.RevokeKey(ctx, s2.KeyID, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the row closed revoked", func() bool { return reason(k) == "revoked" })
	if _, err := r.reg.MCPBackend(s2.ID, e.team, "web"); !errors.Is(err, ErrSessionGone) {
		t.Errorf("a revoked key: %v", err)
	}

	// CloseAll (a drain's end) closes this replica's rows tunnel_lost.
	s3, _ := connect()
	k3 := pin(s3, "fs", "fs-3")
	r.reg.CloseAll(ctx)
	if reason(k3) != "tunnel_lost" {
		t.Errorf("after CloseAll: %s", reason(k3))
	}
}

// A superseding session is registered (Get) as soon as its row commits, never after the superseded session's pinned
// MCP sessions are closed (relay.md §11.2): that statement waits on Postgres, and the new row is visible to every
// replica's picker from the commit on. A session that hosts no MCP service runs no such statement at all. Here
// kontra.mcp_sessions is locked while each key's node reconnects: the new session is held at once, the A2A-only
// key's close never waits on the table, and the MCP key's pinned row closes tunnel_lost once the lock is gone.
func TestSupersedeIsNotHeldByMCPRows(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
		values ($1, 'fs', 'mcp', '{"tunnel": {}}'::jsonb, 'config')`, e.team); err != nil {
		t.Fatal(err)
	}
	a := e.replica("a", replicaOpts{every: time.Hour})
	lock := func() func() {
		t.Helper()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `lock table kontra.mcp_sessions in exclusive mode`); err != nil {
			t.Fatal(err)
		}
		release := func() { _ = tx.Rollback(ctx) }
		t.Cleanup(release)
		return release
	}
	waiting := func() int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = current_database()
			and wait_event_type = 'Lock' and query like '%update kontra.mcp_sessions%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	held := func(n *testNode) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, ok := a.reg.Get(n.session); ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the superseding session %s is not held 2 s after its row committed", n.session)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// A2A only: nothing to close, and no statement on mcp_sessions.
	id := newIdentity(t)
	ag := newAgent()
	first := e.enrollNode(t, a, id, ag, "hello")
	release := lock()
	second := e.connectNode(t, a, id, ag, "hello")
	held(second)
	if !first.closed(5 * time.Second) {
		t.Fatal("the superseded A2A session stayed open")
	}
	time.Sleep(200 * time.Millisecond)
	if n := waiting(); n != 0 {
		t.Errorf("closing a session that hosts no MCP service waits on mcp_sessions (%d statements)", n)
	}
	release()

	// MCP: the pinned row closes tunnel_lost, after the new session is held.
	mid := newIdentity(t)
	hello := func(enroll bool) *tunnel.Hello {
		h := &tunnel.Hello{Name: "n", Agent: "kontra/test", Services: []tunnel.Service{{Kind: tunnel.KindMCP, Name: "fs"}}}
		if enroll {
			tok, err := e.store.MintServiceToken(ctx, e.team, "fs", "test")
			if err != nil {
				t.Fatal(err)
			}
			h.Enroll = append(h.Enroll, tok)
		}
		return h
	}
	old, err := a.startNode(t, mid, hello(true), newAgent())
	if err != nil {
		t.Fatal(err)
	}
	e.waitRow(t, old.session)
	held(old)
	pin := sha256.Sum256([]byte("fs-pinned"))
	if _, err := e.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via,
		tunnel_session, replica_id, protocol_version) values ($1, $2, 'fs', $3, 'key:kk_t_x', 'api_key', $4, $5, '2025-11-25')`,
		pin[:], e.team, uuid.New(), old.session, a.id); err != nil {
		t.Fatal(err)
	}
	release = lock()
	newer, err := a.startNode(t, mid, hello(false), newAgent())
	if err != nil {
		t.Fatal(err)
	}
	e.waitRow(t, newer.session)
	held(newer)
	if !old.closed(5 * time.Second) {
		t.Fatal("the superseded MCP session stayed open")
	}
	release()
	eventually(t, 15*time.Second, "the superseded session's pinned row closed tunnel_lost", func() bool {
		var reason *string
		if err := e.pool.QueryRow(ctx, `select close_reason from kontra.mcp_sessions where client_sid_hash = $1`, pin[:]).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		return reason != nil && *reason == "tunnel_lost"
	})
}

// OnMCPChange runs once per MCP service of a session when the session becomes a candidate and again when it is
// removed (the MCP relay drops the services' eras then, relay.md §11.12, D177), on the replica holding it and, by
// NOTIFY kontra_tunnel, on every other one (through the peer hop each uses the nodes another holds); the holder does
// not run them a second time for its own notice.
func TestOnMCPChange(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	for _, s := range []string{"fs", "web"} {
		if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
			values ($1, $2, 'mcp', '{"tunnel": {}}'::jsonb, 'config')`, e.team, s); err != nil {
			t.Fatal(err)
		}
	}
	r := e.replica("a", replicaOpts{})
	other := e.replica("b", replicaOpts{})
	var mu sync.Mutex
	seen := map[string]int{}
	r.reg.OnMCPChange(func(tenantID uuid.UUID, service string) {
		mu.Lock()
		defer mu.Unlock()
		if tenantID == e.team {
			seen[service]++
		}
	})
	other.reg.OnMCPChange(func(tenantID uuid.UUID, service string) {
		mu.Lock()
		defer mu.Unlock()
		if tenantID == e.team {
			seen["b:"+service]++
		}
	})
	count := func(s string) int {
		mu.Lock()
		defer mu.Unlock()
		return seen[s]
	}
	id := newIdentity(t)
	h := &tunnel.Hello{Name: "n", Agent: "kontra/test", Services: []tunnel.Service{{Kind: tunnel.KindMCP, Name: "fs"}, {Kind: tunnel.KindMCP, Name: "web"}}}
	for _, s := range []string{"fs", "web"} {
		tok, err := e.store.MintServiceToken(ctx, e.team, s, "test")
		if err != nil {
			t.Fatal(err)
		}
		h.Enroll = append(h.Enroll, tok)
	}
	n, err := r.startNode(t, id, h, newAgent())
	if err != nil {
		t.Fatal(err)
	}
	var s *Session
	eventually(t, 10*time.Second, "the session registered", func() bool {
		var ok bool
		s, ok = r.reg.Get(n.session)
		return ok
	})
	eventually(t, 5*time.Second, "the hooks ran for the connection", func() bool { return count("fs") == 1 && count("web") == 1 })
	eventually(t, 5*time.Second, "the other replica's hooks ran for the connection", func() bool { return count("b:fs") == 1 && count("b:web") == 1 })
	_ = s.Close("test")
	eventually(t, 5*time.Second, "the hooks ran for the close", func() bool { return count("fs") == 2 && count("web") == 2 })
	eventually(t, 5*time.Second, "the other replica's hooks ran for the close", func() bool { return count("b:fs") == 2 && count("b:web") == 2 })
	time.Sleep(300 * time.Millisecond) // the holder's own notices have come back by now
	if a, b, c, d := count("fs"), count("web"), count("b:fs"), count("b:web"); a != 2 || b != 2 || c != 2 || d != 2 {
		t.Fatalf("hooks ran fs %d, web %d on the holder and fs %d, web %d on the other replica", a, b, c, d)
	}
}
