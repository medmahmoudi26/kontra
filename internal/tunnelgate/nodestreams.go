package tunnelgate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
)

// NodeStreams serves the streams a node opens (spec/tunnel.md §9, §8.3), replacing M2's refuseNodeStream:
//
//   - op http (hiring): one HTTP/1.1 request read from the stream is served by Hire, the gate's own public
//     router, with the session in the request's context (HiringSession) so that authentication takes the
//     session's principal: the API key from the handshake's auth when present, else node:<key_id> with the key's
//     tenant and buyer ids.NodeBuyer (spec/relay.md §2.3). Its edge limits are the session's. A request the
//     session's revoked API key made is 401 (spec/tunnel.md §12, auth_revoked); paths outside /agents/…, /mcp/…,
//     /mcp and /.well-known/… never reach Hire (the node refuses them first, and the gate again).
//   - op blob: Blobs resolves ref through the blobs table, checks that its task is one this session serves or
//     one whose buyer is this session's hiring principal's, and writes one JSON line, {"size": n} then exactly n
//     bytes, or {"error": "not_found" | "forbidden" | "too_large"} (an object over 64 MiB), then closes the stream
//     (spec/tunnel.md §8.3, D181).
//
// In flight (§7, §14, D137, D182): a stream counts from Accept until both halves have ended; over Limit (32, 8
// for an anonymous session) the gate answers 429 on it, and still closes the session of a node that keeps more
// than Limit refused streams open, since a refusal frees nothing while the node holds its half.
//
// Answers to a hiring request keep kontra-blob:<ref> (blob.WithKeepRefs, spec/relay.md §10.4 "to nodes"): the
// hiring node fetches them on blob streams, authorized by its hiring principal's buyer.
//
// Work package: M3 WP-03.
type NodeStreams struct {
	// Hire serves hiring requests: the gate's public router behind the hiring edge (HiringRoutes builds the
	// gate's). Nil answers every hiring request 404.
	Hire http.Handler
	// Blobs serves blob streams; nil answers every ref not_found (a gate without a bucket).
	Blobs BlobServer
	// Limit is the in-flight cap of node-opened streams per session: tunnel.GateMaxInflight when zero.
	// Anonymous sessions take tunnel.anonymous's hiring_streams instead (HandlerConfig.AnonymousHiringStreams).
	Limit int
	// Keys re-validates a session's hiring API key on every request (*auth.APIKeys); nil refuses every API key
	// principal (401).
	Keys HireKeys
	// HeaderTimeout bounds the wait for a stream's preamble and its request's header; StreamHeaderTimeout
	// when zero.
	HeaderTimeout time.Duration
	Logger        *slog.Logger
}

// HireKeys re-validates the hiring API key of a session (*auth.APIKeys).
type HireKeys interface {
	// Hold keeps the key under the revocation cache's watch for the session's life.
	Hold(p auth.Principal) (release func())
	// Revalidate answers whether the key still authenticates, and as what.
	Revalidate(ctx context.Context, p auth.Principal) (auth.Principal, error)
}

var _ HireKeys = (*auth.APIKeys)(nil)

// BlobServer writes one object for a blob stream.
type BlobServer interface {
	// ServeBlob checks ref against sess and writes the size line and the object's bytes, or the error line, to w.
	ServeBlob(ctx context.Context, sess *Session, ref string, w io.Writer) error
}

// StreamHeaderTimeout bounds a node-opened stream's preamble and its request's header (the edge's
// limits.header_timeout default).
const StreamHeaderTimeout = 10 * time.Second

func (n *NodeStreams) limit() int {
	if n == nil || n.Limit <= 0 {
		return tunnel.GateMaxInflight
	}
	return n.Limit
}

func (n *NodeStreams) headerTimeout() time.Duration {
	if n.HeaderTimeout > 0 {
		return n.HeaderTimeout
	}
	return StreamHeaderTimeout
}

func (n *NodeStreams) logger() *slog.Logger {
	if n.Logger == nil {
		return slog.Default()
	}
	return n.Logger
}

