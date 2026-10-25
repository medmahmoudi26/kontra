package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/store"
)

const testTask = "01920000-0000-7000-8000-000000000001"

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// world is the fake Postgres and upstream the workflow's activities see. Every activity records what it
// was asked; dispatch, forward_cancel and the hooks are scripted per test.
type world struct {
	t  *testing.T
	mu sync.Mutex

	deadline   time.Time
	state      string // tasks.state
	firstID    map[int]bool
	replica    string
	pushCfgs   [][]string // list_push_configs answers, in order; the last one repeats
	reviewWin  hooks.ReviewWindow
	onTerminal hooks.Patch
	onClosed   hooks.Patch

	prepareFn  func(in PrepareInput) error // nil: every prepare succeeds
	dispatchFn func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error)
	forwardFn  func(ctx context.Context, k int, queue string) (ForwardCancelResult, error)
	abortErr   error
	pushFn     func(ctx context.Context, in PushInput) (PushResult, error)
	reviewFn   func(args hooks.ReviewMessageArgs) hooks.ReviewMessageResult

	now     func() time.Time // the test environment's clock
	release chan struct{}    // closed at cleanup: every blocked fake returns
	aborted chan struct{}    // closed by abort

	events     []string // ordered log of what happened, "dispatch:1", "terminal:failed", …
	prepares   []PrepareInput
	dispatches []dispatchRec
	terminals  []WriteTerminalInput
	forwards   []string // queues
	aborts     []string // queues
	patches    []ApplyMetadataInput
	pushes     []string // config ids
	pushQueues []string
	hookCalls  []string
	lists      int
	submits    []SubmitInput
	appends    []ReviewAppendInput
}

type dispatchRec struct {
	in    DispatchInput
	queue string
	at    time.Time // the workflow clock when the activity started
	hb    time.Duration
	stc   time.Duration // the start-to-close timeout the activity got
}

func newWorld(t *testing.T) *world {
	w := &world{
		t: t, deadline: t0.Add(time.Hour), state: string(store.StateSubmitted), firstID: map[int]bool{}, replica: "r1",
		release: make(chan struct{}), aborted: make(chan struct{}),
	}
	t.Cleanup(func() { close(w.release) })
	return w
}

func (w *world) event(e string) {
	w.mu.Lock()
	w.events = append(w.events, e)
	w.mu.Unlock()
}

func (w *world) snapshot() world {
	w.mu.Lock()
	defer w.mu.Unlock()
	return world{
		events: slices.Clone(w.events), prepares: slices.Clone(w.prepares), dispatches: slices.Clone(w.dispatches),
		terminals: slices.Clone(w.terminals), forwards: slices.Clone(w.forwards), aborts: slices.Clone(w.aborts),
		patches: slices.Clone(w.patches), pushes: slices.Clone(w.pushes), pushQueues: slices.Clone(w.pushQueues),
		hookCalls: slices.Clone(w.hookCalls), lists: w.lists, submits: slices.Clone(w.submits),
		appends: slices.Clone(w.appends), state: w.state,
	}
}

func (w *world) setState(s string) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

// block waits until ctx ends, the abort or the test's end.
func (w *world) block(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.aborted:
		return nil
	case <-w.release:
		return errors.New("released")
	}
}

func (w *world) prepare(ctx context.Context, in PrepareInput) (PrepareResult, error) {
	w.mu.Lock()
	w.prepares = append(w.prepares, in)
	fn := w.prepareFn
	w.mu.Unlock()
	w.event(fmt.Sprintf("prepare:%d", in.N))
	if fn != nil {
		if err := fn(in); err != nil {
			return PrepareResult{}, err
		}
	}
	return PrepareResult{Queue: QueueDirect, Backend: "direct", Deadline: w.deadline}, nil
}

func (w *world) attemptState(ctx context.Context, in AttemptRef) (AttemptState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := AttemptState{TaskState: w.state}
	if in.N > 0 {
		st.Backend, st.ReplicaID = "direct", w.replica
		if w.firstID[in.N] {
			ts := t0
			st.FirstUpstreamIDAt = &ts
		}
	}
	return st, nil
}

func (w *world) listPush(ctx context.Context, taskID string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lists++
	if len(w.pushCfgs) == 0 {
		return nil, nil
	}
	i := min(w.lists-1, len(w.pushCfgs)-1)
	return w.pushCfgs[i], nil
}

func (w *world) writeTerminal(ctx context.Context, in WriteTerminalInput) (TerminalResult, error) {
	w.mu.Lock()
	w.terminals = append(w.terminals, in)
	if !store.State(w.state).Terminal() {
		w.state = in.State
	}
	st := w.state
	w.mu.Unlock()
	w.event("terminal:" + in.State + ":" + in.Reason + in.Canceled)
	return TerminalResult{Seq: 9, State: st}, nil
}

func (w *world) applyMetadata(ctx context.Context, in ApplyMetadataInput) (int64, error) {
	w.mu.Lock()
	w.patches = append(w.patches, in)
	w.mu.Unlock()
	w.event("patch:" + in.Source)
	return 7, nil
}

func (w *world) taskRef(ctx context.Context, taskID string) (hooks.TaskRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return hooks.TaskRef{TaskID: taskID, State: w.state}, nil
}

func (w *world) submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	w.mu.Lock()
	w.submits = append(w.submits, in)
	w.state = string(store.StateSubmitted)
	w.mu.Unlock()
	w.event("submit")
	return SubmitResult{Deadline: w.deadline}, nil
}

func (w *world) appendReview(ctx context.Context, in ReviewAppendInput) (int64, error) {
	w.mu.Lock()
	w.appends = append(w.appends, in)
	w.mu.Unlock()
	w.event("append:" + in.WorkItemID)
	return 11, nil
}

func (w *world) dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error) {
	info := activity.GetInfo(ctx)
	w.mu.Lock()
	k := len(w.dispatches)
	w.dispatches = append(w.dispatches, dispatchRec{in: in, queue: info.TaskQueue, at: w.now(), hb: info.HeartbeatTimeout,
		stc: info.Deadline.Sub(info.StartedTime)})
	fn := w.dispatchFn
	w.mu.Unlock()
	w.event(fmt.Sprintf("dispatch:%d:%v", in.N, in.Resume))
	res, err := fn(ctx, k, in)
	w.event(fmt.Sprintf("dispatched:%d:%s", in.N, res.Outcome))
	return res, err
}

func (w *world) abort(ctx context.Context, in AttemptRef) error {
	w.mu.Lock()
	w.aborts = append(w.aborts, activity.GetInfo(ctx).TaskQueue)
	err := w.abortErr
	w.mu.Unlock()
	w.event("abort")
	if err != nil {
		return err
	}
	select {
	case <-w.aborted:
	default:
		close(w.aborted)
	}
	return nil
}

func (w *world) forwardCancel(ctx context.Context, in AttemptRef) (ForwardCancelResult, error) {
	q := activity.GetInfo(ctx).TaskQueue
	w.mu.Lock()
	k := len(w.forwards)
	w.forwards = append(w.forwards, q)
	fn := w.forwardFn
	w.mu.Unlock()
	w.event("forward:" + q)
	if fn == nil {
		return ForwardCancelResult{Outcome: CancelCanceled}, nil
	}
	return fn(ctx, k, q)
}

func (w *world) push(ctx context.Context, in PushInput) (PushResult, error) {
	w.mu.Lock()
	w.pushes = append(w.pushes, in.ConfigID)
	w.pushQueues = append(w.pushQueues, activity.GetInfo(ctx).TaskQueue)
	fn := w.pushFn
	w.mu.Unlock()
	w.event("push:" + in.ConfigID)
	if fn != nil {
		return fn(ctx, in)
	}
	return PushResult{Stopped: "terminal"}, nil
}

