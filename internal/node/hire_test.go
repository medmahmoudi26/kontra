package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// hireRouter stands for the gate's routes behind NodeStreams: it answers with the request's principal and the
// session it arrived on.
type hireRouter struct {
	mu   sync.Mutex
	seen []hireSeen
}

type hireSeen struct {
	Principal string    `json:"principal"`
	Session   uuid.UUID `json:"session"`
	Path      string    `json:"path"`
}

func (h *hireRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, _, err := auth.NewAuthenticator(nil).Authenticate(r, uuid.Nil)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s, _ := tunnelgate.HiringSession(r.Context())
	seen := hireSeen{Principal: p.ID, Session: s.ID, Path: r.URL.Path}
	h.mu.Lock()
	h.seen = append(h.seen, seen)
	h.mu.Unlock()
	_ = json.NewEncoder(w).Encode(seen)
}

var hiringLineRE = regexp.MustCompile(`kontra connect: hiring at http://localhost:(\d+) `)

// localPort is the port of the node's hiring endpoint, from the line it prints.
func (tn *testNode) localPort(t *testing.T) string {
	t.Helper()
	var port string
	eventually(t, 10*time.Second, "the hiring line", func() bool {
		m := hiringLineRE.FindStringSubmatch(tn.out.String())
		if m != nil {
			port = m[1]
		}
		return m != nil
	})
	return port
}

func localGet(t *testing.T, port, path string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+path, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// A node with no --serve only hires (spec/tunnel.md §9): its local endpoint forwards on its session, where the
// gate takes the session's principal; a local refusal opens no stream. After a drain, new hiring streams go on
// the new session and none on the draining one (§13).
func TestNodeHiresOnItsCurrentSession(t *testing.T) {
	router := &hireRouter{}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Hire: router} })
	tn := mustNode(t, g, testConfig(t, g.url, "")).run(t) // no --serve
	tn.welcomed(t)
	s1 := g.session(t)
	port := tn.localPort(t)
	if strings.Contains(tn.out.String(), "7777") {
		t.Fatalf("the node bound 7777 in a test: %s", tn.out.String())
	}

	res, body := localGet(t, port, "/agents/hello", nil)
	var seen hireSeen
	if err := json.Unmarshal(body, &seen); err != nil || res.StatusCode != 200 {
		t.Fatalf("a hiring request: %d %s", res.StatusCode, body)
	}
	if seen.Principal != "node:"+s1.KeyID || seen.Session != s1.ID || seen.Path != "/agents/hello" {
		t.Fatalf("the gate saw %+v, want node:%s on session %s", seen, s1.KeyID, s1.ID)
	}

	// Local refusals open no stream.
	before := s1.NodeStreamsAccepted()
	for _, hdr := range []map[string]string{{"Host": "evil.example:" + port}, {"Origin": "http://evil.example"}} {
		if res, _ := localGet(t, port, "/mcp/fs", hdr); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%v: %d", hdr, res.StatusCode)
		}
	}
	if res, _ := localGet(t, port, "/v1/tunnel", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("a path outside the prefixes: %d", res.StatusCode)
	}
	if got := s1.NodeStreamsAccepted(); got != before {
		t.Fatalf("local refusals opened %d streams", got-before)
	}

	// Drain: the node dials a new session at once, and hiring moves to it.
	if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err != nil {
		t.Fatal(err)
	}
	s2 := g.session(t)
	before1 := s1.NodeStreamsAccepted()
	var last hireSeen
	eventually(t, 10*time.Second, "hiring on the new session", func() bool {
		_, body := localGet(t, port, "/agents/hello", nil)
		_ = json.Unmarshal(body, &last)
		return last.Session == s2.ID
	})
	for range 3 {
		_, body := localGet(t, port, "/agents/hello", nil)
		if err := json.Unmarshal(body, &last); err != nil || last.Session != s2.ID {
			t.Fatalf("after the drain a hiring request went to %s, want the new session %s", last.Session, s2.ID)
		}
	}
	if s1.NodeStreamsAccepted() != before1 {
		t.Fatalf("%d hiring streams opened on the draining session", s1.NodeStreamsAccepted()-before1)
	}
}

