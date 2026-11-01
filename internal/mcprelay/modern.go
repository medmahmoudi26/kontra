package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/upstream"
)

// The 2026-07-28 era (spec/relay.md §11.12, D24, D158–D162, D176–D180; M3 WP-07). A modern request names no
// session: each one goes to a session chosen as for an initialize (this replica's sessions first, else the peer
// hop) or to the direct backend, with MCP-Protocol-Version, Mcp-Method, Mcp-Name and the Mcp-Param-* headers and
// the body changed only by §2.5 and §11.12's rewrites. The gate never translates between eras: an upstream that does
// not speak 2026-07-28 answers with its own 400, 404 or 405, which is relayed to that client, who falls back to
// initialize. A sealed service, and one the gate learned is legacy-only (era.go), get rule 3's 400 at the gate.

// modernInlineWait is how long a gate-owned modern task is waited for before the call is answered with a
// CreateTaskResult (§11.12 step 4, D159).
const modernInlineWait = 2 * time.Second

// admitHold is how long a principal's allowed Admission session decision is reused on this replica while its
// virtual session is not open yet (the requests in flight before the first answer opens it).
const admitHold = time.Minute

// admitFlightTimeout bounds one shared Admission session decision, which runs on a context of its own (the request
// that started it may leave); the hook's own timeout (hooks.admission.timeout, 5 s by default) ends it first.
const admitFlightTimeout = 30 * time.Second

// legacyRefusal is §11.12 rule 3's answer, given at the gate to a modern request for a sealed service (D162) or one
// whose upstream is legacy-only (D177): what a legacy server answers, so a dual-era client falls back to initialize.
var legacyRefusal = &mcpwire.Refusal{Status: http.StatusBadRequest, Body: mcpwire.LegacyNoSessionBody}