func (w *world) hook(name string) {
	w.mu.Lock()
	w.hookCalls = append(w.hookCalls, name)
	w.mu.Unlock()
	w.event("hook:" + name)
}

// env is a test workflow environment with the world's activities registered under the workflow's names.
func (w *world) env() *testsuite.TestWorkflowEnvironment {
	var s testsuite.WorkflowTestSuite
	s.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(t0)
	w.now = env.Now
	env.SetTestTimeout(60 * time.Second)
	env.RegisterWorkflowWithOptions(TaskLifecycle, workflow.RegisterOptions{Name: WorkflowType})
	reg := func(fn any, name string) { env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name}) }
	reg(w.prepare, ActPrepare)
	reg(w.attemptState, ActAttemptState)
	reg(w.listPush, ActListPushConfigs)
	reg(w.writeTerminal, ActWriteTerminal)
	reg(w.applyMetadata, ActApplyMetadata)
	reg(w.taskRef, ActTaskRef)
	reg(w.submit, ActSubmit)
	reg(w.appendReview, ActAppendReviewMessage)
	reg(w.dispatch, ActDispatch)
	reg(w.abort, ActAbort)
	reg(w.forwardCancel, ActForwardCancel)
	reg(w.push, ActPushWebhook)
	reg(func(ctx context.Context, t hooks.TaskRef, a hooks.DispatchedArgs) error {
		w.hook(HookOnDispatched)
		return nil
	}, HookOnDispatched)
	reg(func(ctx context.Context, t hooks.TaskRef) (hooks.Patch, error) {
		w.hook(HookOnTerminal + ":" + t.State)
		return w.onTerminal, nil
	}, HookOnTerminal)
	reg(func(ctx context.Context, t hooks.TaskRef) (hooks.ReviewWindow, error) {
		w.hook(HookReviewWindow)
		return w.reviewWin, nil
	}, HookReviewWindow)
	reg(func(ctx context.Context, t hooks.TaskRef, a hooks.ReviewMessageArgs) (hooks.ReviewMessageResult, error) {
		w.hook(HookOnReviewMessage)
		if w.reviewFn != nil {
			return w.reviewFn(a), nil
		}
		return hooks.ReviewMessageResult{}, nil
	}, HookOnReviewMessage)
	reg(func(ctx context.Context, t hooks.TaskRef, a hooks.ReviewClosedArgs) (hooks.Patch, error) {
		w.hook(fmt.Sprintf("%s:%v", HookOnReviewClosed, a.Disputed))
		return w.onClosed, nil
	}, HookOnReviewClosed)
	return env
}

// updates records the outcome of Updates sent to the test environment.
type updates struct {
	mu   sync.Mutex
	logs []string
	res  map[string]any
	errs map[string]error
	rej  map[string]error
}

func newUpdates() *updates {
	return &updates{res: map[string]any{}, errs: map[string]error{}, rej: map[string]error{}}
}

func (u *updates) cb(id string) *testsuite.TestUpdateCallback {
	return &testsuite.TestUpdateCallback{
		OnAccept: func() { u.add(id + ":accepted") },
		OnReject: func(err error) {
			u.mu.Lock()
			u.rej[id] = err
			u.mu.Unlock()
			u.add(id + ":rejected")
		},
		OnComplete: func(r any, err error) {
			u.mu.Lock()
			u.res[id], u.errs[id] = r, err
			u.mu.Unlock()
			u.add(id + ":completed")
		},
	}
}

func (u *updates) add(s string) {
	u.mu.Lock()
	u.logs = append(u.logs, s)
	u.mu.Unlock()
}

func (u *updates) has(s string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Contains(u.logs, s)
}

func (u *updates) result(id string) (any, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.res[id], u.errs[id]
}

// start sends the first kontra.message with the start, as UpdateWithStart does, and runs the workflow.
func start(env *testsuite.TestWorkflowEnvironment, u *updates, in Input) {
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-1", u.cb("wi-1"), MessageUpdate{WorkItemID: "wi-1"})
	}, 0)
	env.ExecuteWorkflow(TaskLifecycle, in)
}

func input(retries int) Input {
	return Input{TaskID: testTask, TimeoutSeconds: 3600, Retries: retries, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}}
}

func done(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
}

func outcome(o DispatchOutcome, reason string) func(context.Context, int, DispatchInput) (DispatchResult, error) {
	return func(context.Context, int, DispatchInput) (DispatchResult, error) {
		return DispatchResult{Outcome: o, Reason: reason}, nil
	}
}

func heartbeatTimeout() error {
	return temporal.NewTimeoutError(enums.TIMEOUT_TYPE_HEARTBEAT, nil)
}

func eventIndex(t *testing.T, events []string, prefix string) int {
	t.Helper()
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	t.Fatalf("no event %q in %v", prefix, events)
	return -1
}

// --- the retry table (spec §6.4) ---

