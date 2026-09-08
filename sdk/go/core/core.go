// Package core holds the actor lifecycle types shared by the author surface
// (package kontra) and the Temporal runtime (package runtime). It lives apart from
// both so the runtime can consume an author's registry without importing kontra (which
// would cycle): kontra -> core, runtime -> core, kontra -> runtime.
package core

import (
	"reflect"
	"sync"
)

// Session is the shared, mutable state one actor instance carries across its
// load -> method* -> close lifecycle (the Go peer of Python's `self`). Resources opened
// in Load (a browser, a client) stay visible to every Method of that session, which is the
// point of many Methods per Actor: `extract` sees the browser `crawl` opened. The host holds
// one live session per actor id and the workflow blocks on its activity, so this in-process
// state is safe to share across the session's Methods.
type Session struct {
	// mu guards State because a Method body may run its Units in REAL goroutines — the author
	// owns the loop and therefore owns the concurrency (ADR 0023 §18). Without it a concurrent
	// map write is a FATAL, unrecoverable panic that kills the host process. Python's asyncio
	// host is single-threaded and needs no equivalent. A POINTER so any shallow copy of a
	// Session shares this one mutex+State by reference and stays vet-clean (copying a
	// sync.RWMutex value is a lock-copy bug).
	mu             *sync.RWMutex
	State          map[string]any
	Params         map[string]any
	RunID          string
	IdempotencyKey string
	// Rebuilds is how many times this session's resource has been rebuilt after a
	// SessionLost (0 on the first load). An author can react to a rebuild; the runtime
	// gives up after one (SessionUnrecoverable).
	Rebuilds int
	// sessionState is the cross-Method durable store, bound once per batch by the host and
	// shared across the session's Units (nil outside a hosted session — the accessor no-ops).
	// globalState is the cross-session atomic store, bound once per turn by the host (nil
	// outside a hosted session — the accessor no-ops).
	globalState GlobalState
	// emitDurable reports whether an emitted record is persisted at emit time (an object store
	// is configured). Bound once per batch by the host; false outside one.
	emitDurable bool
}

// NewSession builds a session with an initialized shared-state mutex and empty State. The
// host calls it on load AND on every reload; per-unit views (WithArunState) share this
// session's State/mu by reference.
func NewSession(params map[string]any, runID string) *Session {
	return &Session{mu: &sync.RWMutex{}, State: map[string]any{}, Params: params, RunID: runID}
}

// Get/Set are the concurrency-safe accessors over a session's keyed State — use these (not
// the raw State map) to share state between units when a step runs with Parallel>1.
func (s *Session) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.State[key]
	return v, ok
}

func (s *Session) Set(key string, val any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State[key] = val
}

// UnitState is the per-Unit durable RESUME SCRATCH (the Go peer of Python's self.unit_state) —
// a crawl frontier, a cursor; NOT a result store. Get reports absent on a fresh Unit; the host
// DELETES the slot when the Unit commits, so a finished Unit never resumes. Snapshots are opaque
// JSON and at-least-once (a death between the work and Set replays that slice — tolerate replay,
// e.g. a visited-set skip). See ADR 0015.
//
// It hangs off the UNIT rather than the Session because the author owns the loop: with one
// Session for the whole Method call there is no per-Unit view of it to bind, and a
// "current unit" pointer on the Session would be wrong the moment an author ran their Units
// concurrently. Same reasoning the output Dataset follows — the framework attributes a push to the
// Unit the iterator is on, and provenance the author cares about goes inside the record
// (ADR 0023 §18; ADR 0028 §1).
type UnitState interface {
	// Get unmarshals unit_state[key] into out; ok is false when that key is empty. KEYED like
	// the other tiers, so a unit can carry several independent resume slots.
	Get(key string, out any) (ok bool, err error)
	// Set snapshots a keyed scratch value (opaque JSON) durably.
	Set(key string, value any) error
	// Delete drops one resume key (the whole unit's scratch is cleared on commit regardless).
	Delete(key string) error
}

// noopUnitState is the unbound accessor used outside a hosted Unit (plain Method tests): Get
// reports absent, Set/Delete are no-ops — matching Python's None-safe self.unit_state.
type noopUnitState struct{}

func (noopUnitState) Get(string, any) (bool, error) { return false, nil }
func (noopUnitState) Set(string, any) error         { return nil }
func (noopUnitState) Delete(string) error           { return nil }

// EmitDurable reports whether pushed records are persisted at push time (an object store is
// configured). A Method that RESUMES by skipping already-pushed work (a unit.State() cursor over
// what it pushed) MUST gate that skip on this: when false (the inline dev/test mode, no
// KONTRA_S3_*), pushed records are not durable until their Unit commits, so skipping them on a
// resume would silently lose them — the Method should re-do the work atomically instead. (Kept the
// name emit_durable across both SDKs; ADR 0028 renamed the surface verb, not this gate.)
func (s *Session) EmitDurable() bool { return s.emitDurable }

// BindEmitDurable records whether this batch's emits are durable at emit time. The host calls it
// once per batch; authors never do.
func (s *Session) BindEmitDurable(v bool) { s.emitDurable = v }

// `session_state` was tier 2 and RETIRED with ADR 0023 §19, taking SessionState/
// BindSessionState/noopSessionState with it. Every path that could read it runs in the same
// process on the same instance, where the Session's own fields already work; the one path that
// loses them is host death, and that FAILS THE SCOPE (§7), so the reader is gone too.
//
// UnitState survives — see the §19 amendment. Its reader is a Unit that was IN FLIGHT when the
// activity died and re-runs on the handler's retry, against the same batch hash and slot.

