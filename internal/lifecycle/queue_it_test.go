package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/proto"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/temporaltest"
)

// RecordEnv, when set to 1, makes TestQueueWriteOverTemporal write the histories of its workflows to
// testdata/histories, which TestReplayRecordedHistories replays (spec §14.4).
const RecordEnv = "KONTRA_RECORD_HISTORIES"

type ownerKey struct{}

func ownerFrom(ctx context.Context) (store.Owner, error) {
	o, ok := ctx.Value(ownerKey{}).(store.Owner)
	if !ok {
		return store.Owner{}, a2a.ErrUnauthenticated
	}
	return o, nil
}

// caller is a request context as the interceptors leave it: the owner and, for a new task, the id
// Admission pre-minted.
func caller(preminted bool) (context.Context, string) {
	ctx := context.WithValue(context.Background(), ownerKey{}, buyer)
	if !preminted {
		return ctx, ""
	}
	id, _ := ids.NewTaskID()
	return hooks.WithAdmitted(ctx, hooks.Admitted{TaskID: string(id), Decision: hooks.Decision{Outcome: hooks.Allow}}), string(id)
}

func payloadOf(req *a2a.SendMessageRequest) *workqueue.Payload {
	return &workqueue.Payload{Type: workqueue.PayloadTypeExecute, TaskID: a2a.NewTaskID(), ExecuteRequest: req, CallContext: callContextOf(buyer)}
}

// turn is what the fake dispatch does with one dispatch of a task: the state it logs, the outcome it
// returns, and, when hold is set, a channel it waits on first (before it writes anything); when holdAfter
// is set, a channel it waits on after logging the state, before it returns.
type turn struct {
	state     a2a.TaskState
	outcome   DispatchOutcome
	reason    string
	hold      chan struct{}
	holdAfter chan struct{}
}

// fakeDispatch stands in for the relay's Dispatcher on kontra-direct and the control queues: it consumes
// the work item as a turn's first dispatch does (D66), checks that the item is sanitized and rebuilds,
// logs a status through the store, and returns the scripted outcome.
type fakeDispatch struct {
	t      *testing.T
	log    *store.Log
	mu     sync.Mutex
	script map[string][]turn // per task id, one turn per dispatch
	calls  map[string][]DispatchInput
}

func (d *fakeDispatch) set(taskID string, turns ...turn) {
	d.mu.Lock()
	d.script[taskID] = turns
	d.mu.Unlock()
}

func (d *fakeDispatch) count(taskID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls[taskID])
}

func (d *fakeDispatch) Dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error) {
	d.mu.Lock()
	k := len(d.calls[in.TaskID])
	d.calls[in.TaskID] = append(d.calls[in.TaskID], in)
	script := d.script[in.TaskID]
	d.mu.Unlock()
	if k >= len(script) {
		return DispatchResult{}, errors.New("fake dispatch: no script for this dispatch")
	}
	tr := script[k]
	id, wid := uuid.MustParse(in.TaskID), uuid.MustParse(in.WorkItemID)
	if err := d.log.ConsumeWorkItem(ctx, wid, id); err != nil && !(errors.Is(err, store.ErrWorkItemConsumed) && in.N > 1) {
		return DispatchResult{}, err
	}
	wi, err := d.log.LoadWorkItem(ctx, wid, id)
	if err != nil {
		return DispatchResult{}, err
	}
	if _, err := Rebuild(a2a.TaskID(in.TaskID), wi); err != nil {
		return DispatchResult{}, err
	}
	if tr.hold != nil {
		select {
		case <-tr.hold:
		case <-ctx.Done():
			return DispatchResult{Outcome: DispatchAborted}, nil
		}
	}
	if tr.state != "" {
		if _, err := d.log.BeginDispatch(ctx, id, in.N, "it-replica"); err != nil {
			return DispatchResult{}, err
		}
		if err := d.log.SetFirstUpstreamID(ctx, id, in.N, "upstream-"+in.TaskID, "upctx"); err != nil {
			return DispatchResult{}, err
		}
		if err := writeStatus(ctx, d.log, id, tr.state); err != nil {
			return DispatchResult{}, err
		}
	}
	if tr.holdAfter != nil {
		select {
		case <-tr.holdAfter:
		case <-ctx.Done():
			return DispatchResult{Outcome: DispatchContextLost}, nil
		}
	}
	if tr.outcome == DispatchTerminal || tr.outcome == DispatchRetryable {
		o := store.Outcome(tr.reason)
		if tr.outcome == DispatchTerminal {
			o = store.OutcomeCompleted
		}
		_ = d.log.EndAttempt(ctx, id, in.N, o)
	}
	return DispatchResult{Outcome: tr.outcome, Reason: tr.reason}, nil
}

