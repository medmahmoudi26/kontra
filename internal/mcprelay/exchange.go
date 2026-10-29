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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/peer"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// target is where an exchange goes: a direct backend, a tunnel session this replica holds, or another replica's
// peer listener for a tunnel session it holds (§11.2, D163).
type target struct {
	direct   upstream.Backend
	tunnel   upstream.Backend
	peerAddr string
	session  uuid.UUID // the tunnel session (tunnel or peer)
	replica  string    // the replica holding it; "" for a direct backend (mcp_sessions.replica_id, D179)
}

// tunnelled reports whether a node answers on this target (Kontra-Node is then honoured, tunnel.md §8.2).
func (t target) tunnelled() bool { return t.direct == nil }

// goneError is a tunnel session that §6.1's check refused, or that ended during the exchange: a pinned row closes
// with reason and the client gets 404 (§11.2).
type goneError struct {
	reason CloseReason
	err    error
}

func (e *goneError) Error() string {
	return fmt.Sprintf("mcprelay: the tunnel session is gone (%s): %v", e.reason, e.err)
}
func (e *goneError) Unwrap() error { return e.err }

// errUnchecked is a key_scope check that could not read the key's state (tunnelgate.ErrCheckUnavailable): the
// request is refused with 503 and the pinned row stays open.
var errUnchecked = errors.New("mcprelay: the tunnel session's key could not be checked")

// unreachableError is an upstream (or a peer replica) that could not be reached: 502, nothing closed.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return "mcprelay: upstream unreachable: " + e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// classifyTunnel turns a tunnel registry's refusal into goneError (revoked or tunnel_lost) or errUnchecked.
func classifyTunnel(err error) error {
	switch {
	case errors.Is(err, tunnelgate.ErrCheckUnavailable):
		return fmt.Errorf("%w: %w", errUnchecked, err)
	case errors.Is(err, tunnelgate.ErrRevoked):
		return &goneError{reason: CloseRevoked, err: err}
	default:
		return &goneError{reason: CloseTunnelLost, err: err}
	}
}

