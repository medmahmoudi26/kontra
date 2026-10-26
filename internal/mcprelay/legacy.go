package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/upstream"
)

// The legacy era (spec/relay.md §11.1–§11.5): initialize opens a session pinned to a tunnel session (or a direct
// backend); every later request names it with Mcp-Session-Id and is relayed on it.

// LegacyVersions are the versions an upstream's initialize may negotiate (§11.1, D65).
var LegacyVersions = []string{mcpwire.V20251125, mcpwire.V20250618}

// maxPicks bounds how many candidate sessions an initialize tries when the one picked turns out to be gone.
const maxPicks = 3

// initialize opens a legacy session (§11.2): Admission's session decision, the session choice (§6.1: the direct
// backend, or this replica's tunnel sessions first and another replica's through the peer hop), the upstream
// initialize under a client id minted first (its hash is the preamble's mcp_session, tunnel.md §8.1), then, on an
// InitializeResult that negotiated 2025-11-25 or 2025-06-18, the mcp_sessions row and the client's answer: the
// result scrubbed (§2.5), its capabilities.tasks per §11.5, the operator's initialize_meta merged into _meta
// (§13.4), and the gate's Mcp-Session-Id. Another negotiated version is -32602 naming the supported ones, and the
// upstream session gets a best-effort DELETE.
func (h *Handler) initialize(rq *request) {
	if ref := h.admit(rq, hooks.OpSession, ""); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	clientID, hash, err := MintID()
	if err != nil {
		h.internal(rq, "mint a session id", err)
		return
	}
	body, err := mcpwire.ScrubMessage(mcpwire.FromClient, rq.body, rq.msg, "", false)
	if err != nil {
		h.internal(rq, "scrub the request", err)
		return
	}
	hdr := forwardHeaders(rq.r, nil)
	x := upstream.MCPExchange{Session: hexHash(hash)}
	var (
		t    target
		resp *http.Response
	)
	var avoid []uuid.UUID
	for attempt := 0; ; attempt++ {
		var ref *mcpwire.Refusal
		t, ref = h.choose(rq, avoid)
		if ref != nil {
			writeRefusal(rq.w, ref)
			return
		}
		resp, err = h.send(rq.ctx(), t, rq, http.MethodPost, body, hdr, "", x)
		var g *goneError
		if errors.As(err, &g) && attempt+1 < maxPicks {
			avoid = append(avoid, t.session) // gone before the exchange: another candidate
			continue
		}
		break
	}
	if err != nil {
		h.upstreamFailed(rq, err)
		return
	}
	sess := &Session{
		Hash: hash, TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name, BuyerID: rq.p.BuyerID,
		Principal: rq.p.ID, Via: string(rq.p.Via), UpstreamSID: resp.Header.Get(mcpwire.HeaderSessionID),
		ReplicaID: t.replica, ClientCaps: clientCapabilities(rq.msg.Params),
	}
	if t.direct == nil {
		s := t.session
		sess.TunnelSession = &s
	}
	o := &relayOpts{
		sid: clientID, method: mcpwire.MethodInitialize, reqID: rq.msg.ID, tunnel: t.tunnelled(),
		tools: h.toolsKey(rq), hold: true,
	}
	o.onResponse = func(m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal) {
		if m.Error != nil {
			o.sid = "" // no session: the upstream refused the initialize, which is relayed
			return raw, nil
		}
		version, _ := stringMember(m.Result, "protocolVersion")
		if !slices.Contains(LegacyVersions, version) {
			h.log.Warn("mcp: an upstream negotiated a version the gate does not speak", "service", rq.svc.Ref.Name, "version", version)
			if sess.UpstreamSID != "" {
				h.deleteUpstream(context.WithoutCancel(rq.ctx()), t, *sess)
			}
			b, _ := mcpwire.ErrorBody(rq.msg.ID, mcpwire.CodeInvalidParams, "Unsupported protocol version",
				map[string]any{"supported": LegacyVersions, "requested": version})
			return nil, &mcpwire.Refusal{Status: http.StatusOK, Body: b}
		}
		sess.ProtocolVersion = version
		out, err := h.initializeResult(rq, raw, m, version)
		if err != nil {
			h.log.Warn("mcp: an initialize result could not be rewritten", "service", rq.svc.Ref.Name, "error", err)
			b, _ := mcpwire.ErrorBody(rq.msg.ID, mcpwire.CodeInternal, msgUpstreamError, nil)
			return nil, &mcpwire.Refusal{Status: http.StatusBadGateway, Body: b}
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
		defer cancel()
		if err := h.deps.Sessions.Open(ctx, clientID, *sess); err != nil {
			h.log.Error("mcp: a session row was not written", "service", rq.svc.Ref.Name, "error", err)
			if sess.UpstreamSID != "" {
				h.deleteUpstream(context.WithoutCancel(rq.ctx()), t, *sess)
			}
			b, _ := mcpwire.ErrorBody(rq.msg.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
			return nil, &mcpwire.Refusal{Status: http.StatusServiceUnavailable, Body: b}
		}
		h.log.Info("mcp session opened", "service", rq.svc.Ref.Name, "principal", rq.p.ID, "protocol_version", version,
			"backend", backendName(t), "holder", t.replica)
		return out, nil
	}
	h.relayResponse(rq, resp, o)
}

// initializeResult rewrites the InitializeResult the client gets (§11.2, §11.5, §2.5, §13.4).
func (h *Handler) initializeResult(rq *request, raw []byte, m *mcpwire.Message, version string) ([]byte, error) {
	res, err := mcpwire.ScrubResult(mcpwire.FromUpstream, mcpwire.MethodInitialize, m.Result, false)
	if err != nil {
		return nil, err
	}
	if !h.tasksAdvertised(rq.svc, version) {
		if res, err = rewriteCapabilities(res, func(caps json.RawMessage) (json.RawMessage, error) {
			return mcpwire.DeleteMember(caps, "tasks")
		}); err != nil {
			return nil, err
		}
	}
	if len(rq.svc.InitializeMeta) > 0 {
		if res, err = mcpwire.MergeMeta(res, rq.svc.InitializeMeta); err != nil {
			return nil, err
		}
	}
	return mcpwire.SetMember(raw, "result", res)
}

// rewriteCapabilities applies fn to result.capabilities when it is an object.
func rewriteCapabilities(result json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	caps, ok := mcpwire.Member(result, "capabilities")
	if !ok || firstByte(caps) != '{' {
		return result, nil
	}
	c, err := fn(caps)
	if err != nil || bytes.Equal(c, caps) {
		return result, err
	}
	return mcpwire.SetMember(result, "capabilities", c)
}

// tasksAdvertised reports whether the session is shown the upstream's tasks (§11.5): passthrough as the upstream
// declares; durable once gate-owned tasks are served (Deps.Tasks, M3 WP-06); never for off or a 2025-06-18
// session.
func (h *Handler) tasksAdvertised(svc *Service, version string) bool {
	switch {
	case version == mcpwire.V20250618:
		return false
	case svc.Tasks == config.TasksPassthrough:
		return true
	case svc.Tasks == config.TasksDurable:
		return h.deps.Tasks != nil
	}
	return false
}

// choose is the target of a new session (§6.1, §11.2): the direct backend; else a tunnel session, this replica's
// first, another replica's through its peer listener when this one runs one. No candidate is 503.
func (h *Handler) choose(rq *request, avoid []uuid.UUID) (target, *mcpwire.Refusal) {
	if rq.svc.Direct != nil {
		return target{direct: rq.svc.Direct}, nil
	}
	if h.deps.Picker == nil || h.deps.Tunnels == nil {
		return target{}, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "no node serves the service")
	}
	for range maxPicks {
		pick, err := h.deps.Picker.PickMCP(rq.ctx(), rq.svc.Ref.TenantID, rq.svc.Ref.Name, h.deps.ReplicaID, h.deps.Peer != nil, avoid...)
		switch {
		case errors.Is(err, lifecycle.ErrNoSession):
			return target{}, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "no node serves the service")
		case err != nil:
			h.log.Warn("mcp: the session choice failed", "service", rq.svc.Ref.Name, "error", err)
			return target{}, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "the gate could not serve the request")
		}
		if pick.Replica != h.deps.ReplicaID {
			return target{peerAddr: pick.PeerAddr, session: pick.Session, replica: pick.Replica}, nil
		}
		be, err := h.deps.Tunnels.MCPBackend(pick.Session, rq.svc.Ref.TenantID, rq.svc.Ref.Name)
		if err != nil {
			avoid = append(avoid, pick.Session)
			continue
		}
		return target{tunnel: be, session: pick.Session, replica: pick.Replica}, nil
	}
	return target{}, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "no node serves the service")
}