func TestRetryBeforeFirstUpstreamIDBacksOffThenFails(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = outcome(DispatchRetryable, string(store.ReasonUpstreamUnreachable))
	env := w.env()
	u := newUpdates()
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 3 || len(s.prepares) != 3 {
		t.Fatalf("dispatches %d, prepares %d: want 3 attempts (1 + task.retries)", len(s.dispatches), len(s.prepares))
	}
	for i, d := range s.dispatches {
		if d.in.N != i+1 || d.in.Resume || d.in.WorkItemID != "wi-1" || d.queue != QueueDirect {
			t.Errorf("dispatch %d: %+v on %s", i, d.in, d.queue)
		}
		if d.hb != dispatchHeartbeat {
			t.Errorf("dispatch %d heartbeat timeout %v", i, d.hb)
		}
		// start-to-close = time to the deadline + 30 s
		if want := w.deadline.Sub(d.at) + dispatchSlack; d.stc < want-time.Second || d.stc > want+time.Second {
			t.Errorf("dispatch %d start-to-close %v, want %v", i, d.stc, want)
		}
	}
	if gap := s.dispatches[1].at.Sub(s.dispatches[0].at); gap < time.Second || gap >= 2*time.Second {
		t.Errorf("first backoff %v, want 1 s", gap)
	}
	if gap := s.dispatches[2].at.Sub(s.dispatches[1].at); gap < 2*time.Second || gap >= 3*time.Second {
		t.Errorf("second backoff %v, want 2 s", gap)
	}
	if s.prepares[1].PreviousOutcome != "" {
		t.Errorf("a retryable dispatch ended its attempt itself; prepare got %q", s.prepares[1].PreviousOutcome)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "failed" || s.terminals[0].Reason != "upstream_unreachable" {
		t.Fatalf("terminal writes %+v, want failed: upstream_unreachable", s.terminals)
	}
	if !u.has("wi-1:completed") {
		t.Error("the first kontra.message did not complete")
	}
	if i, j := eventIndex(t, s.events, "terminal:failed"), eventIndex(t, s.events, "hook:"+HookOnTerminal); j < i {
		t.Errorf("OnTerminal before the terminal event: %v", s.events)
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
		if k == 0 {
			return DispatchResult{Outcome: DispatchRetryable, Reason: string(store.ReasonUpstreamError), RetryAfterSeconds: 7}, nil
		}
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 2 {
		t.Fatalf("%d dispatches", len(s.dispatches))
	}
	if gap := s.dispatches[1].at.Sub(s.dispatches[0].at); gap < 7*time.Second {
		t.Errorf("the retry came %v after a 429 with Retry-After: 7", gap)
	}
	if len(s.terminals) != 0 {
		t.Errorf("the relay logged the terminal event; the workflow wrote %+v", s.terminals)
	}
}

func TestRetriesStopAtTheDeadline(t *testing.T) {
	w := newWorld(t)
	w.deadline = t0.Add(5 * time.Second)
	w.dispatchFn = outcome(DispatchRetryable, string(store.ReasonUpstreamUnreachable))
	env := w.env()
	start(env, newUpdates(), input(10))
	done(t, env)
	s := w.snapshot()
	// 1 s, 2 s, then 4 s would pass the deadline: the timer fires during the wait.
	if len(s.dispatches) != 3 {
		t.Errorf("%d dispatches, want 3 before the deadline", len(s.dispatches))
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTimeout) {
		t.Fatalf("terminal writes %+v, want failed: timeout", s.terminals)
	}
}

func TestRetryableAfterFirstUpstreamIDFailsWithoutRetry(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = outcome(DispatchRetryable, string(store.ReasonUpstreamUnreachable))
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 1 || len(s.prepares) != 1 {
		t.Fatalf("dispatches %d prepares %d: no retry after the first upstream id", len(s.dispatches), len(s.prepares))
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != "upstream_unreachable" {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

func TestLostDispatchAfterFirstIDResumesSameAttempt(t *testing.T) {
	for name, first := range map[string]func(context.Context, int, DispatchInput) (DispatchResult, error){
		"heartbeat timeout": func(context.Context, int, DispatchInput) (DispatchResult, error) {
			return DispatchResult{}, heartbeatTimeout()
		},
		"context lost": func(context.Context, int, DispatchInput) (DispatchResult, error) {
			return DispatchResult{Outcome: DispatchContextLost}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.firstID[1] = true
			w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
				if k == 0 {
					return first(ctx, k, in)
				}
				w.setState(string(store.StateCompleted))
				return DispatchResult{Outcome: DispatchTerminal}, nil
			}
			env := w.env()
			start(env, newUpdates(), input(2))
			done(t, env)
			s := w.snapshot()
			if len(s.prepares) != 1 {
				t.Errorf("prepares %+v: a re-attach is no new attempt", s.prepares)
			}
			if len(s.dispatches) != 2 {
				t.Fatalf("%d dispatches", len(s.dispatches))
			}
			r := s.dispatches[1]
			if r.in.N != 1 || !r.in.Resume || r.queue != QueueDirect || r.in.WorkItemID != "wi-1" {
				t.Errorf("re-attach dispatch %+v on %s, want dispatch(n=1, resume=true) on kontra-direct", r.in, r.queue)
			}
			if gap := r.at.Sub(s.dispatches[0].at); gap < time.Second {
				t.Errorf("the first resume came after %v, want 1 s", gap)
			}
			if len(s.terminals) != 0 {
				t.Errorf("terminal writes %+v", s.terminals)
			}
			if !slices.Contains(s.hookCalls, HookOnTerminal+":completed") {
				t.Errorf("hooks %v", s.hookCalls)
			}
		})
	}
}

func TestScheduleToStartTimeoutStartsANewAttempt(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if k == 0 {
			return DispatchResult{}, temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_START, nil)
		}
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 2 || s.prepares[1].N != 2 || s.prepares[1].PreviousOutcome != string(store.OutcomeUpstreamUnreachable) {
		t.Fatalf("prepares %+v: want attempt 2, ending attempt 1", s.prepares)
	}
	if len(s.dispatches) != 2 || s.dispatches[1].in.N != 2 || s.dispatches[1].in.Resume {
		t.Fatalf("dispatches %+v", s.dispatches)
	}
}

func TestUpstreamErrorFailsWithoutRetry(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateFailed)) // the relay logged failed: upstream_error with the upstream's message
		return DispatchResult{Outcome: DispatchTerminal, Reason: string(store.ReasonUpstreamError)}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 1 || len(s.prepares) != 1 || len(s.terminals) != 0 {
		t.Fatalf("dispatches %d prepares %d terminals %+v", len(s.dispatches), len(s.prepares), s.terminals)
	}
	if !slices.Contains(s.hookCalls, HookOnTerminal+":failed") {
		t.Errorf("OnTerminal did not run on the failed task: %v", s.hookCalls)
	}
}

func TestReattachLimit(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = outcome(DispatchContextLost, "")
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 1+MaxResumes {
		t.Fatalf("%d dispatches, want 1 + %d resumes", len(s.dispatches), MaxResumes)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30, 30, 30}
	for i := 1; i < len(s.dispatches); i++ {
		gap := s.dispatches[i].at.Sub(s.dispatches[i-1].at)
		if gap < want[i-1]*time.Second || gap >= (want[i-1]+1)*time.Second {
			t.Errorf("resume %d after %v, want %v s", i, gap, want[i-1])
		}
		if !s.dispatches[i].in.Resume || s.dispatches[i].in.N != 1 {
			t.Errorf("dispatch %d: %+v", i, s.dispatches[i].in)
		}
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != "upstream_error" || s.terminals[0].Text != "re-attach limit reached" {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

func TestDeadPrepareBackendIsServiceUnavailable(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	env.RegisterActivityWithOptions(func(ctx context.Context, in PrepareInput) (PrepareResult, error) {
		return PrepareResult{}, temporal.NewNonRetryableApplicationError("gone", ErrTypeServiceUnavailable, nil)
	}, activity.RegisterOptions{Name: ActPrepare})
	w.dispatchFn = outcome(DispatchTerminal, "")
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 0 || len(s.terminals) != 1 || s.terminals[0].Reason != "service_unavailable" {
		t.Fatalf("dispatches %d terminals %+v", len(s.dispatches), s.terminals)
	}
}

// --- the task.timeout (spec §6.5) ---

func TestTimeoutForwardsCancelThenFails(t *testing.T) {
	w := newWorld(t)
	w.deadline = t0.Add(2 * time.Second) // the dispatch runs: the timer fires on the wall clock
	w.firstID[1] = true
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		return DispatchResult{}, w.block(ctx)
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.terminals) != 1 || s.terminals[0].Reason != "timeout" || s.terminals[0].EndAttempt != string(store.OutcomeTimeout) {
		t.Fatalf("terminal writes %+v, want failed: timeout", s.terminals)
	}
	// forward_cancel goes to the dispatching replica's control queue while the dispatch runs.
	if len(s.forwards) != 1 || s.forwards[0] != QueueCtl("r1") {
		t.Errorf("forward_cancel on %v, want once on %s", s.forwards, QueueCtl("r1"))
	}
	if !slices.Contains(s.hookCalls, HookOnTerminal+":failed") {
		t.Errorf("hooks %v", s.hookCalls)
	}
}

func TestTimeoutAtInputRequired(t *testing.T) {
	w := newWorld(t)
	w.deadline = t0.Add(30 * time.Minute)
	w.firstID[1] = true
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateInputRequired))
		return DispatchResult{Outcome: DispatchInterrupted}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.terminals) != 1 || s.terminals[0].Reason != "timeout" {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if len(s.forwards) != 1 || s.forwards[0] != QueueDirectCtl {
		t.Errorf("forward_cancel on %v, want kontra-direct-ctl (no dispatch runs)", s.forwards)
	}
}

// --- cancel (spec §8) ---

func runningDispatch(w *world) func(context.Context, int, DispatchInput) (DispatchResult, error) {
	return func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if err := w.block(ctx); err != nil {
			return DispatchResult{}, err
		}
		return DispatchResult{Outcome: DispatchAborted}, nil
	}
}

