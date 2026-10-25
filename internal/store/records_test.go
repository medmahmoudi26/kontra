package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func workPayload(task *a2a.Task, text string, o Owner) WorkPayload {
	msg := userMessage(task, text)
	msg.Metadata = map[string]any{"k": "v"}
	return WorkPayload{
		Request:     &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}},
		CallContext: map[string]any{"callContext": map[string]any{"svcParams": map[string]any{"A2A-Version": []any{"1.0"}}}},
		Owner:       o,
	}
}

// Work items (spec §8.1, §14.1, D66): the payload round-trips; ConsumeWorkItem succeeds once and only for
// its own task; a consumed item can be reloaded by its owner; a refused follow-up's item is deleted.
func TestWorkItems(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "", "q")
	other := e.newTask(e.svc, buyerA, "", "q")
	id := uid(t, task.ID)

	// Inserted in the transaction that pre-creates a task, as the work queue does.
	wid := uuid.Must(uuid.NewV7())
	newID := uuid.Must(uuid.NewV7())
	newSnap := &a2a.Task{ID: a2a.TaskID(newID.String()), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	want := workPayload(newSnap, "x\x00y", buyerA)
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := e.log.CreateTask(ctx, tx, NewTask{ID: newID, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA,
			ContextID: "c", State: StateSubmitted, Snapshot: newSnap}); err != nil {
			return err
		}
		return e.log.InsertWorkItem(ctx, tx, WorkItem{ID: wid, TaskID: newID, Kind: WorkExecute, Payload: want})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.log.LoadWorkItem(ctx, wid, newID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Payload, want) {
		t.Errorf("payload round trip:\n got %+v\nwant %+v", got.Payload, want)
	}
	if got.Payload.Request.Message.Parts[0].Text() != "x\x00y" || got.Kind != WorkExecute || got.ConsumedAt != nil || got.CreatedAt.IsZero() {
		t.Errorf("work item %+v", got)
	}
	if first, err := e.log.FirstWorkItemID(ctx, newID); err != nil || first != wid {
		t.Errorf("FirstWorkItemID %v %v", first, err)
	}
	// Each item records the task's version when it was stored, read in its own transaction (D81).
	if got.TaskVersion != 1 {
		t.Errorf("the first item's TaskVersion %d; want 1", got.TaskVersion)
	}
	// A failed transaction leaves neither the task nor the item.
	lost := uuid.Must(uuid.NewV7())
	_ = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_ = e.log.InsertWorkItem(ctx, tx, WorkItem{ID: lost, TaskID: id, Kind: WorkExecute, Payload: want})
		return errors.New("rolled back")
	})
	if _, err := e.log.LoadWorkItem(ctx, lost, id); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("a rolled-back item: %v", err)
	}

	w1 := uuid.Must(uuid.NewV7())
	if _, err := e.log.ApplyMetadata(ctx, id, map[string]any{"k": "v"}); err != nil { // version 2
		t.Fatal(err)
	}
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w1, TaskID: id, Kind: WorkExecute, Payload: workPayload(task, "turn", buyerA), TaskVersion: 9}); err != nil {
		t.Fatal(err)
	}
	if wi, err := e.log.LoadWorkItem(ctx, w1, id); err != nil || wi.TaskVersion != 2 {
		t.Errorf("an item stored at version 2: %+v %v", wi, err)
	}
	if _, err := e.log.LoadWorkItem(ctx, w1, uid(t, other.ID)); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("load as another task: %v", err)
	}
	if err := e.log.ConsumeWorkItem(ctx, w1, uid(t, other.ID)); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("consume as another task: %v", err)
	}
	if wi, _ := e.log.LoadWorkItem(ctx, w1, id); wi.ConsumedAt != nil {
		t.Fatal("another task's consume consumed the item")
	}
	if err := e.log.ConsumeWorkItem(ctx, w1, id); err != nil {
		t.Fatal(err)
	}
	if err := e.log.ConsumeWorkItem(ctx, w1, id); !errors.Is(err, ErrWorkItemConsumed) {
		t.Errorf("second consume: %v", err)
	}
	wi, err := e.log.LoadWorkItem(ctx, w1, id) // a retry or resume of the turn reloads it
	if err != nil || wi.ConsumedAt == nil {
		t.Errorf("reload of a consumed item: %+v %v", wi, err)
	}
	if err := e.log.ConsumeWorkItem(ctx, uuid.Must(uuid.NewV7()), id); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("consume of an unknown item: %v", err)
	}

	w2 := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w2, TaskID: id, Kind: WorkExecute, Payload: workPayload(task, "refused", buyerA)}); err != nil {
		t.Fatal(err)
	}
	if err := e.log.DeleteWorkItem(ctx, w2, uid(t, other.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.LoadWorkItem(ctx, w2, id); err != nil {
		t.Errorf("another task's delete removed the item: %v", err)
	}
	if err := e.log.DeleteWorkItem(ctx, w2, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.LoadWorkItem(ctx, w2, id); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("a deleted item: %v", err)
	}
	if err := e.log.DeleteWorkItem(ctx, w2, id); err != nil {
		t.Errorf("a second delete: %v", err)
	}
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: uuid.New(), TaskID: id, Kind: WorkExecute, Payload: WorkPayload{}}); err == nil {
		t.Error("a payload without a request was stored")
	}
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: uuid.New(), TaskID: id, Kind: "other", Payload: want}); err == nil {
		t.Error("an unknown kind was stored")
	}
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: uuid.New(), TaskID: uuid.Must(uuid.NewV7()), Kind: WorkExecute, Payload: want}); !isNotFound(err) {
		t.Errorf("an item of an unknown task: %v", err)
	}
}

