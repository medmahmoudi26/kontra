package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/store"
)

// ResendWindow is how young a first task must be for a resend to be answered with it (spec §15).
const ResendWindow = 24 * time.Hour

// MetaApproveURL is the A2A metadata key of a quoted task's approve URL (spec §5.1, §13.2).
const MetaApproveURL = "kontra.approve.url"

// InlinePushSaver saves a request's inline push config (configuration.taskPushNotificationConfig) for the
// task inside the work queue's transaction, before the work item is written (spec §10.1), with Save's
// rules (idempotent per task, URL and token; the per-task and per-buyer limits). configID is the config to
// announce, empty when the save needs no push_webhook started (an update of a config that is delivering):
// Write sends kontra.push for it once a follow-up's kontra.message is accepted (a new row, or a failed
// config set again); a new task's workflow lists its configs at start. inserted reports a new row:
// DeleteInline removes it when the lifecycle then refuses the follow-up, so a refused request leaves no new
// config on the task (spec §7, §10.1). No kontra.push was sent for it; a push_webhook a listing started for
// it meanwhile ends when its config is deleted (§10.2). internal/push implements it.
type InlinePushSaver interface {
	SaveInline(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, owner store.Owner, cfg *a2a.PushConfig) (configID string, inserted bool, err error)
	DeleteInline(ctx context.Context, taskID uuid.UUID, configID string) error
}

// QueueOptions are what a service's work queue needs besides the log and the client.
type QueueOptions struct {
	// Owner is the caller, as the identity interceptor set it (relay.CallerOwner). Required.
	Owner store.OwnerFunc
	// Push saves inline push configs in Write's transaction; nil leaves an inline config in the work item,
	// and a2a-go's handler saves it through the handler's push config store when the turn is dispatched.
	Push InlinePushSaver
	// Hooks is the hook configuration a new task's workflow starts with (spec §13.3, D46).
	Hooks HooksSnapshot
	// Now is the clock (tests); time.Now when nil.
	Now func() time.Time
}

// Queue is the Temporal-backed a2a-go workqueue.Queue for one service (spec §8.1, §15, D6). Its Write
// is the only Temporal call on the request path; the HandlerFn a2a-go registers is what the dispatch
// activity calls, with a Heartbeater attached. The handler's concurrency limiter stays off: the Temporal
// worker's activity slots bound concurrency, so RegisterHandler keeps a2a-go's function as it is.
type Queue struct {
	svc     store.ServiceRef
	log     *store.Log
	client  *Client
	opts    QueueOptions
	handler workqueue.HandlerFn
}

var _ workqueue.Queue = (*Queue)(nil)

// NewQueue returns the queue for svc.
func NewQueue(svc store.ServiceRef, log *store.Log, c *Client, opts QueueOptions) *Queue {
	return &Queue{svc: svc, log: log, client: c, opts: opts}
}

func (q *Queue) now() time.Time {
	if q.opts.Now != nil {
		return q.opts.Now()
	}
	return time.Now()
}

