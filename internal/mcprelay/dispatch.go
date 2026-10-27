package mcprelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/unimpl"
	"kontra.sh/kontra/internal/upstream"
)

// Dispatcher is the MCP half of lifecycle.DispatchActivities, for tasks with protocol mcp; internal/relay's
// Dispatcher hands those to it (relay.Dispatcher.MCP), and it implements the whole interface itself. It runs on the
// replica holding the attempt's tunnel session (kontra-tunnel-<replica>) or on kontra-direct, heartbeats every 5 s,
// fences every write (D77) and returns only an outcome and a reason (D102); nothing it relays enters Temporal
// history. Its dispatches register in the replica's relay.Registry, so abort reaches them as it reaches an A2A
// dispatch (spec §8, D44).
//
// It speaks to the upstream in the era of the request that created the task (spec/relay.md §11.12):
//
//   - 2025-11-25 (§11.5 steps 3–6, D22; legacy_dispatch.go): its own upstream session (an initialize declaring the
//     client's elicitation and sampling capabilities, recorded in attempts.upstream_session); the task-augmented
//     tools/call reusing the client's JSON-RPC id, with ttl = time to the deadline; the upstream taskId as the
//     first upstream task id; tasks/get polling at the upstream's pollInterval (0.5–30 s) and a GET on its
//     session; on input_required, tasks/result on its session to receive the upstream's elicitation/create and
//     sampling/createMessage, each logged as an input_request under the gate id k.<taskId>.<seq>; the client's
//     answers, tailed from the log, written upstream under the original ids; at terminal, tasks/result logged
//     as the result, one calls row with task_id, and its session closed.
//   - 2026-07-28 (D159): stateless requests on the attempt's session with the stored client _meta; a
//     CallToolResult is the result; a CreateTaskResult starts tasks/get polling (Mcp-Name = the upstream id);
//     inputRequests and an InputRequiredResult become input_request events under gate ids, the upstream keys
//     and requestState kept in the event; answers go upstream as tasks/update, or as the tools/call sent again.
//     The first InputRequiredResult counts as the first upstream task id, its first inputRequests key recorded
//     as attempts.upstream_task_id, so the call is never sent afresh after it (D178).
//
// Every exchange carries the client session's hash and the client's request id in its preamble, so a sealed
// call's binding holds (spec/tunnel.md §8.1, spec/ext-sealed.md §5 rule 4). After the first upstream task id,
// a lost tunnel fails the task tunnel_lost (D32, mcp_task_resume).
//
// Work packages: M3 WP-06 (2025-11-25), M3 WP-07 (2026-07-28).
type Dispatcher struct {
	ReplicaID string
	Log       *store.Log
	Registry  *relay.Registry
	Services  Services
	Tunnels   Tunnels // this replica's tunnel registry; nil on a replica that holds no tunnels
	Waker     store.TaskWaker
	// Signals sends kontra.dispatched on the first upstream task id (*lifecycle.Client).
	Signals interface {
		SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error
	}
	// Heartbeater beats every 5 s while a dispatch runs; lifecycle.Heartbeater when nil.
	Heartbeater workqueue.Heartbeater
	// TaskBytes is limits.task_bytes (spec §6.6): what a task's events and objects may hold; 256 MiB when 0.
	TaskBytes int64
	Logger    *slog.Logger

	// Timings, replaceable in tests.
	minPoll, maxPoll time.Duration // the bounds of the upstream's pollInterval (§11.5 step 4: 0.5–30 s)
	failFor          time.Duration // how long a poll may fail to reach the upstream before the task fails (§6.4's 30 s)
	cancelWait       time.Duration // forward_cancel's bound (§11.6: at most 10 s)
	cancelHold       time.Duration // how long a run marked by forward_cancel waits for the workflow's end (holdForWorkflow)

	// caps holds, per dispatch (relay.Key), whether its upstream declared tasks.cancel, for forward_cancel on this
	// replica (§11.6: tasks/cancel is sent upstream only if the upstream declared it).
	caps sync.Map
	// cancels holds, per dispatch running on this replica (relay.Key), the flag forward_cancel sets before it sends
	// tasks/cancel (legacyRun.cancelling): registered when the dispatch starts, before BeginDispatch records this
	// replica as the one forward_cancel is routed to (spec §14.2).
	cancels sync.Map
}

var _ lifecycle.DispatchActivities = (*Dispatcher)(nil)

