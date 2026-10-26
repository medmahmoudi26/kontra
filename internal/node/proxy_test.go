package node

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// canary is a loopback listener no request may ever reach (sidecar_not_a_proxy, §8.2): every test that
// uses one fails if it accepted a connection.
type canary struct {
	l    net.Listener
	hits atomic.Int32
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &canary{l: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			c.hits.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		if n := c.hits.Load(); n != 0 {
			t.Errorf("the canary at %s received %d connections: the node dialed something other than its target", c.addr(), n)
		}
	})
	return c
}

func (c *canary) addr() string { return c.l.Addr().String() }

// seen is one request the recording target received.
type seen struct {
	method, host, uri string
	header            http.Header
	body              string
}

// recorder is a --serve target that records what reaches it.
type recorder struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []seen
	handler func(w http.ResponseWriter, r *http.Request) bool // true: handled
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, seen{method: r.Method, host: r.Host, uri: r.RequestURI, header: r.Header.Clone(), body: string(b)})
		h := rec.handler
		rec.mu.Unlock()
		if h != nil && h(w, r) {
			return
		}
		// The service's own headers the node must remove (§8.2), a trailer among them.
		w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeFull))
		w.Header().Set(tunnel.HeaderSealedReject, "x")
		w.Header().Set("X-Service", "kept")
		w.Header().Set("Trailer", "X-Service-Trailer")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok "+r.URL.Path)
		w.Header().Set("X-Service-Trailer", "kept")
		w.Header().Set(http.TrailerPrefix+tunnel.HeaderNode, string(tunnel.NodeNotHosted))
		w.Header().Set(http.TrailerPrefix+tunnel.HeaderSealedReject, "y")
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) url() *url.URL { u, _ := url.Parse(r.srv.URL); return u }

func (r *recorder) all() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.reqs...)
}

func (r *recorder) count() int { return len(r.all()) }

func (r *recorder) last(t *testing.T) seen {
	t.Helper()
	all := r.all()
	if len(all) == 0 {
		t.Fatal("the target received no request")
	}
	return all[len(all)-1]
}

func (r *recorder) set(h func(w http.ResponseWriter, r *http.Request) bool) {
	r.mu.Lock()
	r.handler = h
	r.mu.Unlock()
}

// proxyPair is a Proxy behind a real yamux session pair: the node side accepts and serves every stream the
// gate side opens, as a session does.
type proxyPair struct {
	p    *Proxy
	gate *yamux.Session
}

func newProxyPair(t *testing.T, maxInflight int, serves ...Serve) *proxyPair {
	t.Helper()
	pair := tunneltest.NewPair(t)
	node, gate := pair.Sessions(t, nil)
	p := NewProxy(serves, maxInflight, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			st, err := node.AcceptStream()
			if err != nil {
				return
			}
			go p.ServeStream(ctx, st)
		}
	}()
	return &proxyPair{p: p, gate: gate}
}

func a2aServe(name string, target *url.URL) Serve {
	return Serve{Kind: tunnel.KindA2A, Name: name, Target: target}
}

func withBase(u *url.URL, base string) *url.URL {
	c := *u
	c.Path = base
	return &c
}

func hostedPreamble(service string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindA2A,
		Task: "01920000-0000-7000-8000-000000000001", Context: "01920000-0000-7000-8000-000000000002"}
}

// open opens a stream as the gate does and writes its preamble.
func (pp *proxyPair) open(t *testing.T, pre tunnel.Preamble) *yamux.Stream {
	t.Helper()
	st, err := pp.gate.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := tunnel.WritePreamble(st, pre); err != nil {
		t.Fatal(err)
	}
	return st
}

// raw sends one request written by hand (the request target exactly as given) and reads the response, its
// 1xx informational responses first.
func (pp *proxyPair) raw(t *testing.T, pre tunnel.Preamble, request string) (*http.Response, string, []*http.Response) {
	t.Helper()
	st := pp.open(t, pre)
	if _, err := io.WriteString(st, request); err != nil {
		t.Fatal(err)
	}
	_ = st.SetReadDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(st)
	var info []*http.Response
	for {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read the response to %q: %v", strings.SplitN(request, "\r\n", 2)[0], err)
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			info = append(info, resp)
			continue
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the body: %v", err)
		}
		return resp, string(b), info
	}
}

