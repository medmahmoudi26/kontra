package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medmahmoudi26/kontra/internal/pg"
	"github.com/medmahmoudi26/kontra/internal/pgtest"
)

// runListener starts the replica's LISTEN connection on dsn for the test.
func runListener(t *testing.T, dsn string) *pg.Listener {
	t.Helper()
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	l := pg.NewListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for !l.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("the listener did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l
}

// countingWaker records registrations and releases.
type countingWaker struct {
	mu                  sync.Mutex
	registered, release int
	ch                  chan struct{}
}

func (w *countingWaker) WaitTask(ctx context.Context, taskID uuid.UUID) (<-chan struct{}, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.registered++
	var once sync.Once
	return w.ch, func() {
		once.Do(func() {
			w.mu.Lock()
			w.release++
			w.mu.Unlock()
		})
	}
}

// The puller starts at the creation tail, skips message and metadata rows (advancing its cursor past
// them), and sets TaskVersion = seq (spec §15).
func TestPullerTailSkipsAndVersions(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "q")
	id := uid(t, task.ID)
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateInputRequired, "which?")) // 2: an old final event
	mustApply(t, ctx, s, task.ID, userMessage(task, "this one"))                      // 3
	if _, err := e.log.ApplyMetadata(ctx, id, map[string]any{"k": 1}); err != nil {   // 4
		t.Fatal(err)
	}
	w := &countingWaker{ch: make(chan struct{}, 1)}
	p, err := newPullerProvider(e.log, w, 300*time.Millisecond)(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, ""))           // 5
	mustApply(t, ctx, s, task.ID, userMessage(task, "more"))                        // 6
	mustApply(t, ctx, s, task.ID, artifact(task, "a1", false, "out"))               // 7
	if _, err := e.log.ApplyMetadata(ctx, id, map[string]any{"k": 2}); err != nil { // 8
		t.Fatal(err)
	}
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateCompleted, "ok")) // 9

	resp, err := p.Pull(ctx, task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range resp.Messages {
		got = append(got, fmt.Sprintf("%d:%T", m.TaskVersion, m.Event))
		if m.Protocol != a2a.Version {
			t.Errorf("protocol %q", m.Protocol)
		}
	}
	want := "5:*a2a.TaskStatusUpdateEvent 7:*a2a.TaskArtifactUpdateEvent 9:*a2a.TaskStatusUpdateEvent"
	if strings.Join(got, " ") != want {
		t.Errorf("pulled %v, want %s", got, want)
	}
	if resp.Cursor != int64(9) {
		t.Errorf("cursor %v, want 9", resp.Cursor)
	}
	if st := resp.Messages[2].Event.(*a2a.TaskStatusUpdateEvent); st.Status.State != a2a.TaskStateCompleted || st.Status.Message.Parts[0].Text() != "ok" {
		t.Errorf("decoded event %+v", st)
	}

	// Nothing new: the pull waits (here 300 ms), reads once more and returns the cursor unchanged.
	start := time.Now()
	resp, err = p.Pull(ctx, task.ID, int64(9))
	if err != nil || len(resp.Messages) != 0 || resp.Cursor != int64(9) {
		t.Fatalf("an empty pull: %+v %v", resp, err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("an empty pull returned after %v without waiting", d)
	}
	// A cursor past rows that are all skipped advances without returning anything.
	resp, err = p.Pull(ctx, task.ID, int64(5))
	if err != nil || resp.Cursor != int64(9) || len(resp.Messages) != 2 {
		t.Errorf("pull after 5: %+v %v", resp, err)
	}
	if _, err := p.Pull(ctx, task.ID, "5"); err == nil {
		t.Error("a foreign cursor was accepted")
	}
	if _, err := p.Pull(ctx, a2a.TaskID(uuid.Must(uuid.NewV7()).String()), nil); err == nil {
		t.Error("a puller served another task")
	}
	// A canceled pull returns at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Pull(cctx, task.ID, int64(9)); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled pull: %v", err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if w.registered != 1 || w.release != 1 {
		t.Errorf("waker: %d registrations, %d releases", w.registered, w.release)
	}
	if _, err := NewPullerProvider(e.log, nil)(ctx, "not-a-task"); !isNotFound(err) {
		t.Errorf("a provider for a malformed id: %v", err)
	}
}

