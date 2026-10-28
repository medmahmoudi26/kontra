package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
	"kontra.sh/kontra/internal/version"
)

// The 2025-11-25 dispatch of a gate-owned MCP task (spec/relay.md §11.5 steps 3–6, D22).

// mcpLink is a dispatch's upstream: one backend (a direct URL, or a tunnel session this replica holds), the
// preamble's task and MCP fields every exchange carries (tunnel.md §8.1), and the upstream session the dispatch
// opened.
type mcpLink struct {
	backend   upstream.Backend
	tunnelled bool
	session   uuid.UUID // the tunnel session, for held()
	tenantID  uuid.UUID
	service   string
	tunnels   Tunnels
	task      upstream.ExchangeTask
	mcp       upstream.MCPExchange
	version   string // MCP-Protocol-Version once the session is initialized
	sid       string // the upstream's Mcp-Session-Id
	// modern is a 2026-07-28 dispatch's link (§11.12): every request carries MCP-Protocol-Version and the Mcp-Method
	// and Mcp-Name its body names, and a tools/call the client's Mcp-Param-* headers (params).
	modern bool
	params http.Header
}

// held reports whether a tunnel link's session is still on this replica and serving the service (§6.1's check):
// an exchange cut while it is is not a lost tunnel. A direct link is always held.
func (k *mcpLink) held() bool {
	if !k.tunnelled {
		return true
	}
	_, err := k.tunnels.MCPBackend(k.session, k.tenantID, k.service)
	return err == nil
}

// heldGrace is how long a read that failed mid-answer on a tunnelled link waits for the registry to drop a session
// that is going: its streams fail a moment before its end is noticed.
const heldGrace = time.Second

// cut classifies a read that failed mid-answer (a body or an event stream cut before the answer): on a tunnelled link
// whose session this replica no longer holds, the tunnel was lost (§6.4: tunnel_lost, after the first upstream id
// too, D178); otherwise the upstream ended it (unreachable).
func (k *mcpLink) cut(ctx context.Context, msg string) *exchangeError {
	if k.tunnelled {
		for deadline := time.Now().Add(heldGrace); ; {
			if !k.held() {
				return &exchangeError{class: classTunnelLost, msg: msg + " (the tunnel session is gone)"}
			}
			if !time.Now().Before(deadline) || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return &exchangeError{class: classUnreachable, msg: msg}
}

// Classes of a dispatch exchange's failure (spec §6.4's rows).
type exchangeClass int

const (
	classUnreachable exchangeClass = iota + 1 // refused, reset, 502/503/504, a stream that ended early
	classTunnelLost                           // the tunnel stream could not be opened or was cut; a node's answer
	classFull                                 // Kontra-Node: full
	classSealed                               // Kontra-Node: sealed_rejected
	classThrottled                            // 408/429
	classGone                                 // 404: the upstream no longer knows the dispatch's session
	classProtocol                             // 401/403/407, another 4xx, a malformed or refused message, over a limit
	classRPC                                  // the upstream's JSON-RPC error answering the request
)

// exchangeError is a dispatch exchange that failed, in the gate's own words (its text is logged, never stored).
type exchangeError struct {
	class      exchangeClass
	msg        string
	retryAfter time.Duration
	rpc        *mcpwire.Error
	raw        json.RawMessage // the JSON-RPC error object as the upstream wrote it (classRPC)
	reject     string          // classSealed: the node's Kontra-Sealed-Reject, when one of ext-sealed.md §6's reasons
}

func (e *exchangeError) Error() string {
	if e.rpc != nil {
		return fmt.Sprintf("mcprelay: the upstream answered %d", e.rpc.Code)
	}
	return "mcprelay: " + e.msg
}

func asExchange(err error) *exchangeError {
	var xe *exchangeError
	if errors.As(err, &xe) {
		return xe
	}
	return &exchangeError{class: classUnreachable, msg: err.Error()}
}

// send makes one exchange on the link: method and body, the forwarded headers a client would send (Accept,
// Content-Type, MCP-Protocol-Version once initialized) and the upstream's session id; the request's context
// carries the preamble's fields. A response the node wrote itself, and a refusal, are an *exchangeError.
func (k *mcpLink) send(ctx context.Context, method string, body []byte) (*http.Response, error) {
	var rdr io.Reader = http.NoBody
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	ctx = upstream.WithMCP(upstream.WithTask(ctx, k.task), k.mcp)
	req, err := http.NewRequestWithContext(ctx, method, k.backend.Base().String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if k.sid != "" {
		req.Header.Set(mcpwire.HeaderSessionID, k.sid)
		if k.version != "" {
			req.Header.Set(mcpwire.HeaderProtocolVersion, k.version)
		}
	}
	if k.modern {
		k.modernHeaders(req.Header, body)
	}
	resp, err := k.backend.HTTPClient().Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case k.tunnelled && errors.Is(err, upstream.ErrTunnelLost):
			return nil, &exchangeError{class: classTunnelLost, msg: "the tunnel stream could not be opened"}
		}
		return nil, &exchangeError{class: classUnreachable, msg: "the upstream could not be reached"}
	}
	if k.tunnelled {
		if reason := tunnel.NodeReason(strings.TrimSpace(resp.Header.Get(tunnel.HeaderNode))); reason != "" {
			discard(resp)
			switch reason {
			case tunnel.NodeFull:
				return nil, &exchangeError{class: classFull, msg: "the node is full"}
			case tunnel.NodeSealedRejected:
				return nil, &exchangeError{class: classSealed, msg: "the hosting node refused a sealed part",
					reject: upstream.SealedRejectReason(resp.Header.Get(tunnel.HeaderSealedReject))}
			}
			return nil, &exchangeError{class: classTunnelLost, msg: "the node answered " + string(reason)}
		}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		discard(resp)
		return nil, &exchangeError{class: classProtocol, msg: "the upstream refused the gate"}
	case http.StatusNotFound:
		discard(resp)
		return nil, &exchangeError{class: classGone, msg: "the upstream no longer knows the session"}
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		discard(resp)
		xe := &exchangeError{class: classThrottled, msg: "the upstream answered " + strconv.Itoa(resp.StatusCode)}
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			xe.retryAfter = time.Duration(s) * time.Second
		}
		return nil, xe
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		discard(resp)
		return nil, &exchangeError{class: classUnreachable, msg: "the upstream answered " + strconv.Itoa(resp.StatusCode)}
	}
	return resp, nil
}

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// rpcBody is a JSON-RPC request of the dispatch's own: method, a string id, and params (raw, or none).
func rpcBody(id, method string, params json.RawMessage) []byte {
	var b bytes.Buffer
	idb, _ := json.Marshal(id)
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(idb)
	b.WriteString(`,"method":`)
	mb, _ := json.Marshal(method)
	b.Write(mb)
	if len(params) > 0 {
		b.WriteString(`,"params":`)
		b.Write(params)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonObject is a JSON object of one member.
func jsonObject(key string, val any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{key: val})
}

// request sends a request of the dispatch's own (id a string of its own space) and returns the answer to it.
func (k *mcpLink) request(ctx context.Context, id, method string, params json.RawMessage, onMsg func(*mcpwire.Message, []byte)) (*mcpwire.Message, error) {
	m, _, _, err := k.call(ctx, rpcBody(id, method, params), json.RawMessage(strconv.Quote(id)), onMsg)
	return m, err
}

// call posts body, a request whose id is id, and reads the answer to it: a JSON body, or the event of an event
// stream that answers id (each other message of the stream, a request or notification of the upstream, goes to
// onMsg, in order). Every message is decoded strictly (Parse, FromUpstream); one that fails is classProtocol
// (C1: a dispatch fails upstream_error). A JSON-RPC error answering id is classRPC. It returns the answer, its
// bytes and the response's headers.
func (k *mcpLink) call(ctx context.Context, body []byte, id json.RawMessage, onMsg func(*mcpwire.Message, []byte)) (*mcpwire.Message, []byte, http.Header, error) {
	resp, err := k.send(ctx, http.MethodPost, body)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	answer := func(raw []byte) (*mcpwire.Message, bool, error) {
		m, err := mcpwire.Parse(raw, mcpwire.FromUpstream)
		if err != nil {
			return nil, false, &exchangeError{class: classProtocol, msg: "strict decoding refused an upstream message: " + strictLevel(err)}
		}
		if m.Method != "" || !sameID(m.ID, id) {
			if onMsg != nil {
				onMsg(m, raw)
			}
			return nil, false, nil
		}
		if m.Error != nil {
			e, _ := mcpwire.Member(raw, "error")
			return m, true, &exchangeError{class: classRPC, rpc: m.Error, raw: e}
		}
		return m, true, nil
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusBadRequest {
		discard(resp)
		return nil, nil, nil, &exchangeError{class: classProtocol, msg: "the upstream answered " + strconv.Itoa(resp.StatusCode)}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" {
		r := newSSEReader(resp.Body)
		for {
			ev, err := r.next()
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, nil, ctx.Err()
				}
				if errors.Is(err, errEventTooLarge) {
					return nil, nil, nil, &exchangeError{class: classProtocol, msg: "an upstream event over the limit"}
				}
				return nil, nil, nil, k.cut(ctx, "the upstream's event stream ended before the answer")
			}
			if !ev.message() || !ev.hasData || strings.TrimSpace(ev.data) == "" {
				continue
			}
			raw := []byte(ev.data)
			m, ok, err := answer(raw)
			if err != nil || ok {
				return m, raw, resp.Header, err
			}
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, upstream.MaxBody+1))
	switch {
	case err != nil:
		return nil, nil, nil, k.cut(ctx, "the upstream's body could not be read")
	case len(raw) > upstream.MaxBody:
		return nil, nil, nil, &exchangeError{class: classProtocol, msg: "an upstream body over the limit"}
	case mt != "application/json" || len(bytes.TrimSpace(raw)) == 0:
		return nil, nil, nil, &exchangeError{class: classProtocol, msg: "the upstream's answer is not a JSON-RPC message"}
	}
	m, ok, err := answer(raw)
	if err != nil {
		return m, raw, resp.Header, err
	}
	if !ok {
		return nil, nil, nil, &exchangeError{class: classProtocol, msg: "the upstream's answer does not answer the request"}
	}
	return m, raw, resp.Header, nil
}

