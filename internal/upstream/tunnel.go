package upstream

import (
	"context"
	"net"
	"net/http"
	"net/url"

	"kontra.sh/kontra/internal/unimpl"
)

// M2 seam (WP-05): the tunnel backend (spec/tunnel.md §8; relay.md §6.2, §6.4). Behind the same Backend
// interface as Direct, so the a2a-go client and the card fetch above it do not change. Classify gains the
// tunnel's classes when WP-05 builds it: a stream that could not be opened, a session that closed, a
// response that ended incomplete, and a response the node wrote itself (Kontra-Node other than full and
// sealed_rejected) are ClassTunnelLost; Kontra-Node: full is ClassNodeFull.

// Failure classes of a tunnel exchange (relay.md §6.4's tunnel rows).
const (
	// ClassTunnelLost: retried on another session before the first upstream task id; failed: tunnel_lost
	// after it.
	ClassTunnelLost Class = 100 + iota
	// ClassNodeFull: the node's in-flight cap (503, Kontra-Node: full): retried on another session before
	// the first upstream task id; after it, a poll is retried with backoff.
	ClassNodeFull
)

// StreamOpener opens one tunnel stream for one exchange with its preamble written (tunnel.md §8.1).
type StreamOpener interface {
	OpenExchange(ctx context.Context) (net.Conn, error)
}

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
	return nil, unimpl.Error("upstream.NewTunnel", "WP-05")
}

// Kind implements Backend.
func (t *Tunnel) Kind() Kind { return KindTunnel }

// Origin implements Backend: a fixed placeholder origin; the node ignores it (§8.2).
func (t *Tunnel) Origin() *url.URL { panic(unimpl.Error("upstream.Tunnel.Origin", "WP-05")) }

// Base implements Backend: "/" of Origin. The node prefixes its target's base path (§8.2; see the M2
// plan's spec conflict on a non-root base).
func (t *Tunnel) Base() *url.URL { panic(unimpl.Error("upstream.Tunnel.Base", "WP-05")) }

// HTTPClient implements Backend.
func (t *Tunnel) HTTPClient() *http.Client {
	panic(unimpl.Error("upstream.Tunnel.HTTPClient", "WP-05"))
}
