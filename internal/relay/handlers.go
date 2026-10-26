package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/push"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// Backends resolves the backend and upstream card info a dispatch or a forward_cancel of an attempt uses.
// The service registry implements it: a direct service's backend and Info from its card.Keeper
// (Keeper.Dispatch); a tunnel service's backend is the attempt's session on this replica
// (tunnelgate.Registry.Backend), with the Info of the service's one card (spec §3, §6.2).
//
// The error says what the dispatch does (spec §6.4, §14.2): one wrapping upstream.ErrTunnelLost (the
// attempt's session is not held here, or no longer serves the service) is a lost session and opens no
// stream; one wrapping ErrCardPending (a tunnel service whose card no fetch has produced yet) is retried
// like no_session; any other (no usable card, a service removed or disabled) fails service_unavailable.
// forward_cancel takes upstream.ErrTunnelLost for a gone session only for a tunnel attempt, and asks again
// after an exchange that ended without a complete response (Dispatcher.ForwardCancel).
type Backends interface {
	Backend(ctx context.Context, svc store.ServiceRef, attempt *store.Attempt) (upstream.Backend, upstream.Info, error)
}

// ErrCardPending is a tunnel service whose card no fetch through a session has produced yet: a task placed
// on a session that has just connected, whose card fetch failed (its node's agent not listening yet). The
// dispatch is retryable before the first upstream task id, like no_session, rather than failed:
// service_unavailable (M2 plan audit, gap 3, the audit's second option; see gate.Services.tunnelConnected).
var ErrCardPending = errors.New("relay: the tunnel service's card has not been fetched yet")

// ServiceHandler is one service's a2a-go handler and the HandlerFn its work queue registered.
type ServiceHandler struct {
	Service   store.ServiceRef
	Handler   a2asrv.RequestHandler // what the JSON-RPC and REST transports serve
	HandlerFn workqueue.HandlerFn   // what the dispatch activity calls
	Store     *store.Store
}

// HandlerDeps are what every service handler is built from.
type HandlerDeps struct {
	Log          *store.Log
	Puller       eventqueue.PullerProvider
	PushStore    a2apush.ConfigStore                        // nil: push config calls answer PushNotificationNotSupported
	Queue        func(svc store.ServiceRef) workqueue.Queue // lifecycle.NewQueue
	Interceptors func(svc store.ServiceRef) []a2asrv.CallInterceptor
	Registry     *Registry
	Backends     Backends
	Owner        store.OwnerFunc // CallerOwner when nil
	Signals      Signaler        // kontra.dispatched
	Logger       *slog.Logger
	Executor     ExecutorOptions

	// wrapTaskStore, in tests, wraps each service's task store beneath the dispatch's store-failure notes.
	wrapTaskStore func(*store.Store) taskstore.Store
}

// PollInterval is the pull queue's poll interval (spec §15). The puller long-polls on LISTEN, so this only
// bounds the latency after a burst.
const PollInterval = 100 * time.Millisecond

// Handlers builds each service's handler on first use and caches it (spec §15): a2asrv.NewHandler with
// WithClusterMode{TaskStore, QueueManager: eventqueue.NewPullQueueManager(Puller, {PollInterval: 100 ms,
// InactivityTimeout: 0}), WorkQueue}, WithPushNotifications(PushStore, push.NoopSender) (push is the
// lifecycle's, §10), the interceptors, and no input-mode enforcer (D14). The task store and the push
// config store reach a2a-go through dispatchStore and dispatchPushStore. A config or NOTIFY
// kontra_services change drops the cached handler.
type Handlers struct {
	deps  HandlerDeps
	mu    sync.Mutex
	cache map[string]*ServiceHandler
}

// NewHandlers returns an empty cache.
func NewHandlers(deps HandlerDeps) *Handlers {
	return &Handlers{deps: deps, cache: map[string]*ServiceHandler{}}
}

func cacheKey(svc store.ServiceRef) string { return svc.TenantID.String() + "/" + svc.Name }

// capturingQueue records the HandlerFn a2a-go registers on the service's queue, whatever the queue does
// with it, so the dispatch activity can call it.
type capturingQueue struct {
	workqueue.Queue
	fn workqueue.HandlerFn
}

func (q *capturingQueue) RegisterHandler(cfg workqueue.HandlerConfig, fn workqueue.HandlerFn) {
	q.fn = fn
	q.Queue.RegisterHandler(cfg, fn)
}

