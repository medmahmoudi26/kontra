package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/pgtest"
)

// version_conflict (spec §16, D60): two Store.Update calls with the same PrevVersion from two pools
// (two replicas): exactly one commits, the other gets ErrConcurrentModification; seqs 1..N have no gap;
// one NOTIFY per committed event.
func TestVersionConflict(t *testing.T) {
	e := newEnv(t)
	n := listen(t, e.dsn)
	poolB := pgtest.Pool(t, e.dsn)
	stores := []*Store{e.store(nil), New(poolB, e.svc, ctxOwner, nil, NewLog(poolB))}
	task := e.newTask(e.svc, buyerA, "", "hi")
	ctx := asOwner(context.Background(), buyerA)

	const rounds = 25
	for r := range rounds {
		st, err := stores[0].Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		evs := []a2a.Event{
			status(task, a2a.TaskStateWorking, fmt.Sprintf("A%d", r)),
			artifact(task, a2a.ArtifactID(fmt.Sprintf("art-%d", r)), false, fmt.Sprintf("B%d", r)),
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var errs [2]error
		var versions [2]taskstore.TaskVersion
		for i := range 2 {
			next, err := a2aevent.ApplyShallowUpdate(st.Task, evs[i])
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				<-start
				versions[i], errs[i] = stores[i].Update(ctx, &taskstore.UpdateRequest{
					Task: next, Event: evs[i], PrevTask: st.Task, PrevVersion: st.Version})
			})
		}
		close(start)
		wg.Wait()
		won := 0
		for i := range 2 {
			switch {
			case errs[i] == nil:
				won++
				if versions[i] != st.Version+1 {
					t.Errorf("round %d: winner version %d, want %d", r, versions[i], st.Version+1)
				}
			case !errors.Is(errs[i], taskstore.ErrConcurrentModification) || isNotFound(errs[i]):
				t.Errorf("round %d: loser error %v, want ErrConcurrentModification", r, errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d writers committed (errors %v)", r, won, errs)
		}
	}
	seqs, _ := e.seqs(task.ID)
	if len(seqs) != rounds+1 {
		t.Fatalf("%d rows, want %d", len(seqs), rounds+1)
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seqs %v have a gap at %d", seqs, i)
		}
	}
	got := n.waitFor(task.ID, rounds+1)
	if len(got) != rounds+1 {
		t.Fatalf("%d notifications, want %d: %v", len(got), rounds+1, got)
	}
	for i, p := range got {
		if want := fmt.Sprintf("%s:%d", task.ID, i+1); p != want {
			t.Errorf("notification %d = %q, want %q", i, p, want)
		}
	}
	final, err := stores[1].Get(ctx, task.ID)
	if err != nil || final.Version != rounds+1 {
		t.Fatalf("final version %v, %v", final, err)
	}
}

