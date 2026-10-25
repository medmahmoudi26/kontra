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
// registered), and the outcome the dispatch returns to the workflow; and the dispatches that recorded
// this replica but have not registered yet (Expect), so that an abort reaching one of them is kept.
type Registry struct {
	mu      sync.Mutex
	running map[Key]*Running
	byTask  map[a2a.TaskID]*Running
	// pending holds the dispatches that announced themselves (Expect) and have not registered or given up
	// yet, per key, and whether an abort reached them meanwhile.
	pending map[Key]*expected
}

// expected is the announcement of one or more dispatches of a key.
type expected struct {
	n       int
	aborted bool
}

var _ store.FailureGuard = (*Registry)(nil)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{running: map[Key]*Running{}, byTask: map[a2a.TaskID]*Running{}, pending: map[Key]*expected{}}
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
	storeErr   error
}

// Expect announces a dispatch of k that is about to record this replica in attempts.replica_id
// (BeginDispatch), which is what routes abort(k) here (spec §14.2), and has not registered yet: it consumes
// the turn's work item and builds a2a-go's payload and handler first. An abort of k from then until the
// dispatch registers is kept, and Register returns the dispatch already aborted, its context canceled with
// ErrAborted, so it stops before calling the upstream (spec §8). release ends the announcement; it is
// idempotent, and after Register it leaves the registered dispatch alone.
func (r *Registry) Expect(k Key) (release func()) {
	r.mu.Lock()
	p := r.pending[k]
	if p == nil {
		p = &expected{}
		r.pending[k] = p
	}
	p.n++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if p.n--; p.n <= 0 && r.pending[k] == p {
				delete(r.pending, k)
			}
			r.mu.Unlock()
		})
	}
}

// Register records a dispatch about to call a2a-go's handler. A dispatch of the same task already
// registered here (the same attempt or another) is canceled with ErrSuperseded and replaced: the newer
// dispatch takes over, and the older one, whose fence the newer one's BeginDispatch already broke, stops
// instead of looping against it. A dispatch an abort reached while it was announced (Expect) registers
// aborted, its context canceled with ErrAborted.
func (r *Registry) Register(k Key, cancel context.CancelCauseFunc) (*Running, error) {
	if cancel == nil {
		return nil, errors.New("relay: a dispatch registers with its cancel function")
	}
	d := &Running{reg: r, key: k, cancel: cancel, terminals: map[a2a.Event]struct{}{}}
	r.mu.Lock()
	if p := r.pending[k]; p != nil && p.aborted {
		d.aborted = true
	}
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
	if d.aborted {
		cancel(ErrAborted)
	}
	return d, nil
}

// Abort cancels the dispatch's context, and marks a dispatch of k announced but not registered yet
// (Expect) so that it registers aborted; false when nothing runs or is announced under k here.
func (r *Registry) Abort(k Key) bool {
	r.mu.Lock()
	d := r.running[k]
	p := r.pending[k]
	if p != nil {
		p.aborted = true
	}
	r.mu.Unlock()
	if d == nil {
		return p != nil
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

// NoteStoreFailure records a failure of the gate's own store while the dispatch logged an event: a write
// that did not commit, or the push config read a2a-go's processor makes after each event. The dispatch's
// result is then a lost context, never an upstream failure (spec §6.4); the first one is kept.
func (d *Running) NoteStoreFailure(err error) {
	d.mu.Lock()
	if d.storeErr == nil {
		d.storeErr = err
	}
	d.mu.Unlock()
}

// StoreFailure is the first store failure NoteStoreFailure recorded, nil when none.
func (d *Running) StoreFailure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.storeErr
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