// kontra.review_message's append (D66): the item is consumed in the same transaction as the message row,
// once; a retry of an append that committed answers its seq again (a lost activity outcome); a missing,
// other task's item, or one consumed by something else, writes nothing.
func TestAppendReviewMessage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "", "q")
	id := uid(t, task.ID)
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateCompleted, Text: "done"}); err != nil {
		t.Fatal(err)
	}
	w := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w, TaskID: id, Kind: WorkExecute, Payload: workPayload(task, "a dispute", buyerA)}); err != nil {
		t.Fatal(err)
	}
	other := e.newTask(e.svc, buyerA, "", "q")
	if _, err := e.log.AppendReviewMessage(ctx, uid(t, other.ID), w); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("another task's item: %v", err)
	}
	seq, err := e.log.AppendReviewMessage(ctx, id, w)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := e.log.Load(ctx, id)
	if seq != 3 || row.Version != 3 || row.State != StateCompleted {
		t.Fatalf("seq %d, version %d, state %s", seq, row.Version, row.State)
	}
	last := row.Snapshot.History[len(row.Snapshot.History)-1]
	if last.Role != a2a.MessageRoleUser || last.Parts[0].Text() != "a dispute" {
		t.Errorf("history ends with %+v", last)
	}
	if wi, _ := e.log.LoadWorkItem(ctx, w, id); wi.ConsumedAt == nil {
		t.Error("the item was not consumed")
	}
	// The first call committed but its outcome was lost: the retry answers the same seq, writes nothing.
	if again, err := e.log.AppendReviewMessage(ctx, id, w); err != nil || again != seq {
		t.Errorf("a retried append: %d %v, want %d", again, err, seq)
	}
	if row, _ := e.log.Load(ctx, id); row.Version != 3 || len(row.Snapshot.History) != 2 {
		t.Errorf("a retried append wrote: version %d, history %d", row.Version, len(row.Snapshot.History))
	}
	if _, err := e.log.AppendReviewMessage(ctx, id, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrWorkItemNotFound) {
		t.Errorf("a missing item: %v", err)
	}

	// A second review message reusing the first one's messageId (a caller resend): its own row, and each
	// item's retry answers its own seq.
	resent := workPayload(task, "a dispute", buyerA)
	first, _ := e.log.LoadWorkItem(ctx, w, id)
	resent.Request.Message.ID = first.Payload.Request.Message.ID
	w2 := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w2, TaskID: id, Kind: WorkExecute, Payload: resent}); err != nil {
		t.Fatal(err)
	}
	seq2, err := e.log.AppendReviewMessage(ctx, id, w2)
	if err != nil || seq2 != 4 {
		t.Fatalf("a second review message: %d %v", seq2, err)
	}
	if again, err := e.log.AppendReviewMessage(ctx, id, w2); err != nil || again != seq2 {
		t.Errorf("the second item's retry: %d %v, want %d", again, err, seq2)
	}
	if again, err := e.log.AppendReviewMessage(ctx, id, w); err != nil || again != seq {
		t.Errorf("the first item's retry after the second message: %d %v, want %d", again, err, seq)
	}

	// An item consumed by a dispatch (ConsumeWorkItem) logged no message in its transaction: consumed.
	w3 := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w3, TaskID: id, Kind: WorkExecute, Payload: workPayload(task, "turn", buyerA)}); err != nil {
		t.Fatal(err)
	}
	if err := e.log.ConsumeWorkItem(ctx, w3, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.AppendReviewMessage(ctx, id, w3); !errors.Is(err, ErrWorkItemConsumed) {
		t.Errorf("an item a dispatch consumed: %v", err)
	}
	if seqs, kinds := e.seqs(task.ID); len(seqs) != 4 || kinds[2] != KindMessage || kinds[3] != KindMessage {
		t.Errorf("rows %v %v", seqs, kinds)
	}
}

