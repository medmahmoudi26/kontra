package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/push"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// Canceller is the kontra.cancel Update; lifecycle.Client implements it.
type Canceller interface {
	Cancel(ctx context.Context, taskID a2a.TaskID) (lifecycle.CancelResult, bool, error)
}

// ReviewMessenger sends the kontra.review_message Update and waits for its result (spec §7, §14.1);
// lifecycle.Client implements it. A refusal (the validator refused: no window open, the workItemId already
// accepted; or the workflow is closed) is an error wrapping a2a.ErrUnsupportedOperation, and the
// interceptor deletes the work item. Any other error is an Update not accepted, or not completed, within
// 10 s: the caller gets -32603 (HTTP 503) and the work item is kept, because the workflow may still accept
// it (D66).
type ReviewMessenger interface {
	ReviewMessage(ctx context.Context, taskID a2a.TaskID, u lifecycle.ReviewMessageUpdate) (lifecycle.ReviewMessageResult, error)
}

// Interceptors builds the call interceptors of one service, in order (spec §15):
//
//  0. Egress rendering (spec §10.4, D167, D181): on the way out, every answer's kontra-blob:<ref> parts become
//     presigned GETs for the task they belong to (blob.Renderer); first in the chain, so its After runs last,
//     on every answer, the early ones included.
//  1. Identity: CallContext.User from the edge's principal (Name = buyer id, attributes: principal, via,
//     tenant), so the store scopes reads and the identity crosses the work queue. A key without the hire
//     scope may call no service (§2.3).
//  2. Anonymous refusals (spec §2.3): ListTasks and push configs (inline ones included) are
//     UnsupportedOperationError for the anonymous principal.
//  3. Caller keys (spec §2.5): every kontra. key leaves message, part and request metadata, except the
//     sealing keys and caller_metadata. A url part in the gate's reserved kontra-blob: scheme is
//     InvalidRequest (D181), and so is a part claiming the sealed envelope type in another form than raw
//     (ext-sealed.md §3); a sealed: required service refuses an unsealed part with InvalidRequest
//     (sealed_unsealed_part), and an optional one marks it metadata["kontra.sealed"] = false (§5). Without a bucket a part over 256 KiB is InvalidParams ("part too large: configure
//     blobs", §10.4), and so is a contextId or messageId over 256 characters (§4), before any task row.
//  4. Admission (spec §13.1) on SendMessage for a new task: the built-in allow, or the remote hook
//     (hooks.RemoteAdmission, M4 DM4-8). It fails closed: a hook error, a decision whose maps break §13.2's rules,
//     and a quote whose approveUrl is not an absolute https:// URL or whose expiresAt is not in the future
//     (hooks.CheckQuote, DM4-17) are -32603 "admission unavailable", HTTP 503 on HTTP+JSON. The pre-minted task id
//     and the decision travel to the work queue's Write on the context (hooks.AdmittedFrom).
//  5. Follow-up checks (spec §7): a message with a taskId is refused UnsupportedOperationError unless the
//     task is input-required; a taskId/contextId disagreement is InvalidParams; a terminal task is
//     UnsupportedOperationError (a2a-go itself answers InvalidParams there); a review-window message is
//     stored as a work item and sent as kontra.review_message, answered here as an early response (its large
//     parts are stored with its work item, and deleted again when the window does not take it).
//  6. Push URLs (spec §10.1): an inline config's URL, and a CreateTaskPushNotificationConfig's, is
//     checked now (push.CheckURL), so a refused URL is a synchronous InvalidParams and nothing is stored;
//     the work queue saves an inline config with the task. A follow-up's inline config is checked only
//     when its URL is new for the task (push.CheckSet): a follow-up that sends its unchanged config again
//     is never refused for a DNS change.
//     Then resends (spec §9, §15; resendAnswer, DM4-23): a blocking SendMessage that repeats a request whose
//     task stands at input-required, quoted or auth-required is answered with that task as an early response (a
//     quoted task's start re-issued first, through Starts).
//  7. Large parts (spec §10.4, D167; largeParts): with a bucket, each part over 256 KiB of a new task's first
//     message (an object of the pre-minted task) or of an input-required task's follow-up is stored and replaced
//     by its url part kontra-blob:<ref>; the rows are pending on the context for the transaction that creates
//     the task or stores the follow-up's work item. Last of the interceptors that can refuse a message, so none
//     of them leaves an object behind; once the request is over, the objects no record of the task names (a2a-go
//     or the work queue refused it, or answered it with another task) are deleted, rows and all (spec §5.4:
//     objects go with their task, and a refused request has none).
//  8. CancelInterceptor.
//  9. GetExtendedAgentCard is UnsupportedOperationError (spec §3: capabilities.extendedAgentCard false).
//
// After them, on the way out, a blocking SendMessage that a2a-go answered eventqueue.ErrQueueClosed gets the
// stored task instead (finishedTurn).
type Interceptors struct {
	Svc      store.ServiceRef
	Log      *store.Log
	Rules    hooks.KeyRules
	Admit    hooks.Admission
	Canceler Canceller
	// Reviews sends kontra.review_message; nil when no review window can open (a message to a completed
	// task is then refused at once).
	Reviews ReviewMessenger
	// PushPolicy is push.allow_private: inline and created push config URLs are checked against it.
	PushPolicy ssrf.Policy
	// MaxPart is the largest caller part stored inline (256 KiB, §10.4); zero takes upstream.MaxPart.
	MaxPart int64
	// Bucket is the claim-check bucket (blobs:); nil without one, and a caller part over MaxPart is then
	// InvalidParams (§10.4).
	Bucket blob.Store
	// Render renders the kontra-blob: parts of every answer as presigned GETs; nil renders nothing.
	Render *blob.Renderer
	// Logger logs why Admission refused a message (the caller sees only "admission unavailable"); nil logs nothing.
	Logger *slog.Logger
	// Starts re-issues a resend's start before a resend of a quoted task is answered at once (resendAnswer,
	// DM4-23): the service's lifecycle.Queue. Nil leaves such a resend to the work queue, which waits for the end
	// of the quote.
	Starts StartReissuer
}