// modern serves a 2026-07-28 POST (§11.12), after §11.1's one-message rule, strict decoding, Origin and the
// classification. A sealed service (never probed, D162) and one the gate knows to be legacy-only (D177) answer every
// modern request with rule 3's 400, as a legacy server does, whatever it carries; then CheckModern's refusals (a
// response 400, a notification 202, the _meta and header checks); the tasks extension's client side, from Postgres
// on any replica, before anything that needs the upstream (touching the principal's virtual session, a request like
// any other); subscriptions/listen's -32021; then the service's era
// (the gate's own probe when it is not known; a legacy-only one's 400 at the gate), Admission session for a
// principal with no open virtual session (D160, D177), and the method.
func (h *Handler) modern(rq *request) {
	m := rq.msg
	if rq.svc.Sealed {
		writeRefusal(rq.w, legacyRefusal)
		return
	}
	if era, known, _ := h.deps.Tools.Era(rq.ctx(), h.toolsKey(rq), nil); known && era == mcpwire.EraLegacy {
		writeRefusal(rq.w, legacyRefusal)
		return
	}
	if ref := mcpwire.CheckModern(rq.r.Header, m); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	switch m.Method {
	case "tasks/result", "tasks/list":
		// The extension removes them (§11.12): an unknown method is 404 with -32601 (2026-07-28 Streamable HTTP).
		writeError(rq.w, http.StatusNotFound, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
		return
	case "tasks/get", "tasks/update", "tasks/cancel":
		h.touchVirtual(rq)
		h.modernTaskRequest(rq)
		return
	case mcpwire.MethodListen:
		if mcpwire.ListensForTasks(m.Params) && !mcpwire.DeclaresTasks(m.Params) {
			writeRefusal(rq.w, mcpwire.MissingTasks(m.ID)) // D176
			return
		}
	}
	era, known, err := h.era(rq)
	if err != nil {
		return // the client left while the probe ran
	}
	if known && era == mcpwire.EraLegacy {
		writeRefusal(rq.w, legacyRefusal)
		return
	}
	if ref := h.virtualAdmit(rq); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	switch m.Method {
	case mcpwire.MethodToolsList:
		h.modernToolsList(rq)
	case mcpwire.MethodToolsCall:
		h.modernToolsCall(rq)
	default:
		// An anonymous tenant's slot while the request is relayed, or a stream's slot for as long as a
		// subscriptions/listen stream is open (§6.2, D304, D308).
		take := h.takeRelay
		if m.Method == mcpwire.MethodListen {
			take = h.takeStream
		}
		release, ok := take(rq)
		if !ok {
			return
		}
		defer release()
		if m.Method == mcpwire.MethodServerDiscover {
			h.modernForward(rq, nil, h.discoverResult(rq))
			return
		}
		h.modernForward(rq, nil, nil)
	}
}

// --- virtual sessions (D160, D177) ---

// vkey names a principal's virtual session at a service.
type vkey struct {
	tenant    uuid.UUID
	service   string
	principal string
}

// vflight is one Admission session decision for a vkey on this replica (a singleflight): the first request starts
// it, every request of the principal in flight (the first included) waits for its answer, and an allow is reused for
// admitHold while no virtual session is open. ref and at are set before done closes.
type vflight struct {
	done chan struct{}
	ref  *mcpwire.Refusal // nil: allowed
	at   time.Time
}

// reusable reports whether f answers a request at now: in flight, or an allow younger than admitHold.
func (f *vflight) reusable(now time.Time) bool {
	select {
	case <-f.done:
		return f.ref == nil && now.Sub(f.at) < admitHold
	default:
		return true
	}
}

// virtuals are the replica's Admission session flights.
type virtuals struct {
	mu      sync.Mutex
	flights map[vkey]*vflight
	swept   time.Time // the last sweep
}

// sweep drops the flights that answer nobody any more (refused, or allowed longer than admitHold ago: a principal
// whose requests never got a modern answer, so that openVirtual never dropped its flight), at most once per
// admitHold; v.mu is held.
func (v *virtuals) sweep(now time.Time) {
	if now.Sub(v.swept) < admitHold {
		return
	}
	v.swept = now
	for k, f := range v.flights {
		if !f.reusable(now) {
			delete(v.flights, k)
		}
	}
}

// virtualAdmit is what a modern request needs before it is forwarded (§11.12 "per_session", D177): the principal's
// open virtual session at the service, read (and touched at most once a minute); without one, Admission's session
// decision when hooks.admission.operations lists session (none consults the built-in, which allows), one call per
// principal and service at a time on this replica. A refusal is the request's answer; the virtual session itself is
// opened once the upstream answered with a modern result (openVirtual).
//
// The shared decision runs on a context of its own, bounded by admitFlightTimeout (as the era probe does): the
// request that started it leaving does not end it, so no other request of the principal is refused for it. A request
// whose own client leaves while it waits stops waiting.
func (h *Handler) virtualAdmit(rq *request) *mcpwire.Refusal {
	s, err := h.deps.Sessions.OpenVirtual(rq.ctx(), rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID, h.deps.now())
	switch {
	case err == nil:
		rq.vsess = s
		return nil
	case !errors.Is(err, ErrNoSession):
		h.log.Warn("mcp: a virtual session could not be read", "service", rq.svc.Ref.Name, "error", err)
		return refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "the gate could not serve the request")
	case !slices.Contains(h.deps.AdmissionOps, hooks.OpSession):
		return nil
	}
	k := vkey{rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID}
	now := h.deps.now()
	h.virtuals.mu.Lock()
	if h.virtuals.flights == nil {
		h.virtuals.flights = map[vkey]*vflight{}
	}
	f := h.virtuals.flights[k]
	if f == nil || !f.reusable(now) {
		h.virtuals.sweep(now)
		f = &vflight{done: make(chan struct{})}
		h.virtuals.flights[k] = f
		go h.decideSession(context.WithoutCancel(rq.ctx()), rq, k, f)
	}
	h.virtuals.mu.Unlock()
	select {
	case <-f.done:
	case <-rq.ctx().Done():
		return refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
	}
	return withID(f.ref, rq.msg.ID)
}

// decideSession is flight f's Admission session decision for the request that started it, on detached (its context
// without its cancellation) bounded by admitFlightTimeout; a refusal leaves the flights at once (the next request
// asks again).
func (h *Handler) decideSession(detached context.Context, rq *request, k vkey, f *vflight) {
	ctx, cancel := context.WithTimeout(detached, admitFlightTimeout)
	defer cancel()
	ref := h.admitCtx(ctx, rq, hooks.OpSession, "")
	h.virtuals.mu.Lock()
	f.ref, f.at = ref, h.deps.now()
	if ref != nil && h.virtuals.flights[k] == f {
		delete(h.virtuals.flights, k)
	}
	h.virtuals.mu.Unlock()
	close(f.done)
}

