package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/tunnel"
)

// The tunnel backend (spec/tunnel.md §8; relay.md §6.2, §6.4). Behind the same Backend interface as Direct,
// so the a2a-go client and the card fetch above it do not change. Classify gains the tunnel's classes for an
// exchange over it: a stream that could not be opened, a session that closed, a response that ended
// incomplete, and a response the node wrote itself (Kontra-Node other than full and sealed_rejected) are
// ClassTunnelLost; Kontra-Node: full is ClassNodeFull; Kontra-Node: sealed_rejected is ClassSealedRejected.

// Failure classes of a tunnel exchange (relay.md §6.4's tunnel rows).
const (
	// ClassTunnelLost: retried on another session before the first upstream task id; failed: tunnel_lost
	// after it.
	ClassTunnelLost Class = 100 + iota
	// ClassNodeFull: the node's in-flight cap (503, Kontra-Node: full): retried on another session before
	// the first upstream task id; after it, the exchange is retried with backoff (relay.md §6.4).
	ClassNodeFull
	// ClassSealedRejected: a hosting node refused a sealed part (400, Kontra-Node: sealed_rejected,
	// ext-sealed.md §6): failed: sealed_rejected, never retried. Sealing is M3; the class exists so that
	// such an answer is never taken for the service's own.
	ClassSealedRejected
)

// ErrTunnelLost is an exchange whose tunnel session is gone: the stream could not be opened, the session
// is not held by this replica, or its key, binding or session no longer allow the stream (key_scope,
// relay.md §6.1). The dispatch fails tunnel_lost (relay.md §6.4, §14.2).
var ErrTunnelLost = errors.New("upstream: the tunnel session to the service is gone")

// ExchangeTask is the gate task an exchange belongs to: the preamble's task and context (tunnel.md §8.1),
// which the node needs to check sealed headers (ext-sealed.md). It travels in the request's context (the
// dispatch and forward_cancel set it, WithTask) because one backend serves every task of its session and
// service: http.Transport hands the request context's values to the dial that opens the stream.
type ExchangeTask struct {
	TaskID    string
	ContextID string
}

type exchangeTaskKey struct{}

// WithTask returns ctx carrying the gate task of the exchanges made with it.
func WithTask(ctx context.Context, t ExchangeTask) context.Context {
	return context.WithValue(ctx, exchangeTaskKey{}, t)
}

// TaskFrom returns the gate task WithTask set, if any (a card fetch belongs to no task).
func TaskFrom(ctx context.Context) (ExchangeTask, bool) {
	t, ok := ctx.Value(exchangeTaskKey{}).(ExchangeTask)
	return t, ok
}

// StreamOpener opens one tunnel stream for one exchange with its preamble written (tunnel.md §8.1): the
// service and kind it was made for, and the task of TaskFrom(ctx). An error the session caused (a stream
// that could not be opened, a session closing, a key or binding that no longer allows it) should wrap
// ErrTunnelLost; NewTunnel's dial wraps any other error that is not ctx's own.
//
// ctx bounds the open only, and ends with the exchange's request: its cancellation and its deadline are the
// request's (dialContext), so an open still waiting when its request is abandoned returns, and opens no
// stream after it.
type StreamOpener interface {
	OpenExchange(ctx context.Context) (net.Conn, error)
}

type requestContextKey struct{}

// withRequestContext returns r carrying its own context as a value, which survives into the dial:
// http.Transport hands its dial the request context's values but detaches it from the request's
// cancellation and deadline (net/http getConn: context.WithoutCancel), so that a dial may outlive its
// request for a later one. A tunnel stream is never reused (DisableKeepAlives), so a dial that outlives its
// request only leaks: a stuck open (the key and binding read while the revocation view is not fresh, a yamux
// open behind un-acknowledged ones) would wait past the request's end, and then open a stray stream.
func withRequestContext(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestContextKey{}, r.Context()))
}

// dialContext is the dial's context bound to its request again (withRequestContext): the request's own
// context, so it carries the request's deadline and ends with the request (deadline or cancel, with the
// request's error), and it also ends when the transport ends the dial itself (CloseIdleConnections). stop
// releases it once the dial returned.
func dialContext(ctx context.Context) (dctx context.Context, stop func()) {
	rc, ok := ctx.Value(requestContextKey{}).(context.Context)
	if !ok {
		return ctx, func() {}
	}
	dctx, cancel := context.WithCancelCause(rc)
	unhook := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return dctx, func() {
		unhook()
		cancel(nil)
	}
}

// exchangeOrigin is the origin of the requests the gate writes on a tunnel stream: a placeholder, since the
// node sends each request to its --serve target and sets Host itself, whatever the request says (§8.2).
// .invalid never resolves (RFC 6761), so it can never be dialed by mistake.
const exchangeOrigin = "http://kontra-node.invalid"

// maxTunnelHeaderBytes bounds a response header read through the tunnel (as ssrf's transport does).
const maxTunnelHeaderBytes = 64 << 10

// Tunnel is a backend over one tunnel session for one service: every exchange is a fresh stream carrying
// one HTTP/1.1 request in origin form with Connection: close; the client has no overall timeout
// (deadlines come from the request context), never reuses a stream, reads every body through the §6.6
// limits, and records the Kontra-Node header of a response in the request's Exchange. The node ignores
// Host and the scheme, host and port: they always come from its --serve target (§8.2).
type Tunnel struct {
	Service     string
	ServiceKind string // tunnel.KindA2A in M2
	open        StreamOpener
	client      *http.Client
}

var _ Backend = (*Tunnel)(nil)

// NewTunnel returns the backend of one service over a session's opener.
func NewTunnel(service, kind string, open StreamOpener) (*Tunnel, error) {
	switch {
	case service == "":
		return nil, errors.New("upstream: a tunnel backend needs its service")
	case kind != tunnel.KindA2A && kind != tunnel.KindMCP:
		return nil, fmt.Errorf("upstream: a tunnel backend's kind %q", kind)
	case open == nil:
		return nil, errors.New("upstream: a tunnel backend needs its session's opener")
	}
	t := &Tunnel{Service: service, ServiceKind: kind, open: open}
	tr := &http.Transport{
		Proxy: nil, // never the environment's proxy: the request rides the stream
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, stop := dialContext(ctx)
			defer stop()
			c, err := open.OpenExchange(ctx)
			if err == nil {
				return c, nil
			}
			if ctx.Err() == nil && !errors.Is(err, ErrTunnelLost) {
				err = fmt.Errorf("%w: %w", ErrTunnelLost, err)
			}
			return nil, err
		},
		// One exchange per stream, never reused (§8.2): the transport sends Connection: close and closes the
		// stream after the response.
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxTunnelHeaderBytes,
	}
	t.client = &http.Client{Transport: &limitTransport{next: tr, tunnel: true}, CheckRedirect: ssrf.NoRedirects}
	return t, nil
}

// Kind implements Backend.
func (t *Tunnel) Kind() Kind { return KindTunnel }

// Origin implements Backend: a fixed placeholder origin; the node ignores it (§8.2).
func (t *Tunnel) Origin() *url.URL {
	u, _ := url.Parse(exchangeOrigin)
	return u
}

// Base implements Backend: "/" of Origin. The node prefixes its target's base path (§8.2; see the M2
// plan's spec conflict on a non-root base, which kontra connect refuses until the owner decides).
func (t *Tunnel) Base() *url.URL {
	u := t.Origin()
	u.Path = "/"
	return u
}

// HTTPClient implements Backend.
func (t *Tunnel) HTTPClient() *http.Client { return t.client }
