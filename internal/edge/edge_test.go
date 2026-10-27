package edge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/config"
)

// testRoutes registers a JSON-RPC route, an HTTP+JSON route and /healthz, counting handler runs.
type testRoutes struct {
	router *Router
	runs   atomic.Int64
	// block, when set, holds the JSON-RPC handler until it is closed.
	block  chan struct{}
	inside chan struct{}
}

func newTestRoutes() *testRoutes {
	tr := &testRoutes{router: NewRouter()}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.runs.Add(1)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if tr.inside != nil {
			tr.inside <- struct{}{}
		}
		if tr.block != nil {
			<-tr.block
		}
		a, _ := ClientAddr(r.Context())
		w.Header().Set("X-Client", a.String())
		fmt.Fprintf(w, "%d", len(b))
	})
	tr.router.HandleJSONRPC("POST /agents/{name}", h)
	tr.router.HandleJSONRPC("POST /agents/{name}/{$}", h)
	tr.router.Handle("/agents/{name}/{rest...}", h)
	tr.router.Handle("GET /healthz", Healthz(func() bool { return true }))
	return tr
}

type frozenClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *frozenClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *frozenClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newChain(t *testing.T, l Limits, ci ClientIP, clock *frozenClock) (*testRoutes, http.Handler) {
	t.Helper()
	tr := newTestRoutes()
	cfg := MiddlewareConfig{Limits: l, ClientIP: ci}
	if clock != nil {
		cfg.now = clock.now
	}
	return tr, Middleware(cfg, tr.router)
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func jsonrpcBody(t *testing.T, w *httptest.ResponseRecorder) (id json.RawMessage, code int, msg string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json; body %q", ct, w.Body.String())
	}
	var e struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.JSONRPC != "2.0" {
		t.Fatalf("not a JSON-RPC error: %q (%v)", w.Body.String(), err)
	}
	return e.ID, e.Error.Code, e.Error.Message
}

