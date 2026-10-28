package relay

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// TestBlockingResendAtTurnEnd (DM4-23; found by the pinned A2A TCK in M4 WP-02, whose message ids are per session,
// so its prerequisite input-required tasks after the first are resends): a blocking SendMessage that repeats a
// request whose task already stands at input-required is answered at once with that task, as it stands (spec §9,
// §15), instead of waiting at the log's tail for a turn that ended before it. The seller sees the message once; the
// same messageId with another request is a new task; a resend of the task once it completed is answered completed.
// Before the fix the second SendMessage ran into its 10 s deadline.
func TestBlockingResendAtTurnEnd(t *testing.T) {
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
	svc := store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "tck", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	listener := runListener(t, dsn)
	c := temporaltest.Client(t)
	lc := lifecycle.NewClient(c)

	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	backends := &staticBackends{}
	backends.set(b, info, nil)
	reg := NewRegistry()
	intc := &Interceptors{Svc: svc, Log: log, Canceler: lc, Reviews: lc}
	handlers := NewHandlers(HandlerDeps{
		Log: log, Puller: store.NewPullerProvider(log, listener),
		Queue: func(s store.ServiceRef) workqueue.Queue {
			return lifecycle.NewQueue(s, log, lc, lifecycle.QueueOptions{Owner: CallerOwner, Hooks: lifecycle.HooksSnapshot{LifecycleQueue: lifecycle.QueueLifecycle}})
		},
		Interceptors: func(store.ServiceRef) []a2asrv.CallInterceptor { return intc.Chain() },
		Registry:     reg, Backends: backends, Signals: lc, Executor: ExecutorOptions{PollInterval: 50 * time.Millisecond},
	})
	disp := &Dispatcher{ReplicaID: "replica-it", Log: log, Registry: reg, Handlers: handlers, Backends: backends}
	w, err := lifecycle.NewWorkers(c, lifecycle.WorkerConfig{ReplicaID: "replica-it", DrainTimeout: time.Second},
		lifecycle.Activities{Lifecycle: &lifecycle.LifecycleActivities{Log: log, Backends: directOnly{}}, Dispatch: disp})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	sh, err := handlers.For(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	h := sh.Handler

	// request is the TCK's prerequisite: the same messageId and text every time.
	request := func(text string) *a2a.SendMessageRequest {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
		m.ID = "tck-input-required-0a1b2c3d"
		return &a2a.SendMessageRequest{Message: m}
	}
	// send is a blocking SendMessage bounded by 10 s, returning the task and how long it took.
	send := func(what string, req *a2a.SendMessageRequest) (*a2a.Task, time.Duration) {
		t.Helper()
		cctx, cancel := context.WithTimeout(asCaller(buyerA), 10*time.Second)
		defer cancel()
		start := time.Now()
		res, err := h.SendMessage(cctx, req)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("%s: SendMessage after %v: %v", what, took.Round(time.Millisecond), err)
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			t.Fatalf("%s: SendMessage answered %T, want a task", what, res)
		}
		return task, took
	}

	first, _ := send("the first request", request("book"))
	if first.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the first request: %s, want input-required", first.Status.State)
	}
	for n := 1; n <= 2; n++ {
		again, took := send("a resend", request("book"))
		if again.ID != first.ID || again.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("resend %d: task %s %s, want %s input-required", n, again.ID, again.Status.State, first.ID)
		}
		if took > 3*time.Second {
			t.Errorf("resend %d answered after %v: a task at input-required is answered at once", n, took.Round(time.Millisecond))
		}
		if len(again.History) == 0 || again.History[0].ID != "tck-input-required-0a1b2c3d" {
			t.Errorf("resend %d: history %+v, want the task's own, the first message first", n, again.History)
		}
	}
	if sent := agent.sentMessages(t); len(sent) != 1 {
		t.Fatalf("the seller received %d messages, want 1: a resend is never dispatched again", len(sent))
	}

	// The same messageId with another request is a new task (spec §15: the request hash is the key too).
	other, _ := send("another request", request("book two"))
	if other.ID == first.ID || other.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("another request with the same messageId: task %s %s, want a new task at input-required", other.ID, other.Status.State)
	}

	// Once the follow-up completed the task, a resend of its first request is answered completed, at once.
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blue"))
	m.TaskID = first.ID
	if done, _ := send("the follow-up", &a2a.SendMessageRequest{Message: m}); done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up: %s, want completed", done.Status.State)
	}
	again, took := send("a resend of a completed task", request("book"))
	if again.ID != first.ID || again.Status.State != a2a.TaskStateCompleted || took > 3*time.Second {
		t.Fatalf("a resend of the completed task: %s %s after %v", again.ID, again.Status.State, took.Round(time.Millisecond))
	}
	if sent := agent.sentMessages(t); len(sent) != 3 {
		t.Errorf("the seller received %d messages, want 3 (two first turns and one follow-up)", len(sent))
	}
}
