package mcprelay

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// M3 review round 3: regression tests of the gate's MCP session cap and its 404s.

// r3-relay-1 (D249): stock TypeScript-SDK clients (Claude Code included) end without a DELETE. Five short-lived clients
// of one key, one after the other, each initialize, call a tool and exit: under a cap of 4 the fifth is refused 429
// with a Retry-After that says when the first will be idle for EvictIdle, at most 2 minutes, and a retry after that
// wait finds room.
func TestReview3SerialShortLivedClients(t *testing.T) {
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	e.tunnelService(r, "fs", func(s *Service) { s.MaxSessionsPerPrincipal = 4 })
	key, _ := e.apiKey("ci-runner")
	u := r.url + "/mcp/fs"
	var first string
	for i := 1; i <= 4; i++ {
		c := newClient(t, key)
		if rep := c.tryInitialize(u); rep.status != http.StatusOK {
			t.Fatalf("client %d: %d %s", i, rep.status, rep.body)
		}
		if i == 1 {
			first = c.sid
		}
		c.do(http.MethodPost, u, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
		if rep := c.call(u, 1, "echo", "job"); rep.status != http.StatusOK {
			t.Fatalf("client %d call: %d %s", i, rep.status, rep.body)
		}
		// exits: no DELETE, as the TypeScript SDK's close()
	}
	c := newClient(t, key)
	rep := c.tryInitialize(u)
	if rep.status != http.StatusTooManyRequests {
		t.Fatalf("client 5 with 4 sessions used within EvictIdle: %d %s", rep.status, rep.body)
	}
	ra, err := strconv.Atoi(rep.header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > int(EvictIdle.Seconds()) || ra > 120 {
		t.Fatalf("Retry-After %q, want the time until the first session is idle for EvictIdle (%v)", rep.header.Get("Retry-After"), EvictIdle)
	}
	// The wait the gate named passes (every session's last_seen_at moves back by Retry-After): a retry then finds
	// room, the first client's session closed as idle.
	if _, err := e.pool.Exec(t.Context(), `update kontra.mcp_sessions set last_seen_at = last_seen_at - $1::bigint * interval '1 second'
		 where service = 'fs' and closed_at is null`, ra); err != nil {
		t.Fatal(err)
	}
	if rep := c.tryInitialize(u); rep.status != http.StatusOK {
		t.Fatalf("client 5 retrying after Retry-After %ds: %d %s", ra, rep.status, rep.body)
	}
	if row := e.row(first); !row.closed || *row.reason != string(CloseIdle) {
		t.Fatalf("the first client's session: closed %v", row.closed)
	}
}

// r3-relay-2 (D250): a multi-replica gate without peer.listen. A request the balancer sends to the replica that cannot
// reach the session's tunnel is 404, and the row is closed: the client's new initialize does not count the session
// it was told is gone, so a client that bounces between replicas is never refused for its own dead sessions.
func TestReview3NoHopRowsDoNotCountAgainstTheCap(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", replicaOpts{})
	b := e.replica("b", replicaOpts{})
	e.service("tun", "", func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.node(a, up, "tun")
	key, _ := e.apiKey("buyer")
	ua, ub := a.url+"/mcp/tun", b.url+"/mcp/tun"
	c := newClient(t, key)
	for i := 0; i < 4; i++ {
		if r := c.tryInitialize(ua); r.status != http.StatusOK {
			t.Fatalf("initialize %d on A, after %d sessions the gate answered 404 for: %d %s", i, i, r.status, r.body)
		}
		if rep := c.call(ua, 1, "echo", "on A"); rep.status != http.StatusOK {
			t.Fatalf("call on A: %d %s", rep.status, rep.body)
		}
		if rep := c.call(ub, 2, "echo", "on B"); rep.status != http.StatusNotFound {
			t.Fatalf("call on B: %d %s", rep.status, rep.body)
		}
		if row := e.row(c.sid); !row.closed || *row.reason != string(CloseTunnelLost) {
			t.Fatalf("the session B answered 404 for: closed %v", row.closed)
		}
	}
}

// r3-relay-2 and -3 (D196, D250): a DELETE always closes the row deleted and is 200 when the upstream's tunnel cannot
// be reached: on a replica without the peer hop, and with the holder replica no longer live.
func TestReview3DeleteClosesTheRowDeleted(t *testing.T) {
	t.Run("no hop", func(t *testing.T) {
		e := newEnv(t)
		a := e.replica("a", replicaOpts{})
		b := e.replica("b", replicaOpts{})
		e.service("tun", "", nil)
		up := mcptest.New()
		up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
		e.node(a, up, "tun")
		key, _ := e.apiKey("buyer")
		c := newClient(t, key)
		c.initialize(a.url + "/mcp/tun")
		rep := c.do(http.MethodDelete, b.url+"/mcp/tun", "", nil)
		row := e.row(c.sid)
		if rep.status != http.StatusOK || !row.closed || *row.reason != string(CloseDeleted) {
			t.Fatalf("a DELETE on a replica without the hop: %d, row closed %v", rep.status, row.closed)
		}
		if rep := c.call(a.url+"/mcp/tun", 3, "echo", "after delete"); rep.status != http.StatusNotFound {
			t.Fatalf("the DELETEd session still serves calls on A: %d", rep.status)
		}
	})
	t.Run("holder gone", func(t *testing.T) {
		e := newEnv(t)
		a := e.replica("a", replicaOpts{peer: true})
		b := e.replica("b", replicaOpts{peer: true})
		e.service("tun", "", nil)
		up := mcptest.New()
		up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
		e.node(b, up, "tun")
		key, _ := e.apiKey("buyer")
		c := newClient(t, key)
		c.initialize(a.url + "/mcp/tun")
		if _, err := e.pool.Exec(t.Context(), `update kontra.gate_replicas set last_seen_at = now() - interval '1 hour' where replica_id = $1`, b.id); err != nil {
			t.Fatal(err)
		}
		rep := c.do(http.MethodDelete, a.url+"/mcp/tun", "", nil)
		row := e.row(c.sid)
		if rep.status != http.StatusOK || !row.closed || *row.reason != string(CloseDeleted) {
			t.Fatalf("DELETE with the holder gone: %d, row closed %v", rep.status, row.closed)
		}
	})
}
