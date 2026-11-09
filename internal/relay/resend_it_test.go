package relay

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// resendRig is the relay over the real lifecycle (Temporal, Postgres) for one service, as the gate wires it: the
// interceptors (with Admission admit, and the service's work queue as their StartReissuer), the Temporal-backed work
// queue, the workers, and a seller that asks for input once.
type resendRig struct {
	t      *testing.T
	log    *store.Log
	svc    store.ServiceRef
	c      client.Client
	lc     *lifecycle.Client
	agent  *upstreamAgent
	h      a2asrv.RequestHandler
	deps   HandlerDeps
	admit  hooks.Admission
	queues lifecycle.QueueOptions
	// backends serves the seller's backend and card Info to dispatch (a test may change what the card declares).
	backends *staticBackends
}

func newResendRig(t *testing.T, admit hooks.Admission) *resendRig {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	log := store.NewLog(pool)
	tenant := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	for _, q := range []string{
		`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source, retries) values ($1, 'tck', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 2)`,
	} {
		if _, err := pool.Exec(ctx, q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	r := &resendRig{
		t: t, log: log, admit: admit,
		svc: store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "tck", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour},
		c:   temporaltest.Client(t),
	}
	r.lc = lifecycle.NewClient(r.c)
	r.queues = lifecycle.QueueOptions{Owner: CallerOwner, Hooks: lifecycle.HooksSnapshot{LifecycleQueue: lifecycle.QueueLifecycle}}
	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	r.agent = agent
	backends := &staticBackends{}
	backends.set(b, info, nil)
	r.backends = backends
	r.deps = HandlerDeps{
		Log: log, Puller: store.NewPullerProvider(log, runListener(t, dsn)), Backends: backends, Signals: r.lc,
		Executor: ExecutorOptions{PollInterval: 50 * time.Millisecond},
	}
	reg := NewRegistry()
	handlers := r.handlers(r.lc, reg)
	disp := &Dispatcher{ReplicaID: "replica-it", Log: log, Registry: reg, Handlers: handlers, Backends: backends}
	w, err := lifecycle.NewWorkers(r.c, lifecycle.WorkerConfig{ReplicaID: "replica-it", DrainTimeout: time.Second},
		lifecycle.Activities{Lifecycle: &lifecycle.LifecycleActivities{Log: log, Backends: directOnly{}}, Dispatch: disp})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	r.h = r.handler(handlers)
	return r
}

// handlers builds the service's handlers over lifecycle client lc and registry reg: the work queue and the
// interceptors' StartReissuer are the service's lifecycle.Queue, as in internal/gate.
func (r *resendRig) handlers(lc *lifecycle.Client, reg *Registry) *Handlers {
	deps := r.deps
	deps.Registry = reg
	deps.Queue = func(s store.ServiceRef) workqueue.Queue { return lifecycle.NewQueue(s, r.log, lc, r.queues) }
	deps.Interceptors = func(s store.ServiceRef) []a2asrv.CallInterceptor {
		i := &Interceptors{Svc: s, Log: r.log, Admit: r.admit, Canceler: lc}
		return i.Chain()
	}
	return NewHandlers(deps)
}

func (r *resendRig) handler(hs *Handlers) a2asrv.RequestHandler {
	r.t.Helper()
	sh, err := hs.For(context.Background(), r.svc)
	if err != nil {
		r.t.Fatal(err)
	}
	return sh.Handler
}

// request is the TCK's prerequisite: the same messageId and text every time.
func resendRequest(id, text string) *a2a.SendMessageRequest {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ID = id
	return &a2a.SendMessageRequest{Message: m}
}

// send is a blocking SendMessage on h bounded by 10 s, returning the task and how long it took.
func (r *resendRig) send(h a2asrv.RequestHandler, what string, req *a2a.SendMessageRequest) (*a2a.Task, time.Duration) {
	r.t.Helper()
	cctx, cancel := context.WithTimeout(asCaller(buyerA), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := h.SendMessage(cctx, req)
	took := time.Since(start)
	if err != nil {
		r.t.Fatalf("%s: SendMessage after %v: %v", what, took.Round(time.Millisecond), err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		r.t.Fatalf("%s: SendMessage answered %T, want a task", what, res)
	}
	return task, took
}

// TestBlockingResendAtTurnEnd (D290; found by the pinned A2A TCK in M4 WP-02, whose message ids are per session,
// so its prerequisite input-required tasks after the first are resends): a blocking SendMessage that repeats a
// request whose task already stands at input-required is answered at once with that task, as it stands (spec §9,
// §15), instead of waiting at the log's tail for a turn that ended before it. The seller sees the message once; the
// same messageId with another request is a new task; a resend of the task once it completed is answered completed.
// Before the fix the second SendMessage ran into its 10 s deadline.
func TestBlockingResendAtTurnEnd(t *testing.T) {
	r := newResendRig(t, nil)
	const id = "tck-input-required-0a1b2c3d"

	first, _ := r.send(r.h, "the first request", resendRequest(id, "book"))
	if first.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the first request: %s, want input-required", first.Status.State)
	}
	for n := 1; n <= 2; n++ {
		again, took := r.send(r.h, "a resend", resendRequest(id, "book"))
		if again.ID != first.ID || again.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("resend %d: task %s %s, want %s input-required", n, again.ID, again.Status.State, first.ID)
		}
		if took > 3*time.Second {
			t.Errorf("resend %d answered after %v: a task at input-required is answered at once", n, took.Round(time.Millisecond))
		}
		if len(again.History) == 0 || again.History[0].ID != id {
			t.Errorf("resend %d: history %+v, want the task's own, the first message first", n, again.History)
		}
	}
	if sent := r.agent.sentMessages(t); len(sent) != 1 {
		t.Fatalf("the seller received %d messages, want 1: a resend is never dispatched again", len(sent))
	}

	// The same messageId with another request is a new task (spec §15: the request hash is the key too).
	other, _ := r.send(r.h, "another request", resendRequest(id, "book two"))
	if other.ID == first.ID || other.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("another request with the same messageId: task %s %s, want a new task at input-required", other.ID, other.Status.State)
	}

	// Once the follow-up completed the task, a resend of its first request is answered completed, at once.
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blue"))
	m.TaskID = first.ID
	if done, _ := r.send(r.h, "the follow-up", &a2a.SendMessageRequest{Message: m}); done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up: %s, want completed", done.Status.State)
	}
	again, took := r.send(r.h, "a resend of a completed task", resendRequest(id, "book"))
	if again.ID != first.ID || again.Status.State != a2a.TaskStateCompleted || took > 3*time.Second {
		t.Fatalf("a resend of the completed task: %s %s after %v", again.ID, again.Status.State, took.Round(time.Millisecond))
	}
	if sent := r.agent.sentMessages(t); len(sent) != 3 {
		t.Errorf("the seller received %d messages, want 3 (two first turns and one follow-up)", len(sent))
	}
}

func mustHash(t *testing.T, req *a2a.SendMessageRequest) [32]byte {
	t.Helper()
	h, err := store.RequestHashOf(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
