package mcprelay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/peer"
)

// mcp_peer_hop (§11.2, D163, D179) with two replicas in process: the node's tunnel is held by replica B and the
// client reaches replica A. initialize, a pinned tools/call and the session's GET stream (its Last-Event-ID
// included) complete through B's peer listener; the row names B, the holder; the node sees the preamble's MCP fields.
func TestPeerHop(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", `{"sh.kontra/price":"seller's"}`)}
	up.SSE = true
	n := e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	rep := c.initialize(a.url + "/mcp/tun")
	if rep.header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("initialize content type %q", rep.header.Get("Content-Type"))
	}
	row := e.row(c.sid)
	if row.replica == nil || *row.replica != b.id || row.tunnel == nil || *row.tunnel != n.session.ID {
		t.Fatalf("row %+v: replica_id must be the holder %s", row, b.id)
	}
	rep = c.call(a.url+"/mcp/tun", 7, "echo", "through the hop")
	if rep.status != 200 || !strings.Contains(string(rep.body), "through the hop") {
		t.Fatalf("call: %d %s", rep.status, rep.body)
	}
	if got := lastRequest(t, up, "tools/call"); got.SessionID != row.upstreamSID || got.Header.Get("Authorization") != "" {
		t.Errorf("upstream saw %q %q", got.SessionID, got.Header.Get("Authorization"))
	}
	// Policy and the scrub ran once, on A: the list has no seller key.
	rep = c.do(http.MethodPost, a.url+"/mcp/tun", `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`, nil)
	if rep.status != 200 || strings.Contains(string(rep.body), "sh.kontra/") || !strings.Contains(string(rep.body), "echo") {
		t.Errorf("tools/list: %s", rep.body)
	}
	// The preambles carry the client session's hash, and the call's id on tools/call.
	var call bool
	for _, p := range n.fake.Preambles() {
		if p.MCPSession != sha(c.sid) {
			t.Errorf("preamble %+v", p)
		}
		call = call || p.MCPRequestID == "7"
	}
	if !call {
		t.Error("no preamble carried the call's id")
	}
	// The GET stream, with a Last-Event-ID resume.
	resp := openGET(t, c, a.url+"/mcp/tun", up, map[string]string{"Last-Event-ID": "ev-1"})
	defer resp.Body.Close()
	if got := up.Requests(); got[len(got)-1].Header.Get("Last-Event-ID") != "ev-1" {
		t.Errorf("Last-Event-ID %q", got[len(got)-1].Header.Get("Last-Event-ID"))
	}
	if err := up.Push(row.upstreamSID, []byte(`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"message":"?"}}`)); err != nil {
		t.Fatal(err)
	}
	if evs := readEvents(t, resp, 1, 5*time.Second); len(evs) != 1 || !strings.Contains(evs[0], `"e1"`) {
		t.Errorf("GET events %q", evs)
	}
	// DELETE through the hop.
	if rep := c.do(http.MethodDelete, a.url+"/mcp/tun", "", nil); rep.status != 200 {
		t.Errorf("DELETE: %d", rep.status)
	}
	if len(up.Sessions()) != 0 {
		t.Error("the DELETE did not reach the upstream")
	}

	// B's tunnel closing ends a session pinned there: A's next request is 404 and the row closes tunnel_lost.
	c.initialize(a.url + "/mcp/tun")
	_ = n.session.Close("test: the node went away")
	eventually(t, 5*time.Second, "the row closed tunnel_lost", func() bool {
		r := e.row(c.sid)
		return r.closed && *r.reason == string(CloseTunnelLost)
	})
	if rep := c.call(a.url+"/mcp/tun", 9, "echo", "x"); rep.status != 404 {
		t.Errorf("after B's tunnel closed: %d", rep.status)
	}
}

