package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// mcp_session_cross_node (spec/relay.md §11.2, §16; D163, D179): MCP services a and b, each a
// server-everything@1.0.0 behind its own node and recording proxy; node a's tunnel on replica B, every client on
// replica A. Principal P1 (buyer A's key) initializes /mcp/a, calls a tool and holds the session's GET stream: both
// complete through the peer hop, the row naming B, the replica holding the tunnel. P1's session id presented on
// /mcp/b, the same id presented by P2 (buyer B's key) on /mcp/a, and the id once DELETEd are each 404, and node b's
// server sees no request. With node a SIGKILLed, P1's next request on another session is 404 and that row is closed
// tunnel_lost; a new initialize finds no node until node a is back, then succeeds.
func TestMCPSessionCrossNode(t *testing.T) {
	h := start(t, Options{
		MCP:  map[string]MCPService{"a": {ToolsDeny: EverythingDeny}, "b": {ToolsDeny: EverythingDeny}},
		Peer: true,
	})
	ctx := ctxFor(t, 10*time.Minute)
	recA, recB := everythingBehindRecorder(t, h, "a"), everythingBehindRecorder(t, h, "b")
	h.Front.Route(h.Gates[1])
	nodeA, sessA := startNode(t, ctx, h, "node-a", nil, func(o *NodeOptions) { o.MCP = map[string]string{"a": recA.URL} })
	h.Front.Route(h.Gates[0])
	startNode(t, ctx, h, "node-b", nil, func(o *NodeOptions) { o.MCP = map[string]string{"b": recB.URL} })
	if r := sessionReplica(ctx, h, sessA); r != h.Gates[1].ReplicaID {
		t.Fatalf("node a's session is on %q, want B", r)
	}
	A := h.Gates[0]

	// P1 on A; the session's tunnel on B: initialize, a call and the GET stream through the peer hop.
	p1 := NewMCPClient(A.URL+"/mcp/a", h.APIKey)
	if _, err := p1.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	row := sessionRow(t, h, p1.SID)
	if row.replica != h.Gates[1].ReplicaID || row.tunnel != sessA || row.closed {
		t.Fatalf("P1's mcp_sessions row %+v: want the holder B and node a's session %s", row, sessA)
	}
	r, err := p1.Call(ctx, 1, "echo", map[string]any{"message": "through the hop"})
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := r.ToolText(); r.Status != http.StatusOK || isErr || text != "Echo: through the hop" {
		t.Fatalf("the pinned call: %d %s", r.Status, r.Text())
	}
	gctx, gcancel := context.WithTimeout(ctx, 60*time.Second)
	stream, err := p1.Get(gctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if stream.Status != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("the GET stream: %d %v", stream.Status, stream.Header)
	}
	if r, err := p1.Call(ctx, 2, "toggle-simulated-logging", map[string]any{}); err != nil || r.Status != http.StatusOK {
		t.Fatalf("toggle-simulated-logging: %v", err)
	}
	logged := false
	for !logged {
		ev, err := stream.Next(gctx)
		if err != nil {
			t.Fatalf("the GET stream carried no notifications/message within 60 s: %v", err)
		}
		logged = ev.Message()["method"] == "notifications/message"
	}
	stream.Close()
	gcancel()
	if _, err := p1.Call(ctx, 3, "toggle-simulated-logging", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	// Cross-service, cross-principal: 404, and node b's server sees nothing.
	seenB := recB.Len()
	r, err = p1.At(A.URL+"/mcp/b").Call(ctx, 4, "echo", map[string]any{"message": "x"})
	if err != nil || r.Status != http.StatusNotFound {
		t.Fatalf("P1's id on /mcp/b: %v %d %s", err, r.Status, r.Text())
	}
	if n := recB.Len(); n != seenB {
		t.Fatalf("node b's server received %d requests for a foreign session id", n-seenB)
	}
	p2 := &MCPClient{Endpoint: A.URL + "/mcp/a", Key: h.APIKeyB, SID: p1.SID}
	seenA := recA.Len()
	if r, err := p2.Call(ctx, 5, "echo", map[string]any{"message": "x"}); err != nil || r.Status != http.StatusNotFound {
		t.Fatalf("P1's id presented by P2: %v %+v", err, r)
	}
	if n := recA.Len(); n != seenA {
		t.Fatalf("node a's server received %d requests for P2's presentation of P1's id", n-seenA)
	}
	// DELETE, then the id again: 404.
	if st, err := p1.Delete(ctx); err != nil || st != http.StatusOK {
		t.Fatalf("DELETE: %v %d", err, st)
	}
	if row := sessionRow(t, h, p1.SID); !row.closed || row.reason != "deleted" {
		t.Fatalf("the deleted session's row: %+v", row)
	}
	if r, err := p1.Call(ctx, 6, "echo", map[string]any{"message": "x"}); err != nil || r.Status != http.StatusNotFound {
		t.Fatalf("a closed id: %v %+v", err, r)
	}

	// Node a dies: the next request on a session pinned to it is 404, and its row is closed tunnel_lost.
	p1b := NewMCPClient(A.URL+"/mcp/a", h.APIKey)
	if _, err := p1b.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodeA.Kill(); err != nil {
		t.Fatal(err)
	}
	waitRowGone(t, ctx, h, sessA, 30*time.Second, "node a was SIGKILLed")
	if r, err := p1b.Call(ctx, 7, "echo", map[string]any{"message": "x"}); err != nil || r.Status != http.StatusNotFound {
		t.Fatalf("a session whose node died: %v %+v", err, r)
	}
	eventually(t, 10*time.Second, "the row closed tunnel_lost", func() bool {
		row := sessionRow(t, h, p1b.SID)
		return row.closed && row.reason == "tunnel_lost"
	})
	// No node: initialize fails; node a back: it succeeds.
	again := NewMCPClient(A.URL+"/mcp/a", h.APIKey)
	if _, err := again.Initialize(ctx); err == nil {
		t.Fatal("an initialize with no node of the service succeeded")
	}
	h.Front.Route(h.Gates[1])
	if err := nodeA.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "an initialize on node a again", func() bool {
		_, err := again.Initialize(ctx)
		return err == nil
	})
	if r, err := again.Call(ctx, 8, "echo", map[string]any{"message": "back"}); err != nil || r.Status != http.StatusOK {
		t.Fatalf("a call after node a came back: %v %+v", err, r)
	}
}

