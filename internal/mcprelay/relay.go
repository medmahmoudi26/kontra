// Package mcprelay serves /mcp/{svc} (spec/relay.md §11): each kind: mcp service as a Streamable HTTP MCP
// endpoint, revisions 2025-11-25 and 2025-06-18 ("legacy", sessions) and 2026-07-28 ("modern", per-request
// _meta) on the same endpoint (§11.12, D24, D158), through a tunnel session or a direct mcp: {url} backend
// (D170).
//
// Two request shapes, two primitives (note 1 §5):
//
//   - Plain requests (initialize, tools/list, tools/call, resources/*, prompts/*, server/discover,
//     subscriptions/listen, notifications, responses): authentication, allow:, per-tool policy (Policy,
//     ToolsCache), Admission for session and call, metering (Meter, the calls table), and for legacy clients
//     session affinity (Sessions, mcp_sessions) with the peer hop to the replica holding a tunnel session
//     (internal/peer, D163). No durability: a pinned session dies with its tunnel.
//   - Gate-owned tasks: a legacy task-augmented tools/call (params.task) on a durable service (§11.5), or a
//     modern tools/call that declares the tasks extension on a durable service whose upstream declares it
//     (§11.12, D159). The gate creates the gate task (protocol mcp), starts TaskLifecycle, and serves tasks/*
//     from Postgres (tasks.go); the dispatch activity (Dispatcher) talks to the upstream in the creating
//     request's era.
//
// The relay reads and writes raw JSON (internal/mcpwire), decoded with exact keys (§11.1, D172): a client
// message with a repeated or case-variant key at a level it reads is 400 with -32600, and an upstream's is not
// relayed. It never parses arguments, content or structuredContent, which may be sealed (spec/ext-sealed.md
// §2), and metering reads only the tool name, byte counts, duration and the top-level isError (§11.4).
//
// Work packages: M3 WP-04 (plain, legacy, sessions, policy, metering, peer hop, direct and tunnel backends),
// M3 WP-06 (gate-owned tasks, 2025-11-25), M3 WP-07 (2026-07-28).
package mcprelay

import (
	"log/slog"
	"net/http"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/peer"
	"kontra.sh/kontra/internal/unimpl"
)

// Deps are the handler's collaborators, wired by internal/gate.
type Deps struct {
	ReplicaID      string
	Sessions       *Sessions
	Tools          *ToolsCache
	Meter          *Meter
	Admission      hooks.Admission
	Peer           *peer.Client // nil without peer.listen: only this replica's sessions are candidates
	AllowedOrigins []string     // mcp.allowed_origins (§11.1)
	Log            *slog.Logger
}

// Handler serves POST, GET and DELETE on /mcp/{name} (and /mcp/{tenant}/{name} on a multi-tenant gate) after
// the edge and the credential check. An unknown service, one of the other kind, a disabled one, or one whose
// allow: does not admit the principal is the route's 404 (spec §2.1, D50).
type Handler struct {
	deps Deps
}

// NewHandler checks deps.
func NewHandler(deps Deps) (*Handler, error) {
	return nil, unimpl.Error("mcprelay.NewHandler", "M3 WP-04")
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, unimpl.Error("mcprelay.Handler", "M3 WP-04").Error(), http.StatusNotImplemented)
}
