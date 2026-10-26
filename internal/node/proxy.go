package node

import (
	"context"
	"log/slog"
	"net"

	"kontra.sh/kontra/internal/unimpl"
)

// Proxy serves gate-opened streams (§8): it reads the preamble, refuses a service this session does not
// host (404, Kontra-Node: not_hosted) or a kind that does not match (404, kind_mismatch), and over
// MaxInflight streams answers 503 (full). It forwards the stream's one HTTP/1.1 request to the service's
// configured target and nowhere else (sidecar_not_a_proxy):
//
//   - Host and an absolute-form request target are ignored: scheme, host and port come from --serve;
//   - the path is percent-decoded and cleaned (., .., //), prefixed with the target's base path, and a
//     result outside that base is 400;
//   - Host is the target's host; Origin, hop-by-hop headers, Forwarded and X-Forwarded-* are removed, none
//     added; CONNECT, Upgrade and TRACE are 400;
//   - responses stream as they come, text/event-stream flushed per event, never compressed; the stream's
//     server sets a header read timeout only, never a write timeout;
//   - Kontra-Node and Kontra-Sealed-Reject are removed from the service's responses; a target that does not
//     answer is 502 with Kontra-Node: target_unreachable.
//
// A url part in a body is never dialed: the node does not read bodies (D56).
type Proxy struct {
	serves      map[string]Serve
	maxInflight int
	logger      *slog.Logger
}

// NewProxy returns a proxy for the services the session serves.
func NewProxy(serves []Serve, maxInflight int, logger *slog.Logger) *Proxy {
	panic(unimpl.Error("node.NewProxy", "WP-06"))
}

// SetServed narrows the hosted services to those the welcome lists, and later drops one the gate revoked
// ({"t": "revoked", "service"}): its preambles are then refused not_hosted.
func (p *Proxy) SetServed(names []string) {
	panic(unimpl.Error("node.Proxy.SetServed", "WP-06"))
}

// ServeStream serves one accepted stream; it returns when the exchange ends.
func (p *Proxy) ServeStream(ctx context.Context, stream net.Conn) {
	panic(unimpl.Error("node.Proxy.ServeStream", "WP-06"))
}
