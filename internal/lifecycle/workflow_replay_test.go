package lifecycle

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// stuckA2A is an A2A dispatch whose upstream never answers a cancel: it begins its dispatch, records a first
// upstream task id when first is set, logs working, and runs, heartbeating, until Temporal cancels it; abort and
// forward_cancel do nothing to it, and forward_cancel itself hangs.
type stuckA2A struct {
	log       *store.Log
	first     bool
	aborts    atomic.Int32
	forwarded atomic.Int32
	running   atomic.Int32
}

func (d *stuckA2A) Dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error) {
	id := uuid.MustParse(in.TaskID)
	if err := d.log.ConsumeWorkItem(ctx, uuid.MustParse(in.WorkItemID), id); err != nil {
		return DispatchResult{}, err
	}
	f, err := d.log.BeginDispatch(ctx, id, in.N, "it-replica")
	if err != nil {
		return DispatchResult{}, err
	}
	if d.first {
		if err := d.log.SetFirstUpstreamID(ctx, f, "up-task-1", "up-ctx"); err != nil {
			return DispatchResult{}, err
		}
	}
	if err := writeStatus(ctx, d.log, id, a2a.TaskStateWorking); err != nil {
		return DispatchResult{}, err
	}
	d.running.Add(1)
	defer d.running.Add(-1)
	for {
		select {
		case <-ctx.Done():
			return DispatchResult{Outcome: DispatchAborted}, nil
		case <-time.After(500 * time.Millisecond):
			activity.RecordHeartbeat(ctx)
		}
	}
}

func (d *stuckA2A) Abort(context.Context, AttemptRef) error {
	d.aborts.Add(1)
	return nil
}

func (d *stuckA2A) ForwardCancel(ctx context.Context, in AttemptRef) (ForwardCancelResult, error) {
	d.forwarded.Add(1)
	select {
	case <-ctx.Done():
	case <-time.After(12 * time.Second):
	}
	return ForwardCancelResult{Outcome: CancelContinues}, nil
}

// The /mcp tasks/cancel of a hire task (relay.md §12.2, D57; changeHireMCPCancel, D46): kontra.cancel in MCP mode on
// an A2A task, over a development server with the real lifecycle activities. With an upstream task whose
// forward_cancel never answers, and before the first upstream task id with a dispatch an abort does not stop, the
// cancel completes canceled within 10 s whatever the upstream answers, the task is canceled (client), the dispatch
// is stopped, forward_cancel (which sends the upstream §8's CancelTask) or abort ran, and the history carries the
// hire-mcp-cancel marker and not mcp-cancel's. With KONTRA_RECORD_HISTORIES=1 both histories are recorded for
// TestReplayRecordedHistories (never over a file).
func TestHireCancelInMCPMode(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &stuckA2A{log: e.log}
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

	for _, tc := range []struct {
		name   string
		first  bool
		record string
	}{
		{"with an upstream task", true, "hire_mcp_cancel_upstream_silent"},
		{"before the first upstream task id", false, "hire_mcp_cancel_before_first_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.first = tc.first
			aborts, forwarded := d.aborts.Load(), d.forwarded.Load()
			cctx, pre := caller(true)
			id, err := q.Write(cctx, payloadOf(requestOf("m-"+pre, "hire me")))
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "the dispatch runs", func() bool { return e.state(string(id)) == store.StateWorking && d.running.Load() == 1 })
			began := time.Now()
			res, ok, err := lc.CancelMCP(ctx, id)
			took := time.Since(began)
			if err != nil || !ok || res.Outcome != CancelCanceled || took >= 10*time.Second {
				t.Fatalf("kontra.cancel in MCP mode: %+v %v %v after %v", res, ok, err, took)
			}
			row, err := e.log.Load(ctx, uuid.MustParse(string(id)))
			if err != nil || row.State != store.StateCanceled || row.Snapshot.Metadata[store.MetaCanceled] != store.CanceledClient {
				t.Fatalf("the task after the cancel: %+v %v", row, err)
			}
			if tc.first && d.forwarded.Load() != forwarded+1 {
				t.Errorf("forward_cancel ran %d times", d.forwarded.Load()-forwarded)
			}
			if !tc.first && d.aborts.Load() != aborts+1 {
				t.Errorf("abort ran %d times", d.aborts.Load()-aborts)
			}
			eventually(t, "the dispatch stopped", func() bool { return d.running.Load() == 0 })
			eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, string(id)) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
			js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(history(t, c, string(id)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(js), base64.StdEncoding.EncodeToString([]byte(`"`+changeHireMCPCancel+`"`))) {
				t.Fatal("the history has no hire-mcp-cancel marker")
			}
			if strings.Contains(string(js), base64.StdEncoding.EncodeToString([]byte(`"`+changeMCPCancel+`"`))) {
				t.Fatal("an A2A task's history took the MCP tasks' marker")
			}
			recordNewHistory(t, c, string(id), tc.record)
		})
	}
}

