package push

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
)

// provisionalTask pre-creates a new message's record unexposed, as the work queue does for a caller awaiting the
// answer (D362).
func (e *env) provisionalTask(o store.Owner) uuid.UUID {
	e.t.Helper()
	id := e.newTask(o)
	e.exec(`update kontra.tasks set exposed_at = null where id = $1`, id)
	return id
}

// writeFrom appends a status event with its origin, as a dispatch logs the callee's events (D357).
func (e *env) writeFrom(id uuid.UUID, s store.State, msg *a2a.Message, origin *store.Origin) int64 {
	e.t.Helper()
	ev := a2a.NewStatusUpdateEvent(info(id), s.A2A(), msg)
	now := e.statusTime()
	ev.Status.Timestamp = &now
	row, err := e.log.Load(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	next := *row.Snapshot
	next.Status = ev.Status
	payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.log.Append(context.Background(), nil, store.Write{TaskID: id, PrevVersion: row.Version, Kind: store.KindStatus,
		State: s, StatusAt: now, Snapshot: &next, Payload: payload, Origin: origin})
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Seq
}

// D362: an inline push config on a new message receives nothing while the record is provisional; when the callee
// answers with a Message the record ends unexposed, its configs are deleted, and the activity stops as for a deleted
// config. A record the callee answers with a task is delivered from its exposure on, as before.
func TestPushWaitsForExposure(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	d := NewDeliverer(e.log, private, fast())

	id := e.provisionalTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	out := runPush(context.Background(), d, id, cid)
	e.writeFrom(id, store.StateWorking, nil, store.GateOrigin()) // the gate's own: the record stays provisional
	time.Sleep(200 * time.Millisecond)
	if n := len(r.requests()); n != 0 {
		t.Fatalf("%d deliveries for a provisional record, want none", n)
	}
	answer := a2a.NewMessageForTask(a2a.MessageRoleAgent, info(id), a2a.NewTextPart("hello"))
	e.writeFrom(id, store.StateCompleted, answer, &store.Origin{Actor: store.ActorCallee, Type: store.TypeMessage})
	o := waitOut(t, out, 10*time.Second)
	if o.err != nil || o.res.Stopped != StoppedDeleted {
		t.Fatalf("push_webhook of a message-only record: %+v %v, want stopped deleted", o.res, o.err)
	}
	if n := len(r.requests()); n != 0 {
		t.Errorf("%d deliveries for a message-only record, want none", n)
	}
	var left int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.push_configs where task_id = $1`, id).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d push configs left on the message-only record (%v), want none", left, err)
	}

	id = e.provisionalTask(buyerA)
	cid, _ = e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook2")))
	out = runPush(context.Background(), d, id, cid)
	e.writeFrom(id, store.StateWorking, nil, &store.Origin{Actor: store.ActorCallee, Type: store.TypeTask})
	term := e.status(id, store.StateCompleted)
	o = waitOut(t, out, 10*time.Second)
	if o.err != nil || o.res.Stopped != StoppedTerminal || o.res.DeliveredSeq != term {
		t.Fatalf("push_webhook of an exposed task: %+v %v", o.res, o.err)
	}
	if n := len(r.requests()); n == 0 {
		t.Error("nothing delivered for the exposed task")
	}
}