// A closed router: unknown paths, other methods and paths ServeMux would redirect are an empty 404 that
// runs no handler.
func TestClosedDefaultRoute(t *testing.T) {
	tr, h := newChain(t, Limits{}, ClientIP{}, nil)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/admin"},
		{"GET", "/metrics"},
		{"POST", "/healthz"},
		{"GET", "/agents"},
		{"GET", "/agents/"},
		{"GET", "/agents//x"},         // ServeMux would redirect to the cleaned path
		{"GET", "/agents/a/../b/x"},   // same
		{"DELETE", "/agents/hello/x"}, // the subtree route takes any method: answered by the route
	} {
		r := httptest.NewRequest(tc.method, "http://localhost"+tc.path, nil)
		r.URL.Path = tc.path // httptest cleans nothing, but be explicit
		w := do(h, r)
		if tc.method == "DELETE" {
			if w.Code != 200 {
				t.Errorf("%s %s = %d, want the route", tc.method, tc.path, w.Code)
			}
			continue
		}
		if w.Code != 404 || w.Body.Len() != 0 || w.Header().Get("Location") != "" {
			t.Errorf("%s %s = %d %q (Location %q), want an empty 404", tc.method, tc.path, w.Code, w.Body.String(), w.Header().Get("Location"))
		}
	}
	if n := tr.runs.Load(); n != 1 {
		t.Errorf("handler ran %d times, want 1 (the DELETE)", n)
	}
	// The router on its own (the metrics listener uses it so) is closed the same way.
	w := httptest.NewRecorder()
	tr.router.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Code != 404 || w.Body.Len() != 0 {
		t.Errorf("Router.ServeHTTP unknown = %d %q", w.Code, w.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	var ready atomic.Bool
	h := Healthz(ready.Load)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("not ready: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	ready.Store(true)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("ready: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	Healthz(nil).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Errorf("nil ready func = %d, want 503", w.Code)
	}
}

// limits.body: exactly 4 MiB is served; 4 MiB + 1 byte is 413 before the handler runs, whether the size
// is declared (Content-Length) or not (chunked); on a JSON-RPC route the body is a JSON-RPC error.
func TestBodyLimit(t *testing.T) {
	tr, h := newChain(t, Limits{}, ClientIP{}, nil)
	const limit = 4 << 20
	ok := bytes.Repeat([]byte("a"), limit)
	r := httptest.NewRequest("POST", "/agents/hello", bytes.NewReader(ok))
	if w := do(h, r); w.Code != 200 || w.Body.String() != fmt.Sprint(limit) {
		t.Fatalf("4 MiB body = %d %q", w.Code, w.Body.String())
	}
	over := append(ok, 'b')
	for _, chunked := range []bool{false, true} {
		before := tr.runs.Load()
		var body io.Reader = bytes.NewReader(over)
		if chunked {
			body = io.MultiReader(bytes.NewReader(over)) // no Len: ContentLength stays unknown
		}
		r := httptest.NewRequest("POST", "/agents/hello", body)
		if chunked {
			r.ContentLength = -1
		}
		w := do(h, r)
		if w.Code != 413 {
			t.Fatalf("chunked=%v: 4 MiB + 1 = %d, want 413", chunked, w.Code)
		}
		_, code, msg := jsonrpcBody(t, w)
		if code != -32600 || msg != "request too large" {
			t.Errorf("chunked=%v: JSON-RPC error %d %q", chunked, code, msg)
		}
		if w.Header().Get("Connection") != "close" {
			t.Errorf("chunked=%v: a 413 keeps the connection", chunked)
		}
		if tr.runs.Load() != before {
			t.Errorf("chunked=%v: the handler ran for an oversized body", chunked)
		}
	}
	// HTTP+JSON route: 413 with a plain body.
	r = httptest.NewRequest("POST", "/agents/hello/message:send", bytes.NewReader(over))
	if w := do(h, r); w.Code != 413 || w.Body.String() != "request too large\n" {
		t.Errorf("HTTP+JSON 4 MiB + 1 = %d %q", w.Code, w.Body.String())
	}
}

// rps_per_ip: 50 requests in one second from one address pass, the 51st is 429 with Retry-After; another
// address has its own bucket; a second later the bucket has refilled.
func TestRatePerIP(t *testing.T) {
	clock := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	tr, h := newChain(t, Limits{}, ClientIP{}, clock)
	req := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/agents/hello", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"GetTask"}`))
		r.RemoteAddr = remote
		return do(h, r)
	}
	for i := 1; i <= 50; i++ {
		if w := req("192.0.2.1:1000"); w.Code != 200 {
			t.Fatalf("request %d = %d", i, w.Code)
		}
	}
	w := req("192.0.2.1:1001")
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("51st = %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	id, code, msg := jsonrpcBody(t, w)
	if code != -32600 || msg != "rate limited" || string(id) != "null" {
		t.Errorf("51st: id %s code %d %q (the body is not read before a rate refusal)", id, code, msg)
	}
	if tr.runs.Load() != 50 {
		t.Errorf("handler ran %d times", tr.runs.Load())
	}
	if w := req("192.0.2.2:1000"); w.Code != 200 {
		t.Errorf("another address = %d", w.Code)
	}
	if w := req("[::ffff:192.0.2.1]:1000"); w.Code != 429 {
		t.Errorf("the same address IPv4-mapped = %d, want 429", w.Code)
	}
	clock.add(time.Second)
	for i := 1; i <= 50; i++ {
		if w := req("192.0.2.1:1000"); w.Code != 200 {
			t.Fatalf("after a second, request %d = %d", i, w.Code)
		}
	}
	if w := req("192.0.2.1:1000"); w.Code != 429 {
		t.Errorf("after a second, 51st = %d", w.Code)
	}
}

// inflight_per_key: the 33rd concurrent request with one Authorization value is 429 with Retry-After (a
// JSON-RPC error carrying the request's id); another credential, and no credential, are not affected; a
// slot frees when a request ends.
func TestInflightPerKey(t *testing.T) {
	tr, h := newChain(t, Limits{RPSPerIP: 1e6}, ClientIP{}, nil)
	tr.block = make(chan struct{})
	tr.inside = make(chan struct{}, 64)
	srv := httptest.NewServer(h)
	defer srv.Close()
	post := func(auth string) (*http.Response, error) {
		r, _ := http.NewRequest("POST", srv.URL+"/agents/hello", strings.NewReader(`{"jsonrpc":"2.0","id":"abc","method":"SendMessage"}`))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return http.DefaultClient.Do(r)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := post("Bearer kk_a_1")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("one of the 32 = %d", resp.StatusCode)
			}
		}()
	}
	for i := 0; i < 32; i++ {
		select {
		case <-tr.inside:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d requests reached the handler", i)
		}
	}
	resp, err := post("Bearer kk_a_1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("33rd = %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var e struct {
		ID    string `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil || e.ID != "abc" || e.Error.Code != -32600 || e.Error.Message != "rate limited" {
		t.Errorf("33rd body %q", b)
	}
	// Another key and an anonymous request get through while the 32 hold their slots.
	other := make(chan int, 2)
	go func() {
		resp, err := post("Bearer kk_b_1")
		if err == nil {
			resp.Body.Close()
			other <- resp.StatusCode
		}
	}()
	go func() {
		resp, err := post("")
		if err == nil {
			resp.Body.Close()
			other <- resp.StatusCode
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-tr.inside:
		case <-time.After(10 * time.Second):
			t.Fatal("another credential was held back")
		}
	}
	close(tr.block)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if c := <-other; c != 200 {
			t.Errorf("other request = %d", c)
		}
	}
	tr.block = nil
	resp, err = post("Bearer kk_a_1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("after the 32 ended = %d", resp.StatusCode)
	}
}

// edge.client_ip x_forwarded_for: honoured only from a trusted peer, rightmost untrusted hop; the rate
// is counted per forwarded client, not per proxy.
func TestClientIPForwardedFor(t *testing.T) {
	ci := ClientIP{From: config.ClientIPXForwardedFor, Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	cases := []struct {
		remote string
		xff    []string
		want   string
	}{
		{"10.0.0.1:1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"10.0.0.1:1", []string{"203.0.113.9, 198.51.100.7"}, "198.51.100.7"},           // rightmost untrusted
		{"10.0.0.1:1", []string{"203.0.113.9, 198.51.100.7, 10.1.1.1"}, "198.51.100.7"}, // trusted hops skipped
		{"10.0.0.1:1", []string{"203.0.113.9", "198.51.100.7"}, "198.51.100.7"},         // several headers
		{"10.0.0.1:1", []string{"10.9.9.9, 10.1.1.1"}, "10.9.9.9"},                      // every hop trusted: leftmost
		{"10.0.0.1:1", []string{"garbage, 198.51.100.7"}, "198.51.100.7"},               // left of the client: never read
		{"10.0.0.1:1", []string{"198.51.100.7, garbage"}, "10.0.0.1"},                   // the proxy wrote nonsense
		{"10.0.0.1:1", []string{"[2001:db8::1]:443"}, "2001:db8::1"},                    // bracketed with a port
		{"10.0.0.1:1", []string{"198.51.100.7:8080"}, "198.51.100.7"},                   // with a port
		{"10.0.0.1:1", nil, "10.0.0.1"},                                                 // no header
		{"192.0.2.1:1", []string{"198.51.100.7"}, "192.0.2.1"},                          // untrusted peer: header ignored
		{"[::ffff:10.0.0.1]:1", []string{"198.51.100.7"}, "198.51.100.7"},               // mapped trusted peer
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := ci.Addr(r).String(); got != tc.want {
			t.Errorf("remote %s XFF %q = %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
	// remote mode ignores the header.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := (ClientIP{From: config.ClientIPRemote}).Addr(r).String(); got != "10.0.0.1" {
		t.Errorf("remote mode = %s", got)
	}

	clock := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	_, h := newChain(t, Limits{RPSPerIP: 2}, ci, clock)
	call := func(xff string) int {
		r := httptest.NewRequest("GET", "/agents/hello/x", nil)
		r.RemoteAddr = "10.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", xff)
		w := do(h, r)
		if w.Code == 200 && w.Header().Get("X-Client") != xff {
			t.Errorf("handler saw client %q, want %q", w.Header().Get("X-Client"), xff)
		}
		return w.Code
	}
	for i := 0; i < 2; i++ {
		if c := call("198.51.100.1"); c != 200 {
			t.Fatalf("client 1 request %d = %d", i, c)
		}
	}
	if c := call("198.51.100.1"); c != 429 {
		t.Errorf("client 1 third = %d, want 429", c)
	}
	if c := call("198.51.100.2"); c != 200 {
		t.Errorf("client 2 behind the same proxy = %d, want its own bucket", c)
	}
}

// HSTS only on requests over TLS the gate terminates.
func TestHSTSOnlyOnTLS(t *testing.T) {
	tr := newTestRoutes()
	on := Middleware(MiddlewareConfig{HSTS: true}, tr.router)
	off := Middleware(MiddlewareConfig{HSTS: false}, tr.router)
	plain := httptest.NewRequest("GET", "/healthz", nil)
	overTLS := httptest.NewRequest("GET", "https://gate.example/healthz", nil)
	overTLS.TLS = &tls.ConnectionState{}
	if v := do(on, overTLS).Header().Get("Strict-Transport-Security"); v != HSTS {
		t.Errorf("TLS request: HSTS %q", v)
	}
	if v := do(on, plain).Header().Get("Strict-Transport-Security"); v != "" {
		t.Errorf("plaintext request got HSTS %q", v)
	}
	if v := do(off, overTLS).Header().Get("Strict-Transport-Security"); v != "" {
		t.Errorf("terminated_by_proxy (HSTS off) got %q", v)
	}
	// The closed default route's empty 404 is a response over TLS too (M3 WP-10 pentest finding F-2).
	unknown := httptest.NewRequest("GET", "https://gate.example/admin", nil)
	unknown.TLS = &tls.ConnectionState{}
	if rec := do(on, unknown); rec.Code != 404 || rec.Header().Get("Strict-Transport-Security") != HSTS {
		t.Errorf("closed route over TLS: %d, HSTS %q", rec.Code, rec.Header().Get("Strict-Transport-Security"))
	}
	if v := do(on, httptest.NewRequest("GET", "/admin", nil)).Header().Get("Strict-Transport-Security"); v != "" {
		t.Errorf("closed route in plaintext got HSTS %q", v)
	}
}

func TestJSONRPCErrorID(t *testing.T) {
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":7}`:       `7`,
		`{"jsonrpc":"2.0","id":"x\"y"}`:  `"x\"y"`,
		`{"jsonrpc":"2.0","id":null}`:    `null`,
		`{"jsonrpc":"2.0","id":{"a":1}}`: `null`,
		`[{"jsonrpc":"2.0","id":1}]`:     `null`,
		`not json`:                       `null`,
		``:                               `null`,
	} {
		var e struct{ ID json.RawMessage }
		if err := json.Unmarshal(jsonRPCError([]byte(body), "m"), &e); err != nil || string(e.ID) != want {
			t.Errorf("body %q: id %s (%v), want %s", body, e.ID, err, want)
		}
	}
}

