package tunnelgate

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// anonDecision admits every hello as an anonymous session ending at end.
func anonDecision(end time.Time) func(enroll.Request) (*enroll.Decision, error) {
	return func(req enroll.Request) (*enroll.Decision, error) {
		d := admitAll(req)
		d.Anonymous = &enroll.Anonymous{ExpiresAt: end}
		return d, nil
	}
}

// spec/tunnel.md §6.2, §14 (D182): an anonymous session's node-opened streams in flight are capped at
// tunnel.anonymous's hiring_streams, not the gate's 32: the stream past it is 429 with Retry-After.
func TestAnonymousHiringStreamsCap(t *testing.T) {
	who := &whoAmI{release: make(chan struct{}), entered: make(chan struct{}, 64)}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) {
		c.NodeStreams = &NodeStreams{Hire: who}
		c.AnonymousHiringStreams = 2
	}})
	g.adm.set(anonDecision(time.Now().Add(time.Hour)))
	n := dialHiring(t, g)
	defer close(who.release)
	var held []*yamux.Stream
	for range 2 {
		held = append(held, n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello")))
	}
	for i := range 2 {
		select {
		case <-who.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 2 requests reached the router", i)
		}
	}
	over := n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello"))
	_ = over.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(over), nil)
	if err != nil || res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("the 3rd stream of an anonymous session: %v %v, want 429", res, err)
	}
	if who.count() != 2 || n.s.Mux().IsClosed() {
		t.Fatalf("%d requests reached the router, session closed %v", who.count(), n.s.Mux().IsClosed())
	}
	_ = held
}

// §6.1, §13: welcome.anonymous carries the session's end; {"t": "expiring", "at"} comes at once when less than 10
// minutes are left; at the end {"t": "revoked"} (no service) and the session closes, and OnAnonymousExpiry runs.
func TestAnonymousSessionEnds(t *testing.T) {
	ended := make(chan struct{}, 1)
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) {
		c.OnAnonymousExpiry = func() { ended <- struct{}{} }
	}})
	end := time.Now().Add(1500 * time.Millisecond).UTC()
	g.adm.set(anonDecision(end))
	d, w, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Anonymous == nil || !w.Anonymous.ExpiresAt.Equal(end) {
		t.Fatalf("welcome.anonymous %+v, want %s", w.Anonymous, end)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctl, err := m.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	s := g.sink.next(t)
	if at, ok := s.Anonymous(); !ok || !at.Equal(end) {
		t.Fatalf("the session: anonymous %v until %s", ok, at)
	}
	br := bufio.NewReader(ctl)
	if p, err := tunnel.ReadPreamble(br); err != nil || p.Op != tunnel.OpControl {
		t.Fatalf("control preamble %+v %v", p, err)
	}
	c, err := tunnel.ReadControl(br)
	if err != nil || c.T != tunnel.CtlExpiring || c.At == nil || !c.At.Equal(end) || !time.Now().Before(end) {
		t.Fatalf("first control %+v %v (at %s, end %s)", c, err, time.Now(), end)
	}
	c, err = tunnel.ReadControl(br)
	if err != nil || c.T != tunnel.CtlRevoked || c.Service != "" {
		t.Fatalf("second control %+v %v", c, err)
	}
	if time.Now().Before(end) {
		t.Fatal("revoked before the session's end")
	}
	select {
	case <-m.CloseChan():
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not close at its end")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("OnAnonymousExpiry did not run")
	}
}

// The admission is given the edge's client address (relay.md §2.2), which an anonymous session is counted by
// (spec/tunnel.md §6.2): behind a trusted proxy the X-Forwarded-For hop, else the connection's remote address.
func TestTunnelClientAddress(t *testing.T) {
	var got netip.Addr
	rt := edge.NewRouter()
	rt.Handle("GET "+tunnel.Path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = clientAddr(r) }))
	h := edge.Middleware(edge.MiddlewareConfig{ClientIP: edge.ClientIP{From: config.ClientIPXForwardedFor,
		Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}}, rt)
	req := httptest.NewRequest(http.MethodGet, tunnel.Path, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("behind the trusted proxy: %s", got)
	}
	req = httptest.NewRequest(http.MethodGet, tunnel.Path, nil)
	req.RemoteAddr = "[::ffff:198.51.100.4]:5555"
	if a := clientAddr(req); a != netip.MustParseAddr("198.51.100.4") {
		t.Fatalf("without the edge: %s", a)
	}

	g := newTestGate(t, gateOpts{})
	if _, _, err := g.connect(t, newIdentity(t), newHello("hello"), false); err != nil {
		t.Fatal(err)
	}
	reqs := g.adm.requests()
	if len(reqs) != 1 || reqs[0].Client != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("Admit got the client %v", reqs)
	}
}
