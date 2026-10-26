package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// pairOpener is a session's StreamOpener over an in-process yamux pair: each exchange opens a stream and
// writes its preamble (tunnel.md §8.1), the task taken from the request's context.
type pairOpener struct {
	gate    *yamux.Session
	service string
	opens   atomic.Int64
	err     error
}

func (o *pairOpener) OpenExchange(ctx context.Context) (net.Conn, error) {
	o.opens.Add(1)
	if o.err != nil {
		return nil, o.err
	}
	st, err := o.gate.OpenStream()
	if err != nil {
		return nil, err
	}
	p := tunnel.Preamble{Op: tunnel.OpHTTP, Service: o.service, Kind: tunnel.KindA2A}
	if t, ok := TaskFrom(ctx); ok {
		p.Task, p.Context = t.TaskID, t.ContextID
	}
	if err := tunnel.WritePreamble(st, p); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// seen is what the node read on one stream: its preamble and the request's head.
type seen struct {
	stream     uint32
	preamble   tunnel.Preamble
	method     string
	requestURI string
	host       string
	connection string
}

// pairNode is the node's side: it reads every stream's preamble and one HTTP/1.1 request, records them,
// and serves the request with h.
type pairNode struct {
	mu   sync.Mutex
	seen []seen
}

func (n *pairNode) all() []seen {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]seen(nil), n.seen...)
}

func newTunnelPair(t *testing.T, h http.Handler) (*Tunnel, *pairOpener, *pairNode, *yamux.Session) {
	t.Helper()
	nodeMux, gateMux := tunneltest.NewPair(t).Sessions(t, nil)
	n := &pairNode{}
	go func() {
		for {
			st, err := nodeMux.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(st)
				p, err := tunnel.ReadPreamble(br)
				if err != nil {
					_ = st.Close()
					return
				}
				id := st.StreamID()
				serveConn(&readerConn{Conn: st, r: br}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn := r.Header.Get("Connection")
					if r.Close && conn == "" {
						conn = "close"
					}
					n.mu.Lock()
					n.seen = append(n.seen, seen{stream: id, preamble: p, method: r.Method, requestURI: r.RequestURI, host: r.Host, connection: conn})
					n.mu.Unlock()
					h.ServeHTTP(w, r)
				}))
			}()
		}
	}()
	op := &pairOpener{gate: gateMux, service: "hello"}
	tb, err := NewTunnel("hello", tunnel.KindA2A, op)
	if err != nil {
		t.Fatal(err)
	}
	return tb, op, n, gateMux
}

type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// serveConn serves the one HTTP/1.1 exchange on c with h.
func serveConn(c net.Conn, h http.Handler) {
	l := &oneListener{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &closingConn{Conn: c, l: l}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(l)
}

type oneListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *oneListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneListener) Addr() net.Addr { return &net.TCPAddr{} }

type closingConn struct {
	net.Conn
	l *oneListener
}

func (c *closingConn) Close() error { _ = c.l.Close(); return c.Conn.Close() }

