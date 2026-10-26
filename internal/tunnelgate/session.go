package tunnelgate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// allowsReadTimeout bounds the key and binding read Allows makes while the revocation view is not fresh
// (enroll.Revocations.Usable).
const allowsReadTimeout = 2 * time.Second

// Session is one live tunnel session on this replica.
type Session struct {
	ID            uuid.UUID
	KeyID         string
	TenantID      uuid.UUID
	Tenant        string
	HirePrincipal string
	CLIVersion    string
	NodeName      string

	mux       *yamux.Session
	reg       *Registry // set by Registry.Add; nil for sessions no registry holds (the Handler's tests)
	control   net.Conn  // the control stream, the first one the gate opened (set by the Handler)
	controlMu sync.Mutex

	mu         sync.Mutex
	served     map[string]string // service name → kind
	closed     bool              // no new stream: closing, closed, revoked or superseded
	why        string            // why it closed, for the streams refused after
	registered bool              // its tunnels row is in (Add committed)
	rowGone    bool              // its tunnels row was deleted by this replica
	toldDrain  bool
	exchanges  map[*exchange]struct{}
}

// Allows is the key_scope check made before every stream (relay.md §6.1): the session is open, its key is
// neither revoked nor past a rotation's overlap, and the service is in its served set with this kind. A
// rotated key inside its overlap is allowed on its existing session (§12). Every refusal wraps
// ErrSessionGone: the request is handled as a lost session and no stream is opened.
//
// The key and the binding are read from the replica's revocation view (enroll.Revocations, kept current by
// NOTIFY kontra_revoke and a re-read at least every 5 s); while the view is not fresh they are read from
// Postgres, and a read that fails refuses (fail closed).
func (s *Session) Allows(service, kind string) error {
	ctx, cancel := context.WithTimeout(context.Background(), allowsReadTimeout)
	defer cancel()
	return s.allows(ctx, service, kind)
}

func (s *Session) allows(ctx context.Context, service, kind string) error {
	if err := s.allowsLocal(service, kind); err != nil {
		return err
	}
	rev := s.revocations()
	if rev == nil || rev.Fresh() {
		return nil // allowsLocal asked the fresh view
	}
	ok, err := rev.Usable(ctx, s.KeyID, service)
	switch {
	case err != nil:
		return fmt.Errorf("%w: the key's revocation state cannot be read (%v)", ErrSessionGone, err)
	case !ok:
		return fmt.Errorf("%w: key %s is revoked or past its rotation's overlap, or its binding to %s was revoked", ErrSessionGone, s.KeyID, service)
	}
	return nil
}

// allowsLocal is Allows without a database read: the session's own state, and the revocation view when it
// is fresh.
func (s *Session) allowsLocal(service, kind string) error {
	s.mu.Lock()
	closed, why := s.closed, s.why
	k, ok := s.served[service]
	s.mu.Unlock()
	switch {
	case closed:
		return fmt.Errorf("%w: %s", ErrSessionGone, why)
	case s.mux != nil && s.mux.IsClosed():
		return fmt.Errorf("%w: the session closed", ErrSessionGone)
	case !ok:
		return fmt.Errorf("%w: the session does not serve %s", ErrSessionGone, service)
	case k != kind:
		return fmt.Errorf("%w: %s is a %s service on this session, not %s", ErrSessionGone, service, k, kind)
	}
	rev := s.revocations()
	if rev == nil || !rev.Fresh() {
		return nil
	}
	if !rev.KeyUsable(s.KeyID, time.Now()) {
		return fmt.Errorf("%w: key %s is revoked or past its rotation's overlap", ErrSessionGone, s.KeyID)
	}
	if rev.BindingRevoked(s.KeyID, service) {
		return fmt.Errorf("%w: the binding of key %s to %s was revoked", ErrSessionGone, s.KeyID, service)
	}
	return nil
}

func (s *Session) revocations() *enroll.Revocations {
	if s.reg == nil {
		return nil
	}
	return s.reg.cfg.Revocations
}