func TestCancelWithoutUpstreamTaskAborts(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = runningDispatch(w)
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 200*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.aborts) != 1 || s.aborts[0] != QueueCtl("r1") {
		t.Fatalf("aborts %v, want one on %s", s.aborts, QueueCtl("r1"))
	}
	if len(s.forwards) != 0 {
		t.Errorf("forward_cancel %v without an upstream task", s.forwards)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" || s.terminals[0].Canceled != "client" ||
		s.terminals[0].EndAttempt != string(store.OutcomeAborted) {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	// canceled is written only after the dispatch stopped
	if i, j := eventIndex(t, s.events, "dispatched:1"), eventIndex(t, s.events, "terminal:canceled"); j < i {
		t.Errorf("canceled written before the dispatch stopped: %v", s.events)
	}
	r, err := u.result("c1")
	if err != nil || r.(CancelResult).Outcome != CancelCanceled {
		t.Fatalf("cancel result %+v %v", r, err)
	}
	if i, j := eventIndex(t, s.events, "terminal:canceled"), eventIndex(t, s.events, "hook:"+HookOnTerminal); j < i {
		t.Errorf("OnTerminal before canceled: %v", s.events)
	}
}

func TestCancelWithoutReplicaUsesTemporalCancel(t *testing.T) {
	w := newWorld(t)
	w.replica = ""
	w.dispatchFn = runningDispatch(w)
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 200*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.aborts) != 0 {
		t.Errorf("abort on %v with no replica recorded", s.aborts)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if r, err := u.result("c1"); err != nil || r.(CancelResult).Outcome != CancelCanceled {
		t.Fatalf("cancel result %+v %v", r, err)
	}
}

func TestCancelForwardedRefusedTaskContinues(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	finish := make(chan struct{})
	w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
		defer close(finish)
		return ForwardCancelResult{Outcome: CancelUnsupported, UpstreamError: &UpstreamError{Code: -32004, Message: "no cancel here"}}, nil
	}
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		select {
		case <-finish:
		case <-w.release:
		}
		time.Sleep(50 * time.Millisecond)
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 200*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	r, err := u.result("c1")
	if err != nil {
		t.Fatal(err)
	}
	cr := r.(CancelResult)
	if cr.Outcome != CancelUnsupported || cr.UpstreamError == nil || cr.UpstreamError.Code != -32004 {
		t.Fatalf("cancel result %+v", cr)
	}
	if len(s.terminals) != 0 || len(s.aborts) != 0 {
		t.Errorf("the task continues: terminals %+v aborts %v", s.terminals, s.aborts)
	}
	if s.forwards[0] != QueueCtl("r1") {
		t.Errorf("forward_cancel on %s while the dispatch runs", s.forwards[0])
	}
	if !slices.Contains(s.hookCalls, HookOnTerminal+":completed") {
		t.Errorf("the task did not continue to its end: %v", s.hookCalls)
	}
}

func TestCancelForwardedCanceledWaitsForTheDispatch(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	forwarded := make(chan struct{})
	w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
		close(forwarded)
		return ForwardCancelResult{Outcome: CancelCanceled, UpstreamState: "TASK_STATE_CANCELED"}, nil
	}
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		select {
		case <-forwarded:
		case <-w.release:
		}
		time.Sleep(300 * time.Millisecond) // the upstream's canceled status arrives on the stream
		w.setState(string(store.StateCanceled))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 200*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if r, err := u.result("c1"); err != nil || r.(CancelResult).Outcome != CancelCanceled {
		t.Fatalf("cancel result %+v %v", r, err)
	}
	if len(s.terminals) != 0 {
		t.Errorf("the dispatch logged the upstream's canceled status; the workflow wrote %+v", s.terminals)
	}
}

func TestForwardCancelFallsBackToDirectCtl(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.forwardFn = func(_ context.Context, k int, q string) (ForwardCancelResult, error) {
		if q == QueueCtl("r1") {
			return ForwardCancelResult{}, temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_START, nil)
		}
		return ForwardCancelResult{Outcome: CancelNotCancelable, UpstreamError: &UpstreamError{Code: -32002, Message: "busy"}}, nil
	}
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		select {
		case <-ctx.Done():
		case <-w.release:
		case <-time.After(time.Second):
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 100*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if !slices.Equal(s.forwards, []string{QueueCtl("r1"), QueueDirectCtl}) {
		t.Fatalf("forward_cancel queues %v, want the replica's control queue then kontra-direct-ctl (D79)", s.forwards)
	}
	if r, _ := u.result("c1"); r.(CancelResult).Outcome != CancelNotCancelable {
		t.Errorf("cancel result %+v", r)
	}
}

func TestCancelAtInputRequired(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateInputRequired))
		return DispatchResult{Outcome: DispatchInterrupted}, nil
	}
	env := w.env()
	u := newUpdates()
	// Two concurrent cancels get one result; forward_cancel runs once.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{})
		env.UpdateWorkflow(UpdateCancel, "c2", u.cb("c2"), CancelUpdate{})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.forwards) != 1 || s.forwards[0] != QueueDirectCtl {
		t.Fatalf("forward_cancel %v, want once on kontra-direct-ctl", s.forwards)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" || s.terminals[0].EndAttempt != string(store.OutcomeCanceled) {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	for _, id := range []string{"c1", "c2"} {
		if r, err := u.result(id); err != nil || r.(CancelResult).Outcome != CancelCanceled {
			t.Errorf("%s: %+v %v", id, r, err)
		}
	}
	// After it completed, a cancel is refused: the task is terminal.
	if !slices.Contains(s.hookCalls, HookOnTerminal+":canceled") {
		t.Errorf("hooks %v", s.hookCalls)
	}
}

func TestCancelOfTerminalTaskIsRefused(t *testing.T) {
	w := newWorld(t)
	w.reviewWin = hooks.ReviewWindow{Open: true, Seconds: 60} // keeps the workflow open after the end
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 10*time.Second)
	start(env, u, input(2))
	done(t, env)
	if !u.has("c1:rejected") {
		t.Fatalf("a cancel of a terminal task: %v", u.logs)
	}
	if !isType(u.rej["c1"], ErrTypeNotCancelable) {
		t.Errorf("refusal %v, want %s", u.rej["c1"], ErrTypeNotCancelable)
	}
}

// --- kontra.message (spec §7, §14.1) ---

func TestMessageValidator(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	release := make(chan struct{})
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if k == 0 {
			select {
			case <-release:
			case <-w.release:
			}
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	// While the first turn runs: refused, and nothing changes.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2"})
		close(release)
	}, 200*time.Millisecond)
	// At input-required: accepted; the same work item again, and a second follow-up, refused.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-3", u.cb("wi-3"), MessageUpdate{WorkItemID: "wi-3"})
		env.UpdateWorkflow(UpdateMessage, "wi-3b", u.cb("wi-3b"), MessageUpdate{WorkItemID: "wi-3"})
		env.UpdateWorkflow(UpdateMessage, "wi-4", u.cb("wi-4"), MessageUpdate{WorkItemID: "wi-4"})
	}, time.Hour/2)
	start(env, u, input(2))
	done(t, env)
	for id, want := range map[string]string{"wi-1": "completed", "wi-2": "rejected", "wi-3": "completed", "wi-3b": "rejected", "wi-4": "rejected"} {
		if !u.has(id + ":" + want) {
			t.Errorf("%s: want %s; got %v", id, want, u.logs)
		}
	}
	if r, _ := u.result("wi-3"); r.(MessageResult).Turn != 2 {
		t.Errorf("follow-up turn %+v", r)
	}
	s := w.snapshot()
	if len(s.dispatches) != 2 || s.dispatches[1].in.WorkItemID != "wi-3" || s.dispatches[1].in.N != 1 || s.dispatches[1].in.Resume {
		t.Fatalf("dispatches %+v: the follow-up runs on the same attempt", s.dispatches)
	}
	if len(s.prepares) != 1 {
		t.Errorf("prepares %+v", s.prepares)
	}
}

