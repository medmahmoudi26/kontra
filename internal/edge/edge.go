// Package edge is the gate's public listener: a complete edge on its own, meant to be exposed directly
// (PRD §12 "Front proxies"; spec/relay.md §2.1–§2.2).
//
// It terminates TLS (ACME or a provided certificate), sends HSTS, bounds headers and idle connections,
// caps bodies, rates and in-flight requests before authentication, and serves a closed default route:
// a path that is not one of the gate's routes is an empty 404. Metrics live on their own listener.
//
// Dependencies the work package adds: golang.org/x/crypto/acme/autocert (ACME) and golang.org/x/time/rate
// (token buckets). Neither is pinned by spec/relay.md §0; the work package records the versions.
package edge

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Limits are the per-replica edge limits (spec §2.2). Zero values are replaced by the defaults.
type Limits struct {
	Body           int64         // 413 above it; default 4 MiB
	InflightPerKey int           // 429 + Retry-After above it; default 32
	RPSPerIP       float64       // 429 + Retry-After above it; default 50
	HeaderTimeout  time.Duration // http.Server.ReadHeaderTimeout; default 10 s
	Idle           time.Duration // http.Server.IdleTimeout; default 60 s
}

// KeepAlive is the SSE and long-poll comment interval (`: keep-alive`, spec §2.2).
const KeepAlive = 15 * time.Second

// Config configures one public listener.
type Config struct {
	Listen    string
	PublicURL string
	TLS       *tls.Config // nil: plaintext, allowed only when PublicURL is a localhost origin
	Limits    Limits
}

// Server is the public listener. WriteTimeout is never set (SSE); slow bodies are bounded per request.
type Server struct {
	cfg Config
	srv *http.Server
}

// New returns a server for routes behind the edge middleware.
func New(cfg Config, routes http.Handler) (*Server, error) {
	return nil, unimpl.Error("edge.New", "WP-03")
}

// Serve accepts until Shutdown.
func (s *Server) Serve(ctx context.Context) error {
	return unimpl.Error("edge.Server.Serve", "WP-03")
}

// Shutdown stops accepting and waits for in-flight requests until ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	return unimpl.Error("edge.Server.Shutdown", "WP-03")
}

// Middleware is the edge chain, outermost first: closed default route, HSTS (TLS only), per-IP rate,
// body cap, then (after the route has read the bearer credential, before it is verified) the per-key
// in-flight cap. Refusals happen before any task row exists; on JSON-RPC routes the body is also a
// JSON-RPC error (-32600, "request too large" or "rate limited") when the request could be parsed.
func Middleware(l Limits, tlsOn bool, next http.Handler) http.Handler {
	panic(unimpl.Error("edge.Middleware", "WP-03"))
}

// Router is the closed route table: only the routes registered on it answer, everything else is an
// empty 404.
type Router struct {
	mux *http.ServeMux
}

// NewRouter returns an empty closed router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers h for a Go 1.22 ServeMux pattern.
func (r *Router) Handle(pattern string, h http.Handler) {
	panic(unimpl.Error("edge.Router.Handle", "WP-03"))
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	panic(unimpl.Error("edge.Router.ServeHTTP", "WP-03"))
}

// Healthz is GET /healthz: 200 when this replica's Postgres and Temporal connections are up and it is not
// draining, else 503 (spec §2.1).
func Healthz(ready func() bool) http.Handler {
	panic(unimpl.Error("edge.Healthz", "WP-03"))
}