// forwardHeaders are the client request headers the gate forwards upstream on a legacy request (§11.1: Accept,
// Content-Type and MCP-Protocol-Version; Last-Event-ID on a pinned GET, §11.2). Authorization never.
func forwardHeaders(r *http.Request, sess *Session) http.Header {
	h := http.Header{}
	for _, k := range []string{"Accept", "Content-Type", mcpwire.HeaderProtocolVersion} {
		if v := r.Header.Values(k); len(v) > 0 {
			h[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	if r.Method == http.MethodGet {
		if v := r.Header.Get(mcpwire.HeaderLastEventID); v != "" {
			h.Set(mcpwire.HeaderLastEventID, v)
		}
	}
	// §11.1: an absent MCP-Protocol-Version means the session's negotiated version, which the upstream is told.
	if sess != nil && h.Get(mcpwire.HeaderProtocolVersion) == "" && sess.ProtocolVersion != "" {
		h.Set(mcpwire.HeaderProtocolVersion, sess.ProtocolVersion)
	}
	return h
}

// send makes one upstream request on t: method and body as given, hdr as the forwarded headers, the upstream's
// Mcp-Session-Id when there is one, and x as the tunnel preamble's MCP fields (tunnel.md §8.1). Its errors are
// goneError, errUnchecked, unreachableError or the request context's.
func (h *Handler) send(ctx context.Context, t target, rq *request, method string, body []byte, hdr http.Header,
	upstreamSID string, x upstream.MCPExchange) (*http.Response, error) {
	var rdr io.Reader = http.NoBody
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	if t.peerAddr != "" {
		if h.deps.Peer == nil {
			return nil, &unreachableError{errors.New("no peer client")}
		}
		req, err := http.NewRequestWithContext(ctx, method, "/", rdr)
		if err != nil {
			return nil, err
		}
		req.Header = hdr.Clone()
		a := peer.Assertion{
			Replica: h.deps.ReplicaID, TenantID: rq.svc.Ref.TenantID, Service: rq.svc.Ref.Name, TunnelSession: t.session,
			Principal: rq.p.ID, BuyerID: rq.p.BuyerID, Via: string(rq.p.Via),
			MCPSession: x.Session, MCPRequestID: x.RequestID, UpstreamSID: upstreamSID,
		}
		resp, err := h.deps.Peer.Forward(ctx, t.peerAddr, a, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &unreachableError{err}
		}
		if gone := resp.Header.Get(peer.HeaderGone); gone != "" && resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			reason := CloseTunnelLost
			if gone == peer.GoneRevoked {
				reason = CloseRevoked
			}
			return nil, &goneError{reason: reason, err: fmt.Errorf("the holding replica %s refused the session", t.replica)}
		}
		return resp, nil
	}
	be := t.direct
	if be == nil {
		be = t.tunnel
		ctx = upstream.WithMCP(ctx, x)
	}
	req, err := http.NewRequestWithContext(ctx, method, be.Base().String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header = hdr.Clone()
	if upstreamSID != "" {
		req.Header.Set(mcpwire.HeaderSessionID, upstreamSID)
	}
	resp, err := be.HTTPClient().Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case t.tunnel != nil && errors.Is(err, upstream.ErrTunnelLost):
			return nil, classifyTunnel(err)
		}
		return nil, &unreachableError{err}
	}
	return resp, nil
}

// callMeter is the calls row of a tools/call being relayed (§11.4). The row is written when the response completes
// (record), so only for a call some answer came back for: the upstream's, or one its node wrote (Kontra-Node). A
// call whose exchange failed before any answer reached the gate has none, wherever it failed: §6.1's check refusing
// the session on this replica or on the one holding it (peer.HeaderGone), no peer hop, a key that could not be
// checked, an upstream or a peer replica that could not be reached or was lost first, a client that left first.
// Metering reads the outcome the client reads (D172): the first response to the call's id, from a message Parse
// accepted, in an event a client dispatches.
type callMeter struct {
	call     Call
	start    time.Time
	task     bool // a passthrough task-augmented call: its row is written with is_error NULL (§11.5)
	seen     bool // the response to the call was read, or the call failed
	isError  *bool
	disabled bool
}

// observe reads the response to the call: is_error true for a JSON-RPC error or a top-level isError true; for a
// passthrough task-augmented call a CreateTaskResult is not observed (NULL). Only the first response counts, as a
// client takes the first response for its request's id (the relay ends the stream before a second one, relaySSE).
func (c *callMeter) observe(m *mcpwire.Message) {
	if c == nil || c.seen {
		return
	}
	c.seen = true
	t, f := true, false
	switch {
	case m.Error != nil:
		c.isError = &t
	case c.task && hasMember(m.Result, "task"):
		c.isError = nil
	default:
		c.isError = &f
		if set, ok := mcpwire.IsErrorFlag(m.Result); set || !ok {
			c.isError = &t
		}
	}
}

// fail records a call whose response was not relayed (an upstream message refused, a stream cut).
func (c *callMeter) fail() {
	if c == nil {
		return
	}
	t := true
	c.seen, c.isError = true, &t
}

func hasMember(obj json.RawMessage, key string) bool {
	_, ok := mcpwire.Member(obj, key)
	return ok
}

// record writes the call's row once its response has completed (callMeter says when there is one): a call whose
// answer started but whose response to the call never came (a stream that ended or was cut first) is an error.
func (h *Handler) record(rq *request, c *callMeter, bytesOut int64) {
	if c == nil || c.disabled {
		return
	}
	if !c.seen {
		c.fail()
	}
	c.call.BytesIn = int64(len(rq.body))
	c.call.BytesOut = bytesOut
	c.call.Duration = h.deps.now().Sub(c.start)
	c.call.IsError = c.isError
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
	defer cancel()
	if err := h.deps.Meter.Record(ctx, c.call); err != nil {
		h.log.Error("mcp: a calls row was not written", "service", c.call.Service, "error", err)
	}
}

// relayOpts says how a response is relayed.
type relayOpts struct {
	sid    string          // the Mcp-Session-Id the client gets ("" none)
	method string          // the client's request method ("" for a notification, a response, GET, DELETE)
	reqID  json.RawMessage // the client's request id
	sealed bool            // the service's sealed flag (legacy: D162)
	tunnel bool            // a node answered (Kontra-Node is honoured)
	meter  *callMeter
	tools  ToolsKey
	hold   bool // initialize: the headers wait for onResponse
	// status is the upstream's HTTP status, set when the response is relayed (relayResponse), for onResponse: a
	// 2026-07-28 request's virtual session opens on a 2xx answer only (§11.12, D177).
	status int
	// answered is set once the upstream's response to the client's request was relayed: JSON-RPC answers a request
	// once, and a client takes the first response for an id, so the relay ends the stream before another.
	answered bool
	// onResponse sees the upstream's response to the client's request before it is written (initialize): it
	// returns the message to write instead, or a refusal that is written in place of the whole response.
	onResponse func(m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal)
}

// countingWriter counts what reaches the client (bytes_out).
type countingWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// relayResponse relays an upstream response to the client (§11.1–§11.4) and returns the bytes written:
//
//   - a response a node wrote itself (Kontra-Node, a tunnel only): not_hosted and kind_mismatch end a pinned
//     session (tunnel_lost, 404); full is 503; any other is 502;
//   - 401, 403 or 407 is 502 with -32603 "upstream refused": the only WWW-Authenticate a client sees is the gate's;
//   - a 404 on a pinned session closes it upstream_closed and is relayed;
//   - a JSON body is read within §6.6's limit and decoded strictly (Parse, FromUpstream); one that fails is 502
//     with -32603 and nothing of it is relayed; otherwise it is scrubbed (§2.5) and relayed;
//   - an event stream is relayed event by event, each decoded and scrubbed, and ends before an event that fails;
//   - headers: Content-Type and Cache-Control only, Mcp-Session-Id the gate's.
func (h *Handler) relayResponse(rq *request, resp *http.Response, o *relayOpts) int64 {
	defer resp.Body.Close()
	w := rq.w
	o.status = resp.StatusCode
	if o.tunnel {
		if reason := tunnel.NodeReason(strings.TrimSpace(resp.Header.Get(tunnel.HeaderNode))); reason != "" {
			o.meter.fail()
			switch reason {
			case tunnel.NodeNotHosted, tunnel.NodeKindMismatch:
				if rq.sess != nil {
					h.closeRow(rq, CloseTunnelLost)
					writeError(w, http.StatusNotFound, nil, codeNoSession, msgNoSession, nil)
					return 0
				}
				writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUnreachable, nil)
			case tunnel.NodeFull:
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusServiceUnavailable, o.reqID, mcpwire.CodeInternal, "the node is busy", nil)
			case tunnel.NodeSealedRejected:
				// ext-sealed.md §6: the hosting node refused a sealed part of the call. The JSON-RPC error is relayed
				// to the client, as the node wrote it (-32602 "sealed part rejected: <reason>", the reason only when
				// it is one of §6's), the calls row says is_error, and the audit chain records the rejection.
				// SPEC-CONFLICT (D202; ext-sealed.md §6, tunnel.md §8.2, the M3 plan's "the JSON-RPC error is
				// relayed"): §6's 400 is the node's answer to the gate, and nothing says which status the client
				// gets; a Streamable HTTP client takes a 400 as a transport failure and may not read the body. The
				// closest faithful behaviour: the error is relayed as a JSON-RPC error response with 200, the
				// request's id and the session header, as the gate answers any other failed call.
				reason := upstream.SealedRejectReason(resp.Header.Get(tunnel.HeaderSealedReject))
				msg := "sealed part rejected"
				if reason != "" {
					msg += ": " + reason
				}
				tool := ""
				if o.meter != nil {
					tool = o.meter.call.Tool
				}
				h.auditSealed(rq.ctx(), rq, tool, reason, "")
				if o.sid != "" {
					w.Header().Set(mcpwire.HeaderSessionID, o.sid)
				}
				writeError(w, http.StatusOK, o.reqID, mcpwire.CodeInvalidParams, msg, nil)
			default:
				writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUnreachable, nil)
			}
			return 0
		}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		o.meter.fail()
		writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamRefused, nil)
		return 0
	case http.StatusNotFound:
		if rq.sess != nil {
			h.closeRow(rq, CloseUpstreamClosed)
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mt {
	case "text/event-stream":
		return h.relaySSE(rq, resp, o)
	case "application/json":
		return h.relayJSON(rq, resp, o)
	}
	// Anything else: a body MCP does not define. A 2xx with one is not relayed; a status is.
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, upstream.MaxBody))
	if resp.StatusCode/100 == 2 && n > 0 {
		o.meter.fail()
		writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
		return 0
	}
	if resp.StatusCode/100 != 2 {
		o.meter.fail()
	}
	if o.hold {
		o.sid = "" // an initialize answered by a bare status opens no session
	}
	h.writeHeaders(w, resp, o.sid, false)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(resp.StatusCode)
	return 0
}

