package node

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
)

// event is what a session tells Run.
type event int

const (
	evDrain     event = iota + 1 // {"t": "drain"}: dial a new session at once (make-before-break, D35)
	evExhausted                  // the gate's or the node's stream ids are (nearly) exhausted (§14): dial a new session
)

// streamIDHeadroom is how many stream ids before yamux's limit (it refuses an open at id MaxUint32-1) the
// node dials a new session: the gate's stream ids are even and grow by 2, so this leaves it about half a
// million opens, minutes at any rate the gate dispatches, for the new session's handshake.
const streamIDHeadroom = 1 << 20

// nearExhaustion reports a stream id close enough to yamux's limit that the session will soon open no
// more streams (ErrStreamsExhausted on the gate's side, §14).
func nearExhaustion(id uint32) bool { return id >= math.MaxUint32-1-streamIDHeadroom }

// session is one tunnel session of the node (§7): a yamux client over the WebSocket the node dialed.
type session struct {
	n       *Node
	id      string // welcome.session
	replica string
	key     string // the active key's recipient when it was opened
	mux     *yamux.Session
	proxy   *Proxy
	opened  time.Time
	log     *slog.Logger
	events  chan event
	ctx     context.Context // ends when the session closes, or Run ends
	// auth is the API key the session's hello carried (its hiring principal), "" for none.
	auth string
	// slots bounds the node-opened streams in flight on the session to the gate's cap (§14; session.open).
	slots chan struct{}

	retired  atomic.Bool // drained or replaced: serves on, never redialed
	replaced atomic.Bool // its key, rotated since, was revoked: Run dials the active key's session at once
	control  atomic.Bool // its control stream was opened
	exhaust  atomic.Bool // evExhausted was sent
}

// signal tells Run about the session, without waiting: Run watches only the current session's events.
func (s *session) signal(ev event) {
	select {
	case s.events <- ev:
	default:
	}
}

// accept accepts every gate-opened stream promptly, serving each on its own goroutine (§7: an un-accepted
// stream holds the gate's open, and after StreamOpenTimeout the whole session closes; the in-flight cap is
// the Proxy's, after Accept). It returns when the session closes.
func (s *session) accept() {
	for {
		st, err := s.mux.AcceptStream()
		if err != nil {
			return
		}
		if nearExhaustion(st.StreamID()) {
			s.streamsExhausted()
		}
		go s.proxy.ServeStream(s.ctx, st)
	}
}

// readControl reads the control stream (§13, D35): JSON lines from the gate; the node answers nothing on
// it and ignores the types it does not know. Only the first control stream of a session is read.
//
//   - drain: Run dials a new session at once; this one goes on serving the streams the gate opens on it
//     and closes when the gate closes it (make-before-break). The node opens no hiring streams on it: they go
//     on the new session (Node.OpenHire).
//   - revoked: the key (no service): the node stops and does not redial (§12); a rotated key's session ends
//     alone (Node.keyRevoked). revoked with a service: the active key's binding: the service is dropped
//     from every session's served set and from the next hello (Node.dropService); a rotated key's binding:
//     from that session alone (Node.bindingRevoked).
//   - auth_revoked: the gate answers the session's hiring requests 401 from now on, and hosting goes on (§12,
//     §13). The revoked API key is dropped from the credentials and from every later hello (Node.authRevoked),
//     since the next handshake would otherwise be refused auth_invalid, which §4 makes final.
//   - enrolled and expiring are logged (anonymous sessions are M4).
func (s *session) readControl(stream net.Conn, br *bufio.Reader) {
	if !s.control.CompareAndSwap(false, true) {
		s.log.Warn("a second control stream on one session is refused")
		return
	}
	for {
		c, err := tunnel.ReadControl(br)
		if err != nil {
			if !errors.Is(err, io.EOF) && s.ctx.Err() == nil && !s.mux.IsClosed() {
				s.log.Warn("the control stream ended", "err", err)
			}
			return
		}
		switch c.T {
		case tunnel.CtlDrain:
			s.log.Info("the gate drains this session's replica (§13)")
			if !s.retired.Load() {
				s.signal(evDrain)
			}
		case tunnel.CtlRevoked:
			if c.Service == "" {
				s.n.keyRevoked(s)
				continue
			}
			s.n.bindingRevoked(s, c.Service)
		case tunnel.CtlAuthRevoked:
			s.n.authRevoked(s)
		case tunnel.CtlEnrolled:
			s.log.Info("another key was enrolled for a service this node hosts (§5)", "service", c.Service, "enrolled_key_id", c.Key)
		case tunnel.CtlExpiring:
			s.log.Info("the gate says this session expires", "at", c.At)
		default:
			s.log.Debug("an unknown control message is ignored", "t", c.T)
		}
	}
}