// For returns svc's handler, building it on first use. The handler is always the one built from svc
// itself: a cached handler of another ServiceRef (another task.timeout, task.retries, retention) is built
// again and replaces it. A caller can hold a ServiceRef read before a reload and call For after the
// registry's Invalidate (an A2A request between its Lookup and For, a dispatch between LoadService and
// For); the handler it builds must not stay cached for the callers that read the service afterwards.
func (h *Handlers) For(ctx context.Context, svc store.ServiceRef) (*ServiceHandler, error) {
	if svc.TenantID == uuid.Nil || svc.Name == "" {
		return nil, errors.New("relay: a handler needs its service")
	}
	key := cacheKey(svc)
	h.mu.Lock()
	defer h.mu.Unlock()
	if sh, ok := h.cache[key]; ok && sh.Service == svc {
		return sh, nil
	}
	d := h.deps
	if d.Log == nil || d.Puller == nil || d.Queue == nil || d.Registry == nil || d.Backends == nil {
		return nil, errors.New("relay: handler dependencies missing")
	}
	owner := d.Owner
	if owner == nil {
		owner = CallerOwner
	}
	st := store.New(d.Log.DB(), svc, owner, d.Registry, d.Log)
	q := &capturingQueue{Queue: d.Queue(svc)}
	qm := eventqueue.NewPullQueueManager(newTaskFromStart(d.Puller), eventqueue.PullConfig{PollInterval: PollInterval, InactivityTimeout: 0})
	exec := NewExecutor(d.Log, d.Registry, d.Backends).ForService(svc, d.Signals, d.Logger, d.Executor)
	var ts taskstore.Store = st
	if d.wrapTaskStore != nil {
		ts = d.wrapTaskStore(st)
	}
	opts := []a2asrv.RequestHandlerOption{
		a2asrv.WithClusterMode(a2asrv.ClusterConfig{TaskStore: dispatchStore{ts}, QueueManager: qm, WorkQueue: q}),
	}
	if d.Logger != nil {
		opts = append(opts, a2asrv.WithLogger(d.Logger))
	}
	if d.PushStore != nil {
		opts = append(opts, a2asrv.WithPushNotifications(dispatchPushStore{d.PushStore}, push.NoopSender{}))
	}
	if d.Interceptors != nil {
		opts = append(opts, a2asrv.WithCallInterceptors(d.Interceptors(svc)...))
	}
	handler := a2asrv.NewHandler(exec, opts...)
	if q.fn == nil {
		return nil, fmt.Errorf("relay: service %s/%s: a2a-go registered no work queue handler", svc.Tenant, svc.Name)
	}
	sh := &ServiceHandler{Service: svc, Handler: handler, HandlerFn: q.fn, Store: st}
	h.cache[key] = sh
	return sh, nil
}

// dispatchStore is the task store a2a-go's handler reads and writes through. A read or write a dispatch
// makes (its context carries the dispatch) that fails for a reason of the database's own — it ran out of
// its time on a lock or a slow server, the connection broke, the server shut down — is recorded in the
// dispatch's registry entry, so that the dispatch returns context_lost instead of failing a healthy
// upstream task upstream_error (spec §6.4, D42): before the first upstream task id the workflow retries,
// after it a later dispatch re-attaches. a2a-go's processor turns any write error into its own
// SetTaskFailed, which the store refuses (D8), and a2a-go's executor setup turns a failed read of the task
// (loadExecutionContext's Get, on every dispatch in cluster mode) into "executor setup failed", so the
// handler's error alone cannot say whose failure it was. The store's refusals (a version conflict, the
// terminal rule, a fenced dispatch, a2a-go's synthesized failure, a task not found) are a2a-go's to handle
// and the Dispatcher's to map, and an error that would only repeat on a re-attach is not a lost context.
type dispatchStore struct{ taskstore.Store }

func (s dispatchStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	v, err := s.Store.Update(ctx, req)
	if err != nil && storeFailure(err) {
		noteStoreFailure(ctx, err)
	}
	return v, err
}

func (s dispatchStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	t, err := s.Store.Get(ctx, taskID)
	if err != nil && storeFailure(err) {
		noteStoreFailure(ctx, err)
	}
	return t, err
}

// dispatchPushStore records, the same way, a failed read of the task's push configs, which a2a-go's
// processor makes after every event a dispatch logged: the event is in the log, and only the gate's own
// store failed.
type dispatchPushStore struct{ a2apush.ConfigStore }

