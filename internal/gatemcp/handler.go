// Package gatemcp is the gate's own MCP server at {public_url}/mcp (spec/relay.md §12): four tools (search,
// describe, hire, reply) and the MCP Tasks of the tasks hire creates, in both MCP eras (§12.5, D24, DM4-2).
//
// It is not a tunnelled service and has no upstream. §11.1's transport rules apply (one message per POST, strict
// decoding with internal/mcpwire, Origin, versions) and §11.12's classification of a request's era (rules 1–5):
//
//   - Legacy (2025-11-25, 2025-06-18): initialize opens a session, an mcp_sessions row whose service is NULL and
//     whose tenant is the principal's (internal/mcprelay's Sessions). Nothing is pinned and nothing hops: any replica
//     serves any request of a session. The session's GET stream carries notifications/elicitation/complete (§12.3,
//     DM4-6) and nothing else; it has no event ids and is not resumed.
//   - Modern (2026-07-28): the gate answers server/discover itself, advertising the tasks extension; a hire whose
//     request declares it becomes a task answered as §11.12 step 4, and tasks/get, tasks/update and tasks/cancel
//     serve hire tasks. No virtual session is opened and Admission's session operation is not consulted (DM4-2).
//
// /mcp has no anonymous principal: a request without a credential is 401, with §12.1's challenge when the MCP
// OAuth resource server is configured (internal/oidc, DM4-3, DM4-9). Through a node, the session's principal
// applies (spec/tunnel.md §9).
//
// hire creates an ordinary A2A gate task (protocol a2a, dispatched as §6) through Hirer, marked tasks.hired
// (DM4-5); Admission (operation task) decides first, and a quote leads to URL elicitation (-32042, legacy clients
// that declared elicitation.url) or to the approve URL in the answer (D21). The URL is the operator's (Cloud:
// https://app.kontra.sh/#/t/<taskId>, D188); the gate never builds it.
//
// Work packages: M4 WP-04 (the legacy era, the four tools, -32042, MCP Tasks over hire tasks), M4 WP-05 (the
// 2026-07-28 era), M4 WP-06 (the OAuth resource server in front of it).
package gatemcp

import (
	"log/slog"
	"net/http"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/unimpl"
)

// Path is the route of the gate's own MCP server (spec §2.1).
const Path = "/mcp"

// Deps are the handler's collaborators, wired by internal/gate.
type Deps struct {
	PublicURL      string
	Multi          bool                // tenancy: multi: refs are <tenant>/<name> (§12.2, DM4-4)
	Auth           *auth.Authenticator // API keys and node principals; the OAuth resource server wraps it (WP-06)
	Sessions       *mcprelay.Sessions  // legacy sessions: mcp_sessions rows with service NULL
	Catalog        *Catalog            // search and describe
	Hirer          Hirer               // creates and follows up hire tasks
	Tasks          *HireTasks          // the MCP Tasks of hire tasks, both eras
	Elicitations   *Elicitations       // -32042 and notifications/elicitation/complete (§12.3)
	Admission      hooks.Admission     // operation task, for each hire (§12.3 step 1; DM4-8)
	Log            *store.Log          // the task log hire tasks are read from
	Waker          store.TaskWaker     // the replica's LISTEN connection: plain waits, tasks/result, GET streams
	AllowedOrigins []string            // mcp.allowed_origins (§11.1)
	Logger         *slog.Logger
}

// Handler serves POST, GET and DELETE on /mcp after the edge and the credential check.
type Handler struct {
	deps Deps
}

// NewHandler checks deps.
func NewHandler(deps Deps) (*Handler, error) {
	return nil, unimpl.Error("gatemcp.NewHandler", "M4 WP-04")
}

// Register mounts POST /mcp as a JSON-RPC route (the edge's 413 and 429 carry a JSON-RPC error) and GET and DELETE
// /mcp as plain routes, on the edge's closed router.
func (h *Handler) Register(rt *edge.Router) {
	panic(unimpl.Error("gatemcp.Handler.Register", "M4 WP-04"))
}

// ServeHTTP classifies the request's era (§11.12 rules 1–5) and serves it: initialize, tools/list, tools/call of
// the four tools, the legacy tasks/* (§11.6 over hire tasks), server/discover and the modern tasks/* (§12.5), the
// GET stream of a legacy session, and its DELETE.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic(unimpl.Error("gatemcp.Handler.ServeHTTP", "M4 WP-04"))
}
