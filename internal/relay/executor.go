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
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Executor is the relay's a2asrv.AgentExecutor (spec §6.2–§6.4, §15). It
//
//   - never yields a Task (the gate task exists: the work queue pre-created it) and never a bare
//     Message (an upstream Message becomes a terminal completed status carrying it, D5);
//   - reads attempts to tell a first turn, a follow-up turn (the sticky attempt's upstream ids
//     substituted) and a re-attach (Resume, D42) apart;
//   - calls SendStreamingMessage when the upstream card streams, falling back in the same attempt to
//     SendMessage{returnImmediately: true} plus GetTask polling (1 s, backing off to 5 s after a minute
//     without change) on UnsupportedOperationError before any event (D15);
//   - records the first upstream task id before logging the event that carried it, and signals
//     kontra.dispatched;
//   - rewrites every upstream id to the gate's at every depth and strips Kontra's keys (§2.5), fails the
//     attempt upstream_error on an event naming a foreign upstream task;
//   - follows the upstream through auth-required and ends its turn on a terminal state or input-required;
//   - emits explicit terminal events with the upstream's message and returns nil instead of an error,
//     recording them in the Registry so the store lets them through (D8).
type Executor struct {
	log      *store.Log
	registry *Registry
	backends Backends
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// NewExecutor returns the relay executor.
func NewExecutor(log *store.Log, registry *Registry, backends Backends) *Executor {
	return &Executor{log: log, registry: registry, backends: backends}
}

// Execute runs one turn of one attempt.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(nil, unimpl.Error("relay.Executor.Execute", "WP-04"))
	}
}

// Cancel is never reached: CancelTask is decided by CancelInterceptor before a2a-go's cancel path,
// which would mark the task failed when the upstream refuses (D4).
func (e *Executor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(nil, unimpl.Error("relay.Executor.Cancel", "WP-04"))
	}
}