// DefaultTaskBytes is limits.task_bytes's default (spec §6.6).
const DefaultTaskBytes = 256 << 20

func (d *Dispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *Dispatcher) heartbeater() workqueue.Heartbeater {
	if d.Heartbeater == nil {
		return lifecycle.Heartbeater{}
	}
	return d.Heartbeater
}

func (d *Dispatcher) pollBounds() (time.Duration, time.Duration) {
	lo, hi := d.minPoll, d.maxPoll
	if lo <= 0 {
		lo = 500 * time.Millisecond
	}
	if hi <= 0 {
		hi = 30 * time.Second
	}
	return lo, hi
}

func (d *Dispatcher) failWindow() time.Duration {
	if d.failFor > 0 {
		return d.failFor
	}
	return 30 * time.Second
}

func (d *Dispatcher) forwardCancelWait() time.Duration {
	if d.cancelWait > 0 {
		return d.cancelWait
	}
	return 8 * time.Second
}

// cancelHoldFor bounds how long a dispatch marked by forward_cancel waits for the workflow's terminal event: the
// workflow writes it at most 10 s after the cancel started (§11.6), or at once at the deadline (§6.5).
func (d *Dispatcher) cancelHoldFor() time.Duration {
	if d.cancelHold > 0 {
		return d.cancelHold
	}
	return 30 * time.Second
}

func (d *Dispatcher) taskBytes() int64 {
	if d.TaskBytes > 0 {
		return d.TaskBytes
	}
	return DefaultTaskBytes
}

// preambleTimeout bounds the reads a dispatch makes before it calls the upstream (as relay's does).
const preambleTimeout = 10 * time.Second

// Dispatch runs one dispatch of an MCP task (spec §14.2): before any upstream call it writes attempts.replica_id
// and a fresh dispatch_id (the fence, D77), loads and consumes the task's work item, resolves the attempt's
// backend (§6.1's check for a tunnel session: a session gone opens no stream, tunnel_lost; after the first
// upstream task id the dispatch logs failed: tunnel_lost itself, D149), registers in the replica's registry, and
// runs the exchange of the request's era. It ends the attempt when it ends it (a terminal event, a failure that is
// retried, an abort); a lost context leaves it for a later dispatch (a direct backend re-attaches, D42).
func (d *Dispatcher) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	taskID, err := ids.ParseTaskID(in.TaskID)
	if err != nil {
		return lifecycle.DispatchResult{}, fmt.Errorf("mcprelay: dispatch of task %q: %w", in.TaskID, err)
	}
	log := d.logger().With(obs.Task(in.TaskID, in.N)...).With("component", "mcp-dispatch")
	hb := d.heartbeater()
	if err := hb.Heartbeat(ctx); err != nil {
		log.Info("the dispatch's activity ended before it started", "error", err)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	}
	pctx, pcancel := context.WithTimeout(ctx, preambleTimeout)
	defer pcancel()
	row, err := d.Log.Load(pctx, taskID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if row.Protocol != store.ProtocolMCP {
		return lifecycle.DispatchResult{}, fmt.Errorf("mcprelay: task %s is an %s task", taskID, row.Protocol)
	}
	if row.State.Terminal() {
		d.endAttempt(ctx, log, taskID, in.N, terminalOutcome(row))
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, nil
	}
	key := relay.Key{TaskID: a2a.TaskID(in.TaskID), N: in.N}
	release := d.Registry.Expect(key)
	defer release()
	cancelling := new(atomic.Bool)
	d.cancels.Store(key, cancelling)
	defer d.cancels.CompareAndDelete(key, cancelling)
	fence, err := d.Log.BeginDispatch(pctx, taskID, in.N, d.ReplicaID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	wi, err := d.workItem(pctx, taskID, in)
	if errors.Is(err, store.ErrWorkItemNotFound) {
		log.Error("the task's work item is missing or another task's", "work_item", in.WorkItemID)
		return d.failTask(ctx, log, fence, store.ReasonLifecycleUnavailable, "the task's request could not be found")
	}
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	work := wi.Payload.MCP
	attempt, err := d.Log.GetAttempt(pctx, taskID, in.N)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	svc, err := d.Services.MCPServiceByID(pctx, row.TenantID, row.Service)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if svc == nil || svc.Disabled {
		return d.failTask(ctx, log, fence, store.ReasonServiceUnavailable, "")
	}
	if work.Version == mcpwire.V20260728 {
		return d.dispatchModern(ctx, in, work)
	}
	link, err := d.link(svc, attempt, row, work)
	switch {
	case errors.Is(err, upstream.ErrTunnelLost) && attempt.FirstUpstreamIDAt != nil:
		log.Info("the attempt's tunnel session is gone after the first upstream task id", "error", err)
		return d.failTask(ctx, log, fence, store.ReasonTunnelLost, "")
	case errors.Is(err, upstream.ErrTunnelLost):
		log.Info("the attempt's tunnel session is not held here; no stream is opened", "error", err)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTunnelLost}, nil
	case err != nil:
		log.Warn("the attempt's backend is gone", "error", err)
		return d.failTask(ctx, log, fence, store.ReasonServiceUnavailable, "")
	}
	if err := hb.Heartbeat(ctx); err != nil {
		log.Info("the dispatch's activity ended before it called the upstream", "error", err)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	}
	dctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	running, err := d.Registry.Register(key, cancel)
	release()
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if running.Aborted() {
		running.Done()
		log.Info("dispatch aborted before it called the upstream")
		d.endAttempt(ctx, log, taskID, in.N, store.OutcomeAborted)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchAborted}, nil
	}
	defer d.caps.Delete(key)
	r := &legacyRun{
		d: d, log: log, key: key, taskID: taskID, n: in.N, fence: fence, row: row, workItem: wi.ID, work: work,
		answered: wi.AnswersDelivered, attempt: attempt, link: link, sealed: work.Sealed, resume: attempt.UpstreamTaskID != "",
		cancelling: cancelling,
	}
	// The dispatch beats every 5 s for its whole run (spec §14.2: heartbeat timeout 30 s), a slow upstream call
	// included, so load never looks like a dead replica; a cancellation Temporal delivers in a heartbeat's answer
	// ends the run's context.
	stopBeat := beat(dctx, hb)
	res := r.run(dctx)
	stopBeat()
	cause := context.Cause(dctx)
	running.Done()
	return d.settle(ctx, log, in, fence, running, res, cause), nil
}