// listen starts a Server on 127.0.0.1:0 and returns its address; it is closed when the test ends.
func listen(t *testing.T, cfg Config, routes *Router) (*Server, string) {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://localhost:8080"
	}
	s, err := New(cfg, routes)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = s.Shutdown(sctx)
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, addr.String()
}

// The server never sets ReadTimeout or WriteTimeout, and takes the header and idle timeouts from limits
// (10 s and 60 s by default).
func TestServerTimeouts(t *testing.T) {
	s, err := New(Config{Listen: "127.0.0.1:0", PublicURL: "http://localhost:8080"}, NewRouter())
	if err != nil {
		t.Fatal(err)
	}
	h := s.HTTPServer()
	if h.ReadHeaderTimeout != 10*time.Second || h.IdleTimeout != 60*time.Second || h.ReadTimeout != 0 || h.WriteTimeout != 0 {
		t.Errorf("timeouts: header %v idle %v read %v write %v", h.ReadHeaderTimeout, h.IdleTimeout, h.ReadTimeout, h.WriteTimeout)
	}
	l := LimitsFrom(config.Defaults().Limits)
	if l.Body != 4<<20 || l.InflightPerKey != 32 || l.RPSPerIP != 50 || l.HeaderTimeout != 10*time.Second ||
		l.Idle != 60*time.Second || l.ConnsPerIP != 256 || l.MaxConns != 10000 || l.BodyTimeout != 30*time.Second {
		t.Errorf("default limits %+v", l)
	}
}