func get(target, host string, extra ...string) string {
	h := "GET " + target + " HTTP/1.1\r\nHost: " + host + "\r\n"
	for _, e := range extra {
		h += e + "\r\n"
	}
	return h + "Connection: close\r\n\r\n"
}

// The request reaches the target and nowhere else: Host is the target's, the path is under its base, the
// response streams back with the node's own headers removed from the service's headers, 1xx responses and
// trailers.
func TestProxyForwards(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/hints" {
			return false
		}
		w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeFull))
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Del(tunnel.HeaderNode)
		io.WriteString(w, "hinted")
		return true
	})
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()))

	resp, body, _ := pp.raw(t, hostedPreamble("hello"), get("/a2a/?q=1&r=%2F", c.addr(), "X-Keep: 1"))
	if resp.StatusCode != 200 || body != "ok /a2a/" {
		t.Fatalf("response %d %q", resp.StatusCode, body)
	}
	got := rec.last(t)
	if got.host != rec.url().Host || got.uri != "/a2a/?q=1&r=%2F" || got.header.Get("X-Keep") != "1" {
		t.Fatalf("the target saw Host %q, URI %q, X-Keep %q", got.host, got.uri, got.header.Get("X-Keep"))
	}
	for _, h := range []string{tunnel.HeaderNode, tunnel.HeaderSealedReject} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("the service's %s reached the gate: %q", h, v)
		}
		if v := resp.Trailer.Get(h); v != "" {
			t.Errorf("the service's trailer %s reached the gate: %q", h, v)
		}
	}
	if resp.Header.Get("X-Service") != "kept" || resp.Trailer.Get("X-Service-Trailer") != "kept" {
		t.Errorf("the service's own headers were lost: %v, trailer %v", resp.Header, resp.Trailer)
	}
	if !resp.Close {
		t.Error("the response does not close the stream: one exchange per stream (§8.2)")
	}

	// A body goes to the target unchanged.
	post := "POST /rpc HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 11\r\nConnection: close\r\n\r\n{\"id\": 1 }\n"
	if resp, _, _ := pp.raw(t, hostedPreamble("hello"), post); resp.StatusCode != 200 || rec.last(t).body != "{\"id\": 1 }\n" {
		t.Fatalf("POST: %d, the target got %q", resp.StatusCode, rec.last(t).body)
	}

	// A 1xx the service sends is relayed without the node's headers.
	resp, body, info := pp.raw(t, hostedPreamble("hello"), get("/hints", "x"))
	if resp.StatusCode != 200 || body != "hinted" || len(info) != 1 || info[0].StatusCode != http.StatusEarlyHints {
		t.Fatalf("early hints: %d %q, %d informational", resp.StatusCode, body, len(info))
	}
	if info[0].Header.Get(tunnel.HeaderNode) != "" || info[0].Header.Get("Link") == "" {
		t.Errorf("the 103's headers: %v", info[0].Header)
	}
}

// §8.2: the request's Host and an absolute-form target are ignored, and the path is decoded and cleaned;
// with a root base every path is confined to the target (M2 plan audit), with a non-root base a path that
// climbs out of it is 400 and reaches nothing.
func TestProxyConfines(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	pp := newProxyPair(t, 0, a2aServe("root", rec.url()), a2aServe("based", withBase(rec.url(), "/base/")))
	for _, x := range []struct {
		service, target, host, want string // want "" is 400
	}{
		{"root", "/x", c.addr(), "/x"},
		{"root", "http://169.254.169.254/x", "169.254.169.254", "/x"},
		{"root", "http://" + c.addr() + "/x?y=1", c.addr(), "/x?y=1"},
		{"root", "/../x", "x", "/x"},
		{"root", "/%2e%2e/x", "x", "/x"},
		{"root", "/../../etc/passwd", "x", "/etc/passwd"},
		{"root", "//127.0.0.2/x", "x", "/127.0.0.2/x"},
		{"root", "/a%2fb/../c", "x", "/a/c"},
		{"root", "/a/./b//c/", "x", "/a/b/c/"},
		{"based", "/x", "x", "/base/x"},
		{"based", "/", "x", "/base/"},
		{"based", "/x/../y", "x", "/base/y"},
		{"based", "http://169.254.169.254/x", "169.254.169.254", "/base/x"},
		{"based", "/../x", "x", ""},
		{"based", "/%2e%2e/x", "x", ""},
		{"based", "/x/../../y", "x", ""},
		{"based", "/%2e%2e%2fbase2/x", "x", ""},
		{"based", "/..", "x", ""},
	} {
		before := rec.count()
		resp, body, _ := pp.raw(t, hostedPreamble(x.service), get(x.target, x.host))
		if x.want == "" {
			if resp.StatusCode != http.StatusBadRequest || rec.count() != before {
				t.Errorf("%s %s: %d %q, the target got %d requests; want 400 and none", x.service, x.target,
					resp.StatusCode, body, rec.count()-before)
			}
			if resp.Header.Get(tunnel.HeaderNode) != "" {
				t.Errorf("%s %s: a Kontra-Node reason on a 400 §8.2 does not list", x.service, x.target)
			}
			continue
		}
		if resp.StatusCode != 200 || rec.count() != before+1 {
			t.Errorf("%s %s: %d %q", x.service, x.target, resp.StatusCode, body)
			continue
		}
		if got := rec.last(t); got.uri != x.want || got.host != rec.url().Host {
			t.Errorf("%s %s: the target saw %q (Host %q), want %q", x.service, x.target, got.uri, got.host, x.want)
		}
	}
}

