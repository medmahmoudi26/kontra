package gatemcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
)

// PlainWait is how long a plain send_message and every reply wait for the task to end or ask a question before answering the
// send_message result of a task still running: its state and the follow-up text (§12.2, D272, D324). It is below the 60 s request timeout of the MCP SDKs.
const PlainWait = 25 * time.Second

// PlainBudget bounds a plain send_message's or reply's request from its arrival to its answer: the plain wait is cut so that
// the answer is written within it, after Admission (up to 30 s, hooks.admission.timeout) and the task's start or
// follow-up (up to lifecycle.AcceptWait), so that it stays below the 60 s request timeout of the MCP SDKs, whose
// expiry closes the stream and, on a modern send_message, cancels the task (§12.5, D178). plainFloor is the least a plain
// wait lasts all the same.
const (
	PlainBudget = 50 * time.Second
	plainFloor  = time.Second
)

// MCPSendRequest is one send_message: the A2A message the gate builds (its messageId minted by the gate, its parts from the
// arguments, the caller's contextId), the service, and the resend key of §12.3 (FirstRequest).
type MCPSendRequest = relay.MCPSendRequest

// MCPSendOutcome is the task a send_message created or found again (§12.3 Idempotency), as it is now.
type MCPSendOutcome = relay.MCPSendOutcome

// Caller creates and follows up send_message tasks. The gate implements it over the A2A service's handler, work queue and
// TaskLifecycle (*relay.Caller; spec §6, §7, §15): the task is an ordinary A2A task (protocol a2a) marked tasks.sent_by_mcp,
// whose first_message_id is the gate-minted messageId and whose first_request is MCPSendRequest.FirstRequest (§12.3).
type Sender interface {
	// SendMessage runs Admission (operation task, §13.1) and creates the task submitted; or, for a send_message with an
	// idempotencyKey, answers the task an earlier send_message of that key created (D338).
	SendMessage(ctx context.Context, req MCPSendRequest) (MCPSendOutcome, error)
	// Reply sends parts as the §7 follow-up of a send_message task of p's caller in input-required; refused as §7 says
	// otherwise. It returns the task's version before the follow-up.
	Reply(ctx context.Context, p auth.Principal, task *store.TaskRow, parts a2a.ContentParts) (int64, error)
}

