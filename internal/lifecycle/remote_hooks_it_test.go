package lifecycle

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// remoteQueue is the operator's own queue in TestRemoteLifecycleHooks (hooks.lifecycle.task_queue).
const remoteQueue = "it-remote-lifecycle"

// remoteHooks is an operator's remote Lifecycle (spec §13.3): the five activities registered by type name on its
// own queue, as any SDK would serve them, each taking (TaskRef, args). It opens a review window of window seconds,
// accepts every review message without a dispute, and records what it was asked.
type remoteHooks struct {
	window int64

	mu       sync.Mutex
	calls    []string // hook type names, in call order
	terminal []hooks.TaskRef
	opened   time.Time // when ReviewWindow answered
	messages []hooks.ReviewMessageArgs
	closed   []hooks.ReviewClosedArgs
	closedAt time.Time
}

func (h *remoteHooks) record(name string) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	h.mu.Unlock()
}

func (h *remoteHooks) register(w worker.Worker) {
	w.RegisterActivityWithOptions(func(_ context.Context, _ hooks.TaskRef, _ hooks.DispatchedArgs) error {
		h.record(HookOnDispatched)
		return nil
	}, activity.RegisterOptions{Name: HookOnDispatched})
	w.RegisterActivityWithOptions(func(_ context.Context, t hooks.TaskRef, _ struct{}) (hooks.Patch, error) {
		h.record(HookOnTerminal)
		h.mu.Lock()
		h.terminal = append(h.terminal, t)
		h.mu.Unlock()
		return hooks.Patch{}, nil
	}, activity.RegisterOptions{Name: HookOnTerminal})
	w.RegisterActivityWithOptions(func(_ context.Context, _ hooks.TaskRef, _ struct{}) (hooks.ReviewWindow, error) {
		h.record(HookReviewWindow)
		h.mu.Lock()
		h.opened = time.Now()
		h.mu.Unlock()
		return hooks.ReviewWindow{Open: true, Seconds: h.window}, nil
	}, activity.RegisterOptions{Name: HookReviewWindow})
	w.RegisterActivityWithOptions(func(_ context.Context, _ hooks.TaskRef, a hooks.ReviewMessageArgs) (hooks.ReviewMessageResult, error) {
		h.record(HookOnReviewMessage)
		h.mu.Lock()
		h.messages = append(h.messages, a)
		h.mu.Unlock()
		return hooks.ReviewMessageResult{Accept: true}, nil
	}, activity.RegisterOptions{Name: HookOnReviewMessage})
	w.RegisterActivityWithOptions(func(_ context.Context, _ hooks.TaskRef, a hooks.ReviewClosedArgs) (hooks.Patch, error) {
		h.record(HookOnReviewClosed)
		h.mu.Lock()
		h.closed = append(h.closed, a)
		h.closedAt = time.Now()
		h.mu.Unlock()
		return hooks.Patch{}, nil
	}, activity.RegisterOptions{Name: HookOnReviewClosed})
}

func (h *remoteHooks) snapshot() remoteHooks {
	h.mu.Lock()
	defer h.mu.Unlock()
	return remoteHooks{window: h.window, calls: append([]string(nil), h.calls...), terminal: append([]hooks.TaskRef(nil), h.terminal...),
		opened: h.opened, messages: append([]hooks.ReviewMessageArgs(nil), h.messages...),
		closed: append([]hooks.ReviewClosedArgs(nil), h.closed...), closedAt: h.closedAt}
}

// reviewItem stores a review-window message's work item, as relay.Interceptors.reviewMessage does before it sends
// kontra.review_message, and returns its id.
func reviewItem(t *testing.T, e *pgEnv, taskID, msgID, text string) uuid.UUID {
	t.Helper()
	wid, err := ids.NewWorkItemID()
	if err != nil {
		t.Fatal(err)
	}
	req := requestOf(msgID, text)
	req.Message.TaskID, req.Message.ContextID = a2a.TaskID(taskID), ""
	if err := e.log.InsertWorkItem(context.Background(), nil, store.WorkItem{ID: wid, TaskID: uuid.MustParse(taskID),
		Kind: store.WorkExecute, Payload: store.WorkPayload{Request: req, Owner: buyer}}); err != nil {
		t.Fatal(err)
	}
	return wid
}

