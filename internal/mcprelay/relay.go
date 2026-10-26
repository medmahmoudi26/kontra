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
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/peer"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// Service is a kind: mcp service as the relay serves it (internal/gate's registry builds it from its row).
type Service struct {
	Ref      store.ServiceRef
	Allow    auth.AllowList
	Disabled bool
	// Direct is the backend of a direct mcp: {url} service (the SSRF-safe dialer pinned to the URL's host:port,
	// D71), nil for a tunnel service. A direct backend is reached from any replica and never hops (§11.2).
	Direct      upstream.Backend
	Tools       Policy
	Tasks       string // durable, passthrough or off (§11.5)
	Sealed      bool   // sealed: set (spec/ext-sealed.md §1; served from M3 WP-08)
	SessionIdle time.Duration
	// ToolMeta and InitializeMeta are services.annotations' tool_meta and initialize_meta (§13.4).
	ToolMeta       map[string]map[string]json.RawMessage
	InitializeMeta map[string]json.RawMessage
}

// Services is the service registry as the relay reads it.
type Services interface {
	// MCPService returns the tenant's service of that name when it is an MCP service, disabled ones included
	// (the route answers them as unknown); nil when there is none, or it is of the other kind.
	MCPService(ctx context.Context, tenant, name string) (*Service, error)
	// MCPServiceByID is MCPService by the tenant's id.
	MCPServiceByID(ctx context.Context, tenantID uuid.UUID, name string) (*Service, error)
}

// Tunnels is this replica's tunnel registry (*tunnelgate.Registry).
type Tunnels interface {
	MCPBackend(session, tenantID uuid.UUID, service string) (upstream.Backend, error)
}

// Picker is the session choice of a new MCP session (*tunnelgate.Picker).
type Picker interface {
	PickMCP(ctx context.Context, tenantID uuid.UUID, service, local string, remote bool, avoid ...uuid.UUID) (tunnelgate.MCPPick, error)
}

// Deps are the handler's collaborators, wired by internal/gate.
type Deps struct {
	ReplicaID string
	Services  Services
	Auth      *auth.Authenticator
	Multi     bool // /mcp/{tenant}/{name}
	Sessions  *Sessions
	Tools     *ToolsCache
	Meter     *Meter
	Admission hooks.Admission
	// AdmissionOps are the operations that consult Admission (hooks.admission.operations, §13.1): session at
	// initialize, call at a plain tools/call, task at a passthrough task-augmented one. None consults the built-in,
	// which allows.
	AdmissionOps   []hooks.Operation
	Tunnels        Tunnels
	Picker         Picker
	Peer           *peer.Client // nil without peer.listen: only this replica's sessions are candidates
	AllowedOrigins []string     // mcp.allowed_origins (§11.1)
	Log            *slog.Logger
	// Tasks serves gate-owned MCP tasks (M3 WP-06); nil until it is built, and a durable service then advertises
	// no tasks (as tasks: off does) and answers a task-augmented call and tasks/* -32601.
	Tasks TaskServer
	now   func() time.Time
}

// TaskServer is the gate-owned task side of a durable service (M3 WP-06's Tasks).
type TaskServer interface {
	Serve(ctx context.Context, era mcpwire.Era, buyer uuid.UUID, m *mcpwire.Message) (json.RawMessage, *mcpwire.Error, error)
}

// Handler serves POST, GET and DELETE on /mcp/{name} (and /mcp/{tenant}/{name} on a multi-tenant gate) after
// the edge and the credential check. An unknown service, one of the other kind, a disabled one, or one whose
// allow: does not admit the principal is the route's 404 (spec §2.1, D50).
type Handler struct {
	deps Deps
	log  *slog.Logger
}