// send_message answers the send_message tool (§12.2, §12.3): the service a ref names when the principal may send a message to it (else the
// unknown-service answer); an MCP service, one whose sealing is required, or one whose served card requires an
// extension a send_message cannot activate (the service's own, D294; the Caller activates the operator's for the caller, D300) is
// a tool execution error naming the route to use, and no task; then the task through the Caller, answered as §12.3
// says: CreateTaskResult when task-augmented, else after the plain wait. A modern request's task is answered by answerModernSendMessage (§12.5); its creation runs on a
// context its client cannot cancel (bounded by modernSendMessageBound), so that a client that leaves meanwhile has its task
// cancelled rather than running unseen (D178).
func (h *Handler) send(rq *request, args json.RawMessage, augmented bool) {
	if rq.modern && h.deps.Tasks != nil {
		// A modern send_message may cancel its task when its client leaves: the replica's drain awaits it (SendMessageTasks.Wait).
		defer h.deps.Tasks.hold()()
	}
	a, parts, err := parseSendMessageArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	ref, err := ParseRef(a.Service, h.deps.Multi)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	svc, err := h.deps.Catalog.Callable(rq.ctx(), rq.p, ref)
	if err != nil {
		h.internal(rq, "look up the service", err)
		return
	}
	if svc == nil {
		h.toolError(rq, ErrUnknownService)
		return
	}
	base := strings.TrimRight(h.deps.PublicURL, "/")
	switch {
	case svc.ref.Kind == config.KindMCP:
		rq.result(errorResult("service "+ref.String()+" is an MCP server: send_message takes A2A agents only. "+
			"Call its tools directly at "+base+"/mcp/"+ref.String()+".", nil))
		return
	case svc.ref.Sealed == string(config.SealedRequired):
		rq.result(errorResult("service "+ref.String()+" takes sealed messages only, and send_message cannot seal "+
			"tool arguments. Send it A2A messages through a node (kontrad seals them) at "+base+"/agents/"+ref.String()+".", nil))
		return
	}
	key, err := FirstRequest(ref, args)
	if err != nil {
		h.toolError(rq, badArgs("arguments are not JSON the gate can canonicalize"))
		return
	}
	mid, err := uuid.NewV7()
	if err != nil {
		h.internal(rq, "mint a message id", err)
		return
	}
	var hk *store.SendMessageKey
	keyed := a.IdempotencyKey != ""
	if keyed {
		// The key's messageIds (D338): the same for every send_message of this key, caller and principal in one period, so the task
		// the first one created is found by them (store.Log.FindSendMessageKeyed), whatever arguments the later one carries.
		k := SendMessageKeyOf(rq.p, a.IdempotencyKey, h.now())
		hk = &k
		mid = uuid.MustParse(k.MessageIDs[0])
	}
	msg := &a2a.Message{ID: mid.String(), Role: a2a.MessageRoleUser, ContextID: a.ContextID, Parts: parts}
	hctx := rq.ctx()
	if rq.modern {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(context.WithoutCancel(hctx), modernSendMessageBound)
		defer cancel()
	}
	out, err := h.deps.Sender.SendMessage(hctx, MCPSendRequest{Principal: rq.p, Service: svc.ref, Message: msg, FirstRequest: key, Keyed: hk})
	var xe *relay.ExtensionsRequiredError
	switch {
	case errors.Is(err, lifecycle.ErrSendMessageKeyReused):
		rq.result(errorResult("idempotencyKey already used for a different send_message; no task was created.", nil))
		return
	case errors.Is(err, relay.ErrNotServed):
		// The registry stopped serving it since the catalog read it (D303).
		h.toolError(rq, ErrUnknownService)
		return
	case errors.As(err, &xe):
		rq.result(errorResult(extensionsText("service "+ref.String(), "send_message", xe.URIs,
			base+"/agents/"+ref.String())+" No task was created.", nil))
		return
	case err != nil:
		h.refused(rq, "send_message", err)
		return
	}
	row := out.Task
	if out.Existing {
		h.log.Info("send_message answered with an existing task (a retry)", "task_id", row.ID, "state", row.State, "principal", rq.p.ID)
	} else {
		h.log.Info("send_message created a task", "service", ref.String(), "task_id", row.ID, "state", row.State, "principal", rq.p.ID)
	}
	if rq.modern {
		h.answerModernSendMessage(rq, out, svc.ref, augmented, keyed)
		return
	}
	if augmented {
		h.writeCreateTask(rq, row, svc.ref)
		return
	}
	h.waitAndAnswerFrom(rq, row.ID, 0, seenFrom(out))
}

// seenFrom is where a send_message's progress notifications start (D339): a task it created from its creation, an existing
// one (a retry, a re-issued keyed send_message) from the log's tail.
func seenFrom(out MCPSendOutcome) int64 {
	if out.Existing {
		return -1
	}
	return 0
}

// writeCreateTask answers a task-augmented send_message with its CreateTaskResult (§11.5 step 2, §12.3).
func (h *Handler) writeCreateTask(rq *request, row *store.TaskRow, svc store.ServiceRef) {
	res, err := createTaskResult(row, svc, rq.reader())
	if err != nil {
		h.internal(rq, "write the task", err)
		return
	}
	writeResult(rq.w, rq.sid, rq.msg.ID, res)
}

// reply answers the reply tool (§12.2, §7): the send_message task of the principal's caller (any other id is the
// unknown-task answer); a task not at input-required is refused (§7); otherwise the follow-up through the Caller, and the plain
// wait for the turn it started.
func (h *Handler) reply(rq *request, args json.RawMessage) {
	a, parts, err := parseReplyArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.deps.Log.LoadSentByMCP(rq.ctx(), rq.p.CallerID, a.TaskID)
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		rq.result(errorResult("unknown task", nil))
		return
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	case !admitsTask(rq.ctx(), h.deps.Served, rq.p, row):
		// The A2A route answers a follow-up "unknown service" when its service is not served to the principal (disabled,
		// its allow: no longer admitting it): nothing is sent, and the task is left as it is (D303).
		rq.result(errorResult("unknown task", nil))
		return
	}
	switch {
	case row.State.Terminal():
		rq.result(errorResult("The task ended ("+string(row.State)+"); nothing was sent. Continue the conversation with send_message "+
			"{service: \""+h.serviceRef(row)+"\", contextId: \""+row.ContextID+"\", message}.", nil))
		return
	case row.State != store.StateInputRequired:
		rq.result(errorResult("The task is "+string(row.State)+" and takes no input now; nothing was sent. It is still working: "+
			"follow it with status {taskId: \""+row.ID.String()+"\", after: 0, wait: 25}.", nil))
		return
	}
	after, err := h.deps.Sender.Reply(rq.ctx(), rq.p, row, parts)
	var xe *relay.ExtensionsRequiredError
	switch {
	case errors.Is(err, relay.ErrNotServed):
		rq.result(errorResult("unknown task", nil))
		return
	case errors.As(err, &xe):
		ref := h.serviceRef(row)
		rq.result(errorResult(extensionsText("the task's service "+ref, "reply", xe.URIs,
			strings.TrimRight(h.deps.PublicURL, "/")+"/agents/"+ref)+" Nothing was sent.", nil))
		return
	case err != nil:
		h.refused(rq, "reply", err)
		return
	}
	h.waitAndAnswer(rq, row.ID, after)
}

