package relay

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Key names one dispatch: attempt n of a task.
type Key struct {
	TaskID a2a.TaskID
	N      int
}

// Registry is the replica's registry of running dispatches (spec §8, §15; D8, D44). It holds each
// dispatch's cancel function (abort cancels the context in process), the terminal events the relay
// executor yielded (the store lets only those `failed` writes through while the dispatch is
// registered), and the outcome the dispatch returns to the workflow.
type Registry struct {
	mu      sync.Mutex
	running map[Key]*Running
}

var _ store.FailureGuard = (*Registry)(nil)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{running: map[Key]*Running{}}
}

// Running is one registered dispatch.
type Running struct {
	key    Key
	cancel context.CancelCauseFunc
}

// Register records a dispatch about to call a2a-go's handler. It is an error if one is already running
// for the task on this replica.
func (r *Registry) Register(k Key, cancel context.CancelCauseFunc) (*Running, error) {
	return nil, unimpl.Error("relay.Registry.Register", "WP-04")
}

// Abort cancels the dispatch's context; false when nothing runs under k here.
func (r *Registry) Abort(k Key) bool {
	panic(unimpl.Error("relay.Registry.Abort", "WP-04"))
}

// AllowFailed implements store.FailureGuard.
func (r *Registry) AllowFailed(taskID a2a.TaskID, ev a2a.Event) bool {
	panic(unimpl.Error("relay.Registry.AllowFailed", "WP-04"))
}

// RecordTerminal marks ev as relay-emitted.
func (d *Running) RecordTerminal(ev a2a.Event) {
	panic(unimpl.Error("relay.Running.RecordTerminal", "WP-04"))
}

// SetOutcome records what the dispatch returns.
func (d *Running) SetOutcome(o lifecycle.DispatchOutcome, reason store.FailureReason) {
	panic(unimpl.Error("relay.Running.SetOutcome", "WP-04"))
}

// Done unregisters the dispatch and returns its outcome; a dispatch whose context was lost without an
// outcome returns context_lost.
func (d *Running) Done() lifecycle.DispatchResult {
	panic(unimpl.Error("relay.Running.Done", "WP-04"))
}