// StartReissuer re-issues the start of the task a resend repeats, with its first work item, as the work queue does
// for a resend (spec §14.5): idempotent, and a task whose start was lost gets its workflow. lifecycle.Queue
// implements it.
type StartReissuer interface {
	ReissueStart(ctx context.Context, prev *store.TaskRow) error
}

// interceptor adapts a Before function to a2asrv.CallInterceptor.
type interceptor func(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error)

func (f interceptor) Before(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	return f(ctx, cc, req)
}

func (interceptor) After(context.Context, *a2asrv.CallContext, *a2asrv.Response) error { return nil }

// Chain returns the interceptors in order.
func (i *Interceptors) Chain() []a2asrv.CallInterceptor {
	admit := i.Admit
	if admit == nil {
		admit = hooks.BuiltinAdmission{}
	}
	ii := *i
	ii.Admit = admit
	return []a2asrv.CallInterceptor{
		&renderAnswers{render: ii.Render},
		interceptor(ii.identity),
		&finishedTurn{i: &ii},
		interceptor(ii.anonymous),
		interceptor(ii.callerKeys),
		interceptor(ii.admission),
		interceptor(ii.followUp),
		interceptor(ii.pushURLs),
		interceptor(ii.resendAnswer),
		&largeParts{i: &ii},
		&CancelInterceptor{Svc: ii.Svc, Log: ii.Log, Canceler: ii.Canceler},
		interceptor(extendedCard),
	}
}

func unsupported(msg string) error { return a2a.NewError(a2a.ErrUnsupportedOperation, msg) }
func invalid(msg string) error     { return a2a.NewError(a2a.ErrInvalidParams, msg) }

