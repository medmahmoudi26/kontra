package tunnelgate

import (
	"context"
	"log/slog"
	"net/http"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/unimpl"
)

// Admitter decides a proven hello; *enroll.Store implements it.
type Admitter interface {
	Admit(ctx context.Context, req enroll.Request) (*enroll.Decision, error)
}

// HandlerConfig configures the upgrade handler.
type HandlerConfig struct {
	// PublicURL is the gate's public_url: hello.origin must equal its origin (bad_origin).
	PublicURL string
	// ChannelBinding is tunnel.channel_binding: tls-exporter (the gate terminates TLS; the exporter is read
	// from the upgrade request's TLS state) or none (§4).
	ChannelBinding string
	ReplicaID      string
	Admitter       Admitter
	Registry       *Registry
	// Draining reports the replica's drain: a draining replica refuses new tunnels (draining, relay.md §14.3).
	Draining func() bool
	Logger   *slog.Logger
}

// Handler serves GET /v1/tunnel (relay.md §2.1). Mounted on the edge router, it is behind the edge's
// rate and connection limits; it carries no credential in a header (§3), so the edge's per-key in-flight
// cap never applies to it.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler returns the upgrade handler.
func NewHandler(cfg HandlerConfig) *Handler { return &Handler{cfg: cfg} }

// ServeHTTP upgrades (400 without the subprotocol; binary messages only after the handshake; compression
// off), runs the handshake within tunnel.HandshakeTimeout with a 16 KiB read limit (§4): hello, the
// origin check, challenge (a UUIDv7 session id, a gate nonce, cb, enc and enc_old), proof, Admit, then
// Registry.Add and welcome; or error with close status 4000 + i. After welcome: yamux.Server, the control
// stream as the first stream, the ping loop, and the accept loop for node-opened streams (closed in M2).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic(unimpl.Error("tunnelgate.Handler.ServeHTTP", "WP-03"))
}