// Write never puts a2a-go's payload into Temporal: the payload carries the whole request and every
// inbound header, Authorization and cookies included (D6). Only the work item's id crosses into Temporal.
//
// A new task needs the admission interceptor's decision on ctx (hooks.AdmittedFrom: allow or quote, and the
// pre-minted id); without one Write creates nothing and answers -32603 "admission unavailable" (spec
// §13.1: admission fails closed). A resend (same principal, service, messageId and request hash, the first
// task younger than ResendWindow) is answered with the existing task, re-issuing StartWithMessage with its
// first work item id (idempotent; a workflow that has ended refuses it, which is swallowed, spec §14.5).
// Otherwise, in one transaction, which first looks for a resend again under the resend key's lock (two
// identical requests arriving together make one task), the gate task is pre-created (the id Admission was
// shown, gate ids, submitted, or quoted on a quote decision, the caller's message in history), the inline
// push config is saved, and the sanitized payload is stored as a work item; after the commit,
// StartWithMessage. payload.TaskID is set to the task's id, which Write returns.
//
// A follow-up: the work item is stored (and the task's version V, and the turn end it answers, read in the
// same transaction) and kontra.message sent, then kontra.push for an inline config it inserted. A refusal
// deletes the work item and that config and is a2a.ErrUnsupportedOperation; an Update not accepted within
// 10 s keeps both and wraps ErrUnavailable (-32603, HTTP 503; spec §7, D66).
//
// A cancel payload is never written: CancelTask is decided by the cancel interceptor before a2a-go's
// executor sees it (D4).
//
// When ctx carries a StreamStart (WithStreamStart), Write fills it for the gate's SSE writer when it
// returns the task, and only then: a new task streams from seq 0 (the creation row is the first event); a
// follow-up or a resend from V. A refused Update, or one not accepted in time, leaves it empty, so a
// filled StreamStart means the turn was accepted whatever a2a-go reports after Write (spec §9).
func (q *Queue) Write(ctx context.Context, p *workqueue.Payload) (a2a.TaskID, error) {
	if p == nil {
		return "", fmt.Errorf("lifecycle: no payload: %w", a2a.ErrInvalidParams)
	}
	if p.Type == workqueue.PayloadTypeCancel {
		return "", a2a.NewError(a2a.ErrUnsupportedOperation, "CancelTask is decided by the gate's cancel interceptor")
	}
	if p.Type != workqueue.PayloadTypeExecute || p.ExecuteRequest == nil || p.ExecuteRequest.Message == nil {
		return "", fmt.Errorf("lifecycle: an execute payload needs its message: %w", a2a.ErrInvalidParams)
	}
	if q.opts.Owner == nil {
		return "", errors.New("lifecycle: the work queue has no owner function")
	}
	owner, err := q.opts.Owner(ctx)
	if err != nil {
		return "", err
	}
	if p.ExecuteRequest.Message.TaskID == "" {
		return q.writeNew(ctx, p, owner)
	}
	return q.writeFollowUp(ctx, p, owner)
}

func (q *Queue) input(taskID uuid.UUID, quoteExpires *time.Time) Input {
	return Input{
		TaskID: taskID.String(), TimeoutSeconds: int64(q.svc.Timeout / time.Second), Retries: q.svc.Retries,
		Hooks: q.opts.Hooks, QuoteExpiresAt: quoteExpires,
	}
}

func (q *Queue) writeNew(ctx context.Context, p *workqueue.Payload, owner store.Owner) (a2a.TaskID, error) {
	req := p.ExecuteRequest
	// Admission runs before any task exists and fails closed (spec §13.1): a request that reached Write
	// without the admission interceptor's decision creates nothing.
	adm, ok := hooks.AdmittedFrom(ctx)
	if !ok || adm.TaskID == "" || (adm.Decision.Outcome != hooks.Allow && adm.Decision.Outcome != hooks.Quote) {
		return "", a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	hash, err := store.RequestHash(req)
	if err != nil {
		return "", err
	}
	since := q.now().Add(-ResendWindow)
	prev, err := q.log.FindResend(ctx, q.svc, owner, req.Message.ID, hash, since)
	if err != nil {
		return "", err
	}
	if prev != nil {
		return q.resend(ctx, p, prev)
	}

	taskID, err := ids.ParseTaskID(adm.TaskID)
	if err != nil {
		return "", a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	contextID := req.Message.ContextID
	if contextID == "" {
		if contextID, err = ids.NewContextID(); err != nil {
			return "", err
		}
	}
	state, quoteExpires, meta, err := admitted(adm.Decision)
	if err != nil {
		return "", err
	}
	msg := *req.Message
	msg.TaskID, msg.ContextID = a2a.TaskID(taskID.String()), contextID
	now := q.now().UTC().Truncate(time.Millisecond)
	snap := &a2a.Task{
		ID: a2a.TaskID(taskID.String()), ContextID: contextID,
		Status:  a2a.TaskStatus{State: state.A2A(), Timestamp: &now},
		History: []*a2a.Message{&msg}, Metadata: meta,
	}
	wid, err := ids.NewWorkItemID()
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, q.log.DB(), func(tx pgx.Tx) error {
		// The lookup above takes no lock: an identical request arriving at the same moment missed it too.
		// Under the resend key's lock the later one finds the earlier one's task once it committed.
		found, err := q.log.FindResendLocked(ctx, tx, q.svc, owner, req.Message.ID, hash, since)
		if err != nil {
			return err
		}
		if found != nil {
			prev = found
			return errResent
		}
		if _, err := q.log.CreateTask(ctx, tx, store.NewTask{
			ID: taskID, Service: q.svc, Protocol: store.ProtocolA2A, Owner: owner, ContextID: contextID, State: state,
			Snapshot: snap, FirstMessageID: req.Message.ID, FirstRequest: hash, QuoteExpiresAt: quoteExpires,
		}); err != nil {
			return err
		}
		// A new task's workflow lists its push configs at start: no kontra.push is needed.
		stored, _, _, err := q.savePush(ctx, tx, taskID, owner, req)
		if err != nil {
			return err
		}
		wp, err := Sanitize(&workqueue.Payload{Type: p.Type, TaskID: p.TaskID, ExecuteRequest: stored, CallContext: p.CallContext}, owner)
		if err != nil {
			return err
		}
		return q.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: taskID, Kind: store.WorkExecute, Payload: wp})
	})
	if errors.Is(err, errResent) {
		return q.resend(ctx, p, prev)
	}
	if err != nil {
		return "", err
	}
	// The task row exists: its workflow is started even if the caller went away meanwhile (D8), within
	// AcceptWait.
	if _, err := q.client.StartWithMessage(context.WithoutCancel(ctx), q.input(taskID, quoteExpires), wid.String()); err != nil {
		// The task row stays without a workflow: a resend re-issues the start, and the orphan sweep ends it
		// lifecycle_unavailable after 2 minutes (spec §14.5, D63).
		return "", err
	}
	fillStreamStart(ctx, StreamStart{TaskID: a2a.TaskID(taskID.String()), Version: 0, New: true})
	p.TaskID = a2a.TaskID(taskID.String())
	return p.TaskID, nil
}

