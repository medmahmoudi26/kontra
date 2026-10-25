package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/ids"
)

// OwnerFunc extracts the caller from a request context: in a2a-go's handler, from CallContext.User,
// which the gate's authentication interceptor sets and the work queue carries to the dispatching replica.
type OwnerFunc func(ctx context.Context) (Owner, error)

// FailureGuard is the per-process registry of running dispatches (D8, spec §15). While a dispatch for
// the task is registered, Store.Update refuses a `failed` status write the guard does not hold as
// relay-emitted: that is how the failures a2a-go synthesizes on its own error paths (SetTaskFailed) are
// kept out of the log, so the workflow can retry, re-attach or write the terminal event itself.
type FailureGuard interface {
	// AllowFailed reports whether a failed-status write for taskID carrying ev may be stored: true when
	// no dispatch of the task is registered here, or when ev is (by pointer identity: a2a-go hands the
	// yielded value through unchanged) an event the relay executor recorded as its own.
	AllowFailed(taskID a2a.TaskID, ev a2a.Event) bool
}

// Store implements taskstore.Store for one service (spec §5.2, §15).
type Store struct {
	db    *pgxpool.Pool
	svc   ServiceRef
	owner OwnerFunc
	guard FailureGuard
	log   *Log
	// listBytes and listBatch bound ListTasks (ListPageBytes, listBatchStored); tests lower them.
	listBytes int
	listBatch int64
}

var _ taskstore.Store = (*Store)(nil)

// New returns the store a2a-go's handler for svc is built with. The handler must keep a2a-go's default,
// full update materializer: Update stores UpdateRequest.Task as the whole projection. guard may be nil
// (no failure is then refused).
func New(db *pgxpool.Pool, svc ServiceRef, owner OwnerFunc, guard FailureGuard, log *Log) *Store {
	if log == nil {
		log = NewLog(db)
	}
	return &Store{db: db, svc: svc, owner: owner, guard: guard, log: log, listBytes: ListPageBytes, listBatch: listBatchStored}
}

func (s *Store) caller(ctx context.Context) (Owner, error) {
	if s.owner == nil {
		return Owner{}, fmt.Errorf("store: no owner function: %w", a2a.ErrUnauthenticated)
	}
	o, err := s.owner(ctx)
	if err != nil {
		return Owner{}, fmt.Errorf("store: caller: %w", err)
	}
	return o, nil
}

// Create inserts a task at version 1 with its `task` event at seq 1. a2a-go calls it only when an
// executor yields a Task first; the relay never does (the work queue pre-creates every gate task), so
// this path exists for the contract and its tests. A duplicate id is taskstore.ErrTaskAlreadyExists.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: no task: %w", a2a.ErrInvalidParams)
	}
	o, err := s.caller(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	id, err := ids.ParseTaskID(string(task.ID))
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: create task %q: not a gate task id: %w", task.ID, a2a.ErrInvalidParams)
	}
	state, err := StateFromA2A(task.Status.State)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if _, err := s.log.CreateTask(ctx, nil, NewTask{
		ID: id, Service: s.svc, Protocol: ProtocolA2A, Owner: o, ContextID: task.ContextID, State: state, Snapshot: task,
	}); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return 1, nil
}