// pinned serves a request on a pinned session (§11.2): the session looked up for this service and principal (404
// otherwise, opening no stream), §6.1's check on the replica holding its tunnel session, then by method.
func (h *Handler) pinned(rq *request) {
	sid := rq.r.Header.Get(mcpwire.HeaderSessionID)
	sess, err := h.deps.Sessions.Lookup(rq.ctx(), sid, rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID)
	switch {
	case errors.Is(err, ErrNoSession):
		h.noSession(rq)
		return
	case err != nil:
		h.internal(rq, "look up the session", err)
		return
	}
	rq.sess, rq.sid = sess, sid
	if now := h.deps.now(); now.Sub(sess.LastSeenAt) >= IdleWrite {
		if err := h.deps.Sessions.Touch(rq.ctx(), sess.Hash, now); err != nil {
			h.log.Debug("mcp: last_seen_at not written", "error", err)
		}
	}

	if rq.r.Method == http.MethodGet && strings.HasPrefix(rq.r.Header.Get(mcpwire.HeaderLastEventID), "k.") {
		// §11.7: a gate-written stream, served from task_events by any replica and never forwarded. Gate-owned
		// tasks are M3 WP-06's; until then no such stream exists to resume. §11.7 names every Last-Event-ID that
		// starts with "k.", not only the gate's own form k.<taskId>.<seq>, so an upstream whose SSE event ids start
		// with "k." cannot be resumed through the gate (left to the spec's owner).
		writeEmpty(rq.w, http.StatusBadRequest)
		return
	}
	if m := rq.msg; m != nil && m.Method == "" && isGateRequestID(m.ID) {
		// §11.7: a client response to a gate-minted request id is the gate's; without gate-owned tasks (M3 WP-06)
		// every such id is unknown: 202 and dropped.
		writeEmpty(rq.w, http.StatusAccepted)
		return
	}

	t, err := h.pinnedTarget(rq)
	if err != nil {
		h.upstreamFailed(rq, err)
		return
	}
	switch rq.r.Method {
	case http.MethodGet:
		h.forward(rq, t, nil)
		return
	case http.MethodDelete:
		h.delete(rq, t)
		return
	}
	switch m := rq.msg; m.Method {
	case mcpwire.MethodToolsList:
		h.toolsList(rq, t)
	case mcpwire.MethodToolsCall:
		h.toolsCall(rq, t)
	default:
		if strings.HasPrefix(m.Method, "tasks/") && !h.tasksAdvertised(rq.svc, sess.ProtocolVersion) {
			// §11.5: a service that advertises no tasks has none to serve (off; durable until M3 WP-06; 2025-06-18).
			writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
			return
		}
		h.forward(rq, t, nil)
	}
}