// PrevVersion 0 is a conflict; after the terminal event status, artifact and plain message writes are
// refused, and only metadata and a review-window message (AfterTerminalOK) are accepted (spec §5.1, §5.2).
func TestPrevVersionZeroAndTerminalRule(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "hi")
	id := uid(t, task.ID)

	st, _ := s.Get(ctx, task.ID)
	ev := status(task, a2a.TaskStateWorking, "")
	next, _ := a2aevent.ApplyShallowUpdate(st.Task, ev)
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevVersion: 0}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("PrevVersion 0: %v", err)
	}
	if _, err := e.log.Append(ctx, nil, Write{TaskID: id, PrevVersion: 0, Kind: KindStatus, Snapshot: next, Payload: []byte(`{}`)}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("Log.Append PrevVersion 0: %v", err)
	}
	if seqs, _ := e.seqs(task.ID); len(seqs) != 1 {
		t.Fatalf("a refused write logged: %v", seqs)
	}
	// A stale version is a conflict, never a merge.
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, ""))
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevVersion: st.Version}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale version: %v", err)
	}

	v := mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateCompleted, "done"))
	for name, ev := range map[string]a2a.Event{
		"status":   status(task, a2a.TaskStateWorking, "late"),
		"failed":   FailedStatus(info(task), ReasonUpstreamError, "late"),
		"artifact": artifact(task, "late", false, "late"),
	} {
		cur, _ := s.Get(ctx, task.ID)
		// Build the snapshot by hand: a2a-go's materializer itself refuses updates to a terminal task.
		if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: cur.Task, Event: ev, PrevVersion: cur.Version}); !errors.Is(err, taskstore.ErrConcurrentModification) || isNotFound(err) {
			t.Errorf("%s after terminal: %v", name, err)
		}
	}
	cur, _ := s.Get(ctx, task.ID)
	late := appendHistory(cur.Task, userMessage(task, "late"))
	payload, _ := json.Marshal(a2a.StreamResponse{Event: userMessage(task, "late")})
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: late, Event: userMessage(task, "late"), PrevVersion: cur.Version}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Errorf("a plain message after terminal: %v", err)
	}
	for name, w := range map[string]Write{
		"metadata without AfterTerminalOK": {Kind: KindMetadata, Payload: []byte(`{"metadata":{"k":"v"}}`)},
		"status with AfterTerminalOK":      {Kind: KindStatus, Payload: []byte(`{}`), AfterTerminalOK: true},
		"metadata changing the state":      {Kind: KindMetadata, State: StateWorking, Payload: []byte(`{"metadata":{}}`), AfterTerminalOK: true},
		"message changing the state":       {Kind: KindMessage, State: StateCanceled, Payload: payload, AfterTerminalOK: true},
	} {
		w.TaskID, w.PrevVersion, w.Snapshot = id, int64(cur.Version), cur.Task
		if _, err := e.log.Append(ctx, nil, w); !errors.Is(err, taskstore.ErrConcurrentModification) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if seqs, _ := e.seqs(task.ID); seqs[len(seqs)-1] != int64(v) {
		t.Fatalf("a refused write logged after the terminal seq %d: %v", v, seqs)
	}
	a, err := e.log.Append(ctx, nil, Write{TaskID: id, PrevVersion: int64(cur.Version), Kind: KindMetadata,
		Snapshot: mergeMetadata(cur.Task, map[string]any{"k": "v"}), Payload: []byte(`{"metadata":{"k":"v"}}`), AfterTerminalOK: true})
	if err != nil || a.Seq != int64(cur.Version)+1 {
		t.Fatalf("metadata with AfterTerminalOK: %v %v", a, err)
	}
	if _, err := e.log.Append(ctx, nil, Write{TaskID: id, PrevVersion: a.Seq, Kind: KindMessage,
		Snapshot: late, Payload: payload, AfterTerminalOK: true}); err != nil {
		t.Fatalf("a review-window message with AfterTerminalOK: %v", err)
	}
	row, _ := e.log.Load(ctx, id)
	if row.State != StateCompleted || row.Snapshot.Status.State != a2a.TaskStateCompleted {
		t.Errorf("after-terminal writes changed the state: %s", row.State)
	}
}

