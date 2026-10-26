package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// fakeGate is a scripted gate: it upgrades (selecting the subprotocol unless told not to), reads the
// hello and runs script, which reports what it saw on the channel.
type fakeGate struct {
	srv     *httptest.Server
	client  *http.Client
	url     *url.URL
	wires   *tunneltest.Wires
	headers chan http.Header
	cbs     chan []byte
	errs    chan error
}

type gateScript func(ctx context.Context, ws *websocket.Conn, hello *tunnel.Hello, cb []byte) error

func newFakeGate(t *testing.T, useTLS, selectProto bool, script gateScript) *fakeGate {
	t.Helper()
	g := &fakeGate{wires: &tunneltest.Wires{}, headers: make(chan http.Header, 8), cbs: make(chan []byte, 8), errs: make(chan error, 8)}
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.headers <- r.Header.Clone()
		var cb []byte
		if r.TLS != nil {
			cb, _ = tunnel.ChannelBinding(r.TLS)
			g.cbs <- cb
		}
		opts := &websocket.AcceptOptions{}
		if selectProto {
			opts.Subprotocols = []string{tunnel.Subprotocol}
		}
		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			g.errs <- err
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, b, err := ws.Read(ctx)
		if err != nil {
			g.errs <- fmt.Errorf("read hello: %w", err)
			return
		}
		h, err := tunnel.ParseHello(b)
		if err != nil {
			g.errs <- fmt.Errorf("hello: %w", err)
			return
		}
		g.errs <- script(ctx, ws, h, cb)
	}))
	if useTLS {
		g.srv.StartTLS()
		g.wires.Redirect = g.srv.Listener.Addr().String()
		g.client = g.wires.Client(g.srv.Client().Transport.(*http.Transport))
		g.url, _ = url.Parse("https://example.com")
	} else {
		g.srv.Start()
		g.client = g.wires.Client(nil)
		g.url, _ = url.Parse(g.srv.URL)
	}
	t.Cleanup(func() {
		for _, w := range g.wires.All() {
			_ = w.Cut()
		}
		g.srv.Close()
	})
	return g
}

// result is what the script reported.
func (g *fakeGate) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-g.errs:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the fake gate's script did not finish")
		return nil
	}
}