// isGateRequestID reports whether a JSON-RPC id is a gate-minted request id (§11.7): exactly the string
// k.<taskId>.<seq>, a gate task id as the gate writes it (a UUID, lowercase and hyphenated) and a decimal sequence
// number without sign or leading zero. An upstream's own id that merely starts with "k." is not one: its client's
// answer is forwarded like every other response (§11.2: upstream ids pass unchanged). M3 WP-06 mints the ids in this
// form, and then also requires a task of the caller's buyer.
func isGateRequestID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	if len(id) == 0 || id[0] != '"' {
		return false
	}
	var s string
	if json.Unmarshal(id, &s) != nil {
		return false
	}
	rest, ok := strings.CutPrefix(s, "k.")
	if !ok {
		return false
	}
	task, seq, ok := strings.Cut(rest, ".")
	if !ok {
		return false
	}
	if u, err := uuid.Parse(task); err != nil || u.String() != task {
		return false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == seq
}

// pinnedTarget is where a pinned session's requests go (§11.2): its direct backend; its tunnel session on this
// replica after §6.1's check; or, for a tunnel session another replica holds, that replica's peer listener. A
// session §6.1's check refuses is closed (revoked or tunnel_lost); one whose holder is no longer live is closed
// tunnel_lost. Without peer.listen, or when the holder advertises no peer address, it is 404 and the row stays open
// until it is idle (§11.2).
func (h *Handler) pinnedTarget(rq *request) (target, error) {
	s, svc := rq.sess, rq.svc
	switch {
	case s.TunnelSession == nil:
		if svc.Direct == nil {
			// The service moved from a direct backend to a tunnel: its upstream session is not on any node.
			return target{}, &goneError{reason: CloseUpstreamClosed, err: errors.New("the service no longer has a direct backend")}
		}
		return target{direct: svc.Direct}, nil
	case svc.Direct != nil:
		return target{}, &goneError{reason: CloseTunnelLost, err: errors.New("the service no longer has a tunnel backend")}
	case s.ReplicaID == h.deps.ReplicaID:
		if h.deps.Tunnels == nil {
			return target{}, &goneError{reason: CloseTunnelLost, err: errors.New("this replica holds no tunnel sessions")}
		}
		be, err := h.deps.Tunnels.MCPBackend(*s.TunnelSession, s.TenantID, s.Service)
		if err != nil {
			return target{}, classifyTunnel(err)
		}
		return target{tunnel: be, session: *s.TunnelSession, replica: s.ReplicaID}, nil
	}
	addr, live, err := h.deps.Sessions.PeerAddr(rq.ctx(), s.ReplicaID)
	switch {
	case err != nil:
		return target{}, fmt.Errorf("%w: %w", errUnchecked, err)
	case !live:
		return target{}, &goneError{reason: CloseTunnelLost, err: fmt.Errorf("replica %s holding the tunnel session is gone", s.ReplicaID)}
	case h.deps.Peer == nil || addr == "":
		return target{}, errNoHop
	}
	return target{peerAddr: addr, session: *s.TunnelSession, replica: s.ReplicaID}, nil
}