// Attempts (spec §4, §14.2): BeginAttempt is idempotent for prepare's retries and refuses an attempt out
// of sequence; SetFirstUpstreamID is a no-op for the same id and an error for a different one; EndAttempt
// keeps the first outcome.
func TestAttempts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "c1", "q")
	id := uid(t, task.ID)
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 1, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 1, Backend: BackendDirect}); err != nil {
		t.Errorf("a retried prepare: %v", err)
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 3, Backend: BackendDirect}); err == nil {
		t.Error("attempt 3 before attempt 2 was accepted")
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: uuid.Must(uuid.NewV7()), N: 1, Backend: BackendDirect}); !isNotFound(err) {
		t.Errorf("an attempt of an unknown task: %v", err)
	}
	if _, err := e.log.GetAttempt(ctx, id, 2); !errors.Is(err, ErrAttemptNotFound) {
		t.Errorf("a missing attempt: %v", err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, id, 1, "up-1", "up-ctx-1"); err != nil {
		t.Fatal(err)
	}
	a, _ := e.log.GetAttempt(ctx, id, 1)
	if a.UpstreamTaskID != "up-1" || a.UpstreamContextID != "up-ctx-1" || a.FirstUpstreamIDAt == nil || a.Backend != BackendDirect || a.N != 1 {
		t.Fatalf("attempt %+v", a)
	}
	firstAt := *a.FirstUpstreamIDAt
	time.Sleep(10 * time.Millisecond)
	if err := e.log.SetFirstUpstreamID(ctx, id, 1, "up-1", "up-ctx-other"); err != nil {
		t.Errorf("the same id again: %v", err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, id, 1, "up-2", "up-ctx-1"); !errors.Is(err, ErrUpstreamIDMismatch) {
		t.Errorf("a different id: %v", err)
	}
	if a, _ := e.log.GetAttempt(ctx, id, 1); a.UpstreamTaskID != "up-1" || a.UpstreamContextID != "up-ctx-1" || !a.FirstUpstreamIDAt.Equal(firstAt) {
		t.Errorf("the no-op changed the attempt: %+v", a)
	}
	if err := e.log.SetFirstUpstreamID(ctx, id, 9, "up-1", ""); !errors.Is(err, ErrAttemptNotFound) {
		t.Errorf("an unknown attempt: %v", err)
	}
	if err := e.log.EndAttempt(ctx, id, 1, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if err := e.log.EndAttempt(ctx, id, 1, OutcomeFailed); err != nil {
		t.Errorf("a second end: %v", err)
	}
	a, _ = e.log.GetAttempt(ctx, id, 1)
	if a.Outcome != OutcomeCompleted || a.EndedAt == nil {
		t.Errorf("ended attempt %+v", a)
	}
	if err := e.log.EndAttempt(ctx, id, 1, "bogus"); err == nil {
		t.Error("an unknown outcome")
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 2, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.log.LatestAttempt(ctx, id); l.N != 2 {
		t.Errorf("latest attempt %d", l.N)
	}

	// The upstream context of a gate context is reused only within the caller's own scope (spec §4, D53).
	if p, err := e.log.PreviousContextAttempt(ctx, e.svc, buyerA, "c1", BackendDirect); err != nil || p == nil || p.UpstreamContextID != "up-ctx-1" {
		t.Fatalf("A's previous attempt: %+v %v", p, err)
	}
	later := e.newTask(e.svc, buyerA, "c1", "q2")
	_ = e.log.BeginAttempt(ctx, Attempt{TaskID: uid(t, later.ID), N: 1, Backend: BackendDirect})
	_ = e.log.SetFirstUpstreamID(ctx, uid(t, later.ID), 1, "up-3", "up-ctx-3")
	if p, _ := e.log.PreviousContextAttempt(ctx, e.svc, buyerA, "c1", BackendDirect); p.UpstreamContextID != "up-ctx-3" {
		t.Errorf("A's latest upstream context %q", p.UpstreamContextID)
	}
	for name, p := range map[string]func() (*Attempt, error){
		"buyer B, same string": func() (*Attempt, error) { return e.log.PreviousContextAttempt(ctx, e.svc, buyerB, "c1", BackendDirect) },
		"another context":      func() (*Attempt, error) { return e.log.PreviousContextAttempt(ctx, e.svc, buyerA, "c2", BackendDirect) },
		"another backend":      func() (*Attempt, error) { return e.log.PreviousContextAttempt(ctx, e.svc, buyerA, "c1", BackendTunnel) },
	} {
		if a, err := p(); a != nil || err != nil {
			t.Errorf("%s: %+v %v", name, a, err)
		}
	}
}

// write_terminal and apply_metadata (spec §6.4, §14.2): the terminal event's shape, idempotence, and
// metadata after the end.
func TestWriteTerminalAndApplyMetadata(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "", "q")
	id := uid(t, task.ID)
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateFailed, Reason: "made_up"}); err == nil {
		t.Error("an unknown failure reason was written")
	}
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateWorking}); err == nil {
		t.Error("a non-terminal state was written")
	}
	seq, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateFailed, Reason: ReasonNoSession})
	if err != nil || seq != 2 {
		t.Fatalf("WriteTerminal: %d %v", seq, err)
	}
	row, _ := e.log.Load(ctx, id)
	st := row.Snapshot.Status
	if row.State != StateFailed || st.State != a2a.TaskStateFailed || st.Message == nil || st.Message.Role != a2a.MessageRoleAgent ||
		st.Message.Parts[0].Text() != ReasonText(ReasonNoSession) || row.Snapshot.Metadata[MetaFailure] != "no_session" ||
		st.Message.Metadata[MetaFailure] != "no_session" || row.TerminalAt == nil {
		t.Fatalf("terminal row %+v %+v", row, st)
	}
	again, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateCanceled, Canceled: CanceledClient})
	if err != nil || again != seq {
		t.Fatalf("a second WriteTerminal: %d %v, want %d", again, err, seq)
	}
	m1, err := e.log.ApplyMetadata(ctx, id, map[string]any{"kontra.escrow.status": "held", "x": map[string]any{"a": 1.0}})
	if err != nil || m1 != 3 {
		t.Fatalf("ApplyMetadata: %d %v", m1, err)
	}
	m2, _ := e.log.ApplyMetadata(ctx, id, map[string]any{"x": "replaced"})
	if same, _ := e.log.ApplyMetadata(ctx, id, nil); same != m2 {
		t.Errorf("an empty patch wrote: %d", same)
	}
	row, _ = e.log.Load(ctx, id)
	if row.Snapshot.Metadata["kontra.escrow.status"] != "held" || row.Snapshot.Metadata["x"] != "replaced" || row.Snapshot.Metadata[MetaFailure] != "no_session" {
		t.Errorf("metadata %v", row.Snapshot.Metadata)
	}
	if row.State != StateFailed || row.Version != m2 {
		t.Errorf("state %s version %d", row.State, row.Version)
	}
	if again, _ := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateFailed, Reason: ReasonTimeout}); again != seq {
		t.Errorf("WriteTerminal after metadata rows: %d, want the terminal seq %d", again, seq)
	}
	big := map[string]any{"k": string(make([]byte, MaxPatchBytes))}
	if _, err := e.log.ApplyMetadata(ctx, id, big); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("an oversized patch: %v", err)
	}
	if _, err := e.log.ApplyMetadata(ctx, uuid.Must(uuid.NewV7()), map[string]any{"k": 1}); !isNotFound(err) {
		t.Errorf("ApplyMetadata on an unknown task: %v", err)
	}

	// canceled: client, on an input-required task.
	t2 := e.newTask(e.svc, buyerA, "", "q")
	s := e.store(nil)
	mustApply(t, asOwner(ctx, buyerA), s, t2.ID, status(t2, a2a.TaskStateInputRequired, "which one?"))
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: uid(t, t2.ID), State: StateCanceled, Canceled: CanceledClient}); err != nil {
		t.Fatal(err)
	}
	r2, _ := e.log.Load(ctx, uid(t, t2.ID))
	if r2.State != StateCanceled || r2.Snapshot.Metadata[MetaCanceled] != "client" || len(r2.Snapshot.History) != 2 {
		t.Errorf("canceled task %+v", r2.Snapshot)
	}
}