// TestRemoteLifecycleHooks is C3 of the M3 corrections (D169, D186): with hooks.lifecycle set, the gate's workers
// register no built-in hook (lifecycle.WorkerConfig.RemoteHooks), and the five activities are scheduled by type name
// on the operator's queue, where a worker of its own serves them. That worker sees OnTerminal for the completed
// task and opens a 5 s review window: a review message inside it reaches OnReviewMessage and is accepted, and a
// dispute sent after the window closed (OnReviewClosed, not disputed) is refused and reaches no hook.
func TestRemoteLifecycleHooks(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &fakeDispatch{t: t, log: e.log, script: map[string][]turn{}, calls: map[string][]DispatchInput{}}
	w, err := NewWorkers(c, WorkerConfig{ReplicaID: "it-remote", DrainTimeout: time.Second, RemoteHooks: true},
		Activities{Lifecycle: e.acts, Dispatch: d})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	rh := &remoteHooks{window: 5}
	hw := worker.New(c, remoteQueue, worker.Options{DisableWorkflowWorker: true, WorkerStopTimeout: time.Second})
	rh.register(hw)
	if err := hw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hw.Stop)
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: remoteQueue}})
	ctx := context.Background()

	cctx, id := caller(true)
	d.set(id, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
	if _, err := q.Write(cctx, payloadOf(requestOf("m-1", "the work"))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task completed", func() bool { return e.state(id) == store.StateCompleted })
	eventually(t, "the remote ReviewWindow", func() bool { return !rh.snapshot().opened.IsZero() })
	got := rh.snapshot()
	if len(got.terminal) != 1 || got.terminal[0].TaskID != id || got.terminal[0].State != string(store.StateCompleted) {
		t.Fatalf("the remote OnTerminal saw %+v, want task %s completed once", got.terminal, id)
	}

	// Inside the window: the message reaches the operator's hook and is appended.
	in := reviewItem(t, e, id, "r-1", "thanks, one question")
	res, err := lc.ReviewMessage(ctx, a2a.TaskID(id), ReviewMessageUpdate{WorkItemID: in.String(), MessageID: "r-1"})
	if err != nil || !res.Accepted || res.Seq == nil {
		t.Fatalf("a review message inside the window: %+v %v, want accepted with its seq", res, err)
	}
	if got := rh.snapshot(); len(got.messages) != 1 || got.messages[0].MessageID != "r-1" || got.messages[0].WorkItemID != in.String() {
		t.Fatalf("the remote OnReviewMessage saw %+v, want r-1 once", got.messages)
	}

	// The window closes after its 5 s (release): OnReviewClosed, not disputed.
	eventually(t, "the remote OnReviewClosed", func() bool { return len(rh.snapshot().closed) == 1 })
	got = rh.snapshot()
	if got.closed[0].Disputed {
		t.Fatalf("OnReviewClosed %+v, want not disputed", got.closed[0])
	}
	if open := got.closedAt.Sub(got.opened); open < 4500*time.Millisecond {
		t.Fatalf("the window closed %v after ReviewWindow answered, want its 5 s", open)
	}

	// After release: a dispute is refused and reaches no hook; its work item is not appended.
	late := reviewItem(t, e, id, "dispute-1", "I dispute this")
	_, err = lc.ReviewMessage(ctx, a2a.TaskID(id), ReviewMessageUpdate{WorkItemID: late.String(), MessageID: "dispute-1",
		Metadata: map[string]any{"kontra.dispute": true}})
	if !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Fatalf("a dispute after the window closed: %v, want it refused", err)
	}
	time.Sleep(time.Second)
	got = rh.snapshot()
	if len(got.messages) != 1 {
		t.Fatalf("the remote OnReviewMessage saw %+v after the window closed, want only r-1", got.messages)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and payload::text like '%I dispute this%'`, id); n != 0 {
		t.Fatalf("the refused dispute is in the task's log (%d events)", n)
	}
	// OnDispatched follows the dispatch's kontra.dispatched signal, which this fake dispatch does not send; the
	// others run once each, in §13.3's order.
	calls := slices.DeleteFunc(got.calls, func(n string) bool { return n == HookOnDispatched })
	if want := []string{HookOnTerminal, HookReviewWindow, HookOnReviewMessage, HookOnReviewClosed}; !slices.Equal(calls, want) {
		t.Fatalf("remote hook calls %v, want %v", got.calls, want)
	}
	eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, id) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
}