// everythingBehindRecorder starts a server-everything@1.0.0 and a recording proxy in front of it.
func everythingBehindRecorder(t *testing.T, h *Harness, name string) *Recorder {
	t.Helper()
	srv := h.StartEverything(t, "everything-"+name)
	rec, err := StartRecorder(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(func() error { rec.Close(); return nil })
	return rec
}

// mcpSessionRow is an mcp_sessions row as the tests read it.
type mcpSessionRow struct {
	replica, tunnel, reason, principal, version string
	closed                                      bool
}

// sessionRow reads the mcp_sessions row of a gate Mcp-Session-Id (stored as its SHA-256, §11.2).
func sessionRow(t *testing.T, h *Harness, sid string) mcpSessionRow {
	t.Helper()
	var r mcpSessionRow
	err := h.Pool.QueryRow(context.Background(), `select coalesce(replica_id, ''), coalesce(tunnel_session::text, ''),
		coalesce(close_reason, ''), principal, protocol_version, closed_at is not null
		from kontra.mcp_sessions where client_sid_hash = sha256(convert_to($1, 'UTF8'))`, sid).
		Scan(&r.replica, &r.tunnel, &r.reason, &r.principal, &r.version, &r.closed)
	if err != nil {
		t.Fatalf("the mcp_sessions row of %s: %v", sid, err)
	}
	return r
}