// extensionsText is the tool execution error of a send_message or a reply to a service whose served card requires extensions a
// send_message cannot activate (spec §2.4, D294, D300): the service's own, which a tool call has no A2A-Extensions for, so the
// route is A2A with A2A-Extensions, as the required-sealed answer names its route.
func extensionsText(who, tool string, uris []string, a2aURL string) string {
	s, them := "", "it"
	if len(uris) != 1 {
		s, them = "s", "them"
	}
	return who + " requires the A2A extension" + s + " " + strings.Join(uris, ", ") + ", which " + tool +
		" cannot activate. Send it A2A messages that activate " + them + " in the A2A-Extensions header, at " + a2aURL + "."
}

// refused answers an error the Caller returned (the A2A route's own refusals): an anonymous tenant's cap is 429 with
// -32600 "rate limited" and Retry-After (as the MCP relay answers it, spec/tunnel.md §6.2); "admission unavailable"
// and a lifecycle that did not accept the request in time are 503 with -32603 (§13.1, §7); any other A2A refusal
// (Admission's refusal with its code and message, an operator's code as it is (D325), "anonymous storage limit", a
// part or a follow-up refused) is that JSON-RPC error; anything else is the gate's own failure (what names the request
// in the log).
func (h *Handler) refused(rq *request, what string, err error) {
	id := rq.msg.ID
	code, msg := relay.CodeOf(err)
	var ae *a2a.Error
	switch {
	case errors.Is(err, anon.ErrRateLimited):
		rq.w.Header().Set("Retry-After", strconv.Itoa(int(anon.RetryAfter/time.Second)))
		writeError(rq.w, http.StatusTooManyRequests, id, mcpwire.CodeInvalidRequest, anon.ErrRateLimited.Error(), nil)
	case errors.Is(err, lifecycle.ErrUnavailable):
		if msg == "" {
			msg = "the task's lifecycle is unavailable"
		}
		h.log.Warn("mcp: send_message or reply refused as unavailable", "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, id, mcpwire.CodeInternal, msg, nil)
	case errors.Is(err, a2a.ErrUnsupportedOperation):
		// §7's refusals of a follow-up: a tool execution error the model can read.
		if msg == "" {
			msg = "the task takes no input now"
		}
		rq.result(errorResult(msg+"; nothing was sent.", nil))
	case errors.As(err, &ae) || code != mcpwire.CodeInternal:
		if msg == "" {
			msg = err.Error()
		}
		writeError(rq.w, http.StatusOK, id, code, msg, nil)
	default:
		h.internal(rq, what, err)
	}
}

// waitAndAnswer waits up to the plain wait for the task to end, to reach input-required after version after (a
// reply's turn; 0 for a new task), and answers what it then is (§12.2, D272): an ended task as tasks/result renders
// it; input-required as §12.4's reply-tool text; a task still
// running with a text naming the A2A URL that follows it. A request that accepts text/event-stream is answered on an
// event stream with keep-alives while it waits. It reports whether the answer was written: false when the client left
// first (its request context ended), which a modern send_message reads as the request's cancellation (answerModernSendMessage).
func (h *Handler) waitAndAnswer(rq *request, id uuid.UUID, after int64) bool {
	return h.waitAndAnswerFrom(rq, id, after, after)
}

// waitAndAnswerFrom is waitAndAnswer, its progress notifications (D339) for the rows after seq seen: after for a new
// task (0) or a reply's turn; the log's tail when seen is negative, for a send_message answered with an existing task (a retry,
// a keyed send_message re-issued after its client reconnects), whose earlier rows are history, not progress.
func (h *Handler) waitAndAnswerFrom(rq *request, id uuid.UUID, after, seen int64) bool {
	aw := newAnswerWriter(rq)
	ctx := rq.ctx()
	if aw.progress != nil && seen < 0 {
		tail, err := h.deps.Log.Tail(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			aw.internal(h, "read the task's log", err)
			return true
		}
		seen = tail
	}
	wake, stop := h.deps.Waker.WaitTask(ctx, id)
	defer stop()
	deadline := time.NewTimer(h.plainWait(rq))
	defer deadline.Stop()
	reread := time.NewTicker(time.Second)
	defer reread.Stop()
	keep := time.NewTicker(keepAliveEvery)
	defer keep.Stop()
	aw.begin()
	if aw.progress != nil {
		// The rows of the turn this wait follows: a new task's from its creation, a reply's after the version it read, an
		// existing task's from now.
		aw.progress.seen = seen
	}
	var row *store.TaskRow
	for {
		// The row and the seq of the event its state comes from, in one statement (one snapshot): read apart, a turn's
		// terminal event committed between the two reads would end a reply's wait on the row's earlier question.
		r, asked, err := h.deps.Log.LoadSentByMCPAsked(ctx, rq.p.CallerID, id)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			aw.internal(h, "read the task", err)
			return true
		}
		row = r
		if h.deps.afterRead != nil {
			h.deps.afterRead(row)
		}
		h.notifyProgress(rq, aw, id)
		if turnOver(row, asked, after) {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return aw.result(h.plainAnswer(ctx, row, rq.p))
		case <-wake:
		case <-reread.C:
		case <-keep.C:
			aw.keepAlive()
		}
	}
	return aw.result(h.plainAnswer(ctx, row, rq.p))
}

