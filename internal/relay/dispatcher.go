package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// Dispatcher implements lifecycle.DispatchActivities on this replica.
type Dispatcher struct {
	ReplicaID string
	Log       *store.Log
	Registry  *Registry
	Handlers  *Handlers
	Backends  Backends
	// Signals sends kontra.dispatched; the executor of each service handler holds it (HandlerDeps.Signals).
	Signals interface {
		SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error
	}
	// Rebuild turns a work item back into a2a-go's payload; lifecycle.Rebuild when nil.
	Rebuild func(taskID a2a.TaskID, wi *store.WorkItem) (*workqueue.Payload, error)
	// Heartbeater is attached to the dispatch context (every 5 s); lifecycle.Heartbeater when nil.
	Heartbeater workqueue.Heartbeater
	Logger      *slog.Logger
}

var _ lifecycle.DispatchActivities = (*Dispatcher)(nil)

func (d *Dispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Dispatch implements lifecycle.DispatchActivities. Before any upstream call it writes attempts.replica_id
// and a fresh dispatch_id (the fence every append of this dispatch carries, D77); it loads the work item
// by (id, task), consuming it on the turn's first dispatch (D66), rebuilds a2a-go's payload, registers the
// dispatch, attaches the Heartbeater and calls the service's HandlerFn with the activity context. It
// discards the handler's result and returns the registry's outcome. With Resume, the executor re-attaches
// to the recorded upstream task: GetTask, log the difference, then SubscribeToTask (or polling) until
// terminal or input-required; only GetTask failing to reach the service for 30 s fails the task
// upstream_unreachable (spec §6.4, D42).
//
// The dispatch announces itself in the registry before it writes attempts.replica_id, which routes abort
// here (§14.2): an abort that arrives before it registers is kept, and the dispatch then stops before it
// calls the upstream, returns aborted and ends the attempt aborted (§8).
//
// It ends the attempt (attempts.ended_at and outcome) when the dispatch ends it: a terminal event, a
// retryable failure, an abort. A turn that stops at input-required, and a lost context (re-attached by a
// later dispatch of the same attempt), leave it open (D77).
func (d *Dispatcher) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	taskUUID, err := ids.ParseTaskID(in.TaskID)
	if err != nil {
		return lifecycle.DispatchResult{}, fmt.Errorf("relay: dispatch of task %q: %w", in.TaskID, err)
	}
	log := d.logger().With(obs.Task(in.TaskID, in.N)...)
	row, err := d.Log.Load(ctx, taskUUID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if row.State.Terminal() {
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, nil
	}
	svc, err := d.Log.LoadService(ctx, row.TenantID, row.Service)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	key := Key{TaskID: a2a.TaskID(taskUUID.String()), N: in.N}
	release := d.Registry.Expect(key)
	defer release()
	fence, err := d.Log.BeginDispatch(ctx, taskUUID, in.N, d.ReplicaID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	wi, err := d.workItem(ctx, taskUUID, in)
	if errors.Is(err, store.ErrWorkItemNotFound) {
		log.Error("the turn's work item is missing or another task's", "work_item", in.WorkItemID)
		return d.failTask(ctx, fence, taskUUID, store.ReasonLifecycleUnavailable, "the task's request could not be found")
	}
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	rebuild := d.Rebuild
	if rebuild == nil {
		rebuild = lifecycle.Rebuild
	}
	payload, err := rebuild(a2a.TaskID(taskUUID.String()), wi)
	if err != nil {
		return lifecycle.DispatchResult{}, fmt.Errorf("relay: rebuild work item %s: %w", wi.ID, err)
	}
	sh, err := d.Handlers.For(ctx, svc)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	dctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	running, err := d.Registry.Register(key, cancel)
	release()
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if running.Aborted() {
		// An abort reached this replica while the dispatch prepared: nothing was sent upstream.
		running.Done()
		log.Info("dispatch aborted before it called the upstream")
		d.endAttempt(ctx, log, taskUUID, in.N, store.OutcomeAborted)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchAborted}, nil
	}
	info := &dispatchInfo{n: in.N, resume: in.Resume, fence: fence, running: running, task: row, svc: svc, request: wi.Payload.Request}
	dctx = withDispatch(store.WithFence(dctx, fence), info)
	hb := d.Heartbeater
	if hb == nil {
		hb = lifecycle.Heartbeater{}
	}
	dctx = workqueue.AttachHeartbeater(dctx, hb)
	_, herr := sh.HandlerFn(dctx, payload)
	cause := context.Cause(dctx)
	res := running.Done()
	return d.settle(ctx, log, in, fence, running, res, herr, cause)
}

// workItem loads the turn's work item: the turn's first dispatch consumes it; a later dispatch of the same
// turn (a retry on attempt n > 1, a resume) reloads the consumed item it owns (D66).
func (d *Dispatcher) workItem(ctx context.Context, taskID uuid.UUID, in lifecycle.DispatchInput) (*store.WorkItem, error) {
	wid, err := uuid.Parse(in.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("work item id %q: %w", in.WorkItemID, store.ErrWorkItemNotFound)
	}
	if !in.Resume {
		err := d.Log.ConsumeWorkItem(ctx, wid, taskID)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrWorkItemConsumed) && in.N > 1:
			// A retry of the turn on a new attempt: the item was consumed by the attempt before.
		case errors.Is(err, store.ErrWorkItemConsumed):
			// The first attempt's first dispatch of a turn found its item consumed: some other dispatch
			// already sent it. Sending it again could repeat the work upstream; refuse.
			return nil, fmt.Errorf("relay: work item %s was already consumed by another dispatch of attempt %d", wid, in.N)
		default:
			return nil, err
		}
	}
	return d.Log.LoadWorkItem(ctx, wid, taskID)
}

