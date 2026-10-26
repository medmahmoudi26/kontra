package store

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/blob"
)

func (e *env) blobCount(task uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.blobs where task_id = $1`, task).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func pendingRow(t *testing.T, tenant, task uuid.UUID) blob.Row {
	t.Helper()
	ref, err := blob.NewRef()
	if err != nil {
		t.Fatal(err)
	}
	return blob.Row{Ref: ref, Tenant: tenant, Task: task, Size: 300 << 10}
}

// The blobs rows of large parts are written by the write that logs or stores a payload naming them, in that
// write's own transaction (spec §10.4, D167): the task's creation, an append, a follow-up's work item. A pending
// row whose ref the payload does not name, or of another task, is not written; a refused append (a stale
// version) writes none.
func TestPendingBlobRowsGoWithTheirWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pending := blob.NewPending()
	pctx := blob.WithPending(ctx, pending)

	// The creation row names a ref of the task's first message.
	id := uuid.Must(uuid.NewV7())
	first := pendingRow(t, e.tenant, id)
	unnamed := pendingRow(t, e.tenant, id)
	pending.Add(first)
	pending.Add(unnamed)
	msg := a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.URL(first.Ref.URL())})
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), "ctx"
	task := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}}
	if _, err := e.log.CreateTask(pctx, nil, NewTask{ID: id, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA, ContextID: "ctx",
		State: StateSubmitted, Snapshot: task}); err != nil {
		t.Fatal(err)
	}
	var got blob.Row
	var ref string
	if err := e.pool.QueryRow(ctx, `select ref, tenant_id, task_id, size from kontra.blobs`).Scan(&ref, &got.Tenant, &got.Task, &got.Size); err != nil {
		t.Fatal(err)
	}
	got.Ref = blob.Ref(ref)
	if got != first || e.blobCount(id) != 1 {
		t.Fatalf("after the creation: row %+v (want %+v), %d rows", got, first, e.blobCount(id))
	}

	// An append naming another pending ref writes it; a refused one (stale version) writes nothing.
	artifact := pendingRow(t, e.tenant, id)
	pending.Add(artifact)
	ev := a2a.NewArtifactEvent(task, &a2a.Part{Content: a2a.URL(artifact.Ref.URL())})
	s := e.store(nil)
	sctx := asOwner(pctx, buyerA)
	st, err := s.Get(sctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(sctx, &taskstore.UpdateRequest{Task: st.Task, Event: ev, PrevVersion: st.Version + 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("a stale append: %v", err)
	}
	if n := e.blobCount(id); n != 1 {
		t.Fatalf("a refused append wrote blobs rows: %d", n)
	}
	mustApply(t, sctx, s, task.ID, ev)
	if n := e.blobCount(id); n != 2 {
		t.Fatalf("after the artifact's append: %d rows, want 2", n)
	}
	// A follow-up's work item writes the rows its request names.
	followUp := pendingRow(t, e.tenant, id)
	pending.Add(followUp)
	other := pendingRow(t, e.tenant, uuid.Must(uuid.NewV7()))
	pending.Add(other)
	fm := a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.URL(followUp.Ref.URL())}, &a2a.Part{Content: a2a.URL(other.Ref.URL())})
	fm.TaskID = task.ID
	if err := e.log.InsertWorkItem(pctx, nil, WorkItem{ID: uuid.Must(uuid.NewV7()), TaskID: id, Kind: WorkExecute,
		Payload: WorkPayload{Request: &a2a.SendMessageRequest{Message: fm}, Owner: buyerA}}); err != nil {
		t.Fatal(err)
	}
	if n := e.blobCount(id); n != 3 {
		t.Fatalf("after the follow-up's work item: %d rows, want 3", n)
	}
	var total int
	if err := e.pool.QueryRow(ctx, `select count(*) from kontra.blobs`).Scan(&total); err != nil || total != 3 {
		t.Fatalf("%d rows in all: another task's pending row, or one no payload named, was written", total)
	}
	// Writing again changes nothing (a retried write).
	mustApply(t, sctx, s, task.ID, a2a.NewArtifactEvent(task, &a2a.Part{Content: a2a.URL(artifact.Ref.URL())}))
	if n := e.blobCount(id); n != 3 {
		t.Fatalf("a second append naming the same ref: %d rows", n)
	}
	// RequestHashOf: the hash the relay attached wins over the request's own.
	req := &a2a.SendMessageRequest{Message: fm}
	own, _ := RequestHash(req)
	if h, _ := RequestHashOf(ctx, req); h != own {
		t.Error("RequestHashOf without an attached hash")
	}
	var attached [32]byte
	attached[0] = 7
	if h, _ := RequestHashOf(WithRequestHash(ctx, attached), req); h != attached {
		t.Error("RequestHashOf ignored the attached hash")
	}
}
