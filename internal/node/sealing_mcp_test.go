package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
)

// hostingFor is the hosting node's sealing over a fresh job state, with one identity key.
func hostingFor(t *testing.T) (*sealing.Hosting, *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h, err := sealing.NewHosting(sealing.HostingConfig{
		Dir:        filepath.Join(t.TempDir(), "hosted", "gate.test"),
		Identities: func() []age.Identity { return []age.Identity{id} },
		KeyIDs:     func() []string { return []string{tunnel.KeyID(id.Recipient().String())} },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, id
}

// sealedCall is a sealed tools/call as a hiring node sends it, for session (its hash) and the JSON-RPC id id.
func sealedCall(t *testing.T, node *age.X25519Identity, session, tool, id, args string) (string, *age.X25519Identity, []byte) {
	t.Helper()
	job, _ := age.GenerateX25519Identity()
	rk, _ := sealed.NewRK()
	hd := sealed.Header{V: 1, Dir: sealed.DirRequest, IAT: time.Now().Unix(), Session: session, Tool: tool, MessageID: id,
		Part: 0, Parts: 1, Kind: sealed.KindData, Kids: []string{tunnel.KeyID(node.Recipient().String())},
		ReplyTo: job.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal([]age.Recipient{node.Recipient()}, hd, []byte(args))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := json.Marshal(sealed.NewMCPEnvelope(ct))
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":{},"_meta":{"sh.kontra/sealed":` +
		string(env) + `}}}`, job, rk
}

func mcpSealedPreamble(service, session, id string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindMCP, MCPSession: session, MCPRequestID: id}
}

// A hosted MCP URL service with sealing (ext-sealed.md §5, plan corrections WP-08): a sealed tools/call reaches the
// server in plaintext after the roots filter, its result goes back sealed to the job's replyTo; the same call again is
// the node's own refusal (400, Kontra-Node: sealed_rejected, replayed); a sealed call whose params carry "name" and
// "NAME" is refused bad_header and the server sees nothing (D172).
func TestMCPURLSealing(t *testing.T) {
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		var p struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(r.Params, &p)
		return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"Echo: ` + p.Arguments.Text + `"}],"isError":false}`)}
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL + "/mcp")
	pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
	hosting, node := hostingFor(t)
	pp.p.seal = hosting

	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
	resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	sum := sha256.Sum256([]byte("the gate's session id"))
	session := hex.EncodeToString(sum[:])

	call, job, rk := sealedCall(t, node, session, "echo", "1", `{"text":"MARKER-arg"}`)
	resp, body, _ := pp.raw(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "MARKER") {
		t.Fatalf("a sealed call: %d %s", resp.StatusCode, body)
	}
	if len(requestsWith(up, "MARKER-arg")) != 1 {
		t.Fatal("the server did not get the plaintext arguments")
	}
	data := body
	if evs := sseEvents(body); len(evs) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = eventData(evs[len(evs)-1])
	}
	m, err := mcpwire.Parse([]byte(data), mcpwire.FromUpstream)
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	ct, ok, err := sealed.ParseMCPResult(m.Result)
	if !ok || err != nil {
		t.Fatalf("the result is not sealed: %s", data)
	}
	hd, plain, err := sealed.Open([]age.Identity{job}, ct)
	if err != nil || sealed.VerifyAuth(rk, hd, plain) != nil || !strings.Contains(string(plain), "Echo: MARKER-arg") || hd.MessageID != "1" {
		t.Fatalf("the sealed result: %v %+v %s", err, hd, plain)
	}

	// The same call again: replayed, answered by the node.
	resp, _, _ = pp.raw(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderNode) != "sealed_rejected" || resp.Header.Get(tunnel.HeaderSealedReject) != "replayed" {
		t.Fatalf("a replayed call: %d %v", resp.StatusCode, resp.Header)
	}

	// "name" and "NAME": bad_header, the server sees nothing.
	before := len(up.Requests())
	twin, _, _ := sealedCall(t, node, session, "echo", "2", `{"text":"x"}`)
	twin = strings.Replace(twin, `"name":"echo"`, `"name":"echo","NAME":"add"`, 1)
	resp, _, _ = pp.raw(t, mcpSealedPreamble("web", session, "2"), mcpPost(twin, sid, mcpwire.V20251125))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" {
		t.Fatalf("name and NAME: %d %v", resp.StatusCode, resp.Header)
	}
	if len(up.Requests()) != before {
		t.Fatal("the server saw a refused call")
	}
}