// Serve handles one accepted node-opened stream of sess until its exchange ends; the caller closes the gate's
// half of st and waits for the node's (accept, D137).
func (n *NodeStreams) Serve(ctx context.Context, sess *Session, st net.Conn) error {
	_ = st.SetReadDeadline(time.Now().Add(n.headerTimeout()))
	br := bufio.NewReaderSize(st, tunnel.MaxPreamble)
	pre, err := tunnel.ReadPreamble(br)
	if err != nil {
		writeRaw(st, http.StatusBadRequest, "the stream has no valid preamble (spec/tunnel.md §9)")
		return fmt.Errorf("tunnelgate: node-opened stream: %w", err)
	}
	_ = st.SetReadDeadline(time.Time{})
	c := &hiringConn{Conn: st, r: br, remote: sess.hireAddr()}
	switch pre.Op {
	case tunnel.OpHTTP:
		n.serveHTTP(ctx, sess, c)
		return nil
	case tunnel.OpBlob:
		return n.serveBlob(ctx, sess, pre.Ref, c)
	default:
		writeRaw(st, http.StatusBadRequest, "a node-opened stream carries op http or blob")
		return fmt.Errorf("tunnelgate: node-opened stream with op %q", pre.Op)
	}
}

// serveBlob answers one blob stream (§8.3).
func (n *NodeStreams) serveBlob(ctx context.Context, sess *Session, ref string, w io.Writer) error {
	if n.Blobs == nil {
		return blob.WriteRefusal(w, blob.ReasonNotFound)
	}
	return n.Blobs.ServeBlob(ctx, sess, ref, w)
}

// serveHTTP serves the stream's one HTTP/1.1 exchange with an http.Server of its own: no keep-alive (one
// exchange per stream), a header read timeout, never a write timeout (SSE; each write has its own deadline,
// hiringConn).
func (n *NodeStreams) serveHTTP(ctx context.Context, sess *Session, c *hiringConn) {
	srv := &http.Server{
		Handler:                      n.handler(sess),
		ReadHeaderTimeout:            n.headerTimeout(),
		MaxHeaderBytes:               http.DefaultMaxHeaderBytes,
		ErrorLog:                     log.New(io.Discard, "", 0),
		BaseContext:                  func(net.Listener) context.Context { return ctx },
		DisableGeneralOptionsHandler: true,
	}
	srv.SetKeepAlivesEnabled(false)
	l := newOneConnListener(c)
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	_ = srv.Serve(l)
}

// HiringPath reports whether a hiring request's path may reach the router (spec/tunnel.md §9): /agents/…, /mcp/…,
// /mcp and /.well-known/…. Anything else is 404 on the node, and again on the gate.
func HiringPath(p string) bool {
	return strings.HasPrefix(p, "/agents/") || strings.HasPrefix(p, "/mcp/") || p == "/mcp" || strings.HasPrefix(p, "/.well-known/")
}

// handler is the hiring request's handler: the path check, then Hire with the session's principal source.
func (n *NodeStreams) handler(sess *Session) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Hire == nil || !HiringPath(r.URL.Path) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The session decides who this is (auth.HireSource); a credential the node forwarded is never read.
		r.Header.Del("Authorization")
		ctx := WithHiringSession(r.Context(), sess)
		ctx = auth.WithHireSource(ctx, hireSource{n: n, s: sess})
		ctx = blob.WithKeepRefs(ctx)
		n.Hire.ServeHTTP(w, r.WithContext(ctx))
	})
}

// hireSource is a session's auth.HireSource.
type hireSource struct {
	n *NodeStreams
	s *Session
}

func (h hireSource) HirePrincipal(ctx context.Context) (auth.Principal, error) {
	return h.n.principal(ctx, h.s)
}