// errResent rolls back a new task's transaction that found, under the resend key's lock, the task an
// identical request created meanwhile.
var errResent = errors.New("lifecycle: an identical request created the task meanwhile")

// admitted is what Admission's decision makes of a new task: submitted with the decision's metadata, or
// quoted with it and the approve URL (spec §13.2). The decision's mcpMeta is rendered for MCP tasks (M3);
// an A2A task does not keep it.
func admitted(d hooks.Decision) (store.State, *time.Time, map[string]any, error) {
	var meta map[string]any
	if len(d.Metadata) > 0 {
		meta = make(map[string]any, len(d.Metadata)+1)
		for k, v := range d.Metadata {
			meta[k] = v
		}
	}
	if d.Outcome != hooks.Quote {
		return store.StateSubmitted, nil, meta, nil
	}
	if d.Quote == nil {
		return "", nil, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	exp, err := time.Parse(time.RFC3339, d.Quote.ExpiresAt)
	if err != nil {
		return "", nil, nil, a2a.NewError(a2a.ErrInternalError, "admission unavailable")
	}
	exp = exp.UTC()
	if meta == nil {
		meta = map[string]any{}
	}
	meta[MetaApproveURL] = d.Quote.ApproveURL
	return store.StateQuoted, &exp, meta, nil
}

// resend answers a resend with the task it repeats and re-issues its workflow's start with the first work
// item id: a task whose start was lost gets its workflow (spec §14.5). A refusal (the work item was
// accepted already) and a closed workflow are what an idempotent re-issue meets, and are not errors.
func (q *Queue) resend(ctx context.Context, p *workqueue.Payload, prev *store.TaskRow) (a2a.TaskID, error) {
	id := a2a.TaskID(prev.ID.String())
	wid, err := q.log.FirstWorkItemID(ctx, prev.ID)
	switch {
	case errors.Is(err, store.ErrWorkItemNotFound):
	case err != nil:
		return "", err
	default:
		var quoteExpires *time.Time
		if prev.SubmittedAt == nil {
			quoteExpires = prev.QuoteExpiresAt // the task was created quoted and not yet approved
		}
		_, err := q.client.StartWithMessage(ctx, q.input(prev.ID, quoteExpires), wid.String())
		if err != nil && !errors.Is(err, ErrRefused) {
			return "", err
		}
	}
	fillStreamStart(ctx, StreamStart{TaskID: id, Version: prev.Version, New: false})
	p.TaskID = id
	return id, nil
}

func (q *Queue) writeFollowUp(ctx context.Context, p *workqueue.Payload, owner store.Owner) (a2a.TaskID, error) {
	req := p.ExecuteRequest
	taskID, err := ids.ParseTaskID(string(req.Message.TaskID))
	if err != nil {
		return "", err
	}
	wid, err := ids.NewWorkItemID()
	if err != nil {
		return "", err
	}
	var (
		pos      position
		configID string // the inline config to announce
		inserted bool   // its row is new: deleted if the follow-up is refused
	)
	err = pgx.BeginFunc(ctx, q.log.DB(), func(tx pgx.Tx) error {
		stored, announce, ins, err := q.savePush(ctx, tx, taskID, owner, req)
		if err != nil {
			return err
		}
		configID, inserted = announce, ins
		wp, err := Sanitize(&workqueue.Payload{Type: p.Type, TaskID: p.TaskID, ExecuteRequest: stored, CallContext: p.CallContext}, owner)
		if err != nil {
			return err
		}
		if err := q.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: taskID, Kind: store.WorkExecute, Payload: wp}); err != nil {
			return err
		}
		pos, err = positionOf(ctx, tx, taskID)
		return err
	})
	if err != nil {
		return "", err
	}
	tid := a2a.TaskID(taskID.String())
	// The work item exists: the Update is sent even if the caller went away meanwhile, within AcceptWait.
	bg := context.WithoutCancel(ctx)
	if err := q.message(bg, taskID, wid, pos); err != nil {
		if errors.Is(err, ErrRefused) || errors.Is(err, errBehind) {
			// A refused follow-up's content is not kept (spec §7, §14.1), nor its inline push config (§10.1):
			// no Update of it is pending, so nothing can consume them. Deleting is best effort: the caller's
			// answer is the refusal either way, and retention deletes a leftover with the task.
			_ = q.log.DeleteWorkItem(bg, wid, taskID)
			if inserted && configID != "" {
				_ = q.opts.Push.DeleteInline(bg, taskID, configID)
			}
		}
		return "", err
	}
	// V was read in the work item's transaction; the stream starts there once the Update is accepted.
	fillStreamStart(ctx, StreamStart{TaskID: tid, Version: pos.version, New: false})
	if configID != "" {
		// kontra.push is best effort: the workflow lists push configs after each dispatch (spec §10.2).
		_ = q.client.SignalPush(bg, tid, PushSignal{ConfigID: configID})
	}
	return req.Message.TaskID, nil
}

