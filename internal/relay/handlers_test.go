package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
)

// The handler For returns is always built from the ServiceRef its caller passed. A caller that read the
// service before a reload and calls For after the registry's Invalidate (an A2A request between Lookup
// and For, a dispatch between LoadService and For) gets a handler of the old policy, but that handler
// must not stay cached for the callers that read the service afterwards: their work queues would give
// every new task the old task.timeout and task.retries until the service changed again.
func TestHandlersNeverServeAStaleServiceRef(t *testing.T) {
	var mu sync.Mutex
	var built []store.ServiceRef // what each work queue was built from (lifecycle.NewQueue: task.timeout, retries)
	h := NewHandlers(HandlerDeps{
		Log: store.NewLog(nil),
		Puller: func(context.Context, a2a.TaskID) (eventqueue.Puller, error) {
			return nil, nil
		},
		Queue: func(svc store.ServiceRef) workqueue.Queue {
			mu.Lock()
			built = append(built, svc)
			mu.Unlock()
			q, _ := workqueue.NewPushQueue(workqueue.WriterFunc(func(context.Context, *workqueue.Payload) (a2a.TaskID, error) {
				return "", nil
			}))
			return q
		},
		Registry: NewRegistry(),
		Backends: &staticBackends{},
	})
	ctx := context.Background()
	old := store.ServiceRef{TenantID: uuid.Must(uuid.NewV7()), Tenant: "default", Name: "hello", Kind: "a2a",
		Timeout: 10 * time.Minute, Retries: 1, Retention: time.Hour}
	cur := old
	cur.Timeout, cur.Retries = 20*time.Minute, 3

	first, err := h.For(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := h.For(ctx, old); again != first {
		t.Fatal("the same ServiceRef built a second handler")
	}
	// The reload: the registry swaps the service, then drops the cached handler.
	h.Invalidate(cur)
	// A caller that read the service before the reload.
	stale, err := h.For(ctx, old)
	if err != nil || stale.Service != old {
		t.Fatalf("For(old ref) after Invalidate: %+v %v", stale, err)
	}
	// Every caller that reads the service afterwards gets the current policy.
	got, err := h.For(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	if got.Service != cur {
		t.Fatalf("For(current ref) answered the handler of %v, want %v", got.Service, cur)
	}
	mu.Lock()
	last := built[len(built)-1]
	mu.Unlock()
	if last != cur {
		t.Fatalf("the work queue of the current handler was built from %+v", last)
	}
	if again, _ := h.For(ctx, cur); again != got {
		t.Fatal("the current handler is not cached")
	}
}