// principal is the session's hiring principal now (spec/relay.md §2.3, spec/tunnel.md §9, §12): the handshake's
// API key re-validated (a revoked or expired one is ErrUnauthenticated, and the node is told auth_revoked once),
// else node:<key_id> while the key may still open streams.
func (n *NodeStreams) principal(ctx context.Context, s *Session) (auth.Principal, error) {
	if !s.open() {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if s.hire != nil {
		if s.hireRevoked.Load() || n.Keys == nil {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
		p, err := n.Keys.Revalidate(ctx, *s.hire)
		if errors.Is(err, auth.ErrUnauthenticated) {
			s.authRevoked(n.logger())
		}
		return p, err
	}
	if err := s.keyUsable(ctx); err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	buyer, err := ids.NodeBuyer(ids.Namespace, s.KeyID)
	if err != nil {
		return auth.Principal{}, err
	}
	tenant := s.TenantID
	return auth.Principal{Kind: auth.KindNode, ID: string(auth.KindNode) + ":" + s.KeyID, TenantID: &tenant, TenantSlug: s.Tenant,
		BuyerID: buyer, Via: auth.ViaNode}, nil
}

// Hiring is the session's hiring principal now (NodeStreams.principal), for a BlobServer's check of a task the
// session hires; ErrUnauthenticated when it may not hire any more. A session no Handler served has none.
func (s *Session) Hiring(ctx context.Context) (auth.Principal, error) {
	if s.hiring == nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return s.hiring.principal(ctx, s)
}

// keyUsable checks the session's own key on a hiring request (spec/relay.md §2.3: revocation on every request):
// the revocation view when fresh, else the key's row.
func (s *Session) keyUsable(ctx context.Context) error {
	rev := s.revocations()
	if rev == nil {
		return nil
	}
	if rev.Fresh() {
		if !rev.KeyUsable(s.KeyID, time.Now()) {
			return fmt.Errorf("%w: key %s is revoked", ErrSessionGone, s.KeyID)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, allowsReadTimeout)
	defer cancel()
	ok, err := rev.Usable(rctx, s.KeyID, "")
	switch {
	case err != nil:
		return fmt.Errorf("%w: the key's revocation state cannot be read (%v)", ErrSessionGone, err)
	case !ok:
		return fmt.Errorf("%w: key %s is revoked", ErrSessionGone, s.KeyID)
	}
	return nil
}

// authRevoked records that the session's hiring API key no longer authenticates: every later stream it opens is
// 401, and the node is told {"t": "auth_revoked"} once on the control stream (§12, §13). Hosting goes on.
func (s *Session) authRevoked(logger *slog.Logger) {
	if s.hire == nil || !s.hireRevoked.CompareAndSwap(false, true) {
		return
	}
	log := logger.With(obs.KeyTunnelSession, s.ID, "key_id", s.KeyID, obs.KeyPrincipal, s.HirePrincipal)
	log.Info("tunnel session: its hiring API key was revoked; hiring streams now get 401, hosting goes on (§12)")
	tell := func() {
		if err := s.WriteControl(tunnel.Control{T: tunnel.CtlAuthRevoked}); err != nil {
			log.Debug("tunnel session: {t: auth_revoked} not delivered", "error", err)
		}
	}
	if s.reg != nil {
		s.reg.goAsync(tell)
		return
	}
	go tell()
}

// HireRevoked reports whether the session's hiring API key was found revoked.
func (s *Session) HireRevoked() bool { return s.hireRevoked.Load() }

// NodeStreamsAccepted is the number of node-opened streams the gate accepted on the session; NodeInFlight the
// number whose halves have not both ended.
func (s *Session) NodeStreamsAccepted() int64 { return s.nodeAccepted.Load() }

// NodeInFlight is the number of node-opened streams in flight (§14: from Accept until both halves end).
func (s *Session) NodeInFlight() int64 { return s.nodeInFlight.Load() }

// hireAddr is the client address a hiring request gets: one per session, an address no network can hold
// (fd00::/8 with the session's hash in the network bits), so that the edge's per-address limits count the session
// as one client (spec/tunnel.md §9: its edge limits are the session's).
func (s *Session) hireAddr() net.Addr {
	h := sha256.Sum256(s.ID[:])
	var a [16]byte
	a[0] = 0xfd
	copy(a[1:8], h[:7])
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom16(a), 0))
}

// acceptNodeStreams accepts the node-opened streams of s promptly (§7: an un-accepted stream holds the node's
// open, and its data, until the backlog fills) and serves each with ns. It returns when the session ends.
//
// In flight (§7, §14, D137, D182). Closing a stream ends only the gate's half of it: yamux keeps the stream, and
// buffers up to a window (tunnel.MaxStreamWindowSize) of what the node still sends on it, until the node closes
// its half, resets it, or StreamCloseTimeout (5 min) passes; yamux v0.1.2 has no reset. So a stream counts from
// Accept until it has ended on both sides: once served (or refused), the gate closes its half and reads and
// discards what the node sends until the node's half ends. Over the cap (sessionCap: ns.Limit, 32, or an anonymous
// session's hiring_streams, 8) the gate answers 429
// (an http stream; a blob stream is closed unanswered), and a node that keeps more than the cap of refused streams
// open loses its session: what a node can pin is then at most twice the cap's windows and the backlog's.
func acceptNodeStreams(ctx context.Context, s *Session, mux *yamux.Session, ns *NodeStreams, sessionCap int, log *slog.Logger) {
	limit := int64(sessionCap)
	if limit <= 0 {
		limit = int64(ns.limit())
	}
	var refused atomic.Int64
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		s.nodeAccepted.Add(1)
		n := s.nodeInFlight.Add(1)
		if n > limit {
			r := refused.Add(1)
			if r > limit {
				log.Warn("tunnel session closed: the node keeps more refused streams open than the gate allows (§14)",
					"limit", limit, "stream", st.StreamID())
				_ = mux.Close()
				return
			}
			log.Info("node-opened stream refused: over the in-flight cap (§14)", "limit", limit, "in_flight", n, "stream", st.StreamID())
			go func() {
				ns.refuse(st)
				ended(mux, st)
				refused.Add(-1)
				s.nodeInFlight.Add(-1)
				log.Debug("node-opened stream ended", "stream", st.StreamID())
			}()
			continue
		}
		go func() {
			if err := ns.Serve(ctx, s, st); err != nil {
				log.Debug("node-opened stream refused", "stream", st.StreamID(), "error", err)
			}
			ended(mux, st)
			// Counted down before the line, so that a test that saw the line sees the count.
			s.nodeInFlight.Add(-1)
			log.Debug("node-opened stream ended", "stream", st.StreamID())
		}()
	}
}