// errNoHop is a pinned session held by another replica that this replica cannot reach: no peer.listen here, or no
// peer address there. The client gets 404 (it re-initializes on this replica) and the row stays open (§11.2).
var errNoHop = errors.New("mcprelay: the session's tunnel is held by another replica and there is no peer hop")

// upstreamFailed answers a request whose exchange could not be made.
func (h *Handler) upstreamFailed(rq *request, err error) {
	var g *goneError
	var u *unreachableError
	switch {
	case errors.As(err, &g):
		h.log.Info("mcp: a pinned session's tunnel is gone", "service", rq.svc.Ref.Name, "reason", g.reason, "error", g.err)
		h.closeRow(rq, g.reason)
		h.noSession(rq)
	case errors.Is(err, errNoHop):
		h.log.Info("mcp: a session pinned to another replica cannot be reached without the peer hop (§11.2)",
			"service", rq.svc.Ref.Name, "holder", rq.sess.ReplicaID)
		h.noSession(rq)
	case errors.Is(err, errUnchecked):
		h.log.Warn("mcp: a tunnel session's key could not be checked", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, rq.id(), mcpwire.CodeInternal, "the gate could not serve the request", nil)
	case errors.As(err, &u):
		h.log.Warn("mcp: the upstream could not be reached", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusBadGateway, rq.id(), mcpwire.CodeInternal, msgUnreachable, nil)
	case rq.ctx().Err() != nil:
		// The client went away.
	default:
		h.internal(rq, "reach the upstream", err)
	}
}