func (g *fakeGate) dial(t *testing.T) *Dialed {
	t.Helper()
	d, err := Dialer{UserAgent: "kontra/test (linux/amd64)", HTTPClient: g.client}.Dial(context.Background(), g.url)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func testIdentity(t *testing.T) *nodekey.Identity {
	t.Helper()
	id, _, err := nodekey.LoadOrCreate(filepath.Join(t.TempDir(), ".kontra", "identity.key"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testHello() *tunnel.Hello {
	return &tunnel.Hello{Name: "box", Agent: UserAgent("test"), Services: []tunnel.Service{{Kind: tunnel.KindA2A, Name: "hello"}}}
}

// challenge is the gate's M2 for h, and the proofs it expects (§4.1).
type challenge struct {
	msg           tunnel.Challenge
	want, wantOld []byte
}

func newChallenge(h *tunnel.Hello, cb string, cbValue []byte) (*challenge, error) {
	session := uuid.Must(uuid.NewV7()).String()
	gn, gnEnc, _ := tunnel.NewNonce()
	info, err := tunnel.Transcript{Origin: h.Origin, CB: cb, CBValue: cbValue, Hello: h.Raw, Session: session, GateNonce: gn}.Info()
	if err != nil {
		return nil, err
	}
	pub, err := tunnel.ParseRecipient(h.Recipient)
	if err != nil {
		return nil, err
	}
	enc, want, err := tunnel.Encapsulate(pub, info, tunnel.LabelPoP)
	if err != nil {
		return nil, err
	}
	c := &challenge{msg: tunnel.Challenge{Session: session, Nonce: gnEnc, CB: cb, Enc: tunnel.EncodeBase64URL(enc)}, want: want}
	if h.RotateFrom != nil {
		op, err := tunnel.ParseRecipient(h.RotateFrom.Recipient)
		if err != nil {
			return nil, err
		}
		encOld, wantOld, err := tunnel.Encapsulate(op, info, tunnel.LabelPoPOld)
		if err != nil {
			return nil, err
		}
		c.msg.EncOld, c.wantOld = tunnel.EncodeBase64URL(encOld), wantOld
	}
	return c, nil
}

func write(ctx context.Context, ws *websocket.Conn, msg any) error {
	b, err := tunnel.Encode(msg)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, b)
}

// verifyProof reads M3 and checks it against c, as the gate does.
func verifyProof(ctx context.Context, ws *websocket.Conn, c *challenge) error {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return fmt.Errorf("read proof: %w", err)
	}
	p, err := tunnel.ParseProof(b)
	if err != nil {
		return err
	}
	proof, _ := tunnel.DecodeBase64URL(p.Proof)
	if !tunnel.Accept(proof, c.want) {
		return errors.New("the proof does not verify")
	}
	if c.wantOld != nil {
		po, _ := tunnel.DecodeBase64URL(p.ProofOld)
		if !tunnel.Accept(po, c.wantOld) {
			return errors.New("proof_old does not verify")
		}
	} else if p.ProofOld != "" {
		return errors.New("proof_old without rotate_from")
	}
	return nil
}

// welcomeScript completes the handshake as a gate binding cb, verifying the node's proofs.
func welcomeScript(cb string) gateScript {
	return func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, cbValue []byte) error {
		if cb == tunnel.CBNone {
			cbValue = nil
		}
		c, err := newChallenge(h, cb, cbValue)
		if err != nil {
			return err
		}
		if err := write(ctx, ws, &c.msg); err != nil {
			return err
		}
		if err := verifyProof(ctx, ws, c); err != nil {
			return err
		}
		return write(ctx, ws, &tunnel.Welcome{Session: c.msg.Session, Replica: "r1", PingS: 20, Tenant: "team",
			Services: []tunnel.WelcomeService{{Kind: tunnel.KindA2A, Name: "hello", URL: "https://example.com/agents/hello"}}})
	}
}

// The node's half against the spec's arithmetic, independently of the gate's code: the proof, proof_old
// under rotation, and the TLS exporter (§4.1).
func TestHandshakeProves(t *testing.T) {
	for _, c := range []struct {
		name   string
		tls    bool
		cb     string
		rotate bool
	}{
		{"cb none", false, tunnel.CBNone, false},
		{"tls-exporter", true, tunnel.CBTLSExporter, false},
		{"tls-exporter, rotation", true, tunnel.CBTLSExporter, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGate(t, c.tls, true, welcomeScript(c.cb))
			id := testIdentity(t)
			h := testHello()
			if c.rotate {
				old := id
				var err error
				if id, err = nodekey.Rotate(old.Path, time.Now()); err != nil {
					t.Fatal(err)
				}
				h.RotateFrom = &tunnel.KeyRef{KID: old.KeyID(), Recipient: old.Recipient()}
			}
			d := g.dial(t)
			w, err := Handshake(context.Background(), d, id, h, c.tls)
			if err != nil {
				t.Fatalf("handshake: %v (gate: %v)", err, g.result(t))
			}
			if err := g.result(t); err != nil {
				t.Fatalf("gate: %v", err)
			}
			if w.Tenant != "team" || len(w.Services) != 1 {
				t.Fatalf("welcome %+v", w)
			}
			if h.KID != id.KeyID() || h.Recipient != id.Recipient() || h.Origin != d.Origin || h.Raw == nil {
				t.Fatalf("hello as sent: %+v", h)
			}
			if c.tls {
				if gateCB := <-g.cbs; len(d.CBValue) != 32 || string(gateCB) != string(d.CBValue) {
					t.Fatalf("exporters: node %x, gate %x", d.CBValue, gateCB)
				}
			} else if d.CBValue != nil {
				t.Fatalf("an exporter over plain http: %x", d.CBValue)
			}
		})
	}
}

// The upgrade carries the subprotocol and the User-Agent, and no credential: tokens and API keys travel
// in the hello, inside the WebSocket (§3).
func TestDialHeaders(t *testing.T) {
	g := newFakeGate(t, false, true, welcomeScript(tunnel.CBNone))
	h := testHello()
	h.Enroll = []string{"ket_0123456789abcdefghijklmnopqrstuv"}
	h.Auth = "kk_live_secret_api_key"
	d := g.dial(t)
	if _, err := Handshake(context.Background(), d, testIdentity(t), h, false); err != nil {
		t.Fatal(err)
	}
	if err := g.result(t); err != nil {
		t.Fatal(err)
	}
	hdr := <-g.headers
	if ua := hdr.Get("User-Agent"); ua != "kontra/test (linux/amd64)" {
		t.Errorf("User-Agent %q", ua)
	}
	if p := hdr.Get("Sec-WebSocket-Protocol"); p != tunnel.Subprotocol {
		t.Errorf("Sec-WebSocket-Protocol %q", p)
	}
	for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Sec-Websocket-Extensions"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("%s: %q", k, v)
		}
	}
	for k, vs := range hdr {
		for _, v := range vs {
			if strings.Contains(v, "ket_") || strings.Contains(v, h.Auth) {
				t.Errorf("a credential in header %s", k)
			}
		}
	}
	if !strings.Contains(string(h.Raw), h.Enroll[0]) || !strings.Contains(string(h.Raw), h.Auth) {
		t.Error("the credentials are not in the hello")
	}
	if UserAgent("4.0.0") != "kontra/4.0.0 ("+runtimeOSArch()+")" {
		t.Errorf("UserAgent = %q", UserAgent("4.0.0"))
	}
}

