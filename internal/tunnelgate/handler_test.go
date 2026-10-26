package tunnelgate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

const testReplica = "r-test"

var testTenantID = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000aa")

// fakeAdmitter records requests and admits every declared service, unless fn decides otherwise.
type fakeAdmitter struct {
	mu   sync.Mutex
	reqs []enroll.Request
	fn   func(enroll.Request) (*enroll.Decision, error)
}

func (a *fakeAdmitter) Admit(_ context.Context, req enroll.Request) (*enroll.Decision, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	fn := a.fn
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return admitAll(req), nil
}

func (a *fakeAdmitter) set(fn func(enroll.Request) (*enroll.Decision, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fn = fn
}

func (a *fakeAdmitter) requests() []enroll.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]enroll.Request(nil), a.reqs...)
}

func admitAll(req enroll.Request) *enroll.Decision {
	served := []tunnel.WelcomeService{}
	for _, s := range req.Hello.Services {
		served = append(served, tunnel.WelcomeService{Kind: s.Kind, Name: s.Name, URL: "https://gate.example/agents/" + s.Name})
	}
	return &enroll.Decision{
		Key:           enroll.Key{KeyID: req.Hello.KID, TenantID: testTenantID, Tenant: "team", PubKey: req.PubKey},
		Served:        served,
		HirePrincipal: "node:" + req.Hello.KID,
	}
}

// fakeSink records the sessions the Handler registers.
type fakeSink struct {
	mu    sync.Mutex
	err   error
	added chan *Session
}

func newFakeSink() *fakeSink { return &fakeSink{added: make(chan *Session, 64)} }

func (f *fakeSink) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSink) Add(_ context.Context, s *Session, mux *yamux.Session, served []tunnel.WelcomeService) error {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if s.mux != mux || len(s.served) != len(served) {
		return errors.New("fakeSink: the session's mux or served set is not what Add was given")
	}
	select {
	case f.added <- s:
	default:
	}
	return nil
}

func (f *fakeSink) next(t *testing.T) *Session {
	t.Helper()
	select {
	case s := <-f.added:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no session was registered")
		return nil
	}
}

// logBuf is a logger's output, safe after the test ends; it is printed when the test fails.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// testGate is an in-process gate: the Handler behind httptest, a fake Admitter and a fake sink, and the
// node's HTTP client, whose connections are wires the test can freeze or cut.
type testGate struct {
	srv     *httptest.Server
	h       *Handler
	adm     *fakeAdmitter
	sink    *fakeSink
	wires   *tunneltest.Wires
	client  *http.Client
	gateURL *url.URL // what the node dials
	logs    *logBuf

	active   sync.WaitGroup
	returned chan struct{} // one per ServeHTTP that returned
	cbs      chan []byte   // the gate side's TLS exporter of each upgrade
}

type gateOpts struct {
	tls       bool
	publicURL string // default: what the node dials
	cb        string // default none, tls-exporter over TLS
	admitter  Admitter
	mod       func(*HandlerConfig)
}

