package node

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
	"kontra.sh/kontra/test/e2e/slowagent"
)

var testTenantID = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000aa")

const (
	tokA = "ket_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tokB = "ket_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

// admitter is a fake tunnelgate.Admitter: it records every request and lets the test decide each answer
// (the n-th request, from 0); the default admits every declared service.
type admitter struct {
	mu   sync.Mutex
	reqs []enroll.Request
	fn   func(n int, req enroll.Request) (*enroll.Decision, error)
	seen chan enroll.Request
}

func newAdmitter() *admitter { return &admitter{seen: make(chan enroll.Request, 64)} }

func (a *admitter) Admit(_ context.Context, req enroll.Request) (*enroll.Decision, error) {
	a.mu.Lock()
	n := len(a.reqs)
	a.reqs = append(a.reqs, req)
	fn := a.fn
	a.mu.Unlock()
	a.seen <- req
	if fn != nil {
		return fn(n, req)
	}
	return admitAll(req), nil
}

func (a *admitter) set(fn func(n int, req enroll.Request) (*enroll.Decision, error)) {
	a.mu.Lock()
	a.fn = fn
	a.mu.Unlock()
}

func (a *admitter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reqs)
}

// next is the next request the gate admitted (or refused at Admit).
func (a *admitter) next(t *testing.T) enroll.Request {
	t.Helper()
	select {
	case r := <-a.seen:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("no hello reached Admit")
		return enroll.Request{}
	}
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

// sink is a fake tunnelgate.SessionSink: it hands the test every session the Handler registers.
type sink struct{ added chan *tunnelgate.Session }

func (s *sink) Add(_ context.Context, sess *tunnelgate.Session, _ *yamux.Session, _ []tunnel.WelcomeService) error {
	s.added <- sess
	return nil
}

// logBuf is a log's output, printed when the test fails.
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

// testGate is an in-process gate: tunnelgate.Handler behind httptest with a fake Admitter and sink. The
// node's connections are wires the test can cut.
type testGate struct {
	srv    *httptest.Server
	adm    *admitter
	sink   *sink
	wires  *tunneltest.Wires
	url    *url.URL
	logs   *logBuf
	active sync.WaitGroup
}

func newTestGate(t *testing.T, mod func(*tunnelgate.HandlerConfig)) *testGate {
	t.Helper()
	g := &testGate{adm: newAdmitter(), sink: &sink{added: make(chan *tunnelgate.Session, 16)}, wires: &tunneltest.Wires{}, logs: &logBuf{}}
	var h http.Handler
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.active.Add(1)
		defer g.active.Done()
		h.ServeHTTP(w, r)
	}))
	g.url, _ = url.Parse(g.srv.URL)
	cfg := tunnelgate.HandlerConfig{
		PublicURL:      g.srv.URL,
		ChannelBinding: tunnel.CBNone,
		ReplicaID:      "r-test",
		Admitter:       g.adm,
		Registry:       g.sink,
		Logger:         slog.New(slog.NewTextHandler(g.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mod != nil {
		mod(&cfg)
	}
	handler, err := tunnelgate.NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h = handler
	t.Cleanup(func() {
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
	return g
}

// session is the next session the gate registered.
func (g *testGate) session(t *testing.T) *tunnelgate.Session {
	t.Helper()
	select {
	case s := <-g.sink.added:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("no session was registered")
		return nil
	}
}

// noSession checks that no session is registered within d.
func (g *testGate) noSession(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case s := <-g.sink.added:
		t.Fatalf("an unexpected session %s was registered", s.ID)
	case <-time.After(d):
	}
}

// testNode is a node under test with its own HOME.
type testNode struct {
	n    *Node
	cfg  Config
	out  *logBuf
	logs *logBuf
	home string
	done chan error
	stop context.CancelFunc
}

func testConfig(t *testing.T, gate *url.URL, home string, serves ...Serve) Config {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	return Config{
		Gate:            gate,
		Serves:          serves,
		KeyPath:         filepath.Join(home, nodekey.DirName, nodekey.FileName),
		Name:            "test-node",
		MaxInflight:     tunnel.NodeMaxInflight,
		CredentialsPath: filepath.Join(home, nodekey.DirName, CredentialsFile),
		Version:         "test",
		Listen:          "127.0.0.1:0", // never 7777, which another node on this host may hold
	}
}

// newTestNode builds the node (New) without running it: its dials ride g's wires, and its retries do not
// wait.
func newTestNode(t *testing.T, g *testGate, cfg Config) (*testNode, error) {
	t.Helper()
	tn := &testNode{cfg: cfg, out: &logBuf{}, logs: &logBuf{}, home: filepath.Dir(filepath.Dir(cfg.KeyPath))}
	cfg.Out = tn.out
	logger := slog.New(slog.NewTextHandler(tn.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	n, err := New(cfg, logger)
	if err != nil {
		return nil, err
	}
	if g != nil {
		n.dialer.HTTPClient = g.wires.Client(nil)
	}
	n.backoff = Backoff{rand: func() float64 { return 0 }}
	tn.n = n
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("node log:\n%s", tn.logs.String())
		}
	})
	return tn, nil
}

func mustNode(t *testing.T, g *testGate, cfg Config) *testNode {
	t.Helper()
	tn, err := newTestNode(t, g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// run runs the node until the test ends (or stop).
func (tn *testNode) run(t *testing.T) *testNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tn.done = make(chan error, 1)
	tn.stop = cancel
	go func() { tn.done <- tn.n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tn.done:
		case <-time.After(20 * time.Second):
			t.Error("Run did not return after its context ended")
		}
	})
	return tn
}

// result is what Run returned, within d.
func (tn *testNode) result(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-tn.done:
		tn.done <- err // for the cleanup
		return err
	case <-time.After(d):
		t.Fatalf("Run did not return within %v", d)
		return nil
	}
}

// welcomed waits until the node has processed a welcome and opened its session (it prints the welcome
// then), so that stopping it is not a lost welcome.
func (tn *testNode) welcomed(t *testing.T) {
	t.Helper()
	eventually(t, 10*time.Second, "the node's session open", func() bool {
		return strings.Contains(tn.out.String(), "kontra connect: connected to")
	})
}

func (tn *testNode) running() bool {
	select {
	case err := <-tn.done:
		tn.done <- err
		return false
	default:
		return true
	}
}

// credView is what the credentials file holds for a node at its gate: the origin's API key and consumed
// tokens, and the waiting tokens of the node's active key.
type credView struct {
	APIKey              string
	Enroll, Unconfirmed []string
	Consumed            []string
}

func (tn *testNode) creds(t *testing.T) credView {
	t.Helper()
	c, err := LoadCredentials(tn.cfg.CredentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := tunnel.CanonicalOrigin(tn.cfg.Gate)
	cr := c.For(o)
	k := cr.Key(tn.n.cfgKeyID(t))
	return credView{APIKey: cr.APIKey, Enroll: k.Enroll, Unconfirmed: k.Unconfirmed, Consumed: cr.Consumed}
}

// mustUpdate changes a credentials file's entry for origin.
func mustUpdate(t *testing.T, path, origin string, change func(*Credential)) {
	t.Helper()
	if err := ensureDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(origin, change); err != nil {
		t.Fatal(err)
	}
}

// sessOpener opens exchange streams on a gate session, as the gate's tunnel backend does.
type sessOpener struct {
	s       *tunnelgate.Session
	service string
	kind    string
}

func (o sessOpener) OpenExchange(ctx context.Context) (net.Conn, error) {
	kind := o.kind
	if kind == "" {
		kind = tunnel.KindA2A
	}
	return o.s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: o.service, Kind: kind})
}

