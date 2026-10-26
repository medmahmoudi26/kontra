package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/hiring"
	"kontra.sh/kontra/internal/tunnel"
)

// errNoSession is OpenHire's and OpenBlob's error while the node has no session that takes new streams: it is
// dialing, or its only session is draining (§13: no new hiring streams on a draining session).
var errNoSession = errors.New("node: no tunnel session takes new streams")

// errBusy is a node-opened stream that found no slot of its session within the node's slotWait (§14): hiring
// answers 429 (hiring.ErrBusy), a hosted exchange's blob fetch 503 full.
var errBusy = fmt.Errorf("node: %w", hiring.ErrBusy)

// defaultSlotWait is how long a node-opened stream waits for a slot of its session (Node.slotWait): a burst of
// local requests queues this long, then gets 429; a blob fetch of a hosted exchange, then 503 full, which the
// gate retries.
const defaultSlotWait = 5 * time.Second

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
// AcceptBacklog opens are unacknowledged), and writes pre when it is not nil. Every node-opened stream, hiring,
// the key set's fetch and blob fetches alike, the hiring server's and the hosting proxy's, holds one of the
// session's slots from before its open until both its halves have ended (slotStream).
//
// Slots (spec/tunnel.md §14, D137, D182): the gate counts a session's node-opened streams from its accept until
// both halves end, answers 429 over its cap (32, 8 for an anonymous session) and closes the session of a node that
// keeps more refused streams open than that. The node therefore keeps at most the cap in flight itself: an open
// waits up to slotWait for a slot and then fails with errBusy, opening nothing.
//
// Stream ids (§14: on ErrStreamsExhausted the node reconnects): an open refused ErrStreamsExhausted, or one that
// took one of the last ids (nearExhaustion), retires the session; Run dials a new one at once.
func (s *session) open(ctx context.Context, pre *tunnel.Preamble) (net.Conn, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	// A stream whose open completes after ctx ended is closed at once, and keeps its slot until the gate's half
	// ends too, as every other.
	st, err := openStream(ctx, s.mux, func(orphan *yamux.Stream) {
		if orphan == nil {
			release()
			return
		}
		_ = (&slotStream{Stream: orphan, mux: s.mux, release: release}).Close()
	})
	if err != nil {
		if errors.Is(err, errNoSession) { // the open itself failed (else ctx ended: the orphan's func releases)
			release()
		}
		if errors.Is(err, yamux.ErrStreamsExhausted) {
			s.streamsExhausted()
		}
		return nil, err
	}
	if nearExhaustion(st.StreamID()) {
		s.streamsExhausted()
	}
	ss := &slotStream{Stream: st, mux: s.mux, release: release}
	if pre != nil {
		_ = st.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
		if err := tunnel.WritePreamble(st, *pre); err != nil {
			_ = ss.Close()
			return nil, err
		}
		_ = st.SetWriteDeadline(time.Time{})
	}
	return ss, nil
}

// acquire takes one of the session's slots, waiting up to the node's slotWait: errBusy when none frees in time,
// errNoSession when the session closes meanwhile, ctx's error when it ends. release gives the slot back (once).
func (s *session) acquire(ctx context.Context) (release func(), err error) {
	var once sync.Once
	release = func() { once.Do(func() { <-s.slots }) }
	select {
	case s.slots <- struct{}{}:
		return release, nil
	default:
	}
	wait := defaultSlotWait
	if s.n != nil && s.n.slotWait > 0 {
		wait = s.n.slotWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
		return release, nil
	case <-t.C:
		// Its caller logs the refusal (hiring: 429, a hosted exchange: 503 full).
		s.log.Debug("a node-opened stream found no slot: the session has as many in flight as the gate allows (§14)",
			"limit", cap(s.slots), "waited", wait)
		return nil, errBusy
	case <-s.mux.CloseChan():
		return nil, fmt.Errorf("%w: the session closed", errNoSession)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// streamsExhausted retires the session once its stream ids run out, or nearly (§14).
func (s *session) streamsExhausted() {
	if s.exhaust.CompareAndSwap(false, true) {
		s.signal(evExhausted)
	}
}

// slotStream is a node-opened stream holding one of its session's slots until both its halves have ended, as the
// gate counts it (§14): Close ends the node's half, and the slot is given back once the gate's half has ended too:
// its FIN or reset read (what the gate still sends is discarded), the stream's close timeout, or the session's end.
type slotStream struct {
	*yamux.Stream
	mux     *yamux.Session
	release func()
	once    sync.Once
}

func (s *slotStream) Close() error {
	err := s.Stream.Close()
	s.once.Do(func() { go s.drain() })
	return err
}

// drain reads the gate's half until it ends, then releases the slot. yamux v0.1.2 marks a session's streams
// closed only after its connection's close is done, and a read blocked meanwhile spins, so the session's end sets
// a deadline that has passed (as tunnelgate's ended does).
func (s *slotStream) drain() {
	defer s.release()
	_ = s.Stream.SetReadDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		select {
		case <-s.mux.CloseChan():
			_ = s.Stream.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	_, _ = io.Copy(io.Discard, s.Stream)
	close(done)
}

// openStream opens a stream on mux, bounded by ctx: yamux's OpenStream takes no context. Its own failure wraps
// errNoSession (and yamux's error); when ctx ends first it returns ctx's error, and abandoned later receives the
// stream the open made, or nil when it failed.
func openStream(ctx context.Context, mux *yamux.Session, abandoned func(*yamux.Stream)) (*yamux.Stream, error) {
	type opened struct {
		st  *yamux.Stream
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		st, err := mux.OpenStream()
		ch <- opened{st, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, fmt.Errorf("%w: %w", errNoSession, o.err)
		}
		return o.st, nil
	case <-ctx.Done():
		go func() { abandoned((<-ch).st) }()
		return nil, ctx.Err()
	}
}