// GlobalState is cross-session (cross-actor-id), actor-NAME-scoped DURABLE state — tier 3 of
// ADR 0015 (the Go peer of Python's self.global_state). Shared across every session of an actor
// and across versions, over its own ETag'd store. Beyond Get/Set it offers ATOMIC ops so
// concurrent sessions never lose updates: AddToSet (the dedupe flagship), Incr, CompareAndSet.
// Prefer the atomics over Get/Set + your own read-modify-write, which is last-write-wins across
// sessions. Bound once per turn by the host; nil outside a hosted session (a safe no-op).
type GlobalState interface {
	Get(key string, out any) (ok bool, err error)
	Set(key string, value any) error
	AddToSet(key string, member any) (added bool, err error)
	Incr(key string, by int) (int, error)
	CompareAndSet(key string, expected, new any) (bool, error)
}

// noopGlobalState is the unbound accessor used outside a hosted session (plain step tests).
type noopGlobalState struct{}

func (noopGlobalState) Get(string, any) (bool, error)                { return false, nil }
func (noopGlobalState) Set(string, any) error                        { return nil }
func (noopGlobalState) AddToSet(string, any) (bool, error)           { return false, nil }
func (noopGlobalState) Incr(string, int) (int, error)                { return 0, nil }
func (noopGlobalState) CompareAndSet(string, any, any) (bool, error) { return false, nil }

// GlobalState returns the cross-session atomic-state accessor. Outside a hosted session it is a
// safe no-op, so step bodies unit-test without a host.
func (s *Session) GlobalState() GlobalState {
	if s.globalState == nil {
		return noopGlobalState{}
	}
	return s.globalState
}

// BindGlobalState binds this session's global_state store. The host calls it once per turn;
// per-unit views inherit it via WithArunState's copy. Authors never call this.
func (s *Session) BindGlobalState(gs GlobalState) {
	s.globalState = gs
}

// The lifecycle function shapes (the Go peers of @actor.load/method/close/healthcheck).
type (
	LoadFunc  func(*Session) error
	CloseFunc func(*Session) error
	// HealthcheckFunc is a resource-liveness probe (the Go peer of @actor.healthcheck).
	// Convention: it RETURNS progress (any, may be nil) with a nil error when the resource
	// is ALIVE; a non-nil error means DEAD. For Python back-compat it may also return the
	// bool `false` (with a nil error) to mean DEAD — the host treats that as dead too.
	HealthcheckFunc func(*Session) (any, error)
)

// Registry is an actor's full registration: identity + lifecycle + typed I/O. The
// author builds one (kontra.New() ... .Serve()); the runtime serves it.
type Registry struct {
	Name    string
	Version string
	LoadFn  LoadFunc
	// Methods is every declared Method, in declaration order. Order is a presentation
	// detail — the error message that lists them — and NEVER a data-flow contract: a
	// dispatch names the Method it wants (ADR 0023 §16).
	Methods []Method
	CloseFn CloseFunc

	// HealthcheckFn is an optional resource-liveness probe (the Go peer of
	// @actor.healthcheck). nil = none. The host uses it as the primary death signal:
	// on a Method error it probes the resource and, if the probe reports dead (non-nil error
	// or a returned false), treats the batch as SessionLost so the handler reloads.
	HealthcheckFn HealthcheckFunc

	// ParamsType is a zero-value instance of the actor's run-wide config type, for describe().
	// nil = undeclared. It is the ONE type still declared on the Actor: params are run-wide
	// config, not a per-Method signature. What a Method takes and emits lives on the Method
	// (see Method.Takes / Method.Emits), because an Actor has many Methods with different
	// signatures and one Actor-level pair cannot describe them.
	ParamsType any
}

// --- Author error signals (the Go peers of kontra.retry's NonRetryableError /
// SessionLost). Go has no exception subclassing, so a marker-interface lets an author's
// own error type opt into non-retryable handling, mirroring Python's isinstance check.

// NonRetryable is implemented by errors that must NOT be retried — the runtime maps them
// to a non-retryable Temporal ApplicationError and isolates the unit as "terminal".
type NonRetryable interface {
	error
	KontraNonRetryable()
}

// Typed lets an author override the error-type name surfaced in the failure record and
// in Temporal's non_retryable_error_types matching. Without it the Go type name is used.
type Typed interface {
	KontraErrorType() string
}

// NonRetryableError is the built-in non-retryable signal (kontra.NonRetryable("...")).
type NonRetryableError struct{ Msg string }

func (e *NonRetryableError) Error() string           { return e.Msg }
func (e *NonRetryableError) KontraNonRetryable()     {}
func (e *NonRetryableError) KontraErrorType() string { return "NonRetryableError" }

// SessionLostError signals the session's shared resource is dead and must be rebuilt
// once (kontra.SessionLost("...")). It is RETRYABLE — sibling of, not a, NonRetryable.
type SessionLostError struct{ Msg string }

func (e *SessionLostError) Error() string           { return e.Msg }
func (e *SessionLostError) KontraErrorType() string { return "SessionLost" }

// ErrorTypeName returns the cross-language error-type name for an error: the Typed
// override if present, else the Go type's bare name (the peer of type(e).__name__).
func ErrorTypeName(err error) string {
	if t, ok := err.(Typed); ok {
		return t.KontraErrorType()
	}
	rt := reflect.TypeOf(err)
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt != nil && rt.Name() != "" {
		return rt.Name()
	}
	return "error"
}