// D213 through the peer hop: a principal's sessions are pinned to a tunnel replica B holds, and the client reaches
// replica A. Its third initialize closes its longest-idle session on A, and that session's upstream DELETE goes
// through B's peer listener before the new initialize does: the upstream ends the session.
func TestSessionsPerPrincipalThroughTheHop(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", func(s *Service) { s.MaxSessionsPerPrincipal = 2 })
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	c1, c2, c3 := newClient(t, key), newClient(t, key), newClient(t, key)
	c1.initialize(a.url + "/mcp/tun")
	first := e.row(c1.sid).upstreamSID
	c2.initialize(a.url + "/mcp/tun")
	if n := len(up.Sessions()); n != 2 {
		t.Fatalf("%d upstream sessions, want 2", n)
	}
	c3.initialize(a.url + "/mcp/tun")
	if row := e.row(c1.sid); !row.closed || *row.reason != string(CloseIdle) {
		t.Fatalf("the longest-idle session over the cap: %+v", row)
	}
	if slices.Contains(up.Sessions(), first) || len(up.Sessions()) != 2 {
		t.Fatalf("the evicted session's upstream DELETE did not go through the hop: %v (evicted %s)", up.Sessions(), first)
	}
	if rep := c1.call(a.url+"/mcp/tun", 1, "echo", "x"); rep.status != http.StatusNotFound {
		t.Errorf("the evicted session: %d", rep.status)
	}
	for _, c := range []*client{c2, c3} {
		if rep := c.call(a.url+"/mcp/tun", 2, "echo", "x"); rep.status != http.StatusOK {
			t.Errorf("a session within the cap: %d %s", rep.status, rep.body)
		}
	}
}