// Plaintext is refused off localhost unless a front proxy terminates TLS (PRD §11, D69).
func TestPlaintextRefusedOffLocalhost(t *testing.T) {
	for _, tc := range []struct {
		url   string
		proxy bool
		ok    bool
	}{
		{"http://localhost:8080", false, true},
		{"http://127.0.0.1:8080", false, true},
		{"http://[::1]:8080", false, true},
		{"https://gate.example.com", false, false},
		{"http://gate.example.com", false, false},
		{"https://gate.example.com", true, true},
	} {
		_, err := New(Config{Listen: "127.0.0.1:0", PublicURL: tc.url, TerminatedByProxy: tc.proxy}, NewRouter())
		if (err == nil) != tc.ok {
			t.Errorf("%s proxy=%v: %v", tc.url, tc.proxy, err)
		}
	}
}

// A half-written header is cut at the header timeout; an idle kept-alive connection closes at the idle
// timeout; a streaming response lasting several times every timeout is not cut (WriteTimeout 0, no
// ReadTimeout, the body deadline cleared after the body).
func TestServerTimeoutsOnTheWire(t *testing.T) {
	tr := newTestRoutes()
	tr.router.HandleJSONRPC("POST /stream/{$}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			fmt.Fprintf(w, ": keep-alive\n\n")
			fl.Flush()
		}
		fmt.Fprintf(w, "data: done\n\n")
	}))
	l := Limits{HeaderTimeout: 300 * time.Millisecond, Idle: 400 * time.Millisecond, BodyTimeout: 300 * time.Millisecond}
	_, addr := listen(t, Config{Limits: l}, tr.router)

	// Half a header: the server closes the connection after about 300 ms.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(c)
	c.Close()
	if el := time.Since(start); err != nil || el < 250*time.Millisecond || el > 3*time.Second {
		t.Errorf("half-written header: closed after %v (%v), want about 300 ms", el, err)
	}

	// Keep-alive: a finished request's connection closes after about 400 ms of idleness.
	c, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz over keep-alive: %v %v", resp, err)
	}
	_, _ = io.ReadAll(resp.Body)
	idleStart := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = br.ReadByte()
	c.Close()
	if el := time.Since(idleStart); err != io.EOF || el < 300*time.Millisecond || el > 3*time.Second {
		t.Errorf("idle keep-alive: closed after %v (%v), want about 400 ms", el, err)
	}

	// A stream of 2 s (five times every timeout) after a POST body arrives whole.
	start = time.Now()
	r, _ := http.NewRequest("POST", "http://"+addr+"/stream/", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.HasSuffix(string(b), "data: done\n\n") || time.Since(start) < 2*time.Second {
		t.Errorf("stream cut after %v: %v %q", time.Since(start), err, b)
	}
}

// A body that arrives too slowly is cut at the body timeout (408), and the handler never runs.
func TestSlowBodyIsCut(t *testing.T) {
	tr := newTestRoutes()
	_, addr := listen(t, Config{Limits: Limits{BodyTimeout: 300 * time.Millisecond}}, tr.router)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, _ = io.WriteString(c, "POST /agents/hello HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{\"jsonrpc\":")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	if resp.StatusCode != 408 || el < 250*time.Millisecond || el > 3*time.Second {
		t.Errorf("slow body: %d after %v", resp.StatusCode, el)
	}
	if tr.runs.Load() != 0 {
		t.Error("the handler ran for a body that never arrived")
	}
}

