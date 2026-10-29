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

// PlainWait is how long a plain hire and every reply wait for the task to end, ask a question or be quoted
// before answering {taskId, state} (§12.2, D272): below the 60 s request timeout of the MCP SDKs.
const PlainWait = 25 * time.Second

// PlainBudget bounds a plain hire's or reply's request from its arrival to its answer: the plain wait is cut so that
// the answer is written within it, after Admission (up to 30 s, hooks.admission.timeout) and the task's start or
// follow-up (up to lifecycle.AcceptWait), so that it stays below the 60 s request timeout of the MCP SDKs, whose
// expiry closes the stream and, on a modern hire, cancels the task (§12.5, D178). plainFloor is the least a plain
// wait lasts all the same.
const (
	PlainBudget = 50 * time.Second
	plainFloor  = time.Second
)

// RetryWindow is how long after a task left quoted a hire with the same resend key answers that task (§12.3
// Retries), and how long a session's GET streams still get its notifications/elicitation/complete (D288).
const RetryWindow = relay.HireRetryWindow

// HireRequest is one hire: the A2A message the gate builds (its messageId minted by the gate, its parts from the
// arguments, the caller's contextId), the service, and the resend key of §12.3 (FirstRequest).
type HireRequest = relay.HireRequest

// HireOutcome is the task a hire created or found again (§12.3 Retries), as it is now.
type HireOutcome = relay.HireOutcome

// Hirer creates and follows up hire tasks. The gate implements it over the A2A service's handler, work queue and
// TaskLifecycle (*relay.Hirer; spec §6, §7, §15): the task is an ordinary A2A task (protocol a2a) marked tasks.hired,
// whose first_message_id is the gate-minted messageId and whose first_request is HireRequest.FirstRequest (§12.3).
type Hirer interface {
	// Hire runs Admission (operation task, §13.1) and creates the task submitted or quoted; or answers the
	// existing task a retry names: one of the same (tenant, service, buyer, principal, first_request) that is
	// quoted, or left quoted less than RetryWindow ago, without sending anything.
	Hire(ctx context.Context, req HireRequest) (HireOutcome, error)
	// Reply sends parts as the §7 follow-up of a hire task of p's buyer in input-required; refused as §7 says
	// otherwise (a quoted task always). It returns the task's version before the follow-up.
	Reply(ctx context.Context, p auth.Principal, task *store.TaskRow, parts a2a.ContentParts) (int64, error)
}

// hire answers the hire tool (§12.2, §12.3): the service a ref names when the principal may hire it (else the
// unknown-service answer); an MCP service, one whose sealing is required, or one whose served card requires an
// extension (D294) is a tool execution error naming the route to use, and no task; then the task through the
// Hirer, answered as §12.3 says: quoted, -32042 to a session that declared elicitation.url and otherwise the approve
// URL at once (CreateTaskResult working, or the plain text); any other task, CreateTaskResult when task-augmented,
// else after the plain wait. A modern request's task is answered by answerModernHire (§12.5); its creation runs on a
// context its client cannot cancel (bounded by modernHireBound), so that a client that leaves meanwhile has its task
// cancelled rather than running unseen (D178).
func (h *Handler) hire(rq *request, args json.RawMessage, augmented bool) {
	if rq.modern && h.deps.Tasks != nil {
		// A modern hire may cancel its task when its client leaves: the replica's drain awaits it (HireTasks.Wait).
		defer h.deps.Tasks.hold()()
	}
	a, parts, err := parseHireArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	ref, err := ParseRef(a.Service, h.deps.Multi)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	svc, err := h.deps.Catalog.Hireable(rq.ctx(), rq.p, ref)
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
		rq.result(errorResult("service "+ref.String()+" is an MCP server: hire takes A2A agents only. "+
			"Call its tools directly at "+base+"/mcp/"+ref.String()+".", nil))
		return
	case svc.ref.Sealed == string(config.SealedRequired):
		rq.result(errorResult("service "+ref.String()+" takes sealed messages only, and hire cannot seal "+
			"tool arguments. Send it A2A messages through a node (kontra connect seals them) at "+base+"/agents/"+ref.String()+".", nil))
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
	msg := &a2a.Message{ID: mid.String(), Role: a2a.MessageRoleUser, ContextID: a.ContextID, Parts: parts}
	hctx := rq.ctx()
	if rq.modern {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(context.WithoutCancel(hctx), modernHireBound)
		defer cancel()
	}
	out, err := h.deps.Hirer.Hire(hctx, HireRequest{Principal: rq.p, Service: svc.ref, Message: msg, FirstRequest: key})
	var xe *relay.ExtensionsRequiredError
	switch {
	case errors.As(err, &xe):
		rq.result(errorResult(extensionsText("service "+ref.String(), "hire", xe.URIs,
			base+"/agents/"+ref.String())+" No task was created.", nil))
		return
	case err != nil:
		h.refused(rq, "hire", err)
		return
	}
	row := out.Task
	if out.Existing {
		h.log.Info("hire answered with an existing task (a retry)", "task_id", row.ID, "state", row.State, "principal", rq.p.ID)
	} else {
		h.log.Info("hire created a task", "service", ref.String(), "task_id", row.ID, "state", row.State, "principal", rq.p.ID)
	}
	if rq.modern {
		h.answerModernHire(rq, out, svc.ref, augmented)
		return
	}
	if row.State == store.StateQuoted {
		h.quoted(rq, row, svc.ref, augmented)
		return
	}
	if augmented {
		h.writeCreateTask(rq, row, svc.ref)
		return
	}
	h.waitAndAnswer(rq, row.ID, 0)
}