func (i *Interceptors) identity(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind == "" {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	if !p.MayHire() {
		return ctx, nil, a2a.NewError(a2a.ErrUnauthorized, "this API key may not call services (it has no hire scope)")
	}
	attrs := map[string]any{AttrPrincipal: p.ID, AttrVia: string(p.Via)}
	if p.TenantID != nil {
		attrs[AttrTenant] = p.TenantID.String()
	}
	cc.User = &a2asrv.User{Name: p.BuyerID.String(), Authenticated: p.Authenticated(), Attributes: attrs}
	return ctx, nil, nil
}

func isAnonymous(ctx context.Context) bool {
	p, ok := auth.FromContext(ctx)
	return ok && p.Kind == auth.KindAnonymous
}

func (i *Interceptors) anonymous(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if !isAnonymous(ctx) {
		return ctx, nil, nil
	}
	switch cc.Method() {
	case "ListTasks":
		return ctx, nil, unsupported("ListTasks is not available to anonymous callers")
	case "GetTaskPushConfig", "ListTaskPushConfigs", "CreateTaskPushConfig", "DeleteTaskPushConfig":
		return ctx, nil, unsupported("push notifications are not available to anonymous callers")
	case "SendMessage", "SendStreamingMessage":
		if r, ok := req.Payload.(*a2a.SendMessageRequest); ok && r.Config != nil && r.Config.PushConfig != nil {
			return ctx, nil, unsupported("push notifications are not available to anonymous callers")
		}
	}
	return ctx, nil, nil
}

func (i *Interceptors) maxPart() int64 {
	if i.MaxPart > 0 {
		return i.MaxPart
	}
	return upstream.MaxPart
}

func (i *Interceptors) callerKeys(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	switch p := req.Payload.(type) {
	case *a2a.SendMessageRequest:
		if p == nil || p.Message == nil {
			return ctx, nil, nil // a2a-go answers InvalidParams
		}
		msg := *p.Message
		if utf8.RuneCountInString(msg.ContextID) > 256 {
			return ctx, nil, invalid("contextId is over 256 characters")
		}
		if utf8.RuneCountInString(msg.ID) > 256 {
			return ctx, nil, invalid("messageId is over 256 characters")
		}
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			if u, ok := part.Content.(a2a.URL); ok && blob.Reserved(string(u)) {
				// D181: only the gate writes kontra-blob: parts, so every one in the log names its own task's object.
				return ctx, nil, a2a.NewError(a2a.ErrInvalidRequest, "a url part may not use the gate's reserved kontra-blob: scheme")
			}
			switch size := partSize(part); {
			case size <= i.maxPart():
			case i.Bucket == nil:
				return ctx, nil, invalid("part too large: configure blobs")
			case size > blob.MaxObject:
				return ctx, nil, invalid(fmt.Sprintf("part too large: at most %d bytes", blob.MaxObject))
			}
		}
		msg.Metadata = i.Rules.StripCaller(msg.Metadata)
		msg.Parts = i.stripParts(msg.Parts)
		// The sealed extension (ext-sealed.md §3, §5 "Gate"): a part claiming the envelope type in another form than
		// raw is InvalidRequest; a sealed: required service refuses an unsealed part (sealed_unsealed_part), before
		// any task row or stream; an optional one marks it kontra.sealed = false.
		parts, err := sealedCallerParts(i.Svc.Sealed, msg.Parts)
		if err != nil {
			return ctx, nil, err
		}
		msg.Parts = parts
		cp := *p
		cp.Message = &msg
		cp.Metadata = i.Rules.StripCaller(p.Metadata)
		req.Payload = &cp
	case *a2a.CancelTaskRequest:
		if p != nil && p.Metadata != nil {
			cp := *p
			cp.Metadata = i.Rules.StripCaller(p.Metadata)
			req.Payload = &cp
		}
	}
	return ctx, nil, nil
}

func (i *Interceptors) stripParts(ps a2a.ContentParts) a2a.ContentParts {
	if ps == nil {
		return nil
	}
	out := make(a2a.ContentParts, len(ps))
	for n, p := range ps {
		if p == nil {
			continue
		}
		md := i.Rules.StripCaller(p.Metadata)
		if len(md) == len(p.Metadata) {
			out[n] = p
			continue
		}
		cp := *p
		cp.Metadata = md
		out[n] = &cp
	}
	return out
}

func (i *Interceptors) admission(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if cc.Method() != "SendMessage" && cc.Method() != "SendStreamingMessage" {
		return ctx, nil, nil
	}
	r, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || r == nil || r.Message == nil || r.Message.TaskID != "" {
		return ctx, nil, nil
	}
	p, _ := auth.FromContext(ctx)
	taskID, err := ids.NewTaskID()
	if err != nil {
		return ctx, nil, err
	}
	ar := hooks.AdmitRequest{
		Operation: hooks.OpTask,
		TaskID:    string(taskID),
		Service:   hooks.Service{TenantID: i.Svc.TenantID.String(), Tenant: i.Svc.Tenant, Name: i.Svc.Name, Kind: i.Svc.Kind},
		Principal: hooks.Principal{Kind: string(p.Kind), ID: p.ID, BuyerID: p.BuyerID.String(), Via: string(p.Via)},
		Protocol:  string(store.ProtocolA2A),
		ContextID: r.Message.ContextID,
		Message:   &hooks.MessageInfo{MessageID: r.Message.ID, Metadata: r.Message.Metadata, Parts: []hooks.PartInfo{}},
	}
	if p.TenantID != nil {
		ar.Principal.TenantID = p.TenantID.String()
	}
	if sp := cc.ServiceParams(); sp != nil {
		if vals, ok := sp.Get(a2a.SvcParamExtensions); ok {
			for _, v := range vals {
				for u := range strings.SplitSeq(v, ",") {
					if u = strings.TrimSpace(u); u != "" {
						ar.Extensions = append(ar.Extensions, u)
					}
				}
			}
		}
	}
	for _, part := range r.Message.Parts {
		if part == nil {
			continue
		}
		ar.Message.Parts = append(ar.Message.Parts, hooks.PartInfo{
			MediaType: part.MediaType, Size: partSize(part), Sealed: part.MediaType == hooks.SealedMediaType,
		})
	}
	d, err := i.Admit.Admit(ctx, ar)
	if err != nil {
		// Admission fails closed (spec §13.1).
		i.logRefusal("admission failed; the message is refused (it fails closed)", "service", i.Svc.Name, "error", err)
		return ctx, nil, admissionUnavailable()
	}
	if err := i.Rules.CheckOperatorMaps(d.Metadata, d.MCPMeta); err != nil {
		i.logRefusal("an Admission decision's metadata was refused; the message is refused", "service", i.Svc.Name, "error", err)
		return ctx, nil, admissionUnavailable()
	}
	switch d.Outcome {
	case hooks.Allow:
	case hooks.Quote:
		if err := hooks.CheckQuote(d.Quote, time.Now()); err != nil {
			i.logRefusal("an Admission quote was refused; the message is refused", "service", i.Svc.Name, "error", err)
			return ctx, nil, admissionUnavailable()
		}
	case hooks.Deny:
		code, msg := a2a.ErrUnauthorized, "admission denied"
		if d.Refusal != nil {
			if e, ok := errorForCode[d.Refusal.Code]; ok {
				code = e
			}
			if d.Refusal.Message != "" {
				msg = d.Refusal.Message
			}
		}
		return ctx, nil, a2a.NewError(code, msg)
	default:
		return ctx, nil, admissionUnavailable()
	}
	return hooks.WithAdmitted(ctx, hooks.Admitted{TaskID: string(taskID), Decision: d}), nil, nil
}

