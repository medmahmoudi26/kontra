// Package gatemcp is the gate's own MCP server at {public_url}/mcp (spec/relay.md §12): four tools (search,
// describe, hire, reply) and the MCP Tasks of the tasks hire creates, in both MCP eras (§12.5, D24, D269).
//
// It is not a tunnelled service and has no upstream. §11.1's transport rules apply (one message per POST, strict
// decoding with internal/mcpwire, Origin, versions) and §11.12's classification of a request's era (rules 1–5):
//
//   - Legacy (2025-11-25, 2025-06-18): initialize opens a session, an mcp_sessions row whose service is NULL and
//     whose tenant is the principal's (internal/mcprelay's Sessions). Nothing is pinned and nothing hops: any replica
//     serves any request of a session. The session's GET stream carries notifications/elicitation/complete (§12.3,
//     D288) and nothing else; it has no event ids and is not resumed.
//   - Modern (2026-07-28): the gate answers server/discover itself, advertising the tasks extension; a hire whose
//     request declares it becomes a task answered as §11.12 step 4, and tasks/get, tasks/update and tasks/cancel
//     serve hire tasks. No virtual session is opened and Admission's session operation is not consulted (D269).
//
// /mcp has no anonymous principal: a request without a credential is 401, with §12.1's challenge when the MCP
// OAuth resource server is configured (internal/oidc, D270, D276), which also makes a JWT for /mcp's canonical URI a
// principal (oidc:sub:<sub>, via mcp). Through a node, the session's principal applies (spec/tunnel.md §9).
//
// hire creates an ordinary A2A gate task (protocol a2a, dispatched as §6) through the A2A service's own handler
// and work queue (Hirer, internal/relay), marked tasks.hired (D272); Admission (operation task) decides first, and
// a quote leads to URL elicitation (-32042, legacy clients that declared elicitation.url) or to the approve URL in
// the answer (D21). The URL is the operator's (Cloud: https://app.kontra.sh/#/t/<taskId>, D188); the gate never
// builds it, and nothing a client sends approves a quote (D38, D45): a reply, a client response to a gate request
// id and a retried hire all leave a quoted task quoted, and the gate never sends a URL-mode elicitation/create.
//
// /mcp writes no calls rows: a hire is accounted as the A2A task it creates (D289).
//
// Work packages: M4 WP-04 (the legacy era, the four tools, -32042, MCP Tasks over hire tasks), M4 WP-05 (the
// 2026-07-28 era), M4 WP-06 (the OAuth resource server in front of it).
package gatemcp

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
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
)

// Path is the route of the gate's own MCP server (spec §2.1).
const Path = "/mcp"

// SessionIdle is how long a legacy session of /mcp lives without a request: §11.2's default session_idle_s (/mcp is no
// service with one of its own).
const SessionIdle = time.Hour

// Versions are the legacy versions /mcp negotiates, newest first (§11.1).
var Versions = []string{mcpwire.V20251125, mcpwire.V20250618}

// Deps are the handler's collaborators, wired by internal/gate.
type Deps struct {
	PublicURL      string
	Multi          bool                // tenancy: multi: refs are <tenant>/<name> (§12.2, D271)
	Auth           *auth.Authenticator // API keys, node principals, and the MCP OAuth resource server's JWTs (§12.1)
	Sessions       *mcprelay.Sessions  // legacy sessions: mcp_sessions rows with service NULL
	Catalog        *Catalog            // search, describe, and the service a hire names
	Hirer          Hirer               // creates and follows up hire tasks (*relay.Hirer)
	Tasks          *HireTasks          // the MCP Tasks of hire tasks, both eras
	Elicitations   *Elicitations       // -32042 and notifications/elicitation/complete (§12.3)
	Log            *store.Log          // the task log hire tasks are read from
	Waker          store.TaskWaker     // the replica's LISTEN connection: plain waits, tasks/result, GET streams
	Render         *blob.Renderer      // kontra-blob: parts as presigned GETs in what /mcp answers; nil renders none
	AllowedOrigins []string            // mcp.allowed_origins (§11.1)
	Logger         *slog.Logger
	// PlainWait is how long a plain hire and a reply wait (PlainWait when zero; tests shorten it).
	PlainWait time.Duration
	// PlainBudget bounds a plain hire's or reply's whole request, from its arrival to its answer (PlainBudget when
	// zero; tests shorten it).
	PlainBudget time.Duration
	now         func() time.Time
}