func (d *fakeDispatch) Abort(context.Context, AttemptRef) error { return nil }

func (d *fakeDispatch) ForwardCancel(context.Context, AttemptRef) (ForwardCancelResult, error) {
	return ForwardCancelResult{Outcome: CancelCanceled, UpstreamState: string(a2a.TaskStateCanceled)}, nil
}

func writeStatus(ctx context.Context, log *store.Log, id uuid.UUID, state a2a.TaskState) error {
	row, err := log.Load(ctx, id)
	if err != nil {
		return err
	}
	ev := a2a.NewStatusUpdateEvent(row.Snapshot, state, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("status "+string(state))))
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
	st, err := store.StateFromA2A(state)
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, nil, store.Write{TaskID: id, PrevVersion: row.Version, Kind: store.KindStatus, State: st,
		StatusAt: store.StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload})
	return err
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *pgEnv) state(id string) store.State {
	row, err := e.log.Load(context.Background(), uuid.MustParse(id))
	if err != nil {
		e.t.Fatal(err)
	}
	return row.State
}

func (e *pgEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func history(t *testing.T, c client.Client, taskID string) *historypb.History {
	t.Helper()
	it := c.GetWorkflowHistory(context.Background(), WorkflowID(a2a.TaskID(taskID)), "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	h := &historypb.History{}
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		h.Events = append(h.Events, ev)
	}
	return h
}

func closedStatus(t *testing.T, c client.Client, taskID string) enums.WorkflowExecutionStatus {
	t.Helper()
	d, err := c.DescribeWorkflowExecution(context.Background(), WorkflowID(a2a.TaskID(taskID)), "")
	if err != nil {
		t.Fatal(err)
	}
	return d.WorkflowExecutionInfo.Status
}

// TestQueueWriteOverTemporal is the work queue against Postgres and a Temporal development server, with
// the real lifecycle activities and workflow and a fake dispatch (spec §8.1, §14.1, §14.5, §15, D6).
func TestQueueWriteOverTemporal(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &fakeDispatch{t: t, log: e.log, script: map[string][]turn{}, calls: map[string][]DispatchInput{}}
	w, err := NewWorkers(c, WorkerConfig{ReplicaID: "it-replica", DrainTimeout: time.Second}, Activities{Lifecycle: e.acts, Dispatch: d})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}})
	ctx := context.Background()

	// A new task: pre-created submitted at seq 1 with its work item, and its workflow started. Its first
	// attempt fails before an upstream task id and is retried; the second asks for input.
	var aID string
	t.Run("new task", func(t *testing.T) {
		cctx, pre := caller(true)
		d.set(pre,
			turn{outcome: DispatchRetryable, reason: string(store.ReasonUpstreamUnreachable)},
			turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted},
			turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		var ss StreamStart
		id, err := q.Write(WithStreamStart(cctx, &ss), payloadOf(requestOf("m-1", "hello there")))
		if err != nil {
			t.Fatal(err)
		}
		aID = string(id)
		if aID != pre {
			t.Fatalf("task id %s, want the id Admission pre-minted %s", id, pre)
		}
		if ss != (StreamStart{TaskID: id, Version: 0, New: true}) {
			t.Errorf("stream start %+v", ss)
		}
		var kind string
		var payload []byte
		if err := e.pool.QueryRow(ctx, `select kind, payload::text::bytea from kontra.task_events where task_id = $1 and seq = 1`, aID).Scan(&kind, &payload); err != nil {
			t.Fatal(err)
		}
		if kind != "task" || !strings.Contains(string(payload), `"TASK_STATE_SUBMITTED"`) || !strings.Contains(string(payload), "hello there") {
			t.Errorf("seq 1: %s %s", kind, payload)
		}
		var wp []byte
		if err := e.pool.QueryRow(ctx, `select payload::text::bytea from kontra.work_items where task_id = $1`, aID).Scan(&wp); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{secretToken, secretCookie, secretKey} {
			if bytes.Contains(wp, []byte(s)) {
				t.Errorf("work_items.payload holds %q (D6)", s)
			}
		}
		if !bytes.Contains(wp, []byte("a2a-version")) {
			t.Errorf("work item %s", wp)
		}
		ok, err := lc.WorkflowExists(ctx, id)
		if err != nil || !ok {
			t.Fatalf("workflow exists: %v %v", ok, err)
		}
		desc, err := c.DescribeWorkflowExecution(ctx, WorkflowID(id), "")
		if err != nil {
			t.Fatal(err)
		}
		if to := desc.ExecutionConfig.WorkflowExecutionTimeout; to != nil && to.AsDuration() != 0 {
			t.Errorf("workflow execution timeout %v: task.timeout is a workflow timer, never Temporal's", to.AsDuration())
		}
		eventually(t, "the task at input-required", func() bool { return e.state(aID) == store.StateInputRequired })
		if n := d.count(aID); n != 2 {
			t.Errorf("%d dispatches, want the failed attempt and its retry", n)
		}
		if n := e.count(`select count(*) from kontra.attempts where task_id = $1`, aID); n != 2 {
			t.Errorf("%d attempts", n)
		}
	})

	t.Run("resend", func(t *testing.T) {
		cctx, pre := caller(true)
		var ss StreamStart
		id, err := q.Write(WithStreamStart(cctx, &ss), payloadOf(requestOf("m-1", "hello there")))
		if err != nil {
			t.Fatalf("a resend: %v", err)
		}
		if string(id) != aID || string(id) == pre {
			t.Fatalf("a resend made task %s, want %s", id, aID)
		}
		row, _ := e.log.Load(ctx, uuid.MustParse(aID))
		if ss.New || ss.TaskID != id || ss.Version != row.Version {
			t.Errorf("stream start %+v, want the existing task at version %d", ss, row.Version)
		}
		if n := e.count(`select count(*) from kontra.tasks`); n != 1 {
			t.Errorf("%d tasks after a resend", n)
		}
		time.Sleep(time.Second)
		if n := d.count(aID); n != 2 {
			t.Errorf("the re-issued start ran another turn: %d dispatches", n)
		}
	})

	t.Run("reused message id", func(t *testing.T) {
		cctx, pre := caller(true)
		d.set(pre, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		id, err := q.Write(cctx, payloadOf(requestOf("m-1", "a different request")))
		if err != nil {
			t.Fatal(err)
		}
		if string(id) == aID || string(id) != pre {
			t.Fatalf("a reused messageId with another request answered %s (first task %s)", id, aID)
		}
		if n := e.count(`select count(*) from kontra.tasks`); n != 2 {
			t.Errorf("%d tasks", n)
		}
		eventually(t, "the second task completed", func() bool { return e.state(pre) == store.StateCompleted })
	})

	t.Run("follow-up", func(t *testing.T) {
		cctx, _ := caller(false)
		row, _ := e.log.Load(ctx, uuid.MustParse(aID))
		var ss StreamStart
		req := requestOf("m-2", "here is more")
		req.Message.TaskID = a2a.TaskID(aID)
		id, err := q.Write(WithStreamStart(cctx, &ss), payloadOf(req))
		if err != nil || string(id) != aID {
			t.Fatalf("follow-up %s %v", id, err)
		}
		if ss != (StreamStart{TaskID: id, Version: row.Version}) {
			t.Errorf("stream start %+v, want V = %d", ss, row.Version)
		}
		eventually(t, "the task completed", func() bool { return e.state(aID) == store.StateCompleted })
		d.mu.Lock()
		calls := d.calls[aID]
		d.mu.Unlock()
		if last := calls[len(calls)-1]; last.N != 2 || last.Resume || last.WorkItemID == "" {
			t.Errorf("follow-up dispatch %+v: the same attempt, a new work item", last)
		}
		eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, aID) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
	})

	t.Run("refused follow-up", func(t *testing.T) {
		// The workflow has ended: refused, and the work item is not kept.
		cctx, _ := caller(false)
		req := requestOf("m-3", "too late")
		req.Message.TaskID = a2a.TaskID(aID)
		_, err := q.Write(cctx, payloadOf(req))
		if !errors.Is(err, a2a.ErrUnsupportedOperation) || !errors.Is(err, ErrRefused) {
			t.Fatalf("a follow-up to a finished task: %v", err)
		}
		if n := e.count(`select count(*) from kontra.work_items where task_id = $1 and payload::text like '%too late%'`, aID); n != 0 {
			t.Errorf("a refused follow-up's work item was kept")
		}

		// The validator refuses a follow-up while a turn runs.
		hold := make(chan struct{})
		bctx, pre := caller(true)
		d.set(pre, turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted, hold: hold})
		if _, err := q.Write(bctx, payloadOf(requestOf("b-1", "task b"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "task b's turn running", func() bool { return d.count(pre) == 1 })
		fu := requestOf("b-2", "while running")
		fu.Message.TaskID = a2a.TaskID(pre)
		_, err = q.Write(cctx, payloadOf(fu))
		if !errors.Is(err, a2a.ErrUnsupportedOperation) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrClosed) {
			t.Fatalf("a follow-up while a turn runs: %v", err)
		}
		if n := e.count(`select count(*) from kontra.work_items where task_id = $1`, pre); n != 1 {
			t.Errorf("%d work items on task b: the refused one must be deleted", n)
		}
		close(hold)
		eventually(t, "task b at input-required", func() bool { return e.state(pre) == store.StateInputRequired })

		// Cancel at input-required: forward_cancel, then canceled through write_terminal (spec §8).
		res, done, err := lc.Cancel(ctx, a2a.TaskID(pre))
		if err != nil || !done || res.Outcome != CancelCanceled {
			t.Fatalf("cancel %+v %v %v", res, done, err)
		}
		if st := e.state(pre); st != store.StateCanceled {
			t.Fatalf("task b is %s after the cancel completed (D79: the cancel completes once canceled is logged)", st)
		}
		a, _ := e.log.GetAttempt(ctx, uuid.MustParse(pre), 1)
		if a.Outcome != store.OutcomeInterrupted {
			t.Errorf("attempt outcome %q, want interrupted (D77)", a.Outcome)
		}
		_, _, err = lc.Cancel(ctx, a2a.TaskID(pre))
		if !errors.Is(err, a2a.ErrTaskNotCancelable) {
			t.Errorf("a second cancel: %v", err)
		}
		recordHistory(t, c, pre, "cancel_input_required")
	})

	// The P8 check (D6): nothing of the caller's headers, and no content, crosses into Temporal.
	t.Run("history", func(t *testing.T) {
		h := history(t, c, aID)
		bin, err := proto.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{secretToken, secretCookie, secretKey, "hello there", "here is more"} {
			if bytes.Contains(bin, []byte(s)) || bytes.Contains(js, []byte(s)) {
				t.Errorf("the workflow's history holds %q", s)
			}
		}
		if !bytes.Contains(js, []byte(UpdateMessage)) || !bytes.Contains(js, []byte(ActDispatch)) {
			t.Errorf("history without the expected events: %d bytes", len(js))
		}
		recordHistory(t, c, aID, "new_task_retry_follow_up")
	})

	// An Update no kontra-lifecycle worker accepts within 10 s is ErrUnavailable, and keeps its work item.
	t.Run("not accepted", func(t *testing.T) {
		cctx, pre := caller(true)
		d.set(pre, turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted})
		if _, err := q.Write(cctx, payloadOf(requestOf("c-1", "task c"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "task c at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
		w.lifecycle.Stop()
		fu := requestOf("c-2", "nobody listens")
		fu.Message.TaskID = a2a.TaskID(pre)
		began := time.Now()
		_, err := q.Write(cctx, payloadOf(fu))
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, a2a.ErrInternalError) {
			t.Fatalf("a follow-up with no lifecycle worker: %v", err)
		}
		if took := time.Since(began); took < AcceptWait-time.Second || took > AcceptWait+5*time.Second {
			t.Errorf("waited %v, want about %v", took, AcceptWait)
		}
		if n := e.count(`select count(*) from kontra.work_items where task_id = $1 and consumed_at is null`, pre); n != 1 {
			t.Errorf("%d unconsumed work items: an Update not accepted in time keeps its item (D66)", n)
		}
	})
}

var identity = regexp.MustCompile(`"identity": "[^"]*"`)

// recordHistory writes the workflow's history to testdata/histories when RecordEnv is 1.
func recordHistory(t *testing.T, c client.Client, taskID, name string) {
	t.Helper()
	if os.Getenv(RecordEnv) != "1" {
		return
	}
	js, err := temporalproto.CustomJSONMarshalOptions{Indent: "  "}.Marshal(history(t, c, taskID))
	if err != nil {
		t.Fatal(err)
	}
	// The workers' identity (pid@host) and sticky queues name the host: replay reads neither, and the files
	// need not name it.
	js = identity.ReplaceAll(js, []byte(`"identity": "kontra-test"`))
	if host, err := os.Hostname(); err == nil && host != "" {
		js = bytes.ReplaceAll(js, []byte(host), []byte("kontra-test"))
	}
	if err := os.MkdirAll(filepath.Join("testdata", "histories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "histories", name+".json"), append(js, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