// CONNECT, Upgrade and TRACE are 400 and reach nothing (§8.2).
func TestProxyRefusesMethods(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()))
	for name, req := range map[string]string{
		"CONNECT":             "CONNECT " + c.addr() + " HTTP/1.1\r\nHost: " + c.addr() + "\r\n\r\n",
		"Upgrade: websocket":  get("/ws", "x", "Upgrade: websocket", "Connection: Upgrade", "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ=="),
		"Upgrade: h2c":        "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: h2c\r\nConnection: Upgrade, HTTP2-Settings\r\nHTTP2-Settings: AAMAAABkAARAAAAAAAIAAAAA\r\n\r\n",
		"Connection: upgrade": get("/", "x", "Connection: keep-alive, upgrade"),
		"TRACE":               "TRACE / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
		// net/http answers OPTIONS * itself (200) unless the server turns that off (M2 WP-06 review).
		"OPTIONS *": "OPTIONS * HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
	} {
		resp, body, _ := pp.raw(t, hostedPreamble("hello"), req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %q, want 400", name, resp.StatusCode, body)
		}
		// No §8.2 reason fits these 400s: they carry no Kontra-Node (the SPEC-CONFLICT on Proxy).
		if v := resp.Header.Get(tunnel.HeaderNode); v != "" {
			t.Errorf("%s: Kontra-Node %q", name, v)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("the target received %d requests", n)
	}
}

// Origin, Forwarded, X-Forwarded-* and hop-by-hop headers are removed and none are added: the service sees
// a local client (§8.2).
func TestProxyHeaders(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()))
	resp, _, _ := pp.raw(t, hostedPreamble("hello"), get("/h", c.addr(),
		"Origin: http://evil.example",
		"Forwarded: for=10.0.0.1;host="+c.addr(),
		"X-Forwarded-For: 10.0.0.1",
		"X-Forwarded-Host: "+c.addr(),
		"X-Forwarded-Proto: https",
		"X-Forwarded-Port: 443",
		"X-Forwarded-Prefix: /evil",
		"Keep-Alive: timeout=5",
		"Proxy-Authorization: Basic Zm9vOmJhcg==",
		"Proxy-Connection: keep-alive",
		"X-Hop: 1",
		"Connection: X-Hop",
		"Te: gzip",
		"X-Keep: kept",
		"Accept-Encoding: identity",
	))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := rec.last(t)
	for _, h := range []string{"Origin", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Forwarded-Port", "X-Forwarded-Prefix", "Keep-Alive", "Proxy-Authorization", "Proxy-Connection", "X-Hop", "Te"} {
		if v, ok := got.header[http.CanonicalHeaderKey(h)]; ok {
			t.Errorf("the target received %s: %q", h, v)
		}
	}
	for k := range got.header {
		if strings.HasPrefix(k, "X-Forwarded-") || k == "Forwarded" || k == "X-Real-Ip" || k == "Via" {
			t.Errorf("the node added %s", k)
		}
	}
	if got.header.Get("X-Keep") != "kept" || got.header.Get("Accept-Encoding") != "identity" || got.host != rec.url().Host {
		t.Errorf("the target saw %v, Host %q", got.header, got.host)
	}
}