// Handler serves POST, GET and DELETE on /mcp after the edge and the credential check.
type Handler struct {
	deps Deps
	log  *slog.Logger
}

// NewHandler checks deps.
func NewHandler(deps Deps) (*Handler, error) {
	switch {
	case deps.Auth == nil:
		return nil, errors.New("gatemcp: no authenticator")
	case deps.Sessions == nil || deps.Catalog == nil || deps.Hirer == nil || deps.Tasks == nil || deps.Elicitations == nil:
		return nil, errors.New("gatemcp: the sessions, the catalog, the hirer, the tasks and the elicitations are required")
	case deps.Log == nil || deps.Waker == nil:
		return nil, errors.New("gatemcp: the log and the waker are required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.PlainWait <= 0 {
		deps.PlainWait = PlainWait
	}
	if deps.PlainBudget <= 0 {
		deps.PlainBudget = PlainBudget
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	return &Handler{deps: deps, log: deps.Logger.With("component", "gate-mcp")}, nil
}

// Register mounts POST /mcp as a JSON-RPC route (the edge's 413 and 429 carry a JSON-RPC error) and GET and DELETE
// /mcp as plain routes, on the edge's closed router.
func (h *Handler) Register(rt *edge.Router) {
	rt.HandleJSONRPC("POST "+Path, h)
	rt.Handle("GET "+Path, h)
	rt.Handle("DELETE "+Path, h)
}

// request is one client request as the handler serves it.
type request struct {
	w    http.ResponseWriter
	r    *http.Request
	p    auth.Principal
	body []byte
	msg  *mcpwire.Message // nil for GET and DELETE
	sess *mcprelay.Session
	sid  string
	// modern is a 2026-07-28 request (§11.12 rule 2): no session; every result carries resultType (modern.go).
	modern bool
	// start is when the handler took the request (the edge has read its body): a plain wait's budget counts from it.
	start time.Time
}

func (rq *request) ctx() context.Context { return rq.r.Context() }

func (rq *request) id() json.RawMessage {
	if rq.msg == nil {
		return nil
	}
	return rq.msg.ID
}

// version is the legacy protocol version the request's session negotiated (initialize): it decides what the session
// has (tasks/*, hire's execution, task-augmented calls; D293). A request whose MCP-Protocol-Version names another
// version is refused before (session). A modern request's is 2026-07-28, which CheckModern held equal to its _meta.
func (rq *request) version() string {
	if rq.modern {
		return mcpwire.V20260728
	}
	if rq.sess != nil {
		return rq.sess.ProtocolVersion
	}
	return mcpwire.V20251125
}

// reader is the request's Reader: its principal, its era, and what its session (legacy) or the request itself
// (modern: its per-request clientCapabilities, D274) declared.
func (rq *request) reader() Reader {
	if rq.modern {
		return Reader{Principal: rq.p, Era: mcpwire.EraModern,
			FormElicitation: declaresForm(mcpwire.ClientCapabilities(rq.msg.Params))}
	}
	rd := Reader{Principal: rq.p, Era: mcpwire.EraLegacy}
	if rq.sess != nil {
		rd.FormElicitation = declaresForm(rq.sess.ClientCaps)
		h := rq.sess.Hash
		rd.SessionHash = &h
	}
	return rd
}

// ServeHTTP classifies the request's era (§11.12 rules 1–5) and serves it, in the order of spec §2.1 and §11.1: the
// credential (401; a principal that may not hire, 403), the one-message rule and strict decoding (400), Origin (403),
// the era; then initialize,
// tools/list, tools/call of the four tools, the legacy tasks/* (§11.6 over hire tasks), the client's responses to
// gate requests, the GET stream of a legacy session and its DELETE; a modern request is the 2026-07-28 surface's.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rq := &request{w: w, r: r, start: h.now()}
	post := r.Method == http.MethodPost
	if post && r.Body != nil {
		b, err := io.ReadAll(r.Body) // the edge has read it within limits.body
		if err != nil {
			writeError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "the request body could not be read", nil)
			return
		}
		rq.body = b
	}
	// §12.1 (M4 WP-06): with auth.mcp_oauth, a bearer JWT is verified for /mcp's canonical URI, and a 401 carries the
	// challenge naming /mcp's RFC 9728 document (internal/oidc).
	p, authenticated, err := h.deps.Auth.AuthenticateMCP(r, uuid.Nil, "")
	switch {
	case errors.Is(err, auth.ErrUnauthenticated) || (err == nil && !authenticated):
		// D270: /mcp has no anonymous principal.
		w.Header().Set("WWW-Authenticate", h.deps.Auth.MCPChallenge("", err != nil))
		if post {
			writeError(w, http.StatusUnauthorized, nil, codeUnauthenticated, "unauthenticated", nil)
		} else {
			writeEmpty(w, http.StatusUnauthorized)
		}
		return
	case err != nil:
		h.internal(rq, "authenticate", err)
		return
	case !p.MayHire():
		// Spec §2.3: an API key with only enroll: scopes enrolls nodes and nothing else. The A2A routes refuse it every
		// request (their allow: check and the identity interceptor); /mcp, whose every tool and task surface is hiring,
		// refuses it whole, before a session exists, so no task of the buyer it shares with a hire key is read or
		// cancelled through it (D293).
		if post {
			writeError(w, http.StatusForbidden, nil, mcpwire.CodeInvalidRequest, MsgMayNotHire, nil)
		} else {
			writeEmpty(w, http.StatusForbidden)
		}
		return
	}
	rq.p = p
	if post {
		m, err := mcpwire.Parse(rq.body, mcpwire.FromClient)
		if err != nil {
			h.refuseParse(rq, m, err)
			return
		}
		rq.msg = m
	}
	if o := r.Header.Get("Origin"); o != "" && !h.originAllowed(o) {
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
		rq.modern = true
		h.modern(rq)
		return
	}
	if post && rq.msg.Method == mcpwire.MethodInitialize {
		h.initialize(rq)
		return
	}
	h.session(rq)
}

