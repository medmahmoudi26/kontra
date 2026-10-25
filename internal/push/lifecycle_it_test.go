package push

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// TestMain stops the Temporal development server the lifecycle tests started.
func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}

type directOnly struct{}

func (directOnly) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	return store.BackendDirect, nil
}

// scriptedDispatch is the dispatch activity of these tests: instead of an upstream, it runs a script that
// writes the task's log as the relay would, then reports the terminal outcome.
type scriptedDispatch struct {
	log    *store.Log
	script func(ctx context.Context, taskID uuid.UUID) error
}

func (s *scriptedDispatch) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	id, err := uuid.Parse(in.TaskID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	wid, err := uuid.Parse(in.WorkItemID)
	if err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if err := s.log.ConsumeWorkItem(ctx, wid, id); err != nil && !errors.Is(err, store.ErrWorkItemConsumed) {
		return lifecycle.DispatchResult{}, err
	}
	if err := s.script(ctx, id); err != nil {
		return lifecycle.DispatchResult{}, err
	}
	if err := s.log.EndAttempt(ctx, id, in.N, store.OutcomeCompleted); err != nil {
		return lifecycle.DispatchResult{}, err
	}
	return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, nil
}

func (s *scriptedDispatch) Abort(context.Context, lifecycle.AttemptRef) error { return nil }

func (s *scriptedDispatch) ForwardCancel(context.Context, lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelUnsupported}, nil
}

// lifecycleEnv is a database, a Temporal client and the replica's workers (lifecycle, dispatch, and push
// when pushWorker is set).
type lifecycleEnv struct {
	*env
	c  client.Client
	lc *lifecycle.Client
	cs *ConfigStore
}

func newLifecycleEnv(t *testing.T, dispatch *scriptedDispatch, pushOpts *DelivererOptions) *lifecycleEnv {
	t.Helper()
	e := newEnv(t)
	c := temporaltest.Client(t)
	le := &lifecycleEnv{env: e, c: c, lc: lifecycle.NewClient(c)}
	le.cs = NewConfigStore(e.pool, le.lc, private, quiet)
	dispatch.log = e.log
	var push lifecycle.PushActivities
	if pushOpts != nil {
		push = NewDeliverer(e.log, private, *pushOpts)
	}
	w, err := lifecycle.NewWorkers(c, lifecycle.WorkerConfig{ReplicaID: "replica-push-" + uuid.NewString()[:8], DrainTimeout: time.Second},
		lifecycle.Activities{Lifecycle: &lifecycle.LifecycleActivities{Log: e.log, Backends: directOnly{}, Push: le.cs}, Dispatch: dispatch, Push: push})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	return le
}

// start creates a task the way the work queue's Write does (the task row, the inline push config, the work
// item, in one transaction) and starts its workflow with the first message.
func (le *lifecycleEnv) start(inline *a2a.PushConfig) (uuid.UUID, string) {
	le.t.Helper()
	ctx := context.Background()
	var (
		id  uuid.UUID
		cid string
		wid = uuid.Must(uuid.NewV7())
	)
	if err := pgx.BeginFunc(ctx, le.pool, func(tx pgx.Tx) error {
		id = le.newTaskTx(tx, buyerA)
		if inline != nil {
			var err error
			if cid, _, err = le.cs.SaveInline(ctx, tx, id, buyerA, inline); err != nil {
				return err
			}
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
		msg.TaskID = a2a.TaskID(id.String())
		return le.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
			Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, Owner: buyerA}})
	}); err != nil {
		le.t.Fatal(err)
	}
	in := lifecycle.Input{TaskID: id.String(), TimeoutSeconds: 3600, Retries: 2, Hooks: lifecycle.HooksSnapshot{LifecycleQueue: lifecycle.QueueLifecycle}}
	if _, err := le.lc.StartWithMessage(ctx, in, wid.String()); err != nil {
		le.t.Fatal(err)
	}
	return id, cid
}