// The puller long-polls on the LISTEN connection and holds no pool connection while it waits.
func TestPullerLongPollHoldsNoConnection(t *testing.T) {
	e := newEnv(t)
	listener := runListener(t, e.dsn)
	sep := "?"
	if strings.Contains(e.dsn, "?") {
		sep = "&"
	}
	small := pgtest.Pool(t, e.dsn+sep+"pool_max_conns=2")
	log := NewLog(small)
	s := New(small, e.svc, ctxOwner, nil, log)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "q")
	p, err := NewPullerProvider(log, listener)(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	type result struct {
		resp *eventqueue.PullResponse
		err  error
		at   time.Time
	}
	done := make(chan result, 1)
	go func() {
		var cursor eventqueue.PullCursor
		for { // as a2a-go's poll loop: an empty pull is followed by another
			resp, err := p.Pull(ctx, task.ID, cursor)
			if err != nil || len(resp.Messages) > 0 {
				done <- result{resp, err, time.Now()}
				return
			}
			cursor = resp.Cursor
		}
	}()
	// While the pull waits, no pool connection is acquired.
	for range 20 {
		time.Sleep(20 * time.Millisecond)
		if n := small.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("a waiting pull holds %d pool connections", n)
		}
		select {
		case r := <-done:
			t.Fatalf("the pull returned before any write: %+v", r)
		default:
		}
	}
	wrote := time.Now()
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, ""))
	select {
	case r := <-done:
		if r.err != nil || len(r.resp.Messages) != 1 || r.resp.Messages[0].TaskVersion != 2 {
			t.Fatalf("pull: %+v %v", r.resp, r.err)
		}
		if lag := r.at.Sub(wrote); lag > 2*time.Second {
			t.Errorf("the pull woke %v after the write (the NOTIFY should wake it)", lag)
		}
	case <-time.After(PullWait - time.Second):
		t.Fatal("the NOTIFY did not wake the pull")
	}
}

// Database errors are backed off inside Pull, which a2a-go retries every PollInterval without a backoff.
func TestPullerBacksOff(t *testing.T) {
	e := newEnv(t)
	task := e.newTask(e.svc, buyerA, "", "q")
	closed := pgtest.Pool(t, e.dsn)
	log := NewLog(closed)
	p, err := newPullerProvider(log, nil, time.Second)(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	var took []time.Duration
	for range 3 {
		start := time.Now()
		if _, err := p.Pull(context.Background(), task.ID, nil); err == nil {
			t.Fatal("a pull on a closed pool succeeded")
		}
		took = append(took, time.Since(start))
	}
	if took[0] < 90*time.Millisecond || took[2] < took[0]+200*time.Millisecond {
		t.Errorf("no growing backoff: %v", took)
	}
}

// authInterceptor authenticates every call as one user (the gate's auth interceptor's place).
type authInterceptor struct{ name string }

func (a authInterceptor) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	cc.User = &a2asrv.User{Name: a.name, Authenticated: true}
	return ctx, nil, nil
}
func (authInterceptor) After(context.Context, *a2asrv.CallContext, *a2asrv.Response) error {
	return nil
}

// callerOwner maps the call context's user to an owner, as the gate's OwnerFunc does.
func callerOwner(ctx context.Context) (Owner, error) {
	cc, ok := a2asrv.CallContextFrom(ctx)
	if !ok || cc.User == nil {
		return Owner{}, a2a.ErrUnauthenticated
	}
	switch cc.User.Name {
	case "buyer-a":
		return buyerA, nil
	case "buyer-b":
		return buyerB, nil
	}
	return Owner{}, a2a.ErrUnauthenticated
}

// fakeTemporal is the probes' work queue: Write pre-creates the gate task with its work item (as the
// Temporal-backed queue will), and the HandlerFn runs in a goroutine on a JSON round trip of the payload,
// as the dispatch activity will.
type fakeTemporal struct {
	t       *testing.T
	log     *Log
	svc     ServiceRef
	handler workqueue.HandlerFn
	mu      sync.Mutex
	done    []chan error
}

func (f *fakeTemporal) write(ctx context.Context, p *workqueue.Payload) (a2a.TaskID, error) {
	if p.Type == workqueue.PayloadTypeExecute && p.ExecuteRequest.Message.TaskID == "" {
		id, err := uuid.Parse(string(p.TaskID))
		if err != nil {
			return "", err
		}
		o, err := callerOwner(ctx)
		if err != nil {
			return "", err
		}
		msg := *p.ExecuteRequest.Message
		ctxID := msg.ContextID
		if ctxID == "" {
			ctxID = uuid.Must(uuid.NewV7()).String()
		}
		msg.TaskID, msg.ContextID = p.TaskID, ctxID
		snap := &a2a.Task{ID: p.TaskID, ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{&msg}}
		err = pgx.BeginFunc(ctx, f.log.DB(), func(tx pgx.Tx) error {
			if _, err := f.log.CreateTask(ctx, tx, NewTask{ID: id, Service: f.svc, Protocol: ProtocolA2A, Owner: o,
				ContextID: ctxID, State: StateSubmitted, Snapshot: snap}); err != nil {
				return err
			}
			return f.log.InsertWorkItem(ctx, tx, WorkItem{ID: uuid.Must(uuid.NewV7()), TaskID: id, Kind: WorkExecute,
				Payload: WorkPayload{Request: p.ExecuteRequest, Owner: o}})
		})
		if err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var cp workqueue.Payload
	if err := json.Unmarshal(raw, &cp); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	f.mu.Lock()
	f.done = append(f.done, done)
	f.mu.Unlock()
	go func() {
		_, err := f.handler(context.Background(), &cp)
		done <- err
	}()
	return cp.TaskID, nil
}

func (f *fakeTemporal) last() chan error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done[len(f.done)-1]
}