// position is where a follow-up's work item was stored in the task's log: the task's version V (where its
// SSE stream starts, spec §9), and the task's last event other than a metadata row, the turn end the
// follow-up answers when the task was waiting for input there.
type position struct {
	version int64
	turnEnd int64 // the seq of the last non-metadata event
	waiting bool  // the task is input-required and that event is the status (or task) row that said so
}

// positionOf reads a task's position in q (the follow-up's transaction, or the pool).
func positionOf(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, taskID uuid.UUID) (position, error) {
	var (
		p     position
		state string
		kind  string
	)
	err := q.QueryRow(ctx, `select t.version, t.state, coalesce(e.seq, 0), coalesce(e.kind, '')
		from kontra.tasks t
		left join lateral (select seq, kind from kontra.task_events
			where task_id = t.id and kind <> 'metadata' order by seq desc limit 1) e on true
		where t.id = $1`, taskID).Scan(&p.version, &state, &p.turnEnd, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return position{}, a2a.ErrTaskNotFound
	}
	if err != nil {
		return position{}, fmt.Errorf("lifecycle: read the task's position: %w", err)
	}
	p.waiting = store.State(state) == store.StateInputRequired && (kind == string(store.KindStatus) || kind == string(store.KindTask))
	return p, nil
}

// errBehind marks a follow-up the workflow refused because it had not processed the turn end the caller
// answers, until AcceptWait ran out.
var errBehind = errors.New("lifecycle: the task's lifecycle has not caught up with its log")