// noSession is the transport's 404 for an unknown session (§11.2): the client re-initializes.
func (h *Handler) noSession(rq *request) {
	if rq.r.Method == http.MethodPost {
		writeError(rq.w, http.StatusNotFound, nil, codeNoSession, msgNoSession, nil)
		return
	}
	writeEmpty(rq.w, http.StatusNotFound)
}

// forward relays a request on its pinned session: a POST scrubbed (§2.5), a GET (its Last-Event-ID resume
// included) as it came. A tools/call carries its meter, which writes the calls row once an answer came back (§11.4,
// callMeter): an exchange that failed before any did writes none.
func (h *Handler) forward(rq *request, t target, meter *callMeter) {
	body := rq.body
	o := &relayOpts{sid: rq.sid, sealed: h.sealed(rq), tunnel: t.tunnelled(), meter: meter, tools: h.toolsKey(rq)}
	x := upstream.MCPExchange{Session: rq.sess.HashHex()}
	if m := rq.msg; m != nil {
		var err error
		// SPEC-CONFLICT (relay §2.5, D174: keep the envelope "in the result._meta of the caller's response to one of
		// those [elicitation/create, sampling/createMessage]"): a JSON-RPC response does not name the request it
		// answers, and on a pinned session the request may have reached the client through another replica's
		// stream. The closest faithful behaviour: on a sealed service an exact {v, envelope} is kept on every client
		// response (method elicitation/create for the scrub's rule); every other sh.kontra/ key is removed, and the
		// hosting node, which sealed the request, still checks the answer (ext-sealed.md §5 rule 4b).
		respMethod := ""
		if m.Method == "" {
			respMethod = mcpwire.MethodElicit
		}
		if body, err = mcpwire.ScrubMessage(mcpwire.FromClient, rq.body, m, respMethod, o.sealed); err != nil {
			h.internal(rq, "scrub the request", err)
			return
		}
		o.method, o.reqID = m.Method, m.ID
		if m.Method == mcpwire.MethodToolsCall {
			x.RequestID = idString(m.ID)
		}
	}
	resp, err := h.send(rq.ctx(), t, rq, rq.r.Method, body, forwardHeaders(rq.r, rq.sess), rq.sess.UpstreamSID, x)
	if err != nil {
		// No answer reached the gate, so no response completes and there is no calls row (§11.4), as when §6.1's
		// check refuses the session on this replica before anything is sent (pinnedTarget).
		h.upstreamFailed(rq, err)
		return
	}
	n := h.relayResponse(rq, resp, o)
	h.record(rq, meter, n)
}

// sealed is the scrub's sealed flag for a legacy request (D162, D174).
func (h *Handler) sealed(rq *request) bool { return rq.svc.Sealed }

