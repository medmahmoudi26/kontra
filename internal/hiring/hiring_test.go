package hiring

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/tunnel"
)

// fakeTunnel is the node's tunnel seen from the hiring server: each OpenHire is a pipe whose other end a fake
// gate serves (one HTTP/1.1 exchange behind the preamble), each OpenBlob a pipe answered from blobs.
type fakeTunnel struct {
	gate  http.Handler
	blobs map[blob.Ref][]byte

	mu        sync.Mutex
	hires     int
	blobOpens int
	preambles []tunnel.Preamble
	requests  []*http.Request
	fail      error
}

func (f *fakeTunnel) counts() (hires, blobs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hires, f.blobOpens
}

func (f *fakeTunnel) OpenHire(context.Context) (net.Conn, error) {
	f.mu.Lock()
	if f.fail != nil {
		f.mu.Unlock()
		return nil, f.fail
	}
	f.hires++
	f.mu.Unlock()
	node, gate := net.Pipe()
	go f.serveHTTP(gate)
	if err := tunnel.WritePreamble(node, tunnel.Preamble{Op: tunnel.OpHTTP}); err != nil {
		return nil, err
	}
	return node, nil
}

func (f *fakeTunnel) serveHTTP(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	p, err := tunnel.ReadPreamble(br)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.preambles = append(f.preambles, p)
	f.mu.Unlock()
	l := &oneConn{c: &readConn{Conn: c, r: br}, done: make(chan struct{})}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.mu.Unlock()
		f.gate.ServeHTTP(w, r)
	})}
	srv.SetKeepAlivesEnabled(false)
	_ = srv.Serve(l)
}

func (f *fakeTunnel) OpenBlob(context.Context) (net.Conn, error) {
	f.mu.Lock()
	f.blobOpens++
	f.mu.Unlock()
	node, gate := net.Pipe()
	go func() {
		defer gate.Close()
		p, err := tunnel.ReadPreamble(bufio.NewReader(gate))
		if err != nil || p.Op != tunnel.OpBlob {
			return
		}
		b, ok := f.blobs[blob.Ref(p.Ref)]
		if !ok {
			_ = blob.WriteRefusal(gate, blob.ReasonNotFound)
			return
		}
		_ = blob.WriteSize(gate, int64(len(b)))
		_, _ = gate.Write(b)
	}()
	return node, nil
}

type readConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type oneConn struct {
	c    net.Conn
	once sync.Once
	took atomic.Bool
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	if l.took.CompareAndSwap(false, true) {
		return &closeNotify{Conn: l.c, l: l}, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{} }

type closeNotify struct {
	net.Conn
	l *oneConn
}

func (c *closeNotify) Close() error { _ = c.l.Close(); return c.Conn.Close() }

// fakeGate is the gate's routes as a hiring node reaches them: a signed card for hello (the gate's interfaces,
// at its public URL), the JWKS, a JSON-RPC echo, a task with a kontra-blob: part, an SSE stream with one.
type fakeGate struct {
	public string // http://127.0.0.1:<port> the node dialed
	keys   *jws.KeySet
	card   []byte
	jwks   http.Handler
	tamper atomic.Bool // alter the card in transit after signing
	jkus   atomic.Value
	ref    blob.Ref
	jwksN  atomic.Int32
	// cardType replaces the card's Content-Type (application/json) when set; cardBody the signed card.
	cardType atomic.Pointer[string]
	cardBody atomic.Pointer[[]byte]
}

func newFakeGate(t *testing.T, public string) *fakeGate {
	t.Helper()
	keys, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := card.JWKSHandler(keys)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := blob.NewRef()
	g := &fakeGate{public: public, keys: keys, jwks: jwks, ref: ref}
	g.jkus.Store(public + jws.JWKSPath)
	return g
}

func (g *fakeGate) signedCard(t *testing.T) []byte {
	t.Helper()
	unsigned := `{"name":"hello","description":"says hello","version":"1.0.0",` +
		`"supportedInterfaces":[{"url":"` + g.public + `/agents/hello","protocolBinding":"JSONRPC","protocolVersion":"1.0"},` +
		`{"url":"` + g.public + `/agents/hello","protocolBinding":"HTTP+JSON","protocolVersion":"1.0"}],` +
		`"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
	sig, err := g.keys.Sign([]byte(unsigned), g.jkus.Load().(string), &jws.Upstream{SHA256: strings.Repeat("0", 64), URL: g.public + "/x", Verified: false})
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal([]jws.Signature{sig})
	return []byte(strings.TrimSuffix(unsigned, "}") + `,"signatures":` + string(sb) + `}`)
}

func (g *fakeGate) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	serveCard := func(w http.ResponseWriter, r *http.Request) {
		c := g.signedCard(t)
		if b := g.cardBody.Load(); b != nil {
			c = *b
		}
		if g.tamper.Load() {
			c = bytes.Replace(c, []byte(`"says hello"`), []byte(`"says hullo"`), 1)
		}
		ct := "application/json"
		if v := g.cardType.Load(); v != nil {
			ct = *v
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("ETag", `"gate"`)
		_, _ = w.Write(c)
	}
	mux.HandleFunc("GET /agents/hello", serveCard)
	mux.HandleFunc("GET /agents/hello/.well-known/agent-card.json", serveCard)
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		g.jwksN.Add(1)
		g.jwks.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /agents/hello/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tasks":[],"nextPageToken":""}`)
	})
	mux.HandleFunc("GET /agents/hello/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"artifactId":"a",`+
			`"parts":[{"url":"`+g.ref.URL()+`","mediaType":"application/octet-stream","filename":"big.bin"}]}]}`)
	})
	mux.HandleFunc("GET /agents/hello/tasks/{id}/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: {\"artifactUpdate\":{\"artifact\":{\"artifactId\":\"a\",\"parts\":[{\"url\":\""+g.ref.URL()+"\"}]}}}\n\n")
	})
	echo := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	mux.HandleFunc("POST /agents/hello", echo)
	mux.HandleFunc("POST /mcp/fs", echo)
	mux.HandleFunc("GET /mcp/fs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: "+mcpTaskAnswer+"\n\n")
	})
	return mux
}

// mcpTaskAnswer is an MCP answer (a CreateTaskResult) whose text mentions the kontra-blob: scheme: its task's
// status is a string, which an A2A body never has.
const mcpTaskAnswer = `{"jsonrpc":"2.0","id":3,"result":{"task":{"taskId":"t1","status":"completed","statusMessage":"wrote kontra-blob: notes"}}}`

type testServer struct {
	s      *Server
	tun    *fakeTunnel
	gate   *fakeGate
	url    string // http://127.0.0.1:<port>
	host   string // 127.0.0.1:<port>
	port   string
	cancel context.CancelFunc
}

func startServer(t *testing.T, mod func(*Config)) *testServer {
	t.Helper()
	return startServerAt(t, "127.0.0.1:0", mod)
}

// startServerOn is startServer listening on addr ([::1]:0, say).
func startServerOn(t *testing.T, addr string) *testServer {
	t.Helper()
	return startServerAt(t, addr, nil)
}