// touchVirtual is what a request served without virtualAdmit (tasks/get, tasks/update, tasks/cancel: from Postgres,
// whatever the upstream) does to the principal's virtual session: it is a request, so the session's last_seen_at is
// written (at most once a minute), and a client polling its gate-owned task keeps its session from closing idle
// (§11.12 "per_session": closed idle after session_idle_s without a request). A principal with none opens none here;
// a session that cannot be read is logged and the request served all the same.
func (h *Handler) touchVirtual(rq *request) {
	_, err := h.deps.Sessions.OpenVirtual(rq.ctx(), rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID, h.deps.now())
	if err != nil && !errors.Is(err, ErrNoSession) && rq.ctx().Err() == nil {
		h.log.Warn("mcp: a virtual session was not touched", "service", rq.svc.Ref.Name, "error", err)
	}
}

// withID is a shared refusal answering another request: its JSON-RPC id replaced.
func withID(ref *mcpwire.Refusal, id json.RawMessage) *mcpwire.Refusal {
	if ref == nil || len(ref.Body) == 0 {
		return ref
	}
	if len(bytes.TrimSpace(id)) == 0 {
		id = json.RawMessage("null")
	}
	b, err := mcpwire.SetMember(ref.Body, "id", id)
	if err != nil {
		return ref
	}
	return &mcpwire.Refusal{Status: ref.Status, Body: b}
}

// openVirtual opens the principal's virtual session at the service once the upstream answered a modern request with
// a modern result (D177), or reads the one a concurrent first request opened (Sessions.Virtual: one open row per
// principal and service). It is the session the request's calls row names. A failure is logged: the answer is
// relayed all the same, and the next request opens it.
//
// SPEC-CONFLICT (D204; relay §11.12 "per_session", D177: the row opens "once the upstream has answered that request
// with a modern result"): two modern requests are answered without the upstream answering them, a tools/list served
// from the modern cache (§11.3) and a tools/call that becomes a gate-owned task (whose upstream call is the
// dispatch's, later). The closest faithful behaviour: both open the virtual session too, since the service is known
// modern from its upstream's own modern answers (the cached list, the server/discover result that declared the tasks
// extension), which is what the rule guards (a probe ending in the legacy 400 still opens nothing).
func (h *Handler) openVirtual(rq *request) {
	if rq.vsess != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
	defer cancel()
	caps := mcpwire.ClientCapabilities(rq.msg.Params)
	s, err := h.deps.Sessions.Virtual(ctx, Session{TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name,
		BuyerID: rq.p.BuyerID, Principal: rq.p.ID, Via: string(rq.p.Via), ClientCaps: caps})
	if err != nil {
		h.log.Error("mcp: a virtual session was not opened", "service", rq.svc.Ref.Name, "error", err)
		return
	}
	rq.vsess = s
	h.virtuals.mu.Lock()
	delete(h.virtuals.flights, vkey{rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID})
	h.virtuals.mu.Unlock()
}

// --- forwarding ---