func TestTurnLimit(t *testing.T) {
	w := newWorld(t)
	w.deadline = t0.Add(1000 * time.Hour)
	w.firstID[1] = true
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		return DispatchResult{Outcome: DispatchInterrupted}, nil
	}
	env := w.env()
	u := newUpdates()
	// Each follow-up is sent a minute after the previous one completed (the test environment holds at
	// most 1,000 pending callbacks).
	var send func(k int)
	send = func(k int) {
		if k > MaxTurns+1 {
			return
		}
		id := fmt.Sprintf("wi-%d", k)
		cb := u.cb(id)
		complete := cb.OnComplete
		cb.OnComplete = func(r any, err error) {
			complete(r, err)
			env.RegisterDelayedCallback(func() { send(k + 1) }, time.Minute)
		}
		env.UpdateWorkflow(UpdateMessage, id, cb, MessageUpdate{WorkItemID: id})
	}
	env.RegisterDelayedCallback(func() { send(2) }, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != MaxTurns {
		t.Errorf("%d dispatches, want %d", len(s.dispatches), MaxTurns)
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != "upstream_error" || !strings.Contains(s.terminals[0].Text, "1000 turns") {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	// The task ends while its attempt waits at input-required: write_terminal ends the attempt, which it
	// records interrupted (D77).
	if s.terminals[0].EndAttempt != string(store.OutcomeUpstreamError) {
		t.Errorf("the 1,001st turn's terminal write ends no attempt: %+v", s.terminals[0])
	}
}

// --- hooks, pushes, patches (spec §10.2, §13.3) ---

func TestReviewWindowClosedEnds(t *testing.T) {
	w := newWorld(t)
	w.onTerminal = hooks.Patch{Metadata: map[string]any{"kontra.escrow.status": "released"}}
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if !slices.Equal(s.hookCalls, []string{HookOnTerminal + ":completed", HookReviewWindow}) {
		t.Fatalf("hooks %v: OnTerminal then ReviewWindow, and the end", s.hookCalls)
	}
	if len(s.patches) != 1 || s.patches[0].Source != HookOnTerminal {
		t.Fatalf("patches %+v", s.patches)
	}
}

func TestReviewWindowMessageAndDispute(t *testing.T) {
	w := newWorld(t)
	w.reviewWin = hooks.ReviewWindow{Open: true, Seconds: 3600}
	w.reviewFn = func(a hooks.ReviewMessageArgs) hooks.ReviewMessageResult {
		return hooks.ReviewMessageResult{Accept: true, Dispute: a.Metadata["kontra.escrow.dispute"] != nil,
			Patch: &hooks.Patch{Metadata: map[string]any{"kontra.escrow.status": "disputed"}}}
	}
	// OnReviewClosed holds the workflow open until kontra.close.
	w.onClosed = hooks.Patch{Metadata: map[string]any{"kontra.escrow.status": "disputed"}, HoldOpenSeconds: 86400}
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateReviewMessage, "rv-1", u.cb("rv-1"), ReviewMessageUpdate{WorkItemID: "rv-1", MessageID: "m",
			Metadata: map[string]any{"kontra.escrow.dispute": "late"}})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateReviewMessage, "rv-2", u.cb("rv-2"), ReviewMessageUpdate{WorkItemID: "rv-2", MessageID: "m2"})
	}, 2*time.Minute)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalClose, struct{}{}) }, 3*time.Minute)
	start(env, u, input(2))
	done(t, env)
	if end := env.Now(); end.After(t0.Add(time.Hour)) {
		t.Errorf("the workflow ended at %v: kontra.close should have ended the hold at 3 min", end.Sub(t0))
	}
	r, err := u.result("rv-1")
	if err != nil || !r.(ReviewMessageResult).Accepted || r.(ReviewMessageResult).Seq == nil || *r.(ReviewMessageResult).Seq != 11 {
		t.Fatalf("review message %+v %v", r, err)
	}
	if !u.has("rv-2:rejected") {
		t.Errorf("a review message after the dispute closed the window: %v", u.logs)
	}
	s := w.snapshot()
	if len(s.appends) != 1 || s.appends[0].WorkItemID != "rv-1" {
		t.Errorf("appends %+v", s.appends)
	}
	if !slices.Contains(s.hookCalls, HookOnReviewClosed+":true") {
		t.Errorf("hooks %v: OnReviewClosed{disputed: true} after the dispute", s.hookCalls)
	}
	if i, j := eventIndex(t, s.events, "append:rv-1"), eventIndex(t, s.events, "patch:"+HookOnReviewMessage); j < i {
		t.Errorf("the hook's patch applied before the append committed: %v", s.events)
	}
}

func TestPushFanOutAndTheWorkflowOutlivesItsPushes(t *testing.T) {
	w := newWorld(t)
	w.pushCfgs = [][]string{{"c1"}, {"c1", "c2"}}
	pushDone := make(chan struct{})
	w.pushFn = func(ctx context.Context, in PushInput) (PushResult, error) {
		if in.ConfigID == "c2" {
			// The webhook is down across the end of the task.
			select {
			case <-pushDone:
			case <-w.release:
			}
		}
		w.event("pushed:" + in.ConfigID)
		return PushResult{Stopped: "terminal"}, nil
	}
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "c3"}) }, 0)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "c1"}) }, 0)
	go func() {
		time.Sleep(time.Second)
		close(pushDone)
	}()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	got := slices.Clone(s.pushes)
	slices.Sort(got)
	if !slices.Equal(got, []string{"c1", "c2", "c3"}) {
		t.Fatalf("push_webhook started for %v, want one per config", s.pushes)
	}
	for _, q := range s.pushQueues {
		if q != QueuePush {
			t.Errorf("push_webhook on %s", q)
		}
	}
	if s.lists < 2 {
		t.Errorf("list_push_configs ran %d times: at start and after each dispatch", s.lists)
	}
	// The workflow completed only after the last push_webhook (D78).
	if eventIndex(t, s.events, "pushed:c2") < eventIndex(t, s.events, "hook:"+HookOnTerminal) {
		t.Errorf("c2 delivered before the end: %v", s.events)
	}
}

func TestAnnotationsHeldWhileADispatchRuns(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	release := make(chan struct{})
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		select {
		case <-release:
		case <-w.release:
		}
		w.setState(string(store.StateInputRequired))
		return DispatchResult{Outcome: DispatchInterrupted}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateAnnotate, "a1", u.cb("a1"), AnnotateUpdate{Metadata: map[string]any{"kontra.escrow.note": "x"}})
		env.UpdateWorkflow(UpdateAnnotate, "a2", u.cb("a2"), AnnotateUpdate{Metadata: map[string]any{"caller.key": "x"}})
		go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	}, 100*time.Millisecond)
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, time.Hour/2)
	in := input(2)
	in.Hooks.OperatorPrefixes = []string{"kontra.escrow."}
	start(env, u, in)
	done(t, env)
	if !u.has("a1:completed") || !u.has("a2:rejected") {
		t.Fatalf("annotate: %v (a key outside the operator's prefixes is refused)", u.logs)
	}
	s := w.snapshot()
	if i, j := eventIndex(t, s.events, "dispatched:1"), eventIndex(t, s.events, "patch:"+UpdateAnnotate); j < i {
		t.Errorf("the annotation was applied while the dispatch ran: %v", s.events)
	}
}

func TestDispatchedSignalRunsOnDispatched(t *testing.T) {
	w := newWorld(t)
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalDispatched, DispatchedSignal{N: 1}) }, 0)
	start(env, newUpdates(), input(2))
	done(t, env)
	if !slices.Contains(w.snapshot().hookCalls, HookOnDispatched) {
		t.Errorf("hooks %v", w.snapshot().hookCalls)
	}
}

// --- quotes (spec §13.2) ---

func quotedInput() Input {
	in := input(2)
	exp := t0.Add(10 * time.Minute)
	in.QuoteExpiresAt = &exp
	in.Hooks.OperatorPrefixes = []string{"kontra.escrow."}
	return in
}