// admissionUnavailable is Admission failing closed (spec §13.1): -32603 "admission unavailable", which the HTTP+JSON
// binding answers 503 (the error carries lifecycle.ErrUnavailable for sse.HTTPError, as an Update not accepted in
// time does).
func admissionUnavailable() error {
	return unavailableError{a2a.NewError(a2a.ErrInternalError, "admission unavailable")}
}

// logRefusal logs why a message was refused when the interceptors have a logger.
func (i *Interceptors) logRefusal(msg string, args ...any) {
	if i.Logger != nil {
		i.Logger.Warn("relay: "+msg, args...)
	}
}

// errorForCode maps a JSON-RPC error code (an Admission refusal's, an upstream's) to a2a-go's sentinel.
var errorForCode = map[int]error{
	-32700: a2a.ErrParseError, -32600: a2a.ErrInvalidRequest, -32601: a2a.ErrMethodNotFound,
	-32602: a2a.ErrInvalidParams, -32603: a2a.ErrInternalError, -32001: a2a.ErrTaskNotFound,
	-32002: a2a.ErrTaskNotCancelable, -32003: a2a.ErrPushNotificationNotSupported,
	-32004: a2a.ErrUnsupportedOperation, -32005: a2a.ErrUnsupportedContentType,
	-32006: a2a.ErrInvalidAgentResponse, -32007: a2a.ErrExtendedCardNotConfigured,
	-32008: a2a.ErrExtensionSupportRequired, -32009: a2a.ErrVersionNotSupported,
}

// codeForError is the JSON-RPC code of an a2a-go sentinel, -32603 when it is none of them.
func codeForError(err error) int {
	for code, sentinel := range errorForCode {
		if errors.Is(err, sentinel) {
			return code
		}
	}
	return -32603
}

// largeParts is the interceptor that stores large parts (spec §10.4, D167): with a bucket, each part over MaxPart
// of a new task's first message becomes an object of the task Admission pre-minted, and each of an input-required
// task's follow-up an object of that task, replaced in the request by its url part kontra-blob:<ref>; the rows are
// pending on the returned context, and the transaction that creates the task (store.Log.CreateTask) or stores the
// follow-up's work item (store.Log.InsertWorkItem) writes them. For a new task, the request's hash as the caller
// sent it goes with them (store.WithRequestHash), so a resend is still recognized whatever refs its parts were
// given (spec §15); a resend is recognized here first, and stores nothing.
//
// It comes after every interceptor that can refuse a message, so none of them leaves an object behind. a2a-go
// (a message without an id, a task that ended meanwhile) and the work queue (a refused follow-up, a failed write,
// the later of two identical requests, answered with the earlier one's task) still can: After settles what the
// request stored once it is over, deleting the objects no record of their task names, with their rows (spec §5.4:
// a task's objects go with it, and a request that made no task, or whose message was not taken, has none).
type largeParts struct {
	a2asrv.PassthroughCallInterceptor
	i *Interceptors
}

// followUpKey carries the task row the followUp interceptor read for a follow-up it lets through.
type followUpKey struct{}

// storedKey carries what largeParts stored for the request.
type storedKey struct{}