// now is the handler's clock (Deps.now, tests; else the wall clock).
func (h *Handler) now() time.Time {
	if h.deps.now != nil {
		return h.deps.now()
	}
	return time.Now()
}

// originAllowed reports whether an Origin header is listed in mcp.allowed_origins (scheme and host compared
// without case).
func (h *Handler) originAllowed(o string) bool {
	return slices.ContainsFunc(h.deps.AllowedOrigins, func(a string) bool { return strings.EqualFold(strings.TrimRight(a, "/"), o) })
}

// refuseParse answers a POST body Parse refused (§11.1), as the MCP relay does.
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
		writeError(rq.w, http.StatusBadRequest, id, mcpwire.CodeInvalidRequest, "invalid request", nil)
	}
}

// initialize opens a legacy session (§12, D269): the version the client asked for when /mcp speaks it, else the
// newest; capabilities tools, and on 2025-11-25 tasks (list, cancel, requests.tools.call, §12.5); an mcp_sessions row
// whose service is NULL and whose tenant is the principal's, keeping the client's capabilities (its elicitation
// modes decide -32042 and §12.4's form); the gate's Mcp-Session-Id. No Admission is asked: /mcp is not a service,
// and each hire is admitted as a task (§12.5).
func (h *Handler) initialize(rq *request) {
	m := rq.msg
	asked, _ := stringOf(m.Params, "protocolVersion")
	v := mcpwire.V20251125
	if slices.Contains(Versions, asked) {
		v = asked
	}
	clientCaps, ok := mcpwire.Member(m.Params, "capabilities")
	if !ok || firstByte(clientCaps) != '{' {
		clientCaps = json.RawMessage(`{}`)
	}
	clientID, _, err := mcprelay.MintID()
	if err != nil {
		h.internal(rq, "mint a session id", err)
		return
	}
	err = h.deps.Sessions.OpenGate(rq.ctx(), clientID, mcprelay.Session{
		TenantID: tenantOf(rq.p), BuyerID: rq.p.BuyerID, Principal: rq.p.ID, Via: string(rq.p.Via),
		ProtocolVersion: v, ClientCaps: clientCaps,
	})
	if err != nil {
		h.internal(rq, "open a session", err)
		return
	}
	capabilities := map[string]any{"tools": map[string]any{}}
	if v == mcpwire.V20251125 {
		capabilities["tasks"] = map[string]any{"list": map[string]any{}, "cancel": map[string]any{},
			"requests": map[string]any{"tools": map[string]any{"call": map[string]any{}}}}
	}
	res, err := json.Marshal(map[string]any{
		"protocolVersion": v,
		"capabilities":    capabilities,
		"serverInfo":      serverInfo(),
		"instructions":    instructions,
	})
	if err != nil {
		h.internal(rq, "write the initialize result", err)
		return
	}
	h.log.Info("mcp session opened on /mcp", "principal", rq.p.ID, "protocol_version", v)
	writeResult(rq.w, clientID, m.ID, res)
}

