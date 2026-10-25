package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/auth"
	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/ssrf"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/upstream"
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
//  1. Identity: CallContext.User from the edge's principal (Name = buyer id, attributes: principal, via,
//     tenant), so the store scopes reads and the identity crosses the work queue. A key without the hire
//     scope may call no service (§2.3).
//  2. Anonymous refusals (spec §2.3): ListTasks and push configs (inline ones included) are
//     UnsupportedOperationError for the anonymous principal.
//  3. Caller keys (spec §2.5): every kontra. key leaves message, part and request metadata, except the
//     sealing keys and caller_metadata. A part over 256 KiB is InvalidParams ("part too large: configure
//     blobs", §10.4), and so is a contextId or messageId over 256 characters (§4), before any task row.
//  4. Admission (spec §13.1) on SendMessage for a new task: the built-in allow in M1. The pre-minted task
//     id and the decision travel to the work queue's Write on the context (hooks.AdmittedFrom).
//  5. Follow-up checks (spec §7): a message with a taskId is refused UnsupportedOperationError unless the
//     task is input-required; a taskId/contextId disagreement is InvalidParams; a terminal task is
//     UnsupportedOperationError (a2a-go itself answers InvalidParams there); a review-window message is
//     stored as a work item and sent as kontra.review_message, answered here as an early response.
//  6. Push URLs (spec §10.1): an inline config's URL, and a CreateTaskPushNotificationConfig's, is
//     checked now, so a refused URL is a synchronous InvalidParams and nothing is stored; the work queue
//     saves an inline config with the task.
//  7. CancelInterceptor.
//  8. GetExtendedAgentCard is UnsupportedOperationError (spec §3: capabilities.extendedAgentCard false).
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
	// MaxPart is the largest caller part without a bucket (256 KiB, §10.4); zero takes upstream.MaxPart.
	MaxPart int64
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
		interceptor(ii.identity),
		&finishedTurn{i: &ii},
		interceptor(ii.anonymous),
		interceptor(ii.callerKeys),
		interceptor(ii.admission),
		interceptor(ii.followUp),
		interceptor(ii.pushURLs),
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
			if partSize(part) > i.maxPart() {
				return ctx, nil, invalid("part too large: configure blobs")
			}
		}
		msg.Metadata = i.Rules.StripCaller(msg.Metadata)
		msg.Parts = i.stripParts(msg.Parts)
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
		return ctx, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	if err := i.Rules.CheckOperatorMaps(d.Metadata, d.MCPMeta); err != nil {
		return ctx, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	switch d.Outcome {
	case hooks.Allow:
	case hooks.Quote:
		if d.Quote == nil || !strings.HasPrefix(d.Quote.ApproveURL, "https://") || d.Quote.ExpiresAt == "" {
			return ctx, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
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
		return ctx, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	return hooks.WithAdmitted(ctx, hooks.Admitted{TaskID: string(taskID), Decision: d}), nil, nil
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
		return ctx, nil, nil
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
	msg.TaskID, msg.ContextID = a2a.TaskID(row.ID.String()), row.ContextID
	stored.Message = &msg
	if err := i.Log.InsertWorkItem(ctx, nil, store.WorkItem{
		ID: wid, TaskID: row.ID, Kind: store.WorkExecute, Payload: store.WorkPayload{Request: &stored, Owner: o},
	}); err != nil {
		return nil, err
	}
	res, err := i.Reviews.ReviewMessage(ctx, a2a.TaskID(row.ID.String()), lifecycle.ReviewMessageUpdate{
		WorkItemID: wid.String(), MessageID: msg.ID, Metadata: meta,
	})
	switch {
	case errors.Is(err, a2a.ErrUnsupportedOperation):
		i.deleteItem(ctx, wid, row.ID)
		return nil, unsupported("the task is completed and no review window is open")
	case err != nil:
		// Not accepted (or not completed) within 10 s: the item is kept, the workflow may still take it (D66).
		return nil, notInTime("the review message was not accepted in time", err)
	case !res.Accepted:
		i.deleteItem(ctx, wid, row.ID)
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
		if p != nil && p.Config != nil {
			pc = p.Config.PushConfig
		}
	case *a2a.PushConfig:
		pc = p
	}
	if pc == nil {
		return ctx, nil, nil
	}
	if err := CheckPushURL(ctx, pc.URL, i.PushPolicy); err != nil {
		return ctx, nil, err
	}
	return ctx, nil, nil
}

// CheckPushURL is the set-time check of a push config URL (spec §10.1, §10.3): InvalidParams when the URL
// does not parse or the SSRF rules refuse it. It never connects.
func CheckPushURL(ctx context.Context, raw string, p ssrf.Policy) error {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return invalid("push notification URL is not a URL")
	}
	if err := ssrf.CheckURL(ctx, u, p); err != nil {
		return invalid("push notification URL refused: " + err.Error())
	}
	return nil
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
	// Canceled (the terminal event is in the log, D79), or past 60 s: the stored task as it is.
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