// A GET stream whose upstream sends its headers and nothing else (the stdio bridge's, until a message comes) reaches
// the client through the peer hop at once: the holding replica flushes the upstream's headers before any body
// (M3 WP-09: stdio_roots' GET through the hop waited for a first event that never came).
func TestPeerHopIdleGET(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	up.NoPriming = true
	e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(a.url + "/mcp/tun")
	row := e.row(c.sid)
	if row.replica == nil || *row.replica != b.id {
		t.Fatalf("row %+v: want the holder %s", row, b.id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.url+"/mcp/tun", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Mcp-Session-Id", c.sid)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the GET's headers did not come through the hop within 5 s: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET: %d %v", resp.StatusCode, resp.Header)
	}
	if err := up.Push(row.upstreamSID, []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"late"}}`)); err != nil {
		t.Fatal(err)
	}
	if evs := readEvents(t, resp, 1, 5*time.Second); len(evs) != 1 || !strings.Contains(evs[0], "late") {
		t.Fatalf("GET events %q", evs)
	}
}

// The holder's refusal, when only the hop knows: a binding revoked while B's registry is deaf to it (its stream
// check reads the view) closes A's row revoked through peer.HeaderGone, and opens no stream.
func TestPeerHopGone(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true, unsubscribed: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(a.url + "/mcp/tun")
	if err := e.store.RevokeBinding(context.Background(), n.key, "tun", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "B's view holds the binding revoked", func() bool { return b.rev.BindingRevoked(n.key, "tun") })
	streams := len(n.fake.Preambles())
	if rep := c.call(a.url+"/mcp/tun", 1, "echo", "x"); rep.status != 404 {
		t.Errorf("%d %s", rep.status, rep.body)
	}
	if len(n.fake.Preambles()) != streams {
		t.Error("a stream was opened")
	}
	if row := e.row(c.sid); !row.closed || *row.reason != string(CloseRevoked) {
		t.Errorf("row %+v", row)
	}
}

// §11.4: a calls row is written when the call's response completes, so a tools/call no answer came back for has
// none, wherever the exchange failed: §6.1's check refusing the session on the replica holding the tunnel, reached
// through the peer hop (peer.HeaderGone) or on this replica, and an upstream that cannot be reached. A call that
// was answered is metered, and the client gets 404 or 502 either way.
func TestNoCallsRowWithoutAnAnswer(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	a := e.replica("a", replicaOpts{peer: true})
	b := e.replica("b", replicaOpts{peer: true, unsubscribed: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	viaPeer, local := newClient(t, key), newClient(t, key)
	viaPeer.initialize(a.url + "/mcp/tun")
	local.initialize(b.url + "/mcp/tun")
	// Each replica's tools cache is warm, so the next calls are dispatched without a drain.
	if rep := viaPeer.call(a.url+"/mcp/tun", 1, "echo", "x"); rep.status != 200 {
		t.Fatalf("through the peer hop: %d %s", rep.status, rep.body)
	}
	if rep := local.call(b.url+"/mcp/tun", 1, "echo", "x"); rep.status != 200 {
		t.Fatalf("on the holder: %d %s", rep.status, rep.body)
	}
	if err := e.store.RevokeBinding(context.Background(), n.key, "tun", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "B's view holds the binding revoked", func() bool { return b.rev.BindingRevoked(n.key, "tun") })
	if rep := viaPeer.call(a.url+"/mcp/tun", 2, "echo", "x"); rep.status != 404 {
		t.Errorf("refused by the holder through the hop: %d %s", rep.status, rep.body)
	}
	if rep := local.call(b.url+"/mcp/tun", 2, "echo", "x"); rep.status != 404 {
		t.Errorf("refused on this replica: %d %s", rep.status, rep.body)
	}
	if rows := e.calls("tun"); len(rows) != 2 {
		t.Errorf("%d calls rows, want the 2 answered calls'", len(rows))
	}

	// A direct backend that went away.
	gone := mcptest.New()
	gone.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	srv := httptest.NewServer(gone)
	t.Cleanup(srv.Close)
	e.service("gone", srv.URL+"/mcp", nil)
	c := newClient(t, key)
	c.initialize(a.url + "/mcp/gone")
	if rep := c.call(a.url+"/mcp/gone", 1, "echo", "x"); rep.status != 200 {
		t.Fatalf("direct: %d %s", rep.status, rep.body)
	}
	srv.Close()
	if rep := c.call(a.url+"/mcp/gone", 2, "echo", "x"); rep.status != 502 || rep.errCode(t) != -32603 {
		t.Errorf("an unreachable upstream: %d %s", rep.status, rep.body)
	}
	if rows := e.calls("gone"); len(rows) != 1 || rows[0].isError == nil || *rows[0].isError {
		t.Errorf("calls rows %+v, want the answered call's only", rows)
	}
}

// The peer listener refuses a forged, stale (31 s) or replayed Kontra-Peer, a request whose Mcp-Name, Last-Event-ID
// or Mcp-Param-* changed after signing, and one whose UpstreamSID was swapped for another session's (the HMAC): 401,
// and no stream is opened. Any other path is 404. A header added after signing is not forwarded upstream.
func TestPeerListenerRefusals(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	b := e.replica("b", replicaOpts{peer: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	n := e.node(b, up, "tun")
	key, principal := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(b.url + "/mcp/tun")
	upSID := e.row(c.sid).upstreamSID
	signer, _ := peer.NewSigner(peerSecret)
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	assertion := func(at time.Time, sid string, hdr http.Header) (peer.Assertion, http.Header) {
		fwd, sum := peer.ForwardedHeaders(hdr)
		return peer.Assertion{Replica: "r-other", TenantID: e.team, Service: "tun", TunnelSession: n.session.ID,
			Principal: principal, BuyerID: uuid.New(), Via: "api_key", MCPSession: sha(c.sid), UpstreamSID: sid,
			Headers: hex.EncodeToString(sum[:]), Nonce: uuid.NewString(), At: at}, fwd
	}
	base := http.Header{}
	base.Set("Content-Type", "application/json")
	base.Set("Accept", "application/json, text/event-stream")
	base.Set("MCP-Protocol-Version", "2025-11-25")
	base.Set("Mcp-Name", "echo")
	base.Set("Last-Event-ID", "ev-1")
	base.Set("Mcp-Param-Region", "eu")
	sign := func(a peer.Assertion, fwd http.Header, b string) http.Header {
		v, err := signer.Sign(a, http.MethodPost, peer.Path, sha256.Sum256([]byte(b)))
		if err != nil {
			t.Fatal(err)
		}
		h := fwd.Clone()
		h.Set(peer.Header, v)
		return h
	}
	send := func(path string, h http.Header, b string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, b2url(path), strings.NewReader(b))
		req.Header = h
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	b2url = func(path string) string { return b.peerURL + path }

	a, fwd := assertion(time.Now(), upSID, base)
	good := sign(a, fwd, body)
	streams := len(n.fake.Preambles())
	if st, out := send(peer.Path, good, body); st != 200 || !strings.Contains(out, `"result"`) {
		t.Fatalf("a signed request: %d %s", st, out)
	}
	if len(n.fake.Preambles()) != streams+1 {
		t.Fatal("the signed request opened no stream")
	}
	if got := lastRequest(t, up, "ping"); got.SessionID != upSID || got.Header.Get("Mcp-Param-Region") != "eu" {
		t.Errorf("upstream saw %q %v", got.SessionID, got.Header)
	}
	streams++
	refused := func(what string, st int) {
		t.Helper()
		if st != http.StatusUnauthorized {
			t.Errorf("%s: %d", what, st)
		}
		if len(n.fake.Preambles()) != streams {
			t.Errorf("%s opened a stream", what)
		}
	}
	st, _ := send(peer.Path, good, body)
	refused("replayed", st)
	forged := good.Clone()
	forged.Set(peer.Header, "eyJ4IjoxfQ.AAAA")
	st, _ = send(peer.Path, forged, body)
	refused("forged", st)
	a, fwd = assertion(time.Now().Add(-31*time.Second), upSID, base)
	st, _ = send(peer.Path, sign(a, fwd, body), body)
	refused("stale (31 s)", st)
	for _, k := range []string{"Mcp-Name", "Last-Event-ID", "Mcp-Param-Region"} {
		a, fwd = assertion(time.Now(), upSID, base)
		h := sign(a, fwd, body)
		h.Set(k, "changed")
		st, _ = send(peer.Path, h, body)
		refused(k+" changed after signing", st)
	}
	// The UpstreamSID swapped for another session's, the MAC kept: the assertion's bytes no longer verify.
	other := newClient(t, key)
	other.initialize(b.url + "/mcp/tun")
	a, fwd = assertion(time.Now(), upSID, base)
	h := sign(a, fwd, body)
	payload, mac, _ := strings.Cut(h.Get(peer.Header), ".")
	raw, _ := b64.DecodeString(payload)
	swapped := strings.Replace(string(raw), upSID, e.row(other.sid).upstreamSID, 1)
	h.Set(peer.Header, b64.EncodeToString([]byte(swapped))+"."+mac)
	streams = len(n.fake.Preambles())
	st, _ = send(peer.Path, h, body)
	refused("UpstreamSID swapped", st)
	// Another path: 404.
	for _, p := range []string{"/", "/mcp/tun", "/v1/peer/mcp/x", "/healthz"} {
		a, fwd = assertion(time.Now(), upSID, base)
		if st, _ := send(p, sign(a, fwd, body), body); st != 404 {
			t.Errorf("%s: %d", p, st)
		}
	}
	// A header added after signing reaches nothing upstream.
	a, fwd = assertion(time.Now(), upSID, base)
	h = sign(a, fwd, body)
	h.Set("X-Smuggled", "1")
	h.Set("Authorization", "Bearer stolen")
	h.Set("Mcp-Session-Id", "up-other")
	if st, _ := send(peer.Path, h, body); st != 200 {
		t.Fatalf("extra headers: %d", st)
	}
	got := lastRequest(t, up, "ping")
	if got.Header.Get("X-Smuggled") != "" || got.Header.Get("Authorization") != "" || got.SessionID != upSID {
		t.Errorf("an unsigned header reached the upstream: %v", got.Header)
	}
}

var b2url func(string) string

// §11.2: without peer.listen a replica picks only its own sessions; a session pinned to a tunnel another replica
// holds is 404 there, and its row stays open.
func TestWithoutPeerOnlyLocal(t *testing.T) {
	e := newEnv(t)
	e.service("tun", "", nil)
	a := e.replica("a", replicaOpts{})
	b := e.replica("b", replicaOpts{peer: true})
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.node(b, up, "tun")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	rep := c.do(http.MethodPost, a.url+"/mcp/tun", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, nil)
	if rep.status != 503 || up.Count("initialize") != 0 {
		t.Errorf("A without a session of its own: %d %s (upstream initializes %d)", rep.status, rep.body, up.Count("initialize"))
	}
	c.initialize(b.url + "/mcp/tun")
	if rep := c.call(a.url+"/mcp/tun", 1, "echo", "x"); rep.status != 404 {
		t.Errorf("a session held by B, on A: %d", rep.status)
	}
	if row := e.row(c.sid); row.closed {
		t.Errorf("the row closed: %+v", row)
	}
	// With a session of its own, A picks it.
	up2 := mcptest.New()
	n2 := e.node(a, up2, "tun")
	c2 := newClient(t, key)
	c2.initialize(a.url + "/mcp/tun")
	if row := e.row(c2.sid); row.replica == nil || *row.replica != a.id || *row.tunnel != n2.session.ID || up2.Count("initialize") != 1 {
		t.Errorf("A's own session was not picked: %+v", row)
	}
	// A with peer.listen prefers its own session to B's.
	a2 := e.replica("a2", replicaOpts{peer: true})
	up3 := mcptest.New()
	e.node(a2, up3, "tun")
	for range 3 {
		c3 := newClient(t, key)
		c3.initialize(a2.url + "/mcp/tun")
		if row := e.row(c3.sid); *row.replica != a2.id {
			t.Errorf("A2 picked %s's session over its own", *row.replica)
		}
	}
}

var b64 = base64RawURL{}

type base64RawURL struct{}

func (base64RawURL) DecodeString(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
func (base64RawURL) EncodeToString(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