// delete forwards a DELETE on its pinned session and closes the row deleted (§11.2), whatever the upstream answers.
func (h *Handler) delete(rq *request, t target) {
	resp, err := h.send(rq.ctx(), t, rq, http.MethodDelete, nil, forwardHeaders(rq.r, rq.sess), rq.sess.UpstreamSID,
		upstream.MCPExchange{Session: rq.sess.HashHex()})
	h.closeRow(rq, CloseDeleted)
	if err != nil {
		var g *goneError
		if errors.As(err, &g) || errors.Is(err, errNoHop) {
			writeEmpty(rq.w, http.StatusOK) // the gate's session is ended; the upstream's dies with its tunnel
			return
		}
		h.upstreamFailed(rq, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	status := resp.StatusCode
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		status = http.StatusBadGateway
	}
	writeEmpty(rq.w, status)
}

// toolsKey is the request's service in the tools cache.
func (h *Handler) toolsKey(rq *request) ToolsKey {
	return ToolsKey{TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name}
}

// view is how this session sees the service's tools (§11.3, §11.5, §13.4).
func (h *Handler) view(rq *request) ToolView {
	return ToolView{Policy: rq.svc.Tools, Meta: rq.svc.ToolMeta, DropExecution: !h.tasksAdvertised(rq.svc, rq.sess.ProtocolVersion)}
}

// toolsList answers tools/list from the cache (§11.3): every upstream page drained through the client's own
// session when the cache has no fresh list, denied tools dropped, Kontra's keys removed, tool_meta applied, one
// page and no cursor.
func (h *Handler) toolsList(rq *request, t target) {
	list, err := h.deps.Tools.List(rq.ctx(), h.toolsKey(rq), mcpwire.EraLegacy, h.view(rq), h.pager(rq, t))
	if err != nil {
		h.toolsFailed(rq, err)
		return
	}
	if list == nil {
		list = []json.RawMessage{}
	}
	res, err := json.Marshal(map[string]any{"tools": list})
	if err != nil {
		h.internal(rq, "write tools/list", err)
		return
	}
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", rq.msg.ID, res})
	if err != nil {
		h.internal(rq, "write tools/list", err)
		return
	}
	rq.w.Header().Set("Content-Type", "application/json")
	rq.w.Header().Set(mcpwire.HeaderSessionID, rq.sid)
	rq.w.WriteHeader(http.StatusOK)
	_, _ = rq.w.Write(b)
}

// toolsFailed answers a tools/list drain that failed: the upstream's own JSON-RPC error as it answered, a gone
// session's 404, and anything else 502 with -32603.
func (h *Handler) toolsFailed(rq *request, err error) {
	var ue *upstreamRPCError
	switch {
	case errors.As(err, &ue):
		writeError(rq.w, http.StatusOK, rq.msg.ID, ue.e.Code, ue.e.Message, ue.e.Data)
	case errors.Is(err, errUpstreamRefused):
		writeError(rq.w, http.StatusBadGateway, rq.msg.ID, mcpwire.CodeInternal, msgUpstreamRefused, nil)
	case errors.Is(err, errUpstreamGone):
		h.closeRow(rq, CloseUpstreamClosed)
		h.noSession(rq)
	default:
		var g *goneError
		var u *unreachableError
		if errors.As(err, &g) || errors.As(err, &u) || errors.Is(err, errNoHop) || errors.Is(err, errUnchecked) {
			h.upstreamFailed(rq, err)
			return
		}
		if rq.ctx().Err() != nil {
			return
		}
		h.log.Warn("mcp: the upstream's tools/list was not drained", "service", rq.svc.Ref.Name, "error", err)
		writeError(rq.w, http.StatusBadGateway, rq.msg.ID, mcpwire.CodeInternal, msgUpstreamError, nil)
	}
}

// Errors of a page the gate fetches itself.
var (
	errUpstreamRefused = errors.New("mcprelay: the upstream refused the request (401, 403 or 407)")
	errUpstreamGone    = errors.New("mcprelay: the upstream no longer knows the session (404)")
)

// upstreamRPCError is a JSON-RPC error the upstream answered a request the gate made itself.
type upstreamRPCError struct{ e *mcpwire.Error }

func (e *upstreamRPCError) Error() string {
	return fmt.Sprintf("mcprelay: the upstream answered %d %s", e.e.Code, strictjson.Quote(e.e.Message))
}

// pager fetches tools/list pages on the client's own upstream session, under the client's JSON-RPC id (it waits
// for this answer, so no other request of it uses the id). The cache runs the drain on a context of its own, which
// may outlive the request (ToolsCache, DrainTimeout): what the pages need of it is read here, once.
func (h *Handler) pager(rq *request, t target) Pager {
	id := json.RawMessage(bytes.Clone(rq.msg.ID))
	v := rq.r.Header.Get(mcpwire.HeaderProtocolVersion)
	if v == "" {
		v = rq.sess.ProtocolVersion
	}
	upstreamSID := rq.sess.UpstreamSID
	x := upstream.MCPExchange{Session: rq.sess.HashHex()}
	who := &request{svc: rq.svc, p: rq.p} // the peer hop's assertion names the service and the principal
	tunnelled := t.tunnelled()
	return func(ctx context.Context, cursor string) (json.RawMessage, error) {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": mcpwire.MethodToolsList, "params": params})
		if err != nil {
			return nil, err
		}
		hdr := http.Header{}
		hdr.Set("Accept", "application/json, text/event-stream")
		hdr.Set("Content-Type", "application/json")
		hdr.Set(mcpwire.HeaderProtocolVersion, v)
		resp, err := h.send(ctx, t, who, http.MethodPost, body, hdr, upstreamSID, x)
		if err != nil {
			return nil, err
		}
		return h.readAnswer(resp, id, tunnelled)
	}
}

// readAnswer reads the upstream's answer to a request the gate made itself (a tools/list page): a JSON body, or
// the event of an event stream that answers id (the stream's other messages are not the client's and are dropped),
// within MaxToolBytes, decoded strictly. The gate reads an event stream as a client does: the first answer, in an
// event a client dispatches as a message (event.message).
func (h *Handler) readAnswer(resp *http.Response, id json.RawMessage, tunnelled bool) (json.RawMessage, error) {
	defer resp.Body.Close()
	if tunnelled {
		if reason := resp.Header.Get("Kontra-Node"); reason != "" {
			return nil, &unreachableError{fmt.Errorf("the node answered %s", reason)}
		}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		return nil, errUpstreamRefused
	case http.StatusNotFound:
		return nil, errUpstreamGone
	}
	answer := func(raw []byte) (json.RawMessage, bool, error) {
		m, err := mcpwire.Parse(raw, mcpwire.FromUpstream)
		if err != nil {
			return nil, false, err
		}
		if m.Method != "" || !sameID(m.ID, id) {
			return nil, false, nil
		}
		if m.Error != nil {
			return nil, true, &upstreamRPCError{m.Error}
		}
		return m.Result, true, nil
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream") {
		r := newSSEReader(io.LimitReader(resp.Body, MaxToolBytes))
		for {
			ev, err := r.next()
			if err != nil {
				return nil, fmt.Errorf("%w: the event stream ended without the answer: %w", ErrToolsLimit, err)
			}
			if !ev.message() || !ev.hasData || strings.TrimSpace(ev.data) == "" {
				continue
			}
			res, ok, err := answer([]byte(ev.data))
			if err != nil || ok {
				return res, err
			}
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxToolBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxToolBytes {
		return nil, fmt.Errorf("%w: a page over %d bytes", ErrToolsLimit, MaxToolBytes)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%w: the upstream answered %d", ErrToolsLimit, resp.StatusCode)
	}
	res, ok, err := answer(b)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: the answer is not the response to tools/list", ErrToolsLimit)
	}
	return res, nil
}

// toolsCall serves a tools/call (§11.3–§11.5): the tool resolved against the cached list (an unknown or denied tool
// is -32602 "Unknown tool: <name>" before anything is forwarded or metered); a task-augmented call by the service's
// tasks: mode; Admission (call, or task for a passthrough task); then the call forwarded on the pinned session with
// its meter, one calls row when the response completes.
func (h *Handler) toolsCall(rq *request, t target) {
	m := rq.msg
	name, ok := stringMember(m.Params, "name")
	if !ok {
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeInvalidParams, "Unknown tool: ", nil)
		return
	}
	_, known, err := h.deps.Tools.Tool(rq.ctx(), h.toolsKey(rq), mcpwire.EraLegacy, rq.svc.Tools, name, h.pager(rq, t))
	if err != nil {
		h.toolsFailed(rq, err)
		return
	}
	if !known {
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeInvalidParams, "Unknown tool: "+name, nil)
		return
	}
	_, task := mcpwire.Member(m.Params, "task")
	op := hooks.OpCall
	if task {
		if !h.tasksAdvertised(rq.svc, rq.sess.ProtocolVersion) || rq.svc.Tasks != config.TasksPassthrough {
			// §11.5: off answers -32601; durable gate-owned tasks are M3 WP-06's (until then the service advertises
			// none, as off does); a 2025-06-18 session has no tasks.
			writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "tasks are not supported on this service", nil)
			return
		}
		op = hooks.OpTask
	}
	if ref := h.admit(rq, op, name); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	hash := rq.sess.Hash
	meter := &callMeter{
		start: rq.start, task: task,
		call: Call{TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name, Tool: name, BuyerID: rq.p.BuyerID,
			Principal: rq.p.ID, MCPSession: &hash},
	}
	h.forward(rq, t, meter)
}