// plainWait is how long a plain wait of rq lasts: PlainWait, cut so that the answer is written within PlainBudget of
// the request's start (what Admission and the task's start or follow-up took came first), and at least plainFloor (or
// PlainWait when that is shorter).
func (h *Handler) plainWait(rq *request) time.Duration {
	wait, budget := h.deps.PlainWait, h.deps.PlainBudget
	if budget <= 0 {
		budget = PlainBudget
	}
	if rq.start.IsZero() {
		return wait
	}
	return max(min(wait, budget-h.now().Sub(rq.start)), min(wait, plainFloor))
}

// turnOver reports whether a plain wait ends at row: terminal, or input-required with its question asked
// after version after. asked is the seq of the task or status event row's state comes from, read with row
// (store.Log.LoadSentByMCPAsked): a seq read on its own may be a later event's, which row does not hold yet.
func turnOver(row *store.TaskRow, asked, after int64) bool {
	switch {
	case row.State.Terminal():
		return true
	case row.State != store.StateInputRequired:
		return false
	}
	return after == 0 || asked > after
}

// plainAnswer is the CallToolResult of a plain send_message or reply for the task as row has it (§12.2): its text in content
// and, with taskId, state, contextId, service and the task's thread URL, in structuredContent (D324, D336). answerOf
// builds it.
func (h *Handler) plainAnswer(ctx context.Context, row *store.TaskRow, p auth.Principal) json.RawMessage {
	b, err := json.Marshal(h.answerOf(ctx, row, p))
	if err != nil {
		return errorResult("the task's result could not be rendered", nil)
	}
	return b
}

// serviceRef is the ref of a task's service on this gate (the tenant's slug is not on the row: a single-tenant gate
// names the service alone; a multi-tenant one reads the slug).
func (h *Handler) serviceRef(row *store.TaskRow) string {
	if !h.deps.Multi {
		return row.Service
	}
	svc, err := h.deps.Log.LoadService(context.Background(), row.TenantID, row.Service)
	if err != nil {
		return row.Service
	}
	return svc.Tenant + "/" + row.Service
}

// keepAliveEvery is how often a gate-served event stream writes a comment while it waits (spec §2.2).
const keepAliveEvery = 15 * time.Second

// answerWriter writes one JSON-RPC answer, as JSON or, when the request accepts text/event-stream, as the one event
// of an event stream opened at once and kept alive while the answer is awaited (the corrections' recommendation for
// a plain wait).
type answerWriter struct {
	rq  *request
	sse bool
	s   *mcprelay.GateStream
	// progress, when the request's params._meta carries a progressToken and it accepts text/event-stream, is what the
	// wait has told the client of (D339); nil otherwise.
	progress *progress
}