// post sends a notification or a response of the dispatch's (no answer is read: 202, or a 2xx body discarded).
func (k *mcpLink) post(ctx context.Context, body []byte) error {
	resp, err := k.send(ctx, http.MethodPost, body)
	if err != nil {
		return err
	}
	ok := resp.StatusCode/100 == 2
	discard(resp)
	if !ok {
		return &exchangeError{class: classProtocol, msg: "the upstream answered " + strconv.Itoa(resp.StatusCode)}
	}
	return nil
}

// stream posts body (tasks/result) or opens the session's GET (body nil) and hands every message of its answer to
// each, in order, until the stream ends; final is set on the answer to id. It returns why the stream ended.
func (k *mcpLink) stream(ctx context.Context, body []byte, id json.RawMessage, each func(m *mcpwire.Message, raw []byte, final bool) bool) error {
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	resp, err := k.send(ctx, method, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return errNoGet
	}
	if resp.StatusCode/100 != 2 {
		return &exchangeError{class: classProtocol, msg: "the upstream answered " + strconv.Itoa(resp.StatusCode)}
	}
	one := func(raw []byte) error {
		m, err := mcpwire.Parse(raw, mcpwire.FromUpstream)
		if err != nil {
			return &exchangeError{class: classProtocol, msg: "strict decoding refused an upstream message: " + strictLevel(err)}
		}
		final := m.Method == "" && id != nil && sameID(m.ID, id)
		if !each(m, raw, final) || final {
			return io.EOF
		}
		return nil
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "text/event-stream" {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, upstream.MaxBody+1))
		if err != nil || len(raw) > upstream.MaxBody || len(bytes.TrimSpace(raw)) == 0 {
			return &exchangeError{class: classProtocol, msg: "the upstream's answer is not a JSON-RPC message"}
		}
		if err := one(raw); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	r := newSSEReader(resp.Body)
	for {
		ev, err := r.next()
		if err != nil {
			if errors.Is(err, errEventTooLarge) {
				return &exchangeError{class: classProtocol, msg: "an upstream event over the limit"}
			}
			return nil
		}
		if !ev.message() || !ev.hasData || strings.TrimSpace(ev.data) == "" {
			continue
		}
		if err := one([]byte(ev.data)); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// errNoGet is an upstream that serves no GET stream (405).
var errNoGet = errors.New("mcprelay: the upstream serves no GET stream")

// delete ends the dispatch's upstream session (best effort, within 5 s).
func (k *mcpLink) delete(ctx context.Context) {
	if k.sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if resp, err := k.send(ctx, http.MethodDelete, nil); err == nil {
		discard(resp)
	}
}

// streamMsg is one message of a stream the dispatch holds (its tasks/result, its GET), or the stream's end.
type streamMsg struct {
	from  string // "result" or "get"
	m     *mcpwire.Message
	raw   []byte
	final bool  // the answer to the tasks/result
	ended bool  // the stream ended (err why, nil when it ended cleanly)
	err   error // a message that failed strict decoding
}

// legacyRun is one 2025-11-25 dispatch: it owns the attempt's upstream session and is the only writer of the
// task's log while it runs (a client's answers aside, which it tails).
type legacyRun struct {
	d        *Dispatcher
	log      *slog.Logger
	key      relay.Key
	taskID   uuid.UUID
	n        int
	fence    *store.Fence
	row      *store.TaskRow
	workItem uuid.UUID
	work     *store.MCPWork
	answered []int64 // the input_response seqs an earlier dispatch of the attempt wrote upstream (D42)
	attempt  *store.Attempt
	link     *mcpLink
	sealed   bool
	resume   bool // the attempt has its upstream task: re-attach to it (D42)

	// cancelling is set by forward_cancel on this replica (Dispatcher.ForwardCancel) before it sends tasks/cancel:
	// the workflow writes the task's terminal event then (canceled, D57; failed: timeout, §6.5), so the run writes
	// none of its own from that moment, whatever the upstream does about the cancel (holdForWorkflow).
	cancelling *atomic.Bool

	upTask   string // the upstream task id
	interval time.Duration
	last     struct {
		state store.State
		text  string
	}
	logged     map[string]int64          // an upstream request id (mcpwire.IDKey) → the seq it was logged at
	upstreamID map[int64]json.RawMessage // an input request's seq → the upstream's id for it
	sent       map[int64]bool            // input_response seqs written upstream (by this dispatch, or recorded by one before it)
	bytes      int64
	nextN      int
	msgs       chan streamMsg
	resultOpen bool
	failing    time.Time // when the polls started failing to reach the upstream
	stop       *lifecycle.DispatchResult
}

// id is a JSON-RPC id of the dispatch's own: a string in a space of its own, never the client's id, which the
// tools/call reuses (§11.5 step 3).
func (r *legacyRun) id() string {
	for {
		r.nextN++
		s := "kontra.dispatch." + strconv.Itoa(r.nextN)
		if !sameID(json.RawMessage(strconv.Quote(s)), r.work.RequestID) {
			return s
		}
	}
}

func (r *legacyRun) gateID() string { return r.taskID.String() }

// run is the dispatch: the session and the call (or, re-attaching, the recorded ones), then the follow until
// the task is terminal. It closes its upstream session unless the attempt may be re-attached to (a lost context).
func (r *legacyRun) run(ctx context.Context) (res lifecycle.DispatchResult) {
	r.msgs = make(chan streamMsg, 64)
	r.logged, r.upstreamID, r.sent = map[string]int64{}, map[int64]json.RawMessage{}, map[int64]bool{}
	for _, seq := range r.answered {
		r.sent[seq] = true
	}
	r.interval = time.Second
	defer func() {
		if res.Outcome != "" && res.Outcome != lifecycle.DispatchContextLost {
			r.link.delete(ctx)
			return
		}
		if row, err := r.d.Log.Load(context.WithoutCancel(ctx), r.taskID); err == nil && row.State.Terminal() {
			r.link.delete(ctx)
		}
	}()
	b, err := r.d.Log.PayloadBytes(ctx, r.taskID)
	if err != nil {
		return lifecycle.DispatchResult{}
	}
	r.bytes = b
	if err := r.loadRequests(ctx); err != nil {
		return lifecycle.DispatchResult{}
	}
	if r.resume {
		r.upTask = r.attempt.UpstreamTaskID
		r.log.Info("re-attaching to the upstream task", "upstream_session_set", r.link.sid != "")
	} else if res, done := r.start(ctx); done {
		return res
	}
	sctx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()
	go r.holdGet(sctx)
	return r.loop(ctx)
}

// loadRequests reads the input requests the task has logged (a re-attach finds those of an earlier dispatch).
func (r *legacyRun) loadRequests(ctx context.Context) error {
	reqs, err := r.d.Log.InputRequests(ctx, r.taskID, 0)
	if err != nil {
		return err
	}
	for _, q := range reqs {
		if k, ok := mcpwire.IDKey(q.UpstreamID); ok {
			r.logged[k] = q.Seq
		}
		r.upstreamID[q.Seq] = q.UpstreamID
	}
	return nil
}

// start opens the dispatch's own upstream session and sends the task-augmented call (§11.5 steps 3–4): an
// initialize declaring the client's elicitation and sampling capabilities (D22), its session recorded on the
// attempt, notifications/initialized, then the client's tools/call under the client's JSON-RPC id with task.ttl
// the time to the deadline. A CreateTaskResult's taskId is the first upstream task id (kontra.dispatched); an
// answer that is the call's result, or a JSON-RPC error, ends the task (§11.8 case 1). A failure before the
// first upstream task id follows §6.4's table.
func (r *legacyRun) start(ctx context.Context) (lifecycle.DispatchResult, bool) {
	caps := map[string]json.RawMessage{}
	for _, k := range []string{"elicitation", "sampling"} {
		if v, ok := mcpwire.Member(r.work.ClientCapabilities, k); ok && firstByte(v) == '{' {
			caps[k] = v
		}
	}
	params, err := json.Marshal(map[string]any{
		"protocolVersion": mcpwire.V20251125, "capabilities": caps,
		"clientInfo": map[string]string{"name": "kontra-gate", "version": version.Get()},
	})
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	id := r.id()
	init, _, hdr, err := r.link.call(ctx, rpcBody(id, mcpwire.MethodInitialize, params), json.RawMessage(strconv.Quote(id)), r.onMessage(ctx))
	if err != nil {
		return r.beforeFirst(ctx, err)
	}
	if v, _ := stringMember(init.Result, "protocolVersion"); v != mcpwire.V20251125 {
		r.log.Warn("the upstream did not negotiate 2025-11-25 for the dispatch's session; it has no tasks", "version", v)
		r.link.sid = hdr.Get(mcpwire.HeaderSessionID)
		return r.fail(ctx, store.ReasonUpstreamError, "the service does not speak MCP tasks (2025-11-25)")
	}
	r.link.sid, r.link.version = hdr.Get(mcpwire.HeaderSessionID), mcpwire.V20251125
	if err := r.d.Log.SetUpstreamSession(ctx, r.fence, r.link.sid); err != nil {
		return lifecycle.DispatchResult{}, true
	}
	r.d.caps.Store(r.key, cancelDeclared(init.Result))
	if err := r.link.post(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return r.beforeFirst(ctx, err)
	}
	if ctx.Err() != nil {
		return lifecycle.DispatchResult{}, true
	}
	body, err := r.taskCall()
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	m, raw, _, err := r.link.call(ctx, body, r.work.RequestID, r.onMessage(ctx))
	if r.stop != nil {
		return *r.stop, true
	}
	if err != nil {
		if xe := asExchange(err); xe.class == classRPC {
			// §11.8 case 1: the upstream's JSON-RPC error answering the task-augmented tools/call is the task's
			// stored outcome.
			return r.finishError(ctx, xe)
		}
		return r.beforeFirst(ctx, err)
	}
	task, ok := mcpwire.Member(m.Result, "task")
	if !ok {
		// The upstream answered the call itself (it ignored the task): that is its result.
		return r.finishResult(ctx, m, raw)
	}
	up, _ := stringMember(task, "taskId")
	status, _ := stringMember(task, "status")
	st, okStatus := store.StateFromMCP(status)
	if up == "" || !okStatus {
		r.log.Warn("the upstream's CreateTaskResult names no task or no status it defines")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	if err := r.d.Log.SetFirstUpstreamID(ctx, r.fence, up, ""); err != nil {
		return lifecycle.DispatchResult{}, true
	}
	r.upTask = up
	if r.d.Signals != nil {
		if err := r.d.Signals.SignalDispatched(context.WithoutCancel(ctx), a2a.TaskID(r.taskID.String()), r.n); err != nil {
			r.log.Warn("kontra.dispatched was not signalled", "error", err)
		}
	}
	r.setInterval(task)
	text, _ := stringMember(task, "statusMessage")
	if st.Terminal() {
		return r.terminal(ctx, st)
	}
	if res, done := r.logStatus(ctx, st, text); done {
		return res, true
	}
	if st == store.StateInputRequired {
		r.openResult(ctx)
	}
	return lifecycle.DispatchResult{}, false
}

// cancelDeclared reports whether an InitializeResult declares capabilities.tasks.cancel (read with exact keys).
func cancelDeclared(result json.RawMessage) bool {
	caps, ok := mcpwire.Member(result, "capabilities")
	if !ok {
		return false
	}
	tasks, ok := mcpwire.Member(caps, "tasks")
	if !ok || firstByte(tasks) != '{' {
		return false
	}
	_, ok = mcpwire.Member(tasks, "cancel")
	return ok
}

// taskCall is the client's tools/call as stored, with params.task {ttl: the milliseconds until the deadline}
// (§11.5 step 4): every other byte is the client's, its JSON-RPC id included.
func (r *legacyRun) taskCall() ([]byte, error) {
	ttl := int64(time.Hour / time.Millisecond)
	if r.row.Deadline != nil {
		ttl = max(time.Until(*r.row.Deadline).Milliseconds(), 1000)
	}
	params, ok := mcpwire.Member(r.work.Request, "params")
	if !ok || firstByte(params) != '{' {
		return nil, errors.New("mcprelay: the stored tools/call has no params")
	}
	task, err := jsonObject("ttl", ttl)
	if err != nil {
		return nil, err
	}
	if params, err = mcpwire.SetMember(params, "task", task); err != nil {
		return nil, err
	}
	return mcpwire.SetMember(r.work.Request, "params", params)
}

// beforeFirst is §6.4's table for a failure before the first upstream task id: a lost or full tunnel and an
// unreachable upstream are retried (a new attempt), 408/429 with the upstream's Retry-After; a sealed part the node
// refused is sealed_rejected; anything else is upstream_error, not retried.
func (r *legacyRun) beforeFirst(ctx context.Context, err error) (lifecycle.DispatchResult, bool) {
	if ctx.Err() != nil {
		return lifecycle.DispatchResult{}, true
	}
	xe := asExchange(err)
	r.log.Info("the upstream call failed before the first upstream task id", "class", int(xe.class), "error", xe.Error())
	switch xe.class {
	case classTunnelLost, classFull:
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchRetryable, Reason: string(store.ReasonTunnelLost)}, true
	case classUnreachable:
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchRetryable, Reason: string(store.ReasonUpstreamUnreachable)}, true
	case classThrottled:
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchRetryable, Reason: string(store.ReasonUpstreamError),
			RetryAfterSeconds: int64(xe.retryAfter / time.Second)}, true
	case classSealed:
		return r.sealedRejected(ctx, xe)
	}
	return r.fail(ctx, store.ReasonUpstreamError, "")
}

