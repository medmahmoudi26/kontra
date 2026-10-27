package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/peer"
	"kontra.sh/kontra/internal/tunnel"
)

// mcp_peer_hop (spec/relay.md §11.2, §16; D163, D179): the client on replica A, the node's tunnel (a FakeNode, which
// records every stream the gate opens) on B. initialize, a pinned tools/call answered as an event stream, and the
// session's GET stream complete through B's peer listener. On B's listener, a Kontra-Peer assertion that is forged,
// older than 30 s or replayed, a request whose upstream_sid or forwarded headers were changed after signing, are
// 401, and another path is 404; none opens a stream. Killing A, which held only the client's connection, leaves the
// session open: its next request succeeds on B.
func TestMCPPeerHop(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{"tun": {}}, Peer: true})
	ctx := ctxFor(t, 5*time.Minute)
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("echo", "")}
	up.SSE = true
	dbl := h.Double(up)
	A, B := h.Gates[0], h.Gates[1]
	h.Front.Route(B)
	fake, _ := startFakeNode(t, ctx, h, "host", map[string]FakeService{"tun": {Kind: tunnel.KindMCP, Target: dbl}})
	if r := sessionReplica(ctx, h, fake.Session); r != B.ReplicaID {
		t.Fatalf("the node's session is on %q, want B", r)
	}
	mc := NewMCPClient(A.URL+"/mcp/tun", h.APIKey)
	init, err := mc.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if init.Events == nil {
		t.Fatalf("initialize did not come as an event stream: %s", init.Text())
	}
	row := sessionRow(t, h, mc.SID)
	if row.replica != B.ReplicaID || row.tunnel != fake.Session {
		t.Fatalf("the session's row %+v: want the holder B", row)
	}
	r, err := mc.Call(ctx, 1, "echo", map[string]any{"text": "through the hop"})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.ToolText(); r.Status != http.StatusOK || r.Events == nil || text != "through the hop" {
		t.Fatalf("the pinned call: %d %s", r.Status, r.Text())
	}
	gctx, gcancel := context.WithTimeout(ctx, 30*time.Second)
	stream, err := mc.Get(gctx, "")
	if err != nil || stream.Status != http.StatusOK {
		t.Fatalf("the GET stream: %v", err)
	}
	upSIDs := up.Sessions()
	upSID := upSIDs[len(upSIDs)-1]
	if err := up.Push(upSID, []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"pushed"}}`)); err != nil {
		t.Fatal(err)
	}
	for {
		ev, err := stream.Next(gctx)
		if err != nil {
			t.Fatalf("the GET stream carried nothing through the hop: %v", err)
		}
		if strings.Contains(ev.Data, "pushed") {
			break
		}
	}
	stream.Close()
	gcancel()

	// B's peer listener, spoken to directly with the shared secret.
	signer, err := peer.NewSigner([]byte(h.PeerSecret))
	if err != nil {
		t.Fatal(err)
	}
	var tenant, buyer uuid.UUID
	if err := h.Pool.QueryRow(ctx, `select tenant_id from kontra.services where name = 'tun'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(ctx, `select buyer_id from kontra.mcp_sessions where client_sid_hash = sha256(convert_to($1, 'UTF8'))`, mc.SID).Scan(&buyer); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hop"}}}`)
	sum := sha256.Sum256([]byte(mc.SID))
	hdr := http.Header{"Accept": {"application/json, text/event-stream"}, "Content-Type": {"application/json"},
		"Mcp-Protocol-Version": {"2025-11-25"}}
	_, hh := peer.ForwardedHeaders(hdr)
	assertion := func(at time.Time) peer.Assertion {
		return peer.Assertion{Replica: A.ReplicaID, TenantID: tenant, Service: "tun", TunnelSession: uuid.MustParse(fake.Session),
			Principal: principalOf(t, h, h.APIKey), BuyerID: buyer, Via: "api_key", MCPSession: hex.EncodeToString(sum[:]),
			MCPRequestID: "9", UpstreamSID: upSID, Headers: hex.EncodeToString(hh[:]),
			Nonce: base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString())), At: at.UTC()}
	}
	sign := func(a peer.Assertion) string {
		s, err := signer.Sign(a, http.MethodPost, peer.Path, sha256.Sum256(body))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	send := func(path, assertionHeader string, extra http.Header) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+B.PeerAddr+path, bytes.NewReader(body))
		for k, v := range hdr {
			req.Header[k] = v
		}
		for k, v := range extra {
			req.Header[k] = v
		}
		if assertionHeader != "" {
			req.Header.Set(peer.Header, assertionHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	valid := sign(assertion(time.Now()))
	if st := send(peer.Path, valid, nil); st != http.StatusOK {
		t.Fatalf("a valid assertion: %d", st)
	}
	streams := preamblesFor(fake, "tun")
	calls := up.Calls("echo")
	// Tampering after signing: the payload is base64url JSON, the MAC covers it.
	tampered := func(mut func(m map[string]any)) string {
		p64, mac, _ := strings.Cut(sign(assertion(time.Now())), ".")
		raw, _ := base64.RawURLEncoding.DecodeString(p64)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mut(m)
		raw, _ = json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(raw) + "." + mac
	}
	forgedMAC := func() string {
		p64, mac, _ := strings.Cut(sign(assertion(time.Now())), ".")
		b, _ := base64.RawURLEncoding.DecodeString(mac)
		b[0] ^= 1
		return p64 + "." + base64.RawURLEncoding.EncodeToString(b)
	}
	refused := map[string]struct {
		path, header string
		extra        http.Header
		want         int
	}{
		"forged":                        {peer.Path, forgedMAC(), nil, http.StatusUnauthorized},
		"stale (40 s old)":              {peer.Path, sign(assertion(time.Now().Add(-40 * time.Second))), nil, http.StatusUnauthorized},
		"replayed":                      {peer.Path, valid, nil, http.StatusUnauthorized},
		"upstream_sid changed":          {peer.Path, tampered(func(m map[string]any) { m["upstream_sid"] = "up-other" }), nil, http.StatusUnauthorized},
		"a forwarded header changed":    {peer.Path, sign(assertion(time.Now())), http.Header{"Accept": {"text/event-stream"}}, http.StatusUnauthorized},
		"a header added after signing":  {peer.Path, sign(assertion(time.Now())), http.Header{"Mcp-Param-X": {"1"}}, http.StatusUnauthorized},
		"no assertion":                  {peer.Path, "", nil, http.StatusUnauthorized},
		"another path":                  {"/v1/other", sign(assertion(time.Now())), nil, http.StatusNotFound},
		"another path, below the route": {peer.Path + "/x", sign(assertion(time.Now())), nil, http.StatusNotFound},
	}
	for what, c := range refused {
		if st := send(c.path, c.header, c.extra); st != c.want {
			t.Fatalf("%s: %d, want %d", what, st, c.want)
		}
	}
	if n := preamblesFor(fake, "tun"); n != streams {
		t.Fatalf("the refused peer requests opened %d streams", n-streams)
	}
	if n := up.Calls("echo"); n != calls {
		t.Fatalf("the refused peer requests reached the upstream %d times", n-calls)
	}

	// A, holding only the client's connection, dies: the session stays open and works on B.
	if err := A.Kill(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(ReapAfter + 15*time.Second) // past B's reaping of A
	if row := sessionRow(t, h, mc.SID); row.closed {
		t.Fatalf("A's death closed the session: %+v", row)
	}
	r, err = mc.At(B.URL+"/mcp/tun").Call(ctx, 10, "echo", map[string]any{"text": "after A"})
	if text, _ := r.ToolText(); err != nil || r.Status != http.StatusOK || text != "after A" {
		t.Fatalf("the session's next request on B: %v %+v", err, r)
	}
}