// exchange sends one GET through a gate session and returns the response, the body read.
func exchange(t *testing.T, s *tunnelgate.Session, service, path string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tb, err := upstream.NewTunnel(service, tunnel.KindA2A, sessOpener{s: s, service: service})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.Endpoint(tb, upstream.Info{Path: path}), nil)
	resp, err := tb.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s through the tunnel: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startAgent(t *testing.T, cfg slowagent.Config) *slowagent.Agent {
	t.Helper()
	a := slowagent.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); a.Close() })
	if _, err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return a
}

func agentServe(t *testing.T, name string, a *slowagent.Agent) Serve {
	t.Helper()
	s, err := ParseServe("a2a:" + name + "=" + a.URL())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func handshakeCode(err error) tunnel.Code {
	var herr *tunnel.HandshakeError
	if errors.As(err, &herr) {
		return herr.Code
	}
	return ""
}

func serviceNames(h *tunnel.Hello) []string {
	var out []string
	for _, s := range h.Services {
		out = append(out, s.Name)
	}
	return out
}

// Against an in-process gate: the welcome prints the service URLs, and a gate-opened stream reaches an
// a2a-go agent at the --serve target, whose card and JSON-RPC answer come back through the tunnel.
func TestRunServesAnAgent(t *testing.T) {
	g := newTestGate(t, nil)
	agent := startAgent(t, slowagent.Config{Chunks: 2, Interval: 10 * time.Millisecond})
	tn := mustNode(t, g, testConfig(t, g.url, "", agentServe(t, "hello", agent))).run(t)
	s := g.session(t)
	req := g.adm.next(t)
	if req.Hello.Name != "test-node" || req.Hello.Agent != UserAgent("test") || !slices.Equal(serviceNames(req.Hello), []string{"hello"}) {
		t.Fatalf("hello %+v", req.Hello)
	}
	eventually(t, 5*time.Second, "the welcome's service URLs printed", func() bool {
		return strings.Contains(tn.out.String(), "a2a hello  https://gate.example/agents/hello")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tb, err := upstream.NewTunnel("hello", tunnel.KindA2A, sessOpener{s: s, service: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := upstream.FetchCard(ctx, tb)
	if err != nil {
		t.Fatalf("the card through the tunnel: %v", err)
	}
	info, err := upstream.ReadInfo(f.Raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := upstream.NewClient(ctx, tb, info, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SendMessage(upstream.WithTask(ctx, upstream.ExchangeTask{TaskID: "t1", ContextID: "c1"}),
		&a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
	if task, ok := res.(*a2a.Task); err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage through the tunnel: %T %+v %v", res, res, err)
	}
	if agent.Sends() != 1 {
		t.Fatalf("the agent received %d sends", agent.Sends())
	}
	for _, r := range agent.Requests() {
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("Origin") != "" {
			t.Errorf("a forwarding header reached the agent: %v", r.Header)
		}
	}
}

// {"t": "drain"} makes the node dial a second session at once, while the first goes on serving its
// in-flight SSE to the end (make-before-break, §13, D35); the drained session is not redialed when the gate
// closes it.
func TestRunDrainIsMakeBeforeBreak(t *testing.T) {
	g := newTestGate(t, nil)
	agent := startAgent(t, slowagent.Config{Chunks: 6, Interval: 300 * time.Millisecond, Streaming: true})
	tn := mustNode(t, g, testConfig(t, g.url, "", agentServe(t, "hello", agent)))
	// A redial that waited would take a second at least: the drain's must not wait.
	tn.n.backoff = Backoff{rand: func() float64 { return 0.999 }}
	tn.n.backoff.attempt = 5
	tn.run(t)
	s1 := g.session(t)
	g.adm.next(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, _ := upstream.NewTunnel("hello", tunnel.KindA2A, sessOpener{s: s1, service: "hello"})
	f, err := upstream.FetchCard(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := upstream.ReadInfo(f.Raw)
	c, err := upstream.NewClient(ctx, tb, info, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	chunks := 0
	var final a2a.TaskState
	drained := false
	for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("go"))}) {
		if err != nil {
			t.Fatalf("the SSE through the drained session broke after %d events: %v", events, err)
		}
		events++
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			chunks++
		case *a2a.TaskStatusUpdateEvent:
			final = e.Status.State
		case *a2a.Task:
			final = e.Status.State
		}
		if !drained {
			drained = true
			start := time.Now()
			if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err != nil {
				t.Fatal(err)
			}
			s2 := g.session(t)
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the new session took %v after the drain", took)
			}
			if s2.ID == s1.ID || s1.Mux().IsClosed() {
				t.Fatalf("make-before-break: s1 %s closed=%v, s2 %s", s1.ID, s1.Mux().IsClosed(), s2.ID)
			}
			defer func() {
				if s2.Mux().IsClosed() {
					t.Error("the new session closed")
				}
			}()
		}
	}
	if chunks != 6 || final != a2a.TaskStateCompleted {
		t.Fatalf("after the drain the SSE carried %d chunks and ended %s", chunks, final)
	}
	// The gate closes the drained session at the end of its drain: the node does not redial it.
	_ = s1.Mux().Close()
	g.noSession(t, 1500*time.Millisecond)
	if n := g.adm.count(); n != 2 {
		t.Fatalf("%d handshakes, want 2", n)
	}
	if !tn.running() {
		t.Fatal("Run ended")
	}
}

// {"t": "revoked"} ends Run with ErrRevoked and no redial (§12, §13).
func TestRunRevokedStops(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()))).run(t)
	s := g.session(t)
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlRevoked}); err != nil {
		t.Fatal(err)
	}
	if err := tn.result(t, 10*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run = %v, want ErrRevoked", err)
	}
	g.noSession(t, time.Second)
	if n := g.adm.count(); n != 1 {
		t.Fatalf("%d handshakes after the revocation", n)
	}
	eventually(t, 5*time.Second, "the session closed", s.Mux().IsClosed)
}

// {"t": "revoked", "service"} drops the service: its next preamble is refused 404 not_hosted while the
// session's other service goes on, and the next hello no longer declares it (§12).
func TestRunRevokedServiceIsDropped(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()), a2aServe("other", rec.url()))).run(t)
	s := g.session(t)
	if r := g.adm.next(t); !slices.Equal(serviceNames(r.Hello), []string{"hello", "other"}) {
		t.Fatalf("first hello declares %v", serviceNames(r.Hello))
	}
	if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
		t.Fatalf("before the revocation: %d", resp.StatusCode)
	}
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlRevoked, Service: "hello"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "hello refused not_hosted", func() bool {
		resp, _ := exchange(t, s, "hello", "/x")
		return resp.StatusCode == http.StatusNotFound && resp.Header.Get(tunnel.HeaderNode) == string(tunnel.NodeNotHosted)
	})
	if resp, _ := exchange(t, s, "other", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the other service: %d", resp.StatusCode)
	}
	if !tn.running() {
		t.Fatal("Run ended on a binding revocation")
	}
	// A lost session: the redial declares only the other service.
	for _, w := range g.wires.All() {
		_ = w.Cut()
	}
	if r := g.adm.next(t); !slices.Equal(serviceNames(r.Hello), []string{"other"}) {
		t.Fatalf("the next hello declares %v", serviceNames(r.Hello))
	}
	g.session(t)
}