func newCluster(t *testing.T, e *env, exec a2asrv.AgentExecutor, guard FailureGuard) (a2asrv.RequestHandler, *fakeTemporal) {
	t.Helper()
	listener := runListener(t, e.dsn)
	store := New(e.pool, e.svc, callerOwner, guard, e.log)
	qm := eventqueue.NewPullQueueManager(NewPullerProvider(e.log, listener), eventqueue.PullConfig{PollInterval: 100 * time.Millisecond})
	ft := &fakeTemporal{t: t, log: e.log, svc: e.svc}
	q, hfn := workqueue.NewPushQueue(workqueue.WriterFunc(ft.write))
	ft.handler = hfn
	h := a2asrv.NewHandler(exec,
		a2asrv.WithClusterMode(a2asrv.ClusterConfig{QueueManager: qm, WorkQueue: q, TaskStore: store}),
		a2asrv.WithCallInterceptors(authInterceptor{name: "buyer-a"}))
	return h, ft
}

// The a2a-go probes P7/P7b/P7c on Postgres (a2a-go brief C11): behind a real cluster-mode handler, a
// two-turn input-required task completes, and turn 2 returns the task, not the buyer's own message.
func TestClusterTwoTurnTask(t *testing.T) {
	e := newEnv(t)
	turns := 0
	var sawHistory int
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			turns++
			if ec.StoredTask == nil {
				yield(nil, errors.New("the work queue pre-creates every task"))
				return
			}
			if ec.StoredTask.Status.State == a2a.TaskStateSubmitted {
				if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
					return
				}
				q := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, q), nil)
				return
			}
			sawHistory = len(ec.StoredTask.History)
			time.Sleep(50 * time.Millisecond)
			ans := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+ec.Message.Parts[0].Text()))
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, ans), nil)
		}
	})
	h, ft := newCluster(t, e, exec, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("turn 1"))})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("turn 1 answered %T %+v", res, res)
	}
	if err := <-ft.last(); err != nil {
		t.Fatalf("turn 1 handler: %v", err)
	}
	// An operator patch between the turns: a metadata row the puller must skip.
	if _, err := e.log.ApplyMetadata(ctx, uid(t, task.ID), map[string]any{"note": "between turns"}); err != nil {
		t.Fatal(err)
	}
	follow := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("the blue one"))
	follow.TaskID = task.ID
	res, err = h.SendMessage(ctx, &a2a.SendMessageRequest{Message: follow})
	if err != nil {
		t.Fatal(err)
	}
	final, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("turn 2 answered %T (the buyer's own message?): %+v", res, res)
	}
	if final.Status.State != a2a.TaskStateCompleted || final.Status.Message.Parts[0].Text() != "done: the blue one" {
		t.Fatalf("turn 2: %+v", final.Status)
	}
	if err := <-ft.last(); err != nil {
		t.Fatalf("turn 2 handler: %v", err)
	}
	if turns != 2 || sawHistory != 2 { // the first message and the follow-up; turn 1's question is the status message
		t.Errorf("turns %d, history seen by turn 2: %d", turns, sawHistory)
	}
	_, kinds := e.seqs(task.ID)
	want := []EventKind{KindTask, KindStatus, KindStatus, KindMetadata, KindMessage, KindStatus}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("log %v, want %v", kinds, want)
	}
	got, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
	if err != nil || got.Status.State != a2a.TaskStateCompleted || got.Metadata["note"] != "between turns" {
		t.Errorf("GetTask %+v %v", got, err)
	}
}

// D8 end to end: an executor error while the dispatch is registered makes a2a-go try SetTaskFailed; the
// store refuses it, the task stays as it was, and the error reaches the HandlerFn (whose caller, the
// dispatch activity, returns the registry's outcome instead).
func TestClusterRefusesSynthesizedFailure(t *testing.T) {
	e := newEnv(t)
	g := &fakeGuard{registered: map[a2a.TaskID]bool{}, recorded: map[a2a.Event]bool{}}
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			g.mu.Lock()
			g.registered[ec.TaskID] = true
			g.mu.Unlock()
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			yield(nil, errors.New("upstream unreachable before any upstream task id"))
		}
	})
	h, ft := newCluster(t, e, exec, g)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q")),
		Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	if herr := <-ft.last(); herr == nil {
		t.Fatal("the handler hid the executor's error")
	}
	g.mu.Lock()
	denied := g.denied
	g.mu.Unlock()
	if denied == 0 {
		t.Error("a2a-go never tried SetTaskFailed, or the guard was not asked")
	}
	row, err := e.log.Load(ctx, uid(t, task.ID))
	if err != nil {
		t.Fatal(err)
	}
	// The working status may or may not have been processed before the producer's error canceled the
	// consumer; either way no failed status reached the log.
	if row.State != StateWorking && row.State != StateSubmitted {
		t.Errorf("the task is %s at v%d; a2a-go's failure reached the log", row.State, row.Version)
	}
	evs, _ := e.log.Events(ctx, uid(t, task.ID), 0, 100)
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), string(a2a.TaskStateFailed)) {
			t.Errorf("row %d is a failure: %s", ev.Seq, ev.Payload)
		}
	}
}