// progress is a request's MCP progress (D339): its token, the last progress value sent, and the last seq of the task's
// log the wait has read.
type progress struct {
	token json.RawMessage
	n     int64
	seen  int64
	last  string // the message of the last notification sent
}

func newAnswerWriter(rq *request) *answerWriter {
	aw := &answerWriter{rq: rq, sse: strings.Contains(rq.r.Header.Get("Accept"), "text/event-stream"),
		s: mcprelay.NewGateStream(rq.w, rq.sid)}
	if tok, ok := progressToken(rq.msg); ok && aw.sse {
		aw.progress = &progress{token: tok}
	}
	return aw
}

// progressToken is a request's params._meta.progressToken when it is a string or an integer (MCP's ProgressToken).
func progressToken(m *mcpwire.Message) (json.RawMessage, bool) {
	if m == nil {
		return nil, false
	}
	meta, ok := mcpwire.Member(m.Params, "_meta")
	if !ok || firstByte(meta) != '{' {
		return nil, false
	}
	tok, ok := mcpwire.Member(meta, "progressToken")
	if !ok {
		return nil, false
	}
	switch c := firstByte(tok); {
	case c == '"':
		var s string
		return tok, json.Unmarshal(tok, &s) == nil
	case c == '-' || (c >= '0' && c <= '9'):
		var n int64
		return tok, json.Unmarshal(tok, &n) == nil
	}
	return nil, false
}

// notifyProgress writes, on the request's event stream, one notifications/progress for each status and artifact row of
// the task's log after the last one the wait read (D339): progress increasing from 1, message the status message's
// text (else the state it sets) or "artifact <name>". Nothing without a progressToken.
func (h *Handler) notifyProgress(rq *request, aw *answerWriter, id uuid.UUID) {
	pr := aw.progress
	if pr == nil || rq.ctx().Err() != nil {
		return
	}
	evs, _, err := h.deps.Log.EventsWithin(rq.ctx(), id, pr.seen, StatusEvents, StatusBytes)
	if err != nil {
		return
	}
	for _, ev := range evs {
		pr.seen = ev.Seq
		if ev.Kind != store.KindStatus && ev.Kind != store.KindArtifact && ev.Kind != store.KindTask {
			continue
		}
		var sr a2a.StreamResponse
		if json.Unmarshal(ev.Payload, &sr) != nil {
			continue
		}
		msg := ""
		switch v := sr.Event.(type) {
		case *a2a.Task:
			msg = stateWord(v.Status.State)
		case *a2a.TaskStatusUpdateEvent:
			msg = stateWord(v.Status.State)
			if v.Status.Message != nil {
				if t, _, _ := partsOf(id, v.Status.Message.Parts, new(int)); t != "" {
					msg = t
				}
			}
		case *a2a.TaskArtifactUpdateEvent:
			name := ""
			if v.Artifact != nil {
				name = v.Artifact.Name
				if name == "" {
					name = string(v.Artifact.ID)
				}
			}
			msg = "artifact " + name
		default:
			continue
		}
		if msg == pr.last {
			// The creation row and the first status row both say submitted: one notification says it.
			continue
		}
		pr.last = msg
		pr.n++
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
			"params": map[string]any{"progressToken": pr.token, "progress": pr.n, "message": msg}})
		if err != nil {
			continue
		}
		if !aw.s.Started() {
			if aw.s.Start() != nil {
				return
			}
		}
		if aw.s.Event("", b) != nil {
			return
		}
	}
}

func (a *answerWriter) begin() {
	if a.sse {
		_ = a.s.Start()
	}
}

func (a *answerWriter) keepAlive() { _ = a.s.KeepAlive() }

// result writes the answer (with resultType on a modern request), and reports whether it did: false when the
// client had left.
func (a *answerWriter) result(res json.RawMessage) bool {
	if a.rq.ctx().Err() != nil {
		return false
	}
	if !a.s.Started() {
		a.rq.result(res)
		return true
	}
	return a.s.Event("", responseBytes(a.rq.msg.ID, "result", a.rq.complete(res))) == nil
}

func (a *answerWriter) internal(h *Handler, what string, err error) {
	if !a.s.Started() {
		h.internal(a.rq, what, err)
		return
	}
	h.log.Warn("mcp: could not "+what, "error", err)
	b, _ := mcpwire.ErrorBody(a.rq.msg.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
	_ = a.s.Event("", b)
}