func (le *lifecycleEnv) workflowStatus(id uuid.UUID) enums.WorkflowExecutionStatus {
	le.t.Helper()
	d, err := le.c.DescribeWorkflowExecution(context.Background(), lifecycle.WorkflowID(tid(id)), "")
	if err != nil {
		le.t.Fatal(err)
	}
	return d.WorkflowExecutionInfo.Status
}

func (le *lifecycleEnv) waitCompleted(id uuid.UUID, within time.Duration) {
	le.t.Helper()
	deadline := time.Now().Add(within)
	for le.workflowStatus(id) != enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		if time.Now().After(deadline) {
			le.t.Fatalf("the workflow of task %s did not complete within %s", id, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// history returns the workflow's whole history as text, payloads included.
func (le *lifecycleEnv) history(id uuid.UUID) string {
	le.t.Helper()
	it := le.c.GetWorkflowHistory(context.Background(), lifecycle.WorkflowID(tid(id)), "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var b strings.Builder
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			le.t.Fatal(err)
		}
		b.WriteString(ev.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// seqOf maps each delivered body to the seq whose snapshot it is.
func (le *lifecycleEnv) seqOf(id uuid.UUID, tail int64) map[string]int64 {
	le.t.Helper()
	m := map[string]int64{}
	for s := int64(1); s <= tail; s++ {
		m[le.expectedBody(id, s)] = s
	}
	return m
}

// checkDeliveries asserts what spec §10.2 promises one config, from the seq its first delivery is at:
// snapshots in seq order, never backwards; every row delivered except artifact rows followed by another
// artifact row (coalesced); the terminal snapshot last.
func (le *lifecycleEnv) checkDeliveries(name string, id uuid.UUID, got []delivery, from int64) {
	le.t.Helper()
	tail, err := le.log.Tail(context.Background(), id)
	if err != nil {
		le.t.Fatal(err)
	}
	term, err := le.log.TerminalSeq(context.Background(), id)
	if err != nil || term == 0 {
		le.t.Fatalf("terminal seq %d %v", term, err)
	}
	evs, err := le.log.Events(context.Background(), id, 0, 1000)
	if err != nil {
		le.t.Fatal(err)
	}
	seqOf := le.seqOf(id, tail)
	var seqs []int64
	for _, d := range got {
		s, ok := seqOf[d.body]
		if !ok {
			le.t.Fatalf("%s: a delivery that is no snapshot of the task: %s", name, d.body)
		}
		if len(seqs) > 0 && s < seqs[len(seqs)-1] {
			le.t.Fatalf("%s: seq order went backwards: %v then %d", name, seqs, s)
		}
		seqs = append(seqs, s)
	}
	if len(seqs) == 0 || seqs[len(seqs)-1] != term {
		le.t.Fatalf("%s: delivered seqs %v: the terminal snapshot (seq %d) must be delivered, last", name, seqs, term)
	}
	if seqs[0] != from {
		le.t.Errorf("%s: the first delivery is seq %d, want %d", name, seqs[0], from)
	}
	delivered := map[int64]bool{}
	for _, s := range seqs {
		delivered[s] = true
	}
	for i, ev := range evs {
		if ev.Seq < from || ev.Seq > term || delivered[ev.Seq] {
			continue
		}
		coalesced := ev.Kind == store.KindArtifact && i+1 < len(evs) && evs[i+1].Kind == store.KindArtifact
		if !coalesced {
			le.t.Errorf("%s: seq %d (%s) was never delivered (delivered %v)", name, ev.Seq, ev.Kind, seqs)
		}
	}
}

// TestPushDeliveryThroughTheLifecycle is push_delivery (spec §10.2, §16; PRD §15) through the real
// lifecycle, with the spec's timing: the webhook is down for 2 minutes from before the task starts, across
// its terminal event, then back. The task's inline config (saved with the task) and a config created after
// the task started (kontra.push) both get every status change and the terminal snapshot, in seq order;
// a2a-go's per-turn re-save adds no row and no delivery; the workflow stays open until the terminal
// snapshot is delivered (D78); and no credential crosses Temporal history.
func TestPushDeliveryThroughTheLifecycle(t *testing.T) {
	const downFor = 2 * time.Minute
	var (
		mu     sync.Mutex
		upAt   time.Time
		second = make(chan struct{})
	)
	r := newReceiver(t, func(int) int {
		mu.Lock()
		defer mu.Unlock()
		if upAt.IsZero() || time.Now().Before(upAt) {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	inline := pushConfig(r.url("/inline/" + secretPath))
	disp := &scriptedDispatch{}
	var le *lifecycleEnv
	disp.script = func(ctx context.Context, id uuid.UUID) error {
		le.status(id, store.StateWorking)
		// a2a-go saves the inline config again when the turn is dispatched: the same row, no new delivery.
		if s, err := le.cs.Save(ctx, tid(id), pushConfig(r.url("/inline/"+secretPath))); err != nil || s == nil {
			return errors.Join(errors.New("re-save"), err)
		}
		select { // the test creates the second config now
		case <-second:
		case <-ctx.Done():
			return ctx.Err()
		}
		le.artifact(id, "a1", "one", false)
		le.artifact(id, "a1", "two", true)
		le.status(id, store.StateWorking)
		le.artifact(id, "a2", "three", false)
		le.status(id, store.StateCompleted)
		return nil
	}
	le = newLifecycleEnv(t, disp, &DelivererOptions{Logger: quiet}) // the spec's timing

	mu.Lock()
	upAt = time.Now().Add(downFor)
	mu.Unlock()
	id, inlineID := le.start(inline)

	// Wait for the dispatch to have written "working", then create a config after the task started.
	deadline := time.Now().Add(30 * time.Second)
	for {
		row, err := le.log.Load(context.Background(), id)
		if err == nil && row.State == store.StateWorking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task never started working")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The caller names this config: its id must never cross Temporal history (spec §14).
	late, err := le.cs.Save(context.Background(), tid(id), &a2a.PushConfig{ID: callerConfigID, URL: r.url("/late"), Token: secretToken})
	if err != nil {
		t.Fatal(err)
	}
	lateFrom, _ := le.deliveredSeq(id, late.ID)
	close(second)

	// The task ends long before the webhook is back; the workflow must still be open, waiting for its pushes.
	for {
		row, _ := le.log.Load(context.Background(), id)
		if row != nil && row.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task did not end")
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	untilUp := time.Until(upAt)
	mu.Unlock()
	time.Sleep(untilUp - 10*time.Second)
	if st := le.workflowStatus(id); st != enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("10 s before the webhook is back the workflow is %s: it must outlive its pushes (D78)", st)
	}
	if n := len(r.delivered()); n != 0 {
		t.Fatalf("%d deliveries while the webhook was down", n)
	}
	// While pending, the activities' heartbeat details hold seqs only.
	if d, err := le.c.DescribeWorkflowExecution(context.Background(), lifecycle.WorkflowID(tid(id)), ""); err == nil {
		for _, pa := range d.PendingActivities {
			if n := containsAny(pa.String(), secretToken, secretCreds, secretPath); n != "" {
				t.Errorf("a pending activity's details carry %q", n)
			}
		}
	}

	// Back up: the retries (1 s doubling) reach it within about 2 min 10 s of the start.
	le.waitCompleted(id, 90*time.Second)
	var inl, lt []delivery
	for _, d := range r.delivered() {
		switch {
		case strings.HasPrefix(d.path, "/inline/"):
			inl = append(inl, d)
		case d.path == "/late":
			lt = append(lt, d)
		}
	}
	le.checkDeliveries("the inline config", id, inl, 1)
	le.checkDeliveries("the config created after the start", id, lt, lateFrom+1)
	if n := le.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 2 {
		t.Errorf("%d configs: the re-save of the inline config must not add one", n)
	}
	term, _ := le.log.TerminalSeq(context.Background(), id)
	for _, c := range []string{inlineID, late.ID} {
		if seq, failed := le.deliveredSeq(id, c); seq != term || failed {
			t.Errorf("config %s: delivered_seq %d failed %v, want %d", c, seq, failed, term)
		}
	}
	for _, d := range append(inl, lt...) {
		if d.header.Get("A2A-Notification-Token") != secretToken || d.header.Get("Content-Type") != ContentType {
			t.Fatalf("a delivery's headers: %v", d.header)
		}
	}
	if d := inl[0]; d.header.Get("Authorization") != "Bearer "+secretCreds {
		t.Errorf("the inline config's Authorization: %q", d.header.Get("Authorization"))
	}
	if h := le.history(id); containsAny(h, secretToken, secretCreds, secretPath) != "" {
		t.Errorf("Temporal history carries a credential (%q)", containsAny(h, secretToken, secretCreds, secretPath))
	} else if !strings.Contains(h, lifecycle.ConfigRef(id, inlineID)) || !strings.Contains(h, lifecycle.ConfigRef(id, late.ID)) {
		t.Errorf("the history names no push_webhook for the configs %s and %s", inlineID, late.ID)
	}
	// Config ids cross history only as their ConfigRef: not in kontra.push, list_push_configs' result or
	// push_webhook's input, nor in any activity error.
	if h := le.history(id); strings.Contains(h, callerConfigID) || strings.Contains(h, inlineID) {
		t.Errorf("Temporal history carries a config id (%q or %q)", callerConfigID, inlineID)
	}
}

// callerConfigID is a config id chosen by the caller.
const callerConfigID = "acme-billing-webhook-for-order-4711"

// TestPushWorkerRestart: the kontra-push worker running a push_webhook stops mid-delivery (a drain, a
// crash of its replica) and another one takes the activity over; it resumes from delivered_seq and its
// heartbeat details, and the receiver sees the snapshots in seq order, the terminal one last.
func TestPushWorkerRestart(t *testing.T) {
	r := newReceiver(t, nil)
	r.hold = 150 * time.Millisecond
	disp := &scriptedDispatch{}
	var le *lifecycleEnv
	disp.script = func(ctx context.Context, id uuid.UUID) error {
		for range 15 {
			le.status(id, store.StateWorking)
		}
		le.status(id, store.StateCompleted)
		return nil
	}
	le = newLifecycleEnv(t, disp, nil) // no push worker among the replica's workers: this test runs its own
	d := NewDeliverer(le.log, private, fast())
	newPushWorker := func() worker.Worker {
		w := worker.New(le.c, lifecycle.QueuePush, worker.Options{DisableWorkflowWorker: true, WorkerStopTimeout: 2 * time.Second})
		w.RegisterActivityWithOptions(d.PushWebhook, activity.RegisterOptions{Name: lifecycle.ActPushWebhook})
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	first := newPushWorker()
	id, cid := le.start(&a2a.PushConfig{URL: r.url("/hook"), Token: secretToken})

	deadline := time.Now().Add(30 * time.Second)
	for len(r.delivered()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d deliveries before the restart", len(r.delivered()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	first.Stop()
	stoppedAt, _ := le.deliveredSeq(id, cid)
	if stoppedAt == 0 || stoppedAt >= 17 {
		t.Fatalf("delivered_seq %d at the restart: it must fall mid-stream (1..16) for the test to mean anything", stoppedAt)
	}
	second := newPushWorker()
	defer second.Stop()
	le.waitCompleted(id, 90*time.Second)
	got := r.delivered()
	le.checkDeliveries("the restarted config", id, got, 1)
	// At least once: a delivery may repeat across the restart, but none before the recorded position does.
	counts := map[string]int{}
	for _, dl := range got {
		counts[dl.body]++
	}
	var repeats []int64
	seqOf := le.seqOf(id, 17)
	for b, n := range counts {
		if n > 1 {
			repeats = append(repeats, seqOf[b])
		}
	}
	sort.Slice(repeats, func(i, j int) bool { return repeats[i] < repeats[j] })
	for _, s := range repeats {
		if s < stoppedAt {
			t.Errorf("seq %d was delivered again after the restart, below delivered_seq %d", s, stoppedAt)
		}
	}
	if seq, _ := le.deliveredSeq(id, cid); seq != 17 {
		t.Errorf("delivered_seq %d, want 17", seq)
	}
}