// A handshake refused with a code the node does not retry (bad_origin) ends Run with that code, after one
// dial; the CLI then exits 1 (cmd/kontra's test).
func TestRunNonRetryableCode(t *testing.T) {
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.PublicURL = "https://gate.example" })
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()))).run(t)
	err := tn.result(t, 10*time.Second)
	if handshakeCode(err) != tunnel.CodeBadOrigin {
		t.Fatalf("Run = %v, want bad_origin", err)
	}
	if n := len(g.wires.All()); n != 1 {
		t.Fatalf("%d dials, want 1", n)
	}

	// Retryable codes are retried with backoff (draining at the hello).
	draining := true
	var mu sync.Mutex
	g2 := newTestGate(t, func(c *tunnelgate.HandlerConfig) {
		c.Draining = func() bool { mu.Lock(); defer mu.Unlock(); return draining }
	})
	mustNode(t, g2, testConfig(t, g2.url, "", a2aServe("hello", rec.url()))).run(t)
	eventually(t, 10*time.Second, "three dials to a draining gate", func() bool { return len(g2.wires.All()) >= 3 })
	mu.Lock()
	draining = false
	mu.Unlock()
	g2.session(t)
}

// --require-channel-binding refuses a gate that offers cb none, and does not retry (§4.1).
func TestRunRequireChannelBinding(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	cfg.RequireChannelBinding = true
	tn := mustNode(t, g, cfg).run(t)
	if err := tn.result(t, 10*time.Second); !errors.Is(err, ErrChannelBindingRequired) {
		t.Fatalf("Run = %v", err)
	}
	if n := g.adm.count(); n != 0 {
		t.Fatalf("the gate admitted %d hellos", n)
	}
}

// First run: ~/.kontra (0700) holds the new identity.key and the credentials file, both 0600. A key file
// readable by others stops kontra connect, as does such a credentials file (§2.1, §2.4).
func TestNewFiles(t *testing.T) {
	home := t.TempDir()
	rec := newRecorder(t)
	gate, _ := url.Parse("https://gate.example")
	cfg := testConfig(t, gate, home, a2aServe("hello", rec.url()))
	cfg.Enroll = []string{tokA}
	tn := mustNode(t, nil, cfg)
	for path, want := range map[string]fs.FileMode{
		filepath.Join(home, ".kontra"):                 0o700 | fs.ModeDir,
		filepath.Join(home, ".kontra", "identity.key"): 0o600,
		filepath.Join(home, ".kontra", "credentials"):  0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != want {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode(), want)
		}
	}
	id, err := nodekey.Load(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if tn.n.activeKey != id.Recipient() {
		t.Error("the node's key is not the file's")
	}
	if c := tn.creds(t); !slices.Equal(c.Enroll, []string{tokA}) {
		t.Errorf("credentials for the gate: %+v", c)
	}
	// A second start loads the same key.
	tn2 := mustNode(t, nil, cfg)
	if tn2.n.activeKey != tn.n.activeKey {
		t.Error("a second start made a new key")
	}

	if err := os.Chmod(cfg.KeyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestNode(t, nil, cfg); err == nil || !strings.Contains(err.Error(), "mode 0644") {
		t.Fatalf("a 0644 key file: %v", err)
	}
	os.Chmod(cfg.KeyPath, 0o600)
	if err := os.Chmod(cfg.CredentialsPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestNode(t, nil, cfg); err == nil || !strings.Contains(err.Error(), "mode 0640") {
		t.Fatalf("a 0640 credentials file: %v", err)
	}
}

// A credential kept for gate A is never sent to gate B: the hello to B carries only B's (§2.4), and B's
// flags are kept under B's origin, A's entry untouched.
func TestCredentialsStayWithTheirOrigin(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	home := t.TempDir()
	credPath := filepath.Join(home, ".kontra", CredentialsFile)
	const aKey = "kk_live_gate_a_key"
	cfg := testConfig(t, g.url, home, a2aServe("hello", rec.url()))
	id, _, err := nodekey.LoadOrCreate(cfg.KeyPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, credPath, "https://gate-a.example", func(c *Credential) {
		c.APIKey = aKey
		c.key(id.KeyID()).Enroll = []string{tokA}
	})

	tn := mustNode(t, g, cfg).run(t)
	r := g.adm.next(t)
	if len(r.Hello.Enroll) != 0 || r.Hello.Auth != "" {
		t.Fatalf("gate B received A's credentials: enroll %v auth %q", r.Hello.Enroll, r.Hello.Auth)
	}
	g.session(t)
	tn.welcomed(t)
	tn.stop()
	tn.result(t, 10*time.Second)

	cfg.APIKey = "kk_live_gate_b_key"
	cfg.Enroll = []string{tokB}
	mustNode(t, g, cfg).run(t)
	r = g.adm.next(t)
	if !slices.Equal(r.Hello.Enroll, []string{tokB}) || r.Hello.Auth != "kk_live_gate_b_key" {
		t.Fatalf("gate B's hello: enroll %v auth %q", r.Hello.Enroll, r.Hello.Auth)
	}
	g.session(t)
	origin, _ := tunnel.CanonicalOrigin(g.url)
	var all *Credentials
	eventually(t, 5*time.Second, "B's token recorded consumed", func() bool {
		all, err = LoadCredentials(credPath)
		if err != nil {
			t.Fatal(err)
		}
		got := all.For(origin)
		return got.APIKey == "kk_live_gate_b_key" && len(got.Key(id.KeyID()).Enroll) == 0 && slices.Equal(got.Consumed, []string{TokenHash(tokB)})
	})
	if got := all.For("https://gate-a.example"); got.APIKey != aKey || !slices.Equal(got.Key(id.KeyID()).Enroll, []string{tokA}) {
		t.Errorf("A's entry changed: %+v", got)
	}
	b, _ := os.ReadFile(credPath)
	if strings.Contains(string(b), tokB) {
		t.Error("a confirmed token is still in the credentials file")
	}
}

// Two `kontra connect` with different keys under one HOME, at one gate (§11: several nodes serve one
// service, each with its own key): a token given to A is never sent in B's hello, B's lost-welcome probe
// confirms only B's own tokens, and neither erases the other's records (M2 WP-06 review: the waiting
// tokens were the origin's, so B sent A's token, and B's probe recorded it consumed).
func TestCredentialsSharedAcrossKeys(t *testing.T) {
	g := newTestGate(t, nil)
	g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
		if n == 0 {
			return nil, gateFails // B's first hello: its outcome is lost
		}
		return admitAll(req), nil
	})
	rec := newRecorder(t)
	home := t.TempDir()
	cfgA := testConfig(t, g.url, home, a2aServe("alpha", rec.url()))
	cfgA.KeyPath = filepath.Join(home, "a.key")
	cfgA.Enroll = []string{tokA}
	tnA := mustNode(t, g, cfgA) // A started; its hello is not sent yet

	cfgB := testConfig(t, g.url, home, a2aServe("beta", rec.url()))
	cfgB.KeyPath = filepath.Join(home, "b.key")
	cfgB.Enroll = []string{tokB}
	tnB := mustNode(t, g, cfgB).run(t)
	if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokB}) {
		t.Fatalf("B's hello carried %v, want only B's token", r.Hello.Enroll)
	}
	if r := g.adm.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("B's probe carried %v", r.Hello.Enroll)
	}
	g.session(t)
	eventually(t, 5*time.Second, "B's token consumed", func() bool {
		return slices.Equal(tnB.creds(t).Consumed, []string{TokenHash(tokB)})
	})
	if c := tnA.creds(t); !slices.Equal(c.Enroll, []string{tokA}) || slices.Contains(c.Consumed, TokenHash(tokA)) {
		t.Fatalf("A's records after B's: %+v", c)
	}

	tnA.run(t)
	if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokA}) || r.Hello.KID != tnA.n.cfgKeyID(t) {
		t.Fatalf("A's hello carried %v", r.Hello.Enroll)
	}
	g.session(t)
	eventually(t, 5*time.Second, "both tokens consumed", func() bool {
		c := tnA.creds(t)
		return len(c.Enroll) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tokB), TokenHash(tokA)})
	})
}