func startServerAt(t *testing.T, addr string, mod func(*Config)) *testServer {
	t.Helper()
	gateURL, _ := url.Parse("http://127.0.0.1:18443")
	g := newFakeGate(t, "http://127.0.0.1:18443")
	big := bytes.Repeat([]byte{7, 0, 'b'}, 300<<10/3)
	tun := &fakeTunnel{gate: g.handler(t), blobs: map[blob.Ref][]byte{g.ref: big}}
	cfg := Config{Listen: addr, Gate: gateURL}
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg, tun, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	eventually(t, func() bool { return s.Local().Port() == port })
	return &testServer{s: s, tun: tun, gate: g, url: "http://" + ln.Addr().String(), host: ln.Addr().String(), port: port, cancel: cancel}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

func (ts *testServer) do(t *testing.T, method, path, host string, hdr http.Header, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// CheckLocal (spec/tunnel.md §9): Host must be one of --listen's loopback names with its port, an Origin must be
// listed, OPTIONS is 405, and with a local token every request carries it.
func TestCheckLocal(t *testing.T) {
	cfg := Config{Listen: "127.0.0.1:7777", AllowOrigins: []string{"http://app.local:3000"}}
	for _, tc := range []struct {
		host, origin, method, authz string
		cfg                         *Config
		want                        int
	}{
		{host: "127.0.0.1:7777", want: 0},
		{host: "localhost:7777", want: 0},
		{host: "LOCALHOST:7777", want: 0},
		{host: "[::1]:7777", want: 0},
		{host: "evil.example:7777", want: 403},
		{host: "127.0.0.1:7778", want: 403},
		{host: "127.0.0.1", want: 403},
		{host: "localhost.:7777", want: 403},
		{host: "127.0.0.2:7777", want: 403},
		{host: "localhost:7777", origin: "http://evil.example:7777", want: 403},
		{host: "localhost:7777", origin: "null", want: 403},
		{host: "localhost:7777", origin: "http://app.local:3000", want: 0},
		{host: "localhost:7777", origin: "HTTP://APP.local:3000", want: 0},
		{host: "localhost:7777", origin: "http://app.local:3001", want: 403},
		{host: "localhost:7777", method: http.MethodOptions, want: 405},
		{host: "localhost:7777", method: http.MethodOptions, origin: "http://evil.example", want: 403},
		{host: "localhost:7777", cfg: &Config{Listen: "127.0.0.1:7777", LocalToken: "0123456789abcdefXYZ"}, want: 401},
		{host: "localhost:7777", authz: "Bearer wrong-token-0123456789", cfg: &Config{Listen: "127.0.0.1:7777", LocalToken: "0123456789abcdefXYZ"}, want: 401},
		{host: "localhost:7777", authz: "Bearer 0123456789abcdefXYZ", cfg: &Config{Listen: "127.0.0.1:7777", LocalToken: "0123456789abcdefXYZ"}, want: 0},
		{host: "10.1.2.3:7777", authz: "Bearer 0123456789abcdefXYZ", cfg: &Config{Listen: "10.1.2.3:7777", LocalToken: "0123456789abcdefXYZ"}, want: 0},
		{host: "evil.example:7777", authz: "Bearer 0123456789abcdefXYZ", cfg: &Config{Listen: "0.0.0.0:7777", LocalToken: "0123456789abcdefXYZ"}, want: 403},
	} {
		r, _ := http.NewRequest(tc.method, "http://x/agents/hello", nil)
		if tc.method == "" {
			r.Method = http.MethodGet
		}
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.authz != "" {
			r.Header.Set("Authorization", tc.authz)
		}
		c := cfg
		if tc.cfg != nil {
			c = *tc.cfg
		}
		got, err := CheckLocal(r, c)
		if got != tc.want || (tc.want == 0) != (err == nil) {
			t.Errorf("%+v: %d %v, want %d", tc, got, err, tc.want)
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "http://x/agents/hello", nil)
	r.Host = "localhost:7777"
	r.Header.Add("Origin", "http://app.local:3000")
	r.Header.Add("Origin", "http://evil.example")
	if got, _ := CheckLocal(r, cfg); got != 403 {
		t.Errorf("two Origin headers: %d", got)
	}
}

// local_rebinding (spec/tunnel.md §16): a foreign Host or Origin on /mcp/…, /agents/… and /.well-known/… is 403
// (an id-less JSON-RPC error on MCP paths) and opens no stream; so are OPTIONS (405) and a path outside the four
// prefixes (404).
func TestLocalRebinding(t *testing.T) {
	ts := startServer(t, nil)
	for _, path := range []string{"/mcp/fs", "/mcp", "/agents/hello", "/.well-known/jwks.json"} {
		for _, tc := range []struct {
			host   string
			origin string
		}{{host: "evil.example:" + ts.port}, {host: "localhost:" + ts.port, origin: "http://evil.example:" + ts.port}} {
			hdr := http.Header{}
			if tc.origin != "" {
				hdr.Set("Origin", tc.origin)
			}
			res, body := ts.do(t, http.MethodPost, path, tc.host, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %+v: %d, want 403", path, tc, res.StatusCode)
			}
			if strings.HasPrefix(path, "/mcp") {
				var e map[string]json.RawMessage
				if err := json.Unmarshal(body, &e); err != nil || e["error"] == nil || e["id"] != nil || string(e["jsonrpc"]) != `"2.0"` {
					t.Fatalf("%s: the 403's body %s, want a JSON-RPC error without id", path, body)
				}
			}
		}
	}
	if res, _ := ts.do(t, http.MethodOptions, "/agents/hello", "", http.Header{"Origin": {"http://evil.example"}}, ""); res.StatusCode != 403 {
		t.Fatalf("a preflight from a foreign origin: %d", res.StatusCode)
	}
	if res, _ := ts.do(t, http.MethodOptions, "/agents/hello", "", nil, ""); res.StatusCode != 405 {
		t.Fatalf("OPTIONS: %d, want 405", res.StatusCode)
	}
	for _, path := range []string{"/", "/v1/tunnel", "/healthz", "/agentsx", "/mcpx"} {
		if res, _ := ts.do(t, http.MethodGet, path, "", nil, ""); res.StatusCode != 404 {
			t.Fatalf("%s: %d, want 404", path, res.StatusCode)
		}
	}
	if hires, blobs := ts.tun.counts(); hires != 0 || blobs != 0 {
		t.Fatalf("refused requests opened %d hiring and %d blob streams, want none", hires, blobs)
	}
}

// New refuses --listen off loopback without --local-token.
func TestNewChecksListen(t *testing.T) {
	gate, _ := url.Parse("https://gate.example")
	for _, l := range []string{"0.0.0.0:7777", "10.1.2.3:7777", "[::]:7777", "example.com:7777"} {
		if _, err := New(Config{Listen: l, Gate: gate}, &fakeTunnel{}, nil); err == nil || !strings.Contains(err.Error(), "--local-token") {
			t.Errorf("--listen %s without a token: %v", l, err)
		}
		if _, err := New(Config{Listen: l, Gate: gate, LocalToken: "0123456789abcdef"}, &fakeTunnel{}, nil); err != nil {
			t.Errorf("--listen %s with a token: %v", l, err)
		}
	}
	for _, l := range []string{"127.0.0.1:7777", "localhost:7777", "[::1]:0"} {
		if _, err := New(Config{Listen: l, Gate: gate}, &fakeTunnel{}, nil); err != nil {
			t.Errorf("--listen %s: %v", l, err)
		}
	}
	if _, err := New(Config{Listen: "7777", Gate: gate}, &fakeTunnel{}, nil); err == nil {
		t.Error("--listen without a host accepted")
	}
	if _, err := New(Config{Gate: gate, AllowOrigins: []string{"http://x/path"}}, &fakeTunnel{}, nil); err == nil {
		t.Error("an --allow-origin with a path accepted")
	}
	// --listen off (D208): accepted alone; a local token or an allowed Origin, which guard --listen, are refused.
	if _, err := New(Config{Listen: ListenOff, Gate: gate}, &fakeTunnel{}, nil); err != nil {
		t.Errorf("--listen off: %v", err)
	}
	if _, err := New(Config{Listen: ListenOff, Gate: gate, LocalToken: "0123456789abcdef"}, &fakeTunnel{}, nil); err == nil {
		t.Error("--listen off with a local token accepted")
	}
	if _, err := New(Config{Listen: ListenOff, Gate: gate, AllowOrigins: []string{"http://app.local:3000"}}, &fakeTunnel{}, nil); err == nil {
		t.Error("--listen off with an allowed Origin accepted")
	}
}

// Forwarding: the path unchanged on a stream with preamble {"v":1,"op":"http"}; the local token, Authorization,
// cookies and a disallowed Origin stay on the node; an allowed Origin goes on.
func TestForwarding(t *testing.T) {
	const token = "0123456789abcdef-local"
	ts := startServer(t, func(c *Config) { c.LocalToken = token; c.AllowOrigins = []string{"http://app.local:3000"} })
	hdr := http.Header{"Authorization": {"Bearer " + token}, "Cookie": {"s=1"}, "Origin": {"http://app.local:3000"},
		"X-Forwarded-For": {"1.2.3.4"}, "Content-Type": {"application/json"}}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	res, got := ts.do(t, http.MethodPost, "/mcp/fs?x=1", "", hdr, body)
	if res.StatusCode != 200 || string(got) != body {
		t.Fatalf("forwarded: %d %s", res.StatusCode, got)
	}
	ts.tun.mu.Lock()
	pre, req := ts.tun.preambles[0], ts.tun.requests[0]
	ts.tun.mu.Unlock()
	if pre != (tunnel.Preamble{V: tunnel.Version, Op: tunnel.OpHTTP}) {
		t.Fatalf("preamble %+v", pre)
	}
	if req.URL.Path != "/mcp/fs" || req.URL.RawQuery != "x=1" || req.Host != "127.0.0.1:18443" {
		t.Fatalf("the gate saw %s %s?%s Host %s", req.Method, req.URL.Path, req.URL.RawQuery, req.Host)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Forwarded"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("the gate saw %s: %q", h, v)
		}
	}
	if req.Header.Get("Origin") != "http://app.local:3000" {
		t.Errorf("an allowed Origin was not forwarded: %q", req.Header.Get("Origin"))
	}
	// Without the token: 401, no stream.
	before, _ := ts.tun.counts()
	if res, _ := ts.do(t, http.MethodPost, "/mcp/fs", "", nil, body); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the local token: %d", res.StatusCode)
	}
	if after, _ := ts.tun.counts(); after != before {
		t.Fatal("a request without the local token opened a stream")
	}
	// A disallowed origin is refused before; a request without Origin carries none.
	_, _ = ts.do(t, http.MethodPost, "/mcp/fs", "", http.Header{"Authorization": {"Bearer " + token}}, body)
	ts.tun.mu.Lock()
	last := ts.tun.requests[len(ts.tun.requests)-1]
	ts.tun.mu.Unlock()
	if last.Header.Get("Origin") != "" {
		t.Error("an Origin appeared")
	}
}

// No session: 503, saying so. No slot for one more node-opened stream (spec/tunnel.md §14): 429 with Retry-After.
func TestNoSession(t *testing.T) {
	ts := startServer(t, nil)
	ts.tun.mu.Lock()
	ts.tun.fail = errors.New("draining")
	ts.tun.mu.Unlock()
	if res, _ := ts.do(t, http.MethodPost, "/agents/hello", "", nil, "{}"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no session: %d", res.StatusCode)
	}
	ts.tun.mu.Lock()
	ts.tun.fail = fmt.Errorf("node: %w", ErrBusy)
	ts.tun.mu.Unlock()
	if res, _ := ts.do(t, http.MethodPost, "/agents/hello", "", nil, "{}"); res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("no slot: %d Retry-After %q, want 429 with Retry-After", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

// Cards (spec/tunnel.md §9, relay.md §3): the card served locally verifies with the dialed origin's key set
// (fetched through the tunnel, cached) and lists only localhost URLs; a card altered in transit, signed for
// another jku, or unsigned is not served (502); an answer that is no card on a card route passes. An a2a-go
// client resolves the local card and talks to localhost.
func TestCards(t *testing.T) {
	ts := startServer(t, nil)
	for _, path := range []string{"/agents/hello", "/agents/hello/.well-known/agent-card.json"} {
		res, body := ts.do(t, http.MethodGet, path, "", nil, "")
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
		var c a2a.AgentCard
		if err := json.Unmarshal(body, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.SupportedInterfaces) != 2 {
			t.Fatalf("%s: interfaces %+v", path, c.SupportedInterfaces)
		}
		for _, i := range c.SupportedInterfaces {
			if i.URL != "http://localhost:"+ts.port+"/agents/hello" {
				t.Fatalf("%s: interface URL %q, want only localhost URLs", path, i.URL)
			}
		}
		if bytes.Contains(body, []byte("127.0.0.1:18443")) || len(c.Signatures) != 0 || res.Header.Get("ETag") != "" {
			t.Fatalf("%s: the gate's URL, a signature that no longer covers the card, or the gate's ETag survived: %s", path, body)
		}
		if c.Name != "hello" || c.Description != "says hello" {
			t.Fatalf("the card's other members changed: %+v", c)
		}
	}
	if n := ts.gate.jwksN.Load(); n != 1 {
		t.Fatalf("the key set was fetched %d times, want once (cached)", n)
	}
	// Not a card on a card-shaped route (ListTasks on a single-tenant gate): passed as it came.
	if res, body := ts.do(t, http.MethodGet, "/agents/hello/tasks", "", nil, ""); res.StatusCode != 200 || !strings.Contains(string(body), `"tasks"`) {
		t.Fatalf("ListTasks: %d %s", res.StatusCode, body)
	}

	// a2a-go resolves the local card and calls localhost.
	resolved, err := agentcard.DefaultResolver.Resolve(context.Background(), ts.url+"/agents/hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2aclient.NewFromCard(context.Background(), resolved); err != nil {
		t.Fatalf("a2a-go cannot use the local card: %v", err)
	}

	// Altered in transit.
	ts.gate.tamper.Store(true)
	if res, body := ts.do(t, http.MethodGet, "/agents/hello", "", nil, ""); res.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "hullo") {
		t.Fatalf("a tampered card: %d %s, want 502 and nothing of it", res.StatusCode, body)
	}
	ts.gate.tamper.Store(false)
	// Signed for another origin's key set (a gate relaying another's card).
	ts.gate.jkus.Store("http://127.0.0.1:9/.well-known/jwks.json")
	if res, _ := ts.do(t, http.MethodGet, "/agents/hello", "", nil, ""); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("a card whose jku is another origin: %d", res.StatusCode)
	}
	ts.gate.jkus.Store(ts.gate.public + jws.JWKSPath)

	// Whatever its Content-Type: a2a-go decodes any answer as a card (encoding/json), so the node reads every
	// GET answer on a card route. The gate's own card served as text/plain is verified and rewritten; a tampered
	// one is not served, and a2a-go resolves nothing.
	resolves := func() bool {
		_, err := agentcard.DefaultResolver.Resolve(context.Background(), ts.url+"/agents/hello")
		return err == nil
	}
	plain := "text/plain"
	ts.gate.cardType.Store(&plain)
	if res, body := ts.do(t, http.MethodGet, "/agents/hello", "", nil, ""); res.StatusCode != 200 ||
		bytes.Contains(body, []byte("127.0.0.1:18443")) || !bytes.Contains(body, []byte("http://localhost:"+ts.port+"/agents/hello")) {
		t.Fatalf("the gate's card as text/plain: %d %.300s, want it verified and rewritten", res.StatusCode, body)
	}
	ts.gate.tamper.Store(true)
	if res, body := ts.do(t, http.MethodGet, "/agents/hello", "", nil, ""); res.StatusCode != http.StatusBadGateway ||
		strings.Contains(string(body), "hullo") || strings.Contains(string(body), "127.0.0.1:18443") {
		t.Fatalf("a tampered card as text/plain: %d %.300s, want 502 and nothing of it", res.StatusCode, body)
	}
	if resolves() {
		t.Fatal("a2a-go resolved a tampered card served as text/plain")
	}
	ts.gate.tamper.Store(false)
	// A forged, unsigned card whose members are case variants of a card's (encoding/json matches them anyway), as
	// JSON and as text/plain; and an event stream on a card route.
	forged := []byte(`{"name":"hello","description":"forged","version":"1.0.0",` +
		`"SupportedInterfaces":[{"url":"http://attacker.example/agents/hello","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
		`"Capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"Skills":[]}`)
	ts.gate.cardBody.Store(&forged)
	for _, ct := range []string{"application/json", "text/plain", "text/event-stream"} {
		ts.gate.cardType.Store(&ct)
		if res, body := ts.do(t, http.MethodGet, "/agents/hello", "", nil, ""); res.StatusCode != http.StatusBadGateway ||
			strings.Contains(string(body), "attacker.example") {
			t.Fatalf("a forged card with case-variant members as %s: %d %.300s, want 502", ct, res.StatusCode, body)
		}
		if resolves() {
			t.Fatalf("a2a-go resolved a forged card served as %s through the node", ct)
		}
	}
	ts.gate.cardBody.Store(nil)
	ts.gate.cardType.Store(nil)
	if !resolves() {
		t.Fatal("the gate's own card no longer resolves")
	}
}

// MCP answers are never walked as A2A bodies (D167: the bucket carries A2A parts only): an MCP task whose text
// mentions kontra-blob: passes as it came, JSON and SSE, and no blob stream is opened.
func TestMCPAnswersAreNotInlined(t *testing.T) {
	ts := startServer(t, nil)
	res, body := ts.do(t, http.MethodPost, "/mcp/fs", "", http.Header{"Content-Type": {"application/json"}}, mcpTaskAnswer)
	if res.StatusCode != 200 || string(body) != mcpTaskAnswer {
		t.Fatalf("an MCP answer mentioning the scheme: %d %.300s", res.StatusCode, body)
	}
	res, body = ts.do(t, http.MethodGet, "/mcp/fs", "", nil, "")
	if res.StatusCode != 200 || string(body) != "id: 1\ndata: "+mcpTaskAnswer+"\n\n" {
		t.Fatalf("an MCP SSE answer mentioning the scheme: %d %q", res.StatusCode, body)
	}
	if _, blobs := ts.tun.counts(); blobs != 0 {
		t.Fatalf("%d blob streams for MCP answers", blobs)
	}
}

// RewriteCard alone: a card signed by another key than the dialed gate's is refused even after a fresh fetch of
// the key set; an unsigned card and one with a case-variant member are refused.
func TestRewriteCardRefusals(t *testing.T) {
	gateURL, _ := url.Parse("http://127.0.0.1:18443")
	local, _ := url.Parse("http://localhost:7777")
	g := newFakeGate(t, "http://127.0.0.1:18443")
	other := newFakeGate(t, "http://127.0.0.1:18443") // same origin, another key
	client := &http.Client{Transport: &streamTransport{open: &fakeTunnel{gate: g.handler(t)}}}
	if _, err := RewriteCard(context.Background(), g.signedCard(t), gateURL, local, "/agents/hello", client); err != nil {
		t.Fatalf("the gate's own card: %v", err)
	}
	before := g.jwksN.Load()
	if _, err := RewriteCard(context.Background(), other.signedCard(t), gateURL, local, "/agents/hello", client); !errors.Is(err, ErrCardNotVerified) {
		t.Fatalf("a card signed by another key: %v", err)
	}
	if g.jwksN.Load() != before+1 {
		t.Fatalf("a card whose kid is unknown makes the node fetch the key set again once (rotation): %d fetches", g.jwksN.Load()-before)
	}
	unsigned := []byte(`{"name":"x","supportedInterfaces":[{"url":"http://127.0.0.1:18443/agents/x"}]}`)
	if _, err := RewriteCard(context.Background(), unsigned, gateURL, local, "/agents/x", client); !errors.Is(err, ErrCardNotVerified) {
		t.Fatalf("an unsigned card: %v", err)
	}
	dup := bytes.Replace(g.signedCard(t), []byte(`"name":"hello"`), []byte(`"name":"hello","NAME":"evil"`), 1)
	if _, err := RewriteCard(context.Background(), dup, gateURL, local, "/agents/hello", client); !errors.Is(err, ErrCardNotVerified) {
		t.Fatalf("a card with a case-variant member: %v", err)
	}
}

// Answers keep kontra-blob: refs for nodes (relay.md §10.4); the hiring server inlines them, fetched on blob
// streams, so a local client sees raw bytes, JSON and SSE.
func TestInlineAnswers(t *testing.T) {
	ts := startServer(t, nil)
	res, body := ts.do(t, http.MethodGet, "/agents/hello/tasks/t1", "", nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var task a2a.Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	p := task.Artifacts[0].Parts[0]
	if !bytes.Equal(p.Raw(), ts.tun.blobs[ts.gate.ref]) || p.Filename != "big.bin" || bytes.Contains(body, []byte(blob.URIScheme)) {
		t.Fatalf("the part: %d bytes, %q", len(p.Raw()), p.Filename)
	}
	if _, blobs := ts.tun.counts(); blobs != 1 {
		t.Fatalf("%d blob streams, want 1", blobs)
	}
	res, body = ts.do(t, http.MethodGet, "/agents/hello/tasks/t1/sse", "", nil, "")
	if res.StatusCode != 200 || bytes.Contains(body, []byte(blob.URIScheme)) || !bytes.HasPrefix(body, []byte("id: 1\ndata: ")) {
		t.Fatalf("SSE: %d %.200s", res.StatusCode, body)
	}
}