// The node's own answers (§8.1, §8.2): 404 not_hosted and kind_mismatch, 502 target_unreachable, and 400
// for a stream whose preamble cannot be read; none of them dials anything but the target.
func TestProxyNodeAnswers(t *testing.T) {
	newCanary(t)
	rec := newRecorder(t)
	down, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	downURL, _ := url.Parse("http://" + down.Addr().String())
	down.Close()
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()), a2aServe("down", downURL), a2aServe("gone", rec.url()))
	pp.p.SetServed([]string{"hello", "down", "nope"})

	for _, x := range []struct {
		name   string
		pre    tunnel.Preamble
		status int
		reason tunnel.NodeReason
	}{
		{"not hosted at all", hostedPreamble("other"), 404, tunnel.NodeNotHosted},
		{"not served on this session", hostedPreamble("gone"), 404, tunnel.NodeNotHosted},
		{"kind mismatch", tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindMCP}, 404, tunnel.NodeKindMismatch},
		{"target down", hostedPreamble("down"), 502, tunnel.NodeTargetUnreachable},
	} {
		resp, body, _ := pp.raw(t, x.pre, get("/", "x"))
		if resp.StatusCode != x.status || resp.Header.Get(tunnel.HeaderNode) != string(x.reason) {
			t.Errorf("%s: %d %s %q; want %d %s", x.name, resp.StatusCode, resp.Header.Get(tunnel.HeaderNode), body, x.status, x.reason)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("the target received %d requests", n)
	}

	// Preambles the node cannot read: 400 before any target is dialed.
	for name, line := range map[string]string{
		"not JSON":    "nope\n",
		"version 2":   `{"v":2,"op":"http","service":"hello","kind":"a2a"}` + "\n",
		"unknown op":  `{"v":1,"op":"socks","service":"hello"}` + "\n",
		"blob":        `{"v":1,"op":"blob","ref":"r"}` + "\n",
		"over 4096 B": `{"v":1,"op":"http","service":"` + strings.Repeat("a", 4100) + `"}` + "\n",
	} {
		st, err := pp.gate.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(st, line+get("/", "x"))
		_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(st), nil)
		if err != nil || resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderNode) != "" {
			t.Errorf("%s: %v %v", name, resp, err)
		}
		st.Close()
	}
	if n := rec.count(); n != 0 {
		t.Errorf("the target received %d requests", n)
	}
}