// Resend detection (spec §15, D74): same principal, service, messageId and request hash, within the window.
func TestFindResend(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q"))
	m.ID = "msg-1"
	req := &a2a.SendMessageRequest{Message: m}
	h, err := RequestHash(req)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{m}}
	if _, err := e.log.CreateTask(ctx, nil, NewTask{ID: id, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA, ContextID: "c",
		State: StateSubmitted, Snapshot: snap, FirstMessageID: "msg-1", FirstRequest: h}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-24 * time.Hour)
	found, err := e.log.FindResend(ctx, e.svc, buyerA, "msg-1", h, since)
	if err != nil || found == nil || found.ID != id || found.FirstMessageID != "msg-1" {
		t.Fatalf("resend: %+v %v", found, err)
	}
	withPush := &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://h.example/x"}}}
	h2, _ := RequestHash(withPush)
	sameKeyOtherPrincipal := buyerA
	sameKeyOtherPrincipal.Principal = "key:kk_a_00000002"
	for name, f := range map[string]func() (*TaskRow, error){
		"another request": func() (*TaskRow, error) { return e.log.FindResend(ctx, e.svc, buyerA, "msg-1", h2, since) },
		"another buyer":   func() (*TaskRow, error) { return e.log.FindResend(ctx, e.svc, buyerB, "msg-1", h, since) },
		"another principal": func() (*TaskRow, error) {
			return e.log.FindResend(ctx, e.svc, sameKeyOtherPrincipal, "msg-1", h, since)
		},
		"another messageId": func() (*TaskRow, error) { return e.log.FindResend(ctx, e.svc, buyerA, "msg-2", h, since) },
		"too old": func() (*TaskRow, error) {
			return e.log.FindResend(ctx, e.svc, buyerA, "msg-1", h, time.Now().Add(time.Hour))
		},
	} {
		if r, err := f(); r != nil || err != nil {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
	// A messageId no text column can hold is the caller's error, before any query (spec §4).
	for name, mid := range map[string]string{"a NUL": "m\x00", "invalid UTF-8": "m\xff"} {
		if r, err := e.log.FindResend(ctx, e.svc, buyerA, mid, h, since); r != nil || !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("messageId with %s: %+v %v, want InvalidParams", name, r, err)
		}
	}
}