// OpenExchange checks Allows, opens a stream and writes its preamble (§8.1); the caller writes one HTTP/1.1
// request on it. A stream that could not be opened, a session closing, and every refusal of Allows wrap
// ErrSessionGone; ctx's own end is ctx's error.
//
// The stream is the session's until it is closed: a binding revocation aborts the streams of its service,
// and closing the session aborts all of them. An aborted stream's reads and writes fail at once with an
// error wrapping ErrSessionGone, so the exchange over it is a lost session (tunnel_lost, relay.md §6.4),
// never a cancellation (M2 plan audit: relay.Registry.Abort would record aborted).
//
// yamux's OpenStream takes no context and blocks only while AcceptBacklog opens are unacknowledged; it runs
// on its own goroutine here, so ctx bounds the wait, and a stream that opens after ctx ended is closed.
func (s *Session) OpenExchange(ctx context.Context, p tunnel.Preamble) (net.Conn, error) {
	if p.Op != tunnel.OpHTTP {
		return nil, fmt.Errorf("tunnelgate: an exchange's preamble has op http, not %q", p.Op)
	}
	if err := s.allows(ctx, p.Service, p.Kind); err != nil {
		return nil, err
	}
	st, err := s.openStream(ctx)
	if err != nil {
		return nil, err
	}
	x := &exchange{Stream: st, s: s, service: p.Service}
	if !s.track(x) {
		_ = st.Close()
		return nil, s.goneErr()
	}
	// A revocation that ran between the check and track did not see this stream.
	if err := s.allowsLocal(p.Service, p.Kind); err != nil {
		x.abort(err)
		return nil, err
	}
	deadline := time.Now().Add(tunnel.ConnectionWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = st.SetWriteDeadline(deadline)
	if err := tunnel.WritePreamble(x, p); err != nil {
		x.abort(fmt.Errorf("%w: the preamble was not written: %v", ErrSessionGone, err))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: the preamble was not written: %v", ErrSessionGone, err)
	}
	_ = st.SetWriteDeadline(time.Time{})
	return x, nil
}

// openStream is mux.OpenStream bounded by ctx.
func (s *Session) openStream(ctx context.Context) (*yamux.Stream, error) {
	if s.mux == nil {
		return nil, fmt.Errorf("%w: the session has no multiplexer", ErrSessionGone)
	}
	type opened struct {
		st  *yamux.Stream
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		st, err := s.mux.OpenStream()
		ch <- opened{st, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, fmt.Errorf("%w: a stream could not be opened: %v", ErrSessionGone, o.err)
		}
		return o.st, nil
	case <-ctx.Done():
		go func() {
			if o := <-ch; o.st != nil {
				_ = o.st.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// Close closes the session with a reason for the log (its dispatches fail tunnel_lost: every stream on it
// is aborted) and deletes its tunnels row. It is safe to call more than once and on a session already
// closed by its node.
func (s *Session) Close(reason string) error {
	return s.close(reason, true)
}

// close is Close; log says whether the registry logs the close (the Handler logs the ones the node or the
// ping loop caused).
func (s *Session) close(reason string, log bool) error {
	first := s.shut(reason)
	if s.mux != nil {
		_ = s.mux.Close()
	}
	if s.reg == nil {
		return nil
	}
	return s.reg.remove(s, reason, first && log)
}

// shut marks the session closed (no new stream) and aborts its streams; it reports whether this call
// closed it.
func (s *Session) shut(reason string) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed, s.why = true, reason
	xs := make([]*exchange, 0, len(s.exchanges))
	for x := range s.exchanges {
		xs = append(xs, x)
	}
	s.mu.Unlock()
	err := fmt.Errorf("%w: %s", ErrSessionGone, reason)
	for _, x := range xs {
		x.abort(err)
	}
	return true
}

func (s *Session) goneErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Errorf("%w: %s", ErrSessionGone, s.why)
}

// open reports whether the session takes new streams.
func (s *Session) open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && (s.mux == nil || !s.mux.IsClosed())
}

// Serves reports whether the session serves service (of any kind) now.
func (s *Session) Serves(service string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.served[service]
	return ok
}

// Served is the session's served set now: service names, sorted.
func (s *Session) Served() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servedLocked()
}

func (s *Session) servedLocked() []string {
	out := make([]string, 0, len(s.served))
	for name := range s.served {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// dropService removes service from the served set and aborts its streams with why (a binding revocation,
// §12). It reports whether the session served it.
func (s *Session) dropService(service string, why error) bool {
	s.mu.Lock()
	_, had := s.served[service]
	delete(s.served, service)
	var xs []*exchange
	for x := range s.exchanges {
		if x.service == service {
			xs = append(xs, x)
		}
	}
	s.mu.Unlock()
	for _, x := range xs {
		x.abort(why)
	}
	return had
}

// keepServed narrows the served set to the names in keep and returns the names it dropped.
func (s *Session) keepServed(keep map[string]bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []string
	for name := range s.served {
		if !keep[name] {
			delete(s.served, name)
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// track adds a stream to the session's in-flight set, unless the session has closed.
func (s *Session) track(x *exchange) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.exchanges == nil {
		s.exchanges = map[*exchange]struct{}{}
	}
	s.exchanges[x] = struct{}{}
	return true
}

func (s *Session) untrack(x *exchange) {
	s.mu.Lock()
	delete(s.exchanges, x)
	s.mu.Unlock()
}

// InFlight is the number of streams the gate opened on the session and has not closed.
func (s *Session) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.exchanges)
}

// exchange is one gate-opened stream for one service. Once aborted, its reads and writes fail with the
// abort's error, buffered data or not.
type exchange struct {
	*yamux.Stream
	s       *Session
	service string

	mu      sync.Mutex
	aborted error
	once    sync.Once
}

func (x *exchange) abortErr() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.aborted
}

func (x *exchange) Read(p []byte) (int, error) {
	if err := x.abortErr(); err != nil {
		return 0, err
	}
	n, err := x.Stream.Read(p)
	if err != nil {
		if a := x.abortErr(); a != nil {
			return n, a
		}
	}
	return n, err
}

func (x *exchange) Write(p []byte) (int, error) {
	if err := x.abortErr(); err != nil {
		return 0, err
	}
	n, err := x.Stream.Write(p)
	if err != nil {
		if a := x.abortErr(); a != nil {
			return n, a
		}
	}
	return n, err
}

// Close closes the gate's half of the stream and leaves the session's in-flight set.
func (x *exchange) Close() error {
	x.once.Do(func() { x.s.untrack(x) })
	return x.Stream.Close()
}

// abort fails the stream's reads and writes with err from now on: the deadline wakes a read or write in
// progress, the stream leaves the session's in-flight set, and the gate's half is closed. yamux v0.1.2 has
// no stream reset, so what the node still sends is buffered (up to a window) until it closes its half or
// StreamCloseTimeout passes.
//
// abort does not wait on the network. Closing the gate's half sends a FIN, and yamux's Stream.Close waits
// for that frame to be written, up to ConnectionWriteTimeout (15 s) per stream, against a node that stopped
// reading (wedged, or hostile: a stolen key being revoked). Its callers are a revocation (Registry.Revoked,
// on the revocation view's emitter, or on the LISTEN goroutine during a reconnect's catch-up), CloseAll and
// a session's close, so the FIN goes out on a goroutine of its own (M2 review of WP-04): the stream's reads
// and writes already fail with err, and the session's close ends a FIN still waiting.
func (x *exchange) abort(err error) {
	if err == nil {
		err = ErrSessionGone
	}
	x.mu.Lock()
	if x.aborted == nil {
		if !errors.Is(err, ErrSessionGone) {
			err = fmt.Errorf("%w: %w", ErrSessionGone, err)
		}
		x.aborted = err
	}
	x.mu.Unlock()
	_ = x.Stream.SetDeadline(time.Now())
	x.once.Do(func() { x.s.untrack(x) })
	go func() { _ = x.Stream.Close() }()
}