// writeHeaders writes the response headers the client may see: Content-Type and Cache-Control from the upstream,
// and the gate's Mcp-Session-Id (§11.1). With status set it also writes the status line.
func (h *Handler) writeHeaders(w http.ResponseWriter, resp *http.Response, sid string, _ bool) {
	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if sid != "" {
		w.Header().Set(mcpwire.HeaderSessionID, sid)
	}
}

// relayJSON relays a JSON response.
func (h *Handler) relayJSON(rq *request, resp *http.Response, o *relayOpts) int64 {
	w := rq.w
	b, err := io.ReadAll(io.LimitReader(resp.Body, upstream.MaxBody+1))
	if err != nil || len(b) > upstream.MaxBody {
		o.meter.fail()
		h.log.Warn("mcp: an upstream JSON body was not read", "service", rq.svc.Ref.Name, "error", err, "over_limit", len(b) > upstream.MaxBody)
		writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
		return 0
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if resp.StatusCode/100 != 2 || o.hold {
			o.meter.fail()
		}
		if o.hold {
			writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
			return 0
		}
		h.writeHeaders(w, resp, o.sid, false)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return 0
	}
	m, err := mcpwire.Parse(b, mcpwire.FromUpstream)
	if err != nil {
		o.meter.fail()
		h.log.Warn("mcp: an upstream message was not relayed: strict decoding refused it", "service", rq.svc.Ref.Name, "level", strictLevel(err))
		writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
		return 0
	}
	if o.hold && (m.Method != "" || !sameID(m.ID, o.reqID)) {
		o.sid = "" // not the answer to the initialize: no session is opened, and the client gets no id
	}
	if rq.r.Method == http.MethodGet && m.Method == "" && len(m.ID) > 0 {
		h.log.Warn("mcp: an upstream answered a GET with a response; it is not relayed (D265)", "service", rq.svc.Ref.Name)
		writeError(w, http.StatusBadGateway, nil, mcpwire.CodeInternal, msgUpstreamError, nil)
		return 0
	}
	out, ref := h.process(o, m, b)
	if ref != nil {
		writeRefusal(w, ref)
		return 0
	}
	h.writeHeaders(w, resp, o.sid, false)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	cw := &countingWriter{w: w}
	_, _ = cw.Write(out)
	return cw.n
}