// session serves every legacy request but initialize: on the session its Mcp-Session-Id names, for this principal
// only (404 otherwise: unknown, closed, or another principal's).
func (h *Handler) session(rq *request) {
	sid := rq.r.Header.Get(mcpwire.HeaderSessionID)
	sess, err := h.deps.Sessions.Lookup(rq.ctx(), sid, tenantOf(rq.p), "", rq.p.ID)
	switch {
	case errors.Is(err, mcprelay.ErrNoSession):
		if rq.r.Method == http.MethodPost {
			writeError(rq.w, http.StatusNotFound, rq.id(), codeNoSession, msgNoSession, nil)
		} else {
			writeEmpty(rq.w, http.StatusNotFound)
		}
		return
	case err != nil:
		h.internal(rq, "look up the session", err)
		return
	}
	if v := rq.r.Header.Get(mcpwire.HeaderProtocolVersion); v != "" && v != sess.ProtocolVersion {
		// MCP 2025-11-25 transports: a request carries the version the session negotiated, and a server answers an
		// invalid or unsupported one 400. Classify has refused a version /mcp does not speak; this is one the session did
		// not negotiate (a 2025-06-18 session naming 2025-11-25 would otherwise get tasks, D293). Both values are
		// among Versions, so the answer names them.
		writeError(rq.w, http.StatusBadRequest, rq.id(), mcpwire.CodeUnsupportedVersion, "Unsupported protocol version",
			map[string]any{"supported": []string{sess.ProtocolVersion}, "requested": v})
		return
	}
	rq.sess, rq.sid = sess, sid
	if now := h.deps.now(); now.Sub(sess.LastSeenAt) >= mcprelay.IdleWrite {
		if err := h.deps.Sessions.Touch(rq.ctx(), sess.Hash, now); err != nil {
			h.log.Debug("mcp: last_seen_at not written", "error", err)
		}
	}
	switch rq.r.Method {
	case http.MethodGet:
		h.deps.Elicitations.Stream(rq.ctx(), rq.w, sid, sess)
		return
	case http.MethodDelete:
		if _, err := h.deps.Sessions.Close(rq.ctx(), sess.Hash, mcprelay.CloseDeleted); err != nil {
			h.internal(rq, "close the session", err)
			return
		}
		writeEmpty(rq.w, http.StatusOK)
		return
	}
	m := rq.msg
	switch {
	case m.Method == "" && m.ID != nil:
		// A client response: one to a gate request id (an elicitation/create tasks/result delivered, §12.4) is the
		// gate's to consume once; any other is dropped. Nothing a client answers ever approves a quote.
		h.deps.Tasks.Respond(rq.ctx(), rq.reader(), m)
		writeEmpty(rq.w, http.StatusAccepted)
		return
	case len(m.ID) == 0:
		// A notification (notifications/initialized, cancelled, progress …): nothing to do.
		writeEmpty(rq.w, http.StatusAccepted)
		return
	}
	switch m.Method {
	case "ping":
		writeResult(rq.w, rq.sid, m.ID, json.RawMessage(`{}`))
	case mcpwire.MethodToolsList:
		res, err := ToolsList(mcpwire.EraLegacy, rq.version())
		if err != nil {
			h.internal(rq, "list the tools", err)
			return
		}
		writeResult(rq.w, rq.sid, m.ID, res)
	case mcpwire.MethodToolsCall:
		h.toolsCall(rq)
	case "tasks/get", "tasks/result", "tasks/list", "tasks/cancel":
		if rq.version() != mcpwire.V20251125 {
			// §11.1: a 2025-06-18 session has no tasks.
			writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
			return
		}
		h.tasks(rq)
	default:
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
	}
}