// beat heartbeats every hb.HeartbeatInterval() until the returned stop is called or ctx ends.
func beat(ctx context.Context, hb workqueue.Heartbeater) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		every := hb.HeartbeatInterval()
		if every <= 0 {
			every = 5 * time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = hb.Heartbeat(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// dispatchModern is the dispatch of a task a 2026-07-28 request created (§11.12, D159, D178): M3 WP-07's.
func (d *Dispatcher) dispatchModern(ctx context.Context, in lifecycle.DispatchInput, work *store.MCPWork) (lifecycle.DispatchResult, error) {
	return lifecycle.DispatchResult{}, unimpl.Error("mcprelay.Dispatcher.dispatchModern", "M3 WP-07")
}

// workItem loads the task's work item, consuming it on its first dispatch (D66). A later dispatch of the same
// attempt (a resume) or of a new attempt (a retry, n > 1) reloads the item it owns; any other dispatch that finds
// it consumed refuses it, since the dispatch that took it may have sent it (D101). Its payload holds an MCP call.
func (d *Dispatcher) workItem(ctx context.Context, taskID uuid.UUID, in lifecycle.DispatchInput) (*store.WorkItem, error) {
	wid, err := uuid.Parse(in.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("work item id %q: %w", in.WorkItemID, store.ErrWorkItemNotFound)
	}
	err = d.Log.ConsumeWorkItem(ctx, wid, taskID)
	switch {
	case err == nil, in.Resume, in.N > 1 && errors.Is(err, store.ErrWorkItemConsumed):
	case errors.Is(err, store.ErrWorkItemConsumed):
		return nil, fmt.Errorf("mcprelay: work item %s was already consumed by another dispatch of attempt %d", wid, in.N)
	default:
		return nil, err
	}
	wi, err := d.Log.LoadWorkItem(ctx, wid, taskID)
	if err != nil {
		return nil, err
	}
	if wi.Payload.MCP == nil {
		return nil, fmt.Errorf("mcprelay: work item %s holds no MCP call: %w", wid, store.ErrWorkItemNotFound)
	}
	return wi, nil
}

// link is the attempt's upstream: the service's direct backend, or the attempt's tunnel session on this replica
// after §6.1's check (an error wrapping upstream.ErrTunnelLost when this replica does not hold it or it no longer
// serves the service), with the preamble's task and MCP fields (tunnel.md §8.1).
func (d *Dispatcher) link(svc *Service, attempt *store.Attempt, row *store.TaskRow, work *store.MCPWork) (*mcpLink, error) {
	k := &mcpLink{
		task:    upstream.ExchangeTask{TaskID: row.ID.String(), ContextID: row.ContextID},
		mcp:     upstream.MCPExchange{Session: work.Session, RequestID: idString(work.RequestID)},
		version: work.Version,
		sid:     attempt.UpstreamSession,
	}
	switch attempt.Backend {
	case store.BackendDirect:
		if svc.Direct == nil {
			return nil, errors.New("the service no longer has a direct backend")
		}
		k.backend = svc.Direct
	case store.BackendTunnel:
		if svc.Direct != nil {
			return nil, errors.New("the service no longer has a tunnel backend")
		}
		if attempt.TunnelSession == nil || d.Tunnels == nil {
			return nil, fmt.Errorf("%w: no tunnel session on this replica", upstream.ErrTunnelLost)
		}
		be, err := d.Tunnels.MCPBackend(*attempt.TunnelSession, row.TenantID, row.Service)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", upstream.ErrTunnelLost, err)
		}
		k.backend, k.tunnelled, k.session = be, true, *attempt.TunnelSession
		k.tenantID, k.service, k.tunnels = row.TenantID, row.Service, d.Tunnels
	default:
		return nil, fmt.Errorf("attempt backend %q", attempt.Backend)
	}
	return k, nil
}

// settle is the dispatch's result once its run returned: a superseded dispatch's is a lost context (D77); an
// aborted one ends its attempt aborted (unless the task ended first); a terminal one ends its attempt with the
// task's outcome; a retryable one with its reason.
func (d *Dispatcher) settle(ctx context.Context, log *slog.Logger, in lifecycle.DispatchInput, fence *store.Fence,
	running *relay.Running, res lifecycle.DispatchResult, cause error) lifecycle.DispatchResult {
	if running.Superseded() || fence.Fenced() || errors.Is(cause, relay.ErrSuperseded) {
		log.Info("dispatch superseded by a newer dispatch of the task")
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}
	}
	row, err := d.Log.Load(context.WithoutCancel(ctx), fence.TaskID)
	if err == nil && row.State.Terminal() {
		d.endAttempt(ctx, log, fence.TaskID, in.N, terminalOutcome(row))
		res.Outcome = lifecycle.DispatchTerminal
		if res.Reason == "" && row.Snapshot != nil {
			res.Reason, _ = row.Snapshot.Metadata[store.MetaFailure].(string)
		}
		return res
	}
	if running.Aborted() || errors.Is(cause, relay.ErrAborted) {
		d.endAttempt(ctx, log, fence.TaskID, in.N, store.OutcomeAborted)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchAborted}
	}
	switch res.Outcome {
	case lifecycle.DispatchRetryable:
		d.endAttempt(ctx, log, fence.TaskID, in.N, store.Outcome(res.Reason))
		return res
	case lifecycle.DispatchTerminal:
		// The terminal write did not reach the log (the store failed): the attempt is followed again.
		log.Warn("the dispatch's terminal event is not in the log; the attempt is followed again", "error", err)
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}
	case "":
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}
	}
	return res
}