// modernHeaders are the client request headers forwarded with a modern request (§11.12): Accept, Content-Type,
// MCP-Protocol-Version, Mcp-Method, Mcp-Name for a method whose name CheckModern checked, and every Mcp-Param-* (the
// upstream checks them against the arguments it declared them for). Never Mcp-Session-Id, Last-Event-ID or
// Authorization. MCP-Protocol-Version, Mcp-Method and Mcp-Name are forwarded as the one value Classify and
// CheckModern read (each was refused when sent more than once), so the upstream reads what the gate checked (D172).
//
// SPEC-CONFLICT (D204; relay §11.12: "The gate forwards MCP-Protocol-Version, Mcp-Method, Mcp-Name and the
// Mcp-Param-* headers"): 2026-07-28 defines Mcp-Name for tools/call, prompts/get and resources/read (and the tasks
// extension for tasks/get, tasks/update and tasks/cancel) only, so on any other method there is no body member to
// check it against, and an intermediary behind the gate that routed on it would route on a value nobody checked
// (2026-07-28 Streamable HTTP, "Server Validation"). The closest faithful behaviour: an Mcp-Name on such a method is
// not forwarded.
func modernHeaders(r *http.Request, m *mcpwire.Message) http.Header {
	h := http.Header{}
	for _, k := range []string{"Accept", "Content-Type"} {
		if v := r.Header.Values(k); len(v) > 0 {
			h[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	h.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728) // rule 2: the request's one MCP-Protocol-Version
	h.Set(mcpwire.HeaderMethod, m.Method)                   // CheckModern: its one Mcp-Method, equal to method
	if _, ok := mcpwire.NameSource(m.Method); ok {
		h.Set(mcpwire.HeaderName, r.Header.Get(mcpwire.HeaderName)) // CheckModern: its one Mcp-Name, as the client encoded it
	}
	for k, v := range paramHeaders(r.Header) {
		h[k] = v
	}
	return h
}

// paramHeaders are the Mcp-Param-* headers of h.
func paramHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(mcpwire.HeaderParamPrefix)) {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return out
}

// modernBody is the request as it is forwarded (§11.12, "Rewrites"): §2.5 (a modern message keeps no sealed
// envelope, D162), params.task (a 2025-11-25 field) removed, the tasks extension removed from the per-request
// capabilities unless keepTasks (a gate-owned task's stored call), and a subscriptions/listen's taskIds removed.
// Bytes nothing changed come back as they came.
func modernBody(rq *request, keepTasks bool) ([]byte, error) {
	m := rq.msg
	body, err := mcpwire.ScrubMessage(mcpwire.FromClient, rq.body, m, "", false)
	if err != nil {
		return nil, err
	}
	params, ok := mcpwire.Member(body, "params")
	if !ok || firstByte(params) != '{' {
		return body, nil
	}
	p, err := mcpwire.DeleteMember(params, "task")
	if err == nil && !keepTasks {
		p, err = mcpwire.WithoutTasks(p)
	}
	if err == nil && m.Method == mcpwire.MethodListen {
		p, err = mcpwire.WithoutTaskIDs(p)
	}
	if err != nil {
		return nil, err
	}
	if bytes.Equal(p, params) {
		return body, nil
	}
	return mcpwire.SetMember(body, "params", p)
}

// modernForward forwards a modern request to a target chosen as for an initialize and relays the answer
// (§11.12): a session that turns out to be gone before the exchange is replaced by another candidate; the upstream's
// own 400, 404 or 405 is relayed with its status and body and teaches the gate nothing (D177); a 2xx answer opens the
// principal's virtual session (openVirtual), which the call's meter names; rewrite, when set, rewrites the response
// to the request (server/discover), and every other message is scrubbed (§2.5). The client closing its stream
// cancels the upstream exchange (2026-07-28 "Cancellation": the gate closes its upstream stream).
func (h *Handler) modernForward(rq *request, meter *callMeter, rewrite func(m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal)) {
	m := rq.msg
	body, err := modernBody(rq, false)
	if err != nil {
		h.internal(rq, "rewrite the request", err)
		return
	}
	hdr := modernHeaders(rq.r, m)
	var (
		t     target
		resp  *http.Response
		avoid []uuid.UUID
	)
	for attempt := 0; ; attempt++ {
		var ref *mcpwire.Refusal
		t, ref = h.choose(rq, avoid)
		if ref != nil {
			writeRefusal(rq.w, ref)
			return
		}
		resp, err = h.send(rq.ctx(), t, rq, http.MethodPost, body, hdr, "", upstream.MCPExchange{})
		var g *goneError
		if errors.As(err, &g) && attempt+1 < maxPicks {
			avoid = append(avoid, t.session)
			continue
		}
		break
	}
	if err != nil {
		h.modernFailed(rq, err)
		return
	}
	o := &relayOpts{method: m.Method, reqID: m.ID, tunnel: t.tunnelled(), meter: meter, tools: h.toolsKey(rq)}
	o.onResponse = func(msg *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal) {
		if o.status/100 == 2 {
			h.openVirtual(rq)
			if meter != nil && rq.vsess != nil {
				hash := rq.vsess.Hash
				meter.call.MCPSession = &hash
			}
			if meter != nil && msg.Error == nil {
				if rt, _ := stringMember(msg.Result, "resultType"); rt == "input_required" {
					// SPEC-CONFLICT (D204; relay §11.4, §11.12): a multi round-trip request's InputRequiredResult
					// answers the tools/call without its outcome, and the client sends the call again with its answers;
					// §11.4 says one calls row is written when "the response completes" and does not say what an
					// InputRequiredResult is. The closest faithful behaviour: as for a passthrough task's
					// CreateTaskResult (§11.5), the row is written with is_error NULL (the outcome is not observed),
					// so an operator never bills per_call on it; the call sent again with the answers gets its own row.
					meter.isError = nil
				}
			}
		}
		if rewrite != nil {
			return rewrite(msg, raw)
		}
		out, err := mcpwire.ScrubMessage(mcpwire.FromUpstream, raw, msg, m.Method, false)
		if err != nil {
			b, _ := mcpwire.ErrorBody(m.ID, mcpwire.CodeInternal, msgUpstreamError, nil)
			return nil, &mcpwire.Refusal{Status: http.StatusBadGateway, Body: b}
		}
		return out, nil
	}
	n := h.relayResponse(rq, resp, o)
	h.record(rq, meter, n)
}

// modernFailed answers a modern request whose exchange could not be made (no session to close, no 404: a modern
// client has no session).
func (h *Handler) modernFailed(rq *request, err error) {
	var g *goneError
	var u *unreachableError
	switch {
	case errors.As(err, &g), errors.As(err, &u):
		h.log.Warn("mcp: the upstream of a modern request could not be reached", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusBadGateway, rq.id(), mcpwire.CodeInternal, msgUnreachable, nil)
	case errors.Is(err, errUnchecked):
		h.log.Warn("mcp: a tunnel session's key could not be checked", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, rq.id(), mcpwire.CodeInternal, "the gate could not serve the request", nil)
	case rq.ctx().Err() != nil:
		// The client went away: its upstream exchange is closed with it.
	default:
		h.internal(rq, "reach the upstream", err)
	}
}

// discoverResult rewrites the upstream's server/discover result (§11.12): supportedVersions the upstream's versions
// the gate speaks (2026-07-28) plus 2025-11-25 and 2025-06-18; the tasks extension kept only for a durable service
// that serves gate-owned tasks; §2.5; the operator's initialize_meta merged into _meta (§13.4); cacheScope private.
// The upstream's own result makes the service modern and refreshes its cached capabilities (LearnModern, D177).
func (h *Handler) discoverResult(rq *request) func(m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal) {
	return func(m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal) {
		fail := func(err error) ([]byte, *mcpwire.Refusal) {
			h.log.Warn("mcp: a server/discover result could not be rewritten", "service", rq.svc.Ref.Name, "error", err)
			b, _ := mcpwire.ErrorBody(m.ID, mcpwire.CodeInternal, msgUpstreamError, nil)
			return nil, &mcpwire.Refusal{Status: http.StatusBadGateway, Body: b}
		}
		if m.Error != nil {
			return raw, nil // the upstream's own error, relayed as it came
		}
		res, err := mcpwire.ScrubResult(mcpwire.FromUpstream, mcpwire.MethodServerDiscover, m.Result, false)
		if err != nil {
			return fail(err)
		}
		if ans, _ := mcpwire.ClassifyDiscover(http.StatusOK, &mcpwire.Message{Result: res}); ans == mcpwire.AnswerModern {
			h.deps.Tools.LearnModern(h.toolsKey(rq), res)
		}
		out, err := rewriteDiscover(res, config.GateOwnsTasks(rq.svc.Tasks) && h.deps.Tasks != nil, rq.svc.InitializeMeta)
		if err != nil {
			return fail(err)
		}
		b, err := mcpwire.SetMember(raw, "result", out)
		if err != nil {
			return fail(err)
		}
		return b, nil
	}
}

// rewriteDiscover is discoverResult's rewrite of a scrubbed result.
func rewriteDiscover(res json.RawMessage, keepTasks bool, initMeta map[string]json.RawMessage) (json.RawMessage, error) {
	versions := []string{}
	if v, ok := mcpwire.Member(res, "supportedVersions"); ok {
		var up []json.RawMessage
		if json.Unmarshal(v, &up) == nil {
			for _, x := range up {
				var s string
				if json.Unmarshal(x, &s) == nil && s == mcpwire.V20260728 && !slices.Contains(versions, s) {
					versions = append(versions, s)
				}
			}
		}
	}
	versions = append(versions, mcpwire.V20251125, mcpwire.V20250618)
	vb, err := json.Marshal(versions)
	if err != nil {
		return nil, err
	}
	out, err := mcpwire.SetMember(res, "supportedVersions", vb)
	if err != nil {
		return nil, err
	}
	if !keepTasks {
		if out, err = rewriteCapabilities(out, func(caps json.RawMessage) (json.RawMessage, error) {
			return rewriteMemberIn(caps, "extensions", func(ext json.RawMessage) (json.RawMessage, error) {
				return mcpwire.DeleteMember(ext, mcpwire.ExtTasks)
			})
		}); err != nil {
			return nil, err
		}
	}
	if len(initMeta) > 0 {
		if out, err = mcpwire.MergeMeta(out, initMeta); err != nil {
			return nil, err
		}
	}
	return mcpwire.SetMember(out, "cacheScope", json.RawMessage(`"private"`))
}

// rewriteMemberIn applies fn to obj's member key when it is an object.
func rewriteMemberIn(obj json.RawMessage, key string, fn func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	v, ok := mcpwire.Member(obj, key)
	if !ok || firstByte(v) != '{' {
		return obj, nil
	}
	nv, err := fn(v)
	if err != nil || bytes.Equal(nv, v) {
		return obj, err
	}
	return mcpwire.SetMember(obj, key, nv)
}

// --- tools ---

// modernView is how a modern client sees the service's tools (§11.3, §13.4).
func (h *Handler) modernView(rq *request) ToolView {
	return ToolView{Policy: rq.svc.Tools, Meta: rq.svc.ToolMeta}
}

// modernToolsList answers tools/list from the modern cache (§11.3, §11.12): every upstream page drained through a
// target chosen for the drain, denied tools dropped, Kontra's keys removed, tool_meta applied, one page and no
// cursor, ttlMs the upstream's but at most 600000, cacheScope private (the list is per principal).
func (h *Handler) modernToolsList(rq *request) {
	list, ttl, err := h.deps.Tools.ListModern(rq.ctx(), h.toolsKey(rq), h.modernView(rq), h.modernPager(rq))
	if err != nil {
		h.modernToolsFailed(rq, err)
		return
	}
	if list == nil {
		list = []json.RawMessage{}
	}
	res, err := json.Marshal(map[string]any{"tools": list, "ttlMs": ttl, "cacheScope": "private", "resultType": "complete"})
	if err != nil {
		h.internal(rq, "write tools/list", err)
		return
	}
	h.openVirtual(rq)
	writeResult(rq.w, "", rq.msg.ID, res)
}

// upstreamStatusError is a 400, 404 or 405 the upstream answered a request the gate made for a client's modern
// request (a tools/list page): relayed to that client with its status and body (§11.12), which teaches the gate
// nothing (D177).
type upstreamStatusError struct {
	status int
	body   []byte // a JSON-RPC message Parse accepted, scrubbed; nil for none
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("mcprelay: the upstream answered %d", e.status)
}

// modernPager fetches the modern tools/list pages: stateless requests with the client's per-request _meta (the
// tasks extension removed) and its JSON-RPC id, on a target chosen for the drain on the drain's own context.
func (h *Handler) modernPager(rq *request) Pager {
	id := json.RawMessage(bytes.Clone(rq.msg.ID))
	meta, _ := mcpwire.Member(rq.msg.Params, "_meta")
	if p, err := mcpwire.WithoutTasks(json.RawMessage(`{"_meta":` + string(orEmpty(meta)) + `}`)); err == nil {
		meta, _ = mcpwire.Member(p, "_meta")
	}
	meta = bytes.Clone(meta)
	who := &request{svc: rq.svc, p: rq.p}
	var (
		mu     sync.Mutex
		chosen *target
	)
	return func(ctx context.Context, cursor string) (json.RawMessage, error) {
		params := map[string]any{"_meta": json.RawMessage(orEmpty(meta))}
		if cursor != "" {
			params["cursor"] = cursor
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": mcpwire.MethodToolsList, "params": params})
		if err != nil {
			return nil, err
		}
		mu.Lock()
		if chosen == nil {
			t, ref := h.chooseIn(ctx, who, nil, nil)
			if ref != nil {
				mu.Unlock()
				return nil, &unreachableError{errors.New("no node serves the service")}
			}
			chosen = &t
		}
		t := *chosen
		mu.Unlock()
		hdr := http.Header{}
		hdr.Set("Accept", "application/json, text/event-stream")
		hdr.Set("Content-Type", "application/json")
		hdr.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
		hdr.Set(mcpwire.HeaderMethod, mcpwire.MethodToolsList)
		resp, err := h.send(ctx, t, who, http.MethodPost, body, hdr, "", upstream.MCPExchange{})
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed:
			if !t.tunnelled() || resp.Header.Get("Kontra-Node") == "" {
				return nil, statusAnswer(resp)
			}
		}
		return h.readAnswer(resp, id, t.tunnelled())
	}
}

func orEmpty(b json.RawMessage) json.RawMessage {
	if firstByte(b) != '{' {
		return json.RawMessage(`{}`)
	}
	return b
}

// statusAnswer reads a 400, 404 or 405 for relaying: its JSON-RPC message when the body is one Parse accepts
// (scrubbed), none otherwise.
func statusAnswer(resp *http.Response) error {
	defer resp.Body.Close()
	e := &upstreamStatusError{status: resp.StatusCode}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || len(b) > 64<<10 || mt != "application/json" {
		return e
	}
	m, err := mcpwire.Parse(b, mcpwire.FromUpstream)
	if err != nil {
		return e
	}
	if out, err := mcpwire.ScrubMessage(mcpwire.FromUpstream, b, m, "", false); err == nil {
		e.body = out
	}
	return e
}

// modernToolsFailed answers a modern tools/list drain (or a tool's lookup) that failed: the upstream's 400, 404 or
// 405 as it answered it (§11.12, D177), its JSON-RPC error, and anything else 502 with -32603.
func (h *Handler) modernToolsFailed(rq *request, err error) {
	var se *upstreamStatusError
	var ue *upstreamRPCError
	switch {
	case errors.As(err, &se):
		if len(se.body) == 0 {
			writeEmpty(rq.w, se.status)
			return
		}
		rq.w.Header().Set("Content-Type", "application/json")
		rq.w.WriteHeader(se.status)
		_, _ = rq.w.Write(se.body)
	case errors.As(err, &ue):
		writeError(rq.w, http.StatusOK, rq.msg.ID, ue.e.Code, ue.e.Message, ue.e.Data)
	case errors.Is(err, errUpstreamRefused):
		writeError(rq.w, http.StatusBadGateway, rq.msg.ID, mcpwire.CodeInternal, msgUpstreamRefused, nil)
	case errors.Is(err, errUnchecked):
		h.modernFailed(rq, err)
	case rq.ctx().Err() != nil:
	default:
		h.log.Warn("mcp: the upstream's modern tools/list was not drained", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusBadGateway, rq.msg.ID, mcpwire.CodeInternal, msgUpstreamError, nil)
	}
}

// modernToolsCall serves a modern tools/call (§11.3, §11.4, §11.12): the tool resolved against the modern cached
// list (unknown or denied: -32602 before anything is forwarded or metered); a gate-owned task when the request
// declares the tasks extension on a durable or required service whose upstream declared it in its cached
// server/discover (D159); on a required service any other call is refused (D322); otherwise a plain call, Admission
// call, forwarded with its meter, one calls row naming the virtual session.
func (h *Handler) modernToolsCall(rq *request) {
	m := rq.msg
	name, _ := stringMember(m.Params, "name")
	_, known, err := h.deps.Tools.Tool(rq.ctx(), h.toolsKey(rq), mcpwire.EraModern, rq.svc.Tools, name, h.modernPager(rq))
	if err != nil {
		h.modernToolsFailed(rq, err)
		return
	}
	if !known {
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeInvalidParams, "Unknown tool: "+name, nil)
		return
	}
	if h.ownsModernTask(rq) {
		h.modernTaskCall(rq, name)
		return
	}
	if rq.svc.Tasks == config.TasksRequired {
		// D322: every call of a required service is a gate task. A request that does not declare the tasks extension is
		// 400 with -32021 naming it, as the extension answers a request that needs it (D176); an upstream that does not
		// declare it in this revision runs no task, so the service takes no call in it.
		if !mcpwire.DeclaresTasks(m.Params) {
			writeRefusal(rq.w, mcpwire.MissingTasks(m.ID))
			return
		}
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "this service runs every tools/call as a task "+
			"(tasks: required), which its server does not offer in MCP 2026-07-28", nil)
		return
	}
	if ref := h.admit(rq, hooks.OpCall, name); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	release, ok := h.takeCall(rq)
	if !ok {
		return
	}
	defer release()
	meter := &callMeter{start: rq.start, call: Call{TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name, Tool: name,
		BuyerID: rq.p.BuyerID, Principal: rq.p.ID}}
	if rq.vsess != nil {
		hash := rq.vsess.Hash
		meter.call.MCPSession = &hash
	}
	h.modernForward(rq, meter, nil)
}