// Update is the one transaction of spec §5.2. Zero rows (version moved, or the terminal rule refused the
// write) is taskstore.ErrConcurrentModification; no row in scope is a2a.ErrTaskNotFound; PrevVersion 0 is
// ErrConcurrentModification. A `message` event is a caller follow-up appended to history (kind message);
// status and artifact events are logged as they are (ids already rewritten by the relay).
//
// Two refusals come from the dispatch around the write: a fenced dispatch (ctx carries a Fence that no
// longer holds) is ErrFenced, and while the task's dispatch is registered with the FailureGuard, a failed
// status with no message (a2a-go's SetTaskFailed shape) that the relay did not emit is
// ErrSynthesizedFailure. A relay-emitted failure is recognized by the guard's pointer identity first, and
// by its message second: the relay's failures always carry one (spec §6.3).
func (s *Store) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil || req.Task == nil || req.Event == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: an update needs the task and its event: %w", a2a.ErrInvalidParams)
	}
	o, err := s.caller(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	id, err := ids.ParseTaskID(string(req.Task.ID))
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if req.PrevVersion <= taskstore.TaskVersionMissing {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: PrevVersion %d: %w", req.PrevVersion, taskstore.ErrConcurrentModification)
	}
	w := Write{
		TaskID: id, PrevVersion: int64(req.PrevVersion), Snapshot: req.Task,
		Timeout: s.svc.Timeout, Retention: s.svc.Retention, Fence: FenceFrom(ctx),
	}
	scope := ScopeOf(s.svc, o)
	w.Scope = &scope
	switch ev := req.Event.(type) {
	case *a2a.Task:
		w.Kind = KindTask
	case *a2a.TaskStatusUpdateEvent:
		w.Kind = KindStatus
		if s.synthesizedFailure(req.Task.ID, ev, ev.Status) {
			return taskstore.TaskVersionMissing, fmt.Errorf("store: task %s: %w", id, ErrSynthesizedFailure)
		}
	case *a2a.TaskArtifactUpdateEvent:
		w.Kind = KindArtifact
	case *a2a.Message:
		w.Kind = KindMessage
	default:
		return taskstore.TaskVersionMissing, fmt.Errorf("store: unexpected event %T: %w", req.Event, a2a.ErrInvalidParams)
	}
	if t, ok := req.Event.(*a2a.Task); ok && s.synthesizedFailure(req.Task.ID, t, t.Status) {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: task %s: %w", id, ErrSynthesizedFailure)
	}
	if w.Kind == KindTask || w.Kind == KindStatus {
		if w.State, err = StateFromA2A(req.Task.Status.State); err != nil {
			return taskstore.TaskVersionMissing, err
		}
		w.StatusAt = StatusTime(req.Task.Status.Timestamp)
	}
	if w.Payload, err = json.Marshal(a2a.StreamResponse{Event: req.Event}); err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("store: encode event: %w", err)
	}
	a, err := s.log.Append(ctx, nil, w)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(a.Seq), nil
}

// synthesizedFailure reports whether a write of status st is a failure a2a-go made up while the task's
// dispatch runs here: failed, no message, and not recorded by the relay. At the pinned a2a-go v2.7.0 the
// only failed writes that reach the store while a dispatch is registered are the relay's (recorded, and
// always carrying a message, spec §6.3) and SetTaskFailed's (never carrying one), so this refuses exactly
// D8's "failed write the relay did not emit"; the message check keeps the relay's own failure from being
// refused should a2a-go ever stop passing the yielded pointer through.
func (s *Store) synthesizedFailure(taskID a2a.TaskID, ev a2a.Event, st a2a.TaskStatus) bool {
	if s.guard == nil || st.State != a2a.TaskStateFailed || st.Message != nil {
		return false
	}
	return !s.guard.AllowFailed(taskID, ev)
}

// Get returns the projection and its version, scoped to the caller. A malformed id (checked before any
// query), an unknown id and a task of another buyer are all a2a.ErrTaskNotFound (spec §4).
func (s *Store) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	id, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return nil, err
	}
	o, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	var (
		snap      []byte
		version   int64
		principal string
	)
	err = s.db.QueryRow(ctx, `select snapshot, version, principal from kontra.tasks
		where id = $1 and tenant_id = $2 and service = $3 and buyer_id = $4`,
		id, s.svc.TenantID, s.svc.Name, o.BuyerID).Scan(&snap, &version, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task %s: %w", id, err)
	}
	task, err := decodeTask(snap)
	if err != nil {
		return nil, fmt.Errorf("store: task %s snapshot: %w", id, err)
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: principal}, nil
}