// quoted answers a hire whose task is quoted (§12.3 step 2): a session that declared elicitation.url gets -32042
// with the task's URL elicitation (the one already sent when this is a retry; the session that asks now receives its
// notification); any other the approve URL at once, as CreateTaskResult (working, the URL in statusMessage and
// _meta) when task-augmented, else as text. Nothing is asked of the client, so nothing it answers is read as approval.
func (h *Handler) quoted(rq *request, row *store.TaskRow, svc store.ServiceRef, augmented bool) {
	if rq.sess != nil && declaresURL(rq.sess.ClientCaps) {
		id, ok, err := h.deps.Elicitations.Record(rq.ctx(), row.ID, rq.sess.Hash)
		if err != nil {
			h.internal(rq, "record the URL elicitation", err)
			return
		}
		if ok {
			u := approveURL(row.Snapshot)
			writeError(rq.w, http.StatusOK, rq.msg.ID, CodeURLElicitationRequired, "This request requires approval",
				map[string]any{"elicitations": []map[string]any{{"mode": "url", "elicitationId": id, "url": u,
					"message": "A person must approve this task's quote before it runs. Open the URL to review and approve it; " +
						"then call hire again with the same arguments."}}})
			return
		}
		// The task left quoted meanwhile: answer it as it is now.
		r, err := h.deps.Log.Load(rq.ctx(), row.ID)
		if err != nil {
			h.internal(rq, "read the task", err)
			return
		}
		if augmented {
			h.writeCreateTask(rq, r, svc)
			return
		}
		h.waitAndAnswer(rq, r.ID, 0)
		return
	}
	if augmented {
		h.writeCreateTask(rq, row, svc)
		return
	}
	writeResult(rq.w, rq.sid, rq.msg.ID, quotedResult(row))
}

// quotedResult is the plain answer for a quoted task (§12.3): the URL and the task id as text, {taskId, state} and
// the URL in _meta (D21).
func quotedResult(row *store.TaskRow) json.RawMessage {
	u := approveURL(row.Snapshot)
	text := "This task needs a person to approve its quote before it runs: open " + u + " to review and approve it (task " +
		row.ID.String() + "). Then call hire again with the same arguments to get its result."
	return textResult(text, PlainState{TaskID: row.ID.String(), State: string(row.State.A2A())}, map[string]any{MetaApproveURL: u})
}

// writeCreateTask answers a task-augmented hire with its CreateTaskResult (§11.5 step 2, §12.3).
func (h *Handler) writeCreateTask(rq *request, row *store.TaskRow, svc store.ServiceRef) {
	res, err := createTaskResult(row, svc, rq.reader())
	if err != nil {
		h.internal(rq, "write the task", err)
		return
	}
	writeResult(rq.w, rq.sid, rq.msg.ID, res)
}

