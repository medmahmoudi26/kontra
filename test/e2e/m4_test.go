package e2e

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The M4 tests (spec/relay.md §16 gate_mcp_*, spec/tunnel.md §16 anon_*, login_converts; DM4-15, DM4-30): the gate's
// own /mcp, anonymous sessions, the MCP resource server and kontra login, through two gate replicas, Temporal and
// Postgres, with the client on one replica and the tunnel on the other wherever the row allows. Each says its tenancy:
//
//   - single tenant (a services: block): gate_mcp_hire_test.go (TestGateMCPHire), gate_mcp_hire_quote_test.go
//     (TestGateMCPHireQuote), gate_mcp_reply_test.go (TestGateMCPReply), gate_mcp_modern_test.go (TestGateMCPModern),
//     gate_mcp_oauth_test.go (TestGateMCPOAuth);
//   - multi-tenant (tenancy: multi; tenants, services and API keys as rows followed by NOTIFY kontra_services, D116;
//     refs are <tenant>/<name>): gate_mcp_search_test.go (TestGateMCPSearch), anon_limits_test.go (TestAnonLimits),
//     anon_lifecycle_test.go (TestAnonLifecycle), login_converts_test.go (TestLoginConverts).
//
// The anonymous tests run tunnel.anonymous compressed in their own configuration only (never in deploy/), and the
// Front as a PROXY v2 forwarder (proxyproto.go), so their nodes come from the /24s and /48s each test names. This file
// holds what they share.

// hostOn starts a node on replica g (the Front routed to it while the node dials, then back to replica A) serving
// serves (name → root URL) with a service token each (single tenant), and waits until every replica serves each
// service's card.
func hostOn(t *testing.T, ctx context.Context, h *Harness, g *Gate, serves map[string]string, extra ...string) *Node {
	t.Helper()
	var tokens []string
	for _, name := range slices.Sorted(mapKeys(serves)) {
		tok, err := h.EnrollToken(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	h.Front.Route(g)
	n, err := h.StartNode(ctx, NodeOptions{Serves: serves, Enroll: tokens, Args: extra})
	h.Front.Route(h.Gates[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, replica, err := n.WaitSession(ctx, h, 0); err == nil && replica != g.ReplicaID {
		t.Fatalf("the node's session is on %s, want %s", replica, g.ReplicaID)
	}
	for name := range serves {
		if err := h.WaitCard(ctx, name, 60*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// nodeReplica is the replica holding the node's current session.
func nodeReplica(t *testing.T, ctx context.Context, h *Harness, n *Node) string {
	t.Helper()
	var r string
	if err := h.Pool.QueryRow(ctx, `select replica_id from kontra.tunnels where session_id::text = $1`, n.Session()).Scan(&r); err != nil {
		t.Fatalf("the node's tunnels row: %v", err)
	}
	return r
}

// noCredentialPost posts body to u with no credential and returns the status and the WWW-Authenticate header.
func noCredentialPost(t *testing.T, u, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

// initializeBody is a 2025-11-25 initialize request.
const initializeBody = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`

// taskCount is how many tasks the gate holds.
func taskCount(t *testing.T, h *Harness) int64 {
	return count(t, h, `select count(*) from kontra.tasks`)
}

// taskState is a task's state in the gate's table, "" when it has no row.
func taskState(t *testing.T, h *Harness, taskID string) string {
	t.Helper()
	var st string
	if err := h.Pool.QueryRow(context.Background(), `select state from kontra.tasks where id::text = $1`, taskID).Scan(&st); err != nil {
		return ""
	}
	return st
}

// failureOf is the kontra.failure of a task's status message (spec §6.4), "" when it has none.
func failureOf(t *testing.T, h *Harness, taskID string) string {
	t.Helper()
	var f *string
	if err := h.Pool.QueryRow(context.Background(), `select snapshot->'status'->'message'->'metadata'->>'kontra.failure'
		from kontra.tasks where id::text = $1`, taskID).Scan(&f); err != nil || f == nil {
		return ""
	}
	return *f
}

// count runs a count(*) query.
func count(t *testing.T, h *Harness, sql string, args ...any) int64 {
	t.Helper()
	return scalar[int64](t, h, sql, args...)
}