// A token a welcome confirmed is never sent again, even when a restart passes the same --enroll (M2 plan
// audit, gap 1).
func TestConsumedTokenIsNotSentAgain(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{tokA}
	tn := mustNode(t, g, cfg).run(t)
	if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokA}) {
		t.Fatalf("first hello: %v", r.Hello.Enroll)
	}
	g.session(t)
	eventually(t, 5*time.Second, "the token recorded consumed", func() bool {
		c := tn.creds(t)
		return len(c.Enroll) == 0 && len(c.Unconfirmed) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tokA)})
	})
	tn.stop()
	tn.result(t, 10*time.Second)

	// The same flag again (a compose restart), and a reconnect.
	tn2 := mustNode(t, g, cfg).run(t)
	if r := g.adm.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("a consumed token was sent again: %v", r.Hello.Enroll)
	}
	g.session(t)
	for _, w := range g.wires.All() {
		_ = w.Cut()
	}
	if r := g.adm.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("a consumed token was sent on the redial: %v", r.Hello.Enroll)
	}
	g.session(t)
	_ = tn2
}

// gateFails is an admission the gate failed on its own side (Postgres gone): the Handler closes 1011
// without welcome or error, so the node cannot tell whether it committed (ErrNoOutcome).
var gateFails = errors.New("the admission's database went away")

// A connection lost between proof and welcome (M2 plan audit gap 1, conflict C2): the node probes with
// the active key and no tokens. A welcome means the enrollment committed (the tokens are consumed and
// never sent again); unknown_key means it did not, and the full hello goes again at once.
func TestLostWelcomeProbe(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			g := newTestGate(t, nil)
			rec := newRecorder(t)
			g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
				switch {
				case n == 0:
					return nil, gateFails
				case n == 1 && !committed:
					return nil, tunnel.Refuse(tunnel.CodeUnknownKey, "this key is not enrolled at this gate")
				}
				return admitAll(req), nil
			})
			cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
			cfg.Enroll = []string{tokA}
			tn := mustNode(t, g, cfg).run(t)
			if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokA}) {
				t.Fatalf("first hello: %v", r.Hello.Enroll)
			}
			probe := g.adm.next(t)
			if len(probe.Hello.Enroll) != 0 || probe.Hello.RotateFrom != nil || probe.Hello.KID != tn.n.cfgKeyID(t) {
				t.Fatalf("the probe carried enroll %v rotate_from %v", probe.Hello.Enroll, probe.Hello.RotateFrom)
			}
			if !committed {
				if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokA}) {
					t.Fatalf("after unknown_key the hello must carry the tokens again: %v", r.Hello.Enroll)
				}
			}
			g.session(t)
			eventually(t, 5*time.Second, "the token recorded consumed", func() bool {
				c := tn.creds(t)
				return len(c.Enroll) == 0 && len(c.Unconfirmed) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tokA)})
			})
			tn.stop()
			tn.result(t, 10*time.Second)
			// A restart with the same flag sends nothing again.
			mustNode(t, g, cfg).run(t)
			if r := g.adm.next(t); len(r.Hello.Enroll) != 0 {
				t.Fatalf("restart: the hello carried %v", r.Hello.Enroll)
			}
			g.session(t)
		})
	}
}

// cfgKeyID is the key id of the node's key file.
func (n *Node) cfgKeyID(t *testing.T) string {
	t.Helper()
	id, err := nodekey.Load(n.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return id.KeyID()
}

// A process stopped while a hello with tokens was unanswered: the next start probes first (the outcome is
// kept in the credentials file as Unconfirmed). A known key that the earlier hello would have bound a
// service for gets service_denied to the probe, which is "not committed": the full hello follows.
func TestRestartWhileUnconfirmed(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	origin, _ := tunnel.CanonicalOrigin(g.url)
	id, _, err := nodekey.LoadOrCreate(cfg.KeyPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, cfg.CredentialsPath, origin, func(c *Credential) {
		*c.key(id.KeyID()) = KeyCredential{Enroll: []string{tokA}, Unconfirmed: []string{TokenHash(tokA)}}
	})
	g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
		if len(req.Hello.Enroll) == 0 {
			return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "the key was never enrolled for hello")
		}
		return admitAll(req), nil
	})
	tn := mustNode(t, g, cfg).run(t)
	if r := g.adm.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("the first hello after the restart is not the probe: %v", r.Hello.Enroll)
	}
	if r := g.adm.next(t); !slices.Equal(r.Hello.Enroll, []string{tokA}) {
		t.Fatalf("after the probe's service_denied: %v", r.Hello.Enroll)
	}
	g.session(t)
	eventually(t, 5*time.Second, "consumed", func() bool { return slices.Equal(tn.creds(t).Consumed, []string{TokenHash(tokA)}) })
}

// A token the gate refused (enroll_invalid: unknown, used or expired) ends Run with that code, and is
// removed from the credentials file so that the next start does not send it again.
func TestEnrollInvalidDropsTokens(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
		return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "an enrollment token is unknown, already used or expired")
	})
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{tokA}
	tn := mustNode(t, g, cfg).run(t)
	if err := tn.result(t, 10*time.Second); handshakeCode(err) != tunnel.CodeEnrollInvalid {
		t.Fatalf("Run = %v", err)
	}
	if c := tn.creds(t); len(c.Enroll) != 0 || len(c.Unconfirmed) != 0 || len(c.Consumed) != 0 {
		t.Fatalf("credentials after enroll_invalid: %+v", c)
	}
	if n := g.adm.count(); n != 1 {
		t.Fatalf("%d hellos", n)
	}
}