// NewHandler checks deps.
func NewHandler(deps Deps) (*Handler, error) {
	switch {
	case deps.Services == nil:
		return nil, errors.New("mcprelay: no service registry")
	case deps.Auth == nil:
		return nil, errors.New("mcprelay: no authenticator")
	case deps.Sessions == nil || deps.Tools == nil || deps.Meter == nil:
		return nil, errors.New("mcprelay: the sessions, the tools cache and the meter are required")
	case deps.ReplicaID == "":
		return nil, errors.New("mcprelay: no replica id")
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Admission == nil {
		deps.Admission = hooks.BuiltinAdmission{}
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	return &Handler{deps: deps, log: deps.Log.With("component", "mcp")}, nil
}

// Register mounts the routes on the edge's closed router: POST as a JSON-RPC route (the edge's 413 and 429 carry a
// JSON-RPC error), GET and DELETE as plain ones. The gate's own /mcp is M4's (D170) and stays unrouted.
func (h *Handler) Register(rt *edge.Router) {
	p := "/mcp/{name}"
	if h.deps.Multi {
		p = "/mcp/{tenant}/{name}"
	}
	rt.HandleJSONRPC("POST "+p, h)
	rt.Handle("GET "+p, h)
	rt.Handle("DELETE "+p, h)
}

// request is one client request as the relay serves it.
type request struct {
	w     http.ResponseWriter
	r     *http.Request
	svc   *Service
	p     auth.Principal
	body  []byte
	msg   *mcpwire.Message // nil for GET and DELETE
	start time.Time
	sess  *Session // the pinned session
	sid   string   // the client's gate Mcp-Session-Id
}

func (rq *request) ctx() context.Context { return rq.r.Context() }

func (rq *request) id() json.RawMessage {
	if rq.msg == nil {
		return nil
	}
	return rq.msg.ID
}

// ServeHTTP implements http.Handler, in the order of spec §2.1 and §11.1: the credential (401), the service and
// allow: (404), the one-message rule and strict decoding (400), Origin (403), then §11.12's classification.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rq := &request{w: w, r: r, start: h.deps.now()}
	post := r.Method == http.MethodPost
	if post && r.Body != nil {
		b, err := io.ReadAll(r.Body) // the edge has read it within limits.body
		if err != nil {
			writeError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "the request body could not be read", nil)
			return
		}
		rq.body = b
	}

	p, authenticated, err := h.deps.Auth.Authenticate(r, uuid.Nil)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		if post {
			writeError(w, http.StatusUnauthorized, nil, codeUnauthenticated, "unauthenticated", nil)
		} else {
			writeEmpty(w, http.StatusUnauthorized)
		}
		return
	case err != nil:
		h.internal(rq, "authenticate", err)
		return
	}
	svc, err := h.lookup(r)
	if err != nil {
		h.internal(rq, "look up the service", err)
		return
	}
	if svc == nil || svc.Disabled || svc.Ref.Kind != config.KindMCP {
		h.unknownService(rq)
		return
	}
	if !authenticated {
		buyerTenant := uuid.Nil // a team gate: one fixed buyer for every anonymous caller (spec §2.3)
		if h.deps.Multi {
			buyerTenant = svc.Ref.TenantID
		}
		buyer, err := ids.AnonymousBuyer(buyerTenant)
		if err != nil {
			h.internal(rq, "derive the anonymous buyer", err)
			return
		}
		p = auth.Anonymous(buyer)
	}
	if !svc.Allow.Admits(p) {
		h.unknownService(rq)
		return
	}
	rq.svc, rq.p = svc, p

	if post {
		m, err := mcpwire.Parse(rq.body, mcpwire.FromClient)
		if err != nil {
			h.refuseParse(rq, m, err)
			return
		}
		rq.msg = m
	}
	if o := r.Header.Get("Origin"); o != "" && !h.originAllowed(o) {
		// §11.1: an Origin not listed is 403 (the transport's body: a JSON-RPC error without an id).
		if post {
			writeError(w, http.StatusForbidden, nil, mcpwire.CodeInvalidRequest, "origin not allowed", nil)
		} else {
			writeEmpty(w, http.StatusForbidden)
		}
		return
	}
	era, refusal := mcpwire.Classify(r, rq.msg)
	if refusal != nil {
		writeRefusal(w, refusal)
		return
	}
	if era == mcpwire.EraModern {
		h.modern(rq)
		return
	}
	if post && rq.msg.Method == mcpwire.MethodInitialize {
		h.initialize(rq)
		return
	}
	h.pinned(rq)
}

// modern answers a 2026-07-28 request. M3 WP-07 serves them (§11.12); until then the gate answers as a legacy-only
// server does, rule 3's 400 with the -32000 body, so a dual-era client falls back to initialize, never forwarding
// the request (a legacy upstream would spend a server on it, D177).
func (h *Handler) modern(rq *request) {
	writeRefusal(rq.w, &mcpwire.Refusal{Status: http.StatusBadRequest, Body: mcpwire.LegacyNoSessionBody})
}

