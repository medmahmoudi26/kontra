// Package upstream is how dispatch reaches a service (spec/relay.md §6.1–§6.2, §6.6).
//
// A Backend is either Direct (a2a: {url}, M1) or a tunnel session (M2, spec/tunnel.md §8). Both give an
// *http.Client whose dialer goes where the backend says — the SSRF-safe dialer for direct backends, a
// fresh tunnel stream per exchange for tunnels — so the a2a-go client and the card fetch above them do
// not change between milestones. Every upstream body is read through a byte limit.
package upstream

import (
	"context"
	"net/http"
	"net/url"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Upstream limits (spec §6.6). Exceeding one fails the exchange upstream_error (service_unavailable for a
// card).
const (
	MaxBody     = 8 << 20   // a non-streaming upstream body
	MaxSSEEvent = 4 << 20   // one SSE event
	MaxCard     = 256 << 10 // a card
)

// Kind is attempts.backend.
type Kind string

// Backend kinds.
const (
	KindDirect Kind = "direct"
	KindTunnel Kind = "tunnel" // M2
)

// Backend is one way to reach one service.
type Backend interface {
	Kind() Kind
	// Origin is the scheme://host[:port] every upstream request is sent to, whatever the upstream card's
	// interface URL says: a card never steers the dialer.
	Origin() *url.URL
	// HTTPClient sends upstream requests: HTTP/1.1, no client timeout, bodies through the byte limits.
	HTTPClient() *http.Client
}

// Direct is an a2a: {url} backend on the operator's network.
type Direct struct {
	URL          *url.URL
	AllowPrivate bool
	client       *http.Client
}

var _ Backend = (*Direct)(nil)

// NewDirect returns the backend for a configured URL, with an ssrf.Dialer of its allow_private policy.
func NewDirect(rawURL string, allowPrivate bool) (*Direct, error) {
	return nil, unimpl.Error("upstream.NewDirect", "WP-04")
}

// Kind implements Backend.
func (d *Direct) Kind() Kind { return KindDirect }

// Origin implements Backend.
func (d *Direct) Origin() *url.URL { return d.URL }

// HTTPClient implements Backend.
func (d *Direct) HTTPClient() *http.Client { return d.client }

// Info is what dispatch needs to speak to an upstream, read from its card (for 0.x cards through
// a2a-go's a2av0 parser, which is never used to produce the served card, D12).
type Info struct {
	ProtocolVersion a2a.ProtocolVersion // "1.0", or "0.3" for the a2av0 transports
	Binding         a2a.TransportProtocol
	Path            string // the interface URL's path, joined to the backend's origin
	Streaming       bool
	Extensions      []string // URIs the upstream card declares
	PushToUpstream  bool     // always false: the gate is the agent's only client (spec §10.1)
}

// ReadInfo parses the upstream card bytes for Info.
func ReadInfo(raw []byte) (Info, error) {
	return Info{}, unimpl.Error("upstream.ReadInfo", "WP-04")
}

// NewClient returns an a2a-go client for the upstream over the backend: JSON-RPC or HTTP+JSON transport
// (a2av0 transports for 0.3 upstreams), A2A-Version for the upstream's version, A2A-Extensions limited to
// the activated URIs the upstream declares, and never the caller's Authorization, cookies or other
// headers, nor a gate credential (spec §6.2).
func NewClient(ctx context.Context, b Backend, info Info, activated []string) (*a2aclient.Client, error) {
	return nil, unimpl.Error("upstream.NewClient", "WP-04")
}

// Fetched is an upstream card exactly as fetched.
type Fetched struct {
	Raw  []byte
	Path string // "/.well-known/agent-card.json", or "/.well-known/agent.json" for 0.2.x sellers
}

// FetchCard GETs /.well-known/agent-card.json through the backend, then /.well-known/agent.json, at most
// MaxCard bytes (spec §3).
func FetchCard(ctx context.Context, b Backend) (*Fetched, error) {
	return nil, unimpl.Error("upstream.FetchCard", "WP-04")
}