// --rotate writes a new first line, and the next hello carries rotate_from and proof_old (the Handler
// verifies both). After the welcome the old line is stamped for this gate, kept 24 h, then pruned (§12).
func TestRotate(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	home := t.TempDir()
	cfg := testConfig(t, g.url, home, a2aServe("hello", rec.url()))

	// --rotate on a key just created is refused: nothing is enrolled.
	rot := cfg
	rot.Rotate = true
	if _, err := newTestNode(t, g, rot); err == nil || !strings.Contains(err.Error(), "just created") {
		t.Fatalf("--rotate on a new key: %v", err)
	}
	old, err := nodekey.Load(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	tn := mustNode(t, g, rot)
	tn.n.now = func() time.Time { return t0 }
	id, err := nodekey.Load(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient() == old.Recipient() || len(id.Older) != 1 || id.Older[0].Recipient().String() != old.Recipient() {
		t.Fatalf("after --rotate: active %s, older %d", id.KeyID(), len(id.Older))
	}
	b, _ := os.ReadFile(cfg.KeyPath)
	if first := firstKeyLine(string(b)); first != id.Active.String() {
		t.Fatal("the new key is not the file's first key line")
	}
	// A second --rotate before this gate confirmed the first is refused.
	if _, err := newTestNode(t, g, rot); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("a second --rotate: %v", err)
	}

	tn.run(t)
	r := g.adm.next(t)
	if r.Hello.KID != id.KeyID() || r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != old.KeyID() ||
		r.Hello.RotateFrom.Recipient != old.Recipient() || r.OldPubKey == nil {
		t.Fatalf("the hello: kid %s rotate_from %+v old key %v", r.Hello.KID, r.Hello.RotateFrom, r.OldPubKey != nil)
	}
	g.session(t)
	origin, _ := tunnel.CanonicalOrigin(g.url)
	eventually(t, 5*time.Second, "the rotation stamped for this gate", func() bool {
		b, _ := os.ReadFile(cfg.KeyPath)
		return strings.Contains(string(b), nodekey.CommentRotated+"2026-10-26T10:00:00Z "+origin)
	})
	if id, _ := nodekey.Load(cfg.KeyPath); func() bool { _, ok := id.PendingRotation(origin); return ok }() {
		t.Fatal("the rotation is still pending after the welcome")
	}

	// 23 h later the old line is kept; 24 h after the welcome it is pruned (the node's hourly maintenance,
	// driven here with its clock moved, once Run has returned).
	tn.stop()
	if err := tn.result(t, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	tn.n.now = func() time.Time { return t0.Add(23 * time.Hour) }
	tn.n.lastPrune = time.Time{}
	tn.n.maintain(nil)
	if id, _ := nodekey.Load(cfg.KeyPath); len(id.Older) != 1 {
		t.Fatalf("pruned before 24 h: %d older keys", len(id.Older))
	}
	tn.n.now = func() time.Time { return t0.Add(24*time.Hour + time.Minute) }
	tn.n.lastPrune = time.Time{}
	tn.n.maintain(nil)
	if id, _ := nodekey.Load(cfg.KeyPath); len(id.Older) != 0 || id.Recipient() != r.Hello.Recipient {
		t.Fatalf("after 24 h: %d older keys", len(id.Older))
	}
}

// `kontra connect --rotate --enroll <token>`: the token, kept under the key the node started with, is
// sent with the new key, alongside rotate_from (the tokens are the node's, by its key file, not a
// replaced key's).
func TestRotateWithEnroll(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	mustNode(t, nil, cfg) // creates the key
	old, err := nodekey.Load(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	rot := cfg
	rot.Rotate = true
	rot.Enroll = []string{tokA}
	tn := mustNode(t, g, rot).run(t)
	r := g.adm.next(t)
	if r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != old.KeyID() || r.Hello.KID == old.KeyID() ||
		!slices.Equal(r.Hello.Enroll, []string{tokA}) {
		t.Fatalf("the hello: kid %s rotate_from %+v enroll %v", r.Hello.KID, r.Hello.RotateFrom, r.Hello.Enroll)
	}
	g.session(t)
	eventually(t, 5*time.Second, "the token consumed", func() bool {
		c := tn.creds(t)
		return len(c.Enroll) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tokA)})
	})
	all, _ := LoadCredentials(cfg.CredentialsPath)
	origin, _ := tunnel.CanonicalOrigin(g.url)
	if got := all.For(origin); len(got.Keys) != 0 {
		t.Fatalf("waiting tokens left: %+v", got.Keys)
	}
}

func firstKeyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "AGE-SECRET-KEY-1") {
			return l
		}
	}
	return ""
}

// A rotation whose welcome was lost (C2): the node probes with the new key alone; a welcome confirms the
// rotation (MarkRotated) without sending rotate_from again, which would be enroll_invalid.
func TestRotateLostWelcome(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	mustNode(t, nil, cfg) // creates the key
	rot := cfg
	rot.Rotate = true
	tn := mustNode(t, g, rot)
	g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
		if req.Hello.RotateFrom != nil {
			if n > 0 {
				return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "rotate_from: the key was already rotated")
			}
			return nil, gateFails // committed, and the welcome is lost
		}
		return admitAll(req), nil
	})
	tn.run(t)
	if r := g.adm.next(t); r.Hello.RotateFrom == nil {
		t.Fatal("the first hello has no rotate_from")
	}
	if r := g.adm.next(t); r.Hello.RotateFrom != nil || r.Hello.KID != tn.n.cfgKeyID(t) {
		t.Fatalf("the probe: kid %s rotate_from %+v", r.Hello.KID, r.Hello.RotateFrom)
	}
	g.session(t)
	origin, _ := tunnel.CanonicalOrigin(g.url)
	eventually(t, 5*time.Second, "the rotation confirmed", func() bool {
		id, err := nodekey.Load(cfg.KeyPath)
		if err != nil {
			return false
		}
		_, pending := id.PendingRotation(origin)
		return !pending
	})
}

// Another `kontra connect` (another gate) rotates the shared key file: the running node dials a session of
// the new key with rotate_from at once, while the old key's session keeps serving; when the gate ends the
// old key's overlap ({"t": "revoked"} on that session), only that session closes.
func TestRotationByAnotherProcess(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	tn := mustNode(t, g, cfg)
	tn.n.maintainEvery = 50 * time.Millisecond
	tn.run(t)
	old := g.adm.next(t).Hello
	s1 := g.session(t)

	id, err := nodekey.Rotate(cfg.KeyPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := g.adm.next(t)
	if r.Hello.KID != id.KeyID() || r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != old.KID {
		t.Fatalf("after the rotation: kid %s rotate_from %+v", r.Hello.KID, r.Hello.RotateFrom)
	}
	s2 := g.session(t)
	if s1.Mux().IsClosed() {
		t.Fatal("the old key's session closed before the gate closed it")
	}
	if resp, _ := exchange(t, s1, "hello", "/old"); resp.StatusCode != 200 {
		t.Fatalf("the old session no longer serves: %d", resp.StatusCode)
	}
	if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlRevoked}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the old key's session closed", s1.Mux().IsClosed)
	time.Sleep(200 * time.Millisecond)
	if !tn.running() || s2.Mux().IsClosed() {
		t.Fatal("the revocation of the rotated key stopped the node")
	}
	if resp, _ := exchange(t, s2, "hello", "/new"); resp.StatusCode != 200 {
		t.Fatalf("the new session: %d", resp.StatusCode)
	}
	if n := g.adm.count(); n != 2 {
		t.Fatalf("%d handshakes", n)
	}
}

// A binding revocation is per key (§12, D129: the rotation copied the binding to the new key). The gate
// revokes the rotated (old) key's binding to hello during its overlap, on the old key's session: that session
// stops serving hello, while the new key's session goes on serving it and the next hello still declares it
// (M2 review, tunnel-spec-1: the node dropped hello everywhere, and the new key's session answered
// not_hosted).
func TestRotatedKeyBindingRevocationKeepsTheNewKeysService(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	tn := mustNode(t, g, cfg)
	tn.n.maintainEvery = 50 * time.Millisecond
	tn.run(t)
	g.adm.next(t)
	s1 := g.session(t)
	if _, err := nodekey.Rotate(cfg.KeyPath, time.Now()); err != nil { // another kontra connect rotated the key file
		t.Fatal(err)
	}
	if r := g.adm.next(t); r.Hello.RotateFrom == nil {
		t.Fatal("the new key's hello carries no rotate_from")
	}
	s2 := g.session(t)
	if resp, _ := exchange(t, s2, "hello", "/new"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the new key's session before the revocation: %d", resp.StatusCode)
	}
	if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlRevoked, Service: "hello"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the old key's session refusing hello not_hosted", func() bool {
		resp, _ := exchange(t, s1, "hello", "/old")
		return resp.StatusCode == http.StatusNotFound && resp.Header.Get(tunnel.HeaderNode) == string(tunnel.NodeNotHosted)
	})
	if resp, _ := exchange(t, s2, "hello", "/new"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the new key's session after the old key's binding revocation: %d %s=%q", resp.StatusCode,
			tunnel.HeaderNode, resp.Header.Get(tunnel.HeaderNode))
	}
	if !tn.running() {
		t.Fatal("Run ended")
	}
	// Both sessions lost: the redial (the new key) still declares hello.
	for _, w := range g.wires.All() {
		_ = w.Cut()
	}
	if r := g.adm.next(t); !slices.Equal(serviceNames(r.Hello), []string{"hello"}) {
		t.Fatalf("the next hello declares %v, want [hello]", serviceNames(r.Hello))
	}
	g.session(t)
}