// lookup is the route's service: /mcp/{name}, or /mcp/{tenant}/{name} on a multi-tenant gate.
func (h *Handler) lookup(r *http.Request) (*Service, error) {
	name, tn := r.PathValue("name"), tenant.TeamSlug
	if h.deps.Multi {
		tn = r.PathValue("tenant")
	}
	if !config.NameRE.MatchString(name) || !config.NameRE.MatchString(tn) {
		return nil, nil
	}
	return h.deps.Services.MCPService(r.Context(), tn, name)
}

// originAllowed reports whether an Origin header is listed in mcp.allowed_origins (scheme and host compared
// without case).
func (h *Handler) originAllowed(o string) bool {
	return slices.ContainsFunc(h.deps.AllowedOrigins, func(a string) bool { return strings.EqualFold(strings.TrimRight(a, "/"), o) })
}

// refuseParse answers a POST body Parse refused (§11.1): a batch or any non-object value is 400 with an id-less
// -32600 "batch not supported"; bytes that are not JSON are 400 with -32700 and id null; an ambiguous or invalid
// message is 400 with -32600 and the id when it was read unambiguously. Nothing is forwarded, and policy, Admission
// and metering never see it (mcp_batch_refused, mcp_case_smuggling).
func (h *Handler) refuseParse(rq *request, m *mcpwire.Message, err error) {
	switch {
	case errors.Is(err, mcpwire.ErrBatch):
		writeError(rq.w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "batch not supported", nil)
	case errors.Is(err, mcpwire.ErrParse):
		writeError(rq.w, http.StatusBadRequest, nil, mcpwire.CodeParse, "parse error", nil)
	default:
		var id json.RawMessage
		if m != nil {
			id = m.ID
		}
		h.log.Info("an MCP request was refused by strict decoding", "service", rq.svc.Ref.Name, "level", strictLevel(err))
		writeError(rq.w, http.StatusBadRequest, id, mcpwire.CodeInvalidRequest, "invalid request", nil)
	}
}

// strictLevel names the level a strict decoding refusal found for the log: the path, never the keys or values
// (an argument's key is the caller's; §11.4: arguments never appear in the logs).
func strictLevel(err error) string {
	var ke *strictjson.KeyError
	switch {
	case errors.As(err, &ke) && len(ke.Path) == 0:
		return "the message object"
	case errors.As(err, &ke):
		return strings.Join(ke.Path, ".")
	case errors.Is(err, mcpwire.ErrInvalid):
		return "not a JSON-RPC 2.0 message"
	}
	return "unknown"
}

// unknownService is the one answer for a path naming no service, a service of the other kind, a disabled one, or
// one whose allow: does not admit the principal: 404 with -32601 "unknown service" on a POST, empty on GET and
// DELETE (spec §2.1, D50).
func (h *Handler) unknownService(rq *request) {
	if rq.r.Method == http.MethodPost {
		writeError(rq.w, http.StatusNotFound, nil, mcpwire.CodeMethodNotFound, UnknownService, nil)
		return
	}
	writeEmpty(rq.w, http.StatusNotFound)
}

// internal is a failure of the gate's own (Postgres, the registry): 503 with -32603. The cause is logged, never
// sent.
func (h *Handler) internal(rq *request, what string, err error) {
	h.log.Warn("mcp: could not "+what, "path", rq.r.URL.Path, "error", err)
	writeError(rq.w, http.StatusServiceUnavailable, rq.id(), mcpwire.CodeInternal, "the gate could not serve the request", nil)
}

// UnknownService is the message of the unknown-service answer (spec §2.1, D50).
const UnknownService = "unknown service"

// Messages and codes of the gate's own answers.
const (
	codeUnauthenticated = -32001
	codeNoSession       = -32001 // what MCP servers answer for an unknown session (the TypeScript SDK's)
	msgNoSession        = "Session not found"
	msgUpstreamError    = "upstream error"
	msgUpstreamRefused  = "upstream refused"
	msgUnreachable      = "upstream unreachable"
)

// writeError writes a JSON-RPC error response with status.
func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	b, err := mcpwire.ErrorBody(id, code, msg, data)
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, code, msg, nil)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeRefusal writes a refusal mcpwire made: its body as JSON, or nothing.
func writeRefusal(w http.ResponseWriter, r *mcpwire.Refusal) {
	if len(r.Body) == 0 {
		writeEmpty(w, r.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}

func writeEmpty(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}
