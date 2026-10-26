package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"kontra.sh/kontra/internal/hooks"
)

// Worker defaults (spec §14.2, §14.3, §17).
const (
	DefaultDispatchSlots     = 10000
	DefaultPushSlots         = 2000
	DefaultHeartbeatThrottle = 10 * time.Second
	DefaultDrainTimeout      = 10 * time.Minute
	// StopTimeout is the WorkerStopTimeout of every worker but kontra-direct: they stop only after the
	// dispatches have ended, and then within a few seconds (spec §14.3, D76). kontra-tunnel-<replica>'s too:
	// the drain waits for its dispatches itself while it keeps polling (Drain).
	StopTimeout = 5 * time.Second
	// drainPoll is how often a drain looks at the tunnel dispatches in flight.
	drainPoll = 100 * time.Millisecond
	// DefaultSettle is how long a drain keeps the other workers polling after kontra-direct has stopped
	// (Drain): the completion of the last dispatches schedules workflow tasks, which record their results
	// and answer the Updates waiting on them (a cancel, D79).
	DefaultSettle = 2 * time.Second
)

// WorkerConfig sizes one replica's workers (spec §14.2).
type WorkerConfig struct {
	ReplicaID         string
	DispatchSlots     int           // MaxConcurrentActivityExecutionSize of dispatch workers; default 10,000
	PushSlots         int           // kontra-push; default 2,000
	HeartbeatThrottle time.Duration // MaxHeartbeatThrottleInterval of dispatch workers: 10 s
	DrainTimeout      time.Duration // kontra-direct's WorkerStopTimeout: in-flight dispatches finish within it
	// RemoteHooks is hooks.lifecycle set (spec §13.3): the operator's workers serve the five Lifecycle
	// activities on its own queue, and the built-in no-ops are not registered on kontra-lifecycle.
	RemoteHooks bool
	// Settle is how long Drain keeps kontra-lifecycle, kontra-push and the control queues polling once
	// kontra-direct has stopped; default DefaultSettle.
	Settle time.Duration
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.DispatchSlots <= 0 {
		c.DispatchSlots = DefaultDispatchSlots
	}
	if c.PushSlots <= 0 {
		c.PushSlots = DefaultPushSlots
	}
	if c.HeartbeatThrottle <= 0 {
		c.HeartbeatThrottle = DefaultHeartbeatThrottle
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.Settle <= 0 {
		c.Settle = DefaultSettle
	}
	return c
}

// Activities is everything the replica's workers register.
type Activities struct {
	Lifecycle *LifecycleActivities
	Dispatch  DispatchActivities
	Push      PushActivities         // nil: no kontra-push worker (push_webhook is never served here)
	Hooks     hooks.BuiltinLifecycle // registered by type name on kontra-lifecycle unless hooks are remote
}

// Workers are one replica's Temporal workers:
//
//   - kontra-lifecycle: TaskLifecycle and the lifecycle-queue activities, and the built-in hooks;
//   - kontra-direct: dispatch for direct backends (activity-only, explicit slots, 10 s heartbeat throttle);
//   - kontra-direct-ctl: forward_cancel for direct tasks with no dispatch running, and the fallback when a
//     dead replica's control queue does not start it (D79);
//   - kontra-ctl-<replica>: abort and forward_cancel for this replica's dispatches, its own slots;
//   - kontra-push: push_webhook, its own slots;
//   - kontra-tunnel-<replica>: dispatch for the attempts placed on the tunnel sessions this replica holds
//     (spec §6.2, §14.2): activity-only, limits.dispatch_slots, the 10 s heartbeat throttle; only this
//     replica polls it.
type Workers struct {
	lifecycle, direct, directCtl, ctl, push, tunnel worker.Worker
	drainTimeout, settle                            time.Duration
	tunnelInflight                                  atomic.Int64        // dispatches running on kontra-tunnel-<replica>
	now                                             func() time.Time    // tests
	sleep                                           func(time.Duration) // tests
}

// NewWorkers builds the workers; Start begins polling.
func NewWorkers(c client.Client, cfg WorkerConfig, acts Activities) (*Workers, error) {
	if c == nil {
		return nil, errors.New("lifecycle: workers need a Temporal client")
	}
	if cfg.ReplicaID == "" {
		return nil, errors.New("lifecycle: workers need the replica id")
	}
	if acts.Lifecycle == nil || acts.Dispatch == nil {
		return nil, errors.New("lifecycle: workers need the lifecycle and dispatch activities")
	}
	cfg = cfg.withDefaults()
	w := &Workers{drainTimeout: cfg.DrainTimeout, settle: cfg.Settle}

	w.lifecycle = worker.New(c, QueueLifecycle, worker.Options{WorkerStopTimeout: StopTimeout})
	w.lifecycle.RegisterWorkflowWithOptions(TaskLifecycle, workflow.RegisterOptions{Name: WorkflowType})
	la := acts.Lifecycle
	for name, fn := range map[string]any{
		ActPrepare: la.Prepare, ActAttemptState: la.AttemptState, ActListPushConfigs: la.ListPushConfigs,
		ActWriteTerminal: la.WriteTerminal, ActApplyMetadata: la.ApplyMetadata, ActTaskRef: la.TaskRef,
		ActSubmit: la.Submit, ActAppendReviewMessage: la.AppendReviewMessage,
	} {
		w.lifecycle.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	if !cfg.RemoteHooks {
		RegisterBuiltinHooks(w.lifecycle, acts.Hooks)
	}

	// Dispatch: load never looks like a dead replica (explicit slots), and a cancellation reaches a
	// dispatch within 10 s without worker commands (D44). Stopping it lets in-flight dispatches finish
	// within the drain timeout (spec §14.3, D76).
	w.direct = worker.New(c, QueueDirect, worker.Options{
		DisableWorkflowWorker:              true,
		MaxConcurrentActivityExecutionSize: cfg.DispatchSlots,
		MaxHeartbeatThrottleInterval:       cfg.HeartbeatThrottle,
		WorkerStopTimeout:                  cfg.DrainTimeout,
	})
	w.direct.RegisterActivityWithOptions(acts.Dispatch.Dispatch, activity.RegisterOptions{Name: ActDispatch})

	// The tunnel sessions' dispatches (spec §14.2): the same settings as kontra-direct, but this replica's
	// own queue. It keeps polling through a drain until its last dispatch has ended or drain_timeout passes
	// (spec §14.3, D76: a draining replica is no candidate for new attempts, §6.1, while follow-ups of the
	// tasks on its sessions still reach it); Drain waits for its dispatches itself, so its stop is a few
	// seconds, as the spec's "WorkerStopTimeout a few seconds" for the per-replica queues says.
	w.tunnel = worker.New(c, QueueTunnel(cfg.ReplicaID), worker.Options{
		DisableWorkflowWorker:              true,
		MaxConcurrentActivityExecutionSize: cfg.DispatchSlots,
		MaxHeartbeatThrottleInterval:       cfg.HeartbeatThrottle,
		WorkerStopTimeout:                  StopTimeout,
	})
	dispatch := acts.Dispatch.Dispatch
	w.tunnel.RegisterActivityWithOptions(func(ctx context.Context, in DispatchInput) (DispatchResult, error) {
		w.tunnelInflight.Add(1)
		defer w.tunnelInflight.Add(-1)
		return dispatch(ctx, in)
	}, activity.RegisterOptions{Name: ActDispatch})

	w.directCtl = worker.New(c, QueueDirectCtl, worker.Options{DisableWorkflowWorker: true, WorkerStopTimeout: StopTimeout})
	w.directCtl.RegisterActivityWithOptions(acts.Dispatch.ForwardCancel, activity.RegisterOptions{Name: ActForwardCancel})

	// The control queue has its own slots, so abort and forward_cancel are never queued behind dispatches.
	w.ctl = worker.New(c, QueueCtl(cfg.ReplicaID), worker.Options{DisableWorkflowWorker: true, WorkerStopTimeout: StopTimeout})
	w.ctl.RegisterActivityWithOptions(acts.Dispatch.Abort, activity.RegisterOptions{Name: ActAbort})
	w.ctl.RegisterActivityWithOptions(acts.Dispatch.ForwardCancel, activity.RegisterOptions{Name: ActForwardCancel})

	if acts.Push != nil {
		w.push = worker.New(c, QueuePush, worker.Options{
			DisableWorkflowWorker: true, MaxConcurrentActivityExecutionSize: cfg.PushSlots, WorkerStopTimeout: StopTimeout,
		})
		w.push.RegisterActivityWithOptions(acts.Push.PushWebhook, activity.RegisterOptions{Name: ActPushWebhook})
	}
	return w, nil
}

// HookRegistry is what the built-in hooks register on: a worker, or a test environment.
type HookRegistry interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// RegisterBuiltinHooks registers open source's no-op Lifecycle under the type names a remote operator
// serves (spec §13.3).
func RegisterBuiltinHooks(r HookRegistry, h hooks.BuiltinLifecycle) {
	r.RegisterActivityWithOptions(h.OnDispatched, activity.RegisterOptions{Name: HookOnDispatched})
	r.RegisterActivityWithOptions(h.OnTerminal, activity.RegisterOptions{Name: HookOnTerminal})
	r.RegisterActivityWithOptions(h.ReviewWindow, activity.RegisterOptions{Name: HookReviewWindow})
	r.RegisterActivityWithOptions(h.OnReviewMessage, activity.RegisterOptions{Name: HookOnReviewMessage})
	r.RegisterActivityWithOptions(h.OnReviewClosed, activity.RegisterOptions{Name: HookOnReviewClosed})
}

func (w *Workers) all() []worker.Worker {
	out := []worker.Worker{w.lifecycle, w.ctl, w.directCtl, w.direct}
	if w.tunnel != nil {
		out = append(out, w.tunnel)
	}
	if w.push != nil {
		out = append(out, w.push)
	}
	return out
}

// Start starts every worker: the control queues first, so abort and forward_cancel are served before
// this replica takes a dispatch. A failure stops the ones already started.
func (w *Workers) Start() error {
	var started []worker.Worker
	for _, x := range w.all() {
		if err := x.Start(); err != nil {
			for i := len(started) - 1; i >= 0; i-- {
				started[i].Stop()
			}
			return fmt.Errorf("lifecycle: start worker: %w", err)
		}
		started = append(started, x)
	}
	return nil
}

// ErrDrainTimeout is Drain's error when dispatches were still running at drain_timeout: the worker stop
// canceled their contexts, and each is a lost dispatch the workflow re-attaches on another replica (a
// direct backend, D42) after its heartbeat timeout (spec §14.3).
var ErrDrainTimeout = errors.New("lifecycle: drain_timeout passed with dispatches still running")

// Drain drains the workers and returns once all of them have stopped (spec §14.3, D76). kontra-direct
// stops polling at once (its WorkerStopTimeout is drain_timeout): no new dispatch starts here, and the
// dispatches already running keep running and heartbeating, because the SDK keeps sending heartbeats while
// a worker stops. Meanwhile kontra-lifecycle, kontra-push, kontra-direct-ctl, kontra-ctl-<replica> and
// kontra-tunnel-<replica> keep polling, so that on a gate of one replica Updates such as a cancel are still
// accepted, abort and forward_cancel reach this replica's dispatches, and a follow-up of a task on one of
// this replica's tunnel sessions is still dispatched (§6.1 places no new attempt here). The SDK's stop of
// kontra-direct returns when its last dispatch has ended (its result reported), or at drain_timeout, when it
// cancels the ones still running; then the drain waits, within the same drain_timeout, until no dispatch
// runs on kontra-tunnel-<replica>. The other workers then keep polling for Settle more: a dispatch's
// completion schedules a workflow task, and on a gate of one replica only this replica can run it, record
// the result and answer the Update that waits on it (a cancel completes once canceled is logged, D79). Then
// they stop, each within StopTimeout (kontra-tunnel's cancels a dispatch still running at drain_timeout: a
// lost session, §14.3), the control queue last. Drain returns ErrDrainTimeout when drain_timeout cut
// dispatches. It is Stop with the settle and that report; calling either again is a no-op.
func (w *Workers) Drain() error {
	return w.drain(w.settle)
}

// Stop stops every worker in the drain order (spec §14.3, D76): kontra-direct first, which stops polling at
// once and waits up to its WorkerStopTimeout (the drain timeout) for the dispatches in flight, while
// kontra-lifecycle, kontra-push, kontra-direct-ctl, kontra-ctl-<replica> and kontra-tunnel-<replica> keep
// polling, so that on a gate of one replica Updates such as a cancel are still accepted, finished
// dispatches' results recorded, and abort and forward_cancel served; then, once no tunnel dispatch runs
// (within the same drain timeout), those, with a short WorkerStopTimeout, the control queue last.
func (w *Workers) Stop() {
	_ = w.drain(0)
}

func (w *Workers) drain(settle time.Duration) error {
	now, sleep := w.now, w.sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	start := now()
	if w.direct != nil {
		w.direct.Stop()
	}
	timedOut := w.drainTimeout > 0 && now().Sub(start) >= w.drainTimeout
	if !w.waitTunnel(start, now, sleep) {
		timedOut = true
	}
	if settle > 0 {
		sleep(settle)
	}
	w.stopAfterDirect()
	if timedOut {
		return fmt.Errorf("%w (%v)", ErrDrainTimeout, w.drainTimeout)
	}
	return nil
}

// waitTunnel waits, while kontra-tunnel-<replica> keeps polling, until no dispatch runs on it or
// drain_timeout has passed since start; false when dispatches were still running then.
func (w *Workers) waitTunnel(start time.Time, now func() time.Time, sleep func(time.Duration)) bool {
	for w.tunnelInflight.Load() > 0 {
		if w.drainTimeout > 0 && now().Sub(start) >= w.drainTimeout {
			return false
		}
		sleep(drainPoll)
	}
	return true
}

// stopAfterDirect stops every worker but kontra-direct, in the drain order: the control queue last.
func (w *Workers) stopAfterDirect() {
	for _, x := range []worker.Worker{w.lifecycle, w.push, w.directCtl, w.tunnel, w.ctl} {
		if x != nil {
			x.Stop()
		}
	}
}