// Another `kontra connect` rotates the shared key file, and the gate revokes the old key before this process's
// maintain tick has seen the rotation (rotate after a leak, then revoke the old key: D125, D128). The node
// does not stop: it closes the old key's session and dials the new key's at once (M2 review, tunnel-spec-2:
// it compared with the key of its last dial and ended Run with ErrRevoked).
func TestRevokedRotatedKeyBeforeTheMaintainTick(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	tn := mustNode(t, g, cfg)
	tn.n.maintainEvery = time.Hour // the tick does not come during the test
	tn.n.backoff = Backoff{rand: func() float64 { return 0.999 }}
	tn.n.backoff.attempt = 6 // a redial that waited would take up to a minute
	tn.run(t)
	old := g.adm.next(t).Hello
	s1 := g.session(t)
	id, err := nodekey.Rotate(cfg.KeyPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlRevoked}); err != nil { // the operator revokes the old key
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the old key's session closed", s1.Mux().IsClosed)
	start := time.Now()
	r := g.adm.next(t)
	if r.Hello.KID != id.KeyID() || r.Hello.KID == old.KID {
		t.Fatalf("the redial's key %s, want the new key %s", r.Hello.KID, id.KeyID())
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the new key's session was dialed %v after the old key's revocation, want at once", took)
	}
	g.session(t)
	time.Sleep(200 * time.Millisecond)
	if !tn.running() {
		t.Fatalf("Run ended after the rotated key's revocation: %v", tn.result(t, time.Second))
	}
}

// A session that lived BackoffReset or more resets the reconnect backoff when it is retired (a drain), as
// when it closes (§13: "reset after a session that lived 60 s"): a redial refused after the drain of a
// two-hour session waits under BackoffMin, not on the ceiling left from failures before that session (M2
// review, tunnel-spec-3).
func TestRetiredSessionResetsTheBackoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		aged bool
	}{
		{"a session of two hours", true},
		{"a session of seconds", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGate(t, nil)
			rec := newRecorder(t)
			tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url())))
			tn.n.backoff = Backoff{rand: func() float64 { return 0.999 }}
			tn.n.backoff.attempt = 6 // dials failed for a while before this session (the gate was down)
			tn.run(t)
			s1 := g.session(t)
			g.adm.next(t)
			tn.welcomed(t) // the node holds its session
			if tc.aged {
				tn.n.mu.Lock()
				aged := 0
				for s := range tn.n.sessions {
					s.opened = time.Now().Add(-2 * time.Hour)
					aged++
				}
				tn.n.mu.Unlock()
				if aged != 1 {
					t.Fatalf("the node holds %d sessions, want 1", aged)
				}
			}
			g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
				return nil, tunnel.Refuse(tunnel.CodeDraining, "draining") // the redial reached the draining replica
			})
			if err := s1.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err != nil {
				t.Fatal(err)
			}
			var wait time.Duration
			eventually(t, 10*time.Second, "a redial refused and waited on", func() bool {
				w, ok := firstWait(tn.logs.String())
				wait = w
				return ok
			})
			if tc.aged && wait >= BackoffMin {
				t.Fatalf("after the drain of a two-hour session the refused redial waits %v, want under %v", wait, BackoffMin)
			}
			if !tc.aged && wait < 30*time.Second {
				t.Fatalf("after the drain of a session of seconds the refused redial waits %v: the backoff was reset", wait)
			}
		})
	}
}

// firstWait reads the wait of the first "redialing the gate after a wait" line of a node's text log.
func firstWait(log string) (time.Duration, bool) {
	for _, l := range strings.Split(log, "\n") {
		if !strings.Contains(l, "redialing the gate after a wait") {
			continue
		}
		i := strings.Index(l, " wait=")
		if i < 0 {
			return 0, false
		}
		v := l[i+len(" wait="):]
		if j := strings.IndexByte(v, ' '); j >= 0 {
			v = v[:j]
		}
		d, err := time.ParseDuration(v)
		return d, err == nil
	}
	return 0, false
}