// A gate that does not select kontra-tunnel.v1 is closed and reported (§3).
func TestDialRequiresSubprotocol(t *testing.T) {
	closed := make(chan websocket.StatusCode, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil) // selects nothing
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, err = ws.Read(ctx)
		closed <- websocket.CloseStatus(err)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, err := Dial(context.Background(), u, "")
	if !errors.Is(err, ErrSubprotocol) {
		t.Fatalf("Dial = %v, want ErrSubprotocol", err)
	}
	select {
	case s := <-closed:
		if s != websocket.StatusProtocolError {
			t.Fatalf("the node closed with %d, want %d", s, websocket.StatusProtocolError)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node kept the connection")
	}
}

// The hello goes only to the origin dialed: no redirect is followed, and a URL with credentials or of a
// non-local http gate is refused before any connection (§2.4, §3).
func TestDialRefuses(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/tunnel", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	ws := &tunneltest.Wires{}
	u, _ := url.Parse(redirect.URL)
	if _, err := (Dialer{HTTPClient: ws.Client(nil)}).Dial(context.Background(), u); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Dial through a redirect = %v", err)
	}
	if _, err := Dial(context.Background(), u, ""); err == nil {
		t.Fatal("Dial with the default client followed a redirect")
	}
	if n := len(ws.All()); n != 1 {
		t.Fatalf("%d connections, want only the redirecting gate's", n)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect's target got %d requests", n)
	}

	for i, raw := range []string{"https://user:secret@gate.example", "http://gate.example", "https://gate.example/?k=v"} {
		u, _ := url.Parse(raw)
		ws := &tunneltest.Wires{}
		_, err := Dialer{HTTPClient: ws.Client(nil)}.Dial(context.Background(), u)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("case %d: Dial = %v", i, err)
		}
		if len(ws.All()) != 0 {
			t.Errorf("case %d: Dial connected", i)
		}
	}
}