// auth_revoked: the node logs it and goes on hosting and hiring (the gate answers 401).
func TestNodeAuthRevokedIsLogged(t *testing.T) {
	g := newTestGate(t, nil)
	tn := mustNode(t, g, testConfig(t, g.url, "")).run(t)
	tn.welcomed(t)
	s := g.session(t)
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlAuthRevoked}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the auth_revoked line", func() bool {
		return strings.Contains(tn.logs.String(), "the gate revoked the session's hiring API key")
	})
	if !tn.running() {
		t.Fatal("auth_revoked stopped the node")
	}
}

// blobGate serves blob streams from memory for the hosting test.
type blobGate struct {
	objs map[blob.Ref][]byte
	mu   sync.Mutex
	asks []string
}

func (b *blobGate) ServeBlob(_ context.Context, _ *tunnelgate.Session, ref string, w io.Writer) error {
	b.mu.Lock()
	b.asks = append(b.asks, ref)
	b.mu.Unlock()
	obj, ok := b.objs[blob.Ref(ref)]
	if !ok {
		return blob.WriteRefusal(w, blob.ReasonForbidden)
	}
	if err := blob.WriteSize(w, int64(len(obj))); err != nil {
		return err
	}
	_, err := w.Write(obj)
	return err
}

// recordingTarget is a --serve target that records the bodies it receives.
type recordingTarget struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *recordingTarget) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, b)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
}

// Hosting (spec/tunnel.md §8.3, relay.md §10.4): a kontra-blob: part of what the gate forwards reaches the agent
// as raw bytes, fetched on a blob stream of the same session; an ordinary url part is passed as it came and never
// fetched (D56); a ref the gate refuses is 400 and the agent receives nothing.
func TestHostingInlinesBlobs(t *testing.T) {
	ref, _ := blob.NewRef()
	refused, _ := blob.NewRef()
	obj := bytes.Repeat([]byte{0xde, 0xad, 'k'}, 300<<10/3)
	bg := &blobGate{objs: map[blob.Ref][]byte{ref: obj}}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Blobs: bg} })
	target := &recordingTarget{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: target}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	tn := mustNode(t, g, testConfig(t, g.url, "", mustServe(t, "a2a:hello=http://"+ln.Addr().String()))).run(t)
	tn.welcomed(t)
	s := g.session(t)

	send := func(body string) *http.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tb, err := upstream.NewTunnel("hello", tunnel.KindA2A, sessOpener{s: s, service: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.Endpoint(tb, upstream.Info{Path: "/"}), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := tb.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
		return res
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[` +
		`{"url":"` + ref.URL() + `","mediaType":"application/octet-stream"},{"url":"http://169.254.169.254/latest/meta-data"},{"text":"hi"}]}}}`
	if res := send(body); res.StatusCode != 200 {
		t.Fatalf("the exchange: %d", res.StatusCode)
	}
	target.mu.Lock()
	got := target.bodies[0]
	target.mu.Unlock()
	var req struct {
		Params a2a.SendMessageRequest `json:"params"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("the agent received %.200s: %v", got, err)
	}
	parts := req.Params.Message.Parts
	if len(parts) != 3 || !bytes.Equal(parts[0].Raw(), obj) || parts[1].URL() != "http://169.254.169.254/latest/meta-data" || parts[2].Text() != "hi" {
		t.Fatalf("the agent's parts: %d, raw %d bytes", len(parts), len(parts[0].Raw()))
	}
	if bytes.Contains(got, []byte(blob.URIScheme)) {
		t.Fatal("the agent saw the kontra-blob: scheme")
	}
	bg.mu.Lock()
	asks := append([]string(nil), bg.asks...)
	bg.mu.Unlock()
	if len(asks) != 1 || asks[0] != string(ref) {
		t.Fatalf("blob streams for %v, want the one ref (a url part is never fetched)", asks)
	}

	// A ref the gate refuses: 400 without Kontra-Node, nothing reaches the agent.
	res := send(strings.Replace(body, ref.URL(), refused.URL(), 1))
	if res.StatusCode != http.StatusBadRequest || res.Header.Get(tunnel.HeaderNode) != "" {
		t.Fatalf("a refused ref: %d %q", res.StatusCode, res.Header.Get(tunnel.HeaderNode))
	}
	target.mu.Lock()
	n := len(target.bodies)
	target.mu.Unlock()
	if n != 1 {
		t.Fatalf("the agent received %d requests, want only the first", n)
	}
}

func mustServe(t *testing.T, v string) Serve {
	t.Helper()
	s, err := ParseServe(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
