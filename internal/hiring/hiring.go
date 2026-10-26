// Package hiring is the hiring side of kontra connect (spec/tunnel.md §9, D34): the node serves
// 127.0.0.1:7777 (--listen) to local clients and forwards each request on a stream it opens in its own tunnel,
// preamble {"v": 1, "op": "http"}, path unchanged: /agents/…, /mcp/…, /mcp and /.well-known/…; anything else is
// 404. A local client never sends a credential to the node, and the node never sends one the gate did not
// issue: the gate takes the session's principal (spec/relay.md §2.3).
//
// Local requests are checked first (DNS rebinding, other local users, MCP's Origin rule; local_rebinding):
// a Host that is not 127.0.0.1:<port>, localhost:<port> or [::1]:<port> of --listen, or an Origin not listed by
// --allow-origin (default none), is 403 (an id-less JSON-RPC error on MCP paths) and opens no stream; OPTIONS is
// 405; --listen off loopback requires --local-token, whose bearer the node strips. There is no other local port
// (no console, D164).
//
// Cards: the node verifies the gate's signature on every card it serves locally, with a jku equal to
// {the origin it dialed}/.well-known/jwks.json, and rewrites supportedInterfaces[].url to its own
// http://localhost:<port>/… URLs; a card whose signature does not verify is not served. Sealing and opening for
// sealed services happen around the forwarding (internal/sealing, M3 WP-08); blobs a reply names as
// kontra-blob:<ref> are fetched on blob streams and inlined (internal/blob).
//
// Work package: M3 WP-03 (listener, checks, forwarding, cards), M3 WP-08 (sealing around it).
package hiring

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"kontra.sh/kontra/internal/unimpl"
)

// DefaultListen is --listen's default.
const DefaultListen = "127.0.0.1:7777"

// Config is the hiring side's part of the kontra connect command line.
type Config struct {
	Listen       string   // --listen
	LocalToken   string   // --local-token <file>, read; required when Listen is not loopback
	AllowOrigins []string // --allow-origin, repeatable
	Gate         *url.URL // the gate the node dialed: the jku origin and the card URLs' replacement base
}

// Opener opens one node-opened stream in the current session, preamble written; it fails while the node has no
// session or is draining (no new hiring streams on a draining session, spec/tunnel.md §13).
type Opener interface {
	OpenHire(ctx context.Context) (net.Conn, error)
}

// Server is the local endpoint.
type Server struct {
	cfg  Config
	open Opener
	log  *slog.Logger
}

// New checks cfg (a non-loopback Listen without LocalToken is refused).
func New(cfg Config, open Opener, log *slog.Logger) (*Server, error) {
	return nil, unimpl.Error("hiring.New", "M3 WP-03")
}

// Handler is the local HTTP handler: the checks above, then the forwarding.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, unimpl.Error("hiring.Server", "M3 WP-03").Error(), http.StatusNotImplemented)
	})
}

// Serve serves ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return unimpl.Error("hiring.Server.Serve", "M3 WP-03")
}

// CheckLocal applies the local request checks (Host, Origin, local token) to r; a refusal is the status to answer.
func CheckLocal(r *http.Request, cfg Config) (status int, err error) {
	return http.StatusForbidden, unimpl.Error("hiring.CheckLocal", "M3 WP-03")
}

// RewriteCard verifies the gate's signature on a served card (jku = gate origin + /.well-known/jwks.json, the
// key set fetched from that origin only and cached) and returns it with every supportedInterfaces[].url moved
// to local, the node's own base for the service.
func RewriteCard(ctx context.Context, card []byte, gate *url.URL, local *url.URL, jwks *http.Client) ([]byte, error) {
	return nil, unimpl.Error("hiring.RewriteCard", "M3 WP-03")
}