// toolsCall serves tools/call of the four tools; any other name is -32602 "Unknown tool: <name>" (§11.3's answer).
// A task-augmented call (params.task, 2025-11-25 sessions) is hire's only: the other tools are -32601, as for a
// tool whose execution.taskSupport is absent (§11.5 step 0). A modern request has no per-request opt-in: a hire
// whose request declares the tasks extension may become a task, the gate deciding (§11.12, D159, D269), and every
// other tool call, and a modern params.task (a 2025-11-25 field), is a plain call.
func (h *Handler) toolsCall(rq *request) {
	m := rq.msg
	name, _ := stringOf(m.Params, "name")
	args, _ := mcpwire.Member(m.Params, "arguments")
	_, augmented := mcpwire.Member(m.Params, "task")
	augmented = augmented && rq.version() == mcpwire.V20251125
	if rq.modern {
		augmented = name == ToolHire && mcpwire.DeclaresTasks(m.Params)
	}
	switch name {
	case ToolSearch, ToolDescribe, ToolHire, ToolReply:
	default:
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeInvalidParams, "Unknown tool: "+name, nil)
		return
	}
	if augmented && name != ToolHire {
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "Tool "+name+" does not support task-augmented execution", nil)
		return
	}
	switch name {
	case ToolSearch:
		h.search(rq, args)
	case ToolDescribe:
		h.describeTool(rq, args)
	case ToolHire:
		h.hire(rq, args, augmented)
	case ToolReply:
		h.reply(rq, args)
	}
}

// search answers the search tool: structuredContent and the same as a text block (§12.2).
func (h *Handler) search(rq *request, args json.RawMessage) {
	a, err := parseSearchArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	res, err := h.deps.Catalog.Search(rq.ctx(), rq.p, a)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	text, _ := json.Marshal(res)
	rq.result(textResult(string(text), res, nil))
}

// describeTool answers the describe tool (§12.2).
func (h *Handler) describeTool(rq *request, args json.RawMessage) {
	a, err := parseDescribeArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	ref, err := ParseRef(a.Service, h.deps.Multi)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	res, err := h.deps.Catalog.Describe(rq.ctx(), rq.p, ref)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	text, _ := json.Marshal(res)
	rq.result(textResult(string(text), res, nil))
}

// toolError answers a tool call whose execution failed: invalid arguments and an unknown service are tool execution
// errors (isError, MCP 2025-11-25); any other error is the gate's own (503, -32603).
//
// SPEC-CONFLICT (relay §12.2 "else the unknown-service error", §2.1, D50): the unknown-service error is a route's
// answer, HTTP 404 with -32601 "unknown service"; on /mcp a tool call names the service in its arguments, and a 404
// to a POST carrying Mcp-Session-Id tells a Streamable HTTP client its session is gone (it would initialize again).
// The closest faithful behaviour: the same one answer for a ref that is malformed, unknown, disabled, of an ended
// tenant or not admitted, as a CallToolResult with isError and the text "unknown service" (MCP 2025-11-25 answers an
// input validation error as a tool execution error). Proposal: §12.2 says so (D293).
func (h *Handler) toolError(rq *request, err error) {
	var ae *argError
	switch {
	case errors.As(err, &ae):
		rq.result(errorResult(ae.Error(), nil))
	case errors.Is(err, ErrUnknownService):
		rq.result(errorResult(UnknownService, nil))
	default:
		h.internal(rq, "serve a tool call", err)
	}
}