// failTask logs `failed: <reason>` (its MCP status row and the task's calls row, §11.8 case 2) under the dispatch's
// fence and ends the attempt.
func (d *Dispatcher) failTask(ctx context.Context, log *slog.Logger, fence *store.Fence, reason store.FailureReason, text string) (lifecycle.DispatchResult, error) {
	err := appendFenced(ctx, d.Log, fence, func(row *store.TaskRow) (store.Write, error) {
		return failWrite(ctx, d.Log, row, reason, text)
	})
	switch {
	case errors.Is(err, store.ErrFenced):
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchContextLost}, nil
	case errors.Is(err, errTaskEnded):
	case err != nil:
		return lifecycle.DispatchResult{}, err
	}
	row, err := d.Log.Load(ctx, fence.TaskID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	d.endAttempt(ctx, log, fence.TaskID, fence.N, terminalOutcome(row))
	return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal, Reason: string(reason)}, nil
}

// failWrite is `failed: <reason>` as an MCP task logs it, with the task's calls row (is_error true).
func failWrite(ctx context.Context, l *store.Log, row *store.TaskRow, reason store.FailureReason, text string) (store.Write, error) {
	if text == "" {
		text = store.ReasonText(reason)
	}
	w, err := store.MCPStatusWrite(row, store.StateFailed, text, map[string]any{store.MetaFailure: string(reason)})
	if err != nil {
		return store.Write{}, err
	}
	if w.Call, err = l.MCPCall(ctx, row.ID, true, int64(len(w.Payload))); err != nil {
		return store.Write{}, err
	}
	return w, nil
}