func TestQuoteApproved(t *testing.T) {
	w := newWorld(t)
	w.state = string(store.StateQuoted)
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateApproval, "bad", u.cb("bad"), ApprovalUpdate{Accepted: true, Metadata: map[string]any{"x": 1}})
		env.UpdateWorkflow(UpdateApproval, "ok", u.cb("ok"), ApprovalUpdate{Accepted: true, Metadata: map[string]any{"kontra.escrow.status": "held"}})
	}, time.Minute)
	start(env, u, quotedInput())
	done(t, env)
	if !u.has("bad:rejected") {
		t.Errorf("an approval with a key outside the prefixes: %v", u.logs)
	}
	if r, err := u.result("ok"); err != nil || r.(ApprovalResult).State != "submitted" {
		t.Fatalf("approval %+v %v", r, err)
	}
	s := w.snapshot()
	if len(s.submits) != 1 || s.submits[0].Metadata["kontra.escrow.status"] != "held" {
		t.Fatalf("submits %+v", s.submits)
	}
	if len(s.dispatches) != 1 || s.dispatches[0].at.Before(t0.Add(time.Minute)) {
		t.Fatalf("dispatches %+v: nothing is dispatched before the approval", s.dispatches)
	}
}

func TestQuoteExpires(t *testing.T) {
	w := newWorld(t)
	w.state = string(store.StateQuoted)
	w.dispatchFn = outcome(DispatchTerminal, "")
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateApproval, "late", u.cb("late"), ApprovalUpdate{Accepted: true})
	}, 11*time.Minute)
	start(env, u, quotedInput())
	done(t, env)
	s := w.snapshot()
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" || s.terminals[0].Canceled != store.CanceledQuoteExpired {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if len(s.dispatches) != 0 {
		t.Errorf("dispatched an expired quote")
	}
	if u.has("late:completed") {
		t.Error("an approval after the expiry was accepted")
	}
}

func TestQuoteDeclined(t *testing.T) {
	w := newWorld(t)
	w.state = string(store.StateQuoted)
	w.dispatchFn = outcome(DispatchTerminal, "")
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateApproval, "no", u.cb("no"), ApprovalUpdate{Accepted: false})
	}, time.Minute)
	start(env, u, quotedInput())
	done(t, env)
	s := w.snapshot()
	if len(s.terminals) != 1 || s.terminals[0].Canceled != store.CanceledDeclined {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if r, _ := u.result("no"); r.(ApprovalResult).State != "canceled" {
		t.Errorf("approval result %+v", r)
	}
}

// A review message the caller sends as soon as it read completed from the log, before the workflow learned
// that the turn ended: it waits for the window, and is accepted when it opens (spec §7).
func TestReviewMessageBeforeTheWorkflowLearnsTheEnd(t *testing.T) {
	for _, open := range []bool{true, false} {
		t.Run(fmt.Sprintf("window open %v", open), func(t *testing.T) {
			w := newWorld(t)
			w.reviewWin = hooks.ReviewWindow{Open: open, Seconds: 60}
			w.reviewFn = func(hooks.ReviewMessageArgs) hooks.ReviewMessageResult {
				return hooks.ReviewMessageResult{Accept: true}
			}
			sent := make(chan struct{})
			w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
				w.setState(string(store.StateCompleted))
				select {
				case <-sent:
				case <-w.release:
				}
				time.Sleep(100 * time.Millisecond)
				return DispatchResult{Outcome: DispatchTerminal}, nil
			}
			env := w.env()
			u := newUpdates()
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow(UpdateReviewMessage, "rv-1", u.cb("rv-1"), ReviewMessageUpdate{WorkItemID: "rv-1", MessageID: "m"})
				close(sent)
			}, 100*time.Millisecond)
			start(env, u, input(2))
			done(t, env)
			if !u.has("rv-1:accepted") {
				t.Fatalf("the review message was refused while the workflow lagged the log: %v", u.logs)
			}
			r, err := u.result("rv-1")
			if err != nil || r.(ReviewMessageResult).Accepted != open {
				t.Fatalf("review message %+v %v, want accepted %v", r, err, open)
			}
			s := w.snapshot()
			if open && (len(s.appends) != 1 || eventIndex(t, s.events, "hook:"+HookReviewWindow) > eventIndex(t, s.events, "append:rv-1")) {
				t.Errorf("events %v: the append follows the window's opening", s.events)
			}
			if !open && len(s.appends) != 0 {
				t.Errorf("appended %v with no window", s.appends)
			}
		})
	}
}

// An abort its replica's control queue does not start (the replica died, its dispatch's heartbeat has not
// timed out yet): the workflow waits for the dispatch's heartbeat timeout, then applies the cancel table
// (D79).
func TestCancelWhenTheReplicaIsGone(t *testing.T) {
	w := newWorld(t)
	w.abortErr = temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_START, nil)
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-w.release:
		}
		return DispatchResult{}, heartbeatTimeout()
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 100*time.Millisecond)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.aborts) != 1 {
		t.Fatalf("aborts %v", s.aborts)
	}
	if len(s.dispatches) != 1 || len(s.prepares) != 1 {
		t.Errorf("dispatches %d prepares %d: a cancel stops the task, it never retries it", len(s.dispatches), len(s.prepares))
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if r, err := u.result("c1"); err != nil || r.(CancelResult).Outcome != CancelCanceled {
		t.Fatalf("cancel %+v %v", r, err)
	}
}

func TestMessageRefusedWhileQuoted(t *testing.T) {
	w := newWorld(t)
	w.state = string(store.StateQuoted)
	w.dispatchFn = outcome(DispatchTerminal, "")
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2"})
	}, time.Minute)
	start(env, u, quotedInput())
	done(t, env)
	if !u.has("wi-1:completed") || !u.has("wi-2:rejected") {
		t.Fatalf("updates %v: the quoted task's first message is accepted with the start; another is refused", u.logs)
	}
}

// --- push listing at start (spec §10.2) ---

// A new task's inline push config gets no kontra.push (Queue.Write saves it in the creating transaction):
// the workflow lists the task's push configs at start, so a task that ends before its first prepare
// succeeds still delivers its terminal snapshot, and a quoted task's webhook gets the quoted snapshot
// before the quote is decided.
func TestPushConfigsListedAtStart(t *testing.T) {
	t.Run("prepare service_unavailable", func(t *testing.T) {
		w := newWorld(t)
		w.pushCfgs = [][]string{{"c-inline"}}
		env := w.env()
		env.RegisterActivityWithOptions(func(ctx context.Context, in PrepareInput) (PrepareResult, error) {
			return PrepareResult{}, temporal.NewNonRetryableApplicationError("gone", ErrTypeServiceUnavailable, nil)
		}, activity.RegisterOptions{Name: ActPrepare})
		w.dispatchFn = outcome(DispatchTerminal, "")
		start(env, newUpdates(), input(2))
		done(t, env)
		s := w.snapshot()
		if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonServiceUnavailable) {
			t.Fatalf("terminal writes %+v", s.terminals)
		}
		if s.lists < 1 || !slices.Equal(s.pushes, []string{"c-inline"}) {
			t.Fatalf("lists %d, pushes %v: the inline config's push_webhook never started", s.lists, s.pushes)
		}
	})
	t.Run("quote declined", func(t *testing.T) {
		w := newWorld(t)
		w.state = string(store.StateQuoted)
		w.pushCfgs = [][]string{{"c-inline"}}
		w.dispatchFn = outcome(DispatchTerminal, "")
		env := w.env()
		u := newUpdates()
		env.RegisterDelayedCallback(func() {
			env.UpdateWorkflow(UpdateApproval, "no", u.cb("no"), ApprovalUpdate{Accepted: false})
		}, time.Minute)
		start(env, u, quotedInput())
		done(t, env)
		s := w.snapshot()
		if len(s.terminals) != 1 || s.terminals[0].Canceled != store.CanceledDeclined {
			t.Fatalf("terminal writes %+v", s.terminals)
		}
		if !slices.Equal(s.pushes, []string{"c-inline"}) {
			t.Fatalf("pushes %v: the quoted task's webhook got nothing", s.pushes)
		}
		// The webhook was following the task while it was quoted, before the decline.
		if i, j := eventIndex(t, s.events, "push:c-inline"), eventIndex(t, s.events, "terminal:canceled"); i > j {
			t.Errorf("push_webhook started only after the decline: %v", s.events)
		}
	})
	t.Run("quote expired", func(t *testing.T) {
		w := newWorld(t)
		w.state = string(store.StateQuoted)
		w.pushCfgs = [][]string{{"c-inline"}}
		w.dispatchFn = outcome(DispatchTerminal, "")
		env := w.env()
		start(env, newUpdates(), quotedInput())
		done(t, env)
		s := w.snapshot()
		if len(s.terminals) != 1 || s.terminals[0].Canceled != store.CanceledQuoteExpired || !slices.Equal(s.pushes, []string{"c-inline"}) {
			t.Fatalf("terminal writes %+v, pushes %v", s.terminals, s.pushes)
		}
	})
}