// ownsModernTask is D159's rule: a tools/call that declares the tasks extension, on a durable (or required, D322)
// service that serves gate-owned tasks, whose upstream declared the extension in its cached server/discover.
func (h *Handler) ownsModernTask(rq *request) bool {
	if !config.GateOwnsTasks(rq.svc.Tasks) || h.deps.Tasks == nil || !mcpwire.DeclaresTasks(rq.msg.Params) {
		return false
	}
	caps, ok := h.deps.Tools.Capabilities(h.toolsKey(rq))
	return ok && declaresTasks(caps)
}

// modernTaskCall creates a gate-owned task for a modern tools/call (§11.12 steps 2–4, D159): Admission task (a quote
// creates it quoted, §13.2), the virtual session opened (its hash names the task's calls row, never a preamble,
// D180), the gate task with the request as it came after §2.5 (params.task removed, the tasks extension kept: the
// dispatch sends the call with the client's _meta) and its Mcp-Param-* headers, the TaskLifecycle start, then the
// answer: in line when the task ends within 2 s, else a CreateTaskResult (Tasks.AnswerCall).
func (h *Handler) modernTaskCall(rq *request, name string) {
	m := rq.msg
	taskID, d, ref := h.admitTask(rq, name)
	if ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	body, err := modernBody(rq, true)
	if err != nil {
		h.internal(rq, "rewrite the request", err)
		return
	}
	h.openVirtual(rq)
	ct := CreateTask{
		TaskID: taskID, Service: rq.svc.Ref, BuyerID: rq.p.BuyerID, Principal: rq.p.ID, Via: string(rq.p.Via),
		Era: mcpwire.EraModern, Version: mcpwire.V20260728, Request: body, Tool: name, RequestID: m.ID,
		ClientCapabilities: mcpwire.ClientCapabilities(m.Params), Decision: d, Headers: paramHeaders(rq.r.Header),
	}
	if rq.vsess != nil {
		hash := rq.vsess.Hash
		ct.MCPSession = &hash
	}
	id, err := h.deps.Tasks.Create(rq.ctx(), ct)
	switch {
	case errors.Is(err, errAdmission):
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "admission unavailable", nil)
		return
	case errors.Is(err, lifecycle.ErrUnavailable):
		h.log.Warn("mcp: a task's lifecycle did not start in time; the orphan sweep ends it", "service", rq.svc.Ref.Name, "task_id", id, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the task's lifecycle is unavailable", nil)
		return
	case anonRefused(rq.w, m.ID, err):
		return
	case err != nil:
		h.internal(rq, "create the task", err)
		return
	}
	h.log.Info("mcp task created", "service", rq.svc.Ref.Name, "task_id", id, "principal", rq.p.ID, "era", "2026-07-28")
	h.deps.Tasks.AnswerCall(rq.ctx(), rq.w, rq.svc.Ref, id, m.ID)
}

// modernTaskRequest serves tasks/get, tasks/update and tasks/cancel from Postgres on any replica (§11.12, "The client
// side"): a request that does not declare the tasks extension is 400 with -32021 (D176); a service that serves no
// gate-owned tasks (passthrough, off) has no such method, 404 with -32601.
func (h *Handler) modernTaskRequest(rq *request) {
	m := rq.msg
	if !mcpwire.DeclaresTasks(m.Params) {
		writeRefusal(rq.w, mcpwire.MissingTasks(m.ID))
		return
	}
	if !config.GateOwnsTasks(rq.svc.Tasks) || h.deps.Tasks == nil {
		writeError(rq.w, http.StatusNotFound, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
		return
	}
	res, rpcErr, err := h.deps.Tasks.ServeModern(rq.ctx(), h.caller(rq), m)
	switch {
	case err != nil:
		h.internal(rq, "serve "+m.Method, err)
	case rpcErr != nil:
		writeError(rq.w, http.StatusOK, m.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
	default:
		writeResult(rq.w, "", m.ID, res)
	}
}