// settle turns how the handler returned into the dispatch's result, and ends the attempt when the dispatch
// ended it.
func (d *Dispatcher) settle(ctx context.Context, log *slog.Logger, in lifecycle.DispatchInput, fence *store.Fence,
	running *Running, res lifecycle.DispatchResult, herr, cause error) (lifecycle.DispatchResult, error) {
	taskUUID := fence.TaskID
	if running.Superseded() || fence.Fenced() || errors.Is(cause, ErrSuperseded) {
		// A newer dispatch holds the attempt (D77): this one's result is no longer the workflow's.
		log.Info("dispatch superseded by a newer dispatch of the task")
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	}
	recorded, _ := running.Outcome()
	aborted := running.Aborted() || errors.Is(cause, ErrAborted)
	switch {
	case recorded == lifecycle.DispatchTerminal || recorded == lifecycle.DispatchInterrupted:
		row, err := d.Log.Load(ctx, taskUUID)
		if err != nil {
			return lifecycle.DispatchResult{}, err
		}
		if recorded == lifecycle.DispatchTerminal && !row.State.Terminal() ||
			recorded == lifecycle.DispatchInterrupted && row.State != store.StateInputRequired && !row.State.Terminal() {
			// The event never reached the log (the write failed): the attempt is followed again.
			log.Warn("the dispatch's final event is not in the log", "state", row.State, "error", herr)
			return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
		}
		if row.State.Terminal() {
			res.Outcome = lifecycle.DispatchTerminal
			d.endAttempt(ctx, log, taskUUID, in.N, attemptOutcome(row.State, store.FailureReason(res.Reason)))
		}
		return res, nil
	case recorded == lifecycle.DispatchRetryable:
		d.endAttempt(ctx, log, taskUUID, in.N, store.Outcome(res.Reason))
		return res, nil
	case aborted:
		// abort() stopped the dispatch (§8). A lost context the executor recorded on its way out (a read of
		// the log on the context the abort canceled) is the abort's doing.
		d.endAttempt(ctx, log, taskUUID, in.N, store.OutcomeAborted)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchAborted}, nil
	case recorded != "":
		return res, nil
	}
	if serr := running.StoreFailure(); serr != nil {
		// The gate's store failed while the dispatch logged an event (a write that ran out of its time or
		// lost its connection; a2a-go's processor then stopped the execution). That is not an upstream
		// error (§6.4): the upstream task is untouched, and a write that did not commit is a lost context,
		// which a later dispatch of the same attempt re-attaches, logging the difference (D42).
		log.Warn("the task's log could not be written; the attempt is followed again", "error", serr)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	}
	if herr == nil || ctx.Err() != nil {
		// The activity's context ended (Temporal canceled it, the worker stops): a lost context, which a
		// later dispatch of the same attempt re-attaches (direct, D42) or the workflow retries.
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	}
	// An error of a2a-go's handler or the executor that is not a lost context — a2a-go refusing a
	// parallel active execution, a validation failure, a task already terminal — is upstream_error,
	// never context_lost (§6.4, D42): re-attaching would only meet it again. (A failure of the gate's
	// store is a lost context, above.)
	row, err := d.Log.Load(ctx, taskUUID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if row.State.Terminal() {
		d.endAttempt(ctx, log, taskUUID, in.N, attemptOutcome(row.State, ""))
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, nil
	}
	log.Warn("the relay failed without a lost context", "error", herr)
	return d.failTask(ctx, fence, taskUUID, store.ReasonUpstreamError, "the relay could not run the task")
}

