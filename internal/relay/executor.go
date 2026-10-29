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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/blob"
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
	// TaskBytes caps the bytes stored per task (limits.task_bytes, 256 MiB, §6.6): every logged event's
	// whole encoding, not only its parts (overLimit), and every object the task holds in the bucket (storePart).
	TaskBytes int64
	// MaxPart is the largest part stored inline: without a bucket, an upstream part over it fails the
	// task upstream_error (§10.4).
	MaxPart int64
	// Bucket is the claim-check bucket (blobs:, spec §10.4, D167): an upstream part over MaxPart is stored in it
	// and logged as a url part kontra-blob:<ref>. Nil: no bucket.
	Bucket blob.Store
	// Render renders kontra-blob: parts as presigned GETs in the message a dispatch sends to a direct backend
	// (spec §10.4: "direct backends get presigned URLs, like callers"); a tunnel dispatch keeps the refs.
	Render *blob.Renderer
	// RecordBackoff is the first wait before the attempt's first upstream task id is recorded again after
	// the gate's store failed to record it, doubling to 5 s (recordFirst). 250 ms.
	RecordBackoff time.Duration
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
	if o.RecordBackoff <= 0 {
		o.RecordBackoff = 250 * time.Millisecond
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
//     task exists, the Dispatcher's input says whether this dispatch resumes, and the work item says
//     whether the turn is a follow-up and whether its delivery is recorded;
//   - records a follow-up's delivery (work_items.delivered_at) when the upstream first answers it, with
//     the first event logged from that answer, or on its own when the answer logs nothing; a resume of a
//     follow-up turn without it sends the follow-up again, with the same messageId, and one with it
//     re-attaches without sending; a first turn is never sent again (spec §6.4, D81);
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
	// followUp is a follow-up turn (the work item is not the task's first); workItem is the turn's work
	// item, delivered whether its delivered_at was set when the dispatch loaded it, and since the task's
	// version when it was stored, after which the turn's every event is logged (D81).
	followUp  bool
	workItem  uuid.UUID
	delivered bool
	since     int64
	// session is the attempt's tunnel session, which the executor reads with the attempt: the Dispatcher's
	// lines about the dispatch's end carry it too (obs.KeyTunnelSession).
	session atomic.Pointer[uuid.UUID]
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
	attempt   *store.Attempt // the attempt as the run began: its backend and tunnel session (§4)
	proj      *a2a.Task      // the gate projection as this run's yields leave it
	info      upstream.Info
	client    *a2aclient.Client
	ex        *upstream.Exchange
	firstTask bool  // no upstream event of the attempt has been handled yet (no upstream task id at the start)
	bytes     int64 // the bytes the task stores (overLimit)
	finished  bool  // a final event was yielded, or an error
	logger    *slog.Logger

	sent     bool // this dispatch sent the turn's message upstream
	resent   bool // ...as a resume of a follow-up turn whose delivery was not recorded (D81)
	answered bool // the upstream answered this dispatch's send (a Task, a Message, a stream event)
	emits    int  // events yielded
	// turnOpen is a resume of a follow-up turn that has logged no status of its own (its delivery is
	// recorded, or the upstream refused the resent follow-up): an input-required the projection already
	// shows is the previous turn's end, which the upstream shows until it processes the follow-up, not this
	// turn's (D81).
	turnOpen bool
	// full is the run of exchanges a tunnel's node refused with Kontra-Node: full after the first upstream
	// task id (nodeFull); resendFull asks send to send the turn's message again.
	full       fullRetry
	resendFull bool
	// digests are the SHA-256 of the objects this run stored or read, by ref (sameObjects).
	digests map[blob.Ref][sha256.Size]byte
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
	if r.e.opts.Bucket != nil {
		// The task's objects count too (storePart): the caller's large parts and every earlier turn's.
		objects, err := r.e.log.BlobBytes(ctx, id)
		if err != nil {
			// Nothing was sent: the workflow retries or re-attaches, as for an attempt it could not read.
			d.running.SetOutcome(lifecycle.DispatchContextLost, "")
			r.finish(nil, err)
			return
		}
		r.bytes += objects
	}
	attempt, err := r.e.log.GetAttempt(ctx, id, d.n)
	if err != nil {
		// Nothing was sent: the workflow re-reads the attempt and retries or re-attaches.
		d.running.SetOutcome(lifecycle.DispatchContextLost, "")
		r.finish(nil, err)
		return
	}
	r.attempt = attempt
	if attempt.TunnelSession != nil {
		r.logger = r.logger.With(obs.KeyTunnelSession, attempt.TunnelSession.String())
		d.session.Store(attempt.TunnelSession)
	}
	r.ids = IDs{
		GateTaskID: r.ec.TaskID, GateContextID: r.ec.ContextID,
		UpstreamTaskID: a2a.TaskID(attempt.UpstreamTaskID), UpstreamContextID: attempt.UpstreamContextID,
	}
	r.firstTask = r.ids.UpstreamTaskID == ""
	backend, info, err := r.e.backends.Backend(ctx, d.svc, attempt)
	switch {
	case err == nil:
	case errors.Is(err, upstream.ErrTunnelLost):
		// The attempt's session is not held by this replica, or no longer serves the service (spec §6.1
		// key_scope, §14.2): no stream is opened. Before the first upstream task id the workflow retries on
		// another session; after it (a follow-up turn of a sticky task) the task fails tunnel_lost here (§7).
		r.logger.Info("the attempt's tunnel session is gone; no stream opened", "error", err)
		if r.ids.UpstreamTaskID != "" {
			r.fail(store.ReasonTunnelLost, "the tunnel session to the service was lost")
			return
		}
		d.running.SetOutcome(lifecycle.DispatchTunnelLost, store.ReasonTunnelLost)
		r.finish(nil, err)
		return
	case errors.Is(err, ErrCardPending) && r.ids.UpstreamTaskID == "":
		r.logger.Info("the tunnel service's card has not been fetched yet; the attempt is retried", "error", err)
		r.retry(store.ReasonServiceUnavailable, 0, err)
		return
	default:
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
	// A tunnel stream's preamble names the gate task and context (tunnel.md §8.1).
	ctx = upstream.WithTask(ctx, upstream.ExchangeTask{TaskID: string(r.ec.TaskID), ContextID: r.ec.ContextID})
	if d.resume {
		r.resume(ctx)
		return
	}
	r.send(ctx)
}

// resume runs a resume dispatch of the attempt (spec §6.4, D42, D81). A follow-up turn whose delivery is
// not recorded is sent again, the message as the caller sent it (its messageId kept) with the upstream ids
// substituted; the dispatch then continues as the turn's own, logging only the difference from the
// projection. Any other resume re-attaches without sending: a task's first turn is never sent again (before
// its first upstream id a resume is a retry), and neither is a follow-up the upstream answered. A re-attach
// of a follow-up turn that has logged no status since its work item was stored (TurnStatusLogged) does not
// end the turn at the input-required the projection shows: that is the previous turn's end (turnOpen).
func (r *run) resume(ctx context.Context) {
	d := r.d
	if d.followUp && r.ids.UpstreamTaskID != "" {
		if !d.delivered {
			r.logger.Info("resending a follow-up whose delivery is not recorded (D81)", "work_item", d.workItem)
			r.resent = true
			r.send(ctx)
			return
		}
		if r.proj.Status.State == a2a.TaskStateInputRequired {
			// Only an input-required the projection shows can be mistaken for this turn's end. The turn's
			// rows all come after the version its work item was stored at (no messageId, no payload read).
			logged, err := r.e.log.TurnStatusLogged(ctx, r.taskUUID, d.since)
			if err != nil {
				d.running.SetOutcome(lifecycle.DispatchContextLost, "")
				r.finish(nil, err)
				return
			}
			r.turnOpen = !logged
		}
	}
	r.reattach(ctx)
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
	r.emits++
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

// sealedRejected is a hosting node's refusal of a sealed part (ext-sealed.md §6; spec §6.4's row): an audit row
// names the task, the attempt and the node's reason, and the task fails sealed_rejected, never retried, before or
// after the first upstream task id (the attempt's outcome is sealed_rejected, store.TerminalOutcome). An audit row
// that cannot be written is logged; the failure is the task's all the same.
func (r *run) sealedRejected(ctx context.Context, f upstream.Failure) {
	reason := r.ex.SealedReject()
	fields := audit.Fields{
		"task_id": r.taskUUID, "tenant_id": r.d.svc.TenantID, "service": r.d.svc.Name, "protocol": string(store.ProtocolA2A),
		"attempt": r.d.n, "reason": reason,
	}
	if r.attempt != nil {
		if r.attempt.ReplicaID != "" {
			fields["replica_id"] = r.attempt.ReplicaID
		}
		if r.attempt.TunnelSession != nil {
			fields["tunnel_session"] = r.attempt.TunnelSession.String()
		}
		if r.attempt.KeyID != "" {
			fields["key_id"] = r.attempt.KeyID
		}
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := audit.SealedRejected(actx, r.e.log.DB(), fields); err != nil {
		r.logger.Error("a sealed-part rejection was not written to the audit chain", "error", err)
	}
	r.logger.Warn("the hosting node refused a sealed part: the task fails sealed_rejected (not retried)", "reason", reason)
	r.fail(store.ReasonSealedRejected, f.Message)
}

// failureText is f's message as the gate logs it and stores it in the task's log: for a sealed service, never the
// service's own words (an A2A error's message or details, a malformed body's text), which may quote the sealed input
// it refused (a validation error echoing a card number), but the gate's: ext-sealed.md §7 promises that the gate's
// database and logs hold ciphertext only, and Temporal's history already gets the code only (D59). The hosting node
// replaces a sealed job's error text too (sealing.Hosting.errorResponse); this holds whatever the node does.
func (r *run) failureText(f upstream.Failure) string {
	if r.e.svc.Sealed == "" || f.Class != upstream.Protocol {
		return f.Message
	}
	if status, _ := r.ex.Status(); status >= 300 {
		return fmt.Sprintf("the service answered HTTP %d (its text is not kept for a sealed service)", status)
	}
	return "the service returned an error (its text is not kept for a sealed service)"
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
	f.Message = r.failureText(f)
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
	case upstream.ClassTunnelLost:
		// Spec §6.4's tunnel rows: before the first upstream task id, retry on another session; after it,
		// the upstream task's state is unrecoverable from outside.
		if !first {
			r.retry(store.ReasonTunnelLost, 0, err)
			return
		}
		r.fail(store.ReasonTunnelLost, f.Message)
	case upstream.ClassNodeFull:
		if !first {
			r.retry(store.ReasonTunnelLost, 0, err)
			return
		}
		// The node never forwarded the message (a full answer proves it): send it again (nodeFull).
		r.resendFull = r.nodeFull(ctx, f)
	case upstream.ClassSealedRejected:
		r.sealedRejected(ctx, f)
	default:
		if r.resent && !r.answered {
			r.refusedResend(ctx, f)
			return
		}
		r.fail(store.ReasonUpstreamError, "the service returned an error: "+f.Message)
	}
}

// fullRetry is the run of exchanges a node refused with Kontra-Node: full after the attempt's first
// upstream task id.
type fullRetry struct {
	since time.Time     // the first refusal of the current run
	wait  time.Duration // the next wait: PollInterval doubling to PollMax
}

// nodeFull applies the full row of spec §6.4 after the first upstream task id: the exchange is made again
// after the poll's backoff (1 s doubling to 5 s), for at most GetTaskGrace (30 s), then the task fails
// tunnel_lost. It waits the backoff and reports whether to make the exchange again; false when the turn
// ended (the grace ran out, or the dispatch's context ended).
//
// SPEC-CONFLICT (M2 plan, confirmed conflict on spec §6.4's tunnel row "Kontra-Node … full: the dispatch
// retries the poll with backoff"): after the first upstream id the row names only a poll. A follow-up's
// SendMessage, or a stream, refused with full would read as "any other: failed: tunnel_lost", yet a full
// answer proves the node never forwarded the message, so sending it again cannot deliver it twice. The
// closest faithful behaviour: every exchange refused full after the first upstream id is made again with
// the poll's backoff for at most the 30 s grace of D97, then the task fails tunnel_lost.
func (r *run) nodeFull(ctx context.Context, f upstream.Failure) bool {
	o := r.e.opts
	if r.full.since.IsZero() {
		r.full.since, r.full.wait = time.Now(), o.PollInterval
	}
	if time.Since(r.full.since) >= o.GetTaskGrace {
		r.fail(store.ReasonTunnelLost, "the node hosting the service stayed at its limit of exchanges in flight for "+o.GetTaskGrace.String())
		return false
	}
	wait := r.full.wait
	r.full.wait = min(2*r.full.wait, o.PollMax)
	r.logger.Info("the node is full; the exchange is made again", "wait", wait, "full_for", time.Since(r.full.since), "error", f.Message)
	select {
	case <-ctx.Done():
		r.finish(nil, context.Cause(ctx))
		return false
	case <-time.After(wait):
		return true
	}
}

// send is a turn's first dispatch: the caller's message upstream, streaming first.
func (r *run) send(ctx context.Context) {
	msg, err := r.upstreamMessage(ctx)
	if err == nil && r.attempt.Backend == store.BackendDirect {
		// A direct backend gets presigned GETs for the parts in the bucket, as a caller does; a node keeps
		// kontra-blob:<ref> and fetches each through its own tunnel (spec §10.4, tunnel.md §8.3).
		msg, err = r.e.opts.Render.Message(ctx, r.taskUUID, msg)
	}
	if err != nil {
		r.d.running.SetOutcome(lifecycle.DispatchContextLost, "")
		r.finish(nil, err)
		return
	}
	req := &a2a.SendMessageRequest{Message: msg, Metadata: r.ec.Metadata}
	if q := r.d.request; q != nil && q.Config != nil && len(q.Config.AcceptedOutputModes) > 0 {
		req.Config = &a2a.SendMessageConfig{AcceptedOutputModes: q.Config.AcceptedOutputModes}
	}
	r.sent = true
	streaming := r.info.Streaming
	for {
		r.resendFull = false
		if streaming {
			fallback := r.stream(ctx, req)
			if r.resendFull && !r.finished {
				continue // the node refused the exchange full (nodeFull): send it again
			}
			if !fallback {
				return
			}
			r.logger.Info("the upstream refused streaming; polling (D15)")
			streaming = false
		}
		r.poll(ctx, req)
		if !r.resendFull || r.finished {
			return
		}
	}
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
		prev, err := r.e.log.PreviousContextAttempt(ctx, r.d.svc, r.d.task.Owner, r.d.task.ContextID, r.attempt.Backend, r.attempt.TunnelSession)
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
// same gate context on the same backend as this attempt (spec §4, store.SameBackend): the same tunnel
// session, or the service's current direct backend (store.SameDirectBackend); the rest are dropped.
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
		join kontra.services s on s.tenant_id = t.tenant_id and s.name = t.service
		join lateral (select a.upstream_task_id from kontra.attempts a
		               where a.task_id = t.id and a.upstream_task_id is not null
		                 and `+store.SameBackend("$6", "$7")+`
		               order by a.n desc limit 1) a on true
		where t.id = any($1::uuid[]) and t.tenant_id = $2 and t.service = $3 and t.buyer_id = $4 and t.context_id = $5`,
		want, r.d.svc.TenantID, r.d.svc.Name, r.d.task.Owner.BuyerID, r.d.task.ContextID, string(r.attempt.Backend), r.attempt.TunnelSession)
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
		if r.answer(ctx, ev) {
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
	// SPEC-CONFLICT (D96): §6.2–§6.4 say nothing of a stream the upstream closes cleanly before a terminal or
	// input-required event. The closest rule is the re-attach's ("a subscription that ends without a
	// terminal event falls back to polling", §6.4): the dispatch follows the upstream task by GetTask.
	r.follow(ctx)
	return false
}

// answer relays an event the upstream sent in answer to this dispatch's send. The first answer to a
// follow-up records the follow-up's delivery (spec §6.4, D81) before anything from it is logged: the
// dispatch arms its fence, so the first append from the answer sets work_items.delivered_at in the same
// statement; an answer that logs nothing (the task exactly as the projection shows it) has the delivery
// written on its own. It reports whether the turn ended.
func (r *run) answer(ctx context.Context, ev a2a.Event) bool {
	if r.answered || !r.d.followUp {
		r.answered = true
		return r.handle(ctx, ev)
	}
	r.answered = true
	r.d.fence.ArmDelivery(r.d.workItem)
	emits := r.emits
	ended := r.handle(ctx, ev)
	if r.emits == emits {
		r.markDelivered(ctx)
	}
	return ended || r.finished
}

// markDelivered writes the armed delivery on its own: the answer that armed it logged nothing.
func (r *run) markDelivered(ctx context.Context) {
	id := r.d.fence.DisarmDelivery()
	if id == uuid.Nil {
		return // an append wrote it
	}
	err := r.e.log.MarkDelivered(ctx, r.d.fence, id)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrFenced):
		// A newer dispatch holds the attempt: this one stops, and the Dispatcher reports it superseded.
		r.finish(nil, err)
	default:
		// Not written: the next append of the turn carries it.
		r.d.fence.ArmDelivery(id)
		r.logger.Warn("the follow-up's delivery could not be recorded; the next event records it", "error", err)
	}
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
		if r.answer(ctx, v) {
			return
		}
	case *a2a.Message:
		r.answer(ctx, v)
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
		t := r.getTask(ctx, &g, 0)
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
	fullWait     time.Duration // a tunnel node's full answers: the poll's backoff, PollInterval doubling to PollMax
}

// next is the wait before the next call: the interval, a throttled answer's Retry-After up to the grace,
// or the backoff after a node's full answer.
func (g *getTasks) next(interval, grace time.Duration) time.Duration {
	return max(interval, min(g.retryAfter, grace), g.fullWait)
}

// getTask issues one GetTask (historyLength history, 0 but for refusedResend), bounded by GetTaskTimeout,
// and applies §6.4 to a failure.
// It returns the task, or nil when the call failed: then either the turn ended (r.finished: the dispatch's
// context ended, or the failure failed the task) or the call is to be made again.
//
// SPEC-CONFLICT (D97): §6.4 states the 30 s GetTask rule for the re-attach only, and its table fails a task whose
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
//
// Through a tunnel (§6.4's tunnel rows; GetTask always comes after the first upstream task id): a lost
// session fails the task tunnel_lost at once, a node's full answer is retried with the poll's backoff
// within the same grace, then tunnel_lost (nodeFull), and a node's sealed_rejected fails sealed_rejected.
func (r *run) getTask(ctx context.Context, g *getTasks, history int) *a2a.Task {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, r.e.opts.GetTaskTimeout)
	defer cancel()
	r.ex.Reset()
	t, err := r.client.GetTask(cctx, &a2a.GetTaskRequest{ID: r.ids.UpstreamTaskID, HistoryLength: &history})
	g.retryAfter = 0
	full := g.fullWait
	g.fullWait = 0
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
		f.Message = r.failureText(f)
	}
	switch f.Class {
	case upstream.Unreachable, upstream.Throttled:
	case upstream.ClassNodeFull:
		// Spec §6.4's tunnel row: the dispatch retries the poll with backoff (1 s doubling to 5 s), within
		// the 30 s grace (nodeFull's SPEC-CONFLICT), then tunnel_lost.
		g.fullWait = r.e.opts.PollInterval
		if full > 0 {
			g.fullWait = min(2*full, r.e.opts.PollMax)
		}
	case upstream.ClassTunnelLost:
		r.fail(store.ReasonTunnelLost, f.Message)
		return nil
	case upstream.ClassSealedRejected:
		r.sealedRejected(ctx, f)
		return nil
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
	if f.Class == upstream.ClassNodeFull {
		r.fail(store.ReasonTunnelLost, "the node hosting the service stayed at its limit of exchanges in flight for "+r.e.opts.GetTaskGrace.String())
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
	t := r.awaitTask(ctx)
	if t == nil || r.handle(ctx, t) {
		return
	}
	r.track(ctx)
}

// awaitTask calls GetTask until the upstream answers with its task, under the 30 s rule (getTask). It
// returns nil when the turn ended meanwhile (r.finished).
func (r *run) awaitTask(ctx context.Context) *a2a.Task {
	var g getTasks
	for {
		t := r.getTask(ctx, &g, 0)
		if r.finished {
			return nil
		}
		if t != nil {
			return t
		}
		select {
		case <-ctx.Done():
			r.finish(nil, context.Cause(ctx))
			return nil
		case <-time.After(g.next(r.e.opts.PollInterval, r.e.opts.GetTaskGrace)):
		}
	}
}

// refusedResend handles an error the upstream answered a resume's resent follow-up with, before any answer
// (D81). The resend "then continues as a re-attach" (D81) of a turn that has logged nothing (turnOpen): the
// input-required the projection shows is the previous turn's end, which the upstream shows until it
// processes the follow-up. GetTask, asking for the last message of the upstream's history, tells whether
// the upstream took the earlier send:
//
//   - its task moved on from the projection (another status, or terminal: a task the upstream finished
//     refuses any further message), so it processed the follow-up and only its answer was lost; or
//   - its history ends with the follow-up's messageId: it took the earlier send and works on it without
//     having answered yet (a2a-go refuses the duplicate, "task execution is already in progress", and
//     records nothing of the turn but that history entry until its agent yields a first event).
//
// Either way the dispatch re-attaches: it logs the difference and follows the task to the turn's end. An
// upstream that shows neither within GetTaskGrace (polling every PollInterval) never took the follow-up,
// and the refusal fails the task upstream_error, as §6.4's table says.
//
// SPEC-CONFLICT (D98): §6.4's table fails a task at an upstream protocol error after its first upstream id, while
// D81 resends an undelivered follow-up and "then continues as a re-attach". An upstream that already took
// the follow-up refuses the duplicate: a2a-go while the first copy runs, and any upstream once the task it
// finished is terminal. Failing then would lose the result of a turn the seller is working on or finished,
// which D81 exists to prevent; following an upstream that never took the follow-up would wait on its
// input-required until the task deadline. The closest faithful behaviour: the refusal fails the task only
// when, for GetTaskGrace, the upstream task neither moves from the projection's input-required nor shows
// the follow-up in its history (A2A lets an agent keep no history, so an upstream that keeps none and
// takes longer than GetTaskGrace before its first event is still failed).
func (r *run) refusedResend(ctx context.Context, f upstream.Failure) {
	r.turnOpen = true
	msgID := r.ec.Message.ID // the follow-up's, which the resend carried (D81)
	deadline := time.Now().Add(r.e.opts.GetTaskGrace)
	var g getTasks
	for {
		t := r.getTask(ctx, &g, 1)
		if r.finished {
			return
		}
		if t != nil {
			cur, _ := wirePrecision(t).(*a2a.Task)
			moved := cur.Status.State != a2a.TaskStateInputRequired || statusChanged(r.proj, cur)
			took := slices.ContainsFunc(t.History, func(m *a2a.Message) bool { return m != nil && m.ID == msgID })
			if moved || took {
				r.logger.Info("the upstream refused the resent follow-up but had taken it; re-attaching (D81)",
					"moved", moved, "in_history", took, "error", f.Message)
				cp := *t
				cp.History = nil
				if r.handle(ctx, &cp) {
					return
				}
				r.track(ctx)
				return
			}
			if !time.Now().Before(deadline) {
				r.fail(store.ReasonUpstreamError, "the service returned an error: "+f.Message)
				return
			}
		}
		select {
		case <-ctx.Done():
			r.finish(nil, context.Cause(ctx))
			return
		case <-time.After(g.next(r.e.opts.PollInterval, r.e.opts.GetTaskGrace)):
		}
	}
}

// track follows the upstream task after a re-attach's GetTask: SubscribeToTask when the upstream card
// streams, else polling; a subscription that ends without a final event falls back to polling (§6.4).
func (r *run) track(ctx context.Context) {
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
	if CheckReserved(gate) != nil {
		r.fail(store.ReasonUpstreamError, "the service sent a url part in the gate's reserved kontra-blob: scheme")
		return true
	}
	if CheckSealedForm(gate) != nil {
		// ext-sealed.md §3: a part with the envelope media type is the raw form or nothing.
		r.fail(store.ReasonUpstreamError, "the service sent a part with the sealed envelope media type in a form other than raw")
		return true
	}
	gate = wirePrecision(gate)
	// Only a Task that is the attempt's very first upstream event is logged whole; any later one (after
	// events, from polling, after a re-attach) logs its difference from the projection.
	first := isTask && r.firstTask
	r.firstTask = false
	if t, ok := gate.(*a2a.Task); ok && !first {
		if gate, err = r.sameObjects(ctx, t); err != nil {
			r.d.running.SetOutcome(lifecycle.DispatchContextLost, "")
			r.finish(nil, err)
			return true
		}
	}
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
	// The same holds on a resume of a follow-up turn that has logged no status of its own (turnOpen): the
	// projection's input-required is the previous turn's end, and the upstream shows it until it processes
	// the follow-up (D81).
	stale := unchanged && (r.sent || r.turnOpen) && t.Status.State == a2a.TaskStateInputRequired
	if stale {
		evs = slices.DeleteFunc(evs, func(e a2a.Event) bool { _, st := e.(*a2a.TaskStatusUpdateEvent); return st })
	}
	for _, out := range evs {
		out, err := r.externalize(ctx, out)
		if errors.Is(err, errOverTaskBytes) {
			r.fail(store.ReasonUpstreamError, fmt.Sprintf("the task's stored bytes exceed limits.task_bytes (%d bytes)", r.e.opts.TaskBytes))
			return true
		}
		if err != nil {
			// The bucket refused the object: nothing of the event is logged, and a dispatch that cannot store
			// what the upstream sent hands the turn back like a lost store (D42): before the first upstream
			// task id it is retried, after it a later dispatch re-attaches.
			r.logger.Warn("an upstream part over the inline limit could not be stored in the bucket", "error", err)
			r.d.running.SetOutcome(lifecycle.DispatchContextLost, "")
			r.finish(nil, err)
			return true
		}
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

// maxRecordBackoff caps recordFirst's wait between two attempts to record the first upstream task id.
const maxRecordBackoff = 5 * time.Second

// recordFirst records the attempt's first upstream task id and context id before the event that carried
// them is logged (spec §4), and signals kontra.dispatched. It reports whether the turn may continue. The
// write is fenced (D77): a dispatch the workflow has given up on and retried or re-attached is refused, and
// stops here without logging or signalling anything.
//
// The upstream has answered with its task id, so the upstream task exists. If the gate's store fails to
// record it (a broken connection, the write's statement timeout, a failover), the dispatch still holds the
// id in memory: handing the turn back to the workflow would make it read no first upstream id and retry,
// sending the caller's message upstream a second time (§6.4 allows a retry only before the first upstream
// task id). So while the dispatch lives (a2a-go's heartbeater keeps beating) it records the id again,
// backing off from RecordBackoff to 5 s, until it is recorded or the dispatch ends: an abort, the task's
// deadline, Temporal giving up on the activity. Only then is the turn handed back, which is the case of a
// dispatch that died, where Postgres is the only record. An id the store refuses as data (a NUL character)
// fails the task upstream_error: recording it again would only fail again.
func (r *run) recordFirst(ctx context.Context, upTask a2a.TaskID, upCtx string) bool {
	wait := r.e.opts.RecordBackoff
	for {
		err := r.e.log.SetFirstUpstreamID(ctx, r.d.fence, string(upTask), upCtx)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrFenced), errors.Is(err, store.ErrAttemptNotFound):
			r.logger.Info("the first upstream task id was refused: a newer dispatch or attempt holds the task", "error", err)
			r.finish(nil, err)
			return false
		case errors.Is(err, store.ErrUpstreamIDMismatch):
			r.fail(store.ReasonUpstreamError, "the service sent an event for another task")
			return false
		case !storeFailure(err):
			r.logger.Warn("the first upstream task id cannot be recorded", "error", err)
			r.fail(store.ReasonUpstreamError, "the service sent a task id the gate cannot record")
			return false
		default:
			r.logger.Warn("the first upstream task id could not be recorded; recording it again", "wait", wait, "error", err)
			select {
			case <-ctx.Done():
				// The dispatch ends with the id unrecorded: an abort (§8), or a dispatch Temporal no longer
				// waits for, which the workflow treats as lost.
				r.finish(nil, fmt.Errorf("relay: record the first upstream task id: %w (%w)", context.Cause(ctx), err))
				return false
			case <-time.After(wait):
			}
			wait = min(2*wait, maxRecordBackoff)
			continue
		}
		break
	}
	r.ids.UpstreamTaskID, r.ids.UpstreamContextID = upTask, upCtx
	if r.e.signals != nil {
		if err := r.e.signals.SignalDispatched(ctx, r.ec.TaskID, r.d.n); err != nil {
			r.logger.Warn("kontra.dispatched signal failed", "error", err)
		}
	}
	return true
}

// overLimit checks what an event would store against §6.6 and §10.4: one part over MaxPart (left by externalize
// only without a bucket), or the task's stored bytes over TaskBytes, fails the task upstream_error. The bytes
// counted here are the logged event's, where a part in the bucket is its url part; its object was counted when
// it was stored (storePart).
//
// SPEC-CONFLICT (D103): §6.6's table names limits.task_bytes as "Parts stored per task", while its rule is that
// "nothing an upstream sends is unbounded", and task_bytes is the only bound on what one task accumulates.
// Counting parts only left the rest of every event unbounded: the metadata of a task, status, artifact,
// message or part, a part's filename and mediaType, an artifact's name and description, and status events
// with no message at all, each event up to 4 MiB (an SSE event) or 8 MiB (a polled body), every one logged
// and rewritten into the projection until the deadline. The closest faithful behaviour: task_bytes bounds
// what the task stores, so each event counts its whole encoding (the bytes the log stores for it), and a
// dispatch starts from what the projection already holds (taskBytes).
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
	for _, p := range ps {
		if s := partSize(p); s > r.e.opts.MaxPart {
			return fmt.Sprintf("the service sent a part of %d bytes; parts over %d bytes need a bucket (blobs)", s, r.e.opts.MaxPart), true
		}
	}
	n := encodedSize(ev)
	if r.bytes+n > r.e.opts.TaskBytes {
		return fmt.Sprintf("the task's stored bytes exceed limits.task_bytes (%d bytes)", r.e.opts.TaskBytes), true
	}
	r.bytes += n
	return "", false
}

// encodedSize is the length of v's JSON encoding: what the log and the projection store for it.
func encodedSize(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// partSize is a part's decoded size: text and URL bytes, raw bytes before base64, data as JSON (blob.PartSize).
func partSize(p *a2a.Part) int64 { return blob.PartSize(p) }

// externalize stores every part of ev over MaxPart in the bucket and returns ev with each replaced by its url
// part kontra-blob:<ref> (spec §10.4, D167): the logged event, which overLimit then counts; each object counts
// toward limits.task_bytes as it is stored (storePart), and one that would cross it is errOverTaskBytes. The
// objects' rows are pending on the dispatch's context and written by the append that logs ev (blob.Pending,
// store.Log.Append). Without a bucket ev is returned as it is, and overLimit fails the task. ev is not modified.
func (r *run) externalize(ctx context.Context, ev a2a.Event) (a2a.Event, error) {
	if r.e.opts.Bucket == nil {
		return ev, nil
	}
	// parts returns ps with each part over MaxPart stored, or nil when none is.
	parts := func(ps a2a.ContentParts) (a2a.ContentParts, error) {
		var out a2a.ContentParts
		for i, p := range ps {
			if partSize(p) <= r.e.opts.MaxPart {
				continue
			}
			np, err := r.storePart(ctx, p)
			if err != nil {
				return nil, err
			}
			if out == nil {
				out = append(a2a.ContentParts(nil), ps...)
			}
			out[i] = np
		}
		return out, nil
	}
	switch e := ev.(type) {
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact == nil {
			return ev, nil
		}
		ps, err := parts(e.Artifact.Parts)
		if err != nil || ps == nil {
			return ev, err
		}
		cp, art := *e, *e.Artifact
		art.Parts = ps
		cp.Artifact = &art
		return &cp, nil
	case *a2a.TaskStatusUpdateEvent:
		if e.Status.Message == nil {
			return ev, nil
		}
		ps, err := parts(e.Status.Message.Parts)
		if err != nil || ps == nil {
			return ev, err
		}
		cp, msg := *e, *e.Status.Message
		msg.Parts = ps
		cp.Status.Message = &msg
		return &cp, nil
	}
	return ev, nil
}

// errOverTaskBytes is an upstream part whose object would take the task's stored bytes over limits.task_bytes:
// the task fails upstream_error, and the object is never written.
var errOverTaskBytes = errors.New("relay: the task's stored bytes would exceed limits.task_bytes")

// storePart stores one part's content as an object of this task and returns its url part. The object counts
// toward limits.task_bytes, checked before it is written: one that would cross it is errOverTaskBytes.
//
// SPEC-CONFLICT (D194; §6.6 with §10.4): §6.6's table counts in limits.task_bytes "each logged event's whole
// encoding" (D103), and the M3 plan counted a part moved to the bucket only as its logged url part. But §6.6's rule
// is that "nothing an upstream sends is unbounded", and §10.4 says of the bucket "the gate stores bytes it does not
// interpret; per-task totals are bounded (§6.6)". Counting the url part only bounded a task's objects in number
// (task_bytes over a url part's few hundred bytes), each up to an SSE event's 4 MiB: thousands of times
// task_bytes in bytes. The closest faithful behaviour: task_bytes bounds everything the gate stores
// for the task, each logged event's whole encoding plus each object in the bucket (blobs.size); a dispatch starts
// from the projection and the task's blobs rows, the caller's objects included, as D103 counts the caller's
// history.
func (r *run) storePart(ctx context.Context, p *a2a.Part) (*a2a.Part, error) {
	size := partSize(p)
	if size > blob.MaxObject {
		return nil, fmt.Errorf("relay: an upstream part of %d bytes is over the bucket's %d", size, blob.MaxObject)
	}
	if r.bytes+size > r.e.opts.TaskBytes {
		return nil, errOverTaskBytes
	}
	np, row, err := blob.Externalize(ctx, r.e.opts.Bucket, r.d.task.TenantID, r.taskUUID, p)
	if err != nil {
		return nil, err
	}
	r.bytes += row.Size
	if pending := blob.PendingFrom(ctx); pending != nil {
		pending.Add(row)
	} else {
		return nil, errors.New("relay: a dispatch without a pending set cannot record its objects")
	}
	if b, _, err := blob.Content(p); err == nil {
		r.digest(row.Ref, sha256.Sum256(b))
	}
	return np, nil
}

func (r *run) digest(ref blob.Ref, sum [sha256.Size]byte) {
	if r.digests == nil {
		r.digests = map[blob.Ref][sha256.Size]byte{}
	}
	r.digests[ref] = sum
}

// sameObjects returns a later upstream Task (polled, or the first after a re-attach) with each artifact part
// that the projection already holds as an object of this task replaced by the projection's url part, when the
// part's content is that object's (the same bytes, by SHA-256) and its media type, filename and metadata are
// what the logged part kept. Translate then logs only what changed (a2aevent.Recover compares parts whole):
// without it, every poll of a task with a large artifact would store it and log it again. A ref's digest is the
// one this run computed when it stored the object, or that of the object read back from the bucket once. t is
// not modified.
func (r *run) sameObjects(ctx context.Context, t *a2a.Task) (*a2a.Task, error) {
	if r.e.opts.Bucket == nil || r.proj == nil || len(t.Artifacts) == 0 {
		return t, nil
	}
	var out *a2a.Task
	for ai, a := range t.Artifacts {
		if a == nil {
			continue
		}
		pi := slices.IndexFunc(r.proj.Artifacts, func(p *a2a.Artifact) bool { return p != nil && p.ID == a.ID })
		if pi < 0 {
			continue
		}
		prev := r.proj.Artifacts[pi].Parts
		var parts a2a.ContentParts
		for i, p := range a.Parts {
			if i >= len(prev) || prev[i] == nil || p == nil || partSize(p) <= r.e.opts.MaxPart {
				continue
			}
			u, ok := prev[i].Content.(a2a.URL)
			if !ok {
				continue
			}
			ref, ok := blob.RefOf(string(u))
			if !ok {
				continue
			}
			b, mt, err := blob.Content(p)
			if err != nil {
				continue
			}
			want := &a2a.Part{Content: u, Filename: p.Filename, MediaType: mt, Metadata: p.Metadata}
			if !reflect.DeepEqual(want, prev[i]) {
				continue
			}
			sum, err := r.objectDigest(ctx, ref)
			if err != nil {
				return nil, err
			}
			if sum != sha256.Sum256(b) {
				continue
			}
			if parts == nil {
				parts = append(a2a.ContentParts(nil), a.Parts...)
			}
			parts[i] = prev[i]
		}
		if parts == nil {
			continue
		}
		if out == nil {
			cp := *t
			cp.Artifacts = append([]*a2a.Artifact(nil), t.Artifacts...)
			out = &cp
		}
		art := *a
		art.Parts = parts
		out.Artifacts[ai] = &art
	}
	if out == nil {
		return t, nil
	}
	return out, nil
}

// objectDigest is the SHA-256 of one of this task's objects: remembered, or read back from the bucket.
func (r *run) objectDigest(ctx context.Context, ref blob.Ref) ([sha256.Size]byte, error) {
	if sum, ok := r.digests[ref]; ok {
		return sum, nil
	}
	rc, _, err := r.e.opts.Bucket.Open(ctx, blob.Key{Tenant: r.d.task.TenantID, Task: r.taskUUID, Ref: ref})
	if errors.Is(err, blob.ErrNotFound) {
		return [sha256.Size]byte{}, nil // gone: never the same content
	}
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("relay: read object %s back: %w", ref, err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(rc, blob.MaxObject)); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("relay: read object %s back: %w", ref, err)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	r.digest(ref, sum)
	return sum, nil
}

// taskBytes is what the projection already stores: its status, metadata, artifacts and history, each
// encoded on its own (never the whole task at once, which may be limits.task_bytes large).
func taskBytes(t *a2a.Task) int64 {
	n := encodedSize(t.Status)
	if len(t.Metadata) > 0 {
		n += encodedSize(t.Metadata)
	}
	for _, a := range t.Artifacts {
		if a != nil {
			n += encodedSize(a)
		}
	}
	for _, m := range t.History {
		if m != nil {
			n += encodedSize(m)
		}
	}
	return n
}