func (s dispatchPushStore) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	cfgs, err := s.ConfigStore.List(ctx, taskID)
	if err != nil {
		noteStoreFailure(ctx, err)
	}
	return cfgs, err
}

func noteStoreFailure(ctx context.Context, err error) {
	if d, ok := dispatchFrom(ctx); ok {
		d.running.NoteStoreFailure(err)
	}
}

// storeFailure reports whether err, returned by the store, is a failure of the database rather than one of
// the store's refusals or an error a re-attach would only meet again.
func storeFailure(err error) bool {
	switch {
	case errors.Is(err, taskstore.ErrConcurrentModification), // a version conflict, the terminal rule, ErrFenced
		errors.Is(err, store.ErrSynthesizedFailure),
		errors.Is(err, a2a.ErrTaskNotFound), errors.Is(err, a2a.ErrInvalidParams), errors.Is(err, a2a.ErrUnauthenticated):
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		switch pgErr.Code[:2] {
		case "22", "23", "42": // a data exception, an integrity violation, a syntax or access error
			return false
		}
	}
	return true
}

// Invalidate drops svc's cached handler; the next For builds it again with the service's current policy.
func (h *Handlers) Invalidate(svc store.ServiceRef) {
	h.mu.Lock()
	delete(h.cache, cacheKey(svc))
	h.mu.Unlock()
}

// User attributes the identity interceptor sets on CallContext.User (Name is the buyer id). They cross the
// work queue inside the call context, so the dispatching replica's store scopes by the same owner.
const (
	AttrPrincipal = "principal"
	AttrVia       = "via"
	AttrTenant    = "tenant"
)

// CallerOwner is the store.OwnerFunc of every service handler: the owner from CallContext.User, as the
// identity interceptor set it (and the work queue carried it to the dispatching replica).
func CallerOwner(ctx context.Context) (store.Owner, error) {
	cc, ok := a2asrv.CallContextFrom(ctx)
	if !ok || cc.User == nil || cc.User.Name == "" {
		return store.Owner{}, a2a.ErrUnauthenticated
	}
	buyer, err := uuid.Parse(cc.User.Name)
	if err != nil {
		return store.Owner{}, fmt.Errorf("relay: caller %q is not a buyer id: %w", cc.User.Name, a2a.ErrUnauthenticated)
	}
	o := store.Owner{BuyerID: buyer}
	o.Principal, _ = cc.User.Attributes[AttrPrincipal].(string)
	o.Via, _ = cc.User.Attributes[AttrVia].(string)
	if t, _ := cc.User.Attributes[AttrTenant].(string); t != "" {
		if tid, err := uuid.Parse(t); err == nil {
			o.TenantID = &tid
		}
	}
	if o.Principal == "" || o.Via == "" {
		return store.Owner{}, fmt.Errorf("relay: caller without principal or via: %w", a2a.ErrUnauthenticated)
	}
	return o, nil
}

// newTaskFromStart wraps the puller provider so that the reader of the task a SendMessage itself created
// reads that task's log from its first row. a2a-go creates a reader before the work queue's Write, at the
// log's tail; when Write answers another task id (the gate task takes the id Admission pre-minted, §13.1),
// a2a-go creates the reader again after Write, and a turn that already ended (a fast upstream at
// input-required) would leave that reader waiting forever. Every row of a task created by this request is
// of this turn, so reading from the start is what the tail would have been; a2a-go's subscription skips
// what its snapshot holds, and finishedTurn answers the stored task if nothing is left (P15).
func newTaskFromStart(pp eventqueue.PullerProvider) eventqueue.PullerProvider {
	return func(ctx context.Context, taskID a2a.TaskID) (eventqueue.Puller, error) {
		p, err := pp(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if adm, ok := hooks.AdmittedFrom(ctx); ok && adm.TaskID == string(taskID) {
			return fromStart{p}, nil
		}
		return p, nil
	}
}

// fromStart turns the first (nil) cursor into the log's start (the store's Puller takes an int64 seq).
type fromStart struct{ eventqueue.Puller }

func (p fromStart) Pull(ctx context.Context, taskID a2a.TaskID, cursor eventqueue.PullCursor) (*eventqueue.PullResponse, error) {
	if cursor == nil {
		cursor = int64(0)
	}
	return p.Puller.Pull(ctx, taskID, cursor)
}
