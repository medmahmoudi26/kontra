// Package tunneltest connects the two sides of the tunnel over a real loopback TCP connection that a test
// can freeze or cut, for the transport's tests (internal/tunnel, internal/tunnelgate, internal/node and
// the e2e harness). It is test support only: nothing in a binary imports it.
//
// A frozen Wire is a peer that went silent without closing (a dead NAT path, probe tunnel [keepalive]):
// what it would send is dropped and what it would read stalls. A cut Wire is a killed process: its TCP
// connection closes.
package tunneltest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
)

// Wire is one side's end of a TCP connection that can be frozen or cut.
type Wire struct {
	net.Conn

	frozen   atomic.Bool
	released chan struct{} // closed by Close and Cut: a stalled read returns
	once     sync.Once
}

// NewWire wraps c.
func NewWire(c net.Conn) *Wire { return &Wire{Conn: c, released: make(chan struct{})} }

// Freeze makes this side silent and deaf without closing anything: writes are dropped (and report
// success), reads stall, and a Close from now on never reaches the peer (the path is dead), until Cut.
func (w *Wire) Freeze() { w.frozen.Store(true) }

// Frozen reports whether Freeze was called.
func (w *Wire) Frozen() bool { return w.frozen.Load() }

// Cut closes the TCP connection, as a killed process does, frozen or not; a stalled read returns
// net.ErrClosed.
func (w *Wire) Cut() error {
	w.release()
	return w.Conn.Close()
}

// Close is the side closing its socket: on a live wire the peer sees it; on a frozen one it does not,
// and only this side's stalled reads end.
func (w *Wire) Close() error {
	w.release()
	if w.frozen.Load() {
		// Wake a read blocked in the socket without sending anything: the TCP connection stays open, as
		// on a dead path, until Cut.
		return w.Conn.SetReadDeadline(time.Unix(1, 0))
	}
	return w.Conn.Close()
}

func (w *Wire) release() { w.once.Do(func() { close(w.released) }) }

// Read reads from the connection; on a frozen wire what arrives is discarded and the read stalls until
// the wire is closed or cut.
func (w *Wire) Read(p []byte) (int, error) {
	if w.frozen.Load() {
		<-w.released
		return 0, net.ErrClosed
	}
	n, err := w.Conn.Read(p)
	if w.frozen.Load() {
		<-w.released
		return 0, net.ErrClosed
	}
	return n, err
}

// Write writes to the connection; on a frozen wire the bytes vanish.
func (w *Wire) Write(p []byte) (int, error) {
	if w.frozen.Load() {
		return len(p), nil
	}
	return w.Conn.Write(p)
}

// Wires dials TCP and keeps every connection it made as a Wire. Put its DialContext in an
// http.Transport, and TLS, the WebSocket upgrade and yamux all ride the wires.
type Wires struct {
	Dialer net.Dialer
	// Redirect, when set, is dialed instead of the address asked for (a test gate at a name such as
	// example.com, which httptest's certificate covers).
	Redirect string

	mu   sync.Mutex
	list []*Wire
}

// DialContext dials and records the connection.
func (ws *Wires) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if ws.Redirect != "" {
		addr = ws.Redirect
	}
	c, err := ws.Dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	w := NewWire(c)
	ws.mu.Lock()
	ws.list = append(ws.list, w)
	ws.mu.Unlock()
	return w, nil
}

// All is every wire dialed so far, oldest first.
func (ws *Wires) All() []*Wire {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]*Wire(nil), ws.list...)
}

// Last is the newest wire, nil before the first dial.
func (ws *Wires) Last() *Wire {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.list) == 0 {
		return nil
	}
	return ws.list[len(ws.list)-1]
}

// Client is an HTTP client whose connections are wires of ws, for websocket.Dial (or node.Dialer).
func (ws *Wires) Client(base *http.Transport) *http.Client {
	var tr *http.Transport
	if base != nil {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	tr.DialContext = ws.DialContext
	tr.DialTLSContext = nil
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Pair is a WebSocket connection, upgraded with the kontra-tunnel.v1 subprotocol, between a node side (the
// dialer) and a gate side (the acceptor) of one httptest server, before any handshake: what the transport
// sees once the four handshake messages are done.
type Pair struct {
	Node *websocket.Conn
	Gate *websocket.Conn
	// NodeWire is the node's end of the TCP connection.
	NodeWire *Wire
}

// NewPair connects a pair over plain HTTP on loopback; t's cleanup closes both sides and the server.
func NewPair(t testing.TB) *Pair {
	t.Helper()
	gates := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The hijacked connection outlives the handler.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnel.Subprotocol}})
		if err != nil {
			return
		}
		gates <- c
	}))
	t.Cleanup(srv.Close)

	ws := &Wires{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, _, err := websocket.Dial(ctx, srv.URL, &websocket.DialOptions{
		HTTPClient:   ws.Client(nil),
		Subprotocols: []string{tunnel.Subprotocol},
	})
	if err != nil {
		t.Fatalf("tunneltest: dial: %v", err)
	}
	var gate *websocket.Conn
	select {
	case gate = <-gates:
	case <-ctx.Done():
		t.Fatal("tunneltest: the server side never accepted")
	}
	p := &Pair{Node: node, Gate: gate, NodeWire: ws.Last()}
	t.Cleanup(func() {
		_ = p.NodeWire.Cut()
		_ = p.Node.CloseNow()
		_ = p.Gate.CloseNow()
	})
	return p
}

// Sessions starts yamux on both sides with cfg (tunnel.YamuxConfig(nil) when nil): the node as
// yamux.Client, the gate as yamux.Server (§1). t's cleanup closes them.
func (p *Pair) Sessions(t testing.TB, cfg *yamux.Config) (node, gate *yamux.Session) {
	t.Helper()
	if cfg == nil {
		cfg = tunnel.YamuxConfig(nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	node, err := tunnel.ClientSessionWith(ctx, p.Node, cfg)
	if err != nil {
		t.Fatal(err)
	}
	gate, err = tunnel.ServerSessionWith(ctx, p.Gate, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A frozen wire never answers the WebSocket close; cut it so that Close does not wait for it.
		if p.NodeWire.Frozen() {
			_ = p.NodeWire.Cut()
		}
		_ = node.Close()
		_ = gate.Close()
	})
	return node, gate
}

// ShortConfig is tunnel.YamuxConfig with ConnectionWriteTimeout (the pong deadline) set to pong, for
// liveness tests that shorten the ping interval too (a ping's failure takes the pong deadline).
func ShortConfig(pong time.Duration) *yamux.Config {
	c := tunnel.YamuxConfig(nil)
	c.ConnectionWriteTimeout = pong
	return c
}