// Over --max-inflight streams the node answers 503 with Kontra-Node: full (§14), and serves again once
// streams end.
func TestProxyInflightCap(t *testing.T) {
	const max = tunnel.NodeMaxInflight
	rec := newRecorder(t)
	release := make(chan struct{})
	var holding atomic.Int32
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/hold" {
			return false
		}
		holding.Add(1)
		<-release
		io.WriteString(w, "released")
		return true
	})
	pp := newProxyPair(t, max, a2aServe("hello", rec.url()))

	var wg sync.WaitGroup
	results := make(chan int, max)
	for range max {
		wg.Go(func() {
			st, err := pp.gate.OpenStream()
			if err != nil {
				results <- -1
				return
			}
			defer st.Close()
			tunnel.WritePreamble(st, hostedPreamble("hello"))
			io.WriteString(st, get("/hold", "x"))
			resp, err := http.ReadResponse(bufio.NewReader(st), nil)
			if err != nil {
				results <- -2
				return
			}
			io.Copy(io.Discard, resp.Body)
			results <- resp.StatusCode
		})
	}
	deadline := time.Now().Add(20 * time.Second)
	for holding.Load() < max || pp.p.Inflight() < max {
		if time.Now().After(deadline) {
			t.Fatalf("%d held at the target, %d in flight; want %d", holding.Load(), pp.p.Inflight(), max)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, body, _ := pp.raw(t, hostedPreamble("hello"), get("/next", "x"))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Fatalf("stream %d: %d %s %q; want 503 full", max+1, resp.StatusCode, resp.Header.Get(tunnel.HeaderNode), body)
	}
	if got := rec.last(t).uri; got != "/hold" {
		t.Fatalf("the refused stream reached the target: %s", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for code := range results {
		if code != 200 {
			t.Errorf("a held stream ended %d", code)
		}
	}
	if resp, _, _ := pp.raw(t, hostedPreamble("hello"), get("/after", "x")); resp.StatusCode != 200 {
		t.Errorf("after the streams ended: %d", resp.StatusCode)
	}
	if n := pp.p.Inflight(); n != 0 {
		t.Errorf("%d still in flight", n)
	}
}

// An SSE response streams through as it comes: event 1 reaches the gate before the service writes event 2,
// and a response lasting two minutes is not cut (§8.2: no write timeout, flushed per event).
func TestProxySSE(t *testing.T) {
	if testing.Short() {
		t.Skip("two minutes of SSE")
	}
	rec := newRecorder(t)
	got1 := make(chan struct{})
	const hold = 2 * time.Minute
	const beats = 4
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/sse" {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeFull))
		fl := w.(http.Flusher)
		io.WriteString(w, "data: event 1\n\n")
		fl.Flush()
		select {
		case <-got1: // the gate has event 1 before event 2 is written
		case <-time.After(30 * time.Second):
			io.WriteString(w, "data: event 1 was buffered\n\n")
			return true
		}
		for i := range beats {
			time.Sleep(hold / beats)
			fmt.Fprintf(w, "data: beat %d\n\n", i+1)
			fl.Flush()
		}
		io.WriteString(w, "data: last\n\n")
		return true
	})
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()))
	st := pp.open(t, hostedPreamble("hello"))
	io.WriteString(st, get("/sse", "x", "Accept: text/event-stream"))
	br := bufio.NewReader(st)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get(tunnel.HeaderNode) != "" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers %v", resp.Header)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("an SSE response was compressed: %v", resp.Header)
	}
	body := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var ev strings.Builder
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				t.Fatalf("the stream ended after %q: %v", ev.String(), err)
			}
			if line == "\n" {
				return ev.String()
			}
			ev.WriteString(line)
		}
	}
	start := time.Now()
	if ev := readEvent(); ev != "data: event 1\n" {
		t.Fatalf("first event %q", ev)
	}
	close(got1)
	for i := range beats {
		if ev := readEvent(); ev != fmt.Sprintf("data: beat %d\n", i+1) {
			t.Fatalf("event %q", ev)
		}
	}
	if ev := readEvent(); ev != "data: last\n" {
		t.Fatalf("last event %q", ev)
	}
	if rest, err := io.ReadAll(body); err != nil || len(rest) != 0 {
		t.Fatalf("after the last event: %q %v", rest, err)
	}
	if lasted := time.Since(start); lasted < hold {
		t.Fatalf("the stream lasted %v", lasted)
	}
}

// The node dials nothing but its target: not a Host, an absolute URL, a forwarded host, a redirect's
// Location, nor a url part inside a JSON-RPC body (§8.2, §8.3, D56; sidecar_not_a_proxy).
func TestProxyDialsOnlyItsTarget(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	rec.set(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/redirect" {
			return false
		}
		http.Redirect(w, r, "http://"+c.addr()+"/landed", http.StatusFound)
		return true
	})
	pp := newProxyPair(t, 0, a2aServe("hello", rec.url()))
	canaryURL := "http://" + c.addr() + "/x"
	rpc := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER",` +
		`"parts":[{"url":"` + canaryURL + `","mediaType":"text/plain"},{"file":{"uri":"` + canaryURL + `"}}]}}}`
	reqs := []string{
		get(canaryURL, c.addr()),
		get("/x", c.addr(), "X-Forwarded-Host: "+c.addr(), "Forwarded: host="+c.addr()),
		"POST /rpc HTTP/1.1\r\nHost: " + c.addr() + "\r\nContent-Type: application/json\r\nContent-Length: " +
			fmt.Sprint(len(rpc)) + "\r\nConnection: close\r\n\r\n" + rpc,
	}
	for _, r := range reqs {
		if resp, body, _ := pp.raw(t, hostedPreamble("hello"), r); resp.StatusCode != 200 {
			t.Errorf("%d %q", resp.StatusCode, body)
		}
	}
	if got := rec.last(t); got.body != rpc {
		t.Errorf("the body was changed: %q", got.body)
	}
	// A redirect is the service's answer, handed back; the node does not follow it.
	resp, _, _ := pp.raw(t, hostedPreamble("hello"), get("/redirect", "x"))
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://"+c.addr()+"/landed" {
		t.Errorf("redirect: %d %v", resp.StatusCode, resp.Header)
	}
	time.Sleep(100 * time.Millisecond) // a stray dial would have landed by now
}

