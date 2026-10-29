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
		i := &Interceptors{Svc: s, Log: r.log, Admit: r.admit, Canceler: lc, Reviews: lc, Starts: lifecycle.NewQueue(s, r.log, lc, r.queues)}
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

// lostStarts is a Temporal client whose UpdateWithStartWorkflow never reaches the server: the work queue's start of
// a new task is lost after its row and work item committed (spec §14.5).
type lostStarts struct{ client.Client }

func (lostStarts) UpdateWithStartWorkflow(context.Context, client.UpdateWithStartWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	return nil, errors.New("the start was lost")
}

// TestBlockingResendOfAQuotedTask (D290 as amended by M4 WP-02's review): a quoted task stands at input-required on
// the wire (spec §5.1), and a blocking SendMessage returns on it (§9, A2A §3.1.1), so a resend of the request that
// created it is answered at once with the task as it stands, approve URL and all, as D108 closes the stream of the same
// resend. Before that the re-issued start of a quoted task is re-issued (§14.5): a task whose start was lost gets its
// workflow, waiting for kontra.approval, and the operator's approval then runs it. The seller sees nothing until then.
// Before the fix each resend ran into its 10 s deadline.
func TestBlockingResendOfAQuotedTask(t *testing.T) {
	approve := "https://approve.example.com/t/x"
	adm := &recordingAdmission{decision: hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{
		ApproveURL: approve, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}}
	r := newResendRig(t, adm)
	ctx := context.Background()
	quoted := func(what string, task *a2a.Task, want a2a.TaskID, took time.Duration) {
		t.Helper()
		if want != "" && task.ID != want {
			t.Fatalf("%s: task %s, want %s", what, task.ID, want)
		}
		if task.Status.State != a2a.TaskStateInputRequired || task.Metadata[lifecycle.MetaApproveURL] != approve {
			t.Fatalf("%s: %s %v, want input-required with the approve URL", what, task.Status.State, task.Metadata)
		}
		if took > 3*time.Second {
			t.Errorf("%s answered after %v: a quoted task is answered at once", what, took.Round(time.Millisecond))
		}
	}

	// A quoted task, and two resends of its request.
	first, took := r.send(r.h, "the first request", resendRequest("tck-quoted-0a1b2c3d", "book"))
	quoted("the first request", first, "", took)
	for n := 1; n <= 2; n++ {
		again, took := r.send(r.h, "a resend of the quoted task", resendRequest("tck-quoted-0a1b2c3d", "book"))
		quoted("a resend of the quoted task", again, first.ID, took)
	}

	// A quoted task whose start was lost: its row and work item committed, its workflow never started.
	lost := r.handler(r.handlers(lifecycle.NewClient(lostStarts{r.c}), NewRegistry()))
	cctx, cancel := context.WithTimeout(asCaller(buyerA), 10*time.Second)
	_, err := lost.SendMessage(cctx, resendRequest("tck-lost-0a1b2c3d", "book"))
	cancel()
	if err == nil {
		t.Fatal("the request whose start was lost was answered")
	}
	prev, err := r.log.FindResend(ctx, r.svc, buyerA, "tck-lost-0a1b2c3d", mustHash(t, resendRequest("tck-lost-0a1b2c3d", "book")), time.Now().Add(-time.Hour))
	if err != nil || prev == nil || prev.State != store.StateQuoted {
		t.Fatalf("the task whose start was lost: %+v, %v; want a quoted row", prev, err)
	}
	lostID := a2a.TaskID(prev.ID.String())
	if ok, err := r.lc.WorkflowExists(ctx, lostID); err != nil || ok {
		t.Fatalf("the lost start's workflow exists: %v, %v", ok, err)
	}
	again, took := r.send(r.h, "a resend of the task whose start was lost", resendRequest("tck-lost-0a1b2c3d", "book"))
	quoted("a resend of the task whose start was lost", again, lostID, took)
	if ok, err := r.lc.WorkflowExists(ctx, lostID); err != nil || !ok {
		t.Fatalf("the resend did not re-issue the lost start: workflow exists %v, %v", ok, err)
	}
	if sent := r.agent.sentMessages(t); len(sent) != 0 {
		t.Fatalf("the seller received %d messages before any approval, want 0", len(sent))
	}

	// The re-issued workflow waits for the operator's approval, and runs the task once it has it.
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h, err := r.c.UpdateWorkflow(uctx, client.UpdateWorkflowOptions{
		WorkflowID: lifecycle.WorkflowID(lostID), UpdateName: lifecycle.UpdateApproval,
		Args: []any{lifecycle.ApprovalUpdate{Accepted: true}}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatalf("kontra.approval: %v", err)
	}
	var res lifecycle.ApprovalResult
	if err := h.Get(uctx, &res); err != nil || res.State != "submitted" {
		t.Fatalf("kontra.approval answered %+v, %v; want submitted", res, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		row, err := r.log.Load(ctx, prev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == store.StateInputRequired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the approved task stands at %s, want input-required (the seller asks)", row.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sent := r.agent.sentMessages(t); len(sent) != 1 || sent[0].ID != "tck-lost-0a1b2c3d" {
		t.Fatalf("the seller received %+v, want the approved task's message once", sent)
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