// RequestCancelMCP (relay.md §12.5; M4 WP-05 review) returns once the workflow has accepted kontra.cancel in MCP mode,
// not after its result: with an upstream whose forward_cancel never answers, it returns at once while the cancel writes
// canceled only after mcpCancelWait (and within 10 s, D57); on the task then ended it is ErrTaskNotCancelable, whether
// the workflow's validator refuses it (still open while forward_cancel hangs) or the workflow has closed.
func TestRequestCancelMCPReturnsOnAcceptance(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &stuckA2A{log: e.log, first: true}
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
	cctx, pre := caller(true)
	id, err := q.Write(cctx, payloadOf(requestOf("m-"+pre, "hire me")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the dispatch runs", func() bool { return e.state(string(id)) == store.StateWorking && d.running.Load() == 1 })
	began := time.Now()
	err = lc.RequestCancelMCP(ctx, id)
	took := time.Since(began)
	if err != nil || took > 3*time.Second {
		t.Fatalf("RequestCancelMCP: %v after %v", err, took)
	}
	if st := e.state(string(id)); st == store.StateCanceled {
		t.Fatalf("RequestCancelMCP returned after the cancel's result (%v): the task is %s", took, st)
	}
	eventually(t, "the task is canceled", func() bool { return e.state(string(id)) == store.StateCanceled })
	if since := time.Since(began); since >= 10*time.Second || since < mcpCancelWait-time.Second {
		t.Errorf("canceled %v after the cancel was accepted", since)
	}
	if d.forwarded.Load() != 1 {
		t.Errorf("forward_cancel ran %d times", d.forwarded.Load())
	}
	// The workflow refuses a cancel once its terminal event is in (the validator), while forward_cancel still hangs:
	// that refusal comes back with the handle, and is not taken for an acceptance.
	var refused error
	eventually(t, "a cancel of the ended task is refused", func() bool {
		refused = lc.RequestCancelMCP(ctx, id)
		return refused != nil
	})
	open := closedStatus(t, c, string(id)) == enums.WORKFLOW_EXECUTION_STATUS_RUNNING
	if !errors.Is(refused, a2a.ErrTaskNotCancelable) || !open {
		t.Errorf("RequestCancelMCP of the ended task: %v (workflow still running: %v)", refused, open)
	}
	eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, string(id)) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
	if err := lc.RequestCancelMCP(ctx, id); !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Errorf("RequestCancelMCP of a closed workflow: %v", err)
	}
}

// The histories recorded before the cancel-mode patch replay with it (spec §14.4, D46): none of them carries the
// hire-mcp-cancel marker (no Update had a mode then), and each replays against the patched TaskLifecycle
// (TestReplayRecordedHistories replays every file; this one names the pre-patch files, so a patch that changed what
// they replay through fails here even if the recorded ones are re-recorded). The hire_ files are the patch's own.
func TestReplayHistoriesBeforeTheCancelMode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "histories", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	marker := base64.StdEncoding.EncodeToString([]byte(`"` + changeHireMCPCancel + `"`))
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	n := 0
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "hire_") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), marker) {
			t.Errorf("%s was recorded before the cancel mode and carries its marker", f)
		}
		r := worker.NewWorkflowReplayer()
		r.RegisterWorkflowWithOptions(TaskLifecycle, workflow.RegisterOptions{Name: WorkflowType})
		if err := r.ReplayWorkflowHistoryFromJSONFile(logger, f); err != nil {
			t.Errorf("replay of %s: %v", f, err)
		}
		n++
	}
	if n < 30 {
		t.Fatalf("only %d histories recorded before the patch", n)
	}
}
