package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// deliveredAt returns the work item's delivered_at (nil while it has none).
func (e *env) deliveredAt(wid, taskID uuid.UUID) *time.Time {
	e.t.Helper()
	wi, err := e.log.LoadWorkItem(context.Background(), wid, taskID)
	if err != nil {
		e.t.Fatal(err)
	}
	return wi.DeliveredAt
}

// followUpItem stores a follow-up turn's work item for task.
func (e *env) followUpItem(task *a2a.Task, text string) uuid.UUID {
	e.t.Helper()
	wid := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(context.Background(), nil, WorkItem{ID: wid, TaskID: uid(e.t, task.ID), Kind: WorkExecute,
		Payload: workPayload(task, text, buyerA)}); err != nil {
		e.t.Fatal(err)
	}
	return wid
}

// sameTransaction reports whether the work item's current row version was written by the transaction that
// inserted the task's row at seq, and at the same now().
func (e *env) sameTransaction(wid, taskID uuid.UUID, seq int64) bool {
	e.t.Helper()
	var sameXact, sameNow bool
	err := e.pool.QueryRow(context.Background(), `select w.xmin::text = ev.xmin::text, w.delivered_at = ev.created_at
		from kontra.work_items w, kontra.task_events ev
		where w.id = $1 and ev.task_id = $2 and ev.seq = $3`, wid, taskID, seq).Scan(&sameXact, &sameNow)
	if err != nil {
		e.t.Fatal(err)
	}
	return sameXact && sameNow
}

// Follow-up delivery (spec §6.4, D81): the delivery a dispatch arms when the upstream first answers a
// follow-up is written by the first append under its fence that commits, in that append's own transaction,
// and only once; an append made before the answer (a2a-go's history append of the caller's message)
// carries none; a fenced append writes nothing and leaves the delivery armed.
func TestFollowUpDelivery(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "book a room")
	id := uid(t, task.ID)
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 1, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f1, err := e.log.BeginDispatch(ctx, id, 1, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, WithFence(ctx, f1), s, task.ID, status(task, a2a.TaskStateInputRequired, "which one?"))

	// The follow-up turn's dispatch: a2a-go appends the caller's message before anything is sent upstream.
	w := e.followUpItem(task, "the blue one")
	f2, err := e.log.BeginDispatch(ctx, id, 1, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	c2 := WithFence(ctx, f2)
	mustApply(t, c2, s, task.ID, userMessage(task, "the blue one"))
	if got := e.deliveredAt(w, id); got != nil || f2.DeliveryArmed() {
		t.Fatalf("delivered before the upstream answered: %v, armed %v", got, f2.DeliveryArmed())
	}

	// The upstream answers: the first append from the answer records the delivery in its own transaction.
	f2.ArmDelivery(w)
	if !f2.DeliveryArmed() {
		t.Fatal("ArmDelivery armed nothing")
	}
	v := mustApply(t, c2, s, task.ID, status(task, a2a.TaskStateWorking, ""))
	first := e.deliveredAt(w, id)
	if first == nil {
		t.Fatal("the first append from the answer did not record the delivery")
	}
	if !e.sameTransaction(w, id, int64(v)) {
		t.Error("delivered_at was not written in the transaction of the first append from the answer")
	}
	if f2.DeliveryArmed() {
		t.Error("the delivery stayed armed after the append that wrote it")
	}
	// Later appends of the turn leave it as it is.
	mustApply(t, c2, s, task.ID, artifact(task, "a1", false, "room 12"))
	if got := e.deliveredAt(w, id); got == nil || !got.Equal(*first) {
		t.Errorf("a later append moved delivered_at: %v, was %v", got, first)
	}

	// A fenced append (a resume took the attempt) records nothing, and its delivery stays armed.
	w2 := e.followUpItem(task, "another")
	f3, err := e.log.BeginDispatch(ctx, id, 1, "replica-b")
	if err != nil {
		t.Fatal(err)
	}
	f2.ArmDelivery(w2)
	if _, err := apply(c2, s, task.ID, status(task, a2a.TaskStateWorking, "stale")); !errors.Is(err, ErrFenced) {
		t.Fatalf("the fenced append: %v", err)
	}
	if got := e.deliveredAt(w2, id); got != nil {
		t.Errorf("a fenced append recorded a delivery: %v", got)
	}
	if !f2.DeliveryArmed() {
		t.Error("a fenced append disarmed the delivery")
	}
	// So does an append in a transaction that rolls back.
	row, err := e.log.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	write := func(text string) Write {
		ev := status(task, a2a.TaskStateWorking, text)
		next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
		return Write{TaskID: id, PrevVersion: row.Version, Kind: KindStatus, State: StateWorking, Snapshot: next,
			Payload: payload, Fence: f3, Deliver: w2}
	}
	_ = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		a, err := e.log.Append(ctx, tx, write("rolled back"))
		if err != nil || !a.Delivered {
			t.Errorf("append in the transaction: %+v %v", a, err)
		}
		return errors.New("roll back")
	})
	if got := e.deliveredAt(w2, id); got != nil {
		t.Errorf("a rolled-back append recorded a delivery: %v", got)
	}
	// An explicit Deliver on a write that commits: Delivered reports the write that set it, once.
	a, err := e.log.Append(ctx, nil, write("answered"))
	if err != nil || !a.Delivered || e.deliveredAt(w2, id) == nil || !e.sameTransaction(w2, id, a.Seq) {
		t.Fatalf("append with Deliver: %+v %v", a, err)
	}
	row, _ = e.log.Load(ctx, id)
	if a, err := e.log.Append(ctx, nil, write("again")); err != nil || a.Delivered {
		t.Errorf("a second delivery of the same item: %+v %v", a, err)
	}

	// A Deliver naming another task's work item sets nothing.
	other := e.newTask(e.svc, buyerA, "", "other")
	wo := e.followUpItem(other, "not yours")
	row, _ = e.log.Load(ctx, id)
	wr := write("cross")
	wr.Deliver = wo
	if a, err := e.log.Append(ctx, nil, wr); err != nil || a.Delivered {
		t.Errorf("a delivery of another task's item: %+v %v", a, err)
	}
	if got := e.deliveredAt(wo, uid(t, other.ID)); got != nil {
		t.Errorf("another task's item was delivered: %v", got)
	}

	// Writes without a delivery report none.
	row, _ = e.log.Load(ctx, id)
	plain := write("plain")
	plain.Deliver, plain.Fence = uuid.Nil, nil
	if a, err := e.log.Append(ctx, nil, plain); err != nil || a.Delivered {
		t.Errorf("a write without a delivery: %+v %v", a, err)
	}
}