// FindResendLocked serializes two identical requests: the second's lookup, in its own creating transaction,
// waits for the first's transaction and then finds its task (spec §15).
func TestFindResendLockedSerializesIdenticalRequests(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q"))
	m.ID = "msg-dup"
	h, err := RequestHash(&a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-24 * time.Hour)
	tx1, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if r, err := e.log.FindResendLocked(ctx, tx1, e.svc, buyerA, "msg-dup", h, since); r != nil || err != nil {
		t.Fatalf("the first request: %+v %v", r, err)
	}
	id := uuid.Must(uuid.NewV7())
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{m}}
	if _, err := e.log.CreateTask(ctx, tx1, NewTask{ID: id, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA, ContextID: "c",
		State: StateSubmitted, Snapshot: snap, FirstMessageID: "msg-dup", FirstRequest: h}); err != nil {
		t.Fatal(err)
	}
	type found struct {
		r   *TaskRow
		err error
	}
	second := make(chan found, 1)
	go func() {
		tx2, err := e.pool.Begin(ctx)
		if err != nil {
			second <- found{err: err}
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		r, err := e.log.FindResendLocked(ctx, tx2, e.svc, buyerA, "msg-dup", h, since)
		second <- found{r, err}
	}()
	select {
	case f := <-second:
		t.Fatalf("the second lookup did not wait for the first transaction: %+v %v", f.r, f.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-second:
		if f.err != nil || f.r == nil || f.r.ID != id {
			t.Fatalf("the second lookup: %+v %v, want the first request's task", f.r, f.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second lookup is still waiting after the first committed")
	}
}

// LoadService turns a task row's tenant and service into the ServiceRef a dispatch needs.
func TestLoadService(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(`update kontra.services set retries = 4 where tenant_id = $1 and name = 'hello'`, e.tenant)
	ref, err := e.log.LoadService(ctx, e.tenant, "hello")
	if err != nil {
		t.Fatal(err)
	}
	want := ServiceRef{TenantID: e.tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 4, Retention: 7 * 24 * time.Hour}
	if ref != want {
		t.Errorf("LoadService %+v, want %+v", ref, want)
	}
	task := e.newTask(e.svc, buyerA, "", "q")
	row, _ := e.log.Load(ctx, uid(t, task.ID))
	if back, err := e.log.LoadService(ctx, row.TenantID, row.Service); err != nil || back != want {
		t.Errorf("LoadService from a task row: %+v %v", back, err)
	}
	if _, err := e.log.LoadService(ctx, e.tenant, "missing"); err == nil {
		t.Error("a missing service loaded")
	}
}
