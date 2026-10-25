package lifecycle

import (
	"context"

	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// DispatchActivities run on the dispatching replica (kontra-direct, kontra-tunnel-<id>) and on its
// control queue (kontra-ctl-<id>). internal/relay implements them.
type DispatchActivities interface {
	// Dispatch writes attempts.replica_id before any upstream call, loads the work item, and runs a2a-go's
	// handler for it with the relay executor, heartbeating every 5 s (timeout 30 s). It discards the
	// handler's result and returns the outcome the per-process registry recorded.
	Dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error)
	// Abort cancels the running dispatch's context through the replica's registry (spec §8, D44).
	Abort(ctx context.Context, in AttemptRef) error
	// ForwardCancel sends CancelTask upstream for the attempt's upstream task and returns the answer.
	ForwardCancel(ctx context.Context, in AttemptRef) (ForwardCancelResult, error)
}

// PushActivities is push_webhook (spec §10.2); internal/push implements it.
type PushActivities interface {
	PushWebhook(ctx context.Context, in PushInput) (PushResult, error)
}

// BackendResolver tells prepare how a service is reached: the service registry implements it.
type BackendResolver interface {
	BackendFor(ctx context.Context, tenantID uuid.UUID, service string) (store.Backend, error)
}

// PushConfigLister lists a task's live push config ids (list_push_configs); internal/push implements it.
type PushConfigLister interface {
	LiveConfigIDs(ctx context.Context, taskID uuid.UUID) ([]string, error)
}

// LifecycleActivities are the activities on kontra-lifecycle, runnable on any replica.
type LifecycleActivities struct {
	Log      *store.Log
	Backends BackendResolver
	Push     PushConfigLister
}

// Prepare picks the backend (spec §6.1; direct in M1), writes the attempt row and returns the queue to
// dispatch on and the stored deadline.
func (a *LifecycleActivities) Prepare(ctx context.Context, in AttemptRef) (PrepareResult, error) {
	return PrepareResult{}, unimpl.Error("lifecycle.Prepare", "WP-05")
}

// AttemptState reads first_upstream_id_at and replica_id from Postgres.
func (a *LifecycleActivities) AttemptState(ctx context.Context, in AttemptRef) (AttemptState, error) {
	return AttemptState{}, unimpl.Error("lifecycle.AttemptState", "WP-05")
}

// ListPushConfigs returns the task's live push config ids, at start and after each dispatch, to recover
// a lost kontra.push signal (spec §10.2).
func (a *LifecycleActivities) ListPushConfigs(ctx context.Context, taskID string) ([]string, error) {
	return nil, unimpl.Error("lifecycle.ListPushConfigs", "WP-05")
}

// WriteTerminal writes the terminal event through the store when no handler can.
func (a *LifecycleActivities) WriteTerminal(ctx context.Context, in WriteTerminalInput) (int64, error) {
	return 0, unimpl.Error("lifecycle.WriteTerminal", "WP-05")
}

// ApplyMetadata logs one metadata event.
func (a *LifecycleActivities) ApplyMetadata(ctx context.Context, in ApplyMetadataInput) (int64, error) {
	return 0, unimpl.Error("lifecycle.ApplyMetadata", "WP-05")
}