// message sends kontra.message for a follow-up whose work item was stored at pos, within AcceptWait in all.
//
// SPEC-CONFLICT: spec §7 and §14.1 refuse a follow-up "while the previous turn of the same task is still
// being dispatched", by a validator that reads workflow state only (D66). But the caller learns that a
// turn ended from the log: the dispatch logs input-required, a2a-go answers the caller at once, and the
// dispatch activity returns to the workflow a moment later. A caller that answers at once meets a workflow
// that still counts the turn as running, and read literally it would be refused although the turn has
// ended. The closest faithful behaviour: the Update carries the turn end the caller answers
// (MessageUpdate.After), and the validator tells a final refusal (another follow-up of that turn end was
// accepted first, or the caller read no later turn end than the workflow processed: ErrTypeRefused, at once)
// from one by a workflow that has not processed that turn end yet (ErrTypeBehind). Only the latter is sent
// again, backing off, and only while the task was waiting for input when the work item was stored and its
// log still ends at that turn end (metadata rows aside: an operator patch the workflow applies once the
// dispatch returns, spec §13.3, changes no turn). If the workflow has not caught up within AcceptWait, the
// follow-up is not accepted in time (-32603 / HTTP 503, ErrUnavailable) and, since no Update of it is
// pending, its work item is deleted.
func (q *Queue) message(ctx context.Context, taskID uuid.UUID, wid uuid.UUID, pos position) error {
	ctx, cancel := context.WithTimeout(ctx, AcceptWait)
	defer cancel()
	wait := 25 * time.Millisecond
	for {
		_, err := q.client.Message(ctx, a2a.TaskID(taskID.String()), MessageUpdate{WorkItemID: wid.String(), After: pos.turnEnd})
		if err == nil || !errors.Is(err, errBehind) || !pos.waiting {
			return err
		}
		now, lerr := positionOf(ctx, q.log.DB(), taskID)
		if lerr != nil || !now.waiting || now.turnEnd != pos.turnEnd {
			return err // the task moved on: another turn wrote, or it ended
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", unavailable(err), errBehind)
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// savePush saves the request's inline push config in tx when a saver is configured, and returns the
// request to store in the work item, without the config once it is saved, so the caller's webhook
// credentials live only in push_configs.config; the config's id when it is to be announced (InlinePushSaver);
// and whether its row was inserted. Without a saver the request is returned as it is.
func (q *Queue) savePush(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, owner store.Owner, req *a2a.SendMessageRequest) (*a2a.SendMessageRequest, string, bool, error) {
	if q.opts.Push == nil || req.Config == nil || req.Config.PushConfig == nil {
		return req, "", false, nil
	}
	id, inserted, err := q.opts.Push.SaveInline(ctx, tx, taskID, owner, req.Config.PushConfig)
	if err != nil {
		return nil, "", false, err
	}
	cp := *req
	cfg := *req.Config
	cfg.PushConfig = nil
	cp.Config = &cfg
	return &cp, id, inserted, nil
}

// RegisterHandler implements workqueue.Queue; a2a-go calls it once per handler. The function is kept as
// it is: its concurrency limiter stays off (the Temporal worker's activity slots bound concurrency).
func (q *Queue) RegisterHandler(_ workqueue.HandlerConfig, fn workqueue.HandlerFn) {
	q.handler = fn
}

// Handler is the HandlerFn the dispatch activity runs for this service.
func (q *Queue) Handler() workqueue.HandlerFn {
	return q.handler
}

// kept are the service parameters a work item keeps (spec §8.1, D6); every other header — Authorization,
// Cookie, everything else the request carried — is dropped.
var kept = []string{a2a.SvcParamVersion, a2a.SvcParamExtensions}

// Sanitize reduces a2a-go's payload to what work_items may hold: the request and the call context with
// every service parameter removed except A2A-Version and A2A-Extensions, plus the owner. Of the encoded
// call context (a2asrv's codec) it keeps the tenant, the user (the identity the dispatching replica's
// store scopes by), the protocol version and those two parameters; custom codec attributes are dropped.
func Sanitize(p *workqueue.Payload, o store.Owner) (store.WorkPayload, error) {
	if p == nil || p.ExecuteRequest == nil || p.ExecuteRequest.Message == nil {
		return store.WorkPayload{}, fmt.Errorf("lifecycle: a work item needs the request and its message: %w", a2a.ErrInvalidParams)
	}
	cc, err := sanitizeCallContext(p.CallContext)
	if err != nil {
		return store.WorkPayload{}, err
	}
	return store.WorkPayload{Request: p.ExecuteRequest, CallContext: cc, Owner: o}, nil
}

// encodedCallContext mirrors a2asrv's encoded call context (ctxcodec.go) without its attributes.
type encodedCallContext struct {
	CallContext *struct {
		Tenant          string              `json:"tenant,omitempty"`
		SvcParams       map[string][]string `json:"svcParams,omitempty"`
		User            json.RawMessage     `json:"user,omitempty"`
		ProtocolVersion string              `json:"protocolVersion,omitempty"`
	} `json:"callContext,omitempty"`
}

func sanitizeCallContext(cc map[string]any) (map[string]any, error) {
	if cc == nil {
		return nil, nil
	}
	raw, err := json.Marshal(cc)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: encode call context: %w", err)
	}
	var enc encodedCallContext
	if err := json.Unmarshal(raw, &enc); err != nil {
		return nil, fmt.Errorf("lifecycle: decode call context: %w", err)
	}
	if c := enc.CallContext; c != nil {
		params := map[string][]string{}
		for k, v := range c.SvcParams {
			for _, keep := range kept {
				if strings.EqualFold(k, keep) {
					params[strings.ToLower(keep)] = append(params[strings.ToLower(keep)], v...)
				}
			}
		}
		c.SvcParams = params
		if len(params) == 0 {
			c.SvcParams = nil
		}
	}
	raw, err = json.Marshal(enc)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: encode call context: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lifecycle: decode call context: %w", err)
	}
	return out, nil
}

// Rebuild turns a stored work item back into the payload a2a-go's HandlerFn expects: an execute payload
// for taskID with the stored request and call context. The call context is sanitized again, so nothing but
// A2A-Version and A2A-Extensions ever reaches the handler whatever the row holds.
func Rebuild(taskID a2a.TaskID, wi *store.WorkItem) (*workqueue.Payload, error) {
	if wi == nil {
		return nil, errors.New("lifecycle: no work item")
	}
	if wi.Kind != store.WorkExecute {
		return nil, fmt.Errorf("lifecycle: work item %s is a %s item: %w", wi.ID, wi.Kind, workqueue.ErrMalformedPayload)
	}
	if wi.Payload.Request == nil || wi.Payload.Request.Message == nil {
		return nil, fmt.Errorf("lifecycle: work item %s holds no message: %w", wi.ID, workqueue.ErrMalformedPayload)
	}
	if mt := wi.Payload.Request.Message.TaskID; mt != "" && mt != taskID {
		return nil, fmt.Errorf("lifecycle: work item %s names task %s, not %s: %w", wi.ID, mt, taskID, workqueue.ErrMalformedPayload)
	}
	cc, err := sanitizeCallContext(wi.Payload.CallContext)
	if err != nil {
		return nil, err
	}
	return &workqueue.Payload{Type: workqueue.PayloadTypeExecute, TaskID: taskID, ExecuteRequest: wi.Payload.Request, CallContext: cc}, nil
}

// StreamStart is filled by Write for the gate's SSE writer, which attaches it to the request context
// before asking a2a-go to create or continue the task: the SSE stream starts after Version (spec §9). For
// a new task Version is 0 (the creation row, seq 1, is the first event) and New is true; for a follow-up it
// is the task's version read in the transaction that stored the work item, before the kontra.message
// Update, so the snapshot at V leads without an id; for a resend it is the existing task's version. Write
// fills it only when it returns the task (the Update accepted): an empty StreamStart after a2a-go's call
// means no turn was accepted.
type StreamStart struct {
	TaskID  a2a.TaskID
	Version int64
	New     bool
}

type streamStartKey struct{}

// WithStreamStart returns ctx carrying s for Write to fill.
func WithStreamStart(ctx context.Context, s *StreamStart) context.Context {
	return context.WithValue(ctx, streamStartKey{}, s)
}

func fillStreamStart(ctx context.Context, s StreamStart) {
	if p, ok := ctx.Value(streamStartKey{}).(*StreamStart); ok && p != nil {
		*p = s
	}
}

// Heartbeater attaches to the dispatch context (workqueue.AttachHeartbeater): every 5 s it records an
// activity heartbeat and returns the context's error, so an abort, or a cancellation Temporal delivered in
// a heartbeat response, stops the execution.
type Heartbeater struct{}

var _ workqueue.Heartbeater = Heartbeater{}

// HeartbeatInterval implements workqueue.Heartbeater (spec §6.2: every 5 s, timeout 30 s).
func (Heartbeater) HeartbeatInterval() time.Duration { return 5 * time.Second }

// Heartbeat implements workqueue.Heartbeater. Outside an activity (a context Temporal did not create) it
// records nothing. The heartbeat carries no details: the retry decision reads Postgres (spec §14.2).
func (Heartbeater) Heartbeat(ctx context.Context) error {
	if activity.IsActivity(ctx) {
		activity.RecordHeartbeat(ctx)
	}
	return ctx.Err()
}
