// Package relay is the A2A relay on a2a-go v2.7.0 (spec/relay.md §4, §6–§8, §15).
//
// One a2a-go handler per service, in cluster mode over the Postgres store, the Postgres puller and the
// Temporal-backed work queue, with call interceptors for authentication, admission, the follow-up state
// checks of §7, the quoted guard, cancel (§8), inline push configs (§10.1), review-window messages and
// the anonymous refusals of §2.3. Its executor is the relay: it dispatches the caller's message upstream
// and logs what comes back, with gate ids, through a2a-go's processor.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// ExecutorOptions are the relay's timings and limits; zero fields take the defaults.
type ExecutorOptions struct {
	PollInterval  time.Duration // GetTask polling, 1 s (spec §6.2)
	PollMax       time.Duration // the polling interval after PollSlowAfter without change, 5 s
	PollSlowAfter time.Duration // 1 min
	// GetTaskGrace is how long GetTask may fail to reach the service before the task fails, 30 s (§6.4).
	GetTaskGrace time.Duration
	// GetTaskTimeout bounds one GetTask call: an upstream that accepts it and never answers fails the call,
	// which counts toward GetTaskGrace. 10 s, and at most GetTaskGrace.
	GetTaskTimeout time.Duration
	// TaskBytes caps the part bytes stored per task (limits.task_bytes, 256 MiB, §6.6).
	TaskBytes int64
	// MaxPart is the largest part stored inline: without a bucket, an upstream part over it fails the
	// task upstream_error (§10.4).
	MaxPart int64
}

func (o ExecutorOptions) withDefaults() ExecutorOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.PollMax <= 0 {
		o.PollMax = 5 * time.Second
	}
	if o.PollSlowAfter <= 0 {
		o.PollSlowAfter = time.Minute
	}
	if o.GetTaskGrace <= 0 {
		o.GetTaskGrace = 30 * time.Second
	}
	if o.GetTaskTimeout <= 0 {
		o.GetTaskTimeout = min(10*time.Second, o.GetTaskGrace)
	}
	if o.TaskBytes <= 0 {
		o.TaskBytes = 256 << 20
	}
	if o.MaxPart <= 0 {
		o.MaxPart = upstream.MaxPart
	}
	return o
}

// Signaler sends kontra.dispatched when a dispatch records its attempt's first upstream task id (spec
// §13.3, §14.1); lifecycle.Client implements it.
type Signaler interface {
	SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error
}

// Executor is the relay's a2asrv.AgentExecutor (spec §6.2–§6.4, §15). It
//
//   - never yields a Task (the gate task exists: the work queue pre-created it) and never a bare
//     Message (an upstream Message becomes a terminal completed status carrying it, D5);
//   - is told a first turn, a follow-up turn (the sticky attempt's upstream ids substituted) and a
//     re-attach (Resume, D42) apart by the dispatch that runs it: the attempt row says whether an upstream
//     task exists, and the Dispatcher's input says whether this dispatch resumes;
//   - calls SendStreamingMessage when the upstream card streams, falling back in the same attempt to
//     SendMessage{returnImmediately: true} plus GetTask polling (1 s, backing off to 5 s after a minute
//     without change) on UnsupportedOperationError before any event (D15);
//   - records the first upstream task id before logging the event that carried it, and signals
//     kontra.dispatched;
//   - rewrites every upstream id to the gate's at every depth and strips Kontra's keys (§2.5), fails the
//     attempt upstream_error on an event naming a foreign upstream task;
//   - follows the upstream through auth-required and ends its turn on a terminal state or input-required;
//   - emits explicit terminal events with the upstream's message, recording them in the Registry so the
//     store lets them through (D8); an error before the first upstream task id that §6.4 retries is
//     returned as an executor error with the outcome retryable, and nothing terminal is logged.
type Executor struct {
	log      *store.Log
	registry *Registry
	backends Backends
	svc      store.ServiceRef
	signals  Signaler
	logger   *slog.Logger
	opts     ExecutorOptions
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// NewExecutor returns the relay executor.
func NewExecutor(log *store.Log, registry *Registry, backends Backends) *Executor {
	return &Executor{log: log, registry: registry, backends: backends, logger: slog.Default(), opts: ExecutorOptions{}.withDefaults()}
}

// ForService returns a copy of the executor bound to svc, with sig for kontra.dispatched, a logger and
// options (zero options take the defaults).
func (e *Executor) ForService(svc store.ServiceRef, sig Signaler, logger *slog.Logger, opts ExecutorOptions) *Executor {
	cp := *e
	cp.svc, cp.signals, cp.opts = svc, sig, opts.withDefaults()
	if logger != nil {
		cp.logger = logger
	}
	return &cp
}

// dispatchInfo is what the Dispatcher hands the executor through the context a2a-go's handler runs it on.
type dispatchInfo struct {
	n       int
	resume  bool
	fence   *store.Fence
	running *Running
	task    *store.TaskRow
	svc     store.ServiceRef
	request *a2a.SendMessageRequest // the work item's request: its configuration's acceptedOutputModes
}

type dispatchKey struct{}

func withDispatch(ctx context.Context, d *dispatchInfo) context.Context {
	return context.WithValue(ctx, dispatchKey{}, d)
}

func dispatchFrom(ctx context.Context) (*dispatchInfo, bool) {
	d, ok := ctx.Value(dispatchKey{}).(*dispatchInfo)
	return d, ok
}

// errRetryable is the error the executor returns for a failure §6.4 retries: a2a-go's own SetTaskFailed
// that follows is refused by the store, and the dispatch returns retryable from the registry.
var errRetryable = errors.New("relay: upstream failed before its first task id; the attempt is retried")

// Execute runs one turn of one attempt.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		r := &run{e: e, ec: ec, yield: yield}
		r.execute(ctx)
	}
}