func (l *largeParts) Before(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	i := l.i
	if i.Bucket == nil || (cc.Method() != "SendMessage" && cc.Method() != "SendStreamingMessage") {
		return ctx, nil, nil
	}
	r, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || r == nil || r.Message == nil || !slices.ContainsFunc(r.Message.Parts, i.large) {
		return ctx, nil, nil
	}
	var tenant, task uuid.UUID
	if r.Message.TaskID != "" {
		row, ok := ctx.Value(followUpKey{}).(*store.TaskRow)
		if !ok {
			// followUp lets a follow-up through only once it read the caller's task, input-required.
			return ctx, nil, a2a.NewError(a2a.ErrInternalError, "the follow-up's task was not read")
		}
		tenant, task = row.TenantID, row.ID
	} else {
		adm, ok := hooks.AdmittedFrom(ctx)
		if !ok {
			return ctx, nil, nil // the work queue refuses a new task without Admission's decision
		}
		var err error
		if task, err = ids.ParseTaskID(adm.TaskID); err != nil {
			return ctx, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
		}
		hash, err := store.RequestHash(r)
		if err != nil {
			return ctx, nil, err
		}
		ctx = store.WithRequestHash(ctx, hash)
		o, err := CallerOwner(ctx)
		if err != nil {
			return ctx, nil, err
		}
		prev, err := i.Log.FindResend(ctx, i.Svc, o, r.Message.ID, hash, time.Now().Add(-lifecycle.ResendWindow))
		if err != nil {
			return ctx, nil, err
		}
		if prev != nil {
			return ctx, nil, nil // a resend: the work queue answers it with its task, and nothing is stored
		}
		tenant = i.Svc.TenantID
	}
	ctx, msg, st, err := i.externalize(ctx, r.Message, tenant, task)
	if err != nil {
		return ctx, nil, err
	}
	cp := *r
	cp.Message = msg
	req.Payload = &cp
	return context.WithValue(ctx, storedKey{}, st), nil, nil
}

// After settles the request's objects once it is over: the first answer (a blocking SendMessage's, a stream's
// first event) or its error, after the work queue's Write returned.
func (l *largeParts) After(ctx context.Context, _ *a2asrv.CallContext, _ *a2asrv.Response) error {
	if st, ok := ctx.Value(storedKey{}).(*storedParts); ok {
		st.once.Do(func() { l.i.settle(ctx, st) })
	}
	return nil
}

// large reports whether a part goes to the bucket.
func (i *Interceptors) large(p *a2a.Part) bool { return partSize(p) > i.maxPart() }

// storedParts is what one request stored in the bucket: the objects of its message's large parts, all of one
// task.
type storedParts struct {
	tenant, task uuid.UUID
	rows         []blob.Row
	once         sync.Once
}

func (s *storedParts) refs() []blob.Ref {
	out := make([]blob.Ref, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r.Ref)
	}
	return out
}

// settleTimeout bounds the settling of one request's objects, which runs detached from the caller's cancellation:
// a caller that went away must not leave them behind.
const settleTimeout = 30 * time.Second

// externalize stores every part of msg over MaxPart in the bucket as an object of task and returns a copy of msg
// with each replaced by its url part kontra-blob:<ref> (spec §10.4, D167), and what it stored. The objects' rows
// are pending on the returned context, for the transaction that stores the message. A bucket that refuses an
// object is the caller's -32603: the objects already stored for msg are deleted, and nothing is stored for it.
func (i *Interceptors) externalize(ctx context.Context, msg *a2a.Message, tenant, task uuid.UUID) (context.Context, *a2a.Message, *storedParts, error) {
	pending := blob.PendingFrom(ctx)
	if pending == nil {
		pending = blob.NewPending()
		ctx = blob.WithPending(ctx, pending)
	}
	out := *msg
	out.Parts = append(a2a.ContentParts(nil), msg.Parts...)
	st := &storedParts{tenant: tenant, task: task}
	for n, p := range out.Parts {
		if !i.large(p) {
			continue
		}
		np, row, err := blob.Externalize(ctx, i.Bucket, tenant, task, p)
		if err != nil {
			slog.Default().Warn("relay: a caller part over the inline limit could not be stored in the bucket", "task", task, "error", err)
			i.deleteObjects(ctx, st, st.refs())
			return ctx, nil, nil, a2a.NewError(a2a.ErrInternalError, "the part could not be stored")
		}
		pending.Add(row)
		st.rows = append(st.rows, row)
		out.Parts[n] = np
	}
	return ctx, &out, st, nil
}

