package mcptest

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
)

// FakeNode is the node's side of a tunnel session for tests (spec/tunnel.md §8): it accepts the streams the gate
// opens, reads each preamble and records it, and serves the stream as one HTTP/1.1 exchange with Handler (the
// double, or anything else), as a node's fixed-target proxy would after rewriting Host. It never reads a control
// stream: a node that ignores control messages (mcp_session_revoked).
type FakeNode struct {
	Handler http.Handler

	mu        sync.Mutex
	preambles []tunnel.Preamble
}

// Serve accepts streams on m until it closes.
func (n *FakeNode) Serve(m *yamux.Session) {
	for {
		st, err := m.AcceptStream()
		if err != nil {
			return
		}
		go n.serve(st)
	}
}

// Preambles are the preambles of the exchanges the gate opened, in order.
func (n *FakeNode) Preambles() []tunnel.Preamble {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]tunnel.Preamble(nil), n.preambles...)
}

func (n *FakeNode) serve(st net.Conn) {
	br := bufio.NewReader(st)
	p, err := tunnel.ReadPreamble(br)
	if err != nil {
		_ = st.Close()
		return
	}
	if p.Op == tunnel.OpControl {
		// The control stream: read and ignored, as a node that does not honour control messages does.
		_, _ = io.Copy(io.Discard, br)
		return
	}
	if p.Op != tunnel.OpHTTP {
		_ = st.Close()
		return
	}
	n.mu.Lock()
	n.preambles = append(n.preambles, p)
	n.mu.Unlock()
	l := &oneConn{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &closeNotify{Conn: &prefixed{Conn: st, r: br}, l: l}
	srv := &http.Server{Handler: n.Handler, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(l)
}

// prefixed reads through the reader that read the preamble.
type prefixed struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }

// oneConn is a listener of one connection.
type oneConn struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *oneConn) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{} }

type closeNotify struct {
	net.Conn
	l *oneConn
}

func (c *closeNotify) Close() error { _ = c.l.Close(); return c.Conn.Close() }
