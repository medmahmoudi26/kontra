package relay

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"

	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
	"github.com/medmahmoudi26/kontra/internal/upstream"
)

// Backends resolves a service's backend and upstream card info for dispatch and forward_cancel.
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
	PushStore    a2apush.ConfigStore
	Queue        func(svc store.ServiceRef) workqueue.Queue // lifecycle.NewQueue
	Interceptors func(svc store.ServiceRef) []a2asrv.CallInterceptor
	Registry     *Registry
	Backends     Backends
	Owner        store.OwnerFunc
}

// Handlers builds each service's handler on first use and caches it (spec §15): a2a.NewHandler with
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

// For returns svc's handler, building it on first use.
func (h *Handlers) For(ctx context.Context, svc store.ServiceRef) (*ServiceHandler, error) {
	return nil, unimpl.Error("relay.Handlers.For", "WP-04")
}

// Invalidate drops svc's cached handler.
func (h *Handlers) Invalidate(svc store.ServiceRef) {
	panic(unimpl.Error("relay.Handlers.Invalidate", "WP-04"))
}
