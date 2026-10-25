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
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/push"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/upstream"
)

// Backends resolves a service's backend and upstream card info for dispatch and forward_cancel. The
// service registry implements it over each direct service's card.Keeper (Keeper.Dispatch): a service
// without a usable card answers an error, and the dispatch fails service_unavailable (spec §6.4).
type Backends interface {
	Backend(ctx context.Context, svc store.ServiceRef) (upstream.Backend, upstream.Info, error)
}

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
}

// PollInterval is the pull queue's poll interval (spec §15). The puller long-polls on LISTEN, so this only
// bounds the latency after a burst.
const PollInterval = 100 * time.Millisecond

// Handlers builds each service's handler on first use and caches it (spec §15): a2asrv.NewHandler with
// WithClusterMode{TaskStore, QueueManager: eventqueue.NewPullQueueManager(Puller, {PollInterval: 100 ms,
// InactivityTimeout: 0}), WorkQueue}, WithPushNotifications(PushStore, push.NoopSender) (push is the
// lifecycle's, §10), the interceptors, and no input-mode enforcer (D14). A config or NOTIFY
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

// For returns svc's handler, building it on first use.
func (h *Handlers) For(ctx context.Context, svc store.ServiceRef) (*ServiceHandler, error) {
	if svc.TenantID == uuid.Nil || svc.Name == "" {
		return nil, errors.New("relay: a handler needs its service")
	}
	key := cacheKey(svc)
	h.mu.Lock()
	defer h.mu.Unlock()
	if sh, ok := h.cache[key]; ok {
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
	opts := []a2asrv.RequestHandlerOption{
		a2asrv.WithClusterMode(a2asrv.ClusterConfig{TaskStore: st, QueueManager: qm, WorkQueue: q}),
	}
	if d.Logger != nil {
		opts = append(opts, a2asrv.WithLogger(d.Logger))
	}
	if d.PushStore != nil {
		opts = append(opts, a2asrv.WithPushNotifications(d.PushStore, push.NoopSender{}))
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