func newTestGate(t *testing.T, o gateOpts) *testGate {
	t.Helper()
	g := &testGate{adm: &fakeAdmitter{}, sink: newFakeSink(), wires: &tunneltest.Wires{}, logs: &logBuf{},
		returned: make(chan struct{}, 64), cbs: make(chan []byte, 64)}
	var hp atomic.Pointer[Handler]
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.active.Add(1)
		defer g.active.Done()
		if r.TLS != nil {
			if v, err := tunnel.ChannelBinding(r.TLS); err == nil {
				select {
				case g.cbs <- v:
				default:
				}
			}
		}
		hp.Load().ServeHTTP(w, r)
		select {
		case g.returned <- struct{}{}:
		default:
		}
	}))
	if o.tls {
		g.srv.StartTLS()
		g.wires.Redirect = g.srv.Listener.Addr().String()
		g.client = g.wires.Client(g.srv.Client().Transport.(*http.Transport))
		g.gateURL = mustURL(t, "https://example.com") // httptest's certificate names example.com
	} else {
		g.srv.Start()
		g.client = g.wires.Client(nil)
		g.gateURL = mustURL(t, g.srv.URL)
	}
	t.Cleanup(func() {
		// Every session ends with its node's connection; then every ServeHTTP has returned.
		for _, w := range g.wires.All() {
			_ = w.Cut()
		}
		done := make(chan struct{})
		go func() { g.active.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("a tunnel handler did not return after its node's connection was cut")
		}
		g.srv.Close()
		if t.Failed() {
			t.Logf("gate log:\n%s", g.logs.String())
		}
	})
	cfg := HandlerConfig{
		PublicURL:      g.gateURL.String(),
		ChannelBinding: tunnel.CBNone,
		ReplicaID:      testReplica,
		Admitter:       g.adm,
		Registry:       g.sink,
		Logger:         slog.New(slog.NewTextHandler(g.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if o.publicURL != "" {
		cfg.PublicURL = o.publicURL
	}
	if o.tls {
		cfg.ChannelBinding = tunnel.CBTLSExporter
	}
	if o.cb != "" {
		cfg.ChannelBinding = o.cb
	}
	if o.admitter != nil {
		cfg.Admitter = o.admitter
	}
	if o.mod != nil {
		o.mod(&cfg)
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hp.Store(h)
	g.h = h
	return g
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newIdentity(t *testing.T) *nodekey.Identity {
	t.Helper()
	id, _, err := nodekey.LoadOrCreate(filepath.Join(t.TempDir(), ".kontra", "identity.key"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newHello(services ...string) *tunnel.Hello {
	h := &tunnel.Hello{Name: "build-box-3", Agent: node.UserAgent("test")}
	for _, s := range services {
		h.Services = append(h.Services, tunnel.Service{Kind: tunnel.KindA2A, Name: s})
	}
	return h
}

func (g *testGate) dial(t *testing.T) *node.Dialed {
	t.Helper()
	d, err := node.Dialer{UserAgent: node.UserAgent("test"), HTTPClient: g.client}.Dial(context.Background(), g.gateURL)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// connect is the node's dial and handshake.
func (g *testGate) connect(t *testing.T, id *nodekey.Identity, hello *tunnel.Hello, requireCB bool) (*node.Dialed, *tunnel.Welcome, error) {
	t.Helper()
	d := g.dial(t)
	w, err := node.Handshake(context.Background(), d, id, hello, requireCB)
	return d, w, err
}

// raw is a WebSocket to the gate with the subprotocol, for handshakes written by hand.
func (g *testGate) raw(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, g.gateURL.JoinPath(tunnel.Path).String(), &websocket.DialOptions{
		HTTPClient: g.client, Subprotocols: []string{tunnel.Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func (g *testGate) origin(t *testing.T) string {
	t.Helper()
	o, err := tunnel.CanonicalOrigin(g.gateURL)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// encodeHello is a valid hello of id for origin.
func encodeHello(t *testing.T, id *nodekey.Identity, origin string, services ...string) *tunnel.Hello {
	t.Helper()
	h := newHello(services...)
	h.Origin, h.KID, h.Recipient = origin, id.KeyID(), id.Recipient()
	_, h.Nonce, _ = tunnel.NewNonce()
	if _, err := tunnel.Encode(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func send(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// refusal reads M4 error (at M2's or M4's place) and then the close status.
func refusal(t *testing.T, ws *websocket.Conn) (tunnel.Code, websocket.StatusCode) {
	t.Helper()
	b := receive(t, ws)
	_, err := tunnel.ParseOutcome(b)
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("not an error message: %s (%v)", b, err)
	}
	return herr.Code, closeStatus(t, ws)
}

// closeStatus reads until the connection ends and returns its close status (-1 without a close frame).
func closeStatus(t *testing.T, ws *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("the gate did not close the connection")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// prove is the node's M3 for a challenge, written by hand (cb_value empty: cb none).
func prove(t *testing.T, id *nodekey.Identity, hello *tunnel.Hello, challenge []byte) []byte {
	t.Helper()
	ch, err := tunnel.ParseChallenge(challenge)
	if err != nil {
		t.Fatalf("challenge %s: %v", challenge, err)
	}
	nonce, _ := tunnel.DecodeBase64URL(ch.Nonce)
	info, err := tunnel.Transcript{Origin: hello.Origin, CB: ch.CB, Hello: hello.Raw, Session: ch.Session, GateNonce: nonce}.Info()
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := id.PrivateKey()
	enc, _ := tunnel.DecodeBase64URL(ch.Enc)
	p, err := tunnel.Prove(enc, sk, info, tunnel.LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tunnel.Encode(&tunnel.Proof{Proof: tunnel.EncodeBase64URL(p)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func handshakeCode(t *testing.T, err error) tunnel.Code {
	t.Helper()
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("not a handshake refusal: %v", err)
	}
	return herr.Code
}

// NewHandler refuses what it cannot serve.
func TestNewHandlerRefuses(t *testing.T) {
	ok := HandlerConfig{PublicURL: "https://gate.example", ChannelBinding: tunnel.CBTLSExporter, ReplicaID: "r1",
		Admitter: &fakeAdmitter{}, Registry: newFakeSink()}
	if _, err := NewHandler(ok); err != nil {
		t.Fatal(err)
	}
	keepalive := tunnel.YamuxConfig(nil)
	keepalive.EnableKeepAlive = true
	for name, mod := range map[string]func(*HandlerConfig){
		"public_url":      func(c *HandlerConfig) { c.PublicURL = "gate.example" },
		"channel binding": func(c *HandlerConfig) { c.ChannelBinding = "" },
		"replica":         func(c *HandlerConfig) { c.ReplicaID = "" },
		"admitter":        func(c *HandlerConfig) { c.Admitter = nil },
		"registry":        func(c *HandlerConfig) { c.Registry = nil },
		"yamux keepalive": func(c *HandlerConfig) { c.Yamux = keepalive },
	} {
		c := ok
		mod(&c)
		if _, err := NewHandler(c); err == nil {
			t.Errorf("%s: NewHandler accepted %+v", name, c)
		}
	}
	for d, want := range map[time.Duration]int{20 * time.Second: 20, 200 * time.Millisecond: 1, 1500 * time.Millisecond: 2} {
		if got := pingSeconds(d); got != want {
			t.Errorf("pingSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}

// An upgrade without Sec-WebSocket-Protocol: kontra-tunnel.v1 is 400 (§3), and nothing is admitted.
func TestUpgradeWithoutSubprotocol(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	for _, protos := range [][]string{nil, {"other"}, {"kontra-tunnel.v2"}, {"Kontra-Tunnel.V1"}, {"x, kontra-tunnel.v10"}} {
		req, _ := http.NewRequest(http.MethodGet, g.gateURL.JoinPath(tunnel.Path).String(), nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		for _, p := range protos {
			req.Header.Add("Sec-WebSocket-Protocol", p)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("subprotocols %q: status %d, want 400", protos, resp.StatusCode)
		}
	}
	// The subprotocol among others is accepted (one header, comma-separated).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, g.gateURL.JoinPath(tunnel.Path).String(), &websocket.DialOptions{
		HTTPClient: g.client, Subprotocols: []string{"other", tunnel.Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if resp.StatusCode != http.StatusSwitchingProtocols || ws.Subprotocol() != tunnel.Subprotocol {
		t.Fatalf("status %d, subprotocol %q", resp.StatusCode, ws.Subprotocol())
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
}

// The full handshake against a fake Admitter and a fake sink yields welcome; the node holds a yamux.Client
// (odd ids) and the gate a yamux.Server (even); the first stream the gate opens is op control; a
// node-opened stream is accepted and closed (§4, §7).
func TestHandshakeWelcome(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	id := newIdentity(t)
	hello := newHello("hello", "booking")
	d, w, err := g.connect(t, id, hello, false)
	if err != nil {
		t.Fatal(err)
	}

	// welcome.
	sid, err := uuid.Parse(w.Session)
	if err != nil || sid.Version() != 7 {
		t.Fatalf("welcome session %q: want a UUIDv7", w.Session)
	}
	if w.Replica != testReplica || w.PingS != 20 || w.Tenant != "team" || len(w.Services) != 2 ||
		w.Services[0].Name != "hello" || w.Services[1].URL != "https://gate.example/agents/booking" {
		t.Fatalf("welcome %+v", w)
	}
	if d.Origin != g.origin(t) || hello.Origin != d.Origin || d.CBValue != nil {
		t.Fatalf("dialed origin %q, hello origin %q, cb %x", d.Origin, hello.Origin, d.CBValue)
	}

	// The admission got the proven hello and the keys it was proven for.
	reqs := g.adm.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d admissions", len(reqs))
	}
	pub, _ := tunnel.ParseRecipient(id.Recipient())
	r := reqs[0]
	if r.Hello.KID != id.KeyID() || !bytes.Equal(r.PubKey, pub.Bytes()) || r.OldPubKey != nil ||
		r.SessionID != sid || r.ReplicaID != testReplica || !bytes.Equal(r.Hello.Raw, hello.Raw) {
		t.Fatalf("admission request %+v", r)
	}

	// The registered session.
	s := g.sink.next(t)
	if s.ID != sid || s.KeyID != id.KeyID() || s.TenantID != testTenantID || s.Tenant != "team" ||
		s.HirePrincipal != "node:"+id.KeyID() || s.NodeName != "build-box-3" || s.CLIVersion != hello.Agent {
		t.Fatalf("session %+v", s)
	}
	if s.served["hello"] != tunnel.KindA2A || s.served["booking"] != tunnel.KindA2A || len(s.served) != 2 {
		t.Fatalf("served %v", s.served)
	}

	// yamux: the node is the client.
	nodeMux, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeMux.Close()
	st, err := nodeMux.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(st)
	p, err := tunnel.ReadPreamble(br)
	if err != nil || p.Op != tunnel.OpControl || p.Service != "" {
		t.Fatalf("first gate-opened stream: preamble %+v, %v; want op control", p, err)
	}
	if st.StreamID()%2 != 0 {
		t.Fatalf("the gate opened stream %d: a yamux.Server's ids are even", st.StreamID())
	}
	// A control line written by the sink reaches the node on it (§13).
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err != nil {
		t.Fatal(err)
	}
	if c, err := tunnel.ReadControl(br); err != nil || c.T != tunnel.CtlDrain {
		t.Fatalf("control line %+v, %v", c, err)
	}
	// The gate's next stream is even too.
	go func() {
		gs, err := s.Mux().OpenStream()
		if err == nil {
			_, _ = gs.Write([]byte("x"))
			_ = gs.Close()
		}
	}()
	st2, err := nodeMux.AcceptStream()
	if err != nil || st2.StreamID()%2 != 0 || st2.StreamID() == st.StreamID() {
		t.Fatalf("second gate-opened stream %v, %v", st2, err)
	}

	// A node-opened stream (odd) is accepted promptly and closed: hiring is M3.
	ns, err := nodeMux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if ns.StreamID()%2 != 1 {
		t.Fatalf("the node opened stream %d: a yamux.Client's ids are odd", ns.StreamID())
	}
	_ = ns.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := ns.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("node-opened stream: read %d, %v; want the gate to close it (EOF)", n, err)
	}

	// The node leaves: the gate's session closes and its handler returns.
	_ = nodeMux.Close()
	eventually(t, 10*time.Second, "the gate's session closed", s.Mux().IsClosed)
	select {
	case <-g.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the node left")
	}
}

// Over httptest TLS with cb tls-exporter the exporters are equal at both ends and the proof verifies; a
// node with --require-channel-binding accepts it (§4.1).
func TestHandshakeTLSExporter(t *testing.T) {
	g := newTestGate(t, gateOpts{tls: true})
	d, w, err := g.connect(t, newIdentity(t), newHello("hello"), true)
	if err != nil {
		t.Fatal(err)
	}
	if w.Session == "" {
		t.Fatal("no welcome")
	}
	gateCB := <-g.cbs
	if len(d.CBValue) != tunnel.ChannelBindingSize || !bytes.Equal(d.CBValue, gateCB) {
		t.Fatalf("node exporter %x, gate exporter %x: want equal, 32 bytes", d.CBValue, gateCB)
	}
	if d.Origin != "https://example.com" {
		t.Fatalf("origin %q", d.Origin)
	}
}

// A node started with --require-channel-binding refuses cb none and sends no proof (§4.1).
func TestRequireChannelBindingRefusesNone(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	_, _, err := g.connect(t, newIdentity(t), newHello("hello"), true)
	if !errors.Is(err, node.ErrChannelBindingRequired) {
		t.Fatalf("handshake = %v, want ErrChannelBindingRequired", err)
	}
	select {
	case <-g.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate kept the connection")
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions: the node sent a proof", n)
	}
}

// A TLS-terminating middlebox (two TLS legs, each with its own exporter) gives bad_proof (§4.1).
func TestMiddleboxGivesBadProof(t *testing.T) {
	g := newTestGate(t, gateOpts{tls: true})
	roots := x509.NewCertPool()
	roots.AddCert(g.srv.Certificate())
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: g.srv.TLS.Certificates})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := tls.Dial("tcp", g.srv.Listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "example.com"})
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	// The node dials example.com and reaches the middlebox, which it trusts (a corporate root, here the
	// same certificate).
	ws := &tunneltest.Wires{Redirect: l.Addr().String()}
	d, err := node.Dialer{HTTPClient: ws.Client(g.srv.Client().Transport.(*http.Transport))}.Dial(context.Background(), g.gateURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = node.Handshake(context.Background(), d, newIdentity(t), newHello("hello"), true)
	if code := handshakeCode(t, err); code != tunnel.CodeBadProof || code.Retryable() {
		t.Fatalf("through a TLS-terminating middlebox: %s, want bad_proof (not retried)", code)
	}
	if gateCB := <-g.cbs; bytes.Equal(gateCB, d.CBValue) {
		t.Fatal("the two TLS legs have the same exporter")
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
}

// With tunnel.channel_binding tls-exporter, a connection without an exporter is refused before the
// upgrade, never answered with cb none (M2 plan audit).
func TestExporterUnavailableIsRefused(t *testing.T) {
	adm := &fakeAdmitter{}
	h, err := NewHandler(HandlerConfig{PublicURL: "https://gate.example", ChannelBinding: tunnel.CBTLSExporter,
		ReplicaID: "r1", Admitter: adm, Registry: newFakeSink(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		state  *tls.ConnectionState
		status int
	}{
		// A ConnectionState whose exporter fails (none: not from a tls.Conn; on the wire, TLS 1.2 without
		// Extended Master Secret).
		{"exporter fails", &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS12}, http.StatusBadRequest},
		{"handshake incomplete", &tls.ConnectionState{}, http.StatusBadRequest},
		{"no TLS", nil, http.StatusInternalServerError},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://gate.example/v1/tunnel", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.Header.Set("Sec-WebSocket-Protocol", tunnel.Subprotocol)
		req.TLS = c.state
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.status || rec.Header().Get("Upgrade") != "" || strings.Contains(rec.Body.String(), tunnel.MsgChallenge) {
			t.Errorf("%s: status %d, headers %v: want %d and no upgrade", c.name, rec.Code, rec.Header(), c.status)
		}
	}
	if len(adm.requests()) != 0 {
		t.Fatal("an admission without a channel binding")
	}

	// Live: a gate that binds tls-exporter on a plain listener refuses the node's dial.
	g := newTestGate(t, gateOpts{cb: tunnel.CBTLSExporter})
	_, err = node.Dialer{HTTPClient: g.client}.Dial(context.Background(), g.gateURL)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "tls-exporter") {
		t.Fatalf("dial = %v, want the upgrade refused with 500, saying why", err)
	}
}

// A hello whose origin differs from public_url's gets bad_origin and close status 4001 (§4).
func TestBadOrigin(t *testing.T) {
	g := newTestGate(t, gateOpts{publicURL: "https://gate.example/"})
	_, _, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if code := handshakeCode(t, err); code != tunnel.CodeBadOrigin || code.Retryable() {
		t.Fatalf("handshake: %s, want bad_origin", code)
	}

	ws := g.raw(t)
	h := encodeHello(t, newIdentity(t), "https://elsewhere.example", "hello")
	send(t, ws, h.Raw)
	code, status := refusal(t, ws)
	if code != tunnel.CodeBadOrigin || status != 4001 {
		t.Fatalf("refusal %s, close %d; want bad_origin, 4001", code, status)
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
}

// A node that sends hello and never sends proof is closed 10 s (±1 s) after the upgrade (§4).
func TestHandshakeTimeout(t *testing.T) {
	t.Parallel()
	g := newTestGate(t, gateOpts{})
	ws := g.raw(t)
	upgraded := time.Now()
	h := encodeHello(t, newIdentity(t), g.origin(t), "hello")
	send(t, ws, h.Raw)
	if b := receive(t, ws); !bytes.Contains(b, []byte(`"challenge"`)) {
		t.Fatalf("M2 %s", b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	took := time.Since(upgraded)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("read %v after %v: the gate kept the connection", err, took)
	}
	if took < tunnel.HandshakeTimeout-time.Second || took > tunnel.HandshakeTimeout+time.Second {
		t.Fatalf("closed %v after the upgrade, want 10 s ± 1 s", took)
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
}

// A handshake message over 16 KiB closes the connection, and so does a binary one (§4).
func TestHandshakeMessageLimits(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	// Far over the limit: the WebSocket read limit closes with 1009 before the message is read.
	ws := g.raw(t)
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	_ = ws.Write(wctx, websocket.MessageText, bytes.Repeat([]byte("x"), 64<<10)) // the gate may close mid-write
	if status := closeStatus(t, ws); status != websocket.StatusMessageTooBig {
		t.Fatalf("64 KiB hello: close %d, want %d", status, websocket.StatusMessageTooBig)
	}

	// One byte over: refused as no v1 hello (unsupported_version, 4000), or by the read limit.
	ws = g.raw(t)
	h := encodeHello(t, newIdentity(t), g.origin(t), "hello")
	var m map[string]any
	_ = json.Unmarshal(h.Raw, &m)
	m["pad"] = ""
	b, _ := json.Marshal(m)
	m["pad"] = strings.Repeat("p", tunnel.MaxHandshakeMessage+1-len(b))
	b, _ = json.Marshal(m)
	if len(b) != tunnel.MaxHandshakeMessage+1 {
		t.Fatalf("padded hello of %d bytes", len(b))
	}
	send(t, ws, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s != 4000 && s != websocket.StatusMessageTooBig {
				t.Fatalf("hello of 16 KiB + 1: closed %v", err)
			}
			break
		}
		if bytes.Contains(msg, []byte(`"challenge"`)) {
			t.Fatal("a challenge to a hello over 16 KiB")
		}
	}

	// A binary hello is no v1 hello.
	ws = g.raw(t)
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	if err := ws.Write(cctx, websocket.MessageBinary, h.Raw); err != nil {
		t.Fatal(err)
	}
	if code, status := refusal(t, ws); code != tunnel.CodeUnsupportedVersion || status != 4000 {
		t.Fatalf("binary hello: %s, %d", code, status)
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
}

// A draining replica answers draining (4010), which the node retries; the drain refuses up to the proof,
// never after an admission (§4, relay.md §14.3).
func TestDraining(t *testing.T) {
	var draining atomic.Bool
	draining.Store(true)
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.Draining = draining.Load }})
	_, _, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if code := handshakeCode(t, err); code != tunnel.CodeDraining || !code.Retryable() {
		t.Fatalf("handshake: %s, want draining (retried)", code)
	}
	ws := g.raw(t)
	send(t, ws, encodeHello(t, newIdentity(t), g.origin(t), "hello").Raw)
	if code, status := refusal(t, ws); code != tunnel.CodeDraining || status != 4010 {
		t.Fatalf("raw: %s, %d", code, status)
	}

	// The drain starts between hello and proof: refused before Admit.
	draining.Store(false)
	ws = g.raw(t)
	id := newIdentity(t)
	h := encodeHello(t, id, g.origin(t), "hello")
	send(t, ws, h.Raw)
	ch := receive(t, ws)
	draining.Store(true)
	send(t, ws, prove(t, id, h, ch))
	if code, status := refusal(t, ws); code != tunnel.CodeDraining || status != 4010 {
		t.Fatalf("drain after hello: %s, %d", code, status)
	}
	if n := len(g.adm.requests()); n != 0 {
		t.Fatalf("%d admissions while draining", n)
	}

	// The drain starts during Admit: the admission stands, so the handshake ends in welcome.
	draining.Store(false)
	g.adm.set(func(req enroll.Request) (*enroll.Decision, error) {
		draining.Store(true)
		return admitAll(req), nil
	})
	if _, _, err := g.connect(t, newIdentity(t), newHello("hello"), false); err != nil {
		t.Fatalf("drain during Admit: %v, want welcome", err)
	}
}

// Replaying a recorded hello and proof on a new connection gives bad_proof (4002): the session, the gate
// nonce and enc are fresh per connection (§4.1, no replay; M2 plan audit).
func TestReplayGivesBadProof(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	id := newIdentity(t)
	ws := g.raw(t)
	h := encodeHello(t, id, g.origin(t), "hello")
	send(t, ws, h.Raw)
	ch1 := receive(t, ws)
	proof := prove(t, id, h, ch1)
	send(t, ws, proof)
	if w, err := tunnel.ParseOutcome(receive(t, ws)); err != nil || w == nil {
		t.Fatalf("the recorded handshake: %v", err)
	}

	replay := g.raw(t)
	send(t, replay, h.Raw)
	ch2 := receive(t, replay)
	c1, _ := tunnel.ParseChallenge(ch1)
	c2, err := tunnel.ParseChallenge(ch2)
	if err != nil || c1.Session == c2.Session || c1.Nonce == c2.Nonce || c1.Enc == c2.Enc {
		t.Fatalf("the second challenge is not fresh: %+v / %+v", c1, c2)
	}
	send(t, replay, proof)
	if code, status := refusal(t, replay); code != tunnel.CodeBadProof || status != 4002 {
		t.Fatalf("replayed proof: %s, %d; want bad_proof, 4002", code, status)
	}
	if n := len(g.adm.requests()); n != 1 {
		t.Fatalf("%d admissions, want the original's only", n)
	}
}

// A gate the node dialed cannot relay it to another gate (§4.1, no relay; M2 plan audit): one that
// forwards the node's hello unmodified gets bad_origin (4001), one that rewrites the origin bad_proof
// (4002). The node binds the origin it dialed.
func TestRelayToAnotherGate(t *testing.T) {
	for _, c := range []struct {
		name    string
		rewrite bool
		code    tunnel.Code
		status  websocket.StatusCode
	}{
		{"forwarded unmodified", false, tunnel.CodeBadOrigin, 4001},
		{"origin rewritten", true, tunnel.CodeBadProof, 4002},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestGate(t, gateOpts{})
			bOrigin := b.origin(t)
			statuses := make(chan websocket.StatusCode, 1)
			m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nc, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnel.Subprotocol}})
				if err != nil {
					return
				}
				defer nc.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, hello, err := nc.Read(ctx)
				if err != nil {
					return
				}
				if c.rewrite {
					var fields map[string]any
					_ = json.Unmarshal(hello, &fields)
					fields["origin"] = bOrigin
					hello, _ = json.Marshal(fields)
				}
				bc, _, err := websocket.Dial(ctx, b.gateURL.JoinPath(tunnel.Path).String(), &websocket.DialOptions{
					HTTPClient: b.client, Subprotocols: []string{tunnel.Subprotocol}})
				if err != nil {
					return
				}
				defer bc.CloseNow()
				relay := func(from, to *websocket.Conn) ([]byte, error) {
					_, msg, err := from.Read(ctx)
					if err != nil {
						return nil, err
					}
					return msg, to.Write(ctx, websocket.MessageText, msg)
				}
				end := func() {
					_, _, err := bc.Read(ctx)
					statuses <- websocket.CloseStatus(err)
				}
				_ = bc.Write(ctx, websocket.MessageText, hello)
				m2, err := relay(bc, nc) // challenge or error
				if err != nil || bytes.Contains(m2, []byte(`"error"`)) {
					end()
					return
				}
				if _, err := relay(nc, bc); err != nil { // proof
					return
				}
				_, _ = relay(bc, nc) // welcome or error
				end()
			}))
			defer m.Close()

			ws := &tunneltest.Wires{}
			d, err := node.Dialer{HTTPClient: ws.Client(nil)}.Dial(context.Background(), mustURL(t, m.URL))
			if err != nil {
				t.Fatal(err)
			}
			_, err = node.Handshake(context.Background(), d, newIdentity(t), newHello("hello"), false)
			if code := handshakeCode(t, err); code != c.code {
				t.Fatalf("node: %s, want %s", code, c.code)
			}
			select {
			case s := <-statuses:
				if s != c.status {
					t.Fatalf("the other gate closed %d, want %d", s, c.status)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the relay saw no close")
			}
			if n := len(b.adm.requests()); n != 0 {
				t.Fatalf("the other gate admitted %d", n)
			}
		})
	}
}

// Admit's refusal is the node's (§4: 4000 + i); its own failure closes without a code, which the node
// reads as an unknown outcome (M2 plan audit, gap 1).
func TestAdmitOutcomes(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	g.adm.set(func(enroll.Request) (*enroll.Decision, error) {
		return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "the token was used")
	})
	_, _, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if code := handshakeCode(t, err); code != tunnel.CodeEnrollInvalid || code.Retryable() {
		t.Fatalf("handshake: %s", code)
	}
	ws := g.raw(t)
	id := newIdentity(t)
	h := encodeHello(t, id, g.origin(t), "hello")
	send(t, ws, h.Raw)
	send(t, ws, prove(t, id, h, receive(t, ws)))
	if code, status := refusal(t, ws); code != tunnel.CodeEnrollInvalid || status != 4005 {
		t.Fatalf("raw: %s, %d", code, status)
	}

	g.adm.set(func(enroll.Request) (*enroll.Decision, error) { return nil, errors.New("database unreachable") })
	_, _, err = g.connect(t, newIdentity(t), newHello("hello"), false)
	if !errors.Is(err, node.ErrNoOutcome) {
		t.Fatalf("handshake = %v, want ErrNoOutcome", err)
	}
	ws = g.raw(t)
	h = encodeHello(t, id, g.origin(t), "hello")
	send(t, ws, h.Raw)
	send(t, ws, prove(t, id, h, receive(t, ws)))
	if status := closeStatus(t, ws); status != websocket.StatusInternalError {
		t.Fatalf("raw: close %d, want %d and no message", status, websocket.StatusInternalError)
	}
}

// Rotation proves both keys: enc_old and proof_old (§4.1, §12); a missing, wrong or unasked proof_old is
// bad_proof.
func TestRotationProofs(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	old := newIdentity(t)
	id, err := nodekey.Rotate(old.Path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hello := newHello("hello")
	hello.RotateFrom = &tunnel.KeyRef{KID: old.KeyID(), Recipient: old.Recipient()}
	if _, _, err := g.connect(t, id, hello, false); err != nil {
		t.Fatal(err)
	}
	r := g.adm.requests()[0]
	oldPub, _ := tunnel.ParseRecipient(old.Recipient())
	newPub, _ := tunnel.ParseRecipient(id.Recipient())
	if !bytes.Equal(r.OldPubKey, oldPub.Bytes()) || !bytes.Equal(r.PubKey, newPub.Bytes()) || r.Hello.RotateFrom.KID != old.KeyID() {
		t.Fatalf("admission request: old %x new %x", r.OldPubKey, r.PubKey)
	}

	for _, c := range []struct {
		name   string
		rotate bool
		old    func(p *tunnel.Proof)
	}{
		{"missing proof_old", true, func(p *tunnel.Proof) { p.ProofOld = "" }},
		{"wrong proof_old", true, func(p *tunnel.Proof) { p.ProofOld = p.Proof }},
		{"proof_old unasked", false, func(p *tunnel.Proof) { p.ProofOld = p.Proof }},
	} {
		ws := g.raw(t)
		h := encodeHello(t, id, g.origin(t), "hello")
		if c.rotate {
			h.RotateFrom = hello.RotateFrom
			h.Raw = nil
			if _, err := tunnel.Encode(h); err != nil {
				t.Fatal(err)
			}
		}
		send(t, ws, h.Raw)
		chb := receive(t, ws)
		ch, _ := tunnel.ParseChallenge(chb)
		if (ch.EncOld != "") != c.rotate {
			t.Fatalf("%s: enc_old %q", c.name, ch.EncOld)
		}
		var p tunnel.Proof
		_ = json.Unmarshal(prove(t, id, h, chb), &p)
		c.old(&p)
		pb, _ := tunnel.Encode(&p)
		send(t, ws, pb)
		if code, status := refusal(t, ws); code != tunnel.CodeBadProof || status != 4002 {
			t.Fatalf("%s: %s, %d", c.name, code, status)
		}
	}
	if n := len(g.adm.requests()); n != 1 {
		t.Fatalf("%d admissions", n)
	}
}

// A sink that cannot register the session closes it after welcome; the node sees its session end.
func TestSinkErrorClosesSession(t *testing.T) {
	g := newTestGate(t, gateOpts{})
	g.sink.fail(errors.New("tunnels row not inserted"))
	d, _, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	select {
	case <-m.CloseChan():
	case <-time.After(10 * time.Second):
		t.Fatal("the session stayed open")
	}
}

// The gate's own ping loop closes a node that went silent (a frozen path), PingEvery shortened to 200 ms
// and the pong deadline to 300 ms (§11, D33).
func TestPingLoopClosesSilentNode(t *testing.T) {
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) {
		c.PingEvery = 200 * time.Millisecond
		c.Yamux = tunneltest.ShortConfig(300 * time.Millisecond)
	}})
	d, w, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	if w.PingS != 1 {
		t.Fatalf("ping_s %d", w.PingS)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := g.sink.next(t)
	time.Sleep(time.Second) // healthy pings
	if s.Mux().IsClosed() {
		t.Fatal("the session closed while the node answered")
	}
	g.wires.Last().Freeze()
	frozen := time.Now()
	eventually(t, 5*time.Second, "the gate closed the silent node's session", s.Mux().IsClosed)
	if took := time.Since(frozen); took < 900*time.Millisecond {
		t.Fatalf("closed %v after the freeze: before three pong deadlines", took)
	}
	eventually(t, 10*time.Second, "the ping loop logs why it closed the session", func() bool {
		return strings.Contains(g.logs.String(), "the node is silent")
	})
}

// WriteControl on a session whose control stream is gone fails rather than blocks.
func TestWriteControlWithoutStream(t *testing.T) {
	s := &Session{}
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("WriteControl = %v", err)
	}
	a, b := net.Pipe()
	defer b.Close()
	s.control = a
	_ = a.Close()
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err == nil {
		t.Fatal("WriteControl on a closed stream succeeded")
	}
}
