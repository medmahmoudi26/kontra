package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// tunnelResolver is prepare's BackendResolver for a tunnel service.
type tunnelResolver struct{}

func (tunnelResolver) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	return store.BackendTunnel, nil
}

// scriptedPicker answers each attempt n of a task from its script: ErrNoSession, or a session of replica.
type scriptedPicker struct {
	mu      sync.Mutex
	replica string
	none    map[int]bool // attempts with no candidate
	asked   []SessionRequest
}

func (p *scriptedPicker) PickSession(_ context.Context, req SessionRequest) (PickedSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, req)
	if p.none[req.N] {
		return PickedSession{}, ErrNoSession
	}
	return PickedSession{SessionID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + "aaaaaaaaaaaaaaaaaaaaaaaaaa", ReplicaID: p.replica}, nil
}

// goneSession is the fake dispatch whose forward_cancel finds the attempt's session gone (spec §8).
type goneSession struct{ *fakeDispatch }

func (goneSession) ForwardCancel(context.Context, AttemptRef) (ForwardCancelResult, error) {
	return ForwardCancelResult{Outcome: CancelTunnelLost}, nil
}

// A tunnel service over a development server, with the real lifecycle activities on Postgres (spec §6.1,
// §6.4, §8, §14.2): the first attempt finds no candidate session and is written no_session; the workflow
// retries after 1 s and the second attempt, placed on a session of this replica, is dispatched on
// kontra-tunnel-<replica> and completes. A task at input-required whose session is gone is canceled into
// failed: tunnel_lost. Both histories take the new GetVersion branches (tunnel-no-session,
// cancel-tunnel-lost); with KONTRA_RECORD_HISTORIES=1 they are recorded under new names for
// TestReplayRecordedHistories (never over an existing file).
func TestTunnelLifecycleOverTemporal(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	e.acts.Backends = tunnelResolver{}
	picker := &scriptedPicker{replica: "it-replica", none: map[int]bool{}}
	e.acts.Sessions = picker
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &fakeDispatch{t: t, log: e.log, script: map[string][]turn{}, calls: map[string][]DispatchInput{}}
	w, err := NewWorkers(c, WorkerConfig{ReplicaID: "it-replica", DrainTimeout: time.Second}, Activities{Lifecycle: e.acts, Dispatch: goneSession{d}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}})
	ctx := context.Background()

	t.Run("no_session, then a session", func(t *testing.T) {
		cctx, pre := caller(true)
		picker.mu.Lock()
		picker.none = map[int]bool{1: true}
		picker.mu.Unlock()
		d.set(pre, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		began := time.Now()
		if _, err := q.Write(cctx, payloadOf(requestOf("n-1", "anyone there?"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the task completed", func() bool { return e.state(pre) == store.StateCompleted })
		if took := time.Since(began); took < time.Second {
			t.Errorf("completed after %v: the no_session retry waits 1 s", took)
		}
		id := uuid.MustParse(pre)
		a1, err := e.log.GetAttempt(ctx, id, 1)
		if err != nil || a1.Backend != store.BackendTunnel || a1.Outcome != store.OutcomeNoSession || a1.EndedAt == nil {
			t.Fatalf("attempt 1 %+v %v", a1, err)
		}
		a2, err := e.log.GetAttempt(ctx, id, 2)
		if err != nil || a2.TunnelSession == nil || a2.ReplicaID != "it-replica" || a2.KeyID == "" {
			t.Fatalf("attempt 2 %+v %v", a2, err)
		}
		if calls := d.count(pre); calls != 1 {
			t.Fatalf("%d dispatches, want one (the no_session attempt dispatches nothing)", calls)
		}
		eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, pre) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
		recordNewHistory(t, c, pre, "tunnel_no_session")
	})

	t.Run("cancel with the session gone", func(t *testing.T) {
		cctx, pre := caller(true)
		picker.mu.Lock()
		picker.none = map[int]bool{}
		picker.mu.Unlock()
		d.set(pre, turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted})
		if _, err := q.Write(cctx, payloadOf(requestOf("g-1", "ask me"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the task at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
		res, done, err := lc.Cancel(ctx, a2a.TaskID(pre))
		if err != nil || !done || res.Outcome != CancelTunnelLost {
			t.Fatalf("cancel %+v %v %v", res, done, err)
		}
		row, _ := e.log.Load(ctx, uuid.MustParse(pre))
		if row.State != store.StateFailed || row.Snapshot.Metadata[store.MetaFailure] != "tunnel_lost" {
			t.Fatalf("task %s %v", row.State, row.Snapshot.Metadata)
		}
		if a, _ := e.log.GetAttempt(ctx, uuid.MustParse(pre), 1); a.Outcome != store.OutcomeTunnelLost {
			t.Errorf("attempt outcome %q, want tunnel_lost", a.Outcome)
		}
		eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, pre) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
		recordNewHistory(t, c, pre, "tunnel_cancel_session_gone")
	})
}

// recordNewHistory writes the workflow's history to testdata/histories/<name>.json when RecordEnv is 1 and
// no file of that name exists: a recording is never re-recorded over (replay_test.go).
func recordNewHistory(t *testing.T, c client.Client, taskID, name string) {
	t.Helper()
	if os.Getenv(RecordEnv) != "1" {
		return
	}
	path := filepath.Join("testdata", "histories", name+".json")
	if _, err := os.Stat(path); err == nil {
		t.Logf("%s exists: not recorded again", path)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	js, err := temporalproto.CustomJSONMarshalOptions{Indent: "  "}.Marshal(history(t, c, taskID))
	if err != nil {
		t.Fatal(err)
	}
	js = scrubHost(js)
	if err := os.WriteFile(path, append(js, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
