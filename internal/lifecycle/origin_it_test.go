package lifecycle

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// D357: a caller's first message records its origin on the task's first event: the method it came in, the exchange
// that carried it, and whether the gate minted its contextId (D3); a follow-up's work item carries its own.
func TestCallerOrigin(t *testing.T) {
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
	origin := func(id string) store.Origin {
		t.Helper()
		var raw []byte
		if err := e.pool.QueryRow(ctx, `select origin::text::bytea from kontra.task_events where task_id = $1 and seq = 1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var o store.Origin
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("origin %s: %v", raw, err)
		}
		return o
	}

	// No contextId, streamed: the gate assigned the context.
	cctx, pre := caller(true)
	d.set(pre, turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted})
	cctx = store.WithRequestOrigin(cctx, &store.Origin{Actor: store.ActorCaller, Type: "SendStreamingMessage", Method: "SendStreamingMessage", ExchangeID: "ex-1"})
	if _, err := q.Write(cctx, payloadOf(requestOf("o-1", "hi"))); err != nil {
		t.Fatal(err)
	}
	o := origin(pre)
	if o.Actor != store.ActorCaller || o.Type != "SendStreamingMessage" || o.Method != "SendStreamingMessage" || o.ExchangeID != "ex-1" ||
		o.ContextAssigned == nil || !*o.ContextAssigned {
		t.Errorf("origin %+v", o)
	}

	// The caller's own contextId: not assigned.
	c2, pre2 := caller(true)
	req := requestOf("o-2", "hi")
	req.Message.ContextID = "c1"
	if _, err := q.Write(store.WithRequestOrigin(c2, &store.Origin{Actor: store.ActorCaller, Type: "SendMessage", Method: "SendMessage"}), payloadOf(req)); err != nil {
		t.Fatal(err)
	}
	if o := origin(pre2); o.Type != "SendMessage" || o.ContextAssigned == nil || *o.ContextAssigned {
		t.Errorf("origin %+v", o)
	}

	// A follow-up: its work item carries its own origin.
	eventually(t, "the first task at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
	d.set(pre, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
	fu := requestOf("o-3", "more")
	fu.Message.TaskID = a2a.TaskID(pre)
	fctx, _ := caller(false)
	fctx = store.WithRequestOrigin(fctx, &store.Origin{Actor: store.ActorCaller, Type: "SendMessage", Method: "SendMessage", ExchangeID: "ex-3"})
	if _, err := q.Write(fctx, payloadOf(fu)); err != nil {
		t.Fatal(err)
	}
	var wp []byte
	if err := e.pool.QueryRow(ctx, `select payload::text::bytea from kontra.work_items where task_id = $1 order by id desc limit 1`, pre).Scan(&wp); err != nil {
		t.Fatal(err)
	}
	var item store.WorkPayload
	if err := json.Unmarshal(wp, &item); err != nil || item.Origin == nil || item.Origin.ExchangeID != "ex-3" || item.Origin.Type != "SendMessage" {
		t.Errorf("follow-up work item origin %+v (%v): %s", item.Origin, err, wp)
	}
}
