package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

type directOnly struct{}

func (directOnly) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	return store.BackendDirect, nil
}

// TestTurnsOverTheLifecycle runs the relay over the real lifecycle: a2a-go's handler with the gate's
// interceptors, the Temporal-backed work queue, TaskLifecycle on a development server, the dispatch
// activity (this package's Dispatcher) and an a2a-go seller that asks for input once (spec §7, §9, §14.1;
// the audit's "follow-up turn through the real stack"). Then a cancel of an input-required task through
// the cancel interceptor and kontra.cancel (spec §8), and a message to a finished task through the review
// path (spec §7).
func TestTurnsOverTheLifecycle(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	log := store.NewLog(pool)
	tenant := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	for _, q := range []string{
		`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source, retries) values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 2)`,
	} {
		if _, err := pool.Exec(ctx, q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	svc := store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
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

	// Turn 1 asks for input; the blocking SendMessage answers at input-required.
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("book"))})
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("turn 1: %s", task.Status.State)
	}
	// The follow-up goes to the same upstream task, on the same attempt, and completes.
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blue"))
	m.TaskID = task.ID
	res, err = h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatal(err)
	}
	if final := res.(*a2a.Task); final.Status.State != a2a.TaskStateCompleted || final.Status.Message.Parts[0].Text() != "done: blue" {
		t.Fatalf("turn 2: %+v", final.Status)
	}
	id := uid(t, task.ID)
	a, err := log.GetAttempt(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	sent := agent.sentMessages(t)
	if len(sent) != 2 || sent[1].TaskID != a2a.TaskID(a.UpstreamTaskID) || sent[1].ContextID != a.UpstreamContextID {
		t.Fatalf("upstream received %+v", sent)
	}
	var unconsumed int
	if err := pool.QueryRow(ctx, `select count(*) from kontra.work_items where task_id = $1 and consumed_at is null`, id).Scan(&unconsumed); err != nil || unconsumed != 0 {
		t.Errorf("%d work items not consumed: each turn's first dispatch consumes its item (D66)", unconsumed)
	}
	// The workflow ends after the dispatch returned, which ends the attempt first.
	waitClosed(t, c, task.ID)
	if a, _ = log.GetAttempt(ctx, id, 1); a.EndedAt == nil || a.Outcome != store.OutcomeCompleted {
		t.Errorf("attempt %+v: ended completed by the follow-up turn", a)
	}
	// The workflow has ended: a message to the completed task is refused through the review path.
	m3 := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again"))
	m3.TaskID = task.ID
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m3}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("a message to a completed task: %v", err)
	}
	var items int
	_ = pool.QueryRow(ctx, `select count(*) from kontra.work_items where task_id = $1`, id).Scan(&items)
	if items != 2 {
		t.Errorf("%d work items: the refused review message's item must be deleted", items)
	}

	// Cancel of an input-required task: forward_cancel to the seller, then canceled in the log, before
	// CancelTask answers (spec §8, D79).
	res, err = h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("book again"))})
	if err != nil {
		t.Fatal(err)
	}
	t2 := res.(*a2a.Task)
	if t2.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task 2: %s", t2.Status.State)
	}
	canceled, err := h.CancelTask(asCaller(buyerA), &a2a.CancelTaskRequest{ID: t2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status.State != a2a.TaskStateCanceled || canceled.Metadata[store.MetaCanceled] != store.CanceledClient {
		t.Fatalf("CancelTask answered %s %v", canceled.Status.State, canceled.Metadata)
	}
	if got, _ := h.GetTask(asCaller(buyerA), &a2a.GetTaskRequest{ID: t2.ID}); got.Status.State != a2a.TaskStateCanceled {
		t.Errorf("GetTask right after CancelTask: %s", got.Status.State)
	}
	cancels := 0
	for _, r := range agent.recorded() {
		if r.method == "CancelTask" {
			cancels++
		}
	}
	if cancels != 1 {
		t.Errorf("the seller received %d CancelTask", cancels)
	}
	a2, _ := log.GetAttempt(ctx, uid(t, t2.ID), 1)
	if a2.Outcome != store.OutcomeInterrupted {
		t.Errorf("attempt outcome %q, want interrupted (D77)", a2.Outcome)
	}
	if _, err := h.CancelTask(asCaller(buyerA), &a2a.CancelTaskRequest{ID: t2.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Errorf("a second cancel: %v", err)
	}
}

// waitClosed waits for the task's workflow to complete: after OnTerminal and ReviewWindow {open: false}.
func waitClosed(t *testing.T, c client.Client, id a2a.TaskID) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		d, err := c.DescribeWorkflowExecution(context.Background(), lifecycle.WorkflowID(id), "")
		if err == nil && d.WorkflowExecutionInfo.Status == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the workflow of task %s did not complete: %v", id, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
