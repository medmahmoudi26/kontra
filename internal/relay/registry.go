package relay

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/store"
)

// Causes the registry cancels a dispatch's context with.
var (
	// ErrAborted is abort(): a cancel before any upstream task stops the dispatch in process (spec §8, D44).
	ErrAborted = errors.New("relay: dispatch aborted")
	// ErrSuperseded is a newer dispatch of the same task registering on this replica (a resume landing
	// on the replica of a dispatch that is still alive, a gate of one replica after a GC pause): the older
	// one is canceled and the newer one takes over. Its fence already refuses its writes (D77).
	ErrSuperseded = errors.New("relay: dispatch superseded by a newer dispatch of the task")
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
	byTask  map[a2a.TaskID]*Running
}

var _ store.FailureGuard = (*Registry)(nil)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{running: map[Key]*Running{}, byTask: map[a2a.TaskID]*Running{}}
}

// Running is one registered dispatch.
type Running struct {
	reg    *Registry
	key    Key
	cancel context.CancelCauseFunc

	mu         sync.Mutex
	terminals  map[a2a.Event]struct{}
	outcome    lifecycle.DispatchOutcome
	reason     store.FailureReason
	retryAfter time.Duration
	aborted    bool
	superseded bool
}

// Register records a dispatch about to call a2a-go's handler. A dispatch of the same task already
// registered here (the same attempt or another) is canceled with ErrSuperseded and replaced: the newer
// dispatch takes over, and the older one, whose fence the newer one's BeginDispatch already broke, stops
// instead of looping against it.
func (r *Registry) Register(k Key, cancel context.CancelCauseFunc) (*Running, error) {
	if cancel == nil {
		return nil, errors.New("relay: a dispatch registers with its cancel function")
	}
	d := &Running{reg: r, key: k, cancel: cancel, terminals: map[a2a.Event]struct{}{}}
	r.mu.Lock()
	old := r.byTask[k.TaskID]
	if old != nil {
		delete(r.running, old.key)
	}
	r.running[k] = d
	r.byTask[k.TaskID] = d
	r.mu.Unlock()
	if old != nil {
		old.mu.Lock()
		old.superseded = true
		old.mu.Unlock()
		old.cancel(ErrSuperseded)
	}
	return d, nil
}

// Abort cancels the dispatch's context; false when nothing runs under k here.
func (r *Registry) Abort(k Key) bool {
	r.mu.Lock()
	d := r.running[k]
	r.mu.Unlock()
	if d == nil {
		return false
	}
	d.mu.Lock()
	d.aborted = true
	d.mu.Unlock()
	d.cancel(ErrAborted)
	return true
}

// Running returns the dispatch registered for k, nil when there is none.
func (r *Registry) Running(k Key) *Running {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[k]
}

// AllowFailed implements store.FailureGuard: true when no dispatch of the task is registered here, or
// when ev is (by pointer identity) a terminal event the registered dispatch's relay executor yielded.
func (r *Registry) AllowFailed(taskID a2a.TaskID, ev a2a.Event) bool {
	r.mu.Lock()
	d := r.byTask[taskID]
	r.mu.Unlock()
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.terminals[ev]
	return ok
}

// RecordTerminal marks ev as relay-emitted. The executor records an event before it yields it: a2a-go's
// consumer may store it before yield returns.
func (d *Running) RecordTerminal(ev a2a.Event) {
	d.mu.Lock()
	d.terminals[ev] = struct{}{}
	d.mu.Unlock()
}

// SetOutcome records what the dispatch returns.
func (d *Running) SetOutcome(o lifecycle.DispatchOutcome, reason store.FailureReason) {
	d.mu.Lock()
	d.outcome, d.reason = o, reason
	d.mu.Unlock()
}

// SetRetryAfter records an upstream Retry-After for a retryable outcome (HTTP 408/429, spec §6.4).
func (d *Running) SetRetryAfter(wait time.Duration) {
	d.mu.Lock()
	d.retryAfter = wait
	d.mu.Unlock()
}

// Outcome is the outcome recorded so far ("" when none).
func (d *Running) Outcome() (lifecycle.DispatchOutcome, store.FailureReason) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.outcome, d.reason
}

// Aborted reports whether abort() stopped the dispatch.
func (d *Running) Aborted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.aborted
}

// Superseded reports whether a newer dispatch of the task took over on this replica.
func (d *Running) Superseded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.superseded
}

// Done unregisters the dispatch and returns its outcome: the recorded one, aborted when abort() stopped
// it, and otherwise context_lost (a dispatch whose context was lost without an outcome; the Dispatcher
// turns an error that is not a lost context into upstream_error, spec §6.4). A superseded dispatch does
// not unregister its successor.
func (d *Running) Done() lifecycle.DispatchResult {
	r := d.reg
	r.mu.Lock()
	if r.running[d.key] == d {
		delete(r.running, d.key)
	}
	if r.byTask[d.key.TaskID] == d {
		delete(r.byTask, d.key.TaskID)
	}
	r.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	res := lifecycle.DispatchResult{Outcome: d.outcome, Reason: string(d.reason)}
	if d.retryAfter > 0 {
		res.RetryAfterSeconds = int64((d.retryAfter + time.Second - 1) / time.Second)
	}
	switch {
	case d.outcome != "":
	case d.aborted:
		res.Outcome = lifecycle.DispatchAborted
	default:
		res.Outcome = lifecycle.DispatchContextLost
	}
	return res
}