// MarkDelivered is the delivery of an answer that logs nothing (D81): written under the dispatch's fence,
// at most once; a fenced dispatch writes nothing; an item that is missing or another task's is
// ErrWorkItemNotFound.
func TestMarkDelivered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "", "q")
	id := uid(t, task.ID)
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 1, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	old, err := e.log.BeginDispatch(ctx, id, 1, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := e.log.BeginDispatch(ctx, id, 1, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	w := e.followUpItem(task, "a")
	if err := e.log.MarkDelivered(ctx, cur, w); err != nil {
		t.Fatal(err)
	}
	first := e.deliveredAt(w, id)
	if first == nil {
		t.Fatal("not delivered")
	}
	if err := e.log.MarkDelivered(ctx, cur, w); err != nil {
		t.Errorf("a second mark: %v", err)
	}
	if got := e.deliveredAt(w, id); got == nil || !got.Equal(*first) {
		t.Errorf("a second mark moved delivered_at: %v, was %v", got, first)
	}

	w2 := e.followUpItem(task, "b")
	if err := e.log.MarkDelivered(ctx, old, w2); !errors.Is(err, ErrFenced) || !old.Fenced() {
		t.Errorf("a fenced mark: %v, fenced %v", err, old.Fenced())
	}
	if got := e.deliveredAt(w2, id); got != nil {
		t.Errorf("a fenced mark wrote %v", got)
	}
	if err := e.log.MarkDelivered(ctx, cur, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("an unknown item: %v", err)
	}
	other := e.newTask(e.svc, buyerA, "", "other")
	wo := e.followUpItem(other, "x")
	if err := e.log.MarkDelivered(ctx, cur, wo); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("another task's item: %v", err)
	}
	if got := e.deliveredAt(wo, uid(t, other.ID)); got != nil {
		t.Errorf("another task's item was delivered: %v", got)
	}
	if err := e.log.MarkDelivered(ctx, nil, w2); err == nil {
		t.Error("a mark without a fence was accepted")
	}
	// The attempt ends: no dispatch of it marks anything any more.
	if err := e.log.EndAttempt(ctx, id, 1, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if err := e.log.MarkDelivered(ctx, cur, w2); !errors.Is(err, ErrFenced) {
		t.Errorf("a mark on an ended attempt: %v", err)
	}
}

// TurnStatusLogged tells a follow-up turn that has logged a status of its own from one whose only status is
// the previous turn's (D81): by the order of the status rows and the latest message row of its messageId.
func TestTurnStatusLogged(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "book")
	id := uid(t, task.ID)
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateInputRequired, "which one?"))
	msg := userMessage(task, "the blue one")
	check := func(messageID string, want bool) {
		t.Helper()
		got, err := e.log.TurnStatusLogged(ctx, id, messageID)
		if err != nil || got != want {
			t.Errorf("TurnStatusLogged(%q) = %v, %v; want %v", messageID, got, err, want)
		}
	}
	check(msg.ID, false) // no message row yet
	mustApply(t, ctx, s, task.ID, msg)
	check(msg.ID, false) // only the previous turn's status
	mustApply(t, ctx, s, task.ID, artifact(task, "a", false, "x"))
	check(msg.ID, false) // an artifact is no status
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateInputRequired, "which colour?"))
	check(msg.ID, true)
	check("unknown", false)
	check("", false)
	check("nul\x00", false)
	// A later turn's message: its own turn has logged nothing yet.
	next := userMessage(task, "red")
	mustApply(t, ctx, s, task.ID, next)
	check(next.ID, false)
	check(msg.ID, true)
}