// sealedRejected is a hosting node's refusal of a sealed part of the task (ext-sealed.md §6, relay.md §6.4): an
// audit row names the task, the attempt and the node's reason, and the task fails sealed_rejected, never retried.
func (r *legacyRun) sealedRejected(ctx context.Context, xe *exchangeError) (lifecycle.DispatchResult, bool) {
	f := audit.Fields{"task_id": r.taskID, "tenant_id": r.row.TenantID, "service": r.row.Service, "protocol": "mcp",
		"attempt": r.n, "reason": xe.reject, "replica_id": r.d.ReplicaID}
	if r.attempt != nil && r.attempt.TunnelSession != nil {
		f["tunnel_session"] = r.attempt.TunnelSession.String()
	}
	if r.work != nil && r.work.Session != "" {
		f["mcp_session"] = r.work.Session
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := audit.SealedRejected(actx, r.d.Log.DB(), f); err != nil {
		r.log.Error("a sealed-part rejection was not written to the audit chain", "error", err)
	}
	r.log.Warn("the hosting node refused a sealed part: the task fails sealed_rejected (not retried)", "reason", xe.reject)
	return r.fail(ctx, store.ReasonSealedRejected, "")
}

// loop follows the upstream task until it is terminal: tasks/get at the upstream's pollInterval, the messages of
// the streams it holds, and the client's answers from the log.
func (r *legacyRun) loop(ctx context.Context) lifecycle.DispatchResult {
	var wake <-chan struct{}
	if r.d.Waker != nil {
		w, stop := r.d.Waker.WaitTask(ctx, r.taskID)
		defer stop()
		wake = w
	}
	poll := time.NewTimer(r.interval)
	defer poll.Stop()
	answers := time.NewTicker(2 * time.Second)
	defer answers.Stop()
	if r.resume {
		poll.Reset(0)
	}
	r.sendAnswers(ctx)
	if r.stop != nil {
		return *r.stop
	}
	for {
		select {
		case <-ctx.Done():
			return lifecycle.DispatchResult{}
		case <-poll.C:
			if res, done := r.poll(ctx); done {
				return res
			}
			poll.Reset(r.interval)
		case sm := <-r.msgs:
			if res, done := r.onStream(ctx, sm); done {
				return res
			}
		case <-wake:
			r.sendAnswers(ctx)
		case <-answers.C:
			r.sendAnswers(ctx)
		}
		if r.stop != nil {
			return *r.stop
		}
	}
}

// setInterval takes the upstream's pollInterval (ms) from a Task, bounded to 0.5–30 s (§11.5 step 4).
func (r *legacyRun) setInterval(task json.RawMessage) {
	v, ok := mcpwire.Member(task, "pollInterval")
	if !ok {
		return
	}
	var ms float64
	if json.Unmarshal(v, &ms) != nil || ms <= 0 {
		return
	}
	lo, hi := r.d.pollBounds()
	r.interval = min(max(time.Duration(ms*float64(time.Millisecond)), lo), hi)
}

// poll is one upstream tasks/get (§11.5 step 4): a changed status is logged; input_required opens tasks/result on
// the dispatch's session; a terminal status fetches the result. -32602 (the upstream deleted its task, or the
// session is gone) fails the task upstream_error, "upstream task expired"; a lost tunnel fails it tunnel_lost
// (D32); an upstream that cannot be reached for 30 s fails it upstream_unreachable (§6.4's GetTask rule).
func (r *legacyRun) poll(ctx context.Context) (lifecycle.DispatchResult, bool) {
	params, _ := jsonObject("taskId", r.upTask)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	m, err := r.link.request(pctx, r.id(), "tasks/get", params, r.onMessage(ctx))
	cancel()
	if r.stop != nil {
		return *r.stop, true
	}
	if err != nil {
		return r.pollFailed(ctx, err)
	}
	r.failing = time.Time{}
	if err := mcpwire.CheckTask(m.Result); err != nil {
		// The members read here are checked here, not in every upstream message (mcpwire.CheckTask).
		r.log.Warn("strict decoding refused the upstream's tasks/get answer", "level", "result")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	status, _ := stringMember(m.Result, "status")
	st, ok := store.StateFromMCP(status)
	if !ok {
		r.log.Warn("the upstream's tasks/get answered a status MCP does not define")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	r.setInterval(m.Result)
	if st.Terminal() {
		return r.terminal(ctx, st)
	}
	text, _ := stringMember(m.Result, "statusMessage")
	if res, done := r.logStatus(ctx, st, text); done {
		return res, true
	}
	if st == store.StateInputRequired && !r.resultOpen {
		r.openResult(ctx)
	}
	return lifecycle.DispatchResult{}, false
}

// pollFailed is the table for an exchange after the first upstream task id that failed (§6.4, §11.5 step 4): a
// tunnel stream that could not be opened, and a response the node wrote itself other than full, fail the task
// tunnel_lost at once (§6.4's rows for an upstream task id that exists), whether or not this replica still holds
// the session; full, and what §6.4's GetTask rule names (unreachable, 408/429), are made again for 30 s first.
func (r *legacyRun) pollFailed(ctx context.Context, err error) (lifecycle.DispatchResult, bool) {
	if ctx.Err() != nil {
		return lifecycle.DispatchResult{}, true
	}
	xe := asExchange(err)
	switch {
	case xe.class == classRPC && xe.rpc.Code == mcpwire.CodeInvalidParams, xe.class == classGone:
		return r.fail(ctx, store.ReasonUpstreamError, "upstream task expired")
	case xe.class == classRPC, xe.class == classProtocol:
		r.log.Warn("the upstream's answer to the dispatch failed", "error", xe.Error())
		return r.fail(ctx, store.ReasonUpstreamError, "")
	case xe.class == classSealed:
		return r.sealedRejected(ctx, xe)
	case xe.class == classTunnelLost:
		r.log.Info("the attempt's tunnel exchange was lost after the first upstream task id", "error", xe.Error(), "session_held", r.link.held())
		return r.fail(ctx, store.ReasonTunnelLost, "")
	}
	now := time.Now()
	if r.failing.IsZero() {
		r.failing = now
	}
	if now.Sub(r.failing) < r.d.failWindow() {
		return lifecycle.DispatchResult{}, false
	}
	switch xe.class {
	case classFull:
		return r.fail(ctx, store.ReasonTunnelLost, "")
	case classThrottled:
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.fail(ctx, store.ReasonUpstreamUnreachable, "")
}

// terminal ends the task once the upstream's is terminal (§11.5 step 6): cancelled is canceled; otherwise the
// upstream's tasks/result is fetched and logged as the result.
func (r *legacyRun) terminal(ctx context.Context, st store.State) (lifecycle.DispatchResult, bool) {
	if st == store.StateCanceled {
		return r.cancelledUpstream(ctx)
	}
	params, _ := jsonObject("taskId", r.upTask)
	id := r.id()
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	m, raw, _, err := r.link.call(pctx, rpcBody(id, "tasks/result", params), json.RawMessage(strconv.Quote(id)), r.onMessage(ctx))
	cancel()
	if r.stop != nil {
		return *r.stop, true
	}
	if err != nil {
		xe := asExchange(err)
		if xe.class == classRPC && xe.rpc.Code != mcpwire.CodeInvalidParams {
			return r.finishError(ctx, xe)
		}
		return r.pollFailed(ctx, err)
	}
	return r.finishResult(ctx, m, raw)
}

// cancelledUpstream ends the task canceled when the upstream cancelled its task itself: the task's calls row is an
// error (§11.8). After a cancel the gate forwarded, the run writes nothing of the kind: the workflow writes the
// client's canceled (append's hold, D57).
//
// SPEC-CONFLICT (relay §11.8 case 3, §11.5 step 6): case 3's data names who cancelled ("client" | "declined" |
// "quote_expired"), and step 6 fetches the upstream's tasks/result at any terminal status; an upstream that cancels
// its own task fits neither (a 2025-11-25 receiver stores no result for a cancelled task: server-everything@1.0.0's
// tasks/result then answers an error). The closest faithful behaviour (as §11.12 says for the modern dispatch,
// "cancelled is canceled"): the task ends canceled with no kontra.canceled, whose tasks/result is -32603 "task
// cancelled" without data.
func (r *legacyRun) cancelledUpstream(ctx context.Context) (lifecycle.DispatchResult, bool) {
	err := r.append(ctx, gateWrite, func(row *store.TaskRow) (store.Write, error) {
		w, err := store.MCPStatusWrite(row, store.StateCanceled, "the service cancelled the task", nil)
		if err != nil {
			return store.Write{}, err
		}
		w.Call, err = r.d.Log.MCPCall(ctx, r.taskID, true, int64(len(w.Payload)))
		return w, err
	})
	return r.ended(err, "")
}

// finishResult logs the upstream's result (§11.5 step 6, §11.8 case 1): scrubbed (§2.5), its related-task key the
// gate task id, the task completed, or failed for a top-level isError true (a 2025-11-25 upstream reports failed
// for it); its calls row in the same statement.
func (r *legacyRun) finishResult(ctx context.Context, m *mcpwire.Message, raw []byte) (lifecycle.DispatchResult, bool) {
	res, err := mcpwire.ScrubResult(mcpwire.FromUpstream, "tasks/result", m.Result, r.sealed)
	if err == nil {
		res, err = setRelatedTask(res, r.gateID())
	}
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	isErr, ok := mcpwire.IsErrorFlag(res)
	if !ok {
		// A top-level isError that is not a JSON boolean: a malformed upstream message (exchange.go's process).
		r.log.Warn("the upstream's result has an isError that is not a boolean: the task fails upstream_error")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	state := store.StateCompleted
	if isErr {
		state = store.StateFailed
	}
	err = r.append(ctx, upstreamWrite, func(row *store.TaskRow) (store.Write, error) {
		w, err := store.MCPResultWrite(row, state, "result", res, nil)
		if err != nil {
			return store.Write{}, err
		}
		w.Call, err = r.d.Log.MCPCall(ctx, r.taskID, isErr, int64(len(res)))
		return w, err
	})
	if errors.Is(err, errOverTaskBytes) {
		r.log.Warn("the upstream's result would carry the task past limits.task_bytes; it is not logged")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.ended(err, "")
}

// finishError logs the upstream's JSON-RPC error answering the task-augmented tools/call or tasks/result as the
// task's result (§11.8 case 1): failed upstream_error, the error verbatim. -32602 after the first upstream task id
// is the upstream having deleted its task: upstream_error, "upstream task expired" (§11.5 step 4).
func (r *legacyRun) finishError(ctx context.Context, xe *exchangeError) (lifecycle.DispatchResult, bool) {
	if r.upTask != "" && xe.rpc.Code == mcpwire.CodeInvalidParams {
		return r.fail(ctx, store.ReasonUpstreamError, "upstream task expired")
	}
	raw := xe.raw
	if len(raw) == 0 || r.sealed {
		e := *xe.rpc
		if r.sealed {
			// A sealed service's error keeps its code only: its message and data may quote the sealed arguments,
			// and the task's log is Postgres (ext-sealed.md §7; the hosting node replaces them too).
			e = mcpwire.Error{Code: xe.rpc.Code, Message: "the service returned an error (its text is not kept for a sealed service)"}
		}
		b, err := json.Marshal(e)
		if err != nil {
			return r.fail(ctx, store.ReasonUpstreamError, "")
		}
		raw = b
	}
	err := r.append(ctx, upstreamWrite, func(row *store.TaskRow) (store.Write, error) {
		w, err := store.MCPResultWrite(row, store.StateFailed, "error", raw, map[string]any{store.MetaFailure: string(store.ReasonUpstreamError)})
		if err != nil {
			return store.Write{}, err
		}
		w.Call, err = r.d.Log.MCPCall(ctx, r.taskID, true, int64(len(raw)))
		return w, err
	})
	if errors.Is(err, errOverTaskBytes) {
		r.log.Warn("the upstream's error would carry the task past limits.task_bytes; it is not logged")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.ended(err, string(store.ReasonUpstreamError))
}

// fail logs `failed: <reason>` (§11.8 case 2) under the fence: the gate's own few bytes, written even past
// limits.task_bytes (the failure that ends a task that reached it).
func (r *legacyRun) fail(ctx context.Context, reason store.FailureReason, text string) (lifecycle.DispatchResult, bool) {
	err := r.append(ctx, gateWrite, func(row *store.TaskRow) (store.Write, error) {
		return failWrite(ctx, r.d.Log, row, reason, text)
	})
	return r.ended(err, string(reason))
}

// ended is the dispatch's result after its terminal write: terminal when it committed, or when the task had ended
// already (the workflow's cancel or timeout); a lost context when the fence refused it, the dispatch was stopped
// while it waited for the workflow's terminal event, or the store failed (the workflow follows the attempt again).
func (r *legacyRun) ended(err error, reason string) (lifecycle.DispatchResult, bool) {
	switch {
	case err == nil:
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal, Reason: reason}, true
	case errors.Is(err, errTaskEnded):
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, true
	case errors.Is(err, store.ErrFenced), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return lifecycle.DispatchResult{}, true
	}
	r.log.Warn("the task's terminal event could not be written; the attempt is followed again", "error", err)
	return lifecycle.DispatchResult{}, true
}

// errOverTaskBytes is an event that would carry the task past limits.task_bytes (spec §6.6).
var errOverTaskBytes = errors.New("mcprelay: the task's bytes would pass limits.task_bytes")

// errCancelling is a terminal write of the run's own after forward_cancel marked it (legacyRun.cancelling).
var errCancelling = errors.New("mcprelay: a cancel was forwarded; the workflow writes the task's end")

// writeSource says what an event of the run carries: upstreamWrite, what the upstream sent (a status message, a
// result, an error), counted against limits.task_bytes whatever the state it writes, a terminal one included
// (§6.6: each logged event's whole encoding, as the A2A relay counts it); gateWrite, the gate's own few bytes
// (failed: <reason>, the upstream's cancelled), never refused by it, since that failure is how a task that reached
// the limit ends.
type writeSource bool

const (
	upstreamWrite writeSource = false
	gateWrite     writeSource = true
)

// append writes build's event under the fence (appendFenced). An upstream event that would carry the task past
// limits.task_bytes is not written (errOverTaskBytes): its caller fails the task upstream_error (§6.6). Once a
// forward_cancel marked the run, a terminal event of its own is not written either: the run waits for the
// workflow's (holdForWorkflow), and writes its own only when none came.
func (r *legacyRun) append(ctx context.Context, src writeSource, build func(row *store.TaskRow) (store.Write, error)) error {
	held := false
	for {
		var n int
		err := appendFenced(context.WithoutCancel(ctx), r.d.Log, r.fence, func(row *store.TaskRow) (store.Write, error) {
			w, err := build(row)
			if err != nil {
				return store.Write{}, err
			}
			if w.State.Terminal() && !held && r.cancelling != nil && r.cancelling.Load() {
				return store.Write{}, errCancelling
			}
			n = len(w.Payload)
			if src == upstreamWrite && r.bytes+int64(n) > r.d.taskBytes() {
				return store.Write{}, errOverTaskBytes
			}
			return w, nil
		})
		if errors.Is(err, errCancelling) {
			if err := r.holdForWorkflow(ctx); err != nil {
				return err
			}
			held = true
			continue
		}
		if err == nil {
			r.bytes += int64(n)
		}
		return err
	}
}

// holdForWorkflow waits, once forward_cancel marked the run, for the workflow's terminal event instead of writing
// the run's own (spec §11.6, D57: the gate writes canceled whatever the upstream answered; the upstream's own
// reaction to the cancel, its cancelled status or an error ending its tasks/result, is not the task's end). It is
// errTaskEnded once the task is terminal, ctx's error when the dispatch is stopped first (the workflow cancels it
// after its write), and nil when neither happened within cancelHold: the workflow wrote nothing, and the run writes
// its own end after all.
func (r *legacyRun) holdForWorkflow(ctx context.Context) error {
	r.log.Info("a cancel was forwarded: the dispatch leaves the task's end to the workflow")
	until := time.NewTimer(r.d.cancelHoldFor())
	defer until.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if row, err := r.d.Log.Load(ctx, r.taskID); err == nil && row.State.Terminal() {
			return errTaskEnded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-until.C:
			r.log.Warn("the workflow wrote no end within the hold after a forwarded cancel; the dispatch writes its own")
			return nil
		case <-tick.C:
		}
	}
}

// logStatus logs a status change of the upstream task as a status event (§11.5 step 4), in tasks.state's
// vocabulary with the upstream's statusMessage; an unchanged status logs nothing.
func (r *legacyRun) logStatus(ctx context.Context, st store.State, text string) (lifecycle.DispatchResult, bool) {
	if st == r.last.state && text == r.last.text {
		return lifecycle.DispatchResult{}, false
	}
	err := r.append(ctx, upstreamWrite, func(row *store.TaskRow) (store.Write, error) {
		return store.MCPStatusWrite(row, st, text, nil)
	})
	switch {
	case err == nil:
		r.last.state, r.last.text = st, text
		return lifecycle.DispatchResult{}, false
	case errors.Is(err, errOverTaskBytes):
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.ended(err, "")
}

// onMessage handles a message of the upstream's that is not the answer the dispatch waits for (§11.5 step 5): an
// elicitation/create or sampling/createMessage is logged as an input_request; ping is answered; any other request
// is answered -32601 (the dispatch's session declared nothing else); notifications and stray responses are
// dropped. A write the fence or the task's end refuses stops the dispatch.
func (r *legacyRun) onMessage(ctx context.Context) func(*mcpwire.Message, []byte) {
	return func(m *mcpwire.Message, raw []byte) {
		if m.Method == "notifications/tools/list_changed" && len(m.ID) == 0 && r.d.ToolsChanged != nil && r.row != nil {
			// The service's tools changed: its drained lists go on every replica (§11.3), as for a relayed one.
			r.d.ToolsChanged(ToolsKey{TenantID: r.row.TenantID, Service: r.row.Service})
		}
		if m.Method == "" || len(m.ID) == 0 {
			return
		}
		switch m.Method {
		case mcpwire.MethodElicit, mcpwire.MethodSample:
			r.inputRequest(ctx, m)
		case "ping":
			r.answer(ctx, m.ID, "result", json.RawMessage(`{}`))
		default:
			e, _ := json.Marshal(mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "Method not found"})
			r.answer(ctx, m.ID, "error", e)
		}
	}
}

// answer posts the dispatch's own response to an upstream request.
func (r *legacyRun) answer(ctx context.Context, id json.RawMessage, member string, raw json.RawMessage) {
	b, _ := messageBytes(id, member, raw)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.link.post(pctx, b); err != nil {
		r.log.Debug("an answer to the upstream was not delivered", "error", err)
	}
}

// inputRequest logs an upstream's request as an input_request event (§11.5 step 5, D159, D175): its params
// scrubbed (§2.5; a sealed service's exact {v, envelope} kept, D174), its related-task key the gate task id, its
// JSON-RPC id the gate id k.<taskId>.<seq>, and the upstream's own id kept in the event. A request already logged
// (the same upstream id, on another stream) is not logged again.
func (r *legacyRun) inputRequest(ctx context.Context, m *mcpwire.Message) {
	key, ok := mcpwire.IDKey(m.ID)
	if !ok {
		return
	}
	if _, done := r.logged[key]; done {
		return
	}
	params, err := mcpwire.ScrubParams(mcpwire.FromUpstream, m.Method, m.Params, r.sealed)
	if err == nil {
		params, err = setRelatedTask(params, r.gateID())
	}
	if err != nil {
		r.log.Warn("an upstream input request could not be rewritten; it is not relayed", "error", err)
		return
	}
	var n int
	seq, err := r.d.Log.AppendInputRequest(context.WithoutCancel(ctx), r.fence, r.taskID, m.ID, func(gateID string) (json.RawMessage, error) {
		b := rpcBody(gateID, m.Method, params)
		n = len(b) + len(m.ID) + 32
		if r.bytes+int64(n) > r.d.taskBytes() {
			return nil, errOverTaskBytes
		}
		return b, nil
	})
	switch {
	case err == nil:
		r.bytes += int64(n)
		r.logged[key], r.upstreamID[seq] = seq, m.ID
		r.log.Info("an input request of the upstream task was logged", "seq", seq, "method", m.Method)
	case errors.Is(err, errOverTaskBytes):
		res, _ := r.fail(ctx, store.ReasonUpstreamError, "")
		r.stop = &res
	case errors.Is(err, store.ErrFenced):
		r.stop = &lifecycle.DispatchResult{}
	case errors.Is(err, a2a.ErrTaskNotFound):
		r.stop = &lifecycle.DispatchResult{}
	default:
		if row, lerr := r.d.Log.Load(context.WithoutCancel(ctx), r.taskID); lerr == nil && row.State.Terminal() {
			r.stop = &lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}
			return
		}
		r.log.Warn("an upstream input request was not logged", "error", err)
	}
}

// setRelatedTask sets the related-task key of obj's _meta, when it has one, to taskID (§11.6: the key is rewritten
// in both directions), keeping every other member's bytes (a sealed envelope beside it among them, D174).
func setRelatedTask(obj json.RawMessage, taskID string) (json.RawMessage, error) {
	meta, ok := mcpwire.Member(obj, "_meta")
	if !ok || firstByte(meta) != '{' {
		return obj, nil
	}
	if _, ok := mcpwire.Member(meta, mcpwire.MetaRelatedTask); !ok {
		return obj, nil
	}
	rel, err := jsonObject("taskId", taskID)
	if err != nil {
		return nil, err
	}
	if meta, err = mcpwire.SetMember(meta, mcpwire.MetaRelatedTask, rel); err != nil {
		return nil, err
	}
	return mcpwire.SetMember(obj, "_meta", meta)
}

// openResult calls tasks/result on the dispatch's session (§11.5 step 5), held on a goroutine of its own: the
// upstream delivers its input requests on that stream, then, at the end, the task's result.
func (r *legacyRun) openResult(ctx context.Context) {
	r.resultOpen = true
	params, _ := jsonObject("taskId", r.upTask)
	id := r.id()
	body := rpcBody(id, "tasks/result", params)
	go func() {
		err := r.link.stream(ctx, body, json.RawMessage(strconv.Quote(id)), func(m *mcpwire.Message, raw []byte, final bool) bool {
			return r.deliver(ctx, streamMsg{from: "result", m: m, raw: raw, final: final})
		})
		var xe *exchangeError
		if errors.As(err, &xe) && xe.class == classProtocol && strings.HasPrefix(xe.msg, "strict decoding") {
			r.deliver(ctx, streamMsg{from: "result", err: err})
		}
		r.deliver(ctx, streamMsg{from: "result", ended: true})
	}()
}

// holdGet holds a GET on the dispatch's session (§11.5 step 4) while the dispatch runs: the upstream's requests on
// it are treated as those on tasks/result. It is opened again after it ends, backing off, and not at all once the
// upstream answered 405.
func (r *legacyRun) holdGet(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := r.link.stream(ctx, nil, nil, func(m *mcpwire.Message, raw []byte, _ bool) bool {
			wait = time.Second
			return r.deliver(ctx, streamMsg{from: "get", m: m, raw: raw})
		})
		var xe *exchangeError
		switch {
		case errors.Is(err, errNoGet):
			return
		case errors.As(err, &xe) && xe.class == classProtocol && strings.HasPrefix(xe.msg, "strict decoding"):
			r.deliver(ctx, streamMsg{from: "get", err: err})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}

// deliver hands a stream's message to the dispatch's loop; false once the dispatch has ended.
func (r *legacyRun) deliver(ctx context.Context, sm streamMsg) bool {
	select {
	case r.msgs <- sm:
		return true
	case <-ctx.Done():
		return false
	}
}

// onStream handles one message of a stream the dispatch holds: the answer to its tasks/result ends the task with
// it; an upstream request is onMessage's; a message strict decoding refused fails the task upstream_error (C1).
func (r *legacyRun) onStream(ctx context.Context, sm streamMsg) (lifecycle.DispatchResult, bool) {
	switch {
	case sm.err != nil:
		r.log.Warn("an upstream message on the dispatch's stream was refused by strict decoding", "stream", sm.from, "error", sm.err)
		return r.fail(ctx, store.ReasonUpstreamError, "")
	case sm.ended:
		if sm.from == "result" {
			r.resultOpen = false
		}
		return lifecycle.DispatchResult{}, false
	case sm.final:
		r.resultOpen = false
		if sm.m.Error != nil {
			e, _ := mcpwire.Member(sm.raw, "error")
			return r.finishError(ctx, &exchangeError{class: classRPC, rpc: sm.m.Error, raw: e})
		}
		return r.finishResult(ctx, sm.m, sm.raw)
	}
	r.onMessage(ctx)(sm.m, sm.raw)
	if r.stop != nil {
		return *r.stop, true
	}
	return lifecycle.DispatchResult{}, false
}

// sendAnswers writes upstream every input_response the task has logged and no dispatch of the attempt has written
// (§11.5 step 5): the client's response as it arrived, its JSON-RPC id the upstream's original id and its
// related-task key the upstream task id; a sealed envelope in it untouched (D175). An answer that could not be
// delivered is tried again at the next wake; one the upstream took (or refused) is recorded under the fence on the
// task's work item (store.Log.MarkAnswerDelivered), so that a re-attach (D42) does not write it again. A tunnel
// stream that could not be opened, or a node's own answer other than full, fails the task tunnel_lost (§6.4).
func (r *legacyRun) sendAnswers(ctx context.Context) {
	resps, err := r.d.Log.InputResponses(ctx, r.taskID, 0)
	if err != nil {
		return
	}
	for _, a := range resps {
		if r.sent[a.Seq] {
			continue
		}
		up, ok := r.upstreamID[a.RequestSeq]
		if !ok {
			if err := r.loadRequests(ctx); err != nil {
				return
			}
			if up, ok = r.upstreamID[a.RequestSeq]; !ok {
				r.sent[a.Seq] = true
				continue
			}
		}
		body, err := mcpwire.SetMember(a.Response, "id", up)
		if err == nil {
			if res, ok := mcpwire.Member(body, "result"); ok && firstByte(res) == '{' && r.upTask != "" {
				var out json.RawMessage
				if out, err = setRelatedTask(res, r.upTask); err == nil {
					body, err = mcpwire.SetMember(body, "result", out)
				}
			}
		}
		if err != nil {
			r.sent[a.Seq] = true
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = r.link.post(pctx, body)
		cancel()
		delivered := err == nil
		if err != nil {
			switch xe := asExchange(err); xe.class {
			case classUnreachable, classFull, classThrottled:
				continue
			case classTunnelLost:
				r.log.Info("the attempt's tunnel exchange was lost writing an answer upstream", "seq", a.Seq, "error", xe.Error())
				res, _ := r.fail(ctx, store.ReasonTunnelLost, "")
				r.stop = &res
				return
			case classSealed:
				// The hosting node refused the answer (ext-sealed.md §5 rule 4b): a sealed rejection like any other, audited
				// and final (§6). The upstream never received it, and nothing else can answer the request: the client's one
				// answer to its gate id was taken (relay.md §11.7). M3 WP-10 pentest finding F-4.
				res, _ := r.sealedRejected(ctx, xe)
				r.stop = &res
				return
			}
			r.log.Warn("an answer to an input request was refused by the upstream", "seq", a.Seq, "error", err)
		}
		r.sent[a.Seq] = true
		if err := r.d.Log.MarkAnswerDelivered(context.WithoutCancel(ctx), r.fence, r.workItem, a.Seq); err != nil {
			if errors.Is(err, store.ErrFenced) {
				r.stop = &lifecycle.DispatchResult{}
				return
			}
			r.log.Warn("an answer's delivery was not recorded; a re-attach may write it again", "seq", a.Seq, "error", err)
		}
		if delivered {
			r.log.Info("an answer to an input request was written upstream", "seq", a.Seq, "request_seq", a.RequestSeq)
		}
	}
}