// --- a follow-up turn's dispatch that never started (spec §6.4, §7) ---

func scheduleToStartTimeout() error {
	return temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_START, nil)
}

// A schedule-to-start timeout proves the dispatch never ran: nothing consumed the follow-up's work item and
// nothing was sent upstream. The turn is dispatched again as the turn's first dispatch, on the same
// attempt, never re-attached (a re-attach would end the turn at the upstream's unchanged input-required
// without sending the follow-up).
func TestFollowUpDispatchNotStartedIsSentAgain(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		switch k {
		case 0:
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		case 1:
			return DispatchResult{}, scheduleToStartTimeout()
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 3 || len(s.prepares) != 1 {
		t.Fatalf("dispatches %d prepares %d", len(s.dispatches), len(s.prepares))
	}
	for i := 1; i <= 2; i++ {
		if d := s.dispatches[i]; d.in.N != 1 || d.in.WorkItemID != "wi-2" || d.in.Resume || d.queue != QueueDirect {
			t.Errorf("dispatch %d: %+v on %s, want the follow-up's first dispatch on attempt 1 again (resume false)", i, d.in, d.queue)
		}
	}
	if gap := s.dispatches[2].at.Sub(s.dispatches[1].at); gap < time.Second {
		t.Errorf("dispatched again after %v, want a 1 s backoff", gap)
	}
	if len(s.terminals) != 0 {
		t.Errorf("terminal writes %+v", s.terminals)
	}
}

// A resume dispatch that was not started is a resume again: its turn's item was consumed already.
func TestResumeNotStartedStaysAResume(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		switch k {
		case 0:
			return DispatchResult{Outcome: DispatchContextLost}, nil
		case 1:
			return DispatchResult{}, scheduleToStartTimeout()
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 3 || len(s.prepares) != 1 {
		t.Fatalf("dispatches %d prepares %d", len(s.dispatches), len(s.prepares))
	}
	for i := 1; i <= 2; i++ {
		if d := s.dispatches[i]; d.in.N != 1 || !d.in.Resume {
			t.Errorf("dispatch %d: %+v, want a resume of attempt 1", i, d.in)
		}
	}
}

// With no kontra-direct poller at all, the follow-up's dispatch is tried again with the resumes' backoff
// and cap, and the task then fails (upstream_unreachable: no gate replica took the dispatch).
func TestFollowUpDispatchNeverStartedFails(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if k == 0 {
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		}
		return DispatchResult{}, scheduleToStartTimeout()
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 2+MaxResumes {
		t.Fatalf("%d dispatches, want turn 1, the follow-up and %d more", len(s.dispatches), MaxResumes)
	}
	for _, d := range s.dispatches[1:] {
		if d.in.Resume || d.in.WorkItemID != "wi-2" || d.in.N != 1 {
			t.Errorf("dispatch %+v", d.in)
		}
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonUpstreamUnreachable) ||
		s.terminals[0].EndAttempt != string(store.OutcomeUpstreamUnreachable) {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

// --- which refusal a busy task's kontra.message gets (spec §7) ---

// A follow-up answering the turn end the workflow already processed, while another follow-up's turn runs,
// is refused for good (a concurrent second follow-up). One whose caller read a later turn end than the
// workflow has processed is refused as behind: the work queue sends it again while the log still shows
// that turn end.
func TestMessageRefusalSaysWhetherTheWorkflowIsBehind(t *testing.T) {
	w := newWorld(t)
	w.firstID[1] = true
	release := make(chan struct{})
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		switch k {
		case 0:
			select {
			case <-release:
			case <-w.release:
			}
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		case 1:
			select {
			case <-ctx.Done():
			case <-w.release:
			case <-time.After(time.Second):
			}
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	// Turn 1 runs; the caller read input-required at seq 5 before the workflow learned it.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "early", u.cb("early"), MessageUpdate{WorkItemID: "early", After: 5})
		env.UpdateWorkflow(UpdateMessage, "blind", u.cb("blind"), MessageUpdate{WorkItemID: "blind"})
		close(release)
	}, 100*time.Millisecond)
	// At input-required: wi-2 is accepted; wi-3, answering the same turn end, is refused for good; wi-4,
	// whose caller read a later turn end (wi-2's), is behind.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 5})
		env.UpdateWorkflow(UpdateMessage, "wi-3", u.cb("wi-3"), MessageUpdate{WorkItemID: "wi-3", After: 5})
		env.UpdateWorkflow(UpdateMessage, "wi-4", u.cb("wi-4"), MessageUpdate{WorkItemID: "wi-4", After: 9})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	if !u.has("wi-2:completed") {
		t.Fatalf("wi-2: %v", u.logs)
	}
	for id, want := range map[string]string{"early": ErrTypeBehind, "blind": ErrTypeRefused, "wi-3": ErrTypeRefused, "wi-4": ErrTypeBehind} {
		if !u.has(id+":rejected") || !isType(u.rej[id], want) {
			t.Errorf("%s: %v, want a %s refusal (%v)", id, u.rej[id], want, u.logs)
		}
	}
}

// --- OnReviewMessage is bounded from its scheduling (spec §14.1) ---

// A remote hook queue with no poller never starts OnReviewMessage: only a schedule-to-close bound turns
// that into accept: false; without one the Update, and the workflow's end, wait forever.
func TestReviewMessageHookHasAScheduleToCloseBound(t *testing.T) {
	w := newWorld(t)
	w.reviewWin = hooks.ReviewWindow{Open: true, Seconds: 60}
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	var got time.Duration
	env.RegisterActivityWithOptions(func(ctx context.Context, t hooks.TaskRef, a hooks.ReviewMessageArgs) (hooks.ReviewMessageResult, error) {
		got = activity.GetInfo(ctx).ScheduleToCloseTimeout
		return hooks.ReviewMessageResult{Accept: true}, nil
	}, activity.RegisterOptions{Name: HookOnReviewMessage})
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateReviewMessage, "rv-1", u.cb("rv-1"), ReviewMessageUpdate{WorkItemID: "rv-1", MessageID: "m"})
	}, 10*time.Second)
	start(env, u, input(2))
	done(t, env)
	if got != reviewMessageStartToClose {
		t.Fatalf("OnReviewMessage schedule-to-close %v, want %v", got, reviewMessageStartToClose)
	}
}

// --- a push_webhook whose config ended (spec §10.2, §14.1: kontra.push starts or ends a push_webhook) ---