// Cancel is never reached: CancelTask is decided by CancelInterceptor before a2a-go's cancel path,
// which would mark the task failed when the upstream refuses (D4).
func (e *Executor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(nil, fmt.Errorf("relay: CancelTask is decided by the gate's cancel interceptor (D4): %w", a2a.ErrUnsupportedOperation))
	}
}

// run is one execution of the executor.
type run struct {
	e     *Executor
	ec    *a2asrv.ExecutorContext
	d     *dispatchInfo
	yield func(a2a.Event, error) bool

	ids       IDs
	taskUUID  uuid.UUID
	proj      *a2a.Task // the gate projection as this run's yields leave it
	info      upstream.Info
	client    *a2aclient.Client
	ex        *upstream.Exchange
	firstTask bool  // no upstream event of the attempt has been handled yet (no upstream task id at the start)
	bytes     int64 // part bytes the task stores
	finished  bool  // a final event was yielded, or an error
	logger    *slog.Logger
}

func (r *run) execute(ctx context.Context) {
	d, ok := dispatchFrom(ctx)
	if !ok {
		r.finish(nil, errors.New("relay: the relay executor runs only inside a dispatch"))
		return
	}
	r.d = d
	r.logger = r.e.logger.With(obs.Task(string(r.ec.TaskID), d.n)...)
	id, err := ids.ParseTaskID(string(r.ec.TaskID))
	if err != nil {
		r.finish(nil, err)
		return
	}
	r.taskUUID = id
	if r.ec.StoredTask == nil {
		r.finish(nil, errors.New("relay: the gate task does not exist (the work queue pre-creates it)"))
		return
	}
	r.proj = r.ec.StoredTask
	r.bytes = taskBytes(r.proj)
	attempt, err := r.e.log.GetAttempt(ctx, id, d.n)
	if err != nil {
		// Nothing was sent: the workflow re-reads the attempt and retries or re-attaches.
		d.running.SetOutcome(lifecycle.DispatchContextLost, "")
		r.finish(nil, err)
		return
	}
	r.ids = IDs{
		GateTaskID: r.ec.TaskID, GateContextID: r.ec.ContextID,
		UpstreamTaskID: a2a.TaskID(attempt.UpstreamTaskID), UpstreamContextID: attempt.UpstreamContextID,
	}
	r.firstTask = r.ids.UpstreamTaskID == ""
	backend, info, err := r.e.backends.Backend(ctx, d.svc)
	if err != nil {
		r.fail(store.ReasonServiceUnavailable, "the service is unavailable: no usable agent card")
		return
	}
	r.info = info
	client, err := upstream.NewClient(ctx, backend, info, activated(r.ec))
	if err != nil {
		r.fail(store.ReasonServiceUnavailable, "the service's card names no interface the gate can speak")
		return
	}
	r.client = client
	defer client.Destroy()
	ctx, r.ex = upstream.WithExchange(ctx)
	if d.resume {
		r.reattach(ctx)
		return
	}
	r.send(ctx)
}

