package store

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// OwnerFunc extracts the caller from a request context: in a2a-go's handler, from CallContext.User,
// which the gate's authentication interceptor sets and the work queue carries to the dispatching replica.
type OwnerFunc func(ctx context.Context) (Owner, error)

// FailureGuard is the per-process registry of running dispatches (D8, spec §15). While a dispatch for
// the task is registered, Store.Update refuses a `failed` status write the guard does not hold as
// relay-emitted: that is how the failures a2a-go synthesizes on its own error paths (SetTaskFailed) are
// kept out of the log, so the workflow can retry, re-attach or write the terminal event itself.
type FailureGuard interface {
	// AllowFailed reports whether a failed-status write for taskID carrying ev may be stored.
	AllowFailed(taskID a2a.TaskID, ev a2a.Event) bool
}

// Store implements taskstore.Store for one service (spec §5.2, §15).
type Store struct {
	db    *pgxpool.Pool
	svc   ServiceRef
	owner OwnerFunc
	guard FailureGuard
	log   *Log
}

var _ taskstore.Store = (*Store)(nil)

// New returns the store a2a-go's handler for svc is built with.
func New(db *pgxpool.Pool, svc ServiceRef, owner OwnerFunc, guard FailureGuard, log *Log) *Store {
	return &Store{db: db, svc: svc, owner: owner, guard: guard, log: log}
}

// Create inserts a task at version 1 with its `task` event at seq 1. a2a-go calls it only when an
// executor yields a Task first; the relay never does (the work queue pre-creates every gate task), so
// this path exists for the contract and its tests. A duplicate id is taskstore.ErrTaskAlreadyExists.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	return taskstore.TaskVersionMissing, unimpl.Error("store.Store.Create", "WP-02")
}

// Update is the one transaction of spec §5.2. Zero rows (version moved, or the terminal rule refused the
// write) is taskstore.ErrConcurrentModification; no row in scope is a2a.ErrTaskNotFound; PrevVersion 0 is
// ErrConcurrentModification. A `message` event is a caller follow-up appended to history (kind message);
// status and artifact events are logged as they are (ids already rewritten by the relay).
func (s *Store) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	return taskstore.TaskVersionMissing, unimpl.Error("store.Store.Update", "WP-02")
}

// Get returns the projection and its version, scoped to the caller. A malformed id, an unknown id and a
// task of another buyer are all a2a.ErrTaskNotFound (spec §4).
func (s *Store) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	return nil, unimpl.Error("store.Store.Get", "WP-02")
}

// List implements ListTasks for the caller's own tasks of this service, ordered by (status_at desc, id
// desc), keyset-paginated with gate-minted page tokens; page size 1..100 (default 50), the contextId,
// status and statusTimestampAfter filters, history trimming, artifacts only when asked, totalSize before
// pagination (spec §3.1.4, TCK CORE-LIST-*). A bad page token is InvalidParams.
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, unimpl.Error("store.Store.List", "WP-02")
}