// process is one upstream message the relay forwards: the response to the client's request through onResponse
// when there is one, every other message scrubbed (§2.5); notifications/tools/list_changed drops the service's
// cached tools (§11.3); the call's meter reads the response.
func (h *Handler) process(o *relayOpts, m *mcpwire.Message, raw []byte) ([]byte, *mcpwire.Refusal) {
	isResp := m.Method == "" && sameID(m.ID, o.reqID)
	if isResp {
		o.answered = true
		if o.method == mcpwire.MethodToolsCall && m.Result != nil {
			if _, ok := mcpwire.IsErrorFlag(m.Result); !ok {
				// A top-level isError that is not a JSON boolean ("true", 1, null): clients disagree on it (pydantic
				// reads "true" as true, the TypeScript SDK's schema refuses the result), so it is a malformed upstream
				// message, not relayed and metered an error, the value-level twin of D172's key rule.
				o.meter.fail()
				h.log.Warn("mcp: an upstream result's isError is not a boolean; it is not relayed")
				b, _ := mcpwire.ErrorBody(o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
				return nil, &mcpwire.Refusal{Status: http.StatusBadGateway, Body: b}
			}
		}
		o.meter.observe(m)
		if o.onResponse != nil {
			return o.onResponse(m, raw)
		}
	}
	if m.Method == "notifications/tools/list_changed" {
		h.deps.Tools.Invalidate(o.tools)
		if h.deps.ToolsChanged != nil {
			h.deps.ToolsChanged(o.tools)
		}
	}
	reqMethod := ""
	if isResp {
		reqMethod = o.method
	}
	out, err := mcpwire.ScrubMessage(mcpwire.FromUpstream, raw, m, reqMethod, o.sealed)
	if err != nil {
		h.log.Warn("mcp: an upstream message could not be scrubbed; it is not relayed", "error", err)
		b, _ := mcpwire.ErrorBody(o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
		return nil, &mcpwire.Refusal{Status: http.StatusBadGateway, Body: b}
	}
	return out, nil
}

// keepAliveEvery is how long a relayed event stream may stay silent before the gate writes a comment (§2.2).
const keepAliveEvery = 15 * time.Second

// relaySSE relays an event stream event by event, flushing each. An event whose data Parse refuses ends the
// stream before it (§11.1). With o.hold the headers and the events wait for the response to the client's request
// (onResponse decides it), within upstream.MaxBody.
//
// SPEC-CONFLICT (D197; relay §11.1, §11.2, §11.4; D172): the spec says what happens to an event whose message fails
// strict decoding, and that events otherwise pass unchanged, but not what happens to an event a client does not
// dispatch, nor to a second response to one request, both of which let metering read another is_error than the
// client reads. The closest faithful behaviour: the relay reads, meters and relays what a client reads, the events a
// client dispatches as messages (type absent or "message", event.message). An event of another type is no MCP
// message to any client; it is neither read nor relayed, so a seller cannot meter an answer in it that its client
// never sees, nor carry Kontra's keys past the scrub in it. A second response to the client's request ends the
// stream before it, as a refused message does, and the call stays metered as its first response says (a client takes
// the first response for an id, and reports a later one as unknown).
//
// A POST's stream is relayed without its event ids, so that no client resumes it with a GET: the calls row is written
// from what the POST's stream carried, and a response the client read on a GET instead would be one the gate never
// metered (a buyer that drops every call's stream after its first event and resumes would have every successful call
// metered an error, D196). A GET's stream carries no response for the same reason (MCP: a server sends none there
// unless the client resumes a POST's stream, which the gate does not offer): the stream ends before one, so that a
// seller cannot have the client take a response on it while the POST's stream is metered another (D265).
func (h *Handler) relaySSE(rq *request, resp *http.Response, o *relayOpts) int64 {
	type item struct {
		ev  *event
		err error
	}
	ch := make(chan item)
	done := make(chan struct{})
	defer close(done)
	go func() {
		r := newSSEReader(resp.Body)
		for {
			ev, err := r.next()
			select {
			case ch <- item{ev, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	w := rq.w
	cw := &countingWriter{w: w}
	rc := http.NewResponseController(w)
	out := false
	start := func() {
		h.writeHeaders(w, resp, o.sid, false)
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_ = rc.Flush()
		out = true
	}
	write := func(b []byte) bool {
		if _, err := cw.Write(b); err != nil {
			return false
		}
		err := rc.Flush()
		return err == nil || errors.Is(err, http.ErrNotSupported)
	}
	if !o.hold {
		start()
	}
	var held [][]byte
	heldSize := 0
	refuse := func() int64 {
		o.meter.fail()
		if !out {
			writeError(w, http.StatusBadGateway, o.reqID, mcpwire.CodeInternal, msgUpstreamError, nil)
		}
		return cw.n
	}
	tick := time.NewTicker(keepAliveEvery)
	defer tick.Stop()
	for {
		select {
		case <-rq.ctx().Done():
			return cw.n
		case <-tick.C:
			if out && !write([]byte(keepAlive)) {
				return cw.n
			}
		case it := <-ch:
			if it.err != nil {
				if !errors.Is(it.err, io.EOF) {
					h.log.Debug("mcp: an upstream event stream ended", "service", rq.svc.Ref.Name, "error", it.err)
				}
				if !out {
					return refuse()
				}
				return cw.n
			}
			ev := it.ev
			if !ev.message() {
				h.log.Debug("mcp: an upstream event of a type no client dispatches was not relayed", "service", rq.svc.Ref.Name)
				continue
			}
			tick.Reset(keepAliveEvery)
			if rq.r.Method == http.MethodPost {
				ev.dropID() // a POST's stream is not resumable through the gate (D265)
			}
			b := ev.encode(false, "")
			isResp := false
			if ev.hasData && strings.TrimSpace(ev.data) != "" {
				m, err := mcpwire.Parse([]byte(ev.data), mcpwire.FromUpstream)
				if err != nil {
					h.log.Warn("mcp: an upstream event was not relayed: strict decoding refused it; the stream ends",
						"service", rq.svc.Ref.Name, "level", strictLevel(err))
					return refuse()
				}
				if rq.r.Method == http.MethodGet && m.Method == "" && len(m.ID) > 0 {
					h.log.Warn("mcp: an upstream sent a response on a GET stream; the stream ends before it (D265)",
						"service", rq.svc.Ref.Name)
					return cw.n
				}
				isResp = m.Method == "" && sameID(m.ID, o.reqID)
				if isResp && o.answered {
					h.log.Warn("mcp: an upstream answered a request twice; the stream ends before the second answer",
						"service", rq.svc.Ref.Name)
					return cw.n
				}
				msg, ref := h.process(o, m, []byte(ev.data))
				if ref != nil {
					if !out {
						writeRefusal(w, ref)
					}
					return cw.n
				}
				if string(msg) != ev.data {
					b = ev.encode(true, string(msg))
				}
			}
			if !out {
				held = append(held, b)
				if heldSize += len(b); heldSize > upstream.MaxBody {
					return refuse()
				}
				if !isResp {
					continue
				}
				start()
				for _, hb := range held {
					if !write(hb) {
						return cw.n
					}
				}
				held = nil
				continue
			}
			if !write(b) {
				return cw.n
			}
		}
	}
}

// sameID reports whether a response's id a answers a request whose id is b, as stock clients match them
// (mcpwire.SameID).
func sameID(a, b json.RawMessage) bool { return mcpwire.SameID(a, b) }

// idString is a JSON-RPC id as the tunnel preamble's mcp_request_id carries it (tunnel.md §8.1): a string's
// value, a number as written.
func idString(id json.RawMessage) string {
	id = bytes.TrimSpace(id)
	if len(id) > 0 && id[0] == '"' {
		var s string
		if json.Unmarshal(id, &s) == nil {
			return s
		}
	}
	return string(id)
}

// closeRow closes the request's pinned session with reason.
func (h *Handler) closeRow(rq *request, reason CloseReason) {
	if rq.sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
	defer cancel()
	if closed, err := h.deps.Sessions.Close(ctx, rq.sess.Hash, reason); err != nil {
		h.log.Warn("mcp: a session row was not closed", "service", rq.svc.Ref.Name, "reason", reason, "error", err)
	} else if closed {
		h.log.Info("mcp session closed", "service", rq.svc.Ref.Name, "reason", reason)
	}
}

// RunExchange implements peer.Runner on the replica holding the tunnel session (§11.2): §6.1's check on this
// replica's registry, then the one exchange on its tunnel with the preamble's MCP fields from the assertion, the
// upstream's Mcp-Session-Id from it and only the headers it signed, and the response streamed back unchanged. It
// never reads mcp_sessions: every other rule was applied by the receiving replica, which also closes the row when
// the session is refused here (404 with peer.HeaderGone).
func (h *Handler) RunExchange(ctx context.Context, a peer.Assertion, req *http.Request, w http.ResponseWriter) error {
	be, err := h.deps.Tunnels.MCPBackend(a.TunnelSession, a.TenantID, a.Service)
	if err != nil {
		peerRefuse(w, classifyTunnel(err))
		return err
	}
	var body []byte
	if req.Body != nil {
		if body, err = io.ReadAll(req.Body); err != nil {
			return err
		}
	}
	var rdr io.Reader = http.NoBody
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(upstream.WithMCP(ctx, upstream.MCPExchange{Session: a.MCPSession, RequestID: a.MCPRequestID}),
		req.Method, be.Base().String(), rdr)
	if err != nil {
		return err
	}
	out.Header = req.Header.Clone()
	out.Header.Del(mcpwire.HeaderSessionID)
	if a.UpstreamSID != "" {
		out.Header.Set(mcpwire.HeaderSessionID, a.UpstreamSID)
	}
	resp, err := be.HTTPClient().Do(out)
	if err != nil {
		if errors.Is(err, upstream.ErrTunnelLost) {
			peerRefuse(w, classifyTunnel(err))
		} else {
			writeEmpty(w, http.StatusBadGateway)
		}
		return err
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive", peer.HeaderGone:
			continue
		}
		w.Header()[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	// The headers go back at once: an event stream may stay idle for long (a GET stream before its first message),
	// and the receiving replica answers its client only once it has them.
	_ = rc.Flush()
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// peerRefuse is the holding replica's answer to a session §6.1's check refused: 404 naming the reason, or 503 when
// the check could not be made.
func peerRefuse(w http.ResponseWriter, err error) {
	var g *goneError
	switch {
	case errors.As(err, &g):
		v := peer.GoneTunnelLost
		if g.reason == CloseRevoked {
			v = peer.GoneRevoked
		}
		w.Header().Set(peer.HeaderGone, v)
		writeEmpty(w, http.StatusNotFound)
	default:
		writeEmpty(w, http.StatusServiceUnavailable)
	}
}

// SweepIdle closes the sessions idle past their service's session_idle_s that this replica sweeps
// (Sessions.SweepIdle) and sends each upstream a best-effort DELETE (§11.2), on the direct backend or this
// replica's tunnel session. It returns how many it closed.
func (h *Handler) SweepIdle(ctx context.Context) (int, error) {
	rows, err := h.deps.Sessions.SweepIdle(ctx, h.deps.ReplicaID, h.deps.now())
	if err != nil {
		return 0, err
	}
	for _, s := range rows {
		if s.UpstreamSID == "" {
			continue
		}
		var t target
		switch {
		case s.TunnelSession == nil:
			svc, err := h.deps.Services.MCPServiceByID(ctx, s.TenantID, s.Service)
			if err != nil || svc == nil || svc.Direct == nil {
				continue
			}
			t.direct = svc.Direct
		case s.ReplicaID == h.deps.ReplicaID && h.deps.Tunnels != nil:
			be, err := h.deps.Tunnels.MCPBackend(*s.TunnelSession, s.TenantID, s.Service)
			if err != nil {
				continue
			}
			t.tunnel, t.session = be, *s.TunnelSession
		default:
			continue
		}
		h.deleteUpstream(context.WithoutCancel(ctx), t, s)
	}
	if len(rows) > 0 {
		h.log.Info("mcp sessions closed after their idle time", "sessions", len(rows))
	}
	return len(rows), nil
}

// deleteUpstream sends a best-effort DELETE for an upstream session the gate closed (idle, a refused negotiation),
// within 10 s, on its own goroutine.
func (h *Handler) deleteUpstream(ctx context.Context, t target, s Session) {
	go h.deleteUpstreamNow(ctx, t, s)
}

// deleteUpstreamNow is deleteUpstream's DELETE, made before it returns (within 10 s).
func (h *Handler) deleteUpstreamNow(ctx context.Context, t target, s Session) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The session's own principal, which a peer hop's assertion names to the holding replica.
	rq := &request{r: (&http.Request{}).WithContext(ctx), svc: &Service{}, sess: &s,
		p: auth.Principal{ID: s.Principal, BuyerID: s.BuyerID, Via: auth.Via(s.Via)}}
	rq.svc.Ref.TenantID, rq.svc.Ref.Name = s.TenantID, s.Service
	hdr := http.Header{}
	if s.ProtocolVersion != "" {
		hdr.Set(mcpwire.HeaderProtocolVersion, s.ProtocolVersion)
	}
	resp, err := h.send(ctx, t, rq, http.MethodDelete, nil, hdr, s.UpstreamSID, upstream.MCPExchange{Session: s.HashHex()})
	if err != nil {
		h.log.Debug("mcp: the upstream DELETE of a closed session failed", "service", s.Service, "error", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// capped reports whether max_sessions_per_principal applies to rq's new session (§11.2, D213, D231): a positive cap,
// a tunnel service (a direct backend is an HTTP server the operator runs, with no stdio children to share), and a
// principal other than anonymous (its callers are distinct clients the gate cannot tell apart: one budget for all of
// them would let any of them crowd out, or close, the others' sessions).
func capped(rq *request) bool {
	return rq.svc.MaxSessionsPerPrincipal > 0 && rq.svc.Direct == nil && rq.p.Authenticated()
}

// capacity is max_sessions_per_principal's check of rq's new session, now.
func (h *Handler) capacity(rq *request) Capacity {
	return Capacity{Limit: rq.svc.MaxSessionsPerPrincipal, Busy: h.busyHashes(), Now: h.deps.now()}
}

// msgTooManySessions is the message of the 429 an initialize beyond max_sessions_per_principal gets (D231).
const msgTooManySessions = "too many sessions"

// tooManySessions is the refusal of an initialize beyond max_sessions_per_principal when too few of the principal's
// sessions are idle to make room (D231): 429 with Retry-After and -32600, as the edge's in-flight caps (§2.2).
func tooManySessions(w http.ResponseWriter, rq *request, err error) *mcpwire.Refusal {
	retry := EvictIdle
	var te *TooManySessionsError
	if errors.As(err, &te) {
		retry = te.RetryAfter
	}
	// When a retry may find room: once enough of the principal's sessions will have been idle for EvictIdle (D249).
	w.Header().Set("Retry-After", strconv.Itoa(max(int((retry+time.Second-1)/time.Second), 1)))
	b, err := mcpwire.ErrorBody(rq.id(), mcpwire.CodeInvalidRequest, msgTooManySessions,
		map[string]any{"max_sessions_per_principal": rq.svc.MaxSessionsPerPrincipal})
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, mcpwire.CodeInvalidRequest, msgTooManySessions, nil)
	}
	return &mcpwire.Refusal{Status: http.StatusTooManyRequests, Body: b}
}

// makeRoom applies max_sessions_per_principal before the upstream initialize (§11.2, D213, D231): when the
// principal's open sessions of the service leave no room for the new one, its sessions idle for EvictIdle (none in
// flight on any replica) are closed, the longest idle first, each with a DELETE on its pinned target made before
// makeRoom returns, so that an upstream with a bounded number of sessions (the stdio bridge's --max-sessions
// children) has the room before the new initialize reaches it; with too few such sessions it closes none and is
// ErrTooManySessions.
func (h *Handler) makeRoom(rq *request) error {
	if !capped(rq) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
	defer cancel()
	rows, err := h.deps.Sessions.Evict(ctx, rq.svc.Ref.TenantID, rq.svc.Ref.Name, rq.p.ID, h.capacity(rq))
	if err != nil {
		return err
	}
	h.deleteEvicted(ctx, rq, rows)
	return nil
}

// openSession writes the new session's row: under max_sessions_per_principal when it applies (OpenCapped, checked
// again for concurrent initializes of the principal; a session it closes gets its DELETE on its own goroutine).
func (h *Handler) openSession(ctx context.Context, rq *request, clientID string, sess Session) error {
	if !capped(rq) {
		return h.deps.Sessions.Open(ctx, clientID, sess)
	}
	rows, err := h.deps.Sessions.OpenCapped(ctx, clientID, sess, h.capacity(rq))
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(rq.ctx()), 10*time.Second)
			defer cancel()
			h.deleteEvicted(ctx, rq, rows)
		}()
	}
	return nil
}

// deleteEvicted sends each session max_sessions_per_principal closed a DELETE on its pinned target (this replica's
// tunnel session or the holder's peer listener) and waits for them. A session whose target is gone is closed all the
// same: its upstream session dies with its tunnel.
func (h *Handler) deleteEvicted(ctx context.Context, rq *request, rows []Session) {
	var wg sync.WaitGroup
	for _, s := range rows {
		h.log.Info("mcp: a principal's session idle for EvictIdle is closed for its new one (max_sessions_per_principal)",
			"service", rq.svc.Ref.Name, "principal", rq.p.ID, "max_sessions_per_principal", rq.svc.MaxSessionsPerPrincipal,
			"holder", s.ReplicaID)
		if s.UpstreamSID == "" {
			continue
		}
		erq := &request{r: (&http.Request{}).WithContext(ctx), svc: rq.svc, sess: &s}
		t, err := h.pinnedTarget(erq)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.deleteUpstreamNow(ctx, t, s)
		}()
	}
	wg.Wait()
}