// errTaskEnded is a write refused because the task is terminal (a cancel, the timeout or another writer ended it):
// the dispatch stops.
var errTaskEnded = errors.New("mcprelay: the task has ended")

// appendFenced appends the write build makes from the task's current row under the dispatch's fence, again on a
// concurrent write (a client's answer, a patch), at most 8 times. A terminal task is errTaskEnded; a fence that no
// longer holds store.ErrFenced.
func appendFenced(ctx context.Context, l *store.Log, fence *store.Fence, build func(row *store.TaskRow) (store.Write, error)) error {
	var last error
	for range 8 {
		row, err := l.Load(ctx, fence.TaskID)
		if err != nil {
			return err
		}
		if row.State.Terminal() {
			return errTaskEnded
		}
		w, err := build(row)
		if err != nil {
			return err
		}
		w.Fence = fence
		_, err = l.Append(ctx, nil, w)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, store.ErrFenced):
			return err
		case errors.Is(err, taskstore.ErrConcurrentModification):
			last = err
			continue
		}
		return err
	}
	return last
}

// terminalOutcome is attempts.outcome for a task that ended (spec §4).
func terminalOutcome(row *store.TaskRow) store.Outcome {
	var reason string
	if row.Snapshot != nil {
		reason, _ = row.Snapshot.Metadata[store.MetaFailure].(string)
	}
	return store.TerminalOutcome(row.State, store.FailureReason(reason))
}

func (d *Dispatcher) endAttempt(ctx context.Context, log *slog.Logger, taskID uuid.UUID, n int, o store.Outcome) {
	if !o.Valid() {
		o = store.OutcomeFailed
	}
	if err := d.Log.EndAttempt(context.WithoutCancel(ctx), taskID, n, o); err != nil {
		log.Warn("end attempt", "outcome", o, "error", err)
	}
}

// Abort implements lifecycle.DispatchActivities: it cancels the running dispatch of (task, n) through the
// replica's registry, or marks one about to register so that it stops before calling the upstream (spec §8,
// D44).
func (d *Dispatcher) Abort(ctx context.Context, in lifecycle.AttemptRef) error {
	if !d.Registry.Abort(relay.Key{TaskID: a2a.TaskID(in.TaskID), N: in.N}) {
		d.logger().Info("abort: no MCP dispatch of the attempt runs on this replica", obs.Task(in.TaskID, in.N)...)
	}
	return nil
}