// limits.max_conns and conns_per_ip: connections over a cap are closed before anything is read, and a
// slot frees when a connection closes.
func TestConnectionCaps(t *testing.T) {
	tr := newTestRoutes()
	s, addr := listen(t, Config{Limits: Limits{ConnsPerIP: 2, MaxConns: 3}}, tr.router)
	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	served := func(c net.Conn) bool {
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			return false
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		return err == nil && resp.StatusCode == 200
	}
	a, b := dial(), dial()
	if !served(a) || !served(b) {
		t.Fatal("the first two connections were not served")
	}
	third := dial()
	if served(third) {
		t.Error("a third connection from 127.0.0.1 was served over conns_per_ip 2")
	}
	third.Close()
	a.Close()
	waitOpen(t, s, 1)
	again := dial()
	if !served(again) {
		t.Error("a slot did not free when a connection closed")
	}
	again.Close()
	b.Close()
	waitOpen(t, s, 0)
}

func waitOpen(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		total, _ := s.ln.open()
		s.mu.Unlock()
		if total == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open connections %d, want %d", total, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// max_conns applies to everyone, conns_per_ip is waived for a trusted x_forwarded_for proxy.
func TestConnectionCapsTrustedProxy(t *testing.T) {
	tr := newTestRoutes()
	ci := ClientIP{From: config.ClientIPXForwardedFor, Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	_, addr := listen(t, Config{Limits: Limits{ConnsPerIP: 1, MaxConns: 3}, ClientIP: ci}, tr.router)
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Forwarded-For: 198.51.100.9\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		ok := err == nil && resp.StatusCode == 200
		if want := i < 3; ok != want {
			t.Errorf("connection %d from the trusted proxy: served %v, want %v", i+1, ok, want)
		}
	}
}

// proxy_protocol: the PROXY header's source is the client address handlers see and conns_per_ip counts;
// v1 and v2, LOCAL and no header from a trusted peer; an untrusted peer's header is not read.
func TestProxyProtocol(t *testing.T) {
	tr := newTestRoutes()
	ci := ClientIP{From: config.ClientIPProxyProtocol, Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	s, addr := listen(t, Config{Limits: Limits{ConnsPerIP: 1, MaxConns: 100}, ClientIP: ci}, tr.router)
	get := func(header []byte) (string, error) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return "", err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(append(header, "GET /agents/hello/x HTTP/1.1\r\nHost: x\r\n\r\n"...)); err != nil {
			return "", err
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("status %d", resp.StatusCode)
		}
		return resp.Header.Get("X-Client"), nil
	}
	v2 := func(cmd byte, fam byte, src netip.AddrPort) []byte {
		b := append([]byte{}, proxyV2Sig...)
		b = append(b, 0x20|cmd, fam)
		var addrs []byte
		if src.Addr().Is4() {
			a := src.Addr().As4()
			addrs = append(addrs, a[:]...)
			addrs = append(addrs, 10, 0, 0, 1)
		} else {
			a := src.Addr().As16()
			addrs = append(addrs, a[:]...)
			d := netip.MustParseAddr("2001:db8::ff").As16()
			addrs = append(addrs, d[:]...)
		}
		addrs = append(addrs, byte(src.Port()>>8), byte(src.Port()), 0x01, 0xbb)
		addrs = append(addrs, 0x04, 0x00, 0x02, 'h', 'i') // a TLV, skipped
		b = append(b, byte(len(addrs)>>8), byte(len(addrs)))
		return append(b, addrs...)
	}
	for _, tc := range []struct {
		name   string
		header []byte
		want   string
	}{
		{"v1 tcp4", []byte("PROXY TCP4 198.51.100.7 10.0.0.1 40000 443\r\n"), "198.51.100.7"},
		{"v1 tcp6", []byte("PROXY TCP6 2001:db8::7 2001:db8::1 40000 443\r\n"), "2001:db8::7"},
		{"v1 unknown", []byte("PROXY UNKNOWN\r\n"), "127.0.0.1"},
		{"v2 tcp4", v2(1, 0x11, netip.MustParseAddrPort("203.0.113.5:5000")), "203.0.113.5"},
		{"v2 tcp6", v2(1, 0x21, netip.MustParseAddrPort("[2001:db8::5]:5000")), "2001:db8::5"},
		{"v2 local", v2(0, 0x11, netip.MustParseAddrPort("203.0.113.5:5000")), "127.0.0.1"},
		{"no header", nil, "127.0.0.1"},
	} {
		got, err := get(tc.header)
		if err != nil || got != tc.want {
			t.Errorf("%s: client %q (%v), want %s", tc.name, got, err, tc.want)
		}
	}
	waitOpen(t, s, 0)
	// A malformed header closes the connection.
	if _, err := get([]byte("PROXY TCP4 nonsense\r\n")); err == nil {
		t.Error("a malformed PROXY header was served")
	}
	// conns_per_ip 1 counts the header's address: a second connection for the same client is refused,
	// one for another client is not.
	hold, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	_, _ = hold.Write([]byte("PROXY TCP4 198.51.100.7 10.0.0.1 40000 443\r\nGET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"))
	_ = hold.SetDeadline(time.Now().Add(3 * time.Second))
	if resp, err := http.ReadResponse(bufio.NewReader(hold), nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("holding connection: %v", err)
	}
	if _, err := get([]byte("PROXY TCP4 198.51.100.7 10.0.0.1 40001 443\r\n")); err == nil {
		t.Error("a second connection for 198.51.100.7 was served over conns_per_ip 1")
	}
	if got, err := get([]byte("PROXY TCP4 198.51.100.8 10.0.0.1 40001 443\r\n")); err != nil || got != "198.51.100.8" {
		t.Errorf("another client: %q %v", got, err)
	}
}

// From an untrusted peer a PROXY header is not read: it is a malformed request.
func TestProxyProtocolUntrustedPeer(t *testing.T) {
	tr := newTestRoutes()
	ci := ClientIP{From: config.ClientIPProxyProtocol, Trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	_, addr := listen(t, Config{ClientIP: ci}, tr.router)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("PROXY TCP4 198.51.100.7 10.0.0.1 40000 443\r\nGET /agents/hello/x HTTP/1.1\r\nHost: x\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil && resp.StatusCode == 200 {
		t.Errorf("an untrusted peer's PROXY header was honoured (client %q)", resp.Header.Get("X-Client"))
	}
}

func TestReadProxyHeaderErrors(t *testing.T) {
	for name, in := range map[string]string{
		"v1 no crlf":      "PROXY TCP4 1.2.3.4 5.6.7.8 1 2" + strings.Repeat("x", 120),
		"v1 family":       "PROXY TCP4 2001:db8::1 2001:db8::2 1 2\r\n",
		"v1 bad port":     "PROXY TCP4 1.2.3.4 5.6.7.8 99999 2\r\n",
		"v1 leading zero": "PROXY TCP4 1.2.3.4 5.6.7.8 01 2\r\n",
		"v1 fields":       "PROXY TCP4 1.2.3.4\r\n",
		"v2 version":      string(proxyV2Sig) + "\x11\x11\x00\x0c" + strings.Repeat("\x00", 12),
		"v2 short":        string(proxyV2Sig) + "\x21\x11\x00\x04" + strings.Repeat("\x00", 4),
		"v2 command":      string(proxyV2Sig) + "\x2f\x11\x00\x0c" + strings.Repeat("\x00", 12),
		"v2 huge":         string(proxyV2Sig) + "\x21\x11\xff\xff",
	} {
		if _, err := readProxyHeader(bufio.NewReaderSize(strings.NewReader(in), proxyMaxHeader)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// POST is not a PROXY header.
	br := bufio.NewReaderSize(strings.NewReader("POST / HTTP/1.1\r\n"), proxyMaxHeader)
	if ap, err := readProxyHeader(br); err != nil || ap.IsValid() {
		t.Errorf("POST: %v %v", ap, err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "POST / HTTP/1.1\r\n" {
		t.Errorf("POST bytes consumed: %q", rest)
	}
}

// One key spelt several ways shares its in-flight slots.
func TestCredentialKeySpellings(t *testing.T) {
	k := credentialKey([]string{"Bearer kk_a_1"})
	for _, v := range []string{"bearer kk_a_1", "BEARER kk_a_1", "Bearer    kk_a_1"} {
		if credentialKey([]string{v}) != k {
			t.Errorf("%q counts apart from %q", v, "Bearer kk_a_1")
		}
	}
	for _, v := range [][]string{{"Bearer kk_a_2"}, {"Basic kk_a_1"}, {"kk_a_1"}, {"Bearer kk_a_1", "Bearer kk_a_1"}} {
		if credentialKey(v) == k {
			t.Errorf("%q counts with %q", v, "Bearer kk_a_1")
		}
	}
}

// The acceptance numbers themselves, with the default limits: a half-written header is cut at 10 s, an
// idle kept-alive connection closes at 60 s, and an SSE response streaming for 3 minutes is not cut. About
// four minutes, so only with KONTRA_TEST_LONG=1.
func TestDefaultTimeoutsLong(t *testing.T) {
	if os.Getenv("KONTRA_TEST_LONG") == "" {
		t.Skip("set KONTRA_TEST_LONG=1 to run the 10 s / 60 s / 3 min timeout check with the default limits")
	}
	tr := newTestRoutes()
	tr.router.Handle("GET /sse", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		end := time.Now().Add(3 * time.Minute)
		for time.Now().Before(end) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(KeepAlive):
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
		fmt.Fprint(w, "data: done\n\n")
	}))
	_, addr := listen(t, Config{}, tr.router)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		start := time.Now()
		_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n")
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, err = io.ReadAll(c)
		if el := time.Since(start); err != nil || el < 9*time.Second || el > 12*time.Second {
			t.Errorf("half-written header closed after %v (%v), want 10 s", el, err)
		}
	}()
	go func() {
		defer wg.Done()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.ReadAll(resp.Body)
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, err = br.ReadByte()
		if el := time.Since(start); err != io.EOF || el < 59*time.Second || el > 62*time.Second {
			t.Errorf("idle connection closed after %v (%v), want 60 s", el, err)
		}
	}()
	go func() {
		defer wg.Done()
		start := time.Now()
		resp, err := http.Get("http://" + addr + "/sse")
		if err != nil {
			t.Error(err)
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if el := time.Since(start); err != nil || !strings.HasSuffix(string(b), "data: done\n\n") || el < 3*time.Minute {
			t.Errorf("SSE ended after %v: %v", el, err)
		} else {
			t.Logf("SSE streamed for %v uncut", el)
		}
	}()
	wg.Wait()
}

// The metrics listener serves GET /metrics and nothing else, on its own address.
func TestMetricsListener(t *testing.T) {
	m := NewMetrics("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "kontra_up 1\n")
	}))
	addr, err := m.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Serve(ctx) }()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "kontra_up 1\n" {
		t.Errorf("/metrics = %d %q", resp.StatusCode, b)
	}
	for _, p := range []string{"/", "/healthz", "/agents/hello", "/metrics/x"} {
		resp, err := http.Get("http://" + addr.String() + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || len(b) != 0 {
			t.Errorf("%s on the metrics listener = %d %q", p, resp.StatusCode, b)
		}
	}
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	if err := m.Shutdown(sctx); err != nil {
		t.Error(err)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve after Shutdown: %v", err)
	}
}

// A route's wildcards and pattern reach its handler behind the edge exactly as through a bare ServeMux:
// r.PathValue and r.Pattern are set only by ServeMux.ServeHTTP, so the edge dispatches through it.
func TestPathValuesBehindTheEdge(t *testing.T) {
	router := NewRouter()
	show := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "name=%q rest=%q pattern=%q", r.PathValue("name"), r.PathValue("rest"), r.Pattern)
	})
	router.Handle("GET /agents/{name}/{rest...}", show)
	router.HandleJSONRPC("POST /agents/{name}", show)
	const (
		wantGET  = `name="hello" rest=".well-known/agent-card.json" pattern="GET /agents/{name}/{rest...}"`
		wantPOST = `name="hello" rest="" pattern="POST /agents/{name}"`
	)
	get := func() *http.Request {
		return httptest.NewRequest("GET", "/agents/hello/.well-known/agent-card.json", nil)
	}
	post := func() *http.Request {
		return httptest.NewRequest("POST", "/agents/hello", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	}
	for name, h := range map[string]http.Handler{
		"Router":     router,
		"Middleware": Middleware(MiddlewareConfig{}, router),
	} {
		if got := do(h, get()).Body.String(); got != wantGET {
			t.Errorf("%s GET: %s, want %s", name, got, wantGET)
		}
		if got := do(h, post()).Body.String(); got != wantPOST {
			t.Errorf("%s POST: %s, want %s", name, got, wantPOST)
		}
	}
	// And through the server New builds (the production path).
	_, addr := listen(t, Config{}, router)
	resp, err := http.Get("http://" + addr + "/agents/hello/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != wantGET {
		t.Errorf("New GET: %s, want %s", b, wantGET)
	}
	resp, err = http.Post("http://"+addr+"/agents/hello", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != wantPOST {
		t.Errorf("New POST: %s, want %s", b, wantPOST)
	}
}

// stallBody is a request body that sends nothing until released.
type stallBody struct {
	once    sync.Once
	reading chan<- struct{}
	release <-chan struct{}
}

func (b *stallBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.reading <- struct{}{} })
	<-b.release
	return 0, io.ErrUnexpectedEOF
}