// countingTracer counts the queries a pool sends.
type countingTracer struct{ n atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// cross_buyer, the store half (spec §2.3, §4, §16): another buyer's task, an unknown id and a malformed
// id are all TaskNotFound; the malformed one issues no query.
func TestCrossBuyer(t *testing.T) {
	e := newEnv(t)
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &countingTracer{}
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	s := New(traced, e.svc, ctxOwner, nil, NewLog(traced))
	a := asOwner(context.Background(), buyerA)
	b := asOwner(context.Background(), buyerB)
	task := e.newTask(e.svc, buyerA, "shared-ctx", "mine")
	e.newTask(e.svc, buyerB, "shared-ctx", "theirs")

	stA, err := s.Get(a, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(b, task.ID); !isNotFound(err) {
		t.Errorf("B Get of A's task: %v", err)
	}
	ev := status(task, a2a.TaskStateWorking, "")
	next, _ := a2aevent.ApplyShallowUpdate(stA.Task, ev)
	if _, err := s.Update(b, &taskstore.UpdateRequest{Task: next, Event: ev, PrevVersion: stA.Version}); !isNotFound(err) {
		t.Errorf("B Update of A's task: %v", err)
	}
	if _, err := e.log.LoadScoped(b, e.svc, buyerB, uid(t, task.ID)); !isNotFound(err) {
		t.Errorf("B LoadScoped of A's task: %v", err)
	}
	list, err := s.List(b, &a2a.ListTasksRequest{ContextID: "shared-ctx"})
	if err != nil {
		t.Fatal(err)
	}
	if list.TotalSize != 1 || len(list.Tasks) != 1 || list.Tasks[0].ID == task.ID {
		t.Errorf("B List sees %d tasks (total %d), A's among them: %v", len(list.Tasks), list.TotalSize, list.Tasks)
	}
	// Another service of the same tenant is another scope.
	other := e.service("other", 60, 60)
	if _, err := New(traced, other, ctxOwner, nil, NewLog(traced)).Get(a, task.ID); !isNotFound(err) {
		t.Errorf("A Get through another service: %v", err)
	}

	unknown := a2a.TaskID(uuid.Must(uuid.NewV7()).String())
	if _, err := s.Get(a, unknown); !isNotFound(err) {
		t.Errorf("unknown Get: %v", err)
	}
	unknownTask := &a2a.Task{ID: unknown, ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := s.Update(a, &taskstore.UpdateRequest{Task: unknownTask, Event: ev, PrevVersion: 1}); !isNotFound(err) {
		t.Errorf("unknown Update: %v", err)
	}
	before := tracer.n.Load()
	for _, bad := range []a2a.TaskID{"tck-nonexistent-get-002", "", a2a.TaskID(strings.ToUpper(string(task.ID))), "{" + task.ID + "}", task.ID + " "} {
		if _, err := s.Get(a, bad); !isNotFound(err) {
			t.Errorf("malformed %q Get: %v", bad, err)
		}
		if _, err := s.Update(a, &taskstore.UpdateRequest{Task: &a2a.Task{ID: bad}, Event: ev, PrevVersion: 1}); !isNotFound(err) {
			t.Errorf("malformed %q Update: %v", bad, err)
		}
	}
	if got := tracer.n.Load() - before; got != 0 {
		t.Errorf("malformed ids issued %d queries", got)
	}
	if v, _ := s.Get(a, task.ID); v.Version != 1 {
		t.Errorf("A's task moved to version %d", v.Version)
	}
}

// A write whose context is already canceled still commits (D8), and is bounded by its own deadline.
func TestWriteDetachedFromCancel(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	task := e.newTask(e.svc, buyerA, "", "hi")
	ctx, cancel := context.WithCancel(asOwner(context.Background(), buyerA))
	st, _ := s.Get(ctx, task.ID)
	cancel()
	ev := status(task, a2a.TaskStateWorking, "")
	next, _ := a2aevent.ApplyShallowUpdate(st.Task, ev)
	v, err := s.Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevVersion: st.Version})
	if err != nil || v != 2 {
		t.Fatalf("a write on a canceled context: %v %v", v, err)
	}
	row, _ := e.log.Load(context.Background(), uid(t, task.ID))
	if row.State != StateWorking || row.Version != 2 {
		t.Fatalf("not committed: %s v%d", row.State, row.Version)
	}

	// Hold the task's row lock from another connection: the write waits, then gives up at its deadline.
	locker, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(context.Background(), `select 1 from kontra.tasks where id = $1 for update`, uid(t, task.ID)); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(e.dsn, "?") {
		sep = "&"
	}
	one := pgtest.Pool(t, e.dsn+sep+"pool_max_conns=1") // so the check below reads the writer's connection
	short := New(one, e.svc, ctxOwner, nil, NewLog(one, WithWriteTimeout(300*time.Millisecond)))
	ev2 := status(task, a2a.TaskStateAuthRequired, "")
	next2, _ := a2aevent.ApplyShallowUpdate(next, ev2)
	start := time.Now()
	_, err = short.Update(ctx, &taskstore.UpdateRequest{Task: next2, Event: ev2, PrevVersion: 2})
	took := time.Since(start)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a blocked write: %v, want its own deadline", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("the write was not aborted by the server's statement_timeout: %v", err)
	}
	if took < 250*time.Millisecond || took > 1500*time.Millisecond {
		t.Errorf("a blocked write gave up after %v, want about 300ms (the server's timeout, not the client's)", took)
	}
	// The server aborted the write: releasing the lock does not let it commit late.
	locker.Rollback(context.Background())
	time.Sleep(500 * time.Millisecond)
	if row, _ := e.log.Load(context.Background(), uid(t, task.ID)); row.Version != 2 {
		t.Errorf("the timed-out write committed after its writer was told it failed: v%d", row.Version)
	}
	if seqs, _ := e.seqs(task.ID); len(seqs) != 2 {
		t.Errorf("rows %v", seqs)
	}
	// The statement_timeout is transaction-local: the pooled connections keep the server default.
	var setting string
	if err := one.QueryRow(context.Background(), `show statement_timeout`).Scan(&setting); err != nil || setting != "0" {
		t.Errorf("statement_timeout leaked to the pool: %q %v", setting, err)
	}
}