// admit consults Admission for op when hooks.admission.operations lists it (§13.1): a refusal's error as it was
// given (else -32603 "admission denied"); a quote for a session, a call or a passthrough task is a deny with -32603
// "priced tasks need tasks: durable"; a hook that fails or answers nothing usable is 503 with -32603 "admission
// unavailable" (it fails closed).
func (h *Handler) admit(rq *request, op hooks.Operation, tool string) *mcpwire.Refusal {
	if !slices.Contains(h.deps.AdmissionOps, op) {
		return nil
	}
	ref := rq.svc.Ref
	ar := hooks.AdmitRequest{
		Operation: op,
		Service:   hooks.Service{TenantID: ref.TenantID.String(), Tenant: ref.Tenant, Name: ref.Name, Kind: config.KindMCP},
		Principal: hooks.Principal{Kind: string(rq.p.Kind), ID: rq.p.ID, BuyerID: rq.p.BuyerID.String(), Via: string(rq.p.Via)},
		Protocol:  "mcp",
		Tool:      tool,
	}
	if rq.p.TenantID != nil {
		ar.Principal.TenantID = rq.p.TenantID.String()
	}
	if op == hooks.OpTask {
		id, err := uuid.NewV7()
		if err != nil {
			return refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
		}
		ar.TaskID = id.String()
	}
	d, err := h.deps.Admission.Admit(rq.ctx(), ar)
	if err != nil {
		h.log.Warn("mcp: admission failed; refused (it fails closed)", "service", ref.Name, "operation", op, "error", err)
		return refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
	}
	switch d.Outcome {
	case hooks.Allow:
		return nil
	case hooks.Quote:
		return refusal(http.StatusOK, rq.msg.ID, mcpwire.CodeInternal, "priced tasks need tasks: durable")
	case hooks.Deny:
		code, msg := mcpwire.CodeInternal, "admission denied"
		if d.Refusal != nil {
			if d.Refusal.Code != 0 {
				code = d.Refusal.Code
			}
			if d.Refusal.Message != "" {
				msg = d.Refusal.Message
			}
		}
		return refusal(http.StatusOK, rq.msg.ID, code, msg)
	}
	return refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
}

// refusal is a JSON-RPC error answer with status.
func refusal(status int, id json.RawMessage, code int, msg string) *mcpwire.Refusal {
	b, err := mcpwire.ErrorBody(id, code, msg, nil)
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, code, msg, nil)
	}
	return &mcpwire.Refusal{Status: status, Body: b}
}

// stringMember is obj's member key when it is a JSON string, read with exact keys.
func stringMember(obj json.RawMessage, key string) (string, bool) {
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

// clientCapabilities is params.capabilities of a client's initialize (mcp_sessions.client_capabilities).
func clientCapabilities(params json.RawMessage) []byte {
	if c, ok := mcpwire.Member(params, "capabilities"); ok && firstByte(c) == '{' {
		return c
	}
	return []byte("{}")
}

// firstByte is the first byte of raw that is not JSON white space, 0 when there is none.
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

func hexHash(h [32]byte) string { return (&Session{Hash: h}).HashHex() }

// backendName is the target's kind for the log.
func backendName(t target) string {
	switch {
	case t.direct != nil:
		return "direct"
	case t.peerAddr != "":
		return "peer"
	}
	return "tunnel"
}
