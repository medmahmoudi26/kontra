package lifecycle

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// remoteQueue is the operator's own queue in TestRemoteLifecycleHooks (hooks.lifecycle.task_queue).
const remoteQueue = "it-remote-lifecycle"

// remoteHooks is an operator's remote Lifecycle (spec §13.3): OnDispatched and OnTerminal registered by type name on
// its own queue, as any SDK would serve them, each taking (TaskRef, args), and the retired ReviewWindow, which a run
// started after D351 must never ask. It records what it was asked.
type remoteHooks struct {
	mu       sync.Mutex
	calls    []string // hook type names, in call order
	terminal []hooks.TaskRef
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
	w.RegisterActivityWithOptions(func(_ context.Context, _ hooks.TaskRef, _ struct{}) (hooks.RetiredReviewWindow, error) {
		h.record(HookReviewWindow)
		return hooks.RetiredReviewWindow{}, nil
	}, activity.RegisterOptions{Name: HookReviewWindow})
}

func (h *remoteHooks) snapshot() remoteHooks {
	h.mu.Lock()
	defer h.mu.Unlock()
	return remoteHooks{calls: append([]string(nil), h.calls...), terminal: append([]hooks.TaskRef(nil), h.terminal...)}
}

// TestRemoteLifecycleHooks is C3 of the M3 corrections (D169, D186): with hooks.lifecycle set, the gate's workers
// register no built-in hook (lifecycle.WorkerConfig.RemoteHooks), and the hooks are scheduled by type name on the
// operator's queue, where a worker of its own serves them. That worker sees OnTerminal once for the completed task,
// and nothing after it: the workflow ends without asking the retired ReviewWindow (D351), and its history carries the
// no-review-window marker.
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
	rh := &remoteHooks{}
	hw := worker.New(c, remoteQueue, worker.Options{DisableWorkflowWorker: true, WorkerStopTimeout: time.Second})
	rh.register(hw)
	if err := hw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hw.Stop)
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: remoteQueue}})

	cctx, id := caller(true)
	d.set(id, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
	if _, err := q.Write(cctx, payloadOf(requestOf("m-1", "the work"))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task completed", func() bool { return e.state(id) == store.StateCompleted })
	eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, id) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
	got := rh.snapshot()
	if len(got.terminal) != 1 || got.terminal[0].TaskID != id || got.terminal[0].State != string(store.StateCompleted) {
		t.Fatalf("the remote OnTerminal saw %+v, want task %s completed once", got.terminal, id)
	}
	// OnDispatched follows the dispatch's kontra.dispatched signal, which this fake dispatch does not send.
	calls := slices.DeleteFunc(got.calls, func(n string) bool { return n == HookOnDispatched })
	if want := []string{HookOnTerminal}; !slices.Equal(calls, want) {
		t.Fatalf("remote hook calls %v, want %v", got.calls, want)
	}
	js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(history(t, c, id))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(js, []byte(HookReviewWindow)) {
		t.Error("a run started after the change scheduled ReviewWindow")
	}
	if !slices.Contains(changeIDs(t, js), changeNoReviewWindow) {
		t.Errorf("the history holds no %s marker", changeNoReviewWindow)
	}
	recordHistory(t, c, id, "no_review_window_completed")
}