// The failures of the node's half, and what each means for the retry (§4; M2 plan audit, gap 1).
func TestHandshakeFailures(t *testing.T) {
	// The gate never receives a proof in these.
	noProof := func(ctx context.Context, ws *websocket.Conn) error {
		_, b, err := ws.Read(ctx)
		if err == nil {
			return fmt.Errorf("the node sent %s", b)
		}
		return nil
	}
	cases := []struct {
		name      string
		tls       bool
		requireCB bool
		hello     func(*tunnel.Hello)
		script    gateScript
		check     func(t *testing.T, err error)
	}{
		{
			name: "cb none with --require-channel-binding", requireCB: true,
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				_ = write(ctx, ws, &c.msg)
				return noProof(ctx, ws)
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrChannelBindingRequired) {
					t.Fatalf("%v", err)
				}
			},
		},
		{
			name: "tls-exporter without TLS",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, err := newChallenge(h, tunnel.CBTLSExporter, make([]byte, 32))
				if err != nil {
					return err
				}
				_ = write(ctx, ws, &c.msg)
				return noProof(ctx, ws)
			},
			check: func(t *testing.T, err error) {
				if err == nil || errors.Is(err, ErrNoOutcome) || !strings.Contains(err.Error(), "exporter") {
					t.Fatalf("%v", err)
				}
			},
		},
		{
			name: "refused at M2",
			script: func(ctx context.Context, ws *websocket.Conn, _ *tunnel.Hello, _ []byte) error {
				_ = write(ctx, ws, &tunnel.ErrorMessage{Code: tunnel.CodeDraining, Message: "draining"})
				_ = ws.Close(4010, "draining")
				return nil
			},
			check: func(t *testing.T, err error) {
				var herr *tunnel.HandshakeError
				if !errors.As(err, &herr) || herr.Code != tunnel.CodeDraining || herr.Message != "draining" {
					t.Fatalf("%v", err)
				}
			},
		},
		{
			name: "enc_old without rotate_from",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				c.msg.EncOld = c.msg.Enc
				_ = write(ctx, ws, &c.msg)
				return noProof(ctx, ws)
			},
			check: func(t *testing.T, err error) {
				if err == nil || errors.Is(err, ErrNoOutcome) {
					t.Fatalf("%v", err)
				}
			},
		},
		{
			name: "error message lost, close status kept",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				_ = write(ctx, ws, &c.msg)
				if err := verifyProof(ctx, ws, c); err != nil {
					return err
				}
				_ = ws.Close(4006, "service_denied")
				return nil
			},
			check: func(t *testing.T, err error) {
				var herr *tunnel.HandshakeError
				if !errors.As(err, &herr) || herr.Code != tunnel.CodeServiceDenied || herr.Code.Retryable() {
					t.Fatalf("%v", err)
				}
			},
		},
		{
			name: "connection lost after the proof",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				_ = write(ctx, ws, &c.msg)
				if err := verifyProof(ctx, ws, c); err != nil {
					return err
				}
				_ = ws.CloseNow() // as a gate that committed and died
				return nil
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoOutcome) {
					t.Fatalf("%v, want ErrNoOutcome", err)
				}
				var herr *tunnel.HandshakeError
				if errors.As(err, &herr) {
					t.Fatalf("a lost outcome read as a refusal: %v", err)
				}
			},
		},
		{
			name: "the gate's own failure after the proof (1011)",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				_ = write(ctx, ws, &c.msg)
				if err := verifyProof(ctx, ws, c); err != nil {
					return err
				}
				_ = ws.Close(websocket.StatusInternalError, "")
				return nil
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoOutcome) {
					t.Fatalf("%v, want ErrNoOutcome", err)
				}
			},
		},
		{
			name: "welcome for another session",
			script: func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, _ []byte) error {
				c, _ := newChallenge(h, tunnel.CBNone, nil)
				_ = write(ctx, ws, &c.msg)
				if err := verifyProof(ctx, ws, c); err != nil {
					return err
				}
				return write(ctx, ws, &tunnel.Welcome{Session: uuid.Must(uuid.NewV7()).String(), Replica: "r", PingS: 20})
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoOutcome) || !strings.Contains(err.Error(), "session") {
					t.Fatalf("%v", err)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGate(t, c.tls, true, c.script)
			h := testHello()
			d := g.dial(t)
			_, err := Handshake(context.Background(), d, testIdentity(t), h, c.requireCB)
			c.check(t, err)
			if gerr := g.result(t); gerr != nil {
				t.Fatalf("gate: %v", gerr)
			}
		})
	}
}

// Handshake owns the hello's origin, key and nonce: a fresh nonce on every connection (§4.1), and nothing
// sent for a hello that names another origin or key, or a rotate_from the identity file cannot prove.
func TestHandshakeHello(t *testing.T) {
	nonces := make(chan string, 2)
	g := newFakeGate(t, false, true, func(ctx context.Context, ws *websocket.Conn, h *tunnel.Hello, cb []byte) error {
		nonces <- h.Nonce
		return welcomeScript(tunnel.CBNone)(ctx, ws, h, cb)
	})
	id := testIdentity(t)
	h := testHello()
	for range 2 {
		if _, err := Handshake(context.Background(), g.dial(t), id, h, false); err != nil {
			t.Fatal(err)
		}
		if err := g.result(t); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := <-nonces, <-nonces; a == b {
		t.Fatalf("the same nonce on two connections: %s", a)
	}

	other := testIdentity(t)
	for name, mod := range map[string]func(*tunnel.Hello){
		"origin":      func(h *tunnel.Hello) { h.Origin = "https://elsewhere.example" },
		"recipient":   func(h *tunnel.Hello) { h.Recipient = other.Recipient() },
		"kid":         func(h *tunnel.Hello) { h.KID = other.KeyID() },
		"rotate_from": func(h *tunnel.Hello) { h.RotateFrom = &tunnel.KeyRef{KID: other.KeyID(), Recipient: other.Recipient()} },
	} {
		h := testHello()
		mod(h)
		d := g.dial(t)
		if _, err := Handshake(context.Background(), d, id, h, false); err == nil {
			t.Errorf("%s: a hello the identity cannot stand behind was accepted", name)
		}
		if err := g.result(t); err == nil || !strings.Contains(err.Error(), "read hello") {
			t.Errorf("%s: the gate got a hello (%v)", name, err)
		}
	}
}

func runtimeOSArch() string { return runtime.GOOS + "/" + runtime.GOARCH }