// UnknownService is the text of the unknown-service answer (§2.1, D50): the same for a ref that is malformed,
// unknown, disabled, of an ended tenant or not admitted.
const UnknownService = "unknown service"

// internal is a failure of the gate's own (Postgres, the registry): 503 with -32603. The cause is logged, never
// sent.
func (h *Handler) internal(rq *request, what string, err error) {
	h.log.Warn("mcp: could not "+what, "error", err)
	writeError(rq.w, http.StatusServiceUnavailable, rq.id(), mcpwire.CodeInternal, "the gate could not serve the request", nil)
}

// MsgMayNotHire is the 403 answer to a principal that may not call services (an API key without the hire scope),
// the A2A identity interceptor's words.
const MsgMayNotHire = "this API key may not call services (it has no hire scope)"

// Messages and codes of the gate's own answers.
const (
	codeUnauthenticated = -32001
	codeNoSession       = -32001 // what MCP servers answer for an unknown session (the TypeScript SDK's)
	msgNoSession        = "Session not found"
)

// declaresForm reports whether a client's capabilities declare form elicitation (MCP 2025-11-25: elicitation.form,
// or an elicitation object with neither form nor url, which means form).
func declaresForm(caps json.RawMessage) bool {
	e, ok := capsMember(caps, "elicitation")
	if !ok {
		return false
	}
	if _, err := strictjson.Members(e); err != nil {
		return false // an ambiguous object declares nothing
	}
	_, form := capsMember(e, "form")
	_, url := capsMember(e, "url")
	return form || !url
}

// declaresURL reports whether a client's capabilities declare URL elicitation (elicitation.url, 2025-11-25).
func declaresURL(caps json.RawMessage) bool {
	e, ok := capsMember(caps, "elicitation")
	if !ok {
		return false
	}
	_, url := capsMember(e, "url")
	return url
}

// capsMember reads an object member of a capabilities object with exact keys; a member that is not an object (or
// an ambiguous object) is absent.
func capsMember(obj json.RawMessage, key string) (json.RawMessage, bool) {
	m, err := strictjson.Members(obj)
	if err != nil {
		return nil, false
	}
	v, ok := m[key]
	if !ok || firstByte(v) != '{' {
		return nil, false
	}
	return v, true
}

// stringOf reads a string member of an object.
func stringOf(obj json.RawMessage, key string) (string, bool) {
	v, ok := mcpwire.Member(obj, key)
	if !ok || firstByte(v) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

func firstByte(raw []byte) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c
	}
	return 0
}

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

// result writes the request's JSON-RPC result answer, status 200: with the gate's session id (legacy), or with
// resultType "complete" unless res has one (modern: 2026-07-28 gives every result one; a CreateTaskResult's is
// "task").
func (rq *request) result(res json.RawMessage) {
	writeResult(rq.w, rq.sid, rq.msg.ID, rq.complete(res))
}

// complete is res as the request's era writes it: unchanged for a legacy request; for a modern one, with resultType
// "complete" when it has no resultType (2026-07-28: "All results now carry a required resultType field").
func (rq *request) complete(res json.RawMessage) json.RawMessage {
	if !rq.modern {
		return res
	}
	return withResultType(res, resultComplete)
}

// writeResult writes a JSON-RPC result answer with status 200 and the gate's session id.
func writeResult(w http.ResponseWriter, sid string, id json.RawMessage, res json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	if sid != "" {
		w.Header().Set(mcpwire.HeaderSessionID, sid)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBytes(id, "result", res))
}

// responseBytes is a JSON-RPC response with member ("result" or "error") raw.
func responseBytes(id json.RawMessage, member string, raw json.RawMessage) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b := make([]byte, 0, len(raw)+64)
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, id...)
	b = append(b, `,"`+member+`":`...)
	b = append(b, raw...)
	return append(b, '}')
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