// The target's transport dials the target's own host:port and nothing else, whatever address it is asked
// for, and takes no proxy from the environment (M2 plan audit).
func TestTargetTransportIsPinned(t *testing.T) {
	c := newCanary(t)
	rec := newRecorder(t)
	tr := targetTransport(rec.url())
	if tr.Proxy != nil {
		t.Fatal("the target transport has a Proxy function")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", c.addr()); !errors.Is(err, errSteered) {
		t.Fatalf("a dial to another address: %v", err)
	}
	conn, err := tr.DialContext(context.Background(), "tcp", strings.ToUpper(rec.url().Host))
	if err != nil {
		t.Fatalf("the target's own address: %v", err)
	}
	conn.Close()
	for raw, want := range map[string]string{
		"http://LocalHost:9999/": "localhost:9999",
		"http://h/":              "h:80",
		"https://h/":             "h:443",
		"http://[::1]:8080/":     "[::1]:8080",
	} {
		u, _ := url.Parse(raw)
		if got := hostPort(u); got != want {
			t.Errorf("hostPort(%s) = %s, want %s", raw, got, want)
		}
	}
}

// With HTTP_PROXY and HTTPS_PROXY pointing at a canary, a request to a non-loopback target still never
// reaches the canary: the transport ignores the environment's proxy. The control run, a transport that
// takes the environment's proxy, shows the canary would have been reached. Both run in a child process,
// because net/http reads the proxy variables once per process.
func TestProxyIgnoresEnvironmentProxy(t *testing.T) {
	switch os.Getenv("KONTRA_NODE_TEST_CHILD") {
	case "node":
		target, _ := url.Parse("http://kontra-target.invalid:9")
		p := NewProxy([]Serve{a2aServe("hello", target)}, 0, nil)
		w := httptest.NewRecorder()
		p.exchange(hostedPreamble("hello"))(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		fmt.Printf("CHILD-RESULT %d %s\n", w.Code, w.Header().Get(tunnel.HeaderNode))
		return
	case "control":
		tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
		req, _ := http.NewRequest(http.MethodGet, "http://kontra-target.invalid:9/x", nil)
		_, err := tr.RoundTrip(req)
		fmt.Printf("CHILD-RESULT control %v\n", err)
		return
	}
	for _, mode := range []string{"node", "control"} {
		c := &canary{}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c.l = l
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				c.hits.Add(1)
				conn.Close()
			}
		}()
		cmd := exec.Command(os.Args[0], "-test.run=^TestProxyIgnoresEnvironmentProxy$", "-test.count=1")
		proxy := "http://" + c.addr()
		cmd.Env = append(os.Environ(), "KONTRA_NODE_TEST_CHILD="+mode, "HTTP_PROXY="+proxy, "http_proxy="+proxy,
			"HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY=", "no_proxy=", "REQUEST_METHOD=")
		out, err := cmd.CombinedOutput()
		l.Close()
		if err != nil {
			t.Fatalf("%s child: %v\n%s", mode, err, out)
		}
		switch mode {
		case "node":
			if !bytes.Contains(out, []byte("CHILD-RESULT 502 target_unreachable")) {
				t.Fatalf("node child:\n%s", out)
			}
			if n := c.hits.Load(); n != 0 {
				t.Fatalf("the node's request reached the environment's proxy (%d connections)", n)
			}
		case "control":
			if n := c.hits.Load(); n == 0 {
				t.Fatalf("the control run never reached the environment's proxy, so the test proves nothing:\n%s", out)
			}
		}
	}
}

func TestTargetPath(t *testing.T) {
	for _, x := range []struct {
		base, path, want string
		ok               bool
	}{
		{"", "", "/", true},
		{"", "/", "/", true},
		{"", "/a/", "/a/", true},
		{"", "/a//b", "/a/b", true},
		{"", "/../..", "/", true},
		{"", "*", "", false},
		{"", "relative", "", false},
		{"/base", "/", "/base/", true},
		{"/base", "/a", "/base/a", true},
		{"/base", "/..", "", false},
		{"/base", "/../base", "/base", true},
		{"/base", "/../basement", "", false},
		{"/base", "/a/../../b", "", false},
	} {
		got, ok := targetPath(x.base, x.path)
		if got != x.want || ok != x.ok {
			t.Errorf("targetPath(%q, %q) = %q %v, want %q %v", x.base, x.path, got, ok, x.want, x.ok)
		}
	}
}
