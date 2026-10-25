package lifecycle

import (
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// WorkerConfig sizes one replica's workers (spec §14.2).
type WorkerConfig struct {
	ReplicaID         string
	DispatchSlots     int           // MaxConcurrentActivityExecutionSize of dispatch workers; default 10,000
	PushSlots         int           // kontra-push; default 2,000
	HeartbeatThrottle time.Duration // MaxHeartbeatThrottleInterval of dispatch workers: 10 s
	DrainTimeout      time.Duration // kontra-direct's WorkerStopTimeout: in-flight dispatches finish within it
}

// Activities is everything the replica's workers register.
type Activities struct {
	Lifecycle *LifecycleActivities
	Dispatch  DispatchActivities
	Push      PushActivities
	Hooks     hooks.BuiltinLifecycle // registered by type name on kontra-lifecycle unless hooks are remote
}

// Workers are one replica's Temporal workers:
//
//   - kontra-lifecycle: TaskLifecycle and the lifecycle-queue activities, and the built-in hooks;
//   - kontra-direct: dispatch for direct backends (activity-only, explicit slots, 10 s heartbeat throttle);
//   - kontra-direct-ctl: forward_cancel for direct tasks with no dispatch running;
//   - kontra-ctl-<replica>: abort and forward_cancel for this replica's dispatches, its own slots;
//   - kontra-push: push_webhook, its own slots;
//   - kontra-tunnel-<replica>: M2.
type Workers struct {
	lifecycle, direct, directCtl, ctl, push worker.Worker
}

// NewWorkers builds the workers; Start begins polling.
func NewWorkers(c client.Client, cfg WorkerConfig, acts Activities) (*Workers, error) {
	return nil, unimpl.Error("lifecycle.NewWorkers", "WP-05")
}

// Start starts every worker.
func (w *Workers) Start() error {
	return unimpl.Error("lifecycle.Workers.Start", "WP-05")
}

// Drain begins a drain (spec §14.3): stop polling kontra-lifecycle, kontra-direct and kontra-push (the
// in-flight direct dispatches keep running up to the drain timeout), and keep kontra-ctl-<replica>
// polling so abort and forward_cancel for this replica's dispatches still run.
func (w *Workers) Drain() error {
	return unimpl.Error("lifecycle.Workers.Drain", "WP-08")
}

// Stop stops every worker, the control worker last.
func (w *Workers) Stop() {
	panic(unimpl.Error("lifecycle.Workers.Stop", "WP-08"))
}
