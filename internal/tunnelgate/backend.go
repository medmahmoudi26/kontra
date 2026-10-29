package tunnelgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

// Backend returns the upstream.Backend of an attempt placed on session id for svc (relay.md §6.2): an
// upstream.Tunnel whose StreamOpener is the session's OpenExchange with the preamble {"v": 1, "op":
// "http", "service", "kind", "task", "context"}, the task and context taken from the exchange's context
// (upstream.WithTask). An error wrapping both ErrSessionGone and upstream.ErrTunnelLost when this replica
// does not hold the session, the session is another tenant's, or it no longer serves the service (closed,
// key or binding revoked, key_scope): the dispatch fails tunnel_lost at once and no stream is opened
// (relay.md §6.1, §14.2). Every stream the backend opens is checked again (Session.OpenExchange).
func (r *Registry) Backend(id uuid.UUID, svc store.ServiceRef) (upstream.Backend, error) {
	s, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %w: this replica holds no session %s", upstream.ErrTunnelLost, ErrSessionGone, id)
	}
	if s.TenantID != svc.TenantID {
		return nil, fmt.Errorf("%w: %w: session %s is another tenant's", upstream.ErrTunnelLost, ErrSessionGone, id)
	}
	kind := svc.Kind
	if kind == "" {
		kind = tunnel.KindA2A
	}
	if err := s.allowsLocal(svc.Name, kind); err != nil {
		return nil, fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
	}
	return upstream.NewTunnel(svc.Name, kind, &sessionOpener{s: s, service: svc.Name, kind: kind})
}

// MCPBackend returns the upstream.Backend of an MCP exchange on session id for the tenant's service (relay.md
// §11.2, §6.1): an upstream.Tunnel whose streams carry the preamble {"v": 1, "op": "http", "service", "kind":
// "mcp", "mcp_session", "mcp_request_id"}, the MCP fields taken from the exchange's context (upstream.WithMCP).
// Unlike Backend it reads a session that has just closed too, so its refusal says why: an error wrapping
// ErrRevoked when the key or binding was revoked (the pinned row closes revoked), ErrSessionGone (and
// upstream.ErrTunnelLost) when the session is gone or another tenant's, and ErrCheckUnavailable when the key's
// state cannot be read. Every stream the backend opens is checked again (Session.OpenExchange).
func (r *Registry) MCPBackend(id, tenantID uuid.UUID, service string) (upstream.Backend, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, fmt.Errorf("%w: %w: this replica holds no session %s", upstream.ErrTunnelLost, ErrSessionGone, id)
	}
	s.mu.Lock()
	registered := s.registered
	s.mu.Unlock()
	switch {
	case !registered:
		return nil, fmt.Errorf("%w: %w: session %s is not registered", upstream.ErrTunnelLost, ErrSessionGone, id)
	case s.TenantID != tenantID:
		return nil, fmt.Errorf("%w: %w: session %s is another tenant's", upstream.ErrTunnelLost, ErrSessionGone, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), allowsReadTimeout)
	err := s.allows(ctx, service, tunnel.KindMCP)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
	}
	return upstream.NewTunnel(service, tunnel.KindMCP, &sessionOpener{s: s, service: service, kind: tunnel.KindMCP})
}

// sessionOpener opens the exchange streams of one service on one session.
type sessionOpener struct {
	s       *Session
	service string
	kind    string
}

// OpenExchange implements upstream.StreamOpener: Session.OpenExchange with the preamble of the exchange's
// task (§8.1). Its errors, and the stream's read and write errors once the session is gone, wrap
// upstream.ErrTunnelLost.
func (o *sessionOpener) OpenExchange(ctx context.Context) (net.Conn, error) {
	p := tunnel.Preamble{Op: tunnel.OpHTTP, Service: o.service, Kind: o.kind}
	if t, ok := upstream.TaskFrom(ctx); ok {
		p.Task, p.Context = t.TaskID, t.ContextID
	}
	if m, ok := upstream.MCPFrom(ctx); ok && o.kind == tunnel.KindMCP {
		p.MCPSession, p.MCPRequestID = m.Session, m.RequestID
	}
	c, err := o.s.OpenExchange(ctx, p)
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			err = fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
		}
		return nil, err
	}
	return &exchangeConn{Conn: c, s: o.s}, nil
}

// exchangeConn is an exchange stream as the gate's HTTP client reads it. "A stream that ends without a
// complete HTTP response, or while the session is closing, is tunnel_lost" (§8.2): yamux ends every
// stream of a session that closes with a clean EOF, which a response delimited by the connection's close
// would read as its end, so an EOF once the session is closed is the session's loss instead. Reads and
// writes the session aborted (ErrSessionGone) wrap upstream.ErrTunnelLost too.
type exchangeConn struct {
	net.Conn
	s *Session
}

func (c *exchangeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF) && !c.s.open():
		err = fmt.Errorf("%w: %w: the session closed during the exchange", upstream.ErrTunnelLost, ErrSessionGone)
	case errors.Is(err, ErrSessionGone):
		err = fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
	}
	return n, err
}

func (c *exchangeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil && errors.Is(err, ErrSessionGone) {
		err = fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
	}
	return n, err
}