func (b *stallBody) Close() error { return nil }

// A declared Content-Length is not memory: requests that declare the 4 MiB limit and send no body hold
// what they sent, not 4 MiB each, while the edge waits for their bodies (before authentication).
func TestDeclaredLengthIsNotPreallocated(t *testing.T) {
	tr, h := newChain(t, Limits{RPSPerIP: 1e6}, ClientIP{}, nil)
	const n = 40
	reading := make(chan struct{}, n)
	release := make(chan struct{})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		r := httptest.NewRequest("POST", "/agents/hello", nil)
		r.Body = &stallBody{reading: reading, release: release}
		r.ContentLength = 4 << 20
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := do(h, r); w.Code != http.StatusBadRequest {
				t.Errorf("a body cut short = %d, want 400", w.Code)
			}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-reading:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d requests started reading their bodies", i)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	close(release)
	wg.Wait()
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d header-only requests declaring 4 MiB each: heap grew %d KiB", n, grew>>10)
	if grew > 8<<20 {
		t.Errorf("%d requests declaring 4 MiB and sending nothing hold %d MiB of heap, want what they sent", n, grew>>20)
	}
	if tr.runs.Load() != 0 {
		t.Error("the handler ran for a body that never arrived")
	}
}

// rps_per_ip and conns_per_ip count an IPv6 client by its /64 (one end site), an IPv4 client by its
// address; the handler still sees the full address.
func TestIPv6CountedPerSlash64(t *testing.T) {
	clock := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	_, h := newChain(t, Limits{RPSPerIP: 2}, ClientIP{}, clock)
	call := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/agents/hello/x", nil)
		r.RemoteAddr = remote
		return do(h, r)
	}
	served := 0
	for i := 1; i <= 100; i++ {
		w := call(fmt.Sprintf("[2001:db8::%x]:443", i))
		if w.Code == 200 {
			served++
			if want := fmt.Sprintf("2001:db8::%x", i); w.Header().Get("X-Client") != want {
				t.Errorf("handler saw client %q, want the full address %s", w.Header().Get("X-Client"), want)
			}
		}
	}
	if served != 2 {
		t.Errorf("100 addresses of one /64 at rps_per_ip 2: %d served, want 2", served)
	}
	if w := call("[2001:db8:0:1::1]:443"); w.Code != 200 {
		t.Errorf("another /64 = %d, want its own bucket", w.Code)
	}
	if w := call("192.0.2.1:1"); w.Code != 200 {
		t.Errorf("an IPv4 client = %d", w.Code)
	}
	if w := call("192.0.2.2:1"); w.Code != 200 {
		t.Errorf("a second IPv4 address = %d, want its own bucket", w.Code)
	}

	l := newCapListener(nil, Limits{ConnsPerIP: 1, MaxConns: 100}, ClientIP{}, nil)
	a1, a2 := netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")
	if !l.admit(a1, true) {
		t.Fatal("first connection of the /64 refused")
	}
	if l.admit(a2, true) {
		t.Error("a second address of the same /64 got its own conns_per_ip slot")
	}
	if l.countAddr(netip.MustParseAddr("2001:db8::3")) {
		t.Error("a PROXY connection from the same /64 got its own conns_per_ip slot")
	}
	if !l.admit(netip.MustParseAddr("2001:db8:0:1::1"), true) {
		t.Error("another /64 refused")
	}
	if !l.admit(netip.MustParseAddr("192.0.2.1"), true) || !l.admit(netip.MustParseAddr("192.0.2.2"), true) {
		t.Error("two IPv4 addresses share a slot")
	}
	l.release(a2, true) // the /64's slot, released by any of its addresses
	if !l.admit(a2, true) {
		t.Error("the /64's slot did not free")
	}
}