// refuse answers a stream over the cap: 429 on an http stream (its preamble read first, within the header
// timeout), nothing on any other.
//
// SPEC-CONFLICT (D209; spec/tunnel.md §14 "over it 429" with §8.3's framing): a blob stream is no HTTP exchange,
// and §8.3's error line names not_found, forbidden and too_large only, with no "busy". The closest faithful
// behaviour: a blob stream over the cap is closed unanswered, which blob.Fetch reports as a stream that ended early
// (no blob.ErrBadAnswer). kontra connect keeps its node-opened streams under the cap itself (internal/node,
// session.open: a stream waits for a slot), so a node that behaves meets this only when the gate still counts a
// stream the node has seen end; its hosting exchange is then answered 503 Kontra-Node: full, which the relay
// retries, and a hiring answer 502. D209 adds no "busy" reason to §8.3.
func (n *NodeStreams) refuse(st *yamux.Stream) {
	_ = st.SetReadDeadline(time.Now().Add(n.headerTimeout()))
	pre, err := tunnel.ReadPreamble(bufio.NewReaderSize(st, tunnel.MaxPreamble))
	_ = st.SetReadDeadline(time.Time{})
	if err == nil && pre.Op == tunnel.OpHTTP {
		writeRaw(st, http.StatusTooManyRequests, "this tunnel session has too many node-opened streams in flight (spec/tunnel.md §14)")
	}
}

// ended closes the gate's half of st and reads and discards what the node sends until its half ends: its FIN
// (EOF) or reset, the stream's close timeout (StreamCloseTimeout after the gate's FIN), or the session's end.
// Reading is the only way yamux tells the gate a stream ended; what the node can make the gate read is
// discarded as it arrives. yamux v0.1.2 marks a session's streams closed only after its connection's close is
// done, and until then a read blocked on one of them spins, so the session's end sets a deadline that has
// passed (Session.wakeLingering's reason).
func ended(mux *yamux.Session, st *yamux.Stream) {
	_ = st.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-mux.CloseChan():
			_ = st.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	_, _ = io.Copy(io.Discard, st)
	close(done)
}

// writeRaw answers a stream whose request was never read with status and closes nothing (the caller does).
func writeRaw(c net.Conn, status int, msg string) {
	body := msg + "\n"
	_ = c.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
	extra := ""
	if status == http.StatusTooManyRequests {
		extra = "Retry-After: 1\r\n"
	}
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\n%sContent-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), extra, len(body), body)
	_ = c.SetWriteDeadline(time.Time{})
}

// hiringConn is a node-opened stream whose preamble was read through r: the request is read from r, every write
// has its own deadline (tunnel.ConnectionWriteTimeout: a node that stops reading an answer, an SSE stream
// included, frees the handler within it, D155), and its remote address is the session's (Session.hireAddr).
type hiringConn struct {
	net.Conn
	r      *bufio.Reader
	remote net.Addr
}

func (c *hiringConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *hiringConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
	return c.Conn.Write(p)
}

func (c *hiringConn) RemoteAddr() net.Addr { return c.remote }

// oneConnListener hands its one connection to http.Server.Serve, then blocks until that connection is closed
// (the exchange ended), so that Serve returns when the stream's exchange is over.
type oneConnListener struct {
	c      net.Conn
	taken  atomic.Bool
	closed chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{}), done: make(chan struct{})}
	l.c = &notifyConn{Conn: c, closed: l.closed}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.c, nil
	}
	select {
	case <-l.closed:
	case <-l.done:
	}
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// notifyConn closes its channel when it is closed. Its Close does not close the stream: the stream's halves are
// accept's to end (ended), after the exchange.
type notifyConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type hiringKey struct{}

// WithHiringSession marks a request as one a node opened on sess (spec/tunnel.md §9).
func WithHiringSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, hiringKey{}, sess)
}

// HiringSession returns the session a request arrived on, ok false for a request from the network.
func HiringSession(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(hiringKey{}).(*Session)
	return s, ok && s != nil
}