// A push_webhook that ended because its config was deleted, or marked failed, no longer holds the config's
// place in the run: the config created again with the same id (or a failed one set again) gets a new
// push_webhook from its kontra.push, and when that signal raced the end of the old activity (it arrived
// while the old one was still running, so it was dropped), the listing that follows the end starts it.
func TestPushRestartsAfterItsConfigEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string // how the first push_webhook ends
	}{
		{"deleted then created again", "deleted"},
		{"failed then set again", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.pushCfgs = [][]string{{}}
			var n int
			w.pushFn = func(ctx context.Context, in PushInput) (PushResult, error) {
				w.mu.Lock()
				n++
				k := n
				w.mu.Unlock()
				if k == 1 {
					return PushResult{Stopped: tc.first}, nil
				}
				return PushResult{Stopped: "terminal"}, nil
			}
			w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
				w.setState(string(store.StateInputRequired))
				return DispatchResult{Outcome: DispatchInterrupted}, nil
			}
			env := w.env()
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1"}) }, time.Minute)
			if tc.first == "deleted" {
				env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1", Deleted: true}) }, 5*time.Minute)
			}
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1"}) }, 6*time.Minute)
			start(env, newUpdates(), input(0))
			done(t, env)
			if s := w.snapshot(); !slices.Equal(s.pushes, []string{"cfg-1", "cfg-1"}) {
				t.Fatalf("push_webhook ran for %v: the config set again after its push_webhook ended %s got none", s.pushes, tc.first)
			}
		})
	}
	t.Run("the signal raced the end", func(t *testing.T) {
		w := newWorld(t)
		// list_push_configs: nothing at the start; the config (created again) once the first activity ended.
		w.pushCfgs = [][]string{{}, {"cfg-1"}}
		signaled := make(chan struct{})
		second := make(chan struct{})
		var n int
		w.pushFn = func(ctx context.Context, in PushInput) (PushResult, error) {
			w.mu.Lock()
			n++
			k := n
			w.mu.Unlock()
			if k == 1 {
				// The config is deleted and created again while this activity runs; it ends "deleted" only
				// after the new config's kontra.push reached the workflow.
				select {
				case <-signaled:
				case <-w.release:
				}
				return PushResult{Stopped: "deleted"}, nil
			}
			if k == 2 {
				close(second)
			}
			return PushResult{Stopped: "terminal"}, nil
		}
		w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
			select { // the turn ends once the config got its new push_webhook (or the test gives up)
			case <-second:
			case <-time.After(5 * time.Second):
			}
			w.setState(string(store.StateCompleted))
			return DispatchResult{Outcome: DispatchTerminal}, nil
		}
		env := w.env()
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1"}) }, 0)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1", Deleted: true})
			env.SignalWorkflow(SignalPush, PushSignal{ConfigID: "cfg-1"})
			close(signaled)
		}, time.Second)
		start(env, newUpdates(), input(0))
		done(t, env)
		s := w.snapshot()
		if !slices.Equal(s.pushes, []string{"cfg-1", "cfg-1"}) {
			t.Fatalf("push_webhook ran for %v (lists %d): the config created again while the old activity ran got none", s.pushes, s.lists)
		}
	})
}

// A lost dispatch (heartbeat timeout) whose process lives on records attempt 1's first upstream task id
// after the workflow read attempt_state and decided to retry (D77). prepare(2) refuses the retry, and the
// workflow re-reads attempt_state and re-attaches to attempt 1: no dispatch of attempt 2 ever sends the
// caller's message again (spec §6.4).
func TestRetryRefusedAfterALateFirstUpstreamIDReattaches(t *testing.T) {
	w := newWorld(t)
	w.prepareFn = func(in PrepareInput) error {
		if in.N == 1 {
			return nil
		}
		// The old dispatch's SetFirstUpstreamID committed during the backoff.
		w.mu.Lock()
		w.firstID[1] = true
		w.mu.Unlock()
		return temporal.NewNonRetryableApplicationError("attempt 1 has a first upstream task id", ErrTypeAttemptHasUpstream, nil)
	}
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if k == 0 {
			return DispatchResult{}, heartbeatTimeout()
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 2 || s.prepares[1].N != 2 {
		t.Fatalf("prepares %+v, want 1 and the refused 2", s.prepares)
	}
	if len(s.dispatches) != 2 {
		t.Fatalf("dispatches %+v", s.dispatches)
	}
	if r := s.dispatches[1].in; r.N != 1 || !r.Resume {
		t.Errorf("after the refused retry: dispatch %+v, want a re-attach of attempt 1", r)
	}
	if len(s.terminals) != 0 {
		t.Errorf("terminal writes %+v: the refusal is not the deadline", s.terminals)
	}
	if !slices.Contains(s.hookCalls, HookOnTerminal+":completed") {
		t.Errorf("hooks %v", s.hookCalls)
	}
}

// A kontra.dispatched for an attempt the workflow has left (a stale dispatch of attempt 1 while attempt 2
// runs) runs no OnDispatched (D77); the current attempt's does.
func TestDispatchedSignalOfAnOlderAttemptIsIgnored(t *testing.T) {
	w := newWorld(t)
	var env *testsuite.TestWorkflowEnvironment
	w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
		if k == 0 {
			return DispatchResult{Outcome: DispatchRetryable, Reason: string(store.ReasonUpstreamUnreachable)}, nil
		}
		env.SignalWorkflow(SignalDispatched, DispatchedSignal{N: 1})
		env.SignalWorkflow(SignalDispatched, DispatchedSignal{N: 2})
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env = w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	var n int
	for _, h := range w.snapshot().hookCalls {
		if h == HookOnDispatched {
			n++
		}
	}
	if n != 1 {
		t.Errorf("OnDispatched ran %d times, want once (attempt 2's): %v", n, w.snapshot().hookCalls)
	}
}

// forward_cancel answered with the upstream's task in a state that is not canceled (A2A 1.0.1 §3.1.5: the
// call "returns its updated state" and success is not guaranteed): the gate writes no canceled, the
// cancel's outcome says the task continues, and the task does. Both the activity's own answer (continues)
// and an older replica's (canceled, with the upstream's state) are taken that way (spec §8).
func TestCancelAnsweredWithARunningTaskContinues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input bool // the task waits at input-required (no dispatch runs)
		res   ForwardCancelResult
		want  CancelOutcome
	}{
		{"input-required, older replica", true, ForwardCancelResult{Outcome: CancelCanceled, UpstreamState: "TASK_STATE_INPUT_REQUIRED"}, CancelContinues},
		{"input-required", true, ForwardCancelResult{Outcome: CancelContinues, UpstreamState: "TASK_STATE_INPUT_REQUIRED"}, CancelContinues},
		{"working, older replica", false, ForwardCancelResult{Outcome: CancelCanceled, UpstreamState: "TASK_STATE_WORKING"}, CancelContinues},
		{"working", false, ForwardCancelResult{Outcome: CancelContinues, UpstreamState: "TASK_STATE_WORKING"}, CancelContinues},
		{"completed, older replica", false, ForwardCancelResult{Outcome: CancelCanceled, UpstreamState: "TASK_STATE_COMPLETED"}, CancelNotCancelable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.firstID[1] = true
			forwarded := make(chan struct{})
			w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
				close(forwarded)
				return tc.res, nil
			}
			if tc.input {
				w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
					w.setState(string(store.StateInputRequired))
					return DispatchResult{Outcome: DispatchInterrupted}, nil
				}
			} else {
				w.dispatchFn = func(ctx context.Context, k int, in DispatchInput) (DispatchResult, error) {
					select {
					case <-forwarded:
					case <-w.release:
					}
					time.Sleep(100 * time.Millisecond) // the seller keeps working, then completes
					w.setState(string(store.StateCompleted))
					return DispatchResult{Outcome: DispatchTerminal}, nil
				}
			}
			env := w.env()
			u := newUpdates()
			at := 200 * time.Millisecond // while the dispatch runs
			if tc.input {
				at = time.Minute
			}
			env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, at)
			start(env, u, input(2))
			done(t, env)
			s := w.snapshot()
			r, err := u.result("c1")
			if err != nil || r.(CancelResult).Outcome != tc.want {
				t.Fatalf("cancel result %+v %v, want %s", r, err, tc.want)
			}
			for _, tw := range s.terminals {
				if tw.State == string(store.StateCanceled) {
					t.Errorf("canceled written for an upstream that answered %s: %+v", tc.res.UpstreamState, s.terminals)
				}
			}
			if slices.Contains(s.hookCalls, HookOnTerminal+":canceled") {
				t.Errorf("hooks %v", s.hookCalls)
			}
		})
	}
}