// settle deletes the objects st holds that no record of their task names now that the request is over, with their
// blobs rows (store.Log.DropUnnamedBlobs): those of a new task whose row was never written, or of a message whose
// work item was deleted (refused). A message the work queue or the review window may still take keeps its work
// item, and so its objects, which retention deletes with the task otherwise (D66).
func (i *Interceptors) settle(ctx context.Context, st *storedParts) {
	if st == nil || len(st.rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	gone, err := i.Log.DropUnnamedBlobs(ctx, st.task, st.refs())
	if err != nil {
		slog.Default().Warn("relay: the objects of a request that was not taken could not be settled", "task", st.task, "error", err)
		return
	}
	i.deleteObjects(ctx, st, gone)
}

// deleteObjects deletes objects of st's task by key, best effort: one left is logged (retention deletes it with
// its task, when the task exists).
func (i *Interceptors) deleteObjects(ctx context.Context, st *storedParts, refs []blob.Ref) {
	if len(refs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	for _, ref := range refs {
		if err := i.Bucket.Delete(ctx, blob.Key{Tenant: st.tenant, Task: st.task, Ref: ref}); err != nil {
			slog.Default().Warn("relay: an object of a request that was not taken could not be deleted", "task", st.task, "ref", ref, "error", err)
		}
	}
}

func (i *Interceptors) followUp(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if cc.Method() != "SendMessage" && cc.Method() != "SendStreamingMessage" {
		return ctx, nil, nil
	}
	r, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || r == nil || r.Message == nil || r.Message.TaskID == "" {
		return ctx, nil, nil
	}
	id, err := ids.ParseTaskID(string(r.Message.TaskID))
	if err != nil {
		return ctx, nil, err
	}
	o, err := CallerOwner(ctx)
	if err != nil {
		return ctx, nil, err
	}
	row, err := i.Log.LoadScoped(ctx, i.Svc, o, id)
	if err != nil {
		return ctx, nil, err
	}
	if r.Message.ContextID != "" && r.Message.ContextID != row.ContextID {
		return ctx, nil, invalid("the message's taskId and contextId disagree")
	}
	switch row.State {
	case store.StateInputRequired:
		// The follow-up may be taken: largeParts stores its large parts once pushURLs has checked it.
		return context.WithValue(ctx, followUpKey{}, row), nil, nil
	case store.StateSubmitted, store.StateWorking:
		return ctx, nil, unsupported("the task is running; it takes no input until it asks for some")
	case store.StateAuthRequired:
		return ctx, nil, unsupported("the task waits for credentials that arrive out of band, not for a message")
	case store.StateQuoted:
		return ctx, nil, unsupported("a quoted task is accepted on the operator's page, not in the protocol")
	case store.StateCompleted:
		task, err := i.reviewMessage(ctx, r, o, row)
		if err != nil {
			return ctx, nil, err
		}
		return ctx, task, nil
	}
	return ctx, nil, unsupported(fmt.Sprintf("the task is %s and takes no more messages", row.State))
}

// reviewMessage offers a message to a completed task to the operator's review window (spec §7): the
// sanitized message is stored as a work item and sent as kontra.review_message; an accepted one is
// appended by the workflow and the caller gets the task. It is never forwarded upstream.
func (i *Interceptors) reviewMessage(ctx context.Context, r *a2a.SendMessageRequest, o store.Owner, row *store.TaskRow) (*a2a.Task, error) {
	if i.Reviews == nil {
		return nil, unsupported("the task is completed and no review window is open")
	}
	meta := i.Rules.CallerMetadataOnly(r.Message.Metadata)
	if b, err := json.Marshal(meta); err != nil || len(b) > hooks.MaxMapBytes {
		return nil, invalid("caller metadata over 16 KiB")
	}
	wid, err := ids.NewWorkItemID()
	if err != nil {
		return nil, err
	}
	stored := *r
	msg := *r.Message
	var parts *storedParts
	if i.Bucket != nil && slices.ContainsFunc(msg.Parts, i.large) {
		// The work item holds the message as the window's append will log it, so its large parts go to the bucket
		// now, their rows with the item (spec §10.4, D167); a message the window does not take has them deleted
		// with its item (settle).
		var m *a2a.Message
		if ctx, m, parts, err = i.externalize(ctx, &msg, row.TenantID, row.ID); err != nil {
			return nil, err
		}
		msg = *m
	}
	msg.TaskID, msg.ContextID = a2a.TaskID(row.ID.String()), row.ContextID
	stored.Message = &msg
	if r.Config != nil && r.Config.PushConfig != nil {
		// SPEC-CONFLICT (D110): spec §10.1 saves an inline push config before the work item is written, but a
		// review-window message is to a task that has ended: a config set on it would deliver nothing (an
		// ended task has no change left to push, internal/push insertSQL), and its webhook credentials must
		// not be stored in work_items, which hold no credentials (0001). The closest faithful behaviour: the
		// inline config is dropped from the stored request and not saved, and the message is not refused
		// for it (a2a-go's client sends its default config with every message).
		cfg := *r.Config
		cfg.PushConfig = nil
		stored.Config = &cfg
	}
	if err := i.Log.InsertWorkItem(ctx, nil, store.WorkItem{
		ID: wid, TaskID: row.ID, Kind: store.WorkExecute, Payload: store.WorkPayload{Request: &stored, Owner: o},
	}); err != nil {
		i.settle(ctx, parts)
		return nil, err
	}
	res, err := i.Reviews.ReviewMessage(ctx, a2a.TaskID(row.ID.String()), lifecycle.ReviewMessageUpdate{
		WorkItemID: wid.String(), MessageID: msg.ID, Metadata: meta,
	})
	switch {
	case errors.Is(err, a2a.ErrUnsupportedOperation):
		i.deleteItem(ctx, wid, row.ID)
		i.settle(ctx, parts)
		return nil, unsupported("the task is completed and no review window is open")
	case err != nil:
		// Not accepted (or not completed) within 10 s: the item is kept, the workflow may still take it (D66), and
		// so are its objects.
		return nil, notInTime("the review message was not accepted in time", err)
	case !res.Accepted:
		i.deleteItem(ctx, wid, row.ID)
		i.settle(ctx, parts)
		return nil, unsupported("the review message was not accepted")
	}
	after, err := i.Log.LoadScoped(ctx, i.Svc, o, row.ID)
	if err != nil {
		return nil, err
	}
	return after.Snapshot, nil
}

func (i *Interceptors) deleteItem(ctx context.Context, id, taskID uuid.UUID) {
	_ = i.Log.DeleteWorkItem(ctx, id, taskID)
}

func (i *Interceptors) pushURLs(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	var pc *a2a.PushConfig
	switch p := req.Payload.(type) {
	case *a2a.SendMessageRequest:
		if p == nil || p.Config == nil || p.Config.PushConfig == nil {
			return ctx, nil, nil
		}
		if p.Message != nil && p.Message.TaskID != "" {
			// A follow-up (the followUp interceptor already found the task the caller's): its config was
			// checked when it was set, unless its URL is new for the task.
			id, err := ids.ParseTaskID(string(p.Message.TaskID))
			if err != nil {
				return ctx, nil, err
			}
			return ctx, nil, push.CheckSet(ctx, i.Log.DB(), id, p.Config.PushConfig, i.PushPolicy)
		}
		pc = p.Config.PushConfig
	case *a2a.PushConfig:
		pc = p
	}
	if pc == nil {
		return ctx, nil, nil
	}
	return ctx, nil, push.CheckURL(ctx, pc.URL, i.PushPolicy)
}

func extendedCard(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	if cc.Method() == "GetExtendedAgentCard" {
		return ctx, nil, unsupported("the gate serves no extended agent card")
	}
	return ctx, nil, nil
}

// CancelInterceptor answers CancelTask itself (spec §8, D4): a terminal task is TaskNotCancelableError;
// otherwise the kontra.cancel Update, awaited up to 60 s, whose outcome is the canceled task, the
// upstream's error while the task continues, or past 60 s the stored task as it is.
type CancelInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	Svc      store.ServiceRef
	Log      *store.Log
	Canceler Canceller
}

var _ a2asrv.CallInterceptor = (*CancelInterceptor)(nil)

// Before implements a2asrv.CallInterceptor.
func (c *CancelInterceptor) Before(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if cc.Method() != "CancelTask" {
		return ctx, nil, nil
	}
	r, ok := req.Payload.(*a2a.CancelTaskRequest)
	if !ok || r == nil {
		return ctx, nil, a2a.ErrInvalidParams
	}
	id, err := ids.ParseTaskID(string(r.ID))
	if err != nil {
		return ctx, nil, err
	}
	o, err := CallerOwner(ctx)
	if err != nil {
		return ctx, nil, err
	}
	row, err := c.Log.LoadScoped(ctx, c.Svc, o, id)
	if err != nil {
		return ctx, nil, err
	}
	if row.State.Terminal() {
		return ctx, nil, a2a.NewError(a2a.ErrTaskNotCancelable, fmt.Sprintf("the task is %s", row.State))
	}
	if c.Canceler == nil {
		return ctx, nil, a2a.NewError(a2a.ErrInternalError, "cancel is unavailable")
	}
	res, done, err := c.Canceler.Cancel(ctx, a2a.TaskID(id.String()))
	switch {
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		return ctx, nil, a2a.NewError(a2a.ErrTaskNotCancelable, "the task is no longer cancelable")
	case err != nil:
		return ctx, nil, notInTime("the cancel was not accepted in time", err)
	}
	if done {
		switch res.Outcome {
		case lifecycle.CancelNotCancelable:
			return ctx, nil, a2a.NewError(a2a.ErrTaskNotCancelable, upstreamText(res.UpstreamError, "the service cannot cancel the task"))
		case lifecycle.CancelUnsupported:
			return ctx, nil, a2a.NewError(a2a.ErrUnsupportedOperation, upstreamText(res.UpstreamError, "the service does not support cancel"))
		}
	}
	// Canceled (the terminal event is in the log, D79), continues (the upstream answered with its task still
	// running: the task as the gate stores it is that answer with gate ids), or past 60 s: the stored task as
	// it is.
	after, err := c.Log.LoadScoped(ctx, c.Svc, o, id)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, after.Snapshot, nil
}

// notInTime is the -32603 answer to an Update not accepted (or not completed) within its wait. When the
// cause is lifecycle.ErrUnavailable it stays in the chain, so the HTTP+JSON binding can answer 503 (spec
// §14.1: "-32603 / HTTP 503"); the caller's message is msg either way, never the cause's text.
func notInTime(msg string, cause error) error {
	e := a2a.NewError(a2a.ErrInternalError, msg)
	if errors.Is(cause, lifecycle.ErrUnavailable) {
		return unavailableError{e}
	}
	return e
}

// unavailableError is an *a2a.Error that also wraps lifecycle.ErrUnavailable.
type unavailableError struct{ err *a2a.Error }

func (e unavailableError) Error() string   { return e.err.Error() }
func (e unavailableError) Unwrap() []error { return []error{e.err, lifecycle.ErrUnavailable} }

func upstreamText(e *lifecycle.UpstreamError, fallback string) string {
	if e == nil || e.Message == "" {
		return fallback
	}
	return upstream.Cap(e.Message)
}

// renderAnswers is egress rendering for a2a-go's answers (spec §10.4, D167, D181): on the way out, every
// kontra-blob:<ref> part of a task, a message, a list of tasks or a streamed event becomes a presigned GET valid
// 15 minutes, for the task its blobs row names only (blob.Renderer). It is first in the chain, so its After
// runs last and sees every answer, the interceptors' early ones (a cancel's task, a review message's, the
// stored task finishedTurn answers) included. A request that arrived on a hiring node's stream keeps the refs
// (blob.WithKeepRefs, M3 WP-03).
type renderAnswers struct {
	a2asrv.PassthroughCallInterceptor
	render *blob.Renderer
}

func (a *renderAnswers) After(ctx context.Context, _ *a2asrv.CallContext, resp *a2asrv.Response) error {
	if a.render == nil || resp.Payload == nil {
		return nil
	}
	var (
		out any
		err error
	)
	switch p := resp.Payload.(type) {
	case *a2a.Task:
		out, err = a.render.Task(ctx, p)
	case *a2a.ListTasksResponse:
		if p == nil {
			return nil
		}
		cp := *p
		cp.Tasks = make([]*a2a.Task, len(p.Tasks))
		for n, t := range p.Tasks {
			if cp.Tasks[n], err = a.render.Task(ctx, t); err != nil {
				break
			}
		}
		out = &cp
	case a2a.Event:
		out, err = a.render.Event(ctx, p)
	default:
		return nil
	}
	if err != nil {
		slog.Default().Warn("relay: an answer's parts could not be rendered", "error", err)
		resp.Payload, resp.Err = nil, a2a.NewError(a2a.ErrInternalError, "the task's parts could not be rendered")
		return nil
	}
	resp.Payload = out
	return nil
}

// finishedTurn works around a race in a2a-go v2.7.0's remote subscription (contract test P15): when a turn
// ends at input-required before the subscription of a blocking SendMessage reads the store, the snapshot
// it reads is already input-required (not terminal, so it keeps reading), every queued event is older than
// the snapshot and skipped, and the pull reader, having delivered its final event, closes: SendMessage
// answers eventqueue.ErrQueueClosed instead of the task. The answer a blocking SendMessage owes then is
// exactly the stored task (A2A §3.1.1: it returns on the first input-required state), so this interceptor
// answers it, for the task the request named or Admission pre-minted.
type finishedTurn struct {
	a2asrv.PassthroughCallInterceptor
	i *Interceptors
}

type requestTaskKey struct{}

func (f *finishedTurn) Before(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if r, ok := req.Payload.(*a2a.SendMessageRequest); ok && r != nil && r.Message != nil && r.Message.TaskID != "" {
		ctx = context.WithValue(ctx, requestTaskKey{}, r.Message.TaskID)
	}
	return ctx, nil, nil
}

func (f *finishedTurn) After(ctx context.Context, cc *a2asrv.CallContext, resp *a2asrv.Response) error {
	if cc.Method() != "SendMessage" || resp.Err == nil || !errors.Is(resp.Err, eventqueue.ErrQueueClosed) {
		return nil
	}
	id, _ := ctx.Value(requestTaskKey{}).(a2a.TaskID)
	if id == "" {
		if adm, ok := hooks.AdmittedFrom(ctx); ok {
			id = a2a.TaskID(adm.TaskID)
		}
	}
	tid, err := ids.ParseTaskID(string(id))
	if err != nil {
		return nil
	}
	o, err := CallerOwner(ctx)
	if err != nil {
		return nil
	}
	row, err := f.i.Log.LoadScoped(ctx, f.i.Svc, o, tid)
	if err != nil {
		return nil
	}
	resp.Payload, resp.Err = row.Snapshot, nil
	return nil
}