// D49: a NUL character survives tasks.snapshot and task_events.payload; bytes given to Append are stored
// verbatim (a duplicate key and the spacing included).
func TestNULAndVerbatimPayload(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "a\x00b")
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, "\x00"))
	mustApply(t, ctx, s, task.ID, artifact(task, "a1", false, "x\x00y"))

	st, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Task.History[0].Parts[0].Text(); got != "a\x00b" {
		t.Errorf("history text %q", got)
	}
	if got := st.Task.Status.Message.Parts[0].Text(); got != "\x00" {
		t.Errorf("status text %q", got)
	}
	if got := st.Task.Artifacts[0].Parts[0].Text(); got != "x\x00y" {
		t.Errorf("artifact text %q", got)
	}
	var snap string
	if err := e.pool.QueryRow(ctx, `select snapshot::text from kontra.tasks where id = $1`, uid(t, task.ID)).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if strings.Count(snap, `\u0000`) != 3 {
		t.Errorf("snapshot lost a NUL: %s", snap)
	}
	evs, _ := e.log.Events(ctx, uid(t, task.ID), 0, 10)
	for _, ev := range evs {
		if !strings.Contains(string(ev.Payload), `\u0000`) {
			t.Errorf("row %d payload lost its NUL: %s", ev.Seq, ev.Payload)
		}
	}

	raw := []byte("{\"metadata\" :  {\"k\": 1, \"k\": 2,\n \"z\": \"\\u0000\"}  }")
	cur, _ := s.Get(ctx, task.ID)
	a, err := e.log.Append(ctx, nil, Write{TaskID: uid(t, task.ID), PrevVersion: int64(cur.Version), Kind: KindMetadata,
		Snapshot: mergeMetadata(cur.Task, map[string]any{"k": 2.0, "z": "\x00"}), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := e.pool.QueryRow(ctx, `select payload::text from kontra.task_events where task_id = $1 and seq = $2`,
		uid(t, task.ID), a.Seq).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(raw) {
		t.Errorf("payload not verbatim:\n got %q\nwant %q", stored, raw)
	}
	snapAt, err := e.log.SnapshotAt(ctx, uid(t, task.ID), a.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if snapAt.Metadata["k"] != 2.0 || snapAt.Metadata["z"] != "\x00" {
		t.Errorf("fold of the verbatim row: %v", snapAt.Metadata)
	}
}

// The deadline is fixed once, at the transition into submitted, and returned; terminal_at and
// retention_until are set at the terminal event; status_at is the status timestamp truncated to
// microseconds (spec §5.2, §6.5).
func TestDeadlineTerminalAndStatusTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.store(nil)
	octx := asOwner(ctx, buyerA)
	var dbNow time.Time
	if err := e.pool.QueryRow(ctx, `select now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	near := func(name string, got *time.Time, want time.Time) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s is null", name)
		}
		if d := got.Sub(want); d < -5*time.Second || d > 5*time.Second {
			t.Errorf("%s = %v, want about %v", name, got, want)
		}
	}
	ts := time.Date(2026, 10, 25, 12, 0, 0, 123456999, time.UTC)
	task, created := e.newTaskAt(e.svc, buyerA, "", "hi", &ts)
	near("created deadline", created.Deadline, dbNow.Add(time.Hour))
	row, _ := e.log.Load(ctx, uid(t, task.ID))
	if !row.Deadline.Equal(*created.Deadline) || row.SubmittedAt == nil {
		t.Errorf("stored deadline %v, returned %v; submitted_at %v", row.Deadline, created.Deadline, row.SubmittedAt)
	}
	if want := ts.Truncate(time.Microsecond); !row.StatusAt.Equal(want) {
		t.Errorf("status_at %v, want %v (truncated, not rounded)", row.StatusAt, want)
	}
	if row.TerminalAt != nil || row.RetentionUntil != nil {
		t.Errorf("terminal_at %v retention_until %v before the end", row.TerminalAt, row.RetentionUntil)
	}

	// A later status keeps the deadline; status_at follows the logged timestamp.
	ev := status(task, a2a.TaskStateWorking, "")
	later := time.Date(2026, 10, 25, 12, 0, 1, 987654321, time.UTC)
	ev.Status.Timestamp = &later
	cur, _ := s.Get(octx, task.ID)
	next, _ := a2aevent.ApplyShallowUpdate(cur.Task, ev)
	if _, err := s.Update(octx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevVersion: cur.Version}); err != nil {
		t.Fatal(err)
	}
	row, _ = e.log.Load(ctx, uid(t, task.ID))
	if !row.Deadline.Equal(*created.Deadline) {
		t.Errorf("deadline moved from %v to %v", created.Deadline, row.Deadline)
	}
	if !row.StatusAt.Equal(later.Truncate(time.Microsecond)) {
		t.Errorf("status_at %v, want %v", row.StatusAt, later.Truncate(time.Microsecond))
	}
	// An artifact leaves status_at alone (D49).
	mustApply(t, octx, s, task.ID, artifact(task, "a", false, "x"))
	if r, _ := e.log.Load(ctx, uid(t, task.ID)); !r.StatusAt.Equal(row.StatusAt) {
		t.Errorf("an artifact moved status_at to %v", r.StatusAt)
	}
	seq, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: uid(t, task.ID), State: StateFailed, Reason: ReasonTimeout})
	if err != nil {
		t.Fatal(err)
	}
	row, _ = e.log.Load(ctx, uid(t, task.ID))
	near("terminal_at", row.TerminalAt, dbNow)
	near("retention_until", row.RetentionUntil, dbNow.Add(7*24*time.Hour))
	if _, err := e.log.ApplyMetadata(ctx, uid(t, task.ID), map[string]any{"after": true}); err != nil {
		t.Fatal(err)
	}
	r2, _ := e.log.Load(ctx, uid(t, task.ID))
	if !r2.TerminalAt.Equal(*row.TerminalAt) || !r2.RetentionUntil.Equal(*row.RetentionUntil) || !r2.Deadline.Equal(*created.Deadline) {
		t.Errorf("a metadata write after the end moved a time: %v %v %v", r2.TerminalAt, r2.RetentionUntil, r2.Deadline)
	}
	if ts, _ := e.log.TerminalSeq(ctx, uid(t, task.ID)); ts != seq {
		t.Errorf("TerminalSeq %d, want %d", ts, seq)
	}

	// A quoted task has no deadline until it enters submitted; a zero ServiceRef timeout takes the
	// services row's (120 s here), and the transition returns it.
	quick := e.service("quick", 120, 3600)
	qid := uuid.Must(uuid.NewV7())
	qtask := &a2a.Task{ID: a2a.TaskID(qid.String()), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	exp := dbNow.Add(time.Hour)
	qa, err := e.log.CreateTask(ctx, nil, NewTask{ID: qid, Service: quick, Protocol: ProtocolA2A, Owner: buyerA,
		ContextID: "c", State: StateQuoted, Snapshot: qtask, QuoteExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	if qa.Deadline != nil {
		t.Errorf("a quoted task has deadline %v", qa.Deadline)
	}
	sub := status(qtask, a2a.TaskStateSubmitted, "")
	qnext, _ := a2aevent.ApplyShallowUpdate(qtask, sub)
	payload, _ := json.Marshal(a2a.StreamResponse{Event: sub})
	app, err := e.log.Append(ctx, nil, Write{TaskID: qid, PrevVersion: 1, Kind: KindStatus, State: StateSubmitted,
		StatusAt: StatusTime(sub.Status.Timestamp), Snapshot: qnext, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	near("approved deadline", app.Deadline, dbNow.Add(120*time.Second))
	work := status(qtask, a2a.TaskStateWorking, "")
	wnext, _ := a2aevent.ApplyShallowUpdate(qnext, work)
	payload, _ = json.Marshal(a2a.StreamResponse{Event: work})
	app2, err := e.log.Append(ctx, nil, Write{TaskID: qid, PrevVersion: 2, Kind: KindStatus, State: StateWorking,
		Snapshot: wnext, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !app2.Deadline.Equal(*app.Deadline) {
		t.Errorf("deadline moved from %v to %v", app.Deadline, app2.Deadline)
	}
}

// Store.Create (the contract path) and the creation checks.
func TestCreate(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	id := a2a.TaskID(uuid.Must(uuid.NewV7()).String())
	task := &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	v, err := s.Create(ctx, task)
	if err != nil || v != 1 {
		t.Fatalf("Create: %v %v", v, err)
	}
	if _, err := s.Create(ctx, task); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Errorf("duplicate Create: %v", err)
	}
	st, err := s.Get(ctx, id)
	if err != nil || st.Version != 1 || st.Task.Status.State != a2a.TaskStateWorking || st.User != buyerA.Principal {
		t.Fatalf("Get after Create: %+v %v", st, err)
	}
	if _, kinds := e.seqs(id); len(kinds) != 1 || kinds[0] != KindTask {
		t.Errorf("creation rows %v", kinds)
	}
	if _, err := s.Get(asOwner(context.Background(), buyerB), id); !isNotFound(err) {
		t.Errorf("B Get of a created task: %v", err)
	}
	long := strings.Repeat("c", 257)
	bad := &a2a.Task{ID: a2a.TaskID(uuid.Must(uuid.NewV7()).String()), ContextID: long, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	if _, err := s.Create(ctx, bad); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a 257-character contextId: %v, want InvalidParams", err)
	}
	// Lengths are characters, as the columns' CHECKs count them (Postgres length()), not bytes: 200
	// two-byte characters are kept verbatim (spec §4).
	wide := strings.Repeat("é", 200)
	wideID := uuid.Must(uuid.NewV7())
	wideTask := &a2a.Task{ID: a2a.TaskID(wideID.String()), ContextID: wide, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	if _, err := e.log.CreateTask(context.Background(), nil, NewTask{ID: wideID, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA,
		ContextID: wide, State: StateSubmitted, Snapshot: wideTask, FirstMessageID: wide, FirstRequest: [32]byte{1}}); err != nil {
		t.Fatalf("a 200-character non-ASCII contextId and messageId: %v", err)
	}
	if row, _ := e.log.Load(context.Background(), wideID); row.ContextID != wide || row.FirstMessageID != wide {
		t.Errorf("stored %q %q", row.ContextID, row.FirstMessageID)
	}
	wider := strings.Repeat("é", 257)
	if _, err := s.Create(ctx, &a2a.Task{ID: a2a.TaskID(uuid.Must(uuid.NewV7()).String()), ContextID: wider,
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a 257-character non-ASCII contextId: %v, want InvalidParams", err)
	}
	// A string no text column can hold (U+0000, invalid UTF-8) is the caller's error, not a 500.
	for name, nt := range map[string]func(*NewTask){
		"contextId with a NUL":      func(n *NewTask) { n.ContextID, n.Snapshot.ContextID = "a\x00b", "a\x00b" },
		"contextId not UTF-8":       func(n *NewTask) { n.ContextID, n.Snapshot.ContextID = "a\xffb", "a\xffb" },
		"messageId with a NUL":      func(n *NewTask) { n.FirstMessageID = "m\x00" },
		"messageId not UTF-8":       func(n *NewTask) { n.FirstMessageID = "m\xff" },
		"a 257-character messageId": func(n *NewTask) { n.FirstMessageID = wider },
	} {
		nid := uuid.Must(uuid.NewV7())
		n := NewTask{ID: nid, Service: e.svc, Protocol: ProtocolA2A, Owner: buyerA, ContextID: "c", State: StateSubmitted,
			Snapshot:       &a2a.Task{ID: a2a.TaskID(nid.String()), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
			FirstMessageID: "m", FirstRequest: [32]byte{1}}
		nt(&n)
		if _, err := e.log.CreateTask(context.Background(), nil, n); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("%s: %v, want InvalidParams", name, err)
		}
	}
	if _, err := s.Create(ctx, &a2a.Task{ID: "not-a-uuid", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}); err == nil {
		t.Error("a non-gate task id was created")
	}
	if _, err := s.Create(context.Background(), task); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("Create without a caller: %v", err)
	}
	gone := ServiceRef{TenantID: e.tenant, Name: "missing"}
	if _, err := New(e.pool, gone, ctxOwner, nil, e.log).Create(ctx, &a2a.Task{ID: a2a.TaskID(uuid.Must(uuid.NewV7()).String()),
		ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Create on a missing service: %v", err)
	}
}

// fakeGuard is a registry: a task is registered, and the events the relay yielded are recorded.
type fakeGuard struct {
	mu         sync.Mutex
	registered map[a2a.TaskID]bool
	recorded   map[a2a.Event]bool
	denied     int
}

func (g *fakeGuard) AllowFailed(taskID a2a.TaskID, ev a2a.Event) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ok := !g.registered[taskID] || g.recorded[ev]
	if !ok {
		g.denied++
	}
	return ok
}

// D8: while the task's dispatch is registered, a failed status with no message (a2a-go's SetTaskFailed
// shape) is refused; a relay-recorded failed event is stored.
func TestFailureGuard(t *testing.T) {
	e := newEnv(t)
	g := &fakeGuard{registered: map[a2a.TaskID]bool{}, recorded: map[a2a.Event]bool{}}
	s := e.store(g)
	ctx := asOwner(context.Background(), buyerA)

	task := e.newTask(e.svc, buyerA, "", "hi")
	g.registered[task.ID] = true
	mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, ""))
	synth := a2a.NewStatusUpdateEvent(info(task), a2a.TaskStateFailed, nil) // a2a-go's SetTaskFailed
	if _, err := apply(ctx, s, task.ID, synth); !errors.Is(err, ErrSynthesizedFailure) {
		t.Fatalf("SetTaskFailed shape while registered: %v", err)
	}
	asTask := &a2a.Task{ID: task.ID, ContextID: task.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
	if _, err := apply(ctx, s, task.ID, asTask); !errors.Is(err, ErrSynthesizedFailure) {
		t.Fatalf("a failed Task event with no message while registered: %v", err)
	}
	if row, _ := e.log.Load(ctx, uid(t, task.ID)); row.State != StateWorking || row.Version != 2 {
		t.Fatalf("a refused failure was stored: %s v%d", row.State, row.Version)
	}
	relay := FailedStatus(info(task), ReasonUpstreamError, "upstream said no")
	g.recorded[relay] = true
	mustApply(t, ctx, s, task.ID, relay)
	row, _ := e.log.Load(ctx, uid(t, task.ID))
	if row.State != StateFailed || row.Snapshot.Status.Message.Parts[0].Text() != "upstream said no" || row.Snapshot.Metadata[MetaFailure] != "upstream_error" {
		t.Fatalf("the relay's failure: %s %+v", row.State, row.Snapshot)
	}

	// A recorded event passes even without a message (pointer identity first).
	t2 := e.newTask(e.svc, buyerA, "", "hi")
	g.registered[t2.ID] = true
	bare := a2a.NewStatusUpdateEvent(info(t2), a2a.TaskStateFailed, nil)
	g.recorded[bare] = true
	mustApply(t, ctx, s, t2.ID, bare)
	// With no dispatch registered, nothing is refused.
	t3 := e.newTask(e.svc, buyerA, "", "hi")
	mustApply(t, ctx, s, t3.ID, a2a.NewStatusUpdateEvent(info(t3), a2a.TaskStateFailed, nil))
	// Other states are never the guard's business.
	t4 := e.newTask(e.svc, buyerA, "", "hi")
	g.registered[t4.ID] = true
	mustApply(t, ctx, s, t4.ID, a2a.NewStatusUpdateEvent(info(t4), a2a.TaskStateCompleted, nil))
}

// D77: a newer dispatch fences an older one, on the same attempt (a resume) or a later one, and an ended
// attempt fences every dispatch on it.
func TestFence(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	task := e.newTask(e.svc, buyerA, "", "hi")
	id := uid(t, task.ID)
	if _, err := e.log.BeginDispatch(ctx, id, 1, "r1"); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("dispatch before the attempt: %v", err)
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 1, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f1, err := e.log.BeginDispatch(ctx, id, 1, "replica-x")
	if err != nil {
		t.Fatal(err)
	}
	c1 := WithFence(ctx, f1)
	mustApply(t, c1, s, task.ID, status(task, a2a.TaskStateWorking, ""))

	f2, err := e.log.BeginDispatch(ctx, id, 1, "replica-x") // a resume on the same replica (D77)
	if err != nil {
		t.Fatal(err)
	}
	if f2.DispatchID == f1.DispatchID {
		t.Fatal("a dispatch reused the dispatch id")
	}
	c2 := WithFence(ctx, f2)
	if _, err := apply(c1, s, task.ID, artifact(task, "a", false, "stale")); !errors.Is(err, ErrFenced) || !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("the older dispatch's write: %v", err)
	}
	if !f1.Fenced() || f2.Fenced() {
		t.Fatalf("fenced flags: f1 %v f2 %v", f1.Fenced(), f2.Fenced())
	}
	mustApply(t, c2, s, task.ID, artifact(task, "a", false, "fresh"))
	// The follow-up message append a2a-go makes inside the dispatch is fenced too.
	if _, err := apply(c1, s, task.ID, userMessage(task, "stale")); !errors.Is(err, ErrFenced) {
		t.Fatalf("a fenced message append: %v", err)
	}
	a, _ := e.log.GetAttempt(ctx, id, 1)
	if a.ReplicaID != "replica-x" || a.DispatchID != f2.DispatchID {
		t.Errorf("attempt %+v", a)
	}

	// The attempt ends (retried): every dispatch on it is fenced, and none can start.
	if err := e.log.EndAttempt(ctx, id, 1, OutcomeUpstreamUnreachable); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(c2, s, task.ID, artifact(task, "a", false, "late")); !errors.Is(err, ErrFenced) {
		t.Fatalf("a write on an ended attempt: %v", err)
	}
	if _, err := e.log.BeginDispatch(ctx, id, 1, "r2"); !errors.Is(err, ErrAttemptNotOpen) {
		t.Fatalf("a dispatch on an ended attempt: %v", err)
	}
	if err := e.log.BeginAttempt(ctx, Attempt{TaskID: id, N: 2, Backend: BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f3, err := e.log.BeginDispatch(ctx, id, 2, "replica-y")
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, WithFence(ctx, f3), s, task.ID, status(task, a2a.TaskStateCompleted, "ok"))
	// A fence naming another task is refused before any query.
	other := e.newTask(e.svc, buyerA, "", "x")
	if _, err := apply(WithFence(ctx, f3), s, other.ID, status(other, a2a.TaskStateWorking, "")); !errors.Is(err, ErrFenced) {
		t.Fatalf("a fence of another task: %v", err)
	}
	// Unfenced writers (the workflow's) are not affected by the fence.
	if _, err := e.log.ApplyMetadata(ctx, id, map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if _, kinds := e.seqs(task.ID); len(kinds) != 5 {
		t.Errorf("rows %v, want task, status, artifact, status, metadata", kinds)
	}
}