// failTask logs `failed: <reason>` under the dispatch's fence, ends the attempt, and returns terminal.
func (d *Dispatcher) failTask(ctx context.Context, fence *store.Fence, taskID uuid.UUID, reason store.FailureReason, text string) (lifecycle.DispatchResult, error) {
	if err := d.writeFailed(ctx, fence, taskID, reason, text); err != nil {
		if errors.Is(err, store.ErrFenced) {
			return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
		}
		return lifecycle.DispatchResult{}, err
	}
	d.endAttempt(ctx, d.logger(), taskID, fence.N, attemptOutcome(store.StateFailed, reason))
	return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal, Reason: string(reason)}, nil
}

// writeFailed appends the failed status at the task's current version, fenced; a task already terminal is
// left as it is.
func (d *Dispatcher) writeFailed(ctx context.Context, fence *store.Fence, taskID uuid.UUID, reason store.FailureReason, text string) error {
	var last error
	for range 8 {
		row, err := d.Log.Load(ctx, taskID)
		if err != nil {
			return err
		}
		if row.State.Terminal() {
			return nil
		}
		ev := store.FailedStatus(row.Snapshot, reason, text)
		next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
		if err != nil {
			return err
		}
		_, err = d.Log.Append(ctx, nil, store.Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: store.KindStatus, State: store.StateFailed,
			StatusAt: store.StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload, Fence: fence,
		})
		if err == nil || errors.Is(err, store.ErrFenced) || !errors.Is(err, taskstore.ErrConcurrentModification) {
			return err
		}
		last = err
	}
	return last
}

func (d *Dispatcher) endAttempt(ctx context.Context, log *slog.Logger, taskID uuid.UUID, n int, o store.Outcome) {
	if !o.Valid() {
		o = store.OutcomeFailed
	}
	if err := d.Log.EndAttempt(ctx, taskID, n, o); err != nil {
		log.Warn("end attempt", "outcome", o, "error", err)
	}
}

// attemptOutcome is attempts.outcome for a task that ended in state with a failure reason.
func attemptOutcome(state store.State, reason store.FailureReason) store.Outcome {
	switch state {
	case store.StateCompleted:
		return store.OutcomeCompleted
	case store.StateCanceled:
		return store.OutcomeCanceled
	case store.StateRejected:
		return store.OutcomeRejected
	}
	if o := store.Outcome(reason); reason != "" && o.Valid() {
		return o
	}
	return store.OutcomeFailed
}

// Abort implements lifecycle.DispatchActivities: it cancels the running dispatch of (task, n) through the
// replica's registry, or marks one that recorded this replica but has not registered yet so that it stops
// before calling the upstream (spec §8, D44). Nothing running or starting here is not an error: the
// dispatch already ended.
func (d *Dispatcher) Abort(ctx context.Context, in lifecycle.AttemptRef) error {
	if !d.Registry.Abort(Key{TaskID: a2a.TaskID(in.TaskID), N: in.N}) {
		d.logger().Info("abort: no dispatch of the attempt runs on this replica", obs.Task(in.TaskID, in.N)...)
	}
	return nil
}

// ForwardCancel implements lifecycle.DispatchActivities: CancelTask upstream on the attempt's upstream
// task, returning the upstream's answer (a canceled status is logged by the running dispatch, or by the
// workflow through write_terminal for an input-required task). The upstream's error text crosses Temporal
// history only as a capped message (upstream.MaxMessage).
func (d *Dispatcher) ForwardCancel(ctx context.Context, in lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	taskUUID, err := ids.ParseTaskID(in.TaskID)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	attempt, err := d.Log.GetAttempt(ctx, taskUUID, in.N)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	if attempt.UpstreamTaskID == "" {
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("relay: attempt %d of task %s has no upstream task to cancel", in.N, in.TaskID)
	}
	row, err := d.Log.Load(ctx, taskUUID)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	svc, err := d.Log.LoadService(ctx, row.TenantID, row.Service)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	backend, info, err := d.Backends.Backend(ctx, svc)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	client, err := upstream.NewClient(ctx, backend, info, nil)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	defer client.Destroy()
	cctx, ex := upstream.WithExchange(ctx)
	t, err := client.CancelTask(cctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(attempt.UpstreamTaskID)})
	if err == nil {
		res := lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelCanceled}
		if t != nil {
			res.UpstreamState = string(t.Status.State)
		}
		return res, nil
	}
	f := upstream.Classify(cctx, err, ex)
	var a2aErr *a2a.Error
	if f.Class != upstream.Protocol || !errors.As(err, &a2aErr) {
		// Not an answer from the service (unreachable, throttled, over a limit): the cancel was not
		// delivered, and the workflow decides (§8).
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("relay: forward_cancel of task %s: %s", in.TaskID, f.Message)
	}
	ue := &lifecycle.UpstreamError{Code: codeForError(err), Message: upstream.Cap(a2aErr.Error())}
	if errors.Is(err, a2a.ErrTaskNotCancelable) {
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelNotCancelable, UpstreamError: ue}, nil
	}
	return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelUnsupported, UpstreamError: ue}, nil
}
