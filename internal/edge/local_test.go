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
