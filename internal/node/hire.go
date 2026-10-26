package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
)

// errNoSession is OpenHire's and OpenBlob's error while the node has no session that takes new streams: it is
// dialing, or its only session is draining (§13: no new hiring streams on a draining session).
var errNoSession = errors.New("node: no tunnel session takes new streams")

// noOpener is the opener ParseArgs checks the hiring flags with (nothing is opened).
type noOpener struct{}

func (noOpener) OpenHire(context.Context) (net.Conn, error) { return nil, errNoSession }

// setCurrent makes s the session hiring streams go on (nil: none, while the node dials), and wakes the waits of
// OpenHire.
func (n *Node) setCurrent(s *session) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.current = s
	close(n.currentCh)
	n.currentCh = make(chan struct{})
}

// clearCurrent forgets s as the current session, if it still is.
func (n *Node) clearCurrent(s *session) {
	n.mu.Lock()
	cur := n.current
	n.mu.Unlock()
	if cur == s {
		n.setCurrent(nil)
	}
}

// currentSession waits until a session takes new streams, or ctx ends: the current one, never a retired (drained)
// or closed one.
func (n *Node) currentSession(ctx context.Context) (*session, error) {
	for {
		n.mu.Lock()
		s, ch := n.current, n.currentCh
		n.mu.Unlock()
		if s != nil && !s.retired.Load() && !s.mux.IsClosed() {
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", errNoSession, ctx.Err())
		}
	}
}

// OpenHire opens a hiring stream on the current session, its preamble {"v": 1, "op": "http"} written (§9);
// hiring.Server forwards one local request on it. On a drain the next one goes on the new session.
func (n *Node) OpenHire(ctx context.Context) (net.Conn, error) {
	s, err := n.currentSession(ctx)
	if err != nil {
		return nil, err
	}
	return s.open(ctx, &tunnel.Preamble{Op: tunnel.OpHTTP})
}

// OpenBlob opens a blob stream on the current session, its preamble left to blob.Fetch (§8.3): the hiring
// server's inlining of an answer's kontra-blob: parts.
func (n *Node) OpenBlob(ctx context.Context) (net.Conn, error) {
	s, err := n.currentSession(ctx)
	if err != nil {
		return nil, err
	}
	return s.open(ctx, nil)
}

// open opens a node-opened stream on the session, bounded by ctx (yamux's OpenStream takes none and blocks while
// AcceptBacklog opens are unacknowledged), and writes pre when it is not nil.
func (s *session) open(ctx context.Context, pre *tunnel.Preamble) (net.Conn, error) {
	return openStream(ctx, s.mux, pre)
}

func openStream(ctx context.Context, mux *yamux.Session, pre *tunnel.Preamble) (net.Conn, error) {
	type opened struct {
		st  *yamux.Stream
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		st, err := mux.OpenStream()
		ch <- opened{st, err}
	}()
	var st *yamux.Stream
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, fmt.Errorf("%w: %v", errNoSession, o.err)
		}
		st = o.st
	case <-ctx.Done():
		go func() {
			if o := <-ch; o.st != nil {
				_ = o.st.Close()
			}
		}()
		return nil, ctx.Err()
	}
	if pre != nil {
		_ = st.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
		if err := tunnel.WritePreamble(st, *pre); err != nil {
			_ = st.Close()
			return nil, err
		}
		_ = st.SetWriteDeadline(time.Time{})
	}
	return st, nil
}
