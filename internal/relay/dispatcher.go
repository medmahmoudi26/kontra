package relay

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Dispatcher implements lifecycle.DispatchActivities on this replica.
type Dispatcher struct {
	ReplicaID string
	Log       *store.Log
	Registry  *Registry
	Handlers  *Handlers
	Backends  Backends
	Signals   interface {
		SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error
	}
}

var _ lifecycle.DispatchActivities = (*Dispatcher)(nil)

// Dispatch implements lifecycle.DispatchActivities: it writes attempts.replica_id first, rebuilds the
// a2a-go payload from the work item (lifecycle.Rebuild), registers the dispatch, attaches the
// Heartbeater, calls the service's HandlerFn with the activity context, discards its result and returns
// the registry's outcome. With Resume, the executor re-attaches to the recorded upstream task: GetTask,
// log the difference, then SubscribeToTask (or polling) until terminal or input-required; only GetTask
// failing for 30 s fails the task upstream_unreachable (spec §6.4, D42).
func (d *Dispatcher) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	return lifecycle.DispatchResult{}, unimpl.Error("relay.Dispatcher.Dispatch", "WP-04")
}

// Abort implements lifecycle.DispatchActivities.
func (d *Dispatcher) Abort(ctx context.Context, in lifecycle.AttemptRef) error {
	return unimpl.Error("relay.Dispatcher.Abort", "WP-04")
}

// ForwardCancel implements lifecycle.DispatchActivities: CancelTask upstream on the attempt's upstream
// task, returning the upstream's answer (a canceled status is logged by the running dispatch, or by the
// workflow through write_terminal for an input-required task).
func (d *Dispatcher) ForwardCancel(ctx context.Context, in lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	return lifecycle.ForwardCancelResult{}, unimpl.Error("relay.Dispatcher.ForwardCancel", "WP-04")
}
