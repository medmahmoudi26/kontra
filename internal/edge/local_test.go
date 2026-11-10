package edge

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The local development profile's Host check: a request on 127.0.0.1:<port> whose Host names another domain (DNS
// rebinding) is 421 before any route runs; 127.0.0.1, localhost and [::1] with the connection's port are served.
func TestLoopbackHostOnly(t *testing.T) {
	tr := newTestRoutes()
	h := Middleware(MiddlewareConfig{LoopbackOnly: true}, tr.router)
	la := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}
	req := func(host string) *http.Request {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.Host = host
		return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, la))
	}
	for _, host := range []string{"attacker.example:8080", "attacker.example", "127.0.0.1:9999", "localhost", "127.0.0.2:8080",
		"localhost.attacker.example:8080", ""} {
		before := tr.runs.Load()
		if w := do(h, req(host)); w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q = %d, want 421", host, w.Code)
		}
		if tr.runs.Load() != before {
			t.Errorf("Host %q ran a handler", host)
		}
	}
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "LOCALHOST:8080"} {
		if w := do(h, req(host)); w.Code != 200 {
			t.Errorf("Host %q = %d, want 200", host, w.Code)
		}
	}
	// Off by default: any Host is served.
	if w := do(Middleware(MiddlewareConfig{}, tr.router), req("attacker.example:8080")); w.Code != 200 {
		t.Errorf("without LoopbackOnly: %d", w.Code)
	}
}

// OnRefuse hears each of the edge's own refusals, with the request and the reason.
func TestOnRefuse(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	tr := newTestRoutes()
	h := Middleware(MiddlewareConfig{OnRefuse: func(r *http.Request, status int, reason string) {
		mu.Lock()
		got = append(got, r.URL.Path+" "+http.StatusText(status)+" "+reason)
		mu.Unlock()
	}}, tr.router)
	over := bytes.Repeat([]byte("a"), 4<<20+1)
	do(h, httptest.NewRequest("POST", "/agents/hello", bytes.NewReader(over)))
	do(h, httptest.NewRequest("POST", "/agents/hello", strings.NewReader("{}")))
	if len(got) != 1 || got[0] != "/agents/hello Request Entity Too Large body_too_large" {
		t.Fatalf("OnRefuse heard %q", got)
	}
}

// In the profile a browser's cross-site request to an A2A route is 403 before any handler, and heard by OnRefuse; a
// client that sends no Origin (any A2A SDK) and the page's own origin are served; other routes are untouched.
func TestLoopbackRefusesCrossSite(t *testing.T) {
	tr := newTestRoutes()
	var heard []string
	h := Middleware(MiddlewareConfig{LoopbackOnly: true, OnRefuse: func(r *http.Request, status int, reason string) {
		heard = append(heard, reason)
	}}, tr.router)
	req := func(path string, hdr ...string) *http.Request {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"SendMessage"}`))
		r.Host = "127.0.0.1:8080"
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	for _, c := range []struct {
		name string
		hdr  []string
		want int
	}{
		{"no origin", nil, 200},
		{"own origin", []string{"Origin", "http://127.0.0.1:8080"}, 200},
		{"same-origin fetch", []string{"Sec-Fetch-Site", "same-origin"}, 200},
		{"other site", []string{"Origin", "http://attacker.example"}, 403},
		{"null origin", []string{"Origin", "null"}, 403},
		{"other loopback port", []string{"Origin", "http://127.0.0.1:3000"}, 403},
		{"cross-site no-cors", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"same-site", []string{"Sec-Fetch-Site", "same-site"}, 403},
	} {
		before := tr.runs.Load()
		w := do(h, req("/agents/hello?A2A-Version=1.0", c.hdr...))
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 403 && tr.runs.Load() != before {
			t.Errorf("%s: a handler ran", c.name)
		}
	}
	if len(heard) != 5 || heard[0] != ReasonCrossSite {
		t.Errorf("OnRefuse heard %v", heard)
	}
	// Off the A2A routes, and outside the profile, the rule does not apply.
	if w := do(h, req("/healthz", "Origin", "http://attacker.example")); w.Code == 403 {
		t.Error("the rule reached /healthz")
	}
	if w := do(Middleware(MiddlewareConfig{}, tr.router), req("/agents/hello", "Origin", "http://attacker.example")); w.Code != 200 {
		t.Errorf("a team gate refused a cross-site request: %d", w.Code)
	}
}
