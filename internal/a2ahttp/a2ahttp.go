// Package a2ahttp serves every A2A route of the gate (spec/relay.md §2.1, §2.4, §3, §9).
//
// For each service path {svc} (/agents/{name}, or /agents/{tenant}/{name} on a multi-tenant gate):
//
//	GET  {svc} and {svc}/.well-known/agent-card.json   the served card (D10)
//	GET  {svc}/.well-known/agent-card.upstream.json    the upstream card, verbatim (D13)
//	POST {svc} and {svc}/                              JSON-RPC 2.0 (the TCK posts the trailing slash)
//	     {svc}/message:send, {svc}/tasks…, …           HTTP+JSON, every route of spec §5.3
//
// JSON-RPC requests are read (within the body limit) to route SendStreamingMessage and SubscribeToTask
// to the gate's SSE writer and everything else to a2a-go's JSON-RPC handler; on HTTP+JSON,
// message:stream and tasks/{id}:subscribe (GET and POST) go to the SSE writer and the rest to a2a-go's
// REST handler under http.StripPrefix. A JSON-RPC request with a non-empty tenant is an unknown service.
//
// An unknown service, a service of the other kind, a disabled one, or one whose allow: does not admit the
// principal all get the same 404, with a body that fits the route (D50).
package a2ahttp

import (
	"context"
	"net/http"

	"github.com/medmahmoudi26/kontra/internal/auth"
	"github.com/medmahmoudi26/kontra/internal/card"
	"github.com/medmahmoudi26/kontra/internal/relay"
	"github.com/medmahmoudi26/kontra/internal/sse"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Service is what a route needs to know about one service.
type Service struct {
	Ref      store.ServiceRef
	Allow    auth.AllowList
	Listed   bool
	Disabled bool
	Cards    func() (*card.Served, error)
}

// Services resolves a path's tenant and name to a service (nil when there is none).
type Services interface {
	Lookup(ctx context.Context, tenant, name string) (*Service, error)
}

// Deps are the server's collaborators.
type Deps struct {
	Services Services
	Handlers *relay.Handlers
	SSE      *sse.Writer
	Auth     *auth.Authenticator
	Multi    bool // multi-tenant paths
}

// Server is the A2A route set, mounted on the edge's closed router under /agents/.
type Server struct {
	deps Deps
}

// New returns the A2A routes.
func New(deps Deps) *Server {
	return &Server{deps: deps}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic(unimpl.Error("a2ahttp.Server.ServeHTTP", "WP-06"))
}

// Versions normalizes A2A-Version (header, else query parameter): 1.0 and 1.0.<patch> become 1.0; absent
// or empty means 0.3 and is refused with VersionNotSupportedError (-32009, HTTP 400) naming the header to
// send (spec §2.4, D9).
func Versions(next http.Handler) http.Handler {
	panic(unimpl.Error("a2ahttp.Versions", "WP-06"))
}

// Extensions splits, trims and deduplicates A2A-Extensions (one comma-separated header, several headers,
// or both; X-A2A-Extensions as an alias), drops URIs the served card does not declare, refuses a request
// missing a required URI with ExtensionSupportRequiredError (-32008) before any task row exists, and
// echoes the activated URIs in one A2A-Extensions response header (spec §2.4, D9).
func Extensions(declared func(r *http.Request) (all, required []string), next http.Handler) http.Handler {
	panic(unimpl.Error("a2ahttp.Extensions", "WP-06"))
}