// reply answers the reply tool (§12.2, §7): the hire task of the principal's buyer (any other id is the
// unknown-task answer); a quoted task is refused before anything is sent (§13.2: approval does not happen in the
// protocol); a task not at input-required is refused (§7); otherwise the follow-up through the Hirer, and the plain
// wait for the turn it started.
func (h *Handler) reply(rq *request, args json.RawMessage) {
	a, parts, err := parseReplyArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.deps.Log.LoadHired(rq.ctx(), rq.p.BuyerID, a.TaskID)
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		rq.result(errorResult("unknown task", nil))
		return
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	}
	switch {
	case row.State == store.StateQuoted:
		u := approveURL(row.Snapshot)
		rq.result(errorResult("The task is waiting for a person to approve its quote at "+u+
			"; a reply cannot approve it, and nothing was sent.", map[string]any{MetaApproveURL: u}))
		return
	case row.State != store.StateInputRequired:
		rq.result(errorResult("The task is "+string(row.State)+" and takes no input now; nothing was sent.", nil))
		return
	}
	after, err := h.deps.Hirer.Reply(rq.ctx(), rq.p, row, parts)
	var xe *relay.ExtensionsRequiredError
	switch {
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

// extensionsText is the tool execution error of a hire or a reply to a service whose served card requires extensions
// (spec §2.4, D294): a tool call activates none, so the route is A2A with A2A-Extensions, as the required-sealed
// answer names its route.
func extensionsText(who, tool string, uris []string, a2aURL string) string {
	s, them := "", "it"
	if len(uris) != 1 {
		s, them = "s", "them"
	}
	return who + " requires the A2A extension" + s + " " + strings.Join(uris, ", ") + ", which " + tool +
		" cannot activate. Send it A2A messages that activate " + them + " in the A2A-Extensions header, at " + a2aURL + "."
}

// refused answers an error the Hirer returned (the A2A route's own refusals): an anonymous tenant's cap is 429 with
// -32600 "rate limited" and Retry-After (as the MCP relay answers it, spec/tunnel.md §6.2); "admission unavailable"
// and a lifecycle that did not accept the request in time are 503 with -32603 (§13.1, §7); any other A2A refusal
// (Admission's refusal with its code and message, "anonymous storage limit", a part or a follow-up refused) is that
// JSON-RPC error; anything else is the gate's own failure (what names the request in the log).
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
		h.log.Warn("mcp: hire or reply refused as unavailable", "error", err)
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
// reply's turn; 0 for a new task), or to be quoted, and answers what it then is (§12.2, D272): an ended task as
// tasks/result renders it; input-required as §12.4's reply-tool text; quoted as the approve URL; a task still
// running with a text naming the A2A URL that follows it. A request that accepts text/event-stream is answered on an
// event stream with keep-alives while it waits. It reports whether the answer was written: false when the client left
// first (its request context ended), which a modern hire reads as the request's cancellation (answerModernHire).
func (h *Handler) waitAndAnswer(rq *request, id uuid.UUID, after int64) bool {
	aw := newAnswerWriter(rq)
	ctx := rq.ctx()
	wake, stop := h.deps.Waker.WaitTask(ctx, id)
	defer stop()
	deadline := time.NewTimer(h.plainWait(rq))
	defer deadline.Stop()
	reread := time.NewTicker(time.Second)
	defer reread.Stop()
	keep := time.NewTicker(keepAliveEvery)
	defer keep.Stop()
	aw.begin()
	var row *store.TaskRow
	for {
		r, err := h.deps.Log.Load(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			aw.internal(h, "read the task", err)
			return true
		}
		row = r
		if h.turnOver(ctx, row, after) {
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

// turnOver reports whether a plain wait ends at row: terminal, quoted, or input-required with its question asked
// after version after.
func (h *Handler) turnOver(ctx context.Context, row *store.TaskRow, after int64) bool {
	switch {
	case row.State.Terminal(), row.State == store.StateQuoted:
		return true
	case row.State != store.StateInputRequired:
		return false
	case after == 0:
		return true
	}
	seq, err := h.deps.Log.StatusSeq(ctx, row.ID)
	return err == nil && seq > after
}

// plainAnswer is the CallToolResult of a plain hire or reply for the task as row has it (§12.2).
func (h *Handler) plainAnswer(ctx context.Context, row *store.TaskRow, p auth.Principal) json.RawMessage {
	switch {
	case row.State.Terminal():
		task := row.Snapshot
		if h.deps.Render != nil {
			if t, err := h.deps.Render.Task(ctx, task); err == nil {
				task = t
			}
		}
		b, err := Render(task, row.State)
		if err != nil {
			return errorResult("the task's result could not be rendered", nil)
		}
		return b
	case row.State == store.StateQuoted:
		return quotedResult(row)
	case row.State == store.StateInputRequired:
		_, text, _ := Question(row.Snapshot, Reader{Principal: p, Era: mcpwire.EraLegacy})
		return textResult(text, PlainState{TaskID: row.ID.String(), State: string(row.State.A2A())},
			map[string]any{MetaInputRequired: true})
	}
	follow := strings.TrimRight(h.deps.PublicURL, "/") + "/agents/" + h.serviceRef(row)
	text := "The task " + row.ID.String() + " is still " + string(row.State) + ". Follow it with GetTask at " + follow +
		" (the same credential), or call reply when it asks a question."
	return textResult(text, PlainState{TaskID: row.ID.String(), State: string(row.State.A2A())}, nil)
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
}

func newAnswerWriter(rq *request) *answerWriter {
	return &answerWriter{rq: rq, sse: strings.Contains(rq.r.Header.Get("Accept"), "text/event-stream"),
		s: mcprelay.NewGateStream(rq.w, rq.sid)}
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