// activated is the A2A-Extensions the caller activated (the edge already dropped URIs the served card does
// not declare); NewClient keeps those the upstream card declares.
func activated(ec *a2asrv.ExecutorContext) []string {
	if ec.ServiceParams == nil {
		return nil
	}
	vals, _ := ec.ServiceParams.Get(a2a.SvcParamExtensions)
	var out []string
	for _, v := range vals {
		for u := range strings.SplitSeq(v, ",") {
			if u = strings.TrimSpace(u); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// finish yields a terminal outcome: an event, or an error (a2a-go then tries SetTaskFailed, which the
// store refuses while the dispatch is registered).
func (r *run) finish(ev a2a.Event, err error) {
	if r.finished {
		return
	}
	r.finished = true
	r.yield(ev, err)
}

// emit yields one translated event: a terminal one is recorded in the registry first, and its outcome
// set, because a2a-go's consumer may store it before yield returns. It reports whether the turn ended.
func (r *run) emit(ev a2a.Event) bool {
	if r.finished {
		return true
	}
	final := a2aevent.IsFinal(ev)
	if final {
		if st, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
			if st.Status.State == a2a.TaskStateInputRequired {
				r.d.running.SetOutcome(lifecycle.DispatchInterrupted, "")
			} else {
				reason, _ := st.Metadata[store.MetaFailure].(string)
				r.d.running.RecordTerminal(ev)
				r.d.running.SetOutcome(lifecycle.DispatchTerminal, store.FailureReason(reason))
			}
		}
	}
	if !r.yield(ev, nil) {
		r.finished = true
		return true
	}
	if final {
		r.finished = true
		return true
	}
	if next, err := a2aevent.ApplyShallowUpdate(r.proj, ev); err == nil {
		r.proj = next
	}
	return false
}

// fail logs `failed: <reason>` (spec §6.4) and ends the turn.
func (r *run) fail(reason store.FailureReason, text string) {
	r.emit(Failed(r.proj, string(reason), upstream.Cap(text)))
}

// retry ends the turn without a terminal event: the workflow starts a new attempt (§6.4).
func (r *run) retry(reason store.FailureReason, wait time.Duration, cause error) {
	r.d.running.SetOutcome(lifecycle.DispatchRetryable, reason)
	if wait > 0 {
		r.d.running.SetRetryAfter(wait)
	}
	r.finish(nil, fmt.Errorf("%w: %w", errRetryable, cause))
}

// upstreamFailed applies §6.4's table to a failed upstream call.
func (r *run) upstreamFailed(ctx context.Context, err error) {
	f := upstream.Classify(ctx, err, r.ex)
	first := r.ids.UpstreamTaskID != ""
	r.logger.Info("upstream call failed", "class", int(f.Class), "first_upstream_id", first, "error", f.Message)
	switch f.Class {
	case upstream.Canceled:
		// abort, a superseding dispatch, or the activity's end: the Dispatcher maps it.
		r.finish(nil, err)
	case upstream.Unreachable:
		if !first {
			r.retry(store.ReasonUpstreamUnreachable, 0, err)
			return
		}
		r.fail(store.ReasonUpstreamUnreachable, f.Message)
	case upstream.Throttled:
		if !first {
			r.retry(store.ReasonUpstreamError, f.RetryAfter, err)
			return
		}
		r.fail(store.ReasonUpstreamError, f.Message)
	case upstream.Limit:
		r.fail(store.ReasonUpstreamError, f.Message)
	default:
		r.fail(store.ReasonUpstreamError, "the service returned an error: "+f.Message)
	}
}

// send is a turn's first dispatch: the caller's message upstream, streaming first.
func (r *run) send(ctx context.Context) {
	msg, err := r.upstreamMessage(ctx)
	if err != nil {
		r.d.running.SetOutcome(lifecycle.DispatchContextLost, "")
		r.finish(nil, err)
		return
	}
	req := &a2a.SendMessageRequest{Message: msg, Metadata: r.ec.Metadata}
	if q := r.d.request; q != nil && q.Config != nil && len(q.Config.AcceptedOutputModes) > 0 {
		req.Config = &a2a.SendMessageConfig{AcceptedOutputModes: q.Config.AcceptedOutputModes}
	}
	if r.info.Streaming {
		if fallback := r.stream(ctx, req); !fallback {
			return
		}
		r.logger.Info("the upstream refused streaming; polling (D15)")
	}
	r.poll(ctx, req)
}

// upstreamMessage is the caller's message with the upstream ids of §4: on a follow-up the attempt's
// upstream task and context; on a first turn no task, and the upstream context an earlier task of the
// same gate context got from this backend (D53); referenceTaskIds mapped to the caller's own tasks.
func (r *run) upstreamMessage(ctx context.Context) (*a2a.Message, error) {
	if r.ec.Message == nil {
		return nil, errors.New("relay: no message to dispatch")
	}
	m := r.ids
	if m.UpstreamTaskID == "" {
		m.UpstreamContextID = ""
		prev, err := r.e.log.PreviousContextAttempt(ctx, r.d.svc, r.d.task.Owner, r.d.task.ContextID, store.BackendDirect)
		if err != nil {
			return nil, err
		}
		if prev != nil && prev.TaskID != r.taskUUID {
			m.UpstreamContextID = prev.UpstreamContextID
		}
	}
	refs, err := r.references(ctx)
	if err != nil {
		return nil, err
	}
	return m.ToUpstream(r.ec.Message, refs)
}

// references maps the caller's referenceTaskIds to the upstream task ids of the caller's own tasks in the
// same gate context on a direct backend of this service (spec §4); the rest are dropped.
func (r *run) references(ctx context.Context) (map[a2a.TaskID]a2a.TaskID, error) {
	var want []uuid.UUID
	for _, ref := range r.ec.Message.ReferenceTasks {
		if u, err := ids.ParseTaskID(string(ref)); err == nil {
			want = append(want, u)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	rows, err := r.e.log.DB().Query(ctx, `select t.id, a.upstream_task_id from kontra.tasks t
		join lateral (select upstream_task_id from kontra.attempts
		               where task_id = t.id and backend = 'direct' and upstream_task_id is not null
		               order by n desc limit 1) a on true
		where t.id = any($1::uuid[]) and t.tenant_id = $2 and t.service = $3 and t.buyer_id = $4 and t.context_id = $5`,
		want, r.d.svc.TenantID, r.d.svc.Name, r.d.task.Owner.BuyerID, r.d.task.ContextID)
	if err != nil {
		return nil, fmt.Errorf("relay: referenced tasks: %w", err)
	}
	defer rows.Close()
	out := map[a2a.TaskID]a2a.TaskID{}
	for rows.Next() {
		var gate uuid.UUID
		var up string
		if err := rows.Scan(&gate, &up); err != nil {
			return nil, err
		}
		out[a2a.TaskID(gate.String())] = a2a.TaskID(up)
	}
	return out, rows.Err()
}

// stream sends with SendStreamingMessage and relays until a final event. It reports fallback when the
// upstream refused streaming before any event (D15).
func (r *run) stream(ctx context.Context, req *a2a.SendMessageRequest) (fallback bool) {
	r.ex.Reset()
	got := false
	for ev, err := range r.client.SendStreamingMessage(ctx, req) {
		if err != nil {
			if !got && errors.Is(err, a2a.ErrUnsupportedOperation) {
				return true
			}
			r.upstreamFailed(ctx, err)
			return false
		}
		got = true
		if r.handle(ctx, ev) {
			return false
		}
	}
	if r.finished {
		return false
	}
	if r.ids.UpstreamTaskID == "" {
		r.fail(store.ReasonUpstreamError, "the service closed its stream without a task")
		return false
	}
	// SPEC-CONFLICT: §6.2–§6.4 say nothing of a stream the upstream closes cleanly before a terminal or
	// input-required event. The closest rule is the re-attach's ("a subscription that ends without a
	// terminal event falls back to polling", §6.4): the dispatch follows the upstream task by GetTask.
	r.follow(ctx)
	return false
}

// poll is the polling path: SendMessage with returnImmediately, then GetTask until a final state.
func (r *run) poll(ctx context.Context, req *a2a.SendMessageRequest) {
	cp := *req
	cfg := a2a.SendMessageConfig{ReturnImmediately: true}
	if req.Config != nil {
		cfg.AcceptedOutputModes = req.Config.AcceptedOutputModes
	}
	cp.Config = &cfg
	r.ex.Reset()
	res, err := r.client.SendMessage(ctx, &cp)
	if err != nil {
		r.upstreamFailed(ctx, err)
		return
	}
	switch v := res.(type) {
	case *a2a.Task:
		if r.handle(ctx, v) {
			return
		}
	case *a2a.Message:
		r.handle(ctx, v)
		return
	default:
		r.fail(store.ReasonUpstreamError, "the service answered SendMessage with neither a task nor a message")
		return
	}
	r.follow(ctx)
}

// follow polls GetTask (historyLength 0) every PollInterval, backing off to PollMax after PollSlowAfter
// without change, until a final state. A failed GetTask is handled by getTask: the 30 s rule for a call
// that does not reach the service, upstream_error at once for an answer that is an error.
func (r *run) follow(ctx context.Context) {
	interval := r.e.opts.PollInterval
	lastChange := time.Now()
	var g getTasks
	for !r.finished {
		select {
		case <-ctx.Done():
			r.finish(nil, context.Cause(ctx))
			return
		case <-time.After(g.next(interval, r.e.opts.GetTaskGrace)):
		}
		t := r.getTask(ctx, &g)
		if t == nil {
			continue // r.finished, or a failure the 30 s rule still allows
		}
		before := r.proj
		if r.handle(ctx, t) {
			return
		}
		if r.proj != before {
			lastChange, interval = time.Now(), r.e.opts.PollInterval
		} else if time.Since(lastChange) >= r.e.opts.PollSlowAfter {
			interval = r.e.opts.PollMax
		}
	}
}

// getTasks is the 30 s rule's clock over the successive GetTask calls of one loop (spec §6.4).
type getTasks struct {
	failingSince time.Time     // the start of the first call of the current run of failures
	retryAfter   time.Duration // the last throttled answer's Retry-After
}

// next is the wait before the next call: the interval, or a throttled answer's Retry-After up to the grace.
func (g *getTasks) next(interval, grace time.Duration) time.Duration {
	return max(interval, min(g.retryAfter, grace))
}

// getTask issues one GetTask (historyLength 0), bounded by GetTaskTimeout, and applies §6.4 to a failure.
// It returns the task, or nil when the call failed: then either the turn ended (r.finished: the dispatch's
// context ended, or the failure failed the task) or the call is to be made again.
//
// SPEC-CONFLICT: §6.4 states the 30 s GetTask rule for the re-attach only, and its table fails a task whose
// upstream id exists at the first refused or reset connection or 502/503/504 (upstream_unreachable) and at
// the first 408/429 (upstream_error). Read literally, one failed poll of an hour-long task on the polling
// path would fail it. Every GetTask loop (the polling path's and the re-attach's) applies the re-attach's
// rule to the failures that are about reaching the service: a call that is refused, reset, answered
// 502/503/504, throttled (408/429), or not answered within GetTaskTimeout counts toward GetTaskGrace (30 s),
// and when the grace runs out the task fails with the table's reason (upstream_unreachable;
// upstream_error for 408/429). A throttled call's Retry-After is honoured up to the grace. An answer that
// is an error is never retried, as the table says: a protocol error (a JSON-RPC error, another HTTP status,
// a malformed body) fails the task upstream_error with the upstream's message, and a body over its limit
// (§6.6) fails it upstream_error, at the first such answer. The call that carries the message
// (SendMessage, SendStreamingMessage) keeps the table's rule (upstreamFailed).
func (r *run) getTask(ctx context.Context, g *getTasks) *a2a.Task {
	zero := 0
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, r.e.opts.GetTaskTimeout)
	defer cancel()
	r.ex.Reset()
	t, err := r.client.GetTask(cctx, &a2a.GetTaskRequest{ID: r.ids.UpstreamTaskID, HistoryLength: &zero})
	g.retryAfter = 0
	if err == nil && t == nil {
		r.fail(store.ReasonUpstreamError, "the service answered GetTask without a task")
		return nil
	}
	if err == nil {
		g.failingSince = time.Time{}
		return t
	}
	if ctx.Err() != nil {
		r.finish(nil, context.Cause(ctx))
		return nil
	}
	var f upstream.Failure
	if cctx.Err() != nil {
		f = upstream.Failure{Class: upstream.Unreachable, Err: err,
			Message: "the service did not answer GetTask within " + r.e.opts.GetTaskTimeout.String()}
	} else {
		f = upstream.Classify(ctx, err, r.ex)
	}
	switch f.Class {
	case upstream.Unreachable, upstream.Throttled:
	case upstream.Limit:
		r.fail(store.ReasonUpstreamError, f.Message)
		return nil
	case upstream.Canceled:
		r.finish(nil, err)
		return nil
	default:
		r.fail(store.ReasonUpstreamError, "the service returned an error: "+f.Message)
		return nil
	}
	if g.failingSince.IsZero() {
		g.failingSince = start
	}
	failing := time.Since(g.failingSince)
	r.logger.Info("GetTask failed", "class", int(f.Class), "failing_for", failing, "error", f.Message)
	if failing < r.e.opts.GetTaskGrace {
		g.retryAfter = f.RetryAfter
		return nil
	}
	if f.Class == upstream.Throttled {
		r.fail(store.ReasonUpstreamError, "the service throttled GetTask for "+r.e.opts.GetTaskGrace.String()+": "+f.Message)
	} else {
		r.fail(store.ReasonUpstreamUnreachable, "the service did not answer GetTask for "+r.e.opts.GetTaskGrace.String())
	}
	return nil
}

// reattach follows the recorded upstream task of the attempt without sending anything (D42): GetTask,
// log the difference from the projection, then SubscribeToTask (or polling) until a final state. Only
// GetTask failing to reach the service for GetTaskGrace fails the task upstream_unreachable (§6.4; see
// getTask).
func (r *run) reattach(ctx context.Context) {
	if r.ids.UpstreamTaskID == "" {
		// Nothing upstream to follow: nothing was sent, so the attempt may be retried (§6.4).
		r.retry(store.ReasonUpstreamUnreachable, 0, errors.New("relay: re-attach without an upstream task id"))
		return
	}
	var g getTasks
	for {
		t := r.getTask(ctx, &g)
		if r.finished {
			return
		}
		if t != nil {
			if r.handle(ctx, t) {
				return
			}
			break
		}
		select {
		case <-ctx.Done():
			r.finish(nil, context.Cause(ctx))
			return
		case <-time.After(g.next(r.e.opts.PollInterval, r.e.opts.GetTaskGrace)):
		}
	}
	if r.info.Streaming {
		r.ex.Reset()
		for ev, err := range r.client.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: r.ids.UpstreamTaskID}) {
			if err != nil {
				if ctx.Err() != nil {
					r.finish(nil, context.Cause(ctx))
					return
				}
				break // UnsupportedOperationError (just terminal) or a broken stream: poll (§6.4)
			}
			if r.handle(ctx, ev) {
				return
			}
		}
		if r.finished {
			return
		}
	}
	r.follow(ctx)
}

// handle relays one upstream event; it reports whether the turn ended.
func (r *run) handle(ctx context.Context, ev a2a.Event) bool {
	if r.finished {
		return true
	}
	var upTask a2a.TaskID
	var upCtx string
	isTask := false
	switch e := ev.(type) {
	case *a2a.Message:
		upTask, upCtx = e.TaskID, e.ContextID
	case *a2a.Task:
		upTask, upCtx, isTask = e.ID, e.ContextID, true
	case *a2a.TaskStatusUpdateEvent:
		upTask, upCtx = e.TaskID, e.ContextID
	case *a2a.TaskArtifactUpdateEvent:
		upTask, upCtx = e.TaskID, e.ContextID
	default:
		r.fail(store.ReasonUpstreamError, fmt.Sprintf("the service sent an unexpected event %T", ev))
		return true
	}
	if _, isMsg := ev.(*a2a.Message); !isMsg && upTask == "" {
		r.fail(store.ReasonUpstreamError, "the service sent an event without its task id")
		return true
	}
	if upTask != "" {
		if r.ids.UpstreamTaskID == "" {
			if !r.recordFirst(ctx, upTask, upCtx) {
				return true
			}
		} else if upTask != r.ids.UpstreamTaskID {
			r.fail(store.ReasonUpstreamError, "the service sent an event for another task")
			return true
		}
	}
	gate, err := r.ids.ToGate(ev)
	if err != nil {
		r.fail(store.ReasonUpstreamError, "the service sent an event for another task")
		return true
	}
	gate = wirePrecision(gate)
	// Only a Task that is the attempt's very first upstream event is logged whole; any later one (after
	// events, from polling, after a re-attach) logs its difference from the projection.
	first := isTask && r.firstTask
	r.firstTask = false
	evs, err := Translate(gate, r.proj, first)
	if err != nil {
		r.fail(store.ReasonUpstreamError, "the service sent a malformed event: "+err.Error())
		return true
	}
	// A later Task at the turn boundary the projection already shows (the same final state, status message
	// and timestamp). On a re-attach that is the boundary this turn reached before its context was lost:
	// nothing new to log, and the turn ends (endLogged, below). On a turn's own dispatch it can only be the
	// boundary the previous turn ended at, input-required: the upstream answered a follow-up's
	// SendMessage{returnImmediately} (or opened its stream) with its task as it still is, which A2A 1.0.1
	// allows ("an in-progress state (e.g., TASK_STATE_WORKING, TASK_STATE_INPUT_REQUIRED)"). That is not this
	// turn's end: the dispatch logs any artifact difference, keeps following the upstream, and does not
	// repeat the status (a status event at input-required would end the turn in a2a-go's processor).
	t, _ := gate.(*a2a.Task)
	unchanged := t != nil && !first && a2aevent.IsFinal(t) && !statusChanged(r.proj, t)
	stale := unchanged && !r.d.resume && t.Status.State == a2a.TaskStateInputRequired
	if stale {
		evs = slices.DeleteFunc(evs, func(e a2a.Event) bool { _, st := e.(*a2a.TaskStatusUpdateEvent); return st })
	}
	for _, out := range evs {
		if msg, ok := r.overLimit(out); ok {
			r.fail(store.ReasonUpstreamError, msg)
			return true
		}
		if r.emit(out) {
			return true
		}
	}
	if unchanged && !stale {
		r.endLogged()
		return true
	}
	return false
}

// statusChanged is a2aevent's own test of a changed status, which Recover applies but does not export: the
// state, the presence and id of the status message, or the timestamp differ.
func statusChanged(prev, curr *a2a.Task) bool {
	s1, s2 := prev.Status, curr.Status
	if s1.State != s2.State || (s1.Message == nil) != (s2.Message == nil) || s1.Message != nil && s1.Message.ID != s2.Message.ID {
		return true
	}
	if s1.Timestamp == nil || s2.Timestamp == nil {
		return s1.Timestamp != s2.Timestamp
	}
	return !s1.Timestamp.Equal(*s2.Timestamp)
}

// wirePrecision truncates the status timestamp of an upstream Task or status event to the milliseconds the
// A2A wire, and so the log, keeps (a2a.TaskStatus writes .000Z). An upstream writing finer timestamps (the
// Python SDK's isoformat() writes microseconds) would otherwise never equal the projection read back from
// the log: a re-attach would log an unchanged status again (§6.3: nothing already in the log is logged
// twice), and a follow-up's unchanged task would look changed. ev is not modified.
func wirePrecision(ev a2a.Event) a2a.Event {
	trunc := func(ts *time.Time) *time.Time {
		t := ts.UTC().Truncate(time.Millisecond)
		return &t
	}
	switch e := ev.(type) {
	case *a2a.Task:
		if e.Status.Timestamp != nil {
			cp := *e
			cp.Status.Timestamp = trunc(e.Status.Timestamp)
			return &cp
		}
	case *a2a.TaskStatusUpdateEvent:
		if e.Status.Timestamp != nil {
			cp := *e
			cp.Status.Timestamp = trunc(e.Status.Timestamp)
			return &cp
		}
	}
	return ev
}

// errTurnLogged ends an execution whose final state is already in the log: a2a-go's consumer needs a final
// event or an error to stop, and the SetTaskFailed that follows is refused by the store (D8).
var errTurnLogged = errors.New("relay: the turn's final state is already in the log")

// endLogged ends the turn at the projection's state, terminal or input-required, without an event.
func (r *run) endLogged() {
	switch st := r.proj.Status.State; {
	case st == a2a.TaskStateInputRequired:
		r.d.running.SetOutcome(lifecycle.DispatchInterrupted, "")
	case st.Terminal():
		reason, _ := r.proj.Metadata[store.MetaFailure].(string)
		r.d.running.SetOutcome(lifecycle.DispatchTerminal, store.FailureReason(reason))
	}
	r.finish(nil, errTurnLogged)
}

// recordFirst records the attempt's first upstream task id and context id before the event that carried
// them is logged (spec §4), and signals kontra.dispatched. It reports whether the turn may continue.
func (r *run) recordFirst(ctx context.Context, upTask a2a.TaskID, upCtx string) bool {
	err := r.e.log.SetFirstUpstreamID(ctx, r.taskUUID, r.d.n, string(upTask), upCtx)
	switch {
	case errors.Is(err, store.ErrUpstreamIDMismatch):
		r.fail(store.ReasonUpstreamError, "the service sent an event for another task")
		return false
	case err != nil:
		// Not recorded, so not logged either: the workflow sees no first upstream id. The upstream task may
		// exist; a retry would send the message again, which §6.4 forbids only once the id is recorded.
		r.d.running.SetOutcome(lifecycle.DispatchContextLost, "")
		r.finish(nil, fmt.Errorf("relay: record the first upstream task id: %w", err))
		return false
	}
	r.ids.UpstreamTaskID, r.ids.UpstreamContextID = upTask, upCtx
	if r.e.signals != nil {
		if err := r.e.signals.SignalDispatched(ctx, r.ec.TaskID, r.d.n); err != nil {
			r.logger.Warn("kontra.dispatched signal failed", "error", err)
		}
	}
	return true
}

// overLimit checks the parts an event would store against §6.6 and §10.4: one part over MaxPart, or the
// task's parts over TaskBytes, fails the task upstream_error.
func (r *run) overLimit(ev a2a.Event) (string, bool) {
	var ps a2a.ContentParts
	switch e := ev.(type) {
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact != nil {
			ps = e.Artifact.Parts
		}
	case *a2a.TaskStatusUpdateEvent:
		if e.Status.Message != nil {
			ps = e.Status.Message.Parts
		}
	}
	var n int64
	for _, p := range ps {
		s := partSize(p)
		if s > r.e.opts.MaxPart {
			return fmt.Sprintf("the service sent a part of %d bytes; parts over %d bytes need a bucket (blobs)", s, r.e.opts.MaxPart), true
		}
		n += s
	}
	if r.bytes+n > r.e.opts.TaskBytes {
		return fmt.Sprintf("the task's parts exceed limits.task_bytes (%d bytes)", r.e.opts.TaskBytes), true
	}
	r.bytes += n
	return "", false
}

// partSize is a part's decoded size: text and URL bytes, raw bytes before base64, data as JSON.
func partSize(p *a2a.Part) int64 {
	if p == nil {
		return 0
	}
	switch c := p.Content.(type) {
	case a2a.Text:
		return int64(len(c))
	case a2a.Raw:
		return int64(len(c))
	case a2a.URL:
		return int64(len(c))
	case a2a.Data:
		b, _ := json.Marshal(c.Value)
		return int64(len(b))
	}
	return 0
}

func taskBytes(t *a2a.Task) int64 {
	var n int64
	for _, a := range t.Artifacts {
		if a != nil {
			for _, p := range a.Parts {
				n += partSize(p)
			}
		}
	}
	for _, m := range t.History {
		if m != nil {
			for _, p := range m.Parts {
				n += partSize(p)
			}
		}
	}
	if m := t.Status.Message; m != nil {
		for _, p := range m.Parts {
			n += partSize(p)
		}
	}
	return n
}