// trickle returns at most n bytes a Read, as a network body does.
type trickle struct {
	r io.Reader
	n int
}

func (t *trickle) Read(p []byte) (int, error) {
	if len(p) > t.n {
		p = p[:t.n]
	}
	return t.r.Read(p)
}

// readUpTo reads a body whole, or limit+1 bytes of one over the limit, and its buffer follows the bytes
// received: a declared body ends in a buffer of its length and one spare byte, an undeclared one in at most
// about twice its length, and a declared length is never trusted for what is read.
func TestReadUpTo(t *testing.T) {
	const limit = 4 << 20
	for _, tc := range []struct {
		name           string
		size, declared int64
		wantLen        int64
		maxCap         int64
	}{
		{"empty, declared", 0, 0, 0, 1},
		{"empty, chunked", 0, -1, 0, bodyPrealloc},
		{"small, declared", 100, 100, 100, 101},
		{"small, chunked", 100, -1, 100, bodyPrealloc},
		{"prealloc + 1, declared", bodyPrealloc + 1, bodyPrealloc + 1, bodyPrealloc + 1, bodyPrealloc + 2},
		{"1 MiB + 3, chunked", 1<<20 + 3, -1, 1<<20 + 3, 2*(1<<20+3) + bodyPrealloc},
		{"the limit, declared", limit, limit, limit, limit + 1},
		{"the limit, chunked", limit, -1, limit, limit + 1},
		{"over, chunked", limit + 10, -1, limit + 1, limit + 1},
		{"longer than declared", 300, 100, 300, 2 * (300 + 101)},
		{"over, declared short", limit + 10, 100, limit + 1, limit + 1},
	} {
		body := bytes.Repeat([]byte("x"), int(tc.size))
		b, err := readUpTo(&trickle{r: bytes.NewReader(body), n: 1500}, tc.declared, limit)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if int64(len(b)) != tc.wantLen || !bytes.Equal(b, body[:tc.wantLen]) {
			t.Errorf("%s: read %d bytes, want %d", tc.name, len(b), tc.wantLen)
		}
		if int64(cap(b)) > tc.maxCap {
			t.Errorf("%s: buffer of %d for %d bytes, want at most %d", tc.name, cap(b), len(b), tc.maxCap)
		}
	}
	// An absurd limit does not overflow.
	if b, err := readUpTo(strings.NewReader("abc"), -1, math.MaxInt64); err != nil || string(b) != "abc" {
		t.Errorf("limit MaxInt64: %q %v", b, err)
	}
	// A read error is returned, not a partial body.
	r := io.MultiReader(strings.NewReader("abc"), iotestErr{})
	if b, err := readUpTo(r, 10, limit); err == nil || b != nil {
		t.Errorf("a failing body: %q %v", b, err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