func TestParseArgs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := t.TempDir()
	defaultEnv := env(map[string]string{"HOME": home})
	hostname = func() (string, error) { return "build-box-3", nil }
	t.Cleanup(func() { hostname = os.Hostname })

	cfg, err := parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999"}, defaultEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate.String() != "https://gateway.kontra.sh" || cfg.MaxInflight != 64 || cfg.Name != "build-box-3" ||
		cfg.KeyPath != filepath.Join(home, ".kontra", "identity.key") || cfg.CredentialsPath != filepath.Join(home, ".kontra", "credentials") ||
		cfg.LogJSON || cfg.Rotate || cfg.RequireChannelBinding || len(cfg.Enroll) != 0 || cfg.APIKey != "" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if len(cfg.Serves) != 1 || cfg.Serves[0].Kind != "a2a" || cfg.Serves[0].Name != "hello" || cfg.Serves[0].Target.String() != "http://127.0.0.1:9999/" {
		t.Fatalf("serves %+v", cfg.Serves)
	}

	if cfg.Listen != "127.0.0.1:7777" || cfg.LocalToken != "" || len(cfg.AllowOrigins) != 0 {
		t.Fatalf("hiring defaults: %q %q %v", cfg.Listen, cfg.LocalToken, cfg.AllowOrigins)
	}

	// A node that only hires: no --serve (spec/tunnel.md §4, §9); the hiring flags.
	tokenFile := filepath.Join(t.TempDir(), "local-token")
	os.WriteFile(tokenFile, []byte("0123456789abcdef-local\n"), 0o600)
	cfg, err = parseArgs([]string{"--listen", "0.0.0.0:7700", "--local-token", tokenFile, "--allow-origin", "http://app.local:3000",
		"--allow-origin", "https://other.example"}, defaultEnv, nil)
	if err != nil || len(cfg.Serves) != 0 || cfg.Listen != "0.0.0.0:7700" || cfg.LocalToken != "0123456789abcdef-local" ||
		!slices.Equal(cfg.AllowOrigins, []string{"http://app.local:3000", "https://other.example"}) {
		t.Fatalf("hire-only with the hiring flags: %+v %v", cfg, err)
	}
	if cfg, err := parseArgs(nil, defaultEnv, nil); err != nil || len(cfg.Serves) != 0 {
		t.Fatalf("no flags at all (hire only): %v", err)
	}

	// KONTRA_KEY, then --key over it.
	cfg, _ = parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999"}, env(map[string]string{"HOME": home, "KONTRA_KEY": "/run/secrets/key"}), nil)
	if cfg.KeyPath != "/run/secrets/key" {
		t.Errorf("KONTRA_KEY: %s", cfg.KeyPath)
	}
	cfg, _ = parseArgs([]string{"--key", "/k", "--serve", "a2a:hello=http://127.0.0.1:9999"}, env(map[string]string{"HOME": home, "KONTRA_KEY": "/run/secrets/key"}), nil)
	if cfg.KeyPath != "/k" {
		t.Errorf("--key: %s", cfg.KeyPath)
	}

	// Secrets: a value, @file and - (stdin).
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(tokB+"\n"), 0o600)
	cfg, err = parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", tokA, "--enroll", "@" + file,
		"--api-key", "-", "--gate", "http://localhost:8080", "--name", "n1", "--rotate", "--require-channel-binding",
		"--max-inflight", "8", "--log", "json"}, defaultEnv, strings.NewReader("kk_live_from_stdin\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Enroll, []string{tokA, tokB}) || cfg.APIKey != "kk_live_from_stdin" || cfg.Gate.String() != "http://localhost:8080" ||
		cfg.Name != "n1" || !cfg.Rotate || !cfg.RequireChannelBinding || cfg.MaxInflight != 8 || !cfg.LogJSON {
		t.Fatalf("flags: %+v", cfg)
	}
	cfg, err = parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", "-"}, defaultEnv, strings.NewReader(tokA))
	if err != nil || !slices.Equal(cfg.Enroll, []string{tokA}) {
		t.Fatalf("--enroll -: %v %v", cfg.Enroll, err)
	}

	// A long hostname is cut to 64 characters (its first label when that fits).
	hostname = func() (string, error) { return strings.Repeat("a", 70) + ".example.com", nil }
	if cfg, _ := parseArgs([]string{"--serve", "a2a:h=http://127.0.0.1:1"}, defaultEnv, nil); cfg.Name != strings.Repeat("a", 64) {
		t.Errorf("long hostname: %q", cfg.Name)
	}
	hostname = func() (string, error) { return strings.Repeat("b", 30) + "." + strings.Repeat("c", 40), nil }
	if cfg, _ := parseArgs([]string{"--serve", "a2a:h=http://127.0.0.1:1"}, defaultEnv, nil); cfg.Name != strings.Repeat("b", 30) {
		t.Errorf("long FQDN: %q", cfg.Name)
	}
	hostname = func() (string, error) { return "build-box-3", nil }

	serve := "a2a:hello=http://127.0.0.1:9999"
	for _, x := range []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"--listen", "0.0.0.0:7777"}, "", "--local-token"},
		{[]string{"--listen", "10.1.2.3:7777"}, "", "--local-token"},
		{[]string{"--listen", "7777"}, "", "host:port"},
		{[]string{"--local-token", "/nonexistent/token"}, "", "--local-token"},
		{[]string{"--allow-origin", "http://app.local/path"}, "", "--allow-origin"},
		{[]string{"--serve", "mcp:fs=stdio:"}, "", "empty"},
		{[]string{"--serve", "mcp:fs=stdio:npx 'x"}, "", "unterminated"},
		{[]string{"--serve", "mcp:fs=http://h:1/a/../mcp"}, "", "not clean"},
		{[]string{"--serve", "mcp:fs=ftp://h:1/mcp"}, "", "scheme"},
		{[]string{"--serve", serve, "--roots", "hello=" + home}, "", "no --serve mcp:hello"},
		{[]string{"--serve", "mcp:fs=http://h:1/mcp", "--roots", "other=" + home}, "", "no --serve mcp:other"},
		{[]string{"--serve", "mcp:fs=http://h:1/mcp", "--roots", "fs=" + home + "/missing"}, "", "no such file"},
		{[]string{"--serve", "mcp:fs=http://h:1/mcp", "--env", "1BAD"}, "", "variable name"},
		{[]string{"--serve", "mcp:fs=http://h:1/mcp", "--max-sessions", "0"}, "", "--max-sessions"},
		{[]string{"--serve", "mcp:fs=http://h:1/mcp", "--session-idle", "-1m"}, "", "--session-idle"},
		{[]string{"--serve", "a2a:x=http://h:1/base"}, "", "base path"},
		{[]string{"--serve", "grpc:x=http://h:1"}, "", "kind"},
		{[]string{"--serve", "a2a:Hello=http://h:1"}, "", "name"},
		{[]string{"--serve", "a2a:hello"}, "", "name=url"},
		{[]string{"--serve", "a2a:hello=ftp://h"}, "", "scheme"},
		{[]string{"--serve", "a2a:hello=http://u:p@h:1"}, "", "userinfo"},
		{[]string{"--serve", "a2a:hello=http://h:1/?x=1"}, "", "query"},
		{[]string{"--serve", "a2a:hello=http://h:99999"}, "", "port"},
		{[]string{"--serve", serve, "--serve", "a2a:hello=http://h:2"}, "", "twice"},
		{[]string{"--serve", serve, "--gate", "http://gate.example"}, "", "http:// only for localhost"},
		{[]string{"--serve", serve, "--gate", "https://user:secret@gate.example"}, "", "no credentials"},
		{[]string{"--serve", serve, "--gate", "ftp://gate.example"}, "", "scheme"},
		{[]string{"--serve", serve, "--enroll", "not-a-token"}, "", "not an enrollment token"},
		{[]string{"--serve", serve, "--enroll", "@/nonexistent/token"}, "", "--enroll"},
		{[]string{"--serve", serve, "--enroll", "-"}, "", "empty"},
		{[]string{"--serve", serve, "--enroll", "-", "--api-key", "-"}, tokA, "already read"},
		{[]string{"--serve", serve, "--api-key", "two words"}, "", "white space"},
		{[]string{"--serve", serve, "--api-key", "a", "--api-key", "b"}, "", "twice"},
		{[]string{"--serve", serve, "--name", strings.Repeat("n", 65)}, "", "65 characters"},
		{[]string{"--serve", serve, "--name", "a\x00b"}, "", "control"},
		{[]string{"--serve", serve, "--max-inflight", "0"}, "", "--max-inflight"},
		{[]string{"--serve", serve, "--log", "xml"}, "", "--log"},
		{[]string{"--serve", serve, "--nope"}, "", "not defined"},
		{[]string{"--serve", serve, "extra"}, "", "unexpected argument"},
	} {
		_, err := parseArgs(x.args, defaultEnv, strings.NewReader(x.stdin))
		var uerr *UsageError
		if err == nil || !errors.As(err, &uerr) || !strings.Contains(err.Error(), x.want) {
			t.Errorf("%q: %v, want a usage error with %q", x.args, err, x.want)
		}
		if err != nil && (strings.Contains(err.Error(), "user:secret") || strings.Contains(err.Error(), "u:p") ||
			strings.Contains(err.Error(), "two words")) {
			t.Errorf("%q: the error quotes a credential: %v", x.args, err)
		}
	}
	if _, err := parseArgs([]string{"-h"}, defaultEnv, nil); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

// The node's patterns agree with the gate's: relay.md §2.1's service names (config.NameRE) and §5's
// token shape (enroll.ValidToken).
func TestPatternsMatchTheGate(t *testing.T) {
	if serviceNameRE.String() != config.NameRE.String() {
		t.Errorf("service names: node %s, gate %s", serviceNameRE, config.NameRE)
	}
	for _, tok := range []string{tokA, "ket_short", "ket_" + strings.Repeat("-", 32), "xet_" + strings.Repeat("a", 32), tokA + "a"} {
		if tokenRE.MatchString(tok) != enroll.ValidToken(tok) {
			t.Errorf("%q: node %v, gate %v", tok, tokenRE.MatchString(tok), enroll.ValidToken(tok))
		}
	}
}

func TestReadSecret(t *testing.T) {
	file := filepath.Join(t.TempDir(), "s")
	os.WriteFile(file, []byte("  s3cret\r\n"), 0o600)
	for _, x := range []struct {
		v, stdin, want string
		ok             bool
	}{
		{"value", "", "value", true},
		{"@" + file, "", "s3cret", true},
		{"-", "piped\n", "piped", true},
		{"@", "", "", false},
		{"-", "", "", false},
		{"-", "a\nb\n", "", false},
		{"@" + file + ".missing", "", "", false},
	} {
		got, err := readSecret(x.v, strings.NewReader(x.stdin))
		if (err == nil) != x.ok || got != x.want {
			t.Errorf("readSecret(%q) = %q, %v", x.v, got, err)
		}
		if err != nil && strings.Contains(err.Error(), x.stdin) && x.stdin != "" {
			t.Errorf("readSecret(%q): the error quotes the secret: %v", x.v, err)
		}
	}
	big := strings.Repeat("a", maxSecret+1)
	if _, err := readSecret("-", strings.NewReader(big)); err == nil {
		t.Error("a secret over the limit")
	}
}

func TestCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "credentials")
	c, err := LoadCredentials(path)
	if err != nil || !c.For("https://a.example").empty() {
		t.Fatalf("absent: %v", err)
	}
	if _, err := c.Update("https://a.example", func(c *Credential) { c.APIKey = "k"; c.Consumed = append(c.Consumed, "h") }); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory: %v %v", fi.Mode(), err)
	}
	// Another process's change is kept: Update re-reads the file under its lock.
	other, _ := LoadCredentials(path)
	if _, err := other.Update("https://b.example", func(c *Credential) { c.key("k1_b").Enroll = []string{tokB} }); err != nil {
		t.Fatal(err)
	}
	got, err := c.Update("https://a.example", func(c *Credential) { c.APIKey = "k2" })
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "k2" || !slices.Equal(got.Consumed, []string{"h"}) {
		t.Fatalf("Update returned %+v", got)
	}
	all, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if all.For("https://a.example").APIKey != "k2" || !slices.Equal(all.For("https://b.example").Key("k1_b").Enroll, []string{tokB}) {
		t.Fatalf("after two processes' changes: %+v %+v", all.For("https://a.example"), all.For("https://b.example"))
	}
	// An entry left empty is removed, a key's as the origin's.
	if _, err := other.Update("https://b.example", func(c *Credential) { c.key("k1_b").Enroll = nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update("https://a.example", func(c *Credential) { c.APIKey, c.Consumed = "", nil }); err != nil {
		t.Fatal(err)
	}
	all, _ = LoadCredentials(path)
	if len(all.byOrigin) != 0 {
		t.Errorf("empty credentials kept their entries: %+v", all.byOrigin)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// Concurrent changes of different origins lose nothing.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			cc, err := LoadCredentials(path)
			if err == nil {
				_, err = cc.Update(fmt.Sprintf("https://g%d.example", i), func(c *Credential) { c.APIKey = fmt.Sprint(i) })
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	all, _ = LoadCredentials(path)
	for i := range 20 {
		if all.For(fmt.Sprintf("https://g%d.example", i)).APIKey != fmt.Sprint(i) {
			t.Errorf("origin %d lost", i)
		}
	}
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := LoadCredentials(path); err == nil || strings.Contains(err.Error(), "not json") {
		t.Errorf("a broken file: %v", err)
	}
}

// Two `kontra connect` with different keys under one HOME, at one gate (§11: several nodes serve one
// service, each with its own key), each holding the copy it read at start: a change by one never erases
// the other's (M2 WP-06 review). Before Update, Save wrote the caller's whole copy of the origin's entry, so
// B's save of its stale copy erased A's Consumed record, and A's restart with the same --enroll then sent a
// used token (enroll_invalid, final).
func TestCredentialsStaleCopiesAtOneOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kontra", CredentialsFile)
	const o = "https://gate.example"
	a, _ := LoadCredentials(path)
	b, _ := LoadCredentials(path) // B starts before A's welcome
	if _, err := a.Update(o, func(c *Credential) { c.key("k1_a").Enroll = []string{tokA} }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(o, func(c *Credential) { // A's welcome
		c.Consumed = append(c.Consumed, TokenHash(tokA))
		c.key("k1_a").Enroll = nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(o, func(c *Credential) { c.key("k1_b").Enroll = []string{tokB} }); err != nil { // B, from its stale copy
		t.Fatal(err)
	}
	all, _ := LoadCredentials(path)
	got := all.For(o)
	if !slices.Equal(got.Consumed, []string{TokenHash(tokA)}) || !slices.Equal(got.Key("k1_b").Enroll, []string{tokB}) ||
		len(got.Key("k1_a").Enroll) != 0 {
		t.Fatalf("after A's welcome and B's change: %+v", got)
	}
}

// envCredsHelper makes the test binary a `kontra connect` stand-in for TestCredentialsTwoProcesses.
const envCredsHelper = "KONTRA_TEST_CREDENTIALS_HELPER"

// Two processes change one origin's entry at once, each its own key's tokens and the shared Consumed list,
// through flock(2) on the shared file: nothing is lost.
func TestCredentialsTwoProcesses(t *testing.T) {
	if spec := os.Getenv(envCredsHelper); spec != "" {
		credentialsHelper(t, spec)
		return
	}
	path := filepath.Join(t.TempDir(), ".kontra", CredentialsFile)
	const rounds = 40
	var wg sync.WaitGroup
	for _, kid := range []string{"k1_a", "k1_b"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCredentialsTwoProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%s|%d", envCredsHelper, path, kid, rounds))
		wg.Go(func() {
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("process %s: %v\n%s", kid, err, out)
			}
		})
	}
	wg.Wait()
	all, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	got := all.For("https://gate.example")
	if len(got.Consumed) != 2*rounds {
		t.Errorf("%d consumed hashes, want %d", len(got.Consumed), 2*rounds)
	}
	for _, kid := range []string{"k1_a", "k1_b"} {
		if n := len(got.Key(kid).Enroll); n != rounds {
			t.Errorf("%s: %d tokens, want %d", kid, n, rounds)
		}
	}
}

func credentialsHelper(t *testing.T, spec string) {
	parts := strings.Split(spec, "|")
	if len(parts) != 3 {
		t.Fatalf("helper spec %q", spec)
	}
	path, kid := parts[0], parts[1]
	var rounds int
	fmt.Sscan(parts[2], &rounds)
	c, err := LoadCredentials(path) // loaded once: every later change is made from this stale copy
	if err != nil {
		t.Fatal(err)
	}
	for i := range rounds {
		tok := fmt.Sprintf("ket_%s%028d", strings.TrimPrefix(kid, "k1_")+"xxx", i)
		if _, err := c.Update("https://gate.example", func(c *Credential) {
			k := c.key(kid)
			k.Enroll = append(k.Enroll, tok)
			c.Consumed = append(c.Consumed, TokenHash(kid+tok))
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNearExhaustion(t *testing.T) {
	const limit = 1<<32 - 2 // yamux refuses an open at this id
	for id, want := range map[uint32]bool{2: false, 1 << 31: false, limit - streamIDHeadroom - 2: false, limit - streamIDHeadroom: true, limit - 2: true} {
		if got := nearExhaustion(id); got != want {
			t.Errorf("nearExhaustion(%d) = %v", id, got)
		}
	}
}

// A control stream line of an unknown type is ignored; the session goes on.
func TestControlIgnoresUnknown(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()))).run(t)
	s := g.session(t)
	for _, c := range []tunnel.Control{{T: "future-thing"}, {T: tunnel.CtlAuthRevoked}, {T: tunnel.CtlEnrolled, Service: "hello", Key: "k1_x"}} {
		if err := s.WriteControl(c); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 || !tn.running() {
		t.Fatalf("after the control lines: %d, running %v", resp.StatusCode, tn.running())
	}
	if !strings.Contains(tn.logs.String(), "hiring API key") || !strings.Contains(tn.logs.String(), "another key was enrolled") {
		t.Errorf("auth_revoked and enrolled are not logged:\n%s", tn.logs.String())
	}
}
