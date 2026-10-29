package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// Hiring at the binary level (spec/tunnel.md §9, §12; relay.md §2.3; D34, D56): kontra connect with --api-key on a
// loopback --listen, its hiring session on replica B, the services hosted by another node on replica A.

// hiringSetup starts a host node serving echo (A2A, the echo seller) and dbl (MCP, the Go double) on replica A, and
// a hire-only node with a fresh API key on replica B; it returns the hiring node, its endpoint and its key.
func hiringSetup(t *testing.T, h *Harness, ctx context.Context) (*Node, string, string, *EchoSeller, *mcptest.Server) {
	t.Helper()
	seller, err := h.StartEchoSeller()
	if err != nil {
		t.Fatal(err)
	}
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("echo", "")}
	dbl := h.Double(up)
	startNode(t, ctx, h, "host", map[string]string{"echo": seller.URL}, func(o *NodeOptions) { o.MCP = map[string]string{"dbl": dbl} })
	if err := h.WaitCard(ctx, "echo", time.Minute); err != nil {
		t.Fatal(err)
	}
	key, err := h.enroll(ctx, "hirer")
	if err != nil {
		t.Fatal(err)
	}
	h.Front.Route(h.Gates[1])
	hirer, err := h.StartNode(ctx, NodeOptions{Name: "hirer", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	local, err := hirer.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !fileContains(hirer.OutPath, "no service is served on this session") {
		t.Fatalf("the hire-only node's welcome: %s", tail(hirer.OutPath, 10))
	}
	return hirer, local, key, seller, up
}

// The hiring row: a stock a2a-go client and a raw MCP client on a hire-only node's 127.0.0.1 endpoint, with no
// credential, complete a task and a call to services hosted by another node; the local card lists only localhost
// URLs; the task's principal is the API key's (key:<prefix>, via api_key) and its buyer the key's. auth_revoked:
// once the key is revoked, the next hiring request is 401 within 5 s and the node says why.
func TestHiring(t *testing.T) {
	h := start(t, Options{Tunnel: map[string]TunnelService{"echo": {}}, MCP: map[string]MCPService{"dbl": {}}, Peer: true})
	ctx := ctxFor(t, 5*time.Minute)
	hirer, local, key, seller, up := hiringSetup(t, h, ctx)
	c := localA2A(t, ctx, local, "echo")
	task := sendTask(t, ctx, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")))
	if task.Status.State != a2a.TaskStateCompleted || task.Status.Message.Parts[0].Text() != "done: hello" {
		t.Fatalf("a task through the hiring node: %+v", task.Status)
	}
	if len(seller.Turns()) != 1 {
		t.Fatalf("the seller ran %d turns", len(seller.Turns()))
	}
	card, st := getBody(ctx, local+"/agents/echo/.well-known/agent-card.json", "")
	if st != http.StatusOK || !strings.Contains(string(card), "http://localhost:") || strings.Contains(string(card), h.Front.URL) {
		t.Fatalf("the local card: %d %s", st, card)
	}
	var (
		principal, via string
		buyer          uuid.UUID
	)
	if err := h.Pool.QueryRow(ctx, `select principal, via, buyer_id from kontra.tasks where id = $1`, string(task.ID)).Scan(&principal, &via, &buyer); err != nil {
		t.Fatal(err)
	}
	var keyID, keyBuyer uuid.UUID
	var prefix string
	hash := auth.KeyHash(key)
	if err := h.Pool.QueryRow(ctx, `select id, buyer_id, prefix from kontra.api_keys where hash = $1`, hash[:]).Scan(&keyID, &keyBuyer, &prefix); err != nil {
		t.Fatal(err)
	}
	if principal != "key:"+prefix || via != "api_key" || buyer != keyBuyer {
		t.Fatalf("the hired task's principal %s via %s buyer %s, want key:%s via api_key buyer %s", principal, via, buyer, prefix, keyBuyer)
	}
	mc := NewMCPClient(local+"/mcp/dbl", "")
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := mc.Call(ctx, 1, "echo", map[string]any{"text": "hired"}); err != nil || r.Status != http.StatusOK || !strings.Contains(r.Text(), "hired") {
		t.Fatalf("an MCP call through the hiring node: %v %+v", err, r)
	}
	if up.Calls("echo") != 1 {
		t.Fatalf("the double received %d calls", up.Calls("echo"))
	}
	if row := sessionRow(t, h, mc.SID); row.principal != "key:"+prefix {
		t.Fatalf("the hired MCP session's principal %s", row.principal)
	}

	// auth_revoked: the key is revoked; the next hiring request is 401 within 5 s.
	keys := auth.NewAPIKeys(h.Pool, nil)
	if _, err := keys.Revoke(ctx, keyID, "e2e"); err != nil {
		t.Fatal(err)
	}
	keys.Wait()
	revokedAt := time.Now()
	eventually(t, 5*time.Second, "a hiring request refused 401", func() bool {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local+"/agents/echo/.well-known/agent-card.json", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	})
	t.Logf("401 %v after the revocation", time.Since(revokedAt).Round(100*time.Millisecond))
	eventually(t, 10*time.Second, "the node says its hiring key was revoked", func() bool {
		return len(hirer.Log("auth_revoked")) > 0
	})
}

// local_rebinding (spec/tunnel.md §9, §16): a request to the hiring endpoint with Host evil.example:7777, or with
// Origin http://evil.example:7777, on /mcp/…, /agents/… and /.well-known/…, is 403 and opens no stream: the gate's
// count of the hiring session's node-opened streams (its debug log) does not change.
func TestLocalRebinding(t *testing.T) {
	h := start(t, Options{Tunnel: map[string]TunnelService{"echo": {}}, MCP: map[string]MCPService{"dbl": {}}})
	ctx := ctxFor(t, 5*time.Minute)
	hirer, local, _, _, _ := hiringSetup(t, h, ctx)
	// A legitimate request first: the count must see it, or it would prove nothing.
	streams := func() int { return gateLogLines(h, "node-opened stream ended", hirer.Session()) }
	before := streams()
	if _, st := getBody(ctx, local+"/agents/echo/.well-known/agent-card.json", ""); st != http.StatusOK {
		t.Fatalf("the local card: %d", st)
	}
	eventually(t, 5*time.Second, "the gate logs the legitimate request's stream", func() bool { return streams() > before })
	time.Sleep(time.Second)
	before = streams()
	paths := []string{"/mcp/dbl", "/agents/echo", "/agents/echo/.well-known/agent-card.json", "/.well-known/agent-card.json"}
	for _, p := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, rebind := range []map[string]string{{"Host": "evil.example:7777"}, {"Origin": "http://evil.example:7777"}} {
				var body io.Reader
				if method == http.MethodPost {
					body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
				}
				req, _ := http.NewRequestWithContext(ctx, method, local+p, body)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				for k, v := range rebind {
					if k == "Host" {
						req.Host = v
					} else {
						req.Header.Set(k, v)
					}
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("%s %s with %v: %d, want 403", method, p, rebind, resp.StatusCode)
				}
			}
		}
	}
	time.Sleep(time.Second)
	if n := streams(); n != before {
		t.Fatalf("the rebinding requests opened %d node streams", n-before)
	}
}

// gateLogLines counts the lines of both replicas' logs that hold every one of substrs.
func gateLogLines(h *Harness, substrs ...string) int {
	n := 0
	for _, g := range h.Gates {
		for _, line := range strings.Split(readFile(g.LogPath), "\n") {
			ok := line != ""
			for _, s := range substrs {
				ok = ok && strings.Contains(line, s)
			}
			if ok {
				n++
			}
		}
	}
	return n
}