// ForwardCancel sends tasks/cancel upstream when the upstream declared it (legacy: tasks.cancel; modern: the
// extension), and returns at most 10 s later whatever it answered: an MCP cancel always cancels (D57), and the
// workflow writes canceled whatever this answers. It is sent on the dispatch's own upstream session
// (attempts.upstream_session) with the task's preamble, through the attempt's tunnel session on this replica
// (tunnel_lost, opening no stream, when this replica does not hold it) or the direct backend. Its result crosses
// Temporal history: an outcome, the upstream's code and its task's state in a fixed vocabulary, never its text.
//
// Before anything is sent it marks the attempt's dispatch when it runs on this replica (forward_cancel runs on
// kontra-ctl-<replica> of the replica dispatching, spec §14.2): from then on the dispatch writes no terminal event of
// its own, so the upstream's reaction to the cancel (its task cancelled, an error ending its tasks/result) never
// becomes the task's end before the workflow's canceled (§11.6).
func (d *Dispatcher) ForwardCancel(ctx context.Context, in lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	taskID, err := ids.ParseTaskID(in.TaskID)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	if flag, ok := d.cancels.Load(relay.Key{TaskID: a2a.TaskID(in.TaskID), N: in.N}); ok {
		flag.(*atomic.Bool).Store(true)
	}
	log := d.logger().With(obs.Task(in.TaskID, in.N)...).With("component", "mcp-dispatch")
	attempt, err := d.Log.GetAttempt(ctx, taskID, in.N)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	if attempt.UpstreamTaskID == "" {
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("mcprelay: attempt %d of task %s has no upstream task to cancel", in.N, in.TaskID)
	}
	row, err := d.Log.Load(ctx, taskID)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	work, err := d.Log.MCPWorkItem(ctx, taskID)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	if work.Version == mcpwire.V20260728 {
		return lifecycle.ForwardCancelResult{}, unimpl.Error("mcprelay.Dispatcher.ForwardCancel (2026-07-28)", "M3 WP-07")
	}
	svc, err := d.Services.MCPServiceByID(ctx, row.TenantID, row.Service)
	if err != nil {
		return lifecycle.ForwardCancelResult{}, err
	}
	if svc == nil {
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("mcprelay: forward_cancel of task %s: the service is gone", in.TaskID)
	}
	link, err := d.link(svc, attempt, row, work)
	switch {
	case errors.Is(err, upstream.ErrTunnelLost):
		log.Info("forward_cancel: the attempt's tunnel session is gone; nothing to forward", "error", err)
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelTunnelLost}, nil
	case err != nil:
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("mcprelay: forward_cancel of task %s: %w", in.TaskID, err)
	}
	// SPEC-CONFLICT (relay §11.6: forward_cancel "sends upstream tasks/cancel only if the upstream declared
	// tasks.cancel"): what the upstream declared is in the InitializeResult of the dispatch's own session, which
	// attempts (0001's columns, frozen with 0002) has no place for; the replica running the dispatch keeps it in
	// memory. The closest faithful behaviour: on that replica, an upstream that did not declare it gets nothing;
	// where it is not known (the dispatching process is gone, a forward_cancel on kontra-direct-ctl), tasks/cancel is
	// sent, and an upstream that does not serve it answers -32601, which changes nothing: the workflow writes canceled
	// whatever forward_cancel answers (D57).
	if declared, ok := d.caps.Load(relay.Key{TaskID: a2a.TaskID(in.TaskID), N: in.N}); ok && !declared.(bool) {
		log.Info("forward_cancel: the upstream declared no tasks.cancel; nothing is sent")
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelUnsupported}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, d.forwardCancelWait())
	defer cancel()
	params, _ := jsonObject("taskId", attempt.UpstreamTaskID)
	m, err := link.request(cctx, "kontra-cancel-"+uuid.NewString(), "tasks/cancel", params, nil)
	if err != nil {
		var xe *exchangeError
		if errors.As(err, &xe) && xe.rpc != nil {
			log.Info("the upstream refused the cancel", "code", xe.rpc.Code)
			return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelNotCancelable, UpstreamError: &lifecycle.UpstreamError{Code: xe.rpc.Code}}, nil
		}
		if errors.As(err, &xe) && xe.class == classTunnelLost && !link.held() {
			return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelTunnelLost}, nil
		}
		log.Info("forward_cancel got no answer from the service", "error", err)
		return lifecycle.ForwardCancelResult{}, fmt.Errorf("mcprelay: forward_cancel of task %s: the service could not be reached", in.TaskID)
	}
	return cancelAnswer(log, m.Result), nil
}

// cancelAnswer is forward_cancel's result for the upstream's answer to tasks/cancel (a Task): its outcome, and its
// status only as one of MCP's five, carried as the A2A state ForwardCancelResult.UpstreamState names (store.State.A2A,
// as relay's cancelAnswer carries a2a.TaskState). Any other value, of any length, is the upstream's text and stays
// out of the result, which Temporal history keeps (spec §14: service content never enters it); so does a status
// read from an answer strict decoding refuses.
func cancelAnswer(log *slog.Logger, result json.RawMessage) lifecycle.ForwardCancelResult {
	if err := mcpwire.CheckTask(result); err != nil {
		log.Info("strict decoding refused the upstream's answer to tasks/cancel", "level", "result")
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelContinues}
	}
	status, _ := stringMember(result, "status")
	st, ok := store.StateFromMCP(status)
	switch {
	case !ok:
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelContinues}
	case st == store.StateCanceled:
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelCanceled, UpstreamState: string(st.A2A())}
	case st.Terminal():
		return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelNotCancelable, UpstreamState: string(st.A2A())}
	}
	return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelContinues, UpstreamState: string(st.A2A())}
}