func TestNewTunnel(t *testing.T) {
	for name, fn := range map[string]func() (*Tunnel, error){
		"no service": func() (*Tunnel, error) { return NewTunnel("", tunnel.KindA2A, &pairOpener{}) },
		"bad kind":   func() (*Tunnel, error) { return NewTunnel("hello", "grpc", &pairOpener{}) },
		"no opener":  func() (*Tunnel, error) { return NewTunnel("hello", tunnel.KindA2A, nil) },
	} {
		if _, err := fn(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tb, err := NewTunnel("hello", tunnel.KindA2A, &pairOpener{})
	if err != nil {
		t.Fatal(err)
	}
	// A placeholder origin the node ignores (§8.2); .invalid never resolves.
	if tb.Kind() != KindTunnel || tb.Origin().String() != "http://kontra-node.invalid" || tb.Base().String() != "http://kontra-node.invalid/" {
		t.Errorf("kind %s origin %s base %s", tb.Kind(), tb.Origin(), tb.Base())
	}
	if got := Endpoint(tb, Info{Path: "/a2a/rpc"}); got != "http://kontra-node.invalid/a2a/rpc" {
		t.Errorf("endpoint %s", got)
	}
	if _, ok := TaskFrom(context.Background()); ok {
		t.Error("a task without WithTask")
	}
	if got, ok := TaskFrom(WithTask(context.Background(), ExchangeTask{TaskID: "t", ContextID: "c"})); !ok || got != (ExchangeTask{"t", "c"}) {
		t.Errorf("TaskFrom %+v %v", got, ok)
	}
}

// Through an in-process yamux pair (tunnel.md §8.1, §8.2): each exchange is a new stream whose first line
// is the preamble {"v":1,"op":"http","service","kind","task","context"}, then one HTTP/1.1 request in
// origin form with Connection: close; no stream is reused, the a2a-go client works over it unchanged, and
// every body is read through the §6.6 limits.
func TestTunnelExchanges(t *testing.T) {
	agent := &echoAgent{}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(agent.executor()))
	mux := http.NewServeMux()
	mux.Handle("/a2a/", http.StripPrefix("/a2a", rpc))
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain") })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte("a"), MaxBody+1)) })
	mux.HandleFunc("/sse-big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: small\n\n")
		io.WriteString(w, "data: "+strings.Repeat("b", MaxSSEEvent)+"\n\n")
	})
	tb, op, node, _ := newTunnelPair(t, mux)
	ctx := WithTask(context.Background(), ExchangeTask{TaskID: "01920000-0000-7000-8000-000000000001", ContextID: "ctx-1"})

	hc := tb.HTTPClient()
	for range 2 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint(tb, Info{Path: "/plain"}), nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "plain" {
			t.Fatalf("body %q", b)
		}
	}
	c, err := NewClient(ctx, tb, Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/a2a/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
	if task, ok := res.(*a2a.Task); err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage over the tunnel: %T %+v %v", res, res, err)
	}

	got := node.all()
	if len(got) != 3 || op.opens.Load() != 3 {
		t.Fatalf("%d streams seen, %d opened, want 3 (one per exchange)", len(got), op.opens.Load())
	}
	streams := map[uint32]bool{}
	want := tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A, Task: "01920000-0000-7000-8000-000000000001", Context: "ctx-1"}
	for i, s := range got {
		if streams[s.stream] {
			t.Errorf("stream %d reused", s.stream)
		}
		streams[s.stream] = true
		if s.preamble != want {
			t.Errorf("exchange %d: preamble %+v", i, s.preamble)
		}
		if !strings.HasPrefix(s.requestURI, "/") || strings.Contains(s.requestURI, "://") {
			t.Errorf("exchange %d: request target %q is not origin-form", i, s.requestURI)
		}
		if !strings.EqualFold(s.connection, "close") {
			t.Errorf("exchange %d: Connection %q", i, s.connection)
		}
		if s.host != "kontra-node.invalid" {
			t.Errorf("exchange %d: Host %q", i, s.host)
		}
	}
	if got[2].method != http.MethodPost || got[2].requestURI != "/a2a/" {
		t.Errorf("the a2a-go call went as %s %s", got[2].method, got[2].requestURI)
	}

	// The §6.6 limits hold through the tunnel.
	for path, what := range map[string]string{"/big": "a body over 8 MiB", "/sse-big": "an SSE event over 4 MiB"} {
		ectx, ex := WithExchange(ctx)
		req, _ := http.NewRequestWithContext(ectx, http.MethodGet, Endpoint(tb, Info{Path: path}), nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if !errors.Is(err, ErrLimit) || !ex.LimitExceeded() || Classify(ectx, err, ex).Class != Limit {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// Classify over a tunnel (relay.md §6.4's tunnel rows, tunnel.md §8.2): a stream that could not be opened,
// a session that closed and an incomplete chunked body are ClassTunnelLost; a response the node wrote
// itself is told by Kontra-Node: full is ClassNodeFull, target_unreachable and not_hosted ClassTunnelLost,
// sealed_rejected ClassSealedRejected; the service's own 503 (no Kontra-Node) is Unreachable, and its own
// JSON-RPC error a Protocol error. A direct backend never reads Kontra-Node.
func TestClassifyTunnel(t *testing.T) {
	var mode atomic.Value
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := func(status int, reason tunnel.NodeReason) {
			w.Header().Set(tunnel.HeaderNode, string(reason))
			w.WriteHeader(status)
		}
		switch mode.Load().(string) {
		case "full":
			node(http.StatusServiceUnavailable, tunnel.NodeFull)
		case "target_unreachable":
			node(http.StatusBadGateway, tunnel.NodeTargetUnreachable)
		case "not_hosted":
			node(http.StatusNotFound, tunnel.NodeNotHosted)
		case "sealed_rejected":
			node(http.StatusBadRequest, tunnel.NodeSealedRejected)
		case "own503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "rpcerr":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"the seller says no"}}`)
		case "cut":
			// A chunked body the stream ends in the middle of.
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":`)
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		case "hold":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":`)
			w.(http.Flusher).Flush()
			<-hold
		}
	})
	tb, op, _, gateMux := newTunnelPair(t, h)
	send := func(b Backend) (context.Context, error, *Exchange) {
		c, err := NewClient(context.Background(), b, Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, ex := WithExchange(context.Background())
		_, err = c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
		if err == nil {
			t.Fatal("no error")
		}
		return ctx, err, ex
	}
	for _, tc := range []struct {
		mode  string
		class Class
		msg   string
	}{
		{"full", ClassNodeFull, "limit"},
		{"target_unreachable", ClassTunnelLost, "target_unreachable"},
		{"not_hosted", ClassTunnelLost, "not_hosted"},
		{"sealed_rejected", ClassSealedRejected, "sealed"},
		{"own503", Unreachable, "503"},
		{"rpcerr", Protocol, "the seller says no"},
		{"cut", ClassTunnelLost, "lost"},
	} {
		mode.Store(tc.mode)
		ctx, err, ex := send(tb)
		if f := Classify(ctx, err, ex); f.Class != tc.class || !strings.Contains(f.Message, tc.msg) {
			t.Errorf("%s: %+v, want class %d ~%q", tc.mode, f, tc.class, tc.msg)
		}
	}

	// The session closes in the middle of a response.
	mode.Store("hold")
	done := make(chan Failure, 1)
	go func() {
		ctx, err, ex := send(tb)
		done <- Classify(ctx, err, ex)
	}()
	time.Sleep(200 * time.Millisecond)
	_ = gateMux.Close()
	select {
	case f := <-done:
		if f.Class != ClassTunnelLost {
			t.Errorf("a session closed mid-response: %+v", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the exchange did not end when its session closed")
	}

	// A stream that cannot be opened: the session is gone.
	op.err = errors.New("session closed")
	n := op.opens.Load()
	ctx, err, ex := send(tb)
	if f := Classify(ctx, err, ex); f.Class != ClassTunnelLost || !errors.Is(err, ErrTunnelLost) {
		t.Errorf("an open that failed: %+v (%v)", f, err)
	}
	if op.opens.Load() != n+1 {
		t.Errorf("%d opens for one exchange", op.opens.Load()-n)
	}

	// A direct service cannot pass its answer off as a node's: Kontra-Node is not read.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeFull))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, err, ex = send(direct(t, srv.URL))
	if f := Classify(ctx, err, ex); f.Class != Unreachable || ex.Tunnelled() || ex.Node() != "" {
		t.Errorf("a direct 503 with Kontra-Node: %+v", f)
	}
}

// stuckOpener is a session's opener stuck before its stream opens (the key and binding read in Postgres, a
// yamux open behind un-acknowledged ones): each open waits for its context's end or for release. An open
// released while its context is still live opens a stream, which the test counts.
type stuckOpener struct {
	release   chan struct{}
	ended     chan error // ctx.Err() of each open whose context ended first
	deadlines chan time.Time
	opened    atomic.Int64
}

func (o *stuckOpener) OpenExchange(ctx context.Context) (net.Conn, error) {
	d, _ := ctx.Deadline()
	o.deadlines <- d
	select {
	case <-ctx.Done():
		o.ended <- ctx.Err()
		return nil, ctx.Err()
	case <-o.release:
	}
	o.opened.Add(1)
	gate, node := net.Pipe()
	_ = node.Close()
	return gate, nil
}

// A tunnel exchange's open is bounded by its request (M2 WP-05 review): http.Transport detaches its dial
// from the request's cancellation, so without the tunnel's own binding a stuck open would outlive an
// abandoned request and then open a stray stream. The open's context carries the request's deadline and
// ends with the request, by deadline or by cancel, and nothing is opened after it.
func TestTunnelOpenEndsWithItsRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 200*time.Millisecond)
		}, context.DeadlineExceeded},
		{"cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := &stuckOpener{release: make(chan struct{}), ended: make(chan error, 1), deadlines: make(chan time.Time, 1)}
			tb, err := NewTunnel("hello", tunnel.KindA2A, op)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := tc.ctx()
			defer cancel()
			want, _ := ctx.Deadline()
			ctx, ex := WithExchange(ctx)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tb.Base().JoinPath("a2a").String(), strings.NewReader("{}"))
			start := time.Now()
			_, rerr := tb.HTTPClient().Do(req)
			if !errors.Is(rerr, tc.want) {
				t.Fatalf("the request: %v", rerr)
			}
			if d := <-op.deadlines; !d.Equal(want) {
				t.Errorf("the open's deadline %v, want the request's %v", d, want)
			}
			select {
			case err := <-op.ended:
				if !errors.Is(err, tc.want) {
					t.Errorf("the open ended with %v", err)
				}
				if took := time.Since(start); took > 2*time.Second {
					t.Errorf("the open ended %v after the request started", took)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the open outlived its request by 3 s")
			}
			close(op.release)
			time.Sleep(100 * time.Millisecond)
			if n := op.opened.Load(); n != 0 {
				t.Fatalf("%d streams opened after the request was abandoned", n)
			}
			if f := Classify(ctx, rerr, ex); f.Class != Canceled {
				t.Errorf("class %d", f.Class)
			}
		})
	}
}
